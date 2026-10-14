package main

import (
	"testing"
	"time"
)

// TestServiceWatchdogStepRestartsAfterConsecutiveFailures 回归 2026-10-06：服务静默死亡后
// 必须能在若干次探测内被自动拉起（此前沿用的服务没有子进程句柄，死了完全无感）。
func TestServiceWatchdogStepRestartsAfterConsecutiveFailures(t *testing.T) {
	var st serviceWatchdogState
	now := time.Unix(1700000000, 0)

	// 单次失败不动作（吸收瞬时抖动）
	if got := st.step(now, false, false, false); got != watchdogNone {
		t.Fatalf("第一次失败不该立刻拉起：%v", got)
	}
	// 连续第二次失败 → 拉起
	if got := st.step(now, false, false, false); got != watchdogRestart {
		t.Fatalf("连续失败应触发自动拉起：%v", got)
	}
	// 恢复响应后计数清零
	if got := st.step(now, true, false, false); got != watchdogNone {
		t.Fatalf("服务恢复时不该动作：%v", got)
	}
	if st.fails != 0 {
		t.Fatalf("恢复后失败计数应清零，实际 %d", st.fails)
	}
}

// TestServiceWatchdogStepToleratesSlowStartWhileAlive 回归 2026-10-14：服务进程仍存活时
// （慢启动，如唤醒/登录后首分钟系统繁忙）不能按「连续 2 次无响应」判死——必须放宽到
// serviceWatchdogAliveFailures，否则会把一次慢启动误杀成「启动失败」，进而触发禁用全部插件、
// 逐个复验重新启用的一串无谓重启（现场共七次进程启动）。
func TestServiceWatchdogStepToleratesSlowStartWhileAlive(t *testing.T) {
	var st serviceWatchdogState
	now := time.Unix(1700000000, 0)

	for i := 1; i < serviceWatchdogAliveFailures; i++ {
		if got := st.step(now, false, false, true); got != watchdogNone {
			t.Fatalf("进程存活时第 %d 次无响应不该拉起：%v", i, got)
		}
	}
	// 达到存活阈值（长时间未就绪/卡死）→ 兜底拉起
	if got := st.step(now, false, false, true); got != watchdogRestart {
		t.Fatalf("进程存活但长时间无响应应兜底拉起：%v", got)
	}

	// 进程已退出（静默死亡）→ 维持原有 2 次判死，不因放宽而漏掉真正的死亡
	var dead serviceWatchdogState
	dead.step(now, false, false, false)
	if got := dead.step(now, false, false, false); got != watchdogRestart {
		t.Fatalf("进程已退出仍应 2 次判死：%v", got)
	}
}

// TestServiceWatchdogStepResetsOnRespondingWhileAlive 慢启动中途就绪：存活阈值计数必须清零，
// 不能把「启动期间的历史失败」带到后续运行期。
func TestServiceWatchdogStepResetsOnRespondingWhileAlive(t *testing.T) {
	var st serviceWatchdogState
	now := time.Unix(1700000000, 0)
	for i := 0; i < serviceWatchdogAliveFailures-1; i++ {
		st.step(now, false, false, true)
	}
	if got := st.step(now, true, false, true); got != watchdogNone {
		t.Fatalf("就绪后不该动作：%v", got)
	}
	if st.fails != 0 {
		t.Fatalf("就绪后失败计数应清零，实际 %d", st.fails)
	}
}

// TestServiceWatchdogStepRespectsSkip 用户主动停服 / 退出中 / 更新收尾中：一律不探查不拉起。
func TestServiceWatchdogStepRespectsSkip(t *testing.T) {
	var st serviceWatchdogState
	now := time.Unix(1700000000, 0)
	for i := 0; i < 5; i++ {
		if got := st.step(now, false, true, false); got != watchdogNone {
			t.Fatalf("skip 状态下不该动作：%v", got)
		}
	}
	if st.fails != 0 || st.restarts != 0 {
		t.Fatalf("skip 不该累计失败或拉起：fails=%d restarts=%d", st.fails, st.restarts)
	}
}

// TestServiceWatchdogStepCooldownAndCap 冷却期不重复拉起；达到上限后只告警（不重启风暴）。
func TestServiceWatchdogStepCooldownAndCap(t *testing.T) {
	var st serviceWatchdogState
	base := time.Unix(1700000000, 0)

	// 第一次拉起（连续两次失败）
	st.step(base, false, false, false)
	if got := st.step(base, false, false, false); got != watchdogRestart {
		t.Fatalf("应首次拉起：%v", got)
	}
	if st.restarts != 1 {
		t.Fatalf("应记一次拉起，实际 %d", st.restarts)
	}
	// 冷却期内：连续失败也不动作
	for i := 1; i <= 3; i++ {
		if got := st.step(base.Add(time.Duration(i)*10*time.Second), false, false, false); got != watchdogNone {
			t.Fatalf("冷却期内不该重复拉起（第 %d 次）：%v", i, got)
		}
	}
	// 冷却到期后（失败计数已累计）→ 再拉起一次
	if got := st.step(base.Add(serviceWatchdogCooldown+time.Second), false, false, false); got != watchdogRestart {
		t.Fatalf("冷却结束后应可再次拉起：%v", got)
	}
	if st.restarts != 2 {
		t.Fatalf("应记第二次拉起，实际 %d", st.restarts)
	}

	// 达到上限：只告警，且告警本身也有冷却
	st.restarts = serviceWatchdogMaxRestarts
	at := base.Add(time.Hour)
	st.step(at, false, false, false)
	if got := st.step(at.Add(time.Second), false, false, false); got != watchdogGiveUp {
		t.Fatalf("达到上限后应告警而非重启：%v", got)
	}
	st.fails = serviceWatchdogFailures
	if got := st.step(at.Add(2*time.Second), false, false, false); got != watchdogNone {
		t.Fatalf("告警也应有冷却，实际 %v", got)
	}
}
