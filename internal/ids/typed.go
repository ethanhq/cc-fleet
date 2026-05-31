// Typed TeamID / AgentName: an optional fail-fast boundary at CLI / spawn.Spawn
// entry; low-level helpers keep validating plain strings. A zero-value
// TeamID/AgentName ("") is invalid by construction — callers MUST check err.

package ids

// TeamID is a path-safety-validated team identifier. The only legal way to
// construct one is NewTeamID — callers MUST NOT cast a raw string to TeamID to
// bypass validation. Use string(t) on the way out for string-typed APIs.
type TeamID string

// AgentName is the member-name counterpart of TeamID. Only NewAgentName
// produces a valid one; raw string casts skip validation.
type AgentName string

// NewTeamID returns a TeamID iff ValidateTeamName(s) succeeds; on failure the
// value is the empty TeamID and the error wraps ErrInvalidTeamName.
func NewTeamID(s string) (TeamID, error) {
	if err := ValidateTeamName(s); err != nil {
		return "", err
	}
	return TeamID(s), nil
}

// NewAgentName is the member-name analogue of NewTeamID.
func NewAgentName(s string) (AgentName, error) {
	if err := ValidateMemberName(s); err != nil {
		return "", err
	}
	return AgentName(s), nil
}

func (t TeamID) String() string { return string(t) }

func (n AgentName) String() string { return string(n) }
