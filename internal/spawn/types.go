// Package spawn orchestrates the end-to-end "spawn a vendor teammate" flow:
// vendor probe, profile install, fingerprint apply, team registration, and
// tmux split-window. The public entry point is Spawn(Request) Result.
//
// This file defines the request / result / error-code surface used by
// cmd/cc-fleet/spawn.go and by the skill that consumes Spawn's JSON output.
// Keep the JSON tags stable — they are part of the spawn contract.
package spawn

// Request is the input to Spawn. Zero values for optional fields fall back to
// the documented defaults.
type Request struct {
	// Vendor is the vendors.toml table name (e.g. "deepseek"). Required.
	Vendor string

	// AgentName is the teammate's short name (e.g. "worker-1"). Required.
	// Used in --agent-id <name>@<team>, --agent-name <name>, and as the
	// inbox-file basename. Caller is responsible for ensuring it's
	// filesystem- and shell-safe; spawn does not sanitize.
	AgentName string

	// Team is the team this teammate joins (e.g. "myproj"). Required.
	Team string

	// Model is the vendor model id (e.g. "deepseek-v4-flash"). Empty falls
	// back to the vendor's default_model from vendors.toml.
	Model string

	// Color is the teammate's pane color tag (e.g. "cyan"). Empty triggers
	// automatic palette rotation based on team member count.
	Color string

	// Target is a tmux target spec: session / session:window / pane id.
	// Empty triggers tmux.PickAttachedSession().
	Target string

	// Probe controls whether to ping the vendor's models endpoint before
	// spawning. Default true; set false to skip (e.g. for offline tests).
	Probe bool

	// AutoTeam controls whether spawn creates the team directory + config
	// when it doesn't exist. Default true; the CLI exposes --auto-team.
	AutoTeam bool

	// LeadSessionID overrides the parent-session UUID written into the
	// fingerprint flag template. Empty triggers a lookup of the team
	// config's leadSessionId; if that's also empty AND AutoTeam is true,
	// a fresh UUID is generated and persisted.
	LeadSessionID string

	// Verify enables the post-spawn settle check: after the pane is created,
	// confirm the teammate process actually came up rather than exiting
	// immediately on a rejected flag (the symptom of a spawn-recipe mismatch on a
	// CC newer than the bundled recipe). The check only runs when the live CC is
	// also newer than the recipe, so on a matched version it's a no-op regardless.
	// The CLI sets this true by default (`--no-verify` clears it); the zero value
	// is false so library/test callers don't pay the latency unless they opt in.
	Verify bool

	// PermissionModeOverride forces the teammate's permission mode, bypassing
	// the lead-session inheritance probe. One of the internal/permmode
	// values (default / acceptEdits / plan / auto / bypassPermissions); empty
	// means "infer from the lead session's startup flags". The CLI validates
	// the value and rejects --permission-mode + --dangerously-skip-permissions
	// together before this reaches Spawn.
	PermissionModeOverride string
}

// Result is the structured outcome of Spawn. On success ok=true and the
// success-path fields are populated; on failure ok=false and the error_*
// fields plus optional Vendor / Suggestion are populated.
//
// The JSON tags are part of the spawn contract. Empty fields are omitted to
// keep the JSON envelope tight.
type Result struct {
	OK bool `json:"ok"`

	// Success-path fields.
	AgentID     string `json:"agent_id,omitempty"`
	Name        string `json:"name,omitempty"`
	Team        string `json:"team,omitempty"`
	PaneID      string `json:"pane_id,omitempty"`
	TmuxSession string `json:"tmux_session,omitempty"`
	Model       string `json:"model,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
	Color       string `json:"color,omitempty"`
	SpawnTime   string `json:"spawn_time,omitempty"`

	// PermissionInheritance records where the teammate's permission flags came
	// from: "manual" (CLI override), "lead-flag" (inherited an
	// explicit mode from the lead's startup argv), "lead-default" (lead was
	// default/plan/unflagged so no flag was carried), or "frozen-template"
	// (no validated lead — fell back to the fingerprint's captured flags).
	PermissionInheritance string `json:"permission_inheritance,omitempty"`

	// Out-of-tmux swarm fields. Populated ONLY when the teammate was
	// spawned into a private swarm server because the caller wasn't inside tmux;
	// both omitempty, so an in-tmux spawn's envelope is unchanged. TmuxSocket is
	// the persistent socket name (cc-fleet-swarm-<team>) and AttachCommand is the
	// ready-to-run line that attaches to the swarm session.
	TmuxSocket    string `json:"tmux_socket,omitempty"`
	AttachCommand string `json:"attach_command,omitempty"`

	// Failure-path fields.
	ErrorCode  string `json:"error_code,omitempty"`
	ErrorMsg   string `json:"error_msg,omitempty"`
	Vendor     string `json:"vendor,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

// Error code enumeration. Match these via constants in callers — skills
// switch on these strings without parsing prose.
const (
	ErrCodeVendorUnreachable  = "VENDOR_UNREACHABLE"
	ErrCodeKeyInvalid         = "KEY_INVALID"
	ErrCodeModelNotFound      = "MODEL_NOT_FOUND"
	ErrCodeFingerprintMissing = "FINGERPRINT_MISSING"
	ErrCodeFingerprintStale   = "FINGERPRINT_STALE"
	ErrCodeNoLeadSession      = "NO_LEAD_SESSION"
	ErrCodeTeamNotFound       = "TEAM_NOT_FOUND"
	ErrCodePaneCreationFailed = "PANE_CREATION_FAILED"
	ErrCodeUnknownVendor      = "UNKNOWN_VENDOR"
	ErrCodeVendorDisabled     = "VENDOR_DISABLED"
	// ErrCodeDuplicateName is returned when a spawn requests an AgentName that
	// already has a member entry in the same team. Detection happens BEFORE
	// SplitWindow, so the caller gets an explicit code (and no leaked pane) and
	// can pick a fresh name.
	ErrCodeDuplicateName = "DUPLICATE_NAME"
	// ErrCodeSpawnDidNotSettle: the pane was created but the teammate process
	// exited during startup, almost always because a CC newer than the bundled
	// recipe rejected a drifted flag. The spawn is rolled back (pane killed,
	// member + inbox removed) before this is returned, and the skill's self-heal
	// probe re-captures the current recipe.
	ErrCodeSpawnDidNotSettle = "SPAWN_DID_NOT_SETTLE"
)

// fail builds a failure Result with vendor stamped for context.
func fail(code, msg, vendor, suggestion string) Result {
	return Result{
		OK:         false,
		ErrorCode:  code,
		ErrorMsg:   msg,
		Vendor:     vendor,
		Suggestion: suggestion,
	}
}
