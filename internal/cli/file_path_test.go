package cli

import (
	"testing"
)

// TestResolveDstPath verifies scp/cp-style destination path resolution: when
// the destination is a directory (ends in /, or is "."), the source filename's
// basename is appended. This lets users write:
//
//	sshmng file download srv /root/abc.txt ./        → ./abc.txt
//	sshmng file download srv /root/abc.txt .         → abc.txt
//	sshmng file download srv /root/abc.txt /tmp/     → /tmp/abc.txt
//	sshmng file upload   srv ./abc.txt /root/        → /root/abc.txt
//
// A non-directory destination is returned unchanged. The remote flag selects
// the path separator (/ for remote, OS-specific for local) — but basename
// extraction always uses / since both local and remote paths in sshmng use /
// (remote is always /; local on Windows would use \, handled by filepath).
func TestResolveDstPath(t *testing.T) {
	cases := []struct {
		name    string
		src     string // source file path (has a basename)
		dst     string // destination as typed by the user
		isDir   bool   // dst resolved to an existing directory (caller pre-checked)
		want    string // resolved destination path
	}{
		// download: local dst is a directory
		{"dl-dir-trailing-slash", "/root/abc.txt", "/tmp/", true, "/tmp/abc.txt"},
		{"dl-dot-slash", "/root/abc.txt", "./", true, "./abc.txt"},
		{"dl-dot", "/root/abc.txt", ".", true, "abc.txt"},
		{"dl-dir-no-slash", "/root/abc.txt", "/tmp", true, "/tmp/abc.txt"},
		// download: local dst is a file (not a dir) → unchanged
		{"dl-file-dst", "/root/abc.txt", "/tmp/other.txt", false, "/tmp/other.txt"},
		// upload: remote dst is a directory (ends in /)
		{"up-dir-trailing-slash", "./abc.txt", "/root/", true, "/root/abc.txt"},
		{"up-dir-dot", "abc.txt", ".", true, "abc.txt"}, // local "." as remote dst is unusual but resolve anyway
		// upload: remote dst is a file → unchanged
		{"up-file-dst", "./abc.txt", "/root/other.txt", false, "/root/other.txt"},
		// nested source path: only basename used
		{"nested-src", "/a/b/c/deep.bin", "/dst/", true, "/dst/deep.bin"},
		// source with no slash (bare filename)
		{"bare-src", "abc.txt", "/tmp/", true, "/tmp/abc.txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveDstPath(c.src, c.dst, c.isDir)
			if got != c.want {
				t.Errorf("resolveDstPath(src=%q, dst=%q, isDir=%v) = %q, want %q",
					c.src, c.dst, c.isDir, got, c.want)
			}
		})
	}
}
