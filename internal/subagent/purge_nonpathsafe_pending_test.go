package subagent

import (
	"os"
	"strings"
	"testing"
)

// TestPurgeRunRefusesNonPathSafePendingMember (codex r29): a NON-path-safe run id ("a.b", segment
// "a-b") skips PurgeRun's path-safe segment verdict, so its whole-run member walk is the ONLY segment
// guard. A crash-window PENDING member (ChildIdentityPending, no ChildPID) is the sole veto for segment
// "a-b"; PurgeRun must REFUSE rather than reap its meta, else a colliding dead path-safe "a-b" reclaimer
// deletes the shared workdir under the possible orphan. The r24 recovery is unchanged: an explicit
// DeleteJob on the pending job proceeds → the retried PurgeRun proceeds → the segment then reclaims.
func TestPurgeRunRefusesNonPathSafePendingMember(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })

	// The shared isolation-worktree segment; a.b is non-path-safe (segment a-b), a-b the path-safe reclaimer.
	wtAB := seedWorktreeTemp(t, "a-b")
	writeRunForTest(t, WorkflowRun{RunID: "a.b", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: deadEnginePID})
	writeRunForTest(t, WorkflowRun{RunID: "a-b", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: deadEnginePID})
	// The crash-window PENDING member of a.b: registered but never past cmd.Start→identity (no ChildPID).
	writeLeafMeta(t, jobMeta{JobID: "pm", RunID: "a.b", PID: deadEnginePID, ChildIdentityPending: true}, false)

	// PurgeRun(a.b) — its member walk (the only guard for a non-path-safe id) refuses on the pending member.
	if err := PurgeRun("a.b"); err == nil || !strings.Contains(err.Error(), "never identified") {
		t.Fatalf("PurgeRun(a.b) must refuse on the pending member, got %v", err)
	}
	if _, rerr := ReadRun("a.b"); rerr != nil {
		t.Error("the refused PurgeRun must leave the a.b manifest")
	}
	if dir, _ := jobsDir(); func() bool { _, merr := readMeta(dir, "pm"); return merr != nil }() {
		t.Error("the refused PurgeRun must leave the pending member meta (the segment veto)")
	}

	// The colliding path-safe a-b reclaimer is vetoed by that same pending member (segment verdict) →
	// PurgeRun(a-b) refuses, the shared workdir survives.
	if err := PurgeRun("a-b"); err == nil || !strings.Contains(err.Error(), "worktree segment") {
		t.Errorf("PurgeRun(a-b) must be vetoed by a.b's pending member, got %v", err)
	}
	if _, serr := os.Stat(wtAB); serr != nil {
		t.Errorf("the shared segment workdir must survive the vetoed reclaim, err=%v", serr)
	}

	// PruneRuns spares BOTH (each PurgeRun refuses) — the automatic path never erases the veto.
	if removed, err := PruneRuns(); err != nil || removed != 0 {
		t.Errorf("PruneRuns must spare both runs (both PurgeRun refuse), removed=%d err=%v", removed, err)
	}
	if _, rerr := ReadRun("a.b"); rerr != nil {
		t.Error("PruneRuns must leave a.b")
	}

	// Recovery: an explicit DeleteJob on the pending job proceeds (record-only escape, narrower rule).
	if derr := DeleteJob("pm"); derr != nil {
		t.Fatalf("DeleteJob on the pending job must proceed (recovery escape), got %v", derr)
	}
	// With the veto resolved, PurgeRun(a.b) proceeds, and the path-safe a-b reclaimer then reclaims the
	// shared workdir (adjudicated post-recovery semantics).
	if err := PurgeRun("a.b"); err != nil {
		t.Errorf("after recovery PurgeRun(a.b) must proceed, got %v", err)
	}
	if err := PurgeRun("a-b"); err != nil {
		t.Errorf("after recovery the path-safe reclaimer must proceed, got %v", err)
	}
	if _, serr := os.Stat(wtAB); !os.IsNotExist(serr) {
		t.Errorf("post-recovery the shared workdir is reclaimed, err=%v", serr)
	}
}

// TestPurgeRunNonPathSafeDeadMembersStillPurge: a NON-path-safe run whose only members are DEAD
// identified leaves (ChildPID>0, dead child) still purges as before — the widened member-walk predicate
// keys on live-or-pending evidence, so a dead identified member never blocks.
func TestPurgeRunNonPathSafeDeadMembersStillPurge(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })

	writeRunForTest(t, WorkflowRun{RunID: "c.d", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: deadEnginePID})
	writeLeafMeta(t, jobMeta{JobID: "dm", RunID: "c.d", PID: deadEnginePID, ChildPID: deadEnginePID}, true) // dead identified child + terminal

	if err := PurgeRun("c.d"); err != nil {
		t.Fatalf("a non-path-safe run with only dead identified members must purge, got %v", err)
	}
	if _, rerr := ReadRun("c.d"); rerr == nil {
		t.Error("the c.d manifest must be gone after PurgeRun")
	}
}
