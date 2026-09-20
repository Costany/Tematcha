// theme.go —— 主题系统（M5c · §11 / §11.5）：色板集中 + 可切换。
//
// 设计要点：
//   - 全部前景色集中到 Theme 结构（§0.5：只前景色、禁止大块底色）；
//   - 内置主题：sprout（默认 · 翠绿）/ mono（灰度 · 无彩色）；
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

	// markdown 元素（M6 全量主题化：标题 / 链接 / 斜体 / 行内代码）
	Heading string
	Link    string
	Emph    string
	Code    string // 行内代码（低饱和柔色，防晃眼）

	// 线框与容器
	Rule        string // 细线（输入区框线、进度条空槽）
	Placeholder string // 占位提示
	Track       string // 滚动条轨道
	ScrollThumb string // 滚动条拇指（消息区 / 右栏共用；2026-09-19 用户点名用「体绿」）

	// 强调与状态
	Accent     string // 小面积强调绿（用户竖条 / 行内代码 / 滚动条拇指 / 回执）
	OK         string // 成功（工具柔绿）
	Err        string // 失败（工具柔红）
	ErrSoft    string // 错误块（柔红 + bold）
	ErrBadge   string // 错误徽章 ERROR 的底色（鲜艳玫红；全应用唯一一处铺底色，§0.5 例外）
	PanelLabel string // 右栏字段标签（标识 / 模型 / 模式 / 上下文 / LSPs / MCPs / skills）
	// 右栏字段值（2026-09-19 用户点菜：加粗 + 按经典绿恐龙吉祥物的三色赋值——
	// 体绿 / 鞋橙 / 鞍红；肚皮白不用）。mono 下退化为三级灰。
	PanelValID    string // 值·标识（session id）
	PanelValModel string // 值·模型名
	PanelValMode  string // 值·模式
	Warn          string // 提醒（思考中 / 排队 / 需要批准）
	Reply         string // 回复中
	PromptOff     string // 输入提示符（空输入）
	PromptOn      string // 输入提示符（打字后）

	// 柔绿渐变锚点（§11.5：A 青柠黄绿 → B 草绿 → C 深植绿）
	GradA, GradB, GradC string
	// 品牌渐变锚点（§11.5：A 翠绿 → B 素白 → C 暖橙）。
	// 只用于右栏顶部品牌行（逐字符取色）；mono 下退化为灰阶。
	BrandA, BrandB, BrandC string

	// 工作行乱码流光渐变锚点（M21：A 鲜艳绿 → B 浅绿）。
	// 照 crush anim.go 的 CycleColors 双色坡道（A→B→A→B 每帧滑 1 格）；
	// 用户 2026-09-20 点名"颜色鲜艳点、主色绿色不要换、不要暗暗的绿色"——
	// 所以两端都是亮绿，不碰 Grad 系的深植绿。mono 下退化为灰阶。
	WorkGradA, WorkGradB string

	// 推理档调色（M25：照本机 pi 的 statusline 扩展，档位高低各一色）。
	// low 绿 / medium 黄 / high 橙 / xhigh 青 / max 玫粉；none·minimal 退灰
	// （用 Dim，不占 token）；引擎自定义档位用正文色，不瞎猜强弱。mono 退灰阶。
	ThinkLow, ThinkMed, ThinkHigh, ThinkXhi, ThinkMax string
}

// sproutTheme 内置默认主题：翠绿（2026-09-19 定调，只求"绿绿的"清新观感，
// 不用任何 IP 字样）。取色映射：体绿（Accent/OK/PromptOn/渐变 B）、
// 亮白绿（Heading/渐变 A）、柔红（Err/ErrSoft）、暖橙（Warn 提醒/审批）、
// 天空蓝（Reply/Link）。仍只前景色；mono 探测器（灰度主题）不受影响。
var sproutTheme = Theme{
	Name: "sprout", Desc: "翠绿（默认）",

	Text: "#D0D0D0", TextHi: "#E6E6E6", Faint: "#8F8F8F", Dim: "#6E6E6E", Strong: "#D9E7D7",
	Tool: "#9A9A9A", ThoughtPre: "#8A8A8A", ThoughtTtl: "#B4B4B4",
	Rule: "#3E5340", Placeholder: "#5A6A5A", Track: "#3A4A3C", ScrollThumb: "#5AC54F",
	Accent: "#4EE05E", OK: "#8CE28A", Err: "#C97B7B", ErrSoft: "#D98A8A", ErrBadge: "#FE2D6D",
	PanelLabel: "#6E6E6E",
	PanelValID: "#5AC54F", PanelValModel: "#F88F00", PanelValMode: "#E03A3A",
	Warn: "#F5A25C", Reply: "#7CC8F8", PromptOff: "#4E5E4E", PromptOn: "#3FCF52",
	Heading: "#EFF7EA", Link: "#7CC8F8", Emph: "#C2D2BC", Code: "#29B72D",
	GradA: "#C8F5A0", GradB: "#5FCC6E", GradC: "#2F7D46",
	BrandA: "#5AC54F", BrandB: "#F5F5F0", BrandC: "#F5A623",
	WorkGradA: "#4EE05E", WorkGradB: "#C8F5A0",
	ThinkLow: "#8CE28A", ThinkMed: "#E6C455", ThinkHigh: "#F5A25C", ThinkXhi: "#7CD8E8", ThinkMax: "#F06FA8",
}

// monoTheme 灰度主题：全部无色相（R=G=B）。
// 两个用途：无彩色/色弱终端的可用性选项；以及自检的"硬编码探测器"
// （切成 mono 后样张里若还出现彩色 SGR，说明有渲染点没走主题）。
// 注意：markdown 内部样式（标题/链接/chroma）暂仍随 glamour dark，见 §11 说明。
var monoTheme = Theme{
	Name: "mono", Desc: "灰度（界面无彩色）",

	Text: "#D0D0D0", TextHi: "#F2F2F2", Faint: "#969696", Dim: "#6E6E6E", Strong: "#F0F0F0",
	Tool: "#9A9A9A", ThoughtPre: "#8A8A8A", ThoughtTtl: "#B4B4B4",
	Rule: "#464646", Placeholder: "#565656", Track: "#3C3C3C", ScrollThumb: "#D0D0D0",
	Accent: "#E8E8E8", OK: "#C8C8C8", Err: "#A8A8A8", ErrSoft: "#D8D8D8", ErrBadge: "#8C8C8C",
	PanelLabel: "#6E6E6E",
	PanelValID: "#C8C8C8", PanelValModel: "#B0B0B0", PanelValMode: "#A8A8A8",
	Warn: "#DDDDDD", Reply: "#C4C4C4", PromptOff: "#4A4A4A", PromptOn: "#D0D0D0",
	Heading: "#F2F2F2", Link: "#C4C4C4", Emph: "#B0B0B0", Code: "#B0B0B0",
	GradA: "#F0F0F0", GradB: "#C8C8C8", GradC: "#8C8C8C",
	BrandA: "#F0F0F0", BrandB: "#C8C8C8", BrandC: "#8C8C8C",
	WorkGradA: "#C8C8C8", WorkGradB: "#F0F0F0",
	ThinkLow: "#A8A8A8", ThinkMed: "#BCBCBC", ThinkHigh: "#D0D0D0", ThinkXhi: "#E0E0E0", ThinkMax: "#F0F0F0",
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
	accentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Accent))
	thoughtPre = lipgloss.NewStyle().Foreground(lipgloss.Color(t.ThoughtPre))
	thoughtTtl = lipgloss.NewStyle().Foreground(lipgloss.Color(t.ThoughtTtl))
	thoughtTxt = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Faint))
	toolStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Tool))
	toolOKStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.OK))
	toolErStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Err))
	toolOutStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Faint))
	dimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.Dim))
	errStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.ErrSoft)).Bold(true)
	// 错误徽章（2026-09-19 用户点菜，照 crush）：白字 + 红底小方块。
	// 这是全应用唯一一处铺底色——§0.5 的透明度原则在此让位给错误的辨识度
	// （徽章只 7 格，不是大块底色；mono 主题下退化为灰底）。
	errBadgeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF")).Background(lipgloss.Color(t.ErrBadge)).Bold(true)
	// 右栏字段标签（2026-09-19 用户点菜：从紫色改回灰色，与次要信息同档）。
	panelLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.PanelLabel))
	// 右栏字段值（2026-09-19 用户点菜：加粗 + 按经典绿恐龙吉祥物的三色赋值）
	panelValIDStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.PanelValID)).Bold(true)
	panelValModelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.PanelValModel)).Bold(true)
	panelValModeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.PanelValMode)).Bold(true)

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
	scrollThumbStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(t.ScrollThumb))
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

var (
	brandCacheName string
	brandCache     []color.Color
)

// brandGrad 取 n 格品牌渐变（A → B → C，CIELAB 混合）。按主题名缓存。
// 与 contextBarGrad 分开一套缓存：锚点不同（见 Theme.BrandA/BrandC）。
func brandGrad(n int) []color.Color {
	if brandCacheName == activeTheme.Name && len(brandCache) == n {
		return brandCache
	}
	brandCache = lipgloss.Blend1D(n,
		lipgloss.Color(activeTheme.BrandA),
		lipgloss.Color(activeTheme.BrandB),
		lipgloss.Color(activeTheme.BrandC))
	brandCacheName = activeTheme.Name
	return brandCache
}

// brandText 右栏顶部品牌行：逐字符取品牌渐变色（§11.5）。
// 文字 = "Letcode - Tematcha"（2026-09-19 用户点名；此前只有 letcode 一个词）。
// 确定性：同参可复现（渐变只由主题与字符数决定）。
func brandText(s string) string {
	rs := []rune(s)
	if len(rs) == 0 {
		return ""
	}
	grad := brandGrad(len(rs))
	var b strings.Builder
	for i, r := range rs {
		b.WriteString(lipgloss.NewStyle().Foreground(grad[i]).Render(string(r)))
	}
	return b.String()
}

var (
	workCacheName string
	workCache     []color.Color
)

// workGrad 取 n 格工作行流光坡道（A → B → A → B，CIELAB 混合）。
// 照 crush anim.go 的 CycleColors：坡道长 = 流动区宽 × 3，每帧偏移 +1、
// 走 2×宽 后归零——渐变坡道在字符块上滑动，即"流光"。与 contextBarGrad /
// brandGrad 各一套缓存：锚点不同（见 Theme.WorkGradA/B）。
func workGrad(n int) []color.Color {
	if workCacheName == activeTheme.Name && len(workCache) == n {
		return workCache
	}
	workCache = lipgloss.Blend1D(n,
		lipgloss.Color(activeTheme.WorkGradA),
		lipgloss.Color(activeTheme.WorkGradB),
		lipgloss.Color(activeTheme.WorkGradA),
		lipgloss.Color(activeTheme.WorkGradB))
	workCacheName = activeTheme.Name
	return workCache
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
	b.WriteString(m.renderHintBar() + "\n") // 2026-09-20：快捷键提示独立成行
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

// runThemeTest 断言主题系统：注册表 / /theme 命令 / 渐变几何 /
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
	monoClean := !hasNonGrayColor(monoSample) && !hasStrayBackground(monoSample)

	applyTheme(sproutTheme)
	sproutSample := themeSample()
	okColors := strings.Contains(sproutSample, "38;2;78;224;94") && // 体绿（用户竖条）
		strings.Contains(sproutSample, "38;2;140;226;138") && // 成功（工具绿）
		strings.Contains(sproutSample, "38;2;201;123;123") && // 失败（工具红）
		strings.Contains(sproutSample, "38;2;245;162;92") // 暖橙（提醒）
	check("mono：界面样张全灰（无彩色 SGR、无背景色）；sprout：强调/成功/失败/提醒四色都在",
		monoClean && okColors)

	// ④b M6：markdown 标题/链接颜色来自主题（sprout 实测 #EFF7EA / #7CC8F8）
	mdOut := ""
	if out, ok := renderMarkdown("# 标题\n\n[链接](https://example.com)", 60); ok {
		mdOut = out
	}
	okMD := strings.Contains(mdOut, "38;2;239;247;234") && strings.Contains(mdOut, "38;2;124;200;248")
	check("markdown：标题/链接使用主题色（sprout 实测）", okMD)

	// ④c 防晃眼配色（2026-09-19 用户反馈"晃眼""加粗巨晃，白色晃""绿色也晃眼睛"）：
	// 正文 = 主题 Text、粗体 = 柔白 Strong（charmtone Sash）、行内代码 = 低饱和
	// Code；纯白 #FFFFFF 必须缺席（取证见规格 §15 ㊺）。
	glareSrc := "普通段落，包含 **加粗** 与 `行内代码`。"
	glareOut := ""
	if out, ok := renderMarkdown(glareSrc, 60); ok {
		glareOut = out
	}
	okGlareSprout := strings.Contains(glareOut, "38;2;208;208;208") && // 正文 Text #D0D0D0
		strings.Contains(glareOut, "38;2;217;231;215") && // 粗体 Strong #D9E7D7
		strings.Contains(glareOut, "38;2;41;183;45") && // 行内代码 Code #29B72D
		!strings.Contains(glareOut, "38;2;255;255;255") // 纯白缺席
	applyTheme(monoTheme)
	glareMono := ""
	if out, ok := renderMarkdown(glareSrc, 60); ok {
		glareMono = out
	}
	okGlareMono := strings.Contains(glareMono, "38;2;240;240;240") && // 粗体 Strong #F0F0F0
		strings.Contains(glareMono, "38;2;176;176;176") && // 行内代码 Code #B0B0B0
		!strings.Contains(glareMono, "38;2;255;255;255")
	applyTheme(sproutTheme)
	check("防晃眼：sprout 正文/柔白粗体/柔绿代码在场且纯白缺席；mono 对应柔灰（粗体 #F0F0F0 / 代码 #B0B0B0）",
		okGlareSprout && okGlareMono)

	// ④d M25 推理档配色（照本机 pi statusline 扩展的 getEffortColor：low 绿 /
	// medium 黄 / high 橙 / xhigh 青 / max 玫粉；none/minimal/off 退次要灰，
	// 未知自定义档位用正文色）。色板在 theme.go、断言在这里——改色必同步。
	applyTheme(sproutTheme)
	firstRGB := func(s string) string {
		if m := rgbRe.FindStringSubmatch(s); m != nil {
			return m[1] + ";" + m[2] + ";" + m[3]
		}
		return ""
	}
	thinkCases := []struct{ level, want string }{
		{"low", "140;226;138"},   // #8CE28A
		{"medium", "230;196;85"}, // #E6C455
		{"high", "245;162;92"},   // #F5A25C
		{"xhigh", "124;216;232"}, // #7CD8E8
		{"max", "240;111;168"},   // #F06FA8
	}
	okThink := true
	seen := map[string]bool{}
	for _, c := range thinkCases {
		got := firstRGB(reasoningStyle(c.level).Render("x"))
		if got != c.want {
			okThink = false
			fmt.Printf("      推理档 %s：期望 %s 实际 %s\n", c.level, c.want, got)
		}
		seen[got] = true
	}
	// 五档必须彼此可区分：同一个色 = 用户看不出档位差别
	okThink = okThink && len(seen) == len(thinkCases)
	applyTheme(monoTheme)
	okThinkMono := !hasNonGrayColor(reasoningStyle("none").Render("x"))
	for _, c := range thinkCases {
		if hasNonGrayColor(reasoningStyle(c.level).Render("x")) {
			okThinkMono = false
		}
	}
	applyTheme(sproutTheme)
	check("推理档配色：sprout 五档各就各位且互不相同；mono 全灰（含 none 降级档）",
		okThink && okThinkMono)

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
	okTiers := strings.Contains(bar66, "38;2;245;162;92") && !strings.Contains(bar66, probe(grad[0])) &&
		strings.Contains(bar91, "38;2;201;123;123") && !strings.Contains(bar91, "38;2;245;162;92")
	check("上下文条：绿档 = 渐变 A→B→C（填充 5 格 ≥3 色、最左 = A、最右 = C）；黄/红档整段换色",
		okGrad && okTiers)

	// ⑤b 工作行流光坡道（M21）：端点 = WorkGradA/B（A→B→A→B 四停），
	//     长度 = 流动区宽 × 3（crush CycleColors 口径）。
	wg := workGrad(workFlowN() * 3)
	okWorkGrad := len(wg) == workFlowN()*3 &&
		rgbTriple(wg[0]) == rgbTriple(lipgloss.Color(sproutTheme.WorkGradA)) &&
		rgbTriple(wg[len(wg)-1]) == rgbTriple(lipgloss.Color(sproutTheme.WorkGradB))
	check("工作行流光坡道：A→B→A→B（端点 = WorkGradA/B，长 = 流动区宽×3）", okWorkGrad)

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
	okEpoch := strings.Contains(beforeLines, "38;2;78;224;94") &&
		!strings.Contains(afterLines, "38;2;78;224;94")
	applyTheme(sproutTheme)
	check("换主题作废消息条渲染缓存（不 bump ver 也重渲染：绿条 → 灰条）", okEpoch)

	if failed {
		fmt.Println("themetest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("themetest: 全部通过")
}
