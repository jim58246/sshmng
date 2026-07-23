# Quickstart：CLI 重构与首次上手辅助

**Date:** 2026-07-23
**Status:** Approved, ready for implementation plan
**Scope:** `cmd/sshmng`（入口重构）, `internal/cli`（新包）, `internal/config`（复用）, `internal/mcp`（不动）

## 背景

sshmng 当前是纯 stdio MCP server，`cmd/sshmng/main.go` 直接 `flag.Parse` + 启动 MCP server。首次使用门槛高，用户需要：

1. 手工修改 Agent 配置文件添加 MCP 配置（JSON 结构、路径、字段名都需查文档）
2. 手工创建 `~/.sshmng/` 目录与 `config.json`（无模板，需翻 README 拼凑）
3. 手工 chmod 0600 / 0700（Unix）或 NTFS ACL（Windows）

README 的"首次配置流程"章节全靠用户复制粘贴，无任何工具辅助。本 spec 通过 CLI 重构 + `install` / `doctor` 子命令解决这个上手门槛。

## 目标

- **CLI 重构为子命令模式**：`sshmng mcp` / `sshmng install` / `sshmng doctor` / `sshmng help`，无参数 print help 到 stdout 并 exit 0
- **`install` 一键安装**：创建 `~/.sshmng/` + `config.json` 空骨架 + `config.example.json`（Pattern A/B 示例），自动检测并注入 MCP 配置到 Claude Code / Hermes Agent / OpenCode 三个 Agent
- **`doctor` 验证**：独立验证配置正确性（文件权限、config 可加载、Agent 条目就位、binary 路径匹配），exit code 三态（0 全过 / 1 有 fail / 2 仅 warn）
- **跨平台**：Unix 与 Windows 均支持，Windows 跳过 Unix perms 检查改为 NTFS ACL 警告
- **幂等**：重复运行 `install` 不破坏用户数据（`config.json` 存在不动，Agent 配置 merge 更新 sshmng 条目）
- **安全**：Agent 配置写入前带时间戳备份，原子写 + 回读校验

## 非目标

- **`wizard` 配置向导**：原计划含 `sshmng wizard` 子命令交互式添加 server/jumphost/proxy，本 spec 移除——v1 不实现，由 Agent 通过 `update_*` 工具或用户手改 `config.json` 替代
- **`--fix` 自动修复**：`doctor` 仅诊断不修复，未来可加
- **`--json` machine-readable 输出**：v1 不实现，预留 flag
- **JSONC 解析**：OpenCode 配置若含 JSONC 注释，v1 报错让用户手动清理，不引入 JSONC parser
- **NTFS ACL 自动设置**：v1 仅警告 + 文档指引手动设置，不调 `icacls`
- **WSL2 跨 env 注入**：用户在 WSL2 跑 sshmng 时不能注入到 Windows 侧 Agent 配置，按 `runtime.GOOS` 取路径
- **多 Agent 配置同时注入到不同 host**：仅注入到本机 Agent 配置
- **编辑/删除实体**：v1 不提供 CLI 子命令做这事，由 Agent 通过 `update_*` 工具完成

## CLI 结构

```
sshmng                          # 无参数 → print help 到 stdout, exit 0
sshmng mcp [--config <path>]    # MCP server 模式（stdio），Agent 配置应使用此子命令
sshmng install [...]            # 一键安装
sshmng doctor [...]             # 验证配置
sshmng help | -h | --help       # 帮助
sshmng <subcommand> -h          # 子命令帮助
```

### 无参数行为

Print help 到 stdout，exit 0。不弹交互式菜单，不做 TTY 检测——子命令就 4 个，`help` 输出已足够让用户知道下一步该跑什么。主流 CLI（`git` / `kubectl`）也是这个惯例。

若 Agent 误用 `sshmng` 无 args 拉起（非 TTY），同样 print help 后 exit 0——Agent 拿到的是帮助文本而非 MCP 协议响应，会自然报错，用户看到 Agent 报错后会查配置（应为 `sshmng mcp`）。无需专门报错路径。

### 子命令分发

- 用 stdlib `flag` + 手写子命令路由（不引入 cobra / urfave，保持依赖最小）
- `main.go` 解析 `os.Args[1]`：
  - 空 → print help 到 stdout, exit 0
  - `mcp` → MCP 模式（保留 `--config` flag）
  - `install` → install 向导
  - `doctor` → doctor 验证
  - `help` / `-h` / `--help` → 帮助
  - 其他 → 报错 + 提示 `sshmng help`
- 各子命令的 flags 在子命令自己的 `FlagSet` 上解析
- `SSHMNG_HOME` 环境变量对所有子命令生效（覆盖 `~/.sshmng/`）

### 向后兼容

旧 Agent 配置（如用户开发期间使用的）写的是 `"command": "sshmng"` 无 args。新结构下 `sshmng` 无 args 打印 help 并退出（不再启动 MCP server），旧配置失效。

迁移路径：用户重跑 `sshmng install`，install 会用 `["mcp"]` args 覆盖 sshmng 条目（merge 语义）。或用户手动改 Agent 配置加 `["mcp"]` args。

## install 子命令

### 交互流程

```
$ sshmng install
sshmng install — first-time setup

This will:
  1. Create sshmng home + config files
  2. Inject sshmng into your AI Agent(s)
  3. Verify setup

Step 1/4: sshmng home directory
Where to store sshmng config? [~/.sshmng]: <enter>
→ ~/.sshmng

Step 2/4: sshmng binary path
Path to sshmng binary [auto: /Users/zhuanz/go/bin/sshmng]: <enter>
→ /Users/zhuanz/go/bin/sshmng

Step 3/4: Select Agents to inject
Detected:
  [*] Claude Code    (~/.claude.json)
  [ ] Hermes Agent   (~/.hermes/config.yaml)
  [ ] OpenCode       (~/.config/opencode/opencode.json)
Toggle (space), confirm (enter), or 's' to skip Agent injection: <enter>
→ Will inject into: Claude Code

Step 4/4: Review
Will write:
  + ~/.sshmng/                      (dir, 0700)
  + ~/.sshmng/config.json           (0600, empty skeleton)
  + ~/.sshmng/config.example.json   (0600, Pattern A/B examples)
  ~ ~/.claude.json                  (merge sshmng entry, backup → .bak.<ts>)

Proceed? [y/N]: y

Executing:
  [ok] Created ~/.sshmng/ (0700)
  [ok] Wrote ~/.sshmng/config.json (0600)
  [ok] Wrote ~/.sshmng/config.example.json (0600)
  [ok] Backed up ~/.claude.json -> ~/.claude.json.bak.20260723-143021
  [ok] Injected sshmng into ~/.claude.json

Verifying (doctor):
  [ok] ~/.sshmng/config.json — readable, 0600
  [ok] sshmng binary — executable
  [ok] Claude Code config — has sshmng entry

Setup complete!

Next steps:
  1. Restart your Agent to load the new MCP config
  2. Ask Agent: "list_ssh_servers"
  3. Add servers by asking Agent "add an SSH server named ..."
     Or manually edit ~/.sshmng/config.json (see config.example.json for examples)
```

### Flags

| Flag | 默认 | 说明 |
|------|------|------|
| `--home <path>` | `$SSHMNG_HOME` or `~/.sshmng` | sshmng 配置目录 |
| `--binary <path>` | `os.Executable()` | sshmng 二进制路径（写入 Agent 配置） |
| `--agents <list>` | 自动检测 | 逗号分隔：`claude-code,hermes,opencode`；`none` 跳过 Agent 注入 |
| `--yes` | false | 非交互，全用默认值 |
| `--skip-files` | false | 跳过 `~/.sshmng/` 创建 |
| `--skip-agents` | false | 跳过 Agent 注入 |

`--skip-wizard` flag 已随 wizard 特性移除。

### 自动检测逻辑

- **home**：`$SSHMNG_HOME` → `os.UserHomeDir() + /.sshmng`
- **binary**：`os.Executable()`（运行时拿到的路径最可靠）→ 退化到 `exec.LookPath("sshmng")`
- **Agents**：检查已知配置文件是否存在
  - Claude Code: `os.UserHomeDir() + /.claude.json`
  - Hermes: `runtime.GOOS == "windows"` 时 `os.Getenv("LOCALAPPDATA") + /hermes/config.yaml`，否则 `os.UserHomeDir() + /.hermes/config.yaml`
  - OpenCode: `os.UserHomeDir() + /.config/opencode/opencode.json`
  - 未检测到的 Agent 不出现在多选列表，但 `--agents` flag 仍可强制指定（用于提前配置未安装的 Agent）

### 幂等性（merge）

- `~/.sshmng/config.json` 已存在 → **不动**，打印 "already exists, skipping"（保护用户数据）
- `~/.sshmng/config.example.json` → **总是覆盖**（示例文件无用户数据，重新生成安全）
- `~/.sshmng/` 目录已存在 → 不动，但校验是 dir 且权限正确（Unix 0700）
- Agent 配置 → **merge**：更新 sshmng 条目（command/args/env），保留其他 MCP servers 条目

### 安全性

- Agent 配置写入前**总是备份**到 `<path>.bak.<YYYYMMDD-HHMMSS>`，如 `~/.claude.json.bak.20260723-143021`
- 不删除旧备份（用户手动清理）
- 备份与原文件同目录、同权限
- **原子写**：写临时文件 → rename 覆盖（Unix）；Windows 下 `os.Rename` 目标存在会失败，流程改为：备份 → 删原文件 → rename temp → 验证（删原文件后失败可从备份恢复）
- 写入后**回读校验**：重新 parse 确认 JSON/YAML 合法且 sshmng 条目就位
- 任一步失败立即停止，打印明确错误，不继续半完成状态

### Windows 特殊处理

- **home 目录**：`os.UserHomeDir()`（Windows 返回 `%USERPROFILE%`）；`$SSHMNG_HOME` 在所有 OS 都生效
- **文件权限**：Windows 跳过 `chmod 0600/0700`（与现有 `main.go` 一致），install 时打印一次性警告：
  ```
  On Windows, manually restrict NTFS ACL on:
    - ~/.sshmng/config.json
    - ~/.sshmng/config.example.json
    - ~/.sshmng/known_hosts (created on first connection)
    - private key files referenced by config
  Right-click -> Properties -> Security, remove all entries except current user.
  ```
- **binary 路径**：`os.Executable()` 在 Windows 返回 `C:\...\sshmng.exe`，直接写入 Agent 配置（无需手动加 `.exe`）
- **输出字符**：用 ASCII（`[ok]` / `->` / `*`）不用 Unicode `✓→•`——Windows Terminal 支持，但旧 cmd.exe 不支持，统一 ASCII 避免乱码（适用于 install / doctor 的所有终端输出）

## Agent 注入器

### 通用接口

```go
package cli

type MCPEntry struct {
    BinaryPath string            // os.Executable() 拿到的路径
    Args       []string          // ["mcp"]
    Env        map[string]string // {"SSHMNG_HOME": "/home/user/.sshmng"}
}

type AgentInjector interface {
    Name() string                                          // "claude-code" / "hermes" / "opencode"
    DisplayName() string                                   // install/doctor 输出显示用
    Detect() (configPath string, installed bool)           // 检测是否安装，返回配置文件路径
    Inject(path string, entry MCPEntry) error              // 注入/merge
    Verify(path string, expectedBinary string) error       // doctor 用：检查 sshmng 条目是否就位
}
```

### 三个 Agent 的字段映射

| Agent | 文件格式 | 顶层 key | command | args | env 字段 | 必需额外字段 | 配置路径（Unix） | 配置路径（Windows） |
|-------|---------|---------|---------|------|---------|-------------|----------------|-------------------|
| Claude Code | JSON | `mcpServers` | string | `["mcp"]` | `env` | 无 | `~/.claude.json` | `%USERPROFILE%\.claude.json` |
| Hermes Agent | YAML | `mcp_servers` | string | `["mcp"]` | `env` | 无 | `~/.hermes/config.yaml` | `%LOCALAPPDATA%\hermes\config.yaml` |
| OpenCode | JSON | `mcp` | **array** `["/path", "mcp"]` | (并入 command) | `environment` | `type: "local"`, `enabled: true` | `~/.config/opencode/opencode.json` | `%USERPROFILE%\.config\opencode\opencode.json` |

Claude Code 与 OpenCode 跨平台路径一致（只是 home 解析不同）。Hermes 按 `runtime.GOOS` 分支。

### 实际写入示例

binary = `/usr/local/bin/sshmng`, home = `/home/user/.sshmng`：

```json
// Claude Code: ~/.claude.json
{
  "mcpServers": {
    "sshmng": {
      "command": "/usr/local/bin/sshmng",
      "args": ["mcp"],
      "env": {"SSHMNG_HOME": "/home/user/.sshmng"}
    }
  }
}
```

```yaml
# Hermes: ~/.hermes/config.yaml
mcp_servers:
  sshmng:
    command: /usr/local/bin/sshmng
    args:
      - mcp
    env:
      SSHMNG_HOME: /home/user/.sshmng
```

```json
// OpenCode: ~/.config/opencode/opencode.json
{
  "mcp": {
    "sshmng": {
      "type": "local",
      "command": ["/usr/local/bin/sshmng", "mcp"],
      "environment": {"SSHMNG_HOME": "/home/user/.sshmng"},
      "enabled": true
    }
  }
}
```

### Merge 逻辑（三 Agent 共用）

1. 读原文件（不存在 → 视作空 `map[string]any{}`）
2. parse 成 `map[string]any`：
   - JSON 用 `encoding/json`
   - YAML 用 `gopkg.in/yaml.v3`（新依赖）
3. 找顶层 key（`mcpServers` / `mcp_servers` / `mcp`），不存在则创建空 map
4. 强转该 key 的 value 为 `map[string]any`，不存在则创建
5. 设 `["sshmng"]` = 本 Agent 的 entry map
6. 备份原文件 → 原子写新文件 → 回读校验

### JSONC 兼容（OpenCode）

- v1 先用 `encoding/json` 解析
- 若失败，报错："OpenCode config at <path> contains JSONC comments or invalid JSON; parse error: <err>. Please remove comments and re-run."
- 未来可换 JSONC parser（如 `github.com/titanous/jsonc`）

### Verify 逻辑（doctor 用）

- Claude Code：`mcpServers.sshmng.command` 存在且等于 `expectedBinary`
- Hermes：`mcp_servers.sshmng.command` 存在且等于 `expectedBinary`
- OpenCode：`mcp.sshmng.command[0]` 存在且等于 `expectedBinary`（注意 command 是数组）
- 不匹配则报 "stale: expected <binary>, got <old>"，建议重跑 `sshmng install`

### 错误处理

- parse 失败：报错 + 指出备份位置，不写新文件
- 写入失败：从最新备份恢复，报错退出
- 路径不可写：报错，建议检查目录权限

## doctor 子命令

### 检查项

| 类别 | 检查项 | 失败等级 | 失败时的提示 |
|------|--------|---------|-------------|
| **sshmng home** | 目录存在且是 dir | FAIL | "Run `sshmng install` to create" |
| | Unix: 0700 perms | FAIL | "chmod 700 ~/.sshmng" |
| | Windows: 跳过 perms，warn NTFS ACL | WARN | "Manually restrict via Properties -> Security" |
| **config.json** | 存在且可读 | FAIL | "Run `sshmng install`" |
| | JSON 可 parse | FAIL | "Backup and re-create, or fix manually" |
| | `config.Store.Load()` 通过（含引用完整性校验） | FAIL | 报具体校验错 |
| | Unix: 0600 perms | FAIL | "chmod 600 ~/.sshmng/config.json" |
| **config.example.json** | 存在？ | WARN | "Run `sshmng install` to regenerate" |
| **binary** | `os.Executable()` 路径可执行 | FAIL | "Binary missing; rebuild with `go build`" |
| **known_hosts** | 若存在：Unix 0600 | FAIL | "chmod 600 ~/.sshmng/known_hosts" |
| | 若不存在 | OK | — |
| **Agent: Claude Code** | `~/.claude.json` 存在？ | FAIL | "Install Claude Code first" |
| | JSON 可 parse？ | FAIL | "Backup and fix manually" |
| | `mcpServers.sshmng` 存在？ | FAIL | "Run `sshmng install --agents claude-code`" |
| | `mcpServers.sshmng.command` == 当前 binary？ | FAIL | "Stale; re-run `sshmng install`" |
| | `mcpServers.sshmng.args` == `["mcp"]`？ | FAIL | "Stale; re-run `sshmng install`" |
| | `mcpServers.sshmng.env.SSHMNG_HOME` == 当前 home？ | FAIL | "Stale; re-run `sshmng install`" |
| **Agent: Hermes** | 同上结构，字段名 `mcp_servers.sshmng` | — | 同上 |
| **Agent: OpenCode** | 同上结构，字段名 `mcp.sshmng`，`command[0]` 校验 | — | 同上 |

未检测到的 Agent 不检查（用户没装就不算问题），输出 `[SKIP]  not detected`。`--agent <name>` flag 可强制检查指定 Agent。

### 输出格式

```
$ sshmng doctor
sshmng doctor — verifying setup

Home:
  [OK]    ~/.sshmng exists, 0700
  [OK]    ~/.sshmng/config.json exists, 0600
  [OK]    config.json parses, 0 validators failed
  [WARN]  ~/.sshmng/config.example.json missing (optional, run `sshmng install` to regenerate)
  [OK]    binary executable at /Users/zhuanz/go/bin/sshmng
  [OK]    known_hosts: 0600

Agents:
  Claude Code (~/.claude.json)
    [OK]    config parses
    [OK]    mcpServers.sshmng exists
    [OK]    command matches current binary
    [OK]    args == ["mcp"]
    [OK]    env.SSHMNG_HOME matches current home

  Hermes Agent (~/.hermes/config.yaml)
    [SKIP]  not detected (install Hermes or pass --agent hermes to force)

  OpenCode (~/.config/opencode/opencode.json)
    [SKIP]  not detected

Summary: 9 passed, 0 failed, 1 warning
```

### Exit code

- `0`：全过
- `1`：有 FAIL
- `2`：仅 WARN 无 FAIL

脚本可按需判断。

### Flags

- `--agent <name>`：只查指定 Agent（claude-code / hermes / opencode）
- `--json`：machine-readable 输出（v1 预留，不实现）
- `--fix`：预留，v1 不实现

## 文件脚手架（files.go）

### `~/.sshmng/config.json`（空骨架，0600）

```json
{
  "version": "1",
  "idle_timeout_s": 300,
  "jumphosts": [],
  "proxies": [],
  "servers": []
}
```

### `~/.sshmng/config.example.json`（示例，0600）

覆盖组合：
- **Proxy**：无 auth / SOCKS5+auth / HTTP+auth
- **Jumphost**：Pattern A 直连 / Pattern A 经 proxy / Pattern B 堡垒机
- **Server**：Pattern A 经 jumphost / Pattern A 经 jumphost+proxy / Pattern B 经堡垒机 / 直连 password（含 LoginFlow 等 PS1）/ 直连 private_key+passphrase

```json
{
  "version": "1",
  "idle_timeout_s": 300,
  "log_level": "info",
  "log_path": "",
  "proxies": [
    {
      "name": "example-socks5",
      "type": "SOCKS5",
      "addr": "socks.corp:1080",
      "tags": ["example"]
    },
    {
      "name": "example-socks5-auth",
      "type": "SOCKS5",
      "addr": "socks.corp:1080",
      "auth": {"user": "proxy-user", "password": "<replace-me>"},
      "tags": ["example", "auth"]
    },
    {
      "name": "example-http-auth",
      "type": "HTTP",
      "addr": "proxy.corp:8080",
      "auth": {"user": "proxy-user", "password": "<replace-me>"},
      "tags": ["example", "auth"]
    }
  ],
  "jumphosts": [
    {
      "name": "example-jumphost-a",
      "addr": "10.0.0.254:22",
      "user": "ops",
      "auth": {"password": "<replace-me>"},
      "ssh_j": true,
      "tags": ["example", "pattern-a"]
    },
    {
      "name": "example-jumphost-a-via-proxy",
      "addr": "10.0.0.254:22",
      "user": "ops",
      "auth": {"password": "<replace-me>"},
      "ssh_j": true,
      "proxy": "example-socks5-auth",
      "tags": ["example", "pattern-a", "via-proxy"]
    },
    {
      "name": "example-jumphost-b",
      "addr": "10.0.0.254:22",
      "user": "ops",
      "auth": {"password": "<replace-me>"},
      "ssh_j": false,
      "login_flow": {
        "wait_menu": {
          "expects": [{"pattern": "Your choice:", "next": "success"}]
        }
      },
      "login_entry": "wait_menu",
      "tags": ["example", "pattern-b"]
    }
  ],
  "servers": [
    {
      "name": "example-server-a",
      "addr": "10.0.0.1:22",
      "user": "deploy",
      "auth": {"password": "<replace-me>"},
      "via": "example-jumphost-a",
      "tags": ["example", "pattern-a"]
    },
    {
      "name": "example-server-a-via-proxy",
      "addr": "10.0.0.1:22",
      "user": "deploy",
      "auth": {"password": "<replace-me>"},
      "via": "example-jumphost-a-via-proxy",
      "proxy": "example-socks5-auth",
      "tags": ["example", "pattern-a", "via-proxy"]
    },
    {
      "name": "example-server-b",
      "addr": "10.0.0.1:22",
      "user": "deploy",
      "auth": null,
      "via": "example-jumphost-b",
      "login_flow": {
        "select_target": {"send": "1\r", "expects": [{"pattern": "Password:", "next": "input_pass"}]},
        "input_pass": {"send": "<replace-me>\r", "expects": [{"pattern": "$ ", "next": "success"}]}
      },
      "login_entry": "select_target",
      "tags": ["example", "pattern-b"]
    },
    {
      "name": "example-server-direct-password",
      "addr": "10.0.0.2:22",
      "user": "deploy",
      "auth": {"password": "<replace-me>"},
      "login_flow": {
        "wait_ps1": {
          "send": "",
          "expects": [{"pattern": "re:.*]# ", "next": "success"}]
        }
      },
      "login_entry": "wait_ps1",
      "tags": ["example", "direct", "login-flow"]
    },
    {
      "name": "example-server-direct-key",
      "addr": "10.0.0.3:22",
      "user": "deploy",
      "auth": {
        "private_key": "/home/user/.ssh/deploy_key",
        "passphrase": "<replace-me>"
      },
      "tags": ["example", "direct", "private-key"]
    }
  ]
}
```

`example-server-direct-password` 演示：直连 server 也能配 LoginFlow，1 步、send 空、regex `re:.*]# ` 等首个 PS1、直接 success。告诉用户 LoginFlow 不仅用于堡垒机菜单，也可用于"等 shell 就绪"。

## 包结构

```
cmd/sshmng/
  main.go               # 入口，子命令分发
internal/cli/           # 新包
  cli.go                # Dispatch() + 帮助渲染
  install.go            # install 向导
  doctor.go             # doctor 验证
  agent_inject.go       # AgentInjector 接口 + 通用 merge/backup/atomic-write 逻辑
  agent_claudecode.go   # ClaudeCodeInjector
  agent_hermes.go       # HermesInjector
  agent_opencode.go     # OpenCodeInjector
  files.go              # ~/.sshmng/ + config.json + config.example.json 脚手架
  prompt.go             # 交互式 prompt 辅助（读入、默认值、校验）
internal/config/        # 复用（Store.Load 用于 doctor 校验）
internal/mcp/           # 不动
internal/ssh/           # 不动
```

新依赖：
- `gopkg.in/yaml.v3`（Hermes 配置是 YAML）

## 错误处理

- **子命令未知**：`sshmng foo` → "Unknown command 'foo'. Run 'sshmng help' for usage." exit 2
- **flag 解析失败**：打印子命令的 usage，exit 2
- **install 中 Agent 配置 parse 失败**：报错 + 指出备份位置，停止后续步骤，exit 1
- **install 中写入失败**：从最新备份恢复，报错退出，exit 1
- **install 中路径不可写**：报错，建议检查目录权限，exit 1
- **doctor 检测到 FAIL**：打印所有 FAIL 项，exit 1
- **doctor 检测到仅 WARN**：打印所有 WARN 项，exit 2
- **`sshmng mcp` 启动失败**：与现有行为一致，bootstrap logger 写 stderr，exit 1

## 测试

### 单元测试

- `internal/cli/agent_inject_test.go`：每个 AgentInjector 的 `Inject` / `Verify`：
  - 空配置文件 → 注入后 sshmng 条目就位
  - 已有其他 MCP server → merge 后其他条目保留
  - 已有 sshmng 条目 → 覆盖为最新
  - parse 失败的配置 → 报错不写
  - Verify 在条目缺失 / command 不匹配 / args 不匹配时分别报错
- `internal/cli/files_test.go`：脚手架生成
  - 目录创建 + 权限（Unix）
  - `config.json` 空骨架可被 `config.Store.Load` 加载
  - `config.example.json` 可被 `config.Store.Load` 加载（所有示例都通过校验）
  - 幂等：`config.json` 存在时不覆盖
- `internal/cli/install_test.go`：端到端 install 流程
  - 用 temp dir 作 home，注入到 temp dir 下的假 Agent 配置
  - 验证文件生成 + Agent 配置 merge + 备份生成
  - `--yes` 非交互模式
  - `--agents none` 跳过 Agent 注入
  - `--skip-files` 跳过文件创建
- `internal/cli/doctor_test.go`：每个检查项的 pass / fail / warn 三态
- `internal/cli/cli_test.go`：子命令分发
  - 各子命令路由正确
  - 未知子命令报错
  - 无参数 / `-h` / `--help` / `help` 输出 help 文本

### 集成测试

- `cmd/sshmng/e2e_test.go`：spawn `sshmng install --yes` 子进程，验证退出码 0 + 文件生成
- `cmd/sshmng/e2e_test.go`：spawn `sshmng doctor` 子进程，验证在 install 后 exit 0

### 不测试

- 真实 Agent（Claude Code / Hermes / OpenCode）的运行时行为——只测配置文件正确性
- Windows 平台的 NTFS ACL——文档指引，不自动化测试
- WSL2 跨 env 注入——v1 不支持

## 后续考虑

- **`wizard` 子命令**：v1 移除。若未来发现用户在 Agent 之外手改 `config.json` 频繁，可重新设计交互式添加 server/jumphost/proxy 的向导
- **`doctor --fix`**：自动修复 stale Agent 条目（重新跑 install 对应部分）、补建缺失的 `config.example.json`、chmod 权限
- **`doctor --json`**：machine-readable 输出，供其他工具消费
- **JSONC parser**：若 OpenCode 用户反馈 JSONC 注释报错多，引入 `github.com/titanous/jsonc`
- **NTFS ACL 自动设置**：`doctor --fix-perms` 调 `icacls` 子进程收紧 Windows 文件权限
- **WSL2 跨 env 注入**：检测 WSL2 环境（`/proc/version` 含 "microsoft"），允许注入到 Windows 侧 Agent 配置
- **`auth` 结构统一**：当前 `Proxy.auth = {user, password}`（user 在 auth 内），`Jumphost.auth` / `SSHServer.auth = {password, private_key, passphrase}`（user 在 auth 外的 entity 上）。未来可统一为"所有 entity 的 user 都在 auth 外"或"都在 auth 内"——breaking change，需 major version bump
- **未支持的 Agent**：未来可加 Cursor / Cline / Continue / Zed 等，每个 Agent 写一个 `AgentInjector` 实现即可

## 集成指南更新（README）

README 的"集成指南"章节需更新：

- Claude Code 配置示例的 args 改为 `["mcp"]`
- Hermes Agent 与 OpenCode 章节新增
- "首次配置流程"章节改为"运行 `sshmng install`，按向导操作"
- 保留手工配置说明作为 fallback（`sshmng install` 失败时用户可手改）
