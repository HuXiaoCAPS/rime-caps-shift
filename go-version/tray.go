//go:build windows

// tray.go —— 托盘图标 + 右键菜单 + 开机自启动开关入口。
//
// 与 Python 版 TrayIcon / RimeCapsShiftApp 的托盘相关方法一一对应。
// 所有菜单操作发生在 WM_APP_TRAY 消息处理里，即主线程，无需加锁。

package main

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TrayIcon 封装 Shell_NotifyIconW。
type TrayIcon struct {
	hwnd            windows.Handle
	defaultIcon     windows.Handle
	activeIcon      windows.Handle
	colorAvailable  bool
	active          bool
	added           bool
	lastTip         string
	data            NOTIFYICONDATAW
}

func newTrayIcon(hwnd, defaultIcon, activeIcon windows.Handle, colorAvailable bool) *TrayIcon {
	t := &TrayIcon{
		hwnd:           hwnd,
		defaultIcon:    defaultIcon,
		activeIcon:     activeIcon,
		colorAvailable: colorAvailable,
	}
	t.data.CbSize = uint32(unsafe.Sizeof(NOTIFYICONDATAW{}))
	t.data.HWnd = hwnd
	t.data.UID = TRAY_UID
	t.data.UFlags = NIF_MESSAGE | NIF_ICON | NIF_TIP
	t.data.UCallbackMessage = WM_APP_TRAY
	t.data.HIcon = defaultIcon
	copyTip(&t.data.SzTip, "RimeCapsShift")
	return t
}

// copyTip 把字符串写入固定长度的 UTF-16 缓冲区（截断 + 补 0）。
func copyTip(dst *[128]uint16, s string) {
	u, _ := windows.UTF16FromString(s)
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	for i := range dst {
		dst[i] = 0
	}
	copy(dst[:], u)
}

func (t *TrayIcon) iconFor(active bool) windows.Handle {
	if !t.colorAvailable {
		return t.defaultIcon
	}
	if active {
		return t.activeIcon
	}
	return t.defaultIcon
}

func (t *TrayIcon) add(tip string, active bool) bool {
	t.active = active
	copyTip(&t.data.SzTip, tip)
	t.data.HIcon = t.iconFor(active)
	ok, _, errno := procShellNotifyIconW.Call(
		NIM_ADD,
		ptrToUint(unsafe.Pointer(&t.data)),
	)
	if ok == 0 {
		logWarn("添加托盘图标失败：%v；仍可用 --quit 或 Ctrl+C 退出", errno)
		return false
	}
	t.added = true
	t.lastTip = tip
	return true
}

// setState 锁存状态变化时同时刷新图标颜色和提示文本。
func (t *TrayIcon) setState(active bool, tip string) {
	if !t.added {
		t.active = active
		t.lastTip = tip
		return
	}
	if active == t.active && tip == t.lastTip {
		return
	}
	changedIcon := active != t.active
	t.active = active
	copyTip(&t.data.SzTip, tip)
	t.data.HIcon = t.iconFor(active)
	t.data.UFlags = NIF_ICON | NIF_TIP

	ok, _, errno := procShellNotifyIconW.Call(
		NIM_MODIFY,
		ptrToUint(unsafe.Pointer(&t.data)),
	)
	if ok == 0 {
		logDebug("更新托盘图标失败：%v", errno)
	} else {
		t.lastTip = tip
	}
	t.data.UFlags = NIF_MESSAGE | NIF_ICON | NIF_TIP

	if changedIcon {
		state := "默认"
		if active {
			state = "高亮"
		}
		logDebug("托盘图标切换为%s", state)
	}
}

func (t *TrayIcon) update(tip string) {
	t.setState(t.active, tip)
}

func (t *TrayIcon) remove() {
	if !t.added {
		return
	}
	procShellNotifyIconW.Call(NIM_DELETE, ptrToUint(unsafe.Pointer(&t.data)))
	t.added = false
}

// ---------- App 侧：托盘装配与菜单 ----------

// ensureTray 创建或重新添加托盘图标；也用于 --show-tray 恢复。
func (a *App) ensureTray() bool {
	if a.hwnd == 0 {
		return false
	}
	if a.tray == nil {
		off, on, reason := createTrayIcons()
		a.iconDefault = off
		a.iconActive = on
		a.trayColorReason = reason
		a.customIcons = off != on
		a.tray = newTrayIcon(a.hwnd, off, on, a.customIcons)
	}
	if !a.tray.added {
		active := a.visual.latched
		if !a.tray.add(a.trayTip(), active) {
			logWarn("添加托盘图标失败，可运行 --show-tray 重试")
			return false
		}
	}
	a.visual.tray = a.tray
	a.cfg.NoTray = false
	logInfo("托盘变色：%s", a.trayColorReason)
	return true
}

// hideTray 隐藏托盘图标，但保留 HICON 以便 --show-tray 恢复。
func (a *App) hideTray() {
	if a.tray == nil || !a.tray.added {
		return
	}
	a.tray.remove()
	a.visual.tray = nil
	logInfo("托盘图标已隐藏；可运行 `--show-tray` 恢复")
}

// showTray 由 WM_APP_SHOW_TRAY 触发，恢复托盘。
func (a *App) showTray() {
	if a.ensureTray() {
		logInfo("托盘图标已恢复")
	}
}

// trayTip 返回托盘提示文本，VisualState 的 tipProvider 会调用。
func (a *App) trayTip() string {
	return "RimeCapsShift：" + a.state.statusShort()
}

// showTrayMenu 弹出右键菜单并分发命令。
func (a *App) showTrayMenu() {
	a.menuOpen = true
	defer func() { a.menuOpen = false }()

	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	appendMenu := func(flags uintptr, id uintptr, text string) {
		procAppendMenuW.Call(
			menu, flags, id,
			ptrToUint(unsafe.Pointer(utf16Ptr(text))),
		)
	}

	appendMenu(MF_STRING|MF_GRAYED, 0, a.state.statusMenu())

	autostartFlags := uintptr(MF_STRING)
	if isAutostartEnabled() {
		autostartFlags |= MF_CHECKED
	}
	appendMenu(autostartFlags, ID_TRAY_AUTOSTART, "开机自启动")
	appendMenu(MF_STRING, ID_TRAY_HIDE, "隐藏托盘图标")
	appendMenu(MF_SEPARATOR, 0, "")
	appendMenu(MF_STRING, ID_TRAY_EXIT, "退出 RimeCapsShift")

	var pt POINT
	procGetCursorPos.Call(ptrToUint(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(uintptr(a.hwnd))

	cmd, _, _ := procTrackPopupMenu.Call(
		menu,
		TPM_RIGHTBUTTON|TPM_NONOTIFY|TPM_RETURNCMD,
		uintptr(pt.X), uintptr(pt.Y),
		0, uintptr(a.hwnd), 0,
	)
	// 必须 PostMessage(WM_NULL) 让菜单彻底关闭，否则点外面不消失。
	procPostMessageW.Call(uintptr(a.hwnd), WM_NULL, 0, 0)

	switch cmd {
	case ID_TRAY_AUTOSTART:
		a.toggleAutostart()
	case ID_TRAY_HIDE:
		a.hideTray()
	case ID_TRAY_EXIT:
		a.quit()
	}
}

// toggleAutostart 切换开机自启动并提示用户。
func (a *App) toggleAutostart() {
	enable := !isAutostartEnabled()
	if !setAutostartEnabled(enable) {
		procMessageBoxW.Call(
			uintptr(a.hwnd),
			ptrToUint(unsafe.Pointer(utf16Ptr(
				"无法更新开机自启动设置。\n"+
					"请检查当前用户是否有写入 HKCU\\...\\Run 的权限。",
			))),
			ptrToUint(unsafe.Pointer(utf16Ptr(APP_NAME))),
			MB_ICONERROR,
		)
		return
	}
	state := "关闭"
	if enable {
		state = "开启"
	}
	logInfo("开机自启动已%s", state)
}

// 供 main.go 保证 Windows.Handle 类型统一；防止当前文件未使用 import。
var _ = runtime.GOOS
var _ = fmt.Sprintf