//go:build !windows

package main

// startDisableWindowMaximize 非 Windows 平台：macOS 用 Wails 的 mac.Options.DisableZoom 关闭缩放
// （最大化）按钮，见 main.go 的 wails.Run 选项；其它平台无此概念，这里不做事。
func startDisableWindowMaximize(title string) {
	_ = title
}
