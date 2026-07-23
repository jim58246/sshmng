package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	code := RunDoctor(DoctorOpts{Home: home, ExpectedBinary: bin}, &out)
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
	code = RunDoctor(DoctorOpts{Home: home, ExpectedBinary: bin}, &out)
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
	code := RunDoctor(DoctorOpts{Home: home, ExpectedBinary: bin}, &out)
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
	code := RunDoctor(DoctorOpts{Home: home, ExpectedBinary: "/different/bin/sshmng"}, &out)
	if code != 1 {
		t.Errorf("code = %d, want 1 (stale fail). Output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "stale") {
		t.Errorf("expected 'stale' in output:\n%s", out.String())
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
	var installOut bytes.Buffer
	RunInstall(InstallOpts{Home: home, Binary: bin, Yes: true, SkipAgents: true}, &installOut)
	var out bytes.Buffer
	code := RunDoctor(DoctorOpts{Home: home, ExpectedBinary: bin}, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0. Output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "SKIP") {
		t.Errorf("expected SKIP for uninstalled Agents:\n%s", out.String())
	}
}
