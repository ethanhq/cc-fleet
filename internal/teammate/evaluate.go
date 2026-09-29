package teammate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/ccver"
	"github.com/ethanhq/cc-fleet/internal/claudebin"
	"github.com/ethanhq/cc-fleet/internal/claudepaths"
	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/leadsession"
	"github.com/ethanhq/cc-fleet/internal/onboarding"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
	"github.com/ethanhq/cc-fleet/internal/profile"
	"github.com/ethanhq/cc-fleet/internal/providerclass"
)

// Seams. Tests replace them: a test has no real lead session, and the probe and
// the proxy daemon would reach the real key store and network.
var (
	evalGOOS        = runtime.GOOS
	detectSessionFn = leadsession.DetectSession
	leadCmdlineFn   = procintrospect.Cmdline
	ensureProxyFn   = codexproxy.EnsureForProvider
	probeFn         = providerclass.Reachability
)

// Evaluate is the shared verdict behind `teammate check` and `teammate guard`
// (steps 0–13). The first failing step decides the
// result. Steps 0–9 only read files; steps 10–12 run only with Prepare and
// step 13 only with Probe.
func Evaluate(ctx context.Context, in Input) Result {
	// 0. Agent call shape (guard only).
	if c := in.AgentCall; c != nil {
		switch {
		case strings.TrimSpace(c.Name) == "":
			return evalFail(CodeBadAgentCall, DetailMissingName,
				"a `ccf-*` agent without `name` would run as an in-process subagent on Claude",
				"pass `name`, or use `cc-fleet subagent` for one-shot work")
		case c.Model != "":
			return evalFail(CodeBadAgentCall, DetailModelParam,
				"`model` would override the provider routing",
				"drop `model`; choose the slot with the type (`ccf-<p>.strong` / `.fast`)")
		case c.Isolation != "":
			return evalFail(CodeBadAgentCall, DetailIsolation, "`isolation` does not start a teammate", "drop `isolation`")
		case c.Cwd != "":
			return evalFail(CodeBadAgentCall, DetailCwd, "`cwd` does not start a teammate", "drop `cwd`")
		}
	}

	// 1. Platform.
	if evalGOOS == "windows" {
		return evalFail(CodeUnsupportedOnWindows, "",
			"provider teammates need tmux split panes, which Windows does not have",
			"use `cc-fleet subagent`/`workflow` on Windows")
	}

	// 2. Lead session.
	sess, ok := detectSessionFn()
	if !ok {
		return evalFail(CodeLaneUnavailable, DetailNoLeadSession,
			"not running inside a Claude Code session, so there is no team to host a provider teammate",
			"run this from the lead's Bash tool in a terminal `claude` session, or use `cc-fleet subagent`")
	}

	// 3. Claude Code version.
	if !ccver.AtLeast(sess.Version, MinTeammateCC) {
		v := sess.Version
		if v == "" {
			v = "(unknown)"
		}
		return evalFail(CodeLaneUnavailable, DetailCCTooOld,
			fmt.Sprintf("Claude Code %s is older than %s and has no native teammate launcher", v, MinTeammateCC),
			"run `claude update`, or stay on cc-fleet 0.3.x for the old teammate flow")
	}

	// 4. Session team (positive evidence only).
	team, ok := FindLeadTeam(sess)
	if !ok {
		ep := sess.Entrypoint
		if ep == "" {
			ep = "unknown"
		}
		// Not by entrypoint: a terminal lead can inherit the desktop app's.
		return evalFail(CodeLaneUnavailable, DetailNoSessionTeam,
			fmt.Sprintf("this Claude Code session (entrypoint=%s) has no agent team cc-fleet can identify: the desktop app, -p and SDK sessions never get one; a terminal session has none while agent teams are off, and after /clear or resume its team cannot always be told apart from another lead's in the same directory", ep),
			"in a terminal inside tmux or iTerm2, start a new `claude` (after `cc-fleet teammate setup` if agent teams are off); use `cc-fleet subagent`/`workflow` meanwhile")
	}

	// 5. Server-side kill switch.
	if zincHarborOn() {
		return evalFail(CodeLaneUnavailable, DetailDisabledByCC,
			"Claude Code has turned named teammates off for this account",
			"use `cc-fleet subagent`/`workflow`")
	}

	// 6. Launcher.
	warnings := []string{}
	launcher := os.Getenv(EnvTeammateCommand)
	if launcher == "" {
		if v, present, _ := onboarding.SettingsString(claudepaths.Settings(), "env", EnvTeammateCommand); present && IsOurShim(v) {
			return evalFail(CodeLeadRestart, DetailLauncher,
				"the launcher was configured after this claude session started",
				"restart claude (exit, then relaunch inside tmux)")
		}
		return evalFail(CodeSetupRequired, DetailLauncherNotConfigured,
			"the cc-fleet teammate launcher is not configured",
			"run `cc-fleet teammate setup`, then restart claude")
	}
	if !IsOurShim(launcher) {
		return evalFail(CodeSetupRequired, DetailLauncherForeign,
			fmt.Sprintf("%s points at %s, not cc-fleet's launcher", EnvTeammateCommand, launcher),
			"remove that setting, or run `cc-fleet teammate setup --force`")
	}
	st := InspectShim(launcher)
	if !st.Exists || !st.Executable || !st.Managed || st.Pinned == "" || !st.PinnedExists {
		return evalFail(CodeSetupRequired, DetailShimBroken,
			fmt.Sprintf("the launcher %s is missing, not executable, or pins a cc-fleet binary that no longer exists", launcher),
			"run `cc-fleet repair`")
	}
	if !st.PinnedIsSelf {
		warnings = append(warnings, WarnLauncherBinaryDiffers)
	}

	// 7. Provider, slot, and agent definition.
	requested := in.Provider
	if requested == config.ReservedNativeProvider {
		return evalFail(CodeBadArgs, "",
			"`claude` is reserved for native Claude teammates",
			"use a native agent type; cc-fleet is not needed")
	}
	slot := in.Slot
	if slot == "" {
		slot = SlotDefault
	}
	if slot != SlotDefault && slot != SlotStrong && slot != SlotFast {
		return evalFail(CodeBadArgs, "",
			fmt.Sprintf("slot %q is not default, strong or fast", string(slot)),
			"pass --slot default|strong|fast")
	}
	cfg, err := config.Load()
	if err != nil {
		return evalFail(CodeConfigLoadFailed, "", fmt.Sprintf("load providers.toml: %v", err),
			"fix ~/.config/cc-fleet/providers.toml (cc-fleet doctor shows the error), then retry")
	}
	name, _, err := cfg.ResolveProvider(requested)
	if err != nil {
		return evalFail(config.ProviderErrorCode(err), "", err.Error(),
			"name a provider (`cc-fleet teammate check <provider>`), or set one with `cc-fleet default <provider>`")
	}
	if err := ids.ValidateProviderName(name); err != nil {
		return evalFail(CodeBadArgs, "", err.Error(), "use a provider name from `cc-fleet list`")
	}
	v := cfg.Providers[name]
	if v == nil {
		return evalFail(CodeUnknownProvider, "", fmt.Sprintf("provider %q not in providers.toml", name),
			"run `cc-fleet add <provider>` (or check `cc-fleet list --json`)")
	}
	if !v.Enabled {
		return evalFail(CodeProviderDisabled, "", fmt.Sprintf("provider %q is disabled in providers.toml", name),
			"run `cc-fleet edit "+name+" --enable`")
	}
	model := v.ResolveModel(string(slot))
	typ := AgentType{Provider: name, Slot: slot}
	// check picks the type; a slot whose model equals the default has no
	// definition of its own (agentdefs), so it uses the default type.
	if in.AgentCall == nil && slot != SlotDefault && config.Strip1M(model) == config.Strip1M(v.DefaultModel) {
		typ.Slot = SlotDefault
	}
	agentType := typ.String()
	defPath := filepath.Join(claudepaths.Agents(), agentType+".md")
	if _, err := os.Lstat(defPath); err != nil {
		return evalFail(CodeSetupRequired, DetailAgentDefMissing,
			fmt.Sprintf("agent type %s is not installed by cc-fleet", agentType),
			"run `cc-fleet repair`")
	}
	if !IsManagedDef(defPath) {
		return evalFail(CodeSetupRequired, DetailAgentDefForeign,
			fmt.Sprintf("%s exists but is not managed by cc-fleet, so it was left untouched", defPath),
			"rename or remove it, then rerun `cc-fleet teammate setup`")
	}
	if shadows := FindShadowingDefs(sess.Cwd, agentType); len(shadows) > 0 {
		return evalFail(CodeSetupRequired, DetailAgentDefShadowed+":"+shadows[0],
			fmt.Sprintf("a project agent definition %s shadows %s and would bypass cc-fleet", shadows[0], agentType),
			"rename or remove that file")
	}

	// 8. teammateMode written by cc-fleet after the lead started.
	if state, _ := onboarding.LoadState(); state.TeammateLane.ModeWrittenAt > sess.StartedAt {
		return evalFail(CodeLeadRestart, DetailTeammateMode,
			"`teammateMode` was changed after this claude session started",
			"restart claude inside tmux")
	}

	// 9. Effective teammateMode and backend.
	argv, _ := leadCmdlineFn(sess.PID)
	mode, modeFile := resolveMode(ModeInput{
		LeadArgv:      argv,
		LeadCwd:       sess.Cwd,
		LeadStartedAt: sess.StartedAt,
		AgentType:     agentType,
		Getenv:        os.Getenv,
	})
	if mode.Code != "" {
		msg, sugg := modeFailureText(mode)
		return evalFail(mode.Code, mode.Detail, msg, sugg)
	}
	// The lead read its mode at startup. When the sentinel cannot catch an
	// in-process teammate, a mode file edited since then is not trusted.
	if len(mode.Blockers) > 0 && modeFile != "" {
		if fi, err := os.Stat(modeFile); err == nil && fi.ModTime().UnixMilli() > sess.StartedAt {
			return evalFail(CodeLeadRestart, DetailTeammateMode,
				"`teammateMode` was changed after this claude session started",
				"restart claude inside tmux")
		}
	}
	if mode.Warning != "" {
		warnings = append(warnings, mode.Warning)
	}

	// 10–12. Prepare (check only).
	if in.Prepare {
		if _, err := claudebin.ForSession(sess.Version); err != nil {
			return evalFail(CodeClaudeNotFound, "",
				fmt.Sprintf("cannot find the claude binary for this session (version %s)", sess.Version),
				"install Claude Code or put `claude` on PATH, then retry")
		}
		if _, err := profile.WriteForProvider(v, ""); err != nil {
			return evalFail(CodeProfileWriteFailed, "",
				fmt.Sprintf("could not write the profile for %s: %v", name, err),
				"check permissions on `~/.claude/profiles`, then run `cc-fleet repair`")
		}
		if err := ensureProxyFn(v, nil); err != nil {
			return evalFail(CodeCodexProxyUnavailable, "",
				fmt.Sprintf("the local proxy for %s could not start: %v", name, err),
				"run `cc-fleet codex-proxy status`, then retry")
		}
	}

	// 13. Reachability probe (check only, unless --no-probe).
	if in.Probe {
		p := probeFn(v)
		if p.Block {
			return evalFail(p.Code, "", p.Msg, p.Suggestion)
		}
		if p.Warn != "" {
			warnings = append(warnings, WarnProviderProbe)
		}
	}

	return Result{
		OK:                 true,
		Protocol:           Protocol,
		Provider:           name,
		Slot:               slot,
		Model:              model,
		AgentType:          agentType,
		Team:               team.Name,
		LeadPID:            sess.PID,
		CCVersion:          sess.Version,
		Entrypoint:         sess.Entrypoint,
		TeammateMode:       mode.Mode,
		TeammateModeSource: mode.Source,
		BackendHint:        mode.BackendHint,
		Launcher:           launcher,
		Warnings:           warnings,
	}
}

// evalFail builds a failed Result; Warnings stays [] in JSON.
func evalFail(code, detail, msg, suggestion string) Result {
	return Result{
		Protocol:   Protocol,
		Warnings:   []string{},
		ErrorCode:  code,
		Detail:     detail,
		ErrorMsg:   msg,
		Suggestion: suggestion,
	}
}

// modeFailureText is the user-facing wording for a ResolveMode refusal.
func modeFailureText(d ModeDecision) (msg, suggestion string) {
	word, arg, _ := strings.Cut(d.Detail, ":")
	switch word {
	case DetailModeInProcess:
		return fmt.Sprintf("this session runs teammates in-process (set by %s); a provider teammate would silently run on Claude", arg),
			"run `cc-fleet teammate setup --teammate-mode tmux` and restart claude inside tmux (or start `claude --teammate-mode tmux`); use `cc-fleet subagent`/`workflow` for now"
	case DetailAutoNoPaneTerminal:
		return "`teammateMode` is auto but this session is not inside tmux or iTerm2, so teammates run in-process",
			"restart claude inside tmux, or start `claude --teammate-mode tmux`"
	case DetailAutoFallbackUnguarded:
		return fmt.Sprintf("`teammateMode` is auto and %s disables the in-process safety net", arg),
			"set `teammateMode` to tmux (`cc-fleet teammate setup --teammate-mode tmux`) or start `claude --teammate-mode tmux`"
	default:
		return fmt.Sprintf("`teammateMode` %q is not a value cc-fleet understands", d.Mode),
			"set it to tmux, auto or in-process"
	}
}

// zincHarborOn reports whether Claude Code's cached feature flag
// tengu_zinc_harbor (named teammates off) is exactly true. A missing or
// unparseable global config reads as off.
func zincHarborOn() bool {
	p := claudepaths.GlobalConfig()
	if p == "" {
		return false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	var gc struct {
		Features struct {
			ZincHarbor json.RawMessage `json:"tengu_zinc_harbor"`
		} `json:"cachedGrowthBookFeatures"`
	}
	if json.Unmarshal(data, &gc) != nil {
		return false
	}
	return string(gc.Features.ZincHarbor) == "true"
}
