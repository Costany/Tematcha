// md.go —— markdown 渲染层（回答区专用）
//
// 资产与设计：
//   - 渲染器：charm.land/glamour/v2（与 bubbletea/lipgloss 同门），底层 goldmark + chroma
//   - 透明度原则：基于内置 dark 样式做"去背景"定制（禁止大块底色）
//   - 宽度：渲染器按 WordWrap 宽度缓存复用；调用方保证 wrap 宽 ≤ 可用宽 - 4
//   - 回退：渲染失败时由调用方退回纯文本渲染（ui_feed.go 的 labeled）
package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
)

// ---------------------------------------------------------------------------
// 样式：dark 的"透明版"
// ---------------------------------------------------------------------------

func uptr(v uint) *uint     { return &v }
func sptr(v string) *string { return &v }
func boolPtr(v bool) *bool  { return &v }

// transparentDarkStyle 基于内置 dark 样式构建透明版：
//   - 全部背景色置空（透明度原则：好让终端磨砂透过去）
//   - 文档 margin 清零、首尾空行清空（宽度与拼接由外层统一管理）
//   - H1 的"底色空格前缀"一并清掉
//
// 注意：只对拷贝出的结构体字段做置空/换指针，不修改指向的原值。
func transparentDarkStyle() ansi.StyleConfig {
	s := styles.DarkStyleConfig

	s.Document.Margin = uptr(0)
	s.Document.BlockPrefix = ""
	s.Document.BlockSuffix = ""
	// 正文基色接主题 Text（2026-09-19 用户反馈"很晃眼"）：此前不设色，吃终端
	// 默认接近纯白；crush 的正文是 charmtone Smoke #BFBCC8 这类柔和灰，我们落到
	// 主题 Text #D0D0D0，与界面其余文字一致——回答区是大段阅读区，最忌纯白。
	s.Document.Color = sptr(activeTheme.Text)

	s.H1.BackgroundColor = nil
	s.H1.Prefix = ""
	s.H1.Suffix = ""

	s.Code.BackgroundColor = nil // 行内代码去底色
	// 行内代码颜色：主题 Code（sprout = 鲜艳绿 #29B72D）。
	// 2026-09-19 用户反馈"绿色也晃眼睛"——此前用强调绿 Accent（#4EE05E，
	// 与用户竖条同色），那个亮度给小面积竖条正好、给行内文字太刺；同日两轮
	// 反馈后定稿 #29B72D（"纯度太低了"→ 调鲜艳）。
	s.Code.Color = sptr(activeTheme.Code)

	// M6 全量主题化：标题 / 链接 / 斜体接入主题（此前只有行内代码与粗体）。
	// 只覆盖颜色，其余形态沿用 dark 样式；H1 的暗色底已在上方清掉。
	s.Heading.Color = sptr(activeTheme.Heading)
	s.H1.Color = sptr(activeTheme.Heading)
	s.H2.Color = sptr(activeTheme.Heading)
	s.H3.Color = sptr(activeTheme.Heading)
	s.H4.Color = sptr(activeTheme.Heading)
	s.H5.Color = sptr(activeTheme.Heading)
	s.H6.Color = sptr(activeTheme.Heading)
	s.Link.Color = sptr(activeTheme.Link)
	s.LinkText.Color = sptr(activeTheme.Link)
	s.Emph.Color = sptr(activeTheme.Emph)

	// 粗体：终端对 CJK 的 SGR-1 加粗常常没有视觉变化（中文字体普遍没有
	// 粗体变体），因此同时给颜色保证强调可见——这也是 letcode/crush
	// 一类专业 TUI 的通行做法：不依赖终端粗体，用颜色表达强调。
	// 2026-09-19 用户反馈纯白加粗"巨晃"：Strong 从 #FFFFFF 换成柔白
	// （charmtone Sash #D9E7D7，crush 的最亮中性色）——仍亮于正文但不刺。
	s.Strong = ansi.StylePrimitive{
		Bold:  boolPtr(true),
		Color: sptr(activeTheme.Strong),
	}

	if s.CodeBlock.Chroma != nil {
		s.CodeBlock.Chroma.Background.BackgroundColor = nil
		s.CodeBlock.Chroma.Error.BackgroundColor = nil
		// 代码块基色同样接主题 Text：chroma 只给语法制导着色，纯文本部分
		// 沿用 dark 的亮色——大段代码块白底白字最晃，一并柔化。
		s.CodeBlock.Chroma.Text.Color = sptr(activeTheme.Text)
	}
	return s
}

// ---------------------------------------------------------------------------
// 渲染器（按宽度缓存）
// ---------------------------------------------------------------------------

var mdRenderers = map[int]*glamour.TermRenderer{}

// mdRenderer 取指定折行宽度的渲染器（没有就新建并缓存）。
func mdRenderer(w int) *glamour.TermRenderer {
	if r, ok := mdRenderers[w]; ok {
		return r
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(transparentDarkStyle()),
		glamour.WithWordWrap(w),
	)
	if err != nil {
		return nil
	}
	mdRenderers[w] = r
	return r
}

// renderMarkdown 把 markdown 渲染成多行文本（首尾空白已裁剪）。
// w 为 glamour 的折行宽度（调用方保证 ≤ 终端可用宽 - 缩进）。
//
// 后处理（适配 v2 输出特性）：
//   - 剥掉 OSC 8 超链接序列（防终端/渲染管线兼容问题；链接文本与颜色保留）
//   - 裁掉每行尾部填充空格（glamour 为背景色块把行 pad 到满宽；我们禁用了
//     背景色，这些填充只剩副作用——会让流式光标漂到最右侧）
func renderMarkdown(text string, w int) (string, bool) {
	r := mdRenderer(w)
	if r == nil {
		return "", false
	}
	out, err := r.Render(text)
	if err != nil {
		return "", false
	}
	out = osc8Re.ReplaceAllString(out, "")
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		lines[i] = trimLineTail(ln)
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n"), true
}

// osc8Re 匹配 OSC 8 超链接序列（开始与结束标记同形）。
var osc8Re = regexp.MustCompile("\x1b\\]8;[^\x07\x1b]*(?:\x07|\x1b\\\\)")

// sgrTailRe 匹配行尾连续的一串 ANSI SGR 序列（\x1b[m 即 reset）。
var sgrTailRe = regexp.MustCompile("(?:\x1b\\[[0-9;]*m)+$")

// padSegRe 匹配 glamour 的"彩色空格填充段"：
// 每个填充空格形如 SGR + 空格 + reset，且连续成串出现在行尾。
var padSegRe = regexp.MustCompile("(?:\x1b\\[[0-9;]*m \x1b\\[m)+$")

// trimLineTail 裁掉行尾填充（glamour 为背景色把行 pad 到满宽；我们禁了背景色，
// 填充只剩副作用——流式光标会漂到最右侧）。
// 填充形态是"彩色空格段"，须整段剥离；循环处理直到行尾为普通内容
// （允许保留一个尾部 reset 之类的 SGR）。
func trimLineTail(ln string) string {
	for {
		// ① 整段彩色空格填充
		if seg := padSegRe.FindString(ln); seg != "" {
			ln = ln[:len(ln)-len(seg)]
			continue
		}
		// ② 行尾是 SGR，但它前面还有普通空格
		tail := sgrTailRe.FindString(ln)
		body := ln[:len(ln)-len(tail)]
		if trimmed := strings.TrimRight(body, " \t"); trimmed != body {
			ln = trimmed + tail
			continue
		}
		// ③ 行尾就是普通空格
		if trimmed := strings.TrimRight(ln, " \t"); trimmed != ln {
			ln = trimmed
			continue
		}
		return ln
	}
}

// ---------------------------------------------------------------------------
// 回答区渲染
// ---------------------------------------------------------------------------

// assistantLines 渲染回答区：
//   - glamour markdown（透明样式）
//   - 整块 2 格缩进、无前缀符号（用户定稿：去掉铅笔，回答区保持干净；
//     轮次之间的区隔交给回合分隔行 ◇）
//   - 渲染失败或空文本时回退纯文本
func assistantLines(it *FeedItem, w int) []string {
	if strings.TrimSpace(it.Text) == "" {
		return []string{"  "}
	}
	wrap := w - 2
	if wrap < 20 {
		wrap = 20
	}
	out, ok := renderMarkdown(it.Text, wrap)
	if !ok {
		return plainIndent(it.Text, w, 2)
	}
	lines := strings.Split(out, "\n")
	res := make([]string, 0, len(lines))
	for _, ln := range lines {
		res = append(res, "  "+ln)
	}
	return res
}

// plainIndent 纯文本回退渲染：按 indent 格缩进、CJK 安全折行（不用 markdown 时用）。
func plainIndent(text string, w, indent int) []string {
	textW := w - indent
	if textW < 8 {
		textW = 8
	}
	var out []string
	for _, ln := range wrapText(text, textW) {
		out = append(out, strings.Repeat(" ", indent)+textStyle.Render(ln))
	}
	if len(out) == 0 {
		out = append(out, strings.Repeat(" ", indent))
	}
	return out
}

// ---------------------------------------------------------------------------
// -mdtest：无 TTY 的渲染自检（结构视图 + 背景色检查）
// ---------------------------------------------------------------------------

// bgSeqRe 匹配 ANSI 背景色序列：标准色 40-47、256 色 48;5;n、真彩色 48;2;r;g;b。
var bgSeqRe = regexp.MustCompile(`\x1b\[(4[0-7]|48;5;\d+|48;2;\d+;\d+;\d+)m`)

// hasBackgroundColor 检查渲染输出里是否含背景色 ANSI 序列。
func hasBackgroundColor(s string) bool {
	return bgSeqRe.MatchString(s)
}

// hasStrayBackground 检查渲染输出里是否含"计划外"的背景色序列。
//
// 唯一合法的底色 = 错误徽章 ERROR（§1.5 / §0.5 例外，2026-09-19 用户点菜照
// crush）：白字 + 低饱和红底小方块。透明度原则（禁止大块底色）在那 7 格上
// 让位给错误的辨识度——除此之外任何地方再出现底色都算回归。
func hasStrayBackground(s string) bool {
	badge := rgbTriple(lipgloss.Color(activeTheme.ErrBadge))
	for _, m := range bgSeqRe.FindAllString(s, -1) {
		if !strings.Contains(m, badge) {
			return true
		}
	}
	return false
}

// runMDTest 渲染一段样例 markdown：
//   - 打印结构（剥 ANSI + 标注每行可见宽度），检查折行与缩进
//   - 检查输出里是否残留背景色序列（透明度原则）
func runMDTest() {
	sample := sampleMarkdown()
	out, ok := renderMarkdown(sample, 66)
	if !ok {
		fmt.Println("渲染失败")
		return
	}

	fmt.Println("== 结构视图（已剥 ANSI；行尾 (w=N) 是可见宽度）==")
	for _, ln := range strings.Split(out, "\n") {
		fmt.Printf("  %s  (w=%d)\n", stripANSI(ln), lipgloss.Width(ln))
	}

	fmt.Println()
	okAll := true
	if hasBackgroundColor(out) {
		okAll = false
		fmt.Println("!! 背景色检查：检测到背景色序列（透明度原则被破坏）")
	} else {
		fmt.Println("OK 背景色检查：未检测到背景色序列")
	}
	if strings.Contains(out, "38;2;239;247;234") && strings.Contains(out, "38;2;124;200;248") {
		fmt.Println("OK 主题化检查：标题与链接使用主题色")
	} else {
		okAll = false
		fmt.Println("!! 主题化检查：标题/链接未使用主题色")
	}

	// 防晃眼检查（2026-09-19 用户反馈"晃眼""加粗巨晃，白色晃""绿色也晃眼睛"）：
	// 正文接主题 Text（#D0D0D0 = 208;208;208）、粗体低纯度绿白 Strong
	// （#D9E7D7 = 217;231;215）、行内代码鲜艳绿 Code（#29B72D = 41;183;45）；
	// 且整篇不得出现纯白 #FFFFFF（255;255;255）——纯白加粗正是晃眼根源
	// （取证见规格 §15 ㊺）。
	antiGlare := strings.Contains(out, "38;2;208;208;208") &&
		strings.Contains(out, "38;2;217;231;215") &&
		strings.Contains(out, "38;2;41;183;45") &&
		!strings.Contains(out, "38;2;255;255;255")
	if antiGlare {
		fmt.Println("OK 防晃眼检查：正文/绿白粗体/鲜艳绿行内代码在场，纯白缺席")
	} else {
		okAll = false
		fmt.Println("!! 防晃眼检查：正文/粗体/行内代码配色不符预期，或出现纯白")
	}

	// 抽查粗体：含"加粗"的行原始 ANSI（确认亮色样式已注入）
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(stripANSI(ln), "加粗") {
			fmt.Printf("粗体抽查 raw: %q\n", ln)
			break
		}
	}

	// 性能采样：流式场景每个 chunk 都会全量重渲染，先量一下单次成本。
	const runs = 50
	start := time.Now()
	for i := 0; i < runs; i++ {
		renderMarkdown(sample, 66)
	}
	elapsed := time.Since(start)
	fmt.Printf("性能采样：%d 次渲染共 %v（平均 %v/次）\n", runs, elapsed, elapsed/runs)

	if !okAll {
		os.Exit(1)
	}
}

// sampleMarkdown 自检样例：覆盖常见构造 + CJK 折行 + 超长代码行。
func sampleMarkdown() string {
	return strings.Join([]string{
		"## 二级标题样式",
		"",
		"普通段落，包含 **加粗**、*斜体*、`行内代码` 与 [链接](https://example.com)。中文折行宽度测试：这是一段足够长的中文文本，用来验证换行是否在正确的位置发生，不应该超过设定的折行宽度。",
		"",
		"1. 有序列表第一项",
		"2. 第二项",
		"",
		"- 无序列表 A",
		"- 无序列表 B",
		"",
		"```go",
		"func main() {",
		"\tfmt.Println(\"hello, 世界\")",
		"}",
		"```",
		"",
		"| 列一 | 列二 |",
		"|------|------|",
		"| 数据A | 数据B |",
		"",
		"> 引用一行",
		"",
		"超长代码行测试：",
		"",
		"```",
		"this_is_a_very_long_line_of_code_that_should_show_how_overflow_is_handled_0123456789_abcdefghijklmnopqrstuvwxyz",
		"```",
	}, "\n")
}

// stripANSI 去掉 ANSI 转义序列（CSI 与 OSC；仅用于自检输出）。
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c == 0x1b && i+1 < len(s) {
			switch s[i+1] {
			case '[': // CSI：跳到终止字母
				j := i + 2
				for j < len(s) {
					ch := s[j]
					if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') {
						j++
						break
					}
					j++
				}
				i = j
				continue
			case ']': // OSC：跳到 BEL 或 ESC 反斜杠
				j := i + 2
				for j < len(s) {
					if s[j] == 0x07 {
						j++
						break
					}
					if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
						j += 2
						break
					}
					j++
				}
				i = j
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}
