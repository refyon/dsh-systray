//go:build windows

package main

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// 窗口「最大化」能力禁用（Windows）：
//
// 主窗口是固定尺寸的（Wails 选项里 Min==Max），最大化只会把它拉到屏幕左上角、视觉上像"没反应"。
// Wails v2 的 windows.Options 没有对应开关，因此直接改窗口样式：去掉 WS_MAXIMIZEBOX
// （最大化按钮置灰、双击标题栏与系统菜单项一并失效），再从系统菜单里删掉「最大化」项。
//
// 调用时机：窗口已创建之后（onDomReady）。失败只记日志——这是纯外观优化，不能影响启动。
const (
	wsMaximizeBox   = 0x00010000
	scMaximize      = 0xF030
	swpNoMove       = 0x0002
	swpNoSize       = 0x0001
	swpNoZOrder     = 0x0004
	swpFrameChanged = 0x0020
)

// gwlStyle GetWindowLongPtr / SetWindowLongPtr 的索引 GWL_STYLE = -16。
// 用变量而不是常量：常量负数转 uintptr 会编译报错（溢出），变量是运行时转换（按位取补）。
var gwlStyle = int32(-16)

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetWindowLongPtrW        = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW        = user32.NewProc("SetWindowLongPtrW")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
	procGetSystemMenu            = user32.NewProc("GetSystemMenu")
	procDeleteMenu               = user32.NewProc("DeleteMenu")
	procDrawMenuBar              = user32.NewProc("DrawMenuBar")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
)

// disableWindowMaximize 关闭进程主窗口的最大化能力（找不到窗口时返回错误，由调用方决定是否重试）。
func disableWindowMaximize(title string) error {
	hwnd, err := findWindowByPIDAndTitle(uint32(syscall.Getpid()), title)
	if err != nil {
		return err
	}
	style, _, _ := procGetWindowLongPtrW.Call(hwnd, uintptr(gwlStyle))
	newStyle := style &^ uintptr(wsMaximizeBox)
	if newStyle == style {
		return nil // 已经是禁用状态
	}
	if _, _, errno := procSetWindowLongPtrW.Call(hwnd, uintptr(gwlStyle), newStyle); errno != nil && errno != syscall.Errno(0) {
		return fmt.Errorf("SetWindowLongPtr: %v", errno)
	}
	// 样式改动要 SWP_FRAMECHANGED 才会重画标题栏
	procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0,
		uintptr(swpNoMove|swpNoSize|swpNoZOrder|swpFrameChanged))
	// 系统菜单（标题栏右键/Alt+空格）里的「最大化」一并删掉
	if menu, _, _ := procGetSystemMenu.Call(hwnd, 0); menu != 0 {
		procDeleteMenu.Call(menu, uintptr(scMaximize), 0)
		procDrawMenuBar.Call(hwnd)
	}
	return nil
}

// findWindowByPIDAndTitle 按进程 id + 标题找主窗口（标题为空时只按进程匹配，且要求窗口可见）。
//
// 标题非空时**不要求窗口可见**：服务已在运行/自启动时窗口是隐藏创建的（Wails StartHidden），
// 按「只找可见窗口」匹配会永远找不到（2026-10-06 现场日志：每次启动都记「禁用窗口最大化
// 按钮失败」，用户看到的仍是可点的最大化按钮）。标题是精确匹配，足以定位主窗口。
func findWindowByPIDAndTitle(pid uint32, title string) (uintptr, error) {
	var found uintptr
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var winPID uint32
		procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&winPID)))
		if winPID != pid {
			return 1 // 继续枚举
		}
		if title != "" {
			buf := make([]uint16, 512)
			n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
			if n == 0 || !strings.EqualFold(syscall.UTF16ToString(buf[:n]), title) {
				return 1
			}
		} else if visible, _, _ := procIsWindowVisible.Call(hwnd); visible == 0 {
			return 1
		}
		found = hwnd
		return 0 // 找到，停止枚举
	})
	procEnumWindows.Call(cb, 0)
	if found == 0 {
		return 0, fmt.Errorf("未找到窗口（pid=%d title=%q）", pid, title)
	}
	return found, nil
}

// maximizeDisabled 记录「最大化已禁用」：窗口样式一旦去掉 WS_MAXIMIZEBOX 就不会自己回来，
// 成功一次即可——之后每次打开设置窗口再调用本函数直接返回，不重复枚举窗口。
var maximizeDisabled atomic.Bool

// startDisableWindowMaximize 异步禁用最大化：窗口在 onDomReady 后才保证已创建，因此带重试
// （约 3 秒）；全部失败只记一条日志。窗口显示后再调用一次是安全的（已禁用则立即返回）。
func startDisableWindowMaximize(title string) {
	if maximizeDisabled.Load() {
		return
	}
	go func() {
		for i := 0; i < 20; i++ {
			if err := disableWindowMaximize(title); err == nil {
				maximizeDisabled.Store(true)
				log.Printf("[app] 已禁用窗口最大化按钮")
				return
			}
			time.Sleep(150 * time.Millisecond)
		}
		log.Printf("[app] 禁用窗口最大化按钮失败（不影响使用）")
	}()
}
