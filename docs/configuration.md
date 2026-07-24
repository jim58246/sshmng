[English](./configuration.md) | [简体中文](./zh-CN/configuration.md)

# Configuration Reference

sshmng's config file is `~/.sshmng/config.json` (path can be overridden via `--config` or `$SSHMNG_HOME`). This doc covers path resolution, permission requirements, the full field reference, Pattern A/B shape constraints, and examples.

For first-time use, run `sshmng install` — it creates `~/.sshmng/config.json` (empty skeleton) and `~/.sshmng/config.example.json` (with Pattern A/B examples). This doc is for manual config or when you want to understand field details.

## Path Resolution Order

1. `--config <path>` CLI arg (only the `sshmng mcp` subcommand supports this)
2. `$SSHMNG_HOME/config.json`
3. `$HOME/.sshmng/config.json`

## File Permissions

On Unix (macOS/Linux), `config.json` / private key files / `known_hosts` must be `0600`; looser permissions are rejected at load. First-time creation immediately chmods to 0600.

Windows skips permission checks (NTFS uses ACLs, not Unix rwx; `os.FileMode.Perm()`'s group/other bits are always 0, making the check vacuous) — you must manually restrict these files via NTFS ACL (right-click → Properties → Security, remove all entries except the current user). `sshmng install` and `sshmng doctor` emit a WARN on Windows.

## Examples

### Pattern B Interactive Bastion

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

### Pattern A Transparent Forwarding (ssh -J semantics)

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

Differences from Pattern B:
- `jumphost.ssh_j=true`, `jumphost.login_flow` must be empty
- `server.auth` is required (used for SSH auth to target, opposite of Pattern B)
- `server.proxy` not supported (direct-tcpip goes through jumphost's SSH channel; a separate transport proxy is meaningless)
- `server.login_flow` optional (post-target-auth interaction, e.g. `su -` / role switch / PAM)
- SFTP available (the client is to the target)

## Field Reference

### Top-level Config

| Field | Type | Required | Default | Description |
|------|------|------|------|------|
| `version` | string | yes | — | Config version, currently fixed at `"1"` |
| `idle_timeout_s` | int | no | `300` | Session idle timeout (seconds); auto-close on expiry; `0` takes default |
| `log_level` | string | no | `"info"` | Log level: `debug` / `info` / `warn` / `error` (abbreviations `dbg`/`d`/`inf`/`i`/`w`/`err`/`e` supported, case-insensitive); invalid value fails Load |
| `log_path` | string | no | — | Log directory: empty = no logging; non-empty = `<log_path>/sshmng.log`, 10MB rotation, max 5 files (`sshmng.log` + `sshmng.1.log` ~ `sshmng.4.log`) |
| `auto_update_enabled` | bool | no | `true` (skeleton created by `sshmng install`) | Whether auto-update is enabled; background goroutine silently checks on `mcp` startup (writes `log_path` log only, never stdout); set `false` to disable. Note: when config.json exists but this field is omitted, the value is `false` (Go zero value) — recommend setting explicitly |
| `update_url` | string | no | — | Custom update source base URL; empty = use GitHub Releases; non-empty = pull `latest.txt` + archives from this URL (see [Auto-update](auto-update.md) for layout) |
| `jumphosts` | []Jumphost | no | `[]` | SSH jump host list |
| `proxies` | []Proxy | no | `[]` | Transport-layer proxy list |
| `servers` | []SSHServer | no | `[]` | Target host list |

### Proxy

Transport-layer proxy (doesn't participate in SSH protocol, just proxies the TCP connection).

| Field | Type | Required | Description |
|------|------|------|------|
| `name` | string | yes | Unique identifier; referenced by jumphost/server `proxy` field |
| `type` | string | yes | `"HTTP"` (HTTP CONNECT) or `"SOCKS5"` |
| `addr` | string | yes | `host:port` |
| `auth` | ProxyAuth | no | Proxy's own auth; omitted = no auth |
| `tags` | []string | no | Arbitrary tags; `list_proxies` query substring-matches |

ProxyAuth structure: `{"user": "...", "password": "..."}`, both fields can be empty.

### Jumphost

SSH jump host. The `ssh_j` field distinguishes two shapes, determining LoginFlow / Auth required-field rules.

| Field | Type | Required | Default | Description |
|------|------|------|------|------|
| `name` | string | yes | — | Unique identifier; referenced by server.`via` or jumphost.`via` |
| `addr` | string | yes | — | `host:port` |
| `user` | string | yes | — | SSH username |
| `auth` | SSHAuth | yes | — | SSH auth info (Password or PrivateKey) |
| `ssh_j` | bool | yes | — | `true` = transparent forwarding (`ssh -J` semantics); `false` = interactive bastion |
| `login_flow` | map[string]LoginAction | required when `ssh_j=false`, must be empty when `ssh_j=true` | — | Decision tree |
| `login_entry` | string | required when `login_flow` non-empty | — | Name of the entry action |
| `max_steps` | int | no | `50` | LoginFlow max steps, prevents infinite loops |
| `global_timeout_ms` | int | no | `60000` | LoginFlow overall timeout |
| `host_key_verify` | *bool | no | `true` (nil) | Whether to enable TOFU host key verification; set `false` to skip entirely (no known_hosts read/write). Controls SSH dial to this jumphost |
| `via` | string | no | — | Jumphost name for multi-hop (v1 doesn't implement multi-hop) |
| `proxy` | string | no | — | Transport proxy name |
| `tags` | []string | no | — | Arbitrary tags |

### SSHServer

Target host. Whether `via` points to a `ssh_j=false` jumphost determines Pattern A vs B, which in turn determines `auth` / `login_flow` required-field rules.

| Field | Type | Required | Default | Description |
|------|------|------|------|------|
| `name` | string | yes | — | Unique identifier; `login` tool uses this name to connect |
| `addr` | string | yes | — | `host:port` |
| `user` | string | yes | — | SSH username |
| `auth` | SSHAuth | required for Pattern A, must be empty for Pattern B | — | SSH auth info |
| `login_flow` | map[string]LoginAction | required for Pattern B, optional for Pattern A | — | Decision tree |
| `login_entry` | string | required when `login_flow` non-empty | — | Name of the entry action |
| `max_steps` | int | no | `50` | LoginFlow max steps |
| `global_timeout_ms` | int | no | `60000` | LoginFlow overall timeout |
| `host_key_verify` | *bool | no | `true` (nil) | Whether to enable TOFU host key verification; set `false` to skip entirely (no known_hosts read/write). Only effective for direct connection and Pattern A; under Pattern B (`via.ssh_j=false`), target login goes through PTY not SSH dial, so this field is inert — only the jumphost's flag matters |
| `via` | string | no | — | Jumphost name to go through; empty = direct connection |
| `proxy` | string | no | — | Transport proxy name |
| `tags` | []string | no | — | Arbitrary tags; `list_ssh_servers` query substring-matches |

### SSHAuth

SSH auth info, reused by Jumphost and SSHServer. Choose one of `password` or `private_key`; if both configured, only `private_key` is attempted, no fallback on failure.

| Field | Type | Description |
|------|------|------|
| `password` | string | Password auth; empty = not used |
| `private_key` | string | Full path to private key file (PEM format); permissions must be 0600 or stricter, validated at startup |
| `passphrase` | string | Private key passphrase; empty = key is unencrypted. Only effective when `private_key` is non-empty |

Under Pattern B, SSHServer.`auth` must be `null` or an all-empty object — credentials go in `login_flow[action].send`.

### LoginAction

Decision tree node. One `send` + multiple `expects` (tried in order, first match wins).

| Field | Type | Required | Default | Description |
|------|------|------|------|------|
| `send` | string | no | `""` | String to send; supports `\n` `\r` `\t` escapes; empty = just wait for output. **Use `\r` for Enter** (TUI menus / sudo prompts and other raw-mode programs only recognize `\r`); see [design doc 3.7](ssh-session-manager-design.md) "Send byte conventions" (Chinese only — translations welcome) |
| `expects` | []Expect | yes (≥1) | — | List of expected output patterns |
| `timeout_ms` | int | no | `10000` | Read timeout for this action |

### Expect

A branch of LoginAction. When `pattern` matches, jump to the action pointed to by `next`.

| Field | Type | Required | Description |
|------|------|------|------|
| `pattern` | string | yes | Match pattern; no prefix = glob (shell-style wildcard), `re:` prefix = Go regex |
| `next` | string | yes | Action name to jump to on match; `"success"` = login successful (reserved string, can't be used as a login_flow key) |

## Shape and Usage Constraints

**Two jumphost shapes**:
- `ssh_j=true`: transparent forwarding (`ssh -J` semantics). Client SSH-es to target through jumphost's direct-tcpip channel; `SSHServer.Auth` required; SFTP available. LoginFlow must be empty
- `ssh_j=false`: interactive bastion. Jumphost.LoginFlow drives the jumphost itself to main-menu ready, then SSHServer.LoginFlow takes over to select target + enter credentials, finally landing in the target shell

**Direct-connect server**: `via` empty, `auth` required (Password or PrivateKey + optional Passphrase). Optionally configure `SSHServer.LoginFlow` for post-target-auth interaction (e.g. `su -`, role switch, PAM session).

**Behavioral conventions**:
- `LoginAction.Send` is a literal string, **no variable references** — credentials are written directly in Send
- `"success"` is a reserved string, can't be used as a LoginFlow key; every LoginFlow must have at least one Expect with `next` pointing to `"success"`, otherwise login can never succeed
- `LoginAction.Expects` must have at least one pattern; each pattern must be non-empty, `next` must be non-empty and point to an existing action or `"success"`
- `via` / `proxy` are name string references, not nested objects; resolved to pointers at load, unresolvable references are rejected
- Names must be unique within each collection (jumphosts / proxies / servers); cross-collection name reuse is allowed
