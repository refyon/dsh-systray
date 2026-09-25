package main

// 「主动停服不得判成启动失败」的回归测试（2026-09-25 全新首启实证）：
// 首装完成后冷启动校验仍在窗口内（存在 LKG 时用 60s 加长窗口且不做提前通过），此时用户点
// 「重启生效」，同步应用为装插件 killServer() → 校验器看到进程退出 → 判「启动失败，启动日志
// 存在加载错误（版本/插件不兼容）」→ 自愈无嫌疑、回退无备份 → 弹「启动失败…仍未能启动」，
// 并把 serviceFailed 写进服务状态；而同步应用本身完全正常（后续照样装完插件并成功）。
//
// 两道防线：①停服意图/代次让校验让位（不再误判）；②失败原因区分「加载错误」与「进程异常退出」
// （不再把主动停服/崩溃一律标成插件不兼容）。

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// resetStopIntent 复位停服意图/代次（测试间共享包级状态），测试结束还原。
func resetStopIntent(t *testing.T) {
	t.Helper()
	prevGen := serverStartGen.Load()
	prevStop := serverStopByTray.Load()
	serverStopByTray.Store(false)
	t.Cleanup(func() {
		serverStopByTray.Store(prevStop)
		serverStartGen.Store(prevGen)
	})
}

// TestVerifyServerBootSupersededByTrayStop 托盘主动停服（同步应用/插件批处理/更新/重置/导入）
// 期间的进程退出必须判「让位」：既不算失败（不报错、不回退），也不提升 LKG。
func TestVerifyServerBootSupersededByTrayStop(t *testing.T) {
	dir := useTempLogDir(t)
	appendServerLine(t, dir, "dsh web: http://127.0.0.1:3080/?token=abc")
	resetStopIntent(t)

	exitCh := make(chan error, 1)
	exitCh <- errors.New("exit status 1")
	serverStopByTray.Store(true) // 模拟 killServer()

	start := time.Now()
	if got := verifyServerBootPolling(0, exitCh, 30*time.Second, false); got != bootSuperseded {
		t.Fatalf("主动停服期间的进程退出应判让位，得到 %v", got)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("让位未快速返回：%s", el)
	}
}

// TestVerifyServerBootExitedWithoutStopIntentFails 无人主动停服的进程退出仍是失败（真崩溃照旧走回退）。
func TestVerifyServerBootExitedWithoutStopIntentFails(t *testing.T) {
	dir := useTempLogDir(t)
	appendServerLine(t, dir, "dsh web: http://127.0.0.1:3080/?token=abc")
	resetStopIntent(t)

	exitCh := make(chan error, 1)
	exitCh <- errors.New("exit status 1")
	if got := verifyServerBootPolling(0, exitCh, 30*time.Second, false); got != bootFailed {
		t.Fatalf("无停服意图的进程退出应判失败，得到 %v", got)
	}
}

// TestServerExitIsSuperseded 让位判据：①停服标记置位（killServer 已停服、代次可能还没变）；
// ②代次已变（kill→装包→重启 快到一个轮询步长内完成，标记已被 startServer 清零）；
// ③两者都无 = 真崩溃，必须判失败（否则真故障会被静默吞掉）。
func TestServerExitIsSuperseded(t *testing.T) {
	resetStopIntent(t)
	const gen = 7

	serverStopByTray.Store(true)
	if !serverExitIsSuperseded(gen) {
		t.Fatal("停服标记置位应判让位")
	}

	serverStopByTray.Store(false)
	serverStartGen.Store(gen)
	if serverExitIsSuperseded(gen) {
		t.Fatal("无停服标记、代次未变不应判让位")
	}

	serverStartGen.Store(gen + 1)
	if !serverExitIsSuperseded(gen) {
		t.Fatal("代次已变（新进程已接管）应判让位")
	}
}

// TestColdStartBootFailureReason 失败原因必须区分「加载错误」与「进程异常退出」：
// 只有命中启动日志特征才指向插件/版本不兼容，否则是崩溃或主动停服。
func TestColdStartBootFailureReason(t *testing.T) {
	dir := useTempLogDir(t)
	appendServerLine(t, dir, "dsh web: http://127.0.0.1:3080/?token=abc")
	if got := coldStartBootFailureReason(0); got == "启动日志存在加载错误（版本/插件不兼容）" {
		t.Fatalf("无加载错误特征的窗口不应报加载错误：%s", got)
	}
	appendServerLine(t, dir, "Cannot find package '@deepseek-ai/dsh-llm'")
	if got := coldStartBootFailureReason(0); got != "启动日志存在加载错误（版本/插件不兼容）" {
		t.Fatalf("命中加载错误特征应报加载错误，得到 %s", got)
	}
}

// TestClearDanglingLkgMarker 悬空标记（标记在、备份全无）必须清掉：否则冷启动走加长窗口白等 60s，
// 失败路径还会做一次注定 nothing-to-restore 的回退；存在备份（真能回退）时必须保留。
func TestClearDanglingLkgMarker(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())  // lkgMarkerPath 落在临时配置目录
	t.Setenv("DSH_HOME", t.TempDir()) // profiles 枚举不触碰真实数据
	prevHarness := harnessDir
	harnessDir = t.TempDir()
	t.Cleanup(func() { harnessDir = prevHarness })

	writeLkgMarker(lkgMarker{HarnessVersion: "0.1.7-rc.1"})
	if !clearDanglingLkgMarker() {
		t.Fatal("标记在、备份全无时应清理悬空标记")
	}
	if _, ok := readLkgMarker(); ok {
		t.Fatal("悬空标记未被清除")
	}
	if clearDanglingLkgMarker() {
		t.Fatal("无标记时应为 no-op")
	}

	writeLkgMarker(lkgMarker{HarnessVersion: "0.1.7-rc.1"})
	if err := os.WriteFile(filepath.Join(harnessDir, "package.json"+lkgSuffix), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if clearDanglingLkgMarker() {
		t.Fatal("存在 LKG 备份时不得清理标记")
	}
	if _, ok := readLkgMarker(); !ok {
		t.Fatal("存在 LKG 备份时标记应保留")
	}
}
