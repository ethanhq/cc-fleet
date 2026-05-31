package panevis

import (
	"errors"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/spawn"
	"github.com/ethanhq/cc-fleet/internal/tmux"
)

// TestHide_ConfigWriteFailureReturnsRecoverableSuggestion: tmux break-pane
// SUCCEEDS (pane is now in claude-hidden) but the subsequent WriteTeamConfig
// FAILS. The result must:
//
//  1. flip OK=false with ErrConfigWriteFailed,
//  2. expose a Suggestion that includes the live pane id and the exact
//     tmux join-pane command for manual recovery,
//  3. NOT silently succeed (otherwise the user is stuck — pane is hidden
//     but cc-fleet thinks Hidden=false).
//
// We fault-inject via the writeTeamConfigFn seam because in the test env
// (running as root) chmod can't deny the write — only a function-level seam
// produces a hermetic, deterministic failure.
func TestHide_ConfigWriteFailureReturnsRecoverableSuggestion(t *testing.T) {
	setup(t)
	t.Setenv("MOCK_DISPLAY_OUT", "main:0") // fake "origin window"
	seedTeam(t, "rec", []spawn.Member{
		mkMember("alice", "%42", false, ""),
	})

	orig := writeTeamConfigFn
	writeTeamConfigFn = func(_ string, _ *spawn.TeamConfig) error {
		return errors.New("forced: disk full")
	}
	t.Cleanup(func() { writeTeamConfigFn = orig })

	res := Hide("rec", "alice")
	if res.OK {
		t.Fatalf("Hide should report failure when config write fails; got OK=true")
	}
	if res.ErrorCode != ErrConfigWriteFailed {
		t.Fatalf("ErrorCode = %q, want %q", res.ErrorCode, ErrConfigWriteFailed)
	}
	if res.Suggestion == "" {
		t.Fatal("Suggestion must be non-empty so the operator can recover manually")
	}
	if !strings.Contains(res.Suggestion, "%42") {
		t.Errorf("Suggestion %q should mention pane id %%42", res.Suggestion)
	}
	if !strings.Contains(res.Suggestion, "tmux join-pane") {
		t.Errorf("Suggestion %q should give the manual recovery command", res.Suggestion)
	}
}

// TestShow_ConfigWriteFailureReturnsRecoverableSuggestion mirrors the hide
// test for show. tmux join-pane SUCCEEDS (pane is back in origin window) but
// the subsequent WriteTeamConfig FAILS — Show must surface an actionable
// recovery hint on this path.
func TestShow_ConfigWriteFailureReturnsRecoverableSuggestion(t *testing.T) {
	setup(t)
	t.Setenv("MOCK_LISTPANES_OUT", "%99\n%100\n") // satisfy ShowPane's resize step
	seedTeam(t, "rec2", []spawn.Member{
		mkMember("bob", "%99", true, "main:0"),
	})

	orig := writeTeamConfigFn
	writeTeamConfigFn = func(_ string, _ *spawn.TeamConfig) error {
		return errors.New("forced: disk full")
	}
	t.Cleanup(func() { writeTeamConfigFn = orig })

	res := Show("rec2", "bob")
	if res.OK {
		t.Fatalf("Show should report failure when config write fails; got OK=true")
	}
	if res.ErrorCode != ErrConfigWriteFailed {
		t.Fatalf("ErrorCode = %q, want %q", res.ErrorCode, ErrConfigWriteFailed)
	}
	if res.Suggestion == "" {
		t.Fatal("Suggestion must be non-empty for Show recovery")
	}
	if !strings.Contains(res.Suggestion, "%99") {
		t.Errorf("Suggestion %q should mention pane id %%99", res.Suggestion)
	}
	// Show's hint must include a paste-ready `tmux break-pane` command
	// (symmetric to Hide's) so an operator can return the system to hidden
	// without guessing flag shape.
	if !strings.Contains(res.Suggestion, "tmux break-pane") {
		t.Errorf("Suggestion %q should give a paste-ready `tmux break-pane` command (parity with Hide)", res.Suggestion)
	}
	if !strings.Contains(res.Suggestion, tmux.HiddenSessionName) {
		t.Errorf("Suggestion %q should reference the hidden session name %q so the operator pastes the right target", res.Suggestion, tmux.HiddenSessionName)
	}
}
