package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRestoreFromBackup verifies that restoreFromBackup copies the newest
// <path>.bak.* backup back over path. This is the data-safety helper used when
// an atomic write fails after the original has been deleted (spec line 288).
func TestRestoreFromBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	contentA := []byte(`{"v":"A"}`)
	contentB := []byte(`{"v":"B"}`)

	// 1. Write content A.
	if err := os.WriteFile(path, contentA, 0600); err != nil {
		t.Fatal(err)
	}
	// 2. backupFile -> creates <path>.bak.<ts> with content A.
	if err := backupFile(path); err != nil {
		t.Fatalf("backupFile: %v", err)
	}
	// 3. Overwrite the live file with content B (simulating a partial write or
	//    a delete-then-failed-rename where the original is gone).
	if err := os.WriteFile(path, contentB, 0600); err != nil {
		t.Fatal(err)
	}
	// 4. restoreFromBackup should bring the file back to content A.
	if err := restoreFromBackup(path); err != nil {
		t.Fatalf("restoreFromBackup: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(contentA) {
		t.Errorf("after restore got %q, want %q", got, contentA)
	}
}

// TestRestoreFromBackupPicksNewest verifies that when multiple backups exist,
// restoreFromBackup picks the newest by mtime.
func TestRestoreFromBackupPicksNewest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Two backups: older with "OLD", newer with "NEW".
	oldBackup := filepath.Join(dir, "config.json.bak.20260101-000000")
	newBackup := filepath.Join(dir, "config.json.bak.20260102-000000")
	if err := os.WriteFile(oldBackup, []byte("OLD"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newBackup, []byte("NEW"), 0600); err != nil {
		t.Fatal(err)
	}
	mtimeOld := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mtimeNew := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	// Set mtimes so newBackup is newer. On some filesystems sub-second precision
	// matters, but a full day gap is unambiguous.
	if err := os.Chtimes(oldBackup, mtimeOld, mtimeOld); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newBackup, mtimeNew, mtimeNew); err != nil {
		t.Fatal(err)
	}
	// Live file does not matter; restore should overwrite with NEW.
	if err := os.WriteFile(path, []byte("LIVE"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restoreFromBackup(path); err != nil {
		t.Fatalf("restoreFromBackup: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "NEW" {
		t.Errorf("got %q, want NEW", got)
	}
}

// TestRestoreFromBackupNoBackup verifies that restoreFromBackup errors clearly
// when no backup exists.
func TestRestoreFromBackupNoBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	err := restoreFromBackup(path)
	if err == nil {
		t.Fatal("expected error when no backup exists")
	}
	if !strings.Contains(err.Error(), "no backup") {
		t.Errorf("error should mention no backup: %v", err)
	}
}
