package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Store 管理 config.json 的加载与持久化。
type Store struct {
	path string
}

// NewStore 创建一个指向 path 的 Store。
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Path 返回配置文件路径。
func (s *Store) Path() string { return s.path }

// Load 从文件加载配置。文件不存在时返回默认空配置（不创建文件）。
// 文件存在但权限过宽（group/other 有任何权限）时拒绝加载。
//
// Windows 跳过权限检查：NTFS 用 ACL 而非 Unix rwx，os.FileMode.Perm() 的
// group/other 位在 Windows 上恒为 0，此检查形同虚设。由 NTFS ACL 负责
// 文件访问控制（Windows 标准做法）。
func (s *Store) Load() (*Config, error) {
	info, err := os.Stat(s.path)
	if os.IsNotExist(err) {
		return defaultConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat config: %w", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0077 != 0 {
			return nil, fmt.Errorf("config file permissions too open: %o, want 0600 or stricter (no group/other access)", perm)
		}
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := unmarshalConfig(data)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// Save 原子写入配置到文件。写临时文件 + rename，避免写一半崩溃损坏原文件。
// 文件权限强制 0600。
func (s *Store) Save(c *Config) error {
	data, err := marshalConfig(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config.json.tmp.*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename temp to config: %w", err)
	}
	return nil
}

// defaultConfig 返回文件不存在时的默认配置。
func defaultConfig() *Config {
	return &Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Jumphosts:    []*Jumphost{},
		Proxies:      []*Proxy{},
		Servers:      []*SSHServer{},
	}
}

// unmarshalConfig 反序列化 JSON 并解析 via/proxy 引用。
func unmarshalConfig(data []byte) (*Config, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Version == "" {
		return nil, fmt.Errorf("missing \"version\" field")
	}
	if cfg.IdleTimeoutS == 0 {
		cfg.IdleTimeoutS = 300
	}
	// log_level 必须是支持的级别（或空=默认 info）。配错报错，不静默 fallback。
	if _, err := ParseLogLevel(cfg.LogLevel); err != nil {
		return nil, err
	}
	if cfg.Jumphosts == nil {
		cfg.Jumphosts = []*Jumphost{}
	}
	if cfg.Proxies == nil {
		cfg.Proxies = []*Proxy{}
	}
	if cfg.Servers == nil {
		cfg.Servers = []*SSHServer{}
	}
	if err := cfg.resolveReferences(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// marshalConfig 序列化 Config 为 JSON（带缩进，便于人工审查）。
func marshalConfig(c *Config) ([]byte, error) {
	return json.MarshalIndent(c, "", "  ")
}
