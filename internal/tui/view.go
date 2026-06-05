package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/ethanhq/cc-fleet/internal/secrets"
	"github.com/ethanhq/cc-fleet/internal/sessiontitle"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// Shared lipgloss styles. Colors are ANSI 256 indices so they degrade
// gracefully on limited terminals.
var (
	titleStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	cursorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
	selectedStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	faintStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	sessionHdrStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	teamHdrStyle    = lipgloss.NewStyle().Bold(true) // team section header (flush-left bold title)
	errStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	okStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
)

// footer renders a dim key-hint line.
func footer(s string) string { return faintStyle.Render(s) }

// View satisfies tea.Model.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	switch m.screen {
	case screenList:
		return m.viewList()
	case screenSpawn:
		return m.viewSpawn()
	case screenWorkflows:
		return m.viewWorkflows()
	case screenPickTemplate:
		return m.viewPickTemplate()
	case screenForm:
		return m.form.View() + "\n" + footer("esc cancel")
	case screenModelPick:
		return m.viewModelPick()
	case screenRemoveConfirm:
		return m.viewRemoveConfirm()
	case screenResult:
		return m.viewResult()
	case screenKeys:
		return m.viewKeys()
	case screenTeammateDetail:
		return m.viewTeammateDetail()
	case screenWorkflowDetail:
		return m.viewWorkflowDetail()
	case screenSetup:
		return m.viewSetup()
	case screenSetupTmux:
		return m.viewSetupTmux()
	}
	return ""
}

// viewKeys renders the per-vendor key manager. It renders ONLY secrets.MaskKey
// for each key — the full key never reaches the screen — and the add/edit input
// is an EchoPassword field (bullets), so no plaintext is ever displayed.
func (m Model) viewKeys() string {
	var b strings.Builder
	rot := m.keyRotation
	if rot == "" {
		rot = "off"
	}
	b.WriteString(titleStyle.Render("API keys · "+m.keyVendor) +
		faintStyle.Render("    rotation: "+rot) + "\n\n")

	if m.keyEditing {
		title := "Add key"
		if m.keyEditIdx >= 0 {
			title = "Edit " + m.keyLabel(m.keyEditIdx)
		}
		b.WriteString(title + "\n")
		b.WriteString(m.keyInput.View() + "\n")
		if m.keyErr != "" {
			b.WriteString("\n" + errStyle.Render(m.keyErr) + "\n")
		}
		b.WriteString("\n" + footer("enter save · esc cancel"))
		return b.String()
	}

	for i, e := range m.keys {
		cursor := "  "
		label := fmt.Sprintf("%-10s", m.keyLabel(i))
		if i == m.keyCursor {
			cursor = cursorStyle.Render("> ")
			label = selectedStyle.Render(label)
		}
		status := okStyle.Render("● enabled")
		if !e.Enabled {
			status = faintStyle.Render("○ disabled")
		}
		b.WriteString(cursor + label + " " +
			faintStyle.Render(fmt.Sprintf("%-10s", secrets.MaskKey(e.Key))) + " " + status + "\n")
	}
	if len(m.keys) == 0 {
		b.WriteString(faintStyle.Render("  (no keys yet — add one below)") + "\n")
	}

	addCursor := "  "
	addLabel := "+ Add key…"
	if m.keyCursor == len(m.keys) {
		addCursor = cursorStyle.Render("> ")
		addLabel = selectedStyle.Render(addLabel)
	}
	b.WriteString(addCursor + addLabel + "\n")
	if m.keyErr != "" {
		b.WriteString("\n" + errStyle.Render(m.keyErr) + "\n")
	}
	b.WriteString("\n" + footer("↑/↓ move · space toggle · e edit · d delete · a/enter add · t cycle rotation · esc back"))
	return b.String()
}

func (m Model) viewList() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("cc-fleet · Vendors") +
		faintStyle.Render("    tab → Agent status") + "\n\n")
	switch {
	case m.loading:
		b.WriteString("loading…\n")
	case m.vendorsErr != nil:
		b.WriteString(errStyle.Render("error: "+m.vendorsErr.Error()) + "\n")
	default:
		for i, v := range m.vendors {
			cursor := "  "
			// Pad the plain name to a fixed width BEFORE styling so the ANSI
			// codes of the selected row don't throw off column alignment.
			name := fmt.Sprintf("%-12s", v.Name)
			if i == m.vendorCursor {
				cursor = cursorStyle.Render("> ")
				name = selectedStyle.Render(name)
			}
			status := okStyle.Render("● enabled")
			if !v.Enabled {
				status = faintStyle.Render("○ disabled")
			}
			models := fmt.Sprintf("%d models", v.ModelsCount)
			if v.ModelsStale {
				models += " (stale)"
			}
			b.WriteString(cursor + name + " " +
				faintStyle.Render(fmt.Sprintf("%-20s ", v.DefaultModel)) +
				status + faintStyle.Render("  "+models) + "\n")
		}
		if len(m.vendors) == 0 {
			b.WriteString(faintStyle.Render("  (no vendors configured yet)") + "\n")
		}
		// Trailing synthetic "+ Add vendor…" row at index len(vendors).
		b.WriteString(faintStyle.Render("  ────────────────") + "\n")
		addCursor := "  "
		addLabel := "+ Add vendor…"
		if m.vendorCursor == len(m.vendors) {
			addCursor = cursorStyle.Render("> ")
			addLabel = selectedStyle.Render(addLabel)
		}
		b.WriteString(addCursor + addLabel + "\n")
	}
	b.WriteString("\n" + footer("↑/↓ move · enter edit · d delete · tab agent status · q quit"))
	return b.String()
}

func (m Model) viewSpawn() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("cc-fleet · Agent status") + faintStyle.Render("    tab → Workflows") + "\n\n")
	switch {
	case m.loading:
		b.WriteString("discovering…\n")
	case m.spawnErr != nil:
		b.WriteString(errStyle.Render("error: "+m.spawnErr.Error()) + "\n")
	default:
		b.WriteString(m.viewTeammateTable())
		b.WriteString("\n" + m.viewJobTable())
	}
	// Inline hide/show outcome: a failed h/s shows its reason here rather than
	// silently relying on the next refresh.
	if m.boardStatus != "" {
		style := okStyle
		if m.boardStatusErr {
			style = errStyle
		}
		b.WriteString("\n" + style.Render(m.boardStatus))
	}
	b.WriteString("\n" + footer("↑/↓ move · enter detail · h hide · s show · r refresh · tab workflows · esc vendors · q quit"))
	return b.String()
}

// wfMetricCols is the spacing of the leaf metric columns (AGENT VENDOR MODEL STATUS
// TOKENS(i/o/c) COST TRN), shared by the legend and each leaf row so they line up.
const wfMetricCols = "%-14s %-9s %-14s %-7s %-13s %-8s %-3s"

// viewWorkflows renders the Workflows board: RunID-tagged subagent jobs as a
// run→(group|phase)→agent tree with per-leaf token/cost/turn metrics, a run totals
// line, and a flowing live-event log below the tree. Like viewJobTable it shows
// ONLY status + metric columns — NEVER the job's answer text (Result.Result) — so
// no vendor reply can leak; the answer lives only in the drill-in card. The run
// name, phase title, agent label, and every log line are opaque operator metadata
// that may carry control bytes, so each is sanitized through sessiontitle.CleanTitle
// before display. A leaf cursor (flat index over wfLeaves) selects a row for
// enter/x/r; header/group lines never take a cursor slot.
func (m Model) viewWorkflows() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("cc-fleet · Workflows") + faintStyle.Render("    tab → Vendors") + "\n\n")
	switch {
	case m.loading:
		b.WriteString("loading…\n")
	case m.workflowsErr != nil:
		b.WriteString(errStyle.Render("error: "+m.workflowsErr.Error()) + "\n")
	default:
		groups := groupByRun(m.workflowJobs, m.workflowRuns)
		if len(groups) == 0 {
			b.WriteString(faintStyle.Render("  (no workflow runs)") + "\n")
		} else {
			b.WriteString(faintStyle.Render("      "+fmt.Sprintf(wfMetricCols,
				"AGENT", "VENDOR", "MODEL", "STATUS", "TOKENS(i/o/c)", "COST", "TRN")) + "\n")
			// leafIdx is the running flat index over every run's leaf rows; it must
			// advance in the SAME order as Model.wfLeaves so `leafIdx == cursor`
			// highlights the right job. No row windowing — render every row (same as
			// viewTeammateTable / viewJobTable); the operator scrolls the terminal.
			leafIdx := 0
			for _, g := range groups {
				b.WriteString(sessionHdrStyle.Render("◆ run: "+m.runLabel(g)) + "\n")
				m.renderRunTree(&b, g, &leafIdx)
				b.WriteString("    " + faintStyle.Render(runTotalsLine(g)) + "\n")
			}
		}
		b.WriteString(m.viewWorkflowLog())
	}
	if m.workflowStatus != "" {
		style := okStyle
		if m.workflowStatusErr {
			style = errStyle
		}
		b.WriteString("\n" + style.Render(m.workflowStatus))
	}
	b.WriteString("\n" + footer("↑/↓ move · enter detail · x stop · r restart · R refresh · tab/esc vendors · q quit"))
	return b.String()
}

// renderRunTree writes one run's leaf rows, advancing *leafIdx per leaf so the
// cursor stays a flat index over wfLeaves. It uses the DAG structure (group
// headers from the run's events) when the run has group events, else falls back to
// the flat phase→agent tree. Both paths render the SAME leaf jobs in groupByRun
// order — the structure only changes the indentation/headers — so the flat-index
// cursor lands on the right job either way.
func (m Model) renderRunTree(b *strings.Builder, g runGroup, leafIdx *int) {
	// Flatten this run's jobs in phase→job order (the canonical leaf order).
	var jobs []subagent.Result
	for _, p := range g.phases {
		jobs = append(jobs, p.jobs...)
	}
	dag := buildDAG(m.wfEvents[g.runID])
	if dag != nil && len(jobs) > 0 {
		// DAG overlay: render the group skeleton, pairing leaf nodes positionally
		// with the run's jobs (the engine emits a leaf's launch event in start
		// order, so positional pairing matches groupByRun order). A DAG leaf node
		// past the job count renders structure-only (no metrics, not selectable).
		jobPos := 0
		m.renderDAGNodes(b, dag, 1, jobs, &jobPos, leafIdx)
		// Any jobs the DAG didn't cover (more jobs than leaf nodes) render flat under
		// the run so every job stays selectable.
		for ; jobPos < len(jobs); jobPos++ {
			m.renderLeafRow(b, jobs[jobPos], 2, leafIdx)
		}
		return
	}
	for _, p := range g.phases {
		b.WriteString("  " + teamHdrStyle.Render("▸ phase: "+sessiontitle.CleanTitle(p.title)) + "\n")
		for _, j := range p.jobs {
			m.renderLeafRow(b, j, 2, leafIdx)
		}
	}
}

// renderDAGNodes walks the reconstructed structure tree, writing a header for each
// group node (⇉ parallel / → pipeline / ▽ workflow) indented by depth and pairing
// each leaf node positionally with the next run job (so the selectable row carries
// the job's metrics + cursor). depth is the indent level (1 == directly under the
// run header).
func (m Model) renderDAGNodes(b *strings.Builder, nodes []*dagNode, depth int, jobs []subagent.Result, jobPos, leafIdx *int) {
	indent := strings.Repeat("  ", depth)
	for _, n := range nodes {
		if n.group {
			b.WriteString(indent + teamHdrStyle.Render(dagGroupLabel(n)) + "\n")
			m.renderDAGNodes(b, n.children, depth+1, jobs, jobPos, leafIdx)
			continue
		}
		// A leaf DAG node: pair with the next job if one remains, else render the
		// event's own (label/vendor/model/status) as a non-selectable structure row.
		if *jobPos < len(jobs) {
			m.renderLeafRow(b, jobs[*jobPos], depth+1, leafIdx)
			*jobPos++
		} else {
			b.WriteString(indent + "  " + faintStyle.Render(fmt.Sprintf("%-14s %-9s %-14s %-7s",
				trunc(sessiontitle.CleanTitle(n.label), 14), trunc(sessiontitle.CleanTitle(n.vendor), 9),
				trunc(sessiontitle.CleanTitle(n.model), 14), trunc(sessiontitle.CleanTitle(n.status), 7))) + "\n")
		}
	}
}

// dagGroupLabel renders a group node's header: a kind-specific glyph + child count.
func dagGroupLabel(n *dagNode) string {
	count := len(n.children)
	switch n.groupTy {
	case "parallel":
		return fmt.Sprintf("⇉ parallel (%d)", count)
	case "pipeline":
		return fmt.Sprintf("→ pipeline (%d)", count)
	case "workflow":
		return fmt.Sprintf("▽ workflow (%d)", count)
	default:
		return fmt.Sprintf("▸ %s (%d)", sessiontitle.CleanTitle(n.groupTy), count)
	}
}

// renderLeafRow writes one selectable leaf row at the given indent depth, advancing
// *leafIdx. The cursor marker (and bold label) light up when *leafIdx == the
// board cursor. Columns: label / vendor / model / status / tokens(i/o/c) / cost /
// turns — opaque strings CleanTitle-scrubbed, never the answer text.
func (m Model) renderLeafRow(b *strings.Builder, j subagent.Result, depth int, leafIdx *int) {
	indent := strings.Repeat("  ", depth)
	marker := "  "
	label := fmt.Sprintf("%-14s", trunc(sessiontitle.CleanTitle(j.Label), 14))
	if *leafIdx == m.workflowsCursor {
		marker = cursorStyle.Render("> ")
		label = selectedStyle.Render(label)
	}
	mt := metricsOf(j)
	tok := fmt.Sprintf("%s/%s/%s", humanTokens(mt.inTok), humanTokens(mt.outTok), humanTokens(mt.cacheTok))
	cost := fmt.Sprintf("$%.4f", mt.cost)
	b.WriteString(indent + marker + label + " " + fmt.Sprintf("%-9s %-14s %-7s %-13s %-8s %-3d",
		trunc(sessiontitle.CleanTitle(j.Vendor), 9), trunc(sessiontitle.CleanTitle(j.Model), 14),
		trunc(sessiontitle.CleanTitle(j.Status), 7), tok, cost, mt.turns) + "\n")
	*leafIdx++
}

// runTotalsLine sums a run's leaf tokens + cost and renders the elapsed wall-clock
// (StartedAt → the manifest's UpdatedAt, else now). Tool-call counts are not
// available from the data sources, so they are deliberately absent.
func runTotalsLine(g runGroup) string {
	var inTok, outTok, cacheTok int
	var cost float64
	for _, p := range g.phases {
		for _, j := range p.jobs {
			mt := metricsOf(j)
			inTok += mt.inTok
			outTok += mt.outTok
			cacheTok += mt.cacheTok
			cost += mt.cost
		}
	}
	return fmt.Sprintf("Σ tokens %s in / %s out / %s cache · $%.4f · %s elapsed",
		humanTokens(inTok), humanTokens(outTok), humanTokens(cacheTok), cost, g.elapsed())
}

// viewWorkflowLog renders the flowing live-event log pane below the run tree: the
// bounded most-recent rendered lines, each already CleanTitle-scrubbed (in
// workflow.RenderEventLine) and here trunc'd to a sane width.
func (m Model) viewWorkflowLog() string {
	var b strings.Builder
	b.WriteString("\n" + faintStyle.Render("Live log") + "\n")
	if len(m.wfLog) == 0 {
		b.WriteString(faintStyle.Render("  (no events yet)") + "\n")
		return b.String()
	}
	for _, line := range m.wfLog {
		b.WriteString(faintStyle.Render(trunc(line, 100)) + "\n")
	}
	return b.String()
}

// humanTokens compacts a token count: <1000 verbatim, else N.Nk (e.g. 50.7k), else
// N.NM for millions.
func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// viewWorkflowDetail renders the read-only leaf drill-in card: the selected leaf's
// prompt + answer read from its io files, CleanTitle-scrubbed and collapsed by
// default (e/space expands). The answer text appears ONLY here — never the board
// table or the live log. When the io files are absent (run executed with
// --no-persist-io, or GC'd) it shows the not-persisted note.
func (m Model) viewWorkflowDetail() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("cc-fleet · leaf detail") + footer("    esc back") + "\n\n")
	j := m.wfDetailJob
	b.WriteString(selectedStyle.Render("  "+trunc(sessiontitle.CleanTitle(j.Label), 60)) +
		faintStyle.Render("  "+trunc(sessiontitle.CleanTitle(j.Vendor), 16)+"/"+trunc(sessiontitle.CleanTitle(j.Model), 24)) + "\n")
	b.WriteString(faintStyle.Render("  status "+trunc(sessiontitle.CleanTitle(j.Status), 16)+
		" · phase "+trunc(sessiontitle.CleanTitle(j.Phase), 24)) + "\n\n")
	if !m.wfDetailIO {
		b.WriteString(faintStyle.Render("  (not persisted — run with default persist-io)") + "\n")
		b.WriteString("\n" + footer("esc/enter back · q quit"))
		return b.String()
	}
	limit := wfCollapsedChars
	if m.wfDetailExpand {
		limit = 0 // 0 == no limit
	}
	b.WriteString(faintStyle.Render("  PROMPT") + "\n")
	b.WriteString(renderIOBlock(m.wfDetailPrompt, limit))
	b.WriteString("\n" + faintStyle.Render("  ANSWER") + "\n")
	b.WriteString(renderIOBlock(m.wfDetailAnswer, limit))
	hint := "e/space expand"
	if m.wfDetailExpand {
		hint = "e/space collapse"
	}
	b.WriteString("\n" + footer(hint+" · esc/enter back · q quit"))
	return b.String()
}

// wfCollapsedChars is the per-block character cap in the collapsed drill-in view.
const wfCollapsedChars = 600

// renderIOBlock renders one io block (prompt or answer), CleanTitle-scrubbed and
// indented. limit > 0 truncates to that many runes with a "…(N more)" marker; 0
// renders it whole (the expanded view). An empty block shows a dim placeholder.
func renderIOBlock(s string, limit int) string {
	clean := sessiontitle.CleanTitle(s)
	if strings.TrimSpace(clean) == "" {
		return faintStyle.Render("  (empty)") + "\n"
	}
	runes := []rune(clean)
	suffix := ""
	if limit > 0 && len(runes) > limit {
		suffix = fmt.Sprintf("\n  %s", faintStyle.Render(fmt.Sprintf("…(%d more chars — e/space to expand)", len(runes)-limit)))
		clean = string(runes[:limit])
	}
	var b strings.Builder
	for _, line := range strings.Split(clean, "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String() + suffix
}

// runLabel renders a run header label: its sanitized name plus the short run id,
// or just the short id when the run has no name (manifest GC'd or never created).
func (m Model) runLabel(g runGroup) string {
	// The run id is opaque operator metadata: ids.ValidateJobID lets a
	// non-whitespace control rune (e.g. an ANSI escape) through — it only blocks
	// path-unsafe chars — so the id gets the same render-time CleanTitle scrub as
	// the name/phase/label before it reaches the terminal.
	short := shortRunID(sessiontitle.CleanTitle(g.runID))
	if name := sessiontitle.CleanTitle(g.name); name != "" {
		return trunc(name, 48) + " (" + short + ")"
	}
	return short
}

// shortRunID trims a run id to its first 8 chars for the run header.
func shortRunID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// viewTeammateTable renders the upper board table grouped by Claude session and
// then team: session header, team header, indented members. The cursor stays a
// FLAT teammate index — header lines are purely visual and never take a cursor
// slot, so `i == m.spawnCursor` highlights the right member regardless of how
// many headers precede it (teammates are pre-grouped in groupByTeam). The plain
// name is padded BEFORE styling so the selected row's ANSI codes don't break
// column alignment (same discipline as viewList).
func (m Model) viewTeammateTable() string {
	var b strings.Builder
	// Column legend, indented (marker 2 + member indent 8 = 10) to align with the
	// session/team-grouped member rows below.
	b.WriteString(faintStyle.Render("          "+fmt.Sprintf("%-14s %-9s %-16s %-7s %-7s %-8s %-6s",
		"NAME", "VENDOR", "MODEL", "PANE", "PID", "STATUS", "HIDDEN")) + "\n")
	if len(m.teammates) == 0 {
		b.WriteString(faintStyle.Render("  no live teammates (none spawned, or tmux not running)") + "\n")
		return b.String()
	}
	lastLeadSession := ""
	lastTeam := ""
	first := true
	for i, t := range m.teammates {
		if first || t.LeadSessionID != lastLeadSession {
			b.WriteString(sessionHdrStyle.Render("◆ session: "+m.sessionLabel(t.LeadSessionID)) + "\n")
			lastLeadSession = t.LeadSessionID
			lastTeam = ""
		}
		team := t.Team
		if team == "" {
			team = "(no team)"
		}
		if first || team != lastTeam {
			b.WriteString("  " + teamHdrStyle.Render("▸ team: "+team) + "\n")
			lastTeam = team
			first = false
		}
		marker := "  "
		nameCol := fmt.Sprintf("%-14s", trunc(t.Name, 14))
		if i == m.spawnCursor {
			marker = cursorStyle.Render("> ")
			if !t.Hidden { // a hidden row stays faint even when selected (see below)
				nameCol = selectedStyle.Render(nameCol)
			}
		}
		status := t.Status
		if status == "" {
			status = "-"
		}
		hidden := ""
		if t.Hidden {
			hidden = "yes"
		}
		// marker(2) + member indent(8) = 10: deeper than both session and team
		// headers. A hidden teammate renders its whole row faint so it visibly
		// recedes; the cursor marker stays bright so a selected hidden row is
		// still obvious.
		cols := nameCol + " " + fmt.Sprintf("%-9s %-16s %-7s %-7d %-8s %-6s",
			trunc(t.Vendor, 9), trunc(t.Model, 16),
			trunc(t.PaneID, 7), t.PID, trunc(status, 8), hidden)
		if t.Hidden {
			cols = faintStyle.Render(cols)
		}
		b.WriteString(marker + "        " + cols + "\n")
	}
	return b.String()
}

// viewJobTable renders the lower board table: subagent jobs grouped by Claude
// session. It shows only status columns (JOB/VENDOR/MODEL/STATUS/STARTED) —
// NEVER the job's answer text (Result.Result) or captured output, so no vendor
// reply can leak onto the board.
func (m Model) viewJobTable() string {
	var b strings.Builder
	b.WriteString(faintStyle.Render("Subagent Jobs") + "\n")
	b.WriteString(faintStyle.Render("  "+fmt.Sprintf("%-10s %-9s %-16s %-8s %-20s",
		"JOB", "VENDOR", "MODEL", "STATUS", "STARTED")) + "\n")
	// RunID-tagged jobs belong to the Workflows board; show only ungrouped
	// (RunID == "") jobs here so a workflow job never double-renders.
	var jobs []subagent.Result
	for _, j := range m.jobs {
		if j.RunID == "" {
			jobs = append(jobs, j)
		}
	}
	if len(jobs) == 0 {
		b.WriteString(faintStyle.Render("  (no subagent jobs)") + "\n")
		return b.String()
	}
	for _, bucket := range groupedJobsBySession(jobs) {
		b.WriteString(sessionHdrStyle.Render("◆ session: "+m.sessionLabel(bucket.leadSessionID)) + "\n")
		for _, j := range bucket.jobs {
			b.WriteString("  " + fmt.Sprintf("%-10s %-9s %-16s %-8s %-20s",
				shortJobID(j.JobID), trunc(j.Vendor, 9), trunc(j.Model, 16),
				trunc(j.Status, 8), trunc(j.StartedAt, 20)) + "\n")
		}
	}
	return b.String()
}

type jobBucket struct {
	leadSessionID string
	jobs          []subagent.Result
	firstIdx      int
	startedAt     time.Time
	hasStartedAt  bool
}

func groupedJobsBySession(jobs []subagent.Result) []jobBucket {
	bySession := map[string]int{}
	var buckets []jobBucket
	for _, j := range jobs {
		idx, ok := bySession[j.LeadSessionID]
		if !ok {
			idx = len(buckets)
			bySession[j.LeadSessionID] = idx
			buckets = append(buckets, jobBucket{leadSessionID: j.LeadSessionID, firstIdx: idx})
		}
		b := &buckets[idx]
		b.jobs = append(b.jobs, j)
		if started, err := time.Parse(time.RFC3339, j.StartedAt); err == nil {
			if !b.hasStartedAt || started.Before(b.startedAt) {
				b.startedAt = started
				b.hasStartedAt = true
			}
		}
	}
	sort.SliceStable(buckets, func(i, j int) bool {
		a, b := buckets[i], buckets[j]
		if a.leadSessionID != b.leadSessionID {
			if a.leadSessionID == "" {
				return false
			}
			if b.leadSessionID == "" {
				return true
			}
		}
		if a.hasStartedAt != b.hasStartedAt {
			return a.hasStartedAt
		}
		if a.hasStartedAt && !a.startedAt.Equal(b.startedAt) {
			return a.startedAt.Before(b.startedAt)
		}
		return a.firstIdx < b.firstIdx
	})
	return buckets
}

// runGroup is a workflow run with its jobs bucketed by phase, ready to render.
type runGroup struct {
	runID     string
	name      string
	status    string
	startedAt string
	updatedAt string
	phases    []runPhaseGroup
}

// elapsed renders the run's wall-clock from StartedAt to its last heartbeat
// (UpdatedAt) when set, else to now. A run with no parseable StartedAt renders "—".
func (g runGroup) elapsed() string {
	start, err := time.Parse(time.RFC3339, g.startedAt)
	if err != nil {
		return "—"
	}
	end := time.Now()
	if g.updatedAt != "" {
		if u, uerr := time.Parse(time.RFC3339, g.updatedAt); uerr == nil {
			end = u
		}
	}
	d := end.Sub(start)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

// runPhaseGroup is one phase of a run with the jobs observed in it.
type runPhaseGroup struct {
	title string
	jobs  []subagent.Result
}

// groupByRun joins RunID-tagged jobs to their run manifests into a run→phase→job
// tree. A run's manifest supplies its Name/Status/StartedAt and the declared
// phase order; phases observed on a job but absent from the manifest are appended
// in first-seen order. A run with no manifest (GC'd or never created) carries an
// empty name and phases in first-seen order. Runs sort newest-first by StartedAt
// (the manifest's, else the earliest job StartedAt), RFC3339 string compare —
// same discipline as groupedJobsBySession.
func groupByRun(jobs []subagent.Result, runs []subagent.WorkflowRun) []runGroup {
	byRunID := map[string]subagent.WorkflowRun{}
	for _, r := range runs {
		byRunID[r.RunID] = r
	}

	// Assemble groups in first-seen order (manifest first, then jobs), so a run is
	// created even when it has a manifest but zero jobs yet (phase skeleton).
	order := []string{}
	groups := map[string]*runGroup{}
	phaseIdx := map[string]map[string]int{} // runID → phase title → index into phases

	ensureRun := func(runID string) *runGroup {
		g, ok := groups[runID]
		if ok {
			return g
		}
		g = &runGroup{runID: runID}
		if r, ok := byRunID[runID]; ok {
			g.name = r.Name
			g.status = r.Status
			g.startedAt = r.StartedAt
			g.updatedAt = r.UpdatedAt
		}
		groups[runID] = g
		phaseIdx[runID] = map[string]int{}
		order = append(order, runID)
		return g
	}
	ensurePhase := func(g *runGroup, title string) int {
		idx := phaseIdx[g.runID]
		if i, ok := idx[title]; ok {
			return i
		}
		i := len(g.phases)
		g.phases = append(g.phases, runPhaseGroup{title: title})
		idx[title] = i
		return i
	}

	// Manifest-declared runs first: this both seeds the manifest phase order and
	// renders a freshly-created run's phase skeleton before any job lands.
	for _, r := range runs {
		g := ensureRun(r.RunID)
		for _, p := range r.Phases {
			ensurePhase(g, p.Title)
		}
	}

	// Then the jobs: their run may have no manifest, and their phase may be a
	// manifest-absent extra (appended after the declared phases).
	for _, j := range jobs {
		g := ensureRun(j.RunID)
		i := ensurePhase(g, j.Phase)
		g.phases[i].jobs = append(g.phases[i].jobs, j)
		// For a run with no manifest, derive its sort key from the earliest job
		// StartedAt. A manifested run already carries the manifest's StartedAt.
		if _, hasManifest := byRunID[j.RunID]; !hasManifest && j.StartedAt != "" {
			if g.startedAt == "" || j.StartedAt < g.startedAt {
				g.startedAt = j.StartedAt
			}
		}
	}

	out := make([]runGroup, 0, len(order))
	for _, id := range order {
		out = append(out, *groups[id])
	}
	// Newest-first by StartedAt; empty StartedAt sorts last, first-seen order as
	// the stable tiebreaker.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].startedAt, out[j].startedAt
		if a != b {
			if a == "" {
				return false
			}
			if b == "" {
				return true
			}
			return a > b
		}
		return false
	})
	return out
}

func (m Model) sessionLabel(id string) string {
	if id == "" {
		return "(no session)"
	}
	short := shortSessionID(id)
	// Use sessiontitle.CleanTitle so the board header strips ANSI/BEL/OSC control
	// bytes (not just whitespace) before display.
	if title := sessiontitle.CleanTitle(m.sessionTitles[id]); title != "" {
		return trunc(title, 48) + " (" + short + ")"
	}
	return short
}

func shortSessionID(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

// viewTeammateDetail renders the full-field detail card for the board-selected
// teammate: every field UNtruncated, so the operator can read values the table
// clips (vendor / model / detail). Read-only — esc/enter returns to the board.
// It shows the same canonical health fields as `ps --check` (never raw pane
// text), so nothing here can leak a vendor reply.
func (m Model) viewTeammateDetail() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("cc-fleet · teammate detail") + footer("    esc back") + "\n\n")
	if m.spawnCursor < 0 || m.spawnCursor >= len(m.teammates) {
		b.WriteString(faintStyle.Render("  (no teammate selected)") + "\n")
		b.WriteString("\n" + footer("esc back"))
		return b.String()
	}
	t := m.teammates[m.spawnCursor]
	b.WriteString(selectedStyle.Render("  "+t.Name) + faintStyle.Render(" @ "+t.Team) + "\n\n")
	field := func(k, v string) {
		if v == "" {
			v = "—"
		}
		b.WriteString("  " + faintStyle.Render(fmt.Sprintf("%-8s", k)) + "  " + v + "\n")
	}
	field("vendor", t.Vendor)
	field("model", t.Model)
	field("pane", t.PaneID)
	field("pid", fmt.Sprintf("%d", t.PID))
	status := t.Status
	if status == "" {
		status = "—"
	}
	field("status", status)
	if t.ErrorClass != "" {
		field("error", t.ErrorClass)
	}
	if t.Detail != "" {
		field("detail", t.Detail)
	}
	hidden := "no"
	if t.Hidden {
		hidden = "yes"
	}
	field("hidden", hidden)
	b.WriteString("\n" + footer("esc/enter back · q quit"))
	return b.String()
}

// shortJobID trims a job UUID to its first 8 chars for the board's JOB column.
func shortJobID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (m Model) viewPickTemplate() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Add vendor") + faintStyle.Render("  ·  pick a template") + "\n\n")

	rows := make([]string, 0, len(Templates)+1)
	for _, t := range Templates {
		rows = append(rows, t.Label)
	}
	rows = append(rows, "Custom vendor (fill everything manually)")

	for i, label := range rows {
		cursor := "  "
		line := label
		if i == m.tmplCursor {
			cursor = cursorStyle.Render("> ")
			line = selectedStyle.Render(label)
		}
		b.WriteString(cursor + line + "\n")
	}

	// Preview the highlighted template's seed values so the user sees what
	// will be prefilled before committing to the form.
	if m.tmplCursor < len(Templates) {
		t := Templates[m.tmplCursor]
		b.WriteString("\n" + faintStyle.Render("  base_url        "+t.BaseURL) + "\n")
		b.WriteString(faintStyle.Render("  models_endpoint "+t.ModelsEndpoint) + "\n")
		b.WriteString(faintStyle.Render("  default_model   "+t.DefaultModel) + "\n")
		if t.Note != "" {
			b.WriteString(errStyle.Render("  note: "+t.Note) + "\n")
		}
	} else {
		b.WriteString("\n" + faintStyle.Render("  all fields start blank") + "\n")
	}

	b.WriteString("\n" + footer("↑/↓ move · enter choose · esc cancel"))
	return b.String()
}

func (m Model) viewRemoveConfirm() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Remove vendor") + "\n\n")
	b.WriteString("Remove " + selectedStyle.Render(m.removeName) +
		" from vendors.toml, delete its profile, and (for file backend) its secret?\n")
	b.WriteString("\n" + footer("y confirm · n/esc cancel"))
	return b.String()
}

// tmuxOptions are the two choices on the tmux setup screen, in cursor order
// (index 0 = "install it", handled specially by updateSetupTmux).
var tmuxOptions = []string{
	"install it  (I'll run the command, then restart ccf)",
	"skip — I'll only use subagent mode",
}

// setupOptions are the three choices on the agent-teams setup nudge, in cursor
// order (index 0 = "enable it for me", handled specially by updateSetup). The
// trailing "skip — …" wording is kept identical to tmuxOptions' so the two
// setup screens read the same.
var setupOptions = []string{
	"enable it for me  (writes ~/.claude/settings.json)",
	"I've set it up myself",
	"skip — I'll only use subagent mode",
}

// renderSetupOptions renders a cursor-highlighted option list shared by both
// setup screens, so the tmux and agent-teams nudges stay visually identical.
func renderSetupOptions(opts []string, cursor int) string {
	var b strings.Builder
	for i, opt := range opts {
		marker := "  "
		line := opt
		if i == cursor {
			marker = cursorStyle.Render("> ")
			line = selectedStyle.Render(opt)
		}
		b.WriteString(marker + line + "\n")
	}
	return b.String()
}

// viewSetupTmux renders the first-run tmux setup nudge. tmux is needed to spawn
// teammate panes but optional for one-shot subagent jobs, so this offers
// install-vs-subagent-only rather than forcing it.
func (m Model) viewSetupTmux() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("cc-fleet · setup") + "\n\n")
	b.WriteString("tmux isn't installed — it's needed to spawn teammate panes.\n")
	b.WriteString(faintStyle.Render("(one-shot `cc-fleet subagent` jobs work without it.)") + "\n\n")
	b.WriteString(renderSetupOptions(tmuxOptions, m.tmuxCursor))
	b.WriteString("\n" + footer("↑/↓ move · enter select · esc skip"))
	return b.String()
}

// viewSetup renders the first-run agent-teams setup nudge. The wording is a
// SUGGESTION, never an assertion that agent-teams is off — we only know it isn't
// explicitly configured in env / rc / settings.json, and Claude may well have it
// on by default. Once setupMsg is set (after "enable it for me"), it replaces
// the options with a one-line outcome.
func (m Model) viewSetup() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("cc-fleet · setup") + "\n\n")
	if m.setupMsg != "" {
		b.WriteString(m.setupMsg + "\n")
		b.WriteString("\n" + footer("enter to continue"))
		return b.String()
	}
	b.WriteString("agent-teams isn't set in your env / shell rc / settings.json.\n")
	b.WriteString("It powers vendor " + selectedStyle.Render("teammates") + ".\n")
	b.WriteString(faintStyle.Render("(one-shot `cc-fleet subagent` jobs work without it.)") + "\n\n")
	b.WriteString(renderSetupOptions(setupOptions, m.setupCursor))
	b.WriteString("\n" + footer("↑/↓ move · enter select · esc skip"))
	return b.String()
}

func (m Model) viewResult() string {
	var b strings.Builder
	if m.resultErr {
		b.WriteString(errStyle.Render("✗ "+m.result) + "\n")
	} else {
		b.WriteString(okStyle.Render("✓ "+m.result) + "\n")
	}
	b.WriteString("\n" + footer("press any key to return to Vendors"))
	return b.String()
}

// maxVisibleModels caps how many model rows the picker shows at once; longer
// lists scroll a window around the cursor (some vendors return 50+ models).
const maxVisibleModels = 12

func (m Model) viewModelPick() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Select default model") + "\n\n")
	switch {
	case m.loading:
		b.WriteString("fetching models…\n")
	case m.modelsErr != nil:
		b.WriteString(errStyle.Render("couldn't fetch models: "+m.modelsErr.Error()) + "\n")
		b.WriteString(faintStyle.Render("press esc to type the model id manually") + "\n")
	case len(m.modelList) == 0:
		b.WriteString(faintStyle.Render("vendor returned no models") + "\n")
		b.WriteString(faintStyle.Render("press esc to type the model id manually") + "\n")
	default:
		filtered := m.filteredModels()
		total := len(m.modelList)
		if m.modelFilter == "" {
			b.WriteString(faintStyle.Render(fmt.Sprintf("filter: type to narrow %d models", total)) + "\n\n")
		} else {
			b.WriteString("filter: " + m.modelFilter +
				faintStyle.Render(fmt.Sprintf("  (%d/%d)", len(filtered), total)) + "\n\n")
		}
		if len(filtered) == 0 {
			b.WriteString(faintStyle.Render("no model matches — backspace to widen, esc to type manually") + "\n")
			break
		}
		start, end := windowBounds(m.modelCursor, len(filtered), maxVisibleModels)
		if start > 0 {
			b.WriteString(faintStyle.Render(fmt.Sprintf("    ↑ %d more", start)) + "\n")
		}
		for i := start; i < end; i++ {
			mod := filtered[i]
			cursor := "  "
			id := mod.ID
			if i == m.modelCursor {
				cursor = cursorStyle.Render("> ")
				id = selectedStyle.Render(mod.ID)
			}
			b.WriteString(cursor + id + "\n")
			if mod.OwnedBy != "" {
				b.WriteString(faintStyle.Render("    "+mod.OwnedBy) + "\n")
			}
		}
		if end < len(filtered) {
			b.WriteString(faintStyle.Render(fmt.Sprintf("    ↓ %d more", len(filtered)-end)) + "\n")
		}
	}
	b.WriteString("\n" + footer("type to filter · ↑/↓ move · enter pick · esc manual entry"))
	return b.String()
}

// windowBounds returns the [start,end) slice of indices to render so the cursor
// stays visible when a list of n items is longer than max.
func windowBounds(cursor, n, max int) (int, int) {
	if n <= max {
		return 0, n
	}
	start := cursor - max/2
	if start < 0 {
		start = 0
	}
	end := start + max
	if end > n {
		end = n
		start = end - max
	}
	return start, end
}

// trunc shortens s to n runes, appending "…" when it had to cut.
func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
