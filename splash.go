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

	defaultCharset = 1
	cleartypeQual  = 5

	bkTransparent = 1
	whiteBrush    = 0

	// 控件 ID
	idStatus = 10
	idTrack  = 11
	idFill   = 12

	// 布局（客户区坐标）
	pad      = 18
	contentW = 424
	statusY  = 16
	statusH  = 30
	trackY   = 64
	trackH   = 10
	fillY    = 66
	fillH    = 6
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
	pMoveWindow         = modUser32.NewProc("MoveWindow")
	pCreateSolidBrush   = modGdi32.NewProc("CreateSolidBrush")
	pDeleteObject       = modGdi32.NewProc("DeleteObject")
)

// 当前进度窗口的控件句柄与画刷（同一时间只有一个 splash）
var (
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
	case wmCommand:
		if wParam&0xFFFF == idTrack {
			return 0
		}
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
		// 进度条轨道/填充用各自画刷，状态文本用白色（与窗口一致）
		switch lParam {
		case splashTrackHwnd:
			return splashTrackBrush
		case splashFillHwnd:
			return splashFillBrush
		}
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

	clientW := int32(contentW + pad*2)
	clientH := int32(trackY + trackH + 16)
	r := rect{0, 0, clientW, clientH}
	pAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&r)), wsCaption|wsSysMenu|wsMinimizeBox, 0, 0)
	// 用换算后的外框尺寸，避免客户区被边框挤压导致进度条裁切
	winW := r.right - r.left
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

	// 状态文本
	staticCls, _ := syscall.UTF16PtrFromString("STATIC")
	t, _ := syscall.UTF16PtrFromString(text)
	splashStatusHwnd, _, _ = pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(t)),
		wsChild|wsVisible|ssCenter,
		uintptr(pad), uintptr(statusY), uintptr(contentW), uintptr(statusH),
		hwnd, idStatus, moduleHandle(), 0,
	)
	if splashStatusHwnd != 0 {
		pSendMessageW.Call(splashStatusHwnd, wmSetFont, font, 1)
	}

	// 进度条轨道（灰色）
	splashTrackHwnd, _, _ = pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		0,
		wsChild|wsVisible,
		uintptr(pad), uintptr(trackY), uintptr(contentW), uintptr(trackH),
		hwnd, idTrack, moduleHandle(), 0,
	)

	// 进度条填充（品牌蓝，初始宽度 0）
	splashFillHwnd, _, _ = pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		0,
		wsChild|wsVisible,
		uintptr(pad), uintptr(fillY), 0, uintptr(fillH),
		hwnd, idFill, moduleHandle(), 0,
	)

	splashTrackBrush, _, _ = pCreateSolidBrush.Call(0xD9D9D9)
	splashFillBrush, _, _ = pCreateSolidBrush.Call(0xFE6B4D) // DeepSeek 蓝

	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	return hwnd
}

// startSplash 显示带进度条的等待窗口，返回控制器。
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
			pMoveWindow.Call(splashFillHwnd, uintptr(pad), uintptr(fillY), w, uintptr(fillH), 1)
		}
	}
	st.Close = func() {
		if hwnd != 0 {
			pPostMessageW.Call(hwnd, wmClose, 0, 0)
		}
	}
	return st
}
