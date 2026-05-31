package onboarding

import (
	"strings"
	"testing"
)

func TestFamilyFromOSRelease(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ubuntu", "NAME=\"Ubuntu\"\nID=ubuntu\nID_LIKE=debian\n", "debian"},
		{"debian", "ID=debian\n", "debian"},
		{"fedora", "ID=fedora\n", "fedora"},
		{"centos", "ID=\"centos\"\nID_LIKE=\"rhel fedora\"\n", "fedora"},
		{"arch", "ID=arch\n", "arch"},
		{"alpine", "ID=alpine\n", "alpine"},
		{"unknown", "ID=plan9\n", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := familyFromOSRelease(c.in); got != c.want {
				t.Fatalf("familyFromOSRelease(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestTmuxInstallHint_NonEmpty confirms the hint always names tmux on the
// current platform and never returns empty (so the prompt always has a command
// to show).
func TestTmuxInstallHint_NonEmpty(t *testing.T) {
	hint := TmuxInstallHint()
	if hint == "" {
		t.Fatal("TmuxInstallHint() = empty")
	}
	if !strings.Contains(hint, "tmux") {
		t.Fatalf("TmuxInstallHint() = %q, want it to mention tmux", hint)
	}
}

// TestTmuxCmdForFamily checks each family maps to its package manager, and the
// unknown family falls back to a non-empty generic hint.
func TestTmuxCmdForFamily(t *testing.T) {
	cases := map[string]string{
		"debian": "apt-get",
		"fedora": "dnf",
		"arch":   "pacman",
		"alpine": "apk",
	}
	for fam, mgr := range cases {
		hint := tmuxCmdForFamily(fam)
		if !strings.Contains(hint, mgr) || !strings.Contains(hint, "tmux") {
			t.Errorf("tmuxCmdForFamily(%q) = %q, want it to mention %q + tmux", fam, hint, mgr)
		}
	}
	fallback := tmuxCmdForFamily("plan9")
	if fallback == "" || !strings.Contains(fallback, "tmux") {
		t.Errorf("fallback hint = %q, want non-empty mentioning tmux", fallback)
	}
}
