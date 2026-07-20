package ssh

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"sshmng/internal/config"
)

// fakeShellServer 是一个用于集成测试的 SSH server。
// 它接受 SSH 连接、分配 session、在 session 中跑一个 Go 实现的 fake shell。
// fake shell 模拟真实 shell 在 RC 注入后的行为：执行命令、发射 exit/PS1 sentinel。
type fakeShellServer struct {
	t        *testing.T
	listener net.Listener
	hostKey  ssh.Signer
	wg       sync.WaitGroup
}

func newFakeShellServer(t *testing.T) *fakeShellServer {
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
	s := &fakeShellServer{t: t, listener: l, hostKey: signer}
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

// handleSession 处理一个 session channel：响应 pty-req / shell 请求，启动 fake shell。
func (s *fakeShellServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer s.wg.Done()
	defer ch.Close()
	ptyRequested := false
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			ptyRequested = true
			req.Reply(true, nil)
		case "shell":
			req.Reply(true, nil)
			runFakeShell(ch, ptyRequested)
			return
		default:
			req.Reply(false, nil)
		}
	}
}

// runFakeShell 实现 fake shell：读行、解析 RC 注入、执行命令、发射 sentinel。
// 模拟真实 shell 在 RC 注入后的行为。不实现 PTY echo（RC 注入后 stty -echo）。
//
// 关键时序：sid 从 `export PS1='__P_<sid>__> '` 行提取，但 PS1 sentinel 只在
// RC 结束（`stty -echo` 行）后才打印——模拟真实 shell 处理完整个 RC 后才提示。
// 这避免 client 在 RC 还没消费完时看到 PS1 sentinel 就发命令，导致 fake shell
// 把后续 RC 行当命令执行报语法错误。
func runFakeShell(ch ssh.Channel, _ bool) {
	reader := bufio.NewReader(ch)
	var sid string
	rcDone := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
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

		// RC 注入：从 `export PS1='__P_<sid>__> '` 提取 sid（但暂不打印 sentinel）
		if sid == "" && strings.Contains(line, "export PS1='__P_") {
			re := regexp.MustCompile(`__P_([0-9a-f]+)__>`)
			m := re.FindStringSubmatch(line)
			if len(m) > 1 {
				sid = m[1]
			}
			continue
		}

		// sid 已提取但 RC 还没结束：识别 `stty -echo` 作为 RC 结束标记，打印首个 PS1 sentinel
		if sid != "" && !rcDone {
			if strings.Contains(line, "stty -echo") {
				rcDone = true
				fmt.Fprintf(ch, "__P_%s__> ", sid)
				continue
			}
			// 其他 RC 行：忽略
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
	ptyConn, err := NewPtyConn(client, sid)
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
	ptyConn, err := NewPtyConn(client, sid)
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
	ptyConn, err := NewPtyConn(client, sid)
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
	ptyConn, err := NewPtyConn(client, sid)
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
	ptyConn, err := NewPtyConn(client, sid)
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
	ptyConn, err := NewPtyConn(client, sid)
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

// 确保 io 接口被引用（避免未使用 import 误报）
var _ = io.EOF
