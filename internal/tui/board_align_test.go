package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/teardown"
	"github.com/ethanhq/cc-fleet/internal/userops"
)

// titleIndent is the leading-space count of the first rendered line carrying needle.
func titleIndent(t *testing.T, out, needle string) int {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			return len(line) - len(strings.TrimLeft(line, " "))
		}
	}
	t.Fatalf("no line containing %q in:\n%s", needle, out)
	return -1
}

// TestBoardTitleInsetMatchesProviders: the Agents Board title keeps a constant boardMargin
// inset across loading / error / empty / populated, matching the Model Providers title — so
// the two screens align and the title never jumps horizontally on tab-in.
func TestBoardTitleInsetMatchesProviders(t *testing.T) {
	const board = "cc-fleet · Agents Board"

	// Baseline: the inset the board must match.
	hub := withProviders(t, userops.ProviderView{Name: "glm"})
	hub.width, hub.height = 100, 40
	want := titleIndent(t, hub.View(), "cc-fleet · Model Providers")
	if want != boardMargin {
		t.Fatalf("Model Providers title indent = %d, want boardMargin=%d", want, boardMargin)
	}

	// Loading — the first frame after tab, the flicker's origin.
	loading := withProviders(t, userops.ProviderView{Name: "glm"})
	loading, _ = press(t, loading, "tab")
	loading.width, loading.height = 100, 40
	if !loading.loading {
		t.Fatal("board should be loading right after tab")
	}

	// Error.
	errM := boardModel(t, nil, nil)
	errM.loading, errM.spawnErr = false, errors.New("boom")
	errM.width, errM.height = 100, 40

	// Empty — no teammates, no jobs.
	empty := boardModel(t, nil, nil)
	empty.width, empty.height = 100, 40

	// Populated — the frame the loading state used to jump to.
	full := boardModel(t,
		[]teardown.Teammate{{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "sess-aaaaaaaa"}}, nil)
	full.width, full.height = 100, 40

	for _, c := range []struct {
		name string
		m    Model
	}{{"loading", loading}, {"error", errM}, {"empty", empty}, {"populated", full}} {
		if got := titleIndent(t, c.m.View(), board); got != want {
			t.Errorf("%s board title indent = %d, want %d (boardMargin)", c.name, got, want)
		}
	}
}
