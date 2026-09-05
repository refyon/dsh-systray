package main

// 插件禁用 / 启用机制（不兼容自愈）的单元测试：禁用/启用往返、导出合并跳过禁用、
// 恢复侧消毒清理禁用记录、原因截断等（临时目录自包含，不触碰真实 profile 与日志）。

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

type pendingLocalFixture struct {
	Spec    string `json:"spec"`
	Bundled bool   `json:"bundled"`
}

type profileFixture struct {
	Dependencies map[string]string `json:"dependencies"`
	Dsh          struct {
		Profile struct {
			Bundles             []string                       `json:"bundles"`
			DisabledPlugins     map[string]string              `json:"disabledPlugins"`
			PendingLocalPlugins map[string]pendingLocalFixture `json:"pendingLocalPlugins"`
		} `json:"profile"`
	} `json:"dsh"`
}

func writePkgJSON(t *testing.T, dir, content string) {
	t.Helper()
	writeTestJSON(t, filepath.Join(dir, "package.json"), content)
}

func readPkgFixture(t *testing.T, dir string) profileFixture {
	t.Helper()
	var f profileFixture
	if err := json.Unmarshal([]byte(readTestJSON(t, filepath.Join(dir, "package.json"))), &f); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f
}

func TestDisableEnableRoundtrip(t *testing.T) {
	dir := t.TempDir()
	writePkgJSON(t, dir, `{"name":"web","dependencies":{"alpha":"^1.0.0","beta":"^2.0.0"},
	  "dsh":{"profile":{"bundles":["alpha","beta"]}}}`)
	if err := disablePluginInProfile(dir, "alpha", "缺失 settingsNamespace 导出"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	f := readPkgFixture(t, dir)
	if f.Dependencies["alpha"] != "^1.0.0" {
		t.Fatalf("dependency must be kept: %+v", f.Dependencies)
	}
	if len(f.Dsh.Profile.Bundles) != 1 || f.Dsh.Profile.Bundles[0] != "beta" {
		t.Fatalf("bundles after disable = %+v", f.Dsh.Profile.Bundles)
	}
	if f.Dsh.Profile.DisabledPlugins["alpha"] != "缺失 settingsNamespace 导出" {
		t.Fatalf("disabled reason missing: %+v", f.Dsh.Profile.DisabledPlugins)
	}
	if reason := profilePluginDisabledReason(dir, "alpha"); reason != "缺失 settingsNamespace 导出" {
		t.Fatalf("reason = %q", reason)
	}
	if reason := profilePluginDisabledReason(dir, "beta"); reason != "" {
		t.Fatalf("beta unexpectedly disabled: %q", reason)
	}
	// 启用
	if err := enablePluginInProfile(dir, "alpha"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	f = readPkgFixture(t, dir)
	if len(f.Dsh.Profile.DisabledPlugins) != 0 {
		t.Fatalf("disabledPlugins not cleared: %+v", f.Dsh.Profile.DisabledPlugins)
	}
	found := false
	for _, b := range f.Dsh.Profile.Bundles {
		if b == "alpha" {
			found = true
		}
	}
	if !found {
		t.Fatalf("alpha not back in bundles: %+v", f.Dsh.Profile.Bundles)
	}
	// 幂等：再启用不报错也不重复追加
	if err := enablePluginInProfile(dir, "alpha"); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	f = readPkgFixture(t, dir)
	cnt := 0
	for _, b := range f.Dsh.Profile.Bundles {
		if b == "alpha" {
			cnt++
		}
	}
	if cnt != 1 {
		t.Fatalf("alpha duplicated in bundles: %+v", f.Dsh.Profile.Bundles)
	}
}

func TestDisableSkipWhenDepMissing(t *testing.T) {
	dir := t.TempDir()
	writePkgJSON(t, dir, `{"dependencies":{"keep":"^1.0.0"}}`)
	if err := disablePluginInProfile(dir, "ghost", "无依赖不可禁用"); err != nil {
		t.Fatalf("disable missing dep should not error: %v", err)
	}
	if got := readTestJSON(t, filepath.Join(dir, "package.json")); strings.Contains(got, "ghost") {
		t.Fatalf("unexpected write: %s", got)
	}
}

func TestClearDisabledRecord(t *testing.T) {
	dir := t.TempDir()
	writePkgJSON(t, dir, `{"dependencies":{"x":"1.0.0"},"dsh":{"profile":{"bundles":["x"],"disabledPlugins":{"x":"r"}}}}`)
	if err := clearProfileDisabledRecord(dir, "x"); err != nil {
		t.Fatal(err)
	}
	f := readPkgFixture(t, dir)
	if len(f.Dsh.Profile.DisabledPlugins) != 0 {
		t.Fatalf("empty map should be removed: %+v", f.Dsh.Profile.DisabledPlugins)
	}
	if got := readTestJSON(t, filepath.Join(dir, "package.json")); strings.Contains(got, "disabledPlugins") {
		t.Fatalf("empty map field left in file: %s", got)
	}
	if len(f.Dsh.Profile.Bundles) != 1 || f.Dsh.Profile.Bundles[0] != "x" {
		t.Fatalf("bundles touched: %+v", f.Dsh.Profile.Bundles)
	}
}

func TestPluginDisabledAcross(t *testing.T) {
	d1 := t.TempDir()
	d2 := t.TempDir()
	writePkgJSON(t, d1, `{"dependencies":{"p":"1"}}`)
	writePkgJSON(t, d2, `{"dependencies":{"p":"1"},"dsh":{"profile":{"disabledPlugins":{"p":"reason"}}}}`)
	dis, reason := pluginDisabledAcross([]string{d1, d2}, "p")
	if !dis || reason != "reason" {
		t.Fatalf("want disabled reason=reason, got (%v,%q)", dis, reason)
	}
	dis, reason = pluginDisabledAcross([]string{d1}, "p")
	if dis {
		t.Fatal("d1 should not be disabled")
	}
}

func TestMergeActivatesRestoredBundlesUnconditionally(t *testing.T) {
	dir := t.TempDir()
	writePkgJSON(t, dir, `{"name":"web","dependencies":{"existing":"^1"},
	  "dsh":{"profile":{"bundles":["existing"],"disabledPlugins":{"alpha":"target-old-reason","untouched":"u"}}}}`)
	cfg := exportPlugins{
		Profile:      "web",
		Dependencies: map[string]string{"alpha": "^1.0.0", "beta": "github:u/b", "dep-only": "1.0.0"},
		Bundles:      []string{"alpha", "beta"},
		Disabled:     map[string]string{"alpha": "source-reason"}, // 恢复侧不再采用源机禁用原因
	}
	if err := mergePluginConfigIntoProfile(dir, cfg); err != nil {
		t.Fatal(err)
	}
	f := readPkgFixture(t, dir)
	has := func(name string) bool {
		for _, b := range f.Dsh.Profile.Bundles {
			if b == name {
				return true
			}
		}
		return false
	}
	// cfg.Bundles 覆盖名无条件激活（即使目标上曾禁用 / 源机带禁用原因）
	if !has("alpha") || !has("beta") {
		t.Fatalf("restored bundles not activated: %+v", f.Dsh.Profile.Bundles)
	}
	// 依赖存在但源机未激活（不在 cfg.Bundles）的插件：不自动激活
	if has("dep-only") {
		t.Fatalf("dep-only plugin must not be auto-activated: %+v", f.Dsh.Profile.Bundles)
	}
	// 被激活名字的目标禁用记录被清除；无关禁用记录保留
	if _, ok := f.Dsh.Profile.DisabledPlugins["alpha"]; ok {
		t.Fatalf("alpha disabled record should be cleared: %+v", f.Dsh.Profile.DisabledPlugins)
	}
	if f.Dsh.Profile.DisabledPlugins["untouched"] != "u" {
		t.Fatalf("unrelated disabled record lost: %+v", f.Dsh.Profile.DisabledPlugins)
	}
	if _, ok := f.Dsh.Profile.DisabledPlugins["dep-only"]; ok {
		t.Fatalf("dep-only disabled record cleared unexpectedly: %+v", f.Dsh.Profile.DisabledPlugins)
	}
	// 依赖照常合入
	if f.Dependencies["alpha"] != "^1.0.0" || f.Dependencies["dep-only"] != "1.0.0" {
		t.Fatalf("deps mismatch: %+v", f.Dependencies)
	}
}

func TestMergeClearsEmptyDisabledMapField(t *testing.T) {
	dir := t.TempDir()
	writePkgJSON(t, dir, `{"dependencies":{"a":"^1"},"dsh":{"profile":{"disabledPlugins":{"a":"r"}}}}`)
	cfg := exportPlugins{Dependencies: map[string]string{"a": "^1"}, Bundles: []string{"a"}}
	if err := mergePluginConfigIntoProfile(dir, cfg); err != nil {
		t.Fatal(err)
	}
	f := readPkgFixture(t, dir)
	if len(f.Dsh.Profile.DisabledPlugins) != 0 {
		t.Fatalf("disabled map should be empty: %+v", f.Dsh.Profile.DisabledPlugins)
	}
	if got := readTestJSON(t, filepath.Join(dir, "package.json")); strings.Contains(got, "disabledPlugins") {
		t.Fatalf("empty disabledPlugins field left in file: %s", got)
	}
}

func TestProfilePluginConfigCarriesDisabled(t *testing.T) {
	dir := t.TempDir()
	writePkgJSON(t, dir, `{"dependencies":{"a":"^1"},"dsh":{"profile":{"bundles":["b"],"disabledPlugins":{"a":"why"}}}}`)
	cfg := profilePluginConfig(dir)
	if cfg.Disabled["a"] != "why" {
		t.Fatalf("disabled not carried: %+v", cfg.Disabled)
	}
	if len(cfg.Bundles) != 1 || cfg.Bundles[0] != "b" {
		t.Fatalf("bundles mismatch: %+v", cfg.Bundles)
	}
}

func TestSanitizeMissingTargetMovesToPending(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "dev-only") // 不存在
	writePkgJSON(t, dir, `{"dependencies":{"dev-only":"link:`+filepath.ToSlash(missing)+`","keep":"^1.0.0"},
	  "dsh":{"profile":{"bundles":["dev-only","keep"],"disabledPlugins":{"dev-only":"旧原因"}}}}`)
	notes := sanitizeProfileLocalDeps(dir)
	if len(notes) != 1 {
		t.Fatalf("want 1 note, got %v", notes)
	}
	f := readPkgFixture(t, dir)
	if _, ok := f.Dependencies["dev-only"]; ok {
		t.Fatalf("dep not moved out of dependencies: %+v", f.Dependencies)
	}
	for _, b := range f.Dsh.Profile.Bundles {
		if b == "dev-only" {
			t.Fatalf("bundle not stripped: %+v", f.Dsh.Profile.Bundles)
		}
	}
	if len(f.Dsh.Profile.DisabledPlugins) != 0 {
		t.Fatalf("disabled record not cleaned: %+v", f.Dsh.Profile.DisabledPlugins)
	}
	p, ok := f.Dsh.Profile.PendingLocalPlugins["dev-only"]
	if !ok {
		t.Fatalf("pendingLocalPlugins missing dev-only: %+v", f.Dsh.Profile.PendingLocalPlugins)
	}
	if !strings.Contains(p.Spec, "dev-only") || p.Bundled != true {
		t.Fatalf("pending record wrong: %+v", p)
	}
	if _, ok := f.Dependencies["keep"]; !ok {
		t.Fatalf("keep dep touched: %+v", f.Dependencies)
	}
	keepSeen := false
	for _, b := range f.Dsh.Profile.Bundles {
		if b == "keep" {
			keepSeen = true
		}
	}
	if !keepSeen {
		t.Fatalf("unrelated bundle touched: %+v", f.Dsh.Profile.Bundles)
	}
}

func TestTrimHintLine(t *testing.T) {
	long := strings.Repeat("x", 500)
	out := trimHintLine("  a\t\tb  \n" + long)
	if strings.Contains(out, "\t") || strings.Contains(out, "\n") || strings.Contains(out, "  ") {
		t.Fatalf("not collapsed: %q", out)
	}
	if len([]rune(out)) > 222 {
		t.Fatalf("too long: %d", len([]rune(out)))
	}
	if !strings.HasSuffix(out, "…") {
		t.Fatalf("missing ellipsis: %q", out)
	}
}

func TestStripProfileBundleEntry(t *testing.T) {
	dir := t.TempDir()
	writePkgJSON(t, dir, `{"dependencies":{"a":"^1"},"dsh":{"profile":{"bundles":["a","b@0.1.0"]}}}`)
	if err := stripProfileBundleEntry(dir, "a"); err != nil {
		t.Fatal(err)
	}
	f := readPkgFixture(t, dir)
	if len(f.Dsh.Profile.Bundles) != 1 || f.Dsh.Profile.Bundles[0] != "b@0.1.0" {
		t.Fatalf("bundles wrong: %+v", f.Dsh.Profile.Bundles)
	}
	if f.Dependencies["a"] != "^1" {
		t.Fatalf("dependency must be kept: %+v", f.Dependencies)
	}
}

func TestStripUnresolvedBundles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	prof := filepath.Join(home, "profiles", "web")
	writeTestJSON(t, filepath.Join(prof, "package.json"),
		`{"dependencies":{},"dsh":{"profile":{"bundles":["deepseek-idesign","keep"]}}}`)
	// 官方包名（@deepseek-ai/*）不摘除；用户包摘除
	if n := stripUnresolvedBundles([]string{"deepseek-idesign", "@deepseek-ai/official"}); n != 1 {
		t.Fatalf("want 1 stripped, got %d", n)
	}
	f := readPkgFixture(t, prof)
	if len(f.Dsh.Profile.Bundles) != 1 || f.Dsh.Profile.Bundles[0] != "keep" {
		t.Fatalf("bundles wrong: %+v", f.Dsh.Profile.Bundles)
	}
}
