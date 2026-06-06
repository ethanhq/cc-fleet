package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestPrepare_ResolveFailLeavesNoManifest: a script that PARSES but fails to RESOLVE (a reassigned
// top-level global — the same `cannot reassign global` class seen in the wild) must error from Prepare
// with NO run manifest minted, so it never leaves a 0-leaf corpse the board lists forever.
func TestPrepare_ResolveFailLeavesNoManifest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	script := filepath.Join(t.TempDir(), "bad.star")
	src := "meta = {\"name\": \"n\", \"description\": \"d\"}\nfor r in [1, 2]:\n    pass\nfor r in [3, 4]:\n    pass\n"
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(script); err == nil {
		t.Fatal("Prepare must fail on a reassign-global resolve error")
	}
	if runs, _ := subagent.ListRuns(); len(runs) != 0 {
		t.Fatalf("a resolve-failed script must leave NO run manifest, got %d", len(runs))
	}
}

// TestPrepare_ValidScriptMints: a script that resolves cleanly (uses builtins + args) still mints
// exactly one run manifest — the resolve check rejects only invalid scripts.
func TestPrepare_ValidScriptMints(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	script := filepath.Join(t.TempDir(), "ok.star")
	src := "meta = {\"name\": \"ok\", \"description\": \"d\"}\nphase(\"p\")\nlog(str(args))\n"
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(script); err != nil {
		t.Fatalf("valid script (builtins + args): %v", err)
	}
	if runs, _ := subagent.ListRuns(); len(runs) != 1 {
		t.Fatalf("a valid script should mint exactly 1 run, got %d", len(runs))
	}
}
