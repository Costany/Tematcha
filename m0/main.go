// letcode-tui · M0 / M0.5 协议冒烟
//
// M0   先不画界面，让引擎开口说话：
//  1. spawn `letcode.exe acp`（在 stdio 上跑 ACP，一行一条 JSON）
//  2. 发 initialize，收到响应后发 session/new
//
// M0.5 说第一句话（-prompt）：握手完成后发一条 session/prompt，
//
//	控制台按"流式"打印回复（原始 JSON 继续进 acp-trace.log）。
//
// 运行：
//
//	go run ./m0                              （只握手，Ctrl+C 退出）
//	go run ./m0 -wait 6s                     （握手后 6 秒自动退出）
//	go run ./m0 -prompt "你好，一句话介绍你自己"  （聊一句，回合结束自动退出）
//
// 排错：
//   - acp-trace.log  —— 完整收发记录（带时间戳）
//   - acp-stderr.log —— 引擎自己的 stderr 日志
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"time"
)

const (
	letcodeExe = `F:\ComputerLanguage\code\letcode_lab\letcode\target\debug\letcode.exe`
	workspace  = `F:\ComputerLanguage\code\letcode_lab\demo-lab`
)

var (
	traceMu   sync.Mutex
	traceFile *os.File
)

// ── 控制台输出：原始行 / trace / 流式 ─────────────────────────

func writeTrace(tag, line string) {
	traceMu.Lock()
	defer traceMu.Unlock()
	if traceFile != nil {
		fmt.Fprintf(traceFile, "%s %s %s\n", time.Now().Format("15:04:05.000"), tag, line)
	}
}

// logLine：原始行 → 控制台 + trace（会先给流式文本收尾换行）。
func logLine(tag, line string) {
	flushStream()
	fmt.Printf("%s %s\n", tag, line)
	writeTrace(tag, line)
}

// logTraceOnly：只进 trace，不打控制台（流式阶段用）。
func logTraceOnly(tag, line string) { writeTrace(tag, line) }

var (
	streamPrefix string // 当前流式行首前缀
	streamMID    bool   // 是否正停在一行流式文本中间
)

func flushStream() {
	if streamMID {
		fmt.Println()
		streamMID = false
		streamPrefix = ""
	}
}

// emitStream：横向流式追加（打字机效果）。prefix 变化时自动换行重开。
func emitStream(prefix, text string) {
	if prefix != streamPrefix {
		flushStream()
		fmt.Print(prefix + " ")
		streamPrefix = prefix
	}
	fmt.Print(text)
	streamMID = true
}

// emitLine：独立一行的流式旁注（工具卡、用量等）。
func emitLine(prefix, text string) {
	flushStream()
	fmt.Printf("%s %s\n", prefix, text)
}

// ── session/update 的"人话"翻译 ───────────────────────────────

type pretty struct {
	prefix string
	text   string
	stream bool // true=横向流式追加；false=独立一行
}

// prettyUpdate 尽力把一条 session/update 通知翻译成可读的一行；
// 认不出来就返回 false（交给原始打印）。M1 会换成完整的渲染层。
func prettyUpdate(line string) (pretty, bool) {
	var env struct {
		Params struct {
			Update json.RawMessage `json:"update"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(line), &env) != nil || env.Params.Update == nil {
		return pretty{}, false
	}
	var up struct {
		SessionUpdate string `json:"sessionUpdate"`
		Content       struct {
			Text string `json:"text"`
		} `json:"content"`
		Title  string `json:"title"`
		Status string `json:"status"`
		Used   any    `json:"used"`
		Size   any    `json:"size"`
	}
	if json.Unmarshal(env.Params.Update, &up) != nil {
		return pretty{}, false
	}
	switch up.SessionUpdate {
	case "user_message_chunk":
		return pretty{"❯", up.Content.Text, true}, true
	case "agent_thought_chunk":
		return pretty{"~", up.Content.Text, true}, true
	case "agent_message_chunk":
		return pretty{"✎", up.Content.Text, true}, true
	case "tool_call", "tool_call_update":
		t := up.Title
		if t == "" {
			t = "(工具更新)"
		}
		if up.Status != "" {
			t += " [" + up.Status + "]"
		}
		return pretty{"⚙", t, false}, true
	case "usage_update":
		return pretty{"·", fmt.Sprintf("用量 %v / %v", up.Used, up.Size), false}, true
	}
	return pretty{}, false
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
	os.Exit(1)
}

// isResponseWithID 判断一行是不是 id==want 的 JSON-RPC 响应（有 id、没有 method）。
func isResponseWithID(line string, want int) bool {
	var env struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
	}
	if json.Unmarshal([]byte(line), &env) != nil {
		return false
	}
	if env.Method != "" || env.ID == nil {
		return false
	}
	n, ok := env.ID.(float64)
	return ok && int(n) == want
}

func main() {
	wait := flag.Duration("wait", 0, "握手后等待时长；0 = 一直挂到 Ctrl+C（M0 用）")
	promptText := flag.String("prompt", "", "非空时：握手完成后发送这句话并流式打印（M0.5 用）")
	flag.Parse()

	var err error
	traceFile, err = os.Create("acp-trace.log")
	if err != nil {
		fatal(err)
	}
	defer traceFile.Close()

	// ── 1. 启动引擎 ──────────────────────────────────────────────
	cmd := exec.Command(letcodeExe, "acp")
	cmd.Dir = workspace // 引擎服务的工作区：文件/命令工具都在这里作用

	stdin, err := cmd.StdinPipe()
	if err != nil {
		fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fatal(err)
	}

	// stderr 必须有人消费（否则写满管道会阻滞引擎）；这里直接归档成文件
	stderrLog, err := os.Create("acp-stderr.log")
	if err != nil {
		fatal(err)
	}
	defer stderrLog.Close()
	cmd.Stderr = stderrLog

	if err := cmd.Start(); err != nil {
		fatal(err)
	}
	logLine("..", fmt.Sprintf("引擎已启动 pid=%d workspace=%s", cmd.Process.Pid, workspace))

	// ── 2. 写侧：一行一条 JSON ───────────────────────────────────
	var writeMu sync.Mutex
	send := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			fatal(err)
		}
		b = append(b, '\n') // 新行分隔：一行一条消息
		writeMu.Lock()
		_, err = stdin.Write(b)
		writeMu.Unlock()
		if err != nil {
			logLine("!!", fmt.Sprintf("写 stdin 失败: %v", err))
			return
		}
		logLine("→", string(b[:len(b)-1]))
	}

	// ── 3. 读侧：持续打印；推进握手；M0.5 流式渲染 ────────────────
	done := make(chan struct{})
	turnDone := make(chan struct{})
	var turnOnce sync.Once
	var sessionID string
	streaming := false

	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<24) // 行上限放大到 16MB（工具输出可能超长）
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				continue
			}

			// 引擎反向请求的提醒（M0 阶段不会应答，避免误会）
			if strings.Contains(line, "request_permission") {
				logLine("!!", "引擎在询问权限——M0 不会应答；请 Ctrl+C 退出（M2 正式处理）")
			}

			// 流式阶段：session/update 走"人话"翻译，原始 JSON 进 trace
			handled := false
			if streaming {
				if p, ok := prettyUpdate(line); ok {
					logTraceOnly("←", line)
					if p.stream {
						emitStream(p.prefix, p.text)
					} else {
						emitLine(p.prefix, p.text)
					}
					handled = true
				}
			}
			if !handled {
				logLine("←", line)
			}

			// M0 土法握手推进：等 initialize 的响应（id=1）到了，再发 session/new
			if isResponseWithID(line, 1) {
				logLine("..", "收到 initialize 响应 → 发送 session/new")
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      2,
					"method":  "session/new",
					"params": map[string]any{
						"cwd":        workspace,
						"mcpServers": []any{},
					},
				})
			}
			if isResponseWithID(line, 2) {
				var resp struct {
					Result struct {
						SessionID string `json:"sessionId"`
					} `json:"result"`
				}
				_ = json.Unmarshal([]byte(line), &resp)
				sessionID = resp.Result.SessionID
				logLine("..", "[OK] M0 冒烟完成：引擎已开口说话（sessionId="+sessionID+"）")

				if *promptText != "" {
					streaming = true
					logLine("..", "发送第一句话："+*promptText)
					send(map[string]any{
						"jsonrpc": "2.0",
						"id":      3,
						"method":  "session/prompt",
						"params": map[string]any{
							"sessionId": sessionID,
							"prompt": []any{
								map[string]any{"type": "text", "text": *promptText},
							},
						},
					})
				}
			}
			if isResponseWithID(line, 3) {
				var resp struct {
					Result struct {
						StopReason string `json:"stopReason"`
					} `json:"result"`
				}
				_ = json.Unmarshal([]byte(line), &resp)
				flushStream()
				logLine("..", fmt.Sprintf("回合结束：stopReason=%s", resp.Result.StopReason))
				turnOnce.Do(func() { close(turnDone) })
			}
		}
		flushStream()
		logLine("..", "stdout 读到 EOF（引擎连接关闭）")
	}()

	// ── 4. 握手第一步 ────────────────────────────────────────────
	send(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": 1,
			// 首版先不声明 elicitation 表单能力（question 工具会被引擎自动婉拒，安全）
			"clientCapabilities": map[string]any{},
		},
	})

	// ── 5. 收尾：等信号/回合/超时，然后善后 ───────────────────────
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	switch {
	case *promptText != "":
		select {
		case <-done:
		case <-turnDone:
			logLine("..", "对话完毕，自动退出（再玩就再跑一遍）")
		case <-sig:
			logLine("..", "收到 Ctrl+C")
		case <-time.After(180 * time.Second):
			logLine("..", "等待回合结束超时（180s），退出")
		}
	default:
		if *wait > 0 {
			select {
			case <-done:
			case <-time.After(*wait):
				logLine("..", "观察窗口结束，主动关闭")
			case <-sig:
				logLine("..", "收到 Ctrl+C")
			}
		} else {
			select {
			case <-done:
			case <-sig:
				logLine("..", "收到 Ctrl+C")
			}
		}
	}

	_ = stdin.Close() // 关 stdin → 引擎读到 EOF，连接收尾
	time.Sleep(300 * time.Millisecond)
	_ = cmd.Process.Kill() // 兜底
	_ = cmd.Wait()
	logLine("..", "已退出。完整记录见 acp-trace.log，引擎日志见 acp-stderr.log")
}
