package config

import (
	"errors"
	"os"
	"path/filepath"
)

// appDirName is the cc-fleet subdirectory inside the XDG config directory.
const appDirName = "cc-fleet"

// ConfigDir returns the cc-fleet config directory.
//
// Resolution order:
//  1. $XDG_CONFIG_HOME/cc-fleet  (if XDG_CONFIG_HOME is set and non-empty)
//  2. $HOME/.config/cc-fleet     (fallback)
//
// Returns an error if neither XDG_CONFIG_HOME nor HOME is set.
func ConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, appDirName), nil
	}
	home := os.Getenv("HOME")
	if home == "" {
		return "", errors.New("config: neither XDG_CONFIG_HOME nor HOME is set")
	}
	return filepath.Join(home, ".config", appDirName), nil
}

// VendorsPath returns the absolute path to vendors.toml inside ConfigDir.
func VendorsPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "vendors.toml"), nil
}

// SecretsDir returns the absolute path to the secrets/ directory inside ConfigDir.
func SecretsDir() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "secrets"), nil
}
