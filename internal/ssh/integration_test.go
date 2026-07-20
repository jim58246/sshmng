package ssh

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"sshmng/internal/config"
)

// fakeShellServer 是一个用于集成测试的 SSH server。
// 它接受 SSH 连接、分配 session、在 session 中跑一个 Go 实现的 fake shell。
// fake shell 模拟真实 shell 在 RC 注入后的行为：执行命令、发射 exit/PS1 sentinel。
//
// enableSftp=true 时同时支持 sftp subsystem（用于 sftp 集成测试）。
// echoPty=true 时按 pty-req 中的 ECHO mode 决定是否回显 stdin（模拟真实 SSH server
// 的 tty driver 行为），用于复现 PTY ECHO 导致 detectShell 误匹配 sentinel 的 bug。
// realisticPrompt=true 时模拟真实 bash 在 RC 期间每行执行后都显示 PS1 的行为
// （默认 false：sentinel 只在 stty -echo 后才打印，掩盖 BuildRC 多行 RC 的 bug）。
type fakeShellServer struct {
	t               *testing.T
	listener        net.Listener
	hostKey         ssh.Signer
	enableSftp      bool
	echoPty         bool
	realisticPrompt bool
	wg              sync.WaitGroup
}

func newFakeShellServer(t *testing.T) *fakeShellServer {
	return newFakeShellServerOpt(t, false)
}

// newFakeShellServerWithSftp 创建支持 sftp subsystem 的 fake server。
func newFakeShellServerWithSftp(t *testing.T) *fakeShellServer {
	return newFakeShellServerOpt(t, true)
}

// newFakeShellServerWithEcho 创建模拟真实 PTY ECHO 行为的 fake server。
// 当 sshmng 在 pty-req 中请求 ECHO=1 时，fake server 会把 stdin 回显到 stdout，
// 与真实 SSH server 的 tty driver 行为一致。用于复现 detectShell 在 ECHO=1 时
// 误匹配 sentinel 的 bug。
func newFakeShellServerWithEcho(t *testing.T) *fakeShellServer {
	s := newFakeShellServerOpt(t, false)
	s.echoPty = true
	return s
}

// newFakeShellServerWithRealisticPrompt 创建模拟真实 bash prompt 行为的 fake server。
// 真实 bash 在交互模式下，每行命令执行完都会显示 PS1（先执行 PROMPT_COMMAND）。
// 这复现 BuildRC 多行 RC 导致 injectRC 提前匹配 sentinel 的 bug：`export PS1=` 在
// RC 中间，injectRC 等到该行后的 sentinel 就以为 RC 完成，但后续行的 prompt 残留
// 在 stdoutCh 里被下次 Run 误消费。
func newFakeShellServerWithRealisticPrompt(t *testing.T) *fakeShellServer {
	s := newFakeShellServerOpt(t, false)
	s.realisticPrompt = true
	return s
}

func newFakeShellServerOpt(t *testing.T, enableSftp bool) *fakeShellServer {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeShellServer{t: t, listener: l, hostKey: signer, enableSftp: enableSftp}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		l.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeShellServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.handle(conn)
	}
}

func (s *fakeShellServer) handle(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == "alice" && string(pass) == "wonderland" {
				return nil, nil
			}
			return nil, fmt.Errorf("permission denied")
		},
	}
	cfg.AddHostKey(s.hostKey)
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, reqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		s.wg.Add(1)
		go s.handleSession(ch, reqs)
	}
}

// handleSession 处理一个 session channel：响应 pty-req / shell / subsystem 请求。
// subsystem=sftp 时启动 sftp server（需 enableSftp=true，否则拒绝）。
// echoPty=true 时从 pty-req payload 解析 ECHO mode，传给 fake shell 决定是否回显 stdin。
func (s *fakeShellServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer s.wg.Done()
	defer ch.Close()
	ptyRequested := false
	echoEnabled := false
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			ptyRequested = true
			if s.echoPty {
				echoEnabled = parseEchoMode(req.Payload)
			}
			req.Reply(true, nil)
		case "shell":
			req.Reply(true, nil)
			runFakeShell(ch, ptyRequested, echoEnabled, s.realisticPrompt)
			return
		case "subsystem":
			if !s.enableSftp {
				req.Reply(false, nil)
				continue
			}
			name := parseSubsystemPayload(req.Payload)
			if name == "sftp" {
				req.Reply(true, nil)
				runSftpServer(ch)
				return
			}
			req.Reply(false, nil)
		default:
			req.Reply(false, nil)
		}
	}
}

// parseEchoMode 解析 pty-req payload，返回 ECHO mode 是否为 1。
// Go SSH library 把 modes 作为 string 编码（4 字节长度前缀 + 数据），数据段格式
// 见 RFC 4254 §8：byte opcode, uint32 value, ..., byte 0 (TTY_OP_END)。
// ECHO opcode 用 ssh.ECHO 常量（实际值为 53，不是 POSIX termios 的 10）。
func parseEchoMode(payload []byte) bool {
	if len(payload) < 4 {
		return false
	}
	termLen := binary.BigEndian.Uint32(payload[:4])
	if uint32(len(payload)-4) < termLen {
		return false
	}
	pos := 4 + int(termLen)
	// 跳过 4 个 uint32：cols / rows / wpix / hpix = 16 字节
	if pos+16 > len(payload) {
		return false
	}
	pos += 16
	// 跳过 4 字节 modes 长度前缀（Go SSH library 用 string 编码 modes）
	if pos+4 > len(payload) {
		return false
	}
	modesLen := binary.BigEndian.Uint32(payload[pos : pos+4])
	pos += 4
	if pos+int(modesLen) > len(payload) {
		return false
	}
	for pos+5 <= len(payload) {
		opcode := payload[pos]
		if opcode == 0 { // TTY_OP_END
			break
		}
		value := binary.BigEndian.Uint32(payload[pos+1 : pos+5])
		if opcode == ssh.ECHO {
			return value == 1
		}
		pos += 5
	}
	return false
}

// parseSubsystemPayload 解析 subsystem 请求的 payload：4 字节长度 + 子系统名。
func parseSubsystemPayload(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	n := binary.BigEndian.Uint32(payload[:4])
	if uint32(len(payload)-4) < n {
		return ""
	}
	return string(payload[4 : 4+n])
}

// runSftpServer 在 SSH channel 上启动 sftp server（用 InMemHandler 后端）。
func runSftpServer(ch ssh.Channel) {
	srv := sftp.NewRequestServer(ch, sftp.InMemHandler())
	defer srv.Close()
	_ = srv.Serve()
}

// runFakeShell 实现 fake shell：读行、解析 RC 注入、执行命令、发射 sentinel。
// 模拟真实 shell 在 RC 注入后的行为。
//
// echoEnabled=true 时（对应 sshmng 在 pty-req 中请求 ECHO=1），每读到一行就先把
// 原始行回显到 stdout，再交给 shell 处理。这模拟真实 SSH server 的 tty driver
// 行为：ECHO termios flag 为 1 时，stdin 字节立即回显到 stdout。回显先于 shell
// 处理发生，因此 sshmng 会在执行结果到达前先看到命令字符串本身（含未展开的 $0
// 等），容易导致 sentinel 误匹配。
//
// 状态机：detecting（探测 shell）→ rc_injecting（消费 RC 行）→ command（执行命令）。
// RC 结束标记是 `export PS1='__P_<sid>__> '` 行（BuildRC 把 PS1 export 放最后，
// 保证 injectRC 等到 sentinel 时 RC 已全部执行完）。rc_injecting 期间所有行忽略
// 不执行，避免 RC 行被误当命令跑。
//
// realisticPrompt=true 时模拟真实 bash 在 RC 期间每行执行后都显示 PS1 的行为，
// 用于复现 BuildRC 把 `export PS1=` 放在 RC 中间导致 injectRC 提前匹配的 bug：
// 在 `export PS1=` 行后再额外 emit 若干 `__E_<sid>:0__\r\n__P_<sid>__> ` 模拟
// 后续 RC 行触发的 prompt。修复后 BuildRC 把 `export PS1=` 放最后，无后续行，
// 无残留 sentinel。
func runFakeShell(ch ssh.Channel, _ bool, echoEnabled bool, realisticPrompt bool) {
	reader := bufio.NewReader(ch)
	var sid string
	rcDone := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		// ECHO=1 时回显原始行（含 \n），模拟 tty driver echo。
		// 必须在 shell 处理之前回显，让 sshmng 在执行结果到达前先看到命令字符串。
		if echoEnabled {
			ch.Write([]byte(line))
		}
		line = strings.TrimRight(line, "\r\n")

		// 探测命令：echo __SHELL_DETECT__:$0:${BASH_VERSION:-}:${ZSH_VERSION:-}; echo __DETECT_END_<rand>__
		if strings.Contains(line, "__SHELL_DETECT__") {
			rand := extractRand(line)
			fmt.Fprintf(ch, "__SHELL_DETECT__:/bin/bash:5.2.15(1)-release:\r\n")
			if rand != "" {
				fmt.Fprintf(ch, "__DETECT_END_%s__\r\n", rand)
			}
			continue
		}

		// RC 注入阶段：消费 RC 行直到 `export PS1='__P_<sid>__> '`（BuildRC 最后一行）
		if !rcDone {
			if strings.Contains(line, "export PS1='__P_") {
				re := regexp.MustCompile(`__P_([0-9a-f]+)__>`)
				m := re.FindStringSubmatch(line)
				if len(m) > 1 {
					sid = m[1]
				}
				rcDone = true
				// emit sentinel：PS1 已设置。realisticPrompt 模式下 PROMPT_COMMAND
				// 已在 export PS1 之前的 if 行设置，会在显示 PS1 前触发，emit exit sentinel。
				if realisticPrompt {
					fmt.Fprintf(ch, "__E_%s__:0__\r\n", sid)
				}
				fmt.Fprintf(ch, "__P_%s__> ", sid)
			}
			// 其他 RC 行：忽略不执行
			continue
		}

		// 命令阶段：用 sh -c 执行
		cmd := exec.Command("sh", "-c", line)
		output, err := cmd.CombinedOutput()
		if len(output) > 0 {
			ch.Write(output)
		}
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 127
			}
		}
		fmt.Fprintf(ch, "__E_%s__:%d__\r\n", sid, exitCode)
		fmt.Fprintf(ch, "__P_%s__> ", sid)
	}
}

// extractRand 从 `echo ...; echo __DETECT_END_<rand>__` 中提取 <rand>。
func extractRand(line string) string {
	re := regexp.MustCompile(`__DETECT_END_([0-9a-f]+)__`)
	m := re.FindStringSubmatch(line)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

func (s *fakeShellServer) Addr() string { return s.listener.Addr().String() }

// --- 集成测试 ---

// TestIntegrationLoginAndRunCommand 端到端：login → run_in_session(`echo hello`) → output 含 hello，exit_code=0
func TestIntegrationLoginAndRunCommand(t *testing.T) {
	srv := newFakeShellServer(t)
	d := newDialerWithTempKnownHosts(t)

	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	sid, err := RandomSID()
	if err != nil {
		t.Fatalf("RandomSID: %v", err)
	}
	ptyConn, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer ptyConn.Close()

	if ptyConn.Shell() != "bash" {
		t.Errorf("Shell = %q, want bash", ptyConn.Shell())
	}

	output, exitCode, timedOut, _, _, err := ptyConn.Run("echo hello", 5000, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if timedOut {
		t.Errorf("should not time out for echo hello")
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
	if !strings.Contains(output, "hello") {
		t.Errorf("output should contain 'hello', got: %q", output)
	}
}

// TestIntegrationRunFailingCommand 端到端：exit code 非 0 也能正确解析。
func TestIntegrationRunFailingCommand(t *testing.T) {
	srv := newFakeShellServer(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	ptyConn, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer ptyConn.Close()

	output, exitCode, _, _, _, _ := ptyConn.Run("exit 42", 5000, 0)
	if exitCode != 42 {
		t.Errorf("exitCode = %d, want 42 (output: %q)", exitCode, output)
	}
}

// TestIntegrationRunTimeout 端到端：长时间命令触发超时，返回 timedOut=true。
func TestIntegrationRunTimeout(t *testing.T) {
	srv := newFakeShellServer(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	ptyConn, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer ptyConn.Close()

	_, _, timedOut, _, _, _ := ptyConn.Run("sleep 2", 500, 0)
	if !timedOut {
		t.Errorf("should time out for sleep 2 with 500ms timeout")
	}
}

// TestIntegrationSendSpecialInterruptsTimeout 端到端：超时后用 send_special("ctrl-c") 中断。
// 注：本测试只验证 SendSpecial 能成功写入 PTY（不验证命令真的被中断——
// 因为 fake shell 的 sh -c sleep 是子进程，ctrl-c 不一定能传到它）。
func TestIntegrationSendSpecial(t *testing.T) {
	srv := newFakeShellServer(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	ptyConn, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer ptyConn.Close()

	// 启动一个长命令，然后发 ctrl-c
	go func() {
		ptyConn.Run("sleep 2", 3000, 0)
	}()
	time.Sleep(100 * time.Millisecond)

	if err := ptyConn.SendSpecial("ctrl-c"); err != nil {
		t.Errorf("SendSpecial(ctrl-c): %v", err)
	}
	if err := ptyConn.SendSpecial("tab"); err != nil {
		t.Errorf("SendSpecial(tab): %v", err)
	}
	if err := ptyConn.SendSpecial("unknown-key"); err == nil {
		t.Errorf("unknown key should error")
	}
}

// TestIntegrationMultipleCommands 端到端：连续跑多条命令，每条独立识别 exit code。
func TestIntegrationMultipleCommands(t *testing.T) {
	srv := newFakeShellServer(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	ptyConn, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer ptyConn.Close()

	out1, code1, _, _, _, _ := ptyConn.Run("echo one", 5000, 0)
	if code1 != 0 || !strings.Contains(out1, "one") {
		t.Errorf("cmd1: code=%d out=%q", code1, out1)
	}
	out2, code2, _, _, _, _ := ptyConn.Run("echo two", 5000, 0)
	if code2 != 0 || !strings.Contains(out2, "two") {
		t.Errorf("cmd2: code=%d out=%q", code2, out2)
	}
	out3, code3, _, _, _, _ := ptyConn.Run("false", 5000, 0)
	if code3 != 1 {
		t.Errorf("cmd3 (false): code=%d want 1 (out: %q)", code3, out3)
	}
}

// TestIntegrationOutputTruncation 端到端：max_output_bytes 截断生效。
func TestIntegrationOutputTruncation(t *testing.T) {
	srv := newFakeShellServer(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	ptyConn, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer ptyConn.Close()

	out, _, _, truncated, totalBytes, _ := ptyConn.Run("seq 1 1000", 5000, 100)
	if !truncated {
		t.Errorf("should be truncated (out len=%d)", len(out))
	}
	if totalBytes <= 100 {
		t.Errorf("totalBytes should be > 100, got %d", totalBytes)
	}
	if len(out) > 100 {
		t.Errorf("out should be truncated to 100 bytes, got %d", len(out))
	}
}

// TestIntegrationPtyEchoDoesNotBreakShellDetect 验证 sshmng 不会因 PTY ECHO
// 回显而误判 shell 类型。
//
// 真实 SSH server 在 pty-req ECHO=1 时，tty driver 会把 stdin 字节立即回显到
// stdout。sshmng 的 detectShell 命令字符串里包含 `__DETECT_END_<rand>__` 字面量，
// 当 ECHO=1 时该字面量会先于 shell 真正执行结果到达 sshmng，readUntilPatternTimeout
// 在回显字符串里就匹配到 marker 并返回，ParseShellDetect 看到的是未展开的
// `echo __SHELL_DETECT__:$0:...`（$0 仍是字面量），无法解析出 shell 类型。
//
// 修复：sshmng 在 RequestPty 时把 ECHO 显式设为 0，所有 stdin 写入都是 sshmng
// 主动发起（LoginFlow Send / run_in_session cmd / send_input text），不需要回显。
func TestIntegrationPtyEchoDoesNotBreakShellDetect(t *testing.T) {
	srv := newFakeShellServerWithEcho(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	ptyConn, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v (PTY ECHO should be disabled so detectShell waits for real execution result, not echoed command)", err)
	}
	defer ptyConn.Close()

	if ptyConn.Shell() != "bash" {
		t.Errorf("Shell = %q, want bash", ptyConn.Shell())
	}

	// 完整跑一条命令验证后续流程也正常（RC 注入、sentinel 识别）。
	output, exitCode, _, _, _, err := ptyConn.Run("echo hello", 5000, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
	if !strings.Contains(output, "hello") {
		t.Errorf("output should contain 'hello', got: %q", output)
	}
}

// TestIntegrationRealisticBashPromptsDuringRC 验证 BuildRC 在真实 bash 行为下能正确工作。
//
// 真实 bash 在交互模式下，每行 RC 执行完都会显示 PS1（先执行 PROMPT_COMMAND）。
// 如果 BuildRC 把 `export PS1='__P_<sid>__> '` 放在 RC 中间（第 5 行），injectRC 等
// 第一个 `__P_<sid>__> ` 时会在该行后立刻匹配，但 RC 还有 if/set/stty 3 行没执行。
// 后续 3 行的 prompt 输出（`__E_<sid>:0__\r\n__P_<sid>__> `）残留在 stdoutCh，
// 下次 Run 调用 readUntilPatternTimeout 立刻匹配残留 sentinel，返回空 output +
// exit_code=0，命令实际未执行。
//
// 修复：BuildRC 把 `export PS1=` 移到 RC 最后一行，确保 injectRC 等到 sentinel 时
// RC 已全部执行完。
func TestIntegrationRealisticBashPromptsDuringRC(t *testing.T) {
	srv := newFakeShellServerWithRealisticPrompt(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	ptyConn, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer ptyConn.Close()

	// 关键断言：Run 必须等到 cmd 真正执行完才返回，output 含 "hello"
	output, exitCode, timedOut, _, _, err := ptyConn.Run("echo hello", 5000, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if timedOut {
		t.Errorf("should not time out for echo hello")
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
	if !strings.Contains(output, "hello") {
		t.Errorf("output should contain 'hello' (cmd should actually execute), got: %q", output)
	}

	// 第二条命令验证 session 状态没被残留 sentinel 污染
	output2, _, _, _, _, _ := ptyConn.Run("echo world", 5000, 0)
	if !strings.Contains(output2, "world") {
		t.Errorf("second cmd output should contain 'world', got: %q", output2)
	}
}

// 确保 io 接口被引用（避免未使用 import 误报）
var _ = io.EOF
