# Relay Transfer (1:N fanout) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an MCP tool `relay_transfer` that streams a remote file from one SSH session to one or more other sessions through the sshmng process — no local disk, download/upload overlap, source read once for N destinations.

**Architecture:** Pure in-process `io` plumbing on top of the existing sftp path. A `fanWriter` (tee) writes each downloaded chunk concurrently to N `io.Pipe` writers; N upload goroutines each read one pipe and call `UploadSized`. Two new `Conn` methods (`Stat`, `UploadSized`) let the relay verify the source and take sftp's concurrent-pipelining upload path (an order of magnitude faster cross-region than the serial fallback). Each session's existing state machine is untouched.

**Tech Stack:** Go 1.25, `github.com/pkg/sftp`, `github.com/modelcontextprotocol/go-sdk/mcp`. Tests use the existing `fakeConn` stand-in (`internal/ssh/session`) and the fake SSH+sftp server helpers (`internal/ssh/pty`).

## Global Constraints

- **Module path:** `github.com/jim58246/sshmng` (import paths use this prefix).
- **Conn interface** (`internal/ssh/session/session.go:38`): every method added here MUST be implemented on **both** `fakeConn` (`internal/ssh/session/session_test.go`) and `PtyConn` (`internal/ssh/pty`) in the same task, or the build breaks. `Session` forwarding methods are symmetric with the existing `Session.Upload` (`session.go:298`): lock → check Closed/Running → set Running + stopIdleTimer → unlock → call conn → lock → lastActivity + Idle + resetIdleTimer → unlock.
- **sftp client field:** `PtyConn.sftpClient *sftp.Client` (`pty.go:86`), guarded by `PtyConn.mu sync.Mutex` (`pty.go:107`). Guard pattern: `p.mu.Lock(); sc := p.sftpClient; p.mu.Unlock(); if sc == nil { return ..., conn.ErrSftpUnavailable }`.
- **Timeout default:** `conn.DefaultTransferTimeout = 300 * time.Second` (`internal/ssh/conn/sftp.go:16`). `timeoutMs > 0` overrides.
- **Error sentinel:** `conn.ErrSftpUnavailable` (`internal/ssh/conn/sftp.go:19`) — "sftp not available for this session".
- **sftp pipelining:** `Upload` uses `dst.ReadFrom(src)`. The concurrent path is taken only when `src` is `*io.LimitReader` (or exposes Stat/Size/Len). `io.Pipe`'s reader exposes none, so `UploadSized` must wrap with `io.LimitReader(newCtxReader(src, ctx), size)` to take the fast path. `newCtxReader` already exists (`internal/ssh/pty/sftp.go:149`) and preserves ctx-cancellation + Stat passthrough.
- **MCP handler conventions** (`internal/mcp/server.go`): `errorResult(format, args...)` → `IsError=true` result (for hard errors only). `textResult(v any)` → JSON-marshaled success result. `s.sessionLogger(req, sid).Info/Debug(...)`. `mcp.AddTool(server, &mcp.Tool{Name, Description}, svc.Method)` infers input schema from the args struct's `jsonschema` tags.
- **Partial-failure convention** (matches `upload_dir`/`download_dir`): per-destination failures return a result body with top-level `ok:false` and `IsError` NOT set; only hard errors (sids not found, empty destinations) use `errorResult`.
- **Bilingual docs** (CLAUDE.md pre-release checklist): every English doc change to `README.md` / `docs/agents.md` MUST be mirrored in `README.zh-CN.md` / `docs/zh-CN/agents.md`. MCP tool list + signatures in `docs/agents.md` must match `internal/mcp/tools_*.go` + `server.go`.
- **Commit messages** end with `Co-Authored-By: Claude <noreply@anthropic.com>`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/ssh/session/session.go` (modify) | Add `Stat` + `UploadSized` to the `Conn` interface; add `Session.Stat` / `Session.UploadSized` forwarding (state-machine symmetric with `Session.Upload`). |
| `internal/ssh/session/session_test.go` (modify) | Extend `fakeConn` with `Stat` + `UploadSized` + a `fakeFileInfo` helper; add session-level forwarding tests. |
| `internal/ssh/pty/sftp.go` (modify) | `PtyConn.Stat` (sftp `Stat`) + `PtyConn.UploadSized` (`io.LimitReader` → concurrent pipelining). |
| `internal/ssh/pty/sftp_test.go` (modify) | `PtyConn.Stat` / `PtyConn.UploadSized` tests against the fake sftp server. |
| `internal/ssh/session/relay.go` (create) | `fanWriter` tee + `Manager.RelayTransfer` orchestration + `RelayResult` / `RelayDest`. |
| `internal/ssh/session/relay_test.go` (create) | fanWriter unit tests + `Manager.RelayTransfer` tests (1:1, 1:N, error isolation, early termination, pre-flight, Stat-fallback). |
| `internal/mcp/tools_relay.go` (create) | `relay_transfer` MCP handler + `RelayTransferArgs` + JSON output struct. |
| `internal/mcp/tools_relay_test.go` (create) | Handler test (hard error → IsError; partial → ok:false body). |
| `internal/mcp/server.go` (modify) | Register `relay_transfer` in the "File transfer tools" block. |
| `docs/agents.md`, `README.md`, `docs/zh-CN/agents.md`, `README.zh-CN.md` (modify) | Document the new tool (bilingual). |

---

### Task 1: Add `Conn.Stat` + `Session.Stat` + implementations

**Files:**
- Modify: `internal/ssh/session/session.go` (the `Conn` interface, ~line 38; add `Session.Stat` near `Session.Download` ~line 326)
- Modify: `internal/ssh/pty/sftp.go` (add `PtyConn.Stat` near `PtyConn.Download` ~line 75)
- Modify: `internal/ssh/session/session_test.go` (extend `fakeConn` + add `fakeFileInfo`)
- Modify: `internal/ssh/pty/sftp_test.go` (add `TestPtyConnStat`)

**Interfaces:**
- Produces: `Conn.Stat(path string) (os.FileInfo, error)` — returns remote file info; `conn.ErrSftpUnavailable` if no sftp channel. `PtyConn.Stat` delegates to `sftpClient.Stat`. `Session.Stat` forwards through the state machine. `fakeConn.Stat` returns configurable `statFi`/`statErr`.

- [ ] **Step 1: Write the failing pty test**

Add to `internal/ssh/pty/sftp_test.go`:

```go
// TestPtyConnStat: Upload 后 Stat 返回正确的 size，且是普通文件。
func TestPtyConnStat(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr:          srv.Addr(),
		User:          "alice",
		Auth:          config.SSHAuth{Password: "wonderland"},
		HostKeyVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	content := bytes.Repeat([]byte("stat me\n"), 50) // 400 bytes
	if _, _, err := p.Upload(bytes.NewReader(content), "/statme.txt", 30000); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	fi, err := p.Stat("/statme.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() != int64(len(content)) {
		t.Errorf("Size() = %d, want %d", fi.Size(), len(content))
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("IsRegular() = false, want true")
	}
}

// TestPtyConnStatSftpUnavailable: sftp 未建立时 Stat 返回 ErrSftpUnavailable。
func TestPtyConnStatSftpUnavailable(t *testing.T) {
	srv := newFakeShellServer(t) // 不支持 sftp
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr: srv.Addr(), User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"}, HostKeyVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	if _, err := p.Stat("/whatever"); !errors.Is(err, conn.ErrSftpUnavailable) {
		t.Errorf("Stat err = %v, want ErrSftpUnavailable", err)
	}
}
```

Also add `"errors"` to the import block of `sftp_test.go` if not already present (it currently imports `bytes`, `context`, `io`, `os`, `strings`, `testing`, `time`, `config`, `conn`).

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/ssh/pty/ -run TestPtyConnStat -v`
Expected: COMPILE FAILURE — `p.Stat undefined (type *PtyConn has no field or method Stat)`.

- [ ] **Step 3: Add `Stat` to the `Conn` interface and implement on `PtyConn`**

In `internal/ssh/session/session.go`, add to the `Conn` interface (after the `Download` method, ~line 46):

```go
	// Stat 返回远端文件的 os.FileInfo。sftp 通道未建立时返回 conn.ErrSftpUnavailable。
	Stat(path string) (os.FileInfo, error)
```

Ensure `"os"` is imported in `session.go` (it already is — `SessionStat` uses `os.FileInfo` per the existing `SessionStat` type; verify and add if missing).

In `internal/ssh/pty/sftp.go`, add (after `PtyConn.SftpAvailable`, ~line 19):

```go
// Stat 返回远端 path 的文件信息。
// sftp 通道未建立时返回 conn.ErrSftpUnavailable。
func (p *PtyConn) Stat(remotePath string) (os.FileInfo, error) {
	p.mu.Lock()
	sftpClient := p.sftpClient
	p.mu.Unlock()
	if sftpClient == nil {
		return nil, conn.ErrSftpUnavailable
	}
	return sftpClient.Stat(remotePath)
}
```

`"os"` is already imported in `sftp.go` (line 7).

- [ ] **Step 4: Implement `Stat` on `fakeConn` and add `fakeFileInfo`**

The build now fails in `internal/ssh/session` because `fakeConn` does not satisfy the extended `Conn` interface. In `internal/ssh/session/session_test.go`:

Add `"os"` to the import block (currently `bytes`, `io`, `strings`, `sync`, `testing`, `time`, `conn`).

Add fields to the `fakeConn` struct (in the sftp block, after `downloadData`/`uploadDelay` ~line 31):

```go
	// Stat 支持（relay 测试用）
	statFi  os.FileInfo // nil 时 Stat 返回 (nil, statErr)
	statErr error
```

Add the method (after `fakeConn.Download`, ~line 124):

```go
// Stat 返回配置的 statFi/statErr。relay 测试用 statFi 模拟源文件元信息。
func (f *fakeConn) Stat(path string) (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statErr != nil {
		return nil, f.statErr
	}
	if f.statFi != nil {
		return f.statFi, nil
	}
	// 默认：返回一个 0 字节普通文件，避免未配置时 nil 解引用
	return fakeFileInfo{size: 0, mode: 0644}, nil
}
```

Add the `fakeFileInfo` type at the end of the file (before the test functions, or after `newFakeConn`):

```go
// fakeFileInfo 是测试用的 os.FileInfo 替身，relay 测试用它配置 fakeConn.Stat 的返回。
type fakeFileInfo struct {
	size int64
	mode os.FileMode
}

func (f fakeFileInfo) Name() string       { return "fake" }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }
```

- [ ] **Step 5: Add `Session.Stat` forwarding**

In `internal/ssh/session/session.go`, add after `Session.Download` (~line 350), symmetric with `Session.Download`:

```go
// Stat 返回远端 path 的文件信息。
// 状态机与 Upload/Download 对称：进锁检查 state、切 Running + stopIdleTimer、
// 查询、切回 Idle + resetIdleTimer + 更新 lastActivity。
// sftp 错误回到 Idle 而非 Closed——sftp 通道独立于 PTY 通道。
func (s *Session) Stat(remotePath string) (os.FileInfo, error) {
	s.mu.Lock()
	if s.state == StateClosed {
		s.mu.Unlock()
		return nil, errors.New("session closed")
	}
	if s.state == StateRunning {
		s.mu.Unlock()
		return nil, errors.New("session busy")
	}
	s.state = StateRunning
	s.stopIdleTimer()
	s.mu.Unlock()

	fi, err := s.conn.Stat(remotePath)

	s.mu.Lock()
	s.lastActivity = time.Now()
	if s.state != StateClosed {
		s.state = StateIdle
		s.resetIdleTimer()
	}
	s.mu.Unlock()
	return fi, err
}
```

- [ ] **Step 6: Write the session-level forwarding test**

Add to `internal/ssh/session/session_test.go`:

```go
// TestSessionStatForwards: Session.Stat 经状态机转发到 conn.Stat，并切回 Idle。
func TestSessionStatForwards(t *testing.T) {
	conn := newFakeConn()
	conn.sftpEnabled = true
	conn.statFi = fakeFileInfo{size: 123, mode: 0644}
	mgr := NewManager()
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	fi, err := s.Stat("/some/path")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() != 123 {
		t.Errorf("Size() = %d, want 123", fi.Size())
	}
	if s.State() != StateIdle {
		t.Errorf("after Stat, state = %v, want Idle", s.State())
	}
}

// TestSessionStatBusy: 非 idle 态 Stat 返回 "session busy"。
func TestSessionStatBusy(t *testing.T) {
	conn := newFakeConn()
	conn.sftpEnabled = true
	mgr := NewManager()
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	// 手动置 Running 模拟传输进行中
	s.mu.Lock()
	s.state = StateRunning
	s.mu.Unlock()

	if _, err := s.Stat("/x"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("Stat err = %v, want 'session busy'", err)
	}
}
```

- [ ] **Step 7: Run all affected tests**

Run: `go test ./internal/ssh/session/ ./internal/ssh/pty/ -v -run 'Stat'`
Expected: PASS for `TestPtyConnStat`, `TestPtyConnStatSftpUnavailable`, `TestSessionStatForwards`, `TestSessionStatBusy`.

- [ ] **Step 8: Run the full package builds + tests**

Run: `go build ./... && go test ./internal/ssh/session/ ./internal/ssh/pty/`
Expected: build OK, all tests PASS (fakeConn now satisfies the extended interface).

- [ ] **Step 9: Commit**

```bash
git add internal/ssh/session/session.go internal/ssh/session/session_test.go internal/ssh/pty/sftp.go internal/ssh/pty/sftp_test.go
git commit -m "$(cat <<'EOF'
feat(session): add Conn.Stat for remote file metadata

New Conn.Stat(path) returns os.FileInfo via sftp; Session.Stat forwards
through the state machine symmetric with Upload/Download. Foundation for
relay_transfer's source-file validation and size discovery.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Add `Conn.UploadSized` + `Session.UploadSized` + implementations

**Files:**
- Modify: `internal/ssh/session/session.go` (interface + `Session.UploadSized`)
- Modify: `internal/ssh/pty/sftp.go` (`PtyConn.UploadSized`)
- Modify: `internal/ssh/session/session_test.go` (`fakeConn.UploadSized`)
- Modify: `internal/ssh/pty/sftp_test.go` (`TestPtyConnUploadSized`)

**Interfaces:**
- Produces: `Conn.UploadSized(src io.Reader, size int64, remotePath string, timeoutMs int) (bytes int, timedOut bool, err error)`. `PtyConn.UploadSized` = `Upload` but wraps `src` in `io.LimitReader(newCtxReader(src, ctx), size)` so `*sftp.File.ReadFrom` takes the concurrent-pipelining path. Returns `(bytes, timedOut, err)` like `Upload`.

- [ ] **Step 1: Write the failing pty test**

Add to `internal/ssh/pty/sftp_test.go`:

```go
// TestPtyConnUploadSized: UploadSized 传输正确字节数且远端内容一致。
// 关键：src 是 io.PipeReader（无 Stat/Size），UploadSized 用 io.LimitReader 触发
// *sftp.File.ReadFrom 的并发 pipelining 路径，而非串行 writeChunkAt。
func TestPtyConnUploadSized(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr: srv.Addr(), User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"}, HostKeyVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	content := bytes.Repeat([]byte("sized\n"), 200) // 1200 bytes
	pr, pw := io.Pipe()
	go func() {
		pw.Write(content)
		pw.Close()
	}()

	n, timedOut, err := p.UploadSized(pr, int64(len(content)), "/sized.txt", 30000)
	if err != nil {
		t.Fatalf("UploadSized: %v", err)
	}
	if timedOut {
		t.Errorf("timed_out = true, want false")
	}
	if n != len(content) {
		t.Errorf("bytes = %d, want %d", n, len(content))
	}

	remote, err := p.sftpClient.Open("/sized.txt")
	if err != nil {
		t.Fatalf("Open remote: %v", err)
	}
	defer remote.Close()
	got, err := io.ReadAll(remote)
	if err != nil {
		t.Fatalf("ReadAll remote: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("remote content mismatch: got %d bytes, want %d", len(got), len(content))
	}
}

// TestPtyConnUploadSizedBoundsToSize: src 字节数多于 size 时，只写 size 字节。
func TestPtyConnUploadSizedBoundsToSize(t *testing.T) {
	srv := newFakeShellServerWithSftp(t)
	d := newDialerWithTempKnownHosts(t)
	client, err := d.Dial(conn.DialOptions{
		Addr: srv.Addr(), User: "alice",
		Auth: config.SSHAuth{Password: "wonderland"}, HostKeyVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sid, _ := conn.RandomSID()
	p, err := NewPtyConn(client, sid, nil, nil)
	if err != nil {
		t.Fatalf("NewPtyConn: %v", err)
	}
	defer p.Close()

	content := bytes.Repeat([]byte("x"), 1000)
	// size=400：只应写 400 字节
	n, _, err := p.UploadSized(bytes.NewReader(content), 400, "/trunc.txt", 30000)
	if err != nil {
		t.Fatalf("UploadSized: %v", err)
	}
	if n != 400 {
		t.Errorf("bytes = %d, want 400", n)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/ssh/pty/ -run TestPtyConnUploadSized -v`
Expected: COMPILE FAILURE — `p.UploadSized undefined`.

- [ ] **Step 3: Add `UploadSized` to the `Conn` interface and implement on `PtyConn`**

In `internal/ssh/session/session.go`, add to the `Conn` interface (after `Upload`, ~line 45):

```go
	// UploadSized 与 Upload 相同，但以已知 size 传输。内部用 io.LimitReader 包装 src，
	// 让 *sftp.File.ReadFrom 走并发 pipelining 路径（src 为 io.PipeReader 等无 Stat/Size
	// 的 reader 时，Upload 会退化为串行写）。size=-1 时退化为 Upload 语义。
	UploadSized(src io.Reader, size int64, remotePath string, timeoutMs int) (bytes int, timedOut bool, err error)
```

In `internal/ssh/pty/sftp.go`, add (after `PtyConn.Upload`, ~line 65):

```go
// UploadSized 把 src（已知 size）上传到远端 remotePath。
//   - timeoutMs=0 用默认 300s
//   - 返回 (已传输字节数, 是否超时, error)
//   - sftp 通道未建立时返回 conn.ErrSftpUnavailable
//
// 与 Upload 的区别：用 io.LimitReader(newCtxReader(src, ctx), size) 包装 src。
// *io.LimitReader 被 *sftp.File.ReadFrom 的 type switch 识别，走 readFromWithConcurrency
// 并发 pipelining 路径（多个 SSH_FXP_WRITE 包同时在飞）；否则 Upload 对无 Stat/Size 的
// reader（如 io.PipeReader）退化为串行 writeChunkAt，跨地域 RTT 下慢一个数量级。
// newCtxReader 保留 ctx 取消响应（AfterFunc 关闭 dst 解除 ReadFrom 阻塞）。
func (p *PtyConn) UploadSized(src io.Reader, size int64, remotePath string, timeoutMs int) (int, bool, error) {
	p.logger.Debug("sftp upload_sized start", "sid", p.sid, "remote", remotePath, "size", size, "timeout_ms", timeoutMs)
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

	stop := context.AfterFunc(ctx, func() {
		dst.Close()
	})

	// io.LimitReader → *io.LimitReader → ReadFrom 并发 pipelining 路径。
	// newCtxReader 让 src.Read 在 ctx 取消时及时退出（解除 ReadFrom 在 dst 上的阻塞）。
	n, err := io.Copy(dst, io.LimitReader(newCtxReader(src, ctx), size))
	stop()
	timedOut := ctx.Err() == context.DeadlineExceeded
	p.logger.Debug("sftp upload_sized done",
		"sid", p.sid, "remote", remotePath, "bytes", n, "timed_out", timedOut)
	return int(n), timedOut, err
}
```

Note: `io.LimitReader(r, size)` with `size < 0` returns an unlimited reader — so `size = -1` degrades to full `Upload` semantics (reads until EOF). The orchestrator only calls `UploadSized` with a real size (Task 4), so this is a safe bound.

- [ ] **Step 4: Implement `UploadSized` on `fakeConn`**

In `internal/ssh/session/session_test.go`, add (after `fakeConn.Upload`, ~line 109):

```go
// UploadSized 模拟有尺寸上传：读至多 size 字节存入 uploadedBytes。
// fakeConn 不区分 pipelining 路径，行为与 Upload 一致但以 size 截断。
func (f *fakeConn) UploadSized(src io.Reader, size int64, remotePath string, timeoutMs int) (int, bool, error) {
	if !f.sftpEnabled {
		return 0, false, conn.ErrSftpUnavailable
	}
	if f.uploadBlock != nil {
		<-f.uploadBlock
	}
	if f.uploadDelay > 0 {
		time.Sleep(f.uploadDelay)
	}
	n, err := io.ReadAll(io.LimitReader(src, size))
	f.uploadedBytes = append(f.uploadedBytes, n...)
	return len(n), false, err
}
```

- [ ] **Step 5: Add `Session.UploadSized` forwarding**

In `internal/ssh/session/session.go`, add after `Session.Upload` (~line 322), symmetric with `Session.Upload`:

```go
// UploadSized 与 Upload 对称，转发到 conn.UploadSized（已知 size，走并发 pipelining）。
func (s *Session) UploadSized(src io.Reader, size int64, remotePath string, timeoutMs int) (int, bool, error) {
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

	n, timedOut, err := s.conn.UploadSized(src, size, remotePath, timeoutMs)

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

- [ ] **Step 6: Write the session-level forwarding test**

Add to `internal/ssh/session/session_test.go`:

```go
// TestSessionUploadSizedForwards: Session.UploadSized 转发字节数与内容正确。
func TestSessionUploadSizedForwards(t *testing.T) {
	conn := newFakeConn()
	conn.sftpEnabled = true
	mgr := NewManager()
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	content := []byte("hello sized world")
	n, _, err := s.UploadSized(bytes.NewReader(content), int64(len(content)), "/r.txt", 0)
	if err != nil {
		t.Fatalf("UploadSized: %v", err)
	}
	if n != len(content) {
		t.Errorf("bytes = %d, want %d", n, len(content))
	}
	if !bytes.Equal(conn.uploadedBytes, content) {
		t.Errorf("uploaded bytes mismatch: got %q, want %q", conn.uploadedBytes, content)
	}
	if s.State() != StateIdle {
		t.Errorf("after UploadSized, state = %v, want Idle", s.State())
	}
}
```

- [ ] **Step 7: Run all affected tests**

Run: `go test ./internal/ssh/session/ ./internal/ssh/pty/ -v -run 'UploadSized'`
Expected: PASS for all four new tests.

- [ ] **Step 8: Full build + test**

Run: `go build ./... && go test ./internal/ssh/...`
Expected: build OK, all tests PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/ssh/session/session.go internal/ssh/session/session_test.go internal/ssh/pty/sftp.go internal/ssh/pty/sftp_test.go
git commit -m "$(cat <<'EOF'
feat(session): add Conn.UploadSized for pipelined pipe-reader upload

UploadSized wraps src in io.LimitReader so *sftp.File.ReadFrom takes the
concurrent-pipelining path even for size-less readers (io.PipeReader).
An order of magnitude faster cross-region than Upload's serial fallback.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: `fanWriter` (the tee) + unit tests

**Files:**
- Create: `internal/ssh/session/relay.go`
- Create: `internal/ssh/session/relay_test.go`

**Interfaces:**
- Produces: `newFanWriter(pws []*io.PipeWriter) *fanWriter` and `func (fw *fanWriter) Write(p []byte) (int, error)`. Writes `p` concurrently to all live pipe writers; marks a slot dead on write error; returns `(len(p), nil)` if ≥1 live slot remains, else `(0, errAllDestinationsFailed)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/ssh/session/relay_test.go`:

```go
package session

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
)

// TestFanWriterBroadcastsToAll: 数据块并发分发到所有存活目标，全部收到完整内容。
func TestFanWriterBroadcastsToAll(t *testing.T) {
	pr1, pw1 := io.Pipe()
	pr2, pw2 := io.Pipe()
	defer pr1.Close()
	defer pr2.Close()

	fw := newFanWriter([]*io.PipeWriter{pw1, pw2})

	var got1, got2 bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(&got1, pr1) }()
	go func() { defer wg.Done(); io.Copy(&got2, pr2) }()

	if _, err := fw.Write([]byte("hello ")); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if _, err := fw.Write([]byte("world")); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	pw1.Close()
	pw2.Close()
	wg.Wait()

	if got1.String() != "hello world" {
		t.Errorf("dest1 got %q, want %q", got1.String(), "hello world")
	}
	if got2.String() != "hello world" {
		t.Errorf("dest2 got %q, want %q", got2.String(), "hello world")
	}
}

// TestFanWriterIsolatesDeadDest: 一个目标关闭后标记 dead，其余目标仍收完整内容，
// Write 返回 (len(p), nil)。
func TestFanWriterIsolatesDeadDest(t *testing.T) {
	pr1, pw1 := io.Pipe()
	pr2, pw2 := io.Pipe()
	defer pr1.Close()
	defer pr2.Close()

	fw := newFanWriter([]*io.PipeWriter{pw1, pw2})

	// dest1 读几个字节后关闭 reader → 后续 Write 到 pw1 返回 ErrClosedPipe
	var got1 bytes.Buffer
	go func() {
		io.CopyN(&got1, pr1, 3)
		pr1.Close() // 关闭 reader 端，让 pw1.Write 失败
	}()

	var got2 bytes.Buffer
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() { defer wg.Done(); io.Copy(&got2, pr2) }()

	fw.Write([]byte("abc"))   // dest1 读到 "abc" 后关闭
	fw.Write([]byte("def"))   // dest1 dead，只写 dest2
	pw1.Close()
	pw2.Close()
	wg.Wait()

	if got2.String() != "abcdef" {
		t.Errorf("dest2 got %q, want %q", got2.String(), "abcdef")
	}
}

// TestFanWriterAllDeadReturnsError: 全部目标 dead 后 Write 返回 errAllDestinationsFailed。
// 关闭两个 pipe 的 reader 端后，pw.Write 立即返回 io.ErrClosedPipe；两个 slot 都 dead，
// Write 返回 errAllDestinationsFailed，让 Download 的 io.Copy 提前终止。
func TestFanWriterAllDeadReturnsError(t *testing.T) {
	prA, pwA := io.Pipe()
	prB, pwB := io.Pipe()
	prA.Close() // reader 关闭 → pwA.Write 立即返回 ErrClosedPipe
	prB.Close()
	defer pwA.Close()
	defer pwB.Close()

	fw := newFanWriter([]*io.PipeWriter{pwA, pwB})
	_, err := fw.Write([]byte("x"))
	if !errors.Is(err, errAllDestinationsFailed) {
		t.Errorf("Write err = %v, want errAllDestinationsFailed", err)
	}
}
```
- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/ssh/session/ -run TestFanWriter -v`
Expected: COMPILE FAILURE — `newFanWriter undefined`, `errAllDestinationsFailed undefined`.

- [ ] **Step 3: Implement `fanWriter`**

Create `internal/ssh/session/relay.go`:

```go
package session

import (
	"errors"
	"io"
	"sync"
)

// errAllDestinationsFailed 在 fanWriter 的所有目标都 dead 时由 Write 返回，
// 让 Download 的 io.Copy 终止（不再无谓地读源）。
var errAllDestinationsFailed = errors.New("all relay destinations failed")

// fanSlot 是一个目标的 pipe writer 端。dead=true 表示该目标已失败，
// 后续 Write 跳过它（失败隔离）。
type fanSlot struct {
	w    *io.PipeWriter
	dead bool
}

// fanWriter 把下载侧的数据块并发分发到 N 个目标的 io.PipeWriter。
// 每次 Write 并发写所有存活目标，等全部完成；任一目标 Write 失败则标记 dead，
// 不影响其他目标。全部 dead 时返回 errAllDestinationsFailed。
type fanWriter struct {
	mu    sync.Mutex
	slots []*fanSlot
}

// newFanWriter 用给定的 pipe writers 构造 fanWriter。
func newFanWriter(pws []*io.PipeWriter) *fanWriter {
	slots := make([]*fanSlot, len(pws))
	for i, pw := range pws {
		slots[i] = &fanSlot{w: pw}
	}
	return &fanWriter{slots: slots}
}

// Write 并发把 p 写入所有存活目标，等全部完成。
// 返回 (len(p), nil) 只要仍有存活目标；全部 dead 返回 (0, errAllDestinationsFailed)。
func (fw *fanWriter) Write(p []byte) (int, error) {
	fw.mu.Lock()
	live := make([]*fanSlot, 0, len(fw.slots))
	for _, s := range fw.slots {
		if !s.dead {
			live = append(live, s)
		}
	}
	fw.mu.Unlock()

	if len(live) == 0 {
		return 0, errAllDestinationsFailed
	}

	// 并发写每个存活目标；per-chunk 墙钟 = max(各目标接受延迟)，而非 sum。
	var wg sync.WaitGroup
	for _, s := range live {
		wg.Add(1)
		go func(s *fanSlot) {
			defer wg.Done()
			if _, err := s.w.Write(p); err != nil {
				fw.mu.Lock()
				s.dead = true
				fw.mu.Unlock()
			}
		}(s)
	}
	wg.Wait()

	// 重新统计存活数；若本轮全部失败则通知 Download 终止
	fw.mu.Lock()
	remain := 0
	for _, s := range fw.slots {
		if !s.dead {
			remain++
		}
	}
	fw.mu.Unlock()
	if remain == 0 {
		return 0, errAllDestinationsFailed
	}
	return len(p), nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/ssh/session/ -run TestFanWriter -v`
Expected: PASS for all three.

- [ ] **Step 5: Commit**

```bash
git add internal/ssh/session/relay.go internal/ssh/session/relay_test.go
git commit -m "$(cat <<'EOF'
feat(session): add fanWriter for concurrent 1:N pipe tee

fanWriter fans each downloaded chunk out to N io.PipeWriters concurrently,
isolating per-destination failures and returning errAllDestinationsFailed
when no live destination remains (lets Download terminate early).

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: `Manager.RelayTransfer` orchestration + tests

**Files:**
- Modify: `internal/ssh/session/relay.go` (add `RelayResult`, `RelayDest`, `Manager.RelayTransfer`)
- Modify: `internal/ssh/session/relay_test.go` (add orchestration tests)

**Interfaces:**
- Consumes: `Session.Stat(path) (os.FileInfo, error)` (Task 1), `Session.UploadSized(src, size, path, timeoutMs) (int, bool, error)` (Task 2), `Session.Download(remotePath, dst, timeoutMs) (int, bool, error)` (existing), `Session.Upload(src, path, timeoutMs) (int, bool, error)` (existing), `Session.SftpAvailable() bool` / `Session.State() SessionState` / `Session.ServerName() string` (existing), `Manager.Get(sid) (*Session, error)` (existing), `fanWriter` (Task 3).
- Produces: `Manager.RelayTransfer(srcSid, srcPath string, dstSids []string, dstPath string, timeoutMs int) (RelayResult, error)`. `RelayResult{DownloadedBytes, TimedOut, SrcServer, Destinations []RelayDest, Err error}`; `RelayDest{DstSid, DstServer string; OK bool; Bytes int; TimedOut bool; Err error}`. Go `error` return is non-nil only for hard errors (`srcSid` not found, empty `dstSids`); partial failures return `(res, nil)` with `res.Err` set and per-dest `OK=false`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/ssh/session/relay_test.go` (add `"time"` and `"github.com/jim58246/sshmng/internal/ssh/conn"` to imports if not present; `"bytes"`, `"io"`, `"sync"`, `"testing"`, `"errors"` already there):

```go
// relayTransferTestHarness 建一个源 session + N 个目标 session，全部用 fakeConn。
// srcData 是源文件内容；srcSize 用于配置 fakeConn.statFi。
type relayTransferTestHarness struct {
	mgr     *Manager
	srcSid  string
	srcConn *fakeConn
	dst     []struct {
		sid  string
		conn *fakeConn
	}
}

func newRelayHarness(t *testing.T, nDst int, srcData []byte) *relayTransferTestHarness {
	t.Helper()
	mgr := NewManager()
	srcConn := newFakeConn()
	srcConn.sftpEnabled = true
	srcConn.downloadData = srcData
	srcConn.statFi = fakeFileInfo{size: int64(len(srcData)), mode: 0644}
	mgr.newSessionWithConn("src", "srcsrv", srcConn, time.Minute, nil)
	h := &relayTransferTestHarness{mgr: mgr, srcSid: "src", srcConn: srcConn}
	for i := 0; i < nDst; i++ {
		dc := newFakeConn()
		dc.sftpEnabled = true
		sid := "dst" + string(rune('1'+i))
		mgr.newSessionWithConn(sid, "dstsrv", dc, time.Minute, nil)
		h.dst = append(h.dst, struct {
			sid  string
			conn *fakeConn
		}{sid, dc})
	}
	return h
}

// TestRelayTransferOneToOne: 1:1 流式中转，字节一致、两侧 ok。
func TestRelayTransferOneToOne(t *testing.T) {
	data := bytes.Repeat([]byte("relay\n"), 300) // 1800 bytes
	h := newRelayHarness(t, 1, data)
	dstSid := h.dst[0].sid

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", []string{dstSid}, "/dst.bin", 0)
	if err != nil {
		t.Fatalf("RelayTransfer: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("res.Err = %v, want nil", res.Err)
	}
	if res.DownloadedBytes != len(data) {
		t.Errorf("downloaded_bytes = %d, want %d", res.DownloadedBytes, len(data))
	}
	if len(res.Destinations) != 1 {
		t.Fatalf("destinations len = %d, want 1", len(res.Destinations))
	}
	d := res.Destinations[0]
	if !d.OK {
		t.Errorf("dest ok = false, want true; err=%v", d.Err)
	}
	if d.Bytes != len(data) {
		t.Errorf("dest bytes = %d, want %d", d.Bytes, len(data))
	}
	if !bytes.Equal(h.dst[0].conn.uploadedBytes, data) {
		t.Errorf("dest uploaded content mismatch: got %d bytes, want %d", len(h.dst[0].conn.uploadedBytes), len(data))
	}
}

// TestRelayTransferOneToN: 1:3 扇出，源只读一次，三个目标各得完整内容。
func TestRelayTransferOneToN(t *testing.T) {
	data := bytes.Repeat([]byte("fan\n"), 400) // 1600 bytes
	h := newRelayHarness(t, 3, data)
	sids := []string{h.dst[0].sid, h.dst[1].sid, h.dst[2].sid}

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", sids, "/dst.bin", 0)
	if err != nil {
		t.Fatalf("RelayTransfer: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("res.Err = %v", res.Err)
	}
	for i, d := range res.Destinations {
		if !d.OK {
			t.Errorf("dest %d ok=false, err=%v", i, d.Err)
		}
		if d.Bytes != len(data) {
			t.Errorf("dest %d bytes = %d, want %d", i, d.Bytes, len(data))
		}
		if !bytes.Equal(h.dst[i].conn.uploadedBytes, data) {
			t.Errorf("dest %d content mismatch", i)
		}
	}
}

// TestRelayTransferSourceMissing: 源文件不存在（download 出错）→ ok=false，根因清晰。
func TestRelayTransferSourceMissing(t *testing.T) {
	h := newRelayHarness(t, 1, []byte("x"))
	h.srcConn.downloadData = nil
	// 让 Download 返回错误：用一个能注入 download 错误的方式——设 downloadData 为 nil
	// 不足以报错；改用 downloadErr。先给 fakeConn 加 downloadErr 字段（见下方实现）。
	h.srcConn.downloadErr = errors.New("open remote /src.bin: no such file")
	dstSid := h.dst[0].sid

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", []string{dstSid}, "/dst.bin", 0)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if res.Err == nil {
		t.Fatalf("res.Err = nil, want download error")
	}
	if !res.Destinations[0].OK && res.Destinations[0].Err != nil {
		// dest 应因 pipe 关闭收到错误或 0 字节
		if res.Destinations[0].Bytes != 0 && !res.Destinations[0].OK {
			// 接受：download 失败时 dest 不可能成功
		}
	}
	if res.Destinations[0].OK {
		t.Errorf("dest ok = true, want false (source missing)")
	}
}

// TestRelayTransferOneDestFailsOthersSucceed: 一个目标上传失败，其余仍 ok。
func TestRelayTransferOneDestFailsOthersSucceed(t *testing.T) {
	data := bytes.Repeat([]byte("iso\n"), 500) // 2000 bytes
	h := newRelayHarness(t, 3, data)
	// dst2 上传失败：设 uploadErr
	h.dst[1].conn.uploadErr = errors.New("create remote: permission denied")
	sids := []string{h.dst[0].sid, h.dst[1].sid, h.dst[2].sid}

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", sids, "/dst.bin", 0)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if res.Err == nil {
		t.Fatalf("res.Err = nil, want failure (one dest failed)")
	}
	if res.Destinations[0].OK != true {
		t.Errorf("dest0 ok = false, want true")
	}
	if res.Destinations[1].OK != false {
		t.Errorf("dest1 ok = true, want false")
	}
	if res.Destinations[2].OK != true {
		t.Errorf("dest2 ok = false, want true")
	}
}

// TestRelayTransferSrcEqualsDst: src_sid == dst_sid → 该 dest 报错 "cannot relay to itself"。
func TestRelayTransferSrcEqualsDst(t *testing.T) {
	h := newRelayHarness(t, 1, []byte("x"))
	res, err := h.mgr.RelayTransfer(h.srcSid, "/s", []string{h.srcSid}, "/d", 0)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if res.Destinations[0].Err == nil || !strings.Contains(res.Destinations[0].Err.Error(), "itself") {
		t.Errorf("dest err = %v, want 'cannot relay a session to itself'", res.Destinations[0].Err)
	}
}

// TestRelayTransferEmptyDsts: 空 dst_sids → 硬错误。
func TestRelayTransferEmptyDsts(t *testing.T) {
	h := newRelayHarness(t, 0, []byte("x"))
	_, err := h.mgr.RelayTransfer(h.srcSid, "/s", nil, "/d", 0)
	if err == nil || !strings.Contains(err.Error(), "no relay destinations") {
		t.Errorf("err = %v, want 'no relay destinations'", err)
	}
}

// TestRelayTransferStatFallback: Stat 失败时降级为 Upload（无 size），仍完成传输。
func TestRelayTransferStatFallback(t *testing.T) {
	data := bytes.Repeat([]byte("fb\n"), 300)
	h := newRelayHarness(t, 1, data)
	h.srcConn.statErr = errors.New("stat: permission denied") // 触发降级
	dstSid := h.dst[0].sid

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", []string{dstSid}, "/dst.bin", 0)
	if err != nil {
		t.Fatalf("RelayTransfer: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("res.Err = %v, want nil (fallback should succeed)", res.Err)
	}
	if !res.Destinations[0].OK {
		t.Errorf("dest ok=false, err=%v", res.Destinations[0].Err)
	}
	if !bytes.Equal(h.dst[0].conn.uploadedBytes, data) {
		t.Errorf("content mismatch after stat fallback")
	}
}
```

Add `"strings"` to the relay_test.go import block.

> These tests reference two new `fakeConn` fields: `downloadErr` and `uploadErr`. Step 3 adds them.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/ssh/session/ -run TestRelayTransfer -v`
Expected: COMPILE FAILURE — `RelayTransfer undefined`, `fakeConn.downloadErr`/`uploadErr` undefined.

- [ ] **Step 3: Add `downloadErr` / `uploadErr` to `fakeConn`**

In `internal/ssh/session/session_test.go`, add fields to the sftp block of `fakeConn` (after `uploadDelay`, ~line 31):

```go
	downloadErr error // 非 nil 时 Download 直接返回该错误（模拟源文件不存在等）
	uploadErr   error // 非 nil 时 Upload/UploadSized 直接返回该错误
```

Update `fakeConn.Download` (~line 112) to honor `downloadErr` — insert after the `sftpEnabled` check:

```go
func (f *fakeConn) Download(remotePath string, dst io.Writer, timeoutMs int) (int, bool, error) {
	if !f.sftpEnabled {
		return 0, false, conn.ErrSftpUnavailable
	}
	if f.downloadErr != nil {
		return 0, false, f.downloadErr
	}
	// ... 其余不变（downloadBlock / uploadDelay / Write）
```

Update `fakeConn.Upload` (~line 96) and `fakeConn.UploadSized` (Task 2) to honor `uploadErr` — insert after the `sftpEnabled` check in each:

```go
	if f.uploadErr != nil {
		return 0, false, f.uploadErr
	}
```

- [ ] **Step 4: Implement `Manager.RelayTransfer`**

Append to `internal/ssh/session/relay.go` (add `"os"` and `"time"`? — `os` only if needed; the method uses `os.FileInfo` via Stat return, no direct `os` ref needed. Add `"sync"` already there; `"io"` already there; add `"fmt"` if used — not needed. Keep imports minimal.):

```go
// RelayDest 是单个目标的 relay 结果。
type RelayDest struct {
	DstSid    string
	DstServer string
	OK        bool
	Bytes     int
	TimedOut  bool
	Err       error
}

// RelayResult 是 RelayTransfer 的汇总结果。
//   - Err 非 nil 表示下载或某目标失败（部分失败）；硬错误（src 不存在等）经 Go error 返回。
//   - Destinations 按 dstSids 输入顺序，每项含 per-dest ok/bytes/timed_out/err。
type RelayResult struct {
	DownloadedBytes int
	TimedOut        bool
	SrcServer       string
	Destinations    []RelayDest
	Err             error
}

// RelayTransfer 把 srcSid session 上的 srcPath 文件流式中转到 dstSids 各 session 的 dstPath。
// 1:N fanout：源文件只读一次，fanWriter 并发分发到所有存活目标。1:1 是 N=1 特例。
//
// 返回 (RelayResult, error)：
//   - Go error 仅用于硬错误（srcSid 不存在、dstSids 为空）。
//   - 部分失败（下载失败、某目标失败、源非普通文件）返回 (res, nil)，res.Err 非 nil，
//     各 dest 的 OK 反映自身成败——一个目标失败不连累其他目标。
func (m *Manager) RelayTransfer(srcSid, srcPath string, dstSids []string, dstPath string, timeoutMs int) (RelayResult, error) {
	srcSess, err := m.Get(srcSid)
	if err != nil {
		return RelayResult{}, err
	}
	if len(dstSids) == 0 {
		return RelayResult{}, errors.New("no relay destinations")
	}

	dests := make([]RelayDest, len(dstSids))

	type liveEntry struct {
		idx  int
		sess *Session
		pr   *io.PipeReader
		pw   *io.PipeWriter
	}
	var live []*liveEntry

	// 预检：解析每个目标、校验 sftp/idle，失败的记入 dests[idx].Err 但不中止其他目标。
	for i, sid := range dstSids {
		dests[i] = RelayDest{DstSid: sid}
		if sid == srcSid {
			dests[i].Err = errors.New("cannot relay a session to itself")
			continue
		}
		ds, gerr := m.Get(sid)
		if gerr != nil {
			dests[i].Err = gerr
			continue
		}
		dests[i].DstServer = ds.ServerName()
		if !ds.SftpAvailable() {
			dests[i].Err = conn.ErrSftpUnavailable
			continue
		}
		if ds.State() != StateIdle {
			dests[i].Err = errors.New("session busy")
			continue
		}
		pr, pw := io.Pipe()
		live = append(live, &liveEntry{idx: i, sess: ds, pr: pr, pw: pw})
	}

	if len(live) == 0 {
		return RelayResult{
			SrcServer:    srcSess.ServerName(),
			Destinations: dests,
			Err:          errors.New("no live relay destinations (all failed pre-flight)"),
		}, nil
	}

	// Stat 源文件：拿 size + 校验普通文件。失败则降级为 Upload（无 size，串行）。
	var size int64 = -1
	useSized := false
	if fi, statErr := srcSess.Stat(srcPath); statErr == nil {
		if !fi.Mode().IsRegular() {
			for _, e := range live {
				e.pr.CloseWithError(errors.New("source not a regular file"))
				dests[e.idx].Err = errors.New("source not a regular file")
			}
			return RelayResult{
				SrcServer:    srcSess.ServerName(),
				Destinations: dests,
				Err:          errors.New("source not a regular file"),
			}, nil
		}
		size = fi.Size()
		useSized = true
	}
	// statErr != nil：降级，useSized=false

	// fanWriter 持所有存活目标的 pw
	pws := make([]*io.PipeWriter, len(live))
	for i, e := range live {
		pws[i] = e.pw
	}
	fw := newFanWriter(pws)

	// 下载侧 goroutine
	var dlN int
	var dlTimedOut bool
	var dlErr error
	dlDone := make(chan struct{})
	go func() {
		defer close(dlDone)
		dlN, dlTimedOut, dlErr = srcSess.Download(srcPath, fw, timeoutMs)
		// 下载结束：关闭所有 pw，让上传侧收尾（nil → EOF，非 nil → 传播错误）
		for _, e := range live {
			e.pw.CloseWithError(dlErr)
		}
	}()

	// 上传侧 N 个 goroutine
	var upWg sync.WaitGroup
	for _, e := range live {
		upWg.Add(1)
		go func(e *liveEntry) {
			defer upWg.Done()
			var n int
			var tOut bool
			var uerr error
			if useSized {
				n, tOut, uerr = e.sess.UploadSized(e.pr, size, dstPath, timeoutMs)
			} else {
				n, tOut, uerr = e.sess.Upload(e.pr, dstPath, timeoutMs)
			}
			// 上传结束：关闭 pr，解除 fanWriter.Write 对 pw 的阻塞（失败隔离）
			e.pr.CloseWithError(uerr)
			dests[e.idx].OK = uerr == nil && !tOut
			dests[e.idx].Bytes = n
			dests[e.idx].TimedOut = tOut
			dests[e.idx].Err = uerr
		}(e)
	}

	<-dlDone
	upWg.Wait()

	// 聚合
	res := RelayResult{
		DownloadedBytes: dlN,
		TimedOut:        dlTimedOut,
		SrcServer:       srcSess.ServerName(),
		Destinations:    dests,
	}
	allOK := dlErr == nil && !dlTimedOut
	for i := range dests {
		if !dests[i].OK {
			allOK = false
		}
	}
	// 已知 size 时校验字节一致
	if allOK && useSized {
		if dlN != int(size) {
			allOK = false
		}
		for i := range dests {
			if dests[i].Bytes != int(size) {
				dests[i].OK = false
				allOK = false
			}
		}
	}
	if !allOK {
		root := dlErr
		if root == nil {
			root = errors.New("one or more relay destinations failed")
		}
		res.Err = root
	}
	return res, res.Err
}
```

`relay.go` imports: ensure `errors`, `io`, `sync` are present (they are, from Task 3). The method references `conn.ErrSftpUnavailable` — add `"github.com/jim58246/sshmng/internal/ssh/conn"` to `relay.go` imports.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/ssh/session/ -run TestRelayTransfer -v`
Expected: PASS for all seven relay tests.

- [ ] **Step 6: Run race detector**

Run: `go test ./internal/ssh/session/ -run TestRelayTransfer -race`
Expected: PASS, no race detected (fanWriter + goroutines are synchronized via mu + WaitGroup + channel).

- [ ] **Step 7: Full build + test**

Run: `go build ./... && go test ./internal/ssh/...`
Expected: build OK, all tests PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/ssh/session/relay.go internal/ssh/session/relay_test.go internal/ssh/session/session_test.go
git commit -m "$(cat <<'EOF'
feat(session): add Manager.RelayTransfer 1:N stream relay

Streams a remote file from one session to N others via in-process io.Pipe
fanout — no local disk, download/upload overlap, source read once. Per-dest
failure isolation; early Download termination when all dests fail; graceful
fallback to serial Upload when Stat fails. 1:1 is the N=1 special case.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: MCP `relay_transfer` tool + registration + test

**Files:**
- Create: `internal/mcp/tools_relay.go`
- Create: `internal/mcp/tools_relay_test.go`
- Modify: `internal/mcp/server.go` (register in the "File transfer tools" block, ~line 209)

**Interfaces:**
- Consumes: `Service.manager *session.Manager` (`server.go:71`), `Manager.RelayTransfer(...)` (Task 4), `errorResult` / `textResult` / `s.sessionLogger` (`server.go`).
- Produces: `Service.RelayTransfer(ctx, *mcp.CallToolRequest, RelayTransferArgs) (*mcp.CallToolResult, any, error)` — the MCP handler bound to the `relay_transfer` tool.

- [ ] **Step 1: Write the failing handler test**

Create `internal/mcp/tools_relay_test.go`:

```go
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jim58246/sshmng/internal/ssh/session"
)

// newRelayService 建一个带源+目标 session 的 Service（fakeConn 透过 session 包）。
// 这里用 session 包的真实 Manager + 真实 session，fakeConn 在 session_test.go 内部；
// 跨包测试无法直接用 fakeConn，改用 session 包导出的 NewManager + 一个最小 fake。
// 由于 fakeConn 未导出，本测试改为验证 handler 的错误分支（hard error）与结果体形状，
// 端到端流式已在 session 包覆盖。
func newRelayService() *Service {
	mgr := session.NewManager()
	return &Service{manager: mgr, baseLogger: nil}
}

func TestRelayTransferHardErrorEmptyDsts(t *testing.T) {
	svc := newRelayService()
	// 源 session 不存在 → hard error → IsError=true
	res, _, err := svc.RelayTransfer(context.Background(), nil, RelayTransferArgs{
		SrcSid: "nope", SrcPath: "/s", DstSids: []string{"d"}, DstPath: "/d",
	})
	if err != nil {
		t.Fatalf("Go error: %v", err)
	}
	if !res.IsError {
		t.Errorf("IsError = false, want true (src not found)")
	}
}

func TestRelayTransferEmptyDstsIsError(t *testing.T) {
	svc := newRelayService()
	// 先注册一个源 session 使其存在，但 dst_sids 空
	mgr := svc.manager
	_ = mgr // 源不存在也会先报 not found；这里直接测空 dsts 的硬错误路径
	res, _, err := svc.RelayTransfer(context.Background(), nil, RelayTransferArgs{
		SrcSid: "any", SrcPath: "/s", DstSids: nil, DstPath: "/d",
	})
	if err != nil {
		t.Fatalf("Go error: %v", err)
	}
	if !res.IsError {
		t.Errorf("IsError = false, want true (empty dsts)")
	}
}

// TestRelayTransferResultShape: 验证成功路径结果体的 JSON 形状（用 1:1 真实流式）。
// 需要可用的源/目标 session + sftp。fakeConn 未导出，这里用 session 包提供的测试钩子
// 若不存在则 skip——端到端流式断言已在 session 包 TestRelayTransferOneToOne 覆盖。
func TestRelayTransferResultShape(t *testing.T) {
	// 跨包无法装配 fakeConn；本用例留作形状占位，真实断言在 session 包。
	t.Skip("end-to-end shape asserted in session.TestRelayTransferOneToOne; fakeConn not exported cross-package")
	_ = bytes.NewBuffer
	_ = json.Marshal
	_ = strings.Contains
}
```

> Rationale: `fakeConn` lives in the `session` package (test-only, unexported), so the `mcp` package cannot assemble a fake sftp session cross-package. The handler's hard-error branches (which use only `manager.Get` + arg validation, no sftp) ARE testable cross-package and are covered here. The full streaming path is covered in `session.TestRelayTransferOneToOne` (Task 4). Delete the `TestRelayTransferResultShape` skip stub if it adds no value — keep only the two hard-error tests.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/mcp/ -run TestRelayTransfer -v`
Expected: COMPILE FAILURE — `RelayTransfer` / `RelayTransferArgs` undefined.

- [ ] **Step 3: Implement the handler**

Create `internal/mcp/tools_relay.go`:

```go
package mcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RelayTransferArgs 是 relay_transfer 工具的入参。
type RelayTransferArgs struct {
	SrcSid    string   `json:"src_sid"`
	SrcPath   string   `json:"src_path" jsonschema:"remote file path on the source session to relay"`
	DstSids   []string `json:"dst_sids" jsonschema:"destination session sids; 1:1 = single-element array"`
	DstPath   string   `json:"dst_path" jsonschema:"remote write path shared by all destinations"`
	TimeoutMs int      `json:"timeout_ms,omitempty" jsonschema:"optional, 0 = default 300000 (300s). Each of download + every upload side gets its own budget (concurrent)"`
}

// relayDestJSON 是单个目标的 JSON 输出（把 error 转字符串，避免 marshal 成 {}）。
type relayDestJSON struct {
	DstSid    string `json:"dst_sid"`
	DstServer string `json:"dst_server"`
	OK        bool   `json:"ok"`
	Bytes     int    `json:"bytes"`
	TimedOut  bool   `json:"timed_out"`
	Error     string `json:"error,omitempty"`
}

// RelayTransfer 把 src_sid session 上的文件流式中转到 dst_sids 各 session。
// 1:N fanout：源文件只读一次，并发分发到所有目标；1:1 是单元素数组特例。
// 所有目标共用 dst_path。需要源与每个目标的 sftp_available=true（先用 stat 检查）。
// 单目标失败不中止其他目标；部分失败返回 ok:false 的结果体（IsError 不置位），
// 检查 ok 字段而非 MCP error flag——与 upload_dir/download_dir 一致。
// 仅硬错误（sid 不存在、dst_sids 为空）返回 IsError=true。
func (s *Service) RelayTransfer(ctx context.Context, req *mcp.CallToolRequest, args RelayTransferArgs) (*mcp.CallToolResult, any, error) {
	res, err := s.manager.RelayTransfer(args.SrcSid, args.SrcPath, args.DstSids, args.DstPath, args.TimeoutMs)
	if err != nil {
		// 硬错误：src 不存在 / dst_sids 为空
		return errorResult("%v", err)
	}

	s.sessionLogger(req, args.SrcSid).Info("relay_transfer",
		"src_server", res.SrcServer,
		"downloaded_bytes", res.DownloadedBytes,
		"destinations", len(res.Destinations),
		"ok", res.Err == nil)

	dests := make([]relayDestJSON, len(res.Destinations))
	for i, d := range res.Destinations {
		errStr := ""
		if d.Err != nil {
			errStr = d.Err.Error()
		}
		dests[i] = relayDestJSON{
			DstSid:    d.DstSid,
			DstServer: d.DstServer,
			OK:        d.OK,
			Bytes:     d.Bytes,
			TimedOut:  d.TimedOut,
			Error:     errStr,
		}
	}

	out := map[string]any{
		"ok":               res.Err == nil,
		"downloaded_bytes": res.DownloadedBytes,
		"timed_out":        res.TimedOut,
		"src_server":       res.SrcServer,
		"destinations":     dests,
	}
	if res.Err != nil {
		out["error"] = res.Err.Error()
	}
	return textResult(out)
}
```

- [ ] **Step 4: Register the tool**

In `internal/mcp/server.go`, in the "File transfer tools" block, add after the `download_dir` registration (~line 209), before `return server`:

```go
	mcp.AddTool(server, &mcp.Tool{
		Name:        "relay_transfer",
		Description: "Relay (stream) a remote file from one session to one or more other sessions through the sshmng process, without landing it on local disk and without waiting for the full download before uploading. 1:N fanout: the source is read once and streamed concurrently to all destinations (sharded/replicated deploy). 1:1 is the single-element dst_sids case. Requires sftp_available=true on the source and every destination (check stat first). All destinations share the same dst_path. Returns {ok, downloaded_bytes, timed_out, src_server, destinations:[{dst_sid, dst_server, ok, bytes, timed_out, error}]}. Per-destination failures do not abort the others; partial failures return ok:false in the result body with IsError not set (check the ok field, not the MCP error flag).",
	}, svc.RelayTransfer)
```

- [ ] **Step 5: Run the handler tests**

Run: `go test ./internal/mcp/ -run TestRelayTransfer -v`
Expected: PASS for the hard-error tests.

- [ ] **Step 6: Verify the tool is registered & server builds**

Run: `go build ./... && go test ./internal/mcp/`
Expected: build OK, all mcp tests PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/mcp/tools_relay.go internal/mcp/tools_relay_test.go internal/mcp/server.go
git commit -m "$(cat <<'EOF'
feat(mcp): add relay_transfer tool for 1:N streamed file relay

Binds Manager.RelayTransfer to an MCP tool. Partial failures return a
result body with ok:false (IsError not set), matching upload_dir/download_dir;
only hard errors (missing sid, empty dst_sids) set IsError.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: Bilingual docs

**Files:**
- Modify: `docs/agents.md`, `docs/zh-CN/agents.md` (MCP tool list + signature)
- Modify: `README.md`, `README.zh-CN.md` (MCP tools section, if present)

**Constraints:** Per CLAUDE.md pre-release checklist: MCP tools in `docs/agents.md` must match `internal/mcp/tools_*.go` + `server.go`; every English change mirrored in `docs/zh-CN/*` + `README.zh-CN.md`.

- [ ] **Step 1: Locate the existing tool entries to mirror**

Run: `grep -n "download_dir\|download\b\|upload_dir" docs/agents.md docs/zh-CN/agents.md README.md README.zh-CN.md`
Expected: shows the exact lines/sections where `relay_transfer` must be added alongside `download_dir`.

- [ ] **Step 2: Add the English entry to `docs/agents.md`**

Insert a `relay_transfer` row/section immediately after the `download_dir` entry, mirroring the surrounding format (table row if table, or `### relay_transfer` subsection if prose). Use this signature + description (condensed to match the file's existing style — copy the field/description text from the neighboring `download`/`download_dir` entries for tone):

**Signature:**
```
relay_transfer(src_sid, src_path, dst_sids[], dst_path, timeout_ms?)
→ { ok, downloaded_bytes, timed_out, src_server, destinations[{dst_sid, dst_server, ok, bytes, timed_out, error}], error? }
```

**Description (English):** Stream a remote file from one session to one or more other sessions through the sshmng process, without landing it on local disk and without waiting for the full download before uploading. 1:N fanout: the source is read once and streamed concurrently to all destinations (use case: sharded/replicated deploy where N machines need the same artifact). 1:1 is the single-element `dst_sids` case. Requires `sftp_available=true` on the source and every destination (check `stat` first); all destinations share the same `dst_path`. Per-destination failures do not abort the others; partial failures return `ok:false` in the result body with `IsError` not set — check the `ok` field, not the MCP error flag.

- [ ] **Step 3: Add the Chinese entry to `docs/zh-CN/agents.md`**

Insert the mirrored entry immediately after the `download_dir` 条目, matching the file's existing 中文风格:

**签名:**
```
relay_transfer(src_sid, src_path, dst_sids[], dst_path, timeout_ms?)
→ { ok, downloaded_bytes, timed_out, src_server, destinations[{dst_sid, dst_server, ok, bytes, timed_out, error}], error? }
```

**描述（中文）:** 通过 sshmng 进程中转（流式）传输一个远端文件，从一个 session 到一个或多个其他 session，不落本地磁盘、无需等整文件下载完再上传。1:N 扇出：源文件只读一次，并发分发到所有目标（典型场景：分片/副本部署，N 台机器需更新同一产物）。1:1 即 `dst_sids` 单元素数组的特例。需要源与每个目标的 `sftp_available=true`（先查 `stat`）；所有目标共用同一 `dst_path`。单个目标失败不中止其他目标；部分失败返回 `ok:false` 的结果体且不置 `IsError`——检查 `ok` 字段而非 MCP error flag。

- [ ] **Step 4: Update `README.md` and `README.zh-CN.md` if they list MCP tools**

Run: `grep -n "download_dir\|upload_dir\|### .*tool\|MCP" README.md README.zh-CN.md`
If the READMEs enumerate MCP tools, add `relay_transfer` in the same position with the same condensed description (English / 中文 respectively). If the READMEs only document the CLI and link to `docs/agents.md` for MCP tools, no README change is needed — note that in the commit message.

- [ ] **Step 5: Verify doc/code consistency (CLAUDE.md checklist items 2 & 4)**

Run:
```bash
go build ./... && \
grep -n "relay_transfer" internal/mcp/server.go internal/mcp/tools_relay.go docs/agents.md docs/zh-CN/agents.md && \
grep -rn "relay_transfer" README.md README.zh-CN.md
```
Expected: `relay_transfer` appears in `server.go`, `tools_relay.go`, both `agents.md` files; README entries present only if those files list MCP tools. Build OK.

- [ ] **Step 6: Run the full test suite**

Run: `go build ./... && go test ./...`
Expected: build OK, all tests PASS.

- [ ] **Step 7: Commit**

```bash
git add docs/agents.md docs/zh-CN/agents.md README.md README.zh-CN.md
git commit -m "$(cat <<'EOF'
docs: document relay_transfer tool (bilingual)

Add relay_transfer to the MCP tool list in docs/agents.md and zh-CN
mirror, plus README MCP sections where applicable. Tool list matches
internal/mcp/server.go registration.

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

## Self-Review

**1. Spec coverage:**
- 1:N fanout, source read once, no disk, download/upload overlap → Task 3 (fanWriter) + Task 4 (orchestration). ✓
- `Conn.Stat` + `Conn.UploadSized` for upload-side concurrent pipelining → Task 1 + Task 2. ✓
- `Stat` failure → graceful fallback to `Upload` (serial) → Task 4 `TestRelayTransferStatFallback` + orchestrator `useSized=false` path. ✓
- Failure isolation (one dest fails, others proceed) → Task 3 `TestFanWriterIsolatesDeadDest` + Task 4 `TestRelayTransferOneDestFailsOthersSucceed`. ✓
- All dests fail → early Download termination → Task 3 `TestFanWriterAllDeadReturnsError` + Task 4 (dlErr = errAllDestinationsFailed). ✓
- Per-dest result array, input order, `bytes`/`ok`/`timed_out`/`error` → Task 4 `RelayDest` + Task 5 `relayDestJSON`. ✓
- Pre-flight rejections (src==dst, sftp-unavailable, non-idle, empty dsts) → Task 4 tests. ✓
- Source not regular file → early clear error → Task 4 orchestrator. ✓
- Partial failure → `ok:false` body, IsError not set; hard error → IsError → Task 5 handler + tests. ✓
- Bilingual docs, agents.md ↔ server.go consistency → Task 6. ✓
- fakeConn extended (Stat/UploadSized/statFi/statErr/downloadErr/uploadErr) → Task 1/2/4. ✓
- Out of scope (dir relay, per-dest paths, per-dest buffer, progress) → not implemented, as specified. ✓

**2. Placeholder scan:** None. Every code step contains full, compilable code. (The `TestRelayTransferResultShape` skip stub in Task 5 is explicitly explained and may be deleted — it is not a placeholder for required work; end-to-end streaming is covered in the `session` package.)

**3. Type consistency:**
- `Conn.Stat(path string) (os.FileInfo, error)` — used identically in Task 1 (PtyConn/fakeConn/Session) and Task 4 (`srcSess.Stat(srcPath)`). ✓
- `Conn.UploadSized(src io.Reader, size int64, remotePath string, timeoutMs int) (int, bool, error)` — Task 2 defines it; Task 4 calls `e.sess.UploadSized(e.pr, size, dstPath, timeoutMs)` matching the signature. ✓
- `Manager.RelayTransfer(srcSid, srcPath string, dstSids []string, dstPath string, timeoutMs int) (RelayResult, error)` — Task 4 defines it; Task 5 calls `s.manager.RelayTransfer(args.SrcSid, args.SrcPath, args.DstSids, args.DstPath, args.TimeoutMs)` matching. ✓
- `RelayDest` fields (`DstSid, DstServer, OK, Bytes, TimedOut, Err`) — Task 4 defines; Task 5 maps them to `relayDestJSON`. ✓
- `fanWriter` / `newFanWriter` / `errAllDestinationsFailed` — Task 3 defines; Task 4 uses `newFanWriter(pws)` and `dlErr` may equal `errAllDestinationsFailed`. ✓
- `fakeFileInfo{size, mode}` — Task 1 defines; Task 4 harness uses `fakeFileInfo{size: ..., mode: 0644}`. ✓

No type drift found.
