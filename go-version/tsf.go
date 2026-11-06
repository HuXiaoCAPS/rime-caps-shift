//go:build windows

// tsf.go —— 小狼毫识别：HKL 描述匹配 + 注册表扫描 CLSID + TSF 活动配置查询。
//
// 识别策略（与 Python 版一致）：
//  1) 读取前台线程 HKL，用 ImmGetDescriptionW / ImmGetIMEFileNameW 匹配
//     Weasel / 小狼毫 / Rime；带缓存，因为 Hook 之外的 250ms 轮询会频繁调用。
//  2) 新版小狼毫是 TSF 文本服务，HKL 往往只是通用 0x08040804，Imm 描述为空；
//     额外用只读的 ITfInputProcessorProfiles::GetActiveLanguageProfile
//     检查当前活动配置是否为 Weasel。
//  3) 只有前台 HKL 属于中文语言时才采信 TSF 结果，避免切到英语键盘布局
//     时 TSF 仍报告 Weasel 活动。
//
// 全部只读；不修改系统输入法状态，不需要管理员权限。

package main

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// ---------- 关键字匹配 ----------

// rimeWordRe 匹配独立的 "rime" 词，避免把 "Prime"/"crime" 之类误判。
// Go 的 regexp 不支持 lookbehind，所以用 `\b` 近似：在 ASCII 字母上，
// \b 的边界语义正好匹配"前后不是 [a-z0-9_]"，而我们的关键字全是字母，
// 用 \b 足够；数字/下划线跟在 rime 后面（如 "rime64"）也视为词边界，
// 与 Python 版 `(?<![a-z])rime(?![a-z])` 在本场景完全等价。
var rimeWordRe = regexp.MustCompile(`\brime\b`)

// textMatchesWeasel 不区分大小写匹配关键字。
//   - "rime" 走词边界匹配；
//   - "weasel" / "小狼毫" 直接子串匹配（允许出现在文件名里）；
//   - 额外关键字按子串匹配。
func textMatchesWeasel(text string, keywords []string) bool {
	if text == "" {
		return false
	}
	lowered := strings.ToLower(text)
	for _, kw := range keywords {
		key := strings.ToLower(kw)
		switch key {
		case "rime":
			if rimeWordRe.MatchString(lowered) {
				return true
			}
		default:
			if strings.Contains(lowered, key) {
				return true
			}
		}
	}
	return false
}

// ---------- HKL 文本检测 ----------

var (
	hklCache   = map[uintptr]bool{}
	hklCacheMu sync.Mutex
)

// getHKLTexts 返回 (输入法描述, IME 文件名)。不支持的 HKL 返回空字符串。
func getHKLTexts(hkl uintptr) (string, string) {
	if hkl == 0 {
		return "", ""
	}
	descBuf := make([]uint16, 1024)
	fileBuf := make([]uint16, 1024)

	r, _, _ := procImmGetDescriptionW.Call(
		hkl,
		ptrToUint(unsafe.Pointer(&descBuf[0])),
		uintptr(len(descBuf)),
	)
	var desc string
	if r > 0 && int(r) <= len(descBuf) {
		desc = windows.UTF16ToString(descBuf[:r])
	}

	r, _, _ = procImmGetIMEFileNameW.Call(
		hkl,
		ptrToUint(unsafe.Pointer(&fileBuf[0])),
		uintptr(len(fileBuf)),
	)
	var file string
	if r > 0 && int(r) <= len(fileBuf) {
		file = windows.UTF16ToString(fileBuf[:r])
	}
	return desc, file
}

// isWeaselHKL 判断给定 HKL 是否对应小狼毫；结果带缓存。
func isWeaselHKL(hkl uintptr) bool {
	if hkl == 0 {
		return false
	}
	hklCacheMu.Lock()
	if v, ok := hklCache[hkl]; ok {
		hklCacheMu.Unlock()
		return v
	}
	hklCacheMu.Unlock()

	desc, file := getHKLTexts(hkl)
	matched := textMatchesWeasel(desc, WEASEL_KEYWORDS) ||
		textMatchesWeasel(file, WEASEL_KEYWORDS)

	hklCacheMu.Lock()
	hklCache[hkl] = matched
	hklCacheMu.Unlock()

	if matched {
		logDebug("HKL %#010x 直接识别为小狼毫: desc=%q file=%q", hkl, desc, file)
	}
	return matched
}

// getForegroundHKL 获取当前前台窗口所属线程的键盘布局 HKL。
func getForegroundHKL() uintptr {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return 0
	}
	var pid uint32
	threadID, _, _ := procGetWindowThreadProcessId.Call(
		hwnd, ptrToUint(unsafe.Pointer(&pid)),
	)
	if threadID == 0 {
		return 0
	}
	hkl, _, _ := procGetKeyboardLayout.Call(threadID)
	return hkl
}

// ---------- CLSID 扫描 ----------

var clsidRe = regexp.MustCompile(`^\{?[0-9A-Fa-f-]{36}\}?$`)

// discoverWeaselCLSIDs 从注册表只读扫描 Weasel 的 TSF CLSID，作为活动配置
// 检测候选。默认 CLSID 永远在第一位，之后按 CTF 枚举顺序追加。
func discoverWeaselCLSIDs(keywords []string) []GUID {
	var result []GUID
	seen := map[string]bool{}

	add := func(clsid string) {
		s := strings.TrimSpace(clsid)
		if !clsidRe.MatchString(s) {
			return
		}
		s = strings.ToUpper(strings.Trim(s, "{}"))
		if seen[s] {
			return
		}
		g, err := guidFromString(s)
		if err != nil {
			return
		}
		seen[s] = true
		result = append(result, g)
	}

	// 固定 CLSID 永远作为候选。
	add(WEASEL_DEFAULT_CLSID)

	ctfRoots := []struct {
		root registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\CTF\TIP`},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\CTF\TIP`},
		{registry.CURRENT_USER, `Software\Microsoft\CTF\TIP`},
	}
	classRoots := []struct {
		root registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SOFTWARE\Classes\CLSID`},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Classes\CLSID`},
		{registry.CURRENT_USER, `Software\Classes\CLSID`},
	}

	for _, ctf := range ctfRoots {
		tipKey, err := registry.OpenKey(ctf.root, ctf.path, registry.READ)
		if err != nil {
			continue
		}
		clsidNames, err := tipKey.ReadSubKeyNames(-1)
		tipKey.Close()
		if err != nil {
			continue
		}

		for _, clsid := range clsidNames {
			if !clsidRe.MatchString(strings.TrimSpace(clsid)) {
				continue
			}

			// (a) 类名 / InprocServer32 路径
			classMatched := false
			for _, cr := range classRoots {
				base := cr.path + `\` + clsid
				evidence := []string{
					readRegString(cr.root, base, ""),
					readRegString(cr.root, base+`\InprocServer32`, ""),
				}
				for _, v := range evidence {
					if textMatchesWeasel(v, keywords) {
						classMatched = true
						break
					}
				}
				if classMatched {
					break
				}
			}
			if classMatched {
				add(clsid)
				continue
			}

			// (b) LanguageProfile 中的 Description / IconFile
			profileRoot := ctf.path + `\` + clsid + `\LanguageProfile`
			pKey, err := registry.OpenKey(ctf.root, profileRoot, registry.READ)
			if err != nil {
				continue
			}
			langs, err := pKey.ReadSubKeyNames(-1)
			pKey.Close()
			if err != nil {
				continue
			}

			profileMatched := false
			for _, lang := range langs {
				langPath := profileRoot + `\` + lang
				lKey, err := registry.OpenKey(ctf.root, langPath, registry.READ)
				if err != nil {
					continue
				}
				profiles, err := lKey.ReadSubKeyNames(-1)
				lKey.Close()
				if err != nil {
					continue
				}
				for _, profile := range profiles {
					ppath := langPath + `\` + profile
					desc := readRegString(ctf.root, ppath, "Description")
					icon := readRegString(ctf.root, ppath, "IconFile")
					if textMatchesWeasel(desc, keywords) || textMatchesWeasel(icon, keywords) {
						profileMatched = true
						break
					}
				}
				if profileMatched {
					break
				}
			}
			if profileMatched {
				add(clsid)
			}
		}
	}

	parts := make([]string, len(result))
	for i, g := range result {
		parts[i] = g.String()
	}
	logDebug("Weasel TSF CLSID 候选: %s", strings.Join(parts, ", "))
	return result
}

func readRegString(root registry.Key, path, name string) string {
	k, err := registry.OpenKey(root, path, registry.READ)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return v
}

// ---------- TSF 活动配置查询 ----------

// ITfInputProcessorProfiles 只读 vtable。索引：
//   0..2  IUnknown (QueryInterface/AddRef/Release)
//   3..10 Register/Unregister/Add/Remove/EnumInputInfo/GetDefault/SetDefault/Activate
//   11    GetActiveLanguageProfile
type tfInputProcessorProfiles struct {
	vtbl *[12]uintptr
}

func (p *tfInputProcessorProfiles) release() {
	fn := p.vtbl[2] // IUnknown::Release
	syscall.SyscallN(fn, uintptr(unsafe.Pointer(p)))
}

// getActiveLanguageProfile 调用 vtable[11]。
// HRESULT GetActiveLanguageProfile(REFCLSID rclsid, LANGID *plangid, GUID *pguidProfile);
func (p *tfInputProcessorProfiles) getActiveLanguageProfile(
	clsid *GUID, langID *uint16, out *GUID,
) int32 {
	fn := p.vtbl[11]
	r, _, _ := syscall.SyscallN(
		fn,
		uintptr(unsafe.Pointer(p)), // this
		uintptr(unsafe.Pointer(clsid)),
		uintptr(unsafe.Pointer(langID)),
		uintptr(unsafe.Pointer(out)),
	)
	return int32(r)
}

// TSFProfileQuery 只读查询 ITfInputProcessorProfiles::GetActiveLanguageProfile。
type TSFProfileQuery struct {
	candidates    []GUID
	ptr           *tfInputProcessorProfiles
	coInitialized bool
	available     bool
	lastError     string
}

func newTSFProfileQuery(candidates []GUID) *TSFProfileQuery {
	q := &TSFProfileQuery{candidates: candidates}

	// 必须在 LockOSThread 的主线程上调用；与所有 Win32 调用同线程。
	hr, _, _ := procCoInitializeEx.Call(0, COINIT_APARTMENTTHREADED)
	hr32 := int32(hr)
	switch {
	case hr32 == S_OK || hr32 == S_FALSE:
		q.coInitialized = true
	case uint32(hr32) == RPC_E_CHANGED_MODE:
		q.lastError = "COM 已以其他模式初始化"
		return q
	default:
		q.lastError = fmt.Sprintf("CoInitializeEx HRESULT=%#x", uint32(hr32))
		return q
	}

	clsid, err := guidFromString("33C53A50-F456-4884-B049-85FD643ECFED")
	if err != nil {
		q.lastError = "CLSID_TF_InputProcessorProfiles 无效"
		return q
	}
	iid, err := guidFromString("1F02B6C5-7842-4EE6-8A0B-9A24183A95CA")
	if err != nil {
		q.lastError = "IID_ITfInputProcessorProfiles 无效"
		return q
	}

	var out uintptr
	hr, _, _ = procCoCreateInstance.Call(
		ptrToUint(unsafe.Pointer(&clsid)),
		0,
		CLSCTX_INPROC_SERVER,
		ptrToUint(unsafe.Pointer(&iid)),
		ptrToUint(unsafe.Pointer(&out)),
	)
	hr32 = int32(hr)
	if hr32 != S_OK || out == 0 {
		q.lastError = fmt.Sprintf("CoCreateInstance HRESULT=%#x", uint32(hr32))
		return q
	}

	q.ptr = (*tfInputProcessorProfiles)(unsafe.Pointer(out))
	q.available = true
	return q
}

// isWeaselActive 遍历候选 CLSID，任意一个 GetActiveLanguageProfile 返回
// S_OK 且 profile 非空即认为 Weasel 处于活动状态。
func (q *TSFProfileQuery) isWeaselActive() bool {
	if !q.available || q.ptr == nil {
		return false
	}
	for i := range q.candidates {
		var langID uint16
		var profile GUID
		hr := q.ptr.getActiveLanguageProfile(&q.candidates[i], &langID, &profile)
		if hr == S_OK && !profile.IsNull() {
			return true
		}
	}
	return false
}

func (q *TSFProfileQuery) close() {
	if q.ptr != nil {
		q.ptr.release()
		q.ptr = nil
	}
	q.available = false
	if q.coInitialized {
		procCoUninitialize.Call()
		q.coInitialized = false
	}
}

// ---------- 组合检测 ----------

// ForegroundInfo 是 foregroundInfo() 的返回结果，供 --diagnose 打印。
type ForegroundInfo struct {
	HKL           uintptr
	Description   string
	IMEFile       string
	LangID        uint16
	IsChineseHKL  bool
	HKLMatch      bool
	TSFActive     bool
	TSFAvailable  bool
	TSFLastError  string
}

// WeaselDetector 组合 HKL 与 TSF 活动配置判定当前前台是否为小狼毫。
type WeaselDetector struct {
	candidates []GUID
	tsf        *TSFProfileQuery
	lastHKL    uintptr
}

func newWeaselDetector(useTSF bool) *WeaselDetector {
	d := &WeaselDetector{
		candidates: discoverWeaselCLSIDs(WEASEL_KEYWORDS),
	}
	if useTSF {
		d.tsf = newTSFProfileQuery(d.candidates)
		if !d.tsf.available {
			logWarn("TSF 检测不可用：%s（将仅使用 HKL 描述识别）", d.tsf.lastError)
		}
	}
	return d
}

func (d *WeaselDetector) foregroundInfo() ForegroundInfo {
	hkl := getForegroundHKL()
	desc, file := getHKLTexts(hkl)
	langID := uint16(hkl & 0xFFFF)
	isChinese := (langID & 0x03FF) == 0x04 // LANG_CHINESE
	info := ForegroundInfo{
		HKL:          hkl,
		Description:  desc,
		IMEFile:      file,
		LangID:       langID,
		IsChineseHKL: isChinese,
		HKLMatch:     isWeaselHKL(hkl),
	}
	if d.tsf != nil {
		info.TSFAvailable = d.tsf.available
		info.TSFLastError = d.tsf.lastError
		info.TSFActive = d.tsf.isWeaselActive()
	}
	return info
}

func (d *WeaselDetector) isWeaselForeground() bool {
	info := d.foregroundInfo()
	if info.HKLMatch {
		return true
	}
	// TSF 活动配置在小狼毫安装中是全局的；切到英语键盘布局时它仍可能
	// 报告 Weasel 活动。因此只有前台 HKL 属于中文语言时才采信 TSF 结果。
	if info.IsChineseHKL && info.TSFActive {
		return true
	}
	return false
}

func (d *WeaselDetector) close() {
	if d.tsf != nil {
		d.tsf.close()
		d.tsf = nil
	}
}