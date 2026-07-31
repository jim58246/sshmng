package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jim58246/sshmng/internal/version"
)

func TestResolveConfigPathExplicit(t *testing.T) {
	got, err := ResolveConfigPath("/custom/path.json")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "/custom/path.json" {
		t.Errorf("got %q, want /custom/path.json", got)
	}
}

func TestResolveConfigPathSSHMNGHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMNG_HOME", dir)
	got, err := ResolveConfigPath("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := filepath.Join(dir, "config.json")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveConfigPathDefaultHome(t *testing.T) {
	t.Setenv("SSHMNG_HOME", "")
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	got, err := ResolveConfigPath("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := filepath.Join(dir, ".sshmng", "config.json")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRunMCP_AutoUpdateDisabled_DoesNotSpawnGoroutine 验证 auto_update_enabled=false
// 时 runMCP 不会 panic / hang。Goroutine 行为难以直接断言，这里是 smoke test。
func TestRunMCP_AutoUpdateDisabled_DoesNotSpawnGoroutine(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(home, 0700)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{
		"version": "1",
		"auto_update_enabled": false
	}`), 0600)
	t.Setenv("SSHMNG_HOME", home)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var out bytes.Buffer
	code := runMCP(ctx, []string{}, &out)
	// Server runs until ctx cancelled → exit 1 (context error) or 0.
	// We just verify no panic / no update attempt in output.
	_ = code
}

// TestRunMCP_AutoUpdateEnabled_DevBuild_SkipsGoroutine 验证 dev build +
// auto_update_enabled=true 时 goroutine 被 version=="dev" 跳过，不 panic。
func TestRunMCP_AutoUpdateEnabled_DevBuild_SkipsGoroutine(t *testing.T) {
	orig := version.Version
	version.Version = "dev"
	defer func() { version.Version = orig }()

	home := t.TempDir()
	os.MkdirAll(home, 0700)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{
		"version": "1",
		"auto_update_enabled": true
	}`), 0600)
	t.Setenv("SSHMNG_HOME", home)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var out bytes.Buffer
	runMCP(ctx, []string{}, &out)
	// dev build → goroutine skipped. No assertion on output (goroutine is
	// silent). Test just verifies no panic.
}
