package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/pflag"

	"github.com/jim58246/sshmng/internal/config"
	"github.com/jim58246/sshmng/internal/update"
	"github.com/jim58246/sshmng/internal/version"
)

// runUpdate manually checks for a newer version and applies it. Blocks
// until done; writes progress to out. Unaffected by auto_update_enabled
// (the manual command is always allowed, even when auto-update is off).
//
// --file <path>: 用本地下载的 release asset 升级，绕过 GitHub API 限流。
// 支持 .tar.gz / .tar / 目录三种输入（适配 Safari 自动解压行为）。
// 不检查版本新旧（允许降级/回滚），也允许 dev 构建。
func runUpdate(ctx context.Context, args []string, out io.Writer) int {
	fs := pflag.NewFlagSet("update", pflag.ContinueOnError)
	fs.SetOutput(out)
	filePath := fs.String("file", "", "path to a locally downloaded release asset (.tar.gz/.tar) or extracted directory; bypasses GitHub API rate limit")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *filePath != "" {
		return runUpdateFromFile(ctx, *filePath, out)
	}

	fmt.Fprintln(out, "sshmng update - checking for updates")
	fmt.Fprintln(out)

	if version.Version == "dev" {
		fmt.Fprintf(out, "[FAIL] version not set at build time. Install an official build or build with -ldflags=\"-X github.com/jim58246/sshmng/internal/version.Version=vX.Y.Z\".\n")
		return 1
	}

	fmt.Fprintf(out, "Current version: %s\n", version.Version)

	path, err := ResolveConfigPath("")
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

// runUpdateFromFile 执行 --file 模式升级。dev 构建也允许（用户显式指定文件）。
func runUpdateFromFile(ctx context.Context, assetPath string, out io.Writer) int {
	fmt.Fprintln(out, "sshmng update --file")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Asset: %s\n", assetPath)
	fmt.Fprintln(out)

	path, err := ResolveConfigPath("")
	if err != nil {
		fmt.Fprintf(out, "[FAIL] resolve config path: %v\n", err)
		return 1
	}
	u, err := update.New(update.Config{
		RepoOwner: version.RepoOwner,
		RepoName:  version.RepoName,
		CachePath: filepath.Join(filepath.Dir(path), "update_cache.json"),
	})
	if err != nil {
		fmt.Fprintf(out, "[FAIL] %v\n", err)
		return 1
	}

	appliedVersion, err := u.UpdateFromFile(ctx, assetPath)
	if err != nil {
		fmt.Fprintf(out, "[FAIL] %v\n", err)
		return 1
	}

	fmt.Fprintln(out, "Updating ... done")
	fmt.Fprintln(out)
	if appliedVersion == "" {
		fmt.Fprintln(out, "Update applied (version unknown — asset was a directory)")
	} else if version.Version == "dev" {
		fmt.Fprintf(out, "Update applied: dev -> %s\n", appliedVersion)
	} else {
		fmt.Fprintf(out, "Update applied: %s -> %s\n", version.Version, appliedVersion)
	}
	fmt.Fprintln(out, "Restart your Agent (Claude Desktop / Code / Cursor) to use the new version.")
	return 0
}
