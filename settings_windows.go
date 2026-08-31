//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// ==================== 设置窗口（左侧分类栏 + 右侧内容面板） ====================
const (
	settingsCls = "DSH_Systray_Settings"

	swHide = 0
	dtLeft = 0

	// 布局（客户区坐标）
	stSidebarW = 172
	stWinW     = 640
	stWinH     = 460
	stCatX     = 8
	stCatW     = stSidebarW - 2*stCatX
	stCatH     = 36
	stCatY0    = 36
	stCatGap   = 6
	stContentX = 200

	// 控件 ID
	stIdSidebarBg   = 2900
	stIdCatGeneral  = 3000
	stIdCatAbout    = 3001
	stIdCatLog      = 3002
	stIdCatExport   = 3003
	stIdCatImport   = 3004
	stIdPaneTitle   = 3005
	stIdAutoTitle   = 3101
	stIdAutoSub     = 3102
	stIdAutoToggle  = 3103
	stIdRestartInfo = 3104
	stIdRestartBtn  = 3105
	stIdVerTitle    = 3201
	stIdVerValue    = 3202
	stIdCheckBtn    = 3203
	stIdHarTitle    = 3204
	stIdHarValue    = 3205
	stIdLogInfo     = 3300
	stIdLogCombo    = 3301
	stIdLogRefresh  = 3302
	stIdLogEdit     = 3303
	// 导出页
	stIdExpSessions = 3400
	stIdExpSessLbl  = 3401
	stIdExpSessSub  = 3402
	stIdExpPlugins  = 3410
	stIdExpPlugLbl  = 3411
	stIdExpPlugSub  = 3412
	stIdExpFiles    = 3420
	stIdExpFilesLbl = 3421
	stIdExpFilesSub = 3422
	stIdExpAddDir   = 3430
	stIdExpDirs     = 3431
	stIdExpGo       = 3440
	stIdExpStatus   = 3441
	stIdExpPlugHelp = 3413
	// 导入页
	stIdImpAdd      = 3500
	stIdImpPath     = 3501
	stIdImpStatus   = 3502
	stIdImpSessRow  = 3510
	stIdImpSessBtn  = 3511
	stIdImpPlugRow  = 3520
	stIdImpPlugBtn  = 3521
	stIdImpFilesRow = 3530
	stIdImpFilesBtn = 3531
	// 常规页区块卡片 + 日志页卡片（日志卡片改由父窗口 WM_PAINT 绘制，避免盖住编辑框）

	// EDIT / COMBOBOX / STATIC 样式
	esMultiline           = 0x0004
	esAutoVScroll         = 0x0040
	esReadOnly            = 0x0800
	wsVScroll             = 0x00200000
	wsBorder              = 0x00800000
	cbsDropList           = 0x0003
	cbsHasStrings         = 0x0200
	cbnSelChange          = 1
	wmCtlColorEdit        = 0x0134
	wmCtlColorListBox     = 0x0137
	cbAddString           = 0x0143
	cbGetCurSel           = 0x0147
	cbSetCurSel           = 0x014E
	wmTimer               = 0x0113
	ssEtchedHorz          = 0x0010
	ssEndEllipsis         = 0x4000
	emSetSel              = 0x00B1
	emLineScroll          = 0x00B6
	emScrollCaret         = 0x00B7
	emGetLineCount        = 0x00BA
	emGetFirstVisibleLine = 0x00CE
	emExLimitText         = 0x0435
	wmVScroll             = 0x0115
	sbTop                 = 6
	sbBottom              = 7
	sbThumbPos            = 3
	settingsLogTimer      = 1
	wsClipSiblings        = 0x04000000
	// 现代下拉列表（日志文件选择弹层）
	dropCls      = "DSH_Systray_LogDropdown"
	dropPadX     = 8
	dropPadY     = 6
	dropItemH    = 32
	wsPopup      = 0x80000000
	wmActivate   = 0x0006
	waInactive   = 0
	wmMouseMove  = 0x0200
	wmMouseLeave = 0x02A3
	wmLButtonUp  = 0x0202
	wmKeyDown    = 0x0100
	wmKillFocus  = 0x0008
	tmeLeave     = 0x2
	vkUp         = 0x26
	vkDown       = 0x28
	vkReturn     = 0x0D
	vkEscape     = 0x1B
	// RedrawWindow 标志：完整失效 + 擦除背景 + 立即重绘 + 子控件
	rdwInvalidate  = 0x0001
	rdwErase       = 0x0004
	rdwAllChildren = 0x0080
	rdwUpdateNow   = 0x0100
	// 原生工具提示（泡泡样式）
	ttsAlwaysTip       = 0x01
	ttsNoprefix        = 0x02
	ttsBalloon         = 0x40
	ttfSubclass        = 0x0010
	ttmAddToolW        = 0x0432 // WM_USER + 50
	ttmSetMaxTipWidthW = 0x0418 // WM_USER + 24
	vkPageUp           = 0x21
	vkPageDown         = 0x22
	vkHome             = 0x24
	vkEnd              = 0x23

	// 颜色（COLORREF = 0xBBGGRR）
	stColorSidebarBg = 0x00FAF7F5 // #F5F7FA 侧栏浅灰底
	stColorItemSel   = 0x00F3EEEB // #EBEEF3 选中项浅灰
	stColorBlue      = 0x00D84E1D // #1D4ED8 品牌蓝
	stColorGray      = 0x00ECE7E4 // #E4E7EC 开关轨道灰
	stColorText      = 0x00281810 // #101828
	stColorSub       = 0x00857066 // #667085
)

var (
	pInvalidateRect      = modUser32.NewProc("InvalidateRect")
	pSetForegroundWindow = modUser32.NewProc("SetForegroundWindow")
	pSetTimer            = modUser32.NewProc("SetTimer")
	pKillTimer           = modUser32.NewProc("KillTimer")
	pLoadLibraryW        = modKernel32.NewProc("LoadLibraryW")
	pGetWindowRect       = modUser32.NewProc("GetWindowRect")
	pSetFocus            = modUser32.NewProc("SetFocus")
	pGetParent           = modUser32.NewProc("GetParent")
	pTrackMouseEvent     = modUser32.NewProc("TrackMouseEvent")
	pRedrawWindow        = modUser32.NewProc("RedrawWindow")

	settingsOpenFlag        atomic.Bool
	settingsHwnd            uintptr
	settingsCat             int
	settingsAutoOn          bool
	settingsClassReg        bool
	settingsWidgets         = map[uintptr]uintptr{} // hwnd → ctlID
	settingsCatBtns         [5]uintptr              // 分类按钮句柄（常规/关于/日志/导出/导入）
	settingsPaneGen         []uintptr               // 常规面板控件
	settingsPaneAbout       []uintptr               // 关于面板控件
	settingsPaneLog         []uintptr               // 日志面板控件
	settingsPaneExp         []uintptr               // 导出面板控件
	settingsPaneImp         []uintptr               // 导入面板控件
	settingsLogEdit         uintptr                 // 日志 readonly EDIT（RICHEDIT50W）
	settingsRestorePending  bool                    // 恢复过会话/插件：关闭设置窗口时提示重启
	settingsLogComboSel     int                     // 日志文件选择 0=app.log / 1=server.log
	settingsComboOpen       bool                    // 日志下拉列表是否展开
	settingsDropHwnd        uintptr                 // 日志下拉列表窗口
	settingsDropHover       int                     // 下拉列表悬停项
	settingsDropClsReg      bool                    // 下拉列表窗口类是否已注册
	settingsDropClosedAt    time.Time               // 最近一次因失活自动收起的时间（防抖）
	settingsTipHwnd         uintptr                 // 原生泡泡提示窗口
	settingsPlugHelpTip     *uint16                 // 插件选项说明文案（保持指针存活）
	settingsFontTitle       uintptr
	settingsFontBody        uintptr
	settingsFontBold        uintptr
	settingsFontSmall       uintptr
	settingsFontMono        uintptr // 日志等宽字体 Consolas（14px）
	settingsFontBtn         uintptr // 胶囊按钮字体（略大于正文）
	settingsSideBrush       uintptr
	settingsTitleHwnd       uintptr
	settingsRestartInfoHwnd uintptr
	settingsSvcState        atomic.Int32 // 后台服务状态：0=已停止 1=启动中 2=运行中

	// 导出/导入页状态
	settingsExpSessions bool
	settingsExpPlugins  bool
	settingsExpFiles    bool
	settingsExpDirs     []string // 要打包的目录列表
	settingsImpPath     string   // 已选导入压缩包路径
	settingsImpItems    []importItem
	settingsExpBusy     atomic.Bool
	settingsImpBusy     atomic.Bool
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
		case id >= stIdCatGeneral && id <= stIdCatImport:
			settingsCat = id - stIdCatGeneral
			settingsShowPane(hwnd)
			settingsRedrawCats()
			if settingsCat == 2 {
				settingsLogReload(true) // 打开/切换到日志页：滚动到底部一次
			}
		case id == stIdAutoToggle:
			settingsAutoOn = !settingsAutoOn
			setAutostartOn(settingsAutoOn)
			settingsRedrawWidget(stIdAutoToggle)
		case id == stIdExpSessions:
			settingsExpSessions = !settingsExpSessions
			settingsRedrawWidget(stIdExpSessions)
		case id == stIdExpPlugins:
			settingsExpPlugins = !settingsExpPlugins
			settingsRedrawWidget(stIdExpPlugins)
		case id == stIdExpFiles:
			settingsExpFiles = !settingsExpFiles
			settingsRedrawWidget(stIdExpFiles)
		case id == stIdExpAddDir:
			if dir := pickHarnessDir("选择要打包的目录", ""); dir != "" {
				settingsExpDirs = append(settingsExpDirs, dir)
				settingsExpDirsUpdate()
			}
		case id == stIdExpGo:
			settingsExportRun()
		case id == stIdImpAdd:
			if p := pickOpenFile("选择 dsh-systray 导出压缩包", "dsh-systray 导出包 (*.zip)\x00*.zip\x00ZIP 压缩包 (*.zip)\x00*.zip\x00所有文件 (*.*)\x00*.*\x00\x00"); p != "" {
				settingsImportLoad(p)
			}
		case id == stIdImpSessBtn:
			settingsRestoreRun("sessions", "")
		case id == stIdImpPlugBtn:
			settingsRestoreRun("plugins", "")
		case id == stIdImpFilesBtn:
			if dir := pickHarnessDir("选择解压位置", ""); dir != "" {
				settingsRestoreRun("files", dir)
			}
		case id == stIdCheckBtn:
			// 异步检查，避免 GitHub 超时阻塞设置窗口 UI 线程（结果弹窗独立线程显示）
			go checkForUpdatesManual()
		case id == stIdRestartBtn:
			// 先确认再重启；过程中实时刷新服务状态（停止后标红“已停止”，拉起时标黄“启动中”，就绪后标绿“运行中”）
			if !askRestartService() {
				break
			}
			go func() {
				restartBackgroundService(func(stage string) {
					switch stage {
					case "stopping", "stopped", "error":
						settingsSetServiceState(serviceStateStopped)
					case "starting":
						settingsSetServiceState(serviceStateStarting)
					case "running":
						settingsSetServiceState(serviceStateRunning)
					}
				})
				// 结束（含失败）以实际探测为准
				settingsSetServiceState(settingsCurrentServiceState())
			}()
		case id == stIdLogCombo:
			settingsToggleDrop() // 展开/收起日志文件下拉列表
		case id == stIdLogRefresh:
			settingsLogClear()
		}
		return 0
	case wmTimer:
		if int(wParam) == settingsLogTimer && settingsCat == 2 {
			settingsLogReload(false) // 定时跟随：仅新写入且贴底时滚动
		}
		return 0
	case wmPaint:
		// 日志卡片由父窗口绘制：切入/切出都重绘卡片区域；切出时同步清除，避免残影出现在其它页
		ret, _, _ := pDefWindowProcW.Call(hwnd, uMsg, wParam, lParam)
		return ret
	case wmClose:
		// 恢复过会话/插件：提示重启服务生效（复用常规页「重启后台服务」处理流程）
		if settingsRestorePending {
			settingsRestorePending = false
			r := runModernDialog(appName, "恢复数据需要重启服务生效，是否重启后台服务？", []string{"重启", "稍后"}, 0)
			if r == 0 {
				go func() {
					restartBackgroundService(func(stage string) {
						switch stage {
						case "stopping", "stopped", "error":
							settingsSetServiceState(serviceStateStopped)
						case "starting":
							settingsSetServiceState(serviceStateStarting)
						case "running":
							settingsSetServiceState(serviceStateRunning)
						}
					})
					// 结束（含失败）以实际探测为准
					settingsSetServiceState(settingsCurrentServiceState())
				}()
			}
		}
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
		if settingsFontMono != 0 {
			pDeleteObject.Call(settingsFontMono)
		}
		if settingsFontBtn != 0 {
			pDeleteObject.Call(settingsFontBtn)
		}
		if settingsSideBrush != 0 {
			pDeleteObject.Call(settingsSideBrush)
		}
		if settingsTipHwnd != 0 {
			pDestroyWindow.Call(settingsTipHwnd)
			settingsTipHwnd = 0
		}
		settingsCloseDrop(false)
		pKillTimer.Call(hwnd, settingsLogTimer)
		pPostQuitMessage.Call(0)
		return 0
	case wmCtlColorStatic:
		h := settingsWidgets[lParam]
		switch h {
		case stIdPaneTitle, stIdAutoTitle, stIdVerTitle, stIdHarTitle:
			pSetTextColor.Call(wParam, stColorText)
		case stIdVerValue, stIdHarValue:
			pSetTextColor.Call(wParam, stColorBlue)
		case stIdAutoSub, stIdLogInfo, stIdRestartInfo, stIdExpSessSub, stIdExpPlugSub, stIdExpFilesSub, stIdExpStatus, stIdImpPath, stIdImpStatus:
			pSetTextColor.Call(wParam, stColorSub)
		case stIdExpSessLbl, stIdExpPlugLbl, stIdExpFilesLbl, stIdImpSessRow, stIdImpPlugRow, stIdImpFilesRow:
			pSetTextColor.Call(wParam, stColorText)
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
	case wmCtlColorEdit:
		pSetTextColor.Call(wParam, stColorText)
		pSetBkColor.Call(wParam, colorWhite)
		white, _, _ := pGetStockObject.Call(whiteBrush)
		return white
	case wmCtlColorListBox:
		pSetTextColor.Call(wParam, stColorText)
		pSetBkColor.Call(wParam, colorWhite)
		white, _, _ := pGetStockObject.Call(whiteBrush)
		return white
	case wmDrawItem:
		if lParam == 0 {
			break
		}
		dis := *(*drawItemStruct)(unsafe.Add(unsafe.Pointer(nil), lParam))
		switch settingsWidgets[dis.hwndItem] {
		case stIdCatGeneral, stIdCatAbout, stIdCatLog, stIdCatExport, stIdCatImport:
			settingsDrawCat(dis)
		case stIdLogCombo:
			settingsDrawCombo(dis)
		case stIdExpPlugHelp:
			settingsDrawHelp(dis)
		case stIdAutoToggle:
			settingsDrawToggle(dis)
		case stIdCheckBtn:
			settingsDrawCapsule(dis, "检查更新")
		case stIdRestartBtn:
			settingsDrawCapsule(dis, "重启后台服务")
		case stIdRestartInfo:
			settingsDrawServiceStatus(dis)
		case stIdLogRefresh:
			settingsDrawCapsule(dis, "清空")
		case stIdExpSessions:
			settingsDrawCheck(dis, settingsExpSessions)
		case stIdExpPlugins:
			settingsDrawCheck(dis, settingsExpPlugins)
		case stIdExpFiles:
			settingsDrawCheck(dis, settingsExpFiles)
		case stIdExpAddDir:
			settingsDrawCapsule(dis, "选择目录…")
		case stIdExpGo:
			settingsDrawCapsule(dis, "导出…")
		case stIdImpAdd:
			settingsDrawCapsule(dis, "添加导入压缩包…")
		case stIdImpSessBtn, stIdImpPlugBtn, stIdImpFilesBtn:
			settingsDrawCapsule(dis, "恢复")
		}
		return 1
	}
	ret, _, _ := pDefWindowProcW.Call(hwnd, uMsg, wParam, lParam)
	return ret
}

// settingsShowPane 切换右侧内容面板（按当前分类显示/隐藏控件）。
func settingsShowPane(hwnd uintptr) {
	settingsCloseDrop(false) // 切换面板时收起日志下拉列表
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
	for _, id := range settingsPaneLog {
		if w := settingsWidgetKey(id); w != 0 {
			pShowWindow.Call(w, swHide)
		}
	}
	for _, id := range settingsPaneExp {
		if w := settingsWidgetKey(id); w != 0 {
			pShowWindow.Call(w, swHide)
		}
	}
	for _, id := range settingsPaneImp {
		if w := settingsWidgetKey(id); w != 0 {
			pShowWindow.Call(w, swHide)
		}
	}
	var show []uintptr
	var title string
	switch settingsCat {
	case 0:
		show = settingsPaneGen
		title = "常规"
	case 1:
		show = settingsPaneAbout
		title = "关于"
	case 2:
		show = settingsPaneLog
		title = "日志"
	case 3:
		show = settingsPaneExp
		title = "导出"
	default:
		show = settingsPaneImp
		title = "导入"
	}
	for _, id := range show {
		if w := settingsWidgetKey(id); w != 0 {
			pShowWindow.Call(w, swShow)
		}
	}
	// 导入页：恢复行仅在解析成功后显示（切换面板时按当前状态复原）
	if settingsCat == 4 {
		settingsHideImportRows()
		if len(settingsImpItems) > 0 {
			settingsShowImportRows(settingsImpItems)
		}
	}
	if settingsTitleHwnd != 0 {
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

// makeMonoFont 创建等宽字体（日志展示用 Consolas）。
func makeMonoFont(height int32) uintptr {
	face, _ := syscall.UTF16PtrFromString("Consolas")
	h, _, _ := pCreateFontW.Call(uintptr(height), 0, 0, 0, 400, 0, 0, 0, defaultCharset, 0, 0, cleartypeQual, 0, uintptr(unsafe.Pointer(face)))
	return h
}

// settingsCurrentLogName 返回当前下拉选中的日志文件名。
func settingsCurrentLogName() string {
	if settingsLogComboSel == 1 {
		return "server.log"
	}
	return "app.log"
}

// settingsLogClear 清空所选日志文件（截断为 0 字节），并刷新显示。
func settingsLogClear() {
	name := settingsCurrentLogName()
	p := filepath.Join(logDir, name)
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		log.Printf("clear log failed: %v", err)
	}
	settingsLogReload(true) // 清空后刷新：滚动到底部
}

// textmetric 与 gdi32 TEXTMETRIC 对应，用于计算日志行高。
type textmetric struct {
	tmHeight       int32
	tmAscent       int32
	tmDescent      int32
	tmInternalLead int32
	tmExternalLead int32
	tmAveCharWidth int32
	tmMaxCharWidth int32
	tmWeight       int32
	tmOverhang     int32
	tmDigitizedX   int32
	tmDigitizedY   int32
	tmFirstChar    byte
	tmLastChar     byte
	tmDefaultChar  byte
	tmBreakChar    byte
	tmItalic       byte
	tmUnderlined   byte
	tmStruckOut    byte
	tmPitchFamily  byte
	tmCharSet      byte
}

// settingsLogLastContent 上次加载的日志文本（用于判断是否有新写入，避免无变化时重置滚动）。
var settingsLogLastContent string

// settingsLogVL 校准后的可视行数：每次滚到底后 = 行数 - 首可见行，比静态预估更准（用于判断贴底）。
var settingsLogVL int32

// settingsLogVisibleLines 估算日志控件可视行数（初次使用；之后由 settingsLogVL 校准）。
func settingsLogVisibleLines(edit uintptr) int32 {
	if settingsLogVL > 0 {
		return settingsLogVL
	}
	if edit == 0 {
		return 14
	}
	var rc rect
	pGetClientRect.Call(edit, uintptr(unsafe.Pointer(&rc)))
	H := int32(rc.bottom - rc.top)
	if H <= 0 {
		return 14
	}
	dc, _, _ := pGetDC.Call(0)
	if dc == 0 {
		return 14
	}
	defer pReleaseDC.Call(0, dc)
	old, _, _ := pSelectObject.Call(dc, settingsFontMono)
	var tm textmetric
	pGetTextMetrics.Call(dc, uintptr(unsafe.Pointer(&tm)))
	pSelectObject.Call(dc, old)
	lh := tm.tmHeight + tm.tmExternalLead
	if lh <= 0 {
		lh = 18
	}
	return H / lh
}

// settingsLogAtBottom 判断当前日志是否已滚动到底部（用于决定是否跟随新增内容自动滚到底）。
func settingsLogAtBottom(edit uintptr) bool {
	lc, _, _ := pSendMessageW.Call(edit, emGetLineCount, 0, 0)
	fv, _, _ := pSendMessageW.Call(edit, emGetFirstVisibleLine, 0, 0)
	vl := settingsLogVisibleLines(edit)
	if vl < 1 {
		vl = 1
	}
	return int64(fv)+int64(vl) >= int64(lc)-1
}

// settingsLogReload 按当前选择重新加载日志到只读文本框（CRLF 换行）。
// forceScroll：打开/切换日志页时传 true，无论内容是否变化都滚动到底部一次；
// 定时跟随传 false——仅当有新写入且用户贴底时才自动滚到底，用户手动上翻则不打断。
func settingsLogReload(forceScroll bool) {
	name := settingsCurrentLogName()
	if info := settingsWidgetKey(stIdLogInfo); info != 0 {
		// 说明文本显示当前日志文件的完整路径
		ip, _ := syscall.UTF16PtrFromString(filepath.Join(logDir, name))
		pSendMessageW.Call(info, wmSetText, 0, uintptr(unsafe.Pointer(ip)))
	}
	text := ""
	p := filepath.Join(logDir, name)
	if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
		// 只展示尾部（最新）100KB，且截到完整行，避免翻页/换行问题
		const max = 100 * 1024
		if len(data) > max {
			data = data[len(data)-max:]
		}
		text = string(data)
	} else {
		text = "（暂无日志：" + p + "）"
	}
	// Win32 多行 EDIT 需 \r\n 换行；Go 日志为 \n，这里统一转 CRLF，否则文字堆叠。
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")

	edit := settingsWidgetKey(stIdLogEdit)
	if edit == 0 {
		return
	}

	// 打开/切换到日志页（或清空/切换日志文件）：无论内容是否变化都重设文本并重绘，保证每次切入都渲染出内容
	if forceScroll {
		ep, _ := syscall.UTF16PtrFromString(text)
		pSendMessageW.Call(edit, wmSetText, 0, uintptr(unsafe.Pointer(ep)))
		settingsLogLastContent = text
		// 先强制一次重绘：让 RichEdit 完成文本排版/建立滚动范围
		pUpdateWindow.Call(edit)
		settingsLogScrollToEnd(edit, text)
		// 校准可视行数（真实值 = 行数 - 首可见行），供后续“是否贴底”判断
		lc, _, _ := pSendMessageW.Call(edit, emGetLineCount, 0, 0)
		fv, _, _ := pSendMessageW.Call(edit, emGetFirstVisibleLine, 0, 0)
		if lc > 0 {
			settingsLogVL = int32(lc - fv)
		}
		return
	}

	// 定时刷新（跟随最新日志）：无新写入则不重设文本（避免把用户手动上翻的位置拉回顶部）
	if text == settingsLogLastContent {
		return
	}
	settingsLogLastContent = text

	// 记录当前滚动位置与是否贴底，用于文本重置后决定是否跟随
	atBottom := settingsLogAtBottom(edit)
	firstVisible, _, _ := pSendMessageW.Call(edit, emGetFirstVisibleLine, 0, 0)

	ep, _ := syscall.UTF16PtrFromString(text)
	pSendMessageW.Call(edit, wmSetText, 0, uintptr(unsafe.Pointer(ep)))
	pUpdateWindow.Call(edit) // 强制重绘以确保滚动范围就绪
	if atBottom {
		// 贴底：跟随追加内容，自动滚到最后（tail -f 式）
		settingsLogScrollToEnd(edit, text)
		lc, _, _ := pSendMessageW.Call(edit, emGetLineCount, 0, 0)
		fv, _, _ := pSendMessageW.Call(edit, emGetFirstVisibleLine, 0, 0)
		if lc > 0 {
			settingsLogVL = int32(lc - fv)
		}
	} else {
		// 用户手动上翻：重置文本后回滚到原位置，不打断阅读
		pSendMessageW.Call(edit, wmVScroll, sbThumbPos, firstVisible)
		pRedrawWindow.Call(edit, 0, 0, rdwInvalidate|rdwUpdateNow|rdwAllChildren)
	}
}

// settingsLogScrollToEnd 把日志滚动到文末：按总行数 EM_LINESCROLL（系统钳制，不会滚过头），
// 再同步强制完整重绘，避免视口停在空白区（需手动滚动才恢复的问题）。
func settingsLogScrollToEnd(edit uintptr, text string) {
	lc, _, _ := pSendMessageW.Call(edit, emGetLineCount, 0, 0)
	pSendMessageW.Call(edit, emLineScroll, 0, lc)
	pRedrawWindow.Call(edit, 0, 0, rdwInvalidate|rdwUpdateNow|rdwAllChildren)
}

// settingsDrawCat 绘制侧栏分类按钮（选中项浅灰胶囊 + 蓝色文字）。
func settingsDrawCat(dis drawItemStruct) {
	id := int(settingsWidgets[dis.hwndItem])
	selected := int(id-stIdCatGeneral) == settingsCat
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
	switch id {
	case stIdCatAbout:
		label = "关于"
	case stIdCatLog:
		label = "日志"
	case stIdCatExport:
		label = "导出"
	case stIdCatImport:
		label = "导入"
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

// 后台服务三态：已停止 / 启动中 / 运行中。
const (
	serviceStateStopped int32 = iota
	serviceStateStarting
	serviceStateRunning
)

// settingsSetServiceState 记录后台服务状态并触发状态控件重绘（可跨线程调用）。
func settingsSetServiceState(state int32) {
	settingsSvcState.Store(state)
	if settingsRestartInfoHwnd != 0 {
		pInvalidateRect.Call(settingsRestartInfoHwnd, 0, 1)
	}
}

// settingsCurrentServiceState 依据服务实际运行状态推导展示状态：就绪→运行中；
// 尚未就绪且未判定失败→启动中（后台拉起阶段）；已失败→已停止。
func settingsCurrentServiceState() int32 {
	if serverResponding(webURL) {
		return serviceStateRunning
	}
	if serviceFailed.Load() {
		return serviceStateStopped
	}
	return serviceStateStarting
}

// settingsDrawServiceStatus 自绘“后台服务”状态：红点=已停止，黄点=启动中，绿点=运行中。
func settingsDrawServiceStatus(dis drawItemStruct) {
	hdc := dis.hDC
	if wb, _, _ := pGetStockObject.Call(whiteBrush); wb != 0 {
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&dis.rcItem)), wb)
	}
	st := settingsSvcState.Load()
	// 圆点（8px，垂直居中）：ARGB——红=已停止 #DC2626，黄=启动中 #F59E0B，绿=运行中 #16A34A
	dotColor := uint32(0xFFDC2626)
	label := "后台服务：已停止"
	switch st {
	case serviceStateStarting:
		dotColor = 0xFFF59E0B
		label = "后台服务：启动中"
	case serviceStateRunning:
		dotColor = 0xFF16A34A
		label = "后台服务：运行中"
	}
	dotD := int32(8)
	dy := dis.rcItem.top + (dis.rcItem.bottom-dis.rcItem.top-dotD)/2
	dot := rect{dis.rcItem.left + 2, dy, dis.rcItem.left + 2 + dotD, dy + dotD}
	fillRoundedRectAA(hdc, dot, dotD/2, dotColor)
	// 文本
	pSetTextColor.Call(hdc, stColorSub)
	pSetBkColor.Call(hdc, colorWhite)
	pSetBkMode.Call(hdc, bkOpaque)
	if settingsFontBody != 0 {
		pSelectObject.Call(hdc, settingsFontBody)
	}
	lp, _ := syscall.UTF16PtrFromString(label)
	tr := rect{dis.rcItem.left + 16, dis.rcItem.top, dis.rcItem.right, dis.rcItem.bottom}
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(lp)), ^uintptr(0), uintptr(unsafe.Pointer(&tr)), dtLeft|dtVCenter|dtSingle)
}

// askRestartService 重启前确认：true=确认重新启动。
func askRestartService() bool {
	return runModernDialog(appName, "是否重启后台 Web 服务？\n重启期间 Web UI 会短暂不可用。", []string{"重新启动", "取消"}, 0) == 0
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
	if settingsFontBtn != 0 {
		pSelectObject.Call(hdc, settingsFontBtn)
	}
	t, _ := syscall.UTF16PtrFromString(label)
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(t)), ^uintptr(0), uintptr(unsafe.Pointer(&dis.rcItem)), dtCenter|dtVCenter|dtSingle)
}

// settingsDrawCheck 绘制自绘复选框（圆角方框，选中=品牌蓝底+白色对勾）。
func settingsDrawCheck(dis drawItemStruct, checked bool) {
	hdc := dis.hDC
	if wb, _, _ := pGetStockObject.Call(whiteBrush); wb != 0 {
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&dis.rcItem)), wb)
	}
	d := int32(18)
	x := dis.rcItem.left + 4
	y := dis.rcItem.top + (dis.rcItem.bottom-dis.rcItem.top-d)/2
	// 灰色描边：外框灰 + 内框白
	fillRoundedRectAA(hdc, rect{x - 1, y - 1, x + d + 1, y + d + 1}, 5, colorRefToARGB(stColorGray))
	fillRoundedRectAA(hdc, rect{x + 1, y + 1, x + d - 1, y + d - 1}, 4, 0xFFFFFFFF)
	if checked {
		fillRoundedRectAA(hdc, rect{x + 2, y + 2, x + d - 2, y + d - 2}, 3, colorRefToARGB(stColorBlue))
		pSetTextColor.Call(hdc, dialogColorWhite)
		pSetBkMode.Call(hdc, bkTransparent)
		if settingsFontBold != 0 {
			pSelectObject.Call(hdc, settingsFontBold)
		}
		chk, _ := syscall.UTF16PtrFromString("✓")
		rc := rect{x, y, x + d, y + d}
		pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(chk)), ^uintptr(0), uintptr(unsafe.Pointer(&rc)), dtCenter|dtVCenter|dtSingle)
	}
}

// settingsDrawHelp 绘制问号图标（灰圈 + ?，问号在圆心精确居中），悬停时由原生泡泡提示显示说明。
func settingsDrawHelp(dis drawItemStruct) {
	hdc := dis.hDC
	if wb, _, _ := pGetStockObject.Call(whiteBrush); wb != 0 {
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&dis.rcItem)), wb)
	}
	pressed := dis.itemState&odsSelected != 0
	ring := uintptr(stColorGray)
	if pressed {
		ring = stColorSub
	}
	fillRoundedRectAA(hdc, dis.rcItem, 7, colorRefToARGB(ring))
	inner := rect{dis.rcItem.left + 1, dis.rcItem.top + 1, dis.rcItem.right - 1, dis.rcItem.bottom - 1}
	fillRoundedRectAA(hdc, inner, 6, 0xFFFFFFFF)
	// 加粗加深的 ?”：用正文加粗字号（600）、深色文字
	pSetTextColor.Call(hdc, stColorText)
	pSetBkMode.Call(hdc, bkTransparent)
	if settingsFontBold != 0 {
		pSelectObject.Call(hdc, settingsFontBold)
	}
	// 测量 "?" 实际宽高，在圆内居中绘制（字形视觉重心略偏上，上移 1px）
	q, _ := syscall.UTF16PtrFromString("?")
	var sz rect
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(q)), ^uintptr(0), uintptr(unsafe.Pointer(&sz)), dtCalcRect|dtSingle)
	w := sz.right - sz.left
	h := sz.bottom - sz.top
	if w <= 0 {
		w = 8
	}
	if h <= 0 {
		h = 12
	}
	cx := (dis.rcItem.left + dis.rcItem.right) / 2
	cy := (dis.rcItem.top + dis.rcItem.bottom) / 2
	tr := rect{cx - w/2, cy - h/2 - 1, cx + w/2, cy + h/2 - 1}
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(q)), ^uintptr(0), uintptr(unsafe.Pointer(&tr)), 0) // DT_LEFT|DT_TOP
}

// settingsDrawCombo 绘制现代下拉选择器（圆角白底 + 边框 + 当前项文案 + 箭头）。
func settingsDrawCombo(dis drawItemStruct) {
	hdc := dis.hDC
	if wb, _, _ := pGetStockObject.Call(whiteBrush); wb != 0 {
		pFillRect.Call(hdc, uintptr(unsafe.Pointer(&dis.rcItem)), wb)
	}
	border := uintptr(stColorGray)
	if settingsComboOpen {
		border = stColorBlue
	}
	fillRoundedRectAA(hdc, dis.rcItem, 8, colorRefToARGB(border))
	inner := rect{dis.rcItem.left + 1, dis.rcItem.top + 1, dis.rcItem.right - 1, dis.rcItem.bottom - 1}
	innerFill := uint32(0xFFFFFFFF)
	if dis.itemState&odsSelected != 0 {
		innerFill = colorRefToARGB(stColorSidebarBg)
	}
	fillRoundedRectAA(hdc, inner, 7, innerFill)
	name := settingsCurrentLogName()
	pSetTextColor.Call(hdc, stColorText)
	pSetBkMode.Call(hdc, bkTransparent)
	if settingsFontBody != 0 {
		pSelectObject.Call(hdc, settingsFontBody)
	}
	t, _ := syscall.UTF16PtrFromString(name)
	rc := rect{dis.rcItem.left + 12, dis.rcItem.top, dis.rcItem.right - 34, dis.rcItem.bottom}
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(t)), ^uintptr(0), uintptr(unsafe.Pointer(&rc)), dtLeft|dtVCenter|dtSingle)
	pSetTextColor.Call(hdc, stColorSub)
	ch, _ := syscall.UTF16PtrFromString("▾")
	rc2 := rect{dis.rcItem.right - 26, dis.rcItem.top, dis.rcItem.right - 8, dis.rcItem.bottom}
	pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(ch)), ^uintptr(0), uintptr(unsafe.Pointer(&rc2)), dtCenter|dtVCenter|dtSingle)
}

// ==================== 日志下拉列表（现代选择器弹层） ====================

type trackMouseEvent struct {
	cbSize      uint32
	dwFlags     uint32
	hwndTrack   uintptr
	dwHoverTime uint32
}

// toolInfoW 对应 TOOLINFO（TTM_ADDTOOLW 用）。
type toolInfoW struct {
	cbSize   uint32
	uFlags   uint32
	hwnd     uintptr
	uId      uintptr
	rect     rect
	hinst    uintptr
	lpszText *uint16
}

// settingsDropWndProc 下拉列表窗口：悬停高亮、点击/回车选择、Esc 关闭、失活收起。
func settingsDropWndProc(hwnd, uMsg, wParam, lParam uintptr) uintptr {
	switch uMsg {
	case wmMouseMove:
		if idx := settingsDropItemAt(lParam); idx != settingsDropHover {
			settingsDropHover = idx
			pInvalidateRect.Call(hwnd, 0, 0)
		}
		var tme trackMouseEvent
		tme.cbSize = uint32(unsafe.Sizeof(tme))
		tme.dwFlags = tmeLeave
		tme.hwndTrack = hwnd
		pTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tme)))
		return 0
	case wmMouseLeave:
		settingsDropHover = -1
		pInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmLButtonUp:
		if idx := settingsDropItemAt(lParam); idx >= 0 {
			settingsDropPick(idx)
		}
		return 0
	case wmKeyDown:
		switch wParam {
		case vkEscape:
			settingsCloseDrop(false)
		case vkUp, vkDown:
			next := settingsDropHover
			if next < 0 {
				next = settingsLogComboSel
			}
			if wParam == vkUp {
				next--
			} else {
				next++
			}
			if next < 0 {
				next = 0
			}
			if next > 1 {
				next = 1
			}
			settingsDropHover = next
			pInvalidateRect.Call(hwnd, 0, 0)
		case vkReturn:
			if settingsDropHover >= 0 {
				settingsDropPick(settingsDropHover)
			}
		}
		return 0
	case wmKillFocus:
		settingsCloseDrop(true)
		return 0
	case wmActivate:
		if int(wParam) == waInactive {
			settingsCloseDrop(true)
		}
		return 0
	case wmPaint:
		settingsDropPaint(hwnd)
		return 0
	}
	ret, _, _ := pDefWindowProcW.Call(hwnd, uMsg, wParam, lParam)
	return ret
}

// settingsDropItemAt 把鼠标客户区坐标换算为下拉项索引（无效返回 -1）。
func settingsDropItemAt(lParam uintptr) int {
	y := int32((lParam >> 16) & 0xFFFF)
	if y < dropPadY {
		return -1
	}
	i := int((y - dropPadY) / dropItemH)
	if i > 1 {
		i = 1
	}
	return i
}

// settingsDropPaint 绘制下拉列表：圆角白底 + 边框 + 悬停高亮 + 当前项蓝字带对勾。
func settingsDropPaint(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	var cr rect
	pGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&cr)))
	fillRoundedRectAA(hdc, cr, 10, colorRefToARGB(stColorGray))
	inner := rect{cr.left + 1, cr.top + 1, cr.right - 1, cr.bottom - 1}
	fillRoundedRectAA(hdc, inner, 9, 0xFFFFFFFF)
	names := []string{"app.log", "server.log"}
	for i, name := range names {
		rc := rect{int32(dropPadX), int32(dropPadY + i*dropItemH), cr.right - int32(dropPadX), int32(dropPadY + (i+1)*dropItemH)}
		if i == settingsDropHover {
			hover := rect{rc.left + 4, rc.top + 2, rc.right - 4, rc.bottom - 2}
			fillRoundedRectAA(hdc, hover, 6, colorRefToARGB(stColorSidebarBg))
		}
		if i == settingsLogComboSel {
			pSetTextColor.Call(hdc, stColorBlue)
		} else {
			pSetTextColor.Call(hdc, stColorText)
		}
		pSetBkMode.Call(hdc, bkTransparent)
		if settingsFontBody != 0 {
			pSelectObject.Call(hdc, settingsFontBody)
		}
		t, _ := syscall.UTF16PtrFromString(name)
		tr := rect{rc.left + 12, rc.top, rc.right - 40, rc.bottom}
		pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(t)), ^uintptr(0), uintptr(unsafe.Pointer(&tr)), dtLeft|dtVCenter|dtSingle)
		if i == settingsLogComboSel {
			pSetTextColor.Call(hdc, stColorBlue)
			ck, _ := syscall.UTF16PtrFromString("✓")
			cr2 := rect{rc.right - 34, rc.top, rc.right - 8, rc.bottom}
			pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(ck)), ^uintptr(0), uintptr(unsafe.Pointer(&cr2)), dtCenter|dtVCenter|dtSingle)
		}
	}
}

// settingsToggleDrop 展开/收起日志文件下拉列表。
// 刚因点击别处自动收起时，同一击的 BN_CLICKED 不再立即重开（防抖）。
func settingsToggleDrop() {
	if settingsDropHwnd != 0 {
		settingsCloseDrop(false)
		return
	}
	if time.Since(settingsDropClosedAt) < 250*time.Millisecond {
		return
	}
	settingsOpenDrop()
}

// settingsOpenDrop 在选择器按钮下方弹出圆角下拉列表。
func settingsOpenDrop() {
	if settingsDropHwnd != 0 {
		return
	}
	btn := settingsWidgetKey(stIdLogCombo)
	if btn == 0 {
		return
	}
	if !settingsDropClsReg {
		cls, _ := syscall.UTF16PtrFromString(dropCls)
		cb := syscall.NewCallback(settingsDropWndProc)
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
		settingsDropClsReg = true
	}
	var rc rect
	pGetWindowRect.Call(btn, uintptr(unsafe.Pointer(&rc)))
	w := rc.right - rc.left
	h := int32(dropPadY*2 + dropItemH*2)
	cls, _ := syscall.UTF16PtrFromString(dropCls)
	owner, _, _ := pGetParent.Call(btn)
	hwnd, _, _ := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(cls)), 0,
		wsPopup,
		uintptr(rc.left), uintptr(rc.bottom+4), uintptr(w), uintptr(h),
		owner, 0, moduleHandle(), 0,
	)
	if hwnd == 0 {
		return
	}
	settingsDropHwnd = hwnd
	settingsComboOpen = true
	settingsDropHover = -1
	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	pSetFocus.Call(hwnd)
	settingsRedrawWidget(stIdLogCombo)
}

// settingsCloseDrop 收起日志下拉列表（可重复调用）。
// reopenGuard=true 表示因失活自动收起：同一次点击产生的重新展开会被防抖吞掉。
func settingsCloseDrop(reopenGuard bool) {
	if settingsDropHwnd == 0 {
		return
	}
	if reopenGuard {
		settingsDropClosedAt = time.Now()
	}
	h := settingsDropHwnd
	settingsDropHwnd = 0
	settingsComboOpen = false
	settingsDropHover = -1
	pDestroyWindow.Call(h)
	settingsRedrawWidget(stIdLogCombo)
}

// settingsDropPick 选中下拉项：切换日志文件并刷新。
func settingsDropPick(idx int) {
	if idx != settingsLogComboSel {
		settingsLogComboSel = idx
		settingsCloseDrop(false)
		settingsLogReload(true)
		return
	}
	settingsCloseDrop(false)
}

// settingsSetText 设置静态文本/EDIT 控件文本（多行控件自动转 CRLF，可跨线程调用）。
func settingsSetText(id int, text string) {
	w := settingsWidgetKey(uintptr(id))
	if w == 0 {
		return
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
	t, _ := syscall.UTF16PtrFromString(text)
	pSendMessageW.Call(w, wmSetText, 0, uintptr(unsafe.Pointer(t)))
}

// settingsExpDirsUpdate 刷新导出页已选目录列表。
func settingsExpDirsUpdate() {
	if len(settingsExpDirs) == 0 {
		settingsSetText(stIdExpDirs, "（尚未选择目录）")
		return
	}
	settingsSetText(stIdExpDirs, "已选目录：\n"+strings.Join(settingsExpDirs, "\n"))
}

// settingsExportRun 执行导出：选择保存位置 → 后台打包 → 结果弹窗。
func settingsExportRun() {
	if settingsExpBusy.Load() {
		return
	}
	if !settingsExpSessions && !settingsExpPlugins && !settingsExpFiles {
		runModernDialog(appName, "请至少勾选一项导出内容。", []string{"确定"}, 0)
		return
	}
	if settingsExpFiles && len(settingsExpDirs) == 0 {
		runModernDialog(appName, "已勾选「需要打包的文件目录」，请先点击「选择目录…」添加目录。", []string{"确定"}, 0)
		return
	}
	destDir := pickHarnessDir("选择导出包的保存位置", "")
	if destDir == "" {
		return
	}
	settingsExpBusy.Store(true)
	go func() {
		defer settingsExpBusy.Store(false)
		dirs := append([]string(nil), settingsExpDirs...) // 快照，避免导出期间列表被修改
		splash := startSplash("正在导出…")
		settingsSetText(stIdExpStatus, "正在导出…")
		path, err := buildExportZip(settingsExpSessions, settingsExpPlugins, dirs, destDir, func(t string, pct float64) {
			if t != "" {
				settingsSetText(stIdExpStatus, t)
				splash.Update(t, pct)
			}
		})
		splash.Close()
		if err != nil {
			settingsSetText(stIdExpStatus, "导出失败")
			runModernDialog(appName, "导出失败：\n"+err.Error(), []string{"确定"}, 0)
			return
		}
		settingsSetText(stIdExpStatus, "导出完成")
		runModernDialog(appName, "导出完成：\n"+path, []string{"确定"}, 0)
	}()
}

// settingsHideImportRows 隐藏导入页三个恢复行。
func settingsHideImportRows() {
	for _, id := range []int{stIdImpSessRow, stIdImpSessBtn, stIdImpPlugRow, stIdImpPlugBtn, stIdImpFilesRow, stIdImpFilesBtn} {
		if w := settingsWidgetKey(uintptr(id)); w != 0 {
			pShowWindow.Call(w, swHide)
		}
	}
}

// settingsShowImportRows 按解析出的可恢复项显示对应行。
func settingsShowImportRows(items []importItem) {
	settingsHideImportRows()
	for _, it := range items {
		var rowID, btnID int
		switch it.Kind {
		case "sessions":
			rowID, btnID = stIdImpSessRow, stIdImpSessBtn
		case "plugins":
			rowID, btnID = stIdImpPlugRow, stIdImpPlugBtn
		case "files":
			rowID, btnID = stIdImpFilesRow, stIdImpFilesBtn
		default:
			continue
		}
		label := it.Label
		if it.Size > 0 {
			label += fmt.Sprintf("（%.1f MB）", float64(it.Size)/(1024*1024))
		}
		settingsSetText(rowID, label)
		if w := settingsWidgetKey(uintptr(rowID)); w != 0 {
			pShowWindow.Call(w, swShow)
		}
		if w := settingsWidgetKey(uintptr(btnID)); w != 0 {
			pShowWindow.Call(w, swShow)
		}
	}
}

// settingsImportLoad 解析所选导入压缩包（后台线程），成功罗列可恢复项，失败显示解析异常。
func settingsImportLoad(p string) {
	if settingsImpBusy.Load() {
		return
	}
	settingsImpBusy.Store(true)
	settingsImpPath = p
	settingsImpItems = nil
	settingsHideImportRows()
	settingsSetText(stIdImpPath, "导入压缩包：\n"+p)
	settingsSetText(stIdImpStatus, "正在解析压缩包…")
	go func() {
		items, err := parseExportZip(p)
		if err != nil {
			settingsImpBusy.Store(false)
			settingsSetText(stIdImpStatus, "解析异常："+err.Error())
			return
		}
		settingsImpItems = items
		settingsImpBusy.Store(false)
		settingsShowImportRows(items)
		settingsSetText(stIdImpStatus, fmt.Sprintf("解析成功：共 %d 个可恢复项，点击右侧「恢复」逐项恢复。", len(items)))
	}()
}

// settingsRestoreRun 恢复指定子包：会话/插件先查冲突并弹窗询问，文件目录恢复前已由调用方选定解压位置。
// 恢复期间暂停后台服务，完成后自动重启，保证不损坏正在运行的 harness 环境。
func settingsRestoreRun(kind, filesDest string) {
	if settingsImpBusy.Load() || settingsImpPath == "" {
		return
	}
	var item *importItem
	for i := range settingsImpItems {
		if settingsImpItems[i].Kind == kind {
			item = &settingsImpItems[i]
			break
		}
	}
	if item == nil {
		runModernDialog(appName, "当前导入包中没有该项内容。", []string{"确定"}, 0)
		return
	}
	settingsImpBusy.Store(true)
	go func() {
		defer settingsImpBusy.Store(false)
		inner, cleanup, err := extractInnerZip(settingsImpPath, item.Zip)
		if err != nil {
			settingsSetText(stIdImpStatus, "恢复失败："+err.Error())
			return
		}
		defer cleanup()

		overwrite := true
		if kind != "files" {
			n, err := countRestoreConflicts(kind, inner)
			if err != nil {
				settingsSetText(stIdImpStatus, "恢复失败："+err.Error())
				return
			}
			if n > 0 {
				r := runModernDialog(appName, fmt.Sprintf("检测到 %d 项与当前环境存在冲突。\n\n覆盖更新：恢复前备份现有数据，恢复成功后自动清理备份。\n跳过已有：保留现有数据，仅补充缺失内容。", n), []string{"覆盖更新", "跳过已有", "取消"}, 1)
				if r < 0 || r == 2 {
					settingsSetText(stIdImpStatus, "已取消恢复。")
					return
				}
				overwrite = r == 0
			}
		}

		stopped := pauseServiceForRestore()
		_, err = restoreItem(kind, inner, filesDest, overwrite, func(t string, pct float64) {
			if t != "" {
				settingsSetText(stIdImpStatus, t)
			}
		})
		if stopped && kind != "files" {
			// 不自动重启：关闭设置窗口时提示用户重启生效（复用常规页重启流程）
			settingsRestorePending = true
		}
		if err != nil {
			settingsSetText(stIdImpStatus, "恢复失败")
			runModernDialog(appName, "恢复失败：\n"+err.Error(), []string{"确定"}, 0)
			return
		}
		msg := "恢复完成。"
		settingsSetText(stIdImpStatus, msg)
		runModernDialog(appName, msg, []string{"确定"}, 0)
	}()
}

// ---- 文件选择对话框（GetOpenFileNameW） ----

type openFileNameW struct {
	lStructSize       uint32
	hwndOwner         uintptr
	hInstance         uintptr
	lpstrFilter       *uint16
	lpstrCustomFilter *uint16
	nMaxCustFilter    uint32
	nFilterIndex      uint32
	lpstrFile         *uint16
	nMaxFile          uint32
	lpstrFileTitle    *uint16
	nMaxFileTitle     uint32
	lpstrInitialDir   *uint16
	lpstrTitle        *uint16
	flags             uint32
	nFileOffset       uint16
	nFileExtension    uint16
	lpstrDefExt       *uint16
	lCustData         uintptr
	lpfnHook          uintptr
	lpTemplateName    *uint16
	pvReserved        uintptr
	dwReserved        uint32
	flagsEx           uint32
}

// pickOpenFile 弹出文件选择对话框（filter 内含 \x00 分隔的成对“描述|通配”并以双 \x00 结尾）；取消返回 ""。
func pickOpenFile(title, filter string) string {
	getOpen := syscall.NewLazyDLL("comdlg32.dll").NewProc("GetOpenFileNameW")
	ft, _ := syscall.UTF16PtrFromString(filter)
	tt, _ := syscall.UTF16PtrFromString(title)
	var buf [4096]uint16
	ofn := openFileNameW{
		lStructSize:  uint32(unsafe.Sizeof(openFileNameW{})),
		lpstrFilter:  ft,
		nFilterIndex: 1,
		lpstrFile:    &buf[0],
		nMaxFile:     4096,
		lpstrTitle:   tt,
		flags:        0x00001000 | 0x00000800 | 0x00000004 | 0x00000008, // FILEMUSTEXIST|PATHMUSTEXIST|HIDEREADONLY|NOCHANGEDIR
	}
	r, _, _ := getOpen.Call(uintptr(unsafe.Pointer(&ofn)))
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:])
}

func createSettingsWindow() uintptr {
	// 重置上次打开遗留的状态（窗口关闭后再次打开时控件句柄/集合需清空，否则面板错乱）
	settingsWidgets = map[uintptr]uintptr{}
	settingsCatBtns = [5]uintptr{}
	settingsPaneGen = nil
	settingsPaneAbout = nil
	settingsPaneLog = nil
	settingsPaneExp = nil
	settingsPaneImp = nil
	settingsTitleHwnd = 0
	settingsLogLastContent = "" // 重置上次内容，避免重开窗口后因内容未变而跳过刷新导致日志框空白
	settingsLogComboSel = 0
	settingsComboOpen = false
	settingsDropHwnd = 0
	settingsExpDirs = nil
	settingsImpPath = ""
	settingsImpItems = nil

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

	settingsFontTitle = makeFont(21, 600)
	settingsFontBody = makeFont(17, 400)
	settingsFontBold = makeFont(17, 600)
	settingsFontSmall = makeFont(15, 400)
	settingsFontMono = makeMonoFont(14) // 日志字体 14px
	settingsFontBtn = makeFont(16, 600)
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
		wsCaption|wsSysMenu|wsClipChildren, // wsClipChildren：防止子控件互绘覆盖（日志卡片与编辑框）
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
		wsChild|wsVisible|wsClipSiblings, // wsClipSiblings：不覆盖其上的分类按钮
		0, 0, stSidebarW, stWinH,
		hwnd, stIdSidebarBg, moduleHandle(), 0,
	)
	if sideBg != 0 {
		settingsWidgets[sideBg] = stIdSidebarBg
	}

	// 分类按钮（自绘）
	catLabels := []string{"常规", "关于", "日志", "导出", "导入"}
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
	asub, _ := syscall.UTF16PtrFromString("")
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
	// 开关（自绘胶囊）：放在“开机自启动”文案右侧，紧贴文字
	tb, _ := syscall.UTF16PtrFromString("")
	autoToggle, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(btnCls)),
		uintptr(unsafe.Pointer(tb)),
		wsChild|wsVisible|wsTabStop|bsOwnDraw,
		uintptr(stContentX+115), 75, 56, 28,
		hwnd, stIdAutoToggle, moduleHandle(), 0,
	)
	if autoToggle != 0 {
		settingsWidgets[autoToggle] = stIdAutoToggle
		settingsPaneGen = append(settingsPaneGen, stIdAutoToggle)
	}

	// ---- 常规面板：后台服务（状态 + 重启按钮） ----
	// 状态用自绘 BUTTON（无 wsTabStop，纯展示）：绿/红圆点 + “后台服务：运行中/已停止”
	rst, _ := syscall.UTF16PtrFromString("")
	restInfo, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(btnCls)),
		uintptr(unsafe.Pointer(rst)),
		wsChild|wsVisible|bsOwnDraw,
		uintptr(stContentX), 158, 240, 26,
		hwnd, stIdRestartInfo, moduleHandle(), 0,
	)
	if restInfo != 0 {
		settingsWidgets[restInfo] = stIdRestartInfo
		settingsRestartInfoHwnd = restInfo
		settingsPaneGen = append(settingsPaneGen, stIdRestartInfo)
	}
	settingsSetServiceState(settingsCurrentServiceState())
	rb, _ := syscall.UTF16PtrFromString("重启后台服务")
	restBtn, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(btnCls)),
		uintptr(unsafe.Pointer(rb)),
		wsChild|wsVisible|wsTabStop|bsOwnDraw,
		uintptr(stContentX), 190, 124, 30,
		hwnd, stIdRestartBtn, moduleHandle(), 0,
	)
	if restBtn != 0 {
		settingsWidgets[restBtn] = stIdRestartBtn
		settingsPaneGen = append(settingsPaneGen, stIdRestartBtn)
	}

	// ---- 关于面板 ----
	vt, _ := syscall.UTF16PtrFromString("dsh-systray 版本号")
	verTitle, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(vt)),
		wsChild|wsVisible,
		uintptr(stContentX), 78, 220, 24,
		hwnd, stIdVerTitle, moduleHandle(), 0,
	)
	if verTitle != 0 {
		settingsWidgets[verTitle] = stIdVerTitle
		pSendMessageW.Call(verTitle, wmSetFont, settingsFontBody, 1)
		settingsPaneAbout = append(settingsPaneAbout, stIdVerTitle)
	}
	verText := withV(appVersion)
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
	// DeepSeek Harness 版本号
	hvText := withV(installedHarnessVersion())
	if hvText == "" {
		hvText = "未检测到"
	}
	ht, _ := syscall.UTF16PtrFromString("DeepSeek Harness 版本号")
	harTitle, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(ht)),
		wsChild|wsVisible,
		uintptr(stContentX), 140, 240, 22,
		hwnd, stIdHarTitle, moduleHandle(), 0,
	)
	if harTitle != 0 {
		settingsWidgets[harTitle] = stIdHarTitle
		pSendMessageW.Call(harTitle, wmSetFont, settingsFontBody, 1)
		settingsPaneAbout = append(settingsPaneAbout, stIdHarTitle)
	}
	fv, _ := syscall.UTF16PtrFromString(hvText)
	harValue, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(staticCls)),
		uintptr(unsafe.Pointer(fv)),
		wsChild|wsVisible,
		uintptr(stContentX), 162, 220, 26,
		hwnd, stIdHarValue, moduleHandle(), 0,
	)
	if harValue != 0 {
		settingsWidgets[harValue] = stIdHarValue
		pSendMessageW.Call(harValue, wmSetFont, settingsFontTitle, 1)
		settingsPaneAbout = append(settingsPaneAbout, stIdHarValue)
	}
	// 检查更新（自绘胶囊）
	cb2, _ := syscall.UTF16PtrFromString("检查更新")
	checkBtn, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(btnCls)),
		uintptr(unsafe.Pointer(cb2)),
		wsChild|wsVisible|wsTabStop|bsOwnDraw,
		uintptr(stContentX), 200, 106, 30,
		hwnd, stIdCheckBtn, moduleHandle(), 0,
	)
	if checkBtn != 0 {
		settingsWidgets[checkBtn] = stIdCheckBtn
		settingsPaneAbout = append(settingsPaneAbout, stIdCheckBtn)
	}

	// ---- 日志面板（只读、可复制、可刷新） ----
	li, _ := syscall.UTF16PtrFromString(filepath.Join(logDir, "app.log"))
	logInfo, _, _ := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(staticCls)), uintptr(unsafe.Pointer(li)),
		wsChild|wsVisible|ssEndEllipsis,
		uintptr(stContentX), 74, 424, 20, hwnd, stIdLogInfo, moduleHandle(), 0,
	)
	if logInfo != 0 {
		settingsWidgets[logInfo] = stIdLogInfo
		pSendMessageW.Call(logInfo, wmSetFont, settingsFontSmall, 1)
		settingsPaneLog = append(settingsPaneLog, stIdLogInfo)
	}

	// 现代下拉选择器：自绘按钮 + 弹出圆角列表（替代原生 COMBOBOX）
	lc, _ := syscall.UTF16PtrFromString("app.log")
	logCombo, _, _ := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(btnCls)), uintptr(unsafe.Pointer(lc)),
		wsChild|wsVisible|wsTabStop|bsOwnDraw,
		uintptr(stContentX), 98, 160, 34, hwnd, stIdLogCombo, moduleHandle(), 0,
	)
	if logCombo != 0 {
		settingsWidgets[logCombo] = stIdLogCombo
		pSendMessageW.Call(logCombo, wmSetFont, settingsFontBody, 1)
		settingsPaneLog = append(settingsPaneLog, stIdLogCombo)
	}

	lr, _ := syscall.UTF16PtrFromString("清空")
	logRefresh, _, _ := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(btnCls)), uintptr(unsafe.Pointer(lr)),
		wsChild|wsVisible|wsTabStop|bsOwnDraw,
		uintptr(stContentX+175), 99, 92, 32, hwnd, stIdLogRefresh, moduleHandle(), 0,
	)
	if logRefresh != 0 {
		settingsWidgets[logRefresh] = stIdLogRefresh
		settingsPaneLog = append(settingsPaneLog, stIdLogRefresh)
	}

	// 用 RICHEDIT50W（可靠的多行富文本：正确处理换行/大文本/滚动/复制）；无边框、浅灰底
	mdll, _ := syscall.UTF16PtrFromString("Msftedit.dll")
	pLoadLibraryW.Call(uintptr(unsafe.Pointer(mdll)))
	editCls, _ := syscall.UTF16PtrFromString("RICHEDIT50W")
	logEdit, _, _ := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(editCls)), 0,
		wsChild|wsVisible|wsTabStop|esMultiline|esAutoVScroll|esReadOnly|wsVScroll,
		uintptr(stContentX-8), 144, uintptr(stWinW-stContentX-12), 292, hwnd, stIdLogEdit, moduleHandle(), 0,
	)
	if logEdit != 0 {
		settingsWidgets[logEdit] = stIdLogEdit
		pSendMessageW.Call(logEdit, emExLimitText, 1, 0x7FFFFFF) // 放开文本上限
		pSendMessageW.Call(logEdit, wmSetFont, settingsFontMono, 1)
		settingsPaneLog = append(settingsPaneLog, stIdLogEdit)
	}

	// ---- 导出面板 ----
	expHome := dshHomeDir()
	if expHome == "" {
		expHome = "~/.dsh"
	}
	expDefs := []struct {
		cbID, lblID, subID int
		lbl, sub           string
		y                  int32
		checked            *bool
	}{
		{stIdExpSessions, stIdExpSessLbl, stIdExpSessSub, "所有历史会话", "sessions.zip · " + filepath.Join(expHome, "sessions"), 62, &settingsExpSessions},
		{stIdExpPlugins, stIdExpPlugLbl, stIdExpPlugSub, "已安装的插件", "plugins.zip · 通过 dsh add 安装的插件", 112, &settingsExpPlugins},
		{stIdExpFiles, stIdExpFilesLbl, stIdExpFilesSub, "需要打包的文件目录", "files.zip · 恢复时选择解压位置", 162, &settingsExpFiles},
	}
	for _, d := range expDefs {
		cbx, _, _ := pCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(btnCls)), 0,
			wsChild|wsVisible|wsTabStop|bsOwnDraw,
			uintptr(stContentX), uintptr(d.y), 28, 28,
			hwnd, uintptr(d.cbID), moduleHandle(), 0,
		)
		if cbx != 0 {
			settingsWidgets[cbx] = uintptr(d.cbID)
			settingsPaneExp = append(settingsPaneExp, uintptr(d.cbID))
		}
		lt, _ := syscall.UTF16PtrFromString(d.lbl)
		lb, _, _ := pCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(staticCls)), uintptr(unsafe.Pointer(lt)),
			wsChild|wsVisible,
			uintptr(stContentX+32), uintptr(d.y+2), 280, 24,
			hwnd, uintptr(d.lblID), moduleHandle(), 0,
		)
		if lb != 0 {
			settingsWidgets[lb] = uintptr(d.lblID)
			pSendMessageW.Call(lb, wmSetFont, settingsFontBody, 1)
			settingsPaneExp = append(settingsPaneExp, uintptr(d.lblID))
		}
		st, _ := syscall.UTF16PtrFromString(d.sub)
		sb2, _, _ := pCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(staticCls)), uintptr(unsafe.Pointer(st)),
			wsChild|wsVisible|ssEndEllipsis,
			uintptr(stContentX+32), uintptr(d.y+26), 390, 18,
			hwnd, uintptr(d.subID), moduleHandle(), 0,
		)
		if sb2 != 0 {
			settingsWidgets[sb2] = uintptr(d.subID)
			pSendMessageW.Call(sb2, wmSetFont, settingsFontSmall, 1)
			settingsPaneExp = append(settingsPaneExp, uintptr(d.subID))
		}
	}
	ad, _ := syscall.UTF16PtrFromString("选择目录…")
	addDirBtn, _, _ := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(btnCls)), uintptr(unsafe.Pointer(ad)),
		wsChild|wsVisible|wsTabStop|bsOwnDraw,
		uintptr(stContentX), 212, 110, 30,
		hwnd, stIdExpAddDir, moduleHandle(), 0,
	)
	if addDirBtn != 0 {
		settingsWidgets[addDirBtn] = stIdExpAddDir
		settingsPaneExp = append(settingsPaneExp, stIdExpAddDir)
	}

	// 「已安装的插件」右侧问号图标（自绘）：紧贴文字放置，悬停弹出泡泡说明
	hp, _ := syscall.UTF16PtrFromString("")
	plugHelpX := int32(stContentX + 140)
	if settingsFontBody != 0 {
		if dc, _, _ := pGetDC.Call(0); dc != 0 {
			old, _, _ := pSelectObject.Call(dc, settingsFontBody)
			pt, _ := syscall.UTF16PtrFromString("已安装的插件")
			var mrc rect
			pDrawTextW.Call(dc, uintptr(unsafe.Pointer(pt)), ^uintptr(0), uintptr(unsafe.Pointer(&mrc)), dtCalcRect|dtSingle)
			pSelectObject.Call(dc, old)
			pReleaseDC.Call(0, dc)
			if w := mrc.right - mrc.left; w > 0 {
				plugHelpX = stContentX + 32 + w + 4
			}
		}
	}
	plugHelp, _, _ := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(btnCls)), uintptr(unsafe.Pointer(hp)),
		wsChild|wsVisible|bsOwnDraw,
		uintptr(plugHelpX), 117, 16, 16,
		hwnd, stIdExpPlugHelp, moduleHandle(), 0,
	)
	if plugHelp != 0 {
		settingsWidgets[plugHelp] = stIdExpPlugHelp
		settingsPaneExp = append(settingsPaneExp, stIdExpPlugHelp)
		// 原生泡泡提示：悬停显示说明
		if settingsTipHwnd == 0 {
			tipCls, _ := syscall.UTF16PtrFromString("tooltips_class32")
			settingsTipHwnd, _, _ = pCreateWindowExW.Call(
				0, uintptr(unsafe.Pointer(tipCls)), 0,
				wsPopup|ttsAlwaysTip|ttsNoprefix|ttsBalloon,
				0, 0, 0, 0, hwnd, 0, moduleHandle(), 0,
			)
		}
		if settingsTipHwnd != 0 {
			settingsPlugHelpTip, _ = syscall.UTF16PtrFromString("仅打包用户安装的插件")
			var ti toolInfoW
			ti.cbSize = uint32(unsafe.Sizeof(ti))
			ti.uFlags = ttfSubclass
			// TTF_SUBCLASS：tooltip 会子类化 hwnd（即问号按钮）并监听其鼠标消息，uId 为工具标识。
			ti.hwnd = plugHelp
			ti.uId = 1
			ti.rect = rect{0, 0, 16, 16}
			ti.lpszText = settingsPlugHelpTip
			pSendMessageW.Call(settingsTipHwnd, ttmAddToolW, 0, uintptr(unsafe.Pointer(&ti)))
			pSendMessageW.Call(settingsTipHwnd, ttmSetMaxTipWidthW, 0, 300)
		}
	}
	dirsEdit, _, _ := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(editCls)), 0,
		wsChild|wsVisible|esMultiline|esReadOnly|esAutoVScroll,
		uintptr(stContentX), 250, 424, 76,
		hwnd, stIdExpDirs, moduleHandle(), 0,
	)
	if dirsEdit != 0 {
		settingsWidgets[dirsEdit] = stIdExpDirs
		pSendMessageW.Call(dirsEdit, wmSetFont, settingsFontSmall, 1)
		settingsPaneExp = append(settingsPaneExp, stIdExpDirs)
	}
	settingsExpDirsUpdate()
	eg, _ := syscall.UTF16PtrFromString("导出…")
	expGoBtn, _, _ := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(btnCls)), uintptr(unsafe.Pointer(eg)),
		wsChild|wsVisible|wsTabStop|bsOwnDraw,
		uintptr(stContentX), 336, 120, 34,
		hwnd, stIdExpGo, moduleHandle(), 0,
	)
	if expGoBtn != 0 {
		settingsWidgets[expGoBtn] = stIdExpGo
		settingsPaneExp = append(settingsPaneExp, stIdExpGo)
	}
	es2, _ := syscall.UTF16PtrFromString("")
	expStatus, _, _ := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(staticCls)), uintptr(unsafe.Pointer(es2)),
		wsChild|wsVisible|ssEndEllipsis,
		uintptr(stContentX), 378, 424, 20,
		hwnd, stIdExpStatus, moduleHandle(), 0,
	)
	if expStatus != 0 {
		settingsWidgets[expStatus] = stIdExpStatus
		pSendMessageW.Call(expStatus, wmSetFont, settingsFontSmall, 1)
		settingsPaneExp = append(settingsPaneExp, stIdExpStatus)
	}

	// ---- 导入面板 ----
	ia, _ := syscall.UTF16PtrFromString("添加导入压缩包…")
	impAddBtn, _, _ := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(btnCls)), uintptr(unsafe.Pointer(ia)),
		wsChild|wsVisible|wsTabStop|bsOwnDraw,
		uintptr(stContentX), 62, 180, 30,
		hwnd, stIdImpAdd, moduleHandle(), 0,
	)
	if impAddBtn != 0 {
		settingsWidgets[impAddBtn] = stIdImpAdd
		settingsPaneImp = append(settingsPaneImp, stIdImpAdd)
	}
	ip, _ := syscall.UTF16PtrFromString("（尚未选择导入压缩包）")
	impPath, _, _ := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(editCls)), uintptr(unsafe.Pointer(ip)),
		wsChild|wsVisible|esMultiline|esReadOnly|esAutoVScroll,
		uintptr(stContentX), 100, 424, 42,
		hwnd, stIdImpPath, moduleHandle(), 0,
	)
	if impPath != 0 {
		settingsWidgets[impPath] = stIdImpPath
		pSendMessageW.Call(impPath, wmSetFont, settingsFontSmall, 1)
		settingsPaneImp = append(settingsPaneImp, stIdImpPath)
	}
	is, _ := syscall.UTF16PtrFromString("点击上方按钮选择 dsh-systray-export 压缩包。")
	impStatus, _, _ := pCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(editCls)), uintptr(unsafe.Pointer(is)),
		wsChild|wsVisible|esMultiline|esReadOnly|esAutoVScroll,
		uintptr(stContentX), 150, 424, 56,
		hwnd, stIdImpStatus, moduleHandle(), 0,
	)
	if impStatus != 0 {
		settingsWidgets[impStatus] = stIdImpStatus
		pSendMessageW.Call(impStatus, wmSetFont, settingsFontSmall, 1)
		settingsPaneImp = append(settingsPaneImp, stIdImpStatus)
	}
	impRows := []struct {
		rowID, btnID int
		y            int32
	}{
		{stIdImpSessRow, stIdImpSessBtn, 216},
		{stIdImpPlugRow, stIdImpPlugBtn, 258},
		{stIdImpFilesRow, stIdImpFilesBtn, 300},
	}
	for _, r := range impRows {
		rt, _ := syscall.UTF16PtrFromString("")
		row, _, _ := pCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(staticCls)), uintptr(unsafe.Pointer(rt)),
			wsChild, // 初始隐藏，解析成功后显示
			uintptr(stContentX), uintptr(r.y+2), 316, 26,
			hwnd, uintptr(r.rowID), moduleHandle(), 0,
		)
		if row != 0 {
			settingsWidgets[row] = uintptr(r.rowID)
			pSendMessageW.Call(row, wmSetFont, settingsFontBody, 1)
			settingsPaneImp = append(settingsPaneImp, uintptr(r.rowID))
		}
		bt, _ := syscall.UTF16PtrFromString("恢复")
		btn, _, _ := pCreateWindowExW.Call(
			0, uintptr(unsafe.Pointer(btnCls)), uintptr(unsafe.Pointer(bt)),
			wsChild|wsTabStop|bsOwnDraw, // 初始隐藏
			uintptr(stContentX+326), uintptr(r.y), 96, 30,
			hwnd, uintptr(r.btnID), moduleHandle(), 0,
		)
		if btn != 0 {
			settingsWidgets[btn] = uintptr(r.btnID)
			settingsPaneImp = append(settingsPaneImp, uintptr(r.btnID))
		}
	}

	// 初始显示「常规」面板
	settingsCat = 0
	settingsShowPane(hwnd)
	settingsRedrawCats()
	// 日志自动刷新/滚动定时器（仅在日志页生效）
	pSetTimer.Call(hwnd, settingsLogTimer, 2000, 0)

	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)
	// 标题栏图标 + 确保设置窗口自动到前台
	setWindowIcon(hwnd)
	pSetForegroundWindow.Call(hwnd)
	return hwnd
}
