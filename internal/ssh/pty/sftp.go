package pty

import (
	"context"
	"errors"
	"fmt"
	"io"
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

	n, err := io.Copy(dst, src)
	stop()
	timedOut := ctx.Err() == context.DeadlineExceeded
	p.logger.Debug("sftp upload done",
		"sid", p.sid, "remote", remotePath, "bytes", n, "timed_out", timedOut)
	return int(n), timedOut, err
}

// Download 把远端 remotePath 的内容下载到 dst。
//   - timeoutMs=0 用默认 300s
//   - 返回 (已传输字节数, 是否超时, error)
//   - sftp 通道未建立时返回 conn.ErrSftpUnavailable
//   - 超时返回已传输字节 + timed_out=true
func (p *PtyConn) Download(remotePath string, dst io.Writer, timeoutMs int) (int, bool, error) {
	p.logger.Debug("sftp download start", "remote", remotePath, "timeout_ms", timeoutMs)
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
		p.logger.Warn("sftp download open failed",
			"remote", remotePath, "err", err.Error())
		return 0, false, fmt.Errorf("open remote %s: %w", remotePath, err)
	}
	defer src.Close()

	n, err := copyCtx(ctx, dst, src)
	timedOut := errors.Is(err, context.DeadlineExceeded)
	if err != nil && !timedOut {
		p.logger.Warn("sftp download copy failed",
			"remote", remotePath, "bytes", n, "err", err.Error())
	}
	p.logger.Debug("sftp download done",
		"remote", remotePath, "bytes", n, "timed_out", timedOut)
	return int(n), timedOut, err
}

// copyCtx 是 context-aware 版的 io.Copy：每次 Read/Write 前检查 ctx.Err()。
// 超时时返回已传输字节 + context.DeadlineExceeded，调用方可据 errors.Is 判断。
// 不用 io.Copy + context.AfterFunc 是因为后者无法中断阻塞中的 sftp.Write（SSH
// channel 上的同步写），但可在每次迭代间早退，避免无限等待。
func copyCtx(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			total += int64(nw)
			if werr != nil {
				return total, werr
			}
			if nw < nr {
				return total, io.ErrShortWrite
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}
