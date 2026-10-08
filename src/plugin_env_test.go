package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// rowsByName 把插件行按包名索引（测试断言用）。
func rowsByName(rows []PluginRow) map[string]PluginRow {
	out := map[string]PluginRow{}
	for _, r := range rows {
		out[r.Name] = r
	}
	return out
}

// TestActivePluginRowsFollowLaunchTarget 关于页插件清单按启动方式过滤：Web 启动只见
// profiles/web 的插件，Desktop 启动只见 profiles/desktop 的；作用目录、版本、禁用状态都按
// 当前环境重算（同一插件在两套环境里可能版本不同）。
func TestActivePluginRowsFollowLaunchTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	writeTestProfile(t, home, "web", map[string]string{
		"pkg-web":  "^1.0.0",
		"pkg-both": "^1.0.0",
	}, map[string]string{"pkg-web": "1.0.0", "pkg-both": "1.0.0"})
	writeTestProfile(t, home, "desktop", map[string]string{
		"pkg-desk": "^2.0.0",
		"pkg-both": "^1.0.0", // 同名同 spec：buildPluginRows 合并为一行
	}, map[string]string{"pkg-desk": "2.0.0", "pkg-both": "3.0.0"})

	webDir := filepath.Join(home, "profiles", "web")
	deskDir := filepath.Join(home, "profiles", "desktop")

	t.Run("web", func(t *testing.T) {
		stubDesktopPref(t, launchTargetWeb)
		rows := activePluginRows()
		if len(rows) != 2 {
			t.Fatalf("Web 环境应只有 pkg-web/pkg-both 两行：%+v", rows)
		}
		by := rowsByName(rows)
		if _, ok := by["pkg-desk"]; ok {
			t.Fatal("只装在 Desktop 环境的插件不应出现在 Web 清单里")
		}
		both := by["pkg-both"]
		if len(both.Locs) != 1 || !sameProfileDir(both.Locs[0], webDir) {
			t.Fatalf("行内作用目录应只剩 Web 环境：%v", both.Locs)
		}
		if both.Version != "1.0.0" {
			t.Fatalf("版本应按 Web 环境取：got %q", both.Version)
		}
		if both.Profile != "" {
			t.Fatalf("清单已按环境过滤，不应再带环境标注：%q", both.Profile)
		}
	})

	t.Run("desktop", func(t *testing.T) {
		stubDesktopPref(t, launchTargetDesktop)
		rows := activePluginRows()
		if len(rows) != 2 {
			t.Fatalf("Desktop 环境应只有 pkg-desk/pkg-both 两行：%+v", rows)
		}
		by := rowsByName(rows)
		if _, ok := by["pkg-web"]; ok {
			t.Fatal("只装在 Web 环境的插件不应出现在 Desktop 清单里")
		}
		both := by["pkg-both"]
		if len(both.Locs) != 1 || !sameProfileDir(both.Locs[0], deskDir) {
			t.Fatalf("行内作用目录应只剩 Desktop 环境：%v", both.Locs)
		}
		if both.Version != "3.0.0" {
			t.Fatalf("版本应按 Desktop 环境取：got %q", both.Version)
		}
	})
}

// TestFindActivePluginRowScopesToEnv 行内操作解析：只认当前环境的行，另一环境独有的插件不可操作。
func TestFindActivePluginRowScopesToEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	writeTestProfile(t, home, "web", map[string]string{"pkg-both": "^1.0.0"},
		map[string]string{"pkg-both": "1.0.0"})
	writeTestProfile(t, home, "desktop", map[string]string{"pkg-both": "^1.0.0", "pkg-desk": "^2.0.0"},
		map[string]string{"pkg-both": "3.0.0", "pkg-desk": "2.0.0"})

	stubDesktopPref(t, launchTargetWeb)
	row, ok := findActivePluginRowByID("pkg-both")
	if !ok {
		t.Fatal("Web 环境应能解析到 pkg-both")
	}
	if len(row.Locs) != 1 || !sameProfileDir(row.Locs[0], filepath.Join(home, "profiles", "web")) {
		t.Fatalf("作用目录应只剩 Web 环境：%v", row.Locs)
	}
	if row.Version != "1.0.0" {
		t.Fatalf("版本应按 Web 环境取：got %q", row.Version)
	}
	if _, ok := findActivePluginRowByID("pkg-desk"); ok {
		t.Fatal("只装在 Desktop 环境的插件在 Web 启动方式下不应可操作")
	}
}

// TestFindActivePluginRowByIDOrNameSameNameTwoSpecs 同名插件在两套环境里 spec 不同（版本钉法不同）
// 时清单会分成两行、行 ID 带 spec；行内操作传「行 ID」或旧的「裸包名」都必须落到当前环境那一行，
// 不能因为裸包名解析先撞上另一环境的行而报「未找到该插件」（2026-10-08 现场：桌面端 → Web UI 后
// dsh-ui-taste / restrict-discipline 检查更新报未找到，dsh-cost-meter 因两环境 spec 相同合并成一行而正常）。
func TestFindActivePluginRowByIDOrNameSameNameTwoSpecs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	webSpec := "github:refyon/plugin-a"
	deskSpec := "github:refyon/plugin-a#41ad0c9873e578ab950ba9eb74539fc043df2852"
	writeTestProfile(t, home, "web", map[string]string{"plugin-a": webSpec},
		map[string]string{"plugin-a": "0.3.0"})
	writeTestProfile(t, home, "desktop", map[string]string{"plugin-a": deskSpec},
		map[string]string{"plugin-a": "0.3.0"})

	webDir := filepath.Join(home, "profiles", "web")
	deskDir := filepath.Join(home, "profiles", "desktop")
	webID := "plugin-a|" + webSpec
	deskID := "plugin-a|" + deskSpec

	stubDesktopPref(t, launchTargetWeb)
	for _, id := range []string{webID, "plugin-a"} {
		row, ok := findActivePluginRowByIDOrName(id)
		if !ok {
			t.Fatalf("Web 环境下 %q 应能解析到行", id)
		}
		if len(row.Locs) != 1 || !sameProfileDir(row.Locs[0], webDir) {
			t.Fatalf("%q 应落在 Web 环境：locs=%v", id, row.Locs)
		}
		if row.ID != webID || row.Spec != webSpec {
			t.Fatalf("%q 应解析到 Web 环境的行：id=%q spec=%q", id, row.ID, row.Spec)
		}
	}
	if _, ok := findActivePluginRowByIDOrName(deskID); ok {
		t.Fatal("Desktop 环境的行 ID 在 Web 启动方式下不应可操作")
	}

	stubDesktopPref(t, launchTargetDesktop)
	row, ok := findActivePluginRowByIDOrName("plugin-a")
	if !ok || !sameProfileDir(row.Locs[0], deskDir) || row.ID != deskID {
		t.Fatalf("Desktop 环境下裸包名应解析到 Desktop 行：ok=%v row=%+v", ok, row)
	}
}

// TestFindActivePluginRowByIDOrNameDisabledEnv 首行 ID 不以包名开头时（被自动禁用、无依赖声明而
// 合成的行），裸包名仍需回退到当前环境里真正的依赖行。
func TestFindActivePluginRowByIDOrNameDisabledEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	writeTestProfile(t, home, "web", map[string]string{"plugin-c": "^1.0.0"},
		map[string]string{"plugin-c": "1.0.0"})
	deskDir := filepath.Join(home, "profiles", "desktop")
	writeTestProfile(t, home, "desktop", map[string]string{}, map[string]string{})
	// Desktop 侧只有一条禁用记录：清单合成 ID 不带包名的行
	body := `{"name":"dsh-profile-desktop","dependencies":{},` +
		`"dsh":{"profile":{"bundles":[],"disabledPlugins":{"plugin-c":"与当前版本不兼容"}}}}`
	if err := os.WriteFile(filepath.Join(deskDir, "package.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	stubDesktopPref(t, launchTargetWeb)
	row, ok := findActivePluginRowByIDOrName("plugin-c")
	if !ok {
		t.Fatal("Web 环境下裸包名应解析到依赖行")
	}
	if row.Disabled || !sameProfileDir(row.Locs[0], filepath.Join(home, "profiles", "web")) {
		t.Fatalf("应收窄到 Web 环境的启用行：%+v", row)
	}
}

// TestExportCollectsAllProfiles 方案 B：导出一次带走**全部** harness 环境的插件——
// manifest 的 plugins.profiles 按环境分组（web / desktop 各自一份清单），plugins.zip 内保留
// 各环境自己的 profiles/<name>/node_modules/ 前缀；顶层 plugins 段留给旧版托盘（= 导出机当前
// 启动方式那一套）。
func TestExportCollectsAllProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	writeTestProfile(t, home, "web", map[string]string{"pkg-web": "^1.0.0"},
		map[string]string{"pkg-web": "1.0.0"})
	writeTestProfile(t, home, "desktop", map[string]string{"pkg-desk": "^2.0.0"},
		map[string]string{"pkg-desk": "2.0.0"})

	stubDesktopPref(t, launchTargetWeb) // 导出机当前启动方式：Web UI
	dest := filepath.Join(home, "exports")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	master, err := buildExportZip(false, true, false, nil, dest, nil, "")
	if err != nil {
		t.Fatalf("buildExportZip: %v", err)
	}

	mfData, err := zipReadFile(master, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m exportManifest
	if err := json.Unmarshal(mfData, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Plugins.Profiles) != 2 {
		t.Fatalf("应导出全部环境：%+v", m.Plugins.Profiles)
	}
	if _, ok := m.Plugins.Profiles["web"].Dependencies["pkg-web"]; !ok {
		t.Fatalf("web 环境清单缺失：%+v", m.Plugins.Profiles["web"])
	}
	if _, ok := m.Plugins.Profiles["desktop"].Dependencies["pkg-desk"]; !ok {
		t.Fatalf("desktop 环境清单缺失：%+v", m.Plugins.Profiles["desktop"])
	}
	if m.Plugins.Profile != "web" || m.Plugins.Dependencies["pkg-web"] == "" {
		t.Fatalf("顶层 plugins 段应为当前环境（web）：%+v", m.Plugins)
	}

	inner := mustInner(t, master, exportZipPlugins)
	wantPrefixes := []string{"profiles/desktop/node_modules/", "profiles/web/node_modules/"}
	if got := zipPluginPrefixes(inner); !reflect.DeepEqual(got, wantPrefixes) {
		t.Fatalf("包内应带两套环境前缀 %v：got %v", wantPrefixes, got)
	}
}

// TestPluginImportTargetsFollowBundleProfiles 方案 B：导入按包内环境各回各家——旧包（只有
// 单环境字段，profile=web）在 Desktop 启动方式下也恢复进 profiles/web，当前环境不受牵连。
func TestPluginImportTargetsFollowBundleProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	writeTestProfile(t, home, "web", map[string]string{"keep-web": "^1.0.0"}, nil)
	stubDesktopPref(t, launchTargetDesktop)

	manifest := `{"format":"dsh-systray-export","version":1,"items":[{"kind":"plugins","label":"已安装的插件","zip":"plugins.zip"}],` +
		`"plugins":{"profile":"web","dependencies":{"pkg-a":"^1.0.0"},"bundles":["pkg-a"]}}`
	zipPath := filepath.Join(home, "export.zip")
	if err := zipCreate(zipPath, map[string]string{"manifest.json": writeTemp(t, manifest)}, nil); err != nil {
		t.Fatal(err)
	}

	want := []string{filepath.Join(home, "profiles", "web")}
	if got := restoredPluginProfileDirs(zipPath); !reflect.DeepEqual(got, want) {
		t.Fatalf("快照/回退目录应为包内环境（web）：got %v want %v", got, want)
	}
	if err := registerRestoredPlugins(zipPath); err != nil {
		t.Fatal(err)
	}
	webRoot := map[string]any{}
	data, err := os.ReadFile(filepath.Join(home, "profiles", "web", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &webRoot); err != nil {
		t.Fatal(err)
	}
	deps, _ := webRoot["dependencies"].(map[string]any)
	if _, ok := deps["pkg-a"]; !ok {
		t.Fatalf("Web 环境应写入导入的插件依赖：%s", data)
	}
	if _, err := os.Stat(filepath.Join(home, "profiles", "desktop")); !os.IsNotExist(err) {
		t.Fatalf("当前环境（desktop）不该被这次导入创建/改动：err=%v", err)
	}
}

// TestPluginRestoreAllProfiles 方案 B 的落点：两套环境的包各自解到自己的 profiles/<name>；
// 旧布局前缀（profiles/node_modules/）在命名 profile 机器上改写为当前环境的落点。
func TestPluginRestoreAllProfiles(t *testing.T) {
	stubWebEnv(t)
	master := buildPluginExport(t) // 源机：Web 环境导出（前缀 profiles/web/node_modules/）

	inner := mustInner(t, master, exportZipPlugins)
	if got := zipPluginPrefixes(inner); !reflect.DeepEqual(got, []string{"profiles/web/node_modules/"}) {
		t.Fatalf("包内前缀应来自源机：%v", got)
	}

	homeB := t.TempDir()
	t.Setenv("DSH_HOME", homeB)
	stubDesktopPref(t, launchTargetDesktop) // 目标机当前启动方式是 Desktop，但包内是 web 环境
	if rm := pluginZipRemap(inner); rm != nil {
		t.Fatal("包内环境已有同名落点，不应改写条目路径（走 7z 快速解压）")
	}
	if _, err := restoreItem("plugins", inner, "", true, nil); err != nil {
		t.Fatalf("restore plugins: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homeB, "profiles", "web", "node_modules", "pkg-a", "index.js")); err != nil {
		t.Fatalf("插件应各回各家（profiles/web）：%v", err)
	}
	if n, _ := countRestoreConflicts("plugins", inner); n != 1 {
		t.Fatalf("冲突应按包内环境的落点统计（1 项）：got %d", n)
	}

	// 旧布局前缀 → 命名 profile 落点：需要改写条目路径
	legacy := filepath.Join(homeB, "legacy.zip")
	src := filepath.Join(homeB, "legacy-src", "pkg-old", "index.js")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := zipCreate(legacy, map[string]string{"profiles/node_modules/pkg-old": filepath.Dir(src)}, nil); err != nil {
		t.Fatal(err)
	}
	rm := pluginZipRemap(legacy)
	if rm == nil {
		t.Fatal("旧布局前缀在命名 profile 机器上必须改写")
	}
	if got := rm("profiles/node_modules/pkg-old/index.js"); !strings.HasPrefix(got, "profiles/desktop/node_modules/pkg-old/") {
		t.Fatalf("旧布局前缀应改写为当前环境落点：%q", got)
	}
}

// TestExportImportRoundTripAllProfiles 方案 B 端到端：两套环境的导出包，在全新数据目录里
// 一次恢复——文件与清单各回各家，互不混装（目标机当前启动方式与源机不同也不影响）。
func TestExportImportRoundTripAllProfiles(t *testing.T) {
	stubWebEnv(t)
	homeA := t.TempDir()
	t.Setenv("DSH_HOME", homeA)
	writeTestProfile(t, homeA, "web", map[string]string{"pkg-web": "^1.0.0"},
		map[string]string{"pkg-web": "1.0.0"})
	writeTestProfile(t, homeA, "desktop", map[string]string{"pkg-desk": "^2.0.0"},
		map[string]string{"pkg-desk": "2.0.0"})
	dest := filepath.Join(homeA, "exports")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	master, err := buildExportZip(false, true, false, nil, dest, nil, "")
	if err != nil {
		t.Fatalf("buildExportZip: %v", err)
	}

	homeB := t.TempDir()
	t.Setenv("DSH_HOME", homeB)
	stubDesktopPref(t, launchTargetDesktop) // 目标机当前启动方式：Desktop UI（源机是 Web）

	wantDirs := []string{
		filepath.Join(homeB, "profiles", "desktop"),
		filepath.Join(homeB, "profiles", "web"),
	}
	if got := restoredPluginProfileDirs(master); !reflect.DeepEqual(got, wantDirs) {
		t.Fatalf("恢复目标应为两套环境 %v：got %v", wantDirs, got)
	}
	if _, err := restoreItem("plugins", mustInner(t, master, exportZipPlugins), "", true, nil); err != nil {
		t.Fatalf("restore plugins: %v", err)
	}
	if err := registerRestoredPlugins(master); err != nil {
		t.Fatalf("register plugins: %v", err)
	}

	for _, c := range []struct{ profile, want, notWant string }{
		{"web", "pkg-web", "pkg-desk"},
		{"desktop", "pkg-desk", "pkg-web"},
	} {
		pkgFile := filepath.Join(homeB, "profiles", c.profile, "node_modules", c.want, "package.json")
		if _, err := os.Stat(pkgFile); err != nil {
			t.Fatalf("%s 环境的插件文件应恢复到位：%v", c.profile, err)
		}
		data, err := os.ReadFile(filepath.Join(homeB, "profiles", c.profile, "package.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), c.want) {
			t.Fatalf("%s 环境清单应写入自己的依赖 %s：%s", c.profile, c.want, data)
		}
		if strings.Contains(string(data), c.notWant) {
			t.Fatalf("%s 环境不该拿到另一环境的依赖 %s：%s", c.profile, c.notWant, data)
		}
	}
}

// TestAccountSyncCoverAllProfiles 账号同步不按启动方式过滤（用户约定：只有关于页插件清单过滤，
// 同步按原有逻辑执行、两套环境一起参与）——重置清空全部 profile 后仍能把它们一起同步回来。
func TestAccountSyncCoverAllProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	writeTestProfile(t, home, "web", map[string]string{"pkg-web": "^1.0.0"},
		map[string]string{"pkg-web": "1.0.0"})
	writeTestProfile(t, home, "desktop", map[string]string{"pkg-desk": "^2.0.0"},
		map[string]string{"pkg-desk": "2.0.0"})

	for _, mode := range []string{launchTargetWeb, launchTargetDesktop} {
		t.Run(mode, func(t *testing.T) {
			stubDesktopPref(t, mode)
			for _, c := range []struct{ profile, name string }{
				{"web", "pkg-web"}, {"desktop", "pkg-desk"},
			} {
				if _, ok := accountLocalPluginValue(c.profile, c.name); !ok {
					t.Fatalf("%s 启动方式下 %s 环境的 %s 也应参与同步", mode, c.profile, c.name)
				}
			}
			snap := accountLocalPluginsSnapshot()
			for _, key := range []string{
				accountPluginKeyFor("web", "pkg-web"),
				accountPluginKeyFor("desktop", "pkg-desk"),
			} {
				if _, ok := snap[key]; !ok {
					t.Fatalf("本机插件快照应覆盖两套环境，缺 %s：%+v", key, snap)
				}
			}
		})
	}
}

// TestPendingPluginChangeKeepsEnv 待应用变更记住登记时的环境：跨托盘重启（甚至期间切换了启动
// 方式）后仍只作用于登记环境，执行期刷新也不会扩散到另一环境；旧配置无环境字段时按当前环境。
func TestPendingPluginChangeKeepsEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	writeTestProfile(t, home, "web", map[string]string{"pkg-a": "^1.0.0"}, map[string]string{"pkg-a": "1.0.0"})
	writeTestProfile(t, home, "desktop", map[string]string{"pkg-a": "^1.0.0"}, map[string]string{"pkg-a": "1.0.0"})
	webDir := filepath.Join(home, "profiles", "web")
	deskDir := filepath.Join(home, "profiles", "desktop")

	oldPending := pluginPending
	t.Cleanup(func() { pluginPending = oldPending })

	stubWebEnv(t)
	row, ok := findActivePluginRowByID("pkg-a")
	if !ok {
		t.Fatal("Web 环境应能解析到 pkg-a")
	}
	task, why := pluginOpPrepare(row, "update")
	if why != "" {
		t.Fatalf("pluginOpPrepare: %s", why)
	}
	task.profile = activePluginProfile()
	pluginPending = []*pluginOpTask{task}

	ops := pluginPendingOps()
	if len(ops) != 1 || ops[0].Profile != "web" {
		t.Fatalf("待应用变更应记录登记环境 web：%+v", ops)
	}

	// 切到 Desktop 启动方式后重启托盘：按持久化条目重新载入，仍作用于登记环境 web
	stubDesktopPref(t, launchTargetDesktop)
	pluginPending = nil
	if dropped := loadPendingPluginOps(ops); dropped != 0 {
		t.Fatalf("不应丢弃条目：dropped=%d", dropped)
	}
	if len(pluginPending) != 1 {
		t.Fatalf("应载入 1 项：%+v", pluginPending)
	}
	got := pluginPending[0]
	if got.profile != "web" {
		t.Fatalf("载入后应保留登记环境：%q", got.profile)
	}
	if len(got.locs) != 1 || !sameProfileDir(got.locs[0], webDir) {
		t.Fatalf("作用目录必须是登记环境：%v", got.locs)
	}
	if !syncPluginTaskRow(got) {
		t.Fatal("执行期刷新应成功（pkg-a 仍在登记环境里）")
	}
	if len(got.locs) != 1 || !sameProfileDir(got.locs[0], webDir) {
		t.Fatalf("执行期刷新不得扩散到 Desktop 环境：%v", got.locs)
	}

	// 旧配置（无环境字段）：按当前启动方式的环境处理，不因缺少字段被丢弃
	pluginPending = nil
	if dropped := loadPendingPluginOps([]pendingPluginOp{{ID: "pkg-a", Op: "update"}}); dropped != 0 {
		t.Fatalf("旧配置条目不应被丢弃：dropped=%d", dropped)
	}
	if len(pluginPending) != 1 || pluginPending[0].profile != launchTargetDesktop {
		t.Fatalf("旧配置应按当前环境（desktop）处理：%+v", pluginPending)
	}
	if len(pluginPending[0].locs) != 1 || !sameProfileDir(pluginPending[0].locs[0], deskDir) {
		t.Fatalf("旧配置的作用目录应为当前环境：%v", pluginPending[0].locs)
	}
}
