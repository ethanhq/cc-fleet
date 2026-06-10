package subagent

import (
	"fmt"
	"path/filepath"

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
	removedJob := map[string]bool{}
	runsPath := filepath.Join(dir, runsDirName)
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
		// Reap the run's (unpinned) member jobs, then its manifest. pinnedMemberRun already
		// excluded a run with a pinned member, so no member here is pinned.
		for _, j := range jobs {
			if j.RunID == r.RunID && ids.ValidateJobID(j.JobID) == nil {
				removeJob(dir, j.JobID)
				removedJob[j.JobID] = true
			}
		}
		removeRun(runsPath, r.RunID)
		removed++
	}

	// Standalone jobs (no run) that are finished, in-session, and unpinned. Run members are
	// handled above; a member of a KEPT run stays attached to it.
	for _, j := range jobs {
		if j.RunID != "" || removedJob[j.JobID] {
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

// DeleteSession removes EVERY record of one session — workflow runs (any status; a live run's engine
// is stopped first by PurgeRun) and standalone subagent jobs (a still-live one's process tree is
// reaped first) — EXCEPT pinned ones (a pinned run, a run with a pinned member, or a pinned job is
// kept; pins are removed only by an explicit per-record delete). Each run delete runs under the
// per-run lock so it can't race a concurrent restart/resume. A run that won't delete (its engine
// can't be stopped in time) is skipped and REPORTED: the error names how many were skipped, alongside
// the count actually removed.
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
			// PurgeRun stops a live engine first, then reaps the run + members.
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
	removeJob(dir, jobID)
	_ = pinned.Unpin(pinned.Job, jobID)
	return nil
}
