//go:build windows

package main

import (
	"math"
	"os"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// 本文件从原 splash.go 抽取：现代弹窗（runModernDialog）及其依赖的 Win32/GDI+ 工具，
// 供平台层（托盘退出询问、更新询问、就绪提示等）复用。UI 自绘窗口已由 Wails 前端取代。

const (
	wmClose          = 0x0010
	wmDestroy        = 0x0002
	wmPaint          = 0x000F
	wmCommand        = 0x0111
	wmSetFont        = 0x0030
	wmSetIcon        = 0x0080
	wmCtlColorStatic = 0x0138
	wmSetText        = 0x000C
	wmDrawItem       = 0x002B

	iconBig   = 1
	iconSmall = 0

	swShow                     = 5
	wsCaption                  = 0x00C00000
	wsSysMenu                  = 0x00080000
	wsClipChildren             = 0x02000000
	wsChild                    = 0x40000000
	wsVisible                  = 0x10000000
	wsTabStop                  = 0x00010000
	ssCenter                   = 0x00000001
	csHRedraw                  = 0x0002
	csVRedraw                  = 0x0001
	idcArrow                   = 32512
	colorWin                   = 6 // COLOR_WINDOW + 1（白色窗口背景）
	smCX                       = 0
	smCY                       = 1
	bsOwnDraw                  = 0x000B
	odsSelected                = 0x0001
	psSolid                    = 0
	dtCenter                   = 0x0001
	dtVCenter                  = 0x0004
	dtSingle                   = 0x0020
	dtSingleLine               = 0x0020
	dtEndEllipsis              = 0x8000
	dtWordBreak                = 0x0010
	dtCalcRect                 = 0x0400
	srccopy                    = 0x00CC0020
	defaultCharset             = 1
	cleartypeQual              = 5
	antialiasQual              = 4 // 灰度抗锯齿（彩色底上避免 ClearType 彩边）
	bkTransparent              = 1
	bkOpaque                   = 0
	whiteBrush                 = 0
	dwmcWindowCornerPreference = 33
	dwmcRound                  = 2

	dlgPad    = 20
	dlgW      = 380
	dlgBtnH   = 30
	dlgBtnW   = 80
	dlgBtnGap = 16

	dialogColorMsg   = 0x00857066 // #667085
	dialogColorTxt   = 0x00281810 // #101828
	colorWhite       = 0x00FFFFFF
	dialogColorWhite = 0x00FFFFFF
	// 主/次按钮填充（COLORREF=0xBBGGRR）
	dialogColorPrim    = 0x00D84E1D // #1D4ED8 主按钮品牌蓝
	dialogColorPrimSel = 0x00AF401E // #1E40AF 按压加深
	dialogColorGray    = 0x00F7F3F1 // #F1F3F7
	dialogColorGraySel = 0x00ECE6E2 // #E2E6EC
	dialogColorBorder  = 0x00E8E0DC // #DCE0E8
)

var (
	modUser32              = syscall.NewLazyDLL("user32.dll")
	modKernel32            = syscall.NewLazyDLL("kernel32.dll")
	modGdi32               = syscall.NewLazyDLL("gdi32.dll")
	modDwmapi              = syscall.NewLazyDLL("dwmapi.dll")
	modShell32             = syscall.NewLazyDLL("shell32.dll")
	pRegisterClassExW      = modUser32.NewProc("RegisterClassExW")
	pCreateWindowExW       = modUser32.NewProc("CreateWindowExW")
	pDefWindowProcW        = modUser32.NewProc("DefWindowProcW")
	pGetMessageW           = modUser32.NewProc("GetMessageW")
	pTranslateMessage      = modUser32.NewProc("TranslateMessage")
	pDispatchMessageW      = modUser32.NewProc("DispatchMessageW")
	pPostQuitMessage       = modUser32.NewProc("PostQuitMessage")
	pShowWindow            = modUser32.NewProc("ShowWindow")
	pUpdateWindow          = modUser32.NewProc("UpdateWindow")
	pBeginPaint            = modUser32.NewProc("BeginPaint")
	pEndPaint              = modUser32.NewProc("EndPaint")
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
	pSetBkColor            = modGdi32.NewProc("SetBkColor")
	pAdjustWindowRectEx    = modUser32.NewProc("AdjustWindowRectEx")
	pMoveWindow            = modUser32.NewProc("MoveWindow")
	pCreateSolidBrush      = modGdi32.NewProc("CreateSolidBrush")
	pDeleteObject          = modGdi32.NewProc("DeleteObject")
	pGetClientRect         = modUser32.NewProc("GetClientRect")
	pCreateCompatibleDC    = modGdi32.NewProc("CreateCompatibleDC")
	pCreateCompatibleBmp   = modGdi32.NewProc("CreateCompatibleBitmap")
	pDeleteDC              = modGdi32.NewProc("DeleteDC")
	pBitBlt                = modGdi32.NewProc("BitBlt")
	pGetTextMetrics        = modGdi32.NewProc("GetTextMetricsA")
	pAddFontResourceExW    = modGdi32.NewProc("AddFontResourceExW")
	pDwmSetWindowAttribute = modDwmapi.NewProc("DwmSetWindowAttribute")
	// GDI+（抗锯齿绘图）
	modGdiplus                = syscall.NewLazyDLL("gdiplus.dll")
	gpStartup                 = modGdiplus.NewProc("GdiplusStartup")
	gpShutdown                = modGdiplus.NewProc("GdiplusShutdown")
	gpFromHDC                 = modGdiplus.NewProc("GdipCreateFromHDC")
	gpDelGraphics             = modGdiplus.NewProc("GdipDeleteGraphics")
	gpSmooth                  = modGdiplus.NewProc("GdipSetSmoothingMode")
	gpCreatePath              = modGdiplus.NewProc("GdipCreatePath")
	gpDelPath                 = modGdiplus.NewProc("GdipDeletePath")
	gpAddArc                  = modGdiplus.NewProc("GdipAddPathArc")
	gpCloseFig                = modGdiplus.NewProc("GdipClosePathFigure")
	gpCreateSolid             = modGdiplus.NewProc("GdipCreateSolidFill")
	gpDelBrush                = modGdiplus.NewProc("GdipDeleteBrush")
	gpFillPath                = modGdiplus.NewProc("GdipFillPath")
	pGetDC                    = modUser32.NewProc("GetDC")
	pReleaseDC                = modUser32.NewProc("ReleaseDC")
	pExtractIconW             = modShell32.NewProc("ExtractIconW")
	pDrawTextW                = modUser32.NewProc("DrawTextW")
	pGetTextFaceW             = modGdi32.NewProc("GetTextFaceW")
	pFillRect                 = modUser32.NewProc("FillRect")
	pCreateDIBSection         = modGdi32.NewProc("CreateDIBSection")
	pGdiFlush                 = modGdi32.NewProc("GdiFlush")
	pUpdateLayeredWindow      = modUser32.NewProc("UpdateLayeredWindow")
	pCreatePen                = modGdi32.NewProc("CreatePen")
	pSelectObject             = modGdi32.NewProc("SelectObject")
	pRoundRect                = modGdi32.NewProc("RoundRect")
	pSetForegroundWindow      = modUser32.NewProc("SetForegroundWindow")
	pSetWindowPos             = modUser32.NewProc("SetWindowPos")
	pBringWindowToTop         = modUser32.NewProc("BringWindowToTop")
	pSetActiveWindow          = modUser32.NewProc("SetActiveWindow")
	pGetForegroundWindow      = modUser32.NewProc("GetForegroundWindow")
	pGetWindowThreadProcessId = modUser32.NewProc("GetWindowThreadProcessId")
	pAttachThreadInput        = modUser32.NewProc("AttachThreadInput")
)

// ==================== 前台（自动置顶） ====================
// 本文件全部自绘弹窗与平台层原生对话框（pickHarnessDir 等）都要求自动前台：
// 任何新增弹窗必须经 forceForeground / 传入 hwndOwner，规则见 DESIGN.md 第 6 节。

// forceForeground 把窗口可靠地带到前台：
//  1. 置顶（HWND_TOPMOST）闪烁后撤销，确保 z 序跳到最前；
//  2. SetForegroundWindow 失败时（前台被其它进程占用），把本窗口线程 AttachThreadInput
//     挂到当前前台线程再抢一次（Windows 前台锁定常见兜底）。
func forceForeground(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	const (
		hwndTopmost   = ^uintptr(0)     // -1：置顶
		hwndNotopmost = ^uintptr(0) - 1 // -2：取消置顶
		swpNomove     = 0x0002
		swpNosize     = 0x0001
		swpNoactivate = 0x0010
		swpShowWindow = 0x0040
	)
	pSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNomove|swpNosize|swpShowWindow)
	pSetWindowPos.Call(hwnd, hwndNotopmost, 0, 0, 0, 0, swpNomove|swpNosize)
	pBringWindowToTop.Call(hwnd)
	pSetActiveWindow.Call(hwnd)
	if r, _, _ := pSetForegroundWindow.Call(hwnd); r == 0 {
		// 前台被其它进程占用：把本窗口线程临时挂到前台线程再抢一次
		fore, _, _ := pGetForegroundWindow.Call()
		if fore != 0 && fore != hwnd {
			fThread, _ := getWindowThreadID(fore)
			tThread, _ := getWindowThreadID(hwnd)
			if fThread != 0 && tThread != 0 && fThread != tThread {
				pAttachThreadInput.Call(uintptr(fThread), uintptr(tThread), 1)
				pBringWindowToTop.Call(hwnd)
				pSetForegroundWindow.Call(hwnd)
				pAttachThreadInput.Call(uintptr(fThread), uintptr(tThread), 0)
			}
		}
	}
}

// getWindowThreadID 返回窗口所属线程 id（GetWindowThreadProcessId 的返回值即线程 id）。
func getWindowThreadID(hwnd uintptr) (uint32, bool) {
	r, _, _ := pGetWindowThreadProcessId.Call(hwnd, 0)
	return uint32(r), uint32(r) != 0
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

type paintStruct struct {
	hdc         uintptr
	fErase      int32
	rcPaint     rect
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

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
	dialogBtnFont       uintptr
	dialogClassRegister bool
)

// ==================== 字体 ====================

// 首选 Google Noto Sans SC（中英文统一、现代）；系统未安装则回退微软雅黑 UI / Segoe UI。
var fontCandidates = []string{"Noto Sans SC", "Microsoft YaHei UI", "Segoe UI"}

func selectedFace() string {
	for _, name := range fontCandidates {
		face, _ := syscall.UTF16PtrFromString(name)
		h, _, _ := pCreateFontW.Call(20, 0, 0, 0, 400, 0, 0, 0, defaultCharset, 0, 0, cleartypeQual, 0, uintptr(unsafe.Pointer(face)))
		if h == 0 {
			continue
		}
		dc, _, _ := pGetDC.Call(0)
		if dc == 0 {
			pDeleteObject.Call(h)
			continue
		}
		old, _, _ := pSelectObject.Call(dc, h)
		var buf [64]uint16
		n, _, _ := pGetTextFaceW.Call(dc, 64, uintptr(unsafe.Pointer(&buf[0])))
		pSelectObject.Call(dc, old)
		pReleaseDC.Call(0, dc)
		pDeleteObject.Call(h)
		if n > 0 && strings.EqualFold(syscall.UTF16ToString(buf[:n]), name) {
			return name
		}
	}
	return "Microsoft YaHei UI"
}

// makeFont 创建与网站同源的字体（height 像素、weight 400/600）。
func makeFont(height, weight int32) uintptr {
	return makeFontQuality(height, weight, cleartypeQual)
}

// makeSystemFont 使用系统默认 UI 字体（微软雅黑 UI），用于动态文本（如弹窗错误信息/文件路径）。
func makeSystemFont(height, weight int32) uintptr {
	face, _ := syscall.UTF16PtrFromString("Microsoft YaHei UI")
	h, _, _ := pCreateFontW.Call(uintptr(height), 0, 0, 0, uintptr(weight), 0, 0, 0, defaultCharset, 0, 0, cleartypeQual, 0, uintptr(unsafe.Pointer(face)))
	return h
}

// makeFontQuality 带指定抗锯齿质量的字体（quality：cleartypeQual 亚像素 / antialiasQual 灰度）。
func makeFontQuality(height, weight int32, quality uintptr) uintptr {
	face, _ := syscall.UTF16PtrFromString(selectedFace())
	h, _, _ := pCreateFontW.Call(uintptr(height), 0, 0, 0, uintptr(weight), 0, 0, 0, defaultCharset, 0, 0, quality, 0, uintptr(unsafe.Pointer(face)))
	return h
}

func moduleHandle() uintptr {
	h, _, _ := pGetModuleHandleW.Call(0)
	return h
}

// setWindowIcon 给窗口标题栏设置应用图标（提取自身 exe 的图标）。
func setWindowIcon(hwnd uintptr) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	exePtr, _ := syscall.UTF16PtrFromString(exe)
	hicon, _, _ := pExtractIconW.Call(0, uintptr(unsafe.Pointer(exePtr)), 0)
	if hicon != 0 {
		pSendMessageW.Call(hwnd, wmSetIcon, iconBig, hicon)
		pSendMessageW.Call(hwnd, wmSetIcon, iconSmall, hicon)
	}
}

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

// ==================== GDI+ 圆角工具 ====================

var (
	gdiplusToken uintptr
	gdiplusReady bool
)

// fb 把 float32 转成 GDI+ 需要的 REAL 参数。
func fb(f float32) uintptr { return uintptr(math.Float32bits(f)) }

// ensureGdiplus 一次性启动 GDI+。
func ensureGdiplus() bool {
	if gdiplusReady {
		return true
	}
	input := struct {
		gdiplusVersion           uint32
		debugEventCallback       uintptr
		suppressBackgroundThread uint32
		suppressExternalCodecs   uint32
	}{1, 0, 0, 0}
	if ret, _, _ := gpStartup.Call(uintptr(unsafe.Pointer(&gdiplusToken)), uintptr(unsafe.Pointer(&input)), 0); ret != 0 {
		return false
	}
	gdiplusReady = true
	return true
}

// fillRoundedRectAA 用 GDI+ 抗锯齿绘制圆角矩形（r 上限为高度一半）。
func fillRoundedRectAA(hdc uintptr, rc rect, radius int32, argb uint32) {
	if !ensureGdiplus() {
		return
	}
	var g uintptr
	if gpFromHDC.Call(hdc, uintptr(unsafe.Pointer(&g))); g == 0 {
		return
	}
	defer gpDelGraphics.Call(g)
	gpSmooth.Call(g, 2) // SmoothingModeAntiAlias

	w := rc.right - rc.left
	h := rc.bottom - rc.top
	r := radius
	if r > h/2 {
		r = h / 2
	}
	if r < 1 {
		r = 1
	}
	var path uintptr
	if gpCreatePath.Call(0, uintptr(unsafe.Pointer(&path))); path == 0 {
		return
	}
	defer gpDelPath.Call(path)
	d := 2 * r
	gpAddArc.Call(path, fb(float32(rc.left)), fb(float32(rc.top)), fb(float32(d)), fb(float32(d)), fb(180), fb(90))
	gpAddArc.Call(path, fb(float32(rc.left+w-d)), fb(float32(rc.top)), fb(float32(d)), fb(float32(d)), fb(270), fb(90))
	gpAddArc.Call(path, fb(float32(rc.left+w-d)), fb(float32(rc.top+h-d)), fb(float32(d)), fb(float32(d)), fb(0), fb(90))
	gpAddArc.Call(path, fb(float32(rc.left)), fb(float32(rc.top+h-d)), fb(float32(d)), fb(float32(d)), fb(90), fb(90))
	gpCloseFig.Call(path)

	var brush uintptr
	if gpCreateSolid.Call(uintptr(argb), uintptr(unsafe.Pointer(&brush))); brush == 0 {
		return
	}
	defer gpDelBrush.Call(brush)
	gpFillPath.Call(g, brush, path)
}

// colorRefToARGB 把 COLORREF（0xBBGGRR）转为 ARGB。
func colorRefToARGB(c uintptr) uint32 {
	return 0xFF000000 | uint32((c&0xFF)<<16) | uint32(c&0xFF00) | uint32((c&0xFF0000)>>16)
}

// ==================== 现代弹窗 ====================

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
		pSetBkColor.Call(wParam, colorWhite)
		pSetBkMode.Call(wParam, bkOpaque)
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
		var fillColor uintptr
		var textColor uintptr
		if primary {
			fillColor = dialogColorPrim
			if pressed {
				fillColor = dialogColorPrimSel
			}
			textColor = dialogColorWhite
		} else {
			fillColor = dialogColorGray
			if pressed {
				fillColor = dialogColorGraySel
			}
			textColor = dialogColorTxt
		}
		hdc := dis.hDC
		if wb, _, _ := pGetStockObject.Call(whiteBrush); wb != 0 {
			pFillRect.Call(hdc, uintptr(unsafe.Pointer(&dis.rcItem)), wb)
		}
		fillRoundedRectAA(hdc, dis.rcItem, 20, colorRefToARGB(fillColor))
		pSetTextColor.Call(hdc, textColor)
		pSetBkMode.Call(hdc, bkTransparent)
		if dialogBtnFont != 0 {
			pSelectObject.Call(hdc, dialogBtnFont)
		}
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
	dialogMsgFont = makeSystemFont(16, 400)
	dialogBtnFont = makeFont(14, 600)

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

func createDialogWindow(caption, message string) uintptr {
	cls, _ := syscall.UTF16PtrFromString("DSH_Systray_Dialog")
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
	if msgH < 20 {
		msgH = 20
	}
	// 高度余量：容纳 descender 与偶发多一行
	msgH += 6
	btnY := int32(dlgPad) + int32(msgH) + 12
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
	msgHwnd, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(mt)),
		wsChild|wsVisible|ssCenter,
		uintptr(dlgPad), uintptr(dlgPad), uintptr(innerW), uintptr(msgH),
		hwnd, 200, moduleHandle(), 0,
	)
	if msgHwnd != 0 {
		pSendMessageW.Call(msgHwnd, wmSetFont, dialogMsgFont, 1)
	}

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
		if hb != 0 {
			pSendMessageW.Call(hb, wmSetFont, dialogBtnFont, 1)
		}
		dialogButtons = append(dialogButtons, hb)
	}

	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	setWindowIcon(hwnd)
	forceForeground(hwnd) // 弹窗必须自动前台（见本文件头部规则）
	return hwnd
}

// buttonsW 兼容旧调用：未初始化时返回固定 1。
func buttonsW() []string {
	if len(dialogLabels) == 0 {
		return []string{"确定"}
	}
	return dialogLabels
}
