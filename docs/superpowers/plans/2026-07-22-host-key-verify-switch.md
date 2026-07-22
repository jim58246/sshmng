# Host Key Verify Switch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a per-entity `host_key_verify` `*bool` switch on `SSHServer` and `Jumphost` (default on / secure); when false, dialer completely skips known_hosts read/write.

**Architecture:** 3-state `*bool` lives on config structs; nil→true resolution is encapsulated in a `HostKeyVerifyEnabled()` method per struct. Dialer takes a plain `bool` in `DialOptions` (already resolved) and branches in `hostKeyCallback` to return a no-op callback when false. MCP layer threads the helper result into the two `DialOptions` construction sites.

**Tech Stack:** Go 1.21+, `golang.org/x/crypto/ssh`, existing `internal/config`, `internal/ssh/conn`, `internal/mcp` packages. No new dependencies.

## Global Constraints

- Default behavior (nil / unset) MUST remain current TOFU behavior — no regression for existing configs.
- `*bool` with `omitempty` JSON tag: nil serializes out (list output stays clean), `*false` appears as `"host_key_verify": false`.
- `KnownHostsStore` (file format, `Check` signature) MUST NOT change — skip happens at dialer layer.
- No changes to `validate.go`, `known_hosts.go`, or MCP tool registration in `server.go`.
- Pattern B (`Via.SSHJ=false`) only honors `Jumphost.HostKeyVerify` — target's switch has no effect because target login goes through jumphost PTY, not a separate SSH dial.
- Tests use the existing `newMockSSHServer` helper in `dialer_test.go` and `newTestService` in `tools_config_test.go`.
- Commit style: match repo convention (lowercase prefix `feat:`/`fix:`/`test:`/`docs:` etc., short imperative subject).

---

## File Structure

| File | Responsibility | Action |
|---|---|---|
| `internal/config/types.go` | Add `HostKeyVerify *bool` to `SSHServer`, `Jumphost`, `serverJSON`, `jumphostJSON`; update 2 `MarshalJSON` + 2 `UnmarshalJSON`; add 2 `HostKeyVerifyEnabled()` methods | Modify |
| `internal/config/types_test.go` | Unit tests for helpers (nil/true/false) + JSON round-trip asserting the field serializes correctly | Modify |
| `internal/ssh/conn/dialer.go` | Add `HostKeyVerify bool` to `DialOptions`; thread through `Dial`; branch in `hostKeyCallback` | Modify |
| `internal/ssh/conn/dialer_test.go` | `TestDialerSkipsHostKeyWhenDisabled`: verify connection succeeds against mismatched key and known_hosts is unchanged | Modify |
| `internal/mcp/tools_session.go` | `setupDirect` and `setupPatternB` pass `HostKeyVerify` from the entity's helper | Modify |
| `internal/mcp/tools_config_test.go` | Round-trip: `update_ssh_server`/`update_jumphost` with `{"host_key_verify": false}` → `get_*` reads it back; `null`/omitted returns to nil | Modify |

---

### Task 1: Config layer — `HostKeyVerify` field + helpers + unit tests

**Files:**
- Modify: `internal/config/types.go` (SSHServer struct ~line 86, Jumphost struct ~line 64, serverJSON ~line 173, jumphostJSON ~line 115, two MarshalJSON, two UnmarshalJSON)
- Test: `internal/config/types_test.go`

**Interfaces:**
- Consumes: nothing new
- Produces:
  - `SSHServer.HostKeyVerify *bool` field (JSON tag `host_key_verify,omitempty`)
  - `Jumphost.HostKeyVerify *bool` field (JSON tag `host_key_verify,omitempty`)
  - `func (s *SSHServer) HostKeyVerifyEnabled() bool` — nil→true, else `*s.HostKeyVerify`
  - `func (j *Jumphost) HostKeyVerifyEnabled() bool` — same semantics

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/types_test.go`:

```go
// --- HostKeyVerify --—

func TestSSHServerHostKeyVerifyEnabled(t *testing.T) {
	t.Run("nil defaults to true", func(t *testing.T) {
		s := &SSHServer{}
		if !s.HostKeyVerifyEnabled() {
			t.Errorf("nil HostKeyVerify: got false, want true (default secure)")
		}
	})
	t.Run("explicit true", func(t *testing.T) {
		v := true
		s := &SSHServer{HostKeyVerify: &v}
		if !s.HostKeyVerifyEnabled() {
			t.Errorf("explicit true: got false, want true")
		}
	})
	t.Run("explicit false", func(t *testing.T) {
		v := false
		s := &SSHServer{HostKeyVerify: &v}
		if s.HostKeyVerifyEnabled() {
			t.Errorf("explicit false: got true, want false")
		}
	})
}

func TestJumphostHostKeyVerifyEnabled(t *testing.T) {
	t.Run("nil defaults to true", func(t *testing.T) {
		j := &Jumphost{}
		if !j.HostKeyVerifyEnabled() {
			t.Errorf("nil HostKeyVerify: got false, want true (default secure)")
		}
	})
	t.Run("explicit false", func(t *testing.T) {
		v := false
		j := &Jumphost{HostKeyVerify: &v}
		if j.HostKeyVerifyEnabled() {
			t.Errorf("explicit false: got true, want false")
		}
	})
}

func TestSSHServerHostKeyVerifyJSONRoundTrip(t *testing.T) {
	t.Run("nil omits field", func(t *testing.T) {
		s := SSHServer{Name: "s", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"}}
		out, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("remarshal: %v", err)
		}
		if _, ok := m["host_key_verify"]; ok {
			t.Errorf("nil HostKeyVerify should be omitted, got JSON: %s", out)
		}
	})
	t.Run("explicit false marshals and unmarshals", func(t *testing.T) {
		v := false
		s := SSHServer{Name: "s", Addr: "h:22", User: "u", Auth: SSHAuth{Password: "p"}, HostKeyVerify: &v}
		out, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("remarshal: %v", err)
		}
		if m["host_key_verify"] != false {
			t.Errorf("host_key_verify = %v, want false", m["host_key_verify"])
		}
		var loaded SSHServer
		if err := json.Unmarshal(out, &loaded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if loaded.HostKeyVerify == nil {
			t.Fatalf("HostKeyVerify nil after unmarshal, want *false")
		}
		if *loaded.HostKeyVerify {
			t.Errorf("*loaded.HostKeyVerify = true, want false")
		}
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/Zhuanz/wksp/go/sshmng && go test ./internal/config/ -run 'HostKeyVerify' -v`
Expected: COMPILE FAIL — `s.HostKeyVerify undefined`, `s.HostKeyVerifyEnabled undefined`, same for `Jumphost`.

- [ ] **Step 3: Add `HostKeyVerify` field to `SSHServer` and `Jumphost` structs**

In `internal/config/types.go`, on `Jumphost` struct (after `GlobalTimeoutMs`, before `Via`):

```go
	HostKeyVerify  *bool                  `json:"host_key_verify,omitempty"`
```

On `SSHServer` struct (after `GlobalTimeoutMs`, before `Via`):

```go
	HostKeyVerify *bool `json:"host_key_verify,omitempty"`
```

- [ ] **Step 4: Add the field to `jumphostJSON` and `serverJSON` middle structs**

In `jumphostJSON` struct (after `GlobalTimeoutMs`, before `Via`):

```go
	HostKeyVerify  *bool                  `json:"host_key_verify,omitempty"`
```

In `serverJSON` struct (after `GlobalTimeoutMs`, before `Via`):

```go
	HostKeyVerify *bool `json:"host_key_verify,omitempty"`
```

- [ ] **Step 5: Update `Jumphost.MarshalJSON` and `Jumphost.UnmarshalJSON`**

In `Jumphost.MarshalJSON` (the `jumphostJSON` struct literal), add after `GlobalTimeoutMs: j.GlobalTimeoutMs,`:

```go
		HostKeyVerify:  j.HostKeyVerify,
```

In `Jumphost.UnmarshalJSON` (after `s.GlobalTimeoutMs = sj.GlobalTimeoutMs`), add:

```go
	j.HostKeyVerify = sj.HostKeyVerify
```

- [ ] **Step 6: Update `SSHServer.MarshalJSON` and `SSHServer.UnmarshalJSON`**

In `SSHServer.MarshalJSON` (the `serverJSON` struct literal), add after `GlobalTimeoutMs: s.GlobalTimeoutMs,`:

```go
		HostKeyVerify: s.HostKeyVerify,
```

In `SSHServer.UnmarshalJSON` (after `s.GlobalTimeoutMs = sj.GlobalTimeoutMs`), add:

```go
	s.HostKeyVerify = sj.HostKeyVerify
```

- [ ] **Step 7: Add `HostKeyVerifyEnabled` methods**

Append to `internal/config/types.go` (anywhere after the `SSHServer` and `Jumphost` type definitions):

```go
// HostKeyVerifyEnabled 返回是否启用 host key 校验。
// nil（未配置）→ true（默认安全）；显式 false → false。
func (s *SSHServer) HostKeyVerifyEnabled() bool {
	if s.HostKeyVerify == nil {
		return true
	}
	return *s.HostKeyVerify
}

// HostKeyVerifyEnabled 返回是否启用 host key 校验。
// nil（未配置）→ true（默认安全）；显式 false → false。
func (j *Jumphost) HostKeyVerifyEnabled() bool {
	if j.HostKeyVerify == nil {
		return true
	}
	return *j.HostKeyVerify
}
```

- [ ] **Step 8: Run tests to verify they pass**

Run: `cd /Users/Zhuanz/wksp/go/sshmng && go test ./internal/config/ -run 'HostKeyVerify' -v`
Expected: PASS — all subtests green.

- [ ] **Step 9: Run full config package tests to verify no regression**

Run: `cd /Users/Zhuanz/wksp/go/sshmng && go test ./internal/config/ -v`
Expected: PASS — including pre-existing `TestConfigRoundTrip`, `TestSSHServerMarshalViaProxyAsName`, etc.

- [ ] **Step 10: Commit**

```bash
git add internal/config/types.go internal/config/types_test.go
git commit -m "feat(config): add host_key_verify *bool switch to SSHServer and Jumphost"
```

---

### Task 2: Dialer — `DialOptions.HostKeyVerify` + skip path + test

**Files:**
- Modify: `internal/ssh/conn/dialer.go` (DialOptions struct ~line 35, Dial ~line 50, hostKeyCallback ~line 115)
- Test: `internal/ssh/conn/dialer_test.go`

**Interfaces:**
- Consumes: nothing new (dialer is leaf)
- Produces:
  - `DialOptions.HostKeyVerify bool` field (plain bool, caller resolves nil→true)
  - `hostKeyCallback` now branches: `verify=false` returns a no-op callback that does not touch `KnownHostsStore`

- [ ] **Step 1: Write the failing test**

Append to `internal/ssh/conn/dialer_test.go`:

```go
func TestDialerSkipsHostKeyWhenDisabled(t *testing.T) {
	// srv1 用 key A，先连一次让 known_hosts 记下 A
	srv1 := newMockSSHServer(t, "alice", "wonderland", nil)
	d := newDialerWithTempKnownHosts(t)
	c1, err := d.Dial(DialOptions{
		Addr:          srv1.Addr(),
		User:          "alice",
		Auth:          config.SSHAuth{Password: "wonderland"},
		HostKeyVerify: true,
	})
	if err != nil {
		t.Fatalf("first Dial with verify=true: %v", err)
	}
	c1.Close()
	srv1.listener.Close()

	// srv2 复用同端口但 host key 不同；verify=false 应当连上且不写 known_hosts
	l, err := net.Listen("tcp", srv1.Addr())
	if err != nil {
		t.Fatalf("listen on same port: %v", err)
	}
	srv2 := newMockSSHServerWithListener(t, l, "alice", "wonderland", nil)

	knownBefore, err := os.ReadFile(d.knownHosts.Path())
	if err != nil {
		t.Fatalf("read known_hosts before: %v", err)
	}

	c2, err := d.Dial(DialOptions{
		Addr:          srv2.Addr(),
		User:          "alice",
		Auth:          config.SSHAuth{Password: "wonderland"},
		HostKeyVerify: false,
	})
	if err != nil {
		t.Fatalf("Dial with verify=false should succeed against mismatched key, got: %v", err)
	}
	c2.Close()

	// known_hosts 内容必须未变（没有写入 srv2 的 key）
	knownAfter, err := os.ReadFile(d.knownHosts.Path())
	if err != nil {
		t.Fatalf("read known_hosts after: %v", err)
	}
	if !bytes.Equal(knownBefore, knownAfter) {
		t.Errorf("known_hosts changed when verify=false:\nbefore: %s\nafter:  %s", knownBefore, knownAfter)
	}
}
```

Also ensure `bytes` is imported at the top of the file (if not already):

```go
import (
	"bytes"
	...
)
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/Zhuanz/wksp/go/sshmng && go test ./internal/ssh/conn/ -run TestDialerSkipsHostKeyWhenDisabled -v`
Expected: COMPILE FAIL — `unknown field 'HostKeyVerify' in struct literal of type DialOptions`.

- [ ] **Step 3: Add `HostKeyVerify` field to `DialOptions`**

In `internal/ssh/conn/dialer.go`, extend `DialOptions` (line 35):

```go
// DialOptions 是 Dial 的入参。
type DialOptions struct {
	Addr          string         // host:port
	User          string         // SSH 用户名
	Auth          config.SSHAuth // 认证信息（Password / PrivateKey + Passphrase）
	Proxy         *config.Proxy  // 可选：传输层代理（SOCKS5 / HTTP CONNECT）
	ServerName    string         // 可选：仅用于日志关联（dialing / host key verified 等）
	HostKeyVerify bool           // false 时完全跳过 host key 校验（不读不写 known_hosts）
}
```

- [ ] **Step 4: Thread `HostKeyVerify` through `Dial` into `hostKeyCallback`**

In `Dial` (around line 71), change the `HostKeyCallback` line:

```go
	clientConfig := &ssh.ClientConfig{
		User:            opts.User,
		Auth:            authMethods,
		HostKeyCallback: d.hostKeyCallback(opts.Addr, opts.ServerName, opts.HostKeyVerify),
		Timeout:         10 * time.Second,
	}
```

- [ ] **Step 5: Update `hostKeyCallback` to branch on `verify`**

Replace `hostKeyCallback` (line 115) with:

```go
// hostKeyCallback 返回 ssh.ClientConfig.HostKeyCallback。
// verify=true 时通过 knownHosts.Check 实现 TOFU：首次记录、匹配放行、变更拒绝。
// verify=false 时返回 no-op callback，完全不触碰 known_hosts。
// serverName 仅用于日志关联。
func (d *Dialer) hostKeyCallback(addr, serverName string, verify bool) ssh.HostKeyCallback {
	if !verify {
		d.logger.Debug("host key verification disabled",
			"server", serverName, "addr", addr)
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return nil
		}
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fingerprint, err := d.knownHosts.Check(addr, key)
		if err != nil {
			d.logger.Warn("host key check failed",
				"server", serverName, "addr", addr, "err", err.Error())
			return err
		}
		d.logger.Debug("host key verified",
			"server", serverName, "addr", addr, "fingerprint", fingerprint)
		return nil
	}
}
```

- [ ] **Step 6: Run the new test to verify it passes**

Run: `cd /Users/Zhuanz/wksp/go/sshmng && go test ./internal/ssh/conn/ -run TestDialerSkipsHostKeyWhenDisabled -v`
Expected: PASS.

- [ ] **Step 7: Verify existing TOFU tests still pass (default behavior unchanged)**

Run: `cd /Users/Zhuanz/wksp/go/sshmng && go test ./internal/ssh/conn/ -run 'TestDialerTOFU' -v`
Expected: PASS — `TestDialerTOFURemembersHostKey` and `TestDialerTOFURejectsChangedHostKey` green. They construct `DialOptions` without `HostKeyVerify`, which zero-values to `false` — **this would break them.**

- [ ] **Step 8: Fix existing tests to pass `HostKeyVerify: true`**

The existing TOFU tests rely on default = verify on, but `DialOptions.HostKeyVerify` is a plain bool whose zero value is `false`. We must explicitly set `HostKeyVerify: true` in every pre-existing `DialOptions{}` literal in `dialer_test.go` that expects verification.

Find all `d.Dial(DialOptions{` call sites in `internal/ssh/conn/dialer_test.go` and add `HostKeyVerify: true,` to the literal. This includes (at minimum) the calls in `TestDialerTOFURemembersHostKey`, `TestDialerTOFURejectsChangedHostKey`, and any auth-method tests that dial against a mock server and expect success without specifying the flag.

Use `grep -n "d.Dial(DialOptions{" /Users/Zhuanz/wksp/go/sshmng/internal/ssh/conn/dialer_test.go` to enumerate all sites.

For each site that expects the dial to succeed (i.e. the test is not about host-key failure), add:

```go
		HostKeyVerify: true,
```

- [ ] **Step 9: Run the full dialer test suite**

Run: `cd /Users/Zhuanz/wksp/go/sshmng && go test ./internal/ssh/conn/ -v`
Expected: PASS — all tests green, including pre-existing TOFU tests.

- [ ] **Step 10: Commit**

```bash
git add internal/ssh/conn/dialer.go internal/ssh/conn/dialer_test.go
git commit -m "feat(dialer): honor DialOptions.HostKeyVerify=false by skipping known_hosts"
```

---

### Task 3: MCP wiring + round-trip tests

**Files:**
- Modify: `internal/mcp/tools_session.go` (`setupDirect` ~line 125, `setupPatternB` ~line 181)
- Test: `internal/mcp/tools_config_test.go`

**Interfaces:**
- Consumes: `SSHServer.HostKeyVerifyEnabled()` and `Jumphost.HostKeyVerifyEnabled()` from Task 1; `DialOptions.HostKeyVerify` from Task 2
- Produces: end-to-end behavior — a user can `update_ssh_server` with `{"host_key_verify": false}` and the next `login` to that server skips known_hosts

- [ ] **Step 1: Write the failing round-trip tests**

Append to `internal/mcp/tools_config_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/Zhuanz/wksp/go/sshmng && go test ./internal/mcp/ -run 'HostKeyVerifyPatch' -v`
Expected: FAIL — patch applies (struct already has the field from Task 1) but the assertion fails *if* Task 1 wasn't done. If Task 1 is done, these should PASS already because `update_*` uses RFC 7396 merge patch over JSON, and the new field is automatically part of the schema. If they pass immediately, that's fine — the test still earns its keep as a regression guard. The critical failure mode this test catches is: a future refactor that drops the field from `serverJSON`/`jumphostJSON` would cause `host_key_verify` to be silently dropped during patch, and this test would catch it.

- [ ] **Step 3: Wire `HostKeyVerify` into `setupDirect`**

In `internal/mcp/tools_session.go`, in `setupDirect` (around line 125), extend the `DialOptions` literal:

```go
	client, err := dialer.Dial(conn.DialOptions{
		Addr:          srv.Addr,
		User:          srv.User,
		Auth:          srv.Auth,
		Proxy:         srv.Proxy,
		ServerName:    srv.Name,
		HostKeyVerify: srv.HostKeyVerifyEnabled(),
	})
```

- [ ] **Step 4: Wire `HostKeyVerify` into `setupPatternB`**

In `setupPatternB` (around line 181), extend the `DialOptions` literal for the jumphost dial:

```go
	client, err := dialer.Dial(conn.DialOptions{
		Addr:          jump.Addr,
		User:          jump.User,
		Auth:          jump.Auth,
		Proxy:         jump.Proxy,
		ServerName:    jump.Name,
		HostKeyVerify: jump.HostKeyVerifyEnabled(),
	})
```

- [ ] **Step 5: Run round-trip tests to verify they pass**

Run: `cd /Users/Zhuanz/wksp/go/sshmng && go test ./internal/mcp/ -run 'HostKeyVerifyPatch' -v`
Expected: PASS.

- [ ] **Step 6: Run the entire test suite to verify nothing regressed**

Run: `cd /Users/Zhuanz/wksp/go/sshmng && go test ./...`
Expected: PASS — all packages green.

- [ ] **Step 7: Commit**

```bash
git add internal/mcp/tools_session.go internal/mcp/tools_config_test.go
git commit -m "feat(mcp): propagate host_key_verify from SSHServer/Jumphost to dialer"
```

---

## Self-Review

**Spec coverage:**
- Spec §"Config 层" → Task 1 (field on both structs, middle structs, Marshal/Unmarshal, helpers) ✓
- Spec §"Dialer 层" → Task 2 (DialOptions.HostKeyVerify, hostKeyCallback branch, skip test) ✓
- Spec §"MCP 层" → Task 3 (setupDirect + setupPatternB wiring) ✓
- Spec §"RFC 7396 patch 行为" → Task 3 round-trip tests cover `false` and `true` patches ✓
- Spec §"测试" → helper unit tests (Task 1), dialer skip test (Task 2), MCP round-trip (Task 3) ✓
- Spec §"非目标" — no `host_key_strategy`, no global switch, no `KnownHostsStore` changes — all respected ✓

**Placeholder scan:** No TBD/TODO. All code blocks complete. Step 8 of Task 2 uses `grep` to enumerate call sites rather than hardcoding line numbers (test file may have shifted) — this is intentional, not a placeholder.

**Type consistency:**
- `HostKeyVerify *bool` (config) ↔ `HostKeyVerify bool` (DialOptions) — intentional boundary via `HostKeyVerifyEnabled()` helper
- `HostKeyVerifyEnabled()` defined identically on both `*SSHServer` and `*Jumphost` — same signature, same semantics
- `DialOptions.HostKeyVerify` referenced consistently in Task 2 and Task 3

**Risk check:** Step 7→8 of Task 2 acknowledges a real failure mode — existing TOFU tests construct `DialOptions{}` without the new field and would silently flip to `verify=false`. The fix-up step is explicit, not hidden.
