package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeRunForTest persists a Run manifest directly (bypassing NewRun's minting) so
// tests can control RunID + StartedAt. Used by run_test.go and the GC tests.
func writeRunForTest(t *testing.T, run WorkflowRun) {
	t.Helper()
	dir, err := runsDir()
	if err != nil {
		t.Fatalf("runsDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir runs: %v", err)
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, run.RunID+".json"), data, 0o600); err != nil {
		t.Fatalf("write run: %v", err)
	}
}

// runManifestExists reports whether a manifest file is still on disk.
func runManifestExists(t *testing.T, runID string) bool {
	t.Helper()
	dir, err := runsDir()
	if err != nil {
		t.Fatalf("runsDir: %v", err)
	}
	_, statErr := os.Stat(filepath.Join(dir, runID+".json"))
	return statErr == nil
}

// planFinishedRunMember writes a finished job (meta + result cache) tagged with a
// RunID, with the given age, so GC's cutoff applies to the member.
func planFinishedRunMember(t *testing.T, dir, jobID, runID string, started time.Time) {
	t.Helper()
	meta := jobMeta{
		JobID: jobID, PID: os.Getpid(), PGID: os.Getpid(),
		Provider: "glm", Model: "glm-4.6",
		StartedAt: started.Format(time.RFC3339), Status: "done",
		RunID: runID, Phase: "build", Label: "w1",
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, jobID+".json"), data, 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	rdata, err := json.Marshal(Result{OK: true, JobID: jobID, Status: "done", RunID: runID})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, jobID+".result.json"), rdata, 0o600); err != nil {
		t.Fatalf("write result: %v", err)
	}
}

// TestGC_AgedOutRunMemberRemovesManifest: a run whose only member is finished and
// aged out → GC(0) removes the member AND the manifest.
func TestGC_AgedOutRunMemberRemovesManifest(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}

	writeRunForTest(t, WorkflowRun{RunID: "run-aged", Name: "old", StartedAt: time.Now().Add(-72 * time.Hour).Format(time.RFC3339), Status: "running"})
	planFinishedRunMember(t, dir, "job-aged", "run-aged", time.Now().Add(-72*time.Hour))

	out := GC(0)
	if !out.OK {
		t.Fatalf("GC failed: %s", out.ErrorMsg)
	}
	if out.Removed != 1 {
		t.Fatalf("GC Removed = %d, want 1 (the member; manifests are not counted)", out.Removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "job-aged.json")); !os.IsNotExist(err) {
		t.Errorf("aged member should be gone")
	}
	if runManifestExists(t, "run-aged") {
		t.Errorf("manifest of a fully aged-out run should be pruned")
	}
}

// TestGC_RecentRunMemberKeepsManifest: a run with a recent finished member → the
// manifest survives a default-age GC (the member protects it).
func TestGC_RecentRunMemberKeepsManifest(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}

	writeRunForTest(t, WorkflowRun{RunID: "run-recent", StartedAt: time.Now().Add(-1 * time.Minute).Format(time.RFC3339), Status: "running"})
	planFinishedRunMember(t, dir, "job-recent", "run-recent", time.Now().Add(-1*time.Minute))

	if out := GC(24 * time.Hour); !out.OK || out.Removed != 0 {
		t.Fatalf("GC(24h): OK=%v removed=%d, want a recent member kept (0)", out.OK, out.Removed)
	}
	if !runManifestExists(t, "run-recent") {
		t.Errorf("manifest with a recent member must be kept")
	}
}

// TestGC_FreshEmptyManifestKeptThenRemoved: a freshly created empty manifest (no
// members) survives a default-age GC but is removed by GC(0).
func TestGC_FreshEmptyManifestKeptThenRemoved(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	run, err := NewRun("empty", nil)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if out := GC(24 * time.Hour); !out.OK {
		t.Fatalf("GC(24h) failed: %s", out.ErrorMsg)
	}
	if !runManifestExists(t, run.RunID) {
		t.Errorf("a fresh empty manifest must survive a default-age GC")
	}
	if out := GC(0); !out.OK {
		t.Fatalf("GC(0) failed: %s", out.ErrorMsg)
	}
	if runManifestExists(t, run.RunID) {
		t.Errorf("GC(0) should remove an unused empty manifest")
	}
}

// TestGC_RunningMemberKeepsManifestEvenAtZero: a run with a genuinely running
// member (pid = os.Getpid(), no result cache, empty SettingsPath) → the manifest
// is kept even by GC(0), because its member is live.
func TestGC_RunningMemberKeepsManifestEvenAtZero(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}

	writeRunForTest(t, WorkflowRun{RunID: "run-live", StartedAt: time.Now().Add(-72 * time.Hour).Format(time.RFC3339), Status: "running"})
	// A live member: our own pid, NO result cache, empty SettingsPath → processAlive true.
	meta := jobMeta{
		JobID: "job-live", PID: os.Getpid(), PGID: os.Getpid(),
		Provider: "glm", StartedAt: time.Now().Add(-72 * time.Hour).Format(time.RFC3339),
		Status: "running", RunID: "run-live",
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job-live.json"), data, 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	if out := GC(0); !out.OK {
		t.Fatalf("GC(0) failed: %s", out.ErrorMsg)
	}
	if !runManifestExists(t, "run-live") {
		t.Errorf("a run with a live member must be kept even by GC(0)")
	}
}
