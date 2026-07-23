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

// mockSource implements selfupdate.Source for testing. It does not download
// anything — LatestVersion never downloads, and UpdateToLatest's download is
// exercised via mockLib (which skips the real source entirely).
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
		repo:      selfupdate.NewRepositorySlug("test", "test"),
		cachePath: filepath.Join(t.TempDir(), "cache.json"),
		cacheTTL:  time.Hour,
		log:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
}

func TestLatestVersion_CacheFresh_NoSourceCall(t *testing.T) {
	src := &mockSource{releases: []selfupdate.SourceRelease{&flatRelease{tag: "v9.9.9"}}}
	u := newTestUpdater(t, src, &mockLib{})

	// Pre-populate cache as fresh
	if err := writeCache(u.cachePath, cacheEntry{
		LastCheckAt:   time.Now().Add(-5 * time.Minute),
		LatestVersion: "v1.0.0",
	}); err != nil {
		t.Fatalf("writeCache: %v", err)
	}

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
	// selfupdate.Release has no exportable TagName field; the test does not
	// inspect the returned release, so a zero Release is sufficient.
	lib := &mockLib{release: &selfupdate.Release{}}
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
