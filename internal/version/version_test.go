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
