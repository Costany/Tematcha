// ui_nav.go —— 工具卡键盘选择态（M2b v2）：Tab 进入，↑↓ 选卡，
// enter 展开/收起，esc / tab 退出。
//
// 设计要点：
//   - enter 与"发送"的冲突不靠重映射解决——而是靠一个显式模式：Tab 进入后
//     enter 归选择态（展开/收起）；打字 / 鼠标点击 / esc / tab 都会退出模式，
//     enter 立刻恢复"发送"。模式开着时状态栏左侧显示「选择工具卡 i/n」、
//     右侧提示行列出键位，不会出现"按了 enter 却不知道发没发"。
//   - 候选卡 = 有可展开内容的工具卡（hasToolBody）——没内容的卡没有
//     "打开看看"的价值；名单每次现场计算（卡片在流式过程中会长出内容）。
//   - 移动选择时调 Feed.ScrollToItem 保证选中卡的头行可见（尽量少打扰：
//     已可见就不动）。
package main

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
)

// cardCandidates 返回参与键盘选择的工具卡（有可展开内容的才算）。
func (m *model) cardCandidates() []*FeedItem {
	var out []*FeedItem
	for _, it := range m.feed.items {
		if it.Kind == kTool && hasToolBody(it) {
			out = append(out, it)
		}
	}
	return out
}

// enterCardNav 进入选择态：优先选"窗口里可见的"最后一张卡（回看历史时
// 不硬跳回底部）；一张都看不见就选最新的一张并滚过去。没有候选卡则不进。
func (m *model) enterCardNav() {
	cands := m.cardCandidates()
	if len(cands) == 0 {
		return
	}
	pick := cands[len(cands)-1]
	for i := len(cands) - 1; i >= 0; i-- {
		if m.feed.ItemVisible(cands[i]) {
			pick = cands[i]
			break
		}
	}
	m.setCardNav(pick)
	m.feed.ScrollToItem(pick)
}

// exitCardNav 退出选择态（清掉选中标记）。
func (m *model) exitCardNav() {
	m.setCardNav(nil)
}

// setCardNav 换当前选中的卡（nil = 退出）：只动标记并让渲染缓存失效。
func (m *model) setCardNav(it *FeedItem) {
	if m.cardNav == it {
		return
	}
	if m.cardNav != nil {
		m.cardNav.Selected = false
		m.cardNav.touch()
	}
	m.cardNav = it
	if it != nil {
		it.Selected = true
		it.touch()
	}
}

// cardNavMove 在候选卡之间移动选择（delta=±1；到头即止，不绕圈）。
func (m *model) cardNavMove(delta int) {
	cands := m.cardCandidates()
	if len(cands) == 0 {
		m.exitCardNav()
		return
	}
	idx := -1
	for i, it := range cands {
		if it == m.cardNav {
			idx = i
			break
		}
	}
	if idx < 0 {
		// 选中的卡不在候选里（理论上不会发生）：重新落位到最后一张
		m.setCardNav(cands[len(cands)-1])
	} else {
		n := idx + delta
		if n < 0 {
			n = 0
		}
		if n > len(cands)-1 {
			n = len(cands) - 1
		}
		m.setCardNav(cands[n])
	}
	if m.cardNav != nil {
		m.feed.ScrollToItem(m.cardNav)
	}
}

// cardNavToggle 展开/收起当前选中的卡（同鼠标点击：置 ToolUserSet，
// 此卡之后不再受"自动三态"影响）。
func (m *model) cardNavToggle() {
	it := m.cardNav
	if it == nil || !hasToolBody(it) {
		return
	}
	it.ToolExpanded = !it.ToolExpanded
	it.ToolUserSet = true
	it.touch()
}

// cardNavPos 当前选中在候选里的位置（1 起；不在候选里返回 0）。
func (m *model) cardNavPos() (int, int) {
	cands := m.cardCandidates()
	for i, it := range cands {
		if it == m.cardNav {
			return i + 1, len(cands)
		}
	}
	return 0, len(cands)
}

// ---------------------------------------------------------------------------
// -navtest：选择态状态机自检（无 TTY / 无引擎）
// ---------------------------------------------------------------------------

// runNavTest 构造一个假消息区，把选择态的路径完整走一遍并断言：
// 进入 / 移动（跳过无内容卡）/ 夹取 / 展开收起 / esc 只退模式（不取消回合）/
// 打字自动退出 / 点击自动退出 / 无候选卡不进模式。任何一条不满足都以非零码退出。
func runNavTest() {
	failed := false
	check := func(name string, cond bool) {
		tag := "PASS"
		if !cond {
			tag = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", tag, name)
	}

	newM := func() model {
		m := model{width: 76, height: 20, feed: NewFeed(), status: stIdle}
		m.feed.SetSize(70, 20)
		return m
	}
	key := func(m model, code rune, text string) model {
		next, _ := m.handleKey(tea.KeyPressMsg{Code: code, Text: text})
		return next.(model)
	}

	m := newM()
	m.feed.Append(kUser, "你好")
	a := m.feed.Append(kTool, "")
	a.ToolName, a.ToolCall, a.ToolRaw, a.ToolState = "fs__read", "fs__read a.txt", `{"path":"a.txt"}`, "completed"
	empty := m.feed.Append(kTool, "") // 没有 rawInput / 输出：不参与选择
	empty.ToolName, empty.ToolCall, empty.ToolState = "fs__list", "fs__list .", "completed"
	c := m.feed.Append(kTool, "")
	c.ToolName, c.ToolCall, c.ToolCmd, c.ToolState = "shell__exec", "shell__exec go test", "go test", "failed"

	// ① tab 进入：默认落在最新的候选（c）
	m = key(m, tea.KeyTab, "")
	check("tab 进入选择态并选中最新候选卡", m.cardNav == c && c.Selected && !a.Selected)

	// ② ↑ 上移：跳到 a（中间的 empty 卡被跳过）
	m = key(m, tea.KeyUp, "")
	check("↑ 移到上一张候选卡（无内容卡被跳过）", m.cardNav == a && a.Selected && !c.Selected)

	// ③ 已在第一张：再 ↑ 夹取不动
	m = key(m, tea.KeyUp, "")
	check("第一张再 ↑ 夹取不动", m.cardNav == a)

	// ④ enter：展开/收起 + 置 ToolUserSet（手选覆盖自动态）
	before := a.ToolExpanded
	m = key(m, tea.KeyEnter, "")
	check("enter 切换展开并置 ToolUserSet", a.ToolExpanded != before && a.ToolUserSet)

	// ⑤ esc：只退选择态；若漏到输入框路径会走取消分支（这里 status 必须保持原样）
	m = key(m, tea.KeyEscape, "")
	check("esc 退出选择态（不取消回合）", m.cardNav == nil && !a.Selected && m.status == stIdle)

	// ⑥ 重新进入后打字：自动退出模式，字符落进输入框
	m = key(m, tea.KeyTab, "")
	m = key(m, 'h', "h")
	check("打字自动退出选择态并写入输入框", m.cardNav == nil && m.input.Text() == "h")

	// ⑦ 选择态里点击：自动退出（点击本身照常处理）
	m = key(m, tea.KeyTab, "")
	clicked, _ := m.handleClick(tea.MouseClickMsg{X: 4, Y: 0, Button: tea.MouseLeft})
	m = clicked.(model)
	check("鼠标点击自动退出选择态", m.cardNav == nil)

	// ⑧ 没有候选卡：tab 不进模式
	m2 := newM()
	m2.feed.Append(kUser, "只有用户消息")
	m2 = key(m2, tea.KeyTab, "")
	check("无候选卡时 tab 不进选择态", m2.cardNav == nil)

	// ⑨ 滚动定位：小窗口 + 长内容里移动选中卡，选中卡头行始终保持可见
	m3 := newM()
	m3.feed.SetSize(70, 6) // 视口只有 6 行
	for i := 0; i < 12; i++ {
		it := m3.feed.Append(kTool, "")
		it.ToolName = "fs__read"
		it.ToolCall = fmt.Sprintf("fs__read file-%d.txt", i)
		it.ToolCmd = "cat file"
		it.ToolState = "completed"
	}
	m3 = key(m3, tea.KeyTab, "") // 进入：默认选窗口里可见的最后一张
	visible := m3.feed.ItemVisible(m3.cardNav)
	for i := 0; i < 12 && visible; i++ {
		m3 = key(m3, tea.KeyUp, "")
		visible = m3.feed.ItemVisible(m3.cardNav)
	}
	check("滚动定位：长列表里移动选中卡保持可见", visible)

	// ⑩ shell 退出码推导：completed 摘要 "exit N"（N≠0）→ ToolBad（红·默认展开）
	m4 := newM()
	m4.tools = map[string]*FeedItem{}
	m4.handleToolUpdate(map[string]any{
		"toolCallId": "t1", "status": "completed", "title": "exit 1 · stderr 7 lines",
	})
	t1 := m4.tools["t1"]
	check("shell exit 1 → ToolBad（红齿轮 + 默认展开）", t1 != nil && t1.ToolBad && t1.ToolExpanded)
	m4.handleToolUpdate(map[string]any{
		"toolCallId": "t2", "status": "completed", "title": "exit 0 · stdout 15 lines",
	})
	t2 := m4.tools["t2"]
	check("shell exit 0 → 非失败（绿·折叠）", t2 != nil && !t2.ToolBad && !t2.ToolExpanded)
	m4.handleToolUpdate(map[string]any{
		"toolCallId": "t3", "status": "failed", "title": "spawn EPERM",
	})
	t3 := m4.tools["t3"]
	check("引擎 failed → ToolBad（照旧）", t3 != nil && t3.ToolBad)

	if failed {
		fmt.Println("navtest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("navtest: 全部通过")
}
