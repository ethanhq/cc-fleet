package tui

import (
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhq/cc-fleet/internal/ccver"
	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/onboarding"
)

// installOptionCount is the number of choices on the install-Claude nudge.
const installOptionCount = 3

// claudeInstallDoneMsg carries the installer subprocess's exit result back into
// Update after tea.ExecProcess restores the TUI.
type claudeInstallDoneMsg struct{ err error }

// updateInstallClaude drives the first-run install-Claude nudge: ↑/↓ pick a
// choice, enter acts. Index 0 runs the official installer (suspending the TUI);
// "I'll install it myself" acks so the offer never returns; "later" and esc
// proceed without acking, so it comes back next launch. Once installMsg holds an
// outcome, any key proceeds.
func (m Model) updateInstallClaude(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.installMsg != "" {
		return m.afterClaudeInstall()
	}
	switch msg.String() {
	case "up", "k":
		if m.installCursor > 0 {
			m.installCursor--
		}
	case "down", "j":
		if m.installCursor < installOptionCount-1 {
			m.installCursor++
		}
	case "enter":
		switch m.installCursor {
		case 0:
			return m, runClaudeInstallerCmd()
		case 1:
			ackClaudeInstall()
			return m.afterClaudeInstall()
		default:
			return m.afterClaudeInstall()
		}
	case "esc", "q":
		return m.afterClaudeInstall()
	}
	return m, nil
}

// afterClaudeInstall leaves the nudge for the next first-run gate — agent-teams
// if it still needs setup, else the hub — preserving the live model (width /
// height / diag) and clearing the nudge's own cursor and message.
func (m Model) afterClaudeInstall() (tea.Model, tea.Cmd) {
	m.installCursor, m.installMsg = 0, ""
	if onboarding.NeedsAgentTeamsSetup() {
		m.screen = screenSetup
		return m, nil
	}
	return m.toList()
}

// runClaudeInstallerCmd suspends the TUI and runs the official installer in the
// restored terminal with a credential-scrubbed environment — childenv.Clean
// strips the lead's Anthropic credential / routing / model vars and the CC
// markers so the remote script can't inherit them. The command is fail-closed,
// so a non-zero exit is a genuine failure.
func runClaudeInstallerCmd() tea.Cmd {
	name, args, _ := onboarding.ClaudeInstallCommand()
	cmd := exec.Command(name, args...)
	cmd.Env = childenv.Clean(os.Environ())
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return claudeInstallDoneMsg{err: err}
	})
}

// applyClaudeInstallResult turns the installer's exit into the outcome line shown
// on the nudge. A clean exit that left claude locatable acks; a clean exit that
// didn't (usually a stale PATH) reports that without acking or claiming failure;
// a non-zero exit is a failure and is not acked, so the offer returns next launch.
func (m Model) applyClaudeInstallResult(err error) Model {
	_, _, display := onboarding.ClaudeInstallCommand()
	switch {
	case err != nil:
		m.installMsg = "install didn't complete: " + err.Error() + "\ninstall manually: " + display
	default:
		if _, ok := ccver.Located(); ok {
			ackClaudeInstall()
			m.installMsg = "Claude Code installed ✓"
		} else {
			m.installMsg = "installer finished, but cc-fleet still can't find claude —\nopen a new terminal, or install manually: " + display
		}
	}
	return m
}

// ackClaudeInstall records that the install offer is settled so it never shows
// again. Best-effort: a save failure just means it may reappear next run.
func ackClaudeInstall() {
	st, _ := onboarding.LoadState()
	st.ClaudeInstallAck = true
	_ = st.Save()
}

// installClaudeOptions are the three choices in cursor order; option 0 embeds the
// command it will run so the user sees it before pressing enter.
func installClaudeOptions() []string {
	_, _, display := onboarding.ClaudeInstallCommand()
	return []string{
		"install it for me   (" + display + ")",
		"I'll install it myself",
		"later   (remind me next launch)",
	}
}

// viewInstallClaude renders the first-run install-Claude nudge; once installMsg
// holds the installer outcome it replaces the options with that one line.
func (m Model) viewInstallClaude() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("cc-fleet · setup") + "\n\n")
	if m.installMsg != "" {
		b.WriteString(m.installMsg + "\n")
		b.WriteString("\n" + footer("enter to continue"))
		return b.String()
	}
	b.WriteString("Claude Code (the " + selectedStyle.Render("claude") + " CLI) isn't installed.\n")
	b.WriteString(faintStyle.Render("cc-fleet drives it for every provider lane.") + "\n\n")
	b.WriteString(renderSetupOptions(installClaudeOptions(), m.installCursor))
	b.WriteString("\n" + footer("↑/↓ move · enter select · esc later"))
	return b.String()
}
