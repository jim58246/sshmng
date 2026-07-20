package ssh

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// 默认超时：未指定 timeoutMs 时使用。
const defaultCmdTimeout = 30 * time.Second

// shellDetectTimeout 是 shell 类型探测的超时。
const shellDetectTimeout = 5 * time.Second

// rcInjectTimeout 是 RC 注入后等待首个 PS1 sentinel 的超时。
const rcInjectTimeout = 5 * time.Second

// PtyConn 包装 *ssh.Session + PTY，实现 Conn 接口。
// 注入 sentinel 后即可通过 Run 执行单条命令并自动识别命令边界 + exit code。
type PtyConn struct {
	session *ssh.Session
	client  *ssh.Client
	stdin   io.WriteCloser
	stdout  io.Reader
	sid     string
	shell   string

	// 单 reader goroutine：所有读取通过 stdoutCh 序列化，避免多个 goroutine 竞争
	// 同一 SSH channel 的 Read。
	stdoutCh chan []byte
	doneCh   chan struct{}
	closed   bool

	mu sync.Mutex
}

// NewPtyConn 在已建立的 SSH 连接上分配 PTY、启动 shell、探测 shell 类型、注入 RC。
// 调用者负责在失败时 Close。
func NewPtyConn(client *ssh.Client, sid string) (*PtyConn, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
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
	if err := p.injectRC(); err != nil {
		p.Close()
		return nil, fmt.Errorf("inject rc: %w", err)
	}
	return p, nil
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

// Close 实现 Conn 接口。关闭 session 与底层 SSH client。
// 重复调用是 no-op。
func (p *PtyConn) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	close(p.doneCh)
	var errs []string
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
// 返回 (累积输出, 是否超时)。
func (p *PtyConn) readUntilPatternTimeout(pattern string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	var buf []byte
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
			if bytes.Contains(buf, []byte(pattern)) {
				return string(buf), false
			}
		case <-time.After(remaining):
			return string(buf), true
		case <-p.doneCh:
			return string(buf), true
		}
	}
}
