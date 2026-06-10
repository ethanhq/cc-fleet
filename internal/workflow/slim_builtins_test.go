package workflow

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestAgentSlimOptionPlumbing: profile/tools/skills/mcp reach the leaf Request — the
// REQUESTED profile (Run re-resolves the effective one), the canonicalized (dedupe+sort)
// tool set, the inverted NoSkills toggle, and an explicit mcp of either value (an
// explicit false beats slim's inherit default).
func TestAgentSlimOptionPlumbing(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "slimp", 2, echoLeaf(rec), `
await agent("p", {provider: "v", profile: "slim", tools: ["Read", "Bash", "Read"].slice(0, 2), skills: false, mcp: true});
await agent("p2", {provider: "v", profile: "slim", mcp: false});
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	byPrompt := map[string]leafCall{}
	for _, c := range rec.snapshot() {
		byPrompt[c.prompt] = c
	}
	c := byPrompt["p"]
	if c.promptProfile != "slim" {
		t.Errorf("PromptProfile = %q, want slim (the REQUESTED profile)", c.promptProfile)
	}
	if strings.Join(c.tools, ",") != "Bash,Read" {
		t.Errorf("Tools = %v, want [Bash Read] (canonicalized dedupe+sort)", c.tools)
	}
	if !c.noSkills {
		t.Error("skills: false must set NoSkills=true")
	}
	if !c.mcp {
		t.Error("mcp: true must set MCP=true")
	}
	if byPrompt["p2"].mcp {
		t.Error("explicit mcp: false on slim must set MCP=false (beats the inherit default)")
	}
}

// TestAgentSlimDefaults: profile omitted is slim — skills on (NoSkills=false), mcp
// inheriting (the slim per-profile default), and the RESOLVED profile default tool set
// fed to the leaf, so the leaf execs exactly the set that was keyed (no nil-tools
// divergence). A bare slim-ro leaf stays strict-mcp; an explicit full carries no slim
// shape at all.
func TestAgentSlimDefaults(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "slimd", 2, echoLeaf(rec), `
await agent("a", {provider: "v"});
await agent("b", {provider: "v", profile: "slim-ro"});
await agent("c", {provider: "v", profile: "full"});
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	byPrompt := map[string]leafCall{}
	for _, c := range rec.snapshot() {
		byPrompt[c.prompt] = c
	}
	wantSlim, _ := subagent.CanonicalizeTools(subagent.DefaultSlimTools(subagent.ProfileSlim, false))
	if a := byPrompt["a"]; a.promptProfile != subagent.ProfileSlim || a.noSkills || !a.mcp ||
		strings.Join(a.tools, ",") != strings.Join(wantSlim, ",") {
		t.Errorf("bare leaf = %+v, want profile=slim NoSkills=false MCP=true Tools=%v", a, wantSlim)
	}
	wantRO, _ := subagent.CanonicalizeTools(subagent.DefaultSlimTools(subagent.ProfileSlimRO, false))
	if b := byPrompt["b"]; b.promptProfile != subagent.ProfileSlimRO || b.noSkills || b.mcp ||
		strings.Join(b.tools, ",") != strings.Join(wantRO, ",") {
		t.Errorf("bare slim-ro leaf = %+v, want NoSkills=false MCP=false Tools=%v", b, wantRO)
	}
	if c := byPrompt["c"]; c.promptProfile != subagent.ProfileFull || c.tools != nil || c.mcp {
		t.Errorf("full leaf = %+v, want profile=full nil Tools MCP=false", c)
	}
}

// TestAgentSlimValidationErrors: every front-loaded slim validation rejects with a
// thrown error (no leaf exec) — refinements with full (mcp by PRESENCE, either value),
// a bad profile, and a bad tool.
func TestAgentSlimValidationErrors(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"tools-with-full", `agent("p", {provider: "v", profile: "full", tools: ["Read"]})`, "slim-only"},
		{"skills-with-full", `agent("p", {provider: "v", profile: "full", skills: false})`, "slim-only"},
		{"mcp-with-full", `agent("p", {provider: "v", profile: "full", mcp: true})`, "slim-only"},
		{"mcp-false-with-full", `agent("p", {provider: "v", profile: "full", mcp: false})`, "slim-only"},
		{"bad-profile", `agent("p", {provider: "v", profile: "turbo"})`, "unknown prompt profile"},
		{"unknown-tool", `agent("p", {provider: "v", profile: "slim", tools: ["Nope"]})`, "unknown tool"},
		{"duplicate-tool", `agent("p", {provider: "v", profile: "slim", tools: ["Read", "Read"]})`, "duplicate tool"},
		{"skill-with-skills-off", `agent("p", {provider: "v", profile: "slim", tools: ["Read", "Skill"], skills: false})`, "contradictory with skills disabled"},
		{"bad-tools-type", `agent("p", {provider: "v", profile: "slim", tools: "Read"})`, "must be an array"},
		{"bad-profile-type", `agent("p", {provider: "v", profile: 7})`, "must be a string"},
		{"bad-mcp-type", `agent("p", {provider: "v", profile: "slim", mcp: 7})`, "must be a boolean"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			_, err := runScript(t, "slimv", 2, echoLeaf(rec), "return await "+tc.src+";")
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

	_, script := writeScript(t, `await agent("q", {provider: "v", profile: "slim"});`)
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

// TestSchemaAbsentStructuredOutputFails: an OK envelope WITHOUT the structured payload —
// even when the prose Result happens to be valid JSON (the max_turns-starved shape) —
// fails the schema leaf after exactly one exec; the prose is never a fallback.
func TestSchemaAbsentStructuredOutputFails(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: `{"answer": 5}`}
	})
	_, err := runScript(t, "slimso", 1, leaf,
		`return await agent("q", {provider: "v", schema: {required: ["answer"]}});`)
	if err == nil || !strings.Contains(err.Error(), "structured_output") {
		t.Fatalf("err = %v, want a no-structured_output failure", err)
	}
	if n := len(rec.snapshot()); n != 1 {
		t.Errorf("leaf ran %d times, want exactly 1", n)
	}
}

// TestSchemaJournalsStructuredPayload: a schema leaf journals the STRUCTURED payload, not
// the prose Result — what a resume replays is the validated object. Exercised through the
// Execute path so the on-disk journal is wired.
func TestSchemaJournalsStructuredPayload(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: "prose words", StructuredOutput: json.RawMessage(`{"answer":7}`)}
	})
	t.Cleanup(func() { runLeaf = old })

	_, script := writeScript(t, `await agent("q", {provider: "v", schema: {required: ["answer"]}});`)
	run, err := Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	jp, _ := subagent.RunJournalPath(run.RunID)
	data, err := os.ReadFile(jp)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	line := string(data)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	var e journalEntry
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("parse journal line: %v", err)
	}
	if !strings.Contains(e.Result, `{"answer":7}`) || strings.Contains(e.Result, "prose words") {
		t.Errorf("journaled result = %q, want the structured payload, not the prose Result", e.Result)
	}
}
