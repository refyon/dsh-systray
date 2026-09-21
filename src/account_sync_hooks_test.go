package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// ---------- 埋点过滤规则 ----------

func TestIsOnlinePluginSource(t *testing.T) {
	cases := map[string]bool{
		"npm":     true,
		"github":  true,
		"tarball": true,
		"unknown": true, // 非本地来源（无法识别形态的 git 源等）仍需同步
		"file":    false,
		"local":   false,
		"":        false,
		" FILE ":  false, // 大小写/空白归一
	}
	for src, want := range cases {
		if got := isOnlinePluginSource(src); got != want {
			t.Errorf("isOnlinePluginSource(%q)=%v want %v", src, got, want)
		}
	}
}

// pendingByKey 读取队列中某个 key 的值（测试断言用）。
func pendingByKey(t *testing.T, key string) (accountPendingOp, bool) {
	t.Helper()
	accountMu.Lock()
	defer accountMu.Unlock()
	for _, op := range accountCur.PendingOps {
		if op.Key == key {
			return op, true
		}
	}
	return accountPendingOp{}, false
}

// ---------- 插件批处理结果埋点 ----------

func TestReportPluginBatchChangesFilters(t *testing.T) {
	setupAccountTest(t)
	setAccountAPIBase("http://127.0.0.1:1") // 上报触发点指向死地址：只验证登记，不触网
	setAccountState(loggedInState())

	tasks := []*pluginOpTask{
		{op: "update", name: "pkg-a", ok: true, row: PluginRow{Name: "pkg-a", Spec: "^1.0.0", Source: "npm"}, newVer: "1.0.2"},
		{op: "remove", name: "pkg-b", ok: true, row: PluginRow{Name: "pkg-b", Spec: "github:o/r", Source: "github"}},
		{op: "update", name: "pkg-c", ok: false, row: PluginRow{Name: "pkg-c", Source: "npm"}},                        // 失败项不上报
		{op: "update", name: "local-1", ok: true, row: PluginRow{Name: "local-1", Spec: "file:../x", Source: "file"}}, // 本地插件不上报
		{op: "enable", name: "pkg-d", ok: true, row: PluginRow{Name: "pkg-d", Source: "npm"}},                         // 启用不在同步范围
		{op: "remove", name: "ghost-1", ok: true, recordOnly: true, row: PluginRow{Name: "ghost-1", Source: "npm"}},   // 仅改本地记录
		nil, // 防御：nil 任务
	}
	reportPluginBatchChanges(tasks)

	if n := accountPendingCount(); n != 2 {
		t.Fatalf("应只登记 2 条（在线且成功的 update/remove），实际 %d：%+v", n, accountCur.PendingOps)
	}

	upd, ok := pendingByKey(t, accountPluginKey("pkg-a"))
	if !ok {
		t.Fatal("缺少 pkg-a 的更新记录")
	}
	var uv pluginOpValue
	if err := json.Unmarshal(upd.Value, &uv); err != nil {
		t.Fatalf("value 不是合法 JSON: %v (%s)", err, upd.Value)
	}
	if uv.Action != "update" || uv.Spec != "^1.0.0" || uv.Source != "npm" || uv.Version != "1.0.2" {
		t.Fatalf("更新记录字段错误: %+v", uv)
	}

	rem, ok := pendingByKey(t, accountPluginKey("pkg-b"))
	if !ok {
		t.Fatal("缺少 pkg-b 的删除记录")
	}
	var rv pluginOpValue
	if err := json.Unmarshal(rem.Value, &rv); err != nil {
		t.Fatalf("value 不是合法 JSON: %v", err)
	}
	if rv.Action != "remove" || rv.Version != "" {
		t.Fatalf("删除记录应为墓碑（action=remove，无版本）: %+v", rv)
	}
}

func TestReportPluginBatchChangesSkippedWhenLoggedOut(t *testing.T) {
	setupAccountTest(t)
	setAccountState(accountState{}) // 未登录

	reportPluginBatchChanges([]*pluginOpTask{
		{op: "update", name: "pkg-a", ok: true, row: PluginRow{Name: "pkg-a", Source: "npm"}, newVer: "1.0.2"},
	})
	if n := accountPendingCount(); n != 0 {
		t.Fatalf("未登录不应产生本地残队，实际 %d", n)
	}
}

func TestReportPluginUpdateFallsBackToTargetVersion(t *testing.T) {
	setupAccountTest(t)
	setAccountAPIBase("http://127.0.0.1:1") // 上报触发点指向死地址：只验证登记，不触网
	setAccountState(loggedInState())

	// newVer 为空（如 pnpm 未回填实际版本）时用检查阶段的目标版本
	reportPluginBatchChanges([]*pluginOpTask{
		{op: "update", name: "pkg-a", ok: true, row: PluginRow{Name: "pkg-a", Spec: "^1.0.0", Source: "npm"}, target: "1.0.3"},
	})
	op, ok := pendingByKey(t, accountPluginKey("pkg-a"))
	if !ok {
		t.Fatal("缺少更新记录")
	}
	var v pluginOpValue
	_ = json.Unmarshal(op.Value, &v)
	if v.Version != "1.0.3" {
		t.Fatalf("版本应回退到 target，实际 %q", v.Version)
	}
}

// ---------- Harness 版本埋点 ----------

func TestReportHarnessVersionIfChanged(t *testing.T) {
	setupAccountTest(t)
	setAccountAPIBase("http://127.0.0.1:1") // 上报触发点指向死地址：只验证登记，不触网
	setAccountState(loggedInState())

	oldDir := harnessDir
	harnessDir = t.TempDir()
	t.Cleanup(func() { harnessDir = oldDir })

	writeHarnessVersion := func(v string) {
		dir := filepath.Join(harnessDir, "node_modules", "@deepseek-ai", "dsh")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"version":"`+v+`"}`), 0o644); err != nil {
			t.Fatalf("写 package.json 失败: %v", err)
		}
	}

	// 版本未变：不上报
	writeHarnessVersion("1.2.3")
	reportHarnessVersionIfChanged("1.2.3")
	if n := accountPendingCount(); n != 0 {
		t.Fatalf("版本未变不应上报，实际 %d 条", n)
	}

	// 版本变化（带 v 前缀也视为相同语义）：上报新版本
	writeHarnessVersion("v1.3.0")
	reportHarnessVersionIfChanged("1.2.3")
	op, ok := pendingByKey(t, opKeyHarnessVersion)
	if !ok {
		t.Fatal("版本变化应上报")
	}
	if string(op.Value) != `"1.3.0"` {
		t.Fatalf("版本值应为去前缀的 JSON 字符串，实际 %s", op.Value)
	}

	// 第二次调用（同版本）不再新增
	reportHarnessVersionIfChanged("1.3.0")
	if n := accountPendingCount(); n != 1 {
		t.Fatalf("同版本重复调用不应新增，实际 %d 条", n)
	}
}

func TestReportHarnessVersionSkipsEmptyAndLoggedOut(t *testing.T) {
	setupAccountTest(t)
	setAccountAPIBase("http://127.0.0.1:1") // 上报触发点指向死地址：只验证登记，不触网

	oldDir := harnessDir
	harnessDir = t.TempDir()
	t.Cleanup(func() { harnessDir = oldDir })

	// 读不到版本：不上报（即便已登录）
	setAccountState(loggedInState())
	reportHarnessVersionIfChanged("1.0.0")
	if n := accountPendingCount(); n != 0 {
		t.Fatalf("读不到版本不应上报，实际 %d 条", n)
	}

	// 未登录：有版本也不登记
	dir := filepath.Join(harnessDir, "node_modules", "@deepseek-ai", "dsh")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"version":"2.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	setAccountState(accountState{})
	reportHarnessVersionIfChanged("1.0.0")
	if n := accountPendingCount(); n != 0 {
		t.Fatalf("未登录不应上报，实际 %d 条", n)
	}
}

// ---------- 设置项埋点 ----------

func TestReportSettingHooks(t *testing.T) {
	setupAccountTest(t)
	setAccountAPIBase("http://127.0.0.1:1") // 上报触发点指向死地址：只验证登记，不触网

	// 未登录：空操作
	reportSettingPrerelease(true)
	if n := accountPendingCount(); n != 0 {
		t.Fatalf("未登录不应登记，实际 %d 条", n)
	}

	setAccountState(loggedInState())
	reportSettingPrerelease(true)
	op, ok := pendingByKey(t, opKeyHarnessPrerelease)
	if !ok || string(op.Value) != "true" {
		t.Fatalf("预发布通道应上报 true，实际 %v %s", ok, op.Value)
	}

	// 开机自启动：值取后端实际注册状态（测试环境为未注册 → false，且不依赖调用方参数）
	reportSettingAutostart()
	op, ok = pendingByKey(t, opKeyAutostart)
	if !ok {
		t.Fatal("开机自启动应上报")
	}
	var on bool
	if err := json.Unmarshal(op.Value, &on); err != nil {
		t.Fatalf("value 应为布尔: %s", op.Value)
	}
	if on != isAutostartEnabled() {
		t.Fatalf("上报值应与后端实际状态一致：got %v want %v", on, isAutostartEnabled())
	}

	// 两次改动同一 key → 队列中仍只有一条（合并为最新值）
	reportSettingPrerelease(false)
	if n := accountPendingCount(); n != 2 {
		t.Fatalf("同 key 应合并，队列应仍为 2 条（prerelease + autostart），实际 %d", n)
	}
	op, _ = pendingByKey(t, opKeyHarnessPrerelease)
	if string(op.Value) != "false" {
		t.Fatalf("同 key 应保留最新值 false，实际 %s", op.Value)
	}
}

// ---------- Harness 版本对账 ----------

// useTempHarnessDir 把 harnessDir 指向临时目录（installedHarnessVersion 的读取源），测试结束还原。
func useTempHarnessDir(t *testing.T) {
	t.Helper()
	old := harnessDir
	harnessDir = t.TempDir()
	t.Cleanup(func() { harnessDir = old })
}

// writeInstalledHarnessVersion 写入「已安装」的 harness 版本号。
func writeInstalledHarnessVersion(t *testing.T, v string) {
	t.Helper()
	dir := filepath.Join(harnessDir, "node_modules", "@deepseek-ai", "dsh")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"version":"`+v+`"}`), 0o644); err != nil {
		t.Fatalf("写 package.json 失败: %v", err)
	}
}

// serverHasHarnessVersion 假服务端：服务器上已有该 key 的记录。
func serverHasHarnessVersion(t *testing.T, value string) *accountClient {
	t.Helper()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ops":[{"seq":9,"opId":"h1","key":"setting:harness_version","value":"` + value + `","deviceId":"d","updatedAt":1}],"cursor":9,"hasMore":false}`))
	})
	return client
}

// TestReconcileHarnessVersionSkipsDowngrade 本机版本低于账号记录（删除 .dsh / harness 目录后
// bootstrap 装回默认版本的意外回退）时不上报：上报会以更大 seq 覆盖服务器上的「最后选用版本」，
// 使同步永远回不到原版本（2026-09-22 现场问题②）。
func TestReconcileHarnessVersionSkipsDowngrade(t *testing.T) {
	setupAccountTest(t)
	setAccountAPIBase("http://127.0.0.1:1") // 兜底：意外触发上报时指向死地址，只验证登记
	useTempHarnessDir(t)
	writeInstalledHarnessVersion(t, "0.1.2-rc.2")

	st := loggedInState()
	st.LastReportedHarnessVersion = "0.1.6-alpha.2"
	setAccountState(st)

	accountReconcileHarnessVersion(context.Background(), serverHasHarnessVersion(t, "0.1.6-alpha.2"))

	if n := accountPendingCount(); n != 0 {
		t.Fatalf("版本回退不应上报（会覆盖账号版本），实际登记 %d 条", n)
	}
	accountMu.Lock()
	got := accountCur.LastReportedHarnessVersion
	accountMu.Unlock()
	if got != "0.1.2-rc.2" {
		t.Fatalf("应记录已对账版本，实际 %q", got)
	}
}

// TestReconcileHarnessVersionReportsUpgrade 版本在 dsh-systray 之外被升高时补报（原有盲区覆盖保持）。
func TestReconcileHarnessVersionReportsUpgrade(t *testing.T) {
	setupAccountTest(t)
	setAccountAPIBase("http://127.0.0.1:1") // 上报入口指向死地址：只验证登记，不触网
	useTempHarnessDir(t)
	writeInstalledHarnessVersion(t, "0.1.6-alpha.2")

	st := loggedInState()
	st.LastReportedHarnessVersion = "0.1.2-rc.2"
	setAccountState(st)

	accountReconcileHarnessVersion(context.Background(), serverHasHarnessVersion(t, "0.1.2-rc.2"))
	waitAccountSync() // 等异步上报结束（死地址必失败，记录留在队列里）

	op, ok := pendingByKey(t, opKeyHarnessVersion)
	if !ok {
		t.Fatal("外部升高版本应补报")
	}
	if string(op.Value) != `"0.1.6-alpha.2"` {
		t.Fatalf("应上报本机版本，实际 %s", op.Value)
	}
}

// TestReconcileHarnessVersionReportsWhenServerLacksKey 服务器没有该 key 的记录时补报本机版本
// （首次基线时版本尚未可知的盲区，原有行为）。
func TestReconcileHarnessVersionReportsWhenServerLacksKey(t *testing.T) {
	setupAccountTest(t)
	setAccountAPIBase("http://127.0.0.1:1")
	useTempHarnessDir(t)
	writeInstalledHarnessVersion(t, "0.1.6-alpha.2")

	st := loggedInState() // LastReportedHarnessVersion 为空：尚未对账过
	setAccountState(st)

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ops":[],"cursor":0,"hasMore":false}`))
	})
	accountReconcileHarnessVersion(context.Background(), client)
	waitAccountSync()

	op, ok := pendingByKey(t, opKeyHarnessVersion)
	if !ok {
		t.Fatal("服务器缺少记录时应补报本机版本")
	}
	if string(op.Value) != `"0.1.6-alpha.2"` {
		t.Fatalf("应上报本机版本，实际 %s", op.Value)
	}
}
