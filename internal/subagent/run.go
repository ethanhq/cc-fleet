package subagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/pinned"
)

// runsDirName holds run manifests under the jobs dir: ConfigDir/subagent-jobs/runs.
// A manifest <runId>.json is the canonical phase sequencer for a workflow run; the
// member jobs that belong to it carry the same RunID in their own meta. Nesting
// runs/ UNDER the jobs dir keeps GC/PurgeJobs/ListJobs unchanged — they skip
// subdirectories in their readdir filter, so a runs/ entry is already ignored.
const runsDirName = "runs"

// RunPhase is one planned step in a run. Title is the short name a worker passes
// as --phase; Detail is optional free text describing the step.
type RunPhase struct {
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// WorkflowRun is the on-disk manifest for a workflow run, stored at
// ConfigDir/subagent-jobs/runs/<run_id>.json. It records the run's identity and
// its intended phase sequence; the actual subagent jobs are separate files tagged
// with this RunID, joined back in RunStatus.
type WorkflowRun struct {
	RunID       string `json:"run_id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	WhenToUse   string `json:"when_to_use,omitempty"` // meta.whenToUse — display/board text
	StartedAt   string `json:"started_at"`
	// UpdatedAt is a liveness heartbeat: the engine restamps it (RFC3339 UTC) on every
	// manifest write, and a resume restamps it at launch. Run-aware GC treats a run as
	// recent (and so protects its manifest + journal) by the LATER of StartedAt/UpdatedAt
	// — so a resumed run, whose StartedAt is its original (old) timestamp, is not pruned
	// out from under itself in the window before its first leaf registers a member.
	UpdatedAt string     `json:"updated_at,omitempty"`
	Phases    []RunPhase `json:"phases,omitempty"`
	Status    string     `json:"status,omitempty"`
	// EnginePID is the OS pid of the process running the engine (the detached child for a
	// normal run). `workflow stop` reaps its whole process tree — which includes the
	// engine's in-flight vendor-leaf children — after a cmdline reuse-guard check, so a
	// recycled pid can never make stop kill an unrelated process.
	EnginePID int `json:"engine_pid,omitempty"`
	// Error is the failure cause, set when Status is "failed" — so a DETACHED run
	// (whose stderr went to /dev/null) still records WHY it failed for `workflow
	// status`. It is a canonical/script-level message (agent() failures carry
	// subagent's canonical error_msg, never raw vendor body), so it is key-safe.
	Error string `json:"error,omitempty"`
	// SessionID is the parent Claude session this run was launched from (leadsession.Detect
	// at `workflow run`, or a --lead-session-id override) — so the board groups runs by
	// session like the teammates board. Empty when launched outside a Claude session.
	SessionID string `json:"session_id,omitempty"`
	// Cwd is the directory `workflow run` was invoked from (the project dir); the board shows it on
	// the run header. Captured at mint in the foreground launcher and preserved across resume.
	Cwd string `json:"cwd,omitempty"`
	// Launch options replayed on restart (resume re-execs with the SAME inputs, else a leaf's
	// key — and thus its cache validity — would shift). Persisted at mint; the engine carries
	// them so every manifest overwrite preserves them. ArgsJSON is the script's `args` input.
	ArgsJSON    string  `json:"args_json,omitempty"`
	NoPersistIO bool    `json:"no_persist_io,omitempty"`
	BudgetUSD   float64 `json:"budget_usd,omitempty"`
	// BudgetTokens is the run-level token cap (Usage.InputTokens+OutputTokens summed across
	// leaves, cache-read excluded); 0 = uncapped. Replayed on resume like BudgetUSD.
	BudgetTokens int64 `json:"budget_tokens,omitempty"`
	// SpentUSD / SpentTokens are the run's LIVE cumulative spend (list-price USD estimate +
	// input+output tokens), restamped by the engine as each leaf charges — so `workflow status`
	// shows a run-level running total without summing per-leaf jobs (which omit in-flight leaves).
	// A resume counts only newly-run leaves; journaled replays are free. Not a leaf determinant.
	SpentUSD    float64 `json:"spent_usd,omitempty"`
	SpentTokens int64   `json:"spent_tokens,omitempty"`
}

// runsDir is ConfigDir/subagent-jobs/runs.
func runsDir() (string, error) {
	dir, err := jobsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, runsDirName), nil
}

// writeRunManifest persists a manifest to runs/<run_id>.json, creating the runs dir
// 0o700 and writing 0o600 via the atomic-write outlet. It is the single write path
// for both minting (NewRun*) and in-place updates (SetRunStatus / AppendRunPhase),
// so the on-disk shape can never diverge between the two.
func writeRunManifest(run WorkflowRun) error {
	// Validate the run id before it becomes a path component. SaveRun takes a
	// caller-supplied WorkflowRun (its id may originate from a `--run-id` flag), so a
	// "../" id must never escape the runs dir; NewRunWithMeta's uuid always passes.
	if err := ids.ValidateJobID(run.RunID); err != nil {
		return fmt.Errorf("subagent: invalid run id %q: %w", run.RunID, err)
	}
	dir, err := runsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("subagent: mkdir runs dir: %w", err)
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("subagent: marshal run: %w", err)
	}
	return fileutil.AtomicWrite(filepath.Join(dir, run.RunID+".json"), data, 0o600)
}

// NewRun mints a run manifest and persists it. RunID is a fresh uuid; StartedAt is
// RFC3339 UTC (lexically sortable for newest-first listing); Status starts
// "running".
func NewRun(name string, phases []RunPhase) (WorkflowRun, error) {
	return NewRunWithMeta(name, "", "", phases)
}

// NewRunWithMeta is NewRun plus a description + whenToUse — the workflow runtime mints
// from a script's `meta` literal (name + description + whenToUse + declared phases), so a
// detached run's `--json`/board read carries them before the engine child starts.
func NewRunWithMeta(name, description, whenToUse string, phases []RunPhase) (WorkflowRun, error) {
	run := WorkflowRun{
		RunID:       uuid.NewString(),
		Name:        name,
		Description: description,
		WhenToUse:   whenToUse,
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
		Phases:      phases,
		Status:      "running",
	}
	if err := writeRunManifest(run); err != nil {
		return WorkflowRun{}, err
	}
	return run, nil
}

// SaveRun writes a complete manifest, overwriting any prior file (atomic temp+rename).
// The workflow-run engine is the single authoritative writer of its manifest: it holds
// the run's identity + phase plan + status in memory and overwrites the whole file on
// every phase()/finalize, so there is NO read-modify-write to race, and a manifest a
// concurrent GC happened to drop is simply recreated on the next write (the run stays
// inspectable via `workflow status`).
func SaveRun(run WorkflowRun) error {
	return writeRunManifest(run)
}

// ValidateRunID reports whether id is a path-safe run-manifest component (the same
// check ReadRun/SaveRun apply). Exported so the workflow runtime can fail-fast on a
// bad `--run-id` before executing a script.
func ValidateRunID(id string) error { return ids.ValidateJobID(id) }

// runSidecarExts are the per-run sidecar files that live next to a manifest
// (runs/<id>.json) and belong to the same run: the content-hash journal, the
// live-event channel, and the saved script (for restart — .js, plus the pre-JS
// engine's .star so an old run still reaps whole). removeRun and the orphan sweep
// treat them as one unit with the manifest, so reaping a run reaps its whole on-disk
// footprint. (Per-LEAF io — prompt/answer — is leaf-scoped under subagent-jobs and
// reaped by removeJob, not here.)
var runSidecarExts = []string{".journal", ".events", ".ctl", ".js", ".star"}

// runSidecarPath returns runs/<id><ext>, validating the id first (it becomes a path
// component). Centralizes every per-run sidecar path so GC reaps them with the manifest.
func runSidecarPath(runID, ext string) (string, error) {
	if err := ids.ValidateJobID(runID); err != nil {
		return "", fmt.Errorf("subagent: invalid run id %q: %w", runID, err)
	}
	dir, err := runsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, runID+ext), nil
}

// RunJournalPath returns the content-hash journal path runs/<id>.journal. The workflow
// runtime owns the journal's I/O + format; this just centralizes the path so GC reaps it.
func RunJournalPath(runID string) (string, error) { return runSidecarPath(runID, ".journal") }

// RunEventsPath returns the live-event channel path runs/<id>.events — the one-way
// engine→watcher stream `cc-fleet workflow watch` tails for a flowing live log.
func RunEventsPath(runID string) (string, error) { return runSidecarPath(runID, ".events") }

// RunLockPath returns runs/<id>.lock — the per-run execution lock file. It is
// deliberately NOT in runSidecarExts: a flock locks an inode, so GC / removeRun must
// never unlink a possibly-held lock (unlink + recreate yields a new inode and
// silently breaks mutual exclusion). Leftover empty lock files are bounded by
// distinct-run-count and cleared by PurgeJobs' RemoveAll at an exclusive uninstall.
func RunLockPath(runID string) (string, error) { return runSidecarPath(runID, ".lock") }

// WithRunLock serializes a run's lifecycle decisions (restart / resume / stop /
// delete) across processes via a blocking exclusive flock on runs/<id>.lock — the
// workflow runtime's per-run execution lock. It is a STANDALONE flock scope,
// independent of the three config scopes (the run-lifecycle paths take none of
// vendors/team/server). It must wrap an entry point's whole decision and NEVER the
// engine's Execute: the detached engine runs lock-free, so there is no re-entrancy
// or deadlock when Restart composes stop + launch under one lock.
func WithRunLock(runID string, fn func() error) error {
	path, err := RunLockPath(runID)
	if err != nil {
		return err
	}
	return config.WithFlock(path, fn)
}

// RunCtlPath returns the control-plane path runs/<id>.ctl — the NDJSON command file a
// CLI/board writer appends leaf directives to and the live engine polls.
func RunCtlPath(runID string) (string, error) { return runSidecarPath(runID, ".ctl") }

// RunScriptPath returns the saved-script path runs/<id>.js — the run's source,
// persisted so a stopped run can be restarted (resumed).
func RunScriptPath(runID string) (string, error) { return runSidecarPath(runID, ".js") }

// LegacyRunScriptPath returns the pre-JS-engine saved-script path runs/<id>.star.
// Only ever read: its existence (with no .js beside it) marks a run recorded by the
// retired Starlark engine, which restart/save refuse with an explicit engine-changed
// error instead of a JS parse failure.
func LegacyRunScriptPath(runID string) (string, error) { return runSidecarPath(runID, ".star") }

// ReadRun loads a manifest by id. runID is validated first because it becomes a
// filesystem path component (guards against a "../" escape via the CLI/status path).
func ReadRun(runID string) (WorkflowRun, error) {
	if err := ids.ValidateJobID(runID); err != nil {
		return WorkflowRun{}, fmt.Errorf("subagent: invalid run id %q: %w", runID, err)
	}
	dir, err := runsDir()
	if err != nil {
		return WorkflowRun{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, runID+".json"))
	if err != nil {
		// Canonical, path-free "not found" so an unknown-run id doesn't leak the
		// config-dir layout into the CLI's JSON error envelope (a genuine I/O fault
		// keeps its context for debugging).
		if errors.Is(err, os.ErrNotExist) {
			return WorkflowRun{}, fmt.Errorf("run %q not found", runID)
		}
		return WorkflowRun{}, err
	}
	var run WorkflowRun
	if err := json.Unmarshal(data, &run); err != nil {
		return WorkflowRun{}, fmt.Errorf("subagent: parse run %q: %w", runID, err)
	}
	return run, nil
}

// ListRuns returns every run manifest, newest-first by StartedAt (RFC3339 is
// lexically sortable, so a string descending sort works). A missing runs dir means
// nothing has run yet → (nil, nil). Unparseable manifests are skipped.
func ListRuns() ([]WorkflowRun, error) {
	dir, err := runsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("subagent: read runs dir: %w", err)
	}
	var runs []WorkflowRun
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			continue
		}
		var run WorkflowRun
		if json.Unmarshal(data, &run) != nil {
			continue
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].StartedAt > runs[j].StartedAt
	})
	return runs, nil
}

// runIsRecent reports whether a run's last activity — the LATER of StartedAt and the
// UpdatedAt liveness heartbeat — is after cutoff. GC uses it to protect a manifest that
// has no surviving job member yet: a freshly-minted (still-empty) run, OR an actively
// resuming run whose StartedAt is its original (old) timestamp but whose UpdatedAt was
// just restamped. An empty/unparseable timestamp simply doesn't count toward recency.
func runIsRecent(run WorkflowRun, cutoff time.Time) bool {
	var latest time.Time
	for _, ts := range []string{run.StartedAt, run.UpdatedAt} {
		if ts == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, ts); err == nil && t.After(latest) {
			latest = t
		}
	}
	return latest.After(cutoff)
}

// removeRun deletes a manifest AND its per-run sidecars (journal, …) best-effort,
// so GC/PurgeJobs reap a run as one unit (used by manifest pruning).
func removeRun(dir, runID string) {
	_ = os.Remove(filepath.Join(dir, runID+".json"))
	for _, ext := range runSidecarExts {
		_ = os.Remove(filepath.Join(dir, runID+ext))
	}
}

// StopRun reaps an actively-running workflow run and marks its manifest stopped. When a
// reapable DETACHED engine is found it kills the engine's whole process TREE by ANCESTRY
// — the engine plus its in-flight vendor-leaf `claude` children and their grandchildren
// (each leaf is its OWN process group, so an ancestry walk, not a single group signal, is
// required on unix; reapEngineTree handles the platform split). A cmdline reuse guard
// means a recycled EnginePID can NEVER make this kill an unrelated process: the pid is
// reaped only when its argv still proves it is this run's detached `workflow run …
// --run-id <id>` engine. An already-terminal run is returned untouched (no clobbering a
// real done/failed). Every other case — a foreground run (EnginePID deliberately 0), an
// engine already gone (crashed), or a recycled pid whose argv no longer matches — is
// reaped of nothing and simply flipped to stopped, clearing a stale "running"; the reuse
// guard ensures such an unverifiable pid is never killed.
func StopRun(runID string) (WorkflowRun, error) {
	run, err := ReadRun(runID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if run.Status != "" && run.Status != "running" {
		return run, nil // already terminal — don't clobber done/failed/stopped
	}
	switch {
	case run.EnginePID > 0 && pidAlive(run.EnginePID) && engineCmdlineMatches(run.EnginePID, runID):
		// Verifiably this run's live detached engine: reap it + confirm dead before finalizing its leaves.
		reapEngineTree(run.EnginePID) // reaps the engine + its in-flight leaf children
		if !WaitEngineStopped(runID, stopReapTimeout) {
			// Won't die — do NOT flip stopped or clear the pid, or a caller (Restart / PurgeRun) would
			// falsely read it as dead and act under a still-live engine. Fail closed.
			return WorkflowRun{}, fmt.Errorf("subagent: run %s engine did not stop in time", runID)
		}
		// Confirmed dead: finalize the engine's mid-flight leaves (left "running" because its
		// finalizeSyncJob defer died with it) — never racing that defer, since the engine is gone.
		finalizeRunLeaves(runID)
	case EngineAlive(run):
		// The engine MIGHT still be live but we cannot verify it is THIS run's (its argv is unreadable),
		// so we must neither kill it (fail SAFE) nor flip the run stopped — a caller would then rewrite
		// the journal / delete / launch under a possibly-live engine. Fail closed.
		return WorkflowRun{}, fmt.Errorf("subagent: run %s engine (pid %d) is alive but unverifiable; cannot stop safely", runID, run.EnginePID)
	case run.EnginePID > 0:
		// A recorded detached pid that is DEFINITIVELY gone (dead, or recycled to a different process):
		// its mid-flight leaves are ghosts — finalize them. (A foreground EnginePID==0 run has nothing to
		// reap and no ghost leaves here; it just flips stopped below, clearing a stale "running".)
		finalizeRunLeaves(runID)
	}
	run.Status = "stopped"
	run.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	run.EnginePID = 0 // a stopped run has no engine; clear the stale pid
	if serr := SaveRun(run); serr != nil {
		return WorkflowRun{}, serr
	}
	return run, nil
}

// stopReapTimeout bounds how long StopRun waits for a reaped engine to actually die before
// finalizing its leaves — long enough for a SIGTERM'd engine to exit, short enough to not wedge.
const stopReapTimeout = 5 * time.Second

// finalizeRunLeaves writes a terminal failure Result for every leaf of runID that a now-dead engine
// left without a result cache, so a stopped run shows no phantom "running" leaf. The caller invokes it
// ONLY after the engine is confirmed dead (StopRun's reap + WaitEngineStopped), so it never races the
// engine's own finalizeSyncJob defer; the result.json-absence gate skips any leaf that already
// finished. finalizeSyncJob writes the sanitized (answer/Raw-stripped) cache, so the synthetic
// canonical-string Result keeps no raw bytes or keys.
func finalizeRunLeaves(runID string) {
	dir, err := jobsDir()
	if err != nil {
		return
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		jobID := strings.TrimSuffix(name, ".json")
		if _, serr := os.Stat(filepath.Join(dir, jobID+".result.json")); serr == nil {
			continue // already finished — only the killed-mid-flight ghosts need finalizing
		}
		meta, merr := readMeta(dir, jobID)
		if merr != nil || meta.RunID != runID {
			continue
		}
		finalizeSyncJob(jobID, fail(ErrCodeStopped, "run stopped before the leaf finished", meta.Vendor, ""))
	}
}

// engineCmdlineMatches reports whether pid is THIS run's DETACHED workflow engine — its
// argv carries "workflow", "run", the "--run-id" flag, and the run id. Requiring the
// "--run-id" flag (which only the detached child carries — a user's `workflow run … [
// --resume <id>] --foreground` never has it) is what distinguishes the reapable detached
// engine from a foreground run that merely mentions the id, AND from a recycled pid. It
// is the reuse guard for StopRun. If the argv can't be read (a just-exited pid, or a
// platform without process introspection) it returns false — fail SAFE: never kill a pid
// we cannot positively identify.
func engineCmdlineMatches(pid int, runID string) bool {
	argv, ok := reuseGuardArgv(pid)
	if !ok {
		return false
	}
	return argvIsRunEngine(argv, runID)
}

// argvIsRunEngine reports whether argv is this run's detached `workflow run … --run-id <id>`
// engine — the argv-matching core shared by the StopRun kill-guard (engineCmdlineMatches) and the
// EngineAlive liveness check. Requiring the "--run-id" flag (which only the detached child carries)
// distinguishes the reapable detached engine from a foreground run that merely mentions the id and
// from a recycled pid.
func argvIsRunEngine(argv []string, runID string) bool {
	var hasWorkflow, hasRun, hasRunIDFlag, hasID bool
	for _, a := range argv {
		switch a {
		case "workflow":
			hasWorkflow = true
		case "run":
			hasRun = true
		case "--run-id":
			hasRunIDFlag = true
		case runID:
			hasID = true
		}
	}
	return hasWorkflow && hasRun && hasRunIDFlag && hasID
}

// EngineAlive reports whether run's DETACHED engine MIGHT still be running. It is a read-only
// LIVENESS check (it kills nothing), used by a watcher to stop waiting on a stale "running"
// manifest whose engine is gone. A foreground run (EnginePID 0) or a definitively-dead pid is
// "not alive". When the pid IS alive, the answer depends on whether we can read its argv: where
// we can (unix), require it to still be THIS run's engine so a RECYCLED pid (now an unrelated
// process) reads as gone — otherwise a SIGKILLed engine whose pid was reused would hold the
// watcher open forever; where we can't (a platform without process introspection, e.g. Windows),
// trust pidAlive alone, since a false "gone" for a live engine is worse than a rare missed
// recycled-pid detection. Unlike StopRun (which must fail-SAFE to never-kill an unverifiable
// pid), this fails-SOFT to keep-watching — neither can ever kill the wrong process.
func EngineAlive(run WorkflowRun) bool {
	if run.EnginePID <= 0 || !pidAlive(run.EnginePID) {
		return false
	}
	argv, ok := reuseGuardArgv(run.EnginePID)
	if !ok {
		return true // argv unavailable → can't disprove it's our engine; trust pidAlive
	}
	return argvIsRunEngine(argv, run.RunID)
}

// PurgeRun deletes a run entirely — the board's manual "delete" (the board never auto-clears, so runs
// accumulate). A VERIFIABLY-LIVE detached engine is stopped AND confirmed dead before any file is
// removed: the engine is recreate-safe (it rewrites a dropped manifest on its next save), so deleting
// under a live engine would not stick and could race its writes; if it can't be confirmed dead, the
// delete is aborted. A "running" manifest whose engine is GONE — crashed, or finished without
// finalizing — is exactly the accumulated junk this is for, so it is removed as-is. The id is
// validated before it becomes any path component.
//
// When no engine pid is recorded but the run is genuinely still writing — a --foreground
// run, or a detached run in the moment between mint and its child stamping EnginePID — reads as
// not-alive, so its files are removed and the recreate-safe engine harmlessly rewrites the manifest
// (it reappears on the next refresh; atomic writes keep it consistent — no corruption). Blocking that
// would also block deleting freshly-minted and crashed runs, which is the whole point.
func PurgeRun(runID string) error {
	if err := ids.ValidateJobID(runID); err != nil {
		return err
	}
	if run, rerr := ReadRun(runID); rerr == nil && EngineAlive(run) {
		if _, serr := StopRun(runID); serr != nil {
			return serr
		}
		if !WaitEngineStopped(runID, 5*time.Second) {
			return fmt.Errorf("subagent: run %s engine did not stop in time; delete aborted", runID)
		}
	}
	dir, err := jobsDir()
	if err != nil {
		return err
	}
	if entries, rerr := os.ReadDir(dir); rerr == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
				continue
			}
			jobID := strings.TrimSuffix(name, ".json")
			if meta, merr := readMeta(dir, jobID); merr == nil && meta.RunID == runID {
				removeJob(dir, jobID)
				_ = pinned.Unpin(pinned.Job, jobID) // explicit delete clears any leaf pin marker
			}
		}
	}
	removeRun(filepath.Join(dir, runsDirName), runID)
	_ = pinned.Unpin(pinned.Run, runID) // explicit delete is the sanctioned removal of a pinned run
	return nil
}

// PruneRuns deletes every run whose engine is no longer alive — crashed/killed runs still stuck
// "running", plus terminal ones — while sparing any run with a live engine. Each delete runs under the
// per-run execution lock so it can't race a concurrent restart/resume, and the sweep is best-effort:
// one run that won't delete (an unverifiable-live engine PurgeRun refuses) doesn't abort the rest.
// Returns the number of runs removed.
func PruneRuns() (int, error) {
	runs, err := ListRuns()
	if err != nil {
		return 0, err
	}
	pins, perr := pinned.Snapshot()
	if perr != nil {
		return 0, perr
	}
	// A run with any pinned member is spared too: PurgeRun removes ALL its member jobs, so
	// pruning it would delete the pinned leaf. Mirrors the run↔job coupling in GC / ClearFinished.
	// Fail closed if the job scan fails — pruning pin-blind could delete a pinned leaf.
	jobs, jerr := ListJobs()
	if jerr != nil {
		return 0, jerr
	}
	pinnedMemberRun := pinnedRunMembers(jobs, pins)
	removed := 0
	for _, r := range runs {
		id := r.RunID
		if pins.Has(pinned.Run, id) || pinnedMemberRun[id] {
			continue // user-pinned (or has a pinned member): only an explicit delete removes it
		}
		_ = WithRunLock(id, func() error {
			fresh, rerr := ReadRun(id)
			if rerr != nil || isLiveOrUnverifiable(fresh) {
				return nil // gone already, or possibly-live → spare it
			}
			if perr := PurgeRun(id); perr == nil {
				removed++
			}
			return nil
		})
	}
	return removed, nil
}

// isLiveOrUnverifiable reports whether a run might still be writing — a verifiably-live detached
// engine, OR a still-"running" run with no reapable pid (a --foreground run, or a detached run in the
// mint→stamp-pid window). PruneRuns spares both: deleting under either could yank files from a live
// engine. Mirrors the fail-closed liveness guard the resume/restart paths use. A terminal run, or a
// crashed detached run (recorded pid now dead), is not protected — exactly what prune reaps.
func isLiveOrUnverifiable(run WorkflowRun) bool {
	return EngineAlive(run) || (run.Status == "running" && run.EnginePID <= 0)
}

// WaitEngineStopped polls a run's engine liveness until it is gone or the deadline passes (true once
// EngineAlive is false / the manifest is unreadable, false on timeout). EngineAlive fails SOFT (an
// unreadable argv reads as alive), so a stuck poll times out into the caller's fail-closed path rather
// than touching files under a still-live engine. Shared by PurgeRun (delete) and workflow.Restart.
func WaitEngineStopped(runID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		run, err := ReadRun(runID)
		if err != nil || !EngineAlive(run) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// RunStatus returns a run's manifest plus the Results of the jobs tagged with it.
// A missing manifest is an error (unknown run). The jobs are ListJobs() filtered
// by RunID, already newest-first.
func RunStatus(runID string) (WorkflowRun, []Result, error) {
	run, err := ReadRun(runID)
	if err != nil {
		return WorkflowRun{}, nil, err
	}
	all, err := ListJobs()
	if err != nil {
		return run, nil, err
	}
	var jobs []Result
	for _, j := range all {
		if j.RunID == run.RunID {
			jobs = append(jobs, j)
		}
	}
	return run, jobs, nil
}
