package main

import (
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// absPathRe 匹配绝对路径（盘符路径、macOS/Linux 家目录）。
// 演示日志里允许出现的绝对路径只能是演示目录（C:\Users\demo / /Users/demo），
// 其余一律视为泄漏——判定不依赖任何真实用户名或工作区名（规则 1.7）。
var absPathRe = regexp.MustCompile(`[A-Za-z]:\\[^\s"'<>|]*|/(?:Users|home)/[^\s"'<>|]*`)

func demoOnlyPath(s string) string {
	for _, m := range absPathRe.FindAllString(s, -1) {
		if strings.HasPrefix(strings.ToLower(strings.ReplaceAll(m, "/", `\`)), `c:\users\demo`) {
			continue
		}
		return m
	}
	return ""
}

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
	if bad := demoOnlyPath(joined); bad != "" {
		t.Fatalf("shot mode log leaks real path %q: %q", bad, joined)
	}
	for _, ln := range tail.Lines {
		if !strings.Contains(ln, "INFO") && !strings.Contains(ln, "WARN") {
			t.Fatalf("unexpected demo log line: %q", ln)
		}
	}
}
