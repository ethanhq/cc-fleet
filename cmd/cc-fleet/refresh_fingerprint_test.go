package main

import (
	"os"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/fingerprint"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

const fakeClaudeBin = "/root/.local/share/claude/versions/2.1.150"

// probeArgv builds an Agent probe command line for team.
func probeArgv(team string) []string {
	return []string{fakeClaudeBin, "--team-name", team, "--agent-type", "general-purpose", "-p"}
}

// restoreSeams snapshots + restores the refresh-fingerprint seams around a test.
func restoreSeams(t *testing.T) {
	t.Helper()
	o1, o2, o3, o4, o5 := listProcesses, procStartToken, readArgv, captureFromPid, saveFingerprint
	t.Cleanup(func() {
		listProcesses, procStartToken, readArgv, captureFromPid, saveFingerprint = o1, o2, o3, o4, o5
	})
}

// silenceStdio swaps os.Stdout/os.Stderr to /dev/null for the test (reportOK/
// reportErr write there) and restores after.
func silenceStdio(t *testing.T) {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	oStdout, oStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devnull, devnull
	t.Cleanup(func() {
		os.Stdout, os.Stderr = oStdout, oStderr
		_ = devnull.Close()
	})
}

// TestFindProbePid_ExactTokenMatch_NoRegexWiden: a team name with a regex
// metacharacter ("a.b") must match ONLY the literal probe, never the "aXb"
// decoy a `pgrep -f` substring/regex would catch, and never the lead (no
// --agent-type general-purpose).
func TestFindProbePid_ExactTokenMatch_NoRegexWiden(t *testing.T) {
	restoreSeams(t)
	listProcesses = func() ([]procintrospect.Process, error) {
		return []procintrospect.Process{
			{PID: 100, Argv: probeArgv("a.b")},                                                                   // the real probe
			{PID: 101, Argv: probeArgv("aXb")},                                                                   // regex-`.` decoy
			{PID: 102, Argv: []string{fakeClaudeBin, "--team-name", "a.b"}},                                      // lead (no agent-type)
			{PID: 103, Argv: []string{"/usr/bin/grep", "--team-name", "a.b", "--agent-type", "general-purpose"}}, // non-claude
		}, nil
	}
	procStartToken = func(int) (string, bool) { return "tok", true }

	pid, start, err := findProbePid("a.b")
	if err != nil {
		t.Fatalf("findProbePid: %v", err)
	}
	if pid != 100 {
		t.Fatalf("findProbePid = %d, want 100 (exact-token match only)", pid)
	}
	if start != "tok" {
		t.Fatalf("start token = %q, want tok", start)
	}
}

// TestFindProbePid_MultipleExactMatches_Refuses is the no-first-PID-wins rule:
// two processes that BOTH match exactly must refuse, not capture a guess.
func TestFindProbePid_MultipleExactMatches_Refuses(t *testing.T) {
	restoreSeams(t)
	listProcesses = func() ([]procintrospect.Process, error) {
		return []procintrospect.Process{
			{PID: 200, Argv: probeArgv("team")},
			{PID: 201, Argv: probeArgv("team")},
		}, nil
	}
	procStartToken = func(int) (string, bool) { return "tok", true }

	if _, _, err := findProbePid("team"); err == nil {
		t.Fatal("findProbePid with 2 exact matches must refuse (no first-PID-wins), got nil error")
	}
}

// TestRunRefreshFingerprint_StartTokenChanged_NoSave is the PID-reuse gate: when
// the selected probe's start token changes between selection and the pre-save
// re-validation, NO fingerprint is saved.
func TestRunRefreshFingerprint_StartTokenChanged_NoSave(t *testing.T) {
	restoreSeams(t)
	silenceStdio(t)
	t.Setenv("HOME", t.TempDir())

	listProcesses = func() ([]procintrospect.Process, error) {
		return []procintrospect.Process{{PID: 300, Argv: probeArgv("probe")}}, nil
	}
	// First call (selection) → T1; every later call (revalidate) → T2 (recycled).
	calls := 0
	procStartToken = func(int) (string, bool) {
		calls++
		if calls == 1 {
			return "T1", true
		}
		return "T2", true
	}
	readArgv = func(int) ([]string, error) { return probeArgv("probe"), nil }
	captureFromPid = func(int) (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{CCVersion: "2.1.150", BinaryPath: fakeClaudeBin, CapturedAt: time.Now()}, nil
	}
	saved := false
	saveFingerprint = func(*fingerprint.Fingerprint) error { saved = true; return nil }

	if err := runRefreshFingerprint("probe", false); err == nil {
		t.Fatal("expected error when start token changed between selection and capture")
	}
	if saved {
		t.Fatal("fingerprint was SAVED despite a start-token change (stale-PID capture gap)")
	}
}

// TestRunRefreshFingerprint_CommandChanged_NoSave: same gate via the argv re-check
// — if the PID no longer carries the probe argv at re-validation, NO save.
func TestRunRefreshFingerprint_CommandChanged_NoSave(t *testing.T) {
	restoreSeams(t)
	silenceStdio(t)
	t.Setenv("HOME", t.TempDir())

	listProcesses = func() ([]procintrospect.Process, error) {
		return []procintrospect.Process{{PID: 400, Argv: probeArgv("probe")}}, nil
	}
	procStartToken = func(int) (string, bool) { return "T1", true } // token unchanged
	// At revalidation the PID is now an unrelated process (recycled).
	readArgv = func(int) ([]string, error) { return []string{"/usr/bin/bash", "-lc", "sleep 1"}, nil }
	captureFromPid = func(int) (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{CCVersion: "2.1.150", BinaryPath: fakeClaudeBin, CapturedAt: time.Now()}, nil
	}
	saved := false
	saveFingerprint = func(*fingerprint.Fingerprint) error { saved = true; return nil }

	if err := runRefreshFingerprint("probe", false); err == nil {
		t.Fatal("expected error when the probe argv changed between selection and capture")
	}
	if saved {
		t.Fatal("fingerprint was SAVED despite the probe argv changing")
	}
}

// TestRunRefreshFingerprint_HappyPath_Saves is the control: selection + capture
// + re-validation all consistent → the fingerprint IS saved.
func TestRunRefreshFingerprint_HappyPath_Saves(t *testing.T) {
	restoreSeams(t)
	silenceStdio(t)
	t.Setenv("HOME", t.TempDir())

	listProcesses = func() ([]procintrospect.Process, error) {
		return []procintrospect.Process{{PID: 500, Argv: probeArgv("probe")}}, nil
	}
	procStartToken = func(int) (string, bool) { return "T1", true }
	readArgv = func(int) ([]string, error) { return probeArgv("probe"), nil }
	captureFromPid = func(int) (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{CCVersion: "2.1.150", BinaryPath: fakeClaudeBin, CapturedAt: time.Now()}, nil
	}
	saved := false
	saveFingerprint = func(*fingerprint.Fingerprint) error { saved = true; return nil }

	if err := runRefreshFingerprint("probe", false); err != nil {
		t.Fatalf("happy path returned error: %v", err)
	}
	if !saved {
		t.Fatal("happy path should have saved the fingerprint")
	}
}

// TestRunRefreshFingerprint_InvalidTeamName_Rejected: the typed CLI boundary
// rejects an unsafe team name before any process enumeration.
func TestRunRefreshFingerprint_InvalidTeamName_Rejected(t *testing.T) {
	restoreSeams(t)
	silenceStdio(t)

	enumerated := false
	listProcesses = func() ([]procintrospect.Process, error) {
		enumerated = true
		return nil, nil
	}

	if err := runRefreshFingerprint("../evil", false); err == nil {
		t.Fatal("expected error for an unsafe --probe-team name")
	}
	if enumerated {
		t.Fatal("process enumeration ran despite an invalid team name (typed boundary bypassed)")
	}
}
