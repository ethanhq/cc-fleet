package config

import "testing"

func TestParseKeyRotation_Valid(t *testing.T) {
	cases := []struct {
		in   string
		want KeyRotation
	}{
		{"", RotationOff},
		{"off", RotationOff},
		{"round_robin", RotationRoundRobin},
		{"random", RotationRandom},
	}
	for _, tc := range cases {
		got, err := ParseKeyRotation(tc.in)
		if err != nil {
			t.Errorf("ParseKeyRotation(%q): unexpected err %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseKeyRotation(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseKeyRotation_Invalid(t *testing.T) {
	cases := []string{
		"typo",
		"Off",          // case-sensitive
		"round-robin",  // dash instead of underscore
		" round_robin", // leading space
		"random ",      // trailing space
	}
	for _, in := range cases {
		_, err := ParseKeyRotation(in)
		if err == nil {
			t.Errorf("ParseKeyRotation(%q): want error, got nil", in)
		}
	}
}

// TestKeyRotation_NextCycle locks the canonical UI cycle:
// off -> round_robin -> random -> off. The TUI relies on this; if it
// regresses the rotation key in the key manager will skip a state.
func TestKeyRotation_NextCycle(t *testing.T) {
	cur := RotationOff
	cycle := []KeyRotation{RotationRoundRobin, RotationRandom, RotationOff}
	for i, want := range cycle {
		cur = cur.Next()
		if cur != want {
			t.Fatalf("step %d: got %q, want %q", i, cur, want)
		}
	}
}

// TestKeyRotation_NextUnknownGoesOff: a hand-corrupted vendors.toml that
// somehow gets past Validate must not wedge the TUI rotation cycle.
func TestKeyRotation_NextUnknownGoesOff(t *testing.T) {
	bogus := KeyRotation("not-a-real-strategy")
	if got := bogus.Next(); got != RotationOff {
		t.Fatalf("bogus.Next() = %q, want RotationOff", got)
	}
}

// TestValidKeyRotations_StableOrder protects the canonical error-message order
// (used by Validate's "want one of" message). Tests/users grep this; freezing it
// keeps the message stable.
func TestValidKeyRotations_StableOrder(t *testing.T) {
	got := ValidKeyRotations()
	want := []string{"", "off", "round_robin", "random"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: %q, want %q", i, got[i], want[i])
		}
	}
}
