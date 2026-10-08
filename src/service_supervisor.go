package main

// ==================== 后台服务退出守护（快速自动重启） ====================
//
// 背景：DSH 自己有「请求重启」的通道——HMR 判定需要整体重载（框架依赖变化）与插件管理器判定
// restart-required（替换已解析的包版本）时，都会调用宿主注入的 loader.exit()，宿主 profile-boot
// 把它接到 createProcessShutdown：优雅 dispose 之后 process.exit()（见 harness 的
// apps/desktop-host/src/index.ts 与 dsh/lib/profile-boot-*.js 的 exit 钩子）。
// 也就是说：服务进程会**主动退出**，等着外面把它拉起来——官方桌面端由 Electron 壳承担这个角色
// （apps/desktop/src/host-process.ts 的 stop()/start()）。托盘此前没有任何退出观察者：
// 这类退出等于服务直接停摆，用户看到「Web UI 打不开」，只能手动重启一次。
//
// 这里补上守护：进程退出后（非托盘主动停服、非退出流程、退出的是当前代次的进程）立刻按退避策略
// 拉起，复用设置页重启同一条链路（端口预检 → startServer → 就绪 → 健康校验）。
// 额度有界：autoRestartWindow 内最多 autoRestartMax 次，反复崩溃时不再无限重拉。

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// autoRestartWindow / autoRestartMax 滑动窗口与次数上限。服务自行请求重启是正常场景，
	// 一次即成功；窗口内连续 3 次仍在退出说明插件树/版本本身有问题，交给既有失败提示与自愈链路。
	autoRestartWindow = 5 * time.Minute
	autoRestartMax    = 3
)

// autoRestartBackoff 第 n 次自动重启前的等待（下标 0 = 首次，立即拉起）。
var autoRestartBackoff = []time.Duration{0, time.Second, 3 * time.Second}

var (
	autoRestartMu       sync.Mutex
	autoRestartAttempts []time.Time
	// autoRestartInFlight 同一时刻只允许一次自动重启（多次退出事件/竞态时不重复拉起）。
	autoRestartInFlight atomic.Bool
)

// autoRestartVerify / autoRestartRun 守护的两个执行缝（测试可替换，避免测试里拉真实进程）：
// verify = 「拉起 + 就绪 + 健康校验」，run = 收到退出事件后的执行体（默认另起 goroutine，
// 不阻塞 cmd.Wait 的收尾——退出通道还要供启动校验读取）。
//
// 在 init 中装配而不在变量声明处直接赋值：拉起动作最终会走 startServer → noteServerExit →
// autoRestartRun → autoRestartService → autoRestartVerify，Go 的初始化依赖分析按函数体引用
// 传递，声明处赋值会构成初始化环（与 lateBootSelfHeal 同样的处理，见 updater.go 的 init）。
var (
	autoRestartVerify func() (bool, string)
	autoRestartRun    func(cause error)
)

func init() {
	autoRestartVerify = startAndVerifyOnceQuick
	autoRestartRun = func(cause error) { go autoRestartService(cause) }
}

// noteServerExit 服务进程退出后的守护判定。gen 是 startServer 拉起该进程时记录的服务代次：
//
//	quitting / quitRequested —— 应用正在退出：不拉；
//	serverStopByTray         —— 托盘主动停服（更新/装插件/导入/用户停服）：由该操作自己重启；
//	serverStartGen != gen    —— 退出的是旧代次（kill→start 的正常交接），当前进程还活着；
//	!serverReady             —— 还没就绪：启动失败的判定与自愈由既有链路负责，守护不插一脚；
//	desktop 启动方式          —— 后台 Web 服务不由本程序管理（引擎属于官方桌面端）。
func noteServerExit(gen int64, cause error) {
	if quitting.Load() || quitRequested.Load() {
		return
	}
	if serverStopByTray.Load() || serverStartGen.Load() != gen {
		return
	}
	if !serverReady.Load() {
		return
	}
	if launchTargetIsDesktop() {
		return
	}
	autoRestartRun(cause)
}

// reserveAutoRestart 记一次自动重启额度：返回（第几次，是否允许）。now 由调用方传入便于测试。
func reserveAutoRestart(now time.Time) (int, bool) {
	autoRestartMu.Lock()
	defer autoRestartMu.Unlock()
	kept := autoRestartAttempts[:0]
	for _, t := range autoRestartAttempts {
		if now.Sub(t) < autoRestartWindow {
			kept = append(kept, t)
		}
	}
	autoRestartAttempts = kept
	if len(autoRestartAttempts) >= autoRestartMax {
		return 0, false
	}
	autoRestartAttempts = append(autoRestartAttempts, now)
	return len(autoRestartAttempts), true
}

// autoRestartDelay 第 n 次（从 1 起）自动重启前的等待。
func autoRestartDelay(n int) time.Duration {
	if n <= 1 || n > len(autoRestartBackoff) {
		return 0
	}
	return autoRestartBackoff[n-1]
}

// autoRestartService 执行一次自动重启（由 noteServerExit 经 autoRestartRun 异步调用）。
func autoRestartService(cause error) {
	causeText := fmt.Sprintf("%v", cause)
	if !autoRestartInFlight.CompareAndSwap(false, true) {
		log.Printf("service exited (%s), auto restart already in flight; skip", causeText)
		return
	}
	defer autoRestartInFlight.Store(false)

	n, allowed := reserveAutoRestart(time.Now())
	if !allowed {
		reason := "服务反复退出（" + autoRestartWindow.String() + " 内已达自动重启上限），已停止自动重启"
		logError("app", "%s：%s", reason, causeText)
		serverReady.Store(false)
		setServiceFailed("", reason)
		return
	}
	if d := autoRestartDelay(n); d > 0 {
		time.Sleep(d)
	}
	if quitting.Load() || quitRequested.Load() || serverStopByTray.Load() {
		log.Printf("auto restart #%d cancelled (stopped by tray or app quitting)", n)
		return
	}
	// 其它操作（harness 更新/重置、插件批处理、导入恢复、同步应用）在跑：它们自己负责把服务
	// 拉回来，守护让位（与看门狗同一套判据，见 service_watchdog.go 的 watchdogSkip）。
	if watchdogLetAnotherOperationFinish() {
		log.Printf("service exited (%s), but another operation is running; auto restart skipped", causeText)
		return
	}
	log.Printf("service exited (%s), auto restarting (attempt %d)", causeText, n)
	logUI("自动重启服务", "服务进程已退出（"+causeText+"），正在自动拉起")
	if autoRestartVerify == nil {
		log.Printf("auto restart verify hook is not wired; skip")
		return
	}
	ok, msg := autoRestartVerify()
	if !ok {
		logError("app", "自动重启失败：%s", msg)
		serverReady.Store(false)
		setServiceFailed("", "服务进程退出后自动重启失败："+msg)
		return
	}
	serverReady.Store(true)
	serviceFailed.Store(false)
	refreshServiceMenu()
	log.Printf("auto restart ok (attempt %d)", n)
}
