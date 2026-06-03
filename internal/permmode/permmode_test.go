package permmode

import (
	"strings"
	"testing"
)

func TestIsValid(t *testing.T) {
	for _, m := range Modes {
		if !IsValid(m) {
			t.Errorf("IsValid(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"", "typo", "Bypass", "skip"} {
		if IsValid(m) {
			t.Errorf("IsValid(%q) = true, want false", m)
		}
	}
}

func TestToFlags(t *testing.T) {
	cases := map[string]string{
		BypassPermissions: "--dangerously-skip-permissions",
		AcceptEdits:       "--permission-mode acceptEdits",
		Auto:              "--permission-mode auto",
		Default:           "", // no flag
		Plan:              "", // intentionally not forwarded
		"unknown":         "",
	}
	for mode, want := range cases {
		if got := strings.Join(ToFlags(mode), " "); got != want {
			t.Errorf("ToFlags(%q) = %q, want %q", mode, got, want)
		}
	}
}

func TestExplicitFlags(t *testing.T) {
	// Faithful: forwards every valid mode (incl. plan/default), unlike ToFlags.
	cases := map[string]string{
		BypassPermissions: "--dangerously-skip-permissions",
		AcceptEdits:       "--permission-mode acceptEdits",
		Auto:              "--permission-mode auto",
		Plan:              "--permission-mode plan",
		Default:           "--permission-mode default",
		"":                "", // no mode → no flag
	}
	for mode, want := range cases {
		if got := strings.Join(ExplicitFlags(mode), " "); got != want {
			t.Errorf("ExplicitFlags(%q) = %q, want %q", mode, got, want)
		}
	}
}
