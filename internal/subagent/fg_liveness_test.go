package subagent

import (
	"os"
	"strings"
	"testing"
)

// TestPruneAndPurgeHonorFgLiveness: prune/delete must honor the foreground death proof. A
// blind-stopped LIVE foreground run ({stopped, 0, FgAlive}) is SPARED by PruneRuns and REFUSED by a
// direct PurgeRun (its engine runs in the user's terminal — unreapable, fail closed). A Ctrl-C'd fg
// run ({stopped, 0, FgDead}) and an old pre-field record ({stopped, 0, no fg}) are both prunable and
// directly deletable — consistent with resume now allowing the dead one and old records staying
// cleanable.
func TestPruneAndPurgeHonorFgLiveness(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	orig := procStartFn
	procStartFn = func(int) (string, bool) { return "fg-tok", true }
	t.Cleanup(func() { procStartFn = orig })
	self := os.Getpid()
	const dead = 0x7ffffffe

	live := WorkflowRun{RunID: "fg-live", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0, FgEnginePID: self, FgEngineProcStart: "fg-tok"}
	deadFg := WorkflowRun{RunID: "fg-dead", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0, FgEnginePID: dead}
	oldRec := WorkflowRun{RunID: "fg-old", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0}
	seed := func() {
		for _, r := range []WorkflowRun{live, deadFg, oldRec} {
			if err := SaveRun(r); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Direct PurgeRun: the live fg run is refused (and survives); the others delete.
	seed()
	if err := PurgeRun("fg-live"); err == nil || !strings.Contains(err.Error(), "running in the foreground") {
		t.Errorf("PurgeRun of a live fg run must be refused, got %v", err)
	}
	if _, rerr := ReadRun("fg-live"); rerr != nil {
		t.Error("a refused live fg run must still exist")
	}
	if err := PurgeRun("fg-dead"); err != nil {
		t.Errorf("PurgeRun of a Ctrl-C'd fg run must succeed, got %v", err)
	}
	if err := PurgeRun("fg-old"); err != nil {
		t.Errorf("PurgeRun of an old-record run must succeed, got %v", err)
	}

	// PruneRuns: spares the live fg run, reaps the dead + old ones.
	seed()
	removed, err := PruneRuns()
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}
	if removed != 2 {
		t.Errorf("PruneRuns removed = %d, want 2 (dead fg + old record; live fg spared)", removed)
	}
	if _, rerr := ReadRun("fg-live"); rerr != nil {
		t.Error("PruneRuns must spare a live fg run")
	}
	for _, id := range []string{"fg-dead", "fg-old"} {
		if _, rerr := ReadRun(id); rerr == nil {
			t.Errorf("PruneRuns must reap %q", id)
		}
	}
}
