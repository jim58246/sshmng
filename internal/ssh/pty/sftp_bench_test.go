package pty

import (
	"bytes"
	"io"
	"testing"

	"sshmng/internal/config"
	"sshmng/internal/ssh/conn"
)

// BenchmarkSftpUpload 测 sftp 上传吞吐量。
// 修复前（copyCtx 串行 + 32KB 包）：~640 KB/s @ 50ms RTT
// 修复后（io.Copy pipelining + 64KB 包）：接近带宽上限
//
// 注：fake sftp server 走 loopback，RTT 极低，提速比例不反映真实跨地域场景。
// 此 benchmark 主要防回归——确保改 pipelining 后没引入新的同步点。
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
