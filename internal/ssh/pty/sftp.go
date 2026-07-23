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

// Upload 把 src 的内容上传到远端 remotePath。
//   - timeoutMs=0 用默认 300s
//   - 返回 (已传输字节数, 是否超时, error)
//   - sftp 通道未建立时返回 conn.ErrSftpUnavailable
//   - 超时返回已传输字节 + timed_out=true；error 为 context.DeadlineExceeded 包装
//
// 用 context-aware io.Copy 在 Read/Write 迭代间检查 deadline。
func (p *PtyConn) Upload(src io.Reader, remotePath string, timeoutMs int) (int, bool, error) {
	p.logger.Debug("sftp upload start", "remote", remotePath, "timeout_ms", timeoutMs)
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
		p.logger.Warn("sftp upload create failed",
			"remote", remotePath, "err", err.Error())
		return 0, false, fmt.Errorf("create remote %s: %w", remotePath, err)
	}
	defer dst.Close()

	n, err := copyCtx(ctx, dst, src)
	timedOut := errors.Is(err, context.DeadlineExceeded)
	if err != nil && !timedOut {
		p.logger.Warn("sftp upload copy failed",
			"remote", remotePath, "bytes", n, "err", err.Error())
	}
	p.logger.Debug("sftp upload done",
		"remote", remotePath, "bytes", n, "timed_out", timedOut)
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
