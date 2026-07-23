package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ScaffoldOpts controls ScaffoldHome behavior.
type ScaffoldOpts struct {
	// OverwriteConfig if true overwrites existing config.json. Default false
	// (preserve user data).
	OverwriteConfig bool
}

// ScaffoldHome creates the sshmng home directory and config files.
//   - Creates <home>/ (0700 on Unix)
//   - Writes <home>/config.json (0600, empty skeleton) — skipped if exists
//     and OverwriteConfig is false
//   - Writes <home>/config.example.json (0600, examples) — always overwritten
func ScaffoldHome(home string, opts ScaffoldOpts) error {
	if err := os.MkdirAll(home, 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", home, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(home, 0700); err != nil {
			return fmt.Errorf("chmod %s: %w", home, err)
		}
	}

	cfgPath := filepath.Join(home, "config.json")
	if opts.OverwriteConfig {
		if err := writeSecureFile(cfgPath, []byte(configJSONSkeleton)); err != nil {
			return err
		}
	} else if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := writeSecureFile(cfgPath, []byte(configJSONSkeleton)); err != nil {
			return err
		}
	}

	exPath := filepath.Join(home, "config.example.json")
	if err := writeSecureFile(exPath, []byte(configExampleJSON)); err != nil {
		return err
	}
	return nil
}

// writeSecureFile writes data to path with 0600 perms (Unix) or default
// (Windows). Truncates existing files.
func writeSecureFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0600); err != nil {
			return fmt.Errorf("chmod %s: %w", path, err)
		}
	}
	return nil
}

const configJSONSkeleton = `{
  "version": "1",
  "idle_timeout_s": 300,
  "auto_update_enabled": true,
  "jumphosts": [],
  "proxies": [],
  "servers": []
}
`

const configExampleJSON = `{
  "version": "1",
  "idle_timeout_s": 300,
  "log_level": "info",
  "log_path": "",
  "proxies": [
    {
      "name": "example-socks5",
      "type": "SOCKS5",
      "addr": "socks.corp:1080",
      "tags": ["example"]
    },
    {
      "name": "example-socks5-auth",
      "type": "SOCKS5",
      "addr": "socks.corp:1080",
      "auth": {"user": "proxy-user", "password": "<replace-me>"},
      "tags": ["example", "auth"]
    },
    {
      "name": "example-http-auth",
      "type": "HTTP",
      "addr": "proxy.corp:8080",
      "auth": {"user": "proxy-user", "password": "<replace-me>"},
      "tags": ["example", "auth"]
    }
  ],
  "jumphosts": [
    {
      "name": "example-jumphost-a",
      "addr": "10.0.0.254:22",
      "user": "ops",
      "auth": {"password": "<replace-me>"},
      "ssh_j": true,
      "tags": ["example", "pattern-a"]
    },
    {
      "name": "example-jumphost-a-via-proxy",
      "addr": "10.0.0.254:22",
      "user": "ops",
      "auth": {"password": "<replace-me>"},
      "ssh_j": true,
      "proxy": "example-socks5-auth",
      "tags": ["example", "pattern-a", "via-proxy"]
    },
    {
      "name": "example-jumphost-b",
      "addr": "10.0.0.254:22",
      "user": "ops",
      "auth": {"password": "<replace-me>"},
      "ssh_j": false,
      "login_flow": {
        "wait_menu": {
          "expects": [{"pattern": "Your choice:", "next": "success"}]
        }
      },
      "login_entry": "wait_menu",
      "tags": ["example", "pattern-b"]
    }
  ],
  "servers": [
    {
      "name": "example-server-a",
      "addr": "10.0.0.1:22",
      "user": "deploy",
      "auth": {"password": "<replace-me>"},
      "via": "example-jumphost-a",
      "tags": ["example", "pattern-a"]
    },
    {
      "name": "example-server-a-via-proxy",
      "addr": "10.0.0.1:22",
      "user": "deploy",
      "auth": {"password": "<replace-me>"},
      "via": "example-jumphost-a-via-proxy",
      "tags": ["example", "pattern-a", "via-proxy"]
    },
    {
      "name": "example-server-b",
      "addr": "10.0.0.1:22",
      "user": "deploy",
      "auth": null,
      "via": "example-jumphost-b",
      "login_flow": {
        "select_target": {"send": "1\r", "expects": [{"pattern": "Password:", "next": "input_pass"}]},
        "input_pass": {"send": "<replace-me>\r", "expects": [{"pattern": "$ ", "next": "success"}]}
      },
      "login_entry": "select_target",
      "tags": ["example", "pattern-b"]
    },
    {
      "name": "example-server-direct-password",
      "addr": "10.0.0.2:22",
      "user": "deploy",
      "auth": {"password": "<replace-me>"},
      "login_flow": {
        "wait_ps1": {
          "send": "",
          "expects": [{"pattern": "re:.*]# ", "next": "success"}]
        }
      },
      "login_entry": "wait_ps1",
      "tags": ["example", "direct", "login-flow"]
    },
    {
      "name": "example-server-direct-key",
      "addr": "10.0.0.3:22",
      "user": "deploy",
      "auth": {
        "private_key": "/home/user/.ssh/deploy_key",
        "passphrase": "<replace-me>"
      },
      "tags": ["example", "direct", "private-key"]
    }
  ]
}
`
