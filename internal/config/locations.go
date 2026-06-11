package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethanhq/cc-fleet/internal/homedir"
)

// appDirName is the cc-fleet subdirectory inside the XDG config directory.
const appDirName = "cc-fleet"

// ConfigDir returns the cc-fleet config directory.
//
// Resolution order:
//  1. $XDG_CONFIG_HOME/cc-fleet  (if XDG_CONFIG_HOME is set and non-empty)
//  2. <home>/.config/cc-fleet    ($HOME on unix, %USERPROFILE% on windows)
//
// Returns an error if XDG_CONFIG_HOME is unset and the user's home can't be resolved.
func ConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, appDirName), nil
	}
	home, err := homedir.Home()
	if err != nil {
		return "", fmt.Errorf("config: resolve home: %w", err)
	}
	return filepath.Join(home, ".config", appDirName), nil
}

// ProvidersPath returns the absolute path to providers.toml inside ConfigDir.
func ProvidersPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "providers.toml"), nil
}

// SecretsDir returns the absolute path to the secrets/ directory inside ConfigDir.
func SecretsDir() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "secrets"), nil
}
