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
- **单命令执行失败**：`run_in_session` 返回 output/exit_code 即可，命令错误通常直接打印在 output（如 `ls: cannot access ...`），Agent 看 output 就能判断。仅在 output 不足以判断时（sentinel 未匹配、交互式命令卡住、需看 send_input 历史），Agent 主动调 `get_trace(sid, last_n)`

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
    Name      string
    Send      string             // 直接字符串，支持转义 \n \r \t；不支持变量引用（需要的字符如密码、用户名直接写）
    Expects   []Expect           // 按顺序尝试匹配
    TimeoutMs int                // 0 = 默认 10000
}

type Expect struct {
    Pattern string                // 无前缀 = glob，"re:" 前缀 = 正则
    Next    string                // 另一个 LoginAction.Name，或 "success" 表示成功
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
    "send": "2\n",
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
  - 失败分类见 3.2：
    - SSH auth 失败（含 host key 变更、网络拨号失败）：sid 为空，error 说明原因，**login_trace 为空**
    - LoginFlow 失败：sid 为空，error 说明原因，login_trace 供诊断
  - login_trace 结构与 get_trace 返回的一致

run_in_session(sid, cmd, timeout_ms?, max_output_bytes?) → {output, exit_code, timed_out?, truncated?, total_bytes?}
  - timeout_ms 默认 30s
  - max_output_bytes 默认 65536（64KB）；output 超过则保头截断，truncated=true，total_bytes 返回实际字节数
  - 命令结束：返回完整输出，timed_out=false
  - 超时：返回部分输出，timed_out=true，命令仍在后台跑
  - session 状态保持 "running"，新调用报错 "session busy"
  - 截断时 Agent 应改用 `tail`/`head`/`grep` 或重定向到文件后 `download`
  - 失败不携带 trace；output 通常自解释（命令错误直接打印）。需诊断（sentinel 未匹配、交互卡住、看 send_input 历史）时调 get_trace(sid, last_n)

send_special(sid, key) → {ok, err}
  - 异步发送控制字符到 PTY，用于中断卡住的命令
  - key: "ctrl-c"(\x03) | "ctrl-d"(\x04) | "ctrl-z"(\x1a) | "tab"(\t) | "esc"(\x1b)
  - 仅 session "running" 时可调；idle 时报错 "session idle, use run_in_session"

send_input(sid, text) → {ok, err}
  - 向运行中的命令发送任意文本（如密码、read 的回答、cat 的多行内容）
  - 仅 session "running" 时可调；idle 时报错 "session idle, use run_in_session"
  - 用于应对交互式命令：run_in_session 超时后从 partial output 判断命令在等输入（如 "Password:"），调 send_input 喂入文本，命令继续
  - 内容记入当前命令的 TraceEntry.Inputs 字段（含密码等敏感数据，见 3.5 安全模型）

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
  - state: "idle" | "running"
  - sftp_available: bool，login 时同步尝试建立 sftp 通道的结果
  - idle: 可 run_in_session
  - running: 命令执行中，run_in_session 报错 "session busy"
```

**TraceEntry 结构：**

```go
type TraceEntry struct {
    Time      string    // 本地时间字符串，格式 "2006-01-02 15:04:05.000"
    ElapsedMs int64     // 距 session 起点的毫秒数
    Send      string    // 发送的命令（run_in_session）或 LoginAction.Send
    Inputs    []string  // 运行期间通过 send_input 发送的内容（可空，仅 command 阶段）
    Expect    string    // 命中的 pattern / sentinel；失败时为空
    Output    string    // 原始 PTY 输出（按 trunc_output 截断，未清洗）
}
```

Trace 存原始信息用于诊断：`Output` 是未清洗的 PTY 原始流（含 sentinel、PS1 残留、ANSI 转义），与 `run_in_session` 返回的 `output`（已清洗）区分。`Send` 在 LoginFlow 阶段含 `LoginAction.Send` 原文（去掉变量引用后，密码、用户名等凭据直接写在 Send 中，会进 trace）；`Inputs` 含 send_input 发送的全部文本（也可能含密码等敏感数据）。

**expect 字段语义：**

- login_action 阶段：命中的 pattern（失败时为空）
- command 阶段：命中的 sentinel（`__E_<sid>__:<exit_code>__`，见 3.7 命令边界识别）

**关键决策：**

1. 不提供 `exec_command` —— 访问环境本质是连续多命令，显式 session 更自然
2. 不提供 `raw_*` 系列 —— trace 足够诊断，配置修复后重新 login 验证
3. `update_ssh_server` / `update_jumphost` / `update_proxy` 三个独立工具（不统一成一个 `update`），各自有明确 schema；均用 JSON Merge Patch，patch=null 删除整个实体，key 不存在则创建
4. `upload`/`download` 走 sftp 独立通道，复用 sid 对应的会话连接；sftp 通道在 login 时同步建立（5s 超时），不可用时 upload/download 报错
5. `send_special` 发控制字符、`send_input` 发任意文本，均仅限 session "running" 状态；Agent 用 `stat()` 轮询 session 状态判断命令是否结束
6. `run_in_session` 超时后返回部分输出，命令仍在后台跑，需 `send_special("ctrl-c")` 中断；交互式命令（sudo/read/cat>file）靠 `send_input` 喂入文本
7. 统一 PTY 模式 —— 所有连接（含直连）走 PTY 维持 cwd/env，支持登录后交互（su/角色切换）；堡垒机是 PTY 模式的子类型（SSHJ=false），不单独建模
8. Jumphost 两种形态靠 `SSHJ` 字段区分 —— `true` = 透明转发（ssh -J 语义），`false` = 交互式堡垒机（`LoginFlow` 仅准备 jumphost 到主菜单就绪，登录 target 由 `SSHServer.LoginFlow` 接管）
9. Host key 验证用 TOFU —— 首次连接记录 key，后续验证；key 变更报错且 `update_*` 不能重置（安全决策需人工确认），见 3.5
10. 终端规范化统一在 target shell 就绪后一次性注入 —— LoginFlow 阶段不强制规范化（堡垒机菜单可能不支持 `TERM=dumb`/`PS1` 等设置，且交互式 prompt 不应有命令边界概念），靠 expect 前的 ANSI 过滤兜底；target shell 就绪后注入完整 RC（TERM/NO_COLOR/LANG/stty/PS1/PROMPT_COMMAND/history），见 3.7
11. 操作前先识别 —— Agent 执行 `login`/`update_*` 等操作前，先用 `list_*(query)` 把模糊引用（IP/地域/服务名）解析为唯一 name；候选 1 个直接用，多个反问消歧，0 个反问确认。三类资源（SSHServer / Jumphost / Proxy）均支持 `name` 路径 + `tags` 平 token 列表，tag 值用自然语言。详见 2.8

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
- 文件权限 `0600`

### 3.5 安全模型

**v1 阶段（客户端独立运行）：**

- 敏感字段（Password / Passphrase）明文存储在 `config.json`；`PrivateKey` 存的是私钥文件路径，私钥内容不在 config.json 中
- config.json 文件权限 `0600`，仅属主可读写
- 启动时检查 config.json 文件权限，若权限过宽（如 0644）拒绝加载并报错
- 创建 config.json 时立即 `chmod 0600`
- 启动时校验 `PrivateKey` 路径指向的私钥文件权限为 0600（或更严），过宽则拒绝加载
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

- Trace 的 `Send`（LoginFlow 阶段含 `LoginAction.Send` 原文）、`Inputs`（`send_input` 发送的内容）、`Output`（PTY 原始流）都可能含密码等敏感数据——去掉变量引用后，凭据直接写在 `LoginAction.Send` 字符串中，会原样进 trace
- Trace 仅存内存，`close_session` 后保留 10 分钟自动清理，不落盘
- 同进程内 `get_trace` 需要 sid，跨进程不可见
- 若不可接受密码进 trace，优先用 NOPASSWD sudo / PrivateKey 认证避免在 LoginFlow 中明文写密码；`send_input` 传密码同理

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
- `run_in_session` 超时后返回部分输出，session 保持 `running`，需 `send_special("ctrl-c")` 中断
- `send_special` 异步发送控制字符，Agent 用 `stat()` 轮询状态判断命令是否结束
- `close_session` 无论状态都强制关闭

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
- `PS1` 覆盖为 sentinel
- `PROMPT_COMMAND` / `precmd` 注入 sentinel
- `stty -echo`：关闭输入回显
- `set +o history`（bash）/ `unset HISTFILE`（zsh）：不污染远端历史文件

**注入流程时序：**

1. SSH 连接建立（直连 target，或先连 jumphost），PTY 分配（默认 TERM，不强制规范化）
2. Pattern B 下：走 `Jumphost.LoginFlow`，准备到 jumphost 主菜单就绪（expect 匹配前做 ANSI 过滤）
3. 走 `SSHServer.LoginFlow`：Pattern B 下从主菜单登录到 target shell；Pattern A 下完成 SSH auth 到 target 后承担 target 认证后交互（如有，同样做 ANSI 过滤）
4. target shell 就绪
5. 发送 shell 探测命令（见"Shell 类型探测与降级"），确定 shell 类型
6. 根据 shell 类型注入完整 RC（TERM/NO_COLOR/LANG/stty/PS1/PROMPT_COMMAND/history）
7. 等待首个 PS1 sentinel 出现，确认注入成功
8. session 转入 `idle`，可 `run_in_session`

**命令边界识别（PS1 + PROMPT_COMMAND 双 sentinel）：**

PTY 模式下命令输出和 shell 提示符混在一起。通过覆盖 PS1 和注入 PROMPT_COMMAND / precmd hook，让 shell 主动发射已知 sentinel，无需解析用户原有 PS1。

**Sentinel 格式（session 级）：**

- PS1 sentinel：`__P_<sid>__> ` — shell 等待下一条命令时显示
- 命令结束 sentinel：`__E_<sid>__:<exit_code>__` — PROMPT_COMMAND 在每条命令后发射

`<sid>` 是 session 级 8 字节十六进制随机串（如 `a3f2b1c9`），登录时生成一次，整个会话不变。相比每命令 UUID 方案，减少注入开销且足够区分。

**注入 RC（bash）：**

```sh
export TERM=dumb
export NO_COLOR=1
export LANG=C.UTF-8
stty cols 120 rows 100 2>/dev/null
export PS1='__P_<sid>__> '
if [ -n "$PROMPT_COMMAND" ]; then
    PROMPT_COMMAND="$PROMPT_COMMAND; echo \"__E_<sid>__:\$?__\""
else
    PROMPT_COMMAND='echo "__E_<sid>__:$?__"'
fi
set +o history
stty -echo 2>/dev/null
```

注意 `\$?` 转义：让 `$?` 在 PROMPT_COMMAND 执行时展开为上一条命令退出码，而非注入时展开。追加而非覆盖 `PROMPT_COMMAND`，保留用户已有 hook。

**注入 RC（zsh）：**

```sh
export TERM=dumb
export NO_COLOR=1
export LANG=C.UTF-8
stty cols 120 rows 100 2>/dev/null
export PS1='__P_<sid>__> '
function _sshmng_precmd() { echo "__E_<sid>__:$?__" }
precmd_functions+=(_sshmng_precmd)
unset HISTFILE
stty -echo 2>/dev/null
```

**输出流时序：**

```
Agent 发送: ls /tmp\n
           shell 执行 ls，输出 file1\r\nfile2\r\n
           ls 退出，$? = 0
           PROMPT_COMMAND 触发，输出 __E_<sid>__:0__\r\n
           shell 打印 PS1: __P_<sid>__>
Agent 读到: file1\r\nfile2\r\n__E_<sid>__:0__\r\n__P_<sid>__>
```

**解析逻辑：**

1. 读 PTY 流直到匹配 `__P_<sid>__>\s*$`（PS1 sentinel 出现，shell 就绪）
2. 从流中匹配 `__E_<sid>__:(-?\d+)__` 提取 exit code
3. 清洗输出：移除 sentinel 行、PS1 残留、防御性剥离 ANSI 转义（`regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")`）
4. 超时未匹配 PS1 sentinel → 返回部分输出，timed_out=true，session 保持 running

**Shell 类型探测与降级：**

所有 LoginFlow 跑完、target shell 就绪后、RC 注入前，shell 还没被我们控制，不能用 PS1 sentinel 判断命令结束。探测命令自带结束标记：

```sh
echo __SHELL_DETECT__:$0:${BASH_VERSION:-}:${ZSH_VERSION:-}; echo __DETECT_END_<rand>__
```

`<rand>` 是一次性随机串。读到 `__DETECT_END_<rand>__` 即认为探测完成，从 `__SHELL_DETECT__` 行解析 shell 类型。探测阶段的输出（探测命令回显、shell 版本信息、结束标记）在注入 RC 前消耗掉，不进入后续命令的输出流。

根据 shell 类型选择注入路径：

| Shell | 注入路径 | 能力 |
|---|---|---|
| bash | 字符串 PROMPT_COMMAND（兼容所有版本） | exit code + 边界 |
| zsh | precmd_functions | exit code + 边界 |
| dash/ash/sh | 仅覆盖 PS1 | 边界，无 exit code |
| 受限 shell (rbash) | 不注入，靠 prompt 正则 | 仅边界，靠 timeout 兜底 |

**为什么不解析用户原有 PS1：**

- 命令输出可能包含类似 PS1 的字符串（误匹配）
- 堡垒机/远端 `.bashrc` 可能预设怪异 PS1
- 不同 shell PS1 语法不同（bash `\u@\h` vs zsh `%n@%m`）
- 用户中途改 PS1 会破坏后续匹配
- PS1 本身不含退出码

因此选择**主动覆盖 PS1** 为已知 sentinel，把 shell 行为纳入控制，而非猜测用户 PS1 格式。

**边界场景：**

- 命令输出含 sentinel 字面量：sid 用 8 字节随机，碰撞概率 ~1/2^32，可接受；要更稳可升 16 字节
- 命令改 PS1（`export PS1=...`）：下次 prompt 不匹配 → 触发超时 → Agent 用 `send_special("ctrl-c")` 恢复
- 命令进入子 shell（`bash`/`python`）：子 shell PS1 不含 sentinel → 超时。Agent 可通过 `send_input` 与子 shell 交互，但需自行判断命令何时结束（无法靠 sentinel 自动检测）
- 命令读 stdin（`sudo` 提密码、`read -p`、`cat > file` 等）：`run_in_session` 超时返回 partial output（含提示符如 "Password:"）；Agent 据此调 `send_input(sid, text)` 喂入文本，命令继续；`cat > file` 类需 `send_special("ctrl-d")` 结束输入
- SSH 断开：PTY EOF，Read 返回 0 → session 标记 closed，返回部分输出
- 远端 `.bashrc` 报错：报错信息进 PTY 流但 sentinel 仍会出现，清洗时保留 sentinel 之前内容作为输出

### 3.8 日志处理

**约束**：MCP 协议规定 **stdout 严禁写日志**（专用于 JSON-RPC）。日志只能走 stderr、文件、或 MCP logging notifications 三选。

**方案：MCP logging notifications + stderr 兜底。**

- **操作日志**：用 `slog` + `mcp.NewLoggingHandler(req.Session, opts)` 创建 session-scoped logger，通过 `notifications/message` 推到 client。client（Claude Code / Desktop）在 MCP debug 视图展示，支持 `logging/setLevel` 控制 level。
- **stderr**：只留 bootstrap 错误（MCP 还没起来时，如配置加载失败、known_hosts 权限错）和 fatal panic。`cmd/sshmng/main.go` 用 `slog.NewTextHandler(os.Stderr, nil)` 作为 base logger，session 接入前用。
- **日志文件**：v1 不做。client 已捕获 MCP 日志，`--mcp-debug` 可事后诊断；rotation/权限管理是额外复杂度，收益小。

**Level 约定：**

| Level | 用途 |
|---|---|
| Debug | 详细内部状态（sentinel 匹配过程、shell 探测细节） |
| Info | session 创建/关闭、TOFU host key 新增、login 成功 |
| Warning | idle timeout 触发、sftp 通道不可用、host key 变更（也是 Error） |
| Error | login 失败、host key 变更、PTY 建立/RC 注入失败 |

**打什么 / 不打什么：**

- ✅ 打：session 生命周期（create/close/timeout）、TOFU 事件（new/changed）、sftp 可用性、login 失败的 error 类别（不含凭据）、RC 注入失败
- ❌ 不打：命令输出（已在 `run_in_session` 的 `output` 字段）、密码/passphrase/private_key 内容（敏感）、send_input 文本（可能含密码）、完整 PTY 流（量大且含敏感）

**Rate limit：** `LoggingHandlerOptions.MinInterval = 100ms`，防止高频事件（如命令超时循环）淹没 client。

**为什么不用纯 stderr：** info 级日志会混在 fatal 错误里，且 client 不一定持久化；操作日志走 notifications 让 client 能按 level 过滤、按 session 关联。

**为什么不用纯 MCP notifications：** server 启动失败（MCP 还没起来）时无任何日志可看，debug 困难。stderr 兜底是必须的。

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
- [x] 资源识别（name 路径 + tags 平列表 + 子串搜索）
- [x] 日志处理（MCP notifications + stderr 兜底，见 3.8）
- [ ] .xsh 导入导出格式（v2）
- [ ] 同步方向（v2）
