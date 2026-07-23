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
	Verify(path string, expectedBinary string) error
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
				return fmt.Errorf("rename temp to %s: %w", path, err)
			}
			return nil
		}
		return fmt.Errorf("rename temp to %s: %w", path, err)
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
