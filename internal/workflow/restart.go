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

// resumeBlockedReason reports why a run may not be resumed or restarted (nil when it may). It decides
// the FOREGROUND cases (no reapable detached pid, EnginePID<=0) from the recorded fg engine identity:
// a provably-dead fg engine — a Ctrl-C'd run (its shutdown defer wrote the manifest then it exited) or
// a crashed one — is resumable; a still-LIVE one is refused (resuming would run a second engine on this
// manifest/journal/worktree segment, whose later-dead pid would falsely condemn the survivor's live
// worktrees to the sweep); an absent/unverifiable identity (an old pre-field record, or a platform
// that can't read the token) keeps the conservative refusal. A recorded DETACHED engine (EnginePID>0)
// is decided by the tri-state ClassifyDetachedEngine: dead/recycled or verifiably-live → nil (the
// caller's running-refuse / stop-and-confirm path replays or reaps it); an alive-but-UNVERIFIABLE
// retained pid → refused here (its stop-and-confirm keys on Status=="running", so a blind-stopped one
// would otherwise resume under a possibly-live engine). Shared by Launch's resume preflight and
// ensureRestartable so both refuse identically — the latter BEFORE any journal rewrite.
func resumeBlockedReason(run subagent.WorkflowRun) error {
	if run.Status != "stopped" && run.Status != "running" {
		return nil // terminal (done/failed) — a resume/restart is not a live-engine concern here
	}
	if run.EnginePID > 0 {
		// A recorded DETACHED engine: dead/recycled or verifiably-live → nil (the running-refuse /
		// stop-and-confirm path replays or reaps it). But an alive-but-UNVERIFIABLE retained pid is
		// refused: a blind-stop collapse can leave {stopped, retained pid} whose stop-and-confirm never
		// fires (that guard keys on Status=="running"), so resuming could run a SECOND engine on this
		// manifest / journal / worktree segment — whose later-dead pid would then falsely condemn the
		// survivor's live worktrees to the sweep. Self-clearing: once the pid exits it reads DetachedDead.
		if subagent.ClassifyDetachedEngine(run) == subagent.DetachedUnknown {
			return fmt.Errorf("workflow: run %s has a detached engine (pid %d) whose death cannot be verified; confirm the process has exited, then delete it (workflow rm) or launch fresh", run.RunID, run.EnginePID)
		}
		return nil
	}
	switch subagent.ClassifyFgEngine(run) {
	case subagent.FgDead:
		return nil // the foreground engine exited (Ctrl-C'd or crashed) — safe to resume/restart
	case subagent.FgAlive:
		return fmt.Errorf("workflow: run %s is still running in the foreground; stop it in its terminal (Ctrl-C) first", run.RunID)
	default: // FgUnknown — an old record with no fg identity, or one whose liveness can't be verified
		return fmt.Errorf("workflow: run %s has a foreground engine whose death cannot be verified; confirm its terminal process has exited, then delete it (workflow rm) or launch fresh", run.RunID)
	}
}

// ensureRestartable is the shared pre-restart barrier: the run must be resumable
// (resumeBlockedReason), the saved script must be readable (with the explicit pre-JS-engine
// refusal), and a still-"running" run's engine must be verifiably GONE before any journal rewrite
// (it O_APPENDs to it).
func ensureRestartable(runID string) (string, error) {
	run, err := subagent.ReadRun(runID)
	if err != nil {
		return "", err
	}
	// Refuse an unresumable run BEFORE the caller rewrites the journal (removeJournalKey/Keys), so a
	// restart never leaves a mutate-then-fail half-state — the journal must not be touched under an
	// engine whose death can't be verified. Same predicate Launch's preflight uses, so no drift.
	if reason := resumeBlockedReason(run); reason != nil {
		return "", reason
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
	if run.Status == "running" && subagent.EngineAlive(run) {
		// A verifiably-live DETACHED engine → stop it + confirm dead (abort if it won't die in time). A
		// foreground run (EnginePID<=0) was already vetted by resumeBlockedReason above (dead→resume,
		// live→refused); a crashed/killed detached run (recorded pid now dead) falls straight through.
		if _, serr := subagent.StopRun(runID); serr != nil {
			return "", serr
		}
		if !subagent.WaitEngineStopped(runID, stopBarrierTimeout) {
			return "", fmt.Errorf("workflow: run %s engine did not stop in time; restart aborted", runID)
		}
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
