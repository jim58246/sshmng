package update

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/creativeprojects/go-selfupdate"
	"github.com/jim58246/sshmng/internal/version"
)

// selfupdateLib is the subset of *selfupdate.Updater we use. Defined as an
// interface so tests can inject a mock instead of swapping the real binary.
// The real *selfupdate.Updater satisfies this via its UpdateSelf method.
type selfupdateLib interface {
	UpdateSelf(ctx context.Context, current string, repo selfupdate.Repository) (*selfupdate.Release, error)
}

// Updater checks for newer sshmng versions and applies them. The cache stores
// the last-checked version + timestamp to stay under GitHub's 60 req/hour
// unauthenticated rate limit. All methods are safe for concurrent use within
// a single process; cross-process coordination is NOT provided (cache TTL
// makes concurrent updates rare and non-corrupting — a lost write just means
// one extra source fetch).
type Updater struct {
	lib       selfupdateLib
	source    selfupdate.Source
	repo      selfupdate.Repository
	cachePath string
	cacheTTL  time.Duration
	log       *slog.Logger
}

// Config configures New.
type Config struct {
	RepoOwner string // GitHub repo owner (required for GitHub source; unused for flat HTTP)
	RepoName  string // GitHub repo name (required for GitHub source; unused for flat HTTP)
	UpdateURL string // "" = GitHub source; "https://..." = flat HTTP source
	CachePath string // where to store update_cache.json (required)
	Log       *slog.Logger
}

// New creates an Updater. Returns an error if the config is invalid
// (missing CachePath, missing repo owner/name for GitHub source, malformed
// UpdateURL for flat HTTP source).
func New(cfg Config) (*Updater, error) {
	if cfg.CachePath == "" {
		return nil, fmt.Errorf("CachePath is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	var src selfupdate.Source
	if cfg.UpdateURL == "" {
		s, err := newGitHubSource(cfg.RepoOwner, cfg.RepoName)
		if err != nil {
			return nil, err
		}
		src = s
	} else {
		s, err := newFlatHTTPSource(cfg.UpdateURL)
		if err != nil {
			return nil, err
		}
		src = s
	}

	// repo is always built from owner/name. For the flat HTTP source the
	// repo is ignored by ListReleases, but UpdateSelf still requires a
	// non-nil Repository to satisfy the library signature.
	repo := selfupdate.NewRepositorySlug(cfg.RepoOwner, cfg.RepoName)

	lib, err := selfupdate.NewUpdater(selfupdate.Config{
		Source:    src,
		Validator: &selfupdate.ChecksumValidator{UniqueFilename: "checksums.txt"},
	})
	if err != nil {
		return nil, fmt.Errorf("create updater: %w", err)
	}

	return &Updater{
		lib:       lib,
		source:    src,
		repo:      repo,
		cachePath: cfg.CachePath,
		cacheTTL:  time.Hour,
		log:       cfg.Log,
	}, nil
}

// LatestVersion returns the latest released version tag (e.g., "v1.2.3").
// Cache-aware: returns the cached value if fresh; otherwise queries the
// source and updates the cache. Read-only — never downloads or swaps the
// binary.
func (u *Updater) LatestVersion(ctx context.Context) (string, error) {
	entry, ok := readCache(u.cachePath)
	if ok && isCacheFresh(entry, u.cacheTTL) {
		u.log.Debug("cache fresh, skipping source call", "cached_version", entry.LatestVersion)
		return entry.LatestVersion, nil
	}

	u.log.Debug("cache stale, querying source")

	// Optimistic write: stamp "just checked" BEFORE the source call to
	// narrow the concurrent-update conflict window to milliseconds. If the
	// source call fails, the cache holds the old version with a fresh
	// timestamp — the next call within TTL skips the source call. That's
	// acceptable: if the source is down, we can't update anyway, and we
	// avoid hammering a failing endpoint on every invocation.
	now := time.Now()
	_ = writeCache(u.cachePath, cacheEntry{LastCheckAt: now, LatestVersion: entry.LatestVersion})

	releases, err := u.source.ListReleases(ctx, u.repo)
	if err != nil {
		return "", fmt.Errorf("list releases: %w", err)
	}
	if len(releases) == 0 {
		return "", fmt.Errorf("no releases found")
	}
	latest := releases[0].GetTagName()

	// Successful fetch — record the version (timestamp already written).
	_ = writeCache(u.cachePath, cacheEntry{LastCheckAt: now, LatestVersion: latest})
	return latest, nil
}

// UpdateToLatest checks for a newer version (cache-aware) and applies it if
// found. Returns the latest version seen and whether an update was applied.
// Already-up-to-date → (latest, false, nil). Dev build → error.
func (u *Updater) UpdateToLatest(ctx context.Context) (latest string, applied bool, err error) {
	if version.Version == "dev" {
		return "", false, fmt.Errorf("version not set at build time (dev build cannot self-update)")
	}

	_ = u.cleanupStaleStaging()

	latest, err = u.LatestVersion(ctx)
	if err != nil {
		return "", false, err
	}

	if !isNewer(latest, version.Version) {
		u.log.Debug("already at latest", "current", version.Version, "latest", latest)
		return latest, false, nil
	}

	u.log.Info("applying update", "current", version.Version, "latest", latest)
	if _, err := u.lib.UpdateSelf(ctx, version.Version, u.repo); err != nil {
		return latest, false, fmt.Errorf("update self: %w", err)
	}
	return latest, true, nil
}

// cleanupStaleStaging is a no-op. go-selfupdate v1.6.0 does NOT stage
// downloads in os.TempDir(): the library's decompressAndUpdate calls
// update.Apply (github.com/inconshreveable/go-update), which writes the new
// binary to a temp file next to the target path and atomically renames — no
// recognizable prefix lives in the system temp dir, and failures roll back
// in place. Sweeping os.TempDir() by prefix would therefore remove
// unrelated files and provide no benefit. Kept as a method so the call site
// in UpdateToLatest stays stable if a future library version reintroduces
// staging.
func (u *Updater) cleanupStaleStaging() error {
	return nil
}
