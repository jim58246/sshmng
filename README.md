# sshmng

SSH 会话管理工具，以 MCP (Model Context Protocol) server 形式对外提供服务。让 AI Agent（Claude Desktop / Claude Code / Cursor 等）通过统一的工具接口管理 SSH 连接、跑命令、传文件，并支持交互式堡垒机与 LoginFlow 决策树。

> v1 阶段：客户端独立运行，stdio 单进程。配置文件本地存储。设计文档见 [`docs/ssh-session-manager-design.md`](docs/ssh-session-manager-design.md)，实施进度见 [`docs/implementation-plan.md`](docs/implementation-plan.md)。

## 特性

- **配置 CRUD**：`list_*` / `get_*` / `update_*` 三类工具管理 SSHServer / Jumphost / Proxy，RFC 7396 JSON Merge Patch 语义
- **显式会话管理**：`login` → `run_in_session` → `close_session` 三件套，连续多命令共享 cwd/env
- **交互式堡垒机（Pattern B）**：`Jumphost.SSHJ=false` + `LoginFlow` 决策树，自动导航菜单登录 target
- **LoginFlow 决策树**：send + expect 树状结构，glob / `re:` 正则双模；失败返回 trace 供 Agent 诊断 + 修复配置 + 重试
- **TOFU host key**：首次连接记录公钥到 `known_hosts`，变更拒绝（"host key changed, possible MITM"）
- **sftp 文件传输**：`upload` / `download` 走独立 sftp 通道，与 PTY 命令通道分离；不可用时优雅降级
- **命令诊断**：`send_input` / `send_special` 应对交互式命令；`get_trace` 取回命令历史（含 send_input 记录、原始输出）
- **配置自愈**：Agent 据 `error` / `login_trace` 诊断失败后可调 `update_*` 修配置再重试 `login`
- **安全收敛**：`config.json` / `known_hosts` / 私钥文件强制 0600 权限；stdout 严禁写日志（JSON-RPC 专用），操作日志走 MCP notifications/message

## 架构

```
sshmng/
├── cmd/sshmng/         # MCP server 入口（stdio 模式）
├── internal/
│   ├── config/         # 数据模型 + 加载/保存/校验/CRUD（Jumphost/Proxy/SSHServer）
│   ├── loginflow/      # 决策树执行器（纯逻辑，send+expect+ANSI 过滤）
│   ├── ssh/            # SSH 连接层 + session 管理 + sftp + trace
│   │   ├── dialer.go       # 拨号 + auth + TOFU + 代理（SOCKS5/HTTP CONNECT）
│   │   ├── pty.go          # PTY 分配 + sentinel 注入 + 命令边界识别
│   │   ├── session.go      # session 状态机（idle/running/closed）
│   │   ├── sftp.go         # sftp 通道建立 + Upload/Download
│   │   ├── trace.go        # CommandTrace 存储 + 10min graveyard TTL
│   │   ├── sentinel.go     # PS1 / exit sentinel 解析
│   │   └── shell_detect.go # shell 类型探测 + RC 注入脚本
│   └── mcp/            # MCP server + 工具 handler
│       ├── server.go           # 注册 16 个工具
│       ├── tools_config.go     # list_* / get_* / update_*
│       ├── tools_session.go    # login / run_in_session / close_session / stat / send_input / send_special / get_trace
│       └── tools_file.go       # upload / download
└── docs/              # 设计文档 + 实施计划
```

**关键设计**：
- **stdio 单进程**：一个 Agent 拉起一个 sshmng 子进程，进程内 `map[sid]*Session`，跨 Agent 不共享
- **PTY 统一模式**：所有连接（含直连）走 PTY，target shell 就绪后一次性注入 RC（TERM/PS1/PROMPT_COMMAND 等），命令边界靠 sentinel `__P_<sid>__> ` + `__E_<sid>__:<exit>__`
- **三类失败分类**：SSH auth 失败仅 error 字符串；LoginFlow 失败 error + login_trace；命令失败按需 get_trace
- **并发安全**：session map 与 config 各自持锁；同一 session 的 `run_in_session` 靠 `state=running` 串行化

## 安装与构建

要求 Go 1.25+。

```bash
# 编译
go build -o sshmng ./cmd/sshmng

# 或直接 install
go install ./cmd/sshmng
```

运行：

```bash
./sshmng                          # 用默认配置路径
./sshmng --config /path/to/config.json
SSHMNG_HOME=/custom/dir ./sshmng  # 自定义配置目录
```

## 配置

**路径解析顺序**：
1. `--config <path>` 命令行参数
2. `$SSHMNG_HOME/config.json`
3. `$HOME/.sshmng/config.json`

**文件权限**：必须 0600，过宽会被拒绝加载；首次创建时立即 chmod 0600。

**示例（Pattern B 交互式堡垒机）**：

```json
{
  "version": "1",
  "idle_timeout_s": 300,
  "jumphosts": [
    {
      "name": "华东/jumphost-prod",
      "addr": "10.0.0.254:22",
      "user": "ops",
      "auth": {"password": "ops-password"},
      "ssh_j": false,
      "login_flow": {
        "wait_menu": {
          "name": "wait_menu",
          "expects": [{"pattern": "Your choice:", "next": "success"}]
        }
      },
      "login_entry": "wait_menu",
      "tags": ["生产", "华东"]
    }
  ],
  "proxies": [
    {
      "name": "corp-socks5",
      "type": "SOCKS5",
      "addr": "socks.corp:1080",
      "tags": ["生产"]
    }
  ],
  "servers": [
    {
      "name": "华东/order/order-01",
      "addr": "10.0.0.1:22",
      "user": "deploy",
      "auth": null,
      "via": "华东/jumphost-prod",
      "proxy": "corp-socks5",
      "login_flow": {
        "select_target": {
          "name": "select_target",
          "send": "1\n",
          "expects": [{"pattern": "Password:", "next": "input_pass"}]
        },
        "input_pass": {
          "name": "input_pass",
          "send": "deploy-password\n",
          "expects": [{"pattern": "$ ", "next": "success"}]
        }
      },
      "login_entry": "select_target",
      "tags": ["生产", "v2.3", "主备"]
    }
  ]
}
```

**两种 jumphost 形态**：
- `ssh_j=true`：透明转发（`ssh -J` 语义），LoginFlow 必须为空 —— v1.x 实现
- `ssh_j=false`：交互式堡垒机，Jumphost.LoginFlow 准备到主菜单就绪，SSHServer.LoginFlow 接管登录 target

**直连 server**：`via` 留空，`auth` 必填（Password 或 PrivateKey + 可选 Passphrase）。可选配置 `SSHServer.LoginFlow` 承担 target 认证后交互（如 `su -`、角色切换、PAM session）。

**字段约定**：
- `LoginAction.Send` 是直接字符串，**不支持变量引用**——凭据直接写在 Send 中
- `LoginAction.Expects` 至少一条 pattern；`pattern` 无前缀 = glob，`re:` 前缀 = 正则
- `Expect.Next == "success"` 表示登录成功（`"success"` 是保留字符串，不能作为 LoginFlow 的 key）
- `PrivateKey` 是文件完整路径，启动时校验权限 0600 或更严
- 同时配置 PrivateKey + Password 时仅尝试 PrivateKey，失败不回退

## MCP 工具一览

共 16 个工具：

| 类别 | 工具 | 说明 |
|------|------|------|
| 配置查询 | `list_ssh_servers` / `list_jumphosts` / `list_proxies` | 按 query 子串匹配 name/addr/tags（脱敏 auth） |
| 配置查询 | `get_ssh_server` / `get_jumphost` / `get_proxy` | 按 name 取单条（完整 auth） |
| 配置更新 | `update_ssh_server` / `update_jumphost` / `update_proxy` | RFC 7396 JSON Merge Patch；null 删除，object 合并/创建 |
| 会话管理 | `login(name)` → `{sid, sftp_available}` | 拨号 + LoginFlow + RC 注入 + sftp 通道建立 |
| 会话管理 | `run_in_session(sid, cmd, timeout_ms?, max_output_bytes?)` | 跑命令，返回 output/exit_code/timed_out/truncated/total_bytes |
| 会话管理 | `close_session(sid)` | 强制关闭，trace 保留 10 分钟 |
| 会话管理 | `stat()` | 列出所有活跃 session 摘要（含 sftp_available） |
| 交互控制 | `send_input(sid, text)` | running 状态下向 PTY 写文本（如回答密码 prompt） |
| 交互控制 | `send_special(sid, key)` | running 状态下发 ctrl-c / ctrl-d / ctrl-z / tab / esc |
| 诊断 | `get_trace(sid, last_n?, trunc_output?)` | 取命令历史（含 send_input 记录、原始输出） |
| 文件传输 | `upload(sid, src, dst, timeout_ms?)` | 本地 → 远端，走 sftp |
| 文件传输 | `download(sid, src, dst, timeout_ms?)` | 远端 → 本地，走 sftp |

## 集成指南

sshmng 是标准 stdio MCP server，任何支持 MCP 的客户端都能接入。

### Claude Desktop (macOS)

编辑 `~/Library/Application Support/Claude/claude_desktop_config.json`：

```json
{
  "mcpServers": {
    "sshmng": {
      "command": "/Users/<you>/go/bin/sshmng",
      "env": {
        "SSHMNG_HOME": "/Users/<you>/.sshmng"
      }
    }
  }
}
```

重启 Claude Desktop 后，工具面板会出现 `login` / `run_in_session` 等工具。

### Claude Code

在项目根目录或 `~/.claude.json` 中配置：

```json
{
  "mcpServers": {
    "sshmng": {
      "command": "sshmng",
      "args": [],
      "env": {
        "SSHMNG_HOME": "/Users/<you>/.sshmng"
      }
    }
  }
}
```

或用 CLI 注册：

```bash
claude mcp add sshmng sshmng --env SSHMNG_HOME=/Users/<you>/.sshmng
```

启动 `claude` 后用 `/mcp` 查看 sshmng 是否已连接、工具是否加载。

### MCP Inspector（调试用）

```bash
npx @modelcontextprotocol/inspector go run ./cmd/sshmng
```

Inspector 提供 GUI 直接调用工具、查看请求/响应、追踪 notifications/message 日志。首次集成或排查 LoginFlow 时强烈建议先用 Inspector 验证一遍。

### 首次配置流程

1. **准备配置目录**：
   ```bash
   mkdir -p ~/.sshmng
   chmod 700 ~/.sshmng
   ```

2. **写初始 config.json**（参考上方示例，或留空 `{"version":"1","servers":[]}` 由 Agent 通过 `update_*` 工具逐步填充）：
   ```bash
   echo '{"version":"1","idle_timeout_s":300,"jumphosts":[],"proxies":[],"servers":[]}' > ~/.sshmng/config.json
   chmod 600 ~/.sshmng/config.json
   ```

3. **私钥文件**（如果用 PrivateKey 认证）：放到任意路径，权限必须 0600：
   ```bash
   chmod 600 ~/.ssh/id_ed25519
   ```

4. **启动 Agent 测试**：让 Agent 调一次 `list_ssh_servers`，应返回空数组；再调 `update_ssh_server` 添加第一个目标。

### 典型 Agent 调用流程

```
1. Agent 收到"看一下 prod-web-01 的磁盘占用"
2. list_ssh_servers(query="prod-web-01") → 1 个候选，直接用 name
3. login(name="prod-web-01") → {sid: "abc123", sftp_available: true}
4. run_in_session(sid="abc123", cmd="df -h") → output 含磁盘信息
5. close_session(sid="abc123")
```

**带 LoginFlow 诊断的失败循环**：

```
1. login(name="bastion-01") → IsError=true, login_trace=[{send,expect,output}, ...]
2. Agent 分析 trace：第二条 expect 未命中，output 显示菜单文案改了
3. update_ssh_server(name="bastion-01", patch={login_flow:{...}}) 修正 pattern
4. login(name="bastion-01") → 成功
```

## 安全注意事项

- **明文存储**：v1 阶段 password / passphrase 明文存在 `config.json`，文档明确警告；若不可接受，自行用 `age` / `gpg` 加密整个 `config.json`，使用前解密
- **TOFU host key**：首次连接记录公钥；变更时拒绝并报 "host key changed, possible MITM"。重置需手动编辑 `~/.sshmng/known_hosts`，**Agent 无法通过工具重置**（安全决策必须人工确认）
- **Trace 含敏感数据**：`Send`（LoginFlow 阶段）、`Inputs`（send_input）、`Output`（PTY 原始流）都可能含密码；trace 仅存内存，`close_session` 后保留 10 分钟自动清理，不落盘
- **stdout 严禁写日志**：JSON-RPC 专用；操作日志通过 MCP `notifications/message` 推到 client，bootstrap 错误走 stderr
- **认证范围（v1）**：仅支持 Password + PrivateKey；不支持 keyboard-interactive / SSH agent / SSH certificate / 2FA（若环境强制要求，需 v2 扩展或在 LoginFlow 中硬编码交互）

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
- `internal/ssh`：dialer（mock SSH server）/ pty（sentinel 解析）/ session（状态机 + idle timeout）/ sftp（InMemHandler）/ trace（fake clock 验 TTL）
- `internal/mcp`：每个 handler 的错误路径 + 端到端集成（fake SSH server + fake sftp subsystem）

TDD：每阶段先写测试再写实现，race detector 全程开启。

## 后续迭代

- **v1.x**：Pattern A（`ssh_j=true` 透明转发，direct-tcpip 通道）
- **v2**：服务端 + 同步（gRPC over TLS、多用户认证、存储加密）；Xshell `.xsh` 导入导出；只读模式开关
- **认证扩展**：keyboard-interactive / SSH agent / SSH certificate / 2FA（若 v1 LoginFlow 硬编码方案不够用）

## License

私有项目，未发布。
