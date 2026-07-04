package subagent

import (
	"os"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/pinned"
)

// TestFgRecordedLivePidRefused (codex r28b): a foreground record whose FgEnginePID is ALIVE but whose
// token is empty/unreadable classifies FgUnknown. Because a recorded live pid we cannot disprove must
// be treated like FgAlive (the detached principle), every automated deletion path — PurgeRun, PruneRuns,
// ClearFinished, PurgeJobs — must REFUSE/SPARE it (its record survives) until the pid exits (→ FgDead),
// then all proceed. The r23-adjudicated legacy shape (FgEnginePID<=0, no token) stays deletable.
func TestFgRecordedLivePidRefused(t *testing.T) {
	self := os.Getpid()

	// The bug shape: {stopped, EnginePID 0, FgEnginePID=live, FgEngineProcStart=""} → FgUnknown WITH a
	// recorded pid. The empty recorded token forces FgUnknown regardless of what the platform reads.
	live := func() WorkflowRun {
		return WorkflowRun{RunID: "fg-live", SessionID: "s", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0, FgEnginePID: self, FgEngineProcStart: ""}
	}

	t.Run("classification + guards spare the recorded-live-pid shape", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		run := live()
		if got := ClassifyFgEngine(run); got != FgUnknown {
			t.Fatalf("an empty recorded token must classify FgUnknown, got %v", got)
		}
		if RunEngineProvablyNotLive(run) {
			t.Error("a recorded live fg pid must NOT be provably dead")
		}
		if !isLiveOrUnverifiable(run) {
			t.Error("PruneRuns eligibility must SPARE a recorded live fg pid")
		}
	})

	t.Run("PurgeRun refuses; record survives", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		writeRunForTest(t, live())
		if err := PurgeRun("fg-live"); err == nil || !strings.Contains(err.Error(), "foreground") {
			t.Errorf("PurgeRun must refuse under a recorded live fg pid, got %v", err)
		}
		if _, rerr := ReadRun("fg-live"); rerr != nil {
			t.Error("the refused PurgeRun must leave the record")
		}
	})

	t.Run("PruneRuns spares; record survives", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		writeRunForTest(t, live())
		if removed, err := PruneRuns(); err != nil || removed != 0 {
			t.Errorf("PruneRuns must spare the recorded live fg pid, removed=%d err=%v", removed, err)
		}
		if _, rerr := ReadRun("fg-live"); rerr != nil {
			t.Error("PruneRuns must leave the record")
		}
	})

	t.Run("ClearFinished + PurgeJobs leave it; record survives", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		writeRunForTest(t, live())
		if _, err := ClearFinished("s", pinned.Set{}); err != nil {
			t.Fatalf("ClearFinished: %v", err)
		}
		if _, rerr := ReadRun("fg-live"); rerr != nil {
			t.Error("ClearFinished routes through PurgeRun → must leave the record")
		}
		if _, _, _, err := PurgeJobs(); err != nil {
			t.Fatalf("PurgeJobs: %v", err)
		}
		if _, rerr := ReadRun("fg-live"); rerr != nil {
			t.Error("PurgeJobs routes through PurgeRun → must leave the record")
		}
	})

	t.Run("pid dead (FgDead) → PurgeRun proceeds", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		dead := live()
		dead.FgEnginePID = deadEnginePID // a dead recorded pid → FgDead → self-clearing
		if ClassifyFgEngine(dead) != FgDead {
			t.Fatal("a dead recorded fg pid must classify FgDead")
		}
		writeRunForTest(t, dead)
		if err := PurgeRun("fg-live"); err != nil {
			t.Errorf("once the fg pid is gone the delete must proceed, got %v", err)
		}
		if _, rerr := ReadRun("fg-live"); rerr == nil {
			t.Error("a FgDead record must be removed")
		}
	})

	t.Run("FgAlive (recorded pid + matching token) still refuses", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		origPS := procStartFn
		procStartFn = func(int) (string, bool) { return "tok", true }
		t.Cleanup(func() { procStartFn = origPS })
		alive := live()
		alive.FgEngineProcStart = "tok" // matching token → FgAlive
		if ClassifyFgEngine(alive) != FgAlive {
			t.Fatal("a matching recorded token must classify FgAlive")
		}
		writeRunForTest(t, alive)
		if err := PurgeRun("fg-live"); err == nil || !strings.Contains(err.Error(), "foreground") {
			t.Errorf("FgAlive refusal must be unchanged, got %v", err)
		}
	})

	t.Run("legacy no-fg-pid stays deletable (r23 preserved)", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		// {stopped, 0, no fg fields}: FgUnknown with NO recorded pid — status-based, prunable/deletable.
		legacy := WorkflowRun{RunID: "fg-legacy", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0}
		if isLiveOrUnverifiable(legacy) {
			t.Error("a terminal legacy no-fg record must NOT be spared (r23)")
		}
		writeRunForTest(t, legacy)
		if err := PurgeRun("fg-legacy"); err != nil {
			t.Errorf("a legacy no-fg record must stay deletable, got %v", err)
		}
		if _, rerr := ReadRun("fg-legacy"); rerr == nil {
			t.Error("PurgeRun must remove the legacy no-fg record")
		}
	})
}
