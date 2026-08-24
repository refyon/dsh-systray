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
	wmSetText        = 0x000C

	swShow    = 5
	wsCaption = 0x00C00000
	wsSysMenu = 0x00080000
	wsChild   = 0x40000000
	wsVisible = 0x10000000
	ssCenter  = 0x00000001
	csHRedraw = 0x0002
	csVRedraw = 0x0001
	idcArrow  = 32512
	colorWin  = 6 // COLOR_WINDOW + 1（白色窗口背景）
	smCX      = 0
	smCY      = 1

	defaultCharset = 1
	cleartypeQual  = 5

	bkTransparent = 1
	whiteBrush    = 0

	// DWM 圆角（Windows 11）
	dwmcWindowCornerPreference = 33
	dwmcRound                  = 2

	// 控件 ID
	idTitle  = 10
	idStatus = 11
	idTrack  = 12
	idFill   = 13

	// 苹果风浅色界面令牌（COLORREF = 0xBBGGRR）
	colorTitle  = 0x00271811 // #111827 标题近黑
	colorStatus = 0x0080726B // #6B7280 次要灰
	colorTrack  = 0x00EBE7E5 // #E5E7EB 轨道浅灰
	colorFill   = 0x00FE6B4D // #4D6BFE DeepSeek 蓝

	// 布局（客户区坐标）
	pad      = 20
	contentW = 380
	titleY   = 22
	titleH   = 28
	statusY  = 54
	statusH  = 20
	barY     = 90
	barH     = 6
)

var (
	modUser32              = syscall.NewLazyDLL("user32.dll")
	modKernel32            = syscall.NewLazyDLL("kernel32.dll")
	modGdi32               = syscall.NewLazyDLL("gdi32.dll")
	modDwmapi              = syscall.NewLazyDLL("dwmapi.dll")
	pRegisterClassExW      = modUser32.NewProc("RegisterClassExW")
	pCreateWindowExW       = modUser32.NewProc("CreateWindowExW")
	pDefWindowProcW        = modUser32.NewProc("DefWindowProcW")
	pGetMessageW           = modUser32.NewProc("GetMessageW")
	pTranslateMessage      = modUser32.NewProc("TranslateMessage")
	pDispatchMessageW      = modUser32.NewProc("DispatchMessageW")
	pPostQuitMessage       = modUser32.NewProc("PostQuitMessage")
	pShowWindow            = modUser32.NewProc("ShowWindow")
	pUpdateWindow          = modUser32.NewProc("UpdateWindow")
	pDestroyWindow         = modUser32.NewProc("DestroyWindow")
	pGetSystemMetrics      = modUser32.NewProc("GetSystemMetrics")
	pLoadCursorW           = modUser32.NewProc("LoadCursorW")
	pPostMessageW          = modUser32.NewProc("PostMessageW")
	pSendMessageW          = modUser32.NewProc("SendMessageW")
	pGetModuleHandleW      = modKernel32.NewProc("GetModuleHandleW")
	pCreateFontW           = modGdi32.NewProc("CreateFontW")
	pGetStockObject        = modGdi32.NewProc("GetStockObject")
	pSetTextColor          = modGdi32.NewProc("SetTextColor")
	pSetBkMode             = modGdi32.NewProc("SetBkMode")
	pAdjustWindowRectEx    = modUser32.NewProc("AdjustWindowRectEx")
	pMoveWindow            = modUser32.NewProc("MoveWindow")
	pCreateSolidBrush      = modGdi32.NewProc("CreateSolidBrush")
	pDeleteObject          = modGdi32.NewProc("DeleteObject")
	pDwmSetWindowAttribute = modDwmapi.NewProc("DwmSetWindowAttribute")
)

// 当前进度窗口的控件句柄与画刷（同一时间只有一个 splash）
var (
	splashTitleHwnd  uintptr
	splashStatusHwnd uintptr
	splashTrackHwnd  uintptr
	splashFillHwnd   uintptr
	splashTrackBrush uintptr
	splashFillBrush  uintptr
)

// SplashState 进度窗口控制器。
type SplashState struct {
	Update func(text string, fraction float64)
	Close  func()
}

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
	case wmClose:
		pDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		if splashTrackBrush != 0 {
			pDeleteObject.Call(splashTrackBrush)
		}
		if splashFillBrush != 0 {
			pDeleteObject.Call(splashFillBrush)
		}
		pPostQuitMessage.Call(0)
		return 0
	case wmCtlColorStatic:
		switch lParam {
		case splashTrackHwnd:
			return splashTrackBrush
		case splashFillHwnd:
			return splashFillBrush
		case splashTitleHwnd:
			pSetTextColor.Call(wParam, colorTitle)
			pSetBkMode.Call(wParam, bkTransparent)
			h, _, _ := pGetStockObject.Call(whiteBrush)
			return h
		case splashStatusHwnd:
			pSetTextColor.Call(wParam, colorStatus)
			pSetBkMode.Call(wParam, bkTransparent)
			h, _, _ := pGetStockObject.Call(whiteBrush)
			return h
		}
	}
	ret, _, _ := pDefWindowProcW.Call(hwnd, uMsg, wParam, lParam)
	return ret
}

func moduleHandle() uintptr {
	h, _, _ := pGetModuleHandleW.Call(0)
	return h
}

// makeFont 创建 Segoe UI 字体（height 像素、weight 400/600）。
func makeFont(height, weight int32) uintptr {
	face, _ := syscall.UTF16PtrFromString("Segoe UI Variable Display")
	h, _, _ := pCreateFontW.Call(uintptr(height), 0, 0, 0, uintptr(weight), 0, 0, 0, defaultCharset, 0, 0, cleartypeQual, 0, uintptr(unsafe.Pointer(face)))
	if h != 0 {
		return h
	}
	face2, _ := syscall.UTF16PtrFromString("Segoe UI")
	h2, _, _ := pCreateFontW.Call(uintptr(height), 0, 0, 0, uintptr(weight), 0, 0, 0, defaultCharset, 0, 0, cleartypeQual, 0, uintptr(unsafe.Pointer(face2)))
	return h2
}

func createSplashWindow(statusText string) uintptr {
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

	titleText, _ := syscall.UTF16PtrFromString("DeepSeek Harness")
	titleFont := makeFont(17, 600)
	statusFont := makeFont(13, 400)

	clientW := int32(contentW + pad*2)
	clientH := int32(barY + barH + 22)
	r := rect{0, 0, clientW, clientH}
	pAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&r)), wsCaption|wsSysMenu, 0, 0)
	winW := r.right - r.left
	winH := r.bottom - r.top

	sw, _, _ := pGetSystemMetrics.Call(smCX)
	sh, _, _ := pGetSystemMetrics.Call(smCY)
	x := (int32(sw) - winW) / 2
	y := (int32(sh) - winH) / 2

	hwnd, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(titleText)),
		wsCaption|wsSysMenu,
		uintptr(x), uintptr(y), uintptr(winW), uintptr(winH),
		0, 0, moduleHandle(), 0,
	)
	if hwnd == 0 {
		return 0
	}

	// Windows 11 圆角窗口（旧系统忽略失败）
	corner := uintptr(dwmcRound)
	pDwmSetWindowAttribute.Call(hwnd, dwmcWindowCornerPreference, uintptr(unsafe.Pointer(&corner)), unsafe.Sizeof(corner))

	// 标题（固定）
	staticCls, _ := syscall.UTF16PtrFromString("STATIC")
	splashTitleHwnd, _, _ = pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(titleText)),
		wsChild|wsVisible|ssCenter,
		uintptr(pad), uintptr(titleY), uintptr(contentW), uintptr(titleH),
		hwnd, idTitle, moduleHandle(), 0,
	)
	if splashTitleHwnd != 0 {
		pSendMessageW.Call(splashTitleHwnd, wmSetFont, titleFont, 1)
	}

	// 状态文本（随阶段更新）
	t, _ := syscall.UTF16PtrFromString(statusText)
	splashStatusHwnd, _, _ = pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(t)),
		wsChild|wsVisible|ssCenter,
		uintptr(pad), uintptr(statusY), uintptr(contentW), uintptr(statusH),
		hwnd, idStatus, moduleHandle(), 0,
	)
	if splashStatusHwnd != 0 {
		pSendMessageW.Call(splashStatusHwnd, wmSetFont, statusFont, 1)
	}

	// 进度条轨道（浅灰细条）
	splashTrackHwnd, _, _ = pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		0,
		wsChild|wsVisible,
		uintptr(pad), uintptr(barY), uintptr(contentW), uintptr(barH),
		hwnd, idTrack, moduleHandle(), 0,
	)

	// 进度条填充（品牌蓝，初始宽度 0）
	splashFillHwnd, _, _ = pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		0,
		wsChild|wsVisible,
		uintptr(pad), uintptr(barY), 0, uintptr(barH),
		hwnd, idFill, moduleHandle(), 0,
	)

	splashTrackBrush, _, _ = pCreateSolidBrush.Call(colorTrack)
	splashFillBrush, _, _ = pCreateSolidBrush.Call(colorFill)

	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	return hwnd
}

// startSplash 显示苹果风进度窗口，返回控制器。
func startSplash(text string) *SplashState {
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

	st := &SplashState{}
	st.Update = func(t string, f float64) {
		if splashStatusHwnd != 0 && t != "" {
			tp, _ := syscall.UTF16PtrFromString(t)
			pSendMessageW.Call(splashStatusHwnd, wmSetText, 0, uintptr(unsafe.Pointer(tp)))
		}
		if splashFillHwnd != 0 {
			if f < 0 {
				f = 0
			}
			if f > 1 {
				f = 1
			}
			w := uintptr(float64(contentW) * f)
			pMoveWindow.Call(splashFillHwnd, uintptr(pad), uintptr(barY), w, uintptr(barH), 1)
		}
	}
	st.Close = func() {
		if hwnd != 0 {
			pPostMessageW.Call(hwnd, wmClose, 0, 0)
		}
	}
	return st
}
