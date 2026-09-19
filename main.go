// main.go —— Tematcha · M1：真正的 TUI 前端
//
// M1 目标：把 M0/M0.5 的控制台冒烟程序升级为可操作的 TUI：
//   - 消息区：用户 / 思考 / 回复 三种流式增长 + 工具行 + 用量行
//   - 输入框：打字、回车发送、esc 取消回合
//   - 正式三方分发器（见 acp_client.go）
//   - 单条心跳链驱动所有闪烁节奏（500ms）
//
// 用法：
//
//	tematcha.exe                 正常启动 TUI
//	tematcha.exe -smoke "你好"    不走 TUI，跑一次协议自检（调试用）
//	tematcha.exe -widthcheck     打印关键符号的显示宽度（排查错位用）
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

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
	stLoading    = "loading"
)

// cancelGrace 取消看门狗的宽限期：发起 session/cancel 后引擎这么久还没结束
// 回合，就强制本地收尾。2026-09-19 真机事故：挂起的权限请求没被应答时引擎
// 永远不回回合结束，界面钉死"取消中"——正常取消引擎秒回，根本用不到它。
const cancelGrace = 15 * time.Second

// ---------------------------------------------------------------------------
// tea.Msg 类型
// ---------------------------------------------------------------------------

type blinkMsg struct{}
type acpEventMsg struct{ ev ACPEvent }
type acpClosedMsg struct{}

// noopMsg 空消息：loadSessionAsync 已把"载入完成"注入事件通道，UI 无需再做动作。
type noopMsg struct{}

// methodLoadDone 是 session/load 完成事件的**合成**方法名——它不是引擎发的，
// 是 loadSessionAsync 在自己的 goroutine 里塞进 c.Events 的。之所以要走事件
// 通道而不是直接当 tea.Msg 返回：读循环先把整段历史重放推进 c.Events、然后才
// 回应答，而 UI 是一个 tea 周期才消费一条事件——直接返回会让完成事件插到重放
// 中间（2026-09-19 真机事故：/resume 后只剩三条提问，回答全不见了）。
const methodLoadDone = "session/load_done"

type turnDoneMsg struct {
	reason string
	err    error
	// gen = 回合发起时的回合计代（beginTurn 递增）。看门狗强制收尾后，引擎
	// 迟到的旧回合结果靠它识别并丢弃；0 = 自检/内部自构消息，不参与校验。
	gen int
	// forced = 取消看门狗触发的强制收尾（引擎超时未响应取消），文案不同。
	forced bool
}

// turnResult 是 prompt 回合的最终结果（从 goroutine 传回）。
type turnResult struct {
	reason string
	err    error
	gen    int
}

// ---------------------------------------------------------------------------
// -queuetest：输入队列自检（M3c）
// ---------------------------------------------------------------------------

// runQueueTest 断言"回合进行中提交 → 排队 → 回合结束自动接着发"的整条链路：
//   - 忙时提交：入队 + 落 kQueued 条目（暗色、尾行「· 排队中」）、输入清空、
//     进历史、不新开回合（busy 保持、cmd 为空）
//   - FIFO：先交的排前面
//   - 状态栏：「排队 N」常显，窄预算下仍保留（降级从尾部丢段）
//   - 回合结束：队首出队 → 原地转正成 kUser（条目数不变）、自动开下一回合
//   - 队列空：正常收尾（busy=false、无额外起点）
func runQueueTest() {
	failed := false
	check := func(name string, cond bool) {
		tag := "PASS"
		if !cond {
			tag = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", tag, name)
	}

	// ①② 忙时提交两条 → 入队 + FIFO
	m := model{width: 100, feed: NewFeed(), status: stThinking, busy: true}
	m.input.SetText("第二条：等这一轮结束再发")
	next, cmd := m.submit()
	m = next.(model)
	okQueued := len(m.queue) == 1 && m.feed.Len() == 1 &&
		m.feed.items[0].Kind == kQueued && m.feed.items[0].Text == "第二条：等这一轮结束再发"
	check("忙时提交：入队 + 落 kQueued；输入清空、进历史、不新开回合（cmd 为空）",
		okQueued && m.input.Text() == "" && len(m.sent) == 1 && cmd == nil && m.busy && m.status == stThinking)

	m.input.SetText("第三条：按顺序排")
	next, _ = m.submit()
	m = next.(model)
	check("FIFO：第二条在前、第三条在后（队列长度 2）",
		len(m.queue) == 2 && m.queue[0].Text == "第二条：等这一轮结束再发" && m.queue[1].Text == "第三条：按顺序排")

	// ③ 状态栏「排队 N」（窄预算也留得住：降级从尾部丢模式/上下文/模型）
	bar := stripANSI(m.renderStatusBar())
	// 预算须装得下「工作行（§11.6：乱码+静点+状态词）+ 排队 N」两段（约 31 格）——
	// 降级从尾部丢段，先丢的是模式/上下文/模型，排队段比它们留得久。
	narrow := stripANSI(m.statusLeft(40))
	check("状态栏：「排队 2」常显；预算收紧到 40 格时仍在（先丢的是尾部段）",
		strings.Contains(bar, "排队 2") && strings.Contains(narrow, "排队 2"))

	// ④ 排队条目样张：暗色竖条 + 尾行「· 排队中」
	lines := renderItem(m.feed.items[0], 76)
	okDim := len(lines) > 0 && strings.Contains(lines[0], "38;2;110;110;110") // dimStyle #6E6E6E
	okTag := false
	if len(lines) > 0 {
		okTag = strings.Contains(stripANSI(lines[len(lines)-1]), "· 排队中")
	}
	fmt.Printf("  排队样张：%s\n", stripANSI(strings.Join(lines, "\n")))
	check("样张：排队条目是暗色（非绿条）+ 尾行「· 排队中」标签", okDim && okTag)

	// ⑤ 回合结束：队首转正 + 自动开下一回合（beginTurn 在无 client 时只整理状态）
	// 此刻 feed = [排队一, 排队二]；handleTurnDone 会再落一条回合分隔行 → 3 条
	nextM, _ := m.handleTurnDone(turnDoneMsg{reason: "end_turn"})
	m = nextM.(model)
	okFlip := m.feed.Len() == 3 && m.feed.items[0].Kind == kUser && m.feed.items[1].Kind == kQueued && len(m.queue) == 1
	check("回合结束：队首出队并原地转正为 kUser（条目数不变、没多出第二条）、自动开下一回合",
		okFlip && m.busy && m.status == stThinking && !m.turnStart.IsZero())

	// ⑥ 队列空时的回合结束：正常收尾，不多开回合
	m2 := model{width: 100, feed: NewFeed(), status: stReplying, busy: true}
	nm, cm := m2.handleTurnDone(turnDoneMsg{reason: "end_turn"})
	m2 = nm.(model)
	check("队列空：回合正常收尾（busy=false、无额外起点）", !m2.busy && cm == nil && len(m2.queue) == 0)

	if failed {
		fmt.Println("queuetest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("queuetest: 全部通过")
}

// ---------------------------------------------------------------------------
// -echotest：用户消息回显去重自检（引擎回发 user_message_chunk 不是第二条消息）
// ---------------------------------------------------------------------------

// runEchoTest 断言"本地回显 + 引擎回发"不会让同一条用户消息显示两次。
// 实机 trace 取证：正常回合开始引擎会回发一次同文本 user_message_chunk
// （早先以为只有 session/load 重放才发）——客户端本地已回显过，必须消费掉。
func runEchoTest() {
	failed := false
	check := func(name string, cond bool) {
		tag := "PASS"
		if !cond {
			tag = "FAIL"
			failed = true
		}
		fmt.Printf("[%s] %s\n", tag, name)
	}

	echo := func(m *model, text string) {
		m.handleSessionUpdate(map[string]any{
			"update": map[string]any{
				"sessionUpdate": "user_message_chunk",
				"content":       map[string]any{"text": text},
			},
		})
	}
	countUsers := func(m model) int {
		n := 0
		for _, it := range m.feed.items {
			if it.Kind == kUser {
				n++
			}
		}
		return n
	}

	// ① 正常发送：本地回显 1 条；引擎回发同文本 → 消费，不落第二条
	m := model{width: 100, feed: NewFeed(), status: stIdle}
	m.feed.SetSize(94, 20)
	m.input.SetText("单纯好奇")
	next, _ := m.submit()
	m = next.(model)
	echo(&m, "单纯好奇")
	check("正常回合：本地回显 + 引擎回发同文本 → 只显示一条用户消息",
		countUsers(m) == 1 && m.feed.Len() == 1 && m.localEcho == "")

	// ② 排队转正后同理：转正时登记 localEcho，回发被消费
	m.input.SetText("第二条")
	next, _ = m.submit() // busy → 入队
	m = next.(model)
	nm, _ := m.handleTurnDone(turnDoneMsg{reason: "end_turn"}) // 出队转正 + 开下一回合
	m = nm.(model)
	echo(&m, "第二条")
	check("排队转正：转正消息的引擎回发同样被消费（kUser 仍只有两条）",
		countUsers(m) == 2 && m.localEcho == "")

	// ③ 迟到回发：回合已结束（localEcho 仍登记着）→ 照样消费
	m2 := model{width: 100, feed: NewFeed(), status: stIdle}
	m2.feed.SetSize(94, 20)
	m2.input.SetText("迟到场景")
	next, _ = m2.submit()
	m2 = next.(model)
	nm2, _ := m2.handleTurnDone(turnDoneMsg{reason: "end_turn"})
	m2 = nm2.(model)
	echo(&m2, "迟到场景")
	check("迟到回发：回合结束后才到的回显也消费（不会多出一条）",
		countUsers(m2) == 1)

	// ④ 会话重放（loading）：同文本不是回显，必须照收
	m3 := model{width: 100, feed: NewFeed(), status: stLoading, loading: true}
	m3.feed.SetSize(94, 20)
	echo(&m3, "历史里的消息")
	check("会话重放：loading 期间照常收录（不吃历史消息）",
		countUsers(m3) == 1)

	// ⑤ 文本不同：不是回显，照收
	m4 := model{width: 100, feed: NewFeed(), status: stIdle}
	m4.feed.SetSize(94, 20)
	m4.localEcho = "甲"
	echo(&m4, "乙")
	check("文本不匹配：照常收录（回显只吞同一条消息）", countUsers(m4) == 1 && m4.localEcho == "甲")

	if failed {
		fmt.Println("echotest: 有失败项")
		os.Exit(1)
	}
	fmt.Println("echotest: 全部通过")
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
	// cursorAt 流式光标上次翻转时刻（墙钟驱动）：忙时心跳已提到 50ms
	// （对齐 crush 20fps），光标不能跟着心跳频闪——每 500ms 翻一次。
	cursorAt time.Time

	client    *ACPClient
	sessionID string

	busy   bool
	status string

	// 取消看门狗（2026-09-19 卡死事故）：cancelAt = 发起 session/cancel 的时刻
	// （零值 = 没在取消）；超过 cancelGrace 引擎还不结束回合就强制本地收尾，
	// 否则界面会永久钉在"取消中"、排队消息也发不出去。turnGen = 回合计代：
	// beginTurn 递增；强制收尾会再 +1 作废在途结果，引擎迟到的旧回合结果
	// （gen 过期）一律丢弃，不会误杀新回合。
	cancelAt time.Time
	turnGen  int

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

	// M5b 钉面板（§5）：todosOn = 消息区顶部「# Todos」是否展开（/todos 开关）。
	todosOn bool

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

	// M4c 输入历史：sent = 已发送文本（跳过空串与连续重复）；histOn/histIdx = 浏览位置
	//（histOn=false 表示没在翻）；histDraft = 开翻前的草稿（↓ 越过最新一条时还原）。
	// 只活在内存里：不落盘、跨会话不保留。
	sent      []string
	histOn    bool
	histIdx   int
	histDraft string

	// M4d 会话列/载：sessOn = 选择列表开着；sessReady = 列表已读回来（"空列表"与
	// "还没读回来"是两种状态）；sessRows/sessSel = 数据与选中；loading = 正在等
	// session/load 的应答（引擎会先把历史重放成 update 通知，期间对话区只读）。
	sessOn    bool
	sessReady bool
	sessRows  []SessionRow
	sessSel   int
	loading   bool

	// 权限流（M2a）：perm = 正在展示的请求；permQueue = 排队等待的请求
	perm      *PermRequest
	permQueue []*PermRequest

	// 追问面板（M5a · elicitation/create）：elicit = 正在回答的表单；
	// elicitQueue = 排队等待的表单（引擎同一时刻一般只有一条）。
	elicit      *ElicitRequest
	elicitQueue []*ElicitRequest
	// M3c 输入队列：回合进行中提交的消息先排队（在消息区以 kQueued 暗色呈现），
	// 当前回合一结束就出队转正、自动接着发——不再是"回合进行中，拒绝"。
	queue []*FeedItem

	// localEcho = 最近一次本地回显的用户消息文本。实机 trace 取证：引擎会在
	// 回合开始对同一条消息回发 user_message_chunk——消费掉它，避免同一条用户
	// 消息在消息区显示两次（用户截图实况）。会话重放（loading）时不消费。
	localEcho string

	// M5d 压缩进度（§8 · M11 改造）：compactReq = 本回合是 /compact（进度条显示
	// 在输入栏上方的工作区）；compactSawDrop = 这回合里观察到用量下降；
	// compactDone = 压缩环节完成（进度跳 100%）；compactProg = 进度条显示值
	// （0..100，随时间渐近推进）；compactReceipt = 完成收据（回合结束转收尾行）；
	// barPct/barTo/barAnim = 上下文条的缓动显示值（心跳驱动）。
	compactReq     bool
	compactSawDrop bool
	compactDone    bool
	compactProg    int
	compactReceipt string
	barPct         int
	barTo          int
	barAnim        bool

	// M10/M11 灵动工作行与收尾行（§11.6）：turnChars = 本回合引擎吐回的字符数
	// （≈↓token）；turnSeq = 回合序号（收尾语轮换）；closeLine/closeKind = 上一条
	// 收尾行（工作区闲时显示；kind 决定颜色档，见 ui_work.go）。
	turnChars int
	turnSeq   int
	closeLine string
	closeKind int
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
		todosOn:       true,
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
		// 流式光标按墙钟每 500ms 翻转（与心跳频率解耦：忙时心跳 50ms 对齐
		// crush 的 20fps 乱码，光标跟着翻会频闪）
		if m.cursorAt.IsZero() || time.Since(m.cursorAt) >= blinkLag {
			m.blinkOn = !m.blinkOn
			m.cursorAt = time.Now()
		}
		m.blinkN++
		m.stepBarAnim()     // M5d：压缩后的上下文条缓动（心跳驱动）
		m.stepCompactProg() // M11：压缩进度条推进（按已用时长重算）
		// 取消看门狗（2026-09-19 卡死事故）：发了 session/cancel 引擎却迟迟不结束
		// 回合（挂起的反向请求没被应答就会这样）——超时强制本地收尾，别把界面永久
		// 钉在"取消中"（排队消息也发不出去）。正常取消引擎秒回，根本不会触发。
		if m.busy && !m.cancelAt.IsZero() && time.Since(m.cancelAt) > cancelGrace {
			return m.forceCancelFinish()
		}
		if m.active != nil {
			m.active.CursorOn = m.blinkOn
		}
		return m, blinkAfter(m.blinkInterval()) // 续杯：心跳链永远只有这一根

	case acpEventMsg:
		// session/load 的完成事件是 loadSessionAsync 注入事件通道的合成事件：
		// 与历史重放同一条 FIFO 通道，保证排在整段重放之后（见 methodLoadDone）。
		if msg.ev.Method == methodLoadDone {
			id, _ := msg.ev.Params["id"].(string)
			title, _ := msg.ev.Params["title"].(string)
			return m.finishLoad(id, title, msg.ev.Err)
		}
		m.handleEvent(msg.ev)
		return m, waitEvent(m.client)

	case turnDoneMsg:
		return m.handleTurnDone(msg)

	case sessListMsg:
		m.sessReady = true
		if msg.err != nil {
			m.sessOn = false
			m.feed.Append(kError, "读取会话列表失败："+msg.err.Error())
		} else {
			m.sessRows = msg.rows
			m.sessSel = 0
		}
		m.syncLayout()
		return m, nil

	case sessLoadDoneMsg:
		// 只有"没有引擎连接"的自检路径还走这里（loadSessionAsync 直接返回）。
		// 正常载入的完成事件走事件通道（acpEventMsg → methodLoadDone → finishLoad），
		// 保证排在整段历史重放之后。
		return m.finishLoad(msg.id, msg.title, msg.err)

	case acpClosedMsg:
		if m.status != stEngineGone {
			m.busy = false
			m.loading = false
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

	// 追问面板（M5a）：表单模态 —— ↑↓ 选项、←→ 换题、space 勾选、
	// 1..9 直选、enter 提交、esc 拒绝（decline）；其余键不进输入框。
	if m.elicit != nil {
		if m.elicitKey(k) {
			return m, nil
		}
		switch k.String() {
		case "up", "down", "pgup", "pgdown":
			// 滚动键放行
		default:
			return m, nil
		}
	}

	// 会话选择列表（M4d）：↑↓ 选、enter 载入、esc 关闭；开着时其余键不进输入框。
	// 排在权限面板之后、命令弹层之前 —— 它是更"重"的模态。
	if m.sessOn {
		switch k.String() {
		case "esc":
			m.sessOn = false
			m.syncLayout()
			return m, nil
		case "up":
			if m.sessSel > 0 {
				m.sessSel--
			}
			return m, nil
		case "down":
			if m.sessSel < len(m.sessRows)-1 {
				m.sessSel++
			}
			return m, nil
		case "enter":
			if len(m.sessRows) == 0 {
				return m, nil
			}
			row := m.sessRows[m.sessSel]
			m.sessOn = false
			m.loading = true
			m.busy = true // 载入期间禁发：引擎同一时刻只安装一条会话
			m.status = stLoading
			m.clearCursor()
			m.endThoughtCycle()
			m.endAssistantSegment()
			m.curAssistant, m.curThought, m.active, m.usageItem = nil, nil, nil, nil
			m.tools = make(map[string]*FeedItem)
			m.feed = NewFeed() // 重放会把整套历史重新上屏：先清旧画面
			m.feed.Append(kSys, "载入会话 "+row.ID+"（引擎将重放历史）")
			m.resetUsage() // M5d：清用量快照，避免把换会话误判成压缩
			m.syncLayout()
			return m, loadSessionAsync(m.client, row.ID, row.Title)
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
		case "ctrl+y":
			// M6：复制选中的卡到系统剪贴板
			return m, m.cardNavCopy()
		case "pgup", "pgdown":
			// 滚动键放行（保持选择态，位置不丢）
		default:
			m.exitCardNav()
		}
	}

	// M4c 输入历史：输入框空着（或正在翻历史）时 ↑↓ 归历史；其余情形照旧滚消息区。
	// 放在滚动分支之前 —— 历史消费掉按键就不再滚，没消费就原样落回滚动。
	if k.String() == "up" || k.String() == "down" {
		if m.histMove(k.String() == "up") {
			m.syncLayout() // 召回的文字可能触发/收起命令弹层，布局跟着重算
			return m, nil
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

	// M6：ctrl+y 复制最后一条助手回复（选择态里的 ctrl+y 复制选中卡，
	// 已在上面的 cardNav 分支消化）。
	if k.String() == "ctrl+y" {
		return m, m.copyLastReply()
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
			if m.client != nil {
				m.client.CancelTurn(m.sessionID)
			}
			m.status = stCancelling
			m.cancelAt = time.Now() // 取消看门狗起点（超时强制收尾，见 blinkMsg）
		} else {
			m.input.Clear()
			m.histOn = false // M4c：清空输入 = 退出历史浏览
		}
	}
	// 输入内容变了：命令弹层复位（esc 的关闭状态清掉、选中收拢）+ 布局重算
	//（弹层要占输入区上方的行，高度得从消息区扣）；手改过也退出历史浏览
	if m.input.Text() != before {
		m.palHidden, m.palSel = false, 0
		m.histOn = false
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
	if msg.Button == tea.MouseRight {
		return m.handleRightClick(msg)
	}
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
	// M6：命令弹层鼠标点选（点某一行 = 选中并执行；带参数命令先补全）。
	if m.palOpen() {
		// 弹层首行的屏幕行号：顶部留白 + 消息块（钉面板+消息行，总高不变）+ 空行
		// + 权限面板 + 追问面板（2026-09-19 补算：此前漏了追问面板高度）
		palTop := 1 + len(m.renderTodosPinned(m.feed.width)) + m.feed.normH() + 1 +
			len(m.renderPermPanel(m.width)) + len(m.renderElicitPanel(m.width))
		if msg.Y >= palTop {
			if ci, ok := m.palRowAt(msg.Y - palTop); ok {
				c := m.cmds[ci]
				m.completeCmd(c)
				if c.Hint == "" {
					return m.submit()
				}
				m.syncLayout()
				return m, nil
			}
		}
	}
	// View 布局：第 0 行是顶部留白，之后就是消息区窗口（钉面板 M5b 在消息区
	// 底部，不占消息行偏移——2026-09-19 搬家后点击命中不再减钉面板高度）
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

// handleRightClick 鼠标右键点击消息区：复制该条消息到系统剪贴板（M6）。
// 命中口径与左键一致（右栏区域忽略；行号只扣顶部留白——钉面板 M5b 现位于
// 消息区底部，不再占消息行偏移）。
func (m model) handleRightClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if m.panelVisible() && msg.X >= m.feed.width+scrollBarW+panelSepW {
		return m, nil
	}
	target := m.feed.ItemAt(msg.Y - 1)
	if target == nil {
		return m, nil
	}
	text := copyTextFor(target)
	if text == "" {
		return m, nil
	}
	m.feed.ScrollToBottom()
	m.feed.Append(kSys, "已复制（"+fmt.Sprintf("%d", len([]rune(text)))+" 字）")
	return m, clipboardCmd(text)
}

// submit 提交当前输入：空闲 → 直接开回合；回合进行中 → 排队（M3c）。
// 空文本忽略；护栏拦下的命令只提示不发送。
func (m model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Text())
	if text == "" {
		return m, nil
	}

	// M5b：/todos 是客户端命令（引擎命令表里没有它），本地开关钉面板，不发引擎
	if isTodosToggle(text) {
		m.input.Clear()
		m.todosOn = !m.todosOn
		m.syncLayout()
		m.feed.ScrollToBottom()
		switch {
		case !m.todosOn:
			m.feed.Append(kSys, "待办面板已收起（/todos 可再展开）")
		case len(m.todos) == 0:
			m.feed.Append(kSys, "待办面板已展开：快照里还没有待办（引擎发 plan 时自动更新）")
		default:
			m.feed.Append(kSys, "待办面板已展开")
		}
		return m, nil
	}

	// M5c：/theme 是客户端命令（引擎命令表里没有它）—— 列清单 / 切换主题，不发引擎。
	// 切换只重建样式变量（theme.go 的 applyTheme），下一次 View 即生效。
	if name, isCmd := parseThemeCmd(text); isCmd {
		m.input.Clear()
		m.feed.ScrollToBottom()
		if name == "" {
			m.feed.Append(kSys, themeListLine())
			return m, nil
		}
		t, ok := themeByName(name)
		switch {
		case !ok:
			m.feed.Append(kWarn, "没有这个主题："+name+"；可选："+strings.Join(themeOrder, " | "))
		case t.Name == activeTheme.Name:
			m.feed.Append(kSys, "已经是 "+t.Name+"（"+t.Desc+"）")
		default:
			applyTheme(t)
			m.feed.Append(kSys, "主题已切换："+t.Name+"（"+t.Desc+"）")
		}
		return m, nil
	}

	// M4d：/resume（不带参数）归客户端 —— 引擎只认 "/resume <session_id>"，
	// 这里换成弹出会话选择列表，挑完再用 session/load 载入。
	// 载入会重放历史（等于换一个会话）：回合进行中不给载，先等它结束。
	if isResumeOnly(text) {
		if m.busy {
			m.feed.ScrollToBottom()
			m.feed.Append(kSys, "回合进行中：先等它结束（或 esc 取消）再载入会话")
			return m, nil
		}
		m.input.Clear()
		m.histPush(text)
		m.sessOn, m.sessReady, m.sessSel = true, false, 0
		m.sessRows = nil
		m.syncLayout()
		return m, fetchSessions(m.client)
	}

	// M4e：命令护栏 —— 引擎必拒的形态（缺参数 / 参数越界 / 本地专有命令）
	// 在本地用一句琥珀提示说清楚：不发引擎、不动输入框（用户接着补参数）。
	// 连按回车不必刷屏：紧挨着的上一条是同款提示就只留一条。
	if guard, blocked := m.cmdGuard(text); blocked {
		if last := m.feed.Last(); last == nil || last.Kind != kWarn || last.Text != guard {
			m.feed.ScrollToBottom()
			m.feed.Append(kWarn, guard)
		}
		return m, nil
	}

	m.input.Clear()
	m.histPush(text) // M4c：发送过的内容进历史（↑↓ 可召回）

	// /compact 不回显（用户点名"不要出现 /compact 的字样留在上面"）：压缩的
	// 可视化整个在工作区（英文提示 + 进度条 + 百分比），消息区不留痕。localEcho
	// 仍要登记——引擎回合开始会回发同文本 user_message_chunk，不登记就会被
	// 当成普通用户消息落屏（那就前功尽弃了）。
	compact := isCompactCmd(text)

	// M3c：回合进行中 → 排队。消息先以 kQueued 落在消息区（暗色 +「· 排队中」），
	// 当前回合结束由 handleTurnDone 出队转正（原地变回正常用户消息）并自动接着发。
	if m.busy {
		if compact {
			// 压缩也要独占一个空闲回合（同 /resume 口径）：排队会让 "/compact"
			// 以 kQueued 形态留在消息区，与"不留痕"打架，直接提示等待。
			m.feed.ScrollToBottom()
			m.feed.Append(kSys, "回合进行中：先等它结束（或 esc 取消）再压缩")
			return m, nil
		}
		m.feed.ScrollToBottom()
		m.queue = append(m.queue, m.feed.Append(kQueued, text))
		return m, nil
	}
	if !compact {
		m.feed.ScrollToBottom()
		m.feed.Append(kUser, text)
	}
	m.localEcho = text // 引擎会回发同文本 user_message_chunk：登记待消费，防重复显示
	return m.beginTurn(text)
}

// beginTurn 开一个回合：整理回合状态 + 发 Prompt，返回"等这一轮结束"的 cmd。
// 正常发送与队列出队（M3c）共用；用户消息的落屏由调用方负责
// （排队消息的条目早就存在，出队时只做转正）。
func (m model) beginTurn(text string) (model, tea.Cmd) {
	// 回合状态复位（turnStart 从这里开始计：用户发送 → 回合结束的总时长）
	m.busy = true
	m.status = stThinking
	m.turnStart = time.Now()
	m.turnGen++                       // 新回合计代（看门狗丢弃旧结果的依据）
	m.cancelAt = time.Time{}          // 新回合：取消看门狗撤防
	m.turnChars = 0                   // M10：本回合吐回字符数清零（≈↓token 估算的基数）
	m.compactReq = isCompactCmd(text) // M5d：压缩回合（M11 起在输入栏上方的工作区显示进度条）
	if m.compactReq {
		// M11：压缩可视化 = 工作区进度条 + 百分比，由心跳按已用时长推进；
		// 观察到用量下降时跳 100%（noteUsageDrop），回合结束收成收据收尾行。
		m.compactSawDrop = false
		m.compactDone = false
		m.compactProg = 0
		m.compactReceipt = ""
	}
	m.curAssistant, m.curThought, m.active, m.usageItem = nil, nil, nil, nil
	m.tools = make(map[string]*FeedItem)

	if m.client == nil {
		// 自检里的无引擎 model：只整理状态，不发协议（否则会解引用空 client）
		return m, nil
	}
	ch := make(chan turnResult, 1)
	client, sid, gen := m.client, m.sessionID, m.turnGen
	go func() {
		reason, err := client.Prompt(sid, text)
		ch <- turnResult{reason: reason, err: err, gen: gen}
	}()
	return m, waitTurn(ch)
}

// dequeueNext 取出队首的排队消息并"转正"：Kind 换回 kUser（渲染变回绿条亮字，
// 位置不动、不产生第二条消息），返回它的文本；队列为空时 ok=false。
func (m *model) dequeueNext() (string, bool) {
	if len(m.queue) == 0 {
		return "", false
	}
	it := m.queue[0]
	m.queue = m.queue[1:]
	if it == nil {
		return "", true
	}
	it.Kind = kUser
	it.touch()            // 内容版本 +1 → 渲染缓存失效，绿条亮字立刻生效
	m.localEcho = it.Text // 转正消息的引擎回显同理：登记待消费
	return it.Text, true
}

// handleTurnDone 回合结束：熄光标、定格思考计时、记状态、落回合分隔行。
func (m model) handleTurnDone(msg turnDoneMsg) (tea.Model, tea.Cmd) {
	if msg.gen != 0 && msg.gen != m.turnGen {
		// 看门狗强制收尾后引擎迟到的旧回合结果：丢弃（新回合可能已经在跑）
		return m, nil
	}
	m.busy = false
	m.cancelAt = time.Time{} // 回合结束：取消看门狗撤防
	m.clearCursor()
	m.endThoughtCycle()
	m.endAssistantSegment() // 回合收束：最后一段正文到此为止
	m.dropPerms("回合已结束")    // 回合收尾后，挂起的审批请求已失去意义
	m.dropElicits("回合已结束")  // 追问同理（正常情况下它们会在回合内答完）
	switch {
	case msg.err != nil:
		it := m.feed.Append(kError, "回合出错："+msg.err.Error())
		it.Detail = engineErrorHint(msg.err.Error())
		m.status = stError
	case msg.reason == "cancelled":
		if msg.forced {
			m.feed.Append(kSys, fmt.Sprintf("取消超时 · 引擎 %v 内未结束回合，已强制收尾（如仍有残留输出请忽略）", cancelGrace))
		} else {
			m.feed.Append(kSys, "回合已取消")
		}
		m.status = stCancelled
	default:
		m.status = stDone
	}
	// 回合分隔行：◇ 模型 via 供应商 in 总耗时（从用户发送那刻算起）
	//
	// 2026-09-19 用户点菜：**出错时不落这一行**。出错的视觉主角是上面的 ERROR
	// 徽章块，"哪个模型、多久才失败"是噪音（用户原话："也不要显示 XX模型 via
	// xx in xxms"）。
	dur := time.Duration(0)
	if !m.turnStart.IsZero() {
		dur = time.Since(m.turnStart)
	}
	if msg.err == nil {
		m.feed.Append(kTurn, turnSummaryLine(m.modelLabel, m.modelProvider, dur))
	}

	// M10/M11 收尾行（§11.6）：工作区闲时显示"上一回合怎么了"——常规收尾语按
	// 时长分档 + 轮换序号递增；压缩回合随后覆盖成收据/未见下降/取消/失败。
	//
	// 2026-09-19 用户点菜：**出错时整行不显示**（原来会落"半路卡壳 · 10ms"）。
	// 出错的信息量全在 ERROR 块里，工作区留空即不占行（closeLine == ""）。
	m.closeLine = closingLine(m.status, dur, m.turnSeq)
	m.closeKind = closePlain
	switch m.status {
	case stCancelled:
		m.closeKind = closeWarn
	case stError:
		// 错误：既不要"半路卡壳"，也不要时长——工作区直接留空。
		m.closeLine, m.closeKind = "", closeErr
	}
	m.turnSeq++

	// M5d/M11 压缩收尾：工作区收成一行结论（收据 / 未见下降 / 取消 / 失败）。
	if m.compactReq {
		switch {
		case msg.err != nil:
			m.closeLine, m.closeKind = "压缩失败", closeErr
		case msg.reason == "cancelled":
			m.closeLine, m.closeKind = "压缩已取消 · "+formatElapsed(dur), closeWarn
		case m.compactSawDrop:
			m.closeLine, m.closeKind = "\u2713 "+m.compactReceipt, closeOK
		default:
			m.closeLine, m.closeKind = "压缩请求已完成（未观察到用量下降）", closeWarn
		}
		m.compactReq, m.compactSawDrop, m.compactDone = false, false, false
	}

	// M3c：队列里还有排队消息 → 立刻出队转正，接着开下一回合（用户不用再敲一次）
	if text, ok := m.dequeueNext(); ok {
		m.feed.ScrollToBottom()
		return m.beginTurn(text)
	}
	return m, nil
}

// forceCancelFinish 取消看门狗的强制收尾：作废当前回合计代（引擎之后迟到的
// 结果一律丢弃），再按"已取消"走正常收尾流程（forced 文案 + 队列衔接）。
func (m model) forceCancelFinish() (tea.Model, tea.Cmd) {
	m.turnGen++
	return m.handleTurnDone(turnDoneMsg{reason: "cancelled", forced: true})
}

// finishLoad session/load 收尾：释放载入锁（loading/busy）、落"已载入会话"回执。
//
// 调用时机有讲究：必须等整段历史重放都进屏之后。2026-09-19 真机事故里它被
// 当成普通 tea.Msg 从 goroutine 返回，结果插到重放中间——loading/busy 被提前
// 翻假，后面的 agent_message_chunk 全走 lastAssistant 合并路径，回答并进第一条
// 助手消息（位置很高，翻页才看得见），用户看到"只剩三条提问"。现在完成事件由
// loadSessionAsync 注入事件通道，与重放同一条 FIFO，天然排在最后。
func (m model) finishLoad(id, title string, err error) (tea.Model, tea.Cmd) {
	m.loading = false
	m.busy = false
	if err != nil {
		m.status = stError
		m.feed.Append(kError, "载入会话失败："+err.Error())
	} else {
		m.sessionID = id
		// 会话标题：session/load 的应答里没有它，而引擎的 session_info_update
		// 只在会话被命名/改名时才发（letcode projection.rs 的
		// SessionTitleUpdated 分支）——所以标题只能从会话列表那一行带过来
		// （2026-09-19 用户截图：/resume 回来右栏显示「未命名」）。
		if title != "" {
			m.sessTitle = title
		}
		m.status = stDone
		m.feed.Append(kSys, "已载入会话 "+id)
	}
	m.syncLayout()
	return m, waitEvent(m.client)
}

// engineErrorHint 给引擎错误配一句"怎么办"（kError 的 Detail 行，渲染成暗色提示）。
// 覆盖实测见过的三类：命令用法没写全、本地专有命令、会话设置被拒（推理档位等）。
func engineErrorHint(s string) string {
	switch {
	case strings.Contains(s, "Usage: /"):
		return "命令的参数没写全——敲 / 打开命令列表，选中回车即可补全参数"
	case strings.Contains(s, "is not available over ACP"):
		return "这条命令是 letcode 本地 TUI 专有，ACP 模式下不可用"
	case strings.Contains(s, "reasoning effort change"):
		return "当前模型/供应商不接受该推理档位——可先 /model 换模型，或用 /reasoning 查看可选值"
	case strings.Contains(s, "could not compact"):
		return "引擎没提交压缩（常见原因：上下文已在预算内，或另有回合占用）——可稍后再试"
	case strings.Contains(s, "rejected the session"):
		return "引擎拒绝了这次会话设置变更（多为模型/供应商限制）"
	}
	return ""
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

	case "elicitation/create":
		// 追问（M5a）：question 工具的表单请求 —— 面板渲染 + 作答后回
		// {"action":"accept","content":{…}}（decline/cancel 亦然，见 ui_elicit.go）。
		// 前提是 initialize 里广告过 form elicitation。
		if ev.RawID != nil {
			m.pushElicit(parseElicitRequest(ev.RawID, ev.Params))
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
	case "user_message_chunk":
		// 两种来源：① 正常回合开始，引擎对刚提交的消息回发一次（本地已回显过，
		// 消费掉避免显示两次——localEcho 登记于 submit / 出队转正）；② session/load
		// 重放（loading 期间本地没回显，必须照收）。
		// 每来一条 = 上一段正文 / 思考周期到此为止。
		m.endAssistantSegment()
		m.endThoughtCycle()
		if text := chunkText(upd); text != "" {
			if !m.loading && text == m.localEcho {
				m.localEcho = "" // 引擎把本地回显的那条原样回发：吞掉，不落第二条
			} else {
				m.feed.Append(kUser, text)
			}
		}
	case "agent_message_chunk":
		m.endThoughtCycle() // 回答开始 = 上一段思考周期结束（定格其耗时）
		if text := chunkText(upd); text != "" {
			m.turnChars += utf8.RuneCountInString(text) // M10：工作行的 ≈↓token
			// 迟到 chunk 并回上一段：只适用于"回合已结束"的竞态。loading（会话
			// 重放）期间绝不走这条——每条助手消息都必须是独立条目，否则全部回答
			// 会并进第一条助手消息、被顶到消息区上方看不见（2026-09-19 事故）。
			if it := m.lastAssistant; !m.busy && !m.loading && it != nil {
				// 回合已结束但事件才排到（应答通道先到）：并回上一段正文，
				// 保证"完整正文在分隔行上方"（见 lastAssistant 字段注释）。
				m.feed.AppendStr(it, text)
			} else {
				it := m.ensureAssistant() // 当前回答段落（跨段时新起一条，见 endAssistantSegment）
				m.feed.AppendStr(it, text)
				// 只在回合内更新状态/光标：终态之后迟到的 chunk 只补文本，
				// 不能把状态栏改回"回复中"（否则会卡住不回"回合结束"）；
				// 载入重放同理——状态栏该停在"载入中"。
				if m.busy && !m.loading {
					m.setActive(it)
					m.status = stReplying
				}
			}
		}
	case "agent_thought_chunk":
		m.endAssistantSegment() // 推理开始 = 上一段正文结束
		if text := chunkText(upd); text != "" {
			m.turnChars += utf8.RuneCountInString(text) // M10：思考也算产出（≈↓token）
			// 同 agent_message_chunk：loading（重放）期间不走合并路径
			if it := m.lastThought; !m.busy && !m.loading && it != nil {
				// 同上：迟到的思考 chunk 并回上一段思考块
				m.feed.AppendStr(it, text)
			} else {
				it := m.ensureThought()
				m.feed.AppendStr(it, text)
				if m.busy && !m.loading {
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
		m.syncLayout() // M5b：钉面板行数随快照变化 → 消息区高度重算
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
		if isAgentTool(it.ToolName) && status == "completed" {
			// 子代理完成：content 是 JSON 信封（实机 trace 取证）→ 摘要 + 计数 chips
			if env, ok := parseAgentEnvelope(it.ToolOut); ok && env.Summary != "" {
				it.ToolEnd = env.Summary
				it.ToolChips = env.Chips
			}
		}
	case it.ToolCall == "":
		if isAgentTool(it.ToolName) {
			// 子代理派遣：标题是 "agent__explore {JSON}" 噪音，改从 rawInput 造摘要
			it.ToolCall = agentCallLine(agentRole(it.ToolName), agentTaskFromRaw(it.ToolRaw))
		} else {
			it.ToolCall = title
		}
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
	oldUsed, oldSize := m.usageUsed, m.usageSize
	m.usageUsed, m.usageSize = asInt64(upd["used"]), asInt64(upd["size"])
	m.noteUsageDrop(oldUsed, oldSize, m.usageUsed, m.usageSize) // M5d：明显下降 = 压缩完成
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
	// M5b：钉面板（# Todos）钉在消息区最下方（2026-09-19 用户点菜：从顶部搬到
	// 底部，照 Claude Code——待办紧贴最新消息之下、输入区之上），不参与滚动
	// （高度已在 syncLayout 扣出）
	pinned := m.renderTodosPinned(m.feed.width)
	left := make([]string, 0, len(pinned)+len(feedLines))
	left = append(left, feedLines...)
	left = append(left, pinned...)
	var panelLines []string
	if m.panelVisible() {
		panelLines = m.renderPanel(len(left))
	}
	for i, ln := range left {
		// 兜底：渲染器已各自按预算折行；万一有一行超宽，右缘的滚动条列
		// 与右栏会被"顶着"往右挪（截图上就是"右侧顶出去了"）。
		if lipgloss.Width(ln) > m.feed.width {
			ln = clipLine(ln, m.feed.width)
		}
		b := ""
		// 钉面板在底部后消息行在前：滚动条只跟消息行对齐（钉面板行不画条）
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

	// ②.2 追问面板（M5a · elicitation/create）：同一个钉子位；行数已在
	// syncLayout 里从消息区扣出。真机教训（2026-09-18）：这里漏拼装时布局
	// 仍会扣高度 —— 面板隐形、键盘却被接管（esc 被吃成 decline，引擎收到
	// "用户拒绝回答"）。新增面板必须同时过 syncLayout 与 View 两关。
	for _, ln := range m.renderElicitPanel(m.width) {
		sb.WriteString(ln + "\n")
	}

	// ②.5 命令弹层（M4b · §7）：同样的钉子位；行数已在 syncLayout 里从消息区扣出
	for _, ln := range m.renderCmdPalette() {
		sb.WriteString(ln + "\n")
	}

	// ②.6 会话选择列表（M4d · /resume）：同一个钉子位
	for _, ln := range m.renderSessionPicker() {
		sb.WriteString(ln + "\n")
	}

	// ②.7 工作区（M11 · §11.6）：输入栏正上方的一行——忙时工作行 / 压缩进度条，
	// 闲时上一回合的收尾行；行数已在 syncLayout 里从消息区扣出。
	for _, ln := range m.workStripLines() {
		sb.WriteString(ln + "\n")
	}

	// ③ 输入区（上下细线 + 输入行）
	sb.WriteString(m.renderInputBlock() + "\n")

	// ③ 状态栏
	sb.WriteString(m.renderStatusBar() + "\n")

	v := tea.NewView(sb.String())
	v.AltScreen = true                    // 全屏模式：退出自动还原终端
	v.MouseMode = tea.MouseModeCellMotion // 打开鼠标：滚轮可用
	v.WindowTitle = "Tematcha"
	return v
}

// ---------------------------------------------------------------------------
// tea.Cmd 工厂
// ---------------------------------------------------------------------------

// blink 光标闪烁心跳：每 500ms 翻转一次（单链，只在 Update 里续杯）。
func blink() tea.Cmd {
	return blinkAfter(blinkLag)
}

// blinkAfter 以指定间隔续下一次心跳（M9：压缩动画期间走 120ms 快档）。
func blinkAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return blinkMsg{} })
}

// blinkInterval 当前心跳间隔：忙时（工作行乱码在闪 §11.6 / 压缩进度条在跑）
// 用 50ms 快档（20fps，对齐 crush anim.go 的 fps），闲时回到 500ms。流式
// 光标的闪烁相位由墙钟驱动（见 blinkMsg 的 cursorAt），不随心跳频率变化。
func (m model) blinkInterval() time.Duration {
	if m.busy {
		return workFastLag
	}
	return blinkLag
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
		r := <-ch
		return turnDoneMsg{reason: r.reason, err: r.err, gen: r.gen}
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
	smokeElicit := flag.Bool("smokeelicit", false, "smoke 模式：追问表单自动选每題第一个选项（测 elicitation 链路）")
	widthCk := flag.Bool("widthcheck", false, "打印关键符号的显示宽度后退出")
	mdTest := flag.Bool("mdtest", false, "渲染样例 markdown 到 stdout（自检样式）")
	feedTest := flag.Bool("feedtest", false, "渲染消息区样张到 stdout（自检布局）")
	permTest := flag.Bool("permtest", false, "渲染权限面板样张到 stdout（自检布局）")
	navTest := flag.Bool("navtest", false, "工具卡键盘选择态状态机自检（打印断言）")
	statusTest := flag.Bool("statustest", false, "渲染状态栏样张（多状态/多宽度/阈值色 自检）")
	panelTest := flag.Bool("paneltest", false, "渲染右栏样张（会话/上下文/待办 自检）")
	scrollTest := flag.Bool("scrolltest", false, "滚动条几何自检（不溢出/底部/中部/顶部 + View 集成）")
	cmdTest := flag.Bool("cmdtest", false, "命令弹层自检（解析/过滤/渲染/键位/模态互斥）")
	histTest := flag.Bool("histtest", false, "输入历史自检（入栈/翻历史/让位滚动/编辑退出）")
	sessTest := flag.Bool("sesstest", false, "会话列表自检（解析/触发/渲染/键位/submit 拦截）")
	loadTest := flag.Bool("loadtest", false, "会话载入自检（历史重放保序/完成事件落最后/合并护栏/失败路径）")
	queueTest := flag.Bool("queuetest", false, "输入队列自检（忙时入队/FIFO/状态栏/转正/回合衔接）")
	elicitTest := flag.Bool("elicittest", false, "追问面板自检（解析/渲染/键位/应答体/队列）")
	todosTest := flag.Bool("todostest", false, "钉面板自检（渲染/超长截断/开关/布局/View 集成）")
	themeTest := flag.Bool("themetest", false, "主题系统自检（注册表/命令/渐变/单色探测/缓存作废）")
	echoTest := flag.Bool("echotest", false, "回显去重自检（本地回显 vs 引擎回发 user_message_chunk）")
	compactTest := flag.Bool("compacttest", false, "压缩进度自检（/compact 识别/工作区进度条/填充语义/完成跳 100%/三态收尾）")
	workTest := flag.Bool("worktest", false, "灵动工作行自检（乱码块/拼装/收尾语/工作区集成）")
	agentTest := flag.Bool("agenttest", false, "子代理增强自检（识别/信封/chips/工具卡/来源徽章）")
	cancelTest := flag.Bool("canceltest", false, "取消不卡死自检（cancelled 应答体/cancelPerms 状态机/看门狗 gen 守卫/强制收尾）")
	sessions := flag.Bool("sessions", false, "真拉一次 session/list 并打印（无 TTY 探针）")
	loadID := flag.String("load", "", "真载入一条会话并统计重放（无 TTY 探针；值为 sessionId）")
	trace := flag.Bool("trace", false, "把 ACP 原始流量落盘到 acp-trace.log（排障用）")
	copyTest := flag.Bool("copytest", false, "复制消息自检（文本生成/命令构造/回执）")
	flag.Parse()

	traceTo := ""
	if *trace {
		traceTo = "acp-trace.log"
	}

	if *widthCk {
		fmt.Println("符号宽度检查（全部应为 1 才安全）：")
		for _, s := range []string{"\u276F", "\u23E3", "\u25C7", "\u25B8", "\u25BE", "\u00B7", "\u2503", "\u2502", "\u258C", "\u2588", "\u2591", "\u25CB", "\u25D0", "\u2713", "\u00BB", "\u203A", "~", "\u2500", "\u2026", "\u2191", "\u2193"} {
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

	if *histTest {
		runHistTest()
		return
	}

	if *sessTest {
		runSessTest()
		return
	}

	if *loadTest {
		runLoadTest()
		return
	}

	if *queueTest {
		runQueueTest()
		return
	}
	if *elicitTest {
		runElicitTest()
		return
	}

	if *todosTest {
		runTodosTest()
		return
	}

	if *themeTest {
		runThemeTest()
		return
	}

	if *echoTest {
		runEchoTest()
		return
	}
	if *compactTest {
		runCompactTest()
		return
	}

	if *workTest {
		runWorkTest()
		return
	}

	if *agentTest {
		runAgentTest()
		return
	}

	if *cancelTest {
		runCancelTest()
		return
	}
	if *copyTest {
		runCopyTest()
		return
	}

	if *sessions {
		runSessionsProbe(traceTo)
		return
	}

	if *loadID != "" {
		runLoadProbe(*loadID, traceTo)
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
		runSmoke(*smoke, cancelAfter, permMode, *smokeElicit, traceTo)
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
// -sessions / -load：会话列 / 载的真实链路探针（无 TTY）
// ---------------------------------------------------------------------------

// runSessionsProbe（-sessions）起引擎、握手、真拉一次 session/list 并打印结果。
func runSessionsProbe(traceTo string) {
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
	raw, err := client.ListSessions()
	if err != nil {
		fmt.Println("session/list 失败:", err)
		os.Exit(1)
	}
	rows := parseSessions(raw)
	fmt.Printf("session/list 返回 %d 条：\n", len(rows))
	for i, r := range rows {
		if i >= 10 {
			fmt.Printf("  … 还有 %d 条\n", len(rows)-i)
			break
		}
		fmt.Printf("  %s  %s  %s\n", r.ID, sessStamp(r.Updated), r.Title)
	}
}

// runLoadProbe（-load <id>）真载入一条会话：一边等应答、一边数重放来的 update，
// 并打印头几条文本的摘录（验证 user/agent chunk 的渲染素材确实到了）。
func runLoadProbe(id, traceTo string) {
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

	done := make(chan error, 1)
	go func() {
		_, err := client.LoadSession(id, workspace)
		done <- err
	}()

	user, agent, other := 0, 0, 0
	printed := 0
	deadline := time.After(90 * time.Second)
loop:
	for {
		select {
		case ev := <-client.Events:
			if ev.Method != "session/update" {
				other++
				continue
			}
			upd := asMap(ev.Params["update"])
			kind, _ := upd["sessionUpdate"].(string)
			switch kind {
			case "user_message_chunk":
				user++
			case "agent_message_chunk":
				agent++
			default:
				other++
			}
			if (kind == "user_message_chunk" || kind == "agent_message_chunk") && printed < 4 {
				if text := chunkText(upd); text != "" {
					printed++
					fmt.Printf("  [%s] %s\n", kind, clipWidth(strings.ReplaceAll(text, "\n", " "), 100))
				}
			}
		case err := <-done:
			fmt.Printf("session/load 返回：err=%v\n", err)
			break loop
		case <-deadline:
			fmt.Println("等待超时（90s）")
			break loop
		}
	}
	fmt.Printf("重放统计：user=%d agent=%d 其它=%d\n", user, agent, other)
}

// ---------------------------------------------------------------------------
// -smoke：无 TUI 的协议自检（走正式分发器，把事件流打印成文本）
// ---------------------------------------------------------------------------

// runSmoke 在没有 TTY 的环境（比如自动化验证）里检查协议层。
// permMode 决定权限请求怎么自动答："allow"=允许一次、"deny"=拒绝，空=回 cancelled。
// elicitAuto=true 时追问表单自动选每题的第一个选项（测 elicitation 链路）。
// traceTo 非空时把 ACP 原始流量落盘（-trace，语义同 StartACP）。
func runSmoke(text string, cancelAfter time.Duration, permMode string, elicitAuto bool, traceTo string) {
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
				handleSmokeEvent(client, ev, permMode, elicitAuto)
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
// elicitAuto: 追问表单自动选每題第一个选项。
func handleSmokeEvent(client *ACPClient, ev ACPEvent, permMode string, elicitAuto bool) {
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

	case "elicitation/create":
		// 追问（M5a）：-smokeelicit 时自动选每题第一个选项，否则回 decline。
		// 这一条同时验证"能力广告是否生效"——没广告的话引擎根本不会发这个请求。
		req := parseElicitRequest(ev.RawID, ev.Params)
		if !elicitAuto || req.Unsupported {
			fmt.Printf("\n[elicitation] %q —— 自动拒绝（decline，auto=%v unsupported=%v）\n",
				firstLine(req.Message), elicitAuto, req.Unsupported)
			if ev.RawID != nil {
				_ = client.Respond(ev.RawID, req.response(true))
			}
			return
		}
		for _, q := range req.Questions {
			q.toggleAt(0)
		}
		payload := req.response(false)
		fmt.Printf("\n[elicitation] %d 题 · %q —— 自动作答（accept）content=%v\n",
			len(req.Questions), firstLine(req.Message), payload["content"])
		if ev.RawID != nil {
			if err := client.Respond(ev.RawID, payload); err != nil {
				fmt.Printf("[elicitation] !! Respond 出错: %v\n", err)
			}
		}
	}
}
