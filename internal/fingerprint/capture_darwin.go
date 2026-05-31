//go:build darwin

package fingerprint

import (
	"errors"
	"fmt"
	"time"

	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

// captureFromPidDarwin builds a Fingerprint from a live probe agent on macOS,
// where /proc is unavailable. It mirrors CaptureFromFiles but sources its two
// inputs differently:
//
//   - flags + binary_path: from `ps` (procintrospect.Cmdline) instead of
//     /proc/<pid>/cmdline. ps space-joins argv, but the captured claude flags
//     (and the binary path) never contain whitespace, so the split is exact.
//   - env: macOS forbids reading another process's environ, but the two
//     allowlisted vars (envAllowlist: CLAUDECODE, CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS)
//     are KNOWN constants ("1"), already hardcoded in the bundled recipe and
//     managed by cc-fleet itself — so we synthesize them rather than read them.
//
// cc_version + templatize + versionFromBinaryPath/Exec are the SAME helpers the
// Linux path uses, so the resulting Fingerprint is shape-identical.
func captureFromPidDarwin(pid int) (*Fingerprint, error) {
	argv, err := procintrospect.Cmdline(pid)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: ps cmdline for pid %d: %w", pid, err)
	}
	if len(argv) == 0 {
		return nil, errors.New("fingerprint: cmdline is empty")
	}
	binaryPath := argv[0]
	if binaryPath == "" {
		return nil, errors.New("fingerprint: argv[0] (binary_path) is empty")
	}

	// The two allowlisted env vars are stable "1" constants on macOS (can't read
	// the probe's environ; don't need to). Build from envAllowlist so this stays
	// in sync if the allowlist ever changes.
	envMap := make(map[string]string, len(envAllowlist))
	for _, k := range envAllowlist {
		envMap[k] = "1"
	}

	flagsTemplate := templatize(argv[1:])

	ccVersion := versionFromBinaryPath(binaryPath)
	if ccVersion == "" {
		ccVersion = versionFromBinaryExec(binaryPath)
	}
	if ccVersion == "" {
		return nil, fmt.Errorf("fingerprint: could not determine cc_version from %q", binaryPath)
	}

	return &Fingerprint{
		CCVersion:     ccVersion,
		CapturedAt:    time.Now().UTC(),
		BinaryPath:    binaryPath,
		Env:           envMap,
		FlagsTemplate: flagsTemplate,
	}, nil
}
