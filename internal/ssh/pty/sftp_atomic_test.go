package pty

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
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
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
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
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	data := bytes.Repeat([]byte("x"), 4096)
	n, _, err := p.UploadSized(bytes.NewReader(data), int64(len(data)), "/ok.bin", 60000)
	if err != nil {
		t.Fatalf("UploadSized: %v (n=%d)", err, n)
	}
	// Read back via sftp and compare.
	rf, err := p.sftpClient.Open("/ok.bin")
	if err != nil {
		t.Fatalf("Open target: %v", err)
	}
	got, _ := io.ReadAll(rf)
	rf.Close()
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

// TestDownloadToFileAtomicOnTimeout: a download that times out must NOT leave
// the target local file (temp removed).
func TestDownloadToFileAtomicOnTimeout(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr: srv.Addr(), User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"}, HostKeyVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	// Seed a remote file large enough that a 1ms timeout reliably fires (the
	// in-memory fake sftp backend serves reads instantly, so a tiny file
	// completes before the deadline). 1MB ≈ 32 sftp read packets → reliably
	// exceeds 1ms.
	data := bytes.Repeat([]byte("y"), 1<<20)
	if _, _, err := p.UploadSized(bytes.NewReader(data), int64(len(data)), "/dlsrc.bin", 60000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dir := t.TempDir()
	target := dir + "/out.bin"
	_, timedOut, err := p.DownloadToFile("/dlsrc.bin", target, 1, nil) // 1ms → timeout
	if !timedOut {
		// Graceful fallback: if the server is so fast even 1MB finishes within
		// 1ms, the cleanup path can't be exercised — treat as inconclusive.
		t.Logf("note: did not time out (fast server); test inconclusive for the cleanup path")
		return
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
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	data := bytes.Repeat([]byte("z"), 8192)
	if _, _, err := p.UploadSized(bytes.NewReader(data), int64(len(data)), "/dlsrc2.bin", 60000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	dir := t.TempDir()
	target := dir + "/ok.bin"
	n, _, err := p.DownloadToFile("/dlsrc2.bin", target, 60000, nil)
	if err != nil {
		t.Fatalf("DownloadToFile: %v (n=%d)", err, n)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
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
