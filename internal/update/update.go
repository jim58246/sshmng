package update

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/creativeprojects/go-selfupdate"
	goupdate "github.com/creativeprojects/go-selfupdate/update"
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

// UpdateFromFile 用用户本地下载的 release asset 升级，绕过 GitHub API 限流。
//
// 用户用浏览器（带 GitHub 登录 session）下载 asset，比 unauthenticated API
// 稳得多。assetPath 支持三种输入（适配 Safari 自动解压行为，降低用户心智负担）：
//   - .tar.gz：goreleaser 原始产出
//   - .tar：Safari 把 .tar.gz 剥掉 gzip 层后的产物
//   - 目录：Safari 进一步解压后的目录树（内含 sshmng 二进制）
//
// 不做 checksum 校验——用户自己下载的文件，信任源就是用户自己；Safari 解压后
// hash 也对不上，强校验反而阻碍使用。平台校验仍做（文件名解析 goos/goarch，
// 目录输入跳过此检查）。
//
// 不检查版本新旧——用户显式指定文件，即使"降级"也执行（如回滚到旧版）。
// dev 构建也允许（用户可能用本地构建的 release 包升级 dev 实例）。
//
// 返回从文件名解析出的版本 tag（目录输入返回空字符串）。
func (u *Updater) UpdateFromFile(ctx context.Context, assetPath string) (string, error) {
	cmdPath, err := selfupdate.ExecutablePath()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	return u.updateFromFileWithTarget(ctx, assetPath, cmdPath)
}

// updateFromFileWithTarget 是 UpdateFromFile 的可测试核心：接受显式 targetPath，
// 测试时指向临时文件而非真实二进制，避免修改测试二进制。
func (u *Updater) updateFromFileWithTarget(ctx context.Context, assetPath, targetPath string) (string, error) {
	if assetPath == "" {
		return "", fmt.Errorf("asset file path is required")
	}
	if _, err := os.Stat(assetPath); err != nil {
		return "", fmt.Errorf("asset: %w", err)
	}

	if err := validateAssetPlatform(assetPath); err != nil {
		return "", err
	}

	binaryReader, err := findBinaryReader(assetPath)
	if err != nil {
		return "", err
	}
	defer binaryReader.Close()

	tag := assetVersionFromName(assetPath)
	u.log.Info("applying update from local file", "asset", assetPath, "version", tag, "target", targetPath)

	if err := goupdate.Apply(binaryReader, goupdate.Options{TargetPath: targetPath}); err != nil {
		return tag, wrapUpdateApplyError(err, targetPath)
	}
	return tag, nil
}

// wrapUpdateApplyError 在 update.Apply 失败时包装友好提示。
// 权限错误（二进制路径不可写）是常见场景：用户装在 /usr/local/bin 等系统目录
// 时，update.Apply 的 rename 会失败。检测 permission denied 并提示检查安装位置。
func wrapUpdateApplyError(err error, cmdPath string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "permission denied") || strings.Contains(msg, "operation not permitted") {
		return fmt.Errorf("apply update: %w (hint: %s is not writable by current user; check install location permissions, or reinstall to a user-writable path like ~/.local/bin)", err, cmdPath)
	}
	return fmt.Errorf("apply update: %w", err)
}
