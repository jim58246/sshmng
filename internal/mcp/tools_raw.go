package mcp

import (
	"context"
	"strconv"
	"strings"

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
	Input string `json:"input" jsonschema:"input for the PTY. The server interprets C-style escapes: \\r = Enter, \\n, \\t, \\e = ESC, \\uXXXX (e.g. \\u0003 = Ctrl-C), \\\\ = literal backslash. Everything else is written as-is. Max 64KB"`
}

// ReadInSessionArgs 是 read_in_session 工具的入参。
type ReadInSessionArgs struct {
	SID      string `json:"sid"`
	WaitMs   int    `json:"wait_ms,omitempty" jsonschema:"optional, default 5000 (max 60000). Blocks until first output byte or timeout; use 1 for non-blocking poll. Do NOT poll with small values"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"optional, default 131072 (max 1048576). Excess stays queued, more=true"`
}

// expandSendInput 解释 send_in_session input 的最小转义集：
//   - \r → CR(0x0D)   \n → LF(0x0A)   \t → TAB(0x09)   \e → ESC(0x1B)
//   - \uXXXX → 对应 Unicode 码点(4 位 hex;覆盖 Ctrl-C 的 \u0003 等)
//   - \\ → 字面反斜杠(逃逸口,保证可逆);孤反斜杠按字面处理
//
// 其余字节 verbatim。设计动机(收敛式转义)：模型生成 tool-call JSON 时 \r 的落地
// 形态不可控——JSON 转义 "\r" 解码为真 CR,防御性双转义 "\\r" 解码为字面两字符。
// 服务端解释后两者殊途同归为 CR,契约对模型的转义行为免疫。要发字面 "\r" 文本
// 需写 "\\r"。与 PtyConn.SendRaw 的纯 verbatim 语义分层:转义是 MCP 工具契约,不是传输原语。
func expandSendInput(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s // 快路径:绝大多数输入无反斜杠
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) {
			b.WriteByte(c) // 孤反斜杠:按字面
			break
		}
		switch s[i+1] {
		case 'r':
			b.WriteByte('\r')
			i++
		case 'n':
			b.WriteByte('\n')
			i++
		case 't':
			b.WriteByte('\t')
			i++
		case 'e':
			b.WriteByte(0x1b)
			i++
		case '\\':
			b.WriteByte('\\')
			i++
		case 'u':
			if i+6 <= len(s) {
				if v, err := strconv.ParseUint(s[i+2:i+6], 16, 32); err == nil {
					b.WriteRune(rune(v))
					i += 6
					continue
				}
			}
			b.WriteByte(c) // \u 后非 4 位 hex:按字面反斜杠,后续字符原样走
		default:
			b.WriteByte(c) // 未知转义:反斜杠按字面,下一轮循环输出后续字符
		}
	}
	return b.String()
}

// SendInSession 把 input 原样写入 session PTY（终端原语，详见 server instructions）。
// raw 设备（mode=raw）的主要操作方式；unix session 用于交互型/持续型程序。
// input 先经 expandSendInput 解释转义（见函数注释），再写入。
func (s *Service) SendInSession(ctx context.Context, req *mcp.CallToolRequest, args SendInSessionArgs) (*mcp.CallToolResult, any, error) {
	if len(args.Input) > maxSendInputBytes {
		return errorResult("input too large: %d bytes (max %d)", len(args.Input), maxSendInputBytes)
	}
	sess, err := s.manager.Get(args.SID)
	if err != nil {
		return errorResult("%v", err)
	}
	input := expandSendInput(args.Input)
	s.sessionLogger(req, args.SID).Debug("send_in_session",
		"server", sess.ServerName(), "input_bytes", len(input))
	sent, err := sess.SendInSession(input)
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
