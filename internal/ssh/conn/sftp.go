package conn

import (
	"fmt"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SftpDialTimeout 是 login 时建立 sftp 通道的超时。sftp 不可用不影响 login 成功，
// 仅决定 upload/download 可用性。
const SftpDialTimeout = 5 * time.Second

// DefaultTransferTimeout 是 Upload/Download 未指定 timeoutMs 时的默认超时。
const DefaultTransferTimeout = 300 * time.Second

// ErrSftpUnavailable 是 sftp 通道未建立时 Upload/Download 返回的错误。
var ErrSftpUnavailable = fmt.Errorf("sftp not available for this session")

// NewSftpClient 在已有 SSH 连接上建立 sftp 通道，5s 超时。
// 失败返回 error（调用方应把 sftpClient 留空，不影响 login 成功）。
//
// sftp.NewClient 内部 open session + RequestSubsystem 都是同步的，但 ssh.Client
// 没有per-operation timeout；用 goroutine + select 实现总超时。超时后 goroutine
// 仍会泄漏直到 server 响应或连接断开，但 login 流程不被阻塞。
func NewSftpClient(client *ssh.Client) (*sftp.Client, error) {
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
	case <-time.After(SftpDialTimeout):
		return nil, fmt.Errorf("sftp channel establishment timed out after %s", SftpDialTimeout)
	}
}
