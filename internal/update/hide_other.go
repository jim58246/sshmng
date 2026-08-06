//go:build !windows

package update

// hideOldBinary is a no-op on non-Windows: POSIX has no hidden-file attribute,
// and sweepStaleOldBinaries finds no leftovers to hide on Unix anyway (Apply's
// own os.Remove(.old) succeeds, no file-in-use lock).
func hideOldBinary(_ string) {}
