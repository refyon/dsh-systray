package main

import (
	"sync/atomic"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// SplashState 进度控制器（跨平台）：Wails 版本不再创建原生窗口，
// 而是把进度推送到前端 splash 视图（事件 splash:progress）。
// 接口保持与旧实现一致，供启动流程与更新流程复用。
type SplashState struct {
	Update func(text string, fraction float64)
	Close  func()
	// KeepWindow 收尾时不隐藏主窗口（仅同步「重启生效」应用流程置 true）：该流程要求全过程
	// 可见，完成后停在设置页展示结果，而不是把窗口收起来（2026-09-22 用户决策）。
	KeepWindow bool
}

// SetOnClose 设置用户关闭进度窗口时的回调（true=允许关闭并中止；false=取消关闭继续运行）。
// Wails 版本由窗口关闭回调（WindowClosing）驱动：更新流程中关闭窗口会询问是否取消更新。
func (s *SplashState) SetOnClose(fn func() bool) { setSplashOnClose(fn) }

// splash 阶段：启动流程（startup）或更新流程（update），前端据此显示不同文案/按钮。
var splashPhase atomic.Value // string

func setSplashPhase(p string) {
	splashPhase.Store(p)
	emitSplash("", 0)
}

// setSplashPhaseQuiet 只复位相位、不向前端发事件（供流程收尾使用）。
// 相位复位本身没有任何 UI 用途，但它会命中前端 splash:progress 的 startup 分支把已收起的
// 进度视图重新拉起——紧随其后的 update:done 才切回设置页，两枚事件之间一旦丢失/延误，
// 窗口就停在进度视图（2026-09-15 现场问题：更新已完成却不退出更新窗口）。
func setSplashPhaseQuiet(p string) {
	splashPhase.Store(p)
}

func splashOnCloseFn() func() bool {
	if v, ok := splashOnClose.Load().(func() bool); ok {
		return v
	}
	return nil
}

var splashOnClose atomic.Value // func() bool

func setSplashOnClose(fn func() bool) {
	if fn == nil {
		splashOnClose.Store(nil)
	} else {
		splashOnClose.Store(fn)
	}
}

// startSplash 开始推送进度到前端 splash 视图。autostart 场景由 maybeStartSplash 返回空实现。
// 若调用方已声明更新阶段（startUpdateApplyWithUI 置 phase=update），保留 update——
// 前端据此显示更新视图与「取消更新」按钮；否则按启动阶段处理。
func startSplash(text string) *SplashState {
	if cur, _ := splashPhase.Load().(string); cur != "update" {
		setSplashPhase("startup")
	}
	splashActive.Store(true)
	if text != "" {
		emitSplash(text, 0)
	}
	s := &SplashState{}
	s.Update = func(t string, f float64) { emitSplash(t, f) }
	s.Close = func() {
		emitSplash("", 1)
		splashActive.Store(false)
		// 关进度视图必须同时把设置页还回来：进度视图是整块顶掉设置页显示的，而收尾事件
		// （update:done）只有更新类流程才发——插件批量操作与「重启后台服务」此前既没有
		// 收尾事件、又不会有 splash:progress 让前端复位，窗口就一直停在进度视图
		//（2026-09-17 现场问题：插件更新完成后不退出重启中页面）。这里复用启动完成的
		// splash:done 语义（前端切回设置页 + 刷新服务状态），重复发送无害。
		notifySplashDone()
		if !s.KeepWindow {
			hideMainWindow()
		}
	}
	return s
}

// splash 进度状态快照：窗口被用户关掉后再打开时按它把进度视图还原（见 replayActiveSplash）。
type splashProgress struct {
	phase string
	text  string
	pct   float64
}

var (
	// splashActive 是否有进度流程在跑（startSplash → Close 之间）。
	// 用途：窗口关掉后重开时，进度流程还在跑就把进度视图还回来，而不是切设置页——设置页会整块
	// 替换进度界面，用户既看不到下载到哪一步，也点不到「取消更新」（2026-09-17 用户反馈：
	// 下载依赖时关窗再开，只剩设置页、看不到下载进度）。
	splashActive atomic.Bool
	// splashLast 最近一次可展示的进度事件内容，供重开窗口时补发。
	splashLast atomic.Value // splashProgress
)

// replayActiveSplash 进度流程仍在跑时，把最近一次进度事件补发给前端（窗口被关掉后重开用）。
// 返回 false 表示当前没有可还原的进度视图，调用方按正常流程发 ui:show-settings。
func replayActiveSplash() bool {
	if !splashActive.Load() {
		return false
	}
	snap, ok := splashLast.Load().(splashProgress)
	if !ok || snap.text == "" {
		// 流程刚起步、还没有可展示的文案：交给设置页，随后的进度事件会再把进度视图拉起。
		return false
	}
	emitSplash(snap.text, snap.pct)
	return true
}

// emitSplash 推送 splash:progress 事件（text 为空表示完成）。
func emitSplash(text string, pct float64) {
	phase, _ := splashPhase.Load().(string)
	if phase == "" {
		phase = "startup"
	}
	// 先记快照再判 appCtx：窗口/renderer 尚未就绪时发出的进度，同样应当在窗口打开时能补发。
	// 「空文案 + 零进度」是相位复位事件（见 setSplashPhase），不构成可展示的进度，不入快照。
	if text != "" || pct > 0 {
		splashLast.Store(splashProgress{phase: phase, text: text, pct: pct})
	}
	if appCtx == nil {
		return
	}
	runtime.EventsEmit(appCtx, "splash:progress", map[string]interface{}{
		"phase": phase,
		"text":  text,
		"pct":   pct,
	})
}
