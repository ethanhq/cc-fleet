package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestAgentSlimKwargPlumbing: profile=/tools=/skills=/mcp= reach the leaf Request — the
// REQUESTED profile (Run re-resolves the effective one), the canonicalized (dedupe+sort)
// tool set, and the inverted NoSkills / MCP toggles.
func TestAgentSlimKwargPlumbing(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "slimp", 2, echoLeaf(rec), `
x = agent("p", vendor="v", profile="slim", tools=["Read", "Bash", "Read"][:2], skills=False, mcp=True)
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	c := rec.snapshot()[0]
	if c.promptProfile != "slim" {
		t.Errorf("PromptProfile = %q, want slim (the REQUESTED profile)", c.promptProfile)
	}
	if strings.Join(c.tools, ",") != "Bash,Read" {
		t.Errorf("Tools = %v, want [Bash Read] (canonicalized dedupe+sort)", c.tools)
	}
	if !c.noSkills {
		t.Error("skills=False must set NoSkills=true")
	}
	if !c.mcp {
		t.Error("mcp=True must set MCP=true")
	}
}

// TestAgentSlimDefaults: profile omitted is full, and a bare slim leaf gets skills on
// (NoSkills=false) + mcp off + the RESOLVED profile default tool set fed to the leaf — so
// the leaf execs exactly the set that was keyed (no nil-tools divergence).
func TestAgentSlimDefaults(t *testing.T) {
	// Pin the effective profile to the requested one so the resolved default tool set is
	// asserted deterministically regardless of the host claude version.
	oldR := resolveProfile
	resolveProfile = func(requested string) (string, string) { return requested, "" }
	t.Cleanup(func() { resolveProfile = oldR })

	rec := &recorder{}
	_, err := runScript(t, "slimd", 2, echoLeaf(rec), `
a = agent("a", vendor="v")
b = agent("b", vendor="v", profile="slim")
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	byPrompt := map[string]leafCall{}
	for _, c := range rec.snapshot() {
		byPrompt[c.prompt] = c
	}
	if a := byPrompt["a"]; a.promptProfile != subagent.ProfileFull {
		t.Errorf("default profile = %q, want full", a.promptProfile)
	}
	wantTools, _ := subagent.CanonicalizeTools(subagent.DefaultSlimTools(subagent.ProfileSlim, false))
	if b := byPrompt["b"]; b.noSkills || b.mcp || strings.Join(b.tools, ",") != strings.Join(wantTools, ",") {
		t.Errorf("bare slim leaf = %+v, want NoSkills=false MCP=false Tools=%v", b, wantTools)
	}
}

// TestAgentSlimValidationErrors: every front-loaded slim validation rejects with a
// Starlark error (no leaf exec) — refinements with full, a bad profile, and a bad tool.
func TestAgentSlimValidationErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"tools-with-full", `agent("p", vendor="v", tools=["Read"])`, "slim-only"},
		{"skills-with-full", `agent("p", vendor="v", skills=False)`, "slim-only"},
		{"mcp-with-full", `agent("p", vendor="v", mcp=True)`, "slim-only"},
		{"bad-profile", `agent("p", vendor="v", profile="turbo")`, "unknown prompt profile"},
		{"unknown-tool", `agent("p", vendor="v", profile="slim", tools=["Nope"])`, "unknown tool"},
		{"duplicate-tool", `agent("p", vendor="v", profile="slim", tools=["Read", "Read"])`, "duplicate tool"},
		{"skill-with-skills-off", `agent("p", vendor="v", profile="slim", tools=["Read", "Skill"], skills=False)`, "contradictory with skills disabled"},
		{"bad-tools-type", `agent("p", vendor="v", profile="slim", tools="Read")`, "must be a list"},
		{"bad-profile-type", `agent("p", vendor="v", profile=7)`, "must be a string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			_, err := runScript(t, "slimv", 2, echoLeaf(rec), "x = "+tc.src)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if n := len(rec.snapshot()); n != 0 {
				t.Errorf("a rejected leaf must NOT exec, got %d leaf calls", n)
			}
		})
	}
}

// TestAgentSlimDowngradeLogsAndKeysFull: when the version gate downgrades a slim request
// to full, agent() logs a one-line notice BEFORE the journal lookup (visible even on a
// cache hit) AND keys the leaf as full — so a journal entry pre-seeded under the full key
// is served. Exercised through the Execute path so the events file is wired.
func TestAgentSlimDowngradeLogsAndKeysFull(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Force the gate to downgrade any slim request to full with a reason.
	oldR := resolveProfile
	resolveProfile = func(requested string) (string, string) {
		if requested == subagent.ProfileFull || requested == "" {
			return requested, ""
		}
		return subagent.ProfileFull, "claude 2.1.50 below floor 2.1.88"
	}
	t.Cleanup(func() { resolveProfile = oldR })

	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })

	_, script := writeScript(t, `x = agent("q", vendor="v", profile="slim")`)
	run, err := Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	// Pre-seed the journal with the FULL key for this leaf. If the downgraded slim leaf
	// keys as full, this cached entry is served — no leaf exec, a leaf:cached event.
	jp, _ := subagent.RunJournalPath(run.RunID)
	loadJournal(jp).append(fullKey("v", "", "q", "", ""), "CACHED")
	if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if n := len(rec.snapshot()); n != 0 {
		t.Errorf("downgraded slim leaf must hit the FULL-key cache (no exec), got %d calls", n)
	}
	// The downgrade notice was logged (fires before the journal lookup, so even a cache
	// hit emits it).
	ep, _ := subagent.RunEventsPath(run.RunID)
	var loggedDowngrade, cached bool
	for _, r := range readEvents(t, ep) {
		if r.Kind == "log" && strings.Contains(r.Msg, "below floor") {
			loggedDowngrade = true
		}
		if r.Kind == "leaf" && r.Status == "cached" {
			cached = true
		}
	}
	if !loggedDowngrade {
		t.Error("a version-gate downgrade must log a notice (visible even on a cache hit)")
	}
	if !cached {
		t.Error("the downgraded leaf must serve the full-key cache entry (leaf:cached)")
	}
}

// TestAgentSlimResumeKeysByEffective: two runs whose gate resolves DIFFERENT effective
// profiles for the same slim request key differently — a cross-version resume re-executes
// rather than replaying a wrong-shape answer. Asserted at the key layer via the resolver
// seam.
func TestAgentSlimResumeKeysByEffective(t *testing.T) {
	// Effective slim → folds the slim shape; effective full (downgrade) → keys as full.
	asSlim := journalKey("v", "m", "p", "", "", "slim", []string{"Bash", "Read"}, false, false)
	asFull := journalKey("v", "m", "p", "", "", subagent.ProfileFull, []string{"Bash", "Read"}, false, false)
	if asSlim == asFull {
		t.Fatal("a slim and a (downgraded) full resolution of the same request must key differently")
	}
}
