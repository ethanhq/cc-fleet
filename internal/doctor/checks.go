package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ethanhq/cc-fleet/internal/ccver"
	"github.com/ethanhq/cc-fleet/internal/claudebin"
	"github.com/ethanhq/cc-fleet/internal/claudepaths"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/homedir"
	"github.com/ethanhq/cc-fleet/internal/models"
	"github.com/ethanhq/cc-fleet/internal/onboarding"
	"github.com/ethanhq/cc-fleet/internal/teammate"
	"github.com/ethanhq/cc-fleet/internal/tmux"
	"github.com/ethanhq/cc-fleet/internal/version"
)

// providerProbeTimeout caps each provider's /v1/models probe in check 6 at
// 3s/provider so the total check time stays bounded even with several providers
// configured.
const providerProbeTimeout = 3 * time.Second

// (There is deliberately no agent-teams detector here: agent-teams is a Claude
// runtime state set by GrowthBook, invisible to an external process. The env
// var is an unreliable proxy that misfires for the common default-on case, so
// cc-fleet does not detect it. Whether teammate mode is usable is decided by
// the skill from Claude's own tool availability.)

// homeDir resolves the user's home directory. homedir.Home reads $HOME on unix
// (so tests driving t.Setenv("HOME", tempDir) stay hermetic) and %USERPROFILE%
// on windows, where HOME is not a native variable.
func homeDir() (string, error) {
	return homedir.Home()
}

// claudeDir returns $HOME/.claude — Claude Code's per-user config root that
// most checks below inspect (settings.json, profiles/, skills/, credentials).
func claudeDir() (string, error) {
	h, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".claude"), nil
}

// CheckSettingsJSON is check 1: ~/.claude/settings.json exists and parses as
// JSON. We deliberately do NOT validate schema (Claude Code owns that) — a
// well-formed but empty object {} passes.
func CheckSettingsJSON() CheckResult {
	r := CheckResult{ID: 1, Title: "~/.claude/settings.json exists and is valid JSON"}
	cdir, err := claudeDir()
	if err != nil {
		r.Status = StatusFail
		r.Detail = err.Error()
		return r
	}
	path := filepath.Join(cdir, "settings.json")
	info, err := os.Stat(path)
	if err != nil {
		r.Status = StatusFail
		if errors.Is(err, os.ErrNotExist) {
			r.Detail = fmt.Sprintf("not found: %s", path)
			r.FixHint = "create ~/.claude/settings.json (Claude Code auto-creates it on first run)"
		} else {
			r.Detail = err.Error()
		}
		return r
	}
	if info.IsDir() {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("%s is a directory, not a file", path)
		return r
	}
	data, err := os.ReadFile(path)
	if err != nil {
		r.Status = StatusFail
		r.Detail = err.Error()
		return r
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("%s: invalid JSON: %s", path, err.Error())
		return r
	}
	r.Status = StatusOK
	r.Detail = path
	return r
}

// CheckProfilesDirWritable is check 2: ~/.claude/profiles/ is writable when it
// exists. A missing dir is healthy — profile writes MkdirAll it on demand, so a
// fresh machine with no providers passes. Permission problems surface a hint.
func CheckProfilesDirWritable() CheckResult {
	r := CheckResult{ID: 2, Title: "~/.claude/profiles/ writable"}
	cdir, err := claudeDir()
	if err != nil {
		r.Status = StatusFail
		r.Detail = err.Error()
		return r
	}
	path := filepath.Join(cdir, "profiles")
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Healthy only when it CAN be created on demand — an unwritable
		// ancestor would make the eventual MkdirAll fail at provider add.
		if perr := probeCreatable(path); perr != nil {
			r.Status = StatusFail
			r.Detail = fmt.Sprintf("not created yet and cannot be: %v", perr)
			r.FixHint = fmt.Sprintf("fix permissions so %s can be created", path)
			return r
		}
		r.Status = StatusOK
		r.Detail = fmt.Sprintf("not created yet (made on first provider use): %s", path)
		return r
	case err != nil:
		r.Status = StatusFail
		r.Detail = err.Error()
		return r
	case !info.IsDir():
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("%s is not a directory", path)
		return r
	}
	// Probe by creating a temp file and deleting it.
	probe := filepath.Join(path, ".cc-fleet-doctor.writetest")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("%s: not writable: %s", path, err.Error())
		r.FixHint = fmt.Sprintf("chmod u+w %s", path)
		return r
	}
	_ = f.Close()
	if err := os.Remove(probe); err != nil {
		// Created but couldn't remove — odd, surface as warn-ish fail with
		// the leftover path so the user can clean it up themselves.
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("%s: created probe %s but cannot remove: %s", path, probe, err.Error())
		return r
	}
	r.Status = StatusOK
	r.Detail = path
	return r
}

// probeCreatable verifies a not-yet-existing dir could be MkdirAll'd: it walks
// to the nearest existing ancestor and creates + removes a transient probe dir
// there.
func probeCreatable(path string) error {
	anc := filepath.Dir(path)
	for {
		if _, err := os.Stat(anc); err == nil {
			break
		}
		parent := filepath.Dir(anc)
		if parent == anc {
			break
		}
		anc = parent
	}
	probe, err := os.MkdirTemp(anc, ".cc-fleet-doctor.")
	if err != nil {
		return err
	}
	if err := os.Remove(probe); err != nil {
		return fmt.Errorf("created probe %s but cannot remove: %w", probe, err)
	}
	return nil
}

// CheckTmuxInstalled is check 3: tmux is on PATH and `tmux -V` runs. tmux is
// needed only for live teammate panes; subagent / workflow / run all work
// without it — so a missing tmux is a Warn (Optional group), not a Fail, and
// never flips doctor's overall OK.
func CheckTmuxInstalled() CheckResult {
	r := CheckResult{ID: 3, Title: "tmux installed (live teammates only)"}
	out, err := exec.Command("tmux", "-V").Output()
	if err != nil {
		r.Status = StatusWarn
		r.Detail = "not found — needed only for live teammate panes; subagent / workflow / run work without it"
		r.FixHint = "for live teammates, install tmux: " + onboarding.TmuxInstallHint()
		return r
	}
	r.Status = StatusOK
	r.Detail = strings.TrimSpace(string(out))
	return r
}

// CheckClaudeBinary is check 4: a `claude` binary is resolvable the way every
// lane resolves it (claudebin) and, best effort, its version is known. An
// unknown version is only a Warn. A version below teammate.MinTeammateCC only
// adds a note: the other lanes still work.
func CheckClaudeBinary() CheckResult {
	r := CheckResult{ID: 4, Title: "claude binary present; version known"}
	path, version, err := claudebin.Resolve()
	if err != nil {
		r.Status = StatusFail
		r.Detail = err.Error()
		r.FixHint = "install Claude Code (see https://docs.anthropic.com/claude-code)"
		return r
	}
	r.Detail = ccver.String(path, version)
	if version == "" {
		// Path resolved but `--version` didn't produce a parseable token.
		// Treat as Warn so doctor still reports OK=true overall.
		r.Status = StatusWarn
		return r
	}
	r.Status = StatusOK
	if !ccver.AtLeast(version, teammate.MinTeammateCC) {
		r.Detail += "; provider teammates need ≥ " + teammate.MinTeammateCC
	}
	return r
}

// CheckAttachedTmux is check 5: at least one attached tmux session exists (where
// a lead started inside tmux splits its teammate panes). This is only a Warn,
// not a Fail: outside tmux Claude Code runs its own external swarm session, so
// an attached session is not mandatory. A Warn leaves DoctorResult.OK true.
func CheckAttachedTmux() CheckResult {
	r := CheckResult{ID: 5, Title: "at least one attached tmux session"}
	panes, err := tmux.ListPanes()
	if err != nil {
		// `tmux list-panes -a` exits non-zero when no server is running. We
		// surface that as the "no sessions" case (with the actual error text
		// in detail) since both have the same user-facing fix.
		r.Status = StatusWarn
		r.Detail = fmt.Sprintf("tmux list-panes: %s", err.Error())
		r.FixHint = "start claude inside tmux (tmux new-session -A -s main) for split-pane teammates; outside tmux Claude Code manages its own swarm session"
		return r
	}
	attachedSessions := map[string]struct{}{}
	for _, p := range panes {
		if p.Attached {
			attachedSessions[p.SessionName] = struct{}{}
		}
	}
	if len(attachedSessions) == 0 {
		r.Status = StatusWarn
		r.Detail = "no attached tmux session"
		r.FixHint = "attach a tmux session (tmux attach -t main) and start claude there for split-pane teammates; outside tmux Claude Code manages its own swarm session"
		return r
	}
	names := make([]string, 0, len(attachedSessions))
	for n := range attachedSessions {
		names = append(names, n)
	}
	sort.Strings(names)
	r.Status = StatusOK
	r.Detail = fmt.Sprintf("%d attached session(s): %s", len(names), strings.Join(names, ", "))
	return r
}

// CheckProviderKeys is check 6: every enabled provider's /v1/models endpoint
// answers within providerProbeTimeout. Each provider is probed with its own
// 3s-bounded context so a slow provider can't drag the rest down.
//
// "Enabled" means Provider.Enabled = true in providers.toml. Disabled providers are
// reported in the detail but not probed. A missing providers.toml is OK (returns
// an empty Config) — the check just reports "no providers configured".
func CheckProviderKeys() CheckResult {
	r := CheckResult{ID: 6, Title: "all configured providers' keys reachable"}
	cfg, err := config.Load()
	if err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("load providers.toml: %s", err.Error())
		return r
	}
	if len(cfg.Providers) == 0 {
		// No providers configured — nothing to probe; surface as OK with a
		// hint so the user knows the check did consider its inputs.
		r.Status = StatusOK
		r.Detail = "no providers configured"
		return r
	}

	// Deterministic order so detail text doesn't churn between runs.
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)

	var (
		probed   int
		failures []string
	)
	for _, name := range names {
		v := cfg.Providers[name]
		if !v.Enabled {
			continue
		}
		probed++
		ctx, cancel := context.WithTimeout(context.Background(), providerProbeTimeout)
		_, fetchErr := models.Fetch(ctx, v)
		cancel()
		if fetchErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", name, fetchErr.Error()))
		}
	}
	if probed == 0 {
		r.Status = StatusOK
		r.Detail = "no enabled providers"
		return r
	}
	if len(failures) > 0 {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("%d/%d provider(s) failed: %s",
			len(failures), probed, strings.Join(failures, "; "))
		r.FixHint = "verify the provider's API key (cc-fleet keyget <provider>) and base_url"
		return r
	}
	r.Status = StatusOK
	r.Detail = fmt.Sprintf("%d provider(s) reachable", probed)
	return r
}

// CheckSkillInstalled is check 7: the cc-fleet skill is installed at
// ~/.claude/skills/cc-fleet/SKILL.md (the manual `make install-skill`
// channel). We deliberately don't validate SKILL.md content; existence is enough.
//
// skillLanes are the per-lane skill directory basenames (the plugin ships these
// three; the global install prefixes them, see globalSkillDir).
var skillLanes = []string{"subagent", "team", "workflow"}

// sharedDirName is the namespaced shared-doc dir the lane skills link
// (../cc-fleet-shared/<doc>); sharedDocs are the docs every lane depends on.
const sharedDirName = "cc-fleet-shared"

var sharedDocs = []string{"cli-reference.md", "providers.md", "routing.md", "troubleshooting.md"}

// The skill ships through two channels AND two layouts:
//   - the per-lane layout (subagent / team / workflow) — the current form, OK only
//     when ALL THREE are present (a partial install leaves a lane uninvokable);
//   - the legacy single `cc-fleet` skill — a compat OK.
//
// Each layout is checked in both the manual `make install-skill` path
// (~/.claude/skills/) and the cc-fleet plugin cache. Binary and plugin update on
// separate channels, so a new-plugin + old-binary reading either layout as OK is
// the intended behavior. Both layouts present at once is a WARN: the legacy router
// competes with the per-lane skills. A MISSING skill is a WARN (doctor can't
// auto-install — the source lives in the cc-fleet repo).
func CheckSkillInstalled() CheckResult {
	r := CheckResult{ID: 7, Title: "cc-fleet skills installed (per-lane, or legacy single skill)"}
	cdir, err := claudeDir()
	if err != nil {
		r.Status = StatusFail
		r.Detail = err.Error()
		return r
	}
	newRoot, newPrefix, newWhere := perLaneSkillsPath(cdir)
	oldWhere := legacySkillPath(cdir)
	// Coexistence is only a real conflict for a MANUAL ~/.claude/skills/cc-fleet copy
	// (Claude Code loads every dir under skills/, so the old router competes). A legacy
	// skill that only lingers in the plugin cache is a stale, inactive version, not a
	// conflict — so it must not WARN.
	manualLegacy := manualLegacySkillPath(cdir) != ""
	switch {
	case newWhere != "" && manualLegacy:
		r.Status = StatusWarn
		r.Detail = "both the per-lane skills and a legacy ~/.claude/skills/cc-fleet skill are installed — the old router competes"
		r.FixHint = "remove the legacy copy: rm -rf ~/.claude/skills/cc-fleet"
	case newWhere != "" && !sharedDocsPresent(newRoot, newPrefix):
		r.Status = StatusWarn
		r.Detail = "the per-lane skills are installed but the shared docs they link are missing beside them"
		r.FixHint = "re-run `make install-skill` (or reinstall the plugin) so skills/" + sharedDirName + "/ lands next to the lane skills"
	case newWhere != "":
		r.Status = StatusOK
		r.Detail = newWhere
	case oldWhere != "":
		r.Status = StatusOK
		r.Detail = "legacy single skill: " + oldWhere
	default:
		r.Status = StatusWarn
		r.Detail = "no cc-fleet skills found in ~/.claude/skills or the plugin cache"
		r.FixHint = "run `make install-skill`, or install the cc-fleet plugin (/plugin install cc-fleet@<marketplace>)"
	}
	return r
}

// perLaneSkillsPath returns the directory holding the per-lane skills, the lane
// dir-name prefix in that layout, and a display label, when ALL THREE
// (subagent / team / workflow) are present under ONE root — else ("", "", "").
// The global install prefixes the dirs (cc-fleet-<lane>); the plugin ships them
// bare under a single <version> root. The all-three-under-one-root check (not
// three independent globs) means a partial install can't read as healthy.
func perLaneSkillsPath(cdir string) (root, prefix, label string) {
	// Global: ~/.claude/skills/cc-fleet-<lane>/SKILL.md
	if skills := filepath.Join(cdir, "skills"); allLanesPresent(skills, "cc-fleet-") {
		return skills, "cc-fleet-", skills + " (per-lane)"
	}
	// Plugin: ~/.claude/plugins/cache/<marketplace>/cc-fleet/<version>/skills/<lane>/SKILL.md.
	// Glob one lane to enumerate candidate version roots, then require the siblings
	// under the SAME root.
	pattern := filepath.Join(cdir, "plugins", "cache", "*", "cc-fleet", "*", "skills", skillLanes[0], "SKILL.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", "", ""
	}
	for _, m := range matches {
		skillsRoot := filepath.Dir(filepath.Dir(m)) // .../skills
		if allLanesPresent(skillsRoot, "") {
			return skillsRoot, "", skillsRoot + " (plugin, per-lane)"
		}
	}
	return "", "", ""
}

// sharedDocsPresent reports whether every shared doc the INSTALLED lane skills
// link exists beside them under root. The expected dir is read from the skills
// themselves: a lane SKILL.md citing cc-fleet-shared/ requires that dir; an
// older skill set citing shared/ accepts either — so a self-consistent old
// install reads OK, but new skills beside only the legacy dir (every link
// broken) read as missing.
func sharedDocsPresent(root, prefix string) bool {
	dirs := []string{sharedDirName, "shared"}
	if data, err := os.ReadFile(filepath.Join(root, prefix+skillLanes[0], "SKILL.md")); err == nil {
		if strings.Contains(string(data), sharedDirName+"/") {
			dirs = []string{sharedDirName}
		}
	}
	for _, dir := range dirs {
		ok := true
		for _, doc := range sharedDocs {
			if info, err := os.Stat(filepath.Join(root, dir, doc)); err != nil || info.IsDir() {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// allLanesPresent reports whether every skillLane's SKILL.md exists under dir,
// each in a directory named prefix+lane.
func allLanesPresent(dir, prefix string) bool {
	for _, lane := range skillLanes {
		p := filepath.Join(dir, prefix+lane, "SKILL.md")
		if info, err := os.Stat(p); err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

// manualLegacySkillPath returns the manual ~/.claude/skills/cc-fleet/SKILL.md path
// if present, else "" — the only legacy copy that actively competes (Claude Code
// loads every dir under skills/).
func manualLegacySkillPath(cdir string) string {
	manual := filepath.Join(cdir, "skills", "cc-fleet", "SKILL.md")
	if info, err := os.Stat(manual); err == nil && !info.IsDir() {
		return manual
	}
	return ""
}

// legacySkillPath returns the path to a legacy single `cc-fleet` SKILL.md (manual
// or plugin), if one is installed, else "" — used for the compat-OK path (either
// channel counts as "installed").
func legacySkillPath(cdir string) string {
	if m := manualLegacySkillPath(cdir); m != "" {
		return m
	}
	pattern := filepath.Join(cdir, "plugins", "cache", "*", "cc-fleet", "*", "skills", "cc-fleet", "SKILL.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return ""
	}
	for _, m := range matches {
		if info, statErr := os.Stat(m); statErr == nil && !info.IsDir() {
			return m
		}
	}
	return ""
}

// CheckPluginVersionMatch is check 10: the running cc-fleet binary's version
// matches the installed cc-fleet plugin's version. The skill (shipped by the
// plugin) drives the binary, so a large skew can surface stale guidance — a
// mismatch is a WARN with a `ccf update` hint. It is OK when the two match,
// when no plugin is installed (nothing to compare), or when the binary is a
// non-release/dev build (version.IsRelease false → not comparable).
func CheckPluginVersionMatch() CheckResult {
	r := CheckResult{ID: 10, Title: "binary and plugin versions match"}
	cur := version.Resolve()
	if !version.IsRelease(cur) {
		r.Status = StatusOK
		r.Detail = fmt.Sprintf("development build (%s) — not comparable", cur)
		return r
	}
	cdir, err := claudeDir()
	if err != nil {
		// Can't locate ~/.claude — checks 1-2 already cover a broken home dir;
		// this informational check never alarms on its own.
		r.Status = StatusOK
		r.Detail = fmt.Sprintf("could not locate ~/.claude (%s)", err.Error())
		return r
	}
	pv, ok := pluginVersion(cdir)
	if !ok {
		r.Status = StatusOK
		r.Detail = "no cc-fleet plugin installed (nothing to compare)"
		return r
	}
	if version.Normalize(pv) == version.Normalize(cur) {
		r.Status = StatusOK
		r.Detail = fmt.Sprintf("binary %s == plugin %s", cur, pv)
		return r
	}
	r.Status = StatusWarn
	r.Detail = fmt.Sprintf("binary %s != plugin %s", cur, pv)
	r.FixHint = "run `ccf update` to bring the binary and plugin to the same version"
	return r
}

// pluginVersion returns the highest cc-fleet plugin version Claude Code has
// cached, and ok=false if none. Claude Code unpacks marketplace plugins under
// ~/.claude/plugins/cache/<marketplace>/cc-fleet/<version>/, so the <version>
// path segment is the plugin's version; the cache can hold several, so the
// newest comparable release wins (a stale leftover never WARNs against a
// matching current one). A non-release segment is ignored.
func pluginVersion(cdir string) (string, bool) {
	pattern := filepath.Join(cdir, "plugins", "cache", "*", "cc-fleet", "*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", false // only ErrBadPattern, impossible for this fixed pattern
	}
	best := ""
	for _, m := range matches {
		info, statErr := os.Stat(m)
		if statErr != nil || !info.IsDir() {
			continue
		}
		ver := filepath.Base(m)
		if !version.IsRelease(ver) {
			continue
		}
		if best == "" || version.Newer(ver, best) {
			best = ver
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// CheckTeammateLane is check 8 (Optional): the provider-teammate lane that
// `cc-fleet teammate setup` installs. Not set up is healthy — the lane is
// optional. Once enabled, a launcher setting that names a missing or
// non-executable shim Fails (Claude Code runs it for every split-pane teammate,
// native ones too), as does a shim whose pinned cc-fleet is gone; drift that
// only affects provider teammates Warns.
func CheckTeammateLane() CheckResult {
	r := CheckResult{ID: 8, Title: "teammate lane (optional)"}
	st, _ := onboarding.LoadState()
	if !st.TeammateLane.Enabled {
		r.Status = StatusOK
		r.Detail = "not set up (optional) — run: cc-fleet teammate setup"
		return withLegacyFingerprint(r)
	}

	var fails, warns []string
	settings := claudepaths.Settings()
	launcher, _, err := onboarding.SettingsString(settings, "env", teammate.EnvTeammateCommand)
	switch {
	case err != nil:
		fails = append(fails, fmt.Sprintf("cannot read %s: %v", settings, err))
	case launcher == "":
		warns = append(warns, fmt.Sprintf("%s is not set in %s", teammate.EnvTeammateCommand, settings))
	default:
		sh := teammate.InspectShim(launcher)
		switch {
		case !sh.Exists || !sh.Executable:
			fails = append(fails, fmt.Sprintf("launcher %s is missing or not executable — every split-pane teammate fails to start", launcher))
		case !teammate.IsOurShim(launcher):
			warns = append(warns, fmt.Sprintf("%s points at %s, not cc-fleet's launcher", teammate.EnvTeammateCommand, launcher))
		case sh.Pinned == "" || !sh.PinnedExists:
			fails = append(fails, fmt.Sprintf("launcher %s pins a cc-fleet binary that no longer exists (%s)", launcher, sh.Pinned))
		case !sh.PinnedIsSelf:
			warns = append(warns, fmt.Sprintf("launcher pins %s, not this cc-fleet binary", sh.Pinned))
		}
	}

	if cfg, err := config.Load(); err == nil {
		stale, foreign := teammate.AgentDefsDrift(cfg)
		if len(stale) > 0 {
			warns = append(warns, "agent definitions out of sync: "+strings.Join(stale, ", "))
		}
		if len(foreign) > 0 {
			warns = append(warns, "agent definitions not managed by cc-fleet: "+strings.Join(foreign, ", "))
		}
	}
	if mode, _, _ := onboarding.SettingsString(settings, teammate.KeyTeammateMode); mode == "" || mode == "in-process" {
		warns = append(warns, "user teammateMode is unset or in-process, so provider teammates are refused — run: cc-fleet teammate setup --teammate-mode tmux (or start claude --teammate-mode tmux)")
	}
	if v, _, _ := onboarding.SettingsString(settings, "env", teammate.EnvAgentTeams); !envTruthy(v) && !envTruthy(os.Getenv(teammate.EnvAgentTeams)) {
		warns = append(warns, teammate.EnvAgentTeams+" is not turned on")
	}
	if zincHarborOn() {
		warns = append(warns, "Claude Code has turned named teammates off for this account (tengu_zinc_harbor)")
	}
	if os.Getenv(teammate.EnvProcessWrapper) != "" {
		warns = append(warns, teammate.EnvProcessWrapper+" is set; the teammate launcher does not go through it")
	}

	switch {
	case len(fails) > 0:
		r.Status = StatusFail
		r.Detail = strings.Join(append(fails, warns...), "; ")
		r.Fixable = true
		r.FixHint = "run: cc-fleet repair (or cc-fleet teammate setup)"
	case len(warns) > 0:
		r.Status = StatusWarn
		r.Detail = strings.Join(warns, "; ")
		r.FixHint = "run: cc-fleet repair (or cc-fleet teammate setup)"
	default:
		r.Status = StatusOK
		r.Detail = "enabled; launcher " + launcher
	}
	return withLegacyFingerprint(r)
}

// withLegacyFingerprint raises r to at least Warn while the 0.3.x
// <ConfigDir>/fingerprint.json is still on disk; nothing reads it any more.
func withLegacyFingerprint(r CheckResult) CheckResult {
	dir, err := config.ConfigDir()
	if err != nil {
		return r
	}
	p := filepath.Join(dir, "fingerprint.json")
	if _, err := os.Stat(p); err != nil {
		return r
	}
	if r.Status == StatusOK {
		r.Status = StatusWarn
		r.FixHint = "rm " + p
	}
	r.Detail += "; legacy fingerprint.json can be deleted"
	return r
}

// envTruthy mirrors Claude Code's truthy env check (1, true, yes, on).
func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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

// CheckOAuthCredentials is check 9: does the main session have an OAuth /
// subscription credential on disk? **Purely informational** — its purpose is
// to report which auth the MAIN session (and the native `claude` leaf, which
// runs on the same login) can use, NOT to flag a problem. cc-fleet's model is:
// main session on the OAuth subscription, provider teammates on their own API
// key. A main session that runs entirely on a provider profile has no
// credentials.json, and on macOS the login lives in the OS keychain with no
// file at all — so ABSENCE is reported as OK (informational), never a WARN,
// and this check can never flip doctor's overall OK.
func CheckOAuthCredentials() CheckResult {
	r := CheckResult{ID: 9, Title: "OAuth credentials.json exists (informational)"}
	cdir, err := claudeDir()
	if err != nil {
		// Can't even locate ~/.claude — report OK-informational (this check never
		// alarms); checks 1-2 already cover a genuinely broken home dir.
		r.Status = StatusOK
		r.Detail = fmt.Sprintf("could not locate ~/.claude (%s) — informational only", err.Error())
		return r
	}
	// Try both known names. The dot-prefixed one is the current Claude Code
	// default; the unprefixed one is the legacy location some installs still
	// have. Either is fine.
	candidates := []string{
		filepath.Join(cdir, ".credentials.json"),
		filepath.Join(cdir, "credentials.json"),
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			r.Status = StatusOK
			r.Detail = p
			return r
		}
	}
	// Absent is fine and NOT a warning: provider teammates authenticate with
	// their own API key via apiKeyHelper, and on macOS the login lives in the
	// OS keychain (no file at all) — absence here is not proof of no login.
	r.Status = StatusOK
	r.Detail = "no credentials.json (fine — provider teammates use their own API key; the main session and the native `claude` leaf use claude's own login, which may live in the OS keychain instead)"
	return r
}
