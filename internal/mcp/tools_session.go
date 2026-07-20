package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sshmng/internal/config"
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
//
// 支持三种形态：
//   - 直连：srv.Via 为空，直接 SSH 拨号到 srv.Addr；可选 SSHServer.LoginFlow（target 认证后交互）
//   - Pattern A (srv.Via.SSHJ=true)：经 jumphost 的 direct-tcpip 通道 SSH 到 target（v1.x 实现，当前拒绝）
//   - Pattern B (srv.Via.SSHJ=false)：交互式堡垒机。拨号到 jumphost → Jumphost.LoginFlow（菜单就绪）
//     → SSHServer.LoginFlow（选 target + 输入凭据）→ 注入 RC。两段 LoginFlow 共用同一 PTY。
//
// 成功返回 sid；失败返回 IsError=true 的结果。SSH auth 失败仅 error 字符串；
// LoginFlow 失败 error 含 "loginflow" / "no expect matched" 供 Agent 诊断。
func (s *Service) Login(ctx context.Context, req *mcp.CallToolRequest, args LoginArgs) (*mcp.CallToolResult, any, error) {
	cfg, err := s.store.Load()
	if err != nil {
		return errorResult("load config: %v", err)
	}
	srv, err := cfg.GetSSHServer(args.Name)
	if err != nil {
		return errorResult("%v", err)
	}

	if srv.Via != nil && srv.Via.SSHJ {
		// Pattern A (ssh -J 语义) 留 v1.x 实现
		return errorResult("pattern A via ssh-j jumphost %q not yet supported (server %q); deferred to v1.x", srv.Via.Name, args.Name)
	}

	sid, err := ssh.RandomSID()
	if err != nil {
		s.sessionLogger(req, "").Warn("login failed: generate sid", "server", srv.Name, "err", err.Error())
		return errorResult("generate sid: %v", err)
	}
	logger := s.sessionLogger(req, sid)
	dialer := ssh.NewDialer(s.knownHosts)

	var ptyConn *ssh.PtyConn
	if srv.Via != nil {
		ptyConn, err = s.setupPatternB(srv, dialer, sid, logger)
	} else {
		ptyConn, err = s.setupDirect(srv, dialer, sid, logger)
	}
	if err != nil {
		logger.Warn("login failed", "server", srv.Name, "err", err.Error())
		return errorResult("%v", err)
	}

	idleTimeout := time.Duration(cfg.IdleTimeoutS) * time.Second
	if idleTimeout == 0 {
		idleTimeout = 5 * time.Minute
	}
	s.manager.NewSession(sid, srv.Name, ptyConn, idleTimeout, logger)
	logger.Info("session created", "server", srv.Name, "via", viaDesc(srv), "idle_timeout", idleTimeout.String())

	return textResult(map[string]any{
		"sid":            sid,
		"server_name":    srv.Name,
		"sftp_available": false, // v1 phase 5 加入
	})
}

// setupDirect 处理直连场景：SSH 拨号到 srv.Addr + 可选单段 LoginFlow + RC 注入。
func (s *Service) setupDirect(srv *config.SSHServer, dialer *ssh.Dialer, sid string, logger *slog.Logger) (*ssh.PtyConn, error) {
	client, err := dialer.Dial(ssh.DialOptions{
		Addr:  srv.Addr,
		User:  srv.User,
		Auth:  srv.Auth,
		Proxy: srv.Proxy,
	})
	if err != nil {
		return nil, fmt.Errorf("ssh connect to %s: %w", srv.Addr, err)
	}
	ptyConn, err := ssh.NewPtyConn(client, sid, &ssh.PtyConnOptions{
		LoginFlow:       srv.LoginFlow,
		LoginEntry:      srv.LoginEntry,
		MaxSteps:        srv.MaxSteps,
		GlobalTimeoutMs: srv.GlobalTimeoutMs,
	})
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("setup pty: %w", err)
	}
	return ptyConn, nil
}

// setupPatternB 处理 Pattern B 交互式堡垒机场景：
// 拨号 jumphost → OpenPtyConn → Jumphost.LoginFlow（菜单就绪）→ SSHServer.LoginFlow
// （选 target + 凭据）→ InjectRC。两段 LoginFlow 共用同一 PTY，trailing data 通过
// pushback 在调用间保留。
func (s *Service) setupPatternB(srv *config.SSHServer, dialer *ssh.Dialer, sid string, logger *slog.Logger) (*ssh.PtyConn, error) {
	jump := srv.Via
	client, err := dialer.Dial(ssh.DialOptions{
		Addr:  jump.Addr,
		User:  jump.User,
		Auth:  jump.Auth,
		Proxy: jump.Proxy,
	})
	if err != nil {
		return nil, fmt.Errorf("ssh connect to jumphost %s: %w", jump.Addr, err)
	}
	ptyConn, err := ssh.OpenPtyConn(client, sid)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("setup pty: %w", err)
	}
	if _, err := ptyConn.RunLoginFlow(jump.LoginFlow, jump.LoginEntry, ssh.LoginFlowOptions{
		MaxSteps:        jump.MaxSteps,
		GlobalTimeoutMs: jump.GlobalTimeoutMs,
	}); err != nil {
		ptyConn.Close()
		return nil, fmt.Errorf("jumphost loginflow: %w", err)
	}
	if _, err := ptyConn.RunLoginFlow(srv.LoginFlow, srv.LoginEntry, ssh.LoginFlowOptions{
		MaxSteps:        srv.MaxSteps,
		GlobalTimeoutMs: srv.GlobalTimeoutMs,
	}); err != nil {
		ptyConn.Close()
		return nil, fmt.Errorf("target loginflow: %w", err)
	}
	if err := ptyConn.InjectRC(); err != nil {
		ptyConn.Close()
		return nil, fmt.Errorf("inject rc: %w", err)
	}
	return ptyConn, nil
}

// viaDesc 返回 server 的 via 描述，用于日志。无 via 时返回空字符串。
func viaDesc(srv *config.SSHServer) string {
	if srv.Via == nil {
		return ""
	}
	return srv.Via.Name
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
