package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSyncJobPersistsIOOptIn: with PersistIO the sync job writes 0600 prompt+answer side
// files for board drill-in, while the result CACHE stays answer-stripped (the board table
// never shows a reply). Opted out, no side files exist.
func TestSyncJobPersistsIOOptIn(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	jid := regSyncJob(Request{Vendor: "v", PersistIO: true, IOPrompt: "the prompt"}, "m")
	if jid == "" {
		t.Fatal("registerSyncJob returned empty id")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, jid+".prompt")); string(b) != "the prompt" {
		t.Errorf("prompt side file = %q, want 'the prompt'", b)
	}
	finalizeSyncJob(jid, Result{OK: true, Result: "the answer"})
	if b, _ := os.ReadFile(filepath.Join(dir, jid+".answer")); string(b) != "the answer" {
		t.Errorf("answer side file = %q, want 'the answer'", b)
	}
	if cache, _ := os.ReadFile(filepath.Join(dir, jid+".result.json")); strings.Contains(string(cache), "the answer") {
		t.Error("the result cache must stay answer-stripped (board table safety)")
	}

	jid2 := regSyncJob(Request{Vendor: "v", PersistIO: false, IOPrompt: "secret"}, "m")
	if jid2 == "" {
		t.Fatal("registerSyncJob(2) returned empty id")
	}
	if _, err := os.Stat(filepath.Join(dir, jid2+".prompt")); !os.IsNotExist(err) {
		t.Error("no prompt side file when persist-io is off")
	}
	finalizeSyncJob(jid2, Result{OK: true, Result: "ans2"})
	if _, err := os.Stat(filepath.Join(dir, jid2+".answer")); !os.IsNotExist(err) {
		t.Error("no answer side file when persist-io is off")
	}
}

// TestStopRunReuseGuardSparesForeignPID: StopRun must NOT kill a pid whose cmdline is not
// this run's workflow engine (a recycled-pid guard). The test process's own (live) pid stands
// in for a "recycled" pid, with the argv seam driven to a non-engine argv so the guard reads it
// as foreign on every platform, refuses the kill, and merely flips the run to stopped.
func TestStopRunReuseGuardSparesForeignPID(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	writeRunForTest(t, WorkflowRun{
		RunID: "stoprun", StartedAt: time.Now().UTC().Format(time.RFC3339),
		Status: "running", EnginePID: os.Getpid(),
	})
	// Drive the argv seam to a readable non-engine argv so the recycled-pid guard reads this live
	// pid as foreign deterministically — the real reader is platform-specific (and unreadable on
	// Windows, where the liveness soft-fail would otherwise read an unverifiable live pid as ours).
	orig := reuseGuardArgv
	t.Cleanup(func() { reuseGuardArgv = orig })
	reuseGuardArgv = func(int) ([]string, bool) { return []string{"some", "other", "proc"}, true }
	run, err := StopRun("stoprun")
	if err != nil {
		t.Fatalf("StopRun: %v", err)
	}
	if run.Status != "stopped" {
		t.Errorf("status = %q, want stopped", run.Status)
	}
	if !pidAlive(os.Getpid()) {
		t.Fatal("StopRun killed the test process — the reuse guard FAILED to spare a foreign pid")
	}
}

// TestEngineCmdlineMatches drives the reuse-guard matcher directly via the reuseGuardArgv
// seam: the detached-engine argv (workflow + run + --run-id + the id) MATCHES, while a
// foreground run (no --run-id), a --resume run that merely mentions the id, a wrong id,
// and an unreadable argv all REJECT — so StopRun never kills an unverifiable pid.
func TestEngineCmdlineMatches(t *testing.T) {
	const id = "run-xyz"
	old := reuseGuardArgv
	t.Cleanup(func() { reuseGuardArgv = old })

	cases := []struct {
		name string
		argv []string
		ok   bool
		want bool
	}{
		{"detached engine", []string{"/usr/bin/cc-fleet", "workflow", "run", "/t/x.star", "--foreground", "--run-id", id}, true, true},
		{"foreground (no --run-id)", []string{"/usr/bin/cc-fleet", "workflow", "run", "/t/x.star", "--foreground"}, true, false},
		{"foreground --resume mentions id", []string{"/usr/bin/cc-fleet", "workflow", "run", "/t/x.star", "--resume", id, "--foreground"}, true, false},
		{"wrong id", []string{"/usr/bin/cc-fleet", "workflow", "run", "x.star", "--run-id", "other"}, true, false},
		{"unrelated process", []string{"/usr/bin/python", "server.py"}, true, false},
		{"unreadable argv", nil, false, false},
	}
	for _, c := range cases {
		reuseGuardArgv = func(int) ([]string, bool) { return c.argv, c.ok }
		if got := engineCmdlineMatches(4242, id); got != c.want {
			t.Errorf("%s: engineCmdlineMatches = %v, want %v", c.name, got, c.want)
		}
	}
}
