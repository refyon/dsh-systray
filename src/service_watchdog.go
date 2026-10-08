// service_watchdog.go：后台服务存活看门狗。
//
// 由来（2026-10-06 现场两次）：服务进程可能**静默消失**（历史根因是托盘自更新/退出切断了它的
// stdout 管道 → EPIPE → Node exit(1)），而托盘的自动重启只在"服务是本进程的子进程"时才有信号
// （cmd.Wait → noteServerExit）。**沿用的服务（上次退出保留 / 重启电脑后仍在跑）没有子进程句柄**，
// 死了也没有任何回调——用户只能自己发现网页断了再点「重启后台服务」。
//
// 本看门狗用最朴素的判据兜住这类情况：周期性 HTTP 探测，连续无响应就按既有重启通道拉起
// （restartAndVerifyServer：停服 → 拉起 → 就绪 → 健康窗口），并把结果写进日志。
//
// 让位规则（避免误判与打扰用户）：
//   - 用户主动停服（serverStopByTray）不拉起；
//   - 其它操作正在改服务（更新/重置/插件批处理/导入恢复/同步应用，见 watchdogSkip）不拉起；
//   - 退出中（quitting）、更新收尾中（updateFinalizing）、截图模式不探查；
//   - 两次重启之间有冷却，且每会话重启次数有上限（超过只告警，不无限重启）。
package main

import (
	"context"
	"log"
	"sync/atomic"
	"time"
)

const (
	// serviceWatchdogInterval 探测间隔：服务不再频繁写日志，30 秒足够发现死亡且几乎无开销。
	serviceWatchdogInterval = 30 * time.Second
	// serviceWatchdogFailures 连续多少次无响应才判定"已死"（吸收瞬时抖动/短暂 GC/端口争用）。
	serviceWatchdogFailures = 2
	// serviceWatchdogCooldown 两次自动拉起之间的最小间隔（拉起本身要几十秒，避免叠加）。
	serviceWatchdogCooldown = 3 * time.Minute
	// serviceWatchdogMaxRestarts 单次运行最多自动拉起次数（超出后只记日志，避免重启风暴）。
	serviceWatchdogMaxRestarts = 5
)

// serviceWatchdogState 看门狗的判定状态（纯逻辑，便于单测）。
type serviceWatchdogState struct {
	fails      int
	restarts   int
	lastStart  time.Time
	lastReport time.Time
}

// watchdogAction 一轮探测后的动作。
type watchdogAction int

const (
	watchdogNone    watchdogAction = iota // 什么都不做
	watchdogRestart                       // 触发一次自动拉起
	watchdogGiveUp                        // 达到上限：只告警
)

// step 根据本轮探测结果推进状态并给出动作。responding=false 表示服务无响应；
// skip=true 表示本轮不该管（主动停服/退出中/更新中）。
func (s *serviceWatchdogState) step(now time.Time, responding, skip bool) watchdogAction {
	if skip || responding {
		s.fails = 0
		return watchdogNone
	}
	s.fails++
	if s.fails < serviceWatchdogFailures {
		return watchdogNone
	}
	if s.restarts >= serviceWatchdogMaxRestarts {
		if now.Sub(s.lastReport) >= serviceWatchdogCooldown {
			s.lastReport = now
			return watchdogGiveUp
		}
		return watchdogNone
	}
	if !s.lastStart.IsZero() && now.Sub(s.lastStart) < serviceWatchdogCooldown {
		return watchdogNone // 冷却期内：等下一轮
	}
	s.fails = 0
	s.restarts++
	s.lastStart = now
	return watchdogRestart
}

var watchdogRestarting atomic.Bool // 拉起进行中：跳过本轮，避免与用户手动重启并发

// watchdogLetAnotherOperationFinish 是否有别的操作正在接管服务生命周期。
//
// 更新 harness / 重置 / 插件批处理 / 导入恢复 / 同步应用都会「主动停服 → 改文件 → 自己拉起并做
// 启动校验」；看门狗此时探测到的「无响应」是这些操作自己造成的，插手只会互相 kill
//（2026-10-08 现场：插件同步应用 17:04:33 拉起服务，看门狗 17:05:06 判定无响应把它杀掉重启，
// 同步应用的启动校验观察到进程被带走 → 误报「与当前服务不兼容」并回退，再点一次才成功）。
// 与 service_supervisor.go 的让位规则一致：谁在改服务，谁负责把它拉回来。
func watchdogLetAnotherOperationFinish() bool {
	return harnessOpBusy.Load() || pluginBatchRunning() || importRestoreRunning() || accountApplyBusy()
}

// watchdogSkip 本轮不该管的判定（主动停服 / 退出中 / 自己正在拉起 / 别的操作在改服务）。
func watchdogSkip() bool {
	return quitting.Load() || serverStopByTray.Load() || watchdogRestarting.Load() ||
		watchdogLetAnotherOperationFinish()
}

// startServiceWatchdog 启动存活看门狗（ctx 结束即退出；截图/演示模式不启动）。
//
// probe 与 restart 可注入，便于测试；生产分别用 serverResponding(webURL) 与 restartAndVerifyServer。
func startServiceWatchdog(ctx context.Context, probe func() bool, restart func() bool) {
	if ctx == nil || shotMode {
		return
	}
	if probe == nil {
		probe = func() bool { return serverResponding(webURL) }
	}
	if restart == nil {
		restart = restartAndVerifyServer
	}
	var st serviceWatchdogState
	go func() {
		ticker := time.NewTicker(serviceWatchdogInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			skip := watchdogSkip()
			switch st.step(time.Now(), probe(), skip) {
			case watchdogRestart:
				watchdogRestarting.Store(true)
				log.Printf("[app] 后台服务无响应（连续 %d 次探测失败），正在自动拉起", serviceWatchdogFailures)
				ok := restart()
				watchdogRestarting.Store(false)
				if ok {
					log.Printf("[app] 后台服务已自动拉起并校验通过")
				} else {
					log.Printf("[app] 后台服务自动拉起失败（可手动点「重启后台服务」或查看日志）")
				}
			case watchdogGiveUp:
				log.Printf("[app] 后台服务持续无响应，已自动拉起 %d 次仍未恢复（停止自动重试，请查看日志）", st.restarts)
			}
		}
	}()
}
