package fingerprint

import (
	"reflect"
	"testing"
)

// templatedFP is a Fingerprint whose FlagsTemplate carries the full set of
// placeholders that templatize() introduces. Apply() must restore every one
// of them to the values from SpawnContext, leaving non-placeholder tokens
// (e.g. --agent-type, general-purpose, --dangerously-skip-permissions)
// untouched.
func templatedFP() *Fingerprint {
	return &Fingerprint{
		CCVersion:  "2.1.150",
		BinaryPath: "/root/.local/share/claude/versions/2.1.150",
		Env: map[string]string{
			"CLAUDECODE":                           "1",
			"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1",
		},
		FlagsTemplate: []string{
			"--agent-id", "{name}@{team}",
			"--agent-name", "{name}",
			"--team-name", "{team}",
			"--agent-color", "{color}",
			"--parent-session-id", "{lead_session_id}",
			"--agent-type", "general-purpose",
			"--dangerously-skip-permissions",
		},
	}
}

func TestApply_FullSubstitution(t *testing.T) {
	fp := templatedFP()
	ctx := SpawnContext{
		Name:          "alice",
		Team:          "alpha",
		Color:         "magenta",
		LeadSessionID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
	}
	got := Apply(fp, ctx)
	want := []string{
		"--agent-id", "alice@alpha",
		"--agent-name", "alice",
		"--team-name", "alpha",
		"--agent-color", "magenta",
		"--parent-session-id", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"--agent-type", "general-purpose",
		"--dangerously-skip-permissions",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Apply mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestApply_ContainsAgentIdAlphaBeta(t *testing.T) {
	// Explicit smoke test: after Apply, the --agent-id alice@alpha pair must
	// appear in the output.
	fp := templatedFP()
	ctx := SpawnContext{Name: "alice", Team: "alpha", Color: "blue", LeadSessionID: "x"}
	got := Apply(fp, ctx)
	if !containsPair(got, "--agent-id", "alice@alpha") {
		t.Fatalf("Apply output missing --agent-id alice@alpha; got %v", got)
	}
	if !containsPair(got, "--agent-name", "alice") {
		t.Fatalf("Apply output missing --agent-name alice; got %v", got)
	}
	if !containsPair(got, "--team-name", "alpha") {
		t.Fatalf("Apply output missing --team-name alpha; got %v", got)
	}
	if !containsPair(got, "--agent-color", "blue") {
		t.Fatalf("Apply output missing --agent-color blue; got %v", got)
	}
}

func containsPair(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestApply_NilFingerprint(t *testing.T) {
	if got := Apply(nil, SpawnContext{Name: "x"}); got != nil {
		t.Fatalf("Apply(nil, ...) = %v, want nil", got)
	}
}

func TestApply_RoundTripFromCapture(t *testing.T) {
	// Capture (against the real probe sample) → Apply with the original
	// concrete values → output should match the original argv tail except
	// `--model claude-opus-4-7` is gone (cc-fleet adds its own).
	cmd, env := writeMockProc(t, t.TempDir(), realProbeCmdline, realProbeEnviron)
	fp, err := CaptureFromFiles(cmd, env)
	if err != nil {
		t.Fatalf("CaptureFromFiles: %v", err)
	}

	got := Apply(fp, SpawnContext{
		Name:          "probe1",
		Team:          "probe-native",
		Color:         "blue",
		LeadSessionID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
	})

	want := []string{
		"--agent-id", "probe1@probe-native",
		"--agent-name", "probe1",
		"--team-name", "probe-native",
		"--agent-color", "blue",
		"--parent-session-id", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		"--agent-type", "general-purpose",
		"--dangerously-skip-permissions",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestApply_PreservesUnknownFlags(t *testing.T) {
	// If a future cc release introduces a flag templatize doesn't know about,
	// it must pass through Apply verbatim.
	fp := &Fingerprint{
		FlagsTemplate: []string{
			"--agent-name", "{name}",
			"--brand-new-flag", "fixed-value",
		},
	}
	got := Apply(fp, SpawnContext{Name: "alice"})
	want := []string{"--agent-name", "alice", "--brand-new-flag", "fixed-value"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Apply mismatch:\n got: %v\nwant: %v", got, want)
	}
}
