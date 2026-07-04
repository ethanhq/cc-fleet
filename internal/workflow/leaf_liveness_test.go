package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/procintrospect"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// seedLeafMeta writes a member job meta for runID into the jobs dir (derived from a run sidecar path,
// since writeMeta is subagent-internal). status "running" seeds the normal sync shape (meta.PID = the
// ENGINE); status "held" seeds the hold-premark shape (Status held, PID 0). childPID + childTok are the
// recorded claude-child identity the reclaimers check. terminal writes a (possibly synthesized) cached
// result the identity-first predicate must ignore.
func seedLeafMeta(t *testing.T, runID, jobID, status string, childPID int, childTok string, terminal bool) {
	t.Helper()
	jp, err := subagent.RunJournalPath(runID) // .../subagent-jobs/runs/<id>.journal
	if err != nil {
		t.Fatal(err)
	}
	jobsDir := filepath.Dir(filepath.Dir(jp)) // .../subagent-jobs
	if err := os.MkdirAll(jobsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid() // sync leaf's meta.PID is the engine
	if status == "held" {
		pid = 0 // the hold premark clears PID; only the child identity can protect an orphan here
	}
	meta := fmt.Sprintf(`{"job_id":%q,"pid":%d,"status":%q,"run_id":%q,"child_pid":%d,"child_proc_start":%q}`,
		jobID, pid, status, runID, childPID, childTok)
	if err := os.WriteFile(filepath.Join(jobsDir, jobID+".json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	if terminal {
		if err := os.WriteFile(filepath.Join(jobsDir, jobID+".result.json"), []byte(`{"ok":false,"status":"failed"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSweepHonorsLiveLeafWorkdir: engine death does not prove leaf death, and identity outranks the
// result cache. The Execute-time sweep SPARES a provably-dead run's PRESENT workdir while the member
// leaf's CHILD is alive — EVEN with a synthesized terminal cached (the side door) — reclaims it once
// the child reads dead, and reclaims a MISSING workdir regardless (clause b needs no leaf check).
func TestSweepHonorsLiveLeafWorkdir(t *testing.T) {
	liveTok, ok := procintrospect.ProcStart(os.Getpid())
	if !ok {
		t.Skip("procintrospect.ProcStart unavailable on this platform")
	}
	seedDeadRunWithWorktree := func(t *testing.T, id string) (repo, wt string) {
		repo = initSweepRepo(t)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		tempBase := filepath.Join(canonPath(os.TempDir()), "cc-fleet-worktrees")
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, id)) })
		if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
			t.Fatal(err)
		}
		wt = filepath.Join(tempBase, id, "wt")
		addWorktree(t, repo, wt)
		return repo, wt
	}

	t.Run("present workdir spared while the child is alive (synthesized terminal ignored)", func(t *testing.T) {
		repo, wt := seedDeadRunWithWorktree(t, "leaf-present")
		seedLeafMeta(t, "leaf-present", "leaf1", "running", os.Getpid(), liveTok, true) // live child + cached terminal
		sweepRunWorktrees(repo)
		if !worktreeListed(t, repo, wt) {
			t.Error("a present workdir must be spared while the member leaf's child is alive, cache notwithstanding")
		}
	})

	t.Run("present workdir reclaimed once the child is dead", func(t *testing.T) {
		repo, wt := seedDeadRunWithWorktree(t, "leaf-dead")
		seedLeafMeta(t, "leaf-dead", "leaf1", "running", 0x7ffffffe, "", false) // dead child, no cache
		sweepRunWorktrees(repo)
		if worktreeListed(t, repo, wt) {
			t.Error("a present workdir must be reclaimed once the member leaf's child reads dead")
		}
	})

	t.Run("missing workdir reclaimed regardless of a live child", func(t *testing.T) {
		repo, wt := seedDeadRunWithWorktree(t, "leaf-gone")
		seedLeafMeta(t, "leaf-gone", "leaf1", "running", os.Getpid(), liveTok, false) // a live child...
		if err := os.RemoveAll(wt); err != nil {                                      // ...but the workdir is gone
			t.Fatal(err)
		}
		sweepRunWorktrees(repo)
		if worktreeListed(t, repo, wt) {
			t.Error("a missing workdir must be reclaimed regardless of the leaves (clause b needs no leaf check)")
		}
	})
}

// seedDeadRunWorktree seeds a provably-dead run (stopped + dead detached pid) with one registered
// worktree, returning the repo + the worktree path.
func seedDeadRunWorktree(t *testing.T, id string) (repo, wt string) {
	t.Helper()
	repo = initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	tempBase := filepath.Join(canonPath(os.TempDir()), "cc-fleet-worktrees")
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, id)) })
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err)
	}
	wt = filepath.Join(tempBase, id, "wt")
	addWorktree(t, repo, wt)
	return repo, wt
}

// TestReclaimSparesHeldPremarkOrphan: the hold protocol premarks {held, PID 0} BEFORE killing the
// child, so an engine crash in that window leaves a live orphan whose ONLY protection is its recorded
// child identity (the PID-0 fallback would read dead). Both the Execute-time sweep AND the launcher
// own-segment sweep must spare a held-premark row with a live child, and reclaim once the child dies.
func TestReclaimSparesHeldPremarkOrphan(t *testing.T) {
	liveTok, ok := procintrospect.ProcStart(os.Getpid())
	if !ok {
		t.Skip("procintrospect.ProcStart unavailable on this platform")
	}

	t.Run("sweep spares a held-premark orphan (live child)", func(t *testing.T) {
		repo, wt := seedDeadRunWorktree(t, "held-sweep")
		seedLeafMeta(t, "held-sweep", "leaf1", "held", os.Getpid(), liveTok, false) // {held, PID 0, live child}
		sweepRunWorktrees(repo)
		if !worktreeListed(t, repo, wt) {
			t.Error("the Execute-time sweep must spare a held-premark orphan's workdir (child alive)")
		}
	})

	t.Run("sweepOwnSegment spares a held-premark orphan (live child)", func(t *testing.T) {
		repo, wt := seedDeadRunWorktree(t, "held-own")
		seedLeafMeta(t, "held-own", "leaf1", "held", os.Getpid(), liveTok, false)
		sweepOwnSegment(repo, "held-own")
		if !worktreeListed(t, repo, wt) {
			t.Error("the launcher own-segment sweep must spare a held-premark orphan's workdir (child alive)")
		}
	})

	t.Run("reclaimed once the held orphan's child is dead", func(t *testing.T) {
		repo, wt := seedDeadRunWorktree(t, "held-dead")
		seedLeafMeta(t, "held-dead", "leaf1", "held", 0x7ffffffe, "", false) // {held, PID 0, DEAD child}
		sweepRunWorktrees(repo)
		if worktreeListed(t, repo, wt) {
			t.Error("a held row whose child is dead must be reclaimed")
		}
	})
}

// TestSweepSparesCollidingSegment (the codex r13 scenario): ownership is SEGMENT-level, so a LIVE
// non-path-safe "a.b" and a dead path-safe "a-b" share segment "a-b". BOTH the Execute-time sweep and
// the launcher own-segment sweep must SPARE the shared workdir (a.b may be using it); once a.b dies it
// becomes reclaimable.
func TestSweepSparesCollidingSegment(t *testing.T) {
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	tempBase := filepath.Join(canonPath(os.TempDir()), "cc-fleet-worktrees")
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, "a-b")) })
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: "a-b", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err)
	}
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: "a.b", StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: 0}); err != nil {
		t.Fatal(err) // LIVE non-path-safe twin over segment a-b
	}
	wt := filepath.Join(tempBase, "a-b", "wt")
	addWorktree(t, repo, wt)

	sweepRunWorktrees(repo)
	if !worktreeListed(t, repo, wt) {
		t.Error("the Execute-time sweep must spare a segment a colliding live twin (a.b) uses")
	}
	sweepOwnSegment(repo, "a-b")
	if !worktreeListed(t, repo, wt) {
		t.Error("the launcher own-segment sweep must spare a segment a colliding live twin (a.b) uses")
	}

	// a.b dies → the segment (path-safe reclaimer a-b, no veto) becomes reclaimable.
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: "a.b", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err)
	}
	sweepRunWorktrees(repo)
	if worktreeListed(t, repo, wt) {
		t.Error("once the colliding twin dies, the shared segment is reclaimable")
	}
}

// TestSweepOwnSegmentSnapshotScoped (codex r14 TOCTOU): sweepOwnSegment removes only the git-registered
// (porcelain-snapshot) worktrees and os.Remove's the segment dir only if empty — so a workdir NOT in
// the snapshot (here an unregistered dir, standing in for a post-snapshot fresh-uuid creation) SURVIVES.
func TestSweepOwnSegmentSnapshotScoped(t *testing.T) {
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	tempBase := filepath.Join(canonPath(os.TempDir()), "cc-fleet-worktrees")
	const id = "own-snap"
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, id)) })
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err)
	}
	regWt := filepath.Join(tempBase, id, "reg-uuid") // git-registered → in the porcelain snapshot
	addWorktree(t, repo, regWt)
	unregWt := filepath.Join(tempBase, id, "fresh-uuid") // NOT registered → not in the snapshot
	if err := os.MkdirAll(unregWt, 0o700); err != nil {
		t.Fatal(err)
	}

	sweepOwnSegment(repo, id)

	if worktreeListed(t, repo, regWt) {
		t.Error("the porcelain-listed registration must be reclaimed")
	}
	if _, err := os.Stat(unregWt); err != nil {
		t.Errorf("an unlisted (post-snapshot) workdir must SURVIVE, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(tempBase, id)); err != nil {
		t.Errorf("the segment dir must survive (not empty) — os.Remove is empty-only, err=%v", err)
	}
}

// TestSweepSparesCorruptManifestCollision (codex r16): a corrupt run manifest is silently skipped by
// ListRuns, but the live member leaf's veto stands on its own (the leaf projection), so a live colliding
// a.b (with a truncated runs/a.b.json) still vetoes segment a-b. Both the sweep and the own-segment sweep
// spare the shared workdir; once a.b's member dies it's reclaimable.
func TestSweepSparesCorruptManifestCollision(t *testing.T) {
	liveTok, ok := procintrospect.ProcStart(os.Getpid())
	if !ok {
		t.Skip("procintrospect.ProcStart unavailable on this platform")
	}
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	tempBase := filepath.Join(canonPath(os.TempDir()), "cc-fleet-worktrees")
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, "a-b")) })

	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: "a-b", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err) // dead path-safe reclaimer, valid manifest
	}
	seedLeafMeta(t, "a.b", "ablj", "running", os.Getpid(), liveTok, false) // LIVE a.b member
	jp, err := subagent.RunJournalPath("x")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(jp), "a.b.json"), []byte(`{"run_id":"a.b","stat`), 0o600); err != nil {
		t.Fatal(err) // truncated a.b manifest
	}
	wt := filepath.Join(tempBase, "a-b", "wt")
	addWorktree(t, repo, wt)

	sweepRunWorktrees(repo)
	if !worktreeListed(t, repo, wt) {
		t.Error("the sweep must spare segment a-b — a.b's live member vetoes it despite the corrupt manifest")
	}
	sweepOwnSegment(repo, "a-b")
	if !worktreeListed(t, repo, wt) {
		t.Error("the own-segment sweep must skip segment a-b (colliding live a.b)")
	}

	seedLeafMeta(t, "a.b", "ablj", "running", 0x7ffffffe, "", false) // a.b's member dies
	sweepRunWorktrees(repo)
	if worktreeListed(t, repo, wt) {
		t.Error("once a.b's member is dead, the shared segment is reclaimable")
	}
}

// TestLegacyFgRunWorktreeUntouched (codex r23 rebuttal, executable): a {stopped, EnginePID 0, NO fg
// identity} legacy run is NOT provably dead (FgUnknown), so every cleanup path may remove its RECORD
// (today's adjudicated behavior) but NEVER touches its WORKDIR or git registration — worktreePurgeable
// is false so PurgeRun skips the snapshot removal on direct rm / PruneRuns / PurgeJobs (its wholesale
// RemoveAll only reaches the jobs-dir records, not the os.TempDir worktree tree nor the repo's .git),
// and a subsequent sweep SPARES the now-unknown present segment.
func TestLegacyFgRunWorktreeUntouched(t *testing.T) {
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	tempBase := filepath.Join(canonPath(os.TempDir()), "cc-fleet-worktrees")
	const id = "legacy-fg" // path-safe
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, id)) })
	wt := filepath.Join(tempBase, id, "wt")
	addWorktree(t, repo, wt) // registration + workdir, added ONCE — must survive every path below

	legacy := subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0} // no fg identity
	assertWorktreeUntouched := func(label string) {
		if _, serr := os.Stat(wt); serr != nil {
			t.Errorf("%s: the legacy fg run's workdir must SURVIVE (worktreePurgeable=false), err=%v", label, serr)
		}
		if !worktreeListed(t, repo, wt) {
			t.Errorf("%s: the git registration must SURVIVE", label)
		}
	}

	// Sanity: the adjudicated FgUnknown shape — NOT provably dead.
	if err := subagent.SaveRun(legacy); err != nil {
		t.Fatal(err)
	}
	if r, _ := subagent.ReadRun(id); subagent.RunEngineProvablyNotLive(r) {
		t.Fatal("setup: {stopped,0,no-fg} must NOT be provably dead (FgUnknown)")
	}

	// Each path removes the RECORD (assert it does) but never the workdir/registration.
	if err := subagent.PurgeRun(id); err != nil {
		t.Fatalf("PurgeRun: %v", err)
	}
	if _, rerr := subagent.ReadRun(id); rerr == nil {
		t.Error("PurgeRun removes the legacy fg record (adjudicated behavior)")
	}
	assertWorktreeUntouched("PurgeRun")

	if err := subagent.SaveRun(legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := subagent.PruneRuns(); err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}
	if _, rerr := subagent.ReadRun(id); rerr == nil {
		t.Error("PruneRuns removes the legacy fg record")
	}
	assertWorktreeUntouched("PruneRuns")

	if err := subagent.SaveRun(legacy); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := subagent.PurgeJobs(); err != nil {
		t.Fatalf("PurgeJobs: %v", err)
	}
	if _, rerr := subagent.ReadRun(id); rerr == nil {
		t.Error("PurgeJobs removes the legacy fg record")
	}
	assertWorktreeUntouched("PurgeJobs (incl. its wholesale jobs-dir RemoveAll)")

	// The record is now gone → the segment is UNKNOWN; the sweep must SPARE the present workdir.
	sweepRunWorktrees(repo)
	assertWorktreeUntouched("sweepRunWorktrees (unknown present segment)")
}
