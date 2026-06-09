// Package version exposes the cc-fleet build version.
package version

import (
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
)

// devDefault marks a build that was not stamped with a release version.
const devDefault = "0.1.0-dev"

// Version is the cc-fleet release identifier. Release builds override it at
// link time with -ldflags "-X github.com/ethanhq/cc-fleet/internal/version.Version=<tag>".
var Version = devDefault

// Resolve returns the build version, in priority order:
//   - a link-time-stamped Version (release archives / curl / npm binaries),
//   - the clean module tag recorded for a `go install ...@<tag>` build,
//   - the dev default for a plain local build.
//
// A local repo build records "(devel)" or a VCS pseudo-version
// ("v0.0.0-<timestamp>-<commit>", optionally "+dirty") in the build info; those
// fall through to the readable dev default rather than a long pseudo-version.
func Resolve() string {
	if Version != devDefault {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		v := info.Main.Version
		if v != "" && v != "(devel)" &&
			!strings.HasPrefix(v, "v0.0.0-") && !strings.Contains(v, "+dirty") {
			return v
		}
	}
	return Version
}

// releaseRE matches a comparable release version: MAJOR.MINOR.PATCH with an
// optional leading v and nothing trailing. The dev default ("0.1.0-dev") and
// VCS pseudo-versions ("v0.1.7-0.<date>-<sha>") are deliberately excluded.
var releaseRE = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)

// IsRelease reports whether v is a comparable release version. Update checks
// and the startup prompt only act on releases; a non-release build (dev default
// or pseudo-version) is reported as "not comparable" and never prompts.
func IsRelease(v string) bool {
	return releaseRE.MatchString(strings.TrimSpace(v))
}

// Normalize strips a leading v and surrounding space so "v0.1.6" and "0.1.6"
// compare equal (the binary carries the git tag "v0.1.6"; the plugin manifest
// carries the bare "0.1.6").
func Normalize(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// parseRelease parses a release into its three integer components; ok is false
// when v is not a comparable release.
func parseRelease(v string) ([3]int, bool) {
	if !IsRelease(v) {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range strings.Split(Normalize(v), ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// Newer reports whether release a is strictly newer than release b. It returns
// false if either side is not a comparable release, so a dev/pseudo current
// version can never be told an update is available (or that it is a downgrade).
func Newer(a, b string) bool {
	av, aok := parseRelease(a)
	bv, bok := parseRelease(b)
	if !aok || !bok {
		return false
	}
	for i := 0; i < 3; i++ {
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	return false
}
