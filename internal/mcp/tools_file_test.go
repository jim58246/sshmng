package mcp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sshmng/internal/config"
	"sshmng/internal/ssh/conn"
)

// helper: 登录到 fake sftp server，返回 sid + svc + 清理函数。
func loginToSftpServer(t *testing.T) (*Service, string) {
	t.Helper()
	srv := newFakeShellServerWithSftpForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	store.Save(&config.Config{
		Version: "1",
		Servers: []*config.SSHServer{
			{Name: "s", Addr: srv.Addr(), User: "alice", Auth: config.SSHAuth{Password: "wonderland"}},
		},
	})
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)
	loginRes, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "s"})
	if loginRes.IsError {
		t.Fatalf("login failed: %s", resultText(t, loginRes))
	}
	sid := parseJSON(t, resultText(t, loginRes)).(map[string]any)["sid"].(string)
	return svc, sid
}

// helper: 登录到 fake 非 sftp server（用于 sftp 不可用场景）。
func loginToNoSftpServer(t *testing.T) (*Service, string) {
	t.Helper()
	srv := newFakeShellServerForMCP(t) // 不支持 sftp
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	store.Save(&config.Config{
		Version: "1",
		Servers: []*config.SSHServer{
			{Name: "s", Addr: srv.Addr(), User: "alice", Auth: config.SSHAuth{Password: "wonderland"}},
		},
	})
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)
	loginRes, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "s"})
	if loginRes.IsError {
		t.Fatalf("login failed: %s", resultText(t, loginRes))
	}
	sid := parseJSON(t, resultText(t, loginRes)).(map[string]any)["sid"].(string)
	return svc, sid
}

// --- upload ---

// TestUploadUnknownSID: upload 对未知 sid 报错。
func TestUploadUnknownSID(t *testing.T) {
	svc := newTestService(t, &config.Config{Version: "1"})
	res, _, _ := svc.Upload(context.Background(), &mcp.CallToolRequest{}, UploadArgs{
		SID: "nope", Src: "/tmp/x", Dst: "/remote/x",
	})
	if !res.IsError {
		t.Errorf("expected IsError=true for unknown sid")
	}
}

// TestUploadNormalPath: upload 正常路径，返回字节数 + ok=true。
func TestUploadNormalPath(t *testing.T) {
	svc, sid := loginToSftpServer(t)
	defer svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	dir := t.TempDir()
	content := []byte("upload via mcp\n")
	localPath := filepath.Join(dir, "local.txt")
	if err := os.WriteFile(localPath, content, 0600); err != nil {
		t.Fatalf("write local: %v", err)
	}
	res, _, _ := svc.Upload(context.Background(), &mcp.CallToolRequest{}, UploadArgs{
		SID: sid, Src: localPath, Dst: "/mcp.txt",
	})
	if res.IsError {
		t.Fatalf("Upload failed: %s", resultText(t, res))
	}
	r := parseJSON(t, resultText(t, res)).(map[string]any)
	if r["ok"] != true {
		t.Errorf("ok = %v, want true", r["ok"])
	}
	if r["timed_out"] != false {
		t.Errorf("timed_out = %v, want false", r["timed_out"])
	}
	if int(r["bytes"].(float64)) != len(content) {
		t.Errorf("bytes = %v, want %d", r["bytes"], len(content))
	}
}

// TestUploadSftpUnavailable: sftp 不可用时 upload 报错。
func TestUploadSftpUnavailable(t *testing.T) {
	svc, sid := loginToNoSftpServer(t)
	defer svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	dir := t.TempDir()
	localPath := filepath.Join(dir, "local.txt")
	if err := os.WriteFile(localPath, []byte("data"), 0600); err != nil {
		t.Fatalf("write local: %v", err)
	}
	res, _, _ := svc.Upload(context.Background(), &mcp.CallToolRequest{}, UploadArgs{
		SID: sid, Src: localPath, Dst: "/remote/x",
	})
	if !res.IsError {
		t.Errorf("expected IsError=true when sftp unavailable")
	}
	if msg := resultText(t, res); !strings.Contains(msg, "sftp not available") {
		t.Errorf("err should mention 'sftp not available', got: %s", msg)
	}
}

// TestUploadLocalFileMissing: 本地文件不存在时 upload 报错。
func TestUploadLocalFileMissing(t *testing.T) {
	svc, sid := loginToSftpServer(t)
	defer svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	res, _, _ := svc.Upload(context.Background(), &mcp.CallToolRequest{}, UploadArgs{
		SID: sid, Src: "/nonexistent/local/path.txt", Dst: "/remote/x",
	})
	if !res.IsError {
		t.Errorf("expected IsError=true for missing local file")
	}
}

// --- download ---

// TestDownloadUnknownSID: download 对未知 sid 报错。
func TestDownloadUnknownSID(t *testing.T) {
	svc := newTestService(t, &config.Config{Version: "1"})
	res, _, _ := svc.Download(context.Background(), &mcp.CallToolRequest{}, DownloadArgs{
		SID: "nope", Src: "/remote/x", Dst: "/tmp/x",
	})
	if !res.IsError {
		t.Errorf("expected IsError=true for unknown sid")
	}
}

// TestDownloadNormalPath: upload 后 download，本地内容与原内容一致。
func TestDownloadNormalPath(t *testing.T) {
	svc, sid := loginToSftpServer(t)
	defer svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	// 先 upload
	dir := t.TempDir()
	content := []byte("download via mcp\n")
	localUpload := filepath.Join(dir, "up.txt")
	if err := os.WriteFile(localUpload, content, 0600); err != nil {
		t.Fatalf("write local: %v", err)
	}
	upRes, _, _ := svc.Upload(context.Background(), &mcp.CallToolRequest{}, UploadArgs{
		SID: sid, Src: localUpload, Dst: "/dl.txt",
	})
	if upRes.IsError {
		t.Fatalf("Upload failed: %s", resultText(t, upRes))
	}

	// 再 download 到本地
	localDownload := filepath.Join(dir, "down.txt")
	dlRes, _, _ := svc.Download(context.Background(), &mcp.CallToolRequest{}, DownloadArgs{
		SID: sid, Src: "/dl.txt", Dst: localDownload,
	})
	if dlRes.IsError {
		t.Fatalf("Download failed: %s", resultText(t, dlRes))
	}
	r := parseJSON(t, resultText(t, dlRes)).(map[string]any)
	if r["ok"] != true {
		t.Errorf("ok = %v, want true", r["ok"])
	}
	if int(r["bytes"].(float64)) != len(content) {
		t.Errorf("bytes = %v, want %d", r["bytes"], len(content))
	}

	got, err := os.ReadFile(localDownload)
	if err != nil {
		t.Fatalf("read downloaded: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("downloaded content mismatch: got %q, want %q", got, content)
	}
}

// TestDownloadSftpUnavailable: sftp 不可用时 download 报错。
func TestDownloadSftpUnavailable(t *testing.T) {
	svc, sid := loginToNoSftpServer(t)
	defer svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	dir := t.TempDir()
	res, _, _ := svc.Download(context.Background(), &mcp.CallToolRequest{}, DownloadArgs{
		SID: sid, Src: "/remote/x", Dst: filepath.Join(dir, "out.txt"),
	})
	if !res.IsError {
		t.Errorf("expected IsError=true when sftp unavailable")
	}
	if msg := resultText(t, res); !strings.Contains(msg, "sftp not available") {
		t.Errorf("err should mention 'sftp not available', got: %s", msg)
	}
}

// TestDownloadRemoteFileMissing: 远端文件不存在时 download 报错。
func TestDownloadRemoteFileMissing(t *testing.T) {
	svc, sid := loginToSftpServer(t)
	defer svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	dir := t.TempDir()
	res, _, _ := svc.Download(context.Background(), &mcp.CallToolRequest{}, DownloadArgs{
		SID: sid, Src: "/nonexistent/remote.txt", Dst: filepath.Join(dir, "out.txt"),
	})
	if !res.IsError {
		t.Errorf("expected IsError=true for missing remote file")
	}
}

// --- stat 反映 sftp_available ---

// TestStatReflectsSftpAvailable: sftp 可用时 stat 返回 sftp_available=true。
func TestStatReflectsSftpAvailable(t *testing.T) {
	svc, sid := loginToSftpServer(t)
	defer svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	res, _, _ := svc.Stat(context.Background(), &mcp.CallToolRequest{}, StatArgs{})
	stats := parseJSON(t, resultText(t, res)).([]any)
	if len(stats) != 1 {
		t.Fatalf("got %d sessions, want 1", len(stats))
	}
	stat := stats[0].(map[string]any)
	if stat["sftp_available"] != true {
		t.Errorf("sftp_available = %v, want true", stat["sftp_available"])
	}
	if stat["sid"] != sid {
		t.Errorf("sid = %v, want %s", stat["sid"], sid)
	}
}

// TestStatReflectsSftpUnavailable: sftp 不可用时 stat 返回 sftp_available=false。
func TestStatReflectsSftpUnavailable(t *testing.T) {
	svc, sid := loginToNoSftpServer(t)
	defer svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	res, _, _ := svc.Stat(context.Background(), &mcp.CallToolRequest{}, StatArgs{})
	stats := parseJSON(t, resultText(t, res)).([]any)
	if len(stats) != 1 {
		t.Fatalf("got %d sessions, want 1", len(stats))
	}
	stat := stats[0].(map[string]any)
	if stat["sftp_available"] != false {
		t.Errorf("sftp_available = %v, want false", stat["sftp_available"])
	}
}

// --- login 返回 sftp_available 字段 ---

// TestLoginReturnsSftpAvailable: login 返回的 sftp_available 字段反映实际状态。
func TestLoginReturnsSftpAvailable(t *testing.T) {
	srv := newFakeShellServerWithSftpForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	store.Save(&config.Config{
		Version: "1",
		Servers: []*config.SSHServer{
			{Name: "s", Addr: srv.Addr(), User: "alice", Auth: config.SSHAuth{Password: "wonderland"}},
		},
	})
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	res, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "s"})
	if res.IsError {
		t.Fatalf("login failed: %s", resultText(t, res))
	}
	r := parseJSON(t, resultText(t, res)).(map[string]any)
	sid := r["sid"].(string)
	defer svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	if r["sftp_available"] != true {
		t.Errorf("sftp_available = %v, want true", r["sftp_available"])
	}
}

// TestLoginReturnsSftpUnavailable: 无 sftp server 上 login 返回 sftp_available=false。
func TestLoginReturnsSftpUnavailable(t *testing.T) {
	srv := newFakeShellServerForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	store.Save(&config.Config{
		Version: "1",
		Servers: []*config.SSHServer{
			{Name: "s", Addr: srv.Addr(), User: "alice", Auth: config.SSHAuth{Password: "wonderland"}},
		},
	})
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	res, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "s"})
	if res.IsError {
		t.Fatalf("login failed: %s", resultText(t, res))
	}
	r := parseJSON(t, resultText(t, res)).(map[string]any)
	sid := r["sid"].(string)
	defer svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	if r["sftp_available"] != false {
		t.Errorf("sftp_available = %v, want false", r["sftp_available"])
	}
}
