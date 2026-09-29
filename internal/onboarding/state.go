// Package onboarding implements cc-fleet's first-run guided setup: it nudges
// about provider teammates, enables them WITH CONSENT, and persists the user's
// decision so later runs never re-nag. (tmux itself is NOT nudged on first run —
// doctor surfaces the OS-specific install hint instead, via TmuxInstallHint in
// osinfo.go.) A second nudge offers to install the `claude` binary itself when
// none is found.
//
// The consented settings write happens in teammate.Setup (internal/teammate),
// fired from the TUI only when the user explicitly chooses "enable it for me";
// its gate is teammate.NeedsSetupNudge. The order-preserving settings editor it
// uses (EditSettings, SettingsString) lives in settings.go.
//
// The orchestration (setup screens, TTY gating) lives in internal/tui; this
// package holds the pure, unit-testable pieces: decision persistence (state.go),
// the settings editor (settings.go), the OS-specific
// tmux install hint (osinfo.go), and the claude-binary install detection +
// installer command (claude.go).
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

// State is the persisted record of the user's onboarding DECISIONS:
//
//   - AgentTeamsAck: the user dealt with the agent-teams setup screen (any
//     choice), so it never shows again.
//   - ClaudeInstallAck: the install-Claude offer is settled — either dismissed
//     for good ("I'll install it myself") or a successful installer run — so it
//     never shows again. "Later", a failed install, and an exit-0 install that
//     left claude unfindable do NOT set it: the offer returns next launch.
//   - TeammateLaneAck: the user dealt with the provider-teammate lane nudge.
//   - TeammateLane: what cc-fleet recorded about the provider-teammate lane.
//
// We do NOT persist a capability cache. agent-teams *configuration* and the
// claude binary's presence are both detected fresh each run (cheap, reliable);
// the acks only record that the user dealt with a one-time nudge. A file
// written before a field existed reads that field as its zero value.
type State struct {
	Version          int          `json:"version"`
	AgentTeamsAck    bool         `json:"agent_teams_ack"`
	ClaudeInstallAck bool         `json:"claude_install_ack"`
	TeammateLaneAck  bool         `json:"teammate_lane_ack,omitempty"`
	TeammateLane     TeammateLane `json:"teammate_lane"`
}

// TeammateLane records the provider-teammate lane. Only fields with a reader
// are kept: Enabled (userops, doctor, NeedsSetupNudge, repair) and
// ModeWrittenAt (Evaluate). The shim path is not stored: ShimPath() and the
// settings value are authoritative.
type TeammateLane struct {
	Enabled       bool  `json:"enabled"`
	ModeWrittenAt int64 `json:"mode_written_at,omitempty"` // unix ms; 0 means cc-fleet never wrote teammateMode
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
