package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

// TestSpawn_StalePath_RecoversViaCcver: a fingerprint whose cached BinaryPath no
// longer exists (CC upgrade GC'd the version-pinned binary) must NOT fail with
// FINGERPRINT_STALE. ResolveBinaryPath drops the dead path and resolves the live
// binary via ccver, so the spawn recovers and uses the resolved path — not the
// stale one.
func TestSpawn_StalePath_RecoversViaCcver(t *testing.T) {
	f := newFixture(t)
	f.startProviderServer()
	f.writeProvidersTOML("")
	live := f.installFakeClaude() // the binary ccver will resolve to

	// Seed a fingerprint pointing at a file we then delete (the GC'd path).
	binDir := t.TempDir()
	gonePath := filepath.Join(binDir, "claude-gone")
	if err := os.WriteFile(gonePath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	fp := &fingerprint.Fingerprint{
		CCVersion:  "2.1.150",
		CapturedAt: time.Date(2026, 5, 24, 6, 0, 0, 0, time.UTC),
		BinaryPath: gonePath,
		FlagsTemplate: []string{
			"--agent-id", "{name}@{team}",
			"--dangerously-skip-permissions",
		},
	}
	if err := fingerprint.Save(fp); err != nil {
		t.Fatalf("Save fingerprint: %v", err)
	}
	if err := os.Remove(gonePath); err != nil {
		t.Fatalf("delete fake binary: %v", err)
	}

	res := Spawn(Request{
		Provider:  "deepseek",
		AgentName: "worker-1",
		Team:      "myproj",
		AutoTeam:  true,
		Probe:     false,
	})

	if res.ErrorCode == ErrCodeFingerprintStale {
		t.Fatalf("stale cached path must recover via ccver, got FINGERPRINT_STALE: %s", res.ErrorMsg)
	}
	if !res.OK {
		t.Fatalf("spawn should recover and succeed, got %s: %s", res.ErrorCode, res.ErrorMsg)
	}
	// The split command must use the live (resolved) binary, never the gone one.
	joined := strings.Join(f.splitWindowCall(), " ")
	if strings.Contains(joined, gonePath) {
		t.Fatalf("spawn used the stale binary path %q: %s", gonePath, joined)
	}
	if !strings.Contains(joined, live) {
		t.Fatalf("spawn did not use the ccver-resolved binary %q: %s", live, joined)
	}
}

// TestSpawn_NoBinaryAnywhere_StaleBeforeSideEffects: when NO claude binary is
// resolvable at all (empty cached path AND nothing on PATH / in the versions
// dir), spawn returns FINGERPRINT_STALE BEFORE any side effect — no SplitWindow,
// no team directory. A stale cache must never leave a half-built pane behind.
func TestSpawn_NoBinaryAnywhere_StaleBeforeSideEffects(t *testing.T) {
	f := newFixture(t)
	f.startProviderServer()
	f.writeProvidersTOML("")
	// Strip claude (and tmux — unreached) from PATH; HOME is the fixture's temp
	// dir with no ~/.local/share/claude/versions, so ccver.Detect finds nothing.
	t.Setenv("PATH", t.TempDir())

	fp := &fingerprint.Fingerprint{
		CCVersion:  "2.1.150",
		CapturedAt: time.Date(2026, 5, 24, 6, 0, 0, 0, time.UTC),
		BinaryPath: "",
	}
	if err := fingerprint.Save(fp); err != nil {
		t.Fatalf("Save fingerprint: %v", err)
	}
	res := Spawn(Request{
		Provider:  "deepseek",
		AgentName: "worker-1",
		Team:      "myproj",
		AutoTeam:  true,
		Probe:     false,
	})
	if res.OK {
		t.Fatalf("Spawn unexpectedly succeeded with no resolvable binary: res=%+v", res)
	}
	if res.ErrorCode != ErrCodeFingerprintStale {
		t.Fatalf("ErrorCode = %q, want %q", res.ErrorCode, ErrCodeFingerprintStale)
	}
	// No side effects: gate ran before SplitWindow and before EnsureTeamDir.
	for _, c := range f.readMockArgs() {
		if len(c) > 0 && c[0] == "split-window" {
			t.Fatalf("spawn ran split-window despite no resolvable binary; calls=%v", f.readMockArgs())
		}
	}
	dir, _ := TeamDir("myproj")
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("team dir %s was created despite no resolvable binary", dir)
	}
}

// TestSpawn_SettleFails_RollsBack: when the live CC is newer than the recipe and
// --verify is on, a teammate that exits during startup must roll the whole spawn
// back (pane killed, member removed) and surface SPAWN_DID_NOT_SETTLE.
func TestSpawn_SettleFails_RollsBack(t *testing.T) {
	f := newFixture(t)
	f.startProviderServer()
	f.writeProvidersTOML("")
	f.installFakeClaude() // live CC reports 2.1.150

	// A user fingerprint OLDER than the live CC → CurrentVersionExceedsRecipe
	// true → the settle check is gated ON.
	fp := &fingerprint.Fingerprint{
		CCVersion:     "2.1.100",
		CapturedAt:    time.Date(2026, 5, 24, 6, 0, 0, 0, time.UTC),
		BinaryPath:    "", // ResolveBinaryPath → ccver → the fake claude
		FlagsTemplate: []string{"--agent-id", "{name}@{team}", "--agent-name", "{name}"},
	}
	if err := fingerprint.Save(fp); err != nil {
		t.Fatalf("Save fingerprint: %v", err)
	}

	// Stub the settle check: the teammate did NOT come up.
	orig := settleOK
	settleOK = func(string, string) bool { return false }
	t.Cleanup(func() { settleOK = orig })

	res := Spawn(Request{
		Provider:  "deepseek",
		AgentName: "worker-1",
		Team:      "myproj",
		AutoTeam:  true,
		Probe:     false,
		Verify:    true,
	})

	if res.ErrorCode != ErrCodeSpawnDidNotSettle {
		t.Fatalf("want SPAWN_DID_NOT_SETTLE, got %s: %s", res.ErrorCode, res.ErrorMsg)
	}
	// Rollback killed the pane.
	sawKill := false
	for _, c := range f.readMockArgs() {
		if len(c) > 0 && c[0] == "kill-pane" {
			sawKill = true
		}
	}
	if !sawKill {
		t.Fatalf("settle failure must kill the pane; tmux calls=%v", f.readMockArgs())
	}
	// Rollback removed the member from team config. The team dir still exists
	// (AutoTeam created it) and must load with zero members — don't let a load
	// error silently skip this check (a stale entry would block a retry).
	tc, err := LoadTeamConfig("myproj")
	if err != nil {
		t.Fatalf("LoadTeamConfig after rollback: %v", err)
	}
	if len(tc.Members) != 0 {
		t.Fatalf("members after settle rollback = %d, want 0; members=%+v",
			len(tc.Members), tc.Members)
	}
}

// TestSpawn_SettleSkippedWhenVersionMatched: when the live CC is NOT newer than
// the recipe, the settle check never runs (no latency, settleOK untouched) even
// with --verify on.
func TestSpawn_SettleSkippedWhenVersionMatched(t *testing.T) {
	f := newFixture(t)
	f.startProviderServer()
	f.writeProvidersTOML("")
	f.installFakeClaude() // live 2.1.150
	f.writeFingerprint()  // recipe 2.1.150 == live → not newer → settle gated OFF

	called := false
	orig := settleOK
	settleOK = func(string, string) bool { called = true; return true }
	t.Cleanup(func() { settleOK = orig })

	res := Spawn(Request{
		Provider:  "deepseek",
		AgentName: "worker-1",
		Team:      "myproj",
		AutoTeam:  true,
		Probe:     false,
		Verify:    true,
	})
	if !res.OK {
		t.Fatalf("spawn should succeed, got %s: %s", res.ErrorCode, res.ErrorMsg)
	}
	if called {
		t.Fatal("settle check ran despite matched recipe/CC version (decision 1A gate failed)")
	}
}
