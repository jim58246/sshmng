package config

import (
	"fmt"
	"log/slog"
	"strings"
)

// ParseLogLevel 把字符串解析为 slog.Level。
//
// 支持的级别名（大小写不敏感，前后空白被 trim）：
//   - debug / dbg / d
//   - info / inf / i
//   - warn / warning / w
//   - error / err / e
//
// 空字符串返回默认 LevelInfo（config.LogLevel 字段 omitempty 时走默认）。
// 未知值返回 error——用户配错（如 "trace" / "verbose" / "5"）必须报错，不能
// 静默 fallback 到 info，否则用户以为开了 debug 实际没开，排障时误导。
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return slog.LevelInfo, nil
	case "debug", "dbg", "d":
		return slog.LevelDebug, nil
	case "info", "inf", "i":
		return slog.LevelInfo, nil
	case "warn", "warning", "w":
		return slog.LevelWarn, nil
	case "error", "err", "e":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q (supported: debug/dbg/d, info/inf/i, warn/warning/w, error/err/e)", s)
	}
}
