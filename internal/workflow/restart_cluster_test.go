package workflow

import (
	"context"
	"os"
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
	_, lerr := Launch(context.Background(), "/nonexistent.js", Options{Resume: run.RunID}, false)
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

// TestRestart_StarOnlyRunRefused: a run whose only saved script is the retired Starlark
// engine's .star sidecar is refused explicitly — its script can't execute on the
// JavaScript runtime — before any destructive step (stop / journal rewrite).
func TestRestart_StarOnlyRunRefused(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	run, err := subagent.NewRunWithMeta("n", "d", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	lp, err := subagent.LegacyRunScriptPath(run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lp, []byte("meta = {\"name\": \"n\", \"description\": \"d\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rerr := Restart(context.Background(), run.RunID, "")
	if rerr == nil || !strings.Contains(rerr.Error(), "predates the JavaScript workflow engine") {
		t.Fatalf("restart of a .star-only run must refuse with the predates error, got: %v", rerr)
	}
}
