package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// MCPEntry is the sshmng MCP server entry written into Agent configs.
type MCPEntry struct {
	BinaryPath string
	Args       []string
	Env        map[string]string
}

// AgentInjector knows how to inject and verify the sshmng MCP entry in a
// specific AI Agent's config file.
type AgentInjector interface {
	Name() string                                // short identifier, e.g. "claude-code"
	DisplayName() string                         // human-friendly, e.g. "Claude Code"
	Detect() (configPath string, installed bool) // check if Agent is installed
	Inject(path string, entry MCPEntry) error    // merge sshmng entry into config
	Verify(path string, expected MCPEntry) error
}

// backupFile copies path to <path>.bak.<YYYYMMDD-HHMMSS>. Does not delete old
// backups. If path does not exist, returns nil (no-op).
func backupFile(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s for backup: %w", path, err)
	}
	ts := time.Now().Format("20060102-150405")
	backupPath := fmt.Sprintf("%s.bak.%s", path, ts)
	if err := os.WriteFile(backupPath, data, 0600); err != nil {
		return fmt.Errorf("write backup %s: %w", backupPath, err)
	}
	return nil
}

// loadJSONMap reads path as JSON into a map. Empty/missing file -> empty map.
func loadJSONMap(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	m := map[string]any{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s as JSON: %w", path, err)
	}
	return m, nil
}

// writeJSONMapAtomic writes m as indented JSON to path atomically:
//  1. Write temp file in same dir
//  2. Chmod 0600 (Unix; no-op on Windows)
//  3. Rename (Unix) or backup+delete+rename (Windows)
//
// Caller must call backupFile first if backups are desired.
func writeJSONMapAtomic(path string, m map[string]any) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".agent.json.tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if rename succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmpName, 0600); err != nil {
			return fmt.Errorf("chmod temp: %w", err)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		if runtime.GOOS == "windows" {
			// Windows rename fails if destination exists; remove then retry.
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
				return fmt.Errorf("remove old %s: %w (rename err: %v)", path, rmErr, err)
			}
			if err := os.Rename(tmpName, path); err != nil {
				// The original config has been deleted above; restore from the
				// newest backup (created by backupFile before write) so the user
				// does not lose their Agent config. Spec line 288: "写入失败：
				// 从最新备份恢复，报错退出".
				restoreErr := restoreFromBackup(path)
				return fmt.Errorf("rename temp to %s: %w (restore from backup: %v; backup at %s.bak.<ts>)", path, err, restoreErr, path)
			}
			return nil
		}
		// Non-Windows: rename is atomic, original is intact on failure. Include
		// the backup path in the error for diagnosability.
		return fmt.Errorf("rename temp to %s: %w (backup at %s.bak.<ts> if exists)", path, err, path)
	}
	return nil
}

// mergeEntry sets m[topKey]["sshmng"] = entryMap, creating intermediate maps as
// needed. Other entries under topKey are preserved.
func mergeEntry(m map[string]any, topKey string, entryMap map[string]any) {
	servers, _ := m[topKey].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers["sshmng"] = entryMap
	m[topKey] = servers
}

// restoreFromBackup copies the newest <path>.bak.* backup back over path. Used
// to recover the user's Agent config when an atomic write fails after the
// original has been deleted (Windows rename-after-delete branch). Returns an
// error if no backup exists or the copy fails; the error includes the backup
// glob for diagnosis.
func restoreFromBackup(path string) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path) + ".bak.*"
	matches, err := filepath.Glob(filepath.Join(dir, base))
	if err != nil {
		return fmt.Errorf("glob backups: %w", err)
	}
	if len(matches) == 0 {
		return fmt.Errorf("no backup found matching %s.bak.*", path)
	}
	newest := matches[0]
	newestStat, err := os.Stat(newest)
	if err != nil {
		return fmt.Errorf("stat backup %s: %w", newest, err)
	}
	for _, m := range matches[1:] {
		st, err := os.Stat(m)
		if err != nil {
			continue
		}
		if st.ModTime().After(newestStat.ModTime()) {
			newest = m
			newestStat = st
		}
	}
	data, err := os.ReadFile(newest)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", newest, err)
	}
	perm := os.FileMode(0600)
	if runtime.GOOS == "windows" {
		perm = 0644
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		return fmt.Errorf("restore %s from %s: %w", path, newest, err)
	}
	return nil
}

// expectedHome returns the SSHMNG_HOME value from entry.Env, or "" if unset.
func expectedHome(entry MCPEntry) string {
	if entry.Env == nil {
		return ""
	}
	return entry.Env["SSHMNG_HOME"]
}

// argsEqual reports whether a JSON-parsed args field ([]any of strings) equals
// the expected []string. JSON arrays always decode to []any; nil and empty are
// treated as equal to match how an absent args field round-trips.
func argsEqual(got any, want []string) bool {
	arr, ok := got.([]any)
	if !ok {
		return want == nil || len(want) == 0
	}
	if len(arr) != len(want) {
		return false
	}
	for i, v := range arr {
		s, _ := v.(string)
		if s != want[i] {
			return false
		}
	}
	return true
}

// argsEqualYAML reports whether a YAML-parsed args field equals the expected
// []string. yaml.v3 decodes sequences to []any with each element as the
// decoded type (strings for scalar string elements).
func argsEqualYAML(got any, want []string) bool {
	return argsEqual(got, want)
}

// stringSliceEqual reports whether two []string slices are equal, treating nil
// and empty as equal.
func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
