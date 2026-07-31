package mcp

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// logRetention 是日志文件保留时长。超过此时长的 *.log 文件在 OpenLogFile 时被清理。
const logRetention = 7 * 24 * time.Hour

// logFileNameFormat 是日志文件名中时间戳的格式：2006-01-02_15-04-05。
// 用破折号分隔时分秒（不用冒号），保证 Windows 文件名合法。
const logFileNameFormat = "2006-01-02_15-04-05"

// LogFileName 构造日志文件名：<timestamp>-<pid>.log。
// 例：LogFileName(2026-02-02T17:20:22 local, 12345) = "2026-02-02_17-20-22-12345.log"。
func LogFileName(now time.Time, pid int) string {
	return now.Format(logFileNameFormat) + "-" + strconv.Itoa(pid) + ".log"
}

// OpenLogFile 在 dir 下创建本进程的日志文件并返回 writer。
//   - dir 为空：返回 io.Discard（不打日志）
//   - dir 非空：mkdir -p dir，best-effort 清理 7 天前的旧日志，打开 <dir>/<timestamp>-<pid>.log
//
// 文件权限 0600（日志可能含命令输出、host key、认证交互细节）。
// 文件已存在（同秒内 pid 复用）时追加而非截断。
// 返回的 cleanup 函数关闭文件；重复调用返回 os.ErrClosed（*os.File.Close 的标准行为）。
func OpenLogFile(dir string) (io.Writer, func() error, error) {
	if dir == "" {
		return io.Discard, func() error { return nil }, nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, fmt.Errorf("mkdir log dir: %w", err)
	}
	// 清理 best-effort：失败不阻止打开新日志（用户能继续排查本次问题）。
	_ = cleanupOldLogs(dir, logRetention)
	name := LogFileName(time.Now(), os.Getpid())
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file: %w", err)
	}
	return f, func() error { return f.Close() }, nil
}

// cleanupOldLogs 删除 dir 中超过 ttl 的 *.log 文件。
//   - 文件名符合 <timestamp>-<pid>.log 格式：按时间戳判断
//   - 文件名不符合（如 legacy sshmng.log / sshmng.1.log）：按 mtime 判断
//
// 非 *.log 文件不删。目录不删。ReadDir 失败返回错误，单文件删除失败忽略。
func cleanupOldLogs(dir string, ttl time.Duration) error {
	now := time.Now()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		var fileTime time.Time
		if t, ok := parseLogFileNameTime(e.Name()); ok {
			fileTime = t
		} else {
			info, err := e.Info()
			if err != nil {
				continue
			}
			fileTime = info.ModTime()
		}
		if now.Sub(fileTime) > ttl {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return nil
}

// parseLogFileNameTime 从文件名 <timestamp>-<pid>.log 中解析时间戳。
// 文件名格式必须是 2006-01-02_15-04-05-<pid>.log（时间戳固定 19 字符，后接 "-" 和 pid）。
// 用 time.Local 解析（LogFileName 用 time.Now().Format 生成，也是 local）。
// 格式不匹配返回 ok=false。
func parseLogFileNameTime(name string) (time.Time, bool) {
	if !strings.HasSuffix(name, ".log") {
		return time.Time{}, false
	}
	base := strings.TrimSuffix(name, ".log")
	// 时间戳 19 字符 + "-" + 至少 1 位 pid = 至少 21 字符
	if len(base) < 21 || base[19] != '-' {
		return time.Time{}, false
	}
	ts := base[:19]
	t, err := time.ParseInLocation(logFileNameFormat, ts, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
