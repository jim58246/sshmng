package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sshmng/internal/config"
	"sshmng/internal/loginflow"
	"sshmng/internal/ssh/conn"
	"sshmng/internal/ssh/pty"
)

// LoginArgs 是 login 工具的入参。
type LoginArgs struct {
	Name string `json:"name" jsonschema:"SSH server name; supports name/addr/tag substring match (use list_ssh_servers to find)"`
}

// RunInSessionArgs 是 run_in_session 工具的入参。
type RunInSessionArgs struct {
	SID            string `json:"sid"`
	Cmd            string `json:"cmd"`
	TimeoutMs      int    `json:"timeout_ms,omitempty" jsonschema:"optional, default 30000 (30s). On timeout: auto Ctrl-C + 3s drain, partial output returned"`
	MaxOutputBytes int    `json:"max_output_bytes,omitempty" jsonschema:"optional, 0 = no truncation. If exceeded: output truncated, truncated=true returned (use tail/grep or redirect+download for large output)"`
}

// CloseSessionArgs 是 close_session 工具的入参。
type CloseSessionArgs struct {
	SID string `json:"sid"`
}

// StatArgs 是 stat 工具的入参（空）。
type StatArgs struct{}

// GetTraceArgs 是 get_trace 工具的入参。
// LastN=0 返回全部；TruncOutput=0 不截断（取完整 raw_output 含 ANSI/sentinel 原始字节）。
type GetTraceArgs struct {
	SID         string `json:"sid"`
	LastN       int    `json:"last_n,omitempty" jsonschema:"optional, return only the last N traces; 0 = all (default)"`
	TruncOutput int    `json:"trunc_output,omitempty" jsonschema:"optional, truncate each Output and raw_output to this many chars; default 200, 0 = no truncation (full raw PTY bytes for debugging)"`
}

// Login 拨通指定 SSH server，建立 PTY session。
//
// 支持三种形态：
//   - 直连：srv.Via 为空，直接 SSH 拨号到 srv.Addr；可选 SSHServer.LoginFlow（target 认证后交互）
//   - Pattern A (srv.Via.SSHJ=true)：经 jumphost 的 direct-tcpip 通道 SSH 到 target（ssh -J 语义）
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

	sid, err := conn.RandomSID()
	if err != nil {
		s.sessionLogger(req, "").Warn("login failed: generate sid", "server", srv.Name, "err", err.Error())
		return errorResult("generate sid: %v", err)
	}
	logger := s.sessionLogger(req, sid)
	logger.Debug("login start", "server", srv.Name, "via", viaDesc(srv))
	dialer := conn.NewDialer(s.knownHosts, logger)

	var ptyConn *pty.PtyConn
	var loginTrace []loginflow.TraceEntry
	switch {
	case srv.Via == nil:
		ptyConn, loginTrace, err = s.setupDirect(srv, dialer, sid, logger)
	case srv.Via.SSHJ:
		ptyConn, loginTrace, err = s.setupPatternA(srv, dialer, sid, logger)
	default: // srv.Via.SSHJ == false
		ptyConn, loginTrace, err = s.setupPatternB(srv, dialer, sid, logger)
	}
	if err != nil {
		// LoginFlow 失败时携带 login_trace 返给 Agent 诊断（设计文档 §3.x）。
		// SSH auth / detectShell / RC 注入等其他失败仅返回 error 字符串。
		var lfErr *pty.LoginFlowError
		if errors.As(err, &lfErr) && len(lfErr.Trace) > 0 {
			last := lfErr.Trace[len(lfErr.Trace)-1]
			logger.Warn("login failed",
				"server", srv.Name, "err", err.Error(),
				"stage", lfErr.Stage, "steps", len(lfErr.Trace), "last_action", last.Send)
			return loginFlowErrorResult(err, lfErr.Trace)
		}
		logger.Warn("login failed", "server", srv.Name, "err", err.Error())
		return errorResult("%v", err)
	}

	idleTimeout := time.Duration(cfg.IdleTimeoutS) * time.Second
	if idleTimeout == 0 {
		idleTimeout = 5 * time.Minute
	}
	sess := s.manager.NewSession(sid, srv.Name, ptyConn, idleTimeout, logger)
	if len(loginTrace) > 0 {
		sess.SetLoginFlowTrace(loginTrace)
	}
	logger.Info("session created", "server", srv.Name, "via", viaDesc(srv), "idle_timeout", idleTimeout.String(), "sftp_available", sess.SftpAvailable())

	return textResult(map[string]any{
		"sid":            sid,
		"server_name":    srv.Name,
		"sftp_available": sess.SftpAvailable(),
	})
}

// setupDirect 处理直连场景：SSH 拨号到 srv.Addr + 可选单段 LoginFlow + RC 注入。
// 成功返回 ptyConn + LoginFlow trace（若 srv.LoginFlow 为空则 trace 为 nil）。
// LoginFlow 失败返回 *pty.LoginFlowError（携带 trace），供 Login handler 提取
// login_trace 字段返给 Agent。
func (s *Service) setupDirect(srv *config.SSHServer, dialer *conn.Dialer, sid string, logger *slog.Logger) (*pty.PtyConn, []loginflow.TraceEntry, error) {
	client, err := dialer.Dial(conn.DialOptions{
		Addr:          srv.Addr,
		User:          srv.User,
		Auth:          srv.Auth,
		Proxy:         srv.Proxy,
		ServerName:    srv.Name,
		HostKeyVerify: srv.HostKeyVerifyEnabled(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ssh connect to %s: %w", srv.Addr, err)
	}
	ptyConn, err := pty.OpenPtyConnWithTimeout(client, sid, logger, 0)
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("setup pty: %w", err)
	}
	var trace []loginflow.TraceEntry
	if len(srv.LoginFlow) > 0 {
		trace, err = ptyConn.RunLoginFlow(srv.LoginFlow, srv.LoginEntry, pty.LoginFlowOptions{
			MaxSteps:        srv.MaxSteps,
			GlobalTimeoutMs: srv.GlobalTimeoutMs,
		})
		if err != nil {
			ptyConn.Close()
			return nil, trace, fmt.Errorf("direct: %w", &pty.LoginFlowError{Stage: "direct", Trace: trace, Err: err})
		}
		logger.Debug("loginflow phase done", "phase", "direct", "steps", len(trace))
	}
	if err := ptyConn.DetectShell(); err != nil {
		ptyConn.Close()
		return nil, trace, fmt.Errorf("detect shell: %w", err)
	}
	if err := ptyConn.InjectRC(); err != nil {
		ptyConn.Close()
		return nil, trace, fmt.Errorf("inject rc: %w", err)
	}
	// 直连：SFTP 通道是到 target 的，探测启用。
	ptyConn.TryEnableSftp()
	logger.Debug("setup done",
		"server", srv.Name,
		"sftp_available", ptyConn.SftpAvailable(), "shell", ptyConn.Shell())
	return ptyConn, trace, nil
}

// setupPatternB 处理 Pattern B 交互式堡垒机场景：
// 拨号 jumphost → OpenPtyConn → Jumphost.LoginFlow（菜单就绪）→ SSHServer.LoginFlow
// （选 target + 凭据）→ DetectShell → InjectRC。两段 LoginFlow 共用同一 PTY，
// trailing data 通过 pushback 在调用间保留。
//
// detectShell 必须在两段 LoginFlow 之后：堡垒机 session.Shell() 启动的是菜单程序，
// 此时探测 bash 命令无法解析；走完 jumphost + target LoginFlow 才进入目标真 shell。
//
// 成功返回 ptyConn + 两段 LoginFlow 拼接的 trace（jumphost 在前 target 在后）。
// LoginFlow 失败返回 *pty.LoginFlowError（携带 trace），供 Login handler 提取
// login_trace 字段返给 Agent。
func (s *Service) setupPatternB(srv *config.SSHServer, dialer *conn.Dialer, sid string, logger *slog.Logger) (*pty.PtyConn, []loginflow.TraceEntry, error) {
	jump := srv.Via
	client, err := dialer.Dial(conn.DialOptions{
		Addr:          jump.Addr,
		User:          jump.User,
		Auth:          jump.Auth,
		Proxy:         jump.Proxy,
		ServerName:    jump.Name,
		HostKeyVerify: jump.HostKeyVerifyEnabled(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ssh connect to jumphost %s: %w", jump.Addr, err)
	}
	ptyConn, err := pty.OpenPtyConnWithTimeout(client, sid, logger, 0)
	if err != nil {
		client.Close()
		return nil, nil, fmt.Errorf("setup pty: %w", err)
	}
	var trace []loginflow.TraceEntry
	if t, err := ptyConn.RunLoginFlow(jump.LoginFlow, jump.LoginEntry, pty.LoginFlowOptions{
		MaxSteps:        jump.MaxSteps,
		GlobalTimeoutMs: jump.GlobalTimeoutMs,
	}); err != nil {
		ptyConn.Close()
		return nil, trace, fmt.Errorf("jumphost: %w", &pty.LoginFlowError{Stage: "jumphost", Trace: t, Err: err})
	} else {
		trace = append(trace, t...)
		logger.Debug("loginflow phase done", "phase", "jumphost", "steps", len(t))
	}
	if t, err := ptyConn.RunLoginFlow(srv.LoginFlow, srv.LoginEntry, pty.LoginFlowOptions{
		MaxSteps:        srv.MaxSteps,
		GlobalTimeoutMs: srv.GlobalTimeoutMs,
	}); err != nil {
		ptyConn.Close()
		return nil, trace, fmt.Errorf("target: %w", &pty.LoginFlowError{Stage: "target", Trace: t, Err: err})
	} else {
		trace = append(trace, t...)
		logger.Debug("loginflow phase done", "phase", "target", "steps", len(t))
	}
	if err := ptyConn.DetectShell(); err != nil {
		ptyConn.Close()
		return nil, trace, fmt.Errorf("detect shell: %w", err)
	}
	if err := ptyConn.InjectRC(); err != nil {
		ptyConn.Close()
		return nil, trace, fmt.Errorf("inject rc: %w", err)
	}
	// Pattern B：SSH client 是到 jumphost 的，SFTP 通道只会到 jumphost 而非 target，
	// 探测成功反而误导（用户以为能 upload 到 target，实际落到 jumphost）。
	// 故不调用 TryEnableSftp，sftp_available 恒为 false。
	logger.Debug("setup done",
		"server", srv.Name,
		"sftp_available", ptyConn.SftpAvailable(), "shell", ptyConn.Shell())
	return ptyConn, trace, nil
}

// setupPatternA 处理 Pattern A 透明转发场景（ssh -J 语义）：
// 拨号 jumphost → 经 jumphost 的 direct-tcpip 通道拨号 target → OpenPtyConn（在
// target 上）→ SetJumpClient（绑定 jumphost 生命周期）→ 可选 SSHServer.LoginFlow
// （target 认证后交互，如 su / 角色 / PAM）→ DetectShell → InjectRC → TryEnableSftp。
//
// 与 setupDirect 的唯一差异：拨号是两层（jumphost + direct-tcpip + target），
// jumphost client 通过 SetJumpClient 挂到 PtyConn，Close 时随 target 一起关。
//
// SSHServer.LoginFlow 在 Pattern A 下可选（承担 target 认证后交互，非登录 target）；
// Jumphost.LoginFlow 校验阶段已确保为空（ssh_j=true 要求）。
//
// 成功返回 ptyConn + LoginFlow trace（若 srv.LoginFlow 为空则 trace 为 nil）。
// 失败分类：
//   - jumphost / target SSH 拨号失败（auth / host key / 网络）：error 字符串，无 trace
//   - SSHServer.LoginFlow 失败：*pty.LoginFlowError 携 trace，Login handler 提取
//     login_trace 返给 Agent
func (s *Service) setupPatternA(srv *config.SSHServer, dialer *conn.Dialer, sid string, logger *slog.Logger) (*pty.PtyConn, []loginflow.TraceEntry, error) {
	jump := srv.Via

	// 第一层：拨号 jumphost
	jumpClient, err := dialer.Dial(conn.DialOptions{
		Addr:          jump.Addr,
		User:          jump.User,
		Auth:          jump.Auth,
		Proxy:         jump.Proxy,
		ServerName:    jump.Name,
		HostKeyVerify: jump.HostKeyVerifyEnabled(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ssh connect to jumphost %s: %w", jump.Addr, err)
	}

	// 第二层：经 jumphost 的 direct-tcpip 拨号 target
	targetClient, err := dialer.DialThrough(jumpClient, conn.DialOptions{
		Addr:          srv.Addr,
		User:          srv.User,
		Auth:          srv.Auth,
		Proxy:         nil, // Pattern A 下 Server.Proxy 已被 validate.go 拒绝
		ServerName:    srv.Name,
		HostKeyVerify: srv.HostKeyVerifyEnabled(),
	})
	if err != nil {
		jumpClient.Close()
		return nil, nil, fmt.Errorf("ssh connect to target %s through jumphost: %w", srv.Addr, err)
	}

	// 在 target 上开 PTY
	ptyConn, err := pty.OpenPtyConnWithTimeout(targetClient, sid, logger, 0)
	if err != nil {
		targetClient.Close()
		jumpClient.Close()
		return nil, nil, fmt.Errorf("setup pty: %w", err)
	}
	// 绑定 jumphost 生命周期：PtyConn.Close 会先关 target client 再关 jumpClient
	ptyConn.SetJumpClient(jumpClient)

	// 可选：SSHServer.LoginFlow（target 认证后交互）
	var trace []loginflow.TraceEntry
	if len(srv.LoginFlow) > 0 {
		trace, err = ptyConn.RunLoginFlow(srv.LoginFlow, srv.LoginEntry, pty.LoginFlowOptions{
			MaxSteps:        srv.MaxSteps,
			GlobalTimeoutMs: srv.GlobalTimeoutMs,
		})
		if err != nil {
			ptyConn.Close() // 关 target + jumphost
			return nil, trace, fmt.Errorf("patternA: %w", &pty.LoginFlowError{Stage: "patternA", Trace: trace, Err: err})
		}
		logger.Debug("loginflow phase done", "phase", "patternA", "steps", len(trace))
	}

	// DetectShell + InjectRC（与 setupDirect 完全一致）
	if err := ptyConn.DetectShell(); err != nil {
		ptyConn.Close()
		return nil, trace, fmt.Errorf("detect shell: %w", err)
	}
	if err := ptyConn.InjectRC(); err != nil {
		ptyConn.Close()
		return nil, trace, fmt.Errorf("inject rc: %w", err)
	}

	// Pattern A：SFTP 通道是到 target 的（与 setupDirect 一致），探测启用
	ptyConn.TryEnableSftp()
	logger.Debug("setup done",
		"server", srv.Name, "via", jump.Name,
		"sftp_available", ptyConn.SftpAvailable(), "shell", ptyConn.Shell())
	return ptyConn, trace, nil
}

func viaDesc(srv *config.SSHServer) string {
	if srv.Via == nil {
		return ""
	}
	return srv.Via.Name
}

// loginFlowErrorResult 把 LoginFlow 失败的 error + trace 包成 IsError=true 的 JSON 响应。
// trace 含每步的 send / expect / output，Agent 据此诊断失败原因（pattern 不匹配 /
// 超时 / 输出与预期不符）、修配置重试。设计文档 §3.x "LoginFlow 失败 error + login_trace"。
func loginFlowErrorResult(err error, trace []loginflow.TraceEntry) (*mcp.CallToolResult, any, error) {
	data, err := json.MarshalIndent(map[string]any{
		"error":       err.Error(),
		"login_trace": trace,
	}, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("marshal login_trace: %w", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(data)}},
		IsError: true,
	}, nil, nil
}

// RunInSession 在指定 session 中执行一条命令。
// 返回 output（已清洗）/ exit_code / timed_out / truncated / total_bytes。
// session 不存在或状态非 idle 时返回 IsError=true。
func (s *Service) RunInSession(ctx context.Context, req *mcp.CallToolRequest, args RunInSessionArgs) (*mcp.CallToolResult, any, error) {
	sess, err := s.manager.Get(args.SID)
	if err != nil {
		return errorResult("%v", err)
	}
	s.sessionLogger(req, args.SID).Debug("run_in_session",
		"server", sess.ServerName(),
		"cmd", args.Cmd, "timeout_ms", args.TimeoutMs, "max_output_bytes", args.MaxOutputBytes)
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

// GetTrace 返回指定 session 的命令 trace + login 阶段的 LoginFlow trace。
//   - commands: 命令历史（cmd / output / exit_code / timed_out），活 session 与
//     已关闭 session（10min TTL）均可查
//   - login_flow: login 阶段 LoginFlow 每步 trace（send / expect / output）；仅活 session
//     有值，已关闭 session 返回 null（graveyard 未存 login_flow）
//
// last_n / trunc_output 仅作用于 commands。
func (s *Service) GetTrace(ctx context.Context, req *mcp.CallToolRequest, args GetTraceArgs) (*mcp.CallToolResult, any, error) {
	traces, err := s.manager.GetTrace(args.SID, args.LastN, args.TruncOutput)
	if err != nil {
		return errorResult("%v", err)
	}
	var loginFlow []loginflow.TraceEntry
	if sess, err := s.manager.Get(args.SID); err == nil {
		loginFlow = sess.LoginFlowTrace()
	}
	return textResult(map[string]any{
		"commands":   traces,
		"login_flow": loginFlow,
	})
}
