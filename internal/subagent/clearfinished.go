package subagent

import (
	"fmt"

	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/pinned"
)

// terminalStatus reports whether a job/run Status is a finished state the board's
// "clear-finished" action removes. running / queued / "" are still in-flight.
func terminalStatus(s string) bool {
	return s == "done" || s == "failed" || s == "stopped"
}

// pinnedRunMembers returns the set of run ids with at least one pinned member job. Cleanup keeps
// such a run whole so its pinned leaf is never orphaned — shared by ClearFinished, DeleteSession,
// and PruneRuns.
func pinnedRunMembers(jobs []Result, pins pinned.Set) map[string]bool {
	out := map[string]bool{}
	for _, j := range jobs {
		if j.RunID != "" && pins.Has(pinned.Job, j.JobID) {
			out[j.RunID] = true
		}
	}
	return out
}

// ClearFinished removes one session's finished records: workflow runs (with their member
// jobs) and standalone subagent jobs whose Status is done/failed/stopped and whose session
// matches sessionID. It is the board "clear-finished" / `subagent-gc --session` primitive —
// status-driven and immediate (no age threshold), deliberately distinct from GC's
// age/membership housekeeping (a crashed run still labeled "running" is NOT swept here).
//
// Pins are honored from the caller's snapshot: a pinned job, a pinned run, or a run with any
// pinned member is kept whole (the run and all its leaves), so a pinned leaf is never orphaned.
// Returns the number of run manifests + job groups removed (member jobs reaped with their run
// are not counted separately).
func ClearFinished(sessionID string, pins pinned.Set) (int, error) {
	if sessionID == "" {
		return 0, fmt.Errorf("subagent: ClearFinished requires a session id")
	}
	dir, err := jobsDir()
	if err != nil {
		return 0, err
	}
	jobs, err := ListJobs()
	if err != nil {
		return 0, err
	}
	runs, err := ListRuns()
	if err != nil {
		return 0, err
	}

	// A run with any pinned member is kept whole so the pinned leaf isn't orphaned.
	pinnedMemberRun := pinnedRunMembers(jobs, pins)

	removed := 0
	for _, r := range runs {
		if r.SessionID != sessionID || !terminalStatus(r.Status) {
			continue
		}
		if pins.Has(pinned.Run, r.RunID) || pinnedMemberRun[r.RunID] {
			continue
		}
		// The id comes from a manifest's JSON, not its filename, so validate it before it
		// becomes a delete path (GC derives ids from filenames; this path must not trust content).
		if ids.ValidateJobID(r.RunID) != nil {
			continue
		}
		// Delete the run through the SINGLE chokepoint — WithRunLock + PurgeRun — so the liveness / leaf /
		// segment guards AND PurgeRun's workdir-before-record ordering apply: a blind-stopped FgAlive run
		// (or one with a live orphan leaf, or a colliding-segment owner) is refused → skip-and-continue,
		// cleared on a later pass once the engine/orphan is gone — never deleted under a live engine or
		// stranded unknown-present. Mirrors PruneRuns. PurgeRun reaps the run's member jobs too.
		_ = WithRunLock(r.RunID, func() error {
			if PurgeRun(r.RunID) == nil {
				removed++
			}
			return nil
		})
	}

	// Standalone jobs (no run) that are finished, in-session, and unpinned. Run members are handled
	// above (PurgeRun reaps them); a member of a KEPT run stays attached to it.
	for _, j := range jobs {
		if j.RunID != "" {
			continue
		}
		if j.LeadSessionID != sessionID || !terminalStatus(j.Status) {
			continue
		}
		if pins.Has(pinned.Job, j.JobID) || ids.ValidateJobID(j.JobID) != nil {
			continue
		}
		removeJob(dir, j.JobID)
		removed++
	}
	return removed, nil
}

// DeleteSession removes EVERY record of one session — workflow runs (a verifiably-live detached run's
// engine is stopped first by PurgeRun; an unverifiable or foreground live run is refused and skipped)
// and standalone subagent jobs (a still-live one's process tree is reaped first) — EXCEPT pinned ones
// (a pinned run, a run with a pinned member, or a pinned job is kept; pins are removed only by an
// explicit per-record delete). Each run delete runs under the per-run lock so it can't race a
// concurrent restart/resume. A run that won't delete (a live engine that can't be verify-reaped, or a
// live orphan leaf) is skipped and REPORTED: the error names how many were skipped, alongside the
// count actually removed.
func DeleteSession(sessionID string, pins pinned.Set) (int, error) {
	if sessionID == "" {
		return 0, fmt.Errorf("subagent: DeleteSession requires a session id")
	}
	dir, err := jobsDir()
	if err != nil {
		return 0, err
	}
	runs, err := ListRuns()
	if err != nil {
		return 0, err
	}
	jobs, err := ListJobs()
	if err != nil {
		return 0, err
	}
	pinnedMemberRun := pinnedRunMembers(jobs, pins)

	removed, skipped := 0, 0
	var skipErr error
	for _, r := range runs {
		if r.SessionID != sessionID {
			continue
		}
		if pins.Has(pinned.Run, r.RunID) || pinnedMemberRun[r.RunID] {
			continue // pinned (or has a pinned member) — only an explicit per-record delete removes it
		}
		if ids.ValidateJobID(r.RunID) != nil {
			continue
		}
		id := r.RunID
		_ = WithRunLock(id, func() error {
			// PurgeRun refuses a live-or-unverifiable engine (reported as skipped), else reaps run + members.
			if perr := PurgeRun(id); perr != nil {
				skipped++
				if skipErr == nil {
					skipErr = perr
				}
			} else {
				removed++
			}
			return nil
		})
	}
	// Standalone jobs (no run), any status, in-session, unpinned. A non-terminal one is still
	// owned by a live process — reap its tree first so deleting the files can't orphan it.
	for _, j := range jobs {
		if j.RunID != "" || j.LeadSessionID != sessionID {
			continue
		}
		if pins.Has(pinned.Job, j.JobID) || ids.ValidateJobID(j.JobID) != nil {
			continue
		}
		if !terminalStatus(j.Status) {
			_ = ReapJob(j.JobID)
		}
		removeJob(dir, j.JobID)
		_ = pinned.Unpin(pinned.Job, j.JobID)
		removed++
	}
	if skipped > 0 {
		return removed, fmt.Errorf("%d removed; %d run(s) skipped (engine still alive): %v", removed, skipped, skipErr)
	}
	return removed, nil
}

// DeleteJob removes a single standalone job's file group and clears any pin (the board `d`
// per-record delete; works on a pinned record — the sanctioned manual removal). The id is
// validated before it becomes a path component.
func DeleteJob(jobID string) error {
	if err := ids.ValidateJobID(jobID); err != nil {
		return err
	}
	dir, err := jobsDir()
	if err != nil {
		return err
	}
	// A member leaf whose recorded CHILD is alive-or-unverifiable is veto evidence for its worktree
	// segment — refuse (retry once the child exits), mirroring PurgeRun's live-orphan refusal. Keyed on
	// the NARROW isLiveOrphanVetoEvidence (ChildPID>0), NOT the automated keep-rule: a still-identity-
	// PENDING member (Start-window crash, no ChildPID — a state that never self-resolves) is therefore
	// NOT blocked. This is the explicit record-only RECOVERY escape: a user DeleteJob on the pending job
	// proceeds (declared intent), after which its run is no longer member-blocked and cleans up normally;
	// no workdir is deleted under an unverifiable orphan (the user resolved it).
	if meta, merr := readMeta(dir, jobID); merr == nil && isLiveOrphanVetoEvidence(meta) {
		return fmt.Errorf("subagent: job %s is a run member whose leaf is still running; retry the delete after it exits", jobID)
	}
	removeJob(dir, jobID)
	_ = pinned.Unpin(pinned.Job, jobID)
	return nil
}
