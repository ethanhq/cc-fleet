package teammate

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ethanhq/cc-fleet/internal/claudepaths"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/onboarding"
)

// Setup enables (or with opts.Remove, removes) the provider-teammate lane.
// It never prompts: consent is the caller's job. Install writes,
// in order, the shim, the agent definitions, the user settings keys and the
// lane state, stopping at the first failure; a rerun is idempotent and
// completes the rest. Remove deletes the launcher setting (only when it names
// our shim) and the managed definitions, and marks the lane disabled; the shim
// file, AGENT_TEAMS and teammateMode are kept.
func Setup(opts SetupOptions) SetupResult {
	action := "setup"
	if opts.Remove {
		action = "remove"
	}
	if runtime.GOOS == "windows" {
		return setupFail(action, CodeUnsupportedOnWindows, "",
			"provider teammates need tmux split panes, which Windows does not have",
			"use `cc-fleet subagent`/`workflow` on Windows")
	}
	settings := claudepaths.Settings()
	shim, err := ShimPath()
	if settings == "" || err != nil {
		return setupFail(action, CodeInternal, "",
			"cannot resolve the Claude Code or cc-fleet config directory (is HOME set?)",
			"set HOME, then rerun `cc-fleet teammate setup`")
	}
	if opts.Remove {
		return setupRemove(settings, shim)
	}
	return setupInstall(opts, settings)
}

func setupInstall(opts SetupOptions, settings string) SetupResult {
	const action = "setup"
	mode := opts.TeammateMode
	if mode == "" {
		mode = "keep"
	}
	if mode != "tmux" && mode != "keep" {
		return setupFail(action, CodeBadArgs, "",
			fmt.Sprintf("--teammate-mode %q is not tmux or keep", opts.TeammateMode),
			"pass --teammate-mode tmux or --teammate-mode keep")
	}

	cfg, err := config.Load()
	if err != nil {
		return setupFail(action, CodeConfigLoadFailed, "", fmt.Sprintf("load providers.toml: %v", err),
			"fix ~/.config/cc-fleet/providers.toml (cc-fleet doctor shows the error), then retry")
	}

	launcher, _, err := onboarding.SettingsString(settings, "env", EnvTeammateCommand)
	if err != nil {
		return setupSettingsFail(action, settings, err)
	}
	userMode, modePresent, err := onboarding.SettingsString(settings, KeyTeammateMode)
	if err != nil {
		return setupSettingsFail(action, settings, err)
	}
	agentTeams, _, err := onboarding.SettingsString(settings, "env", EnvAgentTeams)
	if err != nil {
		return setupSettingsFail(action, settings, err)
	}

	if launcher != "" && !IsOurShim(launcher) && !opts.Force {
		return setupFail(action, CodeSetupConflict, DetailLauncherForeign,
			fmt.Sprintf("%s is already set to %s", EnvTeammateCommand, launcher),
			"rerun with --force to replace it")
	}
	// --force never overrides a foreign definition: it is checked before any write.
	for _, tg := range agentDefTargets(cfg) {
		p := filepath.Join(claudepaths.Agents(), tg.file)
		if _, err := os.Lstat(p); err == nil && !IsManagedDef(p) {
			return setupDefConflict(action, p)
		}
	}

	shim, _, err := WriteShim("")
	if err != nil {
		return setupFail(action, CodeInternal, "", err.Error(), "fix the permissions of the cc-fleet config directory, then rerun")
	}
	defs, err := SyncAgentDefs(cfg)
	if err != nil {
		return setupFail(action, CodeInternal, "", err.Error(), "fix the permissions of the Claude Code agents directory, then rerun")
	}
	if len(defs.Conflicts) > 0 {
		return setupDefConflict(action, filepath.Join(claudepaths.Agents(), defs.Conflicts[0]))
	}

	edits := []onboarding.SettingsEdit{{Path: []string{"env", EnvTeammateCommand}, Value: shim}}
	if !envTruthy(agentTeams) {
		edits = append(edits, onboarding.SettingsEdit{Path: []string{"env", EnvAgentTeams}, Value: "1"})
	}
	if mode == "tmux" && (userMode == "" || userMode == "in-process") {
		edits = append(edits, onboarding.SettingsEdit{Path: []string{KeyTeammateMode}, Value: "tmux"})
	}
	changed, err := onboarding.EditSettings(settings, edits)
	if err != nil {
		return setupEditFail(action, settings, err)
	}
	if changed == nil {
		changed = []string{}
	}
	modeWritten := slices.Contains(changed, KeyTeammateMode)

	st, _ := onboarding.LoadState() // a corrupt state file reads as zero and is rewritten
	if !st.TeammateLane.Enabled || modeWritten {
		st.TeammateLane.Enabled = true
		if modeWritten {
			st.TeammateLane.ModeWrittenAt = time.Now().UnixMilli()
		}
		if err := st.Save(); err != nil {
			return setupFail(action, CodeInternal, "", fmt.Sprintf("save onboarding state: %v", err),
				"fix the permissions of the cc-fleet config directory, then rerun")
		}
	}

	res := SetupResult{
		OK:                 true,
		Action:             action,
		Shim:               shim,
		SettingsPath:       settings,
		Changed:            changed,
		AgentDefs:          &defs,
		TeammateMode:       userMode,
		TeammateModeSource: SourceUser,
		ModeWritten:        modeWritten,
		RestartRequired:    modeWritten || slices.Contains(changed, "env."+EnvTeammateCommand),
		Warnings:           []string{},
	}
	switch {
	case modeWritten:
		res.TeammateMode = "tmux"
	case !modePresent || userMode == "":
		// Nothing at the user layer: Claude Code's default applies.
		res.TeammateMode, res.TeammateModeSource = "in-process", SourceDefault
	}
	return res
}

func setupRemove(settings, shim string) SetupResult {
	const action = "remove"
	launcher, present, err := onboarding.SettingsString(settings, "env", EnvTeammateCommand)
	if err != nil {
		return setupSettingsFail(action, settings, err)
	}
	changed := []string{}
	if present && IsOurShim(launcher) {
		c, err := onboarding.EditSettings(settings, []onboarding.SettingsEdit{
			{Path: []string{"env", EnvTeammateCommand}, Delete: true},
		})
		if err != nil {
			return setupEditFail(action, settings, err)
		}
		changed = append(changed, c...)
	}
	defs, err := RemoveAgentDefs()
	if err != nil {
		return setupFail(action, CodeInternal, "", err.Error(), "fix the permissions of the Claude Code agents directory, then rerun")
	}
	if st, _ := onboarding.LoadState(); st.TeammateLane.Enabled {
		st.TeammateLane.Enabled = false
		if err := st.Save(); err != nil {
			return setupFail(action, CodeInternal, "", fmt.Sprintf("save onboarding state: %v", err),
				"fix the permissions of the cc-fleet config directory, then rerun")
		}
	}
	return SetupResult{
		OK:           true,
		Action:       action,
		Shim:         shim,
		SettingsPath: settings,
		Changed:      changed,
		AgentDefs:    &defs,
		// Running leads keep the old env until they restart.
		RestartRequired: len(changed) > 0,
		Warnings:        []string{},
	}
}

// NeedsSetupNudge reports whether the TUI should suggest `teammate setup`: not
// Windows, the lane is off, the user has not dismissed the nudge, and at least
// one enabled provider exists.
func NeedsSetupNudge() bool {
	if runtime.GOOS == "windows" {
		return false
	}
	st, _ := onboarding.LoadState()
	if st.TeammateLane.Enabled || st.TeammateLaneAck {
		return false
	}
	cfg, err := config.Load()
	if err != nil {
		return false
	}
	for name, p := range cfg.Providers {
		if p != nil && p.Enabled && name != config.ReservedNativeProvider {
			return true
		}
	}
	return false
}

// AgentDefsDrift reports, without writing, how the agents directory differs
// from what SyncAgentDefs(cfg) would leave: stale lists the wanted definitions
// that are missing or outdated plus the managed ccf-*.md files no longer
// wanted; foreign lists the ccf-*.md files that carry no marker. doctor uses it.
func AgentDefsDrift(cfg *config.Config) (stale, foreign []string) {
	dir := claudepaths.Agents()
	if dir == "" || cfg == nil {
		return nil, nil
	}
	want := map[string]bool{}
	for _, tg := range agentDefTargets(cfg) {
		want[tg.file] = true
		have, err := os.ReadFile(filepath.Join(dir, tg.file))
		if err != nil || !bytes.Equal(have, tg.data) {
			stale = append(stale, tg.file)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, TypePrefix+"*.md"))
	for _, p := range matches {
		file := filepath.Base(p)
		switch {
		case !IsManagedDef(p):
			foreign = append(foreign, file)
		case !want[file]:
			stale = append(stale, file)
		}
	}
	return stale, foreign
}

// envTruthy mirrors Claude Code's truthy env check (1, true, yes, on).
func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func setupFail(action, code, detail, msg, suggestion string) SetupResult {
	return SetupResult{
		Action:     action,
		Changed:    []string{},
		Warnings:   []string{},
		ErrorCode:  code,
		Detail:     detail,
		ErrorMsg:   msg,
		Suggestion: suggestion,
	}
}

func setupDefConflict(action, path string) SetupResult {
	return setupFail(action, CodeSetupConflict, DetailAgentDefForeign,
		fmt.Sprintf("%s exists and is not managed by cc-fleet", path),
		"rename or remove it, then rerun")
}

// setupSettingsFail classifies an onboarding.SettingsString error: a file that
// cannot be read is INTERNAL, anything else (bad JSON, wrong shape) is unparseable.
func setupSettingsFail(action, settings string, err error) SetupResult {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return setupFail(action, CodeInternal, "", fmt.Sprintf("read or write %s: %v", settings, err),
			"fix the file's permissions, then rerun `cc-fleet teammate setup`")
	}
	return setupFail(action, CodeSettingsUnparseable, "", fmt.Sprintf("%s is not a JSON object (%v)", settings, err),
		"fix the file, then rerun `cc-fleet teammate setup`")
}

// setupEditFail classifies an onboarding.EditSettings error. The file was
// already parsed by SettingsString, so only a shape error is unparseable.
func setupEditFail(action, settings string, err error) SetupResult {
	if errors.Is(err, onboarding.ErrSettingsShape) {
		return setupSettingsFail(action, settings, err)
	}
	return setupFail(action, CodeInternal, "", fmt.Sprintf("write %s: %v", settings, err),
		"fix the file's permissions, then rerun `cc-fleet teammate setup`")
}
