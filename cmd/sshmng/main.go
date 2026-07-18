// Command sshmng 是 SSH 会话管理工具的 MCP server 入口。
//
// 在 stdio 上对外提供 9 个 CRUD 工具（list/get/update × jumphosts/proxies/servers）。
// 通过 Claude Desktop / Claude Code 等支持 MCP 的客户端连接后即可调用。
//
// 配置文件路径解析顺序：
//   1. --config <path> 命令行参数
//   2. $SSHMNG_HOME/config.json
//   3. $HOME/.sshmng/config.json
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"sshmng/internal/config"
	"sshmng/internal/mcp"
)

func main() {
	configPath := flag.String("config", "", "path to config.json (default: $SSHMNG_HOME/config.json or $HOME/.sshmng/config.json)")
	flag.Parse()

	path, err := resolveConfigPath(*configPath)
	if err != nil {
		log.Fatalf("resolve config path: %v", err)
	}

	store := config.NewStore(path)
	// 启动时尝试加载一次，提早暴露权限/格式问题。
	if _, err := store.Load(); err != nil {
		log.Fatalf("load config from %s: %v\nnote: if the file does not exist it will be created on first update; permission errors must be fixed manually (chmod 0600)", path, err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	svc := mcp.NewService(store)
	log.Printf("sshmng MCP server starting (config: %s)", path)
	if err := svc.Run(ctx); err != nil {
		log.Fatalf("server: %v", err)
	}
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
