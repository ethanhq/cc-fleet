package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/teardown"
)

// runsModel parks a model on the Agent-status board with the given workflow jobs/runs/activity
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
		{RunID: "run-1", Phase: "map", Label: "m1", Vendor: "glm", Model: "glm-4.6", Status: "done",
			JobID: "job-m1", NumTurns: 3, CostUSD: 0.01, Usage: &subagent.Usage{InputTokens: 50700, OutputTokens: 1200}},
		{RunID: "run-1", Phase: "build", Label: "b1", Vendor: "kimi", Model: "k2", Status: "running",
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
	for _, want := range []string{"x stop", "s save", "d delete", "R refresh", "r restart"} {
		if !strings.Contains(phases, want) {
			t.Fatalf("phases footer missing %q:\n%s", want, phases)
		}
	}
	if strings.Contains(phases, "pause") {
		t.Fatalf("pause is a non-goal — the footer must not offer it:\n%s", phases)
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

// TestWfControlsTargetRun: x/r/s act on the FOCUSED run even when the cursor's phase has no agents;
// at the Phases level lowercase r restarts the run (it does NOT trip the board reload — the
// key-precedence regression guard: a run-level r must never set m.loading).
func TestWfControlsTargetRun(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", Name: "r", SessionID: "sX",
		StartedAt: "2026-06-01T00:00:00Z", Phases: []subagent.RunPhase{{Title: "map"}}}}
	m := drillRun(t, runsModel(t, nil, runs, nil)) // empty phase, no agents
	if mx, cmd := press(t, m, "x"); cmd == nil {
		_ = mx
		t.Fatal("x should stop the focused run even with no agents in the phase")
	}
	// lowercase r at the Phases level is the workflow restart, NOT the board reload: it must
	// dispatch a restart and leave m.loading false. A fresh board: the x press above marked
	// the run busy in the in-flight map, which is shared across model copies.
	m = drillRun(t, runsModel(t, nil, runs, nil))
	mr, cmd := press(t, m, "r")
	if cmd == nil {
		t.Fatal("r should restart the focused run even with no agents")
	}
	if mr.loading {
		t.Fatal("lowercase r at the Phases level must restart the run, not trigger a board reload (loading set)")
	}
	m2, _ := press(t, m, "s")
	if !m2.wfSaving || !strings.Contains(m2.View(), "save as:") {
		t.Fatalf("s should open the save-workflow name prompt:\n%s", m2.View())
	}
	// enter on a non-empty name dispatches the save; esc cancels.
	if _, cmd := press(t, m2, "enter"); cmd == nil {
		t.Fatal("enter on the prefilled save name should dispatch a save")
	}
	m3, _ := press(t, m2, "esc")
	if m3.wfSaving {
		t.Fatal("esc should cancel the save prompt")
	}
}

// TestWfInFlightGuard: a restart marks the run in-flight with a transient "restarting" status; a second
// r (or x) while it is in flight is a no-op; the completing workflowCtlMsg clears the guard so r works again.
func TestWfInFlightGuard(t *testing.T) {
	jobs, runs := oneRun()
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	m1, cmd := press(t, m, "r")
	if cmd == nil {
		t.Fatal("first r should dispatch a restart")
	}
	if !strings.Contains(m1.View(), "restarting") {
		t.Fatalf("first r should show a transient 'restarting' status:\n%s", m1.View())
	}
	if _, c2 := press(t, m1, "r"); c2 != nil {
		t.Fatal("a second r while a restart is in flight must be a no-op")
	}
	if _, cx := press(t, m1, "x"); cx != nil {
		t.Fatal("x on a run with a restart in flight must be a no-op")
	}
	m2, _ := step(t, m1, workflowCtlMsg{verb: "restart", runID: "run-1", epoch: m1.boardEpoch})
	if _, c3 := press(t, m2, "r"); c3 == nil {
		t.Fatal("after the restart completes (guard cleared), r should dispatch again")
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
// board; esc at the boxes top leaves for Vendors.
func TestWfNav_LeftClampsAtTop(t *testing.T) {
	jobs, runs := oneRun()
	m := runsModel(t, jobs, runs, nil) // single session → boxes is the top level
	if m2, _ := press(t, m, "left"); m2.screen != screenSpawn || m2.asMode != asModeBoxes {
		t.Fatalf("← at the single-session boxes top must stay on the board, got screen=%d mode=%d", m2.screen, m2.asMode)
	}
	if m3, _ := press(t, m, "esc"); m3.screen != screenList {
		t.Fatalf("esc at the boxes top must exit to Vendors, got screen=%d", m3.screen)
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

// TestWfRestartLeaf_DispatchesAtAgentLevel: r at the agent level dispatches a single-leaf restart
// (even a leaf with an empty key falls back to a whole-run restart — never a silent no-op).
func TestWfRestartLeaf_DispatchesAtAgentLevel(t *testing.T) {
	jobs, runs := oneRun()
	jobs[0].JournalKey = "deadbeefkey" // the engine persists the leaf's key
	m := drillRun(t, runsModel(t, jobs, runs, nil))
	m, _ = press(t, m, "right") // → agent, focused on the map phase's leaf
	if _, cmd := press(t, m, "r"); cmd == nil {
		t.Fatal("r at the agent level should dispatch a single-leaf restart")
	}
}

// TestWfDelete_TwoPressConfirm: d on a run ROW at the boxes level arms a two-press delete; d at the
// Phases level does too. The first d ARMS (a confirm prompt, no dispatch); a second d confirms and
// dispatches; any other key disarms.
func TestWfDelete_TwoPressConfirm(t *testing.T) {
	runs := []subagent.WorkflowRun{
		{RunID: "r1", Name: "a", SessionID: "s", StartedAt: "2026-06-01T00:00:20Z"},
		{RunID: "r2", Name: "b", SessionID: "s", StartedAt: "2026-06-01T00:00:10Z"},
	}
	// At the boxes level, d on a run row arms + confirms.
	m := runsModel(t, nil, runs, nil)
	if m.asMode != asModeBoxes {
		t.Fatalf("a runs session should land at boxes, got %d", m.asMode)
	}
	m, cmd := press(t, m, "d")
	if cmd != nil {
		t.Fatal("the first d on a run row should ARM the delete, not dispatch it")
	}
	if !strings.Contains(m.View(), "press d again") {
		t.Fatalf("arming should surface a confirm prompt:\n%s", m.View())
	}
	m, cmd = press(t, m, "d")
	if cmd == nil {
		t.Fatal("the second d should confirm + dispatch the delete")
	}
	if m.wfDeleteArm != "" || strings.Contains(m.View(), "press d again") {
		t.Fatalf("confirming should clear the arm + its prompt")
	}
	// A non-d key after arming disarms (no accidental delete on a later stray d).
	m, _ = press(t, m, "d")
	if m.wfDeleteArm == "" {
		t.Fatal("d should re-arm after a prior delete")
	}
	m, _ = press(t, m, "down")
	if m.wfDeleteArm != "" || strings.Contains(m.View(), "press d again") {
		t.Fatal("a non-d key should disarm the pending delete")
	}

	// At the Phases level, d also arms (the run-level control).
	mp := drillRun(t, runsModel(t, nil, runs, nil))
	mp, cmd = press(t, mp, "d")
	if cmd != nil {
		t.Fatal("the first d at the Phases level should ARM the delete, not dispatch it")
	}
	if !strings.Contains(mp.View(), "press d again") {
		t.Fatalf("arming at the Phases level should surface a confirm prompt:\n%s", mp.View())
	}
	if _, cmd := press(t, mp, "d"); cmd == nil {
		t.Fatal("the second d at the Phases level should confirm + dispatch")
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
	if !strings.Contains(parts[0], "cc-fleet · Agent status") || !strings.Contains(parts[0], "/tmp/projects/my-app") {
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
