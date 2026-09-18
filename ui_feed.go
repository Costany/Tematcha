// ui_feed.go —— 消息区（feed）与全 UI 共用样式
//
// M1 的消息区：把每条消息铺成若干终端行，再"切"出可见窗口显示。
//
// 设计要点（参考 crush 的 list.go，做 M1 级简化）：
//   - 一维滚动坐标：scroll = 距底部的行数。scroll==0 即贴底，
//     新内容自然可见 —— 这就是最简的 follow 模式；
//   - 每条 item 有渲染缓存（按"宽度 + 内容版本"失效）；
//     流式中的 item 不缓存（每帧重渲染，因为每帧都可能变）；
//   - item 之间自动隔一个空行。
//
// 透明度原则：只使用前景色，禁止大块底色（好让终端磨砂透过去）。
package main

import (
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
)

// 样式（全 UI 共用；透明度原则：只前景色）。
//
// 颜色不写死在这里 —— 全部由主题驱动：theme.go 的 applyTheme 在包加载时按
// 默认主题 sprout 重建这些变量，/theme 切换时再重建一次。改色请去 theme.go。
// ---------------------------------------------------------------------------

var (
	textStyle    lipgloss.Style // 正文
	userBarStyle lipgloss.Style // 用户消息左侧竖条（强调绿）
	thoughtPre   lipgloss.Style // 思考前缀
	thoughtTtl   lipgloss.Style // 思考标题
	thoughtTxt   lipgloss.Style // 思考正文（暗）
	toolStyle    lipgloss.Style // 工具行（进行中）
	toolOKStyle  lipgloss.Style // 工具行·成功（柔绿）
	toolErStyle  lipgloss.Style // 工具行·失败（柔红）
	toolOutStyle lipgloss.Style // 工具卡展开区·输出（亮度介于正文与暗色之间）
	dimStyle     lipgloss.Style // 次要信息
	errStyle     lipgloss.Style // 错误（柔红）

	// 光标：反色格子 —— 与主题无关（反色不是颜色）
	cursorStyle = lipgloss.NewStyle().Reverse(true)

	// 输入区 / 状态栏
	ruleStyle        lipgloss.Style // 输入区上下细线
	placeholderStyle lipgloss.Style // 占位提示
	inputTextStyle   lipgloss.Style // 输入文字
	modelStyle       lipgloss.Style // letcode 字样
	thinkStyle       lipgloss.Style // 状态：思考中
	replyStyle       lipgloss.Style // 状态：回复中
	warnStyle        lipgloss.Style // 状态：提醒
	promptOffStyle   lipgloss.Style // 输入提示符（空输入）
	promptOnStyle    lipgloss.Style // 输入提示符（打字后）

	// 消息区右缘滚动条（M4a）：拇指=强调绿（与用户消息竖条同色，视觉语言统一），
	// 轨道=深灰（比输入区细线更暗，与右栏分隔列拉开层次）。只前景色，透明度原则。
	scrollThumbStyle lipgloss.Style // 拇指 ┃
	scrollTrackStyle lipgloss.Style // 轨道 │
)

// ---------------------------------------------------------------------------
// 消息种类
// ---------------------------------------------------------------------------

const (
	kUser = iota
	kThought
	kAssistant
	kTool
	kUsage
	kSys
	kError
	kTurn    // 回合分隔行：◇ 模型 via 供应商 in 总耗时
	kReceipt // 审批回执：绿条 ▌ + 说明（§1.6）
	kWarn    // 客户端护栏提示（M4e）：琥珀 ! 前缀——引擎必拒的命令形态提前拦下
	kQueued  // 排队中的用户消息（M3c）：暗色，回合结束后原地转正为 kUser
)

// FeedItem 是消息区里的一条消息。
type FeedItem struct {
	Kind   int
	Text   string
	Detail string // 错误行的"怎么办"提示（可选）

	// 计时（思考块 / 工具行）：StartedAt 在条目创建时打点；
	// EndedAt 为零值表示仍在进行——渲染时按"现在"实时计算耗时，
	// 结束后定格。两者都由 begin/finish/elapsed 三个小方法管理。
	StartedAt time.Time
	EndedAt   time.Time

	// 工具行专用：引擎在三个阶段给三种 title（pending=工具名、
	// in_progress=调用摘要、完成=结果摘要），分字段留存，避免
	// 完成更新把"是什么工具"覆盖成只剩结果。
	ToolName  string // 原始工具名：fs__read / shell__exec
	ToolCall  string // 调用摘要：fs__read demo-lab/README.md
	ToolEnd   string // 结果摘要：read demo-lab/README.md (12 lines)
	ToolChips string // 子代理结果计数（M5d）：read 2 · commands 1 · checks 1
	ToolState string // pending / in_progress / completed / failed

	// ToolBad = "实际失败"（渲染：红齿轮 / 红选中条 / 默认展开）：
	// 引擎 failed，或 shell 类工具"跑完但退出码非 0"——引擎按 ACP 语义
	// 把后者也报 completed（exit code 藏在结果摘要 "exit N" 里，是数据
	// 不是错误），推导见 main.handleToolUpdate。
	ToolBad bool

	// 工具卡展开区（M2b）：rawInput 与输出。
	// 输出在 ACP 里是"整体替换"式下发（每次给累计全文），ToolOut 始终是最新一份。
	ToolCmd      string // rawInput.command（shell 特例：单行命令展示）
	ToolRaw      string // rawInput 紧凑 JSON（没有 command 时展示）
	ToolOut      string // 工具输出全文（展开时渲染尾部 tail）
	ToolExpanded bool   // 卡片当前是否展开
	ToolUserSet  bool   // 用户手点过：之后不再受"自动展开/折叠"影响

	ver uint64 // 内容版本：每次修改 ++，用于缓存失效

	// 渲染缓存（cacheEpoch 对齐主题代次：换主题后旧缓存作废，见 theme.go）
	cacheW     int
	cacheVer   uint64
	cacheEpoch uint64
	cache      []string
	// 流式光标（由主模型维护）：Cursor=本条是否带光标；CursorOn=闪烁相位
	Cursor   bool
	CursorOn bool

	// 用户消息专用：Focused=被鼠标点击选中——左侧竖条由细线 │ 换成
	// 半块 ▌（观感照 crush 的 focused/blurred 两态）。
	Focused bool

	// 工具卡专用（M2b v2）：Selected=键盘选择态当前选中的卡——头行两格
	// 缩进换成强调绿 ▌（宽度不变、不铺底色，与用户消息同款视觉语言）。
	Selected bool
}

// touch 标记内容已变化（版本 +1）。
func (it *FeedItem) touch() { it.ver++ }

// begin 打计时起点（若尚未打点）。
func (it *FeedItem) begin() {
	if it.StartedAt.IsZero() {
		it.StartedAt = time.Now()
	}
}

// finish 打计时终点（若尚未打点）并让渲染缓存失效。
func (it *FeedItem) finish() {
	if it.EndedAt.IsZero() {
		it.EndedAt = time.Now()
		it.touch()
	}
}

// elapsed 返回耗时：已结束用实测区间，进行中则按"现在"实时计算。
func (it *FeedItem) elapsed() time.Duration {
	if it.StartedAt.IsZero() {
		return 0
	}
	if it.EndedAt.IsZero() {
		return time.Since(it.StartedAt)
	}
	return it.EndedAt.Sub(it.StartedAt)
}

// ---------------------------------------------------------------------------
// Feed 容器
// ---------------------------------------------------------------------------

// Feed 是消息区容器。
type Feed struct {
	items  []*FeedItem
	width  int // 渲染可用宽度（终端宽 - 6）
	height int // 可见行数（终端高 - 6）
	scroll int // 距底部行数；0 = 贴底

	// viewOwners 与最近一次 Render 的窗口行一一对应：每行属于哪个 item
	//（留白/空行为 nil）。鼠标点击命中测试用（View 每帧都调 Render，
	// 所以点击时的窗口归属总是最新的）。
	viewOwners []*FeedItem
}

// NewFeed 创建空消息区。
func NewFeed() *Feed {
	return &Feed{width: 80, height: 20}
}

// SetSize 更新尺寸；宽度变化会自然导致缓存失效（缓存带宽度键）。
func (f *Feed) SetSize(w, h int) {
	f.width = w
	f.height = h
}

// Len 返回消息条数。
func (f *Feed) Len() int { return len(f.items) }

// Last 返回最后一条消息（空 feed 返回 nil）。
// 用于"紧挨着的重复提示不落两遍"这类去重判断（见 main.submit 的护栏分支）。
func (f *Feed) Last() *FeedItem {
	if len(f.items) == 0 {
		return nil
	}
	return f.items[len(f.items)-1]
}

// Append 追加一条消息。
func (f *Feed) Append(kind int, text string) *FeedItem {
	it := &FeedItem{Kind: kind, Text: text}
	f.items = append(f.items, it)
	return it
}

// AppendStr 向一条消息追加文字（流式用）。
func (f *Feed) AppendStr(it *FeedItem, s string) {
	if it == nil || s == "" {
		return
	}
	it.Text += s
	it.touch()
}

// SetText 整体替换一条消息的文字（工具行状态更新用）。
func (f *Feed) SetText(it *FeedItem, s string) {
	if it == nil || it.Text == s {
		return
	}
	it.Text = s
	it.touch()
}

// ScrollBy 按行滚动（正数向上回看、负数向下）。
func (f *Feed) ScrollBy(n int) {
	f.scroll += n
	if f.scroll < 0 {
		f.scroll = 0
	}
	if max := f.maxScroll(); f.scroll > max {
		f.scroll = max
	}
}

// ScrollToBottom 回到底部（重新开启跟随）。
func (f *Feed) ScrollToBottom() { f.scroll = 0 }

// AtBottom 是否贴底。
func (f *Feed) AtBottom() bool { return f.scroll == 0 }

// maxScroll 最多能上滚多少行。
func (f *Feed) maxScroll() int {
	all, _ := f.allLines()
	n := len(all) - f.height
	if n < 0 {
		n = 0
	}
	return n
}

// ---------------------------------------------------------------------------
// 渲染
// ---------------------------------------------------------------------------

// allLines 把全部消息铺成行（带每条 item 的渲染缓存）。
// 第二个返回值与行一一对应：该行归属的 item（空行为 nil）。
func (f *Feed) allLines() ([]string, []*FeedItem) {
	var lines []string
	var owners []*FeedItem
	for i, it := range f.items {
		if i > 0 {
			lines = append(lines, "") // item 之间空一行
			owners = append(owners, nil)
		}
		item := f.itemLines(it)
		lines = append(lines, item...)
		for range item {
			owners = append(owners, it)
		}
	}
	return lines, owners
}

// itemLines 渲染一条消息（带缓存；流式中的 item 不缓存）。
func (f *Feed) itemLines(it *FeedItem) []string {
	if !it.Cursor && it.cache != nil && it.cacheW == f.width && it.cacheVer == it.ver && it.cacheEpoch == themeEpoch {
		return it.cache
	}
	out := renderItem(it, f.width)
	if !it.Cursor {
		it.cacheW, it.cacheVer, it.cacheEpoch, it.cache = f.width, it.ver, themeEpoch, out
	}
	return out
}

// normH 可见行数（下限 3：再小的终端也要能画东西）。
func (f *Feed) normH() int {
	h := f.height
	if h < 3 {
		h = 3
	}
	return h
}

// window 计算当前可见窗口：全部行、每行归属、起止下标。
// Render 与选择态的定位方法（ItemVisible / ScrollToItem）共用同一口径。
func (f *Feed) window() (all []string, owners []*FeedItem, start, end int) {
	all, owners = f.allLines()
	h := f.normH()
	end = len(all) - f.scroll
	if end > len(all) {
		end = len(all)
	}
	if end < 0 {
		end = 0
	}
	start = end - h
	if start < 0 {
		start = 0
	}
	return all, owners, start, end
}

// Render 切出可见窗口（不足一屏时在顶部补空行，让输入区钉在底部）。
// 顺手记下窗口每行的归属 item，供 ItemAt 做鼠标命中测试。
func (f *Feed) Render() []string {
	all, owners, start, end := f.window()
	h := f.normH()
	seg := all[start:end]
	own := owners[start:end]
	if padN := h - len(seg); padN > 0 {
		seg = append(make([]string, padN), seg...)
		own = append(make([]*FeedItem, padN), own...)
	}
	f.viewOwners = own
	return seg
}

// scrollBarW 滚动条列宽（消息区右缘的外挂列，占 1 个字符格）。
// 命中测试与 View 拼装都按它算边界（见 main.handleClick / main.View）。
const scrollBarW = 1

// Scrollbar 渲染消息区右缘的竖向滚动条（M4a）：
// 返回与可见窗口逐行对应的字符列（每行 1 格可见宽）；内容不溢出一屏时
// 整列都是空串 —— 调用方拼上去等于没这一列。
//
// 数学照 crush（internal/ui/common/scrollbar.go）：
//
//	拇指长 = max(1, 视口高 * 视口高 / 内容高)
//	拇指顶 = 距顶行数 * (视口高 - 拇指长) / (内容高 - 视口高)
//
// 我们的 scroll 是"距底部行数"（贴底 = 0），先换算成"距顶行数"再代入。
// 字形：拇指 ┃（强调绿）/ 轨道 │（深灰）——只前景色，透明度原则。
func (f *Feed) Scrollbar() []string {
	all, _ := f.allLines()
	h := f.normH()
	content := len(all)
	if content <= h {
		return make([]string, h) // 不溢出：整列留白
	}

	thumb := h * h / content // 拇指长度（至少 1 格）
	if thumb < 1 {
		thumb = 1
	}
	maxOffset := content - h // 距顶行数上限
	track := h - thumb       // 拇指可滑动的格数
	offset := maxOffset - f.scroll
	if offset < 0 {
		offset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}
	pos := 0
	if track > 0 {
		pos = offset * track / maxOffset
	}

	out := make([]string, h)
	for i := 0; i < h; i++ {
		if i >= pos && i < pos+thumb {
			out[i] = scrollThumbStyle.Render("\u2503") // ┃
		} else {
			out[i] = scrollTrackStyle.Render("\u2502") // │
		}
	}
	return out
}

// ItemAt 返回可见窗口第 row 行（0 起、自顶向下）所属的 item；
// 空行、顶部留白或越界返回 nil。
func (f *Feed) ItemAt(row int) *FeedItem {
	if row < 0 || row >= len(f.viewOwners) {
		return nil
	}
	return f.viewOwners[row]
}

// ItemVisible 判断 item 是否有任意一行落在当前可见窗口内（选择态用）。
func (f *Feed) ItemVisible(it *FeedItem) bool {
	_, owners, start, end := f.window()
	for i := start; i < end; i++ {
		if owners[i] == it {
			return true
		}
	}
	return false
}

// ScrollToItem 把滚动位置调到能看见 item 的头部行（选择态导航用）。
// 尽量少打扰：头行已可见则不动；在窗口上方 → 头行贴顶；在窗口下方 →
// 整卡装得下就整卡入画，装不下则头行贴顶（至少能看到卡片标题与状态）。
func (f *Feed) ScrollToItem(it *FeedItem) {
	all, owners, start, end := f.window()
	head, tail := -1, -1
	for i, o := range owners {
		if o == it {
			if head < 0 {
				head = i
			}
			tail = i
		}
	}
	if head < 0 {
		return
	}
	h := f.normH()
	var newEnd int
	switch {
	case head < start:
		newEnd = head + h
	case head >= end:
		if tail-head+1 <= h {
			newEnd = tail + 1
		} else {
			newEnd = head + h
		}
	default:
		return
	}
	f.scroll = len(all) - newEnd
	if f.scroll < 0 {
		f.scroll = 0
	}
	if max := f.maxScroll(); f.scroll > max {
		f.scroll = max
	}
}

// ---------------------------------------------------------------------------
// 单条消息的渲染
// ---------------------------------------------------------------------------

// userLines 渲染用户消息：左侧一根强调绿竖条 + 正文（不再用 ❯ 前缀）。
//
// 两态照 crush：普通态是细线 │，被鼠标点击选中（Focused）后换成半块 ▌
// ——视觉上"加粗"，两态同色（强调绿 #00E7A4）。
func userLines(it *FeedItem, w int) []string {
	bar := "\u2502" // │ 细线
	if it.Focused {
		bar = "\u258C" // ▌ 半块（点击后加粗）
	}
	textW := w - 4
	if textW < 8 {
		textW = 8
	}
	var out []string
	for _, ln := range wrapText(it.Text, textW) {
		out = append(out, "  "+userBarStyle.Render(bar)+" "+textStyle.Render(ln))
	}
	if len(out) == 0 {
		out = append(out, "  "+userBarStyle.Render(bar))
	}
	return out
}

func renderItem(it *FeedItem, w int) []string {
	switch it.Kind {
	case kUser:
		return userLines(it, w)
	case kAssistant:
		return withCursor(assistantLines(it, w), it)
	case kThought:
		return withCursor(thoughtLines(it, w), it)
	case kTool:
		return toolLines(it, w)
	case kUsage:
		return labeled("\u00B7", 1, w, it.Text, dimStyle, dimStyle)
	case kSys:
		return sysLines(w, it.Text)
	case kError:
		return gutterLines(errStyle, "错误", w, it.Text, it.Detail)
	case kWarn:
		// M4e 命令护栏：琥珀 ! + 正文（不是错误——是把引擎必拒的话本地说清）。
		return labeled("!", 1, w, it.Text, warnStyle, warnStyle)
	case kQueued:
		// M3c 排队中的用户消息：暗色竖条 + 暗字（转正后原地变回绿条亮字）
		return queuedLines(it, w)
	case kTurn:
		return turnLines(it, w)
	case kReceipt:
		return receiptLines(it, w)
	}
	return nil
}

// receiptLines 审批回执（§1.6 绿条）：强调绿竖条 ▌ + 灰白文字。
func receiptLines(it *FeedItem, w int) []string {
	textW := w - 4
	if textW < 8 {
		textW = 8
	}
	var out []string
	for _, ln := range wrapText(it.Text, textW) {
		out = append(out, "  "+userBarStyle.Render("\u258C")+" "+textStyle.Render(ln))
	}
	if len(out) == 0 {
		out = append(out, "  "+userBarStyle.Render("\u258C"))
	}
	return out
}

// queuedLines 排队中的用户消息（M3c）：形态与用户消息一致（竖条 + 正文、多行对齐），
// 但整体是暗色——绿条换成暗灰竖条、亮字换成暗字，表示"还没发出去"。
// 尾行右侧放得下时挂一个「· 排队中」标签（放不下就只靠颜色 + 状态栏的「排队 N」）。
//
// 出队转正时它会被原地换成 kUser（绿条 + 亮字），位置不动——不会像"重新发一条"
// 那样在消息区留下两份（老实现里这正是用户看到的"消息重复"）。
func queuedLines(it *FeedItem, w int) []string {
	textW := w - 4
	if textW < 8 {
		textW = 8
	}
	var out []string
	for _, ln := range wrapText(it.Text, textW) {
		out = append(out, "  "+dimStyle.Render("\u2502")+" "+dimStyle.Render(ln))
	}
	if len(out) == 0 {
		out = append(out, "  "+dimStyle.Render("\u2502"))
	}
	if tag := " \u00B7 排队中"; lipgloss.Width(out[len(out)-1])+lipgloss.Width(tag) <= w {
		out[len(out)-1] += dimStyle.Render(tag)
	}
	return out
}

// ---------------------------------------------------------------------------
// 思考块 / 工具行 / 回合分隔行的渲染
// ---------------------------------------------------------------------------

// thoughtLines 渲染思考块（对齐 letcode /thoughts 的 Full 档：标题 + 耗时 + 全文）：
//
//	~ <耗时> · <标题>
//	    <正文（全文，逐行折行）>
//
// 标题 = 思考文本的第一行非空行（剥掉 #、**、` 等标记，同 letcode 的做法）；
// 正文 = 其余内容。进行中时耗时实时增长，周期结束后定格。
func thoughtLines(it *FeedItem, w int) []string {
	title, body := splitThought(it.Text)
	if title == "" {
		if it.EndedAt.IsZero() {
			title = "思考中"
		} else {
			title = "思考"
		}
	}
	textW := w - 4
	if textW < 8 {
		textW = 8
	}
	var out []string
	for i, ln := range wrapText(title, textW) {
		if i == 0 {
			out = append(out, "  "+thoughtPre.Render("~")+" "+thoughtTtl.Render(ln))
		} else {
			out = append(out, "    "+thoughtTtl.Render(ln))
		}
	}
	if body != "" {
		for _, ln := range wrapText(body, textW) {
			out = append(out, "    "+thoughtTxt.Render(ln))
		}
	}
	// 耗时：放在思考文字最终行的下一行（块尾暗色小字；始终贴着块尾，
	// 流式增长时位置稳定，也不会被折行埋掉）。
	out = append(out, "    "+dimStyle.Render("· "+formatElapsed(it.elapsed())))
	return out
}

// splitThought 从思考全文里切出标题与正文（同 letcode reasoning_title_and_body 的规则）：
// 第一行非空行作标题，其余作正文；正文开头的空行去掉，每行都做标记清洗。
func splitThought(text string) (title, body string) {
	lines := strings.Split(text, "\n")
	idx := -1
	for i, ln := range lines {
		if strings.TrimSpace(cleanThoughtLine(ln)) != "" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", ""
	}
	title = cleanThoughtLine(lines[idx])
	rest := lines[idx+1:]
	for len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
		rest = rest[1:]
	}
	cleaned := make([]string, 0, len(rest))
	for _, ln := range rest {
		cleaned = append(cleaned, cleanThoughtLine(ln))
	}
	return title, strings.Join(cleaned, "\n")
}

// cleanThoughtLine 清洗思考行：去掉行首 # 标题记号与 **、` 标记，
// __ 只在整行包裹时剥——letcode 的 clean_reasoning_line 是全局替换，
// 会把 fs__read、__init__ 这类标识符也打断，这里收窄了范围。
func cleanThoughtLine(raw string) string {
	text := strings.TrimSpace(raw)
	for strings.HasPrefix(text, "#") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "#"))
	}
	text = strings.ReplaceAll(text, "**", "")
	text = strings.ReplaceAll(text, "`", "")
	if len(text) >= 4 && strings.HasPrefix(text, "__") && strings.HasSuffix(text, "__") {
		text = strings.TrimSpace(text[2 : len(text)-2])
	}
	return text
}

// toolTailN 工具卡展开时显示的输出行数（tail）：运行中跟末尾、完成后看结尾。
// 完整输出留在 ToolOut 里，以后要做"翻页看全量"时有的是数据。
const toolTailN = 8

// hasToolBody 卡片是否有可展开的内容（命令/参数/输出）。
func hasToolBody(it *FeedItem) bool {
	return it.ToolCmd != "" || it.ToolRaw != "" || it.ToolOut != ""
}

// toolLines 渲染工具卡（M2b）：头部一行 + 可展开的输出区。
//
//	⌬ ▸ <调用摘要> · <结果摘要>   ← 折叠态（▸ = 点击可展开）
//	⌬ ▾ <调用摘要> · <状态>
//	  $ <命令>                     ← 展开区：输入（$ 命令 / 紧凑 JSON 参数）
//	  │ <输出 tail：末 toolTailN 行>
//	  … 共 12 行 · 显示末 8 行
//
// 状态用齿轮颜色表达（用户定稿）：待执行/运行中灰、成功柔绿、失败柔红；
// ▸/▾ 是展开指示（鼠标点击切换，见 main.handleClick）。
// 引擎在三个阶段给三种 title：以调用摘要为主文（保住"是什么工具"），
// 结果摘要接在其后；放不下就另起一行（4 格缩进，暗色）。
func toolLines(it *FeedItem, w int) []string {
	head := it.ToolCall
	if head == "" {
		head = it.ToolName
	}
	if head == "" {
		head = it.Text
	}
	if head == "" {
		head = "工具调用"
	}
	suffix := ""
	gear := toolStyle
	switch {
	case it.ToolBad || it.ToolState == "failed":
		// 实际失败（引擎 failed，或 shell 退出码非 0）：红齿轮
		gear = toolErStyle
	case it.ToolState == "pending":
		suffix = " · 待执行"
	case it.ToolState == "in_progress" || it.ToolState == "running":
		suffix = " · 运行中"
	case it.ToolState == "completed":
		gear = toolOKStyle
	}

	// 齿轮后跟展开指示（▸ 收起 / ▾ 展开）；没有可展开内容时不带指示。
	// 子代理（M5d）：齿轮换 » 派遣图标，颜色仍随状态（成功绿/失败红/进行中灰）。
	pre := "\u23E3"
	if isAgentTool(it.ToolName) {
		pre = "\u00BB"
	}
	preW := 1
	if hasToolBody(it) {
		mark := "\u25B8" // ▸
		if it.ToolExpanded {
			mark = "\u25BE" // ▾
		}
		pre = pre + " " + mark
		preW = 3
	}
	out := labeled(pre, preW, w, head+suffix, gear, toolStyle)

	tail := ""
	tp := ""
	if it.ToolEnd != "" && it.ToolEnd != head && it.ToolEnd != it.ToolName {
		tp = it.ToolEnd
	}
	if it.ToolChips != "" { // M5d：子代理计数拼在结果行尾
		if tp != "" {
			tp += " · " + it.ToolChips
		} else {
			tp = it.ToolChips
		}
	}
	if tp != "" {
		tail = "· " + tp
	}
	if tail != "" {
		last := out[len(out)-1]
		if len(out) == 1 && lipgloss.Width(last)+1+lipgloss.Width(tail) <= w {
			out[len(out)-1] = last + " " + dimStyle.Render(tail)
		} else {
			textW := w - 4
			if textW < 8 {
				textW = 8
			}
			for _, ln := range wrapText(tail, textW) {
				out = append(out, "    "+dimStyle.Render(ln))
			}
		}
	}

	// 选择态（M2b v2）：头行两格缩进换成 ▌，颜色跟随该卡的状态色——
	// 与齿轮同色（成功柔绿 / 失败柔红 / 进行中灰），一眼能看出选中的卡
	// 是成是败；只换字形不挪位置，卡片整体宽度不变。
	if it.Selected && len(out) > 0 {
		out[0] = gear.Render("\u258C") + " " + out[0][2:]
	}

	if !it.ToolExpanded || !hasToolBody(it) {
		return out
	}
	return append(out, toolBodyLines(it, w)...)
}

// toolBodyLines 渲染工具卡展开区：输入（$ 命令或紧凑 JSON）+ 输出尾部。
// 输出行带暗灰│竖条、整段留尾（tail-follow），被截时补一行省略标记。
func toolBodyLines(it *FeedItem, w int) []string {
	var out []string
	textW := w - 4
	if textW < 8 {
		textW = 8
	}

	// 输入：shell 的 command 用 $ 前缀（最像命令的一行）；其余紧凑 JSON 暗色平铺。
	if it.ToolCmd != "" {
		for i, ln := range wrapText(it.ToolCmd, textW) {
			if i == 0 {
				out = append(out, "  "+dimStyle.Render("$")+" "+textStyle.Render(ln))
			} else {
				out = append(out, "    "+textStyle.Render(ln))
			}
		}
	} else if it.ToolRaw != "" {
		for _, ln := range wrapText(it.ToolRaw, textW) {
			out = append(out, "    "+dimStyle.Render(ln))
		}
	}

	// 输出：整段留尾，逐行折行后加 │ 竖条（暗灰，与用户消息的绿竖条区分）。
	src := strings.Split(strings.TrimRight(it.ToolOut, "\n"), "\n")
	if len(src) == 1 && src[0] == "" {
		return out
	}
	shown := src
	if len(src) > toolTailN {
		shown = src[len(src)-toolTailN:]
	}
	for _, ln := range shown {
		if strings.TrimSpace(ln) == "" {
			out = append(out, "  "+dimStyle.Render("\u2502"))
			continue
		}
		for _, seg := range wrapText(ln, textW) {
			out = append(out, "  "+dimStyle.Render("\u2502")+" "+toolOutStyle.Render(seg))
		}
	}
	if len(src) > toolTailN {
		out = append(out, "  "+dimStyle.Render(fmt.Sprintf("… 共 %d 行 · 显示末 %d 行", len(src), toolTailN)))
	}
	return out
}

// turnLines 渲染回合分隔行：◇ 模型 via 供应商 in 总耗时 + 右侧补满横线。
//
// 观感照 crush：整行不着一色（纯灰），横线把这一轮"封口"；窄到放不下时
// 退化成普通折行（不画横线）。
func turnLines(it *FeedItem, w int) []string {
	head := "\u25C7 " + it.Text + " "
	if lipgloss.Width(head)+2 > w {
		var out []string
		for _, ln := range wrapText(strings.TrimRight(head, " "), w-2) {
			out = append(out, "  "+dimStyle.Render(ln))
		}
		return out
	}
	fill := w - 2 - lipgloss.Width(head)
	return []string{"  " + dimStyle.Render(head) + ruleStyle.Render(strings.Repeat("\u2500", fill))}
}

// formatElapsed 把耗时格式化成 letcode 风格（同其 format_elapsed_duration）：
// 123ms / 12s / 2m 30s / 1m。
func formatElapsed(d time.Duration) string {
	ms := d.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	if ms < 60_000 {
		return fmt.Sprintf("%ds", ms/1000)
	}
	total := ms / 1000
	if s := total % 60; s != 0 {
		return fmt.Sprintf("%dm %ds", total/60, s)
	}
	return fmt.Sprintf("%dm", total/60)
}

// withCursor 在最后一行末尾加上闪烁光标。
func withCursor(lines []string, it *FeedItem) []string {
	if !it.Cursor || !it.CursorOn || len(lines) == 0 {
		return lines
	}
	out := make([]string, len(lines))
	copy(out, lines)
	out[len(out)-1] += cursorStyle.Render(" ")
	return out
}

// labeled 渲染"前缀 + 正文（自动折行）"，续行与正文对齐。
// pre 必须是 1 格宽的符号（已过 widthcheck）。
func labeled(pre string, preW, totalW int, text string, preSt, textSt lipgloss.Style) []string {
	textW := totalW - 2 - preW - 1
	if textW < 8 {
		textW = 8
	}
	var out []string
	first := true
	for _, ln := range wrapText(text, textW) {
		if first {
			out = append(out, "  "+preSt.Render(pre)+" "+textSt.Render(ln))
			first = false
		} else {
			out = append(out, "  "+strings.Repeat(" ", preW+1)+textSt.Render(ln))
		}
	}
	if len(out) == 0 {
		out = append(out, "  "+preSt.Render(pre))
	}
	return out
}

// sysLines 系统消息：无前缀、暗色、两格缩进。
func sysLines(totalW int, text string) []string {
	textW := totalW - 2
	if textW < 8 {
		textW = 8
	}
	var out []string
	for _, ln := range wrapText(text, textW) {
		out = append(out, "  "+dimStyle.Render(ln))
	}
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}

// gutterLines 错误专用：色条 + 标签 + 正文 + 可选"怎么办"提示行。
//
// 宽度预算按"前缀实测宽度"算（前缀 = 2 格缩进 + 「┃ 」2 格 + 标签 + 1 空格）。
// 早先按 1 格算 gutter，整行整整溢出 2 格——消息区右缘的滚动条列与右栏
// 被顶着往右挪，用户看到的就是"右侧顶出去了"（见 -feedtest 的宽度断言）。
func gutterLines(c lipgloss.Style, tag string, totalW int, text, detail string) []string {
	gutter := c.Render("\u2503") + " "
	pad := strings.Repeat(" ", lipgloss.Width(tag)+1)
	textW := totalW - lipgloss.Width("  "+gutter+tag+" ")
	if textW < 8 {
		textW = 8
	}
	var out []string
	for i, ln := range wrapText(text, textW) {
		if i == 0 {
			out = append(out, "  "+gutter+c.Render(tag)+" "+textStyle.Render(ln))
		} else {
			out = append(out, "  "+gutter+pad+textStyle.Render(ln))
		}
	}
	if detail != "" {
		for _, ln := range wrapText("提示: "+detail, textW) {
			out = append(out, "  "+gutter+pad+dimStyle.Render(ln))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// -feedtest：无 TTY 的消息区布局自检
// ---------------------------------------------------------------------------

// runFeedTest 渲染一份"消息区样张"到 stdout：
//   - 逐行打印可见文本与宽度（已剥 ANSI），检查折行 / 缩进 / 耗时格式
//   - 打印工具行的原始 ANSI（核对齿轮颜色：成功柔绿 / 失败柔红）
//   - 检查输出里是否残留背景色序列（透明度原则）
func runFeedTest() {
	now := time.Now()
	items := []*FeedItem{
		{Kind: kUser, Text: "查看 demo-lab 里有什么，然后告诉我哪些文件适合做演示素材——这条故意写长一点，用来检查竖条在折行时是否每一行都对齐。"},
		{Kind: kUser, Focused: true, Text: "这一条是点击选中态：左侧细线 │ 变成半块 ▌（加粗）。"},
		{
			Kind: kThought,
			Text: "用户要求读取 demo-lab/README.md 并告诉我第一行是什么。这是一个简单的只读文件访问任务，我应该使用 fs__read 工具来读取文件内容。\n\n" +
				"由于用户只是想知道第一行，**我可以先读取文件**，然后提取第一行返回给用户。\n\n让我先读取文件。",
			StartedAt: now.Add(-3200 * time.Millisecond),
			EndedAt:   now.Add(-1200 * time.Millisecond),
		},
		{
			// 多段正文样张（本段 = 工具调用前的过渡语；
			// 回归点：同回合后续段落必须另起位置，不能并进这一条）
			Kind: kAssistant,
			Text: "当前是 PowerShell，`&&` 写法不被支持，我改用分号分开执行：",
		},
		{
			// 工具卡·失败态：默认展开（stderr 就在输出 tail 里）；
			// Selected=true = 配色样张（失败卡的选中条与齿轮同色：柔红）
			Kind:      kTool,
			ToolName:  "fs__read",
			ToolCall:  "fs__read demo-lab/README.md",
			ToolEnd:   `path does not exist: F:\ComputerLanguage\code\letcode_lab\demo-lab\demo-lab\README.md`,
			ToolState: "failed",
			Selected:  true,
			ToolRaw:   `{"path":"demo-lab/README.md"}`,
			ToolOut:   "path does not exist: F:\\ComputerLanguage\\code\\letcode_lab\\demo-lab\\demo-lab\\README.md",
			// 样张里显式给 true（真实运行时由自动规则展开）
			ToolExpanded: true,
			StartedAt:    now.Add(-3 * time.Second),
			EndedAt:      now.Add(-2 * time.Second),
		},
		{
			// 工具卡·"跑完但退出码非 0"（shell exit 1）：引擎报 completed，
			// ToolBad 由结果摘要 "exit N" 推导 —— 红齿轮 + 默认展开
			Kind:      kTool,
			ToolName:  "shell__exec",
			ToolCall:  "shell__exec ls -la",
			ToolEnd:   "exit 1 · stderr 7 lines",
			ToolState: "completed",
			ToolBad:   true,
			ToolCmd:   "ls -la",
			ToolOut: "Get-ChildItem : 找不到与参数名称“la”匹配的参数。\n" +
				"    + CategoryInfo          : InvalidArgument: (:) [Get-ChildItem]\n" +
				"    + FullyQualifiedErrorId : NamedParameterNotFound,Microsoft.PowerShell.Commands.GetChildItemCommand",
			ToolExpanded: true,
			StartedAt:    now.Add(-900 * time.Millisecond),
			EndedAt:      now.Add(-800 * time.Millisecond),
		},
		{
			// 工具卡·完成态：折叠成一行（▸ 提示可点开）；
			// Selected=true = 键盘选择态样张（选中条=齿轮同色：完成柔绿）
			Kind:      kTool,
			ToolName:  "fs__list",
			ToolCall:  "fs__list .",
			ToolEnd:   "5 entries",
			ToolState: "completed",
			Selected:  true,
			ToolRaw:   `{"path":"."}`,
			StartedAt: now.Add(-2 * time.Second),
			EndedAt:   now.Add(-1900 * time.Millisecond),
		},
		{
			// 工具卡·运行态：自动展开 + 输出 tail（末 8 行 + 省略标记）
			Kind:      kTool,
			ToolName:  "shell__exec",
			ToolCall:  "shell__exec npm test",
			ToolState: "in_progress",
			ToolCmd:   "npm test",
			ToolOut: "> demo-lab@0.0.0 test\n" +
				"> node --test\n\n" +
				"TAP version 13\n" +
				"# Subtest: adds numbers\nok 1 - adds numbers\n" +
				"# Subtest: parses config\nnot ok 2 - parses config\n" +
				"  ---\n  error: 'expected 3, got 2'\n  ...\n" +
				"# tests 2\n# pass 1\n# fail 1",
			ToolExpanded: true,
			StartedAt:    now.Add(-1 * time.Second),
		},
		{
			Kind:      kThought,
			Text:      "让我先读取文件。",
			StartedAt: now.Add(-1500 * time.Millisecond), // 进行中：耗时按"现在"实时计算
		},
		{
			Kind: kAssistant,
			Text: "文件 `demo-lab/README.md` 在该工作区中不存在；我检查了当前目录，找到的是 `README.md`，其第一行内容为：\n\n" +
				"```\n# Demo Lab（工具演示沙盒）\n```",
		},
		{
			// 错误行样张（M4e 回归点）：这条就是实机截图里溢出的那行——
			// 前缀宽度算错时整行会超预算 2 格，把右缘滚动条与右栏顶出去。
			Kind:   kError,
			Text:   "回合出错：引擎返回错误: 参数不合法（-32602）：Usage: /resume <session_id>",
			Detail: "命令的参数没写全——敲 / 打开命令列表，选中回车即可补全参数",
		},
		{
			// 命令护栏提示（M4e）：琥珀 ! 前缀
			Kind: kWarn,
			Text: "「/reasoning」还缺参数：off|none|minimal|low|medium|high|xhigh —— 敲 / 打开命令列表，选中回车即可补全",
		},
		{
			// M3c 队列样张：排队中的用户消息（暗色竖条 + 尾行「· 排队中」；
			// 回合结束出队时原地转正成 kUser，位置不动、不产生第二条）
			Kind: kQueued,
			Text: "这条是回合进行中排队的消息：等当前回合结束后会自动接着发送，出队时原地转正。",
		},
		{Kind: kUsage, Text: "用量 394 / 320000"},
		{Kind: kTurn, Text: turnSummaryLine("Step 3.7 Flash", "guji", 12*time.Second)},
	}

	const w = 76
	var raw []string
	fmt.Println("== 消息区样张（已剥 ANSI；行尾 (w=N) 是可见宽度）==")
	for i, it := range items {
		if i > 0 {
			fmt.Println()
		}
		for _, ln := range renderItem(it, w) {
			raw = append(raw, ln)
			fmt.Printf("%s  (w=%d)\n", stripANSI(ln), lipgloss.Width(ln))
		}
	}

	fmt.Println()
	fmt.Println("== 工具行 / 回合行原始 ANSI（核对颜色：齿轮有彩、菱形无彩）==")
	for _, ln := range raw {
		if strings.Contains(ln, "\u23E3") || strings.Contains(ln, "\u25C7") {
			fmt.Printf("raw: %q\n", ln)
		}
	}

	fmt.Println()
	bad := false
	for _, ln := range raw {
		if hasBackgroundColor(ln) {
			bad = true
		}
	}
	if bad {
		fmt.Println("!! 背景色检查：检测到背景色序列（透明度原则被破坏）")
	} else {
		fmt.Println("OK 背景色检查：未检测到背景色序列")
	}

	// 宽度断言（M4e 回归点）：任何一行超过预算 w，右缘的滚动条列与右栏
	// 就会被"顶着"往右挪——实机上看到的就是"右侧顶出去了"。
	// 错误行前缀「  ┃ 错误 」共 9 格，预算少算一格都会在这里现形。
	fmt.Println()
	wide := 0
	for _, ln := range raw {
		if lw := lipgloss.Width(ln); lw > w {
			wide++
			fmt.Printf("!! 超宽行 w=%d > %d：%s\n", lw, w, stripANSI(ln))
		}
	}
	if wide > 0 {
		fmt.Printf("!! 宽度断言：%d 行超出 %d 格预算\n", wide, w)
		bad = true
	} else {
		fmt.Printf("OK 宽度断言：%d 行全部 ≤ %d 格\n", len(raw), w)
	}

	// 折行规则断言（M4e 打磨）：折行点优先落在空白（词边界）上——英文、路径、
	// 命令名不被拦腰截断；找不到空白时（超长 URL）才退化成硬切。
	fmt.Println()
	wrapLines := wrapText("提示: 当前模型不接受该推理档位——可先 /model 换模型，或用 /reasoning 查看可选值", 30)
	okWord, okWrapW := true, true
	for _, ln := range wrapLines {
		if lipgloss.Width(ln) > 30 {
			okWrapW = false
		}
	}
	if !strings.Contains(strings.Join(wrapLines, "\n"), "/reasoning") {
		okWord = false
	}
	hard := wrapText("https://example.com/a/very/long/path/without/any/space/at/all", 20)
	okHard := len(hard) >= 2 && lipgloss.Width(hard[0]) <= 20
	if !okWord || !okWrapW || !okHard {
		fmt.Printf("!! 折行断言：词边界 %v ｜ 行宽≤30 %v ｜ 超长词硬切 %v\n", okWord, okWrapW, okHard)
		bad = true
	} else {
		fmt.Printf("OK 折行断言：词边界优先 %v ｜ 超长词硬切 %v ｜ 样张 %q\n", okWord, okHard, wrapLines)
	}

	fmt.Println()
	if bad {
		fmt.Println("feedtest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("feedtest: 全部通过")
}

// ---------------------------------------------------------------------------
// -scrolltest：滚动条自检（M4a）
// ---------------------------------------------------------------------------

// runScrollTest 断言滚动条几何，并把样张打到 stdout：
//   - 不溢出：整列留白（空串），拼上去不可见
//   - 溢出：每行 1 格可见宽、无背景色；底部拇指贴底、顶部贴顶、中部悬中
//   - 样张：■=拇指 ┃、·=轨道 │、空格=不溢出留白
//   - View() 集成：加条后行数不变、总宽 ≤ 终端宽（有/无右栏各验一遍）
func runScrollTest() {
	const w, h = 76, 12
	bad := false
	fail := func(format string, a ...any) {
		bad = true
		fmt.Printf("  !! "+format+"\n", a...)
	}

	// ① 不溢出 → 整列留白
	short := NewFeed()
	short.SetSize(w, h)
	short.Append(kUser, "短会话")
	short.Append(kAssistant, "只有两条消息，占不满一屏。")
	col := short.Scrollbar()
	if len(col) != h {
		fail("不溢出：列高 = %d，期望 %d", len(col), h)
	}
	for i, s := range col {
		if s != "" {
			fail("不溢出：第 %d 行应为空串，得到 %q", i, s)
		}
	}

	// ② 溢出 → 每行 1 格、无背景色
	long := NewFeed()
	long.SetSize(w, h)
	for i := 0; i < 30; i++ {
		long.Append(kAssistant, fmt.Sprintf("第 %d 条消息：把内容撑到远超一屏，滚动条才有意义。", i))
	}
	if long.maxScroll() <= 0 {
		fail("溢出样张没撑满一屏（maxScroll=%d），断言不可信", long.maxScroll())
	}
	col = long.Scrollbar()
	if len(col) != h {
		fail("溢出：列高 = %d，期望 %d", len(col), h)
	}
	for i, s := range col {
		if wd := lipgloss.Width(s); wd != 1 {
			fail("溢出：第 %d 行可见宽 = %d，期望 1", i, wd)
		}
		if hasBackgroundColor(s) {
			fail("溢出：第 %d 行带背景色（透明度原则）", i)
		}
	}

	thumbRows := func(c []string) (first, last int) {
		first, last = -1, -1
		for i, s := range c {
			if strings.Contains(stripANSI(s), "\u2503") {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		return first, last
	}
	sketch := func(c []string, label string) string {
		var b strings.Builder
		for _, s := range c {
			p := stripANSI(s)
			switch {
			case p == "":
				b.WriteString(" ")
			case strings.Contains(p, "\u2503"):
				b.WriteString("\u25A0")
			default:
				b.WriteString("\u00B7")
			}
		}
		return label + "：" + b.String()
	}

	// 底部（scroll = 0）
	bf, bl := thumbRows(col)
	if bf < 0 {
		fail("底部：列里找不到拇指")
	} else if bl != h-1 {
		fail("底部：拇指末行 = %d，期望 %d（贴底）", bl, h-1)
	}
	bottom := sketch(col, "底部")

	// 中部（回看半屏）
	long.ScrollBy(long.maxScroll() / 2)
	midCol := long.Scrollbar()
	mf, ml := thumbRows(midCol)
	if mf < 0 {
		fail("中部：列里找不到拇指")
	} else if mf == 0 || ml == h-1 {
		fail("中部：拇指贴边（first=%d last=%d），期望悬在中间", mf, ml)
	}
	midSnap := sketch(midCol, "中部")

	// 顶部（上滚到头）
	long.ScrollBy(long.maxScroll())
	topCol := long.Scrollbar()
	tf, tl := thumbRows(topCol)
	if tf != 0 {
		fail("顶部：拇指首行 = %d，期望 0（贴顶）", tf)
	}
	top := sketch(topCol, "顶部")

	fmt.Println("== 滚动条样张（h=12；■拇指 ┃ / ·轨道 │ / 空格不溢出留白）==")
	fmt.Printf("  %s\n", bottom)
	fmt.Printf("  %s\n", midSnap)
	fmt.Printf("  %s\n", top)
	fmt.Printf("  拇指行：底部 %d-%d ｜ 中部 %d-%d ｜ 顶部 %d-%d\n", bf, bl, mf, ml, tf, tl)
	fmt.Println()

	// ③ View() 集成：行数不变、右块锚定（滚动条钉在消息区右缘）、总宽 ≤ 终端宽。
	// 样张里混进错误行 / 命令护栏 / 工具卡 / 思考块——它们的宽度预算最容易算错，
	// 一旦某行超宽，右缘的滚动条列与右栏会被顶着往右挪（"右侧顶出去了"）。
	for _, panel := range []bool{true, false} {
		vm := model{
			width: 120, height: 30, feed: NewFeed(), status: stIdle, panelOn: panel,
			sessionID: "1234abcd-0000-0000-0000-000000000000", sessTitle: "滚动条集成样张",
			modelLabel: "Step 3.7 Flash", modeID: "default",
			usageUsed: 87300, usageSize: 320000,
		}
		vm.syncLayout()
		vm.feed.Append(kError, "回合出错：引擎返回错误: 参数不合法（-32602）：Usage: /resume <session_id>")
		vm.feed.Append(kWarn, "「/reasoning」还缺参数：off|none|minimal|low|medium|high|xhigh —— 敲 / 打开命令列表")
		vm.feed.Append(kReceipt, "已允许 · shell__exec npm test · 本次会话")
		vm.feed.Append(kThought, "用户要求读取 demo-lab/README.md 并告诉我第一行是什么。这是一个只读文件访问任务，先读再答。")
		vm.feed.Append(kAssistant, "文件 `demo-lab/README.md` 在该工作区中不存在；当前目录下找到的是 `README.md`。")
		tool := vm.feed.Append(kTool, "")
		tool.ToolName, tool.ToolCall, tool.ToolCmd = "shell__exec", "shell__exec npm test", "npm test"
		tool.ToolOut = "TAP version 13\n# Subtest: adds numbers\nok 1 - adds numbers\nnot ok 2 - parses config\n"
		tool.ToolState, tool.ToolExpanded = "in_progress", true
		tool.touch()
		for i := 0; i < 12; i++ {
			vm.feed.Append(kUser, fmt.Sprintf("集成样张第 %d 条：撑出一屏，让滚动条出现。", i))
		}

		content := vm.View().Content
		lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
		okRows := len(lines) == vm.height
		okBar := strings.Contains(stripANSI(content), "\u2503")
		okWidth := true
		for _, ln := range lines {
			if lipgloss.Width(ln) > vm.width {
				okWidth = false
				break
			}
		}
		// 右块锚定：消息区每行的第 feed.width 个显示格是滚动条格（┃ 拇指 / │ 轨道），
		// 右栏分隔「  │ 」钉在它的右边——行一超宽这两处立刻错位。
		// 注意按"显示格"取字符（CJK 占 2 格，rune 下标不等于列号）。
		okAnchor := true
		rows := len(vm.feed.Render())
		for i := 1; i <= rows && i < len(lines); i++ {
			rs := stripANSI(lines[i])
			c, ok := cellAt(rs, vm.feed.width)
			if !ok || (c != '\u2503' && c != '\u2502' && c != ' ') {
				okAnchor = false
				break
			}
			if panel {
				if sep, ok := cellAt(rs, vm.feed.width+3); !ok || sep != '\u2502' {
					okAnchor = false
					break
				}
			}
		}
		if !okRows || !okBar || !okWidth || !okAnchor {
			fail("View(panel=%v)：行数=%v 拇指=%v 宽度≤%d=%v 右块锚定=%v",
				panel, okRows, okBar, vm.width, okWidth, okAnchor)
		}
		if hasBackgroundColor(content) {
			fail("View(panel=%v)：检测到背景色序列", panel)
		}
		fmt.Printf("  View(panel=%v)：行数 %d/%d ｜ 拇指 %v ｜ 宽度合规 %v ｜ 右块锚定 %v\n",
			panel, len(lines), vm.height, okBar, okWidth, okAnchor)
	}

	if bad {
		fmt.Println("scrolltest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("scrolltest: 全部通过")
}

// wrapText 按显示宽度折行（CJK 安全：每个字符按 lipgloss.Width 计宽；
// 显式换行符强制折行）。
//
// 折行点优先落在空白（词边界）上：英文、路径、命令名不会被拦腰截断
// （否则会出现 "…或用 /reasonin" + "g 查看可选值" 这种）；找不到空白时
// 退化为硬折行——超长 URL / 长代码行照样按宽度切开。
func wrapText(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	var lines []string
	var cur []rune
	curW := 0
	flush := func() {
		lines = append(lines, string(cur))
		cur, curW = nil, 0
	}
	for _, r := range s {
		if r == '\n' {
			flush()
			continue
		}
		rw := lipgloss.Width(string(r))
		if curW+rw > width && len(cur) > 0 {
			// 回看最后一个空白：以它为折行点（空白本身丢掉），尾巴搬到
			// 下一行接着排；没有空白（或折出来是空行）才硬切。
			i := lastBlank(cur)
			head := ""
			if i > 0 {
				head = strings.TrimRight(string(cur[:i]), " \t")
			}
			if head == "" {
				flush()
				// 折行点就是这个空白本身（行内没有别的空白）：把它丢掉，
				// 别让它飘到下一行行首（"　/model …" 那种）。
				if r == ' ' || r == '\t' {
					continue
				}
			} else {
				lines = append(lines, head)
				tail := strings.TrimLeft(string(cur[i:]), " \t")
				cur, curW = []rune(tail), lipgloss.Width(tail)
				if curW+rw > width { // 尾巴自己就又满了：它独占一行
					flush()
				}
			}
		}
		cur = append(cur, r)
		curW += rw
	}
	if len(cur) > 0 {
		flush()
	}
	return lines
}

// lastBlank 返回行内最后一个空白（空格 / 制表符）的下标；没有则 -1。
func lastBlank(rs []rune) int {
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i] == ' ' || rs[i] == '\t' {
			return i
		}
	}
	return -1
}

// cellAt 取一行里第 col 个"显示格"上的字符（CJK 按 2 格计；已剥 ANSI）。
// 该列落在双宽字符中间、或行本身不够长时，ok=false。
// 自检断言用它按列号找滚动条格——rune 下标在中文行里不等于列号。
func cellAt(s string, col int) (rune, bool) {
	w := 0
	for _, r := range s {
		if w == col {
			return r, true
		}
		if w > col {
			return 0, false
		}
		w += lipgloss.Width(string(r))
	}
	return 0, false
}

// ansiSeqEnd 返回 s[i:]（i 指向 ESC）这条转义序列的结束下标。
// 只认 UI 里真实会出现的两类：CSI（ESC [ … 终止字母）与 OSC（ESC ] … BEL/ESC\）；
// 其它 ESC 序列按两字节处理（ESC + 一个字符）。
func ansiSeqEnd(s string, i int) int {
	if i+1 >= len(s) {
		return len(s)
	}
	switch s[i+1] {
	case '[': // CSI：参数/中间字节后跟一个终止字母（0x40-0x7E）
		j := i + 2
		for j < len(s) {
			c := s[j]
			if c >= 0x40 && c <= 0x7E {
				return j + 1
			}
			j++
		}
		return len(s)
	case ']': // OSC：BEL 或 ESC \ 收尾
		j := i + 2
		for j < len(s) {
			if s[j] == 0x07 {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
			j++
		}
		return len(s)
	}
	return i + 2
}

// clipLine 把一行硬裁到 w 格以内（最后一道保险，不参与正常排版）：
// 渲染器各自按预算折行，这里兜底——任何一行的超宽都会把右缘的滚动条列
// 与右栏顶着往右挪（用户看到的是"右侧顶出去了"，见 gutterLines 注释）。
//
// ANSI 感知：转义序列整段照抄、不计数；CJK 按 2 格计；被裁时以 … 收尾
// 并补一个 reset（免得半截样式染到行尾）。
func clipLine(s string, w int) string {
	if w <= 1 || lipgloss.Width(s) <= w {
		return s
	}
	var b strings.Builder
	used := 0
	limit := w - 1 // 留一格给 …
	styled := false
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			j := ansiSeqEnd(s, i)
			styled = true
			b.WriteString(s[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := lipgloss.Width(string(r))
		if used+rw > limit {
			break
		}
		b.WriteRune(r)
		used += rw
		i += size
	}
	b.WriteString("\u2026")
	if styled {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}
