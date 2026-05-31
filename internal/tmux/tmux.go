// Package tmux is a thin subprocess wrapper around the `tmux` binary covering
// the operations cc-fleet's spawn / teardown / ps / hide-show flows need: pick
// a spawn target, split a window to host a teammate or create a tiled swarm
// session, enumerate / kill panes, kill a server, and break / join panes for
// hide/show.
//
// Every operation can run against either the default tmux server or a named
// socket, via the Server type (the zero value = default server); the swarm
// flow targets a private socket. Commands are shell-quoted internally so
// callers can pass user-provided strings without risking command injection.
package tmux

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// tmuxBinary is the program name we exec. A variable, not a const, so tests
// can swap in a fake binary via PATH.
var tmuxBinary = "tmux"

// Server is a handle to one tmux server, identified by its socket name. The
// zero value (socket == "") targets the DEFAULT tmux server and MUST produce
// argv byte-identical to a bare exec.Command(tmuxBinary, args...) — every
// in-tmux path uses it. A non-empty socket inserts "-L <socket>" before the
// subcommand, scoping every operation to a private server; the swarm flow uses
// a persistently named socket (cc-fleet-swarm-<team>) so a later teardown / ps
// can still reap that server after the short-lived cc-fleet process exits.
type Server struct{ socket string }

func NewServer(socket string) Server { return Server{socket: socket} }

// command builds `tmux [-L <socket>] <args...>`. The -L prefix is inserted ONLY
// when socket != "": tmux rejects an empty -L value, and the default-server
// path MUST stay byte-for-byte identical to the bare exec so callers see no
// argv drift. This is the single insertion point for socket scoping.
func (s Server) command(args ...string) *exec.Cmd {
	if s.socket != "" {
		full := make([]string, 0, len(args)+2)
		full = append(full, "-L", s.socket)
		full = append(full, args...)
		return exec.Command(tmuxBinary, full...)
	}
	return exec.Command(tmuxBinary, args...)
}

// SwarmSessionName / SwarmViewWindow name the detached session and window that
// host out-of-tmux teammates on a private socket.
const (
	SwarmSessionName = "claude-swarm"
	SwarmViewWindow  = "swarm-view"
)

// Pane describes one tmux pane across all sessions/windows, as returned by
// ListPanes. Field meanings track tmux(1) format directives:
//   - PaneID:        "#{pane_id}"            e.g. "%42"
//   - SessionName:   "#{session_name}"
//   - WindowIndex:   "#{window_index}"
//   - PaneActive:    "#{pane_active}"        "1"=true (genuine boolean)
//   - Attached:      "#{session_attached}"   client count; "0"=detached
//   - Command:       "#{pane_current_command}"
type Pane struct {
	PaneID      string
	SessionName string
	WindowIndex int
	PaneActive  bool
	Attached    bool
	Command     string
}

// listPanesFormat is the format string passed to `tmux list-panes -F`. Fields
// are space-separated and parsed positionally in parseListPanes.
const listPanesFormat = "#{pane_id} #{session_name} #{window_index} #{pane_active} #{session_attached} #{pane_current_command}"

// listSessionsFormat is the format string for picking an attached session. The
// width field MUST be #{window_width}, NOT #{session_width} — the latter is not
// a valid tmux format variable and expands to an empty string.
const listSessionsFormat = "#{session_name} #{session_attached} #{window_width}"

// PickAttachedSession returns the name of the attached session with the
// largest window width — a fallback spawn target used only when the caller's
// own pane ($TMUX_PANE) is unavailable, on the heuristic that the widest
// attached session is the screen the user is looking at.
//
// session_attached is the count of attached clients, so any value other than
// "0" means the session has at least one client.
//
// Returns an error if `tmux ls` reports no sessions or none of them are
// attached.
func PickAttachedSession() (string, error) { return Server{}.PickAttachedSession() }

// PickAttachedSession is the Server-scoped form, routed through Server.command
// (the single tmux exec outlet).
func (s Server) PickAttachedSession() (string, error) {
	out, err := s.command("ls", "-F", listSessionsFormat).Output()
	if err != nil {
		// `tmux ls` exits non-zero when there's no running server. Surface a
		// clean message so the caller can map it to PANE_CREATION_FAILED.
		return "", fmt.Errorf("tmux ls: %w", err)
	}

	type cand struct {
		name  string
		width int
	}
	var attached []cand
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		// session_attached is the attached-client count; "0" means detached.
		if fields[1] == "0" {
			continue
		}
		w, err := strconv.Atoi(fields[2])
		if err != nil {
			// Unparseable width → 0: lowest priority, but still a usable
			// attached candidate, so don't drop it.
			w = 0
		}
		attached = append(attached, cand{name: fields[0], width: w})
	}
	if len(attached) == 0 {
		return "", errors.New("no attached tmux session")
	}

	best := attached[0]
	for _, c := range attached[1:] {
		if c.width > best.width {
			best = c
		}
	}
	return best.name, nil
}

// SessionForPane resolves the session name that owns a tmux target — typically
// a bare pane id such as "%84" taken from $TMUX_PANE. spawn uses it to report
// which session a teammate landed in when the split target is a pane id rather
// than a session name.
func SessionForPane(target string) (string, error) { return Server{}.SessionForPane(target) }

// SessionForPane is the Server-scoped form, routed through Server.command.
func (s Server) SessionForPane(target string) (string, error) {
	if target == "" {
		return "", errors.New("tmux SessionForPane: empty target")
	}
	out, err := s.command("display-message", "-p", "-t", target, "#{session_name}").Output()
	if err != nil {
		return "", fmt.Errorf("tmux display-message -t %s: %w", target, err)
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", fmt.Errorf("tmux display-message -t %s: empty session name", target)
	}
	return name, nil
}

// PaneExists reports whether paneID is still a live pane on this server — the
// cross-platform liveness signal spawn's post-spawn settle check uses (a
// teammate that exits closes its pane, since remain-on-exit is off).
//
// It checks paneID against the full pane list (`list-panes -a`), NOT
// `display-message -t <pane>`: the latter, run inside tmux, silently falls back
// to the caller's current pane for an unknown target and exits 0, so it can't
// tell a live pane from a dead one. List membership is exact; a gone server
// yields an empty list → not alive.
func (s Server) PaneExists(paneID string) bool {
	if paneID == "" {
		return false
	}
	panes, err := s.ListPanesWithPid()
	if err != nil {
		return false
	}
	for _, p := range panes {
		if p.PaneID == paneID {
			return true
		}
	}
	return false
}

// SplitWindow runs cmd in a new tmux pane and returns the new pane's ID
// (e.g. "%42"). Layout: the leader (the caller's pane) stays on the left at 30%
// width while teammates are stacked in the right 70% column.
//
//   - target is a tmux target spec: "%<pane>" (the caller's $TMUX_PANE — the
//     normal case), "<session>", or "<session>:<window>". An empty target is
//     rejected; SplitWindow does not auto-pick (that's PickAttachedSession's
//     job).
//   - direction is "h" (new pane to the right) or "v" (new pane below). It is
//     the split axis for the FIRST teammate and for the fallback when tmux
//     introspection fails. For additional teammates the final geometry is set
//     by select-layout main-vertical, so direction no longer applies there.
//   - cmd is the literal shell command tmux runs in the new pane, passed as a
//     single argv element, so callers MUST pre-quote user input via Quote().
//   - color is the teammate's agent color. The spawn palette uses Claude's
//     AgentColorName vocabulary (e.g. "red"/"purple"); decoratePane maps it to
//     a tmux color for the border via tmuxColorName. An empty color skips
//     border styling.
//   - name is the agent name shown as the pane's border title; empty skips it.
//
// Layout:
//   - First teammate (window has 1 pane): split off the leader with the new
//     pane at 70% (leader keeps 30%). A two-pane split is already balanced, so
//     no reflow is needed.
//   - Additional teammates (2+ panes): split the leader vertically — which
//     keeps the leader's width intact, avoiding a "pane too small" failure when
//     repeatedly halving an already-narrow leader — then reflow the whole
//     window with select-layout main-vertical and pin the leader to 30% width.
//     The intermediate split axis is cosmetic because the reflow recomputes
//     every pane's geometry.
//
// Pane/window introspection (display-message, list-panes) is best-effort: a
// failure falls back to a plain split honoring direction. The cosmetic reflow
// (select-layout, resize-pane) and the border/title decoration (decoratePane)
// are likewise best-effort and silently ignored. Only the load-bearing split
// that yields the pane id surfaces an error — layout and styling polish must
// never fail a spawn.
func SplitWindow(target, direction, cmd, color, name string) (string, error) {
	if target == "" {
		return "", errors.New("tmux SplitWindow: empty target")
	}
	if direction != "h" && direction != "v" {
		return "", fmt.Errorf("tmux SplitWindow: direction %q must be \"h\" or \"v\"", direction)
	}
	if cmd == "" {
		return "", errors.New("tmux SplitWindow: empty cmd")
	}

	// Resolve the caller's leader pane and how many panes already share its
	// window. ok=false means we can't introspect, so fall back to the historical
	// plain split off target.
	leaderPane, paneCount, ok := paneContext(target)

	var pane string
	var err error
	switch {
	case !ok:
		// Can't introspect: historical plain split off the original target.
		pane, err = splitPane(target, direction, 0, cmd)
	case paneCount <= 1:
		// First teammate: leader 30% / teammate 70%. No reflow needed.
		pane, err = splitPane(leaderPane, direction, 70, cmd)
	default:
		// Additional teammate: split the leader vertically (its width is never
		// halved, so this can't fail on a narrow leader), then let main-vertical
		// define the real geometry.
		pane, err = splitPane(leaderPane, "v", 0, cmd)
		if err == nil {
			applyMainVerticalLeader(leaderPane)
		}
	}
	if err != nil {
		return "", err
	}

	// Decorate the new pane (colored border + name title) once the load-bearing
	// split has succeeded. Best-effort: applies to every branch — including the
	// fallback split — because styling is orthogonal to layout.
	Server{}.decoratePane(pane, color, name)
	return pane, nil
}

// splitPane runs `tmux split-window` off target and returns the new pane id.
// direction is "h"/"v"; when sizePercent > 0 the new pane is sized via `-l`
// (requires tmux 3.1+). `-d` keeps focus on the originating pane; `-P -F
// "#{pane_id}"` makes tmux print the new pane id.
//
// cmd is passed as a single argv element (NOT through `sh -c <userinput>`), so
// we never compose a shell line by concatenation; tmux hands cmd to its
// default-shell -c. Routes through Server{}.command — splitPane is in-tmux only,
// so the default server (socket=="") is correct.
func splitPane(target, direction string, sizePercent int, cmd string) (string, error) {
	args := []string{"split-window", "-t", target, "-" + direction}
	if sizePercent > 0 {
		args = append(args, "-l", strconv.Itoa(sizePercent)+"%")
	}
	args = append(args, "-d", "-P", "-F", "#{pane_id}", cmd)

	out, err := (Server{}).command(args...).Output()
	if err != nil {
		return "", fmt.Errorf("tmux split-window: %w", err)
	}
	pane := strings.TrimSpace(string(out))
	if pane == "" {
		return "", errors.New("tmux split-window: empty pane id in output")
	}
	return pane, nil
}

// paneContext resolves the leader pane id that owns target and the number of
// panes in that pane's window. target may be a pane id, a session, or
// "session:window"; `display-message -t target '#{pane_id}'` resolves all three
// to the target's active pane (the leader). ok is false — not an error — when
// tmux can't introspect, signaling SplitWindow to fall back to a plain split.
// Routes through Server{}.command — in-tmux only, so the default server is correct.
func paneContext(target string) (leaderPane string, paneCount int, ok bool) {
	s := Server{}
	out, err := s.command("display-message", "-p", "-t", target, "#{pane_id}").Output()
	if err != nil {
		return "", 0, false
	}
	leaderPane = strings.TrimSpace(string(out))
	if leaderPane == "" {
		return "", 0, false
	}

	// list-panes -t <pane> enumerates every pane in that pane's window.
	out, err = s.command("list-panes", "-t", leaderPane, "-F", "#{pane_id}").Output()
	if err != nil {
		return "", 0, false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			paneCount++
		}
	}
	if paneCount == 0 {
		return "", 0, false
	}
	return leaderPane, paneCount, true
}

// applyMainVerticalLeader reflows windowTarget into main-vertical (one
// full-height pane on the left, the rest stacked on the right) and pins the main
// (left) pane to 30% width. It is the SINGLE reflow shared by SplitWindow and
// ShowPane, so the two can never drift.
//
// It resizes list-panes[0], NOT the resolved leader pane: `select-layout
// main-vertical` always promotes the lowest-indexed pane (= list-panes[0]) to
// the main/left slot, so resizing any other pane would miss the real main pane.
// Best-effort — layout polish must never fail a spawn or a show.
func applyMainVerticalLeader(windowTarget string) { Server{}.applyMainVerticalLeader(windowTarget) }

// applyMainVerticalLeader is the Server-scoped form so socket-aware ShowPane can
// reflow on the right server; SplitWindow keeps using the default server.
func (s Server) applyMainVerticalLeader(windowTarget string) {
	_ = s.command("select-layout", "-t", windowTarget, "main-vertical").Run()
	out, err := s.command("list-panes", "-t", windowTarget, "-F", "#{pane_id}").Output()
	if err != nil {
		return // can't enumerate panes → skip the cosmetic resize
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			_ = s.command("resize-pane", "-t", p, "-x", "30%").Run()
			return
		}
	}
}

// tmuxColorName maps a Claude AgentColorName (the spawn palette / --agent-color
// vocabulary) to the tmux color name used for the pane border. Names tmux
// already understands (red/blue/green/yellow/cyan) map to themselves; the three
// that diverge (purple/orange/pink) translate. Anything not in the table — e.g.
// a raw tmux color like "magenta" or "colour123" — passes through unchanged.
func tmuxColorName(color string) string {
	switch color {
	case "purple":
		return "magenta"
	case "orange":
		return "colour208"
	case "pink":
		return "colour205"
	default:
		return color
	}
}

// decoratePane gives a freshly-split teammate pane a colored border plus the
// agent's name shown in the border.
//
// Everything here is best-effort — every command's error is swallowed — so
// styling never fails a spawn. The per-pane border options (set-option -p)
// require tmux 3.2+; on older tmux they no-op (graceful degradation).
//
// The name is embedded LITERALLY in pane-border-format, NOT via `#{pane_title}`:
// claude's TUI overwrites the pane title at startup, so `#{pane_title}` would
// render claude's agent type instead of the teammate name. That's also why
// there is no `select-pane -T`.
//
// color is a Claude AgentColorName mapped to a tmux color via tmuxColorName for
// the border fg, so the border matches the teammate's own TUI theme. An empty
// color skips border styling, an empty name skips the title, and an unrecognized
// color passes through unchanged so an explicit raw tmux color still works.
func (s Server) decoratePane(paneID, color, name string) {
	tcolor := tmuxColorName(color)
	if color != "" {
		// Pane body fg + inactive/active border fg. select-pane -P sets the
		// pane style; set-option -p sets per-pane border styles (tmux 3.2+).
		_ = s.command("select-pane", "-t", paneID, "-P", "bg=default,fg="+tcolor).Run()
		_ = s.command("set-option", "-p", "-t", paneID, "pane-border-style", "fg="+tcolor).Run()
		_ = s.command("set-option", "-p", "-t", paneID, "pane-active-border-style", "fg="+tcolor).Run()
	}
	if name == "" {
		return
	}

	// tmux uses '#' as the format escape introducer (#{...}, #[...]), so double
	// any '#' in the name to keep it literal — agent names are normally
	// [a-zA-Z0-9_-], but escape defensively.
	escName := strings.ReplaceAll(name, "#", "##")
	format := "#[bold] " + escName + " #[default]"
	if color != "" {
		format = "#[fg=" + tcolor + ",bold] " + escName + " #[default]"
	}
	_ = s.command("set-option", "-p", "-t", paneID, "pane-border-format", format).Run()
	// pane-border-status is a WINDOW option — set on every split: idempotent, so
	// re-setting is harmless and avoids tracking the window's first teammate.
	// Targeting the new pane id with -w resolves to its containing window.
	_ = s.command("set-option", "-w", "-t", paneID, "pane-border-status", "top").Run()
}

// PanePid pairs a tmux pane id with the pid of its top-level shell. Returned
// by ListPanesWithPid for the teardown/discover pipeline; mirrors the layout
// `tmux list-panes -a -F "#{pane_id} #{pane_pid}"` produces.
type PanePid struct {
	PaneID string
	PID    int
}

// ListPanesWithPid runs `tmux [-L socket] list-panes -a -F "#{pane_id}
// #{pane_pid}"` and returns one PanePid per parsed line. An empty server (no
// panes) returns an empty slice with no error; tmux failures with a "no server
// running" ExitError are translated to (nil, nil) so the discover path stays
// robust when tmux is down. Unparseable lines are skipped to keep ps robust
// against future tmux format changes.
func (s Server) ListPanesWithPid() ([]PanePid, error) {
	out, err := s.command("list-panes", "-a", "-F", "#{pane_id} #{pane_pid}").Output()
	if err != nil {
		// No tmux server running → no panes → no teammates. Surface as an
		// empty result rather than a hard error so `ps` works even with tmux
		// down. Any ExitError counts: the hard-coded args here never trigger
		// genuine misuse.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux list-panes: %w", err)
	}
	var pairs []PanePid
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		pairs = append(pairs, PanePid{PaneID: fields[0], PID: pid})
	}
	return pairs, nil
}

// CapturePane returns the visible plain-text content of paneID via
// `tmux [-L socket] capture-pane -t <paneID> -p`. No -e is passed, so escape
// sequences are stripped — the caller gets clean text.
func (s Server) CapturePane(paneID string) (string, error) {
	if paneID == "" {
		return "", errors.New("tmux CapturePane: empty pane id")
	}
	out, err := s.command("capture-pane", "-t", paneID, "-p").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ListPanes returns every pane across every session/window known to the
// running tmux server. Returns an empty slice (not an error) when the
// server is running but has no panes; returns an error only when tmux itself
// fails.
func ListPanes() ([]Pane, error) { return Server{}.ListPanes() }

// ListPanes is the Server-scoped form. On a non-default socket it enumerates
// only that server's panes — used by the socket-aware ps / teardown paths to
// see out-of-tmux swarm panes.
func (s Server) ListPanes() ([]Pane, error) {
	out, err := s.command("list-panes", "-a", "-F", listPanesFormat).Output()
	if err != nil {
		return nil, fmt.Errorf("tmux list-panes: %w", err)
	}
	return parseListPanes(string(out))
}

// parseListPanes parses the multi-line output of `tmux list-panes -a -F ...`.
// Split out for unit-testing without invoking a real tmux.
func parseListPanes(out string) ([]Pane, error) {
	var panes []Pane
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		// 6 fields are: pane_id, session, window_index, pane_active,
		// session_attached, pane_current_command.
		if len(fields) < 6 {
			return nil, fmt.Errorf("tmux list-panes: malformed line %q", line)
		}
		idx, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("tmux list-panes: bad window_index in %q: %w", line, err)
		}
		panes = append(panes, Pane{
			PaneID:      fields[0],
			SessionName: fields[1],
			WindowIndex: idx,
			PaneActive:  fields[3] == "1",
			// session_attached is the attached-client count, so any value other
			// than "0" means at least one client.
			Attached: fields[4] != "0",
			Command:  fields[5],
		})
	}
	return panes, nil
}

// KillPane runs `tmux kill-pane -t <paneID>`. A pane that no longer exists
// counts as a no-op from the caller's perspective (idempotent teardown), so
// we swallow the "can't find pane" error and only surface real exec failures.
func KillPane(paneID string) error { return Server{}.KillPane(paneID) }

// KillPane is the Server-scoped form. teardown uses the socket-scoped form for
// swarm panes, which live on a private server the default socket can't see —
// without it, kill-pane on the default server returns "can't find pane", is
// swallowed as success, and silently leaks the swarm pane.
func (s Server) KillPane(paneID string) error {
	if paneID == "" {
		return errors.New("tmux KillPane: empty pane id")
	}
	out, err := s.command("kill-pane", "-t", paneID).CombinedOutput()
	if err == nil {
		return nil
	}
	// tmux prints "can't find pane" / "no such pane" on exit code 1 when the
	// target was already killed. Treat that as success.
	msg := strings.ToLower(string(out))
	if strings.Contains(msg, "can't find") || strings.Contains(msg, "no such") || strings.Contains(msg, "no current target") {
		return nil
	}
	return fmt.Errorf("tmux kill-pane %s: %w (%s)", paneID, err, strings.TrimSpace(string(out)))
}

// KillServer terminates this server (`tmux -L <socket> kill-server`), used by
// teardown to remove a swarm server once its panes are gone so no orphaned tmux
// server lingers. A not-running server is success (idempotent). Refuses the
// default server: only the in-tmux user's own server runs there and cc-fleet
// must never kill it.
func (s Server) KillServer() error {
	if s.socket == "" {
		return errors.New("tmux KillServer: refusing to kill the default server")
	}
	out, err := s.command("kill-server").CombinedOutput()
	if err == nil {
		return nil
	}
	// "no server running on <socket>" / "failed to connect to server" → already
	// gone, which is exactly the post-condition we want. Treat as success.
	msg := strings.ToLower(string(out))
	if strings.Contains(msg, "no server running") || strings.Contains(msg, "failed to connect") || strings.Contains(msg, "no such file") {
		return nil
	}
	return fmt.Errorf("tmux -L %s kill-server: %w (%s)", s.socket, err, strings.TrimSpace(string(out)))
}

// HasSession reports whether a session named name exists on this server
// (`tmux has-session -t name`, exit 0 = exists). Used by SpawnSwarm to decide
// between creating the swarm session and splitting into the existing one.
func (s Server) HasSession(name string) bool {
	return s.command("has-session", "-t", name).Run() == nil
}

// SpawnSwarm launches cmd in a teammate pane on this server's swarm session
// (SwarmSessionName / SwarmViewWindow), creating the detached session + window
// on first use and tiling additional teammates with NO leader pane. Returns the
// new pane id.
//
// Only valid on a non-default socket (a swarm always has its own server);
// calling it on the default server is a programmer error. Session/pane creation
// is load-bearing (its failure is returned); the tiled reflow and border/title
// decoration are best-effort polish. cmd MUST be pre-quoted by the caller (tmux
// hands it to its default-shell -c).
//
// createdServer reports whether THIS call created the swarm session (the first
// teammate on the socket). The spawn rollback uses it to decide whether a failed
// spawn may kill the whole swarm server — it may ONLY when createdServer is true
// (no other member exists yet); a later teammate's failed spawn must KillPane
// just its own pane and leave the running first member's server alone.
func (s Server) SpawnSwarm(cmd, color, name string) (pane string, createdServer bool, err error) {
	if s.socket == "" {
		return "", false, errors.New("tmux SpawnSwarm: empty socket (swarm requires a private server)")
	}
	if cmd == "" {
		return "", false, errors.New("tmux SpawnSwarm: empty cmd")
	}

	target := SwarmSessionName + ":" + SwarmViewWindow
	createdServer = !s.HasSession(SwarmSessionName)
	if createdServer {
		// First teammate: create the detached session + named window, running cmd
		// in the initial pane. -P -F prints the new pane id.
		out, oerr := s.command("new-session", "-d", "-s", SwarmSessionName,
			"-n", SwarmViewWindow, "-P", "-F", "#{pane_id}", cmd).Output()
		if oerr != nil {
			return "", false, fmt.Errorf("tmux new-session swarm: %w", oerr)
		}
		pane = strings.TrimSpace(string(out))
	} else {
		// Additional teammate: split a new pane into the swarm-view window, then
		// tile so every teammate gets an equal share (no leader). -d keeps the
		// detached session from stealing focus.
		out, oerr := s.command("split-window", "-t", target, "-d", "-P", "-F", "#{pane_id}", cmd).Output()
		if oerr != nil {
			return "", false, fmt.Errorf("tmux split-window swarm: %w", oerr)
		}
		pane = strings.TrimSpace(string(out))
		_ = s.command("select-layout", "-t", target, "tiled").Run()
	}
	if pane == "" {
		return "", false, errors.New("tmux SpawnSwarm: empty pane id in output")
	}

	s.decoratePane(pane, color, name)
	return pane, createdServer, nil
}

// HiddenSessionName is the detached session hidden panes are broken into.
const HiddenSessionName = "claude-hidden"

// DisplayMessage runs `tmux display-message -p -t <target> <format>` and returns
// the expanded, trimmed result. It's the generic form of SessionForPane — used
// by the hide flow to capture a pane's current "session_name:window_index"
// while it's still visible. Returns an error if tmux can't resolve the target.
func DisplayMessage(target, format string) (string, error) {
	return Server{}.DisplayMessage(target, format)
}

// DisplayMessage is the Server-scoped form. When a pane lives on a private
// swarm socket the query MUST hit that same server — otherwise the default
// server reports the pane as missing.
func (s Server) DisplayMessage(target, format string) (string, error) {
	if target == "" {
		return "", errors.New("tmux DisplayMessage: empty target")
	}
	out, err := s.command("display-message", "-p", "-t", target, format).Output()
	if err != nil {
		return "", fmt.Errorf("tmux display-message -t %s: %w", target, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// HidePane breaks paneID out of its current window into the detached
// claude-hidden session. The pane's process is NOT killed (break-pane only
// relocates the pane) and its pane id is unchanged, so teardown still finds and
// KillPanes it afterward.
//
// new-session is idempotent infrastructure: a "duplicate session" error just
// means claude-hidden already exists, so its error is swallowed. break-pane is
// the load-bearing op (it actually moves the pane), so its error is returned.
func HidePane(paneID string) error { return Server{}.HidePane(paneID) }

// HidePane is the Server-scoped form. A swarm pane MUST be broken to ITS
// server's claude-hidden session — the default-server new-session+break-pane
// silently misses on a swarm pane, then the caller records a hide that never
// happened.
func (s Server) HidePane(paneID string) error {
	if paneID == "" {
		return errors.New("tmux HidePane: empty pane id")
	}
	// Create the hidden session if absent; "duplicate session" is the normal
	// idempotent case, so swallow this error.
	_ = s.command("new-session", "-d", "-s", HiddenSessionName).Run()
	// Load-bearing: relocate the pane. A failure here means the pane is gone or
	// tmux is unreachable — surface it so the caller doesn't record a hide that
	// didn't happen.
	if err := s.command("break-pane", "-d", "-s", paneID, "-t", HiddenSessionName+":").Run(); err != nil {
		return fmt.Errorf("tmux break-pane %s: %w", paneID, err)
	}
	return nil
}

// ShowPane joins paneID back into originWindow (e.g. "main:0"), reflows the
// window to main-vertical, and pins the main pane to 30% width. join-pane is
// load-bearing (failure returns an error); the reflow is best-effort polish.
//
// The reflow goes through applyMainVerticalLeader — the SAME helper SplitWindow
// uses — so spawn and show produce identical geometry by construction.
func ShowPane(paneID, originWindow string) error { return Server{}.ShowPane(paneID, originWindow) }

// ShowPane is the Server-scoped form: socket-aware so a swarm pane's join-pane
// targets the right server, with the reflow polish staying on that same server.
func (s Server) ShowPane(paneID, originWindow string) error {
	if paneID == "" {
		return errors.New("tmux ShowPane: empty pane id")
	}
	if originWindow == "" {
		return errors.New("tmux ShowPane: empty origin window")
	}
	// Load-bearing: move the pane back into its origin window.
	if err := s.command("join-pane", "-h", "-s", paneID, "-t", originWindow).Run(); err != nil {
		return fmt.Errorf("tmux join-pane %s -> %s: %w", paneID, originWindow, err)
	}
	// Best-effort polish: reflow + pin the main pane on the same server.
	s.applyMainVerticalLeader(originWindow)
	return nil
}

// Quote returns s wrapped for safe inclusion in a single shell token using
// POSIX single-quoting rules. Used by spawn to assemble the command line
// passed to SplitWindow without trusting user-provided vendor / model /
// name strings.
//
// Rules:
//   - empty string becomes a pair of single quotes (the literal empty arg).
//   - no embedded single quote: wrap in single quotes.
//   - embedded single quote: close the quoted run, emit the escaped quote
//     sequence backslash-quote, reopen quoting. Example: alice's becomes
//     four pieces concatenated: 'alice' then \' then 's'.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsRune(s, '\'') {
		return "'" + s + "'"
	}
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		if r == '\'' {
			// Close the single-quoted segment, emit an escaped quote, reopen.
			b.WriteString(`'\''`)
			continue
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}
