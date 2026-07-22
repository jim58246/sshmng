package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sshmng/internal/config"
	"sshmng/internal/ssh/conn"
)

// newTestService 创建一个用临时 config 文件的 Service，初始内容来自 seed。
func newTestService(t *testing.T, seed *config.Config) *Service {
	t.Helper()
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	if err := store.Save(seed); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	knownHosts := conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts"))
	return NewService(store, knownHosts, nil)
}

// resultText 提取 CallToolResult 的第一个 TextContent 文本。
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	return tc.Text
}

// parseJSON 把文本解析成任意 JSON 值。
func parseJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// --- list_ssh_servers ---

func TestListSSHServersHandlerNoQuery(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Jumphosts:    []*config.Jumphost{},
		Proxies:      []*config.Proxy{},
		Servers: []*config.SSHServer{
			{Name: "s1", Addr: "1.1.1.1:22", User: "u", Auth: config.SSHAuth{Password: "p"}},
			{Name: "s2", Addr: "2.2.2.2:22", User: "u", Auth: config.SSHAuth{Password: "p"}},
		},
	})
	res, _, err := svc.ListSSHServers(context.Background(), &mcp.CallToolRequest{}, ListArgs{})
	if err != nil {
		t.Fatalf("ListSSHServers: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", resultText(t, res))
	}
	v := parseJSON(t, resultText(t, res))
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("expected array, got %T", v)
	}
	if len(arr) != 2 {
		t.Errorf("got %d servers, want 2", len(arr))
	}
}

func TestListSSHServersHandlerWithQuery(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Jumphosts:    []*config.Jumphost{},
		Proxies:      []*config.Proxy{},
		Servers: []*config.SSHServer{
			{Name: "order-01", Addr: "1.1.1.1:22", User: "u", Auth: config.SSHAuth{Password: "p"}},
			{Name: "user-02", Addr: "2.2.2.2:22", User: "u", Auth: config.SSHAuth{Password: "p"}},
		},
	})
	res, _, err := svc.ListSSHServers(context.Background(), &mcp.CallToolRequest{}, ListArgs{Query: "order"})
	if err != nil {
		t.Fatalf("ListSSHServers: %v", err)
	}
	arr := parseJSON(t, resultText(t, res)).([]any)
	if len(arr) != 1 {
		t.Errorf("got %d servers, want 1 (matching 'order')", len(arr))
	}
	first := arr[0].(map[string]any)
	if first["name"] != "order-01" {
		t.Errorf("got name %v, want order-01", first["name"])
	}
}

func TestListSSHServersHandlerOmitsSensitiveAuth(t *testing.T) {
	// list 应返回 server 元信息；为安全考虑不返回 password / private_key 字段。
	// （v1 简化：返回除 password/private_key/passphrase 之外的所有字段。）
	svc := newTestService(t, &config.Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Jumphosts:    []*config.Jumphost{},
		Proxies:      []*config.Proxy{},
		Servers: []*config.SSHServer{
			{Name: "s", Addr: "1.1.1.1:22", User: "u", Auth: config.SSHAuth{Password: "secret"}},
		},
	})
	res, _, _ := svc.ListSSHServers(context.Background(), &mcp.CallToolRequest{}, ListArgs{})
	arr := parseJSON(t, resultText(t, res)).([]any)
	first := arr[0].(map[string]any)
	auth, ok := first["auth"].(map[string]any)
	if !ok {
		t.Fatalf("expected auth object, got %T", first["auth"])
	}
	if _, exists := auth["password"]; exists {
		t.Errorf("list result should not include password, got auth=%v", auth)
	}
}

// --- get_ssh_server ---

func TestGetSSHServerHandler(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Jumphosts:    []*config.Jumphost{},
		Proxies:      []*config.Proxy{},
		Servers: []*config.SSHServer{
			{Name: "s", Addr: "h:22", User: "u", Auth: config.SSHAuth{Password: "p"}, Tags: []string{"prod"}},
		},
	})
	res, _, err := svc.GetSSHServer(context.Background(), &mcp.CallToolRequest{}, GetArgs{Name: "s"})
	if err != nil {
		t.Fatalf("GetSSHServer: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}
	obj := parseJSON(t, resultText(t, res)).(map[string]any)
	if obj["name"] != "s" || obj["addr"] != "h:22" {
		t.Errorf("got %v, want name=s addr=h:22", obj)
	}
}

func TestGetSSHServerHandlerNotFound(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version: "1", Jumphosts: []*config.Jumphost{}, Proxies: []*config.Proxy{}, Servers: []*config.SSHServer{},
	})
	res, _, err := svc.GetSSHServer(context.Background(), &mcp.CallToolRequest{}, GetArgs{Name: "nope"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true for missing server")
	}
}

func TestGetSSHServerHandlerIncludesAuthForGet(t *testing.T) {
	// 与 list 不同，get 应包含完整 auth（用于编辑/审查），因为调用方已明确指定 name。
	svc := newTestService(t, &config.Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Jumphosts:    []*config.Jumphost{},
		Proxies:      []*config.Proxy{},
		Servers: []*config.SSHServer{
			{Name: "s", Addr: "h:22", User: "u", Auth: config.SSHAuth{Password: "secret"}},
		},
	})
	res, _, _ := svc.GetSSHServer(context.Background(), &mcp.CallToolRequest{}, GetArgs{Name: "s"})
	obj := parseJSON(t, resultText(t, res)).(map[string]any)
	auth := obj["auth"].(map[string]any)
	if auth["password"] != "secret" {
		t.Errorf("get should include password, got auth=%v", auth)
	}
}

// --- update_ssh_server ---

func TestUpdateSSHServerHandlerCreate(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version: "1", IdleTimeoutS: 300, Jumphosts: []*config.Jumphost{}, Proxies: []*config.Proxy{}, Servers: []*config.SSHServer{},
	})
	patch := map[string]any{
		"addr": "h:22",
		"user": "u",
		"auth": map[string]any{"password": "p"},
	}
	res, _, err := svc.UpdateSSHServer(context.Background(), &mcp.CallToolRequest{}, UpdateArgs{Name: "new", Patch: patch})
	if err != nil {
		t.Fatalf("UpdateSSHServer: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}
	// Verify persisted
	cfg, _ := svc.store.Load()
	s, err := cfg.GetSSHServer("new")
	if err != nil {
		t.Errorf("expected server persisted, got: %v", err)
	}
	if s.Addr != "h:22" {
		t.Errorf("Addr = %q, want h:22", s.Addr)
	}
}

func TestUpdateSSHServerHandlerPatchExisting(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Jumphosts:    []*config.Jumphost{},
		Proxies:      []*config.Proxy{},
		Servers: []*config.SSHServer{
			{Name: "s", Addr: "old:22", User: "u", Auth: config.SSHAuth{Password: "p"}},
		},
	})
	patch := map[string]any{"addr": "new:22"}
	res, _, err := svc.UpdateSSHServer(context.Background(), &mcp.CallToolRequest{}, UpdateArgs{Name: "s", Patch: patch})
	if err != nil {
		t.Fatalf("UpdateSSHServer: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}
	cfg, _ := svc.store.Load()
	s, _ := cfg.GetSSHServer("s")
	if s.Addr != "new:22" {
		t.Errorf("Addr = %q, want new:22", s.Addr)
	}
	if s.User != "u" {
		t.Errorf("User = %q, want u (unchanged)", s.User)
	}
}

func TestUpdateSSHServerHandlerDeleteWithNullPatch(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Jumphosts:    []*config.Jumphost{},
		Proxies:      []*config.Proxy{},
		Servers: []*config.SSHServer{
			{Name: "s", Addr: "h:22", User: "u", Auth: config.SSHAuth{Password: "p"}},
		},
	})
	res, _, err := svc.UpdateSSHServer(context.Background(), &mcp.CallToolRequest{}, UpdateArgs{Name: "s", Patch: nil})
	if err != nil {
		t.Fatalf("UpdateSSHServer: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}
	cfg, _ := svc.store.Load()
	if _, err := cfg.GetSSHServer("s"); err == nil {
		t.Errorf("server should be deleted")
	}
}

func TestUpdateSSHServerHandlerRejectsInvalid(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Jumphosts:    []*config.Jumphost{},
		Proxies:      []*config.Proxy{},
		Servers: []*config.SSHServer{
			{Name: "s", Addr: "h:22", User: "u", Auth: config.SSHAuth{Password: "p"}},
		},
	})
	// Patch to remove auth.password — should fail (Pattern A requires auth)
	patch := map[string]any{"auth": map[string]any{"password": nil}}
	res, _, err := svc.UpdateSSHServer(context.Background(), &mcp.CallToolRequest{}, UpdateArgs{Name: "s", Patch: patch})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true for invalid patch")
	}
	// Verify rollback (server still has password)
	cfg, _ := svc.store.Load()
	s, _ := cfg.GetSSHServer("s")
	if s.Auth.Password != "p" {
		t.Errorf("rollback failed: password = %q, want p", s.Auth.Password)
	}
}

// --- jumphosts & proxies (lighter coverage — same logic as servers) ---

func TestListJumphostsHandler(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version: "1", IdleTimeoutS: 300,
		Jumphosts: []*config.Jumphost{
			{Name: "jh1", Addr: "h1:22", User: "u", Auth: config.SSHAuth{Password: "p"}, SSHJ: true},
		},
		Proxies: []*config.Proxy{},
		Servers: []*config.SSHServer{},
	})
	res, _, err := svc.ListJumphosts(context.Background(), &mcp.CallToolRequest{}, ListArgs{Query: "jh"})
	if err != nil {
		t.Fatalf("ListJumphosts: %v", err)
	}
	arr := parseJSON(t, resultText(t, res)).([]any)
	if len(arr) != 1 {
		t.Errorf("got %d, want 1", len(arr))
	}
}

func TestGetJumphostHandlerNotFound(t *testing.T) {
	svc := newTestService(t, &config.Config{Version: "1", Jumphosts: []*config.Jumphost{}, Proxies: []*config.Proxy{}, Servers: []*config.SSHServer{}})
	res, _, _ := svc.GetJumphost(context.Background(), &mcp.CallToolRequest{}, GetArgs{Name: "nope"})
	if !res.IsError {
		t.Errorf("expected IsError=true")
	}
}

func TestUpdateJumphostHandlerCreate(t *testing.T) {
	svc := newTestService(t, &config.Config{Version: "1", Jumphosts: []*config.Jumphost{}, Proxies: []*config.Proxy{}, Servers: []*config.SSHServer{}})
	patch := map[string]any{
		"addr":  "h:22",
		"user":  "u",
		"auth":  map[string]any{"password": "p"},
		"ssh_j": true,
	}
	res, _, err := svc.UpdateJumphost(context.Background(), &mcp.CallToolRequest{}, UpdateArgs{Name: "jh", Patch: patch})
	if err != nil {
		t.Fatalf("UpdateJumphost: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected: %s", resultText(t, res))
	}
	cfg, _ := svc.store.Load()
	j, _ := cfg.GetJumphost("jh")
	if !j.SSHJ {
		t.Errorf("SSHJ = false, want true")
	}
}

func TestListProxiesHandler(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version: "1", IdleTimeoutS: 300,
		Jumphosts: []*config.Jumphost{},
		Proxies: []*config.Proxy{
			{Name: "p1", Type: config.ProxySOCKS5, Addr: "s:1080"},
		},
		Servers: []*config.SSHServer{},
	})
	res, _, err := svc.ListProxies(context.Background(), &mcp.CallToolRequest{}, ListArgs{})
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	arr := parseJSON(t, resultText(t, res)).([]any)
	if len(arr) != 1 {
		t.Errorf("got %d, want 1", len(arr))
	}
}

func TestUpdateProxyHandlerDelete(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version:   "1",
		Jumphosts: []*config.Jumphost{},
		Proxies:   []*config.Proxy{{Name: "p", Type: config.ProxyHTTP, Addr: "h:8080"}},
		Servers:   []*config.SSHServer{},
	})
	res, _, err := svc.UpdateProxy(context.Background(), &mcp.CallToolRequest{}, UpdateArgs{Name: "p", Patch: nil})
	if err != nil {
		t.Fatalf("UpdateProxy: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected: %s", resultText(t, res))
	}
	cfg, _ := svc.store.Load()
	if _, err := cfg.GetProxy("p"); err == nil {
		t.Errorf("proxy should be deleted")
	}
}

// --- NewServer integration ---

func TestNewServerRegistersAllTools(t *testing.T) {
	// 用 in-memory transport 验证 9 个工具都被注册。
	svc := newTestService(t, &config.Config{Version: "1", IdleTimeoutS: 300, Jumphosts: []*config.Jumphost{}, Proxies: []*config.Proxy{}, Servers: []*config.SSHServer{}})
	server := NewServer(svc)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	defer clientSession.Close()
	tools, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{
		"list_ssh_servers":  false,
		"get_ssh_server":    false,
		"update_ssh_server": false,
		"list_jumphosts":    false,
		"get_jumphost":      false,
		"update_jumphost":   false,
		"list_proxies":      false,
		"get_proxy":         false,
		"update_proxy":      false,
	}
	for _, tool := range tools.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q not registered", name)
		}
	}
}

func TestNewServerEndToEndList(t *testing.T) {
	// 通过 in-memory client 调用 list_ssh_servers，验证完整链路。
	svc := newTestService(t, &config.Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Jumphosts:    []*config.Jumphost{},
		Proxies:      []*config.Proxy{},
		Servers: []*config.SSHServer{
			{Name: "s", Addr: "h:22", User: "u", Auth: config.SSHAuth{Password: "p"}},
		},
	})
	server := NewServer(svc)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, _ := server.Connect(context.Background(), serverTransport, nil)
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	clientSession, _ := client.Connect(context.Background(), clientTransport, nil)
	defer clientSession.Close()
	res, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_ssh_servers",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("expected content, got empty")
	}
	tc := res.Content[0].(*mcp.TextContent)
	arr := parseJSON(t, tc.Text).([]any)
	if len(arr) != 1 {
		t.Errorf("got %d servers, want 1", len(arr))
	}
}

// --- host_key_verify round-trip ---

func TestUpdateSSHServerHostKeyVerifyPatch(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version: "1", IdleTimeoutS: 300,
		Jumphosts: []*config.Jumphost{}, Proxies: []*config.Proxy{},
		Servers: []*config.SSHServer{
			{Name: "s", Addr: "h:22", User: "u", Auth: config.SSHAuth{Password: "p"}},
		},
	})

	// 设置为 false
	res, _, err := svc.UpdateSSHServer(context.Background(), &mcp.CallToolRequest{}, UpdateArgs{
		Name:  "s",
		Patch: map[string]any{"host_key_verify": false},
	})
	if err != nil {
		t.Fatalf("UpdateSSHServer: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}

	cfg, _ := svc.store.Load()
	s, err := cfg.GetSSHServer("s")
	if err != nil {
		t.Fatalf("GetSSHServer: %v", err)
	}
	if s.HostKeyVerify == nil || *s.HostKeyVerify {
		t.Errorf("HostKeyVerify = %v, want *false", s.HostKeyVerify)
	}
	if s.HostKeyVerifyEnabled() {
		t.Errorf("HostKeyVerifyEnabled() = true, want false")
	}

	// 显式改回 true
	res, _, err = svc.UpdateSSHServer(context.Background(), &mcp.CallToolRequest{}, UpdateArgs{
		Name:  "s",
		Patch: map[string]any{"host_key_verify": true},
	})
	if err != nil {
		t.Fatalf("UpdateSSHServer re-enable: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}
	cfg, _ = svc.store.Load()
	s, _ = cfg.GetSSHServer("s")
	if s.HostKeyVerify == nil || !*s.HostKeyVerify {
		t.Errorf("HostKeyVerify = %v, want *true after re-enable", s.HostKeyVerify)
	}
	if !s.HostKeyVerifyEnabled() {
		t.Errorf("HostKeyVerifyEnabled() = false, want true")
	}

	// null → 字段回到 nil（默认 on 语义恢复）
	// Go 标准 *bool unmarshal 把 null 和缺字段都解为 nil；这里 pin 住 RFC 7396 null→nil→default-on 路径。
	res, _, err = svc.UpdateSSHServer(context.Background(), &mcp.CallToolRequest{}, UpdateArgs{
		Name:  "s",
		Patch: map[string]any{"host_key_verify": nil},
	})
	if err != nil {
		t.Fatalf("UpdateSSHServer null patch: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}
	cfg, _ = svc.store.Load()
	s, _ = cfg.GetSSHServer("s")
	if s.HostKeyVerify != nil {
		t.Errorf("HostKeyVerify = %v, want nil after null patch", s.HostKeyVerify)
	}
	if !s.HostKeyVerifyEnabled() {
		t.Errorf("HostKeyVerifyEnabled() = false, want true (default-on restored)")
	}
}

func TestUpdateJumphostHostKeyVerifyPatch(t *testing.T) {
	svc := newTestService(t, &config.Config{
		Version: "1", IdleTimeoutS: 300,
		Jumphosts: []*config.Jumphost{
			{Name: "j", Addr: "h:22", User: "u", Auth: config.SSHAuth{Password: "p"}, SSHJ: true},
		},
		Proxies: []*config.Proxy{}, Servers: []*config.SSHServer{},
	})

	res, _, err := svc.UpdateJumphost(context.Background(), &mcp.CallToolRequest{}, UpdateArgs{
		Name:  "j",
		Patch: map[string]any{"host_key_verify": false},
	})
	if err != nil {
		t.Fatalf("UpdateJumphost: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, res))
	}

	cfg, _ := svc.store.Load()
	j, err := cfg.GetJumphost("j")
	if err != nil {
		t.Fatalf("GetJumphost: %v", err)
	}
	if j.HostKeyVerify == nil || *j.HostKeyVerify {
		t.Errorf("HostKeyVerify = %v, want *false", j.HostKeyVerify)
	}
	if j.HostKeyVerifyEnabled() {
		t.Errorf("HostKeyVerifyEnabled() = true, want false")
	}
}
