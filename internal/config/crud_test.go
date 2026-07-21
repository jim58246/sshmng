package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func makeConfig(servers ...*SSHServer) *Config {
	return &Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Jumphosts:    []*Jumphost{},
		Proxies:      []*Proxy{},
		Servers:      servers,
	}
}

func serverNames(servers []*SSHServer) []string {
	names := []string{}
	for _, s := range servers {
		names = append(names, s.Name)
	}
	return names
}

// --- ListSSHServers ---

func TestListSSHServersNoQueryReturnsAll(t *testing.T) {
	cfg := makeConfig(
		&SSHServer{Name: "s1", Addr: "1.1.1.1:22", User: "u", Auth: SSHAuth{Password: "p"}},
		&SSHServer{Name: "s2", Addr: "2.2.2.2:22", User: "u", Auth: SSHAuth{Password: "p"}},
	)
	got := cfg.ListSSHServers("")
	if len(got) != 2 {
		t.Errorf("got %d servers, want 2: %v", len(got), serverNames(got))
	}
}

func TestListSSHServersQueryByName(t *testing.T) {
	cfg := makeConfig(
		&SSHServer{Name: "order-01", Addr: "1.1.1.1:22", User: "u", Auth: SSHAuth{Password: "p"}},
		&SSHServer{Name: "user-02", Addr: "2.2.2.2:22", User: "u", Auth: SSHAuth{Password: "p"}},
	)
	got := cfg.ListSSHServers("order")
	if len(got) != 1 || got[0].Name != "order-01" {
		t.Errorf("got %v, want [order-01]", serverNames(got))
	}
}

func TestListSSHServersQueryByAddr(t *testing.T) {
	cfg := makeConfig(
		&SSHServer{Name: "s1", Addr: "10.0.0.1:22", User: "u", Auth: SSHAuth{Password: "p"}},
		&SSHServer{Name: "s2", Addr: "10.0.0.2:22", User: "u", Auth: SSHAuth{Password: "p"}},
	)
	got := cfg.ListSSHServers("10.0.0.2")
	if len(got) != 1 || got[0].Name != "s2" {
		t.Errorf("got %v, want [s2]", serverNames(got))
	}
}

func TestListSSHServersQueryByTag(t *testing.T) {
	cfg := makeConfig(
		&SSHServer{Name: "s1", Addr: "1.1.1.1:22", User: "u", Auth: SSHAuth{Password: "p"}, Tags: []string{"生产", "华东"}},
		&SSHServer{Name: "s2", Addr: "2.2.2.2:22", User: "u", Auth: SSHAuth{Password: "p"}, Tags: []string{"测试", "华北"}},
	)
	got := cfg.ListSSHServers("生产")
	if len(got) != 1 || got[0].Name != "s1" {
		t.Errorf("got %v, want [s1]", serverNames(got))
	}
}

func TestListSSHServersQueryCaseInsensitive(t *testing.T) {
	cfg := makeConfig(
		&SSHServer{Name: "Order-01", Addr: "1.1.1.1:22", User: "u", Auth: SSHAuth{Password: "p"}},
	)
	got := cfg.ListSSHServers("ORDER")
	if len(got) != 1 {
		t.Errorf("got %v, want [Order-01] (case-insensitive)", serverNames(got))
	}
}

func TestListSSHServersQueryNoMatch(t *testing.T) {
	cfg := makeConfig(
		&SSHServer{Name: "s1", Addr: "1.1.1.1:22", User: "u", Auth: SSHAuth{Password: "p"}},
	)
	got := cfg.ListSSHServers("nonexistent")
	if len(got) != 0 {
		t.Errorf("got %v, want []", serverNames(got))
	}
}

// --- 多关键字 AND 搜索 ---

// 多关键字全部命中才返回（AND 语义）。
func TestListSSHServersMultiKeywordAND(t *testing.T) {
	cfg := makeConfig(
		&SSHServer{Name: "web-prod-01", Addr: "10.0.0.1:22", User: "u", Auth: SSHAuth{Password: "p"}, Tags: []string{"prod", "web"}},
		&SSHServer{Name: "db-prod-01", Addr: "10.0.0.2:22", User: "u", Auth: SSHAuth{Password: "p"}, Tags: []string{"prod"}},
		&SSHServer{Name: "web-cache-01", Addr: "10.0.0.3:22", User: "u", Auth: SSHAuth{Password: "p"}, Tags: []string{"web"}},
	)
	got := cfg.ListSSHServers("prod web")
	if len(got) != 1 || got[0].Name != "web-prod-01" {
		t.Errorf("got %v, want [web-prod-01]", serverNames(got))
	}
}

// 关键字跨字段匹配：一个命中 tag、一个命中 addr。
func TestListSSHServersMultiKeywordAcrossFields(t *testing.T) {
	cfg := makeConfig(
		&SSHServer{Name: "s1", Addr: "10.0.0.1:22", User: "u", Auth: SSHAuth{Password: "p"}, Tags: []string{"prod"}},
		&SSHServer{Name: "s2", Addr: "192.168.0.1:22", User: "u", Auth: SSHAuth{Password: "p"}, Tags: []string{"prod"}},
	)
	got := cfg.ListSSHServers("prod 10.0")
	if len(got) != 1 || got[0].Name != "s1" {
		t.Errorf("got %v, want [s1]", serverNames(got))
	}
}

// 多关键字大小写不敏感。
func TestListSSHServersMultiKeywordCaseInsensitive(t *testing.T) {
	cfg := makeConfig(
		&SSHServer{Name: "web-prod-01", Addr: "1.1.1.1:22", User: "u", Auth: SSHAuth{Password: "p"}},
	)
	got := cfg.ListSSHServers("PROD WEB")
	if len(got) != 1 {
		t.Errorf("got %v, want [web-prod-01] (case-insensitive)", serverNames(got))
	}
}

// 多余空格 / 前后空格应被压缩，等价单空格。
func TestListSSHServersMultiKeywordExtraSpaces(t *testing.T) {
	cfg := makeConfig(
		&SSHServer{Name: "web-prod-01", Addr: "1.1.1.1:22", User: "u", Auth: SSHAuth{Password: "p"}},
		&SSHServer{Name: "db-prod-01", Addr: "2.2.2.2:22", User: "u", Auth: SSHAuth{Password: "p"}},
	)
	got := cfg.ListSSHServers("  prod   web  ")
	if len(got) != 1 || got[0].Name != "web-prod-01" {
		t.Errorf("got %v, want [web-prod-01] (extra spaces collapsed)", serverNames(got))
	}
}

// 任一关键字未命中 → 整体不匹配。
func TestListSSHServersMultiKeywordNoMatch(t *testing.T) {
	cfg := makeConfig(
		&SSHServer{Name: "web-prod-01", Addr: "1.1.1.1:22", User: "u", Auth: SSHAuth{Password: "p"}},
	)
	got := cfg.ListSSHServers("prod web db")
	if len(got) != 0 {
		t.Errorf("got %v, want [] (no server has all 3 keywords)", serverNames(got))
	}
}

// 纯空白 query 视为空，返回全部。
func TestListSSHServersWhitespaceOnlyQueryReturnsAll(t *testing.T) {
	cfg := makeConfig(
		&SSHServer{Name: "s1", Addr: "1.1.1.1:22", User: "u", Auth: SSHAuth{Password: "p"}},
		&SSHServer{Name: "s2", Addr: "2.2.2.2:22", User: "u", Auth: SSHAuth{Password: "p"}},
	)
	got := cfg.ListSSHServers("   ")
	if len(got) != 2 {
		t.Errorf("got %d, want 2 (whitespace-only query = return all)", len(got))
	}
}

// --- ListJumphosts / ListProxies ---

func TestListJumphostsQuery(t *testing.T) {
	cfg := &Config{
		Version: "1",
		Jumphosts: []*Jumphost{
			{Name: "jh-prod", Addr: "h1:22", User: "u", Auth: SSHAuth{Password: "p"}, SSHJ: true},
			{Name: "jh-test", Addr: "h2:22", User: "u", Auth: SSHAuth{Password: "p"}, SSHJ: true},
		},
		Proxies: []*Proxy{},
		Servers: []*SSHServer{},
	}
	got := cfg.ListJumphosts("prod")
	if len(got) != 1 || got[0].Name != "jh-prod" {
		names := []string{}
		for _, j := range got {
			names = append(names, j.Name)
		}
		t.Errorf("got %v, want [jh-prod]", names)
	}
}

func TestListProxiesQuery(t *testing.T) {
	cfg := &Config{
		Version:   "1",
		Jumphosts: []*Jumphost{},
		Proxies: []*Proxy{
			{Name: "corp-socks5", Type: ProxySOCKS5, Addr: "s:1080"},
			{Name: "test-http", Type: ProxyHTTP, Addr: "h:8080"},
		},
		Servers: []*SSHServer{},
	}
	got := cfg.ListProxies("socks")
	if len(got) != 1 || got[0].Name != "corp-socks5" {
		names := []string{}
		for _, p := range got {
			names = append(names, p.Name)
		}
		t.Errorf("got %v, want [corp-socks5]", names)
	}
}

// --- Get ---

func TestGetSSHServerFound(t *testing.T) {
	cfg := makeConfig(&SSHServer{Name: "s", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"}})
	s, err := cfg.GetSSHServer("s")
	if err != nil {
		t.Fatalf("GetSSHServer: %v", err)
	}
	if s.Addr != "h:22" {
		t.Errorf("Addr = %q, want h:22", s.Addr)
	}
}

func TestGetSSHServerNotFound(t *testing.T) {
	cfg := makeConfig(&SSHServer{Name: "s", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"}})
	_, err := cfg.GetSSHServer("nope")
	if err == nil {
		t.Errorf("expected error for missing server")
	}
}

func TestGetJumphostNotFound(t *testing.T) {
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{}, Proxies: []*Proxy{}, Servers: []*SSHServer{}}
	_, err := cfg.GetJumphost("nope")
	if err == nil {
		t.Errorf("expected error for missing jumphost")
	}
}

func TestGetProxyNotFound(t *testing.T) {
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{}, Proxies: []*Proxy{}, Servers: []*SSHServer{}}
	_, err := cfg.GetProxy("nope")
	if err == nil {
		t.Errorf("expected error for missing proxy")
	}
}

// --- UpdateSSHServer ---

func TestUpdateSSHServerCreate(t *testing.T) {
	cfg := makeConfig()
	patch := json.RawMessage(`{"addr":"h:22","user":"u","auth":{"password":"p"}}`)
	if err := cfg.UpdateSSHServer("new", patch); err != nil {
		t.Fatalf("UpdateSSHServer: %v", err)
	}
	s, err := cfg.GetSSHServer("new")
	if err != nil {
		t.Fatalf("GetSSHServer: %v", err)
	}
	if s.Addr != "h:22" || s.Auth.Password != "p" {
		t.Errorf("server = %+v, want addr=h:22 password=p", s)
	}
}

func TestUpdateSSHServerPatchField(t *testing.T) {
	cfg := makeConfig(&SSHServer{Name: "s", Addr: "old:22", User: "u", Auth: SSHAuth{Password: "p"}})
	patch := json.RawMessage(`{"addr":"new:22"}`)
	if err := cfg.UpdateSSHServer("s", patch); err != nil {
		t.Fatalf("UpdateSSHServer: %v", err)
	}
	s, _ := cfg.GetSSHServer("s")
	if s.Addr != "new:22" {
		t.Errorf("Addr = %q, want new:22", s.Addr)
	}
	if s.User != "u" {
		t.Errorf("User = %q, want u (unchanged)", s.User)
	}
	if s.Auth.Password != "p" {
		t.Errorf("Auth.Password = %q, want p (unchanged)", s.Auth.Password)
	}
}

func TestUpdateSSHServerDeleteField(t *testing.T) {
	cfg := makeConfig(&SSHServer{
		Name: "s", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"},
		Tags: []string{"a", "b"},
	})
	patch := json.RawMessage(`{"tags":null}`)
	if err := cfg.UpdateSSHServer("s", patch); err != nil {
		t.Fatalf("UpdateSSHServer: %v", err)
	}
	s, _ := cfg.GetSSHServer("s")
	if len(s.Tags) != 0 {
		t.Errorf("Tags = %v, want empty", s.Tags)
	}
}

func TestUpdateSSHServerDeleteEntire(t *testing.T) {
	cfg := makeConfig(&SSHServer{Name: "s", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"}})
	if err := cfg.UpdateSSHServer("s", json.RawMessage("null")); err != nil {
		t.Fatalf("UpdateSSHServer: %v", err)
	}
	if _, err := cfg.GetSSHServer("s"); err == nil {
		t.Errorf("server should be deleted")
	}
}

func TestUpdateSSHServerDeleteNotFoundIsNoop(t *testing.T) {
	cfg := makeConfig()
	// Deleting a non-existent server should not error.
	if err := cfg.UpdateSSHServer("ghost", json.RawMessage("null")); err != nil {
		t.Errorf("expected no error for deleting non-existent server, got: %v", err)
	}
}

func TestUpdateSSHServerMapMergeByKeys(t *testing.T) {
	// LoginFlow is a map — patch should merge by key, not replace.
	cfg := makeConfig(&SSHServer{
		Name: "s", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"},
		LoginFlow: map[string]LoginAction{
			"a": {Expects: []Expect{{Pattern: "x", Next: "success"}}},
		},
		LoginEntry: "a",
	})
	patch := json.RawMessage(`{"login_flow":{"b":{"expects":[{"pattern":"y","next":"success"}]}}}`)
	if err := cfg.UpdateSSHServer("s", patch); err != nil {
		t.Fatalf("UpdateSSHServer: %v", err)
	}
	s, _ := cfg.GetSSHServer("s")
	if len(s.LoginFlow) != 2 {
		t.Errorf("LoginFlow has %d keys, want 2: %v", len(s.LoginFlow), s.LoginFlow)
	}
	if _, ok := s.LoginFlow["a"]; !ok {
		t.Errorf("LoginFlow should still have 'a'")
	}
	if _, ok := s.LoginFlow["b"]; !ok {
		t.Errorf("LoginFlow should have 'b' after patch")
	}
}

func TestUpdateSSHServerMapReplaceKey(t *testing.T) {
	// Patching an existing LoginFlow key replaces that action entirely.
	cfg := makeConfig(&SSHServer{
		Name: "s", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"},
		LoginFlow: map[string]LoginAction{
			"a": {Expects: []Expect{{Pattern: "x", Next: "success"}}},
		},
		LoginEntry: "a",
	})
	patch := json.RawMessage(`{"login_flow":{"a":{"expects":[{"pattern":"z","next":"success"}]}}}`)
	if err := cfg.UpdateSSHServer("s", patch); err != nil {
		t.Fatalf("UpdateSSHServer: %v", err)
	}
	s, _ := cfg.GetSSHServer("s")
	if exp := s.LoginFlow["a"].Expects[0].Pattern; exp != "z" {
		t.Errorf("LoginFlow[a].Expects[0].Pattern = %q, want z", exp)
	}
}

func TestUpdateSSHServerArrayReplace(t *testing.T) {
	// Tags is an array — patch replaces entirely, not append.
	cfg := makeConfig(&SSHServer{
		Name: "s", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"},
		Tags: []string{"a", "b"},
	})
	patch := json.RawMessage(`{"tags":["x","y","z"]}`)
	if err := cfg.UpdateSSHServer("s", patch); err != nil {
		t.Fatalf("UpdateSSHServer: %v", err)
	}
	s, _ := cfg.GetSSHServer("s")
	if len(s.Tags) != 3 || s.Tags[0] != "x" || s.Tags[1] != "y" || s.Tags[2] != "z" {
		t.Errorf("Tags = %v, want [x y z]", s.Tags)
	}
}

func TestUpdateSSHServerForcesNameParameter(t *testing.T) {
	// Patch with mismatched name field — name parameter wins.
	cfg := makeConfig()
	patch := json.RawMessage(`{"name":"wrong","addr":"h:22","user":"u","auth":{"password":"p"}}`)
	if err := cfg.UpdateSSHServer("right", patch); err != nil {
		t.Fatalf("UpdateSSHServer: %v", err)
	}
	s, err := cfg.GetSSHServer("right")
	if err != nil {
		t.Fatalf("GetSSHServer(right): %v", err)
	}
	if s.Name != "right" {
		t.Errorf("Name = %q, want right (forced from parameter)", s.Name)
	}
	if _, err := cfg.GetSSHServer("wrong"); err == nil {
		t.Errorf("server should not be stored under 'wrong' name")
	}
}

func TestUpdateSSHServerRejectsInvalidAndRollsBack(t *testing.T) {
	// Remove Auth.Password from a Pattern A server — should fail validation.
	cfg := makeConfig(&SSHServer{
		Name: "s", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"},
	})
	patch := json.RawMessage(`{"auth":{"password":null}}`)
	err := cfg.UpdateSSHServer("s", patch)
	if err == nil {
		t.Fatalf("expected error: pattern A requires auth")
	}
	// Verify rollback
	s, _ := cfg.GetSSHServer("s")
	if s.Auth.Password != "p" {
		t.Errorf("rollback failed: Auth.Password = %q, want p", s.Auth.Password)
	}
}

func TestUpdateSSHServerPatchViaRepoints(t *testing.T) {
	jh1 := &Jumphost{Name: "jh1", Addr: "h1:22", User: "u", Auth: SSHAuth{Password: "p"}, SSHJ: true}
	jh2 := &Jumphost{Name: "jh2", Addr: "h2:22", User: "u", Auth: SSHAuth{Password: "p"}, SSHJ: true}
	cfg := &Config{
		Version: "1", IdleTimeoutS: 300,
		Jumphosts: []*Jumphost{jh1, jh2},
		Proxies:   []*Proxy{},
		Servers:   []*SSHServer{{Name: "s", Addr: "t:22", User: "u", Auth: SSHAuth{Password: "p"}, Via: jh1}},
	}
	patch := json.RawMessage(`{"via":"jh2"}`)
	if err := cfg.UpdateSSHServer("s", patch); err != nil {
		t.Fatalf("UpdateSSHServer: %v", err)
	}
	s, _ := cfg.GetSSHServer("s")
	if s.Via != jh2 {
		t.Errorf("Via = %v, want jh2", s.Via)
	}
}

func TestUpdateSSHServerPatchViaUnknownJumphostFails(t *testing.T) {
	jh1 := &Jumphost{Name: "jh1", Addr: "h1:22", User: "u", Auth: SSHAuth{Password: "p"}, SSHJ: true}
	cfg := &Config{
		Version: "1", IdleTimeoutS: 300,
		Jumphosts: []*Jumphost{jh1},
		Proxies:   []*Proxy{},
		Servers:   []*SSHServer{{Name: "s", Addr: "t:22", User: "u", Auth: SSHAuth{Password: "p"}, Via: jh1}},
	}
	patch := json.RawMessage(`{"via":"no-such-jh"}`)
	err := cfg.UpdateSSHServer("s", patch)
	if err == nil {
		t.Fatalf("expected error: unknown jumphost reference")
	}
	// Verify rollback — Via should still point to jh1
	s, _ := cfg.GetSSHServer("s")
	if s.Via != jh1 {
		t.Errorf("rollback failed: Via = %v, want jh1", s.Via)
	}
}

// --- UpdateJumphost ---

func TestUpdateJumphostCreate(t *testing.T) {
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{}, Proxies: []*Proxy{}, Servers: []*SSHServer{}}
	patch := json.RawMessage(`{"addr":"h:22","user":"u","auth":{"password":"p"},"ssh_j":true}`)
	if err := cfg.UpdateJumphost("jh", patch); err != nil {
		t.Fatalf("UpdateJumphost: %v", err)
	}
	j, err := cfg.GetJumphost("jh")
	if err != nil {
		t.Fatalf("GetJumphost: %v", err)
	}
	if !j.SSHJ {
		t.Errorf("SSHJ = false, want true")
	}
}

func TestUpdateJumphostDeleteEntire(t *testing.T) {
	cfg := &Config{
		Version: "1",
		Jumphosts: []*Jumphost{
			{Name: "jh", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"}, SSHJ: true},
		},
		Proxies: []*Proxy{},
		Servers: []*SSHServer{},
	}
	if err := cfg.UpdateJumphost("jh", json.RawMessage("null")); err != nil {
		t.Fatalf("UpdateJumphost: %v", err)
	}
	if _, err := cfg.GetJumphost("jh"); err == nil {
		t.Errorf("jumphost should be deleted")
	}
}

func TestUpdateJumphostRejectsDeletingReferenced(t *testing.T) {
	jh := &Jumphost{Name: "jh", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"}, SSHJ: true}
	srv := &SSHServer{Name: "s", Addr: "t:22", User: "u", Auth: SSHAuth{Password: "p"}, Via: jh}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh}, Proxies: []*Proxy{}, Servers: []*SSHServer{srv}}
	err := cfg.UpdateJumphost("jh", json.RawMessage("null"))
	if err == nil {
		t.Errorf("expected error: cannot delete jumphost referenced by server")
	}
	if _, err := cfg.GetJumphost("jh"); err != nil {
		t.Errorf("jumphost should still exist after rejected delete: %v", err)
	}
}

func TestUpdateJumphostRejectsInvalidAndRollsBack(t *testing.T) {
	// SSHJ=true + non-empty LoginFlow → invalid.
	cfg := &Config{
		Version: "1",
		Jumphosts: []*Jumphost{
			{Name: "jh", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"}, SSHJ: true},
		},
		Proxies: []*Proxy{},
		Servers: []*SSHServer{},
	}
	patch := json.RawMessage(`{"login_flow":{"a":{"name":"a","expects":[{"pattern":"x","next":"success"}]}}, "login_entry":"a"}`)
	err := cfg.UpdateJumphost("jh", patch)
	if err == nil {
		t.Fatalf("expected error: ssh_j=true requires empty login_flow")
	}
	if !strings.Contains(err.Error(), "ssh_j=true") {
		t.Errorf("error should mention ssh_j=true, got: %v", err)
	}
	// Verify rollback
	j, _ := cfg.GetJumphost("jh")
	if len(j.LoginFlow) != 0 {
		t.Errorf("rollback failed: LoginFlow = %v, want empty", j.LoginFlow)
	}
}

// --- UpdateProxy ---

func TestUpdateProxyCreate(t *testing.T) {
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{}, Proxies: []*Proxy{}, Servers: []*SSHServer{}}
	patch := json.RawMessage(`{"type":"SOCKS5","addr":"s:1080"}`)
	if err := cfg.UpdateProxy("p", patch); err != nil {
		t.Fatalf("UpdateProxy: %v", err)
	}
	p, err := cfg.GetProxy("p")
	if err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	if p.Type != ProxySOCKS5 {
		t.Errorf("Type = %v, want SOCKS5", p.Type)
	}
}

func TestUpdateProxyDeleteEntire(t *testing.T) {
	cfg := &Config{
		Version:   "1",
		Jumphosts: []*Jumphost{},
		Proxies:   []*Proxy{{Name: "p", Type: ProxyHTTP, Addr: "h:8080"}},
		Servers:   []*SSHServer{},
	}
	if err := cfg.UpdateProxy("p", json.RawMessage("null")); err != nil {
		t.Fatalf("UpdateProxy: %v", err)
	}
	if _, err := cfg.GetProxy("p"); err == nil {
		t.Errorf("proxy should be deleted")
	}
}

func TestUpdateProxyRejectsDeletingReferenced(t *testing.T) {
	proxy := &Proxy{Name: "p", Type: ProxySOCKS5, Addr: "s:1080"}
	srv := &SSHServer{Name: "s", Addr: "t:22", User: "u", Auth: SSHAuth{Password: "p"}, Proxy: proxy}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{}, Proxies: []*Proxy{proxy}, Servers: []*SSHServer{srv}}
	err := cfg.UpdateProxy("p", json.RawMessage("null"))
	if err == nil {
		t.Errorf("expected error: cannot delete proxy referenced by server")
	}
}

// --- mergePatch helper (also tested via Update* tests above) ---

func TestMergePatchTopLevelNullReturnsNil(t *testing.T) {
	out, err := mergePatch([]byte(`{"a":1}`), json.RawMessage("null"))
	if err != nil {
		t.Fatalf("mergePatch: %v", err)
	}
	if out != nil {
		t.Errorf("got %s, want nil", out)
	}
}

func TestMergePatchRejectsNonObjectNonNull(t *testing.T) {
	_, err := mergePatch([]byte(`{"a":1}`), json.RawMessage(`"string"`))
	if err == nil {
		t.Errorf("expected error for non-object patch")
	}
}
