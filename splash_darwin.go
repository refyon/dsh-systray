//go:build darwin

package main

import "fmt"

// SplashState macOS 进度控制器：阶段变化时以系统通知提示；无进度条窗口。
type SplashState struct {
	Update func(text string, fraction float64)
	Close  func()
}

// startSplash 在 macOS 上以通知形式提示阶段进度。
func startSplash(text string) *SplashState {
	last := text
	notify := func(t string) {
		script := fmt.Sprintf(`display notification "%s" with title "%s"`, escapeAppleScript(t), appName)
		_, _ = runAppleScript(script)
	}
	notify(text)
	return &SplashState{
		Update: func(t string, fraction float64) {
			if t != "" && t != last {
				last = t
				notify(t)
			}
		},
		Close: func() {},
	}
}
