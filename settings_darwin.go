//go:build darwin

package main

/*
#cgo darwin CFLAGS: -x objective-c -fobjc-arc -Wno-deprecated-declarations
#cgo darwin LDFLAGS: -framework Cocoa
#include <stdlib.h>
// 原生设置窗口入口（settings_darwin.m 实现，窗口创建于主线程）
void dsh_settings_open(const char* version, int autostartOn);
*/
import "C"

import (
	"unsafe"
)

//export dshSettingsGoAutostartToggled
func dshSettingsGoAutostartToggled(on C.int) {
	onBool := on != 0
	go func() {
		setAutostartOn(onBool)
	}()
}

//export dshSettingsGoCheckUpdate
func dshSettingsGoCheckUpdate() {
	go func() {
		checkForUpdatesManual()
	}()
}

// openSettingsWindow 打开原生 Cocoa 设置窗口（左侧分类栏 + 右侧内容面板）。
func openSettingsWindow() {
	cs := C.CString(appVersion)
	defer C.free(unsafe.Pointer(cs))
	auto := 0
	if isAutostartEnabled() {
		auto = 1
	}
	C.dsh_settings_open(cs, C.int(auto))
}
