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
	// engine's in-flight provider-leaf children — after an identity reuse-guard check
	// (argv where readable, else the start token below), so a recycled pid can never make
	// stop kill an unrelated process.
	EnginePID int `json:"engine_pid,omitempty"`
	// EngineProcStart is EnginePID's kernel start-time token
	// (procintrospect.ProcStart), self-stamped by the engine next to EnginePID.
	// It is the engine-identity check where argv is unreadable (Windows): a
	// recycled pid carries a new start time, so equality proves this is still
	// our engine. Empty (a pre-token manifest / capture failure) degrades to the
	// argv-only behavior.
	EngineProcStart string `json:"engine_proc_start,omitempty"`
	// FgEnginePID / FgEngineProcStart are a FOREGROUND engine's own pid + kernel start token, stamped
	// by an inline `workflow run --foreground` engine as liveness EVIDENCE only — deliberately SEPARATE
	// from EnginePID, which a foreground run keeps at 0 so `workflow stop` never claims a kill on a
	// terminal-attached process it can't reap. ClassifyFgEngine reads them to tell a dead (Ctrl-C'd or
	// crashed) foreground run from a still-live one, so the sweep and the resume/restart guards can act
	// on it. A detached engine leaves them zero; an old (pre-field) record has them zero too.
	FgEnginePID       int    `json:"fg_engine_pid,omitempty"`
	FgEngineProcStart string `json:"fg_engine_proc_start,omitempty"`
	// Error is the failure cause, set when Status is "failed" — so a DETACHED run
	// (whose stderr went to /dev/null) still records WHY it failed for `workflow
	// status`. It is a canonical/script-level message (agent() failures carry
	// subagent's canonical error_msg, never raw provider body), so it is key-safe.
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
	// DefaultProvider / DefaultProviderError record the run's default-provider
	// resolution at mint, so a provider-less agent() resolves to the SAME provider on
	// resume regardless of a live config change (a mid-run default change must never
	// re-key an omitted-provider leaf). Exactly one is set when the run uses any
	// default: the resolved provider name, or the error_code (NO_DEFAULT_PROVIDER /
	// DEFAULT_PROVIDER_DISABLED) a provider-less agent() then throws. Both empty when
	// no default was resolvable AND nothing needed one (an all-explicit script). Not
	// a leaf determinant beyond the provider it supplies.
	DefaultProvider      string `json:"default_provider,omitempty"`
	DefaultProviderError string `json:"default_provider_error,omitempty"`
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
// providers/team/server). It must wrap an entry point's whole decision and NEVER the
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
// — the engine plus its in-flight provider-leaf `claude` children and their grandchildren
// (each leaf is its OWN process group, so an ancestry walk, not a single group signal, is
// required on unix; reapEngineTree handles the platform split). An identity reuse guard
// means a recycled EnginePID can NEVER make this kill an unrelated process: the pid is
// reaped only when its argv still proves it is this run's detached `workflow run …
// --run-id <id>` engine, or — where argv is unreadable (Windows) — when its kernel start
// token still equals the manifest's recorded one. A recorded DETACHED engine is classified TRI-STATE
// (ClassifyDetachedEngine): DetachedLive → reap + confirm dead + finalize ghosts; DetachedDead
// (gone / recycled) → finalize ghost leaves then flip stopped; DetachedUnknown (a retained pid alive
// but argv + start token both unreadable) → REFUSED with an error, status + journal left untouched —
// flipping it stopped or finalizing under a possibly-live engine would report a false success and let a
// caller act under it. A foreground run (EnginePID 0) has nothing to reap and simply flips stopped,
// clearing a stale "running". An already-terminal run is normally returned untouched; the ONE exception
// is a WEDGED terminal record still carrying a Live or Unknown detached pid (an old collapse, or a
// race) — it falls through so Live is properly reaped (an escape hatch) and Unknown fails closed. The
// reuse guard ensures an unverifiable pid is never killed.
func StopRun(runID string) (WorkflowRun, error) {
	run, err := ReadRun(runID)
	if err != nil {
		return WorkflowRun{}, err
	}
	if run.Status != "" && run.Status != "running" {
		// Already terminal → normally nothing live to reap. But a WEDGED terminal record can still carry a
		// detached pid that classifies Live or Unknown (an old binary-collapse stop that flipped stopped
		// under an unverifiable engine, or a race): don't no-op then — fall through so a Live engine is
		// properly reaped + re-verified and an Unknown one fails closed. Terminal + a Dead/absent pid keeps
		// the no-op (a normal done/failed/stopped record, whose engine exited, reads Dead here).
		if run.EnginePID <= 0 || ClassifyDetachedEngine(run) == DetachedDead {
			return run, nil
		}
	}
	if run.EnginePID > 0 {
		switch ClassifyDetachedEngine(run) {
		case DetachedLive:
			// Verifiably this run's live detached engine: reap it + confirm dead before finalizing its leaves.
			reapEngineTreeFn(run.EnginePID) // reaps the engine + its in-flight leaf children
			if !WaitEngineStopped(runID, stopReapTimeout) {
				// Won't die — do NOT flip stopped or clear the pid, or a caller (Restart / PurgeRun) would
				// falsely read it as dead and act under a still-live engine. Fail closed.
				return WorkflowRun{}, fmt.Errorf("subagent: run %s engine did not stop in time", runID)
			}
			// Confirmed dead: finalize the engine's mid-flight leaves (left "running" because its
			// finalizeSyncJob defer died with it) — never racing that defer, since the engine is gone.
			finalizeRunLeaves(runID)
		case DetachedUnknown:
			// A retained detached pid ALIVE but UNVERIFIABLE (argv + start token both unreadable): we can
			// neither kill-verify nor death-verify it, so we must not flip the run stopped or finalize its
			// journal under a possibly-live engine. Fail closed; self-clearing once the pid exits (→ Dead).
			return WorkflowRun{}, fmt.Errorf("subagent: run %s engine (pid %d) is alive but unverifiable; stop it manually or retry once it exits", runID, run.EnginePID)
		case DetachedDead:
			// A recorded detached pid DEFINITIVELY gone (dead, or recycled to a different process): its
			// mid-flight leaves are ghosts — finalize them, then flip stopped below.
			finalizeRunLeaves(runID)
		}
	}
	// EnginePID <= 0 (a foreground run, or the detached mint→stamp window) has nothing to reap: flip
	// stopped, clearing a stale "running".
	run.Status = "stopped"
	run.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	// Keep every identity field (EnginePID/EngineProcStart AND FgEnginePID/FgEngineProcStart): they are
	// the death evidence RunEngineProvablyNotLive / ClassifyFgEngine rely on. For a reaped DETACHED run
	// the retained pid+token positively read dead. A blind-flipped FOREGROUND run keeps EnginePID 0 but
	// retains its fg identity, so once its terminal engine exits it reads dead (resumable + reclaimable)
	// and while alive it reads alive (refused) — never cleared here, only re-stamped by a resume.
	if serr := SaveRun(run); serr != nil {
		return WorkflowRun{}, serr
	}
	return run, nil
}

// stopReapTimeout bounds how long StopRun waits for a reaped engine to actually die before
// finalizing its leaves — long enough for a SIGTERM'd engine to exit, short enough to not wedge.
// A var so a test can shorten the fail-closed did-not-stop path.
var stopReapTimeout = 5 * time.Second

// reapEngineTreeFn is StopRun's engine-tree reaper as a test seam (mirrors reapJobTree), so a test can
// drive the DetachedLive reap path without killing a real process.
var reapEngineTreeFn = reapEngineTree

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
		// A held leaf must leave `held` first — finalizeSyncJob suppresses a stopped
		// cache under a held meta (the live-engine hold window), and an external stop
		// is exactly the moment a hold becomes terminal.
		if meta.Status == "held" {
			ReleaseHeldLeafStopped(jobID, "run stopped while the leaf was held")
			continue
		}
		finalizeSyncJob(jobID, fail(ErrCodeStopped, "run stopped before the leaf finished", meta.Provider, ""))
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

// EngineAlive reports whether run's DETACHED engine MIGHT still be running — a read-only LIVENESS check
// (it kills nothing), used by the watchers to stop waiting on a stale "running" manifest AND by
// WaitEngineStopped to poll a reaped engine to death. It delegates to the tri-state
// ClassifyDetachedEngine and fails SOFT to alive: a foreground run (EnginePID 0) is "not alive", and a
// recorded detached pid is alive unless PROVABLY dead (DetachedDead: pid gone, or a READABLE identity
// mismatch = a recycled pid, which still reads dead). An alive-but-UNVERIFIABLE pid (argv + start token
// both unreadable) reads ALIVE — never a false "gone" for a live engine, which a consumer that treats
// !EngineAlive as death proof (WaitEngineStopped, the watch/wait engine-gone check) would otherwise act
// on. Unknown requires pidAlive, so treating it as alive is truthful.
func EngineAlive(run WorkflowRun) bool {
	if run.EnginePID <= 0 {
		return false // no recorded DETACHED engine (foreground / pre-stamp) — not this check's concern
	}
	return ClassifyDetachedEngine(run) != DetachedDead
}

// DetachedLiveness classifies a DETACHED engine's recorded identity as a TRI-STATE, the detached
// counterpart of ClassifyFgEngine. EngineAlive collapses "recycled/dead" and "unverifiable" into one
// false, which the reclaim path then reads as provably-dead — so an UNVERIFIABLE pid (alive but neither
// argv nor its start token readable: EPERM / hardened kernel / container) would be reclaimed under a
// still-running engine. This split keeps EngineAlive unchanged (StopRun's kill/refuse still consume it)
// while giving RunEngineProvablyNotLive the third answer.
type DetachedLiveness int

const (
	// DetachedUnknown: pid alive but its identity is unverifiable (argv unreadable AND its start token
	// unreadable/unrecorded) — the fail-safe class, NOT proof of death.
	DetachedUnknown DetachedLiveness = iota
	// DetachedDead: the recorded pid is gone, OR a READABLE identity proves a different process (recycled).
	DetachedDead
	// DetachedLive: the pid is alive and a readable identity still matches THIS run's detached engine.
	DetachedLive
)

// ClassifyDetachedEngine classifies run's DETACHED engine (EnginePID + argv/EngineProcStart). Rules:
// no pid → Unknown; pid gone → Dead; pid alive with READABLE argv → argv decides (match=Live,
// mismatch=Dead — a readable argv mismatch is decisive, a token match must never override it); pid
// alive + argv unreadable + a READABLE recorded token → match=Live, mismatch=Dead; pid alive with
// nothing readable → Unknown. Only Dead is proof the reclaim path may act on.
func ClassifyDetachedEngine(run WorkflowRun) DetachedLiveness {
	if run.EnginePID <= 0 {
		return DetachedUnknown // not a detached run
	}
	if !pidAlive(run.EnginePID) {
		return DetachedDead // the recorded pid is gone
	}
	if argv, ok := reuseGuardArgv(run.EnginePID); ok {
		if argvIsRunEngine(argv, run.RunID) {
			return DetachedLive
		}
		return DetachedDead // readable argv proves a DIFFERENT process (recycled)
	}
	if run.EngineProcStart != "" {
		live, ok := procStartFn(run.EnginePID)
		if !ok {
			return DetachedUnknown // pid alive, token unreadable → prove neither ours-alive nor recycled-dead
		}
		if live == run.EngineProcStart {
			return DetachedLive
		}
		return DetachedDead // recycled to a different process
	}
	return DetachedUnknown // pid alive, nothing readable (pre-token manifest) → not provably dead
}

// FgLiveness classifies a FOREGROUND run's recorded engine identity for the resume/restart guards
// and RunEngineProvablyNotLive.
type FgLiveness int

const (
	// FgUnknown: no fg identity recorded (a detached or pre-field run), or one whose liveness the
	// platform can't verify — the fail-safe class, treated as possibly-live.
	FgUnknown FgLiveness = iota
	// FgDead: the recorded foreground engine has exited, or its pid was recycled to a different process.
	FgDead
	// FgAlive: the recorded foreground engine is still the running process.
	FgAlive
)

// ClassifyFgEngine classifies run's FOREGROUND engine identity (FgEnginePID + FgEngineProcStart).
// There is NO argv check, unlike the detached engine: the original `workflow run --foreground`
// invocation's argv carries no run id (only the detached re-exec does), so pid + kernel start token
// is the WHOLE proof on every platform. Rules: no fg pid → FgUnknown; the pid is gone → FgDead; the
// pid is alive with a readable token that MATCHES → FgAlive; a readable MISMATCHED token → FgDead
// (recycled); an unreadable (EPERM / unsupported) or unrecorded token → FgUnknown. A token match is
// thus treated as ALIVE — the fail-safe direction (never resume under a possibly-live fg engine).
func ClassifyFgEngine(run WorkflowRun) FgLiveness {
	if run.FgEnginePID <= 0 {
		return FgUnknown
	}
	if !pidAlive(run.FgEnginePID) {
		return FgDead // the recorded pid is gone
	}
	live, ok := procStartFn(run.FgEnginePID)
	if !ok || run.FgEngineProcStart == "" {
		return FgUnknown // pid alive but unverifiable — can prove neither ours-alive nor recycled-dead
	}
	if live != run.FgEngineProcStart {
		return FgDead // the pid was recycled to a different process
	}
	return FgAlive
}

// RunEngineProvablyNotLive reports whether run's engine is DEFINITIVELY gone, so an external
// reclaimer (the worktree sweep / PurgeRun's temp-root drop) may safely reclaim what the engine left.
// BOTH the detached and foreground branches are tri-state and demand PROOF of death: a recorded
// DETACHED engine (EnginePID>0) is dead only when ClassifyDetachedEngine==DetachedDead (pid gone or a
// readable identity mismatch) — an UNVERIFIABLE alive pid (argv+token unreadable) is NOT proof and
// fails-safe to not-dead, mirroring the fg branch. With no detached pid, a FOREGROUND engine is dead
// only when ClassifyFgEngine==FgDead; an absent/unverifiable fg identity (a pre-field record, a
// still-live engine) is NOT proof. StopRun retains a reaped engine's pid + tokens precisely so this
// check has the evidence.
func RunEngineProvablyNotLive(run WorkflowRun) bool {
	if run.EnginePID > 0 {
		return ClassifyDetachedEngine(run) == DetachedDead
	}
	return ClassifyFgEngine(run) == FgDead
}

// leafProcessMayBeAlive reports whether a member leaf job MIGHT still have a running process — the
// orphan risk that engine death does NOT settle: a SIGKILLed engine's `claude -p` leaf children run
// in their OWN process groups (Setpgid) with cwd = the isolation worktree, so they outlive it. The
// check is IDENTITY-first, never trusting a cached result — a bare engine SIGKILL makes StatusFor
// synthesize a terminal (failVanished) for a still-LIVE orphan, so the result cache is unreliable:
//   - BACKGROUND leaf: meta.PID IS the claude child → processAlive decides;
//   - SYNC leaf with a recorded CHILD identity → the child's own pid+token decides (alive → possibly
//     alive; dead/recycled → dead) REGARDLESS of any cached result;
//   - SYNC leaf WITHOUT a child identity (old meta, or a crash between Start and the record): no
//     verifiable child and the terminal shortcut is unsound → fail-safe POSSIBLY ALIVE, except a
//     queued/never-execed leaf (PID<=0: no child, no worktree).
//
// An orphaned claude leaf is one-shot: it finishes and exits on its own, after which its recorded
// identity reads dead and the workdir becomes reclaimable — same-boot reclamation with a delay equal
// to the orphan's remaining runtime.
func leafProcessMayBeAlive(meta jobMeta) bool {
	if meta.SettingsPath != "" {
		return processAlive(meta.PID, meta.SettingsPath, meta.ProcStart) // background: PID is the child
	}
	if meta.ChildPID > 0 {
		return processAlive(meta.ChildPID, "", meta.ChildProcStart) // sync: the recorded child decides
	}
	if meta.ChildIdentityPending && meta.ChildPID == 0 {
		// A sync member (the pending stamp is member-only) still in the Start window: a child may have
		// started whose identity was never recorded, and PID being 0 (a hold premark clears it) says
		// nothing about that child. Possibly-alive — aligns the read-side veto with the keep-rule.
		return true
	}
	return meta.PID > 0 // sync, no child identity: execed → fail-safe alive; queued (PID 0) → dead
}

// isLiveOrphanVetoEvidence reports whether meta is a MEMBER leaf whose recorded CHILD identity is
// alive-or-unverifiable — the veto evidence a worktree segment relies on. Every job-meta removal path
// (GC, PurgeJobs, DeleteJob) must PRESERVE such a meta: deleting it would strip the segment's veto and
// let a colliding reclaimer delete the live child's workdir. Deliberately SCOPED to CHILD-IDENTIFIED
// metas (RunID != "" && ChildPID > 0): a synthesized terminal cache and a dead meta.PID (the engine
// proxy) don't matter — the child identity outranks them. An identity-less sync meta (a pre-change job,
// or a crash before the identity write) is NOT preserved here: its protection is the fail-safe veto
// WHILE it lives, and the GC TTL bounds the exposure (a one-shot claude leaf outliving the TTL is not a
// realistic lifetime, and preserving identity-less metas forever would break the GC contract).
func isLiveOrphanVetoEvidence(meta jobMeta) bool {
	return meta.RunID != "" && meta.ChildPID > 0 && leafProcessMayBeAlive(meta)
}

// isLiveOrPendingOrphanEvidence widens isLiveOrphanVetoEvidence with the Start-window: a sync member
// whose identity write is still PENDING (ChildIdentityPending, no ChildPID yet). A crash inside the
// ~ms cmd.Start→recordChildIdentity window leaves a LIVE orphan whose child identity was never
// recorded; the read-side fail-safe (leafProcessMayBeAlive: sync, no ChildPID, PID>0 → possibly-alive)
// already vetoes its segment, so AUTOMATED cleanup (GC/PurgeJobs) must KEEP the meta to match — else it
// reaps the veto evidence and the chokepoint then deletes the workdir under the orphan. Rare + bounded
// (a crash in that tiny window). The automated keep-rules AND PurgeRun's whole-run member walk use this
// wider form — the walk is a NON-path-safe id's only segment guard (it skips the path-safe segment
// verdict, which vetoes pending via leafProcessMayBeAlive). Only the EXPLICIT DeleteJob refusal stays on
// the narrower isLiveOrphanVetoEvidence, so a DeleteJob on the pending job is the record-only recovery
// escape (see DeleteJob). A legacy meta lacks the field (false), so the GC contract for pre-change
// jobs is unchanged.
func isLiveOrPendingOrphanEvidence(meta jobMeta) bool {
	return isLiveOrphanVetoEvidence(meta) || (meta.RunID != "" && meta.ChildIdentityPending && meta.ChildPID == 0)
}

// runLeafScan does ONE jobs-dir pass, returning the set of run-ids that have a possibly-alive member
// leaf and whether the scan succeeded (a scan error → false; callers must then fail safe / spare).
func runLeafScan() (map[string]bool, bool) {
	live := map[string]bool{}
	dir, err := jobsDir()
	if err != nil {
		return live, false
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return live, true // no jobs yet → no live leaves
		}
		return live, false
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		jobID := strings.TrimSuffix(name, ".json")
		meta, merr := readMeta(dir, jobID)
		if merr != nil {
			// FAIL CLOSED: an unreadable / partial-JSON member meta makes SOME run's leaf state
			// unknowable, and the id itself is unreadable so it can't be attributed to one segment —
			// so the whole scan is untrusted (ok=false). Every consumer then spares/refuses.
			return nil, false
		}
		if meta.RunID == "" {
			continue // a standalone job (no run) — not a workflow member leaf
		}
		if leafProcessMayBeAlive(meta) {
			live[meta.RunID] = true
		}
	}
	return live, true
}

// SegmentReclaim is a worktree SEGMENT's reclaimability, aggregated over ALL runs mapping to it
// (ids.WorktreeSegment) — path-safe or not. Worktree ownership is keyed by SEGMENT, not exact id, so a
// per-id check would let a live "a.b" (segment "a-b") lose its workdirs to a dead "a-b" being reclaimed.
// Vetoed: some owner is alive/unverifiable OR has a possibly-live leaf, so the segment's PRESENT
// workdirs must never be deleted. Reclaimer: some PATH-SAFE owner is provably engine-dead AND leaf-free
// — identity reclaim stays path-safe-only, so a non-path-safe id never RECLAIMS (leak-only) and, while
// alive, always VETOES its segment. A present workdir is removable iff Reclaimer && !Vetoed.
type SegmentReclaim struct {
	Vetoed    bool
	Reclaimer bool
}

// SegmentReclaimVerdicts builds the per-segment reclaim verdict in ONE ListRuns pass + ONE leaf scan
// (the perf shape the sweep needs — it checks many segments without rescanning). A scan/list failure
// returns ok=false so callers spare every present workdir (fail-safe).
func SegmentReclaimVerdicts() (map[string]SegmentReclaim, bool) {
	live, ok := runLeafScan()
	if !ok {
		return nil, false
	}
	runs, err := ListRuns()
	if err != nil {
		return nil, false
	}
	out := map[string]SegmentReclaim{}
	// Project the leaf-liveness evidence into segment vetoes FIRST — independent of ListRuns. A member
	// job meta's RunID is authoritative on its own, so a live leaf vetoes its segment even when the run's
	// MANIFEST is unreadable (ListRuns silently skips a corrupt runs/<id>.json). This closes the
	// corrupt-manifest sibling of the corrupt-meta hole: a live colliding owner (a.b whose manifest is
	// corrupt) still vetoes segment a-b, so a dead path-safe a-b can never delete a.b's live workdir. An
	// unreadable manifest only costs the Reclaimer half (it can never CONFIRM a reclaimer) — which stays
	// fail-closed via the unknown-segment spare.
	for runID := range live {
		seg := ids.WorktreeSegment(runID)
		v := out[seg]
		v.Vetoed = true
		out[seg] = v
	}
	for _, r := range runs {
		seg := ids.WorktreeSegment(r.RunID)
		v := out[seg]
		if RunEngineProvablyNotLive(r) && !live[r.RunID] {
			if seg == r.RunID { // a PATH-SAFE dead + leaf-free owner → an identity reclaimer
				v.Reclaimer = true
			}
		} else {
			v.Vetoed = true // an alive/unverifiable engine or a possibly-live leaf protects the segment
		}
		out[seg] = v
	}
	return out, true
}

// segReadDir is a seam so a test can drive the verdict-time segment snapshot — a workdir the snapshot
// omits (a colliding owner's post-snapshot fresh-uuid creation) must survive PurgeRun's removal.
var segReadDir = os.ReadDir

// PurgeRun deletes a run entirely — the board's manual "delete" (the board never auto-clears, so runs
// accumulate). A VERIFIABLY-live detached engine is stopped AND confirmed dead before any file is
// removed (the one-action delete-a-running-run): the engine is recreate-safe (it rewrites a dropped
// manifest on its next save), so deleting under it would not stick and could race its writes; if it
// can't be confirmed dead the delete is aborted. A detached pid alive but UNVERIFIABLE, and a live
// FOREGROUND engine, are instead REFUSED (stop it first) — neither can be verify-reaped. A "running"
// manifest whose engine is GONE — crashed, or finished without finalizing — is exactly the accumulated
// junk this is for, so it is removed as-is. The id is validated before it becomes any path component.
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
	if run, rerr := ReadRun(runID); rerr == nil {
		if run.EnginePID > 0 {
			switch ClassifyDetachedEngine(run) {
			case DetachedLive:
				// A VERIFIABLY-live detached engine → stop it + confirm dead before deleting anything (a
				// verified reap + wait is safe, and this is the board's one-action delete-a-running-run).
				// The worktreePurgeable re-read below then sees the terminal state.
				if _, serr := StopRun(runID); serr != nil {
					return serr
				}
				if !WaitEngineStopped(runID, 5*time.Second) {
					return fmt.Errorf("subagent: run %s engine did not stop in time; delete aborted", runID)
				}
			case DetachedUnknown:
				// A detached pid alive but UNVERIFIABLE (argv+token unreadable, or a blind-stop collapse
				// left {stopped, retained pid}): we can neither kill-verify nor death-verify it, so we
				// refuse — mirroring the foreground stance. Self-clearing: once it exits it reads
				// DetachedDead and the delete proceeds.
				return fmt.Errorf("subagent: run %s is still running (pid %d); stop it first (workflow stop), then delete", runID, run.EnginePID)
			}
			// DetachedDead → the accumulated junk this removes; proceed.
		} else if run.FgEnginePID > 0 && ClassifyFgEngine(run) != FgDead {
			// A recorded FOREGROUND engine we can't reap (it runs in the user's terminal, EnginePID 0)
			// whose pid is alive-and-not-provably-dead: deleting its record/jobs/worktrees under it would
			// corrupt a running run. Both FgAlive (token matches) and FgUnknown-WITH-a-recorded-pid (token
			// empty/unreadable — a live pid we cannot disprove, mirroring the detached DetachedUnknown
			// stance) refuse; a token MISMATCH reads FgDead (recycled) and proceeds. Self-clearing: once
			// the pid exits it reads FgDead and the delete proceeds. A FgUnknown with NO recorded pid
			// (FgEnginePID<=0 — a legacy / pre-field record) is not caught here and stays deletable.
			return fmt.Errorf("subagent: run %s is running in the foreground (pid %d); stop it in its terminal (Ctrl-C) first, then delete", runID, run.FgEnginePID)
		}
	}
	// Decide the worktree-temp purge NOW, while the manifest still exists: the RemoveAll near
	// the end deletes the run's isolation-worktree WORKDIRS, which — unlike the git registration
	// a later sweep reclaims — are not recreate-safe, so they must never be dropped under a live
	// engine's leaves. Purge only when the run is PROVABLY dead: a successful stop left a terminal
	// status, and a crashed detached run reads dead; a running FOREGROUND run (EnginePID 0) is not
	// provably dead, so its live leaves keep their cwd for their own cleanup or a later sweep. A
	// re-read failure (record already gone) fails closed → skip.
	worktreePurgeable := false
	if run, rerr := ReadRun(runID); rerr == nil {
		worktreePurgeable = RunEngineProvablyNotLive(run)
	}
	// Snapshot the run's OWN isolation-worktree segment AT VERDICT TIME — a colliding owner's
	// post-snapshot fresh-uuid workdir is then structurally out of reach of the removal below
	// (createWorktree always mints a NEW uuid dir under the segment, and nothing reuses a workdir path,
	// so a listed entry can never be a later, live creation). Only a PATH-SAFE (segment == id),
	// provably-dead (worktreePurgeable) run reclaims its segment; a non-path-safe id leaves it to the
	// sweep. Refuse (RETRYABLE) when the shared segment is still in use — a live/colliding owner or an
	// own orphan leaf vetoes — or its verdict can't be read (fail-closed: scan failed OR a MISSING entry
	// refuses). An unverifiable-foreground run (not worktreePurgeable) skips this and deletes its record.
	var segSnapshot []os.DirEntry
	segDir := ""
	if worktreePurgeable && ids.WorktreeSegment(runID) == runID {
		d := filepath.Join(os.TempDir(), "cc-fleet-worktrees", runID)
		if entries, rerr := segReadDir(d); rerr == nil {
			verdicts, vok := SegmentReclaimVerdicts()
			if v, present := verdicts[runID]; !vok || !present || v.Vetoed {
				return fmt.Errorf("subagent: run %s shares a worktree segment with a still-running engine or leaf; retry the delete after it exits", runID)
			}
			segDir, segSnapshot = d, entries
		}
	}
	dir, err := jobsDir()
	if err != nil {
		return err
	}
	// Collect this run's member jobs in ONE walk and REFUSE — id-shape-independent, BEFORE removing
	// anything — if ANY member is a live orphan OR still identity-PENDING: deleting its meta would erase
	// segment ids.WorktreeSegment(runID)'s veto and let a colliding reclaimer delete the shared workdir
	// under the (possible) orphan. This is the evidence-based gate the path-safe segment check above
	// misses — a valid NON-path-safe id ("a.b") whose segment ("a-b") a path-safe reclaimer owns bypasses
	// that branch entirely, so this walk is its ONLY segment guard. Pending-aware (isLiveOrPendingOrphan-
	// Evidence) to match the automated keep-rules: a crash-window member (ChildIdentityPending, no
	// ChildPID) is the sole veto for its segment and reaping it here would strip it. Legacy metas stay
	// deletable: both arms require a field they lack (ChildPID>0 OR ChildIdentityPending), so identity-less
	// old/non-isolation runs never trigger it. A live-orphan refusal self-clears once the
	// one-shot orphan exits; a pending refusal is resolved by the explicit DeleteJob recovery escape (an
	// unidentified orphan never records identity, so it never self-clears). The same walk feeds the removal.
	var members []string
	if entries, rerr := os.ReadDir(dir); rerr == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
				continue
			}
			jobID := strings.TrimSuffix(name, ".json")
			meta, merr := readMeta(dir, jobID)
			if merr != nil || meta.RunID != runID {
				continue
			}
			if isLiveOrPendingOrphanEvidence(meta) {
				if meta.ChildPID > 0 {
					return fmt.Errorf("subagent: run %s has a member leaf still running; retry the delete after it exits", runID)
				}
				return fmt.Errorf("subagent: run %s has a member (%s) whose leaf may have started but was never identified; delete that job to confirm it is gone, then retry", runID, jobID)
			}
			members = append(members, jobID)
		}
	}
	for _, jobID := range members {
		removeJob(dir, jobID)
		_ = pinned.Unpin(pinned.Job, jobID) // explicit delete clears any leaf pin marker
	}
	// Remove exactly the workdirs the verdict-time snapshot listed — BEFORE the manifest — so a
	// mid-removal crash leaves the workdir gone while the record still proves death (the sweep's
	// workdir-missing clause then reclaims the git registration; deleting the manifest first would strand
	// it unknown-present). Then remove the segment dir only if now EMPTY: a plain os.Remove tolerates
	// ENOTEMPTY as a leak-not-delete residual, so a surviving post-snapshot colliding workdir keeps it.
	if segDir != "" {
		for _, e := range segSnapshot {
			_ = os.RemoveAll(filepath.Join(segDir, e.Name()))
		}
		_ = os.Remove(segDir)
	}
	removeRun(filepath.Join(dir, runsDirName), runID)
	_ = pinned.Unpin(pinned.Run, runID) // explicit delete is the sanctioned removal of a pinned run
	return nil
}

// PruneRuns deletes every run whose engine is no longer alive — crashed/killed runs still stuck
// "running", plus terminal ones — while sparing any run with a live engine. Each delete runs under the
// per-run execution lock so it can't race a concurrent restart/resume, and the sweep is best-effort:
// a run PurgeRun refuses (an unverifiable-live engine, or one with a still-live orphan leaf) leaves
// `removed` unincremented and the loop continues — it never aborts the rest, and the run is retried
// next pass. Returns the number of runs removed.
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

// isLiveOrUnverifiable reports whether a run might still be writing, so PruneRuns spares it (deleting
// its record/jobs under a live engine would yank files out from under it). A verifiably-live DETACHED
// engine is protected; a detached pid that is dead/recycled is not. With no reapable detached pid
// (EnginePID<=0) the FOREGROUND identity decides: FgAlive (a live foreground engine, including a
// blind-stopped one) is protected; a recorded fg pid alive but token-unverifiable (FgUnknown WITH
// FgEnginePID>0) is spared the same way — a live pid we can't disprove (mirrors PurgeRun's refusal);
// FgDead (exited/crashed) is prunable — consistent with resume now allowing it; a FgUnknown with NO
// recorded pid (pre-field / mint→stamp) keeps the prior status-based behavior (a still-"running" one
// spared, a terminal one prunable) so a legacy record isn't stranded. Mirrors the fail-closed liveness
// guard the resume/restart paths use.
func isLiveOrUnverifiable(run WorkflowRun) bool {
	if run.EnginePID > 0 {
		// A detached engine is live-or-unverifiable unless PROVABLY dead: DetachedDead (pid gone or a
		// readable identity mismatch) → prunable; DetachedLive, and DetachedUnknown (a retained pid alive
		// but unverifiable — argv+token unreadable) → SPARED, so PruneRuns never deletes a run whose
		// engine it can't prove dead. Self-clearing: once that pid exits it reads DetachedDead and normal
		// pruning applies.
		return ClassifyDetachedEngine(run) != DetachedDead
	}
	switch ClassifyFgEngine(run) {
	case FgAlive:
		return true
	case FgDead:
		return false
	default: // FgUnknown
		if run.FgEnginePID > 0 {
			return true // a recorded fg pid we can't disprove — spare it (mirrors PurgeRun's refusal)
		}
		return run.Status == "running" // no recorded pid (pre-field / mint→stamp) — status-based, don't strand a legacy record
	}
}

// WaitEngineStopped polls a run's engine liveness until it is gone or the deadline passes (true once
// EngineAlive is false / the manifest is unreadable, false on timeout). EngineAlive fails SOFT (an
// alive-but-UNVERIFIABLE pid — argv + token unreadable — reads as alive), so a stuck poll times out into
// the caller's fail-closed path rather than declaring death and touching files under a still-live engine.
// Shared by StopRun's reap, PurgeRun (delete), and workflow.Restart.
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
