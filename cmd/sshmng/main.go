// Command sshmng is the SSH session manager CLI.
// Subcommands: mcp (MCP server), install (first-time setup), doctor (verify).
// Run 'sshmng help' for usage.
package main

import (
	"context"
	"os"

	"sshmng/internal/cli"
)

func main() {
	os.Exit(cli.Dispatch(context.Background(), os.Args[1:], os.Stdout))
}
