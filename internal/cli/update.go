package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/jim58246/sshmng/internal/config"
	"github.com/jim58246/sshmng/internal/update"
	"github.com/jim58246/sshmng/internal/version"
)

// runUpdate manually checks for a newer version and applies it. Blocks
// until done; writes progress to out. Unaffected by auto_update_enabled
// (the manual command is always allowed, even when auto-update is off).
func runUpdate(ctx context.Context, args []string, out io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	fmt.Fprintln(out, "sshmng update - checking for updates")
	fmt.Fprintln(out)

	if version.Version == "dev" {
		fmt.Fprintf(out, "[FAIL] version not set at build time. Install an official build or build with -ldflags=\"-X github.com/jim58246/sshmng/internal/version.Version=vX.Y.Z\".\n")
		return 1
	}

	fmt.Fprintf(out, "Current version: %s\n", version.Version)

	path, err := resolveConfigPath("")
	if err != nil {
		fmt.Fprintf(out, "[FAIL] resolve config path: %v\n", err)
		return 1
	}
	store := config.NewStore(path)
	cfg, err := store.Load()
	if err != nil {
		fmt.Fprintf(out, "[FAIL] load config: %v\n", err)
		return 1
	}

	fmt.Fprint(out, "Checking latest release ... ")
	u, err := update.New(update.Config{
		RepoOwner: version.RepoOwner,
		RepoName:  version.RepoName,
		UpdateURL: cfg.UpdateURL,
		CachePath: filepath.Join(filepath.Dir(path), "update_cache.json"),
	})
	if err != nil {
		fmt.Fprintf(out, "[FAIL] %v\n", err)
		return 1
	}

	latest, applied, err := u.UpdateToLatest(ctx)
	if err != nil {
		fmt.Fprintf(out, "[FAIL] %v\n", err)
		return 1
	}
	fmt.Fprintln(out, "done")

	if !applied {
		fmt.Fprintf(out, "Already at latest version (%s).\n", latest)
		return 0
	}

	fmt.Fprintf(out, "Latest version:  %s\n", latest)
	fmt.Fprintln(out, "Updating ... done")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Update applied: %s -> %s\n", version.Version, latest)
	fmt.Fprintln(out, "Restart your Agent (Claude Desktop / Code / Cursor) to use the new version.")
	return 0
}
