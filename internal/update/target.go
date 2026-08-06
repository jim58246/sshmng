package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// prepareWindowsUpdateTarget clears the way for goupdate.Apply's
// rename-into-.old step on Windows (and harmlessly sweeps stale leftovers on
// Unix, where Apply already removes the .old itself).
//
// Apply (github.com/creativeprojects/go-selfupdate/update) renames the running
// binary <base> to ".<base>.old", then swaps in the new binary. On Windows two
// failure modes corrupt the second update:
//
//  1. The previous update's ".<base>.old" may still be locked by a long-running
//     sshmng process (typically the MCP server) that was running from the
//     binary at the moment it was renamed. Apply's os.Remove(.old) fails
//     silently, then os.Rename(target, .old) fails because the destination
//     exists and is locked → the update aborts. (First update works; second
//     fails because the first's .old is still locked.)
//  2. Rotated leftovers accumulate.
//
// Fix: ensure ".<base>.old" is free before Apply. Try os.Remove; if locked,
// RENAME it to ".<base>.old.<ts>" — Windows allows renaming a file that's in
// use (the same premise Apply relies on to rename the running exe). Then sweep
// prior rotated leftovers (".<base>.old.*"), best-effort removing unlocked ones
// and hiding locked ones for a future sweep.
//
// On Unix this is effectively a no-op: POSIX rename atomically replaces (no
// "destination exists" failure) and Apply's own os.Remove(.old) succeeds (no
// file-in-use lock), so no .old is left to sweep.
func prepareWindowsUpdateTarget(targetPath string) {
	dir := filepath.Dir(targetPath)
	base := filepath.Base(targetPath)
	sweepStaleOldBinaries(dir, base, hideOldBinary)
}

// sweepStaleOldBinaries is the cross-platform core of
// prepareWindowsUpdateTarget. hide is called on files that cannot be removed
// (locked on Windows); on Unix hide is a no-op.
func sweepStaleOldBinaries(dir, base string, hide func(string)) {
	defOld := filepath.Join(dir, "."+base+".old")

	// 1. Clear the default .old Apply will rename into.
	if _, err := os.Stat(defOld); err == nil {
		if err := os.Remove(defOld); err != nil {
			// Locked (a still-running old process holds it). Rotate it out of
			// the way: rename works on locked files on Windows, freeing defOld.
			rotated := filepath.Join(dir, fmt.Sprintf(".%s.old.%d", base, time.Now().UnixMilli()))
			if rerr := os.Rename(defOld, rotated); rerr == nil && hide != nil {
				hide(rotated)
			}
			// If rename also fails (very unusual), leave defOld; Apply will fail
			// with a clear error. Nothing more we can do here.
		}
	}

	// 2. Best-effort: remove prior rotated leftovers no longer locked.
	prefix := "." + base + ".old."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if rerr := os.Remove(p); rerr != nil && hide != nil {
			hide(p) // still locked; keep it hidden and out of the way
		}
	}
}
