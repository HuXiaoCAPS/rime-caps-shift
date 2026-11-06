//go:build windows

// winapi.go —— 所有 Win32 DLL/Proc 声明、常量、结构体，以及少量通用辅助。
// 本文件不实现业务逻辑，只提供底层绑定；其余文件都依赖本文件。

package main

import (
	"encoding/hex"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---------- 应用级常量（所有文件共享） ----------

const (
	APP_NAME    = "RimeCapsShift"
	APP_VERSION = "1.3.2"
)

const (
	WINDOW_CLASS = "RimeCapsShift.HiddenWindow"
	HALO_CLASS   = "RimeCapsShift.HaloOverlay"
	MUTEX_NAME   = `Local\RimeCapsShift.SingleInstance`

	TRAY_UID = 1

	WM_APP_TRAY      = WM_APP + 1
	WM_APP_SHOW_TRAY = WM_APP + 2

	ID_TRAY_EXIT      = 1001
	ID_TRAY_AUTOSTART = 1002
	ID_TRAY_HIDE      = 1003

	AUTOSTART_KEY   = `Software\Microsoft\Windows\CurrentVersion\Run`
	AUTOSTART_VALUE = "RimeCapsShift"

	POLL_INTERVAL_MS = 250
	HALO_INTERVAL_MS = 50
	TIMER_ID_POLL    = 1
	TIMER_ID_HALO    = 2

	HALO_DEFAULT_COLOR = "FFB347"
	HALO_DEFAULT_SIZE  = 96
	HALO_MIN_SIZE      = 32
	HALO_MAX_SIZE      = 512
)

var WEASEL_KEYWORDS = []string{"weasel", "小狼毫", "rime"}

const WEASEL_DEFAULT_CLSID = "A3F4CDED-B1E9-41EE-9CA6-7B4D0DE6CB0A"

// ---------- Win32 常量 ----------

const (
	// 虚拟键
	VK_CAPITAL = 0x14
	VK_SPACE   = 0x20
	VK_CONTROL = 0x11
	VK_MENU    = 0x12
	VK_SHIFT   = 0x10
	VK_LSHIFT  = 0xA0
	VK_RSHIFT  = 0xA1

	KEYEVENTF_KEYUP = 0x0002
	INPUT_KEYBOARD  = 1
	LLKHF_INJECTED  = 0x10
	WH_KEYBOARD_LL  = 13
	HC_ACTION       = 0

	// 消息
	WM_NULL            = 0x0000
	WM_DESTROY         = 0x0002
	WM_CLOSE           = 0x0010
	WM_QUERYENDSESSION = 0x0011
	WM_ENDSESSION      = 0x0016
	WM_MOUSEACTIVATE   = 0x0021
	WM_CONTEXTMENU     = 0x007B
	WM_NCHITTEST       = 0x0084
	WM_KEYDOWN         = 0x0100
	WM_KEYUP           = 0x0101
	WM_SYSKEYDOWN      = 0x0104
	WM_SYSKEYUP        = 0x0105
	WM_COMMAND         = 0x0111
	WM_TIMER           = 0x0113
	WM_RBUTTONUP       = 0x0205
	WM_APP             = 0x8000

	HTTRANSPARENT = -1
	MA_NOACTIVATE = 3

	WS_POPUP = 0x80000000

	WS_EX_TRANSPARENT = 0x00000020
	WS_EX_TOOLWINDOW  = 0x00000080
	WS_EX_TOPMOST     = 0x00000008
	WS_EX_LAYERED     = 0x00080000
	WS_EX_NOACTIVATE  = 0x08000000

	SW_HIDE           = 0
	SW_SHOWNOACTIVATE = 4

	SWP_NOSIZE     = 0x0001
	SWP_NOMOVE     = 0x0002
	SWP_NOACTIVATE = 0x0010
	SWP_SHOWWINDOW = 0x0040

	ULW_ALPHA    = 2
	AC_SRC_OVER  = 0
	AC_SRC_ALPHA = 1

	BI_RGB         = 0
	DIB_RGB_COLORS = 0

	MB_OK        = 0x00000000
	MB_ICONERROR = 0x00000010

	NIM_ADD    = 0
	NIM_MODIFY = 1
	NIM_DELETE = 2

	NIF_MESSAGE = 0x00000001
	NIF_ICON    = 0x00000002
	NIF_TIP     = 0x00000004

	MF_STRING    = 0x00000000
	MF_GRAYED    = 0x00000001
	MF_CHECKED   = 0x00000008
	MF_SEPARATOR = 0x00000800

	TPM_RIGHTBUTTON = 0x0002
	TPM_NONOTIFY    = 0x0080
	TPM_RETURNCMD   = 0x0100

	CTRL_C_EVENT        = 0
	CTRL_BREAK_EVENT    = 1
	CTRL_CLOSE_EVENT    = 2
	CTRL_LOGOFF_EVENT   = 5
	CTRL_SHUTDOWN_EVENT = 6

	ERROR_ALREADY_EXISTS = 183

	COINIT_APARTMENTTHREADED = 0x2
	CLSCTX_INPROC_SERVER     = 0x1
	S_OK                     = 0
	S_FALSE                  = 1
	RPC_E_CHANGED_MODE       = 0x80010106
)

// 特殊指针值
var (
	// HWND_TOPMOST == HWND(-1)
	HWND_TOPMOST = ^windows.Handle(0)

	// LPVOID(-4)：Per-Monitor V2 DPI 感知上下文
	DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = ^uintptr(3)
)

// ---------- 结构体 ----------

type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

func (g GUID) IsNull() bool {
	return g.Data1 == 0 && g.Data2 == 0 && g.Data3 == 0 && g.Data4 == [8]byte{}
}

func (g GUID) String() string {
	return fmt.Sprintf("{%08X-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X}",
		g.Data1, g.Data2, g.Data3,
		g.Data4[0], g.Data4[1], g.Data4[2], g.Data4[3],
		g.Data4[4], g.Data4[5], g.Data4[6], g.Data4[7])
}

// guidFromString 解析带不带花括号、带不带连字符的 GUID 字符串。
// 这里自己解析，不依赖 windows.GUIDFromString：后者对输入格式的要求
// 随 x/sys 版本变化（部分版本要求 38 字符且必须带花括号），而注册表、
// 常量里两种写法都会出现。字段按 GUID 的字符串语义写入，Data1/2/3 是
// 大端数字，Data4 原样保存。
func guidFromString(s string) (GUID, error) {
	text := strings.NewReplacer("{", "", "}", "", "-", "").Replace(strings.TrimSpace(s))
	if len(text) != 32 {
		return GUID{}, fmt.Errorf("无效的 GUID：%q", s)
	}
	raw, err := hex.DecodeString(text)
	if err != nil {
		return GUID{}, fmt.Errorf("无效的 GUID：%q", s)
	}
	return GUID{
		Data1: uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3]),
		Data2: uint16(raw[4])<<8 | uint16(raw[5]),
		Data3: uint16(raw[6])<<8 | uint16(raw[7]),
		Data4: [8]byte{raw[8], raw[9], raw[10], raw[11], raw[12], raw[13], raw[14], raw[15]},
	}, nil
}

// 显示与 DPI 辅助结构体
type POINT struct{ X, Y int32 }
type SIZE struct{ CX, CY int32 }
type RECT struct{ Left, Top, Right, Bottom int32 }

// 键盘钩子回调结构
type KBDLLHOOKSTRUCT struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

// SendInput 结构
type MOUSEINPUT struct {
	Dx          int32
	Dy          int32
	MouseData   uint32
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

type KEYBDINPUT struct {
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

type HARDWAREINPUT struct {
	UMsg    uint32
	WParamL uint16
	WParamH uint16
}

// INPUT 联合体在 Go 里没法直接表达；这里只用到 keyboard 分支，
// 末尾的 [8]byte 把 union 补齐：
//   - amd64: KEYBDINPUT 24 字节 + 8 padding = 32，整体 4 + 4(pad) + 32 = 40
//   - 386:   KEYBDINPUT 16 字节 + 8 padding = 24，整体 4 + 24 = 28
//
// 启动时由 initInputLayoutCheck() 校验。
type INPUT struct {
	Type uint32
	Ki   KEYBDINPUT
	_    [8]byte
}

// 托盘
type NOTIFYICONDATAW struct {
	CbSize            uint32
	HWnd              windows.Handle
	UID               uint32
	UFlags            uint32
	UCallbackMessage  uint32
	HIcon             windows.Handle
	SzTip             [128]uint16
	DwState           uint32
	DwStateMask       uint32
	SzInfo            [256]uint16
	UTimeoutOrVersion uint32
	SzInfoTitle       [64]uint16
	DwInfoFlags       uint32
	GuidItem          GUID
	HBalloonIcon      windows.Handle
}

// 图标绘制
type BITMAPINFOHEADER struct {
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

type BITMAPINFO struct {
	BmiHeader BITMAPINFOHEADER
	BmiColors [1]uint32
}

type BLENDFUNCTION struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}

type ICONINFO struct {
	FIcon    int32 // BOOL
	XHotspot uint32
	YHotspot uint32
	HbmMask  windows.Handle
	HbmColor windows.Handle
}

// 消息 / 窗口
type MSG struct {
	HWnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      POINT
}

type WNDCLASSEXW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr // syscall.NewCallback 的返回值
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

// ---------- DLL ----------

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	imm32    = windows.NewLazySystemDLL("imm32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	ole32    = windows.NewLazySystemDLL("ole32.dll")
	shcore   = windows.NewLazySystemDLL("shcore.dll") // Win8.1+，按需 Find()
)

// ---------- Proc 声明 ----------

var (
	// ---- user32：键盘 / 输入 ----
	procSetWindowsHookExW   = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procSendInput           = user32.NewProc("SendInput")
	procMapVirtualKeyW      = user32.NewProc("MapVirtualKeyW")
	procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")
	procGetCursorPos        = user32.NewProc("GetCursorPos")

	// ---- user32：窗口 / 前台 ----
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procGetKeyboardLayout        = user32.NewProc("GetKeyboardLayout")

	// ---- user32：消息循环 ----
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procPostMessageW     = user32.NewProc("PostMessageW")

	// ---- user32：窗口类 / 窗口 ----
	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procUnregisterClassW = user32.NewProc("UnregisterClassW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procFindWindowW      = user32.NewProc("FindWindowW")
	procIsWindow         = user32.NewProc("IsWindow")

	// ---- user32：定时器 ----
	procSetTimer  = user32.NewProc("SetTimer")
	procKillTimer = user32.NewProc("KillTimer")

	// ---- user32：资源 ----
	procLoadIconW   = user32.NewProc("LoadIconW")
	procLoadCursorW = user32.NewProc("LoadCursorW")

	// ---- user32：菜单 ----
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")

	// ---- user32：GDI 显示 ----
	procGetDC               = user32.NewProc("GetDC")
	procReleaseDC           = user32.NewProc("ReleaseDC")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetWindowPos        = user32.NewProc("SetWindowPos")
	procUpdateLayeredWindow = user32.NewProc("UpdateLayeredWindow")

	// ---- user32：杂项 ----
	procMessageBeep = user32.NewProc("MessageBeep")
	procMessageBoxW = user32.NewProc("MessageBoxW")

	// ---- user32：图标 ----
	procCreateIconIndirect = user32.NewProc("CreateIconIndirect")
	procDestroyIcon        = user32.NewProc("DestroyIcon")

	// ---- user32：DPI ----
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")

	// ---- gdi32 ----
	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procCreateBitmap       = gdi32.NewProc("CreateBitmap")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")

	// ---- kernel32 ----
	procGetModuleHandleW      = kernel32.NewProc("GetModuleHandleW")
	procCreateMutexW          = kernel32.NewProc("CreateMutexW")
	procCloseHandle           = kernel32.NewProc("CloseHandle")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")

	// ---- imm32 ----
	procImmGetDescriptionW = imm32.NewProc("ImmGetDescriptionW")
	procImmGetIMEFileNameW = imm32.NewProc("ImmGetIMEFileNameW")

	// ---- shell32 ----
	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")

	// ---- ole32 ----
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")

	// ---- shcore ----
	procSetProcessDpiAwareness = shcore.NewProc("SetProcessDpiAwareness")
)

// ---------- 通用辅助 ----------

// utf16Ptr 把字符串转成 C 字符串指针。空串返回 nil。
func utf16Ptr(s string) *uint16 {
	if s == "" {
		return nil
	}
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

// boolToUintptr 便于把 bool 直接作为 Win32 参数。
func boolToUintptr(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}

// getAsyncDown 查询某个 VK 当前是否物理按下（不区分注入）。
func getAsyncDown(vk int) bool {
	r, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
	return (r & 0x8000) != 0
}

// initInputLayoutCheck 启动时校验 INPUT 结构体大小；不一致立即 panic，
// 避免 SendInput 静默失败。
func initInputLayoutCheck() {
	want := uintptr(28)
	if unsafe.Sizeof(uintptr(0)) == 8 {
		want = 40
	}
	got := unsafe.Sizeof(INPUT{})
	if got != want {
		panic(fmt.Sprintf("INPUT struct size mismatch: got %d, want %d", got, want))
	}
}

// ptrToUint 是 uintptr(unsafe.Pointer(p)) 的便捷写法，所有文件都会用到。
func ptrToUint(p unsafe.Pointer) uintptr {
	return uintptr(p)
}
