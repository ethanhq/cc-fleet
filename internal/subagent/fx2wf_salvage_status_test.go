package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// status.showUntrackedFiles=no must not make SalvageWorktree treat a leaf's only new file as a clean
// tree whose HEAD some ref already contains.
func TestFx2wfSalvageWorktreeUntrackedWithShowUntrackedNo(t *testing.T) {
	repo, wt := fxwfWorktree(t, "seg", "0123456789")
	fxwfGit(t, repo, "config", "status.showUntrackedFiles", "no")
	if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("leaf"), 0o644); err != nil {
		t.Fatal(err)
	}
	note, remove := SalvageWorktree(wt)
	if !remove || !strings.HasPrefix(note, "branch cc-fleet/wf-salvage-seg-01234567 ") {
		t.Fatalf("SalvageWorktree = (%q, %v), want a salvage branch", note, remove)
	}
	if got := fxwfGit(t, repo, "show", "cc-fleet/wf-salvage-seg-01234567:new.txt"); got != "leaf" {
		t.Errorf("salvaged new.txt = %q, want %q", got, "leaf")
	}
}
