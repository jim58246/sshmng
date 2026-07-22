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
	"sshmng/internal/config"
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
