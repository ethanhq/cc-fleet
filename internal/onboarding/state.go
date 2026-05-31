// Package onboarding implements cc-fleet's first-run guided setup: it covers
// two teammate-mode prerequisites — tmux and agent-teams — guides the user to
// fix them WITH CONSENT, and persists the user's decisions so later runs never
// re-nag.
//
// What each prerequisite detects differs deliberately:
//   - tmux presence is something cc-fleet CAN observe directly (tmux.go).
//   - agent-teams *runtime enablement* can NOT be observed by an external
//     process (it's a Claude runtime state, commonly default-on via GrowthBook
//     with no env var). So we only detect whether agent-teams has been
//     EXPLICITLY CONFIGURED — AgentTeamsConfigured (agentteams.go) reads four
//     sources: the current env, the user's shell rc files, and the global +
//     project settings.json env blocks — and word the nudge as a suggestion,
//     never an assertion that it's "off".
//
// The consented, idempotent ~/.claude/settings.json merge (EnableAgentTeams)
// also lives in agentteams.go; it is cc-fleet's only write to the user's main
// settings, fired only when the user explicitly chooses "enable it for me".
//
// The orchestration (TUI screens, TTY gating) lives in cmd/cc-fleet/onboarding.go
// + internal/tui; this package holds the pure, unit-testable pieces: per-
// capability decision persistence (state.go — State stores TmuxAck /
// AgentTeamsAck, NOT a single FirstRunDone), agent-teams config detection +
// settings merge (agentteams.go), tmux presence + setup gating (tmux.go), and
// the OS-specific tmux install hint (osinfo.go).
//
// It is invoked ONLY from the bare-interactive TUI path, so it never blocks
// headless / agent callers.
package onboarding

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
)

// stateVersion is the schema version of onboarding.json. Bump only on a
// breaking field change.
const stateVersion = 1

// State is the persisted record of the user's onboarding DECISIONS, one ack per
// capability:
//
//   - TmuxAck: the user chose "skip — subagent mode only" on the tmux setup
//     screen, so we never nudge about tmux again. (Choosing "install it" does
//     NOT ack — once tmux is actually present the nudge stops on its own; if it
//     isn't, the nudge should keep showing.)
//   - AgentTeamsAck: the user dealt with the agent-teams setup screen (any
//     choice), so it never shows again.
//
// We do NOT persist a capability cache. tmux presence and agent-teams
// *configuration* are detected fresh each run (cheap, reliable). agent-teams
// *runtime enablement* is never detected — it's a Claude runtime state an
// external process can't observe; the ack only records that the user dealt with
// the one-time nudge.
type State struct {
	Version       int  `json:"version"`
	TmuxAck       bool `json:"tmux_ack"`
	AgentTeamsAck bool `json:"agent_teams_ack"`
}

// StatePath returns ~/.config/cc-fleet/onboarding.json (XDG-aware via
// config.ConfigDir).
func StatePath() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "onboarding.json"), nil
}

// LoadState reads the onboarding decision file. A MISSING file is not an error
// — it returns a zero State (no acks set) so the caller shows the setup nudges.
// A CORRUPT file is also treated as zero (re-guide beats crashing), with the
// parse error returned for optional logging.
func LoadState() (State, error) {
	var st State
	path, err := StatePath()
	if err != nil {
		return st, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, err
	}
	return st, nil
}

// Save writes the onboarding decision file atomically at 0600, creating the
// 0700 config dir if needed. It stamps the current schema version.
func (s State) Save() error {
	s.Version = stateVersion
	path, err := StatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return fileutil.AtomicWrite(path, data, 0o600)
}
