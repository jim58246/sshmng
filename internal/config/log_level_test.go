package config

import (
	"log/slog"
	"testing"
)

// TestParseLogLevelFullNames 验证完整级别名解析（大小写不敏感）。
func TestParseLogLevelFullNames(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"Debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"WARN", slog.LevelWarn},
		{"error", slog.LevelError},
		{"ERROR", slog.LevelError},
	}
	for _, c := range cases {
		got, err := ParseLogLevel(c.in)
		if err != nil {
			t.Errorf("ParseLogLevel(%q) err: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestParseLogLevelAbbreviations 验证缩写解析（大小写不敏感）。
// 每个级别需兼容常见缩写 + 单字母。
func TestParseLogLevelAbbreviations(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"dbg", slog.LevelDebug},
		{"d", slog.LevelDebug},
		{"DBG", slog.LevelDebug},
		{"D", slog.LevelDebug},
		{"inf", slog.LevelInfo},
		{"i", slog.LevelInfo},
		{"INF", slog.LevelInfo},
		{"I", slog.LevelInfo},
		{"warning", slog.LevelWarn},
		{"w", slog.LevelWarn},
		{"WARNING", slog.LevelWarn},
		{"W", slog.LevelWarn},
		{"err", slog.LevelError},
		{"e", slog.LevelError},
		{"ERR", slog.LevelError},
		{"E", slog.LevelError},
	}
	for _, c := range cases {
		got, err := ParseLogLevel(c.in)
		if err != nil {
			t.Errorf("ParseLogLevel(%q) err: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestParseLogLevelEmptyReturnsInfo 验证空字符串返回默认 info 级别。
// 用法：config.LogLevel 字段 omitempty 时，未配置走默认值。
func TestParseLogLevelEmptyReturnsInfo(t *testing.T) {
	got, err := ParseLogLevel("")
	if err != nil {
		t.Fatalf("ParseLogLevel(\"\") err: %v", err)
	}
	if got != slog.LevelInfo {
		t.Errorf("ParseLogLevel(\"\") = %v, want LevelInfo", got)
	}
}

// TestParseLogLevelTrimsWhitespace 验证前后空白被 trim。
// 用户在 JSON 里写 "log_level": " debug " 应能解析。
func TestParseLogLevelTrimsWhitespace(t *testing.T) {
	got, err := ParseLogLevel("  debug  ")
	if err != nil {
		t.Fatalf("ParseLogLevel(\"  debug  \") err: %v", err)
	}
	if got != slog.LevelDebug {
		t.Errorf("ParseLogLevel(\"  debug  \") = %v, want LevelDebug", got)
	}
}

// TestParseLogLevelUnknownReturnsError 验证未知级别返回 error。
// 用户配错（如 "trace"、"verbose"、"5"）必须报错，不能静默 fallback。
func TestParseLogLevelUnknownReturnsError(t *testing.T) {
	cases := []string{
		"trace", "verbose", "fatal", "5", "dbg-level", "info/debug",
	}
	for _, in := range cases {
		_, err := ParseLogLevel(in)
		if err == nil {
			t.Errorf("ParseLogLevel(%q) expected error, got nil", in)
		}
	}
}
