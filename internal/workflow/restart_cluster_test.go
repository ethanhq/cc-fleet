package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestLaunch_ResumeLiveGuard: Launch's resume branch refuses a run that still claims to be running with a
// live (or foreground/unverifiable EnginePID<=0) engine, so a public `workflow run --resume` can't launch a
// second engine over a live one. A freshly minted run is Status="running", EnginePID=0 → the guard fires.
func TestLaunch_ResumeLiveGuard(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	run, err := subagent.NewRunWithMeta("n", "d", "", nil) // mints Status="running", EnginePID=0
	if err != nil {
		t.Fatal(err)
	}
	_, lerr := Launch(context.Background(), "/nonexistent.star", Options{Resume: run.RunID}, false)
	if lerr == nil || !strings.Contains(lerr.Error(), "already has a live engine") {
		t.Fatalf("Launch --resume of a still-running run must refuse with a live-engine error, got: %v", lerr)
	}
}

// TestWaitEngineStarted_Timeout: WaitEngineStarted returns false when the child never self-stamps the
// expected pid into the manifest within the (test-shortened) startup budget — the path on which Launch
// kills + reaps the child and fails the run.
func TestWaitEngineStarted_Timeout(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	old := engineStartupBudget
	engineStartupBudget = 150 * time.Millisecond
	defer func() { engineStartupBudget = old }()

	run, err := subagent.NewRunWithMeta("n", "d", "", nil) // EnginePID=0, never becomes 12345
	if err != nil {
		t.Fatal(err)
	}
	if WaitEngineStarted(run.RunID, 12345) {
		t.Fatal("WaitEngineStarted must return false when the child never stamps its pid")
	}
}
