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

// isNewOnly 输入是否正好是 /new——客户端要接管这个形态（改走 ACP 的 session/new
// 请求；为什么不能让它当 prompt 原样发给引擎，见 newSessionAsync 的注释）。
func isNewOnly(text string) bool {
	return strings.TrimSpace(text) == "/new"
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
	id    string
	title string // 列表行的标题：session/load 应答里没有它，只能从这儿带过去
	err   error
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
//
// 完成事件**必须**塞回 c.Events，不能直接当 tea.Msg 返回：读循环是先把整段
// 历史重放一条条推进 c.Events、然后才把应答交给等的 Call()——而 UI 是一个 tea
// 周期才消费一条事件。直接返回的话，完成事件会插到重放中间（2026-09-19 真机
// 事故：/resume 后只剩三条提问，回答全不见了），把 loading/busy 提前翻假，
// 后面的 agent chunk 就走 lastAssistant 合并路径。塞回同一条 FIFO 通道即与
// 重放严格保序。
func loadSessionAsync(c *ACPClient, id, title string) tea.Cmd {
	return func() tea.Msg {
		if c == nil {
			return sessLoadDoneMsg{id: id, title: title, err: fmt.Errorf("没有可用的引擎连接")}
		}
		_, err := c.LoadSession(id, workspace)
		c.Events <- ACPEvent{
			Method: methodLoadDone,
			Params: map[string]any{"id": id, "title": title},
			Err:    err,
		}
		return noopMsg{}
	}
}

// newSessionDoneMsg session/new 的应答。与 loadSessionAsync 不同：新建会话没有
// 历史重放要排队，应答可以当普通 tea.Msg 直接返回（不必塞事件通道）。
type newSessionDoneMsg struct {
	sid  string
	sess map[string]any
	err  error
}

// newSessionAsync 让引擎开一条新会话（ACP 的 session/new）。
//
// 为什么不把 "/new" 当 prompt 原样发给引擎：引擎侧那条路只换它自己的当前会话、
// 不发任何通知（driver.rs 的 adopt_session：没有 pending responder 时只写一行
// debug 日志）；客户端还拿着旧 id 继续提问时，start_prompt 的 pending_resume
// 分支又会把旧会话整个"恢复"回来——/new 等于被下一次提问抵消。走 session/new
// 请求才是正路：引擎装上新会话、应答把新 id 带回来（start_session 的
// session_issued 分支），旧会话随时可以用 /resume 找回。
func newSessionAsync(c *ACPClient) tea.Cmd {
	return func() tea.Msg {
		if c == nil {
			return newSessionDoneMsg{err: fmt.Errorf("没有可用的引擎连接")}
		}
		sid, sess, err := c.NewSession(workspace)
		return newSessionDoneMsg{sid: sid, sess: sess, err: err}
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
		if hasStrayBackground(ln) {
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

// ---------------------------------------------------------------------------
// -loadtest：会话载入（session/load 历史重放）自检
// ---------------------------------------------------------------------------

// runLoadTest 覆盖 2026-09-19 真机事故的整条链路：/resume 之后只剩三条提问、
// 回答全不见了。根因是"载入完成"事件被当成普通 tea.Msg 从 goroutine 返回，插到
// 历史重放中间——loading/busy 被提前翻假，后面的 agent_message_chunk 全走
// lastAssistant 合并路径，回答并进第一条助手消息（位置很高，翻页才看得见）。
//
// 修法两件事，这里各有关键断言：
//
//	① 完成事件改走事件通道（loadSessionAsync 注入 c.Events），与重放同一条 FIFO；
//	② 合并路径加 loading 护栏（载入重放期间绝不合并）。
func runLoadTest() {
	failed := false
	check := func(name string, cond bool) {
		tag := "PASS"
		if !cond {
			tag = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", tag, name)
	}

	// newLoading 复现"会话列表按下 enter"之后的状态：新 feed + 载入中 + 禁发。
	newLoading := func() *model {
		m := &model{width: 100, feed: NewFeed(), status: stLoading, loading: true, busy: true}
		m.feed.SetSize(94, 20)
		m.feed.Append(kSys, "载入会话 1789816533057-27968-0（引擎将重放历史）")
		return m
	}
	// replay 模拟引擎重放来的一条 session/update。
	replay := func(m *model, kind, text string) {
		m.handleSessionUpdate(map[string]any{
			"update": map[string]any{
				"sessionUpdate": kind,
				"content":       map[string]any{"text": text},
			},
		})
	}
	kinds := func(m *model) []int {
		out := make([]int, 0, m.feed.Len())
		for _, it := range m.feed.items {
			out = append(out, it.Kind)
		}
		return out
	}
	countKind := func(m *model, k int) int {
		n := 0
		for _, it := range m.feed.items {
			if it.Kind == k {
				n++
			}
		}
		return n
	}
	// drain 按 UI 的真实消费顺序处理事件通道：遇到 methodLoadDone 走 finishLoad，
	// 其余走 handleEvent（也就是 Update 里 acpEventMsg 分支做的事）。
	drain := func(m *model, c *ACPClient) *model {
		for {
			select {
			case ev := <-c.Events:
				if ev.Method == methodLoadDone {
					id, _ := ev.Params["id"].(string)
					title, _ := ev.Params["title"].(string)
					nm, _ := m.finishLoad(id, title, ev.Err)
					mv := nm.(model)
					m = &mv
					continue
				}
				m.handleEvent(ev)
			default:
				return m
			}
		}
	}

	// ① 重放保序 + 完成落最后：三条提问、三条回答，各自成条、顺序不变
	c := &ACPClient{Events: make(chan ACPEvent, 16), Closed: make(chan struct{})}
	for _, ev := range []ACPEvent{
		{Method: "session/update", Params: map[string]any{"update": map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"text": "问一"}}}},
		{Method: "session/update", Params: map[string]any{"update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"text": "答一"}}}},
		{Method: "session/update", Params: map[string]any{"update": map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"text": "问二"}}}},
		{Method: "session/update", Params: map[string]any{"update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"text": "答二"}}}},
		{Method: "session/update", Params: map[string]any{"update": map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"text": "问三"}}}},
		{Method: "session/update", Params: map[string]any{"update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"text": "答三"}}}},
		{Method: methodLoadDone, Params: map[string]any{"id": "1789816533057-27968-0"}},
	} {
		c.Events <- ev
	}
	m := drain(newLoading(), c)
	check("重放保序：user/agent 交替各自成条（3 问 3 答，不是 3 问 1 答）",
		countKind(m, kUser) == 3 && countKind(m, kAssistant) == 3)
	check("条目顺序：sys → 问 → 答 → 问 → 答 → 问 → 答 → sys",
		fmt.Sprint(kinds(m)) == fmt.Sprint([]int{kSys, kUser, kAssistant, kUser, kAssistant, kUser, kAssistant, kSys}))
	last := m.feed.items[m.feed.Len()-1]
	check("完成事件落在最后：末条是「已载入会话」回执",
		last.Kind == kSys && strings.Contains(last.Text, "已载入会话 1789816533057-27968-0"))
	check("收尾状态：loading/busy 释放、status=done、会话 id 落账",
		!m.loading && !m.busy && m.status == stDone && m.sessionID == "1789816533057-27968-0")

	// ①b 会话标题透传（2026-09-19 用户截图：/resume 回来右栏显示「未命名」）。
	// session/load 应答里没有标题，引擎的 session_info_update 也只在会话被命名/
	// 改名时才发（letcode projection.rs 的 SessionTitleUpdated 分支）——所以标题
	// 只能由 loadSessionAsync 从列表那一行塞进合成事件，finishLoad 再落账。
	c2 := &ACPClient{Events: make(chan ACPEvent, 4), Closed: make(chan struct{})}
	c2.Events <- ACPEvent{Method: methodLoadDone, Params: map[string]any{
		"id": "1789672451669-37748-0", "title": "修复 ACP 权限回执"}}
	t2 := drain(newLoading(), c2)
	check("标题透传：finishLoad 把列表行标题写进右栏（不再显示「未命名」）",
		t2.sessTitle == "修复 ACP 权限回执")

	// ①c 空标题不覆盖：列表行没标题时保持原样（不写成空串把已有标题擦掉）
	c3 := &ACPClient{Events: make(chan ACPEvent, 4), Closed: make(chan struct{})}
	c3.Events <- ACPEvent{Method: methodLoadDone, Params: map[string]any{"id": "x"}}
	t3 := newLoading()
	t3.sessTitle = "旧标题"
	t3 = drain(t3, c3)
	check("空标题不覆盖：列表行无标题时保留原值", t3.sessTitle == "旧标题")

	// ② 护栏：loading 期间即便 busy 已假、lastAssistant 非空，也不合并
	g := newLoading()
	replay(g, "user_message_chunk", "问一")
	replay(g, "agent_message_chunk", "答一")
	g.busy = false // 模拟"完成事件提前插进来"把 busy 翻假的最坏情况
	g.lastAssistant = g.feed.items[2]
	replay(g, "user_message_chunk", "问二")
	replay(g, "agent_message_chunk", "答二")
	check("护栏：loading 中 agent chunk 不并入上一段（仍各自成条）",
		countKind(g, kAssistant) == 2 && g.feed.items[3].Kind == kUser &&
			g.feed.items[4].Kind == kAssistant && g.feed.items[4].Text == "答二")

	// ③ 状态栏/光标：载入中不抢"回复中"、不挂光标
	s := newLoading()
	replay(s, "agent_message_chunk", "答一")
	check("载入中：状态栏停在 loading、不挂流式光标",
		s.status == stLoading && s.active == nil)

	// ④ 思考块同理（先结束上一个思考周期，让 lastThought 就位）
	t := newLoading()
	replay(t, "agent_thought_chunk", "想一")
	t.endThoughtCycle() // 周期结束：lastThought = 第一块、curThought = nil
	t.busy = false      // 模拟"完成事件提前插进来"把 busy 翻假的最坏情况
	replay(t, "agent_thought_chunk", "想二")
	check("护栏：loading 中思考 chunk 也不合并（两块思考）",
		countKind(t, kThought) == 2 && t.feed.items[2].Text == "想二")

	// ⑤ 失败路径：finishLoad 带 err → 错误行 + status=error，且不落"已载入"
	e := newLoading()
	ev, _ := e.finishLoad("x", "", fmt.Errorf("引擎内部错误：session transcript is already open for writing"))
	em := ev.(model)
	check("失败路径：落错误行、status=error、不谎报已载入",
		em.status == stError && countKind(&em, kError) == 1 &&
			strings.Contains(em.feed.items[em.feed.Len()-1].Text, "载入会话失败"))

	// ⑥ 用户消息在重放里照收（loading 期间不吃 localEcho 去重）
	d := newLoading()
	d.localEcho = "问一" // 刻意留一条会撞上的本地回显
	replay(d, "user_message_chunk", "问一")
	check("重放收录：loading 期间用户消息照收（本地回显去重让路）",
		countKind(d, kUser) == 1 && d.feed.items[1].Text == "问一")

	fmt.Println("== 载入重放后的消息区样张（宽 94）==")
	for _, it := range m.feed.items {
		for _, ln := range renderItem(it, 94) {
			fmt.Printf("  %s\n", stripANSI(ln))
		}
	}

	if failed {
		fmt.Println("loadtest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("loadtest: 全部通过")
}
