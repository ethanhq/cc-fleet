package subagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeJournalForTest drops a journal sidecar (runs/<id>.journal) on disk so the GC /
// purge / orphan-sweep paths can be exercised directly.
func writeJournalForTest(t *testing.T, runID, content string) {
	t.Helper()
	dir, err := runsDir()
	if err != nil {
		t.Fatalf("runsDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir runs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, runID+".journal"), []byte(content), 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
}

func journalExists(t *testing.T, runID string) bool {
	t.Helper()
	dir, err := runsDir()
	if err != nil {
		t.Fatalf("runsDir: %v", err)
	}
	_, statErr := os.Stat(filepath.Join(dir, runID+".journal"))
	return statErr == nil
}

// TestGC_ReapsRunJournalWithManifest: an aged-out memberless run's journal sidecar is
// reaped as one unit with its manifest.
func TestGC_ReapsRunJournalWithManifest(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	old := time.Now().Add(-72 * time.Hour).Format(time.RFC3339)
	writeRunForTest(t, WorkflowRun{RunID: "run-j", StartedAt: old, UpdatedAt: old, Status: "failed"})
	writeJournalForTest(t, "run-j", `{"key":"k","result":"r"}`+"\n")

	if out := GC(0); !out.OK {
		t.Fatalf("GC: %s", out.ErrorMsg)
	}
	if runManifestExists(t, "run-j") {
		t.Error("aged manifest should be pruned")
	}
	if journalExists(t, "run-j") {
		t.Error("journal sidecar must be reaped with its manifest")
	}
}

// TestGC_ResumingRunProtectedByUpdatedAt: a run with an OLD StartedAt but a FRESH
// UpdatedAt heartbeat — an actively resuming run before its first leaf registers a
// member — survives a periodic GC, and so does its journal; a fully-stale memberless
// run (old StartedAt AND old UpdatedAt) is pruned.
func TestGC_ResumingRunProtectedByUpdatedAt(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	old := time.Now().Add(-72 * time.Hour).Format(time.RFC3339)
	writeRunForTest(t, WorkflowRun{RunID: "run-resume", StartedAt: old, UpdatedAt: time.Now().UTC().Format(time.RFC3339), Status: "running"})
	writeJournalForTest(t, "run-resume", `{"key":"k","result":"r"}`+"\n")
	writeRunForTest(t, WorkflowRun{RunID: "run-stale", StartedAt: old, UpdatedAt: old, Status: "failed"})
	writeJournalForTest(t, "run-stale", `{"key":"k","result":"r"}`+"\n")

	if out := GC(24 * time.Hour); !out.OK {
		t.Fatalf("GC: %s", out.ErrorMsg)
	}
	if !runManifestExists(t, "run-resume") {
		t.Error("a fresh-UpdatedAt run must survive periodic GC (resume protection)")
	}
	if !journalExists(t, "run-resume") {
		t.Error("the protected run's journal must survive too")
	}
	if runManifestExists(t, "run-stale") {
		t.Error("a fully-stale memberless run must be pruned")
	}
	if journalExists(t, "run-stale") {
		t.Error("the stale run's journal must be reaped")
	}
}

// TestGC_SweepsOrphanJournal: a journal whose manifest is already gone (a crash between
// the two removes) is swept rather than leaked.
func TestGC_SweepsOrphanJournal(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	writeJournalForTest(t, "orphan", `{"key":"k","result":"r"}`+"\n") // creates runs/, no manifest
	if out := GC(0); !out.OK {
		t.Fatalf("GC: %s", out.ErrorMsg)
	}
	if journalExists(t, "orphan") {
		t.Error("an orphan journal (no manifest) must be swept")
	}
}

// TestGC_FreshOrphanKeptAgedOrphanSwept: periodic GC KEEPS a fresh orphan journal (it
// may belong to a run another process is launching, whose manifest write hasn't landed)
// but sweeps an aged one — symmetric with the manifest recency rule.
func TestGC_FreshOrphanKeptAgedOrphanSwept(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	writeJournalForTest(t, "fresh-orphan", `{"key":"k","result":"r"}`+"\n")
	writeJournalForTest(t, "aged-orphan", `{"key":"k","result":"r"}`+"\n")
	dir, err := runsDir()
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "aged-orphan.journal"), old, old); err != nil {
		t.Fatal(err)
	}

	if out := GC(24 * time.Hour); !out.OK {
		t.Fatalf("GC: %s", out.ErrorMsg)
	}
	if !journalExists(t, "fresh-orphan") {
		t.Error("a fresh orphan journal must be KEPT by periodic GC (may belong to an active run)")
	}
	if journalExists(t, "aged-orphan") {
		t.Error("an aged orphan journal must be swept by periodic GC")
	}
}

// TestPurge_ReapsJournalAndOrphan: uninstall purge reaps a memberless run's journal and
// sweeps orphan sidecars (so the runs dir can be removed).
func TestPurge_ReapsJournalAndOrphan(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	writeRunForTest(t, WorkflowRun{RunID: "run-p", StartedAt: time.Now().UTC().Format(time.RFC3339), Status: "done"})
	writeJournalForTest(t, "run-p", `{"key":"k","result":"r"}`+"\n")
	writeJournalForTest(t, "orphan-p", `{"key":"k","result":"r"}`+"\n")

	if _, _, _, err := PurgeJobs(); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if runManifestExists(t, "run-p") {
		t.Error("purge should remove a memberless manifest")
	}
	if journalExists(t, "run-p") {
		t.Error("purge should reap the run journal")
	}
	if journalExists(t, "orphan-p") {
		t.Error("purge should sweep the orphan journal")
	}
}
