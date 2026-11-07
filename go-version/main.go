//go:build windows

// main.go —— 应用入口、全局 App、窗口/消息循环、定时器、DPI 与日志。
//
// 约定：
//   * 主线程必须 LockOSThread；所有钩子/COM/窗口都在同一 OS 线程上。
//   * 回调里永远不要阻塞、不要写日志文件、不要做 COM 查询；
//     日志、托盘/光效同步、提示音统一由 WM_TIMER 在钩子外消费。

package main

//go:generate go run github.com/akavel/rsrc@v0.10.2 -arch amd64 -ico ../assets/rime_caps_shift.ico -o rsrc_windows_amd64.syso

import (
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---------- 全局 App ----------

// app 是唯一的 App 实例。回调（wndProc/hookProc/consoleCtrlHandler）都通过
// 它访问状态；这样 syscall.NewCallback 注册的回调不依赖任何可变闭包变量。
var app *App

// ---------- 颜色 ----------

// HaloColor 表示 RGB 三个分量；与 parseHaloColor 的输出一致。
type HaloColor struct {
	R, G, B uint8
}

// ---------- 日志 ----------

var (
	stdLogger = log.New(os.Stderr, "", log.LstdFlags)
	stdMu     sync.Mutex

	debugEnabled bool
)

// setupLogging 配置全局日志级别和输出目标。--debug 打开 DEBUG 日志；
// logFile 为空则只写 stderr。
func setupLogging(cfg *Config) {
	debugEnabled = cfg.Debug
	out := os.Stderr
	if cfg.LogFile != "" {
		if dir := parentDir(cfg.LogFile); dir != "" {
			_ = os.MkdirAll(dir, 0o755)
		}
		if f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			out = f
		} else {
			fmt.Fprintf(os.Stderr, "无法打开日志文件 %s: %v\n", cfg.LogFile, err)
		}
	}
	stdMu.Lock()
	stdLogger.SetOutput(out)
	stdMu.Unlock()
}

func logInfo(f string, args ...any) {
	stdMu.Lock()
	stdLogger.Printf("[INFO] "+f, args...)
	stdMu.Unlock()
}
func logWarn(f string, args ...any) {
	stdMu.Lock()
	stdLogger.Printf("[WARN] "+f, args...)
	stdMu.Unlock()
}
func logError(f string, args ...any) {
	stdMu.Lock()
	stdLogger.Printf("[ERROR] "+f, args...)
	stdMu.Unlock()
}
func logDebug(f string, args ...any) {
	if !debugEnabled {
		return
	}
	stdMu.Lock()
	stdLogger.Printf("[DEBUG] "+f, args...)
	stdMu.Unlock()
}

func parentDir(path string) string {
	idx := strings.LastIndexAny(path, `/\`)
	if idx < 0 {
		return ""
	}
	return path[:idx]
}

// ---------- VisualState ----------

// VisualState 统一驱动托盘图标颜色和光晕显隐，逻辑与 Python 版一致。
// setLatched 只在钩子外（定时器）调用。
type VisualState struct {
	latched     bool
	tray        *TrayIcon
	halo        *HaloOverlay
	tipProvider func() string
}

func (v *VisualState) setLatched(on bool) {
	v.latched = bool(on)
	v.sync()
}

func (v *VisualState) sync() {
	if v.tray != nil {
		tip := ""
		if v.tipProvider != nil {
			tip = v.tipProvider()
		}
		v.tray.setState(v.latched, tip)
	}
	if v.halo != nil {
		v.halo.setVisible(v.latched)
	}
}

func (v *VisualState) shutdown() {
	if v.halo != nil {
		v.halo.close()
		v.halo = nil
	}
}

// ---------- App ----------

// App 汇总所有子系统引用和 Win32 资源句柄。
// 各方法分散在同包不同文件里，方便按职责阅读：
//   - window/timer/message loop: main.go
//   - 键盘钩子、SendInput:      hook.go
//   - 状态机:                    state.go
//   - TSF/HKL 检测:              tsf.go
//   - 光效:                      halo.go
//   - 托盘 & 菜单:               tray.go
//   - 图标生成:                  icons.go
//   - 开机自启:                  autostart.go
//   - 命令行 & 诊断:             cmd.go
type App struct {
	// 配置
	cfg *Config

	// 子系统
	detector *WeaselDetector
	injector *ShiftInjector
	state    *KeyboardHandler
	visual   *VisualState
	halo     *HaloOverlay
	tray     *TrayIcon

	// 图标
	iconDefault     windows.Handle
	iconActive      windows.Handle
	customIcons     bool
	trayColorReason string

	// Win32
	hinstance       windows.Handle
	hwnd            windows.Handle
	hook            windows.Handle
	mutex           windows.Handle
	wndProcCb       uintptr
	hookCb          uintptr
	ctrlHandlerCb   uintptr
	timerID         uintptr
	haloTimerID     uintptr
	classRegistered bool

	// 运行状态
	quitting  bool
	closed    bool
	menuOpen  bool
	pollTicks int
	hookErr   error
}

// ---------- 入口 ----------

func main() {
	// 必须最先调用：钩子、COM、窗口、消息循环都要同一 OS 线程。
	runtime.LockOSThread()

	initInputLayoutCheck()

	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if cfg.Quit {
		os.Exit(runQuit())
	}
	if cfg.ShowTray {
		os.Exit(runShowTray())
	}

	setupLogging(cfg)

	if cfg.Diagnose {
		os.Exit(runDiagnose(cfg))
	}

	enableDPIAwareness()

	if err := runApp(cfg); err != nil {
		msg := "RimeCapsShift 启动失败：" + err.Error()
		logError("%s", msg)
		// 无控制台构建时也能看到错误
		msgPtr := utf16Ptr(msg)
		titlePtr := utf16Ptr(APP_NAME)
		procMessageBoxW.Call(
			0,
			ptrToUint(unsafe.Pointer(msgPtr)),
			ptrToUint(unsafe.Pointer(titlePtr)),
			MB_ICONERROR,
		)
		runtime.KeepAlive(msgPtr)
		runtime.KeepAlive(titlePtr)
		os.Exit(1)
	}
}

// runApp 负责单实例 + App 生命周期。
func runApp(cfg *Config) error {
	// 单实例
	namePtr := utf16Ptr(MUTEX_NAME)
	mutex, _, errno := procCreateMutexW.Call(0, boolToUintptr(false), ptrToUint(unsafe.Pointer(namePtr)))
	runtime.KeepAlive(namePtr)
	if mutex == 0 {
		return fmt.Errorf("CreateMutexW 失败：%w", errno)
	}
	if errors.Is(errno, windows.ERROR_ALREADY_EXISTS) || errno == windows.ERROR_ALREADY_EXISTS {
		procCloseHandle.Call(mutex)
		logInfo("RimeCapsShift 已在运行，退出本次启动")
		return nil
	}

	a := newApp(cfg)
	app = a
	a.mutex = windows.Handle(mutex)
	defer a.shutdown()

	if err := a.init(); err != nil {
		return err
	}
	a.messageLoop()
	return nil
}

// ---------- App 构造与初始化 ----------

func newApp(cfg *Config) *App {
	a := &App{
		cfg:             cfg,
		hinstance:       mustModuleHandle(),
		trayColorReason: "未初始化",
	}
	a.detector = newWeaselDetector(!cfg.NoTSF)
	a.injector = newShiftInjector()
	a.state = newKeyboardHandler(
		a.detector, a.injector,
		cfg.Beep, cfg.ShiftKey, cfg.ShiftThresholdMS,
	)
	a.visual = &VisualState{
		tipProvider: func() string {
			if a.state == nil {
				return "RimeCapsShift"
			}
			return a.trayTip()
		},
	}
	a.state.setVisual(a.visual)
	return a
}

func mustModuleHandle() windows.Handle {
	h, _, _ := procGetModuleHandleW.Call(0)
	return windows.Handle(h)
}

// init 按 Python 版顺序初始化各子系统。
func (a *App) init() error {
	_ = a.state.initialize()

	if err := a.createWindow(); err != nil {
		return err
	}
	a.createHalo() // 失败不致命，降级到托盘
	if err := a.installHook(); err != nil {
		return err
	}
	a.installConsoleHandler()
	a.createTray()
	a.visual.sync()
	a.state.flushPendingUI()
	if err := a.startTimers(); err != nil {
		return err
	}

	logInfo("RimeCapsShift 已启动。托盘菜单可退出；也可运行 `--quit`。")
	return nil
}

// ---------- 窗口 ----------

func (a *App) createWindow() error {
	a.wndProcCb = syscall.NewCallback(wndProc)

	className := utf16Ptr(WINDOW_CLASS)
	wc := WNDCLASSEXW{
		CbSize:        uint32(unsafe.Sizeof(WNDCLASSEXW{})),
		LpfnWndProc:   a.wndProcCb,
		HInstance:     a.hinstance,
		LpszClassName: className,
	}
	r, _, errno := procRegisterClassExW.Call(ptrToUint(unsafe.Pointer(&wc)))
	runtime.KeepAlive(className)
	runtime.KeepAlive(wc)

	const errClassAlreadyExists = syscall.Errno(1410)
	if r == 0 {
		if !errors.Is(errno, errClassAlreadyExists) && errno != errClassAlreadyExists {
			return fmt.Errorf("RegisterClassExW 失败：%w", errno)
		}
	} else {
		a.classRegistered = true
	}

	title := utf16Ptr(APP_NAME)
	hwnd, _, errno := procCreateWindowExW.Call(
		0,
		ptrToUint(unsafe.Pointer(className)),
		ptrToUint(unsafe.Pointer(title)),
		0, 0, 0, 0, 0,
		0, 0, uintptr(a.hinstance), 0,
	)
	runtime.KeepAlive(className)
	runtime.KeepAlive(title)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW 失败：%w", errno)
	}
	a.hwnd = windows.Handle(hwnd)
	return nil
}

// ---------- 控制台处理 ----------

func (a *App) installConsoleHandler() {
	a.ctrlHandlerCb = syscall.NewCallback(consoleCtrlHandler)
	procSetConsoleCtrlHandler.Call(a.ctrlHandlerCb, boolToUintptr(true))
}

func consoleCtrlHandler(event uintptr) uintptr {
	switch uint32(event) {
	case CTRL_C_EVENT, CTRL_BREAK_EVENT, CTRL_CLOSE_EVENT,
		CTRL_LOGOFF_EVENT, CTRL_SHUTDOWN_EVENT:
		if app != nil && app.hwnd != 0 {
			procPostMessageW.Call(uintptr(app.hwnd), WM_CLOSE, 0, 0)
		}
		return 1
	}
	return 0
}

// ---------- 定时器 ----------

func (a *App) startTimers() error {
	r, _, errno := procSetTimer.Call(
		uintptr(a.hwnd), TIMER_ID_POLL, POLL_INTERVAL_MS, 0,
	)
	if r == 0 {
		return fmt.Errorf("SetTimer(poll) 失败：%w", errno)
	}
	a.timerID = r

	if a.halo != nil && a.halo.available {
		r, _, errno = procSetTimer.Call(
			uintptr(a.hwnd), TIMER_ID_HALO, HALO_INTERVAL_MS, 0,
		)
		if r == 0 {
			logWarn("SetTimer(halo) 失败，光效将降级为禁用：%v", errno)
			a.halo.reason = "动画计时器创建失败"
			a.visual.halo = nil
			a.halo.close()
			a.halo = nil
			return nil
		}
		a.haloTimerID = r
	}
	return nil
}

// ---------- 消息循环 ----------

func (a *App) messageLoop() {
	msg := &MSG{}
	for {
		r, _, errno := procGetMessageW.Call(
			ptrToUint(unsafe.Pointer(msg)), 0, 0, 0,
		)
		if int32(r) == -1 {
			logError("GetMessageW 失败：%v", errno)
			return
		}
		if r == 0 {
			return
		}
		procTranslateMessage.Call(ptrToUint(unsafe.Pointer(msg)))
		procDispatchMessageW.Call(ptrToUint(unsafe.Pointer(msg)))
	}
}

// ---------- 窗口过程 ----------

func wndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	// 回调内不能把 panic 抛回 runtime。
	defer func() {
		if r := recover(); r != nil {
			logError("窗口消息处理 panic：%v", r)
		}
	}()
	if app == nil {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
		return r
	}

	switch uint32(msg) {
	case WM_CLOSE:
		app.quit()
		return 0
	case WM_DESTROY:
		procPostQuitMessage.Call(0)
		return 0
	case WM_TIMER:
		if app.hookErr != nil {
			logWarn("键盘钩子回调异常：%v", app.hookErr)
			app.hookErr = nil
		}
		if app.menuOpen || app.quitting {
			return 0
		}
		switch {
		case app.timerID != 0 && wparam == app.timerID:
			app.onPollTimer()
		case app.haloTimerID != 0 && wparam == app.haloTimerID:
			app.onHaloTimer()
		}
		return 0
	case WM_APP_SHOW_TRAY:
		app.showTray()
		return 0
	case WM_APP_TRAY:
		evt := uint32(lparam) & 0xFFFF
		if evt == WM_RBUTTONUP || evt == WM_CONTEXTMENU {
			app.showTrayMenu()
		}
		return 0
	case WM_QUERYENDSESSION:
		return 1
	case WM_ENDSESSION:
		if wparam != 0 {
			app.state.shutdown()
			app.quit()
		}
		return 0
	case WM_COMMAND:
		if uint16(wparam&0xFFFF) == ID_TRAY_EXIT {
			app.quit()
			return 0
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

// ---------- 定时器回调 ----------

func (a *App) onPollTimer() {
	a.state.refreshForeground()
	a.state.ensureShiftRelease()
	a.pollTicks++
	if a.pollTicks%4 == 0 {
		a.state.syncVisual()
	} else {
		a.state.flushPendingUI()
	}
}

func (a *App) onHaloTimer() {
	if a.halo != nil {
		a.halo.tick()
	}
	a.state.flushPendingUI()
}

// ---------- 退出 ----------

func (a *App) quit() {
	if a.quitting {
		return
	}
	a.quitting = true
	procPostQuitMessage.Call(0)
}

// shutdown 分段清理，保证 Shift Up 一定能发出。
// 顺序与 Python 版 close() 一致：定时器 → 钩子 → Shift → 光效 → 托盘/窗口/图标
// → 控制台处理器 → TSF → mutex。
func (a *App) shutdown() {
	if a.closed {
		return
	}
	a.closed = true

	// 1. 停定时器
	if a.timerID != 0 && a.hwnd != 0 {
		procKillTimer.Call(uintptr(a.hwnd), a.timerID)
	}
	a.timerID = 0
	if a.haloTimerID != 0 && a.hwnd != 0 {
		procKillTimer.Call(uintptr(a.hwnd), a.haloTimerID)
	}
	a.haloTimerID = 0

	// 2. 卸载钩子
	if a.hook != 0 {
		procUnhookWindowsHookEx.Call(uintptr(a.hook))
		a.hook = 0
	}

	// 3. 释放 Shift
	if a.state != nil {
		a.state.shutdown()
		a.state.flushPendingUI()
	}
	if a.injector != nil && a.injector.isDown() {
		if !a.injector.release() {
			logError("退出时 Shift 仍处于按下状态：SendInput 失败")
		}
	}

	// 4. 关闭光效
	if a.visual != nil {
		a.visual.shutdown()
	}
	if a.halo != nil {
		a.halo.close()
		a.halo = nil
	}

	// 5. 托盘、窗口、类、图标
	if a.tray != nil {
		a.tray.remove()
		a.tray = nil
	}
	if a.hwnd != 0 {
		procDestroyWindow.Call(uintptr(a.hwnd))
		a.hwnd = 0
	}
	if a.classRegistered {
		className := utf16Ptr(WINDOW_CLASS)
		procUnregisterClassW.Call(
			ptrToUint(unsafe.Pointer(className)),
			uintptr(a.hinstance),
		)
		runtime.KeepAlive(className)
		a.classRegistered = false
	}
	if a.customIcons {
		if a.iconDefault != 0 {
			procDestroyIcon.Call(uintptr(a.iconDefault))
		}
		if a.iconActive != 0 {
			procDestroyIcon.Call(uintptr(a.iconActive))
		}
		a.iconDefault = 0
		a.iconActive = 0
		a.customIcons = false
	}

	// 6. 控制台处理器
	if a.ctrlHandlerCb != 0 {
		procSetConsoleCtrlHandler.Call(a.ctrlHandlerCb, boolToUintptr(false))
		a.ctrlHandlerCb = 0
	}

	// 7. TSF
	if a.detector != nil {
		a.detector.close()
	}

	// 8. mutex
	if a.mutex != 0 {
		procCloseHandle.Call(uintptr(a.mutex))
		a.mutex = 0
	}

	logInfo("RimeCapsShift 已退出")
}

// ---------- 子模块装配（薄壳，逻辑在对应文件） ----------

func (a *App) createHalo() {
	if a.cfg.NoHalo {
		logInfo("光效已通过 --no-halo 禁用")
		return
	}
	a.halo = newHaloOverlay(a.hinstance, true, a.cfg.HaloColor, a.cfg.HaloSize)
	if a.halo.available {
		a.visual.halo = a.halo
		logInfo("光效：%s", a.halo.reason)
	} else {
		logWarn("光效不可用：%s", a.halo.reason)
		a.halo = nil
	}
}

func (a *App) createTray() {
	if a.cfg.NoTray {
		a.trayColorReason = "未启用（--no-tray）"
		logInfo("已通过 --no-tray 隐藏托盘图标")
		return
	}
	a.ensureTray()
}

// ---------- DPI ----------

// enableDPIAwareness 依次尝试 Per-Monitor V2 → V1 → System。
// 只要有一个成功即可；全部失败只警告。
func enableDPIAwareness() bool {
	// SetProcessDpiAwarenessContext 只在 Windows 10 1703+ 存在；
	// LazyProc.Call 在过程不存在时会 panic，所以先用 Find 探测。
	if procSetProcessDpiAwarenessContext.Find() == nil {
		if ok, _, _ := procSetProcessDpiAwarenessContext.Call(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2); ok != 0 {
			logDebug("DPI 感知：Per-Monitor V2")
			return true
		}
	}

	// shcore.dll 在 Win7 上不存在；用 Find 探测，不存在就跳过。
	if procSetProcessDpiAwareness.Find() == nil {
		// PROCESS_PER_MONITOR_DPI_AWARE = 2
		hr, _, _ := procSetProcessDpiAwareness.Call(2)
		if int32(hr) == 0 || int32(hr) == 1 {
			logDebug("DPI 感知：Per-Monitor V1")
			return true
		}
	}

	if procSetProcessDPIAware.Find() == nil {
		if ok, _, _ := procSetProcessDPIAware.Call(); ok != 0 {
			logDebug("DPI 感知：System DPI aware")
			return true
		}
	}

	logWarn("无法启用 DPI 感知，混合 DPI 显示器下光效位置可能不准确")
	return false
}
