package main

import (
	"runtime"
	"strings"
	"testing"
)

// TestShotModeSanitization 验证截图模式脱敏行为（无需真实 GUI）：
// - 常规页 Harness 目录固定显示脱敏的 .dsh 目录（需求 7a）；
// - 日志页返回内置演示日志，不泄露真实路径/用户名（需求 8）。
func TestShotModeSanitization(t *testing.T) {
	old := shotMode
	shotMode = true
	defer func() { shotMode = old }()

	cfg := app.GetConfig()
	want := `C:\Users\demo\.dsh`
	if runtime.GOOS == "darwin" {
		want = "/Users/demo/.dsh"
	}
	if cfg.HarnessDir != want {
		t.Fatalf("shot mode harness dir = %q, want %q", cfg.HarnessDir, want)
	}

	tail := app.ReadLogTail(unifiedLogName, 0)
	if len(tail.Lines) == 0 {
		t.Fatal("shot mode log should not be empty")
	}
	joined := strings.Join(tail.Lines, "\n")
	if strings.Contains(joined, "lenovo") || strings.Contains(joined, "agent-env") ||
		strings.Contains(joined, "deepseek-harness") {
		t.Fatalf("shot mode log leaks real data: %q", joined)
	}
	for _, ln := range tail.Lines {
		if !strings.Contains(ln, "INFO") && !strings.Contains(ln, "WARN") {
			t.Fatalf("unexpected demo log line: %q", ln)
		}
	}
}
