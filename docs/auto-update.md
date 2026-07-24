[English](./auto-update.md) | [简体中文](./zh-CN/auto-update.md)

# Auto-update

sshmng supports automatic and manual self-update. This doc covers the auto-update mechanism, self-hosted HTTP source layout (for internal mirrors / offline environments), macOS-specific notes, and the maintainer release flow.

## Overview

sshmng silently checks for updates in a background goroutine on `mcp` startup (writes `log_path` log only, never stdout). To disable auto-update:

```json
{
  "auto_update_enabled": false
}
```

Manual update:

```bash
sshmng update
```

Check current version against the latest:

```bash
sshmng version --check
```

By default, sshmng pulls from GitHub Releases. To use a self-hosted HTTP source (internal mirror / offline environment), set `update_url`:

```json
{
  "update_url": "https://updates.mycompany.com/sshmng"
}
```

Note: when `config.json` exists but `auto_update_enabled` is omitted, the value is `false` (Go zero value) — recommend setting it explicitly. The `sshmng install` skeleton writes `"auto_update_enabled": true` by default.

## Self-hosted HTTP source layout

The source server can be any static file server (nginx / Caddy / S3 / Python `http.server`). The base URL must serve the following files:

```
{base_url}/
  latest.txt                                    # single line: v1.2.3
  checksums.txt                                 # goreleaser-generated sha256
  sshmng-v1.2.3-darwin-arm64.tar.gz
  sshmng-v1.2.3-darwin-amd64.tar.gz
  sshmng-v1.2.3-linux-amd64.tar.gz
  sshmng-v1.2.3-linux-arm64.tar.gz
  sshmng-v1.2.3-windows-amd64.zip
  sshmng-v1.2.3-windows-arm64.zip
```

To release a new version: run `goreleaser release --clean`, copy `dist/sshmng-*` archives and `dist/checksums.txt` to the server, then update `latest.txt` to the new version number.

## macOS note

If you invoke sshmng via a symlink (e.g. `~/.local/bin/sshmng -> ~/go/bin/sshmng`), self-update will replace the symlink, not the target binary. Install as a regular file (`go install` / `sshmng install` default behavior) to avoid this.

## Release flow (maintainers)

```bash
git tag v1.2.3
git push origin v1.2.3
```

The `release` GitHub Actions workflow triggers goreleaser, which:

1. Builds 6 platform archives (darwin / linux / windows × amd64 / arm64)
2. Generates `checksums.txt`
3. Creates a GitHub Release from the tag
4. Uploads archives and checksums as release assets

Users running `sshmng update` or `sshmng mcp` (auto-update) will see the new version within 1 hour (cache TTL).
