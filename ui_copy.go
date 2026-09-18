// ui_copy.go —— 应用内复制（M6）：把消息条目复制到系统剪贴板。
//
// 通道（Bubble Tea v2 官方 API）：tea.SetClipboard 走 OSC 52 —— 终端把
// base64 文本转发进系统剪贴板；Windows Terminal / kitty / iTerm 等都支持。
// Windows 上再叠一层 PowerShell Set-Clipboard 兜底（conhost 这类不支持
// OSC 52 的终端也能用）：两条通道同发、内容相同，谁生效都行。
//
// 触发入口（见 main.go）：
//   - 鼠标右键点击任意消息条目（消息区命中测试）
//   - 选择态 ctrl+y 复制选中的工具卡（ui_nav.go 的 cardNavCopy）
//   - 非选择态 ctrl+y 复制最后一条助手回复
package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"unicode/utf16"

	tea "charm.land/bubbletea/v2"
)

// copyTextFor 生成一条消息的"纯文本复制版"（无 ANSI、无装饰符号）。
// 各类型的口径：
//   - 用户 / 回复 / 思考：原文（回复是 markdown 源码，粘出去可继续编辑）
//   - 工具卡：调用摘要 + 命令（或参数）+ 输出
//   - 错误：错误文本 + 「提示」行
func copyTextFor(it *FeedItem) string {
	if it == nil {
		return ""
	}
	switch it.Kind {
	case kTool:
		var b strings.Builder
		head := it.ToolCall
		if head == "" {
			head = it.ToolName
		}
		if head != "" {
			b.WriteString(head)
			b.WriteString("\n")
		}
		if it.ToolCmd != "" {
			b.WriteString("$ " + it.ToolCmd + "\n")
		} else if it.ToolRaw != "" {
			b.WriteString(it.ToolRaw + "\n")
		}
		if it.ToolOut != "" {
			b.WriteString(it.ToolOut)
		}
		return strings.TrimRight(b.String(), "\n")
	case kError:
		s := it.Text
		if it.Detail != "" {
			s += "\n提示: " + it.Detail
		}
		return s
	default:
		return it.Text
	}
}

// clipboardCmd 返回"写入系统剪贴板"的命令：OSC 52 + Windows 本地兜底。
func clipboardCmd(s string) tea.Cmd {
	cmds := []tea.Cmd{tea.SetClipboard(s)}
	if runtime.GOOS == "windows" {
		payload := base64.StdEncoding.EncodeToString([]byte(s))
		script := "Set-Clipboard -Value ([System.Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('" + payload + "')))"
		enc := encodePSCommand(script)
		cmds = append(cmds, func() tea.Msg {
			c := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-EncodedCommand", enc)
			_ = c.Run() // 失败无所谓：OSC 52 那条路多半已经到位
			return nil
		})
	}
	return tea.Batch(cmds...)
}

// encodePSCommand 把脚本编码成 powershell -EncodedCommand 需要的 UTF-16LE base64。
// （脚本本身是纯 ASCII —— 用户文本已先做过 base64。）
func encodePSCommand(script string) string {
	u := utf16.Encode([]rune(script))
	buf := make([]byte, len(u)*2)
	for i, r := range u {
		buf[i*2] = byte(r)
		buf[i*2+1] = byte(r >> 8)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// copyLastReply 复制最后一条助手回复（M6 · 非选择态 ctrl+y）。
// 找不到可复制的回复时落一条琥珀提示。
func (m *model) copyLastReply() tea.Cmd {
	for i := len(m.feed.items) - 1; i >= 0; i-- {
		it := m.feed.items[i]
		if it.Kind == kAssistant && strings.TrimSpace(it.Text) != "" {
			m.feed.ScrollToBottom()
			m.feed.Append(kSys, "已复制最后一条回复（"+fmt.Sprintf("%d", len([]rune(it.Text)))+" 字）")
			return clipboardCmd(it.Text)
		}
	}
	m.feed.ScrollToBottom()
	m.feed.Append(kWarn, "还没有可复制的助手回复")
	return nil
}

// ---------------------------------------------------------------------------
// -copytest：复制链路自检（文本生成 + 命令构造；不实际写剪贴板）
// ---------------------------------------------------------------------------

// runCopyTest 断言：各类型复制文本 / 空条目 / PS 命令编码可还原 / 回执两态。
func runCopyTest() {
	failed := false
	check := func(name string, cond bool) {
		tag := "PASS"
		if !cond {
			tag = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", tag, name)
	}

	// ① 各类型复制文本
	user := &FeedItem{Kind: kUser, Text: "你好"}
	asst := &FeedItem{Kind: kAssistant, Text: "# 标题\n正文"}
	tool := &FeedItem{Kind: kTool, ToolName: "fs__read", ToolCall: "fs__read a.txt", ToolCmd: "cat a.txt", ToolOut: "hello\nworld"}
	toolRaw := &FeedItem{Kind: kTool, ToolName: "fs__write", ToolCall: "fs__write b.txt", ToolRaw: `{"path":"b.txt"}`}
	errIt := &FeedItem{Kind: kError, Text: "出错了", Detail: "换模型试试"}

	ok1 := copyTextFor(user) == "你好"
	ok2 := copyTextFor(asst) == "# 标题\n正文"
	ok3 := copyTextFor(tool) == "fs__read a.txt\n$ cat a.txt\nhello\nworld"
	ok4 := copyTextFor(toolRaw) == "fs__write b.txt\n{\"path\":\"b.txt\"}"
	ok5 := copyTextFor(errIt) == "出错了\n提示: 换模型试试"
	check("文本：用户/回复原文；工具=摘要+命令+输出；无命令给参数；错误带提示行",
		ok1 && ok2 && ok3 && ok4 && ok5)

	// ② 空条目：返回空串（不复制空内容）
	okEmpty := copyTextFor(&FeedItem{Kind: kTool}) == "" && copyTextFor(nil) == ""
	check("空条目：工具空内容 / nil 条目 → 空串", okEmpty)

	// ③ 剪贴板命令构造：PS 脚本 UTF-16LE(base64) 可还原
	enc := encodePSCommand("Set-Clipboard -Value 'x'")
	buf, err := base64.StdEncoding.DecodeString(enc)
	okEnc := err == nil && len(buf)%2 == 0 && len(buf) >= 2
	if okEnc {
		u := make([]uint16, 0, len(buf)/2)
		for i := 0; i+1 < len(buf); i += 2 {
			u = append(u, uint16(buf[i])|uint16(buf[i+1])<<8)
		}
		okEnc = string(utf16.Decode(u)) == "Set-Clipboard -Value 'x'"
	}
	check("命令构造：PS -EncodedCommand 编码可还原；两条通道齐备", okEnc && clipboardCmd("hi") != nil)

	// ④ 非选择态 ctrl+y：复制最后回复 / 无回复提示
	m := model{width: 100, feed: NewFeed(), status: stIdle}
	m.feed.SetSize(94, 20)
	m.feed.Append(kUser, "问")
	m.feed.Append(kAssistant, "答")
	cmd := m.copyLastReply()
	last := m.feed.Last()
	okLast := cmd != nil && last != nil && last.Kind == kSys && strings.Contains(last.Text, "已复制最后一条回复")

	m2 := model{width: 100, feed: NewFeed(), status: stIdle}
	m2.feed.SetSize(94, 20)
	m2.copyLastReply()
	last2 := m2.feed.Last()
	okNone := last2 != nil && last2.Kind == kWarn && strings.Contains(last2.Text, "还没有")
	check("回执：有回复→绿条说明；无回复→琥珀提示", okLast && okNone)

	if failed {
		fmt.Println("copytest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("copytest: 全部通过")
}
