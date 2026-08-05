package mcp

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/jim58246/sshmng/internal/ssh/conn"
	"github.com/jim58246/sshmng/internal/ssh/session"
)

// newRelayService 建一个带空 Manager 的 Service。fakeConn 在 session 包内部未导出，
// 跨包无法装配真实 sftp session；本测试覆盖 handler 的硬错误分支（src 不存在 + 空 dsts），
// 端到端流式断言在 session.TestRelayTransferOneToOne。
func newRelayService() *Service {
	mgr := session.NewManager()
	return &Service{manager: mgr, baseLogger: nil}
}

// stubConn 是 session.Conn 的最小桩，仅供注册 session 让 manager.Get 通过。
// 方法不会被调用到——这些测试只触 handler 的硬错误分支（在 Conn 方法前就返回）。
type stubConn struct{}

func (stubConn) Close() error { return nil }
func (stubConn) Run(string, int, int) (string, string, int, bool, bool, bool, int, bool, error) {
	return "", "", 0, false, false, false, 0, false, nil
}
func (stubConn) SftpAvailable() bool                              { return false }
func (stubConn) Upload(io.Reader, string, int) (int, bool, error) { return 0, false, nil }
func (stubConn) UploadSized(io.Reader, int64, string, int) (int, bool, error) {
	return 0, false, nil
}
func (stubConn) Download(string, io.Writer, int) (int, bool, error) { return 0, false, nil }
func (stubConn) Stat(string) (os.FileInfo, error)                   { return nil, nil }
func (stubConn) UploadDir(string, string, conn.DirTransferOptions) (conn.DirTransferResult, error) {
	return conn.DirTransferResult{}, nil
}
func (stubConn) DownloadDir(string, string, conn.DirTransferOptions) (conn.DirTransferResult, error) {
	return conn.DirTransferResult{}, nil
}

// TestRelayTransferSrcNotFoundIsError: 源 session 不存在 → hard error → IsError=true。
func TestRelayTransferSrcNotFoundIsError(t *testing.T) {
	svc := newRelayService()
	res, _, err := svc.RelayTransfer(context.Background(), nil, RelayTransferArgs{
		SrcSid: "nope", SrcPath: "/s", DstSids: []string{"d"}, DstPath: "/d",
	})
	if err != nil {
		t.Fatalf("Go error: %v", err)
	}
	if !res.IsError {
		t.Errorf("IsError = false, want true (src not found)")
	}
}

// TestRelayTransferEmptyDstsIsError: 源 session 存在但 dst_sids 为空 → hard error → IsError=true。
// 这真正走到 manager.RelayTransfer 的 "no relay destinations" 分支（源已注册，m.Get 通过）。
// 端到端流式行为（空 dsts 在 session 层的返回）另由 session.TestRelayTransferEmptyDsts 覆盖。
func TestRelayTransferEmptyDstsIsError(t *testing.T) {
	svc := newRelayService()
	// 注册一个真实源 session（stubConn 不会被调用——空 dsts 在 Conn 方法前就返回）
	svc.manager.NewSession("src", "srcsrv", stubConn{}, 0, nil)
	res, _, err := svc.RelayTransfer(context.Background(), nil, RelayTransferArgs{
		SrcSid: "src", SrcPath: "/s", DstSids: nil, DstPath: "/d",
	})
	if err != nil {
		t.Fatalf("Go error: %v", err)
	}
	if !res.IsError {
		t.Errorf("IsError = false, want true (empty dsts)")
	}
}
