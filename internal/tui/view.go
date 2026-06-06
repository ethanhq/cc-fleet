package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

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
	contentStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245")) // board body text — softer than the bright default, above faint
	liveStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("252")) // active (done/running) labels + the answer body — bright, below the frame
	borderStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("255")) // master-detail box frame — the strongest line (near-white, like native)
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

// viewWorkflows renders the native-mirror Workflows board: a persistent run header + ONE enclosing
// master-detail box that re-roots on wfMode (run picker → Phases overview → agent detail). The agent
// ROWS show only field-source-safe data — status / metric columns + the per-agent Activity feed
// (tool name + masked arg), NEVER Result.Result. The focused agent's inline detail additionally
// reads its prompt/output from the leaf io files (PersistIO opt-in). Run name, phase title, agent
// label, model, and tool signatures are opaque
// operator/model metadata sanitized through sessiontitle.CleanTitle before display.
func (m Model) viewWorkflows() string {
	var b strings.Builder
	switch {
	case m.loading:
		b.WriteString(titleStyle.Render("cc-fleet · Workflows") + "\n\nloading…")
	case m.workflowsErr != nil:
		b.WriteString(titleStyle.Render("cc-fleet · Workflows") + "\n\n" +
			errStyle.Render("error: "+sessiontitle.CleanTitle(m.workflowsErr.Error())))
	case m.wfMode == wfModePicker:
		b.WriteString(m.viewWfPicker())
	case m.wfMode == wfModeAgent:
		b.WriteString(m.viewWfAgent())
	default:
		if _, ok := m.focusedGroup(); !ok {
			b.WriteString(titleStyle.Render("cc-fleet · Workflows") +
				faintStyle.Render("    tab → Vendors") + "\n\n" + faintStyle.Render("(no workflow runs)"))
		} else {
			b.WriteString(m.viewWfPhases())
		}
	}
	switch {
	case m.wfSaving:
		b.WriteString("\n" + faintStyle.Render("save as: ") + m.wfSaveInput.View() +
			faintStyle.Render("  · enter save · esc cancel"))
	case m.workflowStatus != "":
		style := okStyle
		if m.workflowStatusErr {
			style = errStyle
		}
		b.WriteString("\n" + style.Render(sessiontitle.CleanTitle(m.workflowStatus)))
	}
	b.WriteString("\n" + renderWfFooter(m.wfMode))
	return b.String()
}

// statusDot maps a leaf/run/phase status to a colored glyph: done ✔ (green), running ● (accent),
// failed/stopped ● (err), cached ○ (faint), queued/unknown ◌ (faint hollow).
func statusDot(status string) string {
	switch status {
	case "done":
		return okStyle.Render("✔")
	case "running":
		return cursorStyle.Render("●")
	case "failed", "stopped":
		return errStyle.Render("●")
	case "cached":
		return faintStyle.Render("○")
	default: // "" / queued / not-yet-started
		return faintStyle.Render("◌")
	}
}

// labelStyle colors a board row label by progress: bright (liveStyle) for a reached row
// (done/running/failed/stopped), faint for a queued/not-started/cached one. The cursored row overrides
// this with selectedStyle (focus precedence), so labelStyle applies to non-cursored rows only.
func labelStyle(status string) lipgloss.Style {
	switch status {
	case "done", "running", "failed", "stopped":
		return liveStyle
	default: // "" (queued/not-started) / "cached"
		return faintStyle
	}
}

// phaseStatus derives a phase's progress from its agent counts: done when all finished, running when
// some have started, "" (queued) when none have — so a phase row colors like a leaf row.
func phaseStatus(done, total int) string {
	switch {
	case total > 0 && done >= total:
		return "done"
	case total > 0:
		return "running"
	default:
		return ""
	}
}

// statusLabel is the detail-pane status token (glyph + word in one color): done renders an all-green
// "✔ Done", failed/stopped a red dot + word, running/other an accent dot + a bright word.
func statusLabel(status string) string {
	switch status {
	case "done":
		return okStyle.Render("✔ " + humanStatus(status))
	case "failed", "stopped":
		return statusDot(status) + " " + errStyle.Render(humanStatus(status))
	default:
		return statusDot(status) + " " + liveStyle.Render(humanStatus(status))
	}
}

// humanStatus title-cases a status word for the detail card ("running" → "Running"); empty → "Running".
func humanStatus(status string) string {
	if status == "" {
		return "Running"
	}
	return strings.ToUpper(status[:1]) + status[1:]
}

// boardWidth is the usable board width — m.width, or a default when no WindowSizeMsg has arrived
// (every board unit test renders at width 0, so the panes must still size positively).
func (m Model) boardWidth() int {
	if m.width > 40 {
		return m.width
	}
	return 100
}

// phaseAgentCounts / runAgentCounts return (done, total) where done counts terminal (non-running) leaves.
func phaseAgentCounts(p runPhaseGroup) (done, total int) {
	for _, j := range p.jobs {
		total++
		if j.Status != "" && j.Status != "running" {
			done++
		}
	}
	return
}

func runAgentCounts(g runGroup) (done, total int) {
	for _, p := range g.phases {
		d, t := phaseAgentCounts(p)
		done += d
		total += t
	}
	return
}

// renderRunHeader is the persistent native header: the run name (bold) + description (faint) on the
// left, and the right-aligned "<done>/<total> agents · <elapsed>".
func (m Model) renderRunHeader(g runGroup) string {
	done, total := runAgentCounts(g)
	left := titleStyle.Render(m.runLabel(g))
	if g.description != "" {
		left += faintStyle.Render("  " + trunc(sessiontitle.CleanTitle(g.description), 60))
	}
	right := faintStyle.Render(fmt.Sprintf("%d/%d agents · %s", done, total, g.elapsed()))
	gap := m.boardWidth() - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		// A long name+description must not wrap the header onto a second line — that would shift the
		// fixed-height box down. Truncate the left side to fit one line beside the right summary.
		left = boxCell(left, m.boardWidth()-lipgloss.Width(right)-1)
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// boardBodyHeight is the inner row budget for the master-detail box (drives right-pane scroll). It
// derives from the terminal height, with a default for tests / pre-WindowSizeMsg renders.
func (m Model) boardBodyHeight() int {
	h := m.height
	if h < 12 {
		h = 24
	}
	avail := h - 8 // header + blank + box top/bottom + status + footer + margin
	if avail < 5 {
		avail = 5
	}
	return avail
}

// boxCell pads (or ANSI-aware-truncates) a possibly-styled line to EXACTLY w visible columns. After a
// truncation it re-pads: ansi.Truncate refuses to split a double-width (CJK) glyph, so cutting on a
// wide-char boundary returns w-1 columns — without the re-pad the right border would shift left by one.
func boxCell(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if ansi.StringWidth(s) > w {
		s = ansi.Truncate(s, w, "")
	}
	if pad := w - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// boxBorder builds one rounded box border row — a top "╭ <lt> ─┬ <rt> ─╮" or a bottom "╰─┴─ <extra>╯".
// leftW/rightW are the inner cell widths; each title segment spans its cell width + 4 (the cell's two
// surrounding spaces on each side). rightExtra is appended at the right segment's end (the bottom-border scroll
// indicator). The whole line renders in the box frame color so titles read as part of the frame.
func boxBorder(open, join, clos, leftTitle, rightTitle, rightExtra string, leftW, rightW int) string {
	seg := func(title, extra string, width int) string {
		if title == "" {
			fill := width - ansi.StringWidth(extra)
			if fill < 0 {
				fill = 0
			}
			return strings.Repeat("─", fill) + extra
		}
		head := "  " + title + " "
		fill := width - ansi.StringWidth(head) - ansi.StringWidth(extra)
		if fill < 0 {
			return boxCell(head, width-ansi.StringWidth(extra)) + extra
		}
		return head + strings.Repeat("─", fill) + extra
	}
	return borderStyle.Render(open + seg(leftTitle, "", leftW+4) + join + seg(rightTitle, rightExtra, rightW+4) + clos)
}

// renderBoard draws the native single enclosing box: two title segments over an internal divider
// (┬/┴-joined), the left pane's rows beside a scroll-window of the right pane's rows, and a bottom-
// right "↑ a–b of T ↓" when the right pane overflows bodyH. Both panes' lines are pre-styled; cells
// are ANSI-aware padded/truncated to the column widths.
func renderBoard(leftTitle string, leftLines []string, rightTitle string, rightLines []string, leftW, rightW, bodyH, scroll int) string {
	if scroll < 0 {
		scroll = 0
	}
	rightExtra := ""
	if len(rightLines) > bodyH {
		last := scroll + bodyH
		if last > len(rightLines) {
			last = len(rightLines)
		}
		rightExtra = fmt.Sprintf(" ↑ %d–%d of %d ↓ ", scroll+1, last, len(rightLines))
	}
	var b strings.Builder
	b.WriteString(boxBorder("╭", "┬", "╮", leftTitle, rightTitle, "", leftW, rightW) + "\n")
	bar := borderStyle.Render("│")
	for i := 0; i < bodyH; i++ {
		l, r := "", ""
		if i < len(leftLines) {
			l = leftLines[i]
		}
		if ri := i + scroll; ri < len(rightLines) {
			r = rightLines[ri]
		}
		b.WriteString(bar + "  " + boxCell(l, leftW) + "  " + bar + "  " + boxCell(r, rightW) + "  " + bar + "\n")
	}
	b.WriteString(boxBorder("╰", "┴", "╯", "", "", rightExtra, leftW, rightW))
	return b.String()
}

// windowLines keeps the cursor visible for a list longer than the box: it returns up to height lines
// centered on cursor. A list that already fits is returned unchanged.
func windowLines(lines []string, cursor, height int) []string {
	if height < 1 || len(lines) <= height {
		return lines
	}
	start := cursor - height/2
	if start < 0 {
		start = 0
	}
	if start+height > len(lines) {
		start = len(lines) - height
	}
	if start < 0 {
		start = 0
	}
	return lines[start : start+height]
}

// leftWidth sizes the master list (left rail) to its content — the wider of its title and its widest
// row — rather than a fixed fraction, so a short phase list doesn't hog the frame (the native board's
// left rail hugs its labels). Clamped to [14, boardWidth/2]. The right pane gets the rest.
func leftWidth(title string, lines []string, boardW int) int {
	w := ansi.StringWidth(title)
	for _, l := range lines {
		if sw := ansi.StringWidth(l); sw > w {
			w = sw
		}
	}
	w += 2 // breathing room past the widest label
	if w < 14 {
		w = 14
	}
	if cap := boardW / 2; w > cap {
		w = cap
	}
	return w
}

// paneWidths derives the right pane from a content-sized left: left + right + 11 == boardWidth (the 11
// non-content columns are the two outer borders, the divider, and TWO spaces on each side of each cell).
// leftW is CAPPED to always leave ≥20 columns for the detail pane, so the box never overflows (the
// floor only bites on a sub-41-column terminal, where the box must wrap regardless).
func (m Model) paneWidths(leftW int) (left, right int) {
	avail := m.boardWidth() - 11
	if avail < 30 {
		avail = 30
	}
	if leftW > avail-20 {
		leftW = avail - 20
	}
	if leftW < 10 {
		leftW = 10
	}
	return leftW, avail - leftW
}

// viewWfPicker is the run picker (shown only when >1 run): runs grouped under their launching
// session like the teammates board (groupByRun orders runs session-contiguous), the cursor walking
// the flat run list. Each session prints one "◆ <session>" header.
func (m Model) viewWfPicker() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("cc-fleet · Workflows") + faintStyle.Render("    tab → Vendors") + "\n\n")
	lastSession := "\x00" // sentinel so the first header always prints (even for a "" session)
	for i, g := range m.wfGroups() {
		if g.sessionID != lastSession {
			lastSession = g.sessionID
			b.WriteString(sessionHdrStyle.Render("◆ "+m.sessionLabel(g.sessionID)) + "\n")
		}
		marker := "  "
		name := trunc(m.runLabel(g), 42)
		if i == m.wfRunCursor {
			marker = cursorStyle.Render("❯ ")
			name = selectedStyle.Render(name)
		} else {
			name = labelStyle(g.status).Render(name)
		}
		done, total := runAgentCounts(g)
		b.WriteString(fmt.Sprintf("  %s%s %s  %s\n", marker, statusDot(g.status), name,
			faintStyle.Render(fmt.Sprintf("%d/%d agents · %s", done, total, g.elapsed()))))
	}
	return b.String()
}

// viewWfPhases is L1: the run header above one box — "Phases | the selected phase's agents". The box
// is a FIXED height (fills the screen) so the bottom border stays put; the left rail is content-sized.
func (m Model) viewWfPhases() string {
	g, ok := m.focusedGroup()
	if !ok {
		return faintStyle.Render("(no workflow runs)")
	}
	var leftLines []string
	for i, p := range g.phases {
		marker := "  "
		done, total := phaseAgentCounts(p)
		st := phaseStatus(done, total)
		title := trunc(sessiontitle.CleanTitle(p.title), 28)
		// A completed phase shows a green ✔ where its index would be; otherwise the 1-based index.
		glyph := fmt.Sprintf("%d", i+1)
		if st == "done" {
			glyph = statusDot("done")
		}
		if i == m.wfPhaseCursor {
			marker = cursorStyle.Render("❯ ")
			title = selectedStyle.Render(title)
		} else {
			title = labelStyle(st).Render(title)
		}
		counts := ""
		if total > 0 {
			counts = "  " + faintStyle.Render(fmt.Sprintf("%d/%d", done, total))
		}
		leftLines = append(leftLines, fmt.Sprintf("%s%s %s%s", marker, glyph, title, counts))
	}
	leftW, rightW := m.paneWidths(leftWidth("Phases", leftLines, m.boardWidth()))
	rightTitle, rightLines := m.phaseAgentLines(rightW)
	bodyH := m.boardBodyHeight()
	leftLines = windowLines(leftLines, m.wfPhaseCursor, bodyH)
	return m.renderRunHeader(g) + "\n\n" +
		renderBoard("Phases", leftLines, rightTitle, rightLines, leftW, rightW, bodyH, 0)
}

// phaseAgentLines returns the title + full agent rows (right pane) for the focused phase ("Not started
// yet" when empty). width is the right pane width — the metrics right-align to it.
func (m Model) phaseAgentLines(width int) (title string, lines []string) {
	p, ok := m.focusedPhase()
	if !ok {
		return "agents", []string{faintStyle.Render("Not started yet")}
	}
	title = fmt.Sprintf("%s · %d agents", trunc(sessiontitle.CleanTitle(p.title), 20), len(p.jobs))
	if len(p.jobs) == 0 {
		return title, []string{faintStyle.Render("Not started yet")}
	}
	for _, j := range p.jobs {
		lines = append(lines, m.renderAgentRowFull(j, width))
	}
	return title, lines
}

// agentLeftLines builds the COMPACT agent list shown in the L2 left rail (status + label only; the
// metrics live in the detail pane), plus its title. Shared by viewWfAgent and wfAgentRightWidth so the
// scroll clamp and the render agree on the right pane width.
func (m Model) agentLeftLines() (title string, lines []string) {
	p, ok := m.focusedPhase()
	if !ok {
		return "agents", nil
	}
	title = fmt.Sprintf("%s · %d agents", trunc(sessiontitle.CleanTitle(p.title), 20), len(p.jobs))
	for i, j := range p.jobs {
		lines = append(lines, m.renderAgentRowCompact(j, i == m.wfAgentCursor))
	}
	return title, lines
}

// wfAgentRightWidth is the L2 right pane width (mirrors viewWfAgent's content-sized split) so the
// scroll clamp wraps the detail to the same column budget the render uses.
func (m Model) wfAgentRightWidth() int {
	title, lines := m.agentLeftLines()
	_, rightW := m.paneWidths(leftWidth(title, lines, m.boardWidth()))
	return rightW
}

// viewWfAgent is L2: the run header above one box — "agent list | the focused agent's inline detail"
// (the right pane scrolls with j/k via wfCardScroll). Fixed-height box; content-sized left rail.
func (m Model) viewWfAgent() string {
	g, ok := m.focusedGroup()
	if !ok {
		return faintStyle.Render("(no workflow runs)")
	}
	listTitle, leftLines := m.agentLeftLines()
	leftW, rightW := m.paneWidths(leftWidth(listTitle, leftLines, m.boardWidth()))
	cardTitle := "agent"
	if j, jok := m.selectedLeaf(); jok {
		if t := trunc(sessiontitle.CleanTitle(j.Label), rightW-6); t != "" {
			cardTitle = t
		}
	}
	rightLines := m.agentDetailLines(rightW)
	bodyH := m.boardBodyHeight()
	leftLines = windowLines(leftLines, m.wfAgentCursor, bodyH)
	return m.renderRunHeader(g) + "\n\n" +
		renderBoard(listTitle, leftLines, cardTitle, rightLines, leftW, rightW, bodyH, m.clampCardScroll(m.wfCardScroll))
}

// renderAgentRowFull is one agent row for a phase's agent list (right pane): "<dot> <label>  <model>"
// left, "<tok> tok · <N> tools · <dur>" RIGHT-ALIGNED to width. Live tokens/tools for a RUNNING leaf
// come from its activity snapshot; a done leaf uses its final Result metrics. No answer text.
func (m Model) renderAgentRowFull(j subagent.Result, width int) string {
	in, out, tools := m.leafCounts(j)
	label := sessiontitle.CleanTitle(j.Label)
	model := sessiontitle.CleanTitle(j.Model)
	left := statusDot(j.Status) + " "
	switch {
	case label != "":
		left += labelStyle(j.Status).Render(label)
		if model != "" {
			left += "  " + faintStyle.Render(trunc(model, 22))
		}
	case model != "":
		left += faintStyle.Render(trunc(model, 28)) // unlabeled leaf → the model is its identifier
	default:
		left += faintStyle.Render("agent")
	}
	metrics := fmt.Sprintf("%s tok · %d tools", humanTokens(in+out), tools)
	if d := leafDuration(j); d != "" {
		metrics += " · " + d
	}
	right := faintStyle.Render(metrics)
	rw := ansi.StringWidth(right)
	gap := width - ansi.StringWidth(left) - rw
	if gap < 1 {
		if avail := width - rw - 1; avail >= 1 {
			left = boxCell(left, avail) // tight: shrink the label to fit beside the metrics
			gap = 1
		} else {
			return boxCell(right, width) // pathologically narrow: metrics alone, truncated
		}
	}
	return left + strings.Repeat(" ", gap) + right
}

// renderAgentRowCompact is one agent row for the L2 left rail: marker + status + label only (narrow).
func (m Model) renderAgentRowCompact(j subagent.Result, selected bool) string {
	marker := "  "
	label := sessiontitle.CleanTitle(j.Label)
	if label == "" {
		if model := sessiontitle.CleanTitle(j.Model); model != "" {
			label = model // unlabeled leaf → the model is its identifier
		} else {
			label = "agent"
		}
	}
	if selected {
		marker = cursorStyle.Render("❯ ")
		label = selectedStyle.Render(label)
	} else {
		label = labelStyle(j.Status).Render(label)
	}
	return marker + statusDot(j.Status) + " " + label
}

// leafDuration formats a done leaf's wall-clock (DurationMs) as "30s" / "2m 3s"; "" while running.
func leafDuration(j subagent.Result) string {
	if j.DurationMs <= 0 {
		return ""
	}
	d := time.Duration(j.DurationMs) * time.Millisecond
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm %ds", int(d/time.Minute), int(d.Seconds())%60)
}

// leafCounts returns the leaf's (input, output) tokens + tool-call count: live from the activity
// snapshot (a monotonic count) while running, the accurate final from the Result once done. Tool
// count always comes from the snapshot (the final Result doesn't carry it).
func (m Model) leafCounts(j subagent.Result) (in, out, tools int) {
	if j.Usage != nil {
		in, out = j.Usage.InputTokens, j.Usage.OutputTokens
	}
	snap := m.wfActivity[j.JobID]
	if j.Status == "running" && snap.hasUsage {
		in, out = snap.inTok, snap.outTok
	}
	return in, out, snap.toolCount()
}

// agentDetailLines is the focused agent's inline detail (the L2 right pane, scrollable): status/model,
// tokens·tool-calls, the Activity feed (last 3 tool signatures), the Outcome, and — when the io files
// are loaded for THIS leaf (PersistIO opt-in) — the Prompt + Output. The Output reads from the leaf's
// .answer side file (focused-single-agent surface, CleanTitle-scrubbed), NEVER Result.Result on a row.
func (m Model) agentDetailLines(rightW int) []string {
	j, ok := m.selectedLeaf()
	if !ok {
		return []string{faintStyle.Render("(no agent)")}
	}
	in, out, tools := m.leafCounts(j)
	snap := m.wfActivity[j.JobID]
	lines := []string{
		statusLabel(j.Status) + faintStyle.Render(" · "+trunc(sessiontitle.CleanTitle(j.Model), 28)),
		faintStyle.Render(fmt.Sprintf("%s tok · %d tool calls", humanTokens(in+out), tools)),
		"",
		faintStyle.Render(fmt.Sprintf("Activity · last 3 of %d tool calls", tools)),
	}
	sigs := snap.lastSigs(3)
	if len(sigs) == 0 {
		lines = append(lines, faintStyle.Render(" (no tool calls)"))
	}
	for _, s := range sigs {
		lines = append(lines, " "+contentStyle.Render(truncCols(sessiontitle.CleanTitle(s), rightW-2)))
	}
	lines = append(lines, "", faintStyle.Render("Outcome"), " "+m.renderOutcome(j))
	switch {
	case m.wfDetailJob.JobID != j.JobID:
		lines = append(lines, "", faintStyle.Render("(loading…)"))
	case !m.wfDetailIO:
		lines = append(lines, "", faintStyle.Render("(prompt/output not persisted — run with default persist-io)"))
	default:
		lines = append(lines, "")
		if m.wfPromptExpanded {
			lines = append(lines, faintStyle.Render("Prompt"))
			lines = append(lines, ioLines(m.wfDetailPrompt, rightW, contentStyle)...)
		} else {
			total := promptLineCount(m.wfDetailPrompt)
			lines = append(lines, faintStyle.Render(fmt.Sprintf("Prompt · %d lines · ⏎ expand", total)))
			lines = append(lines, ioLines(firstLogicalLines(m.wfDetailPrompt, promptPreviewLines), rightW, contentStyle)...)
			if more := total - promptPreviewLines; more > 0 {
				lines = append(lines, faintStyle.Render(fmt.Sprintf("… %d more lines", more)))
			}
		}
		lines = append(lines, "", faintStyle.Render("Output"))
		lines = append(lines, ioLines(m.wfDetailAnswer, rightW, liveStyle)...)
	}
	return lines
}

// ioLines renders an io block (prompt or answer) preserving its source LOGICAL lines: each newline-
// delimited line is CleanTitle-scrubbed (scrub per line — CleanTitle collapses whitespace, so scrubbing
// the whole block first would lose the line breaks), then hard-wrapped to width-2 and indented ONE
// column within the cell. With the cell's own 2-column pane padding the body sits 3 columns from the
// left box border and a matching margin from the right (boxCell pads the spare column) — one step
// deeper than the section headers, so the hierarchy reads. An empty block shows a dim placeholder.
// style colors the body — the gray contentStyle for the prompt, the bright liveStyle for the answer.
func ioLines(s string, width int, style lipgloss.Style) []string {
	if strings.TrimSpace(s) == "" {
		return []string{faintStyle.Render(" (empty)")}
	}
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		clean := sessiontitle.CleanTitle(ln)
		if clean == "" {
			out = append(out, "") // preserve a blank line between paragraphs
			continue
		}
		for _, w := range wrapTo(clean, width-2) {
			out = append(out, " "+style.Render(w))
		}
	}
	return out
}

// promptLineCount counts the prompt's logical (newline-delimited) lines for the collapsed
// "Prompt · N lines · ⏎ expand" summary — counted on the RAW text (before CleanTitle, which collapses
// the newlines), mirroring native's logical-line view and matching what ioLines renders on expand.
func promptLineCount(s string) int {
	s = strings.TrimRight(s, "\n")
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// promptPreviewLines is how many leading logical lines the collapsed prompt shows before "… N more".
const promptPreviewLines = 2

// firstLogicalLines returns the first n newline-delimited lines of s (raw, pre-scrub), so the collapsed
// prompt preview slices on the same boundaries promptLineCount counts.
func firstLogicalLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, "\n")
}

// truncCols truncates a plain (un-styled) string to w DISPLAY columns with an "…" tail (CJK-aware), so
// a tool signature is bounded by columns, not runes — leaving the pane's right margin intact.
func truncCols(s string, w int) string {
	if w < 1 {
		w = 1
	}
	if ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// wrapTo hard-wraps a plain (un-styled) string to w DISPLAY columns — CJK-aware (a wide glyph counts
// as 2), so a double-width line doesn't overflow the pane and get truncated off-screen.
func wrapTo(s string, w int) []string {
	if w < 1 {
		w = 1
	}
	if ansi.StringWidth(s) <= w {
		return []string{s}
	}
	var out []string
	var cur strings.Builder
	curW := 0
	for _, r := range s {
		rw := ansi.StringWidth(string(r))
		if curW+rw > w && curW > 0 {
			out = append(out, cur.String())
			cur.Reset()
			curW = 0
		}
		cur.WriteRune(r)
		curW += rw
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// renderOutcome is the key-safe outcome line: status + a canonical summary, NEVER Result.Result. A
// done leaf shows "done · N turns"; a failed one its error class; a running one "Still running…".
func (m Model) renderOutcome(j subagent.Result) string {
	switch {
	case j.Status == "running" || j.Status == "":
		return faintStyle.Render("Still running…")
	case j.OK || j.Status == "done":
		return faintStyle.Render(fmt.Sprintf("done · %d turns", j.NumTurns))
	default:
		cls := j.ErrorCode
		if cls == "" {
			cls = "failed"
		}
		return errStyle.Render(sessiontitle.CleanTitle(cls))
	}
}

// renderWfFooter is the contextual footer per wfMode. NOTE: no `p pause` — pause is a deliberate
// non-goal (vendor leaves have no cooperative-pause protocol).
func renderWfFooter(mode wfMode) string {
	switch mode {
	case wfModePicker:
		return footer("↑/↓ select · →/⏎ open · d delete · esc/tab vendors · R refresh · q quit")
	case wfModeAgent:
		return footer("↑/↓ agent · j/k scroll · ⏎ prompt · r restart agent · x stop · s save · ← back · q quit")
	default:
		return footer("↑/↓ phase · → agents · r restart · x stop · d delete · s save · ← back · tab vendors · q quit")
	}
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
	runID       string
	name        string
	description string
	sessionID   string // launching Claude session (picker grouping); "" when launched outside one
	status      string
	startedAt   string
	updatedAt   string
	phases      []runPhaseGroup
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
			g.description = r.Description
			g.sessionID = r.SessionID
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
		g := *groups[id]
		for i := range g.phases {
			g.phases[i].jobs = dedupePhaseJobs(g.phases[i].jobs)
		}
		out = append(out, g)
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
	// Then group sessions contiguously (a session ranked by its newest run), preserving the
	// newest-first order within each — so the picker headers each session exactly once.
	newestPerSession := map[string]string{}
	for _, g := range out {
		if g.startedAt > newestPerSession[g.sessionID] {
			newestPerSession[g.sessionID] = g.startedAt
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return newestPerSession[out[i].sessionID] > newestPerSession[out[j].sessionID]
	})
	return out
}

// dedupePhaseJobs collapses re-run leaves: within a phase, jobs sharing a non-empty Label keep only the
// newest by StartedAt (a single-leaf restart mints a fresh jobID; the old job lingers until GC). The
// slot identity is (Phase, Label), NOT the JournalKey, on purpose: a cascaded downstream re-run gets a
// NEW key (its input shifted) but keeps its label, so key-dedup would leave it doubled. The cost is two
// leaves an author gave the SAME non-empty label collapse to one row — acceptable, since a sensible
// board needs unique labels per phase (the native board requires them). Empty-Label leaves have no
// stable identity, so they are kept as-is. Order is preserved.
func dedupePhaseJobs(jobs []subagent.Result) []subagent.Result {
	out := make([]subagent.Result, 0, len(jobs))
	idx := map[string]int{} // non-empty Label → index in out
	for _, j := range jobs {
		if j.Label == "" {
			out = append(out, j)
			continue
		}
		if k, ok := idx[j.Label]; ok {
			if jobNewer(j, out[k]) {
				out[k] = j
			}
			continue
		}
		idx[j.Label] = len(out)
		out = append(out, j)
	}
	return out
}

// jobNewer reports whether a started strictly after b (StartedAt parsed as time, so a precision or
// format difference doesn't mis-rank). Unparseable timestamps sort as the zero time (oldest).
func jobNewer(a, b subagent.Result) bool {
	ta, _ := time.Parse(time.RFC3339, a.StartedAt)
	tb, _ := time.Parse(time.RFC3339, b.StartedAt)
	return ta.After(tb)
}

func (m Model) sessionLabel(id string) string {
	if id == "" {
		return "(no session)"
	}
	// Scrub both the opaque session id and any /rename title with CleanTitle so the board header
	// strips ANSI/BEL/OSC control bytes (not just whitespace) before display.
	short := shortSessionID(sessiontitle.CleanTitle(id))
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
