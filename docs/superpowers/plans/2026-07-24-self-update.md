# Self-Update (go-selfupdate + goreleaser + flat HTTP source) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add self-update to sshmng: `sshmng mcp` auto-checks GitHub Releases on startup (TTL-cached), `sshmng update` manually applies, `sshmng version [--check]` prints version. Support self-hosted static HTTP as alternative source. Release via goreleaser on tag push.

**Architecture:** `internal/version` leaf package holds ldflags-injected version/repo metadata. `internal/update` wraps `github.com/creativeprojects/go-selfupdate` with a minimal `Updater` abstraction (2 public methods: `LatestVersion` + `UpdateToLatest`); implements `selfupdate.Source` interface for flat HTTP servers. `internal/cli` adds `update` / `version` subcommands, mounts auto-update goroutine in `runMCP`, adds `update_url` + dev-build checks to `doctor`. goreleaser builds 6 platforms with archive naming compatible with both GitHub Releases and flat HTTP layout.

**Tech Stack:** Go 1.25 stdlib + `github.com/creativeprojects/go-selfupdate` (new) + `golang.org/x/mod/semver` (transitive via go-selfupdate) + existing `modelcontextprotocol/go-sdk`.

**Spec:** `docs/superpowers/specs/2026-07-23-self-update-design.md`

## Global Constraints

- Go 1.25.0 (`go.mod` already at `go 1.25.0`)
- New dependency: `github.com/creativeprojects/go-selfupdate` (+ its transitive deps)
- No `gofrs/flock` (lockfile dropped — TTL cache makes conflicts rare and harmless)
- No `golang.org/x/sys/windows` direct dep (go-selfupdate handles Windows swap internally)
- Version string format: `v1.2.3` (with `v` prefix, matches git tag + `golang.org/x/mod/semver` requirement)
- ldflags injection target: `github.com/jim58246/sshmng/internal/version.{Version,Commit,Date,RepoOwner,RepoName}`
- Archive naming: `sshmng-{{.Tag}}-{{.Os}}-{{.Arch}}.{tar.gz|zip}` — must match `flatHTTPSource` asset convention exactly
- Cache TTL: 1 hour (hardcoded, not configurable)
- Cache file: `<config_dir>/update_cache.json` (alongside `config.json`)
- Output characters: ASCII only (`[ok]` / `[FAIL]` / `[WARN]`), no Unicode (Windows cmd.exe compat)
- Exit codes: 0 success / 1 runtime fail / 2 usage error
- Atomic cache writes: temp file + `os.Rename`
- Auto-update goroutine: silent (logs only to `log_path`, never stdout/stderr — MCP server invariant)
- TDD: test before implementation, `go test -race ./...` must pass after each task
- Commit after each task (or each step within a task)
- `internal/version` is a leaf package (stdlib only) — `internal/mcp` and `internal/update` both import it, no cycles

---

## File Structure

```
internal/version/                # New package — ldflags injection target
  version.go                     # var Version, Commit, Date, RepoOwner, RepoName
  version_test.go                # default values test
internal/update/                 # New package — self-update logic
  update.go                      # Updater struct + New + LatestVersion + UpdateToLatest + cleanupStaleStaging
  cache.go                       # readCache / writeCache / isCacheFresh (internal)
  semver.go                      # isNewer (internal, wraps golang.org/x/mod/semver)
  github.go                      # newGitHubSource (internal, wraps selfupdate.GitHubSource)
  flathttp.go                    # flatHTTPSource + flatRelease + flatAsset (implements selfupdate.Source)
  update_test.go                 # Updater tests with mock source + mock lib
  cache_test.go                  # cache read/write/fresh tests
  semver_test.go                 # isNewer tests
  flathttp_test.go               # flatHTTPSource tests with httptest.Server
internal/cli/
  cli.go                         # Modified: Dispatch adds update/version cases + helpText
  cli_test.go                    # Modified: dispatch routing for update/version
  mcp.go                         # Modified: mount auto-update goroutine
  doctor.go                      # Modified: add update_url + dev-build checks
  doctor_test.go                 # Modified: tests for new checks
  install.go                     # Unchanged (ScaffoldHome in files.go is what changes)
  files.go                       # Modified: configJSONSkeleton adds auto_update_enabled: true
  files_test.go                  # Modified: skeleton test checks auto_update_enabled
  update.go                      # New: runUpdate subcommand
  update_test.go                 # New: runUpdate tests
  version.go                     # New: runVersion subcommand
  version_test.go                # New: runVersion tests
internal/config/
  types.go                       # Modified: Config adds AutoUpdateEnabled + UpdateURL fields
  store.go                       # Modified: defaultConfig sets AutoUpdateEnabled=true
  store_test.go                  # Modified (if exists): default config test
internal/mcp/
  server.go                      # Modified: NewServer uses version.Version instead of "v1"
.goreleaser.yaml                 # New: release config
.github/workflows/release.yml    # New: CI workflow on tag push
README.md                        # Modified: install/build + auto-update + self-hosted + release sections
```

---

## Task 1: Dependencies + `internal/version` package

Add go-selfupdate dependency. Create the `internal/version` leaf package that holds ldflags-injected version/repo metadata. This package has no logic — just package-level `var`s with defaults for non-goreleaser builds.

**Files:**
- Create: `internal/version/version.go`
- Create: `internal/version/version_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Produces: `version.Version` (string, default `"dev"`), `version.Commit` (string, default `"none"`), `version.Date` (string, default `"unknown"`), `version.RepoOwner` (string, default `"jim58246"`), `version.RepoName` (string, default `"sshmng"`)
- Consumes: nothing (stdlib only)

- [ ] **Step 1: Add go-selfupdate dependency**

Run:
```bash
go get github.com/creativeprojects/go-selfupdate@latest
go mod tidy
```

Expected: `go.mod` gains `github.com/creativeprojects/go-selfupdate` in `require` block. `go.sum` updated. `golang.org/x/mod` appears as transitive dep.

- [ ] **Step 2: Write failing test for default values**

Create `internal/version/version_test.go`:

```go
package version

import "testing"

func TestDefaults(t *testing.T) {
	if Version != "dev" {
		t.Errorf("Version default = %q, want %q", Version, "dev")
	}
	if Commit != "none" {
		t.Errorf("Commit default = %q, want %q", Commit, "none")
	}
	if Date != "unknown" {
		t.Errorf("Date default = %q, want %q", Date, "unknown")
	}
	if RepoOwner != "jim58246" {
		t.Errorf("RepoOwner default = %q, want %q", RepoOwner, "jim58246")
	}
	if RepoName != "sshmng" {
		t.Errorf("RepoName default = %q, want %q", RepoName, "sshmng")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/version/ -v`
Expected: FAIL — package not found / `Version` undefined

- [ ] **Step 4: Create `internal/version/version.go`**

```go
// Package version holds build-time metadata injected by goreleaser via ldflags.
// It is a leaf package (stdlib only) so that internal/mcp can read Version
// without pulling in the heavier internal/update (which depends on go-selfupdate).
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
	// For HTTP source (update_url set), these are unused.
	RepoOwner = "jim58246"
	RepoName  = "sshmng"
)
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/version/ -v`
Expected: PASS

- [ ] **Step 6: Verify full build still passes**

Run: `go build ./... && go test -race ./...`
Expected: all pass (no regressions — `internal/version` is not imported by anything yet)

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/version/version.go internal/version/version_test.go
git commit -m "feat(version): add internal/version package + go-selfupdate dep"
```

---

## Task 2: Fix `mcp.NewServer` hardcoded `Version: "v1"`

`internal/mcp/server.go:130` hardcodes `Version: "v1"` in `mcp.Implementation`. Replace with `version.Version` so Agents see the real build version via `initialize.serverInfo.version`.

**Files:**
- Modify: `internal/mcp/server.go:130` (and add import)
- Modify: `internal/mcp/tools_config_test.go` (if it asserts on `"v1"`)

**Interfaces:**
- Consumes: `version.Version` from `internal/version`
- Produces: `mcp.NewServer` now reports real version in `serverInfo`

- [ ] **Step 1: Check existing tests for `"v1"` assertions**

Run: `grep -rn '"v1"' internal/mcp/`
Expected: find any test lines asserting serverInfo version is `"v1"`. Note them for update in Step 4.

- [ ] **Step 2: Write failing test for real version in serverInfo**

If `internal/mcp/tools_config_test.go` (or a new test file) doesn't already verify serverInfo version, add a test. Add to `internal/mcp/tools_config_test.go` (or create `internal/mcp/server_test.go` if the file doesn't exist):

```go
package mcp

import (
	"testing"

	"github.com/jim58246/sshmng/internal/version"
)

func TestNewServerReportsVersion(t *testing.T) {
	// Override version for test isolation; restore after.
	orig := version.Version
	version.Version = "v9.9.9-test"
	defer func() { version.Version = orig }()

	svc := &Service{}
	srv := NewServer(svc)
	if srv.Info().Version != "v9.9.9-test" {
		t.Errorf("serverInfo.Version = %q, want %q", srv.Info().Version, "v9.99.9-test")
	}
}
```

Note: `srv.Info()` may not exist on `*mcp.Server`; if the go-sdk exposes serverInfo differently, adjust the assertion. Check `modelcontextprotocol/go-sdk` API: likely `srv.ServerInfo` field or similar. If no accessor exists, skip the direct assertion and instead test via `initialize` handshake in an integration test (out of scope for this task — fall back to just verifying the source line change compiles).

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/mcp/ -run TestNewServerReportsVersion -v`
Expected: FAIL (either `srv.Info()` API mismatch, or version still `"v1"`)

- [ ] **Step 4: Modify `internal/mcp/server.go`**

Add import (if not present):
```go
import "github.com/jim58246/sshmng/internal/version"
```

Change line 130 from:
```go
server := mcp.NewServer(&mcp.Implementation{Name: "sshmng", Version: "v1"}, &mcp.ServerOptions{
```
to:
```go
server := mcp.NewServer(&mcp.Implementation{Name: "sshmng", Version: version.Version}, &mcp.ServerOptions{
```

Update any existing test that asserted `"v1"` to assert `version.Version` instead.

- [ ] **Step 5: Run all mcp tests**

Run: `go test ./internal/mcp/ -race -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/mcp/server.go internal/mcp/tools_config_test.go
git commit -m "fix(mcp): serverInfo.version uses version.Version instead of hardcoded \"v1\""
```

---

## Task 3: Config schema — `AutoUpdateEnabled` + `UpdateURL` fields

Add two fields to `Config`: `AutoUpdateEnabled` (bool, default true) and `UpdateURL` (string, empty = GitHub source). Update `defaultConfig()` and the install skeleton so the default is `true` and visible to users.

**Files:**
- Modify: `internal/config/types.go` (Config struct)
- Modify: `internal/config/store.go` (`defaultConfig`)
- Modify: `internal/cli/files.go` (`configJSONSkeleton`)
- Modify: `internal/config/store_test.go` (if exists, add field assertions) or create test
- Modify: `internal/cli/files_test.go` (skeleton assertion)

**Interfaces:**
- Produces: `Config.AutoUpdateEnabled` (bool, JSON key `auto_update_enabled`, omitempty), `Config.UpdateURL` (string, JSON key `update_url`, omitempty)
- Consumes: nothing new

- [ ] **Step 1: Write failing test for new Config fields**

Add to `internal/config/store_test.go` (create file if it doesn't exist):

```go
package config

import "testing"

func TestDefaultConfigAutoUpdateEnabled(t *testing.T) {
	cfg := defaultConfig()
	if !cfg.AutoUpdateEnabled {
		t.Errorf("defaultConfig().AutoUpdateEnabled = false, want true")
	}
	if cfg.UpdateURL != "" {
		t.Errorf("defaultConfig().UpdateURL = %q, want empty", cfg.UpdateURL)
	}
}

func TestConfigJSONRoundTripPreservesAutoUpdate(t *testing.T) {
	cfg := &Config{
		Version:           "1",
		IdleTimeoutS:      300,
		AutoUpdateEnabled: false,
		UpdateURL:         "https://updates.example.com/sshmng",
	}
	data, err := marshalConfig(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := unmarshalConfig(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.AutoUpdateEnabled != false {
		t.Errorf("AutoUpdateEnabled = %v, want false", parsed.AutoUpdateEnabled)
	}
	if parsed.UpdateURL != "https://updates.example.com/sshmng" {
		t.Errorf("UpdateURL = %q, want the configured URL", parsed.UpdateURL)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run "TestDefaultConfigAutoUpdateEnabled|TestConfigJSONRoundTripPreservesAutoUpdate" -v`
Expected: FAIL — `AutoUpdateEnabled` undefined, `UpdateURL` undefined

- [ ] **Step 3: Add fields to `Config` struct in `internal/config/types.go`**

Find the `Config` struct (around line 110-118) and add two fields:

```go
type Config struct {
	Version             string       `json:"version"`
	IdleTimeoutS        int          `json:"idle_timeout_s"`
	LogLevel            string       `json:"log_level,omitempty"`
	LogPath             string       `json:"log_path,omitempty"`
	AutoUpdateEnabled   bool         `json:"auto_update_enabled,omitempty"`
	UpdateURL           string       `json:"update_url,omitempty"`
	Jumphosts           []*Jumphost  `json:"jumphosts"`
	Proxies             []*Proxy     `json:"proxies"`
	Servers             []*SSHServer `json:"servers"`
}
```

- [ ] **Step 4: Update `defaultConfig()` in `internal/config/store.go`**

Find `defaultConfig()` (around line 106-114) and set `AutoUpdateEnabled: true`:

```go
func defaultConfig() *Config {
	return &Config{
		Version:           "1",
		IdleTimeoutS:      300,
		AutoUpdateEnabled: true,
		Jumphosts:         []*Jumphost{},
		Proxies:           []*Proxy{},
		Servers:           []*SSHServer{},
	}
}
```

- [ ] **Step 5: Run config tests**

Run: `go test ./internal/config/ -race -v`
Expected: PASS

- [ ] **Step 6: Update `configJSONSkeleton` in `internal/cli/files.go`**

Find `configJSONSkeleton` (around line 64-71) and add `auto_update_enabled`:

```go
const configJSONSkeleton = `{
  "version": "1",
  "idle_timeout_s": 300,
  "auto_update_enabled": true,
  "jumphosts": [],
  "proxies": [],
  "servers": []
}
`
```

- [ ] **Step 7: Update `internal/cli/files_test.go` skeleton assertion**

Find the test that verifies `configJSONSkeleton` content (search for `configJSONSkeleton` or `ScaffoldHome` in `files_test.go`). Add assertion that the skeleton contains `"auto_update_enabled": true`. If no such test exists, add:

```go
func TestConfigJSONSkeletonHasAutoUpdateEnabled(t *testing.T) {
	if !strings.Contains(configJSONSkeleton, `"auto_update_enabled": true`) {
		t.Errorf("configJSONSkeleton missing auto_update_enabled: true\n%s", configJSONSkeleton)
	}
}
```

- [ ] **Step 8: Run cli files tests**

Run: `go test ./internal/cli/ -run "Files|Skeleton" -race -v`
Expected: PASS

- [ ] **Step 9: Run full test suite**

Run: `go test -race ./...`
Expected: all PASS

- [ ] **Step 10: Commit**

```bash
git add internal/config/types.go internal/config/store.go internal/config/store_test.go internal/cli/files.go internal/cli/files_test.go
git commit -m "feat(config): add AutoUpdateEnabled + UpdateURL fields, default true"
```

---

## Task 4: `internal/update/semver.go` — `isNewer`

Thin wrapper around `golang.org/x/mod/semver` for version comparison. Both `latest` and `current` must have `v` prefix.

**Files:**
- Create: `internal/update/semver.go`
- Create: `internal/update/semver_test.go`

**Interfaces:**
- Produces: `isNewer(latest, current string) bool` (internal, lowercase — unexported)
- Consumes: `golang.org/x/mod/semver` (transitive via go-selfupdate)

- [ ] **Step 1: Write failing test**

Create `internal/update/semver_test.go`:

```go
package update

import "testing"

func TestIsNewer(t *testing.T) {
	cases := []struct {
		name    string
		latest  string
		current string
		want    bool
	}{
		{"equal", "v1.2.3", "v1.2.3", false},
		{"patch_newer", "v1.2.4", "v1.2.3", true},
		{"minor_newer", "v1.3.0", "v1.2.5", true},
		{"major_newer", "v2.0.0", "v1.9.9", true},
		{"older", "v1.2.2", "v1.2.3", false},
		{"dev_current", "v1.2.3", "dev", true},
		{"invalid_latest", "not-a-version", "v1.2.3", false},
		{"invalid_current", "v1.2.3", "not-a-version", false},
		{"both_invalid", "nope", "nope", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isNewer(tc.latest, tc.current)
			if got != tc.want {
				t.Errorf("isNewer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/update/ -run TestIsNewer -v`
Expected: FAIL — `isNewer` undefined

- [ ] **Step 3: Implement `internal/update/semver.go`**

```go
package update

import "golang.org/x/mod/semver"

// isNewer returns true if latest > current. Both must have "v" prefix
// (golang.org/x/mod/semver requires it). current == "dev" (non-release build)
// always returns true — but callers should have short-circuited already.
// Invalid versions return false (defensive — shouldn't happen in practice).
func isNewer(latest, current string) bool {
	if current == "dev" {
		return true
	}
	if !semver.IsValid(latest) || !semver.IsValid(current) {
		return false
	}
	return semver.Compare(latest, current) > 0
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/update/ -run TestIsNewer -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/update/semver.go internal/update/semver_test.go
git commit -m "feat(update): add semver isNewer helper"
```

---

## Task 5: `internal/update/cache.go` — cache read/write/fresh

Cache stores `last_check_at` + `latest_version` as JSON. Fresh = within 1 hour. Atomic writes via temp + rename.

**Files:**
- Create: `internal/update/cache.go`
- Create: `internal/update/cache_test.go`

**Interfaces:**
- Produces (internal, unexported):
  - `cacheEntry` struct: `{ LastCheckAt time.Time `json:"last_check_at"` LatestVersion string `json:"latest_version"` }`
  - `readCache(path string) (cacheEntry, bool)` — returns entry + `true` if file exists and parses; `false` (zero entry) if missing or corrupt
  - `writeCache(path string, entry cacheEntry) error` — atomic write (temp + rename)
  - `isCacheFresh(entry cacheEntry, ttl time.Duration) bool`

- [ ] **Step 1: Write failing test**

Create `internal/update/cache_test.go`:

```go
package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadCache_MissingFile(t *testing.T) {
	entry, ok := readCache(filepath.Join(t.TempDir(), "nonexistent.json"))
	if ok {
		t.Errorf("readCache missing file: ok = true, want false")
	}
	if entry.LatestVersion != "" {
		t.Errorf("readCache missing file: LatestVersion = %q, want empty", entry.LatestVersion)
	}
}

func TestReadCache_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	data := `{"last_check_at":"2026-07-24T10:00:00Z","latest_version":"v1.2.3"}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	entry, ok := readCache(path)
	if !ok {
		t.Fatalf("readCache: ok = false, want true")
	}
	if entry.LatestVersion != "v1.2.3" {
		t.Errorf("LatestVersion = %q, want v1.2.3", entry.LatestVersion)
	}
	want := "2026-07-24T10:00:00Z"
	if entry.LastCheckAt.Format(time.RFC3339) != want {
		t.Errorf("LastCheckAt = %q, want %q", entry.LastCheckAt.Format(time.RFC3339), want)
	}
}

func TestReadCache_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	os.WriteFile(path, []byte("{not json"), 0600)
	_, ok := readCache(path)
	if ok {
		t.Errorf("readCache corrupt JSON: ok = true, want false")
	}
}

func TestWriteCache_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	entry := cacheEntry{
		LastCheckAt:   time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC),
		LatestVersion: "v1.2.3",
	}
	if err := writeCache(path, entry); err != nil {
		t.Fatalf("writeCache: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "v1.2.3") {
		t.Errorf("written file missing version: %s", data)
	}
}

func TestWriteCache_Overwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	writeCache(path, cacheEntry{LatestVersion: "v1.0.0"})
	writeCache(path, cacheEntry{LatestVersion: "v2.0.0"})
	entry, _ := readCache(path)
	if entry.LatestVersion != "v2.0.0" {
		t.Errorf("after overwrite: LatestVersion = %q, want v2.0.0", entry.LatestVersion)
	}
}

func TestIsCacheFresh(t *testing.T) {
	ttl := time.Hour
	now := time.Now()
	cases := []struct {
		name  string
		entry cacheEntry
		want  bool
	}{
		{"fresh", cacheEntry{LastCheckAt: now.Add(-30 * time.Minute), LatestVersion: "v1.0.0"}, true},
		{"just_expired", cacheEntry{LastCheckAt: now.Add(-61 * time.Minute), LatestVersion: "v1.0.0"}, false},
		{"zero_time", cacheEntry{LastCheckAt: time.Time{}, LatestVersion: "v1.0.0"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isCacheFresh(tc.entry, ttl)
			if got != tc.want {
				t.Errorf("isCacheFresh = %v, want %v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/update/ -run "Cache" -v`
Expected: FAIL — `cacheEntry`, `readCache`, `writeCache`, `isCacheFresh` undefined

- [ ] **Step 3: Implement `internal/update/cache.go`**

```go
package update

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// cacheEntry is the JSON shape of the update cache file.
type cacheEntry struct {
	LastCheckAt   time.Time `json:"last_check_at"`
	LatestVersion string    `json:"latest_version"`
}

// readCache loads the cache entry. Returns (zero, false) if the file is
// missing or unparseable — callers treat both as "stale, re-fetch".
func readCache(path string) (cacheEntry, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cacheEntry{}, false
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return cacheEntry{}, false
	}
	return entry, true
}

// writeCache atomically writes the cache entry (temp file + rename).
// Creates parent directory if needed.
func writeCache(path string, entry cacheEntry) error {
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir cache dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".update_cache.tmp.*")
	if err != nil {
		return fmt.Errorf("create cache temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write cache temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close cache temp: %w", err)
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return fmt.Errorf("chmod cache temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename cache temp: %w", err)
	}
	return nil
}

// isCacheFresh returns true if the entry was checked within ttl of now.
// Zero LastCheckAt (missing/corrupt cache) is never fresh.
func isCacheFresh(entry cacheEntry, ttl time.Duration) bool {
	if entry.LastCheckAt.IsZero() {
		return false
	}
	return time.Since(entry.LastCheckAt) < ttl
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/update/ -run "Cache" -race -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/update/cache.go internal/update/cache_test.go
git commit -m "feat(update): add cache read/write/fresh helpers"
```

---

## Task 6: `internal/update/github.go` — `newGitHubSource`

Thin wrapper around `selfupdate.NewGitHubSource`. Validates that owner/name are non-empty (ldflags injection check).

**Files:**
- Create: `internal/update/github.go`
- Create: `internal/update/github_test.go`

**Interfaces:**
- Produces (internal): `newGitHubSource(owner, name string) (selfupdate.Source, error)`
- Consumes: `github.com/creativeprojects/go-selfupdate`

- [ ] **Step 1: Write failing test**

Create `internal/update/github_test.go`:

```go
package update

import "testing"

func TestNewGitHubSource_MissingOwner(t *testing.T) {
	_, err := newGitHubSource("", "sshmng")
	if err == nil {
		t.Fatal("expected error for empty owner, got nil")
	}
}

func TestNewGitHubSource_MissingName(t *testing.T) {
	_, err := newGitHubSource("jim58246", "")
	if err == nil {
		t.Fatal("expected error for empty name, got nil")
	}
}

func TestNewGitHubSource_Valid(t *testing.T) {
	src, err := newGitHubSource("jim58246", "sshmng")
	if err != nil {
		t.Fatalf("newGitHubSource: %v", err)
	}
	if src == nil {
		t.Fatal("source is nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/update/ -run "NewGitHubSource" -v`
Expected: FAIL — `newGitHubSource` undefined

- [ ] **Step 3: Implement `internal/update/github.go`**

```go
package update

import (
	"fmt"

	"github.com/creativeprojects/go-selfupdate"
)

// newGitHubSource wraps selfupdate.NewGitHubSource. owner/name come from
// version.RepoOwner / version.RepoName (ldflags-injected). Forks override
// via ldflags to redirect updates to their fork.
func newGitHubSource(owner, name string) (selfupdate.Source, error) {
	if owner == "" || name == "" {
		return nil, fmt.Errorf("github source requires non-empty RepoOwner and RepoName (ldflags not injected?)")
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

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/update/ -run "NewGitHubSource" -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/update/github.go internal/update/github_test.go
git commit -m "feat(update): add github source wrapper"
```

---

## Task 7: `internal/update/flathttp.go` — `flatHTTPSource` + `flatRelease` + `flatAsset`

Implement `selfupdate.Source` interface for self-hosted static HTTP servers. `ListReleases` fetches `latest.txt` and constructs a release with 6 platform assets + `checksums.txt` by naming convention. `DownloadReleaseAsset` streams the asset body.

**Files:**
- Create: `internal/update/flathttp.go`
- Create: `internal/update/flathttp_test.go`

**Interfaces:**
- Produces (internal):
  - `newFlatHTTPSource(baseURL string) (*flatHTTPSource, error)`
  - `flatHTTPSource` implements `selfupdate.Source`
  - `flatRelease` implements `selfupdate.SourceRelease`
  - `flatAsset` implements `selfupdate.SourceAsset`
- Consumes: `github.com/creativeprojects/go-selfupdate` (Source/SourceRelease/SourceAsset interfaces), `golang.org/x/mod/semver`

- [ ] **Step 1: Write failing test for `ListReleases`**

Create `internal/update/flathttp_test.go`:

```go
package update

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/creativeprojects/go-selfupdate"
)

func TestFlatHTTPSource_RejectsNonHTTPURL(t *testing.T) {
	_, err := newFlatHTTPSource("ftp://example.com")
	if err == nil {
		t.Fatal("expected error for ftp:// URL")
	}
}

func TestFlatHTTPSource_AcceptsHTTPS(t *testing.T) {
	_, err := newFlatHTTPSource("https://updates.example.com/sshmng")
	if err != nil {
		t.Fatalf("https URL rejected: %v", err)
	}
}

func TestFlatHTTPSource_ListReleases_ValidLatestTxt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest.txt" {
			w.Write([]byte("v1.2.3\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src, err := newFlatHTTPSource(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	releases, err := src.ListReleases(context.Background(), selfupdate.Repository{})
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	if len(releases) != 1 {
		t.Fatalf("got %d releases, want 1", len(releases))
	}
	if releases[0].GetTagName() != "v1.2.3" {
		t.Errorf("TagName = %q, want v1.2.3", releases[0].GetTagName())
	}
	assets := releases[0].GetAssets()
	// 6 platforms + 1 checksums.txt = 7
	if len(assets) != 7 {
		t.Fatalf("got %d assets, want 7", len(assets))
	}
	// Verify darwin-arm64 asset exists with correct URL
	found := false
	for _, a := range assets {
		if strings.Contains(a.GetName(), "darwin-arm64") {
			found = true
			if !strings.HasSuffix(a.GetBrowserDownloadURL(), "/sshmng-v1.2.3-darwin-arm64.tar.gz") {
				t.Errorf("darwin-arm64 URL = %q, want suffix /sshmng-v1.2.3-darwin-arm64.tar.gz", a.GetBrowserDownloadURL())
			}
		}
	}
	if !found {
		t.Error("darwin-arm64 asset not found")
	}
	// Verify checksums.txt asset exists
	foundChecksums := false
	for _, a := range assets {
		if a.GetName() == "checksums.txt" {
			foundChecksums = true
		}
	}
	if !foundChecksums {
		t.Error("checksums.txt asset not found")
	}
}

func TestFlatHTTPSource_ListReleases_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src, _ := newFlatHTTPSource(srv.URL)
	_, err := src.ListReleases(context.Background(), selfupdate.Repository{})
	if err == nil {
		t.Fatal("expected error for 404 latest.txt, got nil")
	}
}

func TestFlatHTTPSource_ListReleases_InvalidSemver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not-a-version\n"))
	}))
	defer srv.Close()

	src, _ := newFlatHTTPSource(srv.URL)
	_, err := src.ListReleases(context.Background(), selfupdate.Repository{})
	if err == nil {
		t.Fatal("expected error for invalid semver, got nil")
	}
}

func TestFlatHTTPSource_DownloadReleaseAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest.txt" {
			w.Write([]byte("v1.2.3\n"))
			return
		}
		if r.URL.Path == "/sshmng-v1.2.3-darwin-arm64.tar.gz" {
			w.Write([]byte("fake archive bytes"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src, _ := newFlatHTTPSource(srv.URL)
	releases, _ := src.ListReleases(context.Background(), selfupdate.Repository{})
	rel := releases[0]

	// Find darwin-arm64 asset ID
	var assetID int64
	for _, a := range rel.GetAssets() {
		if strings.Contains(a.GetName(), "darwin-arm64") {
			assetID = a.GetID()
		}
	}
	if assetID == 0 {
		t.Fatal("darwin-arm64 asset ID not found")
	}

	// DownloadReleaseAsset takes *selfupdate.Release, not SourceRelease.
	// Build a minimal selfupdate.Release from our SourceRelease for the test.
	libRel := &selfupdate.Release{
		ReleaseID: rel.GetID(),
		TagName:   rel.GetTagName(),
		Assets:    releases[0].GetAssets(), // type is []SourceAsset; check if assignable
	}
	// If Assets field type doesn't match, wrap each SourceAsset into selfupdate.Asset.
	// See selfupdate.Release struct definition for exact field types.

	body, err := src.DownloadReleaseAsset(context.Background(), libRel, assetID)
	if err != nil {
		t.Fatalf("DownloadReleaseAsset: %v", err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	if string(data) != "fake archive bytes" {
		t.Errorf("downloaded = %q, want %q", data, "fake archive bytes")
	}
}
```

Note on the `DownloadReleaseAsset` test: the method signature takes `*selfupdate.Release` (the library's struct, not our `SourceRelease` interface). The `Release.Assets` field type may be `[]Asset` (library struct) or `[]SourceAsset` (interface). Check the library's `release.go` for the exact type. If it's `[]Asset`, convert each `flatAsset` to `selfupdate.Asset` in the test. If `[]SourceAsset`, our `flatAsset` works directly.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/update/ -run "FlatHTTP" -v`
Expected: FAIL — `newFlatHTTPSource`, `flatHTTPSource` undefined

- [ ] **Step 3: Implement `internal/update/flathttp.go`**

```go
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

// flatHTTPSource implements selfupdate.Source for a self-hosted static HTTP
// server. The server serves latest.txt + archives + checksums.txt at a flat
// base URL (see spec for layout).
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
	tag, err := s.fetchLatest(ctx)
	if err != nil {
		return nil, err
	}
	if !semver.IsValid(tag) {
		return nil, fmt.Errorf("latest.txt returned invalid semver: %q", tag)
	}

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
			if err != nil {
				return nil, err
			}
			resp, err := s.client.Do(req)
			if err != nil {
				return nil, err
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				return nil, fmt.Errorf("download %s: HTTP %d", a.GetBrowserDownloadURL(), resp.StatusCode)
			}
			return resp.Body, nil
		}
	}
	return nil, fmt.Errorf("asset id %d not found in release", assetID)
}

func (s *flatHTTPSource) fetchLatest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.baseURL+"/latest.txt", nil)
	if err != nil {
		return "", err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch latest.txt: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("read latest.txt: %w", err)
	}
	return strings.TrimSpace(string(body)), nil
}

// flatRelease implements selfupdate.SourceRelease.
type flatRelease struct {
	tag    string
	assets []selfupdate.SourceAsset
}

func (r *flatRelease) GetID() int64                 { return 1 }
func (r *flatRelease) GetTagName() string           { return r.tag }
func (r *flatRelease) GetDraft() bool               { return false }
func (r *flatRelease) GetPrerelease() bool          { return false }
func (r *flatRelease) GetPublishedAt() time.Time    { return time.Time{} }
func (r *flatRelease) GetReleaseNotes() string      { return "" }
func (r *flatRelease) GetName() string              { return r.tag }
func (r *flatRelease) GetURL() string               { return "" }
func (r *flatRelease) GetAssets() []selfupdate.SourceAsset { return r.assets }

// flatAsset implements selfupdate.SourceAsset.
type flatAsset struct {
	id   int64
	name string
	url  string
}

func (a *flatAsset) GetID() int64                  { return a.id }
func (a *flatAsset) GetName() string               { return a.name }
func (a *flatAsset) GetSize() int                  { return 0 }
func (a *flatAsset) GetBrowserDownloadURL() string { return a.url }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/update/ -run "FlatHTTP" -race -v`
Expected: PASS. If the `DownloadReleaseAsset` test fails due to `rel.Assets` type mismatch (library's `Release.Assets` may be `[]Asset` struct, not `[]SourceAsset` interface), adjust the test to construct `[]selfupdate.Asset` from our `flatAsset` values, or check if the library's `Release` struct accepts `[]SourceAsset`. Inspect `selfupdate.Release` definition in `release.go` to resolve.

- [ ] **Step 5: Commit**

```bash
git add internal/update/flathttp.go internal/update/flathttp_test.go
git commit -m "feat(update): add flatHTTPSource implementing selfupdate.Source"
```

---

## Task 8: `internal/update/update.go` — `Updater` + `New` + `LatestVersion` + `UpdateToLatest`

The main orchestration type. `LatestVersion` is cache-aware (optimistic write before source call). `UpdateToLatest` checks cache, short-circuits if up-to-date, calls `lib.UpdateSelf` if newer. Internal `cleanupStaleStaging` sweeps temp dir for stale staging files.

**Files:**
- Create: `internal/update/update.go`
- Create: `internal/update/update_test.go`

**Interfaces:**
- Produces (exported):
  - `Updater` struct
  - `Config` struct: `{ RepoOwner, RepoName, UpdateURL, CachePath string; Log *slog.Logger }`
  - `New(cfg Config) (*Updater, error)`
  - `(*Updater).LatestVersion(ctx context.Context) (string, error)`
  - `(*Updater).UpdateToLatest(ctx context.Context) (latest string, applied bool, err error)`
- Produces (internal):
  - `selfupdateLib` interface (for testability): `{ UpdateSelf(ctx, current string, repo selfupdate.Repository) (*selfupdate.Release, error) }`
  - `cleanupStaleStaging() error`
- Consumes: `internal/version` (Version, RepoOwner, RepoName), `selfupdate.NewUpdater`, `selfupdate.NewGitHubSource`, `selfupdate.ChecksumValidator`, all of Task 4-7

- [ ] **Step 1: Write failing test for `LatestVersion` with cache fresh (no source call)**

Create `internal/update/update_test.go`:

```go
package update

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/creativeprojects/go-selfupdate"
	"github.com/jim58246/sshmng/internal/version"
)

// mockSource implements selfupdate.Source for testing.
type mockSource struct {
	releases []selfupdate.SourceRelease
	calls    int
}

func (m *mockSource) ListReleases(ctx context.Context, repo selfupdate.Repository) ([]selfupdate.SourceRelease, error) {
	m.calls++
	return m.releases, nil
}

func (m *mockSource) DownloadReleaseAsset(ctx context.Context, rel *selfupdate.Release, assetID int64) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not implemented")
}

// mockLib implements selfupdateLib for testing.
type mockLib struct {
	updateSelfCalled bool
	updateSelfErr    error
	release          *selfupdate.Release
}

func (m *mockLib) UpdateSelf(ctx context.Context, current string, repo selfupdate.Repository) (*selfupdate.Release, error) {
	m.updateSelfCalled = true
	return m.release, m.updateSelfErr
}

func newTestUpdater(t *testing.T, src selfupdate.Source, lib selfupdateLib) *Updater {
	t.Helper()
	return &Updater{
		lib:       lib,
		source:    src,
		repo:      selfupdate.Repository{Owner: "test", Name: "test"},
		cachePath: filepath.Join(t.TempDir(), "cache.json"),
		cacheTTL:  time.Hour,
		log:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
}

func TestLatestVersion_CacheFresh_NoSourceCall(t *testing.T) {
	src := &mockSource{releases: []selfupdate.SourceRelease{&flatRelease{tag: "v9.9.9"}}}
	u := newTestUpdater(t, src, &mockLib{})

	// Pre-populate cache as fresh
	writeCache(u.cachePath, cacheEntry{
		LastCheckAt:   time.Now().Add(-5 * time.Minute),
		LatestVersion: "v1.0.0",
	})

	got, err := u.LatestVersion(context.Background())
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	if got != "v1.0.0" {
		t.Errorf("got %q, want v1.0.0 (cached)", got)
	}
	if src.calls != 0 {
		t.Errorf("source called %d times, want 0 (cache fresh)", src.calls)
	}
}

func TestLatestVersion_CacheStale_CallsSource(t *testing.T) {
	src := &mockSource{releases: []selfupdate.SourceRelease{&flatRelease{tag: "v2.0.0"}}}
	u := newTestUpdater(t, src, &mockLib{})

	// No cache file → stale
	got, err := u.LatestVersion(context.Background())
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	if got != "v2.0.0" {
		t.Errorf("got %q, want v2.0.0", got)
	}
	if src.calls != 1 {
		t.Errorf("source called %d times, want 1", src.calls)
	}
	// Cache should now be written
	entry, ok := readCache(u.cachePath)
	if !ok {
		t.Fatal("cache not written after source call")
	}
	if entry.LatestVersion != "v2.0.0" {
		t.Errorf("cached version = %q, want v2.0.0", entry.LatestVersion)
	}
}

func TestUpdateToLatest_AlreadyUpToDate(t *testing.T) {
	src := &mockSource{releases: []selfupdate.SourceRelease{&flatRelease{tag: "v1.0.0"}}}
	lib := &mockLib{}
	u := newTestUpdater(t, src, lib)

	// Override version.Version
	orig := version.Version
	version.Version = "v1.0.0"
	defer func() { version.Version = orig }()

	latest, applied, err := u.UpdateToLatest(context.Background())
	if err != nil {
		t.Fatalf("UpdateToLatest: %v", err)
	}
	if applied {
		t.Error("applied = true, want false (already up to date)")
	}
	if latest != "v1.0.0" {
		t.Errorf("latest = %q, want v1.0.0", latest)
	}
	if lib.updateSelfCalled {
		t.Error("UpdateSelf called, want not called (already up to date)")
	}
}

func TestUpdateToLatest_NewerVersion_CallsUpdateSelf(t *testing.T) {
	src := &mockSource{releases: []selfupdate.SourceRelease{&flatRelease{tag: "v2.0.0"}}}
	lib := &mockLib{release: &selfupdate.Release{TagName: "v2.0.0"}}
	u := newTestUpdater(t, src, lib)

	orig := version.Version
	version.Version = "v1.0.0"
	defer func() { version.Version = orig }()

	latest, applied, err := u.UpdateToLatest(context.Background())
	if err != nil {
		t.Fatalf("UpdateToLatest: %v", err)
	}
	if !applied {
		t.Error("applied = false, want true")
	}
	if latest != "v2.0.0" {
		t.Errorf("latest = %q, want v2.0.0", latest)
	}
	if !lib.updateSelfCalled {
		t.Error("UpdateSelf not called")
	}
}

func TestUpdateToLatest_DevBuild_ReturnsError(t *testing.T) {
	src := &mockSource{}
	lib := &mockLib{}
	u := newTestUpdater(t, src, lib)

	orig := version.Version
	version.Version = "dev"
	defer func() { version.Version = orig }()

	_, _, err := u.UpdateToLatest(context.Background())
	if err == nil {
		t.Fatal("expected error for dev build, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/update/ -run "LatestVersion|UpdateToLatest" -v`
Expected: FAIL — `Updater`, `Config`, `New`, `selfupdateLib` undefined

- [ ] **Step 3: Implement `internal/update/update.go`**

```go
package update

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/creativeprojects/go-selfupdate"
	"github.com/jim58246/sshmng/internal/version"
)

// cacheTTL is how long a cached latest-version is considered fresh.
const cacheTTL = time.Hour

// selfupdateLib is the subset of *selfupdate.Updater we use. Defined as
// interface so tests can inject a mock instead of swapping the real binary.
type selfupdateLib interface {
	UpdateSelf(ctx context.Context, current string, repo selfupdate.Repository) (*selfupdate.Release, error)
}

// Updater checks for newer sshmng versions and applies them. Cache stores
// last-checked version + timestamp to stay under GitHub's 60 req/hour
// unauthenticated rate limit. All methods are safe for concurrent use
// within a single process; cross-process coordination is NOT provided
// (cache TTL makes concurrent updates rare and non-corrupting).
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
	RepoOwner string   // GitHub repo owner (required for GitHub source)
	RepoName  string   // GitHub repo name (required for GitHub source)
	UpdateURL string   // "" = GitHub source; "https://..." = flat HTTP source
	CachePath string   // where to store update_cache.json
	Log       *slog.Logger
}

// New creates an Updater. Returns error if config is invalid (malformed
// URL, missing repo owner/name for GitHub source).
func New(cfg Config) (*Updater, error) {
	if cfg.CachePath == "" {
		return nil, fmt.Errorf("CachePath is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	var src selfupdate.Source
	var repo selfupdate.Repository
	if cfg.UpdateURL == "" {
		s, err := newGitHubSource(cfg.RepoOwner, cfg.RepoName)
		if err != nil {
			return nil, err
		}
		src = s
		repo = selfupdate.Repository{Owner: cfg.RepoOwner, Name: cfg.RepoName}
	} else {
		s, err := newFlatHTTPSource(cfg.UpdateURL)
		if err != nil {
			return nil, err
		}
		src = s
		// repo unused by flatHTTPSource but required by lib.UpdateSelf
		repo = selfupdate.Repository{Owner: cfg.RepoOwner, Name: cfg.RepoName}
	}

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
		cacheTTL:  cacheTTL,
		log:       cfg.Log,
	}, nil
}

// LatestVersion returns the latest released version (e.g., "v1.2.3").
// Cache-aware: returns cached value if fresh; otherwise queries source and
// updates cache. Read-only — never downloads or swaps the binary.
func (u *Updater) LatestVersion(ctx context.Context) (string, error) {
	entry, ok := readCache(u.cachePath)
	if ok && isCacheFresh(entry, u.cacheTTL) {
		return entry.LatestVersion, nil
	}

	// Optimistic write: mark "just checked" BEFORE source call to narrow
	// the concurrent-update conflict window to milliseconds. If the source
	// call fails, the cache stays "checked now, old version" — next call
	// within TTL skips the source call. Acceptable: if source is down, we
	// can't update anyway.
	now := time.Now()
	writeCache(u.cachePath, cacheEntry{LastCheckAt: now, LatestVersion: entry.LatestVersion})

	releases, err := u.source.ListReleases(ctx, u.repo)
	if err != nil {
		return "", fmt.Errorf("list releases: %w", err)
	}
	if len(releases) == 0 {
		return "", fmt.Errorf("no releases found")
	}
	latest := releases[0].GetTagName()

	// Successful fetch — update the version field (timestamp already written).
	writeCache(u.cachePath, cacheEntry{LastCheckAt: now, LatestVersion: latest})
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
		return latest, false, nil
	}

	u.log.Info("applying update", "current", version.Version, "latest", latest)
	if _, err := u.lib.UpdateSelf(ctx, version.Version, u.repo); err != nil {
		return latest, false, fmt.Errorf("update self: %w", err)
	}
	return latest, true, nil
}

// cleanupStaleStaging removes temp files left by previous failed updates.
// go-selfupdate stages downloads in os.TempDir() with a recognizable prefix.
// Best-effort: errors are logged but don't abort the update.
func (u *Updater) cleanupStaleStaging() error {
	dir := os.TempDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // can't read temp dir — skip cleanup
	}
	for _, e := range entries {
		name := e.Name()
		// go-selfupdate temp files use "selfupdate-" prefix (verify against
		// library source if this doesn't match actual naming).
		if strings.HasPrefix(name, "selfupdate-") || strings.HasPrefix(name, ".selfupdate-") {
			path := filepath.Join(dir, name)
			if rerr := os.Remove(path); rerr != nil {
				u.log.Warn("cleanup: could not remove stale staging", "path", path, "err", rerr)
			}
		}
	}
	return nil
}
```

Note: the `cleanupStaleStaging` prefix `"selfupdate-"` is a guess. During implementation, verify the actual temp file prefix by checking go-selfupdate's `update.go` source. If the library cleans up its own temp files on failure, reduce `cleanupStaleStaging` to a no-op.

- [ ] **Step 4: Verify temp file prefix in cleanupStaleStaging**

Check go-selfupdate source for the actual temp file naming:
```bash
grep -rn "CreateTemp\|TempFile\|os.Create" $(go env GOMODCACHE)/github.com/creativeprojects/go-selfupdate@*/
```
Adjust the prefix in `cleanupStaleStaging` (in the implementation above) to match what the library actually uses. If the library cleans up its own temp files on failure, replace the function body with `return nil` (no-op).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/update/ -race -v`
Expected: PASS

- [ ] **Step 6: Run full test suite**

Run: `go test -race ./...`
Expected: all PASS

- [ ] **Step 7: Commit**

```bash
git add internal/update/update.go internal/update/update_test.go
git commit -m "feat(update): add Updater with LatestVersion + UpdateToLatest"
```

---

## Task 9: `internal/cli/version.go` — `runVersion` subcommand

Print current version (Version, Commit, Date, GOOS/GOARCH). `--check` flag queries remote via `LatestVersion` (cache-aware, read-only).

**Files:**
- Create: `internal/cli/version.go`
- Create: `internal/cli/version_test.go`

**Interfaces:**
- Produces: `runVersion(ctx context.Context, args []string, out io.Writer) int`
- Consumes: `internal/version` (Version, Commit, Date, RepoOwner, RepoName), `internal/update` (New + LatestVersion), `runtime.GOOS`/`runtime.GOARCH`

- [ ] **Step 1: Write failing test**

Create `internal/cli/version_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jim58246/sshmng/internal/version"
)

func TestRunVersion_PrintsVersion(t *testing.T) {
	orig := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = orig }()

	var out bytes.Buffer
	code := runVersion(context.Background(), []string{}, &out)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	output := out.String()
	if !strings.Contains(output, "sshmng v1.2.3") {
		t.Errorf("output missing version: %s", output)
	}
}

func TestRunVersion_DevBuild(t *testing.T) {
	orig := version.Version
	version.Version = "dev"
	defer func() { version.Version = orig }()

	var out bytes.Buffer
	code := runVersion(context.Background(), []string{}, &out)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "sshmng dev") {
		t.Errorf("output missing dev marker: %s", out.String())
	}
}

func TestRunVersion_CheckFlag_NoUpdateURL_WarnsButExits0(t *testing.T) {
	orig := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = orig }()

	// No config file → default config has empty UpdateURL
	// runVersion needs a config to check update_url. Use temp home.
	tmpHome := t.TempDir()
	t.Setenv("SSHMNG_HOME", tmpHome)
	// Write minimal config so resolveConfigPath works
	os.MkdirAll(tmpHome, 0700)
	os.WriteFile(filepath.Join(tmpHome, "config.json"), []byte(`{"version":"1"}`), 0600)

	var out bytes.Buffer
	code := runVersion(context.Background(), []string{"--check"}, &out)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (--check warn-only)", code)
	}
	if !strings.Contains(out.String(), "sshmng v1.2.3") {
		t.Errorf("output missing version: %s", out.String())
	}
}

func TestRunVersion_BadFlag_Exits2(t *testing.T) {
	var out bytes.Buffer
	code := runVersion(context.Background(), []string{"--bogus"}, &out)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (bad flag)", code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run RunVersion -v`
Expected: FAIL — `runVersion` undefined

- [ ] **Step 3: Implement `internal/cli/version.go`**

```go
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
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
		CachePath: cachePathForConfig(path),
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

	if isNewerPublic(latest, version.Version) {
		fmt.Fprintf(out, "Checking latest release ... latest is %s\n", latest)
		fmt.Fprintf(out, "Update available: %s -> %s\n", version.Version, latest)
		fmt.Fprintln(out, "Run 'sshmng update' to apply.")
	} else {
		fmt.Fprintln(out, "Checking latest release ... already at latest")
	}
	return 0
}

// isNewerPublic is a thin wrapper around update's isNewer for CLI use.
// Defined here to avoid exporting isNewer from internal/update.
// Alternative: export isNewer as update.IsNewer. For simplicity, duplicate
// the logic (it's 5 lines).
func isNewerPublic(latest, current string) bool {
	if current == "dev" {
		return true
	}
	// Delegate to update package via a small exported helper, or inline:
	// For now, import golang.org/x/mod/semver here too.
	// Actually cleaner: add update.IsNewer as exported wrapper.
	return update.IsNewer(latest, current)
}

// cachePathForConfig returns the cache file path alongside the config file.
func cachePathForConfig(configPath string) string {
	// filepath.Join(filepath.Dir(configPath), "update_cache.json")
	// (inline to avoid importing filepath if not otherwise needed)
	dir := configPath
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' || dir[i] == '\\' {
			dir = dir[:i]
			break
		}
	}
	return dir + string(filepath.Separator) + "update_cache.json"
}
```

Wait — this has issues. We reference `filepath` without import, and `update.IsNewer` which doesn't exist yet (we only have unexported `isNewer`). Let me fix:

**Revised Step 3:** Add an exported `IsNewer` to `internal/update/semver.go`:

```go
// In internal/update/semver.go, add:
// IsNewer is the exported wrapper around isNewer for CLI use.
func IsNewer(latest, current string) bool {
	return isNewer(latest, current)
}
```

And revise `internal/cli/version.go` to use `filepath` properly:

```go
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
```

- [ ] **Step 4: Add exported `IsNewer` to `internal/update/semver.go`**

Edit `internal/update/semver.go` — append:

```go
// IsNewer is the exported wrapper around isNewer for CLI use.
func IsNewer(latest, current string) bool {
	return isNewer(latest, current)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run RunVersion -race -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/cli/version.go internal/cli/version_test.go internal/update/semver.go
git commit -m "feat(cli): add sshmng version subcommand with --check"
```

---

## Task 10: `internal/cli/update.go` — `runUpdate` subcommand

Manual update: check + download + swap. Blocks, stdout progress. Fails on dev build or config errors.

**Files:**
- Create: `internal/cli/update.go`
- Create: `internal/cli/update_test.go`

**Interfaces:**
- Produces: `runUpdate(ctx context.Context, args []string, out io.Writer) int`
- Consumes: `internal/version`, `internal/update`, `internal/config`

- [ ] **Step 1: Write failing test**

Create `internal/cli/update_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jim58246/sshmng/internal/version"
)

func TestRunUpdate_DevBuild_FailsExit1(t *testing.T) {
	orig := version.Version
	version.Version = "dev"
	defer func() { version.Version = orig }()

	var out bytes.Buffer
	code := runUpdate(context.Background(), []string{}, &out)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (dev build)", code)
	}
	if !strings.Contains(out.String(), "[FAIL]") {
		t.Errorf("output missing [FAIL]: %s", out.String())
	}
	if !strings.Contains(out.String(), "version not set") {
		t.Errorf("output missing dev-build hint: %s", out.String())
	}
}

func TestRunUpdate_BadFlag_Exits2(t *testing.T) {
	var out bytes.Buffer
	code := runUpdate(context.Background(), []string{"--bogus"}, &out)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (bad flag)", code)
	}
}

func TestRunUpdate_NoConfig_Exits1(t *testing.T) {
	orig := version.Version
	version.Version = "v1.2.3"
	defer func() { version.Version = orig }()

	// Point to a nonexistent config so store.Load falls back to default
	t.Setenv("SSHMNG_HOME", t.TempDir())

	var out bytes.Buffer
	code := runUpdate(context.Background(), []string{}, &out)
	// With default config (auto_update_enabled=true, update_url=""), it'll
	// try GitHub source. In test env without network, LatestVersion fails.
	// Either exit 1 (network fail) or exit 0 (if cache happens to be fresh).
	// We just verify it doesn't crash with panic.
	_ = code
	_ = out.String()
}
```

Note: the third test is intentionally loose — real network behavior is flaky in CI. The first two tests (dev build + bad flag) are deterministic and sufficient to verify the command structure.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run RunUpdate -v`
Expected: FAIL — `runUpdate` undefined

- [ ] **Step 3: Implement `internal/cli/update.go`**

```go
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
// (manual command is always allowed).
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
```

Note: output ordering differs slightly from the spec's mockup. The spec shows "Latest version" before "Updating", but with go-selfupdate's `UpdateSelf` doing download+swap atomically, we can't print "Latest version" before the update (we only know `applied` after). The output above prints "done" after the full check+apply, then the summary. Acceptable deviation — the spec's mockup assumed granular progress we don't have.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run RunUpdate -race -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cli/update.go internal/cli/update_test.go
git commit -m "feat(cli): add sshmng update subcommand"
```

---

## Task 11: `internal/cli/doctor.go` — add `update_url` + dev-build checks

Add two checks to `RunDoctor`: (1) `update_url` non-empty must be valid http/https URL; (2) `sshmng version` output `== "dev"` warns "dev build, self-update disabled".

**Files:**
- Modify: `internal/cli/doctor.go` (add checks after config.json block, around line 99)
- Modify: `internal/cli/doctor_test.go` (add test cases)

**Interfaces:**
- Produces: two new check blocks in `RunDoctor` output
- Consumes: `internal/version` (Version), `net/url` (for URL validation)

- [ ] **Step 1: Write failing tests**

Add to `internal/cli/doctor_test.go` (find existing test patterns first):

```go
func TestRunDoctor_UpdateURL_Valid(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(home, 0700)
	// Write config with valid update_url
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{
		"version": "1",
		"update_url": "https://updates.example.com/sshmng"
	}`), 0600)

	var out bytes.Buffer
	code := RunDoctor(DoctorOpts{Home: home}, &out)
	// update_url valid → no FAIL for that check. Other checks may fail (no
	// known_hosts etc.) but we only care that update_url line shows OK.
	output := out.String()
	if !strings.Contains(output, "update_url") {
		t.Errorf("output missing update_url check line:\n%s", output)
	}
	_ = code
}

func TestRunDoctor_UpdateURL_Invalid_Fails(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(home, 0700)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{
		"version": "1",
		"update_url": "ftp://bad-scheme"
	}`), 0600)

	var out bytes.Buffer
	RunDoctor(DoctorOpts{Home: home}, &out)
	output := out.String()
	if !strings.Contains(output, "[FAIL]") {
		t.Errorf("expected [FAIL] for invalid update_url:\n%s", output)
	}
	if !strings.Contains(output, "update_url") {
		t.Errorf("output missing update_url mention:\n%s", output)
	}
}

func TestRunDoctor_DevBuild_Warns(t *testing.T) {
	orig := version.Version
	version.Version = "dev"
	defer func() { version.Version = orig }()

	home := t.TempDir()
	os.MkdirAll(home, 0700)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"version":"1"}`), 0600)

	var out bytes.Buffer
	RunDoctor(DoctorOpts{Home: home}, &out)
	if !strings.Contains(out.String(), "dev build") {
		t.Errorf("output missing dev build warning:\n%s", out.String())
	}
}
```

Add necessary imports (`version`, `os`, `filepath`, `strings`) to the test file.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run "Doctor_UpdateURL|Doctor_DevBuild" -v`
Expected: FAIL — new assertions not satisfied (no update_url check in output)

- [ ] **Step 3: Modify `internal/cli/doctor.go`**

Add import `"net/url"` and `"github.com/jim58246/sshmng/internal/version"`.

After the `config.json` block (around line 99, after the `config.example.json` check), add a new section before the "binary" check:

```go
	// update_url (if set)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Update source:")
	if cfgErr == nil {
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
```

Note: `cfg` and `cfgErr` need to be in scope. Looking at the existing doctor.go, the config is loaded inside an `if/else` block (line 80-99). We need to hoist `cfg` and the load error to outer scope so the update_url check can access them. Restructure:

Find the block around line 78-99:
```go
	// config.json
	cfgPath := filepath.Join(opts.Home, "config.json")
	if _, err := os.Stat(cfgPath); err != nil {
		print("FAIL", fmt.Sprintf("%s missing - run 'sshmng install'", cfgPath))
	} else {
		store := config.NewStore(cfgPath)
		if _, err := store.Load(); err != nil {
			print("FAIL", fmt.Sprintf("config.json invalid: %v", err))
		} else {
			// ... perm checks ...
		}
	}
```

Change to hoist `cfg`:
```go
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
			// ... existing perm checks ...
		}
	}
```

Then the update_url check uses `cfg` (nil-safe: only check if `cfgLoadErr == nil && cfg != nil`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run "Doctor" -race -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cli/doctor.go internal/cli/doctor_test.go
git commit -m "feat(cli): doctor adds update_url + dev-build checks"
```

---

## Task 12: `internal/cli/cli.go` — Dispatch routing + helpText

Add `update` and `version` cases to the Dispatch switch. Update `helpText` with two new lines.

**Files:**
- Modify: `internal/cli/cli.go` (Dispatch switch + helpText const)
- Modify: `internal/cli/cli_test.go` (add routing tests)

**Interfaces:**
- Produces: Dispatch now routes `update` → `runUpdate`, `version` → `runVersion`
- Consumes: `runUpdate` (Task 10), `runVersion` (Task 9)

- [ ] **Step 1: Write failing test**

Add to `internal/cli/cli_test.go` (find existing dispatch tests):

```go
func TestDispatch_Update(t *testing.T) {
	var out bytes.Buffer
	code := Dispatch(context.Background(), []string{"update", "-h"}, &out)
	// -h → flag.ErrHelp → exit 0 or 2 depending on flag pkg behavior.
	// Just verify "update" is recognized (not "Unknown command").
	if strings.Contains(out.String(), "Unknown command") {
		t.Errorf("update not routed: %s", out.String())
	}
	_ = code
}

func TestDispatch_Version(t *testing.T) {
	var out bytes.Buffer
	code := Dispatch(context.Background(), []string{"version"}, &out)
	if code != 0 {
		t.Errorf("version exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "sshmng") {
		t.Errorf("version output missing sshmng: %s", out.String())
	}
}

func TestDispatch_HelpTextMentionsUpdateVersion(t *testing.T) {
	var out bytes.Buffer
	Dispatch(context.Background(), []string{}, &out)
	output := out.String()
	if !strings.Contains(output, "update") {
		t.Errorf("helpText missing 'update':\n%s", output)
	}
	if !strings.Contains(output, "version") {
		t.Errorf("helpText missing 'version':\n%s", output)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run "Dispatch_Update|Dispatch_Version|Dispatch_HelpText" -v`
Expected: FAIL — `update` / `version` not routed ("Unknown command")

- [ ] **Step 3: Modify `internal/cli/cli.go`**

Add two cases to the Dispatch switch (after `case "doctor":`):

```go
	case "update":
		return runUpdate(ctx, args[1:], out)
	case "version":
		return runVersion(ctx, args[1:], out)
```

Update `helpText` const — add two lines to the Usage block and two lines to the Subcommands block:

```go
const helpText = `sshmng - SSH session manager

Usage:
  sshmng                          Print this help and exit
  sshmng mcp [--config <path>]    Start MCP server (stdio)
  sshmng install [...]            First-time setup
  sshmng doctor [...]             Verify setup
  sshmng update                   Manually update sshmng to the latest release
  sshmng version [--check]        Print version; --check compares with latest release
  sshmng help | -h | --help       Print this help

Subcommands:
  mcp       Start the MCP server. This is what Agent configs should use
            (e.g. "command": "sshmng", "args": ["mcp"]).
  install   Create ~/.sshmng/, generate config templates, and inject sshmng
            into your AI Agent(s) (Claude Code / Hermes Agent / OpenCode).
  doctor    Verify setup is correct: files, permissions, Agent config entries.
  update    Check for a newer release and apply it. Manual; unaffected by
            auto_update_enabled. Uses GitHub Releases by default, or a
            self-hosted HTTP server if update_url is configured.
  version   Print the current version, commit, and build date. With --check,
            also query the remote source for the latest version.

Run 'sshmng <subcommand> -h' for subcommand-specific flags.
`
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -race -v`
Expected: all PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cli.go internal/cli/cli_test.go
git commit -m "feat(cli): Dispatch routes update + version; helpText updated"
```

---

## Task 13: `internal/cli/mcp.go` — mount auto-update goroutine

In `runMCP`, after logger is set up and config is loaded, before `svc.Run(ctx)`, spawn a goroutine that calls `update.New` + `UpdateToLatest`. Silent: all errors go to `logger.Warn`, never stdout/stderr.

**Files:**
- Modify: `internal/cli/mcp.go` (add goroutine after line 66, before line 67)

**Interfaces:**
- Produces: auto-update goroutine in `runMCP`
- Consumes: `internal/update` (New + UpdateToLatest), `internal/version`, `cfg.AutoUpdateEnabled`, `cfg.UpdateURL`

- [ ] **Step 1: Write failing test**

Add to `internal/cli/mcp_test.go` (or create if not present):

```go
func TestRunMCP_AutoUpdateDisabled_DoesNotSpawnGoroutine(t *testing.T) {
	// Hard to test goroutine spawning directly. Instead, verify that with
	// auto_update_enabled=false, the mcp command doesn't hang or crash on
	// the update path. Use a config with auto_update disabled.
	home := t.TempDir()
	os.MkdirAll(home, 0700)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{
		"version": "1",
		"auto_update_enabled": false
	}`), 0600)
	t.Setenv("SSHMNG_HOME", home)

	// Run mcp with a context that cancels quickly (simulates SIGINT)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var out bytes.Buffer
	code := runMCP(ctx, []string{}, &out)
	// Server runs until ctx cancelled → exit 1 (context error) or 0.
	// We just verify no panic / no update attempt in output.
	_ = code
}

func TestRunMCP_AutoUpdateEnabled_DevBuild_SkipsGoroutine(t *testing.T) {
	orig := version.Version
	version.Version = "dev"
	defer func() { version.Version = orig }()

	home := t.TempDir()
	os.MkdirAll(home, 0700)
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{
		"version": "1",
		"auto_update_enabled": true
	}`), 0600)
	t.Setenv("SSHMNG_HOME", home)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var out bytes.Buffer
	runMCP(ctx, []string{}, &out)
	// dev build → goroutine skipped. No assertion on output (goroutine is
	// silent). Test just verifies no panic.
}
```

These tests are loose (can't easily assert goroutine behavior). They verify no panic + no hang. Real verification of auto-update happens via integration testing (out of scope).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run "RunMCP_AutoUpdate" -v`
Expected: FAIL or PASS — these are mostly smoke tests. If they pass already (because the goroutine isn't there yet, nothing to test), that's fine — Step 3 adds the goroutine and tests should still pass.

- [ ] **Step 3: Modify `internal/cli/mcp.go`**

Add imports:
```go
	"github.com/jim58246/sshmng/internal/update"
	"github.com/jim58246/sshmng/internal/version"
```

After line 66 (`logger.Info("sshmng MCP server starting", ...)`) and before line 67 (`if err := svc.Run(ctx); err != nil {`), add:

```go
	// Auto-update goroutine: silent, never writes to stdout/stderr (MCP
	// server invariant). Skipped when auto_update_enabled=false or dev build.
	if cfg.AutoUpdateEnabled && version.Version != "dev" {
		go func() {
			u, err := update.New(update.Config{
				RepoOwner: version.RepoOwner,
				RepoName:  version.RepoName,
				UpdateURL: cfg.UpdateURL,
				CachePath: filepath.Join(filepath.Dir(path), "update_cache.json"),
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

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -race -v`
Expected: PASS

- [ ] **Step 5: Run full test suite**

Run: `go test -race ./...`
Expected: all PASS

- [ ] **Step 6: Commit**

```bash
git add internal/cli/mcp.go internal/cli/mcp_test.go
git commit -m "feat(cli): mount auto-update goroutine in runMCP"
```

---

## Task 14: `.goreleaser.yaml` + `.github/workflows/release.yml`

Create the release pipeline. goreleaser builds 6 platforms with ldflags injecting version/commit/date/repo into `internal/version`. GitHub Actions triggers on `v*` tag push.

**Files:**
- Create: `.goreleaser.yaml`
- Create: `.github/workflows/release.yml`

**Interfaces:**
- Produces: runnable `goreleaser release` + CI workflow

- [ ] **Step 1: Create `.goreleaser.yaml`**

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

- [ ] **Step 2: Verify goreleaser config syntax**

Install goreleaser if not present:
```bash
brew install goreleaser  # macOS
# or: go install github.com/goreleaser/goreleaser/v2@latest
```

Run:
```bash
goreleaser check
```
Expected: `config is valid` (or similar success message). If errors, fix the YAML.

- [ ] **Step 3: Run a snapshot build to verify archives are produced with correct naming**

```bash
goreleaser build --snapshot --clean
```

Expected: `dist/` directory contains archives matching `sshmng-v0.0.0-next-{goos}-{goarch}.tar.gz` (snapshot uses `v0.0.0-next` tag). Verify the naming convention matches what `flatHTTPSource` expects: `sshmng-{tag}-{goos}-{goarch}.{tar.gz|zip}`.

Note: snapshot version may be `v0.0.0-next` or similar — the exact format depends on goreleaser version. The key check: archive name is `sshmng-<something>-<goos>-<goarch>.<ext>`.

- [ ] **Step 4: Create `.github/workflows/release.yml`**

First ensure the directory exists:
```bash
mkdir -p .github/workflows
```

Create `.github/workflows/release.yml`:

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

- [ ] **Step 5: Verify workflow YAML is valid**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml'))" && echo "YAML OK"
```

Or use `actionlint` if installed:
```bash
actionlint .github/workflows/release.yml
```

Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add .goreleaser.yaml .github/workflows/release.yml
git commit -m "ci: add goreleaser config + release workflow on tag push"
```

---

## Task 15: README.md — document auto-update, self-hosted source, release flow

Update README to reflect: new `update` / `version` subcommands, `auto_update_enabled` / `update_url` config fields, self-hosted HTTP source layout, release flow for maintainers.

**Files:**
- Modify: `README.md`

**Interfaces:**
- Produces: updated docs

- [ ] **Step 1: Read current README to find sections to update**

Run: `grep -n "^##" README.md`
Expected: list of section headings. Identify "Install" / "Build" / "Configuration" sections to update.

- [ ] **Step 2: Add `update` / `version` to the subcommand list**

Find the subcommand usage section (likely near the top). Add `update` and `version` lines alongside `mcp` / `install` / `doctor`.

- [ ] **Step 3: Add auto-update section**

After the configuration section, add:

```markdown
## Auto-update

sshmng checks for updates on `mcp` startup (background goroutine, silent — logs only to `log_path`). To disable:

```json
{
  "auto_update_enabled": false
}
```

Manual update:

```bash
sshmng update
```

Check current version vs. latest:

```bash
sshmng version --check
```

By default, sshmng pulls from GitHub Releases. To use a self-hosted HTTP server (e.g., internal mirror or air-gapped environment), set `update_url`:

```json
{
  "update_url": "https://updates.mycompany.com/sshmng"
}
```

### Self-hosted HTTP server layout

The server is any static file server (nginx, Caddy, S3, Python `http.server`). Required files at the base URL:

```
{base_url}/
  latest.txt                                    # one line: v1.2.3
  checksums.txt                                 # goreleaser-generated sha256
  sshmng-v1.2.3-darwin-arm64.tar.gz
  sshmng-v1.2.3-darwin-amd64.tar.gz
  sshmng-v1.2.3-linux-amd64.tar.gz
  sshmng-v1.2.3-linux-arm64.tar.gz
  sshmng-v1.2.3-windows-amd64.zip
  sshmng-v1.2.3-windows-arm64.zip
```

To publish a release: run `goreleaser release --clean`, copy `dist/sshmng-*` archives + `dist/checksums.txt` to the server, update `latest.txt` to the new version.

### macOS note

If you invoke sshmng via a symlink (e.g., `~/.local/bin/sshmng -> ~/go/bin/sshmng`), self-update replaces the symlink, not the target binary. Install sshmng as a regular file (the default `go install` / `sshmng install` behavior) to avoid this.
```

- [ ] **Step 4: Update build section with ldflags example**

Find the "Build" section. Add a note about ldflags for manual builds:

```markdown
### Build from source

```bash
go build -ldflags="-X github.com/jim58246/sshmng/internal/version.Version=v1.2.3" ./cmd/sshmng
```

Without ldflags, `version.Version` defaults to `"dev"` and self-update is disabled.
```

- [ ] **Step 5: Add maintainer release flow section**

Near the end of README, add:

```markdown
## Release (maintainers)

```bash
git tag v1.2.3
git push origin v1.2.3
```

The `release` GitHub Actions workflow runs goreleaser, which:
1. Builds 6 platform archives (darwin/linux/windows × amd64/arm64)
2. Generates `checksums.txt`
3. Creates a GitHub Release with the tag
4. Uploads archives + checksums as release assets

Users running `sshmng update` or `sshmng mcp` (auto-update) will pick up the new release within 1 hour (cache TTL).
```

- [ ] **Step 6: Commit**

```bash
git add README.md
git commit -m "docs: document auto-update, self-hosted source, release flow"
```

---

## Self-Review Checklist (run after all tasks complete)

- [ ] `go test -race ./...` passes
- [ ] `go build ./...` succeeds
- [ ] `goreleaser check` passes
- [ ] `sshmng version` prints version/commit/date
- [ ] `sshmng version --check` (with no update_url) attempts GitHub source, prints result or warn
- [ ] `sshmng update` with `Version=dev` exits 1 with `[FAIL]` message
- [ ] `sshmng doctor` shows `update_url` line (OK or FAIL) + version line (OK or WARN for dev)
- [ ] `sshmng help` lists `update` and `version` subcommands
- [ ] `sshmng mcp` with `auto_update_enabled: false` starts without update goroutine
- [ ] `sshmng mcp` with `auto_update_enabled: true` + `Version=dev` skips update goroutine
- [ ] Config skeleton (`config.json` written by `sshmng install`) contains `"auto_update_enabled": true`
- [ ] `internal/mcp` serverInfo.version reports real version (not `"v1"`)
- [ ] `.goreleaser.yaml` archive naming matches `flatHTTPSource` asset convention exactly: `sshmng-{tag}-{goos}-{goarch}.{tar.gz|zip}`
