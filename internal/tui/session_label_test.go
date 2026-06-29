package tui

import "testing"

// A codex-launched job/run carries a "codex:<thread>" launcher id; sessionLabel returns just
// the (scrubbed, short-stubbed) thread id — the "codex" launcher is shown by the ◇ marker /
// header tag, not a text prefix.
func TestSessionLabel_Codex(t *testing.T) {
	var m Model
	cases := []struct{ id, want string }{
		{"", "(no session)"},
		{"codex:0199abcd", "0199abcd"},      // short thread, no text prefix
		{"codex:0199abcdef01", "0199abcd…"}, // long thread truncates to 8 + …
		{"codex:\x07secret", "secret"},      // control bytes are scrubbed
	}
	for _, c := range cases {
		if got := m.sessionLabel(c.id); got != c.want {
			t.Errorf("sessionLabel(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}
