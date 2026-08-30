//go:build darwin

package main

/*
#cgo darwin CFLAGS: -x objective-c -fobjc-arc -Wno-deprecated-declarations
#cgo darwin LDFLAGS: -framework Cocoa -framework UniformTypeIdentifiers
#include <stdlib.h>
// 原生设置窗口入口（settings_darwin.m 实现，窗口创建于主线程）
void dsh_settings_open(const char* version, const char* harnessVersion, int autostartOn);
// 读取日志文本（which=0 app.log / 1 server.log），返回 malloc 的 C 字符串（调用方 free）
char* dshSettingsGoLoadLog(int which);
*/
import "C"

import (
	"encoding/json"
	"os"
	"path/filepath"
	"unsafe"
)

// dshResult 返回给 ObjC 的 JSON 结果（{ok, path/error/message/items}）。
type dshResult struct {
	OK            bool         `json:"ok"`
	Path          string       `json:"path,omitempty"`
	Error         string       `json:"error,omitempty"`
	Message       string       `json:"message,omitempty"`
	Items         []importItem `json:"items,omitempty"`
	RestartPending bool        `json:"restartPending,omitempty"` // 恢复会话/插件后需重启服务生效
}

func dshResultCString(r dshResult) *C.char {
	b, err := json.Marshal(r)
	if err != nil {
		return C.CString(`{"ok":false,"error":"内部错误"}`)
	}
	return C.CString(string(b))
}

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

// 返回当前所选日志文件的完整路径（调用方 free）；which=0 app.log / 1 server.log。
//
//export dshSettingsGoLogPath
func dshSettingsGoLogPath(which C.int) *C.char {
	name := "app.log"
	if which == 1 {
		name = "server.log"
	}
	return C.CString(filepath.Join(logDir, name))
}

//export dshSettingsGoClearLog
func dshSettingsGoClearLog(which C.int) {
	name := "app.log"
	if which == 1 {
		name = "server.log"
	}
	_ = os.WriteFile(filepath.Join(logDir, name), nil, 0o644)
}

//export dshSettingsGoRestartService
func dshSettingsGoRestartService() {
	go func() {
		if !askRestartServiceMac() {
			return
		}
		restartBackgroundService(nil)
	}()
}

//export dshSettingsGoServiceState
func dshSettingsGoServiceState() *C.char {
	if serverResponding(webURL) {
		return C.CString("运行中")
	}
	return C.CString("未运行")
}

// 执行导出（阻塞，应在后台队列调用）。sessions/plugins=是否勾选；dirsJSON=目录列表 JSON 数组；
// destDir=保存位置。返回 dshResult JSON（调用方 free）。
//
//export dshSettingsGoExport
func dshSettingsGoExport(sessions, plugins C.int, dirsJSON, destDir *C.char) *C.char {
	var dirs []string
	if s := C.GoString(dirsJSON); s != "" {
		_ = json.Unmarshal([]byte(s), &dirs)
	}
	dest := C.GoString(destDir)
	path, err := buildExportZip(sessions != 0, plugins != 0, dirs, dest, nil)
	if err != nil {
		return dshResultCString(dshResult{OK: false, Error: err.Error()})
	}
	return dshResultCString(dshResult{OK: true, Path: path})
}

// 解析导入压缩包（阻塞，应在后台队列调用）。返回 dshResult JSON（items=可恢复项列表）。
//
//export dshSettingsGoInspect
func dshSettingsGoInspect(zipPath *C.char) *C.char {
	items, err := parseExportZip(C.GoString(zipPath))
	if err != nil {
		return dshResultCString(dshResult{OK: false, Error: err.Error()})
	}
	return dshResultCString(dshResult{OK: true, Items: items})
}

// 统计恢复冲突项数（阻塞）：-1=出错，0=无冲突，>0=冲突项数。
//
//export dshSettingsGoCountConflicts
func dshSettingsGoCountConflicts(kindC, zipPathC *C.char) C.int {
	kind := C.GoString(kindC)
	inner := innerZipName(kind)
	if inner == "" {
		return -1
	}
	tmp, cleanup, err := extractInnerZip(C.GoString(zipPathC), inner)
	if err != nil {
		return -1
	}
	defer cleanup()
	n, err := countRestoreConflicts(kind, tmp)
	if err != nil {
		return -1
	}
	return C.int(n)
}

// 恢复子包（阻塞，应在后台队列调用）：会话/插件恢复前暂停后台服务、完成后自动重启。
// kind=sessions|plugins|files；files 需 destDir（解压位置）；overwrite=1 覆盖 / 0 跳过已有。
// 返回 dshResult JSON（调用方 free）。
//
//export dshSettingsGoRestore
func dshSettingsGoRestore(kindC, zipPathC, destDirC *C.char, overwrite C.int) *C.char {
	kind := C.GoString(kindC)
	inner := innerZipName(kind)
	if inner == "" {
		return dshResultCString(dshResult{OK: false, Error: "未知的恢复项：" + kind})
	}
	tmp, cleanup, err := extractInnerZip(C.GoString(zipPathC), inner)
	if err != nil {
		return dshResultCString(dshResult{OK: false, Error: err.Error()})
	}
	defer cleanup()

	stopped := pauseServiceForRestore()
	_, err = restoreItem(kind, tmp, C.GoString(destDirC), overwrite != 0, nil)
	restartPending := stopped && kind != "files" // 不自动重启：由设置窗口关闭时提示用户重启
	if err != nil {
		return dshResultCString(dshResult{OK: false, Error: err.Error()})
	}
	msg := "恢复完成"
	return dshResultCString(dshResult{OK: true, Message: msg, RestartPending: restartPending})
}

// innerZipName 子包在总 zip 内的文件名。
func innerZipName(kind string) string {
	switch kind {
	case "sessions":
		return exportZipSessions
	case "plugins":
		return exportZipPlugins
	case "files":
		return exportZipFiles
	}
	return ""
}

// openSettingsWindow 打开原生 Cocoa 设置窗口（左侧分类栏 + 右侧内容面板）。
func openSettingsWindow() {
	cs := C.CString(withV(appVersion))
	defer C.free(unsafe.Pointer(cs))
	hv := withV(installedHarnessVersion())
	if hv == "" {
		hv = "未检测到"
	}
	hcs := C.CString(hv)
	defer C.free(unsafe.Pointer(hcs))
	auto := 0
	if isAutostartEnabled() {
		auto = 1
	}
	C.dsh_settings_open(cs, hcs, C.int(auto))
}
