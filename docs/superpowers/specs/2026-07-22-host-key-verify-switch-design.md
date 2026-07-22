# Host Key Verification 开关

**Date:** 2026-07-22
**Status:** Approved, ready for implementation plan
**Scope:** `internal/config`, `internal/ssh/conn`, `internal/mcp`

## 背景

堡垒机域名常解析到多个 IP，每个 IP 背后可能是不同物理机，各自生成了不同的 SSH host key。`sshmng` 当前对每个实体按 `(addr, key)` 做 TOFU 记录，addr 用的是配置里的 `host:port`（通常是域名），于是同一域名的不同 IP 命中不同 key 时会被识别为 "host key changed" 并拒绝连接。

服务端共享 host key 或上 SSH CA 是更根本的解法，但用户控制不了堡垒机配置。客户端需要一个逃生开关：遇到这种情况时关掉 host key 校验。

## 目标

- 给 `SSHServer` 和 `Jumphost` 各加一个 per-entity 的 `host_key_verify` 开关
- 默认 **on**（保持当前 TOFU 行为，安全默认）
- 显式设为 `false` 时完全跳过 host key 校验：不读 known_hosts，不写 known_hosts，连接照常建立
- 非 IP-based 记录策略（之前讨论过的方案）暂不实现，未来再开新 spec

## 非目标

- 不改 `KnownHostsStore` 的文件格式或 `Check` 语义
- 不引入 `host_key_strategy: addr|ip|none` 这类多模式配置
- 不加全局默认开关（每个 entity 独立配置）

## 设计

### Config 层（`internal/config/types.go`）

`Jumphost` 和 `SSHServer` 各加一个字段，紧挨 `GlobalTimeoutMs`：

```go
HostKeyVerify *bool `json:"host_key_verify,omitempty"`
```

三态语义：
- `nil`（未配置 / JSON 缺省）→ 默认 on
- `*false` → 显式关闭
- `*true` → 显式打开（从 false 改回 true 时用）

`omitempty` 让 nil 在序列化时省略，`list_*` 输出干净；只有显式关闭的实体才会出现 `"host_key_verify": false`。

`jumphostJSON` 和 `serverJSON` 中间层 struct 各加同名字段。`MarshalJSON` / `UnmarshalJSON` 把字段在 struct 和中间层之间显式拷贝（既有模式）。

两个 struct 各加一个 helper 方法：

```go
// HostKeyVerifyEnabled 返回是否启用 host key 校验。
// nil（未配置）→ true（默认安全）；显式 false → false。
func (s *SSHServer) HostKeyVerifyEnabled() bool {
    if s.HostKeyVerify == nil {
        return true
    }
    return *s.HostKeyVerify
}
```

`Jumphost` 上同样一个方法。nil→true 的解释逻辑只在这两个方法里出现。

`validate.go` 不动——`*bool` 三态都合法，Pattern A/B 校验和该字段无关。

### Dialer 层（`internal/ssh/conn/dialer.go`）

`DialOptions` 加一个 plain bool（已解析，不携带 nil 语义）：

```go
type DialOptions struct {
    Addr          string
    User          string
    Auth          config.SSHAuth
    Proxy         *config.Proxy
    ServerName    string
    HostKeyVerify bool // false 时完全跳过 host key 校验（不读不写 known_hosts）
}
```

`Dial` 第 71 行 `HostKeyCallback` 构造改为透传 flag：

```go
HostKeyCallback: d.hostKeyCallback(opts.Addr, opts.ServerName, opts.HostKeyVerify),
```

`hostKeyCallback` 多收一个 `verify bool`，构造时一次性分支：

```go
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

- `if !verify` 分支在构造回调时判断一次，不是每次回调都判；返回 no-op closure。一次 Dial 一条 debug 日志，不刷屏。
- verify=true 分支保持原逻辑不变。
- `KnownHostsStore` 完全不动——跳过发生在 dialer 层，known_hosts 文件既不读也不写。
- `translateDialError` 不动——verify=false 时根本不会产生 "host key changed" 错误。

**边界**：DialOptions 的 `HostKeyVerify` 是 plain bool，调用方必须自己解析好 nil→true。3-state 逻辑关在 config 层，dialer 不重复实现。

### MCP 层（`internal/mcp/tools_session.go`）

两处构造 `DialOptions` 的地方各加一行：

`setupDirect` (line 125) — 直连目标机，看 `srv`：

```go
HostKeyVerify: srv.HostKeyVerifyEnabled(),
```

`setupPatternB` (line 181) — 拨号到堡垒机，看 `jump`（Pattern B 下 SSH 连接是到 jumphost 的，target 登录走 PTY 交互而非 SSH dial，所以 target 的 `HostKeyVerify` 在这里不参与）：

```go
HostKeyVerify: jump.HostKeyVerifyEnabled(),
```

MCP schema 不动。`update_ssh_server` / `update_jumphost` 是 RFC 7396 patch，patch 结构镜像 get 输出；新字段加进 struct 后自动可读可写。

### RFC 7396 patch 行为

- `{"host_key_verify": false}` → 关闭
- `{"host_key_verify": true}` → 打开（从 false 改回）
- 缺省字段 → 保持原值（merge patch 语义）
- `{"host_key_verify": null}` → Go 标准 `*bool` unmarshal 下和缺省不可区分，都变成 nil（即默认 on）。用户要"重置为默认"就显式传 `true`。这个边角行为可接受。

## 测试

### `internal/config/types_test.go`

`HostKeyVerifyEnabled` 单测（`SSHServer` 和 `Jumphost` 各一组）：
- `nil` → `true`
- `*true` → `true`
- `*false` → `false`

### `internal/ssh/conn/dialer_test.go`

新增 `TestDialerSkipsHostKeyWhenDisabled`：
- known_hosts 文件预置 key A
- 启一个用 key B 的 SSH 测试 server
- `DialOptions{HostKeyVerify: false}` 拨号 → 期望成功
- 拨号后再读 known_hosts，断言内容未变（仍是 key A，没写入 B）
- 对照组：`HostKeyVerify: true`（或复用既有 `TestDialerTOFURejectsChangedHostKey`）验证默认路径仍拒绝

既有 `TestDialerTOFURemembersHostKey` 和 `TestDialerTOFURejectsChangedHostKey` 走默认 verify=true 路径，无需改。

### `internal/mcp/tools_config_test.go`

Round-trip 测试（`SSHServer` 和 `Jumphost` 各一组）：
- `update_ssh_server` 传 `{"host_key_verify": false}` → `get_ssh_server` 回读断言 `host_key_verify == false`
- 再 `update_ssh_server` 传缺省字段的 patch（或 `null`）→ 回读断言字段消失/回到 nil
- jumphost 同样一组

## 改动文件清单

- `internal/config/types.go` — 加字段、中间层字段、Marshal/Unmarshal、两个 helper 方法
- `internal/config/types_test.go` — helper 方法单测
- `internal/ssh/conn/dialer.go` — DialOptions 加字段、hostKeyCallback 加参数和分支
- `internal/ssh/conn/dialer_test.go` — 新增跳过校验测试
- `internal/mcp/tools_session.go` — 两处 DialOptions 构造加一行
- `internal/mcp/tools_config_test.go` — round-trip 测试

`validate.go`、`known_hosts.go`、`server.go`（MCP 注册）不动。

## 风险与权衡

- **MITM 防护丢失**：用户显式关闭后，对该实体不再有 MITM 检测。这是用户主动选择，文档说明清楚即可。
- **`null` 与缺省不可区分**：Go `*bool` unmarshal 限制。用户要重置为默认 on，需显式传 `true`。可接受。
- **Pattern B 下 target 无独立开关**：target 登录走 jumphost PTY，不做 SSH dial，所以 target 的 `host_key_verify` 不生效。文档里说明，避免用户误以为关掉 target 的开关就能绕过 jumphost 的校验。
