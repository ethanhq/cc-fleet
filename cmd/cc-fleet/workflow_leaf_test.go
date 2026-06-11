package main

import (
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// matchLeaf accepts either spelling the board shows: a job id passes through,
// a unique label resolves to its job's id, an ambiguous label lists every
// candidate, and no match names both interpretations.
func TestMatchLeaf(t *testing.T) {
	jobs := []subagent.Result{
		{JobID: "aaaa-1", Label: "scan"},
		{JobID: "bbbb-2", Label: "verify"},
		{JobID: "cccc-3", Label: "verify"},
	}

	if id, err := matchLeaf(jobs, "aaaa-1"); err != nil || id != "aaaa-1" {
		t.Fatalf("job-id passthrough = (%q, %v), want (aaaa-1, nil)", id, err)
	}
	if id, err := matchLeaf(jobs, "scan"); err != nil || id != "aaaa-1" {
		t.Fatalf("unique label = (%q, %v), want (aaaa-1, nil)", id, err)
	}
	if _, err := matchLeaf(jobs, "verify"); err == nil ||
		!strings.Contains(err.Error(), "bbbb-2") || !strings.Contains(err.Error(), "cccc-3") {
		t.Fatalf("ambiguous label must list both candidates, got %v", err)
	}
	if _, err := matchLeaf(jobs, "absent"); err == nil ||
		!strings.Contains(err.Error(), "job id or label") {
		t.Fatalf("no match must name both interpretations, got %v", err)
	}
	if _, err := matchLeaf(nil, "anything"); err == nil {
		t.Fatalf("empty job list must not resolve")
	}
}
