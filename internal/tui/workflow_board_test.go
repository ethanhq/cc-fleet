package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/teardown"
)

// runsModel parks a model on the Agents Board with the given workflow jobs/runs/activity
// loaded (via a fresh-epoch boardMsg), bypassing disk. A session with runs always lands at the
// boxes level, so the Dynamic Workflows box (run rows) is visible.
func runsModel(t *testing.T, jobs []subagent.Result, runs []subagent.WorkflowRun, activity map[string]activitySnapshot) Model {
	t.Helper()
	m := boardModel(t, nil, nil)
	m, _ = step(t, m, boardMsg{jobs: jobs, runs: runs, activity: activity, epoch: m.boardEpoch})
	return m
}

// drillRun enters the run under the L2 continuum cursor (⏎ on its row) → asModeRunPhases.
func drillRun(t *testing.T, m Model) Model {
	t.Helper()
	if m.asMode != asModeBoxes {
		t.Fatalf("drillRun expects the boxes level, got mode=%d", m.asMode)
	}
	m, _ = press(t, m, "enter")
	if m.asMode != asModeRunPhases {
		t.Fatalf("⏎ on a run row should open Phases, got mode=%d", m.asMode)
	}
	return m
}

// oneRun is a single manifested run with two phases (map: 1 done, build: 1 running).
func oneRun() ([]subagent.Result, []subagent.WorkflowRun) {
	runs := []subagent.WorkflowRun{{
		RunID: "run-1", Name: "sweep", Description: "a sweep run", SessionID: "sX",
		StartedAt: "2026-06-01T00:00:00Z", UpdatedAt: "2026-06-01T00:00:30Z",
		Phases: []subagent.RunPhase{{Title: "map"}, {Title: "build"}},
	}}
	jobs := []subagent.Result{
		{RunID: "run-1", Phase: "map", Label: "m1", Provider: "glm", Model: "glm-4.6", Status: "done",
			JobID: "job-m1", NumTurns: 3, CostUSD: 0.01, Usage: &subagent.Usage{InputTokens: 50700, OutputTokens: 1200}},
		{RunID: "run-1", Phase: "build", Label: "b1", Provider: "kimi", Model: "k2", Status: "running",
			JobID: "job-b1", StartedAt: "2026-06-01T00:00:10Z"},
	}
	return jobs, runs
}

// TestWfHeader_AgentCounts: the run drill header shows the run name and <done>/<total> agents —
// the run description is no longer rendered anywhere.
func TestWfHeader_AgentCounts(t *testing.T) {
	jobs, runs := oneRun()
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	out := m.View()
	if !strings.Contains(out, "sweep") {
		t.Fatalf("header missing the run name:\n%s", out)
	}
	if strings.Contains(out, "a sweep run") {
		t.Fatalf("the run description must not be rendered:\n%s", out)
	}
	// The Phases-level header follows the CURSORED phase: "map" has its 1 agent done.
	if !strings.Contains(out, "1/1 agents") {
		t.Fatalf("header should carry the cursored phase's counts:\n%s", out)
	}
}

// TestWfPhasesPane: the Phases pane is numbered with per-phase done/total; the selected phase's
// agents render on the right.
func TestWfPhasesPane(t *testing.T) {
	jobs, runs := oneRun()
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	if m.focusedRunID != "run-1" {
		t.Fatalf("the drilled run should be focused, got %q", m.focusedRunID)
	}
	out := m.View()
	for _, want := range []string{"Phases", "✔ map", "2 build", "1/1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("phases pane missing %q:\n%s", want, out)
		}
	}
	// The first phase (map) is selected → its agent m1 shows on the right.
	if !strings.Contains(out, "m1") {
		t.Fatalf("selected phase's agent row missing:\n%s", out)
	}
}

// TestWfAgentRow_LiveTokens: a running leaf's OUTPUT tokens + tool count come from the live activity
// snapshot (not the still-empty final Result); a done leaf uses its final Result. The row shows output
// only (↓), so the leaf's input (its peak context) never inflates it — honest cross-leaf-summable tokens.
func TestWfAgentRow_LiveTokens(t *testing.T) {
	jobs, runs := oneRun()
	activity := map[string]activitySnapshot{
		"job-b1": {sigs: []string{"WebSearch(golang)", "Bash(go test)"}, inTok: 12000, outTok: 800, hasUsage: true},
	}
	m := drillRun(t, runsModel(t, jobs, runs, activity))
	// Move to the build phase (the running leaf) so its row renders on the right.
	m, _ = press(t, m, "down")
	out := m.View()
	if !strings.Contains(out, "↓ 800 out") {
		t.Fatalf("running leaf should show its live OUTPUT tokens (800) from the snapshot, not in+out:\n%s", out)
	}
	if !strings.Contains(out, "2 tools") {
		t.Fatalf("running leaf should show the live tool count (2):\n%s", out)
	}
	// The done leaf (map phase) shows its final 1.2k OUTPUT once re-selected (the 50.7k input is the
	// peak context, shown per-leaf in the detail card as "↑ ctx", never summed on the row).
	m, _ = press(t, m, "up")
	if out := m.View(); !strings.Contains(out, "↓ 1.2k out") {
		t.Fatalf("done leaf should show its final output tokens (1.2k):\n%s", out)
	}
}

// TestWfAgentCard: drilling into a phase shows the agent detail card with status/model, ↑ ctx · ↓ out
// · tool-calls, the Activity last-3 feed, and the Outcome line.
func TestWfAgentCard(t *testing.T) {
	jobs, runs := oneRun()
	activity := map[string]activitySnapshot{
		"job-b1": {sigs: []string{"A(1)", "B(2)", "C(3)", "D(4)"}, inTok: 1000, outTok: 50, hasUsage: true},
	}
	m := drillRun(t, runsModel(t, jobs, runs, activity))
	m, _ = press(t, m, "down")  // → build phase
	m, _ = press(t, m, "enter") // → agent detail
	if m.asMode != asModeRunAgent {
		t.Fatalf("enter on a non-empty phase should drill into agents, mode=%d", m.asMode)
	}
	out := m.View()
	if !strings.Contains(out, "Activity · last 3 of 4 tool calls") {
		t.Fatalf("card missing the activity header:\n%s", out)
	}
	// Only the LAST 3 signatures show.
	if strings.Contains(out, "A(1)") || !strings.Contains(out, "D(4)") {
		t.Fatalf("card should show the last 3 sigs (B,C,D), not A:\n%s", out)
	}
	if !strings.Contains(out, "Still running…") {
		t.Fatalf("a running leaf's Outcome should be 'Still running…':\n%s", out)
	}
	// esc ascends back to Phases.
	m, _ = press(t, m, "esc")
	if m.asMode != asModeRunPhases {
		t.Fatalf("esc from the agent card should return to Phases, mode=%d", m.asMode)
	}
}

// TestWfOutcome_Done: a done leaf's Outcome is "done · N turns" — never the raw answer.
func TestWfOutcome_Done(t *testing.T) {
	jobs, runs := oneRun()
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	m, _ = press(t, m, "enter") // map phase (done leaf m1) → agent detail
	out := m.View()
	if !strings.Contains(out, "done · 3 turns") {
		t.Fatalf("done leaf Outcome should read 'done · 3 turns':\n%s", out)
	}
}

// TestWfEmptyPhase_EnterNoOp: a manifest phase with zero jobs is a no-op on Enter (no panic, stays at
// the Phases level).
func TestWfEmptyPhase_EnterNoOp(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", Name: "empty", SessionID: "sX",
		StartedAt: "2026-06-01T00:00:00Z", Phases: []subagent.RunPhase{{Title: "map"}}}}
	m := drillRun(t, runsModel(t, nil, runs, nil)) // manifest only, zero jobs
	out := m.View()
	if !strings.Contains(out, "Not started yet") {
		t.Fatalf("an empty phase should render 'Not started yet':\n%s", out)
	}
	m, _ = press(t, m, "enter")
	if m.asMode != asModeRunPhases {
		t.Fatalf("enter on an empty phase must be a no-op (stay at Phases), mode=%d", m.asMode)
	}
}

// TestWfReroot_GC: when the focused run disappears (GC'd) mid-drill while its session survives
// (it still has a teammate), the board demotes out of the run drill to the session's boxes and
// clears the run focus — no panic.
func TestWfReroot_GC(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", Name: "sweep", SessionID: "sX", StartedAt: "2026-06-01T00:00:00Z"}}
	jobs := []subagent.Result{{RunID: "run-1", Phase: "map", Label: "m1", Status: "running", JobID: "job-m1", StartedAt: "2026-06-01T00:00:10Z"}}
	tms := []teardown.Teammate{{Name: "alice", Team: "t1", PaneID: "%1", PID: 1, Status: "ok", LeadSessionID: "sX", SpawnTime: 2_000_000}}
	m := boardModel(t, nil, nil)
	m, _ = step(t, m, boardMsg{teammates: tms, jobs: jobs, runs: runs, epoch: m.boardEpoch})
	m = drillRun(t, m) // boxes (run row first) → Phases
	if m.focusedRunID != "run-1" {
		t.Fatalf("setup focus = %q, want run-1", m.focusedRunID)
	}
	// A light refresh where run-1's leaf + manifest are gone (but the teammate keeps the session
	// alive) → demote out of the run drill to the session's boxes, clearing the run focus.
	m, _ = step(t, m, wfRefreshMsg{jobs: nil, runs: nil, epoch: m.boardEpoch})
	if m.asMode != asModeBoxes {
		t.Fatalf("a GC'd focused run must demote to the session's boxes, mode=%d", m.asMode)
	}
	if m.focusedRunID != "" {
		t.Fatalf("a GC'd focused run should clear focusedRunID, got %q", m.focusedRunID)
	}
}

// TestWfFooters: the run-drill footers are contextual and carry the new texts (R refresh) — and
// NEVER offer 'p pause' (the non-goal).
func TestWfFooters(t *testing.T) {
	jobs, runs := oneRun()
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	phases := m.View()
	for _, want := range []string{"x stop", "R refresh", "r restart"} {
		if !strings.Contains(phases, want) {
			t.Fatalf("phases footer missing %q:\n%s", want, phases)
		}
	}
	if strings.Contains(phases, "pause") {
		t.Fatalf("pause is a non-goal — the footer must not offer it:\n%s", phases)
	}
	// The run drill offers neither delete nor save — both are whole-workflow ops at the outermost row.
	for _, absent := range []string{"d delete", "s save"} {
		if strings.Contains(phases, absent) {
			t.Fatalf("the run drill must not offer %q — only the outermost run row does:\n%s", absent, phases)
		}
	}
	m, _ = press(t, m, "right") // → agent detail
	agent := m.View()
	for _, want := range []string{"j/k scroll", "restart agent", "R refresh"} {
		if !strings.Contains(agent, want) {
			t.Fatalf("agent footer missing %q:\n%s", want, agent)
		}
	}
	if strings.Contains(agent, "pause") {
		t.Fatalf("agent footer must not offer pause:\n%s", agent)
	}
}

// TestWfControlsTargetPhase: at the Phases level x/r act on the FOCUSED PHASE even when
// it has no agents yet; both open a phase-scoped confirm, and confirming r dispatches
// without tripping the board reload (the key-precedence regression guard: r in the
// drill must never set m.loading).
func TestWfControlsTargetPhase(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", Name: "r", SessionID: "sX", Status: "running",
		StartedAt: "2026-06-01T00:00:00Z", Phases: []subagent.RunPhase{{Title: "map"}}}}
	m := drillRun(t, runsModel(t, nil, runs, nil)) // empty phase, no agents
	mx, _ := press(t, m, "x")
	if mx.confirm == nil || mx.confirm.kind != confirmStopPhase || mx.confirm.id != "run-1" || mx.confirm.arg != "map" {
		t.Fatalf("x should open a stop confirm for the focused phase: %+v", mx.confirm)
	}
	mr, _ := press(t, m, "r")
	if mr.confirm == nil || mr.confirm.kind != confirmRestartPhase {
		t.Fatalf("r should open a phase restart confirm: %+v", mr.confirm)
	}
	mr, _ = press(t, mr, "right")
	mr, cmd := press(t, mr, "enter")
	if cmd == nil {
		t.Fatal("confirming the restart should dispatch it")
	}
	if mr.loading {
		t.Fatal("a phase restart must not trigger a board reload (loading set)")
	}
	if mr.confirm == nil || mr.confirm.phase != modalRunning {
		t.Fatalf("a dispatched restart should hold the modal in its running phase: %+v", mr.confirm)
	}
}

// TestWfSaveAtRunRow: save (s) is offered only at the outermost run row (boxes level), not inside the
// drill — s there opens the centered name prompt, enter dispatches the save, esc cancels.
func TestWfSaveAtRunRow(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "r1", Name: "alpha", SessionID: "s", StartedAt: "2026-06-01T00:00:20Z"}}
	m := runsModel(t, nil, runs, nil)
	if m.asMode != asModeBoxes {
		t.Fatalf("a runs session should land at boxes, got %d", m.asMode)
	}
	m2, _ := press(t, m, "s")
	if !m2.wfSaving || !strings.Contains(m2.View(), "Save workflow as:") {
		t.Fatalf("s on the run row should open the save prompt:\n%s", m2.View())
	}
	if _, cmd := press(t, m2, "enter"); cmd == nil {
		t.Fatal("enter on the prefilled save name should dispatch a save")
	}
	if m3, _ := press(t, m2, "esc"); m3.wfSaving {
		t.Fatal("esc should cancel the save prompt")
	}
	// s inside the drill no longer opens the save prompt.
	if md, _ := press(t, drillRun(t, runsModel(t, nil, runs, nil)), "s"); md.wfSaving {
		t.Fatal("s inside the run drill must not open the save prompt")
	}
}

// TestWfInFlightGuard: confirming a restart dispatches it and holds the modal in "restarting…"; while
// it runs the modal traps focus so r/x are swallowed; the completing workflowCtlMsg resolves the modal
// to a result and clears the guard, so a fresh r opens a new confirm.
func TestWfInFlightGuard(t *testing.T) {
	jobs, runs := oneRun()
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	m1, _ := press(t, m, "r")
	if m1.confirm == nil || m1.confirm.kind != confirmRestartPhaseKeyed {
		t.Fatalf("first r should open a keyed phase-restart confirm: %+v", m1.confirm)
	}
	m1, _ = press(t, m1, "right")
	m1, cmd := press(t, m1, "enter")
	if cmd == nil {
		t.Fatal("confirming the restart should dispatch it")
	}
	if m1.confirm == nil || m1.confirm.phase != modalRunning || !strings.Contains(m1.View(), "restarting") {
		t.Fatalf("a dispatched restart should show 'restarting…' in the running modal:\n%s", m1.View())
	}
	if mr, c2 := press(t, m1, "r"); c2 != nil || mr.confirm.phase != modalRunning {
		t.Fatal("a key while a restart is in flight must be swallowed by the running modal")
	}
	if _, cx := press(t, m1, "x"); cx != nil {
		t.Fatal("x while a restart is in flight must be a no-op")
	}
	m2, _ := step(t, m1, workflowCtlMsg{verb: "restart-phase", runID: "run-1", epoch: m1.boardEpoch})
	if m2.confirm == nil || m2.confirm.phase != modalResult {
		t.Fatalf("the completing restart should resolve the modal to a result: %+v", m2.confirm)
	}
	m2, _ = press(t, m2, "enter") // dismiss the result
	if m3, _ := press(t, m2, "r"); m3.confirm == nil || m3.confirm.kind != confirmRestartPhaseKeyed {
		t.Fatal("after the restart completes (guard cleared), r should open a fresh confirm")
	}
}

// TestWfSaveOutcomeDoesntResolveRestart: a save outcome sharing the workflowCtlMsg channel must not
// resolve a restart modal of the same run (the verbs differ) nor clear its in-flight guard.
func TestWfSaveOutcomeDoesntResolveRestart(t *testing.T) {
	jobs, runs := oneRun()
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	m, _ = press(t, m, "r")
	m, _ = press(t, m, "right")
	m, _ = press(t, m, "enter") // restart dispatched; modal running; run-1 marked busy
	if m.confirm == nil || m.confirm.phase != modalRunning || !m.wfBusy("run-1") {
		t.Fatalf("precondition: a confirmed restart should leave a running modal + busy guard: %+v busy=%v", m.confirm, m.wfBusy("run-1"))
	}
	// A save result for the same run lands first — it must NOT resolve the restart modal or free the guard.
	m, _ = step(t, m, workflowCtlMsg{verb: "save", runID: "run-1", epoch: m.boardEpoch})
	if m.confirm == nil || m.confirm.phase != modalRunning {
		t.Fatalf("a save outcome must not resolve a running restart modal: %+v", m.confirm)
	}
	if !m.wfBusy("run-1") {
		t.Fatal("a save outcome must not clear the restart's in-flight guard")
	}
	// The restart's own outcome resolves it and frees the guard.
	m, _ = step(t, m, workflowCtlMsg{verb: "restart-phase", runID: "run-1", epoch: m.boardEpoch})
	if m.confirm == nil || m.confirm.phase != modalResult {
		t.Fatalf("the restart outcome should resolve the modal: %+v", m.confirm)
	}
}

// TestWithInfoSuppressedDuringSave: an async board outcome that would pop an info modal is suppressed
// while the save-name prompt owns the screen, so it can't steal the input's keys.
func TestWithInfoSuppressedDuringSave(t *testing.T) {
	jobs, runs := oneRun()
	m := runsModel(t, jobs, runs, nil) // boxes level, cursor on the run row
	m, _ = press(t, m, "s")            // open the save-name prompt from the run row
	if !m.wfSaving {
		t.Fatal("precondition: s on the run row should open the save prompt")
	}
	// A delete outcome for another run arrives via withInfo while saving — it must NOT open a modal.
	m, _ = step(t, m, workflowCtlMsg{verb: "delete", runID: "other", epoch: m.boardEpoch})
	if m.confirm != nil {
		t.Fatalf("an info outcome must be suppressed while the save prompt is open: %+v", m.confirm)
	}
	if !m.wfSaving {
		t.Fatal("the save prompt must remain open")
	}
}

// TestWfKeySafety_NoAnswerLeak: a planted Result.Result answer canary never reaches any rendered board
// surface (run box / header / phases / agent-detail pane) — the inline detail reads the leaf's .answer
// side file, never Result.Result.
func TestWfKeySafety_NoAnswerLeak(t *testing.T) {
	const canary = "PLANTED_ANSWER_CANARY"
	jobs := []subagent.Result{{RunID: "run-1", Phase: "p", Label: "a", JobID: "job-a", Status: "done",
		NumTurns: 1, Result: canary, Usage: &subagent.Usage{InputTokens: 10}}}
	runs := []subagent.WorkflowRun{{RunID: "run-1", Name: "r", SessionID: "sX", StartedAt: "2026-06-01T00:00:00Z"}}
	m := runsModel(t, jobs, runs, nil)
	if strings.Contains(m.View(), canary) {
		t.Fatalf("the answer canary leaked onto the boxes board:\n%s", m.View())
	}
	m = drillRun(t, m)
	if strings.Contains(m.View(), canary) {
		t.Fatalf("the answer canary leaked onto the Phases board:\n%s", m.View())
	}
	m, _ = press(t, m, "enter") // agent detail card
	if strings.Contains(m.View(), canary) {
		t.Fatalf("the answer canary leaked into the agent card:\n%s", m.View())
	}
}

// TestWfStaleEpoch_Dropped: a light refresh from a prior visit (stale boardEpoch) must not mutate the
// board's workflow data.
func TestWfStaleEpoch_Dropped(t *testing.T) {
	jobs, runs := oneRun()
	m := runsModel(t, jobs, runs, nil)
	before := len(m.workflowJobs)
	m, _ = step(t, m, wfRefreshMsg{jobs: nil, runs: nil, epoch: m.boardEpoch - 1})
	if len(m.workflowJobs) != before {
		t.Fatalf("a stale-epoch refresh must be dropped, jobs went %d → %d", before, len(m.workflowJobs))
	}
}

// TestWfNav_ArrowsDrillInAndOut: → descends boxes → Phases → Agent, ← ascends back agent → phases →
// boxes.
func TestWfNav_ArrowsDrillInAndOut(t *testing.T) {
	jobs, runs := oneRun()
	m := runsModel(t, jobs, runs, nil)
	if m.asMode != asModeBoxes {
		t.Fatalf("setup: expected boxes, got %d", m.asMode)
	}
	m, _ = press(t, m, "right") // boxes → phases (the run row)
	if m.asMode != asModeRunPhases {
		t.Fatalf("→ on a run row should descend to Phases, got %d", m.asMode)
	}
	m, _ = press(t, m, "right") // phases → agent
	if m.asMode != asModeRunAgent {
		t.Fatalf("→ should descend Phases → Agent, got %d", m.asMode)
	}
	m, _ = press(t, m, "left") // agent → phases
	if m.asMode != asModeRunPhases {
		t.Fatalf("← should ascend Agent → Phases, got %d", m.asMode)
	}
	m, _ = press(t, m, "left") // phases → boxes
	if m.asMode != asModeBoxes {
		t.Fatalf("← should ascend Phases → boxes, got %d", m.asMode)
	}
}

// TestWfNav_LeftClampsAtTop: ← at the board's TOP level (a single-session boxes level) ascends per the
// AS board rules — a single session/project has nowhere to climb, so ← is a no-op and stays on the
// board; esc at the boxes top leaves for Providers.
func TestWfNav_LeftClampsAtTop(t *testing.T) {
	jobs, runs := oneRun()
	m := runsModel(t, jobs, runs, nil) // single session → boxes is the top level
	if m2, _ := press(t, m, "left"); m2.screen != screenSpawn || m2.asMode != asModeBoxes {
		t.Fatalf("← at the single-session boxes top must stay on the board, got screen=%d mode=%d", m2.screen, m2.asMode)
	}
	if m3, _ := press(t, m, "esc"); m3.screen != screenList {
		t.Fatalf("esc at the boxes top must exit to Providers, got screen=%d", m3.screen)
	}
}

// TestWfLiveChain_RunningStartsTickStops: a boardMsg whose data has a running RunID-tagged leaf starts
// the 500ms light chain (wfLiveOn + a non-nil cmd); a current-epoch wfLiveTickMsg with a leaf still
// running reschedules; once nothing runs the tick clears wfLiveOn and returns no cmd; a stale-epoch
// tick is dropped.
func TestWfLiveChain_RunningStartsTickStops(t *testing.T) {
	jobs, runs := oneRun() // b1 is running
	m := runsModel(t, jobs, runs, nil)
	if !m.wfLiveOn {
		t.Fatal("a running leaf in the refresh should have started the light chain (wfLiveOn)")
	}
	// The boardMsg that started the chain returns a cmd; assert via a fresh injection that the
	// boardMsg handler returns a non-nil cmd when a running leaf arrives.
	m2 := boardModel(t, nil, nil)
	m2, cmd := step(t, m2, boardMsg{jobs: jobs, runs: runs, epoch: m2.boardEpoch})
	if cmd == nil || !m2.wfLiveOn {
		t.Fatalf("a running leaf should start the chain with a non-nil cmd: cmd=%v wfLiveOn=%v", cmd, m2.wfLiveOn)
	}
	// Current-epoch tick with a running leaf reschedules.
	if _, c := step(t, m, wfLiveTickMsg{epoch: m.boardEpoch}); c == nil {
		t.Fatal("a current-epoch live tick with a running leaf should reschedule (non-nil cmd)")
	}
	// Stale-epoch tick is dropped.
	if _, c := step(t, m, wfLiveTickMsg{epoch: m.boardEpoch - 1}); c != nil {
		t.Fatal("a stale-epoch live tick must not reschedule")
	}
	// Nothing running → the tick clears the flag and stops the chain.
	for i := range jobs {
		jobs[i].Status = "done"
		jobs[i].NumTurns = 1
	}
	m, _ = step(t, m, wfRefreshMsg{jobs: jobs, runs: runs, epoch: m.boardEpoch})
	m, c := step(t, m, wfLiveTickMsg{epoch: m.boardEpoch})
	if m.wfLiveOn || c != nil {
		t.Fatalf("with nothing running the chain should stop: wfLiveOn=%v cmd=%v", m.wfLiveOn, c)
	}
}

// TestWfPromptFold_TogglesOnEnter: the focused agent's prompt is collapsed by default ("Prompt · N
// lines · ⏎ expand"); ⏎ expands the full text, a second ⏎ collapses it again.
func TestWfPromptFold_TogglesOnEnter(t *testing.T) {
	jobs, runs := oneRun()
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	m, _ = press(t, m, "enter") // → agent detail (map phase, leaf m1)
	leaf, ok := m.selectedLeaf()
	if !ok {
		t.Fatal("setup: no focused leaf at the agent level")
	}
	// Simulate the focused leaf's io load completing (bypassing disk). Six display lines
	// exceed the 4-line preview, so the tail folds.
	m.wfDetailJob, m.wfDetailPrompt, m.wfDetailAnswer, m.wfDetailIO = leaf, "line one\nline two\nline three\nline four\nline five\nline six", "the output", true
	out := m.View()
	if !strings.Contains(out, "Prompt · 6 lines · ⏎ expand") || !strings.Contains(out, "… 2 more lines") {
		t.Fatalf("the prompt should collapse to a preview + more-lines trailer:\n%s", out)
	}
	if !strings.Contains(out, "line one") || strings.Contains(out, "line six") {
		t.Fatalf("collapsed should preview the head (line one) but hide the tail (line six):\n%s", out)
	}
	m, _ = press(t, m, "enter") // expand
	if !strings.Contains(m.View(), "line six") {
		t.Fatalf("⏎ should expand to the full prompt (line six):\n%s", m.View())
	}
	m, _ = press(t, m, "enter") // collapse again
	if strings.Contains(m.View(), "line six") {
		t.Fatalf("a second ⏎ should collapse the prompt again:\n%s", m.View())
	}
}

// TestWfLeafCounts_DoneUsesFinalResult: a done leaf shows its accurate final Result.Usage (not the
// live activity snapshot), while a running leaf shows the live snapshot.
func TestWfLeafCounts_DoneUsesFinalResult(t *testing.T) {
	jobs, runs := oneRun() // m1 done (Usage 50700 in + 1200 out), b1 running
	snap := map[string]activitySnapshot{
		"job-m1": {inTok: 9, outTok: 5000, hasUsage: true}, // a stale live snapshot for the done leaf
		"job-b1": {inTok: 12000, outTok: 800, hasUsage: true},
	}
	m := runsModel(t, jobs, runs, snap)
	if in, out, _ := m.leafCounts(jobs[0]); in != 50700 || out != 1200 {
		t.Fatalf("a done leaf should use its final Result.Usage (50700/1200), got %d/%d", in, out)
	}
	if in, out, _ := m.leafCounts(jobs[1]); in != 12000 || out != 800 {
		t.Fatalf("a running leaf should use the live snapshot (12000/800), got %d/%d", in, out)
	}
}

// TestWfSingleBox_DividerJoins: the run drill renders ONE enclosing box with an internal ┬/┴-joined
// divider.
func TestWfSingleBox_DividerJoins(t *testing.T) {
	jobs, runs := oneRun()
	out := drillRun(t, runsModel(t, jobs, runs, nil)).View()
	if !strings.Contains(out, "┬") || !strings.Contains(out, "┴") {
		t.Fatalf("the run drill should be one box with a ┬/┴-joined divider:\n%s", out)
	}
}

// TestWfSessionGrouping: runs bucket into sessions by SessionID (groupSessions), and a runs-only
// session's project falls back to the run's launch cwd — assert via asProjects.
func TestWfSessionGrouping(t *testing.T) {
	runs := []subagent.WorkflowRun{
		{RunID: "r1", Name: "alpha", SessionID: "sessA", Cwd: "/tmp/proj-a", StartedAt: "2026-06-01T00:00:20Z"},
		{RunID: "r2", Name: "beta", SessionID: "sessB", Cwd: "/tmp/proj-b", StartedAt: "2026-06-01T00:00:10Z"},
		{RunID: "r3", Name: "gamma", SessionID: "sessA", Cwd: "/tmp/proj-a", StartedAt: "2026-06-01T00:00:05Z"},
	}
	m := runsModel(t, nil, runs, nil)
	// Two runs share sessA, one is sessB → two sessions, sessA newest-first.
	sessions := m.asSessions()
	if len(sessions) != 2 {
		t.Fatalf("runs should bucket into 2 sessions (sessA, sessB), got %d", len(sessions))
	}
	bySession := map[string]int{}
	for _, s := range sessions {
		bySession[s.sessionID] = len(s.runs)
	}
	if bySession["sessA"] != 2 || bySession["sessB"] != 1 {
		t.Fatalf("sessA should hold 2 runs, sessB 1, got %v", bySession)
	}
	// The runs-only sessions take their project dir from the run's launch cwd.
	dirs := map[string]int{}
	for _, p := range m.asProjects() {
		dirs[p.dir] = len(p.sessions)
	}
	if _, ok := dirs["/tmp/proj-a"]; !ok {
		t.Fatalf("a runs-only session's project must fall back to the run cwd (/tmp/proj-a), got projects %v", dirs)
	}
	if _, ok := dirs["/tmp/proj-b"]; !ok {
		t.Fatalf("the sessB run's project must be /tmp/proj-b, got projects %v", dirs)
	}
}

// TestWfDedup_RerunKeepsNewest: two jobs sharing (phase,label) — a restarted leaf's fresh job + its
// lingering old job — collapse to the newest by StartedAt.
func TestWfDedup_RerunKeepsNewest(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "r1", Name: "r", SessionID: "sX", StartedAt: "2026-06-01T00:00:00Z",
		Phases: []subagent.RunPhase{{Title: "p"}}}}
	jobs := []subagent.Result{
		{RunID: "r1", Phase: "p", Label: "a", JobID: "old", Status: "failed", StartedAt: "2026-06-01T00:00:05Z"},
		{RunID: "r1", Phase: "p", Label: "a", JobID: "new", Status: "done", NumTurns: 2, StartedAt: "2026-06-01T00:00:20Z"},
	}
	m := runsModel(t, jobs, runs, nil)
	g, ok := m.focusedGroup()
	if !ok {
		m = drillRun(t, m)
		g, _ = m.focusedGroup()
	}
	if n := len(g.phases[0].jobs); n != 1 {
		t.Fatalf("a re-run leaf (same phase+label) should dedup to 1 row, got %d", n)
	}
	if g.phases[0].jobs[0].JobID != "new" {
		t.Fatalf("dedup should keep the NEWEST job, got %q", g.phases[0].jobs[0].JobID)
	}
}

// TestWfScroll_ClampsAndResets: k clamps at the top; moving the agent cursor resets the scroll.
func TestWfScroll_ClampsAndResets(t *testing.T) {
	jobs, runs := oneRun()
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	m, _ = press(t, m, "right") // → agent
	m, _ = press(t, m, "k")
	if m.wfCardScroll != 0 {
		t.Fatalf("k at the top should clamp to 0, got %d", m.wfCardScroll)
	}
	m.wfCardScroll = 5         // simulate a scrolled state
	m, _ = press(t, m, "down") // moving the focused agent reloads io + resets scroll
	if m.wfCardScroll != 0 {
		t.Fatalf("scroll should reset to 0 when the focused agent changes, got %d", m.wfCardScroll)
	}
}

// TestWfRestartLeaf_DispatchesAtAgentLevel: r at the agent level opens a single-leaf restart confirm
// (carrying the leaf's journal key); confirming it dispatches the restart.
func TestWfRestartLeaf_DispatchesAtAgentLevel(t *testing.T) {
	jobs, runs := oneRun()
	jobs[0].JournalKey = "deadbeefkey" // the engine persists the leaf's key
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	m, _ = press(t, m, "right") // → agent, focused on the map phase's leaf
	mr, _ := press(t, m, "r")
	if mr.confirm == nil || mr.confirm.kind != confirmRestartAgent || mr.confirm.arg != "deadbeefkey" {
		t.Fatalf("r at the agent level should open a single-leaf restart confirm: %+v", mr.confirm)
	}
	mr, _ = press(t, mr, "right")
	if _, cmd := press(t, mr, "enter"); cmd == nil {
		t.Fatal("confirming should dispatch the single-leaf restart")
	}
}

// TestWfRestartLeaf_PromptHonestAboutScope: at the agent level, r's confirm prompt tells the truth
// about scope — "whole run" while the run is still running, "just this agent" only once it's terminal.
func TestWfRestartLeaf_PromptHonestAboutScope(t *testing.T) {
	check := func(status, want string) {
		t.Helper()
		jobs, runs := oneRun()
		runs[0].Status = status
		m := drillRun(t, runsModel(t, jobs, runs, nil))
		m, _ = press(t, m, "right") // → agent level
		mr, _ := press(t, m, "r")
		if mr.confirm == nil || mr.confirm.kind != confirmRestartAgent {
			t.Fatalf("status=%s: r should open a restart-agent confirm: %+v", status, mr.confirm)
		}
		if !strings.Contains(mr.confirm.prompt, want) {
			t.Fatalf("status=%s: prompt should mention %q, got %q", status, want, mr.confirm.prompt)
		}
	}
	check("running", "whole run")    // live run: restart re-runs siblings — say so
	check("done", "just this agent") // terminal run: a keyed restart really is leaf-scoped
}

// TestWfLeafControls_LiveMatrix: on a LIVE run, the agent pane's x/r are scoped to the
// focused leaf — a running leaf stops into a hold / restarts in place over the control
// plane; a held leaf restarts (wakes) and refuses a second stop; a finished leaf keeps
// the honest whole-run restart and an inert x.
func TestWfLeafControls_LiveMatrix(t *testing.T) {
	withLeaf := func(status string) Model {
		t.Helper()
		jobs, runs := oneRun()
		runs[0].Status = "running"
		jobs[0].Status = status
		m := drillRun(t, runsModel(t, jobs, runs, nil))
		m, _ = press(t, m, "right") // → agent level
		return m
	}
	// running leaf: x → leaf-scoped stop confirm; r → in-place restart confirm.
	m, _ := press(t, withLeaf("running"), "x")
	if m.confirm == nil || m.confirm.kind != confirmStopLeaf || !strings.Contains(m.confirm.prompt, "just this agent") {
		t.Fatalf("running leaf x: %+v", m.confirm)
	}
	m, _ = press(t, withLeaf("running"), "r")
	if m.confirm == nil || m.confirm.kind != confirmRestartLeaf || !strings.Contains(m.confirm.prompt, "cancels its current attempt") {
		t.Fatalf("running leaf r: %+v", m.confirm)
	}
	// held leaf: r wakes it; x is an inert info (already held).
	m, _ = press(t, withLeaf("held"), "r")
	if m.confirm == nil || m.confirm.kind != confirmRestartLeaf || !strings.Contains(m.confirm.prompt, "held agent") {
		t.Fatalf("held leaf r: %+v", m.confirm)
	}
	m, _ = press(t, withLeaf("held"), "x")
	if m.confirm == nil || m.confirm.phase != modalResult || !strings.Contains(m.confirm.result, "already held") {
		t.Fatalf("held leaf x should pop an info, got %+v", m.confirm)
	}
	// finished leaf on a live run: r keeps the honest whole-run composite; x is inert.
	m, _ = press(t, withLeaf("done"), "r")
	if m.confirm == nil || m.confirm.kind != confirmRestartAgent || !strings.Contains(m.confirm.prompt, "whole run") {
		t.Fatalf("done leaf r: %+v", m.confirm)
	}
	m, _ = press(t, withLeaf("done"), "x")
	if m.confirm == nil || m.confirm.phase != modalResult || !strings.Contains(m.confirm.result, "already finished") {
		t.Fatalf("done leaf x should pop an info, got %+v", m.confirm)
	}
}

// TestWfPhaseControls_Matrix: the Phases pane's x/r scope to the focused phase — live
// run: stop holds the title's agents (merged-target wording), restart re-runs them in
// place; finished run: r is the keyed phase restart, naming shared-key widening.
func TestWfPhaseControls_Matrix(t *testing.T) {
	phasePane := func(runStatus string, mutate func([]subagent.Result) []subagent.Result) Model {
		t.Helper()
		jobs, runs := oneRun()
		runs[0].Status = runStatus
		if mutate != nil {
			jobs = mutate(jobs)
		}
		return drillRun(t, runsModel(t, jobs, runs, nil)) // Phases pane
	}
	m, _ := press(t, phasePane("running", nil), "x")
	if m.confirm == nil || m.confirm.kind != confirmStopPhase || !strings.Contains(m.confirm.prompt, "one title = one merged target") {
		t.Fatalf("live phase x: %+v", m.confirm)
	}
	m, _ = press(t, phasePane("running", nil), "r")
	if m.confirm == nil || m.confirm.kind != confirmRestartPhase || !strings.Contains(m.confirm.prompt, "re-run in place") {
		t.Fatalf("live phase r: %+v", m.confirm)
	}
	m, _ = press(t, phasePane("done", nil), "r")
	if m.confirm == nil || m.confirm.kind != confirmRestartPhaseKeyed || !strings.Contains(m.confirm.prompt, "from the journal") {
		t.Fatalf("terminal phase r: %+v", m.confirm)
	}
	// A key shared across phases is named in the terminal prompt.
	widen := func(jobs []subagent.Result) []subagent.Result {
		jobs[0].JournalKey = "k-shared"
		return append(jobs, subagent.Result{RunID: jobs[0].RunID, Phase: "other", Label: "twin",
			JobID: "job-twin", Status: "done", JournalKey: "k-shared", StartedAt: jobs[0].StartedAt})
	}
	m, _ = press(t, phasePane("done", widen), "r")
	if m.confirm == nil || !strings.Contains(m.confirm.prompt, "other") {
		t.Fatalf("terminal phase r should name the widened phase: %+v", m.confirm)
	}
	m, _ = press(t, phasePane("done", nil), "x")
	if m.confirm == nil || m.confirm.phase != modalResult || !strings.Contains(m.confirm.result, "not live") {
		t.Fatalf("terminal phase x should pop an info, got %+v", m.confirm)
	}
}

// TestSessionDelete_DangerConfirm: d at the session list opens a RED danger confirm to wipe the whole
// session; confirming dispatches the async delete and holds the modal until its outcome resolves it.
func TestSessionDelete_DangerConfirm(t *testing.T) {
	m := boardModel(t,
		[]teardown.Teammate{{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "sess-bbbbbbbb", SpawnTime: 2_000_000}},
		[]subagent.Result{{JobID: "job-a0000000", Status: "done", StartedAt: "1970-01-01T00:00:01Z", LeadSessionID: "sess-aaaaaaaa"}},
	)
	if m.asMode != asModeSessions {
		t.Fatalf("multi session should park on the session list, mode=%d", m.asMode)
	}
	m, cmd := press(t, m, "d")
	if cmd != nil {
		t.Fatal("d should open the modal, not dispatch")
	}
	if m.confirm == nil || m.confirm.kind != confirmSession || !m.confirm.danger || m.confirm.id == "" {
		t.Fatalf("d should open a red danger session-delete confirm: %+v", m.confirm)
	}
	if mc, _ := press(t, m, "enter"); mc.confirm != nil {
		t.Fatal("enter on the default Cancel should cancel the session delete")
	}
	m, _ = press(t, m, "right")
	m, cmd = press(t, m, "enter")
	if cmd == nil {
		t.Fatal("Confirm should dispatch the session delete")
	}
	if m.confirm == nil || m.confirm.phase != modalRunning {
		t.Fatalf("a dispatched session delete should hold the modal running: %+v", m.confirm)
	}
	m, _ = step(t, m, sessionDelMsg{removed: 3, teams: 1, epoch: m.boardEpoch})
	if m.confirm == nil || m.confirm.phase != modalResult || !strings.Contains(m.confirm.result, "deleted 3") {
		t.Fatalf("the outcome should resolve the modal to a result: %+v", m.confirm)
	}
}

// TestWfDelete_ConfirmModal: d on a run ROW at the boxes level opens a confirm modal; enter
// dispatches the delete, esc cancels. d INSIDE the run drill (phases/agents) does NOT delete — a
// run is only deletable at its outermost (boxes) row.
func TestWfDelete_ConfirmModal(t *testing.T) {
	runs := []subagent.WorkflowRun{
		{RunID: "r1", Name: "a", SessionID: "s", StartedAt: "2026-06-01T00:00:20Z"},
		{RunID: "r2", Name: "b", SessionID: "s", StartedAt: "2026-06-01T00:00:10Z"},
	}
	// At the boxes level, d on a run row opens the modal; enter dispatches.
	m := runsModel(t, nil, runs, nil)
	if m.asMode != asModeBoxes {
		t.Fatalf("a runs session should land at boxes, got %d", m.asMode)
	}
	m, cmd := press(t, m, "d")
	if cmd != nil {
		t.Fatal("d on a run row should open the modal, not dispatch")
	}
	if m.confirm == nil || m.confirm.kind != confirmRun || m.confirm.id != "r1" {
		t.Fatalf("d did not open a run-delete confirm: %+v", m.confirm)
	}
	if !strings.Contains(m.View(), "Confirm") {
		t.Fatalf("the confirm modal should render the Confirm button:\n%s", m.View())
	}
	// → selects Confirm, then enter dispatches.
	mC, _ := press(t, m, "right")
	m2, cmd := press(t, mC, "enter")
	if cmd == nil {
		t.Fatal("Confirm + enter should dispatch the delete")
	}
	if m2.confirm != nil {
		t.Fatal("confirming should close the modal")
	}
	// enter on the default Cancel cancels without dispatching.
	m3, cmd := press(t, m, "enter")
	if cmd != nil || m3.confirm != nil {
		t.Fatal("enter on Cancel should cancel the confirm")
	}

	// Inside the run drill, d must NOT open a delete confirm — the run is only deletable at its
	// outermost (boxes) row, so drilling into phases or agents can't nuke the whole workflow.
	mp := drillRun(t, runsModel(t, nil, runs, nil))
	if mp2, cmd := press(t, mp, "d"); cmd != nil || mp2.confirm != nil {
		t.Fatalf("d at the Phases level must not delete the run: confirm=%+v cmd=%v", mp2.confirm, cmd)
	}
	// ...and not at the agent level either (the exact surprise: deleting a leaf nuked the whole run).
	jobs, runs2 := oneRun()
	ma := drillRun(t, runsModel(t, jobs, runs2, nil))
	ma, _ = press(t, ma, "right") // → agent level
	if ma2, cmd := press(t, ma, "d"); cmd != nil || ma2.confirm != nil {
		t.Fatalf("d at the agent level must not delete the run: confirm=%+v cmd=%v", ma2.confirm, cmd)
	}
}

// TestWfAgentRow_LabelThenModelThenMetrics: a phase's agent row reads label → model → metrics, left
// to right (the metrics are right-aligned, but order is the testable part).
func TestWfAgentRow_LabelThenModelThenMetrics(t *testing.T) {
	jobs, runs := oneRun()
	out := drillRun(t, runsModel(t, jobs, runs, nil)).View() // phases view, map phase's agent m1
	// "out ·" is the agent row's output-token metric marker; the run-header total uses "tokens", so anchor on the former.
	li, mi, ti := strings.Index(out, "m1"), strings.Index(out, "glm-4.6"), strings.Index(out, "out ·")
	if li < 0 || mi < 0 || ti < 0 || !(li < mi && mi < ti) {
		t.Fatalf("agent row should read label → model → metrics (idx %d,%d,%d):\n%s", li, mi, ti, out)
	}
}

// TestWfFixedHeight_AcrossViews: the run-drill box is a fixed height, so the rendered frame has the
// same line count whether you're at the Phases or the agent level (the bottom border doesn't move).
func TestWfFixedHeight_AcrossViews(t *testing.T) {
	jobs, runs := oneRun()
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	m.height = 30
	h1 := strings.Count(m.View(), "\n")
	m2, _ := press(t, m, "right") // → agent detail (different content)
	h2 := strings.Count(m2.View(), "\n")
	if h1 != h2 {
		t.Fatalf("box height must be fixed across views (phases=%d, agent=%d lines)", h1, h2)
	}
}

// TestWrapTo_CJKByDisplayWidth: a double-width (CJK) line wraps by display columns, not rune count, so
// no wrapped line overflows the pane.
func TestWrapTo_CJKByDisplayWidth(t *testing.T) {
	lines := wrapTo("你好世界你好世界你好", 10) // 10 CJK runes = 20 display columns
	if len(lines) < 2 {
		t.Fatalf("20-col CJK text must wrap to ≥2 lines at width 10, got %d", len(lines))
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > 10 {
			t.Fatalf("wrapped line exceeds 10 columns: %q (%d)", l, w)
		}
	}
}

// TestBoxCell_CJKExactWidth: truncating a CJK line on a double-width boundary still returns EXACTLY w
// columns (it re-pads after a wide-glyph cut); the pad case too.
func TestBoxCell_CJKExactWidth(t *testing.T) {
	if w := ansi.StringWidth(boxCell("你好世界你好", 5)); w != 5 { // 12 cols → cut at 5 lands mid-glyph
		t.Fatalf("boxCell must return exactly 5 columns on a CJK truncation, got %d", w)
	}
	if w := ansi.StringWidth(boxCell("hi", 6)); w != 6 {
		t.Fatalf("boxCell must pad to 6 columns, got %d", w)
	}
}

// TestWfPromptPreview_BlankLineAndIndent: the collapsed prompt's "… N more lines" trailer is body-
// indented (one column, like the preview) with no blank-line gap when a preview line is blank.
func TestWfPromptPreview_BlankLineAndIndent(t *testing.T) {
	jobs, runs := oneRun()
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	m, _ = press(t, m, "enter") // → agent detail
	leaf, _ := m.selectedLeaf()
	// A prompt whose preview window ends in blank lines (a paragraph break): the blanks
	// are trimmed before the trailer, and the trailer counts everything still hidden.
	m.wfDetailJob, m.wfDetailPrompt, m.wfDetailAnswer, m.wfDetailIO = leaf, "title line\n\n\n\nbody one\nbody two", "", true
	lines := m.agentDetailLines(m.wfAgentRightWidth())
	ti := -1
	for i, l := range lines {
		if strings.Contains(l, "… 5 more lines") {
			ti = i
			break
		}
	}
	if ti < 0 {
		t.Fatalf("trailer '… 5 more lines' missing:\n%q", lines)
	}
	if !strings.HasPrefix(lines[ti], " ") {
		t.Fatalf("the trailer must be body-indented (leading space), got %q", lines[ti])
	}
	if strings.TrimSpace(lines[ti-1]) == "" {
		t.Fatalf("a blank line precedes the trailer (gap not trimmed): %q", lines[ti-1])
	}
}

// TestWfRunHeader_Layout: the run drill header (renderRunHeader) is three lines — the fixed app title
// line (run name absent) on line 1, a blank spacer on line 2, the run label + counts summary on line 3
// (no run description anywhere). An over-width name truncates so line 1/line 3 never overflow the box,
// and a recorded launch cwd shows right-aligned on line 1.
func TestWfRunHeader_Layout(t *testing.T) {
	runs := []subagent.WorkflowRun{{
		RunID: "run-1", Name: "sweep", Description: "a sweep run", Cwd: "/tmp/projects/my-app", SessionID: "sX",
		StartedAt: "2026-06-01T00:00:00Z", UpdatedAt: "2026-06-01T00:00:30Z",
		Phases: []subagent.RunPhase{{Title: "map"}, {Title: "build"}},
	}}
	jobs := []subagent.Result{
		{RunID: "run-1", Phase: "map", Label: "m1", Status: "done", JobID: "job-m1", NumTurns: 1},
		{RunID: "run-1", Phase: "build", Label: "b1", Status: "running", JobID: "job-b1"},
	}
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	g, _ := m.focusedGroup()
	parts := strings.Split(m.renderRunHeader(g), "\n")
	if len(parts) != 3 {
		t.Fatalf("run header must be 3 lines (app title / blank / run summary), got %d", len(parts))
	}
	// Line 1 is the fixed app title + the run cwd; it does NOT carry the run name or the counts.
	if !strings.Contains(parts[0], "cc-fleet · Agents Board") || !strings.Contains(parts[0], "/tmp/projects/my-app") {
		t.Fatalf("line 1 must be the app title with the run cwd right-aligned: %q", parts[0])
	}
	if strings.Contains(parts[0], "sweep") || strings.Contains(parts[0], "agents") {
		t.Fatalf("line 1 must not carry the run name or counts: %q", parts[0])
	}
	if strings.TrimSpace(parts[1]) != "" {
		t.Fatalf("line 2 must be a blank spacer: %q", parts[1])
	}
	// Line 3 is the run label + the cursored phase's agents/token summary — never the
	// description.
	if !strings.Contains(parts[2], "sweep") || !strings.Contains(parts[2], "agents") {
		t.Fatalf("line 3 must be the run label + counts: %q", parts[2])
	}
	if strings.Contains(m.renderRunHeader(g), "a sweep run") {
		t.Fatalf("the run description must not appear in the header:\n%q", m.renderRunHeader(g))
	}
}

// TestWfRunHeader_NameBounded: a run name wider than the box must not let header line 3 overflow — an
// over-width summary line soft-wraps and shifts the fixed-height box down.
func TestWfRunHeader_NameBounded(t *testing.T) {
	long := strings.Repeat("very-long-workflow-name-", 4) // ~96 cols, past any narrow box
	runs := []subagent.WorkflowRun{{
		RunID: "run-1", Name: long, Description: "d", SessionID: "sX",
		StartedAt: "2026-06-01T00:00:00Z", UpdatedAt: "2026-06-01T00:00:30Z",
		Phases: []subagent.RunPhase{{Title: "map"}},
	}}
	jobs := []subagent.Result{{RunID: "run-1", Phase: "map", Label: "m1", Status: "running", JobID: "job-m1"}}
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	m.width = 50 // boardWidth == 50
	g, _ := m.focusedGroup()
	for _, line := range strings.Split(m.renderRunHeader(g), "\n") {
		if w := ansi.StringWidth(line); w > m.boardWidth() {
			t.Fatalf("header line width %d exceeds box width %d: %q", w, m.boardWidth(), line)
		}
	}
}

// TestWfBoard_RuleOverhangsBox: a full-width rule sits directly above the run-drill box and overhangs
// the inset box top border on both sides.
func TestWfBoard_RuleOverhangsBox(t *testing.T) {
	jobs, runs := oneRun()
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	m.width = 80
	lines := strings.Split(m.View(), "\n")
	boxIdx := -1
	for i, l := range lines {
		if strings.Contains(l, "╭") {
			boxIdx = i
			break
		}
	}
	if boxIdx <= 0 {
		t.Fatalf("no box top border found in:\n%s", m.View())
	}
	ruleW, boxW := ansi.StringWidth(lines[boxIdx-1]), ansi.StringWidth(lines[boxIdx])
	if ruleW != m.boardWidth() {
		t.Fatalf("the header rule should be full board width %d, got %d", m.boardWidth(), ruleW)
	}
	if ruleW <= boxW {
		t.Fatalf("the header rule (%d cols) must overhang the box top border (%d cols)", ruleW, boxW)
	}
}

// TestRunElapsed_LiveVsTerminal: a running run's elapsed ticks to now (not frozen at its last heartbeat);
// a terminal run freezes at UpdatedAt-StartedAt so it shows the final duration.
func TestRunElapsed_LiveVsTerminal(t *testing.T) {
	g := runGroup{startedAt: "2020-01-01T00:00:00Z", updatedAt: "2020-01-01T00:00:05Z"}
	g.status = "done"
	if got := g.elapsed(); got != "5s" {
		t.Fatalf("a terminal run should freeze at UpdatedAt-StartedAt = 5s, got %q", got)
	}
	g.status = "running"
	if got := g.elapsed(); got == "5s" {
		t.Fatalf("a running run must tick to now, not freeze at 5s, got %q", got)
	}
}

// TestPhaseAgentCountsTerminalOnly: the done counter counts only TERMINAL leaves — done/failed/stopped/
// cached — so a queued or running leaf is in-progress and never inflates a phase to "complete" early.
func TestPhaseAgentCountsTerminalOnly(t *testing.T) {
	p := runPhaseGroup{jobs: []subagent.Result{
		{Status: "done"}, {Status: "cached"}, {Status: "failed"}, // terminal
		{Status: "queued"}, {Status: "running"}, {Status: ""}, // in-progress
	}}
	done, total := phaseAgentCounts(p)
	if total != 6 {
		t.Errorf("total = %d, want 6", total)
	}
	if done != 3 {
		t.Errorf("done = %d, want 3 (done/cached/failed terminal; queued/running/\"\" in-progress)", done)
	}
}

// TestWfAgentCardAttemptMarker: a leaf that re-ran on a schema mismatch (Attempt>1) shows a faint
// "attempt N" in its detail card; a first-attempt leaf shows none.
func TestWfAgentCardAttemptMarker(t *testing.T) {
	runs := []subagent.WorkflowRun{{
		RunID: "run-1", Name: "n", SessionID: "sX", StartedAt: "2026-06-01T00:00:00Z", Phases: []subagent.RunPhase{{Title: "map"}},
	}}
	jobs := []subagent.Result{{RunID: "run-1", Phase: "map", Label: "m1", Model: "glm-4.6", Status: "done", Attempt: 2, JobID: "j1", NumTurns: 1}}
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	m, _ = press(t, m, "enter") // drill into the agent detail card
	if out := m.View(); !strings.Contains(out, "attempt 2") {
		t.Fatalf("a re-run leaf (Attempt=2) should show 'attempt 2':\n%s", out)
	}

	jobs[0].Attempt = 1 // first attempt → no marker
	m = drillRun(t, runsModel(t, jobs, runs, nil))
	m, _ = press(t, m, "enter")
	if out := m.View(); strings.Contains(out, "attempt") {
		t.Fatalf("a first-attempt leaf must show no attempt marker:\n%s", out)
	}
}

// TestWfNewSurfacesKeySafe: the queued row, attempt marker, and token figure render only canonical
// status + integer tokens — never the leaf's answer/error. A canary key + a NUL planted in a leaf's
// Result/ErrorMsg must not reach any rendered board level (boxes + phases + agent).
func TestWfNewSurfacesKeySafe(t *testing.T) {
	const canary = "sk-CANARYdeadbeef12345678"
	runs := []subagent.WorkflowRun{{
		RunID: "run-1", Name: "n", SessionID: "sX", StartedAt: "2026-06-01T00:00:00Z", Phases: []subagent.RunPhase{{Title: "map"}},
	}}
	jobs := []subagent.Result{{
		RunID: "run-1", Phase: "map", Label: "m1", Status: "queued", Attempt: 2, JobID: "j1",
		Result:   canary + "\x00",  // the (never-rendered-on-a-row) answer
		ErrorMsg: "boom " + canary, // a raw error
		Usage:    &subagent.Usage{InputTokens: 40000, OutputTokens: 1200},
	}}
	m := runsModel(t, jobs, runs, nil)
	assertKeySafe := func(level, out string) {
		if strings.Contains(out, "sk-CANARY") {
			t.Fatalf("%s must never render the leaf's answer/error (canary key leaked):\n%q", level, out)
		}
		if strings.ContainsRune(out, '\x00') {
			t.Fatalf("%s must never render a NUL from the leaf's answer:\n%q", level, out)
		}
	}
	assertKeySafe("boxes", m.View()) // the run box
	m = drillRun(t, m)
	assertKeySafe("phases", m.View()) // the queued row + its token figure
	if out := m.View(); !strings.Contains(out, "↓ 1.2k out") {
		t.Errorf("the queued leaf should still show its integer output tokens:\n%s", out)
	}
	m, _ = press(t, m, "enter") // the agent detail card
	assertKeySafe("agent", m.View())
}

// TestWfRunOnlySession_ShowsBoxes: a session whose only content is a run shows the boxes level with the
// Dynamic Workflows box (the singleKindSrc skip rule is disabled by runs); ⏎ on its run row drills into
// the Phases level.
func TestWfRunOnlySession_ShowsBoxes(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", Name: "solo", SessionID: "sX", StartedAt: "2026-06-01T00:00:00Z",
		Phases: []subagent.RunPhase{{Title: "map"}}}}
	jobs := []subagent.Result{{RunID: "run-1", Phase: "map", Label: "m1", Status: "running", JobID: "job-m1", StartedAt: "2026-06-01T00:00:10Z"}}
	m := runsModel(t, jobs, runs, nil)
	if m.asMode != asModeBoxes {
		t.Fatalf("a run-only session must land at the boxes level (skip rule disabled), got mode=%d", m.asMode)
	}
	if !strings.Contains(m.View(), "Dynamic Workflows") {
		t.Fatalf("a run-only session must show the Dynamic Workflows box:\n%s", m.View())
	}
	m = drillRun(t, m)
	if m.focusedRunID != "run-1" {
		t.Fatalf("⏎ on the run row should focus run-1, got %q", m.focusedRunID)
	}
}

// TestWfRunsPlusTeamSession_ShowsBoxes: a session with runs AND a single team still lands at the boxes
// level (the run keeps the boxes reachable); the L2 continuum runs runs → teams, so the run row is
// first and ⏎ on it drills into the Phases level.
func TestWfRunsPlusTeamSession_ShowsBoxes(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", Name: "sweep", SessionID: "sX", StartedAt: "2026-06-01T00:00:00Z"}}
	jobs := []subagent.Result{{RunID: "run-1", Phase: "map", Label: "m1", Status: "running", JobID: "job-m1", StartedAt: "2026-06-01T00:00:10Z"}}
	tms := []teardown.Teammate{{Name: "alice", Team: "t1", PaneID: "%1", PID: 1, Status: "ok", LeadSessionID: "sX", SpawnTime: 2_000_000}}
	m := boardModel(t, nil, nil)
	m, _ = step(t, m, boardMsg{teammates: tms, jobs: jobs, runs: runs, epoch: m.boardEpoch})
	if m.asMode != asModeBoxes {
		t.Fatalf("a runs+single-team session must land at boxes, got mode=%d", m.asMode)
	}
	out := m.View()
	if !strings.Contains(out, "Dynamic Workflows") || !strings.Contains(out, "Agent Teams") {
		t.Fatalf("a runs+team session must show both boxes:\n%s", out)
	}
	// The run is the first L2 row → ⏎ drills into Phases.
	if g, onRun := m.boxRun(); !onRun || g.runID != "run-1" {
		t.Fatalf("the L2 cursor should start on the run row, got onRun=%v run=%q", onRun, g.runID)
	}
	m = drillRun(t, m)
	if m.focusedRunID != "run-1" {
		t.Fatalf("⏎ on the run row should focus run-1, got %q", m.focusedRunID)
	}
}
