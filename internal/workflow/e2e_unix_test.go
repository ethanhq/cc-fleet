//go:build !windows

package workflow

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.starlark.net/starlark"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// e2eEnv is the fully-wired real-leaf sandbox: an isolated HOME/XDG config dir, a fake
// `claude` binary reachable through the fingerprint cache, and a `[fake]` vendor in
// vendors.toml. The engine drives the REAL subagent.Run (runLeaf is NOT overridden) so
// every leaf shells out to the fake claude over stdin, exactly as production would shell
// out to a vendor. Everything here keys off the PROMPT, never a clock/PID/random, so the
// whole suite is deterministic under -race.
type e2eEnv struct {
	home      string
	cfgDir    string
	promptLog string // every prompt the fake claude received, one per line (with a record sep)
	fakePath  string
}

// fakeClaudeScript is the deterministic fake `claude`. It (a) appends the stdin prompt to
// an invocation log framed by a record separator so a test can count/identify execs, and
// (b) prints a `claude --output-format json` success envelope whose `result` is a pure
// function of the prompt:
//
//   - a prompt carrying the schema JSON instruction ("Return ONLY a single JSON value")
//     gets a JSON object that conforms to the comprehensive script's NESTED schema;
//   - any other prompt gets a deterministic echo string ("LEAF:" + a stable digest line).
//
// total_cost_usd is a small fixed value so budget accounting + board metrics have data.
// It is a POSIX sh script (Unix-only, like the existing integration harness).
const fakeClaudeScript = `#!/bin/sh
prompt=$(cat)
{ printf '%s' "$prompt"; printf '\037\n'; } >> "$PROMPT_LOG"
case "$prompt" in
  *"Return ONLY a single JSON value"*)
    result='{"summary":"ok","items":[{"name":"a","score":1},{"name":"b","score":2}],"meta":{"count":2}}'
    ;;
  *)
    # Deterministic echo: the first line of the prompt, prefixed. No clock/PID/random.
    first=$(printf '%s' "$prompt" | head -n1)
    result="LEAF:$first"
    ;;
esac
# Escape the result for embedding in the JSON envelope (backslash + double-quote).
esc=$(printf '%s' "$result" | sed 's/\\/\\\\/g; s/"/\\"/g')
printf '{"type":"result","subtype":"success","is_error":false,"result":"%s","num_turns":1,"total_cost_usd":0.0025,"usage":{"input_tokens":12,"output_tokens":8}}' "$esc"
`

// newE2EEnv builds the sandbox and points the runtime at the fake claude. It mirrors
// integration_unix_test.go's wiring exactly (fingerprint.json + vendors.toml), adding the
// PROMPT_LOG env the fake appends to. The returned env's promptLog is read back by tests.
func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))

	cfgDir, err := config.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}

	promptLog := filepath.Join(home, "prompts.log")
	t.Setenv("PROMPT_LOG", promptLog)

	fakeClaude := filepath.Join(home, "claude")
	if err := os.WriteFile(fakeClaude, []byte(fakeClaudeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fpJSON := `{"cc_version":"2.1.150","binary_path":"` + fakeClaude + `","env":{},"flags_template":[]}`
	if err := os.WriteFile(filepath.Join(cfgDir, "fingerprint.json"), []byte(fpJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	vendors := "version = 1\n\n[fake]\n" +
		"base_url = \"https://example.invalid/anthropic\"\n" +
		"default_model = \"fake-model\"\n" +
		"models_endpoint = \"https://example.invalid/v1/models\"\n" +
		"secret_backend = \"file\"\n" +
		"secret_ref = \"fake.key\"\n" +
		"enabled = true\n" +
		"added_at = 2026-01-01T00:00:00Z\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "vendors.toml"), []byte(vendors), 0o600); err != nil {
		t.Fatal(err)
	}
	return &e2eEnv{home: home, cfgDir: cfgDir, promptLog: promptLog, fakePath: fakeClaude}
}

// execCount returns how many times the fake claude actually executed (one record per exec).
// A missing log means zero execs (every leaf was served from the journal).
func (e *e2eEnv) execCount(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(e.promptLog)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read prompt log: %v", err)
	}
	return strings.Count(string(data), "\x1f")
}

// execPrompts returns every prompt the fake claude received, in order.
func (e *e2eEnv) execPrompts(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(e.promptLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read prompt log: %v", err)
	}
	parts := strings.Split(string(data), "\x1f\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// comprehensiveScript exercises every runtime feature in ONE run: phase, parallel fan-out,
// a no-barrier pipeline, a bounded for…break loop, a NESTED-schema leaf, budget accounting,
// a nested workflow(child.star), a background leaf + wait(), an isolation="worktree" leaf,
// and log(). Its result globals flow back so the test can assert each path end to end.
const comprehensiveScript = `meta = {"name": "e2e-full", "description": "every feature, one real-leaf run", "phases": [{"title": "map"}, {"title": "reduce"}]}

log("starting e2e full run")
phase("map")

# parallel fan-out: two leaves run concurrently; results collected in order.
fan = parallel([
    lambda: agent("alpha", vendor="fake", label="a"),
    lambda: agent("beta", vendor="fake", label="b"),
])

# no-barrier pipeline: one item through one stage.
chain = pipeline(["gamma"], lambda prev, item, i: agent("stage:" + item, vendor="fake", label="p"))

# bounded for...break loop (no while): run a leaf per item, stop early at the 2nd.
loop = []
for i in ["one", "two", "three"]:
    loop.append(agent("loop:" + i, vendor="fake"))
    if len(loop) >= 2:
        break

# schema= leaf with a NESTED schema; the fake returns a conforming object.
shaped = agent("produce a report", vendor="fake", label="schema", schema={
    "type": "object",
    "required": ["summary", "items", "meta"],
    "properties": {
        "summary": {"type": "string"},
        "items": {"type": "array", "items": {
            "type": "object",
            "required": ["name", "score"],
            "properties": {"name": {"type": "string"}, "score": {"type": "integer"}},
        }},
        "meta": {"type": "object", "required": ["count"], "properties": {"count": {"type": "integer"}}},
    },
})

phase("reduce")

# nested workflow whose child sets a result global.
child = workflow("CHILD_PATH", args={"topic": "delta"})

# background leaf + wait().
bg = agent("background-epsilon", vendor="fake", label="bg", run_in_background=True)
bg_result = wait(bg)

# worktree-isolated leaf (cwd must be a git repo).
wt = agent("isolated-zeta", vendor="fake", label="wt", isolation="worktree")

spent = budget.spent()

# Flatten results into assertable globals.
fan_ok = len([r for r in fan if r == "LEAF:alpha" or r == "LEAF:beta"])
chained = chain[0]
loop_n = len(loop)
schema_summary = shaped["summary"]
schema_count = shaped["meta"]["count"]
schema_first = shaped["items"][0]["name"]
`

// childScript is the nested workflow target: it runs one leaf and sets `result`.
const childScript = `meta = {"name": "child", "description": "nested child run"}
topic = args["topic"]
r = agent("child-task:" + topic, vendor="fake", label="child")
result = r
`

// writeComprehensive writes the comprehensive + child scripts into dir and returns the
// comprehensive path (with CHILD_PATH substituted to the absolute child path).
func writeComprehensive(t *testing.T, dir string) string {
	t.Helper()
	childPath := filepath.Join(dir, "child.star")
	if err := os.WriteFile(childPath, []byte(childScript), 0o600); err != nil {
		t.Fatal(err)
	}
	main := strings.Replace(comprehensiveScript, "CHILD_PATH", childPath, 1)
	mainPath := filepath.Join(dir, "full.star")
	if err := os.WriteFile(mainPath, []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	return mainPath
}

// gitInitRepo makes a temp git repo with one committed file and returns its path. The
// comprehensive run's cwd must be this repo so the worktree-isolated leaf can branch HEAD.
func gitInitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
	} {
		if out, err := runGit(repo, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runGit(repo, "add", "."); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	if out, err := runGit(repo, "commit", "-qm", "init"); err != nil {
		t.Fatalf("git commit: %v %s", err, out)
	}
	return repo
}

// runComprehensiveGlobals executes the comprehensive script and returns BOTH the module
// globals (via eng.run, so they are assertable) AND drives the full Execute on-disk wiring
// (journal/events/manifest). It does this in one engine instance to avoid double-executing
// leaves: it constructs the engine like Execute does, runs the body, then finalizes the
// manifest — i.e. it is Execute, inlined, returning the globals.
func runComprehensiveGlobals(t *testing.T, env *e2eEnv, runID string, opts Options) (starlark.StringDict, string) {
	t.Helper()
	repo := gitInitRepo(t)
	mainPath := writeComprehensive(t, repo)
	t.Chdir(repo)

	run, err := Prepare(mainPath)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	run.RunID = runID
	if err := subagent.SaveRun(run); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	src, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := extractMeta(fileOptions, mainPath, src)
	if err != nil {
		t.Fatal(err)
	}
	phases := make([]subagent.RunPhase, 0, len(meta.Phases))
	for _, p := range meta.Phases {
		phases = append(phases, subagent.RunPhase{Title: p.Title, Detail: p.Detail})
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	eng := &engine{
		sched: newScheduler(context.Background(), opts.Concurrency), runID: runID,
		name: meta.Name, description: meta.Description, startedAt: run.StartedAt, phases: phases,
		persistIO:   !opts.NoPersistIO,
		metaModel:   meta.Model,
		whenToUse:   meta.WhenToUse,
		budgetTotal: opts.BudgetUSD,
	}
	if jp, jerr := subagent.RunJournalPath(runID); jerr == nil {
		eng.journal = loadJournal(jp)
	}
	if ep, eerr := subagent.RunEventsPath(runID); eerr == nil {
		_ = os.Remove(ep)
		eng.events = newEventWriter(ep)
	}
	eng.saveManifest("running", "")
	g, rerr := eng.run(mainPath, src, opts)
	status, errText := "done", ""
	if rerr != nil {
		status, errText = "failed", rerr.Error()
	}
	eng.saveManifest(status, errText)
	if rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	return g, mainPath
}

// --- R4.1 tests -------------------------------------------------------------------------

// TestE2EFullRun is the comprehensive single-run gate: it drives the REAL engine + REAL
// subagent.Run against the fake claude through EVERY feature, then asserts the result
// globals, the manifest, the events file (incl. key-safety), the journal, the io files,
// and the board metrics.
//
// Feature coverage map (script → assertion):
//   - phase("map")/phase("reduce")     → manifest.Phases == [map, reduce]
//   - parallel fan-out                 → fan_ok == 2 (both leaves flowed back)
//   - pipeline (no-barrier)            → chained == "LEAF:gamma"
//   - bounded for...break loop         → loop_n == 2
//   - schema= leaf, NESTED schema      → schema_summary/_count/_first (validated object)
//   - budget                           → spent > 0 (sum of leaf CostUSD)
//   - nested workflow(child, args=)    → child == "LEAF:child-task:delta"
//   - background agent + wait()        → bg_result == "LEAF:background-epsilon"
//   - isolation="worktree" leaf        → wt == "LEAF:isolated-zeta"
//   - log()                            → events file has a "log" record
//   - persist-io (default on)          → <jobID>.prompt/.answer exist; answer NOT in manifest/events
//   - board metrics                    → tagged Results carry Usage + CostUSD
func TestE2EFullRun(t *testing.T) {
	env := newE2EEnv(t)
	const runID = "e2e-full"
	g, _ := runComprehensiveGlobals(t, env, runID, Options{BudgetUSD: 100})

	// --- result globals flow back -------------------------------------------------------
	if n, _ := starlark.AsInt32(g["fan_ok"]); n != 2 {
		t.Errorf("fan_ok = %v, want 2 (both parallel leaves returned the fake result)", g["fan_ok"])
	}
	if s, _ := starlark.AsString(g["chained"]); s != "LEAF:stage:gamma" {
		t.Errorf("chained = %q, want LEAF:stage:gamma (pipeline result)", s)
	}
	if n, _ := starlark.AsInt32(g["loop_n"]); n != 2 {
		t.Errorf("loop_n = %v, want 2 (for...break stopped at 2)", g["loop_n"])
	}
	if s, _ := starlark.AsString(g["schema_summary"]); s != "ok" {
		t.Errorf("schema_summary = %q, want ok (validated nested-schema object)", s)
	}
	if n, _ := starlark.AsInt32(g["schema_count"]); n != 2 {
		t.Errorf("schema_count = %v, want 2 (nested meta.count)", g["schema_count"])
	}
	if s, _ := starlark.AsString(g["schema_first"]); s != "a" {
		t.Errorf("schema_first = %q, want a (nested items[0].name)", s)
	}
	if s, _ := starlark.AsString(g["child"]); s != "LEAF:child-task:delta" {
		t.Errorf("child = %q, want LEAF:child-task:delta (nested workflow result via args)", s)
	}
	if s, _ := starlark.AsString(g["bg_result"]); s != "LEAF:background-epsilon" {
		t.Errorf("bg_result = %q, want LEAF:background-epsilon (background leaf via wait())", s)
	}
	if s, _ := starlark.AsString(g["wt"]); s != "LEAF:isolated-zeta" {
		t.Errorf("wt = %q, want LEAF:isolated-zeta (worktree-isolated leaf)", s)
	}
	if f, _ := starlark.AsFloat(g["spent"]); f <= 0 {
		t.Errorf("budget spent = %v, want > 0 (fake's total_cost_usd accumulated)", g["spent"])
	}

	// --- manifest ends done with the phases --------------------------------------------
	run, jobs, err := subagent.RunStatus(runID)
	if err != nil {
		t.Fatalf("run status: %v", err)
	}
	if run.Status != "done" {
		t.Errorf("manifest status = %q, want done", run.Status)
	}
	gotPhases := []string{}
	for _, p := range run.Phases {
		gotPhases = append(gotPhases, p.Title)
	}
	if strings.Join(gotPhases, ",") != "map,reduce" {
		t.Errorf("manifest phases = %v, want [map reduce]", gotPhases)
	}

	// --- events: phase/leaf(launch+done)/group-open+close, and NEVER any answer text ----
	evPath, _ := subagent.RunEventsPath(runID)
	evData, err := os.ReadFile(evPath)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	kinds := map[string]int{}
	leafStatuses := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(evData)), "\n") {
		if line == "" {
			continue
		}
		var rec EventRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			t.Fatalf("malformed event line: %q", line)
		}
		kinds[rec.Kind]++
		if rec.Kind == "leaf" {
			leafStatuses[rec.Status]++
		}
	}
	for _, k := range []string{"phase", "leaf", "group-open", "group-close", "log"} {
		if kinds[k] == 0 {
			t.Errorf("events missing a %q record (kinds=%v)", k, kinds)
		}
	}
	if leafStatuses["launch"] == 0 || leafStatuses["done"] == 0 {
		t.Errorf("events leaf statuses = %v, want both launch and done present", leafStatuses)
	}
	// Key-safety: no leaf answer text may appear in the events stream.
	for _, answer := range []string{"LEAF:alpha", "LEAF:isolated-zeta", "background-epsilon", "child-task:delta", `"summary":"ok"`} {
		if strings.Contains(string(evData), answer) {
			t.Errorf("events file leaked answer/io text %q", answer)
		}
	}

	// --- journal: an entry per successful leaf, raw answers present ----------------------
	jpPath, _ := subagent.RunJournalPath(runID)
	jpData, err := os.ReadFile(jpPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	jLines := 0
	for _, line := range strings.Split(strings.TrimSpace(string(jpData)), "\n") {
		if line == "" {
			continue
		}
		var e journalEntry
		if json.Unmarshal([]byte(line), &e) != nil || e.Key == "" {
			t.Fatalf("malformed journal line: %q", line)
		}
		jLines++
	}
	// 2 fan + 1 pipeline + 2 loop + 1 schema + 1 child + 1 bg + 1 worktree = 9 leaves.
	if jLines != 9 {
		t.Errorf("journal has %d entries, want 9 (one per successful leaf)", jLines)
	}

	// --- io files: prompt+answer persisted per leaf; answer NOT in manifest/events ------
	answerFiles, _ := filepath.Glob(filepath.Join(env.cfgDir, "subagent-jobs", "*.answer"))
	promptFiles, _ := filepath.Glob(filepath.Join(env.cfgDir, "subagent-jobs", "*.prompt"))
	if len(answerFiles) == 0 {
		t.Error("no <jobID>.answer io files (persist-io is default-on)")
	}
	if len(promptFiles) == 0 {
		t.Error("no <jobID>.prompt io files (persist-io is default-on)")
	}
	foundAnswer := false
	for _, f := range answerFiles {
		b, _ := os.ReadFile(f)
		if strings.HasPrefix(string(b), "LEAF:") || strings.Contains(string(b), `"summary"`) {
			foundAnswer = true
		}
	}
	if !foundAnswer {
		t.Error("no .answer file held a real leaf answer")
	}
	// The manifest must NOT carry any answer text.
	manData, _ := os.ReadFile(filepath.Join(env.cfgDir, "subagent-jobs", "runs", runID+".json"))
	if strings.Contains(string(manData), "LEAF:") {
		t.Error("manifest leaked a leaf answer")
	}

	// --- board metrics: tagged Results carry Usage + CostUSD ----------------------------
	if len(jobs) == 0 {
		t.Fatal("run has no tagged jobs")
	}
	gotUsage, gotCost := false, false
	for _, j := range jobs {
		if j.Usage != nil && (j.Usage.InputTokens > 0 || j.Usage.OutputTokens > 0) {
			gotUsage = true
		}
		if j.CostUSD > 0 {
			gotCost = true
		}
	}
	if !gotUsage {
		t.Error("no tagged job Result carried Usage tokens")
	}
	if !gotCost {
		t.Error("no tagged job Result carried CostUSD")
	}
}

// TestE2EResumeCachedReplay is the keystone: after a full run, the SAME script under the
// SAME runID is re-run (resume) and the fake claude's exec log does NOT grow for the
// unchanged leaves (served from the journal) AND the results are byte-identical. Then ONE
// leaf's prompt is edited and a third resume re-runs ONLY that leaf.
func TestE2EResumeCachedReplay(t *testing.T) {
	env := newE2EEnv(t)
	const runID = "e2e-resume"

	g1, mainPath := runComprehensiveGlobals(t, env, runID, Options{BudgetUSD: 100})
	first := env.execCount(t)
	if first == 0 {
		t.Fatal("first run executed zero leaves")
	}

	// Resume: same script, same id → 100% cache hits, no new execs.
	g2 := resumeAt(t, env, runID, mainPath, Options{BudgetUSD: 100})
	if after := env.execCount(t); after != first {
		t.Errorf("resume grew the exec log: %d → %d (want no new execs — all journaled)", first, after)
	}
	assertSameGlobals(t, g1, g2, "chained", "schema_summary", "child", "bg_result", "wt")

	// Edit ONE leaf's prompt (the pipeline stage item) and resume: only that leaf re-runs.
	edited := strings.Replace(string(mustRead(t, mainPath)), `["gamma"]`, `["gamma2"]`, 1)
	if err := os.WriteFile(mainPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	before := env.execCount(t)
	g3 := resumeAt(t, env, runID, mainPath, Options{BudgetUSD: 100})
	delta := env.execCount(t) - before
	if delta != 1 {
		t.Errorf("editing one leaf re-ran %d leaves, want exactly 1", delta)
	}
	if s, _ := starlark.AsString(g3["chained"]); s != "LEAF:stage:gamma2" {
		t.Errorf("edited pipeline result = %q, want LEAF:stage:gamma2", s)
	}
	// The unedited globals are still served identically from the journal.
	assertSameGlobals(t, g1, g3, "schema_summary", "child", "wt")
}

// TestE2ECrashRecovery simulates a crash: a PARTIAL run (a few leaves journaled, the rest
// not) then a resume that runs ONLY the un-journaled leaves. The partial state is produced
// by cancelling the engine ctx after the first phase's leaves complete is racy; instead we
// pre-seed the journal with the exact keys of a subset of leaves (the crash-survivor set)
// and assert the resume executes only the remainder.
func TestE2ECrashRecovery(t *testing.T) {
	env := newE2EEnv(t)
	const runID = "e2e-crash"
	repo := gitInitRepo(t)
	mainPath := writeComprehensive(t, repo)
	t.Chdir(repo)

	run, err := Prepare(mainPath)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	run.RunID = runID
	if err := subagent.SaveRun(run); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	// Pre-seed the journal as if the two parallel fan-out leaves finished before the crash.
	// Their keys are the engine's exact content keys (vendor fake, no model, base prompt).
	jp, _ := subagent.RunJournalPath(runID)
	j := loadJournal(jp)
	j.append(journalKey("fake", "", "alpha", "", ""), "LEAF:alpha")
	j.append(journalKey("fake", "", "beta", "", ""), "LEAF:beta")

	if err := Execute(context.Background(), mainPath, runID, Options{BudgetUSD: 100}); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// alpha + beta were served from the seeded journal; every OTHER leaf executed.
	for _, p := range env.execPrompts(t) {
		if p == "alpha" || p == "beta" {
			t.Errorf("a pre-journaled leaf re-executed: %q", p)
		}
	}
	// 9 total leaves − 2 pre-seeded = 7 real execs on this resume.
	if n := env.execCount(t); n != 7 {
		t.Errorf("crash resume executed %d leaves, want 7 (9 total − 2 journaled)", n)
	}
	run2, _, err := subagent.RunStatus(runID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if run2.Status != "done" {
		t.Errorf("resumed run status = %q, want done", run2.Status)
	}
}

// TestE2EStop covers `workflow stop` on a foreground/EnginePID=0 manifest: it flips the run
// to stopped without killing an unrelated process (the reuse guard refuses an unverifiable
// pid). It reuses a finished run's manifest — StopRun leaves a terminal run untouched — so
// the test first forces the manifest back to "running" with EnginePID 0 to model a
// foreground run that lost its terminal, then asserts the flip to stopped.
func TestE2EStop(t *testing.T) {
	env := newE2EEnv(t)
	const runID = "e2e-stop"
	_, _ = runComprehensiveGlobals(t, env, runID, Options{BudgetUSD: 100})

	// Model a still-"running" foreground run (EnginePID deliberately 0 → nothing to reap).
	run, err := subagent.ReadRun(runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	run.Status = "running"
	run.EnginePID = 0
	if err := subagent.SaveRun(run); err != nil {
		t.Fatalf("save run: %v", err)
	}

	stopped, err := subagent.StopRun(runID)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.Status != "stopped" {
		t.Errorf("stopped run status = %q, want stopped", stopped.Status)
	}
}

// --- shared resume helpers --------------------------------------------------------------

// resumeAt re-executes mainPath under runID via the same Execute-equivalent engine wiring,
// sharing the on-disk journal (so journaled leaves replay), and returns the module globals.
func resumeAt(t *testing.T, env *e2eEnv, runID, mainPath string, opts Options) starlark.StringDict {
	t.Helper()
	src, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := extractMeta(fileOptions, mainPath, src)
	if err != nil {
		t.Fatal(err)
	}
	phases := make([]subagent.RunPhase, 0, len(meta.Phases))
	for _, p := range meta.Phases {
		phases = append(phases, subagent.RunPhase{Title: p.Title, Detail: p.Detail})
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	prepared, _ := subagent.ReadRun(runID)
	eng := &engine{
		sched: newScheduler(context.Background(), opts.Concurrency), runID: runID,
		name: meta.Name, description: meta.Description, startedAt: prepared.StartedAt, phases: phases,
		persistIO: !opts.NoPersistIO, metaModel: meta.Model, whenToUse: meta.WhenToUse,
		budgetTotal: opts.BudgetUSD,
	}
	if jp, jerr := subagent.RunJournalPath(runID); jerr == nil {
		eng.journal = loadJournal(jp)
	}
	if ep, eerr := subagent.RunEventsPath(runID); eerr == nil {
		_ = os.Remove(ep)
		eng.events = newEventWriter(ep)
	}
	eng.saveManifest("running", "")
	g, rerr := eng.run(mainPath, src, opts)
	status, errText := "done", ""
	if rerr != nil {
		status, errText = "failed", rerr.Error()
	}
	eng.saveManifest(status, errText)
	if rerr != nil {
		t.Fatalf("resume run: %v", rerr)
	}
	return g
}

func assertSameGlobals(t *testing.T, a, b starlark.StringDict, names ...string) {
	t.Helper()
	for _, n := range names {
		sa, _ := starlark.AsString(a[n])
		sb, _ := starlark.AsString(b[n])
		if sa != sb {
			t.Errorf("global %q changed across resume: %q vs %q", n, sa, sb)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
