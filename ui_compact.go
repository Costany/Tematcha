// ui_compact.go —— 压缩动画（M5d · §8）：/compact 的客户端可见状态。
//
// 协议事实（letcode src/acp/driver.rs + projection.rs 取证）：
//   - /compact 是一个 ACP 命令回合：成功 = 正常 end_turn；失败回 JSON-RPC
//     错误（如 "letcode could not compact the session context: context is
//     already within budget"）；压缩过程引擎不发任何进度通知（Compaction
//     事件不进 projection）——所以"进行中"只能由客户端自己模拟。
//   - 完成信号 = usage 下降：used 明显变小（≥ compactDropPct 个百分点）→
//     上下文条从旧值缓动到新值（500ms 心跳驱动）+ 落一条回执；引擎自发
//     压缩走同一条路径，只是回执措辞不同。
package main

import (
	"fmt"
	"image/color"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

const (
	// compactDropPct 触发"压缩完成"动画/回执的最小下降幅度（个百分点）。
	compactDropPct = 5
	// compactBarW 状态栏"跳跃小条"的宽度（格）。
	compactBarW = 8
)

// isCompactCmd 输入是否正好是 /compact（引擎广告命令，无参数）。
func isCompactCmd(text string) bool {
	return strings.TrimSpace(text) == "/compact"
}

// compactScanBar 压缩条（M6 升级）：8 格主题渐变底 + 一条来回扫描的高光带。
// 手艺来自 study\compact-bar 学习项目：渐变色板走 lipgloss.Blend1D（CIELAB 插值），
// 高光带用亮度缩放——压暗底色、点亮光心；位置由 500ms 心跳的 blinkN 驱动。
// 宽度恒为 compactBarW、只用前景色（透明度原则）；mono 主题下自动变全灰。
func compactScanBar(step int) string {
	n := compactBarW
	base := contextBarGrad(n) // 主题渐变（A→B→C；结果按主题名缓存）
	period := 2*n - 2
	if period <= 0 {
		period = 1
	}
	pos := step % period
	if pos >= n {
		pos = period - pos
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		k := 0.34 // 底色：压暗，让光带跳出来
		switch d := i - pos; {
		case d == 0:
			k = 1.0 // 光心：原亮度
		case d == 1 || d == -1:
			k = 0.66 // 光晕：半亮
		}
		b.WriteString(lipgloss.NewStyle().Foreground(scaleBright(base[i], k)).Render("\u2588"))
	}
	return b.String()
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

// compactStateText 压缩进行中的状态词（占引擎状态位）。
func (m model) compactStateText() string {
	return warnStyle.Render("正在整理上下文") + " " + compactScanBar(m.blinkN)
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
	m.compactSawDrop = true
	// 回执：手动 /compact 的回合用直述；其它（引擎自发整理）加注来源。
	if m.busy && m.compactReq {
		m.feed.Append(kSys, "上下文已压缩："+fmtK(oldUsed)+" \u2192 "+fmtK(newUsed))
	} else {
		m.feed.Append(kSys, "上下文已压缩（引擎自动整理）："+fmtK(oldUsed)+" \u2192 "+fmtK(newUsed))
	}
}

// resetUsage 会话切换（/resume 载入）时清用量：避免把"换了一条会话"的
// 数值跳变误判成压缩完成。
func (m *model) resetUsage() {
	m.usageUsed, m.usageSize = 0, 0
	m.barPct, m.barTo, m.barAnim = 0, 0, false
}

// ---------------------------------------------------------------------------
// -compacttest：压缩动画自检
// ---------------------------------------------------------------------------

// runCompactTest 断言 M5d 压缩链路：/compact 识别 / 状态栏瞬态 / usage 下降
// 的缓动与回执 / 小幅噪声不误报 / 回合收尾措辞 / 错误指纹。
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

	// ① /compact 识别：beginTurn 置压缩态；普通消息复位
	m := model{width: 100, feed: NewFeed(), status: stIdle}
	nm, _ := m.beginTurn("/compact")
	m = nm
	okSet := m.compactReq && m.busy
	nm, _ = m.beginTurn("你好")
	m = nm
	check("识别：beginTurn(/compact) 置压缩态；普通消息立即复位", okSet && !m.compactReq)

	// ② 状态栏瞬态：正在整理上下文 + 跳跃小条；宽度守恒、无背景色
	ms := model{width: 110, busy: true, compactReq: true, status: stThinking, blinkN: 2}
	line := ms.renderStatusBar()
	okBar := strings.Contains(stripANSI(line), "正在整理上下文") &&
		strings.Contains(line, "\u2588") &&
		lipgloss.Width(line) == 110 && !hasBackgroundColor(line)
	bar0, bar4 := compactScanBar(0), compactScanBar(4)
	okScan := lipgloss.Width(bar0) == compactBarW && bar0 != bar4
	check("瞬态：状态栏「正在整理上下文」+ 扫描条（8 格、逐帧移动、宽度守恒）",
		okBar && okScan)

	// M6：扫描条 = 主题渐变（≥3 种颜色）+ 光带逐格移动（同格亮度随帧变化）
	{
		colors := map[string]bool{}
		for _, c := range rgbRe.FindAllString(compactScanBar(0), -1) {
			colors[c] = true
		}
		okGrad := len(colors) >= 3 // 8 格渐变（底色 + 光带三段亮度）应出多色

		moved := false
		c0 := rgbRe.FindAllString(compactScanBar(0), -1)
		c4 := rgbRe.FindAllString(compactScanBar(4), -1)
		for i := 0; i < len(c0) && i < len(c4); i++ {
			if c0[i] != c4[i] {
				moved = true
				break
			}
		}
		check("渐变：8 格扫描条为多色渐变 + 光带逐格移动（亮度随位置变化）", okGrad && moved)
	}

	// ③ usage 明显下降：缓动 + 回执（手动措辞）；心跳推进到目标
	md := model{width: 110, feed: NewFeed(), status: stReplying, busy: true, compactReq: true,
		usageUsed: 87300, usageSize: 320000}
	md.feed.SetSize(94, 20)
	md.handleUsageUpdate(map[string]any{"used": float64(12400), "size": float64(320000)})
	okDropStart := md.barAnim && md.barPct == 27 && md.barTo == 3 &&
		strings.Contains(feedTexts(md), "上下文已压缩：87.3k \u2192 12.4k") &&
		!strings.Contains(feedTexts(md), "自动整理") &&
		strings.Contains(stripANSI(md.renderContextBar()), "27%")
	for i := 0; i < 20; i++ {
		md.stepBarAnim()
	}
	okDropEnd := !md.barAnim && md.barPct == 3 && strings.Contains(stripANSI(md.renderContextBar()), "3%")
	check("下降：27%→3% 缓动（心跳推进到目标）+ 回执「上下文已压缩：87.3k → 12.4k」",
		okDropStart && okDropEnd)

	// ④ 小幅噪声（4 个百分点）不误报：不动画、无回执
	mn := model{width: 110, feed: NewFeed(), status: stReplying, busy: true,
		usageUsed: 50000, usageSize: 100000}
	mn.feed.SetSize(94, 20)
	mn.handleUsageUpdate(map[string]any{"used": float64(46000), "size": float64(100000)})
	check("噪声：4 个百分点下降不动画、无回执（直接显示 46%）",
		!mn.barAnim && countKind(mn, kSys) == 0 && strings.Contains(stripANSI(mn.renderContextBar()), "46%"))

	// ⑤ 引擎自发压缩（非 /compact 回合）：回执加注来源
	ma := model{width: 110, feed: NewFeed(), status: stReplying, busy: true,
		usageUsed: 80000, usageSize: 100000}
	ma.feed.SetSize(94, 20)
	ma.handleUsageUpdate(map[string]any{"used": float64(10000), "size": float64(100000)})
	check("自发：非 /compact 回合的下降 → 「上下文已压缩（引擎自动整理）」",
		ma.barAnim && strings.Contains(feedTexts(ma), "引擎自动整理"))

	// ⑥ 回合收尾：手动 /compact 未见下降给说明；取消的不算完成
	mt := model{width: 100, feed: NewFeed(), status: stThinking, busy: true, compactReq: true}
	mt.feed.SetSize(94, 20)
	nt, _ := mt.handleTurnDone(turnDoneMsg{reason: "end_turn"})
	mt = nt.(model)
	okEnd := strings.Contains(feedTexts(mt), "压缩请求已完成（未观察到用量下降）") && !mt.compactReq
	mc := model{width: 100, feed: NewFeed(), status: stThinking, busy: true, compactReq: true}
	mc.feed.SetSize(94, 20)
	nc, _ := mc.handleTurnDone(turnDoneMsg{reason: "cancelled"})
	mc = nc.(model)
	okCancel := !strings.Contains(feedTexts(mc), "压缩请求已完成")
	check("收尾：未见下降 → 一句说明；取消的回合不算完成（且状态复位）", okEnd && okCancel)

	// ⑦ 错误指纹：引擎拒绝压缩时有"怎么办"提示
	hint := engineErrorHint("letcode could not compact the session context: context is already within budget")
	check("指纹：could not compact → 有本地提示", strings.Contains(hint, "压缩"))

	if failed {
		fmt.Println("compacttest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("compacttest: 全部通过")
}
