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
	if df := cmd.Flags().Lookup("skills").DefValue; df != "true" {
		t.Fatalf("--skills default = %q, want true (native parity)", df)
	}
	if df := cmd.Flags().Lookup("mcp").DefValue; df != "false" {
		t.Fatalf("--mcp default = %q, want false (strict-mcp)", df)
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
