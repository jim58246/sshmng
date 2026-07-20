package ssh

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"sshmng/internal/config"
)

// shellDetectPS1Re 从 `export PS1='__P_<sid>__> '` 中提取 sid。
var shellDetectPS1Re = regexp.MustCompile(`__P_([0-9a-f]+)__>`)

// execFakeShellCommand 用 sh -c 执行 line，返回 (output, exitCode)。
// 与 runFakeShell 中的命令执行逻辑同源，抽出供 LoginFlow 测试复用。
func execFakeShellCommand(line string) ([]byte, int) {
	cmd := exec.Command("sh", "-c", line)
	output, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 127
		}
	}
	return output, exitCode
}

// fakeShellServerForLoginFlow 是支持 LoginFlow 阶段的 fake SSH server。
//
// 与 fakeShellServer 的区别：在 shell detect 之后、RC 注入之前，先走一段 LoginFlow
// 交互——emit "login: "，根据输入响应 "Password: " / "READY\n"，然后才进入 RC 阶段。
type fakeShellServerForLoginFlow struct {
	t        *testing.T
	listener net.Listener
	hostKey  ssh.Signer
	wg       sync.WaitGroup
}

func newFakeShellServerForLoginFlow(t *testing.T) *fakeShellServerForLoginFlow {
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
	s := &fakeShellServerForLoginFlow{t: t, listener: l, hostKey: signer}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		l.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeShellServerForLoginFlow) serve() {
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

func (s *fakeShellServerForLoginFlow) handle(conn net.Conn) {
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

func (s *fakeShellServerForLoginFlow) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer s.wg.Done()
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			req.Reply(true, nil)
		case "shell":
			req.Reply(true, nil)
			runFakeShellWithLoginFlow(ch)
			return
		default:
			req.Reply(false, nil)
		}
	}
}

// runFakeShellWithLoginFlow 在 shell detect 后 emit "login: "，按 LoginFlow 输入响应：
//   - "user" → "Password: "
//   - "pass" → "READY\n"（标志 LoginFlow 完成）
//
// 然后转入正常 RC + 命令阶段（复用 runFakeShell 逻辑）。
func runFakeShellWithLoginFlow(ch ssh.Channel) {
	reader := bufio.NewReader(ch)
	var sid string
	rcDone := false
	loginFlowDone := false

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")

		// Shell detect
		if strings.Contains(line, "__SHELL_DETECT__") {
			rand := extractRand(line)
			fmt.Fprintf(ch, "__SHELL_DETECT__:/bin/bash:5.2.15(1)-release:\r\n")
			if rand != "" {
				fmt.Fprintf(ch, "__DETECT_END_%s__\r\n", rand)
			}
			// 进入 LoginFlow 阶段：emit 初始 prompt
			fmt.Fprintf(ch, "login: ")
			continue
		}

		// LoginFlow 阶段
		if !loginFlowDone {
			switch line {
			case "user":
				fmt.Fprintf(ch, "Password: ")
				continue
			case "pass":
				fmt.Fprintf(ch, "READY\n")
				loginFlowDone = true
				continue
			default:
				// 忽略未知输入（如空行）
				continue
			}
		}

		// RC 阶段
		if sid == "" && strings.Contains(line, "export PS1='__P_") {
			re := shellDetectPS1Re.FindStringSubmatch(line)
			if len(re) > 1 {
				sid = re[1]
			}
			continue
		}
		if sid != "" && !rcDone {
			if strings.Contains(line, "stty -echo") {
				rcDone = true
				fmt.Fprintf(ch, "__P_%s__> ", sid)
				continue
			}
			continue
		}

		// 命令阶段：用 sh -c 执行（复用 runFakeShell 逻辑）
		out, code := execFakeShellCommand(line)
		if len(out) > 0 {
			ch.Write(out)
		}
		fmt.Fprintf(ch, "__E_%s__:%d__\r\n", sid, code)
		fmt.Fprintf(ch, "__P_%s__> ", sid)
	}
}

func (s *fakeShellServerForLoginFlow) Addr() string { return s.listener.Addr().String() }

// --- 集成测试 ---

// TestIntegrationLoginFlowSuccess：完整走 LoginFlow → RC → 命令执行。
// LoginFlow 配置：
//   - entry: 空 Send，等 "login:" prompt
//   - send_user: Send "user\n"，等 "Password:"
//   - send_pwd: Send "pass\n"，等 "READY"
func TestIntegrationLoginFlowSuccess(t *testing.T) {
	srv := newFakeShellServerForLoginFlow(t)
	d := newDialerWithTempKnownHosts(t)

	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	sid, _ := RandomSID()
	flow := map[string]config.LoginAction{
		"entry": {
			Name:    "entry",
			Send:    "",
			Expects: []config.Expect{{Pattern: "login:*", Next: "send_user"}},
		},
		"send_user": {
			Name:    "send_user",
			Send:    "user\n",
			Expects: []config.Expect{{Pattern: "Password:*", Next: "send_pwd"}},
		},
		"send_pwd": {
			Name:    "send_pwd",
			Send:    "pass\n",
			Expects: []config.Expect{{Pattern: "READY", Next: "success"}},
		},
	}

	ptyConn, err := NewPtyConn(client, sid, &PtyConnOptions{
		LoginFlow:  flow,
		LoginEntry: "entry",
	})
	if err != nil {
		t.Fatalf("NewPtyConn with LoginFlow: %v", err)
	}
	defer ptyConn.Close()

	// LoginFlow 完成后应能正常跑命令
	output, exitCode, timedOut, _, _, err := ptyConn.Run("echo hello", 5000, 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if timedOut {
		t.Errorf("should not time out")
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
	if !strings.Contains(output, "hello") {
		t.Errorf("output should contain 'hello', got: %q", output)
	}
}

// TestIntegrationLoginFlowFailureReturnsTrace：LoginFlow pattern 不匹配时
// NewPtyConn 返回错误，且 error 含 trace 信息供诊断。
func TestIntegrationLoginFlowFailureReturnsTrace(t *testing.T) {
	srv := newFakeShellServerForLoginFlow(t)
	d := newDialerWithTempKnownHosts(t)

	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	sid, _ := RandomSID()
	// 故意配不匹配的 pattern：fake shell emit "login: " 但我们等 "Username:"
	flow := map[string]config.LoginAction{
		"entry": {
			Name:    "entry",
			Send:    "",
			Expects: []config.Expect{{Pattern: "Username:*", Next: "success"}},
		},
	}

	_, err = NewPtyConn(client, sid, &PtyConnOptions{
		LoginFlow:  flow,
		LoginEntry: "entry",
	})
	if err == nil {
		t.Fatalf("expected LoginFlow failure error, got nil")
	}
	if !strings.Contains(err.Error(), "loginflow") && !strings.Contains(err.Error(), "no expect matched") {
		t.Errorf("err = %q, want contains 'loginflow' or 'no expect matched'", err.Error())
	}
}
