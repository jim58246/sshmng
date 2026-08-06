package update

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSweepRemovesDefaultOld: a removable ".<base>.old" (from a prior update,
// no longer locked) is removed, freeing the path for Apply's rename target.
func TestSweepRemovesDefaultOld(t *testing.T) {
	dir := t.TempDir()
	const base = "sshmng.exe"
	defOld := filepath.Join(dir, "."+base+".old")
	mustWrite(t, defOld, "old bytes")

	sweepStaleOldBinaries(dir, base, nil)

	if _, err := os.Stat(defOld); !os.IsNotExist(err) {
		t.Fatalf(".%s.old should be removed, stat err=%v", base, err)
	}
}

// TestSweepRemovesRotatedLeftovers: prior rotated leftovers (".<base>.old.<ts>")
// that are no longer locked are swept away so they don't accumulate.
func TestSweepRemovesRotatedLeftovers(t *testing.T) {
	dir := t.TempDir()
	const base = "sshmng.exe"
	left1 := filepath.Join(dir, "."+base+".old.1700000000000")
	left2 := filepath.Join(dir, "."+base+".old.1700000000001")
	mustWrite(t, left1, "a")
	mustWrite(t, left2, "b")

	sweepStaleOldBinaries(dir, base, nil)

	for _, p := range []string{left1, left2} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be removed, stat err=%v", filepath.Base(p), err)
		}
	}
}

// TestSweepRotatesLockedDefaultOld: when the default .old cannot be removed
// (locked by a still-running old process on Windows), sweep must RENAME it out
// of the way so Apply's rename-into-.old succeeds. Rename works on locked files
// on Windows; this test exercises the remove-fails→rename branch on Unix by
// making ".<base>.old" a non-empty directory (os.Remove fails with ENOTEMPTY,
// os.Rename succeeds).
func TestSweepRotatesLockedDefaultOld(t *testing.T) {
	dir := t.TempDir()
	const base = "sshmng.exe"
	defOld := filepath.Join(dir, "."+base+".old")
	// non-empty dir: os.Remove fails, os.Rename succeeds — same branch as a
	// locked file on Windows.
	if err := os.MkdirAll(filepath.Join(defOld, "inner"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	sweepStaleOldBinaries(dir, base, func(string) {})

	// default .old must be gone (rotated away)
	if _, err := os.Stat(defOld); !os.IsNotExist(err) {
		t.Fatalf(".%s.old should be rotated away, stat err=%v", base, err)
	}
	// a rotated ".<base>.old.<ts>" must exist
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var found bool
	for _, e := range entries {
		name := e.Name()
		if len(name) > len("." +base+ ".old.") &&
			name[:len("." +base+ ".old.")] == "." +base+ ".old." {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a rotated .%s.old.<ts> entry, got %v", base, entries)
	}
}

// TestSweepDoesNothingWhenNoOld: clean dir → no error, no changes.
func TestSweepDoesNothingWhenNoOld(t *testing.T) {
	dir := t.TempDir()
	sweepStaleOldBinaries(dir, "sshmng.exe", nil) // must not panic / error
}

// TestSweepIgnoresUnrelatedFiles: files not matching the .old pattern are left
// alone (e.g. the real sshmng.exe, unrelated dotfiles).
func TestSweepIgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	const base = "sshmng.exe"
	keep := []string{base, ".config", "readme.txt", ".sshmng.exe.new"}
	for _, name := range keep {
		mustWrite(t, filepath.Join(dir, name), "x")
	}

	sweepStaleOldBinaries(dir, base, nil)

	for _, name := range keep {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("unrelated file %q should be untouched, stat err=%v", name, err)
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
