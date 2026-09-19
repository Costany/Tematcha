// ui_elicit.go —— 追问面板（M5a · elicitation/create）
//
// 协议形状（ACP schema 1.7.0 的 v1/elicitation.rs + letcode src/acp/driver.rs 取证）：
//
//	引擎 → 客户端（反向请求，method = "elicitation/create"）：
//	{
//	  "sessionId": "…", "mode": "form", "message": "问题一\n问题二",
//	  "requestedSchema": {
//	    "type": "object",
//	    "properties": {
//	      "question_1": {"type":"string","title":"<header>","description":"<问题>",
//	                     "oneOf":[{"const":"<label>","title":"<label>","description":"<选项说明>"}]},
//	      "question_2": {"type":"array","minItems":1,…,"items":{…oneOf…}}   ← 多选
//	    },
//	    "required": ["question_1","question_2"]}}
//
//	客户端 → 引擎（应答）：
//	  {"action":"accept","content":{"question_1":"<label>","question_2":["<label>","…"]}}
//	  {"action":"decline"}   ← 用户不想答（面板里的 esc）
//	  {"action":"cancel"}    ← 客户端取消（回合被取消等）
//
// 前提：客户端要在 initialize 里广告 form elicitation
// （clientCapabilities.elicitation.form = {}）；否则引擎把 question 工具判成
// \"the ACP client does not support form elicitation\" 并自动婉拒（QUESTIONS_UNSUPPORTED）。
//
// 透明度原则：只前景色，面板左缘一条强调绿竖条当"面板边"（权限面板用琥珀，颜色区分语义）。
package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// ElicitOption 一个选项：Value = oneOf[].const（应答时回传），Title/Desc 给人看。
type ElicitOption struct {
	Value string
	Title string
	Desc  string
}

// ElicitQuestion 一道追问 + 它的作答状态。
type ElicitQuestion struct {
	Key      string // 属性名（question_1 …）：应答 content 的键
	Title    string // header（引擎给的题面短标题）
	Desc     string // 问题正文
	Multiple bool   // 多选（schema type = array）
	Options  []ElicitOption

	cursor   int          // 光标所在选项
	chosen   int          // 单选：已选项下标（-1 = 未答）
	selected map[int]bool // 多选：已勾选集合
	Text     string       // 自己写的答案（按 0 进入输入；引擎只验非空，任意文本都接受）
	TextMode bool         // 正在输入自定义答案
}

// answered 这道题是否已作答（选了选项或自己写了答案都算）。
func (q *ElicitQuestion) answered() bool {
	if strings.TrimSpace(q.Text) != "" {
		return true
	}
	if len(q.Options) == 0 {
		return false // 纯自由输入题：必须自己写
	}
	if q.Multiple {
		for _, v := range q.selected {
			if v {
				return true
			}
		}
		return false
	}
	return q.chosen >= 0
}

// choiceAt 选项 i 是否处于"已选"状态（多选看集合，单选看 chosen）。
func (q *ElicitQuestion) choiceAt(i int) bool {
	if q.Multiple {
		return q.selected[i]
	}
	return q.chosen == i
}

// toggleAt 选择 / 勾选第 i 个选项（单选会顺带把光标移过去）。
func (q *ElicitQuestion) toggleAt(i int) {
	if i < 0 || i >= len(q.Options) {
		return
	}
	q.cursor = i
	if q.Multiple {
		q.selected[i] = !q.selected[i]
	} else {
		q.chosen = i
		q.Text, q.TextMode = "", false // 单选选定后放弃自定义文本，避免双答案歧义
	}
}

// answerText 已回答的内容（回执与题头状态用）；未答 = ""。
func (q *ElicitQuestion) answerText() string {
	var parts []string
	for i, o := range q.Options {
		if q.choiceAt(i) {
			parts = append(parts, o.Title)
		}
	}
	if t := strings.TrimSpace(q.Text); t != "" {
		parts = append(parts, t)
	}
	return strings.Join(parts, "、")
}

// ElicitRequest 一条待回答的追问表单。
type ElicitRequest struct {
	RPCID       any // 反向请求的原始 JSON-RPC id（原样回传，类型不能变）
	SessionID   string
	Message     string // 表单上方的一句话（引擎把各题拼成多行）
	Questions   []*ElicitQuestion
	Cur         int  // 当前题目下标
	Unsupported bool // mode 不是 form（如 url）：直接回 cancel，不渲染面板

	Placeholder *FeedItem // 消息区的"\u2192 等待回答…"占位（答完换成回执）
}

// parseElicitRequest 从 elicitation/create 的 params 解析表单。
// 题目顺序优先按 schema.required（引擎按提问顺序写），缺失时按 question_N 的数字序。
func parseElicitRequest(rpcID any, params map[string]any) *ElicitRequest {
	e := &ElicitRequest{RPCID: rpcID}
	e.SessionID, _ = params["sessionId"].(string)
	e.Message, _ = params["message"].(string)
	if mode, _ := params["mode"].(string); mode != "form" {
		e.Unsupported = true
		return e
	}
	schema := asMap(params["requestedSchema"])
	if schema == nil {
		e.Unsupported = true
		return e
	}
	props := asMap(schema["properties"])
	if props == nil {
		e.Unsupported = true
		return e
	}

	// 顺序：required 数组优先（顺序即提问顺序），否则按 question_N 的数字序
	var keys []string
	for _, r := range asList(schema["required"]) {
		if k, ok := r.(string); ok {
			if _, exists := props[k]; exists {
				keys = append(keys, k)
			}
		}
	}
	if len(keys) == 0 {
		for k := range props {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return questionIndex(keys[i]) < questionIndex(keys[j]) })
	}

	for _, key := range keys {
		pm := asMap(props[key])
		if pm == nil {
			continue
		}
		q := &ElicitQuestion{Key: key, chosen: -1, selected: map[int]bool{}}
		q.Title, _ = pm["title"].(string)
		q.Desc, _ = pm["description"].(string)
		typ, _ := pm["type"].(string)
		q.Multiple = typ == "array"
		optSrc := asList(pm["oneOf"])
		if q.Multiple {
			if items := asMap(pm["items"]); items != nil {
				if o := asList(items["oneOf"]); len(o) > 0 {
					optSrc = o
				}
				if typ, _ := items["type"].(string); typ == "array" {
					q.Multiple = true
				}
			}
		}
		for _, o := range optSrc {
			om := asMap(o)
			if om == nil {
				continue
			}
			val, _ := om["const"].(string)
			title, _ := om["title"].(string)
			if title == "" {
				title = val
			}
			desc, _ := om["description"].(string)
			q.Options = append(q.Options, ElicitOption{Value: val, Title: title, Desc: desc})
		}
		if len(q.Options) == 0 {
			// 无 oneOf 时兜底：schema 的 enum（无题选项，同样按字符串取）
			for _, v := range asList(pm["enum"]) {
				if s, ok := v.(string); ok {
					q.Options = append(q.Options, ElicitOption{Value: s, Title: s})
				}
			}
		}
		if q.Title == "" {
			q.Title = q.Desc
		}
		if q.Title == "" {
			q.Title = key
		}
		e.Questions = append(e.Questions, q)
	}
	if len(e.Questions) == 0 {
		e.Unsupported = true
	}
	return e
}

// questionIndex 从 "question_3" 里取数字（取不到给一个很大值，排在最后）。
func questionIndex(key string) int {
	i := strings.LastIndexByte(key, '_')
	if i < 0 || i+1 >= len(key) {
		return 1 << 30
	}
	n, err := strconv.Atoi(key[i+1:])
	if err != nil {
		return 1 << 30
	}
	return n
}

// response 组装应答体：decline=true 时回 decline；否则回 accept + 已答内容。
// 未作答的题不放进 content（引擎侧 missing → 空数组，不会替用户编答案）。
func (e *ElicitRequest) response(decline bool) map[string]any {
	if decline {
		return map[string]any{"action": "decline"}
	}
	content := map[string]any{}
	for _, q := range e.Questions {
		if q.Multiple {
			var labels []string
			for i, o := range q.Options {
				if q.selected[i] {
					labels = append(labels, o.Value)
				}
			}
			if t := strings.TrimSpace(q.Text); t != "" {
				labels = append(labels, t)
			}
			if len(labels) > 0 {
				content[q.Key] = labels
			}
			continue
		}
		if q.chosen >= 0 && q.chosen < len(q.Options) {
			content[q.Key] = q.Options[q.chosen].Value
		} else if t := strings.TrimSpace(q.Text); t != "" {
			content[q.Key] = t
		}
	}
	return map[string]any{"action": "accept", "content": content}
}

// ---------------------------------------------------------------------------
// model 侧：队列与应答
// ---------------------------------------------------------------------------

// pushElicit 收下一条追问：进队列；面板空着就把队首点亮。
// 追问面板接管键盘：先退掉工具卡选择态，避免两套键位打架。
func (m *model) pushElicit(e *ElicitRequest) {
	m.exitCardNav()
	if e.Unsupported {
		// 不认识的 mode（如 url）：直接回 cancel，别让引擎干等
		_ = m.client.Respond(e.RPCID, map[string]any{"action": "cancel"})
		m.feed.Append(kSys, "已回绝一条不支持的追问（mode 不是 form）")
		return
	}
	if e.Placeholder == nil {
		head := firstLine(e.Message)
		e.Placeholder = m.feed.Append(kSys, "\u2192 等待回答… "+clipWidth(head, 60))
	}
	m.feed.ScrollToBottom()
	m.elicitQueue = append(m.elicitQueue, e)
	if m.elicit == nil {
		m.popElicit()
	}
	m.syncLayout()
}

// popElicit 队列前移一位（队列空则收起面板）。
func (m *model) popElicit() {
	if len(m.elicitQueue) == 0 {
		m.elicit = nil
		return
	}
	m.elicit = m.elicitQueue[0]
	m.elicitQueue = m.elicitQueue[1:]
}

// settleElicit 结一条已作答的追问：占位换成绿条回执，然后换下一条上桌。
func (m *model) settleElicit(e *ElicitRequest, verb, detail string) {
	if e.Placeholder != nil {
		e.Placeholder.Kind = kReceipt
		m.feed.SetText(e.Placeholder, verb+" \u00B7 "+detail)
	}
	m.popElicit()
	m.syncLayout()
}

// dropElicits 丢弃全部待回答（不回应答）：回合收尾 / 引擎断开等"请求已作废"的场景。
func (m *model) dropElicits(reason string) {
	if m.elicit == nil && len(m.elicitQueue) == 0 {
		return
	}
	if e := m.elicit; e != nil && e.Placeholder != nil {
		m.feed.SetText(e.Placeholder, "追问失效（"+reason+"）")
	}
	for _, e := range m.elicitQueue {
		if e.Placeholder != nil {
			m.feed.SetText(e.Placeholder, "追问失效（"+reason+"）")
		}
	}
	m.elicit, m.elicitQueue = nil, nil
	m.syncLayout()
}

// answerSummary 已答内容的回执摘要（"题1=值1 · 题2=值2"，超长截断）。
func (e *ElicitRequest) answerSummary() string {
	var parts []string
	for i, q := range e.Questions {
		parts = append(parts, fmt.Sprintf("题%d=%s", i+1, q.answerText()))
	}
	return clipWidth(strings.Join(parts, " \u00B7 "), 60)
}

// elicitSubmit 提交表单（enter）：已答内容按 schema 组装成 accept 应答。
func (m *model) elicitSubmit() {
	e := m.elicit
	if e == nil {
		return
	}
	if m.client != nil { // 自检里 client 为 nil：只验应答组装，不发引擎
		_ = m.client.Respond(e.RPCID, e.response(false))
	}
	m.settleElicit(e, "已回答", fmt.Sprintf("%d 题 \u00B7 %s", len(e.Questions), e.answerSummary()))
}

// elicitDecline 拒绝回答（esc）：回 decline —— 引擎会把它记成"用户拒绝回答"。
func (m *model) elicitDecline() {
	e := m.elicit
	if e == nil {
		return
	}
	if m.client != nil {
		_ = m.client.Respond(e.RPCID, e.response(true))
	}
	m.settleElicit(e, "已拒绝回答", clipWidth(firstLine(e.Message), 48))
}

// elicitKey 追问面板的键位；返回是否已被吃掉。
//
//	↑↓       选项间移动（当前题）
//	←→/tab   换题（shift+tab 反向）
//	space    勾选 / 选中当前选项（多选=切换，单选=选中）
//	1..9     直接选第 N 个选项（单选选中后自动跳到下一道未答题）
//	0        自己写：进入自定义答案输入（enter 确认；再按 esc 退出输入态）
//	enter    全部答完 → 提交；否则跳到下一道未答题
//	esc      拒绝回答（decline；输入态下先退出输入态）
func (m *model) elicitKey(k tea.KeyPressMsg) bool {
	e := m.elicit
	if e == nil {
		return false
	}
	if len(e.Questions) == 0 {
		return false
	}
	if e.Cur < 0 || e.Cur >= len(e.Questions) {
		e.Cur = 0
	}
	q := e.Questions[e.Cur]
	key := k.String()

	// 自己写模式：可打印字符入缓冲、backspace 删除；enter 确认后跳题或提交。
	// ↑↓/←→/tab 先退出输入态再走常规导航；esc 先退出输入态（再按才是拒绝）。
	if q.TextMode {
		switch key {
		case "enter":
			q.TextMode = false
			if e.allAnswered() {
				m.elicitSubmit()
				return true
			}
			m.jumpToUnanswered()
			return true
		case "backspace":
			if r := []rune(q.Text); len(r) > 0 {
				q.Text = string(r[:len(r)-1])
			}
			return true
		case "esc":
			q.TextMode = false
			return true
		case "up", "down", "left", "right", "tab", "shift+tab":
			q.TextMode = false
		default:
			if k.Text != "" {
				q.Text = clipWidth(q.Text+k.Text, 120)
			}
			return true
		}
	}

	switch key {
	case "esc":
		m.elicitDecline()
		return true
	case "up":
		if q.cursor > 0 {
			q.cursor--
		}
		return true
	case "down":
		if q.cursor < len(q.Options)-1 {
			q.cursor++
		}
		return true
	case "left", "shift+tab":
		if e.Cur > 0 {
			e.Cur--
		}
		return true
	case "right", "tab":
		if e.Cur < len(e.Questions)-1 {
			e.Cur++
		}
		return true
	case "enter":
		if e.allAnswered() {
			m.elicitSubmit()
			return true
		}
		m.jumpToUnanswered()
		return true
	}
	if key == " " || key == "space" || k.Code == ' ' || k.Text == " " {
		q.toggleAt(q.cursor)
		if !q.Multiple {
			m.jumpToUnanswered()
		}
		return true
	}
	if key == "0" { // 自己写：进入自定义答案输入（单选会先清掉已选项）
		q.TextMode = true
		if !q.Multiple {
			q.chosen = -1
		}
		return true
	}
	if n, ok := digitKey(key); ok {
		q.toggleAt(n)
		if !q.Multiple {
			m.jumpToUnanswered()
		}
		return true
	}
	return false
}

// digitKey 把 "1".."9" 转成选项下标（0 起）；不是数字键返回 false。
func digitKey(key string) (int, bool) {
	if len(key) != 1 || key[0] < '1' || key[0] > '9' {
		return 0, false
	}
	return int(key[0] - '1'), true
}

// allAnswered 是否每道题都已作答。
func (e *ElicitRequest) allAnswered() bool {
	for _, q := range e.Questions {
		if !q.answered() {
			return false
		}
	}
	return true
}

// jumpToUnanswered 把光标移到下一道未答的题（都答完则停在原地）。
func (m *model) jumpToUnanswered() {
	e := m.elicit
	if e == nil {
		return
	}
	n := len(e.Questions)
	for i := 1; i <= n; i++ {
		idx := (e.Cur + i) % n
		if !e.Questions[idx].answered() {
			e.Cur = idx
			return
		}
	}
}

// ---------------------------------------------------------------------------
// 渲染
// ---------------------------------------------------------------------------

// renderElicitPanel 渲染追问面板（钉在输入区上方）：
//
//	┃ 第 1/2 题 · <message 首行>          （强调绿标题 + 边条）
//	┃ ▸ 1/2 <题面标题>
//	┃     <题面说明（折行）>
//	┃     ❯ 1) <选项标题>  <选项说明（暗色）>
//	┃       2) <选项标题>
//	┃       0 自己写…                    ← 自定义答案（enter 确认）
//	┃   ▸ 2/2 <题面标题>            ← 非当前题只留题头一行
//	┃     ✓ 已答：<选项标题>
//	┃ （空一行）
//	┃ space 选/勾 · 0 自己写 · ↑↓ 移动 · ←→ 换题 · enter 提交 · esc 拒绝（整行暗色）
//
// 透明度原则：只前景色；左缘琥珀竖条当"面板边"（无底色）。
func (m *model) renderElicitPanel(w int) []string {
	e := m.elicit
	if e == nil || e.Unsupported {
		return nil
	}
	gutter := "  " + accentStyle.Render("\u2503") + " " // 边条用强调绿（鲜艳档），区别于权限面板的琥珀
	textW := w - 4
	if textW < 12 {
		textW = 12
	}
	var out []string

	head := fmt.Sprintf("第 %d 题", e.Cur+1)
	if len(e.Questions) > 1 {
		head = fmt.Sprintf("第 %d/%d 题", e.Cur+1, len(e.Questions))
	}
	headLine := accentStyle.Bold(true).Render(head)
	if s := firstLine(e.Message); s != "" {
		headLine += dimStyle.Render(" \u00B7 ") + textStyle.Render(clipWidth(s, textW-12))
	}
	out = append(out, gutter+headLine)

	for i, q := range e.Questions {
		cur := i == e.Cur
		mark := "  "
		if cur {
			mark = dimStyle.Render("\u25B8") + " "
		}
		title := dimStyle.Render(fmt.Sprintf("%d/%d ", i+1, len(e.Questions))) + textStyle.Render(clipWidth(q.Title, textW-12))
		if cur {
			title = dimStyle.Render(fmt.Sprintf("%d/%d ", i+1, len(e.Questions))) + accentStyle.Render(clipWidth(q.Title, textW-12))
		}
		out = append(out, gutter+mark+title)

		// 非当前题：只留题头 + 已答内容（面板别太长）
		if !cur {
			if txt := q.answerText(); txt != "" {
				out = append(out, gutter+"    "+dimStyle.Render("\u2713 已答：")+textStyle.Render(clipWidth(txt, textW-10)))
			}
			continue
		}
		for _, ln := range wrapText(q.Desc, textW-4) {
			out = append(out, gutter+"    "+dimStyle.Render(ln))
		}
		for j, o := range q.Options {
			if j > 8 {
				out = append(out, gutter+"      "+dimStyle.Render(fmt.Sprintf("… 还有 %d 个选项", len(q.Options)-j)))
				break
			}
			pre := " "
			if j == q.cursor {
				pre = userBarStyle.Render("\u276F")
			} else if q.choiceAt(j) {
				pre = userBarStyle.Render("\u2713")
			}
			label := fmt.Sprintf("%d) %s", j+1, o.Title)
			line := gutter + "    " + pre + " " + textStyle.Render(clipWidth(label, textW-8))
			if o.Desc != "" {
				line += "  " + dimStyle.Render(clipWidth(o.Desc, textW/2))
			}
			if q.choiceAt(j) || j == q.cursor {
				// 选中/光标项用亮色标签（透明度原则：只换前景色；颜色随主题）
				line = gutter + "    " + pre + " " + inputTextStyle.Render(clipWidth(label, textW-8))
				if o.Desc != "" {
					line += "  " + dimStyle.Render(clipWidth(o.Desc, textW/2))
				}
			}
			out = append(out, line)
		}

		// 自己写（0 号伪选项）：引擎只验答案非空，自定义文本照样接受
		switch {
		case q.TextMode:
			out = append(out, gutter+"    "+userBarStyle.Render("\u276F")+" "+inputTextStyle.Render("自己写："+q.Text)+"\u258C")
		case strings.TrimSpace(q.Text) != "":
			out = append(out, gutter+"    "+userBarStyle.Render("\u2713")+" "+textStyle.Render("自己写："+clipWidth(q.Text, textW-16)))
		default:
			out = append(out, gutter+"    "+dimStyle.Render("0")+" "+dimStyle.Render("自己写…"))
		}
	}

	// 键位提示：与选项之间空一行（边条保持连续），整行调暗不与选项抢眼
	out = append(out, strings.TrimRight(gutter, " "))
	hints := []string{
		dimStyle.Render("space") + " " + dimStyle.Render("选/勾"),
		dimStyle.Render("0") + " " + dimStyle.Render("自己写"),
		dimStyle.Render("\u2191\u2193") + " " + dimStyle.Render("移动"),
		dimStyle.Render("\u2190\u2192") + " " + dimStyle.Render("换题"),
		dimStyle.Render("enter") + " " + dimStyle.Render("提交"),
		dimStyle.Render("esc") + " " + dimStyle.Render("拒绝"),
	}
	out = append(out, gutter+strings.Join(hints, dimStyle.Render(" \u00B7 ")))
	if n := len(m.elicitQueue); n > 0 {
		out = append(out, gutter+dimStyle.Render(fmt.Sprintf("还有 %d 条追问", n)))
	}
	return out
}

// firstLine 取多行文本的第一行（标题/摘要用）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------------
// -elicittest：追问面板自检（无 TTY / 无引擎）
// ---------------------------------------------------------------------------

// elicitSampleParams 用 letcode question 工具的真实形状造一条 elicitation/create：
// 三道题（单选 + 多选 + 无 oneOf 的自由输入题），message 是各题按行拼接。
func elicitSampleParams() map[string]any {
	return map[string]any{
		"sessionId": "1789723456789-9999-0",
		"mode":      "form",
		"message":   "你希望用哪种语言实现？\n要不要顺带补测试？\n还有什么想补充的？",
		"requestedSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question_1": map[string]any{
					"type":        "string",
					"title":       "语言",
					"description": "你希望用哪种语言实现？",
					"oneOf": []any{
						map[string]any{"const": "Rust", "title": "Rust", "description": "系统级、无 GC"},
						map[string]any{"const": "Go", "title": "Go", "description": "上手快、生态好"},
						map[string]any{"const": "TypeScript", "title": "TypeScript", "description": "前端友好"},
					},
				},
				"question_2": map[string]any{
					"type":        "array",
					"minItems":    1,
					"title":       "测试",
					"description": "要不要顺带补测试？",
					"items": map[string]any{
						"type": "string",
						"oneOf": []any{
							map[string]any{"const": "unit", "title": "单元测试"},
							map[string]any{"const": "e2e", "title": "端到端测试"},
						},
					},
				},
				"question_3": map[string]any{
					"type":        "string",
					"title":       "补充",
					"description": "还有什么想补充的？",
				},
			},
			"required": []any{"question_1", "question_2", "question_3"},
		},
	}
}

// runElicitTest 断言解析 / 面板渲染 / 键位 / 应答体形状，全部通过才退出码 0。
func runElicitTest() {
	failed := false
	check := func(name string, cond bool) {
		tag := "PASS"
		if !cond {
			tag = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", tag, name)
	}

	// ① 解析
	e := parseElicitRequest(float64(7), elicitSampleParams())
	okParse := !e.Unsupported && len(e.Questions) == 3 &&
		e.Questions[0].Key == "question_1" && e.Questions[0].Title == "语言" && !e.Questions[0].Multiple &&
		len(e.Questions[0].Options) == 3 && e.Questions[0].Options[0].Value == "Rust" &&
		e.Questions[0].Options[0].Desc == "系统级、无 GC" &&
		e.Questions[1].Multiple && len(e.Questions[1].Options) == 2 &&
		e.Questions[2].Title == "补充" && len(e.Questions[2].Options) == 0
	check("解析：3 题（单选+多选+自由输入）、标题/说明/oneOf 取值与说明各就位", okParse)

	// ② 非 form（url）与空表单：判为不支持，直接回 cancel
	urlReq := parseElicitRequest(nil, map[string]any{"mode": "url", "message": "x"})
	check("非 form mode（url）判为不支持（应答走 cancel，不渲染面板）", urlReq.Unsupported)

	// ③ 渲染：宽度合规 / 无背景色 / 含题面与选项 / 光标与勾选字形
	m := model{width: 100, feed: NewFeed(), status: stIdle}
	m.elicit = e
	lines := m.renderElicitPanel(m.blockWidth())
	okW, okBG := true, true
	for _, ln := range lines {
		if lipgloss.Width(ln) > m.blockWidth() {
			okW = false
		}
		if hasStrayBackground(ln) {
			okBG = false
		}
	}
	joined := stripANSI(strings.Join(lines, "\n"))
	okContent := strings.Contains(joined, "第 1/3 题") && strings.Contains(joined, "语言") &&
		strings.Contains(joined, "Rust") && strings.Contains(joined, "1)") && strings.Contains(joined, "2)") &&
		strings.Contains(joined, "自己写") && strings.Contains(joined, "esc") && strings.Contains(joined, "enter")
	okAccent := strings.Contains(strings.Join(lines, "\n"), "38;2;78;224;94") // 边条/标题 = 强调绿（鲜艳档）
	fmt.Println("== 追问面板样张（宽 100）==")
	for _, ln := range lines {
		fmt.Printf("  %s\n", stripANSI(ln))
	}
	check("渲染：宽度合规 / 无背景色 / 边条强调绿 / 题面+选项+自写行+键位提示齐全", okW && okBG && okContent && okAccent)

	// ④ 键位：↑↓ 移动、space 选中、自动跳题、enter 跳未答/提交、0 自己写
	next := model{width: 100, feed: NewFeed(), status: stIdle, elicit: e}
	key := func(mm model, code rune, text string) model {
		mm.elicitKey(press(code, text))
		return mm
	}
	next = key(next, tea.KeyDown, "")
	okCursor := next.elicit.Questions[0].cursor == 1
	next = key(next, tea.KeySpace, " ")
	okPick := next.elicit.Questions[0].chosen == 1 && next.elicit.Cur == 1 // 单选选中后自动跳到未答的题
	next = key(next, tea.KeySpace, " ")
	next = key(next, tea.KeyDown, "")
	next = key(next, tea.KeySpace, " ")
	okMulti := next.elicit.Questions[1].choiceAt(0) && next.elicit.Questions[1].choiceAt(1)
	next = key(next, tea.KeyEnter, "") // 没答完 → 跳到下一道未答题（自由输入题）
	okJump := next.elicit.Cur == 2
	next = key(next, '0', "0") // 自己写：进入输入态（真实终端 0 = 带 Text 的 rune 键）
	okTextMode := next.elicit.Questions[2].TextMode
	next = key(next, 0, "好的")
	okTyped := next.elicit.Questions[2].Text == "好的" // 输入态下缓冲实时入账（enter 才确认退出）
	next = key(next, tea.KeyEnter, "")               // 全答完 → 提交收桌（自检 client 为 nil，只走应答组装）
	okSubmitted := next.elicit == nil && e.Questions[2].Text == "好的"
	check("键位：↑↓/space 选项与自动跳题 / enter 跳未答 / 0 自己写中文 / 答完 enter 提交收桌",
		okCursor && okPick && okMulti && okJump && okTextMode && okTyped && okSubmitted)

	// ⑤ 应答体（accept）形状：单选=字符串、多选=数组、自写=原文
	payload := e.response(false)
	content := asMap(payload["content"])
	_, isStr := content["question_1"].(string)
	arr, isArr := content["question_2"].([]string)
	txt3, isTxt := content["question_3"].(string)
	okPayload := payload["action"] == "accept" && isStr && isArr && len(arr) == 2 && isTxt && txt3 == "好的"
	check("应答体：accept + content（单选=字符串 / 多选=数组 / 自写=原文）", okPayload)

	// ⑥ esc → decline；未答的题不进 content
	e2 := parseElicitRequest(float64(8), elicitSampleParams())
	e2.Questions[0].toggleAt(0)
	decl := e2.response(true)
	part := asMap(e2.response(false)["content"])
	_, onlyFirst := part["question_1"]
	_, noSecond := part["question_2"]
	_, noThird := part["question_3"]
	check("拒绝：action=decline；accept 时未作答的题不进 content",
		decl["action"] == "decline" && onlyFirst && !noSecond && !noThird)

	// ⑦ 队列：第二条追问排队，答完第一条自动上桌
	mq := model{width: 100, feed: NewFeed(), status: stIdle}
	e1 := parseElicitRequest(float64(1), elicitSampleParams())
	e2q := parseElicitRequest(float64(2), elicitSampleParams())
	mq.pushElicit(e1)
	mq.pushElicit(e2q)
	okQueue := mq.elicit == e1 && len(mq.elicitQueue) == 1 && mq.feed.Len() == 2
	mq.settleElicit(e1, "已回答", "x")
	okNext := mq.elicit == e2q && len(mq.elicitQueue) == 0 && e1.Placeholder.Kind == kReceipt
	check("队列：第二条排队 / 答完自动上桌 / 占位换绿条回执", okQueue && okNext)

	// ⑧ View() 集成：面板行数从消息区扣出，整屏行数不变、面板真的上屏。
	// 真机教训（2026-09-18）：syncLayout 扣了高度但 View 漏拼装 = 面板隐形 +
	// esc 被吃成 decline（引擎收到"用户拒绝回答"）。
	{
		vm := model{
			width: 120, height: 30, feed: NewFeed(), status: stIdle, panelOn: false,
			sessionID: "1234abcd-0000-0000-0000-000000000000",
			sessTitle: "追问集成样张", modelLabel: "Step 3.7 Flash", modeID: "default",
		}
		vm.elicit = parseElicitRequest(float64(9), elicitSampleParams())
		vm.syncLayout()
		content := vm.View().Content
		viewLines := strings.Split(strings.TrimRight(content, "\n"), "\n")
		okRows := len(viewLines) == vm.height
		okShow := strings.Contains(stripANSI(content), "第 1/3 题") &&
			strings.Contains(stripANSI(content), "Rust")
		okVW := true
		for _, ln := range viewLines {
			if lipgloss.Width(ln) > vm.width {
				okVW = false
				break
			}
		}
		if hasStrayBackground(content) {
			okVW = false
		}
		check("View() 集成：行数不变 / 面板可见 / 宽度合规 / 背景干净", okRows && okShow && okVW)
	}

	if failed {
		fmt.Println("elicittest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("elicittest: 全部通过")
}
