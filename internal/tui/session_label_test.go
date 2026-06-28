package tui

import "testing"

// A codex-launched job/run carries a "codex:<thread>" launcher id (no Claude session,
// no /rename title); sessionLabel renders it as a distinct "codex <thread>" header,
// scrubbing control bytes and truncating a long thread to its short stub.
func TestSessionLabel_Codex(t *testing.T) {
	var m Model
	cases := []struct{ id, want string }{
		{"", "(no session)"},
		{"codex:0199abcd", "codex 0199abcd"},      // short thread passes through
		{"codex:0199abcdef01", "codex 0199abcd…"}, // long thread truncates to 8 + …
		{"codex:\x07secret", "codex secret"},      // control bytes are scrubbed
	}
	for _, c := range cases {
		if got := m.sessionLabel(c.id); got != c.want {
			t.Errorf("sessionLabel(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}
