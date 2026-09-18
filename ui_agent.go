// ui_agent.go —— 子代理工具卡增强（M5d · §9）。
//
// 协议事实（letcode 源码 + 实机 trace 取证，2026-09-18）：
//   - 委派工具名：agent__explore / agent__fixer / agent__oracle /
//     agent__designer / agent__librarian / agent__general；
//     管理类：agent__jobs / agent__status / agent__wait / agent__cancel。
//   - in_progress 时 title = "agent__explore {紧凑 JSON}"，同时带 rawInput
//     （task/objective/…）——标题里那串 JSON 不展示，改从 rawInput.task 造摘要。
//   - completed 时 content.text 是 JSON 信封：
//     {"ok":true,"tool":"agent__explore","data":{"agent_name","summary",
//     "structured_result":{files_read,files_changed,commands_run,validation,
//     findings,…}}}——解析出 summary 当结果行、计数拼 chips。
//   - 子代理的权限请求标题带来源前缀："<agent>: <summary>"（driver.rs 的
//     permission_tool_call）——拆出来当"来源徽章"。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

// agentToolPrefix 子代理工具统一前缀。
const agentToolPrefix = "agent__"

// isAgentTool 工具名是否属于子代理族（含管理类）。
func isAgentTool(name string) bool {
	return strings.HasPrefix(name, agentToolPrefix)
}

// agentRole 工具名 → 角色名（agent__explore → explore）。
func agentRole(name string) string {
	if !isAgentTool(name) {
		return ""
	}
	return strings.TrimPrefix(name, agentToolPrefix)
}

// knownAgentRoles 子代理角色名（catalog.rs 的 agent_name 集合）。
var knownAgentRoles = map[string]bool{
	"explore": true, "explorer": true, "fixer": true, "oracle": true,
	"designer": true, "librarian": true, "general": true,
}

// splitAgentOrigin 从 "<agent>: <summary>" 里拆来源。只认已知角色名，
// 避免误伤普通标题里带冒号的摘要（如 "git log: rebuild"）。
func splitAgentOrigin(title string) (origin, rest string) {
	i := strings.Index(title, ": ")
	if i <= 0 || i > 12 {
		return "", title
	}
	name := strings.ToLower(title[:i])
	if !knownAgentRoles[name] {
		return "", title
	}
	return name, strings.TrimSpace(title[i+2:])
}

// agentTaskFromRaw 从 rawInput 的紧凑 JSON 里取 task（没有则 objective）。
func agentTaskFromRaw(raw string) string {
	if raw == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ""
	}
	s, _ := m["task"].(string)
	if s == "" {
		s, _ = m["objective"].(string)
	}
	return strings.TrimSpace(s)
}

// agentCallLine 工具卡的调用摘要行：explore · <task 截断>。
func agentCallLine(role, task string) string {
	if task == "" {
		return role + " · 派遣子代理"
	}
	task = strings.ReplaceAll(strings.ReplaceAll(task, "\n", " "), "\r", "")
	return role + " · " + clipWidth(task, 60)
}

// agentEnvelope 子代理完成信封里要用的字段。
type agentEnvelope struct {
	Summary string
	Chips   string
	Bad     bool // ok=false（失败信封）
}

// agentEnvelopeRaw 信封的 JSON 形状（只声明用得到的字段）。
type agentEnvelopeRaw struct {
	OK   *bool `json:"ok"`
	Data struct {
		Summary          string `json:"summary"`
		StructuredResult struct {
			FilesRead    []string `json:"files_read"`
			FilesChanged []string `json:"files_changed"`
			CommandsRun  []string `json:"commands_run"`
			Validation   []string `json:"validation"`
			Findings     []string `json:"findings"`
		} `json:"structured_result"`
	} `json:"data"`
}

// parseAgentEnvelope 解析 content.text 里的 JSON 信封（实机样例见文件头）。
func parseAgentEnvelope(text string) (agentEnvelope, bool) {
	t := strings.TrimSpace(text)
	if t == "" || !strings.HasPrefix(t, "{") {
		return agentEnvelope{}, false
	}
	var env agentEnvelopeRaw
	if err := json.Unmarshal([]byte(t), &env); err != nil {
		return agentEnvelope{}, false
	}
	if env.OK == nil {
		return agentEnvelope{}, false
	}
	out := agentEnvelope{Summary: strings.TrimSpace(env.Data.Summary)}
	if !*env.OK {
		out.Bad = true
		return out, true
	}
	sr := env.Data.StructuredResult
	var parts []string
	add := func(n int, label string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", label, n))
		}
	}
	add(len(sr.FilesRead), "read")
	add(len(sr.FilesChanged), "changed")
	add(len(sr.CommandsRun), "commands")
	add(len(sr.Validation), "checks")
	add(len(sr.Findings), "findings")
	out.Chips = strings.Join(parts, " \u00B7 ")
	return out, true
}

// ---------------------------------------------------------------------------
// -agenttest：子代理增强自检
// ---------------------------------------------------------------------------

// runAgentTest 断言 M5d 子代理链路：工具识别 / 信封解析与 chips / 调用摘要 /
// 工具卡渲染（派遣图标 + chips + 宽度）/ 子权限来源徽章 / 符号宽度。
func runAgentTest() {
	failed := false
	check := func(name string, cond bool) {
		tag := "PASS"
		if !cond {
			tag = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", tag, name)
	}

	// ① 识别与角色
	okId := isAgentTool("agent__explore") && isAgentTool("agent__jobs") && !isAgentTool("shell__exec")
	okRole := agentRole("agent__explore") == "explore" && agentRole("shell__exec") == ""
	check("识别：agent__ 前缀判族、角色名取后缀", okId && okRole)

	// ② 信封解析：成功（真实样例）→ summary + chips；失败 → Bad
	envJSON := `{"ok":true,"tool":"agent__explore","data":{"agent_name":"explorer",` +
		`"status":"completed","summary":"demo-lab 目录下只有 1 个文件：hello.txt。",` +
		`"structured_result":{"status":"success","summary":"同上","malformed":false,` +
		`"findings":["仅一个文件"],"files_read":["a.txt","b.txt"],"files_changed":["c.txt"],` +
		`"commands_run":["fs__list path=demo-lab"],"validation":["v1","v2"]}}}`
	env, okEnv := parseAgentEnvelope(envJSON)
	okEnv2 := okEnv && !env.Bad &&
		strings.Contains(env.Summary, "hello.txt") &&
		env.Chips == "read 2 \u00B7 changed 1 \u00B7 commands 1 \u00B7 checks 2 \u00B7 findings 1"
	badEnv, okBad := parseAgentEnvelope(`{"ok":false,"tool":"agent__explore","error":{"message":"x"}}`)
	okBad2 := okBad && badEnv.Bad
	_, okNone := parseAgentEnvelope("not json at all")
	check("信封：成功样例 → summary + chips（read 2 · changed 1 · commands 1 · checks 2 · findings 1）；失败置 Bad；非 JSON 拒绝",
		okEnv2 && okBad2 && !okNone)

	// ③ 调用摘要：从 rawInput 取 task 组 "explore · 探索…"
	raw := `{"task":"探索 demo-lab 目录的结构和文件","objective":"用一句话总结","background":false}`
	line := agentCallLine(agentRole("agent__explore"), agentTaskFromRaw(raw))
	okCall := strings.HasPrefix(line, "explore \u00B7 ") && strings.Contains(line, "探索 demo-lab")
	lineEmpty := agentCallLine("fixer", "")
	check("摘要：explore · <task>（rawInput.task 抽取）；无 task 时给兜底文案",
		okCall && strings.Contains(lineEmpty, "派遣子代理"))

	// ④ 工具卡渲染：派遣图标 » + chips；宽度合规
	it := &FeedItem{Kind: kTool, ToolName: "agent__explore",
		ToolCall:  "explore \u00B7 探索 demo-lab 目录的结构和文件",
		ToolState: "completed", ToolEnd: "只有 hello.txt。", ToolChips: "read 1 \u00B7 commands 1"}
	lines := toolLines(it, 76)
	joined := stripANSI(strings.Join(lines, "\n"))
	okCard := strings.Contains(joined, "\u00BB") && strings.Contains(joined, "read 1") &&
		strings.Contains(joined, "只有 hello.txt。")
	okCardW := true
	for _, ln := range lines {
		if lipgloss.Width(ln) > 76 {
			okCardW = false
		}
	}
	fmt.Println("== 子代理工具卡样张（宽 76）==")
	for _, ln := range lines {
		fmt.Printf("  %s\n", stripANSI(ln))
	}
	check("工具卡：» 派遣图标 + 结果摘要 + chips；每行宽度 ≤ 76", okCard && okCardW)

	// ⑤ 子权限来源徽章：标题 "fixer: Run cargo test" 拆出来源并渲染
	p := parsePermRequest(1, map[string]any{
		"toolCall": map[string]any{"title": "fixer: Run cargo test"},
		"options": []any{
			map[string]any{"optionId": "allow_once", "name": "允许一次", "kind": "allow_once"},
		},
	})
	okOrigin := p.Origin == "fixer" && p.Title == "Run cargo test"
	p2 := parsePermRequest(2, map[string]any{"toolCall": map[string]any{"title": "git log: rebuild"}})
	okNoOrigin := p2.Origin == "" && p2.Title == "git log: rebuild"
	mp := model{width: 100, perm: p}
	plines := mp.renderPermPanel(100)
	pjoined := stripANSI(strings.Join(plines, "\n"))
	okPanel := strings.Contains(pjoined, "fixer") && strings.Contains(pjoined, "Run cargo test")
	okPanelW := true
	for _, ln := range plines {
		if lipgloss.Width(ln) > 100 {
			okPanelW = false
		}
	}
	check("来源徽章：拆 \"fixer: …\"（普通冒号标题不误伤）+ 面板渲染 + 宽度合规",
		okOrigin && okNoOrigin && okPanel && okPanelW)

	// ⑥ 符号宽度：» 与 › 都是 1 格（宽度断言的前提）
	okSym := lipgloss.Width("\u00BB") == 1 && lipgloss.Width("\u203A") == 1
	check("符号：»（派遣）/ ›（徽章）显示宽度 = 1", okSym)

	if failed {
		fmt.Println("agenttest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("agenttest: 全部通过")
}
