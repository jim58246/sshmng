package pty

import (
	"bytes"
	"io"
	"os"
	"testing"

	"sshmng/internal/config"
	"sshmng/internal/ssh/conn"
)

// BenchmarkSftpUpload 测 sftp 上传吞吐量，用 *os.File 作为 src（真实场景：
// MCP 工具层 tools_file.go 用 os.Open 打开本地文件后传给 Session.Upload）。
//
// 关键：*os.File 暴露 Stat()，*sftp.File.ReadFrom 在 useConcurrentWrites=true 时
// 通过 type switch 识别 Stat() 走 readFromWithConcurrency 并发 pipelining 路径。
// 若 src 是 bytes.Reader（有 Len 无 Stat）或被 wrapper 隐藏 Stat，ReadFrom 退化为
// 串行 writeChunkAt 循环，速度慢一个数量级。
//
// 注：fake sftp server 走 loopback，RTT 极低，提速比例不反映真实跨地域场景。
// 此 benchmark 主要防回归——确保 *os.File 路径仍走 pipelining。
func BenchmarkSftpUpload(b *testing.B) {
	srv := newFakeShellServerWithSftp(b)
	d := newDialerWithTempKnownHosts(b)
	client, err := d.Dial(conn.DialOptions{
		Addr:          srv.Addr(),
		User:          "alice",
		Auth:          config.SSHAuth{Password: "wonderland"},
		HostKeyVerify: true,
	})
	if err != nil {
		b.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		b.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	data := bytes.Repeat([]byte("x"), 4<<20) // 4MB
	tmp, err := os.CreateTemp("", "bench-upload")
	if err != nil {
		b.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		b.Fatalf("Write tmp: %v", err)
	}
	if err := tmp.Close(); err != nil {
		b.Fatalf("Close tmp: %v", err)
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		f, err := os.Open(tmp.Name())
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		n, _, err := p.Upload(f, "/bench.txt", 60000)
		f.Close()
		if err != nil {
			b.Fatalf("Upload: %v (bytes=%d)", err, n)
		}
	}
}

// BenchmarkSftpUploadBytesReader 用 bytes.Reader 作为 src（有 Len 无 Stat），
// 作为对照组——ReadFrom 退化为串行路径。预期比 *os.File 慢一个数量级。
func BenchmarkSftpUploadBytesReader(b *testing.B) {
	srv := newFakeShellServerWithSftp(b)
	d := newDialerWithTempKnownHosts(b)
	client, err := d.Dial(conn.DialOptions{
		Addr:          srv.Addr(),
		User:          "alice",
		Auth:          config.SSHAuth{Password: "wonderland"},
		HostKeyVerify: true,
	})
	if err != nil {
		b.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		b.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	data := bytes.Repeat([]byte("x"), 4<<20) // 4MB
	b.ResetTimer()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		n, _, err := p.Upload(bytes.NewReader(data), "/bench.txt", 60000)
		if err != nil {
			b.Fatalf("Upload: %v (bytes=%d)", err, n)
		}
	}
}

// BenchmarkSftpDownload 测 sftp 下载吞吐量。
func BenchmarkSftpDownload(b *testing.B) {
	srv := newFakeShellServerWithSftp(b)
	d := newDialerWithTempKnownHosts(b)
	client, err := d.Dial(conn.DialOptions{
		Addr:          srv.Addr(),
		User:          "alice",
		Auth:          config.SSHAuth{Password: "wonderland"},
		HostKeyVerify: true,
	})
	if err != nil {
		b.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		b.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	data := bytes.Repeat([]byte("y"), 4<<20) // 4MB
	if _, _, err := p.Upload(bytes.NewReader(data), "/bench_dl.txt", 60000); err != nil {
		b.Fatalf("seed Upload: %v", err)
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		n, _, err := p.Download("/bench_dl.txt", io.Discard, 60000)
		if err != nil {
			b.Fatalf("Download: %v (bytes=%d)", err, n)
		}
	}
}
