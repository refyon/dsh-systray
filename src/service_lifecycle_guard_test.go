package main

// 服务生命周期守护的让位规则回归（2026-10-08 现场）：
// 插件同步应用会「停服 → 改文件 → 自己拉起并做启动校验」，看门狗/退出守护此时若也判定
// 「无响应/服务已死」并自行重启，就会把正在校验的那个进程带走——同步应用于是误报
// 「与 dsh-cost-meter@1.8.15 与当前服务不兼容，已回退」，用户再点一次才成功。
// 判据：服务生命周期由正在改服务的那个操作负责，看门狗与退出守护都必须让位。

import (
	"testing"
	"time"
)

func TestWatchdogSkipWhileAnotherOperationRuns(t *testing.T) {
	prevQuit, prevQuitting, prevStop, prevRestarting := quitRequested.Load(), quitting.Load(), serverStopByTray.Load(), watchdogRestarting.Load()
	prevBusy := harnessOpBusy.Load()
	t.Cleanup(func() {
		quitRequested.Store(prevQuit)
		quitting.Store(prevQuitting)
		serverStopByTray.Store(prevStop)
		watchdogRestarting.Store(prevRestarting)
		harnessOpBusy.Store(prevBusy)
	})
	reset := func() {
		quitRequested.Store(false)
		quitting.Store(false)
		serverStopByTray.Store(false)
		watchdogRestarting.Store(false)
		harnessOpBusy.Store(false)
		accountSetApplying(false)
	}
	reset()
	if watchdogSkip() {
		t.Fatal("空闲状态不该让位")
	}

	harnessOpBusy.Store(true)
	if !watchdogSkip() {
		t.Fatal("harness 更新/重置/同步应用在跑时必须让位")
	}
	harnessOpBusy.Store(false)

	accountSetApplying(true)
	if !watchdogSkip() {
		t.Fatal("「重启生效」应用流程在跑时必须让位")
	}
	accountSetApplying(false)

	serverStopByTray.Store(true)
	if !watchdogSkip() {
		t.Fatal("托盘主动停服后（就绪前）必须让位")
	}
	serverStopByTray.Store(false)

	watchdogRestarting.Store(true)
	if !watchdogSkip() {
		t.Fatal("看门狗自己正在拉起时必须让位")
	}
	watchdogRestarting.Store(false)
	reset()
}

func TestWatchdogStepSkipsDuringSyncApply(t *testing.T) {
	prevQuit, prevQuitting, prevStop, prevRestarting := quitRequested.Load(), quitting.Load(), serverStopByTray.Load(), watchdogRestarting.Load()
	prevBusy := harnessOpBusy.Load()
	t.Cleanup(func() {
		quitRequested.Store(prevQuit)
		quitting.Store(prevQuitting)
		serverStopByTray.Store(prevStop)
		watchdogRestarting.Store(prevRestarting)
		harnessOpBusy.Store(prevBusy)
		accountSetApplying(false)
	})
	quitRequested.Store(false)
	quitting.Store(false)
	serverStopByTray.Store(false)
	watchdogRestarting.Store(false)
	harnessOpBusy.Store(true) // 同步应用在跑（account_apply.go 全程持有）
	defer harnessOpBusy.Store(false)

	var st serviceWatchdogState
	for i := 0; i < 4; i++ {
		if got := st.step(time.Now(), false, watchdogSkip()); got != watchdogNone {
			t.Fatalf("操作进行中不该自动拉起（第 %d 次）：%v", i, got)
		}
	}
	if st.fails != 0 || st.restarts != 0 {
		t.Fatalf("让位期间不该累计失败或拉起：fails=%d restarts=%d", st.fails, st.restarts)
	}
}

// TestMarkServerResponsiveClearsStopIntent 停服意图只在「服务确认在响应」时解除：
// startServer 不再解除（进程刚 spawn、还没监听的那段窗口是看门狗误判高发区）。
func TestMarkServerResponsiveClearsStopIntent(t *testing.T) {
	prev := serverStopByTray.Load()
	t.Cleanup(func() { serverStopByTray.Store(prev) })

	serverStopByTray.Store(true)
	markServerResponsive()
	if serverStopByTray.Load() {
		t.Fatal("服务确认在响应后应解除停服意图")
	}
}
