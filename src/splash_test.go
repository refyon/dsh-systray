package main

import "testing"

// 重开窗口时的进度视图还原：进度流程进行中，托盘「设置」必须把进度视图补回来，
// 而不是切设置页（2026-09-17 用户反馈：下载依赖时关窗再开，只剩设置页看不到下载进度）。
func TestReplayActiveSplashOnlyWhileFlowActive(t *testing.T) {
	defer splashActive.Store(false)

	// 无流程在跑：不补发（调用方按正常流程发 ui:show-settings）
	splashActive.Store(false)
	splashLast.Store(splashProgress{})
	if replayActiveSplash() {
		t.Fatal("没有进度流程时不应补发进度事件")
	}

	// 流程进行中：补发最近一次进度文案与百分比
	splash := startSplash("正在下载运行环境…")
	splash.Update("正在下载运行环境…", 0.42)
	snap, ok := splashLast.Load().(splashProgress)
	if !ok || snap.text != "正在下载运行环境…" || snap.pct != 0.42 || snap.phase != "startup" {
		t.Fatalf("进度快照不符：%+v", snap)
	}
	if !replayActiveSplash() {
		t.Fatal("进度流程进行中应补发进度事件")
	}

	// 流程收尾后：不再补发（进度已结束，窗口重开就该看到设置页）
	splash.Close()
	if replayActiveSplash() {
		t.Fatal("进度流程结束后不应补发进度事件")
	}

	// 新流程刚起步、尚无文案：不补发（否则会把上一轮的旧文案补出来）
	startSplash("")
	if replayActiveSplash() {
		t.Fatal("流程刚起步且无文案时不应补发进度事件")
	}
}

// 同步「重启生效」的静默通道复用外层应用流程的进度视图：不得改相位（否则前端从「应用同步改动」
// 视图切走、取消按钮消失）、不得关掉外层流程、不得隐藏窗口；内层进度文案要转发给外层视图，
// 使窗口重开时仍能补发（2026-09-22 现场问题②：harness 同步完进度窗口自动关闭、后半程进度无处可看）。
func TestResetFlowSplashReusesOuterFlow(t *testing.T) {
	prevPhase, _ := splashPhase.Load().(string)
	prevActive := splashActive.Load()
	prevLast, _ := splashLast.Load().(splashProgress) // 未写过时为零值（Store 不接受 nil）
	defer func() {
		splashPhase.Store(prevPhase)
		splashActive.Store(prevActive)
		splashLast.Store(prevLast)
	}()

	outer := startSplash("正在应用同步改动…")
	outer.KeepWindow = true // 同步应用流程要求全过程不自动关窗
	setSplashPhase("sync")

	splash, own := resetFlowSplash(true)
	if own {
		t.Fatal("外层应用流程已有进度视图时不应自建（自建会把相位打回 startup 并在 harness 步收尾关窗）")
	}
	splash.Update("正在全新安装 1.2.3…", 0.55)
	if cur, _ := splashPhase.Load().(string); cur != "sync" {
		t.Fatalf("复用路径不得改变相位，实际 %q", cur)
	}
	if snap, _ := splashLast.Load().(splashProgress); snap.text != "正在全新安装 1.2.3…" || snap.pct != 0.55 {
		t.Fatalf("内层进度应转发给外层视图：%+v", snap)
	}
	splash.Close() // 内层收尾：外层仍在跑，视图与流程都不能被关掉
	if !splashActive.Load() {
		t.Fatal("内层收尾不得关掉外层进度流程")
	}
	if !replayActiveSplash() {
		t.Fatal("外层流程仍在跑时应能补发进度视图（窗口重开可恢复）")
	}
	outer.Close()
	if splashActive.Load() {
		t.Fatal("外层收尾后进度流程应结束")
	}
}

// 常规「重置服务」入口（非静默）与无外层流程的静默通道仍然自建进度视图，并由自己收尾。
func TestResetFlowSplashOwnsViewWhenNoOuterFlow(t *testing.T) {
	prevPhase, _ := splashPhase.Load().(string)
	prevActive := splashActive.Load()
	prevLast, _ := splashLast.Load().(splashProgress)
	defer func() {
		splashPhase.Store(prevPhase)
		splashActive.Store(prevActive)
		splashLast.Store(prevLast)
	}()
	splashActive.Store(false)

	splash, own := resetFlowSplash(false)
	if !own {
		t.Fatal("常规入口应自建进度视图")
	}
	splash.Close()
	if splashActive.Load() {
		t.Fatal("自建视图收尾后进度流程应结束")
	}

	splash2, own2 := resetFlowSplash(true) // 静默但无外层流程（直接调用）→ 同样自建
	if !own2 {
		t.Fatal("无外层进度流程时静默通道应自建视图")
	}
	splash2.Close()
	if splashActive.Load() {
		t.Fatal("自建视图收尾后进度流程应结束")
	}
}
