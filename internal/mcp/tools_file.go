package mcp

import (
	"context"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// UploadArgs 是 upload 工具的入参。
type UploadArgs struct {
	SID       string `json:"sid"`
	Src       string `json:"src" jsonschema:"local file path to upload (must exist)"`
	Dst       string `json:"dst" jsonschema:"remote file path to write (e.g. /tmp/remote.txt)"`
	TimeoutMs int    `json:"timeout_ms,omitempty" jsonschema:"optional, 0 = default 300000 (300s). Returns timed_out=true + partial bytes if exceeded"`
}

// DownloadArgs 是 download 工具的入参。
type DownloadArgs struct {
	SID       string `json:"sid"`
	Src       string `json:"src" jsonschema:"remote file path to read"`
	Dst       string `json:"dst" jsonschema:"local file path to write (overwritten if exists)"`
	TimeoutMs int    `json:"timeout_ms,omitempty" jsonschema:"optional, 0 = default 300000 (300s). Returns timed_out=true + partial bytes if exceeded"`
}

// Upload 通过 sftp 通道把本地文件上传到远端。
// sftp 不可用时返回 IsError=true 的 "sftp not available" 错误。
// 超时返回已传输字节 + timed_out=true。
func (s *Service) Upload(ctx context.Context, req *mcp.CallToolRequest, args UploadArgs) (*mcp.CallToolResult, any, error) {
	sess, err := s.manager.Get(args.SID)
	if err != nil {
		return errorResult("%v", err)
	}
	s.sessionLogger(req, args.SID).Debug("upload",
		"server", sess.ServerName(),
		"src", args.Src, "dst", args.Dst, "timeout_ms", args.TimeoutMs)
	f, err := os.Open(args.Src)
	if err != nil {
		return errorResult("open local %s: %v", args.Src, err)
	}
	defer f.Close()
	bytes, timedOut, err := sess.Upload(f, args.Dst, args.TimeoutMs)
	if err != nil && !timedOut {
		s.sessionLogger(req, args.SID).Warn("upload failed",
			"server", sess.ServerName(),
			"src", args.Src, "dst", args.Dst, "err", err.Error())
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
	s.sessionLogger(req, args.SID).Debug("download",
		"server", sess.ServerName(),
		"src", args.Src, "dst", args.Dst, "timeout_ms", args.TimeoutMs)
	f, err := os.Create(args.Dst)
	if err != nil {
		return errorResult("create local %s: %v", args.Dst, err)
	}
	defer f.Close()
	bytes, timedOut, err := sess.Download(args.Src, f, args.TimeoutMs)
	if err != nil && !timedOut {
		s.sessionLogger(req, args.SID).Warn("download failed",
			"server", sess.ServerName(),
			"src", args.Src, "dst", args.Dst, "err", err.Error())
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
