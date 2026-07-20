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
| 4 | Pattern B 交互式堡垒机 | ✅ 完成 | （本次提交） |
| 5 | sftp + send_input/send_special/get_trace + 其余工具 | ⏳ 待开始 | — |

## 项目结构

```
sshmng/
├── go.mod
├── go.sum
├── cmd/
│   └── sshmng/
│       └── main.go                 # MCP server 入口（stdio 模式）
├── internal/
│   ├── config/                     # 数据模型 + config.json 加载/保存/校验/CRUD
│   │   ├── types.go                # Proxy / Jumphost / SSHServer / SSHAuth / LoginAction / Expect / TraceEntry
│   │   ├── store.go                # Load / Save（原子写）/ 0600 权限校验
│   │   ├── validate.go             # SSHJ/LoginFlow 一致性、Auth 必空、引用完整性
│   │   ├── crud.go                 # List / Get / Update（JSON Merge Patch, RFC 7396）
│   │   └── *_test.go
│   ├── loginflow/                  # 决策树执行器（纯逻辑）—— 阶段 3
│   │   ├── executor.go             # 遍历决策树，send+expect+ANSI 过滤
│   │   ├── trace.go                # TraceEntry 累积
│   │   └── *_test.go
│   ├── ssh/                        # SSH 连接层 + session 管理
│   │   ├── dialer.go               # 拨号 + auth + TOFU host key + 代理
│   │   ├── pty.go                  # PTY 分配、sentinel 注入、命令边界识别
│   │   ├── session.go              # session 状态机（idle/running/closed）、map[sid]*Session
│   │   ├── known_hosts.go          # TOFU known_hosts 文件管理
│   │   ├── normalize.go            # ANSI 过滤、输出清洗
│   │   ├── sentinel.go             # PS1/exit sentinel 解析
│   │   ├── shell_detect.go         # shell 类型探测 + RC 注入脚本生成
│   │   ├── sftp.go                 # sftp 通道建立/可用性 —— 阶段 5
│   │   └── *_test.go
│   └── mcp/                        # MCP server + 工具 handler
│       ├── server.go               # 注册工具、stdio 监听
│       ├── tools_session.go        # login / run_in_session / close_session / stat
│       ├── tools_config.go         # list_* / get_* / update_*
│       ├── tools_file.go           # upload / download —— 阶段 5
│       └── *_test.go
├── testdata/                       # 测试 fixtures（sample config.json、mock 堡垒机菜单输出等）
└── docs/
    ├── ssh-session-manager-design.md
    └── implementation-plan.md      # 本文档
```

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
- `internal/ssh/normalize.go` — ANSI CSI 序列剥离 + sentinel 行清理
- `internal/ssh/sentinel.go` — `DetectShellReady` / `ExtractExitCode` / `TruncateOutput`
- `internal/ssh/shell_detect.go` — `ParseShellDetect` / `BuildRC`（bash/zsh/dash 分支）
- `internal/ssh/known_hosts.go` — TOFU store，0600 权限，原子写
- `internal/ssh/session.go` — `Session` 状态机（idle/running/closed）+ `Manager` map，idle timeout
- `internal/ssh/dialer.go` — 拨号 + 密码/私钥 auth + TOFU + SOCKS5/HTTP CONNECT 代理
- `internal/ssh/pty.go` — `PtyConn`：PTY 分配、shell 探测、RC 注入、`Run`/`SendInput`/`SendSpecial`/`Close`
- `internal/mcp/tools_session.go` — `login` / `run_in_session` / `close_session` / `stat` 4 个 MCP 工具

**实现要点**：
- `PtyConn` 用单 reader goroutine 从 stdout 读取并投递到 channel，避免多 goroutine 竞争 SSH channel 的 `Read`
- TOFU：首次见到 host key 记录到 `known_hosts`；后续比对；变更拒绝并报 "possible MITM"
- 私钥文件权限必须 0600 或更严（group/other 任何权限位都拒绝）
- sentinel：`__P_<sid>__> ` (PS1) + `__E_<sid>__:<exit>__` (PROMPT_COMMAND)；sid 为 8 字节十六进制随机串
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
- 不支持 `send_input` / `send_special` / `get_trace` MCP 工具（PtyConn 已实现，但 MCP handler 未注册）

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
- ANSI 过滤复用 `internal/ssh/normalize.go`
- TimeoutMs / MaxSteps / GlobalTimeoutMs 用 0 = 默认值（10000 / 50 / 60000）

**集成**：`internal/ssh/session.go` 在 SSH auth 完成、target shell 就绪后调 `loginflow.Run`，成功后再注入 RC

**验证**：
- `go test ./internal/loginflow/...` 全绿
- mock SSH server 配置一个简单 SSHServer.LoginFlow（如 `su -` 切换用户），MCP Inspector 跑通

**关键文件**：`internal/loginflow/{executor,trace}.go`、`internal/ssh/session.go`（集成）

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

**关键文件**：`internal/loginflow/executor.go`（PTY 接口）、`internal/ssh/pty.go`（OpenPtyConn/RunLoginFlow/InjectRC + pushback 切分）、`internal/mcp/tools_session.go`（setupDirect/setupPatternB 分支）

## 阶段 5：sftp + 其余工具 ⏳

**目标**：补齐 `upload` / `download` / `send_input` / `send_special` / `get_trace`，完成 v1 全部 MCP 工具。

**TDD 测试用例**：

`internal/ssh/sftp_test.go`
- sftp 通道在 login 时同步建立（5s 超时）
- sftp 不可用时 `stat()` 返回 `sftp_available=false`
- sftp 不可用时 `upload` / `download` 报错 "sftp not available for this session"
- `upload` 正常路径：本地文件 → 远端，返回字节数
- `upload` 超时：返回已传输字节，timed_out=true
- `download` 同上

`internal/ssh/session_input_test.go`
- `send_input(sid, text)` 在 running 状态写入 PTY
- `send_input` 在 idle 状态 → 报错 "session idle"
- `send_special("ctrl-c")` 等控制字符正确编码（\x03 / \x04 / \x1a / \t / \x1b）
- `send_special` 在 idle 状态 → 报错 "session idle"
- `send_input` 内容记入当前 TraceEntry.Inputs

`internal/ssh/trace_test.go`
- `get_trace(sid, last_n)` 返回最近 N 轮
- `get_trace(sid)` 返回全部
- `trunc_output` 截断参数生效（默认 200，0 不截断）
- `close_session` 后 trace 保留 10 分钟自动清理（用 fake clock）

**MCP handler**：`internal/mcp/tools_file.go`（upload/download）、`internal/mcp/tools_session.go` 扩展（send_input/send_special/get_trace）

**验证**：
- `go test ./...` 全绿
- MCP Inspector 端到端走完所有工具

**关键文件**：`internal/ssh/{sftp,session,trace}.go`、`internal/mcp/{tools_file,tools_session}.go`

## 跨阶段事项

- **代理（Proxy）支持**：阶段 2 拨号时一并实现 SOCKS5（`golang.org/x/net/proxy`）+ HTTP CONNECT（自写约 50 行）。不单独成阶段。✅ 已完成
- **MCP server 启动参数**：`SSHMNG_HOME` 环境变量、`--config <path>` 命令行参数（用 `flag` 库），阶段 1 实现。✅ 已完成
- **错误处理统一**：SSH auth 失败仅 error 字符串；LoginFlow 失败 error + login_trace；命令失败按需 get_trace。三类失败的 handler 包装在 `internal/mcp/server.go`。✅ 阶段 1-2 已实现 SSH auth / 命令失败两类
- **并发安全**：session map 与 config 各自持锁；同一 session 的 `run_in_session` 靠 state=running 串行化。所有 MCP handler 必须并发安全。✅ 已完成
- **日志处理**：✅ 已完成（MCP notifications + stderr 兜底；详见设计文档 §3.8）

## 待讨论

### 日志处理 ✅ 已决定（2026-07-20）

**决定**：D. MCP notifications + stderr 兜底。

- **操作日志**（session 创建/关闭、idle timeout 触发、TOFU host key 新增/变更、login 失败、命令超时等）走 `slog` + `mcp.NewLoggingHandler`，通过 `notifications/message` 推到 client
- **stderr** 只留 bootstrap 错误（MCP 还没起来时，如配置加载失败）和 fatal panic
- **stdout** 严禁写日志（JSON-RPC 专用）
- **日志文件** v1 不做——client 已捕获 MCP 日志，`--mcp-debug` 可事后诊断；rotation/权限管理是额外复杂度

**实现要点**：
- `cmd/sshmng/main.go`：bootstrap 阶段用 `slog.NewTextHandler(os.Stderr, nil)` 作为默认 logger
- `internal/mcp/server.go`：`Service` 持有 base logger；每个 handler 从 `req.Session` 取 `*ServerSession`，用 `mcp.NewLoggingHandler(ss, opts)` 创建 session-scoped logger 推送操作日志
- client 端可通过 `logging/setLevel` 控制 level；默认 info
- rate limit：用 `LoggingHandlerOptions.MinInterval`（如 100ms）避免高频日志淹没 client

**状态**：✅ 已实施（commit 待提交）。在阶段 3 开始前完成跨阶段日志框架铺设。

**实施细节**：
- `cmd/sshmng/main.go`：bootstrap 阶段用 `slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})` 作为 baseLogger
- `internal/mcp/server.go`：`Service` 持有 `baseLogger *slog.Logger`；新增 `sessionLogger(req, sid)` helper 用 `mcp.NewLoggingHandler(req.Session, &LoggingHandlerOptions{LoggerName:"sshmng", MinInterval:100ms})` 创建 session-scoped logger，req/Session 为 nil 时退回 baseLogger
- `internal/ssh/session.go`：`Session` 持有 `logger *slog.Logger`；`NewSession`/`newSessionWithConn` 新增 logger 参数（nil 退化为 discard）；idle timer 触发时通过 `s.logger.Info("idle timeout fired, closing session", ...)` 记录
- `internal/mcp/tools_session.go`：Login 成功/失败、RunInSession 超时、CloseSession 关闭均通过 sessionLogger 记录；日志内容不含 password / private_key / passphrase

## 端到端验证

每阶段交付后用 MCP Inspector 验证：

```bash
npx @modelcontextprotocol/inspector go run ./cmd/sshmng
```

阶段 5 完成后做完整端到端：
1. 用 mock SSH server（带堡垒机菜单）做集成测试
2. 用真实堡垒机（如有测试环境）做验收测试
3. 用 Claude Code `--mcp-debug` 验证 Agent 集成路径
