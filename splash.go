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
	wsTabStop = 0x00010000
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

	// 单色冷灰令牌（COLORREF = 0xBBGGRR，与图标配色一致）
	colorTitle  = 0x00242120 // #202124 标题近黑
	colorStatus = 0x0068635F // #5F6368 次要灰
	colorTrack  = 0x00EDEAE8 // #E8EAED 轨道浅灰
	colorFill   = 0x00FE6B4D // #4D6BFE 进度填充品牌蓝

	// 布局（客户区坐标）
	pad      = 20
	contentW = 380
	titleY   = 22
	titleH   = 28
	statusY  = 54
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
	pGetDC                 = modUser32.NewProc("GetDC")
	pReleaseDC             = modUser32.NewProc("ReleaseDC")
	pDrawTextW             = modUser32.NewProc("DrawTextW")
	pCreatePen             = modGdi32.NewProc("CreatePen")
	pSelectObject          = modGdi32.NewProc("SelectObject")
	pRoundRect             = modGdi32.NewProc("RoundRect")
)

// 当前进度窗口的控件句柄与画刷（同一时间只有一个 splash）
var (
	splashTitleHwnd  uintptr
	splashStatusHwnd uintptr
	splashTrackHwnd  uintptr
	splashFillHwnd   uintptr
	splashTrackBrush uintptr
	splashFillBrush  uintptr
	splashStatusH    int32
	splashBarY       int32
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

	// 状态文本换行高度自适应，进度条随之下移
	msgH := measureMultilineHeight(statusText, contentW, statusFont)
	if msgH < 24 {
		msgH = 24
	}
	if msgH > 64 {
		msgH = 64
	}
	msgH += 4
	splashStatusH = int32(msgH)
	splashBarY = int32(statusY) + splashStatusH + 14

	clientW := int32(contentW + pad*2)
	clientH := splashBarY + int32(barH) + 22
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
		uintptr(pad), uintptr(statusY), uintptr(contentW), uintptr(splashStatusH),
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
		uintptr(pad), uintptr(splashBarY), uintptr(contentW), uintptr(barH),
		hwnd, idTrack, moduleHandle(), 0,
	)

	// 进度条填充（品牌蓝，初始宽度 0）
	splashFillHwnd, _, _ = pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		0,
		wsChild|wsVisible,
		uintptr(pad), uintptr(splashBarY), 0, uintptr(barH),
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
			pMoveWindow.Call(splashFillHwnd, uintptr(pad), uintptr(splashBarY), w, uintptr(barH), 1)
		}
	}
	st.Close = func() {
		if hwnd != 0 {
			pPostMessageW.Call(hwnd, wmClose, 0, 0)
		}
	}
	return st
}

// ==================== 现代弹窗（苹果风：圆角窗口 + 圆角按钮） ====================
const (
	dialogCls   = "DSH_Systray_Dialog"
	wmDrawItem  = 0x002B
	bsOwnDraw   = 0x000B
	odsSelected = 0x0001
	psSolid     = 0
	dtCenter    = 0x0001
	dtVCenter   = 0x0004
	dtSingle    = 0x0020
	dtWordBreak = 0x0010
	dtCalcRect  = 0x0400

	dlgPad    = 24
	dlgW      = 380
	dlgBtnH   = 30
	dlgBtnW   = 84
	dlgBtnGap = 16

	dialogColorMsg   = 0x0068635F // #5F6368
	dialogColorTxt   = 0x00242120 // #202124
	dialogColorWhite = 0x00FFFFFF
	// 主/次按钮填充（COLORREF=0xBBGGRR）
	dialogColorPrim    = 0x00FE6B4D // #4D6BFE 主按钮品牌蓝
	dialogColorPrimSel = 0x00E04F3A // #3A4FE0 按压加深
	dialogColorGray    = 0x00F6F4F3 // #F3F4F6
	dialogColorGraySel = 0x00EBE7E5 // #E5E7EB
	dialogColorBorder  = 0x00D5D1DB // #D1D5DB
)

type drawItemStruct struct {
	ctlType    uint32
	ctlID      uint32
	itemID     uint32
	itemAction uint32
	itemState  uint32
	hwndItem   uintptr
	hDC        uintptr
	rcItem     rect
	itemData   uintptr
}

var (
	dialogHwnd          uintptr
	dialogButtons       []uintptr
	dialogLabels        []string
	dialogPrimary       int
	dialogResult        int
	dialogBlueBrush     uintptr
	dialogBlueSelBrush  uintptr
	dialogGrayBrush     uintptr
	dialogGraySelBrush  uintptr
	dialogMsgFont       uintptr
	dialogClassRegister bool
)

// measureMultilineHeight 计算多行文本换行后高度。
func measureMultilineHeight(text string, width int, font uintptr) int {
	dc, _, _ := pGetDC.Call(0)
	if dc == 0 {
		return 20
	}
	defer pReleaseDC.Call(0, dc)
	old, _, _ := pSelectObject.Call(dc, font)
	defer pSelectObject.Call(dc, old)
	rc := rect{0, 0, int32(width), 0}
	t, _ := syscall.UTF16PtrFromString(text)
	pDrawTextW.Call(dc, uintptr(unsafe.Pointer(t)), ^uintptr(0), uintptr(unsafe.Pointer(&rc)), dtCalcRect|dtWordBreak)
	return int(rc.bottom - rc.top)
}

func dialogWndProc(hwnd, uMsg, wParam, lParam uintptr) uintptr {
	switch uMsg {
	case wmCommand:
		id := int(wParam & 0xFFFF)
		if id >= 1000 && id-1000 < len(dialogButtons) {
			dialogResult = id - 1000
			pDestroyWindow.Call(hwnd)
			return 0
		}
	case wmClose:
		dialogResult = -1
		pDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		if dialogBlueBrush != 0 {
			pDeleteObject.Call(dialogBlueBrush)
		}
		if dialogBlueSelBrush != 0 {
			pDeleteObject.Call(dialogBlueSelBrush)
		}
		if dialogGrayBrush != 0 {
			pDeleteObject.Call(dialogGrayBrush)
		}
		if dialogGraySelBrush != 0 {
			pDeleteObject.Call(dialogGraySelBrush)
		}
		pPostQuitMessage.Call(0)
		return 0
	case wmCtlColorStatic:
		pSetTextColor.Call(wParam, dialogColorMsg)
		pSetBkMode.Call(wParam, bkTransparent)
		h, _, _ := pGetStockObject.Call(whiteBrush)
		return h
	case wmDrawItem:
		if lParam == 0 {
			break
		}
		dis := *(*drawItemStruct)(unsafe.Add(unsafe.Pointer(nil), lParam))
		idx := -1
		for i, h := range dialogButtons {
			if h == dis.hwndItem {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		primary := idx == dialogPrimary
		pressed := dis.itemState&odsSelected != 0
		var fill uintptr
		var textColor uintptr
		if primary {
			fill = dialogBlueBrush
			if pressed {
				fill = dialogBlueSelBrush
			}
			textColor = dialogColorWhite
		} else {
			fill = dialogGrayBrush
			if pressed {
				fill = dialogGraySelBrush
			}
			textColor = dialogColorTxt
		}
		hdc := dis.hDC
		hPen, _, _ := pCreatePen.Call(psSolid, 1, dialogColorBorder)
		oldPen, _, _ := pSelectObject.Call(hdc, hPen)
		oldBrush, _, _ := pSelectObject.Call(hdc, fill)
		pRoundRect.Call(hdc, uintptr(dis.rcItem.left), uintptr(dis.rcItem.top), uintptr(dis.rcItem.right), uintptr(dis.rcItem.bottom), 8, 8)
		pSelectObject.Call(hdc, oldBrush)
		pSelectObject.Call(hdc, oldPen)
		pDeleteObject.Call(hPen)
		pSetTextColor.Call(hdc, textColor)
		pSetBkMode.Call(hdc, bkTransparent)
		label, _ := syscall.UTF16PtrFromString(dialogLabels[idx])
		pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(label)), ^uintptr(0), uintptr(unsafe.Pointer(&dis.rcItem)), dtCenter|dtVCenter|dtSingle)
		return 1
	}
	ret, _, _ := pDefWindowProcW.Call(hwnd, uMsg, wParam, lParam)
	return ret
}

// runModernDialog 显示现代弹窗，返回被点按钮索引（-1=关闭）。
func runModernDialog(caption, message string, buttons []string, primary int) int {
	if len(buttons) == 0 {
		buttons = []string{"确定"}
	}
	if primary < 0 || primary >= len(buttons) {
		primary = 0
	}
	dialogLabels = buttons
	dialogPrimary = primary
	dialogButtons = nil
	dialogResult = -1
	dialogMsgFont = makeFont(13, 400)

	resultCh := make(chan int, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		hwnd := createDialogWindow(caption, message)
		if hwnd == 0 {
			resultCh <- -1
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
		resultCh <- dialogResult
	}()
	return <-resultCh
}

// measureTextWidth 计算单行文本宽度（用于弹窗宽度自适应）。
func measureTextWidth(text string, font uintptr) int {
	dc, _, _ := pGetDC.Call(0)
	if dc == 0 {
		return 20
	}
	defer pReleaseDC.Call(0, dc)
	old, _, _ := pSelectObject.Call(dc, font)
	defer pSelectObject.Call(dc, old)
	rc := rect{0, 0, int32(dlgW), 0}
	t, _ := syscall.UTF16PtrFromString(text)
	pDrawTextW.Call(dc, uintptr(unsafe.Pointer(t)), ^uintptr(0), uintptr(unsafe.Pointer(&rc)), dtCalcRect|dtSingle)
	return int(rc.right - rc.left)
}

func createDialogWindow(caption, message string) uintptr {
	cls, _ := syscall.UTF16PtrFromString(dialogCls)
	cb := syscall.NewCallback(dialogWndProc)
	cur, _, _ := pLoadCursorW.Call(0, idcArrow)

	if !dialogClassRegister {
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
		dialogClassRegister = true
	}

	// 宽度/高度均按文本自适应：短文本收窄，长文本按内容列宽换行
	msgW := measureTextWidth(message, dialogMsgFont)
	totalBtnW := len(buttonsW())*dlgBtnW + (len(buttonsW())-1)*dlgBtnGap
	innerW := int32(dlgW)
	// 宽度头部余量，避免文本贴着控件边缘被强迫换行而裁切
	needed := int32(msgW) + 28
	if needed < int32(totalBtnW) {
		needed = int32(totalBtnW)
	}
	if needed < innerW {
		innerW = needed
	}
	clientW := innerW + 2*dlgPad
	msgH := measureMultilineHeight(message, int(innerW), dialogMsgFont)
	if msgH < 22 {
		msgH = 22
	}
	// 高度余量：容纳多行文本的 descender 与偶发多一行
	msgH += 10
	btnY := int32(dlgPad) + int32(msgH) + 14
	clientH := btnY + int32(dlgBtnH) + int32(dlgPad)

	r := rect{0, 0, clientW, clientH}
	pAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&r)), wsCaption|wsSysMenu, 0, 0)
	winW := r.right - r.left
	winH := r.bottom - r.top

	sw, _, _ := pGetSystemMetrics.Call(smCX)
	sh, _, _ := pGetSystemMetrics.Call(smCY)
	x := (int32(sw) - winW) / 2
	y := (int32(sh) - winH) / 2

	capPtr, _ := syscall.UTF16PtrFromString(caption)
	hwnd, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(capPtr)),
		wsCaption|wsSysMenu,
		uintptr(x), uintptr(y), uintptr(winW), uintptr(winH),
		0, 0, moduleHandle(), 0,
	)
	if hwnd == 0 {
		return 0
	}
	dialogHwnd = hwnd
	corner := uintptr(dwmcRound)
	pDwmSetWindowAttribute.Call(hwnd, dwmcWindowCornerPreference, uintptr(unsafe.Pointer(&corner)), unsafe.Sizeof(corner))

	// 消息文本
	staticCls, _ := syscall.UTF16PtrFromString("STATIC")
	mt, _ := syscall.UTF16PtrFromString(message)
	pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(mt)),
		wsChild|wsVisible|ssCenter,
		uintptr(dlgPad), uintptr(dlgPad), uintptr(innerW), uintptr(msgH),
		hwnd, 200, moduleHandle(), 0,
	)

	// 圆角按钮（自绘）
	dialogBlueBrush, _, _ = pCreateSolidBrush.Call(dialogColorPrim)
	dialogBlueSelBrush, _, _ = pCreateSolidBrush.Call(dialogColorPrimSel)
	dialogGrayBrush, _, _ = pCreateSolidBrush.Call(dialogColorGray)
	dialogGraySelBrush, _, _ = pCreateSolidBrush.Call(dialogColorGraySel)
	btnCls, _ := syscall.UTF16PtrFromString("BUTTON")
	btnStartX := int32(dlgPad) + (innerW-int32(totalBtnW))/2
	for i, label := range dialogLabels {
		bt, _ := syscall.UTF16PtrFromString(label)
		btnX := int32(btnStartX) + int32(i)*(dlgBtnW+dlgBtnGap)
		hb, _, _ := pCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(btnCls)),
			uintptr(unsafe.Pointer(bt)),
			wsChild|wsVisible|wsTabStop|bsOwnDraw,
			uintptr(btnX), uintptr(btnY), uintptr(dlgBtnW), uintptr(dlgBtnH),
			hwnd, uintptr(1000+i), moduleHandle(), 0,
		)
		dialogButtons = append(dialogButtons, hb)
	}

	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	return hwnd
}

// buttonsW 兼容旧调用：未初始化时返回固定 1。
func buttonsW() []string {
	if len(dialogLabels) == 0 {
		return []string{"确定"}
	}
	return dialogLabels
}
