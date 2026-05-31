package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
)

// CacheVersion is the only supported models-cache.json schema version.
const CacheVersion = 1

// cacheFileName is the basename used inside ConfigDir.
const cacheFileName = "models-cache.json"

// StaleAfter is the cache freshness window: caches older than 7 days are
// flagged stale; callers (skill, `cc-fleet models`) surface the flag but don't
// auto-refresh.
const StaleAfter = 7 * 24 * time.Hour

// Path returns the default cache path: <ConfigDir>/models-cache.json.
func Path() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, cacheFileName), nil
}

// Load reads and parses the default cache file.
//
// A missing file is NOT an error: an empty Cache (version=1, no vendors) is
// returned so first-run callers can mutate + Save it without special-casing.
func Load() (*Cache, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	return LoadFromPath(path)
}

// LoadFromPath is Load against an explicit path. Used by tests.
func LoadFromPath(path string) (*Cache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return emptyCache(), nil
		}
		return nil, fmt.Errorf("models: read %s: %w", path, err)
	}
	var c Cache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("models: parse %s: %w", path, err)
	}
	if c.Vendors == nil {
		c.Vendors = map[string]*VendorCache{}
	}
	return &c, nil
}

// Save writes c to the default cache path atomically with mode 0600.
func Save(c *Cache) error {
	path, err := Path()
	if err != nil {
		return err
	}
	return SaveToPath(c, path)
}

// SaveToPath writes c to path atomically (CreateTemp + rename) with mode 0600.
// The parent directory is created on demand at mode 0700.
func SaveToPath(c *Cache, path string) error {
	if c == nil {
		return errors.New("models: nil Cache")
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("models: marshal: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("models: mkdir %s: %w", dir, err)
	}

	if err := fileutil.AtomicWrite(path, data, 0o600); err != nil {
		return fmt.Errorf("models: write %s: %w", path, err)
	}
	return nil
}

// IsStale reports whether vc was fetched too long ago to trust without a
// refresh. A nil entry or zero FetchedAt is treated as stale so callers
// default to "tell the user to refresh".
func IsStale(vc *VendorCache) bool {
	if vc == nil {
		return true
	}
	if vc.FetchedAt.IsZero() {
		return true
	}
	return time.Since(vc.FetchedAt) >= StaleAfter
}

// emptyCache returns a fresh Cache at the current schema version.
func emptyCache() *Cache {
	return &Cache{Version: CacheVersion, Vendors: map[string]*VendorCache{}}
}
