// Package mcp 实现 SSH 会话管理工具的 MCP server 端：注册工具、调度 handler、
// 在 stdio 上对外提供 list/get/update 三类 CRUD 工具。
//
// 设计要点：
//   - Service 持有 *config.Store 和一个 sync.Mutex。同一时刻只有一个 handler
//     能持有锁，保证并发安全。
//   - 所有 update_* handler 走 "load → mutate → validate → save" 流程；校验
//     失败时 config 内部已回滚到原状态，handler 只需把 error 转成 IsError=true
//     的 CallToolResult 即可。
//   - list_* 输出会脱敏 auth（去掉 password / private_key / passphrase），因为
//     list 常用于概览。get_* 输出保留完整 auth，因为调用方已明确指定 name。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sshmng/internal/config"
	"sshmng/internal/ssh/conn"
	"sshmng/internal/ssh/session"
)

// serverInstructions 是 MCP server 的整体说明，发送给 client（Agent）作为
// initialize 响应的一部分。Agent 据此理解工作流、session 语义、生命周期、失败诊断路径、
// 实体模型——这些信息单靠 tool description 无法充分传达。
//
// 注意：Claude Code 对 server instructions 有 2KB 截断。当前文本约 2.3KB，末尾
// "Failure recovery" 节可能被截断。关键约束（session 复用、loginflow error→login_trace、
// Pattern A 支持 sftp / Pattern B 不支持、idle timeout、NOPASSWD）都塞在前面。
// 压缩后 Instructions 不保证重新注入，关键信息也散落在各 tool description 里兜底。
const serverInstructions = `SSH session manager: servers, jumphosts, proxies, sessions.

== Entity model ==
- SSHServer: name, addr (host:port), user, auth, login_flow?, via?, proxy?, tags, host_key_verify? (default true; set false to skip known_hosts TOFU; Pattern B target login via PTY ignores this — only the jumphost's switch applies).
- Jumphost: ssh_j=true → transparent (ssh -J, login_flow empty); ssh_j=false → bastion (login_flow required). Also has host_key_verify? (default true; set false to skip known_hosts TOFU on dial to this jumphost).
- Proxy: transport HTTP/SOCKS5. Fields: name, type, addr, auth?, tags.
- Relationships: SSHServer.via → Jumphost.name; *.proxy → Proxy.name.
- Auth: password / private_key + passphrase. Prefer NOPASSWD/private_key.
- LoginFlow: map[action]→{send, expects[], timeout_ms}. Expect={pattern, next}; pattern=glob or "re:..." regex; next=action or "success". login_entry=start.

== Workflow ==
1. list_ssh_servers / list_jumphosts / list_proxies (query: keywords AND on name/addr/tags) → resolve name.
2. login(name) → {sid, server_name, sftp_available}. ssh_j=true: transparent ssh -J to target (sftp available). ssh_j=false: jumphost then target LoginFlow, same PTY (no sftp).
3. run_in_session(sid, cmd) → {output, exit_code, timed_out, truncated, total_bytes}.
4. close_session(sid), or rely on idle timeout (default 5min via idle_timeout_s).

== Session semantics ==
- Each session is an independent PTY with its own shell. State (cwd, env, background jobs, history) never leaks across sessions.
- run_in_session runs in the session's PTY; state persists across calls: ` + "`cd /tmp`" + ` then ` + "`ls`" + ` lists /tmp; ` + "`export FOO=bar`" + ` then ` + "`echo $FOO`" + ` prints bar; background jobs keep running.
- Reuse one session for related work. New for fresh state (different user, clean cwd).

== Session lifecycle ==
- States: idle / running / closed. run_in_session requires idle; use stat. idle timeout resets on activity. After close_session: trace 10min for get_trace.

== Failure recovery ==
- loginflow error → login_trace in response.
- run_in_session timeout → auto Ctrl-C + 3s drain. Drain fail: closed, re-login.
- Output insufficient → get_trace(sid, last_n, trunc_output=0) for raw_output.
- upload/download "sftp not available" → check stat.sftp_available first.
`

// Service 是工具 handler 的宿主，封装 store / session manager / known_hosts 与并发保护。
type Service struct {
	store      *config.Store
	knownHosts *conn.KnownHostsStore
	manager    *session.Manager
	baseLogger *slog.Logger // stderr 兜底日志；sessionLogger 不可用时退回此 logger
	mu         sync.Mutex
}

// NewService 创建一个绑定到 store + knownHosts 的 Service。
// baseLogger 用于 stderr 兜底（bootstrap / 异步事件无 req.Session 时）；nil 退化为 discard。
// 内部创建 session Manager。
func NewService(store *config.Store, knownHosts *conn.KnownHostsStore, baseLogger *slog.Logger) *Service {
	if baseLogger == nil {
		baseLogger = slog.New(slog.DiscardHandler)
	}
	return &Service{
		store:      store,
		knownHosts: knownHosts,
		manager:    session.NewManager(),
		baseLogger: baseLogger,
	}
}

// sessionLogger 返回绑定 sid 的 logger。
//
// 所有日志（DEBUG/INFO/Warn/Error）走 baseLogger → config.log_path 指定的文件
// （或 io.Discard 当 log_path 为空），绝不走 MCP notifications/message。原因：
// MCP SDK 的 LoggingHandler.Handle 同步调 ss.Log → ioConn.Write，和 tool result
// 共用 stdout JSON-RPC 通道（writeMu 串行化）。DEBUG 日志多了会堵塞 tool result
// （用户观察到"卡住"），且 notification 进入 Agent 上下文污染决策。
//
// 日志级别由 config.log_level 控制（main.go 加载时解析）：默认 INFO，支持缩写
// （debug/dbg/d、info/inf/i、warn/warning/w、error/err/e）。配错 Load 报错。
//
// req 参数保留是为了调用方签名兼容，实际不使用。
func (s *Service) sessionLogger(req *mcp.CallToolRequest, sid string) *slog.Logger {
	_ = req
	l := s.baseLogger
	if sid != "" {
		l = l.With("sid", sid)
	}
	return l
}

// ListArgs 是 list_* 工具的入参。Query 为空（或纯空白）时返回全部。
type ListArgs struct {
	Query string `json:"query,omitempty" jsonschema:"optional, space-separated keywords with AND semantics; each keyword substring-matches name/addr/tags (case-insensitive). e.g. 'prod web' returns entities matching both 'prod' AND 'web'"`
}

// GetArgs 是 get_* 工具的入参。
type GetArgs struct {
	Name string `json:"name" jsonschema:"entity name"`
}

// UpdateArgs 是 update_* 工具的入参。Patch 是 RFC 7396 JSON Merge Patch：
// null 表示删除整个实体，object 表示合并到现有实体（不存在则创建）。
type UpdateArgs struct {
	Name  string `json:"name" jsonschema:"entity name; if not exist, create"`
	Patch any    `json:"patch" jsonschema:"RFC 7396 JSON Merge Patch; null deletes the entity, object merges (or creates if name not found). Structure mirrors get_* output; via/proxy fields are name strings"`
}

// NewServer 创建 MCP server 并注册 18 个工具（9 CRUD + 7 session/file + 2 dir transfer）。
func NewServer(svc *Service) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "sshmng", Version: "v1"}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_ssh_servers",
		Description: "List SSH servers, optionally filtered by query. Query is space-separated keywords with AND semantics: each keyword substring-matches name/addr/tags (case-insensitive), and ALL keywords must match. Empty/whitespace query returns all. Sensitive auth fields (password/private_key/passphrase) are redacted in output. Use get_ssh_server for full auth.",
	}, svc.ListSSHServers)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_ssh_server",
		Description: "Get a single SSH server by name, including full auth (password/private_key/passphrase). Use to inspect LoginFlow structure or auth method before login.",
	}, svc.GetSSHServer)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_ssh_server",
		Description: "Apply RFC 7396 JSON Merge Patch to an SSH server. Patch=null deletes the entity; object merges (or creates if name not found). Patch structure mirrors get_ssh_server output; via/proxy fields reference entity names (strings).",
	}, svc.UpdateSSHServer)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_jumphosts",
		Description: "List jumphosts, optionally filtered by query. Query is space-separated keywords with AND semantics (each keyword substring-matches name/addr/tags, case-insensitive; all must match). Empty/whitespace query returns all. ssh_j=true means transparent forwarding (ssh -J semantics); ssh_j=false means interactive bastion (login_flow required, drives menu to ready state).",
	}, svc.ListJumphosts)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_jumphost",
		Description: "Get a single jumphost by name, including full auth.",
	}, svc.GetJumphost)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_jumphost",
		Description: "Apply RFC 7396 JSON Merge Patch to a jumphost. Patch=null deletes; object merges (or creates if name not found). Patch structure mirrors get_jumphost output; via/proxy fields reference entity names.",
	}, svc.UpdateJumphost)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_proxies",
		Description: "List transport-layer proxies (HTTP/SOCKS5), optionally filtered by query. Query is space-separated keywords with AND semantics (each keyword substring-matches name/addr/tags, case-insensitive; all must match). Empty/whitespace query returns all. Proxies are not SSH jumps — they proxy the underlying TCP connection.",
	}, svc.ListProxies)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_proxy",
		Description: "Get a single proxy by name.",
	}, svc.GetProxy)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_proxy",
		Description: "Apply RFC 7396 JSON Merge Patch to a proxy. Patch=null deletes; object merges (or creates if name not found).",
	}, svc.UpdateProxy)

	// Session tools
	mcp.AddTool(server, &mcp.Tool{
		Name:        "login",
		Description: "Establish an interactive SSH session to a server. Use list_ssh_servers first to resolve the name (query: space-separated keywords, AND, substring on name/addr/tags). Returns {sid, server_name, sftp_available}. Direct (no via) or Pattern A (via jumphost with ssh_j=true): SSH dials target directly or through jumphost's direct-tcpip channel; sftp available. Pattern B (via jumphost with ssh_j=false): runs jumphost LoginFlow then target LoginFlow on the same PTY; sftp unavailable. On LoginFlow failure, error contains 'loginflow' and response carries login_trace for diagnosis.",
	}, svc.Login)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "run_in_session",
		Description: "Run a single command in an existing session. Session must be idle (use stat to check). Returns {output, exit_code, timed_out, truncated, total_bytes}. On timeout: auto Ctrl-C + 3s drain; if drain fails (vim/REPL/pipe block) session is closed and must re-login. If output insufficient (sentinel mismatch, interactive prompt stuck), call get_trace(sid, last_n, trunc_output=0) for raw PTY bytes. If truncated=true, use tail/head/grep or redirect to file + download. One command per call; commands joined by `\\n` are not supported — subsequent commands run but their results are discarded. Combine multiple commands with `&&` or `;` into a single line. Multi-line single-command constructs (for-loop, heredoc, if-block, line continuation `\\`) work fine.",
	}, svc.RunInSession)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "close_session",
		Description: "Close an SSH session. Forced: closes even if a command is running. Trace retained 10min for get_trace diagnosis. After this, sid is invalid for run_in_session/upload/download.",
	}, svc.CloseSession)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "stat",
		Description: "List all active sessions with state (idle/running/closed), last activity, command count, uptime, sftp_available. Use before run_in_session to avoid 'session busy' error, and before upload/download to verify sftp_available=true.",
	}, svc.Stat)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_trace",
		Description: "Retrieve command trace for a session (alive or closed within last 10min). Returns [{time, cmd, output, raw_output, exit_code, timed_out, ctrl_c_sent}] per command. raw_output contains un cleaned PTY bytes (ANSI/sentinel/\\r\\n) for debugging sentinel mismatch or interactive prompt issues. Use last_n to limit count (0=all), trunc_output to cap each Output/raw_output length (default 200, 0=no truncation for full raw bytes).",
	}, svc.GetTrace)

	// File transfer tools
	mcp.AddTool(server, &mcp.Tool{
		Name:        "upload",
		Description: "Upload a local file to the remote host via sftp. Requires sftp_available=true on the session (check stat first). Returns {bytes, timed_out}. Fails with 'sftp not available' if session's sftp channel wasn't established (server doesn't support sftp subsystem).",
	}, svc.Upload)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "download",
		Description: "Download a remote file to local via sftp. Requires sftp_available=true on the session (check stat first). Returns {bytes, timed_out}. Fails with 'sftp not available' if session's sftp channel wasn't established.",
	}, svc.Download)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "upload_dir",
		Description: "Upload a local directory tree to the remote host via sftp. Walks the local tree, creates remote dirs (MkdirAll), transfers files concurrently (default 4). Conflict policy: overwrite (default) / skip / rename. Per-file errors don't abort the transfer; aggregated in result. Requires sftp_available=true on the session.",
	}, svc.UploadDir)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "download_dir",
		Description: "Download a remote directory tree to local via sftp. Walks the remote tree (sftp.Walk), creates local dirs (os.MkdirAll), transfers files concurrently (default 4). Conflict policy: overwrite (default) / skip / rename. Per-file errors don't abort the transfer; aggregated in result. Requires sftp_available=true on the session.",
	}, svc.DownloadDir)
	return server
}

// Run 在 stdio 上启动 MCP server。阻塞直到 context 取消或 transport 出错。
func (s *Service) Run(ctx context.Context) error {
	server := NewServer(s)
	return server.Run(ctx, &mcp.StdioTransport{})
}

// --- 内部辅助 ---

// textResult 把任意值序列化为 JSON 文本，包装成 CallToolResult。
func textResult(v any) (*mcp.CallToolResult, any, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("marshal result: %w", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(data)}},
	}, nil, nil
}

// errorResult 把 error 转成 IsError=true 的 CallToolResult。不返回 Go error，
// 因为校验失败属于"工具层错误"而非"协议层错误"。
func errorResult(format string, args ...any) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
		IsError: true,
	}, nil, nil
}

// redactAuth 把 map 里的 auth.password / private_key / passphrase 删掉，用于 list_* 输出脱敏。
func redactAuth(m map[string]any) {
	if auth, ok := m["auth"].(map[string]any); ok {
		delete(auth, "password")
		delete(auth, "private_key")
		delete(auth, "passphrase")
	}
}

// entityToMap 把实体序列化为 map（便于脱敏 / 重新序列化）。
func entityToMap(v any) (map[string]any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}
