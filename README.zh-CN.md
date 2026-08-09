[English](./README.md) | [简体中文](./README.zh-CN.md)

# sshmng

sshmng 是一个**统一的 SSH 管理器**，覆盖**所有连接形态**——直连、`ssh -J` 透明转发、交互式堡垒机、传输代理——全在一个**零依赖**二进制里，支持**自动升级**。它既能作为 **MCP server 给 AI Agent**（Claude Code / Hermes / 等）调用，也有 `sshmng ssh` CLI 给人直接用，两者**共享同一份配置**。出错时 Agent 读失败 trace、改配置、重试——**闭环自愈**，**无需人工介入**。

## 特性

- **真的能搞定交互式堡垒机**：大多数 SSH 工具遇到菜单式堡垒机就束手无策。sshmng 的 `LoginFlow` 决策树（send + expect，glob / `re:` 正则双模）自动驱动菜单登录 target——菜单文案变了，失败 trace 回传给 Agent，让它改 pattern 后重试
- **配置自愈闭环**：Agent 据 `error` / `login_trace` 诊断失败后调 `update_*` 修配置再重试 `login`，无需人工介入即可闭环
- **一键安装向导**：`sshmng install` 创建配置目录 + 模板，自动检测已装的 AI Agent（Claude Code / Hermes / OpenCode）并把 sshmng 注入到它们的配置里（带时间戳备份）；`sshmng doctor` 验证一切就绪
- **一份配置，两种用法**：MCP server 给 AI Agent（Claude Code / Hermes / OpenCode / Claude Desktop / Cursor）调用，`sshmng ssh` CLI 给人直接登。同一份 `config.json`，同样的直连 / Pattern A（`ssh -J`）/ Pattern B（堡垒机）模式——配一次，两边都能用
- **显式会话管理**：`login` → `run_in_session` → `close_session` 三件套，连续多命令共享 cwd / env / 后台任务，不像一次性 `ssh host cmd` 那样每次都重新来
- **sftp 文件传输**：`upload` / `download` 单文件走独立 sftp 通道，与 PTY 命令通道分离，不可用时优雅降级；`upload_dir` / `download_dir` 递归传输目录树，并发（默认 4），冲突策略 overwrite / skip / rename；`relay_transfer` 经 sshmng 中转流式传输文件到一个或多个 session（不落盘、1:N 扇出、源只读一次）
- **命令诊断**：`run_in_session` 超时自动 Ctrl-C + drain，返回 `timed_out` / `ctrl_c_sent`；`get_trace` 取回命令历史（含 raw_output、ctrl_c_sent）供事后排查
- **TOFU host key**：首次连接记录公钥到 `known_hosts`，变更拒绝（"host key changed, possible MITM"）
- **配置 CRUD**：`list_*` / `get_*` / `update_*` 三类工具管理 SSHServer / Jumphost / Proxy，RFC 7396 JSON Merge Patch 语义

## 安装与构建

sshmng 是单二进制工具，无运行时依赖。任选一种方式获取：

```bash
# 方式 0：一键安装脚本 —— 下载 release、放到 PATH
#   macOS / Linux：
curl -fsSL https://raw.githubusercontent.com/jim58246/sshmng/main/install.sh | bash
#   Windows（PowerShell）：
irm https://raw.githubusercontent.com/jim58246/sshmng/main/install.ps1 | iex

# 方式一：下载 release 二进制（推荐，无需 Go 环境）
#   从 https://github.com/jim58246/sshmng/releases 下载对应 OS/Arch 的二进制
chmod +x sshmng

# 方式二：go install（需要 Go 1.25+）
go install github.com/jim58246/sshmng/cmd/sshmng@latest

# 方式三：克隆后本地编译
git clone https://github.com/jim58246/sshmng.git
cd sshmng && go build -o sshmng ./cmd/sshmng
```

**或者让 AI Agent 帮你装**：把 [`docs/zh-CN/agent-install-prompt.md`](docs/zh-CN/agent-install-prompt.md) 里的 prompt 复制粘贴给 Claude Code / Cursor / Hermes / OpenCode——Agent 会自动下载二进制、放到 `PATH`、跑 `sshmng install`。

**macOS**：浏览器下载的二进制会带 Gatekeeper 隔离属性——首次运行前执行 `xattr -d com.apple.quarantine sshmng`。`go install` / `go build` 不需要此操作（本地编译）。自动更新的二进制也不需要（详见 [docs/zh-CN/auto-update.md](docs/zh-CN/auto-update.md)）。

拿到二进制后执行 `sshmng install` 即可创建 `~/.sshmng/` 配置目录并注入到已安装的 AI Agent（Claude Code / Hermes / OpenCode 等），详见 [快速上手](#快速上手)。

**推荐**：跑 `install` 之前，先把二进制放到 `PATH` 下的稳定位置（例如 `mv sshmng /usr/local/bin/`，或用 `go install` 时直接放在 `~/go/bin/`）。`sshmng install` 会把二进制的绝对路径写进 Agent 配置，`sshmng doctor` 也会校验路径与当前可执行文件一致——一开始就放好，避免之后移动二进制还得重跑 install。

### 从源码构建

```bash
# 普通构建（version.Version 为 "dev"，自更新会被禁用）
go build -o sshmng ./cmd/sshmng

# 带 ldflags 注入版本信息（自更新需要真实版本号）
go build -ldflags="-X github.com/jim58246/sshmng/internal/version.Version=v1.2.3" -o sshmng ./cmd/sshmng
```

不注入 ldflags 时，`version.Version` 默认为 `"dev"`，此时 `sshmng update` 与 `mcp` 启动时的自动更新 goroutine 都会被跳过。

运行：

```bash
./sshmng                                  # Print help
./sshmng mcp                              # Start MCP server (what Agent configs use)
./sshmng install                          # First-time setup wizard
./sshmng doctor                           # Verify setup
./sshmng version                          # Print version / commit / date
./sshmng version --check                  # Check latest version against source
./sshmng update                           # Self-update to latest release
./sshmng mcp --config /path/to/config.json  # MCP server with custom config
SSHMNG_HOME=/custom/dir ./sshmng mcp         # MCP server with custom home
./sshmng server list [keywords...]        # List SSH servers (AND match on name/addr/tags)
./sshmng server get <name>                # Show SSH server details (full auth)
./sshmng jumphost list|get ...            # Same for jumphosts
./sshmng proxy list|get ...               # Same for proxies
./sshmng ssh <name> [command]             # Interactive SSH login; with command, non-interactive
./sshmng file upload <name> <local> <remote>   # File transfer via sftp (also: download, upload-dir, download-dir, relay)
./sshmng file relay <src-name> <src-path> <dst-path> --to <dst1,dst2>  # 1:N fanout to multiple servers
```

## 快速上手

```bash
# 1. 构建
go build -o sshmng ./cmd/sshmng

# 2. 首次安装（创建 ~/.sshmng/ + 注入到已安装的 AI Agent）
./sshmng install

# 3. 验证配置
./sshmng doctor

# 4. 重启你的 Agent，让它调用 sshmng：
#    "list_ssh_servers"          → 应返回空数组
#    "add an SSH server named prod-web-01 at 10.0.0.1:22 with password ..."
#    "login to prod-web-01 and run df -h"
```

非交互场景：

```bash
./sshmng install --yes --agents claude-code,hermes
```

手动配置 fallback 与各 Agent 详细集成步骤见 [docs/agents.md](docs/zh-CN/agents.md)。

## MCP 工具一览

共 19 个工具：

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
| 文件传输 | `upload_dir(sid, src, dst, conflict?, concurrency?, timeout_ms?)` | 本地目录树 → 远端，递归 sftp，并发默认 4，冲突策略 overwrite/skip/rename |
| 文件传输 | `download_dir(sid, src, dst, conflict?, concurrency?, timeout_ms?)` | 远端目录树 → 本地，递归 sftp，并发默认 4，冲突策略 overwrite/skip/rename |
| 文件传输 | `relay_transfer(src_sid, src_path, dst_sids[], dst_path, timeout_ms?)` | 经 sshmng 中转流式传输远端文件到一个或多个 session（不落盘、1:N 扇出、源只读一次）；需源与所有目标 sftp 可用；部分失败返回 ok:false（看 ok 字段而非 IsError） |

> 不提供 `send_input` / `send_special`：MCP 客户端串行化工具调用，`run_in_session` 执行中调不到这两个工具；命令结束（正常退出或超时 Ctrl-C 后）session 已回 idle 或 closed，再调也报错。交互式命令（sudo/read/cat>file）靠 `run_in_session` 自身超时 + `get_trace` 看 raw_output 诊断，不靠 send_input 喂入。

## 安全注意事项

- **明文存储**：v1 阶段 password / passphrase 明文存在 `config.json`，文档明确警告；若不可接受，自行用 `age` / `gpg` 加密整个 `config.json`，使用前解密
- **TOFU host key**：默认开启，首次连接记录公钥到 `~/.sshmng/known_hosts`，变更拒绝（"host key changed, possible MITM"）。可通过 per-entity `host_key_verify: false` 关闭校验（完全跳过 known_hosts 读写，丢 MITM 防护，仅受信内网堡垒机等场景使用）；删除已记录的某条 key 仍需手动编辑 `~/.sshmng/known_hosts`，无工具支持
- **Trace 含敏感数据**：`Send`（LoginFlow 阶段）、`Output`（PTY 原始流）都可能含密码；trace 仅存内存，`close_session` 后保留 10 分钟自动清理，不落盘
- **stdout 严禁写日志**：JSON-RPC 专用；操作日志走 `config.log_path` 指定的轮转文件（10MB / 5 份，0600 权限），未配置则不打日志；bootstrap 错误走 stderr
- **认证范围（v1）**：仅支持 Password + PrivateKey；不支持 keyboard-interactive / SSH agent / SSH certificate / 2FA（若环境强制要求，需 v2 扩展或在 LoginFlow 中硬编码交互）

## 自动更新

sshmng 在 `mcp` 启动时后台 goroutine 静默检查更新（仅写 `log_path` 日志，不输出 stdout）。关闭：`{"auto_update_enabled": false}`。手动更新：`sshmng update`。撞 GitHub 限流了？用浏览器下载资产后执行 `sshmng update --file <path>`（绕过 API 配额；接受 `.tar.gz`、`.tar` 或解压后的目录）。版本对比：`sshmng version --check`。自定义源：设置 `update_url`（自建源布局、macOS 注意、`--file` 模式、发布流程见 [docs/zh-CN/auto-update.md](docs/zh-CN/auto-update.md)）。

## 测试与开发

```bash
# 跑全部测试（含 race detector）
go test -race ./...
```

测试覆盖与开发细节见 [docs/development.md](docs/development.md)。

## 文档

- [配置参考](docs/zh-CN/configuration.md) — 完整 config.json 字段参考、Pattern A/B 形态约束、示例
- [Agent 集成指南](docs/zh-CN/agents.md) — Claude Code / Hermes Agent / OpenCode / Claude Desktop 详细配置、MCP Inspector 调试、首次配置流程、典型调用流程
- [Agent 安装 prompt](docs/zh-CN/agent-install-prompt.md) — 复制粘贴给 AI Agent，让它端到端帮你装 sshmng
- [自动更新](docs/zh-CN/auto-update.md) — 自建 HTTP 源布局、macOS 注意、发布流程
- [架构与开发](docs/development.md) — 包结构、关键设计、子命令分发、测试覆盖
- [设计文档](docs/ssh-session-manager-design.md) — 完整设计规范（PTY sentinel、LoginFlow、session 状态机等）
- [实施计划](docs/implementation-plan.md) — v1 实施进度

## 状态

v1 阶段：客户端独立运行，stdio 单进程，配置本地存储。设计文档见 [`docs/ssh-session-manager-design.md`](docs/ssh-session-manager-design.md)。

## 贡献

欢迎开 [issue](https://github.com/jim58246/sshmng/issues) 反馈 bug 和 feature request。

## License

[MIT](LICENSE) — Copyright (c) 2026 jim58246
