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
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sshmng/internal/config"
	"sshmng/internal/ssh"
)

// Service 是工具 handler 的宿主，封装 store / session manager / known_hosts 与并发保护。
type Service struct {
	store      *config.Store
	knownHosts *ssh.KnownHostsStore
	manager    *ssh.Manager
	mu         sync.Mutex
}

// NewService 创建一个绑定到 store + knownHosts 的 Service。
// 内部创建 session Manager。
func NewService(store *config.Store, knownHosts *ssh.KnownHostsStore) *Service {
	return &Service{
		store:      store,
		knownHosts: knownHosts,
		manager:    ssh.NewManager(),
	}
}

// ListArgs 是 list_* 工具的入参。Query 为空时返回全部。
type ListArgs struct {
	Query string `json:"query,omitempty" jsonschema:"optional substring to filter by name/addr/tags (case-insensitive)"`
}

// GetArgs 是 get_* 工具的入参。
type GetArgs struct {
	Name string `json:"name" jsonschema:"entity name"`
}

// UpdateArgs 是 update_* 工具的入参。Patch 是 RFC 7396 JSON Merge Patch：
// null 表示删除整个实体，object 表示合并到现有实体（不存在则创建）。
type UpdateArgs struct {
	Name  string `json:"name" jsonschema:"entity name; if not exist, create"`
	Patch any    `json:"patch" jsonschema:"RFC 7396 JSON Merge Patch; null deletes the entity, object merges"`
}

// NewServer 创建 MCP server 并注册 9 个 CRUD 工具（3 类资源 × 3 个操作）。
func NewServer(svc *Service) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "sshmng", Version: "v1"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_ssh_servers",
		Description: "List SSH servers, optionally filtered by query (substring match on name/addr/tags, case-insensitive). Sensitive auth fields are redacted.",
	}, svc.ListSSHServers)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_ssh_server",
		Description: "Get a single SSH server by name, including full auth.",
	}, svc.GetSSHServer)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_ssh_server",
		Description: "Apply RFC 7396 JSON Merge Patch to an SSH server. Patch=null deletes; object merges (or creates if name not found).",
	}, svc.UpdateSSHServer)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_jumphosts",
		Description: "List jumphosts, optionally filtered by query.",
	}, svc.ListJumphosts)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_jumphost",
		Description: "Get a single jumphost by name.",
	}, svc.GetJumphost)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_jumphost",
		Description: "Apply RFC 7396 JSON Merge Patch to a jumphost.",
	}, svc.UpdateJumphost)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_proxies",
		Description: "List proxies, optionally filtered by query.",
	}, svc.ListProxies)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_proxy",
		Description: "Get a single proxy by name.",
	}, svc.GetProxy)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_proxy",
		Description: "Apply RFC 7396 JSON Merge Patch to a proxy.",
	}, svc.UpdateProxy)

	// Session tools (phase 2)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "login",
		Description: "Establish an interactive SSH session to a server. Returns sid for use with run_in_session / close_session. v1 phase 2: direct connections only (no jumphost, no login_flow).",
	}, svc.Login)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "run_in_session",
		Description: "Run a single command in an existing session. Returns output (cleaned), exit_code, timed_out, truncated, total_bytes. Session must be idle.",
	}, svc.RunInSession)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "close_session",
		Description: "Close an SSH session. Forced: closes even if a command is running.",
	}, svc.CloseSession)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "stat",
		Description: "List all active sessions with their state (idle/running/closed), last activity, command count, uptime.",
	}, svc.Stat)
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
