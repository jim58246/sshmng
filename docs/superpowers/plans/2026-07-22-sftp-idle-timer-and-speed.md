# SFTP Idle Timer 与传输速度修复 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 sftp 传输期间会话被 idle timeout 误杀的 bug，并将 sftp 传输速度提升到接近专业 sftp 工具的水平（通过启用 `pkg/sftp` 内置的并发 pipelining）。

**Architecture:** 两个独立修复，分两部分：
- **Part A（idle timer）**：在 `Session.Upload` / `Session.Download` 复用 `RunInSession` 的状态机模式——进锁检查状态、切 `StateRunning` + `stopIdleTimer`、传输、切回 `StateIdle` + `resetIdleTimer` + 更新 `lastActivity`。sftp 与 `run_in_session` 通过 state=Running 互斥串行化（同一 session 本就不该并发）。
- **Part B（速度）**：把 `copyCtx` 串行循环替换为 `io.Copy`——`io.Copy` 会自动断言 `*sftp.File` 的 `ReadFrom` / `WriteTo` 接口，走 `pkg/sftp` 内置的并发 pipelining。超时通过 `context.AfterFunc` 关闭 sftp.File 解除阻塞 io.Copy 的 Write/Read。同时给 `sftp.NewClient` 加 `MaxPacket` 选项调大单包大小。

**Tech Stack:** Go 1.25（`context.AfterFunc` 可用）、`github.com/pkg/sftp`、`golang.org/x/crypto/ssh`。

## Global Constraints

- 不改 `Conn` 接口签名（`Upload(src io.Reader, ...)` / `Download(remotePath, dst io.Writer, ...)` 保持不变）
- 不改 MCP 工具层（`tools_file.go` 不动）
- 现有 sftp 测试（`internal/ssh/pty/sftp_test.go`）必须全过——它们是行为契约
- `Session.Upload` / `Session.Download` 与 `RunInSession` 共用同一把 `s.mu`，state 转换必须对称（进 Running 必出 Running，除非 session 被强制 Close）
- 设计文档 `docs/ssh-session-manager-design.md` §3.7 "命令执行期间不算空闲" 已隐含 sftp 同理——本修复是补实现，不改设计语义

---

## File Structure

| 文件 | 改动 | 责任 |
|---|---|---|
| `internal/ssh/session/session.go` | 改 `Session.Upload` / `Session.Download`（L293-300） | 套状态机 |
| `internal/ssh/session/session_test.go` | 扩 `fakeConn` 支持 sftp + 新增测试 | Part A 测试 |
| `internal/ssh/conn/sftp.go` | 改 `NewSftpClient`（L27-43）加 options | Part B 调优 |
| `internal/ssh/pty/sftp.go` | 改 `Upload` / `Download`（L28-89）+ 删 `copyCtx`（L95-120） | Part B pipelining |
| `internal/ssh/pty/sftp_test.go` | 新增 benchmark | Part B 验证 |

`tools_file.go` 不动——MCP 层调用 `sess.Upload` / `sess.Download`，session 层修复对 MCP 透明。

---

# Part A: SFTP 期间 idle timer 误杀修复

### Task 1: 扩展 fakeConn 支持 sftp 操作

**Files:**
- Modify: `internal/ssh/session/session_test.go`（`fakeConn` 定义在 L14 附近）

**Interfaces:**
- Consumes: `session.Conn.Upload` / `Download` 签名
- Produces: `fakeConn` 上 `Upload` / `Download` 可被测试控制——支持阻塞、返回值注入；新增 `SftpAvailable()` 可配置返回 true

- [ ] **Step 1: 读现有 fakeConn 结构**

Run: `grep -n "type fakeConn\|func newFakeConn\|func (f \*fakeConn)" internal/ssh/session/session_test.go`
Expected: 列出 fakeConn struct 定义 + 所有方法。当前 `Upload` / `Download` 固定返回 `conn.ErrSftpUnavailable`，`SftpAvailable()` 固定返回 false。

- [ ] **Step 2: 写失败的测试——fakeConn 支持 sftp 闭环**

在 `internal/ssh/session/session_test.go` 末尾追加：

```go
// TestFakeConnSftpRoundtrip 验证 fakeConn 扩展后能跑通 Upload/Download 闭环，
// 为后续 session 层状态机测试铺路。
func TestFakeConnSftpRoundtrip(t *testing.T) {
	conn := newFakeConn()
	conn.sftpEnabled = true
	conn.uploadData = []byte("hello sftp")

	// Upload：src 是任意 Reader，fakeConn 把读到的字节存到 uploadData
	n, timedOut, err := conn.Upload(strings.NewReader("uploaded"), "/r.txt", 1000)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if timedOut || n != 8 {
		t.Errorf("Upload returned n=%d timedOut=%v, want 8/false", n, timedOut)
	}
	if string(conn.uploadedBytes) != "uploaded" {
		t.Errorf("uploaded bytes = %q, want %q", conn.uploadedBytes, "uploaded")
	}

	// Download：dst 是任意 Writer，fakeConn 把 downloadData 写进去
	conn.downloadData = []byte("downloaded")
	var buf bytes.Buffer
	n, timedOut, err = conn.Download("/r.txt", &buf, 1000)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if timedOut || n != 11 {
		t.Errorf("Download returned n=%d timedOut=%v, want 11/false", n, timedOut)
	}
	if buf.String() != "downloaded" {
		t.Errorf("downloaded = %q, want %q", buf.String(), "downloaded")
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/ssh/session/ -run TestFakeConnSftpRoundtrip -v`
Expected: FAIL——`fakeConn` 当前 `Upload` 直接返回 `ErrSftpUnavailable`。

- [ ] **Step 4: 扩展 fakeConn struct + 方法**

在 `fakeConn` struct 增加字段（保留现有字段不动）：

```go
type fakeConn struct {
	// ... 现有字段保持不变 ...

	// sftp 支持（Part A 测试用）
	sftpEnabled    bool
	uploadBlock    chan struct{} // nil = 不阻塞；非 nil = Upload 阻塞直到该 chan 关闭
	downloadBlock  chan struct{} // nil = 不阻塞；非 nil = Download 阻塞直到该 chan 关闭
	uploadedBytes  []byte        // Upload 读到的字节
	downloadData   []byte        // Download 写到 dst 的字节
	uploadDelay    time.Duration // Upload 完成前 sleep 这么久（模拟慢传输）
}
```

替换 `SftpAvailable` / `Upload` / `Download` 方法：

```go
func (f *fakeConn) SftpAvailable() bool { return f.sftpEnabled }

func (f *fakeConn) Upload(src io.Reader, remotePath string, timeoutMs int) (int, bool, error) {
	if !f.sftpEnabled {
		return 0, false, conn.ErrSftpUnavailable
	}
	if f.uploadBlock != nil {
		<-f.uploadBlock
	}
	if f.uploadDelay > 0 {
		time.Sleep(f.uploadDelay)
	}
	n, err := io.ReadAll(src)
	f.uploadedBytes = append(f.uploadedBytes, n...)
	return len(n), false, err
}

func (f *fakeConn) Download(remotePath string, dst io.Writer, timeoutMs int) (int, bool, error) {
	if !f.sftpEnabled {
		return 0, false, conn.ErrSftpUnavailable
	}
	if f.downloadBlock != nil {
		<-f.downloadBlock
	}
	if f.uploadDelay > 0 {
		time.Sleep(f.uploadDelay)
	}
	n, err := dst.Write(f.downloadData)
	return n, false, err
}
```

如 `newFakeConn` 未初始化上述字段，零值即可（`sftpEnabled=false`、`uploadBlock=nil`），保持向后兼容。

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/ssh/session/ -run TestFakeConnSftpRoundtrip -v`
Expected: PASS

- [ ] **Step 6: 跑全量 session 测试确认无回归**

Run: `go test ./internal/ssh/session/ -v`
Expected: 全部 PASS——`fakeConn.sftpEnabled` 默认 false，原 Run 测试不受影响。

- [ ] **Step 7: Commit**

```bash
git add internal/ssh/session/session_test.go
git commit -m "test(session): extend fakeConn with controllable sftp Upload/Download"
```

---

### Task 2: Session.Upload 套状态机 + 测试 idle timer 不误杀

**Files:**
- Modify: `internal/ssh/session/session.go:293-295`（`Session.Upload`）
- Test: `internal/ssh/session/session_test.go`

**Interfaces:**
- Consumes: `Session.stopIdleTimer` / `Session.resetIdleTimer` / `s.mu` / `s.state`（已在 `RunInSession` 用过的私有 API）
- Produces: `Session.Upload` 与 `RunInSession` 状态语义对称——非 idle 态返回 "session busy"、Closed 态返回 "session closed"

- [ ] **Step 1: 写失败的测试——idle 期间 sftp 不应触发 timeout**

在 `internal/ssh/session/session_test.go` 追加：

```go
// TestUploadDoesNotFireIdleTimeout: idleTimeout=100ms，sftp Upload 阻塞 400ms。
// 修复前：timer 在 100ms 触发 Close，Upload 返回后 state=Closed。
// 修复后：Upload 期间 timer 被 stop，Upload 返回后 state=Idle。
func TestUploadDoesNotFireIdleTimeout(t *testing.T) {
	conn := newFakeConn()
	conn.sftpEnabled = true
	conn.uploadBlock = make(chan struct{}) // Upload 阻塞直到 close

	mgr := NewManager()
	s := mgr.newSessionWithConn("sid", "srv", conn, 100*time.Millisecond, nil)
	defer s.Close()

	go func() {
		time.Sleep(400 * time.Millisecond)
		close(conn.uploadBlock)
	}()

	start := time.Now()
	_, _, err := s.Upload(strings.NewReader("data"), "/r.txt", 5000)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("Upload returned too fast: %v, want >= 400ms", elapsed)
	}
	if st := s.State(); st != StateIdle {
		t.Errorf("state after Upload = %s, want idle (timer should not have fired)", st)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ssh/session/ -run TestUploadDoesNotFireIdleTimeout -v`
Expected: FAIL——`state after Upload = closed, want idle`。100ms 时 timer 触发 `s.Close()`，state 转 Closed。

- [ ] **Step 3: 写第二个失败的测试——Upload 期间 run_in_session 应得 "session busy"**

```go
// TestUploadBlocksRunInSession: Upload 进行中 state=Running，并发 RunInSession 应立即报 "session busy"。
func TestUploadBlocksRunInSession(t *testing.T) {
	conn := newFakeConn()
	conn.sftpEnabled = true
	conn.uploadBlock = make(chan struct{})

	mgr := NewManager()
	s := mgr.newSessionWithConn("sid", "srv", conn, time.Minute, nil)
	defer s.Close()

	go func() {
		s.Upload(strings.NewReader("data"), "/r.txt", 5000)
	}()
	// 等 Upload 进入阻塞（fakeConn.Upload 会读 src 后阻塞在 uploadBlock）
	time.Sleep(50 * time.Millisecond)

	_, _, _, _, _, err := s.RunInSession("ls", 1000, 0)
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("RunInSession during Upload: err=%v, want 'session busy'", err)
	}

	close(conn.uploadBlock)
	time.Sleep(50 * time.Millisecond)
	if st := s.State(); st != StateIdle {
		t.Errorf("state after Upload done = %s, want idle", st)
	}
}
```

Run: `go test ./internal/ssh/session/ -run TestUploadBlocksRunInSession -v`
Expected: FAIL——当前 `Session.Upload` 不切 state，`RunInSession` 能正常进入，不会报 "busy"。

- [ ] **Step 4: 写第三个失败的测试——Closed 态 Upload 报错**

```go
// TestUploadOnClosedSession: session 关闭后 Upload 返回 "session closed"。
func TestUploadOnClosedSession(t *testing.T) {
	conn := newFakeConn()
	conn.sftpEnabled = true
	mgr := NewManager()
	s := mgr.newSessionWithConn("sid", "srv", conn, time.Minute, nil)
	s.Close()

	_, _, err := s.Upload(strings.NewReader("data"), "/r.txt", 1000)
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("Upload on closed session: err=%v, want 'session closed'", err)
	}
}
```

Run: `go test ./internal/ssh/session/ -run TestUploadOnClosedSession -v`
Expected: FAIL——当前 `Session.Upload` 直接透传 `conn.Upload`，不检查 state。

- [ ] **Step 5: 实现 Session.Upload 状态机**

替换 `internal/ssh/session/session.go:293-295`：

```go
// Upload 把 src 上传到远端 remotePath。
// 与 RunInSession 对称：进锁检查 state、切 Running + stopIdleTimer、传输、
// 切回 Idle + resetIdleTimer + 更新 lastActivity。
// 非 idle 态返回 "session busy"；Closed 态返回 "session closed"。
func (s *Session) Upload(src io.Reader, remotePath string, timeoutMs int) (int, bool, error) {
	s.mu.Lock()
	if s.state == StateClosed {
		s.mu.Unlock()
		return 0, false, errors.New("session closed")
	}
	if s.state == StateRunning {
		s.mu.Unlock()
		return 0, false, errors.New("session busy")
	}
	s.state = StateRunning
	s.stopIdleTimer()
	s.mu.Unlock()

	n, timedOut, err := s.conn.Upload(src, remotePath, timeoutMs)

	s.mu.Lock()
	s.lastActivity = time.Now()
	if s.state != StateClosed {
		s.state = StateIdle
		s.resetIdleTimer()
	}
	s.mu.Unlock()
	return n, timedOut, err
}
```

注意：sftp 错误（如远端磁盘满、权限拒绝）不等于 conn 不可用——不像 `RunInSession` 的 `connUnusable` 路径要 Close。sftp 失败仍回 Idle，下次 op 自然失败检测 conn 死亡。

- [ ] **Step 6: 跑三个测试确认通过**

Run: `go test ./internal/ssh/session/ -run 'TestUploadDoesNotFireIdleTimeout|TestUploadBlocksRunInSession|TestUploadOnClosedSession' -v`
Expected: 三个全 PASS

- [ ] **Step 7: 跑全量 session 测试确认无回归**

Run: `go test ./internal/ssh/session/ -v`
Expected: 全部 PASS

- [ ] **Step 8: Commit**

```bash
git add internal/ssh/session/session.go internal/ssh/session/session_test.go
git commit -m "fix(session): stop idle timer during sftp Upload to prevent session kill"
```

---

### Task 3: Session.Download 套状态机 + 测试

**Files:**
- Modify: `internal/ssh/session/session.go:298-300`（`Session.Download`）
- Test: `internal/ssh/session/session_test.go`

**Interfaces:**
- Consumes: 同 Task 2
- Produces: `Session.Download` 与 `Session.Upload` / `RunInSession` 状态语义对称

- [ ] **Step 1: 写失败的测试——Download 期间 idle timer 不误杀**

在 `internal/ssh/session/session_test.go` 追加（与 Task 2 对称，用 `downloadBlock` 字段；如 fakeConn 没有该字段，复用 `uploadBlock` 或加一个）：

```go
// TestDownloadDoesNotFireIdleTimeout: 与 TestUploadDoesNotFireIdleTimeout 对称。
func TestDownloadDoesNotFireIdleTimeout(t *testing.T) {
	conn := newFakeConn()
	conn.sftpEnabled = true
	conn.downloadData = []byte("data")
	conn.downloadBlock = make(chan struct{}) // 若 fakeConn 字段名不同，调整此处

	mgr := NewManager()
	s := mgr.newSessionWithConn("sid", "srv", conn, 100*time.Millisecond, nil)
	defer s.Close()

	go func() {
		time.Sleep(400 * time.Millisecond)
		close(conn.downloadBlock)
	}()

	var dst bytes.Buffer
	start := time.Now()
	_, _, err := s.Download("/r.txt", &dst, 5000)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("Download returned too fast: %v, want >= 400ms", elapsed)
	}
	if st := s.State(); st != StateIdle {
		t.Errorf("state after Download = %s, want idle", st)
	}
}
```

Task 1 的 fakeConn 已加 `downloadBlock chan struct{}` 字段并在 `Download` 方法中阻塞于它——本测试直接用即可。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ssh/session/ -run TestDownloadDoesNotFireIdleTimeout -v`
Expected: FAIL——`state after Download = closed, want idle`

- [ ] **Step 3: 实现 Session.Download 状态机**

替换 `internal/ssh/session/session.go:298-300`：

```go
// Download 把远端 remotePath 下载到 dst。
// 状态机与 Upload 对称。
func (s *Session) Download(remotePath string, dst io.Writer, timeoutMs int) (int, bool, error) {
	s.mu.Lock()
	if s.state == StateClosed {
		s.mu.Unlock()
		return 0, false, errors.New("session closed")
	}
	if s.state == StateRunning {
		s.mu.Unlock()
		return 0, false, errors.New("session busy")
	}
	s.state = StateRunning
	s.stopIdleTimer()
	s.mu.Unlock()

	n, timedOut, err := s.conn.Download(remotePath, dst, timeoutMs)

	s.mu.Lock()
	s.lastActivity = time.Now()
	if s.state != StateClosed {
		s.state = StateIdle
		s.resetIdleTimer()
	}
	s.mu.Unlock()
	return n, timedOut, err
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/ssh/session/ -run TestDownloadDoesNotFireIdleTimeout -v`
Expected: PASS

- [ ] **Step 5: 跑全量 session 测试 + pty 层 sftp 测试确认无回归**

Run: `go test ./internal/ssh/session/ ./internal/ssh/pty/ -v`
Expected: 全部 PASS——pty 层用真实 fake sftp server，session 层用 fakeConn，互不干扰。

- [ ] **Step 6: Commit**

```bash
git add internal/ssh/session/session.go internal/ssh/session/session_test.go
git commit -m "fix(session): stop idle timer during sftp Download to prevent session kill"
```

---

# Part B: SFTP 传输速度修复

### Task 4: 给 sftp.NewClient 加 MaxPacket 选项

**Files:**
- Modify: `internal/ssh/conn/sftp.go:27-43`（`NewSftpClient`）

**Interfaces:**
- Consumes: `sftp.NewClient` + `sftp.MaxPacket` option
- Produces: `NewSftpClient` 返回的 `*sftp.Client` 单包大小 64KB（默认 32KB）

- [ ] **Step 1: 写测试——sftp client 单包大小为 64KB**

`pkg/sftp` 不直接暴露 maxPacket，但 `File.Write` 内部按 client.maxPacket 分片。可通过 mock server 计数 SSH_FXP_WRITE 包数量间接验证：1MB 数据 / 64KB = 16 个 write 包；若仍是 32KB 则 32 个。

在 `internal/ssh/conn/sftp_test.go`（如不存在则创建）追加：

```go
package conn

import (
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// TestNewSftpClientMaxPacket 验证 NewSftpClient 配置了 64KB MaxPacket。
// 通过反射读 sftp.Client 内部 maxPacket 字段（私有，用 unsafe 或测试 helper）。
// 简化方案：直接断言 NewSftpClient 不 panic 且返回非 nil client；
// 详细 maxPacket 值由 pty 层 benchmark 间接验证速度提升。
func TestNewSftpClientMaxPacket(t *testing.T) {
	// 这个测试主要保证 NewSftpClient 在加了 sftp.MaxPacket option 后仍能编译并工作。
	// 真实的速度提升由 pty 层 benchmark 验证。
	// 此处仅做 smoke test：构造一个 ssh.Client（用 nil 或 mock）调用 NewSftpClient，
	// 期望返回 error（nil ssh.Client 无法建立 sftp session）而不是 panic。
	_, err := NewSftpClient(nil)
	if err == nil {
		t.Errorf("NewSftpClient(nil) should return error, got nil")
	}
}
```

> 注：真实 sftp client 需要 ssh.Client 才能工作，单元测试层难以直接验证 maxPacket 值。此 smoke test 仅防回归——保证改 option 后不 panic、不破坏签名。速度提升由 Task 7 的 benchmark 端到端验证。

- [ ] **Step 2: 跑测试确认当前实现的行为基线**

Run: `go test ./internal/ssh/conn/ -run TestNewSftpClientMaxPacket -v`
Expected: PASS（当前实现 `sftp.NewClient(nil)` 也会返回 error，不 panic）。这个 smoke test 在改前改后都通过——它是防回归护栏，不是失败测试。

- [ ] **Step 3: 改 NewSftpClient 加 MaxPacket option**

替换 `internal/ssh/conn/sftp.go:27-43`：

```go
// SftpMaxPacket 是 sftp 单个 SSH_FXP_WRITE/READ 包的最大 payload 字节数。
// 默认 32KB 偏小（跨地域 RTT 高时 ack 次数多），调到 64KB 减半 ack 次数。
// 再大（128KB+）会撞 SSH channel window 默认 2MB 的边界，且边际收益递减。
const SftpMaxPacket = 64 * 1024

func NewSftpClient(client *ssh.Client) (*sftp.Client, error) {
	type result struct {
		c   *sftp.Client
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := sftp.NewClient(client, sftp.MaxPacket(SftpMaxPacket))
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-time.After(SftpDialTimeout):
		return nil, fmt.Errorf("sftp channel establishment timed out after %s", SftpDialTimeout)
	}
}
```

- [ ] **Step 4: 跑 smoke test 确认通过**

Run: `go test ./internal/ssh/conn/ -run TestNewSftpClientMaxPacket -v`
Expected: PASS

- [ ] **Step 5: 跑 pty 层 sftp 测试确认调大 packet 不破坏行为**

Run: `go test ./internal/ssh/pty/ -run 'TestUpload|TestDownload' -v`
Expected: 全部 PASS

- [ ] **Step 6: Commit**

```bash
git add internal/ssh/conn/sftp.go internal/ssh/conn/sftp_test.go
git commit -m "perf(conn): set sftp MaxPacket to 64KB to halve ack count"
```

---

### Task 5: Upload 用 io.Copy 替换 copyCtx

**Files:**
- Modify: `internal/ssh/pty/sftp.go:28-55`（`PtyConn.Upload`）+ 删 `copyCtx`（L95-120，仅当 Task 6 完成后）
- Test: `internal/ssh/pty/sftp_test.go`（现有测试 + 新增 benchmark 在 Task 7）

**Interfaces:**
- Consumes: `io.Copy`、`context.AfterFunc`（Go 1.21+，项目 Go 1.25 OK）、`*sftp.File`（实现 `io.ReaderFrom`）
- Produces: `PtyConn.Upload` 内部走 `sftp.File.ReadFrom` 的 pipelining 路径

**核心机制：** `io.Copy(dst, src)` 在 dst 是 `*sftp.File` 时自动调用 `dst.ReadFrom(src)`，后者内部并发发多个 `SSH_FXP_WRITE` 包在飞，ack 异步回收——把串行 RTT 摊薄。超时通过 `context.AfterFunc` 关闭 dst（sftp.File），让在飞的 Write 失败解除 io.Copy 阻塞。

- [ ] **Step 1: 先跑现有 Upload 测试建立基线**

Run: `go test ./internal/ssh/pty/ -run 'TestUpload' -v`
Expected: 全部 PASS——`TestUploadNormalPath` / `TestUploadSftpUnavailable` / `TestUploadTimeout` 当前用 `copyCtx` 全过。记录这组测试是契约，改完必须仍全过。

- [ ] **Step 2: 改 PtyConn.Upload 用 io.Copy**

替换 `internal/ssh/pty/sftp.go:28-55`：

```go
// Upload 把 src 上传到远端 remotePath。
//   - timeoutMs=0 用默认 300s
//   - 返回 (已传输字节数, 是否超时, error)
//   - sftp 通道未建立时返回 conn.ErrSftpUnavailable
//   - 超时返回已传输字节 + timed_out=true
//
// 用 io.Copy 触发 *sftp.File.ReadFrom 的内置并发 pipelining——多个 SSH_FXP_WRITE
// 包同时在飞，ack 异步回收，把跨地域 RTT 摊薄。超时通过 context.AfterFunc 关闭
// sftp.File 解除 io.Copy 阻塞：在飞的 Write 收到 close 通知后失败返回。
func (p *PtyConn) Upload(src io.Reader, remotePath string, timeoutMs int) (int, bool, error) {
	p.logger.Debug("sftp upload start", "sid", p.sid, "remote", remotePath, "timeout_ms", timeoutMs)
	p.mu.Lock()
	sftpClient := p.sftpClient
	p.mu.Unlock()
	if sftpClient == nil {
		return 0, false, conn.ErrSftpUnavailable
	}

	timeout := conn.DefaultTransferTimeout
	if timeoutMs > 0 {
		timeout = time.Duration(timeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	dst, err := sftpClient.Create(remotePath)
	if err != nil {
		return 0, false, fmt.Errorf("create remote %s: %w", remotePath, err)
	}
	defer dst.Close()

	// ctx 到期时关闭 dst，解除 io.Copy 在 dst.Write（内部 ReadFrom）上的阻塞。
	// sftp.File.Close 是幂等的——defer 的 Close 在 stop 后不会重复发 SSH_FXP_CLOSE
	// （已 closed 时直接返回 nil）。
	stop := context.AfterFunc(ctx, func() {
		dst.Close()
	})

	n, err := io.Copy(dst, src)
	stop()
	timedOut := ctx.Err() == context.DeadlineExceeded
	p.logger.Debug("sftp upload done",
		"sid", p.sid, "remote", remotePath, "bytes", n, "timed_out", timedOut)
	return int(n), timedOut, err
}
```

注意点：
1. `context.AfterFunc` 返回 `stop func() bool`，调用它取消注册——若 ctx 已触发，stop 返回 false 且 AfterFunc 已在跑，无需特殊处理
2. `dst.Close()` 在 AfterFunc 里调用，与 io.Copy 内部的 Write 存在并发——但 sftp.File 内部有 mutex，Close 会等当前 Write 完成或让其在途包失败。pkg/sftp 文档保证 Close 安全
3. `timedOut` 判定改用 `ctx.Err() == context.DeadlineExceeded`——因为 io.Copy 返回的 err 可能是 "file already closed"（来自 AfterFunc 关闭 dst），`errors.Is(err, context.DeadlineExceeded)` 不一定命中。ctx.Err() 直接反映 timeout 是否触发

- [ ] **Step 3: 跑 Upload 测试确认契约保持**

Run: `go test ./internal/ssh/pty/ -run 'TestUpload' -v`
Expected: 全部 PASS——
- `TestUploadNormalPath`：1MB 数据上传 + 读回校验，内容一致
- `TestUploadSftpUnavailable`：无 sftp 通道时返回 "sftp not available"
- `TestUploadTimeout`：慢 reader + 100ms 超时 → `timed_out=true`、`bytes > 0`。改用 io.Copy 后，慢 reader 让 `dst.ReadFrom` 阻塞在 `src.Read`——AfterFunc 关闭 dst，下一个 `dst.Write` 失败，io.Copy 返回。可能比原 copyCtx 多花一个 chunk 时间（≤20ms），但 `timed_out=true` 与 `bytes > 0` 仍成立

若 `TestUploadTimeout` flaky（慢 reader 20ms chunk 刚好让 100ms 超时落在 src.Read sleep 中间，AfterFunc 关 dst 后 ReadFrom 仍等 src.Read 返回才看到错误），把测试 timeout 从 100ms 调到 150ms 缓冲——但优先保持原参数，只有 flaky 时再调。

- [ ] **Step 4: 跑 pty 全量测试确认无回归**

Run: `go test ./internal/ssh/pty/ -v`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/ssh/pty/sftp.go
git commit -m "perf(pty): use io.Copy in sftp Upload to enable pipelined writes"
```

---

### Task 6: Download 用 io.Copy 替换 copyCtx

**Files:**
- Modify: `internal/ssh/pty/sftp.go:62-89`（`PtyConn.Download`）+ 删 `copyCtx`（L95-120）

**Interfaces:**
- Consumes: `io.Copy`、`context.AfterFunc`、`*sftp.File`（实现 `io.WriterTo`）
- Produces: `PtyConn.Download` 内部走 `sftp.File.WriteTo` 的 pipelining 路径

- [ ] **Step 1: 跑现有 Download 测试建立基线**

Run: `go test ./internal/ssh/pty/ -run 'TestDownload' -v`
Expected: 全部 PASS

- [ ] **Step 2: 改 PtyConn.Download 用 io.Copy**

替换 `internal/ssh/pty/sftp.go:62-89`：

```go
// Download 把远端 remotePath 下载到 dst。
//   - timeoutMs=0 用默认 300s
//   - 返回 (已传输字节数, 是否超时, error)
//   - sftp 通道未建立时返回 conn.ErrSftpUnavailable
//   - 超时返回已传输字节 + timed_out=true
//
// 用 io.Copy 触发 *sftp.File.WriteTo 的内置并发 pipelining——多个 SSH_FXP_READ
// 请求同时在飞。超时通过 context.AfterFunc 关闭 src（sftp.File）解除 io.Copy 阻塞。
func (p *PtyConn) Download(remotePath string, dst io.Writer, timeoutMs int) (int, bool, error) {
	p.logger.Debug("sftp download start", "sid", p.sid, "remote", remotePath, "timeout_ms", timeoutMs)
	p.mu.Lock()
	sftpClient := p.sftpClient
	p.mu.Unlock()
	if sftpClient == nil {
		return 0, false, conn.ErrSftpUnavailable
	}

	timeout := conn.DefaultTransferTimeout
	if timeoutMs > 0 {
		timeout = time.Duration(timeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	src, err := sftpClient.Open(remotePath)
	if err != nil {
		return 0, false, fmt.Errorf("open remote %s: %w", remotePath, err)
	}
	defer src.Close()

	stop := context.AfterFunc(ctx, func() {
		src.Close()
	})

	n, err := io.Copy(dst, src)
	stop()
	timedOut := ctx.Err() == context.DeadlineExceeded
	p.logger.Debug("sftp download done",
		"sid", p.sid, "remote", remotePath, "bytes", n, "timed_out", timedOut)
	return int(n), timedOut, err
}
```

- [ ] **Step 3: 删除 copyCtx 函数**

删 `internal/ssh/pty/sftp.go:91-120` 的 `copyCtx` 函数。Upload 和 Download 都已不再调用，留死代码会被 linter 告警。同时检查 `import` 块——若 `context` 仍被使用（Upload/Download 都用了 `context.WithTimeout`），保留；若不再用则删。

- [ ] **Step 4: 跑 Download 测试确认契约保持**

Run: `go test ./internal/ssh/pty/ -run 'TestDownload' -v`
Expected: 全部 PASS——
- `TestDownloadNormalPath`：上传再下载，内容一致
- `TestDownloadSftpUnavailable`：无 sftp 通道时报错
- `TestDownloadTimeout`：慢 writer + 100ms 超时 → `timed_out=true`、`bytes > 0`

- [ ] **Step 5: 跑 pty 全量测试 + vet**

Run: `go test ./internal/ssh/pty/ -v && go vet ./internal/ssh/pty/`
Expected: 全部 PASS，vet 无警告（确认 copyCtx 删除后无未使用 import）

- [ ] **Step 6: Commit**

```bash
git add internal/ssh/pty/sftp.go
git commit -m "perf(pty): use io.Copy in sftp Download to enable pipelined reads"
```

---

### Task 7: 加 sftp 传输 benchmark 验证提速

**Files:**
- Create: `internal/ssh/pty/sftp_bench_test.go`

**Interfaces:**
- Consumes: `newFakeShellServerWithSftp`（已在 sftp_test.go 用过）、`newDialerWithTempKnownHosts`、`NewPtyConn`
- Produces: `BenchmarkSftpUpload` / `BenchmarkSftpDownload` 测吞吐量

- [ ] **Step 1: 写 benchmark**

创建 `internal/ssh/pty/sftp_bench_test.go`：

```go
package pty

import (
	"bytes"
	"testing"

	"sshmng/internal/config"
	"sshmng/internal/ssh/conn"
)

// BenchmarkSftpUpload 测 sftp 上传吞吐量。
// 修复前（copyCtx 串行 + 32KB 包）：~640 KB/s @ 50ms RTT
// 修复后（io.Copy pipelining + 64KB 包）：接近带宽上限
//
// 注：fake sftp server 走 loopback，RTT 极低，提速比例不反映真实跨地域场景。
// 此 benchmark 主要防回归——确保改 pipelining 后没引入新的同步点。
func BenchmarkSftpUpload(b *testing.B) {
	srv := newFakeShellServerWithSftp(&testing.T{})
	d := newDialerWithTempKnownHosts(&testing.T{})
	client, err := d.Dial(conn.DialOptions{
		Addr:          srv.Addr(),
		User:          "alice",
		Auth:          config.SSHAuth{Password: "wonderland"},
		HostKeyVerify: true,
	})
	if err != nil {
		b.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		b.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	data := bytes.Repeat([]byte("x"), 4<<20) // 4MB
	b.ResetTimer()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		n, _, err := p.Upload(bytes.NewReader(data), "/bench.txt", 60000)
		if err != nil {
			b.Fatalf("Upload: %v (bytes=%d)", err, n)
		}
	}
}

// BenchmarkSftpDownload 测 sftp 下载吞吐量。
func BenchmarkSftpDownload(b *testing.B) {
	srv := newFakeShellServerWithSftp(&testing.T{})
	d := newDialerWithTempKnownHosts(&testing.T{})
	client, err := d.Dial(conn.DialOptions{
		Addr:          srv.Addr(),
		User:          "alice",
		Auth:          config.SSHAuth{Password: "wonderland"},
		HostKeyVerify: true,
	})
	if err != nil {
		b.Fatalf("Dial: %v", err)
	}
	defer client.Close()
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		b.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	data := bytes.Repeat([]byte("y"), 4<<20) // 4MB
	if _, _, err := p.Upload(bytes.NewReader(data), "/bench_dl.txt", 60000); err != nil {
		b.Fatalf("seed Upload: %v", err)
	}

	b.ResetTimer()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		n, _, err := p.Download("/bench_dl.txt", io.Discard, 60000)
		if err != nil {
			b.Fatalf("Download: %v (bytes=%d)", err, n)
		}
	}
}
```

> 注：`newFakeShellServerWithSftp` / `newDialerWithTempKnownHosts` 当前签名是 `(t *testing.T)`。Benchmark 里需 `(b *testing.B)`——若 helper 接受 `*testing.T` 不接受 `*testing.B`，需先在 helper 签名改用 `testing.TB` 接口（这是测试基建改动，先看 helper 实际签名）。若改动过大，benchmark 用 `&testing.T{}` hack 也可——但优先改 helper 接 `testing.TB`。

- [ ] **Step 2: 跑 benchmark 确认能跑通**

Run: `go test ./internal/ssh/pty/ -bench BenchmarkSftpUpload -benchtime=1x`
Expected: 输出一行 `BenchmarkSftpUpload-XX   1   <ns/op>   <MB/s>  4MB/op`，不 panic。

如果 `newFakeShellServerWithSftp` 签名不兼容，先调整 helper：

```go
// 原：func newFakeShellServerWithSftp(t *testing.T) *fakeShellServer
// 改：func newFakeShellServerWithSftp(tb testing.TB) *fakeShellServer
//   并把内部 t.Helper() / t.Cleanup() / t.Fatalf() 全改 tb.XXX
```

同样改 `newDialerWithTempKnownHosts`。

- [ ] **Step 3: 跑完整 benchmark 记录数据**

Run: `go test ./internal/ssh/pty/ -bench 'BenchmarkSftp' -benchtime=3x -run '^$'`
Expected: 输出 Upload 和 Download 各一行，含 MB/s。记录数据，与修复前对比（修复前可 git stash 改动跑一次 baseline）。

> 注：loopback 场景下提速比例不显著（RTT 接近 0，串行 vs pipelining 差距小）。真实跨地域场景的提速需手工验证——可在两个跨地域机器间跑 upload/download 对比专业 sftp 工具。

- [ ] **Step 4: Commit**

```bash
git add internal/ssh/pty/sftp_bench_test.go internal/ssh/pty/test_helpers.go  # 若改了 helper 签名
git commit -m "test(pty): add sftp upload/download benchmarks"
```

---

## Self-Review 检查清单

实施完成后逐项确认：

- [ ] **Part A 全部测试通过**：`go test ./internal/ssh/session/ -v` 全绿
- [ ] **Part B 全部测试通过**：`go test ./internal/ssh/pty/ ./internal/ssh/conn/ -v` 全绿
- [ ] **现有 sftp 行为契约不变**：`TestUploadNormalPath` / `TestDownloadNormalPath` / `TestUploadTimeout` / `TestDownloadTimeout` 全过
- [ ] **copyCtx 已删除**：`grep -n copyCtx internal/ssh/pty/sftp.go` 无输出
- [ ] **vet 无警告**：`go vet ./...`
- [ ] **设计文档同步**：把 §3.7 "命令执行期间不算空闲" 改为 "命令执行与文件传输期间不算空闲"（可选，本计划不强制）
- [ ] **commit 历史清晰**：6 个 commit，每个对应一个 Task，message 以 `fix(session):` 或 `perf(pty):` / `perf(conn):` / `test(...):` 开头

## 已知风险与权衡

1. **`TestUploadTimeout` / `TestDownloadTimeout` 可能 flaky**：io.Copy 改造后，超时解除依赖 `context.AfterFunc` 关 sftp.File，慢 reader/writer 测试中 io.Copy 可能正在 src.Read / dst.Write 的 sleep 中，AfterFunc 关 sftp.File 后需等当前 chunk sleep 完成才能看到错误。若测试 flaky，把 100ms timeout 调到 150ms 缓冲，但优先保持原参数。

2. **loopback benchmark 不反映真实提速**：fake sftp server 走 loopback，RTT 接近 0，pipelining 优势体现不出来。Benchmark 主要防回归。真实场景需跨地域手工验证——预期 50ms RTT 下从 640 KB/s 提到 5-10 MB/s 量级。

3. **`context.AfterFunc` + 关 sftp.File 的并发安全**：依赖 `pkg/sftp` 的 `File.Close` 内部 mutex 保护。若 pkg/sftp 版本较旧不支持并发 Close + Write，需升级库或改用 `Client.Close()`（更激进，会断整个 sftp 通道）。当前项目 `go.mod` 用的 `github.com/pkg/sftp` 版本需在实施时确认 ≥ v1.13.0（v1.13+ 的 File.Close 是并发安全的）。

4. **sftp 错误不再触发 session Close**：与 `RunInSession` 的 `connUnusable` 路径不同——sftp 失败只回 Idle，不 Close。理由：sftp 通道是独立 SSH channel，sftp 失败不代表 PTY channel 也死。下次 op 自然失败检测 conn 死亡。若后续发现 sftp 失败后 PTY 也跟着死的场景，再加 `connUnusable` 等价路径。
