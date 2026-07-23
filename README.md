# sshmng

SSH 会话管理工具，以 MCP (Model Context Protocol) server 形式对外提供服务。让 AI Agent（Claude Code / Claude Desktop / Hermes Agent / OpenCode / Cursor 等）通过统一的工具接口管理 SSH 连接、跑命令、传文件，并支持交互式堡垒机与 LoginFlow 决策树。

> v1 阶段：客户端独立运行，stdio 单进程。配置文件本地存储。设计文档见 [`docs/ssh-session-manager-design.md`](docs/ssh-session-manager-design.md)。

## 特性

- **配置 CRUD**：`list_*` / `get_*` / `update_*` 三类工具管理 SSHServer / Jumphost / Proxy，RFC 7396 JSON Merge Patch 语义
- **显式会话管理**：`login` → `run_in_session` → `close_session` 三件套，连续多命令共享 cwd/env
- **交互式堡垒机（Pattern B）**：`Jumphost.SSHJ=false` + `LoginFlow` 决策树，自动导航菜单登录 target
- **LoginFlow 决策树**：send + expect 树状结构，glob / `re:` 正则双模；失败返回 trace 供 Agent 诊断 + 修复配置 + 重试
- **TOFU host key**：首次连接记录公钥到 `known_hosts`，变更拒绝（"host key changed, possible MITM"）
- **sftp 文件传输**：`upload` / `download` 走独立 sftp 通道，与 PTY 命令通道分离；不可用时优雅降级
- **命令诊断**：`run_in_session` 超时自动 Ctrl-C + drain，返回 timed_out/ctrl_c_sent；`get_trace` 取回命令历史（含 raw_output、ctrl_c_sent）
- **配置自愈**：Agent 据 `error` / `login_trace` 诊断失败后可调 `update_*` 修配置再重试 `login`
- **首次上手辅助**：`sshmng install` 一键创建配置目录 + 模板 + 注入到 AI Agent；`sshmng doctor` 验证配置正确性

## 安装与构建

sshmng 是单二进制工具，无运行时依赖。任选一种方式获取：

```bash
# 方式一：下载 release 二进制（推荐，无需 Go 环境）
#   从 https://github.com/jim58246/sshmng/releases 下载对应 OS/Arch 的二进制
chmod +x sshmng

# 方式二：go install（需要 Go 1.25+）
go install github.com/jim58246/sshmng/cmd/sshmng@latest

# 方式三：克隆后本地编译
git clone https://github.com/jim58246/sshmng.git
cd sshmng && go build -o sshmng ./cmd/sshmng
```

拿到二进制后执行 `sshmng install` 即可创建 `~/.sshmng/` 配置目录并注入到已安装的 AI Agent（Claude Code / Hermes / OpenCode 等），详见 [快速上手](#快速上手)。

运行：

```bash
./sshmng                                  # Print help
./sshmng mcp                              # Start MCP server (what Agent configs use)
./sshmng install                          # First-time setup wizard
./sshmng doctor                           # Verify setup
./sshmng mcp --config /path/to/config.json  # MCP server with custom config
SSHMNG_HOME=/custom/dir ./sshmng mcp         # MCP server with custom home
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

手动配置 fallback 与各 Agent 详细集成步骤见 [docs/agents.md](docs/agents.md)。

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
```

测试覆盖与开发细节见 [docs/development.md](docs/development.md)。

## 文档

- [配置参考](docs/configuration.md) — 完整 config.json 字段参考、Pattern A/B 形态约束、示例
- [Agent 集成指南](docs/agents.md) — Claude Code / Hermes Agent / OpenCode / Claude Desktop 详细配置、MCP Inspector 调试、首次配置流程、典型调用流程
- [架构与开发](docs/development.md) — 包结构、关键设计、子命令分发、测试覆盖
- [设计文档](docs/ssh-session-manager-design.md) — 完整设计规范（PTY sentinel、LoginFlow、session 状态机等）
- [实施计划](docs/implementation-plan.md) — v1 实施进度

## 贡献

欢迎开 [issue](https://github.com/jim58246/sshmng/issues) 反馈 bug 和 feature request。暂不接受 PR。

## License

[MIT](LICENSE) — Copyright (c) 2026 jim58246
