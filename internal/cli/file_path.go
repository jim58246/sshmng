package cli

import (
	"path/filepath"
	"strings"
)

// resolveDstPath applies scp/cp-style destination resolution: when the caller
// has determined dst is (or should be treated as) a directory, the source
// file's basename is appended to dst so the transfer lands inside that
// directory under the same filename.
//
//	sshmng file download srv /root/abc.txt ./   → ./abc.txt
//	sshmng file download srv /root/abc.txt /tmp/ → /tmp/abc.txt
//	sshmng file upload   srv ./abc.txt /root/    → /root/abc.txt
//
// isDir is the caller's pre-check (os.Stat / sftp.Stat IsDir, OR the user wrote
// a trailing "/" / "." which signals "this is a directory" even if it doesn't
// exist yet). When isDir is false, dst is returned unchanged (it's a file path).
//
// For local paths the basename uses filepath.Base (OS-correct separators); for
// remote paths (isRemoteDst=true) it uses path.Base ("/"-only, as SSH/sftp
// paths are always POSIX). The separator join is likewise OS-correct for local
// ("/tmp" + "/" + "abc.txt" on Unix, "\" on Windows via filepath.Join).
func resolveDstPath(src, dst string, isDir bool) string {
	if !isDir {
		return dst
	}
	// "." → use bare basename in the current directory.
	if dst == "." {
		return baseName(src)
	}
	// Trailing slash (or backslash): strip it, then join with basename.
	dst = strings.TrimRight(dst, "/\\")
	if dst == "" {
		// dst was just "/" or "\" → root + basename.
		return "/" + baseName(src)
	}
	return dst + sep(dst) + baseName(src)
}

// baseName returns the filename component of src, handling both "/" (remote /
// POSIX local) and the OS separator (local on Windows). Uses filepath.Base
// which on Unix treats "/" as separator and on Windows treats both "/" and "\".
func baseName(src string) string {
	// path.Base handles the "/" case uniformly; filepath.Base handles OS sep.
	// Use filepath.Base since it also splits on "/" on all OSes (Go's filepath
	// treats "/" as a separator even on Windows).
	return filepath.Base(src)
}

// sep returns the path separator matching dst's convention: if dst uses "\\"
// (Windows local), return OS separator; otherwise "/".
func sep(dst string) string {
	if strings.Contains(dst, "\\") {
		return string(filepath.Separator)
	}
	return "/"
}
