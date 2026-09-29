// Package teammate is cc-fleet's native provider-teammate lane: Claude Code
// starts every split-pane teammate through CLAUDE_CODE_TEAMMATE_COMMAND, which
// points at cc-fleet's shim; the shim hands ccf-<provider> agent types to
// `cc-fleet __teammate-launch`, which swaps in the provider's profile and model
// and execs the lead's own claude binary.
//
// This file holds the lane's shared contract: constants,
// error codes, detail words, the agent-type grammar, the failure line, and the
// check / setup JSON envelopes.
package teammate

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/ids"
)

// Protocol, names and keys.
const (
	Protocol              = 1
	ShimProtocol          = 1
	MinTeammateCC         = "2.1.278"
	TypePrefix            = "ccf-" // ownership is proven only by this prefix on argv --agent-type or a team member's agentType, never by a profile path
	LaunchVerb            = "__teammate-launch"
	ShimFileName          = "claude-teammate" // <config.ConfigDir()>/bin/claude-teammate
	ShimMarker            = "managed-by: cc-fleet (shim protocol 1)"
	AgentDefMarker        = "<!-- managed-by: cc-fleet agentdefs v1; regenerate with `cc-fleet repair`; do not edit -->"
	OriginOption          = "@ccf_origin"
	EnvTeammateCommand    = "CLAUDE_CODE_TEAMMATE_COMMAND"
	EnvAgentTeams         = "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"
	EnvSubagentModelForce = "CLAUDE_CODE_SUBAGENT_MODEL_FORCE"
	EnvProcessWrapper     = "CLAUDE_CODE_PROCESS_WRAPPER"
	KeyTeammateMode       = "teammateMode"
)

// Error codes introduced by the teammate lane (11).
const (
	CodeLaneUnavailable     = "TEAMMATE_LANE_UNAVAILABLE"
	CodeSetupRequired       = "TEAMMATE_SETUP_REQUIRED"
	CodeLeadRestart         = "LEAD_RESTART_REQUIRED"
	CodeModeInProcess       = "TEAMMATE_MODE_IN_PROCESS"
	CodeBadAgentCall        = "BAD_AGENT_CALL"
	CodeClaudeNotFound      = "CLAUDE_NOT_FOUND"
	CodeAmbiguousTarget     = "AMBIGUOUS_TARGET"
	CodeIdentityMismatch    = "IDENTITY_MISMATCH"
	CodeSettingsUnparseable = "SETTINGS_UNPARSEABLE"
	CodeSetupConflict       = "SETUP_CONFLICT"
	CodeCommandRemoved      = "COMMAND_REMOVED"
)

// Error codes shared with other packages (same literals, same meaning).
const (
	CodeBadArgs               = "BAD_ARGS"
	CodeUnsupportedOnWindows  = "UNSUPPORTED_ON_WINDOWS"
	CodeUnknownProvider       = "UNKNOWN_PROVIDER"
	CodeProviderDisabled      = "PROVIDER_DISABLED"
	CodeConfigLoadFailed      = "CONFIG_LOAD_FAILED"
	CodeProfileWriteFailed    = "PROFILE_WRITE_FAILED"
	CodeCodexProxyUnavailable = "CODEX_PROXY_UNAVAILABLE"
	CodeProviderUnreachable   = "PROVIDER_UNREACHABLE"
	CodeKeyInvalid            = "KEY_INVALID"
	CodeInternal              = "INTERNAL"
	// NO_DEFAULT_PROVIDER and DEFAULT_PROVIDER_{UNKNOWN,DISABLED,RESERVED} come verbatim from config.ProviderErrorCode.
)

// Detail words (stable; the annotated ones take ":" + an argument).
const (
	DetailNoLeadSession         = "no_lead_session"
	DetailCCTooOld              = "cc_too_old"
	DetailNoSessionTeam         = "no_session_team"
	DetailDisabledByCC          = "teammates_disabled_by_cc"
	DetailLauncherNotConfigured = "launcher_not_configured"
	DetailLauncherForeign       = "launcher_foreign"
	DetailShimBroken            = "shim_broken"
	DetailLauncherTargetMissing = "launcher_target_missing" // only in the shim's fallback failure line
	DetailAgentDefMissing       = "agent_def_missing"
	DetailAgentDefForeign       = "agent_def_foreign"
	DetailAgentDefShadowed      = "agent_def_shadowed" // + ":" + path
	DetailLauncher              = "launcher"
	DetailTeammateMode          = "teammate_mode"
	DetailModeInProcess         = "mode_in_process" // + ":" + source
	DetailAutoNoPaneTerminal    = "auto_no_pane_terminal"
	DetailAutoFallbackUnguarded = "auto_fallback_unguarded" // + ":" + blockers joined by ","
	DetailModeUnknown           = "mode_unknown"            // + ":" + value
	DetailMissingName           = "missing_name"
	DetailModelParam            = "model_param"
	DetailIsolation             = "isolation"
	DetailCwd                   = "cwd"
	DetailCCFleetNotOnPath      = "cc_fleet_not_on_path" // emitted only by hooks/teammate-guard.sh
)

// Warnings.
const (
	WarnLauncherBinaryDiffers = "launcher_binary_differs"
	WarnAutoMayFallback       = "auto_may_fallback"
	WarnProviderProbe         = "provider_probe_warn"
)

// Sentinel blockers: settings that disable the in-process safety net.
const (
	BlockerSubagentModelForce = "subagent_model_force"
	BlockerAvailableModels    = "available_models"
	BlockerFallbackModel      = "fallback_model"
	BlockerAgentDefNewer      = "agent_def_newer_than_lead"
)

// teammateMode sources.
const (
	SourceCLI     = "cli"
	SourcePolicy  = "policySettings"
	SourceFlag    = "flagSettings"
	SourceLocal   = "localSettings"
	SourceProject = "projectSettings"
	SourceUser    = "userSettings"
	SourceGlobal  = "globalConfig"
	SourceDefault = "default"
)

// backend_hint values.
const (
	HintTmux                 = "tmux"
	HintITerm2OrTmuxExternal = "iterm2-or-tmux-external"
	HintTmuxExternal         = "tmux-external"
)

type Slot string

const (
	SlotDefault Slot = "default"
	SlotStrong  Slot = "strong"
	SlotFast    Slot = "fast"
)

// AgentType is ccf-<provider>[.strong|.fast].
type AgentType struct {
	Provider string
	Slot     Slot
}

// String returns "ccf-<p>", "ccf-<p>.strong" or "ccf-<p>.fast"; the zero Slot
// renders like SlotDefault.
func (a AgentType) String() string {
	if a.Slot == "" || a.Slot == SlotDefault {
		return TypePrefix + a.Provider
	}
	return TypePrefix + a.Provider + "." + string(a.Slot)
}

// ParseAgentType:
//   - ours=false: s does not start with "ccf-"; it belongs to the native path.
//   - ours=true, err!=nil: the prefix is ccf- but the slot is not strong/fast,
//     the provider name fails ids.ValidateProviderName, or it is the reserved
//     name "claude". Callers report BAD_ARGS.
//
// The slot is what follows the last "."; without one it is SlotDefault. t is
// the zero value whenever err != nil.
func ParseAgentType(s string) (t AgentType, ours bool, err error) {
	rest, ok := strings.CutPrefix(s, TypePrefix)
	if !ok {
		return AgentType{}, false, nil
	}
	provider, slot := rest, SlotDefault
	if i := strings.LastIndex(rest, "."); i >= 0 {
		provider = rest[:i]
		switch suffix := Slot(rest[i+1:]); suffix {
		case SlotStrong, SlotFast:
			slot = suffix
		default:
			return AgentType{}, true, fmt.Errorf("agent type %q: slot %q is not %q or %q", s, string(suffix), SlotStrong, SlotFast)
		}
	}
	if err := ids.ValidateProviderName(provider); err != nil {
		return AgentType{}, true, fmt.Errorf("agent type %q: %w", s, err)
	}
	if provider == config.ReservedNativeProvider {
		return AgentType{}, true, fmt.Errorf("agent type %q: %q is reserved for native Claude teammates", s, provider)
	}
	return AgentType{Provider: provider, Slot: slot}, true, nil
}

// FormatFailureLine returns "cc-fleet teammate: <CODE>: <agentID>: <msg> — <suggestion>":
// one line, no trailing newline (CR/LF inside the fields become spaces).
func FormatFailureLine(code, agentID, msg, suggestion string) string {
	return "cc-fleet teammate: " + failureLineField(code) + ": " + failureLineField(agentID) + ": " +
		failureLineField(msg) + " — " + failureLineField(suggestion)
}

// failureLineField flattens line breaks so a failure line always stays on one line.
func failureLineField(s string) string {
	return strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s)
}

// failureLineRe is the failure-line grammar ParseFailureLine matches.
var failureLineRe = regexp.MustCompile(`^cc-fleet teammate: ([A-Z_]+): ([^:\s]+): `)

// ParseFailureLine matches one pane line against ^cc-fleet teammate: ([A-Z_]+): ([^:\s]+): .
// Pane text comes from tmux.Server.CaptureJoined (capture-pane -p -J -S -200),
// so lines tmux wrapped are already joined back.
func ParseFailureLine(line string) (code, agentID string, ok bool) {
	m := failureLineRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// AgentCall is every tool_input field of PreToolUse(Agent) cc-fleet reads;
// prompt and description are deliberately never decoded.
type AgentCall struct {
	Name         string `json:"name"`
	SubagentType string `json:"subagent_type"`
	Model        string `json:"model"`
	Isolation    string `json:"isolation"`
	Cwd          string `json:"cwd"`
}

type Input struct {
	Provider  string     // check: the positional (may be empty); guard: parsed from SubagentType
	Slot      Slot       // check: --slot; guard: parsed from the type
	AgentCall *AgentCall // set only by guard
	Prepare   bool       // check: run the prepare steps (binary, profile, proxy daemon)
	Probe     bool       // check: probe the provider (false with --no-probe)
}

// Result is the `teammate check` JSON envelope and the guard verdict.
type Result struct {
	OK                 bool     `json:"ok"`
	Protocol           int      `json:"protocol"`
	Provider           string   `json:"provider,omitempty"`
	Slot               Slot     `json:"slot,omitempty"`
	Model              string   `json:"model,omitempty"`
	AgentType          string   `json:"agent_type,omitempty"`
	Team               string   `json:"team,omitempty"`
	LeadPID            int      `json:"lead_pid,omitempty"`
	CCVersion          string   `json:"cc_version,omitempty"`
	Entrypoint         string   `json:"entrypoint,omitempty"`
	TeammateMode       string   `json:"teammate_mode,omitempty"`
	TeammateModeSource string   `json:"teammate_mode_source,omitempty"`
	BackendHint        string   `json:"backend_hint,omitempty"`
	Launcher           string   `json:"launcher,omitempty"`
	Warnings           []string `json:"warnings"` // never nil
	ErrorCode          string   `json:"error_code,omitempty"`
	Detail             string   `json:"detail,omitempty"`
	ErrorMsg           string   `json:"error_msg,omitempty"`
	Suggestion         string   `json:"suggestion,omitempty"`
}

// SyncResult is returned by SyncAgentDefs / RemoveAgentDefs and shared by
// setup, repair and userops. Elements are file names (ccf-p.md).
type SyncResult struct {
	Written   []string `json:"written"`
	Removed   []string `json:"removed"`
	Unchanged []string `json:"unchanged"`
	Conflicts []string `json:"conflicts,omitempty"` // unmarked files with our name; never touched
}

type SetupOptions struct {
	TeammateMode string // "tmux" | "keep"; the cmd layer maps "not given" to one of them (CLI default keep); the TUI onboarding passes "tmux"
	Force        bool   // replace a TEAMMATE_COMMAND that is not cc-fleet's (never applies to unmarked definitions)
	Remove       bool
}

type SetupResult struct {
	OK                 bool        `json:"ok"`
	Action             string      `json:"action"` // "setup" | "remove"
	Shim               string      `json:"shim,omitempty"`
	SettingsPath       string      `json:"settings_path,omitempty"`
	Changed            []string    `json:"changed"` // settings key paths, e.g. "env.CLAUDE_CODE_TEAMMATE_COMMAND", "teammateMode"
	AgentDefs          *SyncResult `json:"agent_defs,omitempty"`
	TeammateMode       string      `json:"teammate_mode,omitempty"`        // the user-layer value after any write
	TeammateModeSource string      `json:"teammate_mode_source,omitempty"` // "userSettings" | "default"
	ModeWritten        bool        `json:"mode_written"`
	RestartRequired    bool        `json:"restart_required"`
	Warnings           []string    `json:"warnings"`
	ErrorCode          string      `json:"error_code,omitempty"`
	Detail             string      `json:"detail,omitempty"`
	ErrorMsg           string      `json:"error_msg,omitempty"`
	Suggestion         string      `json:"suggestion,omitempty"`
}

type ShimStatus struct {
	Path         string
	Exists       bool
	Managed      bool // contains ShimMarker
	Executable   bool
	Pinned       string // the path on the ccf='…' line
	PinnedExists bool
	PinnedIsSelf bool // os.SameFile(Pinned, os.Executable())
}

type LeadTeam struct {
	Name          string // session-<id8>
	Dir           string
	LeadSessionID string
	LeadCwd       string
	CreatedAt     int64 // unix ms
}
