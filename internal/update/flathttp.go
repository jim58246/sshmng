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
// base URL (see spec for layout). ListReleases fetches latest.txt and
// synthesizes a single release with 6 platform archives plus a checksums.txt
// asset, named by convention. DownloadReleaseAsset streams the asset body.
//
// The repo argument to ListReleases is ignored — the baseURL carries
// everything (flat layout has no owner/repo path segments).
type flatHTTPSource struct {
	baseURL string
	client  *http.Client
}

// newFlatHTTPSource validates that baseURL is http(s) and returns a source
// ready to fetch latest.txt from baseURL.
func newFlatHTTPSource(baseURL string) (*flatHTTPSource, error) {
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("update_url must be http:// or https:// URL, got: %q", baseURL)
	}
	return &flatHTTPSource{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// ListReleases fetches latest.txt, validates it as a semver tag, and returns
// a single release whose assets are the 6 platform archives + checksums.txt,
// all named by convention. The repository parameter is ignored.
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
	// checksums.txt gets the next id after the last platform asset.
	assets = append(assets, &flatAsset{
		id:   int64(len(platforms)),
		name: "checksums.txt",
		url:  s.baseURL + "/checksums.txt",
	})

	return []selfupdate.SourceRelease{&flatRelease{tag: tag, assets: assets}}, nil
}

// DownloadReleaseAsset streams the body of the asset identified by assetID.
//
// go-selfupdate v1.6.0's *selfupdate.Release is a flat struct with no Assets
// slice: it carries only the "current" asset (AssetID/AssetURL) and an
// optional validation asset (ValidationAssetID/ValidationAssetURL). Matching
// against both slots mirrors the library's own HttpSource and GitHubSource.
func (s *flatHTTPSource) DownloadReleaseAsset(ctx context.Context, rel *selfupdate.Release, assetID int64) (io.ReadCloser, error) {
	if rel == nil {
		return nil, fmt.Errorf("flathttp: nil release")
	}
	var downloadURL string
	if rel.AssetID == assetID && rel.AssetURL != "" {
		downloadURL = rel.AssetURL
	} else if rel.ValidationAssetID == assetID && rel.ValidationAssetURL != "" {
		downloadURL = rel.ValidationAssetURL
	}
	if downloadURL == "" {
		return nil, fmt.Errorf("flathttp: asset id %d not found in release", assetID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("flathttp: build request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("flathttp: download %s: %w", downloadURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("flathttp: download %s: HTTP %d", downloadURL, resp.StatusCode)
	}
	return resp.Body, nil
}

// fetchLatest GETs baseURL/latest.txt and returns its trimmed body, capped at
// 64 KiB (a version tag is tiny; the cap defends against a misconfigured
// server returning a huge file).
func (s *flatHTTPSource) fetchLatest(ctx context.Context) (string, error) {
	url := s.baseURL + "/latest.txt"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("flathttp: build latest.txt request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("flathttp: fetch latest.txt: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("flathttp: fetch latest.txt: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("flathttp: read latest.txt: %w", err)
	}
	return strings.TrimSpace(string(body)), nil
}

// flatRelease implements selfupdate.SourceRelease for a flat-layout release.
type flatRelease struct {
	tag    string
	assets []selfupdate.SourceAsset
}

func (r *flatRelease) GetID() int64                        { return 1 }
func (r *flatRelease) GetTagName() string                  { return r.tag }
func (r *flatRelease) GetDraft() bool                      { return false }
func (r *flatRelease) GetPrerelease() bool                 { return false }
func (r *flatRelease) GetPublishedAt() time.Time           { return time.Time{} }
func (r *flatRelease) GetReleaseNotes() string             { return "" }
func (r *flatRelease) GetName() string                     { return r.tag }
func (r *flatRelease) GetURL() string                      { return "" }
func (r *flatRelease) GetAssets() []selfupdate.SourceAsset { return r.assets }

// flatAsset implements selfupdate.SourceAsset for a flat-layout asset.
type flatAsset struct {
	id   int64
	name string
	url  string
}

func (a *flatAsset) GetID() int64                  { return a.id }
func (a *flatAsset) GetName() string               { return a.name }
func (a *flatAsset) GetSize() int                  { return 0 }
func (a *flatAsset) GetBrowserDownloadURL() string { return a.url }

// Compile-time interface checks.
var (
	_ selfupdate.Source        = (*flatHTTPSource)(nil)
	_ selfupdate.SourceRelease = (*flatRelease)(nil)
	_ selfupdate.SourceAsset   = (*flatAsset)(nil)
)
