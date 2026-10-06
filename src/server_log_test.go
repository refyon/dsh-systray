package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestServerOutputSurvivesTrayHandleClose 回归 2026-10-06「后台服务自己终止」：
//
// 服务进程的输出必须是**真实文件句柄**（不是 Go os/exec 建的管道）。判据：父进程把自己那份
// 句柄关掉后（等价于托盘退出/自更新 relaunch），服务仍能继续写入。原管道实现下，读端随父进程
// 消失，服务的下一次写入拿到 EPIPE → Node 静默 exit(1)，且崩溃文本也写进断管道、全局无痕。
func TestServerOutputSurvivesTrayHandleClose(t *testing.T) {
	if os.Getenv("DSH_TEST_SERVER_TICK") != "" {
		// 子进程角色：持续往 stdout 写心跳，直到被父进程杀掉
		for i := 0; ; i++ {
			fmt.Fprintf(os.Stdout, "tick %d\n", i)
			time.Sleep(40 * time.Millisecond)
		}
	}

	dir := t.TempDir()
	oldLogDir := logDir
	logDir = dir
	t.Cleanup(func() { logDir = oldLogDir })

	rotateServerLogIfLarge()
	f, err := openServerLogForChild()
	if err != nil {
		t.Fatalf("打开服务日志文件失败: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestServerOutputSurvivesTrayHandleClose")
	cmd.Env = append(os.Environ(), "DSH_TEST_SERVER_TICK=1")
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程失败: %v", err)
	}
	_ = f.Close() // 托盘退出：本地句柄消失（管道实现下，子进程此刻已注定在下一次写入时死亡）
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	sizeAt := func() int64 {
		fi, err := os.Stat(serverLogPath())
		if err != nil {
			return -1
		}
		return fi.Size()
	}
	time.Sleep(400 * time.Millisecond)
	first := sizeAt()
	if first <= 0 {
		t.Fatalf("服务应已开始写入 server.log，实际大小 %d", first)
	}
	time.Sleep(600 * time.Millisecond)
	second := sizeAt()
	if second <= first {
		t.Fatalf("父进程关闭句柄后服务应继续写入（管道实现会在此断掉）：%d → %d", first, second)
	}
	_ = cmd.Process.Kill()
}

// TestServerLogTailMergesIntoUnifiedLog tail 把服务输出按行并回统一日志（含半行缓冲与截断重读）。
func TestServerLogTailMergesIntoUnifiedLog(t *testing.T) {
	dir := t.TempDir()
	oldLogDir, oldFile := logDir, unifiedFile
	logDir = dir
	unifiedFile = nil
	t.Cleanup(func() {
		stopServerLogTail()
		if unifiedFile != nil {
			_ = unifiedFile.Close()
		}
		logDir, unifiedFile = oldLogDir, oldFile
	})
	if err := initUnifiedLog(); err != nil {
		t.Fatalf("初始化统一日志失败: %v", err)
	}
	readUnified := func() string {
		data, err := os.ReadFile(unifiedLogPath())
		if err != nil {
			return ""
		}
		return string(data)
	}

	logPath := serverLogPath()
	if err := os.WriteFile(logPath, []byte("dsh web: http://127.0.0.1:18080/?token=T1\npartial line"), 0o644); err != nil {
		t.Fatalf("写入 server.log 失败: %v", err)
	}
	startServerLogTail(false)
	waitFor(t, 3*time.Second, func() bool { return strings.Contains(readUnified(), "token=T1") })
	if got := readUnified(); !strings.Contains(got, "[server]") {
		t.Fatalf("统一日志应带 [server] 前缀，实际：%s", got)
	}
	// 半行（无换行）不该落盘——否则崩溃末行会被截断
	if strings.Contains(readUnified(), "partial line") {
		t.Fatalf("未换行的半行不该先落盘")
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	_, _ = f.WriteString(" done\n")
	_ = f.Close()
	waitFor(t, 3*time.Second, func() bool { return strings.Contains(readUnified(), "partial line done") })

	// 文件被截断（轮转）：应回到开头重读新内容
	if err := os.WriteFile(logPath, []byte("after rotate\n"), 0o644); err != nil {
		t.Fatalf("截断失败: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return strings.Contains(readUnified(), "after rotate") })

	stopServerLogTail()
	// 停机后不再新增
	before := readUnified()
	_, _ = appendFile(logPath, "should not appear\n")
	time.Sleep(500 * time.Millisecond)
	if after := readUnified(); after != before {
		t.Fatalf("停机后不该继续并入：%q → %q", before, after)
	}
}

// appendFile 测试辅助：追加一行到文件。
func appendFile(path, text string) (int, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.WriteString(text)
}

// waitFor 轮询等待条件成立（超时即失败）。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待条件超时（%s）", timeout)
}
