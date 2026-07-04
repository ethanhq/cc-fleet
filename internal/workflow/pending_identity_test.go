package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestPendingIdentityWorktreeProtected (codex r24): a DETACHED dead-engine run with a PENDING
// identity-less member (Start-window crash) + a present workdir. The pending member's read-side veto
// protects the workdir: PurgeJobs keeps it (its run stays), the sweep spares it (segment vetoed), and a
// path-safe PurgeRun refuses. The explicit DeleteJob on the pending job is the record-only recovery
// escape; after it the run's rm is unblocked. Post-recovery disposition is asserted (see comment).
func TestPendingIdentityWorktreeProtected(t *testing.T) {
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	tempBase := filepath.Join(canonPath(os.TempDir()), "cc-fleet-worktrees")
	const id = "pend-run" // path-safe
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, id)) })
	wt := filepath.Join(tempBase, id, "wt")
	addWorktree(t, repo, wt)

	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err) // dead detached engine
	}
	jp, err := subagent.RunJournalPath(id)
	if err != nil {
		t.Fatal(err)
	}
	jobsDir := filepath.Dir(filepath.Dir(jp))
	// meta.PID = the CRASHED engine (dead), so PurgeJobs's processAlive keep-arm can't hold it — only the
	// pending clause can; child_pid absent (identity write never happened).
	pendMeta := fmt.Sprintf(`{"job_id":"pend","pid":%d,"status":"running","run_id":%q,"child_identity_pending":true}`, 0x7ffffffe, id)
	if err := os.WriteFile(filepath.Join(jobsDir, "pend.json"), []byte(pendMeta), 0o600); err != nil {
		t.Fatal(err)
	}

	// PurgeJobs keeps the pending member (→ its run's manifest) so the workdir survives.
	if _, _, _, perr := subagent.PurgeJobs(); perr != nil {
		t.Fatal(perr)
	}
	if _, serr := os.Stat(wt); serr != nil {
		t.Errorf("PurgeJobs must preserve the workdir under a pending member, err=%v", serr)
	}
	// The sweep spares the segment (vetoed by the pending member's read-side fail-safe).
	sweepRunWorktrees(repo)
	if _, serr := os.Stat(wt); serr != nil {
		t.Errorf("the sweep must SPARE the workdir (pending member vetoes the segment), err=%v", serr)
	}
	// A path-safe PurgeRun refuses (segment verdict vetoed by the pending member).
	if err := subagent.PurgeRun(id); err == nil || !strings.Contains(err.Error(), "worktree segment") {
		t.Errorf("path-safe PurgeRun must refuse under a pending member, got %v", err)
	}
	if _, serr := os.Stat(wt); serr != nil {
		t.Errorf("the refused PurgeRun must leave the workdir, err=%v", serr)
	}

	// Recovery escape: DeleteJob on the pending job PROCEEDS (record-only — never touches the workdir).
	if derr := subagent.DeleteJob("pend"); derr != nil {
		t.Errorf("DeleteJob on the pending job must proceed (recovery escape), got %v", derr)
	}
	if _, serr := os.Stat(wt); serr != nil {
		t.Errorf("DeleteJob itself must be record-only — the workdir must still exist, err=%v", serr)
	}
	// After the pending record is gone, the run's rm is unblocked and PROCEEDS. For a worktreePurgeable
	// (dead detached engine) run it removes the workdir WITH the record (ordered) — the user's explicit
	// DeleteJob resolved the orphan, so this is user-sanctioned, not a delete under an unverifiable orphan.
	if perr := subagent.PurgeRun(id); perr != nil {
		t.Errorf("after recovery the run's rm must proceed, got %v", perr)
	}
	if _, serr := os.Stat(wt); !os.IsNotExist(serr) {
		t.Errorf("after recovery the dead run's workdir is removed WITH the record (ordered), err=%v", serr)
	}
}

// TestPendingHeldMemberVetoesSegment (codex r25): a HELD premark clears meta.PID, so a Start-window
// crash after a hold directive leaves {held, PID 0, ChildPID 0, ChildIdentityPending}. The PID>0
// fallback would read it DEAD (no segment veto); the read side must instead veto it (aligned with the
// keep-rule). Dead engine + present workdir → sweep spares, path-safe PurgeRun refuses; DeleteJob
// recovers (r24 chain, held shape).
func TestPendingHeldMemberVetoesSegment(t *testing.T) {
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	tempBase := filepath.Join(canonPath(os.TempDir()), "cc-fleet-worktrees")
	const id = "held-pend" // path-safe
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, id)) })
	wt := filepath.Join(tempBase, id, "wt")
	addWorktree(t, repo, wt)

	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err)
	}
	jp, err := subagent.RunJournalPath(id)
	if err != nil {
		t.Fatal(err)
	}
	jobsDir := filepath.Dir(filepath.Dir(jp))
	// {held, PID 0, pending, no child}: the hold premark cleared PID, but a child may have started.
	heldMeta := fmt.Sprintf(`{"job_id":"hp","pid":0,"status":"held","run_id":%q,"child_identity_pending":true}`, id)
	if err := os.WriteFile(filepath.Join(jobsDir, "hp.json"), []byte(heldMeta), 0o600); err != nil {
		t.Fatal(err)
	}

	sweepRunWorktrees(repo)
	if _, serr := os.Stat(wt); serr != nil {
		t.Errorf("the sweep must SPARE the workdir — a held-pending member vetoes the segment, err=%v", serr)
	}
	if perr := subagent.PurgeRun(id); perr == nil || !strings.Contains(perr.Error(), "worktree segment") {
		t.Errorf("path-safe PurgeRun must refuse under a held-pending member, got %v", perr)
	}
	if _, serr := os.Stat(wt); serr != nil {
		t.Errorf("the refused PurgeRun must leave the workdir, err=%v", serr)
	}
	// Recovery: DeleteJob on the held-pending job proceeds → the run's rm is unblocked.
	if derr := subagent.DeleteJob("hp"); derr != nil {
		t.Errorf("DeleteJob on the held-pending job must proceed (recovery), got %v", derr)
	}
	if perr := subagent.PurgeRun(id); perr != nil {
		t.Errorf("after recovery the run's rm must proceed, got %v", perr)
	}
}
