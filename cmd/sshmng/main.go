// Command sshmng 是 SSH 会话管理工具的 MCP server 入口。
//
// 在 stdio 上对外提供 9 个 CRUD 工具（list/get/update × jumphosts/proxies/servers）。
// 通过 Claude Desktop / Claude Code 等支持 MCP 的客户端连接后即可调用。
//
// 配置文件路径解析顺序：
//  1. --config <path> 命令行参数
//  2. $SSHMNG_HOME/config.json
//  3. $HOME/.sshmng/config.json
//
// 日志策略（绝不发送 MCP notifications/message）：
//   - stdout 严禁写日志（JSON-RPC 专用）
//   - config.log_path 非空：日志写 <log_path>/sshmng.log（10MB 轮转，5 文件，
//     0600）。MCP client 完全收不到，彻底规避 Inspector 捕获 stderr 后 stall
//     导致 tool result 发不出的问题。
//   - config.log_path 空：不打日志（io.Discard）。彻底静默，Inspector 无 stderr
//     可捕获。
//   - 日志绝不写 stderr：Inspector 会把 stderr 包装成 notifications/message 显示，
//     stall 时阻塞 stdout writer 导致 tool result 卡住。
//
// 日志级别（config.log_level）：
//   - 支持 debug/dbg/d、info/inf/i、warn/warning/w、error/err/e（大小写不敏感）
//   - 默认 info（字段省略时）
//   - 配错 Load 报错（如 "trace" / "verbose"）
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"sshmng/internal/config"
	"sshmng/internal/mcp"
	"sshmng/internal/ssh/conn"
)

func main() {
	configPath := flag.String("config", "", "path to config.json (default: $SSHMNG_HOME/config.json or $HOME/.sshmng/config.json)")
	flag.Parse()

	// bootstrap logger：仅用于 config 加载失败等进程启动早期 fatal 错误。
	// 直接写 stderr（同步），因为这些错误立即 os.Exit，不存在 stall 风险。
	// 正常运行时不写任何日志到 stderr。
	bootstrapLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	path, err := resolveConfigPath(*configPath)
	if err != nil {
		bootstrapLogger.Error("resolve config path", "err", err)
		os.Exit(1)
	}

	store := config.NewStore(path)
	// Windows 上 Load 跳过配置文件权限检查（NTFS ACL 模型不同于 Unix rwx）。
	// 私钥文件 / known_hosts 的权限检查同样在 Windows 上跳过（见 conn 包）。
	// 提示用户用 NTFS ACL 保护所有敏感文件。
	if runtime.GOOS == "windows" {
		bootstrapLogger.Info("Unix permission check skipped on Windows; ensure NTFS ACL restricts access to sensitive files (config.json, private keys, known_hosts)", "path", path)
	}
	// 启动时尝试加载一次，提早暴露权限/格式/log_level 配错问题；同时取出 LogPath /
	// LogLevel 决定日志去向。Load 内部已校验 log_level 合法性。
	cfg, err := store.Load()
	if err != nil {
		bootstrapLogger.Error("load config",
			"path", path, "err", err,
			"note", "if the file does not exist it will be created on first update; permission errors must be fixed manually (chmod 0600); log_level must be one of debug/info/warn/error (or abbreviations)")
		os.Exit(1)
	}

	level, _ := config.ParseLogLevel(cfg.LogLevel) // Load 已校验，这里不会 err

	writer, writerCleanup, err := openLogWriter(cfg.LogPath)
	if err != nil {
		bootstrapLogger.Error("open log writer", "log_path", cfg.LogPath, "err", err)
		os.Exit(1)
	}
	defer writerCleanup()

	logger := slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: level}))

	knownHosts := conn.NewKnownHostsStore(filepath.Join(filepath.Dir(path), "known_hosts"))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	svc := mcp.NewService(store, knownHosts, logger)
	logger.Info("sshmng MCP server starting", "config", path, "log_level", level.String(), "log_path", cfg.LogPath)
	if err := svc.Run(ctx); err != nil {
		logger.Error("server", "err", err)
		os.Exit(1)
	}
}

// openLogWriter 打开日志 writer。
//   - logPath 为空：返回 io.Discard，不打任何日志。用于用户未配置 log_path 的场景：
//     无 stderr 输出，Inspector 无从捕获，彻底规避 stall。
//   - logPath 非空：返回 RotatingWriter，写入 <logPath>/sshmng.log（10MB 轮转，
//     保留 4 个 backup 共 5 文件，0600 权限）。
//
// 返回的 cleanup 必须在进程退出前调用：RotatingWriter 模式关闭文件，Discard 模式
// no-op。
func openLogWriter(logPath string) (io.Writer, func() error, error) {
	if logPath == "" {
		return io.Discard, func() error { return nil }, nil
	}
	rw, err := mcp.NewRotatingWriter(logPath, 10*1024*1024, 4)
	if err != nil {
		return nil, nil, err
	}
	return rw, func() error { return rw.Close() }, nil
}

// resolveConfigPath 把 --config / $SSHMNG_HOME / $HOME 三种来源解析成最终的 config.json 路径。
func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if home := os.Getenv("SSHMNG_HOME"); home != "" {
		return filepath.Join(home, "config.json"), nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(userHome, ".sshmng", "config.json"), nil
}
