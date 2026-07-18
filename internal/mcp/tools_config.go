package mcp

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- SSHServer ---

// ListSSHServers: load → query → 脱敏 auth → JSON 数组返回。
func (s *Service) ListSSHServers(ctx context.Context, req *mcp.CallToolRequest, args ListArgs) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, err := s.store.Load()
	if err != nil {
		return errorResult("load config: %v", err)
	}
	servers := cfg.ListSSHServers(args.Query)
	out := make([]map[string]any, 0, len(servers))
	for _, srv := range servers {
		m, err := entityToMap(srv)
		if err != nil {
			return errorResult("marshal server %q: %v", srv.Name, err)
		}
		redactAuth(m)
		out = append(out, m)
	}
	return textResult(out)
}

// GetSSHServer: load → find → JSON 对象（含完整 auth）。
func (s *Service) GetSSHServer(ctx context.Context, req *mcp.CallToolRequest, args GetArgs) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, err := s.store.Load()
	if err != nil {
		return errorResult("load config: %v", err)
	}
	srv, err := cfg.GetSSHServer(args.Name)
	if err != nil {
		return errorResult("%v", err)
	}
	return textResult(srv)
}

// UpdateSSHServer: load → patch → validate → save。失败时 config 层已回滚，handler 只需报错。
func (s *Service) UpdateSSHServer(ctx context.Context, req *mcp.CallToolRequest, args UpdateArgs) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	patchJSON, err := marshalPatch(args.Patch)
	if err != nil {
		return errorResult("marshal patch: %v", err)
	}
	cfg, err := s.store.Load()
	if err != nil {
		return errorResult("load config: %v", err)
	}
	if err := cfg.UpdateSSHServer(args.Name, patchJSON); err != nil {
		return errorResult("update ssh server: %v", err)
	}
	if err := s.store.Save(cfg); err != nil {
		return errorResult("save config: %v", err)
	}
	// 返回更新后的实体（含完整 auth）
	srv, err := cfg.GetSSHServer(args.Name)
	if err != nil {
		// 删除场景：实体已不存在，返回简短确认
		return textResult(map[string]any{"name": args.Name, "deleted": true})
	}
	return textResult(srv)
}

// --- Jumphost ---

func (s *Service) ListJumphosts(ctx context.Context, req *mcp.CallToolRequest, args ListArgs) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, err := s.store.Load()
	if err != nil {
		return errorResult("load config: %v", err)
	}
	items := cfg.ListJumphosts(args.Query)
	out := make([]map[string]any, 0, len(items))
	for _, j := range items {
		m, err := entityToMap(j)
		if err != nil {
			return errorResult("marshal jumphost %q: %v", j.Name, err)
		}
		redactAuth(m)
		out = append(out, m)
	}
	return textResult(out)
}

func (s *Service) GetJumphost(ctx context.Context, req *mcp.CallToolRequest, args GetArgs) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, err := s.store.Load()
	if err != nil {
		return errorResult("load config: %v", err)
	}
	j, err := cfg.GetJumphost(args.Name)
	if err != nil {
		return errorResult("%v", err)
	}
	return textResult(j)
}

func (s *Service) UpdateJumphost(ctx context.Context, req *mcp.CallToolRequest, args UpdateArgs) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	patchJSON, err := marshalPatch(args.Patch)
	if err != nil {
		return errorResult("marshal patch: %v", err)
	}
	cfg, err := s.store.Load()
	if err != nil {
		return errorResult("load config: %v", err)
	}
	if err := cfg.UpdateJumphost(args.Name, patchJSON); err != nil {
		return errorResult("update jumphost: %v", err)
	}
	if err := s.store.Save(cfg); err != nil {
		return errorResult("save config: %v", err)
	}
	j, err := cfg.GetJumphost(args.Name)
	if err != nil {
		return textResult(map[string]any{"name": args.Name, "deleted": true})
	}
	return textResult(j)
}

// --- Proxy ---

func (s *Service) ListProxies(ctx context.Context, req *mcp.CallToolRequest, args ListArgs) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, err := s.store.Load()
	if err != nil {
		return errorResult("load config: %v", err)
	}
	items := cfg.ListProxies(args.Query)
	out := make([]map[string]any, 0, len(items))
	for _, p := range items {
		m, err := entityToMap(p)
		if err != nil {
			return errorResult("marshal proxy %q: %v", p.Name, err)
		}
		out = append(out, m)
	}
	return textResult(out)
}

func (s *Service) GetProxy(ctx context.Context, req *mcp.CallToolRequest, args GetArgs) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, err := s.store.Load()
	if err != nil {
		return errorResult("load config: %v", err)
	}
	p, err := cfg.GetProxy(args.Name)
	if err != nil {
		return errorResult("%v", err)
	}
	return textResult(p)
}

func (s *Service) UpdateProxy(ctx context.Context, req *mcp.CallToolRequest, args UpdateArgs) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	patchJSON, err := marshalPatch(args.Patch)
	if err != nil {
		return errorResult("marshal patch: %v", err)
	}
	cfg, err := s.store.Load()
	if err != nil {
		return errorResult("load config: %v", err)
	}
	if err := cfg.UpdateProxy(args.Name, patchJSON); err != nil {
		return errorResult("update proxy: %v", err)
	}
	if err := s.store.Save(cfg); err != nil {
		return errorResult("save config: %v", err)
	}
	p, err := cfg.GetProxy(args.Name)
	if err != nil {
		return textResult(map[string]any{"name": args.Name, "deleted": true})
	}
	return textResult(p)
}

// marshalPatch 把 UpdateArgs.Patch（any）转回 json.RawMessage，
// 供 config.Update* 消费。nil → "null"（删除），其他值 → 原样序列化。
func marshalPatch(p any) (json.RawMessage, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}
