package conn

import "testing"

// TestNewSftpClientNilSafe 验证 NewSftpClient 对 nil ssh.Client 不 panic。
// 防御性检查：生产调用方（TryEnableSftp）总是传非 nil，但本测试守住边界。
// MaxPacket 选项的正确性由 pty 层 sftp 测试 + Task 7 benchmark 端到端验证。
func TestNewSftpClientNilSafe(t *testing.T) {
	_, err := NewSftpClient(nil)
	if err == nil {
		t.Errorf("NewSftpClient(nil) should return error, got nil")
	}
}
