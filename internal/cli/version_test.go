package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jim58246/sshmng/internal/version"
)

func TestRunVersion_PrintsVersion(t *testing.T) {
	orig := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = orig }()

	var out bytes.Buffer
	code := runVersion(context.Background(), []string{}, &out)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	output := out.String()
	if !strings.Contains(output, "sshmng v1.2.3") {
		t.Errorf("output missing version: %s", output)
	}
}

func TestRunVersion_DevBuild(t *testing.T) {
	orig := version.Version
	version.Version = "dev"
	defer func() { version.Version = orig }()

	var out bytes.Buffer
	code := runVersion(context.Background(), []string{}, &out)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "sshmng dev") {
		t.Errorf("output missing dev marker: %s", out.String())
	}
}

func TestRunVersion_CheckFlag_NoUpdateURL_WarnsButExits0(t *testing.T) {
	orig := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = orig }()

	// No config file → default config has empty UpdateURL
	// runVersion needs a config to check update_url. Use temp home.
	tmpHome := t.TempDir()
	t.Setenv("SSHMNG_HOME", tmpHome)
	// Write minimal config so resolveConfigPath works
	os.MkdirAll(tmpHome, 0700)
	os.WriteFile(filepath.Join(tmpHome, "config.json"), []byte(`{"version":"1"}`), 0600)

	var out bytes.Buffer
	code := runVersion(context.Background(), []string{"--check"}, &out)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (--check warn-only)", code)
	}
	if !strings.Contains(out.String(), "sshmng v1.2.3") {
		t.Errorf("output missing version: %s", out.String())
	}
}

func TestRunVersion_BadFlag_Exits2(t *testing.T) {
	var out bytes.Buffer
	code := runVersion(context.Background(), []string{"--bogus"}, &out)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (bad flag)", code)
	}
}
