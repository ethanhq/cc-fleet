package spawn

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestTeamDir_RejectsPathTraversal: defense-in-depth. Even when a caller
// bypasses the CLI validators (e.g. a programmatic API user, a future internal
// helper), TeamDir must refuse to build a traversal path.
func TestTeamDir_RejectsPathTraversal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bad := []string{"", ".", "..", "../..", "../../etc", "a/b", `c\d`, "/abs", "./x", "x/."}
	for _, name := range bad {
		name := name
		t.Run(name, func(t *testing.T) {
			got, err := TeamDir(name)
			if err == nil {
				t.Fatalf("TeamDir(%q): want error, got path %q", name, got)
			}
		})
	}
}

// TestInboxPath_RejectsPathTraversal: defense-in-depth on the inbox helper.
// Both team and name must be validated.
func TestInboxPath_RejectsPathTraversal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := []struct {
		team, name, desc string
	}{
		{"..", "alice", "team-dotdot"},
		{"a/b", "alice", "team-slash"},
		{"good", "..", "name-dotdot"},
		{"good", "a/b", "name-slash"},
		{"good", "/abs", "name-abs"},
		{"", "alice", "team-empty"},
		{"good", "", "name-empty"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.desc, func(t *testing.T) {
			got, err := InboxPath(tc.team, tc.name)
			if err == nil {
				t.Fatalf("InboxPath(%q,%q): want error, got %q", tc.team, tc.name, got)
			}
		})
	}
}

// TestTeamDir_AcceptsLegitNames: control — normal names still work.
func TestTeamDir_AcceptsLegitNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{"alpha", "team-1", "_test", "a"} {
		got, err := TeamDir(name)
		if err != nil {
			t.Errorf("TeamDir(%q): unexpected error %v", name, err)
			continue
		}
		want := filepath.Join(home, ".claude", "teams", name)
		if got != want {
			t.Errorf("TeamDir(%q) = %q, want %q", name, got, want)
		}
		// Belt-and-braces: confirm the resulting path is under the teams root.
		if !strings.HasPrefix(got, filepath.Join(home, ".claude", "teams")+"/") {
			t.Errorf("TeamDir(%q) = %q escapes teams root", name, got)
		}
	}
}
