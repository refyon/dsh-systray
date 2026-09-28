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

// TestProfilePluginsFollowsLaunchTarget 导出取插件清单同样按当前环境：Desktop 启动方式读
// profiles/desktop（源机另一环境的插件不会被导出），Web 启动方式读 profiles/web。
func TestProfilePluginsFollowsLaunchTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	writeTestProfile(t, home, "web", map[string]string{"pkg-web": "^1.0.0"}, nil)
	writeTestProfile(t, home, "desktop", map[string]string{"pkg-desk": "^2.0.0"}, nil)

	stubDesktopPref(t, launchTargetWeb)
	dir, deps, err := profilePlugins()
	if err != nil {
		t.Fatal(err)
	}
	if !sameProfileDir(dir, filepath.Join(home, "profiles", "web")) || len(deps) != 1 || deps[0] != "pkg-web" {
		t.Fatalf("Web 启动应取 profiles/web 的插件：dir=%q deps=%v", dir, deps)
	}

	stubDesktopPref(t, launchTargetDesktop)
	dir, deps, err = profilePlugins()
	if err != nil {
		t.Fatal(err)
	}
	if !sameProfileDir(dir, filepath.Join(home, "profiles", "desktop")) || len(deps) != 1 || deps[0] != "pkg-desk" {
		t.Fatalf("Desktop 启动应取 profiles/desktop 的插件：dir=%q deps=%v", dir, deps)
	}
}

// TestPluginImportTargetsActiveEnvOnly 插件导入只作用于当前启动方式的环境：包内记录的源
// profile（web）不决定落点，manifest 配置也只写进 profiles/desktop，web 环境原样不变。
func TestPluginImportTargetsActiveEnvOnly(t *testing.T) {
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

	want := []string{filepath.Join(home, "profiles", "desktop")}
	if got := restoredPluginProfileDirs(zipPath); !reflect.DeepEqual(got, want) {
		t.Fatalf("快照/回退目录应只有当前环境：got %v want %v", got, want)
	}
	if err := registerRestoredPlugins(zipPath); err != nil {
		t.Fatal(err)
	}
	deskRoot := map[string]any{}
	data, err := os.ReadFile(filepath.Join(home, "profiles", "desktop", "package.json"))
	if err != nil {
		t.Fatalf("Desktop 环境应被创建并写入清单：%v", err)
	}
	if err := json.Unmarshal(data, &deskRoot); err != nil {
		t.Fatal(err)
	}
	deps, _ := deskRoot["dependencies"].(map[string]any)
	if _, ok := deps["pkg-a"]; !ok {
		t.Fatalf("Desktop 环境应写入导入的插件依赖：%s", data)
	}
	webData, _ := os.ReadFile(filepath.Join(home, "profiles", "web", "package.json"))
	if strings.Contains(string(webData), "pkg-a") {
		t.Fatalf("Web 环境不应被导入改动：%s", webData)
	}
}

// TestPluginRestoreRewritesIntoActiveEnv 跨启动方式导入：包内前缀 profiles/web/node_modules/
// 被改写为当前环境的 profiles/desktop/node_modules/，插件文件落到当前环境，源环境不被写入；
// 冲突统计同样按当前环境落点。
func TestPluginRestoreRewritesIntoActiveEnv(t *testing.T) {
	stubWebEnv(t) // 源机：Web 启动方式（导出包前缀 profiles/web/node_modules/）
	master := buildPluginExport(t)

	homeB := t.TempDir()
	t.Setenv("DSH_HOME", homeB)
	inner := mustInner(t, master, exportZipPlugins)
	if prefix := innerZipContentPrefix("plugins", inner); prefix != "profiles/web/node_modules/" {
		t.Fatalf("包内前缀应来自源机：%q", prefix)
	}
	if rm := pluginZipRemap(inner); rm != nil {
		t.Fatal("同环境导入不应改写条目路径（走 7z 快速解压）")
	}

	stubDesktopPref(t, launchTargetDesktop) // 目标机：Desktop 启动方式
	if rm := pluginZipRemap(inner); rm == nil {
		t.Fatal("跨环境导入必须改写条目路径")
	}
	if n, err := countRestoreConflicts("plugins", inner); err != nil || n != 0 {
		t.Fatalf("新环境无冲突：n=%d err=%v", n, err)
	}
	if _, err := restoreItem("plugins", inner, "", true, nil); err != nil {
		t.Fatalf("restore plugins: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homeB, "profiles", "desktop", "node_modules", "pkg-a", "index.js")); err != nil {
		t.Fatalf("插件应落到当前环境（profiles/desktop）：%v", err)
	}
	if _, err := os.Stat(filepath.Join(homeB, "profiles", "web")); !os.IsNotExist(err) {
		t.Fatalf("源环境（profiles/web）不应被写入：err=%v", err)
	}
	if n, _ := countRestoreConflicts("plugins", inner); n != 1 {
		t.Fatalf("冲突应按当前环境统计（1 项）：got %d", n)
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
