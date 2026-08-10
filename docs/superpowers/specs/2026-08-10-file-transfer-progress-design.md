# `sshmng file` 传输进度提示

日期: 2026-08-10

## 背景

`sshmng file` 的五个子命令（upload / download / upload-dir / download-dir / relay）
传输过程中终端完全静默，只在结束后打印一行汇总（如 `uploaded X -> host:remote (N bytes)`）。
大文件传输时用户看不到进度、速率，无法区分"在传"还是"卡住"。

## 目标

为五个子命令加 TTY 感知的单行进度条：交互式终端实时显示进度，管道/CI/非 TTY 自动静默，
不污染日志和脚本。**不降速**——进度层是传输链路的纯装饰，最坏情况 = 现状速度 + 可忽略的记账开销。

## 非目标

- 不改 relay 的 barrier 传输模型（`fanWriter` 每 chunk `wg.Wait()` 等所有存活目标）。
  存活目标齐头并进，per-dest 进度无真实差异，故 relay 不画独立 per-dest 条。
  relay 引擎重构（让目标按各自速率独立前进）另议，不在本次范围。
- 不改 MCP 路径。MCP 工具（upload/download/upload_dir/download_dir/relay_transfer）
  继续走现有无进度路径，行为不变。

## 架构原则

进度渲染是纯 CLI 层关注点，传输层只负责"量"的测量与回调，不碰终端。所有改动叠加式、可选：
回调 `nil` = 当前进度静默，行为不变。

| 层 | 改动 | 影响面 |
|----|------|--------|
| `internal/progress`（新） | 进度条渲染 + 计数 reader/writer | 新包，零现有代码 |
| `internal/termutil`（新） | 共享 Windows VT flags（从 `pty/relay_console_windows.go` 抽取） | `pty.Relay` 与 `progress.Bar` 共用，消除两份 VT 逻辑漂移 |
| `internal/ssh/conn` + `pty` | `DirTransferOptions.OnProgress`；UploadDir/DownloadDir 内部按文件计数回调 | 加可选字段，nil 短路 |
| `internal/ssh/session` | `Manager.RelayTransferWithProgress` 新方法 | 现有 `RelayTransfer` 不动 |
| `internal/cli` | file_cmd 五个子命令接进度条 | 唯一直接面向用户的改动 |

进度走 **stderr**，最终汇总走 **stdout**（现状的 `out`）。这样 `sshmng file download ... > /dev/null`
仍可见进度，脚本捕获 stdout 不混入进度行。

## 组件设计

### `internal/termutil`（新）

把 `internal/ssh/pty/relay_console_windows.go` 的 `enableVTOutput` / `restoreOutputMode`
抽到共享包。`pty.Relay`（现有 VT 处理）与 `progress.Bar` 共用，避免两份 VT 逻辑漂移。
Unix 下为 no-op。最近三个 commit 都在修 Windows VT，统一一处是降低未来维护成本的关键。

### `internal/progress`（新）

```go
// Bar 是 TTY 感知的单行进度条，写到 w（预期 os.Stderr）。
// w 不是终端时所有方法 no-op —— 调用方无需自己判断 TTY。
type Bar struct { ... }

// NewBar 创建进度条。total<0 = 未知大小（不定模式：只显示已传字节+速率，无百分比/条/ETA）。
// w 非 TTY 时返回静默 bar。
func NewBar(w io.Writer, label string, total int64) *Bar

func (b *Bar) SetFiles(totalFiles int) *Bar   // 启用文件计数维度（目录传输）
func (b *Bar) SetBytes(n int64)               // 设置绝对已传字节，节流重绘
func (b *Bar) SetFilesDone(n int)             // 设置绝对已完成文件数
func (b *Bar) SetStatus(tags string)          // 末尾状态标签（relay: "2✓ 1✗ 1⏳"）
func (b *Bar) Finish()                        // 擦除进度行，终端干净；幂等，可 defer
```

关键设计点：

- **TTY 检测**：`term.IsTerminal(int(os.Stderr.Fd()))`。非 TTY → 静默 bar，所有方法 no-op。
  脚本/CI 天然无污染，测试无需 mock 终端。
- **节流**：`SetBytes` 内部 `time.Since(lastDraw) >= 100ms` 才重绘（约 10fps），首次和 100% 必画。
  避免大文件高频 Read 刷屏 + 浪费 CPU。
- **重绘**：`\r` + 内容 + 末尾空格填满到行宽（擦除残留）+ 不换行。`Finish()` 写 `\r` + 空格 + `\r`
  擦掉进度行，让最终汇总行（stdout）干净。
- **宽度感知**：`term.GetSize` 取宽，bar 填充长度 = 总宽 - 非条部分。窄终端按 ETA→速率→条 顺序降级丢弃。
- **人性化**：`12.3/25.0 MB`、`2.1 MB/s`、`ETA 6s`。速率 = 字节/已耗时；ETA = (剩余/速率)，速率≈0 时显示 `—`。
- **不定模式**（total<0，如远端 Stat 失败）：`myserver  12.3 MB  2.1 MB/s`，无条/百分比/ETA。

计数 reader/writer（同包，可单测）：

```go
type CountingReader struct { R io.Reader; n int64; Fn func(int64) }  // Fn 收累计字节
type CountingWriter struct { W io.Writer; n int64; Fn func(int64) }
```

### 单文件 upload / download

**Upload：改用 `UploadSized` + 计数 reader。**

```go
fi, _ := os.Stat(local)                    // 本地大小，免费
size := -1; if fi != nil { size = fi.Size() }
cr := &progress.CountingReader{R: f, Fn: func(n int64) { bar.SetBytes(n) }}
n, timedOut, err := ptyConn.UploadSized(cr, size, remote, *timeoutMs)
bar.Finish()
```

不降速论证：`UploadSized` 把 reader 包成 `io.LimitReader(newCtxReader(src, ctx), size)`
（sftp.go:117），`*sftp.File.ReadFrom` 的 type switch **直接命中 `*io.LimitReader` 类型**
（sftp.go:84-88 注释明说），走并发 pipelining 路径。它认的是外层 `*io.LimitReader`，不依赖
内层 reader 有无 `Stat()`。故 `CountingReader` 只实现 `Read` 即可，不碰 Stat 转发那条脆弱路径。

**验证承诺**：实施时写基准对比 `Upload(*os.File)`（现状）vs `UploadSized+CountingReader`（新），
确认吞吐同一量级。若意外退化，退路 = `CountingReader` 转发 `Stat()`（照 `ctxReaderWithStat` 样子），
等价于现状路径 + 计数装饰，仍不降速。

**Download：包 writer，远端 Stat 出总量。**

```go
total := int64(-1)
if fi, err := ptyConn.Stat(remote); err == nil { total = fi.Size() }
cw := &progress.CountingWriter{W: f, Fn: func(n int64) { bar.SetBytes(n) }}
n, timedOut, err := ptyConn.Download(remote, cw, *timeoutMs)
bar.Finish()
```

不降速论证：下载侧 `*sftp.File.WriteTo` 的读侧并发（sftp.go:131-132）只看 src（`sftp.File`），
包 dst writer 不影响并发。Stat 失败/非常规文件 → total<0 → 不定模式。

边界：
- size=0：bar 瞬间 100%，Finish。不卡。
- 超时：bar 先 Finish 擦行，再走现有超时汇总输出。
- 错误：defer bar.Finish() 保证进度行擦除后打印 Error 行。

不动的部分：`Conn` 接口、`Session.Upload/Download`、MCP `tools_file.go` 的 Upload/Download。
CLI 持有具体类型 `*pty.PtyConn`，直接调其已有的 `UploadSized`/`Stat`/`Download`。

### 目录传输 upload-dir / download-dir

**总量采集**：walk 阶段累加 `totalBytes` / `totalFiles`。下载侧在 walk 里已有 `fi.Size()`
（远端 stat）；上传侧走本地 walk，同样可拿 `fi.Size()`，实施时核实 `UploadDir` walk 补齐累加。
walk 后 `NewBar(stderr, label, totalBytes).SetFiles(totalFiles)`。

walk 有 `walkErrs` → bar 降不定模式（total=-1），只显示已传字节+文件数+速率，避免假 100%。
最终汇总行（stdout，现状 `printDirResult`）仍报真实 Files/Bytes。

**回调**：`DirTransferOptions.OnProgress`。

```go
type DirTransferOptions struct {
    Conflict    ConflictPolicy
    Concurrency int
    TimeoutMs   int
    // OnProgress 在每文件传输完成后回调（成功才计）。nil = 无进度。
    // bytes 是迄今累计成功字节，files 是迄今成功文件数。
    OnProgress  func(bytes int64, files int)
}
```

worker pool 每文件传完（sftp_dir.go:349 附近 `result.Bytes += int64(n); result.Files++`）旁调一次：

```go
mu.Lock()
if err == nil {
    result.Bytes += int64(n)
    result.Files++
    if opts.OnProgress != nil {
        opts.OnProgress(result.Bytes, result.Files)   // 持锁回调，见下
    }
}
if timedOut { result.TimedOut++ }
mu.Unlock()
```

CLI 传回调：`opts.OnProgress = func(b, f) { bar.SetBytes(b); bar.SetFilesDone(f) }`。

**持锁回调 = bar 免锁 + 安全**：worker pool 已用 `mu` 串行化 `result` 累加（sftp_dir.go:351-361）。
回调在持锁块内调，多 worker 的回调天然串行化，`bar.SetBytes/SetFilesDone` 不会被两个 goroutine
同时重入，bar 不用自己加锁。代价：回调期间持 `mu`，其他 worker 完成文件时短暂阻塞。但回调是
`time.Since` 比较 + 大概率节流跳过（100ms 内只重绘一次），实际写 stderr 微秒级，worker 默认 4，
阻塞可忽略。将来 Concurrency 调到几十、回调变重时再考虑挪到锁外。

bar 多文件视觉（单行，文件数最右，最易被窄终端降级丢弃）：

```
myserver  ████████░░░░░░░░  53%  1.2/2.3 GB  2.0 MB/s  ETA 34s  [412/871 files]
```

不动的部分：`Conn` 接口 `UploadDir`/`DownloadDir` 签名不变（`DirTransferOptions` 加字段是兼容改动）。
MCP dir 工具继续传 `OnProgress=nil` → nil 短路。`Session.UploadDir`/`DownloadDir` 透传 opts，无需改。

### relay（barrier 模型，诚实呈现）

现状 `fanWriter.Write`（relay.go:41-83）每 chunk `wg.Wait()` 等所有存活目标 → 存活目标齐头并进，
per-dest 进度无真实差异。故 relay 不画 N 个独立条，画**单主条 + dest 状态标签**：

```
web-01  ████████████░░░░░░░  62%  15.5/25.0 MB  2.1 MB/s  ETA 5s  [2✓ 1✗ 1⏳]
```

- **主条**：已下载字节 / 源总量。因齐头并进，这一条代表所有存活目标的同步进度。总量来自源 `Stat`
  （relay.go:195 已有 `size`）。
- **dest 状态**：`✓`完成 / `✗`失败（失败隔离） / `⏳`在传。只反映存活状态，不画独立进度。
  失败 dest 详情（final bytes + 原因）留到结束后的 stdout 汇总（现状 per-dest 输出不变）。

**实现**：`fanWriter` 实现 `io.Writer`，是 `srcSess.Download(srcPath, fw, ...)` 的 dst（relay.go:214）。
包一层 `CountingWriter{fanWriter}` 即拿已下载字节——同单文件下载论证，sftp 读侧并发不受 dst 包装影响，
零降速。上传 goroutine 结束时（relay.go:225-242 那段）回调一次状态，bar 更新计数。低频，不需多行高频重绘。

`Manager.RelayTransferWithProgress` 新方法（现有 `RelayTransfer` 不动，MCP 继续用它）：

```go
func (m *Manager) RelayTransferWithProgress(
    srcSid, srcPath string, dstSids []string, dstPath string, timeoutMs int,
    onDownload func(bytes int64),                                    // 高频：主条
    onDestDone  func(dstSid string, ok bool, bytes int64, err error), // 低频：状态
) (*RelayResult, error)
```

不动的部分：`Manager.RelayTransfer`、MCP `RelayTransfer` 工具、`relayDestJSON`、`fanWriter`/`fanSlot`
并发模型。relay 的 per-dest 失败隔离语义不变。

## 测试

- `internal/progress`：纯函数单测 `formatBar`/字节人性化/速率/ETA/节流逻辑；TTY 检测用 fake writer
  覆盖静默路径。
- 单文件：现有 fake Conn（`session_test.go` 的 fake）补 CountingReader/Writer 透传断言。
- 基准：`Upload(*os.File)` vs `UploadSized+CountingReader` 吞吐对比，进 PR。若退化触发 Stat 转发退路。
- 目录/relay：回调签名变更走现有测试路径，OnProgress=nil 行为不变（回归保证）。

## 文档

CLI 用法（`printFileUsage` 输出、README "Usage"、`docs/agents.md`）子命令签名不变，
无需改文档结构。进度是运行时行为，非接口变更。CLAUDE.md 的 Pre-release checklist 不涉及。

## 风险

1. **UploadSized 并发路径未实测**：依 sftp.go 注释推断 `*io.LimitReader` 触发并发。退路 = Stat 转发，
   等价现状。基准验证进 PR。
2. **Windows VT 光标控制**：进度条用 `\r` + 块字符，依赖 termutil 共享 VT flags。`term.GetSize` 在
   Windows console 的行为实施时核实。
3. **持锁回调阻塞**：当前 Concurrency 默认 4，微秒级，可忽略。高并发场景留了锁外化退路。
