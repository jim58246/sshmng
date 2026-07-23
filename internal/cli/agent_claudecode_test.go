package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newClaudeCodeInjectorForTest(t *testing.T) (*ClaudeCodeInjector, string) {
	t.Helper()
	tmp := t.TempDir()
	// Detect() looks for ~/.claude.json (with leading dot, matching the real
	// Claude Code config filename). The returned path is the config file path
	// that tests write to and pass to Inject/Verify.
	path := filepath.Join(tmp, ".claude.json")
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	return &ClaudeCodeInjector{}, path
}

func TestClaudeCodeDetectNotInstalled(t *testing.T) {
	inj, _ := newClaudeCodeInjectorForTest(t)
	_, installed := inj.Detect()
	if installed {
		t.Error("expected installed=false when no config file")
	}
}

func TestClaudeCodeDetectInstalled(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
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

func TestClaudeCodeInjectCreatesEntry(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
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
	want := `"command": "/usr/local/bin/sshmng"`
	if !strings.Contains(string(data), want) {
		t.Errorf("missing %q in:\n%s", want, string(data))
	}
	// json.MarshalIndent expands arrays across multiple lines, so assert the
	// args key and the mcp value separately rather than a single-line form.
	if !strings.Contains(string(data), `"args":`) {
		t.Errorf("missing args key in:\n%s", string(data))
	}
	if !strings.Contains(string(data), `"mcp"`) {
		t.Errorf("missing mcp arg in:\n%s", string(data))
	}
	wantEnv := `"SSHMNG_HOME": "/home/user/.sshmng"`
	if !strings.Contains(string(data), wantEnv) {
		t.Errorf("missing %q in:\n%s", wantEnv, string(data))
	}

	// sshmng entry nested under mcpServers
	if !strings.Contains(string(data), `"mcpServers"`) {
		t.Errorf("missing mcpServers key in:\n%s", string(data))
	}
}

func TestClaudeCodeInjectPreservesOtherServers(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
	original := `{"mcpServers":{"other":{"command":"x","args":["y"]}},"theme":"dark"}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, `"other"`) {
		t.Errorf("other server entry lost:\n%s", s)
	}
	if !strings.Contains(s, `"theme": "dark"`) {
		t.Errorf("theme field lost:\n%s", s)
	}
	if !strings.Contains(s, `"sshmng"`) {
		t.Errorf("sshmng entry missing:\n%s", s)
	}
}

func TestClaudeCodeInjectCreatesBackup(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
	original := `{"mcpServers":{"other":{"command":"x"}}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
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
	prefix := ".claude.json.bak."
	for _, e := range entries {
		if name := e.Name(); strings.HasPrefix(name, prefix) {
			backupCount++
		}
	}
	if backupCount != 1 {
		t.Errorf("expected 1 backup, got %d", backupCount)
	}
}

func TestClaudeCodeVerifyMatches(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
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

func TestClaudeCodeVerifyStaleBinary(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
	entry := MCPEntry{BinaryPath: "/old/bin/sshmng", Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": "/home"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	expected := MCPEntry{BinaryPath: "/new/bin/sshmng", Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": "/home"}}
	err := inj.Verify(path, expected)
	if err == nil {
		t.Error("Verify should fail for stale binary path")
	}
}

func TestClaudeCodeVerifyStaleArgs(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": "/home"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	expected := MCPEntry{BinaryPath: "/sshmng", Args: []string{"old"}, Env: map[string]string{"SSHMNG_HOME": "/home"}}
	err := inj.Verify(path, expected)
	if err == nil {
		t.Error("Verify should fail for stale args")
	}
}

func TestClaudeCodeVerifyStaleEnv(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": "/home"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	expected := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": "/other"}}
	err := inj.Verify(path, expected)
	if err == nil {
		t.Error("Verify should fail for stale env.SSHMNG_HOME")
	}
}

func TestClaudeCodeVerifyMissingEntry(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	err := inj.Verify(path, MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": "/home"}})
	if err == nil {
		t.Error("Verify should fail when sshmng entry missing")
	}
}
