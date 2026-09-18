package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ==================== 导入恢复：多任务队列 + 批量共享自愈 ====================
// 旧实现：单恢复槽 + 每个 plugins 任务独立 pause/heal。新实现：
//   - 多个「恢复项」可先后点击入队，各自独立进度/取消（前端按 kind 逐行展示）；
//   - 服务暂停/恢复按批（batch）整体管理：批内任一非 files 项启动时暂停一次，
//     队列清空后统一收尾——plugins 项只做解压/注册/对齐，自愈延后到批末**共用一次**；
//   - 自愈（不可中断）期间各行的「恢复」按钮由前端全局禁用，取消请求被忽略。
// 崩溃恢复沿用 .importbak 快照 + import-journal.json（一个 zip 的 items 中 plugins 唯一）。

// importTask 队列中的一条恢复任务。
type importTask struct {
	kind      string
	overwrite bool
	cancel    atomic.Bool
	// 执行期字段（worker 独占）
	innerZip  string
	filesDest string
	cln       func()
	dirs      []string
	hadNM     []bool
	notes     []string
	// settle：plugins 对齐成功后等待批末共享自愈的任务
	settle bool
	res    map[string]interface{} // 最终 import:done 负载
	sent   bool
}

// importQueue 全局任务队列（首个元素=正在执行）。
var (
	importQMu      sync.Mutex
	importQueue    []*importTask
	importWorkerOn bool
	importWorkerEx bool        // 现有 worker 已决定退休（队列已排空、正在收尾）：新任务需另起 worker
	importHealOn   atomic.Bool // 共享自愈进行中（不可取消）
	importCurrent  *importTask // worker 正在执行的任务（供 importRestoreCancelled 读取）
	importPaused   bool        // 本批是否已暂停服务
	healDirs       []string    // 批末共享自愈累积目录
	healHadNM      []bool
)

// importEnqueue 受理一个恢复项：占用 kind 槽（同 kind 未完成则拒绝）并启动 worker。
func importEnqueue(kind string, overwriteOn bool) (bool, string) {
	importQMu.Lock()
	kept := importQueue[:0]
	var retire []*importTask
	for _, t := range importQueue {
		if t.kind != kind || t.sent {
			kept = append(kept, t)
			continue
		}
		// 同 kind 且已请求取消：不再继续拦着（用户重试不应被「已在处理中」卡死）。
		// 任务移出队列并终态化：已完成回退的旧任务就此退休；真正还在跑的那个会自行
		// 通过 cancel 中断（terminal 幂等，不会重复发 import:done）。
		t.cancel.Store(true)
		retire = append(retire, t)
		continue
	}
	importQueue = kept
	// 消费 PreviewRestore 暂存的内容
	pendingRestore.mu.Lock()
	ok := pendingRestore.kind == kind && pendingRestore.innerZip != ""
	t := &importTask{
		kind:      kind,
		overwrite: overwriteOn,
		innerZip:  pendingRestore.innerZip,
		filesDest: pendingRestore.filesDest,
		cln:       pendingRestore.innerCln,
	}
	if ok {
		pendingRestore.kind = ""
		pendingRestore.innerZip = ""
		pendingRestore.innerCln = nil
		pendingRestore.filesDest = ""
	}
	pendingRestore.mu.Unlock()
	if !ok {
		importQMu.Unlock()
		return false, "尚未预览或 kind 不匹配: " + kind
	}
	importQueue = append(importQueue, t)
	// 是否要另起 worker：没有在跑的，或现有 worker 已排空队列准备退休
	// （它不会再回头看队列——旧实现只看 importWorkerOn，正好卡在这个交接窗口上，
	// 新入队的任务永远不会被执行：2026-09-14 现场实证 batch finish 只处理了 1 个任务，
	// 而第二次「恢复」明明已被受理）。
	run := !importWorkerOn || importWorkerEx
	if run {
		importWorkerOn = true
		importWorkerEx = false
	}
	importQMu.Unlock()
	// 退休任务在锁外终态化（finalizeTask 自行加锁）：前端据此解锁对应行
	for _, old := range retire {
		finalizeTask(old, false, "", nil)
	}
	if run {
		go importWorker()
	}
	return true, ""
}

// importRestoreRunning 是否有恢复批次在跑（含队列执行与批末共享自愈/收尾阶段）。
// 只看 importWorkerOn：worker 从入队起一直活到整批收尾结束，队列刚空的瞬间收尾尚未开始，
// 此时若按「队列非空」判断会出现窗口期（2026-09-13 修复）。
func importRestoreRunning() bool {
	importQMu.Lock()
	defer importQMu.Unlock()
	return importWorkerOn
}

// importRestoreHealing 是否处于（不可中断的）共享自愈阶段。
func importRestoreHealing() bool {
	return importHealOn.Load()
}

// setImportRestoreHealing 置位/清除共享自愈标记。
func setImportRestoreHealing(on bool) {
	importHealOn.Store(on)
}

// emitImportHealing 共享自愈阶段的进度刷新（kind=plugins, healing=true）：把「正在复验/重新启用
// 哪个插件」告诉界面，避免长自愈期间只剩通用心跳文案（最大化启用遍逐个复验会显著拉长自愈时间）。
func emitImportHealing(text string) {
	if appCtx == nil {
		return
	}
	wruntime.EventsEmit(appCtx, "import:progress", map[string]interface{}{
		"kind": "plugins", "healing": true, "text": text, "pct": 0.93})
}

// importRestoreCancelled 是否已请求取消当前执行中的任务。
func importRestoreCancelled() bool {
	importQMu.Lock()
	cur := importCurrent
	importQMu.Unlock()
	return cur != nil && cur.cancel.Load()
}

// importCancelKind 取消指定 kind 的任务。返回 ok/healing/idle（与旧绑定语义一致，kind 化）。
func importCancelKind(kind string) string {
	if importHealOn.Load() {
		logUI("取消恢复被忽略", "自愈阶段不可中断")
		return "healing"
	}
	importQMu.Lock()
	var retired []*importTask
	hit := false
	kept := importQueue[:0]
	for _, t := range importQueue {
		if t.kind != kind {
			kept = append(kept, t)
			continue
		}
		hit = true
		if t.sent {
			// 已发过 import:done 的残留条目：直接退休，别让它继续拦着后续请求
			// （否则每次取消都命中它、界面永远等不到新结果——2026-09-13 现场问题）。
			log.Printf("import: retiring stale queue entry kind=%s", kind)
			retired = append(retired, t)
			continue
		}
		t.cancel.Store(true)
		kept = append(kept, t)
	}
	importQueue = kept
	importQMu.Unlock()
	for _, old := range retired {
		finalizeTask(old, false, "", nil)
	}
	if !hit {
		return "idle"
	}
	logUI("取消恢复导入项", "kind="+kind+" 已请求中断")
	return "ok"
}

// importWorker 顺序执行队列任务；队列清空后做批末统一收尾（共享自愈或直接恢复服务），
// 再按执行顺序发布各任务的 import:done。
// importWorkerOn 一直保持到**整批收尾（含共享自愈）结束**：队列刚空时收尾还没开始，
// 若此时就置 false，importRestoreRunning 会在「队列空→自愈开始」的窗口里返回 false，
// 让重新选择导入包的请求漏过去（2026-09-13 修复）。
func importWorker() {
	deferred := false
	var processed []*importTask
	for {
		importQMu.Lock()
		if len(importQueue) == 0 {
			// 在自己持有锁的这一步宣布退休：此后（含收尾期间）新入队的任务
			// 由 importEnqueue 另起 worker 接手，不会再丢唤醒。
			importWorkerEx = true
			deferred = true
			importQMu.Unlock()
			break
		}
		t := importQueue[0]
		importCurrent = t
		importQMu.Unlock()

		start := time.Now()
		runImportTask(t)
		log.Printf("import[%s] task returned after %s", t.kind, time.Since(start).Round(time.Second))

		importQMu.Lock()
		importCurrent = nil
		importQueue = importQueue[1:]
		importQMu.Unlock()
		processed = append(processed, t)
	}
	finishImportBatch(processed)
	importQMu.Lock()
	if deferred && importWorkerEx {
		// 收尾期间没有新任务接手：本 worker 归还「在跑」标记
		// （若已有新 worker 顶上来，标记归它，不能在这里清掉）。
		importWorkerOn = false
		importWorkerEx = false
		importQMu.Unlock()
		log.Printf("import: worker exited (processed=%d)", len(processed))
		return
	}
	importQMu.Unlock()
	log.Printf("import: worker exited (processed=%d, handoff)", len(processed))
}

// runImportTask 执行单条任务的应用段（pause/解压/注册/消毒/预检/对齐）。
// 结束态写入 t.res。非 settle 路径（非 plugins 任务、plugins 早失败/取消）在任务内
// 立即 finalizeTask 发布 import:done——会话/文件等恢复完成即显示「已完成」，不必等批末
// 服务收尾；plugins 对齐成功标记 settle，最终结果由批末 finishImportBatch 在共享校验后写入。
func runImportTask(t *importTask) {
	emit := func(text string, pct float64, hint ...bool) {
		h := len(hint) > 0 && hint[0]
		// 阶段埋点：用户报「一直取消不掉、也等不到完成」时，靠日志定位卡在哪一步
		// （2026-09-13 复盘：只有 import:progress 事件、没有阶段日志，无法归因）。
		log.Printf("import[%s] stage: %s (pct=%.2f)", t.kind, text, pct)
		if appCtx != nil {
			wruntime.EventsEmit(appCtx, "import:progress", map[string]interface{}{
				"kind": t.kind, "text": text, "pct": pct, "hint": h})
		}
	}
	stop := func() bool { return t.cancel.Load() }
	done := func(res map[string]interface{}) {
		importQMu.Lock()
		if t.res == nil {
			t.res = res
		}
		importQMu.Unlock()
	}
	// terminal 立即终态：写入 t.res 并马上发布 import:done（批末 finishImportBatch 的
	// finalizeTask 对已 sent 的任务跳过，不会重复发）。
	terminal := func(res map[string]interface{}) {
		done(res)
		finalizeTask(t, false, "", nil)
	}
	okRes := func() map[string]interface{} {
		return map[string]interface{}{"kind": t.kind, "ok": true, "note": strings.Join(t.notes, "；")}
	}
	failRes := func(err error) map[string]interface{} {
		return map[string]interface{}{"kind": t.kind, "error": err.Error(), "note": strings.Join(t.notes, "；")}
	}
	cancelRes := func() map[string]interface{} {
		return map[string]interface{}{"kind": t.kind, "canceled": true, "note": strings.Join(t.notes, "；")}
	}

	if t.cancel.Load() { // 队列中已被取消：未做任何改动，直接取消终态
		terminal(cancelRes())
		return
	}

	// 服务暂停（批内首个非 files 任务执行一次，批末统一恢复）
	if t.kind != "files" && !importPaused {
		emit("正在暂停后台服务…", 0.05)
		pauseServiceForRestore()
		importPaused = true
	}

	if t.kind == "plugins" {
		// 插件项：dirs/快照（在暂停后做），journal importing
		t.dirs = restoredPluginProfileDirs(importZipPath)
		t.hadNM = snapshotImportProfiles(t.dirs)
		_ = writeImportJournal(importJournal{Stage: "importing", Kind: t.kind, Dirs: t.dirs, HadNM: t.hadNM})
	}

	_, err := restoreItem(t.kind, t.innerZip, t.filesDest, t.overwrite,
		func(txt string, p float64) { emit(txt, p) }, stop)
	if t.cln != nil {
		t.cln()
		t.cln = nil
	}
	if err != nil {
		if err == errRestoreCanceled {
			emit("正在回退到恢复前状态…", 0.5)
			if t.kind == "plugins" {
				rollbackImportProfiles(t.dirs, t.hadNM)
			}
			terminal(cancelRes())
			return
		}
		if t.kind == "plugins" {
			rollbackImportProfiles(t.dirs, t.hadNM)
		}
		terminal(failRes(err))
		return
	}

	if t.kind != "plugins" {
		terminal(okRes()) // 会话/文件等：任务完成即报「已完成」，不等批末服务收尾
		return
	}

	// plugins：注册 + 消毒 + 版本预检 + 对齐
	if rerr := registerRestoredPlugins(importZipPath); rerr != nil {
		rollbackImportProfiles(t.dirs, t.hadNM)
		terminal(failRes(rerr))
		return
	}
	t.notes = append(t.notes, sanitizeProfileLocalDepsAll(t.dirs)...)
	if t.cancel.Load() {
		rollbackImportProfiles(t.dirs, t.hadNM)
		terminal(cancelRes())
		return
	}

	// 对齐（含每环境 npm 版本预检与取消 watch）
	total := float64(len(t.dirs))
	for i, dir := range t.dirs {
		if t.cancel.Load() {
			break
		}
		if pf := preflightProfileDeps(dir); len(pf) > 0 {
			pnames := make([]string, 0, len(pf))
			for n := range pf {
				pnames = append(pnames, n)
			}
			sort.Strings(pnames)
			for _, n := range pnames {
				t.notes = append(t.notes, "已自动禁用无效版本插件 "+n+"（"+pf[n]+"）")
				log.Printf("import: preflight disabled %s: %s", n, pf[n])
			}
		}
		emit(fmt.Sprintf("正在下载并安装插件依赖（%d/%d）…", i+1, len(t.dirs)),
			0.45+0.3*float64(i)/total, true)
		stopT := make(chan struct{})
		go func(i int) {
			tk := time.NewTicker(3 * time.Second)
			defer tk.Stop()
			start := time.Now()
			for {
				select {
				case <-tk.C:
					emit(fmt.Sprintf("依赖下载安装中…（第 %d/%d 个环境，已用时 %ds）",
						i+1, len(t.dirs), int(time.Since(start).Seconds())),
						0.45+0.3*(float64(i)+0.5)/total)
				case <-stopT:
					return
				}
			}
		}(i)
		repaired, perr := reconcileProfileDepsRepairWatch(dir, func() bool { return t.cancel.Load() })
		close(stopT)
		if perr != nil {
			log.Printf("import: reconcile failed (%s): %v", dir, perr)
		}
		for _, name := range repaired {
			t.notes = append(t.notes, "已移除无法解析的依赖 "+name)
		}
		if t.cancel.Load() {
			break
		}
	}
	if t.cancel.Load() {
		emit("已中止安装，正在回退…", 0.6, true)
		rollbackImportProfiles(t.dirs, t.hadNM)
		terminal(cancelRes())
		return
	}

	// 对齐完成：不在此校验——注册为批末共享启动校验成员（导入内容必须拉起服务验证一次）
	t.settle = true
	importQMu.Lock()
	healDirs = append(healDirs, t.dirs...)
	healHadNM = append(healHadNM, t.hadNM...)
	importQMu.Unlock()
	emit("插件依赖已就绪，等待启动校验…", 0.85, true)
}

// finishImportBatch worker 收尾：批内如有 plugins settle 成员则做一次共享自愈；然后统一
// 恢复服务，并按执行顺序发布各任务的 import:done。
func finishImportBatch(processed []*importTask) {
	importQMu.Lock()
	dirs := append([]string(nil), healDirs...)
	hadNM := append([]bool(nil), healHadNM...)
	healDirs = nil
	healHadNM = nil
	importQMu.Unlock()

	healRan := false
	var healNote string
	var healErr error
	if len(dirs) > 0 {
		// 共享自愈：不可中断（CancelRestore 忽略）
		setImportRestoreHealing(true)
		_ = writeImportJournal(importJournal{Stage: "healing", Kind: "plugins", Dirs: dirs, HadNM: hadNM})
		if appCtx != nil {
			wruntime.EventsEmit(appCtx, "import:progress", map[string]interface{}{
				"kind": "plugins", "healing": true,
				"text": "正在启动服务并校验插件兼容性…（启动校验过程不可取消，请稍候）", "pct": 0.9})
		}
		stopHB := make(chan struct{})
		hbDone := make(chan struct{})
		go func() {
			defer close(hbDone)
			tk := time.NewTicker(8 * time.Second)
			defer tk.Stop()
			n := 0
			for {
				select {
				case <-tk.C:
					n++
					if appCtx != nil {
						wruntime.EventsEmit(appCtx, "import:progress", map[string]interface{}{
							"kind": "plugins", "healing": true,
							"text": fmt.Sprintf("服务启动校验中…请勿中断（等待步骤 %d）", n), "pct": 0.93})
					}
				case <-stopHB:
					return
				}
			}
		}()
		healNote, healErr = finishPluginImport(dirs, hadNM)
		close(stopHB)
		<-hbDone
		setImportRestoreHealing(false)
		healRan = true
	}
	// 恢复服务：自愈已自行重启服务则不重复恢复；否则批内暂停过恢复一次
	if !healRan && importPaused {
		resumeServiceAfterRestore()
	}
	importPaused = false
	clearImportJournal()
	log.Printf("import: batch finish begin (tasks=%d healRan=%v healErr=%v)", len(processed), healRan, healErr)

	for _, t := range processed {
		finalizeTask(t, healRan, healNote, healErr)
	}
	log.Printf("import: batch finish done (tasks=%d)", len(processed))
}

// finalizeTask 补全任务终态并推送 import:done。
func finalizeTask(t *importTask, healRan bool, healNote string, healErr error) {
	importQMu.Lock()
	if t.sent {
		importQMu.Unlock()
		return
	}
	t.sent = true
	res := t.res
	importQMu.Unlock()
	if t.settle && res == nil {
		// 终态取决于批末共享收尾（启动校验 → 不兼容则自动禁用 → 仍失败回退）
		if healErr != nil {
			t.notes = append(t.notes, "启动校验未通过并已自动回退："+healErr.Error())
			res = map[string]interface{}{"kind": t.kind, "error": healErr.Error(), "note": strings.Join(t.notes, "；")}
		} else {
			if healNote != "" {
				t.notes = append(t.notes, healNote)
			}
			res = map[string]interface{}{"kind": t.kind, "ok": true, "note": strings.Join(t.notes, "；")}
		}
	}
	_ = healRan
	if appCtx == nil {
		return
	}
	if m, ok := res["error"]; ok && fmt.Sprint(m) != "" {
		logUI("恢复失败", fmt.Sprintf("kind=%s | %v", t.kind, m))
		wruntime.EventsEmit(appCtx, "import:done", res)
		return
	}
	if _, ok := res["canceled"]; ok {
		logUI("恢复已取消", "kind="+t.kind)
		wruntime.EventsEmit(appCtx, "import:done", res)
		return
	}
	logUI("恢复完成", "kind="+t.kind)
	wruntime.EventsEmit(appCtx, "import:done", res)
}

// purgeImportQueueForPick 选择新压缩包时清掉未完成队列（避免旧任务引用旧 zip 内容）。
func purgeImportQueueForPick() {
	importQMu.Lock()
	for _, t := range importQueue {
		t.cancel.Store(true)
	}
	importQMu.Unlock()
}
