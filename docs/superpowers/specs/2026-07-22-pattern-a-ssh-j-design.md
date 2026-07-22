# Pattern A（`ssh -J` 透明转发）实现

**Date:** 2026-07-22
**Status:** Approved, ready for implementation plan
**Scope:** `internal/ssh/conn`, `internal/ssh/pty`, `internal/mcp`, `internal/config`

## 背景

`sshmng` 的 `Jumphost.SSHJ` 字段区分两种形态：`true` = 透明转发（`ssh -J` 语义），`false` = 交互式堡垒机。当前实现只支持 `SSHJ=false`（Pattern B）和直连；`SSHJ=true`（Pattern A）在 `tools_session.go:67-70` 显式拒绝：

```go
if srv.Via != nil && srv.Via.SSHJ {
    return errorResult("pattern A via ssh-j jumphost %q not yet supported (server %q); deferred to v1.x", srv.Via.Name, args.Name)
}
```

设计文档 §2.6 / §3.1 早已规定 Pattern A 的完整语义，但代码层一直留空。本 spec 补齐这部分。

## 目标

- 实现 Pattern A：客户端 SSH auth 到 jumphost → 经 jumphost 的 direct-tcpip 通道 SSH auth 到 target → target shell 就绪 → 可选 `SSHServer.LoginFlow`（target 认证后交互，如 su / 角色 / PAM）→ 注入 RC → idle
- SFTP 可用（client 是到 target 的，区别于 Pattern B）
- 两层 SSH 拨号各自走 TOFU host key 校验，尊重 `host_key_verify` 开关
- 两层 auth 各自支持 Password / PrivateKey
- 配置校验：Pattern A 下 `Server.Proxy` 非空 → Load 拒绝（direct-tcpip 走 jumphost 的 SSH 通道，独立传输代理无意义）
- 同步更新 README / design doc / implementation-plan

## 非目标

- 多跳 jumphost 链（A→B→target，`Jumphost.Via` 递归）——设计文档明确 YAGNI，留口子
- `Jumphost.LoginFlow` 在 `ssh_j=true` 下启用——校验已要求 `ssh_j=true` 时 `login_flow` 必空，Pattern A 不跑 jumphost 菜单
- 真实网络环境的 ssh -J 验收——靠 mock 覆盖核心路径，真实环境验收留 MCP Inspector

## 架构与数据流

Pattern A 的两层 SSH 拨号链路：

```
sshmng 进程
  │
  ├─ Dialer.Dial(jumphost opts) ────────────────┐
  │   (TCP/代理 → jumphost SSH auth → TOFU)     │
  │                                             ▼
  │                                       jumpClient (*ssh.Client)
  │                                             │
  ├─ Dialer.DialThrough(jumpClient, target opts)│
  │   (jumpClient.Dial("tcp", targetAddr)       │
  │    → direct-tcpip channel                   │
  │    → ssh.NewClientConn → target SSH auth    │
  │    → TOFU)                                  │
  │                                             ▼
  │                                       targetClient (*ssh.Client)
  │                                             │
  ├─ pty.OpenPtyConn(targetClient, ...)         │
  │   (NewSession + RequestPty + Shell +        │
  │    readLoop 启动)                            │
  │                                             ▼
  │                                       *PtyConn (jumpClient 通过
  │                                             SetJumpClient 绑定)
  │                                             │
  ├─ 可选 SSHServer.LoginFlow（target shell     │
  │    就绪后的 su/角色/PAM 交互）               │
  ├─ DetectShell + InjectRC                     │
  ├─ TryEnableSftp（target client，可用）       │
  │                                             ▼
  └─ session 进入 idle                    PtyConn.Close 顺序：
                                          sftp → session → targetClient
                                          → jumpClient
```

**组件职责（不变）：**
- `Dialer`：所有 SSH 拨号（单跳 `Dial` + 新增 `DialThrough`）
- `PtyConn`：所有 PTY 交互 + 连接生命周期（新增 `jumpClient` 字段）
- `tools_session.go`：编排，`setupPatternA` 与 `setupDirect`/`setupPatternB` 并列

**Login handler 路由（替换现有 line 67-70 的拒绝）：**

```go
switch {
case srv.Via == nil:
    ptyConn, trace, err = s.setupDirect(...)
case srv.Via.SSHJ:
    ptyConn, trace, err = s.setupPatternA(...)   // 新增
default: // srv.Via.SSHJ == false
    ptyConn, trace, err = s.setupPatternB(...)
}
```

## Dialer.DialThrough

新增方法，与 `Dial` 并列。复用现有的 `buildAuthMethods` / `hostKeyCallback` / `translateDialError`。

```go
// DialThrough 经由 jumphost 的 SSH client 拨号到 target。
// 用 jumpClient.Dial("tcp", opts.Addr) 开 direct-tcpip 通道（ssh -J 语义），
// 再 ssh.NewClientConn 在其上建立第二层 SSH 连接。
//
// jumpClient 必须由调用方保持存活——target 的底层 conn 是 jumpClient 上的 channel，
// jumpClient 关闭会导致 target 不可用。调用方负责在 target 关闭后再关 jumpClient
// （PtyConn.Close 通过 SetJumpClient 绑定的引用处理此顺序）。
//
// 失败时关闭已开的 direct-tcpip conn，调用方无需清理。
func (d *Dialer) DialThrough(jumpClient *ssh.Client, opts DialOptions) (*ssh.Client, error)
```

**实现要点：**

1. **auth / hostKeyCallback / 日志**：与 `Dial` 完全一致，复用 `buildAuthMethods(opts.Auth)` 和 `d.hostKeyCallback(opts.Addr, opts.ServerName, opts.HostKeyVerify)`。target 的 TOFU 独立于 jumphost（known_hosts 按 addr 区分）。`opts.HostKeyVerify` 透传，尊重 `SSHServer.host_key_verify` 开关。

2. **direct-tcpip 通道超时**：`jumpClient.Dial("tcp", addr)` 无 timeout 参数。用 goroutine + select 兜底 10s（与 `Dial` 的 TCP 超时一致）：
   ```go
   type res struct { c net.Conn; err error }
   ch := make(chan res, 1)
   go func() { c, err := jumpClient.Dial("tcp", opts.Addr); ch <- res{c, err} }()
   select {
   case r := <-ch:
       // 拿到 direct-tcpip conn 或 err
   case <-time.After(10 * time.Second):
       return nil, fmt.Errorf("dial %s through jumphost: open direct-tcpip timed out after 10s", opts.Addr)
   }
   ```
   > 注：`jumpClient.Dial` 底层是 SSH channel 开启请求，无法取消。超时后 goroutine 阻塞到 jumphost 响应或 jumphost 关闭。jumphost 关闭时所有阻塞 Dial 返回 error，goroutine 退出。可接受的泄漏——超时本身已说明 jumphost 异常，调用方会 Close 整个 session。

3. **SSH 握手超时**：`ssh.NewClientConn(conn, addr, &ssh.ClientConfig{Timeout: 10s, ...})`。`Timeout` 字段用作握手超时。失败时关闭 direct-tcpip conn 兜底。

4. **错误翻译**：复用 `translateDialError`（host key changed 透传，其他包装为 `ssh connect to <addr>: <err>`）。区分两层失败：
   - direct-tcpip 开启失败（jumphost 拒绝转发 / 网络不通）：`fmt.Errorf("dial %s through jumphost: %w", opts.Addr, err)`
   - target SSH 握手失败（auth / host key / 协议）：走 `translateDialError`，消息含 "permission denied" / "host key changed" 等

5. **日志**：`d.logger.Debug("dialing through", "server", opts.ServerName, "addr", opts.Addr, "auth_method", ..., "via_jumphost", true)`，与 `Dial` 的日志结构对齐。

## PtyConn 改动

最小侵入：加一个字段 + 一个 setter + Close 顺序补一段。

**新字段：**

```go
type PtyConn struct {
    // ... 现有字段不变 ...
    
    // jumpClient 是 Pattern A 下的 jumphost SSH client。
    // target 的底层 conn 是 jumpClient 上的 direct-tcpip channel，
    // jumpClient 必须在 target client 关闭前存活。
    // Close() 在关闭 target client 后关闭 jumpClient。
    // direct / Pattern B 路径下为 nil，Close() 跳过。
    jumpClient *ssh.Client
}
```

**Setter（Pattern A 专用）：**

```go
// SetJumpClient 绑定 jumphost SSH client，把其生命周期挂到 PtyConn。
// Pattern A 调用：target client 的底层 conn 是 jumpClient 上的 channel，
// jumpClient 必须存活到 target 关闭。Close() 会先关 target client 再关 jumpClient。
// 必须在 Close() 前调用；重复调用覆盖前值（不预期）。
// direct / Pattern B 不调用，jumpClient 保持 nil。
func (p *PtyConn) SetJumpClient(c *ssh.Client) {
    p.mu.Lock()
    defer p.mu.Unlock()
    p.jumpClient = c
}
```

**Close() 顺序调整：**

```go
func (p *PtyConn) Close() error {
    p.mu.Lock()
    if p.closed { p.mu.Unlock(); return nil }
    p.closed = true
    sftpClient := p.sftpClient
    p.sftpClient = nil
    session := p.session
    client := p.client
    jumpClient := p.jumpClient   // 新增
    p.mu.Unlock()

    close(p.doneCh)
    var errs []string
    if sftpClient != nil { ... }        // 不变
    if session != nil { ... }           // 不变
    if client != nil { ... }            // 不变：先关 target client
    if jumpClient != nil {              // 新增：target 关完后再关 jumphost
        if err := jumpClient.Close(); err != nil {
            errs = append(errs, fmt.Sprintf("jumpClient: %v", err))
        }
    }
    // ... errs 聚合返回不变 ...
}
```

**为什么是 setter 而非构造参数：**
- `OpenPtyConn(client, sid, logger)` 签名不变，direct / Pattern B 路径零改动
- `OpenPtyConnWithTimeout` 同理不变
- setupPatternA 在 `OpenPtyConnWithTimeout` 成功后立刻调 `SetJumpClient`，此时 readLoop 已启动但只读 stdout（与 jumpClient 无关），无竞态
- Close() 是唯一读 `jumpClient` 的地方，`p.mu` 保护

**为什么 Close 顺序是 target → jumphost：**
- target client.Close() 会关闭其底层 conn（即 direct-tcpip channel）
- 若先关 jumphost，target 的底层 channel 立刻失效，target client.Close() 会报 "connection closed" 噪声错误
- 先关 target 干净，再关 jumphost，错误聚合里两边都是干净的 close

**Closed() 不变**：jumpClient 状态不参与 closed 判断（closed 只反映 PtyConn 自身是否被 Close 过）。

## setupPatternA

与 `setupDirect` / `setupPatternB` 并列。结构和 setupDirect 几乎一样，只在拨号阶段换成两层。

```go
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
        Addr:           jump.Addr,
        User:           jump.User,
        Auth:           jump.Auth,
        Proxy:          jump.Proxy,
        ServerName:     jump.Name,
        HostKeyVerify:  jump.HostKeyVerifyEnabled(),
    })
    if err != nil {
        return nil, nil, fmt.Errorf("ssh connect to jumphost %s: %w", jump.Addr, err)
    }

    // 第二层：经 jumphost 的 direct-tcpip 拨号 target
    targetClient, err := dialer.DialThrough(jumpClient, conn.DialOptions{
        Addr:           srv.Addr,
        User:           srv.User,
        Auth:           srv.Auth,
        Proxy:          nil, // Pattern A 下 Server.Proxy 已被 validate.go 拒绝
        ServerName:     srv.Name,
        HostKeyVerify:  srv.HostKeyVerifyEnabled(),
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
        "sid", sid, "server", srv.Name, "via", jump.Name,
        "sftp_available", ptyConn.SftpAvailable(), "shell", ptyConn.Shell())
    return ptyConn, trace, nil
}
```

**关键点：**

1. **资源清理顺序**：任何阶段失败，先关 target client（若已建）再关 jumpClient。`ptyConn.SetJumpClient` 后，`ptyConn.Close()` 会代劳两者，setupPatternA 不再手动关。

2. **LoginFlow stage 标签**：`Stage: "patternA"`（区别于 setupDirect 的 `"direct"` 和 setupPatternB 的 `"jumphost"` / `"target"`）。login_trace 里 Agent 据此识别失败发生在哪段。

3. **SFTP 启用**：与 setupDirect 一致（client 是到 target 的）。区别于 setupPatternB（不启用）。

4. **DetectShell / InjectRC**：完全复用，代码与 setupDirect 一致——target shell 已就绪，行为相同。

5. **Proxy 传 nil**：validate.go 已在加载时拒绝 Pattern A + Server.Proxy 非空，这里防御性传 nil。

6. **HostKeyVerify**：两层拨号分别传 `jump.HostKeyVerifyEnabled()` 和 `srv.HostKeyVerifyEnabled()`，与 setupDirect/PatternB 一致尊重 per-entity 开关。

## 配置校验

`internal/config/validate.go` 的 `validateSSHServer` 加一条 Pattern A 专属规则。

**现状（line 70-79）：** Pattern A 分支（直连或 Via.SSHJ=true）只检查 auth 必填 + 可选 LoginFlow 结构。没检查 Server.Proxy。

**新增规则：**

```go
// Pattern A（直连或 Via.SSHJ=true）
if authIsEmpty(s.Auth) {
    return fmt.Errorf("pattern A requires auth (used for SSH auth to target)")
}
// 新增：Pattern A via ssh_j jumphost 不支持 Server.Proxy
// direct-tcpip 走 jumphost 的 SSH 通道，独立传输代理无意义；
// 直连（Via=nil）仍允许 Server.Proxy（合法的代理拨号到 target）
if s.Via != nil && s.Via.SSHJ && s.Proxy != nil {
    return fmt.Errorf("pattern A (via.ssh_j=true) does not support server.proxy; direct-tcpip tunnels through jumphost's SSH channel, a separate transport proxy is meaningless")
}
if len(s.LoginFlow) > 0 {
    if err := validateLoginFlow(s.LoginFlow, s.LoginEntry); err != nil {
        return err
    }
}
return nil
```

**为什么直连仍允许 Server.Proxy：**
- 直连场景：`Dialer.Dial` 用 `Server.Proxy` 拨号到 target（SOCKS5/HTTP CONNECT 到 target:22）——合法用途
- Pattern A 场景：target 拨号是 `jumpClient.Dial("tcp", targetAddr)`，走 jumphost 的 SSH 通道内部，根本不经过任何传输代理——Server.Proxy 配了也是死代码

**校验时机：** Load 时（启动 + 每次 `update_ssh_server` 后）就拒绝，不用等到 login 时。Agent 配错立刻收到清晰错误，而不是 login 时才报含糊错误。

**Pattern B 下 Server.Proxy 不拒绝：** Pattern B 根本不拨号到 target（走 PTY 菜单），Server.Proxy 无意义但不冲突，静默忽略比校验拒绝更友好。

## 错误分类与 login_trace

遵循设计文档 §3.2 的三类失败分类，Pattern A 不引入新类别。

| 失败阶段 | 错误形式 | 含 trace？ | Agent 诊断路径 |
|---------|---------|-----------|---------------|
| Jumphost SSH 拨号（auth / host key / 网络 / 代理）| `error` 字符串 | 否 | 错误信息自解释（"permission denied" / "host key changed" / "connection refused"）|
| Target SSH 拨号经 direct-tcpip（auth / host key / jumphost 拒绝转发）| `error` 字符串 | 否 | 同上；jumphost 拒绝转发时消息含 "direct-tcpip" |
| OpenPtyConn（RequestPty / Shell 超时）| `error` 字符串 | 否 | "open pty timed out" / "request pty: ..." |
| SSHServer.LoginFlow 失败（Pattern A 下可选）| `*pty.LoginFlowError` | **是** | login_trace 含每步 send/expect/output，`stage="patternA"` |
| DetectShell / InjectRC 失败 | `error` 字符串 | 是（若 LoginFlow 跑过）| trace 已有 LoginFlow 步骤，Agent 可看登录过程是否正常 |

**错误消息模板：**

```go
// Jumphost 拨号
return nil, nil, fmt.Errorf("ssh connect to jumphost %s: %w", jump.Addr, err)

// Target 拨号经 direct-tcpip
return nil, nil, fmt.Errorf("ssh connect to target %s through jumphost: %w", srv.Addr, err)

// LoginFlow 失败
return nil, trace, fmt.Errorf("patternA: %w", &pty.LoginFlowError{Stage: "patternA", Trace: trace, Err: err})
```

**Login handler 提取 trace 的逻辑不变**（`tools_session.go:88-100` 的 `errors.As(err, &lfErr)`）：Pattern A 的 LoginFlow 失败走同一个分支，Agent 拿到 `login_trace` 字段，含 `stage="patternA"` 标签区分于 `direct` / `jumphost` / `target`。

**关键约定：**
- **拨号失败不带 trace**：与 setupDirect / setupPatternB 一致。SSH auth 失败的错误信息已自解释，trace 帮不上忙（trace 是 PTY 交互记录，拨号阶段没有 PTY）。
- **LoginFlow 失败带 trace**：Pattern A 的 LoginFlow 是 target shell 就绪后的交互（su / 角色 / PAM），PTY 已开，trace 有意义。
- **Stage 标签 `"patternA"`**：单标签足够，因为 Pattern A 只有一段 LoginFlow（不像 Pattern B 有 jumphost + target 两段）。Agent 看 stage 就知道是哪段挂了。

**jumphost 拒绝 direct-tcpip 的错误翻译：**

`jumpClient.Dial("tcp", addr)` 在 jumphost 禁用转发时返回类似 `"ssh: rejected: administratively prohibited"` 的错误。`DialThrough` 不特殊翻译，原样包进 `"ssh connect to target %s through jumphost: %w"`——Agent 看到 "administratively prohibited" 就知道是 jumphost 配置问题（`~/.ssh/sshd_config` 的 `AllowTcpForwarding no`），不是 target 的问题。

## 测试

### 测试基础设施

**新增 `fakeJumphostForPatternA`（支持 direct-tcpip 转发）：**

```go
// fakeJumphostForPatternA 是 Pattern A 测试用的 fake jumphost SSH server。
// 与 Pattern B 的 fakeJumphostServerForMCP 不同——它不跑菜单，而是处理
// direct-tcpip channel 请求：解析目标地址，net.Dial 到真实 fake target，双向 pipe。
type fakeJumphostForPatternA struct {
    t               *testing.T
    listener        net.Listener
    hostKey         ssh.Signer
    allowForwarding bool  // false 时拒绝 direct-tcpip（模拟 AllowTcpForwarding no）
    wg              sync.WaitGroup
}
```

channel 处理逻辑：

```go
for newCh := range chans {
    if newCh.ChannelType() == "direct-tcpip" {
        if !s.allowForwarding {
            newCh.Reject(ssh.Prohibited, "administratively prohibited")
            continue
        }
        // 解析 extra data 拿到 target addr
        // ch, reqs, _ := newCh.Accept()
        // go func: net.Dial target → io.Copy 双向
    }
}
```

**复用现有 `fakeShellServerForLoginFlow`** 作为 fake target（已有 shell + detect + 命令执行能力）。

### 测试用例

**`internal/ssh/conn/dialer_test.go`（DialThrough 单元测试）：**
- `TestDialThroughSuccess`：经 fake jumphost 拨号到 fake target，验证返回的 client 能开 session
- `TestDialThroughTargetAuthFails`：target 密码错 → error 含 "permission denied"
- `TestDialThroughJumphostRejectsForwarding`：jumphost `allowForwarding=false` → error 含 "administratively prohibited"
- `TestDialThroughTargetHostKeyChanged`：target host key 变更 → error 含 "host key changed"
- `TestDialThroughOpenDirectTcpipTimeout`：jumphost 卡住不响应 direct-tcpip → 10s 超时

**`internal/config/validate_test.go`：**
- `TestPatternARejectsServerProxy`：`via.ssh_j=true` + `server.proxy` 非空 → 报错
- `TestPatternADirectAllowsServerProxy`：`via=nil` + `server.proxy` 非空 → 通过
- `TestPatternBIgnoresServerProxy`：`via.ssh_j=false` + `server.proxy` 非空 → 通过

**`internal/mcp/tools_session_patterA_test.go`（端到端，新建文件）：**
- `TestIntegrationPatternAEndToEnd`：jumphost + target 都正常 → login 成功 → run_in_session 跑命令 → close。验证 `sftp_available=true`（区别于 Pattern B）
- `TestIntegrationPatternAJumphostAuthFails`：jumphost 密码错 → login 报错，无 login_trace
- `TestIntegrationPatternATargetAuthFails`：target 密码错 → login 报错，无 login_trace，错误含 "through jumphost"
- `TestIntegrationPatternAHostKeyChanged`：target host key 变更 → login 报错 "host key changed"
- `TestIntegrationPatternALoginFlowSuccess`：SSHServer.LoginFlow 跑通（模拟 su 交互）→ login 成功 → login_trace 含 `stage="patternA"`
- `TestIntegrationPatternALoginFlowFailureReturnsTrace`：LoginFlow expect 未命中 → login 报错 + login_trace
- `TestIntegrationPatternACloseReleasesBothClients`：close 后 jumphost 和 target 的 SSH conn 都断开（验证 Close 顺序与资源释放，用 conn 计数或 listener Accept 观察）

**关键测试：`TestIntegrationPatternACloseReleasesBothClients`**

验证生命周期正确性——这是 Pattern A 最容易出 bug 的地方（jumpClient 泄漏）。方法：
- fake jumphost 和 fake target 都跟踪 active conn 数（`atomic.Int32`，Accept 时 +1，conn.Close 时 -1）
- login → close 后，两者 active conn 数都应归 0
- 超时断言（如 5s）防 hang

### 测试覆盖矩阵

| 场景 | 单元测试 | 集成测试 |
|------|---------|---------|
| DialThrough 拨号成功 | ✅ | ✅（端到端）|
| Jumphost auth 失败 | — | ✅ |
| Target auth 失败 | ✅ | ✅ |
| Jumphost 拒绝转发 | ✅ | — |
| Target host key 变更 | ✅ | ✅ |
| direct-tcpip 超时 | ✅ | — |
| LoginFlow 成功 | — | ✅ |
| LoginFlow 失败 + trace | — | ✅ |
| Close 释放两层 client | — | ✅ |
| Server.Proxy 校验 | ✅ | — |
| SFTP 可用 | — | ✅ |

### 不测的

- **多跳 jumphost 链（A→B→target）**：设计文档明确 YAGNI，不测
- **真实网络环境的 ssh -J**：靠 mock 覆盖核心路径，真实环境验收留 MCP Inspector（与 Pattern B 一致）

## 文档同步

实现完成后同步更新三处文档：

1. **`README.md`**
   - 删除 `ssh_j=true` 字段说明里的 "v1.x 实现，当前会拒绝" / "（v1.x）" 等限定
   - 删除 "形态与使用约束" 里 `ssh_j=true` "v1.x 实现，当前会拒绝" 的注释
   - 加一个 Pattern A 的配置示例（与 Pattern B 示例并列）
   - "后续迭代" 节里把 "v1.x：Pattern A" 一条移除

2. **`docs/ssh-session-manager-design.md`**
   - §3.1 里 "Pattern A (srv.Via.SSHJ=true)：经 jumphost 的 direct-tcpip 通道 SSH 到 target（v1.x 实现，当前拒绝）" 改为已实现的描述
   - §3.7 注入流程时序图里，Pattern A 路径补完（jumphost dial → direct-tcpip → target dial → OpenPtyConn → 可选 LoginFlow → DetectShell → InjectRC → TryEnableSftp）
   - 加一个 "Pattern A 实现" 子节，简述 DialThrough + SetJumpClient 的生命周期管理

3. **`docs/implementation-plan.md`**
   - 阶段表格加一行阶段 6："Pattern A（ssh -J 透明转发）"，状态 ✅ 完成，commit hash 待填
   - 加 "阶段 6 详细设计" 节，内容为本 spec 的精简版

## 实现顺序建议

1. `Dialer.DialThrough` + 单元测试（`dialer_test.go`）
2. `PtyConn.SetJumpClient` + Close 顺序调整（无独立测试，靠集成测试覆盖）
3. `setupPatternA` + Login handler 路由改造
4. 配置校验（`validate.go`）+ 测试
5. 端到端集成测试（`tools_session_patterA_test.go`）
6. 跑 `go test -race ./...` 全绿
7. 文档同步
8. Commit + push
