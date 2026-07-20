package ssh

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"sshmng/internal/config"
)

// --- sftp 通道建立 ---

// TestSftpEstablishedAtLogin: 支持 sftp subsystem 的 server 上，OpenPtyConn 后 SftpAvailable()=true。
func TestSftpEstablishedAtLogin(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	p, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()
	if !p.SftpAvailable() {
		t.Errorf("SftpAvailable() = false, want true (server supports sftp)")
	}
}

// TestSftpUnavailableWhenSubsystemRejected: server 拒绝 sftp subsystem 时，SftpAvailable()=false。
func TestSftpUnavailableWhenSubsystemRejected(t *testing.T) {
	srv := newFakeShellServer(t) // 默认 enableSftp=false
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	p, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()
	if p.SftpAvailable() {
		t.Errorf("SftpAvailable() = true, want false (server rejects sftp)")
	}
}

// --- Upload ---

// TestUploadNormalPath: 小文件上传后返回字节数正确，远端内容与本地一致。
func TestUploadNormalPath(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	p, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	content := bytes.Repeat([]byte("hello sftp\n"), 100) // ~1100 bytes
	n, timedOut, err := p.Upload(bytes.NewReader(content), "/remote.txt", 30000)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if timedOut {
		t.Errorf("timed_out = true, want false")
	}
	if n != len(content) {
		t.Errorf("bytes = %d, want %d", n, len(content))
	}

	// 读回远端文件验证内容
	remote, err := p.sftpClient.Open("/remote.txt")
	if err != nil {
		t.Fatalf("Open remote: %v", err)
	}
	defer remote.Close()
	got, err := io.ReadAll(remote)
	if err != nil {
		t.Fatalf("ReadAll remote: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("remote content mismatch: got %d bytes, want %d", len(got), len(content))
	}
}

// TestUploadSftpUnavailable: sftp 通道未建立时 Upload 返回 "sftp not available" 错误。
func TestUploadSftpUnavailable(t *testing.T) {
	srv := newFakeShellServer(t) // 不支持 sftp
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	p, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	_, _, err = p.Upload(strings.NewReader("data"), "/remote.txt", 5000)
	if err == nil {
		t.Errorf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "sftp not available") {
		t.Errorf("err should mention 'sftp not available', got: %v", err)
	}
}

// TestUploadTimeout: 慢 reader + 小超时 → timed_out=true，bytes > 0。
func TestUploadTimeout(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	p, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	// 1MB 数据 + 每次 Read sleep 20ms → 总时长 ~640ms；设 100ms 超时
	data := bytes.Repeat([]byte("x"), 1<<20)
	src := newSlowReader(bytes.NewReader(data), 20*time.Millisecond, 32*1024)
	n, timedOut, err := p.Upload(src, "/slow.txt", 100)
	if !timedOut {
		t.Errorf("timed_out = false, want true (err=%v bytes=%d)", err, n)
	}
	if n <= 0 {
		t.Errorf("bytes = %d, want > 0 (should have partial upload)", n)
	}
}

// --- Download ---

// TestDownloadNormalPath: 先上传再下载，内容一致。
func TestDownloadNormalPath(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	p, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	content := bytes.Repeat([]byte("download me\n"), 100) // ~1200 bytes
	if _, _, err := p.Upload(bytes.NewReader(content), "/dl.txt", 30000); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	var dst bytes.Buffer
	n, timedOut, err := p.Download("/dl.txt", &dst, 30000)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if timedOut {
		t.Errorf("timed_out = true, want false")
	}
	if n != len(content) {
		t.Errorf("bytes = %d, want %d", n, len(content))
	}
	if !bytes.Equal(dst.Bytes(), content) {
		t.Errorf("downloaded content mismatch: got %d bytes, want %d", dst.Len(), len(content))
	}
}

// TestDownloadSftpUnavailable: sftp 通道未建立时 Download 返回 "sftp not available" 错误。
func TestDownloadSftpUnavailable(t *testing.T) {
	srv := newFakeShellServer(t) // 不支持 sftp
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	p, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	var dst bytes.Buffer
	_, _, err = p.Download("/remote.txt", &dst, 5000)
	if err == nil {
		t.Errorf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "sftp not available") {
		t.Errorf("err should mention 'sftp not available', got: %v", err)
	}
}

// TestDownloadTimeout: 大文件 + 小超时 + 慢 writer → timed_out=true，bytes > 0。
func TestDownloadTimeout(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(DialOptions{
		Addr: srv.Addr(),
		User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := RandomSID()
	p, err := NewPtyConn(client, sid, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	// 先上传 1MB 数据
	content := bytes.Repeat([]byte("y"), 1<<20)
	if _, _, err := p.Upload(bytes.NewReader(content), "/big.txt", 30000); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// 慢 writer：每次 Write sleep 20ms；设 100ms 超时
	dst := newSlowWriter(&bytes.Buffer{}, 20*time.Millisecond, 32*1024)
	n, timedOut, err := p.Download("/big.txt", dst, 100)
	if !timedOut {
		t.Errorf("timed_out = false, want true (err=%v bytes=%d)", err, n)
	}
	if n <= 0 {
		t.Errorf("bytes = %d, want > 0 (should have partial download)", n)
	}
}

// --- 辅助 ---

// slowReader 在每次 Read 之前 sleep，用于模拟慢读源。
type slowReader struct {
	r       io.Reader
	delay   time.Duration
	chunkSz int
}

func newSlowReader(r io.Reader, delay time.Duration, chunkSz int) *slowReader {
	return &slowReader{r: r, delay: delay, chunkSz: chunkSz}
}

func (s *slowReader) Read(p []byte) (int, error) {
	if len(p) > s.chunkSz {
		p = p[:s.chunkSz]
	}
	time.Sleep(s.delay)
	return s.r.Read(p)
}

// slowWriter 在每次 Write 之前 sleep，用于模拟慢写目标。
type slowWriter struct {
	w       io.Writer
	delay   time.Duration
	chunkSz int
}

func newSlowWriter(w io.Writer, delay time.Duration, chunkSz int) *slowWriter {
	return &slowWriter{w: w, delay: delay, chunkSz: chunkSz}
}

func (s *slowWriter) Write(p []byte) (int, error) {
	if len(p) > s.chunkSz {
		p = p[:s.chunkSz]
	}
	time.Sleep(s.delay)
	return s.w.Write(p)
}
