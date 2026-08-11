package pty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jim58246/sshmng/internal/ssh/conn"
	"github.com/pkg/sftp"
)

// SftpAvailable 返回 sftp 通道是否在 login 时成功建立。
// false 时 Upload/Download 会返回 conn.ErrSftpUnavailable。
func (p *PtyConn) SftpAvailable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sftpClient != nil
}

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

// Upload 把 src 上传到远端 remotePath。
//   - timeoutMs=0 用默认 300s
//   - 返回 (已传输字节数, 是否超时, error)
//   - sftp 通道未建立时返回 conn.ErrSftpUnavailable
//   - 超时返回已传输字节 + timed_out=true
//
// 用 io.Copy 触发 *sftp.File.ReadFrom 的内置并发 pipelining——多个 SSH_FXP_WRITE
// 包同时在飞，ack 异步回收，把跨地域 RTT 摊薄。超时通过 context.AfterFunc 关闭
// sftp.File 解除 io.Copy 阻塞：在飞的 Write 收到 close 通知后失败返回。
//
// 原子写：先 Create 临时路径 <remotePath>.sshmng-tmp-<hex>，io.Copy 完成后
// finalizeUpload 做 Sync(best-effort)+PosixRename 原子替换为目标路径。失败/超时时
// 删除临时文件，不残留半写目标。
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

	tmpPath := conn.AtomicRemotePath(remotePath)
	dst, err := sftpClient.Create(tmpPath)
	if err != nil {
		return 0, false, fmt.Errorf("create remote %s: %w", tmpPath, err)
	}

	// ctx 到期时关闭 dst，解除 io.Copy 在 dst.Write（内部 ReadFrom）上的阻塞。
	// sftp.File.Close 不会重复发 SSH_FXP_CLOSE 网络包，但第二次调用返回 os.ErrClosed
	// （finalizeUpload 忽略该错误，无功能影响）。
	stop := context.AfterFunc(ctx, func() {
		dst.Close()
	})

	n, err := io.Copy(dst, newCtxReader(src, ctx))
	stop()
	timedOut := ctx.Err() == context.DeadlineExceeded
	p.logger.Debug("sftp upload done",
		"sid", p.sid, "remote", remotePath, "bytes", n, "timed_out", timedOut)
	return p.finalizeUpload(sftpClient, dst, tmpPath, remotePath, int(n), timedOut, err)
}

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

	tmpPath := conn.AtomicRemotePath(remotePath)
	dst, err := sftpClient.Create(tmpPath)
	if err != nil {
		return 0, false, fmt.Errorf("create remote %s: %w", tmpPath, err)
	}

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
	return p.finalizeUpload(sftpClient, dst, tmpPath, remotePath, int(n), timedOut, err)
}

// finalizeUpload closes the temp sftp file, then atomically renames it to
// remotePath (PosixRename; fallback Remove+Rename). On any error after the
// copy, removes the temp so no half-written target remains. Sync is best-effort
// (fsync@openssh.com may be unsupported; ignore that error). Returns the copy's
// (n, timedOut, err) unchanged on success; on rename failure returns a rename
// error and removes the temp.
//
// sftpClient 由调用方在 p.mu 下捕获后传入，避免此处 unlocked 读取 p.sftpClient
// 与 Close()（会置 p.sftpClient=nil）竞争——参照 sftp_dir.go 的 resolveConflict
// 模式。并发 Close 时仍能可靠 Remove 临时文件，不残留。
func (p *PtyConn) finalizeUpload(sftpClient *sftp.Client, dst *sftp.File, tmpPath, remotePath string, n int, timedOut bool, copyErr error) (int, bool, error) {
	// Best-effort fsync; ignore unsupported. dst 可能已被 AfterFunc 关闭（超时），
	// 此时 Sync 返回 os.ErrClosed——忽略。
	if err := dst.Sync(); err != nil && !errors.Is(err, os.ErrClosed) {
		// fsync unsupported → not fatal. Log at debug.
		p.logger.Debug("sftp fsync skipped", "remote", remotePath, "err", err.Error())
	}
	dst.Close()

	if copyErr != nil || timedOut {
		sftpClient.Remove(tmpPath)
		return n, timedOut, copyErr
	}
	// Success: atomic rename.
	if err := sftpClient.PosixRename(tmpPath, remotePath); err != nil {
		// Fallback: standard Rename (non-atomic replace) after Remove.
		p.logger.Debug("posix-rename unsupported, falling back", "remote", remotePath, "err", err.Error())
		sftpClient.Remove(remotePath)
		if err2 := sftpClient.Rename(tmpPath, remotePath); err2 != nil {
			sftpClient.Remove(tmpPath)
			return n, timedOut, fmt.Errorf("rename %s -> %s: %w (posix: %v)", tmpPath, remotePath, err2, err)
		}
	}
	return n, timedOut, nil
}

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

	n, err := io.Copy(&ctxWriter{w: dst, ctx: ctx}, src)
	stop()
	timedOut := ctx.Err() == context.DeadlineExceeded
	p.logger.Debug("sftp download done",
		"sid", p.sid, "remote", remotePath, "bytes", n, "timed_out", timedOut)
	return int(n), timedOut, err
}

// DownloadToFile downloads remotePath to a local file atomically: writes to a
// temp file in the same directory (os.CreateTemp), then os.Rename to localPath
// on success. On error/timeout the temp is removed, leaving no half-written
// target. Used by CLI and MCP download paths. Same pipelining as Download
// (writes go through the existing Download into the temp *os.File).
//
// onBytes, if non-nil, is invoked with the cumulative byte count after each
// successful Write — used by the CLI progress bar. It may be nil.
func (p *PtyConn) DownloadToFile(remotePath, localPath string, timeoutMs int, onBytes func(int64)) (int, bool, error) {
	dir := filepath.Dir(localPath)
	base := filepath.Base(localPath) + ".sshmng-tmp-*"
	tmp, err := os.CreateTemp(dir, base)
	if err != nil {
		return 0, false, fmt.Errorf("create temp for %s: %w", localPath, err)
	}
	tmpPath := tmp.Name()
	// Ensure temp is removed on any non-success exit.
	success := false
	defer func() {
		if !success {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	var w io.Writer = tmp
	if onBytes != nil {
		w = &countingWriter{W: tmp, Fn: onBytes}
	}
	n, timedOut, err := p.Download(remotePath, w, timeoutMs)
	if cerr := tmp.Close(); cerr != nil {
		// Closing the temp failed — surface but still clean up.
		if err == nil {
			err = cerr
		}
	}
	if err != nil || timedOut {
		return n, timedOut, err
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		return n, timedOut, fmt.Errorf("rename temp -> %s: %w", localPath, err)
	}
	success = true
	return n, timedOut, nil
}

// ctxReader 在每次 Read 前检查 ctx.Err()。用于 Upload 路径，让 *sftp.File.ReadFrom
// 在 ctx 取消时能及时退出——否则 ReadFrom 持有 f.mu，AfterFunc 的 dst.Close() 会
// 阻塞直到 ReadFrom 返回（而 ReadFrom 等 src.Read 返回才返回）。
type ctxReader struct {
	r   io.Reader
	ctx context.Context
}

func (cr *ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}

// ctxReaderWithStat 在 ctxReader 基础上保留底层 reader 的 Stat() 方法。
//
// 必要性：*sftp.File.ReadFrom 在 useConcurrentWrites=true 时通过 type switch 检查
// reader 是否实现 Len/Size/Stat/*io.LimitedReader，匹配才走 readFromWithConcurrency
// 并发 pipelining 路径；否则退化为串行 writeChunkAt 循环（每包阻塞等 ack）。
// ctxReader 只实现 Read 会隐藏 *os.File 的 Stat()，导致上传慢一个数量级。
type ctxReaderWithStat struct {
	r   io.Reader
	ctx context.Context
}

func (cr *ctxReaderWithStat) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}

func (cr *ctxReaderWithStat) Stat() (os.FileInfo, error) {
	return cr.r.(interface{ Stat() (os.FileInfo, error) }).Stat()
}

// newCtxReader 用 ctx.Err() 检查包装 r。若 r 暴露 Stat()（如 *os.File），返回
// *ctxReaderWithStat 以保留 Stat——让 *sftp.File.ReadFrom 走并发 pipelining 路径。
// 否则返回 *ctxReader（无 Stat），ReadFrom 退化为串行（与无包装时的行为一致）。
func newCtxReader(r io.Reader, ctx context.Context) io.Reader {
	if _, ok := r.(interface{ Stat() (os.FileInfo, error) }); ok {
		return &ctxReaderWithStat{r: r, ctx: ctx}
	}
	return &ctxReader{r: r, ctx: ctx}
}

// ctxWriter 在每次 Write 前检查 ctx.Err()。用于 Download 路径，让 *sftp.File.WriteTo
// 在 ctx 取消时能及时退出——否则 WriteTo 持有 f.mu，AfterFunc 的 src.Close() 会阻塞。
type ctxWriter struct {
	w   io.Writer
	ctx context.Context
}

func (cw *ctxWriter) Write(p []byte) (int, error) {
	if err := cw.ctx.Err(); err != nil {
		return 0, err
	}
	return cw.w.Write(p)
}

// countingWriter 包装一个 io.Writer，累计已写字节并在每次成功 Write 后通过 Fn
// 回调上报累计字节数。用于 DownloadToFile 把下载进度喂给 CLI 进度条。Fn 可为 nil。
// 定义在 pty 包内（不导入 internal/progress）以避免分层倒置——这里只需一个 5 行的
// 透传 writer。
type countingWriter struct {
	W  io.Writer
	n  int64
	Fn func(int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.W.Write(p)
	if n > 0 {
		c.n += int64(n)
		if c.Fn != nil {
			c.Fn(c.n)
		}
	}
	return n, err
}
