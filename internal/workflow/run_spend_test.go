package workflow

import (
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// budgetCharge restamps the run manifest with the LIVE cumulative spend after every leaf charge,
// so an external `workflow status` reader sees a running total mid-run — not only at the terminal
// write. Reading the manifest between charges (no terminal save yet) proves the live persistence.
func TestBudgetChargePersistsLiveSpend(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const rid = "spend-live"
	eng := &engine{runID: rid, startedAt: time.Now().UTC().Format(time.RFC3339)}
	eng.saveManifest("running", "")

	eng.budgetCharge(0.5, 1200)
	run, _, err := subagent.RunStatus(rid)
	if err != nil {
		t.Fatalf("run status: %v", err)
	}
	if run.SpentUSD != 0.5 || run.SpentTokens != 1200 {
		t.Fatalf("live spend after one charge = $%v / %d tok, want $0.5 / 1200", run.SpentUSD, run.SpentTokens)
	}

	eng.budgetCharge(0.25, 800)
	run, _, err = subagent.RunStatus(rid)
	if err != nil {
		t.Fatalf("run status: %v", err)
	}
	if run.SpentUSD != 0.75 || run.SpentTokens != 2000 {
		t.Fatalf("accumulated live spend = $%v / %d tok, want $0.75 / 2000", run.SpentUSD, run.SpentTokens)
	}
}
