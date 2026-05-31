package spawn

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/ids"
)

// teamsRoot returns $HOME/.claude/teams. $HOME is required — XDG does not
// apply because Claude Code reads ~/.claude/ unconditionally.
func teamsRoot() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return "", errors.New("spawn: HOME is not set")
	}
	return filepath.Join(home, ".claude", "teams"), nil
}

// TeamDir returns $HOME/.claude/teams/<team>.
//
// Defense-in-depth: team is validated against ids.ValidateTeamName before
// joining the path, and the constructed path is under-root checked. CLI entry
// points already validate, so this is belt-and-braces against any future caller
// that bypasses the entry layer.
func TeamDir(team string) (string, error) {
	if err := ids.ValidateTeamName(team); err != nil {
		return "", fmt.Errorf("spawn: %w", err)
	}
	root, err := teamsRoot()
	if err != nil {
		return "", err
	}
	out := filepath.Join(root, team)
	if err := ids.EnsureUnderRoot(root, out); err != nil {
		return "", fmt.Errorf("spawn: %w", err)
	}
	return out, nil
}

// TeamConfigPath returns $HOME/.claude/teams/<team>/config.json.
func TeamConfigPath(team string) (string, error) {
	dir, err := TeamDir(team)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// TeamsDir returns $HOME/.claude/teams — the root every team directory lives
// under. Exported for callers (panevis) that need to glob across teams.
func TeamsDir() (string, error) {
	return teamsRoot()
}

// FindMemberByPane scans every team's config.json for a member whose
// tmuxPaneId == paneID and returns its (team, name). found=false (with nil
// error) means no team claims the pane — including the case where the teams
// root doesn't exist yet. A team whose config is unreadable is skipped rather
// than aborting the scan.
func FindMemberByPane(paneID string) (team, name string, found bool, err error) {
	root, err := teamsRoot()
	if err != nil {
		return "", "", false, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("spawn: read teams root: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		tc, loadErr := LoadTeamConfig(e.Name())
		if loadErr != nil {
			continue
		}
		for _, m := range tc.Members {
			if m.TmuxPaneID == paneID {
				return e.Name(), m.Name, true, nil
			}
		}
	}
	return "", "", false, nil
}

// InboxPath returns $HOME/.claude/teams/<team>/inboxes/<name>.json.
//
// Defense-in-depth: both team and name are path-validated before joining; the
// constructed path is under-root checked. CLI entry points already validate.
func InboxPath(team, name string) (string, error) {
	if err := ids.ValidateMemberName(name); err != nil {
		return "", fmt.Errorf("spawn: %w", err)
	}
	dir, err := TeamDir(team)
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "inboxes", name+".json")
	if err := ids.EnsureUnderRoot(dir, out); err != nil {
		return "", fmt.Errorf("spawn: %w", err)
	}
	return out, nil
}

// Member describes one teammate registered in a team's config.json. JSON
// tags match what Claude Code itself writes — we co-exist with native
// Agent-spawned members in the same members array, so the shape has to
// match exactly.
//
// Color/Prompt/PlanModeRequired/BackendType/IsActive are the fields the main
// session reads to render a teammate's UI: its pane color, the task it was
// given, and—via isActive + backendType—whether it shows as a live tmux-backed
// member in the team tracker. They carry omitempty because the team-lead entry
// omits them, and a zero value must not inject a key into an entry that never
// had one.
//
// Raw preserves any per-member key we don't model (e.g. fields a future Claude
// Code version adds) so a cc-fleet spawn round-trips existing members verbatim
// instead of stripping them — the member-level analogue of TeamConfig.Raw.
type Member struct {
	AgentID          string   `json:"agentId"`
	Name             string   `json:"name"`
	Color            string   `json:"color,omitempty"`
	AgentType        string   `json:"agentType"`
	Model            string   `json:"model"`
	JoinedAt         int64    `json:"joinedAt"` // unix millis
	TmuxPaneID       string   `json:"tmuxPaneId"`
	Cwd              string   `json:"cwd"`
	Subscriptions    []string `json:"subscriptions"`
	Prompt           string   `json:"prompt,omitempty"`
	PlanModeRequired bool     `json:"planModeRequired,omitempty"`
	BackendType      string   `json:"backendType,omitempty"`
	IsActive         bool     `json:"isActive,omitempty"`
	// OriginWindow / Hidden track hide/show state. OriginWindow is the
	// "session:window_index" the pane was in when hidden, captured at hide time;
	// it's cleared on show. Both are omitempty so a never-hidden member — and the
	// team-lead entry — gain neither key (byte-stable round-trip).
	// CAUTION: because they're omitempty, clearing them on show requires deleting
	// the keys from Raw first (the rawMap shadow trap) — panevis does this.
	OriginWindow string `json:"originWindow,omitempty"`
	Hidden       bool   `json:"hidden,omitempty"`
	// Socket is the per-member tmux server socket. When non-empty, the member's
	// pane lives on a private swarm server (the
	// "cc-fleet-swarm-<team>" socket); when empty, the pane lives on the
	// caller's default tmux server (the normal in-tmux path). Adding the
	// socket per-member — rather than relying on TeamConfig.TmuxSocket alone —
	// is what lets a single team hold a mix of in-tmux and swarm panes (e.g.
	// after a re-spawn in different contexts). omitempty keeps in-tmux
	// members' config.json bytes byte-identical to before this field existed.
	//
	// On read, callers that need a member's effective socket should prefer
	// Member.Socket when set and fall back to TeamConfig.TmuxSocket otherwise
	// (the legacy field, kept for backward compatibility with configs written
	// by older cc-fleet versions).
	Socket string `json:"tmuxSocket,omitempty"`
	// LeadSessionID records WHICH Claude parent session spawned THIS member, so
	// a re-used team with a different caller (or an explicit --lead-session-id
	// mismatch) attributes each member to its true lead instead of the team's
	// original one. When a member's LeadSessionID is empty, AnnotateLeadSession
	// falls back to TeamConfig.LeadSessionID so older configs (no per-member
	// field) keep working unchanged.
	LeadSessionID string         `json:"leadSessionId,omitempty"`
	Raw           map[string]any `json:"-"`
}

// rawMap renders a Member as the generic map persisted to config.json: the
// member's preserved unknown fields (Raw) first, then the typed fields layered
// on top so the values cc-fleet owns win. Typed fields tagged omitempty drop
// out when zero, letting a preserved Raw value (or its absence) show through —
// that's what keeps the team-lead entry, which omits color/isActive/etc.,
// byte-stable across a round-trip.
func (m Member) rawMap() (map[string]any, error) {
	b, err := json.Marshal(m) // Raw is json:"-", so excluded here
	if err != nil {
		return nil, fmt.Errorf("spawn: marshal member %q: %w", m.Name, err)
	}
	var typed map[string]any
	if err := json.Unmarshal(b, &typed); err != nil {
		return nil, fmt.Errorf("spawn: unmarshal member %q: %w", m.Name, err)
	}
	out := make(map[string]any, len(m.Raw)+len(typed))
	for k, v := range m.Raw {
		out[k] = v
	}
	for k, v := range typed {
		out[k] = v
	}
	return out, nil
}

// ErrTeamNotFound is returned by LoadTeamConfig when the team directory or
// config.json file does not exist. Distinguishes "spawn into a non-existent
// team without --auto-team" from real I/O errors.
var ErrTeamNotFound = errors.New("spawn: team not found")

// TeamConfig is cc-fleet's view of a team's config.json. We deliberately keep
// the original parsed map in Raw so any fields written by native Agent (or by
// a future CC version) round-trip through Save without loss.
type TeamConfig struct {
	LeadSessionID string         `json:"leadSessionId"`
	Members       []Member       `json:"members"`
	Raw           map[string]any `json:"-"`
}

// swarmSocketKey is the config.json key (stored under Raw, NOT a typed field)
// that records the persistent tmux socket name for an out-of-tmux swarm team.
// It lives in Raw because WriteTeamConfig only force-writes leadSessionId +
// members — a new typed TeamConfig field would silently fail to round-trip. Its
// presence is also the marker that a team is swarm-backed: teardown / ps key off
// it to scan the right tmux server.
const swarmSocketKey = "tmuxSocket"

// SwarmSocketName returns the persistent socket name for a team's out-of-tmux
// swarm server: "cc-fleet-swarm-<team>". It is stable across cc-fleet
// invocations — cc-fleet is a short-lived CLI, so a PID-scoped socket would be
// unreachable the moment it exits; a deterministic per-team name lets a later
// teardown / ps find the server.
func SwarmSocketName(team string) string { return "cc-fleet-swarm-" + team }

// TmuxSocket returns the swarm socket name persisted in tc (Raw["tmuxSocket"]),
// or "" when the team has no swarm socket — the normal in-tmux case.
func (tc *TeamConfig) TmuxSocket() string {
	if tc == nil || tc.Raw == nil {
		return ""
	}
	if v, ok := tc.Raw[swarmSocketKey].(string); ok {
		return v
	}
	return ""
}

// SetTmuxSocket records socket in tc.Raw so WriteTeamConfig persists it. Passing
// "" clears the key (delete), avoiding the omitempty-Raw-shadow trap that would
// otherwise leave a stale value visible.
func (tc *TeamConfig) SetTmuxSocket(socket string) {
	if tc.Raw == nil {
		tc.Raw = map[string]any{}
	}
	if socket == "" {
		delete(tc.Raw, swarmSocketKey)
		return
	}
	tc.Raw[swarmSocketKey] = socket
}

// LoadTeamConfig reads $HOME/.claude/teams/<team>/config.json.
//
// Returns ErrTeamNotFound (wrapped) if the file is missing — discriminate
// via errors.Is. Any other error (parse failure, permission denied) is
// returned as-is.
func LoadTeamConfig(team string) (*TeamConfig, error) {
	path, err := TeamConfigPath(team)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", path, ErrTeamNotFound)
		}
		return nil, fmt.Errorf("spawn: read %s: %w", path, err)
	}
	return parseTeamConfig(data)
}

// parseTeamConfig is split out so spawn_test can exercise the parser
// without touching the filesystem.
func parseTeamConfig(data []byte) (*TeamConfig, error) {
	// Decode into a generic map first so unknown fields survive a round-trip.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("spawn: parse team config: %w", err)
	}

	tc := &TeamConfig{Raw: raw}
	if v, ok := raw["leadSessionId"]; ok {
		if s, ok := v.(string); ok {
			tc.LeadSessionID = s
		}
	}
	if v, ok := raw["members"]; ok {
		// Re-marshal + unmarshal the members slice into the typed Member
		// struct. This is simpler than walking the generic map and matches
		// the round-trip strategy used in internal/config/vendor.go.
		mb, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("spawn: re-marshal members: %w", err)
		}
		if err := json.Unmarshal(mb, &tc.Members); err != nil {
			return nil, fmt.Errorf("spawn: parse members: %w", err)
		}
		// Capture each member's full key set so per-member fields we don't
		// model round-trip verbatim through WriteTeamConfig (rawMap). Both
		// views decode the same array, so indices align.
		var rawMembers []map[string]any
		if err := json.Unmarshal(mb, &rawMembers); err != nil {
			return nil, fmt.Errorf("spawn: parse members raw: %w", err)
		}
		for i := range tc.Members {
			if i < len(rawMembers) {
				tc.Members[i].Raw = rawMembers[i]
			}
		}
	}
	return tc, nil
}

// WriteTeamConfig saves tc to $HOME/.claude/teams/<team>/config.json
// atomically with mode 0600. The parent directory is created at 0700 on
// demand. Unknown fields preserved in tc.Raw are merged back into the
// JSON output so we don't clobber state we didn't create.
func WriteTeamConfig(team string, tc *TeamConfig) error {
	if tc == nil {
		return errors.New("spawn: WriteTeamConfig: nil TeamConfig")
	}
	path, err := TeamConfigPath(team)
	if err != nil {
		return err
	}

	// Merge typed fields onto Raw so unknown keys persist. We overwrite the
	// two keys we own (leadSessionId, members) with the typed values.
	out := make(map[string]any, len(tc.Raw)+2)
	for k, v := range tc.Raw {
		out[k] = v
	}
	out["leadSessionId"] = tc.LeadSessionID
	// Re-encode members as the struct-tagged camelCase shape the rest of CC
	// reads, merging each member's preserved unknown fields underneath the typed
	// ones (rawMap). Marshalling the typed slice directly would drop any
	// per-member key not modelled by Member, blanking those teammates' UI.
	members := make([]any, len(tc.Members))
	for i, m := range tc.Members {
		mo, err := m.rawMap()
		if err != nil {
			return err
		}
		members[i] = mo
	}
	out["members"] = members

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("spawn: marshal team config: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("spawn: mkdir %s: %w", dir, err)
	}

	if err := fileutil.AtomicWrite(path, data, 0o600); err != nil {
		return fmt.Errorf("spawn: write %s: %w", path, err)
	}
	return nil
}

// EnsureTeamDir creates $HOME/.claude/teams/<team>/ and ./inboxes/ at mode
// 0700 if they don't already exist. Idempotent.
func EnsureTeamDir(team string) error {
	dir, err := TeamDir(team)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("spawn: mkdir %s: %w", dir, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "inboxes"), 0o700); err != nil {
		return fmt.Errorf("spawn: mkdir inboxes: %w", err)
	}
	return nil
}

// EnsureMember adds m to the team's config.json members slice iff no entry
// with the same name already exists. Returns (added, error) where added is
// true on the new-entry path.
//
// MUST be called inside config.WithTeamLock(team, ...) — concurrent spawns
// in the same team race otherwise.
func EnsureMember(team string, m Member) (bool, error) {
	tc, err := LoadTeamConfig(team)
	if err != nil {
		// LoadTeamConfig only returns ErrTeamNotFound on a missing config.json;
		// other errors (parse failure, permission) bubble up. For the missing
		// case we treat the team as fresh and create a config with this
		// member.
		if !errors.Is(err, ErrTeamNotFound) {
			return false, err
		}
		tc = &TeamConfig{Members: nil, Raw: map[string]any{}}
	}
	for _, existing := range tc.Members {
		if existing.Name == m.Name {
			return false, nil
		}
	}
	tc.Members = append(tc.Members, m)
	if err := WriteTeamConfig(team, tc); err != nil {
		return false, err
	}
	return true, nil
}

// EnsureInbox writes "[]" to the inbox file at 0600 if it doesn't already
// exist. Parent directories are created at 0700. Idempotent — an existing
// non-empty inbox is left untouched.
func EnsureInbox(team, name string) error {
	path, err := InboxPath(team, name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("spawn: mkdir %s: %w", dir, err)
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("spawn: stat %s: %w", path, err)
	}
	// O_EXCL avoids a TOCTOU race against a concurrent writer (we're inside
	// the team lock so this is belt-and-braces).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return fmt.Errorf("spawn: open inbox %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString("[]"); err != nil {
		return fmt.Errorf("spawn: write inbox %s: %w", path, err)
	}
	return nil
}
