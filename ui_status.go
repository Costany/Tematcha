// ui_status.go —— 输入栏与状态栏
//
// 资产来自 study/input-bar 项目：
//   - []rune 编辑（中文安全）+ slices.Insert/Delete
//   - 反色光标 + 闪烁（由主模型 500ms 心跳统一驱动）
//   - 水平滚动保证光标永远可见（horizontalWindow）
//   - 输入区 = 上下两条细线 + 提示符；每行单独渲染、单独缩进
//     （多行字符串缩进坑的正面示范）
//   - 状态栏左右对齐 = lipgloss.Width 差值补空格
package main

import (
	"fmt"
	"os"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// ---------------------------------------------------------------------------
// 输入框状态
// ---------------------------------------------------------------------------

// InputBar 是底部输入框的状态。
type InputBar struct {
	buf    []rune
	cursor int
}

// Text 返回当前输入文本。
func (ib *InputBar) Text() string { return string(ib.buf) }

// Clear 清空输入。
func (ib *InputBar) Clear() { ib.buf, ib.cursor = nil, 0 }

// SetText 整体替换输入内容（命令补全用；光标停在末尾）。
func (ib *InputBar) SetText(s string) {
	ib.buf = []rune(s)
	ib.cursor = len(ib.buf)
}

// Insert 在光标处插入文本（粘贴走这里）。
func (ib *InputBar) Insert(s string) {
	if s == "" {
		return
	}
	r := []rune(s)
	ib.buf = slices.Insert(ib.buf, ib.cursor, r...)
	ib.cursor += len(r)
}

// HandleKey 处理编辑类按键；返回 (handled, submit, cancel)。
// handled=false 表示这个键不归输入框管（滚动 / 退出键），交回主模型。
func (ib *InputBar) HandleKey(k tea.KeyPressMsg) (handled, submit, cancel bool) {
	switch k.String() {
	case "enter":
		return true, true, false
	case "esc":
		return true, false, true
	case "backspace":
		if ib.cursor > 0 {
			ib.buf = slices.Delete(ib.buf, ib.cursor-1, ib.cursor)
			ib.cursor--
		}
		return true, false, false
	case "delete":
		if ib.cursor < len(ib.buf) {
			ib.buf = slices.Delete(ib.buf, ib.cursor, ib.cursor+1)
		}
		return true, false, false
	case "left":
		if ib.cursor > 0 {
			ib.cursor--
		}
		return true, false, false
	case "right":
		if ib.cursor < len(ib.buf) {
			ib.cursor++
		}
		return true, false, false
	case "home", "ctrl+a":
		ib.cursor = 0
		return true, false, false
	case "end", "ctrl+e":
		ib.cursor = len(ib.buf)
		return true, false, false
	case "up", "down", "pgup", "pgdown", "ctrl+c":
		return false, false, false // 主模型处理（滚动 / 退出）
	}
	// 字符输入：msg.Text 非空 = 真的打出了字符（空格 / 中文 / 输入法全走这）
	if k.Text != "" {
		ib.Insert(k.Text)
	}
	return true, false, false
}

// ---------------------------------------------------------------------------
// 布局尺寸
// ---------------------------------------------------------------------------

// blockWidth 输入区总宽度（左右各留 2 格呼吸空间）。
func (m model) blockWidth() int {
	w := m.width - 4
	if w < 20 {
		w = 20
	}
	return w
}

// textWidth 输入文字可用宽度 = 总宽 - 提示符(1格) - 间隔(1格)。
func (m model) textWidth() int {
	w := m.blockWidth() - 2
	if w < 8 {
		w = 8
	}
	return w
}

// ---------------------------------------------------------------------------
// 输入区渲染
// ---------------------------------------------------------------------------

// renderInputBlock 输入区：上下两条细线 + 提示符行。
// 每一行单独渲染、单独带缩进 —— 绝不能写 "  " + 多行字符串！
func (m model) renderInputBlock() string {
	rule := "  " + ruleStyle.Render(strings.Repeat("\u2500", m.blockWidth()))
	row := m.renderPrompt() + " " + m.renderInputLine()

	// 手动补空格到固定宽度：光标闪烁时整行宽度不变（不会左右"呼吸"）
	if pad := m.blockWidth() - lipgloss.Width(row); pad > 0 {
		row += strings.Repeat(" ", pad)
	}
	return rule + "\n" + "  " + row + "\n" + rule
}

// renderPrompt 提示符（空输入时暗淡，打字后变绿；颜色随主题）。
func (m model) renderPrompt() string {
	st := promptOffStyle
	if len(m.input.buf) > 0 {
		st = promptOnStyle
	}
	return st.Render("\u276F")
}

// renderInputLine 输入行内容（含反色光标）。
func (m model) renderInputLine() string {
	if len(m.input.buf) == 0 {
		// 空输入：光标 + 灰色占位提示
		if m.blinkOn {
			return cursorStyle.Render(" ") + placeholderStyle.Render("输入消息，enter 发送")
		}
		return " " + placeholderStyle.Render("输入消息，enter 发送")
	}

	pre, at, post := m.horizontalWindow()
	if !m.blinkOn { // 闪烁的暗相：光标不显示
		return inputTextStyle.Render(pre + at + post)
	}
	if at == "" {
		at = " " // 末尾光标 = 反色空格
	}
	return inputTextStyle.Render(pre) + cursorStyle.Render(at) + inputTextStyle.Render(post)
}

// horizontalWindow 计算输入内容的"可视窗口"（长文本时水平滚动）：
// 返回 光标前文字 / 光标处字符（在末尾时为空） / 光标后文字。
// 核心规则：光标永远可见 —— 从光标往左倒着收，装不下就停。
func (m model) horizontalWindow() (pre, at, post string) {
	w := m.textWidth()

	curW := 1 // 光标格宽度：在末尾时用一个空格当光标
	if m.input.cursor < len(m.input.buf) {
		curW = lipgloss.Width(string(m.input.buf[m.input.cursor])) // 中文光标宽 2 格
	}

	// 光标左侧：从光标往前收
	budget := w - curW
	if budget < 0 {
		budget = 0
	}
	i, used := m.input.cursor, 0
	for i > 0 {
		rw := lipgloss.Width(string(m.input.buf[i-1]))
		if used+rw > budget {
			break
		}
		used += rw
		i--
	}
	pre = string(m.input.buf[i:m.input.cursor])

	// 光标右侧：拿剩下的宽度往后装
	rest := w - used - curW
	if rest < 0 {
		rest = 0
	}
	j := m.input.cursor
	if j < len(m.input.buf) {
		at = string(m.input.buf[j])
		j++
	}
	k, usedR := j, 0
	for k < len(m.input.buf) {
		rw := lipgloss.Width(string(m.input.buf[k]))
		if usedR+rw > rest {
			break
		}
		usedR += rw
		k++
	}
	post = string(m.input.buf[j:k])
	return pre, at, post
}

// ---------------------------------------------------------------------------
// 状态栏
// ---------------------------------------------------------------------------

// renderStatusBar 底部状态栏：左 = 引擎状态 +「模式徽章 | 上下文条 | 模型」，
// 右 = 快捷键提示（右对齐）。窄屏时左侧从尾部丢段（模型 → 上下文条 → 模式徽章）。
// 2026-09-19：letcode 品牌字样挪到右栏上方（照 crush），状态栏不再带前缀。
func (m model) renderStatusBar() string {
	right := dimStyle.Render(m.hintText())

	// 左段预算：总宽 - 左右边距 - 右段 - 至少 1 格间隙
	budget := m.width - 4 - lipgloss.Width(right) - 1
	// M11：忙时引擎状态段为空（动态信息在工作区）——左侧为空时整段留白。
	left := m.statusLeft(budget)

	pad := m.width - 4 - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return "  " + left + strings.Repeat(" ", pad) + right + "  "
}

// statusLeft 状态栏左侧内容（M3）：模态（权限/选择态）优先；
// 常规态 = 引擎状态 + 模式徽章 + 上下文条 + 模型，预算不够从尾部丢段。
func (m model) statusLeft(budget int) string {
	// 权限面板挂着时优先报"等待批准"（面板本身在输入区上方）
	if m.perm != nil {
		extra := ""
		if n := len(m.permQueue); n > 0 {
			extra = fmt.Sprintf("（还有 %d 个）", n)
		}
		return warnStyle.Render("等待批准" + extra)
	}
	// 工具卡键盘选择态：模式优先于引擎状态（当下在看的就是选中的卡）
	if m.cardNav != nil {
		i, n := m.cardNavPos()
		return userBarStyle.Render(fmt.Sprintf("选择工具卡 %d/%d", i, n))
	}

	// M11：引擎状态段可能为空（忙时动态信息在工作区）——空段不参与拼装，
	// 免得留下前导分隔符。
	segs := []string{}
	if s := m.engineStateText(); s != "" {
		segs = append(segs, s)
	}
	// M3c 输入队列：排队中的消息数（回合结束会自动接着发）。
	// 放在状态词后面——窄屏降级从尾部丢段，它会比模式/上下文/模型留得久。
	if n := len(m.queue); n > 0 {
		segs = append(segs, warnStyle.Render(fmt.Sprintf("排队 %d", n)))
	}
	if s := m.renderModeBadge(); s != "" {
		segs = append(segs, s)
	}
	if s := m.renderContextBar(); s != "" {
		segs = append(segs, s)
	}
	if s := m.renderModelSeg(); s != "" {
		segs = append(segs, s)
	}
	if len(segs) == 0 {
		return ""
	}
	// 降级：从尾部（模型）开始丢，直到装得下（至少保留最左一段）
	for len(segs) > 1 && lipgloss.Width(strings.Join(segs, " \u00B7 ")) > budget {
		segs = segs[:len(segs)-1]
	}
	return strings.Join(segs, dimStyle.Render(" \u00B7 "))
}

// engineStateText 引擎状态（§3 左段）：M11 起动态内容（工作行 / 压缩进度 /
// 收尾行）全部搬去输入栏上方的工作区（workStripLines），状态栏只留轻量静态
// 状态词——忙时为空（动态信息在工作区，避免两处重复）。
func (m model) engineStateText() string {
	if m.busy {
		return ""
	}
	switch m.status {
	case stIdle:
		return dimStyle.Render("空闲")
	case stLoading:
		return replyStyle.Render("载入会话…")
	case stEngineGone:
		return errStyle.Render("引擎已退出")
	}
	// 取消 / 出错 / 回合结束的收尾语都在工作区，这里不重复。
	return ""
}

// renderModeBadge 模式徽章：safe 柔绿 / default 中性 / auto 琥珀 / yolo 柔红。
func (m model) renderModeBadge() string {
	if m.modeID == "" {
		return ""
	}
	st := textStyle
	switch m.modeID {
	case "safe":
		st = toolOKStyle
	case "auto":
		st = warnStyle
	case "yolo":
		st = toolErStyle
	}
	return st.Render(m.modeID)
}

// renderContextBar 上下文条：miniBar + 百分比 + used/size。
// 填充段：绿档走柔绿渐变（§11.5 的 A → B → C，CIELAB 混合）；≥60% 整段转琥珀、
// ≥85% 转柔红（阈值见 §3）。百分比文字始终用阈值色。
func (m model) renderContextBar() string {
	if m.usageSize <= 0 {
		return ""
	}
	pct := m.shownPct() // M5d：压缩后缓动显示（动画中 = barPct）
	const barW = 10
	fill := pct * barW / 100
	st := toolOKStyle
	switch {
	case pct >= 85:
		st = toolErStyle
	case pct >= 60:
		st = warnStyle
	}
	var bar string
	switch {
	case pct >= 60:
		bar = st.Render(strings.Repeat("\u2588", fill))
	default:
		grad := contextBarGrad(barW)
		for i := 0; i < fill && i < len(grad); i++ {
			bar += lipgloss.NewStyle().Foreground(grad[i]).Render("\u2588")
		}
	}
	bar += ruleStyle.Render(strings.Repeat("\u2591", barW-fill))
	tail := dimStyle.Render(fmtK(m.usageUsed) + "/" + fmtK(m.usageSize))
	return bar + " " + st.Render(fmt.Sprintf("%d%%", pct)) + " " + tail
}

// renderModelSeg 模型段：模型名（有思考档时附在其后）。
func (m model) renderModelSeg() string {
	if m.modelLabel == "" {
		return ""
	}
	s := textStyle.Render(m.modelLabel)
	if m.reasoning != "" {
		s += dimStyle.Render(" \u00B7 ") + dimStyle.Render(m.reasoning)
	}
	return s
}

// ---------------------------------------------------------------------------
// -statustest：状态栏样张自检（多状态 / 多宽度 / 阈值色 / 背景色）
// ---------------------------------------------------------------------------

// runStatusTest 渲染状态栏在若干典型状态与宽度下的样张：
//   - 每行打印剥色文本与实际宽度（宽度必须等于终端宽，背景色必须干净）
//   - 覆盖：空闲/思考中/回复中 × default/auto/yolo × 绿/黄/红阈值 × 窄屏降级 × 模态优先
func runStatusTest() {
	cases := []struct {
		name string
		w    int
		m    model
	}{
		{"空闲 · default · 27%（绿）", 110, model{status: stIdle, modeID: "default", usageUsed: 87300, usageSize: 320000, modelLabel: "Step 3.7 Flash"}},
		{"忙 · auto · 91%（红）（动态信息在工作区）", 110, model{busy: true, status: stThinking, blinkN: 2, modeID: "auto", usageUsed: 291200, usageSize: 320000, modelLabel: "Step 3.7 Flash"}},
		{"忙 · yolo · 66%（黄）", 110, model{busy: true, status: stReplying, blinkN: 1, modeID: "yolo", usageUsed: 211200, usageSize: 320000, modelLabel: "Step 3.7 Flash", reasoning: "high"}},
		{"空闲 · safe · 无用量", 110, model{status: stIdle, modeID: "safe", modelLabel: "Step 3.7 Flash"}},
		{"窄屏 80（丢模型）", 80, model{status: stIdle, modeID: "default", usageUsed: 87300, usageSize: 320000, modelLabel: "Step 3.7 Flash"}},
		{"窄屏 64（再丢用量）", 64, model{status: stIdle, modeID: "default", usageUsed: 87300, usageSize: 320000, modelLabel: "Step 3.7 Flash"}},
		{"等待批准（模态优先）", 110, model{status: stIdle, modeID: "default", usageUsed: 87300, usageSize: 320000, modelLabel: "Step 3.7 Flash", perm: &PermRequest{Title: "shell__exec npm test"}, permQueue: []*PermRequest{{}, {}}}},
	}

	bad := false
	for _, c := range cases {
		mm := c.m
		mm.width = c.w
		line := mm.renderStatusBar()
		got, want := lipgloss.Width(line), c.w
		tag := "OK"
		if got != want {
			tag = "WIDTH!"
			bad = true
		}
		if hasBackgroundColor(line) {
			tag += " BG!"
			bad = true
		}
		fmt.Printf("[%s] %s（宽 %d/%d）\n", tag, c.name, got, want)
		fmt.Printf("    %s\n", stripANSI(line))
	}

	// 阈值色抽查：上下文条的百分比处应为对应颜色（SGR 逐字节核对）
	fmt.Println()
	fmt.Println("阈值色抽查（上下文条 SGR 颜色）：")
	for _, c := range []struct {
		used int64
		sgr  string
		name string
	}{
		{27000, "38;2;140;226;138", "体绿 #8CE28A"},
		{66000, "38;2;245;162;92", "暖橙 #F5A25C"},
		{91000, "38;2;201;123;123", "柔红 #C97B7B"},
	} {
		mm := model{status: stIdle, width: 110, usageUsed: c.used, usageSize: 100000}
		line := mm.renderStatusBar()
		ok := strings.Contains(line, c.sgr)
		if !ok {
			bad = true
		}
		fmt.Printf("  pct=%d%% 期望 %s → %v\n", c.used/1000, c.name, ok)
	}

	if bad {
		fmt.Println("statustest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("statustest: 全部通过")
}

// hintText 状态栏右侧快捷键提示（随忙闲 / 权限面板 / 选择态切换）。
func (m model) hintText() string {
	if m.perm != nil {
		return "y 允许一次  \u00B7  a 始终允许  \u00B7  n 拒绝  \u00B7  esc 取消"
	}
	if m.sessOn {
		return "\u2191\u2193 选择  \u00B7  enter 载入  \u00B7  esc 关闭"
	}
	if m.loading {
		return "会话载入中\u2026"
	}
	if m.palOpen() {
		return "\u2191\u2193 选择  \u00B7  tab 补全  \u00B7  enter 执行  \u00B7  esc 关闭"
	}
	if m.histOn {
		return "\u2191\u2193 翻历史  \u00B7  enter 发送  \u00B7  esc 清空"
	}
	if m.cardNav != nil {
		return "\u2191\u2193 选择卡片  \u00B7  enter 展开/收起  \u00B7  esc 退出"
	}
	if m.busy {
		return "esc 取消回合  \u00B7  ctrl+c 退出"
	}
	return "enter 发送  \u00B7  PgUp 回看历史  \u00B7  ctrl+c 退出"
}
