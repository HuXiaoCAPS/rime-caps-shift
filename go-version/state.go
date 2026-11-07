//go:build windows

// state.go —— 小狼毫中文状态 + CapsLock / Shift / Ctrl+Space 处理的状态机。
//
// 约定（与 Python 版完全一致）：
//   * chinese 只由 flipChinese 修改；初始化和输入法切换通过它的 force 参数重置。
//   * CapsLock 只负责吞键和注入左 Shift 的锁存状态，不修改 chinese。
//   * 被监听的 Shift 短按只通过 flipChinese 翻转 chinese，不吞键。
//   * HandleKey 内绝不写日志、不做 COM 查询；所有副作用收集到 pendingLogs /
//     pendingBeep / visualDirty，由定时器里的 flushPendingUI 在钩子外消费。

package main

import (
	"fmt"
	"time"
)

// ---------- 日志缓冲 ----------

const (
	logLevelInfo = iota
	logLevelWarn
)

type pendingLog struct {
	level int
	msg   string
}

// boolPtr 便于给 flipChinese 传 force。
func boolPtr(b bool) *bool { return &b }

// ---------- KeyboardHandler ----------

// KeyboardHandler 维护输入法中文状态与 CapsLock / Shift 行为。
type KeyboardHandler struct {
	detector *WeaselDetector
	injector *ShiftInjector
	visual   *VisualState
	beep     bool

	shiftKey         string // "right" / "left" / "both"
	shiftThresholdMS int    // 短按判定阈值

	// 输入法 / 锁存状态
	weaselForeground bool
	chinese          bool
	capsDown         bool
	capsIntercepted  bool
	spaceDown        bool
	shiftLatched     bool
	paused           bool

	// 物理 Shift 状态：钩子无条件追踪，与 shiftKey 是否监听无关。
	userLShiftDown bool
	userRShiftDown bool

	// 一个 Shift 周期共用的状态；shiftVK 记录本周期被监听的键。
	shiftPressedAt time.Time
	shiftUsed      bool
	shiftVK        uint32

	// 只在定时器中消费的 UI / 日志缓冲。
	pendingLogs   []pendingLog
	visualDirty   bool
	visualLatched bool
	pendingBeep   bool
}

// newKeyboardHandler 构造状态机。shiftKey / threshold 的合法性由 Config 校验，
// 这里做一次防御性检查以防 Config 构造路径绕过。
func newKeyboardHandler(
	detector *WeaselDetector,
	injector *ShiftInjector,
	beep bool,
	shiftKey string,
	thresholdMS int,
) *KeyboardHandler {
	if shiftKey != "right" && shiftKey != "left" && shiftKey != "both" {
		shiftKey = "right"
	}
	if thresholdMS <= 0 {
		thresholdMS = 500
	}
	return &KeyboardHandler{
		detector:         detector,
		injector:         injector,
		beep:             beep,
		shiftKey:         shiftKey,
		shiftThresholdMS: thresholdMS,
		visualDirty:      true,
	}
}

func (h *KeyboardHandler) setVisual(v *VisualState) { h.visual = v }

// ---------- 只读访问器（供 main.go / tray.go） ----------

func (h *KeyboardHandler) isChinese() bool          { return h.chinese }
func (h *KeyboardHandler) isWeaselForeground() bool { return h.weaselForeground }
func (h *KeyboardHandler) isShiftLatched() bool     { return h.shiftLatched }
func (h *KeyboardHandler) isPaused() bool           { return h.paused }

// ---------- 初始化 / 轮询 ----------

func (h *KeyboardHandler) initialize() bool {
	h.weaselForeground = h.detector.isWeaselForeground()
	h.resetShiftCycle()
	// 启动时如果用户已经按住 Shift，钩子看不到之前的 key-down；
	// 此时还没有 CapsLock 锁存注入，因此 GetAsyncKeyState 可以安全使用。
	h.userLShiftDown = getAsyncDown(VK_LSHIFT)
	h.userRShiftDown = getAsyncDown(VK_RSHIFT)

	reason := "启动检测（当前不是小狼毫）"
	if h.weaselForeground {
		reason = "启动检测（当前为小狼毫）"
	}
	h.flipChinese(reason, boolPtr(h.weaselForeground))
	return h.weaselForeground
}

// refreshForeground 轮询前台输入法；返回"状态是否发生变化"。
func (h *KeyboardHandler) refreshForeground() bool {
	now := h.detector.isWeaselForeground()
	if now == h.weaselForeground {
		if !now {
			h.ensureShiftRelease()
			h.setLatchedVisual(false)
		}
		return false
	}
	h.weaselForeground = now
	// 输入法切换后旧 Shift 周期不再可信；物理 Shift 状态不能清零，
	// 否则 Ctrl+Shift+Space 会误翻转。
	h.resetShiftCycle()
	if now {
		h.flipChinese("输入法切换到小狼毫", boolPtr(true))
	} else {
		h.flipChinese("输入法离开小狼毫", boolPtr(false))
	}
	return true
}

// ---------- 副作用缓冲 ----------

func (h *KeyboardHandler) queueLog(level int, msg string) {
	if len(h.pendingLogs) < 128 {
		h.pendingLogs = append(h.pendingLogs, pendingLog{level, msg})
	}
}

// flushPendingUI 在钩子外消费日志、托盘/光效更新和提示音。
func (h *KeyboardHandler) flushPendingUI() {
	if h.visualDirty {
		h.visualDirty = false
		if h.visual != nil {
			h.visual.setLatched(h.visualLatched)
		}
	}
	if h.pendingBeep {
		h.pendingBeep = false
		if h.beep {
			procMessageBeep.Call(MB_OK)
		}
	}
	pending := h.pendingLogs
	h.pendingLogs = nil
	for _, e := range pending {
		switch e.level {
		case logLevelWarn:
			logWarn("%s", e.msg)
		default:
			logInfo("%s", e.msg)
		}
	}
}

// ---------- 状态翻转 ----------

// flipChinese 是唯一修改 chinese 的入口。
func (h *KeyboardHandler) flipChinese(reason string, force *bool) {
	var newState bool
	if force == nil {
		newState = !h.chinese
	} else {
		newState = *force
	}
	h.chinese = newState
	h.releaseLatchedShift(reason)
	h.setLatchedVisual(h.chinese && h.shiftLatched)
	h.visualDirty = true
	h.queueLog(logLevelInfo, fmt.Sprintf("%s：is_rime_chinese=%v", reason, h.chinese))
}

// setLatchedVisual 只标记脏状态，由定时器实际应用。
func (h *KeyboardHandler) setLatchedVisual(on bool) {
	desired := on && h.chinese
	if desired != h.visualLatched {
		h.visualLatched = desired
		h.visualDirty = true
	}
}

func (h *KeyboardHandler) releaseLatchedShift(reason string) {
	wasLatched := h.shiftLatched
	h.shiftLatched = false
	if h.injector.isDown() {
		if !h.injector.release() {
			h.queueLog(logLevelWarn, fmt.Sprintf(
				"Shift Up 发送失败（%v），稍后重试", h.injector.lastError,
			))
		}
	}
	h.setLatchedVisual(false)
	if wasLatched {
		h.queueLog(logLevelInfo, "解除 Shift 锁定："+reason)
	}
}

// ---------- Ctrl+Space ----------

func (h *KeyboardHandler) ctrlIsDown() bool {
	ctrl := getAsyncDown(VK_CONTROL)
	alt := getAsyncDown(VK_MENU)
	return ctrl && !alt
}

func (h *KeyboardHandler) anyUserShiftDown() bool {
	return h.userLShiftDown || h.userRShiftDown
}

func (h *KeyboardHandler) handleSpace(isDown bool) {
	if isDown {
		if h.spaceDown {
			return // 自动重复，只翻转一次
		}
		h.spaceDown = true
		if !h.ctrlIsDown() {
			return
		}
		if !h.weaselForeground {
			return
		}
		// 只用物理 Shift 状态，不用 GetAsyncKeyState：后者会把 CapsLock
		// 锁存注入的左 Shift 误判为用户按着 Shift。
		if h.anyUserShiftDown() {
			return
		}
		h.flipChinese("Ctrl+Space", nil)
	} else {
		h.spaceDown = false
	}
}

// ---------- Shift 短按 ----------

func (h *KeyboardHandler) shiftIsMonitored(vkCode uint32) bool {
	switch h.shiftKey {
	case "left":
		return vkCode == VK_LSHIFT
	case "both":
		return vkCode == VK_LSHIFT || vkCode == VK_RSHIFT
	default:
		return vkCode == VK_RSHIFT
	}
}

func (h *KeyboardHandler) resetShiftCycle() {
	h.shiftPressedAt = time.Time{}
	h.shiftUsed = false
	h.shiftVK = 0
}

func (h *KeyboardHandler) handleShiftDown(vkCode uint32) {
	if !h.shiftPressedAt.IsZero() {
		return
	}
	if !h.shiftIsMonitored(vkCode) {
		return
	}
	h.shiftPressedAt = time.Now()
	h.shiftUsed = false
	h.shiftVK = vkCode
}

func (h *KeyboardHandler) handleShiftUp(vkCode uint32) {
	if h.shiftPressedAt.IsZero() || vkCode != h.shiftVK {
		return
	}
	started := h.shiftPressedAt
	used := h.shiftUsed
	h.resetShiftCycle()
	elapsed := time.Since(started)
	if h.weaselForeground && !used &&
		elapsed < time.Duration(h.shiftThresholdMS)*time.Millisecond {
		which := "右 Shift"
		if vkCode == VK_LSHIFT {
			which = "左 Shift"
		}
		h.flipChinese(fmt.Sprintf("%s短按 %dms", which, elapsed.Milliseconds()), nil)
	}
}

// ---------- CapsLock ----------

// handleCaps 只锁存/解除注入的左 Shift，不修改 chinese。
func (h *KeyboardHandler) handleCaps(isDown bool) bool {
	if isDown {
		if h.capsDown {
			return h.capsIntercepted // Caps 自动重复
		}
		h.capsDown = true
		if !h.chinese {
			h.capsIntercepted = false
			return false
		}
		// 中文模式下始终吞掉 CapsLock，避免重新触发小狼毫的编辑键问题。
		h.capsIntercepted = true
		if h.shiftLatched {
			h.shiftLatched = false
			if !h.injector.release() {
				h.queueLog(logLevelWarn, fmt.Sprintf(
					"Shift Up 发送失败（%v），稍后重试", h.injector.lastError,
				))
			}
			h.setLatchedVisual(false)
			h.pendingBeep = true
			h.queueLog(logLevelInfo, "CapsLock：解除 Shift 锁定")
		} else {
			if h.injector.press() {
				h.shiftLatched = true
				h.setLatchedVisual(true)
				h.pendingBeep = true
				h.queueLog(logLevelInfo, "CapsLock：锁定 Shift")
			} else {
				h.queueLog(logLevelWarn, fmt.Sprintf(
					"Shift Down 发送失败（%v）；本次 CapsLock 仍被吞掉",
					h.injector.lastError,
				))
			}
		}
		return true
	}
	h.capsDown = false
	if h.capsIntercepted {
		h.capsIntercepted = false
		return true
	}
	return false
}

// ---------- 入口：由 hook.go 调用 ----------

// HandleKey 处理一个物理按键；返回 true 时钩子吞掉该事件。
func (h *KeyboardHandler) HandleKey(vkCode uint32, isDown bool) bool {
	// 暂停时完全放行，CapsLock / Shift / Ctrl+Space 都交还给系统和输入法。
	if h.paused {
		return false
	}

	// 无条件追踪物理左右 Shift 状态，供 Ctrl+Space 组合键判定使用。
	// 必须发生在任何吞键/翻转逻辑之前，且不依赖 --shift-key。
	switch vkCode {
	case VK_LSHIFT:
		h.userLShiftDown = isDown
	case VK_RSHIFT:
		h.userRShiftDown = isDown
	}

	// Shift 周期内的污染标记必须先于 CapsLock / Space 处理。
	if isDown && vkCode != VK_LSHIFT && vkCode != VK_RSHIFT && vkCode != VK_SHIFT {
		if !h.shiftPressedAt.IsZero() {
			h.shiftUsed = true
		}
	}

	switch vkCode {
	case VK_LSHIFT, VK_RSHIFT:
		if isDown {
			h.handleShiftDown(vkCode)
		} else {
			h.handleShiftUp(vkCode)
		}
		return false
	case VK_CAPITAL:
		return h.handleCaps(isDown)
	case VK_SPACE:
		h.handleSpace(isDown)
	}
	return false
}

// setPaused 暂停/恢复 CapsLock、Shift、Ctrl+Space 的拦截与翻转。
// 暂停时释放已锁存的 Shift，恢复时重新采样物理按键和前台输入法。
func (h *KeyboardHandler) setPaused(paused bool) {
	if paused == h.paused {
		return
	}
	h.paused = paused
	h.resetShiftCycle()
	h.capsDown = false
	h.capsIntercepted = false
	h.spaceDown = false

	if paused {
		h.releaseLatchedShift("暂停")
		h.visualDirty = true
		h.queueLog(logLevelInfo, "已暂停：CapsLock/Shift 交还系统")
		return
	}

	h.userLShiftDown = getAsyncDown(VK_LSHIFT)
	h.userRShiftDown = getAsyncDown(VK_RSHIFT)
	h.weaselForeground = h.detector.isWeaselForeground()
	h.flipChinese("恢复", boolPtr(h.weaselForeground))
}

// ---------- 退出 / 保护 ----------

func (h *KeyboardHandler) shutdown() {
	h.paused = false
	h.capsDown = false
	h.capsIntercepted = false
	h.spaceDown = false
	h.shiftLatched = false
	h.resetShiftCycle()
	h.userLShiftDown = false
	h.userRShiftDown = false
	h.injector.releaseBestEffort()
	h.setLatchedVisual(false)
}

// ensureShiftRelease 由定时器周期调用：Shift 不该锁定时若释放失败则重试。
func (h *KeyboardHandler) ensureShiftRelease() {
	if !h.injector.isDown() {
		if !h.chinese {
			h.setLatchedVisual(false)
		}
		return
	}
	if h.chinese && h.shiftLatched {
		return
	}
	h.shiftLatched = false
	if h.injector.release() {
		h.setLatchedVisual(false)
	} else {
		h.queueLog(logLevelWarn, fmt.Sprintf(
			"补发 Shift Up 失败（%v）", h.injector.lastError,
		))
	}
}

// syncVisual 周期性强制刷新托盘提示（防止仅输入法状态变化时遗漏）。
func (h *KeyboardHandler) syncVisual() {
	h.setLatchedVisual(h.chinese && h.shiftLatched)
	h.visualDirty = true
	h.flushPendingUI()
}

// ---------- 状态文本 ----------

func (h *KeyboardHandler) statusShort() string {
	if h.paused {
		return "已暂停"
	}
	if !h.weaselForeground {
		return "非小狼毫，CapsLock 正常"
	}
	if h.chinese {
		if h.shiftLatched {
			return "小狼毫 中文，Shift 已锁定"
		}
		return "小狼毫 中文，CapsLock 锁定 Shift"
	}
	return "小狼毫 英文，CapsLock 正常"
}

func (h *KeyboardHandler) statusMenu() string {
	if h.paused {
		return "已暂停"
	}
	if !h.weaselForeground {
		return "非小狼毫"
	}
	if h.chinese {
		if h.shiftLatched {
			return "中文·已锁定"
		}
		return "中文·未锁定"
	}
	return "英文"
}
