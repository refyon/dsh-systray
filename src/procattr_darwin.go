//go:build darwin

package main

import "syscall"

// ghSysProcAttr macOS 无需隐藏控制台窗口（启动 gh 不会新建终端）。
func ghSysProcAttr() *syscall.SysProcAttr { return nil }
