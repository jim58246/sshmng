// Command sshmng is the SSH session manager CLI.
// Subcommands: mcp, install, doctor, update, version, server, jumphost, proxy, ssh.
// Run 'sshmng help' for usage.
package main

import (
	"context"
	"os"

	"github.com/jim58246/sshmng/internal/cli"
)

func main() {
	os.Exit(cli.Dispatch(context.Background(), os.Args[1:], os.Stdout))
}
