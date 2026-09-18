// ui_sess.go —— 会话列 / 载（M4d · §7 /resume）
//
// 数据来源（字段名对过 ACP schema 1.7.0 与 letcode driver.rs）：
//
//	session/list  → {"sessions":[{"sessionId":"…","cwd":"…","title":"…"?,"updatedAt":"ISO8601"?}]}
//	                （letcode 单页返回，不发行游标；列表本就按前端工作区过滤）
//	session/load  → 请求 {"sessionId":"…","cwd":"<绝对路径>","mcpServers":[]}
//	                引擎先把历史**重放**成 session/update 通知（只有文本：
//	                user_message_chunk / agent_message_chunk；工具卡不进重放），
//	                再回应答 {"modes":…,"configOptions":…}，之后重播 available_commands。
//
// 触发：输入 /resume（不带参数）时由客户端接管——引擎只认 "/resume <session_id>"，
// 所以这里弹列表让用户挑，挑完用 session/load 载入并重放。
//
// 透明度原则：只前景色；选中行 ❯ 强调绿；标题浅色、次要信息暗灰。
package main

import (
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// sessPickerRows 列表最多显示的数据行数（超出的截断并提示）。
const sessPickerRows = 6

// SessionRow 一条可载入的会话（session/list 的 SessionInfo 精简版）。
type SessionRow struct {
	ID      string // sessionId（载入时原样回传）
	Title   string // 标题（可能为空）
	Cwd     string // 工作目录（展示用）
	Updated string // ISO 8601（展示时截成 MM-DD HH:MM）
}

// parseSessions 解析 session/list 的 sessions 数组（缺失字段容错跳过）。
func parseSessions(items []any) []SessionRow {
	out := make([]SessionRow, 0, len(items))
	for _, it := range items {
		im := asMap(it)
		if im == nil {
			continue
		}
		id, _ := im["sessionId"].(string)
		if id == "" {
			continue
		}
		title, _ := im["title"].(string)
		cwd, _ := im["cwd"].(string)
		updated, _ := im["updatedAt"].(string)
		out = append(out, SessionRow{ID: id, Title: title, Cwd: cwd, Updated: updated})
	}
	return out
}

// isResumeOnly 输入是否正好是 /resume（不带参数）——客户端要接管这个形态。
func isResumeOnly(text string) bool {
	return strings.TrimSpace(text) == "/resume"
}

// sessStamp 把 ISO 8601 时间截成 "MM-DD HH:MM"（不是这个形状就原样返回，空了返回空）。
func sessStamp(iso string) string {
	if len(iso) >= 16 && iso[4] == '-' && iso[10] == 'T' {
		return iso[5:10] + " " + iso[11:16]
	}
	return iso
}

// ---------------------------------------------------------------------------
// 异步动作（列表 / 载入都不阻塞 UI）
// ---------------------------------------------------------------------------

// sessListMsg session/list 的结果（err 非空表示读取失败）。
type sessListMsg struct {
	rows []SessionRow
	err  error
}

// sessLoadDoneMsg session/load 的应答（历史重放已在此之前到达）。
type sessLoadDoneMsg struct {
	id  string
	err error
}

// fetchSessions 去引擎拉会话列表。
func fetchSessions(c *ACPClient) tea.Cmd {
	return func() tea.Msg {
		if c == nil {
			return sessListMsg{err: fmt.Errorf("没有可用的引擎连接")}
		}
		raw, err := c.ListSessions()
		if err != nil {
			return sessListMsg{err: err}
		}
		return sessListMsg{rows: parseSessions(raw)}
	}
}

// loadSessionAsync 载入一条会话（引擎先重放历史，再回应答）。
func loadSessionAsync(c *ACPClient, id string) tea.Cmd {
	return func() tea.Msg {
		if c == nil {
			return sessLoadDoneMsg{id: id, err: fmt.Errorf("没有可用的引擎连接")}
		}
		_, err := c.LoadSession(id, workspace)
		return sessLoadDoneMsg{id: id, err: err}
	}
}

// ---------------------------------------------------------------------------
// 渲染
// ---------------------------------------------------------------------------

// renderSessionPicker 渲染会话选择列表（钉在输入区上方；高度在 syncLayout 里扣出）：
//
//	载入会话 · 12 条
//	❯ 修复 ACP 权限回执        09-18 17:31 · 1789672451669-37748-0
//	  早先的会话（无标题）      09-17 22:04 · 1789596200000-12345-0
//
// 选中行 ❯ 强调绿；标题浅色（空标题用 id 兜底）、时间与 id 暗灰（窄屏先丢 id 再丢时间）。
func (m model) renderSessionPicker() []string {
	if !m.sessOn {
		return nil
	}
	budget := m.blockWidth()
	out := make([]string, 0, sessPickerRows+1)

	head := "载入会话"
	if !m.sessReady {
		head += " · 正在读取…"
	} else {
		head += fmt.Sprintf(" · %d 条", len(m.sessRows))
	}
	out = append(out, "  "+dimStyle.Render(head))

	if m.sessReady && len(m.sessRows) == 0 {
		out = append(out, "  "+dimStyle.Render("没有可载入的会话（esc 关闭）"))
		return out
	}
	if !m.sessReady {
		return out
	}

	shown := m.sessRows
	more := 0
	if len(shown) > sessPickerRows {
		more = len(shown) - sessPickerRows
		shown = shown[:sessPickerRows]
	}
	sel := m.sessSel
	if sel < 0 {
		sel = 0
	}
	if sel >= len(shown) {
		sel = len(shown) - 1
	}

	for i, row := range shown {
		mark := " "
		if i == sel {
			mark = userBarStyle.Render("\u276F")
		}
		title := row.Title
		if strings.TrimSpace(title) == "" {
			title = row.ID
		}
		// 右侧次要信息：更新时间 + id（宽度不够时先丢 id、再丢时间）
		tail := ""
		ts := sessStamp(row.Updated)
		if ts != "" {
			tail = ts
		}
		withID := tail + " \u00B7 " + row.ID
		if lipgloss.Width(tail) > 0 && lipgloss.Width(withID)+lipgloss.Width(title)+6 <= budget {
			tail = withID
		}
		titleW := budget - 4 - lipgloss.Width(tail) - 2
		if tail == "" {
			titleW = budget - 4
		}
		if titleW < 8 {
			titleW = 8
		}
		line := "  " + mark + " " + textStyle.Render(clipWidth(title, titleW))
		if tail != "" {
			pad := titleW - lipgloss.Width(clipWidth(title, titleW))
			if pad > 0 {
				line += strings.Repeat(" ", pad)
			}
			line += "  " + dimStyle.Render(tail)
		}
		out = append(out, line)
	}
	if more > 0 {
		out = append(out, "  "+dimStyle.Render(fmt.Sprintf("\u2026 还有 %d 条", more)))
	}
	return out
}

// ---------------------------------------------------------------------------
// -sesstest：会话列表自检（无 TTY / 无引擎）
// ---------------------------------------------------------------------------

// runSessTest 覆盖：解析（含缺字段容错）/ 触发判定 / 渲染宽度与选中色 /
// 键位（↑↓ 夹取、esc 关闭）/ submit 拦截（/resume → 打开列表，不发引擎）。
func runSessTest() {
	failed := false
	check := func(name string, cond bool) {
		tag := "PASS"
		if !cond {
			tag = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", tag, name)
	}

	// ① 解析：缺 title / 缺 updatedAt 都不炸；无名条目跳过
	rows := parseSessions([]any{
		map[string]any{"sessionId": "1789672451669-37748-0", "cwd": "F:\\lab\\demo-lab", "title": "修复 ACP 权限回执", "updatedAt": "2026-09-18T09:31:12Z"},
		map[string]any{"sessionId": "1789596200000-12345-0", "cwd": "F:\\lab\\demo-lab"},
		map[string]any{"cwd": "F:\\lab\\demo-lab"},
	})
	check("parseSessions：2 条（缺字段容错、无名跳过）",
		len(rows) == 2 && rows[0].ID == "1789672451669-37748-0" &&
			rows[0].Title == "修复 ACP 权限回执" && rows[0].Updated == "2026-09-18T09:31:12Z" &&
			rows[1].Title == "" && rows[1].Updated == "")

	// ② 时间截断与触发判定
	check("sessStamp：ISO → MM-DD HH:MM",
		sessStamp("2026-09-18T09:31:12Z") == "09-18 09:31" && sessStamp("") == "" && sessStamp("junk") == "junk")
	check("isResumeOnly：/resume 与 '/resume ' 命中；带参数/别的命令不命中",
		isResumeOnly("/resume") && isResumeOnly("  /resume  ") &&
			!isResumeOnly("/resume 1789672451669-37748-0") && !isResumeOnly("/compact") && !isResumeOnly(""))

	// ③ 渲染：宽度合规 / 选中 ❯ 强调绿 / 无背景色 / 空列表提示
	m := model{width: 100, sessOn: true, sessReady: true, sessRows: rows, sessSel: 1}
	lines := m.renderSessionPicker()
	okW, okBG, okMark := true, true, false
	for _, ln := range lines {
		if lipgloss.Width(ln) > m.blockWidth() {
			okW = false
		}
		if hasBackgroundColor(ln) {
			okBG = false
		}
		if strings.Contains(ln, "\u276F") && strings.Contains(ln, "38;2;78;224;94") {
			okMark = true
		}
	}
	fmt.Println("== 会话列表样张（宽 100）==")
	for _, ln := range lines {
		fmt.Printf("  %s\n", stripANSI(ln))
	}
	check("渲染：宽度合规 / 无背景色 / 选中 ❯ 强调绿", okW && okBG && okMark)

	m0 := model{width: 100, sessOn: true, sessReady: true}
	empty := m0.renderSessionPicker()
	okEmpty := len(empty) == 2 && strings.Contains(stripANSI(empty[1]), "没有可载入的会话")
	m1 := model{width: 60, sessOn: true, sessReady: true, sessRows: rows, sessSel: 0}
	okNarrow := true
	for _, ln := range m1.renderSessionPicker() {
		if lipgloss.Width(ln) > m1.blockWidth() {
			okNarrow = false
		}
	}
	fmt.Println("== 窄屏样张（宽 60）==")
	for _, ln := range m1.renderSessionPicker() {
		fmt.Printf("  %s\n", stripANSI(ln))
	}
	check("渲染：空列表给提示；窄屏宽度仍合规", okEmpty && okNarrow)

	// ④ 键位：↑↓ 夹取、esc 关闭（都走 handleKey 的真实路由）
	key := func(m model, code rune, text string) model {
		next, _ := m.handleKey(press(code, text))
		return next.(model)
	}
	k := model{width: 100, feed: NewFeed(), status: stIdle, sessOn: true, sessReady: true, sessRows: rows, sessSel: 0}
	k = key(k, tea.KeyDown, "")
	check("↓ 移到第二条", k.sessSel == 1)
	k = key(k, tea.KeyDown, "")
	check("最后一条再 ↓：夹取不动", k.sessSel == 1)
	k = key(k, tea.KeyUp, "")
	k = key(k, tea.KeyUp, "")
	check("第一条再 ↑：夹取不动", k.sessSel == 0)
	k = key(k, tea.KeyEscape, "")
	check("esc 关闭列表（不取消回合、不进输入框）", !k.sessOn && k.status == stIdle && k.input.Text() == "")

	// ⑤ submit 拦截：/resume → 打开列表（不把 "/resume" 当提问发出去）
	s := model{width: 100, feed: NewFeed(), status: stIdle}
	s.input.SetText("/resume")
	next, cmd := s.submit()
	s = next.(model)
	check("submit('/resume')：打开会话列表、输入清空、返回拉取命令（cmd 非 nil）",
		s.sessOn && !s.sessReady && s.input.Text() == "" && cmd != nil && len(s.sent) == 1)
	check("列表开着时不把 /resume 写进消息区", s.feed.Len() == 0)

	if failed {
		fmt.Println("sesstest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("sesstest: 全部通过")
}
