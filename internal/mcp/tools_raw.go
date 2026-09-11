package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// raw 终端原语的限额（spec 2026-09-11，服务端强制，不信任 AI 传参）。
const (
	defaultReadWaitMs   = 5000
	maxReadWaitMs       = 60000
	defaultReadMaxBytes = 128 * 1024
	maxReadMaxBytes     = 1 << 20
	maxSendInputBytes   = 64 * 1024
)

// SendInSessionArgs 是 send_in_session 工具的入参。
type SendInSessionArgs struct {
	SID   string `json:"sid"`
	Input string `json:"input" jsonschema:"raw input written verbatim to the PTY; include Enter ('\r'), Ctrl-C ('\u0003'), pager keys yourself. Max 64KB"`
}

// ReadInSessionArgs 是 read_in_session 工具的入参。
type ReadInSessionArgs struct {
	SID      string `json:"sid"`
	WaitMs   int    `json:"wait_ms,omitempty" jsonschema:"optional, default 5000 (max 60000). Blocks until first output byte or timeout; use 1 for non-blocking poll. Do NOT poll with small values"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"optional, default 131072 (max 1048576). Excess stays queued, more=true"`
}

// SendInSession 把 input 原样写入 session PTY（终端原语，详见 server instructions）。
// raw 设备（mode=raw）的主要操作方式；unix session 用于交互型/持续型程序。
func (s *Service) SendInSession(ctx context.Context, req *mcp.CallToolRequest, args SendInSessionArgs) (*mcp.CallToolResult, any, error) {
	if len(args.Input) > maxSendInputBytes {
		return errorResult("input too large: %d bytes (max %d)", len(args.Input), maxSendInputBytes)
	}
	sess, err := s.manager.Get(args.SID)
	if err != nil {
		return errorResult("%v", err)
	}
	s.sessionLogger(req, args.SID).Debug("send_in_session",
		"server", sess.ServerName(), "input_bytes", len(args.Input))
	sent, err := sess.SendInSession(args.Input)
	if err != nil {
		return errorResult("%v", err)
	}
	return textResult(map[string]any{"sid": args.SID, "sent_bytes": sent})
}

// ReadInSession 读取 session PTY 的新输出（quiet 吸收，顺序游标，数据不丢）。
func (s *Service) ReadInSession(ctx context.Context, req *mcp.CallToolRequest, args ReadInSessionArgs) (*mcp.CallToolResult, any, error) {
	waitMs := args.WaitMs
	if waitMs <= 0 {
		waitMs = defaultReadWaitMs
	}
	if waitMs > maxReadWaitMs {
		waitMs = maxReadWaitMs
	}
	maxBytes := args.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultReadMaxBytes
	}
	if maxBytes > maxReadMaxBytes {
		maxBytes = maxReadMaxBytes
	}
	sess, err := s.manager.Get(args.SID)
	if err != nil {
		return errorResult("%v", err)
	}
	s.sessionLogger(req, args.SID).Debug("read_in_session",
		"server", sess.ServerName(), "wait_ms", waitMs, "max_bytes", maxBytes)
	output, more, idleMs, err := sess.ReadInSession(waitMs, maxBytes)
	if err != nil {
		return errorResult("%v", err)
	}
	return textResult(map[string]any{
		"output":  output,
		"more":    more,
		"idle_ms": idleMs,
	})
}
