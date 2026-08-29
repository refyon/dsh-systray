//go:build darwin

package main

/*
#cgo darwin CFLAGS: -x objective-c -fobjc-arc -Wno-deprecated-declarations
#cgo darwin LDFLAGS: -framework Cocoa
#include <stdlib.h>
// 原生设置窗口入口（settings_darwin.m 实现，窗口创建于主线程）
void dsh_settings_open(const char* version, int autostartOn);
// 读取日志文本（which=0 app.log / 1 server.log），返回 malloc 的 C 字符串（调用方 free）
char* dshSettingsGoLoadLog(int which);
*/
import "C"

import (
	"os"
	"path/filepath"
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

//export dshSettingsGoLoadLog
func dshSettingsGoLoadLog(which C.int) *C.char {
	name := "app.log"
	if which == 1 {
		name = "server.log"
	}
	p := filepath.Join(logDir, name)
	text := ""
	if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
		const max = 100 * 1024
		if len(data) > max {
			data = data[len(data)-max:]
		}
		text = string(data)
	} else {
		text = "（暂无日志：" + p + "）"
	}
	return C.CString(text)
}

// openSettingsWindow 打开原生 Cocoa 设置窗口（左侧分类栏 + 右侧内容面板）。
func openSettingsWindow() {
	cs := C.CString(withV(appVersion))
	defer C.free(unsafe.Pointer(cs))
	auto := 0
	if isAutostartEnabled() {
		auto = 1
	}
	C.dsh_settings_open(cs, C.int(auto))
}
