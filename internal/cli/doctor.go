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
