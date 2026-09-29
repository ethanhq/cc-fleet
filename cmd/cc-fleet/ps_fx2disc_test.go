//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

// TestFx2discPsJSONNoTmux: with tmux absent and no teammate anywhere, `ps
// --json` succeeds with an empty array. runPs exits the process on failure, so
// it runs in a re-executed test binary.
func TestFx2discPsJSONNoTmux(t *testing.T) {
	if os.Getenv("FX2DISC_PS_CHILD") == "1" {
		if err := runPs(true, false); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	// Only a ccf-* --agent-type process is attributed without a team config or
	// profile under the temp HOME; a live one on this machine would be listed.
	procs, _ := procintrospect.ProcessTable()
	for _, p := range procs {
		if strings.Contains(strings.Join(p.Argv, " "), "--agent-type ccf-") {
			t.Skip("a ccf-* teammate process is running on this machine")
		}
	}

	root := t.TempDir()
	tmuxDir := filepath.Join(root, "tmux")
	if err := os.MkdirAll(tmuxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFx2discPsJSONNoTmux$")
	cmd.Env = append(os.Environ(),
		"FX2DISC_PS_CHILD=1",
		"PATH="+filepath.Join(root, "bin"),
		"HOME="+filepath.Join(root, "home"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(root, "claude"),
		"XDG_CONFIG_HOME="+filepath.Join(root, "xdg"),
		"TMUX_TMPDIR="+tmuxDir,
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ps --json without tmux: %v\nstdout: %s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), `{"ok":true,"teammates":[]}`; got != want {
		t.Fatalf("ps --json = %s, want %s", got, want)
	}
}
