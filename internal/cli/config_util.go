package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/jim58246/sshmng/internal/config"
)

// ResolveConfigPath resolves explicit / $SSHMNG_HOME / $HOME/.sshmng/config.json.
func ResolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if home := os.Getenv("SSHMNG_HOME"); home != "" {
		return filepath.Join(home, "config.json"), nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(userHome, ".sshmng", "config.json"), nil
}

// BootstrapConfig resolves the config path, creates a Store, and loads the config.
// Returns the store (for Save), the loaded config, and any error.
func BootstrapConfig(configPath string) (*config.Store, *config.Config, error) {
	path, err := ResolveConfigPath(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve config path: %w", err)
	}
	if runtime.GOOS == "windows" {
		// NTFS uses ACL, not Unix permissions; skip the check.
	}
	store := config.NewStore(path)
	cfg, err := store.Load()
	if err != nil {
		return store, nil, fmt.Errorf("load config %s: %w", path, err)
	}
	return store, cfg, nil
}

// KnownHostsPath returns the known_hosts file path adjacent to the config file.
func KnownHostsPath(configPath string) string {
	path, _ := ResolveConfigPath(configPath)
	return filepath.Join(filepath.Dir(path), "known_hosts")
}
