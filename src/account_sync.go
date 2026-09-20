// account_sync.go：操作记录同步引擎——增量拉取、按 key 合并（LWW）、按差量应用。
//
// 模型（冻结决策见 Docs/dsh-systray-connect-sync-plan.md）：
//   - 服务器是 per-key LWW 的事件流；客户端「每个 key 取最新一条」即得目标状态；
//   - 合并：opId 去重 → 同 key 取服务器 seq 最大者；本地**尚未上报**的改动手上没有 seq，
//     视为更新（用户刚做的操作不该被远端覆盖）；
//   - 拉到的目标值与**本机当前值相同**时不进入待生效集合（避免无意义的重启提示）；
//   - 应用只在用户点「重启生效」时发生（需求②：不自动生效）；应用只处理与当前值不同的 key。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// accountSyncResult 一次同步检查的结果（状态页与测试断言用）。
type accountSyncResult struct {
	Uploaded  int      // 本次上报的本地变更条数
	Pulled    int      // 本次拉到并记入待生效的服务器记录条数
	Baseline  bool     // 是否执行了「服务器为空时首登上报本地基线」
	Pending   []string // 待生效的 key（升序）
	Applied   int      // 应用阶段真正执行的变更数（仅在 Apply 时非 0）
	Unchanged int      // 与服务端一致、无需应用的 key 数
}

// accountSyncTarget 合并后某个 key 的目标值。
type accountSyncTarget struct {
	Key       string
	Value     json.RawMessage
	Seq       int64 // 服务器序号；0 = 来自本地未上报队列
	FromLocal bool
}

// accountSyncKeySupported 是否属于同步范围（只同步 web profile 的在线插件 + 三个设置项）。
func accountSyncKeySupported(key string) bool {
	switch key {
	case opKeyAutostart, opKeyHarnessPrerelease, opKeyHarnessVersion:
		return true
	}
	return strings.HasPrefix(key, "plugin:")
}

// mergeOps 合并本地待上报队列与服务器记录：opId 去重 + 同 key LWW。
//
// 胜负规则（确定性、可单测）：
//  1. 同 key 有本地未上报记录 → 本地胜（它代表用户最新的意图，尚未进服务器，不能反被覆盖）；
//  2. 否则服务器记录中 seq 最大者胜（服务器接收时间是权威时间，客户端时钟不参与）。
func mergeOps(local []accountPendingOp, remote []accountOpRecord) map[string]accountSyncTarget {
	out := make(map[string]accountSyncTarget)
	seenOp := make(map[string]bool)

	for _, op := range remote {
		if !accountSyncKeySupported(op.Key) || seenOp[op.OpID] {
			continue
		}
		seenOp[op.OpID] = true
		prev, ok := out[op.Key]
		if ok && !prev.FromLocal && prev.Seq >= op.Seq {
			continue
		}
		if ok && prev.FromLocal {
			continue // 本地未上报优先，服务器记录不覆盖
		}
		out[op.Key] = accountSyncTarget{Key: op.Key, Value: op.Value, Seq: op.Seq}
	}

	for _, op := range local {
		if !accountSyncKeySupported(op.Key) || seenOp[op.OpID] {
			continue
		}
		seenOp[op.OpID] = true
		out[op.Key] = accountSyncTarget{Key: op.Key, Value: op.Value, Seq: 0, FromLocal: true}
	}
	return out
}

// accountLocalPluginValue 本机某个在线插件的当前状态（未安装返回 false）。
func accountLocalPluginValue(profile, name string) (pluginOpValue, bool) {
	for _, row := range buildPluginRows() {
		if row.Name != name {
			continue
		}
		if profile != "" && !profileContains(row.Profile, profile) {
			continue
		}
		if !isOnlinePluginSource(row.Source) {
			return pluginOpValue{}, false // 本地插件不参与同步
		}
		action := "update"
		if row.Version == "" {
			action = "install"
		}
		return pluginOpValue{Action: action, Spec: row.Spec, Source: row.Source, Version: row.Version}, true
	}
	return pluginOpValue{}, false
}

// profileContains 判断插件的 profile 声明是否包含目标 profile（多环境时顿号分隔；空 = 旧布局）。
func profileContains(declared, want string) bool {
	if strings.TrimSpace(declared) == "" {
		return want == ""
	}
	for _, part := range strings.FieldsFunc(declared, func(r rune) bool { return r == '、' || r == ',' || r == ' ' }) {
		if part == want {
			return true
		}
	}
	return false
}

// accountLocalPluginValueFn 本机插件状态查询（测试可替换，避免依赖真实 dshHome 目录）。
var accountLocalPluginValueFn = accountLocalPluginValue

// accountKeyTargetSatisfied 本机当前状态是否已满足目标值（相同则不必进入待生效集合）。
func accountKeyTargetSatisfied(key string, value json.RawMessage) bool {
	switch {
	case key == opKeyAutostart:
		var want bool
		if json.Unmarshal(value, &want) != nil {
			return true // 值坏了：当作已满足，避免用非法值改系统设置
		}
		return isAutostartEnabled() == want
	case key == opKeyHarnessPrerelease:
		var want bool
		if json.Unmarshal(value, &want) != nil {
			return true
		}
		return harnessPrereleaseOverride == want
	case key == opKeyHarnessVersion:
		var want string
		if json.Unmarshal(value, &want) != nil {
			return true
		}
		cur := strings.TrimPrefix(installedHarnessVersion(), "v")
		return strings.TrimPrefix(strings.TrimSpace(want), "v") == cur
	case strings.HasPrefix(key, "plugin:"):
		profile, name, ok := splitPluginKey(key)
		if !ok {
			return true
		}
		var want pluginOpValue
		if json.Unmarshal(value, &want) != nil {
			return true
		}
		cur, installed := accountLocalPluginValueFn(profile, name)
		if want.Action == "remove" {
			return !installed
		}
		if !installed {
			return false
		}
		if want.Version != "" && cur.Version != want.Version {
			return false
		}
		if want.Spec != "" && cur.Spec != want.Spec {
			return false
		}
		return true
	}
	return true
}

// splitPluginKey 解析 `plugin:<profile>:<name>`。
func splitPluginKey(key string) (profile, name string, ok bool) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 || parts[0] != "plugin" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// ---------- 应用动作（可注入，测试不触碰真实系统状态） ----------

var (
	applyAutostartFn = func(on bool) error {
		setAutostartOn(on)
		return nil
	}
	applyPrereleaseFn = func(on bool) error {
		harnessPrereleaseOverride = on
		saveCurrentConfig()
		return nil
	}
	// applyHarnessVersionFn 安装指定 Harness 版本（复用重置链路：停服 → 安装 → 校验 → 重启）。
	applyHarnessVersionFn = func(ver string) error {
		if strings.TrimSpace(ver) == "" {
			return errors.New("目标版本为空")
		}
		prev := installedHarnessVersion()
		runHarnessReset(false, false, ver)
		if strings.TrimPrefix(installedHarnessVersion(), "v") == strings.TrimPrefix(prev, "v") {
			return fmt.Errorf("安装 %s 未生效（当前仍为 %s）", ver, prev)
		}
		return nil
	}
	// applyPluginOpFn 应用一条插件变更（安装/更新/卸载）。
	applyPluginOpFn = applyPluginOp
)

// applyKeyTarget 把某个 key 的目标值应用到本机。
func applyKeyTarget(key string, value json.RawMessage) error {
	switch {
	case key == opKeyAutostart:
		var on bool
		if err := json.Unmarshal(value, &on); err != nil {
			return err
		}
		return applyAutostartFn(on)
	case key == opKeyHarnessPrerelease:
		var on bool
		if err := json.Unmarshal(value, &on); err != nil {
			return err
		}
		return applyPrereleaseFn(on)
	case key == opKeyHarnessVersion:
		var ver string
		if err := json.Unmarshal(value, &ver); err != nil {
			return err
		}
		return applyHarnessVersionFn(strings.TrimPrefix(strings.TrimSpace(ver), "v"))
	case strings.HasPrefix(key, "plugin:"):
		_, name, ok := splitPluginKey(key)
		if !ok {
			return fmt.Errorf("插件 key 不合法：%s", key)
		}
		var v pluginOpValue
		if err := json.Unmarshal(value, &v); err != nil {
			return err
		}
		return applyPluginOpFn(name, v)
	}
	return nil
}

// ---------- 同步：拉取 + 合并 ----------

// accountSetPendingApply 写入待生效集合（按 key 合并，保留最新）。
func accountSetPendingApply(targets map[string]accountSyncTarget) {
	accountMu.Lock()
	defer accountMu.Unlock()
	byKey := make(map[string]accountPendingOp, len(targets))
	for key, t := range targets {
		byKey[key] = accountPendingOp{
			OpID:      fmt.Sprintf("pending-%s-%d", t.Key, t.Seq),
			Key:       t.Key,
			Value:     t.Value,
			CreatedAt: time.Now().Unix(),
			Seq:       t.Seq,
		}
	}
	ops := make([]accountPendingOp, 0, len(byKey))
	for _, op := range byKey {
		ops = append(ops, op)
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Key < ops[j].Key })
	accountCur.PendingRemote = ops
	accountCur.PendingApply = len(ops) > 0
	_ = saveAccountState(accountCur)
}

// accountPendingKeys 当前待生效的 key（升序）。
func accountPendingKeys() []string {
	accountMu.Lock()
	defer accountMu.Unlock()
	out := make([]string, 0, len(accountCur.PendingRemote))
	for _, op := range accountCur.PendingRemote {
		out = append(out, op.Key)
	}
	sort.Strings(out)
	return out
}

// accountSyncPull 增量拉取服务器记录并与本地待上报队列合并，返回「目标值有差异」的 key 集合。
func accountSyncPull(ctx context.Context, client *accountClient) (map[string]accountSyncTarget, int, error) {
	accountMu.Lock()
	token, cursor, local := accountCur.Token, accountCur.Cursor, append([]accountPendingOp(nil), accountCur.PendingOps...)
	accountMu.Unlock()

	if token == "" {
		return nil, 0, &accountError{Code: accErrUnauthorized, Message: "未登录"}
	}

	seen := make(map[string]bool)
	var remote []accountOpRecord
	newCursor := cursor
	maxSeq := cursor
	for {
		page, err := client.OpsSince(ctx, token, newCursor, accountOpsPageSize)
		if err != nil {
			return nil, 0, err
		}
		for _, op := range page.Ops {
			if seen[op.OpID] {
				continue
			}
			seen[op.OpID] = true
			remote = append(remote, op)
			if op.Seq > maxSeq {
				maxSeq = op.Seq
			}
		}
		newCursor = page.Cursor
		if !page.HasMore {
			break
		}
	}

	merged := mergeOps(local, remote)
	pending := make(map[string]accountSyncTarget)
	for key, t := range merged {
		if !t.FromLocal && accountKeyTargetSatisfied(key, t.Value) {
			continue // 服务器值与本机当前值一致：不必提示重启
		}
		if t.FromLocal {
			continue // 本地未上报的改动不需要「应用」，只等上报
		}
		pending[key] = t
	}

	// 游标只推进到「待生效记录」之前：这些记录还没应用，下次同步必须能重新拉到
	// （否则一旦推进过头，用户没点「重启生效」的改动会永久丢失）。
	newCursor = maxSeq
	for _, t := range pending {
		if t.Seq > 0 && newCursor >= t.Seq {
			newCursor = t.Seq - 1
		}
	}
	if newCursor < cursor {
		newCursor = cursor
	}

	accountMu.Lock()
	accountCur.Cursor = newCursor
	accountMu.Unlock()
	return pending, len(remote), nil
}

// accountSyncNow 一次同步检查：先推本地变更，再拉服务器记录，把差异放入待生效集合（**不应用**）。
func accountSyncNow(ctx context.Context, client *accountClient) (accountSyncResult, error) {
	var res accountSyncResult

	// 1) 先推：本地待上报的改动先进服务器，合并时才能正确处理并发。
	uploaded, err := accountFlushOps(ctx, client)
	res.Uploaded = uploaded
	if err != nil && accountErrorCode(err) == accErrUnauthorized {
		return res, err
	}

	// 2) 首次同步且服务器为空 → 上报本地基线（决策②），此后一律 LWW。
	accountMu.Lock()
	baseline := !accountCur.BaselineDone && accountCur.Cursor == 0
	accountMu.Unlock()
	if baseline {
		empty, berr := accountServerEmpty(ctx, client)
		if berr != nil {
			return res, berr
		}
		if empty {
			n, err := accountReportBaseline(ctx, client)
			if err != nil {
				return res, err
			}
			res.Baseline = true
			res.Uploaded += n
		}
		accountMu.Lock()
		accountCur.BaselineDone = true
		accountCur.LastSyncedAt = time.Now().Unix()
		_ = saveAccountState(accountCur)
		accountMu.Unlock()
		return res, nil
	}

	// 3) 拉取 + 合并 → 待生效集合（不应用）
	pending, pulled, err := accountSyncPull(ctx, client)
	if err != nil {
		accountSetSyncError(accountErrorText(err))
		return res, err
	}
	accountSetPendingApply(pending)
	res.Pulled = pulled
	res.Pending = accountPendingKeys()

	accountMu.Lock()
	accountCur.BaselineDone = true
	accountCur.LastSyncedAt = time.Now().Unix()
	_ = saveAccountState(accountCur)
	accountMu.Unlock()
	return res, nil
}

// accountServerEmpty 服务器上该账号是否没有任何同步范围内的记录（用于首次同步的基线判定）。
func accountServerEmpty(ctx context.Context, client *accountClient) (bool, error) {
	accountMu.Lock()
	token := accountCur.Token
	accountMu.Unlock()
	page, err := client.OpsSince(ctx, token, 0, accountOpsPageSize)
	if err != nil {
		return false, err
	}
	for _, op := range page.Ops {
		if accountSyncKeySupported(op.Key) {
			return false, nil
		}
	}
	return !page.HasMore, nil
}

// collectBaselineOps 采集本机当前状态作为基线操作记录（服务器为空时的首次上报）。
func collectBaselineOps() []accountOp {
	var ops []accountOp

	appendOp := func(key string, v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			return
		}
		ops = append(ops, accountOp{OpID: newOpID(), Key: key, Value: raw})
	}
	appendOp(opKeyAutostart, isAutostartEnabled())
	appendOp(opKeyHarnessPrerelease, harnessPrereleaseOverride)
	if ver := strings.TrimPrefix(installedHarnessVersion(), "v"); ver != "" {
		appendOp(opKeyHarnessVersion, ver)
	}
	for _, row := range buildPluginRows() {
		if !isOnlinePluginSource(row.Source) || !profileContains(row.Profile, accountPluginProfile) {
			continue
		}
		action := "update"
		if row.Version == "" {
			action = "install"
		}
		appendOp(accountPluginKey(row.Name), pluginOpValue{Action: action, Spec: row.Spec, Source: row.Source, Version: row.Version})
	}
	return ops
}

// accountReportBaseline 上报本机基线（服务器为空时的首次同步）。
func accountReportBaseline(ctx context.Context, client *accountClient) (int, error) {
	accountMu.Lock()
	token := accountCur.Token
	accountMu.Unlock()
	ops := collectBaselineOps()
	if len(ops) == 0 {
		return 0, nil
	}
	sent := 0
	for start := 0; start < len(ops); start += accountOpsMaxBatch {
		end := start + accountOpsMaxBatch
		if end > len(ops) {
			end = len(ops)
		}
		res, err := client.ReportOps(ctx, token, ops[start:end])
		if err != nil {
			return sent, err
		}
		sent += end - start
		accountMu.Lock()
		if res.Cursor > accountCur.Cursor {
			accountCur.Cursor = res.Cursor
		}
		accountMu.Unlock()
	}
	return sent, nil
}

// ---------- 应用：点「重启生效」后合并并落地 ----------

// accountApplyPending 把待生效集合应用到本机（只处理与当前值不同的 key）。
//
// 顺序：设置项 → Harness 版本 → 插件（升序）。任何一项失败都会中止并保留剩余待生效集合，
// 由用户重试；已成功应用的部分会从集合里移除。
func accountApplyPending(ctx context.Context, client *accountClient) (accountSyncResult, error) {
	var res accountSyncResult

	// 1) 先把本地改动推上去、再拉一次最新（时间点合并：应用前对齐到服务器最新状态）
	if _, err := accountFlushOps(ctx, client); err != nil && accountErrorCode(err) == accErrUnauthorized {
		return res, err
	}
	pending, pulled, err := accountSyncPull(ctx, client)
	if err == nil {
		accountSetPendingApply(pending)
	}
	res.Pulled = pulled

	// 2) 逐 key 应用（设置项优先，插件最后）
	accountMu.Lock()
	targets := append([]accountPendingOp(nil), accountCur.PendingRemote...)
	accountMu.Unlock()
	if len(targets) == 0 {
		accountMu.Lock()
		accountCur.PendingApply = false
		_ = saveAccountState(accountCur)
		accountMu.Unlock()
		res.Pending = accountPendingKeys()
		return res, nil
	}
	sort.Slice(targets, func(i, j int) bool { return accountApplyOrder(targets[i].Key) < accountApplyOrder(targets[j].Key) })

	remaining := make([]accountPendingOp, 0, len(targets))
	applied := 0
	for i, op := range targets {
		if accountKeyTargetSatisfied(op.Key, op.Value) {
			res.Unchanged += 1
			continue
		}
		if err := applyKeyTarget(op.Key, op.Value); err != nil {
			remaining = append(remaining, targets[i:]...)
			accountSetPendingApplyRemaining(remaining)
			accountSetSyncError(err.Error())
			res.Applied = applied
			return res, err
		}
		applied += 1
	}
	res.Applied = applied
	accountSetPendingApplyRemaining(nil)

	accountMu.Lock()
	accountCur.LastSyncedAt = time.Now().Unix()
	accountCur.BaselineDone = true
	accountSyncErr = ""
	_ = saveAccountState(accountCur)
	accountMu.Unlock()
	res.Pending = accountPendingKeys()
	return res, nil
}

// accountApplyOrder 应用顺序：设置项（0-2）→ Harness 版本（3）→ 插件（4）。
func accountApplyOrder(key string) int {
	switch key {
	case opKeyAutostart:
		return 0
	case opKeyHarnessPrerelease:
		return 1
	case opKeyHarnessVersion:
		return 2
	}
	return 3
}

// accountSetPendingApplyRemaining 用给定集合替换待生效集合。
func accountSetPendingApplyRemaining(ops []accountPendingOp) {
	accountMu.Lock()
	defer accountMu.Unlock()
	accountCur.PendingRemote = ops
	accountCur.PendingApply = len(ops) > 0
	_ = saveAccountState(accountCur)
}

// ---------- Wails 绑定 ----------

// AccountSyncNow 立即做一次同步检查（登录后与后台定期都会调用；不应用任何改动）。
func (a *App) AccountSyncNow() (AccountStatusInfo, error) {
	client := newAccountClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	accountMu.Lock()
	accountSyncing = true
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accountSyncing = false
		accountMu.Unlock()
	}()

	res, err := accountSyncNow(ctx, client)
	if err != nil {
		accountSetSyncError(accountErrorText(err))
		return accountSnapshot(), errors.New(accountErrorText(err))
	}
	accountClearSyncError()
	logUI("同步检查完成", fmt.Sprintf("上报 %d 项，拉到 %d 条，待生效 %d 项", res.Uploaded, res.Pulled, len(res.Pending)))
	return accountSnapshot(), nil
}

// AccountApplyPending 点「重启生效」：合并本地与服务器的操作记录并落地到本机。
func (a *App) AccountApplyPending() (AccountStatusInfo, error) {
	client := newAccountClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	accountMu.Lock()
	accountSyncing = true
	accountMu.Unlock()
	defer func() {
		accountMu.Lock()
		accountSyncing = false
		accountMu.Unlock()
	}()

	res, err := accountApplyPending(ctx, client)
	if err != nil {
		return accountSnapshot(), errors.New(accountErrorText(err))
	}
	logUI("同步改动已生效", fmt.Sprintf("应用 %d 项，另有 %d 项本就一致", res.Applied, res.Unchanged))
	if res.Applied > 0 && appCtx != nil {
		wruntime.EventsEmit(appCtx, "plugins:changed", nil) // 关于页插件列表按新状态刷新
	}
	return accountSnapshot(), nil
}
