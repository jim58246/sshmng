package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RelayTransferArgs 是 relay_transfer 工具的入参。
type RelayTransferArgs struct {
	SrcSid    string   `json:"src_sid"`
	SrcPath   string   `json:"src_path" jsonschema:"remote file path on the source session to relay"`
	DstSids   []string `json:"dst_sids" jsonschema:"destination session sids; 1:1 = single-element array"`
	DstPath   string   `json:"dst_path" jsonschema:"remote write path shared by all destinations"`
	TimeoutMs int      `json:"timeout_ms,omitempty" jsonschema:"optional, 0 = default 300000 (300s). Each of download + every upload side gets its own budget (concurrent)"`
}

// relayDestJSON 是单个目标的 JSON 输出（把 error 转字符串，避免 marshal 成 {}）。
type relayDestJSON struct {
	DstSid    string `json:"dst_sid"`
	DstServer string `json:"dst_server"`
	OK        bool   `json:"ok"`
	Bytes     int    `json:"bytes"`
	TimedOut  bool   `json:"timed_out"`
	Error     string `json:"error,omitempty"`
}

// RelayTransfer 把 src_sid session 上的文件流式中转到 dst_sids 各 session。
// 1:N fanout：源文件只读一次，并发分发到所有目标；1:1 是单元素数组特例。
// 所有目标共用 dst_path。需要源与每个目标的 sftp_available=true（先用 stat 检查）。
// 单目标失败不中止其他目标；部分失败返回 ok:false 的结果体（IsError 不置位），
// 检查 ok 字段而非 MCP error flag——与 upload_dir/download_dir 一致。
// 仅硬错误（sid 不存在、dst_sids 为空）返回 IsError=true。
func (s *Service) RelayTransfer(ctx context.Context, req *mcp.CallToolRequest, args RelayTransferArgs) (*mcp.CallToolResult, any, error) {
	res, err := s.manager.RelayTransfer(args.SrcSid, args.SrcPath, args.DstSids, args.DstPath, args.TimeoutMs)
	if err != nil {
		// 硬错误：src 不存在 / dst_sids 为空
		return errorResult("%v", err)
	}

	s.sessionLogger(req, args.SrcSid).Info("relay_transfer",
		"src_server", res.SrcServer,
		"downloaded_bytes", res.DownloadedBytes,
		"destinations", len(res.Destinations),
		"ok", res.Err == nil)

	dests := make([]relayDestJSON, len(res.Destinations))
	for i, d := range res.Destinations {
		errStr := ""
		if d.Err != nil {
			errStr = d.Err.Error()
		}
		dests[i] = relayDestJSON{
			DstSid:    d.DstSid,
			DstServer: d.DstServer,
			OK:        d.OK,
			Bytes:     d.Bytes,
			TimedOut:  d.TimedOut,
			Error:     errStr,
		}
	}

	out := map[string]any{
		"ok":               res.Err == nil,
		"downloaded_bytes": res.DownloadedBytes,
		"timed_out":        res.TimedOut,
		"src_server":       res.SrcServer,
		"destinations":     dests,
	}
	if res.Err != nil {
		out["error"] = res.Err.Error()
	}
	return textResult(out)
}
