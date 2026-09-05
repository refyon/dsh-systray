package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestModuleLogWriterFormat 每行都要带统一前缀（时间戳 + 等级 + 模块）：
// 多行逐行落盘、无换行结尾的残留由 Flush 补出。
func TestModuleLogWriterFormat(t *testing.T) {
	dir := t.TempDir()
	f := mustOpenFile(t, filepath.Join(dir, unifiedLogName))
	unifiedMu.Lock()
	prev := unifiedFile
	unifiedFile = f
	unifiedMu.Unlock()
	t.Cleanup(func() {
		unifiedMu.Lock()
		unifiedFile = prev
		unifiedMu.Unlock()
		f.Close() // 先释放测试句柄，TempDir 才能清理文件
	})

	re := regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} \[(INFO|WARN|ERROR)\] \[server\] `)
	w := newModuleLogWriter("server")
	if _, err := w.Write([]byte("line one\nline two\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := w.Write([]byte("line three")); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	w.Flush() // 残留半行也要带上前缀写出

	data := mustReadFile(t, filepath.Join(dir, unifiedLogName))
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), data)
	}
	for _, ln := range lines {
		if !re.MatchString(ln) {
			t.Errorf("line missing unified prefix: %q", ln)
		}
	}
}

// TestModuleLogWriterLevelAndContent 内容保留且等级按内容启发判定（failed→ERROR）。
func TestModuleLogWriterLevelAndContent(t *testing.T) {
	dir := t.TempDir()
	f := mustOpenFile(t, filepath.Join(dir, unifiedLogName))
	unifiedMu.Lock()
	prev := unifiedFile
	unifiedFile = f
	unifiedMu.Unlock()
	t.Cleanup(func() {
		unifiedMu.Lock()
		unifiedFile = prev
		unifiedMu.Unlock()
		f.Close() // 先释放测试句柄，TempDir 才能清理文件
	})

	w := newModuleLogWriter("profile")
	if _, err := w.Write([]byte("alpha\nERR_PNPM_NO_MATCHING_VERSION beta")); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.Flush()
	out := string(mustReadFile(t, filepath.Join(dir, unifiedLogName)))
	for _, want := range []string{"alpha", "beta", "ERR_PNPM_NO_MATCHING_VERSION"} {
		if !strings.Contains(out, want) {
			t.Errorf("content %q lost: %q", want, out)
		}
	}
	// 无换行尾行的等级也要按内容判定（pnpm 报错 → ERROR）
	for _, ln := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.Contains(ln, "alpha") && !strings.Contains(ln, "[INFO]") {
			t.Errorf("info line level wrong: %q", ln)
		}
		if strings.Contains(ln, "ERR_PNPM") && !strings.Contains(ln, "[ERROR]") {
			t.Errorf("error line level wrong: %q", ln)
		}
	}
}

func mustOpenFile(t *testing.T, p string) *os.File {
	t.Helper()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func mustReadFile(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestSetHarnessOverrides 注入/移除 pnpm.overrides["@deepseek-ai/*"]（package.json + 保留既有字段），
// 且同步维护 pnpm-workspace.yaml 顶层的 overrides 块（pnpm ≥ v10 只读该文件，2026-09 实测 v11.7.0），
// 两处都不破坏文件内其它内容。
func TestSetHarnessOverrides(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "package.json")
	ws := filepath.Join(dir, "pnpm-workspace.yaml")
	orig := "{\n  \"name\": \"deepseek-harness\",\n  \"private\": true,\n  \"pnpm\": {\n    \"onlyBuiltDependencies\": [\"node-pty\"]\n  },\n  \"dependencies\": {\n    \"@deepseek-ai/dsh\": \"0.1.1-rc.2\"\n  }\n}\n"
	if err := os.WriteFile(pkg, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	wsOrig := "onlyBuiltDependencies:\n  - node-pty\n"
	if err := os.WriteFile(ws, []byte(wsOrig), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := setHarnessOverrides(dir, "0.1.2-rc.1"); err != nil {
		t.Fatalf("set overrides: %v", err)
	}
	data, _ := os.ReadFile(pkg)
	s := string(data)
	if !strings.Contains(s, `"overrides"`) || !strings.Contains(s, `"@deepseek-ai/*": "0.1.2-rc.1"`) {
		t.Fatalf("overrides not injected: %s", s)
	}
	if !strings.Contains(s, "onlyBuiltDependencies") || !strings.Contains(s, "0.1.1-rc.2") {
		t.Fatalf("existing fields lost: %s", s)
	}
	// workspace 通道（pnpm ≥ v10 真正生效处）：overrides 加入且保留既有键
	wsData, err := os.ReadFile(ws)
	if err != nil {
		t.Fatalf("pnpm-workspace.yaml should be written: %v", err)
	}
	wsS := string(wsData)
	if !strings.Contains(wsS, "overrides:") || !strings.Contains(wsS, `"@deepseek-ai/*": "0.1.2-rc.1"`) {
		t.Fatalf("workspace overrides not injected: %s", wsS)
	}
	if !strings.Contains(wsS, "onlyBuiltDependencies") {
		t.Fatalf("workspace file should preserve unrelated keys: %s", wsS)
	}

	// ver 为空 → 两处都移除 override（保留其余键）
	if err := setHarnessOverrides(dir, ""); err != nil {
		t.Fatalf("clear overrides: %v", err)
	}
	data, _ = os.ReadFile(pkg)
	s = string(data)
	if strings.Contains(s, `"@deepseek-ai/*"`) {
		t.Fatalf("override should be removed: %s", s)
	}
	if !strings.Contains(s, "onlyBuiltDependencies") {
		t.Fatalf("existing fields lost after removal: %s", s)
	}
	wsData2, err := os.ReadFile(ws)
	if err != nil {
		t.Fatalf("pnpm-workspace.yaml should still exist (keeps onlyBuiltDependencies): %v", err)
	}
	wsS2 := string(wsData2)
	if strings.Contains(wsS2, "@deepseek-ai/*") {
		t.Fatalf("workspace override should be removed: %s", wsS2)
	}
	if !strings.Contains(wsS2, "onlyBuiltDependencies") {
		t.Fatalf("workspace unrelated keys should be preserved: %s", wsS2)
	}
}
