package workflow

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// stopBarrierTimeout bounds how long Restart waits for a live engine to actually exit after
// StopRun before giving up. removeJournalKey rewrites the journal with AtomicWrite while the
// engine could still O_APPEND, so the engine MUST be confirmed dead first; if it won't die in
// time we abort rather than risk corrupting the journal.
const stopBarrierTimeout = 5 * time.Second

// Restart re-runs a workflow run, optionally scoped to a single leaf. With journalKey set it
// drops that leaf's cached result so the resume re-runs ONLY it — plus any downstream leaf
// whose input embedded the old answer (content-addressing recomputes those keys → cache miss);
// every other leaf replays from the journal instantly. With an empty journalKey it is a whole-
// run resume (re-runs only the un-journaled / failed leaves). cc-fleet runs ONE engine per run,
// so before touching the journal a still-"running" run is resolved: a verifiably-live detached
// engine is stopped + confirmed dead (its in-flight leaves then re-run on resume); a crashed/killed
// detached run (recorded pid now dead) is resumed as-is; a foreground run with no killable engine
// fails closed. The resume replays the run's original launch options (args / persistIO / budget) off
// the manifest so a leaf's key — and thus its cache validity — doesn't shift.
// The whole decision runs under the per-run execution lock so a concurrent restart / resume / stop never
// acts on stale state or races the pre-launch pid window; the lock releases when Launch returns (after the
// child registers) and is NEVER held around the engine's Execute. StopRun/Launch internals stay lock-free.
func Restart(ctx context.Context, runID, journalKey string) error {
	return subagent.WithRunLock(runID, func() error { return restartLocked(ctx, runID, journalKey) })
}

func restartLocked(ctx context.Context, runID, journalKey string) error {
	run, err := subagent.ReadRun(runID)
	if err != nil {
		return err
	}
	scriptPath, err := subagent.RunScriptPath(runID)
	if err != nil {
		return err
	}
	// The saved script is what the resume re-executes — verify it's readable BEFORE any
	// destructive step (stop / journal rewrite), so a missing script never leaves a half-torn run.
	// A run that carries only the retired Starlark engine's .star sidecar is refused explicitly:
	// its script can't execute on the JavaScript runtime.
	if _, serr := os.Stat(scriptPath); serr != nil {
		if lp, lerr := subagent.LegacyRunScriptPath(runID); lerr == nil {
			if _, sterr := os.Stat(lp); sterr == nil {
				return fmt.Errorf("workflow: run %s predates the JavaScript workflow engine; its Starlark script can't restart — start a fresh run", runID)
			}
		}
		return fmt.Errorf("workflow: saved script for run %s is unavailable; cannot restart: %w", runID, serr)
	}
	// A running run's engine must be GONE before the journal is rewritten (it O_APPENDs to it).
	if run.Status == "running" {
		switch {
		case subagent.EngineAlive(run):
			// A verifiably-live detached engine → stop it + confirm dead (abort if it won't die in time).
			if _, serr := subagent.StopRun(runID); serr != nil {
				return serr
			}
			if !subagent.WaitEngineStopped(runID, stopBarrierTimeout) {
				return fmt.Errorf("workflow: run %s engine did not stop in time; restart aborted", runID)
			}
		case run.EnginePID <= 0:
			// A foreground run (or a detached run in the mint→stamp-pid window) still claiming to run has
			// no killable engine to confirm dead — resuming could run two engines on one journal. Fail
			// closed; stop it first.
			return fmt.Errorf("workflow: run %s is running in the foreground; stop it first", runID)
		}
		// else: a crashed/killed DETACHED run (recorded pid now dead) — safe to resume as-is.
	}
	if journalKey != "" {
		jp, jerr := subagent.RunJournalPath(runID)
		if jerr != nil {
			return jerr
		}
		if _, rerr := removeJournalKey(jp, journalKey); rerr != nil {
			return fmt.Errorf("workflow: invalidate leaf: %w", rerr)
		}
	}
	// Launch's resume branch replays the run's original launch options off the manifest.
	_, err = Launch(ctx, scriptPath, Options{Resume: runID}, false)
	return err
}
