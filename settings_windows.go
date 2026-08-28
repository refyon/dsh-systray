//go:build windows

package main

import (
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// ==================== 设置窗口（左侧分类栏 + 右侧内容面板） ====================
const (
	settingsCls = "DSH_Systray_Settings"

	swHide = 0
	dtLeft = 0

	// 布局（客户区坐标）
	stSidebarW = 172
	stWinW     = 600
	stWinH     = 420
	stCatX     = 8
	stCatW     = stSidebarW - 2*stCatX
	stCatH     = 36
	stCatY0    = 36
	stCatGap   = 6
	stContentX = 200

	// 控件 ID
	stIdSidebarBg  = 2900
	stIdCatGeneral = 3000
	stIdCatAbout   = 3001
	stIdPaneTitle  = 3005
	stIdAutoTitle  = 3101
	stIdAutoSub    = 3102
	stIdAutoToggle = 3103
	stIdVerTitle   = 3201
	stIdVerValue   = 3202
	stIdCheckBtn   = 3203

	// 颜色（COLORREF = 0xBBGGRR）
	stColorSidebarBg = 0x00FAF8F7 // #F7F8FA 侧栏浅灰底
	stColorItemSel   = 0x00F0EDEC // #ECEDF0 选中项浅灰
	stColorBlue      = 0x00FE6B4D // #4D6BFE 品牌蓝
	stColorGray      = 0x00EDEAE8 // #E8EAED 开关轨道灰
	stColorText      = 0x00242120 // #202124
	stColorSub       = 0x0068635F // #5F6368
)

var (
	pInvalidateRect      = modUser32.NewProc("InvalidateRect")
	pSetForegroundWindow = modUser32.NewProc("SetForegroundWindow")

	settingsOpenFlag  atomic.Bool
	settingsHwnd      uintptr
	settingsCat       int
	settingsAutoOn    bool
	settingsClassReg  bool
	settingsWidgets   = map[uintptr]uintptr{} // hwnd → ctlID
	settingsCatBtns   [2]uintptr              // 3000/3001 的分类按钮句柄
	settingsPaneGen   []uintptr               // 常规面板控件
	settingsPaneAbout []uintptr               // 关于面板控件
	settingsFontTitle uintptr
	settingsFontBody  uintptr
	settingsFontBold  uintptr
	settingsFontSmall uintptr
	settingsSideBrush uintptr
	settingsTitleHwnd uintptr
)

// openSettingsWindow 打开设置窗口（已在打开时前置显示）。
func openSettingsWindow() {
	if settingsOpenFlag.Load() {
		if settingsHwnd != 0 {
			pShowWindow.Call(settingsHwnd, swShow)
			pSetForegroundWindow.Call(settingsHwnd)
		}
		return
	}
	if !settingsOpenFlag.CompareAndSwap(false, true) {
		return
	}
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		settingsAutoOn = isAutostartEnabled()
		hwnd := createSettingsWindow()
		if hwnd == 0 {
			settingsOpenFlag.Store(false)
			return
		}
		settingsHwnd = hwnd
		for {
			var m msg
			ret, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(ret) <= 0 {
				break
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
		settingsHwnd = 0
		settingsOpenFlag.Store(false)
	}()
}

func settingsWndProc(hwnd, uMsg, wParam, lParam uintptr) uintptr {
	switch uMsg {
	case wmCommand:
		id := int(wParam & 0xFFFF)
		switch {
		case id == stIdCatGeneral || id == stIdCatAbout:
			settingsCat = id - stIdCatGeneral
			settingsShowPane(hwnd)
			settingsRedrawCats()
		case id == stIdAutoToggle:
			settingsAutoOn = !settingsAutoOn
			setAutostartOn(settingsAutoOn)
			settingsRedrawWidget(stIdAutoToggle)
		case id == stIdCheckBtn:
			checkForUpdatesManual()
		}
		return 0
	case wmClose:
		pDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		if settingsFontTitle != 0 {
			pDeleteObject.Call(settingsFontTitle)
		}
		if settingsFontBody != 0 {
			pDeleteObject.Call(settingsFontBody)
		}
		if settingsFontBold != 0 {
			pDeleteObject.Call(settingsFontBold)
		}
		if settingsFontSmall != 0 {
			pDeleteObject.Call(settingsFontSmall)
		}
		if settingsSideBrush != 0 {
			pDeleteObject.Call(settingsSideBrush)
		}
		pPostQuitMessage.Call(0)
		return 0
	case wmCtlColorStatic:
		h := settingsWidgets[lParam]
		switch h {
		case stIdPaneTitle, stIdAutoTitle, stIdVerTitle:
			pSetTextColor.Call(wParam, stColorText)
		case stIdVerValue:
			pSetTextColor.Call(wParam, stColorBlue)
		case stIdAutoSub:
			pSetTextColor.Call(wParam, stColorSub)
		case stIdSidebarBg:
			if settingsSideBrush != 0 {
				return settingsSideBrush
			}
		default:
			pSetTextColor.Call(wParam, stColorText)
		}
		pSetBkColor.Call(wParam, colorWhite)
		pSetBkMode.Call(wParam, bkOpaque)
		if settingsSideBrush != 0 && h == stIdSidebarBg {
			return settingsSideBrush
		}
		white, _, _ := pGetStockObject.Call(whiteBrush)
		return white
	case wmDrawItem:
		if lParam == 0 {
			break
		}
		dis := *(*drawItemStruct)(unsafe.Add(unsafe.Pointer(nil), lParam))
		switch settingsWidgets[dis.hwndItem] {
		case stIdCatGeneral, stIdCatAbout:
			settingsDrawCat(dis)
		case stIdAutoToggle:
			settingsDrawToggle(dis)
		case stIdCheckBtn:
			settingsDrawCapsule(dis, "检查更新")
		}
		return 1
	}
	ret, _, _ := pDefWindowProcW.Call(hwnd, uMsg, wParam, lParam)
	return ret
}

// settingsShowPane 切换右侧内容面板（按当前分类显示/隐藏控件）。
func settingsShowPane(hwnd uintptr) {
	for _, id := range settingsPaneGen {
		if w := settingsWidgetKey(id); w != 0 {
			pShowWindow.Call(w, swHide)
		}
	}
	for _, id := range settingsPaneAbout {
		if w := settingsWidgetKey(id); w != 0 {
			pShowWindow.Call(w, swHide)
		}
	}
	var show []uintptr
	if settingsCat == 0 {
		show = settingsPaneGen
	} else {
		show = settingsPaneAbout
	}
	for _, id := range show {
		if w := settingsWidgetKey(id); w != 0 {
			pShowWindow.Call(w, swShow)
		}
	}
	if settingsTitleHwnd != 0 {
		title := "常规"
		if settingsCat == 1 {
			title = "关于"
		}
		t, _ := syscall.UTF16PtrFromString(title)
		pSendMessageW.Call(settingsTitleHwnd, wmSetText, 0, uintptr(unsafe.Pointer(t)))
	}
}

// settingsRedrawCats 重绘两个分类按钮（选中态变化）。
func settingsRedrawCats() {
	for _, w := range settingsCatBtns {
		if w != 0 {
			pInvalidateRect.Call(w, 0, 1)
		}
	}
}

func settingsRedrawWidget(id int) {
	if w := settingsWidgetKey(uintptr(id)); w != 0 {
		pInvalidateRect.Call(w, 0, 1)
	}
}

// settingsWidgetKey 按 ctlID 反查控件句柄。
func settingsWidgetKey(id uintptr) uintptr {
	for hwnd, ctlID := range settingsWidgets {
		if ctlID == id {
			return hwnd
		}
	}
	return 0
}

// settingsDrawCat 绘制侧栏分类按钮（选中项浅灰胶囊 + 蓝色文字）。
func settingsDrawCat(dis drawItemStruct) {
	id := int(settingsWidgets[dis.hwndItem])
	selected := (id == stIdCatGeneral && settingsCat == 0) || (id == stIdCatAbout && settingsCat == 1)
	hdc := dis.hDC
	// 先铺侧栏底色，避免圆角外出现白块
	if settingsSideBrush != 0 {
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&dis.rcItem)), settingsSideBrush)
	} else if wb, _, _ := pGetStockObject.Call(whiteBrush); wb != 0 {
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&dis.rcItem)), wb)
	}
	if selected {
		fillRoundedRectAA(hdc, dis.rcItem, 8, colorRefToARGB(stColorItemSel))
		pSetTextColor.Call(hdc, stColorBlue)
	} else {
		pSetTextColor.Call(hdc, stColorText)
	}
	pSetBkMode.Call(hdc, bkTransparent)
	label := "常规"
	if id == stIdCatAbout {
		label = "关于"
	}
	font := settingsFontBody
	if selected {
		font = settingsFontBold
	}
	if font != 0 {
		pSelectObject.Call(hdc, font)
	}
	t, _ := syscall.UTF16PtrFromString(label)
	rc := dis.rcItem
	rc.left += 14
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(t)), ^uintptr(0), uintptr(unsafe.Pointer(&rc)), dtLeft|dtVCenter|dtSingle)
}

// settingsDrawToggle 绘制开关（圆形轨道 + 滑动圆钮）。
func settingsDrawToggle(dis drawItemStruct) {
	hdc := dis.hDC
	if wb, _, _ := pGetStockObject.Call(whiteBrush); wb != 0 {
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&dis.rcItem)), wb)
	}
	// 轨道 46x26 居中
	tw, th := int32(46), int32(26)
	tx := dis.rcItem.left + (dis.rcItem.right-dis.rcItem.left-tw)/2
	ty := dis.rcItem.top + (dis.rcItem.bottom-dis.rcItem.top-th)/2
	track := rect{tx, ty, tx + tw, ty + th}
	trackColor := uint32(colorRefToARGB(stColorGray))
	if settingsAutoOn {
		trackColor = colorRefToARGB(stColorBlue)
	}
	fillRoundedRectAA(hdc, track, th/2, trackColor)
	// 圆钮 20x20（半径=高度一半即正圆）
	knobD := int32(20)
	kx := tx + 4
	if settingsAutoOn {
		kx = tx + tw - knobD - 4
	}
	ky := ty + (th-knobD)/2
	knob := rect{kx, ky, kx + knobD, ky + knobD}
	fillRoundedRectAA(hdc, knob, knobD/2, 0xFFFFFFFF)
}

// settingsDrawCapsule 绘制品牌蓝胶囊按钮（同弹窗主按钮风格）。
func settingsDrawCapsule(dis drawItemStruct, label string) {
	hdc := dis.hDC
	pressed := dis.itemState&odsSelected != 0
	fill := uintptr(stColorBlue)
	if pressed {
		fill = dialogColorPrimSel
	}
	if wb, _, _ := pGetStockObject.Call(whiteBrush); wb != 0 {
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&dis.rcItem)), wb)
	}
	fillRoundedRectAA(hdc, dis.rcItem, 16, colorRefToARGB(fill))
	pSetTextColor.Call(hdc, dialogColorWhite)
	pSetBkMode.Call(hdc, bkTransparent)
	if settingsFontBold != 0 {
		pSelectObject.Call(hdc, settingsFontBold)
	}
	t, _ := syscall.UTF16PtrFromString(label)
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(t)), ^uintptr(0), uintptr(unsafe.Pointer(&dis.rcItem)), dtCenter|dtVCenter|dtSingle)
}

func createSettingsWindow() uintptr {
	cls, _ := syscall.UTF16PtrFromString(settingsCls)
	cb := syscall.NewCallback(settingsWndProc)
	cur, _, _ := pLoadCursorW.Call(0, idcArrow)

	if !settingsClassReg {
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
		settingsClassReg = true
	}

	settingsFontTitle = makeFont(17, 600)
	settingsFontBody = makeFont(14, 400)
	settingsFontBold = makeFont(14, 600)
	settingsFontSmall = makeFont(12, 400)
	settingsSideBrush, _, _ = pCreateSolidBrush.Call(stColorSidebarBg)

	titleText, _ := syscall.UTF16PtrFromString("设置")
	r := rect{0, 0, stWinW, stWinH}
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
	corner := uintptr(dwmcRound)
	pDwmSetWindowAttribute.Call(hwnd, dwmcWindowCornerPreference, uintptr(unsafe.Pointer(&corner)), unsafe.Sizeof(corner))

	staticCls, _ := syscall.UTF16PtrFromString("STATIC")
	btnCls, _ := syscall.UTF16PtrFromString("BUTTON")

	// 侧栏背景（浅灰，用画刷填充）
	sb, _ := syscall.UTF16PtrFromString("")
	sideBg, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(sb)),
		wsChild|wsVisible,
		0, 0, stSidebarW, stWinH,
		hwnd, stIdSidebarBg, moduleHandle(), 0,
	)
	if sideBg != 0 {
		settingsWidgets[sideBg] = stIdSidebarBg
	}

	// 分类按钮（自绘）
	catLabels := []string{"常规", "关于"}
	for i, label := range catLabels {
		bt, _ := syscall.UTF16PtrFromString(label)
		cy := int32(stCatY0 + i*(stCatH+stCatGap))
		hb, _, _ := pCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(btnCls)),
			uintptr(unsafe.Pointer(bt)),
			wsChild|wsVisible|wsTabStop|bsOwnDraw,
			uintptr(stCatX), uintptr(cy), uintptr(stCatW), uintptr(stCatH),
			hwnd, uintptr(stIdCatGeneral+i), moduleHandle(), 0,
		)
		if hb != 0 {
			settingsWidgets[hb] = uintptr(stIdCatGeneral + i)
			settingsCatBtns[i] = hb
			pSendMessageW.Call(hb, wmSetFont, settingsFontBody, 1)
		}
	}

	// 面板标题（随分类切换改文字）
	pt, _ := syscall.UTF16PtrFromString("常规")
	settingsTitleHwnd, _, _ = pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(pt)),
		wsChild|wsVisible,
		uintptr(stContentX), 24, 300, 28,
		hwnd, stIdPaneTitle, moduleHandle(), 0,
	)
	if settingsTitleHwnd != 0 {
		settingsWidgets[settingsTitleHwnd] = stIdPaneTitle
		pSendMessageW.Call(settingsTitleHwnd, wmSetFont, settingsFontTitle, 1)
	}

	// ---- 常规面板 ----
	at, _ := syscall.UTF16PtrFromString("开机自启动")
	autoTitle, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(at)),
		wsChild|wsVisible,
		uintptr(stContentX), 78, 220, 24,
		hwnd, stIdAutoTitle, moduleHandle(), 0,
	)
	if autoTitle != 0 {
		settingsWidgets[autoTitle] = stIdAutoTitle
		pSendMessageW.Call(autoTitle, wmSetFont, settingsFontBody, 1)
		settingsPaneGen = append(settingsPaneGen, stIdAutoTitle)
	}
	asub, _ := syscall.UTF16PtrFromString("登录系统时自动启动托盘程序")
	autoSub, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(asub)),
		wsChild|wsVisible,
		uintptr(stContentX), 104, 340, 18,
		hwnd, stIdAutoSub, moduleHandle(), 0,
	)
	if autoSub != 0 {
		settingsWidgets[autoSub] = stIdAutoSub
		pSendMessageW.Call(autoSub, wmSetFont, settingsFontSmall, 1)
		settingsPaneGen = append(settingsPaneGen, stIdAutoSub)
	}
	// 开关（自绘胶囊）
	tb, _ := syscall.UTF16PtrFromString("")
	autoToggle, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(btnCls)),
		uintptr(unsafe.Pointer(tb)),
		wsChild|wsVisible|wsTabStop|bsOwnDraw,
		uintptr(stContentX), 136, 56, 32,
		hwnd, stIdAutoToggle, moduleHandle(), 0,
	)
	if autoToggle != 0 {
		settingsWidgets[autoToggle] = stIdAutoToggle
		settingsPaneGen = append(settingsPaneGen, stIdAutoToggle)
	}

	// ---- 关于面板 ----
	vt, _ := syscall.UTF16PtrFromString("当前版本号")
	verTitle, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(vt)),
		wsChild|wsVisible,
		uintptr(stContentX), 78, 180, 24,
		hwnd, stIdVerTitle, moduleHandle(), 0,
	)
	if verTitle != 0 {
		settingsWidgets[verTitle] = stIdVerTitle
		pSendMessageW.Call(verTitle, wmSetFont, settingsFontBody, 1)
		settingsPaneAbout = append(settingsPaneAbout, stIdVerTitle)
	}
	verText := appVersion
	vv, _ := syscall.UTF16PtrFromString(verText)
	verValue, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(vv)),
		wsChild|wsVisible,
		uintptr(stContentX), 104, 220, 26,
		hwnd, stIdVerValue, moduleHandle(), 0,
	)
	if verValue != 0 {
		settingsWidgets[verValue] = stIdVerValue
		pSendMessageW.Call(verValue, wmSetFont, settingsFontTitle, 1)
		settingsPaneAbout = append(settingsPaneAbout, stIdVerValue)
	}
	// 检查更新（自绘胶囊）
	cb2, _ := syscall.UTF16PtrFromString("检查更新")
	checkBtn, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(btnCls)),
		uintptr(unsafe.Pointer(cb2)),
		wsChild|wsVisible|wsTabStop|bsOwnDraw,
		uintptr(stContentX), 148, 130, 34,
		hwnd, stIdCheckBtn, moduleHandle(), 0,
	)
	if checkBtn != 0 {
		settingsWidgets[checkBtn] = stIdCheckBtn
		settingsPaneAbout = append(settingsPaneAbout, stIdCheckBtn)
	}

	// 初始显示「常规」面板
	settingsCat = 0
	settingsShowPane(hwnd)
	settingsRedrawCats()

	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	return hwnd
}
