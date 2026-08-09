package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/pflag"

	"github.com/jim58246/sshmng/internal/config"
	"github.com/jim58246/sshmng/internal/ssh/conn"
	"github.com/jim58246/sshmng/internal/ssh/pty"
)

// runSSHCmd is the Dispatch entry point for 'sshmng ssh'.
//
// Positional args: <name> [command].
//   - 1 arg: interactive login (Relay)
//   - 2 args: non-interactive — execute command, print output, exit
//
// Matches OpenSSH `ssh destination [command]` convention. Commands starting
// with `-` require `--` terminator (POSIX): `sshmng ssh server -- -l`.
func runSSHCmd(_ context.Context, args []string, out io.Writer) int {
	fs := pflag.NewFlagSet("ssh", pflag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "path to config.json")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if fs.NArg() == 0 || fs.NArg() > 2 {
		fmt.Fprintln(out, "Usage: sshmng ssh <name> [command]")
		if fs.NArg() > 2 {
			fmt.Fprintf(out, "Error: expected 1 or 2 positional args, got %d (%v)\n", fs.NArg(), fs.Args())
		}
		return 2
	}
	name := fs.Arg(0)
	var command string
	if fs.NArg() == 2 {
		command = fs.Arg(1)
	}

	_, cfg, err := BootstrapConfig(*configPath)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}

	srv, err := resolveSSHServer(cfg, name, command == "")
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}

	knownHostsPath := KnownHostsPath(*configPath)
	knownHosts := conn.NewKnownHostsStore(knownHostsPath)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	dialer := conn.NewDialer(knownHosts, logger)

	sid, _ := conn.RandomSID()

	ptyConn, err := setupSSH(srv, dialer, sid, logger, command != "", false)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}

	if command != "" {
		return runNonInteractive(ptyConn, command, out)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := ptyConn.Relay(ctx); err != nil {
		fmt.Fprintf(out, "\nError: %v\n", err)
		return 1
	}
	return 0
}

// setupSSH establishes an SSH connection following the same three-way pattern as MCP Login.
//   - needShell=true (non-interactive 'ssh <name> <command>'): DetectShell + InjectRC for Run().
//   - needShell=false (interactive Relay, or file transfer): those are skipped.
//   - needSftp=true (file transfer): TryEnableSftp on direct + Pattern A.
//     Pattern B never enables sftp — the SSH client is to the jumphost, so the sftp channel
//     would land on the jumphost, not the target (silently wrong). Mirrors MCP setupPatternB
//     (tools_session.go:225-227). Caller must check SftpAvailable() and surface a clear error.
//
// needShell and needSftp are independent: file transfer needs sftp but not a shell
// (sftp is a separate SSH channel, independent of the PTY/RC).
func setupSSH(srv *config.SSHServer, dialer *conn.Dialer, sid string, logger *slog.Logger, needShell, needSftp bool) (*pty.PtyConn, error) {
	var ptyConn *pty.PtyConn
	var err error

	switch {
	case srv.Via == nil:
		ptyConn, err = setupDirectSSH(srv, dialer, sid, logger)
	case srv.Via.SSHJ:
		ptyConn, err = setupPatternASSH(srv, dialer, sid, logger)
	default:
		ptyConn, err = setupPatternBSSH(srv, dialer, sid, logger)
	}
	if err != nil {
		return nil, err
	}

	// Non-interactive mode needs DetectShell + InjectRC for Run() to work.
	if needShell {
		if err := ptyConn.DetectShell(); err != nil {
			ptyConn.Close()
			return nil, fmt.Errorf("detect shell: %w", err)
		}
		if err := ptyConn.InjectRC(); err != nil {
			ptyConn.Close()
			return nil, fmt.Errorf("inject rc: %w", err)
		}
	}

	// File transfer needs the sftp channel. Only direct + Pattern A: the sftp channel
	// is to the target. Pattern B's sftp channel is to the jumphost (misleading), so
	// never enable it — caller sees SftpAvailable()==false and errors out.
	if needSftp && (srv.Via == nil || srv.Via.SSHJ) {
		ptyConn.TryEnableSftp()
	}

	return ptyConn, nil
}

func setupDirectSSH(srv *config.SSHServer, dialer *conn.Dialer, sid string, logger *slog.Logger) (*pty.PtyConn, error) {
	client, err := dialer.Dial(conn.DialOptions{
		Addr:          srv.Addr,
		User:          srv.User,
		Auth:          srv.Auth,
		Proxy:         srv.Proxy,
		ServerName:    srv.Name,
		HostKeyVerify: srv.HostKeyVerifyEnabled(),
	})
	if err != nil {
		return nil, fmt.Errorf("ssh connect to %s: %w", srv.Addr, err)
	}
	ptyConn, err := pty.OpenPtyConnWithTimeout(client, sid, logger, 0)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("setup pty: %w", err)
	}
	if len(srv.LoginFlow) > 0 {
		if _, err := ptyConn.RunLoginFlow(srv.LoginFlow, srv.LoginEntry, pty.LoginFlowOptions{
			MaxSteps:        srv.MaxSteps,
			GlobalTimeoutMs: srv.GlobalTimeoutMs,
		}); err != nil {
			ptyConn.Close()
			return nil, fmt.Errorf("login flow: %w", err)
		}
	}
	return ptyConn, nil
}

func setupPatternASSH(srv *config.SSHServer, dialer *conn.Dialer, sid string, logger *slog.Logger) (*pty.PtyConn, error) {
	jump := srv.Via
	jumpClient, err := dialer.Dial(conn.DialOptions{
		Addr:          jump.Addr,
		User:          jump.User,
		Auth:          jump.Auth,
		Proxy:         jump.Proxy,
		ServerName:    jump.Name,
		HostKeyVerify: jump.HostKeyVerifyEnabled(),
	})
	if err != nil {
		return nil, fmt.Errorf("ssh connect to jumphost %s: %w", jump.Addr, err)
	}
	targetClient, err := dialer.DialThrough(jumpClient, conn.DialOptions{
		Addr:          srv.Addr,
		User:          srv.User,
		Auth:          srv.Auth,
		ServerName:    srv.Name,
		HostKeyVerify: srv.HostKeyVerifyEnabled(),
	})
	if err != nil {
		jumpClient.Close()
		return nil, fmt.Errorf("ssh connect to target %s through jumphost: %w", srv.Addr, err)
	}
	ptyConn, err := pty.OpenPtyConnWithTimeout(targetClient, sid, logger, 0)
	if err != nil {
		targetClient.Close()
		jumpClient.Close()
		return nil, fmt.Errorf("setup pty: %w", err)
	}
	ptyConn.SetJumpClient(jumpClient)
	if len(srv.LoginFlow) > 0 {
		if _, err := ptyConn.RunLoginFlow(srv.LoginFlow, srv.LoginEntry, pty.LoginFlowOptions{
			MaxSteps:        srv.MaxSteps,
			GlobalTimeoutMs: srv.GlobalTimeoutMs,
		}); err != nil {
			ptyConn.Close()
			return nil, fmt.Errorf("login flow: %w", err)
		}
	}
	return ptyConn, nil
}

func setupPatternBSSH(srv *config.SSHServer, dialer *conn.Dialer, sid string, logger *slog.Logger) (*pty.PtyConn, error) {
	jump := srv.Via
	client, err := dialer.Dial(conn.DialOptions{
		Addr:          jump.Addr,
		User:          jump.User,
		Auth:          jump.Auth,
		Proxy:         jump.Proxy,
		ServerName:    jump.Name,
		HostKeyVerify: jump.HostKeyVerifyEnabled(),
	})
	if err != nil {
		return nil, fmt.Errorf("ssh connect to jumphost %s: %w", jump.Addr, err)
	}
	ptyConn, err := pty.OpenPtyConnWithTimeout(client, sid, logger, 0)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("setup pty: %w", err)
	}
	if _, err := ptyConn.RunLoginFlow(jump.LoginFlow, jump.LoginEntry, pty.LoginFlowOptions{
		MaxSteps:        jump.MaxSteps,
		GlobalTimeoutMs: jump.GlobalTimeoutMs,
	}); err != nil {
		ptyConn.Close()
		return nil, fmt.Errorf("jumphost login flow: %w", err)
	}
	if _, err := ptyConn.RunLoginFlow(srv.LoginFlow, srv.LoginEntry, pty.LoginFlowOptions{
		MaxSteps:        srv.MaxSteps,
		GlobalTimeoutMs: srv.GlobalTimeoutMs,
	}); err != nil {
		ptyConn.Close()
		return nil, fmt.Errorf("target login flow: %w", err)
	}
	return ptyConn, nil
}

// runNonInteractive executes a single command and writes output to out.
func runNonInteractive(ptyConn *pty.PtyConn, cmd string, out io.Writer) int {
	defer ptyConn.Close()
	output, _, exitCode, _, _, _, _, _, err := ptyConn.Run(cmd, 0, 0)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}
	fmt.Fprint(out, output)
	if exitCode > 0 {
		return exitCode
	}
	return 0
}

// resolveSSHServer finds an SSH server by exact name, falling back to fuzzy
// substring match on name/addr/tags.
//   - Exact match: returns immediately (no surprise for existing users).
//   - 1 fuzzy match: returns it; prints "matched: <name>" to stderr so the
//     user sees what was resolved (avoid silently landing on the wrong host).
//   - >1 fuzzy matches: if allowPrompt, prints numbered list to stderr and
//     reads selection from stdin; otherwise returns error listing candidates.
//   - 0 matches: returns error.
//
// allowPrompt is false in non-interactive mode (command provided) to avoid
// blocking on stdin in scripts.
func resolveSSHServer(cfg *config.Config, name string, allowPrompt bool) (*config.SSHServer, error) {
	if srv, err := cfg.GetSSHServer(name); err == nil {
		return srv, nil
	}

	matches := cfg.ListSSHServers(name)
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no server matches %q", name)
	case 1:
		fmt.Fprintf(os.Stderr, "matched: %s\n", matches[0].Name)
		return matches[0], nil
	default:
		if !allowPrompt {
			names := make([]string, len(matches))
			for i, m := range matches {
				names[i] = m.Name
			}
			return nil, fmt.Errorf("multiple servers match %q: %s; specify exact name", name, strings.Join(names, ", "))
		}
		return promptServerChoice(matches)
	}
}

// promptServerChoice prints a numbered list of servers to stderr and reads
// a 1-based selection from stdin. Returns error on EOF, non-numeric input,
// or out-of-range selection.
func promptServerChoice(matches []*config.SSHServer) (*config.SSHServer, error) {
	fmt.Fprintln(os.Stderr, "Multiple servers match:")
	for i, m := range matches {
		tags := strings.Join(m.Tags, ",")
		fmt.Fprintf(os.Stderr, "  [%d] %-20s %-25s %s\n", i+1, m.Name, m.Addr, tags)
	}
	fmt.Fprintf(os.Stderr, "Select [1-%d]: ", len(matches))

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("read selection: %w", err)
		}
		return nil, fmt.Errorf("no selection (EOF)")
	}
	input := strings.TrimSpace(scanner.Text())

	n, err := strconv.Atoi(input)
	if err != nil || n < 1 || n > len(matches) {
		return nil, fmt.Errorf("invalid selection %q (expected 1-%d)", input, len(matches))
	}
	return matches[n-1], nil
}
