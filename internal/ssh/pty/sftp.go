package pty

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"sshmng/internal/ssh/conn"
)

// SftpAvailable 返回 sftp 通道是否在 login 时成功建立。
// false 时 Upload/Download 会返回 conn.ErrSftpUnavailable。
func (p *PtyConn) SftpAvailable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sftpClient != nil
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
	// sftp.File.Close 不会重复发 SSH_FXP_CLOSE 网络包，但第二次调用返回 os.ErrClosed
	// （defer 丢弃该错误，无功能影响）。
	stop := context.AfterFunc(ctx, func() {
		dst.Close()
	})

	n, err := io.Copy(dst, newCtxReader(src, ctx))
	stop()
	timedOut := ctx.Err() == context.DeadlineExceeded
	p.logger.Debug("sftp upload done",
		"sid", p.sid, "remote", remotePath, "bytes", n, "timed_out", timedOut)
	return int(n), timedOut, err
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
