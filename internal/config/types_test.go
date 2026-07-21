package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestProxyTypeMarshal(t *testing.T) {
	cases := []struct {
		v    ProxyType
		want string
	}{
		{ProxyHTTP, `"HTTP"`},
		{ProxySOCKS5, `"SOCKS5"`},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.v)
		if err != nil {
			t.Fatalf("marshal %v: %v", c.v, err)
		}
		if string(got) != c.want {
			t.Errorf("got %s, want %s", got, c.want)
		}
	}
}

func TestProxyTypeUnmarshal(t *testing.T) {
	cases := []struct {
		in   string
		want ProxyType
	}{
		{`"HTTP"`, ProxyHTTP},
		{`"SOCKS5"`, ProxySOCKS5},
	}
	for _, c := range cases {
		var got ProxyType
		if err := json.Unmarshal([]byte(c.in), &got); err != nil {
			t.Fatalf("unmarshal %s: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("got %v, want %v", got, c.want)
		}
	}
}

func TestJumphostSSHJDefault(t *testing.T) {
	// JSON 不含 ssh_j 字段，反序列化后 SSHJ 应为零值 false
	in := `{"name":"jh","addr":"h:22","user":"u","auth":{}}`
	var j Jumphost
	if err := json.Unmarshal([]byte(in), &j); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if j.SSHJ {
		t.Errorf("SSHJ = true, want false (zero value)")
	}
}

func TestJumphostMarshalDropsNilViaProxy(t *testing.T) {
	j := Jumphost{Name: "jh", Addr: "h:22", User: "u", Auth: SSHAuth{}, SSHJ: true}
	out, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	if _, ok := m["via"]; ok {
		t.Errorf("via should be omitted when nil, got: %s", out)
	}
	if _, ok := m["proxy"]; ok {
		t.Errorf("proxy should be omitted when nil, got: %s", out)
	}
}

func TestJumphostMarshalViaProxyAsName(t *testing.T) {
	via := &Jumphost{Name: "upstream-jh"}
	proxy := &Proxy{Name: "corp-socks5", Type: ProxySOCKS5, Addr: "socks:1080"}
	j := Jumphost{Name: "jh", Addr: "h:22", User: "u", Auth: SSHAuth{}, SSHJ: true, Via: via, Proxy: proxy}
	out, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	if m["via"] != "upstream-jh" {
		t.Errorf("via = %v, want \"upstream-jh\"", m["via"])
	}
	if m["proxy"] != "corp-socks5" {
		t.Errorf("proxy = %v, want \"corp-socks5\"", m["proxy"])
	}
}

func TestJumphostUnmarshalKeepsViaProxyNameUnresolved(t *testing.T) {
	in := `{"name":"jh","addr":"h:22","user":"u","auth":{},"ssh_j":false,"via":"upstream-jh","proxy":"corp-socks5","login_flow":{"a":{"name":"a","expects":[{"pattern":"x","next":"success"}]}},"login_entry":"a"}`
	var j Jumphost
	if err := json.Unmarshal([]byte(in), &j); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if j.Via != nil {
		t.Errorf("Via should be nil before resolveReferences, got %v", j.Via)
	}
	if j.Proxy != nil {
		t.Errorf("Proxy should be nil before resolveReferences, got %v", j.Proxy)
	}
	if j.viaName != "upstream-jh" {
		t.Errorf("viaName = %q, want \"upstream-jh\"", j.viaName)
	}
	if j.proxyName != "corp-socks5" {
		t.Errorf("proxyName = %q, want \"corp-socks5\"", j.proxyName)
	}
}

func TestSSHServerMarshalViaProxyAsName(t *testing.T) {
	via := &Jumphost{Name: "jh"}
	proxy := &Proxy{Name: "p", Type: ProxyHTTP, Addr: "p:8080"}
	s := SSHServer{Name: "s", Addr: "t:22", User: "u", Auth: SSHAuth{}, Via: via, Proxy: proxy}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	if m["via"] != "jh" {
		t.Errorf("via = %v, want \"jh\"", m["via"])
	}
	if m["proxy"] != "p" {
		t.Errorf("proxy = %v, want \"p\"", m["proxy"])
	}
}

func TestConfigRoundTrip(t *testing.T) {
	original := &Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Proxies: []*Proxy{
			{Name: "corp-socks5", Type: ProxySOCKS5, Addr: "socks:1080", Tags: []string{"生产"}},
		},
		Jumphosts: []*Jumphost{
			{
				Name:       "华东/jumphost-prod",
				Addr:       "10.0.0.254:22",
				User:       "ops",
				Auth:       SSHAuth{Password: "secret"},
				SSHJ:       false,
				LoginFlow:  map[string]LoginAction{"wait": {Expects: []Expect{{Pattern: "menu", Next: "success"}}}},
				LoginEntry: "wait",
				Proxy:      nil,
				Tags:       []string{"生产", "华东"},
			},
		},
		Servers: []*SSHServer{
			{
				Name:  "华东/order/order-01",
				Addr:  "10.0.0.1:22",
				User:  "deploy",
				Auth:  SSHAuth{},
				Proxy: nil,
				Tags:  []string{"生产", "v2.3", "主备"},
			},
		},
	}
	// resolveReferences 把 via/proxy 指针关联起来（这里都是 nil，无副作用）
	if err := original.resolveReferences(); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	out, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var loaded Config
	if err := json.Unmarshal(out, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := loaded.resolveReferences(); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if !reflect.DeepEqual(original.Jumphosts, loaded.Jumphosts) {
		t.Errorf("Jumphosts mismatch:\norig: %+v\nloaded: %+v", original.Jumphosts, loaded.Jumphosts)
	}
	if !reflect.DeepEqual(original.Servers, loaded.Servers) {
		t.Errorf("Servers mismatch:\norig: %+v\nloaded: %+v", original.Servers, loaded.Servers)
	}
	if !reflect.DeepEqual(original.Proxies, loaded.Proxies) {
		t.Errorf("Proxies mismatch:\norig: %+v\nloaded: %+v", original.Proxies, loaded.Proxies)
	}
}

func TestConfigResolveReferences(t *testing.T) {
	jh := &Jumphost{Name: "jh", Addr: "h:22", User: "u", Auth: SSHAuth{}, SSHJ: true}
	proxy := &Proxy{Name: "p", Type: ProxySOCKS5, Addr: "p:1080"}
	srv := &SSHServer{
		Name: "s", Addr: "t:22", User: "u", Auth: SSHAuth{},
		viaName:   "jh",
		proxyName: "p",
	}
	cfg := &Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Jumphosts:    []*Jumphost{jh},
		Proxies:      []*Proxy{proxy},
		Servers:      []*SSHServer{srv},
	}
	if err := cfg.resolveReferences(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if srv.Via != jh {
		t.Errorf("srv.Via = %p, want %p", srv.Via, jh)
	}
	if srv.Proxy != proxy {
		t.Errorf("srv.Proxy = %p, want %p", srv.Proxy, proxy)
	}
}

func TestConfigResolveReferencesUnknownJumphost(t *testing.T) {
	srv := &SSHServer{Name: "s", Addr: "t:22", User: "u", Auth: SSHAuth{}, viaName: "no-such-jh"}
	cfg := &Config{Version: "1", Servers: []*SSHServer{srv}}
	err := cfg.resolveReferences()
	if err == nil {
		t.Fatalf("expected error for unknown jumphost reference")
	}
}

func TestConfigResolveReferencesUnknownProxy(t *testing.T) {
	srv := &SSHServer{Name: "s", Addr: "t:22", User: "u", Auth: SSHAuth{}, proxyName: "no-such-proxy"}
	cfg := &Config{Version: "1", Servers: []*SSHServer{srv}}
	err := cfg.resolveReferences()
	if err == nil {
		t.Fatalf("expected error for unknown proxy reference")
	}
}

func TestConfigResolveReferencesDuplicateJumphostName(t *testing.T) {
	jh1 := &Jumphost{Name: "dup", Addr: "h1:22", User: "u", Auth: SSHAuth{}, SSHJ: true}
	jh2 := &Jumphost{Name: "dup", Addr: "h2:22", User: "u", Auth: SSHAuth{}, SSHJ: true}
	cfg := &Config{Version: "1", Jumphosts: []*Jumphost{jh1, jh2}}
	err := cfg.resolveReferences()
	if err == nil {
		t.Fatalf("expected error for duplicate jumphost name")
	}
}
