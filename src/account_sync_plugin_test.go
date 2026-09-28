package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeProfileFixture 造一个 profile 目录（package.json 带依赖与可选 bundles）。
func writeProfileFixture(t *testing.T, deps, bundles string) string {
	t.Helper()
	dir := t.TempDir()
	body := `{"name":"dsh-profile-web","private":true,` +
		`"dsh":{"profile":{"bundles":[` + bundles + `]}},` +
		`"dependencies":{` + deps + `}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func stubPluginImportCheck(t *testing.T, kind, reason string) {
	t.Helper()
	old := pluginImportCheckFn
	pluginImportCheckFn = func(string, string) (string, string) { return kind, reason }
	t.Cleanup(func() { pluginImportCheckFn = old })
}

// TestActivateSyncedPluginRegistersBundles 同步安装的插件必须写进 dsh.profile.bundles：
// harness 只加载激活清单里的插件，pnpm add 只写 dependencies——只装不登记就会出现
// 「关于页插件齐全、harness 会话设置→插件里找不到」（2026-09-21 现场问题）。
func TestActivateSyncedPluginRegistersBundles(t *testing.T) {
	dir := writeProfileFixture(t, `"pkg-a":"^1.0.0"`, `"@deepseek-ai/dsh-base"`)
	stubPluginImportCheck(t, "", "")

	if err := activateSyncedPlugin(dir, "pkg-a"); err != nil {
		t.Fatalf("登记应成功: %v", err)
	}
	names := profileBundleNames(dir)
	if len(names) != 1 || names[0] != "pkg-a" {
		t.Fatalf("插件应出现在激活清单（bundles）中，实际 %v", names)
	}
}

// TestActivateSyncedPluginClearsDisabledRecord 同步安装的插件若是「自动禁用」状态，
// 登记后必须解除禁用（否则装了也不会被加载）。
func TestActivateSyncedPluginClearsDisabledRecord(t *testing.T) {
	dir := t.TempDir()
	body := `{"name":"dsh-profile-web","dsh":{"profile":{"bundles":[],"disabledPlugins":{"pkg-a":"启动日志存在加载错误"}}},` +
		`"dependencies":{"pkg-a":"^1.0.0"}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	stubPluginImportCheck(t, "", "")

	if err := activateSyncedPlugin(dir, "pkg-a"); err != nil {
		t.Fatalf("登记应成功: %v", err)
	}
	if reason := profilePluginDisabledReason(dir, "pkg-a"); reason != "" {
		t.Fatalf("禁用记录应被清除，实际 %q", reason)
	}
	if names := profileBundleNames(dir); len(names) != 1 || names[0] != "pkg-a" {
		t.Fatalf("插件应写进激活清单，实际 %v", names)
	}
}

// TestActivateSyncedPluginRejectsBrokenEntry 入口确定性加载失败（求值类错误）的插件不得登记。
func TestActivateSyncedPluginRejectsBrokenEntry(t *testing.T) {
	dir := writeProfileFixture(t, `"pkg-a":"^1.0.0"`, `"@deepseek-ai/dsh-base"`)
	stubPluginImportCheck(t, "import-error", "does not provide an export named 'assertNever'")

	if err := activateSyncedPlugin(dir, "pkg-a"); err == nil {
		t.Fatal("入口加载失败应报错（由调用方回退快照）")
	}
}

// TestVerifySyncedPluginDefersHostDependency 宿主包缺失（预检环境必然报错）不判失败：
// 与导入流程的预检同口径，交启动校验裁决。
func TestVerifySyncedPluginDefersHostDependency(t *testing.T) {
	dir := writeProfileFixture(t, `"pkg-a":"^1.0.0"`, `"pkg-a"`)
	stubPluginImportCheck(t, "resolve-error", "Cannot find package '@deepseek-ai/dsh-home-paths' imported from x")

	if err := verifySyncedPluginLoadable(dir, "pkg-a"); err != nil {
		t.Fatalf("宿主包缺失应延后裁决（不算不兼容）：%v", err)
	}
}

// TestPluginApplySpecUsesExactTargetVersion 同步应用要按「账号里的目标版本」安装，而不是照搬
// 账号里的范围 spec：`pnpm add <name>@<范围>` 在锁定版本已满足该范围时不升级（pnpm 实测），
// 会让「重启生效」变成静默空操作、同一项无限回到待生效（2026-09-28 现场：另一台机器升到
// 1.7.40，本机反复应用仍是 1.7.35）。
func TestPluginApplySpecUsesExactTargetVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	writeTestProfile(t, home, "web", map[string]string{"pkg-a": "1.7.35"},
		map[string]string{"pkg-a": "1.7.35"})
	dir := filepath.Join(home, "profiles", "web")

	cases := []struct {
		desc      string
		plugin    string
		value     pluginOpValue
		wantSpec  string
		wantExact string
	}{
		{"目标版本更新 → 按精确版本安装", "pkg-a",
			pluginOpValue{Action: "update", Spec: "^1.7.35", Source: "npm", Version: "1.7.40"}, "1.7.40", "1.7.40"},
		{"版本已满足 → 仍用账号 spec", "pkg-a",
			pluginOpValue{Action: "update", Spec: "^1.7.35", Source: "npm", Version: "1.7.35"}, "^1.7.35", ""},
		{"目标版本更旧 → 不改判（不降级）", "pkg-a",
			pluginOpValue{Action: "update", Spec: "^1.7.30", Source: "npm", Version: "1.7.30"}, "^1.7.30", ""},
		{"未安装 → 按精确版本安装", "pkg-new",
			pluginOpValue{Action: "install", Spec: "^2.0.0", Source: "npm", Version: "2.0.3"}, "2.0.3", "2.0.3"},
		{"github 来源不得换成 npm 精确版本", "pkg-a",
			pluginOpValue{Action: "update", Spec: "git+https://github.com/o/r.git", Source: "github", Version: "9.9.9"},
			"git+https://github.com/o/r.git", ""},
		{"无 spec 时退回目标版本", "pkg-a",
			pluginOpValue{Action: "update", Source: "npm", Version: "1.7.40"}, "1.7.40", "1.7.40"},
	}
	for _, c := range cases {
		spec, exact := pluginApplySpec(dir, c.plugin, c.value)
		if spec != c.wantSpec || exact != c.wantExact {
			t.Fatalf("%s：spec=%q exact=%q，期望 %q/%q", c.desc, spec, exact, c.wantSpec, c.wantExact)
		}
	}
}

// TestVerifyInstalledTargetVersion 精确安装后的校验：没达到目标版本必须报错——否则「应用成功」
// 会把同一项交回漂移重判，用户看到的是「一直提示有 1 项待同步」却无从知道原因。
func TestVerifyInstalledTargetVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	writeTestProfile(t, home, "web", map[string]string{"pkg-a": "1.7.35"},
		map[string]string{"pkg-a": "1.7.35"})
	dir := filepath.Join(home, "profiles", "web")

	if err := verifyInstalledTargetVersion(dir, "pkg-a", "1.7.35"); err != nil {
		t.Fatalf("版本已达标不应报错：%v", err)
	}
	if err := verifyInstalledTargetVersion(dir, "pkg-a", "1.7.40"); err == nil {
		t.Fatal("版本未达标必须报错（否则同一项会无限回到待生效）")
	}
}

// TestRemovePluginStripsBundleEntry 卸载必须同时摘除激活声明：残留会让服务启动报
// 「cannot resolve profile bundle」硬失败。
func TestRemovePluginStripsBundleEntry(t *testing.T) {
	dir := writeProfileFixture(t, `"pkg-a":"^1.0.0"`, `"@deepseek-ai/dsh-base","pkg-a"`)
	if err := stripProfileBundleEntry(dir, "pkg-a"); err != nil {
		t.Fatalf("摘除激活声明失败: %v", err)
	}
	if names := profileBundleNames(dir); len(names) != 0 {
		t.Fatalf("卸载后不应再出现在激活清单：%v", names)
	}
}
