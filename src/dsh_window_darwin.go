//go:build darwin

package main

/*
#cgo darwin LDFLAGS: -framework Cocoa
#include <stdbool.h>

// 实现在 dsh_window_darwin.m（同包内 Objective-C 文件，cgo 编译时一并链接）。
bool dsh_has_visible_window(void);
*/
import "C"

// mainWindowVisible 设置主窗口当前是否可见（按项目语义“是否已打开”：最小化/被遮挡仍算可见）。
// 用途见 main.go showMainWindow：窗口已开着时托盘「设置」只置前、不重载内容。
// 走进程内 AppKit 查询（[NSApp windows]），不依赖辅助功能权限，也不会拉起子进程。
func mainWindowVisible() bool {
	return bool(C.dsh_has_visible_window())
}
