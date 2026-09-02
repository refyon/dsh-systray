//go:build windows
// +build windows

//
// vendored from github.com/energye/systray v1.0.3（Apache-2.0，见同目录 LICENSE）。
// dsh-systray 定制点（2026-09）：新增 RunOnLoop / ShowMenuAsync / IsAlive / Reinit 公共 API，
// 消息循环改为 PeekMessage 心跳式（窗口被系统回收后自动重建自愈），
// 菜单/弹菜单操作统一投递到消息循环线程串行执行（修复长运行后单击/右键无响应）。

package systray

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Helpful sources: https://github.com/golang/exp/blob/master/shiny/driver/internal/win32

type (
	Handle uintptr
	HWND   uintptr
)

type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	g32                     = syscall.NewLazyDLL("Gdi32.dll")
	pCreateCompatibleBitmap = g32.NewProc("CreateCompatibleBitmap")
	pCreateCompatibleDC     = g32.NewProc("CreateCompatibleDC")
	pCreateDIBSection       = g32.NewProc("CreateDIBSection")
	pDeleteDC               = g32.NewProc("DeleteDC")
	pSelectObject           = g32.NewProc("SelectObject")

	k32              = syscall.NewLazyDLL("Kernel32.dll")
	pGetModuleHandle = k32.NewProc("GetModuleHandleW")

	s32              = syscall.NewLazyDLL("Shell32.dll")
	pShellNotifyIcon = s32.NewProc("Shell_NotifyIconW")

	u32                    = syscall.NewLazyDLL("User32.dll")
	pCreateMenu            = u32.NewProc("CreateMenu")
	pCreatePopupMenu       = u32.NewProc("CreatePopupMenu")
	pCreateWindowEx        = u32.NewProc("CreateWindowExW")
	pDefWindowProc         = u32.NewProc("DefWindowProcW")
	pRemoveMenu            = u32.NewProc("RemoveMenu")
	pDestroyWindow         = u32.NewProc("DestroyWindow")
	pDispatchMessage       = u32.NewProc("DispatchMessageW")
	pDrawIconEx            = u32.NewProc("DrawIconEx")
	pGetCursorPos          = u32.NewProc("GetCursorPos")
	pGetDC                 = u32.NewProc("GetDC")
	pGetMessage            = u32.NewProc("GetMessageW")
	pGetSystemMetrics      = u32.NewProc("GetSystemMetrics")
	pInsertMenuItem        = u32.NewProc("InsertMenuItemW")
	pIsWindow              = u32.NewProc("IsWindow")
	pLoadCursor            = u32.NewProc("LoadCursorW")
	pLoadIcon              = u32.NewProc("LoadIconW")
	pLoadImage             = u32.NewProc("LoadImageW")
	pPeekMessage           = u32.NewProc("PeekMessageW")
	pPostMessage           = u32.NewProc("PostMessageW")
	pPostQuitMessage       = u32.NewProc("PostQuitMessage")
	pRegisterClass         = u32.NewProc("RegisterClassExW")
	pRegisterWindowMessage = u32.NewProc("RegisterWindowMessageW")
	pReleaseDC             = u32.NewProc("ReleaseDC")
	pSetForegroundWindow   = u32.NewProc("SetForegroundWindow")
	pSetMenuInfo           = u32.NewProc("SetMenuInfo")
	pSetMenuItemInfo       = u32.NewProc("SetMenuItemInfoW")
	pShowWindow            = u32.NewProc("ShowWindow")
	pTrackPopupMenu        = u32.NewProc("TrackPopupMenu")
	pTranslateMessage      = u32.NewProc("TranslateMessage")
	pUnregisterClass       = u32.NewProc("UnregisterClassW")
	pUpdateWindow          = u32.NewProc("UpdateWindow")

	// ErrTrayNotReadyYet is returned by functions when they are called before the tray has been initialized.
	ErrTrayNotReadyYet = errors.New("tray not ready yet")
)

// Contains window class information.
// It is used with the RegisterClassEx and GetClassInfoEx functions.
// https://msdn.microsoft.com/en-us/library/ms633577.aspx
type wndClassEx struct {
	Size, Style                        uint32
	WndProc                            uintptr
	ClsExtra, WndExtra                 int32
	Instance, Icon, Cursor, Background Handle
	MenuName, ClassName                *uint16
	IconSm                             Handle
}

// Registers a window class for subsequent use in calls to the CreateWindow or CreateWindowEx function.
// https://msdn.microsoft.com/en-us/library/ms633587.aspx
func (w *wndClassEx) register() error {
	w.Size = uint32(unsafe.Sizeof(*w))
	res, _, err := pRegisterClass.Call(uintptr(unsafe.Pointer(w)))
	if res == 0 {
		return err
	}
	return nil
}

// Unregisters a window class, freeing the memory required for the class.
// https://msdn.microsoft.com/en-us/library/ms644899.aspx
func (w *wndClassEx) unregister() error {
	res, _, err := pUnregisterClass.Call(
		uintptr(unsafe.Pointer(w.ClassName)),
		uintptr(w.Instance),
	)
	if res == 0 {
		return err
	}
	return nil
}

// Contains information that the system needs to display notifications in the notification area.
// Used by Shell_NotifyIcon.
// https://msdn.microsoft.com/en-us/library/windows/desktop/bb773352(v=vs.85).aspx
// https://msdn.microsoft.com/en-us/library/windows/desktop/bb762159
type notifyIconData struct {
	Size                       uint32
	Wnd                        Handle
	ID, Flags, CallbackMessage uint32
	Icon                       Handle
	Tip                        [128]uint16
	State, StateMask           uint32
	Info                       [256]uint16
	Timeout, Version           uint32
	InfoTitle                  [64]uint16
	InfoFlags                  uint32
	GuidItem                   GUID
	BalloonIcon                Handle
}

func (nid *notifyIconData) add() error {
	const NIM_ADD = 0x00000000
	res, _, err := pShellNotifyIcon.Call(
		uintptr(NIM_ADD),
		uintptr(unsafe.Pointer(nid)),
	)
	if res == 0 {
		return err
	}
	return nil
}

func (nid *notifyIconData) modify() error {
	const NIM_MODIFY = 0x00000001
	res, _, err := pShellNotifyIcon.Call(
		uintptr(NIM_MODIFY),
		uintptr(unsafe.Pointer(nid)),
	)
	if res == 0 {
		return err
	}
	return nil
}

func (nid *notifyIconData) delete() error {
	const NIM_DELETE = 0x00000002
	res, _, err := pShellNotifyIcon.Call(
		uintptr(NIM_DELETE),
		uintptr(unsafe.Pointer(nid)),
	)
	if res == 0 {
		return err
	}
	return nil
}

// Contains information about a menu item.
// https://msdn.microsoft.com/en-us/library/windows/desktop/ms647578(v=vs.85).aspx
type menuItemInfo struct {
	Size, Mask, Type, State     uint32
	ID                          uint32
	SubMenu, Checked, Unchecked Handle
	ItemData                    uintptr
	TypeData                    *uint16
	Cch                         uint32
	BMPItem                     Handle
}

// The POINT structure defines the x- and y- coordinates of a point.
// https://msdn.microsoft.com/en-us/library/windows/desktop/dd162805(v=vs.85).aspx
type point struct {
	X, Y int32
}

// The BITMAPINFO structure defines the dimensions and color information for a DIB.
// https://learn.microsoft.com/en-us/windows/win32/api/wingdi/ns-wingdi-bitmapinfo
type bitmapInfo struct {
	BmiHeader bitmapInfoHeader
	BmiColors Handle
}

// The BITMAPINFOHEADER structure contains information about the dimensions and color format of a device-independent bitmap (DIB).
// https://learn.microsoft.com/en-us/previous-versions/dd183376(v=vs.85)
// https://learn.microsoft.com/en-us/windows/win32/api/wingdi/ns-wingdi-bitmapinfoheader
type bitmapInfoHeader struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

// Contains information about loaded resources
type winTray struct {
	instance,
	icon,
	cursor,
	window Handle

	loadedImages   map[string]Handle
	muLoadedImages sync.RWMutex
	// menus keeps track of the submenus keyed by the menu item ID, plus 0
	// which corresponds to the main popup menu.
	menus   map[uint32]Handle
	muMenus sync.RWMutex
	// menuOf keeps track of the menu each menu item belongs to.
	menuOf   map[uint32]Handle
	muMenuOf sync.RWMutex
	// menuItemIcons maintains the bitmap of each menu item (if applies). It's
	// needed to show the icon correctly when showing a previously hidden menu
	// item again.
	menuItemIcons   map[uint32]Handle
	muMenuItemIcons sync.RWMutex
	visibleItems    map[uint32][]uint32
	muVisibleItems  sync.RWMutex

	nid   *notifyIconData
	muNID sync.RWMutex
	wcex  *wndClassEx

	wmSystrayMessage,
	wmTaskbarCreated uint32
	// wmLoopMessage 自定义消息：RunOnLoop 投递的执行唤醒（在消息循环线程消费队列）。
	wmLoopMessage uint32

	// loopQueue 在消息循环线程串行执行的函数队列（dsh-systray 定制：菜单/弹菜单操作
	// 不得跨线程与 TrackPopupMenu 并发，否则长时间运行后点击无响应）。
	loopMu    sync.Mutex
	loopQueue []func()
	// rebuilding 窗口自愈重建中：WM_DESTROY/WM_ENDSESSION 不再触发退出与 systrayExit。
	rebuilding bool
	// lastIconSrc / lastTooltip 最近一次设置值，供窗口重建（rebuild）后恢复图标与提示。
	lastIconSrc string
	lastTooltip string

	initialized *atomic.Bool

	onClick  func(menu IMenu)
	onDClick func(menu IMenu)
	onRClick func(menu IMenu)
}

// windowAlive 判断托盘隐藏窗口是否仍然有效。
func (t *winTray) windowAlive() bool {
	if t.window == 0 {
		return false
	}
	r, _, _ := pIsWindow.Call(uintptr(t.window))
	return r != 0
}

// isAlive 托盘整体健康状态：已初始化且窗口有效。
func (t *winTray) isAlive() bool {
	return t.isReady() && t.windowAlive()
}

// runOnLoopImpl 在托盘消息循环线程串行执行 fn（若窗口已死则忽略，日志由心跳自愈时输出）。
func runOnLoopImpl(fn func()) {
	if fn == nil {
		return
	}
	if !wt.isReady() || !wt.windowAlive() {
		return // 窗口尚未就绪/已失效：心跳线程会在消息循环内重建
	}
	wt.loopMu.Lock()
	wt.loopQueue = append(wt.loopQueue, fn)
	wt.loopMu.Unlock()
	pPostMessage.Call(uintptr(wt.window), uintptr(wt.wmLoopMessage), 0, 0)
}

// drainLoopQueue 在消息循环线程取出并执行全部排队函数。
func (t *winTray) drainLoopQueue() {
	t.loopMu.Lock()
	q := t.loopQueue
	t.loopQueue = nil
	t.loopMu.Unlock()
	for _, fn := range q {
		fn()
	}
}

// showMenuNow 弹出托盘菜单（须在消息循环线程调用）。
func showMenuNow() {
	if !wt.isReady() {
		return
	}
	_ = wt.ShowMenu()
}

// trayAlive 托盘健康（供 systray.IsAlive 诊断）。
func trayAlive() bool {
	return wt.isAlive()
}

// trayReinit 重建托盘（窗口/图标/菜单重放）。必须由 RunOnLoop 在消息循环线程执行。
func trayReinit() {
	wt.rebuild()
}

// rebuild 销毁失效窗口并重建：重新创建隐藏窗口 + 通知图标，把当前全部菜单项重放回去，
// 再恢复图标与 tooltip。仅在窗口意外死亡（explorer 重启异常、系统回收等）后调用，
// 由 nativeLoop 心跳在消息循环线程检测并触发；销毁旧窗口产生的 WM_DESTROY
// 因 rebuilding 标志被跳过（不触发退出与 systrayExit）。
func (t *winTray) rebuild() {
	t.rebuilding = true
	defer func() { t.rebuilding = false }()
	if t.window != 0 {
		pDestroyWindow.Call(uintptr(t.window))
		t.window = 0
		t.nid = nil
	}
	if t.wcex != nil {
		t.wcex.unregister()
		t.wcex = nil
	}
	// 重置菜单结构（menuItems 包级 map 保留，下面重放）
	t.menus = make(map[uint32]Handle)
	t.menuOf = make(map[uint32]Handle)
	t.visibleItems = make(map[uint32][]uint32)
	t.loadedImages = make(map[string]Handle)
	t.initialized.Store(false)
	if err := t.initInstance(); err != nil {
		log.Printf("systray rebuild: initInstance failed: %s", err)
		return
	}
	if err := t.createMenu(); err != nil {
		log.Printf("systray rebuild: createMenu failed: %s", err)
		return
	}
	t.initialized.Store(true)
	// 重放菜单项（以其当前 disabled/checked 状态插入），可见性由主程序周期刷新校正
	menuItemsLock.Lock()
	ids := make([]uint32, 0, len(menuItems))
	for id := range menuItems {
		ids = append(ids, id)
	}
	menuItemsLock.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		menuItemsLock.Lock()
		item, ok := menuItems[id]
		menuItemsLock.Unlock()
		if ok {
			addOrUpdateMenuItem(item)
		}
	}
	if t.lastIconSrc != "" {
		if err := t.setIcon(t.lastIconSrc); err != nil {
			log.Printf("systray rebuild: restore icon failed: %s", err)
		}
	}
	if t.lastTooltip != "" {
		if err := t.setTooltip(t.lastTooltip); err != nil {
			log.Printf("systray rebuild: restore tooltip failed: %s", err)
		}
	}
	log.Printf("systray: tray window rebuilt (self-heal)")
}

// isReady checks if the tray as already been initialized. It is not goroutine safe with in regard to the initialization function, but prevents a panic when functions are called too early.
func (t *winTray) isReady() bool {
	return t.initialized.Load()
}

// Loads an image from file and shows it in tray.
// Shell_NotifyIcon: https://msdn.microsoft.com/en-us/library/windows/desktop/bb762159(v=vs.85).aspx
func (t *winTray) setIcon(src string) error {
	if !wt.isReady() {
		return ErrTrayNotReadyYet
	}

	const NIF_ICON = 0x00000002

	h, err := t.loadIconFrom(src)
	if err != nil {
		return err
	}

	t.muNID.Lock()
	defer t.muNID.Unlock()
	t.lastIconSrc = src // 窗口重建后恢复
	t.nid.Icon = h
	t.nid.Flags |= NIF_ICON
	t.nid.Size = uint32(unsafe.Sizeof(*t.nid))

	return t.nid.modify()
}

// Sets tooltip on icon.
// Shell_NotifyIcon: https://msdn.microsoft.com/en-us/library/windows/desktop/bb762159(v=vs.85).aspx
func (t *winTray) setTooltip(src string) error {
	if !wt.isReady() {
		return ErrTrayNotReadyYet
	}

	const NIF_TIP = 0x00000004
	b, err := syscall.UTF16FromString(src)
	if err != nil {
		return err
	}

	t.muNID.Lock()
	defer t.muNID.Unlock()
	t.lastTooltip = src // 窗口重建后恢复
	copy(t.nid.Tip[:], b[:])
	t.nid.Flags |= NIF_TIP
	t.nid.Size = uint32(unsafe.Sizeof(*t.nid))

	return t.nid.modify()
}

var wt = winTray{
	initialized: &atomic.Bool{},
}

func (t *winTray) setOnClick(fn func(menu IMenu)) {
	t.onClick = fn
}

func (t *winTray) setOnDClick(fn func(menu IMenu)) {
	t.onDClick = fn
}

func (t *winTray) setOnRClick(fn func(menu IMenu)) {
	t.onRClick = fn
}

// WindowProc callback function that processes messages sent to a window.
// https://msdn.microsoft.com/en-us/library/windows/desktop/ms633573(v=vs.85).aspx
func (t *winTray) wndProc(hWnd Handle, message uint32, wParam, lParam uintptr) (lResult uintptr) {
	const (
		WM_RBUTTONUP     = 0x0205
		WM_LBUTTONUP     = 0x0202
		WM_LBUTTONDBLCLK = 0x0203
		WM_COMMAND       = 0x0111
		WM_ENDSESSION    = 0x0016
		WM_CLOSE         = 0x0010
		WM_DESTROY       = 0x0002
	)
	switch message {
	case WM_COMMAND:
		menuItemId := int32(wParam)
		// https://docs.microsoft.com/en-us/windows/win32/menurc/wm-command#menus
		if menuItemId != -1 {
			systrayMenuItemSelected(uint32(wParam))
		}
	case WM_CLOSE:
		pDestroyWindow.Call(uintptr(t.window))
		t.wcex.unregister()
	case WM_DESTROY:
		if !t.rebuilding {
			// same as WM_ENDSESSION, but throws 0 exit code after all
			defer pPostQuitMessage.Call(uintptr(int32(0)))
		}
		fallthrough
	case WM_ENDSESSION:
		if t.rebuilding {
			break // 自愈重建销毁旧窗口：不触发退出流程
		}
		t.muNID.Lock()
		if t.nid != nil {
			t.nid.delete()
		}
		t.muNID.Unlock()
		systrayExit()
	case t.wmSystrayMessage:
		switch lParam {
		case WM_RBUTTONUP:
			if t.onRClick != nil {
				t.onRClick(t)
			} else {
				t.ShowMenu()
			}
		case WM_LBUTTONUP:
			if t.onClick != nil {
				t.onClick(t)
			}
		case WM_LBUTTONDBLCLK:
			if t.onDClick != nil {
				t.onDClick(t)
			}
		}
	case t.wmLoopMessage:
		// RunOnLoop 投递：在消息循环线程串行执行排队函数（菜单操作必须在此线程）
		t.drainLoopQueue()
	case t.wmTaskbarCreated: // on explorer.exe restarts
		t.muNID.Lock()
		if t.nid != nil {
			t.nid.add()
		}
		t.muNID.Unlock()
	default:
		// Calls the default window procedure to provide default processing for any window messages that an application does not process.
		// https://msdn.microsoft.com/en-us/library/windows/desktop/ms633572(v=vs.85).aspx
		lResult, _, _ = pDefWindowProc.Call(
			uintptr(hWnd),
			uintptr(message),
			uintptr(wParam),
			uintptr(lParam),
		)
	}
	return
}

func (t *winTray) initInstance() error {
	const IDI_APPLICATION = 32512
	const IDC_ARROW = 32512 // Standard arrow
	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms633548(v=vs.85).aspx
	const SW_HIDE = 0
	const CW_USEDEFAULT = 0x80000000
	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms632600(v=vs.85).aspx
	const (
		WS_CAPTION     = 0x00C00000
		WS_MAXIMIZEBOX = 0x00010000
		WS_MINIMIZEBOX = 0x00020000
		WS_OVERLAPPED  = 0x00000000
		WS_SYSMENU     = 0x00080000
		WS_THICKFRAME  = 0x00040000

		WS_OVERLAPPEDWINDOW = WS_OVERLAPPED | WS_CAPTION | WS_SYSMENU | WS_THICKFRAME | WS_MINIMIZEBOX | WS_MAXIMIZEBOX
	)
	// https://msdn.microsoft.com/en-us/library/windows/desktop/ff729176
	const (
		CS_HREDRAW = 0x0002
		CS_VREDRAW = 0x0001
	)
	const NIF_MESSAGE = 0x00000001

	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms644931(v=vs.85).aspx
	const WM_USER = 0x0400

	const (
		className  = "SystrayClass"
		windowName = ""
	)

	t.wmSystrayMessage = WM_USER + 1
	t.wmLoopMessage = WM_USER + 2 // RunOnLoop 唤醒消息（WM_USER 保留段，与图标回调不冲突）
	t.visibleItems = make(map[uint32][]uint32)
	t.menus = make(map[uint32]Handle)
	t.menuOf = make(map[uint32]Handle)
	t.menuItemIcons = make(map[uint32]Handle)

	taskbarEventNamePtr, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms644947
	res, _, err := pRegisterWindowMessage.Call(
		uintptr(unsafe.Pointer(taskbarEventNamePtr)),
	)
	t.wmTaskbarCreated = uint32(res)

	t.loadedImages = make(map[string]Handle)

	instanceHandle, _, err := pGetModuleHandle.Call(0)
	if instanceHandle == 0 {
		return err
	}
	t.instance = Handle(instanceHandle)

	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms648072(v=vs.85).aspx
	iconHandle, _, err := pLoadIcon.Call(0, uintptr(IDI_APPLICATION))
	if iconHandle == 0 {
		return err
	}
	t.icon = Handle(iconHandle)

	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms648391(v=vs.85).aspx
	cursorHandle, _, err := pLoadCursor.Call(0, uintptr(IDC_ARROW))
	if cursorHandle == 0 {
		return err
	}
	t.cursor = Handle(cursorHandle)

	classNamePtr, err := syscall.UTF16PtrFromString(className)
	if err != nil {
		return err
	}

	windowNamePtr, err := syscall.UTF16PtrFromString(windowName)
	if err != nil {
		return err
	}

	t.wcex = &wndClassEx{
		Style:      CS_HREDRAW | CS_VREDRAW,
		WndProc:    syscall.NewCallback(t.wndProc),
		Instance:   t.instance,
		Icon:       t.icon,
		Cursor:     t.cursor,
		Background: Handle(6), // (COLOR_WINDOW + 1)
		ClassName:  classNamePtr,
		IconSm:     t.icon,
	}
	if err := t.wcex.register(); err != nil {
		return err
	}

	windowHandle, _, err := pCreateWindowEx.Call(
		uintptr(0),
		uintptr(unsafe.Pointer(classNamePtr)),
		uintptr(unsafe.Pointer(windowNamePtr)),
		uintptr(WS_OVERLAPPEDWINDOW),
		uintptr(CW_USEDEFAULT),
		uintptr(CW_USEDEFAULT),
		uintptr(CW_USEDEFAULT),
		uintptr(CW_USEDEFAULT),
		uintptr(0),
		uintptr(0),
		uintptr(t.instance),
		uintptr(0),
	)
	if windowHandle == 0 {
		return err
	}
	t.window = Handle(windowHandle)

	pShowWindow.Call(
		uintptr(t.window),
		uintptr(SW_HIDE),
	)

	pUpdateWindow.Call(
		uintptr(t.window),
	)

	t.muNID.Lock()
	defer t.muNID.Unlock()
	t.nid = &notifyIconData{
		Wnd:             Handle(t.window),
		ID:              100,
		Flags:           NIF_MESSAGE,
		CallbackMessage: t.wmSystrayMessage,
	}
	t.nid.Size = uint32(unsafe.Sizeof(*t.nid))

	return t.nid.add()
}

func (t *winTray) createMenu() error {
	const MIM_APPLYTOSUBMENUS = 0x80000000 // Settings apply to the menu and all of its submenus

	menuHandle, _, err := pCreatePopupMenu.Call()
	if menuHandle == 0 {
		return err
	}
	t.menus[0] = Handle(menuHandle)

	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms647575(v=vs.85).aspx
	mi := struct {
		Size, Mask, Style, Max uint32
		Background             Handle
		ContextHelpID          uint32
		MenuData               uintptr
	}{
		Mask: MIM_APPLYTOSUBMENUS,
	}
	mi.Size = uint32(unsafe.Sizeof(mi))

	res, _, err := pSetMenuInfo.Call(
		uintptr(t.menus[0]),
		uintptr(unsafe.Pointer(&mi)),
	)
	if res == 0 {
		return err
	}
	return nil
}

func (t *winTray) convertToSubMenu(menuItemId uint32) (Handle, error) {
	const MIIM_SUBMENU = 0x00000004

	res, _, err := pCreateMenu.Call()
	if res == 0 {
		return 0, err
	}
	menu := Handle(res)

	mi := menuItemInfo{Mask: MIIM_SUBMENU, SubMenu: menu}
	mi.Size = uint32(unsafe.Sizeof(mi))
	t.muMenuOf.RLock()
	hMenu := t.menuOf[menuItemId]
	t.muMenuOf.RUnlock()
	res, _, err = pSetMenuItemInfo.Call(
		uintptr(hMenu),
		uintptr(menuItemId),
		0,
		uintptr(unsafe.Pointer(&mi)),
	)
	if res == 0 {
		return 0, err
	}
	t.muMenus.Lock()
	t.menus[menuItemId] = menu
	t.muMenus.Unlock()
	return menu, nil
}

func (t *winTray) addOrUpdateMenuItem(menuItemId uint32, parentId uint32, title string, disabled, checked bool) error {
	if !wt.isReady() {
		return ErrTrayNotReadyYet
	}

	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms647578(v=vs.85).aspx
	const (
		MIIM_FTYPE   = 0x00000100
		MIIM_BITMAP  = 0x00000080
		MIIM_STRING  = 0x00000040
		MIIM_SUBMENU = 0x00000004
		MIIM_ID      = 0x00000002
		MIIM_STATE   = 0x00000001
	)
	const MFT_STRING = 0x00000000
	const (
		MFS_CHECKED  = 0x00000008
		MFS_DISABLED = 0x00000003
	)
	titlePtr, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return err
	}

	mi := menuItemInfo{
		Mask:     MIIM_FTYPE | MIIM_STRING | MIIM_ID | MIIM_STATE,
		Type:     MFT_STRING,
		ID:       uint32(menuItemId),
		TypeData: titlePtr,
		Cch:      uint32(len(title)),
	}
	mi.Size = uint32(unsafe.Sizeof(mi))
	if disabled {
		mi.State |= MFS_DISABLED
	}
	if checked {
		mi.State |= MFS_CHECKED
	}
	t.muMenuItemIcons.RLock()
	hIcon := t.menuItemIcons[menuItemId]
	t.muMenuItemIcons.RUnlock()
	if hIcon > 0 {
		mi.Mask |= MIIM_BITMAP
		mi.BMPItem = hIcon
	}

	var res uintptr
	t.muMenus.RLock()
	menu, exists := t.menus[parentId]
	t.muMenus.RUnlock()
	if !exists {
		menu, err = t.convertToSubMenu(parentId)
		if err != nil {
			return err
		}
		t.muMenus.Lock()
		t.menus[parentId] = menu
		t.muMenus.Unlock()
	} else if t.getVisibleItemIndex(parentId, menuItemId) != -1 {
		// We set the menu item info based on the menuID
		res, _, err = pSetMenuItemInfo.Call(
			uintptr(menu),
			uintptr(menuItemId),
			0,
			uintptr(unsafe.Pointer(&mi)),
		)
	}

	if res == 0 {
		// Menu item does not already exist, create it
		t.muMenus.RLock()
		submenu, exists := t.menus[menuItemId]
		t.muMenus.RUnlock()
		if exists {
			mi.Mask |= MIIM_SUBMENU
			mi.SubMenu = submenu
		}
		t.addToVisibleItems(parentId, menuItemId)
		position := t.getVisibleItemIndex(parentId, menuItemId)
		res, _, err = pInsertMenuItem.Call(
			uintptr(menu),
			uintptr(position),
			1,
			uintptr(unsafe.Pointer(&mi)),
		)
		if res == 0 {
			t.delFromVisibleItems(parentId, menuItemId)
			return err
		}
		t.muMenuOf.Lock()
		t.menuOf[menuItemId] = menu
		t.muMenuOf.Unlock()
	}

	return nil
}

func (t *winTray) addSeparatorMenuItem(menuItemId, parentId uint32) error {
	if !wt.isReady() {
		return ErrTrayNotReadyYet
	}

	// https://msdn.microsoft.com/en-us/library/windows/desktop/ms647578(v=vs.85).aspx
	const (
		MIIM_FTYPE = 0x00000100
		MIIM_ID    = 0x00000002
		MIIM_STATE = 0x00000001
	)
	const MFT_SEPARATOR = 0x00000800

	mi := menuItemInfo{
		Mask: MIIM_FTYPE | MIIM_ID | MIIM_STATE,
		Type: MFT_SEPARATOR,
		ID:   uint32(menuItemId),
	}

	mi.Size = uint32(unsafe.Sizeof(mi))

	t.addToVisibleItems(parentId, menuItemId)
	position := t.getVisibleItemIndex(parentId, menuItemId)
	t.muMenus.RLock()
	menu := uintptr(t.menus[parentId])
	t.muMenus.RUnlock()
	res, _, err := pInsertMenuItem.Call(
		menu,
		uintptr(position),
		1,
		uintptr(unsafe.Pointer(&mi)),
	)
	if res == 0 {
		return err
	}

	return nil
}

func (t *winTray) hideMenuItem(menuItemId, parentId uint32) error {
	if !wt.isReady() {
		return ErrTrayNotReadyYet
	}

	const MF_BYCOMMAND = 0x00000000
	const ERROR_SUCCESS syscall.Errno = 0

	t.muMenus.RLock()
	menu := uintptr(t.menus[parentId])
	t.muMenus.RUnlock()
	res, _, err := pRemoveMenu.Call(
		menu,
		uintptr(menuItemId),
		MF_BYCOMMAND,
	)
	if res == 0 && err.(syscall.Errno) != ERROR_SUCCESS {
		return err
	}
	t.delFromVisibleItems(parentId, menuItemId)

	return nil
}

func (t *winTray) ShowMenu() error {
	if !wt.isReady() {
		return ErrTrayNotReadyYet
	}

	const (
		TPM_BOTTOMALIGN = 0x0020
		TPM_LEFTALIGN   = 0x0000
	)
	p := point{}
	res, _, err := pGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	if res == 0 {
		return err
	}
	pSetForegroundWindow.Call(uintptr(t.window))

	res, _, err = pTrackPopupMenu.Call(
		uintptr(t.menus[0]),
		TPM_BOTTOMALIGN|TPM_LEFTALIGN,
		uintptr(p.X),
		uintptr(p.Y),
		0,
		uintptr(t.window),
		0,
	)
	if res == 0 {
		return err
	}

	return nil
}

func (t *winTray) delFromVisibleItems(parent, val uint32) {
	t.muVisibleItems.Lock()
	defer t.muVisibleItems.Unlock()
	visibleItems := t.visibleItems[parent]
	for i, itemval := range visibleItems {
		if val == itemval {
			t.visibleItems[parent] = append(visibleItems[:i], visibleItems[i+1:]...)
			break
		}
	}
}

func (t *winTray) addToVisibleItems(parent, val uint32) {
	t.muVisibleItems.Lock()
	defer t.muVisibleItems.Unlock()
	if visibleItems, exists := t.visibleItems[parent]; !exists {
		t.visibleItems[parent] = []uint32{val}
	} else {
		newvisible := append(visibleItems, val)
		sort.Slice(newvisible, func(i, j int) bool { return newvisible[i] < newvisible[j] })
		t.visibleItems[parent] = newvisible
	}
}

func (t *winTray) getVisibleItemIndex(parent, val uint32) int {
	t.muVisibleItems.RLock()
	defer t.muVisibleItems.RUnlock()
	for i, itemval := range t.visibleItems[parent] {
		if val == itemval {
			return i
		}
	}
	return -1
}

// Loads an image from file to be shown in tray or menu item.
// LoadImage: https://msdn.microsoft.com/en-us/library/windows/desktop/ms648045(v=vs.85).aspx
func (t *winTray) loadIconFrom(src string) (Handle, error) {
	if !wt.isReady() {
		return 0, ErrTrayNotReadyYet
	}

	const IMAGE_ICON = 1               // Loads an icon
	const LR_LOADFROMFILE = 0x00000010 // Loads the stand-alone image from the file
	const LR_DEFAULTSIZE = 0x00000040  // Loads default-size icon for windows(SM_CXICON x SM_CYICON) if cx, cy are set to zero

	// Save and reuse handles of loaded images
	t.muLoadedImages.RLock()
	h, ok := t.loadedImages[src]
	t.muLoadedImages.RUnlock()
	if !ok {
		srcPtr, err := syscall.UTF16PtrFromString(src)
		if err != nil {
			return 0, err
		}
		res, _, err := pLoadImage.Call(
			0,
			uintptr(unsafe.Pointer(srcPtr)),
			IMAGE_ICON,
			0,
			0,
			LR_LOADFROMFILE|LR_DEFAULTSIZE,
		)
		if res == 0 {
			return 0, err
		}
		h = Handle(res)
		t.muLoadedImages.Lock()
		t.loadedImages[src] = h
		t.muLoadedImages.Unlock()
	}
	return h, nil
}

func iconToBitmap(hIcon Handle) (Handle, error) {
	const SM_CXSMICON = 49
	const SM_CYSMICON = 50
	const DI_NORMAL = 0x3
	hDC, _, err := pGetDC.Call(uintptr(0))
	if hDC == 0 {
		return 0, err
	}
	defer pReleaseDC.Call(uintptr(0), hDC)
	hMemDC, _, err := pCreateCompatibleDC.Call(hDC)
	if hMemDC == 0 {
		return 0, err
	}
	defer pDeleteDC.Call(hMemDC)
	cx, _, _ := pGetSystemMetrics.Call(SM_CXSMICON)
	cy, _, _ := pGetSystemMetrics.Call(SM_CYSMICON)
	hMemBmp, err := create32BitHBitmap(hMemDC, int32(cx), int32(cy))
	hOriginalBmp, _, _ := pSelectObject.Call(hMemDC, hMemBmp)
	defer pSelectObject.Call(hMemDC, hOriginalBmp)
	res, _, err := pDrawIconEx.Call(hMemDC, 0, 0, uintptr(hIcon), cx, cy, 0, uintptr(0), DI_NORMAL)
	if res == 0 {
		return 0, err
	}
	return Handle(hMemBmp), nil
}

// https://learn.microsoft.com/en-us/windows/win32/api/wingdi/nf-wingdi-createdibsection
func create32BitHBitmap(hDC uintptr, cx, cy int32) (uintptr, error) {
	const BI_RGB uint32 = 0
	const DIB_RGB_COLORS = 0
	bmi := bitmapInfo{
		BmiHeader: bitmapInfoHeader{
			BiPlanes:      1,
			BiCompression: BI_RGB,
			BiWidth:       cx,
			BiHeight:      cy,
			BiBitCount:    32,
		},
	}
	bmi.BmiHeader.BiSize = uint32(unsafe.Sizeof(bmi.BmiHeader))
	var bits uintptr
	hBitmap, _, err := pCreateDIBSection.Call(
		hDC,
		uintptr(unsafe.Pointer(&bmi)),
		DIB_RGB_COLORS,
		uintptr(unsafe.Pointer(&bits)),
		uintptr(0),
		0,
	)
	if hBitmap == 0 {
		return 0, err
	}
	return hBitmap, nil
}

func registerSystray() {
	if err := wt.initInstance(); err != nil {
		log.Printf("systray error: unable to init instance: %s\n", err)
		return
	}

	if err := wt.createMenu(); err != nil {
		log.Printf("systray error: unable to create menu: %s\n", err)
		return
	}

	wt.initialized.Store(true)
	if systrayReady != nil {
		systrayReady()
	}
}

// msg 与 Windows MSG 结构一致（64 位：HWND+UINT+对齐+WPARAM+LPARAM+DWORD+POINT）。
type msg struct {
	hwnd    uintptr
	message uint32
	_       uint32 // 对齐填充（32 位下为 0，64 位下补齐 WPARAM 前对齐）
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

// nativeLoop 托盘消息循环（dsh-systray 定制版）：PeekMessage 非阻塞泵消息 +
// 空闲心跳自愈——托盘隐藏窗口若被系统意外销毁（explorer 异常重启等），
// 在本线程检测并自动 rebuild，避免「运行一段时间后点击无响应」。
func nativeLoop() {
	const (
		wmQuit    = 0x0012
		pmRemove  = 0x0001
		idleSleep = 30 * time.Millisecond
	)
	for {
		var m msg
		if ret, _, _ := pPeekMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, pmRemove); ret != 0 {
			if m.message == wmQuit {
				return // 正常退出（托盘 Quit → WM_CLOSE → WM_DESTROY → PostQuitMessage）
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
			continue
		}
		// 空闲心跳：窗口意外死亡 → 在消息循环线程自愈重建
		if wt.isReady() && !wt.windowAlive() {
			log.Printf("systray: tray window no longer valid, rebuilding…")
			wt.rebuild()
		}
		time.Sleep(idleSleep)
	}
}

func nativeEnd() {
}

func nativeStart() {
	go nativeLoop()
}

func quit() {
	const WM_CLOSE = 0x0010

	pPostMessage.Call(
		uintptr(wt.window),
		WM_CLOSE,
		0,
		0,
	)
}

func setInternalLoop(bool) {
}

func iconBytesToFilePath(iconBytes []byte) (string, error) {
	bh := md5.Sum(iconBytes)
	dataHash := hex.EncodeToString(bh[:])
	iconFilePath := filepath.Join(os.TempDir(), "systray_temp_icon_"+dataHash)

	if _, err := os.Stat(iconFilePath); os.IsNotExist(err) {
		if err := ioutil.WriteFile(iconFilePath, iconBytes, 0644); err != nil {
			return "", err
		}
	}
	return iconFilePath, nil
}

// SetIcon sets the systray icon.
// iconBytes should be the content of .ico for windows and .ico/.jpg/.png
// for other platforms.
func SetIcon(iconBytes []byte) {
	iconFilePath, err := iconBytesToFilePath(iconBytes)
	if err != nil {
		log.Printf("systray error: unable to write icon data to temp file: %s\n", err)
		return
	}
	if err := wt.setIcon(iconFilePath); err != nil {
		log.Printf("systray error: unable to set icon: %s\n", err)
		return
	}
}

// SetTemplateIcon sets the systray icon as a template icon (on macOS), falling back
// to a regular icon on other platforms.
// templateIconBytes and iconBytes should be the content of .ico for windows and
// .ico/.jpg/.png for other platforms.
func SetTemplateIcon(templateIconBytes []byte, regularIconBytes []byte) {
	SetIcon(regularIconBytes)
}

// SetTitle sets the systray title, only available on Mac and Linux.
func SetTitle(title string) {
	// do nothing
}

func (item *MenuItem) parentId() uint32 {
	if item.parent != nil {
		return uint32(item.parent.id)
	}
	return 0
}

// SetIcon sets the icon of a menu item. Only works on macOS and Windows.
// iconBytes should be the content of .ico/.jpg/.png
func (item *MenuItem) SetIcon(iconBytes []byte) {
	iconFilePath, err := iconBytesToFilePath(iconBytes)
	if err != nil {
		log.Printf("systray error: unable to write icon data to temp file: %s\n", err)
		return
	}

	h, err := wt.loadIconFrom(iconFilePath)
	if err != nil {
		log.Printf("systray error: unable to load icon from temp file: %s\n", err)
		return
	}

	h, err = iconToBitmap(h)
	if err != nil {
		log.Printf("systray error: unable to convert icon to bitmap: %s\n", err)
		return
	}
	wt.muMenuItemIcons.Lock()
	wt.menuItemIcons[uint32(item.id)] = h
	wt.muMenuItemIcons.Unlock()

	err = wt.addOrUpdateMenuItem(uint32(item.id), item.parentId(), item.title, item.disabled, item.checked)
	if err != nil {
		log.Printf("systray error: unable to addOrUpdateMenuItem: %s\n", err)
		return
	}
}

// SetTooltip sets the systray tooltip to display on mouse hover of the tray icon,
// only available on Mac and Windows.
func SetTooltip(tooltip string) {
	if err := wt.setTooltip(tooltip); err != nil {
		log.Printf("systray error: unable to set tooltip: %s\n", err)
		return
	}
}

func addOrUpdateMenuItem(item *MenuItem) {
	err := wt.addOrUpdateMenuItem(uint32(item.id), item.parentId(), item.title, item.disabled, item.checked)
	if err != nil {
		log.Printf("systray error: unable to addOrUpdateMenuItem: %s\n", err)
		return
	}
}

func setOnClick(fn func(menu IMenu)) {
	wt.setOnClick(fn)
}

func setOnDClick(fn func(menu IMenu)) {
	wt.setOnDClick(fn)
}

func setOnRClick(fn func(menu IMenu)) {
	wt.setOnRClick(fn)
}

// SetTemplateIcon sets the icon of a menu item as a template icon (on macOS). On Windows, it
// falls back to the regular icon bytes and on Linux it does nothing.
// templateIconBytes and regularIconBytes should be the content of .ico for windows and
// .ico/.jpg/.png for other platforms.
func (item *MenuItem) SetTemplateIcon(templateIconBytes []byte, regularIconBytes []byte) {
	item.SetIcon(regularIconBytes)
}

func addSeparator(id uint32) {
	err := wt.addSeparatorMenuItem(id, 0)
	if err != nil {
		log.Printf("systray error: unable to addSeparator: %s\n", err)
		return
	}
}

func hideMenuItem(item *MenuItem) {
	err := wt.hideMenuItem(uint32(item.id), item.parentId())
	if err != nil {
		log.Printf("systray error: unable to hideMenuItem: %s\n", err)
		return
	}
}

func showMenuItem(item *MenuItem) {
	addOrUpdateMenuItem(item)
}

func resetMenu() {
	wt.createMenu()
}
