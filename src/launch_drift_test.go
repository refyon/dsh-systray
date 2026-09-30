package main

// 启动方式漂移询问的回归测试。
//
// 现场（2026-09-28）：用户在设置页把启动方式从 Desktop UI 改回 Web UI 后，2 秒轮询的
// checkLaunchTargetDrift 把这次「主动切换」误判成「桌面端被卸载后的回退」，在端口询问之外
// 又弹了一次「官方桌面端已不可用…是否现在启动后台服务？」，并且二次 startServiceBootstrap
// 与切换流程自己拉起的引导相互踩踏（切换被并发闸门丢弃）。修复见
// rememberLaunchTargetBaseline / checkLaunchTargetDrift 的 prevPref 判据。
//
// 现场（2026-09-30）：自动检测（auto）在运行期翻转启动方式时（装上/卸掉官方桌面端），托盘
// 「打开」菜单项只更新显隐与可点，文案停留在上一次写入时的方式——「菜单写着 Web UI，点开却是
// 桌面端」。修复见 refreshServiceMenu 的文案同步（判据 trayTextsNeedSync）与 web → desktop
// 方向的停服询问。

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// stubAskStopServiceForDrift 把「是否停止后台 Web 服务」的漂移询问替换为可观测的桩。
func stubAskStopServiceForDrift(t *testing.T, answer bool) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	old := askStopServiceForDriftFn
	askStopServiceForDriftFn = func() bool {
		calls.Add(1)
		return answer
	}
	t.Cleanup(func() { askStopServiceForDriftFn = old })
	return &calls
}

// stubRunningService 用本地会应答的 HTTP 服务顶替真实 dsh web 进程（serverResponding 只看
// 状态码 < 500），让 resolveRunningService 判定「服务在跑」。返回后关服务并复原端口变量。
func stubRunningService(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	oldPort, oldURL, oldStarted := port, webURL, serverStartedPort
	port, webURL, serverStartedPort = 80, srv.URL, 0
	t.Cleanup(func() {
		srv.Close()
		port, webURL, serverStartedPort = oldPort, oldURL, oldStarted
	})
}

// stubStoppedService 指向本机未监听的端口：resolveRunningService 快速判定「未运行」。
func stubStoppedService(t *testing.T) {
	t.Helper()
	oldPort, oldURL, oldStarted := port, webURL, serverStartedPort
	port, webURL, serverStartedPort = 9, "http://127.0.0.1:9/", 0
	t.Cleanup(func() { port, webURL, serverStartedPort = oldPort, oldURL, oldStarted })
}

// stubLaunchPref 固定启动方式偏好（含复原）。
func stubLaunchPref(t *testing.T, pref string) {
	t.Helper()
	old := launchTargetPref
	launchTargetPref = pref
	t.Cleanup(func() { launchTargetPref = old })
}

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

// TestLaunchDriftAsksStopServiceWhenDesktopAppears 偏好没变（auto）、检测到桌面端出现且后台
// 服务仍在跑：询问一次「是否停止后台 Web 服务」，且同一次漂移不重复询问。
func TestLaunchDriftAsksStopServiceWhenDesktopAppears(t *testing.T) {
	resetLaunchDriftBaseline(t)
	stubAskStartService(t, false)                 // 反方向询问不应被触发
	calls := stubAskStopServiceForDrift(t, false) // 回答「保留服务」：不真停服
	stubRunningService(t)
	stubLaunchPref(t, launchTargetAuto)

	stubDesktopInstalled(t, false) // 基线：auto + 未装桌面端 → 解析 web
	rememberLaunchTargetBaseline()

	stubDesktopInstalled(t, true) // 桌面端出现 → 解析 desktop

	checkLaunchTargetDrift()
	if !waitAskCalls(t, calls, 1) {
		t.Fatalf("桌面端出现且服务在跑：应询问「是否停止后台 Web 服务」（实际 %d 次）", calls.Load())
	}

	checkLaunchTargetDrift() // 同一次漂移的后续轮询
	if got := calls.Load(); got != 1 {
		t.Fatalf("同一次漂移不应重复询问，实际询问 %d 次", got)
	}
}

// TestLaunchDriftDesktopAppearsSkipsAskWhenServiceStopped 桌面端出现但服务没在跑（desktop 形态
// 本就该如此）：不打扰用户。
func TestLaunchDriftDesktopAppearsSkipsAskWhenServiceStopped(t *testing.T) {
	resetLaunchDriftBaseline(t)
	calls := stubAskStopServiceForDrift(t, false)
	stubStoppedService(t)
	stubLaunchPref(t, launchTargetAuto)

	stubDesktopInstalled(t, false)
	rememberLaunchTargetBaseline()
	stubDesktopInstalled(t, true)

	checkLaunchTargetDrift()
	checkLaunchTargetDrift()
	if got := calls.Load(); got != 0 {
		t.Fatalf("服务没在跑不应询问，实际询问 %d 次", got)
	}
}

// TestTrayTextsNeedSync 托盘「打开」文案与解析结果脱节时才需要改写（自动检测运行期翻转的
// 回归判据）：auto 下桌面端装上/卸掉后，菜单项必须跟着换文案，已同步则不再重复改写
// （2 秒轮询不该每轮都打原生 SetTitle）。
func TestTrayTextsNeedSync(t *testing.T) {
	// 包级原子量：仅在已有值时复原（Load 到 nil 不能回存，见 sync/atomic.Value）。
	if old := lastTrayLaunchMode.Load(); old != nil {
		t.Cleanup(func() { lastTrayLaunchMode.Store(old) })
	}
	stubLaunchPref(t, launchTargetAuto)

	stubDesktopInstalled(t, true) // auto → 解析 desktop
	lastTrayLaunchMode.Store(launchTargetWeb)
	if !trayTextsNeedSync() {
		t.Fatal("解析结果已是 desktop、菜单文案仍标 web：应判定需要同步")
	}
	if got := trayLaunchModeTag(); got != launchTargetDesktop {
		t.Fatalf("解析结果标签应为 desktop，实际 %q", got)
	}

	lastTrayLaunchMode.Store(launchTargetDesktop)
	if trayTextsNeedSync() {
		t.Fatal("文案已同步：不应反复改写")
	}

	stubDesktopInstalled(t, false) // 桌面端消失 → 解析回退 web（反向同样要同步）
	if !trayTextsNeedSync() {
		t.Fatal("解析结果回退 web、菜单文案仍标 desktop：应判定需要同步")
	}
}
