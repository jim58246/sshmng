# Agent 安装 prompt

把下面这块内容整段复制粘贴给你的 AI Agent（Claude Code / Cursor / Hermes Agent / OpenCode）。Agent 会自动装好 sshmng、把自己注入到 Agent 配置里、跑 doctor 验证——全程不用手动开 shell。

> **Unix 快捷方式**：如果你在 macOS / Linux，只想先把二进制放到 `PATH` 上，可以（让 Agent）先跑 `curl -fsSL https://raw.githubusercontent.com/jim58246/sshmng/main/install.sh | bash`，然后直接跳到第 5 步。

---

我想安装 **sshmng**——一个以 MCP server 形式运行的 SSH 会话管理工具，让 AI Agent 帮我管理 SSH 连接。项目地址：https://github.com/jim58246/sshmng

请按下面的步骤操作，每一步都报告进度。如果哪步失败或看起来不对，停下问我，不要自己继续。

1. **检测平台**：识别我的 OS（macOS / Linux / Windows）和架构（amd64 / arm64）。
2. **下载最新 release** 归档，从 https://github.com/jim58246/sshmng/releases 找匹配我平台的文件。
   - 命名规则：`sshmng-v<version>-<os>-<arch>.tar.gz`（macOS / Linux）或 `.zip`（Windows）。
   - 用 GitHub API（`https://api.github.com/repos/jim58246/sshmng/releases/latest`）拿最新版本号。
3. **解压** `sshmng`（Windows 上是 `sshmng.exe`）二进制到我 `PATH` 上的稳定位置：
   - macOS / Linux：`/usr/local/bin/sshmng`（需要时用 `sudo`）或 `~/.local/bin/sshmng`
   - Windows：`%USERPROFILE%\bin\sshmng.exe`，确保该目录在 `PATH` 上
4. **仅 macOS**：如果二进制是浏览器下载的且带隔离属性，用 `xattr -d com.apple.quarantine <path>` 移除。（没有就跳过——`curl` / `go install` 下载不会带。）
5. **跑 `sshmng install --yes`**：创建 `~/.sshmng/`（配置骨架 + 示例），把 sshmng MCP entry 注入到我的 Agent 配置里。命令会自动检测已装的 Agent（Claude Code / Hermes / OpenCode），改配置前会先做带时间戳的备份。
6. **跑 `sshmng doctor`** 验证。报告退出码（0 = 全通过 / 1 = 至少一个 FAIL / 2 = 仅 WARN）和发现的问题。
7. **告诉我重启 Agent** 让新 MCP 配置生效。重启后我会让你调一次 `list_ssh_servers` 确认——应返回空数组。

任何一步失败时停下，告诉我错误，先提修复方案再继续。
