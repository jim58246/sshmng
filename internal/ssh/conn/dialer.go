package conn

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"
	"github.com/jim58246/sshmng/internal/config"
)

// Dialer 封装 SSH 拨号逻辑：私钥加载 + auth method 装配 + TOFU host key 校验 + 代理。
type Dialer struct {
	knownHosts *KnownHostsStore
	logger     *slog.Logger
}

// NewDialer 创建一个绑定到 knownHosts store 的 Dialer。
// logger 用于 DEBUG 级别的拨号过程日志（dialing / host key verified / auth method / proxy）；
// nil 退化为 discard handler。
func NewDialer(knownHosts *KnownHostsStore, logger *slog.Logger) *Dialer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Dialer{knownHosts: knownHosts, logger: logger}
}

// DialOptions 是 Dial 的入参。
type DialOptions struct {
	Addr          string         // host:port
	User          string         // SSH 用户名
	Auth          config.SSHAuth // 认证信息（Password / PrivateKey + Passphrase）
	Proxy         *config.Proxy  // 可选：传输层代理（SOCKS5 / HTTP CONNECT）
	ServerName    string         // 可选：仅用于日志关联（dialing / host key verified 等）
	HostKeyVerify bool           // false 时完全跳过 host key 校验（不读不写 known_hosts）
}

// Dial 建立 SSH 连接。
//   - 加载私钥（校验文件权限 0600 或更严）
//   - 装配 auth methods（同时配置 Password + PrivateKey 时仅用 PrivateKey）
//   - TOFU host key 校验（首次记录，变更拒绝）
//   - 走代理（如有）
//
// 失败返回 error，错误信息自解释（含 "permission denied" / "host key changed" / "connection refused" 等）。
func (d *Dialer) Dial(opts DialOptions) (*ssh.Client, error) {
	authMethod := "password"
	if opts.Auth.PrivateKey != "" {
		authMethod = "private_key"
	}
	d.logger.Debug("dialing",
		"server", opts.ServerName,
		"addr", opts.Addr,
		"user", opts.User,
		"auth_method", authMethod,
		"via_proxy", opts.Proxy != nil,
	)

	authMethods, err := buildAuthMethods(opts.Auth)
	if err != nil {
		return nil, err
	}

	clientConfig := &ssh.ClientConfig{
		User:            opts.User,
		Auth:            authMethods,
		HostKeyCallback: d.hostKeyCallback(opts.Addr, opts.ServerName, opts.HostKeyVerify),
		Timeout:         10 * time.Second,
	}

	conn, err := d.dialUnderlying(opts.Addr, opts.Proxy)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", opts.Addr, err)
	}
	// ssh.NewClientConn 成功后接管 conn；失败时关闭兜底。
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, opts.Addr, clientConfig)
	if err != nil {
		conn.Close()
		return nil, translateDialError(err, opts.Addr)
	}
	d.logger.Debug("ssh connected", "server", opts.ServerName, "addr", opts.Addr)
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// DialThrough 经由 jumphost 的 SSH client 拨号到 target（ssh -J 语义）。
// 用 jumpClient.Dial("tcp", opts.Addr) 开 direct-tcpip 通道，
// 再 ssh.NewClientConn 在其上建立第二层 SSH 连接。
//
// jumpClient 必须由调用方保持存活——target 的底层 conn 是 jumpClient 上的 channel，
// jumpClient 关闭会导致 target 不可用。调用方负责在 target 关闭后再关 jumpClient
// （PtyConn.Close 通过 SetJumpClient 绑定的引用处理此顺序）。
//
// 失败时关闭已开的 direct-tcpip conn，调用方只需关闭 jumpClient。
func (d *Dialer) DialThrough(jumpClient *ssh.Client, opts DialOptions) (*ssh.Client, error) {
	authMethod := "password"
	if opts.Auth.PrivateKey != "" {
		authMethod = "private_key"
	}
	d.logger.Debug("dialing through",
		"server", opts.ServerName,
		"addr", opts.Addr,
		"user", opts.User,
		"auth_method", authMethod,
		"via_jumphost", true,
	)

	authMethods, err := buildAuthMethods(opts.Auth)
	if err != nil {
		return nil, err
	}

	clientConfig := &ssh.ClientConfig{
		User:            opts.User,
		Auth:            authMethods,
		HostKeyCallback: d.hostKeyCallback(opts.Addr, opts.ServerName, opts.HostKeyVerify),
		Timeout:         10 * time.Second,
	}

	// jumpClient.Dial 无 timeout 参数，goroutine + select 兜底 10s。
	// 超时后 goroutine 阻塞到 jumphost 响应或 jumphost 关闭——jumphost 关闭时
	// 所有阻塞 Dial 返回 error，goroutine 退出。可接受的泄漏（超时本身已说明 jumphost 异常）。
	type dialRes struct {
		c   net.Conn
		err error
	}
	ch := make(chan dialRes, 1)
	go func() {
		c, err := jumpClient.Dial("tcp", opts.Addr)
		ch <- dialRes{c, err}
	}()
	var conn net.Conn
	select {
	case r := <-ch:
		conn, err = r.c, r.err
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("dial %s through jumphost: open direct-tcpip timed out after 10s", opts.Addr)
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s through jumphost: %w", opts.Addr, err)
	}

	// ssh.NewClientConn 成功后接管 conn；失败时关闭兜底。
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, opts.Addr, clientConfig)
	if err != nil {
		conn.Close()
		return nil, translateDialError(err, opts.Addr)
	}
	d.logger.Debug("ssh connected through", "server", opts.ServerName, "addr", opts.Addr)
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// dialUnderlying 建立底层 TCP 连接。无代理时直连；有代理时走 SOCKS5 或 HTTP CONNECT。
func (d *Dialer) dialUnderlying(addr string, p *config.Proxy) (net.Conn, error) {
	if p == nil {
		return net.DialTimeout("tcp", addr, 10*time.Second)
	}
	switch p.Type {
	case config.ProxySOCKS5:
		var auth *proxy.Auth
		if p.Auth != nil && p.Auth.User != "" {
			auth = &proxy.Auth{User: p.Auth.User, Password: p.Auth.Password}
		}
		dialer, err := proxy.SOCKS5("tcp", p.Addr, auth, &net.Dialer{Timeout: 10 * time.Second})
		if err != nil {
			return nil, fmt.Errorf("socks5 dialer: %w", err)
		}
		return dialer.Dial("tcp", addr)
	case config.ProxyHTTP:
		return httpConnect(p.Addr, p.Auth, addr, 10*time.Second)
	default:
		return nil, fmt.Errorf("unknown proxy type %q", p.Type)
	}
}

// hostKeyCallback 返回 ssh.ClientConfig.HostKeyCallback。
// verify=true 时通过 knownHosts.Check 实现 TOFU：首次记录、匹配放行、变更拒绝。
// verify=false 时返回 no-op callback，完全不触碰 known_hosts。
// serverName 仅用于日志关联。
func (d *Dialer) hostKeyCallback(addr, serverName string, verify bool) ssh.HostKeyCallback {
	if !verify {
		d.logger.Debug("host key verification disabled",
			"server", serverName, "addr", addr)
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return nil
		}
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fingerprint, err := d.knownHosts.Check(addr, key)
		if err != nil {
			d.logger.Warn("host key check failed",
				"server", serverName, "addr", addr, "err", err.Error())
			return err
		}
		d.logger.Debug("host key verified",
			"server", serverName, "addr", addr, "fingerprint", fingerprint)
		return nil
	}
}

// buildAuthMethods 装配 ssh.AuthMethod 列表。
//   - PrivateKey 非空：仅用私钥（即使 Password 也配置，也不回退）
//   - 否则 Password 非空：用密码
//   - 都空：返回 error
//
// 私钥文件权限过宽（group/other 有任何权限）拒绝加载。
func buildAuthMethods(auth config.SSHAuth) ([]ssh.AuthMethod, error) {
	if auth.PrivateKey != "" {
		signer, err := loadPrivateKey(auth.PrivateKey, auth.Passphrase)
		if err != nil {
			return nil, err
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}
	if auth.Password != "" {
		return []ssh.AuthMethod{ssh.Password(auth.Password)}, nil
	}
	return nil, fmt.Errorf("no auth method available (need password or private_key)")
}

// loadPrivateKey 从 path 加载私钥。passphrase 为空表示私钥未加密。
// 文件权限必须 0600 或更严（防止其他用户读取）。
//
// Windows 跳过权限检查：NTFS 用 ACL 而非 Unix rwx，os.FileMode.Perm() 的
// group/other 位恒为 0，检查形同虚设。由 NTFS ACL 负责文件访问控制。
func loadPrivateKey(path, passphrase string) (ssh.Signer, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat private key %s: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0077 != 0 {
			return nil, fmt.Errorf("private key %s permissions too open: %o, want 0600 or stricter (no group/other access)", path, perm)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key %s: %w", path, err)
	}
	var signer ssh.Signer
	if passphrase != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
	} else {
		signer, err = ssh.ParsePrivateKey(data)
	}
	if err != nil {
		return nil, fmt.Errorf("parse private key %s: %w", path, err)
	}
	return signer, nil
}

// translateDialError 把 ssh.NewClientConn 的错误翻译成更友好的 message。
// host key changed 已由 callback 抛出，会原样透传。
func translateDialError(err error, addr string) error {
	msg := err.Error()
	if strings.Contains(msg, "host key changed") {
		return err
	}
	return fmt.Errorf("ssh connect to %s: %w", addr, err)
}

// httpConnect 实现 HTTP CONNECT 代理隧道。
// 协议简单：发 `CONNECT host:port HTTP/1.1` + 代理认证（如有），等 200 响应。
func httpConnect(proxyAddr string, auth *config.ProxyAuth, target string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial proxy %s: %w", proxyAddr, err)
	}
	deadline := time.Now().Add(timeout)
	if dl, ok := conn.(interface{ SetDeadline(t time.Time) error }); ok {
		_ = dl.SetDeadline(deadline)
	}
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if auth != nil && auth.User != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(auth.User + ":" + auth.Password))
		req += "Proxy-Authorization: Basic " + cred + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	resp := string(buf[:n])
	if !strings.HasPrefix(resp, "HTTP/1.1 200") && !strings.HasPrefix(resp, "HTTP/1.0 200") {
		conn.Close()
		return nil, fmt.Errorf("proxy CONNECT failed: %s", strings.SplitN(resp, "\r\n", 2)[0])
	}
	if dl, ok := conn.(interface{ SetDeadline(t time.Time) error }); ok {
		_ = dl.SetDeadline(time.Time{})
	}
	return conn, nil
}
