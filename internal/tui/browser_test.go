package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/sessiontitle"
	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/teardown"
)

// browserModel parks a model on the Agents Board with the given teammates / standalone jobs /
// workflow runs (+ their leaves) loaded, then opens the flat session browser via ctrl+f.
func browserModel(t *testing.T, tms []teardown.Teammate, jobs []subagent.Result, runs []subagent.WorkflowRun) Model {
	t.Helper()
	m := boardModel(t, nil, nil)
	m, _ = step(t, m, boardMsg{teammates: tms, jobs: jobs, runs: runs, epoch: m.boardEpoch})
	m, _ = step(t, m, keyMsg("ctrl+f"))
	if m.asMode != asModeBrowser {
		t.Fatalf("ctrl+f should open the browser, mode=%d", m.asMode)
	}
	return m
}

// TestBrowserFilterNarrowsAndResetsCursor: typing narrows the flat list over title/provider/lane
// (case-insensitive substring) and resets the cursor to the top on every edit.
func TestBrowserFilterNarrowsAndResetsCursor(t *testing.T) {
	m := browserModel(t,
		[]teardown.Teammate{{Name: "alice", Team: "squad", Provider: "kimi", PaneID: "%1", LeadSessionID: "s1", SpawnTime: 1_000}},
		[]subagent.Result{
			{JobID: "job-aaaa", Label: "indexer", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "s2"},
			{JobID: "job-bbbb", Label: "summary", Provider: "deepseek", Status: "done", StartedAt: "2026-06-01T00:00:02Z", LeadSessionID: "s2"},
		}, nil)

	if got := len(m.browserRows()); got != 3 {
		t.Fatalf("flat rows = %d, want 3 (2 jobs + 1 team)", got)
	}
	m, _ = press(t, m, "down") // move the cursor off the top
	if m.browserCursor != 1 {
		t.Fatalf("cursor = %d, want 1 after down", m.browserCursor)
	}

	// Filter by a provider only the team carries.
	for _, r := range "kimi" {
		m, _ = press(t, m, string(r))
	}
	if rows := m.filteredBrowserRows(); len(rows) != 1 || rows[0].ref.kind != browserTeam {
		t.Fatalf("filter kimi → %+v, want the one team row", rows)
	}
	if m.browserCursor != 0 {
		t.Fatalf("cursor = %d, want 0 (reset on filter edit)", m.browserCursor)
	}

	// Backspace widens; filter by a lane keyword.
	m = clearFilter(t, m)
	for _, r := range "subagent" {
		m, _ = press(t, m, string(r))
	}
	if rows := m.filteredBrowserRows(); len(rows) != 2 {
		t.Fatalf("filter by lane 'subagent' → %d rows, want the 2 jobs", len(rows))
	}
	// Filter by a title substring.
	m = clearFilter(t, m)
	for _, r := range "index" {
		m, _ = press(t, m, string(r))
	}
	if rows := m.filteredBrowserRows(); len(rows) != 1 || rows[0].title != "indexer" {
		t.Fatalf("filter index → %+v, want the indexer job", rows)
	}
}

// clearFilter backspaces the browser filter empty.
func clearFilter(t *testing.T, m Model) Model {
	t.Helper()
	for m.browserFilter != "" {
		m, _ = press(t, m, "backspace")
	}
	return m
}

// TestBrowserNewestFirstOrdering: rows sort newest-first by their time basis across lanes.
func TestBrowserNewestFirstOrdering(t *testing.T) {
	m := browserModel(t,
		[]teardown.Teammate{{Name: "alice", Team: "t", Provider: "glm", PaneID: "%1", LeadSessionID: "s-team", SpawnTime: 1_700_000_000_000}},
		[]subagent.Result{
			{JobID: "job-old", Label: "old-job", Provider: "glm", Status: "done", StartedAt: "2020-01-01T00:00:00Z", LeadSessionID: "s-j"},
		},
		[]subagent.WorkflowRun{
			{RunID: "run-x", Name: "newest-run", SessionID: "s-r", StartedAt: "2030-01-01T00:00:00Z"},
		})
	rows := m.browserRows()
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if rows[0].ref.kind != browserRun {
		t.Fatalf("newest row = %+v, want the 2030 run first", rows[0])
	}
	if rows[2].title != "old-job" {
		t.Fatalf("oldest row = %q, want old-job last", rows[2].title)
	}
}

// TestBrowserJobRowsStandaloneOnly: a job tagged with a RunID is NOT a standalone browser row
// (it belongs to its run); only RunID=="" jobs list as subagent rows.
func TestBrowserJobRowsStandaloneOnly(t *testing.T) {
	m := browserModel(t, nil,
		[]subagent.Result{
			{JobID: "job-free", Label: "free", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "s"},
			{JobID: "job-leaf", Label: "leaf", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:02Z", RunID: "run-1", Phase: "p"},
		},
		[]subagent.WorkflowRun{{RunID: "run-1", Name: "the-run", SessionID: "sR", StartedAt: "2026-06-01T00:00:00Z"}})
	for _, r := range m.browserRows() {
		if r.ref.kind == browserJob && r.jobID == "job-leaf" {
			t.Fatalf("a RunID-tagged leaf must not be a standalone job row: %+v", r)
		}
	}
	// The leaf surfaces ONLY through its run row, never doubled.
	var jobs, runs int
	for _, r := range m.browserRows() {
		switch r.ref.kind {
		case browserJob:
			jobs++
		case browserRun:
			runs++
		}
	}
	if jobs != 1 || runs != 1 {
		t.Fatalf("rows = %d standalone jobs + %d runs, want 1 + 1", jobs, runs)
	}
}

// TestBrowserEnterJobSetsFocus: Enter on a job row opens its existing entity detail by id.
func TestBrowserEnterJobSetsFocus(t *testing.T) {
	m := browserModel(t, nil,
		[]subagent.Result{{JobID: "job-target", Label: "target", Provider: "glm", Status: "done",
			StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "sess-job"}}, nil)
	m, _ = press(t, m, "enter")
	if m.asMode != asModeEntity || !m.asEntitySrc.jobs {
		t.Fatalf("enter on a job → mode=%d src.jobs=%v, want entity+jobs", m.asMode, m.asEntitySrc.jobs)
	}
	if m.focusedSessionID != "sess-job" {
		t.Fatalf("focusedSessionID = %q, want sess-job", m.focusedSessionID)
	}
	if j, ok := m.selectedJob(); !ok || j.JobID != "job-target" {
		t.Fatalf("selected job = %+v ok=%v, want job-target", j, ok)
	}
	if !m.browserOrigin {
		t.Fatal("browserOrigin must be set after a browser Enter")
	}
}

// TestBrowserEnterRunRootsSession: Enter on a run row roots the SESSION (not just focusedRunID)
// and lands on the run's Phases — required for refresh preservation + wfAscend.
func TestBrowserEnterRunRootsSession(t *testing.T) {
	m := browserModel(t, nil, nil,
		[]subagent.WorkflowRun{{RunID: "run-1", Name: "sweep", SessionID: "sess-run", Cwd: "/proj",
			StartedAt: "2026-06-01T00:00:00Z", Phases: []subagent.RunPhase{{Title: "map"}}}})
	m, _ = press(t, m, "enter")
	if m.asMode != asModeRunPhases {
		t.Fatalf("enter on a run → mode=%d, want runPhases", m.asMode)
	}
	if m.focusedRunID != "run-1" || m.focusedSessionID != "sess-run" {
		t.Fatalf("run/session focus = %q/%q, want run-1/sess-run", m.focusedRunID, m.focusedSessionID)
	}
	if m.focusedProject != "/proj" {
		t.Fatalf("focusedProject = %q, want /proj (from the run cwd)", m.focusedProject)
	}
	// The drill survives a refresh (focusedGroup + focusedSession both valid).
	jobs := []subagent.Result{{RunID: "run-1", Phase: "map", Label: "m1", Status: "done", JobID: "j", StartedAt: "2026-06-01T00:00:01Z"}}
	runs := []subagent.WorkflowRun{{RunID: "run-1", Name: "sweep", SessionID: "sess-run", Cwd: "/proj",
		StartedAt: "2026-06-01T00:00:00Z", Phases: []subagent.RunPhase{{Title: "map"}}}}
	m, _ = step(t, m, boardMsg{jobs: jobs, runs: runs, epoch: m.boardEpoch})
	if m.asMode != asModeRunPhases || m.focusedRunID != "run-1" {
		t.Fatalf("run drill not preserved across refresh: mode=%d run=%q", m.asMode, m.focusedRunID)
	}
}

// TestBrowserEnterTeamSetsFocus: Enter on a team row opens its entity (team) detail by id.
func TestBrowserEnterTeamSetsFocus(t *testing.T) {
	m := browserModel(t,
		[]teardown.Teammate{
			{Name: "alice", Team: "squad", Provider: "glm", PaneID: "%1", LeadSessionID: "sess-team", SpawnTime: 1_000},
			{Name: "bob", Team: "squad", Provider: "glm", PaneID: "%2", LeadSessionID: "sess-team", SpawnTime: 1_001},
		}, nil, nil)
	m, _ = press(t, m, "enter")
	if m.asMode != asModeEntity || m.asEntitySrc.team != "squad" {
		t.Fatalf("enter on a team → mode=%d src.team=%q, want entity+squad", m.asMode, m.asEntitySrc.team)
	}
	if m.focusedSessionID != "sess-team" {
		t.Fatalf("focusedSessionID = %q, want sess-team", m.focusedSessionID)
	}
	if m.asEntityCursor != 0 {
		t.Fatalf("team entity cursor = %d, want 0 (first member)", m.asEntityCursor)
	}
}

// TestBrowserCursorReanchorsAcrossRefresh: when a refresh inserts a newer row ABOVE the cursor,
// the cursor re-finds its anchored row by identity instead of staying at the same index.
func TestBrowserCursorReanchorsAcrossRefresh(t *testing.T) {
	jobs := []subagent.Result{
		{JobID: "job-keep", Label: "keep", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:02Z", LeadSessionID: "s"},
		{JobID: "job-old", Label: "old", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "s"},
	}
	m := browserModel(t, nil, jobs, nil)
	// Cursor on the SECOND row (job-old): rows are newest-first, so index 1 is job-old.
	m, _ = press(t, m, "down")
	if m.browserAnchor.id != "job-old" {
		t.Fatalf("anchor = %q, want job-old", m.browserAnchor.id)
	}
	// A refresh inserts a NEWER job above both → job-old shifts from index 1 to index 2.
	withNew := append([]subagent.Result{
		{JobID: "job-new", Label: "new", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:09Z", LeadSessionID: "s"},
	}, jobs...)
	m, _ = step(t, m, boardMsg{jobs: withNew, epoch: m.boardEpoch})
	rows := m.filteredBrowserRows()
	if m.browserCursor >= len(rows) || rows[m.browserCursor].ref.id != "job-old" {
		t.Fatalf("cursor=%d row=%q after insert, want it to still point at job-old", m.browserCursor, rows[m.browserCursor].ref.id)
	}
}

// TestBrowserEnteredJobEntityReanchors: after entering a job's detail from the browser, a
// refresh that inserts a newer job above keeps the L3 cursor on the SAME job (identity anchor),
// not drifting to the neighbor that now occupies its old index.
func TestBrowserEnteredJobEntityReanchors(t *testing.T) {
	jobs := []subagent.Result{
		{JobID: "job-keep", Label: "keep", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:02Z", LeadSessionID: "s"},
		{JobID: "job-other", Label: "other", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "s"},
	}
	m := browserModel(t, nil, jobs, nil)
	// The two jobs share session "s"; entering it lands on the jobs entity list. Filter to the
	// keep job so Enter targets it deterministically.
	for _, r := range "keep" {
		m, _ = press(t, m, string(r))
	}
	m, _ = press(t, m, "enter")
	if j, ok := m.selectedJob(); !ok || j.JobID != "job-keep" {
		t.Fatalf("entered job = %+v ok=%v, want job-keep", j, ok)
	}
	// A refresh adds a NEWER job to the same session → s.jobs reorders, job-keep's index shifts.
	withNew := append([]subagent.Result{
		{JobID: "job-new", Label: "new", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:09Z", LeadSessionID: "s"},
	}, jobs...)
	m, _ = step(t, m, boardMsg{jobs: withNew, epoch: m.boardEpoch})
	if j, ok := m.selectedJob(); !ok || j.JobID != "job-keep" {
		t.Fatalf("after refresh selected job = %+v ok=%v, want still job-keep (identity anchor)", j, ok)
	}
}

// TestBrowserEscReturnsLeftAscends: esc from a browser-entered detail returns to the browser;
// ← does normal board ascent (and drops the browser-origin link).
func TestBrowserEscReturnsLeftAscends(t *testing.T) {
	job := []subagent.Result{{JobID: "job-x", Label: "x", Provider: "glm", Status: "done",
		StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "sess-x"}}

	// esc path: detail → browser.
	m := browserModel(t, nil, job, nil)
	m, _ = press(t, m, "enter")
	if m.asMode != asModeEntity {
		t.Fatalf("setup: mode=%d, want entity", m.asMode)
	}
	m, _ = press(t, m, "esc")
	if m.asMode != asModeBrowser {
		t.Fatalf("esc from a browser-entered detail → mode=%d, want browser", m.asMode)
	}
	if m.browserOrigin {
		t.Fatal("browserOrigin should be cleared after returning to the browser")
	}

	// left path: detail → normal board ascent, origin cleared.
	m2 := browserModel(t, nil, job, nil)
	m2, _ = press(t, m2, "enter")
	m2, _ = press(t, m2, "left")
	if m2.asMode == asModeBrowser {
		t.Fatal("left must do normal board ascent, not return to the browser")
	}
	if m2.browserOrigin {
		t.Fatal("left must clear the browser-origin link")
	}
}

// TestBrowserEnterSeedsBoxCursor: entering a row seeds asBoxCursor to the entity's L2 row, so a
// later ← into the boxes level lands on the right run/team/job — not a stale row that a follow-up
// d/p/s would hit.
func TestBrowserEnterSeedsBoxCursor(t *testing.T) {
	// A multi-kind session (one team + one job): box order is teams, then jobs, so the job sits at
	// continuum index 1 — entering it must seed asBoxCursor=1, not leave it at the team (index 0).
	m := browserModel(t,
		[]teardown.Teammate{{Name: "alice", Team: "squad", Provider: "glm", PaneID: "%1", LeadSessionID: "sess", SpawnTime: 1_000}},
		[]subagent.Result{{JobID: "job-1", Label: "indexer", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "sess"}},
		nil)
	for _, r := range "indexer" { // filter so Enter targets the job deterministically
		m, _ = press(t, m, string(r))
	}
	m, _ = press(t, m, "enter")
	if _, ok := m.selectedJob(); !ok {
		t.Fatal("setup: expected to enter the job's entity detail")
	}
	m, _ = press(t, m, "left") // ← ascends to the boxes level
	if m.asMode != asModeBoxes {
		t.Fatalf("left → mode=%d, want boxes", m.asMode)
	}
	ref, ok := m.boxRowRef()
	if !ok || ref.jobID != "job-1" {
		t.Fatalf("boxes cursor = %+v ok=%v, want the entered job-1", ref, ok)
	}
}

// TestBrowserAnchorResyncsWhenRowVanishes: when the anchored row disappears, the cursor re-anchors
// to the row it now lands on, instead of holding a dead identity that every later refresh re-fails.
func TestBrowserAnchorResyncsWhenRowVanishes(t *testing.T) {
	jobs := []subagent.Result{
		{JobID: "job-a", Label: "a", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:03Z", LeadSessionID: "s"},
		{JobID: "job-b", Label: "b", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:02Z", LeadSessionID: "s"},
	}
	m := browserModel(t, nil, jobs, nil)
	m, _ = press(t, m, "down") // anchor on job-b (index 1)
	if m.browserAnchor.id != "job-b" {
		t.Fatalf("anchor = %q, want job-b", m.browserAnchor.id)
	}
	m, _ = step(t, m, boardMsg{jobs: jobs[:1], epoch: m.boardEpoch}) // job-b vanishes
	if m.browserAnchor.id == "job-b" {
		t.Fatalf("anchor still %q after the row vanished, want a re-sync", m.browserAnchor.id)
	}
	rows := m.filteredBrowserRows()
	if m.browserCursor >= len(rows) || m.browserAnchor != rows[m.browserCursor].ref {
		t.Fatalf("anchor %+v not synced to cursor row %d", m.browserAnchor, m.browserCursor)
	}
}

// TestBrowserRunAgentBackKeys: a browser-entered run drilled to the Agent level retraces to Phases
// on esc (the browser-origin return lives only at the entry/Phases level), then esc at Phases
// returns to the browser.
func TestBrowserRunAgentBackKeys(t *testing.T) {
	runs := []subagent.WorkflowRun{{RunID: "run-1", Name: "sweep", SessionID: "sess-run", Cwd: "/proj",
		StartedAt: "2026-06-01T00:00:00Z", Phases: []subagent.RunPhase{{Title: "map"}}}}
	jobs := []subagent.Result{{RunID: "run-1", Phase: "map", Label: "m1", Status: "done", JobID: "j1", StartedAt: "2026-06-01T00:00:01Z"}}
	m := browserModel(t, nil, jobs, runs)
	m, _ = press(t, m, "enter") // run → Phases
	m, _ = press(t, m, "enter") // Phases → Agent
	if m.asMode != asModeRunAgent {
		t.Fatalf("setup: mode=%d, want runAgent", m.asMode)
	}
	m, _ = press(t, m, "esc") // retrace one level to Phases, not straight to the browser
	if m.asMode != asModeRunPhases {
		t.Fatalf("esc at agent → mode=%d, want runPhases (retrace)", m.asMode)
	}
	if !m.browserOrigin {
		t.Fatal("browser-origin must survive the Agent→Phases retrace")
	}
	m, _ = press(t, m, "esc") // entry level returns to the browser
	if m.asMode != asModeBrowser {
		t.Fatalf("esc at phases → mode=%d, want browser", m.asMode)
	}
}

// TestBrowserOriginClearedOnRefreshDemotion: when a browser-entered run vanishes mid-refresh and
// the board demotes to boxes, the browser back-link is dropped — so hand-descending into a
// different detail and pressing esc does normal ascent, not a jump back to the browser.
func TestBrowserOriginClearedOnRefreshDemotion(t *testing.T) {
	// A run and a team share session sR; the team keeps sR alive after the run vanishes, so the
	// board demotes to boxes (not a full reroute) — the exact spot the back-link must drop.
	tms := []teardown.Teammate{
		{Name: "alice", Team: "squad", Provider: "glm", PaneID: "%1", LeadSessionID: "sR", SpawnTime: 1_000},
		{Name: "bob", Team: "squad", Provider: "glm", PaneID: "%2", LeadSessionID: "sR", SpawnTime: 1_001},
	}
	runs := []subagent.WorkflowRun{{RunID: "run-a", Name: "sweep", SessionID: "sR", Cwd: "/proj",
		StartedAt: "2026-06-01T00:00:05Z", Phases: []subagent.RunPhase{{Title: "map"}}}}
	m := browserModel(t, tms, nil, runs)
	m, _ = press(t, m, "enter") // the run is the newest row
	if m.asMode != asModeRunPhases || !m.browserOrigin {
		t.Fatalf("setup: mode=%d origin=%v, want runPhases+origin", m.asMode, m.browserOrigin)
	}
	// run-a vanishes; sR survives via the team → demote to boxes, origin dropped.
	m, _ = step(t, m, boardMsg{teammates: tms, runs: nil, epoch: m.boardEpoch})
	if m.asMode != asModeBoxes || m.browserOrigin {
		t.Fatalf("after demote: mode=%d origin=%v, want boxes + origin cleared", m.asMode, m.browserOrigin)
	}
	m, _ = press(t, m, "enter") // hand-descend into the team's detail
	if m.asMode != asModeEntity {
		t.Fatalf("descend → mode=%d, want entity", m.asMode)
	}
	m, _ = press(t, m, "esc")
	if m.asMode == asModeBrowser {
		t.Fatal("esc from a hand-opened detail must not jump to the browser after the origin was dropped")
	}
}

// TestBrowserEnterJobRootsProject: entering a job whose session lives in a different project than
// the board last showed roots focusedProject on that session's project, so an immediate ← ascends
// into the right project list (not the stale one) before the next refresh.
func TestBrowserEnterJobRootsProject(t *testing.T) {
	m := boardModel(t, nil, nil)
	jobs := []subagent.Result{
		{JobID: "job-a", Label: "alpha", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:02Z", LeadSessionID: "sA"},
		{JobID: "job-b", Label: "bravo", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "sB"},
	}
	meta := map[string]sessiontitle.Meta{"sA": {Cwd: "/projA"}, "sB": {Cwd: "/projB"}}
	m, _ = step(t, m, boardMsg{jobs: jobs, sessionMeta: meta, epoch: m.boardEpoch})
	m, _ = step(t, m, keyMsg("ctrl+f"))
	for _, r := range "bravo" { // filter to job-b (in /projB)
		m, _ = press(t, m, string(r))
	}
	m, _ = press(t, m, "enter")
	if j, ok := m.selectedJob(); !ok || j.JobID != "job-b" {
		t.Fatalf("entered job = %+v, want job-b", j)
	}
	if m.focusedProject != "/projB" {
		t.Fatalf("focusedProject = %q, want /projB (the entered job's session project)", m.focusedProject)
	}
}

// TestBrowserOriginClearedWhenEnteredJobVanishesSiblingRemains: when a browser-entered job is
// removed mid-refresh but a sibling job keeps the session at the entity level, the cursor falls to
// the sibling and the browser back-link is dropped — esc must then do normal ascent.
func TestBrowserOriginClearedWhenEnteredJobVanishesSiblingRemains(t *testing.T) {
	jobs := []subagent.Result{
		{JobID: "job-a", Label: "alpha", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:02Z", LeadSessionID: "s"},
		{JobID: "job-b", Label: "bravo", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "s"},
	}
	m := browserModel(t, nil, jobs, nil)
	for _, r := range "bravo" { // enter job-b
		m, _ = press(t, m, string(r))
	}
	m, _ = press(t, m, "enter")
	if j, ok := m.selectedJob(); !ok || j.JobID != "job-b" {
		t.Fatalf("entered job = %+v, want job-b", j)
	}
	// job-b vanishes; sibling job-a keeps the session at asModeEntity → cursor falls to job-a.
	m, _ = step(t, m, boardMsg{jobs: jobs[:1], epoch: m.boardEpoch})
	if m.browserOrigin {
		t.Fatal("browserOrigin must clear when the entered job vanishes and the cursor falls to a sibling")
	}
	m, _ = press(t, m, "esc")
	if m.asMode == asModeBrowser {
		t.Fatal("esc must not return to the browser after the entered job vanished")
	}
}

// TestBrowserEnteredRunKeepsOriginAcrossRefresh: a prior job-detail visit leaves asEntitySrc set;
// entering a RUN from the browser must keep its back-link across a refresh — the stale entity state
// must not be read as a live entity focus and clear browserOrigin.
func TestBrowserEnteredRunKeepsOriginAcrossRefresh(t *testing.T) {
	jobs := []subagent.Result{{JobID: "job-z", Label: "zeta", Provider: "glm", Status: "done",
		StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "sM"}}
	runs := []subagent.WorkflowRun{{RunID: "run-r", Name: "sweep", SessionID: "sM", Cwd: "/proj",
		StartedAt: "2026-06-01T00:00:05Z", Phases: []subagent.RunPhase{{Title: "map"}}}}
	m := browserModel(t, nil, jobs, runs)
	// Visit the job first (leaves asEntitySrc.jobs set), then return to the browser.
	for _, r := range "zeta" {
		m, _ = press(t, m, string(r))
	}
	m, _ = press(t, m, "enter")
	if !m.asEntitySrc.jobs {
		t.Fatal("setup: expected asEntitySrc.jobs after entering the job")
	}
	m, _ = press(t, m, "esc")
	m = clearFilter(t, m)
	// Now enter the run; asEntitySrc.jobs is still set from the prior visit.
	for _, r := range "sweep" {
		m, _ = press(t, m, string(r))
	}
	m, _ = press(t, m, "enter")
	if m.asMode != asModeRunPhases || !m.browserOrigin {
		t.Fatalf("setup: mode=%d origin=%v, want runPhases+origin", m.asMode, m.browserOrigin)
	}
	m, _ = step(t, m, boardMsg{jobs: jobs, runs: runs, epoch: m.boardEpoch})
	if !m.browserOrigin {
		t.Fatal("a browser-entered run must keep browserOrigin across refresh despite stale entity state")
	}
	m, _ = press(t, m, "esc")
	if m.asMode != asModeBrowser {
		t.Fatalf("esc from the browser-entered run → mode=%d, want browser", m.asMode)
	}
}

// TestBrowserShowsBoardWarnings: the browser shares the board's persistent warning tail, so a
// jobs-scan failure surfaces here too — a partial list never looks clean.
func TestBrowserShowsBoardWarnings(t *testing.T) {
	m := boardModel(t, nil, nil)
	m, _ = step(t, m, boardMsg{jobsErr: errors.New("scan boom"), epoch: m.boardEpoch})
	m, _ = step(t, m, keyMsg("ctrl+f"))
	if m.asMode != asModeBrowser {
		t.Fatalf("setup: mode=%d, want browser", m.asMode)
	}
	if out := m.View(); !strings.Contains(out, "jobs unavailable") {
		t.Errorf("browser view must surface the jobs-unavailable warning\n---\n%s", out)
	}
}

// TestBrowserEscReturnsToBoard: esc from the browser list closes back to the board level ctrl+f was
// opened from — it does NOT exit to the Model Providers hub.
func TestBrowserEscReturnsToBoard(t *testing.T) {
	m := browserModel(t, nil,
		[]subagent.Result{{JobID: "j", Label: "x", Provider: "glm", Status: "done",
			StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "s"}}, nil)
	want := m.browserReturnMode
	m, _ = press(t, m, "esc")
	if m.screen != screenSpawn {
		t.Fatalf("esc from browser → screen=%d, want screenSpawn (stay on the board, not the hub)", m.screen)
	}
	if m.asMode == asModeBrowser || m.asMode != want {
		t.Fatalf("esc from browser → asMode=%d, want %d (the mode ctrl+f opened from)", m.asMode, want)
	}
}

// TestBrowserFilterMatchesProject: typing a project / directory substring narrows to the rows in
// that project — the session's dir is part of the filter corpus (and its basename shows on the row).
func TestBrowserFilterMatchesProject(t *testing.T) {
	m := boardModel(t, nil, nil)
	jobs := []subagent.Result{
		{JobID: "job-a", Label: "alpha", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:02Z", LeadSessionID: "sA"},
		{JobID: "job-b", Label: "bravo", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "sB"},
	}
	meta := map[string]sessiontitle.Meta{"sA": {Cwd: "/home/me/payments"}, "sB": {Cwd: "/home/me/billing"}}
	m, _ = step(t, m, boardMsg{jobs: jobs, sessionMeta: meta, epoch: m.boardEpoch})
	m, _ = step(t, m, keyMsg("ctrl+f"))
	for _, r := range "payments" { // a directory only job-a's session lives in
		m, _ = press(t, m, string(r))
	}
	rows := m.filteredBrowserRows()
	if len(rows) != 1 || rows[0].ref.id != "job-a" {
		t.Fatalf("filter 'payments' → %+v, want only job-a (in /home/me/payments)", rows)
	}
}

// TestBrowserRowShowsAndFiltersSession: a subagent/workflow row carries its parent session's title —
// shown after the label and matchable by the filter — so a label-less job is identifiable; the
// session id is filterable too.
func TestBrowserRowShowsAndFiltersSession(t *testing.T) {
	m := boardModel(t, nil, nil)
	jobs := []subagent.Result{ // no label → falls back to a short id, so the session title is the human anchor
		{JobID: "deadbeef0000", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "sess-abc"},
	}
	meta := map[string]sessiontitle.Meta{"sess-abc": {Cwd: "/proj", Title: "auth refactor"}}
	m, _ = step(t, m, boardMsg{jobs: jobs, sessionMeta: meta, epoch: m.boardEpoch})
	m, _ = step(t, m, keyMsg("ctrl+f"))
	if out := m.View(); !strings.Contains(out, "auth refactor") {
		t.Errorf("browser row must show the parent session title\n---\n%s", out)
	}
	for _, r := range "auth" { // filter by the session title
		m, _ = press(t, m, string(r))
	}
	if rows := m.filteredBrowserRows(); len(rows) != 1 {
		t.Fatalf("filter 'auth' (session title) → %d rows, want 1", len(rows))
	}
	m = clearFilter(t, m)
	for _, r := range "sess-abc" { // filter by the session id
		m, _ = press(t, m, string(r))
	}
	if rows := m.filteredBrowserRows(); len(rows) != 1 {
		t.Fatalf("filter by session id → %d rows, want 1", len(rows))
	}
}

// TestBrowserSanitizesMetadata: every displayed metadata field carrying a terminal control byte —
// the provider (from on-disk records) and the project basename (raw cwd) — is CleanTitle-scrubbed
// before display (the board's render-time invariant), never emitted raw.
func TestBrowserSanitizesMetadata(t *testing.T) {
	m := boardModel(t, nil, nil)
	jobs := []subagent.Result{{JobID: "j", Label: "x", Provider: "gl\am", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "s"}} // \a (BEL) in provider
	meta := map[string]sessiontitle.Meta{"s": {Cwd: "/home/me/pro\aj"}}                                                                           // and in the cwd basename
	m, _ = step(t, m, boardMsg{jobs: jobs, sessionMeta: meta, epoch: m.boardEpoch})
	m, _ = step(t, m, keyMsg("ctrl+f"))
	if strings.ContainsRune(m.View(), '\a') {
		t.Error("provider / project metadata with a control byte must be CleanTitle-scrubbed before display")
	}
}

// TestBrowserView renders the flat list with its title/provider/lane metadata + footer.
func TestBrowserView(t *testing.T) {
	m := browserModel(t, nil,
		[]subagent.Result{{JobID: "job-v", Label: "viewable", Provider: "glm", Status: "done",
			StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "s"}}, nil)
	out := m.View()
	for _, want := range []string{"viewable", "glm", "subagent", "type to search"} {
		if !strings.Contains(out, want) {
			t.Errorf("browser view missing %q\n---\n%s", want, out)
		}
	}
}
