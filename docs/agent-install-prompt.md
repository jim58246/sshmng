# Agent Install Prompt

Copy the block below and paste it into your AI Agent (Claude Code, Cursor, Hermes Agent, OpenCode). The Agent will install sshmng, inject itself into the Agent config, and verify the setup — no manual shell work needed.

> **Quick shortcut**: if you just want the binary placed on `PATH` without going through the steps below:
> - **macOS / Linux**: `curl -fsSL https://raw.githubusercontent.com/jim58246/sshmng/main/install.sh | bash`
> - **Windows (PowerShell)**: `irm https://raw.githubusercontent.com/jim58246/sshmng/main/install.ps1 | iex`
>
> Then skip to step 5.

---

I want to install **sshmng** — an SSH session manager that runs as an MCP server so AI agents can manage SSH connections for me. Project page: https://github.com/jim58246/sshmng

Please do the following and report progress at each step. Stop and ask me if anything fails or looks unusual.

1. **Detect platform**: identify my OS (macOS / Linux / Windows) and architecture (amd64 / arm64).
2. **Download the latest release** archive from https://github.com/jim58246/sshmng/releases that matches my platform.
   - Archive naming: `sshmng-v<version>-<os>-<arch>.tar.gz` (macOS / Linux) or `.zip` (Windows).
   - Use the GitHub API (`https://api.github.com/repos/jim58246/sshmng/releases/latest`) to find the latest version tag.
3. **Extract** the `sshmng` (or `sshmng.exe` on Windows) binary to a stable location on my `PATH`:
   - macOS / Linux: `/usr/local/bin/sshmng` (use `sudo` if write access requires it) or `~/.local/bin/sshmng`
   - Windows: `%USERPROFILE%\bin\sshmng.exe`, ensure that dir is on `PATH`
4. **macOS only**: if the binary was browser-downloaded and carries a quarantine attribute, remove it with `xattr -d com.apple.quarantine <path>`. (Skip if not quarantined — `curl` / `go install` downloads don't set it.)
5. **Run `sshmng install --yes`** to create `~/.sshmng/` (config skeleton + example) and inject the sshmng MCP entry into my Agent config. The command auto-detects installed Agents (Claude Code / Hermes / OpenCode) and writes a timestamped backup before modifying.
6. **Run `sshmng doctor`** to verify setup. Report exit code (0 = all pass, 1 = FAIL, 2 = WARN-only) and any issues found.
7. **Tell me to restart this Agent** so the new MCP config takes effect. After restart, I'll ask you to call `list_ssh_servers` to confirm — it should return an empty array.

If any step fails, stop, show the error, and propose a fix before proceeding.
