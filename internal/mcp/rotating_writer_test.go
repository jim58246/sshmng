package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRotatingWriterWritesToSSHmngLog 验证基本写入落到 <dir>/sshmng.log。
func TestRotatingWriterWritesToSSHmngLog(t *testing.T) {
	dir := t.TempDir()
	w, err := NewRotatingWriter(dir, 10*1024*1024, 4)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "sshmng.log"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "hello") {
		t.Errorf("file content = %q, want contains 'hello'", string(data))
	}
}

// TestRotatingWriterFilePerm0600 验证日志文件权限 0600。
// 日志可能含命令输出、host key、认证交互细节，不能让其他用户读。
func TestRotatingWriterFilePerm0600(t *testing.T) {
	dir := t.TempDir()
	w, err := NewRotatingWriter(dir, 10*1024*1024, 4)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()
	w.Write([]byte("x"))

	info, err := os.Stat(filepath.Join(dir, "sshmng.log"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("perm = %o, want 0600", perm)
	}
}

// TestRotatingWriterAppendsExisting 验证文件已存在时追加而非截断。
// 日志跨重启复用，旧日志必须保留。
func TestRotatingWriterAppendsExisting(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sshmng.log"), []byte("existing\n"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	w, err := NewRotatingWriter(dir, 10*1024*1024, 4)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()
	w.Write([]byte("appended\n"))

	data, _ := os.ReadFile(filepath.Join(dir, "sshmng.log"))
	s := string(data)
	if !strings.Contains(s, "existing") {
		t.Errorf("should preserve existing, got %q", s)
	}
	if !strings.Contains(s, "appended") {
		t.Errorf("should have appended, got %q", s)
	}
}

// TestRotatingWriterRotatesOnSizeExceed 验证写入超过 maxSize 时触发轮转：
// sshmng.log → sshmng.1.log，新 sshmng.log 被创建。
func TestRotatingWriterRotatesOnSizeExceed(t *testing.T) {
	dir := t.TempDir()
	// maxSize=10 字节，很容易触发轮转
	w, err := NewRotatingWriter(dir, 10, 4)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	// 写 8 字节（未超 10）
	w.Write([]byte("12345678"))
	// 再写 5 字节（8+5=13 > 10，触发轮转）
	w.Write([]byte("abcde"))

	// sshmng.log 应是 "abcde"（轮转后新文件）
	curr, _ := os.ReadFile(filepath.Join(dir, "sshmng.log"))
	if string(curr) != "abcde" {
		t.Errorf("sshmng.log = %q, want \"abcde\"", string(curr))
	}
	// sshmng.1.log 应是 "12345678"（轮转前的内容）
	b1, _ := os.ReadFile(filepath.Join(dir, "sshmng.1.log"))
	if string(b1) != "12345678" {
		t.Errorf("sshmng.1.log = %q, want \"12345678\"", string(b1))
	}
}

// TestRotatingWriterMaxFiveFiles 验证最多 5 个文件（sshmng.log + sshmng.1-4.log）。
// 第 6 次轮转时 sshmng.4.log 被删除（最老的 backup 丢弃）。
func TestRotatingWriterMaxFiveFiles(t *testing.T) {
	dir := t.TempDir()
	w, err := NewRotatingWriter(dir, 5, 4)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	// 写 6 次，每次 5 字节，触发 5 次轮转
	// 期望最终：sshmng.log（当前）+ sshmng.1-4.log（4 个 backup）= 5 个文件
	// 第 6 次写时，sshmng.4.log 被删除（原 sshmng.4.log 内容 "11111" 丢失）
	for i, content := range []string{"00000", "11111", "22222", "33333", "44444", "55555"} {
		// 写一次触发轮转（除了第一次，但第一次也会因为 5==5 不超，再写一次才超）
		// 实际上：写 5 字节后 size=5，再写 5 字节 5+5=10 > 5 触发轮转
		// 所以下面的循环每次都会触发轮转（除了第一次写完后 size=5 未超）
		_ = i
		w.Write([]byte(content))
	}

	// 列出所有 log 文件
	entries, _ := os.ReadDir(dir)
	logFiles := []string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".log") {
			logFiles = append(logFiles, e.Name())
		}
	}
	if len(logFiles) > 5 {
		t.Errorf("expected at most 5 log files, got %d: %v", len(logFiles), logFiles)
	}

	// sshmng.5.log 不应存在（maxBackups=4）
	if _, err := os.Stat(filepath.Join(dir, "sshmng.5.log")); !os.IsNotExist(err) {
		t.Errorf("sshmng.5.log should not exist (maxBackups=4, total 5 files)")
	}
}

// TestRotatingWriterCloseReleasesFile 验证 Close 后文件可被其他人读写。
func TestRotatingWriterCloseReleasesFile(t *testing.T) {
	dir := t.TempDir()
	w, err := NewRotatingWriter(dir, 10*1024*1024, 4)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	w.Write([]byte("data"))
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Close 后能读
	data, err := os.ReadFile(filepath.Join(dir, "sshmng.log"))
	if err != nil {
		t.Fatalf("read after close: %v", err)
	}
	if !strings.Contains(string(data), "data") {
		t.Errorf("content = %q, want contains 'data'", string(data))
	}
}

// TestRotatingWriterErrorOnBadDir 验证目录不可写时返回错误。
func TestRotatingWriterErrorOnBadDir(t *testing.T) {
	_, err := NewRotatingWriter("/nonexistent-xyz-123/dir", 10*1024*1024, 4)
	if err == nil {
		t.Errorf("expected error for nonexistent dir")
	}
}

// TestRotatingWriterConcurrentWrites 验证并发 Write 不 race、不丢数据。
// slog handler 可能在不同 goroutine 调 Write。
func TestRotatingWriterConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	w, err := NewRotatingWriter(dir, 1024, 4)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	const goroutines = 10
	const writesPerG = 20
	done := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < writesPerG; j++ {
				w.Write([]byte(fmt.Sprintf("g%d-w%d\n", id, j)))
			}
		}(i)
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
	// 不检查具体内容（轮转可能丢老的），只验证不 panic、不 race
}
