//go:build windows

// halo.go —— 鼠标周围点击穿透、不抢焦点的半透明呼吸光晕。
//
// 实现要点：
//   * 分层窗口 WS_EX_LAYERED | WS_EX_TRANSPARENT | WS_EX_TOOLWINDOW | WS_EX_NOACTIVATE | WS_EX_TOPMOST
//   * 预乘 alpha 的 BGRA DIB，通过 CreateDIBSection 生成；每个 tick 只改
//     UpdateLayeredWindow 的 SourceConstantAlpha，不重画 DIB。
//   * 呼吸周期约 1.6s（32 个 50ms tick），透明度 155..255。
//   * 窗口过程返回 HTTRANSPARENT / MA_NOACTIVATE，点击穿透且不抢焦点。
//   * 失败不影响主功能：available=false 时降级到"仅托盘变色"。

package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// parseHaloColor 解析 RRGGBB 十六进制颜色（允许前导 #）。
func parseHaloColor(value string) (HaloColor, error) {
	text := strings.TrimPrefix(strings.TrimSpace(value), "#")
	if len(text) != 6 {
		return HaloColor{}, errors.New("颜色必须是 RRGGBB 六位十六进制")
	}
	raw, err := hex.DecodeString(text)
	if err != nil {
		return HaloColor{}, errors.New("颜色必须是 RRGGBB 六位十六进制")
	}
	return HaloColor{raw[0], raw[1], raw[2]}, nil
}

// smoothstep 与 Python 版 _smoothstep 等价；edge1 == edge0 时按 x >= edge1 取 0/1。
func smoothstep(edge0, edge1, x float64) float64 {
	if edge1 == edge0 {
		if x >= edge1 {
			return 1.0
		}
		return 0.0
	}
	t := (x - edge0) / (edge1 - edge0)
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return t * t * (3.0 - 2.0*t)
}

// ---------- HaloOverlay ----------

// HaloOverlay 是鼠标周围的点击穿透、不抢焦点呼吸光晕。
type HaloOverlay struct {
	hinstance       windows.Handle
	enabled         bool
	available       bool
	reason          string
	size            int
	color           HaloColor
	hwnd            windows.Handle
	memdc           uintptr
	bitmap          uintptr
	oldBitmap       uintptr
	wndProcCb       uintptr
	classRegistered bool
	visible         bool
	closed          bool
	phase           float64
}

// newHaloOverlay 按 Python 版 HaloOverlay.__init__ 流程创建光效。
// enabled=false 时直接返回"已禁用"的占位对象；其他情况都会尝试初始化，
// 失败则 available=false 并在 reason 里说明。
func newHaloOverlay(hinstance windows.Handle, enabled bool, color HaloColor, size int) *HaloOverlay {
	h := &HaloOverlay{
		hinstance: hinstance,
		enabled:   enabled,
		size:      size,
		color:     color,
	}
	if !enabled {
		h.reason = "已禁用（--no-halo）"
		return h
	}
	if err := h.buildBitmap(); err != nil {
		h.reason = fmt.Sprintf("初始化失败（%v）", err)
		logWarn("光效初始化失败，降级为仅托盘变色：%s", h.reason)
		h.close()
		return h
	}
	if err := h.createWindow(); err != nil {
		h.reason = fmt.Sprintf("初始化失败（%v）", err)
		logWarn("光效初始化失败，降级为仅托盘变色：%s", h.reason)
		h.close()
		return h
	}
	h.available = true
	h.reason = fmt.Sprintf("可用（#%02X%02X%02X, %dpx）",
		color.R, color.G, color.B, size)
	return h
}

// buildBitmap 预生成 RGBA 环状渐变（预乘 alpha，BGRA 顺序）并挂到 memdc。
func (h *HaloOverlay) buildBitmap() error {
	size := h.size
	half := float64(size) / 2.0
	r0, g0, b0 := h.color.R, h.color.G, h.color.B

	pixels := make([]byte, size*size*4)
	for y := 0; y < size; y++ {
		dy := (float64(y) + 0.5 - half) / half
		for x := 0; x < size; x++ {
			dx := (float64(x) + 0.5 - half) / half
			d := math.Hypot(dx, dy)
			var alpha int
			if d >= 1.0 {
				alpha = 0
			} else {
				rise := smoothstep(0.42, 0.62, d)
				fall := 1.0 - smoothstep(0.85, 1.0, d)
				alpha = int(math.Round(255.0 * rise * fall))
				if alpha < 0 {
					alpha = 0
				}
				if alpha > 255 {
					alpha = 255
				}
			}
			i := (y*size + x) * 4
			// UpdateLayeredWindow 要求预乘 alpha；DIB 是 BGRA 顺序。
			pixels[i] = uint8((int(b0) * alpha) / 255)
			pixels[i+1] = uint8((int(g0) * alpha) / 255)
			pixels[i+2] = uint8((int(r0) * alpha) / 255)
			pixels[i+3] = uint8(alpha)
		}
	}

	bmi := BITMAPINFO{}
	bmi.BmiHeader.BiSize = uint32(unsafe.Sizeof(BITMAPINFOHEADER{}))
	bmi.BmiHeader.BiWidth = int32(size)
	bmi.BmiHeader.BiHeight = -int32(size) // top-down
	bmi.BmiHeader.BiPlanes = 1
	bmi.BmiHeader.BiBitCount = 32
	bmi.BmiHeader.BiCompression = BI_RGB

	memdc, _, _ := procCreateCompatibleDC.Call(0)
	if memdc == 0 {
		return fmt.Errorf("CreateCompatibleDC 失败")
	}
	h.memdc = memdc

	var bits unsafe.Pointer
	bitmap, _, _ := procCreateDIBSection.Call(
		memdc,
		ptrToUint(unsafe.Pointer(&bmi)),
		DIB_RGB_COLORS,
		ptrToUint(unsafe.Pointer(&bits)),
		0,
		0,
	)
	if bitmap == 0 || bits == nil {
		return fmt.Errorf("CreateDIBSection 失败")
	}
	h.bitmap = bitmap

	copy(unsafe.Slice((*byte)(bits), len(pixels)), pixels)

	old, _, _ := procSelectObject.Call(memdc, bitmap)
	h.oldBitmap = old
	return nil
}

// haloWndProc 是光效窗口的窗口过程；纯函数，不依赖任何实例状态。
func haloWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	defer func() { _ = recover() }()
	switch uint32(msg) {
	case WM_NCHITTEST:
		return uintptr(^uintptr(0)) // HTTRANSPARENT == -1
	case WM_MOUSEACTIVATE:
		return MA_NOACTIVATE
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

func (h *HaloOverlay) createWindow() error {
	h.wndProcCb = syscall.NewCallback(haloWndProc)
	className := utf16Ptr(HALO_CLASS)
	wc := WNDCLASSEXW{
		CbSize:        uint32(unsafe.Sizeof(WNDCLASSEXW{})),
		LpfnWndProc:   h.wndProcCb,
		HInstance:     h.hinstance,
		LpszClassName: className,
	}
	r, _, errno := procRegisterClassExW.Call(ptrToUint(unsafe.Pointer(&wc)))
	runtime.KeepAlive(wc)
	runtime.KeepAlive(className)
	if r == 0 {
		const errClassAlreadyExists = syscall.Errno(1410)
		if errno != errClassAlreadyExists {
			return fmt.Errorf("RegisterClassExW 失败：%w", errno)
		}
	} else {
		h.classRegistered = true
	}

	exStyle := uintptr(WS_EX_LAYERED | WS_EX_TRANSPARENT | WS_EX_TOOLWINDOW |
		WS_EX_NOACTIVATE | WS_EX_TOPMOST)
	windowName := utf16Ptr("")
	hwnd, _, errno := procCreateWindowExW.Call(
		exStyle,
		ptrToUint(unsafe.Pointer(className)),
		ptrToUint(unsafe.Pointer(windowName)),
		WS_POPUP,
		0, 0, uintptr(h.size), uintptr(h.size),
		0, 0, uintptr(h.hinstance), 0,
	)
	runtime.KeepAlive(className)
	runtime.KeepAlive(windowName)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW 失败：%w", errno)
	}
	h.hwnd = windows.Handle(hwnd)
	return nil
}

// setVisible 显示/隐藏光效窗口；已显示且再次 visible 时只刷新一次位置/亮度。
func (h *HaloOverlay) setVisible(visible bool) {
	if !h.available {
		return
	}
	if visible == h.visible {
		if visible {
			h.render()
		}
		return
	}
	h.visible = visible
	if visible {
		procShowWindow.Call(uintptr(h.hwnd), SW_SHOWNOACTIVATE)
		procSetWindowPos.Call(
			uintptr(h.hwnd),
			uintptr(HWND_TOPMOST),
			0, 0, uintptr(h.size), uintptr(h.size),
			SWP_NOMOVE|SWP_NOSIZE|SWP_NOACTIVATE|SWP_SHOWWINDOW,
		)
		h.phase = 0.0
		h.render()
	} else {
		procShowWindow.Call(uintptr(h.hwnd), SW_HIDE)
	}
}

// tick 推进呼吸相位并刷新一帧。由 TIMER_ID_HALO 每 50ms 调用。
func (h *HaloOverlay) tick() {
	if !h.available || !h.visible {
		return
	}
	h.phase += (2.0 * math.Pi) / 32.0
	if h.phase >= 2.0*math.Pi {
		h.phase -= 2.0 * math.Pi
	}
	h.render()
}

// render 用当前 phase 计算 alpha，调用 UpdateLayeredWindow 把 memdc 贴到屏幕。
func (h *HaloOverlay) render() {
	if !h.available || !h.visible || h.hwnd == 0 {
		return
	}
	var pt POINT
	if ok, _, _ := procGetCursorPos.Call(ptrToUint(unsafe.Pointer(&pt))); ok == 0 {
		return
	}
	alpha := int(math.Round(205 + 50*math.Sin(h.phase)))
	if alpha < 1 {
		alpha = 1
	} else if alpha > 255 {
		alpha = 255
	}

	blend := BLENDFUNCTION{
		BlendOp:             AC_SRC_OVER,
		BlendFlags:          0,
		SourceConstantAlpha: uint8(alpha),
		AlphaFormat:         AC_SRC_ALPHA,
	}
	half := int32(h.size / 2)
	dst := POINT{X: pt.X - half, Y: pt.Y - half}
	sz := SIZE{CX: int32(h.size), CY: int32(h.size)}
	src := POINT{}

	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return
	}
	ok, _, errno := procUpdateLayeredWindow.Call(
		uintptr(h.hwnd),
		screenDC,
		ptrToUint(unsafe.Pointer(&dst)),
		ptrToUint(unsafe.Pointer(&sz)),
		h.memdc,
		ptrToUint(unsafe.Pointer(&src)),
		0,
		ptrToUint(unsafe.Pointer(&blend)),
		ULW_ALPHA,
	)
	procReleaseDC.Call(0, screenDC)
	if ok == 0 {
		logWarn("UpdateLayeredWindow 失败，关闭光效：%v", errno)
		h.available = false
		h.reason = "运行时 UpdateLayeredWindow 失败"
		procShowWindow.Call(uintptr(h.hwnd), SW_HIDE)
		h.visible = false
	}
}

// releaseResources 清理 GDI 与窗口句柄；可重复调用。
func (h *HaloOverlay) releaseResources() {
	if h.memdc != 0 && h.oldBitmap != 0 {
		procSelectObject.Call(h.memdc, h.oldBitmap)
		h.oldBitmap = 0
	}
	if h.bitmap != 0 {
		procDeleteObject.Call(h.bitmap)
		h.bitmap = 0
	}
	if h.memdc != 0 {
		procDeleteDC.Call(h.memdc)
		h.memdc = 0
	}
	if h.hwnd != 0 {
		procDestroyWindow.Call(uintptr(h.hwnd))
		h.hwnd = 0
	}
}

// close 隐藏并释放所有资源；classRegistered 为 true 时同步注销窗口类。
func (h *HaloOverlay) close() {
	if h.closed {
		return
	}
	h.closed = true
	if h.hwnd != 0 {
		procShowWindow.Call(uintptr(h.hwnd), SW_HIDE)
	}
	h.releaseResources()
	if h.classRegistered {
		className := utf16Ptr(HALO_CLASS)
		procUnregisterClassW.Call(
			ptrToUint(unsafe.Pointer(className)),
			uintptr(h.hinstance),
		)
		runtime.KeepAlive(className)
		h.classRegistered = false
	}
	h.available = false
}

// statusText 供 --diagnose 打印用。
func (h *HaloOverlay) statusText() string {
	if !h.enabled {
		return "光效：已禁用（--no-halo）"
	}
	return "光效：" + h.reason
}
