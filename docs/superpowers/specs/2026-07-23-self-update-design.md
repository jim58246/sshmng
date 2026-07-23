# Self-Update：sshmng 二进制自动更新（go-selfupdate + goreleaser）

**Date:** 2026-07-23
**Status:** Approved, ready for implementation plan
**Replaces:** 同日早些时候的 self-hosted HTTP 版本（`latest.txt` + 自写下载/swap），未实现，被本版取代
**Scope:** `internal/version`（新包）, `internal/update`（新包）, `internal/cli`（追加 `update` / `version` 子命令）, `internal/config`（追加 `auto_update_enabled` / `update_url` 字段）, `internal/mcp`（修 `serverInfo.version` 硬编码）, `.goreleaser.yaml`（新增）, `.github/workflows/release.yml`（新增）

依赖前置 spec：[`2026-07-23-quickstart-design.md`](./2026-07-23-quickstart-design.md)（CLI 子命令结构、`internal/cli` 包、`install` / `doctor` 子命令）

## 背景

sshmng 仓库已公开到 GitHub（`github.com/jim58246/sshmng`），需要一个标准的发布 + 自动更新流程：

- **发布侧**：goreleaser 自动构建多平台 binary + checksums，上传到 GitHub Releases
- **客户端侧**：`sshmng mcp` 启动时后台检查 GitHub Releases 新版本，下载 + swap；也支持用户手动 `sshmng update`
- **企业/内网场景**：支持自建静态 HTTP 服务器托管发布件，客户端配置 `update_url` 切换到 HTTP source

选型：`github.com/creativeprojects/go-selfupdate`——成熟的 Go 自更新库，内置 GitHub Releases API 集成、archive 解包、checksum 校验、跨平台 swap。其 `Source` interface 允许扩展非 GitHub 来源，本 spec 用它实现自建 HTTP source。

**为什么替换之前的 self-hosted HTTP 版本**：repo 公开后 GitHub Releases 是更自然的分发渠道；go-selfupdate 替我们承担了下载 / swap / checksum 的复杂度，自写代码量从 ~600 行降到 ~250 行；同时通过 `Source` interface 仍能支持自建 HTTP，企业场景不丢。

## 目标

- **`sshmng mcp` 启动时自动检查更新**：后台 goroutine，本地缓存 TTL 1 小时，不阻塞 MCP server 启动，不写 stdout/stderr
- **`sshmng update` 手动触发**：同步检查 + 下载 + swap，stdout 输出进度
- **`sshmng version [--check]`**：打印当前版本（含 commit / date）；`--check` 拉远端对比
- **跨平台 swap**：go-selfupdate 内置（Unix rename / Windows MoveFileEx rename trick）
- **修 `serverInfo.version` 硬编码**：`internal/mcp/server.go` 用 `version.Version` 替换 `"v1"`
- **双 source**：GitHub Releases（默认）或自建 HTTP（`flatHTTPSource`）
- **goreleaser 标准发布流**：tag push → CI 跑 goreleaser → 多平台 archive + checksums 上传 GitHub Releases
- **同一份 goreleaser 产物两用**：archive 命名同时满足 GitHub Releases 上传 + 自建 HTTP 服务器拷贝，无需重命名

## 非目标

- **Gitea / GitLab source**：v1 不做。go-selfupdate 内置支持，未来有需求加 ~10 行包装
- **Checksum 签名校验**：v1 只做 goreleaser `checksums.txt` 的 sha256 校验（go-selfupdate 默认），不做 cosign / GPG
- **Release notes 展示**：v1 不做
- **Pre-release / beta channel**：v1 单 channel（latest stable）
- **Downgrade / `--version <ver>`**：v1 不支持
- **`--force` 重装同版本**：YAGNI
- **运行中定期 re-check**：只在 `mcp` 启动时检查一次
- **跨用户 lockfile**：TTL 缓存让并发冲突罕见且无害（见"缓存行为"章节的冲突分析），不做
- **Smoke test**：checksum 校验已覆盖完整性，不再跑 `<staging> version`
- **GitHub API token 配置**：TTL 缓存够用，不引入 token 配置
- **崩溃恢复**：go-selfupdate 的 swap 两步 rename 之间微秒级窗口若进程崩溃，v1 不自动恢复，用户重跑 `sshmng install` 修复
- **macOS symlink 调用**：`os.Executable()` 在 macOS 返回调用路径，swap 会替换 symlink 而非真实文件。文档说明此限制
- **HTTP source auth**：v1 不支持 basic auth / bearer token。需 auth 的场景用 GitHub source 或自建 Gitea

## CLI 结构（追加子命令）

quickstart-design 已定 `mcp` / `install` / `doctor` / `help`。本 spec 追加两个：

```
sshmng                          # 无参数 → print help, exit 0
sshmng mcp [--config <path>]    # MCP server 模式；启动时后台 fork 更新检查 goroutine
sshmng install [...]            # 一键安装（quickstart）
sshmng doctor [...]             # 验证配置（quickstart，新增 update_url 检查项）
sshmng update                   # 手动更新：检查 + 下载 + swap，阻塞，stdout 输出
sshmng version [--check]        # 打印当前版本；--check 同时拉远端对比
sshmng help | -h | --help       # 帮助
```

- `update` / `version` 是一次性 CLI 进程，跑完就退
- 只有 `mcp` 模式启动时 fork 自动更新 goroutine；`install` / `doctor` / `update` / `version` 自己**不**自动触发
- 输出风格与 `install` / `doctor` 一致：用户面消息（含 `[FAIL]` / `[WARN]`）走 `out`（stdout），exit code 表示成败

`internal/cli/cli.go` 的 `Dispatch` switch 新增：

```go
case "update":
    return runUpdate(ctx, args[1:], out)
case "version":
    return runVersion(ctx, args[1:], out)
```

`helpText` 追加两行说明。

## 包结构

```
internal/version/          # 新包，仅持有版本/仓库元信息（ldflags 注入目标）
  version.go               # var Version, Commit, Date, RepoOwner, RepoName；无逻辑

internal/update/           # 新包，不绑 CLI 输出，可被 mcp 模式直接调用
  update.go                # Updater struct + New + LatestVersion + UpdateToLatest + cleanupStaleStaging
  cache.go                 # readCache / writeCache / isCacheFresh（内部）
  semver.go                # isNewer（内部，golang.org/x/mod/semver 包装）
  github.go                # newGitHubSource：包装 selfupdate.GitHubSource（内部）
  flathttp.go              # flatHTTPSource + flatRelease + flatAsset（实现 selfupdate.Source / SourceRelease / SourceAsset）

internal/cli/
  cli.go                   # quickstart 已规划；Dispatch 新增 update / version 分支
  update.go                # sshmng update 子命令
  version.go               # sshmng version 子命令
  doctor.go                # quickstart 规划；新增 update_url 校验项 + dev 构建检查
  install.go               # quickstart 规划，不动
  mcp.go                   # quickstart 规划；新增 auto-update goroutine 挂载

internal/config/types.go   # Config 新增 AutoUpdateEnabled / UpdateURL 字段
internal/mcp/server.go     # mcp.Implementation.Version 从硬编码 "v1" 改为 version.Version

cmd/sshmng/main.go         # 不动（仍是 cli.Dispatch 入口）

.goreleaser.yaml           # 新增
.github/workflows/release.yml  # 新增
```

**为什么拆 `internal/version`**：`internal/mcp` 只需读一个版本字符串，不应 import 整个 `internal/update`（带 go-selfupdate 重量级依赖）。`internal/version` 是 10 行无逻辑包，`internal/mcp` 和 `internal/update` 都 import 它，依赖图清晰：

```
internal/mcp      → internal/version (读 Version)
internal/update   → internal/version (读 Version / RepoOwner / RepoName 做 semver 比较 + source 构造)
internal/cli/mcp  → internal/update  (启动 auto-update goroutine)
internal/cli/*    → internal/version (打印版本)
```

新依赖：
- `github.com/creativeprojects/go-selfupdate`（主依赖，带 archive/tar + archive/zip + sha256 + swap 实现）
- `golang.org/x/mod`（go-selfupdate 传递依赖，直接用其 `mod/semver` 包做版本比较，零新增传递 dep）

不引入：
- `github.com/gofrs/flock`（去掉 lockfile）
- `golang.org/x/sys/windows`（go-selfupdate 内部处理 Windows swap）

## `internal/version` 包

```go
package version

// All variables are injected by goreleaser via -ldflags at build time.
// Defaults apply for non-goreleaser builds (go build / go run / go test).

var (
    // Version is the git tag (e.g., "v1.2.3"). "dev" for non-release builds.
    // Self-update is disabled when Version == "dev".
    Version = "dev"

    // Commit is the git short SHA. "none" for dev builds.
    Commit = "none"

    // Date is the build timestamp (RFC3339). "unknown" for dev builds.
    Date = "unknown"

    // RepoOwner / RepoName identify the GitHub repository for self-update.
    // Forks override these via ldflags to redirect updates to their fork.
    // For HTTP source, these are unused (update_url drives the source).
    RepoOwner = "jim58246"
    RepoName  = "sshmng"
)
```

ldflags 注入目标相应为 `-X github.com/jim58246/sshmng/internal/version.Version={{.Tag}}` 等。

## `internal/update` 包：Updater 抽象

**两个公开方法 + 一个构造函数**：

```go
package update

import (
    "context"
    "log/slog"
)

// Updater checks for newer sshmng versions and applies them.
// Cache stores last-checked version + timestamp to stay under GitHub's
// 60 req/hour unauthenticated rate limit.
//
// All methods are safe for concurrent use within a single process.
// Cross-process coordination is NOT provided — cache TTL makes concurrent
// updates rare and non-corrupting.
type Updater struct {
    // unexported: source selfupdate.Source, cachePath string, cacheTTL time.Duration, log *slog.Logger
}

type Config struct {
    // RepoOwner / RepoName: GitHub repo identifier. Required for GitHub source.
    // Ignored for HTTP source.
    RepoOwner string
    RepoName  string

    // UpdateURL: source selector.
    //   "" → use GitHub Releases (via RepoOwner/RepoName)
    //   "https://..." → use flatHTTPSource pointing at this base URL
    UpdateURL string

    // CachePath: where to store last-checked version + timestamp.
    // Typically filepath.Join(configDir, "update_cache.json").
    CachePath string

    // Log: structured logger for diagnostics. User-facing output is handled
    // by the CLI layer, not by Updater.
    Log *slog.Logger
}

// New creates an Updater. Returns error if Config is invalid
// (e.g., HTTP source with malformed URL).
func New(cfg Config) (*Updater, error)

// LatestVersion returns the latest released version (e.g., "v1.2.3").
// Cache-aware: returns cached value if fresh (within 1 hour); otherwise
// queries the source and updates cache. Read-only — never downloads or
// swaps the binary. Used by `sshmng version --check`.
func (u *Updater) LatestVersion(ctx context.Context) (string, error)

// UpdateToLatest checks for a newer version (cache-aware) and applies it if found.
// Returns the latest version seen and whether an update was applied.
// Already-up-to-date → (latest, false, nil) with no side effects.
// Used by auto-update goroutine and `sshmng update`.
func (u *Updater) UpdateToLatest(ctx context.Context) (latest string, applied bool, err error)
```

**设计要点**：

1. **缓存只存版本字符串，不存 Release 对象** — GitHub asset 下载 URL 是 S3 签名 URL，会过期。`LatestVersion` 缓存命中时直接返回版本号；`UpdateToLatest` 即便缓存命中也重新调 `DetectLatest` 拿新鲜 Release（1 次 API 调用，远低于限速）。稳态（已最新）下 0 次 API 调用。

2. **semver 比较用 `golang.org/x/mod/semver`** — go-selfupdate 传递依赖，零新增 dep。版本字符串带 `v` 前缀（用 goreleaser `{{.Tag}}`），`semver.Compare` 要求 `v` 前缀，匹配。

```go
// internal/update/semver.go
package update

import "golang.org/x/mod/semver"

// isNewer returns true if latest > current. Both must have "v" prefix.
// current == "dev" (non-release build) always returns true (but caller
// should have short-circuited already).
func isNewer(latest, current string) bool {
    if current == "dev" { return true }
    if !semver.IsValid(latest) || !semver.IsValid(current) { return false }
    return semver.Compare(latest, current) > 0
}
```

3. **CleanupStaleStaging 是 `UpdateToLatest` 内部第一步** — 不暴露公开方法。go-selfupdate 失败时可能在系统 temp dir 留 staging 文件，扫一遍清掉。`sshmng version --check` 不创建 staging，不需要清。

4. **进度输出降级** — go-selfupdate 的"下载 + checksum + swap"是一个库调用，无法插入细粒度进度。`sshmng update` 输出简化为五段（见"`sshmng update` 子命令"章节）。

## Source 后端

### GitHub source（默认）

`UpdateURL == ""` 时启用。包装 `selfupdate.NewGitHubSource(GitHubConfig{})`：

```go
// internal/update/github.go
package update

import (
    "fmt"
    "github.com/creativeprojects/go-selfupdate"
)

func newGitHubSource(owner, name string) (selfupdate.Source, error) {
    if owner == "" || name == "" {
        return nil, fmt.Errorf("github source requires RepoOwner and RepoName (ldflags not injected?)")
    }
    src, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{
        Repo: selfupdate.Repository{Owner: owner, Name: name},
    })
    if err != nil {
        return nil, fmt.Errorf("github source: %w", err)
    }
    return src, nil
}
```

- 用 `version.RepoOwner` / `version.RepoName`（ldflags 注入，fork 可覆盖）
- 无 token：公开 repo 不需要，60 req/hour/IP 由 TTL 缓存兜底
- 走 `api.github.com`（公开 GitHub）

### flatHTTPSource（自建 HTTP）

`UpdateURL` 非空时启用。实现 `selfupdate.Source` interface（库的扩展点）：

```go
// internal/update/flathttp.go
package update

import (
    "context"
    "fmt"
    "io"
    "net/http"
    "strings"
    "time"

    "github.com/creativeprojects/go-selfupdate"
    "golang.org/x/mod/semver"
)

type flatHTTPSource struct {
    baseURL string
    client  *http.Client
}

func newFlatHTTPSource(baseURL string) (*flatHTTPSource, error) {
    if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
        return nil, fmt.Errorf("update_url must be http:// or https:// URL, got: %q", baseURL)
    }
    return &flatHTTPSource{
        baseURL: strings.TrimRight(baseURL, "/"),
        client:  &http.Client{Timeout: 60 * time.Second},
    }, nil
}

func (s *flatHTTPSource) ListReleases(ctx context.Context, _ selfupdate.Repository) ([]selfupdate.SourceRelease, error) {
    // 1. GET {baseURL}/latest.txt → "v1.2.3"
    tag, err := s.fetchLatest(ctx)
    if err != nil { return nil, err }
    if !semver.IsValid(tag) {
        return nil, fmt.Errorf("latest.txt returned invalid semver: %q", tag)
    }

    // 2. 构造 6 个 asset by convention（go-selfupdate 按 GOOS/GOARCH 关键字匹配）
    platforms := []struct{ goos, goarch, ext string }{
        {"darwin", "amd64", "tar.gz"},
        {"darwin", "arm64", "tar.gz"},
        {"linux", "amd64", "tar.gz"},
        {"linux", "arm64", "tar.gz"},
        {"windows", "amd64", "zip"},
        {"windows", "arm64", "zip"},
    }
    assets := make([]selfupdate.SourceAsset, 0, len(platforms)+1)
    for i, p := range platforms {
        name := fmt.Sprintf("sshmng-%s-%s-%s.%s", tag, p.goos, p.goarch, p.ext)
        assets = append(assets, &flatAsset{
            id:   int64(i),
            name: name,
            url:  s.baseURL + "/" + name,
        })
    }
    // checksums.txt 作为额外 asset，go-selfupdate 默认 Validator 会读它做 sha256 校验
    assets = append(assets, &flatAsset{
        id:   int64(len(platforms)),
        name: "checksums.txt",
        url:  s.baseURL + "/checksums.txt",
    })

    return []selfupdate.SourceRelease{&flatRelease{tag: tag, assets: assets}}, nil
}

func (s *flatHTTPSource) DownloadReleaseAsset(ctx context.Context, rel *selfupdate.Release, assetID int64) (io.ReadCloser, error) {
    for _, a := range rel.Assets {
        if a.GetID() == assetID {
            req, err := http.NewRequestWithContext(ctx, "GET", a.GetBrowserDownloadURL(), nil)
            if err != nil { return nil, err }
            resp, err := s.client.Do(req)
            if err != nil { return nil, err }
            if resp.StatusCode != http.StatusOK {
                resp.Body.Close()
                return nil, fmt.Errorf("download %s: HTTP %d", a.GetBrowserDownloadURL(), resp.StatusCode)
            }
            return resp.Body, nil
        }
    }
    return nil, fmt.Errorf("asset id %d not found in release", assetID)
}

// + flatRelease implementing selfupdate.SourceRelease (getters for tag, assets, etc.)
// + flatAsset implementing selfupdate.SourceAsset (getters for id, name, url)
// + fetchLatest helper: GET {baseURL}/latest.txt, return trimmed body (64KB limit)
```

**asset 命名约定**：`sshmng-{tag}-{goos}-{goarch}.{tar.gz|zip}`
- go-selfupdate 按 asset 名字里的 `{goos}` / `{goarch}` 关键字自动匹配当前平台，无需我们写选择逻辑
- tag 带 `v` 前缀，跟 GitHub Releases 一致
- Windows 用 `.zip`，其他用 `.tar.gz`（goreleaser 默认）
- 此命名与 `.goreleaser.yaml` 的 `archives.name_template` **完全一致**，同一份产物两用

**checksum 校验**：go-selfupdate 的 `Validator` 是 opt-in（`Config.Validator` 默认 nil = 不校验）。我们在 `update.New` 里显式配置：

```go
// internal/update/update.go 内，构造 selfupdate.Config 时
validator := &selfupdate.ChecksumValidator{UniqueFilename: "checksums.txt"}
lib, err := selfupdate.NewUpdater(selfupdate.Config{
    Source:    src,        // githubSource 或 flatHTTPSource
    Validator: validator,
})
```

`ChecksumValidator` 从 release assets 里找名为 `checksums.txt` 的文件，读 sha256，校验下载的 archive。`flatHTTPSource.ListReleases` 必须把 `checksums.txt` 作为额外 asset 暴露（见上方代码），否则 `Validator` 报 `ErrValidationAssetNotFound`。

GitHub source 自动包含 `checksums.txt`（GitHub Releases 上传的 asset 里就有），无需额外处理。

## 自建 HTTP 服务器文件布局要求

```
{base_url}/
  latest.txt                                    # 必需，纯文本一行：v1.2.3\n
  checksums.txt                                 # 必需，goreleaser 生成，sha256 每行一个
  sshmng-v1.2.3-darwin-arm64.tar.gz
  sshmng-v1.2.3-darwin-amd64.tar.gz
  sshmng-v1.2.3-linux-amd64.tar.gz
  sshmng-v1.2.3-linux-arm64.tar.gz
  sshmng-v1.2.3-windows-amd64.zip
  sshmng-v1.2.3-windows-arm64.zip
```

**服务器要求**：
- 静态文件服务（GET 即可，无服务端脚本）
- 任何静态服务器都行：nginx / Caddy / Python `python3 -m http.server` / S3 + CloudFront / GitHub Pages
- HTTPS 推荐，HTTP 允许（内网场景）
- 无 auth（v1 不支持）

**`latest.txt` 规范**：
- 纯文本，第一行是版本号，必须 `v` 前缀（`v1.2.3`，semver）
- 末尾允许 `\n`，客户端 `strings.TrimSpace`
- HTTP 200 + body 才算成功；非 200 报错放弃
- Content-Type 不校验
- 客户端读 body 上限 64KB（防御性）

**发布流程**：
1. 本地或 CI 跑 `goreleaser release --clean`，产出 `dist/` 目录
2. 把 `dist/` 里的 `sshmng-v*-*-*.*` archives + `checksums.txt` 上传/拷贝到服务器 `{base_url}/`
3. 更新 `{base_url}/latest.txt` 指向新版本（一行：`v1.2.3`）
4. 客户端下次启动检查时拉到新版本

旧版本文件保留不删——改 `latest.txt` 指向旧版即可让客户端下次拉旧版，实现 downgrade。

## 缓存行为

**缓存文件**：`<config_dir>/update_cache.json`（与 `config.json` 同目录）

**格式**：
```json
{
  "last_check_at": "2026-07-23T10:30:00Z",
  "latest_version": "v1.2.3"
}
```

**TTL**：1 小时（v1 硬编码，不可配）

**`LatestVersion` 流程**（乐观更新，见下方冲突分析）：
```
1. readCache()
   - 文件不存在 → 视为 stale
   - JSON 解析失败 → 视为 stale（覆盖写）
   - last_check_at 在 1 小时内 → 返回 latest_version（fresh）
   - 否则 → stale
2. 若 fresh：直接返回 latest_version，不打网络
3. 若 stale：
   a. writeCache(last_check_at=now, latest_version=<保留旧值>)  // 乐观：立即标记已检查，压窄冲突窗口到毫秒级
   b. latest, err := source.ListReleases()
   c. 若 err：return err（cache 已标 now，1 小时内不重试——API 暂时失败可接受延迟）
   d. writeCache(latest_version=latest)  // 成功才更新版本字段
   e. 返回 latest
```

**`UpdateToLatest` 流程**：
```
1. cleanupStaleStaging()
2. latest := u.LatestVersion(ctx)  // 走缓存逻辑
   - 错误 → return "", false, err
3. if !isNewer(latest, version.Version):
   return latest, false, nil  // 已最新，无操作
4. // 需要更新：重新调 source 拿新鲜 Release（asset URL 可能过期）
   release, err := lib.DetectLatest(ctx, repo)
5. err = lib.UpdateSelf(ctx, release)
6. return latest, true, err
```

**缓存与限速的关系**：
- 稳态（已最新）：缓存 fresh → 0 次 API 调用
- 缓存过期（每小时 1 次）：1 次 API 调用
- 单人单 Agent，每 5 分钟启动一次：12 次/小时潜在启动，但缓存让实际 API 调用降到 1 次/小时
- 多 Agent 并发启动（Claude Desktop + Cursor 同时打开）：缓存让两人同时看到 stale 的概率很低（毫秒级窗口），即便同时打 API 也只是 2 次，远低于 60/hour

**冲突分析（为什么不加 lockfile）**：

加了 TTL 后，大部分启动根本不走网络。真正可能冲突的窗口是"两个进程同时看到 stale 缓存"：

| 策略 | stale 窗口大小 | 冲突条件 |
|------|--------------|---------|
| 乐观更新（API 调用前就写 `last_check_at`） | 毫秒级 | 两个进程在毫秒内同时看到 stale，概率极低 |
| 悲观更新（API 成功后才写） | 秒级（API 往返 ~200ms-1s） | 两个进程在 1 秒内同时启动且都看到 stale |

采纳**乐观更新**：`LatestVersion` 决定打 API 时立即写 `last_check_at = now`（`latest_version` 暂留旧值），API 成功后再写 `latest_version`。冲突窗口压到毫秒级。

即便冲突，后果只是：两个进程都下载了 10MB 二进制（浪费带宽），两个都 swap（Unix rename 原子，最后一个赢，无 corruption）。**无数据损坏，无功能影响**。

年冲突频率估算（单人双 Agent 用户，每天打开关闭 10 次）：每天最多 ~1 次潜在冲突，每次冲突后果是浪费 10MB 带宽。lockfile 的边际价值是防那 1 次/天的双下载，代价是 ~30 行代码 + 一个 dep。YAGNI，砍掉。

**并发安全**：
- 缓存文件读写不做进程内锁（单进程内 Updater 只被一个 goroutine 调用）
- 跨进程不做锁（TTL 让并发冲突罕见且无害）
- 极端情况：两个进程同时写缓存文件 → 后写者赢，无 corruption（`writeCache` 写到 temp 文件 + `os.Rename` 原子替换）

## 配置变更

### `Config` 新增字段

`internal/config/types.go`：

```go
// AutoUpdateEnabled controls whether `sshmng mcp` spawns the auto-update
// goroutine on startup. Default true (opt-out). Manual `sshmng update` and
// `sshmng version --check` are unaffected (always allowed).
AutoUpdateEnabled bool `json:"auto_update_enabled,omitempty"`

// UpdateURL selects the update source.
//   "" (empty) → use GitHub Releases (default; uses binary's built-in RepoOwner/RepoName)
//   "https://..." → use self-hosted HTTP server at this base URL
// Format: https://updates.example.com/sshmng (no trailing slash; client appends /latest.txt etc.)
UpdateURL string `json:"update_url,omitempty"`
```

**默认行为处理**：Go 零值 `bool` 是 `false`，但我们要默认启用 auto-update。`install` 生成的空骨架**显式写** `"auto_update_enabled": true`，让默认行为对用户可见、可改。用户手动删该字段 → auto-update 禁用（与零值行为一致）。

### `mcp.Implementation.Version` 修复

`internal/mcp/server.go:130` 现状：

```go
server := mcp.NewServer(&mcp.Implementation{Name: "sshmng", Version: "v1"}, ...)
```

改为：

```go
import "sshmng/internal/version"

server := mcp.NewServer(&mcp.Implementation{
    Name:    "sshmng",
    Version: version.Version,
}, ...)
```

- `internal/mcp` → `internal/version` 单向依赖，无环
- Agent 通过 `initialize.serverInfo.version` 看到真实版本
- dev 构建 → Agent 看到 `"dev"`，能识别

## 版本注入：goreleaser 标准流程

### `.goreleaser.yaml`

```yaml
version: 2

project_name: sshmng

before:
  hooks:
    - go mod tidy

builds:
  - main: ./cmd/sshmng
    env:
      - CGO_ENABLED=0
    goos: [linux, darwin, windows]
    goarch: [amd64, arm64]
    ldflags:
      - -s -w
      - -X github.com/jim58246/sshmng/internal/version.Version={{.Tag}}
      - -X github.com/jim58246/sshmng/internal/version.Commit={{.ShortCommit}}
      - -X github.com/jim58246/sshmng/internal/version.Date={{.CommitDate}}
      - -X github.com/jim58246/sshmng/internal/version.RepoOwner={{.Env.GITHUB_REPOSITORY_OWNER}}
      - -X github.com/jim58246/sshmng/internal/version.RepoName={{.Env.GITHUB_REPOSITORY_NAME}}

archives:
  - id: default
    name_template: >-
      sshmng-{{ .Tag }}-{{ .Os }}-{{ .Arch }}
    format: tar.gz
    format_overrides:
      - goos: windows
        formats: [zip]
    files:
      - LICENSE
      - README.md

checksum:
  name_template: 'checksums.txt'

release:
  draft: false
  prerelease: auto
  name_template: "{{ .Tag }}"

changelog:
  use: gitlab
  sort: asc
  filters:
    exclude:
      - '^docs:'
      - '^test:'
      - '^chore:'
```

**设计决策**：

1. **版本字符串带 `v` 前缀（`{{.Tag}}` 而非 `{{.Version}}`）**：`golang.org/x/mod/semver` 要求 `v` 前缀（`semver.IsValid("1.2.3")` = false）。GitHub Releases API 返回的 `TagName` 也带 `v`。全链路统一。

2. **`RepoOwner` / `RepoName` 通过 GitHub Actions env 注入**：`GITHUB_REPOSITORY_OWNER` / `GITHUB_REPOSITORY_NAME` 是 Actions 自动设置。fork 用户跑自己的 release workflow 时自动指向 fork，无需改 goreleaser 配置。源码里的 `"jim58246"` / `"sshmng"` 是 fallback，只在本地 `go build` 时生效。

3. **6 平台**：darwin/linux/windows × amd64/arm64。覆盖主流桌面 + 服务器 + ARM 设备。Raspberry Pi 32 位 (armv6/v7) 和 FreeBSD 等暂不支持，未来有需求加。

4. **archive 命名**：`sshmng-v1.2.3-darwin-arm64.tar.gz` —— 与 `flatHTTPSource` 的 asset 命名约定**完全一致**。同一份 goreleaser 产物既能上传 GitHub Releases，也能直接拷到自建 HTTP 服务器，无需重命名。

5. **`checksums.txt`**：goreleaser 自动生成，go-selfupdate 默认用它做 sha256 校验。flatHTTPSource 服务器也放同一份。

6. **`prerelease: auto`**：tag 是 `v1.2.3` → stable；tag 是 `v1.2.3-beta.1` → prerelease（go-selfupdate 默认不拉 prerelease，符合 v1 单 channel 目标）。

### `.github/workflows/release.yml`

```yaml
name: release

on:
  push:
    tags: ['v*']

permissions:
  contents: write

jobs:
  goreleaser:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version: '1.25'
      - uses: goreleaser/goreleaser-action@v6
        with:
          version: latest
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

- `fetch-depth: 0`：goreleaser 需要完整 git 历史生成 changelog
- `GITHUB_TOKEN`：Actions 自动提供，无需配 secret
- 触发条件：push `v*` tag。发布流程：`git tag v1.2.3 && git push origin v1.2.3`

## 更新流程

### `sshmng mcp` 启动时的 auto-update goroutine

挂载点在 `internal/cli/mcp.go` 的 `runMCP`，logger 就绪后、`svc.Run(ctx)` 之前：

```go
if cfg.AutoUpdateEnabled && version.Version != "dev" {
    go func() {
        u, err := update.New(update.Config{
            RepoOwner: version.RepoOwner,
            RepoName:  version.RepoName,
            UpdateURL: cfg.UpdateURL,
            CachePath: filepath.Join(filepath.Dir(path), "update_cache.json"),
            Log:       logger,
        })
        if err != nil {
            logger.Warn("auto-update init failed", "err", err)
            return
        }
        latest, applied, err := u.UpdateToLatest(ctx)
        if err != nil {
            logger.Warn("auto-update failed", "err", err)
            return
        }
        if applied {
            logger.Info("auto-update applied", "old", version.Version, "new", latest)
        }
    }()
}
```

- `cfg.AutoUpdateEnabled == false` → 完全跳过
- `version.Version == "dev"` → 跳过（开发构建不做 self-update）
- goroutine 用 MCP server 的 `ctx`，SIGINT/SIGTERM 时取消
- 所有错误只 `logger.Warn`，**绝不写 stdout / stderr**（MCP server 模式不变量）

### `sshmng update` 子命令

`internal/cli/update.go`，输出走 `out`：

```
$ sshmng update
sshmng update - checking for updates

Current version: v1.2.3
Checking latest release ... done
Latest version:  v1.2.4
Updating ... done

Update applied: v1.2.3 -> v1.2.4
Restart your Agent (Claude Desktop / Code / Cursor) to use the new version.

$ sshmng update   # 已是最新
sshmng update - checking for updates

Current version: v1.2.3
Checking latest release ... done
Already at latest version (v1.2.3).

$ sshmng update   # Version = "dev"
sshmng update - checking for updates

[FAIL] version not set at build time. Install an official build or build with -ldflags="-X sshmng/internal/version.Version=vX.Y.Z".

$ sshmng update   # 网络失败
sshmng update - checking for updates

Current version: v1.2.3
Checking latest release ... [FAIL] fetch latest: <err>
```

**Flags**：
- `-h` / `--help`（FlagSet 默认）
- 不加 `--force`（YAGNI）

**Exit codes**：
- `0`：成功（已更新 / 已是最新）
- `1`：失败（网络 / swap / Version=dev / 配置错误）
- `2`：flag 解析错误

**不受 `auto_update_enabled` 影响**：`sshmng update` 是手动命令，永远可用。

### `sshmng version` 子命令

`internal/cli/version.go`：

```
$ sshmng version
sshmng v1.2.3 (darwin/arm64)
commit: abc1234
built:  2026-07-23T10:30:00Z

$ sshmng version --check
sshmng v1.2.3 (darwin/arm64)
commit: abc1234
built:  2026-07-23T10:30:00Z
Checking latest release ... latest is v1.2.4
Update available: v1.2.3 -> v1.2.4
Run 'sshmng update' to apply.

$ sshmng version --check   # 已最新
sshmng v1.2.3 (darwin/arm64)
commit: abc1234
built:  2026-07-23T10:30:00Z
Checking latest release ... already at latest

$ sshmng version --check   # 网络失败
sshmng v1.2.3 (darwin/arm64)
commit: abc1234
built:  2026-07-23T10:30:00Z
[WARN] remote check failed: <err>

$ sshmng version   # dev 构建
sshmng dev (darwin/arm64)
commit: none
built:  unknown
```

**Flags**：
- `--check`：拉远端对比（走 `LatestVersion`，缓存命中时不打网络）
- `-h` / `--help`

**Exit codes**：
- `0`：成功（version 总能打印；`--check` 失败也只是 warn，不 exit 1）
- `2`：flag 解析错误

**行为细节**：
- `--check` 走 `LatestVersion`，**不下载、不 swap**——只读检查
- `Version == "dev"` 时仍可 `--check`（dev 构建只是不自动更新，手动查最新版仍可以）

## doctor 集成

`internal/cli/doctor.go` 新增检查项：

| 类别 | 检查项 | 失败等级 | 失败时的提示 |
|------|--------|---------|-------------|
| **config.json** | `update_url` 非空时是合法 `http://` / `https://` URL | FAIL | "Invalid update_url: <err>. Must be http:// or https:// URL." |
| | `update_url` 为空（用 GitHub source） | OK | "[OK] update_url: not configured (using GitHub Releases)" |
| | `update_url` 配置了 | OK | "[OK] update_url: <url>" |
| **binary** | `sshmng version` 输出非 `dev` | WARN | "Version not set at build time; this is a dev build. Self-update disabled." |

不发网络请求，纯字符串校验。dev 构建 WARN 不 FAIL（开发时正常）。

## 失败处理矩阵

| 步骤 | 失败模式 | auto-update goroutine | `sshmng update` CLI |
|------|---------|----------------------|---------------------|
| `New(cfg)` | 配置错误（非法 URL 等） | `logger.Warn` + return | 打印 `[FAIL]`，exit 1 |
| `cleanupStaleStaging` | 删不掉（权限） | `logger.Warn` + 继续 | 打印 `[WARN]` + 继续 |
| `LatestVersion`（缓存命中） | — | 无操作 | 无操作 |
| `LatestVersion`（缓存 stale） | 网络 / 非 200 / 非法 semver | `logger.Warn` + return err | 打印 `[FAIL]`，exit 1 |
| Compare | `latest <= current` | return nil（无操作） | 打印 "Already at latest"，exit 0 |
| `DetectLatest`（拿新鲜 Release） | 网络 / 非 200 | `logger.Warn` + return err | 打印 `[FAIL]`，exit 1 |
| `UpdateSelf`（下载 + checksum + swap） | 网络 / checksum 失败 / swap 失败 | `logger.Warn` + return err | 打印 `[FAIL]`，exit 1 |
| `UpdateSelf` 成功 | — | `logger.Info` + return nil | 打印 "Update applied"，exit 0 |

**关键不变量**：任何失败路径都不能让 `exePath` 处于"不存在"状态。go-selfupdate 内部用原子 rename（Unix）/ Windows rename trick + rollback，保证此不变量。

## 测试策略

### 单元测试

- `internal/update/semver_test.go`：`isNewer` 覆盖 `=`、`>`、`<`、`dev`、非法格式
- `internal/update/cache_test.go`：tempdir 里测 `readCache` / `writeCache` / `isCacheFresh`，覆盖文件不存在 / JSON 损坏 / fresh / stale
- `internal/update/flathttp_test.go`：`httptest.Server` mock
  - `ListReleases`：`latest.txt` 合法 / 404 / 非法 semver / 超时
  - `DownloadReleaseAsset`：200 返回 bytes / 404 / 超时
  - asset 命名匹配：构造 release，验证 go-selfupdate 能按 GOOS/GOARCH 选对 asset
- `internal/update/update_test.go`：端到端，mock source（实现 `selfupdate.Source` interface）
  - latest > current → 下载 + swap + 验证 exe 内容更新
  - latest == current → 无操作（无下载请求）
  - latest < current → 无操作
  - cache fresh → 0 次 source 调用
  - cache stale → 1 次 source 调用 + cache 更新
- `internal/version/version_test.go`：默认值正确（`dev` / `none` / `unknown` / `jim58246` / `sshmng`）

### CLI 测试

- `internal/cli/update_test.go`：
  - 子命令分发到 `runUpdate`
  - `Version == "dev"` → `[FAIL]`，exit 1
  - 成功路径用 mock `update.Updater`（注入接口，不真跑网络）
  - 已是最新 → exit 0
- `internal/cli/version_test.go`：
  - 无 flag → 打印 `sshmng <version> (<goos>/<goarch>)` + commit + date，exit 0
  - `--check` + mock fetcher 返回 newer → 打印 "Update available"，exit 0
  - `--check` + mock fetcher 失败 → 打印 `[WARN] remote check failed`，exit 0
- `internal/cli/doctor_test.go`：新增 `update_url` 校验项的 pass / fail + dev 构建检查
- `internal/cli/cli_test.go`：分发路由新增 `update` / `version` case

### 集成测试

- `cmd/sshmng/e2e_test.go`：
  - spawn `sshmng version`，验证 exit 0 + stdout 含 `sshmng`
  - spawn `sshmng version --check`（无 `update_url`，GitHub source），验证 exit 0（网络不可达时 warn 也算 pass）
  - spawn `sshmng update`（`Version=dev`），验证 exit 1 + 含 `[FAIL]`

### goreleaser 配置测试

- `goreleaser check`：CI 跑配置语法校验
- `goreleaser release --snapshot --clean`：本地 snapshot 构建验证（不上传 GitHub）

### 不测试

- 真实 GitHub Releases API（所有 source 走 `httptest.Server` 或 mock）
- 真实 binary 自替换（CI 上风险高，仅单元测试 tempdir 模拟）
- 跨平台 swap 行为在错误 OS 上（`t.Skip` + `runtime.GOOS` 分支）
- 跨机器 / 跨用户场景

## 后续考虑（v1 不实现）

- **Gitea / GitLab source**：go-selfupdate 内置支持，~10 行包装。企业用户跑 Gitea/GitLab 可加
- **GitHub API token 配置**：NAT 限速场景下可选 token 拿 5000/h。当前 TTL 缓存够用
- **Checksum 签名校验**：cosign / GPG。当前 sha256 够用
- **Release notes 展示**：`sshmng version --check` 显示 release notes 摘要
- **Pre-release channel**：`config.json` 加 `update_channel` 字段（stable / beta）
- **`--force` / `--version <ver>`**：手动指定版本或强制重装同版本
- **Downgrade**：HTTP source 服务器保留旧版本目录即可支持（改 `latest.txt` 指向旧版）；GitHub source 改 `tag_name` 查询
- **运行中定期 re-check**：`sshmng mcp` 长期运行场景加 `update_check_interval_s` config 字段
- **跨用户 lockfile**：共享 binary 场景下跨用户互斥。当前不做——TTL 缓存让冲突罕见且无害
- **崩溃恢复**：swap 两步 rename 之间窗口若进程崩溃，自动检测并完成
- **HTTP source auth**：basic auth / bearer token 支持
- **HTTP source manifest**：`latest.txt` 升级为 `manifest.json`（含多版本 / release notes / checksum inline）

## README 更新

README 的"安装与构建"章节需更新：

- `go build` 命令加 `-ldflags` 示例（参考 `.goreleaser.yaml`）
- 新增"自动更新"章节：说明默认开、`auto_update_enabled` 配置、`sshmng mcp` 启动时自动检查、`sshmng update` 手动触发
- 新增"自建 HTTP 源"章节：`update_url` 配置 + 服务器文件布局 + 发布流程（拷贝 goreleaser 产物）
- 新增"发布流程"章节（面向维护者）：`git tag v1.2.3 && git push origin v1.2.3` → CI 自动跑 goreleaser
- macOS symlink 限制说明
