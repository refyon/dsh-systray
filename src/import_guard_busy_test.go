package main

import (
	"strings"
	"testing"
	"time"
)

// waitQueueLen 轮询等待队列长度达到 want（消费队列的 worker 是异步的）。
func waitQueueLen(t *testing.T, want int) bool {
	t.Helper()
	for i := 0; i < 100; i++ {
		importQMu.Lock()
		n := len(importQueue)
		importQMu.Unlock()
		if n == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// waitWorkerOn 轮询等待 importWorkerOn 达到 want。
func waitWorkerOn(t *testing.T, want bool) bool {
	t.Helper()
	for i := 0; i < 200; i++ {
		importQMu.Lock()
		on := importWorkerOn
		importQMu.Unlock()
		if on == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// withFakeRestore 模拟「有恢复任务在跑」：worker 标记 + 队列占位，测试结束还原。
func withFakeRestore(t *testing.T) {
	t.Helper()
	importQMu.Lock()
	prevOn, prevQ := importWorkerOn, importQueue
	importWorkerOn, importQueue = true, []*importTask{{kind: "sessions"}}
	importQMu.Unlock()
	t.Cleanup(func() {
		importQMu.Lock()
		importWorkerOn, importQueue = prevOn, prevQ
		importQMu.Unlock()
	})
}

// TestRestoreBusyDuringRestore 恢复进行中必须报告占用（前端据此禁用「添加压缩包…」）。
func TestRestoreBusyDuringRestore(t *testing.T) {
	app := &App{}
	importQMu.Lock()
	empty := !importWorkerOn && len(importQueue) == 0
	importQMu.Unlock()
	if empty && app.RestoreBusy() {
		t.Fatal("空闲时 RestoreBusy 应为 false")
	}
	setImportRestoreHealing(true)
	t.Cleanup(func() { setImportRestoreHealing(false) })
	if !app.RestoreBusy() {
		t.Error("共享自愈阶段 RestoreBusy 应为 true")
	}
	withFakeRestore(t)
	if !app.RestoreBusy() {
		t.Error("恢复任务在跑时 RestoreBusy 应为 true")
	}
}

// TestImportPickRejectedDuringRestore 恢复进行中重新选择导入包必须被拒绝，
// 且不得改动当前导入项状态（importZipPath/importItems 保持旧包）。
// 回归（2026-09-13 现场）：恢复过程中重新添加压缩包会把导入项状态复位、
// 并与后台按旧包内容执行的恢复任务错位。
func TestImportPickRejectedDuringRestore(t *testing.T) {
	app := &App{}
	withFakeRestore(t)
	prevZip, prevItems := importZipPath, importItems
	importZipPath, importItems = "old.zip", []importItem{{Kind: "sessions", Zip: "sessions.zip"}}
	t.Cleanup(func() { importZipPath, importItems = prevZip, prevItems })

	res, err := app.ImportPick()
	if err == nil {
		t.Fatal("恢复进行中 ImportPick 应报错")
	}
	if res != nil {
		t.Errorf("拒绝时不应返回结果，got %+v", res)
	}
	if !strings.Contains(err.Error(), "恢复") {
		t.Errorf("错误文案应说明原因，got %q", err.Error())
	}
	if importZipPath != "old.zip" || len(importItems) != 1 {
		t.Errorf("被拒绝时不得改动导入项状态：zip=%q items=%d", importZipPath, len(importItems))
	}
}

// TestImportInflightReportsQueuedKinds 前端兜底计时到点时要能问出「哪一行还在跑」：
// 已发布结果（sent）的任务不再占用，未发布的按 kind 升序返回。
func TestImportInflightReportsQueuedKinds(t *testing.T) {
	app := &App{}
	importQMu.Lock()
	prevOn, prevQ := importWorkerOn, importQueue
	importWorkerOn = true
	importQueue = []*importTask{
		{kind: "sessions"},
		{kind: "plugins", sent: true}, // 已发 import:done：不再占用
		{kind: "files"},
	}
	importQMu.Unlock()
	t.Cleanup(func() {
		importQMu.Lock()
		importWorkerOn, importQueue = prevOn, prevQ
		importQMu.Unlock()
	})

	got := app.ImportInflight()
	want := []string{"files", "sessions"}
	if len(got) != len(want) {
		t.Fatalf("ImportInflight = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ImportInflight = %v, want %v", got, want)
		}
	}
}

// TestImportCancelRetiresSentEntry 队列里残留的「已发布结果」条目必须被退休清掉，
// 否则每次取消都命中它、界面永远等不到新结果
// （2026-09-13 现场：多次取消后一直提示恢复中，等不到完成）。
func TestImportCancelRetiresSentEntry(t *testing.T) {
	importQMu.Lock()
	prevOn, prevQ := importWorkerOn, importQueue
	importWorkerOn = true
	stale := &importTask{kind: "plugins", sent: true}
	importQueue = []*importTask{stale}
	importQMu.Unlock()
	t.Cleanup(func() {
		importQMu.Lock()
		importWorkerOn, importQueue = prevOn, prevQ
		importQMu.Unlock()
	})

	if got := importCancelKind("plugins"); got != "ok" {
		t.Errorf("取消残留条目应返回 ok，got %q", got)
	}
	importQMu.Lock()
	left := len(importQueue)
	importQMu.Unlock()
	if left != 0 {
		t.Errorf("残留条目应被移出队列，剩余 %d 条", left)
	}
}

// TestImportEnqueueEvictsCancelledPredecessor 同 kind 的旧任务已请求取消时，
// 新的恢复请求不应再被「已在处理中」拒绝（旧任务就此退休、由新任务接手）。
func TestImportEnqueueEvictsCancelledPredecessor(t *testing.T) {
	// 只保存/还原字段，避免复制含 mutex 的整结构（vet 会报 assignment copies lock value）
	prevKind, prevZip, prevCln := pendingRestore.kind, pendingRestore.innerZip, pendingRestore.innerCln
	pendingRestore.kind, pendingRestore.innerZip, pendingRestore.innerCln = "plugins", "inner.zip", func() {}
	t.Cleanup(func() {
		pendingRestore.kind, pendingRestore.innerZip, pendingRestore.innerCln = prevKind, prevZip, prevCln
	})

	importQMu.Lock()
	prevOn, prevQ := importWorkerOn, importQueue
	old := &importTask{kind: "plugins"}
	old.cancel.Store(true) // 已请求取消：正在回退/等待收尾
	importWorkerOn = true  // 假装有 worker 在（旧任务由它收尾）
	importQueue = []*importTask{old}
	importQMu.Unlock()
	t.Cleanup(func() {
		importQMu.Lock()
		importWorkerOn, importQueue = prevOn, prevQ
		importQMu.Unlock()
	})

	ok, why := importEnqueue("plugins", true)
	if !ok {
		t.Fatalf("旧任务已取消时应受理新恢复，却被拒绝：%s", why)
	}
	if !old.sent {
		t.Error("被退休的旧任务应标记为已发布（不再占用该 kind）")
	}
	importQMu.Lock()
	var mine *importTask
	for _, q := range importQueue {
		if q != old {
			mine = q
		}
	}
	importQMu.Unlock()
	if mine == nil || mine.kind != "plugins" || mine.cancel.Load() {
		t.Fatalf("新任务应已入队且未被取消：%+v", mine)
	}
}

// TestImportEnqueueHandsOffToNewWorker 交接窗口不能丢唤醒：
// 现有 worker 已排空队列、正在做批末收尾时（importWorkerOn 仍为 true、importWorkerEx 已置位），
// 新入队的恢复必须另起一个 worker 执行，而不是排进队列后无人处理
// （2026-09-14 现场实证：batch finish 只处理了 1 个任务，第二次「恢复」已被受理却永远没跑）。
func TestImportEnqueueHandsOffToNewWorker(t *testing.T) {
	// 用 sessions 走真实执行路径：restoreItem 读不存在的 inner zip 会快速失败并收尾
	importQMu.Lock()
	prevOn, prevEx, prevQ := importWorkerOn, importWorkerEx, importQueue
	importQueue = nil
	importWorkerOn, importWorkerEx = true, true // 旧 worker 已退休、正在收尾
	importQMu.Unlock()
	t.Cleanup(func() {
		importQMu.Lock()
		importWorkerOn, importWorkerEx, importQueue = prevOn, prevEx, prevQ
		importQMu.Unlock()
	})

	prevKind, prevZip, prevCln := pendingRestore.kind, pendingRestore.innerZip, pendingRestore.innerCln
	pendingRestore.kind, pendingRestore.innerZip, pendingRestore.innerCln = "sessions", "no-such-inner.zip", func() {}
	t.Cleanup(func() {
		pendingRestore.kind, pendingRestore.innerZip, pendingRestore.innerCln = prevKind, prevZip, prevCln
	})

	// 置 importPaused：让任务跳过「暂停后台服务」，测试不触碰真实服务
	importQMu.Lock()
	prevPaused := importPaused
	importPaused = true
	importQMu.Unlock()
	t.Cleanup(func() {
		importQMu.Lock()
		importPaused = prevPaused
		importQMu.Unlock()
	})

	ok, why := importEnqueue("sessions", false)
	if !ok {
		t.Fatalf("交接窗口内应受理恢复：%s", why)
	}
	importQMu.Lock()
	ex := importWorkerEx
	importQMu.Unlock()
	if ex {
		t.Error("新任务入队后应清除退休标记（由新 worker 接手）")
	}
	if !waitQueueLen(t, 0) {
		importQMu.Lock()
		left := len(importQueue)
		importQMu.Unlock()
		t.Fatalf("新任务应被新 worker 消费掉，队列仍剩 %d 条（丢唤醒）", left)
	}
	if !waitWorkerOn(t, false) {
		t.Error("执行完毕后 importWorkerOn 应回到 false")
	}
}

// TestImportRestoreRunningCoversBatchTail 队列刚清空、批末共享自愈尚未开始时，
// importRestoreRunning 必须仍为 true——否则这个窗口里重新选择压缩包会漏过去。
func TestImportRestoreRunningCoversBatchTail(t *testing.T) {
	withFakeRestore(t)
	importQMu.Lock()
	importQueue = nil // 模拟「任务执行完毕、worker 正在做批末收尾」
	importQMu.Unlock()
	if !importRestoreRunning() {
		t.Error("批末收尾期间 importRestoreRunning 应为 true")
	}
	// 收尾结束（worker 退出）后才允许重新选择
	importQMu.Lock()
	importWorkerOn = false
	importQMu.Unlock()
	if importRestoreRunning() {
		t.Error("worker 退出后应为 false")
	}
}
