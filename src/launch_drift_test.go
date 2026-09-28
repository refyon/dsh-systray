package main

// 启动方式漂移询问的回归测试。
//
// 现场（2026-09-28）：用户在设置页把启动方式从 Desktop UI 改回 Web UI 后，2 秒轮询的
// checkLaunchTargetDrift 把这次「主动切换」误判成「桌面端被卸载后的回退」，在端口询问之外
// 又弹了一次「官方桌面端已不可用…是否现在启动后台服务？」，并且二次 startServiceBootstrap
// 与切换流程自己拉起的引导相互踩踏（切换被并发闸门丢弃）。修复见
// rememberLaunchTargetBaseline / checkLaunchTargetDrift 的 prevPref 判据。

import (
	"sync/atomic"
	"testing"
	"time"
)

// stubAskStartService 把漂移询问替换为可观测的桩，避免测试真的弹系统对话框。
func stubAskStartService(t *testing.T, answer bool) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	old := askStartServiceFn
	askStartServiceFn = func() bool {
		calls.Add(1)
		return answer
	}
	t.Cleanup(func() { askStartServiceFn = old })
	return &calls
}

// resetLaunchDriftBaseline 清空漂移基线（包级原子量会跨用例残留），用例结束复原。
func resetLaunchDriftBaseline(t *testing.T) {
	t.Helper()
	oldLaunch, _ := lastResolvedLaunch.Load().(string)
	oldPref, _ := lastResolvedPref.Load().(string)
	oldAsked := launchDriftAsked.Load()
	lastResolvedLaunch.Store("")
	lastResolvedPref.Store("")
	launchDriftAsked.Store(false)
	t.Cleanup(func() {
		lastResolvedLaunch.Store(oldLaunch)
		lastResolvedPref.Store(oldPref)
		launchDriftAsked.Store(oldAsked)
	})
}

// waitAskCalls 等待异步询问桩被调用到 want 次（判定本身是同步的，这里只验证确实问出去了）。
func waitAskCalls(t *testing.T, calls *atomic.Int32, want int32) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestLaunchDriftSkipsExplicitPrefChange 用户显式把偏好改成 Web UI 时不得弹「是否现在启动后台服务」：
// 该询问只服务于「偏好未变、桌面端消失」的实机漂移；显式切换由 setLaunchTarget 自己拉起服务。
func TestLaunchDriftSkipsExplicitPrefChange(t *testing.T) {
	resetLaunchDriftBaseline(t)
	calls := stubAskStartService(t, false)
	stubDesktopPref(t, launchTargetDesktop) // 桌面端已装：偏好 desktop 解析为 desktop

	rememberLaunchTargetBaseline() // 基线：偏好 desktop、解析 desktop

	// 用户在下拉里选 Web UI（此处只改偏好与解析结果，避免 setLaunchTarget 的真实副作用：
	// 写 config.json、拉起服务）——这正是修复前会误触发询问的时序。
	launchTargetPref = launchTargetWeb

	checkLaunchTargetDrift()

	if got := calls.Load(); got != 0 {
		t.Fatalf("显式改偏好不应触发漂移询问，实际询问 %d 次", got)
	}
	if launchDriftAsked.Load() {
		t.Fatal("显式改偏好不应把漂移标记为「已询问」")
	}
	if got, _ := lastResolvedPref.Load().(string); got != launchTargetWeb {
		t.Fatalf("基线偏好应更新为 web，实际 %q", got)
	}
}

// TestLaunchDriftAsksWhenDesktopDisappears 偏好未变、桌面端消失（实机漂移）时仍要询问一次，
// 且同一次漂移不重复询问（用户选「暂不启动」后不打扰）。
func TestLaunchDriftAsksWhenDesktopDisappears(t *testing.T) {
	resetLaunchDriftBaseline(t)
	calls := stubAskStartService(t, false) // 回答「暂不启动」：不拉起服务，避免真实副作用
	stubDesktopPref(t, launchTargetDesktop)
	rememberLaunchTargetBaseline() // 基线：偏好 desktop、解析 desktop

	stubDesktopInstalled(t, false) // 桌面端消失 → 解析回退 web

	checkLaunchTargetDrift()
	if !launchDriftAsked.Load() {
		t.Fatal("桌面端消失应询问一次「是否现在启动后台服务」")
	}
	if !waitAskCalls(t, calls, 1) {
		t.Fatalf("询问未发生（实际 %d 次）", calls.Load())
	}

	checkLaunchTargetDrift() // 同一次漂移的后续轮询
	if got := calls.Load(); got != 1 {
		t.Fatalf("同一次漂移不应重复询问，实际询问 %d 次", got)
	}
}

// TestRememberLaunchTargetBaseline 基线记录当前偏好与解析结果：setLaunchTarget 显式切换后
// 轮询看到的第一帧就是新状态（prev == cur），不会把它当成变化。
func TestRememberLaunchTargetBaseline(t *testing.T) {
	resetLaunchDriftBaseline(t)
	stubDesktopPref(t, launchTargetWeb)
	launchDriftAsked.Store(true) // 历史遗留：切换后应被复位，允许后续真实漂移重新询问

	rememberLaunchTargetBaseline()

	if got, _ := lastResolvedLaunch.Load().(string); got != launchTargetWeb {
		t.Fatalf("基线解析结果应为 web，实际 %q", got)
	}
	if got, _ := lastResolvedPref.Load().(string); got != launchTargetWeb {
		t.Fatalf("基线偏好应为 web，实际 %q", got)
	}
	if launchDriftAsked.Load() {
		t.Fatal("切换启动方式后应复位「已询问」标记")
	}
}
