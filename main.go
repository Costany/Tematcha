// main.go —— letcode-tui · M1：真正的 TUI 前端
//
// M1 目标：把 M0/M0.5 的控制台冒烟程序升级为可操作的 TUI：
//   - 消息区：用户 / 思考 / 回复 三种流式增长 + 工具行 + 用量行
//   - 输入框：打字、回车发送、esc 取消回合
//   - 正式三方分发器（见 acp_client.go）
//   - 单条心跳链驱动所有闪烁节奏（500ms）
//
// 用法：
//
//	letcode-tui.exe                 正常启动 TUI
//	letcode-tui.exe -smoke "你好"    不走 TUI，跑一次协议自检（调试用）
//	letcode-tui.exe -widthcheck     打印关键符号的显示宽度（排查错位用）
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	letcodeExe = `F:\ComputerLanguage\code\letcode_lab\letcode\target\debug\letcode.exe`
	workspace  = `F:\ComputerLanguage\code\letcode_lab\demo-lab`
	stderrLog  = `acp-stderr.log`

	blinkLag = 500 * time.Millisecond
)

// 引擎状态（状态栏左侧显示用）
const (
	stIdle       = "idle"
	stThinking   = "thinking"
	stReplying   = "replying"
	stCancelling = "cancelling"
	stDone       = "done"
	stCancelled  = "cancelled"
	stError      = "error"
	stEngineGone = "engine-gone"
)

// ---------------------------------------------------------------------------
// tea.Msg 类型
// ---------------------------------------------------------------------------

type blinkMsg struct{}
type acpEventMsg struct{ ev ACPEvent }
type acpClosedMsg struct{}

type turnDoneMsg struct {
	reason string
	err    error
}

// turnResult 是 prompt 回合的最终结果（从 goroutine 传回）。
type turnResult struct {
	reason string
	err    error
}

// ---------------------------------------------------------------------------
// 顶层 model
// ---------------------------------------------------------------------------

type model struct {
	width, height int

	feed  *Feed
	input InputBar

	blinkOn bool
	blinkN  int

	client    *ACPClient
	sessionID string

	busy   bool
	status string

	// 回合计时与模型信息（回合分隔行用）：turnStart = 用户发送时刻；
	// modelLabel/modelProvider 从 session/new 的 configOptions 解析
	//（如 Step 3.7 Flash / guji），之后由 config_option_update 保持同步。
	turnStart     time.Time
	modelLabel    string
	modelProvider string

	// M3 状态栏数据：模式徽章 / 思考档 / 上下文用量。
	// modeID 来自 session/new 的 modes，由 current_mode_update 与
	// config_option_update 同步；usage 来自 usage_update。
	modeID    string // safe / default / auto / yolo
	reasoning string // 思考档显示名（configOptions 的 reasoning_effort；可空）
	usageUsed int64  // 上下文已用 token
	usageSize int64  // 上下文窗口大小

	// M3b 右栏（§4/§5）：会话标题 / 待办快照 / 右栏开关（默认开，窄屏自动隐藏）。
	sessTitle string
	todos     []TodoEntry
	panelOn   bool

	// M4b 命令弹层（§7）：cmds = 引擎广告的斜杠命令（available_commands_update）；
	// palHidden = 被 esc 关掉过（输入再变化即恢复）；palSel = 选中在过滤结果里的下标。
	cmds      []AvailCmd
	palHidden bool
	palSel    int

	// 回合内的流式锚点
	curAssistant *FeedItem
	curThought   *FeedItem
	// 「上一段」锚点：段落/周期收束时留一份。回合应答（waitTurn 通道）与事件流
	// （waitEvent 泵）是两条通道，存在"应答先到、正文 chunk 后到"的时序竞争——
	// 迟到的 chunk 靠它并回上一段，避免正文被回合分隔行劈成两半。
	lastAssistant *FeedItem
	lastThought   *FeedItem
	active        *FeedItem            // 当前带光标的 item
	tools         map[string]*FeedItem // toolCallId → 工具行
	usageItem     *FeedItem            // 本回合的用量行

	// focused = 被鼠标点击选中的用户消息（左侧竖条由细线加粗成半块）
	focused *FeedItem

	// 工具卡键盘选择态（M2b v2）：非 nil = Tab 选择模式开着，值是当前选中的卡。
	// 该模式里 ↑↓ 选卡、enter 展开/收起、esc/tab 退出（esc 只退模式、不取消回合）；
	// 其余按键自动退出模式后照常处理（打字直接续上）。
	cardNav *FeedItem

	// 权限流（M2a）：perm = 正在展示的请求；permQueue = 排队等待的请求
	perm      *PermRequest
	permQueue []*PermRequest
}

func initialModel(c *ACPClient, sid, modelLabel, modelProvider, modeID, reasoning string) model {
	return model{
		width:         80,
		height:        24,
		feed:          NewFeed(),
		status:        stIdle,
		client:        c,
		sessionID:     sid,
		modelLabel:    modelLabel,
		modelProvider: modelProvider,
		modeID:        modeID,
		reasoning:     reasoning,
		panelOn:       true,
		tools:         make(map[string]*FeedItem),
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(blink(), waitEvent(m.client))
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.syncLayout() // 消息区高度 = 终端高 - 固定件 - 权限面板
		return m, nil

	case blinkMsg:
		m.blinkOn = !m.blinkOn
		m.blinkN++
		if m.active != nil {
			m.active.CursorOn = m.blinkOn
		}
		return m, blink() // 续杯：心跳链永远只有这一根

	case acpEventMsg:
		m.handleEvent(msg.ev)
		return m, waitEvent(m.client)

	case turnDoneMsg:
		return m.handleTurnDone(msg)

	case acpClosedMsg:
		if m.status != stEngineGone {
			m.busy = false
			m.clearCursor()
			m.exitCardNav()
			m.dropPerms("引擎连接断开")
			m.feed.Append(kError, "引擎连接断开（letcode 进程已退出）")
			m.status = stEngineGone
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.MouseClickMsg:
		return m.handleClick(msg)

	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.feed.ScrollBy(3)
		case tea.MouseWheelDown:
			m.feed.ScrollBy(-3)
		}
		return m, nil

	case tea.PasteMsg:
		m.input.Insert(msg.Content)
		return m, nil
	}
	return m, nil
}

// handleKey 键盘总入口：先处理退出键与模态（权限面板 / 工具卡选择态），
// 其余交给输入框。
func (m model) handleKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if k.String() == "ctrl+c" {
		return m, tea.Quit
	}

	// ctrl+b：右栏开关（M3b；窄屏下自动隐藏，宽了按开关恢复）
	if k.String() == "ctrl+b" {
		m.panelOn = !m.panelOn
		m.syncLayout()
		return m, nil
	}

	// 权限面板模态：y/a/n/esc 归它；其余键不进输入框（防误输入）。
	// 滚动键放行（交给下面的滚动分支）——面板期间也能回看历史。
	if m.perm != nil {
		switch k.String() {
		case "y":
			m.answerPerm("allow_once")
			return m, nil
		case "a":
			m.answerPerm("allow_always")
			return m, nil
		case "n":
			m.answerPerm("reject_once")
			return m, nil
		case "esc":
			m.cancelPerms()
			return m, nil
		case "up", "down", "pgup", "pgdown":
			// 滚动键放行
		default:
			return m, nil
		}
	}

	// 命令弹层（M4b）：↑↓/tab/enter/esc 归它；其余键落回输入框（过滤实时更新）。
	// 必须排在选择态与滚动键之前 —— 弹层开着时 ↑↓ 是"选命令"、tab 是"补全"，
	// 不是滚消息、也不是进工具卡选择态。
	if m.palOpen() {
		if handled, submit := m.palKey(k); handled {
			if submit {
				m.palHidden, m.palSel = false, 0
				return m.submit()
			}
			m.syncLayout() // 弹层行数变化 → 消息区高度重算
			return m, nil
		}
	}

	// 工具卡键盘选择态（M2b v2）：↑↓ 选卡、enter 展开/收起、esc/tab 退出。
	// esc 在这一层被吃掉——只退模式，不触发回合取消（防误触）；enter 同上，
	// 不会边浏览边把消息发出去。其余按键：退出模式后继续走普通路径（打字
	// 自然续上）。
	if m.cardNav != nil {
		switch k.String() {
		case "esc", "tab":
			m.exitCardNav()
			return m, nil
		case "up":
			m.cardNavMove(-1)
			return m, nil
		case "down":
			m.cardNavMove(1)
			return m, nil
		case "enter":
			m.cardNavToggle()
			return m, nil
		case "pgup", "pgdown":
			// 滚动键放行（保持选择态，位置不丢）
		default:
			m.exitCardNav()
		}
	}

	// 滚动键：up/down 一次 1 行，pgup/pgdown 一次 8 行
	switch k.String() {
	case "up":
		m.feed.ScrollBy(1)
		return m, nil
	case "down":
		m.feed.ScrollBy(-1)
		return m, nil
	case "pgup":
		m.feed.ScrollBy(8)
		return m, nil
	case "pgdown":
		m.feed.ScrollBy(-8)
		return m, nil
	}

	// tab：进入工具卡选择态（M2b v2）
	if k.String() == "tab" {
		m.enterCardNav()
		return m, nil
	}

	before := m.input.Text()
	handled, submit, cancel := m.input.HandleKey(k)
	if !handled {
		return m, nil
	}
	if submit {
		return m.submit()
	}
	if cancel {
		if m.busy {
			m.client.CancelTurn(m.sessionID)
			m.status = stCancelling
		} else {
			m.input.Clear()
		}
	}
	// 输入内容变了：命令弹层复位（esc 的关闭状态清掉、选中收拢）+ 布局重算
	//（弹层要占输入区上方的行，高度得从消息区扣）
	if m.input.Text() != before {
		m.palHidden, m.palSel = false, 0
		m.syncLayout()
	}
	return m, nil
}

// handleClick 鼠标左键点击消息区：
//   - 工具卡：切换展开/收起（M2b）；点过之后不再受自动展开/折叠影响
//   - 用户消息：选中该条（竖条 │ → ▌）；点空白或别的消息则取消选中
//
// 观感照 crush。
func (m model) handleClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if msg.Button != tea.MouseLeft {
		return m, nil
	}
	// 鼠标与键盘选择态互斥：点击先退掉键盘选择态（再按普通路径处理本次点击）
	m.exitCardNav()
	// 右栏区域（M3b）：点击不落到消息区（命中测试只算消息区列）。
	// 消息区右缘还挂着滚动条列（M4a）——面板区起点要把它算进去。
	if m.panelVisible() && msg.X >= m.feed.width+scrollBarW+panelSepW {
		return m, nil
	}
	// View 布局：第 0 行是顶部留白，之后才是消息区窗口
	target := m.feed.ItemAt(msg.Y - 1)

	// 工具卡：展开/收起（没有可展开内容时不响应，免得误置 UserSet）。
	if target != nil && target.Kind == kTool {
		if hasToolBody(target) {
			target.ToolExpanded = !target.ToolExpanded
			target.ToolUserSet = true
			target.touch()
		}
		return m, nil
	}

	if target != nil && target.Kind != kUser {
		target = nil // 只有用户消息可选中
	}
	if target == m.focused {
		return m, nil
	}
	if m.focused != nil {
		m.focused.Focused = false
		m.focused.touch()
	}
	m.focused = target
	if m.focused != nil {
		m.focused.Focused = true
		m.focused.touch()
	}
	return m, nil
}

// submit 发送当前输入（空文本与忙时直接忽略）。
func (m model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Text())
	if text == "" {
		return m, nil
	}
	if m.busy {
		m.feed.Append(kSys, "回合进行中——esc 可取消当前回合，或等它结束后再发")
		return m, nil
	}

	m.input.Clear()
	m.feed.ScrollToBottom()
	m.feed.Append(kUser, text)

	// 回合状态复位（turnStart 从这里开始计：用户发送 → 回合结束的总时长）
	m.busy = true
	m.status = stThinking
	m.turnStart = time.Now()
	m.curAssistant, m.curThought, m.active, m.usageItem = nil, nil, nil, nil
	m.tools = make(map[string]*FeedItem)

	ch := make(chan turnResult, 1)
	client, sid := m.client, m.sessionID
	go func() {
		reason, err := client.Prompt(sid, text)
		ch <- turnResult{reason: reason, err: err}
	}()
	return m, waitTurn(ch)
}

// handleTurnDone 回合结束：熄光标、定格思考计时、记状态、落回合分隔行。
func (m model) handleTurnDone(msg turnDoneMsg) (tea.Model, tea.Cmd) {
	m.busy = false
	m.clearCursor()
	m.endThoughtCycle()
	m.endAssistantSegment() // 回合收束：最后一段正文到此为止
	m.dropPerms("回合已结束")    // 回合收尾后，挂起的审批请求已失去意义
	switch {
	case msg.err != nil:
		m.feed.Append(kError, "回合出错："+msg.err.Error())
		m.status = stError
	case msg.reason == "cancelled":
		m.feed.Append(kSys, "回合已取消")
		m.status = stCancelled
	default:
		m.status = stDone
	}
	// 回合分隔行：◇ 模型 via 供应商 in 总耗时（从用户发送那刻算起）
	dur := time.Duration(0)
	if !m.turnStart.IsZero() {
		dur = time.Since(m.turnStart)
	}
	m.feed.Append(kTurn, turnSummaryLine(m.modelLabel, m.modelProvider, dur))
	return m, nil
}

// ---------------------------------------------------------------------------
// 引擎事件处理
// ---------------------------------------------------------------------------

// handleEvent 处理引擎发来的通知与反向请求。
func (m *model) handleEvent(ev ACPEvent) {
	switch ev.Method {
	case "session/update":
		m.handleSessionUpdate(ev.Params)

	case "session/request_permission":
		// 权限请求（M2a）：进面板队列，由用户 y/a/n/esc 定夺。
		// 应答格式是双重嵌套的 outcome（官方协议的怪癖，见 ui_perm.go）；
		// id 用 RawID 原样回传（类型不能变，否则引擎对不上、回合卡死）。
		if ev.RawID != nil {
			m.pushPerm(parsePermRequest(ev.RawID, ev.Params))
		}

	default:
		// 其它反向请求：先回空结果，防止引擎一直等
		if ev.RawID != nil {
			_ = m.client.Respond(ev.RawID, map[string]any{})
		}
	}
}

// handleSessionUpdate 处理 session/update 通知（流式内容的主入口）。
func (m *model) handleSessionUpdate(params map[string]any) {
	upd := asMap(params["update"])
	if upd == nil {
		return
	}
	switch kind, _ := upd["sessionUpdate"].(string); kind {
	case "agent_message_chunk":
		m.endThoughtCycle() // 回答开始 = 上一段思考周期结束（定格其耗时）
		if text := chunkText(upd); text != "" {
			if it := m.lastAssistant; !m.busy && it != nil {
				// 回合已结束但事件才排到（应答通道先到）：并回上一段正文，
				// 保证"完整正文在分隔行上方"（见 lastAssistant 字段注释）。
				m.feed.AppendStr(it, text)
			} else {
				it := m.ensureAssistant() // 当前回答段落（跨段时新起一条，见 endAssistantSegment）
				m.feed.AppendStr(it, text)
				// 只在回合内更新状态/光标：终态之后迟到的 chunk 只补文本，
				// 不能把状态栏改回"回复中"（否则会卡住不回"回合结束"）
				if m.busy {
					m.setActive(it)
					m.status = stReplying
				}
			}
		}
	case "agent_thought_chunk":
		m.endAssistantSegment() // 推理开始 = 上一段正文结束
		if text := chunkText(upd); text != "" {
			if it := m.lastThought; !m.busy && it != nil {
				// 同上：迟到的思考 chunk 并回上一段思考块
				m.feed.AppendStr(it, text)
			} else {
				it := m.ensureThought()
				m.feed.AppendStr(it, text)
				if m.busy {
					m.setActive(it)
					m.status = stThinking
				}
			}
		}
	case "tool_call", "tool_call_update":
		m.endAssistantSegment() // 工具开始 = 上一段正文结束（正文留在工具行上方原位）
		m.endThoughtCycle()     // 工具开始 = 上一段思考周期结束
		m.handleToolUpdate(upd)
	case "usage_update":
		m.handleUsageUpdate(upd)
	case "current_mode_update":
		// 引擎切换模式（/permission 等）：同步状态栏模式徽章
		if md, _ := upd["currentModeId"].(string); md != "" {
			m.modeID = md
		}
	case "config_option_update":
		// 引擎在 /model、/permission、/reasoning 等切换时刻重发配置项：
		// 同步模型名/供应商（回合分隔行）、模式徽章、思考档（状态栏）
		opts := asList(upd["configOptions"])
		if l, p := modelFromOptions(opts); l != "" {
			m.modelLabel, m.modelProvider = l, p
		}
		if md := modeFromOptions(opts); md != "" {
			m.modeID = md
		}
		if r := reasoningFromOptions(opts); r != "" {
			m.reasoning = r
		}
	case "plan":
		// 待办全量快照（每次整体替换；blocked→pending、cancelled→completed 为引擎侧有损映射，接受）
		m.todos = parsePlan(asList(upd["entries"]))
	case "session_info_update":
		// 会话标题（引擎在会话命名/改名时下发；null = 清除）
		if v, ok := upd["title"]; ok {
			if s, isStr := v.(string); isStr {
				m.sessTitle = s
			} else if v == nil {
				m.sessTitle = ""
			}
		}
	case "available_commands_update":
		// 引擎广告的斜杠命令（M4b；会话建立后下发，命令以 prompt 文本执行）
		m.cmds = parseCommands(asList(upd["availableCommands"]))
	}
	// 其余更新类型暂未消费
}

// chunkText 从 update 里取文本块内容（content.text）。
func chunkText(upd map[string]any) string {
	c := asMap(upd["content"])
	if c == nil {
		return ""
	}
	t, _ := c["text"].(string)
	return t
}

// ensureAssistant 取当前"回答段落"的 item；还没有就新起一段。
// 引擎一个回合的正文是分段的（每次 LLM 迭代一段：常见于工具调用前的过渡语
// 与最后的总结），ACP 不带段落边界——边界交给非正文事件（见 endAssistantSegment）。
// 教训：早先一个回合只用一个槽位，多段正文全并进第一段出现的位置；
// 实测后果 = 工具调用后的最终回复被钉在消息区上方（翻页才看得到），
// 观感像"模型思考完直接结束回合、什么都没回"。
func (m *model) ensureAssistant() *FeedItem {
	if m.curAssistant == nil {
		m.curAssistant = m.feed.Append(kAssistant, "")
	}
	return m.curAssistant
}

// endAssistantSegment 结束当前回答段落：之后的正文会新起一段、出现在流的最新位置。
// 与思考周期（endThoughtCycle）同款做法：非正文事件 = 段落边界。
// 收束时留一份 lastAssistant：迟到的 chunk（应答先到的竞态）靠它并回原位。
func (m *model) endAssistantSegment() {
	if m.curAssistant != nil {
		m.lastAssistant = m.curAssistant
		m.curAssistant = nil
	}
}

func (m *model) ensureThought() *FeedItem {
	if m.curThought == nil {
		m.curThought = m.feed.Append(kThought, "")
		m.curThought.begin() // 思考计时起点（渲染时实时增长，周期结束定格）
		if !m.busy {
			// 回合结束后迟到的思考 chunk：新块立刻定格，避免计时器一直涨
			m.curThought.finish()
		}
	}
	return m.curThought
}

// endThoughtCycle 结束当前思考周期（若有）：定格耗时，并让下一段思考新起一块。
// 引擎的思考是"一段一段"来的（每段有自己的标题与时长），ACP 不带周期边界，
// 这里用"别的类型事件到来"当边界——和 letcode 一个思考块一个周期的行为一致。
func (m *model) endThoughtCycle() {
	if it := m.curThought; it != nil {
		it.finish()
		m.lastThought = it // 迟到 chunk 的归位锚点（见 lastAssistant 字段注释）
		m.curThought = nil
	}
}

// setActive 把流式光标移到指定 item（旧的光标熄灭）。
func (m *model) setActive(it *FeedItem) {
	if m.active == it {
		return
	}
	if m.active != nil {
		m.active.Cursor = false
	}
	m.active = it
	it.Cursor = true
	it.CursorOn = m.blinkOn
}

// clearCursor 关闭所有光标（回合结束时）。
func (m *model) clearCursor() {
	if m.active != nil {
		m.active.Cursor = false
		m.active = nil
	}
}

// handleToolUpdate 处理工具卡（M2b：头部一行 + 可展开的输出区）。
//
// 引擎分三次给不同的 title：pending=工具名、in_progress=调用摘要、
// 完成=结果摘要。三个字段分开留存，完成时行里仍能看到"是什么工具"，
// 状态本身交给齿轮颜色表达（成功柔绿 / 失败柔红 / 进行中灰）。
//
// 展开区数据：rawInput 只在 in_progress 那条里给；content 是"整体替换"式
// 输出（每次给的都是累计全文），直接覆盖即可。
// 自动展开/折叠（用户手点过就不再干预）：运行中点开、完成折叠、失败展开
// ——stderr 通常就在最后几行，折叠了等于没信息。
func (m *model) handleToolUpdate(upd map[string]any) {
	id, _ := upd["toolCallId"].(string)
	title, _ := upd["title"].(string)
	status, _ := upd["status"].(string)

	it := m.tools[id]
	if it == nil {
		it = m.feed.Append(kTool, "")
		it.begin()
		m.tools[id] = it
	}
	if status != "" {
		it.ToolState = status
	}
	if raw, ok := upd["rawInput"]; ok && raw != nil {
		it.ToolCmd, it.ToolRaw = toolInputParts(raw)
	}
	if out := toolContentText(upd["content"]); out != "" {
		it.ToolOut = out
	}
	switch {
	case title == "":
		// 无标题的更新（如纯输出增量）：只可能刷新状态
	case it.ToolName == "" && (status == "pending" || status == ""):
		it.ToolName = title
	case status == "completed" || status == "failed":
		it.ToolEnd = title
	case it.ToolCall == "":
		it.ToolCall = title
	default:
		it.ToolEnd = title
	}

	// "实际失败"推导（ToolBad）：引擎只在"工具执行出错"时给 failed；shell 类
	// 工具"命令跑完但退出码非 0"引擎按 ACP 语义仍报 completed——退出码是结果
	// 数据，藏在摘要里（"exit 1 · stderr 7 lines"；见 letcode 的
	// session/formatting.rs summarize_command 与 acp/projection.rs：
	// Failed ⟺ ToolOutcome::Failure）。前端补这一刀，让"命令失败"也按失败
	// 呈现（红齿轮 / 红选中条 + 默认展开）。
	if status == "failed" {
		it.ToolBad = true
	}
	if status == "completed" {
		if code, ok := parseExitCode(it.ToolEnd); ok && code != 0 {
			it.ToolBad = true
		}
	}

	// 自动展开/折叠：用户手点过（ToolUserSet）就不再干预。
	// 失败（含 shell 退出码非 0）默认展开——stderr 通常就在输出尾部。
	if !it.ToolUserSet {
		switch {
		case it.ToolBad || it.ToolState == "failed":
			it.ToolExpanded = true
		case it.ToolState == "in_progress" || it.ToolState == "running":
			it.ToolExpanded = true
		case it.ToolState == "completed":
			it.ToolExpanded = false
		}
	}
	it.touch()
}

// toolInputParts 把 rawInput 拆成展示用的两块（互斥）：
// shell 取 command 单行（最像"我们在跑什么"）；其余给紧凑 JSON。
// timeout_secs 之类的伴随字段不展示——那是噪音。
func toolInputParts(raw any) (cmd, rawText string) {
	if raw == nil {
		return "", ""
	}
	if mm := asMap(raw); mm != nil {
		if c, ok := mm["command"].(string); ok && c != "" {
			return c, ""
		}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return "", fmt.Sprintf("%v", raw)
	}
	return "", string(b)
}

// parseExitCode 从 shell 类工具的"结果摘要"里取退出码：letcode 的
// summarize_command 产出末形如 "exit 0 · stdout 15 lines" /
// "exit 1 · stderr 7 lines"（git_status/diff/log 同格式）。只认行首
// "exit <数字>"——匹配不上返回 false（比如错误消息摘要）。
func parseExitCode(s string) (int, bool) {
	const prefix = "exit "
	if !strings.HasPrefix(s, prefix) {
		return 0, false
	}
	n, digits := 0, 0
	for _, c := range s[len(prefix):] {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
		digits++
	}
	if digits == 0 {
		return 0, false
	}
	return n, true
}

// toolContentText 从 tool_call_update 的 content 里抽纯文本。
// ACP 形态：[{"type":"content","content":{"type":"text","text":"…"}}]；
// 兼容 {"type":"text","text":"…"} 与纯字符串。\r\n 归一成 \n。
func toolContentText(v any) string {
	var b strings.Builder
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		b.WriteString(t)
	default:
		for _, el := range asList(v) {
			mm := asMap(el)
			if mm == nil {
				continue
			}
			if inner := asMap(mm["content"]); inner != nil {
				if s, ok := inner["text"].(string); ok {
					b.WriteString(s)
					continue
				}
			}
			if s, ok := mm["text"].(string); ok {
				b.WriteString(s)
			}
		}
	}
	out := strings.ReplaceAll(b.String(), "\r\n", "\n")
	return strings.ReplaceAll(out, "\r", "")
}

// handleUsageUpdate 更新本回合的用量行（一条 item 原地刷新），
// 并把数字留给状态栏的上下文条（M3）。
func (m *model) handleUsageUpdate(upd map[string]any) {
	m.usageUsed, m.usageSize = asInt64(upd["used"]), asInt64(upd["size"])
	text := fmt.Sprintf("用量 %v / %v", fmtNum(upd["used"]), fmtNum(upd["size"]))
	if m.usageItem == nil {
		m.usageItem = m.feed.Append(kUsage, text)
	} else {
		m.feed.SetText(m.usageItem, text)
	}
}

// fmtNum 把 JSON 数字格式化成不带小数的字符串。
func fmtNum(v any) string {
	switch n := v.(type) {
	case float64:
		return fmt.Sprintf("%.0f", n)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// asInt64 把 JSON 数字（float64 / int 类）转 int64（解析失败回 0）。
func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}

// fmtK 用量数字的紧凑写法：8898 → 8.9k、31800 → 31.8k、320000 → 320k。
func fmtK(v int64) string {
	if v < 1000 {
		return fmt.Sprintf("%d", v)
	}
	d := float64(v) / 1000
	if d >= 100 {
		return fmt.Sprintf("%.0fk", d)
	}
	return fmt.Sprintf("%.1fk", d)
}

// ---------------------------------------------------------------------------
// 模型信息（回合分隔行用）
// ---------------------------------------------------------------------------

// asList 把 any 转成 []any（不是数组则返回 nil）。
func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// modeFromSession 从 session/new 响应里取当前模式（modes.currentModeId）。
func modeFromSession(sess map[string]any) string {
	modes := asMap(sess["modes"])
	if modes == nil {
		return ""
	}
	s, _ := modes["currentModeId"].(string)
	return s
}

// modeFromOptions 从配置项里取当前模式（option id = "mode"，currentValue 即模式名）。
func modeFromOptions(opts []any) string {
	for _, o := range opts {
		opt := asMap(o)
		if opt == nil || opt["id"] != "mode" {
			continue
		}
		s, _ := opt["currentValue"].(string)
		return s
	}
	return ""
}

// reasoningFromOptions 从配置项里取思考档显示名（option id = "reasoning_effort"）：
// currentValue 是值 id，用 options[].value 匹配后取 name（没有 name 就用原值）。
func reasoningFromOptions(opts []any) string {
	for _, o := range opts {
		opt := asMap(o)
		if opt == nil || opt["id"] != "reasoning_effort" {
			continue
		}
		cur, _ := opt["currentValue"].(string)
		if cur == "" {
			return ""
		}
		for _, e := range asList(opt["options"]) {
			entry := asMap(e)
			if entry == nil || entry["value"] != cur {
				continue
			}
			if name, _ := entry["name"].(string); name != "" {
				return name
			}
		}
		return cur
	}
	return ""
}

// modelFromOptions 从 ACP 的配置项数组里解析模型显示名与供应商。
// model 选项形如：
//
//	{id:"model", currentValue:"guji/step-3.7-flash",
//	 options:[{value:"guji/step-3.7-flash", name:"Step 3.7 Flash"}]}
//
// —— value 是"供应商/模型"路由名，name 是显示名。
func modelFromOptions(opts []any) (label, provider string) {
	for _, o := range opts {
		opt := asMap(o)
		if opt == nil || opt["id"] != "model" {
			continue
		}
		cur, _ := opt["currentValue"].(string)
		if i := strings.Index(cur, "/"); i > 0 {
			provider = cur[:i]
		}
		for _, e := range asList(opt["options"]) {
			entry := asMap(e)
			if entry == nil || entry["value"] != cur {
				continue
			}
			label, _ = entry["name"].(string)
		}
		if label == "" {
			if i := strings.Index(cur, "/"); i >= 0 {
				label = cur[i+1:]
			} else {
				label = cur
			}
		}
		return label, provider
	}
	return "", ""
}

// turnSummaryLine 拼回合分隔行：◇ 模型 via 供应商 in 总耗时。
func turnSummaryLine(label, provider string, d time.Duration) string {
	name := label
	if name == "" {
		name = "letcode"
	}
	via := ""
	if provider != "" {
		via = " via " + provider
	}
	return name + via + " in " + formatElapsed(d)
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

func (m model) View() tea.View {
	var sb strings.Builder

	sb.WriteString("\n") // 顶部留白

	// ① 消息区（滚动窗口；有权限面板时高度已被 syncLayout 让出一块）
	// 右栏（§4/§5）显示时：消息区每行右侧拼「  │ 」+ 右栏对应行
	feedLines := m.feed.Render()
	bar := m.feed.Scrollbar() // M4a：右缘滚动条列（空串 = 该行不画）
	var panelLines []string
	if m.panelVisible() {
		panelLines = m.renderPanel(len(feedLines))
	}
	for i, ln := range feedLines {
		b := ""
		if i < len(bar) {
			b = bar[i]
		}
		// 该行要画条（或右栏在那儿等着）时才补空格对齐：滚动条钉在消息区右缘
		if b != "" || panelLines != nil {
			if pad := m.feed.width - lipgloss.Width(ln); pad > 0 {
				ln += strings.Repeat(" ", pad)
			}
		}
		if b != "" {
			ln += b
		} else if panelLines != nil {
			ln += " " // 保留滚动条列：右栏不因条的出现/消失而横移
		}
		if panelLines != nil {
			ln += "  " + ruleStyle.Render("\u2502") + " " + panelLines[i]
		}
		sb.WriteString(ln + "\n")
	}
	sb.WriteString("\n")

	// ② 权限面板（有请求时钉在输入区上方；高度已从消息区扣出）
	for _, ln := range m.renderPermPanel(m.width) {
		sb.WriteString(ln + "\n")
	}

	// ②.5 命令弹层（M4b · §7）：同样的钉子位；行数已在 syncLayout 里从消息区扣出
	for _, ln := range m.renderCmdPalette() {
		sb.WriteString(ln + "\n")
	}

	// ③ 输入区（上下细线 + 输入行）
	sb.WriteString(m.renderInputBlock() + "\n")

	// ③ 状态栏
	sb.WriteString(m.renderStatusBar() + "\n")

	v := tea.NewView(sb.String())
	v.AltScreen = true                    // 全屏模式：退出自动还原终端
	v.MouseMode = tea.MouseModeCellMotion // 打开鼠标：滚轮可用
	v.WindowTitle = "letcode-tui \u00B7 M1"
	return v
}

// ---------------------------------------------------------------------------
// tea.Cmd 工厂
// ---------------------------------------------------------------------------

// blink 光标闪烁心跳：每 500ms 翻转一次（单链，只在 Update 里续杯）。
func blink() tea.Cmd {
	return tea.Tick(blinkLag, func(time.Time) tea.Msg { return blinkMsg{} })
}

// waitEvent 阻塞等一条引擎事件；每次消费后由 Update 再 arm 下一条。
func waitEvent(c *ACPClient) tea.Cmd {
	return func() tea.Msg {
		select {
		case ev := <-c.Events:
			return acpEventMsg{ev: ev}
		case <-c.Closed:
			// 引擎退出：把通道里可能残留的最后一批事件先捞出来
			select {
			case ev := <-c.Events:
				return acpEventMsg{ev: ev}
			default:
				return acpClosedMsg{}
			}
		}
	}
}

// waitTurn 等一个 prompt 回合结束（goroutine 里的 Call 返回后传回）。
func waitTurn(ch chan turnResult) tea.Cmd {
	return func() tea.Msg {
		return turnDoneMsg(<-ch)
	}
}

// ---------------------------------------------------------------------------
// 入口
// ---------------------------------------------------------------------------

func main() {
	smoke := flag.String("smoke", "", "不走 TUI：发一句话跑一次协议自检")
	smokeCancel := flag.Bool("smokecancel", false, "smoke 模式：3 秒后自动取消回合（测取消链路）")
	smokeAllow := flag.Bool("smokeallow", false, "smoke 模式：权限请求自动选“允许一次”（测审批链路）")
	smokeDeny := flag.Bool("smokedeny", false, "smoke 模式：权限请求自动选“拒绝”（测拒绝链路）")
	widthCk := flag.Bool("widthcheck", false, "打印关键符号的显示宽度后退出")
	mdTest := flag.Bool("mdtest", false, "渲染样例 markdown 到 stdout（自检样式）")
	feedTest := flag.Bool("feedtest", false, "渲染消息区样张到 stdout（自检布局）")
	permTest := flag.Bool("permtest", false, "渲染权限面板样张到 stdout（自检布局）")
	navTest := flag.Bool("navtest", false, "工具卡键盘选择态状态机自检（打印断言）")
	statusTest := flag.Bool("statustest", false, "渲染状态栏样张（多状态/多宽度/阈值色 自检）")
	panelTest := flag.Bool("paneltest", false, "渲染右栏样张（会话/上下文/待办 自检）")
	scrollTest := flag.Bool("scrolltest", false, "滚动条几何自检（不溢出/底部/中部/顶部 + View 集成）")
	cmdTest := flag.Bool("cmdtest", false, "命令弹层自检（解析/过滤/渲染/键位/模态互斥）")
	trace := flag.Bool("trace", false, "把 ACP 原始流量落盘到 acp-trace.log（排障用）")
	flag.Parse()

	traceTo := ""
	if *trace {
		traceTo = "acp-trace.log"
	}

	if *widthCk {
		fmt.Println("符号宽度检查（全部应为 1 才安全）：")
		for _, s := range []string{"\u276F", "\u23E3", "\u25C7", "\u25B8", "\u25BE", "\u00B7", "\u2503", "\u2502", "\u258C", "\u2588", "\u2591", "\u25CB", "\u25D0", "\u2713", "~", "\u2500", "\u2026"} {
			fmt.Printf("  %q  width=%d\n", s, lipgloss.Width(s))
		}
		return
	}

	if *mdTest {
		runMDTest()
		return
	}

	if *feedTest {
		runFeedTest()
		return
	}

	if *permTest {
		runPermTest()
		return
	}

	if *navTest {
		runNavTest()
		return
	}
	if *statusTest {
		runStatusTest()
		return
	}

	if *panelTest {
		runPanelTest()
		return
	}

	if *scrollTest {
		runScrollTest()
		return
	}

	if *cmdTest {
		runCmdTest()
		return
	}

	if *smoke != "" {
		cancelAfter := time.Duration(0)
		if *smokeCancel {
			cancelAfter = 3 * time.Second
		}
		permMode := ""
		switch {
		case *smokeAllow:
			permMode = "allow"
		case *smokeDeny:
			permMode = "deny"
		}
		runSmoke(*smoke, cancelAfter, permMode, traceTo)
		return
	}

	fmt.Println("正在启动 letcode 引擎……")
	client, err := StartACP(letcodeExe, workspace, stderrLog, traceTo)
	if err != nil {
		fmt.Println("启动失败:", err)
		os.Exit(1)
	}
	defer client.Close()

	fmt.Println("正在握手（initialize → session/new）……")
	if _, err := client.Initialize(); err != nil {
		fmt.Println("initialize 失败:", err)
		os.Exit(1)
	}
	sid, sess, err := client.NewSession(workspace)
	if err != nil {
		fmt.Println("session/new 失败:", err)
		os.Exit(1)
	}
	modelLabel, modelProvider := modelFromOptions(asList(sess["configOptions"]))
	fmt.Println("会话就绪:", sid)
	if modelLabel != "" {
		fmt.Printf("模型: %s via %s\n", modelLabel, modelProvider)
	}

	modeID := modeFromSession(sess)
	reasoning := reasoningFromOptions(asList(sess["configOptions"]))
	p := tea.NewProgram(initialModel(client, sid, modelLabel, modelProvider, modeID, reasoning))
	if _, err := p.Run(); err != nil {
		fmt.Println("TUI 退出:", err)
	}
}

// ---------------------------------------------------------------------------
// -smoke：无 TUI 的协议自检（走正式分发器，把事件流打印成文本）
// ---------------------------------------------------------------------------

// runSmoke 在没有 TTY 的环境（比如自动化验证）里检查协议层。
// permMode 决定权限请求怎么自动答："allow"=允许一次、"deny"=拒绝，空=回 cancelled。
// traceTo 非空时把 ACP 原始流量落盘（-trace，语义同 StartACP）。
func runSmoke(text string, cancelAfter time.Duration, permMode string, traceTo string) {
	fmt.Println("正在启动 letcode 引擎……")
	client, err := StartACP(letcodeExe, workspace, stderrLog, traceTo)
	if err != nil {
		fmt.Println("启动失败:", err)
		os.Exit(1)
	}
	defer client.Close()

	if _, err := client.Initialize(); err != nil {
		fmt.Println("initialize 失败:", err)
		os.Exit(1)
	}
	sid, sess, err := client.NewSession(workspace)
	if err != nil {
		fmt.Println("session/new 失败:", err)
		os.Exit(1)
	}
	modelLabel, modelProvider := modelFromOptions(asList(sess["configOptions"]))
	fmt.Println("会话就绪:", sid)
	if modelLabel != "" {
		fmt.Printf("模型: %s via %s\n", modelLabel, modelProvider)
	}

	// 事件泵：把通知打成文本流
	go func() {
		for {
			select {
			case ev := <-client.Events:
				handleSmokeEvent(client, ev, permMode)
			case <-client.Closed:
				return
			}
		}
	}()

	if cancelAfter > 0 {
		go func() {
			time.Sleep(cancelAfter)
			fmt.Println("\n[mock esc] 发送 session/cancel ...")
			client.CancelTurn(sid)
		}()
	}

	fmt.Printf("\n发送: %s\n---\n", text)
	reason, err := client.Prompt(sid, text)
	fmt.Printf("\n---\n回合结束: reason=%v err=%v\n", reason, err)
}

// handleSmokeEvent 把一条事件打印成人话（smoke 专用）。
// permMode: "allow" 自动选 allow_once、"deny" 自动选 reject_once、空回 cancelled。
func handleSmokeEvent(client *ACPClient, ev ACPEvent, permMode string) {
	switch ev.Method {
	case "session/update":
		upd := asMap(ev.Params["update"])
		if upd == nil {
			return
		}
		switch kind, _ := upd["sessionUpdate"].(string); kind {
		case "agent_message_chunk":
			fmt.Print("\n" + chunkText(upd))
		case "agent_thought_chunk":
			fmt.Print("\n~ " + chunkText(upd))
		case "tool_call", "tool_call_update":
			title, _ := upd["title"].(string)
			st, _ := upd["status"].(string)
			fmt.Printf("\n[tool] %s (%s)\n", title, st)
		case "usage_update":
			fmt.Printf("\n[usage] %v / %v\n", upd["used"], upd["size"])
		}
	case "session/request_permission":
		req := parsePermRequest(ev.RawID, ev.Params)
		wantKind := ""
		switch permMode {
		case "allow":
			wantKind = "allow_once"
		case "deny":
			wantKind = "reject_once"
		}
		if wantKind != "" {
			opt := req.optionByKind(wantKind)
			if opt == nil && len(req.Options) > 0 {
				opt = &req.Options[0]
			}
			if opt != nil {
				fmt.Printf("\n[permission] %s —— 自动选择 %s（name=%q, optionId=%s, rpcID=%v/%T）\n",
					req.Title, wantKind, opt.Name, opt.ID, req.RPCID, req.RPCID)
				if ev.RawID != nil {
					if err := client.Respond(ev.RawID, map[string]any{
						"outcome": map[string]any{"outcome": "selected", "optionId": opt.ID},
					}); err != nil {
						fmt.Printf("[permission] !! Respond 出错: %v\n", err)
					}
				} else {
					fmt.Println("[permission] !! ev.RawID 为空，无法应答")
				}
				return
			}
		}
		fmt.Printf("\n[permission] %s —— 自动拒绝（rpcID=%v/%T）\n", req.Title, req.RPCID, req.RPCID)
		if ev.RawID != nil {
			_ = client.Respond(ev.RawID, map[string]any{
				"outcome": map[string]any{"outcome": "cancelled"},
			})
		}
	}
}
