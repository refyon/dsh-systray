package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMain 关掉安装时刻台账的默认落盘：单测会大量构造临时 profile 并触发「本机安装时刻」查询，
// 默认路径落在用户真实的 %APPDATA%\dsh-systray 下，会把测试数据（pkg-* 等）写进用户的配置目录
// （2026-09-28 实测：跑完全量测试后真实目录里多出几条测试记录）。需要验证落盘行为的用例用
// setPluginInstallTimesFileOverride 显式指定临时路径，不受此开关影响。
func TestMain(m *testing.M) {
	setPluginInstallTimesNoPersist(true)
	os.Exit(m.Run())
}

// setupPluginInstallTimes 把安装时刻台账落到临时文件并清空内存状态，隔绝真实 %APPDATA%。
func setupPluginInstallTimes(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), pluginInstallTimesFile)
	oldOverride := pluginInstallTimesFileOverrideValue()
	setPluginInstallTimesFileOverride(path)
	resetPluginInstallTimes()
	t.Cleanup(func() {
		setPluginInstallTimesFileOverride(oldOverride)
		resetPluginInstallTimes()
	})
	return path
}

// TestPluginInstallTimeNoteInstalledKeepsFirstTime 台账幂等：版本更新（再次「安装」同一个包）
// 不得刷新已有记录——否则每次更新都会把「本机装得更晚」推到现在，删除墓碑会被反复推翻。
func TestPluginInstallTimeNoteInstalledKeepsFirstTime(t *testing.T) {
	setupPluginInstallTimes(t)

	first := int64(1759000000)
	pluginInstallTimeNoteInstalled("web", "pkg-a", first)
	pluginInstallTimeNoteInstalled("web", "pkg-a", first+86400)

	if got := pluginInstallTimeLookup("web", "pkg-a"); got != first {
		t.Fatalf("台账应保留首次安装时刻 %d，实际 %d", first, got)
	}
}

// TestPluginInstallTimeForgetClearsRecord 卸载后清除记录（下次安装重新计时）。
func TestPluginInstallTimeForgetClearsRecord(t *testing.T) {
	path := setupPluginInstallTimes(t)

	pluginInstallTimeNoteInstalled("web", "pkg-a", 1759000000)
	pluginInstallTimeForget("web", "pkg-a")
	if got := pluginInstallTimeLookup("web", "pkg-a"); got != 0 {
		t.Fatalf("卸载后应无记录，实际 %d", got)
	}
	// 落盘同样为空（重启后不会把旧时刻读回来）
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("台账文件应已写入: %v", err)
	}
	var disk pluginInstallTimes
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatalf("台账文件应可解析: %v", err)
	}
	if _, exists := disk.Times[accountPluginKeyFor("web", "pkg-a")]; exists {
		t.Fatalf("落盘记录应已被清除: %v", disk.Times)
	}
}

// TestPluginInstallTimePersistsAcrossReload 台账跨进程持久：写盘后清空内存再读，记录仍在。
func TestPluginInstallTimePersistsAcrossReload(t *testing.T) {
	setupPluginInstallTimes(t)

	at := int64(1759000000)
	pluginInstallTimeNoteInstalled("desktop", "pkg-b", at)

	pluginInstallTimesMu.Lock()
	pluginInstallTimesCur, pluginInstallTimesLoaded = &pluginInstallTimes{Times: map[string]int64{}}, false
	pluginInstallTimesMu.Unlock()

	if got := pluginInstallTimeLookup("desktop", "pkg-b"); got != at {
		t.Fatalf("重新载入后应读回 %d，实际 %d", at, got)
	}
}

// TestPluginInstallTimeForPrefersLedgerOverFilesystem 有台账时一律用台账时刻——这正是修复点：
// 文件系统时间（包目录创建时间）会被 pnpm 重建 node_modules 刷新，不能作为安装时刻依据。
func TestPluginInstallTimeForPrefersLedgerOverFilesystem(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	writeTestProfile(t, home, "web",
		map[string]string{"pkg-a": "^1.0.0"},
		map[string]string{"pkg-a": "1.0.3"})
	setupPluginInstallTimes(t)

	ledger := time.Now().Unix() - 86400 // 台账：一天前装的
	pluginInstallTimeNoteInstalled("web", "pkg-a", ledger)

	if got := accountLocalPluginInstallTimeFor("web", "pkg-a"); got != ledger {
		t.Fatalf("应取台账时刻 %d，实际 %d（文件系统时间被误用）", ledger, got)
	}
}

// TestPluginInstallTimeForBootstrapsFilesystemOnce 台账没有该插件（功能引入前就装着、或在外部
// pnpm/npm 安装）时用文件系统时间兜底，并把这次的值固化——此后文件系统时间再变（pnpm 重建
// node_modules 会把包目录创建时间刷成当前时刻）也不影响判定。
func TestPluginInstallTimeForBootstrapsFilesystemOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	writeTestProfile(t, home, "web",
		map[string]string{"pkg-a": "^1.0.0"},
		map[string]string{"pkg-a": "1.0.3"})
	setupPluginInstallTimes(t)

	bootstrapped := accountLocalPluginInstallTimeFor("web", "pkg-a")
	if bootstrapped <= 0 {
		t.Fatalf("文件系统兜底应给出安装时刻，实际 %d", bootstrapped)
	}
	if got := pluginInstallTimeLookup("web", "pkg-a"); got != bootstrapped {
		t.Fatalf("兜底值应被固化进台账（期望 %d，实际 %d）", bootstrapped, got)
	}
	// 固化后再读：即使文件系统时间已是别的值（模拟 pnpm 重建把创建时间刷新到现在），仍返回固化值。
	if got := accountLocalPluginInstallTimeFor("web", "pkg-a"); got != bootstrapped {
		t.Fatalf("固化后应稳定返回 %d，实际 %d", bootstrapped, got)
	}
}

// TestRemovedPluginNotResurrectedByFilesystemTime 回归：服务端的删除墓碑落在「台账安装时刻」
// 之后、文件系统时间之前（pnpm 重建把创建时间刷新的典型形态）时，本机这份是墓碑之前装的旧包：
// 判定为**未满足**（删除生效，本机旧包被卸载），既不会静默当成「已满足」，也不会被对账当成
// 「本机装得更晚」补报 install 把删除推翻（2026-09-28 现场：dsh-purge 删不掉）。
func TestRemovedPluginNotResurrectedByFilesystemTime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	writeTestProfile(t, home, "web",
		map[string]string{"pkg-a": "github:owner/repo#v1.0.0"},
		map[string]string{"pkg-a": "1.0.0"})
	setupPluginInstallTimes(t)

	now := time.Now().Unix()
	pluginInstallTimeNoteInstalled("web", "pkg-a", now-3600) // 本机一小时前装的（台账）

	value := json.RawMessage(`{"action":"remove","spec":"github:owner/repo#v1.0.0","source":"github","version":""}`)
	tombstone := now - 1800 // 墓碑：半小时前（晚于台账安装时刻）

	if accountKeyTargetSatisfiedAt("plugin:web:pkg-a", value, tombstone) {
		t.Fatalf("墓碑晚于台账安装时刻：本机这份是删除前装的旧包，应判为未满足（按删除处理，而不是当成本机装得更晚）")
	}

	// 对照：本机在墓碑之后才装上（台账时刻晚于墓碑）→ 属于「删掉后又装回来」，墓碑已被盖过，
	// 判定为已满足并交给对账补报 install。
	pluginInstallTimeForget("web", "pkg-a")
	pluginInstallTimeNoteInstalled("web", "pkg-a", now)
	if !accountKeyTargetSatisfiedAt("plugin:web:pkg-a", value, tombstone) {
		t.Fatalf("台账安装时刻晚于墓碑：应判为已满足（本机装得更晚，墓碑已被盖过）")
	}
}
