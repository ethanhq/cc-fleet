package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestPrepare_ParseFailLeavesNoManifest: a script with a syntax error must error from
// Prepare with NO run manifest minted, so it never leaves a 0-leaf corpse the board
// lists forever.
func TestPrepare_ParseFailLeavesNoManifest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	script := filepath.Join(t.TempDir(), "bad.js")
	src := "const meta = {name: \"n\", description: \"d\"};\nconst broken = ;\n"
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(script); err == nil {
		t.Fatal("Prepare must fail on a syntax error")
	}
	if runs, _ := subagent.ListRuns(); len(runs) != 0 {
		t.Fatalf("a parse-failed script must leave NO run manifest, got %d", len(runs))
	}
}

// TestPrepare_ValidScriptMints: a script that parses cleanly (uses builtins + args)
// mints exactly one run manifest — the parse check rejects only invalid scripts.
func TestPrepare_ValidScriptMints(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	script := filepath.Join(t.TempDir(), "ok.js")
	src := "const meta = {name: \"ok\", description: \"d\"};\nphase(\"p\");\nlog(\"got \" + args);\n"
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
