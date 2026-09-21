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
