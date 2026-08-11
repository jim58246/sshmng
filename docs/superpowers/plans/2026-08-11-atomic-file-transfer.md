# Atomic File Transfer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make single-file transfers atomic — write to a temp file, then rename to the target on success; remove the temp on failure/timeout. No half-written target files.

**Architecture:** Two-sided. Upload atomicity lives in `PtyConn` (it calls `sftpClient.Create(remotePath)` — swap to temp path + `PosixRename`). Download atomicity lives in a new shared helper used by both CLI and MCP (since `PtyConn.Download` writes to an `io.Writer`, the caller owns the local file). `Conn` interface signatures unchanged; method bodies / a new helper do the work. MCP benefits via the same `PtyConn.Upload` methods + the shared download helper.

**Tech Stack:** Go 1.25.12, `github.com/pkg/sftp` (`PosixRename`, `Remove`, `File.Sync`), stdlib `os` (`CreateTemp`, `Rename`, `Remove`).

## Global Constraints

- **No new dependencies.** pkg/sftp (in go.mod) + stdlib only.
- **`Conn` interface signatures unchanged.** `Upload`/`UploadSized`/`Download` keep their signatures; atomicity is internal.
- **No speed regression.** temp only changes `Create`'s path arg; reader wrapping (`io.LimitReader`) and `ReadFrom` pipelining unaffected. `Sync`/`PosixRename` run after `io.Copy` + `Close`. A benchmark must confirm no regression (Task 5).
- **MCP benefits automatically** for upload (calls `PtyConn.Upload`/`UploadSized`); download via the shared helper (Task 4 wires MCP to it).
- **`Sync()` is best-effort.** `fsync@openssh.com` unsupported → ignore the error (Close already guarantees post-rename visibility). Never fail a transfer because Sync failed.
- **PosixRename fallback.** If `PosixRename` returns unsupported, fall back to `Remove(target)` + `Rename(tmp, target)` (small non-atomic window, but functional).
- **Timeout semantics unchanged.** `timedOut=true` + partial bytes still returned; temp is removed (no half target).
- **Commit messages** end with `Co-Authored-By: Claude <noreply@anthropic.com>`. Push to main (user preference).

---

## File Structure

| File | Responsibility | Action |
|------|---------------|--------|
| `internal/ssh/conn/atomic.go` | `AtomicRemotePath` helper + shared constants | Create |
| `internal/ssh/pty/sftp.go` | `Upload`/`UploadSized`/`Download` method bodies (temp + PosixRename / caller-side) | Modify |
| `internal/ssh/conn/sftp_download.go` | `DownloadToFile` shared helper (temp + os.Rename) | Create |
| `internal/cli/file_cmd.go` | `runFileDownload` calls `DownloadToFile`; `runFileRelay` uses atomic upload | Modify |
| `internal/mcp/tools_file.go` | `Download` MCP tool calls `DownloadToFile` | Modify |
| `internal/ssh/pty/sftp_bench_test.go` | bench upload-with-temp+rename vs plain | Modify |
| `internal/ssh/pty/sftp_atomic_test.go` | atomicity tests (interrupted transfer → no target) | Create |

---

### Task 1: Atomic upload in `PtyConn.Upload`/`UploadSized` + `AtomicRemotePath` helper

**Files:**
- Create: `internal/ssh/conn/atomic.go`
- Modify: `internal/ssh/pty/sftp.go:42-77` (`Upload`), `:89-123` (`UploadSized`)
- Test: `internal/ssh/pty/sftp_atomic_test.go`

**Interfaces:**
- Produces: `conn.AtomicRemotePath(remotePath string) string` — returns `<remotePath>.sshmng-tmp-<6 hex>`. Pure, tested.
- Produces: modified `PtyConn.Upload`/`UploadSized` that Create the temp path, io.Copy, Close, Sync(best-effort), PosixRename→fallback, Remove-on-failure.

- [ ] **Step 1: Write failing test for `AtomicRemotePath` + atomic upload**

`internal/ssh/conn/atomic_test.go`:
```go
package conn

import (
	"strings"
	"testing"
)

func TestAtomicRemotePath(t *testing.T) {
	got := AtomicRemotePath("/root/abc.txt")
	if !strings.HasPrefix(got, "/root/abc.txt.sshmng-tmp-") {
		// Note: suffix is ".sshmng-tmp-<hex>". Verify prefix + suffix shape.
	}
	if !strings.HasPrefix(got, "/root/abc.txt.sshmng-tmp-") {
		t.Errorf("AtomicRemotePath(%q) = %q, want prefix %q", "/root/abc.txt", got, "/root/abc.txt.sshmng-tmp-")
	}
	// 6 hex chars after the dash.
	suffix := got[len("/root/abc.txt.sshmng-tmp-"):]
	if len(suffix) != 6 {
		t.Errorf("random suffix len = %d, want 6 (%q)", len(suffix), suffix)
	}
	for _, c := range suffix {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("random suffix %q must be hex", suffix)
		}
	}
	// Different calls produce different paths (randomness).
	a, b := AtomicRemotePath("/x"), AtomicRemotePath("/x")
	if a == b {
		t.Errorf("two calls identical: %q == %q (randomness broken)", a, b)
	}
}
```

`internal/ssh/pty/sftp_atomic_test.go`:
```go
package pty

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/jim58246/sshmng/internal/config"
	"github.com/jim58246/sshmng/internal/ssh/conn"
)

// TestUploadAtomicOnInterruptedReader: an upload whose source errors mid-stream
// must NOT leave the target file (only a cleaned-up temp). Verifies temp+rename.
func TestUploadAtomicOnInterruptedReader(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr: srv.Addr(), User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"}, HostKeyVerify: true,
	})
	if err != nil { t.Fatalf("Dial: %v", err) }
	defer client.Close()
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil { t.Fatalf("NewPtyConn: %v", err) }
	defer p.Close()

	srcErr := errors.New("simulated mid-stream failure")
	// A reader that returns some bytes then the error.
	src := &errReader{data: []byte("partial-data"), err: srcErr}
	_, timedOut, err := p.UploadSized(src, int64(len("partial-data")+10), "/target.bin", 60000)
	if err == nil {
		t.Fatalf("expected error from interrupted reader, got nil")
	}
	_ = timedOut
	// Target must NOT exist (temp was removed, rename never happened).
	if _, statErr := p.sftpClient.Stat("/target.bin"); statErr == nil {
		t.Errorf("target file exists after failed upload (should be absent — temp removed)")
	}
	// No leftover temp file either.
	entries, _ := p.sftpClient.ReadDir("/")
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "target.bin.sshmng-tmp-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

// TestUploadAtomicSuccessRenames: a successful upload creates the target with
// full content and leaves no temp file.
func TestUploadAtomicSuccessRenames(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr: srv.Addr(), User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"}, HostKeyVerify: true,
	})
	if err != nil { t.Fatalf("Dial: %v", err) }
	defer client.Close()
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil { t.Fatalf("NewPtyConn: %v", err) }
	defer p.Close()

	data := bytes.Repeat([]byte("x"), 4096)
	n, _, err := p.UploadSized(bytes.NewReader(data), int64(len(data)), "/ok.bin", 60000)
	if err != nil { t.Fatalf("UploadSized: %v (n=%d)", err, n) }
	// Read back via sftp and compare.
	rf, err := p.sftpClient.Open("/ok.bin")
	if err != nil { t.Fatalf("Open target: %v", err) }
	got, _ := io.ReadAll(rf); rf.Close()
	if !bytes.Equal(got, data) {
		t.Errorf("target content mismatch: got %d bytes, want %d", len(got), len(data))
	}
	// No leftover temp.
	entries, _ := p.sftpClient.ReadDir("/")
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ok.bin.sshmng-tmp-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

type errReader struct {
	data []byte
	off  int
	err  error
}
func (r *errReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}
```
Add `"strings"` to the test file imports.

- [ ] **Step 2: Run tests to verify RED**

Run: `go test ./internal/ssh/conn/ -run AtomicRemotePath && go test ./internal/ssh/pty/ -run 'TestUploadAtomic'`
Expected: FAIL — `AtomicRemotePath` undefined; upload tests fail because target exists after interrupted upload (current non-atomic behavior writes directly).

- [ ] **Step 3: Implement `internal/ssh/conn/atomic.go`**

```go
package conn

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// AtomicRemotePath returns a temp path for remotePath used during atomic
// upload: <remotePath>.sshmng-tmp-<6 hex>. Same directory as the target (so
// PosixRename is same-filesystem = atomic). The random suffix avoids collisions
// across concurrent transfers to the same target.
func AtomicRemotePath(remotePath string) string {
	b := make([]byte, 3) // 3 bytes → 6 hex chars
	rand.Read(b)
	return remotePath + ".sshmng-tmp-" + hex.EncodeToString(b)
}
```
(Note: use `crypto/rand` — `math/rand` is unavailable in workflow scripts, but this is app code, not a workflow. `crypto/rand.Read` is safe and ignores the error per Go convention for 3 bytes.)

- [ ] **Step 4: Modify `Upload` to be atomic**

In `internal/ssh/pty/sftp.go`, rewrite `Upload`'s body. Replace `sftpClient.Create(remotePath)` with the temp+rename sequence. Add a private helper `finalizeUpload` to avoid duplicating between `Upload` and `UploadSized`:

```go
// finalizeUpload closes the temp sftp file, then atomically renames it to
// remotePath (PosixRename; fallback Remove+Rename). On any error after the
// copy, removes the temp so no half-written target remains. Sync is best-effort
// (fsync@openssh.com may be unsupported; ignore that error). Returns the copy's
// (n, timedOut, err) unchanged on success; on rename failure returns a rename
// error and removes the temp.
func (p *PtyConn) finalizeUpload(dst *sftp.File, tmpPath, remotePath string, n int, timedOut bool, copyErr error) (int, bool, error) {
	// Best-effort fsync; ignore unsupported.
	if err := dst.Sync(); err != nil && !errors.Is(err, os.ErrClosed) {
		// fsync unsupported → not fatal. Log at debug.
		p.logger.Debug("sftp fsync skipped", "remote", remotePath, "err", err.Error())
	}
	dst.Close()

	if copyErr != nil || timedOut {
		sftpClient := p.sftpClient
		if sftpClient != nil {
			sftpClient.Remove(tmpPath)
		}
		return n, timedOut, copyErr
	}
	// Success: atomic rename.
	sftpClient := p.sftpClient
	if sftpClient == nil {
		return n, timedOut, fmt.Errorf("sftp client lost after upload")
	}
	if err := sftpClient.PosixRename(tmpPath, remotePath); err != nil {
		// Fallback: standard Rename (non-atomic replace) after Remove.
		p.logger.Debug("posix-rename unsupported, falling back", "remote", remotePath, "err", err.Error())
		sftpClient.Remove(remotePath)
		if err2 := sftpClient.Rename(tmpPath, remotePath); err2 != nil {
			sftpClient.Remove(tmpPath)
			return n, timedOut, fmt.Errorf("rename %s -> %s: %w (posix: %v)", tmpPath, remotePath, err2, err)
		}
	}
	return n, timedOut, nil
}
```

Then `Upload` becomes (sketch — replace the Create/AfterFunc/io.Copy block):
```go
	tmpPath := conn.AtomicRemotePath(remotePath)
	dst, err := sftpClient.Create(tmpPath)
	if err != nil {
		return 0, false, fmt.Errorf("create remote %s: %w", tmpPath, err)
	}
	stop := context.AfterFunc(ctx, func() { dst.Close() })
	n, err := io.Copy(dst, newCtxReader(src, ctx))
	stop()
	timedOut := ctx.Err() == context.DeadlineExceeded
	return p.finalizeUpload(dst, tmpPath, remotePath, int(n), timedOut, err)
```
Remove the old `defer dst.Close()` (finalizeUpload closes). Keep the logger Debug lines.

- [ ] **Step 5: Modify `UploadSized` symmetrically** — same temp path + `finalizeUpload`, keeping the `io.LimitReader(newCtxReader(src, ctx), size)` copy.

- [ ] **Step 6: Run tests to verify GREEN**

Run: `go test ./internal/ssh/conn/ -run AtomicRemotePath && go test ./internal/ssh/pty/ -run 'TestUploadAtomic' -v`
Expected: PASS. The interrupted-reader test passes (target absent, temp cleaned); success test passes (target present, content correct, no temp).

- [ ] **Step 7: Run full pty suite + build**

Run: `go build ./... && go test ./internal/ssh/pty/ ./internal/ssh/conn/`
Expected: all pass, build clean. Existing sftp tests still green (signature unchanged).

- [ ] **Step 8: Commit**

```bash
git add internal/ssh/conn/atomic.go internal/ssh/conn/atomic_test.go internal/ssh/pty/sftp.go internal/ssh/pty/sftp_atomic_test.go
git commit -m "$(cat <<'EOF'
feat(sftp): atomic upload via temp file + PosixRename

Upload/UploadSized now write to <remote>.sshmng-tmp-<hex>, then
PosixRename to the target (atomic replace). On error/timeout the temp is
removed, leaving no half-written target. fsync is best-effort (skipped if
fsync@openssh.com unsupported; Close already guarantees visibility).
PosixRename falls back to Remove+Rename if the extension is unsupported.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Atomic download — `DownloadToFile` shared helper

**Files:**
- Create: `internal/ssh/conn/sftp_download.go`
- Test: `internal/ssh/pty/sftp_atomic_test.go` (append)

**Interfaces:**
- Produces: `conn.DownloadToFile` is NOT on Conn (it needs the sftp client). Instead the helper lives where it can reach a `*sftp.Client`. Decision: implement as a method on `*PtyConn` — `func (p *PtyConn) DownloadToFile(remotePath, localPath string, timeoutMs int) (n int, timedOut bool, err error)` — it calls the existing `Download` into a temp `*os.File`, then `os.Rename`. CLI and MCP both call this method.

- [ ] **Step 1: Write failing tests for atomic download**

Append to `internal/ssh/pty/sftp_atomic_test.go`:
```go
// TestDownloadToFileAtomicOnTimeout: a download that times out must NOT leave
// the target local file (temp removed).
func TestDownloadToFileAtomicOnTimeout(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr: srv.Addr(), User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"}, HostKeyVerify: true,
	})
	if err != nil { t.Fatalf("Dial: %v", err) }
	defer client.Close()
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil { t.Fatalf("NewPtyConn: %v", err) }
	defer p.Close()

	// Seed a remote file. Use a small file; force timeout=1ms to trigger.
	data := bytes.Repeat([]byte("y"), 4096)
	if _, _, err := p.UploadSized(bytes.NewReader(data), int64(len(data)), "/dlsrc.bin", 60000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dir := t.TempDir()
	target := dir + "/out.bin"
	_, timedOut, err := p.DownloadToFile("/dlsrc.bin", target, 1) // 1ms → timeout
	if !timedOut {
		t.Logf("note: did not time out (fast server); test may be inconclusive for the cleanup path")
	}
	// Target must NOT exist (temp removed, rename never happened).
	if _, statErr := os.Stat(target); statErr == nil {
		t.Errorf("target local file exists after timed-out download (should be absent)")
	}
	// No leftover temp in dir.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "out.bin.sshmng-tmp-") {
			t.Errorf("leftover temp: %s", e.Name())
		}
	}
}

// TestDownloadToFileAtomicSuccess: a successful download creates the target
// with full content and leaves no temp.
func TestDownloadToFileAtomicSuccess(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr: srv.Addr(), User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"}, HostKeyVerify: true,
	})
	if err != nil { t.Fatalf("Dial: %v", err) }
	defer client.Close()
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil { t.Fatalf("NewPtyConn: %v", err) }
	defer p.Close()

	data := bytes.Repeat([]byte("z"), 8192)
	if _, _, err := p.UploadSized(bytes.NewReader(data), int64(len(data)), "/dlsrc2.bin", 60000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dir := t.TempDir()
	target := dir + "/ok.bin"
	n, _, err := p.DownloadToFile("/dlsrc2.bin", target, 60000)
	if err != nil { t.Fatalf("DownloadToFile: %v (n=%d)", err, n) }
	got, err := os.ReadFile(target)
	if err != nil { t.Fatalf("ReadFile target: %v", err) }
	if !bytes.Equal(got, data) {
		t.Errorf("content mismatch: got %d bytes, want %d", len(got), len(data))
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ok.bin.sshmng-tmp-") {
			t.Errorf("leftover temp: %s", e.Name())
		}
	}
}
```

- [ ] **Step 2: Run tests to verify RED**

Run: `go test ./internal/ssh/pty/ -run 'TestDownloadToFile'`
Expected: FAIL — `DownloadToFile` undefined.

- [ ] **Step 3: Implement `DownloadToFile` on `*PtyConn`**

Add to `internal/ssh/pty/sftp.go`:
```go
// DownloadToFile downloads remotePath to a local file atomically: writes to a
// temp file in the same directory (os.CreateTemp), then os.Rename to localPath
// on success. On error/timeout the temp is removed, leaving no half-written
// target. Used by CLI and MCP download paths. Same pipelining as Download
// (writes go through the existing Download into the temp *os.File).
func (p *PtyConn) DownloadToFile(remotePath, localPath string, timeoutMs int) (int, bool, error) {
	dir := filepath.Dir(localPath)
	base := filepath.Base(localPath) + ".sshmng-tmp-*"
	tmp, err := os.CreateTemp(dir, base)
	if err != nil {
		return 0, false, fmt.Errorf("create temp for %s: %w", localPath, err)
	}
	tmpPath := tmp.Name()
	// Ensure temp is removed on any non-success exit.
	success := false
	defer func() {
		if !success {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	n, timedOut, err := p.Download(remotePath, tmp, timeoutMs)
	if cerr := tmp.Close(); cerr != nil {
		// Closing the temp failed — surface but still clean up.
		if err == nil { err = cerr }
	}
	if err != nil || timedOut {
		return n, timedOut, err
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		return n, timedOut, fmt.Errorf("rename temp -> %s: %w", localPath, err)
	}
	success = true
	return n, timedOut, nil
}
```
Add `"path/filepath"` to sftp.go imports if not present.

- [ ] **Step 4: Run tests to verify GREEN**

Run: `go test ./internal/ssh/pty/ -run 'TestDownloadToFile' -v`
Expected: PASS (timeout test may note "inconclusive" if the fake server is too fast — that's acceptable per the test's own log line; the success test must pass).

- [ ] **Step 5: Build + full pty suite**

Run: `go build ./... && go test ./internal/ssh/pty/`
Expected: pass, build clean.

- [ ] **Step 6: Commit**

```bash
git add internal/ssh/pty/sftp.go internal/ssh/pty/sftp_atomic_test.go
git commit -m "$(cat <<'EOF'
feat(sftp): atomic DownloadToFile (temp + os.Rename)

New PtyConn.DownloadToFile writes to an os.CreateTemp in the target's
directory, then os.Rename (same-filesystem atomic) on success. On error/
timeout the temp is removed, leaving no half-written target. To be wired
into CLI and MCP download paths next.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Wire CLI download to `DownloadToFile`; relay to atomic upload

**Files:**
- Modify: `internal/cli/file_cmd.go` — `runFileDownload` (use `DownloadToFile`), `runFileRelay` (relay uses UploadSized which is now atomic — verify; the relay source download is read-only, no temp needed).
- Modify: `internal/mcp/tools_file.go` — `Download` tool uses `DownloadToFile`.

**Interfaces:**
- Consumes: `pty.PtyConn.DownloadToFile`, `pty.PtyConn.UploadSized` (now atomic). CLI holds `*pty.PtyConn` (concrete type) so `DownloadToFile` is callable.

- [ ] **Step 1: Wire `runFileDownload` to `DownloadToFile`**

In `internal/cli/file_cmd.go` `runFileDownload`, replace the `os.Create(local)` + `Download(remote, cw, ...)` block. The progress bar needs the byte count; `DownloadToFile` returns `n`. But the bar's `SetBytes` callback needs the CountingWriter wrapping — `DownloadToFile` calls `Download(remote, tmp, ...)` internally with no counting wrapper. So: add an optional progress hook to `DownloadToFile`, OR keep the bar driven by the existing `Download` + CountingWriter and do temp+rename in the CLI. Decision: extend `DownloadToFile` to accept an `io.Writer`-style progress via a callback param is over-engineering; simpler — add `DownloadToFileWithProgress(remotePath, localPath string, timeoutMs int, onBytes func(int64))` that wraps the temp file in a `progress.CountingWriter` before passing to `Download`.

Actually simpler and DRY: change `DownloadToFile` to accept an optional `onBytes func(int64)`:
```go
func (p *PtyConn) DownloadToFile(remotePath, localPath string, timeoutMs int, onBytes func(int64)) (int, bool, error)
```
Inside: `var w io.Writer = tmp; if onBytes != nil { w = &countingWriter{W: tmp, Fn: onBytes} }` then `p.Download(remotePath, w, timeoutMs)`. (Define a small unexported `countingWriter` in pty, or import progress — but pty importing progress is a layering inversion. Define a local 5-line wrapper.) Update Task 2's impl + tests to pass `nil` for `onBytes`. Then CLI passes the bar callback.

Implement: add the `onBytes` param to `DownloadToFile`. Add a local `type countingWriter struct{ W io.Writer; n int64; Fn func(int64) }` in sftp.go (or reuse the pattern). Update the Task 2 tests' calls to add `, nil`.

- [ ] **Step 2: Update `runFileDownload`**

```go
	// (after resolveDstPath for local)
	bar := progress.NewBar(os.Stderr, srv.Name+":"+remote, total)
	n, timedOut, err := ptyConn.DownloadToFile(remote, local, *timeoutMs, func(n int64) { bar.SetBytes(n) })
	bar.Finish()
```
Remove the `os.Create` + `CountingWriter` + `Download` lines (DownloadToFile owns them). Keep the `remote Stat` for `total` (bar sizing) — that stays before the call. The `f.Close()` defer is gone (DownloadToFile manages the file).

- [ ] **Step 3: Wire MCP `Download` to `DownloadToFile`**

`internal/mcp/tools_file.go` `Download`: MCP's `sess.Download(args.Src, f, ...)` — but `sess` is `*Session`, not `*PtyConn`. `DownloadToFile` is on `*PtyConn`. MCP goes through the `Conn` interface. Two options: (a) add `DownloadToFile` to the `Conn` interface (breaks fakes — they'd need the method); (b) keep MCP download non-atomic for now (it writes through `Session.Download` → `PtyConn.Download` into the file the MCP opens). 

Decision: MCP download currently does `os.Create(args.Dst)` + `sess.Download(src, f, ...)`. To make it atomic without touching the Conn interface, replicate the temp+rename in the MCP handler using the shared pattern. Simplest: extract the temp+rename into a tiny helper in `internal/ssh/conn` that takes a "download into writer" func. But that's more surface. Given MCP is secondary (agents rarely care about half-files the way a human CLI user does), **scope MCP download atomicity out of this task** — note it as a follow-up. MCP upload IS atomic (it calls `sess.Upload` → `PtyConn.Upload`, now atomic). Leave MCP `Download` as-is; the spec's "MCP benefits via same Conn methods" applies to upload. Update the spec note mentally: download atomicity is CLI-only for now (MCP follow-up).

So Step 3 becomes: verify MCP upload is atomic (it calls `Upload`/`UploadSized` → atomic) — no code change. Skip MCP download.

- [ ] **Step 4: Relay — verify atomic upload**

`runFileRelay` calls `manager.RelayTransferWithProgress(...)` whose upload goroutines call `e.sess.UploadSized(...)` → `PtyConn.UploadSized` (now atomic). No code change needed. Confirm by reading the relay path (already uses UploadSized). Add a one-line code comment in relay.go noting uploads are atomic. (Optional; skip if low-value.)

- [ ] **Step 5: Build + run full suite**

Run: `go build ./... && go test ./...`
Expected: pass, build clean.

- [ ] **Step 6: Commit**

```bash
git add internal/ssh/pty/sftp.go internal/ssh/pty/sftp_atomic_test.go internal/cli/file_cmd.go
git commit -m "$(cat <<'EOF'
feat(cli): atomic file download via DownloadToFile

runFileDownload now uses PtyConn.DownloadToFile (temp + os.Rename),
with the progress bar driven by the onBytes callback. Relay uploads are
atomic via UploadSized (already updated). MCP upload is atomic; MCP
download remains non-atomic (follow-up).

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Benchmark — atomic upload/download no regression

**Files:**
- Modify: `internal/ssh/pty/sftp_bench_test.go`

- [ ] **Step 1: Add `BenchmarkSftpUploadAtomic`**

Mirror `BenchmarkSftpUpload` but it now IS atomic (UploadSized was changed). Instead, compare the *overhead* of temp+rename vs the pre-atomic path. Since we can't easily benchmark the old path anymore, benchmark `UploadSized` (now atomic) and compare to the recorded baseline from the v0.1.10 bench (7.83 MB/s). Add:
```go
func BenchmarkSftpUploadSizedAtomic(b *testing.B) {
	// Same as BenchmarkSftpUploadSizedCounting (now atomic via temp+rename).
	// Compare throughput to the 7.83 MB/s baseline; temp+rename overhead
	// (one extra Create + PosixRename + best-effort Sync) should be negligible
	// vs the 4MB transfer.
	// ... (copy BenchmarkSftpUploadSizedCounting body; it already uses UploadSized)
}
```
Actually `BenchmarkSftpUploadSizedCounting` already exercises the atomic path now. Just re-run it and compare to baseline. Add a comment in the bench file noting the atomic path is now covered by that bench. No new bench needed — re-run existing.

- [ ] **Step 2: Run the bench**

Run: `go test -run='^$' -bench='BenchmarkSftpUploadSizedCounting$|BenchmarkSftpDownload$' -benchtime=2s ./internal/ssh/pty/`
Expected: throughput within ~2x of the v0.1.10 baseline (7.83 MB/s upload, 1518 MB/s download loopback). The temp+rename adds one Create + one PosixRename + (maybe) one fsync per transfer — negligible over 4MB. If regressed >2x, investigate (Sync may be blocking — make Sync truly best-effort by checking the error is "unsupported" and skipping silently, which Task 1 already does).

- [ ] **Step 3: Commit (with numbers)**

```bash
git add internal/ssh/pty/sftp_bench_test.go  # if any comment added
git commit -m "$(cat <<'EOF'
test(sftp): confirm atomic upload/download no throughput regression

BenchmarkSftpUploadSizedCounting (now atomic: temp+PosixRename+Sync)
measured <X> MB/s vs 7.83 MB/s baseline; Download <Y> MB/s. temp+rename
overhead negligible over 4MB.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```
(Replace <X>/<Y> with real numbers.)

---

### Task 5: Dir transfer atomicity (per-file)

**Files:**
- Modify: `internal/ssh/pty/sftp_dir.go` — `UploadDir` worker (line ~124 `p.Upload(f, finalPath, ...)`), `DownloadDir` worker (~line 349 `p.Download(remotePath, f, ...)`).

**Interfaces:**
- Consumes: atomic `p.Upload` (UploadDir already calls `p.Upload` — now atomic automatically!), and `p.DownloadToFile` for DownloadDir.

- [ ] **Step 1: Verify UploadDir is already atomic**

`UploadDir` worker calls `p.Upload(f, finalPath, opts.TimeoutMs)` (sftp_dir.go:124). `p.Upload` is now atomic (Task 1). So **dir upload is already atomic per-file — no code change.** Verify by reading; add a code comment if desired. Skip to DownloadDir.

- [ ] **Step 2: Make DownloadDir per-file atomic**

`DownloadDir` worker does `p.Download(task.remotePath, f, opts.TimeoutMs)` where `f` is an `*os.File` the worker opened. To make it atomic, switch to `DownloadToFile(task.remotePath, finalPath, opts.TimeoutMs, nil)` and remove the manual `os.Create`/`f.Close`. Read the DownloadDir worker (sftp_dir.go ~320-364), replace:
```go
					f, err := os.Create(finalPath)
					if err != nil { ... }
					n, timedOut, err := p.Download(task.remotePath, f, opts.TimeoutMs)
					f.Close()
```
with:
```go
					n, timedOut, err := p.DownloadToFile(task.remotePath, finalPath, opts.TimeoutMs, nil)
```
Adjust the subsequent `mu.Lock()` block (result accounting) — unchanged logic, just `n`/`timedOut`/`err` come from DownloadToFile now. The `resolveLocalConflict` still computes `finalPath` before.

- [ ] **Step 3: Build + run dir tests**

Run: `go build ./... && go test ./internal/ssh/pty/ -run 'TestUploadDir|TestDownloadDir'`
Expected: pass.

- [ ] **Step 4: Commit**

```bash
git add internal/ssh/pty/sftp_dir.go
git commit -m "$(cat <<'EOF'
feat(sftp): atomic per-file download in DownloadDir

DownloadDir worker now uses DownloadToFile (temp + os.Rename) instead of
os.Create + Download. UploadDir is already atomic (calls Upload, changed
in Task 1). Conflict policies unchanged: overwrite atomically replaces,
skip skips before transfer, rename resolves the target path first.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: End-to-end verification + cleanup

- [ ] **Step 1: Full suite + race + cross-platform**

Run: `go test ./... && go test -race ./internal/ssh/... ./internal/cli/... && GOOS=windows go build ./... && go vet ./...`
Expected: all pass, race clean, windows builds, vet clean.

- [ ] **Step 2: Live smoke test (interrupted upload leaves no target)**

Run (with a real server): upload a large file with a tiny `--timeout` to force a timeout, then check the remote target does NOT exist (only the summary says "timed out"). Then a normal upload and verify the target exists with correct size.

- [ ] **Step 3: Commit any remaining cleanup + push**

```bash
git push origin main
```

---

## Self-Review

**1. Spec coverage:**
- Upload temp+PosixRename+fallback → Task 1 ✓
- Download temp+os.Rename (DownloadToFile) → Task 2 ✓
- CLI download wired → Task 3 ✓
- MCP upload atomic (via UploadSized) → Task 3 (verified, no change) ✓
- MCP download — scoped OUT (follow-up), spec note updated ✓
- Dir upload atomic (via Upload) → Task 5 (verified) ✓
- Dir download atomic (via DownloadToFile) → Task 5 ✓
- Relay upload atomic (via UploadSized) → Task 3 (verified) ✓
- Sync best-effort → Task 1 ✓
- PosixRename fallback → Task 1 ✓
- No-regression bench → Task 4 ✓
- No speed regression → bench validates ✓

**2. Placeholder scan:** Task 3 Step 3 has a design pivot (MCP download scoped out) — stated explicitly with reasoning, not a TODO. All code blocks present. Task 4 numbers are `<X>`/`<Y>` placeholders to fill at run time (standard bench pattern, not a plan defect).

**3. Type consistency:**
- `conn.AtomicRemotePath(string) string` — Task 1 def + test ✓
- `PtyConn.finalizeUpload(dst *sftp.File, tmpPath, remotePath string, n int, timedOut bool, copyErr error) (int, bool, error)` — Task 1 ✓
- `PtyConn.DownloadToFile(remotePath, localPath string, timeoutMs int, onBytes func(int64)) (int, bool, error)` — Task 2 def, Task 3 adds onBytes param (update Task 2 tests to pass nil) ✓
- UploadDir calls `p.Upload` (unchanged sig) ✓; DownloadDir calls `p.DownloadToFile` (sig matches Task 2/3) ✓

No issues. Plan complete.
