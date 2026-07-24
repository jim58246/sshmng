package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestRunVersion_CheckFlag_RemoteFailure_WarnsAndExits0(t *testing.T) {
	orig := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = orig }()

	// Hermetic: stand up a local HTTP server that fails /latest.txt with
	// HTTP 500. Config points update_url at it, so LatestVersion hits this
	// server (no real network call) and fails → [WARN] printed → exit 0.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	tmpHome := t.TempDir()
	t.Setenv("SSHMNG_HOME", tmpHome)
	if err := os.MkdirAll(tmpHome, 0700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	cfgJSON, err := json.Marshal(map[string]string{
		"version":    "1",
		"update_url": srv.URL,
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpHome, "config.json"), cfgJSON, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out bytes.Buffer
	code := runVersion(context.Background(), []string{"--check"}, &out)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (--check warn-only)", code)
	}
	output := out.String()
	if !strings.Contains(output, "[WARN]") {
		t.Errorf("output missing [WARN] (warn path not exercised): %s", output)
	}
	if !strings.Contains(output, "sshmng v1.2.3") {
		t.Errorf("output missing version line: %s", output)
	}
}

func TestRunVersion_BadFlag_Exits2(t *testing.T) {
	var out bytes.Buffer
	code := runVersion(context.Background(), []string{"--bogus"}, &out)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (bad flag)", code)
	}
}
