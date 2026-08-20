//go:build darwin

package main

import "fmt"

// startSplash 在 macOS 上以通知形式提示“正在启动…”，返回空关闭函数。
func startSplash(text string) func() {
	script := fmt.Sprintf(`display notification "%s" with title "%s"`, escapeAppleScript(text), appName)
	_, _ = runAppleScript(script)
	return func() {}
}
