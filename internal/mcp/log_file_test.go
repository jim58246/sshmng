package mcp

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLogFileNameFormat(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		pid  int
		want string
	}{
		{
			name: "typical",
			now:  time.Date(2026, 2, 2, 17, 20, 22, 0, time.Local),
			pid:  12345,
			want: "2026-02-02_17-20-22-12345.log",
		},
		{
			name: "single digit pid",
			now:  time.Date(2026, 7, 30, 8, 5, 1, 0, time.Local),
			pid:  7,
			want: "2026-07-30_08-05-01-7.log",
		},
		{
			name: "zero padded fields",
			now:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local),
			pid:  1,
			want: "2026-01-01_00-00-00-1.log",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LogFileName(tc.now, tc.pid)
			if got != tc.want {
				t.Errorf("LogFileName(%v, %d) = %q, want %q", tc.now, tc.pid, got, tc.want)
			}
		})
	}
}

func TestParseLogFileNameTime_RoundTrip(t *testing.T) {
	now := time.Date(2026, 2, 2, 17, 20, 22, 0, time.Local)
	name := LogFileName(now, 12345)
	got, ok := parseLogFileNameTime(name)
	if !ok {
		t.Fatalf("parseLogFileNameTime(%q) ok=false, want true", name)
	}
	if !got.Equal(now) {
		t.Errorf("parseLogFileNameTime = %v, want %v", got, now)
	}
}

func TestParseLogFileNameTime_LegacyFiles(t *testing.T) {
	cases := []string{
		"sshmng.log",
		"sshmng.1.log",
		"sshmng.2.log",
		"random.log",
		"2026-02-02.log",            // 缺时间部分
		"2026-02-02_17-20-22.log",   // 缺 pid（无 -<pid> 后缀）
		"2026-02-02_17-20-22X1.log", // 缺 dash 分隔符
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			_, ok := parseLogFileNameTime(name)
			if ok {
				t.Errorf("parseLogFileNameTime(%q) ok=true, want false", name)
			}
		})
	}
}

func TestCleanupOldLogs_RemovesExpiredTimestampFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	// 8 天前的文件 — 应删除
	oldName := LogFileName(now.Add(-8*24*time.Hour), 11111)
	if err := os.WriteFile(filepath.Join(dir, oldName), []byte("old"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 1 小时前的文件 — 应保留
	recentName := LogFileName(now.Add(-1*time.Hour), 22222)
	if err := os.WriteFile(filepath.Join(dir, recentName), []byte("recent"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := cleanupOldLogs(dir, 7*24*time.Hour); err != nil {
		t.Fatalf("cleanupOldLogs: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, oldName)); !os.IsNotExist(err) {
		t.Errorf("old timestamp file should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, recentName)); err != nil {
		t.Errorf("recent timestamp file should be kept, stat err=%v", err)
	}
}

func TestCleanupOldLogs_LegacyFilesByMtime(t *testing.T) {
	dir := t.TempDir()

	// 旧的 legacy 文件（按 mtime 判断）
	oldLegacy := filepath.Join(dir, "sshmng.log")
	if err := os.WriteFile(oldLegacy, []byte("old"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(oldLegacy, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// 新的 legacy 文件（mtime 近期）
	newLegacy := filepath.Join(dir, "sshmng.1.log")
	if err := os.WriteFile(newLegacy, []byte("recent"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := cleanupOldLogs(dir, 7*24*time.Hour); err != nil {
		t.Fatalf("cleanupOldLogs: %v", err)
	}

	if _, err := os.Stat(oldLegacy); !os.IsNotExist(err) {
		t.Errorf("old legacy file should be removed by mtime, stat err=%v", err)
	}
	if _, err := os.Stat(newLegacy); err != nil {
		t.Errorf("recent legacy file should be kept, stat err=%v", err)
	}
}

func TestCleanupOldLogs_NonLogFilesNotRemoved(t *testing.T) {
	dir := t.TempDir()

	// 旧的非 log 文件 — 不应删除
	oldPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(oldPath, []byte("{}"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	if err := cleanupOldLogs(dir, 7*24*time.Hour); err != nil {
		t.Fatalf("cleanupOldLogs: %v", err)
	}

	if _, err := os.Stat(oldPath); err != nil {
		t.Errorf("non-log file should not be removed, stat err=%v", err)
	}
}

func TestOpenLogFile_EmptyPathReturnsDiscard(t *testing.T) {
	w, cleanup, err := OpenLogFile("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer cleanup()

	if w != io.Discard {
		t.Errorf("expected io.Discard for empty path, got %T", w)
	}
}

func TestOpenLogFile_CreatesTimestampedFile(t *testing.T) {
	dir := t.TempDir()
	w, cleanup, err := OpenLogFile(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	// 找到 log 文件 — 应该只有一个，且名字符合格式
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var logFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			logFiles = append(logFiles, e.Name())
		}
	}
	if len(logFiles) != 1 {
		t.Fatalf("expected 1 log file, got %d: %v", len(logFiles), logFiles)
	}
	name := logFiles[0]
	if _, ok := parseLogFileNameTime(name); !ok {
		t.Errorf("log file name %q doesn't match timestamp format", name)
	}
	// 验证 pid 后缀
	wantSuffix := "-" + strconv.Itoa(os.Getpid()) + ".log"
	if !strings.HasSuffix(name, wantSuffix) {
		t.Errorf("log file name %q should end with %q", name, wantSuffix)
	}
	// 验证内容
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "hello") {
		t.Errorf("content = %q, want contains 'hello'", string(data))
	}
}

func TestOpenLogFile_FilePerm0600(t *testing.T) {
	dir := t.TempDir()
	w, cleanup, err := OpenLogFile(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer cleanup()
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			info, err := e.Info()
			if err != nil {
				t.Fatalf("info: %v", err)
			}
			if perm := info.Mode().Perm(); perm != 0600 {
				t.Errorf("perm = %o, want 0600", perm)
			}
			return
		}
	}
	t.Fatalf("no .log file found")
}

func TestOpenLogFile_BadPathReturnsError(t *testing.T) {
	// 父路径是文件，MkdirAll 会失败
	tmpFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(tmpFile, []byte("x"), 0600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, _, err := OpenLogFile(filepath.Join(tmpFile, "sub", "dir"))
	if err == nil {
		t.Errorf("expected error for bad path")
	}
}

func TestOpenLogFile_RunsCleanupOnStartup(t *testing.T) {
	dir := t.TempDir()
	// 预先放一个 8 天前的旧文件
	oldName := LogFileName(time.Now().Add(-8*24*time.Hour), 99999)
	if err := os.WriteFile(filepath.Join(dir, oldName), []byte("old"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, cleanup, err := OpenLogFile(dir)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer cleanup()

	// 旧文件应被清理
	if _, err := os.Stat(filepath.Join(dir, oldName)); !os.IsNotExist(err) {
		t.Errorf("old file should be removed on OpenLogFile, stat err=%v", err)
	}
}
