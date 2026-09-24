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
	"strconv"
	"strings"
	"time"
)

// accountSyncResult 一次同步检查的结果（状态页与测试断言用）。
type accountSyncResult struct {
	Uploaded  int      // 本次上报的本地变更条数
	Pulled    int      // 本次拉到并记入待生效的服务器记录条数
	Baseline  bool     // 是否执行了「服务器为空时首登上报本地基线」
	Pending   []string // 待生效的 key（升序）
	Applied   int      // 应用阶段真正执行的变更数（仅在 Apply 时非 0）
	Unchanged int      // 与服务端一致、无需应用的 key 数
	// Reenqueued 本机现状已偏离、被重新放回待生效集合的「已应用记录」项数
	// （手工删除 .dsh / harness 目录后仍能发现漂移的依据，见 accountReenqueueDriftedApplied）。
	Reenqueued int
	// PluginsReported 本次对账登记/补报的本地在线插件条数（服务器上没有记录、在托盘外升高了
	// 版本、或已删除后又装回来的插件，见 accountReconcileLocalPlugins）。登记后本次同步的上报
	// 阶段即送出，因此统计的是「本次登记」而非「本次送达」。
	PluginsReported int
	// Failed 应用阶段失败的 key → 原因。失败项保留在待生效集合，单项失败不中止整批。
	Failed map[string]string
	// Canceled 应用流程被用户取消（已应用的保留，剩余留待生效）。
	Canceled bool
}

// accountSyncTarget 合并后某个 key 的目标值。
type accountSyncTarget struct {
	Key       string
	Value     json.RawMessage
	Seq       int64 // 服务器序号；0 = 来自本地未上报队列
	FromLocal bool
	// UpdatedAt 服务器写入时间（Unix 秒；本地队列项为 0）：与 Seq 一起供本机现状重判使用
	// （见 accountKeyTargetSatisfiedAt）。
	UpdatedAt int64
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
		out[op.Key] = accountSyncTarget{Key: op.Key, Value: op.Value, Seq: op.Seq, UpdatedAt: op.UpdatedAt}
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

// accountLocalPluginInstallTimeForFn 本机插件安装时间查询（测试可替换，避免依赖真实 dshHome 目录）。
var accountLocalPluginInstallTimeForFn = accountLocalPluginInstallTimeFor

// accountKeyTargetSatisfied 本机当前状态是否已满足目标值（时间未知的兼容入口）。
//
// 等价于 accountKeyTargetSatisfiedAt(key, value, 0)：调用方拿不到服务器写入时间时使用，
// 行为与旧版一致（不按时间判定）；能拿到时间的路径必须传时间，见下。
func accountKeyTargetSatisfied(key string, value json.RawMessage) bool {
	return accountKeyTargetSatisfiedAt(key, value, 0)
}

// accountKeyTargetSatisfiedAt 本机当前状态是否已满足目标值（相同则不必进入待生效集合）。
//
// updatedAt 是这条目标记录在服务器上的写入时间（Unix 秒；0 = 未知），只有删除墓碑判定用到它。
//
// 目标值解析失败时**不**视为已满足：这些值来自服务器，无法应用时应让用户看到
// （进入待生效 → 应用时给出明确原因），而不是静默丢掉（2026-09-21 评估：harness
// 版本记录一旦形态异常就被当作「已满足」，表现为「版本根本没同步」且无任何提示）。
func accountKeyTargetSatisfiedAt(key string, value json.RawMessage, updatedAt int64) bool {
	switch {
	case key == opKeyAutostart:
		var want bool
		if json.Unmarshal(value, &want) != nil {
			accountLogBadTarget(key, value)
			return false // 值坏了：当作未满足，由应用阶段给出明确失败原因（不会真的改系统设置）
		}
		return isAutostartEnabled() == want
	case key == opKeyHarnessPrerelease:
		var want bool
		if json.Unmarshal(value, &want) != nil {
			accountLogBadTarget(key, value)
			return false
		}
		return harnessPrereleaseOverride == want
	case key == opKeyHarnessVersion:
		var want string
		if json.Unmarshal(value, &want) != nil {
			accountLogBadTarget(key, value)
			return false
		}
		return normalizeVersionText(want) == normalizeVersionText(installedHarnessVersion())
	case strings.HasPrefix(key, "plugin:"):
		profile, name, ok := splitPluginKey(key)
		if !ok {
			return true
		}
		var want pluginOpValue
		if json.Unmarshal(value, &want) != nil {
			accountLogBadTarget(key, value)
			return false
		}
		cur, installed := accountLocalPluginValueFn(profile, name)
		if want.Action == "remove" {
			if !installed {
				return true
			}
			// 本机装着，但装的时间**晚于**这条删除记录的写入时间 → 本机是「删掉后又装回来」，
			// 该墓碑已被这次安装盖过：视为已满足，不再进待生效集合。否则它会每轮被拉取/漂移检查
			// 重新入队，而对账又会跳过所有待生效 key，install 就永远补报不上去（2026-09-24 现场：
			// dsh-cost-meter 重装后账号记录始终停在 remove 墓碑、插件补报恒为 0）。
			// 判据与对账 accountReconcileLocalPlugins 的「本机安装更新 → 补报 install」同口径：
			// 时间以服务器 updatedAt 为权威；时间未知（旧记录 / 本地队列项）时保守视为未满足。
			if updatedAt > 0 && accountLocalPluginInstallTimeForFn(profile, name) > updatedAt {
				return true
			}
			return false
		}
		if !installed {
			return false
		}
		// 本机版本读不到（包在但 package.json 缺失/无 version 字段）：**不**视为差异。
		// 判读失败不等于版本不符——否则「已应用 且 读不到版本」会在每次同步里反复入队重装
		// （2026-09-21 反复重装问题的另一形态）。
		if cur.Version == "" {
			return want.Spec == "" || normalizeSpecText(cur.Spec) == normalizeSpecText(want.Spec)
		}
		if want.Version != "" && !versionTargetSatisfied(cur.Version, want.Version) {
			return false
		}
		if want.Spec != "" && !pluginSpecSatisfied(cur, want.Spec) {
			return false
		}
		return true
	}
	return true
}

// accountLogBadTarget 记录目标值解析失败（不静默：便于排障与用户反馈）。
func accountLogBadTarget(key string, value json.RawMessage) {
	logWarn("account", "同步目标值无法解析，按未满足处理 key=%s value=%.120s", key, string(value))
}

// normalizeVersionText 归一化版本文本：去空白与前导 v / dsh- / dsh-v 前缀（循环剥离）。
func normalizeVersionText(s string) string {
	s = strings.TrimSpace(s)
	for i := 0; i < 3; i++ {
		switch {
		case strings.HasPrefix(s, "dsh-v"), strings.HasPrefix(s, "dsh-"):
			s = strings.TrimPrefix(strings.TrimPrefix(s, "dsh-"), "v")
		case strings.HasPrefix(s, "v"), strings.HasPrefix(s, "V"):
			s = s[1:]
		default:
			return s
		}
	}
	return s
}

// normalizeSpecText 归一化依赖声明文本：去空白、大小写不敏感、github:owner/repo 与
// https://github.com/owner/repo(.git) 视为同一来源（跨机声明写法可能不同）。
func normalizeSpecText(s string) string {
	s = strings.TrimSpace(s)
	low := strings.ToLower(s)
	for _, p := range []string{"git+https://github.com/", "https://github.com/", "git://github.com/", "github:"} {
		if strings.HasPrefix(low, p) {
			s = s[len(p):]
			break
		}
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), ".git"))
}

// pluginSpecSatisfied 本机插件是否满足服务器目标的 spec 声明：声明一致（归一化后）即满足；
// 否则只要已装版本满足该范围也算满足（跨机可能出现 ^1.7.30 与 1.7.30 的写法差异）。
func pluginSpecSatisfied(cur pluginOpValue, wantSpec string) bool {
	if normalizeSpecText(cur.Spec) == normalizeSpecText(wantSpec) {
		return true
	}
	return cur.Version != "" && versionTargetSatisfied(cur.Version, wantSpec)
}

// versionTargetSatisfied 已安装版本是否满足目标（精确版本 / ^ ~ 范围 / 比较符 / x 通配 / latest）。
// 这是「同一记录被反复重装」的判定闸口：服务器记录的 Version 字段可能是范围值（如 ^1.7.30），
// 与已装版本（1.7.30）做字符串比较必然不等（2026-09-21 现场问题）。
func versionTargetSatisfied(installed, target string) bool {
	ver := normalizeVersionText(installed)
	tgt := strings.TrimSpace(target)
	if ver == "" {
		return false
	}
	if tgt == "" {
		return true
	}
	for _, part := range strings.Split(tgt, "||") {
		if versionRangeMatches(ver, strings.TrimSpace(part)) {
			return true
		}
	}
	return false
}

// versionRangeMatches 单个范围子句是否满足（|| 由调用方拆分）。
func versionRangeMatches(ver, rng string) bool {
	r := normalizeVersionText(rng)
	if r == "" || r == "*" || r == "x" || strings.EqualFold(r, "latest") {
		return true
	}
	switch {
	case strings.HasPrefix(r, "^"):
		return caretSatisfied(ver, strings.TrimPrefix(r, "^"))
	case strings.HasPrefix(r, "~"):
		return tildeSatisfied(ver, strings.TrimPrefix(r, "~"))
	case strings.HasPrefix(r, ">="):
		return compareVersions(ver, strings.TrimSpace(r[2:])) >= 0
	case strings.HasPrefix(r, "<="):
		return compareVersions(ver, strings.TrimSpace(r[2:])) <= 0
	case strings.HasPrefix(r, ">"):
		return compareVersions(ver, strings.TrimSpace(r[1:])) > 0
	case strings.HasPrefix(r, "<"):
		return compareVersions(ver, strings.TrimSpace(r[1:])) < 0
	case strings.HasPrefix(r, "="):
		return compareVersions(ver, strings.TrimSpace(r[1:])) == 0
	}
	if strings.ContainsAny(r, "xX*") {
		return strings.HasPrefix(ver, wildcardPrefix(r))
	}
	return compareVersions(ver, r) == 0
}

// caretSatisfied ^X.Y.Z：>= X.Y.Z 且 < 下一主版本（0.x.y 时按 npm 语义 < 下一非零段）。
func caretSatisfied(ver, base string) bool {
	base = strings.TrimSpace(base)
	if compareVersions(ver, base) < 0 {
		return false
	}
	nums := numericPartsOf(base)
	switch {
	case len(nums) == 0:
		return true
	case nums[0] > 0:
		return compareVersions(ver, fmt.Sprintf("%d.0.0", nums[0]+1)) < 0
	case len(nums) == 1:
		return compareVersions(ver, "1.0.0") < 0
	case nums[1] > 0:
		return compareVersions(ver, fmt.Sprintf("0.%d.0", nums[1]+1)) < 0
	case len(nums) == 2:
		return compareVersions(ver, "0.1.0") < 0
	default:
		return compareVersions(ver, fmt.Sprintf("0.0.%d", nums[2]+1)) < 0
	}
}

// tildeSatisfied ~X.Y.Z：>= X.Y.Z 且 < X.(Y+1).0（只写主版本时 < (X+1).0.0），npm 语义。
func tildeSatisfied(ver, base string) bool {
	base = strings.TrimSpace(base)
	if compareVersions(ver, base) < 0 {
		return false
	}
	nums := numericPartsOf(base)
	switch {
	case len(nums) == 0:
		return true
	case len(nums) == 1:
		return compareVersions(ver, fmt.Sprintf("%d.0.0", nums[0]+1)) < 0
	default:
		return compareVersions(ver, fmt.Sprintf("%d.%d.0", nums[0], nums[1]+1)) < 0
	}
}

// wildcardPrefix 把 "1.7.x" 之类的通配范围转成前缀 "1.7."。
func wildcardPrefix(r string) string {
	parts := strings.Split(r, ".")
	var out []string
	for _, p := range parts {
		if p == "" || p == "x" || p == "X" || p == "*" {
			break
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, ".") + "."
}

// numericPartsOf 取版本文本的数值段（非数值段截断）。
func numericPartsOf(v string) []int {
	num, _ := splitVersionParts(v)
	var out []int
	for _, p := range num {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
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
	// applyHarnessVersionFn 安装指定 Harness 版本（复用重置链路：停服 → 全新安装 → 校验 → 重启）。
	//
	// 与常规页「重置服务」共用同一实现，但走**静默通道**（popup=nil）：不弹原生对话框
	// （应用流程自带进度视图），失败以错误返回给应用循环——单项失败不阻断同批其它改动。
	applyHarnessVersionFn = func(ver string) error {
		ver = normalizeVersionText(ver)
		if ver == "" {
			return errors.New(T("目标版本为空"))
		}
		if isSourceHarnessDir() {
			// 源码 checkout 形态不支持自动清空重装（重置链路同口径）：明确报错并保留待生效。
			return errors.New(T("本机 Harness 为源码 checkout 形态，暂不支持自动同步版本（可在常规页手动处理）"))
		}
		out := runHarnessResetFlow(false, false, ver, nil)
		if !out.OK {
			return errors.New(out.Err)
		}
		if cur := normalizeVersionText(installedHarnessVersion()); cur != ver {
			return fmt.Errorf(T("安装 %s 未生效（当前仍为 %s）"), ver, orDash(cur))
		}
		// harness 换版后各 profile 的插件树仍按旧 harness 解析（pnpm 的 .modules.yaml /
		// junction / 虚拟商店都是旧一代），与「更新 Harness」流程的第 3.5 步同口径对齐——
		// 不对齐会出现「插件装上了但 harness 加载不了」（2026-09-21 现场 DSHPREFLIGHT 实证）。
		if dir, ok := webProfileDir(); ok {
			if rerr := reconcileProfileDeps(dir); rerr != nil {
				logWarn("account", "harness 换版后 profile 依赖对齐失败（已由启动校验兜底）: %v", rerr)
			}
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
		return applyHarnessVersionFn(normalizeVersionText(ver))
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
			UpdatedAt: t.UpdatedAt,
		}
	}
	ops := make([]accountPendingOp, 0, len(byKey))
	for _, op := range byKey {
		ops = append(ops, op)
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Key < ops[j].Key })
	accountCur.PendingRemote = ops
	accountCur.PendingApply = len(ops) > 0
	accountClampCursorLocked() // 兜底：待生效集合整体替换后，游标必须仍在它们之前
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
	accountMu.Lock()
	applied := make(map[string]int64, len(accountCur.AppliedSeqs))
	for k, v := range accountCur.AppliedSeqs {
		applied[k] = v
	}
	accountMu.Unlock()
	// remember 本轮确认「本机现状已满足」的记录：写入 AppliedVals，供下次启动/同步离线重判
	// 漂移（见 accountReenqueueDriftedApplied）。
	remember := make(map[string]appliedRecord)
	pending := make(map[string]accountSyncTarget)
	for key, t := range merged {
		if t.FromLocal {
			continue // 本地未上报的改动不需要「应用」，只等上报
		}
		if t.Seq > 0 && applied[key] >= t.Seq {
			// 已应用过的记录：只有本机**当前状态**仍满足目标才跳过。手工删除 .dsh /
			// harness 目录后本机插件与版本被重置，此时若只看序号就会「显示已同步、
			// 实际什么都没恢复」（2026-09-22 现场问题①）。
			if accountKeyTargetSatisfiedAt(key, t.Value, t.UpdatedAt) {
				remember[key] = appliedRecord{Seq: t.Seq, Value: t.Value, UpdatedAt: t.UpdatedAt}
				continue
			}
			logWarn("account", "已应用记录与本机状态不一致，重新入队 key=%s seq=%d", key, t.Seq)
			pending[key] = t
			continue
		}
		if accountKeyTargetSatisfiedAt(key, t.Value, t.UpdatedAt) {
			remember[key] = appliedRecord{Seq: t.Seq, Value: t.Value, UpdatedAt: t.UpdatedAt}
			continue // 服务器值与本机当前值一致：不必提示重启
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
	for k, rec := range remember {
		accountRememberAppliedLocked(k, rec.Value, rec.Seq, rec.UpdatedAt)
	}
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
			// 服务器为空：以本机现状上报基线（决策②），本轮无需拉取。
			// 基线的同步范围含 Harness 版本（collectBaselineOps）：初始化即一并上报，
			// 后续机器才能在服务器上看到本机的「最后选用版本」。
			n, err := accountReportBaseline(ctx, client)
			if err != nil {
				return res, err
			}
			res.Baseline = true
			res.Uploaded += n
			accountMu.Lock()
			accountCur.BaselineDone = true
			accountCur.LastSyncedAt = time.Now().Unix()
			// 版本已随基线发出则记下（避免重复补报）；基线时版本尚且未知（harness 仍在
			// 安装）则保持空，由 accountReconcileHarnessVersion 在后续同步里补报。
			if v := normalizeVersionText(installedHarnessVersion()); v != "" {
				accountCur.LastReportedHarnessVersion = v
			}
			_ = saveAccountState(accountCur)
			accountMu.Unlock()
			return res, nil
		}
		// 服务器已有记录：**继续走拉取分支**——首次同步同样要把服务器记录拉到本地
		// （只保存不生效，等用户点「重启生效」），这是需求②的核心路径。
	}

	// 3) 拉取 + 合并 → 待生效集合（不应用）
	pending, pulled, err := accountSyncPull(ctx, client)
	if err != nil {
		accountSetSyncError(accountErrorText(err))
		return res, err
	}
	accountSetPendingApply(pending)
	// 已应用记录按本机现状兜底重判：拉取只看得到游标之后的记录，用户手动删除 .dsh /
	// harness 目录时这些记录早已在游标之前（2026-09-22 现场问题①）。
	res.Reenqueued = accountReenqueueDriftedApplied()
	res.Pulled = pulled
	res.Pending = accountPendingKeys()

	accountMu.Lock()
	accountCur.BaselineDone = true
	accountCur.LastSyncedAt = time.Now().Unix()
	_ = saveAccountState(accountCur)
	accountMu.Unlock()

	// 4) 对账本机 Harness 版本与本地在线插件：服务器上还没有该记录（或版本在应用外变过）时补报，
	//    保证「初始化时一并上报」不因基线时刻版本未知而落空（插件对账另见 accountReconcileLocalPlugins：
	//    用 npm / pnpm 直接装的插件不会经过托盘，不补报就永远进不了账号记录）。
	accountReconcileHarnessVersion(ctx, client)
	res.PluginsReported = accountReconcileLocalPlugins(ctx, client)
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
		accountClampCursorLocked() // 同上：上报推进的游标不得越过仍未生效的记录
		accountMu.Unlock()
	}
	return sent, nil
}

// ---------- 应用：点「重启生效」后合并并落地 ----------

// applyRetryDelays 应用失败后的重试等待（首项为首次尝试）。仅「网络瞬断」类错误重试：
// 非网络原因（版本不存在、格式非法、形态不支持）重试也不会成功，直接如实上报。
var applyRetryDelays = []time.Duration{0, 5 * time.Second}

// accountApplyPending 把待生效集合应用到本机（只处理与当前值不同的 key）。
//
// 顺序：设置项 → Harness 版本 → 插件（升序）。逐 key 隔离：任何一项失败都**不中止整批**
// ——失败项记入 res.Failed 并保留在待生效集合（下次同步/再点一次续做），其余项照常应用。
// 应用成功的项立即从集合移除并记录已应用序号（进程中途退出也不会留下「已生效」假象）。
// notify 推进度文案（可为 nil）。
func accountApplyPending(ctx context.Context, client *accountClient, notify func(text string, pct float64)) (accountSyncResult, error) {
	var res accountSyncResult
	if notify == nil {
		notify = func(string, float64) {}
	}

	// 1) 先把本地改动推上去、再拉一次最新（时间点合并：应用前对齐到服务器最新状态）。
	//    网络不可用不阻断应用：按本地已保存的待生效集合继续（离线可用）。
	if _, err := accountFlushOps(ctx, client); err != nil {
		if accountErrorCode(err) == accErrUnauthorized {
			return res, err
		}
		logWarn("account", "应用前上报失败（继续应用本地待生效集合）: %v", err)
	}
	if pending, pulled, err := accountSyncPull(ctx, client); err == nil {
		accountSetPendingApply(pending)
		res.Pulled = pulled
	} else if accountErrorCode(err) == accErrUnauthorized {
		return res, err
	} else {
		logWarn("account", "应用前拉取失败（按本地待生效集合继续）: %v", err)
	}

	// 2) 逐 key 应用（设置项优先，插件最后）
	accountMu.Lock()
	targets := append([]accountPendingOp(nil), accountCur.PendingRemote...)
	accountMu.Unlock()
	if len(targets) == 0 {
		accountSetPendingApplyRemaining(nil) // 顺带把 PendingApply 复位（持锁写盘）
		accountClearApplyError()
		res.Pending = accountPendingKeys()
		return res, nil
	}
	sort.Slice(targets, func(i, j int) bool { return accountApplyOrder(targets[i].Key) < accountApplyOrder(targets[j].Key) })

	for i, op := range targets {
		if ctx.Err() != nil {
			res.Canceled = true
			break
		}
		pct := float64(i) / float64(len(targets))
		if accountKeyTargetSatisfiedAt(op.Key, op.Value, op.UpdatedAt) {
			res.Unchanged += 1
			accountMarkApplied(op.Key, op.Value, op.Seq, op.UpdatedAt)
			continue
		}
		notify(fmt.Sprintf(T("正在应用同步改动（%d/%d）：%s"), i+1, len(targets), accountApplyLabel(op.Key)), pct)
		if err := applyKeyTargetWithRetry(ctx, op.Key, op.Value, notify, pct); err != nil {
			if res.Failed == nil {
				res.Failed = map[string]string{}
			}
			res.Failed[op.Key] = err.Error()
			logWarn("account", "同步改动应用失败 key=%s: %v", op.Key, err)
			continue
		}
		res.Applied += 1
		accountMarkApplied(op.Key, op.Value, op.Seq, op.UpdatedAt)
	}

	// 3) 收尾：失败/取消的说明写入应用错误（与同步错误分离，直到下次应用成功才清除）
	switch {
	case len(res.Failed) > 0:
		accountSetApplyError(fmt.Sprintf(T("有 %d 项同步改动应用失败：%s"), len(res.Failed), strings.Join(sortedFailureLabels(res.Failed), "、")))
	case res.Canceled:
		accountSetApplyError(T("应用已取消，剩余改动留待生效"))
	default:
		accountClearApplyError()
	}
	// 失败/取消的项仍留在待生效集合：游标重新钳回它们之前（不变量）。这样用户再点「立即同步」
	// 时这些记录还能被拉到、「重启生效」按钮会再次出现（2026-09-22 现场问题②）。
	accountClampCursor()
	res.Pending = accountPendingKeys()
	return res, nil
}

// sortedFailureLabels 失败 key 的界面名（升序，提示文案稳定可读）。
func sortedFailureLabels(failed map[string]string) []string {
	out := make([]string, 0, len(failed))
	for k := range failed {
		out = append(out, accountApplyLabel(k))
	}
	sort.Strings(out)
	return out
}

// accountApplyLabel 待生效/失败项的界面名（进度与提示文案用）。
func accountApplyLabel(key string) string {
	switch key {
	case opKeyAutostart:
		return T("开机自启动")
	case opKeyHarnessPrerelease:
		return T("预发布通道")
	case opKeyHarnessVersion:
		return T("Harness 版本")
	}
	if _, name, ok := splitPluginKey(key); ok {
		return T("插件") + " " + name
	}
	return key
}

// applyKeyTargetWithRetry 应用单个 key：网络瞬断类失败按 applyRetryDelays 退避重试。
// 每次尝试都是自洽事务（插件路径自带快照/回退），失败后重试不会留下半成品。
func applyKeyTargetWithRetry(ctx context.Context, key string, value json.RawMessage, notify func(string, float64), pct float64) error {
	var lastErr error
	for i, wait := range applyRetryDelays {
		if wait > 0 {
			notify(fmt.Sprintf(T("网络异常，%d 秒后重试（第 %d 次）…"), int(wait.Seconds()), i+1), pct)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		err := applyKeyTarget(key, value)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isTransientNetworkError(err) {
			return err
		}
		logWarn("account", "应用 %s 网络异常，准备重试: %v", key, err)
	}
	return lastErr
}

// isTransientNetworkError 失败是否属于「网络瞬断」类（值得退避重试）。
// 覆盖 pnpm / git / HTTP 各层报错形态（2026-09-21 日志实证：git ls-remote
// "Failed to connect to github.com port 443"、ECONNRESET、ETIMEDOUT）。
func isTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{
		"econnreset", "etimedout", "econnrefused", "econnaborted", "eai_again", "enotfound", "epipe",
		"socket hang up", "unexpected eof", "failed to connect", "could not connect",
		"connection reset", "connection refused", "network is unreachable",
		"no such host", "temporary failure in name resolution", "i/o timeout",
		"timeout", "timed out", "502", "503", "504", "exit status 128",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// accountMarkApplied 记录某 key 已应用到本机：从待生效集合移除该 key、记下已应用序号、目标值与
// 来源时间并落盘。逐项落盘（而非整批结束才写）：进程在应用中途退出时，已完成的项不会被当成「还没做」。
func accountMarkApplied(key string, value json.RawMessage, seq, updatedAt int64) {
	accountMu.Lock()
	defer accountMu.Unlock()
	out := make([]accountPendingOp, 0, len(accountCur.PendingRemote))
	for _, op := range accountCur.PendingRemote {
		if op.Key == key {
			continue
		}
		out = append(out, op)
	}
	accountCur.PendingRemote = out
	accountCur.PendingApply = len(out) > 0
	accountRememberAppliedLocked(key, value, seq, updatedAt)
	_ = saveAccountState(accountCur)
}

// accountRememberAppliedLocked 记下某 key 已应用的目标值、序号与来源时间（调用方须持有 accountMu）。
// 序号只作参考，目标值才是「本机被重置后能否重判漂移」的依据（见 accountState.AppliedVals）；
// updatedAt（服务器写入时间）供「本机更晚的安装是否已盖过删除墓碑」判定使用（0 = 未知）。
func accountRememberAppliedLocked(key string, value json.RawMessage, seq, updatedAt int64) {
	if seq <= 0 || len(value) == 0 {
		return
	}
	if accountCur.AppliedSeqs == nil {
		accountCur.AppliedSeqs = map[string]int64{}
	}
	accountCur.AppliedSeqs[key] = seq
	if accountCur.AppliedVals == nil {
		accountCur.AppliedVals = map[string]appliedRecord{}
	}
	accountCur.AppliedVals[key] = appliedRecord{Seq: seq, Value: append(json.RawMessage(nil), value...), UpdatedAt: updatedAt}
}

// accountClampCursorLocked 把游标钳回最早一条未生效记录之前（调用方须持有 accountMu）。
//
// 不变量：Cursor 必须始终小于 PendingRemote 中所有记录的 seq。任何推进游标的路径——拉取、
// 上报确认（accountFlushAck）、基线上报——都必须经过这里，否则游标一旦越过未生效记录，
// 增量拉取再也取不到它们，下一次 accountSetPendingApply 的整体替换会把它们静默丢弃
// （2026-09-22 现场问题②：插件同步失败后再点「立即同步」永远不再弹「重启生效」按钮）。
// 返回是否发生了回拨。
func accountClampCursorLocked() bool {
	minSeq := int64(0)
	for _, op := range accountCur.PendingRemote {
		if op.Seq > 0 && (minSeq == 0 || op.Seq < minSeq) {
			minSeq = op.Seq
		}
	}
	if minSeq == 0 || accountCur.Cursor < minSeq {
		return false
	}
	accountCur.Cursor = minSeq - 1
	return true
}

// accountClampCursor 持锁包装：回拨后立即落盘（不变量跨托盘重启同样成立）。
func accountClampCursor() {
	accountMu.Lock()
	defer accountMu.Unlock()
	if accountClampCursorLocked() {
		_ = saveAccountState(accountCur)
	}
}

// accountMigrateCursorInvariant 存量 account.json 的一次性自愈（2026-09-22 现场问题①②）。
//
// 旧版本有两处缺陷：①上报确认时无条件把游标推进到服务器流头部（可能越过尚未生效的记录）；
// ②已应用记录只存序号、不存目标值。存量状态因而可能是「游标已越过未生效记录」或
// 「只有 appliedSeqs、没有 appliedVals」——此时增量拉取永远取不到那些记录，也没有可离线
// 重判的目标值，表现为重启托盘后「显示已同步」、点多少次「立即同步」都不再出现「重启生效」
// （问题①：手工删除 .dsh / harness 目录；问题②：插件同步失败）。
//
// 被越过的记录无法从本地还原（目标值可能根本没存下来），因此一次性把游标回拨到 0 做全量重拉：
// 本机现状仍满足的记录重新记入 appliedVals（顺带完成老格式补值），不再满足的记录进入待生效
// 集合，由用户点「重启生效」恢复。标记随 account.json 持久化，迁移只做一次。
func accountMigrateCursorInvariant() {
	accountMu.Lock()
	defer accountMu.Unlock()
	if accountCur.SyncInvariantOK {
		return
	}
	accountCur.SyncInvariantOK = true
	old := accountCur.Cursor
	if old > 0 {
		accountCur.Cursor = 0
	}
	_ = saveAccountState(accountCur)
	if old > 0 {
		logWarn("account", "存量同步状态迁移：游标回拨 %d → 0，下次同步全量重拉并按本机现状重判", old)
	}
}

// accountReenqueueDriftedApplied 按本机现状重判「已应用记录」，把不再满足的项放回待生效集合。
//
// 为什么需要它：拉取阶段的重判（见 accountSyncPull）只作用于**本轮拉到**的记录，而服务器游标
// 早已推进到已应用记录之后——手工删除 .dsh / harness 目录后，增量拉取永远拿不到那些记录，
// 于是「已应用序号 + 本机现状」的漂移检查根本没机会执行，表现为重启托盘后显示「已同步」、
// 插件却一个都没有，点「立即同步」也没有任何变化（2026-09-22 现场问题①）。
//
// 本函数只读本机状态（离线可用），并把游标回拨到最早一条漂移记录之前：这样后续拉取会重新
// 取到这些记录，与「游标只推进到待生效记录之前」的不变量保持一致，也不会被下一轮
// accountSetPendingApply 的整体替换冲掉。返回本次重新入队的项数。
func accountReenqueueDriftedApplied() int {
	accountMu.Lock()
	defer accountMu.Unlock()
	if len(accountCur.AppliedVals) == 0 {
		return 0
	}
	inPending := make(map[string]bool, len(accountCur.PendingRemote))
	for _, op := range accountCur.PendingRemote {
		inPending[op.Key] = true
	}
	added, minSeq := 0, int64(0)
	for key, rec := range accountCur.AppliedVals {
		if inPending[key] || rec.Seq <= 0 || len(rec.Value) == 0 {
			continue
		}
		if accountKeyTargetSatisfiedAt(key, rec.Value, rec.UpdatedAt) {
			continue
		}
		accountCur.PendingRemote = append(accountCur.PendingRemote, accountPendingOp{
			OpID:      fmt.Sprintf("pending-%s-%d", key, rec.Seq),
			Key:       key,
			Value:     rec.Value,
			CreatedAt: time.Now().Unix(),
			Seq:       rec.Seq,
			UpdatedAt: rec.UpdatedAt,
		})
		if minSeq == 0 || rec.Seq < minSeq {
			minSeq = rec.Seq
		}
		added++
	}
	if added == 0 {
		return 0
	}
	sort.Slice(accountCur.PendingRemote, func(i, j int) bool { return accountCur.PendingRemote[i].Key < accountCur.PendingRemote[j].Key })
	accountCur.PendingApply = true
	if c := minSeq - 1; c >= 0 && c < accountCur.Cursor {
		accountCur.Cursor = c
	}
	_ = saveAccountState(accountCur)
	logWarn("account", "已应用记录与本机状态不一致：重新入队 %d 项，游标回拨至 %d", added, accountCur.Cursor)
	return added
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
	if accountApplyBusy() {
		// 应用流程进行中：两边都会改写待生效集合，直接拒绝比并发写坏状态更安全
		return accountSnapshot(), errors.New(T("正在应用同步改动，请稍候再试"))
	}
	if !beginAccountSync() {
		return accountSnapshot(), errors.New(T("正在同步，请稍候再试"))
	}
	defer endAccountSync()

	client := newAccountClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := accountSyncNow(ctx, client)
	// 手动同步成功同样算「本会话已检查」：用户在启动检查之前手点同步后，状态行应立刻
	// 按本次结果渲染，不能继续显示「正在检查同步…」。
	if err == nil {
		accountMarkStartupChecked()
	}
	if err != nil {
		accountSetSyncError(accountErrorText(err))
		return accountSnapshot(), errors.New(accountErrorText(err))
	}
	accountClearSyncError()
	logUI("同步检查完成", fmt.Sprintf("上报 %d 项，拉到 %d 条，待生效 %d 项，重入队 %d 项，插件补报 %d 项", res.Uploaded, res.Pulled, len(res.Pending), res.Reenqueued, res.PluginsReported))
	return accountSnapshot(), nil
}
