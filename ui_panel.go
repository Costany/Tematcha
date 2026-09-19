// ui_panel.go —— 右栏（M3b · §4/§5：会话信息 / 上下文 / 待办）
//
// 布局：消息区右侧一条「  │ 」分隔列 + 固定宽度的信息栏；消息区宽度在
// syncLayout 里让位。窄屏（< minPanelWidth）自动隐藏，宽了恢复；ctrl+b
// 手动开关（见 main.handleKey）。
//
// 数据来源（ACP，字段名已对 schema 1.7.0 核对）：
//   - 会话标题：session_info_update {title}
//   - 待办：plan {entries:[{content,priority,status}]}（全量快照，整体替换；
//     blocked→pending、cancelled→completed 是引擎侧有损映射）
//   - 模型/模式/用量：与状态栏同源（modelLabel / modeID / usageUsed / usageSize）
//
// 透明度原则：只前景色；分区标题暗色，内容浅色；超长截断。
package main

import (
	"fmt"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

const (
	panelW        = 28  // 右栏内容宽度
	panelSepW     = 4   // 分隔列「  │ 」的宽度
	minPanelWidth = 100 // 低于此宽度自动隐藏右栏（宽了恢复）
)

// TodoEntry 待办快照里的一条（ACP plan entry 的精简版；priority 不展示）。
type TodoEntry struct {
	Content string
	Status  string // pending / in_progress / completed
}

// parsePlan 解析 ACP plan 的 entries（content/status；缺失字段跳过）。
func parsePlan(entries []any) []TodoEntry {
	out := make([]TodoEntry, 0, len(entries))
	for _, e := range entries {
		em := asMap(e)
		if em == nil {
			continue
		}
		content, _ := em["content"].(string)
		status, _ := em["status"].(string)
		out = append(out, TodoEntry{Content: content, Status: status})
	}
	return out
}

// panelVisible 右栏是否显示：用户开关（ctrl+b）&& 宽度足够（窄屏自动隐藏，宽了恢复）。
func (m model) panelVisible() bool {
	return m.panelOn && m.width >= minPanelWidth
}

// panelDoneCount 待办完成数 / 总数（计数行用）。
func (m model) panelDoneCount() (done, total int) {
	for _, t := range m.todos {
		if t.Status == "completed" {
			done++
		}
	}
	return done, len(m.todos)
}

// todoMark 待办状态字形与配色：○ 待办 / ◐ 进行中 / ✓ 完成。
func todoMark(status string) (string, lipgloss.Style) {
	switch status {
	case "in_progress":
		return "\u25D0", thinkStyle
	case "completed":
		return "\u2713", dimStyle
	default:
		return "\u25CB", textStyle
	}
}

// renderPanel 渲染右栏内容（会话 / 上下文 / 待办），行数固定为 height。
func (m model) renderPanel(height int) []string {
	var out []string

	// ① 会话信息
	out = append(out, dimStyle.Render("会话"))
	if m.sessTitle != "" {
		for _, ln := range wrapText(m.sessTitle, panelW) {
			out = append(out, textStyle.Render(ln))
		}
	} else {
		out = append(out, dimStyle.Render("（未命名）"))
	}
	if m.modelLabel != "" {
		out = append(out, dimStyle.Render("模型 ")+textStyle.Render(clipWidth(m.modelLabel, panelW-7)))
	}
	if m.modeID != "" {
		out = append(out, dimStyle.Render("模式 ")+m.renderModeBadge())
	}
	if sid := m.sessionID; sid != "" {
		if len(sid) > 8 {
			sid = sid[:8]
		}
		out = append(out, dimStyle.Render("会话 ")+textStyle.Render(sid))
	}
	out = append(out, "")

	// ② 上下文（有数据才显示）
	if m.usageSize > 0 {
		out = append(out, dimStyle.Render("上下文"))
		out = append(out, textStyle.Render(fmtK(m.usageUsed)+" / "+fmtK(m.usageSize)))
		rest := m.usageSize - m.usageUsed
		if rest < 0 {
			rest = 0
		}
		out = append(out, dimStyle.Render("剩余 "+fmtK(rest)))
		out = append(out, "")
	}

	// ③ 待办（空则整节省略；放不下时截断 + 溢出提示）
	if len(m.todos) > 0 {
		done, total := m.panelDoneCount()
		out = append(out, dimStyle.Render(fmt.Sprintf("待办 %d/%d", done, total)))
		room := height - len(out)
		shown := m.todos
		overflow := 0
		if room > 0 && len(shown) > room {
			keep := room - 1 // 留一行给溢出提示
			if keep < 0 {
				keep = 0
			}
			overflow = len(shown) - keep
			shown = shown[:keep]
		}
		for _, t := range shown {
			mark, st := todoMark(t.Status)
			cst := textStyle
			if t.Status == "completed" {
				cst = dimStyle
			}
			out = append(out, st.Render(mark)+" "+cst.Render(clipWidth(t.Content, panelW-2)))
		}
		if overflow > 0 {
			out = append(out, dimStyle.Render(fmt.Sprintf("… 还有 %d 条", overflow)))
		}
	}

	// 补齐 / 截断到 height
	for len(out) < height {
		out = append(out, "")
	}
	if len(out) > height {
		out = out[:height]
	}
	return out
}

// ---------------------------------------------------------------------------
// -paneltest：右栏样张自检（布局 / 可见性判定 / parsePlan / 背景色）
// ---------------------------------------------------------------------------

// runPanelTest 渲染右栏样张并断言：
//   - 每行可见宽度 ≤ panelW，且无背景色（透明度原则）
//   - panelVisible：<100 自动隐藏、ctrl+b 关闭、≥100 恢复
//   - parsePlan：content/status 抽取正确
func runPanelTest() {
	bad := false
	m := model{
		width: 120, panelOn: true,
		sessionID:  "0f2a1c9e-dead-beef-0000-123456789abc",
		sessTitle:  "演示会话：把 README 改一遍",
		modelLabel: "Step 3.7 Flash", modeID: "default",
		usageUsed: 87300, usageSize: 320000,
		todos: []TodoEntry{
			{Content: "读取 demo-lab 的目录结构", Status: "completed"},
			{Content: "把 README.md 的第一行改成标题（这行故意写长来测截断）", Status: "in_progress"},
			{Content: "运行 npm test 验证", Status: "pending"},
			{Content: "提交改动并总结", Status: "pending"},
		},
	}

	fmt.Println("== 右栏样张（每行 ≤ 28 可见宽）==")
	for _, ln := range m.renderPanel(24) {
		w := lipgloss.Width(ln)
		if w > panelW {
			bad = true
		}
		if hasBackgroundColor(ln) {
			bad = true
		}
		fmt.Printf("%s  (w=%d)\n", stripANSI(ln), w)
	}

	fmt.Println()
	fmt.Println("== panelVisible 判定 ==")
	for _, c := range []struct {
		w    int
		on   bool
		want bool
	}{
		{120, true, true}, {100, true, true}, {99, true, false}, {120, false, false},
	} {
		mm := m
		mm.width, mm.panelOn = c.w, c.on
		got := mm.panelVisible()
		if got != c.want {
			bad = true
		}
		fmt.Printf("  w=%d on=%v → %v（期望 %v）\n", c.w, c.on, got, c.want)
	}

	fmt.Println()
	fmt.Println("== parsePlan 抽查 ==")
	gotPlan := parsePlan([]any{map[string]any{"content": "a", "priority": "medium", "status": "in_progress"}})
	okPlan := len(gotPlan) == 1 && gotPlan[0].Content == "a" && gotPlan[0].Status == "in_progress"
	if !okPlan {
		bad = true
	}
	fmt.Printf("  解析 1 条 → %v\n", okPlan)

	// 集成抽查：整个 View() 的横向组合（消息区 │ 右栏）：行数/分隔列/宽度守恒
	vm := model{
		width: 120, height: 30, feed: NewFeed(), status: stIdle, panelOn: true,
		sessionID: "1234abcd-0000-0000-0000-000000000000", sessTitle: "集成样张",
		modelLabel: "Step 3.7 Flash", modeID: "default",
	}
	vm.syncLayout()
	vm.feed.Append(kUser, "看看右栏")
	content := vm.View().Content
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	okRows := len(lines) == vm.height
	plain := stripANSI(content)
	okSep := strings.Contains(plain, "\u2502")
	okPanel := strings.Contains(plain, "会话") && strings.Contains(plain, "集成样张")
	okWidth := true
	for _, ln := range lines {
		if lipgloss.Width(ln) > vm.width {
			okWidth = false
			break
		}
	}
	if !okRows || !okSep || !okPanel || !okWidth {
		bad = true
	}
	fmt.Println()
	fmt.Println("== View() 集成抽查 ==")
	fmt.Printf("  行数 %d/%d：%v ｜ 分隔列：%v ｜ 右栏内容：%v ｜ 宽度≤%d：%v\n",
		len(lines), vm.height, okRows, okSep, okPanel, vm.width, okWidth)
	if hasBackgroundColor(content) {
		bad = true
		fmt.Println("  !! 背景色检查：检测到背景色序列")
	} else {
		fmt.Println("  OK 背景色检查：未检测到背景色序列")
	}

	if bad {
		fmt.Println("paneltest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("paneltest: 全部通过")
}

// ---------------------------------------------------------------------------
// M5b：消息区钉面板（# Todos）
// ---------------------------------------------------------------------------

// todosMaxRows 钉面板最多显示几条待办（超出的折成一行「… 还有 N 条」）。
const todosMaxRows = 6

// isTodosToggle 输入是否正好是 /todos（客户端命令：开关钉面板，不发引擎）。
// 注意：letcode 的命令表里没有 /todos，不拦的话它会被当成普通提问发给模型。
func isTodosToggle(text string) bool {
	return strings.TrimSpace(text) == "/todos"
}

// todosVisible 钉面板是否显示：开关开着且快照里确实有待办。
func (m model) todosVisible() bool {
	return m.todosOn && len(m.todos) > 0
}

// renderTodosPinned 渲染消息区顶部的钉面板（不随消息区滚动消失）：
//
//	# Todos · 1/4
//	  ○ 读取 demo-lab 的目录结构
//	  ◐ 把 README 第一行改成标题
//	  ✓ 运行 npm test 验证
//
// 透明度原则：只前景色；标题暗色、正文浅色、完成项暗色。宽度按显示宽裁剪。
// 篇幅：最多 todosMaxRows 条 + 一行溢出提示。
func (m model) renderTodosPinned(w int) []string {
	if !m.todosVisible() {
		return nil
	}
	if w < 12 {
		w = 12
	}
	done, total := m.panelDoneCount()
	out := []string{dimStyle.Render(fmt.Sprintf("# Todos \u00B7 %d/%d", done, total))}
	shown := m.todos
	overflow := 0
	if len(shown) > todosMaxRows {
		overflow = len(shown) - todosMaxRows
		shown = shown[:todosMaxRows]
	}
	for _, t := range shown {
		mark, st := todoMark(t.Status)
		cst := textStyle
		if t.Status == "completed" {
			cst = dimStyle
		}
		out = append(out, "  "+st.Render(mark)+" "+cst.Render(clipWidth(t.Content, w-4)))
	}
	if overflow > 0 {
		out = append(out, "  "+dimStyle.Render(fmt.Sprintf("\u2026 还有 %d 条", overflow)))
	}
	return out
}

// ---------------------------------------------------------------------------
// -todostest：钉面板自检（渲染 / 超长截断 / 开关 / 布局 / View 集成）
// ---------------------------------------------------------------------------

// runTodosTest 断言 M5b 钉面板的渲染、开关、高度让位与 /todos 拦截。
func runTodosTest() {
	failed := false
	check := func(name string, cond bool) {
		tag := "PASS"
		if !cond {
			tag = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", tag, name)
	}

	sample := []TodoEntry{
		{Content: "读取 demo-lab 的目录结构", Status: "completed"},
		{Content: "把 README.md 的第一行改成标题（这条故意写得很长，用来验证宽度裁剪不会超预算）", Status: "in_progress"},
		{Content: "运行 npm test 验证", Status: "pending"},
		{Content: "提交改动并总结", Status: "pending"},
	}

	// ① 渲染：# Todos · 计数 + ○/◐/✓ 标记 + 宽度合规 + 无背景色
	m := model{width: 100, feed: NewFeed(), status: stIdle, todosOn: true, todos: sample}
	m.feed.SetSize(m.width-6, 20)
	lines := m.renderTodosPinned(m.feed.width)
	joined := stripANSI(strings.Join(lines, "\n"))
	okHead := len(lines) == 5 && strings.Contains(stripANSI(lines[0]), "# Todos \u00B7 1/4")
	okMarks := strings.Contains(joined, "\u2713") && strings.Contains(joined, "\u25D0") && strings.Contains(joined, "\u25CB")
	okW, okBG := true, true
	for _, ln := range lines {
		if lipgloss.Width(ln) > m.feed.width {
			okW = false
		}
		if hasBackgroundColor(ln) {
			okBG = false
		}
	}
	fmt.Println("== 钉面板样张（宽 100）==")
	for _, ln := range lines {
		fmt.Printf("  %s\n", stripANSI(ln))
	}
	check("渲染：# Todos · 1/4 + ○/◐/✓ 标记 / 宽度合规 / 无背景色", okHead && okMarks && okW && okBG)

	// ② 超长列表：最多 6 条 + 「… 还有 N 条」
	many := make([]TodoEntry, 10)
	for i := range many {
		many[i] = TodoEntry{Content: fmt.Sprintf("待办第 %d 条", i+1), Status: "pending"}
	}
	m.todos = many
	lines = m.renderTodosPinned(m.feed.width)
	okCap := len(lines) == 1+todosMaxRows+1 && strings.Contains(stripANSI(lines[len(lines)-1]), "还有 4 条")
	check("超长列表：最多 6 条 + 末行「… 还有 4 条」", okCap)

	// ③ /todos 开关：收起/展开；不发引擎、不新开回合
	m.todos = sample
	m.input.SetText("/todos")
	next, cmd := m.submit()
	m = next.(model)
	okOff := !m.todosOn && len(m.renderTodosPinned(m.feed.width)) == 0 && len(m.sent) == 0 && cmd == nil && !m.busy
	m.input.SetText("/todos")
	next, _ = m.submit()
	m = next.(model)
	okOn := m.todosOn && len(m.renderTodosPinned(m.feed.width)) == 5
	check("/todos：收起/展开；不发引擎（sent 为空）、不新开回合", okOff && okOn)

	// ④ 布局：钉面板占几行、消息区就让几行；View 行数不变且钉面板在消息区顶部
	vm := model{
		width: 120, height: 30, feed: NewFeed(), status: stIdle, panelOn: true, todosOn: true, todos: sample,
		sessTitle: "钉面板集成样张", modelLabel: "Step 3.7 Flash", modeID: "default",
	}
	vm.syncLayout()
	pinnedH := len(vm.renderTodosPinned(vm.feed.width))
	hWith := vm.feed.height
	vm.todosOn = false
	vm.syncLayout()
	hWithout := vm.feed.height
	check("布局：钉面板占几行、消息区就让几行", pinnedH == 5 && hWith+pinnedH == hWithout)

	vm.todosOn = true
	vm.syncLayout()
	content := vm.View().Content
	vlines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	okRows := len(vlines) == vm.height
	// 钉面板钉在消息区底部（2026-09-19 搬家：顶部 → 底部，照 Claude Code）：
	// 标题行紧跟在消息行之后（索引 = 顶部留白 1 行 + feed 高），之后还有输入区/状态栏
	hdr := -1
	for i, ln := range vlines {
		if strings.Contains(stripANSI(ln), "# Todos") {
			hdr = i
			break
		}
	}
	okBottom := hdr == 1+vm.feed.height && hdr+pinnedH <= len(vlines)
	okW2 := true
	for _, ln := range vlines {
		if lipgloss.Width(ln) > vm.width {
			okW2 = false
		}
	}
	check("View 集成：行数不变 / 钉面板在消息区底部（Claude Code 式）/ 宽度合规", okRows && okBottom && okW2)

	if failed {
		fmt.Println("todostest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("todostest: 全部通过")
}
