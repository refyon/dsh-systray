//go:build windows

package main

import "syscall"

// ghSysProcAttr 让 gh 子进程不留控制台窗口：gh.exe 是控制台程序，不加这个属性时
// Windows 会给它新建一个命令行窗口——用户看到的是黑窗口而不是浏览器（2026-09-13 实证）。
// gh 的输出仍由调用方以管道捕获（CombinedOutput），不依赖控制台。
func ghSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true}
}
