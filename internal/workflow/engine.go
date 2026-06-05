// Package workflow is cc-fleet's deterministic orchestration runtime: it runs a
// Starlark script that fans out vendor subagent leaves, in a cc-fleet process OFF the
// main Claude context. The script's plan lives in Starlark variables (CPU, ~0 tokens);
// the model is invoked only at agent() leaves. It mirrors the native Claude Code
// Workflow API (meta / agent / parallel / pipeline / phase / log); the only shape
// differences are agent()'s vendor= parameter and Starlark syntax.
//
// Concurrency is a GIL (see sched.go): one mutex serializes ALL Starlark interpreter
// execution, released only around the blocking vendor exec, so the engine is -race
// clean while the slow leaves still overlap up to a bounded pool.
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// fileOptions are the resolver/compiler options for every workflow script:
// top-level for/if allowed (the body reads like the native JS body); `while` disabled
// (forces a bounded `for _ in range(N): … break`, complementing the lifetime cap);
// recursion disabled (default); `set` allowed; no global reassignment (globals freeze
// after load → safe concurrent reads from parallel/pipeline goroutines).
var fileOptions = &syntax.FileOptions{
	TopLevelControl: true,
	While:           false,
	GlobalReassign:  false,
	Recursion:       false,
	Set:             true,
}

// runLeaf is the vendor-subagent leaf — a seam so tests inject a deterministic fake
// in place of a real `claude -p` exec. Production = subagent.Run (in-process,
// key-safe via apiKeyHelper, board-registered, tagged with run/phase/label).
var runLeaf = subagent.Run

// Options configures a workflow run.
type Options struct {
	// RunID, when set, names an EXISTING manifest to execute (the detached child /
	// foreground re-exec path); empty means Prepare mints a fresh one.
	RunID       string
	Concurrency int    // 0 → defaultConcurrency()
	ArgsJSON    string // optional; predeclared to the script as `args`
	// Resume names an EXISTING run to re-execute against its journal (cross-invocation
	// replay): Launch reuses this id instead of minting a fresh one, and the engine's
	// unconditional journal load then serves the leaves that already completed. Empty =
	// a fresh run. Used only at Launch — the detached child carries the id via RunID and
	// resumes transparently (the journal load keys on the id, not this flag).
	Resume string
	// NoPersistIO opts OUT of board prompt/answer drill-in (persistence is DEFAULT ON).
	// When set, leaves persist no <jobID>.prompt/.answer side files; the result cache is
	// answer-stripped either way, so the board table is unaffected. Propagated to the
	// detached child so the whole run honors it.
	NoPersistIO bool
	// BudgetUSD caps total vendor spend (sum of leaf CostUSD); agent() raises once the cap
	// is reached. <=0 is uncapped. Propagated to the detached child.
	BudgetUSD float64
}

// Prepare parses a script, extracts + validates its `meta` literal, and mints a run
// manifest with the name/description/declared phases — BEFORE any execution, so a bad
// script never mints a half-run and the board shows the named, phase-skeletoned run
// immediately. Returns the new manifest (its RunID is handed to a detached child or
// printed to the caller).
func Prepare(scriptPath string) (subagent.WorkflowRun, error) {
	src, err := os.ReadFile(scriptPath)
	if err != nil {
		return subagent.WorkflowRun{}, fmt.Errorf("workflow: read script: %w", err)
	}
	meta, err := extractMeta(fileOptions, scriptPath, src)
	if err != nil {
		return subagent.WorkflowRun{}, err
	}
	return subagent.NewRunWithMeta(meta.Name, meta.Description, meta.WhenToUse, metaPhases(meta))
}

// metaPhases converts a parsed script meta's phase plan into the manifest's RunPhase
// slice. Both Prepare (minting the manifest) and Execute (re-deriving the engine's
// authoritative copy) feed it, so the on-disk phase shape stays identical between them.
func metaPhases(meta scriptMeta) []subagent.RunPhase {
	phases := make([]subagent.RunPhase, 0, len(meta.Phases))
	for _, p := range meta.Phases {
		phases = append(phases, subagent.RunPhase{Title: p.Title, Detail: p.Detail})
	}
	return phases
}

// Execute runs a prepared script's body to completion in the CURRENT process,
// tagging every leaf with runID, and flips the manifest to done/failed on exit. It
// NEVER lets a panic escape: a panic (anywhere on the top-level thread) is recovered
// into a failed status so a detached run always finalizes. (Goroutine panics inside
// parallel/pipeline are recovered at their own boundary in callOrNone.)
func Execute(ctx context.Context, scriptPath, runID string, opts Options) (err error) {
	// Fail-fast on a bad run id (e.g. a malformed `--run-id`) before doing anything —
	// it becomes a manifest path component, and a doomed-to-not-persist run shouldn't run.
	if verr := subagent.ValidateRunID(runID); verr != nil {
		return fmt.Errorf("workflow: invalid run id: %w", verr)
	}
	// Seed the engine's authoritative manifest state from the Prepare-minted manifest
	// (best-effort) + the script's meta. The engine then OWNS the manifest, overwriting
	// it whole on every phase()/finalize — so there is no read-modify-write to race and
	// a concurrently-dropped manifest is recreated on the next write.
	prepared, _ := subagent.ReadRun(runID) // the minted manifest, if still present
	startedAt := time.Now().UTC().Format(time.RFC3339)
	if prepared.StartedAt != "" {
		startedAt = prepared.StartedAt
	}
	// On a pre-execution failure, finalize the run as failed WITHOUT dropping the
	// prepared name/description/phases (only status + the failure cause change).
	failManifest := func(cause error) {
		prepared.RunID = runID
		if prepared.StartedAt == "" {
			prepared.StartedAt = startedAt
		}
		prepared.Status = "failed"
		prepared.Error = cause.Error()
		_ = subagent.SaveRun(prepared)
	}

	src, rerr := os.ReadFile(scriptPath)
	if rerr != nil {
		e := fmt.Errorf("workflow: read script: %w", rerr)
		failManifest(e)
		return e
	}
	meta, merr := extractMeta(fileOptions, scriptPath, src)
	if merr != nil {
		failManifest(merr)
		return merr
	}
	phases := metaPhases(meta)

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency()
	}
	eng := &engine{
		sched: newScheduler(ctx, concurrency), runID: runID,
		name: meta.Name, description: meta.Description, startedAt: startedAt, phases: phases,
		persistIO:   !opts.NoPersistIO, // board prompt/answer drill-in is default-on
		enginePID:   detachedEnginePID(opts),
		metaModel:   meta.Model, // default model for agents that omit model=
		whenToUse:   meta.WhenToUse,
		budgetTotal: opts.BudgetUSD,
	}
	// Load the run's content-hash journal (resume). The path is derived from the
	// already-validated runID; a missing file (a fresh run) yields an empty cache that
	// the first completed leaf creates. The load is unconditional, so a detached
	// --run-id child resumes transparently with no extra flag.
	//
	// Concurrency note: two simultaneous resumes of the SAME run id are not serialized
	// here — each loads the journal, runs un-cached leaves, and (atomically, O_APPEND
	// per line) appends; the worst case is duplicated leaf work, not a corrupt journal
	// or manifest. There is no per-run execution lock, so a caller must not launch
	// concurrent resumes of one id.
	if jp, jerr := subagent.RunJournalPath(runID); jerr == nil {
		eng.journal = loadJournal(jp)
	}
	// Open the run's live-event channel (board live log / DAG). Best-effort: a path
	// error leaves events nil → emits no-op. The file is TRUNCATED at the start of every
	// invocation (incl. a resume), so the live stream + DAG always represent exactly the
	// CURRENT invocation rather than a concatenation of per-invocation event streams (the
	// board's DAG would otherwise double on resume). Safe because events are observability
	// only — never read back by the engine, never journaled.
	if ep, eerr := subagent.RunEventsPath(runID); eerr == nil {
		_ = os.Remove(ep)
		eng.events = newEventWriter(ep)
	}
	// Stamp the manifest once up front: records EnginePID (so `workflow stop` can reap
	// this process), flips a resumed run to "running", and refreshes UpdatedAt from the
	// start (GC recency) — before the first leaf, closing the mint→first-leaf window.
	eng.saveManifest("running", "")

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("workflow: script panicked: %v", r)
		}
		status, errText := "done", ""
		if err != nil {
			status, errText = "failed", err.Error()
		}
		eng.saveManifest(status, errText)
	}()

	_, execErr := eng.run(scriptPath, src, opts)
	return execErr
}

// detachedEnginePID is os.Getpid() for the DETACHED engine child (opts.RunID set by the
// re-exec) and 0 for a foreground/inline run. `workflow stop` reaps only a recorded
// (detached) engine pid; a foreground run is stopped in its own terminal (Ctrl-C), so it
// records no pid rather than letting stop claim a kill it can't make.
func detachedEnginePID(opts Options) int {
	if opts.RunID != "" {
		return os.Getpid()
	}
	return 0
}

// run executes a script body under the GIL and returns its module globals. The top
// level holds the GIL; every builtin returns with the GIL held (runBlocking's
// defer-lock invariant), so this unlock is balanced on the normal path. On a panic
// the unlock is skipped, leaving the GIL locked — harmless, the run is ending and the
// caller's recover finalizes. Shared by Execute and the tests (which assert on the
// returned globals).
func (eng *engine) run(scriptPath string, src interface{}, opts Options) (starlark.StringDict, error) {
	predeclared := eng.builtins(opts)
	thread := eng.sched.newThread("workflow:" + eng.runID)
	eng.sched.lock()
	g, err := starlark.ExecFileOptions(fileOptions, thread, scriptPath, src, predeclared)
	eng.sched.unlock()
	return g, err
}

// Launch is the entry for `cc-fleet workflow run`. It prepares the run (parse + meta +
// mint), then either runs it inline (foreground — the debug / deterministic-e2e path)
// or re-execs cc-fleet as a DETACHED child that runs it to completion off the launching
// process, returning the run id immediately. Detaching reuses the subagent leaf's
// proven process-group primitive — no new platform split.
func Launch(ctx context.Context, scriptPath string, opts Options, foreground bool) (string, error) {
	abs, err := filepath.Abs(scriptPath)
	if err != nil {
		return "", fmt.Errorf("workflow: resolve script path: %w", err)
	}
	if opts.ArgsJSON != "" && !json.Valid([]byte(opts.ArgsJSON)) {
		return "", fmt.Errorf("workflow: --args-json is not valid JSON")
	}
	// Resume reuses an existing run id (validated + confirmed to exist) so the engine
	// replays its journal; a fresh run mints a new manifest from the script's meta.
	var run subagent.WorkflowRun
	if opts.Resume != "" {
		if verr := subagent.ValidateRunID(opts.Resume); verr != nil {
			return "", fmt.Errorf("workflow: invalid resume id: %w", verr)
		}
		existing, rerr := subagent.ReadRun(opts.Resume)
		if rerr != nil {
			return "", fmt.Errorf("workflow: cannot resume: %w", rerr)
		}
		run = existing
		// Restamp liveness + flip to running BEFORE detaching, so a concurrent GC can't
		// prune this (possibly old) run's manifest + journal in the window before the
		// resumed engine writes its first heartbeat (GC recency keys on the later of
		// StartedAt / UpdatedAt). A stamp failure means the manifest is unwritable — the
		// run can't be tracked or protected, so fail the resume rather than launch blind.
		run.Status = "running"
		run.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if serr := subagent.SaveRun(run); serr != nil {
			return "", fmt.Errorf("workflow: stamp resume liveness: %w", serr)
		}
	} else {
		minted, perr := Prepare(abs)
		if perr != nil {
			return "", perr
		}
		run = minted
	}
	// Persist the script as runs/<id>.star (the saved-script slice of native save-script)
	// so a stopped run can be restarted from the board (`workflow run --resume <id>`).
	// Best-effort: a write hiccup just means restart needs the original path.
	if data, rerr := os.ReadFile(abs); rerr == nil {
		if sp, serr := subagent.RunScriptPath(run.RunID); serr == nil {
			_ = os.WriteFile(sp, data, 0o600)
		}
	}
	if foreground {
		return run.RunID, Execute(ctx, abs, run.RunID, opts)
	}
	if lerr := launchDetached(abs, run.RunID, opts); lerr != nil {
		run.Status = "failed"
		_ = subagent.SaveRun(run)
		return "", lerr
	}
	return run.RunID, nil
}
