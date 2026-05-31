package main

import "testing"

// TestShouldEnterTUI verifies the bare-invocation gate. The TUI must launch
// ONLY for a true bare invocation on an interactive terminal; every
// non-interactive context falls through to help so scripts never block.
func TestShouldEnterTUI(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		stdinTTY  bool
		stdoutTTY bool
		want      bool
	}{
		{"bare + both tty", nil, true, true, true},
		{"bare + empty args slice", []string{}, true, true, true},
		{"stdin not tty (</dev/null, pipe)", nil, false, true, false},
		{"stdout not tty (| cat, redirect)", nil, true, false, false},
		{"neither tty (CI)", nil, false, false, false},
		{"positional args present", []string{"unexpected"}, true, true, false},
		{"args + non-tty", []string{"x"}, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldEnterTUI(tc.args, tc.stdinTTY, tc.stdoutTTY); got != tc.want {
				t.Errorf("shouldEnterTUI(%v, stdin=%v, stdout=%v) = %v, want %v",
					tc.args, tc.stdinTTY, tc.stdoutTTY, got, tc.want)
			}
		})
	}
}
