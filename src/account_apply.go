// account_apply.go：同步「重启生效」的应用流程——进度视图 + 事件驱动 + 中断自愈。
//
// 为什么单独一个文件：旧实现把应用做成同步阻塞的 Wails 调用（最长 30 分钟）且没有任何进度，
// 用户既看不到「在做哪一步」也无法取消（2026-09-21 评估问题①）。这里改为与「更新 Harness」
// 同一套机制：Go 侧起协程推进度（splash.go），前端只负责渲染进度与收尾事件。
//
// 中断语义（用户决策）：应用中途退出托盘应用是允许的——已应用的项逐项落盘（不会把
// 「还没做」记成已生效），下次启动由 revalidatePendingApplyOnStartup 重校验待生效集合，
// 不把半途状态当成事实，也不自动续跑。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

var (
	// accountApplyMu 保护 accountApplyCancel（取消句柄只被应用流程自身与取消按钮/退出清理访问）。
	accountApplyMu     sync.Mutex
	accountApplyCancel context.CancelFunc
)

// accountApplyTimeout 应用流程总预算：含 harness 版本重装（分钟级）与多插件安装 + 网络重试。
const accountApplyTimeout = 45 * time.Minute

// AccountApplyPending 点「重启生效」：起协程执行应用流程并推进度视图，立即返回当前状态。
// 结果经 sync:apply:done 事件回报前端（旧实现是同步返回结果，长操作期间界面无法反映进度）。
func (a *App) AccountApplyPending() AccountStatusInfo {
	if !startAccountApply() {
		return accountSnapshot() // 已在应用中：重复点击幂等忽略
	}
	go runAccountApplyFlow()
	return accountSnapshot()
}

// CancelSyncApply 取消进行中的同步应用（前端进度视图「取消」按钮）：
// 已应用的项保留，剩余项留在待生效集合，下次点击可续做。
func (a *App) CancelSyncApply() {
	cancelAccountApply()
	logUI("取消同步改动应用", "")
}

// startAccountApply 置位应用标记；已有应用在跑时返回 false。
func startAccountApply() bool {
	accountMu.Lock()
	defer accountMu.Unlock()
	if accountApplying {
		return false
	}
	accountApplying = true
	return true
}

// cancelAccountApply 触发取消（无进行中应用则忽略）。
func cancelAccountApply() {
	accountApplyMu.Lock()
	if accountApplyCancel != nil {
		accountApplyCancel()
	}
	accountApplyMu.Unlock()
}

// runAccountApplyFlow 应用流程主体：进度视图 → 逐项应用 → 事件收尾（+失败提示弹窗）。
func runAccountApplyFlow() {
	defer func() {
		accountApplyMu.Lock()
		accountApplyCancel = nil
		accountApplyMu.Unlock()
		accountMu.Lock()
		accountApplying = false
		accountMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), accountApplyTimeout)
	accountApplyMu.Lock()
	accountApplyCancel = cancel
	accountApplyMu.Unlock()
	defer cancel()

	splash := startSplash(T("正在应用同步改动…"))
	setSplashPhase("sync") // 前端据此显示「应用同步改动」进度视图（带取消按钮）
	// 整个同步过程不自动隐藏窗口（用户决策 2026-09-22）：harness 换版步骤会嵌套重置流程，
	// 收尾时若隐藏窗口，用户就看不到后半程（插件）进度，也点不到「取消应用」。
	splash.KeepWindow = true
	// 与更新/重置/插件批处理互斥；托盘「设置」在应用期间只置前、不抢回设置页（见 updateProgressActive）
	harnessOpBusy.Store(true)
	defer harnessOpBusy.Store(false)

	splash.Update(T("正在对齐账号同步数据…"), 0.05)
	res, err := accountApplyPending(ctx, newAccountClient(""), func(text string, pct float64) {
		if pct < 0 {
			pct = 0
		}
		splash.Update(text, pct)
	})
	// 仅登录失效等致命错误会走到这里（单项失败已在应用循环内隔离并保留待生效）
	fatal := ""
	if err != nil {
		fatal = accountErrorText(err)
		accountSetApplyError(fatal)
	}

	switch {
	case fatal != "":
		logUI("同步改动应用失败", fatal)
	case res.Canceled:
		logUI("同步改动应用已取消", fmt.Sprintf("已应用 %d 项，剩余留待生效", res.Applied))
	case len(res.Failed) > 0:
		logUI("同步改动应用部分失败", fmt.Sprintf("应用 %d 项，失败 %d 项", res.Applied, len(res.Failed)))
	default:
		logUI("同步改动已生效", fmt.Sprintf("应用 %d 项，另有 %d 项本就一致", res.Applied, res.Unchanged))
	}

	splash.Close()
	setSplashPhaseQuiet("startup")
	if res.Applied > 0 && appCtx != nil {
		wruntime.EventsEmit(appCtx, "plugins:changed", nil) // 关于页插件列表按新状态刷新
	}
	emitSyncApplyDone(res, fatal)
	// 事件先行、弹窗后行：完成提示是阻塞式原生弹窗，先弹会挡住前端收尾（见 finishHarnessUpdate 注释）
	if note := syncApplyNote(res, fatal); note != "" {
		go showMessageBox(note, appName)
	}
}

// emitSyncApplyDone 通知前端应用流程结束（成功/取消/失败都发）。
func emitSyncApplyDone(res accountSyncResult, fatal string) {
	if appCtx == nil {
		return
	}
	wruntime.EventsEmit(appCtx, "sync:apply:done", map[string]interface{}{
		"ok":       fatal == "" && len(res.Failed) == 0 && !res.Canceled,
		"canceled": res.Canceled,
		"applied":  res.Applied,
		"failed":   sortedFailureLabels(res.Failed),
		"error":    syncApplyErrText(res, fatal),
	})
}

// syncApplyErrText 应用结果的一句话说明（前端提示用；无失败返回空串）。
func syncApplyErrText(res accountSyncResult, fatal string) string {
	switch {
	case fatal != "":
		return fatal
	case res.Canceled:
		return T("应用已取消，剩余改动留待生效")
	case len(res.Failed) > 0:
		return fmt.Sprintf(T("有 %d 项同步改动应用失败：%s"), len(res.Failed), strings.Join(sortedFailureLabels(res.Failed), "、"))
	default:
		return ""
	}
}

// syncApplyNote 需要弹原生对话框的收尾说明（成功时为空——状态页已能表达结果）。
func syncApplyNote(res accountSyncResult, fatal string) string {
	if fatal != "" {
		return fatal + "\n\n待生效改动已保留，可稍后再试。\n\n日志：" + unifiedLogPath()
	}
	if res.Canceled {
		return T("同步改动应用已取消：已生效的改动保留，剩余改动留待下次应用。")
	}
	if len(res.Failed) == 0 {
		return ""
	}
	lines := make([]string, 0, len(res.Failed))
	for _, key := range sortedKeys(res.Failed) {
		lines = append(lines, accountApplyLabel(key)+"："+res.Failed[key])
	}
	return fmt.Sprintf(T("有 %d 项同步改动应用失败：\n· %s\n\n失败项已保留为待生效，修复网络或稍后可再点「重启生效」续做。\n\n日志：%s"),
		len(res.Failed), strings.Join(lines, "\n· "), unifiedLogPath())
}

// revalidatePendingApplyOnStartup 启动时重校验待生效集合（中断自愈）。
//
// 上一次应用可能被强杀（应用进行中用户退出托盘进程 / 进程崩溃）：account.json 里的集合与
// 真实落地状态不再一致——已应用的项可能仍标着「待生效」。这里逐项用**本机当前状态**重判：
// 已满足的丢弃并补记序号（避免下次拉取又入队），其余保留待生效。
// **不自动续跑**：应用是显式动作，由用户再点「重启生效」。
func revalidatePendingApplyOnStartup() {
	// 先做存量迁移：旧版本可能留下「游标已越过未生效记录」的坏状态（见 accountMigrateCursorInvariant），
	// 回拨到 0 让下一次同步全量重拉并按本机现状重判。
	accountMigrateCursorInvariant()

	// 再按本机现状重判「已应用记录」并放回待生效集合：手工删除 .dsh / harness 目录后服务器
	// 游标已推进到这些记录之后，增量拉取再也拿不到它们，只重校验 PendingRemote 会整条漏掉
	// （2026-09-22 现场问题①：重启托盘显示「已同步」，插件列表却是空的）。
	accountReenqueueDriftedApplied()

	accountMu.Lock()
	pending := append([]accountPendingOp(nil), accountCur.PendingRemote...)
	applied := make(map[string]int64, len(accountCur.AppliedSeqs))
	for k, v := range accountCur.AppliedSeqs {
		applied[k] = v
	}
	accountMu.Unlock()

	// 启动即复位上次的应用失败提示：状态按本机现状重新判定，不带着陈旧错误启动。
	defer func() {
		accountMu.Lock()
		accountApplyErr = ""
		accountMu.Unlock()
	}()

	if len(pending) == 0 {
		accountMu.Lock()
		accountCur.PendingApply = false
		accountMu.Unlock()
		return
	}
	kept := make([]accountPendingOp, 0, len(pending))
	dropped := 0
	for _, op := range pending {
		// 判定只看本机当前状态：已应用序号只作参考——本机被重置（删除 .dsh / harness
		// 目录）后该序号不代表改动仍生效，必须保留待生效（2026-09-22 现场问题①）。
		if accountKeyTargetSatisfied(op.Key, op.Value) {
			if op.Seq > 0 {
				applied[op.Key] = op.Seq
			}
			dropped++
			continue
		}
		kept = append(kept, op)
	}
	accountMu.Lock()
	accountCur.PendingRemote = kept
	accountCur.PendingApply = len(kept) > 0
	accountCur.AppliedSeqs = applied
	_ = saveAccountState(accountCur)
	accountMu.Unlock()
	log.Printf("[account] 启动重校验待生效集合：丢弃 %d 项（已应用/已满足），保留 %d 项", dropped, len(kept))
}

// recoverInterruptedPluginSnapshot 插件操作快照残骸自愈：应用/更新/删除在「node_modules 改名
// 快照」后被强杀时会留下快照残骸——活体可能是半装的依赖树。只要快照存在就还原到操作前状态
// （快照 = 最后一次可用状态），再清理残骸；被撤销的那一项仍是待生效，下次同步检查会重新给出。
func recoverInterruptedPluginSnapshot(dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	nmBak := filepath.Join(dir, "node_modules"+pluginSnapSuffix)
	_, nmErr := os.Stat(nmBak)
	pkgBak := filepath.Join(dir, "package.json"+pluginSnapSuffix)
	_, pkgErr := os.Stat(pkgBak)
	if nmErr != nil && pkgErr != nil {
		return // 无残骸
	}
	logWarn("app", "检测到被中断的插件操作留下的快照，正在还原到操作前状态：%s", dir)
	// hadNM 必须与快照实情一致：为 true 时还原会先删活体 node_modules 再改名回来，
	// 若备份其实不存在就会把好好的依赖树删掉。
	restorePluginProfileSnapshot(dir, nmErr == nil)
	cleanupPluginProfileSnapshot(dir)
}
