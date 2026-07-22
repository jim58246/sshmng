package conn

import (
	"crypto/rand"
	"crypto/rsa"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// generateTestHostKey 生成一个 RSA host key 用于测试。
func generateTestHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pub, err := ssh.NewPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatalf("new pubkey: %v", err)
	}
	return pub
}

// newKnownHostsStore 在临时目录创建一个空 known_hosts 文件路径的 Store。
func newKnownHostsStore(t *testing.T) *KnownHostsStore {
	t.Helper()
	return NewKnownHostsStore(filepath.Join(t.TempDir(), "known_hosts"))
}

func TestKnownHostsFirstConnectionRecords(t *testing.T) {
	store := newKnownHostsStore(t)
	key := generateTestHostKey(t)

	got, err := store.Check("h:22", key)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got != HostKeyNew {
		t.Errorf("first Check = %v, want HostKeyNew", got)
	}

	// 文件应该被写入
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Errorf("perm = %o, want 0600", perm)
		}
	}

	// 第二次 Check 应该是 HostKeyMatch
	got, err = store.Check("h:22", key)
	if err != nil {
		t.Fatalf("second Check: %v", err)
	}
	if got != HostKeyMatch {
		t.Errorf("second Check = %v, want HostKeyMatch", got)
	}
}

func TestKnownHostsKeyChangedRejected(t *testing.T) {
	store := newKnownHostsStore(t)
	key1 := generateTestHostKey(t)
	key2 := generateTestHostKey(t)

	// 首次记录 key1
	if _, err := store.Check("h:22", key1); err != nil {
		t.Fatalf("first Check: %v", err)
	}

	// 用 key2 连接同一个 host，应拒绝
	got, err := store.Check("h:22", key2)
	if err == nil {
		t.Fatalf("expected error for changed host key, got nil (result=%v)", got)
	}
	if !strings.Contains(err.Error(), "host key changed") {
		t.Errorf("error should mention 'host key changed', got: %v", err)
	}
	if !strings.Contains(err.Error(), "MITM") {
		t.Errorf("error should warn about MITM, got: %v", err)
	}
	if got != HostKeyChanged {
		t.Errorf("got = %v, want HostKeyChanged", got)
	}
}

func TestKnownHostsDifferentHostsIndependent(t *testing.T) {
	store := newKnownHostsStore(t)
	key1 := generateTestHostKey(t)
	key2 := generateTestHostKey(t)

	if _, err := store.Check("h1:22", key1); err != nil {
		t.Fatalf("Check h1: %v", err)
	}
	if _, err := store.Check("h2:22", key2); err != nil {
		t.Fatalf("Check h2: %v", err)
	}
	// 两个 host 各自的 key 独立存储
	got, err := store.Check("h1:22", key1)
	if err != nil || got != HostKeyMatch {
		t.Errorf("h1 recheck: got=%v err=%v, want HostKeyMatch", got, err)
	}
	got, err = store.Check("h2:22", key2)
	if err != nil || got != HostKeyMatch {
		t.Errorf("h2 recheck: got=%v err=%v, want HostKeyMatch", got, err)
	}
}

func TestKnownHostsFileNotExistTreatedAsEmpty(t *testing.T) {
	store := newKnownHostsStore(t)
	// 文件还不存在，第一次 Check 应返回 HostKeyNew 并创建文件
	key := generateTestHostKey(t)
	got, err := store.Check("h:22", key)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got != HostKeyNew {
		t.Errorf("got = %v, want HostKeyNew", got)
	}
}

func TestKnownHostsRejectsWidePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("known_hosts permission check skipped on Windows (NTFS ACL model)")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(path, []byte("h:22 c29tZWtleQ==\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	store := NewKnownHostsStore(path)
	key := generateTestHostKey(t)
	_, err := store.Check("h:22", key)
	if err == nil {
		t.Errorf("expected error for wide permissions")
	}
	if !strings.Contains(err.Error(), "permissions too open") {
		t.Errorf("error should mention permissions, got: %v", err)
	}
}

func TestKnownHostsAtomicWrite(t *testing.T) {
	// 验证 Record 写入时不留临时文件
	store := newKnownHostsStore(t)
	key := generateTestHostKey(t)
	if _, err := store.Check("h:22", key); err != nil {
		t.Fatalf("Check: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(store.Path()))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("found temp file residue: %s", e.Name())
		}
	}
}

func TestKnownHostsLoadExistingFile(t *testing.T) {
	// 已有 known_hosts 文件，新 Store 加载后应能用
	store := newKnownHostsStore(t)
	key := generateTestHostKey(t)
	if _, err := store.Check("h:22", key); err != nil {
		t.Fatalf("first Check: %v", err)
	}

	// 新 Store 加载同一文件
	store2 := NewKnownHostsStore(store.Path())
	got, err := store2.Check("h:22", key)
	if err != nil {
		t.Fatalf("store2 Check: %v", err)
	}
	if got != HostKeyMatch {
		t.Errorf("got = %v, want HostKeyMatch", got)
	}
}
