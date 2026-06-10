package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestGC_RemovesFinishedSyncJobUnderPIDReuse: a sync job records
// `PID = os.Getpid()` and `SettingsPath = ""`, so when the cc-fleet PID gets
// recycled processAlive returns true forever. GC must instead use the cached
// `<id>.result.json` as the authoritative terminal signal; this test proves GC
// succeeds even when meta.PID is the (still alive) current process.
func TestGC_RemovesFinishedSyncJobUnderPIDReuse(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}

	// Plant a job whose:
	//   - meta.PID  = os.Getpid()           (the cc-fleet PID a sync job would record)
	//   - SettingsPath = ""                  (sync jobs leave this empty)
	//   - StartedAt = long ago              (so olderThan cuts in)
	//   - <id>.result.json exists           (sync job WAS finalized)
	jobID := "sync-pid-reuse"
	meta := jobMeta{
		JobID:     jobID,
		PID:       os.Getpid(),
		PGID:      os.Getpid(),
		Provider:  "glm",
		Model:     "glm-4.6",
		StartedAt: time.Now().Add(-72 * time.Hour).Format(time.RFC3339),
		Status:    "done",
	}
	if data, err := json.Marshal(meta); err != nil {
		t.Fatalf("marshal meta: %v", err)
	} else if err := os.WriteFile(filepath.Join(dir, jobID+".json"), data, 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	// Result cache: presence is the load-bearing signal.
	res := Result{OK: true, JobID: jobID, Status: "done", Provider: "glm", Model: "glm-4.6"}
	if data, err := json.Marshal(res); err != nil {
		t.Fatalf("marshal result: %v", err)
	} else if err := os.WriteFile(filepath.Join(dir, jobID+".result.json"), data, 0o600); err != nil {
		t.Fatalf("write result: %v", err)
	}

	// Confirm the recycled-PID guard is still "alive" without our fix.
	if !processAlive(meta.PID, meta.SettingsPath) {
		t.Fatalf("test precondition broken: processAlive should say true for current PID")
	}

	out := GC(time.Hour)
	if !out.OK {
		t.Fatalf("GC failed: %s %s", out.ErrorCode, out.ErrorMsg)
	}
	if out.Removed != 1 {
		t.Fatalf("GC removed %d, want 1", out.Removed)
	}
	// Both files must be gone.
	for _, suffix := range []string{".json", ".result.json"} {
		path := filepath.Join(dir, jobID+suffix)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s should be gone; stat err=%v", path, err)
		}
	}
}

// TestGC_KeepsRunningJobWithoutResultCache verifies the negative case: a job
// whose process truly is alive AND has no result.json yet must NOT be GC'd.
func TestGC_KeepsRunningJobWithoutResultCache(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}

	jobID := "still-running"
	meta := jobMeta{
		JobID:     jobID,
		PID:       os.Getpid(),
		PGID:      os.Getpid(),
		Provider:  "glm",
		StartedAt: time.Now().Add(-72 * time.Hour).Format(time.RFC3339),
		Status:    "running",
	}
	if data, err := json.Marshal(meta); err != nil {
		t.Fatalf("marshal: %v", err)
	} else if err := os.WriteFile(filepath.Join(dir, jobID+".json"), data, 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	// No .result.json — the job is genuinely running.

	out := GC(time.Hour)
	if !out.OK {
		t.Fatalf("GC failed: %s", out.ErrorMsg)
	}
	if out.Removed != 0 {
		t.Errorf("GC removed %d, want 0 — running job without result must be kept", out.Removed)
	}
	if _, err := os.Stat(filepath.Join(dir, jobID+".json")); err != nil {
		t.Errorf("meta should still exist: %v", err)
	}
}

// plantFinishedJob writes a finished job (meta + authoritative result cache) so
// liveness is irrelevant; `started` controls its age for GC's cutoff.
func plantFinishedJob(t *testing.T, dir, jobID string, started time.Time) {
	t.Helper()
	meta := jobMeta{
		JobID: jobID, PID: os.Getpid(), PGID: os.Getpid(),
		Provider: "glm", Model: "glm-4.6",
		StartedAt: started.Format(time.RFC3339), Status: "done",
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, jobID+".json"), data, 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	rdata, err := json.Marshal(Result{OK: true, JobID: jobID, Status: "done"})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, jobID+".result.json"), rdata, 0o600); err != nil {
		t.Fatalf("write result: %v", err)
	}
}

// TestGC_ZeroOlderThanRemovesAllFinished: `--older-than 0s` means "no age limit
// — remove every finished job", NOT "fall back to 24h". A job that finished a
// minute ago survives GC(24h) but is removed by GC(0).
func TestGC_ZeroOlderThanRemovesAllFinished(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}
	plantFinishedJob(t, dir, "recent-done", time.Now().Add(-1*time.Minute))

	if out := GC(24 * time.Hour); !out.OK || out.Removed != 0 {
		t.Fatalf("GC(24h): OK=%v removed=%d, want a recent finished job kept (0)", out.OK, out.Removed)
	}
	if out := GC(0); !out.OK || out.Removed != 1 {
		t.Fatalf("GC(0): OK=%v removed=%d, want all finished removed (1)", out.OK, out.Removed)
	}
}

// TestGC_NegativeOlderThanFallsBackToDefault: a negative duration is treated as
// "unset" → defaultGCAge, so a recent finished job is kept (not removed).
func TestGC_NegativeOlderThanFallsBackToDefault(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}
	plantFinishedJob(t, dir, "recent-done-neg", time.Now().Add(-1*time.Minute))

	if out := GC(-5 * time.Minute); !out.OK || out.Removed != 0 {
		t.Fatalf("GC(negative): OK=%v removed=%d, want fallback-to-default → kept (0)", out.OK, out.Removed)
	}
}
