package tui

import (
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/sessiontitle"
	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/teardown"
)

// TestBrowserTeamRowShowsLauncher: a codex-launched team row carries the "codex" launcher tag
// (not a bare thread id) — the browser divider alone isn't a reliable per-row signal.
func TestBrowserTeamRowShowsLauncher(t *testing.T) {
	tms := []teardown.Teammate{
		{Name: "alice", Team: "squad", Provider: "kimi", PaneID: "%1", LeadSessionID: "codex:0199abcd", SpawnTime: 1000},
	}
	m := browserModel(t, tms, nil, nil)
	var teamRow *browserRow
	rows := m.browserRows()
	for i := range rows {
		if rows[i].ref.kind == browserTeam {
			teamRow = &rows[i]
		}
	}
	if teamRow == nil {
		t.Fatal("no team row in the browser")
	}
	if !strings.Contains(teamRow.title, "codex") {
		t.Errorf("codex team row title = %q, want it to carry the codex launcher tag", teamRow.title)
	}
}

func TestSessionRank(t *testing.T) {
	cases := []struct {
		id   string
		want int
	}{
		{"claude-sess", 0},
		{"abc123", 0},
		{"codex:0199abcd", 1},
		{"", 2},
	}
	for _, c := range cases {
		if got := sessionRank(c.id); got != c.want {
			t.Errorf("sessionRank(%q) = %d, want %d", c.id, got, c.want)
		}
	}
}

func sessionIDs(sessions []asSession) []string {
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.sessionID
	}
	return ids
}

func TestSinkCodexSessions(t *testing.T) {
	// Input is groupSessions order: non-empty newest-first (codex interleaved), "" last.
	in := []asSession{
		{sessionID: "claude-a"},
		{sessionID: "codex:t1"},
		{sessionID: "claude-b"},
		{sessionID: ""},
	}
	out := sinkCodexSessions(in)
	got := strings.Join(sessionIDs(out), ",")
	// codex sinks below the claude group (whose newest-first order is kept), "" stays last.
	if want := "claude-a,claude-b,codex:t1,"; got != want {
		t.Errorf("sink order = %q, want %q", got, want)
	}
	// The input slice is not mutated.
	if in[1].sessionID != "codex:t1" {
		t.Errorf("sinkCodexSessions mutated its input: %v", sessionIDs(in))
	}

	// No codex → returned in the same order (a codex-free board is identical).
	noCodex := []asSession{{sessionID: "claude-a"}, {sessionID: "claude-b"}, {sessionID: ""}}
	if got := strings.Join(sessionIDs(sinkCodexSessions(noCodex)), ","); got != "claude-a,claude-b," {
		t.Errorf("no-codex sink = %q, want it unchanged", got)
	}
	// No codex, "(no session)" NOT last in the input → still returned unchanged, never reordered.
	midEmpty := []asSession{{sessionID: ""}, {sessionID: "claude-a"}}
	if got := strings.Join(sessionIDs(sinkCodexSessions(midEmpty)), ","); got != ",claude-a" {
		t.Errorf("no-codex sink reordered an empty-first input: got %q, want \",claude-a\"", got)
	}
}

func TestFirstCodexSession(t *testing.T) {
	mixed := []asSession{{sessionID: "claude-a"}, {sessionID: "codex:t1"}, {sessionID: ""}}
	if got := firstCodexSession(mixed); got != 1 {
		t.Errorf("mixed: firstCodexSession = %d, want 1", got)
	}
	if got := firstCodexSession([]asSession{{sessionID: "claude-a"}, {sessionID: ""}}); got != -1 {
		t.Errorf("no codex: firstCodexSession = %d, want -1", got)
	}
	// All-codex: boundary is index 0, so the L1 splice (gated on idx>0) skips the divider.
	if got := firstCodexSession([]asSession{{sessionID: "codex:t1"}, {sessionID: "codex:t2"}}); got != 0 {
		t.Errorf("all codex: firstCodexSession = %d, want 0", got)
	}
}

func TestBrowserSessionRank(t *testing.T) {
	cases := []struct {
		row  browserRow
		want int
	}{
		{browserRow{ref: browserRef{kind: browserJob}, leadSessionID: "codex:t1"}, 1},
		{browserRow{ref: browserRef{kind: browserJob}, leadSessionID: "claude-s"}, 0},
		{browserRow{ref: browserRef{kind: browserJob}, leadSessionID: ""}, 2},
		{browserRow{ref: browserRef{kind: browserRun}, sessionID: "codex:t2"}, 1}, // a run carries its launcher in sessionID
		{browserRow{ref: browserRef{kind: browserRun}, sessionID: "claude-s"}, 0},
		{browserRow{ref: browserRef{kind: browserTeam}, leadSessionID: "codex:t3"}, 1},
	}
	for _, c := range cases {
		if got := browserSessionRank(c.row); got != c.want {
			t.Errorf("browserSessionRank(kind=%d id=%q/%q) = %d, want %d",
				c.row.ref.kind, c.row.leadSessionID, c.row.sessionID, got, c.want)
		}
	}
}

// TestAsProjectsSinksCodex: within a project, codex sessions group below the non-codex
// ones — applied after groupProjects so the projects' own order is untouched.
func TestAsProjectsSinksCodex(t *testing.T) {
	jobs := []subagent.Result{
		{JobID: "j1", Status: "done", StartedAt: "2026-06-01T00:00:04Z", LeadSessionID: "claude-a"},
		{JobID: "j2", Status: "done", StartedAt: "2026-06-01T00:00:03Z", LeadSessionID: "codex:t1"},
		{JobID: "j3", Status: "done", StartedAt: "2026-06-01T00:00:02Z", LeadSessionID: "claude-b"},
	}
	meta := map[string]sessiontitle.Meta{
		"claude-a": {Cwd: "/proj"}, "codex:t1": {Cwd: "/proj"}, "claude-b": {Cwd: "/proj"},
	}
	m := boardModel(t, nil, nil)
	m, _ = step(t, m, boardMsg{jobs: jobs, sessionMeta: meta, epoch: m.boardEpoch})

	var proj asProject
	for _, p := range m.asProjects() {
		if p.dir == "/proj" {
			proj = p
		}
	}
	if got := strings.Join(sessionIDs(proj.sessions), ","); got != "claude-a,claude-b,codex:t1" {
		t.Errorf("project sessions = %q, want claude-a,claude-b,codex:t1 (codex sunk)", got)
	}
}

func browserTitles(rows []browserRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.title
	}
	return out
}

// TestBrowserGroupsCodexUnderDivider: with codex present the flat list groups codex (and
// "(no session)") below the non-codex rows, and the rendered browser shows the divider.
func TestBrowserGroupsCodexUnderDivider(t *testing.T) {
	jobs := []subagent.Result{
		{JobID: "j1", Label: "claude-new", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:04Z", LeadSessionID: "claude-a"},
		{JobID: "j2", Label: "codex-mid", Provider: "minimax", Status: "done", StartedAt: "2026-06-01T00:00:03Z", LeadSessionID: "codex:t1"},
		{JobID: "j3", Label: "claude-old", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:02Z", LeadSessionID: "claude-b"},
		{JobID: "j4", Label: "orphan", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: ""},
	}
	m := browserModel(t, nil, jobs, nil)
	rows := m.browserRows()
	// non-codex newest-first, then codex, then "(no session)".
	want := []string{"claude-new", "claude-old", "codex-mid", "orphan"}
	if got := strings.Join(browserTitles(rows), ","); got != strings.Join(want, ",") {
		t.Fatalf("browser order = %v, want %v", browserTitles(rows), want)
	}
	if got := browserFirstCodex(rows); got != 2 {
		t.Errorf("browserFirstCodex = %d, want 2", got)
	}
	if !strings.Contains(m.viewBrowser(), "── codex") {
		t.Error("viewBrowser missing the codex divider when codex + non-codex are present")
	}
}

// TestBrowserNoCodexUnchanged: with zero codex rows the list stays pure newest-first —
// an empty-session ("(no session)") row is NOT sunk — and no divider renders.
func TestBrowserNoCodexUnchanged(t *testing.T) {
	jobs := []subagent.Result{
		{JobID: "j1", Label: "newest", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:04Z", LeadSessionID: "claude-a"},
		{JobID: "j2", Label: "orphan", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:03Z", LeadSessionID: ""},
		{JobID: "j3", Label: "older", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:02Z", LeadSessionID: "claude-b"},
	}
	m := browserModel(t, nil, jobs, nil)
	want := []string{"newest", "orphan", "older"} // pure time order; the empty row keeps its place
	if got := strings.Join(browserTitles(m.browserRows()), ","); got != strings.Join(want, ",") {
		t.Errorf("no-codex browser order = %v, want %v (empties not sunk)", browserTitles(m.browserRows()), want)
	}
	if strings.Contains(m.viewBrowser(), "── codex") {
		t.Error("viewBrowser rendered a codex divider with 0 codex rows")
	}
}

func TestSessionMarker(t *testing.T) {
	// codex → hollow ◇; everything else → filled ◆ (each rendered through its style).
	if got := sessionMarker("codex:t1"); !strings.Contains(got, "◇") || strings.Contains(got, "◆") {
		t.Errorf("codex marker = %q, want a hollow ◇", got)
	}
	for _, id := range []string{"claude-sess", ""} {
		if got := sessionMarker(id); !strings.Contains(got, "◆") || strings.Contains(got, "◇") {
			t.Errorf("non-codex marker(%q) = %q, want a filled ◆", id, got)
		}
	}
}

func TestSessionLauncherTag(t *testing.T) {
	cases := []struct{ id, want string }{
		{"codex:t1", "codex"},
		{"claude-sess", "claude"},
		{"", ""}, // "(no session)" — its label already says so, no tag
	}
	for _, c := range cases {
		if got := sessionLauncherTag(c.id); got != c.want {
			t.Errorf("sessionLauncherTag(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

func TestSessionTitleWithLauncher(t *testing.T) {
	var m Model
	cases := []struct{ id, want string }{
		{"codex:0199abcd", "codex · 0199abcd"},
		{"abc12345", "claude · abc12345"}, // a titleless claude session
		{"", "(no session)"},              // no tag for "(no session)"
	}
	for _, c := range cases {
		if got := m.sessionTitleWithLauncher(c.id); got != c.want {
			t.Errorf("sessionTitleWithLauncher(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

func TestSessionLabel_CodexWithTitle(t *testing.T) {
	m := Model{sessionMeta: map[string]sessiontitle.Meta{
		"codex:0199abcd": {Title: "research the project"},
	}}
	// A resolved codex title renders "title (id)" like a claude session.
	if got := m.sessionLabel("codex:0199abcd"); got != "research the project (0199abcd)" {
		t.Errorf("codex label with title = %q, want %q", got, "research the project (0199abcd)")
	}
	// No resolved title → the bare thread id.
	if got := m.sessionLabel("codex:0199ef01"); got != "0199ef01" {
		t.Errorf("codex label without a title = %q, want the bare id", got)
	}
}

// TestViewAsSessionsNoSessionDivider: with codex present, a "(no session)" row gets its own
// "── no session ──" divider, symmetric with the codex divider.
func TestViewAsSessionsNoSessionDivider(t *testing.T) {
	jobs := []subagent.Result{
		{JobID: "j1", Label: "claude-job", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:03Z", LeadSessionID: "claude-a"},
		{JobID: "j2", Label: "codex-job", Provider: "minimax", Status: "done", StartedAt: "2026-06-01T00:00:02Z", LeadSessionID: "codex:t1"},
		{JobID: "j3", Label: "orphan-job", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: ""},
	}
	meta := map[string]sessiontitle.Meta{
		"claude-a": {Cwd: "/proj"}, "codex:t1": {Cwd: "/proj"}, "": {Cwd: "/proj"},
	}
	m := boardModel(t, nil, nil)
	m, _ = step(t, m, boardMsg{jobs: jobs, sessionMeta: meta, epoch: m.boardEpoch})
	m.asMode = asModeSessions
	m.focusedProject = "/proj"
	out := m.viewAsSessions()
	if !strings.Contains(out, "── codex") {
		t.Errorf("missing codex divider:\n%s", out)
	}
	if !strings.Contains(out, "── no session") {
		t.Errorf("missing no-session divider:\n%s", out)
	}
}

// TestViewAsProjectsCodexDividers: the L0 project preview shows the same launcher dividers as
// the L1 Sessions pane (consistency), not a flat list.
func TestViewAsProjectsCodexDividers(t *testing.T) {
	jobs := []subagent.Result{
		{JobID: "j1", Label: "claude-job", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:03Z", LeadSessionID: "claude-a"},
		{JobID: "j2", Label: "codex-job", Provider: "minimax", Status: "done", StartedAt: "2026-06-01T00:00:02Z", LeadSessionID: "codex:t1"},
		{JobID: "j3", Label: "orphan-job", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: ""},
	}
	meta := map[string]sessiontitle.Meta{"claude-a": {Cwd: "/proj"}, "codex:t1": {Cwd: "/proj"}, "": {Cwd: "/proj"}}
	m := boardModel(t, nil, nil)
	m, _ = step(t, m, boardMsg{jobs: jobs, sessionMeta: meta, epoch: m.boardEpoch})
	m.asProjectCursor = 0
	out := m.viewAsProjects()
	if !strings.Contains(out, "── codex") || !strings.Contains(out, "── no session") {
		t.Errorf("L0 project preview missing the launcher dividers:\n%s", out)
	}
}

// TestLauncherDividerRows pins the render-cursor math: a cursor above both dividers, below the
// codex divider, and below both must still point at its own data row after the splice.
func TestLauncherDividerRows(t *testing.T) {
	// claude, codex, "(no session)" → both dividers; the data rows render at 0, 2, 4.
	sessions := []asSession{{sessionID: "claude-a"}, {sessionID: "codex:t1"}, {sessionID: ""}}
	leftLines := []string{"L0", "L1", "L2"}
	for _, c := range []struct{ cursor, wantRender int }{
		{0, 0}, // claude — above both dividers
		{1, 2}, // codex — below the codex divider
		{2, 4}, // (no session) — below both dividers
	} {
		rows, rc := launcherDividerRows(leftLines, sessions, c.cursor, 30)
		if len(rows) != 5 {
			t.Fatalf("cursor %d: want 5 render rows (3 + 2 dividers), got %d", c.cursor, len(rows))
		}
		if rc != c.wantRender || rows[rc] != leftLines[c.cursor] {
			t.Errorf("cursor %d: renderCursor=%d (row %q), want %d (data row %q)",
				c.cursor, rc, rows[rc], c.wantRender, leftLines[c.cursor])
		}
	}
	// No codex → rows + cursor unchanged (a codex-free project renders byte-identically).
	if rows, rc := launcherDividerRows([]string{"A", "B"}, []asSession{{sessionID: "x"}, {sessionID: ""}}, 1, 30); len(rows) != 2 || rc != 1 {
		t.Errorf("no codex: want (2 rows, cursor 1), got (%d rows, cursor %d)", len(rows), rc)
	}
}

// TestViewAsSessionsCodexDivider: the L1 Sessions pane shows the divider + the codex ◇ marker
// iff the focused project has both codex and non-codex sessions.
func TestViewAsSessionsCodexDivider(t *testing.T) {
	render := func(t *testing.T, jobs []subagent.Result, meta map[string]sessiontitle.Meta) string {
		t.Helper()
		m := boardModel(t, nil, nil)
		m, _ = step(t, m, boardMsg{jobs: jobs, sessionMeta: meta, epoch: m.boardEpoch})
		m.asMode = asModeSessions
		m.focusedProject = "/proj"
		return m.viewAsSessions()
	}

	withCodex := render(t,
		[]subagent.Result{
			{JobID: "j1", Label: "claude-job", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:02Z", LeadSessionID: "claude-a"},
			{JobID: "j2", Label: "codex-job", Provider: "minimax", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "codex:t1"},
		},
		map[string]sessiontitle.Meta{"claude-a": {Cwd: "/proj"}, "codex:t1": {Cwd: "/proj"}},
	)
	if !strings.Contains(withCodex, "── codex") {
		t.Errorf("L1 pane missing the codex divider with codex + non-codex sessions:\n%s", withCodex)
	}
	if !strings.Contains(withCodex, "◇") {
		t.Errorf("L1 pane missing the codex ◇ marker:\n%s", withCodex)
	}

	noCodex := render(t,
		[]subagent.Result{
			{JobID: "j1", Label: "claude-a-job", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:02Z", LeadSessionID: "claude-a"},
			{JobID: "j3", Label: "claude-b-job", Provider: "glm", Status: "done", StartedAt: "2026-06-01T00:00:01Z", LeadSessionID: "claude-b"},
		},
		map[string]sessiontitle.Meta{"claude-a": {Cwd: "/proj"}, "claude-b": {Cwd: "/proj"}},
	)
	if strings.Contains(noCodex, "── codex") {
		t.Errorf("L1 pane rendered a codex divider with 0 codex sessions:\n%s", noCodex)
	}
	if strings.Contains(noCodex, "◇") {
		t.Errorf("L1 pane rendered a codex ◇ with 0 codex sessions:\n%s", noCodex)
	}
}
