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
- **命令诊断**：`run_in_session` 超时自动 Ctrl-C + drain，返回 timed_out/ctrl_c_sent；`get_trace` 取回命令历史（含 raw_output、ctrl_c_sent）
- **配置自愈**：Agent 据 `error` / `login_trace` 诊断失败后可调 `update_*` 修配置再重试 `login`
- **安全收敛**：`config.json` / `known_hosts` / 私钥文件强制 0600 权限；stdout 严禁写日志（JSON-RPC 专用），操作日志走 `config.log_path` 指定的轮转文件（10MB / 5 份），未配置则不打日志

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
│       ├── tools_session.go    # login / run_in_session / close_session / stat / get_trace
│       └── tools_file.go       # upload / download
└── docs/              # 设计文档 + 实施计划
```

**关键设计**：
- **stdio 单进程**：一个 Agent 拉起一个 sshmng 子进程，进程内 `map[sid]*Session`，跨 Agent 不共享
- **PTY 统一模式**：所有连接（含直连）走 PTY，target shell 就绪后一次性注入 RC（TERM/PS1 等），命令边界靠 sentinel 识别。bash/zsh 走 PS1-only token 化 sentinel（每次 Run 生成唯一 `<token>` 直接嵌入 PS1，`$(echo _$?)__<sid>_<token>__]# ` 在 prompt 展开时捕获 exit code），命令输出无法预知 token，从根本上杜绝命令/结果错配；dash/ash 不 token 化（固定 `__P_<sid>__> `，无 exit code）
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
./sshmng                                  # Print help
./sshmng mcp                              # Start MCP server (what Agent configs use)
./sshmng install                          # First-time setup wizard
./sshmng doctor                           # Verify setup
./sshmng mcp --config /path/to/config.json  # MCP server with custom config
SSHMNG_HOME=/custom/dir ./sshmng mcp         # MCP server with custom home
```

## 配置

**路径解析顺序**：
1. `--config <path>` 命令行参数
2. `$SSHMNG_HOME/config.json`
3. `$HOME/.sshmng/config.json`

**文件权限**：Unix（macOS/Linux）下 config.json / 私钥文件 / known_hosts 必须 0600，过宽会被拒绝加载；首次创建时立即 chmod 0600。Windows 跳过权限检查（NTFS 用 ACL 而非 Unix rwx，`os.FileMode.Perm()` 的 group/other 位恒为 0，检查形同虚设）——需手动用 NTFS ACL 限制这些文件访问（右键→属性→安全，移除除当前用户外的所有条目）。

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
          "send": "1\r",
          "expects": [{"pattern": "Password:", "next": "input_pass"}]
        },
        "input_pass": {
          "send": "deploy-password\r",
          "expects": [{"pattern": "$ ", "next": "success"}]
        }
      },
      "login_entry": "select_target",
      "tags": ["生产", "v2.3", "主备"]
    }
  ]
}
```

**示例（Pattern A 透明转发，ssh -J 语义）**：

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
      "ssh_j": true,
      "tags": ["生产", "华东"]
    }
  ],
  "servers": [
    {
      "name": "华东/order/order-01",
      "addr": "10.0.0.1:22",
      "user": "deploy",
      "auth": {"password": "deploy-password"},
      "via": "华东/jumphost-prod",
      "tags": ["生产", "v2.3", "主备"]
    }
  ]
}
```

与 Pattern B 的差异：
- `jumphost.ssh_j=true`，`jumphost.login_flow` 必须为空
- `server.auth` 必填（用于 SSH auth 到 target，跟 Pattern B 相反）
- `server.proxy` 不支持（direct-tcpip 走 jumphost 的 SSH 通道，独立传输代理无意义）
- `server.login_flow` 可选（target 认证后交互，如 `su -` / 角色切换 / PAM）
- SFTP 可用（client 是到 target 的）

### 字段参考

#### 顶层 Config

| 字段 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `version` | string | 是 | — | 配置版本，当前固定为 `"1"` |
| `idle_timeout_s` | int | 否 | `300` | session 空闲超时（秒），超时自动 close；`0` 取默认 |
| `log_level` | string | 否 | `"info"` | 日志级别：`debug` / `info` / `warn` / `error`（支持缩写 `dbg`/`d`/`inf`/`i`/`w`/`err`/`e`，大小写不敏感）；配错 Load 报错 |
| `log_path` | string | 否 | — | 日志目录：空 = 不打日志；非空 = `<log_path>/sshmng.log`，10MB 轮转、最多 5 份（`sshmng.log` + `sshmng.1.log` ~ `sshmng.4.log`） |
| `jumphosts` | []Jumphost | 否 | `[]` | SSH 跳板列表 |
| `proxies` | []Proxy | 否 | `[]` | 传输层代理列表 |
| `servers` | []SSHServer | 否 | `[]` | 目标机列表 |

#### Proxy

传输层代理（不参与 SSH 协议，只代理 TCP 连接）。

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `name` | string | 是 | 唯一标识，被 jumphost/server 的 `proxy` 字段引用 |
| `type` | string | 是 | `"HTTP"`（HTTP CONNECT）或 `"SOCKS5"` |
| `addr` | string | 是 | `host:port` |
| `auth` | ProxyAuth | 否 | 代理自身认证；省略 = 无认证 |
| `tags` | []string | 否 | 任意标签，`list_proxies` 的 query 子串会匹配 |

ProxyAuth 结构：`{"user": "...", "password": "..."}`，两字段均可空。

#### Jumphost

SSH 跳板。`ssh_j` 字段区分两种形态，决定 LoginFlow / Auth 的必填规则。

| 字段 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `name` | string | 是 | — | 唯一标识，被 server.`via` 或 jumphost.`via` 引用 |
| `addr` | string | 是 | — | `host:port` |
| `user` | string | 是 | — | SSH 用户名 |
| `auth` | SSHAuth | 是 | — | SSH 认证信息（Password 或 PrivateKey） |
| `ssh_j` | bool | 是 | — | `true` = 透明转发（`ssh -J` 语义）；`false` = 交互式堡垒机 |
| `login_flow` | map[string]LoginAction | `ssh_j=false` 必填，`ssh_j=true` 必空 | — | 决策树 |
| `login_entry` | string | `login_flow` 非空时必填 | — | entry action 的 name |
| `max_steps` | int | 否 | `50` | LoginFlow 最大步数，防止死循环 |
| `global_timeout_ms` | int | 否 | `60000` | LoginFlow 整体超时 |
| `host_key_verify` | *bool | 否 | `true`（nil） | 是否启用 TOFU host key 校验；设 `false` 完全跳过（不读不写 known_hosts）。控制到本 jumphost 的 SSH dial |
| `via` | string | 否 | — | 多跳跳板的 jumphost name（v1 不实现多跳） |
| `proxy` | string | 否 | — | 传输代理的 name |
| `tags` | []string | 否 | — | 任意标签 |

#### SSHServer

目标机。`via` 是否指向 `ssh_j=false` 的 jumphost 决定走 Pattern A 还是 B，进而决定 `auth` / `login_flow` 必填规则。

| 字段 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `name` | string | 是 | — | 唯一标识，`login` 工具用此 name 连接 |
| `addr` | string | 是 | — | `host:port` |
| `user` | string | 是 | — | SSH 用户名 |
| `auth` | SSHAuth | Pattern A 必填，Pattern B 必空 | — | SSH 认证信息 |
| `login_flow` | map[string]LoginAction | Pattern B 必填，Pattern A 可选 | — | 决策树 |
| `login_entry` | string | `login_flow` 非空时必填 | — | entry action 的 name |
| `max_steps` | int | 否 | `50` | LoginFlow 最大步数 |
| `global_timeout_ms` | int | 否 | `60000` | LoginFlow 整体超时 |
| `host_key_verify` | *bool | 否 | `true`（nil） | 是否启用 TOFU host key 校验；设 `false` 完全跳过（不读不写 known_hosts）。仅直连和 Pattern A 生效；Pattern B（`via.ssh_j=false`）下 target 登录走 PTY 非 SSH dial，此字段不参与，只看 jumphost 的开关 |
| `via` | string | 否 | — | 经由的 jumphost name；空 = 直连 |
| `proxy` | string | 否 | — | 传输代理的 name |
| `tags` | []string | 否 | — | 任意标签，`list_ssh_servers` 的 query 子串会匹配 |

#### SSHAuth

SSH 认证信息，复用于 Jumphost 和 SSHServer。`password` 和 `private_key` 二选一；同时配置时仅尝试 `private_key`，失败不回退。

| 字段 | 类型 | 说明 |
|------|------|------|
| `password` | string | 密码认证；空 = 不使用 |
| `private_key` | string | 私钥文件完整路径（PEM 格式），启动时校验权限必须 0600 或更严 |
| `passphrase` | string | 私钥口令；空 = 私钥未加密。仅在 `private_key` 非空时有效 |

Pattern B 下 SSHServer.`auth` 必须为 `null` 或全空对象——凭据写在 `login_flow[action].send` 里。

#### LoginAction

决策树节点。一条 `send` + 多个 `expects`（按顺序尝试匹配，首个命中者生效）。

| 字段 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `send` | string | 否 | `""` | 发送字符串，支持 `\n` `\r` `\t` 转义；空 = 仅等待输出。**回车用 `\r`**（TUI 菜单 / sudo 提示等 raw mode 程序只认 `\r`），详见 [设计文档 3.7](docs/ssh-session-manager-design.md) 的"Send 字节约定" |
| `expects` | []Expect | 是（≥1） | — | 期望的输出模式列表 |
| `timeout_ms` | int | 否 | `10000` | 当前 action 的 read 超时 |

#### Expect

LoginAction 的一个分支。`pattern` 命中后跳转到 `next` 指向的 action。

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `pattern` | string | 是 | 匹配模式；无前缀 = glob（shell 风格通配），`re:` 前缀 = Go 正则 |
| `next` | string | 是 | 命中后跳转的 action name；`"success"` = 登录成功（保留字符串，不能作为 login_flow 的 key） |

### 形态与使用约束

**两种 jumphost 形态**：
- `ssh_j=true`：透明转发（`ssh -J` 语义）。客户端经 jumphost 的 direct-tcpip 通道 SSH 到 target，`SSHServer.Auth` 必填，SFTP 可用。LoginFlow 必须为空
- `ssh_j=false`：交互式堡垒机。Jumphost.LoginFlow 把 jumphost 自身驱动到主菜单就绪，SSHServer.LoginFlow 接管选 target + 输入凭据，最终落在 target shell

**直连 server**：`via` 留空，`auth` 必填（Password 或 PrivateKey + 可选 Passphrase）。可选配置 `SSHServer.LoginFlow` 承担 target 认证后交互（如 `su -`、角色切换、PAM session）。

**行为约定**：
- `LoginAction.Send` 是直接字符串，**不支持变量引用**——凭据直接写在 Send 中
- `"success"` 是保留字符串，不能作为 LoginFlow 的 key；每个 LoginFlow 必须至少有一个 Expect 的 `next` 指向 `"success"`，否则永远登录不成功
- `LoginAction.Expects` 至少一条 pattern；每条 pattern 必须非空，`next` 必须非空且指向已存在的 action 或 `"success"`
- `via` / `proxy` 是 name 字符串引用，不是嵌套对象；加载时解析为指针，引用不存在会拒绝加载
- name 在各自集合（jumphosts / proxies / servers）内必须唯一，跨集合可重名

## MCP 工具一览

共 14 个工具：

| 类别 | 工具 | 说明 |
|------|------|------|
| 配置查询 | `list_ssh_servers` / `list_jumphosts` / `list_proxies` | 按 query 多关键字 AND 匹配 name/addr/tags（空格分词、大小写不敏感、脱敏 auth） |
| 配置查询 | `get_ssh_server` / `get_jumphost` / `get_proxy` | 按 name 取单条（完整 auth） |
| 配置更新 | `update_ssh_server` / `update_jumphost` / `update_proxy` | RFC 7396 JSON Merge Patch；null 删除，object 合并/创建 |
| 会话管理 | `login(name)` → `{sid, sftp_available}` | 拨号 + LoginFlow + RC 注入 + sftp 通道建立 |
| 会话管理 | `run_in_session(sid, cmd, timeout_ms?, max_output_bytes?)` | 跑命令，返回 output/exit_code/timed_out/truncated/total_bytes |
| 会话管理 | `close_session(sid)` | 强制关闭，trace 保留 10 分钟 |
| 会话管理 | `stat()` | 列出所有活跃 session 摘要（含 sftp_available） |
| 诊断 | `get_trace(sid, last_n?, trunc_output?)` | 取命令历史（含 ctrl_c_sent、原始输出） |
| 文件传输 | `upload(sid, src, dst, timeout_ms?)` | 本地 → 远端，走 sftp |
| 文件传输 | `download(sid, src, dst, timeout_ms?)` | 远端 → 本地，走 sftp |

> 不提供 `send_input` / `send_special`：MCP 客户端串行化工具调用，`run_in_session` 执行中调不到这两个工具；命令结束（正常退出或超时 Ctrl-C 后）session 已回 idle 或 closed，再调也报错。交互式命令（sudo/read/cat>file）靠 `run_in_session` 自身超时 + `get_trace` 看 raw_output 诊断，不靠 send_input 喂入。

## 集成指南

sshmng 是标准 stdio MCP server，任何支持 MCP 的客户端都能接入。推荐用 `sshmng install` 自动注入到已安装的 Agent；下面也给出各 Agent 的手动配置（`install` 失败时的 fallback）。

所有 Agent 配置都走 `"args": ["mcp"]` 子命令语法——`sshmng` 不带子命令时只打印帮助，必须显式用 `mcp` 启动 MCP server。

### 推荐：`sshmng install`

```bash
sshmng install
```

向导会自动检测已安装的 AI Agent（Claude Code / Hermes Agent / OpenCode），让你勾选要注入哪些，然后在每个 Agent 配置里写入 sshmng entry（带时间戳备份 `.bak.<ts>`）。非交互场景：

```bash
sshmng install --yes --agents claude-code,hermes
```

`--agents` 取值：`claude-code` / `hermes` / `opencode`，逗号分隔；`none` 跳过 Agent 注入。详见 `sshmng install -h`。

### Claude Code

编辑 `~/.claude.json`：

```json
{
  "mcpServers": {
    "sshmng": {
      "command": "/Users/<you>/go/bin/sshmng",
      "args": ["mcp"],
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

注意：CLI 注册方式不会自动加 `args: ["mcp"]`（claude mcp add 把 `sshmng` 当成 server name + command），需要手动改 `~/.claude.json` 补 `"args": ["mcp"]`，或直接用 `sshmng install` 自动写入正确 entry。

启动 `claude` 后用 `/mcp` 查看 sshmng 是否已连接、工具是否加载。

### Hermes Agent

编辑 `~/.hermes/config.yaml`（Unix）或 `%LOCALAPPDATA%\hermes\config.yaml`（Windows）：

```yaml
mcp_servers:
  sshmng:
    command: /Users/<you>/go/bin/sshmng
    args:
      - mcp
    env:
      SSHMNG_HOME: /Users/<you>/.sshmng
```

或运行 `sshmng install` 选择 Hermes Agent。Hermes 的 schema 与 Claude Code 一致（`command` 字符串 / `args` 列表 / `env` map），只是顶层 key 用 `mcp_servers`（YAML）而非 `mcpServers`（JSON）。

### OpenCode

编辑 `~/.config/opencode/opencode.json`：

```json
{
  "mcp": {
    "sshmng": {
      "type": "local",
      "command": ["/Users/<you>/go/bin/sshmng", "mcp"],
      "environment": {"SSHMNG_HOME": "/Users/<you>/.sshmng"},
      "enabled": true
    }
  }
}
```

或运行 `sshmng install` 选择 OpenCode。OpenCode 的 schema 与前两者不同：
- 顶层 key 是 `mcp`（不是 `mcpServers` / `mcp_servers`）
- `command` 是数组（binary + args 合并成一个数组：`["sshmng", "mcp"]`）
- env 字段叫 `environment`（不叫 `env`）
- 额外需要 `type: "local"` 和 `enabled: true`

### Claude Desktop (macOS)

编辑 `~/Library/Application Support/Claude/claude_desktop_config.json`：

```json
{
  "mcpServers": {
    "sshmng": {
      "command": "/Users/<you>/go/bin/sshmng",
      "args": ["mcp"],
      "env": {
        "SSHMNG_HOME": "/Users/<you>/.sshmng"
      }
    }
  }
}
```

重启 Claude Desktop 后，工具面板会出现 `login` / `run_in_session` 等工具。Claude Desktop 目前不在 `sshmng install` 自动注入范围内（install 只覆盖 Claude Code / Hermes Agent / OpenCode），需手动按上面格式编辑配置文件。

### MCP Inspector（调试用）

```bash
npx @modelcontextprotocol/inspector go run ./cmd/sshmng mcp
```

Inspector 提供 GUI 直接调用工具、查看请求/响应。首次集成或排查 LoginFlow 时强烈建议先用 Inspector 验证一遍。

sshmng 不通过 MCP `notifications/message` 推日志——所有日志走 `config.log_path` 指定的文件（未配置则不打日志）。要看 DEBUG 日志，把 `config.json` 的 `log_level` 设为 `"debug"` 后重启 Inspector 即可，日志写到 `<log_path>/sshmng.log`。

#### 日志配置

```json
{
  "log_level": "debug",
  "log_path": "/Users/<you>/.sshmng"
}
```

- `log_level`：`debug` / `info` / `warn` / `error`（支持缩写 `dbg`/`d`/`inf`/`i`/`w`/`err`/`e`，大小写不敏感）；空 = 默认 `info`；配错 Load 报错
- `log_path`：日志目录；空 = 不打日志；非空 = `<log_path>/sshmng.log`，10MB 轮转、最多 5 份（`sshmng.log` + `sshmng.1.log` ~ `sshmng.4.log`，0600 权限）
- bootstrap 阶段错误（config 加载失败、known_hosts 权限错等）走 stderr，Inspector "Server" 面板可见
- DEBUG 日志会**完整记录** LoginFlow 每步 send/read/match、run_in_session 的 cmd/output、sftp upload/download、PTY stdout 片段（不截断、不打码）。**分享日志时注意脱敏**——LoginFlow 的 `send` 字段、PTY 输出都可能含密码

#### login_trace 诊断

LoginFlow 失败时，`login` 工具响应含 `login_trace` JSON 字段（每步 send / expect / output），Agent 据此修配置重试。login 成功后，`get_trace` 返回值含 `login_flow` 字段（同样的 trace 结构），用于事后排查登录过程。

### 首次配置流程

推荐用 install 向导：

```bash
sshmng install
```

向导会：

1. 创建 `~/.sshmng/`（0700）含 `config.json`（空 skeleton）和 `config.example.json`（Pattern A/B 示例）
2. 检测已安装的 AI Agent（Claude Code / Hermes Agent / OpenCode），让你勾选要注入哪些
3. 往每个选中的 Agent 配置写入 sshmng MCP entry（带时间戳备份 `.bak.<ts>`）
4. 自动跑 `sshmng doctor` 验证

非交互场景：

```bash
sshmng install --yes --agents claude-code,hermes
```

手动 fallback（`install` 失败时）：

1. 创建配置目录：
   ```bash
   mkdir -p ~/.sshmng && chmod 700 ~/.sshmng
   ```
2. 写 `~/.sshmng/config.json`（参考 `config.example.json` 模板，或用空 skeleton：`{"version":"1","idle_timeout_s":300,"jumphosts":[],"proxies":[],"servers":[]}`）：
   ```bash
   echo '{"version":"1","idle_timeout_s":300,"jumphosts":[],"proxies":[],"servers":[]}' > ~/.sshmng/config.json
   chmod 600 ~/.sshmng/config.json
   ```
3. 私钥文件（如果用 PrivateKey 认证）：放到任意路径，权限必须 0600：
   ```bash
   chmod 600 ~/.ssh/id_ed25519
   ```
4. 编辑 Agent 的配置文件（参考上方"集成指南"），sshmng 命令用 `"args": ["mcp"]`
5. 启动 Agent 测试：让 Agent 调一次 `list_ssh_servers`，应返回空数组；再调 `update_ssh_server` 添加第一个目标。

### Verifying setup

```bash
sshmng doctor
```

检查项：home 目录权限、`config.json` 可加载性、各 Agent 配置中 sshmng entry 存在且 binary path 匹配当前 sshmng 可执行文件。退出码：`0` 全通过 / `1` 至少一个 FAIL / `2` 仅 WARN（无 FAIL）。Windows 下权限检查降级为 WARN（NTFS ACL 需手动设置）。

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
- **TOFU host key**：默认开启，首次连接记录公钥到 `~/.sshmng/known_hosts`，变更拒绝（"host key changed, possible MITM"）。可通过 per-entity `host_key_verify: false` 关闭校验（完全跳过 known_hosts 读写，丢 MITM 防护，仅受信内网堡垒机等场景使用）；删除已记录的某条 key 仍需手动编辑 `~/.sshmng/known_hosts`，无工具支持
- **Trace 含敏感数据**：`Send`（LoginFlow 阶段）、`Output`（PTY 原始流）都可能含密码；trace 仅存内存，`close_session` 后保留 10 分钟自动清理，不落盘
- **stdout 严禁写日志**：JSON-RPC 专用；操作日志走 `config.log_path` 指定的轮转文件（10MB / 5 份，0600 权限），未配置则不打日志；bootstrap 错误走 stderr
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

- **v2**：服务端 + 同步（gRPC over TLS、多用户认证、存储加密）；Xshell `.xsh` 导入导出；只读模式开关
- **认证扩展**：keyboard-interactive / SSH agent / SSH certificate / 2FA（若 v1 LoginFlow 硬编码方案不够用）

## License

私有项目，未发布。
