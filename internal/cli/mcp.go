package cli

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

// runMCP starts the stdio MCP server. Mirrors the pre-refactor main.go behavior.
func runMCP(ctx context.Context, args []string, out io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "path to config.json (default: $SSHMNG_HOME/config.json or $HOME/.sshmng/config.json)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	bootstrapLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	path, err := resolveConfigPath(*configPath)
	if err != nil {
		bootstrapLogger.Error("resolve config path", "err", err)
		return 1
	}

	store := config.NewStore(path)
	if runtime.GOOS == "windows" {
		bootstrapLogger.Info("Unix permission check skipped on Windows; ensure NTFS ACL restricts access to sensitive files (config.json, private keys, known_hosts)", "path", path)
	}
	cfg, err := store.Load()
	if err != nil {
		bootstrapLogger.Error("load config",
			"path", path, "err", err,
			"note", "if the file does not exist it will be created on first update; permission errors must be fixed manually (chmod 0600); log_level must be one of debug/info/warn/error (or abbreviations)")
		return 1
	}

	level, _ := config.ParseLogLevel(cfg.LogLevel)

	writer, writerCleanup, err := openLogWriter(cfg.LogPath)
	if err != nil {
		bootstrapLogger.Error("open log writer", "log_path", cfg.LogPath, "err", err)
		return 1
	}
	defer writerCleanup()

	logger := slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: level}))

	knownHosts := conn.NewKnownHostsStore(filepath.Join(filepath.Dir(path), "known_hosts"))

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	svc := mcp.NewService(store, knownHosts, logger)
	logger.Info("sshmng MCP server starting", "config", path, "log_level", level.String(), "log_path", cfg.LogPath)
	if err := svc.Run(ctx); err != nil {
		logger.Error("server", "err", err)
		return 1
	}
	return 0
}

// resolveConfigPath resolves --config / $SSHMNG_HOME / $HOME/.sshmng/config.json.
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

// openLogWriter opens the log writer based on logPath.
//   - empty: io.Discard (no logs)
//   - non-empty: RotatingWriter writing to <logPath>/sshmng.log
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
