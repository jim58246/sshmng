# SFTP 文件夹传输 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 加 2 个 MCP 工具 `upload_dir` / `download_dir`，一次调用传输整棵目录树，封装 `Walk` + `MkdirAll` + 并发文件传输 + conflict policy（overwrite/skip/rename）。

**Architecture:** 三层对称扩展：
- `conn` 包加类型（`ConflictPolicy` / `DirTransferOptions` / `DirTransferResult`）
- `pty` 包加 `PtyConn.UploadDir` / `PtyConn.DownloadDir`——`filepath.Walk`（upload）或 `sftpClient.Walk`（download）遍历树，`MkdirAll` 建目录，并发 worker pool 传文件，per-file conflict policy
- `session` 包扩展 `Conn` 接口加 `UploadDir` / `DownloadDir` 方法，`Session.UploadDir` / `Session.DownloadDir` 套状态机（与 `Upload` / `Download` 对称）
- `mcp` 包加 `Service.UploadDir` / `Service.DownloadDir`，注册 2 个新工具

**Tech Stack:** Go 1.25、`github.com/pkg/sftp` v1.13.11（`Client.Walk` 返回 `*github.com/kr/fs.Walker`，用 `Step`/`Path`/`Stat`/`Err` 遍历）、`path/filepath.Walk`、`errors.Join`（Go 1.20+）。

## Global Constraints

- **扩展 `Conn` 接口**加 `UploadDir(localDir, remoteDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error)` 和 `DownloadDir(remoteDir, localDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error)` 方法——与现有 `Upload`/`Download` 对称
- 类型定义在 `internal/ssh/conn/sftp_dir.go`：`ConflictPolicy`（int enum）、`DirTransferOptions`、`DirTransferResult`。`session` 和 `pty` 都 import `conn`，无新依赖方向
- `Session.UploadDir` / `Session.DownloadDir` 与 `Session.Upload` / `Session.Download` 共用 `s.mu`，state 转换对称（进 Running 必出 Running，除非 session 被强制 Close）
- 现有 sftp 测试（`internal/ssh/pty/sftp_test.go`）必须全过——它们是行为契约
- 不改 MCP 工具层现有 `upload` / `download` 工具（`tools_file.go` 不动）——新工具在 `tools_file_dir.go`
- v1 YAGNI 边界：
  - 不支持 symlink（遇到跳过 + 计入 `Skipped`）
  - 不保留权限/mtime（用 sftp/server 默认）
  - 不支持 resume（失败文件可重传，overwrite 模式幂等）
  - 不支持 glob filter（include/exclude 模式）
  - 不支持多并发 session 间协调
- per-file 错误不中断整树传输——继续传其他文件，用 `errors.Join` 聚合返回
- `Concurrency` 默认 4，0 = 默认 4；per-file `TimeoutMs` 默认 300s
- 设计文档 `docs/ssh-session-manager-design.md` §3.3 MCP 工具清单加 `upload_dir` / `download_dir` 条目（本 plan 最后一个 task 顺带更新）

---

## File Structure

| 文件 | 改动 | 责任 |
|---|---|---|
| `internal/ssh/conn/sftp_dir.go` | 新建 | `ConflictPolicy` 类型 + `DirTransferOptions` / `DirTransferResult` struct |
| `internal/ssh/conn/sftp_dir_test.go` | 新建 | 类型常量 smoke test |
| `internal/ssh/pty/sftp_dir.go` | 新建 | `PtyConn.UploadDir` / `PtyConn.DownloadDir` 实现 |
| `internal/ssh/pty/sftp_dir_test.go` | 新建 | pty 层 dir 传输测试（用现有 `newFakeShellServerWithSftp`） |
| `internal/ssh/session/session.go` | 改 | 扩展 `Conn` 接口 + 加 `Session.UploadDir` / `Session.DownloadDir` |
| `internal/ssh/session/session_test.go` | 改 | 扩 `fakeConn` 支持 `UploadDir` / `DownloadDir`；加 session 层测试 |
| `internal/mcp/tools_file_dir.go` | 新建 | `Service.UploadDir` / `Service.DownloadDir` + args struct |
| `internal/mcp/tools_file_dir_test.go` | 新建 | MCP 层测试 |
| `internal/mcp/server.go` | 改 | 注册 2 个新工具 |
| `docs/ssh-session-manager-design.md` | 改 | §3.3 加 `upload_dir` / `download_dir` 条目 |

`tools_file.go` 不动——现有 `upload` / `download` 单文件工具保持不变。

---

## Task 1: ConflictPolicy 类型 + UploadDir 基础（overwrite，顺序）

**Files:**
- Create: `internal/ssh/conn/sftp_dir.go`
- Create: `internal/ssh/pty/sftp_dir.go`
- Test: `internal/ssh/pty/sftp_dir_test.go`

**Interfaces:**
- Consumes: `path/filepath`、`github.com/pkg/sftp`、`PtyConn.sftpClient`（已在 `sftp.go` 用）
- Produces: `conn.ConflictPolicy` / `conn.DirTransferOptions` / `conn.DirTransferResult` 类型；`PtyConn.UploadDir(localDir, remoteDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error)` 方法

- [ ] **Step 1: 写类型定义**

Create `internal/ssh/conn/sftp_dir.go`:

```go
package conn

// ConflictPolicy 定义目标文件已存在时的处理策略。
type ConflictPolicy int

const (
	// ConflictOverwrite 覆盖目标文件（sftp.Create / os.Create 语义，truncate）。
	ConflictOverwrite ConflictPolicy = iota
	// ConflictSkip 跳过已存在的目标文件，不计入失败。
	ConflictSkip
	// ConflictRename 自动重命名源文件为 name_1、name_2... 直到无冲突。
	ConflictRename
)

// String 把 ConflictPolicy 转为字符串，用于 MCP 工具 args 解析与日志。
func (c ConflictPolicy) String() string {
	switch c {
	case ConflictOverwrite:
		return "overwrite"
	case ConflictSkip:
		return "skip"
	case ConflictRename:
		return "rename"
	}
	return "unknown"
}

// ParseConflictPolicy 把字符串转为 ConflictPolicy，无效值默认 ConflictOverwrite。
func ParseConflictPolicy(s string) ConflictPolicy {
	switch s {
	case "skip":
		return ConflictSkip
	case "rename":
		return ConflictRename
	case "", "overwrite":
		fallthrough
	default:
		return ConflictOverwrite
	}
}

// DirTransferOptions 是文件夹传输的选项。
type DirTransferOptions struct {
	Conflict    ConflictPolicy // 0 = ConflictOverwrite
	Concurrency int            // 0 = 默认 4
	TimeoutMs   int            // 0 = 默认 300000（300s），per file
}

// DirTransferResult 是文件夹传输的汇总结果。
type DirTransferResult struct {
	Bytes    int64 // 成功传输的字节总数
	Files    int   // 成功传输的文件数
	Skipped  int   // 因 ConflictSkip 跳过的文件数
	Renamed  int   // 因 ConflictRename 重命名的文件数
	TimedOut int   // per-file 超时的文件数
}
```

- [ ] **Step 2: 写失败的测试——UploadDir 传 2 文件树**

Create `internal/ssh/pty/sftp_dir_test.go`:

```go
package pty

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"sshmng/internal/config"
	"sshmng/internal/ssh/conn"
)

// TestUploadDirBasic: 2 文件 + 1 子目录的本地树上传到 remote，验证：
// - 目录被创建（MkdirAll）
// - 文件内容正确
// - DirTransferResult.Files/Bytes 正确
func TestUploadDirBasic(t *testing.T) {
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

	// 本地树：localRoot/a.txt, localRoot/sub/b.txt
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "a.txt"), []byte("aaa"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(localRoot, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localRoot, "sub", "b.txt"), []byte("bbbbb"), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := p.UploadDir(localRoot, "/uploaddir", conn.DirTransferOptions{})
	if err != nil {
		t.Fatalf("UploadDir: %v", err)
	}
	if res.Files != 2 {
		t.Errorf("Files = %d, want 2", res.Files)
	}
	if res.Bytes != 8 {
		t.Errorf("Bytes = %d, want 8", res.Bytes)
	}

	// 验证远端文件内容
	for _, c := range []struct{ path, want string }{
		{"/uploaddir/a.txt", "aaa"},
		{"/uploaddir/sub/b.txt", "bbbbb"},
	} {
		f, err := p.sftpClient.Open(c.path)
		if err != nil {
			t.Fatalf("Open %s: %v", c.path, err)
		}
		got, err := readAll(f)
		f.Close()
		if err != nil {
			t.Fatalf("Read %s: %v", c.path, err)
		}
		if !bytes.Equal(got, []byte(c.want)) {
			t.Errorf("%s = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestUploadDirEmptyLocalDir: 空本地目录 → 创建空远端目录，0 文件
func TestUploadDirEmptyLocalDir(t *testing.T) {
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

	localRoot := t.TempDir() // 空目录

	res, err := p.UploadDir(localRoot, "/emptydir", conn.DirTransferOptions{})
	if err != nil {
		t.Fatalf("UploadDir: %v", err)
	}
	if res.Files != 0 {
		t.Errorf("Files = %d, want 0", res.Files)
	}

	// 远端目录应存在
	info, err := p.sftpClient.Stat("/emptydir")
	if err != nil {
		t.Errorf("Stat /emptydir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("/emptydir not a dir")
	}
}

// TestUploadDirLocalNotExist: 本地目录不存在 → error
func TestUploadDirLocalNotExist(t *testing.T) {
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

	_, err = p.UploadDir("/nonexistent/local/path", "/uploaddir", conn.DirTransferOptions{})
	if err == nil {
		t.Errorf("UploadDir with non-existent local dir: err=nil, want error")
	}
}

// readAll 是测试辅助：读完 sftp.File 内容。io.ReadAll 可直接用，但需 import io。
// 简化：直接用 io.ReadAll。
func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/ssh/pty/ -run TestUploadDir -v`
Expected: FAIL——`undefined: p.UploadDir`。

- [ ] **Step 4: 实现 UploadDir（overwrite only，顺序）**

Create `internal/ssh/pty/sftp_dir.go`:

```go
package pty

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"

	"sshmng/internal/ssh/conn"
)

// UploadDir 把本地 localDir 整树上传到远端 remoteDir。
//   - opts.Concurrency=0 默认 4；本 Task 1 先实现顺序版本（Concurrency 强制 1）
//   - opts.Conflict=0 默认 ConflictOverwrite；本 Task 1 只实现 overwrite
//   - per-file 错误不中断整树传输，用 errors.Join 聚合返回
//   - symlink 跳过（不计入 Skipped，因为 Skipped 是 ConflictSkip 计数；symlink 不计入任何计数）
//
// 返回 DirTransferResult 汇总。
func (p *PtyConn) UploadDir(localDir, remoteDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error) {
	p.logger.Debug("sftp upload_dir start", "sid", p.sid, "local", localDir, "remote", remoteDir, "opts", opts)

	p.mu.Lock()
	sftpClient := p.sftpClient
	p.mu.Unlock()
	if sftpClient == nil {
		return conn.DirTransferResult{}, conn.ErrSftpUnavailable
	}

	// 本地目录必须存在
	info, err := os.Stat(localDir)
	if err != nil {
		return conn.DirTransferResult{}, err
	}
	if !info.IsDir() {
		return conn.DirTransferResult{}, errors.New("local path is not a directory")
	}

	var result conn.DirTransferResult
	var errs []error

	walkErr := filepath.Walk(localDir, func(localPath string, fi os.FileInfo, err error) error {
		if err != nil {
			errs = append(errs, err)
			return nil // continue walking
		}

		// 相对路径 → 远端路径
		rel, err := filepath.Rel(localDir, localPath)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		remotePath := path.Join(remoteDir, filepath.ToSlash(rel))

		// symlink：跳过（v1 YAGNI）
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		if fi.IsDir() {
			if err := sftpClient.MkdirAll(remotePath); err != nil {
				errs = append(errs, err)
			}
			return nil
		}

		// 文件：本 Task 1 只实现 overwrite
		f, err := os.Open(localPath)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		n, timedOut, err := p.uploadOne(f, remotePath, opts.TimeoutMs)
		f.Close()
		result.Bytes += int64(n)
		if err != nil {
			errs = append(errs, err)
		}
		if timedOut {
			result.TimedOut++
		}
		// 注：uploadOne 内部用 p.Upload（已实现），ConflictOverwrite 直接覆盖
		result.Files++
		_ = timedOut // 占位，Task 2 加 skip/rename 时再细化
		return nil
	})
	if walkErr != nil {
		errs = append(errs, walkErr)
	}

	p.logger.Debug("sftp upload_dir done",
		"sid", p.sid, "files", result.Files, "bytes", result.Bytes, "errors", len(errs))

	if len(errs) > 0 {
		return result, errors.Join(errs...)
	}
	return result, nil
}

// uploadOne 是单文件上传的 helper，复用 p.Upload 的逻辑但用独立 timeout。
// 本 Task 1 直接调 p.Upload。Task 2 加 skip/rename 时会扩展。
func (p *PtyConn) uploadOne(src io.Reader, remotePath string, timeoutMs int) (int, bool, error) {
	return p.Upload(src, remotePath, timeoutMs)
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/ssh/pty/ -run TestUploadDir -v`
Expected: 3 个测试 PASS。

- [ ] **Step 6: 跑全量 pty 测试确认无回归**

Run: `go test ./internal/ssh/pty/ -v`
Expected: 全部 PASS。

- [ ] **Step 7: Commit**

```bash
git add internal/ssh/conn/sftp_dir.go internal/ssh/pty/sftp_dir.go internal/ssh/pty/sftp_dir_test.go
git commit -m "feat(pty): add UploadDir basic (overwrite, sequential)"
```

---

## Task 2: UploadDir conflict policies（skip, rename）

**Files:**
- Modify: `internal/ssh/pty/sftp_dir.go`
- Test: `internal/ssh/pty/sftp_dir_test.go`

**Interfaces:**
- Consumes: Task 1 的 `conn.ConflictSkip` / `conn.ConflictRename` / `conn.DirTransferResult.Skipped` / `conn.DirTransferResult.Renamed`
- Produces: `PtyConn.UploadDir` 完整 conflict policy 支持

- [ ] **Step 1: 写失败的测试——ConflictSkip**

在 `internal/ssh/pty/sftp_dir_test.go` 追加：

```go
// TestUploadDirConflictSkip: 目标已存在 → 跳过，Skipped=1，Files=1
func TestUploadDirConflictSkip(t *testing.T) {
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

	// 先在远端预存 a.txt（旧内容）
	if err := p.sftpClient.MkdirAll("/skipdir"); err != nil {
		t.Fatal(err)
	}
	dst, err := p.sftpClient.Create("/skipdir/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Write([]byte("OLD")); err != nil {
		t.Fatal(err)
	}
	dst.Close()

	// 本地 a.txt（新内容）
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "a.txt"), []byte("NEW"), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := p.UploadDir(localRoot, "/skipdir", conn.DirTransferOptions{Conflict: conn.ConflictSkip})
	if err != nil {
		t.Fatalf("UploadDir: %v", err)
	}
	if res.Skipped != 1 || res.Files != 0 {
		t.Errorf("Skipped=%d Files=%d, want 1/0", res.Skipped, res.Files)
	}

	// 验证远端仍是旧内容
	f, err := p.sftpClient.Open("/skipdir/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := readAll(f)
	f.Close()
	if string(got) != "OLD" {
		t.Errorf("remote = %q, want OLD (skip should not overwrite)", got)
	}
}

// TestUploadDirConflictRename: 目标已存在 → 重命名 a.txt → a_1.txt
func TestUploadDirConflictRename(t *testing.T) {
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

	// 远端预存 a.txt
	if err := p.sftpClient.MkdirAll("/renamedir"); err != nil {
		t.Fatal(err)
	}
	dst, _ := p.sftpClient.Create("/renamedir/a.txt")
	dst.Write([]byte("OLD"))
	dst.Close()

	// 本地 a.txt
	localRoot := t.TempDir()
	os.WriteFile(filepath.Join(localRoot, "a.txt"), []byte("NEW"), 0644)

	res, err := p.UploadDir(localRoot, "/renamedir", conn.DirTransferOptions{Conflict: conn.ConflictRename})
	if err != nil {
		t.Fatalf("UploadDir: %v", err)
	}
	if res.Renamed != 1 || res.Files != 1 {
		t.Errorf("Renamed=%d Files=%d, want 1/1", res.Renamed, res.Files)
	}

	// 验证远端：a.txt 仍是 OLD，a_1.txt 是 NEW
	for _, c := range []struct{ path, want string }{
		{"/renamedir/a.txt", "OLD"},
		{"/renamedir/a_1.txt", "NEW"},
	} {
		f, err := p.sftpClient.Open(c.path)
		if err != nil {
			t.Fatalf("Open %s: %v", c.path, err)
		}
		got, _ := readAll(f)
		f.Close()
		if string(got) != c.want {
			t.Errorf("%s = %q, want %q", c.path, got, c.want)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ssh/pty/ -run 'TestUploadDirConflict' -v`
Expected: FAIL——当前 Task 1 的 UploadDir 不管 Conflict 字段，都按 overwrite 处理。`TestUploadDirConflictSkip` 的远端 a.txt 内容会变成 "NEW" 而非 "OLD"。

- [ ] **Step 3: 实现 conflict policy 分派**

修改 `internal/ssh/pty/sftp_dir.go` 的 `UploadDir`，把文件传输分支改为按 `opts.Conflict` 分派。把 Task 1 的文件处理部分：

```go
// 文件：本 Task 1 只实现 overwrite
f, err := os.Open(localPath)
if err != nil {
    errs = append(errs, err)
    return nil
}
n, timedOut, err := p.uploadOne(f, remotePath, opts.TimeoutMs)
f.Close()
result.Bytes += int64(n)
if err != nil {
    errs = append(errs, err)
}
if timedOut {
    result.TimedOut++
}
// 注：uploadOne 内部用 p.Upload（已实现），ConflictOverwrite 直接覆盖
result.Files++
_ = timedOut // 占位，Task 2 加 skip/rename 时再细化
return nil
```

替换为：

```go
// 文件：按 conflict policy 分派
f, err := os.Open(localPath)
if err != nil {
    errs = append(errs, err)
    return nil
}
defer f.Close()

finalPath, action, err := p.resolveConflict(remotePath, opts.Conflict)
if err != nil {
    errs = append(errs, err)
    return nil
}
switch action {
case conflictSkip:
    result.Skipped++
    return nil
case conflictRename:
    result.Renamed++
}

n, timedOut, err := p.Upload(f, finalPath, opts.TimeoutMs)
result.Bytes += int64(n)
if err != nil {
    errs = append(errs, err)
}
if timedOut {
    result.TimedOut++
}
result.Files++
return nil
```

在 `sftp_dir.go` 顶部加 conflict action 内部类型 + `resolveConflict` helper：

```go
// conflictAction 是 resolveConflict 的内部结果。
type conflictAction int

const (
    conflictOverwrite conflictAction = iota
    conflictSkip
    conflictRename
)

// resolveConflict 根据 policy 决定最终远端路径。
//   - ConflictOverwrite: 直接返回 remotePath，action=overwrite
//   - ConflictSkip: Stat 检查存在；存在返回 skip，不存在返回 overwrite
//   - ConflictRename: 找无冲突路径 name_1.ext、name_2.ext...
//
// 调用前 sftpClient 必须非 nil。
func (p *PtyConn) resolveConflict(remotePath string, policy conn.ConflictPolicy) (finalPath string, action conflictAction, err error) {
    if policy == conn.ConflictOverwrite {
        return remotePath, conflictOverwrite, nil
    }

    // Stat 检查存在性
    _, statErr := p.sftpClient.Stat(remotePath)
    notExist := isNotExist(statErr)

    if policy == conn.ConflictSkip {
        if notExist {
            return remotePath, conflictOverwrite, nil
        }
        if statErr != nil {
            return "", 0, statErr // 其他 Stat 错误
        }
        return remotePath, conflictSkip, nil
    }

    // ConflictRename
    if notExist {
        return remotePath, conflictOverwrite, nil // 无冲突直接用原名
    }
    if statErr != nil {
        return "", 0, statErr
    }
    // 找 name_1、name_2...
    dir := path.Dir(remotePath)
    base := path.Base(remotePath)
    ext := ""
    if dot := indexOfDot(base); dot >= 0 {
        ext = base[dot:]
        base = base[:dot]
    }
    for i := 1; ; i++ {
        candidate := path.Join(dir, base+"_"+itoa(i)+ext)
        if _, e := p.sftpClient.Stat(candidate); e != nil {
            if isNotExist(e) {
                return candidate, conflictRename, nil
            }
            return "", 0, e
        }
    }
}

// isNotExist 判断 sftp.Stat 错误是否表示"文件不存在"。
func isNotExist(err error) bool {
    if err == nil {
        return false
    }
    // os.ErrNotExist 包装 sftp 的 SSH_FX_NO_SUCH_FILE
    return errors.Is(err, os.ErrNotExist)
}

// indexOfDot 返回 base 中最后一个 '.' 的索引（用于切分扩展名）。
// 无点返回 -1。
func indexOfDot(base string) int {
    for i := len(base) - 1; i >= 0; i-- {
        if base[i] == '.' {
            return i
        }
    }
    return -1
}

// itoa 是 strconv.Itoa 的简版（避免新 import）。
func itoa(n int) string {
    if n == 0 {
        return "0"
    }
    var buf [20]byte
    i := len(buf)
    for n > 0 {
        i--
        buf[i] = byte('0' + n%10)
        n /= 10
    }
    return string(buf[i:])
}
```

同时删除 Task 1 中的 `uploadOne` helper（不再用）。

- [ ] **Step 4: 跑 conflict 测试确认通过**

Run: `go test ./internal/ssh/pty/ -run 'TestUploadDirConflict' -v`
Expected: 2 个测试 PASS。

- [ ] **Step 5: 跑全 UploadDir 测试确认无回归**

Run: `go test ./internal/ssh/pty/ -run TestUploadDir -v`
Expected: 5 个测试全 PASS（Task 1 的 3 个 + Task 2 的 2 个）。

- [ ] **Step 6: Commit**

```bash
git add internal/ssh/pty/sftp_dir.go internal/ssh/pty/sftp_dir_test.go
git commit -m "feat(pty): add skip/rename conflict policies to UploadDir"
```

---

## Task 3: UploadDir 并发

**Files:**
- Modify: `internal/ssh/pty/sftp_dir.go`
- Test: `internal/ssh/pty/sftp_dir_test.go`

**Interfaces:**
- Consumes: Task 2 的 `resolveConflict` / `conflictAction`
- Produces: `PtyConn.UploadDir` 支持 `opts.Concurrency` 并发文件传输

- [ ] **Step 1: 写失败的测试——多文件并发传输正确性**

在 `internal/ssh/pty/sftp_dir_test.go` 追加：

```go
// TestUploadDirConcurrency: 10 文件 + Concurrency=4，验证所有文件都正确传输。
// 不直接测并发（难测），但测并发下不丢文件、不内容错乱。
func TestUploadDirConcurrency(t *testing.T) {
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

	// 本地树：10 文件，每个 1KB
	localRoot := t.TempDir()
	wantFiles := map[string]string{}
	for i := 0; i < 10; i++ {
		name := "file_" + string(rune('0'+i)) + ".txt"
		content := string(bytes.Repeat([]byte{byte('a' + i)}, 1024))
		if err := os.WriteFile(filepath.Join(localRoot, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		wantFiles["/concurrent/"+name] = content
	}

	res, err := p.UploadDir(localRoot, "/concurrent", conn.DirTransferOptions{Concurrency: 4})
	if err != nil {
		t.Fatalf("UploadDir: %v", err)
	}
	if res.Files != 10 {
		t.Errorf("Files = %d, want 10", res.Files)
	}
	if res.Bytes != 10*1024 {
		t.Errorf("Bytes = %d, want %d", res.Bytes, 10*1024)
	}

	// 验证每个文件内容
	for remotePath, want := range wantFiles {
		f, err := p.sftpClient.Open(remotePath)
		if err != nil {
			t.Errorf("Open %s: %v", remotePath, err)
			continue
		}
		got, _ := readAll(f)
		f.Close()
		if string(got) != want {
			t.Errorf("%s content mismatch: got %d bytes, want %d", remotePath, len(got), len(want))
		}
	}
}
```

- [ ] **Step 2: 跑测试确认当前实现能否通过**

Run: `go test ./internal/ssh/pty/ -run TestUploadDirConcurrency -v`
Expected: PASS（当前顺序实现也能完成 10 文件传输，正确性不依赖并发）。这个测试是并发下的"正确性不回归"基线，不是失败测试——Task 3 的关键是引入并发不破坏正确性。

- [ ] **Step 3: 重构 UploadDir 加并发 worker pool**

修改 `internal/ssh/pty/sftp_dir.go` 的 `UploadDir`，把顺序循环改为：
1. `filepath.Walk` 收集所有文件路径到 slice（目录处理保留在 Walk 内即时 MkdirAll）
2. 启动 N 个 worker goroutine，从 channel 拉文件路径
3. 每个 worker 调 `resolveConflict` + `p.Upload`，把 result 累加到 mutex 保护的 `DirTransferResult`
4. `sync.WaitGroup` 等所有 worker 完成
5. 收集 errors，用 `errors.Join` 返回

完整重构后的 `UploadDir`：

```go
func (p *PtyConn) UploadDir(localDir, remoteDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error) {
	p.logger.Debug("sftp upload_dir start", "sid", p.sid, "local", localDir, "remote", remoteDir, "opts", opts)

	p.mu.Lock()
	sftpClient := p.sftpClient
	p.mu.Unlock()
	if sftpClient == nil {
		return conn.DirTransferResult{}, conn.ErrSftpUnavailable
	}

	info, err := os.Stat(localDir)
	if err != nil {
		return conn.DirTransferResult{}, err
	}
	if !info.IsDir() {
		return conn.DirTransferResult{}, errors.New("local path is not a directory")
	}

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}

	// 第一遍：Walk 收集所有文件（目录在 Walk 内即时 MkdirAll）
	type fileTask struct {
		localPath, remotePath string
	}
	var tasks []fileTask
	var walkErrs []error

	walkErr := filepath.Walk(localDir, func(localPath string, fi os.FileInfo, err error) error {
		if err != nil {
			walkErrs = append(walkErrs, err)
			return nil
		}
		rel, err := filepath.Rel(localDir, localPath)
		if err != nil {
			walkErrs = append(walkErrs, err)
			return nil
		}
		remotePath := path.Join(remoteDir, filepath.ToSlash(rel))

		if fi.Mode()&os.ModeSymlink != 0 {
			return nil // skip symlinks
		}
		if fi.IsDir() {
			if err := sftpClient.MkdirAll(remotePath); err != nil {
				walkErrs = append(walkErrs, err)
			}
			return nil
		}
		tasks = append(tasks, fileTask{localPath, remotePath})
		return nil
	})
	if walkErr != nil {
		walkErrs = append(walkErrs, walkErr)
	}

	// 第二遍：并发 worker pool 传文件
	taskCh := make(chan fileTask)
	var mu sync.Mutex
	var result conn.DirTransferResult
	var errs []error
	errs = append(errs, walkErrs...)
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			for task := range taskCh {
				f, err := os.Open(task.localPath)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					continue
				}
				finalPath, action, err := p.resolveConflict(task.remotePath, opts.Conflict)
				if err != nil {
					f.Close()
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					continue
				}
				if action == conflictSkip {
					f.Close()
					mu.Lock()
					result.Skipped++
					mu.Unlock()
					continue
				}
				if action == conflictRename {
					mu.Lock()
					result.Renamed++
					mu.Unlock()
				}
				n, timedOut, err := p.Upload(f, finalPath, opts.TimeoutMs)
				f.Close()
				mu.Lock()
				result.Bytes += int64(n)
				result.Files++
				if err != nil {
					errs = append(errs, err)
				}
				if timedOut {
					result.TimedOut++
				}
				mu.Unlock()
			}
		}()
	}
	go func() {
		for _, t := range tasks {
			taskCh <- t
		}
		close(taskCh)
	}()
	wg.Wait()

	p.logger.Debug("sftp upload_dir done",
		"sid", p.sid, "files", result.Files, "bytes", result.Bytes,
		"skipped", result.Skipped, "renamed", result.Renamed,
		"errors", len(errs))

	if len(errs) > 0 {
		return result, errors.Join(errs...)
	}
	return result, nil
}
```

- [ ] **Step 4: 跑 UploadDir 全测试确认无回归**

Run: `go test ./internal/ssh/pty/ -run TestUploadDir -v`
Expected: 6 个测试全 PASS（Task 1 的 3 个 + Task 2 的 2 个 + Task 3 的 1 个）。

- [ ] **Step 5: Commit**

```bash
git add internal/ssh/pty/sftp_dir.go internal/ssh/pty/sftp_dir_test.go
git commit -m "perf(pty): add concurrent worker pool to UploadDir"
```

---

## Task 4: DownloadDir（mirror of UploadDir，all features）

**Files:**
- Modify: `internal/ssh/pty/sftp_dir.go`
- Test: `internal/ssh/pty/sftp_dir_test.go`

**Interfaces:**
- Consumes: Task 3 的 `resolveConflict` / worker pool 模式
- Produces: `PtyConn.DownloadDir(remoteDir, localDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error)`

- [ ] **Step 1: 写失败的测试——DownloadDir 2 文件树**

在 `internal/ssh/pty/sftp_dir_test.go` 追加：

```go
// TestDownloadDirBasic: 先 UploadDir 建远端树，再 DownloadDir 下来，验证内容一致。
func TestDownloadDirBasic(t *testing.T) {
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

	// 本地源树
	localSrc := t.TempDir()
	os.WriteFile(filepath.Join(localSrc, "a.txt"), []byte("aaa"), 0644)
	os.MkdirAll(filepath.Join(localSrc, "sub"), 0755)
	os.WriteFile(filepath.Join(localSrc, "sub", "b.txt"), []byte("bbbbb"), 0644)

	// 上传到远端 /dldir
	if _, err := p.UploadDir(localSrc, "/dldir", conn.DirTransferOptions{}); err != nil {
		t.Fatalf("UploadDir seed: %v", err)
	}

	// 下载到本地空目录
	localDst := t.TempDir()
	res, err := p.DownloadDir("/dldir", localDst, conn.DirTransferOptions{})
	if err != nil {
		t.Fatalf("DownloadDir: %v", err)
	}
	if res.Files != 2 {
		t.Errorf("Files = %d, want 2", res.Files)
	}
	if res.Bytes != 8 {
		t.Errorf("Bytes = %d, want 8", res.Bytes)
	}

	// 验证本地文件
	for _, c := range []struct{ path, want string }{
		{filepath.Join(localDst, "a.txt"), "aaa"},
		{filepath.Join(localDst, "sub", "b.txt"), "bbbbb"},
	} {
		got, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", c.path, err)
		}
		if string(got) != c.want {
			t.Errorf("%s = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestDownloadDirConflictSkip: 远端有 a.txt，本地也有 a.txt → 跳过
func TestDownloadDirConflictSkip(t *testing.T) {
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

	// 远端 a.txt = "REMOTE"
	p.sftpClient.MkdirAll("/skipdl")
	dst, _ := p.sftpClient.Create("/skipdl/a.txt")
	dst.Write([]byte("REMOTE"))
	dst.Close()

	// 本地 a.txt = "LOCAL"
	localDst := t.TempDir()
	os.WriteFile(filepath.Join(localDst, "a.txt"), []byte("LOCAL"), 0644)

	res, err := p.DownloadDir("/skipdl", localDst, conn.DirTransferOptions{Conflict: conn.ConflictSkip})
	if err != nil {
		t.Fatalf("DownloadDir: %v", err)
	}
	if res.Skipped != 1 || res.Files != 0 {
		t.Errorf("Skipped=%d Files=%d, want 1/0", res.Skipped, res.Files)
	}

	got, _ := os.ReadFile(filepath.Join(localDst, "a.txt"))
	if string(got) != "LOCAL" {
		t.Errorf("local = %q, want LOCAL (skip should not overwrite)", got)
	}
}

// TestDownloadDirConflictRename: 远端 a.txt，本地也有 a.txt → 下载为 a_1.txt
func TestDownloadDirConflictRename(t *testing.T) {
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

	p.sftpClient.MkdirAll("/renamedl")
	dst, _ := p.sftpClient.Create("/renamedl/a.txt")
	dst.Write([]byte("REMOTE"))
	dst.Close()

	localDst := t.TempDir()
	os.WriteFile(filepath.Join(localDst, "a.txt"), []byte("LOCAL"), 0644)

	res, err := p.DownloadDir("/renamedl", localDst, conn.DirTransferOptions{Conflict: conn.ConflictRename})
	if err != nil {
		t.Fatalf("DownloadDir: %v", err)
	}
	if res.Renamed != 1 || res.Files != 1 {
		t.Errorf("Renamed=%d Files=%d, want 1/1", res.Renamed, res.Files)
	}

	for _, c := range []struct{ path, want string }{
		{filepath.Join(localDst, "a.txt"), "LOCAL"},
		{filepath.Join(localDst, "a_1.txt"), "REMOTE"},
	} {
		got, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", c.path, err)
		}
		if string(got) != c.want {
			t.Errorf("%s = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestDownloadDirConcurrency: 10 文件 + Concurrency=4，验证正确性
func TestDownloadDirConcurrency(t *testing.T) {
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

	// 本地源：10 文件
	localSrc := t.TempDir()
	wantFiles := map[string]string{}
	for i := 0; i < 10; i++ {
		name := "file_" + string(rune('0'+i)) + ".txt"
		content := string(bytes.Repeat([]byte{byte('a' + i)}, 1024))
		os.WriteFile(filepath.Join(localSrc, name), []byte(content), 0644)
		wantFiles[name] = content
	}

	if _, err := p.UploadDir(localSrc, "/concurrentdl", conn.DirTransferOptions{}); err != nil {
		t.Fatalf("UploadDir seed: %v", err)
	}

	localDst := t.TempDir()
	res, err := p.DownloadDir("/concurrentdl", localDst, conn.DirTransferOptions{Concurrency: 4})
	if err != nil {
		t.Fatalf("DownloadDir: %v", err)
	}
	if res.Files != 10 {
		t.Errorf("Files = %d, want 10", res.Files)
	}

	for name, want := range wantFiles {
		got, err := os.ReadFile(filepath.Join(localDst, name))
		if err != nil {
			t.Errorf("ReadFile %s: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s content mismatch", name)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ssh/pty/ -run TestDownloadDir -v`
Expected: FAIL——`undefined: p.DownloadDir`。

- [ ] **Step 3: 实现 DownloadDir**

在 `internal/ssh/pty/sftp_dir.go` 追加 `DownloadDir` 方法。结构与 `UploadDir` 对称——用 `sftpClient.Walk` 遍历远端树，`os.MkdirAll` 建本地目录，并发 worker pool 下载文件：

```go
// DownloadDir 把远端 remoteDir 整树下载到本地 localDir。
// 与 UploadDir 对称：sftpClient.Walk 遍历，os.MkdirAll 建目录，并发 worker pool 下载。
func (p *PtyConn) DownloadDir(remoteDir, localDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error) {
	p.logger.Debug("sftp download_dir start", "sid", p.sid, "local", localDir, "remote", remoteDir, "opts", opts)

	p.mu.Lock()
	sftpClient := p.sftpClient
	p.mu.Unlock()
	if sftpClient == nil {
		return conn.DirTransferResult{}, conn.ErrSftpUnavailable
	}

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}

	// 第一遍：Walk 收集所有文件（目录在 Walk 内即时 os.MkdirAll）
	type fileTask struct {
		localPath, remotePath string
	}
	var tasks []fileTask
	var walkErrs []error

	walker := sftpClient.Walk(remoteDir)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			walkErrs = append(walkErrs, err)
			continue
		}
		remotePath := walker.Path()
		fi := walker.Stat()

		rel := strings.TrimPrefix(remotePath, remoteDir)
		rel = strings.TrimPrefix(rel, "/")
		localPath := filepath.Join(localDir, filepath.FromSlash(rel))

		if fi.Mode()&os.ModeSymlink != 0 {
			continue // skip symlinks
		}
		if fi.IsDir() {
			if err := os.MkdirAll(localPath, 0755); err != nil {
				walkErrs = append(walkErrs, err)
			}
			continue
		}
		tasks = append(tasks, fileTask{localPath, remotePath})
	}

	// 第二遍：并发 worker pool 下载
	taskCh := make(chan fileTask)
	var mu sync.Mutex
	var result conn.DirTransferResult
	var errs []error
	errs = append(errs, walkErrs...)
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			for task := range taskCh {
				finalPath, action, err := p.resolveLocalConflict(task.localPath, opts.Conflict)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					continue
				}
				if action == conflictSkip {
					mu.Lock()
					result.Skipped++
					mu.Unlock()
					continue
				}
				if action == conflictRename {
					mu.Lock()
					result.Renamed++
					mu.Unlock()
				}
				f, err := os.Create(finalPath)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					continue
				}
				n, timedOut, err := p.Download(task.remotePath, f, opts.TimeoutMs)
				f.Close()
				mu.Lock()
				result.Bytes += int64(n)
				result.Files++
				if err != nil {
					errs = append(errs, err)
				}
				if timedOut {
					result.TimedOut++
				}
				mu.Unlock()
			}
		}()
	}
	go func() {
		for _, t := range tasks {
			taskCh <- t
		}
		close(taskCh)
	}()
	wg.Wait()

	p.logger.Debug("sftp download_dir done",
		"sid", p.sid, "files", result.Files, "bytes", result.Bytes,
		"skipped", result.Skipped, "renamed", result.Renamed,
		"errors", len(errs))

	if len(errs) > 0 {
		return result, errors.Join(errs...)
	}
	return result, nil
}

// resolveLocalConflict 是 resolveConflict 的本地文件版本。
//   - ConflictOverwrite: 直接返回 localPath
//   - ConflictSkip: 本地存在则跳过
//   - ConflictRename: 找 name_1、name_2...
func (p *PtyConn) resolveLocalConflict(localPath string, policy conn.ConflictPolicy) (finalPath string, action conflictAction, err error) {
	if policy == conn.ConflictOverwrite {
		return localPath, conflictOverwrite, nil
	}

	_, statErr := os.Stat(localPath)
	notExist := os.IsNotExist(statErr)

	if policy == conn.ConflictSkip {
		if notExist {
			return localPath, conflictOverwrite, nil
		}
		if statErr != nil {
			return "", 0, statErr
		}
		return localPath, conflictSkip, nil
	}

	// ConflictRename
	if notExist {
		return localPath, conflictOverwrite, nil
	}
	if statErr != nil {
		return "", 0, statErr
	}
	dir := filepath.Dir(localPath)
	base := filepath.Base(localPath)
	ext := ""
	if dot := strings.LastIndex(base, "."); dot >= 0 {
		ext = base[dot:]
		base = base[:dot]
	}
	for i := 1; ; i++ {
		candidate := filepath.Join(dir, base+"_"+itoa(i)+ext)
		if _, e := os.Stat(candidate); os.IsNotExist(e) {
			return candidate, conflictRename, nil
		} else if e != nil {
			return "", 0, e
		}
	}
}
```

需要加 `strings` import。

- [ ] **Step 4: 跑 DownloadDir 测试确认通过**

Run: `go test ./internal/ssh/pty/ -run TestDownloadDir -v`
Expected: 4 个测试全 PASS。

- [ ] **Step 5: 跑全量 pty 测试确认无回归**

Run: `go test ./internal/ssh/pty/ -v`
Expected: 全部 PASS。

- [ ] **Step 6: Commit**

```bash
git add internal/ssh/pty/sftp_dir.go internal/ssh/pty/sftp_dir_test.go
git commit -m "feat(pty): add DownloadDir with conflict policies and concurrency"
```

---

## Task 5: Session 状态机包装 + Conn 接口扩展

**Files:**
- Modify: `internal/ssh/session/session.go`
- Modify: `internal/ssh/session/session_test.go`

**Interfaces:**
- Consumes: Task 4 的 `PtyConn.UploadDir` / `PtyConn.DownloadDir`、`conn.DirTransferOptions` / `conn.DirTransferResult`
- Produces: `Conn` 接口加 `UploadDir` / `DownloadDir` 方法；`Session.UploadDir` / `Session.DownloadDir` 状态机包装；`fakeConn` 扩展支持新方法

- [ ] **Step 1: 写失败的测试——Session.UploadDir 不触发 idle timer**

在 `internal/ssh/session/session_test.go` 追加。**注意：测试变量名用 `fc`（fakeConn），不能用 `conn`——会与 `conn` package import 冲突**（现有 fakeConn 测试用 `conn` 变量名，但那些测试不引用 `conn` package；本测试需要 `conn.DirTransferOptions` 等）：

```go
// TestUploadDirDoesNotFireIdleTimeout: idleTimeout=100ms，UploadDir 阻塞 400ms。
// 修复前：timer 在 100ms 触发 Close。
// 修复后：UploadDir 期间 timer 被 stop，返回后 state=Idle。
func TestUploadDirDoesNotFireIdleTimeout(t *testing.T) {
	fc := newFakeConn()
	fc.sftpEnabled = true
	fc.uploadDirBlock = make(chan struct{}) // 阻塞直到 close
	fc.uploadDirResult = conn.DirTransferResult{Files: 1}

	mgr := NewManager()
	s := mgr.newSessionWithConn("sid", "srv", fc, 100*time.Millisecond, nil)
	defer s.Close()

	go func() {
		time.Sleep(400 * time.Millisecond)
		close(fc.uploadDirBlock)
	}()

	start := time.Now()
	_, err := s.UploadDir("/local", "/remote", conn.DirTransferOptions{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("UploadDir: %v", err)
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("UploadDir returned too fast: %v, want >= 400ms", elapsed)
	}
	if st := s.State(); st != StateIdle {
		t.Errorf("state after UploadDir = %s, want idle", st)
	}
}

// TestDownloadDirDoesNotFireIdleTimeout: 对称测试 DownloadDir。
func TestDownloadDirDoesNotFireIdleTimeout(t *testing.T) {
	fc := newFakeConn()
	fc.sftpEnabled = true
	fc.downloadDirBlock = make(chan struct{})
	fc.downloadDirResult = conn.DirTransferResult{Files: 1}

	mgr := NewManager()
	s := mgr.newSessionWithConn("sid", "srv", fc, 100*time.Millisecond, nil)
	defer s.Close()

	go func() {
		time.Sleep(400 * time.Millisecond)
		close(fc.downloadDirBlock)
	}()

	start := time.Now()
	_, err := s.DownloadDir("/remote", "/local", conn.DirTransferOptions{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("DownloadDir: %v", err)
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("DownloadDir returned too fast: %v, want >= 400ms", elapsed)
	}
	if st := s.State(); st != StateIdle {
		t.Errorf("state after DownloadDir = %s, want idle", st)
	}
}

// TestUploadDirOnClosedSession: session 关闭后 UploadDir 返回 "session closed"。
func TestUploadDirOnClosedSession(t *testing.T) {
	fc := newFakeConn()
	fc.sftpEnabled = true
	mgr := NewManager()
	s := mgr.newSessionWithConn("sid", "srv", fc, time.Minute, nil)
	s.Close()

	_, err := s.UploadDir("/local", "/remote", conn.DirTransferOptions{})
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("UploadDir on closed session: err=%v, want 'session closed'", err)
	}
}

// TestUploadDirBlocksRunInSession: UploadDir 进行中 state=Running，并发 RunInSession 应立即报 "session busy"。
func TestUploadDirBlocksRunInSession(t *testing.T) {
	fc := newFakeConn()
	fc.sftpEnabled = true
	fc.uploadDirBlock = make(chan struct{})

	mgr := NewManager()
	s := mgr.newSessionWithConn("sid", "srv", fc, time.Minute, nil)
	defer s.Close()

	go func() {
		s.UploadDir("/local", "/remote", conn.DirTransferOptions{})
	}()
	time.Sleep(50 * time.Millisecond)

	_, _, _, _, _, err := s.RunInSession("ls", 1000, 0)
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("RunInSession during UploadDir: err=%v, want 'session busy'", err)
	}

	close(fc.uploadDirBlock)
	time.Sleep(50 * time.Millisecond)
	if st := s.State(); st != StateIdle {
		t.Errorf("state after UploadDir done = %s, want idle", st)
	}
}
```

需要 `strings` import（如还没有）。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ssh/session/ -run 'TestUploadDirDoesNotFire|TestDownloadDirDoesNotFire' -v`
Expected: FAIL——`s.UploadDir undefined`、`conn.uploadDirBlock undefined`、`DirTransferOptions undefined`。

- [ ] **Step 3: 扩展 Conn 接口 + 加 Session.UploadDir/DownloadDir**

在 `internal/ssh/session/session.go` 的 `Conn` 接口（约 L37-46）加两个方法：

```go
type Conn interface {
    Close() error
    Run(cmd string, timeoutMs int, maxOutputBytes int) (output string, rawOutput string, exitCode int, timedOut bool, ctrlCSent bool, truncated bool, totalBytes int, connUnusable bool, err error)
    SftpAvailable() bool
    Upload(src io.Reader, remotePath string, timeoutMs int) (bytes int, timedOut bool, err error)
    Download(remotePath string, dst io.Writer, timeoutMs int) (bytes int, timedOut bool, err error)
    UploadDir(localDir, remoteDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error)
    DownloadDir(remoteDir, localDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error)
}
```

在文件末尾加 `Session.UploadDir` / `Session.DownloadDir`（与 `Session.Upload` / `Session.Download` 对称）：

```go
// UploadDir 把本地 localDir 整树上传到远端 remoteDir。
// 状态机与 Upload 对称：进锁检查 state、切 Running + stopIdleTimer、传输、
// 切回 Idle + resetIdleTimer + 更新 lastActivity。
func (s *Session) UploadDir(localDir, remoteDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error) {
    s.mu.Lock()
    if s.state == StateClosed {
        s.mu.Unlock()
        return conn.DirTransferResult{}, errors.New("session closed")
    }
    if s.state == StateRunning {
        s.mu.Unlock()
        return conn.DirTransferResult{}, errors.New("session busy")
    }
    s.state = StateRunning
    s.stopIdleTimer()
    s.mu.Unlock()

    res, err := s.conn.UploadDir(localDir, remoteDir, opts)

    s.mu.Lock()
    s.lastActivity = time.Now()
    if s.state != StateClosed {
        s.state = StateIdle
        s.resetIdleTimer()
    }
    s.mu.Unlock()
    return res, err
}

// DownloadDir 把远端 remoteDir 整树下载到本地 localDir。
// 状态机与 Download 对称。
func (s *Session) DownloadDir(remoteDir, localDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error) {
    s.mu.Lock()
    if s.state == StateClosed {
        s.mu.Unlock()
        return conn.DirTransferResult{}, errors.New("session closed")
    }
    if s.state == StateRunning {
        s.mu.Unlock()
        return conn.DirTransferResult{}, errors.New("session busy")
    }
    s.state = StateRunning
    s.stopIdleTimer()
    s.mu.Unlock()

    res, err := s.conn.DownloadDir(remoteDir, localDir, opts)

    s.mu.Lock()
    s.lastActivity = time.Now()
    if s.state != StateClosed {
        s.state = StateIdle
        s.resetIdleTimer()
    }
    s.mu.Unlock()
    return res, err
}
```

需要加 `sshmng/internal/ssh/conn` import（如果还没有）。

- [ ] **Step 4: 扩展 fakeConn 支持 UploadDir/DownloadDir**

在 `internal/ssh/session/session_test.go` 的 `fakeConn` struct 加字段：

```go
type fakeConn struct {
    // ... 现有字段 ...

    // dir 传输支持（Task 5 测试用）
    uploadDirBlock   chan struct{}       // nil = 不阻塞；非 nil = UploadDir 阻塞直到 close
    downloadDirBlock chan struct{}       // 同上
    uploadDirResult  conn.DirTransferResult
    downloadDirResult conn.DirTransferResult
    uploadDirErr     error
    downloadDirErr   error
}
```

在 fakeConn 方法区加：

```go
func (f *fakeConn) UploadDir(localDir, remoteDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error) {
    if !f.sftpEnabled {
        return conn.DirTransferResult{}, conn.ErrSftpUnavailable
    }
    if f.uploadDirBlock != nil {
        <-f.uploadDirBlock
    }
    return f.uploadDirResult, f.uploadDirErr
}

func (f *fakeConn) DownloadDir(remoteDir, localDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error) {
    if !f.sftpEnabled {
        return conn.DirTransferResult{}, conn.ErrSftpUnavailable
    }
    if f.downloadDirBlock != nil {
        <-f.downloadDirBlock
    }
    return f.downloadDirResult, f.downloadDirErr
}
```

需要确保 `session_test.go` import 了 `sshmng/internal/ssh/conn`。

- [ ] **Step 5: 跑 session 测试确认通过**

Run: `go test ./internal/ssh/session/ -v`
Expected: 全部 PASS（含 2 个新测试）。

- [ ] **Step 6: 跑 pty 测试确认 Conn 接口扩展未破坏实现**

Run: `go test ./internal/ssh/pty/ -v`
Expected: 全部 PASS——`PtyConn` 已实现 UploadDir/DownloadDir（Task 1-4）。

- [ ] **Step 7: Commit**

```bash
git add internal/ssh/session/session.go internal/ssh/session/session_test.go
git commit -m "feat(session): wrap UploadDir/DownloadDir with state machine"
```

---

## Task 6: MCP 工具层 + 注册 + 文档更新

**Files:**
- Create: `internal/mcp/tools_file_dir.go`
- Create: `internal/mcp/tools_file_dir_test.go`
- Modify: `internal/mcp/server.go`
- Modify: `docs/ssh-session-manager-design.md`

**Interfaces:**
- Consumes: Task 5 的 `Session.UploadDir` / `Session.DownloadDir`、`conn.ParseConflictPolicy` / `conn.DirTransferOptions`
- Produces: `Service.UploadDir` / `Service.DownloadDir` MCP handlers；`upload_dir` / `download_dir` 工具注册

- [ ] **Step 1: 写 MCP handler 测试**

Create `internal/mcp/tools_file_dir_test.go`:

```go
package mcp

import (
    "testing"

    "sshmng/internal/ssh/conn"
    "sshmng/internal/ssh/session"
)

// TestParseConflictPolicyFromArgs: "skip" → ConflictSkip，"rename" → ConflictRename，"" / "overwrite" / "unknown" → ConflictOverwrite
func TestParseConflictPolicyFromArgs(t *testing.T) {
    cases := []struct{ in, want string }{
        {"", "overwrite"},
        {"overwrite", "overwrite"},
        {"skip", "skip"},
        {"rename", "rename"},
        {"bogus", "overwrite"}, // 无效值默认 overwrite
    }
    for _, c := range cases {
        got := conn.ParseConflictPolicy(c.in).String()
        if got != c.want {
            t.Errorf("ParseConflictPolicy(%q).String() = %q, want %q", c.in, got, c.want)
        }
    }
}

// TestUploadDirArgsFields: 验证 UploadDirArgs JSON tag 正确
func TestUploadDirArgsFields(t *testing.T) {
    // 仅 smoke test：构造 args，验证字段名与默认行为
    args := UploadDirArgs{SID: "s1", Src: "/local", Dst: "/remote", Conflict: "skip", Concurrency: 4}
    if args.SID != "s1" || args.Src != "/local" || args.Dst != "/remote" || args.Conflict != "skip" || args.Concurrency != 4 {
        t.Errorf("UploadDirArgs fields mismatch: %+v", args)
    }
}

// TestDownloadDirArgsFields: 同上
func TestDownloadDirArgsFields(t *testing.T) {
    args := DownloadDirArgs{SID: "s1", Src: "/remote", Dst: "/local", Conflict: "rename"}
    if args.SID != "s1" || args.Src != "/remote" || args.Dst != "/local" || args.Conflict != "rename" {
        t.Errorf("DownloadDirArgs fields mismatch: %+v", args)
    }
}

// 占位：保证 session 包被引用（避免 unused import）
var _ = session.StateIdle
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcp/ -run 'TestParseConflictPolicyFromArgs|TestUploadDirArgsFields|TestDownloadDirArgsFields' -v`
Expected: FAIL——`undefined: UploadDirArgs` / `DownloadDirArgs`。

- [ ] **Step 3: 实现 MCP handler + args struct**

Create `internal/mcp/tools_file_dir.go`:

```go
package mcp

import (
    "context"

    "github.com/modelcontextprotocol/go-sdk/mcp"

    "sshmng/internal/ssh/conn"
)

// UploadDirArgs 是 upload_dir 工具的入参。
type UploadDirArgs struct {
    SID         string `json:"sid"`
    Src         string `json:"src" jsonschema:"local dir path to upload (must exist)"`
    Dst         string `json:"dst" jsonschema:"remote dir path to write (created if not exists, MkdirAll semantics)"`
    Conflict    string `json:"conflict,omitempty" jsonschema:"optional, default 'overwrite'. One of: overwrite, skip, rename"`
    Concurrency int    `json:"concurrency,omitempty" jsonschema:"optional, default 4. Number of files to transfer in parallel"`
    TimeoutMs   int    `json:"timeout_ms,omitempty" jsonschema:"optional, 0 = default 300000 (300s). Per-file timeout"`
}

// DownloadDirArgs 是 download_dir 工具的入参。
type DownloadDirArgs struct {
    SID         string `json:"sid"`
    Src         string `json:"src" jsonschema:"remote dir path to read"`
    Dst         string `json:"dst" jsonschema:"local dir path to write (created if not exists, MkdirAll semantics)"`
    Conflict    string `json:"conflict,omitempty" jsonschema:"optional, default 'overwrite'. One of: overwrite, skip, rename"`
    Concurrency int    `json:"concurrency,omitempty" jsonschema:"optional, default 4. Number of files to transfer in parallel"`
    TimeoutMs   int    `json:"timeout_ms,omitempty" jsonschema:"optional, 0 = default 300000 (300s). Per-file timeout"`
}

// UploadDir 通过 sftp 把本地目录整树上传到远端。
// 内部用 filepath.Walk + MkdirAll + 并发 worker pool（Concurrency 默认 4）。
// Conflict policy：overwrite（默认）/ skip / rename。per-file 错误不中断整树，用 errors.Join 聚合返回。
func (s *Service) UploadDir(ctx context.Context, req *mcp.CallToolRequest, args UploadDirArgs) (*mcp.CallToolResult, any, error) {
    sess, err := s.manager.Get(args.SID)
    if err != nil {
        return errorResult("%v", err)
    }
    s.sessionLogger(req, args.SID).Debug("upload_dir",
        "sid", args.SID, "server", sess.ServerName(),
        "src", args.Src, "dst", args.Dst, "conflict", args.Conflict, "concurrency", args.Concurrency)

    opts := conn.DirTransferOptions{
        Conflict:    conn.ParseConflictPolicy(args.Conflict),
        Concurrency: args.Concurrency,
        TimeoutMs:   args.TimeoutMs,
    }
    res, err := sess.UploadDir(args.Src, args.Dst, opts)
    if err != nil {
        // per-file 错误聚合后仍返回 result（partial），err 非 nil 时 ok=false
        return textResult(map[string]any{
            "sid":       args.SID,
            "ok":        false,
            "bytes":     res.Bytes,
            "files":     res.Files,
            "skipped":   res.Skipped,
            "renamed":   res.Renamed,
            "timed_out": res.TimedOut,
            "err":       err.Error(),
        })
    }
    return textResult(map[string]any{
        "sid":       args.SID,
        "ok":        true,
        "bytes":     res.Bytes,
        "files":     res.Files,
        "skipped":   res.Skipped,
        "renamed":   res.Renamed,
        "timed_out": res.TimedOut,
    })
}

// DownloadDir 通过 sftp 把远端目录整树下载到本地。
// 语义同 UploadDir，方向相反。
func (s *Service) DownloadDir(ctx context.Context, req *mcp.CallToolRequest, args DownloadDirArgs) (*mcp.CallToolResult, any, error) {
    sess, err := s.manager.Get(args.SID)
    if err != nil {
        return errorResult("%v", err)
    }
    s.sessionLogger(req, args.SID).Debug("download_dir",
        "sid", args.SID, "server", sess.ServerName(),
        "src", args.Src, "dst", args.Dst, "conflict", args.Conflict, "concurrency", args.Concurrency)

    opts := conn.DirTransferOptions{
        Conflict:    conn.ParseConflictPolicy(args.Conflict),
        Concurrency: args.Concurrency,
        TimeoutMs:   args.TimeoutMs,
    }
    res, err := sess.DownloadDir(args.Src, args.Dst, opts)
    if err != nil {
        return textResult(map[string]any{
            "sid":       args.SID,
            "ok":        false,
            "bytes":     res.Bytes,
            "files":     res.Files,
            "skipped":   res.Skipped,
            "renamed":   res.Renamed,
            "timed_out": res.TimedOut,
            "err":       err.Error(),
        })
    }
    return textResult(map[string]any{
        "sid":       args.SID,
        "ok":        true,
        "bytes":     res.Bytes,
        "files":     res.Files,
        "skipped":   res.Skipped,
        "renamed":   res.Renamed,
        "timed_out": res.TimedOut,
    })
}
```

- [ ] **Step 4: 注册 2 个新工具到 server.go**

在 `internal/mcp/server.go` 的 `NewServer` 函数里，File transfer tools 区块（在 `download` 注册之后）加：

```go
    mcp.AddTool(server, &mcp.Tool{
        Name:        "upload_dir",
        Description: "Upload a local directory tree to the remote host via sftp. Walks the local tree, creates remote dirs (MkdirAll), transfers files concurrently (default 4). Conflict policy: overwrite (default) / skip / rename. Per-file errors don't abort the transfer; aggregated in result. Requires sftp_available=true on the session.",
    }, svc.UploadDir)
    mcp.AddTool(server, &mcp.Tool{
        Name:        "download_dir",
        Description: "Download a remote directory tree to local via sftp. Walks the remote tree (sftp.Walk), creates local dirs (os.MkdirAll), transfers files concurrently (default 4). Conflict policy: overwrite (default) / skip / rename. Per-file errors don't abort the transfer; aggregated in result. Requires sftp_available=true on the session.",
    }, svc.DownloadDir)
```

- [ ] **Step 5: 跑 MCP 测试确认通过**

Run: `go test ./internal/mcp/ -run 'TestParseConflictPolicyFromArgs|TestUploadDirArgsFields|TestDownloadDirArgsFields' -v`
Expected: 3 个测试 PASS。

- [ ] **Step 6: 跑全量测试 + vet**

Run: `go test ./... && go vet ./...`
Expected: 全部 PASS，vet 无警告。

- [ ] **Step 7: 更新设计文档**

在 `docs/ssh-session-manager-design.md` §3.3 文件传输区块（现有 `upload` / `download` 之后）加：

```text
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
```

也在 §3.3 的 "为什么单独提供 upload_dir/download_dir 工具" 加一句解释：

```text
**为什么加 upload_dir/download_dir：** 单文件 upload 强迫 Agent 用 N 次 MCP 往返编排文件夹传输——每个文件一次 SSH channel 操作，慢且容易半途失败留脏状态。upload_dir 把 Walk + MkdirAll + 并发 + conflict policy 封装成一次调用，Agent 编排复杂度降下来，trace 也集中在一条调用里便于诊断。sftp 协议本身没有"递归传输"原语，专业 sftp 工具都是这样自己实现的。
```

- [ ] **Step 8: Commit**

```bash
git add internal/mcp/tools_file_dir.go internal/mcp/tools_file_dir_test.go internal/mcp/server.go docs/ssh-session-manager-design.md
git commit -m "feat(mcp): add upload_dir/download_dir tools"
```

---

## Self-Review 检查清单

实施完成后逐项确认：

- [ ] **6 个 Task 全部通过 review**：每个 task 的 spec compliance + code quality 都 Approved
- [ ] **现有 sftp 测试不回归**：`internal/ssh/pty/sftp_test.go` 的 `TestUpload*` / `TestDownload*` 全过
- [ ] **Conn 接口对称**：`Upload`/`UploadDir`、`Download`/`DownloadDir` 状态语义对称
- [ ] **ConflictPolicy 三种都覆盖**：overwrite / skip / rename 各有测试
- [ ] **并发不破坏正确性**：`TestUploadDirConcurrency` / `TestDownloadDirConcurrency` 10 文件全过
- [ ] **per-file 错误聚合**：`errors.Join` 返回，不中断整树
- [ ] **MCP 工具注册**：`stat()` 应该能看到新工具数量从 12 涨到 14（如统计）
- [ ] **设计文档同步**：§3.3 加了 `upload_dir` / `download_dir` 条目
- [ ] **vet 无警告**：`go vet ./...`
- [ ] **commit 历史清晰**：6 个 commit，每个对应一个 Task，message 以 `feat(...)` / `perf(...)` / `fix(...)` 开头

## 已知风险与权衡

1. **ConflictRename 在高并发下可能 race**：两个 worker 同时检查 `a_1.txt` 不存在 → 都决定用 `a_1.txt` → 一个写成功，另一个覆盖。v1 接受此风险（rename 主要用于 dst 已有少量冲突的场景，concurrent rename 同名概率低）。如需严格，可加 per-dir mutex 串行化 rename 决策。

2. **fake sftp server 的 InMemHandler 不支持权限**：测试无法验证权限保留——但 v1 本就不保留权限（YAGNI），无需测试。

3. **symlink 跳过策略**：v1 跳过 symlink 不计入任何计数。若用户需要"symlink 复制为对应文件"，留 v2。

4. **大目录内存占用**：`filepath.Walk` 第一遍收集所有文件 tasks 到 slice，对超大目录（10万+文件）有内存压力。可改为流式（边 walk 边投 taskCh），但增加复杂度。v1 接受 slice 方案，留 v2 优化。

5. **per-file 超时聚合**：`TimeoutMs` 是 per-file。整个 dir 传输无总超时——大目录可能传很久。Agent 可用 MCP client 级别的超时（如 600s）兜底，或拆成多个 `upload_dir` 调用分批传。v1 不加总超时。

6. **Progress reporting 缺失**：MCP 协议同步，工具执行中不能流式上报进度。Agent 只能在工具返回后看到汇总。大目录传输时 Agent 会"卡住"等返回——这是 MCP 协议固有限制，无法在工具层解决。
