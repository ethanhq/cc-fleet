package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// workflowsModel parks a model on the Workflows board with the given jobs/runs
// loaded (screen=screenWorkflows, workflowsEpoch=1, loading=false), bypassing disk
// via a direct fresh-epoch workflowsMsg.
func workflowsModel(t *testing.T, jobs []subagent.Result, runs []subagent.WorkflowRun, evts map[string][]eventLine) Model {
	t.Helper()
	m := boardModel(t, nil, nil)
	m, _ = press(t, m, "tab") // -> Workflows, epoch 1, loading
	m, _ = step(t, m, workflowsMsg{jobs: jobs, runs: runs, newEvts: evts, epoch: m.workflowsEpoch})
	return m
}

// ---------------------------------------------------------------------------
// R2.1 incremental events tailer (parseEventLines — the pure core)
// ---------------------------------------------------------------------------

func TestParseEventLines_SplitsAndCarriesPartial(t *testing.T) {
	// Two whole lines + a torn trailing partial.
	chunk := `{"seq":1,"kind":"phase","phase":"plan"}` + "\n" +
		`{"seq":2,"kind":"leaf","status":"launch","label":"a"}` + "\n" +
		`{"seq":3,"kind":"leaf",`
	evs, partial := parseEventLines(chunk)
	if len(evs) != 2 {
		t.Fatalf("parsed %d complete events, want 2", len(evs))
	}
	if evs[0].Kind != "phase" || evs[0].Phase != "plan" {
		t.Fatalf("event 0 = %+v, want phase/plan", evs[0])
	}
	if evs[1].Status != "launch" || evs[1].Label != "a" {
		t.Fatalf("event 1 = %+v, want launch/a", evs[1])
	}
	if partial != `{"seq":3,"kind":"leaf",` {
		t.Fatalf("partial = %q, want the torn trailing line", partial)
	}
}

func TestParseEventLines_RejoinsPartialNextRead(t *testing.T) {
	// First read tears mid-line; feed the rest prefixed with the carried partial.
	evs, partial := parseEventLines(`{"seq":1,"kind":"leaf","stat`)
	if len(evs) != 0 || partial == "" {
		t.Fatalf("first read should yield no complete event + a partial, got evs=%d partial=%q", len(evs), partial)
	}
	evs2, partial2 := parseEventLines(partial + `us":"done","label":"a"}` + "\n")
	if len(evs2) != 1 || evs2[0].Status != "done" || evs2[0].Label != "a" {
		t.Fatalf("rejoined read = %+v (partial2=%q), want one done/a event", evs2, partial2)
	}
	if partial2 != "" {
		t.Fatalf("partial2 = %q, want empty (line completed)", partial2)
	}
}

func TestParseEventLines_SkipsBlankAndMalformed(t *testing.T) {
	chunk := "\n" + `not-json` + "\n" + `{"seq":5,"kind":"log","msg":"hi"}` + "\n"
	evs, partial := parseEventLines(chunk)
	if len(evs) != 1 || evs[0].Msg != "hi" {
		t.Fatalf("evs = %+v, want one log/hi (blank + malformed skipped)", evs)
	}
	if partial != "" {
		t.Fatalf("partial = %q, want empty", partial)
	}
}

func TestTailEvents_Incremental(t *testing.T) {
	// tailEvents reads only the bytes appended since the prior offset.
	dir := t.TempDir()
	path := filepath.Join(dir, "run.events")
	first := `{"seq":1,"kind":"phase","phase":"plan"}` + "\n"
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	evs, tail, reset := tailEvents(path, runTail{})
	if len(evs) != 1 || tail.offset != int64(len(first)) || reset {
		t.Fatalf("first tail: evs=%d offset=%d reset=%v, want 1 + %d + false", len(evs), tail.offset, reset, len(first))
	}
	// Append a second line; the next tail must read ONLY the new bytes.
	second := `{"seq":2,"kind":"leaf","status":"done","label":"a"}` + "\n"
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(second)
	_ = f.Close()
	evs2, tail2, reset2 := tailEvents(path, tail)
	if len(evs2) != 1 || evs2[0].Label != "a" || reset2 {
		t.Fatalf("incremental tail = %+v reset=%v, want only the new done/a event + no reset", evs2, reset2)
	}
	if tail2.offset != int64(len(first)+len(second)) {
		t.Fatalf("offset = %d, want %d", tail2.offset, len(first)+len(second))
	}
	// A re-tail with no new bytes yields nothing, offset unchanged.
	evs3, tail3, _ := tailEvents(path, tail2)
	if len(evs3) != 0 || tail3.offset != tail2.offset {
		t.Fatalf("no-op tail = %+v offset=%d, want empty + unchanged", evs3, tail3.offset)
	}
}

// TestTailEvents_ShrinkResets: the engine truncates the events file at the start of
// every (re)run, so a file shorter than the prior offset must signal reset=true and
// restart reading from the top (the prior history is stale).
func TestTailEvents_ShrinkResets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.events")
	// Read a multi-line file to advance the offset well past 0.
	orig := `{"seq":1,"kind":"phase","phase":"old1"}` + "\n" + `{"seq":2,"kind":"phase","phase":"old2"}` + "\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	_, tail, _ := tailEvents(path, runTail{})
	if tail.offset != int64(len(orig)) {
		t.Fatalf("setup offset = %d, want %d", tail.offset, len(orig))
	}
	// A (re)run truncates + rewrites a SHORTER stream.
	fresh := `{"seq":1,"kind":"phase","phase":"new1"}` + "\n"
	if err := os.WriteFile(path, []byte(fresh), 0o600); err != nil {
		t.Fatal(err)
	}
	evs, tail2, reset := tailEvents(path, tail)
	if !reset {
		t.Fatal("a shrunk file must report reset=true")
	}
	if len(evs) != 1 || evs[0].Phase != "new1" {
		t.Fatalf("after shrink tail = %+v, want the fresh new1 event read from the top", evs)
	}
	if tail2.offset != int64(len(fresh)) {
		t.Fatalf("post-shrink offset = %d, want %d", tail2.offset, len(fresh))
	}
}

func TestTailEvents_MissingFileNoCrash(t *testing.T) {
	evs, tail, reset := tailEvents(filepath.Join(t.TempDir(), "absent.events"), runTail{offset: 42})
	if len(evs) != 0 || reset {
		t.Fatalf("missing file should yield no events + no reset, got %d evs reset=%v", len(evs), reset)
	}
	if tail.offset != 42 {
		t.Fatalf("missing file should preserve the prior tail, got offset %d", tail.offset)
	}
}

func TestAppendLog_BoundedRing(t *testing.T) {
	var ring []string
	for i := 0; i < maxLogLines+50; i++ {
		ring = appendLog(ring, []eventLine{{Kind: "log", Msg: "line"}})
	}
	if len(ring) != maxLogLines {
		t.Fatalf("ring len = %d, want bounded to %d", len(ring), maxLogLines)
	}
}

// ---------------------------------------------------------------------------
// R2.5 DAG reconstruction + fallback
// ---------------------------------------------------------------------------

func TestBuildDAG_NestedGroups(t *testing.T) {
	// pipeline { parallel { leaf, leaf } } — nested by seq bracket order.
	evs := []eventLine{
		{Seq: 1, Kind: "group-open", GroupID: "g1", GroupTy: "pipeline"},
		{Seq: 2, Kind: "group-open", GroupID: "g2", GroupTy: "parallel"},
		{Seq: 3, Kind: "leaf", Status: "done", Label: "a"},
		{Seq: 4, Kind: "leaf", Status: "done", Label: "b"},
		{Seq: 5, Kind: "group-close", GroupID: "g2"},
		{Seq: 6, Kind: "group-close", GroupID: "g1"},
	}
	roots := buildDAG(evs)
	if len(roots) != 1 || !roots[0].group || roots[0].groupTy != "pipeline" {
		t.Fatalf("root = %+v, want one pipeline group", roots)
	}
	pipe := roots[0]
	if len(pipe.children) != 1 || pipe.children[0].groupTy != "parallel" {
		t.Fatalf("pipeline children = %+v, want one parallel group", pipe.children)
	}
	par := pipe.children[0]
	if len(par.children) != 2 || par.children[0].label != "a" || par.children[1].label != "b" {
		t.Fatalf("parallel leaves = %+v, want [a b]", par.children)
	}
}

func TestBuildDAG_NoGroupsFallsBackNil(t *testing.T) {
	evs := []eventLine{
		{Seq: 1, Kind: "phase", Phase: "plan"},
		{Seq: 2, Kind: "leaf", Status: "done", Label: "a"},
	}
	if got := buildDAG(evs); got != nil {
		t.Fatalf("no group events should yield nil (flat fallback), got %+v", got)
	}
}

func TestBuildDAG_UnmatchedCloseNoCrash(t *testing.T) {
	// A stray close (malformed stream) must degrade, never panic or pop the root.
	evs := []eventLine{
		{Seq: 1, Kind: "group-close", GroupID: "g0"},
		{Seq: 2, Kind: "group-open", GroupID: "g1", GroupTy: "parallel"},
		{Seq: 3, Kind: "leaf", Label: "a"},
		// no matching close for g1
	}
	roots := buildDAG(evs)
	if len(roots) != 1 || roots[0].groupTy != "parallel" || len(roots[0].children) != 1 {
		t.Fatalf("malformed stream = %+v, want one parallel with one leaf", roots)
	}
}

func TestWorkflowsView_DAGStructureAndFallback(t *testing.T) {
	runs := []subagent.WorkflowRun{
		{RunID: "run-dag", Name: "dag", StartedAt: "2026-05-01T00:00:00Z"},
		{RunID: "run-flat", Name: "flat", StartedAt: "2026-04-01T00:00:00Z",
			Phases: []subagent.RunPhase{{Title: "build"}}},
	}
	jobs := []subagent.Result{
		{RunID: "run-dag", Phase: "p", Label: "d1", Vendor: "glm", Status: "done", StartedAt: "2026-05-01T01:00:00Z"},
		{RunID: "run-dag", Phase: "p", Label: "d2", Vendor: "glm", Status: "done", StartedAt: "2026-05-01T01:00:01Z"},
		{RunID: "run-flat", Phase: "build", Label: "f1", Vendor: "kimi", Status: "running", StartedAt: "2026-04-01T01:00:00Z"},
	}
	evts := map[string][]eventLine{
		"run-dag": {
			{Seq: 1, Kind: "group-open", GroupID: "g1", GroupTy: "parallel"},
			{Seq: 2, Kind: "leaf", Status: "done", Label: "d1"},
			{Seq: 3, Kind: "leaf", Status: "done", Label: "d2"},
			{Seq: 4, Kind: "group-close", GroupID: "g1"},
		},
		// run-flat has NO events → flat phase tree.
	}
	m := workflowsModel(t, jobs, runs, evts)
	out := m.viewWorkflows()
	if !strings.Contains(out, "⇉ parallel (2)") {
		t.Fatalf("DAG run should render a parallel group header:\n%s", out)
	}
	if !strings.Contains(out, "▸ phase: build") {
		t.Fatalf("event-less run should fall back to the flat phase tree:\n%s", out)
	}
	// Every leaf still renders (selectable rows survive both paths).
	for _, lbl := range []string{"d1", "d2", "f1"} {
		if !strings.Contains(out, lbl) {
			t.Fatalf("leaf %q missing from board:\n%s", lbl, out)
		}
	}
}

// ---------------------------------------------------------------------------
// R2.3 metrics columns + totals
// ---------------------------------------------------------------------------

func TestHumanTokens(t *testing.T) {
	cases := map[int]string{0: "0", 999: "999", 1000: "1.0k", 50700: "50.7k", 2_000_000: "2.0M"}
	for in, want := range cases {
		if got := humanTokens(in); got != want {
			t.Errorf("humanTokens(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestWorkflowsView_RendersMetricsAndTotals(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-m", Name: "m",
		StartedAt: "2026-05-01T00:00:00Z", UpdatedAt: "2026-05-01T00:00:30Z"}}
	jobs := []subagent.Result{
		{RunID: "run-m", Phase: "p", Label: "a", Vendor: "glm", Status: "done",
			StartedAt: "2026-05-01T00:00:01Z", NumTurns: 3, CostUSD: 0.0123,
			Usage: &subagent.Usage{InputTokens: 50700, OutputTokens: 1200, CacheReadInputTokens: 800}},
	}
	m := workflowsModel(t, jobs, runs, nil)
	out := m.viewWorkflows()
	if !strings.Contains(out, "50.7k") {
		t.Fatalf("leaf row should humanize input tokens (50.7k):\n%s", out)
	}
	if !strings.Contains(out, "$0.0123") {
		t.Fatalf("leaf row should show cost:\n%s", out)
	}
	// Totals line: summed tokens + cost + elapsed (30s window).
	if !strings.Contains(out, "Σ tokens") || !strings.Contains(out, "30s elapsed") {
		t.Fatalf("run totals line missing tokens/elapsed:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// R2.2 leaf cursor over grouped rows + drill-in card
// ---------------------------------------------------------------------------

func TestWorkflowsCursor_WalksLeafRowsOnly(t *testing.T) {
	runs := []subagent.WorkflowRun{
		{RunID: "run-1", Name: "one", StartedAt: "2026-05-02T00:00:00Z",
			Phases: []subagent.RunPhase{{Title: "plan"}, {Title: "build"}}},
	}
	jobs := []subagent.Result{
		{RunID: "run-1", Phase: "plan", Label: "p1", JobID: "job-p1", StartedAt: "2026-05-02T00:01:00Z"},
		{RunID: "run-1", Phase: "build", Label: "b1", JobID: "job-b1", StartedAt: "2026-05-02T00:02:00Z"},
		{RunID: "run-1", Phase: "build", Label: "b2", JobID: "job-b2", StartedAt: "2026-05-02T00:03:00Z"},
	}
	m := workflowsModel(t, jobs, runs, nil)
	if n := m.workflowLeafCount(); n != 3 {
		t.Fatalf("leaf count = %d, want 3 (header lines excluded)", n)
	}
	// up at the top clamps.
	m, _ = press(t, m, "up")
	if m.workflowsCursor != 0 {
		t.Fatalf("up at top: cursor = %d, want 0", m.workflowsCursor)
	}
	// down lands on each leaf in groupByRun order (p1, b1, b2).
	m, _ = press(t, m, "down")
	if job, ok := m.selectedLeaf(); !ok || job.JobID != "job-b1" {
		t.Fatalf("cursor 1 leaf = %v/%q, want job-b1", ok, job.JobID)
	}
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "down") // clamp at the last leaf
	if m.workflowsCursor != 2 {
		t.Fatalf("cursor should clamp at the last leaf, got %d", m.workflowsCursor)
	}
	if job, _ := m.selectedLeaf(); job.JobID != "job-b2" {
		t.Fatalf("last leaf = %q, want job-b2", job.JobID)
	}
	if rid, _ := m.selectedRunID(); rid != "run-1" {
		t.Fatalf("selected run id = %q, want run-1", rid)
	}
}

func TestWorkflowsCursor_ClampsOnReload(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", StartedAt: "2026-05-02T00:00:00Z"}}
	jobs := []subagent.Result{
		{RunID: "run-1", Phase: "p", Label: "a", JobID: "ja", StartedAt: "2026-05-02T00:01:00Z"},
		{RunID: "run-1", Phase: "p", Label: "b", JobID: "jb", StartedAt: "2026-05-02T00:02:00Z"},
	}
	m := workflowsModel(t, jobs, runs, nil)
	m, _ = press(t, m, "down")
	if m.workflowsCursor != 1 {
		t.Fatalf("cursor = %d, want 1", m.workflowsCursor)
	}
	// A refresh that shrinks to one leaf must clamp the cursor back into range.
	m, _ = step(t, m, workflowsMsg{
		jobs:  []subagent.Result{{RunID: "run-1", Phase: "p", Label: "a", JobID: "ja", StartedAt: "2026-05-02T00:01:00Z"}},
		runs:  runs,
		epoch: m.workflowsEpoch,
	})
	if m.workflowsCursor != 0 {
		t.Fatalf("after shrink: cursor = %d, want 0 (clamped)", m.workflowsCursor)
	}
}

func TestWorkflowDetail_RendersPersistedIO(t *testing.T) {
	const prompt = "SUMMARIZE THIS PROMPT TEXT"
	const answer = "THE-VENDOR-ANSWER-DRILL-IN-ONLY"
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	jobsDir := filepath.Join(xdg, "cc-fleet", "subagent-jobs")
	if err := os.MkdirAll(jobsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobsDir, "job-x.prompt"), []byte(prompt), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobsDir, "job-x.answer"), []byte(answer), 0o600); err != nil {
		t.Fatal(err)
	}

	runs := []subagent.WorkflowRun{{RunID: "run-1", StartedAt: "2026-05-02T00:00:00Z"}}
	jobs := []subagent.Result{{RunID: "run-1", Phase: "p", Label: "a", JobID: "job-x",
		Status: "done", StartedAt: "2026-05-02T00:01:00Z"}}
	m := workflowsModel(t, jobs, runs, nil)

	// enter on the leaf opens the drill-in and dispatches the io read.
	m, cmd := press(t, m, "enter")
	if m.screen != screenWorkflowDetail || cmd == nil {
		t.Fatalf("enter on leaf: screen=%d cmd=%v, want detail + io-load cmd", m.screen, cmd)
	}
	// Run the io-load cmd and deliver its message.
	msg := cmd()
	m, _ = step(t, m, msg)
	if !m.wfDetailIO {
		t.Fatal("io should be marked present after reading the side files")
	}
	out := m.View()
	if !strings.Contains(out, prompt) {
		t.Fatalf("drill-in should render the prompt:\n%s", out)
	}
	// Collapsed by default: a short answer still shows (under the cap); the answer
	// appears ONLY in the card.
	if !strings.Contains(out, answer) {
		t.Fatalf("drill-in should render the answer:\n%s", out)
	}
	// The answer must NEVER appear on the board table itself.
	board := m.viewWorkflows()
	if strings.Contains(board, answer) {
		t.Fatalf("answer leaked into the board table:\n%s", board)
	}
	// esc returns to the board.
	m, _ = press(t, m, "esc")
	if m.screen != screenWorkflows {
		t.Fatalf("esc from detail: screen=%d, want screenWorkflows", m.screen)
	}
}

func TestWorkflowDetail_AbsentIOShowsNote(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg) // no side files written

	runs := []subagent.WorkflowRun{{RunID: "run-1", StartedAt: "2026-05-02T00:00:00Z"}}
	jobs := []subagent.Result{{RunID: "run-1", Phase: "p", Label: "a", JobID: "job-missing",
		Status: "done", StartedAt: "2026-05-02T00:01:00Z"}}
	m := workflowsModel(t, jobs, runs, nil)
	m, cmd := press(t, m, "enter")
	if m.screen != screenWorkflowDetail || cmd == nil {
		t.Fatalf("enter: screen=%d cmd=%v, want detail + cmd", m.screen, cmd)
	}
	m, _ = step(t, m, cmd())
	if m.wfDetailIO {
		t.Fatal("io should be absent (no side files)")
	}
	if out := m.View(); !strings.Contains(out, "not persisted") {
		t.Fatalf("absent io should show the not-persisted note:\n%s", out)
	}
}

func TestWorkflowDetail_ExpandToggle(t *testing.T) {
	// A prompt longer than the collapsed cap is truncated by default and whole when
	// expanded.
	long := strings.Repeat("x", wfCollapsedChars+200)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	jobsDir := filepath.Join(xdg, "cc-fleet", "subagent-jobs")
	_ = os.MkdirAll(jobsDir, 0o700)
	_ = os.WriteFile(filepath.Join(jobsDir, "job-l.prompt"), []byte(long), 0o600)

	runs := []subagent.WorkflowRun{{RunID: "run-1", StartedAt: "2026-05-02T00:00:00Z"}}
	jobs := []subagent.Result{{RunID: "run-1", Phase: "p", Label: "a", JobID: "job-l",
		Status: "done", StartedAt: "2026-05-02T00:01:00Z"}}
	m := workflowsModel(t, jobs, runs, nil)
	m, cmd := press(t, m, "enter")
	m, _ = step(t, m, cmd())
	if collapsed := m.View(); !strings.Contains(collapsed, "more chars") {
		t.Fatalf("collapsed long prompt should show a truncation marker:\n%s", collapsed)
	}
	m, _ = press(t, m, "e") // expand
	if !m.wfDetailExpand {
		t.Fatal("e should toggle expand on")
	}
	if expanded := m.View(); strings.Contains(expanded, "more chars") {
		t.Fatalf("expanded view should render the whole prompt (no truncation marker):\n%s", expanded)
	}
}

// ---------------------------------------------------------------------------
// R2.4 controls (x stop / r restart)
// ---------------------------------------------------------------------------

func TestWorkflowsControls_StopAndRestartDispatch(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-ctl", StartedAt: "2026-05-02T00:00:00Z"}}
	jobs := []subagent.Result{{RunID: "run-ctl", Phase: "p", Label: "a", JobID: "job-c",
		Status: "running", StartedAt: "2026-05-02T00:01:00Z"}}
	m := workflowsModel(t, jobs, runs, nil)
	if _, cmd := press(t, m, "x"); cmd == nil {
		t.Fatal("x on a leaf should dispatch a stop command")
	}
	if _, cmd := press(t, m, "r"); cmd == nil {
		t.Fatal("r on a leaf should dispatch a restart command")
	}
}

func TestWorkflowsControls_NoOpWhenNoLeaves(t *testing.T) {
	// A manifest-only run with zero jobs has no selectable leaf → x/r are no-ops.
	runs := []subagent.WorkflowRun{{RunID: "run-empty", StartedAt: "2026-05-02T00:00:00Z"}}
	m := workflowsModel(t, nil, runs, nil)
	if m.workflowLeafCount() != 0 {
		t.Fatalf("leaf count = %d, want 0", m.workflowLeafCount())
	}
	if _, cmd := press(t, m, "x"); cmd != nil {
		t.Fatal("x with no leaves should be a no-op (nil cmd)")
	}
	if _, cmd := press(t, m, "r"); cmd != nil {
		t.Fatal("r with no leaves should be a no-op (nil cmd)")
	}
}

func TestWorkflowCtlMsg_SurfacesStatusAndReloads(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-ctl", StartedAt: "2026-05-02T00:00:00Z"}}
	jobs := []subagent.Result{{RunID: "run-ctl", Phase: "p", Label: "a", JobID: "job-c",
		Status: "running", StartedAt: "2026-05-02T00:01:00Z"}}
	m := workflowsModel(t, jobs, runs, nil)
	m, cmd := step(t, m, workflowCtlMsg{verb: "stop", runID: "run-ctl", epoch: m.workflowsEpoch})
	if cmd == nil {
		t.Fatal("a control outcome should trigger a reload")
	}
	if m.workflowStatusErr || !strings.Contains(m.workflowStatus, "stop") {
		t.Fatalf("ok stop should set an ok status line: %q (err=%v)", m.workflowStatus, m.workflowStatusErr)
	}
	if out := m.viewWorkflows(); !strings.Contains(out, "stop") {
		t.Fatalf("board should surface the control status:\n%s", out)
	}
	// A refresh must NOT wipe the surfaced status.
	m, _ = step(t, m, workflowsMsg{jobs: jobs, runs: runs, epoch: m.workflowsEpoch})
	if m.workflowStatus == "" {
		t.Fatal("a workflows refresh must not clear the control status line")
	}
}

// TestWorkflowsLiveLog_RendersScrubbedLines: the live-log pane renders tailed
// events, CleanTitle-scrubbed; a control byte in an event msg must not reach the
// terminal raw.
func TestWorkflowsLiveLog_RendersScrubbedLines(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", StartedAt: "2026-05-02T00:00:00Z"}}
	m := workflowsModel(t, nil, runs, nil)
	// Deliver a refresh carrying new log lines (mirrors loadWorkflows output).
	m, _ = step(t, m, workflowsMsg{
		runs:     runs,
		logLines: []eventLine{{Kind: "log", Msg: "hello\x1b[31mworld"}, {Kind: "phase", Phase: "plan"}},
		epoch:    m.workflowsEpoch,
	})
	out := m.viewWorkflows()
	if !strings.Contains(out, "Live log") {
		t.Fatalf("board should render a live-log pane:\n%s", out)
	}
	if !strings.Contains(out, "world") || !strings.Contains(out, "plan") {
		t.Fatalf("live log should render the event text:\n%s", out)
	}
	// renderLogLine (the unstyled core) must drop the raw ESC byte so it can't reach
	// the terminal as an interpretable escape (the styled view also injects ESC for
	// dimming, so assert on the scrubbed line itself).
	if line := renderLogLine(eventLine{Kind: "log", Msg: "hello\x1b[31mworld"}); strings.ContainsRune(line, '\x1b') {
		t.Fatalf("renderLogLine leaked a raw ESC byte: %q", line)
	}
}

// TestWfDetailMsg_StaleNonceDropped: a leaf-A IO read landing after the user drilled
// into leaf-B (nonce bumped) must be dropped — the card keeps leaf-B's content and
// never shows leaf-A's answer (#1 wrong-answer race).
func TestWfDetailMsg_StaleNonceDropped(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", StartedAt: "2026-05-02T00:00:00Z"}}
	jobs := []subagent.Result{
		{RunID: "run-1", Phase: "p", Label: "A", JobID: "job-a", StartedAt: "2026-05-02T00:01:00Z"},
		{RunID: "run-1", Phase: "p", Label: "B", JobID: "job-b", StartedAt: "2026-05-02T00:02:00Z"},
	}
	m := workflowsModel(t, jobs, runs, nil)
	// Drill into leaf A (nonce -> 1), then immediately drill into leaf B (nonce -> 2)
	// before A's read returns.
	m, _ = press(t, m, "enter") // A
	nonceA := m.wfDetailNonce
	m, _ = press(t, m, "esc")   // back to board (cursor preserved on A)
	m, _ = press(t, m, "down")  // cursor -> B
	m, _ = press(t, m, "enter") // B (nonce bumped past A's)
	if m.wfDetailNonce == nonceA {
		t.Fatal("opening a second leaf should bump the detail nonce")
	}
	// A's slow read finally lands carrying the OLD nonce + A's content.
	m, _ = step(t, m, wfDetailMsg{nonce: nonceA, job: jobs[0], prompt: "A-PROMPT", answer: "A-ANSWER", present: true})
	if m.wfDetailJob.JobID != "job-b" {
		t.Fatalf("stale nonce overwrote the current card: job=%q, want job-b", m.wfDetailJob.JobID)
	}
	if m.wfDetailPrompt == "A-PROMPT" || m.wfDetailAnswer == "A-ANSWER" {
		t.Fatalf("stale leaf-A content leaked into leaf-B's card: prompt=%q answer=%q",
			m.wfDetailPrompt, m.wfDetailAnswer)
	}
	// The matching-nonce read for B DOES land.
	m, _ = step(t, m, wfDetailMsg{nonce: m.wfDetailNonce, job: jobs[1], prompt: "B-PROMPT", answer: "B-ANSWER", present: true})
	if m.wfDetailPrompt != "B-PROMPT" || m.wfDetailAnswer != "B-ANSWER" {
		t.Fatalf("matching-nonce read dropped: prompt=%q answer=%q", m.wfDetailPrompt, m.wfDetailAnswer)
	}
}

// TestWorkflowCtlMsg_StaleEpochDropped: a stop/restart result from a prior Workflows
// visit (epoch != current) must not mutate the fresh visit's status (#2).
func TestWorkflowCtlMsg_StaleEpochDropped(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", StartedAt: "2026-05-02T00:00:00Z"}}
	jobs := []subagent.Result{{RunID: "run-1", Phase: "p", Label: "a", JobID: "job-a",
		Status: "running", StartedAt: "2026-05-02T00:01:00Z"}}
	m := workflowsModel(t, jobs, runs, nil)                                          // workflowsEpoch == 1
	stale, cmd := step(t, m, workflowCtlMsg{verb: "stop", runID: "run-1", epoch: 0}) // from a prior visit
	if cmd != nil {
		t.Fatal("a stale-epoch control result should not trigger a reload")
	}
	if stale.workflowStatus != "" {
		t.Fatalf("stale-epoch control result mutated the status line: %q", stale.workflowStatus)
	}
	// A matching-epoch result DOES land.
	fresh, cmd := step(t, m, workflowCtlMsg{verb: "stop", runID: "run-1", epoch: m.workflowsEpoch})
	if cmd == nil || fresh.workflowStatus == "" {
		t.Fatalf("matching-epoch control result dropped: cmd=%v status=%q", cmd, fresh.workflowStatus)
	}
}

// TestWorkflowCtlMsg_ScrubsRunIDAndError: the status line scrubs a control byte in
// the run id and the error text before rendering (#4 unscrubbed status text).
func TestWorkflowCtlMsg_ScrubsRunIDAndError(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", StartedAt: "2026-05-02T00:00:00Z"}}
	m := workflowsModel(t, nil, runs, nil)
	m, _ = step(t, m, workflowCtlMsg{
		verb: "restart", runID: "\x1b[31mrun", err: errors.New("boom\x1b[0m"), epoch: m.workflowsEpoch,
	})
	if strings.ContainsRune(m.workflowStatus, '\x1b') {
		t.Fatalf("status line leaked a raw ESC byte from the run id/error: %q", m.workflowStatus)
	}
	if !m.workflowStatusErr {
		t.Fatal("a control error should style the status line as an error")
	}
}

// TestWorkflowsMsg_ShrinkReplacesEventHistory: when a run's events file is truncated
// (reset=true), the handler REPLACES that run's accumulated events + log rather than
// appending the fresh lines onto the stale ones (#3).
func TestWorkflowsMsg_ShrinkReplacesEventHistory(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", StartedAt: "2026-05-02T00:00:00Z"}}
	m := workflowsModel(t, nil, runs, nil)
	// First refresh: two events accumulate.
	old := []eventLine{{Kind: "phase", Phase: "old1"}, {Kind: "phase", Phase: "old2"}}
	m, _ = step(t, m, workflowsMsg{
		runs: runs, newEvts: map[string][]eventLine{"run-1": old}, logLines: old, epoch: m.workflowsEpoch,
	})
	if len(m.wfEvents["run-1"]) != 2 {
		t.Fatalf("after first refresh: %d events, want 2", len(m.wfEvents["run-1"]))
	}
	// A (re)run truncates the file: the refresh reports reset=true with the FRESH
	// (shorter) event set. History must be replaced, not appended.
	fresh := []eventLine{{Kind: "phase", Phase: "new1"}}
	m, _ = step(t, m, workflowsMsg{
		runs:     runs,
		newEvts:  map[string][]eventLine{"run-1": fresh},
		resets:   map[string]bool{"run-1": true},
		logLines: fresh,
		epoch:    m.workflowsEpoch,
	})
	if got := m.wfEvents["run-1"]; len(got) != 1 || got[0].Phase != "new1" {
		t.Fatalf("after truncate: events = %+v, want only the fresh new1 (history replaced)", got)
	}
	// The log ring is rebuilt from the corrected history — the stale old lines are gone.
	joined := strings.Join(m.wfLog, "\n")
	if strings.Contains(joined, "old1") || strings.Contains(joined, "old2") {
		t.Fatalf("log ring kept stale pre-truncate lines:\n%s", joined)
	}
	if !strings.Contains(joined, "new1") {
		t.Fatalf("log ring missing the fresh post-truncate line:\n%s", joined)
	}
}

// TestReadLeafIO_RejectsTraversalJobID: a malformed cached JobID (path separators /
// "..") must be rejected by ids.ValidateJobID so readLeafIO never reads outside
// subagent-jobs (#5 path traversal). It degrades to not-persisted.
func TestReadLeafIO_RejectsTraversalJobID(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	// Plant a file OUTSIDE subagent-jobs that a "../" id would otherwise reach.
	secretDir := filepath.Join(xdg, "cc-fleet")
	_ = os.MkdirAll(secretDir, 0o700)
	_ = os.WriteFile(filepath.Join(secretDir, "escape.prompt"), []byte("SECRET-OUTSIDE-JOBS"), 0o600)

	for _, bad := range []string{"", "..", "../escape", "a/b", `a\b`, "../../etc/passwd"} {
		prompt, answer, present := readLeafIO(bad)
		if present || prompt != "" || answer != "" {
			t.Fatalf("readLeafIO(%q) returned present=%v prompt=%q answer=%q, want rejected/empty",
				bad, present, prompt, answer)
		}
	}
}
