// ui_cmd.go —— 命令弹层（M4b · §7）
//
// 数据来自引擎：会话建立后 letcode 发 available_commands_update（形状对过实机 trace）：
//
//	{"sessionUpdate":"available_commands_update","availableCommands":[
//	  {"name":"permission","description":"Set the session permission mode",
//	   "input":{"hint":"safe|default|auto|yolo"}},
//	  {"name":"compact","description":"Compact the session context"}, …]}
//
// 命令以 prompt 文本下发（引擎侧 slash.rs 负责分派：不带值的命令回 "Usage: /…"，
// ACP 不接受的命令回 "… is not available over ACP"，未知文本照常当提问）。
// 弹层只在输入停在"命令词"上时出现（/ 开头、还没打空格）；esc 可临时关掉。
//
// 透明度原则：只前景色（选中 ❯ 强调绿、参数提示琥珀、描述暗灰）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// cmdPaletteRows 弹层最多显示的行数（spec §7：最多 8 行）。
const cmdPaletteRows = 8

// AvailCmd 引擎广告的一条斜杠命令。
type AvailCmd struct {
	Name string // 命令名（不含 /）
	Desc string // 描述（引擎原文）
	Hint string // 参数提示（input.hint）；空 = 不带参数
}

// parseCommands 解析 availableCommands（字段名对过实机 trace：input 下只有 hint）。
func parseCommands(items []any) []AvailCmd {
	out := make([]AvailCmd, 0, len(items))
	for _, it := range items {
		im := asMap(it)
		if im == nil {
			continue
		}
		name, _ := im["name"].(string)
		if name == "" {
			continue
		}
		desc, _ := im["description"].(string)
		hint := ""
		if in := asMap(im["input"]); in != nil {
			hint, _ = in["hint"].(string)
		}
		out = append(out, AvailCmd{Name: name, Desc: desc, Hint: hint})
	}
	return out
}

// ---------------------------------------------------------------------------
// 弹层状态
// ---------------------------------------------------------------------------

// cmdToken 取"命令词"：输入以 / 开头且还没打空格时，返回 / 后面的部分。
// 一旦出现空格（正在写参数）就不再算命令词——弹层收起，enter 照常发送。
func cmdToken(text string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	rest := text[1:]
	if strings.ContainsAny(rest, " \t") {
		return "", false
	}
	return rest, true
}

// palOpen 弹层是否显示：有命令数据、输入停在命令词上、没被 esc 关掉，
// 且权限面板 / 工具卡选择态这类模态不在场（模态优先）。
func (m model) palOpen() bool {
	if m.palHidden || len(m.cmds) == 0 || m.perm != nil || m.cardNav != nil {
		return false
	}
	_, ok := cmdToken(m.input.Text())
	return ok
}

// palMatches 返回过滤后的命令下标（前缀匹配、大小写不敏感；命令词为空 = 全部）。
func (m model) palMatches() []int {
	token, _ := cmdToken(m.input.Text())
	lt := strings.ToLower(token)
	var out []int
	for i, c := range m.cmds {
		if lt == "" || strings.HasPrefix(strings.ToLower(c.Name), lt) {
			out = append(out, i)
		}
	}
	return out
}

// palSelAt 把选中下标收拢进过滤结果的合法范围（选中 = 过滤结果里的第几条）。
func (m model) palSelAt(n int) int {
	s := m.palSel
	if s < 0 {
		s = 0
	}
	if n == 0 {
		return 0
	}
	if s >= n {
		s = n - 1
	}
	return s
}

// cmdLabel 命令的「/名字 提示」纯文本（算宽度用）。
// withHint=false 时只给名字（窄屏降级：名字列超过半屏就先丢提示）。
func (m model) cmdLabel(c AvailCmd, withHint bool) string {
	s := "/" + c.Name
	if withHint && c.Hint != "" {
		s += " " + c.Hint
	}
	return s
}

// cmdLabelStyled 同上但上色：名字浅色、参数提示琥珀。
func (m model) cmdLabelStyled(c AvailCmd, withHint bool) string {
	s := textStyle.Render("/" + c.Name)
	if withHint && c.Hint != "" {
		s += " " + warnStyle.Render(c.Hint)
	}
	return s
}

// ---------------------------------------------------------------------------
// 渲染
// ---------------------------------------------------------------------------

// renderCmdPalette 渲染命令弹层（钉在输入区上方；高度已在 syncLayout 里从消息区扣出）：
//
//	❯ /permission  safe|default|auto|yolo   Set the session permission mode
//	  /model       model id                 Switch the model the session runs with
//
// 选中行 ❯ 强调绿；描述暗灰、放不下就先丢描述、再丢参数提示（窄屏降级）。
// 超过 8 条：显示前 7 条 +「… 还有 N 条」；过滤到空：一行提示。
func (m model) renderCmdPalette() []string {
	if !m.palOpen() {
		return nil
	}
	idx := m.palMatches()
	if len(idx) == 0 {
		return []string{"  " + dimStyle.Render("没有匹配的命令 \u00B7 esc 关闭")}
	}
	sel := m.palSelAt(len(idx))

	shown, more := idx, 0
	if len(shown) > cmdPaletteRows {
		more = len(shown) - (cmdPaletteRows - 1)
		shown = shown[:cmdPaletteRows-1]
	}

	budget := m.blockWidth()
	withHint := true
	nameW := 0
	for _, i := range shown {
		if w := lipgloss.Width(m.cmdLabel(m.cmds[i], true)); w > nameW {
			nameW = w
		}
	}
	// 名字列（含提示）超过 3/4 宽度才丢提示（reasoning 的提示有 38 字符，
	// 阈值定太严会把宽屏的提示也一起砍掉）；窄屏时这里必然触发。
	if nameW > budget*3/4 {
		withHint = false
		nameW = 0
		for _, i := range shown {
			if w := lipgloss.Width(m.cmdLabel(m.cmds[i], false)); w > nameW {
				nameW = w
			}
		}
	}
	descW := budget - 4 - nameW - 2
	if descW < 12 {
		descW = 0
	}

	out := make([]string, 0, len(shown)+1)
	for row, i := range shown {
		c := m.cmds[i]
		mark := " "
		if row == sel {
			mark = userBarStyle.Render("\u276F") // ❯
		}
		label := m.cmdLabel(c, withHint)
		pad := nameW - lipgloss.Width(label)
		if pad < 0 {
			pad = 0
		}
		line := "  " + mark + " " + m.cmdLabelStyled(c, withHint) + strings.Repeat(" ", pad)
		if descW > 0 && c.Desc != "" {
			line += "  " + dimStyle.Render(clipWidth(c.Desc, descW))
		}
		out = append(out, line)
	}
	if more > 0 {
		out = append(out, "  "+dimStyle.Render(fmt.Sprintf("\u2026 还有 %d 条", more)))
	}
	return out
}

// ---------------------------------------------------------------------------
// 键位
// ---------------------------------------------------------------------------

// palKey 处理弹层打开时的按键，返回 (handled, submit)：
//
//	↑↓     选择（夹取、不绕圈）
//	tab    补全（带参数补到 "/名字 "，光标留在末尾）
//	enter  无参数命令 = 直接执行；带参数命令 = 先补全（参数交给用户填）
//	esc    临时关掉弹层（输入文字保留；再打字即恢复）
//
// 其余键返回 handled=false，照常落进输入框（过滤实时更新）。
func (m *model) palKey(k tea.KeyPressMsg) (handled, submit bool) {
	idx := m.palMatches()
	sel := m.palSelAt(len(idx))
	switch k.String() {
	case "up":
		if sel > 0 {
			m.palSel = sel - 1
		}
		return true, false
	case "down":
		if len(idx) > 0 && sel < len(idx)-1 {
			m.palSel = sel + 1
		}
		return true, false
	case "esc":
		m.palHidden = true
		return true, false
	case "tab":
		if len(idx) > 0 {
			m.completeCmd(m.cmds[idx[sel]])
		}
		return true, false
	case "enter":
		if len(idx) == 0 {
			return false, false // 没有匹配：当普通文本发送（引擎自己解释）
		}
		c := m.cmds[idx[sel]]
		m.completeCmd(c)
		return true, c.Hint == "" // 无参数：直接执行
	}
	return false, false
}

// completeCmd 把命令写进输入框（带参数时补一个空格，光标停在末尾等填参数）。
func (m *model) completeCmd(c AvailCmd) {
	t := "/" + c.Name
	if c.Hint != "" {
		t += " "
	}
	m.input.SetText(t)
	m.palSel, m.palHidden = 0, false
}

// ---------------------------------------------------------------------------
// -cmdtest：弹层数据 / 过滤 / 渲染 / 键位自检（无 TTY / 无引擎）
// ---------------------------------------------------------------------------

// engineCmds 样张 = 引擎广告的 9 条命令（照 slash.rs COMMANDS 抄，名字与提示逐字一致）。
func engineCmds() []AvailCmd {
	return []AvailCmd{
		{Name: "permission", Desc: "Set the session permission mode", Hint: "safe|default|auto|yolo"},
		{Name: "model", Desc: "Switch the model the session runs with", Hint: "model id"},
		{Name: "reasoning", Desc: "Set the reasoning effort of the model the session runs with", Hint: "off|none|minimal|low|medium|high|xhigh"},
		{Name: "compact", Desc: "Compact the session context"},
		{Name: "fast", Desc: "Toggle fast mode"},
		{Name: "new", Desc: "Start a new session"},
		{Name: "resume", Desc: "Resume a session by id", Hint: "session id"},
		{Name: "undo", Desc: "Undo the last turn"},
		{Name: "redo", Desc: "Redo the last undone turn"},
	}
}

// runCmdTest 断言命令弹层的数据层、过滤、渲染与键位，全部通过才退出码 0。
func runCmdTest() {
	failed := false
	check := func(name string, cond bool) {
		tag := "PASS"
		if !cond {
			tag = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", tag, name)
	}

	// ① 解析（样张含：带 hint / 无 input / 无名条目）
	cmds := parseCommands([]any{
		map[string]any{"name": "permission", "description": "Set the session permission mode", "input": map[string]any{"hint": "safe|default|auto|yolo"}},
		map[string]any{"name": "compact", "description": "Compact the session context"},
		map[string]any{"description": "没有名字的条目"},
	})
	check("parseCommands：跳过无名条目，hint/空 hint 各就位",
		len(cmds) == 2 &&
			cmds[0].Name == "permission" && cmds[0].Hint == "safe|default|auto|yolo" &&
			cmds[1].Name == "compact" && cmds[1].Hint == "")

	// ② 命令词识别
	{
		ok := true
		for _, c := range []struct {
			text string
			want string
			on   bool
		}{
			{"/pe", "pe", true},
			{"/", "", true},
			{"/model test/model", "", false},
			{"/permission ", "", false},
			{"hello", "", false},
			{"", "", false},
		} {
			got, on := cmdToken(c.text)
			if got != c.want || on != c.on {
				ok = false
				fmt.Printf("  !! cmdToken(%q) = (%q,%v)，期望 (%q,%v)\n", c.text, got, on, c.want, c.on)
			}
		}
		check("cmdToken：命令词 6 例（含空 / 带参数 / 非命令）", ok)
	}

	base := func(w int) model {
		m := model{width: w, cmds: engineCmds(), status: stIdle}
		return m
	}

	// ③ 过滤
	m := base(100)
	m.input.SetText("/")
	all := len(m.palMatches())
	m.input.SetText("/r")
	var rNames []string
	for _, i := range m.palMatches() {
		rNames = append(rNames, m.cmds[i].Name)
	}
	m.input.SetText("/ZZ")
	none := len(m.palMatches())
	check("过滤：/ = 全部 9；/r = reasoning·resume·redo；/ZZ（大写）= 0",
		all == 9 && strings.Join(rNames, ",") == "reasoning,resume,redo" && none == 0)

	// ④ 渲染：宽度 / 行数 / 颜色 / 背景色
	{
		m := base(100)
		m.input.SetText("/")
		rows := m.renderCmdPalette()
		okRows := len(rows) == 8 // 9 条 > 8：显示 7 条 + 「… 还有 2 条」
		okW, okBG, okMark := true, true, false
		for _, ln := range rows {
			if lipgloss.Width(ln) > m.blockWidth() {
				okW = false
			}
			if hasBackgroundColor(ln) {
				okBG = false
			}
			if strings.Contains(ln, "\u276F") && strings.Contains(ln, "38;2;0;231;164") {
				okMark = true
			}
		}
		fmt.Println("== 弹层样张（宽 100）==")
		for _, ln := range rows {
			fmt.Printf("  %s\n", stripANSI(ln))
		}
		check("渲染：9 条 → 7 行 + 溢出提示；宽度 ≤ 输入区宽；无背景色；选中 ❯ 强调绿",
			okRows && okW && okBG && okMark)

		m2 := base(40) // 窄屏：丢描述与参数提示
		m2.input.SetText("/")
		rows2 := m2.renderCmdPalette()
		okW2, okPlain := true, true
		for _, ln := range rows2 {
			if lipgloss.Width(ln) > m2.blockWidth() {
				okW2 = false
			}
			if strings.Contains(stripANSI(ln), "safe|default") {
				okPlain = false // 参数提示应整列丢掉
			}
		}
		fmt.Println("== 弹层样张（窄屏 40：只留命令名）==")
		for _, ln := range rows2 {
			fmt.Printf("  %s\n", stripANSI(ln))
		}
		check("窄屏降级：先丢描述、再丢参数提示；宽度仍合规", okW2 && okPlain)
	}

	// ⑤ 键位：↑↓ 夹取 / tab 补全 / enter 执行（无参数）/ enter 补全（带参数）/ esc 关闭
	m = base(100)
	m.input.SetText("/")
	if sel := m.palSelAt(len(m.palMatches())); sel != 0 {
		check("初始选中 = 第一条", false)
	}
	up, _ := m.palKey(press(tea.KeyUp, ""))
	check("↑ 在第一条：夹取不动", up && m.palSel == 0)

	m.palKey(press(tea.KeyDown, ""))
	check("↓ 移到第二条（model）", m.palSel == 1)
	m.palKey(press(tea.KeyUp, ""))
	check("↑ 回到第一条（permission）", m.palSel == 0)

	m.palKey(press(tea.KeyTab, ""))
	check("tab 补全带参数命令：/permission + 空格", m.input.Text() == "/permission ")
	check("补全后弹层收起（出现空格 = 已进入参数）", !m.palOpen())

	m2 := base(100)
	m2.input.SetText("/comp")
	m2.palKey(press(tea.KeyTab, ""))
	check("tab 补全无参数命令：/compact", m2.input.Text() == "/compact")

	m3 := base(100)
	m3.input.SetText("/comp")
	handled, submit := m3.palKey(press(tea.KeyEnter, ""))
	check("enter 无参数命令：handled + submit（直接执行）", handled && submit && m3.input.Text() == "/compact")

	m4 := base(100)
	m4.input.SetText("/resu")
	_, submit4 := m4.palKey(press(tea.KeyEnter, ""))
	check("enter 带参数命令：只补全不发送（参数留给用户）", !submit4 && m4.input.Text() == "/resume ")

	m5 := base(100)
	m5.input.SetText("/per")
	m5.palKey(press(tea.KeyEscape, ""))
	check("esc：只关弹层，输入文字保留", m5.palHidden && !m5.palOpen() && m5.input.Text() == "/per")

	// ⑥ 键路由：弹层开着时 ↑ 归弹层（不滚消息区）、tab 不进工具卡选择态、esc 不取消回合
	{
		mm := base(100)
		mm.feed = NewFeed()
		mm.feed.SetSize(94, 10)
		for i := 0; i < 30; i++ {
			mm.feed.Append(kAssistant, fmt.Sprintf("第 %d 行内容，用来撑出滚动空间。", i))
		}
		mm.input.SetText("/")
		scrollBefore := mm.feed.scroll
		next, _ := mm.handleKey(press(tea.KeyUp, ""))
		mm = next.(model)
		check("弹层开着时 ↑ 不动消息区滚动", mm.feed.scroll == scrollBefore)

		next, _ = mm.handleKey(press(tea.KeyTab, ""))
		mm = next.(model)
		check("弹层开着时 tab 不进工具卡选择态（补全命令）", mm.cardNav == nil && mm.input.Text() == "/permission ")

		mm2 := base(100)
		mm2.feed = NewFeed()
		mm2.input.SetText("/per")
		next, _ = mm2.handleKey(press(tea.KeyEscape, ""))
		mm2 = next.(model)
		check("弹层开着时 esc 不取消回合（只关弹层）", mm2.status == stIdle && mm2.palHidden)

		// 输入变化：esc 关闭后继续打字 → 弹层恢复；打空格 → 收起
		next, _ = mm2.handleKey(press('m', "m"))
		mm2 = next.(model)
		check("打字后弹层恢复（/perm）", mm2.palOpen() && mm2.input.Text() == "/perm")
		next, _ = mm2.handleKey(press(' ', " "))
		mm2 = next.(model)
		check("打空格后弹层收起（进入参数）", !mm2.palOpen())
	}

	// ⑦ 模态互斥：权限面板 / 工具卡选择态在场时弹层不显示
	{
		mm := base(100)
		mm.input.SetText("/")
		mm.perm = &PermRequest{Title: "shell__exec npm test"}
		ok1 := !mm.palOpen()
		mm.perm = nil
		mm.cardNav = &FeedItem{}
		ok2 := !mm.palOpen()
		check("权限面板 / 选择态在场时弹层不显示（模态优先）", ok1 && ok2)
	}

	// ⑧ View() 集成：弹层行数从消息区扣出，整屏行数不变
	{
		vm := model{
			width: 120, height: 30, feed: NewFeed(), status: stIdle, panelOn: false,
			cmds: engineCmds(), sessionID: "1234abcd-0000-0000-0000-000000000000",
			sessTitle: "命令弹层集成样张", modelLabel: "Step 3.7 Flash", modeID: "default",
		}
		vm.input.SetText("/")
		vm.syncLayout()
		content := vm.View().Content
		lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
		okRows := len(lines) == vm.height
		okCmd := strings.Contains(stripANSI(content), "/permission")
		okW := true
		for _, ln := range lines {
			if lipgloss.Width(ln) > vm.width {
				okW = false
				break
			}
		}
		if hasBackgroundColor(content) {
			okW = false
		}
		check("View() 集成：行数不变 / 弹层可见 / 宽度合规 / 背景干净", okRows && okCmd && okW)
	}

	// ⑨ 端到端：把实机 trace 的原始 update JSON 喂给 handleSessionUpdate，
	// 验证"引擎报文 → model → 弹层"整条链路（含 /model 过滤后只剩一条）
	{
		const raw = `{"sessionId":"x","update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"permission","description":"Set the session permission mode","input":{"hint":"safe|default|auto|yolo"}},{"name":"model","description":"Switch the model the session runs with","input":{"hint":"model id"}},{"name":"reasoning","description":"Set the reasoning effort of the model the session runs with","input":{"hint":"off|none|minimal|low|medium|high|xhigh"}},{"name":"compact","description":"Compact the session context"},{"name":"fast","description":"Toggle fast mode"},{"name":"new","description":"Start a new session"},{"name":"resume","description":"Resume a session by id","input":{"hint":"session id"}},{"name":"undo","description":"Undo the last turn"},{"name":"redo","description":"Redo the last undone turn"}]}}`
		var params map[string]any
		okJSON := json.Unmarshal([]byte(raw), &params) == nil
		mm := model{width: 100, feed: NewFeed()}
		mm.handleSessionUpdate(params)
		okCmds := len(mm.cmds) == 9 && mm.cmds[0].Name == "permission" && mm.cmds[2].Hint != ""
		mm.input.SetText("/mo")
		rows := mm.renderCmdPalette()
		okRow := len(rows) == 1 && strings.Contains(stripANSI(rows[0]), "model id") &&
			len(mm.palMatches()) == 1 && mm.cmds[mm.palMatches()[0]].Name == "model"
		check("端到端：实机 update JSON → handleSessionUpdate → 过滤 /mo 只剩 /model", okJSON && okCmds && okRow)
	}

	if failed {
		fmt.Println("cmdtest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("cmdtest: 全部通过")
}

// press 造一个按键消息（自检用）。
func press(code rune, text string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Text: text}
}
