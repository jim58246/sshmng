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
