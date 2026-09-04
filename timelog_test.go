package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestTimePrefixWriter 每行都要带时间戳前缀：普通多行与无换行结尾的残留（Flush 补上）。
func TestTimePrefixWriter(t *testing.T) {
	re := regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)
	var buf bytes.Buffer
	w := newTimePrefixWriter(&buf)

	if _, err := w.Write([]byte("line one\nline two\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := w.Write([]byte("line three")); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	w.Flush() // 残留半行也要带上时间戳写出

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), out)
	}
	for _, ln := range lines {
		if !re.MatchString(ln) {
			t.Errorf("line missing timestamp prefix: %q", ln)
		}
	}
}

// TestTimePrefixWriterContent 内容保留（前缀只是加到行头，不吞内容）。
func TestTimePrefixWriterContent(t *testing.T) {
	var buf bytes.Buffer
	w := newTimePrefixWriter(&buf)
	if _, err := w.Write([]byte("alpha\nbeta")); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.Flush()
	for _, want := range []string{"alpha", "beta"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("content %q lost: %q", want, buf.String())
		}
	}
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
