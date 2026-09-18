// ui_history.go —— 输入历史（M4c · §6）
//
// 目标：↑↓ 召回发送过的内容（shell 手感），但不抢消息区的滚动键：
//
//	输入框空着   → ↑ 开始翻历史（最新一条起）
//	正在翻历史   → ↑ 更早 / ↓ 更近；↓ 越过最新一条时还原"开翻前的草稿"
//	输入框有草稿 → ↑↓ 照旧滚消息区（历史不介入）
//	翻出历史后手改一个字 → 退出浏览（之后 ↑↓ 回滚动）
//
// 滚轮 / PgUp / PgDn 全程不受影响。历史只活在内存里（不落盘；跨会话不保留）。
package main

import (
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// histPush 记一条已发送文本：空串不入栈；与上一条重复也不入栈（连续重复去重）。
// 发送之后浏览态一律复位。
func (m *model) histPush(text string) {
	text = strings.TrimSpace(text)
	if text != "" {
		if n := len(m.sent); n == 0 || m.sent[n-1] != text {
			m.sent = append(m.sent, text)
		}
	}
	m.histOn, m.histIdx, m.histDraft = false, 0, ""
}

// histMove 翻历史：up=true 往更早，false 往更近。返回 true = 这次按键被历史消费
// （调用方不要再当滚动键处理）。
func (m *model) histMove(up bool) bool {
	if len(m.sent) == 0 {
		return false
	}
	if !m.histOn {
		// 还没在翻：只有"空输入 + ↑"才开翻（有草稿时把 ↑ 留给滚动）
		if !up || m.input.Text() != "" {
			return false
		}
		m.histOn = true
		m.histDraft = ""
		m.histIdx = len(m.sent) - 1
		m.input.SetText(m.sent[m.histIdx])
		return true
	}
	if up {
		if m.histIdx > 0 {
			m.histIdx--
			m.input.SetText(m.sent[m.histIdx])
		}
		return true // 已经是最早一条：夹取不动
	}
	if m.histIdx < len(m.sent)-1 {
		m.histIdx++
		m.input.SetText(m.sent[m.histIdx])
		return true
	}
	// 已在最新一条：再 ↓ 归还草稿并退出浏览
	m.input.SetText(m.histDraft)
	m.histOn, m.histIdx, m.histDraft = false, 0, ""
	return true
}

// ---------------------------------------------------------------------------
// -histtest：输入历史自检（无 TTY / 无引擎）
// ---------------------------------------------------------------------------

// runHistTest 走一遍历史状态机：入栈去重 / 空输入开翻 / 夹取 / 还原草稿 /
// 有草稿时让位滚动 / 编辑后退出浏览 / 无历史不拦截 / esc 清空退出。
func runHistTest() {
	failed := false
	check := func(name string, cond bool) {
		tag := "PASS"
		if !cond {
			tag = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", tag, name)
	}
	key := func(m model, code rune, text string) model {
		next, _ := m.handleKey(press(code, text))
		return next.(model)
	}
	feedWith := func(rows int) *Feed {
		f := NewFeed()
		f.SetSize(90, 6)
		for i := 0; i < rows; i++ {
			f.Append(kAssistant, fmt.Sprintf("撑滚动用的第 %d 行内容，够长一点。", i))
		}
		return f
	}

	// ① histPush：空串与连续重复不入栈
	mm := model{width: 100}
	mm.histPush("   ")
	mm.histPush("第一条")
	mm.histPush("第一条")
	mm.histPush("第二条")
	check("histPush：空串与连续重复被跳过", len(mm.sent) == 2 && mm.sent[0] == "第一条" && mm.sent[1] == "第二条")

	// ② 空输入 ↑ 开翻（从最新一条起）、↑↓ 往返、↓ 越过最新还原草稿
	m := model{width: 100, feed: NewFeed(), status: stIdle}
	m.histPush("第一条")
	m.histPush("第二条")
	m = key(m, tea.KeyUp, "")
	check("空输入 ↑ → 最新一条（进入浏览）", m.histOn && m.input.Text() == "第二条")
	m = key(m, tea.KeyUp, "")
	check("再 ↑ → 更早一条", m.input.Text() == "第一条" && m.histIdx == 0)
	m = key(m, tea.KeyUp, "")
	check("最早一条再 ↑：夹取不动", m.input.Text() == "第一条")
	m = key(m, tea.KeyDown, "")
	check("↓ → 回到较新一条", m.input.Text() == "第二条" && m.histIdx == 1)
	m = key(m, tea.KeyDown, "")
	check("最新一条再 ↓ → 还原草稿并退出浏览", !m.histOn && m.input.Text() == "")

	// ③ 有草稿时 ↑ 归滚动，不碰历史
	m2 := model{width: 100, feed: feedWith(20), status: stIdle}
	m2.histPush("旧输入")
	m2.input.SetText("正在写的草稿")
	before2 := m2.feed.scroll
	m2 = key(m2, tea.KeyUp, "")
	check("有草稿时 ↑ 照旧滚消息区（历史不介入）",
		m2.feed.scroll != before2 && m2.input.Text() == "正在写的草稿" && !m2.histOn)

	// ④ 翻出历史后打字：退出浏览；此后 ↑ 回滚动
	m3 := model{width: 100, feed: feedWith(20), status: stIdle}
	m3.histPush("历史文本")
	m3 = key(m3, tea.KeyUp, "")
	typed := key(m3, 'x', "x")
	check("翻出历史后打字：退出浏览、文字可继续编辑", !typed.histOn && typed.input.Text() == "历史文本x")
	before3 := typed.feed.scroll
	after := key(typed, tea.KeyUp, "")
	check("退出浏览后 ↑ 回到滚动", after.feed.scroll != before3)

	// ⑤ 没有历史：↑ 不被拦截，照旧滚动
	m4 := model{width: 100, feed: feedWith(20), status: stIdle}
	before4 := m4.feed.scroll
	m4 = key(m4, tea.KeyUp, "")
	check("无历史时 ↑ 仍然滚消息区", m4.feed.scroll != before4)

	// ⑥ esc 清空输入的同时退出浏览（且不取消回合）
	m5 := model{width: 100, feed: NewFeed(), status: stIdle}
	m5.histPush("历史")
	m5 = key(m5, tea.KeyUp, "")
	m5 = key(m5, tea.KeyEscape, "")
	check("esc：清空输入并退出浏览（不取消回合）", m5.input.Text() == "" && !m5.histOn && m5.status == stIdle)

	// ⑦ 发送后的复位语义（histPush 复位浏览态；真正的 submit 在 main.go 里调用）
	m6 := model{width: 100}
	m6.histOn = true
	m6.histPush("一条消息")
	check("histPush 后浏览态复位且入栈", !m6.histOn && len(m6.sent) == 1)

	if failed {
		fmt.Println("histtest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("histtest: 全部通过")
}
