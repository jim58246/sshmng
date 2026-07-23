package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jim58246/sshmng/internal/version"
)

// setupInstallTest creates a temp HOME and returns (home, claudePath).
func setupInstallTest(t *testing.T) (home string, claudePath string) {
	t.Helper()
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	home = filepath.Join(tmp, ".sshmng")
	claudePath = filepath.Join(tmp, ".claude.json")
	return home, claudePath
}

func TestRunInstallCreatesFiles(t *testing.T) {
	home, _ := setupInstallTest(t)
	orig := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = orig }()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := RunInstall(InstallOpts{
		Home:       home,
		Binary:     bin,
		Agents:     nil, // none
		Yes:        true,
		SkipAgents: true,
	}, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0. Output:\n%s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); err != nil {
		t.Errorf("config.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.example.json")); err != nil {
		t.Errorf("config.example.json missing: %v", err)
	}
}

func TestRunInstallInjectsClaudeCode(t *testing.T) {
	home, claudePath := setupInstallTest(t)
	orig := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = orig }()
	// Pre-create claude.json so Detect finds it
	if err := os.WriteFile(claudePath, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	bin, _ := os.Executable()
	var out bytes.Buffer
	code := RunInstall(InstallOpts{
		Home:       home,
		Binary:     bin,
		Agents:     []string{"claude-code"},
		Yes:        true,
		SkipFiles:  false,
		SkipAgents: false,
	}, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0. Output:\n%s", code, out.String())
	}
	data, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"sshmng"`) {
		t.Errorf("sshmng entry missing:\n%s", s)
	}
	if !strings.Contains(s, bin) {
		t.Errorf("binary path missing:\n%s", s)
	}
}

func TestRunInstallPreservesExistingConfigJSON(t *testing.T) {
	home, _ := setupInstallTest(t)
	orig := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = orig }()
	original := `{"version":"1","idle_timeout_s":600,"jumphosts":[],"proxies":[],"servers":[]}`
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	bin, _ := os.Executable()
	var out bytes.Buffer
	code := RunInstall(InstallOpts{
		Home:       home,
		Binary:     bin,
		Agents:     nil,
		Yes:        true,
		SkipAgents: true,
	}, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	data, _ := os.ReadFile(filepath.Join(home, "config.json"))
	if !strings.Contains(string(data), `"idle_timeout_s":600`) {
		t.Errorf("existing config.json was modified:\n%s", string(data))
	}
}

func TestRunInstallCreatesBackupBeforeInject(t *testing.T) {
	home, claudePath := setupInstallTest(t)
	orig := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = orig }()
	original := `{"mcpServers":{"other":{"command":"x"}}}`
	if err := os.WriteFile(claudePath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	bin, _ := os.Executable()
	var out bytes.Buffer
	code := RunInstall(InstallOpts{
		Home:   home,
		Binary: bin,
		Agents: []string{"claude-code"},
		Yes:    true,
	}, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	entries, err := os.ReadDir(filepath.Dir(claudePath))
	if err != nil {
		t.Fatal(err)
	}
	backupCount := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".claude.json.bak.") {
			backupCount++
		}
	}
	if backupCount != 1 {
		t.Errorf("expected 1 backup, got %d", backupCount)
	}
}

// TestRunInstallYesAutoDetectsAgents verifies that --yes mode with no explicit
// --agents flag and no --skip-agents auto-injects into all detected (installed)
// Agents. This matches the spec default "auto-detect" for the --agents flag.
func TestRunInstallYesAutoDetectsAgents(t *testing.T) {
	home, claudePath := setupInstallTest(t)
	orig := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = orig }()
	// Pre-create claude.json so Detect finds it
	if err := os.WriteFile(claudePath, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	bin, _ := os.Executable()
	var out bytes.Buffer
	code := RunInstall(InstallOpts{
		Home:       home,
		Binary:     bin,
		Agents:     nil, // no explicit list
		Yes:        true,
		SkipAgents: false,
	}, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0. Output:\n%s", code, out.String())
	}
	// Claude Code was detected (claude.json exists), so it should be injected
	data, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"sshmng"`) {
		t.Errorf("--yes auto-detect did not inject into detected Claude Code:\n%s\nOutput:\n%s", string(data), out.String())
	}
}
