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
	if got := st.step(now, false, false); got != watchdogNone {
		t.Fatalf("第一次失败不该立刻拉起：%v", got)
	}
	// 连续第二次失败 → 拉起
	if got := st.step(now, false, false); got != watchdogRestart {
		t.Fatalf("连续失败应触发自动拉起：%v", got)
	}
	// 恢复响应后计数清零
	if got := st.step(now, true, false); got != watchdogNone {
		t.Fatalf("服务恢复时不该动作：%v", got)
	}
	if st.fails != 0 {
		t.Fatalf("恢复后失败计数应清零，实际 %d", st.fails)
	}
}

// TestServiceWatchdogStepRespectsSkip 用户主动停服 / 退出中 / 更新收尾中：一律不探查不拉起。
func TestServiceWatchdogStepRespectsSkip(t *testing.T) {
	var st serviceWatchdogState
	now := time.Unix(1700000000, 0)
	for i := 0; i < 5; i++ {
		if got := st.step(now, false, true); got != watchdogNone {
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
	st.step(base, false, false)
	if got := st.step(base, false, false); got != watchdogRestart {
		t.Fatalf("应首次拉起：%v", got)
	}
	if st.restarts != 1 {
		t.Fatalf("应记一次拉起，实际 %d", st.restarts)
	}
	// 冷却期内：连续失败也不动作
	for i := 1; i <= 3; i++ {
		if got := st.step(base.Add(time.Duration(i)*10*time.Second), false, false); got != watchdogNone {
			t.Fatalf("冷却期内不该重复拉起（第 %d 次）：%v", i, got)
		}
	}
	// 冷却到期后（失败计数已累计）→ 再拉起一次
	if got := st.step(base.Add(serviceWatchdogCooldown+time.Second), false, false); got != watchdogRestart {
		t.Fatalf("冷却结束后应可再次拉起：%v", got)
	}
	if st.restarts != 2 {
		t.Fatalf("应记第二次拉起，实际 %d", st.restarts)
	}

	// 达到上限：只告警，且告警本身也有冷却
	st.restarts = serviceWatchdogMaxRestarts
	at := base.Add(time.Hour)
	st.step(at, false, false)
	if got := st.step(at.Add(time.Second), false, false); got != watchdogGiveUp {
		t.Fatalf("达到上限后应告警而非重启：%v", got)
	}
	st.fails = serviceWatchdogFailures
	if got := st.step(at.Add(2*time.Second), false, false); got != watchdogNone {
		t.Fatalf("告警也应有冷却，实际 %v", got)
	}
}
