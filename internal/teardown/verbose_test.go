//go:build !windows

package teardown

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/diag"
	"github.com/ethanhq/cc-fleet/internal/spawn"
)

// A verbose teardown traces lock acquisition, pane kills, and the dir removal.
func TestTeardownTeam_VerboseTrace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installFakeTmux(t)
	installReapHarness(t)

	seedTeam(t, "vtrace", []spawn.Member{
		{Name: "a1", AgentID: "a1@vtrace", TmuxPaneID: "%10", AgentType: "general-purpose"},
	})

	var buf bytes.Buffer
	res := TeardownTeam("vtrace", diag.New(&buf))
	if !res.OK {
		t.Fatalf("teardown failed: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	out := buf.String()
	for _, marker := range []string{
		"teardown: team lock acquired vtrace",
		"teardown: killed pane %10",
		"teardown: team dir removed (existed=true)",
	} {
		if !strings.Contains(out, marker) {
			t.Fatalf("verbose trace missing %q:\n%s", marker, out)
		}
	}
}
