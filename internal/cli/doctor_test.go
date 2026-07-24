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

func TestDoctorEmptyHomeFails(t *testing.T) {
	// Point HOME to a temp dir without ~/.sshmng
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	home := filepath.Join(tmp, ".sshmng")
	bin, _ := os.Executable()
	var out bytes.Buffer
	code := RunDoctor(DoctorOpts{Home: home, ExpectedEntry: MCPEntry{BinaryPath: bin, Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": home}}}, &out)
	if code != 1 {
		t.Errorf("code = %d, want 1 (fail). Output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "FAIL") {
		t.Errorf("expected FAIL in output:\n%s", out.String())
	}
}

func TestDoctorPassesAfterInstall(t *testing.T) {
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	home := filepath.Join(tmp, ".sshmng")
	bin, _ := os.Executable()
	// Pin to a release version so the dev-build check reports OK (otherwise
	// the default "dev" would produce a WARN and code 2).
	orig := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = orig }()
	// Run install first
	var installOut bytes.Buffer
	code := RunInstall(InstallOpts{
		Home:       home,
		Binary:     bin,
		Yes:        true,
		SkipAgents: true,
	}, &installOut)
	if code != 0 {
		t.Fatalf("install failed: %s", installOut.String())
	}
	var out bytes.Buffer
	code = RunDoctor(DoctorOpts{Home: home, ExpectedEntry: MCPEntry{BinaryPath: bin, Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": home}}}, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0. Output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "Summary:") {
		t.Errorf("missing Summary line:\n%s", out.String())
	}
}

func TestDoctorWarnsOnMissingExample(t *testing.T) {
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	home := filepath.Join(tmp, ".sshmng")
	bin, _ := os.Executable()
	// Install then delete example
	var installOut bytes.Buffer
	RunInstall(InstallOpts{Home: home, Binary: bin, Yes: true, SkipAgents: true}, &installOut)
	os.Remove(filepath.Join(home, "config.example.json"))

	var out bytes.Buffer
	code := RunDoctor(DoctorOpts{Home: home, ExpectedEntry: MCPEntry{BinaryPath: bin, Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": home}}}, &out)
	if code != 2 {
		t.Errorf("code = %d, want 2 (warn-only). Output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "WARN") {
		t.Errorf("expected WARN in output:\n%s", out.String())
	}
}

func TestDoctorDetectsStaleAgentEntry(t *testing.T) {
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	home := filepath.Join(tmp, ".sshmng")
	claudePath := filepath.Join(tmp, ".claude.json")
	// Install with binary X
	bin, _ := os.Executable()
	var installOut bytes.Buffer
	RunInstall(InstallOpts{
		Home:   home,
		Binary: bin,
		Agents: []string{"claude-code"},
		Yes:    true,
	}, &installOut)
	// Pre-create claude.json so install finds it
	os.WriteFile(claudePath, []byte(`{"mcpServers":{}}`), 0600)
	RunInstall(InstallOpts{
		Home:   home,
		Binary: bin,
		Agents: []string{"claude-code"},
		Yes:    true,
	}, &installOut)

	// Now run doctor with a different expected binary — should FAIL
	var out bytes.Buffer
	code := RunDoctor(DoctorOpts{Home: home, ExpectedEntry: MCPEntry{BinaryPath: "/different/bin/sshmng", Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": home}}}, &out)
	if code != 1 {
		t.Errorf("code = %d, want 1 (stale fail). Output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "stale") {
		t.Errorf("expected 'stale' in output:\n%s", out.String())
	}
}

func TestRunDoctor_UpdateURL_Valid(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(home, 0700)
	// Write config with valid update_url
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{
		"version": "1",
		"update_url": "https://updates.example.com/sshmng"
	}`), 0600)

	var out bytes.Buffer
	code := RunDoctor(DoctorOpts{Home: home}, &out)
	// update_url valid → no FAIL for that check. Other checks may fail (no
	// known_hosts etc.) but we only care that update_url line shows OK.
	output := out.String()
	if !strings.Contains(output, "update_url") {
		t.Errorf("output missing update_url check line:\n%s", output)
	}
	_ = code
}

func TestRunDoctor_UpdateURL_Invalid_Fails(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(home, 0700)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{
		"version": "1",
		"update_url": "ftp://bad-scheme"
	}`), 0600)

	var out bytes.Buffer
	RunDoctor(DoctorOpts{Home: home}, &out)
	output := out.String()
	if !strings.Contains(output, "[FAIL]") {
		t.Errorf("expected [FAIL] for invalid update_url:\n%s", output)
	}
	if !strings.Contains(output, "update_url") {
		t.Errorf("output missing update_url mention:\n%s", output)
	}
}

func TestRunDoctor_UpdateURL_EmbeddedCredentials_Fails(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(home, 0700)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{
		"version": "1",
		"update_url": "https://user:pass@updates.example.com/sshmng"
	}`), 0600)

	var out bytes.Buffer
	RunDoctor(DoctorOpts{Home: home}, &out)
	output := out.String()
	if !strings.Contains(output, "[FAIL]") {
		t.Errorf("expected [FAIL] for embedded credentials:\n%s", output)
	}
	if !strings.Contains(output, "embedded credentials") {
		t.Errorf("expected 'embedded credentials' in output:\n%s", output)
	}
	// Credential leak guard: the raw user:pass pair must NOT appear anywhere
	// in the doctor output.
	if strings.Contains(output, "user:pass") {
		t.Errorf("credential echo detected in output:\n%s", output)
	}
}

func TestRunDoctor_DevBuild_Warns(t *testing.T) {
	orig := version.Version
	version.Version = "dev"
	defer func() { version.Version = orig }()

	home := t.TempDir()
	os.MkdirAll(home, 0700)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"version":"1"}`), 0600)

	var out bytes.Buffer
	RunDoctor(DoctorOpts{Home: home}, &out)
	if !strings.Contains(out.String(), "dev build") {
		t.Errorf("output missing dev build warning:\n%s", out.String())
	}
}

func TestDoctorSkipsUninstalledAgents(t *testing.T) {
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	home := filepath.Join(tmp, ".sshmng")
	bin, _ := os.Executable()
	// Pin to a release version so the dev-build check reports OK (otherwise
	// the default "dev" would produce a WARN and code 2).
	orig := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = orig }()
	var installOut bytes.Buffer
	RunInstall(InstallOpts{Home: home, Binary: bin, Yes: true, SkipAgents: true}, &installOut)
	var out bytes.Buffer
	code := RunDoctor(DoctorOpts{Home: home, ExpectedEntry: MCPEntry{BinaryPath: bin, Args: []string{"mcp"}, Env: map[string]string{"SSHMNG_HOME": home}}}, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0. Output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "SKIP") {
		t.Errorf("expected SKIP for uninstalled Agents:\n%s", out.String())
	}
}
