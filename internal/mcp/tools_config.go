package mcp

import (
	"bytes"
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

	logger := s.sessionLogger(req, "")
	patchJSON, err := marshalPatch(args.Patch)
	if err != nil {
		logger.Warn("update ssh server failed: marshal patch", "name", args.Name, "err", err.Error())
		return errorResult("marshal patch: %v", err)
	}
	logger.Debug("update ssh server", "name", args.Name, "patch", string(patchJSON))
	cfg, err := s.store.Load()
	if err != nil {
		logger.Warn("update ssh server failed: load config", "name", args.Name, "err", err.Error())
		return errorResult("load config: %v", err)
	}
	_, getErr := cfg.GetSSHServer(args.Name)
	existed := getErr == nil
	if err := cfg.UpdateSSHServer(args.Name, patchJSON); err != nil {
		logger.Warn("update ssh server failed", "name", args.Name, "err", err.Error())
		return errorResult("update ssh server: %v", err)
	}
	if err := s.store.Save(cfg); err != nil {
		logger.Warn("update ssh server failed: save config", "name", args.Name, "err", err.Error())
		return errorResult("save config: %v", err)
	}
	logger.Info("ssh server "+updateOp(patchJSON, existed), "name", args.Name)
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

	logger := s.sessionLogger(req, "")
	patchJSON, err := marshalPatch(args.Patch)
	if err != nil {
		logger.Warn("update jumphost failed: marshal patch", "name", args.Name, "err", err.Error())
		return errorResult("marshal patch: %v", err)
	}
	logger.Debug("update jumphost", "name", args.Name, "patch", string(patchJSON))
	cfg, err := s.store.Load()
	if err != nil {
		logger.Warn("update jumphost failed: load config", "name", args.Name, "err", err.Error())
		return errorResult("load config: %v", err)
	}
	_, getErr := cfg.GetJumphost(args.Name)
	existed := getErr == nil
	if err := cfg.UpdateJumphost(args.Name, patchJSON); err != nil {
		logger.Warn("update jumphost failed", "name", args.Name, "err", err.Error())
		return errorResult("update jumphost: %v", err)
	}
	if err := s.store.Save(cfg); err != nil {
		logger.Warn("update jumphost failed: save config", "name", args.Name, "err", err.Error())
		return errorResult("save config: %v", err)
	}
	logger.Info("jumphost "+updateOp(patchJSON, existed), "name", args.Name)
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

	logger := s.sessionLogger(req, "")
	patchJSON, err := marshalPatch(args.Patch)
	if err != nil {
		logger.Warn("update proxy failed: marshal patch", "name", args.Name, "err", err.Error())
		return errorResult("marshal patch: %v", err)
	}
	logger.Debug("update proxy", "name", args.Name, "patch", string(patchJSON))
	cfg, err := s.store.Load()
	if err != nil {
		logger.Warn("update proxy failed: load config", "name", args.Name, "err", err.Error())
		return errorResult("load config: %v", err)
	}
	_, getErr := cfg.GetProxy(args.Name)
	existed := getErr == nil
	if err := cfg.UpdateProxy(args.Name, patchJSON); err != nil {
		logger.Warn("update proxy failed", "name", args.Name, "err", err.Error())
		return errorResult("update proxy: %v", err)
	}
	if err := s.store.Save(cfg); err != nil {
		logger.Warn("update proxy failed: save config", "name", args.Name, "err", err.Error())
		return errorResult("save config: %v", err)
	}
	logger.Info("proxy "+updateOp(patchJSON, existed), "name", args.Name)
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

// isDeletePatch 判断 patch 是否为顶层 null（删除信号）。
func isDeletePatch(patch json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(patch), []byte("null"))
}

// updateOp 根据 patch 和 existed（操作前实体是否已存在）返回操作类型：created/updated/deleted。
func updateOp(patch json.RawMessage, existed bool) string {
	if isDeletePatch(patch) {
		return "deleted"
	}
	if !existed {
		return "created"
	}
	return "updated"
}
