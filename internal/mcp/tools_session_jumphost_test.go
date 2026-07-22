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
	"sshmng/internal/ssh/conn"
)

// fakeJumphostServerForMCP 是 Pattern B 测试用的 fake SSH server。
//
// 全程在同一 SSH session 内完成（Pattern B = 交互式堡垒机，不开新 channel）：
//  1. SSH auth (alice/wonderland)
//  2. Shell 启动即 emit 菜单（真实堡垒机行为，不等 shell detect 探测）
//  3. 接受菜单选择 "1" → emit "login: "
//  4. 接受 username → emit "Password: "
//  5. 接受 password (wonderland) → emit "Welcome to prod-db!" → 转入 target shell
//  6. 两段 LoginFlow 完成后 shell detect → RC 注入 + 命令执行
type fakeJumphostServerForMCP struct {
	t          *testing.T
	listener   net.Listener
	hostKey    cryptossh.Signer
	enableSftp bool
	wg         sync.WaitGroup
}

func newFakeJumphostServerForMCP(t *testing.T) *fakeJumphostServerForMCP {
	return newFakeJumphostServerForMCPOpt(t, false)
}

// newFakeJumphostServerWithSftpForMCP 创建支持 sftp subsystem 的 jumphost fake server。
// 用于验证 Pattern B 不应探测 SFTP（即便 jumphost 自己有 SFTP）。
func newFakeJumphostServerWithSftpForMCP(t *testing.T) *fakeJumphostServerForMCP {
	return newFakeJumphostServerForMCPOpt(t, true)
}

func newFakeJumphostServerForMCPOpt(t *testing.T, enableSftp bool) *fakeJumphostServerForMCP {
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
	s := &fakeJumphostServerForMCP{t: t, listener: l, hostKey: signer, enableSftp: enableSftp}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		l.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeJumphostServerForMCP) serve() {
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

func (s *fakeJumphostServerForMCP) handle(conn net.Conn) {
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

func (s *fakeJumphostServerForMCP) handleSession(ch cryptossh.Channel, reqs <-chan *cryptossh.Request) {
	defer s.wg.Done()
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			req.Reply(true, nil)
		case "shell":
			req.Reply(true, nil)
			runFakeJumphostShellForMCP(ch)
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

// runFakeJumphostShellForMCP 模拟 Pattern B 堡垒机的状态机：
// menu → target_user → target_pass → target_shell。
//
// Shell 启动即 emit 菜单（真实堡垒机行为）；menu 阶段读 "1" 触发 target login；
// target_user 阶段读 username emit "Password: "；target_pass 阶段读 password
// 验证后 emit "Welcome to prod-db!" 转入 target_shell；target_shell 阶段响应
// shell detect（两段 LoginFlow 完成后才到），再走 RC + 命令。
func runFakeJumphostShellForMCP(ch cryptossh.Channel) {
	reader := bufio.NewReader(ch)
	var sid string
	var tok string
	rcDone := false
	phase := "menu"

	// Shell 启动即 emit 菜单（不等 shell detect——真实堡垒机行为）
	fmt.Fprintf(ch, "Welcome to Jumphost v2\r\n")
	fmt.Fprintf(ch, "Main menu:\r\n")
	fmt.Fprintf(ch, "1) prod-db\r\n")
	fmt.Fprintf(ch, "Select target: ")

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")

		// Shell detect（两段 LoginFlow 完成后才到，任何 phase 都响应）
		if strings.Contains(line, "__SHELL_DETECT__") {
			rand := extractRandMCP(line)
			fmt.Fprintf(ch, "__SHELL_DETECT__:/bin/bash:5.2.15(1)-release:\r\n")
			if rand != "" {
				fmt.Fprintf(ch, "__DETECT_END_%s__\r\n", rand)
			}
			continue
		}

		switch phase {
		case "menu":
			if line == "1" {
				fmt.Fprintf(ch, "Connecting to prod-db...\r\n")
				fmt.Fprintf(ch, "login: ")
				phase = "target_user"
			}
			continue
		case "target_user":
			fmt.Fprintf(ch, "Password: ")
			phase = "target_pass"
			continue
		case "target_pass":
			if line == "wonderland" {
				fmt.Fprintf(ch, "Welcome to prod-db!\r\n")
				phase = "target_shell"
			} else {
				fmt.Fprintf(ch, "Login incorrect\r\n")
				phase = "menu"
			}
			continue
		case "target_shell":
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
			// setup token 命令：记录 token，emit setup sentinel（含 token）
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
}

func (s *fakeJumphostServerForMCP) Addr() string { return s.listener.Addr().String() }

// --- Pattern B 集成测试 ---

// TestIntegrationPatternBEndToEnd: Pattern B 完整流程。
// Jumphost.LoginFlow（菜单就绪）→ SSHServer.LoginFlow（选 target + 输入凭据）→ RC → 命令。
func TestIntegrationPatternBEndToEnd(t *testing.T) {
	srv := newFakeJumphostServerForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))

	jumphostFlow := map[string]config.LoginAction{
		"entry": {
			Send:    "",
			Expects: []config.Expect{{Pattern: "Select target:*", Next: "success"}},
		},
	}
	serverFlow := map[string]config.LoginAction{
		"entry": {
			Send:    "1\n",
			Expects: []config.Expect{{Pattern: "login:*", Next: "send_user"}},
		},
		"send_user": {
			Send:    "alice\n",
			Expects: []config.Expect{{Pattern: "Password:*", Next: "send_pwd"}},
		},
		"send_pwd": {
			Send:    "wonderland\n",
			Expects: []config.Expect{{Pattern: "Welcome to*", Next: "success"}},
		},
	}

	jumphost := &config.Jumphost{
		Name:       "jump",
		Addr:       srv.Addr(),
		User:       "alice",
		Auth:       config.SSHAuth{Password: "wonderland"},
		SSHJ:       false,
		LoginFlow:  jumphostFlow,
		LoginEntry: "entry",
	}
	server := &config.SSHServer{
		Name:       "prod-db",
		LoginFlow:  serverFlow,
		LoginEntry: "entry",
	}
	server.Via = jumphost

	if err := store.Save(&config.Config{
		Version:   "1",
		Jumphosts: []*config.Jumphost{jumphost},
		Servers:   []*config.SSHServer{server},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	// 1. Login (Pattern B)
	res, _, err := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "prod-db"})
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
	if loginResult["server_name"] != "prod-db" {
		t.Errorf("server_name = %v, want prod-db", loginResult["server_name"])
	}

	// 2. RunInSession: echo hello
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

	// 3. CloseSession
	closeRes, _, _ := svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})
	if closeRes.IsError {
		t.Errorf("CloseSession failed: %s", resultText(t, closeRes))
	}
}

// TestPatternBDoesNotProbeSftp: Pattern B 下即便 jumphost 自己支持 sftp，
// 也不应探测 sftp（因为 sftp 通道是到 jumphost 的，不是到 target 的）。
// 期望 sftp_available=false；sftp 探测若发生会成功（jumphost 支持），导致断言失败。
func TestPatternBDoesNotProbeSftp(t *testing.T) {
	srv := newFakeJumphostServerWithSftpForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))

	jumphostFlow := map[string]config.LoginAction{
		"entry": {
			Send:    "",
			Expects: []config.Expect{{Pattern: "Select target:*", Next: "success"}},
		},
	}
	serverFlow := map[string]config.LoginAction{
		"entry": {
			Send:    "1\n",
			Expects: []config.Expect{{Pattern: "login:*", Next: "send_user"}},
		},
		"send_user": {
			Send:    "alice\n",
			Expects: []config.Expect{{Pattern: "Password:*", Next: "send_pwd"}},
		},
		"send_pwd": {
			Send:    "wonderland\n",
			Expects: []config.Expect{{Pattern: "Welcome to*", Next: "success"}},
		},
	}

	jumphost := &config.Jumphost{
		Name:       "jump",
		Addr:       srv.Addr(),
		User:       "alice",
		Auth:       config.SSHAuth{Password: "wonderland"},
		SSHJ:       false,
		LoginFlow:  jumphostFlow,
		LoginEntry: "entry",
	}
	server := &config.SSHServer{
		Name:       "prod-db",
		LoginFlow:  serverFlow,
		LoginEntry: "entry",
	}
	server.Via = jumphost

	if err := store.Save(&config.Config{
		Version:   "1",
		Jumphosts: []*config.Jumphost{jumphost},
		Servers:   []*config.SSHServer{server},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	res, _, err := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "prod-db"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if res.IsError {
		t.Fatalf("Login failed: %s", resultText(t, res))
	}
	loginResult := parseJSON(t, resultText(t, res)).(map[string]any)
	sid, _ := loginResult["sid"].(string)
	defer svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	if loginResult["sftp_available"] != false {
		t.Errorf("Pattern B sftp_available = %v, want false (SFTP must not be probed; SFTP goes to jumphost not target)",
			loginResult["sftp_available"])
	}
}

// TestIntegrationPatternBJumphostFlowFailure: Jumphost.LoginFlow 失败（pattern 不匹配）时
// login 报错，error 含 loginflow / no expect matched 供诊断，且响应包含 login_trace。
func TestIntegrationPatternBJumphostFlowFailure(t *testing.T) {
	srv := newFakeJumphostServerForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))

	// 故意配不匹配的 pattern：fake server emit "Select target:" 但我们等 "Choose target:"
	jumphostFlow := map[string]config.LoginAction{
		"entry": {
			Send:      "",
			Expects:   []config.Expect{{Pattern: "Choose target:*", Next: "success"}},
			TimeoutMs: 500,
		},
	}
	serverFlow := map[string]config.LoginAction{
		"entry": {
			Send:    "1\n",
			Expects: []config.Expect{{Pattern: "login:*", Next: "success"}},
		},
	}

	jumphost := &config.Jumphost{
		Name:       "jump",
		Addr:       srv.Addr(),
		User:       "alice",
		Auth:       config.SSHAuth{Password: "wonderland"},
		SSHJ:       false,
		LoginFlow:  jumphostFlow,
		LoginEntry: "entry",
	}
	server := &config.SSHServer{
		Name:       "prod-db",
		LoginFlow:  serverFlow,
		LoginEntry: "entry",
	}
	server.Via = jumphost

	store.Save(&config.Config{
		Version:   "1",
		Jumphosts: []*config.Jumphost{jumphost},
		Servers:   []*config.SSHServer{server},
	})
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	res, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "prod-db"})
	if !res.IsError {
		t.Fatalf("expected IsError=true for jumphost flow failure")
	}
	r := parseJSON(t, resultText(t, res)).(map[string]any)
	errMsg, _ := r["error"].(string)
	if !strings.Contains(errMsg, "loginflow") && !strings.Contains(errMsg, "no expect matched") {
		t.Errorf("err = %q, want contains 'loginflow' or 'no expect matched'", errMsg)
	}
	trace, ok := r["login_trace"].([]any)
	if !ok || len(trace) == 0 {
		t.Errorf("response should include non-empty login_trace, got: %v", r["login_trace"])
	}
}

// TestIntegrationPatternBTargetFlowFailureReturnsTrace: Pattern B target LoginFlow 失败时
// 响应包含 login_trace。fake jumphost menu 正常通过，但 SSHServer.LoginFlow 等不匹配的
// pattern，应在 target 阶段超时并返回 trace。
func TestIntegrationPatternBTargetFlowFailureReturnsTrace(t *testing.T) {
	srv := newFakeJumphostServerForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))

	// jumphost flow 正常匹配 fake server 的菜单
	jumphostFlow := map[string]config.LoginAction{
		"entry": {
			Send:    "",
			Expects: []config.Expect{{Pattern: "Select target:", Next: "success"}},
		},
	}
	// target flow 故意配不匹配的 pattern + 短超时
	serverFlow := map[string]config.LoginAction{
		"entry": {
			Send:      "1\n",
			Expects:   []config.Expect{{Pattern: "NEVER_MATCHES_THIS", Next: "success"}},
			TimeoutMs: 500,
		},
	}

	jumphost := &config.Jumphost{
		Name:       "jump",
		Addr:       srv.Addr(),
		User:       "alice",
		Auth:       config.SSHAuth{Password: "wonderland"},
		SSHJ:       false,
		LoginFlow:  jumphostFlow,
		LoginEntry: "entry",
	}
	server := &config.SSHServer{
		Name:       "prod-db",
		LoginFlow:  serverFlow,
		LoginEntry: "entry",
	}
	server.Via = jumphost

	store.Save(&config.Config{
		Version:   "1",
		Jumphosts: []*config.Jumphost{jumphost},
		Servers:   []*config.SSHServer{server},
	})
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	res, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "prod-db"})
	if !res.IsError {
		t.Fatalf("expected IsError=true for target flow failure")
	}
	r := parseJSON(t, resultText(t, res)).(map[string]any)
	errMsg, _ := r["error"].(string)
	if !strings.Contains(errMsg, "target") {
		t.Errorf("err should mention 'target' stage, got: %s", errMsg)
	}
	trace, ok := r["login_trace"].([]any)
	if !ok || len(trace) == 0 {
		t.Fatalf("response should include non-empty login_trace, got: %v", r["login_trace"])
	}
	first, ok := trace[0].(map[string]any)
	if !ok {
		t.Fatalf("login_trace[0] should be object, got: %T", trace[0])
	}
	if first["send"] != "1\n" {
		t.Errorf("login_trace[0].send = %q, want '1\\n'", first["send"])
	}
}

// TestIntegrationPatternBLoginFlowTracePersisted: login 成功后 get_trace 返回 login_flow
// 字段，含 jumphost + target 两段 LoginFlow 的所有 trace entry（成功路径持久化）。
// 失败路径已由 TestIntegrationPatternBJumphostFlowFailure /
// TestIntegrationPatternBTargetFlowFailureReturnsTrace 覆盖（响应直接返 login_trace）。
func TestIntegrationPatternBLoginFlowTracePersisted(t *testing.T) {
	srv := newFakeJumphostServerForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))

	jumphostFlow := map[string]config.LoginAction{
		"entry": {
			Send:    "",
			Expects: []config.Expect{{Pattern: "Select target:*", Next: "success"}},
		},
	}
	serverFlow := map[string]config.LoginAction{
		"entry": {
			Send:    "1\n",
			Expects: []config.Expect{{Pattern: "login:*", Next: "send_user"}},
		},
		"send_user": {
			Send:    "alice\n",
			Expects: []config.Expect{{Pattern: "Password:*", Next: "send_pwd"}},
		},
		"send_pwd": {
			Send:    "wonderland\n",
			Expects: []config.Expect{{Pattern: "Welcome to*", Next: "success"}},
		},
	}

	jumphost := &config.Jumphost{
		Name:       "jump",
		Addr:       srv.Addr(),
		User:       "alice",
		Auth:       config.SSHAuth{Password: "wonderland"},
		SSHJ:       false,
		LoginFlow:  jumphostFlow,
		LoginEntry: "entry",
	}
	server := &config.SSHServer{
		Name:       "prod-db",
		LoginFlow:  serverFlow,
		LoginEntry: "entry",
	}
	server.Via = jumphost

	store.Save(&config.Config{
		Version:   "1",
		Jumphosts: []*config.Jumphost{jumphost},
		Servers:   []*config.SSHServer{server},
	})
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	loginRes, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "prod-db"})
	if loginRes.IsError {
		t.Fatalf("Login failed: %s", resultText(t, loginRes))
	}
	sid := parseJSON(t, resultText(t, loginRes)).(map[string]any)["sid"].(string)
	defer svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	traceRes, _, err := svc.GetTrace(context.Background(), &mcp.CallToolRequest{}, GetTraceArgs{SID: sid})
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if traceRes.IsError {
		t.Fatalf("GetTrace failed: %s", resultText(t, traceRes))
	}
	r := parseJSON(t, resultText(t, traceRes)).(map[string]any)

	// commands 应为空（login 后还没跑命令）
	if cmds, _ := r["commands"].([]any); len(cmds) != 0 {
		t.Errorf("commands should be empty before run_in_session, got %d entries", len(cmds))
	}

	// login_flow 应含 4 条 entry：1 jumphost + 3 target
	flow, ok := r["login_flow"].([]any)
	if !ok {
		t.Fatalf("login_flow should be array, got %T", r["login_flow"])
	}
	if len(flow) != 4 {
		t.Fatalf("login_flow should have 4 entries (1 jumphost + 3 target), got %d", len(flow))
	}

	// 校验每条 entry 的 send 值，按顺序应为："" / "1\n" / "alice\n" / "wonderland\n"
	expectedSends := []string{"", "1\n", "alice\n", "wonderland\n"}
	for i, want := range expectedSends {
		entry, ok := flow[i].(map[string]any)
		if !ok {
			t.Fatalf("login_flow[%d] should be object, got %T", i, flow[i])
		}
		if got, _ := entry["send"].(string); got != want {
			t.Errorf("login_flow[%d].send = %q, want %q", i, got, want)
		}
	}
}
