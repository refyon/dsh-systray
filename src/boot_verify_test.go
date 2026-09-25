package main

// 启动健康校验提速（2026-09-25）与 LKG 备份异步清理的回归测试。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// appendServerLine 往临时统一日志追加一行 [server] 输出（复刻 harness 启动输出形态）。
func appendServerLine(t *testing.T, dir, body string) {
	t.Helper()
	p := filepath.Join(dir, unifiedLogName)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	line := time.Now().Format("2006/01/02 15:04:05") + " [INFO] [server] " + body + "\n"
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

// waitLateBootWatch 等后台兜底监视退出（它读到窗口结束），避免测试还原 logDir 后它继续扫真实日志。
func waitLateBootWatch(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !lateBootWatchActive.Load() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("后台兜底监视未在预期时间内退出")
}

// TestVerifyServerBootFastExit 命中就绪标志 + 静默期即提前通过，不再等满窗口。
// 依据：本机 15:08 重置后就绪标志 15:08:22 出现，却一直干等到 15:09:49 才判通过（窗口 60s）。
func TestVerifyServerBootFastExit(t *testing.T) {
	dir := useTempLogDir(t)
	appendServerLine(t, dir, "dsh web: http://127.0.0.1:3080/?token=abc")
	settle := bootVerifyFastExitMin + bootVerifyQuietPeriod + time.Second

	start := time.Now()
	if !verifyServerBootWithin(0, nil, settle) {
		t.Fatal("命中就绪标志 + 静默期应判健康")
	}
	el := time.Since(start)
	if el < bootVerifyFastExitMin {
		t.Fatalf("提前通过早于最短等待：%s < %s", el, bootVerifyFastExitMin)
	}
	if el >= settle {
		t.Fatalf("提前通过未生效（%s，窗口 %s）", el, settle)
	}
	waitLateBootWatch(t, settle+5*time.Second)
}

// TestVerifyServerBootErrorFailsFast 加载错误一出现即判失败，不等窗口。
func TestVerifyServerBootErrorFailsFast(t *testing.T) {
	dir := useTempLogDir(t)
	appendServerLine(t, dir, "Error: failed to import loader entry /x.js")
	start := time.Now()
	if verifyServerBootWithin(0, nil, 30*time.Second) {
		t.Fatal("存在加载错误应判失败")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("错误未快速判失败：%s", el)
	}
}

// TestVerifyServerBootErrorAfterReadyStillFails 就绪标志之后的加载错误优先于提前通过。
func TestVerifyServerBootErrorAfterReadyStillFails(t *testing.T) {
	dir := useTempLogDir(t)
	appendServerLine(t, dir, "dsh web: http://127.0.0.1:3080/?token=abc")
	appendServerLine(t, dir, "Cannot find package '@deepseek-ai/dsh-llm'")
	if verifyServerBootWithin(0, nil, 30*time.Second) {
		t.Fatal("就绪后出现的加载错误应判失败")
	}
}

// TestVerifyServerBootWaitsWindowWithoutReadyMarker 没有就绪标志时保持原语义：等满窗口才判健康。
func TestVerifyServerBootWaitsWindowWithoutReadyMarker(t *testing.T) {
	useTempLogDir(t) // 空日志：既无就绪标志也无错误
	settle := 2 * time.Second
	start := time.Now()
	if !verifyServerBootWithin(0, nil, settle) {
		t.Fatal("无错误应判健康")
	}
	if el := time.Since(start); el < settle {
		t.Fatalf("窗口未等满：%s < %s", el, settle)
	}
}

// TestVerifyServerBootNoFastExitWhenDisallowed 冷启动带 LKG 的场景关闭提前通过（通过后即清 LKG，
// 必须等满窗口才能保证迟到的加载错误仍可回退）。
func TestVerifyServerBootNoFastExitWhenDisallowed(t *testing.T) {
	dir := useTempLogDir(t)
	appendServerLine(t, dir, "dsh web: http://127.0.0.1:3080/?token=abc")
	settle := bootVerifyFastExitMin + bootVerifyQuietPeriod + time.Second
	start := time.Now()
	if !verifyServerBootPolling(0, nil, settle, false) {
		t.Fatal("无错误应判健康")
	}
	if el := time.Since(start); el < settle {
		t.Fatalf("关闭提前通过后仍提前返回：%s < %s", el, settle)
	}
	if lateBootWatchActive.Load() {
		t.Fatal("关闭提前通过时不应启动后台兜底")
	}
}

// TestRemoveAllAsyncAsyncDeletion pathsToLkgBackups 展开 + 异步删除（不阻塞调用方）。
func TestRemoveAllAsyncAsyncDeletion(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"+lkgSuffix, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"+lkgSuffix), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"+lkgSuffix), []byte("lock"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := pathsToLkgBackups([]string{dir})
	if len(paths) != 3 {
		t.Fatalf("pathsToLkgBackups = %v，want 3 项", paths)
	}
	if !hasLkgInDir(dir) {
		t.Fatal("前置：应存在 LKG 备份")
	}
	removeAllAsync(paths...)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && hasLkgInDir(dir) {
		time.Sleep(50 * time.Millisecond)
	}
	if hasLkgInDir(dir) {
		t.Fatal("LKG 备份未被异步清除")
	}
}

// TestClearAllLkgAsyncCleanup 标记文件同步清除（决定下次冷启动用不用加长窗口），
// 体积大的备份后台删除——收尾流程不再为它阻塞几十秒（本机实证同步删 27s）。
func TestClearAllLkgAsyncCleanup(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())  // lkgMarkerPath 落在临时配置目录
	t.Setenv("DSH_HOME", t.TempDir()) // profiles 枚举不触碰真实数据
	prevHarness := harnessDir
	harnessDir = t.TempDir()
	t.Cleanup(func() { harnessDir = prevHarness })

	writeLkgMarker(lkgMarker{HarnessVersion: "0.1.6"})
	if _, ok := readLkgMarker(); !ok {
		t.Fatal("前置：LKG 标记未写入")
	}
	if err := os.MkdirAll(filepath.Join(harnessDir, "node_modules"+lkgSuffix), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(harnessDir, "pnpm-lock.yaml"+lkgSuffix), []byte("lock"), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	clearAllLkg()
	if el := time.Since(start); el > time.Second {
		t.Fatalf("clearAllLkg 不应阻塞：%s", el)
	}
	if _, ok := readLkgMarker(); ok {
		t.Fatal("LKG 标记应同步清除")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && hasLkgInDir(harnessDir) {
		time.Sleep(50 * time.Millisecond)
	}
	if hasLkgInDir(harnessDir) {
		t.Fatal("LKG 备份未被异步清除")
	}
}
