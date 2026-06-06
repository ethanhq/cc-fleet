package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// workflowsModel parks a model on the Workflows board with the given jobs/runs/activity loaded
// (via toWorkflows + a fresh-epoch workflowsMsg), bypassing disk.
func workflowsModel(t *testing.T, jobs []subagent.Result, runs []subagent.WorkflowRun, activity map[string]activitySnapshot) Model {
	t.Helper()
	mm, _ := boardModel(t, nil, nil).toWorkflows()
	m := mm.(Model)
	m, _ = step(t, m, workflowsMsg{jobs: jobs, runs: runs, activity: activity, epoch: m.workflowsEpoch})
	return m
}

// oneRun is a single manifested run with two phases (map: 1 done, build: 1 running).
func oneRun() ([]subagent.Result, []subagent.WorkflowRun) {
	runs := []subagent.WorkflowRun{{
		RunID: "run-1", Name: "sweep", Description: "a sweep run",
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

// TestWfHeader_AgentCounts: the header shows the run name, description, and <done>/<total> agents.
func TestWfHeader_AgentCounts(t *testing.T) {
	jobs, runs := oneRun()
	out := workflowsModel(t, jobs, runs, nil).viewWorkflows()
	if !strings.Contains(out, "sweep") || !strings.Contains(out, "a sweep run") {
		t.Fatalf("header missing run name/description:\n%s", out)
	}
	if !strings.Contains(out, "1/2 agents") {
		t.Fatalf("header should count 1 done of 2 agents:\n%s", out)
	}
}

// TestWfPhasesPane: single run auto-focuses (no picker); the Phases pane is numbered with per-phase
// done/total; the selected phase's agents render on the right.
func TestWfPhasesPane(t *testing.T) {
	jobs, runs := oneRun()
	m := workflowsModel(t, jobs, runs, nil)
	if m.wfMode != wfModePhases || m.focusedRunID != "run-1" {
		t.Fatalf("single run should auto-focus into Phases, got mode=%d focus=%q", m.wfMode, m.focusedRunID)
	}
	out := m.viewWorkflows()
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

// TestWfAgentRow_LiveTokens: a running leaf's tokens + tool count come from the live activity
// snapshot (not the still-empty final Result); a done leaf uses its final Result metrics.
func TestWfAgentRow_LiveTokens(t *testing.T) {
	jobs, runs := oneRun()
	activity := map[string]activitySnapshot{
		"job-b1": {sigs: []string{"WebSearch(golang)", "Bash(go test)"}, inTok: 12000, outTok: 800, hasUsage: true},
	}
	m := workflowsModel(t, jobs, runs, activity)
	// Move to the build phase (the running leaf) so its row renders on the right.
	m, _ = press(t, m, "down")
	out := m.viewWorkflows()
	if !strings.Contains(out, "12.8k tok") {
		t.Fatalf("running leaf should show live tokens (12.8k) from the snapshot:\n%s", out)
	}
	if !strings.Contains(out, "2 tools") {
		t.Fatalf("running leaf should show the live tool count (2):\n%s", out)
	}
	// The done leaf (map phase) shows its final 51.9k (50.7k in + 1.2k out) once re-selected.
	m, _ = press(t, m, "up")
	if out := m.viewWorkflows(); !strings.Contains(out, "51.9k tok") {
		t.Fatalf("done leaf should show final tokens:\n%s", out)
	}
}

// TestWfAgentCard: drilling into a phase shows the agent detail card with status/model, tok·tools,
// the Activity last-3 feed, and the Outcome line.
func TestWfAgentCard(t *testing.T) {
	jobs, runs := oneRun()
	activity := map[string]activitySnapshot{
		"job-b1": {sigs: []string{"A(1)", "B(2)", "C(3)", "D(4)"}, inTok: 1000, outTok: 50, hasUsage: true},
	}
	m := workflowsModel(t, jobs, runs, activity)
	m, _ = press(t, m, "down")  // → build phase
	m, _ = press(t, m, "enter") // → agent detail (L2)
	if m.wfMode != wfModeAgent {
		t.Fatalf("enter on a non-empty phase should drill into agents, mode=%d", m.wfMode)
	}
	out := m.viewWorkflows()
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
	if m.wfMode != wfModePhases {
		t.Fatalf("esc from the agent card should return to Phases, mode=%d", m.wfMode)
	}
}

// TestWfOutcome_Done: a done leaf's Outcome is "done · N turns" — never the raw answer.
func TestWfOutcome_Done(t *testing.T) {
	jobs, runs := oneRun()
	m := workflowsModel(t, jobs, runs, nil)
	m, _ = press(t, m, "enter") // map phase (done leaf m1) → agent detail
	out := m.viewWorkflows()
	if !strings.Contains(out, "done · 3 turns") {
		t.Fatalf("done leaf Outcome should read 'done · 3 turns':\n%s", out)
	}
}

// TestWfPicker_MultiRun: with >1 run the board opens on the run picker; enter focuses a run.
func TestWfPicker_MultiRun(t *testing.T) {
	runs := []subagent.WorkflowRun{
		{RunID: "run-a", Name: "alpha", StartedAt: "2026-06-02T00:00:00Z"},
		{RunID: "run-b", Name: "beta", StartedAt: "2026-06-01T00:00:00Z"},
	}
	m := workflowsModel(t, nil, runs, nil)
	if m.wfMode != wfModePicker {
		t.Fatalf(">1 run should open the picker, mode=%d", m.wfMode)
	}
	out := m.viewWorkflows()
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("picker should list both runs:\n%s", out)
	}
	m, _ = press(t, m, "down")  // select beta (newest-first → alpha at 0, beta at 1)
	m, _ = press(t, m, "enter") // focus it
	if m.wfMode != wfModePhases || m.focusedRunID != "run-b" {
		t.Fatalf("enter should focus the cursor's run, mode=%d focus=%q", m.wfMode, m.focusedRunID)
	}
}

// TestWfEmptyPhase_EnterNoOp: a manifest phase with zero jobs is a no-op on Enter (no panic, stays L1).
func TestWfEmptyPhase_EnterNoOp(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", Name: "empty",
		StartedAt: "2026-06-01T00:00:00Z", Phases: []subagent.RunPhase{{Title: "map"}}}}
	m := workflowsModel(t, nil, runs, nil) // manifest only, zero jobs
	out := m.viewWorkflows()
	if !strings.Contains(out, "Not started yet") {
		t.Fatalf("an empty phase should render 'Not started yet':\n%s", out)
	}
	m, _ = press(t, m, "enter")
	if m.wfMode != wfModePhases {
		t.Fatalf("enter on an empty phase must be a no-op (stay in Phases), mode=%d", m.wfMode)
	}
}

// TestWfReroot_GC: when the focused run disappears (GC'd) on a refresh, the board re-roots without panic.
func TestWfReroot_GC(t *testing.T) {
	jobs, runs := oneRun()
	m := workflowsModel(t, jobs, runs, nil)
	if m.focusedRunID != "run-1" {
		t.Fatalf("setup focus = %q, want run-1", m.focusedRunID)
	}
	// A refresh where run-1 is gone → 0 runs → empty Phases, no focus, no crash.
	m, _ = step(t, m, workflowsMsg{jobs: nil, runs: nil, epoch: m.workflowsEpoch})
	if m.focusedRunID != "" {
		t.Fatalf("a GC'd focused run should clear focus, got %q", m.focusedRunID)
	}
	if got := m.viewWorkflows(); !strings.Contains(got, "no workflow runs") {
		t.Fatalf("empty board should render the no-runs note:\n%s", got)
	}
}

// TestWfFooters: each mode's footer is contextual and NEVER offers 'p pause' (the non-goal).
func TestWfFooters(t *testing.T) {
	jobs, runs := oneRun()
	m := workflowsModel(t, jobs, runs, nil)
	phases := m.viewWorkflows()
	if !strings.Contains(phases, "x stop") || !strings.Contains(phases, "s save") {
		t.Fatalf("phases footer missing controls:\n%s", phases)
	}
	if strings.Contains(phases, "pause") {
		t.Fatalf("pause is a non-goal — the footer must not offer it:\n%s", phases)
	}
	m, _ = press(t, m, "right") // → agent detail
	agent := m.viewWorkflows()
	if !strings.Contains(agent, "j/k scroll") || !strings.Contains(agent, "restart agent") {
		t.Fatalf("agent footer missing scroll / restart-agent hints:\n%s", agent)
	}
	if strings.Contains(agent, "pause") {
		t.Fatalf("agent footer must not offer pause:\n%s", agent)
	}
}

// TestWfControlsTargetRun: x/r/s act on the FOCUSED run even when the cursor's phase has no agents.
func TestWfControlsTargetRun(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", Name: "r",
		StartedAt: "2026-06-01T00:00:00Z", Phases: []subagent.RunPhase{{Title: "map"}}}}
	m := workflowsModel(t, nil, runs, nil) // empty phase, no agents
	if _, cmd := press(t, m, "x"); cmd == nil {
		t.Fatal("x should stop the focused run even with no agents in the phase")
	}
	// Fresh model: x above marked the run in-flight (the guard shares its map), which would correctly
	// block a back-to-back r on the SAME model — here we just assert r dispatches on its own.
	if _, cmd := press(t, workflowsModel(t, nil, runs, nil), "r"); cmd == nil {
		t.Fatal("r should restart the focused run even with no agents")
	}
	m2, _ := press(t, m, "s")
	if !m2.wfSaving || !strings.Contains(m2.viewWorkflows(), "save as:") {
		t.Fatalf("s should open the save-workflow name prompt:\n%s", m2.viewWorkflows())
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
	m := workflowsModel(t, jobs, runs, nil)
	m1, cmd := press(t, m, "r")
	if cmd == nil {
		t.Fatal("first r should dispatch a restart")
	}
	if !strings.Contains(m1.viewWorkflows(), "restarting") {
		t.Fatalf("first r should show a transient 'restarting' status:\n%s", m1.viewWorkflows())
	}
	if _, c2 := press(t, m1, "r"); c2 != nil {
		t.Fatal("a second r while a restart is in flight must be a no-op")
	}
	if _, cx := press(t, m1, "x"); cx != nil {
		t.Fatal("x on a run with a restart in flight must be a no-op")
	}
	m2, _ := step(t, m1, workflowCtlMsg{verb: "restart", runID: "run-1", epoch: m1.workflowsEpoch})
	if _, c3 := press(t, m2, "r"); c3 == nil {
		t.Fatal("after the restart completes (guard cleared), r should dispatch again")
	}
}

// TestWfKeySafety_NoAnswerLeak: a planted Result.Result answer canary never reaches any rendered board
// surface (header / picker / row / agent-detail pane) — the inline detail reads the leaf's .answer side
// file, never Result.Result.
func TestWfKeySafety_NoAnswerLeak(t *testing.T) {
	const canary = "PLANTED_ANSWER_CANARY"
	jobs := []subagent.Result{{RunID: "run-1", Phase: "p", Label: "a", JobID: "job-a", Status: "done",
		NumTurns: 1, Result: canary, Usage: &subagent.Usage{InputTokens: 10}}}
	runs := []subagent.WorkflowRun{{RunID: "run-1", Name: "r", StartedAt: "2026-06-01T00:00:00Z"}}
	m := workflowsModel(t, jobs, runs, nil)
	if strings.Contains(m.viewWorkflows(), canary) {
		t.Fatalf("the answer canary leaked onto the Phases board:\n%s", m.viewWorkflows())
	}
	m, _ = press(t, m, "enter") // agent detail card
	if strings.Contains(m.viewWorkflows(), canary) {
		t.Fatalf("the answer canary leaked into the agent card:\n%s", m.viewWorkflows())
	}
}

// TestWfStaleEpoch_Dropped: a refresh from a prior visit (stale epoch) must not mutate the board.
func TestWfStaleEpoch_Dropped(t *testing.T) {
	jobs, runs := oneRun()
	m := workflowsModel(t, jobs, runs, nil)
	before := len(m.workflowJobs)
	m, _ = step(t, m, workflowsMsg{jobs: nil, runs: nil, epoch: m.workflowsEpoch - 1})
	if len(m.workflowJobs) != before {
		t.Fatalf("a stale-epoch refresh must be dropped, jobs went %d → %d", before, len(m.workflowJobs))
	}
}

// TestWfNav_ArrowsDrillInAndOut: → descends a level (Phases → Agent), ← ascends back.
func TestWfNav_ArrowsDrillInAndOut(t *testing.T) {
	jobs, runs := oneRun()
	m := workflowsModel(t, jobs, runs, nil) // single run → auto-focus Phases
	if m.wfMode != wfModePhases {
		t.Fatalf("setup: expected Phases, got %d", m.wfMode)
	}
	m, _ = press(t, m, "right")
	if m.wfMode != wfModeAgent {
		t.Fatalf("→ should descend Phases → Agent, got %d", m.wfMode)
	}
	m, _ = press(t, m, "left")
	if m.wfMode != wfModePhases {
		t.Fatalf("← should ascend Agent → Phases, got %d", m.wfMode)
	}
}

// TestWfNav_LeftClampsAtTop: ← at the board's TOP level (single-run Phases or the multi-run picker) is
// a no-op — only esc/tab leave for Vendors, so repeated ← can't fall out of the board.
func TestWfNav_LeftClampsAtTop(t *testing.T) {
	jobs, runs := oneRun()
	m := workflowsModel(t, jobs, runs, nil) // single run → Phases is the top level
	if m2, _ := press(t, m, "left"); m2.screen != screenWorkflows || m2.wfMode != wfModePhases {
		t.Fatalf("← at single-run Phases must stay on the board, got screen=%d mode=%d", m2.screen, m2.wfMode)
	}
	if m3, _ := press(t, m, "esc"); m3.screen != screenList {
		t.Fatalf("esc at the board top must exit to Vendors, got screen=%d", m3.screen)
	}
	// The multi-run picker is also a top level: ← stays put there too.
	mp := workflowsModel(t, nil, []subagent.WorkflowRun{
		{RunID: "ra", Name: "a", StartedAt: "2026-06-02T00:00:00Z"},
		{RunID: "rb", Name: "b", StartedAt: "2026-06-01T00:00:00Z"},
	}, nil)
	if mp.wfMode != wfModePicker {
		t.Fatalf("setup: expected the picker, got %d", mp.wfMode)
	}
	if mp2, _ := press(t, mp, "left"); mp2.screen != screenWorkflows || mp2.wfMode != wfModePicker {
		t.Fatalf("← at the picker must stay put, got screen=%d mode=%d", mp2.screen, mp2.wfMode)
	}
}

// TestWfTickInterval_LiveWhileRunning: the board ticks fast while a leaf runs (smooth counters) and
// falls back to the slow cadence once nothing is running.
func TestWfTickInterval_LiveWhileRunning(t *testing.T) {
	jobs, runs := oneRun() // b1 is running
	if got := workflowsModel(t, jobs, runs, nil).workflowsTickInterval(); got != workflowsLiveInterval {
		t.Fatalf("a running leaf should drive the live tick interval, got %v", got)
	}
	for i := range jobs {
		jobs[i].Status = "done"
	}
	if got := workflowsModel(t, jobs, runs, nil).workflowsTickInterval(); got != boardRefreshInterval {
		t.Fatalf("with no running leaf the tick should fall back to the slow cadence, got %v", got)
	}
}

// TestWfPromptFold_TogglesOnEnter: the focused agent's prompt is collapsed by default ("Prompt · N
// lines · ⏎ expand"); ⏎ expands the full text, a second ⏎ collapses it again.
func TestWfPromptFold_TogglesOnEnter(t *testing.T) {
	jobs, runs := oneRun()
	m := workflowsModel(t, jobs, runs, nil)
	m, _ = press(t, m, "enter") // → agent detail (map phase, leaf m1)
	leaf, ok := m.selectedLeaf()
	if !ok {
		t.Fatal("setup: no focused leaf at the agent level")
	}
	// Simulate the focused leaf's io load completing (bypassing disk).
	m.wfDetailJob, m.wfDetailPrompt, m.wfDetailAnswer, m.wfDetailIO = leaf, "line one\nline two\nline three\nline four", "the output", true
	out := m.viewWorkflows()
	// Collapsed: header + a 2-line preview + "… N more lines"; the tail stays hidden until expand.
	if !strings.Contains(out, "Prompt · 4 lines · ⏎ expand") || !strings.Contains(out, "… 2 more lines") {
		t.Fatalf("the prompt should collapse to a preview + more-lines trailer:\n%s", out)
	}
	if !strings.Contains(out, "line one") || strings.Contains(out, "line four") {
		t.Fatalf("collapsed should preview the head (line one) but hide the tail (line four):\n%s", out)
	}
	m, _ = press(t, m, "enter") // expand
	if !strings.Contains(m.viewWorkflows(), "line four") {
		t.Fatalf("⏎ should expand to the full prompt (line four):\n%s", m.viewWorkflows())
	}
	m, _ = press(t, m, "enter") // collapse again
	if strings.Contains(m.viewWorkflows(), "line four") {
		t.Fatalf("a second ⏎ should collapse the prompt again:\n%s", m.viewWorkflows())
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
	m := workflowsModel(t, jobs, runs, snap)
	if in, out, _ := m.leafCounts(jobs[0]); in != 50700 || out != 1200 {
		t.Fatalf("a done leaf should use its final Result.Usage (50700/1200), got %d/%d", in, out)
	}
	if in, out, _ := m.leafCounts(jobs[1]); in != 12000 || out != 800 {
		t.Fatalf("a running leaf should use the live snapshot (12000/800), got %d/%d", in, out)
	}
}

// TestWfSingleBox_DividerJoins: the board is ONE enclosing box with an internal ┬/┴-joined divider.
func TestWfSingleBox_DividerJoins(t *testing.T) {
	jobs, runs := oneRun()
	out := workflowsModel(t, jobs, runs, nil).viewWorkflows()
	if !strings.Contains(out, "┬") || !strings.Contains(out, "┴") {
		t.Fatalf("board should be one box with a ┬/┴-joined divider:\n%s", out)
	}
}

// TestWfSessionGrouping: >1 run groups under one "◆ session" header each, runs contiguous per session
// (a session ranked by its newest run).
func TestWfSessionGrouping(t *testing.T) {
	runs := []subagent.WorkflowRun{
		{RunID: "r1", Name: "alpha", SessionID: "sessA", StartedAt: "2026-06-01T00:00:20Z"},
		{RunID: "r2", Name: "beta", SessionID: "sessB", StartedAt: "2026-06-01T00:00:10Z"},
		{RunID: "r3", Name: "gamma", SessionID: "sessA", StartedAt: "2026-06-01T00:00:05Z"},
	}
	m := workflowsModel(t, nil, runs, nil)
	if m.wfMode != wfModePicker {
		t.Fatalf(">1 run should show the picker, got mode=%d", m.wfMode)
	}
	if out := m.viewWorkflows(); strings.Count(out, "◆") != 2 {
		t.Fatalf("expected one header per session (2 ◆), got:\n%s", out)
	}
	g := m.wfGroups()
	if g[0].sessionID != "sessA" || g[1].sessionID != "sessA" || g[2].sessionID != "sessB" {
		t.Fatalf("sessions must be contiguous (A,A,B), got %q,%q,%q", g[0].sessionID, g[1].sessionID, g[2].sessionID)
	}
}

// TestWfDedup_RerunKeepsNewest: two jobs sharing (phase,label) — a restarted leaf's fresh job + its
// lingering old job — collapse to the newest by StartedAt.
func TestWfDedup_RerunKeepsNewest(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "r1", Name: "r", StartedAt: "2026-06-01T00:00:00Z",
		Phases: []subagent.RunPhase{{Title: "p"}}}}
	jobs := []subagent.Result{
		{RunID: "r1", Phase: "p", Label: "a", JobID: "old", Status: "failed", StartedAt: "2026-06-01T00:00:05Z"},
		{RunID: "r1", Phase: "p", Label: "a", JobID: "new", Status: "done", NumTurns: 2, StartedAt: "2026-06-01T00:00:20Z"},
	}
	m := workflowsModel(t, jobs, runs, nil)
	g, _ := m.focusedGroup()
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
	m := workflowsModel(t, jobs, runs, nil)
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
	m := workflowsModel(t, jobs, runs, nil)
	m, _ = press(t, m, "right") // → agent, focused on the map phase's leaf
	if _, cmd := press(t, m, "r"); cmd == nil {
		t.Fatal("r at the agent level should dispatch a single-leaf restart")
	}
}

// TestWfDelete_TwoPressConfirm: the first d ARMS (a confirm prompt, no dispatch); a second d confirms
// and dispatches; any other key disarms.
func TestWfDelete_TwoPressConfirm(t *testing.T) {
	runs := []subagent.WorkflowRun{
		{RunID: "r1", Name: "a", SessionID: "s", StartedAt: "2026-06-01T00:00:20Z"},
		{RunID: "r2", Name: "b", SessionID: "s", StartedAt: "2026-06-01T00:00:10Z"},
	}
	m := workflowsModel(t, nil, runs, nil) // >1 run → picker
	m, cmd := press(t, m, "d")
	if cmd != nil {
		t.Fatal("the first d should ARM the delete, not dispatch it")
	}
	if !strings.Contains(m.viewWorkflows(), "press d again") {
		t.Fatalf("arming should surface a confirm prompt:\n%s", m.viewWorkflows())
	}
	m, cmd = press(t, m, "d")
	if cmd == nil {
		t.Fatal("the second d should confirm + dispatch the delete")
	}
	if m.wfDeleteArm != "" || strings.Contains(m.viewWorkflows(), "press d again") {
		t.Fatalf("confirming should clear the arm + its prompt")
	}
	// A non-d key after arming disarms (no accidental delete on a later stray d).
	m, _ = press(t, m, "d")
	if m.wfDeleteArm == "" {
		t.Fatal("d should re-arm after a prior delete")
	}
	m, _ = press(t, m, "down")
	if m.wfDeleteArm != "" || strings.Contains(m.viewWorkflows(), "press d again") {
		t.Fatal("a non-d key should disarm the pending delete")
	}
}

// TestWfAgentRow_LabelThenModelThenMetrics: a phase's agent row reads label → model → metrics, left
// to right (the metrics are right-aligned, but order is the testable part).
func TestWfAgentRow_LabelThenModelThenMetrics(t *testing.T) {
	jobs, runs := oneRun()
	out := workflowsModel(t, jobs, runs, nil).viewWorkflows() // phases view, map phase's agent m1
	li, mi, ti := strings.Index(out, "m1"), strings.Index(out, "glm-4.6"), strings.Index(out, "tok")
	if li < 0 || mi < 0 || ti < 0 || !(li < mi && mi < ti) {
		t.Fatalf("agent row should read label → model → metrics (idx %d,%d,%d):\n%s", li, mi, ti, out)
	}
}

// TestWfFixedHeight_AcrossViews: the box is a fixed height, so the rendered frame has the same line
// count whether you're at the Phases or the agent level (the bottom border doesn't move).
func TestWfFixedHeight_AcrossViews(t *testing.T) {
	jobs, runs := oneRun()
	m := workflowsModel(t, jobs, runs, nil)
	m.height = 30
	h1 := strings.Count(m.viewWorkflows(), "\n")
	m2, _ := press(t, m, "right") // → agent detail (different content)
	h2 := strings.Count(m2.viewWorkflows(), "\n")
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
	m := workflowsModel(t, jobs, runs, nil)
	m, _ = press(t, m, "enter") // → agent detail
	leaf, _ := m.selectedLeaf()
	// A prompt whose 2nd logical line is blank (a paragraph break): 4 logical lines, preview = 2.
	m.wfDetailJob, m.wfDetailPrompt, m.wfDetailAnswer, m.wfDetailIO = leaf, "title line\n\nbody one\nbody two", "", true
	lines := m.agentDetailLines(m.wfAgentRightWidth())
	ti := -1
	for i, l := range lines {
		if strings.Contains(l, "… 2 more lines") {
			ti = i
			break
		}
	}
	if ti < 0 {
		t.Fatalf("trailer '… 2 more lines' missing:\n%q", lines)
	}
	if !strings.HasPrefix(lines[ti], " ") {
		t.Fatalf("the trailer must be body-indented (leading space), got %q", lines[ti])
	}
	if strings.TrimSpace(lines[ti-1]) == "" {
		t.Fatalf("a blank line precedes the trailer (gap not trimmed): %q", lines[ti-1])
	}
}

// TestWfRunHeader_TwoLines: the run header is two lines — the bold name on line 1, the description +
// right-aligned counts on line 2.
func TestWfRunHeader_TwoLines(t *testing.T) {
	jobs, runs := oneRun()
	m := workflowsModel(t, jobs, runs, nil)
	g, _ := m.focusedGroup()
	parts := strings.Split(m.renderRunHeader(g), "\n")
	if len(parts) != 2 {
		t.Fatalf("run header must be 2 lines, got %d", len(parts))
	}
	if !strings.Contains(parts[0], "sweep") || strings.Contains(parts[0], "agents") {
		t.Fatalf("line 1 must be the name with no counts: %q", parts[0])
	}
	if !strings.Contains(parts[1], "a sweep run") || !strings.Contains(parts[1], "agents") {
		t.Fatalf("line 2 must be the description + counts: %q", parts[1])
	}
}

// TestWfRunHeader_NameBounded: a run name wider than the box must not let header line 1 overflow the box
// width — an over-width line 1 soft-wraps onto a third row and shifts the fixed-height box down.
func TestWfRunHeader_NameBounded(t *testing.T) {
	long := strings.Repeat("very-long-workflow-name-", 4) // ~96 cols, past any narrow box
	runs := []subagent.WorkflowRun{{
		RunID: "run-1", Name: long, Description: "d",
		StartedAt: "2026-06-01T00:00:00Z", UpdatedAt: "2026-06-01T00:00:30Z",
		Phases: []subagent.RunPhase{{Title: "map"}},
	}}
	jobs := []subagent.Result{{RunID: "run-1", Phase: "map", Label: "m1", Status: "running", JobID: "job-m1"}}
	m := workflowsModel(t, jobs, runs, nil)
	m.width = 50 // boardWidth == 50
	g, _ := m.focusedGroup()
	line1 := strings.Split(m.renderRunHeader(g), "\n")[0]
	if w := ansi.StringWidth(line1); w > m.boardWidth() {
		t.Fatalf("header line 1 width %d exceeds box width %d: %q", w, m.boardWidth(), line1)
	}
	if !strings.Contains(line1, "…") {
		t.Fatalf("an over-width name must be truncated with an ellipsis: %q", line1)
	}
}

// TestWfTickKeepsResolvedTitles: a live-tick refresh (titlesResolved=false) carries no titles and must NOT
// overwrite the map a prior resolve produced — only a resolve load replaces it. Guards against an
// out-of-order tick clobbering freshly resolved picker titles back to a pre-resolve snapshot.
func TestWfTickKeepsResolvedTitles(t *testing.T) {
	jobs, runs := oneRun()
	m := workflowsModel(t, jobs, runs, nil)
	resolved := map[string]string{"sess-x": "My Chat"}
	m, _ = step(t, m, workflowsMsg{jobs: jobs, runs: runs, sessionTitles: resolved, titlesResolved: true, epoch: m.workflowsEpoch})
	if m.sessionTitles["sess-x"] != "My Chat" {
		t.Fatalf("a resolve load must set titles, got %v", m.sessionTitles)
	}
	m, _ = step(t, m, workflowsMsg{jobs: jobs, runs: runs, sessionTitles: nil, titlesResolved: false, epoch: m.workflowsEpoch})
	if m.sessionTitles["sess-x"] != "My Chat" {
		t.Fatalf("a live tick must not clobber resolved titles, got %v", m.sessionTitles)
	}
}

// TestWfPicker_FullSessionIdAndTitle: the run-picker session header shows the /rename title first + the
// FULL session id in parens (not truncated to 8 + …).
func TestWfPicker_FullSessionIdAndTitle(t *testing.T) {
	full := "347597cf-1234-5678-9abc-def012345678"
	runs := []subagent.WorkflowRun{
		{RunID: "r1", Name: "research", SessionID: full, StartedAt: "2026-06-01T00:00:20Z"},
		{RunID: "r2", Name: "other", SessionID: "sess-untitled-long", StartedAt: "2026-06-01T00:00:10Z"},
	}
	m := workflowsModel(t, nil, runs, nil)
	m.sessionTitles = map[string]string{full: "my conversation"}
	out := m.viewWorkflows()
	if !strings.Contains(out, "my conversation ("+full+")") {
		t.Fatalf("picker header should show title + full session id:\n%s", out)
	}
	if !strings.Contains(out, "sess-untitled-long") {
		t.Fatalf("an untitled session should show its full id (not truncated):\n%s", out)
	}
}
