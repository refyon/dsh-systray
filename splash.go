//go:build windows

package main

import (
	"runtime"
	"syscall"
	"unsafe"
)

const (
	wmClose          = 0x0010
	wmDestroy        = 0x0002
	wmCommand        = 0x0111
	wmSetFont        = 0x0030
	wmCtlColorStatic = 0x0138

	swShow        = 5
	wsCaption     = 0x00C00000
	wsSysMenu     = 0x00080000
	wsMinimizeBox = 0x00020000
	wsChild       = 0x40000000
	wsVisible     = 0x10000000
	wsTabStop     = 0x00010000
	ssCenter      = 0x00000001
	csHRedraw     = 0x0002
	csVRedraw     = 0x0001
	idcArrow      = 32512
	colorWin      = 6 // COLOR_WINDOW + 1
	smCX          = 0
	smCY          = 1

	idOkButton = 1

	defaultCharset = 1
	cleartypeQual  = 5

	bkTransparent = 1
	whiteBrush    = 0
)

var (
	modUser32           = syscall.NewLazyDLL("user32.dll")
	modKernel32         = syscall.NewLazyDLL("kernel32.dll")
	modGdi32            = syscall.NewLazyDLL("gdi32.dll")
	pRegisterClassExW   = modUser32.NewProc("RegisterClassExW")
	pCreateWindowExW    = modUser32.NewProc("CreateWindowExW")
	pDefWindowProcW     = modUser32.NewProc("DefWindowProcW")
	pGetMessageW        = modUser32.NewProc("GetMessageW")
	pTranslateMessage   = modUser32.NewProc("TranslateMessage")
	pDispatchMessageW   = modUser32.NewProc("DispatchMessageW")
	pPostQuitMessage    = modUser32.NewProc("PostQuitMessage")
	pShowWindow         = modUser32.NewProc("ShowWindow")
	pUpdateWindow       = modUser32.NewProc("UpdateWindow")
	pDestroyWindow      = modUser32.NewProc("DestroyWindow")
	pGetSystemMetrics   = modUser32.NewProc("GetSystemMetrics")
	pLoadCursorW        = modUser32.NewProc("LoadCursorW")
	pPostMessageW       = modUser32.NewProc("PostMessageW")
	pSendMessageW       = modUser32.NewProc("SendMessageW")
	pGetModuleHandleW   = modKernel32.NewProc("GetModuleHandleW")
	pCreateFontW        = modGdi32.NewProc("CreateFontW")
	pGetStockObject     = modGdi32.NewProc("GetStockObject")
	pSetTextColor       = modGdi32.NewProc("SetTextColor")
	pSetBkMode          = modGdi32.NewProc("SetBkMode")
	pAdjustWindowRectEx = modUser32.NewProc("AdjustWindowRectEx")
	pGetDC              = modUser32.NewProc("GetDC")
	pReleaseDC          = modUser32.NewProc("ReleaseDC")
	pDrawTextW          = modUser32.NewProc("DrawTextW")
	pSelectObject       = modGdi32.NewProc("SelectObject")
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type point struct{ x, y int32 }

type rect struct {
	left, top, right, bottom int32
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

func splashWndProc(hwnd, uMsg, wParam, lParam uintptr) uintptr {
	switch uMsg {
	case wmCommand:
		// 确定按钮被点击（BN_CLICKED，低 16 位为控件 ID）
		if wParam&0xFFFF == idOkButton {
			pDestroyWindow.Call(hwnd)
			return 0
		}
	case wmClose:
		pDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	case wmCtlColorStatic:
		// 静态文本：文字用黑色、背景透明（配合白色画刷），与窗口背景一致
		pSetTextColor.Call(wParam, 0)
		pSetBkMode.Call(wParam, bkTransparent)
		h, _, _ := pGetStockObject.Call(whiteBrush)
		return h
	}
	ret, _, _ := pDefWindowProcW.Call(hwnd, uMsg, wParam, lParam)
	return ret
}

func moduleHandle() uintptr {
	h, _, _ := pGetModuleHandleW.Call(0)
	return h
}

func splashFont() uintptr {
	face, _ := syscall.UTF16PtrFromString("Segoe UI")
	h, _, _ := pCreateFontW.Call(18, 0, 0, 0, 400, 0, 0, 0, defaultCharset, 0, 0, cleartypeQual, 0, uintptr(unsafe.Pointer(face)))
	return h
}

// measureTextHeight 用 GDI 计算文本在指定宽度下按词换行后的实际像素高度。
func measureTextHeight(text string, width int, font uintptr) int {
	dc, _, _ := pGetDC.Call(0)
	if dc == 0 {
		return 24
	}
	defer pReleaseDC.Call(0, dc)
	old, _, _ := pSelectObject.Call(dc, font)
	defer pSelectObject.Call(dc, old)
	rc := rect{0, 0, int32(width), 0}
	t, _ := syscall.UTF16PtrFromString(text)
	// DT_CALCRECT(0x0400) | DT_WORDBREAK(0x0010)：只计算换行后的矩形高度
	pDrawTextW.Call(dc, uintptr(unsafe.Pointer(t)), ^uintptr(0), uintptr(unsafe.Pointer(&rc)), 0x0400|0x0010)
	return int(rc.bottom - rc.top)
}

func createSplashWindow(text string) uintptr {
	cls, _ := syscall.UTF16PtrFromString("DSH_Systray_Splash")
	cb := syscall.NewCallback(splashWndProc)
	cur, _, _ := pLoadCursorW.Call(0, idcArrow)

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		style:         csHRedraw | csVRedraw,
		lpfnWndProc:   cb,
		hInstance:     moduleHandle(),
		hCursor:       cur,
		hbrBackground: colorWin,
		lpszClassName: cls,
	}
	pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	title, _ := syscall.UTF16PtrFromString("DeepSeek Harness")
	font := splashFont()

	// 自适应：按文本在固定宽度下换行后的实际高度决定窗口高度，确保“确定”按钮始终可见
	const textW = 360
	contentW := int32(textW)
	textH := measureTextHeight(text, textW, font)
	if textH < 24 {
		textH = 24
	}
	const staticY, btnH = 16, 30
	staticH := int32(textH) + 12
	btnY := staticY + int32(textH) + 26
	clientH := btnY + btnH + 16
	winW := contentW + 36

	// 用 AdjustWindowRectEx 把目标客户区换算为包含标题栏/边框的实际窗口尺寸
	r := rect{0, 0, winW, clientH}
	pAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&r)), wsCaption|wsSysMenu|wsMinimizeBox, 0, 0)
	winH := r.bottom - r.top

	sw, _, _ := pGetSystemMetrics.Call(smCX)
	sh, _, _ := pGetSystemMetrics.Call(smCY)
	x := (int32(sw) - winW) / 2
	y := (int32(sh) - winH) / 2

	hwnd, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(title)),
		wsCaption|wsSysMenu|wsMinimizeBox,
		uintptr(x), uintptr(y), uintptr(winW), uintptr(winH),
		0, 0, moduleHandle(), 0,
	)
	if hwnd == 0 {
		return 0
	}

	// 静态文本（多行自动换行）
	staticCls, _ := syscall.UTF16PtrFromString("STATIC")
	t, _ := syscall.UTF16PtrFromString(text)
	staticHwnd, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(t)),
		wsChild|wsVisible|ssCenter,
		18, uintptr(staticY), uintptr(contentW), uintptr(staticH),
		hwnd, 0, moduleHandle(), 0,
	)
	if staticHwnd != 0 {
		pSendMessageW.Call(staticHwnd, wmSetFont, font, 1)
	}

	// 确定按钮（置于文本下方）
	btnCls, _ := syscall.UTF16PtrFromString("BUTTON")
	btnText, _ := syscall.UTF16PtrFromString("确定")
	btnHwnd, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(btnCls)),
		uintptr(unsafe.Pointer(btnText)),
		wsChild|wsVisible|wsTabStop,
		uintptr((contentW-100)/2+18), uintptr(btnY), 100, uintptr(btnH),
		hwnd, idOkButton, moduleHandle(), 0,
	)
	if btnHwnd != 0 {
		pSendMessageW.Call(btnHwnd, wmSetFont, font, 1)
	}

	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	return hwnd
}

// startSplash 在独立线程显示 loading 窗口，返回关闭函数。
func startSplash(text string) func() {
	done := make(chan uintptr, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		hwnd := createSplashWindow(text)
		done <- hwnd
		if hwnd == 0 {
			return
		}
		for {
			var m msg
			ret, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(ret) <= 0 {
				break
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
	hwnd := <-done
	return func() {
		if hwnd != 0 {
			pPostMessageW.Call(hwnd, wmClose, 0, 0)
		}
	}
}
