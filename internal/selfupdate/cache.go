package selfupdate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/version"
)

// checkInterval bounds both how often the background refresh re-queries GitHub
// and how long a dismissal suppresses the startup prompt.
const checkInterval = 24 * time.Hour

// OptOutEnv disables the startup update check entirely when set non-empty.
const OptOutEnv = "CC_FLEET_NO_UPDATE_CHECK"

// checkCache is the on-disk state behind the startup prompt. It is driven purely
// from this cache (no network on the launch path); a background refresh keeps it
// current for the next launch.
type checkCache struct {
	LastChecked  int64  `json:"last_checked"` // unix seconds of the last GitHub query
	LatestTag    string `json:"latest_tag"`
	DismissedTag string `json:"dismissed_tag,omitempty"`
	DismissedAt  int64  `json:"dismissed_at,omitempty"`
}

// CheckCachePath returns ConfigDir/update-check.json — the on-disk cache
// behind the startup update prompt. Exported so uninstall can remove it.
func CheckCachePath() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "update-check.json"), nil
}

func cacheLockPath() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".cc-fleet-update-cache.lock"), nil
}

func loadCache() checkCache {
	var c checkCache
	p, err := CheckCachePath()
	if err != nil {
		return c
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c) // a corrupt cache is treated as empty
	return c
}

func saveCache(c checkCache) error {
	p, err := CheckCachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return fileutil.AtomicWrite(p, data, 0o600)
}

func withCacheLock(fn func() error) error {
	p, err := cacheLockPath()
	if err != nil {
		return err
	}
	return config.WithFlock(p, fn)
}

// OptedOut reports whether the startup update check is disabled by env.
func OptedOut() bool { return os.Getenv(OptOutEnv) != "" }

// PromptTag returns a newer release tag to prompt about (ok=true) when the
// cache holds a comparable release newer than the running binary that has not
// been dismissed within checkInterval. It is a pure cache read — no network —
// and returns false for a dev/non-release build or when opted out.
func PromptTag(now time.Time) (string, bool) {
	if OptedOut() {
		return "", false
	}
	cur := version.Resolve()
	if !version.IsRelease(cur) {
		return "", false
	}
	c := loadCache()
	if c.LatestTag == "" || !version.Newer(c.LatestTag, cur) {
		return "", false
	}
	if c.DismissedTag == c.LatestTag && now.Unix()-c.DismissedAt < int64(checkInterval/time.Second) {
		return "", false
	}
	return c.LatestTag, true
}

// Dismiss records that the user was prompted about tag and declined, so the
// prompt stays quiet for checkInterval. Locked read-merge-write.
func Dismiss(tag string, now time.Time) error {
	return withCacheLock(func() error {
		c := loadCache()
		c.DismissedTag = tag
		c.DismissedAt = now.Unix()
		return saveCache(c)
	})
}

// RefreshCache re-queries GitHub for the latest tag when the cache is older than
// checkInterval and records it (locked). Meant to run in a background goroutine
// so the launch never blocks on the network; an offline/transient failure
// leaves the cache untouched (no prompt, no error).
func RefreshCache(ctx context.Context, now time.Time) {
	if OptedOut() {
		return
	}
	if c := loadCache(); c.LastChecked != 0 && now.Unix()-c.LastChecked < int64(checkInterval/time.Second) {
		return // still fresh
	}
	tag, err := LatestTag(ctx)
	if err != nil {
		return
	}
	_ = withCacheLock(func() error {
		c := loadCache() // re-read under the lock to preserve a concurrent dismissal
		c.LastChecked = now.Unix()
		c.LatestTag = tag
		return saveCache(c)
	})
}
