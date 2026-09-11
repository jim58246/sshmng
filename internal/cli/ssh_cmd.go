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
// <name> resolves to an SSH server, or — if no server matches — a jumphost
// (direct jumphost login, including bastions). Server names take precedence.
//
// Matches OpenSSH `ssh destination [command]` convention. Commands starting
// with `-` require `--` terminator (POSIX): `sshmng ssh server -- -l`.
//
// Non-interactive command against a bastion (ssh_j=false jumphost) is rejected:
// the login_flow lands on an interactive menu with no shell command boundary.
// Use interactive mode for bastions. ssh_j=true jumphosts land on a shell and
// support non-interactive commands normally.
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

	srv, jump, err := resolveSSHTarget(cfg, name, command == "")
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}

	// Raw devices (switches etc.) have no unix shell: DetectShell/InjectRC and
	// Run() can't work. Reject before dialing — clear early error instead of a
	// confusing detect-shell timeout. Interactive mode is the supported path
	// (raw sessions are driven via MCP send_in_session/read_in_session).
	if srv != nil && command != "" && srv.Raw {
		fmt.Fprintln(out, "Error: raw device: no unix shell, non-interactive mode not supported; use interactive mode")
		return 1
	}

	knownHostsPath := KnownHostsPath(*configPath)
	knownHosts := conn.NewKnownHostsStore(knownHostsPath)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	dialer := conn.NewDialer(knownHosts, logger)

	sid, _ := conn.RandomSID()

	var ptyConn *pty.PtyConn
	if srv != nil {
		ptyConn, err = setupSSH(srv, dialer, sid, logger, command != "", false)
	} else {
		// Direct jumphost login (sshmng ssh <jumphost>). Non-interactive command
		// execution only works when the landing is a shell: a bastion's main menu
		// (ssh_j=false) has no shell command boundary, so reject it — interactive
		// mode is the supported path for bastions.
		if command != "" && !jump.SSHJ {
			fmt.Fprintf(out, "Error: non-interactive command not supported for bastion %q (ssh_j=false lands on an interactive menu, not a shell); use interactive mode: sshmng ssh %s\n", jump.Name, jump.Name)
			return 1
		}
		ptyConn, err = setupJumphostSSH(jump, dialer, sid, logger, command != "")
	}
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
		if _, err := ptyConn.InjectRC(); err != nil {
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
	// First half: dial the bastion jumphost and run its login_flow to reach the
	// main menu. Reuses dialJumphost (shared with direct bastion login). Note:
	// dialJumphost does NOT set menuLanding — Pattern B continues to the target
	// shell, whose Relay landing is a shell (stty echo wanted), not a menu.
	ptyConn, err := dialJumphost(srv.Via, dialer, sid, logger)
	if err != nil {
		return nil, err
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

// dialJumphost dials a jumphost, opens a PTY, and runs its login_flow.
// Shared by setupPatternBSSH (bastion → target) and setupJumphostSSH (direct
// jumphost login). Caller owns closing the returned PtyConn on error.
//
// Does NOT set menuLanding — that's setupJumphostSSH's call (only the direct
// bastion-login path lands on a menu; Pattern B lands on the target shell).
func dialJumphost(jump *config.Jumphost, dialer *conn.Dialer, sid string, logger *slog.Logger) (*pty.PtyConn, error) {
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
	return ptyConn, nil
}

// setupJumphostSSH establishes a direct SSH connection to a jumphost (including
// a bastion), for 'sshmng ssh <jumphost>'. Mirrors the first half of
// setupPatternBSSH via dialJumphost:
//
//   - ssh_j=true (transparent): login_flow is empty (validated), lands on the
//     jumphost's shell. Relay's "stty echo" re-enables echo normally.
//   - ssh_j=false (bastion): login_flow lands on the bastion's main menu.
//     "stty echo" still runs (sets the tty driver echo flag) and is read by the
//     menu as an unrecognized selection — harmless, echo is now on.
//
// needShell (non-interactive 'ssh <jumphost> <command>') runs DetectShell +
// InjectRC so Run() works. Only valid when the landing is a shell; the caller
// rejects non-interactive commands against bastions (ssh_j=false) beforehand.
func setupJumphostSSH(jump *config.Jumphost, dialer *conn.Dialer, sid string, logger *slog.Logger, needShell bool) (*pty.PtyConn, error) {
	ptyConn, err := dialJumphost(jump, dialer, sid, logger)
	if err != nil {
		return nil, err
	}
	if needShell {
		if err := ptyConn.DetectShell(); err != nil {
			ptyConn.Close()
			return nil, fmt.Errorf("detect shell: %w", err)
		}
		if _, err := ptyConn.InjectRC(); err != nil {
			ptyConn.Close()
			return nil, fmt.Errorf("inject rc: %w", err)
		}
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

// resolveJumphost mirrors resolveSSHServer for jumphosts: exact name first,
// then fuzzy substring match on name/addr/tags, then a numbered prompt.
func resolveJumphost(cfg *config.Config, name string, allowPrompt bool) (*config.Jumphost, error) {
	if j, err := cfg.GetJumphost(name); err == nil {
		return j, nil
	}

	matches := cfg.ListJumphosts(name)
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no jumphost matches %q", name)
	case 1:
		fmt.Fprintf(os.Stderr, "matched: %s\n", matches[0].Name)
		return matches[0], nil
	default:
		if !allowPrompt {
			names := make([]string, len(matches))
			for i, m := range matches {
				names[i] = m.Name
			}
			return nil, fmt.Errorf("multiple jumphosts match %q: %s; specify exact name", name, strings.Join(names, ", "))
		}
		return promptJumphostChoice(matches)
	}
}

// resolveSSHTarget resolves a name to an SSH server or (if no server matches)
// a jumphost. Server names take precedence over jumphost names on collision
// (exact server match is tried first, before any fuzzy match).
//
// Used by 'sshmng ssh', which supports direct jumphost login. The 'file'
// command uses resolveSSHServer directly — file transfer to a bastion is
// unsupported (Pattern B never enables sftp).
//
// Returns exactly one of srv/jump non-nil on success (the other is nil).
func resolveSSHTarget(cfg *config.Config, name string, allowPrompt bool) (*config.SSHServer, *config.Jumphost, error) {
	if srv, err := resolveSSHServer(cfg, name, allowPrompt); err == nil {
		return srv, nil, nil
	}
	jump, err := resolveJumphost(cfg, name, allowPrompt)
	if err != nil {
		return nil, nil, fmt.Errorf("no server or jumphost matches %q", name)
	}
	return nil, jump, nil
}

// promptJumphostChoice mirrors promptServerChoice for jumphosts.
func promptJumphostChoice(matches []*config.Jumphost) (*config.Jumphost, error) {
	fmt.Fprintln(os.Stderr, "Multiple jumphosts match:")
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
