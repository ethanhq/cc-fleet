package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/ethanhq/cc-fleet/internal/secrets"
	"github.com/ethanhq/cc-fleet/internal/sessiontitle"
	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/teardown"
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

// viewSpawn renders the Agent-status board: a project-first master-detail in the Dynamic
// Workflows board's design language. asMode re-roots four levels under one shared
// header+rule chrome — projects → sessions → the session's Agent Teams + Subagents boxes →
// entity detail. Rows and cards show only field-source-safe data — canonical `ps --check`
// health vocabulary, CleanTitle-scrubbed names/models/ids, integer token counts — NEVER a
// job's answer text (Result.Result), an ErrorMsg body, or raw pane capture; the detail
// card's Output reads the focused job's .answer side file alone.
func (m Model) viewSpawn() string {
	var b strings.Builder
	switch {
	case m.loading:
		b.WriteString(m.spawnTitle() + "\n\ndiscovering…")
	case m.spawnErr != nil:
		b.WriteString(m.spawnTitle() + "\n\n" + errStyle.Render("error: "+m.spawnErr.Error()))
	case m.asMode == asModeProjects:
		b.WriteString(m.viewAsProjects())
	case m.asMode == asModeSessions:
		b.WriteString(m.viewAsSessions())
	case m.asMode == asModeEntity:
		b.WriteString(m.viewAsEntity())
	case m.asMode == asModeRunPhases:
		b.WriteString(m.viewWfPhases())
	case m.asMode == asModeRunAgent:
		b.WriteString(m.viewWfAgent())
	default:
		if _, ok := m.focusedSession(); !ok {
			b.WriteString(m.spawnTitle() + "\n\n" +
				faintStyle.Render("(no live agents — none spawned, and no subagent jobs)"))
		} else {
			b.WriteString(m.viewAsBoxes())
		}
	}
	// The jobs-scan failure renders on its OWN line so it never overwrites a surfaced
	// hide/show outcome (boardStatus keeps its survive-the-refresh semantics).
	if m.boardJobsErr != nil {
		b.WriteString("\n" + faintStyle.Render("jobs unavailable: "+
			sessiontitle.CleanTitle(m.boardJobsErr.Error())))
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
	// Run-control outcomes + the save-workflow name prompt (the run drill's controls).
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
	b.WriteString("\n" + m.renderAsFooter())
	return b.String()
}

// spawnTitle is the Agent-status app title + tab hint (picker / empty / loading / error
// states; the groups and entity levels show the session header instead).
func (m Model) spawnTitle() string {
	return titleStyle.Render("cc-fleet · Agent status") + faintStyle.Render("    tab → Vendors")
}

// renderAsFooter is the contextual footer per asMode; the boxes level swaps in the card
// keys while the cursor sits on a job row (its card is inline there).
func (m Model) renderAsFooter() string {
	switch m.asMode {
	case asModeProjects:
		return footer("↑/↓ project · →/⏎ open · esc/tab vendors · r refresh · q quit")
	case asModeSessions:
		return footer("↑/↓ session · →/⏎ open · ← back · r refresh · esc/tab vendors · q quit")
	case asModeEntity:
		return footer("↑/↓ row · j/k scroll · ⏎ expand · h hide · s show · ←/esc back · r refresh · q quit")
	case asModeRunPhases:
		return footer("↑/↓ phase · → agents · r restart · x stop · d delete · s save · ←/esc back · R refresh · q quit")
	case asModeRunAgent:
		return footer("↑/↓ agent · j/k scroll · ⏎ prompt · r restart agent · x stop · s save · ←/esc back · R refresh · q quit")
	default:
		if _, onJob := m.boxJob(); onJob {
			return footer("↑/↓ row · j/k scroll · ⏎ expand · ←/esc back · r refresh · q quit")
		}
		return footer("↑/↓ row · →/⏎ detail · d delete run · ← back · r refresh · esc/tab vendors · q quit")
	}
}

// statusDot maps a leaf/run/phase status to a colored glyph: done ✔ (green), running ● (accent),
// failed ● (err), stopped ■ (faint — a stop is neutral, not a failure), cached ○ (faint),
// queued/unknown ◌ (faint hollow).
func statusDot(status string) string {
	switch status {
	case "done":
		return okStyle.Render("✔")
	case "running":
		return cursorStyle.Render("●")
	case "failed":
		return errStyle.Render("●")
	case "stopped":
		return faintStyle.Render("■")
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
// "✔ Done", failed a red dot + word, stopped a faint dot + word (neutral), running/other an accent dot
// + a bright word.
func statusLabel(status string) string {
	switch status {
	case "done":
		return okStyle.Render("✔ " + humanStatus(status))
	case "failed":
		return statusDot(status) + " " + errStyle.Render(humanStatus(status))
	case "stopped":
		return statusDot(status) + " " + faintStyle.Render(humanStatus(status))
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

// boardMargin is the horizontal inset of the header + box; the full-width header rule overhangs the
// inset box by this much on each side.
const boardMargin = 2

// boardInner is the content width of the inset header + box (boardWidth minus the two-side margin).
func (m Model) boardInner() int {
	return m.boardWidth() - 2*boardMargin
}

// headerRule is the full-width divider drawn between the run header and the inset box, so it overhangs
// the box top border by boardMargin on each side.
func (m Model) headerRule() string {
	return borderStyle.Render(strings.Repeat("─", m.boardWidth()))
}

// indentBox left-pads every line of s by n columns — the board content inset that lets the full-width
// header rule overhang the box.
func indentBox(s string, n int) string {
	if n <= 0 {
		return s
	}
	pad := strings.Repeat(" ", n)
	return pad + strings.ReplaceAll(s, "\n", "\n"+pad)
}

// isTerminalLeaf reports whether a leaf status is finished. cached counts as done (it completed in a
// prior run and is being replayed); queued / running / "" (not-yet-started) are all in-progress — so a
// queued placeholder never inflates the done counter into showing a phase complete before any work runs.
func isTerminalLeaf(status string) bool {
	switch status {
	case "done", "failed", "stopped", "cached":
		return true
	}
	return false
}

// phaseAgentCounts / runAgentCounts return (done, total) where done counts only TERMINAL leaves.
func phaseAgentCounts(p runPhaseGroup) (done, total int) {
	for _, j := range p.jobs {
		total++
		if isTerminalLeaf(j.Status) {
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

// runTokens sums each leaf's cumulative OUTPUT tokens across the run — the run header's "↓ tokens".
// Output is the one cross-leaf-additive figure (the generated text accumulates); input is each leaf's
// PEAK context window, so summing it across leaves is dimensionally meaningless — the header shows
// output only, and per-leaf input lives in the agent detail (↑ ctx). Live snapshot while running, the
// final Result once done. Cache-read is excluded throughout.
func (m Model) runTokens(g runGroup) int {
	total := 0
	for _, p := range g.phases {
		for _, j := range p.jobs {
			_, out, _ := m.leafCounts(j)
			total += out
		}
	}
	return total
}

// renderRunHeader is the run drill's header — the board's unified chrome: line 1 = the
// fixed app title (run cwd right-aligned), a blank spacer, line 3 = the run label beside
// the right-aligned "<done>/<total> agents · <elapsed> · ↓ <tokens> tokens" rollup.
func (m Model) renderRunHeader(g runGroup) string {
	done, total := runAgentCounts(g)
	bw := m.boardInner()
	right := fmt.Sprintf("%d/%d agents · %s · ↓ %s tokens", done, total, g.elapsed(), humanTokens(m.runTokens(g)))
	return m.appTitleLine(g.cwd) + "\n\n" + headerSummaryLine(m.runLabel(g), right, bw)
}

// boardBodyHeight is the inner row budget for the master-detail box (drives right-pane scroll). It
// derives from the terminal height, with a default for tests / pre-WindowSizeMsg renders.
func (m Model) boardBodyHeight() int {
	h := m.height
	if h < 12 {
		h = 24
	}
	avail := h - 10 // header (3 lines: name / blank / desc) + rule + box top/bottom + status + footer + margin
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

// paneWidths derives the right pane from a content-sized left: left + right + 11 == boardInner (the 11
// non-content columns are the two outer borders, the divider, and TWO spaces on each side of each cell).
// leftW is CAPPED to always leave ≥20 columns for the detail pane, so the box never overflows its inset
// width (the floor only bites on a sub-45-column terminal, where the box must wrap regardless).
func (m Model) paneWidths(leftW int) (left, right int) {
	avail := m.boardInner() - 11
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

// renderRunRow is one run row: "<dot> <name (short id)>" left; "<done>/<total> agents ·
// <elapsed>[ · <started MM-DD HH:MM>]" right-aligned. selected marks the picker's cursored row.
func (m Model) renderRunRow(g runGroup, width int, selected bool) string {
	marker := ""
	name := trunc(m.runLabel(g), 42)
	switch {
	case selected:
		marker = cursorStyle.Render("❯ ")
		name = selectedStyle.Render(name)
	default:
		name = labelStyle(g.status).Render(name)
	}
	done, total := runAgentCounts(g)
	left := marker + statusDot(g.status) + " " + name
	metrics := fmt.Sprintf("%d/%d agents · %s", done, total, g.elapsed())
	if t, err := time.Parse(time.RFC3339, g.startedAt); err == nil {
		metrics += " · " + t.Format("01-02 15:04")
	}
	return joinRowEnds(left, faintStyle.Render(metrics), width)
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
	leftW, rightW := m.paneWidths(leftWidth("Phases", leftLines, m.boardInner()))
	rightTitle, rightLines := m.phaseAgentLines(rightW)
	bodyH := m.boardBodyHeight()
	leftLines = windowLines(leftLines, m.wfPhaseCursor, bodyH)
	return indentBox(m.renderRunHeader(g), boardMargin) + "\n" + m.headerRule() + "\n" +
		indentBox(renderBoard("Phases", leftLines, rightTitle, rightLines, leftW, rightW, bodyH, 0), boardMargin)
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
	_, rightW := m.paneWidths(leftWidth(title, lines, m.boardInner()))
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
	leftW, rightW := m.paneWidths(leftWidth(listTitle, leftLines, m.boardInner()))
	cardTitle := "agent"
	if j, jok := m.selectedLeaf(); jok {
		if t := trunc(sessiontitle.CleanTitle(j.Label), rightW-6); t != "" {
			cardTitle = t
		}
	}
	rightLines := m.agentDetailLines(rightW)
	bodyH := m.boardBodyHeight()
	leftLines = windowLines(leftLines, m.wfAgentCursor, bodyH)
	return indentBox(m.renderRunHeader(g), boardMargin) + "\n" + m.headerRule() + "\n" +
		indentBox(renderBoard(listTitle, leftLines, cardTitle, rightLines, leftW, rightW, bodyH, m.clampCardScroll(m.wfCardScroll)), boardMargin)
}

// renderAgentRowFull is one agent row for a phase's agent list (right pane): "<dot> <label>  <model>"
// left, "↓ <out> out · <N> tools · <dur>" RIGHT-ALIGNED to width — output tokens only, so the rows sum
// to the header total (input is the leaf's peak context, shown per-leaf in the detail, never summed).
// Live for a RUNNING leaf from its activity snapshot; a done leaf uses its final Result. No answer text.
func (m Model) renderAgentRowFull(j subagent.Result, width int) string {
	_, out, tools := m.leafCounts(j)
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
	metrics := fmt.Sprintf("↓ %s out · %d tools", humanTokens(out), tools)
	if d := leafDuration(j); d != "" {
		metrics += " · " + d
	}
	return joinRowEnds(left, faintStyle.Render(metrics), width)
}

// joinRowEnds right-aligns the metrics beside the label within width: a tight row shrinks
// the label to fit, a pathologically narrow one keeps the metrics alone, truncated.
func joinRowEnds(left, right string, width int) string {
	rw := ansi.StringWidth(right)
	gap := width - ansi.StringWidth(left) - rw
	if gap < 1 {
		if avail := width - rw - 1; avail >= 1 {
			left = boxCell(left, avail)
			gap = 1
		} else {
			return boxCell(right, width)
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

// leafCounts returns the leaf's (input, output) tokens + tool-call count. input is the PEAK context
// window (max over turns, non-cumulative — so it is shown per-leaf, never summed across leaves);
// output is CUMULATIVE generated tokens (additive, what the run header sums); cache-read is excluded.
// Live from the activity snapshot while running, the accurate final from the Result once done; the
// tool count always comes from the snapshot (the final Result doesn't carry it).
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

// agentDetailLines is the focused agent's inline detail (the L2 right pane, scrollable): status/model
// (with an "attempt N" marker when a legacy record carries one) and ↑ ctx · ↓ out · tool-calls, then a fixed
// Prompt → Activity → Output → Outcome order — the Prompt, the Activity
// feed (last 3 tool signatures), the Output (when the io files are loaded for THIS leaf via the PersistIO
// opt-in), and the Outcome. The Output reads from the leaf's .answer side file (focused-single-agent
// surface, CleanTitle-scrubbed), NEVER Result.Result on a row.
func (m Model) agentDetailLines(rightW int) []string {
	j, ok := m.selectedLeaf()
	if !ok {
		return []string{faintStyle.Render("(no agent)")}
	}
	in, out, tools := m.leafCounts(j)
	snap := m.wfActivity[j.JobID]
	status := statusLabel(j.Status) + faintStyle.Render(" · "+trunc(sessiontitle.CleanTitle(j.Model), 28))
	if j.Attempt > 1 {
		// >1 occurs only in records from engines that retried schema mismatches; surface it.
		status += faintStyle.Render(fmt.Sprintf(" · attempt %d", j.Attempt))
	}
	// ↑ peak input context · ↓ cumulative output (the header sums only output across leaves) · cache-write
	// tokens when the leaf wrote the prompt cache · tool calls.
	tokLine := fmt.Sprintf("↑ %s ctx · ↓ %s out", humanTokens(in), humanTokens(out))
	if j.Usage != nil && j.Usage.CacheCreationInputTokens > 0 {
		tokLine += fmt.Sprintf(" · ⊕ %s cache-w", humanTokens(j.Usage.CacheCreationInputTokens))
	}
	tokLine += fmt.Sprintf(" · %d tool calls", tools)
	lines := []string{
		status,
		faintStyle.Render(tokLine),
	}
	// Prompt first.
	switch {
	case m.wfDetailJob.JobID != j.JobID:
		lines = append(lines, "", faintStyle.Render("(loading…)"))
	case !m.wfDetailIO:
		lines = append(lines, "", faintStyle.Render("(prompt/output not persisted — run with default persist-io)"))
	default:
		lines = append(lines, "")
		lines = append(lines, promptSection(m.wfDetailPrompt, m.wfPromptExpanded, rightW)...)
	}
	// Then the Activity feed.
	lines = append(lines, "")
	lines = append(lines, activityLines(snap, rightW)...)
	// Then the Output, when this leaf's io files are loaded.
	if m.wfDetailJob.JobID == j.JobID && m.wfDetailIO {
		lines = append(lines, "", faintStyle.Render("Output"))
		lines = append(lines, ioLines(m.wfDetailAnswer, rightW, liveStyle)...)
	}
	// Outcome last.
	lines = append(lines, "", faintStyle.Render("Outcome"), " "+m.renderOutcome(j))
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

// promptPreviewLines is how many DISPLAY lines a collapsed fold section shows before "… N more".
const promptPreviewLines = 4

// promptSection renders the detail card's Prompt block: the full text when expanded, else a
// collapsed "Prompt · N lines · ⏎ expand" header + the leading preview. The fold counts
// DISPLAY lines (post-wrap), so a single long paragraph folds too; a prompt that already
// fits the preview shows in full with no expand hint. Shared by the Workflows agent card
// and the Agent-status job card so the two read identically.
func promptSection(prompt string, expanded bool, rightW int) []string {
	full := ioLines(prompt, rightW, contentStyle)
	if expanded || len(full) <= promptPreviewLines {
		return append([]string{faintStyle.Render("Prompt")}, full...)
	}
	lines := []string{faintStyle.Render(fmt.Sprintf("Prompt · %d lines · ⏎ expand", len(full)))}
	prev := full[:promptPreviewLines]
	for len(prev) > 0 && strings.TrimSpace(prev[len(prev)-1]) == "" {
		prev = prev[:len(prev)-1] // drop a trailing blank preview row so "… N more lines" doesn't gap
	}
	lines = append(lines, prev...)
	lines = append(lines, " "+faintStyle.Render(fmt.Sprintf("… %d more lines", len(full)-len(prev)))) // body-aligned (indent 1)
	return lines
}

// activityLines renders a detail card's Activity section: the tool-call header + the last
// 3 signatures ("(no tool calls)" when none). Shared by all three detail cards.
func activityLines(snap activitySnapshot, rightW int) []string {
	lines := []string{faintStyle.Render(fmt.Sprintf("Activity · last 3 of %d tool calls", snap.toolCount()))}
	sigs := snap.lastSigs(3)
	if len(sigs) == 0 {
		lines = append(lines, faintStyle.Render(" (no tool calls)"))
	}
	for _, s := range sigs {
		lines = append(lines, " "+contentStyle.Render(truncCols(sessiontitle.CleanTitle(s), rightW-2)))
	}
	return lines
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

// prettyDir abbreviates $HOME to ~ for a compact project-dir display.
func prettyDir(dir string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if dir == home {
			return "~"
		}
		if strings.HasPrefix(dir, home+string(os.PathSeparator)) {
			return "~" + dir[len(home):]
		}
	}
	return dir
}

// leftTruncCols keeps the TRAILING w display columns of s (a path's tail is the useful part), prefixing
// "…" when it cut. CJK-aware via ansi.StringWidth.
func leftTruncCols(s string, w int) string {
	if w < 1 {
		w = 1
	}
	if ansi.StringWidth(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && ansi.StringWidth(string(r)) > w-1 {
		r = r[1:]
	}
	return "…" + string(r)
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

// renderOutcome is the key-safe outcome line: status + a canonical summary, NEVER Result.Result.
// Done shows turns, stopped is neutral, a failure shows its error class, queued/running stay in progress.
func (m Model) renderOutcome(j subagent.Result) string {
	switch {
	case j.Status == "running" || j.Status == "queued" || j.Status == "":
		return faintStyle.Render("Still running…") // queued has OK=true but isn't done — keep it in-progress
	case j.OK || j.Status == "done":
		return faintStyle.Render(fmt.Sprintf("done · %d turns", j.NumTurns))
	case j.Status == "stopped":
		return faintStyle.Render("stopped") // a `workflow stop` reap — neutral, not a failure
	default:
		cls := j.ErrorCode
		if cls == "" {
			cls = "failed"
		}
		return errStyle.Render(sessiontitle.CleanTitle(cls))
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

// headerSummaryLine is the chrome's third line: the CURSORED child's label on the left (the
// live preview context, the slot the wf run header gives its description) + the container
// rollup right-aligned. The left side truncates so the line never wraps and shifts the
// fixed-height boxes down.
func headerSummaryLine(left, right string, bw int) string {
	r := liveStyle.Render(right)
	if left == "" {
		gap := bw - lipgloss.Width(r)
		if gap < 0 {
			gap = 0
		}
		return strings.Repeat(" ", gap) + r
	}
	l := liveStyle.Render(trunc(left, 80))
	gap := bw - lipgloss.Width(l) - lipgloss.Width(r)
	if gap < 1 {
		l = boxCell(l, bw-lipgloss.Width(r)-1)
		gap = 1
	}
	return l + strings.Repeat(" ", gap) + r
}

// projectCounts is one project's "N sessions[ · K workflows][ · T teammates][ · J subagents]" rollup.
func projectCounts(p asProject) string {
	mates, jobs, runs := 0, 0, 0
	for _, sess := range p.sessions {
		for _, t := range sess.teams {
			for _, mem := range t.members {
				if mem.Status != endedStatus { // ended members aren't live teammates
					mates++
				}
			}
		}
		jobs += len(sess.jobs)
		runs += len(sess.runs)
	}
	out := fmt.Sprintf("%d sessions", len(p.sessions))
	if runs > 0 {
		out += fmt.Sprintf(" · %d workflows", runs)
	}
	if mates > 0 {
		out += fmt.Sprintf(" · %d teammates", mates)
	}
	if jobs > 0 {
		out += fmt.Sprintf(" · %d subagents", jobs)
	}
	return out
}

// renderAppHeader is the L0 header — the same THREE-line + rule chrome every deeper level
// uses, so changing levels never reshapes the screen: the app title + tab hint on line 1, a
// blank spacer, then the cursored project beside ITS rollup (the project total is the
// rail's own row count).
func (m Model) renderAppHeader() string {
	bw := m.boardInner()
	projects := m.asProjects()
	left, right := "", ""
	if m.asProjectCursor < len(projects) {
		p := projects[m.asProjectCursor]
		left = sessiontitle.CleanTitle(projectLabel(p.dir))
		right = projectCounts(p)
	}
	return m.spawnTitle() + "\n\n" + headerSummaryLine(left, right, bw)
}

// viewAsProjects is L0 (>1 project): the app header above one box — the project rail | the
// cursored project's session rows (title, separated teammate/subagent counts, created time).
func (m Model) viewAsProjects() string {
	projects := m.asProjects()
	var leftLines []string
	for i, p := range projects {
		marker := "  "
		label := leftTruncCols(sessiontitle.CleanTitle(projectLabel(p.dir)), 28)
		if i == m.asProjectCursor {
			marker = cursorStyle.Render("❯ ")
			label = selectedStyle.Render(label)
		} else {
			label = liveStyle.Render(label)
		}
		leftLines = append(leftLines, fmt.Sprintf("%s%s  %s", marker, label,
			faintStyle.Render(fmt.Sprintf("%d", len(p.sessions)))))
	}
	leftW, rightW := m.paneWidths(leftWidth("Projects", leftLines, m.boardInner()))
	rightTitle := "sessions"
	rightLines := []string{faintStyle.Render("(none)")}
	if m.asProjectCursor < len(projects) {
		p := projects[m.asProjectCursor]
		rightTitle = fmt.Sprintf("%s · %d sessions", leftTruncCols(sessiontitle.CleanTitle(projectLabel(p.dir)), 24), len(p.sessions))
		rightLines = nil
		for _, s := range p.sessions {
			rightLines = append(rightLines, m.renderSessionRow(s, rightW))
		}
	}
	bodyH := m.boardBodyHeight()
	leftLines = windowLines(leftLines, m.asProjectCursor, bodyH)
	return indentBox(m.renderAppHeader(), boardMargin) + "\n" + m.headerRule() + "\n" +
		indentBox(renderBoard("Projects", leftLines, rightTitle, rightLines, leftW, rightW, bodyH, 0), boardMargin)
}

// appTitleLine is the chrome's FIRST line at every Agent-status level: the fixed app title
// + tab hint, with the level's directory (when it has one) right-aligned in faint — so the
// top anchor never changes as the user drills.
func (m Model) appTitleLine(dir string) string {
	left := m.spawnTitle()
	if dir == "" {
		return left
	}
	bw := m.boardInner()
	d := faintStyle.Render(leftTruncCols(sessiontitle.CleanTitle(prettyDir(dir)), bw/2))
	gap := bw - lipgloss.Width(left) - lipgloss.Width(d)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + d
}

// renderProjectHeader is the L1 header — the fixed app title line over the CONTAINER's
// label + rollup. The cursored session names only the right pane (the preview).
func (m Model) renderProjectHeader(p asProject) string {
	bw := m.boardInner()
	line1 := m.appTitleLine(p.dir)
	// The left slot stays FIXED on the container label (the cursored session already
	// shows in the rail highlight + the right pane title; a moving header reads jumpy).
	return line1 + "\n\n" + headerSummaryLine(sessiontitle.CleanTitle(projectLabel(p.dir)), projectCounts(p), bw)
}

// viewAsSessions is L1 (>1 session in the focused project): the PROJECT's header (the
// container the user descended into) above one box — the session rail | the cursored
// session's row preview.
func (m Model) viewAsSessions() string {
	p, ok := m.focusedProjectGroup()
	if !ok {
		return m.spawnTitle() + "\n\n" + faintStyle.Render("(no live agents)")
	}
	var leftLines []string
	for i, s := range p.sessions {
		marker := "  "
		label := trunc(m.sessionLabel(s.sessionID), 40)
		if i == m.asSessionCursor {
			marker = cursorStyle.Render("❯ ")
			label = selectedStyle.Render(label)
		} else {
			label = liveStyle.Render(label)
		}
		leftLines = append(leftLines, marker+sessionHdrStyle.Render("◆ ")+label)
	}
	leftW, rightW := m.paneWidths(leftWidth("Sessions", leftLines, m.boardInner()))
	rightTitle := "overview"
	if m.asSessionCursor < len(p.sessions) {
		rightTitle = trunc(m.sessionLabel(p.sessions[m.asSessionCursor].sessionID), rightW-6)
	}
	rightLines := m.sessionOverviewLines(p, rightW)
	bodyH := m.boardBodyHeight()
	leftLines = windowLines(leftLines, m.asSessionCursor, bodyH)
	return indentBox(m.renderProjectHeader(p), boardMargin) + "\n" + m.headerRule() + "\n" +
		indentBox(renderBoard("Sessions", leftLines, rightTitle, rightLines, leftW, rightW, bodyH, 0), boardMargin)
}

// sessionOverviewLines is the L1 right pane: the cursored session's ACTUAL rows under
// separate Agent Teams / Subagents sections — what ⏎ will open, previewed in place
// (renderBoard clips overflow to the box height; created/project live in the header).
func (m Model) sessionOverviewLines(p asProject, rightW int) []string {
	if m.asSessionCursor >= len(p.sessions) {
		return []string{faintStyle.Render("(none)")}
	}
	s := p.sessions[m.asSessionCursor]
	var lines []string
	if len(s.runs) > 0 {
		lines = append(lines, "", faintStyle.Render(fmt.Sprintf("Dynamic Workflows · %d", len(s.runs))))
		for _, g := range s.runs {
			lines = append(lines, " "+m.renderRunRow(g, rightW-1, false))
		}
	}
	if len(s.teams) > 0 {
		lines = append(lines, "", faintStyle.Render("Agent Teams"))
		for _, t := range s.teams {
			okN := 0
			for _, mem := range t.members {
				if mem.Status == "ok" {
					okN++
				}
			}
			lines = append(lines, " "+teamAggregateDot(t.members)+" "+
				contentStyle.Render(trunc(sessiontitle.CleanTitle(displayTeam(t.name)), 24))+
				faintStyle.Render(fmt.Sprintf("  %d/%d", okN, len(t.members))))
			for _, mem := range t.members {
				lines = append(lines, "   "+m.renderTeammateRowFull(mem, rightW-3))
			}
		}
	}
	if len(s.jobs) > 0 {
		lines = append(lines, "", faintStyle.Render(fmt.Sprintf("Subagents · %d", len(s.jobs))))
		for _, j := range s.jobs {
			lines = append(lines, " "+m.renderJobRowFull(j, rightW-1))
		}
	}
	if len(lines) > 0 && lines[0] == "" {
		lines = lines[1:] // the first section needs no leading spacer
	}
	return lines
}

// renderSessionRow is one session row in the L0 right pane: "◆ <title (short id)>" left,
// the separated counts + created time right-aligned.
func (m Model) renderSessionRow(s asSession, width int) string {
	left := sessionHdrStyle.Render("◆ ") + liveStyle.Render(trunc(m.sessionLabel(s.sessionID), 44))
	right := asSessionCounts(s)
	if c := asCreated(s); c != "" {
		right += " · " + c
	}
	return joinRowEnds(left, faintStyle.Render(right), width)
}

// asSessionCounts is a session's rollup with the two kinds separated: "N teammates
// [(K hidden)] · M subagents"; "empty" when it has neither.
func asSessionCounts(s asSession) string {
	mates, hidden := 0, 0
	for _, t := range s.teams {
		for _, mem := range t.members {
			if mem.Status == endedStatus {
				continue // ended members don't inflate the live "N teammates" rollup
			}
			mates++
			if mem.Hidden {
				hidden++
			}
		}
	}
	var parts []string
	if len(s.runs) > 0 {
		parts = append(parts, fmt.Sprintf("%d workflows", len(s.runs)))
	}
	if mates > 0 {
		seg := fmt.Sprintf("%d teammates", mates)
		if hidden > 0 {
			seg += fmt.Sprintf(" (%d hidden)", hidden)
		}
		parts = append(parts, seg)
	}
	if len(s.jobs) > 0 {
		parts = append(parts, fmt.Sprintf("%d subagents", len(s.jobs)))
	}
	if len(parts) == 0 {
		return "empty"
	}
	return strings.Join(parts, " · ")
}

// asCreated is the session's first-activity timestamp ("" when unrecorded).
func asCreated(s asSession) string {
	if !s.hasTime {
		return ""
	}
	return s.earliest.Format("01-02 15:04")
}

// projectLabel renders a project dir as its LAST TWO path segments (a deep absolute path
// doesn't fit a rail and the tail is the identifying part); "(no project)" when unresolved.
func projectLabel(dir string) string {
	if dir == "" {
		return "(no project)"
	}
	segs := strings.Split(strings.Trim(dir, "/"), "/")
	if n := len(segs); n > 2 {
		segs = segs[n-2:]
	}
	return strings.Join(segs, "/")
}

// renderSessionHeader is the persistent header above the L2/L3 boxes, THREE lines: the
// FIXED app title (the session's dir right-aligned); a blank spacer; then the session title
// beside the right slot — the CURSORED agent's own stats when the cursor sits on a single
// job/teammate (its tokens/elapsed live where the eye already is), else the session's
// counts + created rollup. Width-bounded so a long title can't wrap and shift the
// fixed-height boxes down.
func (m Model) renderSessionHeader(s asSession) string {
	bw := m.boardInner()
	line1 := m.appTitleLine(m.sessionMeta[s.sessionID].Cwd)
	right := m.headerEntityStats()
	if right == "" {
		right = asSessionCounts(s)
		if c := asCreated(s); c != "" {
			right += " · created " + c
		}
	}
	return line1 + "\n\n" + headerSummaryLine(m.sessionLabel(s.sessionID), right, bw)
}

// headerEntityStats is the cursored agent's own stat summary for the session header: a
// job's tokens + elapsed + started time, a teammate's uptime + spawn time. "" when the
// cursored row isn't a single agent (an L2 team row, an empty collection) — the header
// falls back to the session rollup.
func (m Model) headerEntityStats() string {
	switch m.asMode {
	case asModeEntity:
		if t, ok := m.selectedTeammate(); ok {
			return m.teammateStats(t)
		}
		if j, ok := m.selectedJob(); ok {
			return m.jobStats(j)
		}
	case asModeBoxes:
		if s, ok := m.focusedSession(); ok {
			if i := m.asBoxCursor - len(s.teams); i >= 0 && i < len(s.jobs) {
				return m.jobStats(s.jobs[i])
			}
		}
	}
	return ""
}

// jobStats is one job's header summary: "↑ <ctx> ctx · ↓ <out> out · <elapsed> · <started>"
// with the token segment omitted until usage exists (a running unfocused job reports none).
func (m Model) jobStats(j subagent.Result) string {
	var parts []string
	if in, out := m.jobTokens(j); in > 0 || out > 0 {
		parts = append(parts, fmt.Sprintf("↑ %s ctx · ↓ %s out", humanTokens(in), humanTokens(out)))
	}
	if el := jobElapsed(j); el != "" {
		parts = append(parts, el)
	}
	if ts, err := time.Parse(time.RFC3339, j.StartedAt); err == nil {
		parts = append(parts, ts.Format("01-02 15:04"))
	}
	return strings.Join(parts, " · ")
}

// teammateStats is one teammate's header summary: its transcript token aggregates (once
// the focused payload is loaded), uptime, and spawn time.
func (m Model) teammateStats(t teardown.Teammate) string {
	var parts []string
	if s := m.asMateSnap; m.asMateKey == mateKey(t) && (s.ctxTok > 0 || s.outTok > 0) {
		parts = append(parts, fmt.Sprintf("↑ %s ctx · ↓ %s out", humanTokens(s.ctxTok), humanTokens(s.outTok)))
	}
	if up := teammateUptime(t); up != "" {
		parts = append(parts, "up "+up)
	}
	if t.SpawnTime > 0 {
		parts = append(parts, time.UnixMilli(t.SpawnTime).Format("01-02 15:04"))
	}
	return strings.Join(parts, " · ")
}

// jobTokens is the job's (peak-context, cumulative-output) token pair: the final Result
// usage, overridden by the live activity snapshot while the focused job still runs.
func (m Model) jobTokens(j subagent.Result) (in, out int) {
	if j.Usage != nil {
		in, out = j.Usage.InputTokens, j.Usage.OutputTokens
	}
	if j.Status == "running" && m.asDetailJobID == j.JobID && m.asDetailSnap.hasUsage {
		in, out = m.asDetailSnap.inTok, m.asDetailSnap.outTok
	}
	return in, out
}

// viewAsBoxes is L2: the session header above the two stacked boxes — Agent Teams
// (master-detail: team rail | the cursored team's member rows) and Subagents. One ↑/↓
// cursor walks both (see updateAsBoxes). With the cursor on a JOB row the Subagents box
// is itself a master-detail (job rail | the job's inline card — no descend needed) and
// takes the height; on a team row it stays the flat row list. A session missing one kind
// simply omits that box.
func (m Model) viewAsBoxes() string {
	s, ok := m.focusedSession()
	if !ok {
		return m.spawnTitle() + "\n\n" +
			faintStyle.Render("(no live agents — none spawned, and no subagent jobs)")
	}
	header := indentBox(m.renderSessionHeader(s), boardMargin) + "\n" + m.headerRule() + "\n"
	runsBody, teamsBody, jobsBody := m.splitBoxHeights(s)
	var parts []string
	if len(s.runs) > 0 {
		parts = append(parts, indentBox(m.renderRunsBox(s, runsBody), boardMargin))
	}
	if len(s.teams) > 0 {
		parts = append(parts, indentBox(m.renderTeamsBox(s, teamsBody), boardMargin))
	}
	if len(s.jobs) > 0 {
		if _, onJob := m.boxJob(); onJob {
			parts = append(parts, indentBox(m.renderJobsBoxDetail(s, jobsBody), boardMargin))
		} else {
			parts = append(parts, indentBox(m.renderJobsBox(s, jobsBody), boardMargin))
		}
	}
	return header + strings.Join(parts, "\n")
}

// splitBoxHeights divides the body budget across the session's boxes: the Dynamic
// Workflows box gets its content up to a quarter, the Agent Teams box its content up to
// half of the rest — only up to a quarter while the cursor sits on a job row, whose inline
// card then takes the remainder; a missing kind hands its share down. Each extra box costs
// its two border rows.
func (m Model) splitBoxHeights(s asSession) (runs, teams, jobs int) {
	avail := m.boardBodyHeight()
	boxes := 0
	for _, n := range []int{len(s.runs), len(s.teams), len(s.jobs)} {
		if n > 0 {
			boxes++
		}
	}
	if boxes > 1 {
		avail -= 2 * (boxes - 1)
	}
	if avail < 4 {
		avail = 4
	}
	if len(s.runs) > 0 {
		runs = len(s.runs)
		if cap := avail / 4; runs > cap {
			runs = cap
		}
		if runs < 1 {
			runs = 1
		}
		avail -= runs
	}
	if len(s.teams) == 0 {
		return runs, 0, avail
	}
	if len(s.jobs) == 0 {
		return runs, avail, 0
	}
	need := len(s.teams)
	if ti := m.boxTeamIdx(s); ti >= 0 && len(s.teams[ti].members) > need {
		need = len(s.teams[ti].members)
	}
	teams = need
	cap := avail / 2
	if _, onJob := m.boxJob(); onJob {
		cap = avail / 4
	}
	if teams > cap {
		teams = cap
	}
	if teams < 2 {
		teams = 2
	}
	return runs, teams, avail - teams
}

// boxTeamIdx is the team the Agent Teams box previews: the cursored team while the L2
// cursor sits in the teams range, the first team while it is above (the runs box), else
// the last team (the cursor moved into the Subagents box).
func (m Model) boxTeamIdx(s asSession) int {
	if len(s.teams) == 0 {
		return -1
	}
	i := m.asBoxCursor - len(s.runs)
	switch {
	case i < 0:
		return 0
	case i < len(s.teams):
		return i
	}
	return len(s.teams) - 1
}

// renderTeamsBox is the Agent Teams master-detail box: team rail | the previewed team's
// member rows.
func (m Model) renderTeamsBox(s asSession, bodyH int) string {
	var leftLines []string
	for i, t := range s.teams {
		marker := "  "
		okN := 0
		for _, mem := range t.members {
			if mem.Status == "ok" {
				okN++
			}
		}
		title := trunc(sessiontitle.CleanTitle(displayTeam(t.name)), 20)
		if i == m.asBoxCursor-len(s.runs) {
			marker = cursorStyle.Render("❯ ")
			title = selectedStyle.Render(title)
		} else {
			title = liveStyle.Render(title)
		}
		// An ended team reads "ended · N members" — never an okN/N that would mimic
		// an all-unhealthy live team.
		count := fmt.Sprintf("%d/%d", okN, len(t.members))
		if m.isEndedTeam(t.name) {
			count = fmt.Sprintf("%s · %d members", endedStatus, len(t.members))
		}
		leftLines = append(leftLines, fmt.Sprintf("%s%s %s  %s", marker, teamAggregateDot(t.members), title,
			faintStyle.Render(count)))
	}
	leftW, rightW := m.paneWidths(leftWidth("Agent Teams", leftLines, m.boardInner()))
	rightTitle := "teammates"
	var rightLines []string
	if ti := m.boxTeamIdx(s); ti >= 0 {
		t := s.teams[ti]
		rightTitle = fmt.Sprintf("%s · %d teammates",
			trunc(sessiontitle.CleanTitle(displayTeam(t.name)), 20), len(t.members))
		for _, mem := range t.members {
			rightLines = append(rightLines, m.renderTeammateRowFull(mem, rightW))
		}
	}
	leftLines = windowLines(leftLines, m.asBoxCursor, bodyH)
	return renderBoard("Agent Teams", leftLines, rightTitle, rightLines, leftW, rightW, bodyH, 0)
}

// renderJobsBox is the Subagents box: the session's job rows, full width, the continuum
// cursor marking the active row.
func (m Model) renderJobsBox(s asSession, bodyH int) string {
	innerW := m.boardInner() - 6
	cursor := m.asBoxCursor - len(s.runs) - len(s.teams)
	var lines []string
	for i, j := range s.jobs {
		marker := "  "
		if i == cursor {
			marker = cursorStyle.Render("❯ ")
		}
		lines = append(lines, marker+m.renderJobRowFull(j, innerW-2))
	}
	if cursor < 0 {
		cursor = 0
	}
	return renderBox(fmt.Sprintf("Subagents · %d", len(s.jobs)), windowLines(lines, cursor, bodyH), innerW, bodyH)
}

// jobsBoxLeftLines is the Subagents box's compact job rail (dot + short id), cursor-marked
// when the L2 continuum sits in the jobs range — shared by renderJobsBoxDetail and
// jobsBoxRightWidth so the scroll clamp and the render agree on the card width.
func (m Model) jobsBoxLeftLines(s asSession) []string {
	cursor := m.asBoxCursor - len(s.runs) - len(s.teams)
	var lines []string
	for i, j := range s.jobs {
		marker := "  "
		label := shortJobID(sessiontitle.CleanTitle(j.JobID))
		if i == cursor {
			marker = cursorStyle.Render("❯ ")
			label = selectedStyle.Render(label)
		} else {
			label = labelStyle(j.Status).Render(label)
		}
		lines = append(lines, marker+statusDot(j.Status)+" "+label)
	}
	return lines
}

// jobsBoxRightWidth is the inline job card's column budget in the Subagents box.
func (m Model) jobsBoxRightWidth(s asSession) int {
	title := fmt.Sprintf("Subagents · %d", len(s.jobs))
	_, rightW := m.paneWidths(leftWidth(title, m.jobsBoxLeftLines(s), m.boardInner()))
	return rightW
}

// renderJobsBoxDetail is the Subagents box with the cursor on a job row: a master-detail
// of the job rail | that job's inline card (the same card the entity level shows), so a
// job's detail is one cursor move away — never a descend.
func (m Model) renderJobsBoxDetail(s asSession, bodyH int) string {
	listTitle := fmt.Sprintf("Subagents · %d", len(s.jobs))
	leftLines := m.jobsBoxLeftLines(s)
	leftW, rightW := m.paneWidths(leftWidth(listTitle, leftLines, m.boardInner()))
	cardTitle := "job"
	var rightLines []string
	if j, ok := m.boxJob(); ok {
		cardTitle = shortJobID(sessiontitle.CleanTitle(j.JobID))
		rightLines = m.jobDetailLines(j, rightW)
	}
	cursor := m.asBoxCursor - len(s.runs) - len(s.teams)
	leftLines = windowLines(leftLines, cursor, bodyH)
	return renderBoard(listTitle, leftLines, cardTitle, rightLines, leftW, rightW, bodyH, m.clampAsCardScroll(m.asCardScroll))
}

// renderRunsBox is the Dynamic Workflows box: the session's run rows (newest first), the
// continuum cursor marking the active row; ⏎ opens the run's Phases level.
func (m Model) renderRunsBox(s asSession, bodyH int) string {
	innerW := m.boardInner() - 6
	var lines []string
	for i, g := range s.runs {
		lines = append(lines, m.renderRunRow(g, innerW, i == m.asBoxCursor))
	}
	cursor := m.asBoxCursor
	if cursor >= len(s.runs) {
		cursor = len(s.runs) - 1
	}
	return renderBox(fmt.Sprintf("Dynamic Workflows · %d", len(s.runs)), windowLines(lines, cursor, bodyH), innerW, bodyH)
}

// renderBox draws a full-width single-pane rounded box with a title segment — the
// one-column sibling of renderBoard.
func renderBox(title string, lines []string, innerW, bodyH int) string {
	var b strings.Builder
	head := "  " + title + " "
	fill := innerW + 4 - ansi.StringWidth(head)
	if fill < 0 {
		fill = 0
	}
	b.WriteString(borderStyle.Render("╭"+head+strings.Repeat("─", fill)+"╮") + "\n")
	bar := borderStyle.Render("│")
	for i := 0; i < bodyH; i++ {
		l := ""
		if i < len(lines) {
			l = lines[i]
		}
		b.WriteString(bar + "  " + boxCell(l, innerW) + "  " + bar + "\n")
	}
	b.WriteString(borderStyle.Render("╰" + strings.Repeat("─", innerW+4) + "╯"))
	return b.String()
}

// displayTeam renders a team name, "(no team)" when empty.
func displayTeam(team string) string {
	if team == "" {
		return "(no team)"
	}
	return team
}

// teammateDot maps teammate health to the board glyph: ok ● (green), error ● (err),
// unknown/unscanned ◌ (faint).
func teammateDot(t teardown.Teammate) string {
	switch t.Status {
	case "ok":
		return okStyle.Render("●")
	case "error":
		return errStyle.Render("●")
	case endedStatus:
		return faintStyle.Render("■")
	default:
		return faintStyle.Render("◌")
	}
}

// teammateStatusWord is the row's canonical health word — the classified error class when
// present (rate_limit / insufficient_balance / auth / api_error, the `ps --check`
// vocabulary), else the scan status.
func teammateStatusWord(t teardown.Teammate) string {
	if t.ErrorClass != "" {
		return t.ErrorClass
	}
	if t.Status == "" {
		return "unknown"
	}
	return t.Status
}

// teammateUptime renders how long the teammate has been up, from its JoinedAt-derived
// SpawnTime (unix ms); "" when unrecorded.
func teammateUptime(t teardown.Teammate) string {
	if t.SpawnTime <= 0 {
		return ""
	}
	d := time.Since(time.UnixMilli(t.SpawnTime))
	switch {
	case d < 0:
		return ""
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// teamAggregateDot rolls a team's health up to the rail: any error → ● (err), all ok → ●
// (green), else ◌ (faint — unscanned/unknown somewhere).
func teamAggregateDot(members []teardown.Teammate) string {
	okN, endedN := 0, 0
	for _, t := range members {
		switch t.Status {
		case "error":
			return errStyle.Render("●")
		case "ok":
			okN++
		case endedStatus:
			endedN++
		}
	}
	if len(members) > 0 && endedN == len(members) {
		return faintStyle.Render("■") // a fully-ended team reads as gone, not unknown
	}
	if len(members) > 0 && okN == len(members) {
		return okStyle.Render("●")
	}
	return faintStyle.Render("◌")
}

// jobsAggregateDot rolls the session's jobs up to the rail — a failure is never masked:
// any failed → ● (err), else any running → ● (accent), else all done → ● (green), else ◌.
func jobsAggregateDot(jobs []subagent.Result) string {
	running, done := false, 0
	for _, j := range jobs {
		switch j.Status {
		case "failed":
			return errStyle.Render("●")
		case "running":
			running = true
		case "done":
			done++
		}
	}
	switch {
	case running:
		return cursorStyle.Render("●")
	case len(jobs) > 0 && done == len(jobs):
		return okStyle.Render("●")
	default:
		return faintStyle.Render("◌")
	}
}

// renderTeammateRowFull is one teammate row (right pane): "<dot> <name>  <model>" left,
// "<status>[ · hidden] · up <uptime>" right-aligned. The status word stays canonical even on
// a hidden row (every row carries its `ps --check` health); hidden adds the suffix and the
// whole row renders faint so it visibly recedes.
func (m Model) renderTeammateRowFull(t teardown.Teammate, width int) string {
	name := sessiontitle.CleanTitle(t.Name)
	if name == "" {
		name = "teammate"
	}
	nameStyle := liveStyle
	if t.Hidden || t.Status == endedStatus {
		nameStyle = faintStyle // an ended (gone) row recedes like a hidden one
	}
	left := teammateDot(t) + " " + nameStyle.Render(trunc(name, 24))
	if model := sessiontitle.CleanTitle(t.Model); model != "" {
		left += "  " + faintStyle.Render(trunc(model, 22))
	}
	status := teammateStatusWord(t)
	if t.Hidden {
		status += " · hidden"
	}
	// An ended row has no live pane — no uptime.
	if up := teammateUptime(t); up != "" && t.Status != endedStatus {
		status += " · up " + up
	}
	return joinRowEnds(left, faintStyle.Render(status), width)
}

// renderJobRowFull is one subagent-job row (right pane): "<dot> <jobID>  <model>" left,
// "<status> · <elapsed>" right-aligned — status columns only, NEVER answer text.
func (m Model) renderJobRowFull(j subagent.Result, width int) string {
	left := statusDot(j.Status) + " " +
		labelStyle(j.Status).Render(shortJobID(sessiontitle.CleanTitle(j.JobID)))
	if model := sessiontitle.CleanTitle(j.Model); model != "" {
		left += "  " + faintStyle.Render(trunc(model, 22))
	}
	metrics := j.Status
	if metrics == "" {
		metrics = "unknown"
	}
	if el := jobElapsed(j); el != "" {
		metrics += " · " + el
	}
	return joinRowEnds(left, faintStyle.Render(metrics), width)
}

// jobElapsed is the job's wall-clock: the recorded duration once terminal, a live
// since-StartedAt while in progress; "" when unparseable — and never a live, growing figure
// for a terminal job whose record carries no duration.
func jobElapsed(j subagent.Result) string {
	if d := leafDuration(j); d != "" {
		return d
	}
	if isTerminalLeaf(j.Status) {
		return ""
	}
	start, err := time.Parse(time.RFC3339, j.StartedAt)
	if err != nil {
		return ""
	}
	d := time.Since(start)
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm %ds", int(d/time.Minute), int(d.Seconds())%60)
}

// asEntityLeftLines builds the COMPACT entity list for the L3 left rail (dot + name only),
// plus its title — shared by viewAsEntity and asEntityRightWidth so the scroll clamp and the
// render agree on the right pane width (mirror agentLeftLines).
func (m Model) asEntityLeftLines() (title string, lines []string) {
	members, jobs := m.railEntities()
	if m.asEntitySrc.jobs {
		title = fmt.Sprintf("Subagents · %d", len(jobs))
		for i, j := range jobs {
			marker := "  "
			label := shortJobID(sessiontitle.CleanTitle(j.JobID))
			if i == m.asEntityCursor {
				marker = cursorStyle.Render("❯ ")
				label = selectedStyle.Render(label)
			} else {
				label = labelStyle(j.Status).Render(label)
			}
			lines = append(lines, marker+statusDot(j.Status)+" "+label)
		}
		return title, lines
	}
	title = fmt.Sprintf("%s · %d teammates",
		trunc(sessiontitle.CleanTitle(displayTeam(m.asEntitySrc.team)), 20), len(members))
	for i, t := range members {
		marker := "  "
		label := sessiontitle.CleanTitle(t.Name)
		if label == "" {
			label = "teammate"
		}
		label = trunc(label, 24)
		switch {
		case i == m.asEntityCursor:
			marker = cursorStyle.Render("❯ ")
			label = selectedStyle.Render(label)
		case t.Hidden:
			label = faintStyle.Render(label)
		default:
			label = liveStyle.Render(label)
		}
		lines = append(lines, marker+teammateDot(t)+" "+label)
	}
	return title, lines
}

// asEntityRightWidth is the L3 right pane width (mirror wfAgentRightWidth) so the scroll
// clamp wraps the detail to the same column budget the render uses.
func (m Model) asEntityRightWidth() int {
	title, lines := m.asEntityLeftLines()
	_, rightW := m.paneWidths(leftWidth(title, lines, m.boardInner()))
	return rightW
}

// viewAsEntity is L3: the session header above one box — "entity list | the focused
// entity's inline detail card" (the right pane scrolls with j/k via asCardScroll).
func (m Model) viewAsEntity() string {
	s, ok := m.focusedSession()
	if !ok {
		return m.spawnTitle() + "\n\n" + faintStyle.Render("(no live agents)")
	}
	listTitle, leftLines := m.asEntityLeftLines()
	leftW, rightW := m.paneWidths(leftWidth(listTitle, leftLines, m.boardInner()))
	cardTitle := "detail"
	if t, tok := m.selectedTeammate(); tok {
		if n := trunc(sessiontitle.CleanTitle(t.Name), rightW-6); n != "" {
			cardTitle = n
		}
	} else if j, jok := m.selectedJob(); jok {
		cardTitle = shortJobID(sessiontitle.CleanTitle(j.JobID))
	}
	rightLines := m.entityDetailLines(rightW)
	bodyH := m.asEntityBodyHeight()
	leftLines = windowLines(leftLines, m.asEntityCursor, bodyH)
	board := indentBox(renderBoard(listTitle, leftLines, cardTitle, rightLines, leftW, rightW, bodyH, m.clampAsCardScroll(m.asCardScroll)), boardMargin)
	// A single-kind session lands here directly (the boxes level is skipped): keep the
	// two-box silhouette with a slim placeholder for the missing collection.
	if _, single := singleKindSrc(s); single {
		innerW := m.boardInner() - 6
		if m.asEntitySrc.jobs {
			ph := renderBox("Agent Teams", []string{faintStyle.Render("(none in this session)")}, innerW, 1)
			board = indentBox(ph, boardMargin) + "\n" + board
		} else {
			ph := renderBox("Subagents · 0", []string{faintStyle.Render("(none in this session)")}, innerW, 1)
			board = board + "\n" + indentBox(ph, boardMargin)
		}
	}
	return indentBox(m.renderSessionHeader(s), boardMargin) + "\n" + m.headerRule() + "\n" + board
}

// asEntityBodyHeight is the entity box's row budget: the full body, minus the slim
// placeholder box a single-kind session shows for its missing collection (two borders +
// one row).
func (m Model) asEntityBodyHeight() int {
	if s, ok := m.focusedSession(); ok {
		if _, single := singleKindSrc(s); single {
			h := m.boardBodyHeight() - 3
			if h < 5 {
				h = 5
			}
			return h
		}
	}
	return m.boardBodyHeight()
}

// entityDetailLines is the focused entity's inline detail card (the L2 right pane): the
// teammate card mirrors `ps --check` (canonical fields only, no raw pane text); the job card
// renders from the already-loaded Result — usage/cost/turns appear once terminal, and a
// failure shows the canonical ErrorCode ONLY (ErrorMsg never renders on this board).
func (m Model) entityDetailLines(rightW int) []string {
	if t, ok := m.selectedTeammate(); ok {
		return m.teammateDetailLines(t, rightW)
	}
	if j, ok := m.selectedJob(); ok {
		return m.jobDetailLines(j, rightW)
	}
	return []string{faintStyle.Render("(no row)")}
}

// detailField renders one "key  value" card line set, wrapping a long value to the pane
// (continuation lines indent under the value column) — the card never truncates a field.
func detailField(lines *[]string, k, v string, rightW int) {
	if v == "" {
		v = "—"
	}
	for fi, w := range wrapTo(v, rightW-12) {
		key := fmt.Sprintf("%-8s", k)
		if fi > 0 {
			key = strings.Repeat(" ", 8)
		}
		*lines = append(*lines, " "+faintStyle.Render(key)+"  "+contentStyle.Render(w))
	}
}

// detailKV is one Overview field, paired two-per-line by detailFieldPairs.
type detailKV struct{ k, v string }

// detailFieldPairs renders Overview fields two per line (halving the card's field-block
// height); each value truncates to its half-cell — long free-text fields stay on
// detailField's full-width wrapping lines instead. An odd tail leaves the right cell empty.
func detailFieldPairs(lines *[]string, fields []detailKV, rightW int) {
	half := (rightW - 1) / 2
	cell := func(f detailKV, w int) string {
		if f.k == "" {
			return strings.Repeat(" ", w)
		}
		v := f.v
		if v == "" {
			v = "—"
		}
		return boxCell(" "+faintStyle.Render(fmt.Sprintf("%-8s", f.k))+"  "+contentStyle.Render(truncCols(v, w-12)), w)
	}
	for i := 0; i < len(fields); i += 2 {
		right := detailKV{}
		if i+1 < len(fields) {
			right = fields[i+1]
		}
		*lines = append(*lines, cell(fields[i], half)+" "+cell(right, rightW-half-1))
	}
}

// teammateStatusToken is the card's status line token: ok → green, error → red dot + the
// canonical class, unknown → faint.
func teammateStatusToken(t teardown.Teammate) string {
	switch t.Status {
	case "ok":
		return okStyle.Render("● ok")
	case "error":
		return errStyle.Render("● " + teammateStatusWord(t))
	case endedStatus:
		return faintStyle.Render("■ " + endedStatus)
	default:
		return faintStyle.Render("◌ " + teammateStatusWord(t))
	}
}

func (m Model) teammateDetailLines(t teardown.Teammate, rightW int) []string {
	status := teammateStatusToken(t)
	if t.Status == endedStatus {
		if ts, ok := m.endedSeen[t.Team]; ok {
			status += faintStyle.Render(" · last seen " + ts.Format("01-02 15:04"))
		}
	}
	if model := sessiontitle.CleanTitle(t.Model); model != "" {
		status += faintStyle.Render(" · " + trunc(model, 28))
	}
	lines := []string{status, "", faintStyle.Render("Overview")}
	// Two fields per line (the status line already carries the model).
	pane := t.PaneID
	if t.Socket != "" {
		pane += " · " + sessiontitle.CleanTitle(t.Socket)
	}
	hidden := "no"
	if t.Hidden {
		hidden = "yes"
	}
	fields := []detailKV{
		{"team", sessiontitle.CleanTitle(displayTeam(t.Team))},
		{"vendor", sessiontitle.CleanTitle(t.Vendor)},
		{"pane", pane},
		{"pid", fmt.Sprintf("%d", t.PID)},
		{"status", teammateStatusWord(t)},
		{"hidden", hidden},
	}
	if up := teammateUptime(t); up != "" && t.Status != endedStatus {
		fields = append(fields, detailKV{"up", up})
	}
	if t.LeadSessionID != "" {
		fields = append(fields, detailKV{"session", shortSessionID(sessiontitle.CleanTitle(t.LeadSessionID))})
	}
	detailFieldPairs(&lines, fields, rightW)
	if t.Detail != "" {
		detailField(&lines, "detail", t.Detail, rightW)
	}
	// Messages → Activity → Output, fed from the nonce-gated transcript/inbox load. The
	// text reaches ONLY this focused card — rows, rails, and headers never carry it.
	if s := m.asMateSnap; m.asMateKey == mateKey(t) && (s.ctxTok > 0 || s.outTok > 0) {
		detailField(&lines, "tokens", fmt.Sprintf("↑ %s ctx · ↓ %s out",
			humanTokens(s.ctxTok), humanTokens(s.outTok)), rightW)
	}
	lines = append(lines, "")
	if m.asMateKey != mateKey(t) {
		lines = append(lines, faintStyle.Render("(loading…)"))
		return lines
	}
	lines = append(lines, m.mateMessagesSection(rightW)...)
	if !m.asMateFound {
		lines = append(lines, "", faintStyle.Render("(no transcript found)"))
		return lines
	}
	lines = append(lines, "")
	lines = append(lines, activityLines(m.asMateSnap.activity, rightW)...)
	lines = append(lines, "")
	lines = append(lines, m.mateOutputSection(rightW)...)
	return lines
}

// mateMessagesSection renders the teammate card's Messages block: every message it
// received, newest first — pending (● undelivered inbox backlog) and consumed (○, from
// the transcript). Collapsed, each message is one "<dot> <from> · <time>  <summary>"
// line; ⏎ expands the full bodies.
func (m Model) mateMessagesSection(rightW int) []string {
	msgs := m.asMateSnap.msgs
	pending := 0
	for _, msg := range msgs {
		if msg.pending {
			pending++
		}
	}
	header := fmt.Sprintf("Messages · %d", len(msgs))
	if pending > 0 {
		header += fmt.Sprintf(" · %d pending", pending)
	}
	if len(msgs) == 0 {
		return []string{faintStyle.Render(header), faintStyle.Render(" (none)")}
	}
	if !m.asPromptExpanded {
		header += " · ⏎ expand"
	}
	lines := []string{faintStyle.Render(header)}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		dot := faintStyle.Render("○")
		if msg.pending {
			dot = cursorStyle.Render("●")
		}
		meta := sessiontitle.CleanTitle(msg.from)
		if ts, err := time.Parse(time.RFC3339, msg.ts); err == nil {
			meta += " · " + ts.Format("01-02 15:04")
		}
		head := " " + dot + " " + faintStyle.Render(meta)
		if !m.asPromptExpanded {
			if text := sessiontitle.CleanTitle(msg.summary); text != "" {
				budget := rightW - lipgloss.Width(head) - 2
				if budget < 8 {
					budget = 8 // renderBoard's boxCell still bounds the row to the pane
				}
				// A consumed row reads as one quiet faint line; only the pending
				// backlog keeps a bright summary.
				style := faintStyle
				if msg.pending {
					style = liveStyle
				}
				head += "  " + style.Render(truncCols(text, budget))
			}
			lines = append(lines, head)
			continue
		}
		lines = append(lines, head)
		lines = append(lines, ioLines(msg.body, rightW, contentStyle)...)
		if i > 0 {
			lines = append(lines, "")
		}
	}
	return lines
}

// mateOutputSection renders the teammate card's Output block: every output message,
// newest first, each under a faint timestamp rule — always expanded (j/k scroll).
func (m Model) mateOutputSection(rightW int) []string {
	outs := m.asMateSnap.outputs
	lines := []string{faintStyle.Render(fmt.Sprintf("Output · %d messages", len(outs)))}
	if len(outs) == 0 {
		return append(lines, faintStyle.Render(" (no output yet)"))
	}
	for i := len(outs) - 1; i >= 0; i-- {
		o := outs[i]
		stamp := "── "
		if ts, err := time.Parse(time.RFC3339, o.ts); err == nil {
			stamp += ts.Format("01-02 15:04") + " "
		}
		if fill := rightW - 1 - lipgloss.Width(stamp); fill > 0 {
			stamp += strings.Repeat("─", fill)
		}
		lines = append(lines, " "+faintStyle.Render(stamp))
		lines = append(lines, ioLines(o.text, rightW, liveStyle)...)
		if i > 0 {
			lines = append(lines, "") // breathing room between messages
		}
	}
	return lines
}

func (m Model) jobDetailLines(j subagent.Result, rightW int) []string {
	status := statusLabel(j.Status)
	if model := sessiontitle.CleanTitle(j.Model); model != "" {
		status += faintStyle.Render(" · " + trunc(model, 28))
	}
	if j.Attempt > 1 {
		// >1 occurs only in records from engines that retried schema mismatches; surface it.
		status += faintStyle.Render(fmt.Sprintf(" · attempt %d", j.Attempt))
	}
	lines := []string{status, "", faintStyle.Render("Overview")}
	// Two fields per line (the status line already carries the model). The job id keeps
	// its own full-width wrapping line — it is the one field that must stay findable.
	detailField(&lines, "job", sessiontitle.CleanTitle(j.JobID), rightW)
	var fields []detailKV
	if profile := sessiontitle.CleanTitle(j.PromptProfile); profile != "" {
		if d := sessiontitle.CleanTitle(j.SlimDowngrade); d != "" {
			profile += " (ran full: " + d + ")"
		}
		fields = append(fields, detailKV{"profile", profile})
	}
	started := sessiontitle.CleanTitle(j.StartedAt)
	if ts, err := time.Parse(time.RFC3339, j.StartedAt); err == nil {
		started = ts.Format("01-02 15:04")
	}
	fields = append(fields, detailKV{"started", started})
	if el := jobElapsed(j); el != "" {
		fields = append(fields, detailKV{"elapsed", el})
	}
	if in, out := m.jobTokens(j); in > 0 || out > 0 {
		fields = append(fields, detailKV{"tokens", fmt.Sprintf("↑ %s · ↓ %s", humanTokens(in), humanTokens(out))})
	}
	if isTerminalLeaf(j.Status) {
		if j.CostUSD > 0 {
			fields = append(fields, detailKV{"cost", fmt.Sprintf("~$%.4f est", j.CostUSD)})
		}
		if j.NumTurns > 0 {
			fields = append(fields, detailKV{"turns", fmt.Sprintf("%d", j.NumTurns)})
		}
		if stop := sessiontitle.CleanTitle(j.StopReason); stop != "" {
			fields = append(fields, detailKV{"stop", stop})
		}
	}
	detailFieldPairs(&lines, fields, rightW)
	// Prompt → Activity → Output → Outcome, the wf agent card's order, fed from the
	// nonce-gated io load. The Output reads the .answer side file (focused-single-job
	// surface, CleanTitle-scrubbed) — NEVER Result.Result.
	lines = append(lines, "")
	switch {
	case m.asDetailJobID != j.JobID:
		lines = append(lines, faintStyle.Render("(loading…)"))
	case !m.asDetailIO:
		lines = append(lines, faintStyle.Render("(prompt/output not persisted — run with default persist-io)"))
	default:
		lines = append(lines, promptSection(m.asDetailPrompt, m.asPromptExpanded, rightW)...)
	}
	if m.asDetailJobID == j.JobID {
		lines = append(lines, "")
		lines = append(lines, activityLines(m.asDetailSnap, rightW)...)
		if m.asDetailIO {
			lines = append(lines, "", faintStyle.Render("Output"))
			lines = append(lines, ioLines(m.asDetailAnswer, rightW, liveStyle)...)
		}
	}
	lines = append(lines, "", faintStyle.Render("Outcome"), " "+m.renderOutcome(j))
	return lines
}

// runGroup is a workflow run with its jobs bucketed by phase, ready to render.
type runGroup struct {
	runID       string
	name        string
	description string
	sessionID   string // launching Claude session (picker grouping); "" when launched outside one
	cwd         string // launching project dir (shown right-aligned on the run header); "" when unknown
	status      string
	startedAt   string
	updatedAt   string
	phases      []runPhaseGroup
}

// elapsed renders the run's wall-clock from StartedAt. A running run ticks live to now (the board
// re-renders each frame); a terminal run freezes at its last heartbeat (UpdatedAt) so the duration
// stops growing once it ends. A run with no parseable StartedAt renders "—".
func (g runGroup) elapsed() string {
	start, err := time.Parse(time.RFC3339, g.startedAt)
	if err != nil {
		return "—"
	}
	end := time.Now()
	if g.status != "running" && g.updatedAt != "" {
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
// (the manifest's, else the earliest job StartedAt) — an RFC3339 string compare,
// whose lexicographic order matches chronological order for the fixed-width format.
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
			g.cwd = r.Cwd
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
	if title := sessiontitle.CleanTitle(m.sessionMeta[id].Title); title != "" {
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

// shortJobID trims a job UUID to its first 8 chars for the board's job rows.
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
