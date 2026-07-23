package cli

import (
	"fmt"
	"os"
	"path/filepath"
)

// OpenCodeInjector handles ~/.config/opencode/opencode.json (JSON, mcp key).
// Schema differs from Claude Code / Hermes:
//   - command is an array (binary + args combined)
//   - env field is called "environment"
//   - requires type: "local" and enabled: true
type OpenCodeInjector struct{}

func (o *OpenCodeInjector) Name() string        { return "opencode" }
func (o *OpenCodeInjector) DisplayName() string { return "OpenCode" }

func (o *OpenCodeInjector) Detect() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	if _, err := os.Stat(path); err != nil {
		return path, false
	}
	return path, true
}

// entryMap builds the JSON map for the sshmng entry under mcp.sshmng.
// command is an array combining binary + args; env field is "environment".
func (o *OpenCodeInjector) entryMap(entry MCPEntry) map[string]any {
	command := make([]any, 0, len(entry.Args)+1)
	command = append(command, entry.BinaryPath)
	for _, a := range entry.Args {
		command = append(command, a)
	}
	return map[string]any{
		"type":        "local",
		"command":     command,
		"environment": entry.Env,
		"enabled":     true,
	}
}

func (o *OpenCodeInjector) Inject(path string, entry MCPEntry) error {
	if err := backupFile(path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	m, err := loadJSONMap(path)
	if err != nil {
		return err
	}
	mergeEntry(m, "mcp", o.entryMap(entry))
	if err := writeJSONMapAtomic(path, m); err != nil {
		return err
	}
	if _, err := loadJSONMap(path); err != nil {
		return fmt.Errorf("post-write verify: %w", err)
	}
	return nil
}

func (o *OpenCodeInjector) Verify(path string, expected MCPEntry) error {
	m, err := loadJSONMap(path)
	if err != nil {
		return err
	}
	servers, _ := m["mcp"].(map[string]any)
	if servers == nil {
		return fmt.Errorf("no mcp key in %s", path)
	}
	sshmng, _ := servers["sshmng"].(map[string]any)
	if sshmng == nil {
		return fmt.Errorf("no sshmng entry in mcp")
	}
	cmdArr, ok := sshmng["command"].([]any)
	if !ok {
		return fmt.Errorf("sshmng.command is not an array (got %T)", sshmng["command"])
	}
	if len(cmdArr) == 0 {
		return fmt.Errorf("sshmng.command array is empty")
	}
	first, _ := cmdArr[0].(string)
	if first != expected.BinaryPath {
		return fmt.Errorf("stale: expected command[0] %q, got %q", expected.BinaryPath, first)
	}
	rest := make([]string, 0, len(cmdArr)-1)
	for _, v := range cmdArr[1:] {
		s, _ := v.(string)
		rest = append(rest, s)
	}
	if !stringSliceEqual(rest, expected.Args) {
		return fmt.Errorf("stale: expected command args %q, got %v", expected.Args, rest)
	}
	env, _ := sshmng["environment"].(map[string]any)
	if env == nil || env["SSHMNG_HOME"] != expectedHome(expected) {
		return fmt.Errorf("stale: expected environment.SSHMNG_HOME %q, got %v", expectedHome(expected), env["SSHMNG_HOME"])
	}
	return nil
}
