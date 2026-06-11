// Package childenv builds the environment handed to a child claude process that
// cc-fleet launches — a one-shot subagent (`claude -p`) or an interactive `run`
// session. It only ever strips variables, never injects, so the lead's
// credentials and the nested-CC/teams markers cannot leak into the child.
package childenv

import "strings"

// ModelEnvKeys are the model/effort-selection vars the provider profile owns. A
// child must not inherit any of these from the launching shell, or an operator's
// exported value would override the profile (these env vars outrank the profile's
// settings effortLevel, and ANTHROPIC_MODEL would shadow --model). The subagent /
// run path strips them here; the spawn teammate path unsets the SAME set in its
// `env -u` prefix — one exported list so the two launchers can't drift on the
// policy. The profile then re-injects the per-provider values.
var ModelEnvKeys = []string{
	"ANTHROPIC_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"CLAUDE_CODE_SUBAGENT_MODEL",
	"CLAUDE_CODE_EFFORT_LEVEL",
}

// dropList is the variables Clean removes. Key-safety boundary: unexported so no
// other package can mutate the scrub set, and shared (not duplicated) by subagent
// and run. Built from the credential + nested-CC/teams markers plus ModelEnvKeys.
//
// Case-fold invariant (relied on by the windows matcher in match_windows.go):
// every entry is an ANTHROPIC_*/CLAUDE* name, so folding case can only ever match
// MORE credential/marker variants — never a legitimate var. A future generic-word
// entry (e.g. "PATH") would break this and must not be added.
var dropList = buildDropList()

func buildDropList() map[string]bool {
	d := map[string]bool{
		// Key-safety: never let the lead's subscription creds reach the provider call;
		// provider auth must come solely from the profile's apiKeyHelper.
		"ANTHROPIC_API_KEY":    true,
		"ANTHROPIC_AUTH_TOKEN": true,
		// Nested-CC / teams markers. A child launched from inside the lead's session
		// inherits these via os.Environ(); leaving CLAUDECODE=1 marks the child as
		// "nested in CC" (alters/refuses the run), and the teams trigger would make a
		// non-teammate behave like one. We never re-apply them.
		"CLAUDECODE":                           true,
		"CLAUDE_CODE_ENTRYPOINT":               true,
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": true,
	}
	for _, k := range ModelEnvKeys {
		d[k] = true
	}
	return d
}

// upperKeys returns a copy of m keyed by the upper-cased name. The windows
// matcher uses it to fold case-insensitive env names onto the canonical
// upper-case dropList entries; kept here (not in match_windows.go) so it builds
// and is testable on every platform.
func upperKeys(m map[string]bool) map[string]bool {
	u := make(map[string]bool, len(m))
	for k := range m {
		u[strings.ToUpper(k)] = true
	}
	return u
}

// Clean returns environ (os.Environ() form) with dropList entries removed. It
// only removes; it never injects. A line with no '=' is passed through
// untouched. Load-bearing — see dropList for why each var must go.
func Clean(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		eq := strings.IndexByte(kv, '=')
		if eq >= 0 && inDropList(kv[:eq]) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
