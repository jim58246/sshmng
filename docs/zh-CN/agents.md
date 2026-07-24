[English](../agents.md) | [简体中文](./agents.md)

# Agent 集成指南

sshmng 是标准 stdio MCP server，任何支持 MCP 的客户端都能接入。推荐用 `sshmng install` 自动注入到已安装的 Agent；本文档也给出各 Agent 的手动配置（`install` 失败时的 fallback）、调试工具（MCP Inspector）、首次配置流程、验证命令与典型调用流程。

所有 Agent 配置都走 `"args": ["mcp"]` 子命令语法——`sshmng` 不带子命令时只打印帮助，必须显式用 `mcp` 启动 MCP server。

## 推荐：`sshmng install`

```bash
sshmng install
```

向导会自动检测已安装的 AI Agent（Claude Code / Hermes Agent / OpenCode），让你勾选要注入哪些，然后在每个 Agent 配置里写入 sshmng entry（带时间戳备份 `.bak.<ts>`）。非交互场景：

```bash
sshmng install --yes --agents claude-code,hermes
```

`--agents` 取值：`claude-code` / `hermes` / `opencode`，逗号分隔；`none` 跳过 Agent 注入。详见 `sshmng install -h`。

## Claude Code

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

## Hermes Agent

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

## OpenCode

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

## Claude Desktop (macOS)

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

## MCP Inspector（调试用）

```bash
npx @modelcontextprotocol/inspector go run ./cmd/sshmng mcp
```

Inspector 提供 GUI 直接调用工具、查看请求/响应。首次集成或排查 LoginFlow 时强烈建议先用 Inspector 验证一遍。

sshmng 不通过 MCP `notifications/message` 推日志——所有日志走 `config.log_path` 指定的文件（未配置则不打日志）。要看 DEBUG 日志，把 `config.json` 的 `log_level` 设为 `"debug"` 后重启 Inspector 即可，日志写到 `<log_path>/sshmng.log`。

### 日志配置

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

### login_trace 诊断

LoginFlow 失败时，`login` 工具响应含 `login_trace` JSON 字段（每步 send / expect / output），Agent 据此修配置重试。login 成功后，`get_trace` 返回值含 `login_flow` 字段（同样的 trace 结构），用于事后排查登录过程。

## 首次配置流程

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
4. 编辑 Agent 的配置文件（参考上方各 Agent 章节），sshmng 命令用 `"args": ["mcp"]`
5. 启动 Agent 测试：让 Agent 调一次 `list_ssh_servers`，应返回空数组；再调 `update_ssh_server` 添加第一个目标。

## Verifying setup

```bash
sshmng doctor
```

检查项：home 目录权限、`config.json` 可加载性、各 Agent 配置中 sshmng entry 存在且 binary path 匹配当前 sshmng 可执行文件、`args` 是 `["mcp"]`、`env.SSHMNG_HOME` 匹配当前 home。退出码：`0` 全通过 / `1` 至少一个 FAIL / `2` 仅 WARN（无 FAIL）。Windows 下权限检查降级为 WARN（NTFS ACL 需手动设置）。

## 典型 Agent 调用流程

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
