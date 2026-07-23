package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ClaudeCodeInjector handles ~/.claude.json (JSON, mcpServers key).
type ClaudeCodeInjector struct{}

func (c *ClaudeCodeInjector) Name() string        { return "claude-code" }
func (c *ClaudeCodeInjector) DisplayName() string { return "Claude Code" }

func (c *ClaudeCodeInjector) Detect() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	path := filepath.Join(home, ".claude.json")
	if _, err := os.Stat(path); err != nil {
		return path, false
	}
	return path, true
}

// entryMap builds the JSON map for the sshmng entry under mcpServers.sshmng.
// Schema: {command: string, args: [...], env: {...}}
func (c *ClaudeCodeInjector) entryMap(entry MCPEntry) map[string]any {
	return map[string]any{
		"command": entry.BinaryPath,
		"args":    entry.Args,
		"env":     entry.Env,
	}
}

func (c *ClaudeCodeInjector) Inject(path string, entry MCPEntry) error {
	if err := backupFile(path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	m, err := loadJSONMap(path)
	if err != nil {
		return err
	}
	mergeEntry(m, "mcpServers", c.entryMap(entry))
	if err := writeJSONMapAtomic(path, m); err != nil {
		return err
	}
	// Read back to verify
	if _, err := loadJSONMap(path); err != nil {
		return fmt.Errorf("post-write verify: %w", err)
	}
	return nil
}

func (c *ClaudeCodeInjector) Verify(path string, expectedBinary string) error {
	m, err := loadJSONMap(path)
	if err != nil {
		return err
	}
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return fmt.Errorf("no mcpServers key in %s", path)
	}
	sshmng, _ := servers["sshmng"].(map[string]any)
	if sshmng == nil {
		return fmt.Errorf("no sshmng entry in mcpServers")
	}
	cmd, _ := sshmng["command"].(string)
	if cmd != expectedBinary {
		return fmt.Errorf("stale: expected command %q, got %q", expectedBinary, cmd)
	}
	return nil
}

// Used by tests: parse entry back to MCPEntry for inspection.
func parseClaudeCodeEntry(m map[string]any) (MCPEntry, error) {
	servers, _ := m["mcpServers"].(map[string]any)
	if servers == nil {
		return MCPEntry{}, fmt.Errorf("no mcpServers")
	}
	sshmng, _ := servers["sshmng"].(map[string]any)
	if sshmng == nil {
		return MCPEntry{}, fmt.Errorf("no sshmng entry")
	}
	data, err := json.Marshal(sshmng)
	if err != nil {
		return MCPEntry{}, err
	}
	var e MCPEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return MCPEntry{}, err
	}
	return e, nil
}
