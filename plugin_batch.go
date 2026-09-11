package main

// ==================== 插件操作批处理队列 ====================
// 背景：单次插件更新/删除的耗时大头是固定开销——停服 → pnpm → 重启 → 健康校验（≥10s 窗口）。
// 连续操作多个插件时这段开销被逐次重复。本队列把「连续点击的多个插件更新/删除」并入一批：
//   - 包操作阶段：整批只停一次服务，逐项「快照 → 消毒 → pnpm → 结果校验」；单项失败只回退
//     该项快照（其余继续），不发弹窗、不重启；
//   - 收尾阶段：整批一次启动 + 健康校验。校验失败才走自愈（禁用启动日志点名的嫌疑插件 →
//     兜底禁用全部已激活用户插件），自愈仍失败才整批回退（逆序还原各项快照）并重启。
// 结构与导入恢复队列（import_flow.go）同构：顺序执行、批末统一收尾、逐项 plugin:op:done 回报。
//
// 队列边界：worker 在队列非空期间持续取任务——用户在一次批处理进行中继续点击的行会并入同一批，
// 全部完成后统一校验、统一提示，无需用户等待上一项结束再点下一项。
//
// 与单插件路径的语义差异（有意收敛）：
//   - 更新前处于「已禁用」的插件：更新成功后先解除禁用（加回 bundles），由批末那次启动校验一并
//     判定——仍不兼容时 disableBootSuspects 会重新禁用它（等价于单插件路径的「启用失败自动重新
//     禁用」），不额外增加重启次数；
//   - 启动失败归因不再区分「嫌疑是否含本次目标插件」：批内可能同时改了多个插件，统一按
//     「点名嫌疑 → 全部用户插件」两级自愈，两级都不奏效才整批回退。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// pluginOpTask 队列中的一项插件操作（update / remove）。
type pluginOpTask struct {
	id   string
	op   string // update | remove
	name string

	// 执行期字段（worker 独占）
	row         PluginRow
	locs        []string
	hadNM       []bool
	snap        string // 本项专属的快照后缀（.pbak<N>）：同一 profile 上多项操作各自独立快照
	target      string // update：本次安装的目标版本（检查结果）
	newVer      string // update：安装后的实际版本
	recordOnly  bool   // 只改记录（待重指定 / 无依赖声明的自动禁用行）：不停服、不做包操作
	wasDisabled bool   // update 前处于禁用态：成功后解除禁用，交批末校验判定

	// 结果
	ok     bool
	reason string
	note   string
}

var (
	pluginQMu       sync.Mutex
	pluginQueue     []*pluginOpTask
	pluginActive    []*pluginOpTask // 已出队、正在本批执行/收尾的任务（重复登记者判定范围）
	pluginPending   []*pluginOpTask // 已登记但**尚未执行**的变更（等用户确认应用）
	pluginQWorkerOn bool
	pluginQSeq      atomic.Int64 // 任务序号：分配快照后缀，避免同环境多项快照互相覆盖
)

// pluginBatchRunning 是否有插件操作批处理在跑（含队列中待执行的任务）。
func pluginBatchRunning() bool {
	pluginQMu.Lock()
	defer pluginQMu.Unlock()
	return pluginQWorkerOn
}

// pluginPendingCount 待应用变更条数（关闭窗口询问 / 关于页横幅用）。
func pluginPendingCount() int {
	pluginQMu.Lock()
	defer pluginQMu.Unlock()
	return len(pluginPending)
}

// pluginPendingOps 待应用变更的持久化形态（写 config.json；跨托盘重启保留）。
func pluginPendingOps() []pendingPluginOp {
	pluginQMu.Lock()
	defer pluginQMu.Unlock()
	out := make([]pendingPluginOp, 0, len(pluginPending))
	for _, t := range pluginPending {
		out = append(out, pendingPluginOp{ID: t.id, Op: t.op})
	}
	return out
}

// pluginPendingMarks 待应用变更的「插件名 → 操作」，供插件行标记待应用状态。
func pluginPendingMarks() map[string]string {
	pluginQMu.Lock()
	defer pluginQMu.Unlock()
	out := make(map[string]string, len(pluginPending))
	for _, t := range pluginPending {
		out[t.name] = t.op
	}
	return out
}

// PendingPluginChange 关于页横幅展示的一条待应用变更。
type PendingPluginChange struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Op   string `json:"op"` // update | remove
}

// pluginPendingList 待应用变更列表（关于页横幅）。
func pluginPendingList() []PendingPluginChange {
	pluginQMu.Lock()
	defer pluginQMu.Unlock()
	out := make([]PendingPluginChange, 0, len(pluginPending))
	for _, t := range pluginPending {
		out = append(out, PendingPluginChange{ID: t.id, Name: t.name, Op: t.op})
	}
	return out
}

// loadPendingPluginOps 启动时载入持久化的待应用变更（跨托盘重启保留）。逐条按当前插件列表
// 现场校验：插件已不存在 / 操作已不适用则丢弃，避免恢复出无法执行的条目；返回丢弃条数
// （>0 时调用方回写配置）。
func loadPendingPluginOps(ops []pendingPluginOp) int {
	if len(ops) == 0 {
		return 0
	}
	pluginQMu.Lock()
	defer pluginQMu.Unlock()
	dropped := 0
	for _, o := range ops {
		row, ok := findPluginRowByID(o.ID)
		if !ok {
			dropped++
			continue
		}
		t, why := pluginOpPrepare(row, o.Op)
		if why != "" {
			dropped++
			continue
		}
		dup := false
		for _, q := range pluginPending {
			if q.id == t.id {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		pluginPending = append(pluginPending, t)
	}
	if len(pluginPending) > 0 {
		logUI("载入待应用插件变更", fmt.Sprintf("%d 项（上次运行登记，尚未生效）", len(pluginPending)))
	}
	return dropped
}

// pluginPendingDropRestored 导入恢复成功后对账：本次恢复到的插件，其先前登记的待应用变更一律
// 作废——按「最后一次操作生效」，重新导入即用户对该插件的最新意图（典型：先登记删除、随后又把
// 它导回来，则不应再删）。返回被作废变更的展示文案（供导入结果提示）。
func pluginPendingDropRestored(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	pluginQMu.Lock()
	var dropped []string
	pluginPending, dropped = pendingDropNames(pluginPending, names)
	pluginQMu.Unlock()
	if len(dropped) > 0 {
		saveCurrentConfig() // 持久化：作废后不再跨重启提示
		logUI("按最后操作生效，作废待应用变更", strings.Join(dropped, "、"))
		emitPluginPendingChanged()
	}
	return dropped
}

// emitPluginPendingChanged 通知前端待应用集合变化（横幅/行标记刷新）。
func emitPluginPendingChanged() {
	if appCtx != nil {
		wruntime.EventsEmit(appCtx, "plugin:pending:changed", map[string]interface{}{
			"count": pluginPendingCount(),
		})
	}
}

// pendingUpsertTask 把一条变更并入待应用列表（纯函数，便于单测）：同一插件按「最后操作生效」
// ——同操作幂等（原样返回），换操作则覆盖；返回新列表与是否发生覆盖。
func pendingUpsertTask(list []*pluginOpTask, t *pluginOpTask) ([]*pluginOpTask, bool) {
	for i, q := range list {
		if q.id != t.id {
			continue
		}
		if q.op == t.op {
			return list, false
		}
		out := append([]*pluginOpTask{}, list...)
		out[i] = t
		return out, true
	}
	return append(append([]*pluginOpTask{}, list...), t), false
}

// pendingDropNames 从待应用列表移除指定插件名的条目（纯函数，便于单测）：返回保留列表与被移除
// 条目的展示文案（如 "dsh-x（删除）"）。
func pendingDropNames(list []*pluginOpTask, names []string) (kept []*pluginOpTask, dropped []string) {
	if len(names) == 0 {
		return list, nil
	}
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	verbOf := map[string]string{"update": "（更新）", "remove": "（删除）"}
	for _, t := range list {
		if set[t.name] {
			dropped = append(dropped, t.name+verbOf[t.op])
			continue
		}
		kept = append(kept, t)
	}
	return kept, dropped
}

// pluginOpStage 登记一条插件变更（点击更新/删除时调用）：**不执行、不停服、不切窗口**，只把
// 变更放进待应用区，由用户在关闭设置窗口时确认、或在关于页点「立即应用」时整批执行（一次重启）。
// 同一插件重复登记按「最后操作生效」：同操作幂等，换操作则覆盖。拒绝原因经 plugin:op:done 回报。
func pluginOpStage(id, op string) (bool, string) {
	reject := func(reason string) (bool, string) {
		name := id
		if row, ok := findPluginRowByID(id); ok {
			name = row.Name
		}
		emitPluginOpDone(PluginOpDone{Name: name, Op: op, OK: false, Reason: reason})
		return false, reason
	}
	if importRestoreRunning() {
		return reject("正在恢复导入内容，请等待完成后再操作插件。")
	}
	if harnessOpBusy.Load() {
		return reject("正在更新或重置 DeepSeek Harness，请等待完成后再操作插件。")
	}
	if pluginBatchRunning() {
		return reject("正在应用已登记的插件变更，请等待完成后再操作。")
	}
	row, ok := findPluginRowByID(id)
	if !ok {
		return reject("未找到该插件，可能已被移除。")
	}
	t, why := pluginOpPrepare(row, op)
	if why != "" {
		return reject(why)
	}
	verb := map[string]string{"update": "更新", "remove": "删除"}[op]

	pluginQMu.Lock()
	pluginPending, _ = pendingUpsertTask(pluginPending, t)
	n := len(pluginPending)
	pluginQMu.Unlock()

	saveCurrentConfig()
	logUI("登记待应用变更", fmt.Sprintf("%s（%s）| 待应用共 %d 项", row.Name, verb, n))
	emitPluginPendingChanged()
	if appCtx != nil {
		wruntime.EventsEmit(appCtx, "plugins:changed", nil)
	}
	return true, ""
}

// pluginOpDiscard 撤销一条待应用变更（行内「撤销」）。
func pluginOpDiscard(id string) bool {
	pluginQMu.Lock()
	found := false
	var kept []*pluginOpTask
	for _, t := range pluginPending {
		if t.id == id {
			found = true
			continue
		}
		kept = append(kept, t)
	}
	pluginPending = kept
	pluginQMu.Unlock()
	if !found {
		return false
	}
	saveCurrentConfig()
	logUI("撤销待应用变更", id)
	emitPluginPendingChanged()
	if appCtx != nil {
		wruntime.EventsEmit(appCtx, "plugins:changed", nil)
	}
	return true
}

// pluginPendingHasPackageOps 待应用变更里是否有需要真实包操作（停服/pnpm）的条目。
// 全为记录类（待重指定 / 无依赖的自动禁用行）时应用不产生进度窗口，调用方需自行隐藏窗口。
func pluginPendingHasPackageOps() bool {
	pluginQMu.Lock()
	defer pluginQMu.Unlock()
	for _, t := range pluginPending {
		if !t.recordOnly {
			return true
		}
	}
	return false
}

// pluginOpApplyPending 应用全部待应用变更：移入执行队列并启动批处理（整批一次重启校验）。
// 返回 (是否受理, 拒绝原因)。
func pluginOpApplyPending() (bool, string) {
	pluginQMu.Lock()
	if len(pluginPending) == 0 {
		pluginQMu.Unlock()
		return false, ""
	}
	if pluginQWorkerOn {
		pluginQMu.Unlock()
		return false, "正在应用插件变更，请等待本次批量操作完成。"
	}
	batch := pluginPending
	pluginPending = nil
	for _, t := range batch {
		t.ok, t.reason, t.note = false, "", ""
		t.hadNM = nil
	}
	pluginQueue = append(pluginQueue, batch...)
	pluginQWorkerOn = true
	pluginQMu.Unlock()

	saveCurrentConfig() // 已转入执行：清空持久化的待应用列表
	logUI("应用待应用插件变更", fmt.Sprintf("%d 项", len(batch)))
	emitPluginPendingChanged()
	go pluginBatchWorker()
	return true, ""
}

// pluginOpPrepare 校验一次插件操作并构造任务（纯逻辑，便于单测）：返回任务与拒绝原因。
// 更新：仅接受有远程来源的行（本地路径安装走行内「更新…」选目录，需目录选择框）；
// 删除：待重指定行与「无依赖声明的自动禁用」行只需清记录，标记 recordOnly（不停服、不重启）。
func pluginOpPrepare(row PluginRow, op string) (*pluginOpTask, string) {
	t := &pluginOpTask{
		id: row.ID, op: op, name: row.Name, row: row, locs: row.Locs,
		wasDisabled: row.Disabled, snap: fmt.Sprintf(".pbak%d", pluginQSeq.Add(1)),
	}
	switch op {
	case "update":
		switch {
		case row.PendingLocal:
			return nil, "本地插件「" + row.Name + "」的原路径在本机不存在，请用行内「更新…」重新指定目录。"
		case row.Source == "file":
			return nil, "本地路径安装的插件请用行内「更新…」选择目录后更新。"
		case !row.CanUpdate:
			msg := row.Reason
			if msg == "" {
				msg = "该插件无远程更新来源。"
			}
			return nil, msg
		}
	case "remove":
		t.recordOnly = row.PendingLocal || row.GhostDisabled
	default:
		return nil, "未知操作：" + op
	}
	return t, ""
}

// pluginBatchWorker 顺序执行队列中的任务：包操作阶段逐项进行（单项失败只回退该项），
// 队列清空后统一收尾（一次启动校验 + 自愈/整批回退 + 逐项事件）。
//
// 批标记（pluginQWorkerOn）直到收尾结束才清除：收尾期间入队的任务会排在下一批，
// 由同一个 worker 接着处理——避免收尾（正在 kill/拉起服务）与新 worker 的启动校验并发。
func pluginBatchWorker() {
	for {
		tasks, splash, paused := pluginBatchDrain()
		finishPluginBatch(tasks, splash, paused)
		pluginQMu.Lock()
		pluginActive = nil
		if len(pluginQueue) == 0 {
			pluginQWorkerOn = false
			pluginQMu.Unlock()
			return
		}
		pluginQMu.Unlock()
	}
}

// pluginBatchDrain 取空当前队列（含执行期新入队的任务）并执行包操作阶段：
// 首个真实包操作前停一次服务（整批只停这一次），逐项执行。
func pluginBatchDrain() (tasks []*pluginOpTask, splash *SplashState, paused bool) {
	for {
		pluginQMu.Lock()
		if len(pluginQueue) == 0 {
			pluginQMu.Unlock()
			break
		}
		t := pluginQueue[0]
		pluginQueue = pluginQueue[1:]
		pluginActive = append(pluginActive, t)
		pluginQMu.Unlock()
		tasks = append(tasks, t)

		// 执行前用当前插件状态刷新任务：待应用期间插件可能被重新导入、换版本或改了 spec；
		// 已不存在则跳过本项（按最后操作生效，不误删/误更新其它来源装回来的插件）。
		if !syncPluginTaskRow(t) {
			t.fail("插件已不存在（可能已被删除或移除），本项已跳过")
			continue
		}
		if t.recordOnly {
			runPluginRecordOnly(t)
			continue
		}
		if !paused {
			killServer()
			time.Sleep(1 * time.Second)
			paused = true
		}
		if splash == nil {
			openUpdateCheckFlow() // 批处理同样占用「检查/更新窗口」，自动更新提示不再重复弹窗
			splash = startSplash(TF("正在批量处理插件（%d 项）…", len(tasks)))
		}
		runPluginPackagePhase(t, splash, len(tasks))
	}
	return tasks, splash, paused
}

// syncPluginTaskRow 执行前用当前插件状态刷新任务字段（待应用期间插件可能被重新导入、换版本或
// 改了 spec）：返回 false 表示该插件已不在插件列表中，调用方跳过本项。
func syncPluginTaskRow(t *pluginOpTask) bool {
	row, ok := findPluginRowByID(t.id)
	if !ok {
		return false
	}
	t.row = row
	t.locs = row.Locs
	t.wasDisabled = row.Disabled
	t.recordOnly = t.op == "remove" && (row.PendingLocal || row.GhostDisabled)
	return true
}

// runPluginRecordOnly 只改记录的任务（待重指定行 / 无依赖声明的自动禁用行）：
// 不进事务、不停服、不重启（与单插件路径一致）。
func runPluginRecordOnly(t *pluginOpTask) {
	row := t.row
	if row.PendingLocal {
		for _, dir := range row.Locs {
			clearPendingLocal(dir, row.Name)
			_ = clearProfileDisabledRecord(dir, row.Name)
		}
		t.ok = true
		t.note = "「待重指定」记录已移除"
		logUI("移除待重指定记录", fmt.Sprintf("%s（%d 个环境）", row.Name, len(row.Locs)))
		return
	}
	for _, dir := range row.Locs {
		_ = clearProfileDisabledRecord(dir, row.Name)
		target := filepath.Join(dir, "node_modules", filepath.FromSlash(row.Name))
		if st, err := os.Stat(target); err == nil && st.IsDir() {
			_ = os.RemoveAll(target)
		}
	}
	t.ok = true
	t.note = "自动禁用记录已移除"
	logUI("移除自动禁用记录", row.Name)
}

// runPluginPackagePhase 单项包操作阶段（不含重启）。
func runPluginPackagePhase(t *pluginOpTask, splash *SplashState, idx int) {
	switch t.op {
	case "update":
		runPluginUpdatePhase(t, splash, idx)
	case "remove":
		runPluginRemovePhase(t, splash, idx)
	}
}

// runPluginUpdatePhase 更新单项：解析目标版本 → 快照 → 消毒 → pnpm → 安装后版本对照。
// 任一步失败只回退本项快照并记录原因，不影响批内其它项。
func runPluginUpdatePhase(t *pluginOpTask, splash *SplashState, idx int) {
	row := t.row
	splash.Update(fmt.Sprintf("正在检查 %s 的可用版本（第 %d 项）…", row.Name, idx), 0.2)
	check := checkPluginUpdateByRow(row)
	if check.Error != "" {
		t.fail("检查更新失败：" + check.Error)
		return
	}
	target := check.Latest
	registry := ""
	if row.Source == "npm" {
		if fresh, reg, err := fetchNpmLatestWithSource(row.Name); err == nil && fresh != "" {
			target, registry = fresh, reg
		} else {
			registry = pluginRegistries[0]
		}
	}
	args, err := pluginUpdateArgs(row, target, registry)
	if err != nil {
		t.fail(err.Error())
		return
	}
	t.target = target

	for _, dir := range row.Locs {
		t.hadNM = append(t.hadNM, snapshotPluginProfileSuffix(dir, t.snap))
	}
	for _, n := range guardProfileLocalDeps(row.Locs...) {
		logUI("本地依赖消毒", n)
	}

	perr, retried := runPluginPnpmWithAgeRetry(splash, row.Locs, args, row.Name, idx, "更新")
	if perr != nil {
		restorePluginTaskSnapshot(t)
		msg := fmt.Sprintf("安装失败：%v", perr)
		if retried {
			msg += "（已以临时跳过发布年龄校验重试仍失败；可检查 pnpm-workspace.yaml 的 minimumReleaseAge 设置）"
		}
		t.fail(msg)
		return
	}

	// 安装后版本对照：安装结果必须达到检查到的目标（同单插件路径）
	newVer := installedPluginVersion(row.Locs[0], row.Name)
	switch row.Source {
	case "npm":
		if newVer == "" || compareVersions("v"+newVer, "v"+target) < 0 {
			restorePluginTaskSnapshot(t)
			t.fail(fmt.Sprintf("安装后版本仍为 %s（预期 %s，registry 元数据缓存/镜像可能滞后）",
				orDash(newVer), orDash(target)))
			return
		}
	case "github":
		if newVer != "" && check.Latest != "" && compareVersions("v"+newVer, "v"+check.Latest) < 0 {
			restorePluginTaskSnapshot(t)
			t.fail(fmt.Sprintf("安装后版本 %s 低于预期 %s，未能重解析到默认分支最新状态",
				orDash(newVer), orDash(check.Latest)))
			return
		}
	}
	if newVer == "" || newVer == row.Version {
		if row.Source == "github" {
			t.note = "已更新到默认分支最新提交（上游未递增版本号）"
		} else {
			t.note = "已更新到 " + orDash(target) + "（版本号显示未变化，可能为 registry 元数据延迟）"
		}
	}
	t.newVer = newVer
	t.ok = true
	logUI("插件包操作完成", fmt.Sprintf("%s: %s -> %s", row.Name, orDash(row.Version), orDash(newVer)))
}

// runPluginRemovePhase 删除单项：快照 → 消毒 → pnpm remove → 摘除 bundle 激活 → 校验已不再声明。
func runPluginRemovePhase(t *pluginOpTask, splash *SplashState, idx int) {
	row := t.row
	for _, dir := range row.Locs {
		t.hadNM = append(t.hadNM, snapshotPluginProfileSuffix(dir, t.snap))
	}
	for _, n := range guardProfileLocalDeps(row.Locs...) {
		logUI("本地依赖消毒", n)
	}
	perr, retried := runPluginPnpmWithAgeRetry(splash, row.Locs, []string{"remove", row.Name}, row.Name, idx, "移除")
	if perr != nil {
		restorePluginTaskSnapshot(t)
		msg := fmt.Sprintf("移除失败：%v", perr)
		if retried {
			msg += "（已以临时跳过发布年龄校验重试仍失败）"
		}
		t.fail(msg)
		return
	}
	// pnpm 不感知 dsh.profile.bundles：删除必须同步摘除激活声明，残留会导致
	// 「cannot resolve profile bundle」硬失败
	for _, dir := range row.Locs {
		if err := stripProfileBundleEntry(dir, row.Name); err != nil {
			restorePluginTaskSnapshot(t)
			t.fail("清理激活清单失败：" + err.Error())
			return
		}
	}
	for _, dir := range row.Locs {
		if profileDeclaresPlugin(dir, row.Name) {
			restorePluginTaskSnapshot(t)
			t.fail(fmt.Sprintf("删除后 %s 的 package.json 仍声明 %s", dir, row.Name))
			return
		}
	}
	t.ok = true
	logUI("插件包操作完成", fmt.Sprintf("已移除 %s", row.Name))
}

// runPluginPnpmWithAgeRetry 逐目录执行 pnpm 命令并捕获输出；供应链发布年龄校验拦截时以
// --config.minimumReleaseAge=0 临时跳过校验重试一次（与单插件路径同一策略：拦截对象常是
// 与本次操作无关的其它插件，pnpm 解析整张依赖图时一并被拒）。返回最终错误与是否重试过。
func runPluginPnpmWithAgeRetry(splash *SplashState, dirs []string, args []string, name string, idx int, verb string) (error, bool) {
	var perr error
	retried := false
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt == 2 {
			retried = true
			splash.Update("供应链策略拦截（发布年龄校验），正在以临时跳过校验重试…", 0.5)
		}
		perr = nil
		failOut := ""
		for i, dir := range dirs {
			splash.Update(fmt.Sprintf("正在%s %s（第 %d 项 · %d/%d 个环境）…",
				verb, name, idx, i+1, len(dirs)), 0.3+0.3*float64(i)/float64(len(dirs)))
			cmdArgs := args
			if attempt == 2 {
				cmdArgs = append(append([]string{}, args...), "--config.minimumReleaseAge=0")
			}
			out, err := runProfileCmdCapture(dir, pnpmCmd(), cmdArgs...)
			if err != nil {
				failOut = out
				perr = profileInstallErr(err, out)
				break
			}
		}
		if perr == nil || !supplyChainViolation(failOut) || attempt == 2 {
			break
		}
		logUI("供应链策略拦截，重试", fmt.Sprintf("%s：lockfile 发布年龄校验失败，临时跳过重试", name))
	}
	return perr, retried
}

// restorePluginTaskSnapshot 回退单项的包操作（不发弹窗、不重启——重启由批末统一进行）。
// 尚未快照（失败发生在快照之前）时无可回退，直接返回——否则会走「无 node_modules 备份」
// 分支对 profile 空跑一次 pnpm install。
func restorePluginTaskSnapshot(t *pluginOpTask) {
	if len(t.hadNM) == 0 {
		return
	}
	for i, dir := range t.locs {
		had := false
		if i < len(t.hadNM) {
			had = t.hadNM[i]
		}
		restorePluginProfileSnapshotSuffix(dir, t.snap, had)
	}
}

// fail 记录单项失败。
func (t *pluginOpTask) fail(reason string) {
	t.ok = false
	t.reason = reason
	t.note = ""
	logUI("插件操作失败", fmt.Sprintf("%s（%s）：%s", t.name, t.op, reason))
}

// finishPluginBatch 批末收尾：一次启动校验 → 失败时两级自愈 → 仍失败整批回退 → 逐项结果与汇总。
// paused：本批是否停过服务（没停过且无成功项时无需重启；停过就必须把服务拉回来）。
func finishPluginBatch(tasks []*pluginOpTask, splash *SplashState, paused bool) {
	if splash != nil {
		defer closeUpdateCheckFlow()
	}
	var active []*pluginOpTask // 有真实包操作且成功、需要参与启动校验与收尾的任务
	for _, t := range tasks {
		if !t.recordOnly && t.ok {
			active = append(active, t)
		}
	}
	scopeNote := ""
	switch {
	case len(active) > 0:
		// 此前被禁用的插件已随本次更新成功：先解除禁用，让批末这一次启动校验一并判定
		// （仍不兼容时下方自愈会重新禁用它）。
		for _, t := range active {
			if t.op == "update" && t.wasDisabled {
				for _, dir := range t.locs {
					_ = enablePluginInProfile(dir, t.row.Name)
				}
			}
		}
		splash.Update(TF("正在重启服务并校验（%d 项变更）…", len(active)), 0.85)
		if !restartAndVerifyServer() {
			splash.Update(T("启动校验失败，正在排查不兼容插件…"), 0.9)
			dirs := batchTaskDirs(active)
			disabled, ok := disableBootSuspects(dirs)
			allDisabled := false
			if !ok {
				splash.Update(T("服务启动受阻，正在尝试禁用全部用户插件…"), 0.93)
				disabled, ok = disableAllUserPlugins(allProfileDirs())
				allDisabled = ok
			}
			if ok {
				scopeNote = batchDisabledNote(disabled, allDisabled)
			} else {
				// 两级自愈均未奏效（核心故障）：整批回退到各自操作前的状态
				rollbackPluginBatch(active, splash)
			}
		}
	case paused:
		// 批内没有成功项（全部失败已逐项回退）：服务仍停着，必须拉回来
		splash.Update(T("正在重启服务…"), 0.85)
		if !restartAndVerifyServer() {
			scopeNote = "\n\n注意：服务未能自动恢复启动，请到「常规」页点击「重启服务」。"
		}
	}

	// 收尾：清理快照；更新项提升 LKG（新基线），删除项清理 LKG 与残留禁用记录
	for _, t := range tasks {
		if t.recordOnly {
			continue
		}
		for _, dir := range t.locs {
			cleanupPluginProfileSnapshotSuffix(dir, t.snap)
			switch t.op {
			case "update":
				if t.ok {
					promoteProfileLkg(dir)
				}
			case "remove":
				if t.ok {
					clearLkgInDir(dir)
					_ = clearProfileDisabledRecord(dir, t.name)
				}
			}
		}
	}
	if splash != nil {
		splash.Close()
	}

	emitPluginBatchResults(tasks, scopeNote)
}

// rollbackPluginBatch 整批回退：逆序还原各项快照（后改的先还原，保证逐项回到各自操作前状态），
// 重启校验并重启后把所有成功项标记为失败。
func rollbackPluginBatch(active []*pluginOpTask, splash *SplashState) {
	splash.Update(T("批量操作失败，正在回退到操作前状态…"), 0.6)
	killServer()
	for i := len(active) - 1; i >= 0; i-- {
		restorePluginTaskSnapshot(active[i])
	}
	splash.Update(T("正在重启服务…"), 0.8)
	restartAndVerifyServer()
	const reason = "服务启动失败且自动禁用不兼容插件未能恢复（疑为核心故障），本项已回退"
	for _, t := range active {
		t.fail(reason)
	}
}

// batchTaskDirs 批内成功项涉及的 profile 目录（去重，供自愈定位范围）。
func batchTaskDirs(tasks []*pluginOpTask) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tasks {
		for _, dir := range t.locs {
			if !seen[dir] {
				seen[dir] = true
				out = append(out, dir)
			}
		}
	}
	return out
}

// allProfileDirs 全部 profile 目录（兜底禁用全部用户插件的范围，与单插件路径一致）。
func allProfileDirs() []string {
	var dirs []string
	for _, pf := range enumeratePluginProfiles() {
		dirs = append(dirs, pf.dir)
	}
	return dirs
}

// batchDisabledNote 自愈禁用后的说明文案。
func batchDisabledNote(disabled []PluginRow, all bool) string {
	names := disabledNames(disabled)
	if names == "" {
		return ""
	}
	if all {
		return "\n\n未能定位到具体的不兼容插件，已禁用全部已激活的用户插件以保证服务启动（可在关于页逐个重新启用）：\n· " + names
	}
	return "\n\n以下插件与当前版本不兼容，已自动禁用（保留记录，可在关于页检查更新后重新启用）：\n· " + names
}

// emitPluginBatchResults 逐项结果事件 + 一次性刷新列表 + 汇总弹窗。
// 事件先发（行内即时更新），刷新与汇总随后——单独用事件而不是只发 plugins:changed，
// 是因为失败项必须在行内留住原因（plugins:changed 只刷新版本列）。
func emitPluginBatchResults(tasks []*pluginOpTask, scopeNote string) {
	var lines []string
	for _, t := range tasks {
		switch {
		case t.ok && t.op == "update":
			ver := t.newVer
			if ver == "" {
				ver = t.target
			}
			label := fmt.Sprintf("已更新 %s", orDash(ver))
			if t.wasDisabled {
				if disabled, _ := pluginDisabledAcross(t.locs, t.name); disabled {
					label = fmt.Sprintf("已更新到 %s，但启用后仍不兼容，继续保持禁用", orDash(ver))
				} else {
					label += "，并已重新启用"
				}
			}
			if t.note != "" && t.note != label {
				label += "（" + t.note + "）"
			}
			lines = append(lines, "· "+t.name+" "+label)
			emitPluginOpDone(PluginOpDone{Name: t.name, Op: "update", OK: true, Version: t.newVer})
		case t.ok && t.op == "remove":
			label := "已删除"
			if t.note != "" {
				label = t.note
			}
			lines = append(lines, "· "+t.name+" "+label)
			emitPluginOpDone(PluginOpDone{Name: t.name, Op: "remove", OK: true})
		case t.ok:
			lines = append(lines, "· "+t.name+" 已完成")
		default:
			lines = append(lines, "· "+t.name+" 失败："+t.reason)
			emitPluginOpDone(PluginOpDone{Name: t.name, Op: t.op, OK: false, Reason: t.reason})
		}
		if t.ok && t.op == "update" && t.row.Source == "github" {
			lines = append(lines, "  （GitHub 来源插件更新依赖其 prepare 构建脚本，异常时请检查 profile 的 allowBuilds）")
		}
	}
	if appCtx != nil {
		wruntime.EventsEmit(appCtx, "plugins:changed", nil)
	}
	if len(lines) == 0 {
		return
	}
	failures := 0
	for _, t := range tasks {
		if !t.ok {
			failures++
		}
	}
	title := fmt.Sprintf("插件批量操作完成（%d 项）", len(tasks))
	if failures > 0 {
		title = fmt.Sprintf("插件批量操作完成（%d 项，%d 项失败已回退）", len(tasks), failures)
	}
	msg := title + "：\n" + strings.Join(lines, "\n") + "\n\n服务已重启。" + scopeNote
	showMessageBox(msg, appName)
}
