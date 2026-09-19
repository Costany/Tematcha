// ui_compact.go —— 压缩（M5d 起 · §8）：/compact 的客户端可见状态。
//
// 协议事实（letcode src/acp/driver.rs + projection.rs 取证）：
//   - /compact 是一个 ACP 命令回合：成功 = 正常 end_turn；失败回 JSON-RPC
//     错误（如 "letcode could not compact the session context: context is
//     already within budget"）；压缩过程引擎不发任何进度通知（Compaction
//     事件不进 projection）——所以"进行中"只能由客户端自己模拟。
//   - 完成信号 = usage 下降：used 明显变小（≥ compactDropPct 个百分点）→
//     上下文条从旧值缓动到新值 + 进度条跳 100% + 回合结束收成一行收据。
//
// M11（2026-09-19 用户点菜）：压缩可视化从消息区搬进"工作区"（输入栏上方，
// 见 ui_work.go 的 workStripLines）——
//   - 进行中 = 英文提示 + 定长进度条 + 百分比：不放状态词、不放乱码；
//   - 进度条从左往右填充（不再是一道来回扫的光）：填充段 = 主题渐变，
//     未填充 = 暗格 ░；定长 compactBarCells 格、不横跨终端（2026-09-19
//     用户点菜"跟 Claude Code 那样长就行了"）；
//   - 进度是模拟值：随时间渐近推进、封顶 compactProgCap，观察到用量下降时
//     跳 100%；回合结束时工作区收成一行结论（收据 / 未见下降 / 取消 / 失败）。
package main

import (
	"fmt"
	"image/color"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

const (
	// compactDropPct 触发"压缩完成"动画/回执的最小下降幅度（个百分点）。
	compactDropPct = 5
	// compactProgCap 模拟进度的封顶值（留 4 个百分点给"完成跳 100%"）。
	compactProgCap = 96
	// compactProgT 渐近曲线的时间常数（秒）：p(t) = cap · t / (t + T)。
	// 前几秒涨得快、越往后越慢——引擎不给进度时的保守模拟，不装精确。
	compactProgT = 5.0
	// compactBarCells 压缩进度条格数（定长）：用户点名"不要让压缩条横跨整个
	// 终端，跟 Claude Code 那样长就行了"——条前放英文提示，整行约 55 格。
	compactBarCells = 30
	// compactHint 压缩进行中的提示（用户点名"提示用英文"）。
	compactHint = "Compacting context"
)

// isCompactCmd 输入是否正好是 /compact（引擎广告命令，无参数）。
func isCompactCmd(text string) bool {
	return strings.TrimSpace(text) == "/compact"
}

// compactProgAt 压缩进度模拟：p(t) = cap · t / (t + T)，封顶 compactProgCap。
// 纯函数（给时长回百分比），自检可复现。真实完成由 noteUsageDrop 跳 100%。
func compactProgAt(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	t := d.Seconds()
	p := int(float64(compactProgCap) * t / (t + compactProgT))
	if p < 0 {
		p = 0
	}
	if p > compactProgCap {
		p = compactProgCap
	}
	return p
}

// stepCompactProg 心跳推进：压缩回合进行中按已用时长重算进度（时间函数，
// 与心跳频率无关）；观察到用量下降后停格 100%（compactDone）。
func (m *model) stepCompactProg() {
	if !m.compactReq || m.compactDone || m.turnStart.IsZero() {
		return
	}
	m.compactProg = compactProgAt(time.Since(m.turnStart))
}

// compactProgressLine 压缩进度条（工作区 · §8）：英文提示 + 定长进度条 + 百分比。
// 定长 compactBarCells 格（不随终端宽度横跨——用户点名"跟 Claude Code 那样长
// 就行了"）；填充段 = 主题渐变（A→B→C）逐格取色；未填充 = 暗格 ░；右端百分比
// （100% 转柔绿）。只前景色（透明度原则）——mono 主题下自然退化为灰阶。
func (m model) compactProgressLine() string {
	p := m.compactProg
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	suffix := fmt.Sprintf(" %3d%%", p) // "  0%".."100%"
	barW := compactBarCells
	fill := barW * p / 100
	grad := contextBarGrad(barW)
	var b strings.Builder
	for i := 0; i < barW; i++ {
		if i < fill {
			b.WriteString(lipgloss.NewStyle().Foreground(grad[i]).Render("█"))
		} else {
			b.WriteString(ruleStyle.Render("░"))
		}
	}
	st := dimStyle
	if p >= 100 {
		st = toolOKStyle
	}
	return dimStyle.Render(compactHint) + "  " + b.String() + st.Render(suffix)
}

// scaleBright 把颜色按系数缩放亮度（k<1 压暗、k>1 提亮，色相不变）。
func scaleBright(c color.Color, k float64) color.Color {
	r, g, bl, a := c.RGBA()
	f := func(v uint32) uint8 {
		x := float64(v) * k
		if x > 65535 {
			x = 65535
		}
		return uint8(x / 256)
	}
	return color.RGBA{R: f(r), G: f(g), B: f(bl), A: uint8(a / 256)}
}

// pctOf used/size → 0..100（size<=0 时回 0）。
func pctOf(used, size int64) int {
	if size <= 0 {
		return 0
	}
	pct := int(used * 100 / size)
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// shownPct 上下文条当前要显示的百分比：动画中 = barPct（缓动中），平时现算。
func (m model) shownPct() int {
	if m.barAnim {
		return m.barPct
	}
	return pctOf(m.usageUsed, m.usageSize)
}

// stepBarAnim 每 500ms 心跳推进一步：把显示值往目标缓动（距离的 1/4，至少 1）。
func (m *model) stepBarAnim() {
	if !m.barAnim {
		return
	}
	diff := m.barTo - m.barPct
	if diff == 0 {
		m.barAnim = false
		return
	}
	step := diff / 4
	if step == 0 {
		if diff > 0 {
			step = 1
		} else {
			step = -1
		}
	}
	m.barPct += step
	if m.barPct == m.barTo {
		m.barAnim = false
	}
}

// noteUsageDrop 观察一次用量变化：明显下降 = 压缩完成（手动 /compact 或引擎
// 自发整理）。调用方（handleUsageUpdate）已把新值写进 usageUsed/usageSize。
func (m *model) noteUsageDrop(oldUsed, oldSize, newUsed, newSize int64) {
	if m.loading || oldSize <= 0 || newSize <= 0 || oldUsed <= 0 {
		return
	}
	from, to := pctOf(oldUsed, oldSize), pctOf(newUsed, newSize)
	if from-to < compactDropPct {
		return
	}
	// 动画：显示值停在旧位置，由心跳缓动到新值。
	m.barAnim = true
	m.barPct = from
	m.barTo = to
	receipt := "上下文已压缩：" + fmtK(oldUsed) + " \u2192 " + fmtK(newUsed)
	if m.compactReq {
		// 手动 /compact：进度条跳 100%，收据留到回合结束落成工作区收尾行
		//（handleTurnDone），消息区不留两份记录。
		m.compactSawDrop = true
		m.compactDone = true
		m.compactProg = 100
		m.compactReceipt = receipt
		return
	}
	// 引擎自发整理（不在 /compact 回合里）：消息区留一行记录（信息价值）。
	m.feed.Append(kSys, "上下文已压缩（引擎自动整理）："+fmtK(oldUsed)+" \u2192 "+fmtK(newUsed))
}

// resetUsage 会话切换（/resume 载入）时清用量：避免把"换了一条会话"的
// 数值跳变误判成压缩完成；顺便清掉压缩进度状态（跨会话不残留）。
func (m *model) resetUsage() {
	m.usageUsed, m.usageSize = 0, 0
	m.barPct, m.barTo, m.barAnim = 0, 0, false
	m.compactSawDrop, m.compactDone, m.compactProg, m.compactReceipt = false, false, 0, ""
}

// ---------------------------------------------------------------------------
// -compacttest：压缩进度自检
// ---------------------------------------------------------------------------

// runCompactTest 断言 M5d/M11 压缩链路：/compact 识别（进度条走工作区、不再
// 进消息区）/ 进度模拟函数（单调、封顶）/ 工作区进度条（左→右填充 + 百分比 +
// 不放状态词与乱码）/ 心跳推进与完成跳 100% / usage 下降的上下文条缓动与收据 /
// 小幅噪声不误报 / 自发压缩走消息区记录 / 收尾四态 / 错误指纹。
func runCompactTest() {
	failed := false
	check := func(name string, cond bool) {
		tag := "PASS"
		if !cond {
			tag = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", tag, name)
	}

	feedTexts := func(m model) string {
		var b strings.Builder
		for _, it := range m.feed.items {
			b.WriteString(it.Text)
			b.WriteString("\n")
		}
		return b.String()
	}
	countKind := func(m model, kind int) int {
		n := 0
		for _, it := range m.feed.items {
			if it.Kind == kind {
				n++
			}
		}
		return n
	}

	// ① /compact 识别：beginTurn 只置压缩回合状态（进度条显示在工作区），
	// 不再落消息区条目；普通消息复位。
	m := model{width: 100, feed: NewFeed(), status: stIdle}
	nm, _ := m.beginTurn("/compact")
	m = nm
	okSet := m.compactReq && m.busy && m.compactProg == 0 && !m.compactSawDrop &&
		!m.compactDone && m.feed.Len() == 0 && !m.turnStart.IsZero()
	nm, _ = m.beginTurn("你好")
	m = nm
	check("识别：beginTurn(/compact) 置压缩回合（进度条走工作区、消息区零条目）；普通消息复位",
		okSet && !m.compactReq && m.feed.Len() == 0)

	// ② 进度模拟：0 起步、随时间单调增、封顶 compactProgCap（时间渐近曲线）
	p0 := compactProgAt(0)
	p3 := compactProgAt(3 * time.Second)
	p20 := compactProgAt(20 * time.Second)
	pLong := compactProgAt(10 * time.Minute)
	check("进度模拟：0 起步、单调增、封顶 96（时间渐近曲线）",
		p0 == 0 && p3 > 0 && p3 < p20 && p20 < pLong && pLong <= compactProgCap)

	// ③ 工作区进度条：1 行、英文提示 + 定长条 + 百分比、不放状态词/乱码/静点；
	// 只前景色。定长 = 同样内容在宽/窄终端下条格数不变（用户点名"不要让压缩条
	// 横跨整个终端，跟 Claude Code 那样长就行了"）。
	mp := model{width: 110, status: stThinking, busy: true, compactReq: true, compactProg: 42}
	strip := mp.workStripLines()
	plain := ""
	if len(strip) == 1 {
		plain = stripANSI(strip[0])
	}
	barCells := func(width int) int {
		mm := model{width: width, busy: true, compactReq: true, compactProg: 42}
		ls := mm.workStripLines()
		if len(ls) != 1 {
			return -1
		}
		s := stripANSI(ls[0])
		return strings.Count(s, "█") + strings.Count(s, "░")
	}
	okBar := len(strip) == 1 &&
		strings.HasPrefix(plain, "  "+compactHint) && // 英文提示在前（用户点名）
		strings.Contains(plain, "42%") &&
		strings.Contains(plain, "█") && strings.Contains(plain, "░") &&
		!strings.Contains(plain, workDots) &&
		!strings.Contains(plain, "思考中") && !strings.Contains(plain, "回复中") &&
		!strings.Contains(plain, "正在整理") &&
		lipgloss.Width(strip[0]) <= mp.blockWidth()+2 &&
		!hasBackgroundColor(strip[0]) &&
		barCells(110) == compactBarCells && barCells(80) == compactBarCells // 定长：不随宽度伸缩
	check("工作区进度条：英文提示 + 定长条 + 百分比、无状态词/乱码/静点、只前景色", okBar)

	// ③b 填充随进度增长：p=80 的 █ 比 p=20 多；p=100 无未填充格；p=0 无填充格
	countBlock := func(p int) int {
		mm := model{width: 110, busy: true, compactReq: true, compactProg: p}
		ls := mm.workStripLines()
		if len(ls) != 1 {
			return -1
		}
		return strings.Count(ls[0], "\u2588")
	}
	c20, c80, c100, c0 := countBlock(20), countBlock(80), countBlock(100), countBlock(0)
	stripFull := ""
	if ls := (model{width: 110, busy: true, compactReq: true, compactProg: 100}).workStripLines(); len(ls) == 1 {
		stripFull = stripANSI(ls[0])
	}
	okFill := c0 == 0 && c20 > 0 && c80 > c20 && c100 > c80 &&
		stripFull != "" && !strings.Contains(stripFull, "\u2591")
	check("填充语义：p 越大填充越长（20<80<100）、p=0 无填充、p=100 全填满", okFill)

	// ④ 心跳推进：按已用时长重算；完成（compactDone）后停格 100%
	ms := model{width: 110, busy: true, compactReq: true, turnStart: time.Now().Add(-6 * time.Second)}
	ms.stepCompactProg()
	okStep := ms.compactProg > 0 && ms.compactProg <= compactProgCap
	ms.compactDone, ms.compactProg = true, 100
	ms.stepCompactProg()
	okStep = okStep && ms.compactProg == 100
	check("心跳推进：按已用时长重算进度；完成后停格 100%", okStep)

	// ⑤ usage 明显下降：进度跳 100%（条填满）、收据落账、上下文条 27%→3% 缓动、
	// 不额外落消息区行
	md := model{width: 110, feed: NewFeed(), status: stReplying, busy: true, compactReq: true,
		usageUsed: 87300, usageSize: 320000}
	md.handleUsageUpdate(map[string]any{"used": float64(12400), "size": float64(320000)})
	okDrop := md.barAnim && md.barPct == 27 && md.barTo == 3 &&
		md.compactSawDrop && md.compactDone && md.compactProg == 100 &&
		md.compactReceipt == "上下文已压缩：87.3k \u2192 12.4k" &&
		countKind(md, kSys) == 0 &&
		strings.Contains(stripANSI(md.renderContextBar()), "27%")
	strip100 := stripANSI(strings.Join(md.workStripLines(), "\n"))
	okDrop = okDrop && strings.Contains(strip100, "100%") && !strings.Contains(strip100, "\u2591")
	for i := 0; i < 20; i++ {
		md.stepBarAnim()
	}
	okDrop = okDrop && !md.barAnim && md.barPct == 3 &&
		strings.Contains(stripANSI(md.renderContextBar()), "3%")
	check("完成：usage 下降 → 进度跳 100%、收据落账、上下文条 27%→3% 缓动、无重复消息行", okDrop)

	// ⑥ 小幅噪声（4 个百分点）不误报：不动画、无回执、进度条仍在跑
	mn := model{width: 110, feed: NewFeed(), status: stReplying, busy: true, compactReq: true,
		usageUsed: 50000, usageSize: 100000}
	mn.handleUsageUpdate(map[string]any{"used": float64(46000), "size": float64(100000)})
	check("噪声：4 个百分点下降不动画、无回执、进度条仍在跑",
		!mn.barAnim && !mn.compactSawDrop && !mn.compactDone && mn.compactProg == 0 &&
			countKind(mn, kSys) == 0 && strings.Contains(stripANSI(mn.renderContextBar()), "46%"))

	// ⑦ 引擎自发压缩（非 /compact 回合）：消息区留一行记录；不碰进度条状态
	ma := model{width: 110, feed: NewFeed(), status: stReplying, busy: true,
		usageUsed: 80000, usageSize: 100000}
	ma.handleUsageUpdate(map[string]any{"used": float64(10000), "size": float64(100000)})
	check("自发：非 /compact 回合的下降 → kSys「上下文已压缩（引擎自动整理）」记录",
		ma.barAnim && strings.Contains(feedTexts(ma), "引擎自动整理") && !ma.compactDone)

	// ⑧ 回合收尾四态（都收进工作区收尾行）：
	//    有下降 → ✓ 收据（绿）；未见下降 → 琥珀说明；取消 / 失败各有措辞。
	mt := model{width: 100, feed: NewFeed(), status: stThinking, busy: true, compactReq: true,
		compactSawDrop: true, compactReceipt: "上下文已压缩：87.3k \u2192 12.4k"}
	nt, _ := mt.handleTurnDone(turnDoneMsg{reason: "end_turn"})
	mt = nt.(model)
	okReceipt := mt.closeKind == closeOK && mt.closeLine == "\u2713 上下文已压缩：87.3k \u2192 12.4k" &&
		!mt.compactReq && len(mt.workStripLines()) == 1 &&
		strings.Contains(stripANSI(mt.workStripLines()[0]), "上下文已压缩")

	mn2 := model{width: 100, feed: NewFeed(), status: stThinking, busy: true, compactReq: true}
	nn2, _ := mn2.handleTurnDone(turnDoneMsg{reason: "end_turn"})
	mn2 = nn2.(model)
	okNoDrop := mn2.closeKind == closeWarn && strings.Contains(mn2.closeLine, "未观察到用量下降") &&
		!mn2.compactReq

	mc := model{width: 100, feed: NewFeed(), status: stThinking, busy: true, compactReq: true}
	nc, _ := mc.handleTurnDone(turnDoneMsg{reason: "cancelled"})
	mc = nc.(model)
	okCancel := mc.closeKind == closeWarn && strings.Contains(mc.closeLine, "压缩已取消")

	mx := model{width: 100, feed: NewFeed(), status: stThinking, busy: true, compactReq: true}
	nx, _ := mx.handleTurnDone(turnDoneMsg{err: fmt.Errorf("letcode could not compact the session context: context is already within budget")})
	mx = nx.(model)
	okFail := mx.closeKind == closeErr && mx.closeLine == "压缩失败" &&
		strings.Contains(feedTexts(mx), "回合出错")
	check("收尾：下降→✓收据(绿) / 未下降→说明(琥珀) / 取消 / 失败(红)；都收在工作区",
		okReceipt && okNoDrop && okCancel && okFail)

	// ⑨ 错误指纹：引擎拒绝压缩时有"怎么办"提示
	hint := engineErrorHint("letcode could not compact the session context: context is already within budget")
	check("指纹：could not compact → 有本地提示", strings.Contains(hint, "压缩"))

	// ⑩ /compact 不回显（用户点名"不要出现 /compact 的字样留在上面"）：submit
	// 后消息区零条目、可视化全在工作区；localEcho 仍登记（引擎回显被消费，
	// 不落屏）。busy 时不入队（排队会以 kQueued 留痕），给等待提示。对照：
	// 普通消息照常回显（别把 /compact 的特殊逻辑溢出去）。
	em := model{width: 100, feed: NewFeed(), status: stIdle}
	em.input.SetText("/compact")
	sm2, _ := em.submit()
	em = sm2.(model)
	okNoEcho := em.compactReq && em.busy && em.feed.Len() == 0 && em.localEcho == "/compact"

	bm := model{width: 100, feed: NewFeed(), status: stReplying, busy: true}
	bm.input.SetText("/compact")
	bm2, _ := bm.submit()
	bm = bm2.(model)
	okBusy := !bm.compactReq && len(bm.queue) == 0 && bm.feed.Len() == 1 &&
		strings.Contains(feedTexts(bm), "再压缩")

	nm3 := model{width: 100, feed: NewFeed(), status: stIdle}
	nm3.input.SetText("你好")
	nm4, _ := nm3.submit()
	nm3 = nm4.(model)
	okEcho := countKind(nm3, kUser) == 1 && nm3.localEcho == "你好"
	check("submit：/compact 不回显（localEcho 仍登记）；busy 时提示等待不入队；普通消息照常回显",
		okNoEcho && okBusy && okEcho)

	// 样张（人眼核对）
	fmt.Println("  压缩进度条样张（p=8 / p=42 / p=88 / p=100）：")
	for _, p := range []int{8, 42, 88, 100} {
		mm := model{width: 110, busy: true, compactReq: true, compactProg: p}
		ls := mm.workStripLines()
		if len(ls) == 1 {
			fmt.Printf("    %s\n", stripANSI(ls[0]))
		}
	}

	if failed {
		fmt.Println("compacttest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("compacttest: 全部通过")
}
