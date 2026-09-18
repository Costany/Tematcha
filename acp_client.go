// acp_client.go —— 正式版 ACP 客户端（三方分发器）
//
// 从 m0/main.go 的脚手架升级：把"按 id 顺序猜响应"的临时握手，
// 换成正式的 JSON-RPC 三方分发：
//
//	响应    （有 id、无 method） → pending map → Call() 的等待者
//	通知    （有 method、无 id） → Events 通道 → UI 消费
//	反向请求（有 method、有 id） → Events 通道 → UI 决定后调 Respond() 答复
//
// 线程模型：
//   - 一个后台 goroutine 独占 stdout 读取（readLoop），按上面三类分发；
//   - 所有写出（Call / Notify / Respond）共用 writeMu 串行化；
//   - Events 永不关闭；引擎退出通过 Closed 通道广播。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// 类型
// ---------------------------------------------------------------------------

// ACPEvent 是读循环交给 UI 的一条被动消息（通知或反向请求）。
// 反向请求的 RawID 非 nil —— UI 处理完后必须调用 Respond() 答复，
// 否则引擎会一直等在这条请求上。
type ACPEvent struct {
	Method string // 例如 "session/update" / "session/request_permission"
	// RawID 是反向请求的原始 JSON-RPC id，**保持原类型**（数字 / 字符串）。
	// 应答必须原样回传：把字符串 id 转成数字回传会与引擎的 pending 表对不上，
	// 引擎就会一直等（表现为"回合卡死、CPU 为 0"）。通知为 nil。
	RawID  any
	Params map[string]any // 解析后的参数（可能为 nil）
}

// ACPClient 是 letcode acp 子进程的薄封装。
type ACPClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	nextID  atomic.Int64

	pendMu  sync.Mutex
	pending map[int64]chan map[string]any

	// Events：给 UI 的事件流（带缓冲；UI 必须持续消费，不然读循环会堵住）。
	Events chan ACPEvent
	// Closed：读循环已退出（引擎退出或管道断开）。
	Closed chan struct{}

	// traceFile：-trace 打开的原始流量落盘（排障用）；nil = 未开启。
	traceFile *os.File
	traceMu   sync.Mutex

	closeOnce sync.Once
}

// ---------------------------------------------------------------------------
// 启动与关闭
// ---------------------------------------------------------------------------

// StartACP 启动引擎子进程并开始读循环。
//
//	exePath  letcode 可执行文件
//	workDir  引擎服务的工作区（作为 cmd.Dir，决定引擎眼中的仓库）
//	stderrTo 引擎 stderr 落盘路径（诊断用；必须消费掉，不然引擎可能卡住）
//	traceTo  ACP 原始流量落盘路径（""=不落盘；排障用，见 trace）
func StartACP(exePath, workDir, stderrTo, traceTo string) (*ACPClient, error) {
	if exePath == "" {
		return nil, errors.New("letcode 可执行文件路径为空")
	}
	cmd := exec.Command(exePath, "acp")
	cmd.Dir = workDir

	stderrFile, err := os.Create(stderrTo)
	if err != nil {
		return nil, fmt.Errorf("创建 stderr 日志失败: %w", err)
	}
	cmd.Stderr = stderrFile

	// trace 落盘（可选）：把收发每一行原始 JSON 留底，
	// 用于复盘"回复丢失 / 回合卡死"这类只有协议层能说清的问题。
	var traceFile *os.File
	if traceTo != "" {
		traceFile, err = os.Create(traceTo)
		if err != nil {
			stderrFile.Close()
			return nil, fmt.Errorf("创建 trace 日志失败: %w", err)
		}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		stderrFile.Close()
		return nil, fmt.Errorf("stdin 管道失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stderrFile.Close()
		return nil, fmt.Errorf("stdout 管道失败: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stderrFile.Close()
		return nil, fmt.Errorf("启动 letcode acp 失败: %w", err)
	}

	c := &ACPClient{
		cmd:       cmd,
		stdin:     stdin,
		pending:   make(map[int64]chan map[string]any),
		Events:    make(chan ACPEvent, 256),
		Closed:    make(chan struct{}),
		traceFile: traceFile,
	}
	go c.readLoop(stdout, stderrFile)
	return c, nil
}

// Close 先关 stdin（让引擎体面退出），超时才强杀。
func (c *ACPClient) Close() {
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		select {
		case <-c.Closed:
		case <-time.After(500 * time.Millisecond):
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
		}
		_ = c.cmd.Wait()
		if c.traceFile != nil {
			_ = c.traceFile.Close()
		}
	})
}

// ---------------------------------------------------------------------------
// 读循环（三方分发器）
// ---------------------------------------------------------------------------

func (c *ACPClient) readLoop(stdout io.Reader, stderrFile *os.File) {
	defer close(c.Closed)
	defer stderrFile.Close()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<24) // 单行上限提到 16MB：工具输出可能很大

	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		c.trace("<<", line) // 原始入站一行（解析失败的行也会留底）
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			fmt.Fprintf(os.Stderr, "[acp] 跳过无法解析的行: %v\n", err)
			continue
		}

		method, hasMethod := m["method"].(string)
		idRaw, hasID := m["id"]

		switch {
		case hasMethod && hasID:
			// 反向请求：引擎要我们做事（比如审批权限）。
			// id 原样带着（类型也要保留），应答时回传。
			c.emit(ACPEvent{Method: method, RawID: idRaw, Params: asMap(m["params"])})

		case hasMethod:
			// 通知：单向广播（session/update 等）
			c.emit(ACPEvent{Method: method, Params: asMap(m["params"])})

		case hasID:
			// 响应：唤醒正在等待的 Call()
			id := anyToInt64(idRaw)
			c.pendMu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.pendMu.Unlock()
			if ch == nil {
				continue
			}
			if e, ok := m["error"]; ok {
				ch <- map[string]any{"__error__": e}
			} else {
				ch <- asMap(m["result"])
			}
		}
	}
}

// emit 把事件交给 UI。阻塞式：不丢事件（UI 正常时通道远不会满）。
func (c *ACPClient) emit(ev ACPEvent) {
	c.Events <- ev
}

// ---------------------------------------------------------------------------
// 出站调用
// ---------------------------------------------------------------------------

// Call 发一条请求并等待响应（timeout<=0 表示不限时）。
func (c *ACPClient) Call(method string, params map[string]any, timeout time.Duration) (map[string]any, error) {
	id := c.nextID.Add(1)
	ch := make(chan map[string]any, 1)
	c.pendMu.Lock()
	c.pending[id] = ch
	c.pendMu.Unlock()

	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
		return nil, err
	}

	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}

	select {
	case res := <-ch:
		if e, ok := res["__error__"]; ok {
			return nil, fmt.Errorf("引擎返回错误: %s", acpErrorMessage(e))
		}
		return res, nil
	case <-timer:
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
		return nil, fmt.Errorf("调用 %s 超时", method)
	case <-c.Closed:
		return nil, errors.New("引擎连接已关闭")
	}
}

// Notify 发一条通知（不等响应）。
func (c *ACPClient) Notify(method string, params map[string]any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// Respond 答复一条反向请求（id 来自 ACPEvent.RawID，原样回传）。
//
// id 收 any 而不是 int64：JSON-RPC 要求响应里的 id 与请求**完全一致**
// （数值 vs 字符串是两种 id），转换过的 id 会让引擎的 pending 表对不上。
func (c *ACPClient) Respond(id any, result map[string]any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// send 序列化一行 JSON 并写出（换行结尾、写锁串行）。
func (c *ACPClient) send(v map[string]any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.trace(">>", data)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(data)
	return err
}

// ---------------------------------------------------------------------------
// 协议快捷方法
// ---------------------------------------------------------------------------

// Initialize 握手第一步：锁协议版本，声明客户端能力。
func (c *ACPClient) Initialize() (map[string]any, error) {
	return c.Call("initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
	}, 30*time.Second)
}

// NewSession 握手第二步：在指定工作区开一条会话。
func (c *ACPClient) NewSession(cwd string) (string, map[string]any, error) {
	res, err := c.Call("session/new", map[string]any{
		"cwd":        cwd,
		"mcpServers": []any{},
	}, 30*time.Second)
	if err != nil {
		return "", nil, err
	}
	sid, _ := res["sessionId"].(string)
	if sid == "" {
		return "", nil, errors.New("session/new 没有返回 sessionId")
	}
	return sid, res, nil
}

// Prompt 发送一句话；返回值直到回合结束才回来（stopReason: end_turn / cancelled）。
// 不设超时：回合靠 esc（CancelTurn）或自然结束来收敛。
func (c *ACPClient) Prompt(sessionID, text string) (string, error) {
	res, err := c.Call("session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt": []map[string]any{
			{"type": "text", "text": text},
		},
	}, 0)
	if err != nil {
		return "", err
	}
	reason, _ := res["stopReason"].(string)
	return reason, nil
}

// CancelTurn 请求取消当前回合（引擎会把未决的工具调用映射为拒绝）。
func (c *ACPClient) CancelTurn(sessionID string) {
	_ = c.Notify("session/cancel", map[string]any{"sessionId": sessionID})
}

// ListSessions 列出可载入的会话（session/list）。letcode 单页返回、不发行游标，
// 列表本就按引擎服务的工作区过滤，所以不带 cwd 过滤参数。
// 返回值是原样的 sessions 数组（每条一个 map，字段名见 ui_sess.go 顶部注释）。
func (c *ACPClient) ListSessions() ([]any, error) {
	res, err := c.Call("session/list", map[string]any{}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	return asList(res["sessions"]), nil
}

// LoadSession 载入一条会话（session/load）：引擎先把历史重放成 session/update
// 通知（只有文本），再回应答 {modes, configOptions}。重放可能很长，超时给足。
func (c *ACPClient) LoadSession(sessionID, cwd string) (map[string]any, error) {
	return c.Call("session/load", map[string]any{
		"sessionId":  sessionID,
		"cwd":        cwd,
		"mcpServers": []any{},
	}, 120*time.Second)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// trace 落盘一条原始 ACP 流量（-trace 排障用；未开启时什么也不做）。
// 格式：<时分秒.毫秒> <方向> <原始一行 JSON>；"<<"=入站、">>"=出站。
// 复盘"回合卡死 / 回复丢失"这类问题时，它是第一手证据。
func (c *ACPClient) trace(dir string, line []byte) {
	if c.traceFile == nil {
		return
	}
	c.traceMu.Lock()
	defer c.traceMu.Unlock()
	fmt.Fprintf(c.traceFile, "%s %s %s\n", time.Now().Format("15:04:05.000"), dir, bytes.TrimRight(line, "\n"))
}

// anyToInt64 把 JSON 数字（float64）转成 int64；容错 string 与 json.Number。
func anyToInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

// asMap 把 any 转成 map[string]any（失败返回 nil）。
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// acpErrLabels JSON-RPC 标准错误码 → 中文短标签（先说清"是哪一类问题"）。
var acpErrLabels = map[int64]string{
	-32700: "解析错误",
	-32600: "请求被拒",
	-32601: "方法不存在",
	-32602: "参数不合法",
	-32603: "引擎内部错误",
}

// acpErrorMessage 把 JSON-RPC 错误对象转成人话：
//
//	map[code:-32602 message:Usage: /resume <session_id>]
//	→ 参数不合法（-32602）：Usage: /resume <session_id>
//
// letcode 的斜杠命令被拒走的就是这条通道（-32602 = 用法没写全，
// -32600 = 会话设置被拒），原始形态是 Go 的 map 打印，读起来太生硬。
func acpErrorMessage(e any) string {
	em := asMap(e)
	if em == nil {
		return fmt.Sprintf("%v", e)
	}
	code := anyToInt64(em["code"])
	msg, _ := em["message"].(string)
	label := acpErrLabels[code]
	switch {
	case label != "" && msg != "":
		return fmt.Sprintf("%s（%d）：%s", label, code, msg)
	case msg != "":
		return msg
	default:
		return fmt.Sprintf("%v", e)
	}
}
