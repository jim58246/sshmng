package pty

import (
	"crypto/rand"
	"crypto/rsa"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"sshmng/internal/config"
	"sshmng/internal/ssh/conn"
)

// fakeShellServerHangOnShell 是 fake SSH server，完成握手 + auth + pty-req，
// 但 shell 请求永远不 Reply——模拟 server 卡死。
// 用于测试 OpenPtyConnWithTimeout：session.Shell() 会阻塞等 Reply，timeout 后
// 应被强制中断。
type fakeShellServerHangOnShell struct {
	t        *testing.T
	listener net.Listener
	hostKey  ssh.Signer
	wg       sync.WaitGroup
}

func newFakeShellServerHangOnShell(t *testing.T) *fakeShellServerHangOnShell {
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
	s := &fakeShellServerHangOnShell{t: t, listener: l, hostKey: signer}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		l.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeShellServerHangOnShell) serve() {
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

func (s *fakeShellServerHangOnShell) handle(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			return nil, nil // 接受任何凭据
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

func (s *fakeShellServerHangOnShell) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer s.wg.Done()
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			req.Reply(true, nil)
		case "shell":
			// 不 Reply，模拟 server 卡死。测试结束后 conn 关闭，reqs channel 关闭，goroutine 退出。
		default:
			req.Reply(false, nil)
		}
	}
}

func (s *fakeShellServerHangOnShell) Addr() string { return s.listener.Addr().String() }

// TestOpenPtyConnWithTimeoutHangsOnShell 验证 server 在 shell 请求不响应时，
// OpenPtyConnWithTimeout 在 timeout 内返回 error，不会无限阻塞。
//
// 修复前：OpenPtyConn 调 session.Shell() 阻塞等 Reply，server 不响应时永远卡住
// （SSH 协议无 per-operation 超时）。login 卡死，Agent 无法继续。
// 修复后：OpenPtyConnWithTimeout 用 goroutine + select，timeout 后 Close client
// 中断阻塞的 Shell()，返回 timeout error。
func TestOpenPtyConnWithTimeoutHangsOnShell(t *testing.T) {
	srv := newFakeShellServerHangOnShell(t)
	d := newDialerWithTempKnownHosts(t)

	client, err := d.Dial(conn.DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "any"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	timeout := 500 * time.Millisecond
	start := time.Now()
	_, err = OpenPtyConnWithTimeout(client, "deadbeef", slog.New(slog.DiscardHandler), timeout)
	elapsed := time.Since(start)

	if err == nil {
		t.Errorf("expected timeout error, got nil")
	}
	if elapsed > 2*timeout {
		t.Errorf("OpenPtyConnWithTimeout took %s, expected < %s", elapsed, 2*timeout)
	}
	if err != nil && !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err should contain 'timed out', got: %v", err)
	}
}
