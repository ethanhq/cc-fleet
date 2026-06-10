//go:build !windows

package subagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

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
	res := Run(context.Background(), Request{Vendor: "glm", Prompt: "hi", JSON: true, LeadSessionID: "lead-run-1"})
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

	res := Run(context.Background(), Request{Vendor: "glm", Prompt: "hi", JSON: true})
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

	res := Run(context.Background(), Request{Vendor: "glm", Prompt: "hi", JSON: true, LeadSessionID: "explicit-lead"})
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

// TestRun_SyncSlimRegistrationFailureLeavesNoOrphans: a slim sync run whose board
// registration fails (writeMeta error) must still return the answer to the caller, yet
// leave NO orphan on disk — no .result.json (finalize is skipped without a backing meta)
// and no .slimprompt sidecar (reaped after the child exits, since GC keys on the absent
// meta and would never find it).
func TestRun_SyncSlimRegistrationFailureLeavesNoOrphans(t *testing.T) {
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

	// Keep the slim profile effective (the fake binary has no real --version): pin the
	// resolved version at the floor so a sidecar is actually written before registration.
	origVer := resolveBinaryPathVersion
	resolveBinaryPathVersion = func(*fingerprint.Fingerprint) (string, string, error) {
		return fakeClaude, SlimVersionFloor, nil
	}
	t.Cleanup(func() { resolveBinaryPathVersion = origVer })

	// Force the board registration to fail AFTER the slim sidecar was written.
	origWrite := writeMetaFn
	writeMetaFn = func(string, jobMeta) error { return os.ErrPermission }
	t.Cleanup(func() { writeMetaFn = origWrite })

	res := Run(context.Background(), Request{Vendor: "glm", Prompt: "hi", JSON: true, PromptProfile: ProfileSlim})
	if !res.OK || res.Result != "SUBAGENT_SMOKE_OK=42" {
		t.Fatalf("a failed registration must not change the returned Result: %+v", res)
	}

	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".result.json") {
			t.Fatalf("registration failed but a .result.json orphan was written: %s", name)
		}
		if strings.HasSuffix(name, ".slimprompt") {
			t.Fatalf("registration failed but the slim sidecar orphaned (no meta → GC can't find it): %s", name)
		}
		if strings.HasSuffix(name, ".json") {
			t.Fatalf("registration failed but a meta survived: %s", name)
		}
	}
}
