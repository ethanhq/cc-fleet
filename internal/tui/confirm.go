package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/ethanhq/cc-fleet/internal/pinned"
	"github.com/ethanhq/cc-fleet/internal/sessiontitle"
	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/teamhist"
)

// modalPhase is the centered board modal's stage: ask for confirmation, wait on a dispatched async
// op, or show the outcome. modalAsk is the zero value so a fresh confirmModal opens asking.
type modalPhase int

const (
	modalAsk     modalPhase = iota // Cancel / Confirm buttons
	modalRunning                   // an async op dispatched from the modal is in flight ("restarting…")
	modalResult                    // the outcome is shown; any key dismisses
)

// confirmModal is the centered overlay shared by the Agents Board and the provider hub — the single
// home for every transient prompt and outcome they surface. It confirms a destructive or disruptive
// action (delete a run / ended team / job, clear a session's finished, stop or restart a run/agent,
// remove a provider, delete or replace an API key) via the Cancel / Confirm buttons (←/→, cursor
// starts on Cancel, the safe default; no esc), then, for an async action, stays open through a
// modalRunning stage until its outcome lands. A bare outcome with no preceding question (a control
// result, a hide/show or pin failure) opens straight in modalResult via withInfo. Any key dismisses
// a result.
type confirmModal struct {
	prompt    string
	kind      string // one of the confirm* kind constants below
	id        string // the action's target: a run / team / job / session id, a vendor name, or a key index
	arg       string // extra action input — the leaf journal key (restart-agent), the job id (stop-leaf / restart-leaf), or the targeted key's current value (delete-key / replace-key; identity only, never rendered)
	cursor    int    // 0 = Cancel (default), 1 = Confirm
	danger    bool   // a heavy/irreversible ask — the ask frame + prompt warn in red, not amber
	phase     modalPhase
	busy      string // the in-flight label shown in modalRunning ("stopping…", "restarting…")
	result    string // the outcome line (modalResult)
	resultErr bool   // the result is a failure (rendered red, not green)
}

const (
	confirmRun          = "run"
	confirmTeam         = "team"
	confirmJob          = "job"
	confirmClear        = "clear"
	confirmStop         = "stop"
	confirmStopLeaf     = "stop-leaf"
	confirmRestartLeaf  = "restart-leaf"
	confirmStopPhase    = "stop-phase"
	confirmRestartPhase = "restart-phase"
	// confirmRestartPhaseKeyed is the TERMINAL-run variant (journal-key drop + resume).
	confirmRestartPhaseKeyed = "restart-phase-keyed"
	confirmRestart           = "restart"
	confirmRestartAgent      = "restart-agent"
	confirmSession           = "session"
	confirmRemoveVendor      = "remove-vendor"
	confirmDeleteKey         = "delete-key"
	confirmReplaceKey        = "replace-key"
	confirmSwitchDefault     = "switch-default" // change the pinned default to another provider
	confirmUnsetDefault      = "unset-default"  // clear the pinned default
)

// confirmAmber is the modal's confirm-phase accent: the border AND the Cancel/Confirm buttons share
// it. A finished result re-tints the border green (ok) / red (err); the buttons are gone by then.
var confirmAmber = lipgloss.AdaptiveColor{Light: "136", Dark: "220"}

// openConfirm opens a centered confirm modal (cursor defaulting to Cancel) for a destructive action.
func (m Model) openConfirm(kind, id, prompt string) (tea.Model, tea.Cmd) {
	m.confirm = &confirmModal{prompt: prompt, kind: kind, id: id}
	return m, nil
}

// openConfirmDanger opens a confirm modal for a heavy/irreversible action — framed and worded in red.
func (m Model) openConfirmDanger(kind, id, prompt string) (tea.Model, tea.Cmd) {
	m.confirm = &confirmModal{prompt: prompt, kind: kind, id: id, danger: true}
	return m, nil
}

// updateConfirm handles keys while a modal is open. A shown result is dismissed by any key; a running
// op traps every key until its outcome lands; otherwise ←/→ (h/l, tab) move between Cancel and Confirm
// and enter runs the highlighted choice, every other key swallowed (the modal traps focus — no esc).
func (m Model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.confirm.phase {
	case modalResult:
		m.confirm = nil // outcome shown — any key closes it (the board already reloaded)
		return m, nil
	case modalRunning:
		return m, nil // an op is in flight; keys are trapped until its outcome resolves the modal
	}
	c := *m.confirm // modalAsk — copy-on-write: a cursor move must not alias another snapshot's modal
	switch msg.String() {
	case "left", "h":
		c.cursor = 0
	case "right", "l":
		c.cursor = 1
	case "tab":
		c.cursor = 1 - c.cursor
	case "enter":
		if c.cursor != 1 {
			m.confirm = nil // Cancel chosen
			return m, nil
		}
		return m.runConfirmed()
	default:
		return m, nil
	}
	m.confirm = &c
	return m, nil
}

// runConfirmed executes the modal's confirmed action. A clear runs in place and shows its outcome
// here, failing CLOSED — a pin-snapshot or clear error aborts the sweep (it must never run pin-blind)
// and shows a red failure. A stop/restart dispatches and stays open in modalRunning until its
// workflowCtlMsg resolves the result. A delete dispatches and closes; its outcome pops via withInfo.
func (m Model) runConfirmed() (tea.Model, tea.Cmd) {
	c := *m.confirm // work on a copy; the new phase/result is published back via m.confirm = &c
	switch c.kind {
	case confirmClear:
		c.phase = modalResult
		pins, perr := pinned.Snapshot()
		if perr != nil {
			c.result, c.resultErr = "clear failed: "+sessiontitle.CleanTitle(perr.Error()), true
			m.confirm = &c
			return m, nil
		}
		removed, cerr := subagent.ClearFinished(c.id, pins)
		deleted, derr := teamhist.ClearEnded(c.id, pins)
		err := cerr
		if err == nil {
			err = derr
		}
		if err != nil {
			c.result, c.resultErr = "clear failed: "+sessiontitle.CleanTitle(err.Error()), true
			m.confirm = &c
			return m, loadBoard(m.boardEpoch)
		}
		c.result = fmt.Sprintf("cleared %d finished · %d ended team(s)", removed, deleted)
		m.confirm = &c
		return m, loadBoard(m.boardEpoch) // refresh the board behind the result
	case confirmSession:
		// Wipe every record in the session (a live run is stopped first) — runs off the Update
		// goroutine since PurgeRun can block on an engine stop; the modal holds "deleting…".
		c.phase, c.busy = modalRunning, "deleting…"
		m.confirm = &c
		return m, deleteSessionCmd(c.id, m.boardEpoch)
	case confirmStop:
		return m.runAsync(c, "stopping…", stopRunCmd(c.id, m.boardEpoch))
	case confirmRestart:
		return m.runAsync(c, "restarting…", restartCmd(c.id, "", m.boardEpoch))
	case confirmRestartAgent:
		return m.runAsync(c, "restarting…", restartCmd(c.id, c.arg, m.boardEpoch))
	case confirmStopLeaf:
		return m.runAsync(c, "stopping agent…", leafCtlCmd("stop-leaf", c.id, c.arg, m.boardEpoch))
	case confirmRestartLeaf:
		return m.runAsync(c, "restarting agent…", leafCtlCmd("restart-leaf", c.id, c.arg, m.boardEpoch))
	case confirmStopPhase:
		return m.runAsync(c, "stopping phase…", phaseCtlCmd("stop-phase", c.id, c.arg, m.boardEpoch))
	case confirmRestartPhase:
		return m.runAsync(c, "restarting phase…", phaseCtlCmd("restart-phase", c.id, c.arg, m.boardEpoch))
	case confirmRestartPhaseKeyed:
		return m.runAsync(c, "restarting phase…", restartPhaseCmd(c.id, c.arg, m.boardEpoch))
	case confirmRun:
		m.confirm = nil
		if m.wfBusy(c.id) {
			return m, nil // a stop/restart/delete is already in flight
		}
		m = m.markBusy(c.id)
		return m, deleteRunCmd(c.id, m.boardEpoch)
	case confirmTeam:
		m.confirm = nil
		return m, deleteTeamHistCmd(c.id, m.boardEpoch)
	case confirmJob:
		m.confirm = nil
		return m, deleteJobCmd(c.id, m.boardEpoch)
	case confirmRemoveVendor:
		m.confirm = nil
		m.loading = true
		return m, removeVendorCmd(c.id)
	case confirmSwitchDefault:
		m.confirm = nil
		m.loading = true
		return m, setDefaultCmd(c.id, true) // user confirmed the switch → force
	case confirmUnsetDefault:
		m.confirm = nil
		m.loading = true
		return m, unsetDefaultCmd(c.id)
	case confirmDeleteKey:
		// The keyset can refresh while the modal is open, so the target is re-validated
		// by identity (index + the key value captured at open) before the splice.
		m.confirm = nil
		idx, err := strconv.Atoi(c.id)
		if err != nil || idx < 0 || idx >= len(m.keys) || m.keys[idx].Key != c.arg {
			m.keyErr = "key list changed; nothing deleted"
			return m, nil
		}
		m.keys = append(m.keys[:idx], m.keys[idx+1:]...)
		if m.keyCursor > len(m.keys) {
			m.keyCursor = len(m.keys)
		}
		return m, m.saveKeysetCmd()
	case confirmReplaceKey:
		// Same identity re-validation as the delete; a vanished target also ends the
		// edit — committing the typed value into a refreshed list could hit the wrong row.
		m.confirm = nil
		idx, err := strconv.Atoi(c.id)
		if err != nil || idx < 0 || idx >= len(m.keys) || m.keys[idx].Key != c.arg {
			m.keyEditing = false
			m.keyErr = "key list changed; nothing replaced"
			return m, nil
		}
		m.keys[idx].Key = strings.TrimSpace(m.keyInput.Value())
		m.keyEditing = false
		m.keyErr = ""
		return m, m.saveKeysetCmd()
	}
	m.confirm = nil
	return m, nil
}

// runAsync dispatches an async run-control op (stop/restart) and holds the modal open in
// modalRunning with a busy label; the matching workflowCtlMsg later resolves it to a result. A run
// already mid-op just closes (its in-flight guard owns the outcome).
func (m Model) runAsync(c confirmModal, busy string, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	if m.wfBusy(c.id) {
		m.confirm = nil
		return m, nil
	}
	m = m.markBusy(c.id)
	c.phase, c.busy = modalRunning, busy
	m.confirm = &c
	return m, cmd
}

// withInfo opens a standalone info modal: an outcome line (green ok / red error) dismissed by any key.
// It is a no-op when a modal is already open OR a save-name prompt owns the screen, so an async
// outcome arriving while the user is mid-confirm, reading another result, or typing a save name can't
// clobber it or steal that overlay's keys — that outcome's feedback is the board reload instead.
func (m Model) withInfo(msg string, isErr bool) Model {
	if m.confirm != nil || m.wfSaving {
		return m
	}
	m.confirm = &confirmModal{phase: modalResult, result: msg, resultErr: isErr}
	return m
}

// confirmBorder frames the modal: amber while asking or running, green on a successful
// result, red on a failed one — and red while asking/running a danger action (a heavy delete).
func (m Model) confirmBorder() lipgloss.AdaptiveColor {
	switch {
	case m.confirm.phase == modalResult && m.confirm.resultErr:
		return errColor
	case m.confirm.phase == modalResult:
		return okColor
	case m.confirm.danger:
		return errColor
	default:
		return confirmAmber
	}
}

// renderConfirmBox is the modal's bordered content per phase: the colored outcome (modalResult), the
// in-flight busy label (modalRunning), or the prompt over the Cancel / Confirm buttons (modalAsk).
func (m Model) renderConfirmBox() string {
	// Long prompts and outcome lines (the hub's consequence-stating asks, a failed-op
	// message) wrap to the modal's width budget; the board's one-liners pass through.
	w := 56
	if m.width > 0 && m.width-12 < w {
		w = m.width - 12
	}
	var body string
	switch m.confirm.phase {
	case modalResult:
		resultStyle := okStyle
		if m.confirm.resultErr {
			resultStyle = errStyle
		}
		body = lipgloss.JoinVertical(lipgloss.Center,
			resultStyle.Render(strings.Join(wrapTo(m.confirm.result, w), "\n")),
			faintStyle.Render("press any key"))
	case modalRunning:
		body = modalBodyStyle.Render(m.confirm.busy)
	default: // modalAsk
		promptStyle := modalBodyStyle
		if m.confirm.danger {
			promptStyle = errStyle // a heavy delete warns in red
		}
		body = lipgloss.JoinVertical(lipgloss.Center,
			promptStyle.Render(strings.Join(wrapTo(m.confirm.prompt, w), "\n")),
			"",
			confirmButtons(m.confirm.cursor, m.confirmBorder()))
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.confirmBorder()).
		Padding(0, 3).
		Render(body)
}

// confirmButtons renders the Cancel / Confirm pair in the given accent — matching the modal frame
// (amber normally, red for a danger ask) — the cursor's choice highlighted (bold, in ‹ ›), the other
// plain.
func confirmButtons(cursor int, accent lipgloss.AdaptiveColor) string {
	style := lipgloss.NewStyle().Foreground(accent)
	btn := func(label string, selected bool) string {
		if selected {
			return style.Bold(true).Render("‹ " + label + " ›")
		}
		return style.Render("  " + label + "  ")
	}
	return btn("Cancel", cursor == 0) + "     " + btn("Confirm", cursor == 1)
}

// renderSaveBox is the centered name prompt for saving the focused run as a named workflow — the
// modal-style counterpart of the confirm box, framed in the same amber. updateWfSaveInput drives it.
func (m Model) renderSaveBox() string {
	body := lipgloss.JoinVertical(lipgloss.Center,
		liveStyle.Render("Save workflow as:"),
		"",
		m.wfSaveInput.View(),
		"",
		faintStyle.Render("enter save · esc cancel"))
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(confirmAmber).
		Padding(0, 3).
		Render(body)
}

// overlayCenter floats box centered over base, preserving the board on both sides of it: for each
// covered row it keeps base's columns left of the box, drops in the box's line, then keeps base's
// columns right of the box (ANSI-aware via ansi.Cut, so the board's borders survive). width is the
// rendered board width; 0 falls back to the widest base line.
func overlayCenter(base, box string, width int) string {
	baseLines := strings.Split(base, "\n")
	boxLines := strings.Split(box, "\n")
	boxW := 0
	for _, l := range boxLines {
		if w := ansi.StringWidth(l); w > boxW {
			boxW = w
		}
	}
	if width <= 0 {
		for _, l := range baseLines {
			if w := ansi.StringWidth(l); w > width {
				width = w
			}
		}
	}
	left := (width - boxW) / 2
	if left < 0 {
		left = 0
	}
	top := (len(baseLines) - len(boxLines)) / 2
	if top < 0 {
		top = 0
	}
	for i, bl := range boxLines {
		ri := top + i
		if ri < 0 || ri >= len(baseLines) {
			continue
		}
		row := baseLines[ri]
		leftPart := padTo(ansi.Cut(row, 0, left), left)
		rightPart := ansi.Cut(row, left+boxW, 1<<30)
		baseLines[ri] = leftPart + bl + rightPart
	}
	return strings.Join(baseLines, "\n")
}

// padTo right-pads s with spaces to width w (ANSI-aware); a longer s is returned unchanged.
func padTo(s string, w int) string {
	if d := w - ansi.StringWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}
