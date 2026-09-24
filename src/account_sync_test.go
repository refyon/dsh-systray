package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
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

// TestKeyTargetSatisfiedRemoveUsesTimestamp 删除墓碑的时间戳判据（2026-09-24 现场修复）：
// 本机装着时，只有「本机安装时间晚于该墓碑的写入时间」才算已满足（墓碑已被更晚的安装盖过，
// 本机是删掉后又装回来的）；安装更早或时间未知时保守视为未满足（删除方胜）。
func TestKeyTargetSatisfiedRemoveUsesTimestamp(t *testing.T) {
	oldVal, oldTime := accountLocalPluginValueFn, accountLocalPluginInstallTimeForFn
	t.Cleanup(func() { accountLocalPluginValueFn, accountLocalPluginInstallTimeForFn = oldVal, oldTime })
	accountLocalPluginValueFn = func(profile, name string) (pluginOpValue, bool) {
		return pluginOpValue{Action: "update", Spec: "^1.7.35", Source: "npm", Version: "1.7.35"}, true
	}
	accountLocalPluginInstallTimeForFn = func(profile, name string) int64 { return 3000 }

	key := accountPluginKey("dsh-cost-meter")
	remove := json.RawMessage(`{"action":"remove","spec":"^1.7.35","source":"npm","version":""}`)

	if !accountKeyTargetSatisfiedAt(key, remove, 2000) {
		t.Fatal("本机安装（3000）晚于删除墓碑（2000）：应视为已满足（墓碑已被盖过）")
	}
	if accountKeyTargetSatisfiedAt(key, remove, 4000) {
		t.Fatal("本机安装（3000）早于删除墓碑（4000）：不应视为已满足（删除方胜）")
	}
	if accountKeyTargetSatisfiedAt(key, remove, 0) {
		t.Fatal("墓碑写入时间未知时应保守视为未满足")
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

// TestSyncPullAppliedSeqsRecheckedAgainstLocal 已应用过的服务器记录按**本机当前状态**重判：
// 本机仍满足才跳过；本机被重置（删除 .dsh / harness 目录后插件与版本消失）时必须重新入队，
// 否则会出现「显示已同步、实际什么都没恢复」（2026-09-22 现场问题①）。
func TestSyncPullAppliedSeqsRecheckedAgainstLocal(t *testing.T) {
	useTempHarnessDir(t)
	writeInstalledHarnessVersion(t, "1.2.3")

	remoteOps := func(ver string) string {
		return `{"ops":[{"seq":5,"opId":"h1","key":"setting:harness_version","value":"` + ver + `","deviceId":"d","updatedAt":1}],"cursor":5,"hasMore":false}`
	}
	pullWith := func(t *testing.T, body string) map[string]accountSyncTarget {
		t.Helper()
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		})
		pending, _, err := accountSyncPull(context.Background(), client)
		if err != nil {
			t.Fatalf("拉取失败: %v", err)
		}
		return pending
	}

	t.Run("本机仍满足→跳过（同一记录不反复应用）", func(t *testing.T) {
		setupAccountTest(t)
		st := loggedInState()
		st.AppliedSeqs = map[string]int64{opKeyHarnessVersion: 5}
		setAccountState(st)

		if pending := pullWith(t, remoteOps("1.2.3")); len(pending) != 0 {
			t.Fatalf("本机仍满足时不应进入待生效：%+v", pending)
		}
	})

	t.Run("本机已偏离→重新入队", func(t *testing.T) {
		setupAccountTest(t)
		st := loggedInState()
		st.AppliedSeqs = map[string]int64{opKeyHarnessVersion: 5}
		setAccountState(st)

		pending := pullWith(t, remoteOps("9.9.9-not-installed"))
		if _, ok := pending[opKeyHarnessVersion]; !ok {
			t.Fatalf("已应用序号但本机版本不符时应重新入队：%+v", pending)
		}
	})
}

// TestSyncPullReenqueuesMissingPlugin 本机插件被删除（删除 .dsh 目录后被重置）时，
// 即使该记录曾应用过也必须重新进入待生效（现场问题①：显示已同步、插件却没恢复）。
func TestSyncPullReenqueuesMissingPlugin(t *testing.T) {
	setupAccountTest(t)
	oldLocal := accountLocalPluginValueFn
	t.Cleanup(func() { accountLocalPluginValueFn = oldLocal })
	accountLocalPluginValueFn = func(profile, name string) (pluginOpValue, bool) {
		return pluginOpValue{}, false // 本机没有该插件（被重置）
	}
	key := accountPluginKey("pkg-gone")
	st := loggedInState()
	st.AppliedSeqs = map[string]int64{key: 7}
	setAccountState(st)

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ops":[{"seq":7,"opId":"p1","key":"` + key + `","value":{"action":"update","spec":"^1.0.0","source":"npm","version":"1.0.2"},"deviceId":"d","updatedAt":1}],"cursor":7,"hasMore":false}`))
	})
	pending, _, err := accountSyncPull(context.Background(), client)
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if _, ok := pending[key]; !ok {
		t.Fatalf("插件已被本机删除时应重新入队：%+v", pending)
	}
}

// TestKeyTargetSatisfiedPluginVersionUnreadable 本机已装但版本读不到时不判为差异：
// 判读失败不等于版本不符——否则「已应用 且 读不到版本」会在每次同步里反复入队重装
// （修复①不能把 2026-09-21 的反复重装问题带回来）。
func TestKeyTargetSatisfiedPluginVersionUnreadable(t *testing.T) {
	old := accountLocalPluginValueFn
	t.Cleanup(func() { accountLocalPluginValueFn = old })
	accountLocalPluginValueFn = func(profile, name string) (pluginOpValue, bool) {
		return pluginOpValue{Action: "install", Spec: "^1.0.0", Source: "npm", Version: ""}, true
	}
	v := func(x pluginOpValue) json.RawMessage { b, _ := json.Marshal(x); return b }

	if !accountKeyTargetSatisfied(accountPluginKey("pkg-x"), v(pluginOpValue{Action: "update", Spec: "^1.0.0", Version: "1.0.2"})) {
		t.Fatal("版本读不到但声明一致时应判定为已满足（判读失败不等于差异）")
	}
	if accountKeyTargetSatisfied(accountPluginKey("pkg-x"), v(pluginOpValue{Action: "update", Spec: "github:owner/repo", Version: ""})) {
		t.Fatal("声明不同且版本不可比时应判定为未满足")
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
		// 已应用序号覆盖但本机状态不满足（已装 1.0.2 不满足目标 ^9.0.0）→ 应保留
		{Key: accountPluginKey("pkg-a"), Value: json.RawMessage(`{"action":"update","spec":"^9.0.0","version":"9.0.0"}`), Seq: 4},
		// 仍需应用 → 应保留
		{Key: opKeyAutostart, Value: json.RawMessage(strconv.FormatBool(!isAutostartEnabled())), Seq: 6},
	}
	st.AppliedSeqs = map[string]int64{accountPluginKey("pkg-a"): 4}
	st.PendingApply = true
	setAccountState(st)

	revalidatePendingApplyOnStartup()

	keys := accountPendingKeys()
	if len(keys) != 2 || keys[0] != accountPluginKey("pkg-a") || keys[1] != opKeyAutostart {
		t.Fatalf("重校验后应保留本机状态不满足的项（已应用序号不代表仍生效），实际 %v", keys)
	}
	if st := accountSnapshot(); !st.PendingApply || st.ApplyError != "" {
		t.Fatalf("应保留待生效提示并复位旧的应用失败原因：%+v", st)
	}
}

// TestReenqueueDriftedAppliedCatchesResetDirs 手工删除 .dsh / harness 目录后，服务器游标已推进到
// 已应用记录之后（增量拉取再也拿不到它们），必须靠持久化的目标值重判漂移并重新入队
// （2026-09-22 现场问题①：重启托盘显示「已同步」，插件列表却是空的）。
func TestReenqueueDriftedAppliedCatchesResetDirs(t *testing.T) {
	setupAccountTest(t)
	oldLocal := accountLocalPluginValueFn
	t.Cleanup(func() { accountLocalPluginValueFn = oldLocal })
	accountLocalPluginValueFn = func(profile, name string) (pluginOpValue, bool) {
		return pluginOpValue{}, false // 本机插件已被删除（目录重置）
	}
	key := accountPluginKey("pkg-gone")
	st := loggedInState()
	st.Cursor = 20
	st.AppliedSeqs = map[string]int64{key: 7}
	st.AppliedVals = map[string]appliedRecord{
		key: {Seq: 7, Value: json.RawMessage(`{"action":"update","spec":"^1.0.0","source":"npm","version":"1.0.2"}`)},
	}
	setAccountState(st)

	if n := accountReenqueueDriftedApplied(); n != 1 {
		t.Fatalf("应重新入队 1 项，实际 %d", n)
	}
	if keys := accountPendingKeys(); len(keys) != 1 || keys[0] != key {
		t.Fatalf("漂移项应回到待生效集合：%v", keys)
	}
	accountMu.Lock()
	cursor, pending := accountCur.Cursor, accountCur.PendingApply
	accountMu.Unlock()
	if cursor != 6 {
		t.Fatalf("游标应回拨到漂移记录之前（6），实际 %d", cursor)
	}
	if !pending || !accountSnapshot().PendingApply {
		t.Fatal("应标记待生效（前端据此常驻显示「重启生效」）")
	}
	if n := accountReenqueueDriftedApplied(); n != 0 {
		t.Fatalf("已在待生效集合中的项不应重复入队：%d", n)
	}
}

// TestReenqueueDriftedAppliedSkipsSatisfied 本机仍满足时不重判入队：正常启动不得凭空产生
// 「重启生效」提示，也不得无谓回拨游标（否则每次启动都会重拉一遍历史记录）。
func TestReenqueueDriftedAppliedSkipsSatisfied(t *testing.T) {
	setupAccountTest(t)
	oldLocal := accountLocalPluginValueFn
	t.Cleanup(func() { accountLocalPluginValueFn = oldLocal })
	accountLocalPluginValueFn = func(profile, name string) (pluginOpValue, bool) {
		return pluginOpValue{Action: "update", Spec: "^1.0.0", Source: "npm", Version: "1.0.2"}, true
	}
	key := accountPluginKey("pkg-a")
	st := loggedInState()
	st.Cursor = 20
	st.AppliedSeqs = map[string]int64{key: 7}
	st.AppliedVals = map[string]appliedRecord{
		key: {Seq: 7, Value: json.RawMessage(`{"action":"update","spec":"^1.0.0","source":"npm","version":"1.0.2"}`)},
	}
	setAccountState(st)

	if n := accountReenqueueDriftedApplied(); n != 0 {
		t.Fatalf("本机仍满足时不应入队：%d", n)
	}
	if keys := accountPendingKeys(); len(keys) != 0 {
		t.Fatalf("不应产生待生效项：%v", keys)
	}
	accountMu.Lock()
	cursor := accountCur.Cursor
	accountMu.Unlock()
	if cursor != 20 {
		t.Fatalf("无漂移时不得回拨游标，实际 %d", cursor)
	}
}

// TestStartupRevalidationRequeuesDriftedApplied 启动重校验覆盖「已应用记录漂移」：
// 删目录后重启时 PendingRemote 本就为空，旧实现只看 PendingRemote 会整条漏掉漂移。
func TestStartupRevalidationRequeuesDriftedApplied(t *testing.T) {
	useTempHarnessDir(t)
	writeInstalledHarnessVersion(t, "1.2.3")
	setupAccountTest(t)
	st := loggedInState()
	st.Cursor = 20
	st.AppliedSeqs = map[string]int64{opKeyHarnessVersion: 5}
	st.AppliedVals = map[string]appliedRecord{
		opKeyHarnessVersion: {Seq: 5, Value: json.RawMessage(`"9.9.9-not-installed"`)},
	}
	setAccountState(st)

	revalidatePendingApplyOnStartup()

	if keys := accountPendingKeys(); len(keys) != 1 || keys[0] != opKeyHarnessVersion {
		t.Fatalf("启动重校验应把漂移记录放回待生效：%v", keys)
	}
	if st := accountSnapshot(); !st.PendingApply || st.PendingApplyCount != 1 {
		t.Fatalf("应标记待生效 1 项（前端「重启生效」按钮）：%+v", st)
	}
}

// TestSyncNowReenqueuesDriftedApplied 「立即同步」路径同样兜底重判漂移：托盘运行期间用户手动
// 删除目录时，本轮拉取为空也必须重新给出待生效提示。
func TestSyncNowReenqueuesDriftedApplied(t *testing.T) {
	useTempHarnessDir(t) // 版本读不到 → 版本对账提前返回，测试不触发真实网络
	setupAccountTest(t)
	oldLocal := accountLocalPluginValueFn
	t.Cleanup(func() { accountLocalPluginValueFn = oldLocal })
	accountLocalPluginValueFn = func(profile, name string) (pluginOpValue, bool) {
		return pluginOpValue{}, false
	}
	key := accountPluginKey("pkg-gone")
	st := loggedInState()
	st.BaselineDone = true
	st.Cursor = 9
	st.AppliedVals = map[string]appliedRecord{
		key: {Seq: 9, Value: json.RawMessage(`{"action":"update","spec":"^1.0.0","source":"npm","version":"1.0.2"}`)},
	}
	setAccountState(st)

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ops":[],"cursor":9,"hasMore":false}`)) // 游标之后没有新记录
	})
	res, err := accountSyncNow(context.Background(), client)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if res.Reenqueued != 1 {
		t.Fatalf("同步应重判出 1 项漂移，实际 %d", res.Reenqueued)
	}
	if keys := accountPendingKeys(); len(keys) != 1 || keys[0] != key {
		t.Fatalf("漂移项应进入待生效集合：%v", keys)
	}
}

// TestAppliedValsPersistAcrossReload 已应用目标值与序号一起落盘并在下次读取时还原
// （跨托盘重启后仍能重判漂移）。
func TestAppliedValsPersistAcrossReload(t *testing.T) {
	setupAccountTest(t)
	setAccountState(loggedInState())
	key := accountPluginKey("pkg-a")
	val := json.RawMessage(`{"action":"update","spec":"^1.0.0","source":"npm","version":"1.0.2"}`)
	accountMarkApplied(key, val, 9, 1700000000)

	loaded := loadAccountState()
	rec, ok := loaded.AppliedVals[key]
	if !ok || rec.Seq != 9 {
		t.Fatalf("已应用目标值应随 account.json 持久化：%+v", loaded.AppliedVals)
	}
	// 落盘经 json.MarshalIndent 重新缩进，按语义比较（不比较字面量空白）。
	var gotVal, wantVal pluginOpValue
	if err := json.Unmarshal(rec.Value, &gotVal); err != nil {
		t.Fatalf("已应用目标值不是合法 JSON：%v（%s）", err, rec.Value)
	}
	_ = json.Unmarshal(val, &wantVal)
	if gotVal != wantVal {
		t.Fatalf("已应用目标值内容不符：%+v != %+v", gotVal, wantVal)
	}
	if loaded.AppliedSeqs[key] != 9 {
		t.Fatalf("旧字段 AppliedSeqs 应继续维护（向后兼容）：%+v", loaded.AppliedSeqs)
	}
	if loaded.PendingApply || len(loaded.PendingRemote) != 0 {
		t.Fatalf("应用成功后不应留待生效项：%+v", loaded.PendingRemote)
	}
}

// TestAccountStateLegacyAppliedSeqsLoads 旧版 account.json（只有 appliedSeqs、没有 appliedVals）
// 必须照常读取：新增字段不得把老用户读成未登录。
func TestAccountStateLegacyAppliedSeqsLoads(t *testing.T) {
	setupAccountTest(t)
	p := accountStatePath()
	if p == "" {
		t.Fatal("测试应已注入 account.json 目录")
	}
	legacy := `{"token":"tok-1","issuedAt":1,"tokenExpiresAt":9999999999,"email":"user@example.com",` +
		`"appliedSeqs":{"plugin:web:pkg-a":7}}`
	if err := os.WriteFile(p, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st := loadAccountState()
	if st.Token != "tok-1" || st.AppliedSeqs["plugin:web:pkg-a"] != 7 {
		t.Fatalf("旧格式登录态应完整读取：%+v", st)
	}
	if len(st.AppliedVals) != 0 {
		t.Fatalf("旧格式无 appliedVals，应为空：%+v", st.AppliedVals)
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

// ---------- 游标不变量：上传确认不得越过未生效记录，存量状态一次性迁移 ----------

// testOp 假服务器上的一条账号记录（value 为 JSON 字面量）。
type testOp struct {
	seq   int64
	key   string
	value string
}

// fakeOpsServer 假服务器：/v1/ops/since 按 since 只返回 seq 更大的记录（复刻服务端增量语义
// ——必须尊重 since，否则「游标被推到未生效记录之后」这类缺陷在测试里看不见）；
// /v1/ops/report 返回给定的上报后游标。
func fakeOpsServer(t *testing.T, ops []testOp, reportCursor int64) *accountClient {
	t.Helper()
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ops/since":
			since, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
			maxSeq := since
			var out []string
			for _, op := range ops {
				if op.seq <= since {
					continue
				}
				if op.seq > maxSeq {
					maxSeq = op.seq
				}
				out = append(out, fmt.Sprintf(
					`{"seq":%d,"opId":"op-%d","key":%s,"value":%s,"deviceId":"d","updatedAt":1}`,
					op.seq, op.seq, strconv.Quote(op.key), op.value))
			}
			_, _ = w.Write([]byte(fmt.Sprintf(`{"ops":[%s],"cursor":%d,"hasMore":false}`, strings.Join(out, ","), maxSeq)))
		case "/v1/ops/report":
			_, _ = w.Write([]byte(fmt.Sprintf(`{"accepted":1,"duplicates":0,"cursor":%d}`, reportCursor)))
		default:
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
	})
	return client
}

// TestCursorInvariantMigrationRewindsLegacyStateOnce 存量 account.json（旧版本写入：游标已越过
// 未生效记录 / 已应用记录只有序号没有目标值）一次性把游标回拨到 0 做全量重拉，此后不再回拨。
func TestCursorInvariantMigrationRewindsLegacyStateOnce(t *testing.T) {
	setupAccountTest(t)
	st := loggedInState()
	st.BaselineDone = true
	st.Cursor = 50
	st.AppliedSeqs = map[string]int64{accountPluginKey("pkg-a"): 7} // 老格式：没有 appliedVals
	setAccountState(st)

	accountMigrateCursorInvariant()

	accountMu.Lock()
	cursor, marked := accountCur.Cursor, accountCur.SyncInvariantOK
	accountMu.Unlock()
	if cursor != 0 || !marked {
		t.Fatalf("存量状态应回拨游标到 0 并置迁移标记：cursor=%d marked=%v", cursor, marked)
	}

	// 迁移只做一次：再次调用不得把已推进的游标再拉回 0（否则每次启动都重拉一遍历史记录）
	accountMu.Lock()
	accountCur.Cursor = 33
	accountMu.Unlock()
	accountMigrateCursorInvariant()
	accountMu.Lock()
	cursor = accountCur.Cursor
	accountMu.Unlock()
	if cursor != 33 {
		t.Fatalf("迁移应只执行一次，实际游标被再次回拨到 %d", cursor)
	}
}

// TestLegacyStateRecoversAfterReset 老格式 account.json + 本机被重置（删 .dsh / harness 目录）：
// 迁移后全量重拉必须重新给出待生效项，而不是继续显示「已同步」
// （2026-09-22 现场问题①；上一版修复依赖 appliedVals，对存量老格式无效）。
func TestLegacyStateRecoversAfterReset(t *testing.T) {
	useTempHarnessDir(t) // 版本读不到 → 版本对账提前返回，不触发真实网络
	setupAccountTest(t)
	oldLocal := accountLocalPluginValueFn
	t.Cleanup(func() { accountLocalPluginValueFn = oldLocal })
	accountLocalPluginValueFn = func(profile, name string) (pluginOpValue, bool) {
		return pluginOpValue{}, false // 目录被删：插件全部消失
	}
	key := accountPluginKey("pkg-gone")
	st := loggedInState()
	st.BaselineDone = true
	st.Cursor = 50 // 旧版本把游标推到了记录之后：增量拉取永远取不到
	st.AppliedSeqs = map[string]int64{key: 7}
	setAccountState(st)

	accountMigrateCursorInvariant() // 启动重校验的第一步

	client := fakeOpsServer(t, []testOp{
		{seq: 7, key: key, value: `{"action":"update","spec":"^1.0.0","source":"npm","version":"1.0.2"}`},
	}, 50)
	if _, err := accountSyncNow(context.Background(), client); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if keys := accountPendingKeys(); len(keys) != 1 || keys[0] != key {
		t.Fatalf("全量重拉后应给出待生效项（否则表现为「已同步」）：%v", keys)
	}
	if st := accountSnapshot(); !st.PendingApply {
		t.Fatalf("应标记待生效（前端「重启生效」按钮）：%+v", st)
	}
}

// TestApplyFailureKeepsRestartButtonAcrossSyncs 插件应用失败后，再点「立即同步」必须能再次出现
// 「重启生效」（2026-09-22 现场问题②）。
//
// 复现链：应用 Harness 版本时其重置流程结束会补报版本 → 异步上报确认把游标推到服务器头部
// （越过仍待生效的插件记录）→ 插件应用失败 → 记录留在待生效但落在游标之前 → 下一次同步的
// 整体替换把它静默丢弃，用户怎么点同步都不再弹按钮。
func TestApplyFailureKeepsRestartButtonAcrossSyncs(t *testing.T) {
	useTempHarnessDir(t)
	setupAccountTest(t)
	setAccountState(loggedInState())
	oldLocal := accountLocalPluginValueFn
	t.Cleanup(func() { accountLocalPluginValueFn = oldLocal })
	accountLocalPluginValueFn = func(profile, name string) (pluginOpValue, bool) {
		return pluginOpValue{}, false // 本机没有该插件：目标必须进入待生效
	}
	oldPlg := applyPluginOpFn
	t.Cleanup(func() { applyPluginOpFn = oldPlg })
	applyPluginOpFn = func(string, pluginOpValue) error { return fmt.Errorf("安装失败：模拟故障") }

	key := accountPluginKey("pkg-fail")
	client := fakeOpsServer(t, []testOp{
		{seq: 30, key: key, value: `{"action":"update","spec":"^1.0.0","source":"npm","version":"1.0.2"}`},
	}, 50)

	if _, err := accountSyncNow(context.Background(), client); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if keys := accountPendingKeys(); len(keys) != 1 || keys[0] != key {
		t.Fatalf("同步后应给出待生效项：%v", keys)
	}

	// 模拟「Harness 版本已应用 → 重置流程补报版本 → 异步上报确认」把游标推到服务器头部
	if err := accountEnqueueOp(opKeyHarnessVersion, "9.9.9-applied"); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	if _, err := accountFlushOps(context.Background(), client); err != nil {
		t.Fatalf("上报失败: %v", err)
	}

	res, err := accountApplyPending(context.Background(), client, nil)
	if err != nil {
		t.Fatalf("单项失败不应作为整批致命错误返回: %v", err)
	}
	if len(res.Failed) != 1 || res.Failed[key] == "" {
		t.Fatalf("插件应用应失败并保留在待生效：%+v", res.Failed)
	}
	accountMu.Lock()
	cursor := accountCur.Cursor
	accountMu.Unlock()
	if cursor != 29 {
		t.Fatalf("应用失败后游标必须停在未生效记录之前（29），实际 %d", cursor)
	}

	// 用户再点「立即同步」：必须能重新拉到这条记录并再次给出「重启生效」
	if _, err := accountSyncNow(context.Background(), client); err != nil {
		t.Fatalf("再次同步失败: %v", err)
	}
	if keys := accountPendingKeys(); len(keys) != 1 || keys[0] != key {
		t.Fatalf("失败项必须能再次进入待生效（否则再也弹不出「重启生效」）：%v", keys)
	}
	if st := accountSnapshot(); !st.PendingApply {
		t.Fatalf("应保留重启提示：%+v", st)
	}
}
