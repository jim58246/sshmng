package config

import (
	"os"
	"path/filepath"
	"reflect"
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
