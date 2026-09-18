// theme.go —— 主题系统（M5c · §11 / §11.5）：色板集中 + 可切换。
//
// 设计要点：
//   - 全部前景色集中到 Theme 结构（§0.5：只前景色、禁止大块底色）；
//   - 内置主题：sprout（默认 · 柔绿渐变）/ mono（灰度 · 无彩色）；
//   - applyTheme 重建所有样式变量（ui_feed.go 的 var 块）并作废 glamour
//     渲染器缓存（mdRenderers 里烤着旧颜色，不清会拿旧色渲染）；
//   - 柔绿渐变（§11.5）：上下文条填充格按 A → B → C 取值
//     （lipgloss.Blend1D，CIELAB 混合，避免中间发灰）；
//   - /theme 是客户端命令（引擎命令表里没有它）：/theme 列清单、
//     /theme <名字> 切换，见 main.submit 的拦截。
package main

import (
	"fmt"
	"image/color"
	"os"
	"regexp"
	"strings"
	"time"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
)

// Theme 是一套完整色板（所有值都是前景色 hex）。
type Theme struct {
	Name string // 唯一名（/theme 用）
	Desc string // 一句话说明（清单/回执用）

	// 文字层级
	Text   string // 正文
	TextHi string // 亮文字（输入文字、模型名、追问选中项）
	Faint  string // 中间层（思考正文、工具输出）
	Dim    string // 次要信息
	Strong string // markdown 粗体

	// 消息区元素
	Tool       string // 工具行·进行中
	ThoughtPre string // 思考前缀
	ThoughtTtl string // 思考标题

	// markdown 元素（M6 全量主题化：标题 / 链接 / 斜体）
	Heading string
	Link    string
	Emph    string

	// 线框与容器
	Rule        string // 细线（输入区框线、进度条空槽）
	Placeholder string // 占位提示
	Track       string // 滚动条轨道

	// 强调与状态
	Accent    string // 小面积强调绿（用户竖条 / 行内代码 / 滚动条拇指 / 回执）
	OK        string // 成功（工具柔绿）
	Err       string // 失败（工具柔红）
	ErrSoft   string // 错误块（柔红 + bold）
	Warn      string // 提醒（思考中 / 排队 / 需要批准）
	Reply     string // 回复中
	PromptOff string // 输入提示符（空输入）
	PromptOn  string // 输入提示符（打字后）

	// 柔绿渐变锚点（§11.5：A 青柠黄绿 → B 草绿 → C 深植绿）
	GradA, GradB, GradC string
}

// sproutTheme 内置默认主题：柔绿渐变（§11.5 定调）。
// 色值取自已实装验收的现状（M1.7 起的强调绿 + M1.6 起的工具状态色），
// 默认主题必须保持与既有截图一致 —— 改这里的值等于改整套观感。
var sproutTheme = Theme{
	Name: "sprout", Desc: "柔绿渐变（默认）",

	Text: "#D0D0D0", TextHi: "#E6E6E6", Faint: "#8F8F8F", Dim: "#6E6E6E", Strong: "#FFFFFF",
	Tool: "#9A9A9A", ThoughtPre: "#8A8A8A", ThoughtTtl: "#B4B4B4",
	Rule: "#464646", Placeholder: "#565656", Track: "#3C3C3C",
	Accent: "#00E7A4", OK: "#79C77D", Err: "#C97B7B", ErrSoft: "#D98A8A",
	Warn: "#FFD166", Reply: "#6FB6F0", PromptOff: "#4A4A4A", PromptOn: "#3CCF7E",
	Heading: "#E6E6E6", Link: "#6FB6F0", Emph: "#B8B8B8",

	GradA: "#C4F07E", GradB: "#79C77D", GradC: "#4C875F",
}

// monoTheme 灰度主题：全部无色相（R=G=B）。
// 两个用途：无彩色/色弱终端的可用性选项；以及自检的"硬编码探测器"
// （切成 mono 后样张里若还出现彩色 SGR，说明有渲染点没走主题）。
// 注意：markdown 内部样式（标题/链接/chroma）暂仍随 glamour dark，见 §11 说明。
var monoTheme = Theme{
	Name: "mono", Desc: "灰度（界面无彩色）",

	Text: "#D0D0D0", TextHi: "#F2F2F2", Faint: "#969696", Dim: "#6E6E6E", Strong: "#FFFFFF",
	Tool: "#9A9A9A", ThoughtPre: "#8A8A8A", ThoughtTtl: "#B4B4B4",
	Rule: "#464646", Placeholder: "#565656", Track: "#3C3C3C",
	Accent: "#E8E8E8", OK: "#C8C8C8", Err: "#A8A8A8", ErrSoft: "#D8D8D8",
	Warn: "#DDDDDD", Reply: "#C4C4C4", PromptOff: "#4A4A4A", PromptOn: "#D0D0D0",
	Heading: "#F2F2F2", Link: "#C4C4C4", Emph: "#B0B0B0",

	GradA: "#F0F0F0", GradB: "#C8C8C8", GradC: "#8C8C8C",
}

var (
	// themeOrder 决定清单顺序（第一个是默认主题）。
	themeOrder  = []string{"sprout", "mono"}
	activeTheme = sproutTheme

	// themeEpoch 每次 applyTheme 递增：消息条的渲染缓存把它算进有效性
	// —— 缓存里烤着颜色，换主题必须让旧缓存作废（见 ui_feed.go 的 itemLines）。
	themeEpoch uint64
)

// themeByName 按名字取主题。
func themeByName(name string) (Theme, bool) {
	for _, n := range themeOrder {
		if n == name {
			switch name {
			case "sprout":
				return sproutTheme, true
			case "mono":
				return monoTheme, true
			}
		}
	}
	return Theme{}, false
}

// themeListLine 主题清单（当前项前标 *），/theme 的回执用。
func themeListLine() string {
	parts := make([]string, 0, len(themeOrder))
	for _, n := range themeOrder {
		t, _ := themeByName(n)
		mark := " "
		if n == activeTheme.Name {
			mark = "*"
		}
		parts = append(parts, mark+n+"（"+t.Desc+"）")
	}
	return "主题：" + strings.Join(parts, "  ") + " —— /theme <名字> 切换"
}

// applyTheme 应用主题：重建全部样式变量 + 作废 markdown 渲染器缓存。
// 样式变量是包级的（ui_feed.go 声明），渲染点全部引用它们，所以这里一改，
// 下一次 View 即生效。
func applyTheme(t Theme) {
	activeTheme = t

	// 消息区
	textStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Text))
	userBarStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Accent))
	thoughtPre = lipgloss.NewStyle().Foreground(lipgloss.Color(t.ThoughtPre))
	thoughtTtl = lipgloss.NewStyle().Foreground(lipgloss.Color(t.ThoughtTtl))
	thoughtTxt = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Faint))
	toolStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Tool))
	toolOKStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.OK))
	toolErStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Err))
	toolOutStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Faint))
	dimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Dim))
	errStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.ErrSoft)).Bold(true)

	// 输入区 / 状态栏
	ruleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Rule))
	placeholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Placeholder))
	inputTextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.TextHi))
	modelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.TextHi)).Bold(true)
	thinkStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Warn))
	replyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Reply))
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Warn))
	promptOffStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.PromptOff))
	promptOnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.PromptOn))

	// 滚动条（M4a）
	scrollThumbStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Accent))
	scrollTrackStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Track))
	// glamour 渲染器必须重建：每台的样式表是构造时烤进去的（md.go 的
	// transparentDarkStyle 读 activeTheme 取 Code/Strong 色）。
	mdRenderers = map[int]*glamour.TermRenderer{}

	// 消息条的渲染缓存同理：递增代次让旧缓存全部失效（ui_feed.go itemLines）。
	themeEpoch++
}

func init() {
	// 包加载即应用默认主题；所有样式变量在此之前为零值。
	applyTheme(sproutTheme)
}

// ---------------------------------------------------------------------------
// 柔绿渐变（§11.5）
// ---------------------------------------------------------------------------

var (
	gradCacheName string
	gradCache     []color.Color
)

// contextBarGrad 取 n 格上下文条渐变（A → B → C，CIELAB 混合）。
// 结果按主题名缓存：切换主题后第一次取会重算。
func contextBarGrad(n int) []color.Color {
	if gradCacheName == activeTheme.Name && len(gradCache) == n {
		return gradCache
	}
	gradCache = lipgloss.Blend1D(n,
		lipgloss.Color(activeTheme.GradA),
		lipgloss.Color(activeTheme.GradB),
		lipgloss.Color(activeTheme.GradC))
	gradCacheName = activeTheme.Name
	return gradCache
}

// ---------------------------------------------------------------------------
// /theme 客户端命令
// ---------------------------------------------------------------------------

// parseThemeCmd 解析 /theme 命令：isCmd=false 表示这不是 /theme（前缀相似
// 的 /themed 之类不误伤）；name 为空 = 只列出清单。
func parseThemeCmd(text string) (name string, isCmd bool) {
	s := strings.TrimSpace(text)
	if s != "/theme" && !strings.HasPrefix(s, "/theme ") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(s, "/theme")), true
}

// ---------------------------------------------------------------------------
// -themetest：主题系统自检（注册表 / 命令 / 渐变 / 单色探测 / 缓存作废）
// ---------------------------------------------------------------------------

// rgbTriple 把 color.Color 转成 "r;g;b"（与 SGR 输出同构，便于断言）。
func rgbTriple(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("%d;%d;%d", r>>8, g>>8, b>>8)
}

// rgbRe 抓 truecolor 前景色/背景色序列（38;2 / 48;2）。
var rgbRe = regexp.MustCompile(`(?:38|48);2;(\d+);(\d+);(\d+)`)

// hasNonGrayColor 样张里是否出现彩色（R≠G 或 G≠B）。
func hasNonGrayColor(s string) bool {
	for _, m := range rgbRe.FindAllStringSubmatch(s, -1) {
		if m[1] != m[2] || m[2] != m[3] {
			return true
		}
	}
	return false
}

// themeSample 渲染一份覆盖主要组件的样张（单色断言 + 颜色存在性断言用）：
// 状态栏（含模式徽章/上下文条/模型段）+ 输入区两态 + 消息区全条目 + 右栏。
// 刻意不含 markdown（glamour 内部配色暂不随主题，见 §11）。
func themeSample() string {
	var b strings.Builder
	m := model{
		width: 110, height: 30, feed: NewFeed(), status: stReplying, busy: true, blinkN: 1,
		modeID: "default", usageUsed: 27000, usageSize: 100000,
		modelLabel: "Step 3.7 Flash", reasoning: "high",
		sessTitle: "主题样张", panelOn: true,
		todos: []TodoEntry{{Content: "某条待办", Status: "in_progress"}},
	}
	m.syncLayout()

	b.WriteString(m.renderStatusBar() + "\n")
	b.WriteString(m.renderInputBlock() + "\n")
	m.input.SetText("打字中的输入")
	b.WriteString(m.renderInputBlock() + "\n")

	items := []*FeedItem{
		{Kind: kUser, Text: "看看主题"},
		{Kind: kThought, Text: "在想……", StartedAt: time.Now().Add(-2 * time.Second), EndedAt: time.Now()},
		{Kind: kTool, ToolName: "shell__exec", ToolCall: "npm test", ToolState: "completed", ToolEnd: "exit 0 · stdout 12 lines", ToolCmd: "npm test"},
		{Kind: kTool, ToolName: "shell__exec", ToolCall: "npm run lint", ToolState: "completed", ToolEnd: "exit 1 · stderr 3 lines", ToolCmd: "npm run lint", ToolBad: true},
		{Kind: kWarn, Text: "「/reasoning」还缺参数：safe|default|auto|yolo"},
		{Kind: kError, Text: "回合出错：参数不合法（-32602）", Detail: "提示: 命令的参数没写全"},
		{Kind: kQueued, Text: "排队的一条"},
		{Kind: kTurn, Text: turnSummaryLine("Step 3.7 Flash", "guji", 3*time.Second)},
	}
	for _, it := range items {
		b.WriteString(strings.Join(renderItem(it, m.feed.width), "\n") + "\n")
	}
	for _, ln := range m.renderPanel(12) {
		b.WriteString(ln + "\n")
	}
	// markdown（M6：标题/链接/斜体已接入主题。只放已主题化的元素——代码块/
	// 表格的 chroma 语法制导色仍随 glamour dark，见 §11 说明）
	if out, ok := renderMarkdown("# 标题\n\n一段 **粗体** *斜体* `代码` [链接](https://example.com)", 60); ok {
		b.WriteString(out + "\n")
	}

	// 滚动条（缩到 3 行制造溢出，让拇指/轨道都出场）
	sc := NewFeed()
	sc.SetSize(40, 3)
	sc.Append(kUser, "一行\n两行\n三行\n四行\n五行\n六行")
	b.WriteString(strings.Join(sc.Scrollbar(), "\n"))
	return b.String()
}

// runThemeTest 断言 M5c 主题系统：注册表 / /theme 命令 / 渐变几何 /
// mono 全灰（硬编码探测）/ sprout 颜色存在性 / markdown 缓存作废。
func runThemeTest() {
	failed := false
	check := func(name string, cond bool) {
		tag := "PASS"
		if !cond {
			tag = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", tag, name)
	}

	// ① 注册表与默认主题
	_, okSprout := themeByName("sprout")
	_, okMono := themeByName("mono")
	_, okNope := themeByName("nope")
	check("注册表：sprout / mono 在册、未知名字拒绝；默认 sprout",
		okSprout && okMono && !okNope && activeTheme.Name == "sprout")

	// ② /theme 解析：前缀相似不误伤
	_, okExact := parseThemeCmd("/theme")
	n1, okArg := parseThemeCmd("/theme  mono ")
	_, okBad := parseThemeCmd("/themed")
	_, okOther := parseThemeCmd("主题")
	check("解析：/theme 精确匹配、/theme <名字> 取参数（容忍空白）、/themed 不误伤",
		okExact && okArg && n1 == "mono" && !okBad && !okOther)

	// ③ /theme 命令全链路：列出 → 切换 → 未知名字 → 切回；不发引擎、不进历史
	m := model{width: 100, feed: NewFeed(), status: stIdle}
	m.feed.SetSize(94, 20)

	lastText := func(m model) string {
		if it := m.feed.Last(); it != nil {
			return it.Text
		}
		return ""
	}
	lastKind := func(m model) int {
		if it := m.feed.Last(); it != nil {
			return it.Kind
		}
		return -1
	}

	m.input.SetText("/theme")
	next, cmd := m.submit()
	m = next.(model)
	okList := cmd == nil && !m.busy && len(m.sent) == 0 && m.input.Text() == "" &&
		m.feed.Len() == 1 && strings.Contains(lastText(m), "*sprout") && strings.Contains(lastText(m), "mono")

	m.input.SetText("/theme mono")
	next, _ = m.submit()
	m = next.(model)
	okSwitch := activeTheme.Name == "mono" && lastKind(m) == kSys && strings.Contains(lastText(m), "已切换")

	m.input.SetText("/theme nope")
	next, _ = m.submit()
	m = next.(model)
	okBadName := activeTheme.Name == "mono" && lastKind(m) == kWarn && strings.Contains(lastText(m), "没有这个主题")

	m.input.SetText("/theme sprout")
	next, _ = m.submit()
	m = next.(model)
	okBack := activeTheme.Name == "sprout" && lastKind(m) == kSys
	check("命令：列表（* 标当前）→ 切 mono → 未知名字只警示（不切换）→ 切回；不发引擎、不进历史",
		okList && okSwitch && okBadName && okBack && len(m.sent) == 0)

	// ④ mono = 全灰探测器；sprout = 关键色齐备
	applyTheme(monoTheme)
	monoSample := themeSample()
	monoClean := !hasNonGrayColor(monoSample) && !hasBackgroundColor(monoSample)

	applyTheme(sproutTheme)
	sproutSample := themeSample()
	okColors := strings.Contains(sproutSample, "38;2;0;231;164") && // 强调绿（用户竖条）
		strings.Contains(sproutSample, "38;2;121;199;125") && // 成功（工具绿）
		strings.Contains(sproutSample, "38;2;201;123;123") && // 失败（工具红）
		strings.Contains(sproutSample, "38;2;255;209;102") // 提醒（琥珀）
	check("mono：界面样张全灰（无彩色 SGR、无背景色）；sprout：强调/成功/失败/提醒四色都在",
		monoClean && okColors)

	// ④b M6：markdown 标题/链接颜色来自主题（sprout 实测 #E6E6E6 / #6FB6F0）
	mdOut := ""
	if out, ok := renderMarkdown("# 标题\n\n[链接](https://example.com)", 60); ok {
		mdOut = out
	}
	okMD := strings.Contains(mdOut, "38;2;230;230;230") && strings.Contains(mdOut, "38;2;111;182;240")
	check("markdown：标题/链接使用主题色（sprout 实测）", okMD)

	// ⑤ 渐变：10 格、端点 = A / C、50% 填充格 ≥3 种颜色（确实在渐变）
	grad := contextBarGrad(10)
	probe := func(c color.Color) string {
		return lipgloss.NewStyle().Foreground(c).Render("\u2588")
	}
	bar50 := model{status: stIdle, width: 110, usageUsed: 50000, usageSize: 100000}.renderContextBar()
	distinct := 0
	for _, c := range grad[:5] { // 50% → 填充 5 格
		if strings.Contains(bar50, probe(c)) {
			distinct++
		}
	}
	okGrad := len(grad) == 10 &&
		rgbTriple(grad[0]) == rgbTriple(lipgloss.Color(sproutTheme.GradA)) &&
		rgbTriple(grad[9]) == rgbTriple(lipgloss.Color(sproutTheme.GradC)) &&
		distinct >= 3
	bar66 := model{status: stIdle, width: 110, usageUsed: 66000, usageSize: 100000}.renderContextBar()
	bar91 := model{status: stIdle, width: 110, usageUsed: 91000, usageSize: 100000}.renderContextBar()
	okTiers := strings.Contains(bar66, "38;2;255;209;102") && !strings.Contains(bar66, probe(grad[0])) &&
		strings.Contains(bar91, "38;2;201;123;123") && !strings.Contains(bar91, "38;2;255;209;102")
	check("上下文条：绿档 = 渐变 A→B→C（填充 5 格 ≥3 色、最左 = A、最右 = C）；黄/红档整段换色",
		okGrad && okTiers)

	// ⑥ 切换主题必须作废 markdown 渲染器缓存（否则 markdown 还用旧色）
	_ = mdRenderer(40)
	warm := len(mdRenderers)
	applyTheme(monoTheme)
	applyTheme(sproutTheme)
	check("切换主题作废 markdown 渲染器缓存（旧配置里烤着旧色）",
		warm > 0 && len(mdRenderers) == 0)

	// ⑦ 切换主题必须作废消息条渲染缓存（同一 item 不 bump ver 也要重渲染）
	applyTheme(sproutTheme)
	probeIt := &FeedItem{Kind: kUser, Text: "缓存探测"}
	probeFeed := NewFeed()
	probeFeed.SetSize(60, 10)
	beforeLines := strings.Join(probeFeed.itemLines(probeIt), "\n") // 预热缓存（sprout 绿条）
	applyTheme(monoTheme)
	afterLines := strings.Join(probeFeed.itemLines(probeIt), "\n")
	okEpoch := strings.Contains(beforeLines, "38;2;0;231;164") &&
		!strings.Contains(afterLines, "38;2;0;231;164")
	applyTheme(sproutTheme)
	check("换主题作废消息条渲染缓存（不 bump ver 也重渲染：绿条 → 灰条）", okEpoch)

	if failed {
		fmt.Println("themetest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("themetest: 全部通过")
}
