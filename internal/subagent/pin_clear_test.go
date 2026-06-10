package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/pinned"
)

// jobsDirForTest sets a temp ConfigDir and returns the jobs dir.
func jobsDirForTest(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}
	return dir
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeFinishedJobRec writes a finished job (meta + terminal result cache) tagged with the given
// run/session/status, started an hour ago so any GC(0) cutoff treats it as finished-and-not-recent.
func writeFinishedJobRec(t *testing.T, dir, jobID, runID, session, status string) {
	t.Helper()
	started := time.Now().Add(-time.Hour).Format(time.RFC3339)
	writeJSONFile(t, filepath.Join(dir, jobID+".json"), jobMeta{
		JobID: jobID, PID: os.Getpid(), StartedAt: started,
		Status: status, RunID: runID, LeadSessionID: session,
	})
	writeJSONFile(t, filepath.Join(dir, jobID+".result.json"), Result{
		OK: status == "done", JobID: jobID, Status: status, RunID: runID, LeadSessionID: session,
	})
}

// writeRunningJob writes a job with no result cache and a live pid, so StatusFor reports "running".
func writeRunningJob(t *testing.T, dir, jobID, session string) {
	t.Helper()
	writeJSONFile(t, filepath.Join(dir, jobID+".json"), jobMeta{
		JobID: jobID, PID: os.Getpid(), StartedAt: time.Now().Format(time.RFC3339),
		Status: "running", LeadSessionID: session,
	})
}

func jobExists(t *testing.T, dir, jobID string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, jobID+".json"))
	return err == nil
}

// TestDeleteSession_WipesAllExceptPinned: DeleteSession removes a session's runs (with members) and
// standalone jobs of ANY status, but keeps a pinned standalone job; another session is untouched.
func TestDeleteSession_WipesAllExceptPinned(t *testing.T) {
	dir := jobsDirForTest(t)
	hour := time.Now().Add(-time.Hour)
	writeRunForTest(t, WorkflowRun{RunID: "runA", SessionID: "sessA", Status: "done", StartedAt: hour.Format(time.RFC3339)})
	planFinishedRunMember(t, dir, "leafA", "runA", hour)
	writeFinishedJobRec(t, dir, "jobA", "", "sessA", "done")
	writeFinishedJobRec(t, dir, "jobPin", "", "sessA", "done")
	writeFinishedJobRec(t, dir, "jobB", "", "sessB", "done") // another session — must survive

	if err := pinned.Pin(pinned.Job, "jobPin"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	pins, _ := pinned.Snapshot()
	removed, err := DeleteSession("sessA", pins)
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if removed != 2 { // runA + jobA; leafA reaped with runA (uncounted); jobPin kept
		t.Fatalf("expected 2 removed (runA + jobA), got %d", removed)
	}
	if runManifestExists(t, "runA") {
		t.Error("runA should be deleted")
	}
	if jobExists(t, dir, "leafA") {
		t.Error("runA's member leafA should be reaped with it")
	}
	if jobExists(t, dir, "jobA") {
		t.Error("standalone jobA should be deleted")
	}
	if !jobExists(t, dir, "jobPin") {
		t.Error("pinned jobPin must survive a session wipe")
	}
	if !jobExists(t, dir, "jobB") {
		t.Error("another session's jobB must be untouched")
	}
}

// TestDeleteSession_ReapsRunningStandaloneJob: a still-live standalone job is reaped (its process
// tree killed) BEFORE its files are removed — deleting the handle must not orphan a live process.
func TestDeleteSession_ReapsRunningStandaloneJob(t *testing.T) {
	dir := jobsDirForTest(t)
	writeRunningJob(t, dir, "jobLive", "sessA")
	killed := 0
	origReap := reapJobTree
	reapJobTree = func(pid int) { killed++ }
	t.Cleanup(func() { reapJobTree = origReap })

	pins, _ := pinned.Snapshot()
	removed, err := DeleteSession("sessA", pins)
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}
	if killed == 0 {
		t.Error("a running standalone job must be reaped (process tree killed) before its files go")
	}
	if jobExists(t, dir, "jobLive") {
		t.Error("the reaped job's files should be removed")
	}
}

// TestGC_PinnedRunKeepsLeavesAndManifest: pinning a run protects its manifest AND its leaf jobs
// from GC(0); unpinning lets a later GC(0) remove both (acceptance 1+2, run↔job coupling).
func TestGC_PinnedRunKeepsLeavesAndManifest(t *testing.T) {
	dir := jobsDirForTest(t)
	writeRunForTest(t, WorkflowRun{RunID: "run-pin", Status: "done", StartedAt: time.Now().Add(-72 * time.Hour).Format(time.RFC3339)})
	planFinishedRunMember(t, dir, "leaf-1", "run-pin", time.Now().Add(-72*time.Hour))

	if err := pinned.Pin(pinned.Run, "run-pin"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if out := GC(0); !out.OK {
		t.Fatalf("GC: %s", out.ErrorMsg)
	}
	if !jobExists(t, dir, "leaf-1") {
		t.Error("a pinned run's leaf must survive GC(0)")
	}
	if !runManifestExists(t, "run-pin") {
		t.Error("a pinned run's manifest must survive GC(0)")
	}

	if err := pinned.Unpin(pinned.Run, "run-pin"); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	if out := GC(0); !out.OK {
		t.Fatalf("GC: %s", out.ErrorMsg)
	}
	if jobExists(t, dir, "leaf-1") {
		t.Error("after unpin, the aged leaf should be removed")
	}
	if runManifestExists(t, "run-pin") {
		t.Error("after unpin, the aged manifest should be removed")
	}
}

// TestGC_PinnedMemberlessRunKept: a pinned run with no surviving member is still kept by GC(0)
// (the explicit pin skip, not membership, protects it).
func TestGC_PinnedMemberlessRunKept(t *testing.T) {
	jobsDirForTest(t)
	writeRunForTest(t, WorkflowRun{RunID: "run-empty", Status: "done", StartedAt: time.Now().Add(-72 * time.Hour).Format(time.RFC3339)})
	if err := pinned.Pin(pinned.Run, "run-empty"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	GC(0)
	if !runManifestExists(t, "run-empty") {
		t.Error("a pinned memberless run manifest must survive GC(0)")
	}
}

// TestGC_PinnedStandaloneJobKept: a pinned standalone finished job survives GC(0).
func TestGC_PinnedStandaloneJobKept(t *testing.T) {
	dir := jobsDirForTest(t)
	writeFinishedJobRec(t, dir, "job-pin", "", "sessA", "done")
	if err := pinned.Pin(pinned.Job, "job-pin"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	GC(0)
	if !jobExists(t, dir, "job-pin") {
		t.Error("a pinned standalone job must survive GC(0)")
	}
	pinned.Unpin(pinned.Job, "job-pin")
	GC(0)
	if jobExists(t, dir, "job-pin") {
		t.Error("after unpin, GC(0) should remove the finished job")
	}
}

// TestClearFinished_SessionScopedStatus: only the target session's done/failed/stopped jobs+runs
// are removed; another session and a still-running job survive (acceptance 3).
func TestClearFinished_SessionScopedStatus(t *testing.T) {
	dir := jobsDirForTest(t)
	writeFinishedJobRec(t, dir, "jobA", "", "sessA", "done")
	writeRunForTest(t, WorkflowRun{RunID: "runA", SessionID: "sessA", Status: "failed", StartedAt: time.Now().Format(time.RFC3339)})
	writeFinishedJobRec(t, dir, "leafA", "runA", "sessA", "failed")
	writeFinishedJobRec(t, dir, "jobB", "", "sessB", "done") // other session — keep
	writeRunningJob(t, dir, "jobRun", "sessA")               // in-flight — keep

	pins, err := pinned.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	n, err := ClearFinished("sessA", pins)
	if err != nil {
		t.Fatalf("ClearFinished: %v", err)
	}
	if n != 2 { // jobA + runA (leafA reaped with its run, not counted)
		t.Errorf("removed = %d, want 2 (jobA + runA)", n)
	}
	if jobExists(t, dir, "jobA") {
		t.Error("sessA finished standalone job should be removed")
	}
	if runManifestExists(t, "runA") || jobExists(t, dir, "leafA") {
		t.Error("sessA finished run + its leaf should be removed")
	}
	if !jobExists(t, dir, "jobB") {
		t.Error("another session's finished job must be kept")
	}
	if !jobExists(t, dir, "jobRun") {
		t.Error("an in-flight (running) job must be kept")
	}
}

// TestClearFinished_SkipsPinned: a pinned run, a run with a pinned member, and a pinned standalone
// job all survive clear-finished — kept whole so no pinned leaf is orphaned (acceptance 3).
func TestClearFinished_SkipsPinned(t *testing.T) {
	dir := jobsDirForTest(t)
	now := time.Now().Format(time.RFC3339)
	writeRunForTest(t, WorkflowRun{RunID: "runP", SessionID: "sessA", Status: "done", StartedAt: now})
	writeFinishedJobRec(t, dir, "leafP", "runP", "sessA", "done")
	writeRunForTest(t, WorkflowRun{RunID: "runM", SessionID: "sessA", Status: "done", StartedAt: now})
	writeFinishedJobRec(t, dir, "leafM", "runM", "sessA", "done")
	writeFinishedJobRec(t, dir, "jobP", "", "sessA", "done")

	for _, p := range []struct {
		k  pinned.Kind
		id string
	}{{pinned.Run, "runP"}, {pinned.Job, "leafM"}, {pinned.Job, "jobP"}} {
		if err := pinned.Pin(p.k, p.id); err != nil {
			t.Fatalf("Pin %s/%s: %v", p.k, p.id, err)
		}
	}
	pins, _ := pinned.Snapshot()
	if _, err := ClearFinished("sessA", pins); err != nil {
		t.Fatalf("ClearFinished: %v", err)
	}
	if !runManifestExists(t, "runP") || !jobExists(t, dir, "leafP") {
		t.Error("a pinned run must be kept whole (manifest + leaf)")
	}
	if !runManifestExists(t, "runM") || !jobExists(t, dir, "leafM") {
		t.Error("a run with a pinned member must be kept whole")
	}
	if !jobExists(t, dir, "jobP") {
		t.Error("a pinned standalone job must be kept")
	}
}

func TestClearFinished_RequiresSession(t *testing.T) {
	jobsDirForTest(t)
	if _, err := ClearFinished("", pinned.Set{}); err == nil {
		t.Error("ClearFinished with empty session id should error")
	}
}

// TestDeleteJob: the board `d` path removes a job's files and clears its pin.
func TestDeleteJob(t *testing.T) {
	dir := jobsDirForTest(t)
	writeFinishedJobRec(t, dir, "00000000-0000-0000-0000-0000000000ab", "", "sessA", "done")
	id := "00000000-0000-0000-0000-0000000000ab"
	if err := pinned.Pin(pinned.Job, id); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if err := DeleteJob(id); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if jobExists(t, dir, id) {
		t.Error("DeleteJob should remove the job files even when pinned")
	}
	if s, _ := pinned.Snapshot(); s.Has(pinned.Job, id) {
		t.Error("DeleteJob should clear the pin marker")
	}
}

// TestPruneRuns_SparesRunWithPinnedMember: prune skips a dead-engine run that has a pinned leaf
// (PurgeRun would delete the pinned leaf with the run); unpinning lets prune remove it.
func TestPruneRuns_SparesRunWithPinnedMember(t *testing.T) {
	dir := jobsDirForTest(t)
	writeRunForTest(t, WorkflowRun{RunID: "run-x", Status: "done", StartedAt: time.Now().Format(time.RFC3339)})
	planFinishedRunMember(t, dir, "leaf-x", "run-x", time.Now())
	if err := pinned.Pin(pinned.Job, "leaf-x"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	n, err := PruneRuns()
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}
	if n != 0 {
		t.Errorf("PruneRuns removed %d, want 0 (run has a pinned member)", n)
	}
	if !runManifestExists(t, "run-x") || !jobExists(t, dir, "leaf-x") {
		t.Error("a run with a pinned member must be spared by prune")
	}
	if err := pinned.Unpin(pinned.Job, "leaf-x"); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	if n, _ := PruneRuns(); n != 1 {
		t.Errorf("after unpin, PruneRuns should remove the run (got %d)", n)
	}
}

// TestClearFinished_SkipsUnvalidatedID: a record whose cached id (JSON content, not the filename)
// fails id validation is left untouched — the delete path never joins an untrusted id.
func TestClearFinished_SkipsUnvalidatedID(t *testing.T) {
	dir := jobsDirForTest(t)
	fname := "00000000-0000-0000-0000-0000000000cd" // a valid-uuid filename
	writeJSONFile(t, filepath.Join(dir, fname+".json"), jobMeta{
		JobID: fname, PID: os.Getpid(), StartedAt: time.Now().Add(-time.Hour).Format(time.RFC3339),
		Status: "done", LeadSessionID: "sessA",
	})
	// The cached result carries a TRAVERSAL id in its content.
	writeJSONFile(t, filepath.Join(dir, fname+".result.json"), Result{
		OK: true, JobID: "../evil", Status: "done", LeadSessionID: "sessA",
	})
	pins, _ := pinned.Snapshot()
	if _, err := ClearFinished("sessA", pins); err != nil {
		t.Fatalf("ClearFinished: %v", err)
	}
	if !jobExists(t, dir, fname) {
		t.Error("a job whose cached id fails validation must be left untouched (no traversal, no bad-id delete)")
	}
}
