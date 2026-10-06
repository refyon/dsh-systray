//go:build windows

package main

import (
	"fmt"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// 窗口创建时带上 WS_MAXIMIZEBOX（真实主窗口由 Wails 这样创建）。
const wsOverlappedWindow = 0x00CF0000

// TestDisableWindowMaximizeHiddenWindow 回归 2026-10-06 现场：程序自启动/服务已就绪时
// 窗口是**隐藏创建**的（Wails StartHidden），此时也必须能找到它并去掉 WS_MAXIMIZEBOX——
// 否则用户打开设置窗口后最大化按钮依然可点（日志表现为反复「禁用窗口最大化按钮失败」）。
//
// 注意：窗口属于**创建它的线程**，而本测试没有消息循环——测试 goroutine 一旦被调度到别的
// 线程，改样式时的跨线程消息就会永久阻塞（曾把整包 go test 挂到 10 分钟超时）。
// 因此把窗口创建与调用一起锁在同一个 OS 线程上，并给调用加超时兜底。
func TestDisableWindowMaximizeHiddenWindow(t *testing.T) {
	const title = "dsh-systray-test-maximize"

	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		done <- disableMaximizeOnHiddenWindow(title)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("禁用最大化调用超时：窗口线程没有消息循环时被跨线程消息卡住")
	}
}

// disableMaximizeOnHiddenWindow 建一个隐藏窗口并调 disableWindowMaximize 校验样式位
// （必须在创建窗口的同一个 OS 线程上调用）。
func disableMaximizeOnHiddenWindow(title string) error {
	user32 := syscall.NewLazyDLL("user32.dll")
	procCreateWindowExW := user32.NewProc("CreateWindowExW")
	procDestroyWindow := user32.NewProc("DestroyWindow")

	cls, _ := syscall.UTF16PtrFromString("STATIC")
	text, _ := syscall.UTF16PtrFromString(title)
	hwnd, _, callErr := procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(text)),
		uintptr(wsOverlappedWindow), 0, 0, 320, 200, 0, 0, 0, 0)
	if hwnd == 0 {
		return fmt.Errorf("无法创建测试窗口（无 GUI 会话？）：%v", callErr)
	}
	defer procDestroyWindow.Call(hwnd)

	if visible, _, _ := procIsWindowVisible.Call(hwnd); visible != 0 {
		return fmt.Errorf("测试窗口应保持隐藏（复现 StartHidden 场景）")
	}
	if err := disableWindowMaximize(title); err != nil {
		return fmt.Errorf("隐藏窗口也应能被匹配并禁用最大化：%w", err)
	}
	style, _, _ := procGetWindowLongPtrW.Call(hwnd, uintptr(gwlStyle))
	if style&uintptr(wsMaximizeBox) != 0 {
		return fmt.Errorf("WS_MAXIMIZEBOX 应被清除，实际 style=0x%x", style)
	}
	return nil
}
