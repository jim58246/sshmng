package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newHermesInjectorForTest(t *testing.T) (*HermesInjector, string) {
	t.Helper()
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", tmp)
		path := filepath.Join(tmp, "hermes", "config.yaml")
		return &HermesInjector{}, path
	}
	t.Setenv("HOME", tmp)
	path := filepath.Join(tmp, ".hermes", "config.yaml")
	return &HermesInjector{}, path
}

func TestHermesDetectNotInstalled(t *testing.T) {
	inj, _ := newHermesInjectorForTest(t)
	_, installed := inj.Detect()
	if installed {
		t.Error("expected installed=false")
	}
}

func TestHermesDetectInstalled(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("model: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, installed := inj.Detect()
	if !installed {
		t.Error("expected installed=true")
	}
	if got != path {
		t.Errorf("got %q, want %q", got, path)
	}
}

func TestHermesInjectCreatesEntry(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	entry := MCPEntry{
		BinaryPath: "/usr/local/bin/sshmng",
		Args:       []string{"mcp"},
		Env:        map[string]string{"SSHMNG_HOME": "/home/user/.sshmng"},
	}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "mcp_servers:") {
		t.Errorf("missing mcp_servers key:\n%s", s)
	}
	if !strings.Contains(s, "sshmng:") {
		t.Errorf("missing sshmng entry:\n%s", s)
	}
	if !strings.Contains(s, "command: /usr/local/bin/sshmng") {
		t.Errorf("missing command:\n%s", s)
	}
	if !strings.Contains(s, "- mcp") {
		t.Errorf("missing args:\n%s", s)
	}
	if !strings.Contains(s, "SSHMNG_HOME: /home/user/.sshmng") {
		t.Errorf("missing env:\n%s", s)
	}
}

func TestHermesInjectPreservesOtherServers(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	original := "model:\n  name: hermes-3\nmcp_servers:\n  other:\n    command: x\n"
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, "name: hermes-3") {
		t.Errorf("model field lost:\n%s", s)
	}
	if !strings.Contains(s, "other:") {
		t.Errorf("other server lost:\n%s", s)
	}
	if !strings.Contains(s, "sshmng:") {
		t.Errorf("sshmng entry missing:\n%s", s)
	}
}

func TestHermesInjectCreatesBackup(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("model: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	backupCount := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "config.yaml.bak.") {
			backupCount++
		}
	}
	if backupCount != 1 {
		t.Errorf("expected 1 backup, got %d", backupCount)
	}
}

func TestHermesVerifyMatches(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	entry := MCPEntry{
		BinaryPath: "/sshmng",
		Args:       []string{"mcp"},
		Env:        map[string]string{"SSHMNG_HOME": "/home"},
	}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := inj.Verify(path, entry); err != nil {
		t.Errorf("Verify should pass: %v", err)
	}
}

func TestHermesVerifyStaleBinary(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	entry := MCPEntry{BinaryPath: "/old/bin/sshmng", Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": "/home"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	expected := MCPEntry{BinaryPath: "/new/bin/sshmng", Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": "/home"}}
	if err := inj.Verify(path, expected); err == nil {
		t.Error("Verify should fail for stale binary")
	}
}

func TestHermesVerifyStaleArgs(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": "/home"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	expected := MCPEntry{BinaryPath: "/sshmng", Args: []string{"old"}, Env: map[string]string{"SSHMNG_HOME": "/home"}}
	if err := inj.Verify(path, expected); err == nil {
		t.Error("Verify should fail for stale args")
	}
}

func TestHermesVerifyStaleEnv(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": "/home"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	expected := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": "/other"}}
	if err := inj.Verify(path, expected); err == nil {
		t.Error("Verify should fail for stale env.SSHMNG_HOME")
	}
}

func TestHermesVerifyMissingEntry(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("mcp_servers: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := inj.Verify(path, MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": "/home"}}); err == nil {
		t.Error("Verify should fail when sshmng entry missing")
	}
}
