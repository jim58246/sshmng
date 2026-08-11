//go:build !windows

package termutil

import "os"

// EnableVTFile is a no-op on Unix: POSIX terminals natively interpret VT
// sequences (cursor movement, clear-line/screen) and use xterm LF semantics.
// Returns (0, nil); RestoreVTFile is likewise a no-op. The Bar still calls
// this (and sets vtEnabled=true on nil error) so it uses the VT clear path.
func EnableVTFile(*os.File) (uint32, error) { return 0, nil }

// RestoreVTFile is a no-op on Unix.
func RestoreVTFile(*os.File, uint32) {}

// EnableVTOutput enables VT on stdout. Kept for backward compatibility with
// pty.Relay; delegates to EnableVTFile(os.Stdout).
func EnableVTOutput() (uint32, error) { return EnableVTFile(os.Stdout) }

// RestoreOutputMode restores stdout's mode saved by EnableVTOutput.
func RestoreOutputMode(old uint32) { RestoreVTFile(os.Stdout, old) }
