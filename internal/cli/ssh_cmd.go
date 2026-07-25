package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jim58246/sshmng/internal/config"
	"github.com/jim58246/sshmng/internal/ssh/conn"
	"github.com/jim58246/sshmng/internal/ssh/pty"
)

// runSSHCmd is the Dispatch entry point for 'sshmng ssh'.
func runSSHCmd(_ context.Context, args []string, out io.Writer) int {
	fs := flag.NewFlagSet("ssh", flag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "path to config.json")
	command := fs.String("command", "", "command to execute non-interactively")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(out, "Usage: sshmng ssh <name> [--command CMD]")
		return 2
	}
	name := fs.Arg(0)

	_, cfg, err := BootstrapConfig(*configPath)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}

	srv, err := cfg.GetSSHServer(name)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}

	knownHostsPath := KnownHostsPath(*configPath)
	knownHosts := conn.NewKnownHostsStore(knownHostsPath)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	dialer := conn.NewDialer(knownHosts, logger)

	sid, _ := conn.RandomSID()

	ptyConn, err := setupSSH(srv, dialer, sid, logger, *command != "")
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}

	if *command != "" {
		return runNonInteractive(ptyConn, *command, out)
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
// If needShell is true (non-interactive mode), DetectShell + InjectRC are called for Run().
// If needShell is false (interactive mode), those are skipped and Relay takes over.
func setupSSH(srv *config.SSHServer, dialer *conn.Dialer, sid string, logger *slog.Logger, needShell bool) (*pty.PtyConn, error) {
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
