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
