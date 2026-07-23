package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jim58246/sshmng/internal/ssh/conn"
)

// UploadDirArgs 是 upload_dir 工具的入参。
type UploadDirArgs struct {
	SID         string `json:"sid"`
	Src         string `json:"src" jsonschema:"local dir path to upload (must exist)"`
	Dst         string `json:"dst" jsonschema:"remote dir path to write (created if not exists, MkdirAll semantics)"`
	Conflict    string `json:"conflict,omitempty" jsonschema:"optional, default 'overwrite'. One of: overwrite, skip, rename"`
	Concurrency int    `json:"concurrency,omitempty" jsonschema:"optional, default 4. Number of files to transfer in parallel"`
	TimeoutMs   int    `json:"timeout_ms,omitempty" jsonschema:"optional, 0 = default 300000 (300s). Per-file timeout"`
}

// DownloadDirArgs 是 download_dir 工具的入参。
type DownloadDirArgs struct {
	SID         string `json:"sid"`
	Src         string `json:"src" jsonschema:"remote dir path to read"`
	Dst         string `json:"dst" jsonschema:"local dir path to write (created if not exists, MkdirAll semantics)"`
	Conflict    string `json:"conflict,omitempty" jsonschema:"optional, default 'overwrite'. One of: overwrite, skip, rename"`
	Concurrency int    `json:"concurrency,omitempty" jsonschema:"optional, default 4. Number of files to transfer in parallel"`
	TimeoutMs   int    `json:"timeout_ms,omitempty" jsonschema:"optional, 0 = default 300000 (300s). Per-file timeout"`
}

// UploadDir 通过 sftp 把本地目录整树上传到远端。
// 内部用 filepath.Walk + MkdirAll + 并发 worker pool（Concurrency 默认 4）。
// Conflict policy：overwrite（默认）/ skip / rename。per-file 错误不中断整树，用 errors.Join 聚合返回。
func (s *Service) UploadDir(ctx context.Context, req *mcp.CallToolRequest, args UploadDirArgs) (*mcp.CallToolResult, any, error) {
	sess, err := s.manager.Get(args.SID)
	if err != nil {
		return errorResult("%v", err)
	}
	s.sessionLogger(req, args.SID).Debug("upload_dir",
		"sid", args.SID, "server", sess.ServerName(),
		"src", args.Src, "dst", args.Dst, "conflict", args.Conflict, "concurrency", args.Concurrency)

	opts := conn.DirTransferOptions{
		Conflict:    conn.ParseConflictPolicy(args.Conflict),
		Concurrency: args.Concurrency,
		TimeoutMs:   args.TimeoutMs,
	}
	res, err := sess.UploadDir(args.Src, args.Dst, opts)
	if err != nil {
		// per-file 错误聚合后仍返回 result（partial），err 非 nil 时 ok=false
		return textResult(map[string]any{
			"sid":       args.SID,
			"ok":        false,
			"bytes":     res.Bytes,
			"files":     res.Files,
			"skipped":   res.Skipped,
			"renamed":   res.Renamed,
			"timed_out": res.TimedOut,
			"err":       err.Error(),
		})
	}
	return textResult(map[string]any{
		"sid":       args.SID,
		"ok":        true,
		"bytes":     res.Bytes,
		"files":     res.Files,
		"skipped":   res.Skipped,
		"renamed":   res.Renamed,
		"timed_out": res.TimedOut,
	})
}

// DownloadDir 通过 sftp 把远端目录整树下载到本地。
// 语义同 UploadDir，方向相反。
func (s *Service) DownloadDir(ctx context.Context, req *mcp.CallToolRequest, args DownloadDirArgs) (*mcp.CallToolResult, any, error) {
	sess, err := s.manager.Get(args.SID)
	if err != nil {
		return errorResult("%v", err)
	}
	s.sessionLogger(req, args.SID).Debug("download_dir",
		"sid", args.SID, "server", sess.ServerName(),
		"src", args.Src, "dst", args.Dst, "conflict", args.Conflict, "concurrency", args.Concurrency)

	opts := conn.DirTransferOptions{
		Conflict:    conn.ParseConflictPolicy(args.Conflict),
		Concurrency: args.Concurrency,
		TimeoutMs:   args.TimeoutMs,
	}
	res, err := sess.DownloadDir(args.Src, args.Dst, opts)
	if err != nil {
		return textResult(map[string]any{
			"sid":       args.SID,
			"ok":        false,
			"bytes":     res.Bytes,
			"files":     res.Files,
			"skipped":   res.Skipped,
			"renamed":   res.Renamed,
			"timed_out": res.TimedOut,
			"err":       err.Error(),
		})
	}
	return textResult(map[string]any{
		"sid":       args.SID,
		"ok":        true,
		"bytes":     res.Bytes,
		"files":     res.Files,
		"skipped":   res.Skipped,
		"renamed":   res.Renamed,
		"timed_out": res.TimedOut,
	})
}
