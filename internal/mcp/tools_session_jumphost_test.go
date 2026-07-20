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

// fakeJumphostServerForMCP 是 Pattern B 测试用的 fake SSH server。
//
// 全程在同一 SSH session 内完成（Pattern B = 交互式堡垒机，不开新 channel）：
//  1. SSH auth (alice/wonderland)
//  2. Shell detect → 自动 emit 菜单
//  3. 接受菜单选择 "1" → emit "login: "
//  4. 接受 username → emit "Password: "
//  5. 接受 password (wonderland) → emit "Welcome to prod-db!" → 转入 target shell
//  6. RC 注入 + 命令执行（同 fakeShellServerForMCP）
type fakeJumphostServerForMCP struct {
	t        *testing.T
	listener net.Listener
	hostKey  cryptossh.Signer
	wg       sync.WaitGroup
}

func newFakeJumphostServerForMCP(t *testing.T) *fakeJumphostServerForMCP {
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
	s := &fakeJumphostServerForMCP{t: t, listener: l, hostKey: signer}
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
		default:
			req.Reply(false, nil)
		}
	}
}

// runFakeJumphostShellForMCP 模拟 Pattern B 堡垒机的状态机：
// menu → target_user → target_pass → target_shell。
//
// shell detect 响应后自动 emit 菜单；menu 阶段读 "1" 触发 target login；
// target_user 阶段读 username emit "Password: "；target_pass 阶段读 password
// 验证后 emit "Welcome to prod-db!" 转入 target_shell；target_shell 走 RC + 命令。
func runFakeJumphostShellForMCP(ch cryptossh.Channel) {
	reader := bufio.NewReader(ch)
	var sid string
	rcDone := false
	phase := "menu"

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")

		// Shell detect（任何 phase 都响应，但只在 menu phase 触发菜单 emit）
		if strings.Contains(line, "__SHELL_DETECT__") {
			rand := extractRandMCP(line)
			fmt.Fprintf(ch, "__SHELL_DETECT__:/bin/bash:5.2.15(1)-release:\r\n")
			if rand != "" {
				fmt.Fprintf(ch, "__DETECT_END_%s__\r\n", rand)
			}
			fmt.Fprintf(ch, "Welcome to Jumphost v2\r\n")
			fmt.Fprintf(ch, "Main menu:\r\n")
			fmt.Fprintf(ch, "1) prod-db\r\n")
			fmt.Fprintf(ch, "Select target: ")
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
			Name:    "entry",
			Send:    "",
			Expects: []config.Expect{{Pattern: "Select target:*", Next: "success"}},
		},
	}
	serverFlow := map[string]config.LoginAction{
		"entry": {
			Name:    "entry",
			Send:    "1\n",
			Expects: []config.Expect{{Pattern: "login:*", Next: "send_user"}},
		},
		"send_user": {
			Name:    "send_user",
			Send:    "alice\n",
			Expects: []config.Expect{{Pattern: "Password:*", Next: "send_pwd"}},
		},
		"send_pwd": {
			Name:    "send_pwd",
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
	svc := NewService(store, ssh.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

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

// TestIntegrationPatternBJumphostFlowFailure: Jumphost.LoginFlow 失败（pattern 不匹配）时
// login 报错，error 含 loginflow / no expect matched 供诊断。
func TestIntegrationPatternBJumphostFlowFailure(t *testing.T) {
	srv := newFakeJumphostServerForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))

	// 故意配不匹配的 pattern：fake server emit "Select target:" 但我们等 "Choose target:"
	jumphostFlow := map[string]config.LoginAction{
		"entry": {
			Name:    "entry",
			Send:    "",
			Expects: []config.Expect{{Pattern: "Choose target:*", Next: "success"}},
		},
	}
	serverFlow := map[string]config.LoginAction{
		"entry": {
			Name:    "entry",
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
	svc := NewService(store, ssh.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	res, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "prod-db"})
	if !res.IsError {
		t.Fatalf("expected IsError=true for jumphost flow failure")
	}
	text := resultText(t, res)
	if !strings.Contains(text, "loginflow") && !strings.Contains(text, "no expect matched") {
		t.Errorf("err = %q, want contains 'loginflow' or 'no expect matched'", text)
	}
}
