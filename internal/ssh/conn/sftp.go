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

// SftpMaxPacket 是 sftp 单个 SSH_FXP_WRITE/READ 包的最大 payload 字节数。
// 默认 32KB 偏小（跨地域 RTT 高时 ack 次数多），调到 64KB 减半 ack 次数。
// 再大（128KB+）会撞 SSH channel window 默认 2MB 的边界，且边际收益递减。
//
// 注意：必须用 sftp.MaxPacketUnchecked 而非 sftp.MaxPacket 设置。pkg/sftp v1.13.11
// 的 MaxPacket（=MaxPacketChecked）会拒绝 > 32KB 的 size 并返回 error
// "sizes larger than 32KB might not work with all servers"，导致 NewSftpClient
// 失败、sftp 通道恒不可用。sshmng 只连自己控制的 server（OpenSSH 默认支持 256KB
// 包），用 Unchecked 变体绕过库的保守校验。
const SftpMaxPacket = 64 * 1024

// NewSftpClient 在已有 SSH 连接上建立 sftp 通道，5s 超时。
// 失败返回 error（调用方应把 sftpClient 留空，不影响 login 成功）。
//
// sftp.NewClient 内部 open session + RequestSubsystem 都是同步的，但 ssh.Client
// 没有per-operation timeout；用 goroutine + select 实现总超时。超时后 goroutine
// 仍会泄漏直到 server 响应或连接断开，但 login 流程不被阻塞。
//
// client == nil 时显式返回 error：sftp.NewClient(nil) 在 pkg/sftp v1.13.11 会
// panic（ssh.Client.NewSession 解引用 nil），而 NewSftpClient 的语义是"失败返回
// error 不 panic"。生产调用方 TryEnableSftp 总是传非 nil，此处为防御性边界守护。
func NewSftpClient(client *ssh.Client) (*sftp.Client, error) {
	if client == nil {
		return nil, fmt.Errorf("ssh client is nil")
	}
	type result struct {
		c   *sftp.Client
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := sftp.NewClient(client, sftp.MaxPacketUnchecked(SftpMaxPacket))
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-time.After(SftpDialTimeout):
		return nil, fmt.Errorf("sftp channel establishment timed out after %s", SftpDialTimeout)
	}
}
