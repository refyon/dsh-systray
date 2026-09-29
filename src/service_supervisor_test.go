package main

// 后台服务退出守护（快速自动重启）的回归测试，见 service_supervisor.go。
//
// 现场依据：DSH 在 HMR 整体重载（框架依赖变化）与插件管理器判定 restart-required（替换已解析的
// 包版本）时会调用宿主注入的 loader.exit() → 宿主优雅退出进程，等着外壳把它拉起来。托盘此前没有
// 退出观察者，这类退出等于服务停摆（「Web UI 打不开」，只能手动重启）。

import (
	"errors"
	"testing"
	"time"
)

// stubSupervisor 替换守护的两个执行缝（run/verify）并隔离所有相关全局状态，
// 返回 runCalls / verifyCalls 计数指针。verifyOK/verifyMsg 决定拉起校验的结果。
func stubSupervisor(t *testing.T, verifyOK bool, verifyMsg string) (runCalls, verifyCalls *int) {
	t.Helper()
	useTempLogDir(t)

	prevRun, prevVerify := autoRestartRun, autoRestartVerify
	prevReady, prevFailed := serverReady.Load(), serviceFailed.Load()
	prevStop, prevQuit, prevQuitting := serverStopByTray.Load(), quitRequested.Load(), quitting.Load()
	prevGen, prevPref := serverStartGen.Load(), launchTargetPref
	autoRestartMu.Lock()
	prevAttempts := append([]time.Time(nil), autoRestartAttempts...)
	autoRestartAttempts = nil
	autoRestartMu.Unlock()
	autoRestartInFlight.Store(false)

	t.Cleanup(func() {
		autoRestartRun, autoRestartVerify = prevRun, prevVerify
		serverReady.Store(prevReady)
		serviceFailed.Store(prevFailed)
		serverStopByTray.Store(prevStop)
		quitRequested.Store(prevQuit)
		quitting.Store(prevQuitting)
		serverStartGen.Store(prevGen)
		launchTargetPref = prevPref
		autoRestartMu.Lock()
		autoRestartAttempts = prevAttempts
		autoRestartMu.Unlock()
		autoRestartInFlight.Store(false)
	})

	launchTargetPref = launchTargetWeb // 确定性：不受本机是否装了官方桌面端影响
	run, verify := 0, 0
	autoRestartRun = func(error) { run++ }
	autoRestartVerify = func() (bool, string) { verify++; return verifyOK, verifyMsg }
	return &run, &verify
}

// TestNoteServerExitGuards 守护的准入条件：只有「就绪之后、当前代次的进程、非托盘停服、非退出流程」
// 才拉起，其余情形让位给既有链路（启动校验 / 停服方自己重启 / 退出流程）。
func TestNoteServerExitGuards(t *testing.T) {
	run, _ := stubSupervisor(t, true, "")
	serverReady.Store(true)
	serverStopByTray.Store(false)
	quitRequested.Store(false)
	quitting.Store(false)
	serverStartGen.Store(7)

	noteServerExit(7, errors.New("exit status 0"))
	if *run != 1 {
		t.Fatalf("就绪后的意外退出应触发守护，实际 %d 次", *run)
	}

	serverStopByTray.Store(true)
	noteServerExit(7, errors.New("exit status 1"))
	if *run != 1 {
		t.Fatal("托盘主动停服（更新/装插件/导入）不应触发守护")
	}
	serverStopByTray.Store(false)

	noteServerExit(6, errors.New("exit status 1"))
	if *run != 1 {
		t.Fatal("旧代次退出（kill→start 正常交接）不应触发守护")
	}

	serverReady.Store(false)
	noteServerExit(7, errors.New("exit status 1"))
	if *run != 1 {
		t.Fatal("尚未就绪时的退出不应触发守护（启动失败由既有链路负责）")
	}
	serverReady.Store(true)

	quitRequested.Store(true)
	noteServerExit(7, errors.New("exit status 1"))
	if *run != 1 {
		t.Fatal("退出流程中不应触发守护")
	}
}

// TestReserveAutoRestartWindow 滑动窗口额度：窗口内最多 autoRestartMax 次，窗口滑过后重新计数。
func TestReserveAutoRestartWindow(t *testing.T) {
	stubSupervisor(t, true, "")
	now := time.Now()
	for i := 1; i <= autoRestartMax; i++ {
		n, ok := reserveAutoRestart(now)
		if !ok || n != i {
			t.Fatalf("第 %d 次额度应被允许（n=%d ok=%v）", i, n, ok)
		}
	}
	if _, ok := reserveAutoRestart(now); ok {
		t.Fatal("超出上限应拒绝")
	}
	n, ok := reserveAutoRestart(now.Add(autoRestartWindow + time.Second))
	if !ok || n != 1 {
		t.Fatalf("窗口滑过后应重新计数（n=%d ok=%v）", n, ok)
	}
}

// TestAutoRestartDelay 退避：首次立即拉起，其后按表等待（不越界 panic）。
func TestAutoRestartDelay(t *testing.T) {
	if d := autoRestartDelay(1); d != 0 {
		t.Fatalf("首次自动重启应无等待，实际 %s", d)
	}
	if d := autoRestartDelay(2); d != time.Second {
		t.Fatalf("第二次等待应为 1s，实际 %s", d)
	}
	if d := autoRestartDelay(autoRestartMax + 5); d != 0 {
		t.Fatalf("超出退避表应退化为 0（有界额度已拦住），实际 %s", d)
	}
}

// TestAutoRestartServiceSuccess 拉起成功：置就绪、清失败状态。
func TestAutoRestartServiceSuccess(t *testing.T) {
	_, verify := stubSupervisor(t, true, "")
	serverReady.Store(false)
	serviceFailed.Store(true)

	autoRestartService(errors.New("exit status 0"))

	if *verify != 1 {
		t.Fatalf("应执行一次拉起校验，实际 %d 次", *verify)
	}
	if !serverReady.Load() {
		t.Fatal("自动重启成功后应置为就绪")
	}
	if serviceFailed.Load() {
		t.Fatal("自动重启成功后应清除失败状态")
	}
}

// TestAutoRestartServiceGiveUp 额度耗尽：不再拉起，写入失败状态并落日志（有界重试，不无限重拉）。
func TestAutoRestartServiceGiveUp(t *testing.T) {
	_, verify := stubSupervisor(t, true, "")
	for i := 0; i < autoRestartMax; i++ {
		if _, ok := reserveAutoRestart(time.Now()); !ok {
			t.Fatalf("第 %d 次额度应被允许", i+1)
		}
	}

	autoRestartService(errors.New("exit status 1"))

	if *verify != 0 {
		t.Fatal("超出额度后不应再拉起服务")
	}
	if !serviceFailed.Load() {
		t.Fatal("超出额度后应写入失败状态（托盘可显示原因）")
	}
}

// TestAutoRestartServiceFailure verify 失败：写入失败状态，不置就绪。
func TestAutoRestartServiceFailure(t *testing.T) {
	_, verify := stubSupervisor(t, false, "服务未在预期时间内就绪（timeout）。")
	serverReady.Store(true)

	autoRestartService(errors.New("exit status 1"))

	if *verify != 1 {
		t.Fatalf("应尝试一次拉起，实际 %d 次", *verify)
	}
	if !serviceFailed.Load() {
		t.Fatal("拉起失败应写入失败状态")
	}
	if serverReady.Load() {
		t.Fatal("拉起失败不应置就绪")
	}
}
