package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jim58246/sshmng/internal/config"
)

// buildResolveConfig builds an in-memory config with two servers and two
// jumphosts for resolveSSHTarget tests. No Validate() is called — resolution
// only matches names, so minimal fields suffice.
func buildResolveConfig() *config.Config {
	return &config.Config{
		Version: "1",
		Servers: []*config.SSHServer{
			{Name: "web1", Addr: "10.0.0.1"},
			{Name: "web2", Addr: "10.0.0.2"},
		},
		Jumphosts: []*config.Jumphost{
			{Name: "bastion1", Addr: "10.1.0.1", SSHJ: true},
			{Name: "bastion2", Addr: "10.1.0.2", SSHJ: false},
		},
	}
}

func TestResolveSSHTarget_ExactServer(t *testing.T) {
	cfg := buildResolveConfig()
	srv, jump, err := resolveSSHTarget(cfg, "web1", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if srv == nil || srv.Name != "web1" {
		t.Errorf("expected server web1, got %+v", srv)
	}
	if jump != nil {
		t.Errorf("expected nil jumphost, got %+v", jump)
	}
}

func TestResolveSSHTarget_JumphostFallback(t *testing.T) {
	cfg := buildResolveConfig()
	srv, jump, err := resolveSSHTarget(cfg, "bastion1", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if srv != nil {
		t.Errorf("expected nil server, got %+v", srv)
	}
	if jump == nil || jump.Name != "bastion1" {
		t.Errorf("expected jumphost bastion1, got %+v", jump)
	}
}

func TestResolveSSHTarget_NoMatch(t *testing.T) {
	cfg := buildResolveConfig()
	srv, jump, err := resolveSSHTarget(cfg, "nope", false)
	if err == nil {
		t.Errorf("expected error for no match, got server=%+v jumphost=%+v", srv, jump)
	}
}

// Server names take precedence over jumphost names on collision.
func TestResolveSSHTarget_ServerWinsOnCollision(t *testing.T) {
	cfg := buildResolveConfig()
	// Add a jumphost sharing a server's name.
	cfg.Jumphosts = append(cfg.Jumphosts, &config.Jumphost{Name: "web1", Addr: "10.2.0.1", SSHJ: true})
	srv, jump, err := resolveSSHTarget(cfg, "web1", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if srv == nil || srv.Name != "web1" {
		t.Errorf("expected server web1 to win, got %+v", srv)
	}
	if jump != nil {
		t.Errorf("expected nil jumphost on collision, got %+v", jump)
	}
}

// Fuzzy single match falls back to jumphost when no server matches.
func TestResolveSSHTarget_FuzzyJumphostFallback(t *testing.T) {
	cfg := buildResolveConfig()
	// "bastion" substring-matches bastion1 and bastion2 → multiple; use a
	// unique substring to get a single jumphost fuzzy match with no server.
	srv, jump, err := resolveSSHTarget(cfg, "bastion2", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if srv != nil {
		t.Errorf("expected nil server, got %+v", srv)
	}
	if jump == nil || jump.Name != "bastion2" {
		t.Errorf("expected jumphost bastion2, got %+v", jump)
	}
}

// writeBastionConfig writes a minimal config with one bastion jumphost
// (ssh_j=false) to a temp file with 0600 perms, returning its path.
func writeBastionConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := []byte(`{
  "version": "1",
  "jumphosts": [
    {"name": "bastion", "addr": "10.0.0.1:22", "user": "u", "auth": {"password": "p"}, "ssh_j": false, "login_flow": {"start": {"send": "1\n", "expects": [{"pattern": "$", "next": "success"}]}}, "login_entry": "start"}
  ]
}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestRunSSHCmd_BastionNonInteractiveRejected verifies that a non-interactive
// command against a bastion (ssh_j=false) is rejected before any SSH dial —
// the menu landing has no shell command boundary. No SSH server needed: the
// error fires at resolution time, before setupJumphostSSH dials.
func TestRunSSHCmd_BastionNonInteractiveRejected(t *testing.T) {
	cfgPath := writeBastionConfig(t)
	var out strings.Builder
	code := runSSHCmd(context.Background(), []string{"--config", cfgPath, "bastion", "ls"}, &out)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "not supported for bastion") {
		t.Errorf("expected bastion rejection, got: %s", out.String())
	}
}

// TestRunSSHCmd_RawNonInteractiveRejected verifies that a non-interactive
// command against a raw device (no unix shell) is rejected before any SSH
// dial — raw devices can't run DetectShell/InjectRC, so Run() can't work.
// No SSH server needed: the error fires at resolution time.
func TestRunSSHCmd_RawNonInteractiveRejected(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{
  "version": "1",
  "servers": [
    {"name": "sw1", "addr": "10.0.0.9:22", "user": "u", "auth": {"password": "p"}, "raw": true}
  ]
}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var out strings.Builder
	code := runSSHCmd(context.Background(), []string{"--config", cfgPath, "sw1", "show version"}, &out)
	if code != 1 {
		t.Errorf("code = %d, want 1; output: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "raw device: no unix shell, non-interactive mode not supported") {
		t.Errorf("expected raw rejection, got: %s", out.String())
	}
}

// TestRunSSHCmd_TransparentJumphostNonInteractiveAccepted verifies that a
// non-interactive command against a transparent (ssh_j=true) jumphost is NOT
// rejected at the bastion check — it proceeds to dial (and fails on the
// unreachable host). This guards the ssh_j branch of the guard.
func TestRunSSHCmd_TransparentJumphostNonInteractiveAccepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := []byte(`{
  "version": "1",
  "jumphosts": [
    {"name": "jump", "addr": "127.0.0.1:1", "user": "u", "auth": {"password": "p"}, "ssh_j": true}
  ]
}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var out strings.Builder
	code := runSSHCmd(context.Background(), []string{"--config", path, "jump", "ls"}, &out)
	// Must NOT be the bastion rejection; it should be a connect error (code 1).
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if strings.Contains(out.String(), "not supported for bastion") {
		t.Errorf("ssh_j=true jumphost should not hit bastion rejection, got: %s", out.String())
	}
}
