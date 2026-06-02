package teardown

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/procintrospect"
	"github.com/ethanhq/cc-fleet/internal/spawn"
	"github.com/ethanhq/cc-fleet/internal/tmux"
)

// Teammate is the structured row cc-fleet ps emits per live teammate.
// JSON tags here match what cmd/cc-fleet/ps.go writes to stdout — keep
// stable.
//
// Status / ErrorClass / Detail are health fields populated ONLY by
// `cc-fleet ps --check` (the capture-pane scan in health.go). They are all
// omitempty, so a plain `cc-fleet ps --json` (no --check) emits the exact
// same shape it always has.
type Teammate struct {
	Name          string `json:"name"`
	Team          string `json:"team"`
	PaneID        string `json:"pane_id"`
	Vendor        string `json:"vendor"`
	Model         string `json:"model"`
	PID           int    `json:"pid"`
	LeadSessionID string `json:"lead_session_id,omitempty"`
	SpawnTime     int64  `json:"-"`

	// Socket is the tmux server socket the pane lives on. Populated by
	// DiscoverTeammates when the pane was found on a private swarm server
	// (cc-fleet-swarm-<team>); empty when the pane is on the caller's default
	// tmux server (the normal in-tmux case). omitempty keeps the in-tmux ps JSON
	// contract byte-identical.
	//
	// Downstream consumers (AnnotateHealth's capturePane, panevis.HideRef /
	// ShowRef, etc.) use this socket to scope tmux ops to the right server;
	// without it, default-server tmux calls on a swarm pane silently miss.
	Socket string `json:"tmux_socket,omitempty"`

	Status     string `json:"status,omitempty"`      // ok | error | unknown (--check only)
	ErrorClass string `json:"error_class,omitempty"` // set when status=error
	Detail     string `json:"detail,omitempty"`      // canonical, never raw pane text

	// Hidden is populated ONLY by AnnotateHidden (reads the team config.json).
	// A plain `ps --json` leaves it absent (omitempty), same as the health fields.
	Hidden bool `json:"hidden,omitempty"`
}

// DiscoverTeammates returns every live cc-fleet teammate, identified by a
// claude process running with --agent-id inside a tmux pane.
//
// Pipeline:
//  1. tmux list-panes -a -F "#{pane_id} #{pane_pid}"  → pane_id ↔ shell pid
//  2. For each shell pid, walk its process subtree looking for a claude
//     process whose cmdline contains --agent-id.
//  3. Parse that cmdline for name / team / vendor / model.
//
// Teammates outside tmux (e.g. ones a user started manually for testing)
// are intentionally skipped — cc-fleet only owns the ones it spawned, and
// every spawn lives in a pane.
//
// Returns an empty slice (not nil-error) when no teammates are found. Hard
// errors (tmux unreachable) are returned as-is so the caller can surface
// them.
func DiscoverTeammates() ([]Teammate, error) {
	// Scan the default tmux server PLUS every team's private swarm socket: an
	// out-of-tmux teammate lives on cc-fleet-swarm-<team>, invisible to the
	// default server's list-panes. A teammate's claude process is found the same
	// way on either server — via /proc from the pane's shell pid — so a pid is
	// deduped across servers (it can only belong to one).
	sockets := append([]string{""}, teamSwarmSockets()...)
	var teammates []Teammate
	seen := make(map[int]struct{})
	for _, sock := range sockets {
		panes, err := listPanesWithPid(sock)
		if err != nil {
			if sock == "" {
				return nil, fmt.Errorf("list panes: %w", err)
			}
			// A swarm socket whose server is already gone yields nothing to list,
			// not a hard error — skip it (best-effort, ps stays robust).
			continue
		}
		for _, p := range panes {
			tmId, ok := findTeammateInSubtree(p.PID)
			if !ok {
				continue
			}
			if _, dup := seen[tmId]; dup {
				continue
			}
			argv, err := readCmdline(tmId)
			if err != nil {
				// Process disappeared between the subtree walk and the read —
				// skip silently; ps is a snapshot.
				continue
			}
			info, ok := parseTeammateCmdline(argv)
			if !ok {
				continue
			}
			info.PaneID = p.PaneID
			info.PID = tmId
			// Stamp the socket the pane was found on so downstream annotate /
			// hide-show / capture can scope tmux ops to the right server.
			// Empty sock = default server (in-tmux); non-empty = a private
			// swarm server.
			info.Socket = sock
			seen[tmId] = struct{}{}
			teammates = append(teammates, info)
		}
	}
	return teammates, nil
}

// teamSwarmSockets returns the distinct, non-empty swarm socket names recorded
// across every team's config.json (Raw["tmuxSocket"]). These are the private
// tmux servers out-of-tmux teammates live on; ps must scan them in addition to
// the default server. Best-effort — unreadable teams are skipped.
func teamSwarmSockets() []string {
	home := os.Getenv("HOME")
	if home == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(home, ".claude", "teams"))
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var sockets []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		tc, loadErr := spawn.LoadTeamConfig(e.Name())
		if loadErr != nil {
			continue
		}
		if s := tc.TmuxSocket(); s != "" {
			if _, dup := seen[s]; !dup {
				seen[s] = struct{}{}
				sockets = append(sockets, s)
			}
		}
	}
	return sockets
}

// listPanesWithPid routes through internal/tmux.Server.ListPanesWithPid so
// every tmux exec funnels through the one Server.command outlet. socket ""
// targets the default server. An empty tmux server (no panes) returns an empty
// slice with no error, matching the historical contract.
func listPanesWithPid(socket string) ([]tmux.PanePid, error) {
	pairs, err := tmux.NewServer(socket).ListPanesWithPid()
	if err != nil {
		return nil, fmt.Errorf("tmux list-panes: %w", err)
	}
	return pairs, nil
}

// findTeammateInSubtree breadth-first walks the process subtree rooted at
// shellPid looking for a claude process whose cmdline contains --agent-id.
// Returns (pid, true) on first hit, (0, false) otherwise.
//
// We use /proc/<pid>/task/<tid>/children which is more portable than pgrep
// --parent (and doesn't require recursive shell exec).
func findTeammateInSubtree(shellPid int) (int, bool) {
	queue := []int{shellPid}
	seen := map[int]struct{}{shellPid: {}}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]

		// Check this pid's cmdline first — the teammate could be the pane's
		// top-level process (rare, but the spawn cmd uses `env <binary> ...`
		// which execs through env and then claude, so the depth varies).
		if argv, err := readCmdline(pid); err == nil {
			if cmdlineLooksLikeTeammate(argv) {
				return pid, true
			}
		}

		// Enqueue children.
		for _, child := range readChildren(pid) {
			if _, dup := seen[child]; dup {
				continue
			}
			seen[child] = struct{}{}
			queue = append(queue, child)
		}
	}
	return 0, false
}

// readChildren returns pid's immediate children via procintrospect (Linux /proc
// task children, darwin `pgrep -P`). Errors degrade to no children.
func readChildren(pid int) []int {
	return procintrospect.Children(pid)
}

// readCmdline returns pid's argv via procintrospect (Linux /proc, darwin ps).
// On Linux the NUL-separated kernel layout is preserved as an exact slice so
// arguments containing spaces (e.g.
// `--settings /tmp/home with space/.claude/profiles/glm.json`) survive intact;
// on darwin ps space-splits the argv, which is sufficient because cc-fleet's
// markers (--agent-id, --settings, --model) never contain spaces. An empty
// cmdline yields a nil slice.
func readCmdline(pid int) ([]string, error) {
	return procintrospect.Cmdline(pid)
}

// cmdlineLooksLikeTeammate returns true if argv names a claude binary AND
// carries --agent-id. The --agent-id flag is the primary, near-decisive marker
// (teardown matches the exact <name>@<team>); the binary check below only
// rejects an unrelated process that happens to carry an --agent-id-like token
// but isn't actually claude. It takes the argv slice directly so arguments with
// embedded spaces (e.g. `--settings /tmp/home with space/...`) are not shredded.
//
// We look for a claude *executable* token, NOT any "claude" substring: an
// incidental ".claude" in a path argument (e.g. --settings
// /home/u/.claude/foo.json) must not match a non-claude process. A token
// qualifies only if it contains a "/claude/" PATH SEGMENT — which covers our
// spawn binary …/share/claude/versions/<hash>, whose basename is a version
// number, NOT "claude" — or its basename contains "claude" (e.g. /usr/bin/claude).
// Deliberately NOT basename=="claude": that would miss the versions/<hash> path.
func cmdlineLooksLikeTeammate(argv []string) bool {
	hasAgentID := false
	for _, tok := range argv {
		if tok == "--agent-id" {
			hasAgentID = true
			break
		}
	}
	if !hasAgentID {
		return false
	}
	for _, tok := range argv {
		if strings.Contains(tok, "/claude/") {
			return true
		}
		base := tok
		if i := strings.LastIndexByte(tok, '/'); i >= 0 {
			base = tok[i+1:]
		}
		if strings.Contains(base, "claude") {
			return true
		}
	}
	return false
}

// cmdlineAgentID returns the value following --agent-id in argv (e.g.
// "alice@alpha"), or "" if the flag is absent or has no value. Takes argv
// []string (the kernel cmdline layout) so we never depend on space-split
// parsing that can mangle paths with spaces.
func cmdlineAgentID(argv []string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--agent-id" {
			return argv[i+1]
		}
	}
	return ""
}

// discoverTeammatePIDs scans /proc for live claude teammate processes whose
// --agent-id exactly matches agentID. Unlike findTeammateInSubtree (which
// walks down from a pane's shell), this searches the whole process table —
// required because a teammate reparents to init when its tmux pane is killed,
// leaving it outside any pane subtree (the "ghost teammate").
//
// Matching is exact on the full <name>@<team> id, so tearing down one teammate
// never signals a different team's process. Our own pid is always skipped.
// Best-effort: unreadable /proc entries (races, permissions) are silently
// skipped.
func discoverTeammatePIDs(agentID string) []int {
	if agentID == "" {
		return nil
	}
	procs, err := procintrospect.ProcessTable()
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, p := range procs {
		if p.PID == self {
			continue
		}
		if cmdlineLooksLikeTeammate(p.Argv) && cmdlineAgentID(p.Argv) == agentID {
			pids = append(pids, p.PID)
		}
	}
	return pids
}

// cmdlineTeamName returns the value following --team-name in argv, or "" if
// absent. Matching on this explicit token (rather than splitting the
// <name>@<team> agent id) is unambiguous: team and member names may contain '@'.
func cmdlineTeamName(argv []string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--team-name" {
			return argv[i+1]
		}
	}
	return ""
}

// discoverTeamAgentIDsFn is a test seam over discoverTeamAgentIDs so the
// parse-fail teardown path can be exercised without faking the process table.
var discoverTeamAgentIDsFn = discoverTeamAgentIDs

// discoverTeamAgentIDs scans the process table for live teammate processes in
// team, returning their distinct agent ids. The config-free counterpart to
// discoverTeammatePIDs: used when a team's config can't be parsed, so ghosts
// still get reaped by team name.
func discoverTeamAgentIDs(team string) []string {
	if team == "" {
		return nil
	}
	procs, err := procintrospect.ProcessTable()
	if err != nil {
		return nil
	}
	self := os.Getpid()
	seen := map[string]struct{}{}
	var ids []string
	for _, p := range procs {
		if p.PID == self || !cmdlineLooksLikeTeammate(p.Argv) || cmdlineTeamName(p.Argv) != team {
			continue
		}
		if id := cmdlineAgentID(p.Argv); id != "" {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// parseTeammateCmdline extracts the structured Teammate fields from a /proc
// cmdline argv slice. Missing fields are left empty rather than erroring —
// ps's job is to display what's there.
//
// Takes argv []string (NUL-separated kernel layout) so arguments containing
// spaces — most notably `--settings /tmp/home with space/.../glm.json` — aren't
// shredded into multiple tokens (which would feed vendorFromProfilePath the
// wrong fragment).
//
// Returns ok=false when --agent-id has no value following it (i.e. the
// cmdline doesn't parse as a teammate at all), or when no name could be
// resolved.
func parseTeammateCmdline(argv []string) (Teammate, bool) {
	var t Teammate

	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--agent-id":
			if i+1 >= len(argv) {
				return Teammate{}, false
			}
			// Format: <name>@<team>. The skill always spawns with this
			// shape; we tolerate a missing @ by leaving Team empty.
			val := argv[i+1]
			if at := strings.Index(val, "@"); at >= 0 {
				t.Name = val[:at]
				t.Team = val[at+1:]
			} else {
				t.Name = val
			}
			i++
		case "--agent-name":
			if i+1 < len(argv) {
				// --agent-id usually arrives first and already populated
				// Name; if not, take it from here.
				if t.Name == "" {
					t.Name = argv[i+1]
				}
				i++
			}
		case "--team-name":
			if i+1 < len(argv) {
				if t.Team == "" {
					t.Team = argv[i+1]
				}
				i++
			}
		case "--settings":
			if i+1 < len(argv) {
				t.Vendor = vendorFromProfilePath(argv[i+1])
				i++
			}
		case "--model":
			if i+1 < len(argv) {
				t.Model = argv[i+1]
				i++
			}
		}
	}
	if t.Name == "" {
		// Without a name we can't usefully report this teammate.
		return Teammate{}, false
	}
	return t, true
}

// vendorFromProfilePath strips the directory and ".json" suffix off a
// profile path. /root/.claude/profiles/glm.json → "glm". Returns "" for
// inputs that don't end in .json.
func vendorFromProfilePath(p string) string {
	base := filepath.Base(p)
	if !strings.HasSuffix(base, ".json") {
		return ""
	}
	return strings.TrimSuffix(base, ".json")
}
