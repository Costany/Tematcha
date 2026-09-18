// ui_perm.go —— 权限面板（M2a）：把 session/request_permission 接成人机问答
//
// 协议形状（对过 agent-client-protocol-schema-1.7.0 与 letcode driver.rs）：
//
//	请求 params: {sessionId, toolCall{toolCallId,title,kind,rawInput},
//	              options:[{optionId, name, kind}]}
//	应答 result: {"outcome": {"outcome": "selected", "optionId": "<optionId>"}}
//	取消 result: {"outcome": {"outcome": "cancelled"}}
//
// options 的 kind 固定四选（snake_case）：allow_once / allow_always /
// reject_once / reject_always；optionId 通常同名，但一律"引擎给什么回什么"，
// 不猜。allow_always 的 name 是引擎动态生成的授权描述（如
// "Allow cargo test always"），必须原样展示。
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// PermOption 一个可选项（原样保存引擎给的 id/name/kind）。
type PermOption struct {
	ID   string // optionId：应答时原样回传
	Name string // 展示文案（allow_always 是引擎动态生成的）
	Kind string // allow_once / allow_always / reject_once / reject_always
}

// PermRequest 一条待审批的权限请求。
type PermRequest struct {
	RPCID       any    // 反向请求的原始 JSON-RPC id（原样回传，类型不能变）
	Title       string // 工具调用标题（如 shell__exec npm test）
	Command     string // rawInput.command（有则单独展示成 $ 行）
	RawInput    string // rawInput 的紧凑 JSON（备查）
	Options     []PermOption
	Placeholder *FeedItem // 消息区的"→ 等待批准…"占位（答完换成回执）
}

// parsePermRequest 从 session/request_permission 的 params 解析面板数据。
func parsePermRequest(rpcID any, params map[string]any) *PermRequest {
	p := &PermRequest{RPCID: rpcID}
	if tc := asMap(params["toolCall"]); tc != nil {
		p.Title, _ = tc["title"].(string)
		if raw := asMap(tc["rawInput"]); raw != nil {
			if cmd, ok := raw["command"].(string); ok {
				p.Command = cmd
			}
			if b, err := json.Marshal(raw); err == nil {
				p.RawInput = string(b)
			}
		}
	}
	if strings.TrimSpace(p.Title) == "" {
		p.Title = "工具调用"
	}
	for _, o := range asList(params["options"]) {
		om := asMap(o)
		if om == nil {
			continue
		}
		id, _ := om["optionId"].(string)
		if id == "" {
			continue
		}
		name, _ := om["name"].(string)
		kind, _ := om["kind"].(string)
		p.Options = append(p.Options, PermOption{ID: id, Name: name, Kind: kind})
	}
	return p
}

// optionByKind 按 kind 取选项（没有该 kind 时返回 nil）。
func (p *PermRequest) optionByKind(kind string) *PermOption {
	for i := range p.Options {
		if p.Options[i].Kind == kind {
			return &p.Options[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// model 侧：队列与应答
// ---------------------------------------------------------------------------

// pushPerm 收下一条权限请求：进队列；面板空着就把队首点亮。
// 权限面板接管键盘：先把工具卡选择态退掉，避免两套键位打架。
func (m *model) pushPerm(p *PermRequest) {
	m.exitCardNav()
	p.Placeholder = m.feed.Append(kSys, "\u2192 等待批准… "+p.Title)
	m.feed.ScrollToBottom()
	m.permQueue = append(m.permQueue, p)
	if m.perm == nil {
		m.popPerm()
	}
	m.syncLayout()
}

// popPerm 队列前移一位（队列空则收起面板）。
func (m *model) popPerm() {
	if len(m.permQueue) == 0 {
		m.perm = nil
		return
	}
	m.perm = m.permQueue[0]
	m.permQueue = m.permQueue[1:]
}

// answerPerm 按 kind 应答当前请求（选项里没有该 kind 就忽略这次按键）。
func (m *model) answerPerm(kind string) {
	p := m.perm
	if p == nil {
		return
	}
	opt := p.optionByKind(kind)
	if opt == nil {
		return
	}
	_ = m.client.Respond(p.RPCID, map[string]any{
		"outcome": map[string]any{"outcome": "selected", "optionId": opt.ID},
	})
	// 回执动词随选项性质走（allow_* / reject_*）：早先这里写死了"已批准"，
	// 按 n 拒绝时也显示"已批准"（协议应答是对的，只是回执文案错）。
	verb := "已批准"
	if strings.HasPrefix(opt.Kind, "reject") {
		verb = "已拒绝"
	}
	m.settlePerm(p, verb, clipWidth(opt.Name, 48))
}

// cancelPerms 取消全部待审批：先取消回合（若在回合中），再逐条回 cancelled。
// 协议要求：客户端取消回合时，所有挂起的权限请求都必须以 cancelled 应答。
func (m *model) cancelPerms() {
	if m.perm == nil && len(m.permQueue) == 0 {
		return
	}
	if m.busy {
		m.client.CancelTurn(m.sessionID)
		m.status = stCancelling
	}
	if m.perm != nil {
		m.settlePermCancelled(m.perm)
	}
	for _, p := range m.permQueue {
		m.settlePermCancelled(p)
	}
	m.perm, m.permQueue = nil, nil
	m.syncLayout()
}

// dropPerms 丢弃全部待审批（不回应答）：引擎断开 / 回合已结束等"请求已作废"的场景。
func (m *model) dropPerms(reason string) {
	if m.perm == nil && len(m.permQueue) == 0 {
		return
	}
	if p := m.perm; p != nil && p.Placeholder != nil {
		m.feed.SetText(p.Placeholder, "审批失效 · "+p.Title+"（"+reason+"）")
	}
	for _, p := range m.permQueue {
		if p.Placeholder != nil {
			m.feed.SetText(p.Placeholder, "审批失效 · "+p.Title+"（"+reason+"）")
		}
	}
	m.perm, m.permQueue = nil, nil
	m.syncLayout()
}

// settlePerm 结一条已选择的请求：占位换成绿条回执，然后换下一条上桌。
func (m *model) settlePerm(p *PermRequest, verb, detail string) {
	if p.Placeholder != nil {
		p.Placeholder.Kind = kReceipt
		m.feed.SetText(p.Placeholder, verb+" \u00B7 "+p.Title+" \u00B7 "+detail)
	}
	m.popPerm()
	m.syncLayout()
}

// settlePermCancelled 结一条被取消的请求：占位换成暗色说明（不给绿条）。
func (m *model) settlePermCancelled(p *PermRequest) {
	if p.Placeholder != nil {
		m.feed.SetText(p.Placeholder, "已取消审批 · "+p.Title)
	}
}

// syncLayout 按面板高度重算消息区高度：面板占几行、消息区就矮几行，
// 整屏行数恒定（面板钉在输入区上方、随内容上推，不悬浮遮挡）。
func (m *model) syncLayout() {
	panelH := len(m.renderPermPanel(m.width))
	cmdH := len(m.renderCmdPalette())     // M4b：命令弹层也钉在输入区上方，一起让位
	sessH := len(m.renderSessionPicker()) // M4d：会话选择列表同理
	h := m.height - 6 - panelH - cmdH - sessH
	if h < 3 {
		h = 3
	}
	// 右栏（M3b）显示时，消息区宽度让位（分隔列 + 右栏内容）
	w := m.width - 6
	if m.panelVisible() {
		w -= panelW + panelSepW
	}
	if w < 30 {
		w = 30
	}
	m.feed.SetSize(w, h)
}

// ---------------------------------------------------------------------------
// 面板渲染
// ---------------------------------------------------------------------------

// renderPermPanel 渲染权限面板（钉在输入区上方）：
//
//	┃ 需要批准 · shell__exec npm test
//	┃ $ npm test
//	┃ {"command":"npm test","cwd":"…"}
//	┃ y 允许一次 · a Allow cargo test always · n 拒绝 · esc 取消
//	┃ 还有 2 个待处理
//
// 透明度原则：只前景色；左侧一条琥珀色竖条当"面板边"（无底色）。
func (m *model) renderPermPanel(w int) []string {
	p := m.perm
	if p == nil {
		return nil
	}
	gutter := "  " + warnStyle.Render("\u2503") + " "
	textW := w - 4
	if textW < 8 {
		textW = 8
	}
	var out []string

	// ① 标题：琥珀"需要批准" + 工具标题
	head := warnStyle.Render("需要批准")
	if p.Title != "" {
		head += dimStyle.Render(" · ") + textStyle.Render(clipWidth(p.Title, textW-9))
	}
	out = append(out, gutter+head)

	// ② 命令正文（rawInput.command）+ 原始参数备查（紧凑 JSON，最多 3 行）
	if p.Command != "" {
		for i, ln := range wrapText(p.Command, textW-2) {
			if i == 0 {
				out = append(out, gutter+dimStyle.Render("$ ")+textStyle.Render(ln))
			} else {
				out = append(out, gutter+"  "+textStyle.Render(ln))
			}
		}
	}
	if p.RawInput != "" && p.RawInput != "{}" {
		lines := wrapText(p.RawInput, textW)
		if len(lines) > 3 {
			lines = append(lines[:3], "\u2026")
		}
		for _, ln := range lines {
			out = append(out, gutter+dimStyle.Render(ln))
		}
	}

	// ③ 按键提示（按键行只放短标签，别把长文案塞进来）
	var hints []string
	if p.optionByKind("allow_once") != nil {
		hints = append(hints, warnStyle.Render("y")+" "+textStyle.Render("允许一次"))
	}
	always := p.optionByKind("allow_always")
	if always != nil {
		hints = append(hints, warnStyle.Render("a")+" "+textStyle.Render("始终允许"))
	}
	if p.optionByKind("reject_once") != nil {
		hints = append(hints, warnStyle.Render("n")+" "+textStyle.Render("拒绝"))
	}
	hints = append(hints, warnStyle.Render("esc")+" "+textStyle.Render("取消"))
	out = append(out, gutter+strings.Join(hints, dimStyle.Render(" · ")))

	// ④ 始终允许的"记住范围"（引擎的动态文案，如 shell__exec: Remove-Item x），
	// 单独一行、暗色、超长截断 —— 直接塞进按键行会看起来像又一条命令。
	if always != nil && strings.TrimSpace(always.Name) != "" {
		label := "始终允许范围："
		out = append(out, gutter+dimStyle.Render(label)+
			dimStyle.Render(clipWidth(always.Name, textW-lipgloss.Width(label))))
	}

	// ⑤ 队列提示
	if n := len(m.permQueue); n > 0 {
		out = append(out, gutter+dimStyle.Render(fmt.Sprintf("还有 %d 个待处理", n)))
	}
	return out
}

// clipWidth 截到 maxw 显示宽（超出补 …；按显示宽度算，CJK 安全）。
func clipWidth(s string, maxw int) string {
	if lipgloss.Width(s) <= maxw {
		return s
	}
	if maxw <= 1 {
		return "\u2026"
	}
	var b []rune
	w := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > maxw-1 {
			break
		}
		b = append(b, r)
		w += rw
	}
	return string(b) + "\u2026"
}

// ---------------------------------------------------------------------------
// -permtest：无 TTY 的面板布局自检
// ---------------------------------------------------------------------------

// runPermTest 渲染两个样张面板（有/无 always 选项）+ 一条回执行：
//   - 逐行打印可见文本与宽度（已剥 ANSI）
//   - 打印回执行的原始 ANSI（核对绿条颜色）
//   - 检查输出里是否残留背景色序列（透明度原则）
func runPermTest() {
	const w = 76
	withAlways := &PermRequest{
		Title:    "shell__exec Remove-Item demo-lab/hello.txt",
		Command:  "Remove-Item demo-lab/hello.txt",
		RawInput: `{"command":"Remove-Item demo-lab/hello.txt","timeout_secs":10}`,
		Options: []PermOption{
			{ID: "allow_once", Name: "Allow once", Kind: "allow_once"},
			// 引擎的 always 文案是"记住范围"（实测长这样）
			{ID: "allow_always", Name: "shell__exec: Remove-Item demo-lab/hello.txt", Kind: "allow_always"},
			{ID: "reject_once", Name: "Deny", Kind: "reject_once"},
		},
	}
	noAlways := &PermRequest{
		Title:    "fs__write demo-lab/notes.txt",
		RawInput: `{"path":"demo-lab/notes.txt","bytes_written":128}`,
		Options: []PermOption{
			{ID: "allow_once", Name: "Allow once", Kind: "allow_once"},
			{ID: "reject_once", Name: "Deny", Kind: "reject_once"},
		},
	}
	receipt := &FeedItem{Kind: kReceipt, Text: "已批准 · shell__exec npm test · Allow cargo test always"}

	var raw []string
	dump := func(title string, lines []string) {
		fmt.Println(title)
		for _, ln := range lines {
			raw = append(raw, ln)
			fmt.Printf("%s  (w=%d)\n", stripANSI(ln), lipgloss.Width(ln))
		}
		fmt.Println()
	}

	m := &model{width: w, perm: withAlways}
	m.permQueue = []*PermRequest{noAlways}
	dump("== 面板样张 1（有 always；后面还有 1 条排队）==", m.renderPermPanel(w))

	m2 := &model{width: w, perm: noAlways}
	dump("== 面板样张 2（无 always 选项）==", m2.renderPermPanel(w))

	dump("== 回执行（绿条）==", renderItem(receipt, w))

	fmt.Println("== 回执行原始 ANSI（核对绿条颜色）==")
	for _, ln := range renderItem(receipt, w) {
		fmt.Printf("raw: %q\n", ln)
	}

	bad := false
	for _, ln := range raw {
		if hasBackgroundColor(ln) {
			bad = true
		}
	}
	fmt.Println()
	if bad {
		fmt.Println("!! 背景色检查：检测到背景色序列（透明度原则被破坏）")
	} else {
		fmt.Println("OK 背景色检查：未检测到背景色序列")
	}
}
