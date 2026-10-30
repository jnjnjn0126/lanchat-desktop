// 窗口外观与系统集成：无边框窗口、圆角、托盘、开机自启、窗口控制接口
//
// 这里的做法和 QQ 一样：不要系统的标题栏，拖动和关闭按钮都由网页自己画，
// 通过 /api/window 回传动作。
//
// 两种窗口模式（由页面切换时通知）：
//
//	home —— 主页：固定尺寸的小卡片，就是一个「登录框」，不能拉伸、不能最大化
//	chat —— 聊天页：标题和按钮都长在顶部拖拽条里，可自由拉伸、可最大化
package main

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

/* ===================== Win32 常量 ===================== */

const (
	wsPopup        = 0x80000000
	wsVisible      = 0x10000000
	wsClipChildren = 0x02000000
	wsMinimizeBox  = 0x00020000
	wsMaximizeBox  = 0x00010000
	wsThickFrame   = 0x00040000

	wsExAppWindow = 0x00040000

	swHide     = 0
	swShow     = 5
	swMinimize = 6
	swRestore  = 9

	swpNoZOrder     = 0x0004
	swpNoActivate   = 0x0010
	swpFrameChanged = 0x0020
	swpShowWindow   = 0x0040

	wmDestroy       = 0x0002
	wmSize          = 0x0005
	wmClose         = 0x0010
	wmSetIcon       = 0x0080
	wmNCLButtonDown = 0x00A1
	wmApp           = 0x8000
	wmGetMinMaxInfo = 0x0024
	wmNCHitTest     = 0x0084

	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205

	// 非客户区命中测试的返回值
	htClient      = 1
	htCaption     = 2
	htLeft        = 10
	htRight       = 11
	htTop         = 12
	htTopLeft     = 13
	htTopRight    = 14
	htBottom      = 15
	htBottomLeft  = 16
	htBottomRight = 17

	spiGetWorkArea = 0x0030

	dwmwaWindowCornerPreference = 33
	dwmwaBorderColor            = 34
	dwmwcpRound                 = 2

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	niifInfo = 0x00000001

	mfString    = 0x00000000
	mfSeparator = 0x00000800
	mfDefault   = 0x00001000

	tpmRightButton = 0x0002
	tpmNoNotify    = 0x0080
	tpmReturnCmd   = 0x0100

	trayMsg = wmApp + 1 // 托盘图标回调消息
	wmDrag  = wmApp + 5 // 请求拖动窗口（在 UI 线程执行）

	/* 窗口尺寸（单位：物理像素，程序没开 DPI 缩放感知时就是逻辑像素）
	   v2.8 起窗口只有一种形态：可拉伸的聊天窗口（主页和对话框窗口都删了） */
	chatWinW, chatWinH = 1040, 720 // 默认大小
	chatMinW, chatMinH = 780, 520  // 聊天页能拖到的最小尺寸
	resizeGrip         = 7         // 聊天页四周可拉伸的宽度
)

// 非客户区尺寸限制（WM_GETMINMAXINFO 的 MINMAXINFO）
type minMaxInfo struct {
	Reserved     point
	MaxSize      point
	MaxPosition  point
	MinTrackSize point
	MaxTrackSize point
}

var (
	// GetWindowLongPtr 的索引是负数，用变量避免常量溢出检查
	gwlStyle    = -16
	gwlExStyle  = -20
	gwlpWndProc = -4

	u32   = windows.NewLazySystemDLL("user32.dll")
	g32   = windows.NewLazySystemDLL("gdi32.dll")
	sh32l = windows.NewLazySystemDLL("shell32.dll")
	k32   = windows.NewLazySystemDLL("kernel32.dll")
	dwm32 = windows.NewLazySystemDLL("dwmapi.dll")

	pGetWindowLongPtr  = u32.NewProc("GetWindowLongPtrW")
	pSetWindowLongPtr  = u32.NewProc("SetWindowLongPtrW")
	pCallWindowProc    = u32.NewProc("CallWindowProcW")
	pDefWindowProc     = u32.NewProc("DefWindowProcW")
	pShowWindow        = u32.NewProc("ShowWindow")
	pIsWindowVisible   = u32.NewProc("IsWindowVisible")
	pSetWindowPos      = u32.NewProc("SetWindowPos")
	pSetForeground     = u32.NewProc("SetForegroundWindow")
	pGetCursorPos      = u32.NewProc("GetCursorPos")
	pCreateWindowEx    = u32.NewProc("CreateWindowExW")
	pDestroyWindow     = u32.NewProc("DestroyWindow")
	pPostMessage       = u32.NewProc("PostMessageW")
	pSendMessage       = u32.NewProc("SendMessageW")
	pReleaseCapture    = u32.NewProc("ReleaseCapture")
	pCreatePopupMenu   = u32.NewProc("CreatePopupMenu")
	pAppendMenu        = u32.NewProc("AppendMenuW")
	pTrackPopupMenu    = u32.NewProc("TrackPopupMenu")
	pDestroyMenu       = u32.NewProc("DestroyMenu")
	pSystemParameters  = u32.NewProc("SystemParametersInfoW")
	pCreateIconFromRes = u32.NewProc("CreateIconFromResourceEx")
	pLoadImage         = u32.NewProc("LoadImageW")
	pSetWindowRgn      = u32.NewProc("SetWindowRgn")
	pRegisterClassEx   = u32.NewProc("RegisterClassExW")
	pGetSysColorBrush  = u32.NewProc("GetSysColorBrush")
	pGetClientRect     = u32.NewProc("GetClientRect")
	pGetWindowRect     = u32.NewProc("GetWindowRect")
	pInvalidateRect    = u32.NewProc("InvalidateRect")

	pCreateRoundRectRgn = g32.NewProc("CreateRoundRectRgn")
	pShellNotifyIcon    = sh32l.NewProc("Shell_NotifyIconW")
	pGetModuleHandle    = k32.NewProc("GetModuleHandleW")
	pDwmSetAttribute    = dwm32.NewProc("DwmSetWindowAttribute")
	pDwmGetAttribute    = dwm32.NewProc("DwmGetWindowAttribute")

	pGetMessage       = u32.NewProc("GetMessageW")
	pTranslateMessage = u32.NewProc("TranslateMessage")
	pDispatchMessage  = u32.NewProc("DispatchMessageW")

	mapWndProc    = windows.NewCallback(mainWndProc)
	trayWndProc   = windows.NewCallback(trayProc)
	testWndProcCb = windows.NewCallback(testWndProc)
)

type rect struct{ Left, Top, Right, Bottom int32 }
type point struct{ X, Y int32 }

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

// NOTIFYICONDATAW（x64 下完整结构 976 字节）
type notifyIconData struct {
	CbSize           uint32
	HWnd             windows.Handle
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            windows.Handle
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UTimeoutVersion  uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     windows.Handle
}

var (
	trayHwnd     uintptr
	trayIcon     windows.Handle
	trayOK       bool
	origTrayProc uintptr
	origMainProc uintptr
	mainHookOn   bool

	dragging    bool
	menuOpening bool
	roundedOK   bool

	/* 窗口模式状态：home / chat；是否最大化；最大化前的窗口位置 */
	winMu          sync.Mutex
	winMode        = "chat"
	winMax         bool
	restoreRect    rect
	regionFallback bool // Win11 的 DWM 圆角用不了，退化成区域裁剪
)

/* ===================== 小工具 ===================== */

func utf16z(s string, max int) []uint16 {
	u, err := windows.UTF16FromString(s)
	if err != nil || len(u) == 0 {
		u = []uint16{0}
	}
	if max > 0 && len(u) > max {
		u = u[:max]
		u[max-1] = 0
	}
	return u
}

func exePath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}

/* ===================== 窗口外观 ===================== */

func workArea() rect {
	var wa rect
	pSystemParameters.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&wa)), 0)
	if wa.Right-wa.Left <= 200 || wa.Bottom-wa.Top <= 200 {
		wa = rect{0, 0, 1280, 800}
	}
	return wa
}

// 按模式算窗口大小，并夹紧到当前工作区（小屏笔记本也不会超出屏幕）
func modeSize(mode string) (int, int) {
	wa := workArea()
	aw, ah := int(wa.Right-wa.Left), int(wa.Bottom-wa.Top)
	w, h := chatWinW, chatWinH
	switch mode {
	default:
		w, h = chatWinW, chatWinH
	}
	if w > aw-40 {
		w = aw - 40
	}
	if h > ah-40 {
		h = ah - 40
	}
	return w, h
}

func modeNow() string {
	winMu.Lock()
	defer winMu.Unlock()
	return winMode
}

func maxNow() bool {
	winMu.Lock()
	defer winMu.Unlock()
	return winMax
}

func setModeState(mode string) {
	winMu.Lock()
	winMode = mode
	winMax = false
	winMu.Unlock()
}

// 居中放到工作区上（两种模式都用它换尺寸）
func placeCentered(h uintptr, mode string) (int, int) {
	w, hh := modeSize(mode)
	wa := workArea()
	aw, ah := int(wa.Right-wa.Left), int(wa.Bottom-wa.Top)
	x := int(wa.Left) + (aw-w)/2
	y := int(wa.Top) + (ah-hh)/2
	pSetWindowPos.Call(h, 0, uintptr(x), uintptr(y), uintptr(w), uintptr(hh),
		swpFrameChanged|swpNoZOrder|swpNoActivate|swpShowWindow)
	return w, hh
}

// 圆角：Win11 直接让 DWM 画（自带抗锯齿），Win10 退回窗口区域裁剪。
// 返回 true 表示走了 DWM。
func applyCorners(h uintptr, w, hh int) bool {
	// 窗口边框颜色跟页面边框对齐（#e4e8ee），聊天页带系统拉伸边框时看不出突兀
	border := uint32(0x00EEE8E4) // COLORREF = 0x00BBGGRR
	pDwmSetAttribute.Call(h, uintptr(dwmwaBorderColor),
		uintptr(unsafe.Pointer(&border)), unsafe.Sizeof(border))

	pref := uint32(dwmwcpRound)
	r, _, _ := pDwmSetAttribute.Call(h,
		uintptr(dwmwaWindowCornerPreference),
		uintptr(unsafe.Pointer(&pref)),
		unsafe.Sizeof(pref))
	if r == 0 {
		regionFallback = false
		return true
	}
	regionFallback = true
	updateRegion(h, w, hh)
	return false
}

// Win10 退化方案：给窗口套一个圆角区域。最大化时取消裁剪（直角）。
func updateRegion(h uintptr, w, hh int) {
	if !regionFallback || h == 0 {
		return
	}
	if maxNow() {
		pSetWindowRgn.Call(h, 0, 1) // 0 = 取消区域
		return
	}
	if w <= 0 || hh <= 0 {
		var rc rect
		pGetWindowRect.Call(h, uintptr(unsafe.Pointer(&rc)))
		w, hh = int(rc.Right-rc.Left), int(rc.Bottom-rc.Top)
	}
	rgn, _, _ := pCreateRoundRectRgn.Call(0, 0, uintptr(w+1), uintptr(hh+1), 20, 20)
	if rgn != 0 {
		pSetWindowRgn.Call(h, rgn, 1)
	}
}

// 窗口样式：主页是纯弹出式小卡片（不可拉伸）；聊天页加 WS_THICKFRAME，
// 系统自带那圈看不见的拉伸边框（网页那层子窗口盖不到它，拉边才一定有效）。
func styleForMode(mode string) uintptr {
	s := uintptr(wsPopup | wsVisible | wsClipChildren | wsMaximizeBox | wsMinimizeBox | wsThickFrame)
	return s
}

// 无边框窗口：去掉标题栏，按模式定尺寸、居中、加圆角。
func applyFrameless(h uintptr, mode string) bool {
	if h == 0 {
		return false
	}
	// 去掉标题栏/边框，改成弹出式窗口（最大化/最小化能力保留，方便任务栏行为）
	pSetWindowLongPtr.Call(h, uintptr(gwlStyle), styleForMode(mode))
	ex, _, _ := pGetWindowLongPtr.Call(h, uintptr(gwlExStyle))
	pSetWindowLongPtr.Call(h, uintptr(gwlExStyle), ex|wsExAppWindow)

	setModeState(mode)
	w, hh := placeCentered(h, mode)
	return applyCorners(h, w, hh)
}

// 客户区比窗口小多少（有 WS_THICKFRAME 时边框吃掉的那几像素）
func clientInset(h uintptr) int {
	var wr, cr rect
	pGetWindowRect.Call(h, uintptr(unsafe.Pointer(&wr)))
	pGetClientRect.Call(h, uintptr(unsafe.Pointer(&cr)))
	d := int(wr.Right-wr.Left) - int(cr.Right-cr.Left)
	if d < 0 {
		d = 0
	}
	return d
}

// 最大化 / 还原（自己算，不依赖窗口样式，主页模式不允许）
func toggleMaximize(h uintptr) bool {
	if h == 0 || modeNow() != "chat" {
		return false
	}
	if maxNow() {
		winMu.Lock()
		r := restoreRect
		winMax = false
		winMu.Unlock()
		if r.Right-r.Left < chatMinW {
			r = rect{0, 0, int32(chatWinW), int32(chatWinH)}
		}
		pSetWindowPos.Call(h, 0, uintptr(r.Left), uintptr(r.Top),
			uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top),
			swpNoZOrder|swpShowWindow|swpFrameChanged)
		updateRegion(h, int(r.Right-r.Left), int(r.Bottom-r.Top))
		debugf("window: 还原 %dx%d", r.Right-r.Left, r.Bottom-r.Top)
		return false
	}
	var rc rect
	pGetWindowRect.Call(h, uintptr(unsafe.Pointer(&rc)))
	wa := workArea()
	winMu.Lock()
	restoreRect = rc
	winMax = true
	winMu.Unlock()
	pSetWindowPos.Call(h, 0, uintptr(wa.Left), uintptr(wa.Top),
		uintptr(wa.Right-wa.Left), uintptr(wa.Bottom-wa.Top),
		swpNoZOrder|swpShowWindow|swpFrameChanged)
	updateRegion(h, 0, 0)
	debugf("window: 最大化 %dx%d", wa.Right-wa.Left, wa.Bottom-wa.Top)
	return true
}

// 聊天页四周 7 像素内返回拉伸用的命中码；主页、最大化时返回 0（不拦）
func resizeHit(h uintptr, lp uintptr) uintptr {
	if h == 0 || modeNow() != "chat" || maxNow() {
		return 0
	}
	var rc rect
	pGetWindowRect.Call(h, uintptr(unsafe.Pointer(&rc)))
	x := int32(int16(lp & 0xffff))
	y := int32(int16((lp >> 16) & 0xffff))
	l := x >= rc.Left-resizeGrip && x < rc.Left+resizeGrip
	r := x > rc.Right-resizeGrip && x <= rc.Right+resizeGrip
	t := y >= rc.Top-resizeGrip && y < rc.Top+resizeGrip
	b := y > rc.Bottom-resizeGrip && y <= rc.Bottom+resizeGrip
	switch {
	case l && t:
		return htTopLeft
	case r && t:
		return htTopRight
	case l && b:
		return htBottomLeft
	case r && b:
		return htBottomRight
	case l:
		return htLeft
	case r:
		return htRight
	case t:
		return htTop
	case b:
		return htBottom
	}
	return 0
}

// 聊天页的最小尺寸 + 最大化不超过工作区
func applyMinMax(lp uintptr) {
	if lp == 0 || modeNow() != "chat" {
		return
	}
	mi := (*minMaxInfo)(unsafe.Pointer(lp)) // lp 是系统传进来的栈上结构，直接转指针是安全的
	wa := workArea()
	mi.MinTrackSize.X = chatMinW
	mi.MinTrackSize.Y = chatMinH
	mi.MaxSize.X = wa.Right - wa.Left
	mi.MaxSize.Y = wa.Bottom - wa.Top
	mi.MaxPosition.X = 0
	mi.MaxPosition.Y = 0
}

// 给窗口设置任务栏图标（大图标给任务栏/Alt+Tab，小图标给窗口左上角）
func setWindowIcon(h uintptr, ico windows.Handle) {
	if h == 0 || ico == 0 {
		return
	}
	pSendMessage.Call(h, wmSetIcon, 1, uintptr(ico)) // ICON_BIG
	small := appIconFromRes(16)
	if small == 0 {
		small = ico
	}
	pSendMessage.Call(h, wmSetIcon, 0, uintptr(small)) // ICON_SMALL
}

/* ===================== 主窗口钩子（拦 WM_CLOSE / 拖动） ===================== */

func installMainHook(h uintptr) {
	if h == 0 {
		return
	}
	old, _, _ := pSetWindowLongPtr.Call(h, uintptr(gwlpWndProc), mapWndProc)
	origMainProc = old
	mainHookOn = old != 0
}

func mainWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmClose:
		// 点 ✕ / Alt+F4：按设置决定「收进托盘」还是「退出」
		doCloseAction()
		return 0
	case wmDrag:
		if !dragging {
			dragging = true
			pReleaseCapture.Call()
			pSendMessage.Call(hwnd, wmNCLButtonDown, htCaption, 0)
			dragging = false
		}
		return 0
	case wmNCHitTest:
		// 聊天页：窗口没有系统边框，四周的拉伸感觉得自己造
		if code := resizeHit(hwnd, lp); code != 0 {
			return code
		}
	case wmGetMinMaxInfo:
		applyMinMax(lp)
	case wmSize:
		// 拉伸后重画圆角区域（Win10 的退化方案才需要）
		if regionFallback {
			updateRegion(hwnd, 0, 0)
		}
	}
	if origMainProc != 0 {
		r, _, _ := pCallWindowProc.Call(origMainProc, hwnd, msg, wp, lp)
		return r
	}
	r, _, _ := pDefWindowProc.Call(hwnd, msg, wp, lp)
	return r
}

/* ===================== 窗口显示 / 隐藏 / 退出 ===================== */

func showMain() {
	h := getOwner()
	if h == 0 {
		return
	}
	pShowWindow.Call(h, swShow)
	pShowWindow.Call(h, swRestore)
	pSetForeground.Call(h)
}

func hideToTray() {
	h := getOwner()
	if h == 0 {
		return
	}
	pShowWindow.Call(h, swHide)
	trayBalloon("Lanchat 仍在后台运行", "双击托盘图标可以重新打开窗口，右键有退出菜单。")
}

// 开机自启时用：安静地收进托盘，不弹气泡
func hideQuiet() {
	if h := getOwner(); h != 0 {
		pShowWindow.Call(h, swHide)
	}
}

func doCloseAction() {
	if closeActionName() == "tray" && trayOK {
		hideToTray()
		return
	}
	quitApp()
}

func quitApp() {
	removeTray()
	os.Exit(0)
}

/* ===================== 托盘图标 ===================== */

func createTray(h uintptr) {
	hInst, _, _ := pGetModuleHandle.Call(0)
	cls := utf16z("STATIC", 0)
	th, _, _ := pCreateWindowEx.Call(0,
		uintptr(unsafe.Pointer(&cls[0])),
		uintptr(unsafe.Pointer(&cls[0])),
		wsPopup, 0, 0, 0, 0, 0, 0, hInst, 0)
	if th == 0 {
		return
	}
	trayHwnd = th
	old, _, _ := pSetWindowLongPtr.Call(th, uintptr(gwlpWndProc), trayWndProc)
	origTrayProc = old

	if trayIcon == 0 {
		trayIcon = makeAppIcon(32)
	}
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = windows.Handle(th)
	nid.UID = 1
	nid.UFlags = nifMessage | nifIcon | nifTip
	nid.UCallbackMessage = trayMsg
	nid.HIcon = trayIcon
	copy(nid.SzTip[:], utf16z("Lanchat v"+appVersion, len(nid.SzTip)))

	r, _, _ := pShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	trayOK = r != 0
	debugf("tray: hwnd=%#x icon=%v ok=%v", th, trayIcon, trayOK)
}

func trayBalloon(title, msg string) {
	if !trayOK || trayHwnd == 0 {
		return
	}
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = windows.Handle(trayHwnd)
	nid.UID = 1
	nid.UFlags = nifInfo
	copy(nid.SzInfoTitle[:], utf16z(title, len(nid.SzInfoTitle)))
	copy(nid.SzInfo[:], utf16z(msg, len(nid.SzInfo)))
	nid.DwInfoFlags = niifInfo
	pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

func removeTray() {
	if trayHwnd == 0 {
		return
	}
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = windows.Handle(trayHwnd)
	nid.UID = 1
	pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	trayOK = false
}

func trayProc(hwnd, msg, wp, lp uintptr) uintptr {
	if msg == trayMsg {
		switch uintptr(uint16(lp)) { // 低 16 位是鼠标消息
		case wmLButtonUp, wmLButtonDblClk:
			showMain()
			return 0
		case wmRButtonUp:
			if !menuOpening {
				menuOpening = true
				trayMenu(hwnd)
				menuOpening = false
			}
			return 0
		}
		return 0
	}
	if origTrayProc != 0 {
		r, _, _ := pCallWindowProc.Call(origTrayProc, hwnd, msg, wp, lp)
		return r
	}
	r, _, _ := pDefWindowProc.Call(hwnd, msg, wp, lp)
	return r
}

func trayMenu(hwnd uintptr) {
	menu, _, _ := pCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer pDestroyMenu.Call(menu)

	addItem := func(id uintptr, text string, def bool) {
		flags := uintptr(mfString)
		if def {
			flags |= mfDefault
		}
		t := utf16z(text, 0)
		pAppendMenu.Call(menu, flags, id, uintptr(unsafe.Pointer(&t[0])))
	}
	addItem(1, "打开主界面", true)
	addItem(2, "打开共享文件夹", false)
	pAppendMenu.Call(menu, mfSeparator, 0, 0)
	addItem(3, "退出", false)

	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForeground.Call(hwnd)
	// TrackPopupMenu 会阻塞到菜单关闭，返回被点中的 id
	r, _, _ := pTrackPopupMenu.Call(menu,
		tpmReturnCmd|tpmRightButton|tpmNoNotify,
		uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
	pPostMessage.Call(hwnd, 0, 0, 0) // 让菜单正常消失（WM_NULL）

	switch r {
	case 1:
		showMain()
	case 2:
		if root := rootDir(); root != "" {
			_ = execExplore(root)
		} else {
			showMain()
		}
	case 3:
		quitApp()
	}
}

func execExplore(path string) error {
	return startDetached("explorer.exe", path)
}

/* ===================== 托盘 / 窗口图标 =====================
   图标资源（rsrc_windows_amd64.syso）里塞的就是主页那个 logo：蓝色圆角方块 + 白色气泡。
   运行时按尺寸直接从 exe 里读，任务栏、托盘、Alt+Tab 和资源管理器里的文件图标才是一套。
   万一带不上 syso（换了构建方式），再退回下面的代码画法。 */

const (
	resAppIcon = 1 // rsrc 生成的图标组 ID 从 1 开始
	imageIcon  = 1 // IMAGE_ICON
)

// 从 exe 资源里按尺寸取图标；取不到返回 0
func appIconFromRes(size int) windows.Handle {
	hInst, _, _ := pGetModuleHandle.Call(0)
	if hInst == 0 {
		return 0
	}
	r, _, _ := pLoadImage.Call(hInst, resAppIcon, imageIcon, uintptr(size), uintptr(size), 0)
	return windows.Handle(r)
}

func makeAppIcon(size int) windows.Handle {
	if h := appIconFromRes(size); h != 0 {
		return h
	}
	return drawAppIcon(size)
}

// 兜底：用代码画一个，形状和 logo 一致
func drawAppIcon(size int) windows.Handle {
	w, h := size, size
	dib := make([]byte, 40+w*h*4)
	le := binary.LittleEndian
	le.PutUint32(dib[0:], 40)             // biSize
	le.PutUint32(dib[4:], uint32(w))      // biWidth
	le.PutUint32(dib[8:], uint32(h))      // biHeight（正数=自下而上）
	le.PutUint16(dib[12:], 1)             // biPlanes
	le.PutUint16(dib[14:], 32)            // biBitCount
	le.PutUint32(dib[20:], uint32(w*h*4)) // biSizeImage

	f := float64(size)
	blue := [3]float64{47, 124, 246}
	white := [3]float64{255, 255, 255}
	const ss = 4

	for py := 0; py < h; py++ {
		for px := 0; px < w; px++ {
			var sr, sg, sb, cov float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := float64(px) + (float64(sx)+0.5)/ss
					y := float64(py) + (float64(sy)+0.5)/ss
					if !inRoundRect(x, y, 0.02*f, 0.02*f, 0.98*f, 0.98*f, 0.23*f) {
						continue
					}
					cov++
					c := blue
					if inBubble(x, y, f) {
						c = white
					}
					sr += c[0]
					sg += c[1]
					sb += c[2]
				}
			}
			// DIB 是自下而上存储
			off := 40 + ((h-1-py)*w+px)*4
			if cov > 0 {
				dib[off+0] = uint8(sb / cov)
				dib[off+1] = uint8(sg / cov)
				dib[off+2] = uint8(sr / cov)
				dib[off+3] = uint8(cov * 255 / (ss * ss))
			}
		}
	}

	hIcon, _, _ := pCreateIconFromRes.Call(
		uintptr(unsafe.Pointer(&dib[0])),
		uintptr(len(dib)),
		1,          // fIcon
		0x00030000, // 版本
		uintptr(size), uintptr(size),
		0)
	return windows.Handle(hIcon)
}

func inRoundRect(x, y, x0, y0, x1, y1, r float64) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	cx := x
	if cx < x0+r {
		cx = x0 + r
	} else if cx > x1-r {
		cx = x1 - r
	}
	cy := y
	if cy < y0+r {
		cy = y0 + r
	} else if cy > y1-r {
		cy = y1 - r
	}
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

func inBubble(x, y, f float64) bool {
	// 气泡主体
	if inRoundRect(x, y, 0.21*f, 0.27*f, 0.79*f, 0.60*f, 0.10*f) {
		return true
	}
	// 左下的小尖角
	if x >= 0.32*f && x <= 0.50*f && y >= 0.55*f && y <= 0.75*f {
		// 斜边：从 (0.32,0.75) 到 (0.50,0.55)
		t := (x - 0.32*f) / (0.18 * f)
		return y >= 0.75*f-t*(0.20*f)
	}
	return false
}

/* ===================== 开机自启（注册表 HKCU\...\Run） ===================== */

const (
	runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValName = "LanChat"
)

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValName)
	return err == nil && strings.TrimSpace(v) != ""
}

func setAutostart(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if on {
		exe := exePath()
		if exe == "" {
			return fmt.Errorf("拿不到程序路径")
		}
		// --tray：开机时直接收进托盘，不弹窗打扰
		return k.SetStringValue(runValName, `"`+exe+`" --tray`)
	}
	if err := k.DeleteValue(runValName); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

/* ===================== 配置相关读取 ===================== */

func closeActionName() string {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	if cfg.CloseAction == "" {
		return "tray"
	}
	return cfg.CloseAction
}

/* ===================== HTTP 接口 ===================== */

func mainVisible() bool {
	h := getOwner()
	if h == 0 {
		return false
	}
	r, _, _ := pIsWindowVisible.Call(h)
	return r != 0
}

func mainRect() map[string]int {
	h := getOwner()
	out := map[string]int{"x": 0, "y": 0, "w": 0, "h": 0}
	if h == 0 {
		return out
	}
	var rc rect
	pGetWindowRect.Call(h, uintptr(unsafe.Pointer(&rc)))
	out["x"] = int(rc.Left)
	out["y"] = int(rc.Top)
	out["w"] = int(rc.Right - rc.Left)
	out["h"] = int(rc.Bottom - rc.Top)
	return out
}

func settingsPayload() map[string]interface{} {
	return map[string]interface{}{
		"version":     appVersion,
		"autostart":   autostartEnabled(),
		"closeAction": closeActionName(),
		"tray":        trayOK,
		"visible":     mainVisible(),
		"rect":        mainRect(),
		"mode":        modeNow(),
		"maximized":   maxNow(),
		"rounded":     roundedOK,
		"exe":         exePath(),
		"cfg":         cfgFile,
		"root":        rootDir(),
	}
}

func handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, settingsPayload())
}

func handleSetSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Autostart   *bool   `json:"autostart"`
		CloseAction *string `json:"closeAction"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Autostart != nil {
		if err := setAutostart(*req.Autostart); err != nil {
			writeErr(w, 500, "设置开机自启失败："+err.Error())
			return
		}
		cfgMu.Lock()
		cfg.Autostart = *req.Autostart
		cfgMu.Unlock()
		_ = saveConfig()
	}
	if req.CloseAction != nil {
		v := strings.ToLower(strings.TrimSpace(*req.CloseAction))
		if v != "tray" && v != "exit" {
			writeErr(w, 400, "closeAction 只能是 tray 或 exit")
			return
		}
		cfgMu.Lock()
		cfg.CloseAction = v
		cfgMu.Unlock()
		if err := saveConfig(); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	writeJSON(w, settingsPayload())
}

// /api/window  动作：min / max / mode / show / hide / close / quit / drag
func handleWindow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		Value  string `json:"value"`
	}
	_ = readBodyJSON(r, &req)
	act := strings.ToLower(strings.TrimSpace(req.Action))
	h := getOwner()

	if h == 0 && act != "close" && act != "quit" {
		// 无窗口模式（自检/测试）下窗口类操作是空动作
		writeJSON(w, map[string]interface{}{"ok": true, "noop": true})
		return
	}

	resp := map[string]interface{}{"ok": true}

	switch act {
	case "min":
		pShowWindow.Call(h, swMinimize)
	case "max":
		resp["maximized"] = toggleMaximize(h)
	case "show", "restore":
		showMain()
	case "hide":
		hideToTray()
	case "drag":
		pPostMessage.Call(h, wmDrag, 0, 0)
	case "close":
		go func() { time.Sleep(80 * time.Millisecond); doCloseAction() }()
	case "quit":
		go func() { time.Sleep(80 * time.Millisecond); quitApp() }()
	default:
		writeErr(w, 400, "未知动作")
		return
	}
	resp["maximized"] = maxNow()
	resp["mode"] = modeNow()
	resp["rect"] = mainRect()
	writeJSON(w, resp)
}

// /api/open-cfg  打开配置所在目录（不受共享文件夹限制）
func handleOpenCfgDir(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Dir(cfgFile)
	if err := startDetached("explorer.exe", dir); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "path": dir})
}

/* ===================== 外壳自检模式 =====================
   本机 WebView2 运行时可能坏掉（窗口白屏），这时无法验证
   「无边框 / 圆角 / 托盘」这几段代码。设 LANCHAT_SHELLTEST=1 时
   只创建一个纯 Win32 空窗口，走同一套外观和托盘逻辑。 */

func shellTestMain() {
	hInst, _, _ := pGetModuleHandle.Call(0)
	cls := utf16z("LanChatShellTest", 0)
	brush, _, _ := pGetSysColorBrush.Call(5) // COLOR_WINDOW
	wc := wndClassEx{
		Size:       uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:    testWndProcCb,
		Instance:   windows.Handle(hInst),
		Background: windows.Handle(brush),
		ClassName:  &cls[0],
	}
	if r, _, err := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		debugf("shelltest: RegisterClassEx 失败 %v", err)
	}

	title := utf16z("Lanchat · 外壳自检", 0)
	h, _, _ := pCreateWindowEx.Call(wsExAppWindow,
		uintptr(unsafe.Pointer(&cls[0])),
		uintptr(unsafe.Pointer(&title[0])),
		wsPopup|wsVisible, 300, 200, 480, 300, 0, 0, hInst, 0)
	if h == 0 {
		debugf("shelltest: 创建窗口失败")
		os.Exit(2)
	}
	setOwner(h)
	roundedOK = applyFrameless(h, "chat")
	installMainHook(h)
	if trayIcon == 0 {
		trayIcon = makeAppIcon(32)
	}
	setWindowIcon(h, trayIcon)
	createTray(h)
	debugf("shelltest: hwnd=%#x dwm圆角=%v tray=%v 模式=%s 尺寸=%v", h, roundedOK, trayOK, modeNow(), mainRect())

	// 走真实的 HTTP 服务，方便外部验证窗口动作
	ln, err := netListen()
	if err != nil {
		os.Exit(2)
	}
	go func() { _ = httpServe(ln) }()
	debugf("shelltest: http://%s/", ln.Addr().String())

	// 必须有消息循环：别的 goroutine 调 ShowWindow 时会往这个线程发消息
	for {
		var m msgStruct
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// MSG（x64 下 48 字节）
type msgStruct struct {
	Hwnd    uintptr
	Message uint32
	_       uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	_       uint32
}

func testWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmClose:
		doCloseAction()
		return 0
	case wmDrag:
		pReleaseCapture.Call()
		pSendMessage.Call(hwnd, wmNCLButtonDown, htCaption, 0)
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, msg, wp, lp)
	return r
}
