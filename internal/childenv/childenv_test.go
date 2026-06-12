package childenv

import (
	"strings"
	"testing"
)

// TestClean_StripsTheLoadBearingVars: the env strip removes the lead's creds and
// the nested-CC/teams markers, keeps everything else (incl. a malformed no-'='
// line), and never re-injects a dropped marker.
func TestClean_StripsTheLoadBearingVars(t *testing.T) {
	in := []string{
		"ANTHROPIC_API_KEY=sk-leak",
		"ANTHROPIC_AUTH_TOKEN=tok-leak",
		"CLAUDECODE=1",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1",
		"PATH=/usr/bin",
		"HOME=/root",
		"NO_EQUALS_LINE",
	}
	out := Clean(in)

	for _, banned := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
		"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS",
	} {
		for _, kv := range out {
			if strings.HasPrefix(kv, banned+"=") {
				t.Fatalf("Clean leaked %q: %q", banned, kv)
			}
		}
	}
	// Defense in depth: no dropped marker or secret value survives anywhere.
	joined := strings.Join(out, "\n")
	for _, marker := range []string{"CLAUDECODE", "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "sk-leak", "tok-leak"} {
		if strings.Contains(joined, marker) {
			t.Fatalf("Clean output still contains %q: %q", marker, joined)
		}
	}
	if !containsLine(out, "PATH=/usr/bin") || !containsLine(out, "HOME=/root") {
		t.Fatalf("Clean dropped a keeper var: %v", out)
	}
	if !containsLine(out, "NO_EQUALS_LINE") {
		t.Fatalf("Clean dropped the malformed line: %v", out)
	}
}

// TestClean_StripsBaseURL: an inherited routing override could silently send
// any child — provider or native — to a foreign backend; it never survives.
func TestClean_StripsBaseURL(t *testing.T) {
	out := Clean([]string{"ANTHROPIC_BASE_URL=http://evil.example", "PATH=/usr/bin"})
	if strings.Contains(strings.Join(out, "\n"), "ANTHROPIC_BASE_URL") {
		t.Fatalf("Clean leaked ANTHROPIC_BASE_URL: %v", out)
	}
	if !containsLine(out, "PATH=/usr/bin") {
		t.Fatalf("Clean dropped a keeper var: %v", out)
	}
}

// TestClean_StripsModelEnvKeys: a model/effort var exported in the launching
// shell must not reach the child — the provider profile is the sole authority, so
// every ModelEnvKeys entry is stripped while unrelated vars survive.
func TestClean_StripsModelEnvKeys(t *testing.T) {
	var in []string
	for _, k := range ModelEnvKeys {
		in = append(in, k+"=leaked")
	}
	in = append(in, "PATH=/usr/bin")
	out := Clean(in)
	for _, k := range ModelEnvKeys {
		for _, kv := range out {
			if strings.HasPrefix(kv, k+"=") {
				t.Fatalf("Clean leaked model env %q: %q", k, kv)
			}
		}
	}
	if !containsLine(out, "PATH=/usr/bin") {
		t.Fatalf("Clean dropped an unrelated var: %v", out)
	}
}

// TestUpperKeys_FoldsToCanonical: the windows matcher's table folds case so a
// mixed-case env name resolves to its canonical upper-case dropList entry, while
// a non-dropList name (even one whose upper form differs) never gains an entry.
// Runs on every platform — it exercises the matcher logic without the windows tag.
func TestUpperKeys_FoldsToCanonical(t *testing.T) {
	tbl := upperKeys(dropList)
	for canonical := range dropList {
		for _, variant := range []string{
			strings.ToUpper(canonical),
			strings.ToLower(canonical),
		} {
			if !tbl[strings.ToUpper(variant)] {
				t.Fatalf("upperKeys missed %q (canonical %q)", variant, canonical)
			}
		}
	}
	if tbl[strings.ToUpper("PATH")] || tbl[strings.ToUpper("HOME")] {
		t.Fatalf("upperKeys gained a keeper var: %v", tbl)
	}
}

func containsLine(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}
