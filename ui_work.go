// ui_work.go —— 工作区与灵动工作行（§11.6 · M10/M11）：输入栏正上方的一行。
//
// 用户口径（2026-09-19）：crush 那样的乱码在闪，后面跟一串**不闪**的点；
// 闪的节奏像本机 pi 的工作指示器；右侧依次是"是不是 thinking"、时长，
// 以及 ↑ token（↓ 已于 2026-09-19 按"太挤了"反馈撤下）——能算就算，
// 算不准标 ≈。乱码速度对齐 crush：20fps（50ms/帧，见 workFastLag）。
// M11 追加口径：整行从底部状态栏搬到输入栏上方（"放上面，长点"），
// 收尾语（"各种输出完毕之类的"）同样落在这里；压缩时这一行只剩进度条 +
// 百分比（见 ui_compact.go）。
//
// 三件套的分工（§11.6 的"趣味必须真实"）：
//   - 乱码块（scrambleBlock）= 纯动效（照 crush internal/ui/anim/anim.go 的
//     availableRunes + 逐帧重掷 + 亮度渐变）；没有信息量，只告诉用户"引擎在动"；
//   - 点（workDots）= 静态锚点，不参与动画（和乱码形成"动-静"对比），
//     但颜色跟随主题渐变（2026-09-19 用户点名"....也要跟随渐变"）；
//   - 状态词 / 时长 / token = 真实信息；token 是客户端估算（协议只给 used/size），
//     整段带 ≈ 前缀，绝不假装精确。
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

const (
	// workScrambleN 乱码块字符数（M11：4 → 8。用户反馈"太短了，长点"；
	// crush 默认 10——整条工作行面积大了，8 格足够抢眼又不喧宾夺主）。
	workScrambleN = 8
	// workDots 乱码后面的静态省略号（用户点名「....,....」：特意不做动画，
	// 闪动只属于乱码块本身，点是锚；颜色跟随主题渐变——见 dotsBlock）。
	workDots = "....,...."
	// workFastLag 工作行动画心跳：50ms/帧（20fps，对齐 crush anim.go 的
	// fps；2026-09-19 用户点名"要像 crush 的乱码一样的速度"）。流式光标
	// 的翻转已改墙钟驱动（main.go 的 cursorAt），不会跟着频闪。
	workFastLag = 50 * time.Millisecond
)

// workCharset 乱码字符集（照 crush 的 availableRunes，去掉非 ASCII 的 £€）。
const workCharset = "0123456789abcdefABCDEF~!@#$%^&*()+=_"

// scrambleBlock 乱码块：N 个字符逐帧整体重掷（crush 的 cycling 区做法），
// 亮度沿块从左到右由暗到亮（主题渐变的亮度缩放）——观感是"在漏电"。
// 确定性：同一 (step, seed) 渲染结果一致（自检可复现）。
func scrambleBlock(step, seed int) string {
	if workScrambleN <= 0 {
		return ""
	}
	base := contextBarGrad(workScrambleN)
	den := workScrambleN - 1
	if den < 1 {
		den = 1
	}
	var b strings.Builder
	for i := 0; i < workScrambleN; i++ {
		h := (step+1)*2654435761 + (seed+i*97)*40503
		if h < 0 {
			h = -h
		}
		r := workCharset[h%len(workCharset)]
		k := 0.5 + 0.8*float64(i)/float64(den)
		b.WriteString(lipgloss.NewStyle().Foreground(scaleBright(base[i], k)).Render(string(r)))
	}
	return b.String()
}

// dotsBlock 静态省略号：字符不重掷（用户点名"不闪的点"），但每个字符按主题
// 渐变上色（2026-09-19 用户点名"....也要跟随渐变"）——亮度曲线与乱码块
// 一致（左暗 → 右亮），两者读起来像一条连续的渐变。确定性：同参可复现。
func dotsBlock() string {
	dots := []rune(workDots)
	n := len(dots)
	if n == 0 {
		return ""
	}
	base := contextBarGrad(n)
	den := n - 1
	if den < 1 {
		den = 1
	}
	var b strings.Builder
	for i, r := range dots {
		k := 0.5 + 0.8*float64(i)/float64(den)
		b.WriteString(lipgloss.NewStyle().Foreground(scaleBright(base[i], k)).Render(string(r)))
	}
	return b.String()
}

// workStateWord 工作行的状态词（"是不是 thinking"那一栏）。
// 取值与颜色沿用 §3 的既有语义（思考中暖色 / 回复中蓝 / 载入会话蓝）。
// 压缩回合不走工作行（工作区显示进度条，见 workStrip）——所以这里没有
// "正在整理上下文"的分支（M11 用户点名：压缩时不要状态词）。
func (m model) workStateWord() string {
	if !m.busy {
		return ""
	}
	switch m.status {
	case stThinking:
		return thinkStyle.Render("思考中")
	case stReplying:
		return replyStyle.Render("回复中")
	case stCancelling:
		return warnStyle.Render("取消中")
	case stLoading:
		return replyStyle.Render("载入会话")
	}
	return dimStyle.Render("忙")
}

// workElapsed 本回合已用时长（回合开始打点；没有起点回 0）。
func (m model) workElapsed() time.Duration {
	if !m.busy || m.turnStart.IsZero() {
		return 0
	}
	return time.Since(m.turnStart)
}

// tokenSpan token 段（客户端估算）：↑ = 最近一次 usage 的 used（上下文里
// 已经发上去的量）。协议只给 used/size，拿不到精确的收发计数——所以整段
// 带 ≈，不装精确。2026-09-19 用户反馈工作行"太挤了，尤其是 token 那里"：
// ↓（本回合吐出字符数÷4）撤下只留 ↑；turnChars 照常累加（将来要恢复
// 显示不用改接线）。
func (m model) tokenSpan() string {
	if m.usageUsed <= 0 {
		return ""
	}
	return "≈↑" + fmtK(m.usageUsed)
}

// workingLine 工作行：乱码块 + 渐变静态点 + 状态词 + 时长 + ≈↑ token。
// 拼装式：缺项自动跳过（没时长就不显示时长段，没 token 就不显示 token 段）。
func (m model) workingLine() string {
	var segs []string
	segs = append(segs, scrambleBlock(m.blinkN, m.turnSeq)+dotsBlock())
	if s := m.workStateWord(); s != "" {
		segs = append(segs, s)
	}
	if d := m.workElapsed(); d > 0 {
		segs = append(segs, dimStyle.Render(formatElapsed(d)))
	}
	if s := m.tokenSpan(); s != "" {
		segs = append(segs, dimStyle.Render(s))
	}
	return strings.Join(segs, dimStyle.Render(" \u00B7 "))
}

// ---------------------------------------------------------------------------
// 工作区（M11 · §11.6）：输入栏上方的一行
// ---------------------------------------------------------------------------

// 收尾行样式档（closeKind）：plain = 常规收尾语（dim）/ ok = 完成收据（柔绿）/
// warn = 取消与未见下降（琥珀）/ err = 出错（柔红）。
const (
	closeNone = iota
	closePlain
	closeOK
	closeWarn
	closeErr
)

// closeStyle 收尾行颜色档 → 样式。
func closeStyle(kind int) lipgloss.Style {
	switch kind {
	case closeOK:
		return toolOKStyle
	case closeWarn:
		return warnStyle
	case closeErr:
		return toolErStyle
	}
	return dimStyle
}

// workStripLines 工作区（M11）：输入栏正上方的一行"引擎在干嘛"。
//   - 忙时（普通回合）= 工作行（乱码 + 静点 + 状态词 + 时长 + ≈token）；
//   - 忙时（压缩回合）= 英文提示 + 定长进度条 + 百分比（见 compactProgressLine）；
//   - 闲时 = 上一回合的收尾行（收尾语 / 压缩收据，handleTurnDone 落好）；
//   - 都没有 → 不占行。
//
// 宽度与输入块对齐（blockWidth），超宽兜底 clipLine 硬裁（ANSI 感知）。
func (m model) workStripLines() []string {
	line := m.workStrip()
	if line == "" {
		return nil
	}
	return []string{"  " + line}
}

// workStrip 工作区那一行的内容（不含缩进；空串 = 不显示）。
func (m model) workStrip() string {
	w := m.blockWidth()
	if m.busy {
		if m.compactReq {
			// 压缩：英文提示 + 定长进度条 + 百分比（用户点名：条不横跨终端、
			// 提示用英文；不放状态词、不放乱码）。窄屏 clipLine 兜底硬裁。
			return clipLine(m.compactProgressLine(), w)
		}
		return clipLine(m.workingLine(), w)
	}
	if m.closeLine != "" {
		return clipLine(closeStyle(m.closeKind).Render(m.closeLine), w)
	}
	return ""
}

// ---------------------------------------------------------------------------
// 回合收尾语（§11.6：替代干巴巴的「回合结束」）
// ---------------------------------------------------------------------------

// 收尾语库：按回合时长分档；同档内按回合序号轮换（连续回合不重词）。
var (
	closingFast  = []string{"秒回", "眼都不眨", "一眨眼"}
	closingShort = []string{"一气呵成", "顺手拈来", "利落收工"}
	closingMid   = []string{"稳稳当当", "一步一个脚印", "细细打磨"}
	closingLong  = []string{"慢工细活", "厚积薄发", "长考出细活"}
)

// closingPhrase 按回合时长挑一句收尾语。
func closingPhrase(d time.Duration, seq int) string {
	var bank []string
	switch {
	case d < 3*time.Second:
		bank = closingFast
	case d < 15*time.Second:
		bank = closingShort
	case d < 60*time.Second:
		bank = closingMid
	default:
		bank = closingLong
	}
	if len(bank) == 0 {
		return "收工"
	}
	if seq < 0 {
		seq = -seq
	}
	return bank[seq%len(bank)]
}

// closingLine 收尾行：<收尾语> · <时长>；取消 / 出错各有措辞（不用喜庆话）。
func closingLine(status string, d time.Duration, seq int) string {
	tail := formatElapsed(d)
	switch status {
	case stCancelled:
		return "中途收兵 · " + tail
	case stError:
		return "半路卡壳 · " + tail
	}
	return closingPhrase(d, seq) + " · " + tail
}

// ---------------------------------------------------------------------------
// -worktest：灵动工作行自检
// ---------------------------------------------------------------------------

// runWorkTest 断言 M10/M11 工作链路：乱码块（宽度 / 逐帧重掷 / 同参可复现 /
// 亮度渐变 / 只前景色）+ 工作行拼装（乱码 + 静态点 + 状态词 + 时长 + ≈token）+
// 收尾语分档与轮换 + 工作区集成（忙=工作行 / 闲=收尾行 / 空态不占行 /
// 状态栏不再重复动态信息）。
func runWorkTest() {
	failed := false
	check := func(name string, cond bool) {
		tag := "PASS"
		if !cond {
			tag = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", tag, name)
	}

	// ① 乱码块：N 格宽、逐帧重掷、同参可复现、多色（亮度渐变）、只前景色
	a0 := scrambleBlock(0, 0)
	colors := map[string]bool{}
	for _, c := range rgbRe.FindAllString(a0, -1) {
		colors[c] = true
	}
	okBlock := lipgloss.Width(a0) == workScrambleN &&
		a0 != scrambleBlock(1, 0) &&
		a0 == scrambleBlock(0, 0) &&
		!hasStrayBackground(a0)
	check("乱码块：8 格宽、逐帧重掷、同参可复现、多色（亮度渐变）、只前景色",
		okBlock && len(colors) >= 3)

	// ② 工作行拼装：乱码 + 静态点 + 状态词 + 时长 + ≈↑ token（↓ 已撤下）
	m := model{width: 110, status: stThinking, busy: true, blinkN: 3, turnSeq: 0,
		turnStart: time.Now().Add(-12 * time.Second), usageUsed: 31800, turnChars: 1648}
	plain := stripANSI(m.workingLine())
	okLine := strings.Contains(plain, workDots) &&
		strings.Contains(plain, "思考中") &&
		strings.Contains(plain, "12s") &&
		strings.Contains(plain, "≈↑31.8k") &&
		!strings.Contains(plain, "↓") && // 2026-09-19：token 只留 ↑
		!hasStrayBackground(m.workingLine())
	check("工作行：乱码+点+思考中+12s+≈↑31.8k（无↓）；只前景色", okLine)

	// ②c 静态点跟随渐变：字符不重掷（同参稳定），但多色（亮度渐变）
	dots := dotsBlock()
	dotColors := map[string]bool{}
	for _, c := range rgbRe.FindAllString(dots, -1) {
		dotColors[c] = true
	}
	okDots := lipgloss.Width(dots) == len(workDots) &&
		dots == dotsBlock() && len(dotColors) >= 3 && !hasStrayBackground(dots)
	check("静态点：不重掷（稳定）、跟随主题渐变（多色）、只前景色", okDots)

	// ②b 缺项自动跳过：没起点 / 没用量 → 只剩乱码 + 点 + 状态词
	mBare := model{status: stReplying, busy: true, blinkN: 1}
	plainBare := stripANSI(mBare.workingLine())
	okBare := strings.Contains(plainBare, workDots) && strings.Contains(plainBare, "回复中") &&
		!strings.Contains(plainBare, "s") && !strings.Contains(plainBare, "\u2191")
	check("拼装式：没起点/没用量时跳过时长与 token 段", okBare)

	// ③ 收尾语：四档选择 + 同档轮换 + 取消 / 出错措辞
	okBuckets := closingPhrase(1*time.Second, 0) == closingFast[0] &&
		closingPhrase(10*time.Second, 1) == closingShort[1] &&
		closingPhrase(30*time.Second, 2) == closingMid[2] &&
		closingPhrase(120*time.Second, 3) == closingLong[0]
	okRotate := closingPhrase(10*time.Second, 0) != closingPhrase(10*time.Second, 1)
	okEnd := closingLine(stDone, 12*time.Second, 0) == "一气呵成 · 12s" &&
		strings.Contains(closingLine(stCancelled, 3*time.Second, 0), "中途收兵") &&
		strings.Contains(closingLine(stError, 3*time.Second, 0), "半路卡壳")
	check("收尾语：四档选择 + 同档轮换 + 取消/出错各有措辞", okBuckets && okRotate && okEnd)

	// ④ 工作区集成：忙时 = 工作行（乱码+点+状态词，宽度不超输入块、只前景色）；
	//    闲时 = 收尾行；空态不占行。
	m2 := model{width: 110, status: stThinking, busy: true, blinkN: 1, turnSeq: 0,
		turnStart: time.Now().Add(-5 * time.Second)}
	strip2 := m2.workStripLines()
	okBusy := len(strip2) == 1 &&
		strings.Contains(stripANSI(strip2[0]), workDots) &&
		strings.Contains(stripANSI(strip2[0]), "思考中") &&
		lipgloss.Width(strip2[0]) <= m2.blockWidth()+2 &&
		!hasStrayBackground(strip2[0])
	m3 := model{width: 110, status: stDone, closeLine: "一气呵成 · 12s", closeKind: closePlain}
	strip3 := m3.workStripLines()
	okIdle := len(strip3) == 1 && strings.Contains(stripANSI(strip3[0]), "一气呵成 · 12s")
	m4 := model{width: 110, status: stIdle}
	okEmpty := len(m4.workStripLines()) == 0
	check("工作区：忙=工作行（不超宽/只前景色）、闲=收尾行、空态不占行",
		okBusy && okIdle && okEmpty)

	// ⑤ 状态栏撤下工作行：动态信息只留在工作区（同一 model 两处对照）
	bar := stripANSI(m2.renderStatusBar())
	okBar := !strings.Contains(bar, workDots) && !strings.Contains(bar, "思考中") &&
		lipgloss.Width(m2.renderStatusBar()) == 110
	check("状态栏撤下工作行（动态信息只留在工作区）", okBar)

	// 样张（人眼核对）
	fmt.Printf("  工作行样张：%s\n", stripANSI(m2.workingLine()))
	fmt.Printf("  工作区样张：%s\n", stripANSI(strip2[0]))
	fmt.Printf("  收尾行样张：%s\n", stripANSI(strip3[0]))
	fmt.Printf("  状态栏样张：%s\n", stripANSI(m2.renderStatusBar()))

	if failed {
		fmt.Println("worktest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("worktest: 全部通过")
}
