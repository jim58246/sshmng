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
