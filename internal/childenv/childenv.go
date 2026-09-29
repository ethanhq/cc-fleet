// Package childenv builds the environment handed to a child claude process that
// cc-fleet launches — a one-shot subagent (`claude -p`) or an interactive `run`
// session. It only ever strips variables, never injects, so the lead's
// credentials and the nested-CC/teams markers cannot leak into the child.
// A provider teammate uses CleanForTeammate instead, which keeps the teams
// markers the agent-teams member needs.
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
		"ANTHROPIC_API_KEY":        true,
		"ANTHROPIC_AUTH_TOKEN":     true,
		"ANTHROPIC_CUSTOM_HEADERS": true,
		// Backend routing: a provider child gets its base URL from the profile, and
		// a native (reserved `claude`) child must talk to Anthropic itself — an
		// inherited override could silently reroute either to a foreign backend.
		"ANTHROPIC_BASE_URL": true,
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
// untouched. Load-bearing — see dropList for why each var must go. The native
// (reserved `claude`) child gets the SAME scrub: keys never ride env, so it
// authenticates only from claude's own stored login (file / OS keychain) — an
// env-key-only setup uses a configured `anthropic` provider instead.
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

// TeammateScrubKeys are the exact names the teammate launcher drops before it
// execs the provider teammate's claude: the lead's credentials, base URL and
// custom headers, its forced subagent model, and every cloud-backend / host-
// managed routing switch, so the teammate talks only to the profile's provider.
var TeammateScrubKeys = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS",
	"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_SUBAGENT_MODEL_FORCE",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
	"CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_GATEWAY",
	"AWS_BEARER_TOKEN_BEDROCK",
	"CLAUDE_CODE_HOST_CREDS_FILE", "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "CLAUDE_CODE_HOST_GATEWAY_LINEAGE",
}

// teammateScrubPrefixes drop the cloud backends' own settings (base URLs,
// regions, auth-skip switches). Upper-case canonical, like every exact key.
var teammateScrubPrefixes = []string{
	"ANTHROPIC_BEDROCK_", "ANTHROPIC_VERTEX_", "ANTHROPIC_AWS_", "ANTHROPIC_GOOGLE_CLOUD_",
}

// teammateScrub is TeammateScrubKeys plus ModelEnvKeys, snapshotted at init so a
// later mutation of the exported slice cannot shrink the scrub set. Every entry
// is an ANTHROPIC_*/CLAUDE*/AWS_BEARER_TOKEN_BEDROCK name, so the windows case
// fold can only match more variants of them (same invariant as dropList).
var teammateScrub = buildTeammateScrub()

func buildTeammateScrub() map[string]bool {
	d := make(map[string]bool, len(TeammateScrubKeys)+len(ModelEnvKeys))
	for _, k := range TeammateScrubKeys {
		d[k] = true
	}
	for _, k := range ModelEnvKeys {
		d[k] = true
	}
	return d
}

// hasTeammateScrubPrefix reports whether name (already case-folded by the
// platform matcher where needed) starts with a teammateScrubPrefixes entry.
func hasTeammateScrubPrefix(name string) bool {
	for _, p := range teammateScrubPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// CleanForTeammate returns environ with TeammateScrubKeys, the cloud-backend
// prefixes and ModelEnvKeys removed. Unlike Clean it keeps CLAUDECODE and
// CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS: the teammate is a real agent-teams
// member. It only removes, never injects; a line with no '=' passes through.
func CleanForTeammate(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		eq := strings.IndexByte(kv, '=')
		if eq >= 0 && inTeammateScrub(kv[:eq]) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
