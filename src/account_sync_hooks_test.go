package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
		{op: "install", name: "pkg-e", ok: true, row: PluginRow{Name: "pkg-e", Spec: "^2.0.0", Source: "npm"}, newVer: "2.0.0"},
		{op: "update", name: "pkg-c", ok: false, row: PluginRow{Name: "pkg-c", Source: "npm"}},                        // 失败项不上报
		{op: "update", name: "local-1", ok: true, row: PluginRow{Name: "local-1", Spec: "file:../x", Source: "file"}}, // 本地插件不上报
		{op: "enable", name: "pkg-d", ok: true, row: PluginRow{Name: "pkg-d", Source: "npm"}},                         // 启用不在同步范围
		{op: "remove", name: "ghost-1", ok: true, recordOnly: true, row: PluginRow{Name: "ghost-1", Source: "npm"}},   // 仅改本地记录
		nil, // 防御：nil 任务
	}
	reportPluginBatchChanges(tasks)

	if n := accountPendingCount(); n != 3 {
		t.Fatalf("应只登记 3 条（在线且成功的 install/update/remove），实际 %d：%+v", n, accountCur.PendingOps)
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

	// 托盘内安装也必须有上报通道（此前 install 被过滤掉，只剩对账兜底——对账一旦被待生效
	// 集合挡住就永远补不上去，见 2026-09-24 现场）
	inst, ok := pendingByKey(t, accountPluginKey("pkg-e"))
	if !ok {
		t.Fatal("缺少 pkg-e 的安装记录（托盘内安装应上报 install）")
	}
	var iv pluginOpValue
	if err := json.Unmarshal(inst.Value, &iv); err != nil {
		t.Fatalf("value 不是合法 JSON: %v (%s)", err, inst.Value)
	}
	if iv.Action != "install" || iv.Spec != "^2.0.0" || iv.Source != "npm" || iv.Version != "2.0.0" {
		t.Fatalf("安装记录字段错误: %+v", iv)
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

// ---------- 本地插件对账补报 ----------

// stubLocalPlugins 替换本机插件快照（不触碰真实 dshHome），测试结束还原。
func stubLocalPlugins(t *testing.T, plugins ...accountLocalPlugin) {
	t.Helper()
	old := accountLocalPluginsSnapshotFn
	m := map[string]accountLocalPlugin{}
	for _, p := range plugins {
		m[p.Name] = p
	}
	accountLocalPluginsSnapshotFn = func() map[string]accountLocalPlugin { return m }
	t.Cleanup(func() { accountLocalPluginsSnapshotFn = old })
}

// captureFakeOpsServer 在 fakeOpsServer 之外额外记录上报请求体（断言「补报真的送到了服务器」用）。
func captureFakeOpsServer(t *testing.T, ops []testOp) (*accountClient, *[]map[string]any) {
	t.Helper()
	var reports []map[string]any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ops/since":
			_, _ = w.Write([]byte(opsSinceBody(ops, 0)))
		case "/v1/ops/report":
			reports = append(reports, readJSONBody(t, r))
			_, _ = w.Write([]byte(`{"accepted":1,"duplicates":0,"cursor":100}`))
		default:
			t.Errorf("未预期路径: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return client, &reports
}

// opsServerWithUpdatedAt 假服务器：单条记录、可指定 updatedAt（时间戳判定用例要用真实秒数）。
func opsServerWithUpdatedAt(t *testing.T, key, value string, seq, updatedAt int64) *accountClient {
	t.Helper()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ops/since" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"ops":[{"seq":%d,"opId":"o-%d","key":%q,"value":%s,"deviceId":"other","updatedAt":%d}],"cursor":%d,"hasMore":false}`,
			seq, seq, key, value, updatedAt, seq)))
	})
	return client
}

// opsSinceBody 拼 /v1/ops/since 的响应体（测试记录默认 updatedAt=1：时间戳判定单独用上面的服务器）。
func opsSinceBody(ops []testOp, updatedAt int64) string {
	var out []string
	maxSeq := int64(0)
	for _, op := range ops {
		out = append(out, fmt.Sprintf(
			`{"seq":%d,"opId":"op-%d","key":%q,"value":%s,"deviceId":"other","updatedAt":%d}`,
			op.seq, op.seq, op.key, op.value, updatedAt))
		if op.seq > maxSeq {
			maxSeq = op.seq
		}
	}
	return fmt.Sprintf(`{"ops":[%s],"cursor":%d,"hasMore":false}`, strings.Join(out, ","), maxSeq)
}

// runPluginReconcile 跑一次插件对账并返回补报条数（上报入口指向死地址：只验证登记，不触网）。
func runPluginReconcile(t *testing.T, ops []testOp, local ...accountLocalPlugin) int {
	t.Helper()
	setupAccountTest(t)
	setAccountAPIBase("http://127.0.0.1:1")
	setAccountState(loggedInState())
	stubLocalPlugins(t, local...)
	return accountReconcileLocalPlugins(context.Background(), fakeOpsServer(t, ops, 0))
}

// pendingPluginValue 读取队列中某插件的目标值。
func pendingPluginValue(t *testing.T, name string) (pluginOpValue, bool) {
	t.Helper()
	op, ok := pendingByKey(t, accountPluginKey(name))
	if !ok {
		return pluginOpValue{}, false
	}
	var v pluginOpValue
	if err := json.Unmarshal(op.Value, &v); err != nil {
		t.Fatalf("value 不是合法 JSON: %v (%s)", err, op.Value)
	}
	return v, true
}

// TestReconcileLocalPluginsReportsMissingRecord 账号上完全没有该插件的记录时补报
// （托盘外 npm/pnpm 直接安装的主盲区：不补报，其它机器点多少次「立即同步」都是 0 条）。
func TestReconcileLocalPluginsReportsMissingRecord(t *testing.T) {
	lp := accountLocalPlugin{Name: "dsh-code-index", Spec: "^1.0.0", Source: "npm", Version: "1.0.3", InstalledAt: 2000}
	if n := runPluginReconcile(t, nil, lp); n != 1 {
		t.Fatalf("账号无记录应补报 1 条，实际 %d", n)
	}
	v, ok := pendingPluginValue(t, "dsh-code-index")
	if !ok {
		t.Fatal("缺少补报记录")
	}
	if v.Action != "install" || v.Spec != "^1.0.0" || v.Source != "npm" || v.Version != "1.0.3" {
		t.Fatalf("补报字段错误: %+v", v)
	}
}

// TestReconcileLocalPluginsSkipsExistingRecord 账号上已有同版本记录（含本机自己此前上报的）时不重复补报。
func TestReconcileLocalPluginsSkipsExistingRecord(t *testing.T) {
	ops := []testOp{{seq: 9, key: accountPluginKey("pkg-a"),
		value: `{"action":"update","spec":"^1.0.0","source":"npm","version":"1.0.3"}`}}
	lp := accountLocalPlugin{Name: "pkg-a", Spec: "^1.0.0", Source: "npm", Version: "1.0.3", InstalledAt: 2000}
	if n := runPluginReconcile(t, ops, lp); n != 0 {
		t.Fatalf("账号已有同版本记录不应补报，实际 %d", n)
	}
}

// TestReconcileLocalPluginsSkipsOldInstallAfterRemoteRemove 服务器最新记录是**删除**且删除时间
// 晚于本机安装时间：本机这个是删除前的旧安装，删除方胜，不把它装回来（墓碑不复活）。
func TestReconcileLocalPluginsSkipsOldInstallAfterRemoteRemove(t *testing.T) {
	setupAccountTest(t)
	setAccountAPIBase("http://127.0.0.1:1")
	setAccountState(loggedInState())
	stubLocalPlugins(t, accountLocalPlugin{Name: "pkg-a", Spec: "^1.0.0", Source: "npm", Version: "1.0.3", InstalledAt: 2000})
	client := opsServerWithUpdatedAt(t, accountPluginKey("pkg-a"), `{"action":"remove"}`, 20, 3000)

	if n := accountReconcileLocalPlugins(context.Background(), client); n != 0 {
		t.Fatalf("删除记录晚于本机安装时间：不应补报，实际 %d 条", n)
	}
}

// TestReconcileLocalPluginsReportsLaterInstall 删除记录之后本机又装回来：补报（本机装得更晚）。
func TestReconcileLocalPluginsReportsLaterInstall(t *testing.T) {
	setupAccountTest(t)
	setAccountAPIBase("http://127.0.0.1:1")
	setAccountState(loggedInState())
	stubLocalPlugins(t, accountLocalPlugin{Name: "pkg-a", Spec: "^1.0.0", Source: "npm", Version: "1.0.3", InstalledAt: 3000})
	client := opsServerWithUpdatedAt(t, accountPluginKey("pkg-a"), `{"action":"remove"}`, 20, 2000)

	if n := accountReconcileLocalPlugins(context.Background(), client); n != 1 {
		t.Fatalf("本机安装晚于删除记录应补报 1 条，实际 %d", n)
	}
	if v, _ := pendingPluginValue(t, "pkg-a"); v.Action != "install" {
		t.Fatalf("删后重装应报 action=install，实际 %+v", v)
	}
}

// tombstoneSyncServer 假服务器：单条删除墓碑记录（带真实 updatedAt），并接受上报。
func tombstoneSyncServer(t *testing.T, key string, value string, seq, updatedAt int64) *accountClient {
	t.Helper()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ops/since":
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"ops":[{"seq":%d,"opId":"o-%d","key":%q,"value":%s,"deviceId":"other","updatedAt":%d}],"cursor":%d,"hasMore":false}`,
				seq, seq, key, value, updatedAt, seq)))
		case "/v1/ops/report":
			_, _ = w.Write([]byte(`{"accepted":1,"duplicates":0,"cursor":100}`))
		default:
			t.Errorf("未预期路径: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return client
}

// TestSyncUnblocksPendingTombstoneAndReportsInstall 回归 2026-09-24 现场（dsh-cost-meter）：
// 一条**早于本机安装**的删除墓碑卡在待生效集合里时，对账会被「待生效即跳过」挡住，install
// 永远补报不上去（实测：插件补报恒为 0、待生效恒为 1 项、游标每轮回拨到 29）。
// 修复后该墓碑在拉取与漂移重判时都判为「已被本机更晚的安装盖过」：待生效集合清空，
// 对账随即补报 install。
func TestSyncUnblocksPendingTombstoneAndReportsInstall(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())
	setAccountAPIBase("http://127.0.0.1:1")

	const (
		installAt   = int64(3000) // 本机安装时间（晚于墓碑）
		tombstoneAt = int64(2000) // 服务器删除记录时间
		name        = "dsh-cost-meter"
	)
	key := accountPluginKey(name)
	remove := json.RawMessage(`{"action":"remove","spec":"^1.7.35","source":"npm","version":""}`)

	// 本机：插件已装（生产环境由 buildPluginRows 供数，这里用桩替代）
	stubLocalPlugins(t, accountLocalPlugin{Name: name, Spec: "^1.7.35", Source: "npm", Version: "1.7.35", InstalledAt: installAt})
	oldVal, oldTime := accountLocalPluginValueFn, accountLocalPluginInstallTimeForFn
	t.Cleanup(func() { accountLocalPluginValueFn, accountLocalPluginInstallTimeForFn = oldVal, oldTime })
	accountLocalPluginValueFn = func(profile, n string) (pluginOpValue, bool) {
		if n != name {
			return pluginOpValue{}, false
		}
		return pluginOpValue{Action: "update", Spec: "^1.7.35", Source: "npm", Version: "1.7.35"}, true
	}
	accountLocalPluginInstallTimeForFn = func(profile, n string) int64 { return installAt }

	// 现场状态：墓碑曾被应用（appliedVals），并卡在待生效集合里
	accountMu.Lock()
	accountCur.Cursor = 29
	accountCur.AppliedSeqs = map[string]int64{key: 30}
	accountCur.AppliedVals = map[string]appliedRecord{key: {Seq: 30, Value: remove, UpdatedAt: tombstoneAt}}
	accountCur.PendingRemote = []accountPendingOp{{OpID: "pending-" + key + "-30", Key: key, Value: remove, CreatedAt: 1, Seq: 30, UpdatedAt: tombstoneAt}}
	accountCur.PendingApply = true
	accountMu.Unlock()

	client := tombstoneSyncServer(t, key, string(remove), 30, tombstoneAt)
	if _, err := accountSyncNow(context.Background(), client); err != nil {
		t.Fatalf("同步失败: %v", err)
	}

	if keys := accountPendingKeys(); len(keys) != 0 {
		t.Fatalf("被更晚安装盖过的墓碑不应留在待生效集合，实际 %v", keys)
	}
	v, ok := pendingPluginValue(t, name)
	if !ok || v.Action != "install" {
		t.Fatalf("应对账补报 install，实际 %+v（存在=%v）", v, ok)
	}
	if v.Version != "1.7.35" || v.Source != "npm" {
		t.Fatalf("补报字段错误: %+v", v)
	}
}

// TestSyncKeepsPendingTombstoneWhenLocalInstallOlder 反向保护：本机安装**早于**墓碑（真正的
// 「别的设备删掉了」）时，墓碑仍留在待生效集合、对账也不补报——修复不能把删除意图放跑。
func TestSyncKeepsPendingTombstoneWhenLocalInstallOlder(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())
	setAccountAPIBase("http://127.0.0.1:1")

	const (
		installAt   = int64(1000) // 本机安装时间（早于墓碑）
		tombstoneAt = int64(2000)
		name        = "dsh-cost-meter"
	)
	key := accountPluginKey(name)
	remove := json.RawMessage(`{"action":"remove","spec":"^1.7.35","source":"npm","version":""}`)

	stubLocalPlugins(t, accountLocalPlugin{Name: name, Spec: "^1.7.35", Source: "npm", Version: "1.7.35", InstalledAt: installAt})
	oldVal, oldTime := accountLocalPluginValueFn, accountLocalPluginInstallTimeForFn
	t.Cleanup(func() { accountLocalPluginValueFn, accountLocalPluginInstallTimeForFn = oldVal, oldTime })
	accountLocalPluginValueFn = func(profile, n string) (pluginOpValue, bool) {
		if n != name {
			return pluginOpValue{}, false
		}
		return pluginOpValue{Action: "update", Spec: "^1.7.35", Source: "npm", Version: "1.7.35"}, true
	}
	accountLocalPluginInstallTimeForFn = func(profile, n string) int64 { return installAt }

	accountMu.Lock()
	accountCur.Cursor = 29
	accountCur.AppliedSeqs = map[string]int64{key: 30}
	accountCur.AppliedVals = map[string]appliedRecord{key: {Seq: 30, Value: remove, UpdatedAt: tombstoneAt}}
	accountMu.Unlock()

	client := tombstoneSyncServer(t, key, string(remove), 30, tombstoneAt)
	if _, err := accountSyncNow(context.Background(), client); err != nil {
		t.Fatalf("同步失败: %v", err)
	}

	keys := accountPendingKeys()
	if len(keys) != 1 || keys[0] != key {
		t.Fatalf("本机安装早于墓碑时应保留待生效（等用户点重启生效删除），实际 %v", keys)
	}
	if _, ok := pendingPluginValue(t, name); ok {
		t.Fatal("不应把本机这个删除前的旧安装补报为 install")
	}
}

// TestReconcileLocalPluginsReportsUpgrade 托盘外把插件升到更高版本：补报（与 Harness 版本同口径）。
func TestReconcileLocalPluginsReportsUpgrade(t *testing.T) {
	ops := []testOp{{seq: 9, key: accountPluginKey("pkg-a"),
		value: `{"action":"update","spec":"^1.0.0","source":"npm","version":"1.0.3"}`}}
	lp := accountLocalPlugin{Name: "pkg-a", Spec: "^1.0.0", Source: "npm", Version: "1.0.5", InstalledAt: 2000}
	if n := runPluginReconcile(t, ops, lp); n != 1 {
		t.Fatalf("本机版本更高应补报 1 条，实际 %d", n)
	}
	if v, _ := pendingPluginValue(t, "pkg-a"); v.Action != "update" || v.Version != "1.0.5" {
		t.Fatalf("升级补报字段错误: %+v", v)
	}
}

// TestReconcileLocalPluginsSkipsDowngrade 本机版本低于账号记录（意外回退）时不覆盖账号记录。
func TestReconcileLocalPluginsSkipsDowngrade(t *testing.T) {
	ops := []testOp{{seq: 9, key: accountPluginKey("pkg-a"),
		value: `{"action":"update","spec":"^1.0.0","source":"npm","version":"1.0.5"}`}}
	lp := accountLocalPlugin{Name: "pkg-a", Spec: "^1.0.0", Source: "npm", Version: "1.0.3", InstalledAt: 2000}
	if n := runPluginReconcile(t, ops, lp); n != 0 {
		t.Fatalf("版本回退不应补报（会覆盖账号记录），实际 %d 条", n)
	}
}

// TestReconcileLocalPluginsNotLoggedIn 未登录时不动（连服务器都不该查）。
func TestReconcileLocalPluginsNotLoggedIn(t *testing.T) {
	setupAccountTest(t)
	setAccountAPIBase("http://127.0.0.1:1")
	setAccountState(accountState{})
	stubLocalPlugins(t, accountLocalPlugin{Name: "pkg-a", Spec: "^1.0.0", Source: "npm", Version: "1.0.3", InstalledAt: 2000})
	if n := accountReconcileLocalPlugins(context.Background(), fakeOpsServer(t, nil, 0)); n != 0 {
		t.Fatalf("未登录不应补报，实际 %d", n)
	}
	if n := accountPendingCount(); n != 0 {
		t.Fatalf("未登录不应产生本地残队，实际 %d", n)
	}
}

// TestReconcileLocalPluginsPendingNotDuplicated 已有待上报记录（本次同步前半程刚补报过）时不重复登记。
func TestReconcileLocalPluginsPendingNotDuplicated(t *testing.T) {
	// 同步流程内的形态：上报阶段已登记 → 对账阶段看到队列里已有同 key 记录，不再登记（但算作本次补报）
	setupAccountTest(t)
	setAccountAPIBase("http://127.0.0.1:1")
	setAccountState(loggedInState())
	stubLocalPlugins(t, accountLocalPlugin{Name: "pkg-a", Spec: "^1.0.0", Source: "npm", Version: "1.0.3", InstalledAt: 2000})
	if err := accountEnqueueOp(accountPluginKey("pkg-a"),
		pluginOpValue{Action: "install", Spec: "^1.0.0", Source: "npm", Version: "1.0.3"}); err != nil {
		t.Fatalf("预置待上报记录失败: %v", err)
	}

	before := accountPendingCount()
	if n := accountReconcileLocalPlugins(context.Background(), fakeOpsServer(t, nil, 0)); n != 1 {
		t.Fatalf("队列里已有该插件记录：应算作本次补报 1 条，实际 %d", n)
	}
	if after := accountPendingCount(); after != before {
		t.Fatalf("不应重复登记：before=%d after=%d", before, after)
	}
}

// TestReconcileLocalPluginsUploadsToServer 补报的记录经一次同步送到服务器（请求体含该插件记录）。
func TestReconcileLocalPluginsUploadsToServer(t *testing.T) {
	setupAccountTest(t)
	st := loggedInState()
	st.BaselineDone, st.Cursor = true, 5 // 已完成首次同步：跳过基线分支，走「拉取 + 对账」
	setAccountState(st)
	stubLocalPlugins(t, accountLocalPlugin{Name: "dsh-code-index", Spec: "^1.0.0", Source: "npm", Version: "1.0.3", InstalledAt: 2000})
	client, reports := captureFakeOpsServer(t, nil)
	setAccountAPIBase(client.base) // 异步上报走同一个假服务器（生产用默认域名）

	res, err := accountSyncNow(context.Background(), client)
	waitAccountSync()
	if err != nil {
		t.Fatalf("同步检查失败: %v", err)
	}
	if res.PluginsReported != 1 {
		t.Fatalf("应补报 1 个插件，实际 %d（res=%+v）", res.PluginsReported, res)
	}
	if len(*reports) == 0 {
		t.Fatal("没有向服务器上报任何记录")
	}
	ops, _ := (*reports)[0]["ops"].([]any)
	if len(ops) != 1 {
		t.Fatalf("应上报 1 条记录，实际 %d（%v）", len(ops), (*reports)[0])
	}
	rec, _ := ops[0].(map[string]any)
	if rec["key"] != "plugin:web:dsh-code-index" {
		t.Fatalf("上报 key 错误: %v", rec["key"])
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
