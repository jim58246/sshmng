package mcp

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"
	"sshmng/internal/config"
	"sshmng/internal/ssh/conn"
)

// testLogger 返回一个写向 io.Discard 的 slog.Logger，供 mcp 测试包内需要 logger 入参的场景复用。
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeJumphostForPatternA 是 Pattern A 集成测试用的 fake jumphost SSH server。
// 与 Pattern B 的 fakeJumphostServerForMCP 不同——它不跑菜单，而是处理
// direct-tcpip channel 请求：解析目标地址，net.Dial 到真实 target，双向 pipe。
type fakeJumphostForPatternA struct {
	t               *testing.T
	listener        net.Listener
	hostKey         cryptossh.Signer
	hostPub         cryptossh.PublicKey
	allowForwarding bool
	activeConns     atomic.Int32 // 跟踪活跃 SSH 连接数（Close 生命周期测试用）
	wg              sync.WaitGroup
}

func newFakeJumphostForPatternA(t *testing.T, allowForwarding bool) *fakeJumphostForPatternA {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := cryptossh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	pub, err := cryptossh.NewPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatalf("new pubkey: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeJumphostForPatternA{
		t:               t,
		listener:        l,
		hostKey:         signer,
		hostPub:         pub,
		allowForwarding: allowForwarding,
	}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		l.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeJumphostForPatternA) serve() {
	defer s.wg.Done()
	for {
		c, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.handle(c)
	}
}

func (s *fakeJumphostForPatternA) handle(c net.Conn) {
	defer s.wg.Done()
	defer c.Close()
	cfg := &cryptossh.ServerConfig{
		PasswordCallback: func(cm cryptossh.ConnMetadata, pass []byte) (*cryptossh.Permissions, error) {
			if cm.User() == "jump-user" && string(pass) == "jump-pass" {
				return nil, nil
			}
			return nil, fmt.Errorf("permission denied")
		},
	}
	cfg.AddHostKey(s.hostKey)
	sshConn, chans, reqs, err := cryptossh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	s.activeConns.Add(1)
	defer s.activeConns.Add(-1)
	defer sshConn.Close()
	go cryptossh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "direct-tcpip" {
			newCh.Reject(cryptossh.UnknownChannelType, "unsupported")
			continue
		}
		if !s.allowForwarding {
			newCh.Reject(cryptossh.Prohibited, "administratively prohibited")
			continue
		}
		var msg struct {
			Addr       string
			Port       uint32
			OriginAddr string
			OriginPort uint32
		}
		if err := cryptossh.Unmarshal(newCh.ExtraData(), &msg); err != nil {
			newCh.Reject(cryptossh.ConnectionFailed, "bad extra data")
			continue
		}
		target := net.JoinHostPort(msg.Addr, fmt.Sprint(msg.Port))
		upstream, err := net.Dial("tcp", target)
		if err != nil {
			newCh.Reject(cryptossh.ConnectionFailed, err.Error())
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			upstream.Close()
			continue
		}
		go cryptossh.DiscardRequests(chReqs)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer ch.Close()
			defer upstream.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(upstream, ch); done <- struct{}{} }()
			go func() { io.Copy(ch, upstream); done <- struct{}{} }()
			<-done
		}()
	}
}

func (s *fakeJumphostForPatternA) Addr() string                     { return s.listener.Addr().String() }
func (s *fakeJumphostForPatternA) HostPublicKey() cryptossh.PublicKey { return s.hostPub }
func (s *fakeJumphostForPatternA) ActiveConns() int32               { return s.activeConns.Load() }

// fakeTargetForPatternA 是 Pattern A 集成测试用的 fake target SSH server。
// 支持 shell + 命令执行（run_in_session 能跑通），跟踪活跃连接数（Close 生命周期测试用）。
type fakeTargetForPatternA struct {
	t           *testing.T
	listener    net.Listener
	hostKey     cryptossh.Signer
	hostPub     cryptossh.PublicKey
	activeConns atomic.Int32
	wg          sync.WaitGroup
}

func newFakeTargetForPatternA(t *testing.T) *fakeTargetForPatternA {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := cryptossh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	pub, err := cryptossh.NewPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatalf("new pubkey: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeTargetForPatternA{
		t:        t,
		listener: l,
		hostKey:  signer,
		hostPub:  pub,
	}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		l.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeTargetForPatternA) serve() {
	defer s.wg.Done()
	for {
		c, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.handle(c)
	}
}

func (s *fakeTargetForPatternA) handle(c net.Conn) {
	defer s.wg.Done()
	defer c.Close()
	cfg := &cryptossh.ServerConfig{
		PasswordCallback: func(cm cryptossh.ConnMetadata, pass []byte) (*cryptossh.Permissions, error) {
			if cm.User() == "alice" && string(pass) == "wonderland" {
				return nil, nil
			}
			return nil, fmt.Errorf("permission denied")
		},
	}
	cfg.AddHostKey(s.hostKey)
	sshConn, chans, reqs, err := cryptossh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	s.activeConns.Add(1)
	defer s.activeConns.Add(-1)
	defer sshConn.Close()
	go cryptossh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(cryptossh.UnknownChannelType, "unsupported")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		s.wg.Add(1)
		go s.handleSession(ch, chReqs)
	}
}

// handleSession 处理 target 上的 session channel：pty-req + shell（委托
// runFakeShellForMCP，响应 __SHELL_DETECT__ 探针 + RC 注入 + 命令执行）+
// subsystem（sftp，使 TryEnableSftp 成功、SftpAvailable()=true）。
// 与 fakeShellServerForMCP.handleSession 同构——Pattern A 的 target 行为与
// 直连 target 一致，只是连接是经 jumphost 的 direct-tcpip 通道建立的。
func (s *fakeTargetForPatternA) handleSession(ch cryptossh.Channel, reqs <-chan *cryptossh.Request) {
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

func (s *fakeTargetForPatternA) Addr() string                     { return s.listener.Addr().String() }
func (s *fakeTargetForPatternA) HostPublicKey() cryptossh.PublicKey { return s.hostPub }
func (s *fakeTargetForPatternA) ActiveConns() int32               { return s.activeConns.Load() }

// TestIntegrationPatternAEndToEnd 验证 Pattern A 完整路径：
// login → run_in_session → close，sftp_available=true（区别于 Pattern B）。
func TestIntegrationPatternAEndToEnd(t *testing.T) {
	target := newFakeTargetForPatternA(t)
	jump := newFakeJumphostForPatternA(t, true)

	// 构造 config：jumphost ssh_j=true，server.via 指向它
	jhCfg := &config.Jumphost{
		Name: "jh", Addr: jump.Addr(), User: "jump-user",
		Auth: config.SSHAuth{Password: "jump-pass"}, SSHJ: true,
	}
	srvCfg := &config.SSHServer{
		Name: "srv", Addr: target.Addr(), User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"}, Via: jhCfg,
	}

	// 用真实 conn.Dialer + 临时 known_hosts
	knownHosts := conn.NewKnownHostsStore(t.TempDir() + "/known_hosts")
	dialer := conn.NewDialer(knownHosts, nil)

	// 直接调 setupPatternA（绕过 MCP handler 层，专注连接层）
	// sid 用十六进制（与 conn.RandomSID 同格式），因 fake target 的 shell detect
	// regex __([0-9a-f]+)___\]# 仅匹配 hex sid；非 hex sid 会让 sentinel 缺 sid 导致
	// InjectRC 等不到 initial PS1 sentinel 超时。
	svc := &Service{} // 仅用 setupPatternA，不需要 store/manager
	logger := testLogger(t)
	ptyConn, _, err := svc.setupPatternA(srvCfg, dialer, "deadbeefcafebabe", logger)
	if err != nil {
		t.Fatalf("setupPatternA: %v", err)
	}
	defer ptyConn.Close()

	// 验证 sftp_available=true（Pattern A 启用 SFTP，区别于 Pattern B）
	if !ptyConn.SftpAvailable() {
		t.Errorf("Pattern A should enable SFTP (client is to target), got sftp_available=false")
	}

	// 验证活跃连接：jumphost 和 target 各 1 个
	if got := jump.ActiveConns(); got != 1 {
		t.Errorf("jumphost activeConns after login = %d, want 1", got)
	}
	if got := target.ActiveConns(); got != 1 {
		t.Errorf("target activeConns after login = %d, want 1", got)
	}
}
