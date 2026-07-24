package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jim58246/sshmng/internal/config"
	"github.com/jim58246/sshmng/internal/version"
)

// DoctorOpts configures RunDoctor.
type DoctorOpts struct {
	Home          string   // sshmng home dir
	ExpectedEntry MCPEntry // expected sshmng MCP entry in Agent configs
	AgentFilter   []string // restrict to specific Agents; nil = all
}

// RunDoctor verifies setup and writes results to out. Returns exit code:
//   - 0: all checks pass
//   - 1: at least one FAIL
//   - 2: WARN-only (no FAIL)
func RunDoctor(opts DoctorOpts, out io.Writer) int {
	if opts.Home == "" {
		opts.Home = defaultHome()
	}
	if opts.ExpectedEntry.BinaryPath == "" {
		bin, _ := os.Executable()
		opts.ExpectedEntry.BinaryPath = bin
	}
	if opts.ExpectedEntry.Args == nil {
		opts.ExpectedEntry.Args = []string{"mcp"}
	}
	if opts.ExpectedEntry.Env == nil {
		opts.ExpectedEntry.Env = map[string]string{}
	}
	if opts.ExpectedEntry.Env["SSHMNG_HOME"] == "" {
		opts.ExpectedEntry.Env["SSHMNG_HOME"] = opts.Home
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
	var cfg *config.Config
	var cfgLoadErr error
	if _, err := os.Stat(cfgPath); err != nil {
		print("FAIL", fmt.Sprintf("%s missing - run 'sshmng install'", cfgPath))
	} else {
		store := config.NewStore(cfgPath)
		cfg, cfgLoadErr = store.Load()
		if cfgLoadErr != nil {
			print("FAIL", fmt.Sprintf("config.json invalid: %v", cfgLoadErr))
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

	// update_url (if set)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Update source:")
	if cfgLoadErr == nil && cfg != nil {
		if cfg.UpdateURL == "" {
			print("OK", "update_url: not configured (using GitHub Releases)")
		} else {
			u, parseErr := url.Parse(cfg.UpdateURL)
			if parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				print("FAIL", fmt.Sprintf("invalid update_url %q: must be http:// or https:// URL with host", cfg.UpdateURL))
			} else {
				print("OK", fmt.Sprintf("update_url: %s", cfg.UpdateURL))
			}
		}
	}

	// version (dev build check)
	if version.Version == "dev" {
		print("WARN", "version not set at build time; this is a dev build. Self-update disabled.")
	} else {
		print("OK", fmt.Sprintf("version: %s", version.Version))
	}

	// binary
	if _, err := os.Stat(opts.ExpectedEntry.BinaryPath); err != nil {
		print("FAIL", fmt.Sprintf("binary %s not executable - rebuild with 'go build'", opts.ExpectedEntry.BinaryPath))
	} else {
		print("OK", fmt.Sprintf("binary at %s", opts.ExpectedEntry.BinaryPath))
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
		if err := inj.Verify(path, opts.ExpectedEntry); err != nil {
			print("FAIL", fmt.Sprintf("%s: %v", inj.DisplayName(), err))
		} else {
			print("OK", fmt.Sprintf("%s config has sshmng entry, command/args/env match", inj.DisplayName()))
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
