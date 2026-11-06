//go:build windows

// autostart.go —— 开机自启动（仅 HKCU，普通用户权限）。
//
// 命令行参数复用当前进程的参数，但过滤掉一次性/调试参数：
//   --debug --diagnose --quit --show-tray --log-file <PATH>
//   --log-file=<PATH>
// 拼接方式与 Python subprocess.list2cmdline 一致；包含空格/引号的参数
// 用双引号括起，内部的 \ 与 " 按 Windows 规则转义。

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

// autostartArgs 复用当前参数，去掉一次性/调试参数。
func autostartArgs() []string {
	var result []string
	skipNext := false
	for _, arg := range os.Args[1:] {
		if skipNext {
			skipNext = false
			continue
		}
		switch arg {
		case "--debug", "--diagnose", "--quit", "--show-tray":
			continue
		case "--log-file":
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "--log-file=") {
			continue
		}
		result = append(result, arg)
	}
	return result
}

// quoteWinArg 是 Windows 命令行拼接的简化实现（与 subprocess.list2cmdline
// 的转义规则一致）：普通参数原样返回；含空格/制表符/换行/引号的参数用
// 双引号括起，反斜杠与双引号按 Windows 规则加倍。
func quoteWinArg(s string) string {
	if s == "" {
		return `""`
	}
	needQuote := false
	for _, c := range s {
		if c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '"' {
			needQuote = true
			break
		}
	}
	if !needQuote {
		return s
	}

	var b strings.Builder
	b.WriteByte('"')
	bslashes := 0
	for _, c := range s {
		switch c {
		case '\\':
			bslashes++
		case '"':
			// 引号前的 N 个反斜杠要写成 2N+1 个，再跟一个引号。
			b.WriteString(strings.Repeat(`\`, bslashes*2+1))
			b.WriteByte('"')
			bslashes = 0
		default:
			b.WriteString(strings.Repeat(`\`, bslashes))
			b.WriteRune(c)
			bslashes = 0
		}
	}
	// 参数结尾的 N 个反斜杠要写成 2N 个，避免把结尾引号转义掉。
	b.WriteString(strings.Repeat(`\`, bslashes*2))
	b.WriteByte('"')
	return b.String()
}

// autostartCommand 生成写入注册表的完整命令行。
func autostartCommand() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return ""
	}
	parts := []string{quoteWinArg(exe)}
	for _, a := range autostartArgs() {
		parts = append(parts, quoteWinArg(a))
	}
	return strings.Join(parts, " ")
}

// readAutostartCommand 读取 HKCU Run 下的当前值；不存在或读取失败返回 ""。
func readAutostartCommand() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, AUTOSTART_KEY, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue(AUTOSTART_VALUE)
	if err != nil {
		return ""
	}
	return v
}

func isAutostartEnabled() bool {
	return readAutostartCommand() != ""
}

// setAutostartEnabled 写入/删除 HKCU\Run 下的自启动值。返回是否成功。
func setAutostartEnabled(enabled bool) bool {
	k, _, err := registry.CreateKey(
		registry.CURRENT_USER, AUTOSTART_KEY,
		registry.SET_VALUE|registry.QUERY_VALUE,
	)
	if err != nil {
		logWarn("打开开机自启动注册表失败：%v", err)
		return false
	}
	defer k.Close()

	if enabled {
		cmd := autostartCommand()
		if cmd == "" {
			logWarn("无法生成开机自启动命令行")
			return false
		}
		if err := k.SetStringValue(AUTOSTART_VALUE, cmd); err != nil {
			logWarn("写入开机自启动失败：%v", err)
			return false
		}
		logInfo("已开启开机自启动：%s", cmd)
		return true
	}

	if err := k.DeleteValue(AUTOSTART_VALUE); err != nil {
		// 值本来就不存在，视为成功。
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, registry.ErrNotExist) {
			logInfo("已关闭开机自启动（原未启用）")
			return true
		}
		logWarn("删除开机自启动失败：%v", err)
		return false
	}
	logInfo("已关闭开机自启动")
	return true
}
