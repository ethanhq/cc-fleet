package fingerprint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// envAllowlist is the closed set of environment variables we copy from the
// probe's /proc/<pid>/environ into the fingerprint. These two suffice for the
// native Agent's spawn — every other provider-agnostic var either comes from the
// user's shell on demand or is set by the spawning shell wrapper.
var envAllowlist = []string{
	"CLAUDECODE",
	"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS",
}

// modelFlag is removed from the captured template — cc-fleet always appends
// its own --model <provider-model-id> at spawn time.
const modelFlag = "--model"

// settingsFlag is removed for the SAME reason as modelFlag: cc-fleet always
// appends its own --settings <provider-profile>.json at spawn time. A captured
// --settings is either absent (a native Agent probe — the intended source) or a
// provider-profile path (if the probe snapshotted was itself a provider teammate).
// Either way it must be stripped: leaving a provider's --settings in the "native"
// template freezes that provider's base_url/apiKeyHelper into EVERY later spawn —
// it lands first, so the request hits the wrong provider's endpoint carrying
// another provider's model → "model not found" for all non-matching providers.
// removeFlagPair drops all occurrences, so a doubly-tainted capture is cleaned
// too.
const settingsFlag = "--settings"

// ccVersionRegex extracts a semver-ish version string from a path basename.
// Captures the trailing dotted-numeric portion of e.g.
// "/root/.local/share/claude/versions/2.1.150".
var ccVersionRegex = regexp.MustCompile(`(\d+\.\d+\.\d+)$`)

// versionLookupTimeout caps the fallback `<binary> --version` exec when the
// binary path lacks a semver suffix.
const versionLookupTimeout = 5 * time.Second

// CaptureFromPid reads /proc/<pid>/{cmdline,environ}, extracts the env vars +
// flag template (with placeholders), looks up the cc_version, and returns a
// ready-to-save Fingerprint. /proc is Linux-only; macOS uses captureFromPidDarwin.
func CaptureFromPid(pid int) (*Fingerprint, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("fingerprint: invalid pid %d", pid)
	}
	// macOS has no /proc. The flag template (the drift-prone part) is recoverable
	// from `ps` (procintrospect.Cmdline); the env part is the two known constants
	// in envAllowlist (macOS can't read another process's environ, but doesn't
	// need to). captureFromPidDarwin assembles the Fingerprint from those.
	if runtime.GOOS == "darwin" {
		return captureFromPidDarwin(pid)
	}
	cmdlinePath := fmt.Sprintf("/proc/%d/cmdline", pid)
	environPath := fmt.Sprintf("/proc/%d/environ", pid)
	return CaptureFromFiles(cmdlinePath, environPath)
}

// CaptureFromFiles is the low-level helper that backs CaptureFromPid. Tests
// pass mock files in a t.TempDir() so we never depend on a live process.
func CaptureFromFiles(cmdlinePath, environPath string) (*Fingerprint, error) {
	cmdlineRaw, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: read cmdline %s: %w", cmdlinePath, err)
	}
	environRaw, err := os.ReadFile(environPath)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: read environ %s: %w", environPath, err)
	}

	argv := splitNul(cmdlineRaw)
	if len(argv) == 0 {
		return nil, errors.New("fingerprint: cmdline is empty")
	}
	binaryPath := argv[0]
	if binaryPath == "" {
		return nil, errors.New("fingerprint: argv[0] (binary_path) is empty")
	}

	envMap := filterEnv(splitNul(environRaw), envAllowlist)

	flagsTemplate := templatize(argv[1:])

	ccVersion := versionFromBinaryPath(binaryPath)
	if ccVersion == "" {
		// Fallback: ask the binary itself. We bound the call so a stuck binary
		// can't wedge the probe.
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

// splitNul splits a NUL-separated buffer into a string slice, dropping the
// trailing empty token that /proc/.../cmdline and .../environ always have.
func splitNul(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	parts := bytes.Split(b, []byte{0})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		out = append(out, string(p))
	}
	return out
}

// filterEnv reduces an environ entry list (KEY=VALUE strings) to the keys we
// care about. Entries without '=' or whose key is not in allow are skipped.
func filterEnv(entries, allow []string) map[string]string {
	allowed := make(map[string]struct{}, len(allow))
	for _, k := range allow {
		allowed[k] = struct{}{}
	}
	out := make(map[string]string, len(allow))
	for _, e := range entries {
		idx := strings.IndexByte(e, '=')
		if idx <= 0 {
			continue
		}
		key := e[:idx]
		if _, ok := allowed[key]; !ok {
			continue
		}
		out[key] = e[idx+1:]
	}
	return out
}

// templatize transforms the captured argv tail into a placeholder template:
//   - drops "--model <value>" (cc-fleet appends its own model flag at spawn)
//   - drops "--settings <value>" (cc-fleet appends its own provider profile at
//     spawn; a captured provider --settings would otherwise taint every spawn —
//     see settingsFlag)
//   - replaces provider-agnostic but spawn-specific values with {placeholder}
//
// Unknown flags are kept verbatim — e.g. --agent-type general-purpose and
// --dangerously-skip-permissions pass through unchanged. The order of
// arguments is preserved so the produced template still corresponds to what
// Claude Code currently expects.
func templatize(args []string) []string {
	args = removeFlagPair(args, modelFlag)
	args = removeFlagPair(args, settingsFlag)

	args = renameArgPair(args, "--agent-id", func(string) string {
		// We deliberately don't parse "<name>@<team>" here — we substitute the
		// whole pair so Apply can put exactly that shape back.
		return "{name}@{team}"
	})
	args = renameArgPair(args, "--agent-name", func(string) string { return "{name}" })
	args = renameArgPair(args, "--team-name", func(string) string { return "{team}" })
	args = renameArgPair(args, "--agent-color", func(string) string { return "{color}" })
	args = renameArgPair(args, "--parent-session-id", func(string) string { return "{lead_session_id}" })

	return args
}

// renameArgPair walks args looking for a `--flag value` pair (flags use space
// separation in claude's cmdline, never `--flag=value`). When it sees flagName
// it rewrites the immediately following value via transform. If flagName
// appears without a successor token, the trailing flag is left as-is — we
// never want to silently drop user data.
func renameArgPair(args []string, flagName string, transform func(string) string) []string {
	if len(args) == 0 {
		return args
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		out = append(out, args[i])
		if args[i] == flagName && i+1 < len(args) {
			out = append(out, transform(args[i+1]))
			i++
		}
	}
	return out
}

// removeFlagPair strips one `--flag value` pair from args. If the flag is
// present but has no successor token, only the flag itself is removed.
func removeFlagPair(args []string, flagName string) []string {
	if len(args) == 0 {
		return args
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flagName {
			// Skip its value too, if any.
			if i+1 < len(args) {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// versionFromBinaryPath tries to pull a semver-ish suffix off the binary
// path's basename (e.g. /root/.local/share/claude/versions/2.1.150).
func versionFromBinaryPath(binaryPath string) string {
	base := filepath.Base(binaryPath)
	m := ccVersionRegex.FindStringSubmatch(base)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

// versionFromBinaryExec invokes `<binary> --version` as a fallback. We accept
// any whitespace-separated token that matches ccVersionRegex from stdout.
// Returns "" on any error so the caller can decide how to surface it.
func versionFromBinaryExec(binaryPath string) string {
	if binaryPath == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionLookupTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, binaryPath, "--version").Output()
	if err != nil {
		return ""
	}
	for _, field := range strings.Fields(string(out)) {
		if m := ccVersionRegex.FindStringSubmatch(field); len(m) >= 2 {
			return m[1]
		}
	}
	return ""
}
