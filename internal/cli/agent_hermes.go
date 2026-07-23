package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// HermesInjector handles ~/.hermes/config.yaml (Unix) or
// %LOCALAPPDATA%\hermes\config.yaml (Windows). YAML, mcp_servers key.
type HermesInjector struct{}

func (h *HermesInjector) Name() string        { return "hermes" }
func (h *HermesInjector) DisplayName() string { return "Hermes Agent" }

func (h *HermesInjector) configPath() string {
	if runtime.GOOS == "windows" {
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData == "" {
			localAppData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
		}
		return filepath.Join(localAppData, "hermes", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".hermes", "config.yaml")
}

func (h *HermesInjector) Detect() (string, bool) {
	path := h.configPath()
	if path == "" {
		return path, false
	}
	if _, err := os.Stat(path); err != nil {
		return path, false
	}
	return path, true
}

// entryMap builds the YAML map for the sshmng entry. Same shape as Claude Code
// (command string, args list, env map) — yaml.v3 marshals map[string]any fine.
func (h *HermesInjector) entryMap(entry MCPEntry) map[string]any {
	return map[string]any{
		"command": entry.BinaryPath,
		"args":    entry.Args,
		"env":     entry.Env,
	}
}

func (h *HermesInjector) Inject(path string, entry MCPEntry) error {
	if err := backupFile(path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	m, err := loadYAMLMap(path)
	if err != nil {
		return err
	}
	mergeEntry(m, "mcp_servers", h.entryMap(entry))
	if err := writeYAMLMapAtomic(path, m); err != nil {
		return err
	}
	if _, err := loadYAMLMap(path); err != nil {
		return fmt.Errorf("post-write verify: %w", err)
	}
	return nil
}

func (h *HermesInjector) Verify(path string, expected MCPEntry) error {
	m, err := loadYAMLMap(path)
	if err != nil {
		return err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return fmt.Errorf("no mcp_servers key in %s", path)
	}
	sshmng, _ := servers["sshmng"].(map[string]any)
	if sshmng == nil {
		return fmt.Errorf("no sshmng entry in mcp_servers")
	}
	cmd, _ := sshmng["command"].(string)
	if cmd != expected.BinaryPath {
		return fmt.Errorf("stale: expected command %q, got %q", expected.BinaryPath, cmd)
	}
	if !argsEqualYAML(sshmng["args"], expected.Args) {
		return fmt.Errorf("stale: expected args %q, got %v", expected.Args, sshmng["args"])
	}
	env, _ := sshmng["env"].(map[string]any)
	if env == nil || env["SSHMNG_HOME"] != expectedHome(expected) {
		return fmt.Errorf("stale: expected env.SSHMNG_HOME %q, got %v", expectedHome(expected), env["SSHMNG_HOME"])
	}
	return nil
}

// loadYAMLMap reads path as YAML into a map. Empty/missing file -> empty map.
func loadYAMLMap(path string) (map[string]any, error) {
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
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s as YAML: %w", path, err)
	}
	return m, nil
}

// writeYAMLMapAtomic writes m as YAML to path atomically. Same pattern as
// writeJSONMapAtomic.
func writeYAMLMapAtomic(path string, m map[string]any) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal yaml: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".agent.yaml.tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
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
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
				return fmt.Errorf("remove old %s: %w (rename err: %v)", path, rmErr, err)
			}
			if err := os.Rename(tmpName, path); err != nil {
				// Original config was deleted above; restore from newest backup.
				// Spec line 288: "写入失败：从最新备份恢复，报错退出".
				restoreErr := restoreFromBackup(path)
				return fmt.Errorf("rename temp to %s: %w (restore from backup: %v; backup at %s.bak.<ts>)", path, err, restoreErr, path)
			}
			return nil
		}
		return fmt.Errorf("rename temp to %s: %w (backup at %s.bak.<ts> if exists)", path, err, path)
	}
	return nil
}
