package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/pinned"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// pinHas point-queries the registry the way production does (Snapshot + Set.Has).
func pinHas(k pinned.Kind, id string) bool {
	s, _ := pinned.Snapshot()
	return s.Has(k, id)
}

// TestBoard_ClearFailsClosedOnPinError: if the pin registry can't be read, the clear must NOT run
// (a pin-blind sweep could delete pinned records) — it shows a red failure in the modal instead.
func TestBoard_ClearFailsClosedOnPinError(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	// Break pinned.Snapshot: make the job kind-dir a FILE so its ReadDir errors.
	pinDir := filepath.Join(xdg, "cc-fleet", "pinned")
	if err := os.MkdirAll(pinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pinDir, "job"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runs := []subagent.WorkflowRun{{RunID: "r1", SessionID: "s", Status: "done", StartedAt: "2026-06-01T00:00:20Z"}}
	m := runsModel(t, nil, runs, nil)
	m, _ = press(t, m, "c")
	m, _ = press(t, m, "right") // select Confirm
	m, _ = press(t, m, "enter")
	if m.confirm == nil || !m.confirm.resultErr {
		t.Fatalf("a clear with an unreadable pin registry must fail closed (red result), got %+v", m.confirm)
	}
}

// TestConfirmModal_Selector: the confirm modal defaults to Cancel, moves with ←/→/tab, and has no
// esc (a swallowed key keeps it open — you cancel by leaving the cursor on Cancel and pressing enter).
func TestConfirmModal_Selector(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	runs := []subagent.WorkflowRun{{RunID: "r1", SessionID: "s", StartedAt: "2026-06-01T00:00:20Z"}}
	m := runsModel(t, nil, runs, nil)
	m, _ = press(t, m, "d")
	if m.confirm == nil || m.confirm.cursor != 0 {
		t.Fatalf("the modal should default to Cancel (cursor 0): %+v", m.confirm)
	}
	if r, _ := press(t, m, "right"); r.confirm.cursor != 1 {
		t.Error("right should select Confirm")
	}
	mr, _ := press(t, m, "right")
	if l, _ := press(t, mr, "left"); l.confirm.cursor != 0 {
		t.Error("left should select Cancel")
	}
	if tb, _ := press(t, m, "tab"); tb.confirm.cursor != 1 {
		t.Error("tab should toggle to Confirm")
	}
	if e, _ := press(t, m, "esc"); e.confirm == nil {
		t.Error("esc is removed; the modal should stay open")
	}
}

// TestOverlayCenter_PreservesSides: the confirm overlay floats over the board's central rows while
// keeping the board content (e.g. box borders) on both sides of the modal — no whole-row blanking.
func TestOverlayCenter_PreservesSides(t *testing.T) {
	row := "L" + strings.Repeat(".", 24) + "R" // a board row with left+right borders, width 26
	base := strings.Join([]string{row, row, row, row, row}, "\n")
	box := strings.Join([]string{"+--+", "|hi|", "+--+"}, "\n") // 4 wide, 3 tall
	out := overlayCenter(base, box, 26)
	lines := strings.Split(out, "\n")
	for i := 1; i <= 3; i++ { // the 3-row box centers over rows 1..3 of 5
		if !strings.HasPrefix(lines[i], "L") || !strings.HasSuffix(lines[i], "R") {
			t.Errorf("row %d lost a side border: %q", i, lines[i])
		}
	}
	if !strings.Contains(out, "|hi|") {
		t.Errorf("the box content should be spliced in:\n%s", out)
	}
}

// TestBoard_ClearAtEntityLevel: c is reachable at the L3 entity view too (where a single-kind
// session auto-lands), opening the clear confirm for the focused session.
func TestBoard_ClearAtEntityLevel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	jobs := []subagent.Result{{OK: true, JobID: "j1", Status: "done", LeadSessionID: "s"}}
	m := boardModel(t, nil, jobs)
	if m.asMode != asModeEntity {
		t.Fatalf("a jobs-only single session should land at the entity level: mode=%d", m.asMode)
	}
	m, cmd := press(t, m, "c")
	if cmd != nil {
		t.Fatal("c should open the modal, not dispatch")
	}
	if m.confirm == nil || m.confirm.kind != confirmClear || m.confirm.id != "s" {
		t.Fatalf("c at the entity level should open a clear confirm: %+v", m.confirm)
	}
}

// TestBoard_PinAtEntityLevel: a jobs-only single session auto-skips to the L3 entity view, and p
// there must pin the focused job AND render its ★ (the level a single session actually lands on).
func TestBoard_PinAtEntityLevel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	jobs := []subagent.Result{{OK: true, JobID: "j1", Status: "done", LeadSessionID: "s"}}
	m := boardModel(t, nil, jobs)
	if m.asMode != asModeEntity || !m.asEntitySrc.jobs {
		t.Fatalf("a jobs-only single session should land at the entity (jobs) level: mode=%d", m.asMode)
	}
	m, _ = press(t, m, "p")
	if !pinHas(pinned.Job, "j1") {
		t.Fatal("p at the entity level should pin the focused job")
	}
	if !strings.Contains(m.View(), "★") {
		t.Fatalf("a pinned job should render ★ at the entity level:\n%s", m.View())
	}
}

// TestBoard_PinToggleAndRender: `p` on a run row pins it (★ renders); `p` again unpins.
func TestBoard_PinToggleAndRender(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	runs := []subagent.WorkflowRun{{RunID: "r1", Name: "alpha", SessionID: "s", StartedAt: "2026-06-01T00:00:20Z"}}
	m := runsModel(t, nil, runs, nil)
	if m.asMode != asModeBoxes {
		t.Fatalf("a runs session should land at boxes, got %d", m.asMode)
	}

	m, cmd := press(t, m, "p")
	if cmd == nil {
		t.Fatal("p on a run row should pin + dispatch a board reload")
	}
	if !pinHas(pinned.Run, "r1") {
		t.Fatal("p should have pinned the run (synchronous write)")
	}
	// The optimistic snapshot bump renders the ★ without waiting for the reload.
	if !strings.Contains(m.View(), "★") {
		t.Fatalf("a pinned run should render the pin glyph:\n%s", m.View())
	}

	// p again unpins.
	m, _ = press(t, m, "p")
	if pinHas(pinned.Run, "r1") {
		t.Fatal("a second p should unpin the run")
	}
}

// TestBoard_ClearFinished_Modal: `c` opens a clear confirm; enter runs the clear in place and shows
// its green result in the modal; any key then dismisses it; esc cancels from the confirm phase.
func TestBoard_ClearFinished_Modal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	runs := []subagent.WorkflowRun{{RunID: "r1", SessionID: "s", Status: "done", StartedAt: "2026-06-01T00:00:20Z"}}
	m := runsModel(t, nil, runs, nil)

	m, cmd := press(t, m, "c")
	if cmd != nil {
		t.Fatal("c should open the modal, not dispatch")
	}
	if m.confirm == nil || m.confirm.kind != confirmClear || m.confirm.id != "s" {
		t.Fatalf("c did not open a clear confirm: %+v", m.confirm)
	}
	// enter on the default (Cancel) cancels without running.
	mc, _ := press(t, m, "enter")
	if mc.confirm != nil {
		t.Fatal("enter on the default Cancel should cancel the clear confirm")
	}
	// → selects Confirm, then enter runs the clear in place and shows the green outcome.
	m, _ = press(t, m, "right")
	m, cmd = press(t, m, "enter")
	if cmd == nil {
		t.Fatal("Confirm + enter should reload the board behind the result")
	}
	if m.confirm == nil || !strings.Contains(m.confirm.result, "cleared") {
		t.Fatalf("after confirm the modal should show a 'cleared …' result, got %+v", m.confirm)
	}
	if !strings.Contains(m.View(), "cleared") {
		t.Fatalf("the cleared result should render in the modal:\n%s", m.View())
	}
	// any key dismisses the result.
	m, _ = press(t, m, "enter")
	if m.confirm != nil {
		t.Fatal("a key in the result phase should dismiss the modal")
	}
}

// TestBoard_JobDelete_Modal: `d` on a job row opens a confirm; enter dispatches a jobCtlMsg.
func TestBoard_JobDelete_Modal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	runs := []subagent.WorkflowRun{{RunID: "r1", SessionID: "s", StartedAt: "2026-06-01T00:00:20Z"}}
	jobs := []subagent.Result{{OK: true, JobID: "j1", Status: "done", LeadSessionID: "s"}}
	m := runsModel(t, jobs, runs, nil)

	m, _ = press(t, m, "down") // move off the run row onto the standalone job row
	if _, onJob := m.boxJob(); !onJob {
		t.Fatalf("expected the cursor on the job row after down")
	}
	m, cmd := press(t, m, "d")
	if cmd != nil {
		t.Fatal("d on a job row should open the modal, not dispatch")
	}
	if m.confirm == nil || m.confirm.kind != confirmJob || m.confirm.id != "j1" {
		t.Fatalf("d did not open a job-delete confirm: %+v", m.confirm)
	}
	// → selects Confirm, then enter dispatches the job delete.
	m, _ = press(t, m, "right")
	m2, cmd := press(t, m, "enter")
	if cmd == nil {
		t.Fatal("Confirm + enter should dispatch the job delete")
	}
	if _, ok := cmd().(jobCtlMsg); !ok {
		t.Fatal("enter should produce a jobCtlMsg")
	}
	if m2.confirm != nil {
		t.Fatal("confirming should close the modal")
	}
}

// TestBoard_PinRapidToggle: two p presses before a reload alternate (pin then unpin), not
// double-pin — the optimistic snapshot update drives the second press's direction.
func TestBoard_PinRapidToggle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	runs := []subagent.WorkflowRun{{RunID: "r1", SessionID: "s", StartedAt: "2026-06-01T00:00:20Z"}}
	m := runsModel(t, nil, runs, nil)

	m, _ = press(t, m, "p") // synchronous pin
	if !pinHas(pinned.Run, "r1") {
		t.Fatal("the first p should pin")
	}
	m, _ = press(t, m, "p") // reads the optimistic state → synchronous unpin
	if pinHas(pinned.Run, "r1") {
		t.Error("a rapid pin+unpin should end unpinned, not double-pinned")
	}
}
