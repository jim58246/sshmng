package ssh

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"sshmng/internal/config"
	"sshmng/internal/loginflow"
)

// 默认超时：未指定 timeoutMs 时使用。
const defaultCmdTimeout = 30 * time.Second

// shellDetectTimeout 是 shell 类型探测的超时。
const shellDetectTimeout = 5 * time.Second

// rcInjectTimeout 是 RC 注入后等待首个 PS1 sentinel 的超时。
const rcInjectTimeout = 5 * time.Second

// loginFlowQuietPeriod 是 LoginFlow Read 在 mustContain 为空时的静默期：
// 连续无新数据超过此时长即返回，避免无限等待。
const loginFlowQuietPeriod = 200 * time.Millisecond

// LoginFlowError 携带 LoginFlow 失败时的 trace，供 MCP handler 在 error 响应中
// 一并返回 login_trace 字段供 Agent 诊断（设计文档 §3.x "LoginFlow 失败 error +
// login_trace"）。trace 含每步的 send / expect / output，Agent 据此修配置重试。
//
// Stage 标识失败发生在哪段 flow（"direct" / "jumphost" / "target"），仅用于日志，
// 不影响 Error() 输出——Err 本身已含足够上下文。
type LoginFlowError struct {
	Stage string
	Trace []loginflow.TraceEntry
	Err   error
}

func (e *LoginFlowError) Error() string { return e.Err.Error() }
func (e *LoginFlowError) Unwrap() error { return e.Err }

// PtyConnOptions 是 NewPtyConn 的可选参数；nil 表示无 LoginFlow（直连场景）。
type PtyConnOptions struct {
	LoginFlow       map[string]config.LoginAction // 非空时在 shell detect 后、RC 注入前执行
	LoginEntry      string                        // LoginFlow 起始 Action 名
	MaxSteps        int                           // 0 = 默认 50
	GlobalTimeoutMs int                           // 0 = 默认 60000
}

// PtyConn 包装 *ssh.Session + PTY，实现 Conn 接口。
// 注入 sentinel 后即可通过 Run 执行单条命令并自动识别命令边界 + exit code。
type PtyConn struct {
	session *ssh.Session
	client  *ssh.Client
	stdin   io.WriteCloser
	stdout  io.Reader
	sid     string
	shell   string

	// sftpClient 在 OpenPtyConn 时尝试建立，5s 超时；失败留空，SftpAvailable()=false。
	sftpClient *sftp.Client

	// 单 reader goroutine：所有读取通过 stdoutCh 序列化，避免多个 goroutine 竞争
	// 同一 SSH channel 的 Read。
	stdoutCh chan []byte
	doneCh   chan struct{}
	closed   bool

	// pushback 缓冲：readUntilPatternTimeout 匹配 pattern 后，pattern 之后的
	// trailing data 存此，下次 Read 优先消费。避免 detectShell 吃掉 LoginFlow
	// 阶段 server 自发输出。
	pushback []byte

	mu sync.Mutex
}

// NewPtyConn 是直连场景的便捷构造器：OpenPtyConn + 可选单段 LoginFlow + InjectRC。
// 返回 ready-to-use 的 PtyConn。
//
// Pattern B（两段式 LoginFlow）不要用此构造器——改用 OpenPtyConn + 多次 RunLoginFlow
// + InjectRC，以便在 jumphost flow 与 server flow 之间切分 PTY 流。
//
// opts 为 nil 或 opts.LoginFlow 为空时跳过 LoginFlow（纯直连）。
//
// LoginFlow 失败时返回 *LoginFlowError（携带 trace）；其他失败（detectShell / RC
// 注入）返回普通 error。调用方可用 errors.As 提取 trace 返给 Agent 诊断。
func NewPtyConn(client *ssh.Client, sid string, opts *PtyConnOptions) (*PtyConn, error) {
	p, err := OpenPtyConn(client, sid)
	if err != nil {
		return nil, err
	}
	if opts != nil && len(opts.LoginFlow) > 0 {
		trace, err := p.RunLoginFlow(opts.LoginFlow, opts.LoginEntry, LoginFlowOptions{
			MaxSteps:        opts.MaxSteps,
			GlobalTimeoutMs: opts.GlobalTimeoutMs,
		})
		if err != nil {
			p.Close()
			return nil, &LoginFlowError{Stage: "direct", Trace: trace, Err: err}
		}
	}
	if err := p.InjectRC(); err != nil {
		p.Close()
		return nil, fmt.Errorf("inject rc: %w", err)
	}
	return p, nil
}

// OpenPtyConn 在已建立的 SSH 连接上分配 PTY、启动 shell、探测 shell 类型。
// 不执行 LoginFlow、不注入 RC——调用方负责后续装配：
//   - 直连：直接 InjectRC
//   - SSHServer.LoginFlow：RunLoginFlow → InjectRC
//   - Pattern B：RunLoginFlow(jumphost) → RunLoginFlow(server) → InjectRC
//
// 调用者负责在失败时 Close。
func OpenPtyConn(client *ssh.Client, sid string) (*PtyConn, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	// ECHO=0：所有 stdin 写入都是 sshmng 主动发起（LoginFlow Send / run_in_session
	// cmd / send_input text），不需要 tty driver 回显。若 ECHO=1，detectShell 发送的
	// 探测命令会先被回显到 stdout，readUntilPattern 在回显字符串里就匹配到
	// __DETECT_END__，ParseShellDetect 看到的是未展开的 `echo __SHELL_DETECT__:$0:...`
	// （$0 仍是字面量），无法解析 shell 类型。RC 注入里有 stty -echo 但那是 RC 之后
	// 才生效，detectShell 在 RC 之前执行，必须在 pty-req 阶段就关闭 ECHO。
	modes := ssh.TerminalModes{
		ssh.ECHO:          0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm", 100, 120, modes); err != nil {
		session.Close()
		return nil, fmt.Errorf("request pty: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := session.Shell(); err != nil {
		session.Close()
		return nil, fmt.Errorf("start shell: %w", err)
	}
	p := &PtyConn{
		session:  session,
		client:   client,
		stdin:    stdin,
		stdout:   stdout,
		sid:      sid,
		stdoutCh: make(chan []byte, 1024),
		doneCh:   make(chan struct{}),
	}
	go p.readLoop()

	shell, err := p.detectShell()
	if err != nil {
		p.Close()
		return nil, fmt.Errorf("detect shell: %w", err)
	}
	p.shell = shell

	// 尝试建立 sftp 通道；失败不影响 login，仅影响 upload/download 可用性。
	if sc, err := newSftpClient(client); err == nil {
		p.sftpClient = sc
	}
	return p, nil
}

// LoginFlowOptions 是 RunLoginFlow 的可选参数；零值使用 loginflow 包默认值。
type LoginFlowOptions struct {
	MaxSteps        int
	GlobalTimeoutMs int
}

// RunLoginFlow 在 PTY 上执行一段 LoginFlow 决策树。
// 多次调用可串联（Pattern B：先 jumphost flow 再 server flow），trailing data 通过
// pushback 在调用间保留——前段流的最后 Read 不会吞掉后段流要等的 prompt。
//
// flow 为空时直接返回（无操作），便于调用方无条件调用。
// 返回 trace 供诊断（如 MCP handler 在失败时返回 login_trace）。
func (p *PtyConn) RunLoginFlow(flow map[string]config.LoginAction, entry string, opts LoginFlowOptions) ([]loginflow.TraceEntry, error) {
	if len(flow) == 0 {
		return nil, nil
	}
	return loginflow.Run(p, flow, entry, loginflow.Options{
		MaxSteps:       opts.MaxSteps,
		GlobalTimeout:  time.Duration(opts.GlobalTimeoutMs) * time.Millisecond,
		DefaultTimeout: 0, // 用 loginflow 包默认值
	})
}

// InjectRC 注入 RC 脚本并等待首个 PS1 sentinel 出现。
// 在所有 LoginFlow（如有）执行完后调用。
func (p *PtyConn) InjectRC() error {
	return p.injectRC()
}

// readLoop 单 goroutine 持续读 stdout，把数据投递到 stdoutCh。
// 所有 readUntilPattern 都从 stdoutCh 消费，避免多 goroutine 竞争 Read。
func (p *PtyConn) readLoop() {
	tmp := make([]byte, 1024)
	for {
		n, err := p.stdout.Read(tmp)
		if n > 0 {
			data := make([]byte, n)
			copy(data, tmp[:n])
			select {
			case p.stdoutCh <- data:
			case <-p.doneCh:
				return
			}
		}
		if err != nil {
			// 把剩余数据投递完，然后关闭 channel
			close(p.stdoutCh)
			return
		}
	}
}

// Shell 返回探测到的 shell 类型（bash/zsh/dash/ash/...）。
func (p *PtyConn) Shell() string { return p.shell }

// detectShell 发送探测命令并解析 shell 类型。
func (p *PtyConn) detectShell() (string, error) {
	rand, err := RandomSID()
	if err != nil {
		return "", err
	}
	cmd := fmt.Sprintf("echo __SHELL_DETECT__:$0:${BASH_VERSION:-}:${ZSH_VERSION:-}; echo __DETECT_END_%s__\n", rand)
	if _, err := p.stdin.Write([]byte(cmd)); err != nil {
		return "", fmt.Errorf("write detect cmd: %w", err)
	}
	endMarker := fmt.Sprintf("__DETECT_END_%s__", rand)
	output, err := p.readUntilPattern(endMarker, shellDetectTimeout)
	if err != nil {
		return "", err
	}
	shell, ok := ParseShellDetect(output, rand)
	if !ok {
		return "", fmt.Errorf("could not parse shell from: %q", output)
	}
	return shell, nil
}

// injectRC 发送 RC 脚本并等待首个 PS1 sentinel 出现。
func (p *PtyConn) injectRC() error {
	rc := BuildRC(p.shell, p.sid)
	if _, err := p.stdin.Write([]byte(rc)); err != nil {
		return fmt.Errorf("write rc: %w", err)
	}
	ps1Sentinel := fmt.Sprintf("__P_%s__> ", p.sid)
	_, err := p.readUntilPattern(ps1Sentinel, rcInjectTimeout)
	return err
}

// Run 实现 Conn 接口。发送命令、读取输出、解析 exit code、清洗、截断。
func (p *PtyConn) Run(cmd string, timeoutMs int, maxOutputBytes int) (string, int, bool, bool, int, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return "", 0, false, false, 0, errors.New("connection closed")
	}
	p.mu.Unlock()

	if _, err := p.stdin.Write([]byte(cmd + "\n")); err != nil {
		return "", 0, false, false, 0, fmt.Errorf("write cmd: %w", err)
	}
	timeout := defaultCmdTimeout
	if timeoutMs > 0 {
		timeout = time.Duration(timeoutMs) * time.Millisecond
	}
	ps1Sentinel := fmt.Sprintf("__P_%s__> ", p.sid)
	raw, timedOut := p.readUntilPatternTimeout(ps1Sentinel, timeout)
	code, found := ExtractExitCode(raw, p.sid)
	if !found {
		code = -1
	}
	cleaned := CleanOutput(raw, p.sid)
	out, wasTruncated, totalBytes := TruncateOutput(cleaned, maxOutputBytes)
	return out, code, timedOut, wasTruncated, totalBytes, nil
}

// SendInput 实现 Conn 接口。向 PTY stdin 写入任意文本。
func (p *PtyConn) SendInput(text string) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("connection closed")
	}
	p.mu.Unlock()
	_, err := p.stdin.Write([]byte(text))
	return err
}

// Read 实现 loginflow.PTY 接口。
//
// matchers 非空时持续累积 PTY 输出，每收到新数据就用所有 matchers（针对 ANSI
// 过滤后的输出）试匹配；命中即返回截至 match 末尾的 raw output，trailing 留 pushback
// 供下次 Read。这是 Pattern B 两段式 LoginFlow 的关键：第一段流的最后一次 Read 不能
// 把 target shell 的 prompt（如 "login: "）一并吞掉——否则第二段流入口 Action 等不到
// 任何输出。
//
// matchers 为空时按静默期 heuristic（连续 loginFlowQuietPeriod 无新数据即返回），
// 用于无 Expects 的 Action 等远端自发输出（MOTD / 菜单）。
//
// 优先消费 pushback（前次 Read 留下的 trailing data）。
func (p *PtyConn) Read(deadline time.Time, matchers []*regexp.Regexp) (string, int, bool, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return "", -1, false, errors.New("connection closed")
	}

	// 先消费 pushback
	p.mu.Lock()
	var buf []byte
	if len(p.pushback) > 0 {
		buf = append(buf, p.pushback...)
		p.pushback = nil
	}
	p.mu.Unlock()

	// pushback 已含匹配：直接返回
	if idx, rawEnd := matchRaw(buf, matchers); idx >= 0 {
		p.mu.Lock()
		p.pushback = append([]byte{}, buf[rawEnd:]...)
		p.mu.Unlock()
		return string(buf[:rawEnd]), idx, false, nil
	}

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return string(buf), -1, true, nil
		}
		timeout := remaining
		// matchers 为空时按静默期返回；否则等到 deadline
		if len(matchers) == 0 && timeout > loginFlowQuietPeriod {
			timeout = loginFlowQuietPeriod
		}
		select {
		case data, ok := <-p.stdoutCh:
			if !ok {
				return string(buf), -1, true, nil
			}
			buf = append(buf, data...)
			if idx, rawEnd := matchRaw(buf, matchers); idx >= 0 {
				p.mu.Lock()
				p.pushback = append([]byte{}, buf[rawEnd:]...)
				p.mu.Unlock()
				return string(buf[:rawEnd]), idx, false, nil
			}
			// matchers 为空时继续 select 等下一个 quiet period
		case <-time.After(timeout):
			if len(matchers) == 0 {
				return string(buf), -1, false, nil
			}
			return string(buf), -1, true, nil
		case <-p.doneCh:
			return string(buf), -1, true, nil
		}
	}
}

// matchRaw 在 raw 输出（含 ANSI）中尝试每个 matcher（针对 stripped 输出）。
// 返回 (matchedIdx, rawEndPos)；matchedIdx=-1 表示未命中。
// rawEndPos 是 match 末尾在 raw 中的字节位置，用于切分 pushback。
func matchRaw(buf []byte, matchers []*regexp.Regexp) (int, int) {
	if len(matchers) == 0 {
		return -1, 0
	}
	stripped, posMap := stripANSIWithPos(string(buf))
	for idx, m := range matchers {
		if loc := m.FindStringIndex(stripped); loc != nil {
			return idx, posMap[loc[1]]
		}
	}
	return -1, 0
}

// stripANSIWithPos 剥离 ANSI CSI / OSC 序列并返回 stripped 输出 + 位置映射。
// posMap[i] = stripped 中第 i 字节对应的 raw 字节偏移；posMap[len(stripped)] = len(raw)。
// 用于在 raw 输出中切分 match 边界（pushback trailing 保留 raw 形式）。
// 使用包级 ansiRe（定义在 normalize.go，同时匹配 CSI 与 OSC）。
func stripANSIWithPos(s string) (string, []int) {
	var stripped strings.Builder
	posMap := make([]int, 0, len(s)+1)
	posMap = append(posMap, 0) // posMap[0] = 0: stripped 起点对应 raw 起点偏移 0

	matches := ansiRe.FindAllStringIndex(s, -1)
	lastEnd := 0
	for _, m := range matches {
		// 拷贝 [lastEnd, m[0]) 区间的 raw 字节
		for i := lastEnd; i < m[0]; i++ {
			stripped.WriteByte(s[i])
			posMap = append(posMap, i+1)
		}
		// 跳过 [m[0], m[1]) 区间的 ANSI 序列
		lastEnd = m[1]
	}
	// 拷贝尾部 [lastEnd, len(s)) 区间的 raw 字节
	for i := lastEnd; i < len(s); i++ {
		stripped.WriteByte(s[i])
		posMap = append(posMap, i+1)
	}
	return stripped.String(), posMap
}

// Send 实现 loginflow.PTY 接口。等价于 SendInput。
func (p *PtyConn) Send(s string) error {
	return p.SendInput(s)
}

// SendSpecial 实现 Conn 接口。把命名控制字符编码为字节写入 PTY stdin。
// key: "ctrl-c"(\x03) | "ctrl-d"(\x04) | "ctrl-z"(\x1a) | "tab"(\t) | "esc"(\x1b)
func (p *PtyConn) SendSpecial(key string) error {
	b, ok := encodeSpecial(key)
	if !ok {
		return fmt.Errorf("unknown special key %q", key)
	}
	return p.SendInput(string(b))
}

func encodeSpecial(key string) (byte, bool) {
	switch key {
	case "ctrl-c":
		return 0x03, true
	case "ctrl-d":
		return 0x04, true
	case "ctrl-z":
		return 0x1a, true
	case "tab":
		return '\t', true
	case "esc":
		return 0x1b, true
	default:
		return 0, false
	}
}

// Close 实现 Conn 接口。关闭 sftp 通道、session 与底层 SSH client。
// 重复调用是 no-op。
func (p *PtyConn) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	sftpClient := p.sftpClient
	p.sftpClient = nil
	p.mu.Unlock()

	close(p.doneCh)
	var errs []string
	if sftpClient != nil {
		if err := sftpClient.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("sftp: %v", err))
		}
	}
	if err := p.session.Close(); err != nil {
		errs = append(errs, fmt.Sprintf("session: %v", err))
	}
	if err := p.client.Close(); err != nil {
		errs = append(errs, fmt.Sprintf("client: %v", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("close: %s", strings.Join(errs, "; "))
	}
	return nil
}

// readUntilPattern 从 stdoutCh 读取直到 data 包含 pattern 或超时。
// 超时返回错误。
func (p *PtyConn) readUntilPattern(pattern string, timeout time.Duration) (string, error) {
	out, timedOut := p.readUntilPatternTimeout(pattern, timeout)
	if timedOut {
		return out, fmt.Errorf("timeout waiting for %q after %s; got: %q", pattern, timeout, out)
	}
	return out, nil
}

// readUntilPatternTimeout 从 stdoutCh 读取直到 data 包含 pattern 或超时。
// 返回 (累积输出, 是否超时)。pattern 之后的 trailing data 存入 pushback 供下次 Read。
func (p *PtyConn) readUntilPatternTimeout(pattern string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	var buf []byte

	// 先消费 pushback
	p.mu.Lock()
	if len(p.pushback) > 0 {
		buf = append(buf, p.pushback...)
		p.pushback = nil
	}
	p.mu.Unlock()

	if idx := bytes.Index(buf, []byte(pattern)); idx >= 0 {
		// pushback 已包含 pattern：切分，trailing 回存
		end := idx + len(pattern)
		p.mu.Lock()
		p.pushback = append([]byte{}, buf[end:]...)
		p.mu.Unlock()
		return string(buf[:end]), false
	}

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return string(buf), true
		}
		select {
		case data, ok := <-p.stdoutCh:
			if !ok {
				// reader 已关闭
				return string(buf), true
			}
			buf = append(buf, data...)
			if idx := bytes.Index(buf, []byte(pattern)); idx >= 0 {
				end := idx + len(pattern)
				p.mu.Lock()
				p.pushback = append([]byte{}, buf[end:]...)
				p.mu.Unlock()
				return string(buf[:end]), false
			}
		case <-time.After(remaining):
			return string(buf), true
		case <-p.doneCh:
			return string(buf), true
		}
	}
}
