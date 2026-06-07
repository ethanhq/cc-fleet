package userops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// subagentJobsDir mirrors internal/subagent.jobsDir() (ConfigDir/subagent-jobs)
// so the test can seed raw job files there and verify Uninstall's PurgeJobs
// integration without launching real subagent processes.
func subagentJobsDir(t *testing.T) string {
	t.Helper()
	cfgDir, err := config.ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	return filepath.Join(cfgDir, "subagent-jobs")
}

// seedFinishedJob writes a complete FINISHED job file group: meta (.json),
// captured streams (.out/.err/.prompt) and the authoritative terminal cache
// (.result.json). The result cache marks it finished regardless of the meta
// PID — PurgeJobs is result-cache-first, like GC. Returns the seeded paths so
// the test can assert every one is gone.
func seedFinishedJob(t *testing.T, dir, jobID string) []string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs dir: %v", err)
	}
	files := map[string]string{
		jobID + ".json":        fmt.Sprintf(`{"job_id":%q,"pid":999999,"status":"running","vendor":"glm","model":"glm-4.6"}`, jobID),
		jobID + ".out":         "stdout fragment",
		jobID + ".err":         "stderr fragment",
		jobID + ".prompt":      "prompt fragment",
		jobID + ".result.json": fmt.Sprintf(`{"ok":true,"status":"done","job_id":%q}`, jobID),
	}
	paths := make([]string, 0, len(files))
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
		paths = append(paths, p)
	}
	return paths
}

// TestUninstall_PurgesFinishedSubagentJobs: with only finished jobs present,
// Uninstall removes the entire job file group + the now-empty subagent-jobs dir,
// and reports the dir in Removed.
func TestUninstall_PurgesFinishedSubagentJobs(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	dir := subagentJobsDir(t)
	paths := seedFinishedJob(t, dir, "j1")

	res, err := Uninstall(UninstallRequest{KeepSecrets: true})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	// Every seeded file (incl. the prompt/result fragments) must be gone.
	for _, p := range paths {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists post-Uninstall (err=%v)", p, err)
		}
	}
	// The now-empty jobs dir itself must be removed.
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subagent-jobs dir still exists post-Uninstall (err=%v)", err)
	}
	if !contains(res.Removed, dir) {
		t.Fatalf("Removed missing subagent-jobs dir (got %v)", res.Removed)
	}
}

// TestUninstall_KeepsRunningSubagentJob: when a job is still running, Uninstall
// removes NOTHING under subagent-jobs and reports the live job id in Kept (and a
// note on stderr) — so it never yanks files from under a live background job.
func TestUninstall_KeepsRunningSubagentJob(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	dir := subagentJobsDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs dir: %v", err)
	}
	// A "running" job: meta carries our own (alive) PID and there is NO
	// .result.json, so PurgeJobs's result-cache-first check falls through to
	// processAlive, which sees this live test process (SettingsPath empty → bare
	// kill(0)).
	jobID := "live1"
	metaPath := filepath.Join(dir, jobID+".json")
	meta := fmt.Sprintf(`{"job_id":%q,"pid":%d,"status":"running","vendor":"glm","model":"glm-4.6"}`, jobID, os.Getpid())
	if err := os.WriteFile(metaPath, []byte(meta), 0o600); err != nil {
		t.Fatalf("seed meta: %v", err)
	}
	outPath := filepath.Join(dir, jobID+".out")
	if err := os.WriteFile(outPath, []byte("in progress"), 0o600); err != nil {
		t.Fatalf("seed out: %v", err)
	}

	res, err := Uninstall(UninstallRequest{KeepSecrets: true})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	// The running job's files must be left intact.
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("running job meta removed (should be kept): %v", err)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("running job .out removed (should be kept): %v", err)
	}
	// The dir must NOT be reported as Removed...
	if contains(res.Removed, dir) {
		t.Fatalf("subagent-jobs dir wrongly in Removed while a job is running (got %v)", res.Removed)
	}
	// ...and must be reported in Kept, naming the live job id.
	reported := false
	for _, k := range res.Kept {
		if strings.Contains(k, dir) && strings.Contains(k, jobID) {
			reported = true
			break
		}
	}
	if !reported {
		t.Fatalf("Kept missing running-job report for %q (got %v)", jobID, res.Kept)
	}
}

// TestUninstall_MixedJobsPartialClean: with one FINISHED job and one RUNNING job
// present, Uninstall removes the finished job's full file group while keeping the
// running job's files AND the jobs dir — and reports both (removed-finished in
// Removed, the live id in Kept).
func TestUninstall_MixedJobsPartialClean(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	dir := subagentJobsDir(t)

	// Finished job "done": full group incl. the authoritative terminal cache.
	donePaths := seedFinishedJob(t, dir, "done")

	// Running job "live": meta with our own (alive) PID and NO .result.json, so
	// PurgeJobs's result-cache-first check falls through to processAlive (alive).
	liveMeta := filepath.Join(dir, "live.json")
	liveMetaBody := fmt.Sprintf(`{"job_id":"live","pid":%d,"status":"running","vendor":"glm","model":"glm-4.6"}`, os.Getpid())
	if err := os.WriteFile(liveMeta, []byte(liveMetaBody), 0o600); err != nil {
		t.Fatalf("seed live meta: %v", err)
	}
	liveOut := filepath.Join(dir, "live.out")
	if err := os.WriteFile(liveOut, []byte("in progress"), 0o600); err != nil {
		t.Fatalf("seed live out: %v", err)
	}

	res, err := Uninstall(UninstallRequest{KeepSecrets: true})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	// The finished job's files must ALL be gone, even though a live job remains.
	for _, p := range donePaths {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("finished job file %s survived (err=%v) — partial clean failed", p, err)
		}
	}
	// The running job's files must ALL be kept.
	if _, err := os.Stat(liveMeta); err != nil {
		t.Fatalf("running job meta removed (should be kept): %v", err)
	}
	if _, err := os.Stat(liveOut); err != nil {
		t.Fatalf("running job .out removed (should be kept): %v", err)
	}
	// The dir must remain (a running job keeps it alive).
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("subagent-jobs dir removed while a job is running: %v", err)
	}
	// Report: removed-finished surfaced in Removed; the live id surfaced in Kept.
	removedReported := false
	for _, r := range res.Removed {
		if strings.Contains(r, dir) && strings.Contains(r, "finished") {
			removedReported = true
			break
		}
	}
	if !removedReported {
		t.Fatalf("Removed missing finished-job report (got %v)", res.Removed)
	}
	keptReported := false
	for _, k := range res.Kept {
		if strings.Contains(k, dir) && strings.Contains(k, "live") {
			keptReported = true
			break
		}
	}
	if !keptReported {
		t.Fatalf("Kept missing running-job report for %q (got %v)", "live", res.Kept)
	}
}

// TestUninstall_PurgesTeamsHistory: Uninstall removes the whole teams-history dir
// (an ended team's board snapshot) and reports it in Removed.
func TestUninstall_PurgesTeamsHistory(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfgDir, err := config.ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	histDir := filepath.Join(cfgDir, "teams-history")
	if err := os.MkdirAll(histDir, 0o700); err != nil {
		t.Fatalf("mkdir teams-history: %v", err)
	}
	rec := filepath.Join(histDir, "alpha.json")
	if err := os.WriteFile(rec, []byte(`{"team":"alpha","members":[]}`), 0o600); err != nil {
		t.Fatalf("seed record: %v", err)
	}

	res, err := Uninstall(UninstallRequest{KeepSecrets: true})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(histDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("teams-history dir still exists post-Uninstall (err=%v)", err)
	}
	if !contains(res.Removed, histDir) {
		t.Fatalf("Removed missing teams-history dir (got %v)", res.Removed)
	}
}
