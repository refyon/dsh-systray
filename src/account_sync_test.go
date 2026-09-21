package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
)

// ---------- 合并引擎（纯函数） ----------

func TestMergeOpsLWWAndDedup(t *testing.T) {
	remote := []accountOpRecord{
		{OpID: "r1", Key: opKeyAutostart, Value: json.RawMessage(`true`), Seq: 5},
		{OpID: "r2", Key: opKeyAutostart, Value: json.RawMessage(`false`), Seq: 9}, // 同 key 更大的 seq 胜
		{OpID: "r2", Key: opKeyAutostart, Value: json.RawMessage(`false`), Seq: 9}, // 重复 opId：只算一次
		{OpID: "r3", Key: opKeyHarnessVersion, Value: json.RawMessage(`"1.0.0"`), Seq: 7},
		{OpID: "r4", Key: "unknown:key", Value: json.RawMessage(`1`), Seq: 8}, // 不在同步范围
	}
	got := mergeOps(nil, remote)

	if len(got) != 2 {
		t.Fatalf("应只保留同步范围内的 2 个 key，实际 %d：%+v", len(got), got)
	}
	if string(got[opKeyAutostart].Value) != "false" || got[opKeyAutostart].Seq != 9 {
		t.Fatalf("同 key 应取 seq 最大者：%+v", got[opKeyAutostart])
	}
	if string(got[opKeyHarnessVersion].Value) != `"1.0.0"` {
		t.Fatalf("harness 版本合并错误：%+v", got[opKeyHarnessVersion])
	}
}

func TestMergeOpsLocalPendingWins(t *testing.T) {
	remote := []accountOpRecord{
		{OpID: "r1", Key: opKeyHarnessVersion, Value: json.RawMessage(`"1.0.0"`), Seq: 7},
		{OpID: "r2", Key: opKeyAutostart, Value: json.RawMessage(`true`), Seq: 8},
	}
	local := []accountPendingOp{
		// 本地刚改、还没上报成功：不能被服务器记录覆盖
		{OpID: "l1", Key: opKeyHarnessVersion, Value: json.RawMessage(`"2.0.0"`)},
	}
	got := mergeOps(local, remote)

	if !got[opKeyHarnessVersion].FromLocal || string(got[opKeyHarnessVersion].Value) != `"2.0.0"` {
		t.Fatalf("本地未上报的改动应获胜：%+v", got[opKeyHarnessVersion])
	}
	if got[opKeyAutostart].FromLocal || string(got[opKeyAutostart].Value) != "true" {
		t.Fatalf("无本地改动时取服务器值：%+v", got[opKeyAutostart])
	}
}

// ---------- 目标值是否已满足（差量判定） ----------

func TestKeyTargetSatisfiedSettings(t *testing.T) {
	b := func(v bool) json.RawMessage { return json.RawMessage(strconv.FormatBool(v)) }
	s := func(v string) json.RawMessage { b, _ := json.Marshal(v); return b }

	cur := isAutostartEnabled()
	if !accountKeyTargetSatisfied(opKeyAutostart, b(cur)) {
		t.Fatal("与本机一致应判定为已满足")
	}
	if accountKeyTargetSatisfied(opKeyAutostart, b(!cur)) {
		t.Fatal("与本机不同应判定为未满足")
	}

	oldPre := harnessPrereleaseOverride
	t.Cleanup(func() { harnessPrereleaseOverride = oldPre })
	harnessPrereleaseOverride = false
	if !accountKeyTargetSatisfied(opKeyHarnessPrerelease, b(false)) {
		t.Fatal("预发布通道一致应判定为已满足")
	}
	if accountKeyTargetSatisfied(opKeyHarnessPrerelease, b(true)) {
		t.Fatal("预发布通道不同应判定为未满足")
	}

	if !accountKeyTargetSatisfied(opKeyHarnessVersion, s(installedHarnessVersion())) {
		t.Fatal("Harness 版本一致（含 v 前缀差异）应判定为已满足")
	}
	if accountKeyTargetSatisfied(opKeyHarnessVersion, s("9.9.9-not-installed")) {
		t.Fatal("Harness 版本不同应判定为未满足")
	}

	// 坏值不得被当作「已满足」：否则同步会静默丢记录（表现为「根本没同步」且无提示）。
	// 正确行为是进入待生效，由应用阶段给出明确失败原因（不会真的用坏值改系统设置）。
	if accountKeyTargetSatisfied(opKeyAutostart, json.RawMessage(`"not-a-bool"`)) {
		t.Fatal("非法值不应视为已满足（应进入待生效并给出原因）")
	}
	if accountKeyTargetSatisfied(opKeyHarnessVersion, json.RawMessage(`{"v":"1.0.0"}`)) {
		t.Fatal("非字符串的版本值不应视为已满足")
	}
}

func TestKeyTargetSatisfiedPlugins(t *testing.T) {
	old := accountLocalPluginValueFn
	t.Cleanup(func() { accountLocalPluginValueFn = old })
	accountLocalPluginValueFn = func(profile, name string) (pluginOpValue, bool) {
		if name == "pkg-a" {
			return pluginOpValue{Action: "update", Spec: "^1.0.0", Source: "npm", Version: "1.0.2"}, true
		}
		return pluginOpValue{}, false
	}
	v := func(x pluginOpValue) json.RawMessage { b, _ := json.Marshal(x); return b }

	if !accountKeyTargetSatisfied(accountPluginKey("pkg-a"), v(pluginOpValue{Action: "update", Spec: "^1.0.0", Version: "1.0.2"})) {
		t.Fatal("版本与 spec 一致应判定为已满足")
	}
	if accountKeyTargetSatisfied(accountPluginKey("pkg-a"), v(pluginOpValue{Action: "update", Spec: "^1.0.0", Version: "1.0.3"})) {
		t.Fatal("版本不同应判定为未满足")
	}
	if accountKeyTargetSatisfied(accountPluginKey("pkg-a"), v(pluginOpValue{Action: "remove"})) {
		t.Fatal("已安装时 remove 目标应判定为未满足")
	}
	if !accountKeyTargetSatisfied(accountPluginKey("pkg-b"), v(pluginOpValue{Action: "remove"})) {
		t.Fatal("未安装时 remove 目标应判定为已满足")
	}
	if accountKeyTargetSatisfied("plugin:web:pkg-c", v(pluginOpValue{Action: "install", Spec: "^2.0.0", Version: "2.0.0"})) {
		t.Fatal("未安装时 install 目标应判定为未满足")
	}
}

// ---------- 拉取：游标不能越过未应用的记录 ----------

func TestSyncPullKeepsCursorBeforePending(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())

	// 服务器上有一条「开机自启动 = 与本机相反」的记录 → 与本机不同，进入待生效
	opposite := strconv.FormatBool(!isAutostartEnabled())
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ops/since" {
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ops":[{"seq":5,"opId":"r1","key":"setting:autostart","value":` + opposite +
			`,"deviceId":"d1","updatedAt":1}],"cursor":5,"hasMore":false}`))
	})

	pending, pulled, err := accountSyncPull(context.Background(), client)
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if pulled != 1 || len(pending) != 1 {
		t.Fatalf("应拉到 1 条并产生 1 个待生效 key：pulled=%d pending=%+v", pulled, pending)
	}
	accountSetPendingApply(pending)

	accountMu.Lock()
	cursor := accountCur.Cursor
	accountMu.Unlock()
	if cursor != 4 {
		t.Fatalf("游标必须停在待生效记录之前（应为 4），实际 %d", cursor)
	}
	if keys := accountPendingKeys(); len(keys) != 1 || keys[0] != opKeyAutostart {
		t.Fatalf("待生效集合错误：%v", keys)
	}
	if st := accountSnapshot(); !st.PendingApply {
		t.Fatal("快照应标记 PendingApply")
	}
}

// ---------- 应用：顺序、清理、失败保留 ----------

// syncTestServer 假服务器：/v1/ops/since 返回给定记录，/v1/ops/report 返回接受。
func syncTestServer(t *testing.T, opsJSON string) *accountClient {
	t.Helper()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ops/since":
			_, _ = w.Write([]byte(`{"ops":` + opsJSON + `,"cursor":9,"hasMore":false}`))
		case "/v1/ops/report":
			_, _ = w.Write([]byte(`{"accepted":0,"duplicates":0,"cursor":9}`))
		default:
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
	})
	return client
}

func TestApplyPendingAppliesInOrderAndClears(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())

	// 四个 key 都与本机当前状态不同 → 都应被应用
	autoTarget := !isAutostartEnabled()
	preTarget := !harnessPrereleaseOverride
	opsJSON := fmt.Sprintf(`[`+
		`{"seq":1,"opId":"p1","key":"plugin:web:pkg-a","value":{"action":"remove"},"deviceId":"d","updatedAt":1},`+
		`{"seq":2,"opId":"p2","key":"setting:harness_version","value":"9.9.9-sync","deviceId":"d","updatedAt":1},`+
		`{"seq":3,"opId":"p3","key":"setting:harness_prerelease","value":%v,"deviceId":"d","updatedAt":1},`+
		`{"seq":4,"opId":"p4","key":"setting:autostart","value":%v,"deviceId":"d","updatedAt":1}]`, preTarget, autoTarget)

	// 插件 key 需要「本机已安装」才会被判为未满足 → 注入本机状态
	oldLocal := accountLocalPluginValueFn
	accountLocalPluginValueFn = func(profile, name string) (pluginOpValue, bool) {
		return pluginOpValue{Action: "update", Spec: "^1.0.0", Source: "npm", Version: "1.0.2"}, true
	}
	t.Cleanup(func() { accountLocalPluginValueFn = oldLocal })

	var calls []string
	oldAuto, oldPre, oldVer, oldPlg := applyAutostartFn, applyPrereleaseFn, applyHarnessVersionFn, applyPluginOpFn
	t.Cleanup(func() {
		applyAutostartFn, applyPrereleaseFn, applyHarnessVersionFn, applyPluginOpFn = oldAuto, oldPre, oldVer, oldPlg
	})
	applyAutostartFn = func(on bool) error { calls = append(calls, fmt.Sprintf("autostart=%v", on)); return nil }
	applyPrereleaseFn = func(on bool) error { calls = append(calls, fmt.Sprintf("prerelease=%v", on)); return nil }
	applyHarnessVersionFn = func(v string) error { calls = append(calls, "version="+v); return nil }
	applyPluginOpFn = func(name string, v pluginOpValue) error {
		calls = append(calls, "plugin="+name+":"+v.Action)
		return nil
	}

	client := syncTestServer(t, opsJSON)
	if _, err := accountSyncNow(context.Background(), client); err != nil {
		t.Fatalf("同步检查失败: %v", err)
	}
	res, err := accountApplyPending(context.Background(), client, nil)
	if err != nil {
		t.Fatalf("应用失败: %v", err)
	}

	want := []string{
		fmt.Sprintf("autostart=%v", autoTarget),
		fmt.Sprintf("prerelease=%v", preTarget),
		"version=9.9.9-sync",
		"plugin=pkg-a:remove",
	}
	if len(calls) != len(want) {
		t.Fatalf("应用次数错误：%v（期望 %v）", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("应用顺序错误：%v（期望 %v）", calls, want)
		}
	}
	if res.Applied != 4 || res.Unchanged != 0 {
		t.Fatalf("结果计数错误：applied=%d unchanged=%d", res.Applied, res.Unchanged)
	}
	if keys := accountPendingKeys(); len(keys) != 0 {
		t.Fatalf("应用后待生效集合应清空：%v", keys)
	}
	if st := accountSnapshot(); st.PendingApply {
		t.Fatal("快照的 PendingApply 应复位")
	}
}

// TestApplyPendingFailureKeepsOthersGoing 单项失败不中止整批：
// 失败项保留在待生效集合、其余项照常应用（2026-09-21 评估问题③：此前首个失败即中止，
// 用户点一次「重启生效」只能推进到第一个网络失败的插件）。
func TestApplyPendingFailureKeepsOthersGoing(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())

	autoTarget := !isAutostartEnabled()
	preTarget := !harnessPrereleaseOverride
	opsJSON := fmt.Sprintf(`[`+
		`{"seq":1,"opId":"p2","key":"setting:harness_version","value":"9.9.9-sync","deviceId":"d","updatedAt":1},`+
		`{"seq":2,"opId":"p3","key":"setting:harness_prerelease","value":%v,"deviceId":"d","updatedAt":1},`+
		`{"seq":3,"opId":"p4","key":"setting:autostart","value":%v,"deviceId":"d","updatedAt":1}]`, preTarget, autoTarget)

	oldAuto, oldPre, oldVer, oldPlg := applyAutostartFn, applyPrereleaseFn, applyHarnessVersionFn, applyPluginOpFn
	t.Cleanup(func() {
		applyAutostartFn, applyPrereleaseFn, applyHarnessVersionFn, applyPluginOpFn = oldAuto, oldPre, oldVer, oldPlg
	})
	applied := 0
	applyAutostartFn = func(bool) error { applied++; return nil }
	applyPrereleaseFn = func(bool) error { return fmt.Errorf("权限不足，无法写入自启动项") }
	applyHarnessVersionFn = func(string) error { applied++; return nil }
	applyPluginOpFn = func(string, pluginOpValue) error { applied++; return nil }

	client := syncTestServer(t, opsJSON)
	if _, err := accountSyncNow(context.Background(), client); err != nil {
		t.Fatalf("同步检查失败: %v", err)
	}
	res, err := accountApplyPending(context.Background(), client, nil)
	if err != nil {
		t.Fatalf("单项失败不应作为整批致命错误返回: %v", err)
	}
	if applied != 2 || res.Applied != 2 {
		t.Fatalf("失败项之外的改动应继续应用，实际 applied=%d res.Applied=%d", applied, res.Applied)
	}
	if len(res.Failed) != 1 || res.Failed[opKeyHarnessPrerelease] == "" {
		t.Fatalf("失败项应记入 res.Failed：%+v", res.Failed)
	}
	keys := accountPendingKeys()
	if len(keys) != 1 || keys[0] != opKeyHarnessPrerelease {
		t.Fatalf("失败项应保留待生效（下次可续做），实际 %v", keys)
	}
	st := accountSnapshot()
	if !st.PendingApply || st.ApplyError == "" {
		t.Fatalf("快照应保留重启提示与应用失败原因：%+v", st)
	}
	if st.SyncError != "" {
		t.Fatalf("应用失败不得写进同步错误（否则下次同步检查会把状态刷成「已同步」）：%+v", st)
	}
}

// ---------- 目标值形态容错（2026-09-21 现场问题：同一记录被反复重装） ----------

func TestVersionTargetSatisfied(t *testing.T) {
	cases := []struct {
		installed, target string
		want              bool
	}{
		{"1.7.30", "^1.7.30", true}, // 服务器记录 Version 字段是范围（现场形态）
		{"1.7.31", "^1.7.30", true}, // 范围内
		{"2.0.0", "^1.7.30", false}, // 越界
		{"1.7.30", "1.7.30", true},  // 精确
		{"v1.7.30", "1.7.30", true}, // v 前缀
		{"1.7.30", "v1.7.30", true}, // 目标带 v
		{"1.7.30", "~1.7.0", true},  // ~ 范围
		{"1.8.0", "~1.7.0", false},  // ~ 越界
		{"1.7.30", ">=1.7.0", true}, // 比较符
		{"1.6.0", ">=1.7.0", false},
		{"1.7.30", "1.x", true},    // 通配
		{"1.7.30", "latest", true}, // latest
		{"1.7.30", "9.9.9", false}, // 不同版本
		{"", "^1.7.30", false},     // 本机版本未知 → 未满足（由应用阶段给出原因）
		{"0.2.1", "^0.2.0", true},  // 0.x 的 caret 语义
		{"0.3.0", "^0.2.0", false},
	}
	for _, c := range cases {
		if got := versionTargetSatisfied(c.installed, c.target); got != c.want {
			t.Fatalf("versionTargetSatisfied(%q, %q) = %v，期望 %v", c.installed, c.target, got, c.want)
		}
	}
}

func TestPluginSpecSatisfiedToleratesSpecShape(t *testing.T) {
	cur := pluginOpValue{Action: "update", Spec: "github:refyon/dsh-ui-taste", Source: "github", Version: "0.2.1"}
	if !pluginSpecSatisfied(cur, "github:refyon/dsh-ui-taste") {
		t.Fatal("声明一致应判定为满足")
	}
	if !pluginSpecSatisfied(cur, "https://github.com/refyon/dsh-ui-taste.git") {
		t.Fatal("github 声明写法差异应判定为满足（归一化后同一来源）")
	}
	if pluginSpecSatisfied(cur, "github:refyon/other-plugin") {
		t.Fatal("不同来源不应判定为满足")
	}
	// 依赖声明写法不同但已装版本满足范围：也算满足（跨机 ^1.7.30 vs 1.7.30）
	npm := pluginOpValue{Action: "update", Spec: "1.7.30", Source: "npm", Version: "1.7.30"}
	if !pluginSpecSatisfied(npm, "^1.7.30") {
		t.Fatal("已装版本满足目标范围应判定为满足")
	}
}

// TestSyncPullSkipsAppliedSeqs 已应用过的服务器记录不再进入待生效集合：
// 本机状态读取口径与目标值形态不完全一致时，同一记录不该被反复应用（现场问题④）。
func TestSyncPullSkipsAppliedSeqs(t *testing.T) {
	setupAccountTest(t)
	st := loggedInState()
	st.AppliedSeqs = map[string]int64{opKeyHarnessVersion: 5}
	setAccountState(st)

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ops":[{"seq":5,"opId":"h1","key":"setting:harness_version","value":"9.9.9-sync","deviceId":"d","updatedAt":1}],"cursor":5,"hasMore":false}`))
	})
	pending, _, err := accountSyncPull(context.Background(), client)
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("已应用序号的记录不应再进入待生效：%+v", pending)
	}
}

// TestRevalidatePendingApplyOnStartup 启动重校验：已满足/已应用序号的项丢弃，
// 仍需应用的项保留（应用中途退出后不把半途状态当成事实）。
func TestRevalidatePendingApplyOnStartup(t *testing.T) {
	setupAccountTest(t)
	oldLocal := accountLocalPluginValueFn
	t.Cleanup(func() { accountLocalPluginValueFn = oldLocal })
	accountLocalPluginValueFn = func(profile, name string) (pluginOpValue, bool) {
		return pluginOpValue{Action: "update", Spec: "^1.0.0", Source: "npm", Version: "1.0.2"}, true
	}

	st := loggedInState()
	st.PendingRemote = []accountPendingOp{
		// 已满足（本机版本一致）→ 应丢弃
		{Key: opKeyHarnessVersion, Value: json.RawMessage(`"` + installedHarnessVersion() + `"`), Seq: 3},
		// 已应用序号覆盖 → 应丢弃（即使本机状态看起来不满足）
		{Key: accountPluginKey("pkg-a"), Value: json.RawMessage(`{"action":"update","spec":"^9.0.0","version":"9.0.0"}`), Seq: 4},
		// 仍需应用 → 应保留
		{Key: opKeyAutostart, Value: json.RawMessage(strconv.FormatBool(!isAutostartEnabled())), Seq: 6},
	}
	st.AppliedSeqs = map[string]int64{accountPluginKey("pkg-a"): 4}
	st.PendingApply = true
	setAccountState(st)

	revalidatePendingApplyOnStartup()

	keys := accountPendingKeys()
	if len(keys) != 1 || keys[0] != opKeyAutostart {
		t.Fatalf("重校验后应只剩仍需应用的项，实际 %v", keys)
	}
	if st := accountSnapshot(); !st.PendingApply || st.ApplyError != "" {
		t.Fatalf("应保留待生效提示并复位旧的应用失败原因：%+v", st)
	}
}

// TestRevalidatePendingApplyClearsWhenAllDone 全部已满足时清空待生效（下次启动不再提示重启）。
func TestRevalidatePendingApplyClearsWhenAllDone(t *testing.T) {
	setupAccountTest(t)
	st := loggedInState()
	st.PendingRemote = []accountPendingOp{
		{Key: opKeyHarnessVersion, Value: json.RawMessage(`"` + installedHarnessVersion() + `"`), Seq: 3},
	}
	st.PendingApply = true
	setAccountState(st)

	revalidatePendingApplyOnStartup()

	if keys := accountPendingKeys(); len(keys) != 0 {
		t.Fatalf("全部已满足时应清空待生效集合，实际 %v", keys)
	}
	if st := accountSnapshot(); st.PendingApply {
		t.Fatal("PendingApply 应复位")
	}
}
