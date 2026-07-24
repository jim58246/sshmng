[English](./README.md) | [简体中文](./README.zh-CN.md)

# sshmng

SSH session manager exposed as an MCP (Model Context Protocol) server. Lets AI agents (Claude Code / Claude Desktop / Hermes Agent / OpenCode / Cursor, etc.) manage SSH connections, run commands, and transfer files through a unified tool interface — with support for interactive bastions and LoginFlow decision trees.

> v1 stage: client runs standalone, stdio single-process, config stored locally. Design doc: [`docs/ssh-session-manager-design.md`](docs/ssh-session-manager-design.md) (Chinese only — translations welcome).

## Features

- **Config CRUD**: `list_*` / `get_*` / `update_*` tool families manage SSHServer / Jumphost / Proxy, with RFC 7396 JSON Merge Patch semantics
- **Explicit session management**: `login` → `run_in_session` → `close_session` trio; consecutive commands share cwd/env
- **Interactive bastion (Pattern B)**: `Jumphost.SSHJ=false` + `LoginFlow` decision tree, auto-navigates menu to log into the target
- **LoginFlow decision tree**: send + expect tree structure, glob / `re:` regex dual mode; on failure returns trace for the Agent to diagnose → patch config → retry
- **TOFU host key**: first connection records public key to `known_hosts`; changes are rejected ("host key changed, possible MITM")
- **sftp file transfer**: `upload` / `download` single files over a dedicated sftp channel, separate from the PTY command channel; graceful degradation when unavailable. `upload_dir` / `download_dir` recursively transfer directory trees, concurrent (default 4), conflict policy overwrite / skip / rename
- **Command diagnostics**: `run_in_session` timeout auto Ctrl-C + drain, returns timed_out/ctrl_c_sent; `get_trace` retrieves command history (including raw_output, ctrl_c_sent)
- **Self-healing config**: Agent diagnoses failures via `error` / `login_trace`, calls `update_*` to patch config, retries `login`
- **First-time setup wizard**: `sshmng install` creates the config directory + template + injects into AI Agents; `sshmng doctor` verifies config correctness

## Install & Build

sshmng is a single binary with no runtime dependencies. Pick one:

```bash
# Option 1: download release binary (recommended, no Go required)
#   From https://github.com/jim58246/sshmng/releases, pick the binary for your OS/Arch
chmod +x sshmng

# Option 2: go install (requires Go 1.25+)
go install github.com/jim58246/sshmng/cmd/sshmng@latest

# Option 3: clone and build locally
git clone https://github.com/jim58246/sshmng.git
cd sshmng && go build -o sshmng ./cmd/sshmng
```

**macOS**: browser-downloaded binaries carry a Gatekeeper quarantine attribute — run `xattr -d com.apple.quarantine sshmng` before first use. `go install` / `go build` binaries don't need this (local compilation). Auto-updated binaries also don't need this (see [docs/auto-update.md](docs/auto-update.md)).

After getting the binary, run `sshmng install` to create `~/.sshmng/` and inject into installed AI Agents (Claude Code / Hermes / OpenCode, etc.). See [Quick Start](#quick-start).

### Build from source

```bash
# Plain build (version.Version is "dev", self-update is disabled)
go build -o sshmng ./cmd/sshmng

# Inject version via ldflags (self-update needs a real version number)
go build -ldflags="-X github.com/jim58246/sshmng/internal/version.Version=v1.2.3" -o sshmng ./cmd/sshmng
```

Without ldflags, `version.Version` defaults to `"dev"`, in which case both `sshmng update` and the `mcp` startup auto-update goroutine are skipped.

Run:

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
```

## Quick Start

```bash
# 1. Build
go build -o sshmng ./cmd/sshmng

# 2. First-time install (creates ~/.sshmng/ + injects into installed AI Agents)
./sshmng install

# 3. Verify config
./sshmng doctor

# 4. Restart your Agent, have it call sshmng:
#    "list_ssh_servers"          → should return an empty array
#    "add an SSH server named prod-web-01 at 10.0.0.1:22 with password ..."
#    "login to prod-web-01 and run df -h"
```

Non-interactive:

```bash
./sshmng install --yes --agents claude-code,hermes
```

For manual config fallback and per-Agent integration steps, see [docs/agents.md](docs/agents.md).

## MCP Tools Overview

18 tools total:

| Category | Tool | Description |
|------|------|------|
| Config query | `list_ssh_servers` / `list_jumphosts` / `list_proxies` | Multi-keyword AND match on name/addr/tags (space-separated, case-insensitive, auth redacted) |
| Config query | `get_ssh_server` / `get_jumphost` / `get_proxy` | Single record by name (full auth) |
| Config update | `update_ssh_server` / `update_jumphost` / `update_proxy` | RFC 7396 JSON Merge Patch; null deletes, object merges/creates |
| Session | `login(name)` → `{sid, sftp_available}` | Dial + LoginFlow + RC injection + sftp channel setup |
| Session | `run_in_session(sid, cmd, timeout_ms?, max_output_bytes?)` | Run command, returns output/exit_code/timed_out/truncated/total_bytes |
| Session | `close_session(sid)` | Force close, trace retained for 10 minutes |
| Session | `stat()` | List all active session summaries (including sftp_available) |
| Diagnostics | `get_trace(sid, last_n?, trunc_output?)` | Retrieve command history (including ctrl_c_sent, raw output) |
| File transfer | `upload(sid, src, dst, timeout_ms?)` | Local → remote, via sftp |
| File transfer | `download(sid, src, dst, timeout_ms?)` | Remote → local, via sftp |
| File transfer | `upload_dir(sid, src, dst, conflict?, concurrency?, timeout_ms?)` | Local directory tree → remote, recursive sftp, concurrent default 4, conflict policy overwrite/skip/rename |
| File transfer | `download_dir(sid, src, dst, conflict?, concurrency?, timeout_ms?)` | Remote directory tree → local, recursive sftp, concurrent default 4, conflict policy overwrite/skip/rename |

> No `send_input` / `send_special` provided: MCP clients serialize tool calls, so during `run_in_session` execution these two tools can't be invoked; after the command ends (normal exit or timeout Ctrl-C), the session is already idle or closed, and calling them also errors. Interactive commands (sudo/read/cat>file) rely on `run_in_session`'s own timeout + `get_trace` for raw_output diagnostics, not on send_input feeding.

## Security Notes

- **Plaintext storage**: v1 stores password / passphrase in plaintext in `config.json`, documented explicitly; if unacceptable, encrypt the whole `config.json` with `age` / `gpg` yourself, decrypt before use
- **TOFU host key**: enabled by default; first connection records public key to `~/.sshmng/known_hosts`, changes rejected ("host key changed, possible MITM"). Can be disabled per-entity via `host_key_verify: false` (completely skips known_hosts read/write, loses MITM protection — only for trusted intranet bastions, etc.); deleting a recorded key still requires manually editing `~/.sshmng/known_hosts`, no tool support
- **Trace contains sensitive data**: `Send` (LoginFlow stage), `Output` (PTY raw stream) may contain passwords; trace is in-memory only, retained for 10 minutes after `close_session` then auto-cleaned, never persisted to disk
- **stdout must never log**: JSON-RPC is dedicated to stdout; operation logs go to the rotating file specified by `config.log_path` (10MB / 5 files, 0600 perms), or no logging if unconfigured; bootstrap errors go to stderr
- **Auth scope (v1)**: only Password + PrivateKey supported; no keyboard-interactive / SSH agent / SSH certificate / 2FA (if your environment requires these, v2 extension or hardcoded interaction in LoginFlow)

## Auto-update

sshmng silently checks for updates in a background goroutine on `mcp` startup (writes `log_path` log only, never stdout). Disable via `{"auto_update_enabled": false}`. Manual update: `sshmng update`. Version check: `sshmng version --check`. Custom source: set `update_url` (see [docs/auto-update.md](docs/auto-update.md) for self-hosted source layout, macOS notes, and release flow).

## Testing & Development

```bash
# Run all tests (with race detector)
go test -race ./...
```

For test coverage and development details, see [docs/development.md](docs/development.md) (Chinese only — translations welcome).

## Documentation

- [Configuration reference](docs/configuration.md) — full config.json field reference, Pattern A/B shape constraints, examples
- [Agent integration guide](docs/agents.md) — Claude Code / Hermes Agent / OpenCode / Claude Desktop detailed config, MCP Inspector debugging, first-time setup flow, typical call flow
- [Auto-update](docs/auto-update.md) — self-hosted HTTP source layout, macOS notes, release flow
- [Architecture & development](docs/development.md) — package structure, key designs, subcommand dispatch, test coverage (Chinese only — translations welcome)
- [Design doc](docs/ssh-session-manager-design.md) — full design spec (PTY sentinel, LoginFlow, session state machine, etc.) (Chinese only — translations welcome)
- [Implementation plan](docs/implementation-plan.md) — v1 implementation progress (Chinese only — translations welcome)

## Contributing

Feel free to open [issues](https://github.com/jim58246/sshmng/issues) for bugs and feature requests.

## License

[MIT](LICENSE) — Copyright (c) 2026 jim58246
