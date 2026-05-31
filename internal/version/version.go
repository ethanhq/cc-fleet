// Package version exposes the cc-fleet build version.
package version

import (
	"runtime/debug"
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
