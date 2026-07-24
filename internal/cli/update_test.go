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

func TestRunUpdate_DevBuild_FailsExit1(t *testing.T) {
	orig := version.Version
	version.Version = "dev"
	defer func() { version.Version = orig }()

	var out bytes.Buffer
	code := runUpdate(context.Background(), []string{}, &out)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (dev build)", code)
	}
	output := out.String()
	if !strings.Contains(output, "[FAIL]") {
		t.Errorf("output missing [FAIL]: %s", output)
	}
	if !strings.Contains(output, "version not set") {
		t.Errorf("output missing dev-build hint: %s", output)
	}
}

func TestRunUpdate_BadFlag_Exits2(t *testing.T) {
	var out bytes.Buffer
	code := runUpdate(context.Background(), []string{"--bogus"}, &out)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (bad flag)", code)
	}
}

// TestRunUpdate_AlreadyUpToDate_Hermetic verifies the already-up-to-date
// path (applied=false, exit 0) without touching the real network. A local
// httptest.Server serves latest.txt whose version is NOT newer than the
// injected current version, so UpdateToLatest short-circuits before any
// binary download/swap attempt.
func TestRunUpdate_AlreadyUpToDate_Hermetic(t *testing.T) {
	orig := version.Version
	version.Version = "v9.9.9" // newer than what the server reports
	defer func() { version.Version = orig }()

	// Hermetic: local HTTP server serving latest.txt = v1.0.0 (older than
	// v9.9.9), so isNewer returns false → applied=false → exit 0. No binary
	// download is attempted (UpdateToLatest short-circuits before UpdateSelf).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest.txt" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("v1.0.0\n"))
			return
		}
		http.NotFound(w, r)
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
	code := runUpdate(context.Background(), []string{}, &out)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (already up-to-date)", code)
	}
	output := out.String()
	if !strings.Contains(output, "Already at latest version") {
		t.Errorf("output missing 'Already at latest version': %s", output)
	}
	if !strings.Contains(output, "v9.9.9") {
		t.Errorf("output missing current version v9.9.9: %s", output)
	}
}

// TestRunUpdate_RemoteFailure_FailsExit1_Hermetic verifies the error path
// (remote returns 500 → LatestVersion fails → [FAIL] → exit 1) without
// touching the real network.
func TestRunUpdate_RemoteFailure_FailsExit1_Hermetic(t *testing.T) {
	orig := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = orig }()

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
	code := runUpdate(context.Background(), []string{}, &out)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (remote failure)", code)
	}
	if !strings.Contains(out.String(), "[FAIL]") {
		t.Errorf("output missing [FAIL]: %s", out.String())
	}
}
