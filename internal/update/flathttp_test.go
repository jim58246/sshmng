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
	// Repository is an interface in go-selfupdate v1.6.0; flatHTTPSource
	// ignores it (baseURL carries everything). Pass a valid slug to avoid
	// noise from implementations that do consult it.
	repo := selfupdate.NewRepositorySlug("owner", "repo")
	releases, err := src.ListReleases(context.Background(), repo)
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
			if !strings.HasSuffix(a.GetBrowserDownloadURL(), "/checksums.txt") {
				t.Errorf("checksums URL = %q, want suffix /checksums.txt", a.GetBrowserDownloadURL())
			}
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
	_, err := src.ListReleases(context.Background(), selfupdate.NewRepositorySlug("owner", "repo"))
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
	_, err := src.ListReleases(context.Background(), selfupdate.NewRepositorySlug("owner", "repo"))
	if err == nil {
		t.Fatal("expected error for invalid semver, got nil")
	}
}

// TestFlatHTTPSource_DownloadReleaseAsset verifies the download path.
//
// go-selfupdate v1.6.0's DownloadReleaseAsset takes *selfupdate.Release,
// which is a FLAT struct (no Assets slice). The library's own HttpSource and
// GitHubSource match assetID against rel.AssetID / rel.ValidationAssetID and
// fetch the corresponding rel.AssetURL / rel.ValidationAssetURL. Our
// flatHTTPSource follows the same contract.
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
		if r.URL.Path == "/checksums.txt" {
			w.Write([]byte("fake checksums"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src, _ := newFlatHTTPSource(srv.URL)
	releases, _ := src.ListReleases(context.Background(), selfupdate.NewRepositorySlug("owner", "repo"))
	rel := releases[0]

	// Find darwin-arm64 asset ID + URL from our SourceRelease.
	var (
		assetID  int64
		assetURL string
	)
	for _, a := range rel.GetAssets() {
		if strings.Contains(a.GetName(), "darwin-arm64") {
			assetID = a.GetID()
			assetURL = a.GetBrowserDownloadURL()
		}
	}
	if assetID == 0 {
		t.Fatal("darwin-arm64 asset ID not found")
	}

	// Build the library's flat *Release. AssetID/AssetURL is the primary
	// download slot; ValidationAssetID/ValidationAssetURL is the checksums
	// slot. Our source matches against either.
	libRel := &selfupdate.Release{
		ReleaseID: rel.GetID(),
		AssetID:   assetID,
		AssetURL:  assetURL,
	}

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

// TestFlatHTTPSource_DownloadReleaseAsset_ValidationSlot verifies the
// checksums asset is reachable via the ValidationAssetID slot.
func TestFlatHTTPSource_DownloadReleaseAsset_ValidationSlot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest.txt" {
			w.Write([]byte("v1.2.3\n"))
			return
		}
		if r.URL.Path == "/checksums.txt" {
			w.Write([]byte("abc123  sshmng-v1.2.3-darwin-arm64.tar.gz\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src, _ := newFlatHTTPSource(srv.URL)
	releases, _ := src.ListReleases(context.Background(), selfupdate.NewRepositorySlug("owner", "repo"))
	rel := releases[0]

	var (
		validationID  int64
		validationURL string
	)
	for _, a := range rel.GetAssets() {
		if a.GetName() == "checksums.txt" {
			validationID = a.GetID()
			validationURL = a.GetBrowserDownloadURL()
		}
	}
	if validationID == 0 {
		t.Fatal("checksums.txt asset ID not found")
	}

	libRel := &selfupdate.Release{
		ReleaseID:          rel.GetID(),
		ValidationAssetID:  validationID,
		ValidationAssetURL: validationURL,
	}

	body, err := src.DownloadReleaseAsset(context.Background(), libRel, validationID)
	if err != nil {
		t.Fatalf("DownloadReleaseAsset: %v", err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	if !strings.HasPrefix(string(data), "abc123") {
		t.Errorf("downloaded = %q, want checksums content", data)
	}
}

func TestFlatHTTPSource_DownloadReleaseAsset_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src, _ := newFlatHTTPSource(srv.URL)
	libRel := &selfupdate.Release{
		AssetID:  42,
		AssetURL: srv.URL + "/sshmng-v1.2.3-darwin-arm64.tar.gz",
	}
	_, err := src.DownloadReleaseAsset(context.Background(), libRel, 999)
	if err == nil {
		t.Fatal("expected error for unknown assetID, got nil")
	}
}

func TestFlatHTTPSource_DownloadReleaseAsset_NilRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	src, _ := newFlatHTTPSource(srv.URL)
	_, err := src.DownloadReleaseAsset(context.Background(), nil, 1)
	if err == nil {
		t.Fatal("expected error for nil release, got nil")
	}
}

// TestFlatHTTPSource_DownloadReleaseAsset_HTTPError verifies non-200 responses
// surface as errors.
func TestFlatHTTPSource_DownloadReleaseAsset_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	src, _ := newFlatHTTPSource(srv.URL)
	libRel := &selfupdate.Release{
		AssetID:  1,
		AssetURL: srv.URL + "/missing.tar.gz",
	}
	_, err := src.DownloadReleaseAsset(context.Background(), libRel, 1)
	if err == nil {
		t.Fatal("expected error for HTTP 404, got nil")
	}
}
