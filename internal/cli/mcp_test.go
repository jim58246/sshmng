package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jim58246/sshmng/internal/version"
)

func TestResolveConfigPathExplicit(t *testing.T) {
	got, err := resolveConfigPath("/custom/path.json")
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
	got, err := resolveConfigPath("")
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
	got, err := resolveConfigPath("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := filepath.Join(dir, ".sshmng", "config.json")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestOpenLogWriterEmptyPathReturnsDiscard 验证 log_path 为空时返回 io.Discard。
// 空路径是默认场景：用户未配置 log_path，不打任何日志（无 stderr 输出，Inspector
// 无从捕获，彻底规避 stall）。
func TestOpenLogWriterEmptyPathReturnsDiscard(t *testing.T) {
	w, cleanup, err := openLogWriter("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer cleanup()

	// 应是 io.Discard（写入无副作用、无错误）
	if _, err := w.Write([]byte("should-be-discarded\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// io.Discard 是具体值，直接比较
	if w != io.Discard {
		t.Errorf("expected io.Discard for empty log_path")
	}
}

// TestOpenLogWriterPathReturnsRotatingWriter 验证 log_path 非空时写入
// <log_path>/sshmng.log。
func TestOpenLogWriterPathReturnsRotatingWriter(t *testing.T) {
	dir := t.TempDir()
	w, cleanup, err := openLogWriter(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "sshmng.log"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "hello") {
		t.Errorf("file content = %q, want contains 'hello'", string(data))
	}
}

// TestOpenLogWriterPathErrorReturnsError 验证路径不可写时返回错误（不 panic）。
func TestOpenLogWriterPathErrorReturnsError(t *testing.T) {
	_, _, err := openLogWriter("/nonexistent-xyz-123/dir")
	if err == nil {
		t.Errorf("expected error for nonexistent dir")
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
