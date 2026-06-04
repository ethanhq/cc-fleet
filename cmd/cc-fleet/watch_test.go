package main

import (
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/teardown"
)

var fixedNow = time.Date(2026, 1, 1, 15, 4, 5, 0, time.UTC)

// TestRenderFleetKeySafe: renderFleet prints a field allowlist — never a job's Result (the
// vendor answer) — and scrubs control runes from opaque strings.
func TestRenderFleetKeySafe(t *testing.T) {
	snap := fleetSnap{
		teammates: []teardown.Teammate{{
			Name: "wkr\x1b[31m", Team: "team", Vendor: "deepseek", Model: "chat",
			PaneID: "%1", PID: 42, Status: "ok", LeadSessionID: "lead1",
		}},
		jobs: []subagent.Result{{
			Phase: "Build", Label: "analyze", Vendor: "glm", Model: "4", Status: "done",
			JobID: "job-1", Result: "SECRET_ANSWER_DO_NOT_PRINT",
		}},
		runs:   []subagent.WorkflowRun{{RunID: "run-1", Name: "nightly", Status: "running", StartedAt: "2026-01-01T00:00:00Z"}},
		titles: map[string]string{"lead1": "my-session"},
	}
	out := renderFleet(snap, fixedNow)

	if strings.Contains(out, "SECRET_ANSWER_DO_NOT_PRINT") {
		t.Errorf("a job's Result (vendor answer) leaked to stdout:\n%s", out)
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("a control rune leaked (terminal-injection risk):\n%s", out)
	}
	for _, want := range []string{
		"fleet 15:04:05", "teammates (1)", "subagent jobs (1)", "workflow runs (1)",
		"deepseek/chat", "glm/4", "done", "job-1", "run-1", "nightly", "my-session",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderFleetEmpty: an empty fleet (no tmux / nothing running) renders a "(none)" line per
// section rather than crashing.
func TestRenderFleetEmpty(t *testing.T) {
	out := renderFleet(fleetSnap{}, fixedNow)
	if n := strings.Count(out, "(none)"); n != 3 {
		t.Errorf("empty fleet should show (none) in all 3 sections, got %d:\n%s", n, out)
	}
}

// TestFleetLeadIDsDedup: lead ids are collected from teammates + jobs and de-duplicated.
func TestFleetLeadIDsDedup(t *testing.T) {
	ids := fleetLeadIDs(
		[]teardown.Teammate{{LeadSessionID: "a"}, {LeadSessionID: ""}, {LeadSessionID: "a"}},
		[]subagent.Result{{LeadSessionID: "b"}, {LeadSessionID: "a"}},
	)
	if len(ids) != 2 {
		t.Fatalf("got %v, want 2 distinct ids (a, b)", ids)
	}
}
