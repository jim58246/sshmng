package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatingWriter 是按文件大小轮转的 io.Writer。
//
// 写入 <dir>/sshmng.log，文件超过 maxSize 时轮转：
//  1. 删除 sshmng.<maxBackups>.log（最老的 backup）
//  2. sshmng.N.log → sshmng.N+1.log（N 从 maxBackups-1 到 1）
//  3. sshmng.log → sshmng.1.log
//  4. 创建新 sshmng.log
//
// 最终文件数 = 1（current）+ maxBackups = 5（maxBackups=4 时）。
//
// 文件权限 0600：日志可能含命令输出、host key、认证交互细节，不能让其他用户读。
// 跨进程重启：打开已存在的 sshmng.log 时追加（不截断），保留历史日志。
type RotatingWriter struct {
	dir        string
	maxSize    int64
	maxBackups int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// NewRotatingWriter 创建写入 <dir>/sshmng.log 的 RotatingWriter。
// maxSize 单位字节；maxBackups 是 backup 文件数（不含 current）。
// dir 不存在时创建（0700）。sshmng.log 不存在时创建（0600），存在则追加。
func NewRotatingWriter(dir string, maxSize int64, maxBackups int) (*RotatingWriter, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("mkdir log dir: %w", err)
	}
	w := &RotatingWriter{
		dir:        dir,
		maxSize:    maxSize,
		maxBackups: maxBackups,
	}
	if err := w.openCurrent(); err != nil {
		return nil, err
	}
	return w, nil
}

// openCurrent 打开 <dir>/sshmng.log（追加模式），记录当前 size。
func (w *RotatingWriter) openCurrent() error {
	path := filepath.Join(w.dir, "sshmng.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat log file: %w", err)
	}
	w.f = f
	w.size = info.Size()
	return nil
}

// Write 写入 p。若写入后超过 maxSize，先轮转再写。
// 单次写入 len(p) > maxSize 时仍会写入（无法拆分单条日志），下次 Write 会立即轮转。
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.f == nil {
		return 0, fmt.Errorf("writer closed")
	}

	if w.size+int64(len(p)) > w.maxSize && w.size > 0 {
		if err := w.rotateLocked(); err != nil {
			return 0, fmt.Errorf("rotate: %w", err)
		}
	}

	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotateLocked 执行轮转。调用方持有 w.mu。
func (w *RotatingWriter) rotateLocked() error {
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("close current: %w", err)
	}

	// 删除最老的 backup（sshmng.<maxBackups>.log）。不存在时忽略。
	oldest := filepath.Join(w.dir, fmt.Sprintf("sshmng.%d.log", w.maxBackups))
	_ = os.Remove(oldest)

	// 从 maxBackups-1 到 1，依次 rename 到 N+1
	for i := w.maxBackups - 1; i >= 1; i-- {
		old := filepath.Join(w.dir, fmt.Sprintf("sshmng.%d.log", i))
		next := filepath.Join(w.dir, fmt.Sprintf("sshmng.%d.log", i+1))
		_ = os.Rename(old, next) // 不存在时忽略
	}

	// sshmng.log → sshmng.1.log
	current := filepath.Join(w.dir, "sshmng.log")
	first := filepath.Join(w.dir, "sshmng.1.log")
	if err := os.Rename(current, first); err != nil {
		return fmt.Errorf("rename current to .1: %w", err)
	}

	return w.openCurrent()
}

// Close 关闭当前文件。重复调用是 no-op。
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
