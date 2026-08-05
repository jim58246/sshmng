package mcp

import (
	"context"
	"testing"

	"github.com/jim58246/sshmng/internal/ssh/session"
)

// newRelayService 建一个带空 Manager 的 Service。fakeConn 在 session 包内部未导出，
// 跨包无法装配真实 sftp session；本测试只覆盖 handler 的硬错误分支（只用 manager.Get
// + arg 校验，不触 sftp）。端到端流式断言在 session.TestRelayTransferOneToOne。
func newRelayService() *Service {
	mgr := session.NewManager()
	return &Service{manager: mgr, baseLogger: nil}
}

func TestRelayTransferHardErrorEmptyDsts(t *testing.T) {
	svc := newRelayService()
	// 源 session 不存在 → hard error → IsError=true
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

func TestRelayTransferEmptyDstsIsError(t *testing.T) {
	svc := newRelayService()
	// dst_sids 空 → 硬错误路径（源不存在也会先报 not found，但空 dsts 同样是硬错误）
	res, _, err := svc.RelayTransfer(context.Background(), nil, RelayTransferArgs{
		SrcSid: "any", SrcPath: "/s", DstSids: nil, DstPath: "/d",
	})
	if err != nil {
		t.Fatalf("Go error: %v", err)
	}
	if !res.IsError {
		t.Errorf("IsError = false, want true (empty dsts)")
	}
}
