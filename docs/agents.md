[English](./agents.md) | [简体中文](./zh-CN/agents.md)

# Agent Integration Guide

sshmng is a standard stdio MCP server; any MCP-capable client can connect. We recommend using `sshmng install` to auto-inject into installed Agents; this doc also provides manual config per Agent (fallback when `install` fails), debugging tools (MCP Inspector), first-time setup flow, verification commands, and typical call flows.

All Agent configs use the `"args": ["mcp"]` subcommand syntax — `sshmng` without a subcommand only prints help; you must explicitly use `mcp` to start the MCP server.

## Recommended: `sshmng install`

```bash
sshmng install
```

The wizard auto-detects installed AI Agents (Claude Code / Hermes Agent / OpenCode), lets you select which to inject, then writes an sshmng entry into each Agent's config (with a timestamped backup `.bak.<ts>`). Non-interactive:

```bash
sshmng install --yes --agents claude-code,hermes
```

`--agents` values: `claude-code` / `hermes` / `opencode`, comma-separated; `none` skips Agent injection. See `sshmng install -h`.

## Claude Code

Edit `~/.claude.json`:

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

Or register via CLI:

```bash
claude mcp add sshmng sshmng --env SSHMNG_HOME=/Users/<you>/.sshmng
```

Note: CLI registration doesn't auto-add `args: ["mcp"]` (`claude mcp add` treats `sshmng` as server name + command); you'll need to manually edit `~/.claude.json` to add `"args": ["mcp"]`, or just use `sshmng install` to write the correct entry automatically.

After launching `claude`, use `/mcp` to check whether sshmng is connected and tools are loaded.

## Hermes Agent

Edit `~/.hermes/config.yaml` (Unix) or `%LOCALAPPDATA%\hermes\config.yaml` (Windows):

```yaml
mcp_servers:
  sshmng:
    command: /Users/<you>/go/bin/sshmng
    args:
      - mcp
    env:
      SSHMNG_HOME: /Users/<you>/.sshmng
```

Or run `sshmng install` and select Hermes Agent. Hermes's schema matches Claude Code's (`command` string / `args` list / `env` map), just with top-level key `mcp_servers` (YAML) instead of `mcpServers` (JSON).

## OpenCode

Edit `~/.config/opencode/opencode.json`:

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

Or run `sshmng install` and select OpenCode. OpenCode's schema differs from the other two:
- Top-level key is `mcp` (not `mcpServers` / `mcp_servers`)
- `command` is an array (binary + args merged: `["sshmng", "mcp"]`)
- Env field is `environment` (not `env`)
- Requires `type: "local"` and `enabled: true`

## Claude Desktop (macOS)

Edit `~/Library/Application Support/Claude/claude_desktop_config.json`:

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

After restarting Claude Desktop, the tools panel will show `login` / `run_in_session` etc. Claude Desktop is not currently covered by `sshmng install` auto-injection (install only covers Claude Code / Hermes Agent / OpenCode); you need to manually edit the config file in the format above.

## MCP Inspector (for debugging)

```bash
npx @modelcontextprotocol/inspector go run ./cmd/sshmng mcp
```

Inspector provides a GUI to invoke tools directly and inspect requests/responses. Strongly recommended for first-time integration or LoginFlow debugging.

sshmng doesn't push logs via MCP `notifications/message` — all logs go to the file specified by `config.log_path` (or no logging if unconfigured). To see DEBUG logs, set `log_level` to `"debug"` in `config.json` and restart Inspector; logs write to `<log_path>/sshmng.log`.

### Log Configuration

```json
{
  "log_level": "debug",
  "log_path": "/Users/<you>/.sshmng"
}
```

- `log_level`: `debug` / `info` / `warn` / `error` (abbreviations `dbg`/`d`/`inf`/`i`/`w`/`err`/`e`, case-insensitive); empty = default `info`; invalid value fails Load
- `log_path`: log directory; empty = no logging; non-empty = `<log_path>/sshmng.log`, 10MB rotation, max 5 files (`sshmng.log` + `sshmng.1.log` ~ `sshmng.4.log`, 0600 perms)
- Bootstrap-stage errors (config load failure, known_hosts permission error, etc.) go to stderr, visible in Inspector's "Server" panel
- DEBUG logs **fully record** every LoginFlow step's send/read/match, run_in_session's cmd/output, sftp upload/download, PTY stdout snippets (untruncated, unmasked). **Sanitize before sharing** — LoginFlow's `send` field and PTY output may contain passwords

### login_trace Diagnostics

When LoginFlow fails, the `login` tool response contains a `login_trace` JSON field (each step's send / expect / output); the Agent uses this to patch config and retry. After successful login, `get_trace` returns a `login_flow` field (same trace structure) for post-hoc login-process debugging.

## First-Time Setup Flow

Recommended: use the install wizard:

```bash
sshmng install
```

The wizard will:

1. Create `~/.sshmng/` (0700) containing `config.json` (empty skeleton) and `config.example.json` (Pattern A/B examples)
2. Detect installed AI Agents (Claude Code / Hermes Agent / OpenCode), let you select which to inject
3. Write the sshmng MCP entry into each selected Agent's config (with timestamped backup `.bak.<ts>`)
4. Auto-run `sshmng doctor` to verify

Non-interactive:

```bash
sshmng install --yes --agents claude-code,hermes
```

Manual fallback (when `install` fails):

1. Create the config directory:
   ```bash
   mkdir -p ~/.sshmng && chmod 700 ~/.sshmng
   ```
2. Write `~/.sshmng/config.json` (see `config.example.json` template, or use the empty skeleton: `{"version":"1","idle_timeout_s":300,"jumphosts":[],"proxies":[],"servers":[]}`):
   ```bash
   echo '{"version":"1","idle_timeout_s":300,"jumphosts":[],"proxies":[],"servers":[]}' > ~/.sshmng/config.json
   chmod 600 ~/.sshmng/config.json
   ```
3. Private key file (if using PrivateKey auth): place anywhere, permissions must be 0600:
   ```bash
   chmod 600 ~/.ssh/id_ed25519
   ```
4. Edit your Agent's config file (see per-Agent sections above); the sshmng command uses `"args": ["mcp"]`
5. Launch your Agent to test: have it call `list_ssh_servers`, should return an empty array; then call `update_ssh_server` to add your first target.

## Verifying Setup

```bash
sshmng doctor
```

Checks: home directory permissions, `config.json` loadability, each Agent config's sshmng entry exists and binary path matches the current sshmng executable, `args` is `["mcp"]`, `env.SSHMNG_HOME` matches the current home. Exit code: `0` all pass / `1` at least one FAIL / `2` only WARN (no FAIL). On Windows, permission checks downgrade to WARN (NTFS ACL must be set manually).

## Typical Agent Call Flow

```
1. Agent receives "check disk usage on prod-web-01"
2. list_ssh_servers(query="prod-web-01") → 1 candidate, use the name directly
3. login(name="prod-web-01") → {sid: "abc123", sftp_available: true}
4. run_in_session(sid="abc123", cmd="df -h") → output contains disk info
5. close_session(sid="abc123")
```

**Failure loop with LoginFlow diagnostics**:

```
1. login(name="bastion-01") → IsError=true, login_trace=[{send,expect,output}, ...]
2. Agent analyzes trace: second expect didn't match, output shows menu text changed
3. update_ssh_server(name="bastion-01", patch={login_flow:{...}}) fixes the pattern
4. login(name="bastion-01") → success
```
