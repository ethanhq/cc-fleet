package subagent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// injectManifestRead makes the run-manifest read for runID fail with a transient (sharing-violation-
// shaped) error — persistently when failN < 0, or only the first failN reads then the real bytes —
// while every other read is served normally. It wires the sentinel into transientRead so readFileRetry
// treats it as the retryable class on unix, where the production isSharingViolation is always false.
// Returns a pointer to the manifest read count. Both seams are restored on cleanup.
func injectManifestRead(t *testing.T, runID string, failN int) *int {
	t.Helper()
	sentinel := errors.New("simulated ERROR_SHARING_VIOLATION")
	target := filepath.Join(runsDirName, runID+".json")
	realRead, realPred := osReadFile, transientRead
	calls := 0
	osReadFile = func(path string) ([]byte, error) {
		if strings.HasSuffix(path, target) {
			calls++
			if failN < 0 || calls <= failN {
				return nil, sentinel
			}
		}
		return realRead(path)
	}
	transientRead = func(err error) bool { return errors.Is(err, sentinel) || realPred(err) }
	t.Cleanup(func() { osReadFile, transientRead = realRead, realPred })
	return &calls
}

func newReadRetryEnv(t *testing.T) {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(xdg, "cc-fleet", jobsDirName), 0o700); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}
}

// TestReadRun_AbsorbsTransientSharingViolation: a transient-then-success manifest read is retried past
// the sharing-violation window (a) and returns the parsed manifest, not an error.
func TestReadRun_AbsorbsTransientSharingViolation(t *testing.T) {
	newReadRetryEnv(t)
	writeRunForTest(t, WorkflowRun{RunID: "run-transient", Name: "flaky", StartedAt: nowRFC3339(), Status: "running"})

	calls := injectManifestRead(t, "run-transient", 2) // fail twice, then serve the real bytes

	run, err := ReadRun("run-transient")
	if err != nil {
		t.Fatalf("ReadRun should absorb a transient sharing violation, got %v", err)
	}
	if run.RunID != "run-transient" || run.Name != "flaky" {
		t.Fatalf("ReadRun returned the wrong manifest: %+v", run)
	}
	if *calls != 3 {
		t.Fatalf("manifest read attempts = %d, want 3 (two retries then success)", *calls)
	}
}

// TestReadRun_PersistentReadFailIsUnreadable: a persistent transient read surfaces as
// errRunManifestUnreadable — NOT os.ErrNotExist — so destructive/liveness callers can fail closed.
func TestReadRun_PersistentReadFailIsUnreadable(t *testing.T) {
	newReadRetryEnv(t)
	writeRunForTest(t, WorkflowRun{RunID: "run-io", StartedAt: nowRFC3339(), Status: "running"})
	injectManifestRead(t, "run-io", -1)

	_, err := ReadRun("run-io")
	if !errors.Is(err, errRunManifestUnreadable) {
		t.Fatalf("want errRunManifestUnreadable, got %v", err)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a transient read failure must not read as ErrNotExist: %v", err)
	}
}

// TestWaitEngineStopped_TransientReadNotStopped: a persistent transient manifest read does NOT report
// the engine stopped (b) — it times out into the caller's fail-closed path instead of declaring death.
func TestWaitEngineStopped_TransientReadNotStopped(t *testing.T) {
	newReadRetryEnv(t)
	writeRunForTest(t, WorkflowRun{RunID: "run-wait", StartedAt: nowRFC3339(), Status: "running", EnginePID: os.Getpid()})
	injectManifestRead(t, "run-wait", -1)

	if WaitEngineStopped("run-wait", 60*time.Millisecond) {
		t.Fatal("WaitEngineStopped must NOT report stopped on a persistent transient read (fail closed)")
	}
}

// TestWaitEngineStopped_MissingIsStopped: a genuinely-absent manifest (os.ErrNotExist) DOES report
// stopped — the one read-error that means gone.
func TestWaitEngineStopped_MissingIsStopped(t *testing.T) {
	newReadRetryEnv(t)
	if !WaitEngineStopped("run-gone", 60*time.Millisecond) {
		t.Fatal("WaitEngineStopped should report stopped when the manifest is genuinely absent")
	}
}

// TestPurgeRun_TransientReadFailsClosed: PurgeRun on a persistent transient manifest read deletes
// NOTHING and returns the unreadable error (c); on a genuinely-absent manifest it proceeds (nil).
func TestPurgeRun_TransientReadFailsClosed(t *testing.T) {
	newReadRetryEnv(t)
	writeRunForTest(t, WorkflowRun{RunID: "run-purge", StartedAt: nowRFC3339(), Status: "running", EnginePID: os.Getpid()})
	injectManifestRead(t, "run-purge", -1)

	if err := PurgeRun("run-purge"); !errors.Is(err, errRunManifestUnreadable) {
		t.Fatalf("PurgeRun on a transient read must fail closed with errRunManifestUnreadable, got %v", err)
	}
	if !runManifestExists(t, "run-purge") {
		t.Fatal("PurgeRun failed closed but still deleted the manifest")
	}

	// A genuinely-absent run proceeds (nothing to delete) — the accumulated-junk path stays open.
	if err := PurgeRun("run-absent"); err != nil {
		t.Fatalf("PurgeRun of an absent run should proceed (nil), got %v", err)
	}
}

// TestGC_SparesRunWithUnreadableManifest: a reclaim scan spares a memberless run whose manifest read
// persistently errors (d) — PurgeRun fails closed, so GC never reaps under a possibly-live engine.
func TestGC_SparesRunWithUnreadableManifest(t *testing.T) {
	newReadRetryEnv(t)
	// An OLD, memberless manifest that, read successfully, would be pruned as an aged orphan.
	writeRunForTest(t, WorkflowRun{RunID: "run-gc", StartedAt: "2000-01-01T00:00:00Z", Status: "running"})
	injectManifestRead(t, "run-gc", -1)

	if out := GC(0); !out.OK {
		t.Fatalf("GC(0) failed: %s", out.ErrorMsg)
	}
	if !runManifestExists(t, "run-gc") {
		t.Fatal("GC must spare a run whose manifest read persistently errors, not reclaim it")
	}
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
