package conn

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"sshmng/internal/config"
)

// mockSSHServer 是用于测试的本地 SSH server。支持密码 / 公钥认证。
// 不实现 PTY / 命令执行，只做 SSH 握手；dialer 测试只需握手成功/失败。
type mockSSHServer struct {
	t              *testing.T
	listener       net.Listener
	hostKey        ssh.Signer
	hostPub        ssh.PublicKey
	password       string        // 接受的密码；空表示不接受密码
	pubKey         ssh.PublicKey // 接受的公钥；nil 表示不接受公钥
	authorizedUser string
	wg             sync.WaitGroup
}

func newMockSSHServer(t *testing.T, user, password string, pubKey ssh.PublicKey) *mockSSHServer {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	pub, err := ssh.NewPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatalf("new pubkey: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &mockSSHServer{
		t:              t,
		listener:       l,
		hostKey:        signer,
		hostPub:        pub,
		password:       password,
		pubKey:         pubKey,
		authorizedUser: user,
	}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		l.Close()
		s.wg.Wait()
	})
	return s
}

func (s *mockSSHServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed
		}
		s.wg.Add(1)
		go s.handle(conn)
	}
}

func (s *mockSSHServer) handle(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if s.password == "" {
				return nil, fmt.Errorf("password auth not configured")
			}
			if c.User() == s.authorizedUser && string(pass) == s.password {
				return nil, nil
			}
			return nil, fmt.Errorf("permission denied")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if s.pubKey == nil {
				return nil, fmt.Errorf("public key auth not configured")
			}
			if c.User() != s.authorizedUser {
				return nil, fmt.Errorf("permission denied")
			}
			// 比较公钥 marshaled bytes
			if string(key.Marshal()) == string(s.pubKey.Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("permission denied")
		},
	}
	cfg.AddHostKey(s.hostKey)
	sshConn, _, _, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	// 不处理 channel 请求，直接等关闭
	sshConn.Wait()
}

// Addr 返回 server 监听地址。
func (s *mockSSHServer) Addr() string { return s.listener.Addr().String() }

// HostPublicKey 返回 server 的 host public key。
func (s *mockSSHServer) HostPublicKey() ssh.PublicKey { return s.hostPub }

// --- helpers ---

// writePrivateKeyFile 把 RSA 私钥写入临时文件，权限 perm。返回路径。
func writePrivateKeyFile(t *testing.T, perm os.FileMode) (string, ssh.Signer, ssh.PublicKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	// 用 PEM 编码私钥
	block, err := ssh.MarshalPrivateKey(k, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "id_rsa")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), perm); err != nil {
		t.Fatalf("write key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	pub, err := ssh.NewPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatalf("new pubkey: %v", err)
	}
	return path, signer, pub
}

// newDialerWithTempKnownHosts 创建一个用临时 known_hosts 文件的 Dialer。
func newDialerWithTempKnownHosts(t *testing.T) *Dialer {
	t.Helper()
	return NewDialer(NewKnownHostsStore(filepath.Join(t.TempDir(), "known_hosts")), nil)
}

// --- 密码认证 ---

func TestDialerPasswordAuthSuccess(t *testing.T) {
	srv := newMockSSHServer(t, "alice", "wonderland", nil)
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
}

func TestDialerPasswordAuthFailure(t *testing.T) {
	srv := newMockSSHServer(t, "alice", "wonderland", nil)
	d := newDialerWithTempKnownHosts(t)

	_, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wrong"},
	})
	if err == nil {
		t.Errorf("expected auth failure error")
	}
	if !strings.Contains(err.Error(), "permission denied") && !strings.Contains(err.Error(), "ssh: handshake") {
		t.Errorf("error should mention permission denied or handshake, got: %v", err)
	}
}

// --- 公钥认证 ---

func TestDialerPublicKeyAuthSuccess(t *testing.T) {
	keyPath, _, pub := writePrivateKeyFile(t, 0600)
	srv := newMockSSHServer(t, "bob", "", pub)
	d := newDialerWithTempKnownHosts(t)

	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "bob",
		Auth: config.SSHAuth{PrivateKey: keyPath},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
}

func TestDialerPublicKeyAuthWithPassphrase(t *testing.T) {
	// 生成带 passphrase 加密的私钥
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(k, "", []byte("hunter2"))
	if err != nil {
		t.Fatalf("marshal encrypted: %v", err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_rsa")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	pub, err := ssh.NewPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatalf("new pubkey: %v", err)
	}
	srv := newMockSSHServer(t, "carol", "", pub)
	d := newDialerWithTempKnownHosts(t)

	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "carol",
		Auth: config.SSHAuth{PrivateKey: keyPath, Passphrase: "hunter2"},
	})
	if err != nil {
		t.Fatalf("Dial with passphrase: %v", err)
	}
	defer client.Close()
}

func TestDialerPublicKeyAuthWrongPassphrase(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(k, "", []byte("hunter2"))
	if err != nil {
		t.Fatalf("marshal encrypted: %v", err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_rsa")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	srv := newMockSSHServer(t, "carol", "", nil)
	d := newDialerWithTempKnownHosts(t)

	_, err = d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "carol",
		Auth: config.SSHAuth{PrivateKey: keyPath, Passphrase: "wrong"},
	})
	if err == nil {
		t.Errorf("expected error for wrong passphrase")
	}
}

func TestDialerPrivateKeyWidePermissionsRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("private key permission check skipped on Windows (NTFS ACL model)")
	}
	// 私钥文件权限 0644 应被拒绝
	keyPath, _, pub := writePrivateKeyFile(t, 0644)
	srv := newMockSSHServer(t, "bob", "", pub)
	d := newDialerWithTempKnownHosts(t)

	_, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "bob",
		Auth: config.SSHAuth{PrivateKey: keyPath},
	})
	if err == nil {
		t.Errorf("expected error for wide private key permissions")
	}
	if !strings.Contains(err.Error(), "permissions") && !strings.Contains(err.Error(), "permission") {
		t.Errorf("error should mention permissions, got: %v", err)
	}
}

func TestDialerPrivateKeyNotExist(t *testing.T) {
	srv := newMockSSHServer(t, "bob", "", nil)
	d := newDialerWithTempKnownHosts(t)

	_, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "bob",
		Auth: config.SSHAuth{PrivateKey: "/nonexistent/key"},
	})
	if err == nil {
		t.Errorf("expected error for missing private key")
	}
}

// 同时配置 Password 和 PrivateKey 时仅尝试 PrivateKey
func TestDialerPrefersPrivateKeyWhenBothConfigured(t *testing.T) {
	keyPath, _, pub := writePrivateKeyFile(t, 0600)
	// mock server 只接受公钥，不接受密码
	srv := newMockSSHServer(t, "bob", "", pub)
	d := newDialerWithTempKnownHosts(t)

	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "bob",
		Auth: config.SSHAuth{Password: "wrong", PrivateKey: keyPath},
	})
	if err != nil {
		t.Fatalf("Dial should succeed with private key (not fall back to password): %v", err)
	}
	defer client.Close()
}

// --- TOFU ---

func TestDialerTOFURemembersHostKey(t *testing.T) {
	srv := newMockSSHServer(t, "alice", "wonderland", nil)
	d := newDialerWithTempKnownHosts(t)

	// 首次连接：host key 写入 known_hosts
	c1, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("first Dial: %v", err)
	}
	c1.Close()

	// 第二次连接：key 匹配
	c2, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("second Dial: %v", err)
	}
	c2.Close()

	// 验证 known_hosts 文件非空
	data, err := os.ReadFile(d.knownHosts.Path())
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	if !strings.Contains(string(data), srv.Addr()) {
		t.Errorf("known_hosts should contain addr %s: %s", srv.Addr(), string(data))
	}
}

func TestDialerTOFURejectsChangedHostKey(t *testing.T) {
	// 第一台 server，记录其 host key
	srv1 := newMockSSHServer(t, "alice", "wonderland", nil)
	d := newDialerWithTempKnownHosts(t)
	c1, err := d.Dial(DialOptions{
		Addr: srv1.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("first Dial: %v", err)
	}
	c1.Close()
	srv1.listener.Close() // 关掉 srv1

	// 第二台 server，复用 srv1 的端口但 host key 不同
	l, err := net.Listen("tcp", srv1.Addr())
	if err != nil {
		t.Fatalf("listen on same port: %v", err)
	}
	srv2 := newMockSSHServerWithListener(t, l, "alice", "wonderland", nil)

	_, err = d.Dial(DialOptions{
		Addr: srv2.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err == nil {
		t.Errorf("expected error for changed host key")
	}
	if !strings.Contains(err.Error(), "host key changed") {
		t.Errorf("error should mention 'host key changed', got: %v", err)
	}
	if !strings.Contains(err.Error(), "MITM") {
		t.Errorf("error should warn about MITM, got: %v", err)
	}
}

// --- 拨号失败 ---

func TestDialerConnectionRefused(t *testing.T) {
	d := newDialerWithTempKnownHosts(t)
	// 找一个没人监听的端口
	l, err := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()

	_, err = d.Dial(DialOptions{
		Addr: addr,
		User: "alice",
		Auth: config.SSHAuth{Password: "p"},
	})
	if err == nil {
		t.Errorf("expected error for connection refused")
	}
	// 不应该有 trace 信息（这是 SSH auth 失败）
}

// --- helpers ---

func newMockSSHServerWithListener(t *testing.T, l net.Listener, user, password string, pubKey ssh.PublicKey) *mockSSHServer {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	pub, err := ssh.NewPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatalf("new pubkey: %v", err)
	}
	s := &mockSSHServer{
		t:              t,
		listener:       l,
		hostKey:        signer,
		hostPub:        pub,
		password:       password,
		pubKey:         pubKey,
		authorizedUser: user,
	}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		l.Close()
		s.wg.Wait()
	})
	return s
}

// (unused imports guard removed — all imports are used above)
