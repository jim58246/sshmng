# Quickstart (CLI Restructure + install + doctor) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restructure sshmng CLI into subcommand mode (`mcp`/`install`/`doctor`/`help`) with end-to-end first-time setup via `sshmng install` and verification via `sshmng doctor`.

**Architecture:** New `internal/cli` package owns dispatch + all subcommands. `cmd/sshmng/main.go` becomes a thin entry that calls `cli.Dispatch`. Agent config injection handled by per-Agent `AgentInjector` implementations (Claude Code JSON, Hermes YAML, OpenCode JSON-with-array-command). File scaffolding reuses `config.Store` for validation.

**Tech Stack:** Go 1.25 stdlib + `gopkg.in/yaml.v3` (new, for Hermes config) + existing `modelcontextprotocol/go-sdk`, `pkg/sftp`, `golang.org/x/crypto`.

**Spec:** `docs/superpowers/specs/2026-07-23-quickstart-design.md`

## Global Constraints

- Go 1.25.0 (`go.mod` already at `go 1.25.0`)
- New dependency: `gopkg.in/yaml.v3` (only for Hermes config)
- No `golang.org/x/term` (TTY detection dropped with menu removal)
- Unix perms: 0700 for `~/.sshmng/`, 0600 for files. Windows: skip perm checks, warn NTFS ACL
- Output characters: ASCII only (`[ok]` / `->` / `*`), no Unicode `✓→•` (Windows cmd.exe compat)
- Exit codes: 0 success / 1 runtime fail / 2 usage error
- Atomic writes: temp file + rename (Unix); backup + delete + rename (Windows)
- Backups: `<original>.bak.<YYYYMMDD-HHMMSS>`, never auto-deleted
- TDD: test before implementation, `go test -race ./...` must pass after each task
- Commit after each task (or each step within a task)

---

## File Structure

```
cmd/sshmng/
  main.go                    # Modified: thin entry, calls cli.Dispatch
  main_test.go               # Modified: remove (tests move to internal/cli/mcp_test.go)
  e2e_test.go                # Existing, may need update for new subcommand
internal/cli/                # New package
  cli.go                     # Dispatch() + printHelp()
  mcp.go                     # runMCP() — moved from cmd/sshmng/main.go
  install.go                 # runInstall() + wizard flow
  doctor.go                  # runDoctor() + check logic
  agent_inject.go            # AgentInjector interface + shared merge/backup/atomic-write
  agent_claudecode.go        # ClaudeCodeInjector
  agent_hermes.go            # HermesInjector
  agent_opencode.go          # OpenCodeInjector
  files.go                   # ScaffoldHome() + config.json/config.example.json templates
  prompt.go                  # promptString/promptConfirm/promptSelect helpers
  cli_test.go                # dispatch routing tests
  mcp_test.go                # resolveConfigPath/openLogWriter tests (moved from main_test.go)
  install_test.go            # install end-to-end tests
  doctor_test.go             # doctor check tests
  agent_inject_test.go       # shared injector helpers tests
  agent_claudecode_test.go   # ClaudeCodeInjector tests
  agent_hermes_test.go       # HermesInjector tests
  agent_opencode_test.go     # OpenCodeInjector tests
  files_test.go              # scaffolding tests
  prompt_test.go             # prompt helper tests
README.md                    # Updated: integration guide for 3 Agents, install flow
```

---

## Task 1: CLI Dispatch Foundation + MCP Subcommand

Refactor `cmd/sshmng/main.go` into a thin entry that calls `cli.Dispatch`. Move all existing MCP logic to `internal/cli/mcp.go`. Add `cli.go` with Dispatch routing for `mcp`/`help`/unknown (install/doctor cases added in Tasks 6/7).

**Files:**
- Create: `internal/cli/cli.go`
- Create: `internal/cli/mcp.go`
- Create: `internal/cli/cli_test.go`
- Create: `internal/cli/mcp_test.go`
- Modify: `cmd/sshmng/main.go`
- Delete: `cmd/sshmng/main_test.go` (tests move to `internal/cli/mcp_test.go`)

**Interfaces:**
- Produces: `cli.Dispatch(ctx context.Context, args []string, out io.Writer) int` — entry for all subcommands
- Produces: `cli.runMCP(ctx context.Context, args []string, out io.Writer) int` — MCP server subcommand
- Consumes: `config.NewStore`, `config.Store.Load`, `conn.NewKnownHostsStore`, `mcp.NewService`, `mcp.Service.Run` (all existing)

- [ ] **Step 1: Write failing test for Dispatch routing**

Create `internal/cli/cli_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDispatchNoArgsPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	code := Dispatch(context.Background(), nil, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected help output, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "sshmng mcp") {
		t.Errorf("expected 'sshmng mcp' in help, got: %s", out.String())
	}
}

func TestDispatchHelpFlagsPrintHelp(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var out bytes.Buffer
			code := Dispatch(context.Background(), []string{arg}, &out)
			if code != 0 {
				t.Errorf("code = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "Usage:") {
				t.Errorf("expected help output for %q, got: %s", arg, out.String())
			}
		})
	}
}

func TestDispatchUnknownCommandErrors(t *testing.T) {
	var out bytes.Buffer
	code := Dispatch(context.Background(), []string{"foobar"}, &out)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "Unknown command") {
		t.Errorf("expected 'Unknown command' error, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "sshmng help") {
		t.Errorf("expected hint to run 'sshmng help', got: %s", out.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/cli/`
Expected: FAIL — package `cli` doesn't exist or `Dispatch` undefined.

- [ ] **Step 3: Write cli.go with Dispatch + printHelp**

Create `internal/cli/cli.go`:

```go
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

const helpText = `sshmng — SSH session manager

Usage:
  sshmng                          Print this help and exit
  sshmng mcp [--config <path>]    Start MCP server (stdio)
  sshmng install [...]            First-time setup
  sshmng doctor [...]             Verify setup
  sshmng help | -h | --help       Print this help

Subcommands:
  mcp       Start the MCP server. This is what Agent configs should use
            (e.g. "command": "sshmng", "args": ["mcp"]).
  install   Create ~/.sshmng/, generate config templates, and inject sshmng
            into your AI Agent(s) (Claude Code / Hermes Agent / OpenCode).
  doctor    Verify setup is correct: files, permissions, Agent config entries.

Run 'sshmng <subcommand> -h' for subcommand-specific flags.
`
```

- [ ] **Step 4: Write mcp.go (move logic from main.go)**

Create `internal/cli/mcp.go`:

```go
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
```

- [ ] **Step 5: Move main_test.go tests to internal/cli/mcp_test.go**

Create `internal/cli/mcp_test.go` by copying the content of `cmd/sshmng/main_test.go`, changing `package main` to `package cli`. Keep all existing test functions (`TestResolveConfigPathExplicit`, `TestResolveConfigPathSSHMNGHome`, `TestResolveConfigPathDefaultHome`, and any `openLogWriter` tests). Delete `cmd/sshmng/main_test.go`.

Read the current `cmd/sshmng/main_test.go` to copy its exact content. Then write `internal/cli/mcp_test.go` with `package cli` instead of `package main`. Then `rm cmd/sshmng/main_test.go`.

- [ ] **Step 6: Refactor cmd/sshmng/main.go to thin entry**

Replace `cmd/sshmng/main.go` content with:

```go
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
```

- [ ] **Step 7: Run all tests and verify**

Run: `go test -race ./...`
Expected: PASS — all existing tests still pass (now under `internal/cli`), new dispatch tests pass.

Run: `go build -o /tmp/sshmng ./cmd/sshmng && /tmp/sshmng help`
Expected: prints help text with "Usage:" and "sshmng mcp" lines.

Run: `/tmp/sshmng foobar`
Expected: prints "Unknown command 'foobar'. Run 'sshmng help' for usage." and exits with code 2 (verify with `echo $?`).

- [ ] **Step 8: Commit**

```bash
git add cmd/sshmng/main.go internal/cli/ cmd/sshmng/main_test.go
git commit -m "$(cat <<'EOF'
refactor: restructure CLI into subcommand dispatch

Move MCP server logic from cmd/sshmng/main.go to internal/cli/mcp.go.
Add cli.Dispatch routing for mcp/help/unknown (install/doctor cases
added in later tasks). No-arg prints help and exits 0. main.go is now
a thin entry calling cli.Dispatch.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: AgentInjector Interface + ClaudeCodeInjector

Define the `AgentInjector` interface and shared merge/backup/atomic-write helpers. Implement `ClaudeCodeInjector` (JSON, `mcpServers` key, string command). This task establishes the pattern that Hermes and OpenCode injectors follow.

**Files:**
- Create: `internal/cli/agent_inject.go`
- Create: `internal/cli/agent_claudecode.go`
- Create: `internal/cli/agent_inject_test.go`
- Create: `internal/cli/agent_claudecode_test.go`

**Interfaces:**
- Produces: `MCPEntry{BinaryPath string, Args []string, Env map[string]string}` — the sshmng entry to inject
- Produces: `AgentInjector` interface with `Name()`, `DisplayName()`, `Detect()`, `Inject()`, `Verify()`
- Produces: shared helpers `loadJSONMap`, `loadYAMLMap`, `writeMapAtomic`, `backupFile`, `mergeEntry`
- Consumes: `os.UserHomeDir`, `encoding/json`, `os.Rename`, `time.Now`

- [ ] **Step 1: Write failing test for ClaudeCodeInjector**

Create `internal/cli/agent_claudecode_test.go`:

```go
package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func newClaudeCodeInjectorForTest(t *testing.T) (*ClaudeCodeInjector, string) {
	t.Helper()
	tmp := t.TempDir()
	home := filepath.Join(tmp, "claude.json")
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	return &ClaudeCodeInjector{}, home
}

func TestClaudeCodeDetectNotInstalled(t *testing.T) {
	inj, _ := newClaudeCodeInjectorForTest(t)
	_, installed := inj.Detect()
	if installed {
		t.Error("expected installed=false when no config file")
	}
}

func TestClaudeCodeDetectInstalled(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, installed := inj.Detect()
	if !installed {
		t.Error("expected installed=true")
	}
	if got != path {
		t.Errorf("got %q, want %q", got, path)
	}
}

func TestClaudeCodeInjectCreatesEntry(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
	entry := MCPEntry{
		BinaryPath: "/usr/local/bin/sshmng",
		Args:       []string{"mcp"},
		Env:        map[string]string{"SSHMNG_HOME": "/home/user/.sshmng"},
	}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `"command": "/usr/local/bin/sshmng"`
	if !contains(string(data), want) {
		t.Errorf("missing %q in:\n%s", want, string(data))
	}
	wantArgs := `"args": ["mcp"]`
	if !contains(string(data), wantArgs) {
		t.Errorf("missing %q in:\n%s", wantArgs, string(data))
	}
	wantEnv := `"SSHMNG_HOME": "/home/user/.sshmng"`
	if !contains(string(data), wantEnv) {
		t.Errorf("missing %q in:\n%s", wantEnv, string(data))
	}

	// sshmng entry nested under mcpServers
	if !contains(string(data), `"mcpServers"`) {
		t.Errorf("missing mcpServers key in:\n%s", string(data))
	}
}

func TestClaudeCodeInjectPreservesOtherServers(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
	original := `{"mcpServers":{"other":{"command":"x","args":["y"]}},"theme":"dark"}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !contains(s, `"other"`) {
		t.Errorf("other server entry lost:\n%s", s)
	}
	if !contains(s, `"theme": "dark"`) {
		t.Errorf("theme field lost:\n%s", s)
	}
	if !contains(s, `"sshmng"`) {
		t.Errorf("sshmng entry missing:\n%s", s)
	}
}

func TestClaudeCodeInjectCreatesBackup(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
	original := `{"mcpServers":{"other":{"command":"x"}}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	backupCount := 0
	for _, e := range entries {
		if name := e.Name(); len(name) > len("claude.json.bak.") && name[:len("claude.json.bak.")] == "claude.json.bak." {
			backupCount++
		}
	}
	if backupCount != 1 {
		t.Errorf("expected 1 backup, got %d", backupCount)
	}
}

func TestClaudeCodeVerifyMatches(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := inj.Verify(path, "/sshmng"); err != nil {
		t.Errorf("Verify should pass: %v", err)
	}
}

func TestClaudeCodeVerifyStaleBinary(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
	entry := MCPEntry{BinaryPath: "/old/bin/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	err := inj.Verify(path, "/new/bin/sshmng")
	if err == nil {
		t.Error("Verify should fail for stale binary path")
	}
}

func TestClaudeCodeVerifyMissingEntry(t *testing.T) {
	inj, path := newClaudeCodeInjectorForTest(t)
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	err := inj.Verify(path, "/sshmng")
	if err == nil {
		t.Error("Verify should fail when sshmng entry missing")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || (strings.Contains(s, sub)))
}
```

Add `"strings"` to imports if not already present (it's used by `contains`). Actually, replace the local `contains` helper with `strings.Contains` directly. Simpler: remove the local helper and use `strings.Contains(s, sub)` throughout.

(Refactor the test to use `strings.Contains` instead of the local helper.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/cli/`
Expected: FAIL — `ClaudeCodeInjector`, `MCPEntry` undefined.

- [ ] **Step 3: Write agent_inject.go (interface + shared helpers)**

Create `internal/cli/agent_inject.go`:

```go
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// MCPEntry is the sshmng MCP server entry written into Agent configs.
type MCPEntry struct {
	BinaryPath string
	Args       []string
	Env        map[string]string
}

// AgentInjector knows how to inject and verify the sshmng MCP entry in a
// specific AI Agent's config file.
type AgentInjector interface {
	Name() string                                // short identifier, e.g. "claude-code"
	DisplayName() string                         // human-friendly, e.g. "Claude Code"
	Detect() (configPath string, installed bool) // check if Agent is installed
	Inject(path string, entry MCPEntry) error    // merge sshmng entry into config
	Verify(path string, expectedBinary string) error
}

// backupFile copies path to <path>.bak.<YYYYMMDD-HHMMSS>. Does not delete old
// backups. If path does not exist, returns nil (no-op).
func backupFile(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s for backup: %w", path, err)
	}
	ts := time.Now().Format("20060102-150405")
	backupPath := fmt.Sprintf("%s.bak.%s", path, ts)
	if err := os.WriteFile(backupPath, data, 0600); err != nil {
		return fmt.Errorf("write backup %s: %w", backupPath, err)
	}
	return nil
}

// loadJSONMap reads path as JSON into a map. Empty/missing file -> empty map.
func loadJSONMap(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	m := map[string]any{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s as JSON: %w", path, err)
	}
	return m, nil
}

// writeJSONMapAtomic writes m as indented JSON to path atomically:
//  1. Write temp file in same dir
//  2. Chmod 0600 (Unix; no-op on Windows)
//  3. Rename (Unix) or backup+delete+rename (Windows)
//
// Caller must call backupFile first if backups are desired.
func writeJSONMapAtomic(path string, m map[string]any) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".agent.json.tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if rename succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmpName, 0600); err != nil {
			return fmt.Errorf("chmod temp: %w", err)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		if runtime.GOOS == "windows" {
			// Windows rename fails if destination exists; remove then retry.
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
				return fmt.Errorf("remove old %s: %w (rename err: %v)", path, rmErr, err)
			}
			if err := os.Rename(tmpName, path); err != nil {
				return fmt.Errorf("rename temp to %s: %w", path, err)
			}
			return nil
		}
		return fmt.Errorf("rename temp to %s: %w", path, err)
	}
	return nil
}

// mergeEntry sets m[topKey]["sshmng"] = entryMap, creating intermediate maps as
// needed. Other entries under topKey are preserved.
func mergeEntry(m map[string]any, topKey string, entryMap map[string]any) {
	servers, _ := m[topKey].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers["sshmng"] = entryMap
	m[topKey] = servers
}
```

- [ ] **Step 4: Write agent_claudecode.go**

Create `internal/cli/agent_claudecode.go`:

```go
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ClaudeCodeInjector handles ~/.claude.json (JSON, mcpServers key).
type ClaudeCodeInjector struct{}

func (c *ClaudeCodeInjector) Name() string        { return "claude-code" }
func (c *ClaudeCodeInjector) DisplayName() string { return "Claude Code" }

func (c *ClaudeCodeInjector) Detect() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	path := filepath.Join(home, ".claude.json")
	if _, err := os.Stat(path); err != nil {
		return path, false
	}
	return path, true
}

// entryMap builds the JSON map for the sshmng entry under mcpServers.sshmng.
// Schema: {command: string, args: [...], env: {...}}
func (c *ClaudeCodeInjector) entryMap(entry MCPEntry) map[string]any {
	return map[string]any{
		"command": entry.BinaryPath,
		"args":    entry.Args,
		"env":     entry.Env,
	}
}

func (c *ClaudeCodeInjector) Inject(path string, entry MCPEntry) error {
	if err := backupFile(path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	m, err := loadJSONMap(path)
	if err != nil {
		return err
	}
	mergeEntry(m, "mcpServers", c.entryMap(entry))
	if err := writeJSONMapAtomic(path, m); err != nil {
		return err
	}
	// Read back to verify
	if _, err := loadJSONMap(path); err != nil {
		return fmt.Errorf("post-write verify: %w", err)
	}
	return nil
}

func (c *ClaudeCodeInjector) Verify(path string, expectedBinary string) error {
	m, err := loadJSONMap(path)
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return fmt.Errorf("no mcpServers key in %s", path)
	}
	sshmng, _ := servers["sshmng"].(map[string]any)
	if sshmng == nil {
		return fmt.Errorf("no sshmng entry in mcpServers")
	}
	cmd, _ := sshmng["command"].(string)
	if cmd != expectedBinary {
		return fmt.Errorf("stale: expected command %q, got %q", expectedBinary, cmd)
	}
	return nil
}

// Used by tests: parse entry back to MCPEntry for inspection.
func parseClaudeCodeEntry(m map[string]any) (MCPEntry, error) {
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return MCPEntry{}, fmt.Errorf("no mcpServers")
	}
	sshmng, _ := servers["sshmng"].(map[string]any)
	if sshmng == nil {
		return MCPEntry{}, fmt.Errorf("no sshmng entry")
	}
	data, err := json.Marshal(sshmng)
	if err != nil {
		return MCPEntry{}, err
	}
	var e MCPEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return MCPEntry{}, err
	}
	return e, nil
}
```

- [ ] **Step 5: Run tests and verify**

Run: `go test -race ./internal/cli/`
Expected: PASS — all ClaudeCodeInjector tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/agent_inject.go internal/cli/agent_claudecode.go internal/cli/agent_inject_test.go internal/cli/agent_claudecode_test.go
git commit -m "$(cat <<'EOF'
feat(cli): AgentInjector interface + ClaudeCodeInjector

Shared helpers (backupFile, loadJSONMap, writeJSONMapAtomic, mergeEntry)
in agent_inject.go. ClaudeCodeInjector writes to ~/.claude.json under
mcpServers key with {command, args, env} schema. Hermes and OpenCode
injectors follow in subsequent tasks.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: HermesInjector (YAML)

Add `gopkg.in/yaml.v3` dependency. Implement `HermesInjector` — `~/.hermes/config.yaml` on Unix, `%LOCALAPPDATA%\hermes\config.yaml` on Windows. YAML uses `mcp_servers` top-level key with same per-server schema as Claude Code (`command` string, `args` list, `env` map).

**Files:**
- Modify: `go.mod`, `go.sum` (add `gopkg.in/yaml.v3`)
- Create: `internal/cli/agent_hermes.go`
- Create: `internal/cli/agent_hermes_test.go`

**Interfaces:**
- Produces: `HermesInjector` struct implementing `AgentInjector`
- Produces: `loadYAMLMap(path)`, `writeYAMLMapAtomic(path, m)` helpers (in `agent_inject.go` or `agent_hermes.go`)
- Consumes: `gopkg.in/yaml.v3`, `runtime.GOOS`, `os.Getenv("LOCALAPPDATA")`

- [ ] **Step 1: Add yaml.v3 dependency**

Run: `go get gopkg.in/yaml.v3`
Then verify `go.mod` contains `gopkg.in/yaml.v3` in the `require` block.

- [ ] **Step 2: Write failing test for HermesInjector**

Create `internal/cli/agent_hermes_test.go`:

```go
package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newHermesInjectorForTest(t *testing.T) (*HermesInjector, string) {
	t.Helper()
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", tmp)
		path := filepath.Join(tmp, "hermes", "config.yaml")
		return &HermesInjector{}, path
	}
	t.Setenv("HOME", tmp)
	path := filepath.Join(tmp, ".hermes", "config.yaml")
	return &HermesInjector{}, path
}

func TestHermesDetectNotInstalled(t *testing.T) {
	inj, _ := newHermesInjectorForTest(t)
	_, installed := inj.Detect()
	if installed {
		t.Error("expected installed=false")
	}
}

func TestHermesDetectInstalled(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("model: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, installed := inj.Detect()
	if !installed {
		t.Error("expected installed=true")
	}
	if got != path {
		t.Errorf("got %q, want %q", got, path)
	}
}

func TestHermesInjectCreatesEntry(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	entry := MCPEntry{
		BinaryPath: "/usr/local/bin/sshmng",
		Args:       []string{"mcp"},
		Env:        map[string]string{"SSHMNG_HOME": "/home/user/.sshmng"},
	}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "mcp_servers:") {
		t.Errorf("missing mcp_servers key:\n%s", s)
	}
	if !strings.Contains(s, "sshmng:") {
		t.Errorf("missing sshmng entry:\n%s", s)
	}
	if !strings.Contains(s, "command: /usr/local/bin/sshmng") {
		t.Errorf("missing command:\n%s", s)
	}
	if !strings.Contains(s, "- mcp") {
		t.Errorf("missing args:\n%s", s)
	}
	if !strings.Contains(s, "SSHMNG_HOME: /home/user/.sshmng") {
		t.Errorf("missing env:\n%s", s)
	}
}

func TestHermesInjectPreservesOtherServers(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	original := "model:\n  name: hermes-3\nmcp_servers:\n  other:\n    command: x\n"
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, "name: hermes-3") {
		t.Errorf("model field lost:\n%s", s)
	}
	if !strings.Contains(s, "other:") {
		t.Errorf("other server lost:\n%s", s)
	}
	if !strings.Contains(s, "sshmng:") {
		t.Errorf("sshmng entry missing:\n%s", s)
	}
}

func TestHermesInjectCreatesBackup(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("model: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	backupCount := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "config.yaml.bak.") {
			backupCount++
		}
	}
	if backupCount != 1 {
		t.Errorf("expected 1 backup, got %d", backupCount)
	}
}

func TestHermesVerifyMatches(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := inj.Verify(path, "/sshmng"); err != nil {
		t.Errorf("Verify should pass: %v", err)
	}
}

func TestHermesVerifyStaleBinary(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	entry := MCPEntry{BinaryPath: "/old/bin/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := inj.Verify(path, "/new/bin/sshmng"); err == nil {
		t.Error("Verify should fail for stale binary")
	}
}

func TestHermesVerifyMissingEntry(t *testing.T) {
	inj, path := newHermesInjectorForTest(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("mcp_servers: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := inj.Verify(path, "/sshmng"); err == nil {
		t.Error("Verify should fail when sshmng entry missing")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test -race ./internal/cli/`
Expected: FAIL — `HermesInjector` undefined.

- [ ] **Step 4: Write agent_hermes.go**

Create `internal/cli/agent_hermes.go`:

```go
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// HermesInjector handles ~/.hermes/config.yaml (Unix) or
// %LOCALAPPDATA%\hermes\config.yaml (Windows). YAML, mcp_servers key.
type HermesInjector struct{}

func (h *HermesInjector) Name() string        { return "hermes" }
func (h *HermesInjector) DisplayName() string { return "Hermes Agent" }

func (h *HermesInjector) configPath() string {
	if runtime.GOOS == "windows" {
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData == "" {
			localAppData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
		}
		return filepath.Join(localAppData, "hermes", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".hermes", "config.yaml")
}

func (h *HermesInjector) Detect() (string, bool) {
	path := h.configPath()
	if path == "" {
		return path, false
	}
	if _, err := os.Stat(path); err != nil {
		return path, false
	}
	return path, true
}

// entryMap builds the YAML map for the sshmng entry. Same shape as Claude Code
// (command string, args list, env map) — yaml.v3 marshals map[string]any fine.
func (h *HermesInjector) entryMap(entry MCPEntry) map[string]any {
	return map[string]any{
		"command": entry.BinaryPath,
		"args":    entry.Args,
		"env":     entry.Env,
	}
}

func (h *HermesInjector) Inject(path string, entry MCPEntry) error {
	if err := backupFile(path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	m, err := loadYAMLMap(path)
	if err != nil {
		return err
	}
	mergeEntry(m, "mcp_servers", h.entryMap(entry))
	if err := writeYAMLMapAtomic(path, m); err != nil {
		return err
	}
	if _, err := loadYAMLMap(path); err != nil {
		return fmt.Errorf("post-write verify: %w", err)
	}
	return nil
}

func (h *HermesInjector) Verify(path string, expectedBinary string) error {
	m, err := loadYAMLMap(path)
	if err != nil {
		return err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return fmt.Errorf("no mcp_servers key in %s", path)
	}
	sshmng, _ := servers["sshmng"].(map[string]any)
	if sshmng == nil {
		return fmt.Errorf("no sshmng entry in mcp_servers")
	}
	cmd, _ := sshmng["command"].(string)
	if cmd != expectedBinary {
		return fmt.Errorf("stale: expected command %q, got %q", expectedBinary, cmd)
	}
	return nil
}

// loadYAMLMap reads path as YAML into a map. Empty/missing file -> empty map.
func loadYAMLMap(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	m := map[string]any{}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s as YAML: %w", path, err)
	}
	return m, nil
}

// writeYAMLMapAtomic writes m as YAML to path atomically. Same pattern as
// writeJSONMapAtomic.
func writeYAMLMapAtomic(path string, m map[string]any) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal yaml: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".agent.yaml.tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmpName, 0600); err != nil {
			return fmt.Errorf("chmod temp: %w", err)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		if runtime.GOOS == "windows" {
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
				return fmt.Errorf("remove old %s: %w (rename err: %v)", path, rmErr, err)
			}
			if err := os.Rename(tmpName, path); err != nil {
				return fmt.Errorf("rename temp to %s: %w", path, err)
			}
			return nil
		}
		return fmt.Errorf("rename temp to %s: %w", path, err)
	}
	return nil
}
```

- [ ] **Step 5: Run tests and verify**

Run: `go test -race ./internal/cli/`
Expected: PASS — all HermesInjector tests pass.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/cli/agent_hermes.go internal/cli/agent_hermes_test.go
git commit -m "$(cat <<'EOF'
feat(cli): HermesInjector for ~/.hermes/config.yaml

Adds gopkg.in/yaml.v3 dependency. HermesInjector writes to mcp_servers
key with {command, args, env} schema. Path differs by OS:
~/.hermes/config.yaml (Unix) vs %LOCALAPPDATA%\hermes\config.yaml
(Windows).

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: OpenCodeInjector

Implement `OpenCodeInjector` — `~/.config/opencode/opencode.json`. JSON, `mcp` top-level key. Schema differs: `command` is an **array** (binary + args combined), env field is `environment` (not `env`), and requires `type: "local"` + `enabled: true`.

**Files:**
- Create: `internal/cli/agent_opencode.go`
- Create: `internal/cli/agent_opencode_test.go`

**Interfaces:**
- Produces: `OpenCodeInjector` struct implementing `AgentInjector`
- Consumes: `loadJSONMap`, `writeJSONMapAtomic`, `mergeEntry`, `backupFile` (from Task 2)

- [ ] **Step 1: Write failing test for OpenCodeInjector**

Create `internal/cli/agent_opencode_test.go`:

```go
package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newOpenCodeInjectorForTest(t *testing.T) (*OpenCodeInjector, string) {
	t.Helper()
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	path := filepath.Join(tmp, ".config", "opencode", "opencode.json")
	return &OpenCodeInjector{}, path
}

func TestOpenCodeDetectNotInstalled(t *testing.T) {
	inj, _ := newOpenCodeInjectorForTest(t)
	_, installed := inj.Detect()
	if installed {
		t.Error("expected installed=false")
	}
}

func TestOpenCodeDetectInstalled(t *testing.T) {
	inj, path := newOpenCodeInjectorForTest(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, installed := inj.Detect()
	if !installed {
		t.Error("expected installed=true")
	}
	if got != path {
		t.Errorf("got %q, want %q", got, path)
	}
}

func TestOpenCodeInjectCreatesEntry(t *testing.T) {
	inj, path := newOpenCodeInjectorForTest(t)
	entry := MCPEntry{
		BinaryPath: "/usr/local/bin/sshmng",
		Args:       []string{"mcp"},
		Env:        map[string]string{"SSHMNG_HOME": "/home/user/.sshmng"},
	}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"mcp"`) {
		t.Errorf("missing mcp key:\n%s", s)
	}
	if !strings.Contains(s, `"sshmng"`) {
		t.Errorf("missing sshmng entry:\n%s", s)
	}
	if !strings.Contains(s, `"type": "local"`) {
		t.Errorf("missing type:local:\n%s", s)
	}
	// command is an array combining binary + args
	if !strings.Contains(s, `"command": ["/usr/local/bin/sshmng", "mcp"]`) {
		t.Errorf("missing command array:\n%s", s)
	}
	if !strings.Contains(s, `"environment"`) {
		t.Errorf("missing environment field (not env):\n%s", s)
	}
	if strings.Contains(s, `"env":`) {
		t.Errorf("should not have env field:\n%s", s)
	}
	if !strings.Contains(s, `"enabled": true`) {
		t.Errorf("missing enabled:true:\n%s", s)
	}
}

func TestOpenCodeInjectPreservesOtherServers(t *testing.T) {
	inj, path := newOpenCodeInjectorForTest(t)
	original := `{"mcp":{"other":{"type":"local","command":["x"]}},"theme":"dark"}`
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, `"other"`) {
		t.Errorf("other server lost:\n%s", s)
	}
	if !strings.Contains(s, `"theme": "dark"`) {
		t.Errorf("theme lost:\n%s", s)
	}
	if !strings.Contains(s, `"sshmng"`) {
		t.Errorf("sshmng missing:\n%s", s)
	}
}

func TestOpenCodeVerifyMatches(t *testing.T) {
	inj, path := newOpenCodeInjectorForTest(t)
	entry := MCPEntry{BinaryPath: "/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := inj.Verify(path, "/sshmng"); err != nil {
		t.Errorf("Verify should pass: %v", err)
	}
}

func TestOpenCodeVerifyStaleBinary(t *testing.T) {
	inj, path := newOpenCodeInjectorForTest(t)
	entry := MCPEntry{BinaryPath: "/old/bin/sshmng", Args: []string{"mcp"}}
	if err := inj.Inject(path, entry); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := inj.Verify(path, "/new/bin/sshmng"); err == nil {
		t.Error("Verify should fail for stale binary")
	}
}

func TestOpenCodeVerifyCommandNotArray(t *testing.T) {
	inj, path := newOpenCodeInjectorForTest(t)
	// Manually write a malformed config where command is a string, not array
	original := `{"mcp":{"sshmng":{"type":"local","command":"/sshmng","environment":{},"enabled":true}}}`
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := inj.Verify(path, "/sshmng"); err == nil {
		t.Error("Verify should fail when command is not an array")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/cli/`
Expected: FAIL — `OpenCodeInjector` undefined.

- [ ] **Step 3: Write agent_opencode.go**

Create `internal/cli/agent_opencode.go`:

```go
package cli

import (
	"fmt"
	"os"
	"path/filepath"
)

// OpenCodeInjector handles ~/.config/opencode/opencode.json (JSON, mcp key).
// Schema differs from Claude Code / Hermes:
//   - command is an array (binary + args combined)
//   - env field is called "environment"
//   - requires type: "local" and enabled: true
type OpenCodeInjector struct{}

func (o *OpenCodeInjector) Name() string        { return "opencode" }
func (o *OpenCodeInjector) DisplayName() string { return "OpenCode" }

func (o *OpenCodeInjector) Detect() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	if _, err := os.Stat(path); err != nil {
		return path, false
	}
	return path, true
}

// entryMap builds the JSON map for the sshmng entry under mcp.sshmng.
// command is an array combining binary + args; env field is "environment".
func (o *OpenCodeInjector) entryMap(entry MCPEntry) map[string]any {
	command := make([]any, 0, len(entry.Args)+1)
	command = append(command, entry.BinaryPath)
	for _, a := range entry.Args {
		command = append(command, a)
	}
	return map[string]any{
		"type":        "local",
		"command":     command,
		"environment": entry.Env,
		"enabled":     true,
	}
}

func (o *OpenCodeInjector) Inject(path string, entry MCPEntry) error {
	if err := backupFile(path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	m, err := loadJSONMap(path)
	if err != nil {
		return err
	}
	mergeEntry(m, "mcp", o.entryMap(entry))
	if err := writeJSONMapAtomic(path, m); err != nil {
		return err
	}
	if _, err := loadJSONMap(path); err != nil {
		return fmt.Errorf("post-write verify: %w", err)
	}
	return nil
}

func (o *OpenCodeInjector) Verify(path string, expectedBinary string) error {
	m, err := loadJSONMap(path)
	if err != nil {
		return err
	}
	servers, _ := m["mcp"].(map[string]any)
	if servers == nil {
		return fmt.Errorf("no mcp key in %s", path)
	}
	sshmng, _ := servers["sshmng"].(map[string]any)
	if sshmng == nil {
		return fmt.Errorf("no sshmng entry in mcp")
	}
	cmdArr, ok := sshmng["command"].([]any)
	if !ok {
		return fmt.Errorf("sshmng.command is not an array (got %T)", sshmng["command"])
	}
	if len(cmdArr) == 0 {
		return fmt.Errorf("sshmng.command array is empty")
	}
	first, _ := cmdArr[0].(string)
	if first != expectedBinary {
		return fmt.Errorf("stale: expected command[0] %q, got %q", expectedBinary, first)
	}
	return nil
}
```

- [ ] **Step 4: Run tests and verify**

Run: `go test -race ./internal/cli/`
Expected: PASS — all OpenCodeInjector tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/agent_opencode.go internal/cli/agent_opencode_test.go
git commit -m "$(cat <<'EOF'
feat(cli): OpenCodeInjector for ~/.config/opencode/opencode.json

OpenCode schema differs from Claude Code / Hermes: command is an array
(binary + args combined), env field is "environment" (not "env"),
requires type: "local" and enabled: true.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: File Scaffolding (files.go)

Implement `ScaffoldHome(home string, opts ScaffoldOpts) error` — creates `~/.sshmng/` (0700), `config.json` (0600, empty skeleton), `config.example.json` (0600, Pattern A/B examples). Idempotent: `config.json` left alone if exists, `config.example.json` always overwritten.

**Files:**
- Create: `internal/cli/files.go`
- Create: `internal/cli/files_test.go`

**Interfaces:**
- Produces: `ScaffoldOpts{OverwriteConfig bool}` (false = skip if exists)
- Produces: `ScaffoldHome(home string, opts ScaffoldOpts) error`
- Produces: `ConfigJSONSkeleton`, `ConfigExampleJSON` constants (raw JSON strings)
- Consumes: `os.MkdirAll`, `os.WriteFile`, `os.Chmod`, `runtime.GOOS`

- [ ] **Step 1: Write failing test for ScaffoldHome**

Create `internal/cli/files_test.go`:

```go
package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sshmng/internal/config"
)

func TestScaffoldHomeCreatesDirAndFiles(t *testing.T) {
	home := t.TempDir()
	err := ScaffoldHome(home, ScaffoldOpts{})
	if err != nil {
		t.Fatalf("ScaffoldHome: %v", err)
	}
	// Directory exists with 0700 (Unix)
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Error("home is not a directory")
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0700 {
			t.Errorf("home perm = %o, want 0700", perm)
		}
	}
	// config.json exists
	cfgPath := filepath.Join(home, "config.json")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("config.json missing: %v", err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(cfgPath); err == nil {
			if perm := info.Mode().Perm(); perm != 0600 {
				t.Errorf("config.json perm = %o, want 0600", perm)
			}
		}
	}
	// config.example.json exists
	exPath := filepath.Join(home, "config.example.json")
	if _, err := os.Stat(exPath); err != nil {
		t.Errorf("config.example.json missing: %v", err)
	}
}

func TestScaffoldHomeConfigJSONLoadsViaStore(t *testing.T) {
	home := t.TempDir()
	if err := ScaffoldHome(home, ScaffoldOpts{}); err != nil {
		t.Fatalf("ScaffoldHome: %v", err)
	}
	store := config.NewStore(filepath.Join(home, "config.json"))
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("Load config.json: %v", err)
	}
	if cfg.Version != "1" {
		t.Errorf("version = %q, want '1'", cfg.Version)
	}
	if len(cfg.Servers) != 0 {
		t.Errorf("expected 0 servers, got %d", len(cfg.Servers))
	}
}

func TestScaffoldHomeConfigExampleJSONLoadsViaStore(t *testing.T) {
	home := t.TempDir()
	if err := ScaffoldHome(home, ScaffoldOpts{}); err != nil {
		t.Fatalf("ScaffoldHome: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Example file must be valid JSON loadable by config.Store
	tmpCfg := filepath.Join(home, "test_load.json")
	if err := os.WriteFile(tmpCfg, data, 0600); err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(tmpCfg)
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("example config invalid: %v", err)
	}
	if len(cfg.Servers) < 4 {
		t.Errorf("expected >=4 example servers, got %d", len(cfg.Servers))
	}
	if len(cfg.Proxies) < 2 {
		t.Errorf("expected >=2 example proxies, got %d", len(cfg.Proxies))
	}
	if len(cfg.Jumphosts) < 2 {
		t.Errorf("expected >=2 example jumphosts, got %d", len(cfg.Jumphosts))
	}
}

func TestScaffoldHomePreservesExistingConfigJSON(t *testing.T) {
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.json")
	original := `{"version":"1","idle_timeout_s":600,"jumphosts":[],"proxies":[],"servers":[]}`
	if err := os.WriteFile(cfgPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ScaffoldHome(home, ScaffoldOpts{}); err != nil {
		t.Fatalf("ScaffoldHome: %v", err)
	}
	data, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(data), `"idle_timeout_s": 600`) {
		t.Errorf("existing config.json was modified:\n%s", string(data))
	}
}

func TestScaffoldHomeOverwritesConfigExampleJSON(t *testing.T) {
	home := t.TempDir()
	exPath := filepath.Join(home, "config.example.json")
	original := `{"old": true}`
	if err := os.WriteFile(exPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ScaffoldHome(home, ScaffoldOpts{}); err != nil {
		t.Fatalf("ScaffoldHome: %v", err)
	}
	data, _ := os.ReadFile(exPath)
	if strings.Contains(string(data), `"old": true`) {
		t.Errorf("config.example.json was not overwritten:\n%s", string(data))
	}
	if !strings.Contains(string(data), "example-server-direct-key") {
		t.Errorf("config.example.json missing expected content:\n%s", string(data))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/cli/`
Expected: FAIL — `ScaffoldHome`, `ScaffoldOpts` undefined.

- [ ] **Step 3: Write files.go**

Create `internal/cli/files.go`:

```go
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ScaffoldOpts controls ScaffoldHome behavior.
type ScaffoldOpts struct {
	// OverwriteConfig if true overwrites existing config.json. Default false
	// (preserve user data).
	OverwriteConfig bool
}

// ScaffoldHome creates the sshmng home directory and config files.
//   - Creates <home>/ (0700 on Unix)
//   - Writes <home>/config.json (0600, empty skeleton) — skipped if exists
//     and OverwriteConfig is false
//   - Writes <home>/config.example.json (0600, examples) — always overwritten
func ScaffoldHome(home string, opts ScaffoldOpts) error {
	if err := os.MkdirAll(home, 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", home, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(home, 0700); err != nil {
			return fmt.Errorf("chmod %s: %w", home, err)
		}
	}

	cfgPath := filepath.Join(home, "config.json")
	if opts.OverwriteConfig {
		if err := writeSecureFile(cfgPath, []byte(configJSONSkeleton)); err != nil {
			return err
		}
	} else if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := writeSecureFile(cfgPath, []byte(configJSONSkeleton)); err != nil {
			return err
		}
	}

	exPath := filepath.Join(home, "config.example.json")
	if err := writeSecureFile(exPath, []byte(configExampleJSON)); err != nil {
		return err
	}
	return nil
}

// writeSecureFile writes data to path with 0600 perms (Unix) or default
// (Windows). Truncates existing files.
func writeSecureFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0600); err != nil {
			return fmt.Errorf("chmod %s: %w", path, err)
		}
	}
	return nil
}

const configJSONSkeleton = `{
  "version": "1",
  "idle_timeout_s": 300,
  "jumphosts": [],
  "proxies": [],
  "servers": []
}
`

const configExampleJSON = `{
  "version": "1",
  "idle_timeout_s": 300,
  "log_level": "info",
  "log_path": "",
  "proxies": [
    {
      "name": "example-socks5",
      "type": "SOCKS5",
      "addr": "socks.corp:1080",
      "tags": ["example"]
    },
    {
      "name": "example-socks5-auth",
      "type": "SOCKS5",
      "addr": "socks.corp:1080",
      "auth": {"user": "proxy-user", "password": "<replace-me>"},
      "tags": ["example", "auth"]
    },
    {
      "name": "example-http-auth",
      "type": "HTTP",
      "addr": "proxy.corp:8080",
      "auth": {"user": "proxy-user", "password": "<replace-me>"},
      "tags": ["example", "auth"]
    }
  ],
  "jumphosts": [
    {
      "name": "example-jumphost-a",
      "addr": "10.0.0.254:22",
      "user": "ops",
      "auth": {"password": "<replace-me>"},
      "ssh_j": true,
      "tags": ["example", "pattern-a"]
    },
    {
      "name": "example-jumphost-a-via-proxy",
      "addr": "10.0.0.254:22",
      "user": "ops",
      "auth": {"password": "<replace-me>"},
      "ssh_j": true,
      "proxy": "example-socks5-auth",
      "tags": ["example", "pattern-a", "via-proxy"]
    },
    {
      "name": "example-jumphost-b",
      "addr": "10.0.0.254:22",
      "user": "ops",
      "auth": {"password": "<replace-me>"},
      "ssh_j": false,
      "login_flow": {
        "wait_menu": {
          "expects": [{"pattern": "Your choice:", "next": "success"}]
        }
      },
      "login_entry": "wait_menu",
      "tags": ["example", "pattern-b"]
    }
  ],
  "servers": [
    {
      "name": "example-server-a",
      "addr": "10.0.0.1:22",
      "user": "deploy",
      "auth": {"password": "<replace-me>"},
      "via": "example-jumphost-a",
      "tags": ["example", "pattern-a"]
    },
    {
      "name": "example-server-a-via-proxy",
      "addr": "10.0.0.1:22",
      "user": "deploy",
      "auth": {"password": "<replace-me>"},
      "via": "example-jumphost-a-via-proxy",
      "tags": ["example", "pattern-a", "via-proxy"]
    },
    {
      "name": "example-server-b",
      "addr": "10.0.0.1:22",
      "user": "deploy",
      "auth": null,
      "via": "example-jumphost-b",
      "login_flow": {
        "select_target": {"send": "1\r", "expects": [{"pattern": "Password:", "next": "input_pass"}]},
        "input_pass": {"send": "<replace-me>\r", "expects": [{"pattern": "$ ", "next": "success"}]}
      },
      "login_entry": "select_target",
      "tags": ["example", "pattern-b"]
    },
    {
      "name": "example-server-direct-password",
      "addr": "10.0.0.2:22",
      "user": "deploy",
      "auth": {"password": "<replace-me>"},
      "login_flow": {
        "wait_ps1": {
          "send": "",
          "expects": [{"pattern": "re:.*]# ", "next": "success"}]
        }
      },
      "login_entry": "wait_ps1",
      "tags": ["example", "direct", "login-flow"]
    },
    {
      "name": "example-server-direct-key",
      "addr": "10.0.0.3:22",
      "user": "deploy",
      "auth": {
        "private_key": "/home/user/.ssh/deploy_key",
        "passphrase": "<replace-me>"
      },
      "tags": ["example", "direct", "private-key"]
    }
  ]
}
`
```

- [ ] **Step 4: Run tests and verify**

Run: `go test -race ./internal/cli/`
Expected: PASS — all ScaffoldHome tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/files.go internal/cli/files_test.go
git commit -m "$(cat <<'EOF'
feat(cli): ScaffoldHome creates ~/.sshmng + config templates

Creates home dir (0700), config.json (0600, empty skeleton), and
config.example.json (0600, Pattern A/B examples including direct
server with LoginFlow waiting for PS1). Idempotent: config.json
preserved if exists, example always overwritten.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: install Command

Implement `sshmng install` — interactive wizard that scaffolds home, detects Agents, injects MCP config, runs doctor at end. Flags: `--home`, `--binary`, `--agents`, `--yes`, `--skip-files`, `--skip-agents`. Add `case "install"` to Dispatch.

**Files:**
- Create: `internal/cli/install.go`
- Create: `internal/cli/prompt.go`
- Create: `internal/cli/install_test.go`
- Create: `internal/cli/prompt_test.go`
- Modify: `internal/cli/cli.go` (add `case "install"`)

**Interfaces:**
- Produces: `runInstall(ctx, args, out) int`
- Produces: `InstallOpts{Home, Binary, Agents, Yes, SkipFiles, SkipAgents}`
- Produces: `RunInstall(opts InstallOpts, out io.Writer) int` (testable entry)
- Produces: prompt helpers `promptString(label, default string)`, `promptConfirm(label string, default bool)`, `promptMultiSelect(label string, options []SelectOption) []int`
- Consumes: `ScaffoldHome` (Task 5), `AgentInjector` impls (Tasks 2-4), `os.Executable`, `os.UserHomeDir`

- [ ] **Step 1: Write failing test for prompt helpers**

Create `internal/cli/prompt_test.go`:

```go
package cli

import (
	"bufio"
	"strings"
	"testing"
)

func TestPromptStringReturnsDefaultOnEmpty(t *testing.T) {
	r := strings.NewReader("\n")
	got, err := promptStringReader(bufio.NewReader(r), "Label", "/default/path")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/default/path" {
		t.Errorf("got %q, want %q", got, "/default/path")
	}
}

func TestPromptStringReturnsInput(t *testing.T) {
	r := strings.NewReader("/custom/path\n")
	got, _ := promptStringReader(bufio.NewReader(r), "Label", "/default/path")
	if got != "/custom/path" {
		t.Errorf("got %q, want %q", got, "/custom/path")
	}
}

func TestPromptConfirmDefaultsNo(t *testing.T) {
	r := strings.NewReader("\n")
	got, _ := promptConfirmReader(bufio.NewReader(r), "Confirm?", false)
	if got != false {
		t.Errorf("got %v, want false", got)
	}
}

func TestPromptConfirmYes(t *testing.T) {
	r := strings.NewReader("y\n")
	got, _ := promptConfirmReader(bufio.NewReader(r), "Confirm?", false)
	if got != true {
		t.Errorf("got %v, want true", got)
	}
}

func TestPromptConfirmYesFull(t *testing.T) {
	r := strings.NewReader("yes\n")
	got, _ := promptConfirmReader(bufio.NewReader(r), "Confirm?", false)
	if got != true {
		t.Errorf("got %v, want true", got)
	}
}

func TestParseAgentsFlag(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"none", nil},
		{"claude-code", []string{"claude-code"}},
		{"claude-code,hermes", []string{"claude-code", "hermes"}},
		{"claude-code, hermes , opencode", []string{"claude-code", "hermes", "opencode"}},
		{"", nil},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := parseAgentsFlag(tt.in)
			if len(got) != len(tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race ./internal/cli/`
Expected: FAIL — `promptStringReader`, `promptConfirmReader`, `parseAgentsFlag` undefined.

- [ ] **Step 3: Write prompt.go**

Create `internal/cli/prompt.go`:

```go
package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// promptStringReader reads a line from r, returns input or def if empty.
// Writer w is for prompts (os.Stdout in production). For testability,
// pass bufio.Reader directly.
func promptStringReader(r *bufio.Reader, label, def string) (string, error) {
	for {
		if def != "" {
			fmt.Printf("%s [%s]: ", label, def)
		} else {
			fmt.Printf("%s: ", label)
		}
		line, err := r.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return def, nil
		}
		return line, nil
	}
}

// promptConfirmReader reads a line, returns true for y/yes, false for n/no/empty.
func promptConfirmReader(r *bufio.Reader, label string, def bool) (bool, error) {
	defStr := "y/N"
	if def {
		defStr = "Y/n"
	}
	fmt.Printf("%s [%s]: ", label, defStr)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	line = strings.ToLower(strings.TrimSpace(line))
	switch line {
	case "y", "yes":
		return true, nil
	case "n", "no", "":
		return def, nil
	default:
		return false, fmt.Errorf("invalid response %q (expected y/n)", line)
	}
}

// parseAgentsFlag parses --agents value into a list of Agent names.
// "none" or "" -> nil (skip Agent injection). Comma-separated otherwise.
// Whitespace around items is trimmed.
func parseAgentsFlag(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "none" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
```

- [ ] **Step 4: Run prompt tests and verify**

Run: `go test -race -run 'TestPrompt|TestParseAgents' ./internal/cli/`
Expected: PASS.

- [ ] **Step 5: Write failing test for RunInstall**

Create `internal/cli/install_test.go`:

```go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setupInstallTest creates a temp HOME and returns (home, claudePath).
func setupInstallTest(t *testing.T) (home string, claudePath string) {
	t.Helper()
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	home = filepath.Join(tmp, ".sshmng")
	claudePath = filepath.Join(tmp, ".claude.json")
	return home, claudePath
}

func TestRunInstallCreatesFiles(t *testing.T) {
	home, _ := setupInstallTest(t)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := RunInstall(InstallOpts{
		Home:        home,
		Binary:      bin,
		Agents:      nil, // none
		Yes:         true,
		SkipAgents:  true,
	}, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0. Output:\n%s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); err != nil {
		t.Errorf("config.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.example.json")); err != nil {
		t.Errorf("config.example.json missing: %v", err)
	}
}

func TestRunInstallInjectsClaudeCode(t *testing.T) {
	home, claudePath := setupInstallTest(t)
	// Pre-create claude.json so Detect finds it
	if err := os.WriteFile(claudePath, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	bin, _ := os.Executable()
	var out bytes.Buffer
	code := RunInstall(InstallOpts{
		Home:       home,
		Binary:     bin,
		Agents:     []string{"claude-code"},
		Yes:        true,
		SkipFiles:  false,
		SkipAgents: false,
	}, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0. Output:\n%s", code, out.String())
	}
	data, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"sshmng"`) {
		t.Errorf("sshmng entry missing:\n%s", s)
	}
	if !strings.Contains(s, bin) {
		t.Errorf("binary path missing:\n%s", s)
	}
}

func TestRunInstallPreservesExistingConfigJSON(t *testing.T) {
	home, _ := setupInstallTest(t)
	original := `{"version":"1","idle_timeout_s":600,"jumphosts":[],"proxies":[],"servers":[]}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(original), 0600); err != nil {
		// Need to mkdir first
		if err := os.MkdirAll(home, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
	}
	bin, _ := os.Executable()
	var out bytes.Buffer
	code := RunInstall(InstallOpts{
		Home:       home,
		Binary:     bin,
		Agents:     nil,
		Yes:        true,
		SkipAgents: true,
	}, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	data, _ := os.ReadFile(filepath.Join(home, "config.json"))
	if !strings.Contains(string(data), `"idle_timeout_s": 600`) {
		t.Errorf("existing config.json was modified:\n%s", string(data))
	}
}

func TestRunInstallCreatesBackupBeforeInject(t *testing.T) {
	home, claudePath := setupInstallTest(t)
	original := `{"mcpServers":{"other":{"command":"x"}}}`
	if err := os.WriteFile(claudePath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	bin, _ := os.Executable()
	var out bytes.Buffer
	code := RunInstall(InstallOpts{
		Home:       home,
		Binary:     bin,
		Agents:     []string{"claude-code"},
		Yes:        true,
	}, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	entries, err := os.ReadDir(filepath.Dir(claudePath))
	if err != nil {
		t.Fatal(err)
	}
	backupCount := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".claude.json.bak.") {
			backupCount++
		}
	}
	if backupCount != 1 {
		t.Errorf("expected 1 backup, got %d", backupCount)
	}
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `go test -race -run TestRunInstall ./internal/cli/`
Expected: FAIL — `RunInstall`, `InstallOpts` undefined.

- [ ] **Step 7: Write install.go**

Create `internal/cli/install.go`:

```go
package cli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// InstallOpts configures runInstall.
type InstallOpts struct {
	Home       string   // sshmng home dir (default ~/.sshmng or $SSHMNG_HOME)
	Binary     string   // sshmng binary path (default os.Executable())
	Agents     []string // Agent names to inject; nil = auto-detect; empty slice = skip
	Yes        bool     // non-interactive, use defaults
	SkipFiles  bool     // skip ~/.sshmng/ creation
	SkipAgents bool     // skip Agent injection
}

// RunInstall runs the install wizard. Returns process exit code.
// out is where progress messages are written (os.Stdout in production).
func RunInstall(opts InstallOpts, out io.Writer) int {
	stdoutBackup := os.Stdout
	r := bufio.NewReader(os.Stdin)
	defer func() { os.Stdout = stdoutBackup }()
	os.Stdout = out
	defer func() { os.Stdout = stdoutBackup }()

	// Default home
	if opts.Home == "" {
		opts.Home = defaultHome()
	}
	// Default binary
	if opts.Binary == "" {
		bin, err := os.Executable()
		if err != nil {
			fmt.Fprintf(out, "Error: cannot determine sshmng binary path: %v\n", err)
			return 1
		}
		opts.Binary = bin
	}

	// Resolve injectors
	allInjectors := []AgentInjector{
		&ClaudeCodeInjector{},
		&HermesInjector{},
		&OpenCodeInjector{},
	}
	var injectors []AgentInjector
	if opts.SkipAgents || len(opts.Agents) == 0 && opts.Agents != nil {
		// SkipAgents=true OR Agents=empty slice (from "none")
		// (note: nil means auto-detect, empty slice from parseAgentsFlag("none") means skip)
	} else if len(opts.Agents) > 0 {
		// Explicit list
		for _, name := range opts.Agents {
			for _, inj := range allInjectors {
				if inj.Name() == name {
					injectors = append(injectors, inj)
					break
				}
			}
		}
	}

	// Interactive: prompt for missing values
	if !opts.Yes {
		if opts.Home == "" {
			opts.Home = promptStringReader(r, "sshmng home directory", defaultHome())
		}
		if opts.Binary == "" {
			bin, _ := os.Executable()
			opts.Binary = promptStringReader(r, "sshmng binary path", bin)
		}
		if !opts.SkipAgents && injectors == nil {
			injectors = promptAgentSelection(r, allInjectors)
		}
		fmt.Println()
		fmt.Println("Review:")
		if !opts.SkipFiles {
			fmt.Printf("  + %s/                      (dir, 0700)\n", opts.Home)
			fmt.Printf("  + %s/config.json           (0600, empty skeleton)\n", opts.Home)
			fmt.Printf("  + %s/config.example.json   (0600, examples)\n", opts.Home)
		}
		for _, inj := range injectors {
			path, _ := inj.Detect()
			fmt.Printf("  ~ %s                  (merge sshmng entry, backup -> .bak.<ts>)\n", path)
		}
		confirmed, err := promptConfirmReader(r, "Proceed?", false)
		if err != nil || !confirmed {
			fmt.Println("Aborted.")
			return 0
		}
	}

	// Execute
	fmt.Fprintln(out, "Executing:")
	if !opts.SkipFiles {
		if err := ScaffoldHome(opts.Home, ScaffoldOpts{}); err != nil {
			fmt.Fprintf(out, "  [FAIL] ScaffoldHome: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "  [ok] Created %s (0700)\n", opts.Home)
		fmt.Fprintf(out, "  [ok] Wrote %s/config.json (0600)\n", opts.Home)
		fmt.Fprintf(out, "  [ok] Wrote %s/config.example.json (0600)\n", opts.Home)
	}

	entry := MCPEntry{
		BinaryPath: opts.Binary,
		Args:       []string{"mcp"},
		Env:        map[string]string{"SSHMNG_HOME": opts.Home},
	}
	for _, inj := range injectors {
		path, installed := inj.Detect()
		if !installed {
			// Allow injection to create the file (e.g., user passed --agents
			// for an Agent whose config doesn't exist yet)
			path = injectorPath(inj)
		}
		if err := inj.Inject(path, entry); err != nil {
			fmt.Fprintf(out, "  [FAIL] %s: %v\n", inj.DisplayName(), err)
			return 1
		}
		fmt.Fprintf(out, "  [ok] Injected sshmng into %s\n", path)
	}

	// Run doctor at end
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Verifying (doctor):")
	docCode := RunDoctor(DoctorOpts{
		Home:           opts.Home,
		ExpectedBinary: opts.Binary,
		AgentFilter:    nil,
	}, out)
	if docCode != 0 {
		fmt.Fprintf(out, "\nSetup completed with warnings/errors. Run 'sshmng doctor' for details.\n")
		return 1
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Setup complete!")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next steps:")
	fmt.Fprintln(out, "  1. Restart your Agent to load the new MCP config")
	fmt.Fprintln(out, "  2. Ask Agent: \"list_ssh_servers\"")
	fmt.Fprintln(out, "  3. Add servers by asking Agent \"add an SSH server named ...\"")
	fmt.Fprintf(out, "     Or manually edit %s/config.json (see config.example.json for examples)\n", opts.Home)
	return 0
}

// promptAgentSelection shows detected Agents, lets user toggle selection.
// Returns selected injectors. If user skips, returns nil.
func promptAgentSelection(r *bufio.Reader, all []AgentInjector) []AgentInjector {
	type item struct {
		inj      AgentInjector
		selected bool
	}
	items := make([]item, 0, len(all))
	for _, inj := range all {
		_, installed := inj.Detect()
		items = append(items, item{inj: inj, selected: installed})
	}
	for {
		fmt.Println()
		fmt.Println("Detected Agents:")
		for i, it := range items {
			mark := " "
			if it.selected {
				mark = "*"
			}
			path, _ := it.inj.Detect()
			if path == "" {
				path = "(not installed)"
			}
			fmt.Printf("  [%s] %d. %s    (%s)\n", mark, i+1, it.inj.DisplayName(), path)
		}
		fmt.Print("Toggle (1-N), 's' to skip, enter to confirm: ")
		line, err := r.ReadString('\n')
		if err != nil {
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			var out []AgentInjector
			for _, it := range items {
				if it.selected {
					out = append(out, it.inj)
				}
			}
			return out
		}
		if line == "s" || line == "S" {
			return nil
		}
		// Toggle by number
		var n int
		_, err = fmt.Sscanf(line, "%d", &n)
		if err != nil || n < 1 || n > len(items) {
			fmt.Printf("Invalid input %q\n", line)
			continue
		}
		items[n-1].selected = !items[n-1].selected
	}
}

// injectorPath returns the config file path for an injector, even if not
// installed (for creating new configs).
func injectorPath(inj AgentInjector) string {
	switch i := inj.(type) {
	case *ClaudeCodeInjector:
		home, _ := os.UserHomeDir()
		return home + "/.claude.json"
	case *HermesInjector:
		return i.configPath()
	case *OpenCodeInjector:
		home, _ := os.UserHomeDir()
		return home + "/.config/opencode/opencode.json"
	}
	return ""
}

// defaultHome returns $SSHMNG_HOME or ~/.sshmng.
func defaultHome() string {
	if h := os.Getenv("SSHMNG_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".sshmng"
	}
	return home + "/.sshmng"
}

// runInstall is the Dispatch entry point for 'sshmng install'.
func runInstall(ctx context.Context, args []string, out io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(out)
	home := fs.String("home", "", "sshmng config directory (default $SSHMNG_HOME or ~/.sshmng)")
	binary := fs.String("binary", "", "sshmng binary path (default: auto-detect)")
	agents := fs.String("agents", "", "comma-separated Agent names (claude-code,hermes,opencode); 'none' to skip")
	yes := fs.Bool("yes", false, "non-interactive, use defaults")
	skipFiles := fs.Bool("skip-files", false, "skip ~/.sshmng/ creation")
	skipAgents := fs.Bool("skip-agents", false, "skip Agent injection")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	opts := InstallOpts{
		Home:       *home,
		Binary:     *binary,
		Agents:     parseAgentsFlag(*agents),
		Yes:        *yes,
		SkipFiles:  *skipFiles,
		SkipAgents: *skipAgents,
	}
	// parseAgentsFlag returns nil for "none"/"". We need to distinguish "none"
	// (explicit skip) from "" (auto-detect). Use a sentinel.
	if *agents == "none" {
		opts.SkipAgents = true
	}
	return RunInstall(opts, out)
}
```

**Important note on `RunInstall` stdout redirect:** The above code redirects `os.Stdout` to `out` so that `fmt.Println` (used by prompt helpers) writes to the test buffer. This is messy. A cleaner approach is to pass `io.Writer` through all prompt functions. However, for simplicity and to keep prompt.go small, the redirect approach works. If the implementer prefers, they may refactor prompt helpers to take `io.Writer` as the first argument — this is a judgment call. The tests will pass either way as long as `RunInstall(opts, out)` ends up with all output in `out`.

Add `"context"` to imports of install.go.

- [ ] **Step 8: Add `case "install"` to Dispatch in cli.go**

Modify `internal/cli/cli.go`, add to the switch in `Dispatch`:

```go
	case "install":
		return runInstall(ctx, args[1:], out)
```

- [ ] **Step 9: Run install tests and verify**

Run: `go test -race ./internal/cli/`
Expected: PASS — all install tests pass (may need iteration on stdout redirect).

If doctor isn't implemented yet, `RunDoctor` call will fail to compile. In that case, comment out the `RunDoctor` call temporarily with a TODO and uncomment in Task 7. **Or**, better: stub `RunDoctor` in doctor.go now (just `return 0`), and replace with real impl in Task 7.

Create minimal stub `internal/cli/doctor.go`:

```go
package cli

import "io"

// DoctorOpts will be expanded in Task 7.
type DoctorOpts struct {
	Home           string
	ExpectedBinary string
	AgentFilter    []string
}

// RunDoctor is a stub; replaced in Task 7.
func RunDoctor(opts DoctorOpts, out io.Writer) int {
	return 0
}
```

- [ ] **Step 10: Commit**

```bash
git add internal/cli/install.go internal/cli/prompt.go internal/cli/install_test.go internal/cli/prompt_test.go internal/cli/doctor.go internal/cli/cli.go
git commit -m "$(cat <<'EOF'
feat(cli): sshmng install subcommand

Interactive wizard scaffolds ~/.sshmng/, detects Agents, injects MCP
config, runs doctor at end. Flags: --home, --binary, --agents,
--yes, --skip-files, --skip-agents. Doctor stub added (Task 7 fills
in). Dispatch routes 'install' to runInstall.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: doctor Command

Implement `sshmng doctor` — verifies home dir perms, config.json loadability, Agent config entries. Three-state exit (0 pass / 1 fail / 2 warn-only). Flags: `--agent <name>`. Add `case "doctor"` to Dispatch. Replace the stub from Task 6.

**Files:**
- Modify: `internal/cli/doctor.go` (replace stub)
- Create: `internal/cli/doctor_test.go`
- Modify: `internal/cli/cli.go` (add `case "doctor"`)

**Interfaces:**
- Produces: `RunDoctor(opts DoctorOpts, out io.Writer) int` (real impl)
- Produces: `runDoctor(ctx, args, out) int` (Dispatch entry, parses flags)
- Consumes: `config.NewStore`, `config.Store.Load`, `AgentInjector.Detect/Verify`, `os.Stat`, `runtime.GOOS`

- [ ] **Step 1: Write failing test for RunDoctor**

Create `internal/cli/doctor_test.go`:

```go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDoctorEmptyHomeFails(t *testing.T) {
	// Point HOME to a temp dir without ~/.sshmng
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	home := filepath.Join(tmp, ".sshmng")
	bin, _ := os.Executable()
	var out bytes.Buffer
	code := RunDoctor(DoctorOpts{Home: home, ExpectedBinary: bin}, &out)
	if code != 1 {
		t.Errorf("code = %d, want 1 (fail). Output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "FAIL") {
		t.Errorf("expected FAIL in output:\n%s", out.String())
	}
}

func TestDoctorPassesAfterInstall(t *testing.T) {
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	home := filepath.Join(tmp, ".sshmng")
	bin, _ := os.Executable()
	// Run install first
	var installOut bytes.Buffer
	code := RunInstall(InstallOpts{
		Home:       home,
		Binary:     bin,
		Yes:        true,
		SkipAgents: true,
	}, &installOut)
	if code != 0 {
		t.Fatalf("install failed: %s", installOut.String())
	}
	var out bytes.Buffer
	code = RunDoctor(DoctorOpts{Home: home, ExpectedBinary: bin}, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0. Output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "Summary:") {
		t.Errorf("missing Summary line:\n%s", out.String())
	}
}

func TestDoctorWarnsOnMissingExample(t *testing.T) {
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	home := filepath.Join(tmp, ".sshmng")
	bin, _ := os.Executable()
	// Install then delete example
	var installOut bytes.Buffer
	RunInstall(InstallOpts{Home: home, Binary: bin, Yes: true, SkipAgents: true}, &installOut)
	os.Remove(filepath.Join(home, "config.example.json"))

	var out bytes.Buffer
	code := RunDoctor(DoctorOpts{Home: home, ExpectedBinary: bin}, &out)
	if code != 2 {
		t.Errorf("code = %d, want 2 (warn-only). Output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "WARN") {
		t.Errorf("expected WARN in output:\n%s", out.String())
	}
}

func TestDoctorDetectsStaleAgentEntry(t *testing.T) {
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	home := filepath.Join(tmp, ".sshmng")
	claudePath := filepath.Join(tmp, ".claude.json")
	// Install with binary X
	bin, _ := os.Executable()
	var installOut bytes.Buffer
	RunInstall(InstallOpts{
		Home:       home,
		Binary:     bin,
		Agents:     []string{"claude-code"},
		Yes:        true,
	}, &installOut)
	// Pre-create claude.json so install finds it
	os.WriteFile(claudePath, []byte(`{"mcpServers":{}}`), 0600)
	RunInstall(InstallOpts{
		Home:       home,
		Binary:     bin,
		Agents:     []string{"claude-code"},
		Yes:        true,
	}, &installOut)

	// Now run doctor with a different expected binary — should FAIL
	var out bytes.Buffer
	code := RunDoctor(DoctorOpts{Home: home, ExpectedBinary: "/different/bin/sshmng"}, &out)
	if code != 1 {
		t.Errorf("code = %d, want 1 (stale fail). Output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "stale") {
		t.Errorf("expected 'stale' in output:\n%s", out.String())
	}
}

func TestDoctorSkipsUninstalledAgents(t *testing.T) {
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	home := filepath.Join(tmp, ".sshmng")
	bin, _ := os.Executable()
	var installOut bytes.Buffer
	RunInstall(InstallOpts{Home: home, Binary: bin, Yes: true, SkipAgents: true}, &installOut)
	var out bytes.Buffer
	code := RunDoctor(DoctorOpts{Home: home, ExpectedBinary: bin}, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0. Output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "SKIP") {
		t.Errorf("expected SKIP for uninstalled Agents:\n%s", out.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -race -run TestDoctor ./internal/cli/`
Expected: FAIL — stub returns 0, doesn't print anything.

- [ ] **Step 3: Write doctor.go (replace stub)**

Replace `internal/cli/doctor.go` content with:

```go
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"sshmng/internal/config"
)

// DoctorOpts configures RunDoctor.
type DoctorOpts struct {
	Home           string   // sshmng home dir
	ExpectedBinary string   // expected sshmng binary path in Agent configs
	AgentFilter    []string // restrict to specific Agents; nil = all
}

// RunDoctor verifies setup and writes results to out. Returns exit code:
//   - 0: all checks pass
//   - 1: at least one FAIL
//   - 2: WARN-only (no FAIL)
func RunDoctor(opts DoctorOpts, out io.Writer) int {
	if opts.Home == "" {
		opts.Home = defaultHome()
	}
	if opts.ExpectedBinary == "" {
		bin, _ := os.Executable()
		opts.ExpectedBinary = bin
	}
	failCount, warnCount, passCount := 0, 0, 0
	print := func(level, msg string) {
		fmt.Fprintf(out, "  [%s]  %s\n", level, msg)
		if level == "FAIL" {
			failCount++
		} else if level == "WARN" {
			warnCount++
		} else if level == "OK" {
			passCount++
		}
	}

	fmt.Fprintln(out, "sshmng doctor - verifying setup")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Home:")

	// Home dir
	info, err := os.Stat(opts.Home)
	if err != nil {
		print("FAIL", fmt.Sprintf("%s missing - run 'sshmng install' to create", opts.Home))
	} else if !info.IsDir() {
		print("FAIL", fmt.Sprintf("%s is not a directory", opts.Home))
	} else {
		if runtime.GOOS != "windows" {
			if perm := info.Mode().Perm(); perm != 0700 {
				print("FAIL", fmt.Sprintf("%s perm %o, want 0700 (chmod 700 %s)", opts.Home, perm, opts.Home))
			} else {
				print("OK", fmt.Sprintf("%s exists, 0700", opts.Home))
			}
		} else {
			print("WARN", fmt.Sprintf("%s exists; manually restrict NTFS ACL (Properties -> Security)", opts.Home))
		}
	}

	// config.json
	cfgPath := filepath.Join(opts.Home, "config.json")
	if _, err := os.Stat(cfgPath); err != nil {
		print("FAIL", fmt.Sprintf("%s missing - run 'sshmng install'", cfgPath))
	} else {
		store := config.NewStore(cfgPath)
		if _, err := store.Load(); err != nil {
			print("FAIL", fmt.Sprintf("config.json invalid: %v", err))
		} else {
			if runtime.GOOS != "windows" {
				if info, err := os.Stat(cfgPath); err == nil {
					if perm := info.Mode().Perm(); perm != 0600 {
						print("FAIL", fmt.Sprintf("config.json perm %o, want 0600 (chmod 600 %s)", perm, cfgPath))
					} else {
						print("OK", fmt.Sprintf("%s exists, 0600, loads OK", cfgPath))
					}
				}
			} else {
				print("OK", fmt.Sprintf("%s exists, loads OK", cfgPath))
			}
		}
	}

	// config.example.json (WARN-only)
	exPath := filepath.Join(opts.Home, "config.example.json")
	if _, err := os.Stat(exPath); err != nil {
		print("WARN", fmt.Sprintf("%s missing (optional, run 'sshmng install' to regenerate)", exPath))
	} else {
		print("OK", fmt.Sprintf("%s exists", exPath))
	}

	// binary
	if _, err := os.Stat(opts.ExpectedBinary); err != nil {
		print("FAIL", fmt.Sprintf("binary %s not executable - rebuild with 'go build'", opts.ExpectedBinary))
	} else {
		print("OK", fmt.Sprintf("binary at %s", opts.ExpectedBinary))
	}

	// known_hosts (if exists)
	khPath := filepath.Join(opts.Home, "known_hosts")
	if info, err := os.Stat(khPath); err == nil {
		if runtime.GOOS != "windows" {
			if perm := info.Mode().Perm(); perm != 0600 {
				print("FAIL", fmt.Sprintf("known_hosts perm %o, want 0600 (chmod 600 %s)", perm, khPath))
			} else {
				print("OK", "known_hosts: 0600")
			}
		} else {
			print("OK", "known_hosts exists")
		}
	}
	// If known_hosts doesn't exist, that's fine - will be created on first connection.

	// Agents
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Agents:")
	allInjectors := []AgentInjector{
		&ClaudeCodeInjector{},
		&HermesInjector{},
		&OpenCodeInjector{},
	}
	for _, inj := range allInjectors {
		if len(opts.AgentFilter) > 0 && !containsString(opts.AgentFilter, inj.Name()) {
			continue
		}
		path, installed := inj.Detect()
		// Detect() returns the expected path even when installed=false, so we
		// can display it in the SKIP message.
		fmt.Fprintf(out, "  %s (%s)\n", inj.DisplayName(), path)
		if !installed {
			fmt.Fprintf(out, "    [SKIP]  not detected (install %s or pass --agent %s to force)\n",
				inj.DisplayName(), inj.Name())
			continue
		}
		if err := inj.Verify(path, opts.ExpectedBinary); err != nil {
			print("FAIL", fmt.Sprintf("%s: %v", inj.DisplayName(), err))
		} else {
			print("OK", fmt.Sprintf("%s config has sshmng entry, command matches", inj.DisplayName()))
		}
	}

	// Summary
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Summary: %d passed, %d failed, %d warning(s)\n", passCount, failCount, warnCount)
	switch {
	case failCount > 0:
		return 1
	case warnCount > 0:
		return 2
	default:
		return 0
	}
}

// containsString reports whether s is in list.
func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// runDoctor is the Dispatch entry point for 'sshmng doctor'.
func runDoctor(ctx context.Context, args []string, out io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(out)
	agent := fs.String("agent", "", "check only specific Agent (claude-code / hermes / opencode)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	opts := DoctorOpts{}
	if *agent != "" {
		opts.AgentFilter = strings.Split(*agent, ",")
		for i := range opts.AgentFilter {
			opts.AgentFilter[i] = strings.TrimSpace(opts.AgentFilter[i])
		}
	}
	return RunDoctor(opts, out)
}
```

Note: `Detect()` returns the expected config path even when the file doesn't exist (installed=false). Doctor uses this path in the SKIP message, so no separate `configPathLabel` method is needed.

- [ ] **Step 4: Add `case "doctor"` to Dispatch in cli.go**

Modify `internal/cli/cli.go`, add to the switch in `Dispatch`:

```go
	case "doctor":
		return runDoctor(ctx, args[1:], out)
```

- [ ] **Step 5: Run tests and verify**

Run: `go test -race ./internal/cli/`
Expected: PASS — all doctor tests pass.

Run end-to-end: `go build -o /tmp/sshmng ./cmd/sshmng && /tmp/sshmng install --yes --agents none && /tmp/sshmng doctor`
Expected: install succeeds (exit 0), doctor prints OK/FAIL/SKIP with summary.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/doctor.go internal/cli/doctor_test.go internal/cli/cli.go
git commit -m "$(cat <<'EOF'
feat(cli): sshmng doctor subcommand

Verifies home dir perms, config.json loadability, Agent config entries.
Three-state exit (0 pass / 1 fail / 2 warn-only). --agent flag filters
to specific Agent. Dispatch routes 'doctor' to runDoctor. Replaces
stub from Task 6.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: README Update

Update README.md to document new CLI structure, 3 Agents, and `install`/`doctor` flow. Replace the "首次配置流程" manual steps with `sshmng install`. Add Hermes Agent and OpenCode sections.

**Files:**
- Modify: `README.md`

**Interfaces:** None (documentation only).

- [ ] **Step 1: Read current README to find sections to update**

Run: `grep -n "首次配置流程\|集成指南\|claude_desktop_config\|Claude Code\|Hermes\|OpenCode" README.md`
Identify sections: "集成指南" (around line 301), "首次配置流程" (around line 378).

- [ ] **Step 2: Update "集成指南" section**

Replace the existing "集成指南" section (which only covers Claude Desktop and Claude Code) with a version that:
- Leads with `sshmng install` as the recommended path
- Documents all three Agents (Claude Code, Hermes Agent, OpenCode) with their config paths and schemas
- Notes that `install` writes `args: ["mcp"]` (not bare `sshmng`)
- Keeps manual config as fallback for when `install` fails

Add Hermes Agent subsection:
```markdown
### Hermes Agent

Edit `~/.hermes/config.yaml` (Unix) or `%LOCALAPPDATA%\hermes\config.yaml` (Windows):

\`\`\`yaml
mcp_servers:
  sshmng:
    command: /Users/<you>/go/bin/sshmng
    args:
      - mcp
    env:
      SSHMNG_HOME: /Users/<you>/.sshmng
\`\`\`

Or run `sshmng install` and select Hermes Agent.
```

Add OpenCode subsection:
```markdown
### OpenCode

Edit `~/.config/opencode/opencode.json`:

\`\`\`json
{
  "mcp": {
    "sshmng": {
      "type": "local",
      "command": ["/Users/<you>/go/bin/sshmng", "mcp"],
      "environment": {"SSHMNG_HOME": "/Users/<you>/.sshmng"},
      "enabled": true
    }
  }
}
\`\`\`

Note: OpenCode uses `command` as an array (binary + args combined) and `environment` (not `env`). Or run `sshmng install` and select OpenCode.
```

Update the Claude Code subsection to use `"args": ["mcp"]` instead of `"args": []`.

- [ ] **Step 3: Replace "首次配置流程" section**

Replace the manual `mkdir`/`echo`/`chmod` steps with:

```markdown
### 首次配置流程

Recommended: run the install wizard.

\`\`\`bash
sshmng install
\`\`\`

The wizard will:
1. Create `~/.sshmng/` (0700) with `config.json` (empty skeleton) and `config.example.json` (Pattern A/B examples)
2. Detect installed AI Agents (Claude Code / Hermes Agent / OpenCode) and let you select which to inject
3. Write the sshmng MCP entry into each selected Agent's config (with timestamped backup)
4. Run `sshmng doctor` to verify setup

For non-interactive use:

\`\`\`bash
sshmng install --yes --agents claude-code,hermes
\`\`\`

Manual fallback (if install fails):

1. Create the directory:
   \`\`\`bash
   mkdir -p ~/.sshmng && chmod 700 ~/.sshmng
   \`\`\`
2. Write `~/.sshmng/config.json` (see `config.example.json` for templates, or use empty skeleton: `{"version":"1","idle_timeout_s":300,"jumphosts":[],"proxies":[],"servers":[]}`)
3. `chmod 600 ~/.sshmng/config.json`
4. Edit your Agent's config file (see 集成指南 above) — use `"args": ["mcp"]` for the sshmng command

### Verifying setup

\`\`\`bash
sshmng doctor
\`\`\`

Checks: home dir permissions, config.json loadability, Agent config entries have sshmng with matching binary path. Exit codes: 0 pass / 1 fail / 2 warn-only.
```

- [ ] **Step 4: Update "安装与构建" section to mention subcommands**

Find the "运行:" section and update to show subcommand usage:

```markdown
运行:

\`\`\`bash
./sshmng                          # Print help
./sshmng mcp                      # Start MCP server (what Agent configs use)
./sshmng install                  # First-time setup wizard
./sshmng doctor                   # Verify setup
./sshmng mcp --config /path/to/config.json  # MCP server with custom config
SSHMNG_HOME=/custom/dir ./sshmng mcp         # MCP server with custom home
\`\`\`
```

- [ ] **Step 5: Verify README renders correctly**

Run: `grep -n "sshmng install\|sshmng doctor\|sshmng mcp" README.md | head -20`
Expected: Multiple matches showing the new commands documented.

- [ ] **Step 6: Final full test run**

Run: `go test -race ./...`
Expected: PASS — all tests pass.

Run: `go build -o /tmp/sshmng ./cmd/sshmng && /tmp/sshmng`
Expected: prints help text.

Run: `/tmp/sshmng install --yes --agents none && /tmp/sshmng doctor`
Expected: install succeeds, doctor prints all-OK summary.

- [ ] **Step 7: Commit**

```bash
git add README.md
git commit -m "$(cat <<'EOF'
docs: update README for new CLI structure + install/doctor

- Document sshmng mcp/install/doctor/help subcommands
- Add Hermes Agent and OpenCode integration sections
- Update Claude Code config to use args: ["mcp"]
- Replace manual first-time setup with 'sshmng install'
- Add 'sshmng doctor' verification section

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
EOF
)"
```

---

## Self-Review Checklist (run after writing plan)

- [x] Spec coverage: All spec sections have tasks
  - CLI structure -> Task 1
  - install command -> Task 6
  - Agent injectors (3) -> Tasks 2-4
  - doctor command -> Task 7
  - files.go scaffolding -> Task 5
  - Windows handling -> woven into Tasks 2/3/5/7 (perm skips, path branches)
  - Error handling -> each task's code includes error returns
  - Testing -> each task has tests
  - README update -> Task 8
- [x] Placeholder scan: No TBD/TODO in steps (the Task 6 doctor stub is intentional, filled in Task 7)
- [x] Type consistency: `MCPEntry`, `AgentInjector`, `ScaffoldOpts`, `InstallOpts`, `DoctorOpts` defined once and used consistently
- [x] Naming: `runInstall`/`RunInstall`, `runDoctor`/`RunDoctor` — lowercase `runX` is the Dispatch entry (parses flags), uppercase `RunX` is the testable function
