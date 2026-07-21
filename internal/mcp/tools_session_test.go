package mcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkg/sftp"
	cryptossh "golang.org/x/crypto/ssh"
	"sshmng/internal/config"
	"sshmng/internal/ssh/conn"
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

// TestLoginRejectsPatternA 校验 v1 phase 4 拒绝 Pattern A（SSHJ=true 的 jumphost），
// 该形态留 v1.x 实现。Pattern B（SSHJ=false）由 TestIntegrationPatternBEndToEnd 覆盖。
func TestLoginRejectsPatternA(t *testing.T) {
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
		t.Errorf("expected IsError=true for Pattern A (SSHJ=true not yet supported)")
	}
	text := resultText(t, res)
	if !strings.Contains(text, "jumphost") {
		t.Errorf("error should mention 'jumphost', got: %s", text)
	}
}

// TestLoginAcceptsLoginFlowButFailsOnDial 校验 v1 phase 3 接受带 LoginFlow 的 server
// （不再以 "not supported" 拒绝），但因为 addr 不可达所以仍返回 IsError=true。
func TestLoginAcceptsLoginFlowButFailsOnDial(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version: "1",
		Servers: []*config.SSHServer{
			{
				Name:       "with-flow",
				Addr:       "2.2.2.2:22",
				User:       "u",
				Auth:       config.SSHAuth{Password: "p"},
				LoginFlow:  map[string]config.LoginAction{"start": {Send: "x", Expects: []config.Expect{{Pattern: "y", Next: "success"}}}},
				LoginEntry: "start",
			},
		},
	})
	res, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "with-flow"})
	if !res.IsError {
		t.Errorf("expected IsError=true (dial should fail for unreachable addr)")
	}
	// 不能再以 "not supported" 拒绝
	if msg := resultText(t, res); strings.Contains(msg, "not supported") {
		t.Errorf("phase 3 should accept LoginFlow; got: %s", msg)
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
// enableSftp=true 时同时支持 sftp subsystem。
type fakeShellServerForMCP struct {
	t          *testing.T
	listener   net.Listener
	hostKey    cryptossh.Signer
	enableSftp bool
	wg         sync.WaitGroup
}

func newFakeShellServerForMCP(t *testing.T) *fakeShellServerForMCP {
	return newFakeShellServerForMCPOpt(t, false)
}

// newFakeShellServerWithSftpForMCP 创建支持 sftp subsystem 的 fake server。
func newFakeShellServerWithSftpForMCP(t *testing.T) *fakeShellServerForMCP {
	return newFakeShellServerForMCPOpt(t, true)
}

func newFakeShellServerForMCPOpt(t *testing.T, enableSftp bool) *fakeShellServerForMCP {
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
	s := &fakeShellServerForMCP{t: t, listener: l, hostKey: signer, enableSftp: enableSftp}
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
		case "subsystem":
			if !s.enableSftp {
				req.Reply(false, nil)
				continue
			}
			if parseSubsystemPayloadMCP(req.Payload) == "sftp" {
				req.Reply(true, nil)
				runSftpServerForMCP(ch)
				return
			}
			req.Reply(false, nil)
		default:
			req.Reply(false, nil)
		}
	}
}

// parseSubsystemPayloadMCP 解析 subsystem 请求 payload：4 字节长度 + 子系统名。
func parseSubsystemPayloadMCP(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	n := binary.BigEndian.Uint32(payload[:4])
	if uint32(len(payload)-4) < n {
		return ""
	}
	return string(payload[4 : 4+n])
}

// runSftpServerForMCP 在 SSH channel 上启动 sftp server（InMemHandler 后端）。
func runSftpServerForMCP(ch cryptossh.Channel) {
	srv := sftp.NewRequestServer(ch, sftp.InMemHandler())
	defer srv.Close()
	_ = srv.Serve()
}

func runFakeShellForMCP(ch cryptossh.Channel) {
	reader := bufio.NewReader(ch)
	var sid string
	var tok string
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
		// RC 阶段：消费 RC 行直到 `export PS1='__P_<sid>__> '`（BuildRC 最后一行）
		if !rcDone {
			if strings.Contains(line, "export PS1='__P_") {
				re := regexp.MustCompile(`__P_([0-9a-f]+)__>`)
				m := re.FindStringSubmatch(line)
				if len(m) > 1 {
					sid = m[1]
				}
				rcDone = true
				fmt.Fprintf(ch, "__P_%s__> ", sid)
			}
			// 其他 RC 行：忽略
			continue
		}
		// setup token 命令：`__sshmng_tok=<token>; export __sshmng_tok; export PS1='__P_<sid>_<token>__> '`
		// 记录 token，emit setup sentinel（含 token）。不当作正常命令执行。
		if strings.HasPrefix(line, "__sshmng_tok=") {
			re := regexp.MustCompile(`__sshmng_tok=([0-9a-f]+)`)
			m := re.FindStringSubmatch(line)
			if len(m) > 1 {
				tok = m[1]
			}
			fmt.Fprintf(ch, "__E_%s_%s__:0__\r\n__P_%s_%s__> ", sid, tok, sid, tok)
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
		if tok != "" {
			fmt.Fprintf(ch, "__E_%s_%s__:%d__\r\n__P_%s_%s__> ", sid, tok, exitCode, sid, tok)
		} else {
			fmt.Fprintf(ch, "__E_%s__:%d__\r\n__P_%s__> ", sid, exitCode, sid)
		}
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
	knownHosts := conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts"))
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
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

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
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	res, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "test"})
	if !res.IsError {
		t.Errorf("expected IsError=true for wrong password")
	}
	text := resultText(t, res)
	if !strings.Contains(text, "permission denied") && !strings.Contains(text, "handshake") {
		t.Errorf("error should mention permission denied or handshake, got: %s", text)
	}
}

// TestLoginDirectLoginFlowFailureReturnsTrace: Pattern A 直连 + LoginFlow 失败时，
// login 响应必须包含 login_trace 字段供 Agent 诊断（设计文档 §3.x "LoginFlow 失败
// error + login_trace"）。trace 含 send / expect / output，Agent 据此修配置重试。
//
// 此处配置一个永不匹配的 pattern + 短 TimeoutMs，让 entry action 快速超时。
// Send 用 echo test（不会 hang），fake shell 会回 output 但不含 "Password:"。
func TestLoginDirectLoginFlowFailureReturnsTrace(t *testing.T) {
	srv := newFakeShellServerForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	flow := map[string]config.LoginAction{
		"entry": {
			Send:      "echo test\n",
			Expects:   []config.Expect{{Pattern: "Password:", Next: "success"}},
			TimeoutMs: 500,
		},
	}
	store.Save(&config.Config{
		Version: "1",
		Servers: []*config.SSHServer{
			{
				Name:       "s",
				Addr:       srv.Addr(),
				User:       "alice",
				Auth:       config.SSHAuth{Password: "wonderland"},
				LoginFlow:  flow,
				LoginEntry: "entry",
			},
		},
	})
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	res, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "s"})
	if !res.IsError {
		t.Fatalf("expected IsError=true for LoginFlow failure")
	}
	r := parseJSON(t, resultText(t, res)).(map[string]any)
	if _, ok := r["login_trace"]; !ok {
		t.Fatalf("response should include login_trace for LoginFlow failure, got: %s", resultText(t, res))
	}
	trace, ok := r["login_trace"].([]any)
	if !ok || len(trace) == 0 {
		t.Fatalf("login_trace should be non-empty array, got: %v", r["login_trace"])
	}
	first, ok := trace[0].(map[string]any)
	if !ok {
		t.Fatalf("login_trace[0] should be object, got: %T", trace[0])
	}
	if first["send"] != "echo test\n" {
		t.Errorf("login_trace[0].send = %q, want 'echo test\\n'", first["send"])
	}
}
