package tui

import (
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// A stopped leaf renders neutrally (the word "stopped"), never the red error class — so a
// `workflow stop` reads as a deliberate stop, not a failure.
func TestStoppedLeafRendersNeutral(t *testing.T) {
	out := Model{}.renderOutcome(subagent.Result{Status: "stopped", ErrorCode: subagent.ErrCodeStopped})
	if !strings.Contains(out, "stopped") {
		t.Errorf("stopped outcome = %q, want it to read 'stopped'", out)
	}
	if strings.Contains(out, "SUBAGENT") || strings.Contains(out, "STOPPED") {
		t.Errorf("stopped outcome leaked the error class: %q", out)
	}
	if lbl := statusLabel("stopped"); !strings.Contains(lbl, "Stopped") {
		t.Errorf("stopped status label = %q, want it to read 'Stopped'", lbl)
	}
}
