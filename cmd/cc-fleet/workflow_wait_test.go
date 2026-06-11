package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/workflow"
)

// The wait envelope is injected into the waking session unasked, so it must stay slim:
// never WorkflowRun.Error (a schema-reject can taint it with a provider reply) and
// never the Jobs array — counts + held identifiers only.
func TestWaitEnvelopeOmitsRunError(t *testing.T) {
	res := workflow.WaitResult{
		Run: subagent.WorkflowRun{
			RunID: "r1", Name: "n", Status: "failed",
			Error:    "agent(x): schema not satisfied: LEAKED_PROVIDER_REPLY",
			SpentUSD: 0.5, SpentTokens: 100,
		},
		Outcome: workflow.WaitTerminal,
		Counts:  workflow.WaitCounts{Failed: 1},
	}
	data, err := json.Marshal(waitEnvelope(res))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if strings.Contains(s, "LEAKED_PROVIDER_REPLY") || strings.Contains(s, "run_error") {
		t.Errorf("wait envelope must not carry WorkflowRun.Error:\n%s", s)
	}
	if strings.Contains(s, `"jobs"`) {
		t.Errorf("wait envelope must not carry the Jobs array:\n%s", s)
	}
	if !strings.Contains(s, `"wait_outcome":"terminal"`) || !strings.Contains(s, `"failed":1`) {
		t.Errorf("wait envelope missing outcome/counts:\n%s", s)
	}
}
