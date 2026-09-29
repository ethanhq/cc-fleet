// Package tmux is a thin subprocess wrapper around the `tmux` binary covering
// the operations cc-fleet's teardown / ps / doctor / hide-show flows need:
// enumerate sockets and panes, capture and kill panes, read and write pane
// options, and break / join panes for hide/show.
//
// Every operation can run against either the default tmux server or a socket
// file path, via the Server type (the zero value = default server). Commands
// are passed as argv, never through a shell line, so callers can pass
// user-provided strings without risking command injection.
package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// tmuxBinary is the program name we exec. A variable, not a const, so tests
// can swap in a fake binary via PATH.
var tmuxBinary = "tmux"

// Server is a handle to one tmux server. The zero value targets the DEFAULT
// tmux server and MUST produce argv byte-identical to a bare
// exec.Command(tmuxBinary, args...). A Server built by NewServerPath is
// identified by its socket file path and scopes every command with "-S <path>".
type Server struct {
	path string
}

// NewServerPath returns a handle to the server listening on socketPath; every
// command carries "-S <socketPath>". An empty path is the default server.
func NewServerPath(socketPath string) Server { return Server{path: socketPath} }

// command builds `tmux [-S <path>] <args...>`. The prefix is inserted ONLY
// when non-empty: the default-server path MUST stay byte-for-byte identical to
// the bare exec so callers see no argv drift. This is the single insertion
// point for socket scoping.
func (s Server) command(args ...string) *exec.Cmd {
	if s.path != "" {
		full := make([]string, 0, len(args)+2)
		full = append(full, "-S", s.path)
		full = append(full, args...)
		return exec.Command(tmuxBinary, full...)
	}
	return exec.Command(tmuxBinary, args...)
}

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

// applyMainVerticalLeader reflows windowTarget into main-vertical (one
// full-height pane on the left, the rest stacked on the right) and pins the main
// (left) pane to 30% width, on this server.
//
// It resizes list-panes[0], NOT the resolved leader pane: `select-layout
// main-vertical` always promotes the lowest-indexed pane (= list-panes[0]) to
// the main/left slot, so resizing any other pane would miss the real main pane.
// Best-effort — layout polish must never fail a show.
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

// CapturePane returns the visible plain-text content of paneID via
// `tmux [-S path] capture-pane -t <paneID> -p`. No -e is passed, so escape
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

// SocketPaths returns the socket files in ${TMUX_TMPDIR:-/tmp}/tmux-<uid>/,
// i.e. every tmux server this user may be running (default, claude-swarm-*,
// 0.3.x cc-fleet-swarm-*, custom -L names). A missing directory yields nil;
// whether each server is still alive is left to the caller.
func SocketPaths() []string {
	uid := os.Getuid()
	if uid < 0 {
		return nil // no uid-scoped socket directory on this platform
	}
	base := os.Getenv("TMUX_TMPDIR")
	if base == "" {
		base = "/tmp"
	}
	dir := filepath.Join(base, "tmux-"+strconv.Itoa(uid))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		if e.Type()&os.ModeSocket != 0 {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	return paths
}

// PaneInfo is one pane as ListAllPanes reports it.
type PaneInfo struct {
	SocketPath, SessionName, WindowID, PaneID string
	PanePID                                   int
	Dead                                      bool
	DeadStatus                                int
	Origin                                    string // @ccf_origin
}

// listAllPanesFormat is ':'-separated: tmux 3.6 prints control characters
// such as a tab as '_', and tmux never lets a session name contain ':'. The
// socket path goes last so a ':' inside it survives SplitN; an empty field (an
// unset @ccf_origin) still parses positionally.
const listAllPanesFormat = "#{session_name}:#{window_id}:#{pane_id}:#{pane_pid}:#{pane_dead}:#{pane_dead_status}:#{@ccf_origin}:#{socket_path}"

// ListAllPanes runs `list-panes -a` with listAllPanesFormat. Any tmux failure
// (including a dead server behind a stale socket) is returned as an error so
// the caller can decide to skip that server. Unparseable lines are skipped.
func (s Server) ListAllPanes() ([]PaneInfo, error) {
	out, err := s.command("list-panes", "-a", "-F", listAllPanesFormat).Output()
	if err != nil {
		return nil, fmt.Errorf("tmux list-panes: %w", err)
	}
	var panes []PaneInfo
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.SplitN(strings.TrimRight(line, "\r"), ":", 8)
		if len(f) != 8 || f[2] == "" {
			continue
		}
		pid, err := strconv.Atoi(f[3])
		if err != nil {
			continue
		}
		status, _ := strconv.Atoi(f[5]) // empty while the pane is alive
		panes = append(panes, PaneInfo{
			SessionName: f[0],
			WindowID:    f[1],
			PaneID:      f[2],
			PanePID:     pid,
			Dead:        f[4] == "1",
			DeadStatus:  status,
			Origin:      f[6],
			SocketPath:  f[7],
		})
	}
	return panes, nil
}

// CaptureJoined runs `capture-pane -p -J -S -<lines> -t <paneID>`: the last
// lines of history plus the screen, with lines tmux wrapped joined back (-J),
// so a long failure marker in a narrow pane comes back as one line. Launch
// failure markers are read only through this function.
func (s Server) CaptureJoined(paneID string, lines int) (string, error) {
	if paneID == "" {
		return "", errors.New("tmux CaptureJoined: empty pane id")
	}
	out, err := s.command("capture-pane", "-p", "-J", "-S", "-"+strconv.Itoa(lines), "-t", paneID).Output()
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane -t %s: %w", paneID, err)
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

// HiddenSessionName is the detached session hidden panes are broken into.
const HiddenSessionName = "claude-hidden"

// DisplayMessage runs `tmux display-message -p -t <target> <format>` and returns
// the expanded, trimmed result. Used by the hide flow to capture a pane's
// current window while it's still visible. Returns an error if tmux can't resolve the target.
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

// SetPaneOption runs `set-option -p -t <paneID> <name> <value>`. hide stores
// the pane's origin window in a user option (@ccf_origin), which lives and
// dies with the pane.
func (s Server) SetPaneOption(paneID, name, value string) error {
	if out, err := s.command("set-option", "-p", "-t", paneID, name, value).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux set-option -p -t %s %s: %w (%s)", paneID, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// UnsetPaneOption runs `set-option -p -u -t <paneID> <name>`.
func (s Server) UnsetPaneOption(paneID, name string) error {
	if out, err := s.command("set-option", "-p", "-u", "-t", paneID, name).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux set-option -p -u -t %s %s: %w (%s)", paneID, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// PaneOption runs `show-options -p -v -t <paneID> <name>`. An unset user
// option is "" with no error (tmux reports it as "invalid option").
func (s Server) PaneOption(paneID, name string) (string, error) {
	cmd := s.command("show-options", "-p", "-v", "-t", paneID, name)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "invalid option") {
			return "", nil
		}
		return "", fmt.Errorf("tmux show-options -p -t %s %s: %w (%s)", paneID, name, err, strings.TrimSpace(stderr.String()))
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

// HidePane is the Server-scoped form (a -S server). A pane MUST be broken
// to ITS server's claude-hidden session — new-session+break-pane on another
// server silently misses the pane, then the caller records a hide that never
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
func ShowPane(paneID, originWindow string) error { return Server{}.ShowPane(paneID, originWindow) }

// ShowPane is the Server-scoped form (a -S server): join-pane targets the
// pane's own server, with the reflow polish staying on that same server.
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
// POSIX single-quoting rules, so a user-provided string (e.g. a path in a
// printed shell command) stays one token.
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
