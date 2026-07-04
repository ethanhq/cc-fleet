package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/pinned"
)

// writeLeafMeta seeds a member job meta, optionally with a cached terminal result (which the
// identity-first predicate must IGNORE for a sync leaf with a recorded child).
func writeLeafMeta(t *testing.T, m jobMeta, terminal bool) {
	t.Helper()
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if m.Status == "" {
		m.Status = "running"
	}
	if m.StartedAt == "" {
		m.StartedAt = "2026-01-01T00:00:00Z"
	}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	if terminal {
		if err := os.WriteFile(filepath.Join(dir, m.JobID+".result.json"), []byte(`{"ok":false,"status":"failed"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLeafProcessMayBeAlive: the per-leaf classifier, IDENTITY-first (it never reads the result cache,
// so a synthesized failVanished can't fool it). meta.PID is the ENGINE for a sync leaf → its child
// pid+token is the authority; a background leaf's meta.PID IS the child; a held-premark orphan is
// decided by its recorded child, not the PID-0 fallback.
func TestLeafProcessMayBeAlive(t *testing.T) {
	self := os.Getpid()
	const dead = 0x7ffffffe
	origPS := procStartFn
	origArgv := hasArgvIntrospection
	procStartFn = func(int) (string, bool) { return "tok", true }
	hasArgvIntrospection = false // background cases: let processAlive degrade to pid+token (no argv marker)
	t.Cleanup(func() { procStartFn = origPS; hasArgvIntrospection = origArgv })

	engine := self // sync meta.PID is the engine
	cases := []struct {
		name string
		meta jobMeta
		want bool
	}{
		{"sync, live child", jobMeta{PID: engine, ChildPID: self, ChildProcStart: "tok"}, true},
		{"held premark, live child (orphan window)", jobMeta{PID: 0, Status: "held", ChildPID: self, ChildProcStart: "tok"}, true},
		{"held premark, dead child", jobMeta{PID: 0, Status: "held", ChildPID: dead}, false},
		{"held premark, identity PENDING (no child)", jobMeta{PID: 0, Status: "held", ChildIdentityPending: true}, true}, // r25: PID 0 says nothing about the started child
		{"held premark, NON-pending legacy (no child)", jobMeta{PID: 0, Status: "held"}, false},                          // legacy behavior unchanged
		{"sync, dead child", jobMeta{PID: engine, ChildPID: dead}, false},
		{"sync, recycled child (alive pid, token mismatch)", jobMeta{PID: engine, ChildPID: self, ChildProcStart: "stale"}, false},
		{"sync, no child identity, execed", jobMeta{PID: engine}, true},
		{"queued leaf (pid 0, no child)", jobMeta{PID: 0}, false},
		{"background, live pid + matching token", jobMeta{PID: self, SettingsPath: "/p", ProcStart: "tok"}, true},
		{"background, dead pid", jobMeta{PID: dead, SettingsPath: "/p", ProcStart: "tok"}, false},
	}
	for _, c := range cases {
		if got := leafProcessMayBeAlive(c.meta); got != c.want {
			t.Errorf("%s: leafProcessMayBeAlive = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSegmentReclaimVerdicts: ownership is SEGMENT-level (ids.WorktreeSegment). A path-safe dead +
// leaf-free owner is a Reclaimer; any owner — path-safe or a colliding non-path-safe twin — that is
// alive/unverifiable or leaf-bearing VETOES; a lone non-path-safe dead owner reclaims nothing.
func TestSegmentReclaimVerdicts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })
	const dead = 0x7ffffffe

	writeRunForTest(t, WorkflowRun{RunID: "solo", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: dead})  // dead path-safe, no leaf
	writeRunForTest(t, WorkflowRun{RunID: "leafy", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: dead}) // dead but a live orphan
	writeLeafMeta(t, jobMeta{JobID: "lj", RunID: "leafy", PID: self, ChildPID: self, ChildProcStart: "tok"}, false)
	writeRunForTest(t, WorkflowRun{RunID: "a-b", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: dead}) // dead path-safe reclaimer of seg a-b
	writeRunForTest(t, WorkflowRun{RunID: "a.b", StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: 0})    // LIVE non-path-safe twin (seg a-b)
	writeRunForTest(t, WorkflowRun{RunID: "x.y", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: dead}) // lone dead non-path-safe (seg x-y)

	v, ok := SegmentReclaimVerdicts()
	if !ok {
		t.Fatal("SegmentReclaimVerdicts scan failed")
	}
	check := func(seg string, wantReclaimer, wantVetoed bool) {
		if got := v[seg]; got.Reclaimer != wantReclaimer || got.Vetoed != wantVetoed {
			t.Errorf("seg %q = %+v, want {Reclaimer:%v Vetoed:%v}", seg, got, wantReclaimer, wantVetoed)
		}
	}
	check("solo", true, false)  // removable
	check("leafy", false, true) // live leaf → vetoed
	check("a-b", true, true)    // dead a-b (reclaimer) + LIVE a.b (veto) → NOT removable (the r13 fix)
	check("x-y", false, false)  // lone non-path-safe dead → no path-safe reclaimer → leak-only
}

// TestRecordChildIdentity: the sync-leaf child recorder stamps ChildPID + ChildProcStart on a running
// meta AND on a HELD one (the hold premark precedes the child kill — an orphan in that window needs the
// identity), preserving Status/PID; and the attempt guard rejects a stale write across a restart.
func TestRecordChildIdentity(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "child-tok", true }
	t.Cleanup(func() { procStartFn = origPS })
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// A running meta (attempt 1) → the child identity is stamped.
	if err := writeMeta(dir, jobMeta{JobID: "j1", PID: os.Getpid(), RunID: "r", Status: "running", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	recordChildIdentity("j1", 4242, 1)
	if m, _ := readMeta(dir, "j1"); m.ChildPID != 4242 || m.ChildProcStart != "child-tok" {
		t.Errorf("recordChildIdentity must set ChildPID/ChildProcStart on a running meta, got %d/%q", m.ChildPID, m.ChildProcStart)
	}

	// A HELD meta (the premark window) → the identity IS written, preserving the held status + PID 0.
	if err := writeMeta(dir, jobMeta{JobID: "j2", PID: 0, RunID: "r", Status: "held", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	recordChildIdentity("j2", 4242, 1)
	if m, _ := readMeta(dir, "j2"); m.ChildPID != 4242 || m.Status != "held" || m.PID != 0 {
		t.Errorf("recordChildIdentity must stamp a held meta (identity protects the orphan) while preserving Status/PID, got ChildPID=%d Status=%q PID=%d", m.ChildPID, m.Status, m.PID)
	}

	// A stale attempt: the meta is now attempt 2 (restarted) → an attempt-1 write is ignored.
	if err := writeMeta(dir, jobMeta{JobID: "j3", PID: os.Getpid(), RunID: "r", Status: "running", Attempt: 2}); err != nil {
		t.Fatal(err)
	}
	recordChildIdentity("j3", 4242, 1)
	if m, _ := readMeta(dir, "j3"); m.ChildPID != 0 {
		t.Error("recordChildIdentity must ignore a stale attempt-1 write on an attempt-2 (restarted) meta")
	}
}

// TestPurgeRunRefusesUnderLiveLeaf: PurgeRun REFUSES (never half-deletes) while a member leaf's child
// is alive — the record, member jobs, AND workdir all survive so the ownership + identity evidence
// stays intact — with a retryable error; once the child dies a retried purge fully removes everything.
// A held-premark orphan is refused the same way. No live leaf → it purges.
func TestPurgeRunRefusesUnderLiveLeaf(t *testing.T) {
	self := os.Getpid()
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })

	assertIntact := func(t *testing.T, runID, jobID, wtDir string) {
		t.Helper()
		if _, rerr := ReadRun(runID); rerr != nil {
			t.Error("the run record must survive a refused purge")
		}
		if dir, derr := jobsDir(); derr == nil {
			if _, merr := readMeta(dir, jobID); merr != nil {
				t.Error("the member job meta must survive a refused purge")
			}
		}
		if _, err := os.Stat(wtDir); err != nil {
			t.Errorf("the workdir must survive a refused purge (err=%v)", err)
		}
	}

	t.Run("live child → refused, nothing deleted; retried after death → fully purged", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		const runID = "purge-live-leaf"
		wtDir := seedWorktreeTemp(t, runID)
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe})
		writeLeafMeta(t, jobMeta{JobID: "leaf1", RunID: runID, PID: self, ChildPID: self, ChildProcStart: "tok"}, true) // live child + synthesized terminal

		if err := PurgeRun(runID); err == nil || !strings.Contains(err.Error(), "worktree segment") {
			t.Errorf("PurgeRun under a live leaf must be refused with a retryable error, got %v", err)
		}
		assertIntact(t, runID, "leaf1", wtDir)

		// The orphan exits → a retried purge fully removes record + workdir.
		writeLeafMeta(t, jobMeta{JobID: "leaf1", RunID: runID, PID: self, ChildPID: 0x7ffffffe}, false) // dead child
		if err := PurgeRun(runID); err != nil {
			t.Fatalf("retried PurgeRun after the child died: %v", err)
		}
		if _, rerr := ReadRun(runID); rerr == nil {
			t.Error("the record must be gone after the retried purge")
		}
		if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
			t.Errorf("the workdir must be gone after the retried purge (err=%v)", err)
		}
	})

	t.Run("held-premark orphan (live child) → refused, nothing deleted", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		const runID = "purge-held-leaf"
		wtDir := seedWorktreeTemp(t, runID)
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe})
		writeLeafMeta(t, jobMeta{JobID: "leaf1", RunID: runID, PID: 0, Status: "held", ChildPID: self, ChildProcStart: "tok"}, false) // {held, PID 0, live child}

		if err := PurgeRun(runID); err == nil || !strings.Contains(err.Error(), "worktree segment") {
			t.Errorf("PurgeRun under a held-premark orphan must be refused, got %v", err)
		}
		assertIntact(t, runID, "leaf1", wtDir)
	})

	t.Run("no live leaf → purges", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		const runID = "purge-dead-leaf"
		wtDir := seedWorktreeTemp(t, runID)
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe})
		writeLeafMeta(t, jobMeta{JobID: "leaf1", RunID: runID, PID: self, ChildPID: 0x7ffffffe}, false) // dead child

		if err := PurgeRun(runID); err != nil {
			t.Fatalf("PurgeRun: %v", err)
		}
		if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
			t.Errorf("PurgeRun must remove the worktree temp when no leaf is alive (err=%v)", err)
		}
	})
}

// TestPruneRunsContinuesPastLiveLeaf: PruneRuns tolerates a per-run PurgeRun refusal — a run with a
// live orphan leaf is spared (PurgeRun refuses) while a plain dead run is reaped; the sweep never aborts.
func TestPruneRunsContinuesPastLiveLeaf(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })

	seedWorktreeTemp(t, "live-leaf-run") // an isolation worktree the orphan could be using → refusal applies
	writeRunForTest(t, WorkflowRun{RunID: "live-leaf-run", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe})
	writeLeafMeta(t, jobMeta{JobID: "leaf1", RunID: "live-leaf-run", PID: self, ChildPID: self, ChildProcStart: "tok"}, false) // live orphan
	writeRunForTest(t, WorkflowRun{RunID: "plain-dead-run", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe})

	removed, err := PruneRuns()
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}
	if removed != 1 {
		t.Errorf("PruneRuns removed = %d, want 1 (plain dead reaped; live-leaf run spared by refusal)", removed)
	}
	if _, rerr := ReadRun("live-leaf-run"); rerr != nil {
		t.Error("PruneRuns must spare a run with a live orphan leaf (PurgeRun refused, loop continued)")
	}
	if _, rerr := ReadRun("plain-dead-run"); rerr == nil {
		t.Error("PruneRuns must still reap a plain dead run")
	}
}

// TestPurgeRunRefusesNonPathSafeLiveMember (codex r18): PurgeRun("a.b") is a valid NON-path-safe id
// that bypasses the path-safe segment gate, so it must still refuse — id-shape-independent — on its OWN
// live child-identified member, else it deletes the veto for segment a-b and a dead path-safe a-b
// reclaimer deletes the live child's workdir. Kill the child → PurgeRun("a.b") succeeds + reclaimable.
func TestPurgeRunRefusesNonPathSafeLiveMember(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })

	writeRunForTest(t, WorkflowRun{RunID: "a.b", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}) // non-path-safe own record
	writeRunForTest(t, WorkflowRun{RunID: "a-b", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}) // dead path-safe reclaimer (seg a-b)
	writeLeafMeta(t, jobMeta{JobID: "ablj", RunID: "a.b", PID: self, ChildPID: self, ChildProcStart: "tok"}, false)            // LIVE a.b member

	if err := PurgeRun("a.b"); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Errorf("PurgeRun(a.b) must refuse while its member leaf is live, got %v", err)
	}
	dir, _ := jobsDir()
	if _, merr := readMeta(dir, "ablj"); merr != nil {
		t.Error("the live member meta must survive the refused purge (veto evidence)")
	}
	if _, rerr := ReadRun("a.b"); rerr != nil {
		t.Error("the a.b record must survive the refused purge")
	}
	if v, ok := SegmentReclaimVerdicts(); !ok || !v["a-b"].Vetoed {
		t.Errorf("segment a-b must still be vetoed by the live a.b member, got %+v ok=%v", v["a-b"], ok)
	}

	// The child dies → the segment is reclaimable and PurgeRun("a.b") succeeds.
	writeLeafMeta(t, jobMeta{JobID: "ablj", RunID: "a.b", PID: self, ChildPID: 0x7ffffffe}, false)
	if v, ok := SegmentReclaimVerdicts(); !ok || v["a-b"].Vetoed {
		t.Errorf("once the a.b member is dead, segment a-b must not be vetoed, got %+v ok=%v", v["a-b"], ok)
	}
	if err := PurgeRun("a.b"); err != nil {
		t.Fatalf("PurgeRun(a.b) after the child died: %v", err)
	}
	if _, rerr := ReadRun("a.b"); rerr == nil {
		t.Error("PurgeRun(a.b) must remove the record once the member is dead")
	}
}

// TestPurgeRunRefusesUnderCollidingSegment (the codex r13 scenario): a dead PATH-SAFE "a-b" shares
// segment "a-b" with a LIVE non-path-safe twin "a.b". PurgeRun("a-b") must NOT RemoveAll the shared
// segment (where a.b's workdirs live) — it refuses, consistent with the r12 refusal shape — until the
// colliding owner exits, then it purges.
func TestPurgeRunRefusesUnderCollidingSegment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	wtDir := seedWorktreeTemp(t, "a-b")                                                                                        // the shared segment dir a-b's RemoveAll would delete
	writeRunForTest(t, WorkflowRun{RunID: "a-b", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}) // dead path-safe
	writeRunForTest(t, WorkflowRun{RunID: "a.b", StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: 0})          // LIVE colliding twin

	if err := PurgeRun("a-b"); err == nil || !strings.Contains(err.Error(), "worktree segment") {
		t.Errorf("PurgeRun(a-b) must be refused while the colliding a.b is live, got %v", err)
	}
	if _, err := os.Stat(wtDir); err != nil {
		t.Errorf("the shared segment must survive (a.b's live workdirs), err=%v", err)
	}

	// The colliding twin exits → the segment becomes reclaimable → a-b purges.
	writeRunForTest(t, WorkflowRun{RunID: "a.b", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe})
	if err := PurgeRun("a-b"); err != nil {
		t.Fatalf("PurgeRun(a-b) after a.b died: %v", err)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("the segment must be removed once no colliding owner is live, err=%v", err)
	}
}

// TestCorruptMemberMetaFailsClosed (codex r15): an unreadable/partial member meta makes the run's
// leaf state unknowable, so runLeafScan fails CLOSED — SegmentReclaimVerdicts returns !ok, PurgeRun
// refuses, and the worktree survives (never deleted under a possibly-live orphan whose meta was torn).
func TestCorruptMemberMetaFailsClosed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })

	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRunForTest(t, WorkflowRun{RunID: "corrupt-run", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe})
	writeLeafMeta(t, jobMeta{JobID: "good", RunID: "corrupt-run", PID: self, ChildPID: self, ChildProcStart: "tok"}, false) // a valid live-child member
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{"job_id":"bad","run_id":`), 0o600); err != nil {        // truncated
		t.Fatal(err)
	}

	if _, ok := SegmentReclaimVerdicts(); ok {
		t.Error("a corrupt member meta must make SegmentReclaimVerdicts fail closed (ok=false)")
	}
	wtDir := seedWorktreeTemp(t, "corrupt-run")
	if err := PurgeRun("corrupt-run"); err == nil || !strings.Contains(err.Error(), "worktree segment") {
		t.Errorf("PurgeRun must refuse when the leaf scan can't be trusted, got %v", err)
	}
	if _, err := os.Stat(wtDir); err != nil {
		t.Errorf("the worktree must survive the refused purge, err=%v", err)
	}
}

// TestWriteMetaAtomicNoTornReads (codex r15): the meta is load-bearing death evidence, so writeMeta
// must be atomic (temp+rename) — a concurrent reader never sees a partial file. A large payload widens
// the write window so a NON-atomic (plain truncate+write) meta would surface a torn (parse-error) read.
func TestWriteMetaAtomicNoTornReads(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	base := jobMeta{JobID: "big", RunID: "r", Status: "running", Label: strings.Repeat("x", 64<<10)}
	if err := writeMeta(dir, base); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = writeMeta(dir, base)
		}
	}()
	var tornErr error
	for i := 0; i < 600 && tornErr == nil; i++ {
		if _, rerr := readMeta(dir, "big"); rerr != nil {
			tornErr = rerr
		}
	}
	close(stop)
	wg.Wait()
	if tornErr != nil {
		t.Errorf("readMeta saw a TORN write — writeMeta is not atomic: %v", tornErr)
	}
}

// TestPruneRunsSparesLiveDetached (codex r20 correction): PruneRuns is BULK cleanup and never
// auto-stops — a verifiably-live detached run (DetachedLive) is SPARED (isLiveOrUnverifiable's tri-state
// arm, behavior-preserving vs the old EngineAlive arm), while a plainly-dead one is reaped. Uses the
// test's own pid — safe because a spared run is never PurgeRun'd, so nothing is reaped.
func TestPruneRunsSparesLiveDetached(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origArgv := reuseGuardArgv
	reuseGuardArgv = func(int) ([]string, bool) {
		return []string{"cc-fleet", "workflow", "run", "--run-id", "live", "s.js"}, true // matches → DetachedLive
	}
	t.Cleanup(func() { reuseGuardArgv = origArgv })

	if got := ClassifyDetachedEngine(WorkflowRun{RunID: "live", EnginePID: self}); got != DetachedLive {
		t.Fatalf("setup: expected DetachedLive, got %v", got)
	}
	writeRunForTest(t, WorkflowRun{RunID: "live", StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: self})
	writeRunForTest(t, WorkflowRun{RunID: "gone", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe})

	removed, err := PruneRuns()
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}
	if removed != 1 {
		t.Errorf("PruneRuns removed=%d, want 1 (dead reaped; live spared)", removed)
	}
	if _, rerr := ReadRun("live"); rerr != nil {
		t.Error("PruneRuns must spare a verifiably-live detached run (never auto-stops)")
	}
	if _, rerr := ReadRun("gone"); rerr == nil {
		t.Error("PruneRuns must reap a plainly-dead detached run")
	}
}

// TestGCReapsRunThroughPurgeRunNoStrand (codex r20): GC's aged-run removal routes through PurgeRun, so
// a dead run + a dead-child member + a leaked workdir (all TTL-expired) is reaped with the workdir
// removed WITH the manifest (PurgeRun's ordered, physical-snapshot cleanup) — NO unknown-present strand.
// The job-meta pass reaping the dead member BEFORE the run's PurgeRun is immaterial (PurgeRun removes
// the segment by physical snapshot, not by member metas), and the standalone-jobs pass is untouched.
func TestGCReapsRunThroughPurgeRunNoStrand(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const old = "2000-01-01T00:00:00Z"
	const runID = "gcrun"
	segDir := filepath.Join(os.TempDir(), "cc-fleet-worktrees", runID)
	t.Cleanup(func() { _ = os.RemoveAll(segDir) })
	if err := os.MkdirAll(filepath.Join(segDir, "wt"), 0o700); err != nil { // the leaked workdir
		t.Fatal(err)
	}

	writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: old, Status: "stopped", EnginePID: 0x7ffffffe}) // provably dead, aged
	if err := writeMeta(dir, jobMeta{JobID: "m", RunID: runID, PID: os.Getpid(), ChildPID: 0x7ffffffe, Status: "running", StartedAt: old}); err != nil {
		t.Fatal(err) // dead-child member
	}
	if err := os.WriteFile(filepath.Join(dir, "m.result.json"), []byte(`{"ok":false}`), 0o600); err != nil {
		t.Fatal(err) // terminal cache → the job-meta pass reaps it (dead child, aged)
	}
	// A LIVE standalone job (RunID "") — the standalone-jobs pass is untouched, so it survives.
	if err := writeMeta(dir, jobMeta{JobID: "solo", RunID: "", PID: os.Getpid(), Status: "running", StartedAt: old}); err != nil {
		t.Fatal(err)
	}

	if res := GC(time.Hour); !res.OK {
		t.Fatalf("GC: %+v", res)
	}
	if _, serr := os.Stat(segDir); !os.IsNotExist(serr) {
		t.Errorf("GC must remove the leaked workdir WITH the manifest (no unknown-present strand), stat err=%v", serr)
	}
	if _, rerr := ReadRun(runID); rerr == nil {
		t.Error("GC must remove the aged dead run's manifest")
	}
	if _, merr := readMeta(dir, "solo"); merr != nil {
		t.Error("GC's standalone-jobs pass must be untouched — a live standalone job survives")
	}
}

// TestPurgeJobsReapsRunThroughPurgeRunNoStrand (codex r21): PurgeJobs's aged-run removal (the
// uninstall/purge sweep) routes through PurgeRun, so a dead run + a dead-child member + a leaked workdir
// is reaped with the workdir removed WITH the manifest (no unknown-present strand), while a run with a
// LIVE-orphan member is kept intact (member pass keeps it → runningRuns → manifest kept).
func TestPurgeJobsReapsRunThroughPurgeRunNoStrand(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })

	// Run A: dead + dead-child member (terminal cache) + a leaked workdir → reaped, workdir gone with it.
	segA := filepath.Join(os.TempDir(), "cc-fleet-worktrees", "runA")
	t.Cleanup(func() { _ = os.RemoveAll(segA) })
	if err := os.MkdirAll(filepath.Join(segA, "wt"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRunForTest(t, WorkflowRun{RunID: "runA", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe})
	writeLeafMeta(t, jobMeta{JobID: "ma", RunID: "runA", PID: self, ChildPID: 0x7ffffffe}, true) // dead child + terminal cache

	// Run B: a LIVE-orphan member → kept intact.
	segB := filepath.Join(os.TempDir(), "cc-fleet-worktrees", "runB")
	t.Cleanup(func() { _ = os.RemoveAll(segB) })
	if err := os.MkdirAll(filepath.Join(segB, "wt"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRunForTest(t, WorkflowRun{RunID: "runB", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe})
	writeLeafMeta(t, jobMeta{JobID: "mb", RunID: "runB", PID: self, ChildPID: self, ChildProcStart: "tok"}, false) // LIVE child

	if _, _, _, err := PurgeJobs(); err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(segA); !os.IsNotExist(serr) {
		t.Errorf("runA's leaked workdir must be removed WITH the record (no strand), stat err=%v", serr)
	}
	if _, rerr := ReadRun("runA"); rerr == nil {
		t.Error("runA's manifest must be removed")
	}
	if _, rerr := ReadRun("runB"); rerr != nil {
		t.Error("runB (live-orphan) must be kept intact")
	}
	if _, serr := os.Stat(segB); serr != nil {
		t.Errorf("runB's workdir must survive (live orphan), stat err=%v", serr)
	}
}

// TestPurgeJobsKeepsDirWhenRunRefused (codex r22): PurgeJobs's final wholesale RemoveAll must not
// override a PurgeRun refusal. A LIVE foreground run with NO member job (so `running` alone is empty)
// is refused by PurgeRun → its manifest SURVIVES and the jobs dir is KEPT. Flip it dead → the refusal
// clears and the full cleanup (incl. the dir removal) proceeds.
func TestPurgeJobsKeepsDirWhenRunRefused(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "fg-tok", true }
	t.Cleanup(func() { procStartFn = origPS })
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}

	// A blind-stopped LIVE foreground run ({stopped, 0, FgAlive}), NO member jobs → PurgeRun refuses it.
	if err := SaveRun(WorkflowRun{RunID: "fg-live", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0, FgEnginePID: self, FgEngineProcStart: "fg-tok"}); err != nil {
		t.Fatal(err)
	}
	if _, _, running, perr := PurgeJobs(); perr != nil {
		t.Fatal(perr)
	} else if len(running) != 0 {
		t.Errorf("the bug precondition: with no member JOB, running is empty, got %v", running)
	}
	if _, rerr := ReadRun("fg-live"); rerr != nil {
		t.Error("a refused live-fg run's manifest must SURVIVE PurgeJobs")
	}
	if _, serr := os.Stat(dir); os.IsNotExist(serr) {
		t.Error("PurgeJobs must KEEP the jobs dir when a run was refused (no wholesale RemoveAll)")
	}

	// Flip the fg identity DEAD (→ FgDead → provably dead) → the refusal clears, full cleanup proceeds.
	if err := SaveRun(WorkflowRun{RunID: "fg-live", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0, FgEnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, perr := PurgeJobs(); perr != nil {
		t.Fatal(perr)
	}
	if _, rerr := ReadRun("fg-live"); rerr == nil {
		t.Error("a now-dead run must be purged")
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Error("with nothing surviving, PurgeJobs must remove the jobs dir (full cleanup)")
	}
}

// TestUnverifiableDetachedEngineSparesSegment (codex r19): an alive-but-UNVERIFIABLE detached engine
// (recorded token, but neither argv nor its start token readable — EPERM/hardened) must NOT read as
// provably dead, so its worktree segment is spared (Vetoed, not Reclaimer). Once the token becomes
// readable + MISMATCHED (recycled), it reads dead and the segment becomes reclaimable. Covers both the
// detached-manifest path (RunEngineProvablyNotLive) and the verdict map (SegmentReclaimVerdicts).
func TestUnverifiableDetachedEngineSparesSegment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origArgv := reuseGuardArgv
	origPS := procStartFn
	reuseGuardArgv = func(int) ([]string, bool) { return nil, false } // argv unreadable
	procStartFn = func(int) (string, bool) { return "", false }       // token unreadable
	t.Cleanup(func() { reuseGuardArgv = origArgv; procStartFn = origPS })

	run := WorkflowRun{RunID: "det", StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: self, EngineProcStart: "tok"}
	writeRunForTest(t, run)

	if RunEngineProvablyNotLive(run) {
		t.Error("an unverifiable-alive detached engine must NOT be provably dead (was reclaiming under a live engine)")
	}
	if v, ok := SegmentReclaimVerdicts(); !ok || !v["det"].Vetoed || v["det"].Reclaimer {
		t.Errorf("an unverifiable detached run must VETO its segment (never Reclaimer), got %+v ok=%v", v["det"], ok)
	}

	// The token becomes readable + MISMATCHED → recycled → provably dead → reclaimable.
	procStartFn = func(int) (string, bool) { return "different", true }
	if !RunEngineProvablyNotLive(run) {
		t.Error("a recycled detached pid (readable token mismatch) must be provably dead")
	}
	if v, ok := SegmentReclaimVerdicts(); !ok || v["det"].Vetoed || !v["det"].Reclaimer {
		t.Errorf("once recycled-dead, segment det must be reclaimable (Reclaimer, not Vetoed), got %+v ok=%v", v["det"], ok)
	}
}

// TestUnverifiableDetachedRefusedByPurgeAndPrune (codex r20): a blind-stop collapse can leave
// {stopped, retained pid alive, token} — an UNVERIFIABLE detached engine (DetachedUnknown). Because it
// is not PROVABLY dead, PurgeRun refuses it (mirroring FgAlive), PruneRuns spares it, and — since the
// same DetachedUnknown is what resumeBlockedReason refuses on (workflow) — resume is refused too. Once
// the pid recycles (token readable + mismatch → DetachedDead) everything proceeds.
func TestUnverifiableDetachedRefusedByPurgeAndPrune(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origArgv := reuseGuardArgv
	origPS := procStartFn
	reuseGuardArgv = func(int) ([]string, bool) { return nil, false } // argv unreadable
	procStartFn = func(int) (string, bool) { return "", false }       // token unreadable
	t.Cleanup(func() { reuseGuardArgv = origArgv; procStartFn = origPS })

	run := WorkflowRun{RunID: "det", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: self, EngineProcStart: "tok"}
	// The shape resumeBlockedReason (workflow) refuses on: an alive-but-unverifiable detached pid.
	if got := ClassifyDetachedEngine(run); got != DetachedUnknown {
		t.Fatalf("setup: expected DetachedUnknown (the resume-refused shape), got %v", got)
	}
	writeRunForTest(t, run)

	// PruneRuns' spare decision (its outcome is doubly protected by PurgeRun's refusal, so assert it directly too).
	if !isLiveOrUnverifiable(run) {
		t.Error("isLiveOrUnverifiable must spare an unverifiable detached run")
	}
	if err := PurgeRun("det"); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Errorf("PurgeRun must refuse an unverifiable detached run, got %v", err)
	}
	if _, rerr := ReadRun("det"); rerr != nil {
		t.Error("the refused run must survive PurgeRun")
	}
	if removed, err := PruneRuns(); err != nil {
		t.Fatalf("PruneRuns: %v", err)
	} else if removed != 0 {
		t.Errorf("PruneRuns must spare an unverifiable detached run, removed=%d", removed)
	}
	if _, rerr := ReadRun("det"); rerr != nil {
		t.Error("PruneRuns must spare (not delete) the unverifiable detached run")
	}

	// The pid recycles: token readable + MISMATCHED → DetachedDead → prune reaps it (via PurgeRun).
	procStartFn = func(int) (string, bool) { return "different", true }
	if removed, err := PruneRuns(); err != nil {
		t.Fatalf("PruneRuns after recycle: %v", err)
	} else if removed != 1 {
		t.Errorf("PruneRuns must reap the run once its pid reads DetachedDead, removed=%d", removed)
	}
	if _, rerr := ReadRun("det"); rerr == nil {
		t.Error("the recycled-dead run must be gone after PruneRuns")
	}
}

// TestPendingIdentityKeptByAutomatedCleanup (codex r24): a sync member still in the
// cmd.Start→recordChildIdentity window (ChildIdentityPending, no ChildPID) is a possibly-live orphan the
// read-side fail-safe already vetoes — so AUTOMATED cleanup (GC/PurgeJobs) must KEEP it (else it reaps
// the veto and the chokepoint deletes the workdir). A NORMAL dead-child member (pending cleared) is
// still reaped (r17 contract). The EXPLICIT DeleteJob on the pending job PROCEEDS (record-only recovery).
func TestPendingIdentityKeptByAutomatedCleanup(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const old = "2000-01-01T00:00:00Z"
	seed := func(jobID, runID string, childPID int, pending bool) {
		if err := writeMeta(dir, jobMeta{JobID: jobID, RunID: runID, PID: self, ChildPID: childPID, ChildIdentityPending: pending, Status: "running", StartedAt: old}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, jobID+".result.json"), []byte(`{"ok":false}`), 0o600); err != nil {
			t.Fatal(err) // synthesized terminal — must NOT let a pending member be reaped
		}
	}

	// GC: a PENDING member survives; a NORMAL dead-child member is reaped (r17 unchanged).
	seed("pend", "a-b", 0, true)
	seed("norm", "c-d", 0x7ffffffe, false)
	if res := GC(time.Hour); !res.OK {
		t.Fatalf("GC: %+v", res)
	}
	if _, merr := readMeta(dir, "pend"); merr != nil {
		t.Error("GC must PRESERVE a pending-identity member (Start-window veto evidence)")
	}
	if _, merr := readMeta(dir, "norm"); merr == nil {
		t.Error("GC must still reap a normal dead-child member (r17 contract unchanged)")
	}

	// Point 3: the pending member vetoes its segment (read-side fail-safe → projection).
	if v, ok := SegmentReclaimVerdicts(); !ok || !v["a-b"].Vetoed {
		t.Errorf("a pending member must VETO its segment (read-side), got %+v ok=%v", v["a-b"], ok)
	}

	// PurgeJobs: the pending member is kept + reported running (which keeps its run's manifest too).
	seed("pend2", "e-f", 0, true)
	_, _, running, perr := PurgeJobs()
	if perr != nil {
		t.Fatal(perr)
	}
	if _, merr := readMeta(dir, "pend2"); merr != nil {
		t.Error("PurgeJobs must PRESERVE a pending-identity member")
	}
	pend2Running := false
	for _, r := range running {
		if r == "pend2" {
			pend2Running = true
		}
	}
	if !pend2Running {
		t.Errorf("PurgeJobs must report the pending member as running, got %v", running)
	}

	// Point 4: an EXPLICIT DeleteJob on the pending job PROCEEDS (record-only recovery escape).
	if err := DeleteJob("pend2"); err != nil {
		t.Errorf("DeleteJob on a PENDING job must proceed (recovery escape), got %v", err)
	}
	if _, merr := readMeta(dir, "pend2"); merr == nil {
		t.Error("DeleteJob must remove the pending member's meta")
	}
}

// TestGCPreservesLiveOrphanVetoEvidence (codex r17): the recency GC must NOT delete a live orphan's
// member meta — even TTL-expired, with a SYNTHESIZED terminal cache and a dead meta.PID (engine proxy)
// — because it is the veto evidence its worktree segment relies on. Once the child reads dead, GC reaps
// it and the segment becomes reclaimable.
func TestGCPreservesLiveOrphanVetoEvidence(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const old = "2000-01-01T00:00:00Z" // far past the TTL
	if err := writeMeta(dir, jobMeta{JobID: "orphan", RunID: "a.b", PID: self, ChildPID: self, ChildProcStart: "tok", Status: "running", StartedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orphan.result.json"), []byte(`{"ok":false,"status":"failed"}`), 0o600); err != nil { // synthesized terminal
		t.Fatal(err)
	}

	if res := GC(time.Hour); !res.OK {
		t.Fatalf("GC: %+v", res)
	}
	if _, rerr := readMeta(dir, "orphan"); rerr != nil {
		t.Error("GC must PRESERVE a live-orphan member meta despite TTL + synthesized terminal (veto evidence)")
	}
	if v, ok := SegmentReclaimVerdicts(); !ok || !v["a-b"].Vetoed {
		t.Errorf("the preserved live-orphan meta must still veto segment a-b, got %+v ok=%v", v["a-b"], ok)
	}

	// The child dies → GC reaps the (now dead) member on the next pass.
	if err := writeMeta(dir, jobMeta{JobID: "orphan", RunID: "a.b", PID: self, ChildPID: 0x7ffffffe, Status: "running", StartedAt: old}); err != nil {
		t.Fatal(err)
	}
	if res := GC(time.Hour); !res.OK {
		t.Fatalf("GC 2: %+v", res)
	}
	if _, rerr := readMeta(dir, "orphan"); rerr == nil {
		t.Error("once the child is dead, GC must reap the member meta (TTL-expired + cached terminal)")
	}
}

// TestPurgeJobsPreservesLiveOrphan (codex r17 audit): PurgeJobs keeps a live-orphan member (reported
// running) despite a synthesized terminal cache — the same veto-evidence rule as GC.
func TestPurgeJobsPreservesLiveOrphan(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(dir, jobMeta{JobID: "orphan", RunID: "a.b", PID: self, ChildPID: self, ChildProcStart: "tok", Status: "running", StartedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orphan.result.json"), []byte(`{"ok":false}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, running, perr := PurgeJobs()
	if perr != nil {
		t.Fatal(perr)
	}
	if _, rerr := readMeta(dir, "orphan"); rerr != nil {
		t.Error("PurgeJobs must PRESERVE a live-orphan member meta")
	}
	found := false
	for _, id := range running {
		if id == "orphan" {
			found = true
		}
	}
	if !found {
		t.Errorf("PurgeJobs must report the preserved live orphan as running, got %v", running)
	}
}

// TestDeleteJobRefusesLiveOrphan (codex r17 audit): the board's per-record delete refuses a live-orphan
// member (retryable), mirroring PurgeRun; once the child dies it deletes.
func TestDeleteJobRefusesLiveOrphan(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(dir, jobMeta{JobID: "orphan", RunID: "a.b", PID: self, ChildPID: self, ChildProcStart: "tok", Status: "running", StartedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}

	if err := DeleteJob("orphan"); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Errorf("DeleteJob must refuse a live-orphan member, got %v", err)
	}
	if _, rerr := readMeta(dir, "orphan"); rerr != nil {
		t.Error("the refused member meta must survive")
	}
	if err := writeMeta(dir, jobMeta{JobID: "orphan", RunID: "a.b", PID: self, ChildPID: 0x7ffffffe, Status: "running", StartedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := DeleteJob("orphan"); err != nil {
		t.Fatalf("DeleteJob after the child died: %v", err)
	}
}

// TestCorruptManifestLiveLeafVetoes (codex r16): ListRuns silently skips a corrupt run manifest, but a
// live member leaf's veto stands on its own (its RunID comes from the job meta, not the manifest). So a
// live colliding owner (a.b whose runs/a.b.json is truncated) still vetoes segment a-b via the leaf
// projection — a dead path-safe a-b can't reclaim under it. Kill the member → reclaimable again.
func TestCorruptManifestLiveLeafVetoes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })

	writeRunForTest(t, WorkflowRun{RunID: "a-b", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}) // dead path-safe reclaimer, valid manifest
	writeLeafMeta(t, jobMeta{JobID: "ablj", RunID: "a.b", PID: self, ChildPID: self, ChildProcStart: "tok"}, false)            // LIVE a.b member
	rd, err := runsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rd, "a.b.json"), []byte(`{"run_id":"a.b","stat`), 0o600); err != nil { // truncated manifest
		t.Fatal(err)
	}

	v, ok := SegmentReclaimVerdicts()
	if !ok {
		t.Fatal("scan should succeed — the corrupt file is a MANIFEST, not a member meta")
	}
	if !v["a-b"].Vetoed {
		t.Error("a live colliding owner with a CORRUPT manifest must veto segment a-b via the leaf projection")
	}
	wtDir := seedWorktreeTemp(t, "a-b")
	if err := PurgeRun("a-b"); err == nil || !strings.Contains(err.Error(), "worktree segment") {
		t.Errorf("PurgeRun(a-b) must refuse while the corrupt-manifest a.b is live, got %v", err)
	}
	if _, err := os.Stat(wtDir); err != nil {
		t.Errorf("the shared segment must survive the refused purge, err=%v", err)
	}

	// The a.b member dies → the segment is reclaimable again.
	writeLeafMeta(t, jobMeta{JobID: "ablj", RunID: "a.b", PID: self, ChildPID: 0x7ffffffe}, false)
	if v2, ok2 := SegmentReclaimVerdicts(); !ok2 || v2["a-b"].Vetoed {
		t.Errorf("once a.b's member is dead, segment a-b must not be vetoed, got %+v ok=%v", v2["a-b"], ok2)
	}
	if err := PurgeRun("a-b"); err != nil {
		t.Fatalf("PurgeRun(a-b) after the a.b member died: %v", err)
	}
}

// TestPurgeRunSnapshotScopedRemoval (codex r14 TOCTOU): PurgeRun removes exactly the workdirs its
// VERDICT-TIME segment snapshot listed, and os.Remove's the segment dir only if now empty — so a
// colliding owner's post-snapshot fresh-uuid workdir (unlisted, via the segReadDir seam) SURVIVES,
// while the snapshot-listed stale one is removed with the record.
func TestPurgeRunSnapshotScopedRemoval(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	const runID = "snap-run"
	segDir := filepath.Join(os.TempDir(), "cc-fleet-worktrees", runID)
	t.Cleanup(func() { _ = os.RemoveAll(segDir) })
	staleWt := filepath.Join(segDir, "stale-uuid")
	freshWt := filepath.Join(segDir, "fresh-uuid") // simulates a colliding owner's POST-snapshot creation
	for _, d := range []string{staleWt, freshWt} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe})

	orig := segReadDir
	segReadDir = func(d string) ([]os.DirEntry, error) { // the snapshot omits fresh-uuid ("appeared after")
		es, err := os.ReadDir(d)
		if err != nil {
			return nil, err
		}
		kept := es[:0]
		for _, e := range es {
			if e.Name() != "fresh-uuid" {
				kept = append(kept, e)
			}
		}
		return kept, nil
	}
	t.Cleanup(func() { segReadDir = orig })

	if err := PurgeRun(runID); err != nil {
		t.Fatalf("PurgeRun: %v", err)
	}
	if _, err := os.Stat(staleWt); !os.IsNotExist(err) {
		t.Error("the snapshot-listed stale workdir must be removed")
	}
	if _, err := os.Stat(freshWt); err != nil {
		t.Errorf("a post-snapshot fresh workdir (unlisted) must SURVIVE, err=%v", err)
	}
	if _, err := os.Stat(segDir); err != nil {
		t.Errorf("the segment dir must survive (not empty) — os.Remove is empty-only, err=%v", err)
	}
	if _, rerr := ReadRun(runID); rerr == nil {
		t.Error("the record must still be deleted")
	}
}

// TestClearFinishedThroughPurgeRun (codex r14): ClearFinished routes run deletion through
// WithRunLock + PurgeRun, so a blind-stopped FgAlive run is REFUSED (skip-and-continue, survives) and
// a terminal provably-dead run with a leaked worktree is cleared with its workdir removed WITH the
// record — the unknown-present strand can no longer happen through this door.
func TestClearFinishedThroughPurgeRun(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })
	const sess = "sess-1"

	writeRunForTest(t, WorkflowRun{RunID: "fg-live", SessionID: sess, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0, FgEnginePID: self, FgEngineProcStart: "tok"}) // blind-stopped FgAlive
	segDir := filepath.Join(os.TempDir(), "cc-fleet-worktrees", "term-run")
	t.Cleanup(func() { _ = os.RemoveAll(segDir) })
	if err := os.MkdirAll(filepath.Join(segDir, "wt"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRunForTest(t, WorkflowRun{RunID: "term-run", SessionID: sess, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}) // terminal + provably dead + leaked worktree

	removed, err := ClearFinished(sess, pinned.Set{})
	if err != nil {
		t.Fatalf("ClearFinished: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (term-run cleared; fg-live refused)", removed)
	}
	if _, rerr := ReadRun("fg-live"); rerr != nil {
		t.Error("a blind-stopped FgAlive run must survive ClearFinished (PurgeRun refused, loop continued)")
	}
	if _, rerr := ReadRun("term-run"); rerr == nil {
		t.Error("a terminal provably-dead run must be cleared")
	}
	if _, err := os.Stat(segDir); !os.IsNotExist(err) {
		t.Errorf("the cleared run's leaked worktree must be removed WITH the record (no strand), err=%v", err)
	}
}
