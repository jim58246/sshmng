package update

import (
	"fmt"

	"github.com/creativeprojects/go-selfupdate"
)

// newGitHubSource wraps selfupdate.NewGitHubSource. owner/name come from
// version.RepoOwner / version.RepoName (ldflags-injected) and are validated
// here as an ldflags-injection check; they are later paired with the source
// as a RepositorySlug when calling ListReleases. Forks override via ldflags
// to redirect updates to their fork.
func newGitHubSource(owner, name string) (selfupdate.Source, error) {
	if owner == "" || name == "" {
		return nil, fmt.Errorf("github source requires non-empty RepoOwner and RepoName (ldflags not injected?)")
	}
	src, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{})
	if err != nil {
		return nil, fmt.Errorf("github source: %w", err)
	}
	return src, nil
}
