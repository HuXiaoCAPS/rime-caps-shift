//go:build windows

// hook.go —— WH_KEYBOARD_LL 全局低级键盘钩子 + 左 Shift 注入器。
//
// 约定：
//   * 回调内只做纯内存判断，绝不 log、绝不 COM 查询、绝不 I/O。
//   * 任何 panic 用 recover 兜住并记录到 app.hookErr，由定时器在钩子外 log。
//   * 只处理物理按键；LLKHF_INJECTED 事件（本程序或其他程序注入）一律放行。

package main

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// shiftExtraInfo 与 Python 版一致："RCS1"，用于识别本程序注入的 Shift。
const shiftExtraInfo = uintptr(0x52435331)

// ---------- ShiftInjector ----------

// ShiftInjector 只发送左 Shift 的按下/抬起，绝不产生自动重复。
// 与 Python 版 ShiftInjector 一一对应；字段访问全部发生在主线程，无需加锁。
type ShiftInjector struct {
	downSent  bool
	lastError error
	scanCode  uint32
}

func newShiftInjector() *ShiftInjector {
	r, _, _ := procMapVirtualKeyW.Call(VK_LSHIFT, 0)
	sc := uint32(r)
	if sc == 0 {
		sc = 0x2A // 左 Shift 的典型扫描码，兜底
	}
	return &ShiftInjector{scanCode: sc}
}

func (s *ShiftInjector) isDown() bool { return s.downSent }

// send 构造 KEYBDINPUT 并 SendInput。keyup=true 时发送抬起。
func (s *ShiftInjector) send(keyup bool) bool {
	var inp INPUT
	inp.Type = INPUT_KEYBOARD
	inp.Ki.WVk = VK_LSHIFT
	inp.Ki.WScan = uint16(s.scanCode)
	if keyup {
		inp.Ki.DwFlags = KEYEVENTF_KEYUP
	}
	inp.Ki.Time = 0
	inp.Ki.DwExtraInfo = shiftExtraInfo

	n, _, errno := procSendInput.Call(
		1,
		ptrToUint(unsafe.Pointer(&inp)),
		unsafe.Sizeof(inp),
	)
	runtime.KeepAlive(&inp)
	if n != 1 {
		s.lastError = errno
		return false
	}
	return true
}

func (s *ShiftInjector) press() bool {
	if s.downSent {
		return true
	}
	if s.send(false) {
		s.downSent = true
		return true
	}
	return false
}

func (s *ShiftInjector) release() bool {
	if !s.downSent {
		return true
	}
	if s.send(true) {
		s.downSent = false
		return true
	}
	// 保留 downSent=true，后续清理时还会再试一次。
	return false
}

func (s *ShiftInjector) releaseBestEffort() { s.release() }

// ---------- 全局低级键盘钩子 ----------

// lowLevelKeyboardProc 是 WH_KEYBOARD_LL 的回调。在安装钩子的线程
// （即主线程，被 LockOSThread 锁定）上被调用。
//
// 注意：低级钩子默认 300ms 超时；超时 Windows 会静默丢弃这个钩子。
// 所以这里只做内存判断，所有副作用都交给 WM_TIMER 处理。
func lowLevelKeyboardProc(nCode int32, wParam, lParam uintptr) uintptr {
	defer func() {
		if r := recover(); r != nil && app != nil {
			app.hookErr = fmt.Errorf("hook panic: %v", r)
		}
	}()

	if nCode == HC_ACTION && app != nil && app.state != nil {
		kb := (*KBDLLHOOKSTRUCT)(unsafe.Pointer(lParam))
		if kb.Flags&LLKHF_INJECTED == 0 {
			down := wParam == WM_KEYDOWN || wParam == WM_SYSKEYDOWN
			up := wParam == WM_KEYUP || wParam == WM_SYSKEYUP
			if (down || up) && app.state.HandleKey(kb.VkCode, down) {
				return 1 // 吞掉
			}
		}
	}

	var hook uintptr
	if app != nil {
		hook = uintptr(app.hook)
	}
	r, _, _ := procCallNextHookEx.Call(hook, uintptr(nCode), wParam, lParam)
	return r
}

// installHook 在 App 中注册全局低级键盘钩子。
func (a *App) installHook() error {
	a.hookCb = syscall.NewCallback(lowLevelKeyboardProc)
	h, _, errno := procSetWindowsHookExW.Call(
		uintptr(WH_KEYBOARD_LL),
		a.hookCb,
		uintptr(a.hinstance),
		0,
	)
	if h == 0 {
		return fmt.Errorf("SetWindowsHookExW 失败：%w", errno)
	}
	a.hook = windows.Handle(h)
	logDebug("WH_KEYBOARD_LL 钩子已安装")
	return nil
}