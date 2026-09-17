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
