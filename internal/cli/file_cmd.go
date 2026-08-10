package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/pflag"

	"github.com/jim58246/sshmng/internal/config"
	"github.com/jim58246/sshmng/internal/progress"
	"github.com/jim58246/sshmng/internal/ssh/conn"
	"github.com/jim58246/sshmng/internal/ssh/pty"
	"github.com/jim58246/sshmng/internal/ssh/session"
)

// runFileCmd is the Dispatch entry point for 'sshmng file'.
//
// Subcommands mirror the MCP transfer tools (upload/download/upload_dir/
// download_dir/relay_transfer) as a human-facing CLI. Single-file and dir
// transfers drive the ptyConn directly (those methods live on PtyConn, which
// implements session.Conn); relay uses a temporary session.Manager because
// RelayTransfer's 1:N fanout orchestration lives on the Manager.
//
// All subcommands accept --config <path>. Pattern B (bastion, ssh_j=false) does
// not support file transfer — the sftp channel lands on the jumphost, not the
// target, so setupSSH never enables sftp for Pattern B; the handler surfaces a
// clear error instead of silently uploading to the wrong host.
func runFileCmd(_ context.Context, args []string, out io.Writer) int {
	if len(args) == 0 {
		printFileUsage(out)
		return 2
	}
	switch args[0] {
	case "upload":
		return runFileUpload(args[1:], out)
	case "download":
		return runFileDownload(args[1:], out)
	case "upload-dir":
		return runFileUploadDir(args[1:], out)
	case "download-dir":
		return runFileDownloadDir(args[1:], out)
	case "relay":
		return runFileRelay(args[1:], out)
	default:
		fmt.Fprintf(out, "Unknown file subcommand %q. Use 'upload', 'download', 'upload-dir', 'download-dir', or 'relay'.\n", args[0])
		printFileUsage(out)
		return 2
	}
}

func printFileUsage(out io.Writer) {
	fmt.Fprint(out, `Usage:
  sshmng file upload       <name> <local>  <remote>  [--timeout <ms>]
  sshmng file download     <name> <remote> <local>   [--timeout <ms>]
  sshmng file upload-dir   <name> <local>  <remote>  [--conflict overwrite|skip|rename] [--concurrency N] [--timeout <ms>]
  sshmng file download-dir <name> <remote> <local>   [--conflict ...] [--concurrency N] [--timeout <ms>]
  sshmng file relay        <src-name> <src-path> <dst-path> --to <dst1,dst2,...> [--timeout <ms>]

All subcommands accept --config <path>.
<name> is an SSH server name (supports name/addr/tag substring match, same as 'sshmng ssh').
Pattern B (bastion, ssh_j=false) does not support file transfer: sftp lands on the
jumphost, not the target. Use a direct or ssh_j=true connection.
`)
}

// setupFileSession resolves <name>, dials, and returns a ptyConn with the sftp
// channel enabled (direct + Pattern A). Caller must Close the returned ptyConn
// and check SftpAvailable() before transferring (false => Pattern B or negotiate
// failure; surface sftpUnavailableReason).
func setupFileSession(cfg *config.Config, name, configPath string, allowPrompt bool) (*pty.PtyConn, *config.SSHServer, error) {
	srv, err := resolveSSHServer(cfg, name, allowPrompt)
	if err != nil {
		return nil, nil, err
	}
	knownHosts := conn.NewKnownHostsStore(KnownHostsPath(configPath))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	dialer := conn.NewDialer(knownHosts, logger)
	sid, _ := conn.RandomSID()
	ptyConn, err := setupSSH(srv, dialer, sid, logger, false, true)
	if err != nil {
		return nil, srv, err
	}
	return ptyConn, srv, nil
}

// sftpUnavailableReason explains why SftpAvailable()==false for srv.
// Pattern B is by design (sftp to jumphost); otherwise the server's sftp
// subsystem could not be negotiated.
func sftpUnavailableReason(srv *config.SSHServer) string {
	if srv.Via != nil && !srv.Via.SSHJ {
		return fmt.Sprintf("server %s uses bastion mode (ssh_j=false); sftp lands on the jumphost, not the target — use a direct or ssh_j=true connection", srv.Name)
	}
	return fmt.Sprintf("sftp channel unavailable for %s (server may not support the sftp subsystem)", srv.Name)
}

// --- single-file ---

// runFileUpload uploads a local file to <name>:<remote>.
func runFileUpload(args []string, out io.Writer) int {
	fs := pflag.NewFlagSet("file upload", pflag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "path to config.json")
	timeoutMs := fs.Int("timeout", 0, "transfer timeout in ms; 0 = default 300s")
	fs.Usage = func() {
		fmt.Fprintf(out, "Usage: sshmng file upload <name> <local> <remote> [--timeout <ms>] [--config <path>]\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		if fs.NArg() != 0 {
			fmt.Fprintf(out, "Error: expected 3 positional args (name local remote), got %d\n", fs.NArg())
		}
		return 2
	}
	name, local, remote := fs.Arg(0), fs.Arg(1), fs.Arg(2)

	_, cfg, err := BootstrapConfig(*configPath)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}
	ptyConn, srv, err := setupFileSession(cfg, name, *configPath, true)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}
	defer ptyConn.Close()
	if !ptyConn.SftpAvailable() {
		fmt.Fprintf(out, "Error: %s\n", sftpUnavailableReason(srv))
		return 1
	}

	f, err := os.Open(local)
	if err != nil {
		fmt.Fprintf(out, "Error: open %s: %v\n", local, err)
		return 1
	}
	defer f.Close()

	fi, _ := os.Stat(local)
	size := int64(-1)
	if fi != nil {
		size = fi.Size()
	}
	bar := progress.NewBar(os.Stderr, srv.Name+":"+remote, size)
	cr := &progress.CountingReader{R: f, Fn: func(n int64) { bar.SetBytes(n) }}
	n, timedOut, err := ptyConn.UploadSized(cr, size, remote, *timeoutMs)
	bar.Finish()
	if err != nil && !timedOut {
		fmt.Fprintf(out, "Error: upload %s -> %s:%s: %v\n", local, srv.Name, remote, err)
		return 1
	}
	if timedOut {
		fmt.Fprintf(out, "uploaded %s -> %s:%s (timed out, %d bytes transferred)\n", local, srv.Name, remote, n)
		return 1
	}
	fmt.Fprintf(out, "uploaded %s -> %s:%s (%d bytes)\n", local, srv.Name, remote, n)
	return 0
}

// runFileDownload downloads <name>:<remote> to a local file.
func runFileDownload(args []string, out io.Writer) int {
	fs := pflag.NewFlagSet("file download", pflag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "path to config.json")
	timeoutMs := fs.Int("timeout", 0, "transfer timeout in ms; 0 = default 300s")
	fs.Usage = func() {
		fmt.Fprintf(out, "Usage: sshmng file download <name> <remote> <local> [--timeout <ms>] [--config <path>]\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		if fs.NArg() != 0 {
			fmt.Fprintf(out, "Error: expected 3 positional args (name remote local), got %d\n", fs.NArg())
		}
		return 2
	}
	name, remote, local := fs.Arg(0), fs.Arg(1), fs.Arg(2)

	_, cfg, err := BootstrapConfig(*configPath)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}
	ptyConn, srv, err := setupFileSession(cfg, name, *configPath, true)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}
	defer ptyConn.Close()
	if !ptyConn.SftpAvailable() {
		fmt.Fprintf(out, "Error: %s\n", sftpUnavailableReason(srv))
		return 1
	}

	f, err := os.Create(local)
	if err != nil {
		fmt.Fprintf(out, "Error: create %s: %v\n", local, err)
		return 1
	}
	defer f.Close()

	total := int64(-1)
	if fi, statErr := ptyConn.Stat(remote); statErr == nil {
		total = fi.Size()
	}
	bar := progress.NewBar(os.Stderr, srv.Name+":"+remote, total)
	cw := &progress.CountingWriter{W: f, Fn: func(n int64) { bar.SetBytes(n) }}
	n, timedOut, err := ptyConn.Download(remote, cw, *timeoutMs)
	bar.Finish()
	if err != nil && !timedOut {
		fmt.Fprintf(out, "Error: download %s:%s -> %s: %v\n", srv.Name, remote, local, err)
		return 1
	}
	if timedOut {
		fmt.Fprintf(out, "downloaded %s:%s -> %s (timed out, %d bytes transferred)\n", srv.Name, remote, local, n)
		return 1
	}
	fmt.Fprintf(out, "downloaded %s:%s -> %s (%d bytes)\n", srv.Name, remote, local, n)
	return 0
}

// --- directory ---

// runFileUploadDir uploads a local directory tree to <name>:<remote>.
func runFileUploadDir(args []string, out io.Writer) int {
	fs := pflag.NewFlagSet("file upload-dir", pflag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "path to config.json")
	conflict := fs.String("conflict", "overwrite", "conflict policy: overwrite|skip|rename")
	concurrency := fs.Int("concurrency", 0, "parallel file transfers; 0 = default 4")
	timeoutMs := fs.Int("timeout", 0, "per-file timeout in ms; 0 = default 300s")
	fs.Usage = func() {
		fmt.Fprintf(out, "Usage: sshmng file upload-dir <name> <local> <remote> [--conflict overwrite|skip|rename] [--concurrency N] [--timeout <ms>] [--config <path>]\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		if fs.NArg() != 0 {
			fmt.Fprintf(out, "Error: expected 3 positional args (name local remote), got %d\n", fs.NArg())
		}
		return 2
	}
	name, local, remote := fs.Arg(0), fs.Arg(1), fs.Arg(2)

	_, cfg, err := BootstrapConfig(*configPath)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}
	ptyConn, srv, err := setupFileSession(cfg, name, *configPath, true)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}
	defer ptyConn.Close()
	if !ptyConn.SftpAvailable() {
		fmt.Fprintf(out, "Error: %s\n", sftpUnavailableReason(srv))
		return 1
	}

	opts := conn.DirTransferOptions{
		Conflict:    conn.ParseConflictPolicy(*conflict),
		Concurrency: *concurrency,
		TimeoutMs:   *timeoutMs,
	}
	totalBytes, totalFiles := localDirTotals(local)
	bar := progress.NewBar(os.Stderr, srv.Name+":"+remote, totalBytes)
	if totalFiles > 0 {
		bar.SetFiles(totalFiles)
	}
	opts.OnProgress = func(bytes int64, files int) {
		bar.SetBytes(bytes)
		bar.SetFilesDone(files)
	}
	res, err := ptyConn.UploadDir(local, remote, opts)
	bar.Finish()
	printDirResult(out, "uploaded", local, srv.Name, remote, res, err)
	if err != nil {
		return 1
	}
	return 0
}

// runFileDownloadDir downloads a remote directory tree to <local>.
func runFileDownloadDir(args []string, out io.Writer) int {
	fs := pflag.NewFlagSet("file download-dir", pflag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "path to config.json")
	conflict := fs.String("conflict", "overwrite", "conflict policy: overwrite|skip|rename")
	concurrency := fs.Int("concurrency", 0, "parallel file transfers; 0 = default 4")
	timeoutMs := fs.Int("timeout", 0, "per-file timeout in ms; 0 = default 300s")
	fs.Usage = func() {
		fmt.Fprintf(out, "Usage: sshmng file download-dir <name> <remote> <local> [--conflict overwrite|skip|rename] [--concurrency N] [--timeout <ms>] [--config <path>]\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		if fs.NArg() != 0 {
			fmt.Fprintf(out, "Error: expected 3 positional args (name remote local), got %d\n", fs.NArg())
		}
		return 2
	}
	name, remote, local := fs.Arg(0), fs.Arg(1), fs.Arg(2)

	_, cfg, err := BootstrapConfig(*configPath)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}
	ptyConn, srv, err := setupFileSession(cfg, name, *configPath, true)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}
	defer ptyConn.Close()
	if !ptyConn.SftpAvailable() {
		fmt.Fprintf(out, "Error: %s\n", sftpUnavailableReason(srv))
		return 1
	}

	opts := conn.DirTransferOptions{
		Conflict:    conn.ParseConflictPolicy(*conflict),
		Concurrency: *concurrency,
		TimeoutMs:   *timeoutMs,
	}
	totalBytes, totalFiles := ptyConn.RemoteDirTotals(remote)
	bar := progress.NewBar(os.Stderr, srv.Name+":"+remote, totalBytes)
	if totalFiles > 0 {
		bar.SetFiles(totalFiles)
	}
	opts.OnProgress = func(bytes int64, files int) {
		bar.SetBytes(bytes)
		bar.SetFilesDone(files)
	}
	res, err := ptyConn.DownloadDir(remote, local, opts)
	bar.Finish()
	printDirResult(out, "downloaded", local, srv.Name, remote, res, err)
	if err != nil {
		return 1
	}
	return 0
}

// localDirTotals walks localDir (via filepath.Walk) returning the total bytes
// and regular-file count of regular files (symlinks skipped, matching
// pty.UploadDir's walk). Used to size the progress bar before transfer starts.
// Walk errors are tolerated (returns counts seen so far) — a totals-walk
// failure must not abort the transfer.
func localDirTotals(localDir string) (bytes int64, files int) {
	filepath.Walk(localDir, func(_ string, fi os.FileInfo, err error) error {
		if err != nil || fi == nil {
			return nil
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !fi.IsDir() {
			bytes += fi.Size()
			files++
		}
		return nil
	})
	return
}

// printDirResult prints a human-readable dir transfer summary.
// Per-file errors are aggregated (err != nil means partial failure), but the
// summary still reports what was transferred.
func printDirResult(out io.Writer, verb, local, serverName, remote string, res conn.DirTransferResult, err error) {
	fmt.Fprintf(out, "%s %s -> %s:%s (%d files, %d bytes", verb, local, serverName, remote, res.Files, res.Bytes)
	if res.Skipped > 0 {
		fmt.Fprintf(out, ", %d skipped", res.Skipped)
	}
	if res.Renamed > 0 {
		fmt.Fprintf(out, ", %d renamed", res.Renamed)
	}
	if res.TimedOut > 0 {
		fmt.Fprintf(out, ", %d timed out", res.TimedOut)
	}
	fmt.Fprint(out, ")")
	if err != nil {
		fmt.Fprintf(out, " — partial: %v", err)
	}
	fmt.Fprintln(out)
}

// --- relay ---

// runFileRelay relays a file from <src-name>:<src-path> to each --to destination
// at <dst-path> (shared by all destinations). 1:N fanout via Manager.RelayTransfer;
// N=1 degrades to a single transfer. Per-destination failure isolation: one slow
// or failed destination does not abort the others.
func runFileRelay(args []string, out io.Writer) int {
	fs := pflag.NewFlagSet("file relay", pflag.ContinueOnError)
	fs.SetOutput(out)
	configPath := fs.String("config", "", "path to config.json")
	to := fs.StringSlice("to", nil, "destination server names (repeatable or comma-separated)")
	timeoutMs := fs.Int("timeout", 0, "transfer timeout in ms; 0 = default 300s")
	fs.Usage = func() {
		fmt.Fprintf(out, "Usage: sshmng file relay <src-name> <src-path> <dst-path> --to <dst1,dst2,...> [--timeout <ms>] [--config <path>]\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 3 {
		fs.Usage()
		if fs.NArg() != 0 {
			fmt.Fprintf(out, "Error: expected 3 positional args (src-name src-path dst-path), got %d\n", fs.NArg())
		}
		return 2
	}
	if len(*to) == 0 {
		fs.Usage()
		fmt.Fprintln(out, "Error: --to is required (one or more destination server names)")
		return 2
	}
	srcName, srcPath, dstPath := fs.Arg(0), fs.Arg(1), fs.Arg(2)

	_, cfg, err := BootstrapConfig(*configPath)
	if err != nil {
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}

	knownHosts := conn.NewKnownHostsStore(KnownHostsPath(*configPath))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	dialer := conn.NewDialer(knownHosts, logger)
	manager := session.NewManager()

	// Login source. allowPrompt=true for interactive name resolution (same as ssh).
	srcSid, srcServerName, err := loginRelaySession(cfg, dialer, manager, srcName, true, logger)
	if err != nil {
		fmt.Fprintf(out, "Error: source %s: %v\n", srcName, err)
		return 1
	}
	allSids := []string{srcSid}

	// Login each destination. allowPrompt=false: relay is often scripted, and a
	// multi-match interactive prompt per destination is hostile. Multi-match
	// returns an error listing candidates. Login failures are reported but do not
	// abort the relay — RelayTransfer handles whatever destinations connected.
	dstSids := make([]string, 0, len(*to))
	dstNames := make(map[string]string, len(*to)) // sid -> name, for output fallback
	for _, dstName := range *to {
		dstSid, _, err := loginRelaySession(cfg, dialer, manager, dstName, false, logger)
		if err != nil {
			fmt.Fprintf(out, "%s: failed: %v\n", dstName, err)
			continue
		}
		dstSids = append(dstSids, dstSid)
		dstNames[dstSid] = dstName
		allSids = append(allSids, dstSid)
	}
	// Always close every session we opened before returning.
	defer closeRelaySessions(manager, allSids)

	if len(dstSids) == 0 {
		fmt.Fprintf(out, "Error: no relay destinations connected (%d failed)\n", len(*to))
		return 1
	}

	res, err := manager.RelayTransfer(srcSid, srcPath, dstSids, dstPath, *timeoutMs)
	if err != nil {
		// Hard error: srcSid invalid (shouldn't happen here) or dstSids empty.
		fmt.Fprintf(out, "Error: %v\n", err)
		return 1
	}

	okCount := 0
	for _, d := range res.Destinations {
		name := d.DstServer
		if name == "" {
			name = dstNames[d.DstSid] // pre-flight-failed dest has no DstServer
		}
		if d.OK {
			fmt.Fprintf(out, "%s: ok (%d bytes)\n", name, d.Bytes)
			okCount++
		} else {
			errStr := "unknown error"
			if d.Err != nil {
				errStr = d.Err.Error()
			}
			fmt.Fprintf(out, "%s: failed: %s\n", name, errStr)
		}
	}
	total := len(*to)
	fmt.Fprintf(out, "relay %s -> %d destinations: %d ok, %d failed\n", srcServerName, total, okCount, total-okCount)
	if okCount < total {
		return 1
	}
	return 0
}

// loginRelaySession resolves <name>, dials, enables sftp, and registers the
// session into manager with idleTimeout=0 (no auto-close; relay is one-shot and
// the handler closes all sessions when done — a 0 timeout avoids the idle timer
// killing a session mid-transfer of a large file).
//
// Returns the sid and resolved server name. sftp unavailable (Pattern B or
// negotiate failure) closes the ptyConn and returns an error — the caller
// reports it per-destination without aborting the whole relay.
func loginRelaySession(cfg *config.Config, dialer *conn.Dialer, manager *session.Manager, name string, allowPrompt bool, logger *slog.Logger) (sid, serverName string, err error) {
	srv, err := resolveSSHServer(cfg, name, allowPrompt)
	if err != nil {
		return "", "", err
	}
	sid, _ = conn.RandomSID()
	ptyConn, err := setupSSH(srv, dialer, sid, logger, false, true)
	if err != nil {
		return "", srv.Name, err
	}
	if !ptyConn.SftpAvailable() {
		ptyConn.Close()
		return "", srv.Name, fmt.Errorf("%s", sftpUnavailableReason(srv))
	}
	manager.NewSession(sid, srv.Name, ptyConn, 0, logger)
	return sid, srv.Name, nil
}

// closeRelaySessions closes every session in sids. Errors are ignored: sessions
// may already be closed (RelayTransfer does not close them) and a double-close
// is harmless. Missing sids (removed from the manager) are skipped.
func closeRelaySessions(manager *session.Manager, sids []string) {
	for _, sid := range sids {
		if sess, err := manager.Get(sid); err == nil {
			sess.Close()
		}
	}
}
