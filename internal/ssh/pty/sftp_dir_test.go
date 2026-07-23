package pty

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"sshmng/internal/config"
	"sshmng/internal/ssh/conn"
)

// readAll 是 io.ReadAll 的薄封装，供并发测试读回远端文件内容使用。
func readAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

// TestUploadDirBasic: 2 文件 + 1 子目录的本地树上传到 remote，验证：
// - 目录被创建（MkdirAll）
// - 文件内容正确
// - DirTransferResult.Files/Bytes 正确
func TestUploadDirBasic(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr:          srv.Addr(),
		User:          "alice",
		Auth:          config.SSHAuth{Password: "wonderland"},
		HostKeyVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	// 本地树：localRoot/a.txt, localRoot/sub/b.txt
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "a.txt"), []byte("aaa"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(localRoot, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "sub", "b.txt"), []byte("bbbbb"), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := p.UploadDir(localRoot, "/uploaddir", conn.DirTransferOptions{})
	if err != nil {
		t.Fatalf("UploadDir: %v", err)
	}
	if res.Files != 2 {
		t.Errorf("Files = %d, want 2", res.Files)
	}
	if res.Bytes != 8 {
		t.Errorf("Bytes = %d, want 8", res.Bytes)
	}

	// 验证远端文件内容
	for _, c := range []struct{ path, want string }{
		{"/uploaddir/a.txt", "aaa"},
		{"/uploaddir/sub/b.txt", "bbbbb"},
	} {
		f, err := p.sftpClient.Open(c.path)
		if err != nil {
			t.Fatalf("Open %s: %v", c.path, err)
		}
		got, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatalf("Read %s: %v", c.path, err)
		}
		if !bytes.Equal(got, []byte(c.want)) {
			t.Errorf("%s = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestUploadDirEmptyLocalDir: 空本地目录 → 创建空远端目录，0 文件
func TestUploadDirEmptyLocalDir(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr: srv.Addr(), User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"}, HostKeyVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	localRoot := t.TempDir() // 空目录

	res, err := p.UploadDir(localRoot, "/emptydir", conn.DirTransferOptions{})
	if err != nil {
		t.Fatalf("UploadDir: %v", err)
	}
	if res.Files != 0 {
		t.Errorf("Files = %d, want 0", res.Files)
	}

	// 远端目录应存在
	info, err := p.sftpClient.Stat("/emptydir")
	if err != nil {
		t.Errorf("Stat /emptydir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("/emptydir not a dir")
	}
}

// TestUploadDirLocalNotExist: 本地目录不存在 → error
func TestUploadDirLocalNotExist(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr: srv.Addr(), User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"}, HostKeyVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	_, err = p.UploadDir("/nonexistent/local/path", "/uploaddir", conn.DirTransferOptions{})
	if err == nil {
		t.Errorf("UploadDir with non-existent local dir: err=nil, want error")
	}
}

// TestUploadDirConflictSkip: 目标已存在 → 跳过，Skipped=1，Files=0
func TestUploadDirConflictSkip(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr: srv.Addr(), User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"}, HostKeyVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	// 先在远端预存 a.txt（旧内容）
	if err := p.sftpClient.MkdirAll("/skipdir"); err != nil {
		t.Fatal(err)
	}
	dst, err := p.sftpClient.Create("/skipdir/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Write([]byte("OLD")); err != nil {
		t.Fatal(err)
	}
	dst.Close()

	// 本地 a.txt（新内容）
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "a.txt"), []byte("NEW"), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := p.UploadDir(localRoot, "/skipdir", conn.DirTransferOptions{Conflict: conn.ConflictSkip})
	if err != nil {
		t.Fatalf("UploadDir: %v", err)
	}
	if res.Skipped != 1 || res.Files != 0 {
		t.Errorf("Skipped=%d Files=%d, want 1/0", res.Skipped, res.Files)
	}

	// 验证远端仍是旧内容
	f, err := p.sftpClient.Open("/skipdir/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != "OLD" {
		t.Errorf("remote = %q, want OLD (skip should not overwrite)", got)
	}
}

// TestUploadDirConflictRename: 目标已存在 → 重命名 a.txt → a_1.txt
func TestUploadDirConflictRename(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr: srv.Addr(), User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"}, HostKeyVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	// 远端预存 a.txt
	if err := p.sftpClient.MkdirAll("/renamedir"); err != nil {
		t.Fatal(err)
	}
	dst, _ := p.sftpClient.Create("/renamedir/a.txt")
	dst.Write([]byte("OLD"))
	dst.Close()

	// 本地 a.txt
	localRoot := t.TempDir()
	os.WriteFile(filepath.Join(localRoot, "a.txt"), []byte("NEW"), 0644)

	res, err := p.UploadDir(localRoot, "/renamedir", conn.DirTransferOptions{Conflict: conn.ConflictRename})
	if err != nil {
		t.Fatalf("UploadDir: %v", err)
	}
	if res.Renamed != 1 || res.Files != 1 {
		t.Errorf("Renamed=%d Files=%d, want 1/1", res.Renamed, res.Files)
	}

	// 验证远端：a.txt 仍是 OLD，a_1.txt 是 NEW
	for _, c := range []struct{ path, want string }{
		{"/renamedir/a.txt", "OLD"},
		{"/renamedir/a_1.txt", "NEW"},
	} {
		f, err := p.sftpClient.Open(c.path)
		if err != nil {
			t.Fatalf("Open %s: %v", c.path, err)
		}
		got, _ := io.ReadAll(f)
		f.Close()
		if string(got) != c.want {
			t.Errorf("%s = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestUploadDirConcurrency: 10 文件 + Concurrency=4，验证所有文件都正确传输。
// 不直接测并发（难测），但测并发下不丢文件、不内容错乱。
func TestUploadDirConcurrency(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr: srv.Addr(), User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"}, HostKeyVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	// 本地树：10 文件，每个 1KB
	localRoot := t.TempDir()
	wantFiles := map[string]string{}
	for i := 0; i < 10; i++ {
		name := "file_" + string(rune('0'+i)) + ".txt"
		content := string(bytes.Repeat([]byte{byte('a' + i)}, 1024))
		if err := os.WriteFile(filepath.Join(localRoot, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		wantFiles["/concurrent/"+name] = content
	}

	res, err := p.UploadDir(localRoot, "/concurrent", conn.DirTransferOptions{Concurrency: 4})
	if err != nil {
		t.Fatalf("UploadDir: %v", err)
	}
	if res.Files != 10 {
		t.Errorf("Files = %d, want 10", res.Files)
	}
	if res.Bytes != 10*1024 {
		t.Errorf("Bytes = %d, want %d", res.Bytes, 10*1024)
	}

	// 验证每个文件内容
	for remotePath, want := range wantFiles {
		f, err := p.sftpClient.Open(remotePath)
		if err != nil {
			t.Errorf("Open %s: %v", remotePath, err)
			continue
		}
		got, _ := readAll(f)
		f.Close()
		if string(got) != want {
			t.Errorf("%s content mismatch: got %d bytes, want %d", remotePath, len(got), len(want))
		}
	}
}
