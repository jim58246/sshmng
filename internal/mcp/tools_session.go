package mcp

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sshmng/internal/ssh"
)

// LoginArgs 是 login 工具的入参。
type LoginArgs struct {
	Name string `json:"name" jsonschema:"SSH server name (use list_ssh_servers to find)"`
}

// RunInSessionArgs 是 run_in_session 工具的入参。
type RunInSessionArgs struct {
	SID            string `json:"sid"`
	Cmd            string `json:"cmd"`
	TimeoutMs      int    `json:"timeout_ms,omitempty" jsonschema:"optional, default 30000; command timeout in milliseconds"`
	MaxOutputBytes int    `json:"max_output_bytes,omitempty" jsonschema:"optional, 0 = no truncation"`
}

// CloseSessionArgs 是 close_session 工具的入参。
type CloseSessionArgs struct {
	SID string `json:"sid"`
}

// StatArgs 是 stat 工具的入参（空）。
type StatArgs struct{}

// Login 拨通指定 SSH server，建立 PTY session。
// v1 phase 2 仅支持直连（Via 为空、无 LoginFlow）；jumphost / login_flow 在后续阶段支持。
// 成功返回 sid；失败返回 IsError=true 的结果（不含 login_trace，因为 SSH 层错误自解释）。
func (s *Service) Login(ctx context.Context, req *mcp.CallToolRequest, args LoginArgs) (*mcp.CallToolResult, any, error) {
	cfg, err := s.store.Load()
	if err != nil {
		return errorResult("load config: %v", err)
	}
	srv, err := cfg.GetSSHServer(args.Name)
	if err != nil {
		return errorResult("%v", err)
	}

	// v1 phase 2 限制：只支持直连
	if srv.Via != nil {
		return errorResult("jumphost via not supported in v1 phase 2 (server %q uses via %q); will be added in phase 4", args.Name, srv.Via.Name)
	}
	if srv.LoginFlow != nil {
		return errorResult("server login_flow not supported in v1 phase 2 (server %q); will be added in phase 3", args.Name)
	}

	dialer := ssh.NewDialer(s.knownHosts)
	client, err := dialer.Dial(ssh.DialOptions{
		Addr:  srv.Addr,
		User:  srv.User,
		Auth:  srv.Auth,
		Proxy: srv.Proxy,
	})
	if err != nil {
		// 不记录凭据；addr/user 在 config 中可见，不算敏感
		s.sessionLogger(req, "").Warn("login failed: ssh dial",
			"server", srv.Name, "addr", srv.Addr, "err", err.Error())
		return errorResult("ssh connect to %s: %v", srv.Addr, err)
	}

	sid, err := ssh.RandomSID()
	if err != nil {
		client.Close()
		s.sessionLogger(req, "").Warn("login failed: generate sid", "server", srv.Name, "err", err.Error())
		return errorResult("generate sid: %v", err)
	}
	logger := s.sessionLogger(req, sid)
	ptyConn, err := ssh.NewPtyConn(client, sid)
	if err != nil {
		client.Close()
		logger.Warn("login failed: setup pty", "server", srv.Name, "err", err.Error())
		return errorResult("setup pty: %v", err)
	}

	idleTimeout := time.Duration(cfg.IdleTimeoutS) * time.Second
	if idleTimeout == 0 {
		idleTimeout = 5 * time.Minute
	}
	s.manager.NewSession(sid, srv.Name, ptyConn, idleTimeout, logger)
	logger.Info("session created", "server", srv.Name, "addr", srv.Addr, "idle_timeout", idleTimeout.String())

	return textResult(map[string]any{
		"sid":            sid,
		"server_name":    srv.Name,
		"sftp_available": false, // v1 phase 2 doesn't support sftp; added in phase 5
	})
}

// RunInSession 在指定 session 中执行一条命令。
// 返回 output（已清洗）/ exit_code / timed_out / truncated / total_bytes。
// session 不存在或状态非 idle 时返回 IsError=true。
func (s *Service) RunInSession(ctx context.Context, req *mcp.CallToolRequest, args RunInSessionArgs) (*mcp.CallToolResult, any, error) {
	sess, err := s.manager.Get(args.SID)
	if err != nil {
		return errorResult("%v", err)
	}
	output, exitCode, timedOut, truncated, totalBytes, err := sess.RunInSession(args.Cmd, args.TimeoutMs, args.MaxOutputBytes)
	if err != nil {
		return errorResult("%v", err)
	}
	if timedOut {
		s.sessionLogger(req, args.SID).Warn("command timed out",
			"server", sess.ServerName(), "timeout_ms", args.TimeoutMs, "total_bytes", totalBytes)
	}
	return textResult(map[string]any{
		"output":      output,
		"exit_code":   exitCode,
		"timed_out":   timedOut,
		"truncated":   truncated,
		"total_bytes": totalBytes,
	})
}

// CloseSession 强制关闭指定 session。
// 重复调用或 sid 不存在返回 IsError=true。
func (s *Service) CloseSession(ctx context.Context, req *mcp.CallToolRequest, args CloseSessionArgs) (*mcp.CallToolResult, any, error) {
	sess, err := s.manager.Get(args.SID)
	if err != nil {
		return errorResult("%v", err)
	}
	serverName := sess.ServerName()
	if err := sess.Close(); err != nil {
		s.sessionLogger(req, args.SID).Warn("close session failed", "server", serverName, "err", err.Error())
		return errorResult("close session %q: %v", args.SID, err)
	}
	s.sessionLogger(req, args.SID).Info("session closed", "server", serverName)
	return textResult(map[string]any{"sid": args.SID, "closed": true})
}

// Stat 返回所有活跃 session 的摘要。
func (s *Service) Stat(ctx context.Context, req *mcp.CallToolRequest, args StatArgs) (*mcp.CallToolResult, any, error) {
	return textResult(s.manager.Stat())
}
