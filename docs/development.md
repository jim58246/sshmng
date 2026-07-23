# 架构与开发

本文档面向贡献者：包结构、关键设计、测试与开发流程。用户文档见 [README](../README.md)；配置字段见 [configuration.md](configuration.md)；Agent 集成见 [agents.md](agents.md)。

## 架构

```
sshmng/
├── cmd/sshmng/         # 入口（子命令分发：mcp/install/doctor/help）
├── internal/
│   ├── cli/            # CLI 子命令处理（dispatch + install + doctor + Agent 注入器 + 文件脚手架）
│   ├── config/         # 数据模型 + 加载/保存/校验/CRUD（Jumphost/Proxy/SSHServer）
│   ├── loginflow/      # 决策树执行器（纯逻辑，send+expect+ANSI 过滤）
│   ├── ssh/            # SSH 连接层 + session 管理 + sftp + trace
│   │   ├── conn/           # dialer + auth + TOFU + 代理（SOCKS5/HTTP CONNECT）+ known_hosts
│   │   ├── pty/            # PTY 分配 + sentinel 注入 + 命令边界识别 + sftp 通道
│   │   └── session/        # session 状态机（idle/running/closed）+ idle timeout
│   └── mcp/            # MCP server + 工具 handler
│       ├── server.go           # 注册工具
│       ├── tools_config.go     # list_* / get_* / update_*
│       ├── tools_session.go    # login / run_in_session / close_session / stat / get_trace
│       └── tools_file.go       # upload / download / upload_dir / download_dir
└── docs/              # 用户文档 + 设计文档 + 实施计划
```

### 关键设计

- **stdio 单进程**：一个 Agent 拉起一个 sshmng 子进程，进程内 `map[sid]*Session`，跨 Agent 不共享
- **PTY 统一模式**：所有连接（含直连）走 PTY，target shell 就绪后一次性注入 RC（TERM/PS1 等），命令边界靠 sentinel 识别。bash/zsh 走 PS1-only token 化 sentinel（每次 Run 生成唯一 `<token>` 直接嵌入 PS1，`$(echo _$?)__<sid>_<token>__]# ` 在 prompt 展开时捕获 exit code），命令输出无法预知 token，从根本上杜绝命令/结果错配；dash/ash 不 token 化（固定 `__P_<sid>__> `，无 exit code）
- **三类失败分类**：SSH auth 失败仅 error 字符串；LoginFlow 失败 error + login_trace；命令失败按需 get_trace
- **并发安全**：session map 与 config 各自持锁；同一 session 的 `run_in_session` 靠 `state=running` 串行化

### 子命令分发

`cmd/sshmng/main.go` 是薄入口，调 `cli.Dispatch(ctx, args, out)`。Dispatch 根据 `os.Args[1]` 路由：

- `sshmng mcp` — 启动 MCP server（stdio），Agent 配置应使用此子命令
- `sshmng install` — 首次安装向导（创建 `~/.sshmng/`、注入 Agent 配置、跑 doctor）
- `sshmng doctor` — 验证配置正确性（三态退出码 0/1/2）
- `sshmng help` / `-h` / `--help` / 无参数 — 打印帮助

### Agent 注入器

`internal/cli/agent_*.go` 实现 `AgentInjector` 接口，每个 Agent 一个注入器：

- `ClaudeCodeInjector` — `~/.claude.json`，JSON，`mcpServers` 顶层 key
- `HermesInjector` — `~/.hermes/config.yaml`（Unix）/ `%LOCALAPPDATA%\hermes\config.yaml`（Windows），YAML，`mcp_servers` 顶层 key
- `OpenCodeInjector` — `~/.config/opencode/opencode.json`，JSON，`mcp` 顶层 key，`command` 是数组，env 字段叫 `environment`

每个注入器实现 `Detect()` / `Inject(path, MCPEntry)` / `Verify(path, MCPEntry)`。`sshmng install` 和 `sshmng doctor` 复用这些注入器。

## 测试与开发

```bash
# 跑全部测试（含 race detector）
go test -race ./...

# 跑单个包
go test -race ./internal/ssh/...

# 看 trace 输出
go test -race -v -run TestGetTrace ./internal/ssh/
```

测试覆盖：
- `internal/config`：CRUD + 校验 + 引用完整性
- `internal/loginflow`：决策树执行器（纯逻辑，fake PTY）
- `internal/ssh/conn`：dialer（mock SSH server）/ known_hosts（TOFU）
- `internal/ssh/pty`：sentinel 解析 / sftp（InMemHandler）
- `internal/ssh/session`：状态机 + idle timeout
- `internal/mcp`：每个 handler 的错误路径 + 端到端集成（fake SSH server + fake sftp subsystem）
- `internal/cli`：子命令分发、Agent 注入器（3 个 Agent 各 stale-args/stale-env 测试）、install/doctor 端到端、文件脚手架、backup restore

TDD：每阶段先写测试再写实现，race detector 全程开启。

### CLI 端到端测试

```bash
# 构建 + 跑 install + doctor
go build -o /tmp/sshmng ./cmd/sshmng
/tmp/sshmng install --yes --agents none
/tmp/sshmng doctor
```

`cmd/sshmng/e2e_test.go` 包含 binary 级集成测试，需要先 `go build -o /tmp/sshmng ./cmd/sshmng`，测试会 spawn 子进程通过 JSON-RPC 验证 MCP 协议。
