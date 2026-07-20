package mcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	cryptossh "golang.org/x/crypto/ssh"
	"sshmng/internal/config"
	"sshmng/internal/ssh"
)

// --- 错误路径单元测试（不需要真实 SSH server） ---

// TestLoginUnknownServer 校验 login 对未知 server 报错。
func TestLoginUnknownServer(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Servers:      []*config.SSHServer{},
	})
	res, _, err := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "nope"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true for unknown server")
	}
}

// TestLoginRejectsJumphost 校验 v1 phase 2 拒绝带 Via 的 server。
func TestLoginRejectsJumphost(t *testing.T) {
	jumphost := &config.Jumphost{Name: "j1", Addr: "1.1.1.1:22", User: "u", SSHJ: true}
	svc := newTestService(t, &config.Config{
		Version:   "1",
		Jumphosts: []*config.Jumphost{jumphost},
		Servers: []*config.SSHServer{
			{Name: "via-jump", Addr: "2.2.2.2:22", User: "u", Auth: config.SSHAuth{Password: "p"}},
		},
	})
	// 设置 server 的 via（绕过 JSON 序列化直接设指针）
	svc.store.Load() // 触发一次加载确保 config 内部状态可用
	cfg, _ := svc.store.Load()
	for _, s := range cfg.Servers {
		if s.Name == "via-jump" {
			s.Via = jumphost
		}
	}
	svc.store.Save(cfg)

	res, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "via-jump"})
	if !res.IsError {
		t.Errorf("expected IsError=true for server with Via (jumphost not supported in phase 2)")
	}
	text := resultText(t, res)
	if !strings.Contains(text, "jumphost") {
		t.Errorf("error should mention 'jumphost', got: %s", text)
	}
}

// TestLoginRejectsLoginFlow 校验 v1 phase 2 拒绝带 LoginFlow 的 server。
func TestLoginRejectsLoginFlow(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version: "1",
		Servers: []*config.SSHServer{
			{
				Name:       "with-flow",
				Addr:       "2.2.2.2:22",
				User:       "u",
				Auth:       config.SSHAuth{Password: "p"},
				LoginFlow:  map[string]config.LoginAction{"start": {Name: "start", Send: "x", Expects: []config.Expect{{Pattern: "y", Next: "success"}}}},
				LoginEntry: "start",
			},
		},
	})
	res, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "with-flow"})
	if !res.IsError {
		t.Errorf("expected IsError=true for server with LoginFlow")
	}
}

// TestRunInSessionUnknownSID 校验 run_in_session 对未知 sid 报错。
func TestRunInSessionUnknownSID(t *testing.T) {
	svc := newTestService(t, &config.Config{Version: "1"})
	res, _, _ := svc.RunInSession(context.Background(), &mcp.CallToolRequest{}, RunInSessionArgs{SID: "nope", Cmd: "echo"})
	if !res.IsError {
		t.Errorf("expected IsError=true for unknown sid")
	}
}

// TestCloseSessionUnknownSID 校验 close_session 对未知 sid 报错。
func TestCloseSessionUnknownSID(t *testing.T) {
	svc := newTestService(t, &config.Config{Version: "1"})
	res, _, _ := svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: "nope"})
	if !res.IsError {
		t.Errorf("expected IsError=true for unknown sid")
	}
}

// TestStatEmpty 校验 stat 初始返回空数组。
func TestStatEmpty(t *testing.T) {
	svc := newTestService(t, &config.Config{Version: "1"})
	res, _, err := svc.Stat(context.Background(), &mcp.CallToolRequest{}, StatArgs{})
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if res.IsError {
		t.Fatalf("Stat should not error: %s", resultText(t, res))
	}
	v := parseJSON(t, resultText(t, res))
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("expected array, got %T", v)
	}
	if len(arr) != 0 {
		t.Errorf("got %d sessions, want 0", len(arr))
	}
}

// --- 集成测试（fake SSH server） ---

// fakeShellServerForMCP 是 mcp 测试用的 fake SSH server。
// 与 internal/ssh 包中的 fakeShellServer 实现等价（duplication 为避免循环依赖）。
type fakeShellServerForMCP struct {
	t        *testing.T
	listener net.Listener
	hostKey  cryptossh.Signer
	wg       sync.WaitGroup
}

func newFakeShellServerForMCP(t *testing.T) *fakeShellServerForMCP {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := cryptossh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeShellServerForMCP{t: t, listener: l, hostKey: signer}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		l.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeShellServerForMCP) serve() {
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

func (s *fakeShellServerForMCP) handle(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	cfg := &cryptossh.ServerConfig{
		PasswordCallback: func(c cryptossh.ConnMetadata, pass []byte) (*cryptossh.Permissions, error) {
			if c.User() == "alice" && string(pass) == "wonderland" {
				return nil, nil
			}
			return nil, fmt.Errorf("permission denied")
		},
	}
	cfg.AddHostKey(s.hostKey)
	sshConn, chans, reqs, err := cryptossh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go cryptossh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(cryptossh.UnknownChannelType, "only session")
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

func (s *fakeShellServerForMCP) handleSession(ch cryptossh.Channel, reqs <-chan *cryptossh.Request) {
	defer s.wg.Done()
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			req.Reply(true, nil)
		case "shell":
			req.Reply(true, nil)
			runFakeShellForMCP(ch)
			return
		default:
			req.Reply(false, nil)
		}
	}
}

func runFakeShellForMCP(ch cryptossh.Channel) {
	reader := bufio.NewReader(ch)
	var sid string
	rcDone := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.Contains(line, "__SHELL_DETECT__") {
			rand := extractRandMCP(line)
			fmt.Fprintf(ch, "__SHELL_DETECT__:/bin/bash:5.2.15(1)-release:\r\n")
			if rand != "" {
				fmt.Fprintf(ch, "__DETECT_END_%s__\r\n", rand)
			}
			continue
		}
		if sid == "" && strings.Contains(line, "export PS1='__P_") {
			re := regexp.MustCompile(`__P_([0-9a-f]+)__>`)
			m := re.FindStringSubmatch(line)
			if len(m) > 1 {
				sid = m[1]
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

func extractRandMCP(line string) string {
	re := regexp.MustCompile(`__DETECT_END_([0-9a-f]+)__`)
	m := re.FindStringSubmatch(line)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

func (s *fakeShellServerForMCP) Addr() string { return s.listener.Addr().String() }

// TestIntegrationLoginRunClose 端到端：login → run_in_session → close_session → stat。
func TestIntegrationLoginRunClose(t *testing.T) {
	srv := newFakeShellServerForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(dir + "/config.json")
	store.Save(&config.Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Servers: []*config.SSHServer{
			{Name: "test", Addr: srv.Addr(), User: "alice", Auth: config.SSHAuth{Password: "wonderland"}},
		},
	})
	knownHosts := ssh.NewKnownHostsStore(filepath.Join(dir, "known_hosts"))
	svc := NewService(store, knownHosts, nil)

	// 1. Login
	res, _, err := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "test"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if res.IsError {
		t.Fatalf("Login failed: %s", resultText(t, res))
	}
	loginResult := parseJSON(t, resultText(t, res)).(map[string]any)
	sid, ok := loginResult["sid"].(string)
	if !ok || sid == "" {
		t.Fatalf("Login result missing sid: %v", loginResult)
	}
	if loginResult["server_name"] != "test" {
		t.Errorf("server_name = %v, want test", loginResult["server_name"])
	}

	// 2. Stat 应返回 1 个 session
	statRes, _, _ := svc.Stat(context.Background(), &mcp.CallToolRequest{}, StatArgs{})
	stats := parseJSON(t, resultText(t, statRes)).([]any)
	if len(stats) != 1 {
		t.Errorf("after login, stat should have 1 session, got %d", len(stats))
	}

	// 3. RunInSession: echo hello
	runRes, _, err := svc.RunInSession(context.Background(), &mcp.CallToolRequest{}, RunInSessionArgs{
		SID: sid,
		Cmd: "echo hello",
	})
	if err != nil {
		t.Fatalf("RunInSession: %v", err)
	}
	if runRes.IsError {
		t.Fatalf("RunInSession failed: %s", resultText(t, runRes))
	}
	runResult := parseJSON(t, resultText(t, runRes)).(map[string]any)
	if runResult["exit_code"].(float64) != 0 {
		t.Errorf("exit_code = %v, want 0", runResult["exit_code"])
	}
	if !strings.Contains(runResult["output"].(string), "hello") {
		t.Errorf("output should contain 'hello', got: %q", runResult["output"])
	}

	// 4. CloseSession
	closeRes, _, _ := svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})
	if closeRes.IsError {
		t.Errorf("CloseSession failed: %s", resultText(t, closeRes))
	}

	// 5. Stat 应再次为空
	statRes2, _, _ := svc.Stat(context.Background(), &mcp.CallToolRequest{}, StatArgs{})
	stats2 := parseJSON(t, resultText(t, statRes2)).([]any)
	if len(stats2) != 0 {
		t.Errorf("after close, stat should have 0 sessions, got %d", len(stats2))
	}
}

// TestIntegrationRunMultipleCommands 端到端：同一 session 连续跑多条命令。
func TestIntegrationRunMultipleCommands(t *testing.T) {
	srv := newFakeShellServerForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(dir + "/config.json")
	store.Save(&config.Config{
		Version: "1",
		Servers: []*config.SSHServer{
			{Name: "test", Addr: srv.Addr(), User: "alice", Auth: config.SSHAuth{Password: "wonderland"}},
		},
	})
	svc := NewService(store, ssh.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	res, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "test"})
	sid := parseJSON(t, resultText(t, res)).(map[string]any)["sid"].(string)

	// 跑 3 条命令
	for _, cmd := range []string{"echo one", "echo two", "false"} {
		runRes, _, _ := svc.RunInSession(context.Background(), &mcp.CallToolRequest{}, RunInSessionArgs{SID: sid, Cmd: cmd})
		if runRes.IsError {
			t.Errorf("RunInSession(%q) failed: %s", cmd, resultText(t, runRes))
		}
	}
	// close
	svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})
}

// TestIntegrationLoginAuthFailure 端到端：密码错误时 login 报错。
func TestIntegrationLoginAuthFailure(t *testing.T) {
	srv := newFakeShellServerForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(dir + "/config.json")
	store.Save(&config.Config{
		Version: "1",
		Servers: []*config.SSHServer{
			{Name: "test", Addr: srv.Addr(), User: "alice", Auth: config.SSHAuth{Password: "wrong-password"}},
		},
	})
	svc := NewService(store, ssh.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	res, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "test"})
	if !res.IsError {
		t.Errorf("expected IsError=true for wrong password")
	}
	text := resultText(t, res)
	if !strings.Contains(text, "permission denied") && !strings.Contains(text, "handshake") {
		t.Errorf("error should mention permission denied or handshake, got: %s", text)
	}
}
