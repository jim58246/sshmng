[English](../configuration.md) | [简体中文](./configuration.md)

# 配置参考

sshmng 的配置文件是 `~/.sshmng/config.json`（路径可由 `--config` 或 `$SSHMNG_HOME` 覆盖）。本文档涵盖路径解析、权限要求、完整字段参考、Pattern A/B 形态约束与示例。

首次使用建议跑 `sshmng install`，会自动创建 `~/.sshmng/config.json`（空骨架）和 `~/.sshmng/config.example.json`（含 Pattern A/B 示例）。本文档供手动配置或想了解字段细节时参考。

## 路径解析顺序

1. `--config <path>` 命令行参数（仅 `sshmng mcp` 子命令支持）
2. `$SSHMNG_HOME/config.json`
3. `$HOME/.sshmng/config.json`

## 文件权限

Unix（macOS/Linux）下 `config.json` / 私钥文件 / `known_hosts` 必须 `0600`，过宽会被拒绝加载；首次创建时立即 chmod 0600。

Windows 跳过权限检查（NTFS 用 ACL 而非 Unix rwx，`os.FileMode.Perm()` 的 group/other 位恒为 0，检查形同虚设）——需手动用 NTFS ACL 限制这些文件访问（右键→属性→安全，移除除当前用户外的所有条目）。`sshmng install` 和 `sshmng doctor` 在 Windows 上会 WARN 提醒。

## 示例

### Pattern B 交互式堡垒机

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

### Pattern A 透明转发（ssh -J 语义）

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

与 Pattern B 的差异：
- `jumphost.ssh_j=true`，`jumphost.login_flow` 必须为空
- `server.auth` 必填（用于 SSH auth 到 target，跟 Pattern B 相反）
- `server.proxy` 不支持（direct-tcpip 走 jumphost 的 SSH 通道，独立传输代理无意义）
- `server.login_flow` 可选（target 认证后交互，如 `su -` / 角色切换 / PAM）
- SFTP 可用（client 是到 target 的）

## 字段参考

### 顶层 Config

| 字段 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `version` | string | 是 | — | 配置版本，当前固定为 `"1"` |
| `idle_timeout_s` | int | 否 | `300` | session 空闲超时（秒），超时自动 close；`0` 取默认 |
| `log_level` | string | 否 | `"info"` | 日志级别：`debug` / `info` / `warn` / `error`（支持缩写 `dbg`/`d`/`inf`/`i`/`w`/`err`/`e`，大小写不敏感）；配错 Load 报错 |
| `log_path` | string | 否 | — | 日志目录：空 = 不打日志；非空 = `<log_path>/sshmng.log`，10MB 轮转、最多 5 份（`sshmng.log` + `sshmng.1.log` ~ `sshmng.4.log`） |
| `auto_update_enabled` | bool | 否 | `true`（`sshmng install` 创建的骨架） | 是否启用自动更新；`mcp` 启动时后台 goroutine 静默检查（仅写 `log_path` 日志，不输出 stdout）；设 `false` 关闭。注意：手动创建 config.json 且缺省此字段时为 `false`（Go 零值），建议显式设置 |
| `update_url` | string | 否 | — | 自定义更新源 base URL；空 = 走 GitHub Releases；非空 = 从该 URL 拉 `latest.txt` + 归档（布局详见 [自动更新](auto-update.md)） |
| `jumphosts` | []Jumphost | 否 | `[]` | SSH 跳板列表 |
| `proxies` | []Proxy | 否 | `[]` | 传输层代理列表 |
| `servers` | []SSHServer | 否 | `[]` | 目标机列表 |

### Proxy

传输层代理（不参与 SSH 协议，只代理 TCP 连接）。

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `name` | string | 是 | 唯一标识，被 jumphost/server 的 `proxy` 字段引用 |
| `type` | string | 是 | `"HTTP"`（HTTP CONNECT）或 `"SOCKS5"` |
| `addr` | string | 是 | `host:port` |
| `auth` | ProxyAuth | 否 | 代理自身认证；省略 = 无认证 |
| `tags` | []string | 否 | 任意标签，`list_proxies` 的 query 子串会匹配 |

ProxyAuth 结构：`{"user": "...", "password": "..."}`，两字段均可空。

### Jumphost

SSH 跳板。`ssh_j` 字段区分两种形态，决定 LoginFlow / Auth 的必填规则。

| 字段 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `name` | string | 是 | — | 唯一标识，被 server.`via` 或 jumphost.`via` 引用 |
| `addr` | string | 是 | — | `host:port` |
| `user` | string | 是 | — | SSH 用户名 |
| `auth` | SSHAuth | 是 | — | SSH 认证信息（Password 或 PrivateKey） |
| `ssh_j` | bool | 是 | — | `true` = 透明转发（`ssh -J` 语义）；`false` = 交互式堡垒机 |
| `login_flow` | map[string]LoginAction | `ssh_j=false` 必填，`ssh_j=true` 必空 | — | 决策树 |
| `login_entry` | string | `login_flow` 非空时必填 | — | entry action 的 name |
| `max_steps` | int | 否 | `50` | LoginFlow 最大步数，防止死循环 |
| `global_timeout_ms` | int | 否 | `60000` | LoginFlow 整体超时 |
| `host_key_verify` | *bool | 否 | `true`（nil） | 是否启用 TOFU host key 校验；设 `false` 完全跳过（不读不写 known_hosts）。控制到本 jumphost 的 SSH dial |
| `via` | string | 否 | — | 多跳跳板的 jumphost name（v1 不实现多跳） |
| `proxy` | string | 否 | — | 传输代理的 name |
| `tags` | []string | 否 | — | 任意标签 |

### SSHServer

目标机。`via` 是否指向 `ssh_j=false` 的 jumphost 决定走 Pattern A 还是 B，进而决定 `auth` / `login_flow` 必填规则。

| 字段 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `name` | string | 是 | — | 唯一标识，`login` 工具用此 name 连接 |
| `addr` | string | 是 | — | `host:port` |
| `user` | string | 是 | — | SSH 用户名 |
| `auth` | SSHAuth | Pattern A 必填，Pattern B 必空 | — | SSH 认证信息 |
| `login_flow` | map[string]LoginAction | Pattern B 必填，Pattern A 可选 | — | 决策树 |
| `login_entry` | string | `login_flow` 非空时必填 | — | entry action 的 name |
| `max_steps` | int | 否 | `50` | LoginFlow 最大步数 |
| `global_timeout_ms` | int | 否 | `60000` | LoginFlow 整体超时 |
| `host_key_verify` | *bool | 否 | `true`（nil） | 是否启用 TOFU host key 校验；设 `false` 完全跳过（不读不写 known_hosts）。仅直连和 Pattern A 生效；Pattern B（`via.ssh_j=false`）下 target 登录走 PTY 非 SSH dial，此字段不参与，只看 jumphost 的开关 |
| `via` | string | 否 | — | 经由的 jumphost name；空 = 直连 |
| `proxy` | string | 否 | — | 传输代理的 name |
| `tags` | []string | 否 | — | 任意标签，`list_ssh_servers` 的 query 子串会匹配 |

### SSHAuth

SSH 认证信息，复用于 Jumphost 和 SSHServer。`password` 和 `private_key` 二选一；同时配置时仅尝试 `private_key`，失败不回退。

| 字段 | 类型 | 说明 |
|------|------|------|
| `password` | string | 密码认证；空 = 不使用 |
| `private_key` | string | 私钥文件完整路径（PEM 格式），启动时校验权限必须 0600 或更严 |
| `passphrase` | string | 私钥口令；空 = 私钥未加密。仅在 `private_key` 非空时有效 |

Pattern B 下 SSHServer.`auth` 必须为 `null` 或全空对象——凭据写在 `login_flow[action].send` 里。

### LoginAction

决策树节点。一条 `send` + 多个 `expects`（按顺序尝试匹配，首个命中者生效）。

| 字段 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `send` | string | 否 | `""` | 发送字符串，支持 `\n` `\r` `\t` 转义；空 = 仅等待输出。**回车用 `\r`**（TUI 菜单 / sudo 提示等 raw mode 程序只认 `\r`），详见 [设计文档 3.7](../ssh-session-manager-design.md) 的"Send 字节约定" |
| `expects` | []Expect | 是（≥1） | — | 期望的输出模式列表 |
| `timeout_ms` | int | 否 | `10000` | 当前 action 的 read 超时 |

### Expect

LoginAction 的一个分支。`pattern` 命中后跳转到 `next` 指向的 action。

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `pattern` | string | 是 | 匹配模式；无前缀 = glob（shell 风格通配），`re:` 前缀 = Go 正则 |
| `next` | string | 是 | 命中后跳转的 action name；`"success"` = 登录成功（保留字符串，不能作为 login_flow 的 key） |

## 形态与使用约束

**两种 jumphost 形态**：
- `ssh_j=true`：透明转发（`ssh -J` 语义）。客户端经 jumphost 的 direct-tcpip 通道 SSH 到 target，`SSHServer.Auth` 必填，SFTP 可用。LoginFlow 必须为空
- `ssh_j=false`：交互式堡垒机。Jumphost.LoginFlow 把 jumphost 自身驱动到主菜单就绪，SSHServer.LoginFlow 接管选 target + 输入凭据，最终落在 target shell

**直连 server**：`via` 留空，`auth` 必填（Password 或 PrivateKey + 可选 Passphrase）。可选配置 `SSHServer.LoginFlow` 承担 target 认证后交互（如 `su -`、角色切换、PAM session）。

**行为约定**：
- `LoginAction.Send` 是直接字符串，**不支持变量引用**——凭据直接写在 Send 中
- `"success"` 是保留字符串，不能作为 LoginFlow 的 key；每个 LoginFlow 必须至少有一个 Expect 的 `next` 指向 `"success"`，否则永远登录不成功
- `LoginAction.Expects` 至少一条 pattern；每条 pattern 必须非空，`next` 必须非空且指向已存在的 action 或 `"success"`
- `via` / `proxy` 是 name 字符串引用，不是嵌套对象；加载时解析为指针，引用不存在会拒绝加载
- name 在各自集合（jumphosts / proxies / servers）内必须唯一，跨集合可重名
