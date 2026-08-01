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

## Update from a local file (bypass GitHub API rate limit)

sshmng's auto-update hits GitHub's unauthenticated API, shared across all processes at 60 req/hour. When you hit the rate limit, download the release asset manually with your browser (your GitHub login session doesn't count against the API quota) and apply it locally:

```bash
sshmng update --file ~/Downloads/sshmng-v0.1.4-darwin-arm64.tar.gz
```

`--file` accepts three input forms (so you don't have to fight Safari's auto-extraction):

- `.tar.gz` — the original goreleaser archive. Just pass it as-is.
- `.tar` — Safari strips the `.gz` layer automatically; pass the resulting `.tar` file.
- **a directory** — if Safari extracted the archive into a folder, pass the folder path. sshmng walks it to find the `sshmng` binary.

No checksum verification is performed — you downloaded the file yourself, so you are the trust source. Platform is still validated from the filename (directory input skips this check).

`--file` mode does not check version freshness — it applies whatever you give it, even a downgrade (useful for rollback). Dev builds are allowed (you can use a release archive to upgrade a `dev` binary).

If the update fails with a permission error, your binary is installed in a system-owned location (e.g., `/usr/local/bin`). Reinstall to a user-writable path (`~/.local/bin` or `~/go/bin`).

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

Auto-updated binaries do **not** carry the Gatekeeper quarantine attribute (`com.apple.quarantine`) — no `xattr -d` needed after `sshmng update`. The quarantine attribute is only set by macOS LaunchServices (invoked by browser/Mail downloads); `sshmng update` downloads via Go's `net/http` and extracts via Go's `archive/tar` / `archive/zip`, neither of which touches LaunchServices. Browser-downloaded release binaries do need `xattr -d com.apple.quarantine sshmng` before first run (see README install section). `sshmng update --file` also bypasses LaunchServices (it reads the local file directly and applies the binary via Go's file APIs), so no `xattr -d` is needed on the binary after a `--file` update. Safari may have already extracted the archive for you — just pass the `.tar` file or the extracted directory to `--file`.

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
