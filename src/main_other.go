//go:build !windows

package main

// processExecutionContext 非 Windows 平台的占位实现：macOS 上不存在 Windows 的
// "完整性级别 / 会话号"概念，启动日志只需保持同一格式（见 platform_windows.go）。
func processExecutionContext() string {
	return "integrity=n/a session=n/a user=n/a"
}
