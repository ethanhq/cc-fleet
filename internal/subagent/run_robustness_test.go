package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCorruptManifest drops an unparseable file at runs/<runID>.json so the GC /
// Purge robustness paths (membership decided by filename, corrupt orphan reaped) can
// be exercised.
func writeCorruptManifest(t *testing.T, runID string) {
	t.Helper()
	dir, err := runsDir()
	if err != nil {
		t.Fatalf("runsDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir runs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, runID+".json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write corrupt manifest: %v", err)
	}
}

// writeLiveRunMember writes a running job meta (our own pid, no result cache, empty
// SettingsPath → processAlive true) tagged with runID.
func writeLiveRunMember(t *testing.T, dir, jobID, runID string) {
	t.Helper()
	meta := jobMeta{
		JobID: jobID, PID: os.Getpid(), PGID: os.Getpid(), Vendor: "glm",
		StartedAt: time.Now().Format(time.RFC3339), Status: "running", RunID: runID,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, jobID+".json"), data, 0o600); err != nil {
		t.Fatalf("write live meta: %v", err)
	}
}

// TestGC_CorruptOrphanManifestPruned: an unparseable manifest with no surviving
// member is treated as an aged orphan and removed — before the run-aware GC
// hardening it was skipped via `continue` and leaked forever.
func TestGC_CorruptOrphanManifestPruned(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(xdg, "cc-fleet", jobsDirName), 0o700); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}

	writeCorruptManifest(t, "run-bad")
	if out := GC(0); !out.OK {
		t.Fatalf("GC(0) failed: %s", out.ErrorMsg)
	}
	if runManifestExists(t, "run-bad") {
		t.Errorf("a corrupt orphan manifest must be pruned, not leaked forever")
	}
}

// TestGC_CorruptManifestWithLiveMemberKept: membership is checked by run id BEFORE
// the manifest is read, so a live member protects even an unparseable manifest.
func TestGC_CorruptManifestWithLiveMemberKept(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}

	writeCorruptManifest(t, "run-corrupt")
	writeLiveRunMember(t, dir, "job-live", "run-corrupt")
	if out := GC(0); !out.OK {
		t.Fatalf("GC(0) failed: %s", out.ErrorMsg)
	}
	if !runManifestExists(t, "run-corrupt") {
		t.Errorf("a live member must protect even a corrupt manifest")
	}
}

// TestPurge_CorruptManifestRobustness: PurgeJobs keeps a corrupt manifest whose run
// still has a live member (membership keyed on the filename, not the parsed body)
// and drops a corrupt orphan.
func TestPurge_CorruptManifestRobustness(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}

	writeCorruptManifest(t, "run-live")
	writeLiveRunMember(t, dir, "job-live", "run-live")
	writeCorruptManifest(t, "run-orphan")

	if _, _, _, err := PurgeJobs(); err != nil {
		t.Fatalf("PurgeJobs: %v", err)
	}
	if !runManifestExists(t, "run-live") {
		t.Errorf("a corrupt manifest with a live member must be kept")
	}
	if runManifestExists(t, "run-orphan") {
		t.Errorf("a corrupt orphan manifest must be purged")
	}
}

// TestReadRun_UnknownIsPathFree: an unknown run id yields a canonical, path-free
// "not found" error so the config-dir layout never leaks into the CLI's JSON
// error envelope.
func TestReadRun_UnknownIsPathFree(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	_, err := ReadRun("no-such-run")
	if err == nil {
		t.Fatal("ReadRun of an unknown run should error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("want a canonical 'not found' message, got %q", err.Error())
	}
	if strings.Contains(err.Error(), xdg) || strings.Contains(err.Error(), "cc-fleet") {
		t.Errorf("error must not leak the filesystem path: %q", err.Error())
	}
}
