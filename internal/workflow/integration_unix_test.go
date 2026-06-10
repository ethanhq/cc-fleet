//go:build !windows

package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestRealLeafIntegration drives the FULL chain end to end through the REAL leaf
// (runLeaf = subagent.Run, NOT the test seam): a cc-fleet config + a fake `claude`
// binary wired via the fingerprint cache, then a workflow whose agent() calls exec
// that fake claude, classify its JSON envelope, and flow the result back into the
// script. It asserts the fan-out + pipeline results, the board jobs tagged with the
// run, and that the prompts actually reached the leaf over stdin. Unix-only (the fake
// is a /bin/sh script); the engine + leaf are otherwise platform-identical.
func TestRealLeafIntegration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))

	cfgDir, err := config.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Fake `claude`: append the prompt it receives on stdin to a log, then emit a
	// `claude --output-format json` result envelope — standing in for a vendor leaf.
	promptLog := filepath.Join(home, "prompts.log")
	fakeClaude := filepath.Join(home, "claude")
	fakeScript := "#!/bin/sh\ncat >> " + promptLog + "\n" +
		`printf '%s' '{"type":"result","subtype":"success","is_error":false,"result":"LEAF_OK","num_turns":1,"total_cost_usd":0.002}'` + "\n"
	if err := os.WriteFile(fakeClaude, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// Point the fingerprint cache at the fake binary (ResolveBinaryPath keeps a valid
	// cached path), and declare one enabled vendor.
	fpJSON := `{"cc_version":"2.1.150","binary_path":"` + fakeClaude + `","env":{},"flags_template":[]}`
	if err := os.WriteFile(filepath.Join(cfgDir, "fingerprint.json"), []byte(fpJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	vendors := "version = 1\n\n[fake]\n" +
		"base_url = \"https://example.invalid/anthropic\"\n" +
		"default_model = \"fake-model\"\n" +
		"models_endpoint = \"https://example.invalid/v1/models\"\n" +
		"secret_backend = \"file\"\n" +
		"secret_ref = \"fake.key\"\n" +
		"enabled = true\n" +
		"added_at = 2026-01-01T00:00:00Z\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "vendors.toml"), []byte(vendors), 0o600); err != nil {
		t.Fatal(err)
	}

	// A real workflow: a barriered fan-out + a no-barrier pipeline, ALL through the
	// real in-process leaf (runLeaf is intentionally NOT overridden here).
	wf := filepath.Join(home, "wf.js")
	wfSrc := `const meta = {name: "e2e", description: "real fake-claude leaves", phases: [{title: "map"}]};
phase("map");
const fan = (await parallel([
    () => agent("alpha", {vendor: "fake", label: "a"}),
    () => agent("beta", {vendor: "fake", label: "b"}),
])).filter((r) => r !== null);
const chain = await pipeline(["x"], (prev, item, i) => agent("stage1:" + item, {vendor: "fake"}));
return {
    ok: fan.filter((r) => r === "LEAF_OK").length,
    chained: chain[0],
};
`
	if err := os.WriteFile(wf, []byte(wfSrc), 0o600); err != nil {
		t.Fatal(err)
	}

	run, err := Prepare(wf)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Run via the engine directly so the script's returned object (the leaf results
	// flowing back) is assertable; runLeaf stays = subagent.Run (the real leaf).
	src, _ := os.ReadFile(wf)
	leafCtx, cancelLeaves := context.WithCancel(context.Background())
	defer cancelLeaves()
	eng := &engine{
		sched: newScheduler(4), runID: run.RunID,
		runCtx: context.Background(), leafCtx: leafCtx, cancelLeaves: cancelLeaves,
		cbs: make(chan leafCB, 64), loopDone: make(chan struct{}), ctl: map[string]*leafCtl{},
		name: run.Name, description: run.Description, startedAt: run.StartedAt, phases: run.Phases,
	}
	v, err := eng.run(wf, src, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	m := wantMap(t, v)
	if i := intField(t, m, "ok"); i != 2 {
		t.Errorf("ok = %v, want 2 (both fan-out leaves returned the fake claude result)", i)
	}
	if s := strField(t, m, "chained"); s != "LEAF_OK" {
		t.Errorf("chained = %q, want LEAF_OK (pipeline leaf result flowed back)", s)
	}

	// The leaves registered board jobs tagged with the run id.
	_, jobs, serr := subagent.RunStatus(run.RunID)
	if serr != nil {
		t.Fatalf("run status: %v", serr)
	}
	if len(jobs) != 3 {
		t.Errorf("run has %d tagged jobs, want 3 (2 fan-out + 1 pipeline)", len(jobs))
	}

	// The prompts actually reached the (fake) claude over stdin.
	logData, _ := os.ReadFile(promptLog)
	for _, want := range []string{"alpha", "beta", "stage1:x"} {
		if !strings.Contains(string(logData), want) {
			t.Errorf("prompt %q did not reach the leaf; log=%q", want, string(logData))
		}
	}
}
