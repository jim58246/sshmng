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
