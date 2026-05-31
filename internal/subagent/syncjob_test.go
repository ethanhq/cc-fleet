package subagent

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

// ----- a sync run is visible on the board, without leaking its answer -----

func TestRegisterAndFinalizeSyncJob(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	// Register a sync job. PID is THIS (alive) process; SettingsPath empty so the
	// board's StatusFor uses a bare kill(0) and sees it running.
	jobID := registerSyncJob(Request{Vendor: "glm", JSON: true, LeadSessionID: "lead-sync-1"}, "glm-4.6")
	if jobID == "" {
		t.Fatal("registerSyncJob returned an empty job id")
	}
	jobs, err := ListJobs()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ListJobs after register: err=%v n=%d", err, len(jobs))
	}
	if jobs[0].JobID != jobID || jobs[0].Status != "running" {
		t.Fatalf("sync job should be visible as running: %+v", jobs[0])
	}
	if jobs[0].LeadSessionID != "lead-sync-1" {
		t.Fatalf("sync job should carry lead_session_id, got %+v", jobs[0])
	}

	// Finalize with a successful result whose answer text MUST NOT be persisted
	// (the sync caller already received it on stdout — key/answer-safety).
	const answer = "SECRET-SYNC-ANSWER-42"
	finalizeSyncJob(jobID, Result{OK: true, Vendor: "glm", Model: "glm-4.6", Result: answer})

	jobs, _ = ListJobs()
	if len(jobs) != 1 || jobs[0].Status != "done" {
		t.Fatalf("after finalize want exactly 1 done job: %+v", jobs)
	}
	if jobs[0].Result != "" {
		t.Fatalf("sync result cache must not carry the answer text: %q", jobs[0].Result)
	}
	if jobs[0].Vendor != "glm" || jobs[0].StartedAt == "" {
		t.Fatalf("finalize should carry vendor/started from meta: %+v", jobs[0])
	}
	if jobs[0].LeadSessionID != "lead-sync-1" {
		t.Fatalf("finalized sync job should retain lead_session_id: %+v", jobs[0])
	}
	// Neither the meta nor the cached result file may contain the answer on disk.
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	for _, suffix := range []string{".json", ".result.json"} {
		data, _ := os.ReadFile(filepath.Join(dir, jobID+suffix))
		if strings.Contains(string(data), answer) {
			t.Fatalf("%s leaked the answer text to disk", jobID+suffix)
		}
	}
}

func TestRun_SyncRecordsBoardJobNoAnswerLeak(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalVendors(t, xdg)

	fakeClaude := writeFakeBin(t, "#!/bin/sh\nprintf '%s' '"+smokeSuccessJSON+"'\nexit 0\n")
	orig := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: fakeClaude}, nil
	}
	t.Cleanup(func() { loadFP = orig })

	// A SYNCHRONOUS run (no Background). The caller still gets the answer inline.
	res := Run(Request{Vendor: "glm", Prompt: "hi", JSON: true, LeadSessionID: "lead-run-1"})
	if !res.OK || res.Result != "SUBAGENT_SMOKE_OK=42" {
		t.Fatalf("sync run should return the answer to the caller: %+v", res)
	}
	if res.LeadSessionID != "lead-run-1" {
		t.Fatalf("sync Result should carry lead_session_id: %+v", res)
	}
	// The returned envelope is unchanged — no JobID stamped, so CLI output parity
	// holds (board bookkeeping is a pure side channel).
	if res.JobID != "" {
		t.Fatalf("sync Result must not carry a JobID: %q", res.JobID)
	}

	// The board now sees the finished sync job as done — WITHOUT the answer text.
	jobs, err := ListJobs()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ListJobs: err=%v n=%d", err, len(jobs))
	}
	if jobs[0].Status != "done" || jobs[0].Vendor != "glm" {
		t.Fatalf("finished sync job wrong: %+v", jobs[0])
	}
	if jobs[0].LeadSessionID != "lead-run-1" {
		t.Fatalf("board job should retain lead_session_id: %+v", jobs[0])
	}
	if jobs[0].Result != "" {
		t.Fatalf("board job must not expose the answer: %q", jobs[0].Result)
	}
	// The answer text never reaches any job file on disk for a sync run.
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if strings.Contains(string(data), "SUBAGENT_SMOKE_OK=42") {
			t.Fatalf("sync job file %s leaked the answer to disk", e.Name())
		}
	}
}

func TestRun_SyncAutoDetectsLeadSession(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalVendors(t, xdg)

	fakeClaude := writeFakeBin(t, "#!/bin/sh\nprintf '%s' '"+smokeSuccessJSON+"'\nexit 0\n")
	origFP := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: fakeClaude}, nil
	}
	t.Cleanup(func() { loadFP = origFP })

	origDetect := detectLeadSession
	detectLeadSession = func() string { return "auto-lead-session" }
	t.Cleanup(func() { detectLeadSession = origDetect })

	res := Run(Request{Vendor: "glm", Prompt: "hi", JSON: true})
	if !res.OK {
		t.Fatalf("Run failed: %+v", res)
	}
	if res.LeadSessionID != "auto-lead-session" {
		t.Fatalf("sync Result LeadSessionID = %q, want auto-lead-session", res.LeadSessionID)
	}
	jobs, err := ListJobs()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ListJobs: err=%v n=%d", err, len(jobs))
	}
	if jobs[0].LeadSessionID != "auto-lead-session" {
		t.Fatalf("board job LeadSessionID = %q, want auto-lead-session", jobs[0].LeadSessionID)
	}
}

func TestRun_ExplicitLeadSessionOverridesAutoDetect(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalVendors(t, xdg)

	fakeClaude := writeFakeBin(t, "#!/bin/sh\nprintf '%s' '"+smokeSuccessJSON+"'\nexit 0\n")
	origFP := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: fakeClaude}, nil
	}
	t.Cleanup(func() { loadFP = origFP })

	called := false
	origDetect := detectLeadSession
	detectLeadSession = func() string {
		called = true
		return "auto-lead-session"
	}
	t.Cleanup(func() { detectLeadSession = origDetect })

	res := Run(Request{Vendor: "glm", Prompt: "hi", JSON: true, LeadSessionID: "explicit-lead"})
	if !res.OK {
		t.Fatalf("Run failed: %+v", res)
	}
	if called {
		t.Fatal("detectLeadSession should not run when LeadSessionID is explicit")
	}
	if res.LeadSessionID != "explicit-lead" {
		t.Fatalf("LeadSessionID = %q, want explicit-lead", res.LeadSessionID)
	}
}

// ----- processAlive PID-reuse guard via /proc/<pid>/cmdline -----

func TestProcessAlive_CmdlineReuseGuard(t *testing.T) {
	// cmdlineIsClaudeJob reads argv through the reuseGuardArgv seam (linux /proc
	// OR darwin ps), so the matcher BEHAVIOR is tested platform-independently by
	// stubbing the argv each case returns. The real linux /proc reader is
	// additionally exercised below; the darwin ps reader is covered by an e2e case.
	const prof = "/root/.config/cc-fleet/profiles/glm.settings.json"
	// The real version-pinned binary path — note the "/claude/" segment and a
	// hash/version basename (basename != "claude", so the path segment is
	// load-bearing).
	const claudeBin = "/root/.local/share/claude/versions/2.1.150"

	origSeam := reuseGuardArgv
	t.Cleanup(func() { reuseGuardArgv = origSeam })
	stub := func(argv []string) { reuseGuardArgv = func(int) ([]string, bool) { return argv, true } }

	// 1. OUR claude child for this job: claude path + this job's --settings → ours.
	stub([]string{claudeBin, "--dangerously-skip-permissions", "--settings", prof, "--model", "glm-4.6", "-p"})
	if !cmdlineIsClaudeJob(1001, prof) {
		t.Fatal("claude binary + this job's --settings should be recognized as our job")
	}
	// 2. A recycled pid now held by an unrelated process → NOT ours.
	stub([]string{"/usr/bin/bash", "-lc", "sleep 1000"})
	if cmdlineIsClaudeJob(1002, prof) {
		t.Fatal("an unrelated recycled pid must not look like our claude job")
	}
	// 3. A claude child for a DIFFERENT job (other --settings) → not THIS job.
	stub([]string{claudeBin, "--settings", "/root/.config/cc-fleet/profiles/kimi.settings.json", "-p"})
	if cmdlineIsClaudeJob(1003, prof) {
		t.Fatal("claude with a different --settings is not THIS job (--model alone is too loose)")
	}
	// 4. Unreadable cmdline (proc race / just-exited) → trust the kill(0) liveness.
	reuseGuardArgv = func(int) ([]string, bool) { return nil, false }
	if !cmdlineIsClaudeJob(424242, prof) {
		t.Fatal("an unreadable cmdline should fall back to alive (no flaky false-dead)")
	}

	// Linux: also exercise the REAL /proc reader via the procRoot seam, proving
	// platformReuseGuardArgv's /proc path still parses NUL-separated cmdline.
	if runtime.GOOS == "linux" {
		reuseGuardArgv = origSeam
		root := t.TempDir()
		origRoot := procRoot
		procRoot = root
		t.Cleanup(func() { procRoot = origRoot })
		d := filepath.Join(root, strconv.Itoa(1001))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		argv := strings.Join([]string{claudeBin, "--settings", prof, "-p"}, "\x00")
		if err := os.WriteFile(filepath.Join(d, "cmdline"), []byte(argv), 0o644); err != nil {
			t.Fatal(err)
		}
		if !cmdlineIsClaudeJob(1001, prof) {
			t.Fatal("linux /proc reader: matching cmdline should be recognized as our job")
		}
		if cmdlineIsClaudeJob(1001, "/some/other.json") {
			t.Fatal("linux /proc reader: settings mismatch must not match (pid reuse)")
		}
	}
}

func TestProcessAlive_LivePidWrongCmdlineReadsDead(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the /proc cmdline reuse guard is linux-only")
	}
	// A live pid (this test process) whose cmdline is obviously NOT our claude
	// job: this is exactly the recycled-pid footgun the guard exists to catch.
	if processAlive(os.Getpid(), "/no/such/cc-fleet/profile-marker.json") {
		t.Fatal("a live pid whose cmdline is not our claude subagent must read as dead (reuse guard)")
	}
	// Empty SettingsPath (a sync job / legacy meta) degrades to a bare kill(0).
	if !processAlive(os.Getpid(), "") {
		t.Fatal("empty settingsPath should degrade to a bare kill(0) = alive for a live pid")
	}
	// pid <= 0 is always dead.
	if processAlive(-1, "") {
		t.Fatal("pid <= 0 must be reported dead")
	}
}
