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

// resolveProfile maps a REQUESTED prompt profile to the effective one (the version
// gate), pre-keying. A seam so tests drive the downgrade path without a real claude
// binary. Production loads the fingerprint the same way Run does and resolves the
// version against THAT recipe's binary (process-cached per resolved path); a load
// failure fails open to full with a reason, matching the resolver's own discipline.
var resolveProfile = func(requested string) (string, string) {
	if requested == "" || requested == subagent.ProfileFull {
		return requested, ""
	}
	fp, err := subagent.LoadFingerprint()
	if err != nil {
		return subagent.ProfileFull, fmt.Sprintf("slim disabled: load fingerprint: %v", err)
	}
	return subagent.ResolveEffectiveProfile(requested, fp)
}

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
	// NoPersistIO opts OUT of the board's inline prompt/answer detail (persistence is DEFAULT ON).
	// When set, leaves persist no <jobID>.prompt/.answer side files; the result cache is
	// answer-stripped either way, so the board table is unaffected. Propagated to the
	// detached child so the whole run honors it.
	NoPersistIO bool
	// BudgetUSD caps total vendor spend (sum of leaf CostUSD); agent() raises once the cap
	// is reached. <=0 is uncapped. Propagated to the detached child.
	BudgetUSD float64
	// LeadSessionID is the parent Claude session this run was launched from (detected at
	// `workflow run`). Stored on the manifest at mint so the board groups runs by session.
	// Used only by Launch's fresh-mint path; a resume reads it back off the manifest.
	LeadSessionID string
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
	// Resolve the whole script (not just meta) BEFORE minting: a resolve error — e.g. a reassigned
	// top-level global — must fail with NO manifest left behind, not mint a 0-leaf run the board then
	// lists forever. SourceProgramOptions resolves without executing the body.
	if _, _, rerr := starlark.SourceProgramOptions(fileOptions, scriptPath, src, scriptPredeclared()); rerr != nil {
		return subagent.WorkflowRun{}, rerr
	}
	return subagent.NewRunWithMeta(meta.Name, meta.Description, meta.WhenToUse, metaPhases(meta))
}

// scriptPredeclared reports the names a workflow script may reference without defining them — the
// builtins() environment plus `args` (runtime-predeclared only with --args-json, but always allowed
// here so the Prepare resolve doesn't reject a script that legitimately uses it). Universe builtins
// (len/range/…) are resolved by the resolver itself.
func scriptPredeclared() func(string) bool {
	base := (&engine{}).builtins(Options{})
	return func(name string) bool { return name == "args" || base.Has(name) }
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
		persistIO:   !opts.NoPersistIO, // the board's inline prompt/answer detail is default-on
		enginePID:   detachedEnginePID(opts),
		metaModel:   meta.Model, // default model for agents that omit model=
		whenToUse:   meta.WhenToUse,
		budgetTotal: opts.BudgetUSD,
		sessionID:   prepared.SessionID, // from the minted manifest; re-persisted on every save
		cwd:         prepared.Cwd,       // launching project dir; ditto
		argsJSON:    opts.ArgsJSON,
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
	// Open the run's live-event channel that `workflow watch` renders. Best-effort: a path
	// error leaves events nil → emits no-op. The file is TRUNCATED at the start of every
	// invocation (incl. a resume), so the live stream always represents exactly the CURRENT
	// invocation rather than a concatenation of per-invocation event streams (it would otherwise
	// double on resume). Safe because events are observability only — never read back by the
	// engine, never journaled.
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
		// One engine per run: refuse to resume a run that still has a LIVE engine — a verifiably-live
		// detached one, or a foreground/unreapable run (EnginePID<=0) still claiming to run. The board
		// Restart path stops the old engine first (so the status is no longer running when it reaches
		// here); a crashed/killed detached run (recorded pid now dead) falls through and resumes as-is.
		// This guards the public `workflow run --resume` entry, which would otherwise launch a second
		// concurrent engine. (Mirrors restart.go's fail-closed liveness check.)
		if run.Status == "running" && (subagent.EngineAlive(run) || run.EnginePID <= 0) {
			return "", fmt.Errorf("workflow: run %s already has a live engine; stop it first", opts.Resume)
		}
		// Replay the run's original launch options on resume so a leaf's content key — and thus
		// its journal-cache validity — doesn't shift. A non-zero/non-empty opts value overrides
		// (e.g. resuming with a larger --budget-usd); a zero/empty one reads as "not set" and
		// inherits the manifest. The corollary — you can't override TO the zero value on resume
		// (uncap a capped run, blank the args) — is a faithful-continuation tradeoff, not a real
		// use case (the CLI can't express it and Restart wants the inherit).
		if opts.ArgsJSON == "" {
			opts.ArgsJSON = existing.ArgsJSON
		}
		if !opts.NoPersistIO {
			opts.NoPersistIO = existing.NoPersistIO
		}
		if opts.BudgetUSD == 0 {
			opts.BudgetUSD = existing.BudgetUSD
		}
		// Restamp liveness + flip to running BEFORE detaching, so a concurrent GC can't
		// prune this (possibly old) run's manifest + journal in the window before the
		// resumed engine writes its first heartbeat (GC recency keys on the later of
		// StartedAt / UpdatedAt). A stamp failure means the manifest is unwritable — the
		// run can't be tracked or protected, so fail the resume rather than launch blind.
		run.Status = "running"
		run.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		run.EnginePID = 0 // clear the prior engine's pid; the new child re-stamps, and WaitEngineStarted waits for exactly that
		if serr := subagent.SaveRun(run); serr != nil {
			return "", fmt.Errorf("workflow: stamp resume liveness: %w", serr)
		}
	} else {
		minted, perr := Prepare(abs)
		if perr != nil {
			return "", perr
		}
		run = minted
		// Stamp the session + replay options onto the manifest BEFORE the child reads it, so
		// the board can group by session and a later restart resumes with the same inputs.
		run.SessionID = opts.LeadSessionID
		run.ArgsJSON = opts.ArgsJSON
		run.NoPersistIO = opts.NoPersistIO
		run.BudgetUSD = opts.BudgetUSD
		if cwd, cerr := os.Getwd(); cerr == nil {
			run.Cwd = cwd
		}
		if serr := subagent.SaveRun(run); serr != nil {
			return "", fmt.Errorf("workflow: persist run options: %w", serr)
		}
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
	pid, reaper, lerr := launchDetached(abs, run.RunID, opts)
	if lerr != nil {
		run.Status = "failed"
		_ = subagent.SaveRun(run)
		return "", lerr
	}
	if !WaitEngineStarted(run.RunID, pid) {
		// The detached child did not self-stamp its pid within the startup budget (slow OR
		// dead). Kill + reap it BEFORE failing the run, so a slow-but-live child can't later
		// overwrite the failed manifest and run on as a second engine.
		reaper.kill()
		_ = reaper.wait()
		run.Status, run.Error, run.EnginePID = "failed", "workflow: engine did not register within the startup budget", 0
		_ = subagent.SaveRun(run)
		return "", fmt.Errorf("workflow: engine failed to start")
	}
	// Registered: reap the child asynchronously so it never lingers as a zombie under a
	// long-lived parent (the TUI board). A short-lived CLI launcher exits before this
	// goroutine completes; init then adopts and reaps the orphan.
	go func() { _ = reaper.wait() }()
	return run.RunID, nil
}

// engineStartupBudget bounds how long a launcher waits — under the per-run execution lock —
// for a freshly detached engine child to self-stamp its pid into the manifest. (A var so a test can
// shorten it; production never reassigns it.)
var engineStartupBudget = 10 * time.Second

// WaitEngineStarted blocks until the detached child has self-stamped EnginePID == childPID
// (its first manifest write in Execute's pre-run saveManifest) or the startup budget
// elapses. The launcher holds the per-run execution lock across this wait, so a serialized
// second restart/stop/resume always observes a fully-registered engine — never the
// pre-stamp EnginePID==0 window. Returns false on timeout; the caller must then kill+reap
// the child before failing the run.
func WaitEngineStarted(runID string, childPID int) bool {
	deadline := time.Now().Add(engineStartupBudget)
	for {
		if run, err := subagent.ReadRun(runID); err == nil && run.EnginePID == childPID {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
