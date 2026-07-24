package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"runtime"

	"github.com/jim58246/sshmng/internal/config"
	"github.com/jim58246/sshmng/internal/update"
	"github.com/jim58246/sshmng/internal/version"
)

// runVersion prints the current sshmng version. With --check, also queries
// the remote source for the latest version (cache-aware, read-only).
func runVersion(ctx context.Context, args []string, out io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(out)
	check := fs.Bool("check", false, "check remote for latest version")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	fmt.Fprintf(out, "sshmng %s (%s/%s)\n", version.Version, runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(out, "commit: %s\n", version.Commit)
	fmt.Fprintf(out, "built:  %s\n", version.Date)

	if !*check {
		return 0
	}

	// Load config to get UpdateURL + config dir (for cache path)
	path, err := resolveConfigPath("")
	if err != nil {
		fmt.Fprintf(out, "[WARN] cannot resolve config path: %v\n", err)
		return 0
	}
	store := config.NewStore(path)
	cfg, err := store.Load()
	if err != nil {
		fmt.Fprintf(out, "[WARN] cannot load config: %v\n", err)
		return 0
	}

	u, err := update.New(update.Config{
		RepoOwner: version.RepoOwner,
		RepoName:  version.RepoName,
		UpdateURL: cfg.UpdateURL,
		CachePath: filepath.Join(filepath.Dir(path), "update_cache.json"),
	})
	if err != nil {
		fmt.Fprintf(out, "[WARN] update init failed: %v\n", err)
		return 0
	}

	latest, err := u.LatestVersion(ctx)
	if err != nil {
		fmt.Fprintf(out, "[WARN] remote check failed: %v\n", err)
		return 0
	}

	if update.IsNewer(latest, version.Version) {
		fmt.Fprintf(out, "Checking latest release ... latest is %s\n", latest)
		fmt.Fprintf(out, "Update available: %s -> %s\n", version.Version, latest)
		fmt.Fprintln(out, "Run 'sshmng update' to apply.")
	} else {
		fmt.Fprintln(out, "Checking latest release ... already at latest")
	}
	return 0
}
