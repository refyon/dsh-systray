package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// setupUnifiedForMerge 让统一日志句柄指向 temp 目录下文件并设置 logDir（测试结束还原）。
func setupUnifiedForMerge(t *testing.T) string {
	t.Helper()
	prevDir := logDir
	dir := t.TempDir()
	logDir = dir
	f := mustOpenFile(t, filepath.Join(dir, unifiedLogName))
	unifiedMu.Lock()
	prevFile := unifiedFile
	unifiedFile = f
	unifiedMu.Unlock()
	t.Cleanup(func() {
		unifiedMu.Lock()
		unifiedFile = prevFile
		unifiedMu.Unlock()
		f.Close()
		logDir = prevDir
	})
	return dir
}

func TestMergeLegacyLogs(t *testing.T) {
	dir := setupUnifiedForMerge(t)

	// 样本 1：app.log（Go log 格式 + [UI]/[tray] 前缀 + 无时间戳裸行）
	oldTime := time.Date(2026, 9, 4, 20, 14, 20, 0, time.Local)
	appLog := "2026/09/04 20:14:20 [tray] left click\n" +
		"2026/09/05 09:24:38 [UI] 重置服务 | clearSessions=false\n" +
		"2026/09/05 09:30:10 import: service not healthy, rolling back\n" +
		"no-timestamp bare line\n"
	writeFileAt(t, filepath.Join(dir, "app.log"), appLog, oldTime)

	// 样本 2：harness-update.log（timePrefixWriter + pnpm [WARN] + ===== 分隔头 + 空行）
	writeFileAt(t, filepath.Join(dir, "harness-update.log"),
		"2026/09/05 09:24:51 [WARN] 1 deprecated subdependencies found\n"+
			"2026/09/05 09:24:55 ===== pnpm install =====\n"+
			"2026/09/05 09:24:56 + @deepseek-ai/dsh 0.1.1-rc.2\n\n", time.Now())

	// 样本 3：server.log 空文件（应被删除且不产生行）
	writeFileAt(t, filepath.Join(dir, "server.log"), "", time.Now())

	mergeLegacyLogs()

	data := string(mustReadFile(t, filepath.Join(dir, unifiedLogName)))
	// 过滤合并摘要行（模块 log，如「已合并历史日志 …」），只对数据行断言
	var lines []string
	for _, ln := range strings.Split(strings.TrimRight(data, "\n"), "\n") {
		if !strings.Contains(ln, " [log] ") {
			lines = append(lines, ln)
		}
	}
	if len(lines) != 7 {
		t.Fatalf("expected 7 merged data lines, got %d:\n%q", len(lines), data)
	}
	re := regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) \[(INFO|WARN|ERROR)\] \[([a-z]+)\] `)
	expectModule := []string{"tray", "ui", "app", "app", "harness", "harness", "harness"}
	expectLevel := []string{"INFO", "INFO", "INFO", "INFO", "WARN", "INFO", "INFO"}
	for i, ln := range lines {
		m := re.FindStringSubmatch(ln)
		if m == nil {
			t.Fatalf("line %d missing unified prefix: %q", i, ln)
		}
		if m[2] != expectLevel[i] {
			t.Errorf("line %d level = %s, want %s (%q)", i, m[2], expectLevel[i], ln)
		}
		if m[3] != expectModule[i] {
			t.Errorf("line %d module = %s, want %s (%q)", i, m[3], expectModule[i], ln)
		}
		if i == 0 && m[1] != "2026/09/04 20:14:20" {
			t.Errorf("line 0 timestamp not preserved: %q", ln)
		}
	}
	// 无时间戳裸行的兜底时间 = 源文件 mtime（app.log 的 ModTime）
	if !strings.Contains(data, oldTime.Format("2006/01/02 15:04:05")+" [INFO] [app] no-timestamp bare line") {
		t.Errorf("bare line should use source file mtime as ts:\n%s", data)
	}
	// 源文件已删除；再跑一次幂等 no-op（统一文件行数不变）
	for _, name := range []string{"app.log", "harness-update.log", "server.log", "install.log", "build.log", "plugin-update.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("legacy %s should be removed after merge", name)
		}
	}
	before := len(string(mustReadFile(t, filepath.Join(dir, unifiedLogName))))
	mergeLegacyLogs()
	after := len(string(mustReadFile(t, filepath.Join(dir, unifiedLogName))))
	if after != before {
		t.Fatalf("merge not idempotent: %d -> %d", before, after)
	}
}

// TestMergeLegacyLogsMissingFile 缺失文件静默跳过（无删除、无错误）。
func TestMergeLegacyLogsMissingFile(t *testing.T) {
	dir := setupUnifiedForMerge(t)
	mergeLegacyLogs() // 目录无任何旧文件
	if len(string(mustReadFile(t, filepath.Join(dir, unifiedLogName)))) != 0 {
		t.Fatal("unified log should stay empty")
	}
}

func writeFileAt(t *testing.T, p, content string, mt time.Time) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(p, mt, mt)
}
