//go:build windows

// icons.go —— 从内嵌 base64(zlib(RGBA)) 生成带 alpha 的 HICON。
//
// 图标数据来自 Python 版的 TRAY_ICON_OFF_B64 / TRAY_ICON_ON_B64：
// 32x32 RGBA，zlib 压缩后 base64。运行时不需要外部 .ico 文件。
// 生成失败时回退到 IDI_APPLICATION（32512）。

package main

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"io"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const trayIconSize = 32

// 这两张 32x32 RGBA 图标与 Python 版完全一致，原样搬运。
const trayIconOffB64 = "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"
const trayIconOnB64 = "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"

// decodeEmbeddedIcon 解码 base64 -> zlib -> RGBA，校验长度。
func decodeEmbeddedIcon(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("base64 解码失败：%w", err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("zlib 初始化失败：%w", err)
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("zlib 读取失败：%w", err)
	}
	expected := trayIconSize * trayIconSize * 4
	if len(data) != expected {
		return nil, fmt.Errorf("图标数据长度错误：%d != %d", len(data), expected)
	}
	return data, nil
}

// createIconFromRGBA 把 RGBA 像素转成带 alpha 的 HICON（Windows 期望 BGRA DIB）。
func createIconFromRGBA(rgba []byte, size int) (windows.Handle, error) {
	if len(rgba) != size*size*4 {
		return 0, fmt.Errorf("RGBA 数据长度不匹配：%d", len(rgba))
	}

	// DIB 使用 BGRA；RGBA 与 BGRA 交换 R/B。
	bgra := make([]byte, len(rgba))
	for i := 0; i < len(rgba); i += 4 {
		bgra[i] = rgba[i+2]
		bgra[i+1] = rgba[i+1]
		bgra[i+2] = rgba[i]
		bgra[i+3] = rgba[i+3]
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
		return 0, fmt.Errorf("CreateCompatibleDC 失败")
	}
	defer procDeleteDC.Call(memdc)

	var bits unsafe.Pointer
	colorBmp, _, _ := procCreateDIBSection.Call(
		memdc,
		ptrToUint(unsafe.Pointer(&bmi)),
		DIB_RGB_COLORS,
		ptrToUint(unsafe.Pointer(&bits)),
		0,
		0,
	)
	if colorBmp == 0 || bits == nil {
		return 0, fmt.Errorf("CreateDIBSection 失败")
	}

	// 复制像素到 DIB section
	copy(unsafe.Slice((*byte)(bits), len(bgra)), bgra)

	oldBmp, _, _ := procSelectObject.Call(memdc, colorBmp)

	// 1bpp AND 掩码：alpha == 0 的像素置 1，保证旧版 Shell 透明。
	stride := ((size + 15) / 16) * 2
	maskBytes := make([]byte, stride*size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if bgra[(y*size+x)*4+3] == 0 {
				maskBytes[y*stride+x/8] |= 1 << uint(7-(x%8))
			}
		}
	}
	maskBmp, _, _ := procCreateBitmap.Call(
		uintptr(size), uintptr(size),
		1, 1,
		ptrToUint(unsafe.Pointer(&maskBytes[0])),
	)

	ii := ICONINFO{
		FIcon:    1,
		HbmMask:  windows.Handle(maskBmp),
		HbmColor: windows.Handle(colorBmp),
	}
	hicon, _, _ := procCreateIconIndirect.Call(ptrToUint(unsafe.Pointer(&ii)))

	// 清理 GDI 资源
	procSelectObject.Call(memdc, oldBmp)
	procDeleteObject.Call(colorBmp)
	if maskBmp != 0 {
		procDeleteObject.Call(maskBmp)
	}

	if hicon == 0 {
		return 0, fmt.Errorf("CreateIconIndirect 失败")
	}
	return windows.Handle(hicon), nil
}

// createTrayIcons 返回 (默认 HICON, 高亮 HICON, 状态说明)。
// 失败时回退到 IDI_APPLICATION（32512）；回退时两个 HICON 相同。
func createTrayIcons() (windows.Handle, windows.Handle, string) {
	offRaw, err1 := decodeEmbeddedIcon(trayIconOffB64)
	onRaw, err2 := decodeEmbeddedIcon(trayIconOnB64)
	if err1 != nil || err2 != nil {
		logWarn("解码内嵌图标失败：%v %v", err1, err2)
		return fallbackSystemIcon()
	}

	off, err := createIconFromRGBA(offRaw, trayIconSize)
	if err != nil {
		logWarn("生成默认托盘图标失败：%v", err)
		return fallbackSystemIcon()
	}
	on, err := createIconFromRGBA(onRaw, trayIconSize)
	if err != nil {
		logWarn("生成高亮托盘图标失败：%v", err)
		if off != 0 {
			procDestroyIcon.Call(uintptr(off))
		}
		return fallbackSystemIcon()
	}
	return off, on, "可用（内嵌双色图标）"
}

func fallbackSystemIcon() (windows.Handle, windows.Handle, string) {
	const idiApplication = 32512
	h, _, _ := procLoadIconW.Call(
		0,
		uintptr(idiApplication),
	)
	runtime.KeepAlive(h)
	icon := windows.Handle(h)
	return icon, icon, fmt.Sprintf("回退到系统图标(IDI_APPLICATION=%d)", idiApplication)
}