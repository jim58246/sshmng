# SSH 会话管理工具设计

> 本文档基于 `/Users/Zhuanz/Documents/EtucObsidianNotes/etuc/01. 采集/05. 软件设计/03. SSH会话统一管理工具/SSH会话管理工具.md` 的原始需求，经过讨论后形成的新版设计。
>
> 当前阶段（v1）：先实现客户端独立运行，使用一段时间后再迭代服务端与同步能力。服务端相关章节保留为"后续迭代"。

## 1. 原始需求

我所在的团队是 Linux 后台开发团队，经常需要登录到远程 Linux 进行部署、调测、运维。这些 Linux 机器随着我们部署新版本、新实例、删除旧版本、迁移等动作而动态变化，由多人共同维护。

这些 Linux 机器的登录方式也不尽相同：有的需要通过堡垒机登录，有的可以直连，有的用密码登录，有的用 key 登录，还有不同类型的代理。

希望开发一个软件，集中管理这些 Linux 机器，对外提供统一的界面和接口。

本软件功能集中在"管理"和"登录"两项能力上。提供导出到 Xshell 会话的能力。提供 AI Agent 集成的能力（提供 MCP）。

> 注：v1 阶段先实现 MCP 集成能力与配置/会话管理，Xshell 会话导出推迟到 v2。

## 2. 设计决策

### 2.1 客户端形态：stdio 单进程

MCP server 以 stdio 模式由 Agent（Claude Desktop / Cursor 等）拉起，一个 Agent 进程对应一个 MCP server 子进程。进程内维护 SSH 连接缓存。

**为什么不做跨进程连接共享：**

- 会话共享（复用同一 PTY/执行上下文）：有害，多 Agent 输出互相干扰
- 连接共享（复用同一 SSH TCP 连接，不同 channel）：收益小，复杂度高
- 配置共享（多 Agent 都能查到"这个 IP 怎么连"）：才是真需求，由配置存储解决，不需要连接复用

**进程内会话管理：**

- `map[sid]*session`，每个 sid 对应一个独立 SSH 连接
- `login(name)` 每次新建独立连接，支持同 name 多 session 并行
- idle timeout 可配置（`config.json` 的 `idle_timeout_s`，默认 300s），无活动自动断开（Agent 忘 `close_session` 的兜底）
- 命令执行期间不算空闲
- 跨 Agent 不共享

### 2.2 错误报告

**三类失败，三种处理：**

- **SSH auth 失败**：SSH 握手 / 认证 / 拨号 / host key 验证失败（如 "Permission denied (publickey)"、passphrase 错、"host key changed"、dial tcp refused）。错误信息直接放 `error` 字段，**不返回 trace**——错误已自解释，trace 反而稀释 Agent 注意力
- **LoginFlow 失败**：菜单导航中的 expect 未命中、单 Action 超时、MaxSteps / GlobalTimeoutMs 超限。失败原因藏在交互过程里（哪一步、发了什么、期望什么、收到什么），必须返回 trace 供诊断。且此时还没有 sid，Agent 无法事后调 `get_trace`，trace 只能随 `login` 失败响应携带
- **单命令执行失败**：`run_in_session` 返回 output/exit_code 即可，命令错误通常直接打印在 output（如 `ls: cannot access ...`），Agent 看 output 就能判断。仅在 output 不足以判断时（sentinel 未匹配、交互式命令卡住、需看 raw_output 诊断原始 PTY 字节），Agent 主动调 `get_trace(sid, last_n)`

**必要性**：MCP 工具由 Agent 非交互调用，失败时无人在场调试。但"无人在场"不等于"每次都喂全量 trace"——喂太多反而稀释 Agent 注意力。区分“失败在 SSH 层”、“失败在交互过程”、“失败在输出本身”：SSH 层错误自解释（Permission denied 等），仅 error 字符串；交互过程错误藏上下文，必须带 trace；输出本身错误自解释，按需取 trace。

**通用性**：多步骤 MCP 工具的基本素养，不只 SSH 管理器特有。部署、CI、云资源编排等工具都该区分这三类失败。

trace 的具体结构与异常处理策略见 3.2。

### 2.3 配置自愈

`update_ssh_server` 允许 Agent 读取失败信息（SSH auth 失败看 `error` 字符串，LoginFlow 失败看 `login_trace`，见 2.2）后修改 SSH 配置并重试登录，形成"失败 → 诊断 → 修复 → 重试"闭环。

**动机**：SSH 配置容易过时——

- IP 变化（机器迁移、新实例上线）
- 凭据轮换（密码过期、key 更新）
- 堡垒机菜单改版（选项编号或文案变化）

这些变化频繁且往往可预测，让人工逐条修很烦。

**风险**：

- Agent 可能改错配置，污染已知好配置
- 凭据写入有安全风险（覆盖正确密码）
- 配置变更模糊"人配的"和"Agent 改的"边界，影响审计

**v1 强制开启，无只读开关**。v2 计划加只读模式开关（仅暴露 `list_*` / `get_*`，不暴露 `update_*`），适用于配置变更必须留 review 痕迹的团队。

### 2.4 显式 session 管理

`login` / `run_in_session` / `close_session` 三件套，Agent 显式管理会话生命周期，连续多命令共享 cwd/env。

**为什么不用 exec_command**：访问环境本质是连续多命令（cd 到工作目录、export 环境变量、跑命令、看结果、再跑下一条）。exec_command 每次是新进程，cwd/env 不保留，对运维场景不自然。显式 session 把"一个登录会话"建模成有状态对象，更贴合实际使用。

会话管理的具体细节见 3.3（MCP 工具清单）与 3.7（会话生命周期）。

### 2.5 LoginAction：决策树模型

LoginAction 本质是 send+expect 行为组合：在 PTY 上发送命令、等待并判断结果。具体生效时机随所属资源与 Pattern 而异（详见 2.6）：`Jumphost.LoginFlow` 在 jumphost SSH auth 完成、jumphost shell 就绪后跑；`SSHServer.LoginFlow` 在 Pattern A 下于 target shell 就绪后跑，Pattern B 下于 jumphost 主菜单就绪后在 jumphost PTY 上跑（完成登录 target）。

**结构：** 一个 LoginAction 由一条 Send + 多个 Expects 组成，Expects 按顺序尝试匹配，命中的 Expect.Next 决定跳转到下一个 LoginAction，由此构成决策树。多个 Expects 用于应对同一场景下的多种可能输出（菜单变化、二次认证可能弹可能不弹、首次登录警告等）。

**入口与终止：**

- 入口：`Jumphost.LoginEntry`（或 `SSHServer.LoginEntry`）字段指向首个 Action 名
- 终止：当某 `Expect.Next == "success"` 时，登录成功
- `"success"` 是保留字符串，不能作为 LoginFlow 的 key

**Send / Expects 语义：**

- `Send` 可空：空时跳过发送，直接进入 expect 阶段。典型场景是入口 Action——SSH auth 完成、shell 就绪后远端先输出一段文字（MOTD / PS1 / 堡垒机菜单），我们等它输出完再行动
- `Expects` 暂不允许为空：必须至少一条 pattern，否则 Action 无法判定何时进入下一步

### 2.6 数据模型：三概念正交

把"传输代理"、"SSH 跳板"、"交互菜单"分开，避免字段语义随类型漂移：

- **Proxy** — 传输层代理（HTTP / SOCKS5），不含 SSH 跳板
- **Jumphost** — SSH 跳板，`SSHJ` 字段决定形态：`true` = 透明转发（ssh -J 语义），`false` = 交互式堡垒机。`SSHJ=false` 时 `LoginFlow` 仅负责把 jumphost 自身准备到"主菜单就绪"状态（如二次认证、协议确认、过 MOTD），**不**负责登录具体 target——jumphost 配置可被多 SSHServer 复用，无法预知引用者
- **SSHServer** — 目标机，引用 Jumphost 和 Proxy，本身有 `LoginFlow`：Pattern B（`Via.SSHJ=false`）下负责从 jumphost 主菜单登录到 target（选目标、输 target 凭据）；Pattern A（直连或 `Via.SSHJ=true`）下负责 target 认证完成后的交互（如 su / 角色选择 / PAM）

**为什么 `ssh -J` 归 Jumphost 而非 Proxy：**

`ssh -J` 是 SSH 协议层操作——客户端先对 jumphost 完成 SSH 认证（user / auth / host key 验证），再经 jumphost 的 `direct-tcpip` 通道拨 target。Proxy 是传输层代理（HTTP / SOCKS5），只转发 TCP 字节，不参与 SSH 认证，没有 user / auth / host key 概念。把 `ssh -J` 塞进 Proxy 会让 Proxy 携带 SSH 专有字段，破坏传输层语义纯净。

更关键的是，`ssh -J`（Pattern A）与交互式堡垒机（Pattern B）共享"客户端先 SSH auth 到 jumphost"这一步，差别只在 auth 后是否走菜单。两者归同一 Jumphost 实体、用 `SSHJ` 字段区分形态，SSH auth 与 host key 验证代码只写一份。

**两种形态的认证链路：**

- **Pattern A（`Jumphost.SSHJ = true`，透明转发，等价 ssh -J）**：客户端 SSH auth 到 jumphost（`Jumphost.Auth`）→ 客户端经 jumphost 的 direct-tcpip 通道 SSH auth 到 target（`SSHServer.Auth`）→ target shell 就绪 → `SSHServer.LoginFlow`（如有，承担 target 认证后交互如 su / 角色选择 / PAM）
- **Pattern B（`Jumphost.SSHJ = false`，交互式堡垒机菜单）**：客户端 SSH auth 到 jumphost（`Jumphost.Auth`）→ jumphost shell 就绪 → `Jumphost.LoginFlow` 走 jumphost 自身菜单到"主菜单就绪" → `SSHServer.LoginFlow` 接管，从主菜单选择/输入 target 地址、可能再走 target 凭据交互，最终拿到 target shell
- **Pattern B 下客户端不再做 SSH auth 到 target**（不走 direct-tcpip 通道）；`SSHServer.Auth` 必须为空，target 凭据直接写在 `SSHServer.LoginFlow.Send` 字符串中

多跳跳板机（A→B→目标）暂不支持（YAGNI），留 `Jumphost.Via *Jumphost` 递归口子扩展。

### 2.7 统一 PTY 模式

所有连接（直连、经由 Jumphost）统一走 PTY，不区分"直连 exec 模式"与"堡垒机 PTY 模式"。

**动机：**

- 有些环境登录后还需交互（su 切用户、角色选择、PAM session 模块），统一 PTY 让这类场景不特判
- 堡垒机场景本就是 PTY，统一后逻辑一致——堡垒机只是 PTY 的一个子类型（`SSHJ = false`）
- exec 模式下 cwd/env 不保留，对运维连续多命令场景不自然

**Jumphost 两种形态靠 `SSHJ` 字段区分：**

- `SSHJ = true` = 无菜单交互，等价于透明转发（ssh -J 语义）
- `SSHJ = false` = 交互式堡垒机，`Jumphost.LoginFlow` 准备到主菜单就绪，再由 `SSHServer.LoginFlow` 接管登录 target

**sftp 通道独立：**

- upload/download 走独立 sftp channel，与 PTY 命令执行通道分离
- login 时同步尝试建立 sftp 通道（5s 超时；部分堡垒机 / 受限环境不允许 sftp-subsystem）
- 不可用时 upload/download 报错 "sftp not available for this session"，`stat()` 返回 `sftp_available: false`

PTY 命令边界识别与终端规范化的实现细节见 3.7。

### 2.8 资源识别

用户与 Agent 交流时不会精确指出资源——有时用名字，有时用 IP（不带端口），有时用地域或服务名。Agent 在执行任何操作（`login` / `update_*` / `upload` 等）前，必须先把模糊引用解析为唯一 `name`。

**机制：**

- `name` 是路径字符串（如 `华东/order/order-01`），团队约定结构（维度自定，工具不强加）
- `tags` 是平 token 列表（如 `["prod", "v2.3", "主备"]`），承载正交维度；SSHServer / Jumphost / Proxy 三类资源均支持
- `list_ssh_servers` / `list_jumphosts` / `list_proxies` 均支持 `query?` 参数，跨 `name` / `addr` / `tags` 做子串模糊匹配（大小写不敏感），返回候选
- Agent 按候选数决策：1 个直接用；多个反问用户消歧；0 个反问确认（用户口误或资源不存在）

**先验知识在 tags：**

模型把"华东"翻译成"cn-east"靠的不是 LLM 世界知识（不可靠），而是 tags 里就写着"华东"（自然语言值）。tags 本身就是团队先验知识的沉淀。因此 **tag 值应使用团队日常用语**（如 `华东` 而非 `cn-east`，`生产` 而非 `prod`），这是配置规范，不是可选建议。

**path 改名=改身份：**

和 etcd 一样是路径固有代价，v1 接受。引用用全 path。团队若在意，可约定"最末段稳定，前缀可变"，但工具不强制。

## 3. 客户端设计

### 3.1 数据模型

```go
// 传输层代理（不含 SSH 跳板）
type ProxyType int
const (
    ProxyHTTP ProxyType = iota
    ProxySOCKS5
)

type Proxy struct {
    Type ProxyType
    Addr string       // host:port
    Auth *ProxyAuth   // 可选，代理自身认证
    Tags []string     // 可空，平 token 列表，自然语言值，见 2.8
}

type ProxyAuth struct {
    User     string
    Password string
}

// SSH 认证信息（复用于 Jumphost 和 SSHServer）
type SSHAuth struct {
    Password    string  // 明文密码，依赖 config.json 0600 权限保护
    PrivateKey  string  // 私钥文件完整路径（PEM 格式），不是内联内容
    Passphrase  string  // 私钥口令，可空（空 = 私钥未加密）
}

// 决策树节点
type LoginAction struct {
    Send      string             // 直接字符串，支持转义 \n \r \t；不支持变量引用（需要的字符如密码、用户名直接写）
    Expects   []Expect           // 按顺序尝试匹配
    TimeoutMs int                // 0 = 默认 10000
}

type Expect struct {
    Pattern string                // 无前缀 = glob，"re:" 前缀 = 正则
    Next    string                // 另一个 LoginFlow 的 key，或 "success" 表示成功
}

// SSH 跳板
type Jumphost struct {
    Name             string
    Addr             string                // host:port
    User             string
    Auth             SSHAuth
    SSHJ             bool                  // true = 透明转发（ssh -J 语义）；false = 交互式堡垒机菜单
    LoginFlow        map[string]LoginAction  // SSHJ=false 时必填，准备 jumphost 自身到主菜单就绪（如二次认证、协议确认），不负责登录 target；SSHJ=true 时必须为空
    LoginEntry       string                // SSHJ=false 时必填，LoginFlow 的起始 Action 名
    MaxSteps         int                   // 0 = 默认 50
    GlobalTimeoutMs  int                   // 0 = 默认 60000
    Via              *Jumphost             // 可选，多跳递归口子（v1 不实现）
    Proxy            *Proxy                // 可空，连接此 jumphost 走的传输代理
    Tags             []string              // 可空，平 token 列表，自然语言值，见 2.8
}

// 目标机
type SSHServer struct {
    Name             string
    Addr             string                // host:port
    User             string
    Auth             SSHAuth
    LoginFlow        map[string]LoginAction  // Pattern B（Via.SSHJ=false）下必填，从 jumphost 主菜单登录到 target；Pattern A（直连或 Via.SSHJ=true）下可选，承担 target 认证后交互（如 su/角色选择/PAM）
    LoginEntry       string                // LoginFlow 非空时必填，起始 Action 名
    MaxSteps         int
    GlobalTimeoutMs  int
    Via              *Jumphost             // 可空，空表示直连
    Proxy            *Proxy                // 可空，空表示不走传输代理
    Tags             []string              // 可空，平 token 列表（如 ["prod", "v2.3", "主备"]），自然语言值，见 2.8
}
```

**约定：**

- `"success"` 是保留字符串，不能作为任何 LoginFlow 的 key
- `LoginAction.Send` 是直接字符串，**不支持变量引用**——需要的字符（含密码、用户名、地址）直接写在 Send 中。避免字段名拼错、跨资源引用语义、空字段引用等出错点
- `LoginAction.Send` 为空时跳过发送，仅 expect（典型场景：入口 Action 等远端 MOTD / 菜单输出）
- `LoginAction.Expects` 至少一条 pattern，暂不允许为空
- Jumphost 形态校验（启动时强制，避免 `SSHJ` 与 `LoginFlow` 不一致导致行为静默漂移）：
  - `SSHJ = true`：`LoginFlow` 必须为空，`LoginEntry` 必须为空
  - `SSHJ = false`：`LoginFlow` 必须非空，`LoginEntry` 必须指向 `LoginFlow` 中存在的 Action
- SSHServer.LoginFlow 校验：
  - `Via.SSHJ = false`（Pattern B）：`LoginFlow` 必填，`LoginEntry` 必须指向存在的 Action，`Auth` 必须为空
  - `Via` 为空或 `Via.SSHJ = true`（Pattern A）：`LoginFlow` 可选，非空时 `LoginEntry` 必填且指向存在的 Action

**认证约定：**

- v1 仅支持 `Password` + `PrivateKey` 两种 SSH 认证方法；不支持 keyboard-interactive、SSH agent、SSH certificate、PAM auth phase、2FA（详见 3.5）
- `PrivateKey` 是文件完整路径，启动时校验文件权限为 0600（或更严），过宽则拒绝加载（见 3.5）
- 同时配置 `PrivateKey` 和 `Password` 时，**仅尝试 PrivateKey**；PrivateKey 失败不回退到 Password（避免掩盖 key 配置问题，配置自愈路径更清晰）
- `Jumphost.Auth` 必填；`SSHServer.Auth` 在 Pattern A（`Via` 为空或 `Via.SSHJ = true`）下必填（用于客户端 SSH auth 到 target），Pattern B（`Via.SSHJ = false`）下必须为空（客户端不再 SSH auth 到 target，凭据直接配在 `SSHServer.LoginFlow.Send` 字符串中）
- 认证失败（SSH auth 失败）不返回 trace，错误信息直接放 `error` 字段，见 3.2

### 3.2 LoginAction 失败与异常处理

**失败分类（详见 2.2）：**

- **SSH auth 失败**：SSH 握手 / 认证 / 拨号 / host key 验证失败。仅 `error` 字符串，**不返回 trace**——错误已自解释
- **LoginFlow 失败**：菜单导航中 expect 未命中、超时、MaxSteps 超限。返回 `error` + `login_trace`，trace 含多步骤上下文供 Agent 调 `update_*` 修复 pattern 后重试（见 2.3 配置自愈）

以下 trace 结构与异常处理策略仅适用 LoginFlow 失败。

**失败 trace 结构（`[]TraceEntry`）：**

```json
[
  {
    "time": "2026-07-17 14:23:45.000",
    "elapsed_ms": 0,
    "send": "",
    "expect": "Please select*",
    "output": "Welcome to Jumphost v2\nPlease select:\n  1) prod-db\n  2) prod-web\nYour choice: "
  },
  {
    "time": "2026-07-17 14:23:45.080",
    "elapsed_ms": 80,
    "send": "2\r",
    "expect": "",
    "output": "Connecting to prod-web...\nPermission denied (publickey)."
  }
]
```

模型看到第二条 entry 的 `expect` 为空，就知道没命中任何 pattern；看 `output` 是 "Permission denied (publickey)"，推断目标机认证方式变了。

**异常处理策略：**

- 单 Action 所有 Expects 都未命中：默认报错，trace 返回。不引入 Fallback 字段
- 单 Action 超时（TimeoutMs）：报错，trace 记录"等待期间累计输出"
- 全局循环保护：MaxSteps（默认 50）+ GlobalTimeoutMs（默认 60000），任一触发即报错
- 重试不在 LoginAction 层做，由模型通过 `error` / `login_trace` 诊断 + `update_ssh_server` 修复配置 + 重新 `login` 验证

### 3.3 MCP 工具清单

```text
========== 会话管理（核心）==========
login(name) → {sid?, ok, error?, login_trace?}
  - 走完整登录流程：Pattern B 下依次跑 `Jumphost.LoginFlow`（jumphost 到主菜单就绪）+ `SSHServer.LoginFlow`（主菜单到 target shell）；Pattern A 下仅跑 `SSHServer.LoginFlow`（如有）。返回 sid
  - 成功后同步建立 sftp 通道（5s 超时，不影响 login 成功与否，仅决定 upload/download 可用性）；建立失败时 stat() 返回 sftp_available=false
  - **Pattern B（`Via.SSHJ=false`）下不建立 sftp 通道**：SSH client 是到 jumphost 的，sftp 通道只会到 jumphost 而非 target，探测成功反而误导（用户以为能 upload 到 target，实际落到 jumphost）。故 `sftp_available` 恒为 false，upload/download 直接报 "sftp not available"
  - 失败分类见 3.2：
    - SSH auth 失败（含 host key 变更、网络拨号失败）：sid 为空，error 说明原因，**login_trace 为空**
    - LoginFlow 失败：sid 为空，error 说明原因，login_trace 供诊断
  - login_trace 结构与 get_trace 返回的一致

run_in_session(sid, cmd, timeout_ms?, max_output_bytes?) → {output, exit_code, timed_out?, truncated?, total_bytes?}
  - timeout_ms 默认 30s
  - max_output_bytes 默认 65536（64KB）；output 超过则保头截断，truncated=true，total_bytes 返回实际字节数
  - 命令结束：返回完整输出，timed_out=false，session 回到 idle
  - 超时处理（三段式，全自动，见 3.7 命令边界识别）：
    1) 超时后自动向 PTY 发 Ctrl-C (\x03) 中断远端命令
    2) drain 等 PS1-only sentinel（3s 超时）；drain 成功 → session 回 idle，Agent 可继续 run_in_session
    3) drain 超时（远端不响应 SIGINT，如 vim/REPL/管道阻塞）→ PtyConn 返回 connUnusable=true，Session 据此调 s.Close()，session 进入 closed，Agent 需重新 login
  - 返回值：output（清洗后，移除 sentinel/ANSI/PS1 残留）+ exit_code（命令退出码；drain 成功时通常 130=SIGINT；drain 超时时 -1 表示未提取到）
  - **不返回 raw_output**：原始 PTY 字节（含 ANSI/sentinel/\\r\\n）只存入 `CommandTrace.RawOutput`，不进 run_in_session 响应——避免大量噪声字节污染 Agent 上下文。需要诊断时调 get_trace(sid, last_n, 0) 取 raw_output
  - 截断时 Agent 应改用 `tail`/`head`/`grep` 或重定向到文件后 `download`
  - 失败不携带 trace；output 通常自解释（命令错误直接打印）。需诊断（sentinel 未匹配、交互卡住、看 raw_output 原始 PTY 字节）时调 get_trace(sid, last_n)
  - **硬超时约束**：MCP 客户端（Inspector / Claude Code）通常串行化工具调用，等 run_in_session 返回才发下一个请求。因此 run_in_session 必须有硬超时上限（30s cmd + 3s drain = 33s），不能无限阻塞——否则 Agent 无法调 get_trace 或 close_session 诊断。drain 超时强制 Close 正是为此设计

**不提供 send_input / send_special**：MCP 客户端串行化工具调用，run_in_session 执行中调不到这两个工具；命令结束（正常退出或超时 Ctrl-C drain 后）session 已回 idle 或 closed，再调也报错。交互式命令（sudo/read/cat>file）靠 run_in_session 自身超时 + get_trace 看 raw_output 诊断，不靠 send_input 喂入。Ctrl-C 由 run_in_session 超时后自动发送，不需要外部工具触发。

close_session(sid) → {ok}
  - 强制关闭，无论 session 状态
  - trace 保留 10 分钟供事后诊断

get_trace(sid, last_n?, trunc_output=200) → [TraceEntry]
  - last_n: 最近 N 轮，省略时全返回
  - trunc_output: output 截断长度，默认 200，0 不截断

========== 文件传输 ==========
upload(sid, src, dst, timeout_ms?) → {ok, bytes, err, timed_out?}
  - 走 sftp 独立通道（与 PTY 命令执行通道分离）
  - src=本地路径，dst=远端路径
  - timeout_ms 默认 300s；超时返回已传输字节 bytes，timed_out=true
  - sftp 通道不可用时 err="sftp not available for this session"
download(sid, src, dst, timeout_ms?) → {ok, bytes, err, timed_out?}
  - 同上；src=远端路径，dst=本地路径
upload_dir(sid, src, dst, conflict?, concurrency?, timeout_ms?) → {sid, ok, bytes, files, skipped, renamed, timed_out, err?}
  - 把本地 src 目录整树上传到远端 dst
  - 走 sftp 通道（与 upload 单文件一致），filepath.Walk 遍历 + MkdirAll 建目录 + 并发 worker pool 传文件
  - conflict policy：overwrite（默认，sftp.Create 语义）/ skip（跳过已存在）/ rename（自动重命名 name_1、name_2...）
  - concurrency 默认 4，0 = 默认 4
  - timeout_ms 默认 300s，per-file 超时
  - per-file 错误不中断整树传输，继续其他文件；err 字段聚合所有错误（errors.Join）
  - 返回 bytes（成功传输字节总数）/ files（成功文件数）/ skipped（跳过数）/ renamed（重命名数）/ timed_out（per-file 超时数）
  - sftp 通道不可用时 err="sftp not available for this session"
download_dir(sid, src, dst, conflict?, concurrency?, timeout_ms?) → 同上，方向相反
  - 把远端 src 目录整树下载到本地 dst
  - 走 sftp 通道，sftpClient.Walk 遍历 + os.MkdirAll 建目录 + 并发 worker pool 传文件
  - 其余语义同 upload_dir

**为什么加 upload_dir/download_dir：** 单文件 upload 强迫 Agent 用 N 次 MCP 往返编排文件夹传输——每个文件一次 SSH channel 操作，慢且容易半途失败留脏状态。upload_dir 把 Walk + MkdirAll + 并发 + conflict policy 封装成一次调用，Agent 编排复杂度降下来，trace 也集中在一条调用里便于诊断。sftp 协议本身没有"递归传输"原语，专业 sftp 工具都是这样自己实现的。

========== 配置查询/更新 ==========
list_ssh_servers(query?) → [{name, addr, via, proxy, tags}]
  - query 可选：跨 name / addr / tags 子串模糊匹配（大小写不敏感）
  - 省略时返回全部
  - Agent 按候选数决策：1 个直接用、多个反问消歧、0 个反问确认，见 2.8
get_ssh_server(name) → SSHServer
update_ssh_server(name, patch) → {ok, err}
  - JSON Merge Patch (RFC 7396)
  - 字段非 null：设置/替换；null：删除
  - struct 递归合并；map 按 key 合并；array 整体替换
  - name 不存在：创建新 server；patch=null：删除整个 server
  - 不可用于重置 host key（见 3.5 安全模型）

list_jumphosts(query?) → [{name, addr, via, proxy, tags}]
  - query 可选：跨 name / addr / tags 子串模糊匹配（大小写不敏感）
  - 省略时返回全部
get_jumphost(name) → Jumphost
update_jumphost(name, patch) → {ok, err}
  - 语义同 update_ssh_server，key 为 name
  - 不可用于重置 host key（见 3.5 安全模型）

list_proxies(query?) → [{name, type, addr, tags}]
  - query 可选：跨 name / addr / tags 子串模糊匹配（大小写不敏感）
  - 省略时返回全部
get_proxy(name) → Proxy
update_proxy(name, patch) → {ok, err}
  - 语义同 update_ssh_server，key 为 name

**为什么不统一成一个 update 工具：**

候选 `update(type, name, patch)` 看似更简洁，但 patch 的字段集随 type 变（Proxy ⊂ Jumphost ⊂ SSHServer）。MCP 工具的 input schema 无法干净表达 discriminated union——要么 patch 退化成 generic object 失去字段校验，要么用 oneOf 但 MCP 客户端支持参差。三个独立工具各自有明确 schema，Agent 构造 patch 时能直接看到允许的字段。MCP 工具数量不是瓶颈，schema 清晰度才是。

**update_* 对已有 session 的影响**：仅修改 config，不触碰已建立的 session。已有 session 用旧配置继续跑直到 close_session 或 idle 超时；新配置对下次 `login` 生效。

========== 状态查询 ==========
stat() → [{sid, name, state, sftp_available, last_activity, commands_run, uptime_s}]
  - state: "idle" | "running" | "closed"
  - sftp_available: bool，login 时同步尝试建立 sftp 通道的结果
  - idle: 可 run_in_session
  - running: 命令执行中，run_in_session 报错 "session busy"
  - closed: session 已关闭（close_session 调用、idle timeout 触发、或 run_in_session drain 超时强制 Close）；trace 保留 10 分钟供 get_trace 诊断
```

**Trace 结构（两种，分开存储）：**

command 阶段与 LoginFlow 阶段的 trace 结构不同，分开存储：

```go
// command 阶段（run_in_session 的每条命令）
type CommandTrace struct {
    Time      time.Time `json:"time"`             // 命令开始时间
    Cmd       string    `json:"cmd"`              // 发送的命令
    Output    string    `json:"output"`           // 清洗后输出（移除 sentinel/ANSI/PS1 残留）
    RawOutput string    `json:"raw_output"`       // 原始 PTY 字节（含 ANSI/sentinel/\\r\\n），供诊断
    ExitCode  int       `json:"exit_code"`        // 命令退出码；-1 表示未提取到
    TimedOut  bool      `json:"timed_out"`        // 是否超时
    CtrlCSent bool      `json:"ctrl_c_sent"`      // 超时后是否发了 Ctrl-C（诊断"为何 session 进 closed"用）
}

// LoginFlow 阶段（login 时的每步交互）
type TraceEntry struct {
    Time      string    // 本地时间字符串，格式 "2006-01-02 15:04:05.000"
    ElapsedMs int64     // 距 session 起点的毫秒数
    Send      string    // LoginAction.Send 原文（含凭据）
    Expect    string    // 命中的 pattern（失败时为空）
    Output    string    // 累计 PTY 输出（含 ANSI，按 trunc_output 截断）
}
```

**为什么分两种**：command 阶段有 exit_code / timed_out / ctrl_c_sent 等 command 专属字段；LoginFlow 阶段有 elapsed_ms / expect 等 flow 专属字段。强行统一会留空字段，JSON 噪声大。MCP 工具返回时按阶段选结构：`get_trace` 返回 `[]CommandTrace`，`login` 失败时 `login_trace` 返回 `[]TraceEntry`。

**raw_output 用途**：`CommandTrace.RawOutput` 保留 PTY 原始字节，诊断 sentinel 未匹配、ANSI 异常、字符编码问题等。`get_trace` 的 `trunc_output` 参数同时截断 `Output` 和 `RawOutput`。

**currentTrace 不返回**：`get_trace` 只返回已完成的命令 trace（`s.traces`）。正在 running 的命令 trace（`currentTrace`）不返回——因为 MCP 客户端串行化工具调用，run_in_session 卡住时根本调不到 get_trace（见 run_in_session 硬超时约束）。诊断卡死靠 run_in_session 自身的硬超时 + raw_output，不靠 get_trace 看现场。

**Trace 敏感数据**：`CommandTrace.Cmd`、`TraceEntry.Send` / `Output` 都可能含密码等敏感数据——`LoginAction.Send` 字段直接写凭据会原样进 trace。Trace 仅存内存，`close_session` 后保留 10 分钟自动清理，不落盘。

**关键决策：**

1. 不提供 `exec_command` —— 访问环境本质是连续多命令，显式 session 更自然
2. 不提供 `raw_*` 系列 —— trace 足够诊断，配置修复后重新 login 验证
3. `update_ssh_server` / `update_jumphost` / `update_proxy` 三个独立工具（不统一成一个 `update`），各自有明确 schema；均用 JSON Merge Patch，patch=null 删除整个实体，key 不存在则创建
4. `upload`/`download` 走 sftp 独立通道，复用 sid 对应的会话连接；sftp 通道在 login 时同步建立（5s 超时），不可用时 upload/download 报错
5. `run_in_session` 超时后自动三段式处理（Ctrl-C → drain → 强制 Close），不需要外部工具触发中断；Ctrl-C 由 run_in_session 内部发送
6. 交互式命令（sudo/read/cat>file）靠 `run_in_session` 自身超时 + `get_trace` 看 raw_output 诊断，不提供 `send_input` / `send_special`——MCP 客户端串行化工具调用，run_in_session 执行中调不到这两个工具，命令结束后再调也已 idle/closed
7. 统一 PTY 模式 —— 所有连接（含直连）走 PTY 维持 cwd/env，支持登录后交互（su/角色切换）；堡垒机是 PTY 模式的子类型（SSHJ=false），不单独建模
8. Jumphost 两种形态靠 `SSHJ` 字段区分 —— `true` = 透明转发（ssh -J 语义），`false` = 交互式堡垒机（`LoginFlow` 仅准备 jumphost 到主菜单就绪，登录 target 由 `SSHServer.LoginFlow` 接管）
9. Host key 验证用 TOFU —— 首次连接记录 key，后续验证；key 变更报错且 `update_*` 不能重置（安全决策需人工确认），见 3.5
10. 终端规范化统一在 target shell 就绪后一次性注入 —— LoginFlow 阶段不强制规范化（堡垒机菜单可能不支持 `TERM=dumb`/`PS1` 等设置，且交互式 prompt 不应有命令边界概念），靠 expect 前的 ANSI 过滤兜底；target shell 就绪后注入完整 RC（TERM/NO_COLOR/LANG/stty/PS1/history），见 3.7
11. 操作前先识别 —— Agent 执行 `login`/`update_*` 等操作前，先用 `list_*(query)` 把模糊引用（IP/地域/服务名）解析为唯一 name；候选 1 个直接用，多个反问消歧，0 个反问确认。三类资源（SSHServer / Jumphost / Proxy）均支持 `name` 路径 + `tags` 平 token 列表，tag 值用自然语言。详见 2.8
12. **Server Instructions** —— `NewServer` 通过 `mcp.ServerOptions.Instructions` 把 `serverInstructions` 常量传给 MCP server，client（Agent）在 initialize 响应里收到完整文本。内容覆盖 Entity model / Workflow / Session semantics / Session lifecycle / Failure recovery 五段，单靠 tool description 拼凑不出来的关键约束（"session 间互不干扰 + session 内状态延续"、"失败时调 get_trace"、"idle timeout"、"Pattern B 不支持 sftp"、"NOPASSWD 优先"）都在这里。缺失时 Agent 容易漏掉这些约束，靠 trial-and-error 学习。**注意**：Claude Code 对 server instructions 有 2KB 截断，当前文本压在 1.9KB 以下；且压缩后 Instructions 不保证重新注入（不在 "What survives compaction" 列表里），关键信息也散落在各 tool description 里兜底。

### 3.4 本地存储格式

**配置目录：**

- 环境变量 `SSHMNG_HOME` 指定配置目录，默认 `~/.sshmng/`
- 配置文件：`$SSHMNG_HOME/config.json`

**文件结构（单文件，Jumphost/Proxy 独立引用）：**

下方示例展示典型 Pattern B 场景：jumphost `ssh_j=false`，其 `login_flow` 负责把 jumphost 自身准备到主菜单就绪；server `via` 引用该 jumphost，自己的 `login_flow` 从主菜单登录到 target，`auth` 必须为 `null`（凭据直接写在 `login_flow.Send` 中）。

```json
{
  "version": "1",
  "idle_timeout_s": 300,
  "log_level": "info",
  "log_path": "~/.sshmng",
  "jumphosts": [
    {
      "name": "华东/jumphost-prod",
      "addr": "10.0.0.254:22",
      "user": "ops",
      "auth": {...},
      "ssh_j": false,
      "login_flow": {...},
      "login_entry": "...",
      "max_steps": 0,
      "global_timeout_ms": 0,
      "proxy": null,
      "tags": ["生产", "华东"]
    }
  ],
  "proxies": [
    {
      "name": "corp-socks5",
      "type": "SOCKS5",
      "addr": "socks.corp:1080",
      "auth": null,
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
      "login_flow": {...},
      "login_entry": "...",
      "max_steps": 0,
      "global_timeout_ms": 0,
      "tags": ["生产", "v2.3", "主备"]
    }
  ]
}
```

**存储与内存模型映射：**

- `SSHServer.Via` / `SSHServer.Proxy` 在 struct 里是指针（`*Jumphost` / `*Proxy`），JSON 存储时序列化为名字引用（字符串），加载时按名字解析回指针
- `Jumphost.Via` / `Jumphost.Proxy` 同理

**引用完整性：**

- `servers[i].via` 引用 `jumphosts` 里的 `name`
- `servers[i].proxy` 引用 `proxies` 里的 `name`
- `jumphosts[i].proxy` 引用 `proxies` 里的 `name`
- 删除 Jumphost/Proxy 前检查引用，有引用则报错

**文件读写：**

- 客户端启动时加载 `config.json` 到内存
- `update_ssh_server` 等写操作：修改内存 + 持久化到文件
- 持久化采用原子写（写临时文件 + rename），避免写一半崩溃
- Windows fallback：rename 因目标文件被外部进程占用失败时，降级为直接 truncate + write（非原子，但只需 FILE_SHARE_WRITE，兼容编辑器打开场景）
- 文件权限 `0600`

### 3.5 安全模型

**v1 阶段（客户端独立运行）：**

- 敏感字段（Password / Passphrase）明文存储在 `config.json`；`PrivateKey` 存的是私钥文件路径，私钥内容不在 config.json 中
- config.json 文件权限 `0600`，仅属主可读写
- 启动时检查 config.json 文件权限，若权限过宽（如 0644）拒绝加载并报错
- 创建 config.json 时立即 `chmod 0600`
- 启动时校验 `PrivateKey` 路径指向的私钥文件权限为 0600（或更严），过宽则拒绝加载
- Windows 平台跳过上述 Unix 权限检查（含 config.json / 私钥文件 / known_hosts）：NTFS 用 ACL 而非 Unix rwx 模型，`os.FileMode.Perm()` 的 group/other 位恒为 0，检查形同虚设。Windows 上由 NTFS ACL 负责文件访问控制（标准做法），启动时打一条 Info 日志提示用户确保 ACL 限制访问
- 文档明确警告：明文存储，请确保机器本身安全

**为什么 v1 不加密：**

- B 方案（主密码派生密钥）在 MCP 场景下尴尬：Agent 拉起 MCP server 是非交互的，主密码传递只能靠环境变量或文件，与明文差别不大
- C 方案（系统密钥环）跨平台复杂，且 MCP 子进程可能访问不到用户会话密钥环（SSH 远程场景）
- 真要高安全，用户可自行用 `age` / `gpg` 加密整个 `config.json`，使用前解密

**Host key 验证（TOFU）：**

- 首次连接某 host 时，记录其公钥到 `$SSHMNG_HOME/known_hosts`（文件权限 `0600`）
- 后续连接验证公钥匹配；不匹配则拒绝连接，`login` 失败（属 SSH auth 失败，见 2.2）
- `error` 字段中明确标注 "host key changed, possible MITM"，便于 Agent 排查
- Agent 无法通过 `update_*` 工具重置已知 key——key 变更是安全决策，必须人工确认
- 人工重置方式：编辑 `$SSHMNG_HOME/known_hosts` 删除对应条目

**为什么不用 InsecureIgnoreHostKey：**

Agent 驱动的连接无人在场确认，不验证等于默认接受 MITM。内网环境也非绝对安全（ARP 欺骗、VLAN 跳跃、被攻陷的堡垒机）。

**为什么不用预置 known_hosts 严格模式：**

配置成本高，每个新机器都要先手动采集 key。TOFU 是安全性与易用性的平衡，与 OpenSSH 客户端默认行为一致。

**认证范围（v1）：**

- 仅支持 `Password` + `PrivateKey`（PEM 文件路径）两种 SSH 认证方法
- 不支持 keyboard-interactive——server/bastion 端 2FA 不在 v1 范围
- 不支持 SSH agent、SSH certificate
- PAM 认证模块（auth phase）不在 v1 范围；PAM session 模块（post-auth shell 层交互）通过 LoginFlow 表达
- 若环境强制要求上述能力，需 v2 扩展或在 LoginFlow 中硬编码交互

**Trace 敏感数据：**

- Trace 的 `Send`（LoginFlow 阶段含 `LoginAction.Send` 原文）、`Output`（PTY 原始流）都可能含密码等敏感数据——去掉变量引用后，凭据直接写在 `LoginAction.Send` 字符串中，会原样进 trace
- Trace 仅存内存，`close_session` 后保留 10 分钟自动清理，不落盘
- 同进程内 `get_trace` 需要 sid，跨进程不可见
- 若不可接受密码进 trace，优先用 NOPASSWD sudo / PrivateKey 认证避免在 LoginFlow 中明文写密码

**v2 阶段（服务端）：**

- gRPC over TLS 传输加密
- 服务端存储加密（方案待定）
- 多用户认证与权限（方案待定）

### 3.6 .xsh 导入导出（后续迭代）

v1 阶段不实现。核心功能（MCP + 配置管理 + 会话管理）稳定后再考虑。

**待讨论（v2）：**

- `xshell_path` 参数是目录还是文件
- Xshell 密码加密处理（导入无法解密、导出留空）
- Jumphost 引用导出策略（单独 `.xsh` / 内嵌 / 跳过）
- 字段映射表

### 3.7 会话生命周期与终端规范化

**会话管理：**

- 进程内 `map[sid]*session`，每个 sid 对应一个独立 SSH 连接
- `login(name)` 每次新建独立连接，支持同 name 多 session 并行
- idle timeout 可配置（`config.json` 的 `idle_timeout_s`，默认 300s），无活动自动断开（Agent 忘 `close_session` 的兜底）
- `close_session(sid)` 显式关闭，trace 保留 10 分钟
- 命令执行期间不算空闲
- 跨 Agent 不共享（stdio 单进程模型决定）
- 所有工具调用并发安全：session map 与 config 各自持锁；同一 session 的 `run_in_session` 靠 state=running 串行化（"session busy" 报错）

**Session 状态机：**

- `idle` — 空闲，可 `run_in_session`
- `running` — 命令执行中，`run_in_session` 报错 "session busy"
- `closed` — 已关闭（`close_session` 调用、idle timeout 触发、或 run_in_session drain 超时强制 Close）；所有操作报错 "session closed"，trace 保留 10 分钟供 `get_trace` 诊断
- `run_in_session` 超时后自动三段式处理（Ctrl-C → drain → 强制 Close），见 3.7 命令边界识别
- `close_session` 无论状态都强制关闭

**为什么 closed 是独立状态而非直接从 map 删除**：trace 保留 10 分钟供事后 `get_trace` 诊断。closed 态阻止任何后续操作（run_in_session 均报错），但 `get_trace` 仍可查（从 Manager.graveyard 取）。

**终端规范化（target shell 就绪后一次性注入）：**

`run_in_session` 对所有会话（含直连）走 PTY 以维持 cwd/env。规范化统一在 LoginFlow 完成、target shell 就绪后一次性注入，**不在 LoginFlow 阶段做**。理由：

- 部分堡垒机不是传统 SSH 终端，有自己的菜单界面，不接受 `TERM=dumb` / `PS1` 等设置，强行规范化可能引起错误
- LoginFlow 是原始 send&expect，灵活性高——弹密码、菜单选择、二次认证等交互式 prompt 不会因"命令未结束"卡住（规范化后每条命令有 sentinel 结束标志，弹密码时该标志尚未出现）
- expect 匹配前对 output 做 ANSI 过滤兜底（见下），pattern 按纯字符匹配，不依赖规范化消除噪声

**LoginFlow 阶段的 expect 匹配（ANSI 过滤兜底）：**

- PTY 分配时用默认 TERM（如 `xterm`），不强制 `dumb`，让堡垒机菜单自然显示
- LoginFlow 输出可能含 ANSI 转义、颜色码、光标控制序列
- expect 匹配前对累计 output 做剥离：`regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")` 移除 CSI 序列后再做 pattern 匹配
- pattern 按纯字符匹配，不依赖规范化

**target shell 就绪后注入 RC（一次性）：**

- `TERM=dumb`：禁用颜色码和光标控制序列
- `NO_COLOR=1`：覆盖只查 `isatty` 不查 TERM 的工具
- `LANG=C.UTF-8`（C.UTF-8 在精简系统/容器也普遍可用，避免 en_US.UTF-8 缺失时的 locale 警告）
- `stty cols 120 rows 100`：终端宽度高度（PTY 分配时的默认值不强制）
- `PS1` 覆盖为 sentinel（bash/zsh 用 `$(echo _$?)__<sid>___]# ` 捕获 exit code；dash/ash 用 `__P_<sid>__> `）
- `stty -echo`：关闭输入回显
- `set +o history`（bash）/ `unset HISTFILE`（zsh）：不污染远端历史文件

> 注：旧设计用 `PROMPT_COMMAND`（bash）/ `precmd_functions`（zsh）在每条命令后发射 exit sentinel。但审计机器会把 `PROMPT_COMMAND` 设为只读，破坏注入路径。新设计放弃 PROMPT_COMMAND 包装器，改用 PS1 的 `$(echo _$?)` 命令替换——bash 在 prompt 展开时求值 `$?` 拿到上一条命令的退出码，zsh 需 `setopt PROMPT_SUBST` 才会展开 PS1 里的 `$(...)`。dash/ash 不展开 PS1 中的 `$(...)`，退化为固定 PS1（无 exit code）。

**注入流程时序：**

1. SSH 连接建立：
   - 直连：直接 SSH 拨号到 target，PTY 分配（默认 TERM，不强制规范化）
   - Pattern A：先 SSH 拨号到 jumphost，经 jumphost 的 direct-tcpip 通道（`jumpClient.Dial`）SSH 拨号到 target，PTY 在 target 上分配
   - Pattern B：SSH 拨号到 jumphost，PTY 在 jumphost 上分配（不拨号到 target）
2. Pattern B 下：走 `Jumphost.LoginFlow`，准备到 jumphost 主菜单就绪（expect 匹配前做 ANSI 过滤）
3. 走 `SSHServer.LoginFlow`：Pattern B 下从主菜单登录到 target shell；Pattern A 下完成 SSH auth 到 target 后承担 target 认证后交互（如有，同样做 ANSI 过滤）；直连下同 Pattern A
4. target shell 就绪
5. 发送 shell 探测命令（见"Shell 类型探测与降级"），确定 shell 类型
6. 根据 shell 类型注入完整 RC（TERM/NO_COLOR/LANG/stty/PS1/history）
7. 等待首个 PS1 sentinel 出现，确认注入成功
8. session 转入 `idle`，可 `run_in_session`

**命令边界识别（PS1-only sentinel + token 化）：**

PTY 模式下命令输出和 shell 提示符混在一起。通过覆盖 PS1，让 shell 在每次 prompt 展开时主动发射已知 sentinel，无需解析用户原有 PS1，也无需依赖 PROMPT_COMMAND / precmd hook（审计机器会把 `PROMPT_COMMAND` 设为只读，破坏 hook 注入路径）。

**Sentinel 格式（bash / zsh，PS1-only + token 化）：**

- 单一 sentinel：`_<exit_code>__<sid>_<token>__]# ` — prompt 展开时由 `$(echo _$?)` 拼出 exit code，`<sid>` 和 `<token>` 直接嵌在 PS1 字面量里
- PS1 字符串：`'$(echo _$?)__<sid>_<token>__]# '`（bash）/ 同左（zsh 需 `setopt PROMPT_SUBST` 才展开 `$(...)`）
- 初始 PS1（injectRC 等的就绪标记，token 为空）：`'$(echo _$?)__<sid>___]# '`，展开后 `_<rc>__<sid>___]# `

`<sid>` 是 session 级 8 字节十六进制随机串（如 `a3f2b1c9`），登录时生成一次，整个会话不变。`<token>` 是 Run 级 8 字节十六进制随机串，**每次 `run_in_session` 生成一次**，整个 Run 不变。token 直接嵌入 PS1 字面量，不依赖 shell 变量——setup 命令重新 `export PS1=...` 把新 token 写死进 PS1。

**Sentinel 格式（dash / ash，不 token 化）：**

- PS1 sentinel：`__P_<sid>__> ` — 固定，无 exit code
- dash / ash 不展开 PS1 中的 `$(...)`，无法用 `$(echo _$?)` 捕获 exit code，退化为仅边界识别

**为什么 bash/zsh 用 PS1-only 而非 PROMPT_COMMAND：**

审计机器 / 加固 shell 会把 `PROMPT_COMMAND` 设为只读（`readonly PROMPT_COMMAND`），任何 `PROMPT_COMMAND=...` 赋值都会报错，导致 RC 注入失败。PS1 没有此限制——覆盖 PS1 是 shell 原生行为，不触发 readonly。`$(echo _$?)` 在 prompt 展开时求值 `$?`，拿到的是上一条命令的退出码（prompt 展开发生在命令结束后、下一条命令前），等价于 PROMPT_COMMAND 里的 `$?` 捕获。zsh 需 `setopt PROMPT_SUBST` 让 PS1 里的 `$(...)` 被展开（默认 zsh 不展开 PS1 中的命令替换）。

**为什么 bash/zsh 要 token 化：**

单一 sentinel 仍可能被命令输出里的字面量误匹配（如 `echo $PS1` / `cat ~/.bashrc` 含 PS1 字面量 / `typeset -p PS1`）。token 化彻底封死：每次 Run 的 token 是运行时随机生成并直接写进 setup 命令的 PS1 字面量，命令输出不可能预知 token，字面量误匹配概率为零。

**为什么 dash/ash 不 token 化：** 这些 shell 不展开 PS1 中的 `$(...)`，无法在 prompt 时动态注入 token，也无法捕获 exit code。保持固定 PS1 sentinel，可能误匹配 PS1 字面量，但概率低且这些 shell 少见。

**为什么用单一 sentinel 而非 exit+PS1 组合（历史动机）：**

旧设计用 PROMPT_COMMAND 发射 `__E_<sid>_<token>__:<exit>__` exit sentinel，PS1 发射 `__P_<sid>_<token>__> ` 边界 sentinel，Run 等两行组合 `__E_...__\r\n__P_...__> `。组合降低了误匹配概率，但没杜绝——命令输出含完整组合字面量仍会误匹配。token 化后组合已不必要：单一 sentinel `_<rc>__<sid>_<token>__]# ` 含 token 已足封死字面量误匹配，且 PS1-only 不依赖 PROMPT_COMMAND，绕过 readonly 限制。组合方案被废弃。

**注入 RC（bash）：**

```sh
export TERM=dumb
export NO_COLOR=1
export LANG=C.UTF-8
stty cols 120 rows 100 2>/dev/null
set +o history
stty -echo 2>/dev/null
export PS1='$(echo _$?)__<sid>___]# '
```

**关键约束（bash RC）：**

1. `export PS1=` 必须放最后一行，且**初始无 token**（`__<sid>___]# `，token 段为空）。真实 bash 交互模式逐行执行 RC，每行执行完都会显示 PS1。若 PS1 在中间，injectRC 等首个 sentinel 时会在该行后立刻匹配，但后续行还没执行，残留 prompt 进 stdoutCh 被下次 Run 误消费。初始无 token 是因为首次 Run 的 setup 命令才会把 token 嵌进 PS1。
2. PS1 里的 `$(echo _$?)` 在 prompt 展开时求值。bash 在每条命令结束后、显示下一条 prompt 前展开 PS1，此时 `$?` 是上一条命令的退出码。`echo _$?` 输出 `_<rc>`，拼上 `__<sid>_<token>__]# ` 形成完整 sentinel。
3. 不使用 `PROMPT_COMMAND` / `__sshmng_precmd` / `__sshmng_rc` / `__sshmng_tok` 等变量——审计机器 `readonly PROMPT_COMMAND` 会让这些路径失败。PS1 是 shell 原生机制，无 readonly 限制。

**注入 RC（zsh）：**

```sh
export TERM=dumb
export NO_COLOR=1
export LANG=C.UTF-8
stty cols 120 rows 100 2>/dev/null
setopt PROMPT_SUBST
unset HISTFILE
stty -echo 2>/dev/null
export PS1='$(echo _$?)__<sid>___]# '
```

**关键约束（zsh RC）：**

1. `setopt PROMPT_SUBST` 必须在 `export PS1=` 之前。zsh 默认不展开 PS1 中的 `$(...)` 命令替换，`PROMPT_SUBST` 开启后才展开，否则 PS1 字面量 `$()` 原样显示。
2. PS1 初始无 token（`__<sid>___]# `），同 bash：首次 Run 的 setup 命令把 token 嵌进 PS1。
3. 不使用 `precmd_functions` / `_sshmng_precmd` / `__sshmng_rc` / `__sshmng_tok`——PS1-only 设计不需要 hook。

**每次 Run 的 setup 命令（bash/zsh）：**

```
PS1='$(echo _$?)__<sid>_<token>__]# '
```

`<token>` 是本次 Run 生成的 8 字节十六进制随机串。setup 命令直接把 token 写死进 PS1 字面量，不依赖 shell 变量——后续每次 prompt 展开都含这个 token。Run 流程：

1. 生成 token
2. 写 setup 命令（重新 export PS1，把 token 嵌进字面量）
3. 等精确 `<token>` 的 sentinel（消费 pushback + stdoutCh 里的旧残留 + setup sentinel）
4. 清空 pushback（丢弃 setup sentinel 后的任何残留）
5. 写 `<cmd>\n`
6. 等精确 `<token>` 的 sentinel（cmd 的 sentinel）；超时 → Ctrl-C → drain 等同；drain 超时 → 返回 connUnusable=true，Session 决定 Close

**输出流时序（bash/zsh，含 token）：**

```
Agent 发送: PS1='$(echo _$?)__<sid>_<token>__]# '\n
           (setup sentinel: _0__<sid>_<token>__]# )
           → 匹配后清空 pushback
Agent 发送: ls /tmp\n
           shell 执行 ls，输出 file1\r\nfile2\r\n
           ls 退出，$? = 0
           shell 展开 PS1: $(echo _$?) → _0，拼上 __<sid>_<token>__]#
Agent 读到: file1\r\nfile2\r\n_0__<sid>_<token>__]# 
           (匹配 sentinel _0__<sid>_<token>__]# )
```

`<token>` 是本次 Run 生成的一次性随机串，命令输出无法预知，故命令输出里的字面量不会误匹配。

**多行命令场景（cmd1; cmd2; cmd3）：**

shell 依次执行 cmd1/cmd2/cmd3，每条命令结束后展开 PS1 发射一个 sentinel。Run 步骤 6 的正则 `_-?\d+__<sid>_<token>__]# ` 匹配第一个 sentinel（cmd1 的），Run 返回 cmd1 的输出 + exit code。cmd2/cmd3 的 sentinel 含相同 token，但步骤 6 匹配第一个后立即返回，后续 sentinel 作为 trailing 进 pushback。下次 Run 的步骤 3（等精确 `<token>` 的 sentinel）会用**新的** token 正则，旧 token 的 sentinel 不匹配，作为非匹配数据被消耗掉。多行命令的行为：返回第一条命令的结果，后续命令的结果在下次 Run 的步骤 3 被丢弃。

**解析逻辑：**

1. 写 setup 命令（`PS1='$(echo _$?)__<sid>_<token>__]# '`），等精确 `<token>` 的 sentinel，清空 pushback
2. 写命令到 stdin，读 PTY 流直到匹配 sentinel `_-?\d+__<sid>_<token>__]# `（正则，exit code 是变量，token 是本次 Run 的）
3. sentinel 之后的 trailing data 存入 pushback，下次 Run 优先消费
4. 从流中匹配 `_(-?\d+)__<sid>_<token>__]# ` 提取 exit code（多 sentinel 取最后一个）
5. 清洗输出：移除 sentinel 残留、剥离 ANSI 转义
6. 超时未匹配 sentinel → 进入三段式超时处理（见下）

**超时处理（三段式，全自动）：**

```
1) cmd timeout (默认 30s) 触发 → 发 Ctrl-C (\x03) 到 stdin
2) drain 等 PS1-only sentinel (ctrlCDrainTimeout = 3s)
   - drain 成功：远端响应 SIGINT，命令退出，新 PS1 出现
     → Run 返回 timedOut=true, exit_code=130 (SIGINT), connUnusable=false
     → session 回 idle，Agent 可继续 run_in_session
3) drain 超时（远端不响应 SIGINT，如 vim / REPL / 管道阻塞）
   → Run 返回 timedOut=true, exit_code=-1 (未提取到), connUnusable=true
   → Session 收到 connUnusable=true 调 s.Close()，session 进入 closed
   → Agent 需重新 login
```

**为什么 drain 超时返回 connUnusable 而非 PtyConn 自己 Close：**

分层原则：close 决策在状态机层（Session），不在传输层（PtyConn）。PtyConn 只负责报告"我已经不可用了"，由 Session 决定何时调 Close。这样 Session 能在 Close 前把当前 trace 入栈、记录日志、从 Manager 移除——PtyConn 自己 Close 会让 Session 事后才发现（zombie session：state=Idle 但 conn 已死）。

**为什么 drain 超时后 session 必须关闭：**

不 Close 的话远端命令继续跑，下次 Run 的 cmd 会被旧命令消费（如 cat 把 cmd 当输入回显），输出混乱——表现为"一直执行上一个命令，输出上一个命令的结果"。Close 终止 SSH channel，远端 shell 收到 SIGHUP 退出，污染源被清除。代价是 session 不可用，但比起连环污染这是正确权衡。

**硬超时上限：** `cmd timeout (30s) + drain timeout (3s) = 33s`。这是 run_in_session 的最坏阻塞时长。MCP 客户端串行化工具调用时，Agent 最多等 33s 就能继续操作（调 get_trace 诊断、或重新 login）。

**Shell 类型探测与降级：**

所有 LoginFlow 跑完、target shell 就绪后、RC 注入前，shell 还没被我们控制，不能用 PS1 sentinel 判断命令结束。探测命令自带结束标记：

```sh
echo __SHELL_DETECT__:$0:${BASH_VERSION:-}:${ZSH_VERSION:-}; echo __DETECT_END_<rand>__
```

`<rand>` 是一次性随机串。读到 `__DETECT_END_<rand>__` 即认为探测完成，从 `__SHELL_DETECT__` 行解析 shell 类型。探测阶段的输出（探测命令回显、shell 版本信息、结束标记）在注入 RC 前消耗掉，不进入后续命令的输出流。

根据 shell 类型选择注入路径：

| Shell | 注入路径 | 能力 |
|---|---|---|
| bash | PS1 `$(echo _$?)` + token 化（兼容所有版本，绕过 readonly PROMPT_COMMAND） | exit code + 边界 |
| zsh | PS1 `$(echo _$?)` + `setopt PROMPT_SUBST` + token 化 | exit code + 边界 |
| dash/ash/sh | 仅覆盖 PS1（`__P_<sid>__> `，不展开 `$(...)`） | 边界，无 exit code |
| 受限 shell (rbash) | 不注入，靠 prompt 正则 | 仅边界，靠 timeout 兜底 |

**为什么不解析用户原有 PS1：**

- 命令输出可能包含类似 PS1 的字符串（误匹配）
- 堡垒机/远端 `.bashrc` 可能预设怪异 PS1
- 不同 shell PS1 语法不同（bash `\u@\h` vs zsh `%n@%m`）
- 用户中途改 PS1 会破坏后续匹配
- PS1 本身不含退出码

因此选择**主动覆盖 PS1** 为已知 sentinel，把 shell 行为纳入控制，而非猜测用户 PS1 格式。

**边界场景：**

- 命令输出含 PS1 字面量（如 `echo $PS1` / `env | grep PS1`）：bash/zsh 走 token 化 sentinel，命令输出不可能预知本次 Run 的 `<token>`，字面量不会误匹配——readUntilCommandDoneToken 用精确 token 匹配，旧 token 的 sentinel 字面量也不匹配。dash/ash 不 token 化仍可能误匹配，靠固定 PS1 字面量 + sid 限制降低概率
- 命令改 PS1（`export PS1=...`）：下次 prompt 不匹配 sentinel → 超时 → 自动 Ctrl-C + drain；drain 成功则 session 回 idle，drain 失败则强制 Close
- 命令进入子 shell（`bash`/`python`）：子 shell PS1 不含 sentinel → 超时 → 自动 Ctrl-C；子 shell 通常响应 SIGINT 退出，drain 成功
- 命令读 stdin（`sudo` 提密码、`read -p`、`cat > file` 等）：`run_in_session` 超时返回 partial output（含提示符如 "Password:"）；Agent 据此改用 NOPASSWD sudo / PrivateKey 认证避免交互，或调 `get_trace` 看 raw_output 诊断后重新 login 调整 LoginFlow
- 命令不响应 SIGINT（vim / less / 交互式 REPL）：drain 超时 → 强制 Close，session 不可用。Agent 需重新 login。这是已知代价——vim 等不响应 SIGINT 的程序无法靠 Ctrl-C 中断
- SSH 断开：PTY EOF，Read 返回 0 → session 标记 closed，返回部分输出
- 远端 `.bashrc` 报错：报错信息进 PTY 流但 sentinel 仍会出现，清洗时保留 sentinel 之前内容作为输出

**Send 字节约定：`\r` vs `\n`：**

向远端 PTY 写字节时，"回车"该发 `\r`（CR, 0x0D）还是 `\n`（LF, 0x0A）？结论：**LoginFlow 的 `send` 字段用 `\r`，`run_in_session` 的 `cmd` 内部追加 `\n`**，两者不冲突，理由见下。

**链路：** 我们的 Go 代码 → SSH channel → 远端 PTY master → 远端 PTY 行规范（termios）→ bash / TUI 菜单。我们写什么字节就原样到达远端 PTY master，**本地终端不参与**（SSH 客户端把本地终端切 raw mode 直通）。翻译发生在远端 PTY 的行规范，由 termios 输入标志位控制：

| 标志 | 作用 | 默认 |
|------|------|------|
| ICRNL | 输入 `\r` → `\n` | **ON**（canonical 模式）|
| INLCR | 输入 `\n` → `\r` | **OFF** |
| IGNCR | 丢弃输入 `\r` | OFF |

**两种字节在不同模式下的行为：**

| 目标 | 模式 | 发 `\r` | 发 `\n` |
|------|------|---------|---------|
| bash 提示符（readline，ICRNL on）| 半 canonical | ✅ ICRNL 转 `\n`，bash 收到 `\n` | ✅ INLCR off，bash 收到 `\n` |
| TUI 菜单 / sudo 提示 / vim / less | raw mode（ICRNL off）| ✅ 菜单匹配 `\r`（真实终端发 `\r`）| ❌ 菜单不认 `\n`，输入卡住 |
| `\r\n` 到 bash（ICRNL on）| canonical | ⚠️ 两次回车：`\r`→`\n` + 原有 `\n`，第二行空命令 | — |

**核心结论：**
- 对 bash（ICRNL on），`\r` 和 `\n` 等价——都变成 bash 读到的 `\n`。所以 `run_in_session` 内部 `cmd + "\n"` 对 bash 零风险。
- 对 TUI 菜单（raw mode），**必须发 `\r`**——真实终端按 Enter 发的就是 `\r`，菜单程序按字节匹配 `\r`，发 `\n` 不认。
- 混合发 `\r\n` 在 bash 下是两次回车，TUI 下行为不可预测，避免。

**业界约定：**
- **expect（Don Libes, 1995）**——PTY 自动化鼻祖，文档明确："In most cases, you should use `\r` instead of `\n` when sending to a process. Processes that read from a terminal expect to see carriage returns, not line feeds." `send` 不做转换，用户显式写 `\r`。
- **OpenSSH 客户端**——把本地终端切 raw mode，Enter 键发 `\r` 透传到远端 PTY。`\r` 约定的来源。
- **pexpect（Python）**——反面教材。`sendline()` 默认追加 `os.linesep`（Unix 上是 `\n`），在 bash 下能用，遇到 sudo 密码提示或 TUI 菜单就挂。社区 workaround 是 `child.send('foo\r')` 或 `child.linesep = '\r'`。这是"自动处理但选错字节"的教训。
- **底层 SSH 库**（paramiko / golang.org/x/crypto/ssh / libssh2 / ssh2）——一律不转换，`Write([]byte("ls\n"))` 发 `ls\n`，`Write([]byte("ls\r"))` 发 `ls\r`。传输层语义透明。
- **telnet（RFC 854）**——协议规定行结束是 `\r\n`，与 PTY 语义不同，不参考。

**sshmng 的设计决策：**

1. **不做全局 `\n`→`\r` 自动替换**——与 paramiko / golang.org/x/crypto/ssh 一致。一旦做自动替换，调试时用户永远要问"我写的 `\n` 到底被换成什么了"，而 PTY 调试偏偏最依赖字节级精确。expect 30 年的经验印证了显式优于隐式。

2. **`PtyConn.Send(s string)`（LoginFlow 用）**：原样写入，不转换。配置里写什么就发什么。`LoginAction.Send` 字段支持 `\r` `\n` `\t` 转义，配置时**回车用 `\r`**（TUI 菜单要求）。

3. **`PtyConn.Run(cmd string)`（run_in_session 用）**：内部 `Write([]byte(cmd + "\n"))`，自动追加 `\n`。因为 target shell 此时已注入 RC、在 canonical 模式下跑，`\n` 是行分隔符，bash 收到 `\n` 执行命令。这里追加 `\r` 也等价（ICRNL 会转），但 `\n` 是 Go 字符串里最自然的换行，保持透明。

4. **配置示例**：LoginFlow 的 `send` 字段统一用 `\r` 表示回车（如 `"send": "1\r"`、`"send": "deploy-password\r"`），不要用 `\n`。`run_in_session` 的 `cmd` 参数按普通 shell 命令写（单行或多行都用 `\n` 分隔），sshmng 内部会追加最终的那个 `\n` 触发执行。

**为什么不给 LoginFlow 也加自动替换**：LoginFlow 几乎总是面对 raw mode 的 TUI 菜单，自动替换 `\n`→`\r` 看似方便，但破坏了"配置里写什么就发什么"的契约。Agent 调试 LoginFlow 失败时看 trace 里的 `send` 字段，必须能直接对照配置文件判断发了什么字节，不能有隐式转换层。显式写 `\r` 是可预测的，自动替换是不可预测的——PTY 自动化工具的 30 年经验一致选择前者。

**Pattern A 的连接生命周期：**

Pattern A 持有两个 SSH client：`jumpClient`（到 jumphost）和 `targetClient`（到 target，底层 conn 是 `jumpClient` 上的 direct-tcpip channel）。`targetClient` 必须在 `jumpClient` 关闭前关闭——先关 jumphost 会让 target 的底层 channel 立即失效，target client.Close() 报噪声错误。

`PtyConn.jumpClient` 字段（`ssh_j=true` 路径专用，direct / Pattern B 为 nil）通过 `SetJumpClient` 绑定。`Close()` 顺序：sftp → session → targetClient → jumpClient。

`Dialer.DialThrough(jumpClient, opts)` 开 direct-tcpip 通道（`jumpClient.Dial("tcp", addr)`，10s 超时兜底）+ `ssh.NewClientConn` 在其上建立第二层 SSH（10s 握手超时）。失败时关闭 direct-tcpip conn，调用方关闭 jumpClient。

`setupPatternA` 编排：Dial jumphost → DialThrough target → OpenPtyConn → SetJumpClient → 可选 SSHServer.LoginFlow（Stage="patternA"）→ DetectShell → InjectRC → TryEnableSftp（SFTP 可用，区别于 Pattern B）。

### 3.8 日志处理

**约束**：MCP 协议规定 **stdout 严禁写日志**（专用于 JSON-RPC）。日志只能走 stderr 或文件。

**方案：配置文件 + stderr bootstrap 兜底。**

- **操作日志**：走 `config.log_path` 指定的轮转文件。`slog` + `RotatingWriter`：写到 `<log_path>/sshmng.log`，超过 10MB 轮转，最多 5 份（`sshmng.log` + `sshmng.1.log` ~ `sshmng.4.log`），文件权限 0600。`log_path` 为空时不打日志（io.Discard）。
- **日志级别**：`config.log_level` 控制，支持 `debug` / `info` / `warn` / `error` 及缩写（`dbg`/`d`/`inf`/`i`/`w`/`err`/`e`，大小写不敏感）；空 = 默认 `info`；配错 Load 报错。
- **stderr**：只留 bootstrap 错误（MCP 还没起来、config 加载失败、known_hosts 权限错）和 fatal panic。`cmd/sshmng/main.go` 启动时先建一个 stderr logger 用于 bootstrap 阶段，config 加载成功后切到文件 logger。
- **不通过 MCP `notifications/message` 推日志**：MCP SDK 的 `LoggingHandler.Handle` 同步调 `ss.Log()` → `handleNotify` → `ioConn.Write`，和 tool result 共用 stdout JSON-RPC 通道、由 `writeMu` 串行化。client 处理慢时 result 等不到发送机会，工具"卡住"。同时这些日志作为 `notifications/message` 进入 Agent 上下文，占 context window、干扰决策。彻底废弃 notification 路径，所有日志走文件即可。

**Level 约定：**

| Level | 用途 |
|---|---|
| Debug | 详细内部状态（sentinel 匹配过程、shell 探测细节、loginflow 每步、sftp 建立） |
| Info | session 创建/关闭、TOFU host key 新增、login 成功 |
| Warning | idle timeout 触发、sftp 通道不可用、host key 变更（也是 Error）、Ctrl-C drain 超时 |
| Error | login 失败、host key 变更、PTY 建立/RC 注入失败 |

**打什么 / 不打什么：**

- ✅ 打：session 生命周期（create/close/timeout）、TOFU 事件（new/changed）、sftp 可用性、login 失败的 error 类别（不含凭据）、RC 注入失败
- ❌ 不打：命令输出（已在 `run_in_session` 的 `output` 字段）、密码/passphrase/private_key 内容（敏感）、完整 PTY 流（量大且含敏感）

**DEBUG 日志会完整记录** LoginFlow 每步 send/read/match、run_in_session 的 cmd/output、sftp upload/download、PTY stdout 片段（不截断、不打码）。**分享日志时注意脱敏**——LoginFlow 的 `send` 字段、PTY 输出都可能含密码。

**为什么不用纯 stderr：** MCP Inspector 等 client 默认捕获 server stderr 作为日志，但 info 级日志会混在 fatal 错误里，且 client 不一定持久化。配置文件 + 轮转让用户能按 level 过滤、按 session 关联、事后翻查。

**为什么不用 MCP notifications（旧方案，已废弃）：** 旧方案用 `mcp.NewLoggingHandler(req.Session, opts)` 把 slog 记录转成 `notifications/message` 推到 client，导致两个问题：(1) 同步写 stdout 通道，和 tool result 共用 `writeMu`，client 处理慢时 tool result 卡住；(2) DEBUG 日志作为 notification 进入 Agent 上下文，占 context window、干扰决策。彻底废弃 notification 路径后，日志走文件即可，问题消除。

### 3.9 诊断手段（卡死时的可观察性）

**核心约束**：MCP 客户端（Inspector / Claude Code）通常**串行化工具调用**——等当前工具返回才发下一个。因此"卡死时调 get_trace 看现场"不可行：run_in_session 卡住时，客户端在等它返回，根本不会发 get_trace 请求。

**设计原则**：所有可能阻塞的操作必须有**硬超时上限**，到时间一定返回。诊断靠"事后看 trace + raw_output"，不靠"卡死时调工具看现场"。

**各操作的硬超时：**

| 操作 | 硬超时 | 超时后行为 |
|---|---|---|
| `login`（Dial+auth） | 10s | 返回 error |
| `login`（detectShell） | 5s | 返回 error |
| `login`（LoginFlow 单 step） | 10s（可配） | 报错，返回 login_trace |
| `login`（LoginFlow global） | 60s（可配） | 报错，返回 login_trace |
| `login`（injectRC） | 5s | 返回 error |
| `login`（sftp 建立） | 5s | sftp 不可用，login 仍成功 |
| `run_in_session`（cmd） | 30s（可配） | 进三段式超时处理 |
| `run_in_session`（drain） | 3s | 返回 connUnusable=true，Session 调 Close，session 进 closed |
| `upload` / `download` | 300s（可配） | 返回已传输字节 + timed_out=true |

**最坏阻塞时长**：
- `login`：10s + 5s + 60s + 5s + 5s = 85s（理论上限；实际通常 < 15s）
- `run_in_session`：30s + 3s = 33s

**已知缺口（待修复）**：
- `client.NewSession()` / `session.RequestPty()` / `session.Shell()` 这三个 SSH 协议操作无 per-operation 超时，网络半开（TCP 死了但没 RST）时可能卡几分钟。待加 global deadline 保护。

**卡死时的事后诊断路径**：

1. **run_in_session 返回 timed_out=true**：调 `get_trace(sid, last_n, 0)` 看最近命令的 `raw_output`，含 PTY 原始字节（ANSI / sentinel / 部分输出），判断命令卡在哪。run_in_session 响应本身只有清洗后的 output，不含 raw_output
2. **session 进 closed 态**：可能是 drain 超时强制 Close。调 `get_trace(sid, last_n, 0)` 看最近命令的 raw_output（closed 后 trace 保留 10 分钟）
3. **login 失败**：看返回的 `login_trace`（LoginFlow 失败时携带）或 `error`（SSH auth 失败时自解释）
4. **stat() 看状态**：state=idle/running/closed、last_activity、commands_run 判断 session 是否健康

**不提供的诊断**：
- 不返回 currentTrace（正在 running 的命令 trace）——客户端串行化，调不到
- 不提供"中断当前 run_in_session"的工具——run_in_session 自身有硬超时，不需要外部中断
- 不提供 raw PTY 流实时订阅——MCP 协议不适合流式输出，且 raw 流含敏感数据

## 4. 服务端设计（后续迭代）

> v1 阶段不实现服务端。客户端独立运行，本地存储配置。以下保留为后续迭代参考。

- 服务端提供增删改查能力
- 暴露 gRPC 接口
- JSON 存储

**待讨论（v2）：**

- 同步方向：服务端权威 + 客户端只读缓存 / 双向同步 + 冲突检测 / 单向
- 离线写支持
- 多人冲突解决策略

## 5. 技术选型

- Golang
- 客户端：`golang.org/x/crypto/ssh` + `github.com/pkg/sftp`
- 传输代理：`golang.org/x/net/proxy`（SOCKS5）+ 自写 HTTP CONNECT（约 50 行，协议简单标准库无现成实现）
- MCP SDK：`github.com/modelcontextprotocol/go-sdk`（官方，与 Google 协作维护）
- 服务端（v2）：grpc + json 存储
- 不使用 Web 技术栈

**为什么不用 goph：** 6 个核心需求里 5 个需要直接操作 `*ssh.Client` / `*ssh.Session`（PTY 精细控制、ProxyJump 透明转发、LoginFlow 决策树、sftp 动态可用性、Host key TOFU 验证）。goph 唯一能帮上忙的 `Run` / `Upload` / `Download` 我们都不用或会绕过——`Run` 不用（统一 PTY），`Upload`/`Download` 内部就是调 `pkg/sftp`，我们要自己控动态可用性必然绕过。约 90% 代码会穿透 goph 抽象层，被频繁穿透的抽象反而增加阅读成本，省下的 ~20 行 auth 装配不值得引入这层依赖。

**为什么不用 mark3labs/mcp-go：** 两个候选都成熟可用，但官方 SDK 对我们这个项目有两处关键优势。一是 API 稳定阶段不同：官方 v1.x（2026-05 已发 v1.6.1，跟踪 MCP spec 2026-07-28），mcp-go 仍在 v0.x（2026-07 v0.56.0，README 自述"under active development"），我们的工具集 schema 不希望哪天升级被 break。二是结构化 I/O 支持差异：我们的 MCP 工具几乎全部有结构化输入输出（`login` 返回 `{sid, ok, error, login_trace}`、`run_in_session` 返回 `{output, exit_code, timed_out, truncated, total_bytes}`、`get_trace` 返回 `[]TraceEntry`）。官方 SDK 用 typed struct + jsonschema tag 同时定义 input 和 output，handler 直接 `return nil, Output{...}, nil`；mcp-go 则要 builder pattern 拼参数 + 手动 `json.Marshal` 塞进 `CallToolResult`，schema 校验和类型安全都丢了。mcp-go 唯一优势是入场早、社区例子多，但官方 SDK 的 API 足够直白，不太需要照抄例子。

## 6. 待讨论清单

- [x] MCP 工具清单细化与接口签名
- [x] 本地存储格式
- [x] 安全模型（敏感数据保护）
- [x] 技术选型（SSH 库）
- [x] MCP SDK 具体包选择
- [x] 资源识别（name 路径 + tags 平列表 + 多关键字 AND 搜索）
- [x] 日志处理（配置文件 log_path + log_level + stderr 兜底，见 3.8）
- [ ] .xsh 导入导出格式（v2）
- [ ] 同步方向（v2）
