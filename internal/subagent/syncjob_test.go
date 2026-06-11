package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Cross-platform sync-job board + processAlive guard tests. The fake-claude
// exec cases live in syncjob_unix_test.go.

// ----- a sync run is visible on the board, without leaking its answer -----

func TestRegisterAndFinalizeSyncJob(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	// Register a sync job. PID is THIS (alive) process; SettingsPath empty so the
	// board's StatusFor uses a bare liveness probe and sees it running.
	jobID := regSyncJob(Request{Provider: "glm", JSON: true, LeadSessionID: "lead-sync-1"}, "glm-4.6")
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

	// Finalize with a successful result whose answer text and structured_output
	// payload MUST NOT be persisted (the sync caller already received them —
	// key/answer-safety; the cache copies an explicit field allowlist).
	const answer = "SECRET-SYNC-ANSWER-42"
	const structuredPayload = "SECRET-STRUCTURED-PAYLOAD-7"
	finalizeSyncJob(jobID, Result{
		OK: true, Provider: "glm", Model: "glm-4.6", Result: answer,
		StructuredOutput: json.RawMessage(`{"secret":"` + structuredPayload + `"}`),
	})

	jobs, _ = ListJobs()
	if len(jobs) != 1 || jobs[0].Status != "done" {
		t.Fatalf("after finalize want exactly 1 done job: %+v", jobs)
	}
	if jobs[0].Result != "" {
		t.Fatalf("sync result cache must not carry the answer text: %q", jobs[0].Result)
	}
	if jobs[0].Provider != "glm" || jobs[0].StartedAt == "" {
		t.Fatalf("finalize should carry provider/started from meta: %+v", jobs[0])
	}
	if jobs[0].LeadSessionID != "lead-sync-1" {
		t.Fatalf("finalized sync job should retain lead_session_id: %+v", jobs[0])
	}
	// Neither the meta nor the cached result file may contain the answer or the
	// structured_output payload on disk (StructuredOutput is not in the
	// allowlist, so not even its key reaches the file).
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	for _, suffix := range []string{".json", ".result.json"} {
		data, _ := os.ReadFile(filepath.Join(dir, jobID+suffix))
		for _, leak := range []string{answer, "structured_output", structuredPayload} {
			if strings.Contains(string(data), leak) {
				t.Fatalf("%s leaked %q to disk", jobID+suffix, leak)
			}
		}
	}
}

// ----- processAlive PID-reuse guard via the reuseGuardArgv seam -----

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
	if processAlive(os.Getpid(), "/no/such/cc-fleet/profile-marker.json", "") {
		t.Fatal("a live pid whose cmdline is not our claude subagent must read as dead (reuse guard)")
	}
	// Empty SettingsPath (a sync job / legacy meta) degrades to a bare liveness probe.
	if !processAlive(os.Getpid(), "", "") {
		t.Fatal("empty settingsPath should degrade to a bare liveness probe = alive for a live pid")
	}
	// pid <= 0 is always dead.
	if processAlive(-1, "", "") {
		t.Fatal("pid <= 0 must be reported dead")
	}
}
