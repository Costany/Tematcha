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
