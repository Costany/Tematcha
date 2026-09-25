// ui_panel.go —— 右栏（M3b · §4：会话信息 / Context 用量）+ To-Do 卡片
//
// 布局：消息区右侧一条「  │ 」分隔列 + 固定宽度的信息栏；消息区宽度在
// syncLayout 里让位。窄屏（< minPanelWidth）自动隐藏，宽了恢复；ctrl+b
// 手动开关（见 main.handleKey）。
//
// M29 起待办从右栏搬走：右栏只留会话信息与 Context 用量条；待办变成
// 消息区底部的 To-Do 卡片（见本文件后半段的 M5b/M29 段，ctrl+t 收起）。
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

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	panelW        = 28  // 右栏逻辑宽度（含最右 1 列滚动条）
	panelContentW = 27  // 右栏内容宽度（panelW - 滚动条列）
	panelSepW     = 4   // 分隔列「  │ 」的宽度
	minPanelWidth = 100 // 低于此宽度自动隐藏右栏（宽了恢复）

	// brandName 右栏顶部品牌行文字（2026-09-19 用户点名：从 letcode 一个词
	// 改成 "Letcode - Tematcha"，配色走品牌渐变）。
	brandName = "Letcode - Tematcha"
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

// todoMark 待办状态字形与配色（照 Crush 截图）：✓ 完成（绿）/ ◐ 进行中 / ○ 待办。
func todoMark(status string) (string, lipgloss.Style) {
	switch status {
	case "in_progress":
		return "◐", thinkStyle
	case "completed":
		return "✓", toolOKStyle
	default:
		return "○", dimStyle
	}
}

// todoText 待办正文的配色：已完成的降为暗色（办完的别抢眼），其余走正文色。
func todoText(status string) lipgloss.Style {
	if status == "completed" {
		return dimStyle
	}
	return textStyle
}

// currentTodoContent 当前进行中那条待办的文字（卡片收起时接在标题行后面；
// 没有进行中的就返回空串——不猜、不拿已完成的第一条顶替）。
func (m model) currentTodoContent() string {
	for _, t := range m.todos {
		if t.Status == "in_progress" {
			return t.Content
		}
	}
	return ""
}

// panelRule 右栏分区标题行：标签 + 横线补满到 panelContentW（如「上下文 ─────」）。
// 横线用制表符 U+2500（lipgloss/终端通用的细横线），颜色走 ruleStyle。
func panelRule(label string) string {
	fill := panelContentW - lipgloss.Width(label) - 1
	if fill < 0 {
		fill = 0
	}
	return panelLabelStyle.Render(label) + " " + ruleStyle.Render(strings.Repeat("─", fill))
}

// renderContextMeter 右栏 Context 段的用量进度条：整宽一条，█ = 已占用、
// ░ = 剩余（与状态栏的 miniBar 同语义，只是放大到 panelContentW）。
// 填充色沿用 §3 的同一套阈值：<60% 柔绿、≥60% 琥珀、≥85% 柔红。
// 百分比取 shownPct（压缩后缓动显示，动画中 = barPct）。
func (m model) renderContextMeter() string {
	w := panelContentW
	pct := m.shownPct()
	fill := pct * w / 100
	if fill > w {
		fill = w
	}
	if fill < 0 {
		fill = 0
	}
	st := toolOKStyle
	switch {
	case pct >= 85:
		st = toolErStyle
	case pct >= 60:
		st = warnStyle
	}
	var b strings.Builder
	b.WriteString(st.Render(strings.Repeat("█", fill)))
	b.WriteString(ruleStyle.Render(strings.Repeat("░", w-fill)))
	return b.String()
}

// panelScrollbar 右栏竖向滚动条列（M17）
// （照 crush internal/ui/common/scrollbar.go），拇指 = Theme.ScrollThumb。
//
// off = 距顶行数（0 = 贴顶，右栏信息面板默认贴顶——品牌行/目录/标题最常看）。
// 不溢出时整列留白（与消息区一致：右栏不因条的出现/消失而横移）。
func panelScrollbar(all []string, height, off int) []string {
	content := len(all)
	out := make([]string, height)
	if content <= height {
		return out
	}
	thumb := height * height / content
	if thumb < 1 {
		thumb = 1
	}
	maxOff := content - height
	if off < 0 {
		off = 0
	}
	if off > maxOff {
		off = maxOff
	}
	track := height - thumb
	pos := 0
	if track > 0 {
		pos = off * track / maxOff
	}
	for i := 0; i < height; i++ {
		if i >= pos && i < pos+thumb {
			out[i] = scrollThumbStyle.Render("┃")
		} else {
			out[i] = scrollTrackStyle.Render("│")
		}
	}
	return out
}

// panelScrollMax 右栏可滚动的最大距顶行数（0 = 不溢出）。
func panelScrollMax(all []string, height int) int {
	if len(all) <= height {
		return 0
	}
	return len(all) - height
}

// workspacePath 当前工作目录的**完整路径**（右栏「文件目录」行显示用）。
// 2026-09-19 用户点名要路径不是 basename；超宽部分由调用方 wrapText 折行。
// 分隔符统一成 Windows 反斜杠（workspace 常量写的是反斜杠，容错正斜杠写法）。
func workspacePath() string {
	if workspace == "" {
		return ""
	}
	return strings.ReplaceAll(workspace, "/", "\\")
}

// renderPanel 渲染右栏内容（品牌 / 会话标题 / 标识·模型·模式 / 上下文 / 待办），
// 行数固定为 height。
func (m model) renderPanel(height int) []string {
	var out []string

	// ⓿ 品牌行（2026-09-19 用户点名：letcode 字样从底部状态栏左上挪到右栏
	// 上方，"好像 crush 一样"）——品牌一行 + 一行呼吸空白。
	// 2026-09-19 二次点菜：文字改 "Letcode - Tematcha"，配色走品牌渐变
	// （逐字符取色，§11.5；渐变锚点见 theme.go 的 BrandA/BrandC）。
	out = append(out, brandText(brandName))
	out = append(out, "")

	// ① 文件目录（2026-09-19 用户点菜：插在品牌行与项目标题之间，灰色）
	//   2026-09-19 二次点菜：要**完整路径**（不是 basename）——超出 panelContentW
	//   的部分靠折行显示全（右栏另有竖向滚动条兜底行数溢出）。
	if d := workspacePath(); d != "" {
		for _, ln := range wrapText(d, panelContentW) {
			out = append(out, dimStyle.Render(ln))
		}
		out = append(out, "")
	}

	// ② 会话标题（单独一块）
	// 2026-09-19 用户点菜：原来标题和 session id 都叫"会话"，右栏一眼重名——
	// 标题上提成独立一块（与下方字段隔一行），数字 id 改名"标识"。
	if m.sessTitle != "" {
		for _, ln := range wrapText(m.sessTitle, panelContentW) {
			out = append(out, textStyle.Render(ln))
		}
	} else {
		out = append(out, dimStyle.Render("（未命名）"))
	}
	out = append(out, "")

	// ③ 字段行：标识 / 模型 / 模式
	// 标签 = PanelLabel（2026-09-19 用户点菜：紫 → 改回灰）；值 = 加粗 + 三色
	// （体绿 / 鞋橙 / 鞍红，见 theme.go 的 PanelVal* token）。
	if sid := m.sessionID; sid != "" {
		if len(sid) > 8 {
			sid = sid[:8]
		}
		out = append(out, panelLabelStyle.Render("标识 ")+panelValIDStyle.Render(sid))
	}
	if m.modelLabel != "" {
		out = append(out, panelLabelStyle.Render("模型 ")+panelValModelStyle.Render(clipWidth(m.modelLabel, panelContentW-7)))
	}
	if m.modeID != "" {
		out = append(out, panelLabelStyle.Render("模式 ")+panelValModeStyle.Render(m.modeID))
	}
	out = append(out, "")

	// ④ 上下文（2026-09-19 恒显示；2026-09-25 用户点菜：标题改英文 Context
	//   并加一条真实用量进度条）。
	//
	//   取证（为什么只有一条总量条、没有"工具/消息/提示词"的分段色）：ACP 的
	//   usage_update 只有 used / size 两个数（letcode acp/projection.rs:96-103
	//   的 TokenUsageEvent 投影）；引擎内部那套分类构成（system / skills /
	//   context / messages / tools 的 prompt_composition，见
	//   request_builder/prompt_plan.rs:463-470）在投影层被整个丢掉，标准协议里
	//   没有它的容身处。拿本地字符数冒充引擎的分类只会得到假账——所以这里只画
	//   真值：已用 / 窗口。要真分段得先扩引擎的 ACP 扩展，那是另一件事。
	//
	//   无数据时的占位文案：引擎只在"有 token 活动"后才广播 usage
	//   （session/new 与 session/load 的应答结构体都没有 usage 字段，
	//   projection.rs 的 UsageUpdate 只由 TokenUsage/SessionTokenUsage 触发），
	//   所以新建或 /resume 载入后、首次对话前必然拿不到——说清楚比画个
	//   破折号友好。横线 = 标签 + U+2500 补满 panelContentW。
	out = append(out, panelRule("Context"))
	if m.usageSize > 0 {
		out = append(out, m.renderContextMeter())
		rest := m.usageSize - m.usageUsed
		if rest < 0 {
			rest = 0
		}
		out = append(out, dimStyle.Render(clipWidth(
			fmtK(m.usageUsed)+" / "+fmtK(m.usageSize)+" · left "+fmtK(rest), panelContentW)))
	} else {
		out = append(out, dimStyle.Render("after first turn"))
	}
	out = append(out, "")

	// ⑤ 能力清单：LSPs / MCPs / Skills（2026-09-19 用户点菜）
	//   格式 = 横线行 + **下一行**显示 None（用户截图纠正：None 不跟横线同行）。
	//   ACP 侧暂无这三个数据源，先占位——将来有源只改这一处。
	for _, k := range []string{"LSPs", "MCPs", "Skills"} {
		out = append(out, panelRule(k))
		out = append(out, dimStyle.Render("None"))
		out = append(out, "")
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
		if hasStrayBackground(ln) {
			bad = true
		}
		fmt.Printf("%s  (w=%d)\n", stripANSI(ln), w)
	}

	fmt.Println()
	fmt.Println("== Context 用量条（M29：英文标题 + 真实 used/size 进度条）==")
	okMeter := true
	for _, c := range []struct {
		used, size int64
	}{{87300, 320000}, {200000, 320000}, {300000, 320000}} {
		cm := model{usageUsed: c.used, usageSize: c.size}
		meter := stripANSI(cm.renderContextMeter())
		w := lipgloss.Width(meter)
		if w != panelContentW {
			okMeter = false
		}
		fmt.Printf("  %6d / %6d  %s  (w=%d)\n", c.used, c.size, meter, w)
	}
	if !okMeter {
		bad = true
		fmt.Println("  !! 进度条宽度不等于 panelContentW")
	}

	fmt.Println()
	fmt.Println("== 输入区（M29 ① 单底线 ②③ 提示符两态）==")
	fm := model{width: 100, feed: NewFeed(), status: stIdle, inputFocused: true}
	fm.feed.SetSize(60, 10)
	ibOn := strings.Split(fm.renderInputBlock(), "\n")
	fm.inputFocused = false
	ibOff := strings.Split(fm.renderInputBlock(), "\n")
	// 两态各两行（输入 + 底线）、每行等宽、前缀恒 promptW 格、单底线（末行是 ─）
	// 注意输入块整行带 2 格左缩进，比对提示符前要先剥掉
	onHead := strings.TrimPrefix(stripANSI(ibOn[0]), "  ")
	offHead := strings.TrimPrefix(stripANSI(ibOff[0]), "  ")
	okPrompt := len(ibOn) == 2 && len(ibOff) == 2 &&
		lipgloss.Width(ibOn[1]) == lipgloss.Width(ibOn[0]) &&
		lipgloss.Width(ibOff[1]) == lipgloss.Width(ibOff[0]) &&
		strings.HasPrefix(onHead, promptFocused) && strings.HasPrefix(offHead, promptBlurred) &&
		lipgloss.Width(promptFocused) == promptW && lipgloss.Width(promptBlurred) == promptW &&
		strings.Count(ibOn[1], "─") == fm.blockWidth() &&
		!strings.Contains(onHead, "─")
	for _, ln := range []string{onHead, offHead, strings.TrimPrefix(stripANSI(ibOn[1]), "  ")} {
		fmt.Printf("  |%s|\n", ln)
	}
	if !okPrompt {
		bad = true
		fmt.Println("  !! 提示符两态 / 单底线 / 宽度守恒 断言失败")
	} else {
		fmt.Println("  OK 两行（输入 + 底线）、前缀恒 4 格、聚焦 ❯ / 失焦 :::")
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
	okSep := strings.Contains(plain, "│")
	// 2026-09-19 右栏改版：标题上提成独立块、数字 id 改名"标识"、字段标签
	// （标识/模型/模式）走 PanelLabel 色。断言跟着改。
	okPanel := strings.Contains(plain, brandName) &&
		strings.Contains(plain, "集成样张") &&
		strings.Contains(plain, "标识 ") &&
		strings.Contains(plain, "模型 ") &&
		strings.Contains(plain, "模式 ") &&
		// M29：右栏标题改英文 Context，中文「上下文」不该再出现
		strings.Contains(plain, "Context") && !strings.Contains(plain, "上下文")
	okWidth := true
	for _, ln := range lines {
		if lipgloss.Width(ln) > vm.width {
			okWidth = false
			break
		}
	}
	// M20（2026-09-20 用户点菜「输入框不要沾满整个终端的宽度，右边的侧边栏要
	// 截断他」）：① 输入区两行（输入行 + 单底线，M29 后）各自 = 左列宽（消息区宽 +
	// 滚动条列），不再沾满终端；② 组成后的输入行带右栏分隔列（侧栏一路延伸到底）
	// 且不满宽；③ 状态栏/提示行（最后两行）才满宽（照 crush 的 help 行）。
	ibLines := strings.Split(vm.renderInputBlock(), "\n")
	okIB := len(ibLines) == 2
	for _, ln := range ibLines {
		if lipgloss.Width(ln) != vm.feed.width+scrollBarW {
			okIB = false
		}
	}
	okSep20, okFull := false, false
	for _, ln := range lines {
		p := stripANSI(ln)
		if strings.Contains(p, "输入消息") {
			okSep20 = strings.Contains(p, "│") && lipgloss.Width(ln) < vm.width
			break
		}
	}
	if len(lines) >= 2 {
		okFull = lipgloss.Width(lines[len(lines)-1]) == vm.width &&
			lipgloss.Width(lines[len(lines)-2]) == vm.width
	}
	okCut := okIB && okSep20 && okFull && vm.blockWidth() == vm.feed.width+scrollBarW-2
	if !okRows || !okSep || !okPanel || !okWidth || !okCut {
		bad = true
	}
	fmt.Println()
	fmt.Println("== View() 集成抽查 ==")
	fmt.Printf("  行数 %d/%d：%v ｜ 分隔列：%v ｜ 右栏内容：%v ｜ 宽度≤%d：%v ｜ 输入框被截断：%v\n",
		len(lines), vm.height, okRows, okSep, okPanel, vm.width, okWidth, okCut)
	// 底部样张（人眼核对：输入框被右栏截断、侧栏一路延伸到底、状态/提示行满宽）
	fmt.Println("  底部样张（剥色）：")
	for _, ln := range lines[len(lines)-8:] {
		fmt.Printf("    |%s|\n", stripANSI(ln))
	}
	if hasStrayBackground(content) {
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
// M5b / M29：To-Do 卡片（消息区底部、输入区之上；照 Crush 的 To-Do pill）
// ---------------------------------------------------------------------------

// todosMaxRows 卡片里最多列几条待办（超出的折成一行「… N more」）。
const todosMaxRows = 6

// isTodosToggle 输入是否正好是 /todos（客户端命令：整张卡片的显隐，不发引擎）。
// 注意：letcode 的命令表里没有 /todos，不拦的话它会被当成普通提问发给模型。
func isTodosToggle(text string) bool {
	return strings.TrimSpace(text) == "/todos"
}

// todosVisible 卡片是否显示：开关开着且快照里确实有待办。
func (m model) todosVisible() bool {
	return m.todosOn && len(m.todos) > 0
}

// toggleTodosExpanded 收起 / 展开待办列表（M29；ctrl+t 走它）。
// 快照为空时不动 —— 没有东西可展开，别让一个空壳卡片占掉输入区的位置。
func (m *model) toggleTodosExpanded() {
	if len(m.todos) == 0 {
		return
	}
	m.todosOpen = !m.todosOpen
	m.syncLayout() // 列表行数变了 → 消息区高度要重算
}

// todoCardRow 把一行内容包进左右边框，内容区两侧各留 1 格内边距（照 Crush 的
// Padding(0,1)）。contentW = 卡片总宽 - 3（两个边框列 + 左侧内边距）；不足的
// 宽度补到右内边距上，于是每一行的右竖线都落在同一列。
func todoCardRow(content string, contentW int) string {
	pad := contentW - lipgloss.Width(content)
	if pad < 0 {
		pad = 0
	}
	return ruleStyle.Render("│") + " " + content + strings.Repeat(" ", pad) + ruleStyle.Render("│")
}

// renderTodosPinned 渲染 To-Do 卡片（钉在消息区底部，不随消息区滚动消失），
// 照 Crush 的 To-Do pill：圆角边框的标题行 + 框内的任务列表。
//
//	╭── To-Do  1/4                       ctrl+t close ──╮
//	│  ✓ 读取 demo-lab 的目录结构                        │
//	│  ◐ 把 README.md 的第一行改成标题                    │
//	╰────────────────────────────────────────────────────╯
//
// 状态机分两层，照 Crush 的 pills：todosOn 管整张卡片在不在（/todos 切换）；
// todosOpen 管列表展不展开（ctrl+t 切换）——收起时只剩标题行一行，并把当前
// 进行中的任务接在计数后面（Crush 收起态就是标题行后面接当前任务）。
// 透明度原则：只前景色、无背景色；边框走 ruleStyle。
func (m model) renderTodosPinned(w int) []string {
	if !m.todosVisible() {
		return nil
	}
	if w < 24 {
		w = 24
	}
	// contentW = 卡片总宽 - 3（两个边框列 + 左内边距 1 格）
	contentW := w - 3
	if contentW < 8 {
		contentW = 8
	}
	done, total := m.panelDoneCount()

	// 标题行：左 = To-Do x/y（收起时再接当前任务），右 = ctrl+t 开关提示
	action := "ctrl+t close"
	head := fmt.Sprintf("To-Do  %d/%d", done, total)
	if !m.todosOpen {
		action = "ctrl+t open"
		if cur := m.currentTodoContent(); cur != "" {
			head += "  " + cur
		}
	}
	// 左段先裁到给右段留够位置（窄屏时牺牲左边，不动开关提示）
	budget := contentW - lipgloss.Width(action) - 2
	if budget < 8 {
		budget = 8
	}
	left := clipWidth(head, budget)
	// 留 1 格右内边距（-1），让「ctrl+t close」不贴着右边框
	gap := contentW - lipgloss.Width(left) - lipgloss.Width(action) - 1
	if gap < 1 {
		gap = 1
	}

	out := []string{ruleStyle.Render("╭" + strings.Repeat("─", w-2) + "╮")}
	out = append(out, todoCardRow(
		textStyle.Render(left)+strings.Repeat(" ", gap)+dimStyle.Render(action), contentW))

	if m.todosOpen {
		shown := m.todos
		overflow := 0
		if len(shown) > todosMaxRows {
			overflow = len(shown) - todosMaxRows
			shown = shown[:todosMaxRows]
		}
		for _, t := range shown {
			mark, st := todoMark(t.Status)
			out = append(out, todoCardRow(
				st.Render(mark)+" "+todoText(t.Status).Render(clipWidth(t.Content, contentW-2)), contentW))
		}
		if overflow > 0 {
			out = append(out, todoCardRow(dimStyle.Render(fmt.Sprintf("… %d more", overflow)), contentW))
		}
	}
	out = append(out, ruleStyle.Render("╰"+strings.Repeat("─", w-2)+"╯"))
	return out
}

// ---------------------------------------------------------------------------
// -todostest：To-Do 卡片自检（渲染 / ctrl+t 收起 / /todos 显隐 / 布局 / View 集成）
// ---------------------------------------------------------------------------

// runTodosTest 断言 To-Do 卡片的渲染、ctrl+t 收起、/todos 显隐与高度让位。
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

	// ① 展开态：圆角外框 + To-Do 1/4 + ctrl+t close + ○/◐/✓ + 宽度恒等 + 无背景色
	m := model{width: 100, feed: NewFeed(), status: stIdle,
		todosOn: true, todosOpen: true, todos: sample}
	m.feed.SetSize(m.width-6, 20)
	lines := m.renderTodosPinned(m.feed.width)
	joined := stripANSI(strings.Join(lines, "\n"))
	want := 2 + len(sample) + 1 // 上下边框 + 标题行 + 列表
	okFrame := strings.HasPrefix(joined, "╭") &&
		strings.HasSuffix(stripANSI(lines[len(lines)-1]), "╯") &&
		strings.Contains(joined, "│")
	okHead := len(lines) == want && strings.Contains(joined, "To-Do  1/4") &&
		strings.Contains(joined, "ctrl+t close") && !strings.Contains(joined, "#")
	okMarks := strings.Contains(joined, "✓") && strings.Contains(joined, "◐") && strings.Contains(joined, "○")
	okW, okBG := true, true
	for _, ln := range lines {
		if lipgloss.Width(ln) != m.feed.width {
			okW = false
		}
		if hasStrayBackground(ln) {
			okBG = false
		}
	}
	fmt.Println("== To-Do 卡片样张（展开，宽 100）==")
	for _, ln := range lines {
		fmt.Printf("  %s\n", stripANSI(ln))
	}
	check("展开：圆角外框 / To-Do 1/4 / ctrl+t close / 无 # / 宽度恒等 / 无背景色", okFrame && okHead && okMarks && okW && okBG)

	// ② 超长列表：最多 6 条 + 末行「… 4 more」
	many := make([]TodoEntry, 10)
	for i := range many {
		many[i] = TodoEntry{Content: fmt.Sprintf("待办第 %d 条", i+1), Status: "pending"}
	}
	m.todos = many
	lines = m.renderTodosPinned(m.feed.width)
	okCap := len(lines) == 2+todosMaxRows+2 &&
		strings.Contains(stripANSI(lines[len(lines)-2]), "… 4 more")
	check("超长列表：最多 6 条 + 倒数第二行「… 4 more」", okCap)

	// ③ ctrl+t 收起：只剩标题行，提示变 open，并把当前进行中的任务接在计数后
	m.todos, m.todosOpen = sample, true
	nm, _ := m.handleKey(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	cm := nm.(model)
	closed := stripANSI(strings.Join(cm.renderTodosPinned(cm.feed.width), "\n"))
	okClose := !cm.todosOpen && len(cm.renderTodosPinned(cm.feed.width)) == 3 &&
		strings.Contains(closed, "ctrl+t open") && strings.Contains(closed, "To-Do  1/4") &&
		strings.Contains(closed, "把 README.md") && !strings.Contains(closed, "读取 demo-lab")
	fmt.Println("== ctrl+t 收起后 ==")
	for _, ln := range cm.renderTodosPinned(cm.feed.width) {
		fmt.Printf("  %s\n", stripANSI(ln))
	}
	// 再按一次 ctrl+t 回到展开
	nm2, _ := cm.handleKey(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	cm2 := nm2.(model)
	okReopen := cm2.todosOpen && len(cm2.renderTodosPinned(cm2.feed.width)) == 2+len(sample)+1
	check("ctrl+t：收起（只剩标题行 + 当前任务 + open）/ 再按回展开", okClose && okReopen)

	// ④ /todos 显隐：整张卡片开/关；不发引擎、不新开回合
	m.todos, m.todosOpen = sample, true
	m.input.SetText("/todos")
	next, cmd := m.submit()
	m = next.(model)
	okOff := !m.todosOn && len(m.renderTodosPinned(m.feed.width)) == 0 && len(m.sent) == 0 && cmd == nil && !m.busy
	m.input.SetText("/todos")
	next, _ = m.submit()
	m = next.(model)
	okOn := m.todosOn && len(m.renderTodosPinned(m.feed.width)) == 2+len(sample)+1
	check("/todos：整卡收起/展开；不发引擎（sent 为空）、不新开回合", okOff && okOn)

	// ⑤ 布局：卡片占几行、消息区就让几行；View 行数不变且卡片在消息区底部
	vm := model{
		width: 120, height: 30, feed: NewFeed(), status: stIdle, panelOn: true,
		todosOn: true, todosOpen: true, todos: sample,
		sessTitle: "To-Do 卡片集成样张", modelLabel: "Step 3.7 Flash", modeID: "default",
	}
	vm.syncLayout()
	pinnedH := len(vm.renderTodosPinned(vm.feed.width))
	hWith := vm.feed.height
	vm.todosOn = false
	vm.syncLayout()
	hWithout := vm.feed.height
	okLayout := hWith+pinnedH == hWithout

	vm.todosOn = true
	vm.syncLayout()
	content := vm.View().Content
	vlines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	okRows := len(vlines) == vm.height
	// 卡片钉在消息区底部（2026-09-19 搬家：顶部 → 底部，照 Claude Code）：
	// 卡片第 0 行是圆角上边框、第 1 行才是带 To-Do 计数的标题行，所以
	// 标题行索引 = 顶部留白 1 行 + feed 高 + 边框 1 行
	hdr := -1
	for i, ln := range vlines {
		if strings.Contains(stripANSI(ln), "To-Do  1/4") {
			hdr = i
			break
		}
	}
	okBottom := hdr == 1+vm.feed.height+1 && hdr+pinnedH <= len(vlines)
	okW2 := true
	for _, ln := range vlines {
		if lipgloss.Width(ln) > vm.width {
			okW2 = false
		}
	}
	check("布局：卡片占几行、消息区就让几行", okLayout)
	check("View 集成：行数不变 / 卡片在消息区底部（Claude Code 式）/ 宽度合规", okRows && okBottom && okW2)

	if failed {
		fmt.Println("todostest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("todostest: 全部通过")
}
