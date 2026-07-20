package mcp

import (
	"context"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// UploadArgs 是 upload 工具的入参。
//   - SID: 会话 ID
//   - Src: 本地文件路径（不存在则报错）
//   - Dst: 远端文件路径（如 /tmp/remote.txt）
//   - TimeoutMs: 可选，0 用默认 300s
type UploadArgs struct {
	SID       string `json:"sid"`
	Src       string `json:"src" jsonschema:"local file path to upload"`
	Dst       string `json:"dst" jsonschema:"remote file path to write"`
	TimeoutMs int    `json:"timeout_ms,omitempty" jsonschema:"optional, 0 = default 300000"`
}

// DownloadArgs 是 download 工具的入参。
//   - SID: 会话 ID
//   - Src: 远端文件路径
//   - Dst: 本地文件路径（会被覆盖）
//   - TimeoutMs: 可选，0 用默认 300s
type DownloadArgs struct {
	SID       string `json:"sid"`
	Src       string `json:"src" jsonschema:"remote file path to read"`
	Dst       string `json:"dst" jsonschema:"local file path to write"`
	TimeoutMs int    `json:"timeout_ms,omitempty" jsonschema:"optional, 0 = default 300000"`
}

// Upload 通过 sftp 通道把本地文件上传到远端。
// sftp 不可用时返回 IsError=true 的 "sftp not available" 错误。
// 超时返回已传输字节 + timed_out=true。
func (s *Service) Upload(ctx context.Context, req *mcp.CallToolRequest, args UploadArgs) (*mcp.CallToolResult, any, error) {
	sess, err := s.manager.Get(args.SID)
	if err != nil {
		return errorResult("%v", err)
	}
	f, err := os.Open(args.Src)
	if err != nil {
		return errorResult("open local %s: %v", args.Src, err)
	}
	defer f.Close()
	bytes, timedOut, err := sess.Upload(f, args.Dst, args.TimeoutMs)
	if err != nil && !timedOut {
		return errorResult("%v", err)
	}
	return textResult(map[string]any{
		"sid":        args.SID,
		"ok":         err == nil,
		"bytes":      bytes,
		"timed_out":  timedOut,
		"remote_url": args.Dst,
	})
}

// Download 通过 sftp 通道把远端文件下载到本地。
// 语义同 Upload，方向相反。
func (s *Service) Download(ctx context.Context, req *mcp.CallToolRequest, args DownloadArgs) (*mcp.CallToolResult, any, error) {
	sess, err := s.manager.Get(args.SID)
	if err != nil {
		return errorResult("%v", err)
	}
	f, err := os.Create(args.Dst)
	if err != nil {
		return errorResult("create local %s: %v", args.Dst, err)
	}
	defer f.Close()
	bytes, timedOut, err := sess.Download(args.Src, f, args.TimeoutMs)
	if err != nil && !timedOut {
		return errorResult("%v", err)
	}
	return textResult(map[string]any{
		"sid":       args.SID,
		"ok":        err == nil,
		"bytes":     bytes,
		"timed_out": timedOut,
		"local_url": args.Dst,
	})
}
