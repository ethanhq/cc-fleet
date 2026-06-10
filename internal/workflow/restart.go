package workflow

import (
	"context"
	"fmt"
	"os"
	"sort"
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
	scriptPath, err := ensureRestartable(runID)
	if err != nil {
		return err
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

// ensureRestartable is the shared pre-restart barrier: the saved script must be
// readable (with the explicit pre-JS-engine refusal) and a still-"running" run's
// engine must be verifiably GONE before any journal rewrite (it O_APPENDs to it).
func ensureRestartable(runID string) (string, error) {
	run, err := subagent.ReadRun(runID)
	if err != nil {
		return "", err
	}
	scriptPath, err := subagent.RunScriptPath(runID)
	if err != nil {
		return "", err
	}
	if _, serr := os.Stat(scriptPath); serr != nil {
		if lp, lerr := subagent.LegacyRunScriptPath(runID); lerr == nil {
			if _, sterr := os.Stat(lp); sterr == nil {
				return "", fmt.Errorf("workflow: run %s predates the JavaScript workflow engine; its Starlark script can't restart — start a fresh run", runID)
			}
		}
		return "", fmt.Errorf("workflow: saved script for run %s is unavailable; cannot restart: %w", runID, serr)
	}
	if run.Status == "running" {
		switch {
		case subagent.EngineAlive(run):
			// A verifiably-live detached engine → stop it + confirm dead (abort if it won't die in time).
			if _, serr := subagent.StopRun(runID); serr != nil {
				return "", serr
			}
			if !subagent.WaitEngineStopped(runID, stopBarrierTimeout) {
				return "", fmt.Errorf("workflow: run %s engine did not stop in time; restart aborted", runID)
			}
		case run.EnginePID <= 0:
			// A foreground run (or a detached run in the mint→stamp-pid window) still claiming to run has
			// no killable engine to confirm dead — resuming could run two engines on one journal. Fail
			// closed; stop it first.
			return "", fmt.Errorf("workflow: run %s is running in the foreground; stop it first", runID)
		}
		// else: a crashed/killed DETACHED run (recorded pid now dead) — safe to resume as-is.
	}
	return scriptPath, nil
}

// RestartPhase re-runs a TERMINAL run's phase: under the per-run lock + stop barrier it
// collects the phase's journal-key SET from the member jobs, whole-key drops the set,
// and resumes (un-journaled members — failed/stopped leaves — re-run regardless).
// Returns the OTHER phase titles the restart widens into: identical agent() calls share
// one content key, so a key whose jobs span more than one phase re-runs everywhere it
// appears — meta-derived per-phase counts would over-remove, the whole-key drop is the
// honest scope and the caller names it.
func RestartPhase(ctx context.Context, runID, phase string) ([]string, error) {
	var widened []string
	err := subagent.WithRunLock(runID, func() error {
		scriptPath, perr := ensureRestartable(runID)
		if perr != nil {
			return perr
		}
		_, leaves, serr := subagent.RunStatus(runID)
		if serr != nil {
			return serr
		}
		var keys map[string]bool
		keys, widened = phaseRestartPlan(leaves, phase)
		if len(keys) > 0 {
			jp, jerr := subagent.RunJournalPath(runID)
			if jerr != nil {
				return jerr
			}
			if _, rerr := removeJournalKeys(jp, keys); rerr != nil {
				return fmt.Errorf("workflow: invalidate phase: %w", rerr)
			}
		}
		_, lerr := Launch(ctx, scriptPath, Options{Resume: runID}, false)
		return lerr
	})
	return widened, err
}

// phaseRestartPlan derives a keyed phase restart's scope from the run's member leaves:
// the phase's journal-key set, plus the OTHER phase titles those keys also appear in
// (the honest widening a whole-key drop implies).
func phaseRestartPlan(leaves []subagent.Result, phase string) (map[string]bool, []string) {
	keys := map[string]bool{}
	for _, l := range leaves {
		if l.Phase == phase && l.JournalKey != "" {
			keys[l.JournalKey] = true
		}
	}
	widenedSet := map[string]bool{}
	for _, l := range leaves {
		if l.Phase != phase && l.JournalKey != "" && keys[l.JournalKey] {
			widenedSet[l.Phase] = true
		}
	}
	widened := make([]string, 0, len(widenedSet))
	for p := range widenedSet {
		widened = append(widened, p)
	}
	sort.Strings(widened)
	return keys, widened
}
