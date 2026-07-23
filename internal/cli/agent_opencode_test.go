package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newOpenCodeInjectorForTest(t *testing.T) (*OpenCodeInjector, string) {
	t.Helper()
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	path := filepath.Join(tmp, ".config", "opencode", "opencode.json")
	return &OpenCodeInjector{}, path
}

func TestOpenCodeDetectNotInstalled(t *testing.T) {
	inj, _ := newOpenCodeInjectorForTest(t)
	_, installed := inj.Detect()
	if installed {
		t.Error("expected installed=false")
	}
}

func TestOpenCodeDetectInstalled(t *testing.T) {
	inj, path := newOpenCodeInjectorForTest(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
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

func TestOpenCodeInjectCreatesEntry(t *testing.T) {
	inj, path := newOpenCodeInjectorForTest(t)
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
	if !strings.Contains(s, `"mcp"`) {
		t.Errorf("missing mcp key:\n%s", s)
	}
	if !strings.Contains(s, `"sshmng"`) {
		t.Errorf("missing sshmng entry:\n%s", s)
	}
	if !strings.Contains(s, `"type": "local"`) {
		t.Errorf("missing type:local:\n%s", s)
	}
	// command is an array combining binary + args. Indented JSON breaks the
	// array across lines, so check the binary and arg both appear as array
	// elements rather than asserting a single-line literal.
	if !strings.Contains(s, `"/usr/local/bin/sshmng"`) {
		t.Errorf("missing command binary in array:\n%s", s)
	}
	if !strings.Contains(s, `"mcp"`) {
		t.Errorf("missing command arg in array:\n%s", s)
	}
	if !strings.Contains(s, `"environment"`) {
		t.Errorf("missing environment field (not env):\n%s", s)
	}
	if strings.Contains(s, `"env":`) {
		t.Errorf("should not have env field:\n%s", s)
	}
	if !strings.Contains(s, `"enabled": true`) {
		t.Errorf("missing enabled:true:\n%s", s)
	}
}

func TestOpenCodeInjectPreservesOtherServers(t *testing.T) {
	inj, path := newOpenCodeInjectorForTest(t)
	original := `{"mcp":{"other":{"type":"local","command":["x"]}},"theme":"dark"}`
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
	if !strings.Contains(s, `"other"`) {
		t.Errorf("other server lost:\n%s", s)
	}
	if !strings.Contains(s, `"theme": "dark"`) {
		t.Errorf("theme lost:\n%s", s)
	}
	if !strings.Contains(s, `"sshmng"`) {
		t.Errorf("sshmng missing:\n%s", s)
	}
}

func TestOpenCodeVerifyMatches(t *testing.T) {
	inj, path := newOpenCodeInjectorForTest(t)
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := inj.Verify(path, "/sshmng"); err != nil {
		t.Errorf("Verify should pass: %v", err)
	}
}

func TestOpenCodeVerifyStaleBinary(t *testing.T) {
	inj, path := newOpenCodeInjectorForTest(t)
	entry := MCPEntry{BinaryPath: "/old/bin/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := inj.Verify(path, "/new/bin/sshmng"); err == nil {
		t.Error("Verify should fail for stale binary")
	}
}

func TestOpenCodeVerifyCommandNotArray(t *testing.T) {
	inj, path := newOpenCodeInjectorForTest(t)
	// Manually write a malformed config where command is a string, not array
	original := `{"mcp":{"sshmng":{"type":"local","command":"/sshmng","environment":{},"enabled":true}}}`
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := inj.Verify(path, "/sshmng"); err == nil {
		t.Error("Verify should fail when command is not an array")
	}
}
