// account_sync_hooks.go：把用户可见的设置 / 在线插件变更登记为操作记录（op）并立即尝试上报。
//
// 冻结决策（见 Docs/dsh-systray-connect-sync-plan.md）：
//   - 只同步 web profile 的**在线**插件（npm/github/tarball 等）；file/local 来源的本地插件不上报；
//   - 安装位置由本机派生，绝对路径一律不上报（只报 spec/source/version）；
//   - 「仅改本地记录」类变更（待重指定、自动禁用记录清理）不算插件变动，不上报；
//   - 上报是异步的：登记入队后立刻尝试一次，失败留在队列里由后台重试（状态页显示「同步失败」）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// pluginOpValue 插件操作记录的 value 形状（与 dsh-connect docs/API.md §11 一致）。
type pluginOpValue struct {
	Action  string `json:"action"` // update | remove（install 由服务端/其它客户端产生）
	Spec    string `json:"spec"`
	Source  string `json:"source"`
	Version string `json:"version"`
}

// isOnlinePluginSource 是否为在线来源（本地插件不上报）。
func isOnlinePluginSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "", "file", "local":
		return false
	default:
		return true
	}
}

// accountSyncKick 触发一次异步上报：不阻塞调用方；失败只记录状态，由后台重试。
func accountSyncKick() {
	accountMu.Lock()
	loggedIn := accountCur.loggedIn(time.Now())
	accountMu.Unlock()
	if !loggedIn {
		return
	}
	accountSyncWG.Add(1) // 必须在 go 之前 Add：否则测试里 Wait 可能先于计数器自增返回
	go func() {
		defer accountSyncWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := accountFlushOps(ctx, newAccountClient("")); err != nil {
			accountSetSyncError(accountErrorText(err))
			return
		}
		accountClearSyncError()
	}()
}

// reportSettingAutostart 上报开机自启动开关：值取后端**实际注册状态**（登记失败时不会上报错值）。
func reportSettingAutostart() {
	if !accountLoggedIn() {
		return
	}
	if err := accountEnqueueOp(opKeyAutostart, isAutostartEnabled()); err != nil {
		return
	}
	accountSyncKick()
}

// reportSettingPrerelease 上报 Harness 预发布通道开关。
func reportSettingPrerelease(on bool) {
	if !accountLoggedIn() {
		return
	}
	if err := accountEnqueueOp(opKeyHarnessPrerelease, on); err != nil {
		return
	}
	accountSyncKick()
}

// reportHarnessVersionIfChanged 上报最后选用的 Harness 版本。
//
// 在「更新 / 重置 Harness」流程结束时调用：与流程开始前的版本比较，只有真的变了才上报——
// 这样不必在长流程里找成功点，失败回滚时版本未变，自然不会产生错误记录。
func reportHarnessVersionIfChanged(prev string) {
	cur := normalizeVersionText(installedHarnessVersion())
	if cur == "" || cur == normalizeVersionText(prev) {
		return
	}
	if !accountLoggedIn() {
		return
	}
	if err := accountEnqueueOp(opKeyHarnessVersion, cur); err != nil {
		return
	}
	accountRememberHarnessVersion(cur)
	accountSyncKick()
}

// accountRememberHarnessVersion 记录「本机版本已与服务端对账」（落盘，避免重复扫描/上报）。
func accountRememberHarnessVersion(v string) {
	accountMu.Lock()
	accountCur.LastReportedHarnessVersion = normalizeVersionText(v)
	_ = saveAccountState(accountCur)
	accountMu.Unlock()
}

// accountReconcileHarnessVersion 对账本机 Harness 版本并补齐服务器缺失的记录。
//
// 覆盖两种「版本不上报」的盲区（2026-09-21 评估问题②）：
//   - 服务器上没有该 key 的记录（含首次基线时 harness 仍在安装、版本读不到）；
//   - 版本在 dsh-systray 之外被**升高**（源码 checkout 切换、外部 npm 安装）。
//
// 不覆盖的情况：本机从未对账过，而服务器已有该 key 的记录——此时按 LWW 与「待生效」
// 流程收敛（避免两台机器启动时互相把版本推回去）。
//
// 版本**下降**同样不补报（2026-09-22 现场问题②）：手工删除 .dsh / harness 目录后由
// bootstrap 装回默认版本属意外回退，上报会以更大 seq 覆盖账号上的「最后选用版本」，
// 使同步永远回不到原版本；差异留给拉取阶段进入待生效，由用户点「重启生效」恢复。
func accountReconcileHarnessVersion(ctx context.Context, client *accountClient) {
	cur := normalizeVersionText(installedHarnessVersion())
	if cur == "" || !accountLoggedIn() {
		return
	}
	accountMu.Lock()
	prev := normalizeVersionText(accountCur.LastReportedHarnessVersion)
	accountMu.Unlock()
	if prev == cur {
		return // 已对账且无变化
	}
	has, err := accountServerHasKey(ctx, client, opKeyHarnessVersion)
	if err != nil {
		return // 网络/服务端问题：留给下次同步
	}
	if has && prev == "" {
		accountRememberHarnessVersion(cur) // 服务器已有记录：只记「已对账」，不覆盖他人选择
		return
	}
	if has && prev != "" && compareVersions(cur, prev) < 0 {
		// 意外回退（非托盘内重置）：只记对账、不上报，避免覆盖账号版本
		accountRememberHarnessVersion(cur)
		logInfo("account", "本机 Harness 版本低于账号记录（%s < %s）：按意外回退处理，不覆盖账号版本", cur, prev)
		return
	}
	if err := accountEnqueueOp(opKeyHarnessVersion, cur); err != nil {
		return
	}
	accountRememberHarnessVersion(cur)
	accountSyncKick()
	if has {
		logInfo("account", "本机 Harness 版本已在 dsh-systray 之外升高，已补报：%s", cur)
	} else {
		logInfo("account", "服务器缺少本机 Harness 版本记录，已补报：%s", cur)
	}
}

// ---------- 本地插件对账（托盘外安装的插件补报） ----------

// accountLocalPlugin 本机一个参与同步的在线插件（对账用快照）。
type accountLocalPlugin struct {
	Name        string
	Spec        string
	Source      string
	Version     string
	InstalledAt int64 // 安装时间（Unix 秒）；0 = 读不到
}

// accountServerOpHead 服务器上某个 key 的最新一条记录（seq 最大者）。
type accountServerOpHead struct {
	Seq       int64
	UpdatedAt int64 // 服务器接收时间（Unix 秒）
	Value     pluginOpValue
}

// accountFetchOpHeads 全量扫描服务器记录（自游标 0），取每个同步范围内的 key 的最新一条。
//
// 为什么不按本地游标增量扫描：判断「记录是不是已被删除」需要看**历史**记录——游标早已推过
// 那些 seq，增量扫描看不到它们，会把「另一台机器删除后本机又装回」误判成「服务器上没有记录」。
func accountFetchOpHeads(ctx context.Context, client *accountClient) (map[string]accountServerOpHead, error) {
	accountMu.Lock()
	token := accountCur.Token
	accountMu.Unlock()
	if token == "" {
		return nil, &accountError{Code: accErrUnauthorized, Message: "未登录"}
	}
	heads := map[string]accountServerOpHead{}
	cursor := int64(0)
	for i := 0; i < accountOpsScanMaxPages; i++ {
		page, err := client.OpsSince(ctx, token, cursor, accountOpsPageSize)
		if err != nil {
			return nil, err
		}
		for _, op := range page.Ops {
			if !accountSyncKeySupported(op.Key) {
				continue
			}
			if prev, ok := heads[op.Key]; ok && prev.Seq >= op.Seq {
				continue
			}
			var v pluginOpValue
			_ = json.Unmarshal(op.Value, &v) // 形态异常的值保留零值：只按 seq/时间比较
			heads[op.Key] = accountServerOpHead{Seq: op.Seq, UpdatedAt: op.UpdatedAt, Value: v}
		}
		if !page.HasMore || page.Cursor <= cursor {
			break
		}
		cursor = page.Cursor
	}
	return heads, nil
}

// accountLocalPluginInstallTime 插件包的安装时间（Unix 秒）；读不到返回 0。
//
// 取「包的创建时间」：它是 pnpm/npm 把包装进 node_modules 的时刻，正是用户在本机装它的时间；
// 更新（覆盖已有包）不改变创建时间，因此「装完又被账号上的删除记录覆盖」这一判定不会把
// 正常的版本更新误伤成「本机装得更早」。平台不支持创建时间（Unix 返回零值）时退回修改时间；
// 包目录已不在（残留声明等）时退回其所在目录的时间，仍读不到按 0 处理（当无记录补报）。
func accountLocalPluginInstallTime(dirs []string, name string) int64 {
	for _, dir := range dirs {
		p := filepath.Join(dir, "node_modules", filepath.FromSlash(name))
		st, err := os.Stat(p)
		if err != nil {
			parent := filepath.Dir(p) // scoped 包（@scope/name）落在 @scope 目录下
			if ps, perr := os.Stat(parent); perr == nil && ps.IsDir() {
				st = ps
			} else {
				continue
			}
		}
		if bt := creationTimeOf(st); bt > 0 {
			return bt
		}
		return st.ModTime().Unix()
	}
	return 0
}

// accountLocalPluginsSnapshot 本机参与同步的在线插件快照（只含已生效、未处于待应用变更的插件）。
func accountLocalPluginsSnapshot() map[string]accountLocalPlugin {
	marks := pluginPendingMarks()
	pending := map[string]bool{}
	for _, key := range accountPendingKeys() {
		if _, name, ok := splitPluginKey(key); ok {
			pending[name] = true
		}
	}
	out := map[string]accountLocalPlugin{}
	for _, row := range buildPluginRows() {
		if _, ok := out[row.Name]; ok {
			continue // 同名多 spec：以首个行为准（与 accountLocalPluginValue 同口径）
		}
		if !isOnlinePluginSource(row.Source) || !profileContains(row.Profile, accountPluginProfile) {
			continue
		}
		if row.Version == "" || row.Disabled || row.PendingLocal {
			continue // 读不到版本 / 已禁用 / 待重指定：不是本机生效的插件
		}
		if _, ok := marks[row.Name]; ok {
			continue // 托盘内已登记待应用变更：等它执行完再由批处理埋点上报
		}
		if pending[row.Name] {
			continue // 已是待生效改动：等用户点「重启生效」，此时不反向覆盖账号
		}
		out[row.Name] = accountLocalPlugin{
			Name:        row.Name,
			Spec:        strings.TrimSpace(row.Spec),
			Source:      row.Source,
			Version:     row.Version,
			InstalledAt: accountLocalPluginInstallTime(row.Locs, row.Name),
		}
	}
	return out
}

// accountLocalPluginsSnapshotFn 本机插件快照（测试可替换，避免依赖真实 dshHome 目录）。
var accountLocalPluginsSnapshotFn = accountLocalPluginsSnapshot

// accountReconcileLocalPlugins 把本机有、账号记录里没有（或在托盘外升高了版本）的在线插件补报上去。
//
// 为什么需要：插件埋点只覆盖「托盘内发起的变更」（见 reportPluginBatchChanges），而用户完全可能
// 用 npm / pnpm 直接装插件——那条路径不经过托盘，账号上永远没有这条记录，其它机器点多少次
// 「立即同步」都拉不到（2026-09-22 现场：B 机 npm 安装后，本机同步始终 0 条）。
//
// 判定规则（每个插件独立）：
//  1. 服务器上**没有**该 key 的记录 → 补报（action=install）——本机装得比账号已知状态更晚；
//  2. 服务器最新记录是**删除**（action=remove）：
//     - 服务器删除时间**晚于**本机安装时间 → 不报（本机这个是删除前的旧安装，删除方胜）；
//     - 本机安装时间更晚（删掉后又装回来）→ 补报（action=install）；
//  3. 服务器最新记录是安装/更新：本机版本**更高** → 补报（action=update，托盘外升高版本，
//     与 accountReconcileHarnessVersion 同口径）；版本更低或相同 → 不报（可能是意外回退，
//     不覆盖账号记录；差异由拉取阶段进入待生效，等用户点「重启生效」）。
//
// 返回本次补报的条数（记录留在本地队列里，由本次/后续上报送到服务器）。
func accountReconcileLocalPlugins(ctx context.Context, client *accountClient) int {
	if !accountLoggedIn() {
		return 0
	}
	heads, err := accountFetchOpHeads(ctx, client)
	if err != nil {
		if accountErrorCode(err) != accErrUnauthorized {
			logWarn("account", "插件对账扫描服务器记录失败（留待下次同步）: %v", err)
		}
		return 0
	}
	accountMu.Lock()
	pendingKeys := make(map[string]bool, len(accountCur.PendingOps))
	for _, op := range accountCur.PendingOps {
		pendingKeys[op.Key] = true
	}
	accountMu.Unlock()

	reported := 0
	for name, lp := range accountLocalPluginsSnapshotFn() {
		key := accountPluginKey(name)
		if pendingKeys[key] {
			reported++ // 本次同步的上报阶段已登记（前半程刚入队）：算作本次补报，不重复登记
			continue
		}
		head, ok := heads[key]
		if !ok {
			accountReportReconciledPlugin(lp, "install", "账号上没有该插件的记录")
			reported++
			continue
		}
		if head.Value.Action == "remove" {
			if lp.InstalledAt > 0 && lp.InstalledAt <= head.UpdatedAt {
				continue // 本机这个是服务器删除记录之前装的旧包：删除方胜，不把它装回来
			}
			accountReportReconciledPlugin(lp, "install", fmt.Sprintf("账号记录为已删除（%s），本机安装更新", formatAccountTime(head.UpdatedAt)))
			reported++
			continue
		}
		if lp.Version != "" && head.Value.Version != "" {
			switch cmp := compareVersions(lp.Version, head.Value.Version); {
			case cmp > 0:
				accountReportReconciledPlugin(lp, "update", fmt.Sprintf("本机版本更高（%s > %s）", lp.Version, head.Value.Version))
				reported++
			case cmp < 0:
				logInfo("account", "本机插件 %s 版本低于账号记录（%s < %s）：按意外回退处理，不覆盖账号记录",
					name, lp.Version, head.Value.Version)
			}
		}
	}
	return reported
}

// formatAccountTime 服务器时间戳的排障展示（0 或异常值给占位符）。
func formatAccountTime(ts int64) string {
	if ts <= 0 {
		return "时间未知"
	}
	return time.Unix(ts, 0).Local().Format("2006-01-02 15:04")
}

// accountReportReconciledPlugin 登记一条对账补报（入队 + 立刻试上报；失败留队列由后台重试）。
func accountReportReconciledPlugin(lp accountLocalPlugin, action, why string) {
	val := pluginOpValue{Action: action, Spec: lp.Spec, Source: lp.Source, Version: lp.Version}
	if err := accountEnqueueOp(accountPluginKey(lp.Name), val); err != nil {
		logWarn("account", "插件对账补报登记失败 %s: %v", lp.Name, err)
		return
	}
	logInfo("account", "插件对账补报：%s（%s，本机 v%s）", lp.Name, why, lp.Version)
	accountSyncKick()
}

// accountServerHasKey 服务器上是否存在该 key 的记录（分页扫描；用于判定「服务器还没有
// 本机 Harness 版本记录」这一初始化补报条件）。
func accountServerHasKey(ctx context.Context, client *accountClient, key string) (bool, error) {
	accountMu.Lock()
	token := accountCur.Token
	accountMu.Unlock()
	if token == "" {
		return false, &accountError{Code: accErrUnauthorized, Message: "未登录"}
	}
	cursor := int64(0)
	for i := 0; i < accountOpsScanMaxPages; i++ {
		page, err := client.OpsSince(ctx, token, cursor, accountOpsPageSize)
		if err != nil {
			return false, err
		}
		for _, op := range page.Ops {
			if op.Key == key {
				return true, nil
			}
		}
		if !page.HasMore || page.Cursor <= cursor {
			return false, nil
		}
		cursor = page.Cursor
	}
	return false, nil
}

// accountOpsScanMaxPages 单次「按 key 扫描服务器记录」的页数上限（防御性：正常账号记录数极少）。
const accountOpsScanMaxPages = 20

// reportPluginBatchChanges 批处理结果确定后（含自愈/回退）上报成功的**在线**插件变更。
func reportPluginBatchChanges(tasks []*pluginOpTask) {
	if !accountLoggedIn() {
		return
	}
	changed := 0
	for _, t := range tasks {
		if t == nil || !t.ok || t.recordOnly {
			continue
		}
		if t.op != "update" && t.op != "remove" { // enable 只改本地激活状态，不在同步范围
			continue
		}
		if !isOnlinePluginSource(t.row.Source) {
			continue
		}
		val := pluginOpValue{Action: t.op, Spec: t.row.Spec, Source: t.row.Source}
		if t.op == "update" {
			val.Version = t.newVer
			if val.Version == "" {
				val.Version = t.target
			}
		}
		if err := accountEnqueueOp(accountPluginKey(t.name), val); err != nil {
			continue
		}
		changed += 1
	}
	if changed > 0 {
		accountSyncKick()
	}
}
