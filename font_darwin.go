//go:build darwin

package main

/*
#cgo darwin CFLAGS: -x objective-c -fobjc-arc
#cgo darwin LDFLAGS: -framework Cocoa -framework CoreText
#import <Foundation/Foundation.h>
#import <CoreText/CoreText.h>
#include <stdlib.h>
static int dshFontExists(const char* name) {
    NSString *n = [NSString stringWithUTF8String:name];
    NSFont *f = [NSFont fontWithName:n size:14];
    return f != nil ? 1 : 0;
}
static int dshRegisterFont(const char* path) {
    NSString *p = [NSString stringWithUTF8String:path];
    NSURL *u = [NSURL fileURLWithPath:p];
    BOOL ok = CTFontManagerRegisterFontsForURL((CFURLRef)u, kCTFontManagerScopeProcess, NULL);
    return ok ? 1 : 0;
}
*/
import "C"

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"unsafe"
)

// notoSansSCFamily 首选 UI 字体：Google Noto Sans SC。系统已装则直接用，未装则下载并注册，失败回退系统默认字体。
var notoSansSCFamily = "Noto Sans SC"

// notoSansSCURLs Noto Sans SC 可变字体下载地址（含全部字重），经多镜像回退。
var notoSansSCURLs = []string{
	"https://github.com/googlefonts/noto-cjk/raw/main/Sans/Variable/TTF/NotoSansSC%5Bwght%5D.ttf",
}

func notoSansSCFontDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "dsh-systray", "fonts")
}

// ensureNotoSansSC 启动时确保 Noto Sans SC 可用：已装→直接用；未装→下载到用户目录并注册到进程；
// 失败则回退系统默认字体。注册为进程级（kCTFontManagerScopeProcess），不写系统、无需管理员。
// onStatus 可选：用于进度窗口同步阶段/下载进度。
func ensureNotoSansSC(onStatus func(text string, pct float64)) {
	cs := C.CString(notoSansSCFamily)
	exists := C.dshFontExists(cs) != 0
	C.free(unsafe.Pointer(cs))
	if exists {
		log.Printf("noto sans sc available")
		progress(onStatus, "", 1)
		return
	}
	dir := notoSansSCFontDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("noto sans font dir create failed: %v", err)
		progress(onStatus, "", 0)
		return
	}
	fontPath := filepath.Join(dir, "NotoSansSC-VF.ttf")
	if _, err := os.Stat(fontPath); err != nil {
		log.Printf("noto sans sc not installed; downloading ...")
		progress(onStatus, "正在下载 UI 字体…", 0)
		if err := downloadFileWithProgress(context.Background(), notoSansSCURLs[0], fontPath, func(p float64) {
			progress(onStatus, "正在下载 UI 字体…", p)
		}); err != nil {
			log.Printf("noto sans sc download failed: %v; using system font", err)
			progress(onStatus, "", 0)
			return
		}
	}
	cp := C.CString(fontPath)
	defer C.free(unsafe.Pointer(cp))
	if C.dshRegisterFont(cp) == 0 {
		log.Printf("noto sans sc register failed; using system font")
	} else {
		log.Printf("noto sans sc registered from %s", fontPath)
	}
	progress(onStatus, "", 1)
}
