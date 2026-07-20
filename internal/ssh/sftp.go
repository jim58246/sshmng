package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// sftpDialTimeout 是 login 时建立 sftp 通道的超时。sftp 不可用不影响 login 成功，
// 仅决定 upload/download 可用性。
const sftpDialTimeout = 5 * time.Second

// defaultTransferTimeout 是 Upload/Download 未指定 timeoutMs 时的默认超时。
const defaultTransferTimeout = 300 * time.Second

// errSftpUnavailable 是 sftp 通道未建立时 Upload/Download 返回的错误。
var errSftpUnavailable = errors.New("sftp not available for this session")

// newSftpClient 在已有 SSH 连接上建立 sftp 通道，5s 超时。
// 失败返回 error（调用方应把 sftpClient 留空，不影响 login 成功）。
//
// sftp.NewClient 内部 open session + RequestSubsystem 都是同步的，但 ssh.Client
// 没有per-operation timeout；用 goroutine + select 实现总超时。超时后 goroutine
// 仍会泄漏直到 server 响应或连接断开，但 login 流程不被阻塞。
func newSftpClient(client *ssh.Client) (*sftp.Client, error) {
	type result struct {
		c   *sftp.Client
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := sftp.NewClient(client)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-time.After(sftpDialTimeout):
		return nil, fmt.Errorf("sftp channel establishment timed out after %s", sftpDialTimeout)
	}
}

// SftpAvailable 返回 sftp 通道是否在 login 时成功建立。
// false 时 Upload/Download 会返回 errSftpUnavailable。
func (p *PtyConn) SftpAvailable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sftpClient != nil
}

// Upload 把 src 的内容上传到远端 remotePath。
//   - timeoutMs=0 用默认 300s
//   - 返回 (已传输字节数, 是否超时, error)
//   - sftp 通道未建立时返回 errSftpUnavailable
//   - 超时返回已传输字节 + timed_out=true；error 为 context.DeadlineExceeded 包装
//
// 用 context-aware io.Copy 在 Read/Write 迭代间检查 deadline。
func (p *PtyConn) Upload(src io.Reader, remotePath string, timeoutMs int) (int, bool, error) {
	p.mu.Lock()
	sftpClient := p.sftpClient
	p.mu.Unlock()
	if sftpClient == nil {
		return 0, false, errSftpUnavailable
	}

	timeout := defaultTransferTimeout
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

	n, err := copyCtx(ctx, dst, src)
	timedOut := errors.Is(err, context.DeadlineExceeded)
	return int(n), timedOut, err
}

// Download 把远端 remotePath 的内容下载到 dst。
//   - timeoutMs=0 用默认 300s
//   - 返回 (已传输字节数, 是否超时, error)
//   - sftp 通道未建立时返回 errSftpUnavailable
//   - 超时返回已传输字节 + timed_out=true
func (p *PtyConn) Download(remotePath string, dst io.Writer, timeoutMs int) (int, bool, error) {
	p.mu.Lock()
	sftpClient := p.sftpClient
	p.mu.Unlock()
	if sftpClient == nil {
		return 0, false, errSftpUnavailable
	}

	timeout := defaultTransferTimeout
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

	n, err := copyCtx(ctx, dst, src)
	timedOut := errors.Is(err, context.DeadlineExceeded)
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
