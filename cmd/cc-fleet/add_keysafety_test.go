package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestAdd_HelpMarksApiKeyDeprecated checks `cc-fleet add` --help advertises
// --api-key as DEPRECATED and exposes --api-key-stdin / --api-key-file as the
// safe alternatives.
func TestAdd_HelpMarksApiKeyDeprecated(t *testing.T) {
	cmd := newAddCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("add --help: %v", err)
	}
	got := buf.String()
	for _, must := range []string{
		"DEPRECATED",
		"--api-key-stdin",
		"--api-key-file",
	} {
		if !strings.Contains(got, must) {
			t.Errorf("add --help missing %q\n----\n%s\n----", must, got)
		}
	}
}

// TestEdit_HelpMarksApiKeyDeprecated mirrors the add check for edit.
func TestEdit_HelpMarksApiKeyDeprecated(t *testing.T) {
	cmd := newEditCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("edit --help: %v", err)
	}
	got := buf.String()
	for _, must := range []string{
		"DEPRECATED",
		"--api-key-stdin",
		"--api-key-file",
	} {
		if !strings.Contains(got, must) {
			t.Errorf("edit --help missing %q\n----\n%s\n----", must, got)
		}
	}
}
