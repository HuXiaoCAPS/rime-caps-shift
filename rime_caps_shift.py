#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
RimeCapsShift
=============

在 Windows 上把 CapsLock 变成“左 Shift 锁存开关”，仅当小狼毫（Weasel / Rime）
处于中文模式时生效；其他输入法或小狼毫英文模式下 CapsLock 保持原样。
第一次按 CapsLock 锁定左 Shift，第二次按下解除锁定。

视觉反馈（均不修改系统光标、不注入 CapsLock、不要求管理员权限）：
  * 托盘图标在锁定时从默认图标切换为高亮图标；
  * 锁定时在鼠标周围显示一个点击穿透、不抢焦点的半透明呼吸光晕；
  * 可选 --beep 在锁定/解除时播放系统提示音。

实现要点：
  * WH_KEYBOARD_LL 全局低级键盘钩子（普通用户权限，不需要驱动）。
  * 输入法识别：
      1) 读取前台线程 HKL，用 ImmGetDescriptionW / ImmGetIMEFileNameW 匹配
         Weasel / 小狼毫 / Rime；
      2) 由于新版小狼毫是 TSF 文本服务，HKL 往往只是通用的 0x08040804，
         ImmGetDescription 返回空，因此额外用只读的 TSF
         ITfInputProcessorProfiles::GetActiveLanguageProfile 检查当前
         活动配置是否为 Weasel。
  * EVENT_SYSTEM_INPUTLANGCHANGED 不直接使用，改为 250ms 轻量轮询前台
    HKL + TSF 活动配置，可靠且实现简单。
  * 小狼毫内部中英文状态改为键盘事件驱动：可配置右/左/双 Shift 的短按
    判定，Shift 回调只做内存状态判断；钩子回调内不做任何 TSF/COM 查询，
    也不写日志或执行文件/网络 I/O。
  * 托盘菜单使用 Shell_NotifyIconW；光效窗口使用 UpdateLayeredWindow。
    纯 ctypes，不依赖 pywin32。

只用 Python 标准库与 ctypes。无第三方运行时依赖。
"""

from __future__ import annotations

import argparse
import atexit
import ctypes
import logging
import math
import os
import re
import sys
import time
import uuid
import winreg
from ctypes import wintypes

APP_NAME = "RimeCapsShift"
APP_VERSION = "1.3.1"
WINDOW_CLASS = "RimeCapsShift.HiddenWindow"
MUTEX_NAME = r"Local\RimeCapsShift.SingleInstance"
TRAY_UID = 1
WM_APP_TRAY = 0x8000 + 1
WM_APP_SHOW_TRAY = 0x8000 + 2
ID_TRAY_EXIT = 1001
ID_TRAY_AUTOSTART = 1002
ID_TRAY_HIDE = 1003
AUTOSTART_KEY = r"Software\Microsoft\Windows\CurrentVersion\Run"
AUTOSTART_VALUE = "RimeCapsShift"
POLL_INTERVAL_MS = 250
HALO_INTERVAL_MS = 50
TIMER_ID_POLL = 1
TIMER_ID_HALO = 2
HALO_CLASS = "RimeCapsShift.HaloOverlay"
HALO_DEFAULT_COLOR = "FFB347"
HALO_DEFAULT_SIZE = 96
HALO_MIN_SIZE = 32
HALO_MAX_SIZE = 512

# 调整识别关键字时只改这里即可。匹配规则见 _text_matches_weasel()。
WEASEL_KEYWORDS = ("weasel", "小狼毫", "rime")

# 小狼毫的 TSF CLSID。它多年保持稳定；作为注册表扫描失败时的兜底候选。
WEASEL_DEFAULT_CLSID = "A3F4CDED-B1E9-41EE-9CA6-7B4D0DE6CB0A"

# Win32 常量
VK_CAPITAL = 0x14
VK_SPACE = 0x20
VK_CONTROL = 0x11
VK_MENU = 0x12
VK_SHIFT = 0x10
VK_LSHIFT = 0xA0
VK_RSHIFT = 0xA1
KEYEVENTF_KEYUP = 0x0002
INPUT_KEYBOARD = 1
LLKHF_INJECTED = 0x10
WH_KEYBOARD_LL = 13
HC_ACTION = 0
WM_KEYDOWN = 0x0100
WM_KEYUP = 0x0101
WM_SYSKEYDOWN = 0x0104
WM_SYSKEYUP = 0x0105
WM_CLOSE = 0x0010
WM_DESTROY = 0x0002
WM_QUERYENDSESSION = 0x0011
WM_ENDSESSION = 0x0016
WM_TIMER = 0x0113
WM_RBUTTONUP = 0x0205
WM_CONTEXTMENU = 0x007B
WM_NULL = 0x0000
WM_COMMAND = 0x0111
WM_APP = 0x8000
WM_NCHITTEST = 0x0084
WM_MOUSEACTIVATE = 0x0021
HTTRANSPARENT = -1
MA_NOACTIVATE = 3

WS_EX_LAYERED = 0x00080000
WS_EX_TRANSPARENT = 0x00000020
WS_EX_TOOLWINDOW = 0x00000080
WS_EX_NOACTIVATE = 0x08000000
WS_EX_TOPMOST = 0x00000008
WS_POPUP = 0x80000000
SW_SHOWNOACTIVATE = 4
SW_HIDE = 0
HWND_TOPMOST = wintypes.HWND(-1)
SWP_NOSIZE = 0x0001
SWP_NOMOVE = 0x0002
SWP_NOACTIVATE = 0x0010
SWP_SHOWWINDOW = 0x0040
ULW_ALPHA = 2
AC_SRC_OVER = 0
AC_SRC_ALPHA = 1
BI_RGB = 0
DIB_RGB_COLORS = 0
MB_OK = 0x00000000
DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = ctypes.c_void_p(-4)

NIM_ADD = 0
NIM_MODIFY = 1
NIM_DELETE = 2
NIF_MESSAGE = 0x00000001
NIF_ICON = 0x00000002
NIF_TIP = 0x00000004
MF_STRING = 0x00000000
MF_GRAYED = 0x00000001
MF_CHECKED = 0x00000008
MF_SEPARATOR = 0x00000800
TPM_RIGHTBUTTON = 0x0002
TPM_NONOTIFY = 0x0080
TPM_RETURNCMD = 0x0100

CTRL_C_EVENT = 0
CTRL_BREAK_EVENT = 1
CTRL_CLOSE_EVENT = 2
CTRL_LOGOFF_EVENT = 5
CTRL_SHUTDOWN_EVENT = 6
ERROR_ALREADY_EXISTS = 183
COINIT_APARTMENTTHREADED = 0x2
CLSCTX_INPROC_SERVER = 0x1
S_OK = 0
S_FALSE = 1

LRESULT = ctypes.c_ssize_t
ULONG_PTR = ctypes.c_size_t
HRESULT = ctypes.c_long
HHOOK = ctypes.c_void_p
HICON = ctypes.c_void_p
HCURSOR = ctypes.c_void_p
HBRUSH = ctypes.c_void_p
HMENU = ctypes.c_void_p
HINSTANCE = ctypes.c_void_p
HANDLE = ctypes.c_void_p
UINT_PTR = ctypes.c_size_t
ATOM = ctypes.c_ushort
LPVOID = ctypes.c_void_p

log = logging.getLogger(APP_NAME)


def _print(message: str = "", file=None) -> None:
    """在 --windows-disable-console 构建中 stdout/stderr 可能为 None。"""
    stream = file if file is not None else sys.stdout
    if stream is None:
        return
    try:
        print(message, file=stream)
    except Exception:
        pass


# ---------------------------------------------------------------------------
# Win32 结构体
# ---------------------------------------------------------------------------

class GUID(ctypes.Structure):
    _fields_ = [
        ("Data1", wintypes.DWORD),
        ("Data2", wintypes.WORD),
        ("Data3", wintypes.WORD),
        ("Data4", ctypes.c_ubyte * 8),
    ]

    @classmethod
    def from_string(cls, value: str) -> "GUID":
        g = cls()
        ctypes.memmove(ctypes.byref(g), uuid.UUID(value).bytes_le, 16)
        return g

    def is_null(self) -> bool:
        return not (
            self.Data1 or self.Data2 or self.Data3
            or any(self.Data4)
        )

    def __str__(self) -> str:
        return "{%08X-%04X-%04X-%02X%02X-%s}" % (
            self.Data1,
            self.Data2,
            self.Data3,
            self.Data4[0],
            self.Data4[1],
            "".join("%02X" % b for b in self.Data4[2:]),
        )


class KBDLLHOOKSTRUCT(ctypes.Structure):
    _fields_ = [
        ("vkCode", wintypes.DWORD),
        ("scanCode", wintypes.DWORD),
        ("flags", wintypes.DWORD),
        ("time", wintypes.DWORD),
        ("dwExtraInfo", ULONG_PTR),
    ]


class MOUSEINPUT(ctypes.Structure):
    _fields_ = [
        ("dx", wintypes.LONG),
        ("dy", wintypes.LONG),
        ("mouseData", wintypes.DWORD),
        ("dwFlags", wintypes.DWORD),
        ("time", wintypes.DWORD),
        ("dwExtraInfo", ULONG_PTR),
    ]


class KEYBDINPUT(ctypes.Structure):
    _fields_ = [
        ("wVk", wintypes.WORD),
        ("wScan", wintypes.WORD),
        ("dwFlags", wintypes.DWORD),
        ("time", wintypes.DWORD),
        ("dwExtraInfo", ULONG_PTR),
    ]


class HARDWAREINPUT(ctypes.Structure):
    _fields_ = [
        ("uMsg", wintypes.DWORD),
        ("wParamL", wintypes.WORD),
        ("wParamH", wintypes.WORD),
    ]


class INPUTUNION(ctypes.Union):
    _fields_ = [
        ("mi", MOUSEINPUT),
        ("ki", KEYBDINPUT),
        ("hi", HARDWAREINPUT),
    ]


class INPUT(ctypes.Structure):
    _anonymous_ = ("u",)
    _fields_ = [
        ("type", wintypes.DWORD),
        ("u", INPUTUNION),
    ]


class NOTIFYICONDATAW(ctypes.Structure):
    _fields_ = [
        ("cbSize", wintypes.DWORD),
        ("hWnd", wintypes.HWND),
        ("uID", wintypes.UINT),
        ("uFlags", wintypes.UINT),
        ("uCallbackMessage", wintypes.UINT),
        ("hIcon", HICON),
        ("szTip", wintypes.WCHAR * 128),
        ("dwState", wintypes.DWORD),
        ("dwStateMask", wintypes.DWORD),
        ("szInfo", wintypes.WCHAR * 256),
        ("uTimeoutOrVersion", wintypes.UINT),
        ("szInfoTitle", wintypes.WCHAR * 64),
        ("dwInfoFlags", wintypes.DWORD),
        ("guidItem", GUID),
        ("hBalloonIcon", HICON),
    ]


class BITMAPINFOHEADER(ctypes.Structure):
    _fields_ = [
        ("biSize", wintypes.DWORD),
        ("biWidth", wintypes.LONG),
        ("biHeight", wintypes.LONG),
        ("biPlanes", wintypes.WORD),
        ("biBitCount", wintypes.WORD),
        ("biCompression", wintypes.DWORD),
        ("biSizeImage", wintypes.DWORD),
        ("biXPelsPerMeter", wintypes.LONG),
        ("biYPelsPerMeter", wintypes.LONG),
        ("biClrUsed", wintypes.DWORD),
        ("biClrImportant", wintypes.DWORD),
    ]


class BITMAPINFO(ctypes.Structure):
    _fields_ = [
        ("bmiHeader", BITMAPINFOHEADER),
        ("bmiColors", wintypes.DWORD * 1),
    ]


class BLENDFUNCTION(ctypes.Structure):
    _fields_ = [
        ("BlendOp", ctypes.c_ubyte),
        ("BlendFlags", ctypes.c_ubyte),
        ("SourceConstantAlpha", ctypes.c_ubyte),
        ("AlphaFormat", ctypes.c_ubyte),
    ]


class ICONINFO(ctypes.Structure):
    _fields_ = [
        ("fIcon", wintypes.BOOL),
        ("xHotspot", wintypes.DWORD),
        ("yHotspot", wintypes.DWORD),
        ("hbmMask", ctypes.c_void_p),
        ("hbmColor", ctypes.c_void_p),
    ]


WNDPROC = ctypes.WINFUNCTYPE(
    LRESULT, wintypes.HWND, wintypes.UINT, wintypes.WPARAM, wintypes.LPARAM
)
HOOKPROC = ctypes.WINFUNCTYPE(
    LRESULT, ctypes.c_int, wintypes.WPARAM, wintypes.LPARAM
)
TIMERPROC = ctypes.WINFUNCTYPE(
    None, wintypes.HWND, wintypes.UINT, UINT_PTR, wintypes.DWORD
)
PHANDLER_ROUTINE = ctypes.WINFUNCTYPE(wintypes.BOOL, wintypes.DWORD)


class WNDCLASSEXW(ctypes.Structure):
    _fields_ = [
        ("cbSize", wintypes.UINT),
        ("style", wintypes.UINT),
        ("lpfnWndProc", WNDPROC),
        ("cbClsExtra", ctypes.c_int),
        ("cbWndExtra", ctypes.c_int),
        ("hInstance", HINSTANCE),
        ("hIcon", HICON),
        ("hCursor", HCURSOR),
        ("hbrBackground", HBRUSH),
        ("lpszMenuName", wintypes.LPCWSTR),
        ("lpszClassName", wintypes.LPCWSTR),
        ("hIconSm", HICON),
    ]


# ---------------------------------------------------------------------------
# Win32 API 声明
# ---------------------------------------------------------------------------

user32 = ctypes.WinDLL("user32", use_last_error=True)
kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
imm32 = ctypes.WinDLL("imm32", use_last_error=True)
shell32 = ctypes.WinDLL("shell32", use_last_error=True)
ole32 = ctypes.WinDLL("ole32", use_last_error=True)
gdi32 = ctypes.WinDLL("gdi32", use_last_error=True)
shcore = ctypes.WinDLL("shcore", use_last_error=True)


def _declare_win32() -> None:
    user32.GetForegroundWindow.argtypes = []
    user32.GetForegroundWindow.restype = wintypes.HWND

    user32.GetWindowThreadProcessId.argtypes = [
        wintypes.HWND, ctypes.POINTER(wintypes.DWORD)
    ]
    user32.GetWindowThreadProcessId.restype = wintypes.DWORD

    user32.GetKeyboardLayout.argtypes = [wintypes.DWORD]
    user32.GetKeyboardLayout.restype = wintypes.HKL

    user32.SetWindowsHookExW.argtypes = [
        ctypes.c_int, HOOKPROC, HINSTANCE, wintypes.DWORD
    ]
    user32.SetWindowsHookExW.restype = HHOOK
    user32.UnhookWindowsHookEx.argtypes = [HHOOK]
    user32.UnhookWindowsHookEx.restype = wintypes.BOOL
    user32.CallNextHookEx.argtypes = [
        HHOOK, ctypes.c_int, wintypes.WPARAM, wintypes.LPARAM
    ]
    user32.CallNextHookEx.restype = LRESULT

    user32.SendInput.argtypes = [
        wintypes.UINT, ctypes.POINTER(INPUT), ctypes.c_int
    ]
    user32.SendInput.restype = wintypes.UINT
    user32.MapVirtualKeyW.argtypes = [wintypes.UINT, wintypes.UINT]
    user32.MapVirtualKeyW.restype = wintypes.UINT
    user32.GetAsyncKeyState.argtypes = [ctypes.c_int]
    user32.GetAsyncKeyState.restype = ctypes.c_short
    user32.GetCursorPos.argtypes = [ctypes.POINTER(wintypes.POINT)]
    user32.GetCursorPos.restype = wintypes.BOOL

    user32.GetMessageW.argtypes = [
        ctypes.POINTER(wintypes.MSG), wintypes.HWND,
        wintypes.UINT, wintypes.UINT
    ]
    user32.GetMessageW.restype = ctypes.c_int
    user32.TranslateMessage.argtypes = [ctypes.POINTER(wintypes.MSG)]
    user32.TranslateMessage.restype = wintypes.BOOL
    user32.DispatchMessageW.argtypes = [ctypes.POINTER(wintypes.MSG)]
    user32.DispatchMessageW.restype = LRESULT
    user32.PostQuitMessage.argtypes = [ctypes.c_int]
    user32.PostQuitMessage.restype = None
    user32.PostMessageW.argtypes = [
        wintypes.HWND, wintypes.UINT, wintypes.WPARAM, wintypes.LPARAM
    ]
    user32.PostMessageW.restype = wintypes.BOOL

    user32.RegisterClassExW.argtypes = [ctypes.POINTER(WNDCLASSEXW)]
    user32.RegisterClassExW.restype = ATOM
    user32.UnregisterClassW.argtypes = [wintypes.LPCWSTR, HINSTANCE]
    user32.UnregisterClassW.restype = wintypes.BOOL
    user32.CreateWindowExW.argtypes = [
        wintypes.DWORD, wintypes.LPCWSTR, wintypes.LPCWSTR, wintypes.DWORD,
        ctypes.c_int, ctypes.c_int, ctypes.c_int, ctypes.c_int,
        wintypes.HWND, HMENU, HINSTANCE, LPVOID,
    ]
    user32.CreateWindowExW.restype = wintypes.HWND
    user32.DestroyWindow.argtypes = [wintypes.HWND]
    user32.DestroyWindow.restype = wintypes.BOOL
    user32.DefWindowProcW.argtypes = [
        wintypes.HWND, wintypes.UINT, wintypes.WPARAM, wintypes.LPARAM
    ]
    user32.DefWindowProcW.restype = LRESULT
    user32.FindWindowW.argtypes = [wintypes.LPCWSTR, wintypes.LPCWSTR]
    user32.FindWindowW.restype = wintypes.HWND
    user32.IsWindow.argtypes = [wintypes.HWND]
    user32.IsWindow.restype = wintypes.BOOL

    user32.SetTimer.argtypes = [
        wintypes.HWND, UINT_PTR, wintypes.UINT, LPVOID
    ]
    user32.SetTimer.restype = UINT_PTR
    user32.KillTimer.argtypes = [wintypes.HWND, UINT_PTR]
    user32.KillTimer.restype = wintypes.BOOL

    user32.LoadIconW.argtypes = [HINSTANCE, wintypes.LPCWSTR]
    user32.LoadIconW.restype = HICON
    user32.LoadCursorW.argtypes = [HINSTANCE, wintypes.LPCWSTR]
    user32.LoadCursorW.restype = HCURSOR

    user32.CreatePopupMenu.argtypes = []
    user32.CreatePopupMenu.restype = HMENU
    user32.AppendMenuW.argtypes = [
        HMENU, wintypes.UINT, UINT_PTR, wintypes.LPCWSTR
    ]
    user32.AppendMenuW.restype = wintypes.BOOL
    user32.TrackPopupMenu.argtypes = [
        HMENU, wintypes.UINT, ctypes.c_int, ctypes.c_int,
        ctypes.c_int, wintypes.HWND, LPVOID,
    ]
    user32.TrackPopupMenu.restype = ctypes.c_int
    user32.DestroyMenu.argtypes = [HMENU]
    user32.DestroyMenu.restype = wintypes.BOOL
    user32.SetForegroundWindow.argtypes = [wintypes.HWND]
    user32.SetForegroundWindow.restype = wintypes.BOOL
    user32.GetDC.argtypes = [wintypes.HWND]
    user32.GetDC.restype = wintypes.HDC
    user32.ReleaseDC.argtypes = [wintypes.HWND, wintypes.HDC]
    user32.ReleaseDC.restype = ctypes.c_int
    user32.ShowWindow.argtypes = [wintypes.HWND, ctypes.c_int]
    user32.ShowWindow.restype = wintypes.BOOL
    user32.SetWindowPos.argtypes = [
        wintypes.HWND, wintypes.HWND, ctypes.c_int, ctypes.c_int,
        ctypes.c_int, ctypes.c_int, wintypes.UINT,
    ]
    user32.SetWindowPos.restype = wintypes.BOOL
    user32.UpdateLayeredWindow.argtypes = [
        wintypes.HWND, wintypes.HDC,
        ctypes.POINTER(wintypes.POINT), ctypes.POINTER(wintypes.SIZE),
        wintypes.HDC, ctypes.POINTER(wintypes.POINT),
        wintypes.DWORD, ctypes.POINTER(BLENDFUNCTION), wintypes.DWORD,
    ]
    user32.UpdateLayeredWindow.restype = wintypes.BOOL
    user32.MessageBeep.argtypes = [wintypes.UINT]
    user32.MessageBeep.restype = wintypes.BOOL
    user32.CreateIconIndirect.argtypes = [ctypes.POINTER(ICONINFO)]
    user32.CreateIconIndirect.restype = HICON
    user32.DestroyIcon.argtypes = [HICON]
    user32.DestroyIcon.restype = wintypes.BOOL
    user32.SetProcessDpiAwarenessContext.argtypes = [LPVOID]
    user32.SetProcessDpiAwarenessContext.restype = wintypes.BOOL
    user32.SetProcessDPIAware.argtypes = []
    user32.SetProcessDPIAware.restype = wintypes.BOOL
    user32.MessageBoxW.argtypes = [
        wintypes.HWND, wintypes.LPCWSTR, wintypes.LPCWSTR, wintypes.UINT
    ]
    user32.MessageBoxW.restype = ctypes.c_int

    gdi32.CreateCompatibleDC.argtypes = [wintypes.HDC]
    gdi32.CreateCompatibleDC.restype = wintypes.HDC
    gdi32.DeleteDC.argtypes = [wintypes.HDC]
    gdi32.DeleteDC.restype = wintypes.BOOL
    gdi32.CreateDIBSection.argtypes = [
        wintypes.HDC, ctypes.POINTER(BITMAPINFO), wintypes.UINT,
        ctypes.POINTER(ctypes.c_void_p), HANDLE, wintypes.DWORD,
    ]
    gdi32.CreateDIBSection.restype = ctypes.c_void_p
    gdi32.CreateBitmap.argtypes = [
        ctypes.c_int, ctypes.c_int, wintypes.UINT, wintypes.UINT,
        ctypes.c_void_p,
    ]
    gdi32.CreateBitmap.restype = ctypes.c_void_p
    gdi32.SelectObject.argtypes = [wintypes.HDC, wintypes.HGDIOBJ]
    gdi32.SelectObject.restype = wintypes.HGDIOBJ
    gdi32.DeleteObject.argtypes = [wintypes.HGDIOBJ]
    gdi32.DeleteObject.restype = wintypes.BOOL

    shcore.SetProcessDpiAwareness.argtypes = [ctypes.c_int]
    shcore.SetProcessDpiAwareness.restype = HRESULT

    imm32.ImmGetDescriptionW.argtypes = [
        wintypes.HKL, wintypes.LPWSTR, wintypes.UINT
    ]
    imm32.ImmGetDescriptionW.restype = wintypes.UINT
    imm32.ImmGetIMEFileNameW.argtypes = [
        wintypes.HKL, wintypes.LPWSTR, wintypes.UINT
    ]
    imm32.ImmGetIMEFileNameW.restype = wintypes.UINT

    shell32.Shell_NotifyIconW.argtypes = [
        wintypes.DWORD, ctypes.POINTER(NOTIFYICONDATAW)
    ]
    shell32.Shell_NotifyIconW.restype = wintypes.BOOL

    kernel32.GetModuleHandleW.argtypes = [wintypes.LPCWSTR]
    kernel32.GetModuleHandleW.restype = HINSTANCE
    kernel32.CreateMutexW.argtypes = [
        LPVOID, wintypes.BOOL, wintypes.LPCWSTR
    ]
    kernel32.CreateMutexW.restype = HANDLE
    kernel32.CloseHandle.argtypes = [HANDLE]
    kernel32.CloseHandle.restype = wintypes.BOOL
    kernel32.SetConsoleCtrlHandler.argtypes = [PHANDLER_ROUTINE, wintypes.BOOL]
    kernel32.SetConsoleCtrlHandler.restype = wintypes.BOOL

    ole32.CoInitializeEx.argtypes = [LPVOID, wintypes.DWORD]
    ole32.CoInitializeEx.restype = HRESULT
    ole32.CoUninitialize.argtypes = []
    ole32.CoUninitialize.restype = None
    ole32.CoCreateInstance.argtypes = [
        ctypes.POINTER(GUID), LPVOID, wintypes.DWORD,
        ctypes.POINTER(GUID), ctypes.POINTER(ctypes.c_void_p),
    ]
    ole32.CoCreateInstance.restype = HRESULT
    ole32.CoTaskMemFree.argtypes = [LPVOID]
    ole32.CoTaskMemFree.restype = None


_declare_win32()


# ---------------------------------------------------------------------------
# 小狼毫识别
# ---------------------------------------------------------------------------

def _text_matches_weasel(text: str, keywords=WEASEL_KEYWORDS) -> bool:
    """不区分大小写地匹配关键字。

    "rime" 使用拉丁词边界，避免把 "Prime"/"crime" 之类误判；
    "weasel" 允许出现在文件名中（weaselx64.ime）；中文关键字直接子串匹配。
    额外关键字按不区分大小写的子串匹配。
    """
    if not text:
        return False
    lowered = text.casefold()
    for keyword in keywords:
        key = keyword.casefold()
        if key == "rime":
            if re.search(r"(?<![a-z])rime(?![a-z])", lowered):
                return True
        elif key in ("weasel", "小狼毫"):
            if key in lowered:
                return True
        elif key in lowered:
            return True
    return False


_hkl_cache: dict[int, bool] = {}


def get_hkl_texts(hkl: int) -> tuple[str, str]:
    """返回 (输入法描述, IME 文件名)。不支持的 HKL 返回空字符串。"""
    if not hkl:
        return "", ""
    handle = wintypes.HKL(hkl)
    description = ctypes.create_unicode_buffer(1024)
    ime_file = ctypes.create_unicode_buffer(1024)
    try:
        imm32.ImmGetDescriptionW(handle, description, len(description))
    except Exception:
        pass
    try:
        imm32.ImmGetIMEFileNameW(handle, ime_file, len(ime_file))
    except Exception:
        pass
    return description.value, ime_file.value


def is_weasel_hkl(hkl: int) -> bool:
    """判断给定 HKL 是否对应小狼毫。

    这是需求中要求的兼容路径：通过 ImmGetDescription / ImmGetIMEFileName
    的描述或文件名关键字识别。新版 TSF 小狼毫可能返回空描述，此时由
    WeaselDetector 的 TSF 活动配置检测补足。
    """
    key = int(hkl or 0)
    if key in _hkl_cache:
        return _hkl_cache[key]
    description, ime_file = get_hkl_texts(key)
    matched = _text_matches_weasel(description) or _text_matches_weasel(ime_file)
    _hkl_cache[key] = matched
    if matched:
        log.debug("HKL %#010x 直接识别为小狼毫: desc=%r file=%r",
                  key, description, ime_file)
    return matched


def get_foreground_hkl() -> int:
    """获取当前前台窗口所属线程的键盘布局 HKL。"""
    hwnd = user32.GetForegroundWindow()
    if not hwnd:
        return 0
    thread_id = user32.GetWindowThreadProcessId(hwnd, None)
    if not thread_id:
        return 0
    return int(user32.GetKeyboardLayout(thread_id) or 0)


def discover_weasel_clsids(keywords=WEASEL_KEYWORDS) -> list[GUID]:
    """从注册表只读扫描 Weasel 的 TSF CLSID，作为活动配置检测候选。"""
    candidates: dict[str, GUID] = {}

    def add(clsid: str) -> None:
        text = clsid.strip()
        if not re.fullmatch(r"\{?[0-9A-Fa-f-]{36}\}?", text):
            return
        text = text.strip("{}").upper()
        if text not in candidates:
            try:
                candidates[text] = GUID.from_string(text)
            except ValueError:
                pass

    # 固定 CLSID 永远作为候选。
    add(WEASEL_DEFAULT_CLSID)

    ctf_roots = [
        (winreg.HKEY_LOCAL_MACHINE, r"SOFTWARE\Microsoft\CTF\TIP"),
        (winreg.HKEY_LOCAL_MACHINE, r"SOFTWARE\WOW6432Node\Microsoft\CTF\TIP"),
        (winreg.HKEY_CURRENT_USER, r"Software\Microsoft\CTF\TIP"),
    ]
    class_roots = [
        (winreg.HKEY_LOCAL_MACHINE, r"SOFTWARE\Classes\CLSID"),
        (winreg.HKEY_LOCAL_MACHINE, r"SOFTWARE\WOW6432Node\Classes\CLSID"),
        (winreg.HKEY_CURRENT_USER, r"Software\Classes\CLSID"),
    ]

    def read_value(root, path: str, name: str) -> str:
        try:
            with winreg.OpenKey(root, path, 0, winreg.KEY_READ) as key:
                value, _ = winreg.QueryValueEx(key, name)
                return str(value) if value is not None else ""
        except OSError:
            return ""

    for root, tip_root in ctf_roots:
        try:
            with winreg.OpenKey(root, tip_root, 0, winreg.KEY_READ) as tip_key:
                count = winreg.QueryInfoKey(tip_key)[0]
                clsid_names = []
                for i in range(count):
                    try:
                        clsid_names.append(winreg.EnumKey(tip_key, i))
                    except OSError:
                        break
        except OSError:
            continue

        for clsid in clsid_names:
            if not re.fullmatch(r"\{?[0-9A-Fa-f-]{36}\}?", clsid.strip()):
                continue
            # 类名 / InprocServer32 路径
            for class_root, class_path in class_roots:
                base = class_path + "\\" + clsid
                evidence = [
                    read_value(class_root, base, ""),
                    read_value(class_root, base + r"\InprocServer32", ""),
                ]
                if any(_text_matches_weasel(v, keywords) for v in evidence):
                    add(clsid)
                    break

            # LanguageProfile 中的 Description
            profile_root = tip_root + "\\" + clsid + r"\LanguageProfile"
            try:
                with winreg.OpenKey(root, profile_root, 0, winreg.KEY_READ) as pkey:
                    lang_count = winreg.QueryInfoKey(pkey)[0]
                    langs = []
                    for i in range(lang_count):
                        try:
                            langs.append(winreg.EnumKey(pkey, i))
                        except OSError:
                            break
            except OSError:
                continue

            matched = False
            for lang in langs:
                lang_path = profile_root + "\\" + lang
                try:
                    with winreg.OpenKey(root, lang_path, 0, winreg.KEY_READ) as lkey:
                        prof_count = winreg.QueryInfoKey(lkey)[0]
                        profiles = []
                        for i in range(prof_count):
                            try:
                                profiles.append(winreg.EnumKey(lkey, i))
                            except OSError:
                                break
                except OSError:
                    continue
                for profile in profiles:
                    ppath = lang_path + "\\" + profile
                    desc = read_value(root, ppath, "Description")
                    icon = read_value(root, ppath, "IconFile")
                    if _text_matches_weasel(desc, keywords) or _text_matches_weasel(icon, keywords):
                        matched = True
                        break
                if matched:
                    break
            if matched:
                add(clsid)

    result = list(candidates.values())
    log.debug("Weasel TSF CLSID 候选: %s", ", ".join(str(g) for g in result))
    return result


class TSFProfileQuery:
    """只读查询 ITfInputProcessorProfiles::GetActiveLanguageProfile。"""

    # {33C53A50-F456-4884-B049-85FD643ECFED}
    CLSID_TF_InputProcessorProfiles = GUID.from_string(
        "33C53A50-F456-4884-B049-85FD643ECFED"
    )
    # {1F02B6C5-7842-4EE6-8A0B-9A24183A95CA}
    IID_ITfInputProcessorProfiles = GUID.from_string(
        "1F02B6C5-7842-4EE6-8A0B-9A24183A95CA"
    )
    # vtable: IUnknown(3) + Register/Unregister/Add/Remove/EnumInputInfo/
    #         GetDefault/SetDefault/Activate/GetActiveLanguageProfile
    # GetActiveLanguageProfile 的下标为 3 + 8 = 11。
    _GET_ACTIVE_LANGUAGE_PROFILE_INDEX = 11

    def __init__(self, candidates: list[GUID]):
        self._candidates = candidates
        self._ptr: Optional[ctypes.c_void_p] = None
        self._release = None
        self._get_active = None
        self._co_initialized = False
        self.available = False
        self.last_error = ""

        hr = ole32.CoInitializeEx(None, COINIT_APARTMENTTHREADED)
        if hr in (S_OK, S_FALSE):
            self._co_initialized = True
        elif hr & 0xFFFFFFFF == 0x80010106:  # RPC_E_CHANGED_MODE
            self.last_error = "COM 已以其他模式初始化"
            return
        else:
            self.last_error = "CoInitializeEx HRESULT=%#x" % (hr & 0xFFFFFFFF)
            return

        ptr = ctypes.c_void_p()
        try:
            hr = ole32.CoCreateInstance(
                ctypes.byref(self.CLSID_TF_InputProcessorProfiles),
                None,
                CLSCTX_INPROC_SERVER,
                ctypes.byref(self.IID_ITfInputProcessorProfiles),
                ctypes.byref(ptr),
            )
            if hr != S_OK or not ptr:
                self.last_error = "CoCreateInstance HRESULT=%#x" % (hr & 0xFFFFFFFF)
                return
            self._ptr = ptr
            vtable = ctypes.cast(
                ptr, ctypes.POINTER(ctypes.POINTER(ctypes.c_void_p))
            ).contents
            self._release = ctypes.WINFUNCTYPE(
                wintypes.ULONG, ctypes.c_void_p
            )(vtable[2])
            self._get_active = ctypes.WINFUNCTYPE(
                HRESULT,
                ctypes.c_void_p,
                ctypes.POINTER(GUID),
                ctypes.POINTER(wintypes.WORD),
                ctypes.POINTER(GUID),
            )(vtable[self._GET_ACTIVE_LANGUAGE_PROFILE_INDEX])
            self.available = True
        except Exception as exc:  # pragma: no cover - 防御性
            self.last_error = "TSF 初始化异常: %r" % (exc,)

    def is_weasel_active(self) -> bool:
        if not self.available or self._ptr is None or self._get_active is None:
            return False
        for clsid in self._candidates:
            langid = wintypes.WORD()
            profile = GUID()
            try:
                hr = self._get_active(
                    self._ptr,
                    ctypes.byref(clsid),
                    ctypes.byref(langid),
                    ctypes.byref(profile),
                )
            except Exception as exc:  # pragma: no cover - 防御性
                log.debug("GetActiveLanguageProfile 调用异常: %r", exc)
                return False
            if hr == S_OK and not profile.is_null():
                return True
        return False

    def close(self) -> None:
        if self._release is not None and self._ptr is not None:
            try:
                self._release(self._ptr)
            except Exception:
                pass
        self._ptr = None
        self._release = None
        self._get_active = None
        self.available = False
        if self._co_initialized:
            try:
                ole32.CoUninitialize()
            except Exception:
                pass
            self._co_initialized = False


class WeaselDetector:
    """组合 HKL 与 TSF 活动配置判定当前前台是否为小狼毫。"""

    def __init__(self, use_tsf: bool = True):
        self.candidates = discover_weasel_clsids()
        self.tsf: Optional[TSFProfileQuery] = None
        if use_tsf:
            self.tsf = TSFProfileQuery(self.candidates)
            if not self.tsf.available:
                log.warning("TSF 检测不可用：%s（将仅使用 HKL 描述识别）",
                            self.tsf.last_error)
        self._last_hkl = 0

    def foreground_info(self) -> dict:
        hkl = get_foreground_hkl()
        description, ime_file = get_hkl_texts(hkl)
        langid = hkl & 0xFFFF
        is_chinese_hkl = (langid & 0x03FF) == 0x04  # LANG_CHINESE
        hkl_match = is_weasel_hkl(hkl)
        tsf_active = bool(self.tsf and self.tsf.is_weasel_active())
        return {
            "hkl": hkl,
            "description": description,
            "ime_file": ime_file,
            "langid": langid,
            "is_chinese_hkl": is_chinese_hkl,
            "hkl_match": hkl_match,
            "tsf_active": tsf_active,
        }

    def is_weasel_foreground(self) -> bool:
        info = self.foreground_info()
        if info["hkl_match"]:
            return True
        # TSF 活动配置在小狼毫安装中是全局的，切到英语键盘布局时它仍可能
        # 报告 Weasel 活动；因此只有前台 HKL 属于中文语言时才采用 TSF 结果。
        if info["is_chinese_hkl"] and info["tsf_active"]:
            return True
        return False

    def close(self) -> None:
        if self.tsf:
            self.tsf.close()
            self.tsf = None


# ---------------------------------------------------------------------------
# 输入注入：CapsLock -> 左 Shift
# ---------------------------------------------------------------------------

class ShiftInjector:
    """只发送左 Shift 的按下/抬起，绝不产生自动重复。"""

    _EXTRA_INFO = 0x52435331  # "RCS1"，便于识别本程序注入的事件

    def __init__(self):
        self._down_sent = False
        self.last_error = 0
        self._scan_code = user32.MapVirtualKeyW(VK_LSHIFT, 0) or 0x2A
        if ctypes.sizeof(INPUT) != (40 if ctypes.sizeof(ctypes.c_void_p) == 8 else 28):
            log.warning("INPUT 结构大小异常：%d（64 位应为 40）", ctypes.sizeof(INPUT))

    @property
    def is_down(self) -> bool:
        return self._down_sent

    def _send(self, keyup: bool) -> bool:
        inp = INPUT()
        inp.type = INPUT_KEYBOARD
        inp.ki.wVk = VK_LSHIFT
        inp.ki.wScan = self._scan_code
        inp.ki.dwFlags = KEYEVENTF_KEYUP if keyup else 0
        inp.ki.time = 0
        inp.ki.dwExtraInfo = self._EXTRA_INFO
        sent = user32.SendInput(1, ctypes.byref(inp), ctypes.sizeof(INPUT))
        if sent != 1:
            self.last_error = ctypes.get_last_error()
            return False
        return True

    def press(self) -> bool:
        if self._down_sent:
            return True
        if self._send(False):
            self._down_sent = True
            return True
        return False

    def release(self) -> bool:
        if not self._down_sent:
            return True
        if self._send(True):
            self._down_sent = False
            return True
        # 保留 _down_sent=True，后续清理时还会再试一次。
        return False

    def release_best_effort(self) -> None:
        self.release()


# ---------------------------------------------------------------------------
# 视觉反馈：内嵌托盘图标、光晕层
# ---------------------------------------------------------------------------

# 这两张 32x32 RGBA 图标由 D:\Backup\Downloads\rcs_icon.png 生成：
# 默认米白背景 / 锁存时琥珀色高亮。数据内嵌，运行时不需要外部 .ico 文件。
# （生成后的 base64 数据在下方常量中。）

TRAY_ICON_OFF_B64 = (
    "eNqdVwl0VOUVnuysYZGQsIU1IQkQBMNiBbQsQkU4ldUiiwUrCJWWpUXwoD1osXigWmppwdSCLAUU0KO1tZYcEKtWQZAjEFSIErLM"
    "kklmMjOZeW/e+/rd+wKipJU2J+/Mm/f++e+93/3ud+/vcjX9ByCBV9JX38Od47HaBVbU92crVnM23uAJmBG3FZeL9/IMhn+PrJG1"
    "1+yTJHu5/oc/rk+8el9fPYD7buP+HiDEV2EgXgs76oEZroYZqoLV4AZMP9/V66Vrzbpt8tum9vwW2xozKo63sGL+TTDqDID/MR+i"
    "gQorVl8Zj4erbZg1NlDH5QG55N5GvCZuRaotu4GuogGWUWvoHtzr2r2/zXY0WJZnGf6TDFRjjAUrTBhe24m/HkZ9FcovnsHJ4+/i"
    "2NHDOHrkLXz4/jGUfXpafOSaINfWm4iKHzH6UXdS9vxvPlx5boS/LLLMOo/gHA9XGU5sUVSVf4oXd+7CvAeX4eYRk9Gh961o1mkQ"
    "kjMKkZQxAGmZA9GuxzAUDLsL9y34Cfbu24f62sv8bdDQnBjcM1xZ1JQPV3JDf3OImRdWQGOOMfZjx0owddZCJLTNg8vVgVcGry5w"
    "tewDV6ucrz7T+8LVvgBJHfrD1Yb3rXLRt2g8duzaDVi1JuwgeeH3io2v2VSe7xeONrcavKc17ki1KVh7Kj7HUxs2YdnP1uLxJzZg"
    "/YZnsG79Rixcugqjxs/AhHvmMtaHeS3FuO/PRfcB373qU8uug5F4Uz+4mvfCgsUrYTa4TdnbavCcFluNNq/WmBWu2kQSC5eNUO1l"
    "3DvvIQwbNRkTpt6PSfc+gIkz5mPi9B9i+pxFWLXmF3j9tVedWmBulPeWH0aoGsfJifmLliO9y2C06DwI6d2HwJXSXX0mR5XLYuva"
    "2kTUm0dsTNZznFjZ5z45DlezHnCldXc+E7s04t4RruRucCV1hSs1G+PunoWKS+cheYLpw7kzJ7B583NY9vO16Fl4B1Iz+qsPrbsV"
    "EZMcvPrKIfIhHI9HfabYvKIL5Hix8DQWrCRGQfyTOU9sk0NODUByhwJk9BmOoWOmoN/wCUhsn4eEdnlI7TSQ/mRh1PdmKg7bd2xH"
    "astejb5m0u/u5EI+WnW9BenZQ8iffIyZPEd0whRbZqS6uFFfMplvv22wvoMVWmOvHDqg8TXvPFDjXbzsEa1ng3GWHH4THXsUIbFt"
    "X/pQyHXdcPqj93Dgpf1wJXSGq20usnKGIWfwWLTI6u/g1yYPaayVzNwRuFx21rbtOqlrv9hGg2e+5M8MV4m20E4EzxcXaxwt1H5X"
    "rF33FOLxegS8FyF/S5av1vfNutyMxJRsHDr4Eiq+OIuE5Cz8gLzx15Qj6PsSFz47ja3FL2Dk+JnELJ/c7KO+wqqxBGfq9nwr7N7L"
    "mG36E2fNaZxP/moj9+981f42+iPclN9EqD2DR0xEQsveSGV+XC164kLpKbxz9B/EvT1mzFkIM+Zv5Gatw01q89G3S3Df/Ifx+TmR"
    "NX+cz21yfS+1tFT0nDpuO/YjWLriUY1P8W/eExs2PoOTp09gHzG+bfQ9imkzeUdOTpv9I62BabMfRKJgTQ4UDB6DLX/YBp+7TH2W"
    "HiGfGnPYDdVvizbD7lLeB0Un46Kzin8IM+ct1rynML6EDv2Q3JF5JOdcxFriTc7k94ROGDtxJuoDbpw68R5roov62kx4mZ6j37Pz"
    "bsOzv91C3fEJ36jbldqr2DPpsld0Jki7lhWRZ3yvfgYwetIsrb3kjmK/QHPgat0HKeRbCn1JZm08+5vnYAtuEQ/rz0Ns9iFv4B2O"
    "H7SfJvi0ySVGnTCJuhEOVIoGaZxijzoE7d2RK/add4h5qe93M85eSGTsaVkDsPyRx5BXNJbx9UBKViESiMGCxSu0Dwtm+jviFg5W"
    "kit/RJ/+o4iV4NefvC/UenxgyUrFX2K8Yt9y7AcFC+lz0tMjteXU0ds1XhdrLKPPMEqWD9XU4uy+t2p+U+iT7Llk2WrlV5Q+CHfY"
    "e5V3gdoKTJk5nxhmO3hl9KNu9MWZj/8FzbvgHBP83UHaL5VnRqjSFjvu8vNo2/0WJ9+tc5DLuIO+L9TOB+yxzdv1RRKvVOIgWK9/"
    "+tdqMxq4rDjIp2hxiHF0zfsOEhiHrqWe/GnHDuW36oxVJ/GXMu59Un/ERbQX56mhonmJknfmYPjYqaqtDXXl+ttde3Y72DKuFMYl"
    "PHlx185GH8pZ027HB/Lorilzda3UUQLtb32+2FnHGYafWn+O/oQkj6oJ7797RLFPFY5zb+k5luXkOdpYn2see0J5JblNoh6nkGcl"
    "JX9vjO0ydbJK85nLOtR8iU606o23OaeIDcdWSPVHNJA4+C2dVUL2IWqv+NpceEPeL/jxSq3vsP8S89ZYI5wPJk+/39GorpxBmI8W"
    "1HqZg2LKgRh+v3Wbxp4mfKVW5Q+9k3FXSx3atlEjdaj6q7034i52+mjAPFLypmq69J5ExtWjYCRKz3yksQlvGuou04YHfu8l5A8a"
    "rZonPOuUPQibf7eF62y8zFpsRlwEmzTBkXEcpEYLh3SWkx5Mm1fnD+m/Rq1JLpIDAVs0VHqb6LtwILPXEOzes0fwatRVmTsNXDh/"
    "Cg8tWorDb72hvK7xlGEpe5VgnnRTPnnn1MmK1Y+jMce2FfXGOVte7b/XzR9Rj1HH/jF01CT1QXWE+AoXBlEXHv/l0/jLG6/hxIfv"
    "4NwnJ/DxqQ+wn7o8m3NQuqwVzCV3oj3Mzwpqh3DdcGZ1Q2ba6+aPa+cvu17q3fT7LmHytHmqs8IdjUV0NTVbMUlgfIm8JFadR9jv"
    "9bvULX/TmZjt3L0bTm+tRjxU1eT89c35k+cGilGd6LMpfr+wfTv6F43T/VWHxZ74ce1FfqlfvLrlDseateuoV59proSvnKOd+VNm"
    "22/Mn9edOzgjwwzoWccMVRpSEzJf/e2vr2PZykcx6s5p6FV4OzpSF+XqxTlr5LipWPLTVTh44GXUesuUq6Jlmu+I21DOyJ7/Yf5u"
    "+vxRd1L4EAtJz5J5uN6W2QB2rdaA330R/uoLei/cU5tyPqGuir4x16bovOb7Bs4fTZ+/fJvkDKXnr6jua3FOijOH1Ooa1Wuxx/za"
    "PPdwfqiME29Ln1MDoOcv3w2fv5o6K8Z4hqT/Xz9/Srzav93ay8QHB4NQ4/nT52GNbYv9H+fPGzt/e3n+dge0f3N+kHt55py/fTx/"
    "e2/4/P1vuynfeg=="
)

TRAY_ICON_ON_B64 = (
    "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"
    "mzx//w92is1i"
)
TRAY_ICON_SIZE = 32


def _decode_embedded_icon(data_b64: str) -> bytes:
    import base64
    import zlib

    raw = zlib.decompress(base64.b64decode(data_b64))
    expected = TRAY_ICON_SIZE * TRAY_ICON_SIZE * 4
    if len(raw) != expected:
        raise ValueError(f"托盘图标数据长度错误：{len(raw)} != {expected}")
    return raw


def _create_icon_from_rgba(raw_rgba: bytes, size: int = TRAY_ICON_SIZE) -> HICON:
    """把 RGBA 像素转成带 alpha 的 HICON（Windows 期望 BGRA DIB）。"""
    if len(raw_rgba) != size * size * 4:
        raise ValueError("RGBA 数据长度不匹配")

    # DIB 使用 BGRA；Pillow 输出的是 RGBA。
    bgra = bytearray(raw_rgba)
    for offset in range(0, len(bgra), 4):
        bgra[offset], bgra[offset + 2] = bgra[offset + 2], bgra[offset]

    bmi = BITMAPINFO()
    bmi.bmiHeader.biSize = ctypes.sizeof(BITMAPINFOHEADER)
    bmi.bmiHeader.biWidth = size
    bmi.bmiHeader.biHeight = -size
    bmi.bmiHeader.biPlanes = 1
    bmi.bmiHeader.biBitCount = 32
    bmi.bmiHeader.biCompression = BI_RGB

    memdc = gdi32.CreateCompatibleDC(None)
    if not memdc:
        raise OSError(ctypes.get_last_error(), "CreateCompatibleDC 失败")
    bits = ctypes.c_void_p()
    color_bmp = gdi32.CreateDIBSection(
        None, ctypes.byref(bmi), DIB_RGB_COLORS, ctypes.byref(bits), None, 0
    )
    if not color_bmp or not bits:
        gdi32.DeleteDC(memdc)
        raise OSError(ctypes.get_last_error(), "CreateDIBSection 失败")
    ctypes.memmove(bits, bytes(bgra), len(bgra))
    old_bmp = gdi32.SelectObject(memdc, color_bmp)

    # 生成 1bpp AND 掩码，确保旧版 Shell 也能正确显示透明区域。
    stride = ((size + 15) // 16) * 2
    mask_bytes = bytearray(stride * size)
    for y in range(size):
        for x in range(size):
            if bgra[(y * size + x) * 4 + 3] == 0:
                mask_bytes[y * stride + x // 8] |= 1 << (7 - (x % 8))
    mask_buf = ctypes.create_string_buffer(bytes(mask_bytes))
    mask_bmp = gdi32.CreateBitmap(size, size, 1, 1, mask_buf)

    icon_info = ICONINFO(True, 0, 0, mask_bmp, color_bmp)
    hicon = user32.CreateIconIndirect(ctypes.byref(icon_info))
    gdi32.SelectObject(memdc, old_bmp)
    if color_bmp:
        gdi32.DeleteObject(color_bmp)
    if mask_bmp:
        gdi32.DeleteObject(mask_bmp)
    gdi32.DeleteDC(memdc)

    if not hicon:
        raise OSError(ctypes.get_last_error(), "CreateIconIndirect 失败")
    return hicon


def create_tray_icons() -> tuple[HICON, HICON, str]:
    """返回 (默认 HICON, 高亮 HICON, 状态说明)。失败时回退到系统图标。"""
    off_icon = HICON()
    on_icon = HICON()
    try:
        off_raw = _decode_embedded_icon(TRAY_ICON_OFF_B64)
        on_raw = _decode_embedded_icon(TRAY_ICON_ON_B64)
        off_icon = _create_icon_from_rgba(off_raw)
        on_icon = _create_icon_from_rgba(on_raw)
        return off_icon, on_icon, "可用（内嵌双色图标）"
    except Exception as exc:
        if off_icon:
            try:
                user32.DestroyIcon(off_icon)
            except Exception:
                pass
        if on_icon:
            try:
                user32.DestroyIcon(on_icon)
            except Exception:
                pass
        log.warning("生成双色托盘图标失败，回退到系统图标：%r", exc)
        # IDI_APPLICATION = 32512
        try:
            fallback = user32.LoadIconW(
                None, ctypes.cast(ctypes.c_void_p(32512), wintypes.LPCWSTR)
            )
        except Exception:
            fallback = HICON()
        return fallback, fallback, f"不可用（{exc}）"


def enable_dpi_awareness() -> bool:
    """尽量启用 Per-Monitor V2，保证 GetCursorPos 和光效窗口使用物理像素。"""
    try:
        if user32.SetProcessDpiAwarenessContext(
            DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2
        ):
            log.debug("DPI 感知：Per-Monitor V2")
            return True
    except Exception:
        pass
    try:
        hr = shcore.SetProcessDpiAwareness(2)  # PROCESS_PER_MONITOR_DPI_AWARE
        if hr in (0, 1):
            log.debug("DPI 感知：Per-Monitor V1")
            return True
    except Exception:
        pass
    try:
        if user32.SetProcessDPIAware():
            log.debug("DPI 感知：System DPI aware")
            return True
    except Exception:
        pass
    log.warning("无法启用 DPI 感知，混合 DPI 显示器下光效位置可能不准确")
    return False


def parse_halo_color(value: str) -> tuple[int, int, int]:
    text = (value or "").strip().lstrip("#")
    if not re.fullmatch(r"[0-9A-Fa-f]{6}", text):
        raise ValueError("颜色必须是 RRGGBB 六位十六进制")
    return int(text[0:2], 16), int(text[2:4], 16), int(text[4:6], 16)


def _smoothstep(edge0: float, edge1: float, x: float) -> float:
    if edge1 == edge0:
        return 1.0 if x >= edge1 else 0.0
    t = max(0.0, min(1.0, (x - edge0) / (edge1 - edge0)))
    return t * t * (3.0 - 2.0 * t)


class HaloOverlay:
    """鼠标周围的点击穿透、不抢焦点呼吸光晕。"""

    def __init__(self, hinstance, enabled: bool = True,
                 color: tuple[int, int, int] = (255, 179, 71),
                 size: int = HALO_DEFAULT_SIZE):
        self.hinstance = hinstance
        self.enabled = enabled
        self.available = False
        self.reason = ""
        self.size = size
        self.color = color
        self.hwnd = None
        self.memdc = None
        self.bitmap = None
        self.old_bitmap = None
        self._wndproc_ref = None
        self._visible = False
        self._closed = False
        self._phase = 0.0
        if not enabled:
            self.reason = "已禁用（--no-halo）"
            return
        self._create()
        if self.hwnd:
            self.available = True
            self.reason = f"可用（#{color[0]:02X}{color[1]:02X}{color[2]:02X}, {size}px）"

    def _create(self) -> None:
        try:
            self._build_bitmap()
            self._create_window()
        except Exception as exc:
            self.reason = f"初始化失败（{exc}）"
            log.warning("光效初始化失败，降级为仅托盘变色：%s", self.reason)
            self._release_resources()
            self.available = False

    def _build_bitmap(self) -> None:
        size = self.size
        half = size / 2.0
        r0, g0, b0 = self.color
        pixels = bytearray(size * size * 4)
        for y in range(size):
            dy = (y + 0.5 - half) / half
            for x in range(size):
                dx = (x + 0.5 - half) / half
                d = math.hypot(dx, dy)
                if d >= 1.0:
                    alpha = 0
                else:
                    rise = _smoothstep(0.42, 0.62, d)
                    fall = 1.0 - _smoothstep(0.85, 1.0, d)
                    alpha = int(round(255 * rise * fall))
                    alpha = max(0, min(255, alpha))
                i = (y * size + x) * 4
                # 预乘 alpha，供 UpdateLayeredWindow 使用。
                pixels[i] = (b0 * alpha) // 255
                pixels[i + 1] = (g0 * alpha) // 255
                pixels[i + 2] = (r0 * alpha) // 255
                pixels[i + 3] = alpha

        bmi = BITMAPINFO()
        bmi.bmiHeader.biSize = ctypes.sizeof(BITMAPINFOHEADER)
        bmi.bmiHeader.biWidth = size
        bmi.bmiHeader.biHeight = -size
        bmi.bmiHeader.biPlanes = 1
        bmi.bmiHeader.biBitCount = 32
        bmi.bmiHeader.biCompression = BI_RGB

        self.memdc = gdi32.CreateCompatibleDC(None)
        if not self.memdc:
            raise OSError(ctypes.get_last_error(), "CreateCompatibleDC 失败")
        bits = ctypes.c_void_p()
        self.bitmap = gdi32.CreateDIBSection(
            None, ctypes.byref(bmi), DIB_RGB_COLORS, ctypes.byref(bits), None, 0
        )
        if not self.bitmap or not bits:
            raise OSError(ctypes.get_last_error(), "CreateDIBSection 失败")
        ctypes.memmove(bits, bytes(pixels), len(pixels))
        self.old_bitmap = gdi32.SelectObject(self.memdc, self.bitmap)

    def _create_window(self) -> None:
        self._wndproc_ref = WNDPROC(self._wndproc)
        wc = WNDCLASSEXW()
        wc.cbSize = ctypes.sizeof(WNDCLASSEXW)
        wc.lpfnWndProc = self._wndproc_ref
        wc.hInstance = self.hinstance
        wc.lpszClassName = HALO_CLASS
        if not user32.RegisterClassExW(ctypes.byref(wc)):
            err = ctypes.get_last_error()
            if err != 1410:
                raise OSError(err, "RegisterClassExW 失败")
        ex_style = (
            WS_EX_LAYERED | WS_EX_TRANSPARENT | WS_EX_TOOLWINDOW
            | WS_EX_NOACTIVATE | WS_EX_TOPMOST
        )
        self.hwnd = user32.CreateWindowExW(
            ex_style, HALO_CLASS, "", WS_POPUP,
            0, 0, self.size, self.size, None, None, self.hinstance, None
        )
        if not self.hwnd:
            raise OSError(ctypes.get_last_error(), "CreateWindowExW 失败")

    def _wndproc(self, hwnd, msg, wparam, lparam):
        try:
            if msg == WM_NCHITTEST:
                return HTTRANSPARENT
            if msg == WM_MOUSEACTIVATE:
                return MA_NOACTIVATE
        except Exception:
            pass
        return user32.DefWindowProcW(hwnd, msg, wparam, lparam)

    def set_visible(self, visible: bool) -> None:
        if not self.available:
            return
        visible = bool(visible)
        if visible == self._visible:
            if visible:
                self._render()
            return
        self._visible = visible
        if visible:
            try:
                user32.ShowWindow(self.hwnd, SW_SHOWNOACTIVATE)
                user32.SetWindowPos(
                    self.hwnd, HWND_TOPMOST, 0, 0, self.size, self.size,
                    SWP_NOMOVE | SWP_NOSIZE | SWP_NOACTIVATE | SWP_SHOWWINDOW,
                )
                self._phase = 0.0
                self._render()
            except Exception:
                log.exception("显示光效窗口失败")
        else:
            try:
                user32.ShowWindow(self.hwnd, SW_HIDE)
            except Exception:
                pass

    def tick(self) -> None:
        if not self.available or not self._visible:
            return
        self._phase = (self._phase + (2.0 * math.pi) / 32.0) % (2.0 * math.pi)
        self._render()

    def _render(self) -> None:
        if not self.available or not self._visible or not self.hwnd:
            return
        pt = wintypes.POINT()
        if not user32.GetCursorPos(ctypes.byref(pt)):
            return
        # 呼吸周期约 1.6 秒，透明度 155..255。
        alpha = int(round(205 + 50 * math.sin(self._phase)))
        blend = BLENDFUNCTION(AC_SRC_OVER, 0, max(1, min(255, alpha)), AC_SRC_ALPHA)
        dst = wintypes.POINT(pt.x - self.size // 2, pt.y - self.size // 2)
        sz = wintypes.SIZE(self.size, self.size)
        src = wintypes.POINT(0, 0)
        screen_dc = user32.GetDC(None)
        if not screen_dc:
            return
        try:
            ok = user32.UpdateLayeredWindow(
                self.hwnd, screen_dc, ctypes.byref(dst), ctypes.byref(sz),
                self.memdc, ctypes.byref(src), 0, ctypes.byref(blend), ULW_ALPHA,
            )
        finally:
            user32.ReleaseDC(None, screen_dc)
        if not ok:
            log.warning("UpdateLayeredWindow 失败，关闭光效：WinError=%s",
                        ctypes.get_last_error())
            self.available = False
            self.reason = "运行时 UpdateLayeredWindow 失败"
            try:
                user32.ShowWindow(self.hwnd, SW_HIDE)
            except Exception:
                pass
            self._visible = False

    def _release_resources(self) -> None:
        if self.memdc and self.old_bitmap:
            try:
                gdi32.SelectObject(self.memdc, self.old_bitmap)
            except Exception:
                pass
            self.old_bitmap = None
        if self.bitmap:
            try:
                gdi32.DeleteObject(self.bitmap)
            except Exception:
                pass
            self.bitmap = None
        if self.memdc:
            try:
                gdi32.DeleteDC(self.memdc)
            except Exception:
                pass
            self.memdc = None
        if self.hwnd:
            try:
                user32.DestroyWindow(self.hwnd)
            except Exception:
                pass
            self.hwnd = None

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True
        try:
            if self.hwnd:
                user32.ShowWindow(self.hwnd, SW_HIDE)
        except Exception:
            pass
        self._release_resources()
        try:
            user32.UnregisterClassW(HALO_CLASS, self.hinstance)
        except Exception:
            pass
        self.available = False

    def status_text(self) -> str:
        if not self.enabled:
            return "光效：已禁用（--no-halo）"
        return f"光效：{self.reason}"


class VisualState:
    """统一驱动托盘图标颜色和光晕显隐。"""

    def __init__(self, tip_provider):
        self._tip_provider = tip_provider
        self.latched = False
        self.tray: Optional[TrayIcon] = None
        self.halo: Optional[HaloOverlay] = None

    def set_latched(self, on: bool) -> None:
        self.latched = bool(on)
        self.sync()

    def sync(self) -> None:
        if self.tray is not None:
            try:
                self.tray.set_state(self.latched, self._tip_provider())
            except Exception:
                log.exception("更新托盘视觉状态失败")
        if self.halo is not None:
            try:
                self.halo.set_visible(self.latched)
            except Exception:
                log.exception("更新光晕状态失败")

    def shutdown(self) -> None:
        if self.halo is not None:
            try:
                self.halo.close()
            except Exception:
                log.exception("关闭光效失败")
            self.halo = None


# ---------------------------------------------------------------------------
# 状态机与键盘处理
# ---------------------------------------------------------------------------

class KeyboardHandler:
    """维护输入法中文状态与 CapsLock / Shift 行为。

    约定：
      * ``_chinese`` 只由 ``_flip_chinese()`` 修改；初始化和输入法切换通过
        它的 ``force`` 参数重置状态。
      * CapsLock 只负责吞键和注入左 Shift 的锁存状态，不修改 ``_chinese``。
      * 被监听的 Shift 短按只通过 ``_flip_chinese()`` 翻转 ``_chinese``，
        不吞键；从中文翻到英文时，``_flip_chinese()`` 会为安全释放注入 Shift。
      * 钩子回调内不做 TSF/COM 查询、日志或 I/O；日志、托盘/光效和提示音
        都由 ``flush_pending_ui()`` 在定时器中处理。
    """

    def __init__(self, detector: WeaselDetector, injector: ShiftInjector,
                 visual: Optional[VisualState] = None,
                 beep: bool = False,
                 shift_key: str = "right",
                 shift_threshold_ms: int = 500):
        self.detector = detector
        self.injector = injector
        self.visual = visual
        self.beep = beep
        self.shift_key = shift_key if shift_key in ("right", "left", "both") else "right"
        self.shift_threshold_ms = 500
        try:
            value = int(shift_threshold_ms)
            if value > 0:
                self.shift_threshold_ms = value
        except (TypeError, ValueError):
            pass
        self._weasel_foreground = False
        self._chinese = False
        self._caps_down = False
        self._caps_intercepted = False
        self._space_down = False
        self._shift_latched = False

        # 物理 Shift 状态：只要钩子看到左右 Shift 的物理按键就更新，与
        # --shift-key 是否监听无关。CapsLock 锁存注入的 Shift 在钩子入口
        # 已按 LLKHF_INJECTED 过滤，因此不会污染这里。
        self._user_lshift_down = False
        self._user_rshift_down = False

        # 一个 Shift 周期共用的状态；_shift_vk 记录本周期被监听的那个键。
        self._rshift_down_time = 0.0
        self._rshift_used = False
        self._shift_vk = 0

        # 只在定时器中消费的 UI / 日志缓冲，保证钩子内不做这些操作。
        self._pending_logs: list[tuple[int, str]] = []
        self._visual_dirty = True
        self._visual_latched = False
        self._pending_beep = False

    @property
    def chinese(self) -> bool:
        return self._chinese

    @property
    def weasel_foreground(self) -> bool:
        return self._weasel_foreground

    @property
    def shift_latched(self) -> bool:
        return self._shift_latched

    def initialize(self) -> bool:
        self._weasel_foreground = self.detector.is_weasel_foreground()
        self._reset_shift_cycle()
        # 启动时如果用户已经按住 Shift，钩子看不到之前的 key-down；此时
        # 还没有 CapsLock 锁存注入，因此 GetAsyncKeyState 可以安全地用来
        # 初始化物理 Shift 状态。
        self._user_lshift_down = bool(user32.GetAsyncKeyState(VK_LSHIFT) & 0x8000)
        self._user_rshift_down = bool(user32.GetAsyncKeyState(VK_RSHIFT) & 0x8000)
        reason = ("启动检测（当前为小狼毫）"
                  if self._weasel_foreground else "启动检测（当前不是小狼毫）")
        self._flip_chinese(reason, force=self._weasel_foreground)
        return self._weasel_foreground

    def refresh_foreground(self) -> bool:
        """轮询前台输入法；返回“状态是否发生变化”。"""
        now = self.detector.is_weasel_foreground()
        if now == self._weasel_foreground:
            if not now:
                self.ensure_shift_release()
                self._set_latched_visual(False)
            return False
        self._weasel_foreground = now
        # 输入法切换后，旧的 Shift 计时/污染周期不再可信；但物理 Shift
        # 是否按下与输入法无关，不能清零，否则 Ctrl+Shift+Space 会误翻转。
        self._reset_shift_cycle()
        if now:
            self._flip_chinese("输入法切换到小狼毫", force=True)
        else:
            self._flip_chinese("输入法离开小狼毫", force=False)
        return True

    def _queue_log(self, level: int, message: str) -> None:
        """仅把消息放入内存队列；供钩子回调安全调用。"""
        if len(self._pending_logs) < 128:
            self._pending_logs.append((level, message))

    def flush_pending_ui(self) -> None:
        """在钩子外消费日志、托盘/光效更新和提示音。"""
        if self._visual_dirty:
            self._visual_dirty = False
            if self.visual is not None:
                try:
                    self.visual.set_latched(self._visual_latched)
                except Exception:
                    log.exception("更新锁存视觉状态失败")
        if self._pending_beep:
            self._pending_beep = False
            self._beep()
        pending = self._pending_logs
        self._pending_logs = []
        for level, message in pending:
            log.log(level, message)

    def _flip_chinese(self, reason: str, force: Optional[bool] = None) -> None:
        """唯一修改 ``_chinese`` 的入口。"""
        new_state = (not self._chinese) if force is None else bool(force)
        self._chinese = new_state
        self._release_latched_shift(reason)
        self._set_latched_visual(self._chinese and self._shift_latched)
        # 即使锁存视觉未变化，提示文本也随中文/英文状态变化，必须标记刷新。
        self._visual_dirty = True
        self._queue_log(logging.INFO, f"{reason}：is_rime_chinese={self._chinese}")

    def _set_latched_visual(self, on: bool) -> None:
        # 不在钩子内直接操作托盘/光效；仅标记脏状态，由定时器应用。
        desired = bool(on and self._chinese)
        if desired != self._visual_latched:
            self._visual_latched = desired
            self._visual_dirty = True

    def _release_latched_shift(self, reason: str) -> None:
        was_latched = self._shift_latched
        self._shift_latched = False
        if self.injector.is_down:
            if not self.injector.release():
                self._queue_log(
                    logging.WARNING,
                    f"Shift Up 发送失败（WinError={self.injector.last_error}），稍后重试",
                )
        self._set_latched_visual(False)
        if was_latched:
            self._queue_log(logging.INFO, f"解除 Shift 锁定：{reason}")

    def _beep(self) -> None:
        if self.beep:
            try:
                user32.MessageBeep(MB_OK)
            except Exception:
                pass

    def _ctrl_is_down(self) -> bool:
        ctrl = bool(user32.GetAsyncKeyState(VK_CONTROL) & 0x8000)
        alt = bool(user32.GetAsyncKeyState(VK_MENU) & 0x8000)
        return ctrl and not alt

    def _any_user_shift_down(self) -> bool:
        return self._user_lshift_down or self._user_rshift_down

    def _handle_space(self, is_down: bool) -> None:
        if is_down:
            if self._space_down:
                return  # 自动重复，只翻转一次
            self._space_down = True
            if not self._ctrl_is_down():
                return
            # 只依赖 250ms 轮询维护的缓存，不在钩子内查询 TSF。
            if not self._weasel_foreground:
                return
            # Ctrl+Shift+Space 跳过。只用物理 Shift 状态，不用
            # GetAsyncKeyState——后者会把 CapsLock 锁存注入的左 Shift
            # 误判为用户按着 Shift，导致锁存状态下 Ctrl+Space 无效。
            if self._any_user_shift_down():
                return
            self._flip_chinese("Ctrl+Space")
        else:
            self._space_down = False

    def _shift_is_monitored(self, vk_code: int) -> bool:
        if self.shift_key == "left":
            return vk_code == VK_LSHIFT
        if self.shift_key == "both":
            return vk_code in (VK_LSHIFT, VK_RSHIFT)
        return vk_code == VK_RSHIFT

    def _reset_shift_cycle(self) -> None:
        self._rshift_down_time = 0.0
        self._rshift_used = False
        self._shift_vk = 0

    def _handle_shift_down(self, vk_code: int) -> None:
        if self._rshift_down_time > 0:
            return
        if not self._shift_is_monitored(vk_code):
            return
        self._rshift_down_time = time.monotonic()
        self._rshift_used = False
        self._shift_vk = vk_code

    def _handle_shift_up(self, vk_code: int) -> None:
        if self._rshift_down_time <= 0 or vk_code != self._shift_vk:
            return
        started = self._rshift_down_time
        used = self._rshift_used
        self._reset_shift_cycle()
        elapsed = time.monotonic() - started
        if (self._weasel_foreground and not used
                and elapsed < self.shift_threshold_ms / 1000.0):
            which = "左 Shift" if vk_code == VK_LSHIFT else "右 Shift"
            self._flip_chinese(f"{which}短按 {int(elapsed * 1000)}ms")

    def _handle_caps(self, is_down: bool) -> bool:
        """CapsLock 只锁存/解除注入的左 Shift，不修改 ``_chinese``。"""
        if is_down:
            if self._caps_down:
                return self._caps_intercepted  # Caps 自动重复
            self._caps_down = True
            if not self._chinese:
                self._caps_intercepted = False
                return False
            # 中文模式下始终吞掉 CapsLock，避免重新触发小狼毫的编辑键问题。
            self._caps_intercepted = True
            if self._shift_latched:
                self._shift_latched = False
                if not self.injector.release():
                    self._queue_log(
                        logging.WARNING,
                        f"Shift Up 发送失败（WinError={self.injector.last_error}），稍后重试",
                    )
                self._set_latched_visual(False)
                self._pending_beep = True
                self._queue_log(logging.INFO, "CapsLock：解除 Shift 锁定")
            else:
                if self.injector.press():
                    self._shift_latched = True
                    self._set_latched_visual(True)
                    self._pending_beep = True
                    self._queue_log(logging.INFO, "CapsLock：锁定 Shift")
                else:
                    self._queue_log(
                        logging.WARNING,
                        f"Shift Down 发送失败（WinError={self.injector.last_error}）；"
                        "本次 CapsLock 仍被吞掉",
                    )
            return True
        else:
            self._caps_down = False
            if self._caps_intercepted:
                self._caps_intercepted = False
                return True
            return False

    def handle_key(self, vk_code: int, is_down: bool) -> bool:
        """处理一个物理按键；返回 True 时钩子吞掉该事件。"""
        # 无条件追踪物理左右 Shift 状态，供 Ctrl+Space 组合键判定使用。
        # 这必须发生在任何吞键/翻转逻辑之前，且不依赖 --shift-key。
        # _hook_proc 已过滤 LLKHF_INJECTED，所以这里不会把注入 Shift 当成物理键。
        if vk_code == VK_LSHIFT:
            self._user_lshift_down = bool(is_down)
        elif vk_code == VK_RSHIFT:
            self._user_rshift_down = bool(is_down)

        # Shift 周期内的污染标记必须先于 CapsLock/Space 处理。
        if is_down and vk_code not in (VK_LSHIFT, VK_RSHIFT, VK_SHIFT):
            if self._rshift_down_time > 0:
                self._rshift_used = True
        if vk_code in (VK_LSHIFT, VK_RSHIFT):
            if is_down:
                self._handle_shift_down(vk_code)
            else:
                self._handle_shift_up(vk_code)
            return False
        if vk_code == VK_CAPITAL:
            return self._handle_caps(is_down)
        if vk_code == VK_SPACE:
            self._handle_space(is_down)
        return False

    def shutdown(self) -> None:
        self._caps_down = False
        self._caps_intercepted = False
        self._space_down = False
        self._shift_latched = False
        self._reset_shift_cycle()
        self._user_lshift_down = False
        self._user_rshift_down = False
        self.injector.release_best_effort()
        self._set_latched_visual(False)

    def ensure_shift_release(self) -> None:
        """定时重试保护：Shift 不该锁定时，若之前释放失败则再试一次。"""
        if not self.injector.is_down:
            if not self._chinese:
                self._set_latched_visual(False)
            return
        if self._chinese and self._shift_latched:
            return
        self._shift_latched = False
        if self.injector.release():
            self._set_latched_visual(False)
        else:
            self._queue_log(
                logging.WARNING,
                f"补发 Shift Up 失败（WinError={self.injector.last_error}）",
            )

    def sync_visual(self) -> None:
        """周期性同步托盘图标和光效状态。"""
        self._set_latched_visual(self._chinese and self._shift_latched)
        # 定期强制刷新提示文本，防止仅输入法状态变化时遗漏。
        self._visual_dirty = True
        self.flush_pending_ui()

    def status_short(self) -> str:
        if not self._weasel_foreground:
            return "非小狼毫，CapsLock 正常"
        if self._chinese:
            if self._shift_latched:
                return "小狼毫 中文，Shift 已锁定"
            return "小狼毫 中文，CapsLock 锁定 Shift"
        return "小狼毫 英文，CapsLock 正常"

    def status_menu(self) -> str:
        if not self._weasel_foreground:
            return "当前：非小狼毫 · CapsLock 保持原样"
        if self._chinese:
            latch = "Shift 已锁定" if self._shift_latched else "Shift 未锁定"
            return f"当前：小狼毫 · 中文模式（{latch}）"
        return "当前：小狼毫 · 英文模式（CapsLock 保持原样）"


# ---------------------------------------------------------------------------
# 开机自启动（仅 HKCU，普通用户权限）
# ---------------------------------------------------------------------------

def _is_compiled_binary() -> bool:
    return bool(
        getattr(sys, "frozen", False)
        or "__compiled__" in globals()
        or not os.path.basename(sys.executable).lower().startswith("python")
    )


def _autostart_args() -> list[str]:
    """复用当前参数，但去掉一次性/调试参数。"""
    result = []
    skip_next = False
    for arg in sys.argv[1:]:
        if skip_next:
            skip_next = False
            continue
        if arg in ("--debug", "--diagnose", "--quit", "--show-tray"):
            continue
        if arg == "--log-file":
            skip_next = True
            continue
        if arg.startswith("--log-file="):
            continue
        result.append(arg)
    return result


def _autostart_command() -> str:
    import subprocess

    if _is_compiled_binary():
        executable = os.path.abspath(sys.argv[0]) if sys.argv else ""
        if not executable or not os.path.isfile(executable):
            executable = sys.executable
        parts = [executable]
    else:
        script = os.path.abspath(__file__)
        executable = sys.executable
        pythonw = os.path.join(os.path.dirname(executable), "pythonw.exe")
        if os.path.isfile(pythonw):
            executable = pythonw
        parts = [executable, script]
    return subprocess.list2cmdline(parts + _autostart_args())


def read_autostart_command() -> str:
    try:
        with winreg.OpenKey(
            winreg.HKEY_CURRENT_USER, AUTOSTART_KEY, 0, winreg.KEY_READ
        ) as key:
            value, _ = winreg.QueryValueEx(key, AUTOSTART_VALUE)
            return str(value)
    except FileNotFoundError:
        return ""
    except OSError:
        return ""


def is_autostart_enabled() -> bool:
    return bool(read_autostart_command())


def set_autostart_enabled(enabled: bool) -> bool:
    try:
        if enabled:
            command = _autostart_command()
            if not command:
                return False
            with winreg.CreateKeyEx(
                winreg.HKEY_CURRENT_USER, AUTOSTART_KEY, 0, winreg.KEY_SET_VALUE
            ) as key:
                winreg.SetValueEx(key, AUTOSTART_VALUE, 0, winreg.REG_SZ, command)
            log.info("已开启开机自启动：%s", command)
        else:
            try:
                with winreg.OpenKey(
                    winreg.HKEY_CURRENT_USER, AUTOSTART_KEY, 0, winreg.KEY_SET_VALUE
                ) as key:
                    winreg.DeleteValue(key, AUTOSTART_VALUE)
            except FileNotFoundError:
                pass
            log.info("已关闭开机自启动")
        return True
    except OSError as exc:
        log.warning("更新开机自启动设置失败：%s", exc)
        return False


# ---------------------------------------------------------------------------
# 托盘图标与隐藏窗口
# ---------------------------------------------------------------------------

class TrayIcon:
    def __init__(self, hwnd: wintypes.HWND, default_icon: HICON,
                 active_icon: HICON, color_available: bool):
        self.hwnd = hwnd
        self.default_icon = default_icon
        self.active_icon = active_icon
        self.color_available = color_available
        self.active = False
        self.added = False
        self._last_tip = ""
        self.data = NOTIFYICONDATAW()
        self.data.cbSize = ctypes.sizeof(NOTIFYICONDATAW)
        self.data.hWnd = hwnd
        self.data.uID = TRAY_UID
        self.data.uFlags = NIF_MESSAGE | NIF_ICON | NIF_TIP
        self.data.uCallbackMessage = WM_APP_TRAY
        self.data.hIcon = default_icon
        self.data.szTip = "RimeCapsShift"

    def _icon_for(self, active: bool) -> HICON:
        if not self.color_available:
            return self.default_icon
        return self.active_icon if active else self.default_icon

    def add(self, tip: str, active: bool = False) -> bool:
        self.active = bool(active)
        self.set_tip_text(tip)
        self.data.hIcon = self._icon_for(self.active)
        ok = bool(shell32.Shell_NotifyIconW(NIM_ADD, ctypes.byref(self.data)))
        self.added = ok
        if ok:
            self._last_tip = tip
        else:
            log.warning("添加托盘图标失败，WinError=%s；"
                        "仍可用 --quit 或 Ctrl+C 退出", ctypes.get_last_error())
        return ok

    def set_tip_text(self, tip: str) -> None:
        self.data.szTip = tip[:127]

    def set_state(self, active: bool, tip: str) -> None:
        """锁存状态变化时同时刷新图标颜色和提示文本（NIM_MODIFY）。"""
        active = bool(active)
        if not self.added:
            self.active = active
            self._last_tip = tip
            return
        if active == self.active and tip == self._last_tip:
            return
        changed_icon = active != self.active
        self.active = active
        self.set_tip_text(tip)
        self.data.hIcon = self._icon_for(active)
        self.data.uFlags = NIF_ICON | NIF_TIP
        if not shell32.Shell_NotifyIconW(NIM_MODIFY, ctypes.byref(self.data)):
            log.debug("更新托盘图标失败，WinError=%s", ctypes.get_last_error())
        else:
            self._last_tip = tip
        self.data.uFlags = NIF_MESSAGE | NIF_ICON | NIF_TIP
        if changed_icon:
            log.debug("托盘图标切换为%s", "高亮" if active else "默认")

    def update(self, tip: str) -> None:
        self.set_state(self.active, tip)

    def remove(self) -> None:
        if self.added:
            try:
                shell32.Shell_NotifyIconW(NIM_DELETE, ctypes.byref(self.data))
            except Exception:
                pass
            self.added = False


class RimeCapsShiftApp:
    """创建隐藏窗口、钩子、托盘、光效与消息循环。"""

    def __init__(self, use_tsf: bool = True, no_tray: bool = False,
                 no_halo: bool = False, halo_color: str = HALO_DEFAULT_COLOR,
                 halo_size: int = HALO_DEFAULT_SIZE, beep: bool = False,
                 shift_key: str = "right", shift_threshold_ms: int = 500):
        self.use_tsf = use_tsf
        self.no_tray = no_tray
        self.no_halo = no_halo
        self.beep = beep
        self.shift_key = shift_key if shift_key in ("right", "left", "both") else "right"
        self.shift_threshold_ms = 500
        try:
            value = int(shift_threshold_ms)
            if value > 0:
                self.shift_threshold_ms = value
        except (TypeError, ValueError):
            pass
        if self.shift_threshold_ms != shift_threshold_ms:
            log.warning("--shift-threshold %s 无效，使用默认值 500ms",
                        shift_threshold_ms)
        try:
            self.halo_color = parse_halo_color(halo_color)
        except ValueError as exc:
            log.warning("无效的 --halo-color %r：%s；改用默认值 #%s",
                        halo_color, exc, HALO_DEFAULT_COLOR)
            self.halo_color = parse_halo_color(HALO_DEFAULT_COLOR)
        self.halo_size = max(HALO_MIN_SIZE, min(HALO_MAX_SIZE, int(halo_size)))
        if self.halo_size != int(halo_size):
            log.warning("--halo-size %s 超出范围，已调整为 %s px",
                        halo_size, self.halo_size)
        self.detector = WeaselDetector(use_tsf=use_tsf)
        self.injector = ShiftInjector()
        self.visual = VisualState(lambda: self._tray_tip())
        self.handler = KeyboardHandler(
            self.detector, self.injector, visual=self.visual, beep=beep,
            shift_key=self.shift_key,
            shift_threshold_ms=self.shift_threshold_ms,
        )
        self.hwnd = None
        self.tray: Optional[TrayIcon] = None
        self.halo: Optional[HaloOverlay] = None
        self._hook: Optional[HHOOK] = None
        self._hook_proc_ref = None
        self._wndproc_ref = None
        self._ctrl_handler_ref = None
        self._timer_id = 0
        self._halo_timer_id = 0
        self._mutex = None
        self._class_registered = False
        self._quitting = False
        self._closed = False
        self._menu_open = False
        self._poll_ticks = 0
        self._hook_error_message = ""
        self._tray_color_available = False
        self._tray_color_reason = "未初始化"
        self._icon_default = HICON()
        self._icon_active = HICON()
        self._custom_icons = False
        self._hinstance = kernel32.GetModuleHandleW(None)

    def run(self) -> int:
        mutex = kernel32.CreateMutexW(None, False, MUTEX_NAME)
        last_error = ctypes.get_last_error()
        if not mutex:
            raise OSError(last_error, "CreateMutexW 失败")
        self._mutex = mutex
        if last_error == ERROR_ALREADY_EXISTS:
            log.info("RimeCapsShift 已在运行，退出本次启动")
            self._close_mutex()
            return 0

        self.handler.initialize()
        try:
            self._create_window()
            self._create_halo()
            self._install_hook()
            self._install_console_handler()
            self._create_tray()
            self.visual.sync()
            self.handler.flush_pending_ui()
            self._start_timers()

            log.info("RimeCapsShift 已启动。托盘菜单可退出；也可运行 "
                     "`python rime_caps_shift.py --quit`。")
            self._message_loop()
        except KeyboardInterrupt:
            log.info("收到 KeyboardInterrupt")
        finally:
            self.close()
        return 0

    def _create_window(self) -> None:
        self._wndproc_ref = WNDPROC(self._wndproc)
        wc = WNDCLASSEXW()
        wc.cbSize = ctypes.sizeof(WNDCLASSEXW)
        wc.lpfnWndProc = self._wndproc_ref
        wc.hInstance = self._hinstance
        wc.hIcon = None
        wc.hCursor = None
        wc.lpszClassName = WINDOW_CLASS
        if not user32.RegisterClassExW(ctypes.byref(wc)):
            err = ctypes.get_last_error()
            if err != 1410:  # ERROR_CLASS_ALREADY_EXISTS
                raise OSError(err, "RegisterClassExW 失败")
        else:
            self._class_registered = True

        self.hwnd = user32.CreateWindowExW(
            0, WINDOW_CLASS, APP_NAME, 0,
            0, 0, 0, 0, None, None, self._hinstance, None
        )
        if not self.hwnd:
            raise OSError(ctypes.get_last_error(), "CreateWindowExW 失败")

    def _create_halo(self) -> None:
        if self.no_halo:
            log.info("光效已通过 --no-halo 禁用")
            return
        self.halo = HaloOverlay(
            self._hinstance, enabled=True,
            color=self.halo_color, size=self.halo_size,
        )
        if self.halo.available:
            self.visual.halo = self.halo
            log.info("光效：%s", self.halo.reason)
        else:
            log.warning("光效不可用：%s", self.halo.reason)

    def _install_hook(self) -> None:
        self._hook_proc_ref = HOOKPROC(self._hook_proc)
        self._hook = user32.SetWindowsHookExW(
            WH_KEYBOARD_LL, self._hook_proc_ref, self._hinstance, 0
        )
        if not self._hook:
            raise OSError(ctypes.get_last_error(), "SetWindowsHookExW 失败")
        log.debug("WH_KEYBOARD_LL 钩子已安装")

    def _install_console_handler(self) -> None:
        self._ctrl_handler_ref = PHANDLER_ROUTINE(self._console_ctrl)
        try:
            kernel32.SetConsoleCtrlHandler(self._ctrl_handler_ref, True)
        except Exception:
            pass

    def _start_timers(self) -> None:
        self._timer_id = user32.SetTimer(
            self.hwnd, TIMER_ID_POLL, POLL_INTERVAL_MS, None
        )
        if not self._timer_id:
            raise OSError(ctypes.get_last_error(), "SetTimer(poll) 失败")
        if self.halo is not None and self.halo.available:
            self._halo_timer_id = user32.SetTimer(
                self.hwnd, TIMER_ID_HALO, HALO_INTERVAL_MS, None
            )
            if not self._halo_timer_id:
                log.warning("SetTimer(halo) 失败，光效将降级为禁用")
                self.halo.available = False
                self.halo.reason = "动画计时器创建失败"
                self.halo.set_visible(False)
                self.visual.halo = None

    def _create_tray(self) -> None:
        if self.no_tray:
            self._tray_color_reason = "未启用（--no-tray）"
            log.info("已通过 --no-tray 隐藏托盘图标")
            return
        self._ensure_tray()

    def _ensure_tray(self) -> bool:
        """创建或重新添加托盘图标；也用于 --show-tray 恢复。"""
        if self.hwnd is None:
            return False
        if self.tray is None:
            self._icon_default, self._icon_active, self._tray_color_reason = \
                create_tray_icons()
            self._custom_icons = self._icon_default is not self._icon_active
            self._tray_color_available = self._custom_icons
            self.tray = TrayIcon(
                self.hwnd, self._icon_default, self._icon_active,
                color_available=self._tray_color_available,
            )
        if not self.tray.added:
            if not self.tray.add(self._tray_tip(), active=self.visual.latched):
                log.warning("添加托盘图标失败，可运行 --show-tray 重试")
                return False
        self.visual.tray = self.tray
        self.no_tray = False
        log.info("托盘变色：%s", self._tray_color_reason)
        return True

    def _hide_tray(self) -> None:
        if self.tray is None or not self.tray.added:
            return
        self.tray.remove()
        # 保留 TrayIcon/HICON，方便 --show-tray 重新添加；停止视觉更新。
        self.visual.tray = None
        log.info("托盘图标已隐藏；可运行 `--show-tray` 恢复")

    def _show_tray(self) -> None:
        if self._ensure_tray():
            log.info("托盘图标已恢复")

    def _tray_tip(self) -> str:
        return "RimeCapsShift：" + self.handler.status_short()

    def _message_loop(self) -> None:
        msg = wintypes.MSG()
        while not self._quitting:
            ret = user32.GetMessageW(ctypes.byref(msg), None, 0, 0)
            if ret == -1:
                raise OSError(ctypes.get_last_error(), "GetMessageW 失败")
            if ret == 0:
                break
            user32.TranslateMessage(ctypes.byref(msg))
            user32.DispatchMessageW(ctypes.byref(msg))

    def _wndproc(self, hwnd, msg, wparam, lparam):
        try:
            if msg == WM_CLOSE:
                self._request_quit()
                return 0
            if msg == WM_DESTROY:
                user32.PostQuitMessage(0)
                return 0
            if msg == WM_TIMER:
                timer_id = int(wparam)
                if self._hook_error_message:
                    log.warning("键盘钩子回调异常：%s", self._hook_error_message)
                    self._hook_error_message = ""
                if self._menu_open or self._quitting:
                    return 0
                if timer_id == TIMER_ID_POLL:
                    self.handler.refresh_foreground()
                    self.handler.ensure_shift_release()
                    self._poll_ticks += 1
                    if self._poll_ticks % 4 == 0:
                        self.handler.sync_visual()
                    else:
                        self.handler.flush_pending_ui()
                elif timer_id == TIMER_ID_HALO:
                    if self.halo is not None:
                        self.halo.tick()
                    self.handler.flush_pending_ui()
                return 0
            if msg == WM_APP_SHOW_TRAY:
                self._show_tray()
                return 0
            if msg == WM_APP_TRAY:
                event = int(lparam) & 0xFFFF
                if event in (WM_RBUTTONUP, WM_CONTEXTMENU):
                    self._show_tray_menu()
                return 0
            if msg == WM_QUERYENDSESSION:
                return 1
            if msg == WM_ENDSESSION:
                if wparam:
                    self.handler.shutdown()
                    self._request_quit()
                return 0
            if msg == WM_COMMAND and int(wparam) & 0xFFFF == ID_TRAY_EXIT:
                self._request_quit()
                return 0
        except Exception:
            log.exception("窗口消息处理异常")
        return user32.DefWindowProcW(hwnd, msg, wparam, lparam)

    def _show_tray_menu(self) -> None:
        self._menu_open = True
        menu = user32.CreatePopupMenu()
        if not menu:
            self._menu_open = False
            return
        try:
            user32.AppendMenuW(
                menu, MF_STRING | MF_GRAYED, 0, self.handler.status_menu()
            )
            autostart_flags = MF_STRING
            if is_autostart_enabled():
                autostart_flags |= MF_CHECKED
            user32.AppendMenuW(
                menu, autostart_flags, ID_TRAY_AUTOSTART, "开机自启动"
            )
            user32.AppendMenuW(
                menu, MF_STRING, ID_TRAY_HIDE, "隐藏托盘图标"
            )
            user32.AppendMenuW(menu, MF_SEPARATOR, 0, None)
            user32.AppendMenuW(menu, MF_STRING, ID_TRAY_EXIT, "退出 RimeCapsShift")
            pt = wintypes.POINT()
            user32.GetCursorPos(ctypes.byref(pt))
            user32.SetForegroundWindow(self.hwnd)
            command = user32.TrackPopupMenu(
                menu,
                TPM_RIGHTBUTTON | TPM_NONOTIFY | TPM_RETURNCMD,
                pt.x, pt.y, 0, self.hwnd, None,
            )
            user32.PostMessageW(self.hwnd, WM_NULL, 0, 0)
            if command == ID_TRAY_AUTOSTART:
                self._toggle_autostart()
            elif command == ID_TRAY_HIDE:
                self._hide_tray()
            elif command == ID_TRAY_EXIT:
                self._request_quit()
        finally:
            user32.DestroyMenu(menu)
            self._menu_open = False

    def _toggle_autostart(self) -> None:
        enable = not is_autostart_enabled()
        if not set_autostart_enabled(enable):
            user32.MessageBoxW(
                self.hwnd,
                "无法更新开机自启动设置。\n"
                "请检查当前用户是否有写入 HKCU\\...\\Run 的权限。",
                APP_NAME,
                0x00000010,  # MB_ICONERROR
            )
            return
        log.info("开机自启动已%s", "开启" if enable else "关闭")

    def _console_ctrl(self, event) -> bool:
        if event in (CTRL_C_EVENT, CTRL_BREAK_EVENT, CTRL_CLOSE_EVENT,
                     CTRL_LOGOFF_EVENT, CTRL_SHUTDOWN_EVENT):
            if self.hwnd:
                user32.PostMessageW(self.hwnd, WM_CLOSE, 0, 0)
            return True
        return False

    def _request_quit(self) -> None:
        if not self._quitting:
            self._quitting = True
            user32.PostQuitMessage(0)

    def _hook_proc(self, n_code, wparam, lparam):
        try:
            if n_code == HC_ACTION:
                kb = ctypes.cast(
                    lparam, ctypes.POINTER(KBDLLHOOKSTRUCT)
                ).contents
                # 忽略本程序和其他程序注入的按键，避免递归。
                if not (kb.flags & LLKHF_INJECTED):
                    down = wparam in (WM_KEYDOWN, WM_SYSKEYDOWN)
                    up = wparam in (WM_KEYUP, WM_SYSKEYUP)
                    if (down or up) and self.handler.handle_key(kb.vkCode, down):
                        return 1
        except Exception as exc:
            # 钩子内绝不写日志；把错误留给定时器在钩子外记录。
            self._hook_error_message = repr(exc)
        return user32.CallNextHookEx(self._hook, n_code, wparam, lparam)

    def close(self) -> None:
        if self._closed:
            return
        self._closed = True

        # 清理必须分段做：任何一步失败都不能跳过 Shift Up。
        try:
            if self._timer_id and self.hwnd:
                user32.KillTimer(self.hwnd, TIMER_ID_POLL)
        except Exception:
            log.exception("停止轮询计时器失败")
        self._timer_id = 0
        try:
            if self._halo_timer_id and self.hwnd:
                user32.KillTimer(self.hwnd, TIMER_ID_HALO)
        except Exception:
            log.exception("停止光效计时器失败")
        self._halo_timer_id = 0

        try:
            if self._hook:
                user32.UnhookWindowsHookEx(self._hook)
        except Exception:
            log.exception("卸载键盘钩子失败")
        self._hook = None

        try:
            # 先移除钩子，再确保 Shift 被释放。
            self.handler.shutdown()
            if self.injector.is_down:
                log.error("退出时 Shift 仍处于按下状态，发送额外 Shift Up")
                self.injector.release()
            self.handler.flush_pending_ui()
        except Exception:
            log.exception("释放 Shift 失败")

        try:
            self.visual.shutdown()
        except Exception:
            log.exception("关闭光效失败")
        if self.halo is not None:
            try:
                self.halo.close()
            except Exception:
                pass
            self.halo = None

        try:
            if self.tray is not None:
                self.tray.remove()
                self.tray = None
            if self.hwnd:
                user32.DestroyWindow(self.hwnd)
                self.hwnd = None
            if self._class_registered:
                user32.UnregisterClassW(WINDOW_CLASS, self._hinstance)
                self._class_registered = False
        except Exception:
            log.exception("销毁窗口/托盘失败")

        if self._custom_icons:
            for icon in (self._icon_default, self._icon_active):
                try:
                    if icon:
                        user32.DestroyIcon(icon)
                except Exception:
                    pass
            self._custom_icons = False
            self._icon_default = HICON()
            self._icon_active = HICON()

        try:
            if self._ctrl_handler_ref is not None:
                kernel32.SetConsoleCtrlHandler(self._ctrl_handler_ref, False)
        except Exception:
            pass
        self._ctrl_handler_ref = None

        try:
            self.detector.close()
        except Exception:
            log.exception("关闭 TSF 检测失败")
        try:
            self._close_mutex()
        except Exception:
            log.exception("关闭单实例互斥量失败")
        log.info("RimeCapsShift 已退出")


    def _close_mutex(self) -> None:
        if self._mutex:
            kernel32.CloseHandle(self._mutex)
            self._mutex = None


# ---------------------------------------------------------------------------
# 命令行
# ---------------------------------------------------------------------------

def _setup_logging(debug: bool, log_file: Optional[str]) -> None:
    level = logging.DEBUG if debug else logging.INFO
    handlers: list[logging.Handler] = []
    if sys.stderr is not None:
        handlers.append(logging.StreamHandler(sys.stderr))
    if log_file:
        try:
            parent = os.path.dirname(os.path.abspath(log_file))
            if parent:
                os.makedirs(parent, exist_ok=True)
            handlers.append(logging.FileHandler(log_file, encoding="utf-8"))
        except OSError as exc:
            _print(f"无法打开日志文件 {log_file}: {exc}", file=sys.stderr)
    if not handlers:
        handlers.append(logging.NullHandler())
    logging.basicConfig(
        level=level,
        format="%(asctime)s [%(levelname)s] %(message)s",
        handlers=handlers,
        force=True,
    )


def _quit_running_instance() -> int:
    hwnd = user32.FindWindowW(WINDOW_CLASS, None)
    if not hwnd:
        _print("RimeCapsShift 当前未运行。")
        return 0
    if not user32.PostMessageW(hwnd, WM_CLOSE, 0, 0):
        _print("发送退出请求失败，WinError=%s" % ctypes.get_last_error())
        return 1
    # 给主线程一点时间处理 WM_CLOSE，避免用户马上重启时互斥量尚未释放。
    exited = False
    for _ in range(40):
        time.sleep(0.05)
        if not user32.IsWindow(hwnd):
            exited = True
            break
    if exited:
        _print("已请求 RimeCapsShift 退出。")
    else:
        _print("退出请求已发送，但进程在 2 秒内未退出。")
    return 0


def _show_tray_running_instance() -> int:
    hwnd = user32.FindWindowW(WINDOW_CLASS, None)
    if not hwnd:
        _print("RimeCapsShift 当前未运行。")
        return 0
    if not user32.PostMessageW(hwnd, WM_APP_SHOW_TRAY, 0, 0):
        _print("发送显示托盘请求失败，WinError=%s" % ctypes.get_last_error())
        return 1
    _print("已请求显示 RimeCapsShift 托盘图标。")
    return 0


def _run_diagnose(use_tsf: bool, no_tray: bool = False,
                  no_halo: bool = False, halo_color: str = HALO_DEFAULT_COLOR,
                  halo_size: int = HALO_DEFAULT_SIZE, beep: bool = False,
                  shift_key: str = "right",
                  shift_threshold_ms: int = 500) -> int:
    detector = WeaselDetector(use_tsf=use_tsf)
    try:
        color = parse_halo_color(halo_color)
    except ValueError as exc:
        _print(f"无效的 --halo-color {halo_color!r}：{exc}；使用默认值")
        color = parse_halo_color(HALO_DEFAULT_COLOR)
    size = max(HALO_MIN_SIZE, min(HALO_MAX_SIZE, int(halo_size)))
    threshold = shift_threshold_ms if shift_threshold_ms > 0 else 500
    selected_shift = shift_key if shift_key in ("right", "left", "both") else "right"
    icon_off = icon_on = HICON()
    icons_custom = False
    halo = None
    try:
        info = detector.foreground_info()
        final = detector.is_weasel_foreground()
        tips = []
        tips.append("RimeCapsShift 诊断")
        tips.append("=" * 44)
        tips.append(f"前台 HKL          : 0x{info['hkl']:08X}")
        tips.append(f"HKL 描述          : {info['description']!r}")
        tips.append(f"IME 文件名        : {info['ime_file']!r}")
        tips.append(f"HKL 语言 ID       : 0x{info['langid']:04X}"
                    f"（{'中文' if info['is_chinese_hkl'] else '非中文'}）")
        tips.append(f"HKL 直接识别      : {'是' if info['hkl_match'] else '否'}")
        if detector.tsf is not None:
            tips.append(f"TSF 检测可用      : {'是' if detector.tsf.available else '否'}")
            if not detector.tsf.available and detector.tsf.last_error:
                tips.append(f"TSF 错误          : {detector.tsf.last_error}")
            tips.append("Weasel CLSID 候选 : " + ", ".join(
                str(g) for g in detector.candidates
            ))
            tips.append(f"TSF 活动 Weasel   : {'是' if info['tsf_active'] else '否'}")
        else:
            tips.append("TSF 检测可用      : 否（命令行 --no-tsf）")
        tips.append(f"最终前台判定      : {'小狼毫' if final else '不是小狼毫'}")
        tips.append(f"启动后状态        : "
                    f"{'中文模式（CapsLock 锁存 Shift）' if final else '英文/非小狼毫（CapsLock 正常）'}")
        tips.append(f"Shift 监听键      : {selected_shift}")
        tips.append(f"Shift 阈值        : {threshold}ms")
        tips.append("钩子内 TSF 查询  : 已移除")

        if no_tray:
            tray_status = "未启用（--no-tray）"
        else:
            icon_off, icon_on, tray_status = create_tray_icons()
            icons_custom = icon_off is not icon_on
        tips.append(f"托盘变色          : {tray_status}")

        if no_halo:
            tips.append("光效              : 已禁用（--no-halo）")
        else:
            enable_dpi_awareness()
            halo = HaloOverlay(
                kernel32.GetModuleHandleW(None), enabled=True,
                color=color, size=size,
            )
            tips.append(f"光效              : {halo.reason}")
        tips.append(f"光效颜色          : #{color[0]:02X}{color[1]:02X}{color[2]:02X}")
        tips.append(f"光效大小          : {size} px")
        tips.append(f"开机自启动        : {'开' if is_autostart_enabled() else '关'}（HKCU Run）")
        tips.append(f"提示音            : {'开' if beep else '关'}（--beep）")
        tips.append("识别关键字        : " + ", ".join(WEASEL_KEYWORDS))
        _print("\n".join(tips))
        return 0
    finally:
        if halo is not None:
            halo.close()
        if icons_custom:
            for icon in (icon_off, icon_on):
                try:
                    if icon:
                        user32.DestroyIcon(icon)
                except Exception:
                    pass
        detector.close()


def _fatal_error(message: str) -> None:
    log.exception(message)
    if sys.stderr is None:
        try:
            user32.MessageBoxW(
                None, message, APP_NAME, 0x00000010  # MB_ICONERROR
            )
        except Exception:
            pass


def _parse_shift_threshold(value: str) -> int:
    try:
        number = int(value)
    except (TypeError, ValueError):
        return 500
    return number if number > 0 else 500


def build_arg_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="rime_caps_shift.py",
        description="小狼毫中文模式下将 CapsLock 锁存为左 Shift。",
    )
    parser.add_argument("--diagnose", action="store_true",
                        help="只读检查当前前台输入法识别结果后退出")
    parser.add_argument("--quit", action="store_true",
                        help="请求已运行的实例退出")
    parser.add_argument("--show-tray", action="store_true",
                        help="请求已运行的实例恢复托盘图标（隐藏后使用）")
    parser.add_argument("--no-halo", action="store_true",
                        help="禁用鼠标周围的呼吸光晕，仅保留托盘变色")
    parser.add_argument("--halo-color", metavar="RRGGBB",
                        default=HALO_DEFAULT_COLOR,
                        help=f"光晕颜色，默认 {HALO_DEFAULT_COLOR}")
    parser.add_argument("--halo-size", metavar="PX", type=int,
                        default=HALO_DEFAULT_SIZE,
                        help=f"光晕直径（像素），默认 {HALO_DEFAULT_SIZE}")
    parser.add_argument("--beep", action="store_true",
                        help="锁存/解除时播放系统提示音（默认关）")
    parser.add_argument("--shift-key", choices=("right", "left", "both"),
                        default="right",
                        help="用于切换中英文的 Shift 键，默认 right")
    parser.add_argument("--shift-threshold", metavar="MS",
                        type=_parse_shift_threshold, default=500,
                        help="Shift 短按判定阈值，默认 500ms")
    parser.add_argument("--no-tsf", action="store_true",
                        help="禁用 TSF 补充检测，仅用 HKL 描述匹配")
    parser.add_argument("--no-tray", "--hide-tray", action="store_true",
                        dest="no_tray",
                        help="隐藏托盘图标（不创建托盘；用 --quit 退出）")
    parser.add_argument("--debug", action="store_true",
                        help="输出调试日志")
    parser.add_argument("--log-file", metavar="PATH",
                        help="额外把日志写入指定文件")
    parser.add_argument("--version", action="version",
                        version=f"{APP_NAME} {APP_VERSION}")
    return parser


def main(argv: Optional[list[str]] = None) -> int:
    if os.name != "nt":
        _print("RimeCapsShift 仅支持 Windows。", file=sys.stderr)
        return 2
    try:
        if hasattr(sys.stdout, "reconfigure"):
            sys.stdout.reconfigure(errors="replace")
        if hasattr(sys.stderr, "reconfigure"):
            sys.stderr.reconfigure(errors="replace")
    except Exception:
        pass

    args = build_arg_parser().parse_args(argv)

    if args.quit:
        return _quit_running_instance()
    if args.show_tray:
        return _show_tray_running_instance()

    _setup_logging(args.debug, args.log_file)

    if args.diagnose:
        return _run_diagnose(
            use_tsf=not args.no_tsf,
            no_tray=args.no_tray,
            no_halo=args.no_halo,
            halo_color=args.halo_color,
            halo_size=args.halo_size,
            beep=args.beep,
            shift_key=args.shift_key,
            shift_threshold_ms=args.shift_threshold,
        )

    enable_dpi_awareness()

    app = None
    try:
        app = RimeCapsShiftApp(
            use_tsf=not args.no_tsf,
            no_tray=args.no_tray,
            no_halo=args.no_halo,
            halo_color=args.halo_color,
            halo_size=args.halo_size,
            beep=args.beep,
            shift_key=args.shift_key,
            shift_threshold_ms=args.shift_threshold,
        )
        atexit.register(app.close)
        return app.run()
    except Exception as exc:
        _fatal_error("RimeCapsShift 启动失败：%s" % exc)
        return 1
    finally:
        if app is not None:
            try:
                app.close()
            except Exception:
                pass


if __name__ == "__main__":
    sys.exit(main())
