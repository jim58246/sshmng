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
