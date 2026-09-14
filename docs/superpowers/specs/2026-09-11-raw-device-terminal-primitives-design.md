# Raw 设备支持:终端原语 send/read (#raw)

## 背景

交换机等网络设备(目标:华为 VRP、Cisco IOS/IOS-XE、H3C Comware)走 SSH 登录,但没有 unix shell:

- 无法 `stty`,无法注入 RC 脚本,无 PS1/token 哨兵
- CLI 交互模式(`sshmng ssh <name>`)不受影响(`needShell=false` 本就跳过探测)
- MCP `login` 必挂:三条 setup 路径(direct/PatternA/PatternB)均无条件 `DetectShell`(发送 shell 探测命令等回显)→ 交换机上必然超时
- `run_in_session` 不可实现:依赖 token 化 PS1(bash/zsh)或 `__P_<sid>__>` 提示符(dash/ash)判断命令完成

## 目标

1. raw 设备可通过 MCP `login` 建会话,跳过 shell 探测/RC 注入
2. 暴露终端原语 `send_in_session` / `read_in_session`,AI 像人操作终端一样交互:完成判断、分页处理全由 AI 自适应
3. 原语对所有 session 开放 → unix 主机的持续型命令(`tail -f`/`top`/`vim`)也可操作
4. 服务端只提供能力与信息(输出、`idle_ms`、tags),不做策略(不写厂商配方、不做服务端完成判断)

## 非目标

- 服务端 prompt 匹配 / expect 式工具(未来可作向后兼容增量,届时提示符可由 AI 每次提供)
- ANSI 屏幕渲染(vim/top 全屏重绘 v1 以原始转义序列呈现)
- Jumphost 的 `raw` 字段(raw 性质属于 target;Pattern B 下 target 标志生效即可)
- 厂商分页配方硬编码(代码/文档均不写死;由 AI 结合 tags 与输出现场自适应)

## 设计

### 配置

`SSHServer` 新增一个字段:

```json
{ "name": "sw-core-1", "addr": "10.0.0.1:22", "user": "admin", "auth": {}, "raw": true }
```

- `raw: true` → 无 unix shell。不引入 `prompt`/`quiet_ms`/`device_type` 等字段。
- `tags` 字段已存在,本设计将其透传到 login/stat 返回,作为人→AI 的提示通道;服务端不理解 tag 内容。

### Login 变更

| 路径 | 变更 |
|---|---|
| MCP setupDirect/PatternA/PatternB | `srv.Raw == true` 时跳过 `DetectShell` + `InjectRC`;LoginFlow(菜单/2FA/堡垒机链)照常跑;`pty.shell` 置 `"raw"` |
| CLI 交互模式 | 无变化(今天即可用) |
| CLI 非交互 `sshmng ssh <raw> <cmd>` | 明确报错 "raw device: no unix shell, non-interactive mode not supported; use interactive mode" (early error,替代现在难懂的 detect-shell 超时;消息语言随代码库约定用英文) |
| CLI `sshmng ssh <jumphost>` | 无变化 |

双保险:`PtyConn.Run` 对 `shell=="raw"` 返回错误,防止哨兵逻辑被误触。

### 新 MCP 工具(19 → 21)

**`send_in_session(sid, input)`**

- `input` 写入 PTY stdin,服务端先解释最小转义集:`\r`(CR)、`\n`(LF)、`\t`(TAB)、`\e`(ESC)、`\uXXXX`(码点,如 Ctrl-C)、`\\`(字面反斜杠);其余字节 verbatim。回车/控制键由 AI 以转义形式自带
- 仅 idle 状态可用;输入上限 64KB(服务端强制,转义前计量)
- 返回 `{sid, sent_bytes}`

**`read_in_session(sid, wait_ms?, max_bytes?)`**

- 顺序游标语义:返回自上次 read 之后新产生的输出。数据在 stdoutCh 排队,不读不丢;`max_bytes` 截断时剩余**留在队列**,`more: true` 提示继续读
- quiet 吸收:等首字节(至多 wait_ms;调用时已有数据则立即返回)→ 持续吸收,字节间隔 < quiet_gap(内部 400ms,不可配)即继续;直到 静默 gap / wait_ms 总时限 / max_bytes 先到
- 返回 `{output, more, idle_ms}`;`idle_ms` = 距上次收到任何输出的毫秒数(初值为 login 时刻),**信息性信号**,AI 判断完成度的依据之一
- PTY 关闭(设备重启/链路断)→ 报 `connection lost`,session 置 closed 进 graveyard

限额(全部服务端强制,不信任 AI 传参):

| 参数 | 默认 | 上限 |
|---|---|---|
| `wait_ms` | 5000 | 60000 |
| `max_bytes` | 131072 (128KB) | 1MB |
| `send input` | — | 64KB |

### 会话状态机

- 复用现有 idle/running/closed:send/read **仅 idle 可用**;`run_in_session` 期间状态为 running,send/read 被拒(session busy)
- raw session 上 `run_in_session` 报错:"raw device: use send_in_session/read_in_session"
- unix session 混用:允许 idle 间隔混用;未读尽的残留输出会被下一次 `run_in_session` 的 setup 步骤当作噪声丢弃(现有 pushback 清理),文档说明
- upload/download 类工具不受影响(交换机无 sftp 子系统,`sftp_available=false` 自然拒绝)

### login / stat 返回与 tags 透传

- `login` 返回增加 `mode: "raw"|"shell"` 与 `tags`(来自 `srv.Tags`,nil 归一为 `[]`)
- `Session` 增加 `tags []string` 字段 + `SetTags()` setter(照 `SetLoginFlowTrace` 模式,不改 `NewSession` 签名);登录时快照,stat 反映登录时刻状态,不反查 config
- `SessionStat` 增加 `Tags []string json:"tags,omitempty"` 与 `Mode string json:"mode"`(由 `s.raw` 派生 `"raw"|"shell"`)
- raw 标志同样以 setter(`SetRaw`)进 Session,供 `run_in_session` 拒绝判断
- 工具 description 同步:login 返回 `{sid, server_name, sftp_available, mode, tags}`;stat 每条含 `mode` 与 `tags`

### trace

- 每次 send 记一条 `CommandTrace`(input 作 cmd)
- read 的输出追加到最近一次 send 的条目;无 send 时记 `"(read)"` 条目
- `get_trace` 排障时可见 AI 发了什么、收到什么

### MCP server instructions 更新

- 说明 `mode` 语义:raw → 用 send/read;shell → 优先 `run_in_session`
- send/read 使用模式:大 `wait_ms`(3-10s),勿小值轮询;`idle_ms` 大且内容自洽即认为完成,无需确认读;`more=true` 连续 read 排空
- 分页自适应提示:输出可能在分页标记处暂停(各厂商标记不同,如 `---- More ----`);结合 tags 与输出现场自行选择翻页键或禁用分页命令;服务端不写厂商配方

### 错误处理

- read 遇 stdoutCh 关闭 → `connection lost` → session closed
- send 写 stdin 失败 → 连接已坏,session closed
- raw 设备 login 阶段 LoginFlow 失败:沿用现有 `login_trace` 诊断机制

### 不改的部分

- 哨兵机制全部逻辑(runWithToken/runPS1Only/InjectRC/DetectShell)——unix 路径零回归
- `close_session` / `get_trace` 工具签名
- config 校验框架(仅加字段)

## 测试(TDD)

**单元**

- config:`raw` 字段序列化/roundtrip/merge patch
- login 三路径:raw 时 DetectShell/InjectRC 被跳过(shell="raw");非 raw 行为不变
- Session:send/read 状态机拒绝(running/closed/raw 上 run_in_session);SetTags/SetRaw 快照
- read 语义:quiet 吸收(gap 时间可注入以便测试)、more 留队不丢、idle_ms、限额、connection lost
- trace:send/read 条目规则

**E2E:假交换机 fixture**

Go 测试内起 sshd,login 后给 `Switch>` 提示符 CLI(无 shell、回显输入):`show version`(短输出)、`show run`(分页:输出 N 行后停 `---- More ----`,空格续页、q 退出)、`reboot`(关连接)。测试扮演 AI 角色,全链路验证:login(mode=raw)→ send/read → 分页交互 → Ctrl-C → connection lost。该 fixture 同时守护"无 shell 设备不会误触 DetectShell"。

**回归**:现有全部测试保持绿。

## 风险

- **quiet gap 误判**:流式输出中途停顿 >400ms 时 read 提前返回——AI 凭内容(未见结尾/提示符)与再次 read 自纠;这是"AI 是完成判断者"模型的固有属性,文档说明
- **AI 小值轮询浪费 token**:靠工具 description 硬性指引 + 默认 wait_ms=5000 兜底
- **vim/top 原始转义序列可读性**:AI 通常可读;不可行时未来加 screen 模式,不影响本设计
- **残留输出混淆 unix 混用场景**:已有 pushback 丢弃机制兜底,e2e 覆盖

## 文档

- `README.md` + `README.zh-CN.md`:MCP 工具数 19→21、send/read 简介、raw 字段
- `docs/agents.md` + zh-CN:两新工具签名与使用模式、login/stat 新返回字段、instructions 摘要
- `docs/configuration.md` + zh-CN:`raw` 字段
- 按 pre-release checklist 双向同步

## 变更记录

### 2026-09-14:send_in_session 由纯 verbatim 改为收敛式转义

v0.2.0 实际使用中发现:纯 verbatim 契约把"产生控制字节"押在模型的 JSON 转义行为上,
而模型落地形态不可控(JSON 转义 `\r` → 真 CR;防御性双转义 `\\r` → 字面两字符),
Claude Code 下回车不生效。修复:服务端解释最小转义集(见 send_in_session 节),
两种落地形态收敛为同一字节,契约对模型转义行为免疫;`\\` 逃逸保证可逆。
转义属 MCP 工具契约层(internal/mcp.expandSendInput),PtyConn.SendRaw 保持纯 verbatim。
