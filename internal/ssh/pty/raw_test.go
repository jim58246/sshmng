package pty

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/jim58246/sshmng/internal/ssh/conn"
)

// rawEchoServer 是 SendRaw/ReadRaw 测试用的极简 SSH 服务端:
// 接受 pty-req + shell 后,把客户端发来的字节原样回显(模拟无 shell 设备的 echo 行为)。
type rawEchoServer struct {
	listener net.Listener
	hostKey  gossh.Signer
	wg       sync.WaitGroup
	delay    time.Duration // 每次回显前的延迟(模拟慢速输出)
	closeOn  string        // 收到该子串后关闭连接(模拟 connection lost);空=不关
	closed   bool
	mu       sync.Mutex
}

func newRawEchoServer(t *testing.T) *rawEchoServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &rawEchoServer{listener: l, hostKey: signer}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		l.Close()
		s.wg.Wait()
	})
	return s
}

func (s *rawEchoServer) Addr() string { return s.listener.Addr().String() }

func (s *rawEchoServer) serve() {
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

func (s *rawEchoServer) handle(c net.Conn) {
	defer s.wg.Done()
	defer c.Close()
	cfg := &gossh.ServerConfig{
		PasswordCallback: func(m gossh.ConnMetadata, pass []byte) (*gossh.Permissions, error) {
			if m.User() == "alice" && string(pass) == "wonderland" {
				return nil, nil
			}
			return nil, errors.New("permission denied")
		},
	}
	cfg.AddHostKey(s.hostKey)
	sshConn, chans, reqs, err := gossh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go gossh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(gossh.UnknownChannelType, "only session")
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

func (s *rawEchoServer) handleSession(ch gossh.Channel, reqs <-chan *gossh.Request) {
	defer s.wg.Done()
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			req.Reply(true, nil)
		case "shell":
			req.Reply(true, nil)
			s.echoLoop(ch) // 同步运行:返回后 defer ch.Close() 才关通道
			return
		default:
			req.Reply(false, nil)
		}
	}
}

func (s *rawEchoServer) echoLoop(ch gossh.Channel) {
	// 同步运行在 handleSession 的 goroutine 内,不单独占用 wg 计数
	buf := make([]byte, 256)
	for {
		n, err := ch.Read(buf)
		if n > 0 {
			data := string(buf[:n])
			s.mu.Lock()
			shouldClose := s.closeOn != "" && strings.Contains(data, s.closeOn) && !s.closed
			if shouldClose {
				s.closed = true
			}
			s.mu.Unlock()
			if shouldClose {
				return // 直接关闭,不回显
			}
			out := make([]byte, n)
			copy(out, buf[:n])
			if s.delay > 0 {
				time.Sleep(s.delay)
			}
			if _, err := ch.Write(out); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func rawTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// dialRawEcho 建立 PtyConn(不做 DetectShell/InjectRC——raw 用法)。
func dialRawEcho(t *testing.T, srv *rawEchoServer) *PtyConn {
	t.Helper()
	client, err := gossh.Dial("tcp", srv.Addr(), &gossh.ClientConfig{
		User:            "alice",
		Auth:            []gossh.AuthMethod{gossh.Password("wonderland")},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	p, err := OpenPtyConnWithTimeout(client, "testsid", rawTestLogger(), 0)
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func TestMarkRawAndRunGuard(t *testing.T) {
	p := dialRawEcho(t, newRawEchoServer(t))
	p.MarkRaw()
	if p.Shell() != "raw" {
		t.Fatalf("Shell() = %q, want raw", p.Shell())
	}
	_, _, _, _, _, _, _, _, err := p.Run("echo hi", 0, 0)
	if err == nil || !strings.Contains(err.Error(), "raw device") {
		t.Errorf("Run on raw conn should error with 'raw device', got: %v", err)
	}
}

func TestSendRawEcho(t *testing.T) {
	p := dialRawEcho(t, newRawEchoServer(t))
	if err := p.SendRaw([]byte("hello\r")); err != nil {
		t.Fatalf("SendRaw: %v", err)
	}
	chunk, more, err := p.ReadRaw(2*time.Second, 4096)
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	if !bytes.Contains(chunk, []byte("hello")) {
		t.Errorf("chunk = %q, want contains hello", chunk)
	}
	if more {
		t.Error("more should be false after echo drained")
	}
}

func TestReadRawQuietAbsorption(t *testing.T) {
	// 回显带 30ms 延迟:quiet gap=200ms 时一次 read 吸收全部字节
	srv := newRawEchoServer(t)
	srv.delay = 30 * time.Millisecond
	p := dialRawEcho(t, srv)
	p.rawQuietGap = 200 * time.Millisecond
	if err := p.SendRaw([]byte("abcdef\r")); err != nil {
		t.Fatalf("SendRaw: %v", err)
	}
	chunk, _, err := p.ReadRaw(2*time.Second, 4096)
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	if !bytes.Contains(chunk, []byte("abcdef")) {
		t.Errorf("quiet absorption failed: %q", chunk)
	}
}

func TestReadRawMaxBytesSplit(t *testing.T) {
	p := dialRawEcho(t, newRawEchoServer(t))
	p.rawQuietGap = 50 * time.Millisecond
	p.SendRaw([]byte("0123456789"))
	chunk, more, err := p.ReadRaw(2*time.Second, 4)
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	if string(chunk) != "0123" || !more {
		t.Errorf("chunk=%q more=%v, want 0123/true", chunk, more)
	}
	chunk2, more2, err := p.ReadRaw(2*time.Second, 4096)
	if err != nil {
		t.Fatalf("ReadRaw 2: %v", err)
	}
	if !bytes.Contains(chunk2, []byte("456789")) || more2 {
		t.Errorf("chunk2=%q more2=%v, want remainder/false", chunk2, more2)
	}
}

func TestReadRawWaitTimeoutNoData(t *testing.T) {
	p := dialRawEcho(t, newRawEchoServer(t))
	start := time.Now()
	chunk, more, err := p.ReadRaw(150*time.Millisecond, 4096)
	if err != nil {
		t.Fatalf("ReadRaw should not error on wait timeout: %v", err)
	}
	if len(chunk) != 0 || more {
		t.Errorf("want empty/false, got %q/%v", chunk, more)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Error("ReadRaw should have blocked ~wait duration")
	}
}

func TestReadRawConnLost(t *testing.T) {
	srv := newRawEchoServer(t)
	srv.closeOn = "die"
	p := dialRawEcho(t, srv)
	p.rawQuietGap = 30 * time.Millisecond
	p.SendRaw([]byte("die"))
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, _, err := p.ReadRaw(200*time.Millisecond, 4096)
		if errors.Is(err, conn.ErrConnLost) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ErrConnLost within 3s, last err: %v", err)
		}
	}
}
