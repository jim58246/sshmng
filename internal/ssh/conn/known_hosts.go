package conn

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// HostKeyStatus 表示 Check 的返回状态。
type HostKeyStatus int

const (
	HostKeyNew     HostKeyStatus = iota // 首次见到该 host，已记录新 key
	HostKeyMatch                        // host 已知，key 匹配
	HostKeyChanged                      // host 已知，key 变更（拒绝连接）
)

// KnownHostsStore 管理 known_hosts 文件：TOFU（首次记录）+ 后续校验。
// 文件格式：每行 `host:port base64-pubkey`，权限 0600，原子写。
type KnownHostsStore struct {
	path string
	mu   sync.Mutex
}

// NewKnownHostsStore 创建一个指向 path 的 store。文件不必存在。
func NewKnownHostsStore(path string) *KnownHostsStore {
	return &KnownHostsStore{path: path}
}

// Path 返回 known_hosts 文件路径。
func (s *KnownHostsStore) Path() string { return s.path }

// Check 校验 host 的公钥。
//   - host 首次出现：记录 key 到文件，返回 HostKeyNew
//   - host 已知，key 匹配：返回 HostKeyMatch
//   - host 已知，key 变更：返回 HostKeyChanged + error（含 MITM 警告）
//
// 文件权限过宽（group/other 有任何权限）时拒绝加载。
func (s *KnownHostsStore) Check(addr string, key ssh.PublicKey) (HostKeyStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := s.load()
	if err != nil {
		return 0, err
	}
	marshaled := base64.StdEncoding.EncodeToString(key.Marshal())
	for _, e := range entries {
		if e.addr == addr {
			if e.keyB64 == marshaled {
				return HostKeyMatch, nil
			}
			return HostKeyChanged, fmt.Errorf("host key changed for %s: possible MITM attack; if this is intentional, remove the entry manually from %s", addr, s.path)
		}
	}
	// 首次见到，记录
	entries = append(entries, knownHostEntry{addr: addr, keyB64: marshaled})
	if err := s.save(entries); err != nil {
		return 0, fmt.Errorf("record host key: %w", err)
	}
	return HostKeyNew, nil
}

// knownHostEntry 是文件中一行的解析结果。
type knownHostEntry struct {
	addr   string
	keyB64 string
}

// load 读取 known_hosts 文件。文件不存在视为空。
// 权限过宽拒绝加载。
//
// Windows 跳过权限检查：NTFS 用 ACL 而非 Unix rwx，os.FileMode.Perm() 的
// group/other 位恒为 0，检查形同虚设。由 NTFS ACL 负责文件访问控制。
func (s *KnownHostsStore) load() ([]knownHostEntry, error) {
	info, err := os.Stat(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat known_hosts: %w", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0077 != 0 {
			return nil, fmt.Errorf("known_hosts permissions too open: %o, want 0600 (no group/other access)", perm)
		}
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read known_hosts: %w", err)
	}
	entries := []knownHostEntry{}
	for lineNo, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return nil, fmt.Errorf("known_hosts:%d: expected 'addr base64-key', got %q", lineNo+1, line)
		}
		entries = append(entries, knownHostEntry{addr: parts[0], keyB64: parts[1]})
	}
	return entries, nil
}

// save 原子写入 known_hosts 文件。权限强制 0600。
func (s *KnownHostsStore) save(entries []knownHostEntry) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir known_hosts dir: %w", err)
	}
	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(e.addr)
		sb.WriteByte(' ')
		sb.WriteString(e.keyB64)
		sb.WriteByte('\n')
	}
	tmp, err := os.CreateTemp(dir, ".known_hosts.tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 兜底；rename 成功后 remove 是 no-op
	if _, err := tmp.WriteString(sb.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("rename temp to known_hosts: %w", err)
	}
	return nil
}

// RandomSID 生成 8 字节十六进制 session ID。
// 供 session.go 和 pty.go 使用；放在这里是为了避免循环依赖。
func RandomSID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate sid: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
