package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sshmng/internal/config"
)

func TestScaffoldHomeCreatesDirAndFiles(t *testing.T) {
	home := t.TempDir()
	err := ScaffoldHome(home, ScaffoldOpts{})
	if err != nil {
		t.Fatalf("ScaffoldHome: %v", err)
	}
	// Directory exists with 0700 (Unix)
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Error("home is not a directory")
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0700 {
			t.Errorf("home perm = %o, want 0700", perm)
		}
	}
	// config.json exists
	cfgPath := filepath.Join(home, "config.json")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("config.json missing: %v", err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(cfgPath); err == nil {
			if perm := info.Mode().Perm(); perm != 0600 {
				t.Errorf("config.json perm = %o, want 0600", perm)
			}
		}
	}
	// config.example.json exists
	exPath := filepath.Join(home, "config.example.json")
	if _, err := os.Stat(exPath); err != nil {
		t.Errorf("config.example.json missing: %v", err)
	}
}

func TestScaffoldHomeConfigJSONLoadsViaStore(t *testing.T) {
	home := t.TempDir()
	if err := ScaffoldHome(home, ScaffoldOpts{}); err != nil {
		t.Fatalf("ScaffoldHome: %v", err)
	}
	store := config.NewStore(filepath.Join(home, "config.json"))
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("Load config.json: %v", err)
	}
	if cfg.Version != "1" {
		t.Errorf("version = %q, want '1'", cfg.Version)
	}
	if len(cfg.Servers) != 0 {
		t.Errorf("expected 0 servers, got %d", len(cfg.Servers))
	}
}

func TestScaffoldHomeConfigExampleJSONLoadsViaStore(t *testing.T) {
	home := t.TempDir()
	if err := ScaffoldHome(home, ScaffoldOpts{}); err != nil {
		t.Fatalf("ScaffoldHome: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Example file must be valid JSON loadable by config.Store
	tmpCfg := filepath.Join(home, "test_load.json")
	if err := os.WriteFile(tmpCfg, data, 0600); err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(tmpCfg)
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("example config invalid: %v", err)
	}
	if len(cfg.Servers) < 4 {
		t.Errorf("expected >=4 example servers, got %d", len(cfg.Servers))
	}
	if len(cfg.Proxies) < 2 {
		t.Errorf("expected >=2 example proxies, got %d", len(cfg.Proxies))
	}
	if len(cfg.Jumphosts) < 2 {
		t.Errorf("expected >=2 example jumphosts, got %d", len(cfg.Jumphosts))
	}
}

func TestScaffoldHomePreservesExistingConfigJSON(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.json")
	original := `{"version": "1", "idle_timeout_s": 600, "jumphosts": [], "proxies": [], "servers": []}`
	if err := os.WriteFile(cfgPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ScaffoldHome(home, ScaffoldOpts{}); err != nil {
		t.Fatalf("ScaffoldHome: %v", err)
	}
	data, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(data), `"idle_timeout_s": 600`) {
		t.Errorf("existing config.json was modified:\n%s", string(data))
	}
}

func TestScaffoldHomeOverwritesConfigExampleJSON(t *testing.T) {
	home := t.TempDir()
	exPath := filepath.Join(home, "config.example.json")
	original := `{"old": true}`
	if err := os.WriteFile(exPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ScaffoldHome(home, ScaffoldOpts{}); err != nil {
		t.Fatalf("ScaffoldHome: %v", err)
	}
	data, _ := os.ReadFile(exPath)
	if strings.Contains(string(data), `"old": true`) {
		t.Errorf("config.example.json was not overwritten:\n%s", string(data))
	}
	if !strings.Contains(string(data), "example-server-direct-key") {
		t.Errorf("config.example.json missing expected content:\n%s", string(data))
	}
}
