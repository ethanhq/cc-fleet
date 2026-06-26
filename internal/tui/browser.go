package tui

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ethanhq/cc-fleet/internal/sessiontitle"
)

// browserKind tags a flat row's lane so a row can re-find its source after a refresh and
// route Enter to the right detail view. It also tags an L3 entity anchor (job vs teammate).
type browserKind int

const (
	browserJob  browserKind = iota // a standalone subagent job (RunID == "")
	browserRun                     // a workflow run
	browserTeam                    // a team session
	browserMate                    // an L3 entity anchor only: a teammate (never a flat browser row)
)

// browserRef is a row's stable identity, used both as the flat browser cursor anchor and as
// the L3 entity-cursor anchor. id is the kind's key: a job/run id, a teammate's mateKey, or
// leadSessionID+"\x00"+team for a team session — so it survives the wholesale slice swap a
// board refresh does.
type browserRef struct {
	kind browserKind
	id   string
}

// browserRow is one flat, newest-first list entry. The display fields feed the filter (title +
// provider + lane) and the rendered line; the id fields carry exactly what Enter needs to set
// the board's navigation state for that entity's existing detail view.
type browserRow struct {
	ref      browserRef
	title    string
	provider string
	lane     string
	project  string // the session's project dir — filterable; its basename shows on the row
	session  string // the parent session's title (subagent/workflow rows; teams carry it in title)
	when     time.Time
	hasWhen  bool

	// Enter targets.
	jobID         string
	runID         string
	sessionID     string // run's launching session (the board grouping key)
	leadSessionID string // job/team session root
	team          string // team name (browserTeam)
}

// teamRefID is a team session's anchor id: its session root and name together (a team named
// "" — the "(no team)" bucket — still gets a stable, distinct key per session).
func teamRefID(leadSessionID, team string) string { return leadSessionID + "\x00" + team }

// browserRows flattens the board's already-loaded slices into the flat list, newest-first.
// It reuses the board's per-lane row helpers (jobRowLabel / runLabel / sessionLabel) so there
// is no second title path: a job with no --label shows a short id, which the filter still
// matches on provider/lane/project/session. Standalone jobs only (RunID == "") — workflow leaves belong
// to their run row; teams come from the session tree. Ties break by id for a stable order as
// the board polls underneath.
func (m Model) browserRows() []browserRow {
	var rows []browserRow

	for _, j := range m.jobs {
		if j.RunID != "" {
			continue // a workflow leaf belongs to its run row, never listed standalone
		}
		row := browserRow{
			ref:           browserRef{kind: browserJob, id: j.JobID},
			title:         jobRowLabel(j),
			provider:      j.Provider,
			lane:          "subagent",
			project:       m.sessionMeta[j.LeadSessionID].Cwd,
			session:       m.browserSessionTitle(j.LeadSessionID),
			jobID:         j.JobID,
			leadSessionID: j.LeadSessionID,
		}
		if ts, err := time.Parse(time.RFC3339, j.StartedAt); err == nil {
			row.when, row.hasWhen = ts, true
		}
		rows = append(rows, row)
	}

	for _, g := range m.wfGroups() {
		row := browserRow{
			ref:       browserRef{kind: browserRun, id: g.runID},
			title:     m.runLabel(g),
			provider:  runProvider(g),
			lane:      "workflow",
			project:   g.cwd,
			session:   m.browserSessionTitle(g.sessionID),
			runID:     g.runID,
			sessionID: g.sessionID,
		}
		if ts, err := time.Parse(time.RFC3339, g.startedAt); err == nil {
			row.when, row.hasWhen = ts, true
		}
		rows = append(rows, row)
	}

	for _, s := range m.asSessions() {
		for _, t := range s.teams {
			row := browserRow{
				ref:           browserRef{kind: browserTeam, id: teamRefID(s.sessionID, t.name)},
				title:         teamTitle(m, s.sessionID, t.name),
				provider:      teamProvider(t),
				lane:          "team",
				project:       sessionProjectDir(s, m.sessionMeta),
				leadSessionID: s.sessionID,
				team:          t.name,
			}
			if when, ok := teamWhen(t); ok {
				row.when, row.hasWhen = when, true
			}
			rows = append(rows, row)
		}
	}

	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.hasWhen != b.hasWhen {
			return a.hasWhen // timed rows before undated ones
		}
		if a.hasWhen && !a.when.Equal(b.when) {
			return a.when.After(b.when) // newest first
		}
		return a.ref.id < b.ref.id // stable tiebreak
	})
	return rows
}

// runProvider is a run's display provider: the first leaf's provider (a run is usually
// single-provider; a mixed run just shows the first, which still feeds the filter).
func runProvider(g runGroup) string {
	for _, p := range g.phases {
		for _, j := range p.jobs {
			if j.Provider != "" {
				return j.Provider
			}
		}
	}
	return ""
}

// teamProvider is a team's display provider: its first live member's provider, else the first
// member's (an ended team still shows a recorded provider when one survives).
func teamProvider(t asTeam) string {
	for _, mem := range t.members {
		if mem.Status != endedStatus && mem.Provider != "" {
			return mem.Provider
		}
	}
	for _, mem := range t.members {
		if mem.Provider != "" {
			return mem.Provider
		}
	}
	return ""
}

// teamTitle names a team row: "<team> · <session>" so the same team across sessions stays
// distinguishable, falling back to the session label when a team has no name.
func teamTitle(m Model, sessionID, team string) string {
	sess := m.sessionLabel(sessionID)
	if name := sessiontitle.CleanTitle(team); name != "" {
		return trunc(name, 24) + " · " + sess
	}
	return sess
}

// teamWhen is a team session's sort time: the earliest member SpawnTime (its "created").
func teamWhen(t asTeam) (time.Time, bool) {
	var earliest int64
	for _, mem := range t.members {
		if mem.SpawnTime > 0 && (earliest == 0 || mem.SpawnTime < earliest) {
			earliest = mem.SpawnTime
		}
	}
	if earliest == 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(earliest), true
}

// browserSessionTitle is the parent Claude session's title (its /rename or AI title) for a row,
// empty when the conversation was never titled — so a row only ever shows a real session name, not
// a raw id. The id stays filterable via the row's leadSessionID/sessionID.
func (m Model) browserSessionTitle(id string) string {
	return trunc(sessiontitle.CleanTitle(m.sessionMeta[id].Title), 40)
}

// filteredBrowserRows narrows browserRows by a case-insensitive substring over the row's title,
// provider, lane, project dir, parent-session title, and parent-session id. An empty filter returns
// the full list.
func (m Model) filteredBrowserRows() []browserRow {
	all := m.browserRows()
	if m.browserFilter == "" {
		return all
	}
	q := strings.ToLower(m.browserFilter)
	var out []browserRow
	for _, r := range all {
		hay := strings.ToLower(strings.Join([]string{
			r.title, r.provider, r.lane, r.project, r.session, r.leadSessionID, r.sessionID,
		}, " "))
		if strings.Contains(hay, q) {
			out = append(out, r)
		}
	}
	return out
}

// openBrowser switches the board into the flat session browser. It is a DEDICATED entry path
// (NOT the boardEntryRoute reset, which forces asModeBoxes on the first refresh): asModeBrowser
// is preserved by rerootSpawn's own branch, so the browser survives the board's polling.
func (m Model) openBrowser() (tea.Model, tea.Cmd) {
	m.browserReturnMode = m.asMode // restored on esc, so the browser closes back to the board
	m.asMode = asModeBrowser
	m.browserFilter = ""
	m.browserCursor = 0
	m.browserAnchor = browserRef{}
	if rows := m.filteredBrowserRows(); len(rows) > 0 {
		m.browserAnchor = rows[0].ref
	}
	return m, nil
}

// updateBrowser drives the flat browser: keystrokes route to the filter FIRST (printable runes,
// backspace, ↑/↓, enter, esc) — only ctrl+c stays global (consumed before this in Update). A
// filter edit resets the cursor to the top; Enter opens the cursored row's existing detail.
func (m Model) updateBrowser(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rows := m.filteredBrowserRows()
	switch msg.String() {
	case "esc":
		m.asMode = m.browserReturnMode // close the browser back to the board level it opened from
		return m, nil
	case "up":
		if m.browserCursor > 0 {
			m.browserCursor--
		}
		m.syncBrowserAnchor(rows)
	case "down":
		if m.browserCursor < len(rows)-1 {
			m.browserCursor++
		}
		m.syncBrowserAnchor(rows)
	case "enter":
		if m.browserCursor >= 0 && m.browserCursor < len(rows) {
			return m.enterBrowserRow(rows[m.browserCursor])
		}
	case "backspace", "ctrl+h": // some terminals report Backspace as Ctrl-H
		if m.browserFilter != "" {
			r := []rune(m.browserFilter)
			m.browserFilter = string(r[:len(r)-1])
			m.browserCursor = 0
			m.syncBrowserAnchor(m.filteredBrowserRows())
		}
	default:
		if msg.Type == tea.KeyRunes {
			m.browserFilter += string(msg.Runes)
			m.browserCursor = 0
			m.syncBrowserAnchor(m.filteredBrowserRows())
		}
	}
	return m, nil
}

// syncBrowserAnchor records the cursored row's identity, so a refresh that reshapes the list
// can re-find it (reanchorBrowser) instead of letting the index drift onto another row.
func (m *Model) syncBrowserAnchor(rows []browserRow) {
	if m.browserCursor >= 0 && m.browserCursor < len(rows) {
		m.browserAnchor = rows[m.browserCursor].ref
	}
}

// reanchorBrowser re-finds the anchored row's index after a refresh rebuilds the rows. When the
// anchored row is gone it clamps the cursor and re-anchors to whatever row the cursor now lands
// on, so identity tracking resumes (else every later refresh re-fails the stale anchor and the
// cursor silently drifts back to positional).
func (m *Model) reanchorBrowser() {
	rows := m.filteredBrowserRows()
	for i, r := range rows {
		if r.ref == m.browserAnchor {
			m.browserCursor = i
			return
		}
	}
	m.browserCursor = clampIndex(m.browserCursor, len(rows))
	if m.browserCursor < len(rows) {
		m.browserAnchor = rows[m.browserCursor].ref
	} else {
		m.browserAnchor = browserRef{}
	}
}

// enterBrowserRow opens the row's entity in its EXISTING board detail view by setting the same
// navigation state a hand-drill would (mirror enterSession / asDescend), and marks browserOrigin
// so esc returns here while ← does normal board ascent. It roots the session and its project (so
// the run drill survives a refresh and ← climbs into the right project) and seeds asBoxCursor to
// the entity's L2 row, so a later ← — and any row action there (d/p/s) — targets the right
// run/team/job instead of a stale one.
func (m Model) enterBrowserRow(row browserRow) (tea.Model, tea.Cmd) {
	m.browserOrigin = true
	switch row.ref.kind {
	case browserRun:
		m.focusedSessionID = row.sessionID
		m.rootBrowserProject()
		m.asBoxCursor = m.browserBoxCursor(row)
		m.focusedRunID = row.runID
		m.wfPhaseCursor, m.wfAgentCursor = 0, 0
		m.asMode = asModeRunPhases
		return m, nil
	case browserTeam:
		m.focusedSessionID = row.leadSessionID
		m.rootBrowserProject()
		m.asBoxCursor = m.browserBoxCursor(row)
		m.asEntitySrc = asRailRef{team: row.team}
		m.asEntityCursor, m.asCardScroll = 0, 0
		m.asMode = asModeEntity
		return m.focusEntityIO()
	default: // browserJob
		m.focusedSessionID = row.leadSessionID
		m.rootBrowserProject()
		m.asBoxCursor = m.browserBoxCursor(row)
		m.asEntitySrc = asRailRef{jobs: true}
		m.asEntityCursor = m.jobIndex(row.jobID)
		m.asCardScroll = 0
		m.asMode = asModeEntity
		return m.focusEntityIO()
	}
}

// browserBoxCursor is the L2 continuum index (runs, then teams, then jobs — mirror boxRowRef) of
// the entered row's entity in the now-focused session, so ← out of a browser-entered detail lands
// the boxes cursor on the right row. Requires focusedSessionID already set; a vanished entity → 0.
func (m Model) browserBoxCursor(row browserRow) int {
	s, ok := m.focusedSession()
	if !ok {
		return 0
	}
	switch row.ref.kind {
	case browserRun:
		for i, g := range s.runs {
			if g.runID == row.runID {
				return i
			}
		}
	case browserTeam:
		for i, t := range s.teams {
			if t.name == row.team {
				return len(s.runs) + i
			}
		}
	default: // browserJob
		for i, j := range s.jobs {
			if j.JobID == row.jobID {
				return len(s.runs) + len(s.teams) + i
			}
		}
	}
	return 0
}

// rootBrowserProject sets focusedProject from the now-focused session (mirror rerootSpawn) so a
// browser-entered detail's ← ascent climbs into the right project immediately, before the next
// refresh would re-derive it. Requires focusedSessionID already set.
func (m *Model) rootBrowserProject() {
	if s, ok := m.focusedSession(); ok {
		m.focusedProject = sessionProjectDir(s, m.sessionMeta)
	}
}

// jobIndex is the focused job's index in the now-current session jobs slice (the L3 cursor
// indexes that per-tick-rebuilt slice). A vanished job clamps to 0.
func (m Model) jobIndex(jobID string) int {
	_, jobs := m.railEntities()
	for i, j := range jobs {
		if j.JobID == jobID {
			return i
		}
	}
	return 0
}

// viewBrowser renders the flat session browser in the resume style: a "Session browser · N records"
// header, a rounded search box, then two-line entries (label · session title, then a dim relative-
// time metadata line) spaced by a blank line and windowed to the cursor. The board's warning/footer
// tail is appended by viewSpawn — the same tail every board mode shares.
func (m Model) viewBrowser() string {
	all := m.browserRows()
	rows := m.filteredBrowserRows()

	count := fmt.Sprintf("%d records", len(all))
	if m.browserFilter != "" {
		count = fmt.Sprintf("%d / %d", len(rows), len(all))
	}
	header := titleStyle.Render("cc-fleet · Session browser") + faintStyle.Render(" · "+count)

	glyph := contentStyle.Render("⌕")
	field := glyph + " " + faintStyle.Render("Search…")
	if m.browserFilter != "" {
		field = glyph + " " + contentStyle.Render(m.browserFilter)
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.AdaptiveColor{Light: "237", Dark: "252"}).
		Padding(0, 1).
		Width(m.boardInner() - 4).
		Render(field)

	var body []string
	switch {
	case len(all) == 0:
		body = append(body, faintStyle.Render("(no past sessions yet)"))
	case len(rows) == 0:
		body = append(body, faintStyle.Render("no match — backspace to widen the filter"))
	default:
		// Window to the real terminal height — NOT boardBodyHeight, which budgets the master-detail
		// chrome the browser doesn't draw. The browser's own chrome is the header + blank + 3-row
		// search box + blank above and a blank + footer below (~9 rows); each entry is 2 lines + a blank.
		// The 24-row fallback is ONLY for the pre-WindowSizeMsg state; a real (even tiny) height wins.
		h := m.height
		if h <= 0 {
			h = 24
		}
		visible := (h - 9) / 3
		if visible < 1 {
			visible = 1
		}
		start, end := windowBounds(m.browserCursor, len(rows), visible)
		for i := start; i < end; i++ {
			body = append(body, m.browserEntryLines(rows[i], i, start, end, len(rows))...)
			if i < end-1 {
				body = append(body, "")
			}
		}
	}

	// Trailing blank line so the shared footer viewSpawn appends below sits off the last entry.
	content := header + "\n\n" + box + "\n\n" + strings.Join(body, "\n") + "\n"
	return indentBox(content, boardMargin)
}

// browserEntryLines renders one entry as two lines: the entity label · parent-session title (with a
// cursor / scroll-edge marker), then a dim "<relative time> · provider · lane · project" line.
func (m Model) browserEntryLines(r browserRow, i, start, end, total int) []string {
	marker, style := "  ", contentStyle
	switch {
	case i == m.browserCursor:
		marker, style = cursorStyle.Render("❯ "), selectedStyle
	case i == start && start > 0:
		marker = faintStyle.Render("↑ ") // more above the window
	case i == end-1 && end < total:
		marker = faintStyle.Render("↓ ") // more below the window
	}
	name := r.title
	if r.session != "" {
		name += " · " + r.session
	}
	titleLine := marker + style.Render(trunc(name, m.boardInner()-4))

	parts := make([]string, 0, 4)
	if r.hasWhen {
		parts = append(parts, relAgo(r.when))
	}
	// Scrub ANSI/BEL/OSC from every displayed metadata field (provider from on-disk records, the raw
	// cwd basename) before render — the board's CleanTitle-at-render invariant. lane is a literal.
	if p := sessiontitle.CleanTitle(r.provider); p != "" {
		parts = append(parts, p)
	}
	parts = append(parts, r.lane)
	if p := sessiontitle.CleanTitle(filepath.Base(r.project)); p != "" {
		parts = append(parts, p)
	}
	metaLine := "  " + faintStyle.Render(trunc(strings.Join(parts, " · "), m.boardInner()-4))
	return []string{titleLine, metaLine}
}

// relAgo renders a coarse "N units ago" for a row's time — second / minute / hour / day, singular-
// aware, "just now" under a second.
func relAgo(t time.Time) string {
	d := time.Since(t)
	if d < time.Second {
		return "just now"
	}
	var n int
	var unit string
	switch {
	case d < time.Minute:
		n, unit = int(d.Seconds()), "second"
	case d < time.Hour:
		n, unit = int(d.Minutes()), "minute"
	case d < 24*time.Hour:
		n, unit = int(d.Hours()), "hour"
	default:
		n, unit = int(d.Hours())/24, "day"
	}
	if n == 1 {
		return "1 " + unit + " ago"
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}
