package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// InstallOpts configures RunInstall.
type InstallOpts struct {
	Home       string   // sshmng home dir (default ~/.sshmng or $SSHMNG_HOME)
	Binary     string   // sshmng binary path (default os.Executable())
	Agents     []string // Agent names to inject; nil = auto-detect; explicit list = use list
	Yes        bool     // non-interactive, use defaults
	SkipFiles  bool     // skip ~/.sshmng/ creation
	SkipAgents bool     // skip Agent injection (set when --agents none)
}

// RunInstall runs the install wizard. Returns process exit code.
// out is where progress messages are written (os.Stdout in production).
func RunInstall(opts InstallOpts, out io.Writer) int {
	r := bufio.NewReader(os.Stdin)

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
	switch {
	case opts.SkipAgents:
		// skip Agent injection entirely
	case len(opts.Agents) > 0:
		// Explicit list from --agents flag
		for _, name := range opts.Agents {
			for _, inj := range allInjectors {
				if inj.Name() == name {
					injectors = append(injectors, inj)
					break
				}
			}
		}
	case opts.Yes:
		// Non-interactive with no explicit list: auto-inject into all
		// detected (installed) Agents. Matches spec default "auto-detect".
		for _, inj := range allInjectors {
			if _, installed := inj.Detect(); installed {
				injectors = append(injectors, inj)
			}
		}
	}
	// else: interactive mode, nil Agents, !SkipAgents -> prompt below

	// Interactive: prompt for missing values
	if !opts.Yes {
		if !opts.SkipAgents && injectors == nil {
			injectors = promptAgentSelection(out, r, allInjectors)
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Review:")
		if !opts.SkipFiles {
			fmt.Fprintf(out, "  + %s/                      (dir, 0700)\n", opts.Home)
			fmt.Fprintf(out, "  + %s/config.json           (0600, empty skeleton)\n", opts.Home)
			fmt.Fprintf(out, "  + %s/config.example.json   (0600, examples)\n", opts.Home)
		}
		for _, inj := range injectors {
			path, _ := inj.Detect()
			fmt.Fprintf(out, "  ~ %s                  (merge sshmng entry, backup -> .bak.<ts>)\n", path)
		}
		confirmed, err := promptConfirmReader(out, r, "Proceed?", false)
		if err != nil || !confirmed {
			fmt.Fprintln(out, "Aborted.")
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
		path, _ := inj.Detect()
		// Detect() returns the config path even when the Agent is not yet
		// installed; Inject() will create the file (and parent dirs) as needed.
		if err := inj.Inject(path, entry); err != nil {
			fmt.Fprintf(out, "  [FAIL] %s: %v\n", inj.DisplayName(), err)
			return 1
		}
		fmt.Fprintf(out, "  [ok] Injected sshmng into %s\n", path)
	}

	// Run doctor at end. Pass the same entry used for injection so doctor can
	// verify command, args, and env.SSHMNG_HOME match what we just wrote.
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Verifying (doctor):")
	docCode := RunDoctor(DoctorOpts{
		Home:          opts.Home,
		ExpectedEntry: entry,
		AgentFilter:   nil,
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
func promptAgentSelection(out io.Writer, r *bufio.Reader, all []AgentInjector) []AgentInjector {
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
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Detected Agents:")
		for i, it := range items {
			mark := " "
			if it.selected {
				mark = "*"
			}
			path, _ := it.inj.Detect()
			if path == "" {
				path = "(not installed)"
			}
			fmt.Fprintf(out, "  [%s] %d. %s    (%s)\n", mark, i+1, it.inj.DisplayName(), path)
		}
		fmt.Fprint(out, "Toggle (1-N), 's' to skip, enter to confirm: ")
		line, err := r.ReadString('\n')
		if err != nil {
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			var selected []AgentInjector
			for _, it := range items {
				if it.selected {
					selected = append(selected, it.inj)
				}
			}
			return selected
		}
		if line == "s" || line == "S" {
			return nil
		}
		// Toggle by number
		var n int
		_, err = fmt.Sscanf(line, "%d", &n)
		if err != nil || n < 1 || n > len(items) {
			fmt.Fprintf(out, "Invalid input %q\n", line)
			continue
		}
		items[n-1].selected = !items[n-1].selected
	}
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
	// parseAgentsFlag returns nil for both "none" and "". We need to
	// distinguish "none" (explicit skip) from "" (auto-detect). Set
	// SkipAgents when the user passed "none" explicitly.
	if *agents == "none" {
		opts.SkipAgents = true
	}
	return RunInstall(opts, out)
}
