// Package cli implements sshmng's subcommand dispatch and handlers.
//
// Subcommands: mcp (MCP server), install (first-time setup), doctor (verify).
// No-arg prints help and exits 0. Unknown commands exit 2 with a hint.
package cli

import (
	"context"
	"fmt"
	"io"
)

// Dispatch parses args and routes to the appropriate subcommand handler.
// Returns the process exit code.
func Dispatch(ctx context.Context, args []string, out io.Writer) int {
	if len(args) == 0 {
		printHelp(out)
		return 0
	}
	switch args[0] {
	case "mcp":
		return runMCP(ctx, args[1:], out)
	case "install":
		return runInstall(ctx, args[1:], out)
	case "doctor":
		return runDoctor(ctx, args[1:], out)
	case "update":
		return runUpdate(ctx, args[1:], out)
	case "version":
		return runVersion(ctx, args[1:], out)
	case "help", "-h", "--help":
		printHelp(out)
		return 0
	default:
		fmt.Fprintf(out, "Unknown command %q. Run 'sshmng help' for usage.\n", args[0])
		return 2
	}
}

// printHelp writes the top-level help text to out.
func printHelp(out io.Writer) {
	fmt.Fprint(out, helpText)
}

const helpText = `sshmng - SSH session manager

Usage:
  sshmng                          Print this help and exit
  sshmng mcp [--config <path>]    Start MCP server (stdio)
  sshmng install [...]            First-time setup
  sshmng doctor [...]             Verify setup
  sshmng update                   Manually update sshmng to the latest release
  sshmng version [--check]        Print version; --check compares with latest release
  sshmng help | -h | --help       Print this help

Subcommands:
  mcp       Start the MCP server. This is what Agent configs should use
            (e.g. "command": "sshmng", "args": ["mcp"]).
  install   Create ~/.sshmng/, generate config templates, and inject sshmng
            into your AI Agent(s) (Claude Code / Hermes Agent / OpenCode).
  doctor    Verify setup is correct: files, permissions, Agent config entries.
  update    Check for a newer release and apply it. Manual; unaffected by
            auto_update_enabled. Uses GitHub Releases by default, or a
            self-hosted HTTP server if update_url is configured.
  version   Print the current version, commit, and build date. With --check,
            also query the remote source for the latest version.

Run 'sshmng <subcommand> -h' for subcommand-specific flags.
`
