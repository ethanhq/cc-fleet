package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/teamhist"
	"github.com/ethanhq/cc-fleet/internal/teardown"
)

// fx2discNoTmux makes the board's tmux probe report tmux as absent.
func fx2discNoTmux(t *testing.T) {
	t.Helper()
	orig := lookPathFn
	t.Cleanup(func() { lookPathFn = orig })
	lookPathFn = func(string) (string, error) { return "", exec.ErrNotFound }
}

// TestFx2discLoadBoardTmuxMissingKeepsRows: the degrade flag comes from the
// board's own tmux probe, not from a discovery error. Discovery rows found
// without tmux (an in-process ccf-* member) stay on the board, the notice
// flag is set and no recorded team is synthesized as ended.
func TestFx2discLoadBoardTmuxMissingKeepsRows(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	t.Setenv("PATH", t.TempDir()) // no real tmux server is scanned
	if err := teamhist.Upsert(
		[]teardown.Teammate{{Team: "alpha", Name: "w1", PID: 4242}},
		func(string) string { return "" },
	); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	const team = "session-7c8f769b"
	dir := filepath.Join(claudeDir, "teams", team)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"name":"` + team + `","members":[{"agentId":"glm1@` + team + `","name":"glm1",` +
		`"agentType":"ccf-glm","backendType":"in-process","isActive":true,"joinedAt":1}]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	fx2discNoTmux(t)

	bm, ok := loadBoard(1)().(boardMsg)
	if !ok {
		t.Fatal("loadBoard did not return a boardMsg")
	}
	if !bm.tmuxMissing || bm.teamErr != nil {
		t.Fatalf("tmuxMissing=%v teamErr=%v, want true / nil", bm.tmuxMissing, bm.teamErr)
	}
	if len(bm.teammates) != 1 || bm.teammates[0].AgentID != "glm1@"+team ||
		bm.teammates[0].State != teardown.StateBypassed {
		t.Fatalf("teammates = %+v, want only the in-process bypassed row", bm.teammates)
	}
	if len(bm.endedSeen) != 0 {
		t.Fatalf("endedSeen must stay empty while the live set is unknown: %v", bm.endedSeen)
	}
}
