package fingerprint

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/ethanhq/cc-fleet/internal/ccver"
)

// ResolveBinaryPath returns the claude binary to spawn with: the fingerprint's
// cached path if it STILL exists on disk, otherwise a live ccver.Detect()
// lookup. It decouples the (semi-static) spawn recipe from the
// (per-upgrade-volatile) binary path:
//
//   - bundled recipe (empty BinaryPath)      → live lookup
//   - post-upgrade cache (GC'd version path) → stale path drops out, live lookup
//   - valid cached path (incl. test fakes)   → kept as-is (no needless lookup,
//     and recipe+binary stay version-matched)
//
// Returns ErrFingerprintStale only when NO claude binary can be found anywhere
// — the one genuine "can't spawn" case left after dynamic resolution.
func ResolveBinaryPath(fp *Fingerprint) (string, error) {
	if fp != nil && fp.BinaryPath != "" {
		if _, err := os.Stat(fp.BinaryPath); err == nil {
			return fp.BinaryPath, nil
		}
	}
	binPath, _, err := ccver.Detect()
	if err != nil {
		return "", fmt.Errorf("%w: no claude binary found: %v", ErrFingerprintStale, err)
	}
	return binPath, nil
}

// versionForPath resolves the version of the binary at path. A var so tests can
// count invocations and prove the cache below short-circuits repeat calls.
var versionForPath = ccver.VersionForPath

var (
	versionCacheMu sync.Mutex
	versionCache   = map[string]string{}
)

// ResolveBinaryPathVersion resolves the binary to spawn with — identical
// semantics to ResolveBinaryPath — and ALSO the version OF THAT resolved path:
// the per-version basename if present, else a bounded `<path> --version`. This
// is the version of the executable that will actually run, never the PATH
// claude's version for a fingerprint-cached path.
//
// The (path → version) result is cached for the process lifetime, so repeated
// calls cost nothing. Version may legitimately be "" (unknown) — returned with
// no error; only a missing binary is an error (ErrFingerprintStale, via
// ResolveBinaryPath).
func ResolveBinaryPathVersion(fp *Fingerprint) (string, string, error) {
	path, err := ResolveBinaryPath(fp)
	if err != nil {
		return "", "", err
	}
	versionCacheMu.Lock()
	defer versionCacheMu.Unlock()
	if v, ok := versionCache[path]; ok {
		return path, v, nil
	}
	v := versionForPath(path)
	versionCache[path] = v
	return path, v, nil
}

// CurrentVersionExceedsRecipe reports whether the live Claude Code version is
// newer than the version fp's recipe was captured from. It is the gate for the
// post-spawn settle check: only when the running CC is newer than the recipe
// might the flag/env template be drifted, so only then is the extra liveness
// check worth its latency.
//
// Best-effort: if the live version can't be detected or either version can't
// be parsed, it returns false — we don't pay the settle cost on uncertainty,
// and a cc == recipe install (the common fresh case) skips it.
func CurrentVersionExceedsRecipe(fp *Fingerprint) bool {
	if fp == nil {
		return false
	}
	_, liveVer, err := ccver.Detect()
	if err != nil {
		return false
	}
	return semverGreater(liveVer, fp.CCVersion)
}

// semverGreater reports whether version a is strictly newer than b, comparing
// dotted numeric components left to right. A component that doesn't parse, or
// an empty version, makes the comparison conservatively return false (treated
// as "not newer" so we don't trigger drift handling on garbage).
func semverGreater(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		ai, err1 := strconv.Atoi(as[i])
		bi, err2 := strconv.Atoi(bs[i])
		if err1 != nil || err2 != nil {
			return false
		}
		if ai != bi {
			return ai > bi
		}
	}
	// All shared components equal → the one with more components is newer
	// (e.g. 2.1.150.1 > 2.1.150). Only treat extra components as "greater"
	// when they're present on a; equal length → not greater.
	return len(as) > len(bs)
}
