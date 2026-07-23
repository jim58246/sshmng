package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestLoadFileNotExistReturnsDefault(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "config.json"))
	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Version != "1" {
		t.Errorf("Version = %q, want \"1\"", cfg.Version)
	}
	if cfg.IdleTimeoutS != 300 {
		t.Errorf("IdleTimeoutS = %d, want 300 (default)", cfg.IdleTimeoutS)
	}
	if len(cfg.Jumphosts) != 0 || len(cfg.Proxies) != 0 || len(cfg.Servers) != 0 {
		t.Errorf("expected empty slices, got jh=%d p=%d s=%d", len(cfg.Jumphosts), len(cfg.Proxies), len(cfg.Servers))
	}
}

func TestLoadRejectsWidePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission check skipped on Windows (NTFS ACL model)")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"version":"1"}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := NewStore(path)
	_, err := s.Load()
	if err == nil {
		t.Fatalf("expected error for 0644 permissions")
	}
}

func TestLoadAccepts0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"version":"1","idle_timeout_s":120}`), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := NewStore(path)
	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IdleTimeoutS != 120 {
		t.Errorf("IdleTimeoutS = %d, want 120", cfg.IdleTimeoutS)
	}
}

func TestSaveCreatesFileWith0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0600 no-op on Windows (NTFS ACL model)")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	s := NewStore(path)
	if err := s.Save(&Config{Version: "1", IdleTimeoutS: 300}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file perm = %o, want 0600", perm)
	}
}

func TestSaveAtomicNoTempResidue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	s := NewStore(path)
	if err := s.Save(&Config{Version: "1"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected 1 file in dir, got %d: %v", len(entries), names)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	s := NewStore(path)
	original := &Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Proxies:      []*Proxy{{Name: "p", Type: ProxySOCKS5, Addr: "p:1080"}},
		Jumphosts:    []*Jumphost{{Name: "jh", Addr: "h:22", User: "u", Auth: SSHAuth{}, SSHJ: true}},
		Servers:      []*SSHServer{{Name: "s", Addr: "t:22", User: "u", Auth: SSHAuth{}}},
	}
	if err := s.Save(original); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(loaded, original) {
		t.Errorf("round-trip mismatch:\norig: %+v\nloaded: %+v", original, loaded)
	}
}

func TestSaveOverwritePreservesPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0600 no-op on Windows (NTFS ACL model)")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// 先创建 0600 文件
	if err := os.WriteFile(path, []byte(`{"version":"1"}`), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := NewStore(path)
	if err := s.Save(&Config{Version: "1", IdleTimeoutS: 200}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file perm = %o, want 0600 preserved", perm)
	}
}

// TestLoadRejectsUnknownLogLevel 验证 log_level 配错时 Load 报错。
// 用户配 "trace" / "verbose" 等不支持的级别，必须报错不能静默 fallback。
func TestLoadRejectsUnknownLogLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"version":"1","log_level":"trace"}`), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := NewStore(path)
	_, err := s.Load()
	if err == nil {
		t.Fatalf("expected error for unknown log_level \"trace\"")
	}
}

// TestLoadAcceptsValidLogLevel 验证合法 log_level 能 Load，且字段值保留。
func TestLoadAcceptsValidLogLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"version":"1","log_level":"debug"}`), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := NewStore(path)
	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want \"debug\"", cfg.LogLevel)
	}
}

// TestLoadAcceptsLogLevelAbbreviation 验证缩写能 Load。
func TestLoadAcceptsLogLevelAbbreviation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"version":"1","log_level":"dbg"}`), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := NewStore(path)
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load with abbreviation \"dbg\": %v", err)
	}
}

// TestLoadAcceptsEmptyLogLevel 验证 log_level 省略时 Load 成功（走默认 info）。
func TestLoadAcceptsEmptyLogLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"version":"1"}`), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := NewStore(path)
	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != "" {
		t.Errorf("LogLevel = %q, want \"\" (empty, will default to info)", cfg.LogLevel)
	}
}

// TestDefaultConfigAutoUpdateEnabled 验证默认配置开启自动更新，且 UpdateURL 空（表示走 GitHub 源）。
func TestDefaultConfigAutoUpdateEnabled(t *testing.T) {
	cfg := defaultConfig()
	if !cfg.AutoUpdateEnabled {
		t.Errorf("defaultConfig().AutoUpdateEnabled = false, want true")
	}
	if cfg.UpdateURL != "" {
		t.Errorf("defaultConfig().UpdateURL = %q, want empty", cfg.UpdateURL)
	}
}

// TestConfigJSONRoundTripPreservesAutoUpdate 验证 marshal/unmarshal 往返保留 AutoUpdateEnabled 和 UpdateURL。
func TestConfigJSONRoundTripPreservesAutoUpdate(t *testing.T) {
	cfg := &Config{
		Version:           "1",
		IdleTimeoutS:      300,
		AutoUpdateEnabled: false,
		UpdateURL:         "https://updates.example.com/sshmng",
	}
	data, err := marshalConfig(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := unmarshalConfig(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.AutoUpdateEnabled != false {
		t.Errorf("AutoUpdateEnabled = %v, want false", parsed.AutoUpdateEnabled)
	}
	if parsed.UpdateURL != "https://updates.example.com/sshmng" {
		t.Errorf("UpdateURL = %q, want the configured URL", parsed.UpdateURL)
	}
}
