package onboarding

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/fileutil"
)

// AgentTeamsEnvVar is the env key Claude Code reads to enable its experimental
// agent-teams runtime (native TeamCreate / SendMessage).
const AgentTeamsEnvVar = "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"

// envTruthy reports whether a value (env var, JSON string, or rc assignment)
// means "enabled". Surrounding whitespace and quotes are stripped in one pass
// so ` "1" ` / `'1'` / ` 1 ` all normalize.
func envTruthy(v string) bool {
	switch strings.ToLower(strings.Trim(v, " \t\r\n\"'")) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// NeedsAgentTeamsSetup reports whether the first-run TUI should show the
// agent-teams setup nudge: the var is not configured in any source we can read
// AND the user hasn't dismissed the nudge before.
//
// IMPORTANT — this detects whether agent-teams has been EXPLICITLY CONFIGURED,
// NOT whether it is ENABLED at runtime. Claude commonly turns agent-teams on by
// default (GrowthBook) leaving no env var / flag, which an external process
// can't observe. So "not configured" ≠ "off": the nudge is therefore worded as
// a suggestion (never an assertion that it's off) and is shown at most once —
// after any choice, AgentTeamsAck is set.
func NeedsAgentTeamsSetup() bool {
	if runtime.GOOS == "windows" {
		return false // the teammate lane is unix-only; never park the Windows TUI on its setup
	}
	if AgentTeamsConfigured() {
		return false
	}
	st, _ := LoadState()
	return !st.AgentTeamsAck
}

// AgentTeamsConfigured reports whether the agent-teams env var is set truthy in
// ANY source cc-fleet can read: the current environment, the user's shell rc
// files, or Claude's global / project settings.json env block. Every probe is
// best-effort — an unreadable or malformed source is simply skipped.
func AgentTeamsConfigured() bool {
	if envTruthy(os.Getenv(AgentTeamsEnvVar)) {
		return true
	}
	if home := os.Getenv("HOME"); home != "" {
		for _, rc := range []string{".zshrc", ".bashrc", ".bash_profile", ".profile"} {
			if rcSetsAgentTeams(filepath.Join(home, rc)) {
				return true
			}
		}
		if settingsSetsAgentTeams(filepath.Join(home, ".claude", "settings.json")) {
			return true
		}
	}
	// Project-level settings, relative to the current working directory.
	for _, p := range []string{
		filepath.Join(".claude", "settings.json"),
		filepath.Join(".claude", "settings.local.json"),
	} {
		if settingsSetsAgentTeams(p) {
			return true
		}
	}
	return false
}

// rcAssignRe matches `...AGENT_TEAMS=<token>` in a shell rc file, with or
// without a leading `export` and optional surrounding quotes on the value.
var rcAssignRe = regexp.MustCompile(`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS\s*=\s*["']?([A-Za-z0-9]+)`)

func rcSetsAgentTeams(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, m := range rcAssignRe.FindAllStringSubmatch(string(data), -1) {
		if envTruthy(m[1]) {
			return true
		}
	}
	return false
}

// settingsSetsAgentTeams parses a Claude settings.json and reports whether its
// env block sets the var truthy. Uses RawMessage throughout so an unrelated
// non-string env value elsewhere can't make us miss ours.
func settingsSetsAgentTeams(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return false
	}
	envRaw, ok := root["env"]
	if !ok {
		return false
	}
	var env map[string]json.RawMessage
	if json.Unmarshal(envRaw, &env) != nil {
		return false
	}
	raw, ok := env[AgentTeamsEnvVar]
	if !ok {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return false
	}
	return envTruthy(s)
}

// settingsPath returns ~/.claude/settings.json. We read HOME directly (not
// os.UserHomeDir) so tests driving t.Setenv("HOME", tmp) stay hermetic.
func settingsPath() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return "", errors.New("onboarding: HOME is not set")
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// EnableAgentTeams idempotently merges
//
//	{"env": {"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1"}}
//
// into the user's MAIN ~/.claude/settings.json. It is invoked ONLY when the
// user explicitly chooses "enable it for me" in the first-run setup screen, so
// the write is consented. It is deliberately conservative:
//
//   - reads + parses the existing file (missing/empty → start from {});
//   - preserves EVERY other top-level key and env entry byte-for-byte
//     (json.RawMessage), touching only env.<var>;
//   - writes atomically at 0600 via fileutil.AtomicWrite (a failed write never
//     truncates the existing file);
//   - is idempotent: when the var is already "1" it writes nothing and returns
//     already=true.
//
// A top-level non-object document, or a non-object `env`, is a hard error — we
// refuse to clobber a shape we don't understand. The caller must tell the user
// to restart claude: the env block applies at claude STARTUP, so the write has
// no effect on the running session.
func EnableAgentTeams() (already bool, err error) {
	path, err := settingsPath()
	if err != nil {
		return false, err
	}

	root := map[string]json.RawMessage{}
	data, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &root); err != nil {
				return false, fmt.Errorf("onboarding: %s is not a JSON object: %w", path, err)
			}
		}
	case errors.Is(readErr, os.ErrNotExist):
		// Start from an empty object.
	default:
		return false, readErr
	}

	env := map[string]json.RawMessage{}
	if raw, ok := root["env"]; ok {
		if err := json.Unmarshal(raw, &env); err != nil {
			return false, fmt.Errorf("onboarding: %s env block is not a JSON object: %w", path, err)
		}
	}
	if cur, ok := env[AgentTeamsEnvVar]; ok {
		var s string
		if json.Unmarshal(cur, &s) == nil && s == "1" {
			return true, nil
		}
	}
	env[AgentTeamsEnvVar] = json.RawMessage(`"1"`)

	envBytes, err := json.Marshal(env)
	if err != nil {
		return false, err
	}
	root["env"] = envBytes

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	return false, fileutil.AtomicWrite(path, append(out, '\n'), 0o600)
}
