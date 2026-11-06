//go:build windows

// cmd.go —— 命令行解析、--quit / --show-tray / --diagnose 的实现。
//
// 与 Python 版 argparse 的行为对齐：
//   * --halo-color 解析失败 → 打印警告到 stderr，用默认值继续（不退出）；
//   * --halo-size 越界 → 钳制到 [32, 512] 并打印警告；
//   * --shift-threshold 非正整数 → 用默认 500ms（不报错）；
//   * --shift-key 非 right/left/both → 报错退出；
//   * --version / --help → 打印后 os.Exit(0)（与 argparse 一致）。

package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---------- Config ----------

// Config 汇总命令行参数。默认值在 parseArgs 里初始化，字段名与
// main.go 里引用的保持一致。
type Config struct {
	Diagnose         bool
	Quit             bool
	ShowTray         bool
	NoHalo           bool
	NoTray           bool
	NoTSF            bool
	Beep             bool
	Debug            bool
	HaloColor        HaloColor
	HaloSize         int
	ShiftKey         string
	ShiftThresholdMS int
	LogFile          string
}

// ---------- 参数解析 ----------

// parseArgs 解析命令行。支持 --key value 和 --key=value 两种形式。
// --version / --help 会直接 os.Exit(0)；其余错误返回给 main 处理。
func parseArgs(args []string) (*Config, error) {
	cfg := &Config{
		HaloColor:        HaloColor{0xFF, 0xB3, 0x47},
		HaloSize:         HALO_DEFAULT_SIZE,
		ShiftKey:         "right",
		ShiftThresholdMS: 500,
	}

	i := 0
	for i < len(args) {
		arg := args[i]
		i++

		// --key=value 拆解
		var inlineVal string
		hasInline := false
		if strings.HasPrefix(arg, "--") {
			if eq := strings.IndexByte(arg, '='); eq >= 0 {
				inlineVal = arg[eq+1:]
				arg = arg[:eq]
				hasInline = true
			}
		}

		takeValue := func() (string, error) {
			if hasInline {
				return inlineVal, nil
			}
			if i >= len(args) {
				return "", fmt.Errorf("%s 需要一个参数", arg)
			}
			v := args[i]
			i++
			return v, nil
		}

		switch arg {
		case "-h", "--help":
			printHelp()
			os.Exit(0)
		case "--version":
			fmt.Printf("%s %s\n", APP_NAME, APP_VERSION)
			os.Exit(0)

		case "--diagnose":
			cfg.Diagnose = true
		case "--quit":
			cfg.Quit = true
		case "--show-tray":
			cfg.ShowTray = true
		case "--no-halo":
			cfg.NoHalo = true
		case "--no-tray", "--hide-tray":
			cfg.NoTray = true
		case "--no-tsf":
			cfg.NoTSF = true
		case "--beep":
			cfg.Beep = true
		case "--debug":
			cfg.Debug = true

		case "--halo-color":
			raw, err := takeValue()
			if err != nil {
				return nil, err
			}
			c, perr := parseHaloColor(raw)
			if perr != nil {
				fmt.Fprintf(os.Stderr,
					"无效的 --halo-color %q：%v；改用默认值 #%s\n",
					raw, perr, HALO_DEFAULT_COLOR)
			} else {
				cfg.HaloColor = c
			}

		case "--halo-size":
			raw, err := takeValue()
			if err != nil {
				return nil, err
			}
			n, perr := strconv.Atoi(raw)
			if perr != nil {
				return nil, fmt.Errorf("--halo-size 需要整数，得到 %q", raw)
			}
			if n < HALO_MIN_SIZE {
				fmt.Fprintf(os.Stderr,
					"--halo-size %d 超出范围，已调整为 %d px\n",
					n, HALO_MIN_SIZE)
				n = HALO_MIN_SIZE
			}
			if n > HALO_MAX_SIZE {
				fmt.Fprintf(os.Stderr,
					"--halo-size %d 超出范围，已调整为 %d px\n",
					n, HALO_MAX_SIZE)
				n = HALO_MAX_SIZE
			}
			cfg.HaloSize = n

		case "--shift-key":
			raw, err := takeValue()
			if err != nil {
				return nil, err
			}
			if raw != "right" && raw != "left" && raw != "both" {
				return nil, fmt.Errorf(
					"--shift-key 必须是 right/left/both，得到 %q", raw)
			}
			cfg.ShiftKey = raw

		case "--shift-threshold":
			raw, err := takeValue()
			if err != nil {
				return nil, err
			}
			n, perr := strconv.Atoi(raw)
			if perr != nil || n <= 0 {
				n = 500
			}
			cfg.ShiftThresholdMS = n

		case "--log-file":
			raw, err := takeValue()
			if err != nil {
				return nil, err
			}
			cfg.LogFile = raw

		default:
			return nil, fmt.Errorf("未知参数: %s", arg)
		}
	}
	return cfg, nil
}

// ---------- 帮助 ----------

func printHelp() {
	fmt.Printf(`%s %s
小狼毫中文模式下将 CapsLock 锁存为左 Shift。

用法：
  rimecaps [选项]

选项：
  --diagnose                 只读检查当前前台输入法识别结果后退出
  --quit                     请求已运行的实例退出
  --show-tray                请求已运行的实例恢复托盘图标（隐藏后使用）
  --no-halo                  禁用鼠标周围的呼吸光晕，仅保留托盘变色
  --halo-color RRGGBB        光晕颜色，默认 %s
  --halo-size PX             光晕直径（像素），默认 %d
  --beep                     锁存/解除时播放系统提示音（默认关）
  --shift-key {right,left,both}
                             用于切换中英文的 Shift 键，默认 right
  --shift-threshold MS       Shift 短按判定阈值，默认 500ms
  --no-tsf                   禁用 TSF 补充检测，仅用 HKL 描述匹配
  --no-tray, --hide-tray     隐藏托盘图标（不创建托盘；用 --quit 退出）
  --debug                    输出调试日志
  --log-file PATH            额外把日志写入指定文件
  --version                  显示版本后退出
  -h, --help                 显示本帮助后退出
`, APP_NAME, APP_VERSION, HALO_DEFAULT_COLOR, HALO_DEFAULT_SIZE)
}

// ---------- --quit ----------

// runQuit 找到已运行实例的窗口并发送 WM_CLOSE，最多等 2 秒。
func runQuit() int {
	className := utf16Ptr(WINDOW_CLASS)
	hwnd, _, _ := procFindWindowW.Call(
		ptrToUint(unsafe.Pointer(className)),
		0,
	)
	runtime.KeepAlive(className)

	if hwnd == 0 {
		fmt.Println("RimeCapsShift 当前未运行。")
		return 0
	}

	ok, _, errno := procPostMessageW.Call(hwnd, WM_CLOSE, 0, 0)
	if ok == 0 {
		fmt.Printf("发送退出请求失败：%v\n", errno)
		return 1
	}

	exited := false
	for n := 0; n < 40; n++ {
		time.Sleep(50 * time.Millisecond)
		r, _, _ := procIsWindow.Call(hwnd)
		if r == 0 {
			exited = true
			break
		}
	}
	if exited {
		fmt.Println("已请求 RimeCapsShift 退出。")
	} else {
		fmt.Println("退出请求已发送，但进程在 2 秒内未退出。")
	}
	return 0
}

// ---------- --show-tray ----------

// runShowTray 找到已运行实例并发送 WM_APP_SHOW_TRAY。
func runShowTray() int {
	className := utf16Ptr(WINDOW_CLASS)
	hwnd, _, _ := procFindWindowW.Call(
		ptrToUint(unsafe.Pointer(className)),
		0,
	)
	runtime.KeepAlive(className)

	if hwnd == 0 {
		fmt.Println("RimeCapsShift 当前未运行。")
		return 0
	}

	ok, _, errno := procPostMessageW.Call(hwnd, WM_APP_SHOW_TRAY, 0, 0)
	if ok == 0 {
		fmt.Printf("发送显示托盘请求失败：%v\n", errno)
		return 1
	}
	fmt.Println("已请求显示 RimeCapsShift 托盘图标。")
	return 0
}

// ---------- --diagnose ----------

// runDiagnose 创建临时检测器/光效，打印诊断信息后清理退出。
// 不创建隐藏窗口、不装钩子、不写注册表，纯只读。
func runDiagnose(cfg *Config) int {
	enableDPIAwareness()

	detector := newWeaselDetector(!cfg.NoTSF)
	defer detector.close()

	// 颜色/大小已由 parseArgs 校验和钳制，这里直接用。
	color := cfg.HaloColor
	size := cfg.HaloSize
	if size < HALO_MIN_SIZE {
		size = HALO_MIN_SIZE
	}
	if size > HALO_MAX_SIZE {
		size = HALO_MAX_SIZE
	}

	// 图标：仅在启用托盘时生成，customIcons 才负责 Destroy。
	var (
		iconOff     windows.Handle
		iconOn      windows.Handle
		trayStatus  string
		customIcons bool
	)
	if cfg.NoTray {
		trayStatus = "未启用（--no-tray）"
	} else {
		iconOff, iconOn, trayStatus = createTrayIcons()
		customIcons = iconOff != iconOn
	}
	defer func() {
		if !customIcons {
			return
		}
		if iconOff != 0 {
			procDestroyIcon.Call(uintptr(iconOff))
		}
		if iconOn != 0 {
			procDestroyIcon.Call(uintptr(iconOn))
		}
	}()

	// 光效：仅在启用时创建。
	var (
		halo       *HaloOverlay
		haloStatus string
	)
	if cfg.NoHalo {
		haloStatus = "已禁用（--no-halo）"
	} else {
		halo = newHaloOverlay(mustModuleHandle(), true, color, size)
		haloStatus = halo.reason
	}
	if halo != nil {
		defer halo.close()
	}

	info := detector.foregroundInfo()
	final := detector.isWeaselForeground()

	chineseDesc := "非中文"
	if info.IsChineseHKL {
		chineseDesc = "中文"
	}

	fmt.Println("RimeCapsShift 诊断")
	fmt.Println(strings.Repeat("=", 44))
	fmt.Printf("前台 HKL          : 0x%08X\n", info.HKL)
	fmt.Printf("HKL 描述          : %q\n", info.Description)
	fmt.Printf("IME 文件名        : %q\n", info.IMEFile)
	fmt.Printf("HKL 语言 ID       : 0x%04X（%s）\n", info.LangID, chineseDesc)
	fmt.Printf("HKL 直接识别      : %s\n", yesNo(info.HKLMatch))

	if cfg.NoTSF {
		fmt.Println("TSF 检测可用      : 否（命令行 --no-tsf）")
	} else {
		fmt.Printf("TSF 检测可用      : %s\n", yesNo(detector.tsf != nil && detector.tsf.available))
		if detector.tsf != nil && !detector.tsf.available && detector.tsf.lastError != "" {
			fmt.Printf("TSF 错误          : %s\n", detector.tsf.lastError)
		}
		clsidStrs := make([]string, len(detector.candidates))
		for idx, g := range detector.candidates {
			clsidStrs[idx] = g.String()
		}
		fmt.Printf("Weasel CLSID 候选 : %s\n", strings.Join(clsidStrs, ", "))
		fmt.Printf("TSF 活动 Weasel   : %s\n", yesNo(info.TSFActive))
	}

	if final {
		fmt.Println("最终前台判定      : 小狼毫")
		fmt.Println("启动后状态        : 中文模式（CapsLock 锁存 Shift）")
	} else {
		fmt.Println("最终前台判定      : 不是小狼毫")
		fmt.Println("启动后状态        : 英文/非小狼毫（CapsLock 正常）")
	}

	fmt.Printf("Shift 监听键      : %s\n", cfg.ShiftKey)
	fmt.Printf("Shift 阈值        : %dms\n", cfg.ShiftThresholdMS)
	fmt.Println("钩子内 TSF 查询  : 已移除")
	fmt.Printf("托盘变色          : %s\n", trayStatus)
	fmt.Printf("光效              : %s\n", haloStatus)
	fmt.Printf("光效颜色          : #%02X%02X%02X\n", color.R, color.G, color.B)
	fmt.Printf("光效大小          : %d px\n", size)
	fmt.Printf("开机自启动        : %s（HKCU Run）\n", onOff(isAutostartEnabled()))
	fmt.Printf("提示音            : %s（--beep）\n", onOff(cfg.Beep))
	fmt.Printf("识别关键字        : %s\n", strings.Join(WEASEL_KEYWORDS, ", "))
	return 0
}

// ---------- 小工具 ----------

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

func onOff(b bool) string {
	if b {
		return "开"
	}
	return "关"
}
