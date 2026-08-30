//go:build windows

package main

import (
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	wmClose          = 0x0010
	wmDestroy        = 0x0002
	wmPaint          = 0x000F
	wmCommand        = 0x0111
	wmSetFont        = 0x0030
	wmSetIcon        = 0x0080
	wmCtlColorStatic = 0x0138
	wmSetText        = 0x000C

	iconBig   = 1
	iconSmall = 0

	swShow          = 5
	wsCaption       = 0x00C00000
	wsSysMenu       = 0x00080000
	wsClipChildren  = 0x02000000
	wsChild         = 0x40000000
	wsVisible       = 0x10000000
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
	antialiasQual  = 4 // 灰度抗锯齿（彩色底上避免 ClearType 彩边，按钮文字用它更清晰）

	bkTransparent = 1
	bkOpaque      = 0
	whiteBrush    = 0

	// DWM 圆角（Windows 11）
	dwmcWindowCornerPreference = 33
	dwmcRound                  = 2

	// 控件 ID
	idTitle  = 10
	idStatus = 11

	// 单色冷灰令牌（COLORREF = 0xBBGGRR，与图标配色一致）
	colorTitle  = 0x00281810 // #101828 标题近黑
	colorWhite  = 0x00FFFFFF
	colorStatus = 0x00857066 // #667085 次要灰
	colorTrack  = 0x00ECE7E4 // #E4E7EC 轨道浅灰
	colorFill   = 0x00D84E1D // #1D4ED8 进度填充品牌蓝

	// 布局（客户区坐标）
	pad      = 20
	contentW = 380
	titleY   = 18
	titleH   = 30
	statusY  = 56
	barH     = 10 // 进度条高度（两端圆角胶囊）
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
	pDwmSetWindowAttribute = modDwmapi.NewProc("DwmSetWindowAttribute")
	// GDI+（抗锯齿绘图）
	modGdiplus    = syscall.NewLazyDLL("gdiplus.dll")
	gpStartup     = modGdiplus.NewProc("GdiplusStartup")
	gpShutdown    = modGdiplus.NewProc("GdiplusShutdown")
	gpFromHDC     = modGdiplus.NewProc("GdipCreateFromHDC")
	gpDelGraphics = modGdiplus.NewProc("GdipDeleteGraphics")
	gpSmooth      = modGdiplus.NewProc("GdipSetSmoothingMode")
	gpCreatePath  = modGdiplus.NewProc("GdipCreatePath")
	gpDelPath     = modGdiplus.NewProc("GdipDeletePath")
	gpAddArc      = modGdiplus.NewProc("GdipAddPathArc")
	gpCloseFig    = modGdiplus.NewProc("GdipClosePathFigure")
	gpCreateSolid = modGdiplus.NewProc("GdipCreateSolidFill")
	gpDelBrush    = modGdiplus.NewProc("GdipDeleteBrush")
	gpFillPath    = modGdiplus.NewProc("GdipFillPath")
	pGetDC        = modUser32.NewProc("GetDC")
	pReleaseDC    = modUser32.NewProc("ReleaseDC")
	pExtractIconW = modShell32.NewProc("ExtractIconW")
	pDrawTextW    = modUser32.NewProc("DrawTextW")
	pGetTextFaceW = modGdi32.NewProc("GetTextFaceW")
	pFillRect     = modUser32.NewProc("FillRect")
	pCreatePen    = modGdi32.NewProc("CreatePen")
	pSelectObject = modGdi32.NewProc("SelectObject")
	pRoundRect    = modGdi32.NewProc("RoundRect")
)

// 当前进度窗口状态（同一时间只有一个 splash）。窗口整体自绘 + 双缓冲，避免高频刷新闪烁。
var (
	splashHwnd       uintptr
	splashStatusH    int32
	splashBarY       int32
	splashBarW       int32
	// splashProgressBits 当前进度（float64 位模式，原子读写；由 Update 写入、WM_PAINT 读取）。
	splashProgressBits atomic.Uint64
	// 自绘文本状态
	splashTitle     string
	splashStatus    string
	splashTextMu    sync.Mutex
	splashTitleFont uintptr
	splashStatusFont uintptr
	// 双缓冲后缓冲（WM_PAINT 画到内存 DC，再整块 BitBlt 到窗口，彻底消除闪烁）
	splashBackDC  uintptr
	splashBackBmp uintptr
	splashBackW   int32
	splashBackH   int32
	// splashOnClose：用户点窗口关闭按钮时的回调，返回 true 允许关闭并中止，false 保持窗口。
	splashOnClose     func() bool
	splashCloseSilent atomic.Bool // 程序化 Close() 时为 true，跳过 OnClose 确认
)

// SplashState 进度窗口控制器。
type SplashState struct {
	Update func(text string, fraction float64)
	Close  func()
}

// SetOnClose 设置用户关闭窗口时的回调（true=允许关闭并中止流程；false=取消关闭继续运行）。
func (s *SplashState) SetOnClose(fn func() bool) { splashOnClose = fn }

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

func splashWndProc(hwnd, uMsg, wParam, lParam uintptr) uintptr {
	switch uMsg {
	case wmClose:
		if !splashCloseSilent.Load() && splashOnClose != nil {
			if !splashOnClose() {
				return 0 // 用户选择继续，不关闭窗口
			}
		}
		pDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		splashHwnd = 0
		if splashBackDC != 0 {
			if wb, _, _ := pGetStockObject.Call(whiteBrush); wb != 0 {
				pSelectObject.Call(splashBackDC, wb)
			}
			if splashBackBmp != 0 {
				pDeleteObject.Call(splashBackBmp)
			}
			pDeleteDC.Call(splashBackDC)
			splashBackDC = 0
			splashBackBmp = 0
		}
		if splashTitleFont != 0 {
			pDeleteObject.Call(splashTitleFont)
			splashTitleFont = 0
		}
		if splashStatusFont != 0 {
			pDeleteObject.Call(splashStatusFont)
			splashStatusFont = 0
		}
		pPostQuitMessage.Call(0)
		return 0
	case wmPaint:
		var ps paintStruct
		hdc, _, _ := pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		if hdc != 0 && splashBackDC != 0 {
			splashRender(splashBackDC)
			// 整块拷贝到窗口，窗口不直接绘制 → 无闪烁
			pBitBlt.Call(hdc, 0, 0, uintptr(splashBackW), uintptr(splashBackH), splashBackDC, 0, 0, srccopy)
		}
		pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	}
	ret, _, _ := pDefWindowProcW.Call(hwnd, uMsg, wParam, lParam)
	return ret
}

// splashRender 把整个客户区绘制到指定 DC（内存缓冲），供 WM_PAINT 整块 BitBlt。
func splashRender(hdc uintptr) {
	if wb, _, _ := pGetStockObject.Call(whiteBrush); wb != 0 {
		rc := rect{0, 0, splashBackW, splashBackH}
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), wb)
	}
	// 标题（居中）
	if splashTitleFont != 0 {
		pSelectObject.Call(hdc, splashTitleFont)
	}
	pSetTextColor.Call(hdc, colorTitle)
	pSetBkMode.Call(hdc, bkTransparent)
	titlePtr, _ := syscall.UTF16PtrFromString(splashTitle)
	tr := rect{int32(pad), titleY, int32(pad + contentW), titleY + titleH}
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(titlePtr)), ^uintptr(0), uintptr(unsafe.Pointer(&tr)), dtCenter|dtVCenter|dtSingleLine)
	// 状态文本（居中，随阶段变化）
	if splashStatusFont != 0 {
		pSelectObject.Call(hdc, splashStatusFont)
	}
	splashTextMu.Lock()
	status := splashStatus
	splashTextMu.Unlock()
	pSetTextColor.Call(hdc, colorStatus)
	pSetBkMode.Call(hdc, bkOpaque)
	pSetBkColor.Call(hdc, colorWhite)
	statusPtr, _ := syscall.UTF16PtrFromString(status)
	sr := rect{int32(pad), statusY, int32(pad+contentW), statusY + splashStatusH}
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(statusPtr)), ^uintptr(0), uintptr(unsafe.Pointer(&sr)), dtCenter|dtVCenter|dtSingleLine|dtEndEllipsis)
	// 进度条（两端圆角胶囊）
	drawProgressBar(hdc, math.Float64frombits(splashProgressBits.Load()))
}

// drawProgressBar 绘制圆角胶囊进度条（轨道 + 按 frac 0~1 的填充），
// 所有进度窗口共用，保证样式一致。
func drawProgressBar(hdc uintptr, frac float64) {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	radius := int32(barH / 2) // 两端半圆
	// 轨道
	rc := rect{int32(pad), splashBarY, int32(pad + contentW), splashBarY + int32(barH)}
	fillRoundedRectAA(hdc, rc, radius, colorRefToARGB(colorTrack))
	// 填充（最窄保持一个圆点宽度，左端圆角完整）
	if frac > 0 {
		fw := int32(float64(contentW) * frac)
		if fw < int32(barH) {
			fw = int32(barH)
		}
		rc = rect{int32(pad), splashBarY, int32(pad) + fw, splashBarY + int32(barH)}
		fillRoundedRectAA(hdc, rc, radius, colorRefToARGB(colorFill))
	}
}

func moduleHandle() uintptr {
	h, _, _ := pGetModuleHandleW.Call(0)
	return h
}

// setWindowIcon 给窗口标题栏设置应用图标（提取自身 exe 的图标），Windows 支持。
func setWindowIcon(hwnd uintptr) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	exePtr, _ := syscall.UTF16PtrFromString(exe)
	hicon, _, _ := pExtractIconW.Call(0, uintptr(unsafe.Pointer(exePtr)), 0) // nIconIndex=0
	if hicon != 0 {
		pSendMessageW.Call(hwnd, wmSetIcon, iconBig, hicon)
		pSendMessageW.Call(hwnd, wmSetIcon, iconSmall, hicon)
	}
}

// makeFont 创建字体（height 像素、weight 400/600）。
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

// makeFontQuality 带指定抗锯齿质量的字体（quality：cleartypeQual 亚像素 / antialiasQual 灰度）。
// 按钮文字建议用灰度抗锯齿，避免在品牌蓝底上出现 ClearType 彩色边缘。
func makeFontQuality(height, weight int32, quality uintptr) uintptr {
	face, _ := syscall.UTF16PtrFromString(selectedFace())
	h, _, _ := pCreateFontW.Call(uintptr(height), 0, 0, 0, uintptr(weight), 0, 0, 0, defaultCharset, 0, 0, quality, 0, uintptr(unsafe.Pointer(face)))
	return h
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
	splashTitleFont = makeFont(22, 600)
	splashStatusFont = makeFont(16, 400)
	splashTitle = "DeepSeek Harness"
	splashTextMu.Lock()
	splashStatus = statusText
	splashTextMu.Unlock()

	// 状态文本换行高度自适应，进度条随之下移
	msgH := measureMultilineHeight(statusText, contentW, splashStatusFont)
	if msgH < 22 {
		msgH = 22
	}
	if msgH > 64 {
		msgH = 64
	}
	msgH += 2
	splashStatusH = int32(msgH)
	splashBarY = int32(statusY) + splashStatusH + 10
	splashProgressBits.Store(0)

	clientW := int32(contentW + pad*2)
	clientH := splashBarY + int32(barH) + 16
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
		wsCaption|wsSysMenu|wsClipChildren,
		uintptr(x), uintptr(y), uintptr(winW), uintptr(winH),
		0, 0, moduleHandle(), 0,
	)
	if hwnd == 0 {
		return 0
	}
	splashHwnd = hwnd

	// Windows 11 圆角窗口（旧系统忽略失败）
	corner := uintptr(dwmcRound)
	pDwmSetWindowAttribute.Call(hwnd, dwmcWindowCornerPreference, uintptr(unsafe.Pointer(&corner)), unsafe.Sizeof(corner))

	// 双缓冲后缓冲：WM_PAINT 整体画到内存 DC 再 BitBlt，彻底消除高频刷新闪烁
	var cr rect
	pGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&cr)))
	splashBackW = cr.right
	splashBackH = cr.bottom
	screenDC, _, _ := pGetDC.Call(0)
	splashBackDC, _, _ = pCreateCompatibleDC.Call(screenDC)
	splashBackBmp, _, _ = pCreateCompatibleBmp.Call(screenDC, uintptr(splashBackW), uintptr(splashBackH))
	if splashBackBmp != 0 {
		pSelectObject.Call(splashBackDC, splashBackBmp)
	}
	pReleaseDC.Call(0, screenDC)

	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	// 标题栏显示应用图标；确保窗口自动提到前台
	setWindowIcon(hwnd)
	pSetForegroundWindow.Call(hwnd)
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
	splashCloseSilent.Store(false)
	splashOnClose = nil

	st := &SplashState{}
	st.Update = func(t string, f float64) {
		if t != "" {
			splashTextMu.Lock()
			splashStatus = t
			splashTextMu.Unlock()
		}
		if splashHwnd != 0 {
			if f < 0 {
				f = 0
			}
			if f > 1 {
				f = 1
			}
			splashProgressBits.Store(math.Float64bits(f))
			// 双缓冲整窗重绘：高频更新（下载进度）由 WM_PAINT 画到后缓冲再整块拷贝，无闪烁。
			pInvalidateRect.Call(splashHwnd, 0, 0)
		}
	}
	st.Close = func() {
		splashCloseSilent.Store(true)
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
	dtSingleLine = 0x0020
	dtEndEllipsis = 0x8000
	dtWordBreak = 0x0010
	dtCalcRect  = 0x0400

	srccopy = 0x00CC0020

	dlgPad    = 20
	dlgW      = 380
	dlgBtnH   = 36
	dlgBtnW   = 96
	dlgBtnGap = 16

	dialogColorMsg   = 0x00857066 // #667085
	dialogColorTxt   = 0x00281810 // #101828
	dialogColorWhite = 0x00FFFFFF
	// 主/次按钮填充（COLORREF=0xBBGGRR）
	dialogColorPrim    = 0x00D84E1D // #1D4ED8 主按钮品牌蓝
	dialogColorPrimSel = 0x00AF401E // #1E40AF 按压加深
	dialogColorGray    = 0x00F7F3F1 // #F1F3F7
	dialogColorGraySel = 0x00ECE6E2 // #E2E6EC
	dialogColorBorder  = 0x00E8E0DC // #DCE0E8
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
	dialogBtnFont       uintptr
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
		pSetBkMode.Call(wParam, bkOpaque) // ClearType 子像素渲染（网页级）
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
		// 蓝色填充胶囊按钮（GDI+ 抗锯齿）+ 文字背景色 = 按钮填充色（无任何色差）
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
		// 铺上与窗口背景一致的白色底，覆盖按钮控件自带的默认底色（消除圆角外色差）
		if wb, _, _ := pGetStockObject.Call(whiteBrush); wb != 0 {
			pFillRect.Call(hdc, uintptr(unsafe.Pointer(&dis.rcItem)), wb)
		}
		// 抗锯齿胶囊填充（圆角 20，上限高度一半）
		fillRoundedRectAA(hdc, dis.rcItem, 20, colorRefToARGB(fillColor))
		// 文字：透明背景绘制——文字背后直接就是按钮填充色，零色差且不会产生方形角块
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
	dialogMsgFont = makeFont(16, 400)
	dialogBtnFont = makeFont(18, 400)

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

	// 消息文本（应用与网站一致的字体）
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
	// 标题栏图标 + 确保弹窗自动到前台
	setWindowIcon(hwnd)
	pSetForegroundWindow.Call(hwnd)
	return hwnd
}

// buttonsW 兼容旧调用：未初始化时返回固定 1。
func buttonsW() []string {
	if len(dialogLabels) == 0 {
		return []string{"确定"}
	}
	return dialogLabels
}
