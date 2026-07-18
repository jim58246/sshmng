package main

import (
	"path/filepath"
	"testing"
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
