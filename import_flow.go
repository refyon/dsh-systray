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
	importHealOn   atomic.Bool // 共享自愈进行中（不可取消）
	importCurrent  *importTask // worker 正在执行的任务（供 importRestoreCancelled 读取）
	importPaused   bool        // 本批是否已暂停服务
	healDirs       []string    // 批末共享自愈累积目录
	healHadNM      []bool
)

// importEnqueue 受理一个恢复项：占用 kind 槽（同 kind 未完成则拒绝）并启动 worker。
func importEnqueue(kind string, overwriteOn bool) (bool, string) {
	importQMu.Lock()
	for _, t := range importQueue {
		if t.kind == kind && !t.sent {
			importQMu.Unlock()
			return false, "该恢复项已在处理中（请等待完成或先取消）"
		}
	}
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
	run := !importWorkerOn
	if run {
		importWorkerOn = true
	}
	importQMu.Unlock()
	if run {
		go importWorker()
	}
	return true, ""
}

// importRestoreRunning 是否有恢复任务（队列非空或在跑）。
func importRestoreRunning() bool {
	importQMu.Lock()
	defer importQMu.Unlock()
	return importWorkerOn && len(importQueue) > 0
}

// importRestoreHealing 是否处于（不可中断的）共享自愈阶段。
func importRestoreHealing() bool {
	return importHealOn.Load()
}

// setImportRestoreHealing 置位/清除共享自愈标记。
func setImportRestoreHealing(on bool) {
	importHealOn.Store(on)
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
	defer importQMu.Unlock()
	for _, t := range importQueue {
		if t.kind == kind && !t.sent {
			t.cancel.Store(true)
			logUI("取消恢复导入项", "kind="+kind+" 已请求中断")
			return "ok"
		}
	}
	return "idle"
}

// importWorker 顺序执行队列任务；队列清空后做批末统一收尾（共享自愈或直接恢复服务），
// 再按执行顺序发布各任务的 import:done。
func importWorker() {
	var processed []*importTask
	for {
		importQMu.Lock()
		if len(importQueue) == 0 {
			importWorkerOn = false
			importQMu.Unlock()
			break
		}
		t := importQueue[0]
		importCurrent = t
		importQMu.Unlock()

		runImportTask(t)

		importQMu.Lock()
		importCurrent = nil
		importQueue = importQueue[1:]
		importQMu.Unlock()
		processed = append(processed, t)
	}
	finishImportBatch(processed)
}

// runImportTask 执行单条任务的应用段（pause/解压/注册/消毒/预检/对齐）。
// 结束态写入 t.res（非 plugins 或取消/失败的立即终态；plugins 对齐成功标记 settle，最终
// 结果由批末 finishImportBatch 在共享自愈后写入）。
func runImportTask(t *importTask) {
	emit := func(text string, pct float64, hint ...bool) {
		h := len(hint) > 0 && hint[0]
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
		done(cancelRes())
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
			done(cancelRes())
			return
		}
		if t.kind == "plugins" {
			rollbackImportProfiles(t.dirs, t.hadNM)
		}
		done(failRes(err))
		return
	}

	if t.kind != "plugins" {
		done(okRes())
		return
	}

	// plugins：注册 + 消毒 + 版本预检 + 对齐
	if rerr := registerRestoredPlugins(importZipPath); rerr != nil {
		rollbackImportProfiles(t.dirs, t.hadNM)
		done(failRes(rerr))
		return
	}
	for _, dir := range t.dirs {
		t.notes = append(t.notes, sanitizeProfileLocalDeps(dir)...)
	}
	if t.cancel.Load() {
		rollbackImportProfiles(t.dirs, t.hadNM)
		done(cancelRes())
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
		done(cancelRes())
		return
	}

	// 对齐完成：不在此自愈——注册为批末共享自愈成员
	t.settle = true
	importQMu.Lock()
	healDirs = append(healDirs, t.dirs...)
	healHadNM = append(healHadNM, t.hadNM...)
	importQMu.Unlock()
	emit("插件依赖已就绪，等待批量收尾自愈…", 0.85, true)
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
				"text": "正在启动服务并自愈…（自愈过程不可取消，请稍候）", "pct": 0.9})
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
							"text": fmt.Sprintf("服务自愈进行中…请勿中断（等待步骤 %d）", n), "pct": 0.93})
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

	for _, t := range processed {
		finalizeTask(t, healRan, healNote, healErr)
	}
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
		// 终态取决于共享自愈
		if healErr != nil {
			t.notes = append(t.notes, "自愈未通过并已自动回退："+healErr.Error())
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
