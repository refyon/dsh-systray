package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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

// TestSetHarnessFamilyOverrides 逐包钉版写入两个通道（package.json 的 pnpm.overrides 与
// pnpm-workspace.yaml 顶层 overrides，后者是 pnpm ≥ v10 真正读取处），清理历史
// "@deepseek-ai/*" 通配条目，且两处都不破坏文件内其它内容。
func TestSetHarnessFamilyOverrides(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "package.json")
	ws := filepath.Join(dir, "pnpm-workspace.yaml")
	orig := "{\n  \"name\": \"deepseek-harness\",\n  \"private\": true,\n  \"pnpm\": {\n    \"onlyBuiltDependencies\": [\"node-pty\"],\n    \"overrides\": {\"@deepseek-ai/*\": \"0.1.1-rc.2\"}\n  },\n  \"dependencies\": {\n    \"@deepseek-ai/dsh\": \"0.1.1-rc.2\"\n  }\n}\n"
	if err := os.WriteFile(pkg, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	wsOrig := "onlyBuiltDependencies:\n  - node-pty\noverrides:\n  \"@deepseek-ai/*\": \"0.1.1-rc.2\"\n"
	if err := os.WriteFile(ws, []byte(wsOrig), 0o644); err != nil {
		t.Fatal(err)
	}

	names := []string{"@deepseek-ai/dsh", "@deepseek-ai/dsh-base", "@deepseek-ai/dsh-llm"}
	if err := setHarnessFamilyOverrides(dir, "0.1.2-rc.1", names); err != nil {
		t.Fatalf("set overrides: %v", err)
	}
	data, _ := os.ReadFile(pkg)
	s := string(data)
	for _, n := range names {
		if !strings.Contains(s, `"`+n+`": "0.1.2-rc.1"`) {
			t.Fatalf("family pin %s not injected: %s", n, s)
		}
	}
	if strings.Contains(s, `"@deepseek-ai/*"`) {
		t.Fatalf("legacy wildcard override should be dropped: %s", s)
	}
	if !strings.Contains(s, "onlyBuiltDependencies") || !strings.Contains(s, "0.1.1-rc.2") {
		t.Fatalf("existing fields lost: %s", s)
	}
	// workspace 通道（pnpm ≥ v10 真正读取处）：逐包钉版写入且保留既有键
	wsData, err := os.ReadFile(ws)
	if err != nil {
		t.Fatalf("pnpm-workspace.yaml should be written: %v", err)
	}
	wsS := string(wsData)
	if !strings.Contains(wsS, "overrides:") || !strings.Contains(wsS, `"@deepseek-ai/dsh-llm": "0.1.2-rc.1"`) {
		t.Fatalf("workspace overrides not injected: %s", wsS)
	}
	if strings.Contains(wsS, "@deepseek-ai/*") {
		t.Fatalf("legacy wildcard should be dropped from workspace: %s", wsS)
	}
	if !strings.Contains(wsS, "onlyBuiltDependencies") {
		t.Fatalf("workspace file should preserve unrelated keys: %s", wsS)
	}

	// 空包名列表 → 两处都移除 override（保留其余键）
	if err := setHarnessFamilyOverrides(dir, "0.1.2-rc.1", nil); err != nil {
		t.Fatalf("clear overrides: %v", err)
	}
	data, _ = os.ReadFile(pkg)
	s = string(data)
	if strings.Contains(s, `"@deepseek-ai/dsh-llm"`) {
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
	if strings.Contains(wsS2, "@deepseek-ai") {
		t.Fatalf("workspace override should be removed: %s", wsS2)
	}
	if !strings.Contains(wsS2, "onlyBuiltDependencies") {
		t.Fatalf("workspace unrelated keys should be preserved: %s", wsS2)
	}
}

// TestHarnessFamilyNames 家族包名枚举：锁文件覆盖「只作为 peer 出现」的核心包，
// node_modules/.pnpm 目录名兜底，且必须排除同 scope 但不同版本线的
// cordis / schemastery / cordis-plugin-*（钉版会把它们钉成不存在的版本）。
func TestHarnessFamilyNames(t *testing.T) {
	dir := t.TempDir()
	lock := `lockfileVersion: '9.0'

importers:

  .:
    dependencies:
      '@deepseek-ai/dsh':
        specifier: 0.1.5-rc.1
        version: 0.1.5-rc.1

packages:

  '@deepseek-ai/dsh@0.1.5-rc.1':
    resolution: {integrity: sha512-x}

  '@deepseek-ai/dsh-llm@0.1.5-rc.1(@deepseek-ai/cordis@4.0.2)':
    resolution: {integrity: sha512-y}

  '@deepseek-ai/cordis@4.0.2':
    resolution: {integrity: sha512-z}

  '@deepseek-ai/schemastery@3.18.2':
    resolution: {integrity: sha512-w}
`
	if err := os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	got := harnessFamilyNames(dir)
	want := []string{"@deepseek-ai/dsh", "@deepseek-ai/dsh-llm"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("harnessFamilyNames = %v, want %v", got, want)
	}

	// 无锁文件时回退 .pnpm 目录名（含 peer 后缀），同样排除非家族包
	dir2 := t.TempDir()
	for _, e := range []string{
		"@deepseek-ai+dsh-base@0.1.5-rc.1_@deepseek-ai+cordis@4.0.2",
		"@deepseek-ai+dsh-compaction@0.1.5-rc.1",
		"@deepseek-ai+cordis@4.0.2",
		"zod@3.25.76",
	} {
		if err := os.MkdirAll(filepath.Join(dir2, "node_modules", ".pnpm", e), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got2 := harnessFamilyNames(dir2)
	want2 := []string{"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-compaction"}
	if strings.Join(got2, ",") != strings.Join(want2, ",") {
		t.Fatalf("harnessFamilyNames(.pnpm) = %v, want %v", got2, want2)
	}

	if names := harnessFamilyNames(t.TempDir()); len(names) != 0 {
		t.Fatalf("empty dir should yield no names, got %v", names)
	}
}

// TestDropHarnessLockfile 删除旧锁文件（存在时删掉、不存在时不报错），
// 使随后的 pnpm add/install 不再复用旧解析（那正是新旧混装的另一半根因）。
func TestDropHarnessLockfile(t *testing.T) {
	prev := harnessDir
	harnessDir = t.TempDir()
	t.Cleanup(func() { harnessDir = prev })

	lock := filepath.Join(harnessDir, "pnpm-lock.yaml")
	if err := os.WriteFile(lock, []byte("lockfileVersion: '9.0'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dropHarnessLockfile()
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("lockfile should be removed, err=%v", err)
	}
	dropHarnessLockfile() // 不存在时不应 panic/报错
}

// TestHarnessInstallHint 失败归类：上游分批发布（NO_MATCHING_VERSION）与 registry 抖动
// （meta fetch fail / socket timeout）给出不同且可执行的提示，其余输出不臆测原因。
func TestHarnessInstallHint(t *testing.T) {
	cases := []struct {
		out      string
		contains string
	}{
		{"ERR_PNPM_NO_MATCHING_VERSION No matching version found for @deepseek-ai/dsh-chunked-list@^0.1.5-rc.2", "尚未发布完整"},
		{"ERR_PNPM_META_FETCH_FAIL GET https://registry.npmjs.org/x: Socket timeout", "超时"},
		{"request to https://registry.npmjs.org/y failed, reason: read ECONNRESET", "连接被重置"},
		{"some other failure", ""},
	}
	for _, c := range cases {
		got := harnessInstallHint(c.out)
		if c.contains == "" {
			if got != "" {
				t.Errorf("unexpected hint for %q: %s", c.out, got)
			}
			continue
		}
		if !strings.Contains(got, c.contains) {
			t.Errorf("hint for %q = %q, want contains %q", c.out, got, c.contains)
		}
	}
}

// TestBootVerifySettleAfterHarnessChange 改版路径的健康校验窗口必须显著长于常规窗口：
// 0.1.5-rc.1 混装树实测启动后 45s 才刷出加载错误，10s 窗口会误判成功并提升 LKG。
func TestBootVerifySettleAfterHarnessChange(t *testing.T) {
	if bootVerifySettleAfterHarnessChange < 45*time.Second {
		t.Fatalf("after-change settle = %v, want ≥ 45s", bootVerifySettleAfterHarnessChange)
	}
	if bootVerifySettleAfterHarnessChange <= bootVerifySettle {
		t.Fatalf("after-change settle (%v) must exceed the default (%v)",
			bootVerifySettleAfterHarnessChange, bootVerifySettle)
	}
}
