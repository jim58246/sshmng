[English](../auto-update.md) | [简体中文](./auto-update.md)

# 自动更新

sshmng 支持自动与手动自更新。本文档涵盖自动更新机制、自建 HTTP 源布局（内部镜像 / 离线环境）、macOS 注意事项与 maintainer 发布流程。

## 概览

sshmng 在 `mcp` 启动时后台 goroutine 静默检查更新（仅写 `log_path` 日志，不输出 stdout）。关闭自动更新：

```json
{
  "auto_update_enabled": false
}
```

手动更新：

```bash
sshmng update
```

查看当前版本与最新版本对比：

```bash
sshmng version --check
```

默认从 GitHub Releases 拉取。若需走自建 HTTP 源（内部镜像 / 离线环境），设置 `update_url`：

```json
{
  "update_url": "https://updates.mycompany.com/sshmng"
}
```

注意：当 `config.json` 存在但未设置 `auto_update_enabled` 时，值为 `false`（Go 零值）——建议显式设置。`sshmng install` 骨架默认写 `"auto_update_enabled": true`。

## 自建 HTTP 源布局

源服务器可以是任意静态文件服务（nginx / Caddy / S3 / Python `http.server`）。base URL 下需提供以下文件：

```
{base_url}/
  latest.txt                                    # 一行：v1.2.3
  checksums.txt                                 # goreleaser 生成的 sha256
  sshmng-v1.2.3-darwin-arm64.tar.gz
  sshmng-v1.2.3-darwin-amd64.tar.gz
  sshmng-v1.2.3-linux-amd64.tar.gz
  sshmng-v1.2.3-linux-arm64.tar.gz
  sshmng-v1.2.3-windows-amd64.zip
  sshmng-v1.2.3-windows-arm64.zip
```

发布新版本：执行 `goreleaser release --clean`，把 `dist/sshmng-*` 归档与 `dist/checksums.txt` 复制到服务器，再更新 `latest.txt` 为新版本号。

## macOS 注意

若通过符号链接调用 sshmng（如 `~/.local/bin/sshmng -> ~/go/bin/sshmng`），自更新会替换符号链接而非目标二进制。请以普通文件方式安装（`go install` / `sshmng install` 的默认行为）以避免此问题。

## 发布流程（maintainers）

```bash
git tag v1.2.3
git push origin v1.2.3
```

`release` GitHub Actions workflow 会触发 goreleaser，依次：

1. 构建 6 个平台归档（darwin / linux / windows × amd64 / arm64）
2. 生成 `checksums.txt`
3. 用该 tag 创建 GitHub Release
4. 把归档与 checksums 上传为 release assets

用户执行 `sshmng update` 或 `sshmng mcp`（自动更新）时，会在 1 小时内（缓存 TTL）感知到新版本。
