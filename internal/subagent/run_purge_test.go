package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPurgeJobs_KeepsManifestWithRunningMember: a run with a live member keeps its
// manifest, the runs/ dir, and the jobs dir; the running job is reported.
func TestPurgeJobs_KeepsManifestWithRunningMember(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}

	writeRunForTest(t, WorkflowRun{RunID: "run-keep", StartedAt: time.Now().Format(time.RFC3339), Status: "running"})
	// Live member: our pid, no result cache, empty SettingsPath.
	meta := jobMeta{
		JobID: "live", PID: os.Getpid(), PGID: os.Getpid(),
		Vendor: "glm", StartedAt: time.Now().Format(time.RFC3339),
		Status: "running", RunID: "run-keep",
	}
	data, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, "live.json"), data, 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	_, _, running, err := PurgeJobs()
	if err != nil {
		t.Fatalf("PurgeJobs: %v", err)
	}
	if len(running) != 1 || running[0] != "live" {
		t.Fatalf("running = %v, want [live]", running)
	}
	if !runManifestExists(t, "run-keep") {
		t.Errorf("manifest with a running member must be kept")
	}
	if _, err := os.Stat(filepath.Join(dir, runsDirName)); err != nil {
		t.Errorf("runs dir should be kept while a run has a live member: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("jobs dir should be kept while a job is running: %v", err)
	}
}

// TestPurgeJobs_RemovesManifestAndDirsWhenNothingRunning: a run with only finished
// members → manifest removed, runs/ removed, and (nothing running) jobs dir removed.
func TestPurgeJobs_RemovesManifestAndDirsWhenNothingRunning(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}

	writeRunForTest(t, WorkflowRun{RunID: "run-done", StartedAt: time.Now().Format(time.RFC3339), Status: "running"})
	planFinishedRunMember(t, dir, "done-member", "run-done", time.Now())

	_, removedFinished, running, err := PurgeJobs()
	if err != nil {
		t.Fatalf("PurgeJobs: %v", err)
	}
	if len(running) != 0 {
		t.Fatalf("running = %v, want none", running)
	}
	if len(removedFinished) != 1 || removedFinished[0] != "done-member" {
		t.Fatalf("removedFinished = %v, want [done-member]", removedFinished)
	}
	if runManifestExists(t, "run-done") {
		t.Errorf("manifest with no running member must be removed")
	}
	// runs/ dir gone (empty after manifest removal).
	if _, err := os.Stat(filepath.Join(dir, runsDirName)); !os.IsNotExist(err) {
		t.Errorf("runs dir should be gone after purge (err=%v)", err)
	}
	// jobs dir gone (nothing running, runs/ no longer orphans it).
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("jobs dir should be removed when nothing runs (err=%v)", err)
	}
}
