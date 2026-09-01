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
func startSplash(text string) *SplashState {
	setSplashPhase("startup")
	if text != "" {
		emitSplash(text, 0)
	}
	return &SplashState{
		Update: func(t string, f float64) { emitSplash(t, f) },
		Close: func() {
			emitSplash("", 1)
			hideMainWindow()
		},
	}
}

// emitSplash 推送 splash:progress 事件（text 为空表示完成）。
func emitSplash(text string, pct float64) {
	if appCtx == nil {
		return
	}
	phase, _ := splashPhase.Load().(string)
	if phase == "" {
		phase = "startup"
	}
	runtime.EventsEmit(appCtx, "splash:progress", map[string]interface{}{
		"phase": phase,
		"text":  text,
		"pct":   pct,
	})
}
