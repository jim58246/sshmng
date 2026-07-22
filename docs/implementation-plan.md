# SSH 会话管理工具 v1 实施计划

> 跟踪实施进度。设计文档见 [ssh-session-manager-design.md](./ssh-session-manager-design.md)。
>
> 每阶段独立提交，commit message 风格：`phase N: <module> - <short desc>`。每阶段遵循 TDD：先写测试再写实现。

## 阶段进度

| 阶段 | 主题 | 状态 | Commit |
|------|------|------|--------|
| 1 | 项目骨架 + 配置管理 + MCP CRUD 工具 | ✅ 完成 | `34f8311` |
| 2 | SSH 连接层 + 直连会话（Pattern A，无 LoginFlow） | ✅ 完成 | `96b669a` |
| 3 | LoginFlow 决策树执行器（Pattern A 下 SSHServer.LoginFlow） | ✅ 完成 | `7fe6a2d` |
| 4 | Pattern B 交互式堡垒机 | ✅ 完成 | `56ba3dd` |
| 5 | sftp + get_trace + 其余工具 | ✅ 完成 | `ba4e197` |
| 5.1 | sentinel 误匹配 + Ctrl-C 失效修复（A+D 方案） | ✅ 完成 | （已合入主线） |
| 5.2 | token 化 sentinel（bash/zsh）+ 日志改配置文件 + zombie 修复 | ✅ 完成 | （已合入主线） |
| 6.1 | login 硬超时（NewSession/RequestPty/Shell） | ✅ 完成 | — |
| 6.2 | 模块 3 层目录拆解（conn/ + pty/ + session/） | ✅ 完成 | — |
| 6.3 | 模块测试补全（sentinel / PTY / 状态机 / 连接层） | ✅ 完成 | — |
| 6.4 | MCP 描述增强（Instructions + jsonschema）+ 多关键字 AND 搜索 | ✅ 完成 | `5926c70` |

## 项目结构

当前结构（阶段 1-5 完成后）：

```
sshmng/
├── cmd/sshmng/main.go              # MCP server 入口（stdio 模式）
├── internal/
│   ├── config/                     # 数据模型 + config.json 加载/保存/校验/CRUD
│   ├── loginflow/                  # 决策树执行器（纯逻辑）
│   ├── ssh/                        # SSH 连接层 + session 管理（当前混合，待拆 3 层）
│   │   ├── dialer.go               # 拨号 + auth + TOFU host key + 代理
│   │   ├── pty.go                  # PTY 分配、sentinel 匹配、命令边界识别
│   │   ├── session.go              # session 状态机（idle/running/closed）、Manager
│   │   ├── known_hosts.go          # TOFU known_hosts 文件管理
│   │   ├── normalize.go            # ANSI 过滤、输出清洗
│   │   ├── sentinel.go             # PS1/exit sentinel 解析
│   │   ├── shell_detect.go         # shell 类型探测 + RC 注入脚本生成
│   │   ├── sftp.go                 # sftp 通道建立/可用性
│   │   └── trace.go                # CommandTrace + graveyard
│   └── mcp/                        # MCP server + 工具 handler
└── docs/
```

**目标结构（阶段 6 拆解后，按职责 3 层）：**

```
sshmng/
├── cmd/sshmng/main.go
├── internal/
│   ├── config/                     # 数据模型 + config.json（不变）
│   ├── loginflow/                  # 决策树执行器（不变）
│   ├── ssh/                        # 3 层拆分
│   │   ├── conn/                   # L1 连接层：拨号、auth、TOFU、sftp 通道
│   │   │   ├── dialer.go           # Dial + 代理 + auth
│   │   │   ├── known_hosts.go      # TOFU store
│   │   │   ├── sftp.go             # sftp 通道建立
│   │   │   └── *_test.go
│   │   ├── pty/                    # L2 PTY 层：PTY 读写、sentinel 匹配、RC 注入
│   │   │   ├── pty.go              # PtyConn：OpenPtyConn / Run / Close / Upload / Download
│   │   │   ├── read.go             # readLoop + readUntilCommandDone + pushback
│   │   │   ├── sentinel.go         # PS1-only sentinel 正则 + ExtractExitCode
│   │   │   ├── normalize.go        # ANSI 过滤 + CleanOutput
│   │   │   ├── shell_detect.go     # shell 探测 + BuildRC（PS1-only，无 PROMPT_COMMAND）
│   │   │   └── *_test.go
│   │   └── session/                # L3 状态机层：session 生命周期 + Manager + trace
│   │       ├── session.go          # Session 状态机（idle/running/closed）
│   │       ├── manager.go          # Manager：map[sid]*Session + graveyard
│   │       ├── trace.go            # CommandTrace + truncateTraces
│   │       └── *_test.go
│   └── mcp/                        # MCP server + 工具 handler（不变）
└── docs/
```

**3 层职责契约：**

- **L1 连接层** (`conn/`)：输入 `DialOptions`，输出 `*ssh.Client` + sftp 通道。不关心 PTY / sentinel / session 状态。可独立测试（mock SSH server）。
- **L2 PTY 层** (`pty/`)：输入 `*ssh.Client` + `sid` + 可选 `LoginFlow`，输出 `Conn` 接口（Run/Close/Upload/Download/SftpAvailable）。负责 sentinel 匹配、pushback、drain、Ctrl-C。可独立测试（io.Pipe 模拟 stdin/stdout）。
- **L3 状态机层** (`session/`)：输入 `Conn` 接口，输出 session 生命周期管理（idle/running/closed + idle timeout + trace）。不关心 PTY / SSH。可独立测试（fakeConn）。

**拆解动机**：当前 `internal/ssh/` 8 个文件职责纠缠——pty.go 既做 PTY 读写又做 sentinel 匹配，session.go 既做状态机又持有 Conn。测试分散，改一处怕动多处。拆 3 层后每层有明确接口，可独立测试，改动隔离。

## 依赖（go.mod）

- `github.com/modelcontextprotocol/go-sdk` v1.x
- `golang.org/x/crypto/ssh`
- `github.com/pkg/sftp`（阶段 5 加入）
- `golang.org/x/net/proxy`（SOCKS5）
- Go 版本：1.22+

## 阶段 1：项目骨架 + 配置管理 ✅

**目标**：能加载/保存 `config.json`，MCP 暴露 `list_*` / `get_*` / `update_*` 三类 CRUD 工具。无 SSH 连接。

**交付物**：
- `internal/config/{types,store,validate,crud}.go` — 数据模型、原子写、0600 权限校验、JSON Merge Patch (RFC 7396)、引用完整性校验
- `internal/mcp/{server,tools_config}.go` — 9 个 CRUD 工具注册与 handler
- `cmd/sshmng/main.go` — 入口，配置路径解析（`--config` / `$SSHMNG_HOME` / `$HOME/.sshmng`）

**验证**：`go test ./internal/config/...` 全绿；MCP Inspector 能调用 9 个 CRUD 工具。

## 阶段 2：SSH 连接层 + 直连会话 ✅

**目标**：`login` 能拨通直连 server（`Via` 为空、无 LoginFlow），`run_in_session` 能跑命令并解析 sentinel，`close_session` / `stat` 工作。

**交付物**：
- `internal/ssh/pty/normalize.go` — ANSI CSI 序列剥离 + sentinel 行清理
- `internal/ssh/pty/sentinel.go` — `DetectShellReady` / `ExtractExitCode` / `TruncateOutput`
- `internal/ssh/pty/shell_detect.go` — `ParseShellDetect` / `BuildRC`（bash/zsh/dash 分支）
- `internal/ssh/conn/known_hosts.go` — TOFU store，0600 权限，原子写
- `internal/ssh/session/session.go` — `Session` 状态机（idle/running/closed）+ `Manager` map，idle timeout
- `internal/ssh/conn/dialer.go` — 拨号 + 密码/私钥 auth + TOFU + SOCKS5/HTTP CONNECT 代理
- `internal/ssh/pty/pty.go` — `PtyConn`：PTY 分配、shell 探测、RC 注入、`Run`/`Close`/`Upload`/`Download`
- `internal/mcp/tools_session.go` — `login` / `run_in_session` / `close_session` / `stat` 4 个 MCP 工具

**实现要点**：
- `PtyConn` 用单 reader goroutine 从 stdout 读取并投递到 channel，避免多 goroutine 竞争 SSH channel 的 `Read`
- TOFU：首次见到 host key 记录到 `known_hosts`；后续比对；变更拒绝并报 "possible MITM"
- 私钥文件权限必须 0600 或更严（group/other 任何权限位都拒绝）
- sentinel：bash/zsh 用 PS1-only `$(echo _$?)__<sid>_<token>__]# `（prompt 展开时捕获 exit code + token）；dash/ash 用固定 `__P_<sid>__> `（无 exit code）；sid 为 8 字节十六进制随机串，token 为每次 Run 生成的 8 字节十六进制随机串
- session 状态机：`run_in_session` 仅在 idle 时可调；running 时报 "session busy"；closed 时报 "session closed"
- idle timeout：`time.AfterFunc`，命令执行期间停止 timer，结束后重置

**验证**：
- `go test -race ./internal/ssh/...` 全绿（11 dialer + 6 集成 + 单元测试）
- `go test -race ./internal/mcp/...` 全绿（9 session 工具测试，含 fake SSH server 端到端）
- fake SSH server 实现：用 `golang.org/x/crypto/ssh` 起 server，shell 用 Go goroutine 模拟（响应 `__SHELL_DETECT__`、识别 `stty -echo` 作为 RC 结束标记、用 `sh -c` 执行命令并发射 sentinel）

**已知限制**（v1 phase 2）：
- 不支持 jumphost（`Via` 非空时 login 报错）
- 不支持 LoginFlow（`SSHServer.LoginFlow` 非空时 login 报错）
- 不支持 sftp（`sftp_available` 恒为 false）
- 不支持 `get_trace` MCP 工具（PtyConn 已实现，但 MCP handler 未注册）

## 阶段 3：LoginFlow 决策树执行器 ✅

**目标**：支持 `SSHServer.LoginFlow`（target 认证后交互，如 su / 角色选择 / PAM）。Pattern A 下 login 流程：SSH auth → target shell 就绪 → SSHServer.LoginFlow（如有）→ 注入 RC → idle。

**TDD 测试用例**：

`internal/loginflow/executor_test.go`（纯逻辑，用 fake PTY）
- 入口 Action = LoginEntry 指向的 Action
- Send 空 → 跳过发送，直接 expect
- Expects 按顺序尝试，命中第一个跳转 Next
- `Next == "success"` → 登录成功
- 所有 Expects 未命中 → 失败，trace 记录 send/expect/output
- 单 Action TimeoutMs 超时 → 失败
- MaxSteps 超限 → 失败
- GlobalTimeoutMs 超限 → 失败
- glob pattern 匹配（`Please select*`）
- `re:` 前缀正则匹配
- ANSI 过滤在 expect 匹配前应用（output 含 `\x1b[0m` 不影响匹配）
- trace 结构正确（Time / ElapsedMs / Send / Expect / Output）

**实现要点**：
- `executor.go` 接口：`Run(pty PTY, flow map[string]LoginAction, entry string) (trace []TraceEntry, err error)`
- PTY 接口抽象：`Send(s string)`、`Read(deadline time.Time, mustContain string) (output string, timedOut bool)`，便于测试用 fake PTY
- ANSI 过滤复用 `internal/ssh/pty/normalize.go`
- TimeoutMs / MaxSteps / GlobalTimeoutMs 用 0 = 默认值（10000 / 50 / 60000）

**集成**：`internal/ssh/session/session.go` 在 SSH auth 完成、target shell 就绪后调 `loginflow.Run`，成功后再注入 RC

**验证**：
- `go test ./internal/loginflow/...` 全绿
- mock SSH server 配置一个简单 SSHServer.LoginFlow（如 `su -` 切换用户），MCP Inspector 跑通

**关键文件**：`internal/loginflow/{executor,trace}.go`、`internal/ssh/session/session.go`（集成）

## 阶段 4：Pattern B 交互式堡垒机 ✅

**目标**：支持 `Jumphost.SSHJ=false`。两段式 LoginFlow：Jumphost.LoginFlow 准备到主菜单就绪 → SSHServer.LoginFlow 接管登录 target。

**TDD 测试用例**：

`internal/mcp/tools_session_jumphost_test.go`
- Pattern B 端到端：login → run_in_session → close_session。fake jumphost server 模拟菜单 → target 选择 → target 凭据 → target shell 全流程，全程同一 SSH session
- Jumphost.LoginFlow 失败（pattern 不匹配）→ login 报错，error 含 "loginflow" / "no expect matched"
- Pattern A (`Via.SSHJ=true`) → login 拒绝（"pattern A via ssh-j jumphost not yet supported"，留 v1.x 实现）

**实现要点**：
- **PTY 接口重设计**（关键）：`loginflow.PTY.Read` 从 `(deadline, mustContain string)` 改为 `(deadline, matchers []*regexp.Regexp) (output, matchedIdx, timedOut, err)`。matcher 命中即停，trailing data 留 pushback。这是 Pattern B 的关键——第一段流的最后一次 Read 不能吞掉第二段流要等的 prompt
- **NewPtyConn 拆分**：`OpenPtyConn`（detect only）+ `RunLoginFlow`（可链式调用）+ `InjectRC`。原有 `NewPtyConn` 保留为直连场景的便捷构造器
- **stripANSIWithPos**：剥离 ANSI 同时返回位置映射，用于把 stripped 中的 match 末尾映射回 raw 字节位置切分 pushback
- **Login handler 分支**：`srv.Via == nil` → 直连；`srv.Via.SSHJ=true` → 拒绝（v1.x）；`srv.Via.SSHJ=false` → setupPatternB（拨号 jumphost → OpenPtyConn → RunLoginFlow(jump) → RunLoginFlow(server) → InjectRC）
- 两段 LoginFlow 共用同一 PTY，trailing data 通过 pushback 在调用间保留

**验证**：
- `go test -race ./...` 全绿（含 Pattern B 端到端 + jumphost flow 失败 + Pattern A 拒绝）
- MCP Inspector 待真实环境验收（mock 已覆盖核心路径）

**关键文件**：`internal/loginflow/executor.go`（PTY 接口）、`internal/ssh/pty/pty.go`（OpenPtyConn/RunLoginFlow/InjectRC + pushback 切分）、`internal/mcp/tools_session.go`（setupDirect/setupPatternB 分支）

## 阶段 5：sftp + 其余工具 ✅

**目标**：补齐 `upload` / `download` / `get_trace`，完成 v1 全部 MCP 工具。

**TDD 测试用例**：

`internal/ssh/pty/sftp_test.go`
- sftp 通道在 login 时同步建立（5s 超时）
- sftp 不可用时 `stat()` 返回 `sftp_available=false`
- sftp 不可用时 `upload` / `download` 报错 "sftp not available for this session"
- `upload` 正常路径：本地文件 → 远端，返回字节数
- `upload` 超时：返回已传输字节，timed_out=true
- `download` 同上

`internal/ssh/session/trace_test.go`
- `get_trace(sid, last_n)` 返回最近 N 轮
- `get_trace(sid)` 返回全部
- `trunc_output` 截断参数生效（默认 200，0 不截断）
- `close_session` 后 trace 保留 10 分钟自动清理（用 fake clock）

**MCP handler**：`internal/mcp/tools_file.go`（upload/download）、`internal/mcp/tools_session.go` 扩展（get_trace）

**验证**：
- `go test ./...` 全绿
- MCP Inspector 端到端走完所有工具

## 阶段 5.1：sentinel 误匹配 + Ctrl-C 失效修复 ✅

**问题**：
1. `readUntilPatternTimeout` 用 `bytes.Index` 找第一个 PS1 sentinel，命令输出含 PS1 字面量（如 `echo $PS1`）时误匹配，trailing 进 pushback，下次 Run 直接从 pushback 匹配返回——命令根本没执行，返回上次命令残留
2. Ctrl-C drain 失败后远端命令仍在跑，下次 Run 的 cmd 被旧命令消费，输出混乱

**修复（A+D 方案）**：
- **A**：Run 等 PS1-only sentinel（`_-?\d+__<sid>_<token>__]# ` 正则，`$(echo _$?)` 在 prompt 展开时捕获 exit code），而非单独 PS1 字面量。避免 PS1 字面量误匹配
- **D**：drain 超时后强制 Close SSH channel，终止远端命令。session 进 closed，Agent 需重新 login

> 注：阶段 5.1 的 exit+PS1 组合 sentinel（`__E_<sid>:N__\r\n__P_<sid>__> `）已被后续 PS1-only 重构取代——审计机器把 `PROMPT_COMMAND` 设为只读会破坏组合 sentinel 的 PROMPT_COMMAND 路径。新设计见 `docs/ssh-session-manager-design.md` 3.7 节"命令边界识别"。

**测试**：
- `TestRunIgnoresPS1LiteralInCommandOutput`：命令输出含 PS1 字面量，验证下次 Run 正常执行
- `TestRunReturnsConnUnusableWhenDrainTimesOut`：drain 超时后 PtyConn 返回 connUnusable=true（不自己 Close），Session 据此调 Close

**关键文件**：`internal/ssh/pty/pty.go`（readUntilCommandDone + Run 三段式超时）、`internal/ssh/pty/pty_combo_test.go`

## 阶段 6：模块 3 层拆解 + login 硬超时 + 模块测试

**目标**：提升稳定性与可测试性。三件事独立提交。6.1 / 6.3 已完成，6.2 待开始。

### 6.1 login 硬超时修复 ✅

**问题**：`client.NewSession()` / `session.RequestPty()` / `session.Shell()` 无 per-operation 超时，网络半开时卡几分钟。

**修复**：给 login 流程加 global deadline，超时强制 Close。用 goroutine + select 包裹 SSH 协议操作。

**测试**：mock SSH server 不响应 Shell 请求，验证 login 在 deadline 内返回 error。

### 6.2 模块 3 层目录拆解 ✅

**拆解动机**：`internal/ssh/` 单 package 职责纠缠——pty.go 既做 PTY 读写又做 sentinel 匹配，session.go 既做状态机又持有 Conn 接口。测试分散，改一处怕动多处。拆 3 层后每层有明确接口，可独立测试，改动隔离。

**已完成**：
- close 决策从 PtyConn 上移到 Session（PtyConn.Run 返回 `connUnusable=true`，Session 据此调 `s.Close()`）——传输层不做状态决策
- `internal/ssh/` 拆成 3 个子包，每层有明确职责：

```go
// L1 internal/ssh/conn/ — 拨号 + TOFU + sftp 通道建立
package conn

type Dialer struct{...}
func (d *Dialer) Dial(opts DialOptions) (*ssh.Client, error)
type KnownHostsStore struct{...}
func NewSftpClient(client *ssh.Client) (*sftp.Client, error)  // 5s 超时
var ErrSftpUnavailable error

// L2 internal/ssh/pty/ — PTY 连接 + sentinel 解析 + 终端规范化
package pty

type PtyConn struct{...}  // 持有 *ssh.Client / *ssh.Session / *sftp.Client
func OpenPtyConnWithTimeout(client *ssh.Client, sid string, logger *slog.Logger, timeout time.Duration) (*PtyConn, error)
func (p *PtyConn) Run(cmd string, timeoutMs, maxOutputBytes int) (output, rawOutput string, exitCode int, timedOut, ctrlCSent, truncated bool, totalBytes int, connUnusable bool, err error)
func (p *PtyConn) SftpAvailable() bool
func (p *PtyConn) Upload(src io.Reader, remotePath string, timeoutMs int) (int, bool, error)
func (p *PtyConn) Download(remotePath string, dst io.Writer, timeoutMs int) (int, bool, error)
func (p *PtyConn) RunLoginFlow(...) ([]loginflow.TraceEntry, error)
func (p *PtyConn) InjectRC() error

// L3 internal/ssh/session/ — 状态机 + Manager + trace
package session

type Conn interface {  // PtyConn 实现此接口；测试用 fakeConn 也实现
    Close() error
    Run(...) (...)
    SftpAvailable() bool
    Upload(...) (...)
    Download(...) (...)
}
type Session struct{...}  // 状态机 + idle timeout + trace
type Manager struct{...}  // map[sid]*Session + graveyard
```

**分层依赖**：
- `conn/` 无内部依赖（只用外部 `golang.org/x/crypto/ssh`、`github.com/pkg/sftp`）
- `pty/` 依赖 `conn/`（用 `conn.RandomSID`、`conn.NewSftpClient`、`conn.ErrSftpUnavailable`）
- `session/` 无内部依赖（`Conn` 接口自包含，`PtyConn` 隐式实现；测试用 `fakeConn` 也定义在 session 包）

**拆解步骤**：
1. 创建 `internal/ssh/conn/`、`internal/ssh/pty/`、`internal/ssh/session/` 目录
2. `git mv` 文件到对应目录，更新 `package` 声明
3. `sftp.go` 拆分：`conn/sftp.go`（`NewSftpClient` + `ErrSftpUnavailable` + `DefaultTransferTimeout`）+ `pty/sftp.go`（`PtyConn.SftpAvailable/Upload/Download` 方法 + `copyCtx` helper）
4. `pty.go` 加 `import "sshmng/internal/ssh/conn"`，`RandomSID` → `conn.RandomSID`，`newSftpClient` → `conn.NewSftpClient`
5. pty 测试文件加 `import "sshmng/internal/ssh/conn"`，`DialOptions` → `conn.DialOptions`，`RandomSID` → `conn.RandomSID`，本地定义 `newDialerWithTempKnownHosts`（封装 `conn.NewDialer` + `conn.NewKnownHostsStore`）
6. session 测试 `fakeConn` 的 `errSftpUnavailable` → `conn.ErrSftpUnavailable`
7. 外部消费者更新：`cmd/sshmng/main.go`（`ssh.NewKnownHostsStore` → `conn.NewKnownHostsStore`）、`internal/mcp/server.go`（`ssh.KnownHostsStore` → `conn.KnownHostsStore`、`ssh.Manager` → `session.Manager`）、`internal/mcp/tools_session.go`（`ssh.RandomSID/NewDialer/Dialer/DialOptions` → `conn.*`、`ssh.PtyConn/OpenPtyConnWithTimeout/LoginFlowOptions/LoginFlowError` → `pty.*`）、5 个 mcp 测试文件（`ssh.NewKnownHostsStore` → `conn.NewKnownHostsStore`）
8. `go test -race ./...` 全绿

### 6.3 模块测试补全 ✅

针对性补测试：

- **sentinel 模块**：PS1-only sentinel 正则匹配（exit code 变量、字面量碰撞、跨边界）+ token 化匹配（精确 token、旧 token 不匹配）
- **PTY 模块**：pushback 跨 Run 持久化、drain 成功/失败、Ctrl-C 发送、setup token 超时返回 connUnusable、sentinel 分片组装
- **状态机模块**：idle→running→idle 转换、closed 态拒绝所有操作、idle timeout 重置、connUnusable 触发 Session.Close
- **连接层模块**：Dial 超时、TOFU 新增/变更、代理拨号

**验证**：`go test -race ./...` 全绿。

**关键文件**：`internal/ssh/pty/pty_token_test.go`、`internal/ssh/pty/pty_combo_test.go`、`internal/ssh/pty/pty_ctrlc_test.go`、`internal/ssh/session/session_test.go`、`internal/ssh/pty/sentinel_test.go`、`internal/ssh/pty/shell_detect_test.go`

### 6.4 MCP 描述增强 + 多关键字搜索 ✅

**问题**：基本功能测试 OK，但 tool description 和 server 级元信息不足以支撑 Agent 正确使用 MCP——Agent 只能从各 tool description 拼凑工作流，容易漏掉"失败时调 get_trace"、"session 复用"、"idle timeout"、"Pattern B 不支持 sftp"等关键约束。同时 `list_*` 的单关键字子串搜索不符合实际"多关键字逐步缩小范围"的用法。

**修复**：

- **Server Instructions**：`internal/mcp/server.go` 新增 `serverInstructions` 常量（Entity model / Workflow / Session semantics / Session lifecycle / Failure recovery 五段，约 1.9KB 塞在 Claude Code 2KB 截断阈值下），通过 `mcp.ServerOptions.Instructions` 传给 MCP server，Agent 在 initialize 响应里收到完整文本。新增 Session semantics 节明确"session 间互不干扰 + session 内 PTY 状态延续"心智模型
- **Tool description 重写**：14 个工具的 description 全部加上 workflow 上下文 + 失败模式 + 诊断路径；修复 3 处过时描述（login 的 "phase 2 direct only"、get_trace 的 "inputs"、"9 CRUD tools" 注释）
- **Args jsonschema 增强**：`LoginArgs.Name` / `RunInSessionArgs.TimeoutMs` / `MaxOutputBytes` / `GetTraceArgs.TruncOutput` / `UploadArgs` / `DownloadArgs` / `UpdateArgs.Patch` 都加上失败模式 + 诊断路径说明
- **多关键字 AND 搜索**：`internal/config/crud.go` 的 `matchesQuery` 重写为 `strings.Fields` 分词 + AND 语义，每个关键字独立子串匹配 name/addr/tags（大小写不敏感）；`list_ssh_servers` / `list_jumphosts` / `list_proxies` 的 query 现支持 `"prod web"` 这样的多关键字逐步缩小范围

**测试**：

- `internal/mcp/server_test.go` 新增 `TestNewServerSetsInstructions`：通过 `mcp.NewInMemoryTransports()` 连 client+server，读 `ClientSession.InitializeResult().Instructions` 验证 Agent 实际看到的 initialize 响应非空且含 11 个关键关键词
- `internal/config/crud_test.go` 新增 6 个多关键字测试：AND 语义 / 跨字段匹配 / 大小写不敏感 / 多余空格压缩 / 无匹配 / 纯空白返回全部

**验证**：`go test -race ./...` 全绿。

**关键文件**：`internal/mcp/server.go`（serverInstructions + NewServer）、`internal/mcp/tools_session.go` + `internal/mcp/tools_file.go`（Args jsonschema）、`internal/config/crud.go`（matchesQuery）、`internal/mcp/server_test.go` + `internal/config/crud_test.go`（测试）

## 跨阶段事项

- **代理（Proxy）支持**：阶段 2 拨号时一并实现 SOCKS5（`golang.org/x/net/proxy`）+ HTTP CONNECT（自写约 50 行）。不单独成阶段。✅ 已完成
- **MCP server 启动参数**：`SSHMNG_HOME` 环境变量、`--config <path>` 命令行参数（用 `flag` 库），阶段 1 实现。✅ 已完成
- **错误处理统一**：SSH auth 失败仅 error 字符串；LoginFlow 失败 error + login_trace；命令失败按需 get_trace。三类失败的 handler 包装在 `internal/mcp/server.go`。✅ 阶段 1-2 已实现 SSH auth / 命令失败两类
- **并发安全**：session map 与 config 各自持锁；同一 session 的 `run_in_session` 靠 state=running 串行化。所有 MCP handler 必须并发安全。✅ 已完成
- **日志处理**：✅ 已完成（配置文件 + stderr bootstrap 兜底；详见设计文档 §3.8）。旧方案 MCP notifications 已废弃

## 待讨论

### 日志处理 ✅ 已决定（2026-07-21 修订）

**决定**：配置文件 + stderr bootstrap 兜底（旧方案"MCP notifications + stderr 兜底"已废弃）。

- **操作日志**（session 创建/关闭、idle timeout 触发、TOFU host key 新增/变更、login 失败、命令超时等）走 `slog` + `RotatingWriter`，写到 `<log_path>/sshmng.log`，10MB 轮转、最多 5 份（`sshmng.log` + `sshmng.1.log` ~ `sshmng.4.log`），文件权限 0600
- **`log_path` 为空**时不打日志（io.Discard），彻底静默
- **`log_level`** 控制级别：`debug` / `info` / `warn` / `error`（支持缩写 `dbg`/`d`/`inf`/`i`/`w`/`err`/`e`，大小写不敏感）；空 = 默认 `info`；配错 Load 报错
- **stderr** 只留 bootstrap 错误（MCP 还没起来时，如配置加载失败、known_hosts 权限错）和 fatal panic
- **stdout** 严禁写日志（JSON-RPC 专用）
- **不通过 MCP `notifications/message` 推日志**（旧方案废弃，原因见下）

**为什么废弃 MCP notifications 方案**：
- MCP SDK 的 `LoggingHandler.Handle` 同步调 `ss.Log()` → `handleNotify` → `ioConn.Write`，和 tool result 共用 stdout JSON-RPC 通道、由 `writeMu` 串行化。client 处理慢时 result 等不到发送机会，工具"卡住"
- DEBUG 日志作为 `notifications/message` 进入 Agent 上下文，占 context window、干扰决策
- `sessionLogger` 每次调用都 `NewLoggingHandler`，`lastMessageSent` 不跨调用共享，`MinInterval=100ms` 限流实际失效——DEBUG 日志全量发送
- 配置文件方案彻底分离日志通道与 JSON-RPC 通道，问题消除

**实现要点**：
- `cmd/sshmng/main.go`：bootstrap 阶段用 `slog.NewTextHandler(os.Stderr, ...)` 作为 bootstrapLogger（仅用于 config 加载前的 fatal 错误）；config 加载成功后调 `openLogWriter(logPath)` 创建 RotatingWriter（或 io.Discard），切到文件 logger
- `internal/config/types.go`：Config 加 `LogLevel` / `LogPath` 字段；`ParseLogLevel` 支持缩写
- `internal/mcp/rotating_writer.go`：`RotatingWriter` 实现 `io.Writer`，超过 maxSize 轮转，最多 maxBackups 份
- `internal/mcp/server.go`：`Service` 持有 `baseLogger`；`sessionLogger(req, sid)` 直接返回 `baseLogger.With("sid", sid)`，不走 `mcp.NewLoggingHandler`
- `internal/ssh/session/session.go`：`Session` 持有 `logger *slog.Logger`；`NewSession`/`newSessionWithConn` 新增 logger 参数（nil 退化为 discard）
- 日志内容不含 password / private_key / passphrase（敏感字段不打）；DEBUG 级会完整记录 LoginFlow send / PTY stdout 片段，分享时注意脱敏

**状态**：✅ 已实施。

## 端到端验证

每阶段交付后用 MCP Inspector 验证：

```bash
npx @modelcontextprotocol/inspector go run ./cmd/sshmng
```

阶段 5 完成后做完整端到端：
1. 用 mock SSH server（带堡垒机菜单）做集成测试
2. 用真实堡垒机（如有测试环境）做验收测试
3. 用 Claude Code `--mcp-debug` 验证 Agent 集成路径
