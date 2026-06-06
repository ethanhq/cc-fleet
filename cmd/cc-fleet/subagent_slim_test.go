package main

import (
	"reflect"
	"testing"
)

// TestSubagentSlimFlags_Registered locks the slim profile flags + their defaults
// onto the subagent command (the skill / workflow drive this CLI).
func TestSubagentSlimFlags_Registered(t *testing.T) {
	cmd := newSubagentCmd()
	for _, name := range []string{"profile", "tools", "skills", "mcp"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("--%s flag missing", name)
		}
	}
	if df := cmd.Flags().Lookup("profile").DefValue; df != "slim" {
		t.Fatalf("--profile default = %q, want slim (the default prompt profile)", df)
	}
	if df := cmd.Flags().Lookup("skills").DefValue; df != "true" {
		t.Fatalf("--skills default = %q, want true (native parity)", df)
	}
	if df := cmd.Flags().Lookup("mcp").DefValue; df != "false" {
		t.Fatalf("--mcp default = %q, want false (the per-profile default is resolved at the boundary, not on the flag)", df)
	}
}

// TestResolveMCPDefault: an explicit --mcp wins either way; omitted resolves the
// per-profile default — slim inherits the host config, everything else stays strict/inert.
func TestResolveMCPDefault(t *testing.T) {
	cases := []struct {
		explicit, value bool
		profile         string
		want            bool
	}{
		{true, true, "slim-ro", true}, // explicit true beats slim-ro's strict default
		{true, false, "slim", false},  // explicit false beats slim's inherit default
		{false, false, "slim", true},
		{false, false, "slim-ro", false},
		{false, false, "full", false},
		{false, false, "", false},
	}
	for _, c := range cases {
		if got := resolveMCPDefault(c.explicit, c.value, c.profile); got != c.want {
			t.Errorf("resolveMCPDefault(%v, %v, %q) = %v, want %v", c.explicit, c.value, c.profile, got, c.want)
		}
	}
}

func TestSplitToolsCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"Read", []string{"Read"}},
		{"Read,Grep", []string{"Read", "Grep"}},
		{"Read, Grep", []string{"Read", "Grep"}},
		{"Read Grep", []string{"Read", "Grep"}},
	}
	for _, c := range cases {
		got, err := splitToolsCSV(c.in)
		if err != nil {
			t.Errorf("splitToolsCSV(%q) unexpected error: %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitToolsCSV(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	// An empty segment inside a non-blank value is rejected, not silently dropped.
	for _, bad := range []string{"Read,,Grep", "Read,", ",Read", "Read, ,Grep"} {
		if _, err := splitToolsCSV(bad); err == nil {
			t.Errorf("splitToolsCSV(%q) = nil error, want an empty-entry error", bad)
		}
	}
}
