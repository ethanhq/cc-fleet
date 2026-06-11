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
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fingerprint"
	"github.com/ethanhq/cc-fleet/internal/homedir"
	"github.com/ethanhq/cc-fleet/internal/models"
	"github.com/ethanhq/cc-fleet/internal/onboarding"
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

// CheckProfilesDirWritable is check 2: ~/.claude/profiles/ exists and is
// writable. Missing dir is fixable (mkdir 0700); permission-denied isn't (we
// surface a hint).
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
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("not found: %s", path)
		r.Fixable = true
		r.FixHint = fmt.Sprintf("mkdir -p %s (mode 0700)", path)
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

// CheckClaudeBinary is check 4: a `claude` binary is locatable and (best
// effort) its version is known. We don't fail if version detection comes back
// empty — that's only a Warn.
func CheckClaudeBinary() CheckResult {
	r := CheckResult{ID: 4, Title: "claude binary present; version known"}
	path, version, err := ccver.Detect()
	if err != nil {
		r.Status = StatusFail
		r.Detail = err.Error()
		r.FixHint = "install Claude Code (see https://docs.anthropic.com/claude-code)"
		return r
	}
	if version == "" {
		// Path resolved but `--version` didn't produce a parseable token.
		// Treat as Warn so doctor still reports OK=true overall.
		r.Status = StatusWarn
		r.Detail = ccver.String(path, version)
		return r
	}
	r.Status = StatusOK
	r.Detail = ccver.String(path, version)
	return r
}

// CheckAttachedTmux is check 5: at least one attached tmux session exists (the
// default in-tmux spawn target). This is only a Warn, not a Fail: the
// out-of-tmux swarm path builds its own persistent-socket session, so an
// in-session spawn target is not mandatory. A Warn leaves DoctorResult.OK true.
func CheckAttachedTmux() CheckResult {
	r := CheckResult{ID: 5, Title: "at least one attached tmux session"}
	panes, err := tmux.ListPanes()
	if err != nil {
		// `tmux list-panes -a` exits non-zero when no server is running. We
		// surface that as the "no sessions" case (with the actual error text
		// in detail) since both have the same user-facing fix.
		r.Status = StatusWarn
		r.Detail = fmt.Sprintf("tmux list-panes: %s", err.Error())
		r.FixHint = "start/attach a tmux session for in-session spawn (tmux new-session -A -s main); not needed for out-of-tmux swarm"
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
		r.FixHint = "attach a tmux session for in-session spawn (tmux attach -t main); not needed for out-of-tmux swarm"
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

// CheckFingerprint is check 8: a fingerprint cache exists and its cached
// cc_version matches the binary currently installed. A stale cache is a
// Fixable Fail (the user runs `cc-fleet refresh-fingerprint --probe-team
// ...` — doctor can't auto-fix because the probe needs a live native Agent
// teammate).
func CheckFingerprint() CheckResult {
	r := CheckResult{ID: 8, Title: "fingerprint cached and matches current cc version"}
	fp, err := fingerprint.Load()
	if err != nil {
		if errors.Is(err, fingerprint.ErrNotFound) {
			// A missing USER cache is NOT unhealthy. spawn/subagent fall back to
			// the embedded bundled recipe (LoadOrBundled) and resolve the binary
			// live (ResolveBinaryPath), so doctor validates that SAME runtime
			// contract here instead of failing fresh installs. Healthy when the
			// bundled recipe + a resolvable claude binary pass ValidateForRuntime;
			// it only Fails when that contract genuinely can't be met (no claude
			// binary anywhere → an unspawnable install).
			return checkBundledFingerprintRuntime(r)
		}
		r.Status = StatusFail
		r.Detail = err.Error()
		return r
	}

	// Compare cached cc_version against what's on disk now.
	_, currentVer, ccErr := ccver.Detect()
	if ccErr != nil {
		// We can't tell whether the fingerprint is stale without knowing
		// the current version — report the fingerprint we found but warn
		// that comparison failed. Don't fail the whole check on cc detect
		// failure; check 4 already covers that.
		r.Status = StatusWarn
		r.Detail = fmt.Sprintf("cached cc %s; current cc version unknown (%s)",
			fp.CCVersion, ccErr.Error())
		return r
	}
	if currentVer == "" {
		r.Status = StatusWarn
		r.Detail = fmt.Sprintf("cached cc %s; current cc version unknown", fp.CCVersion)
		return r
	}
	if fingerprint.IsStale(fp, currentVer) {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("cached cc %s != current cc %s", fp.CCVersion, currentVer)
		r.Fixable = true
		r.FixHint = "ask Claude to spawn a probe teammate, then: cc-fleet refresh-fingerprint --probe-team <team>"
		return r
	}
	r.Status = StatusOK
	r.Detail = fmt.Sprintf("cc %s (captured %s)",
		fp.CCVersion, fp.CapturedAt.UTC().Format(time.RFC3339))
	return r
}

// checkBundledFingerprintRuntime validates the no-user-cache path against the
// SAME runtime contract spawn/subagent use: LoadOrBundled → ResolveBinaryPath →
// ValidateForRuntime. It returns OK when the bundled recipe plus a resolvable
// claude binary are runtime-usable (a healthy fresh install), and
// only Fails when no claude binary can be resolved anywhere — the one genuine
// "can't spawn" state, fixable by installing/repairing Claude Code.
func checkBundledFingerprintRuntime(r CheckResult) CheckResult {
	fp, err := fingerprint.LoadOrBundled()
	if err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("no user fingerprint cache and bundled recipe unreadable: %v", err)
		return r
	}
	binPath, err := fingerprint.ResolveBinaryPath(fp)
	if err != nil {
		// No claude binary resolvable → spawn/subagent would FINGERPRINT_STALE.
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("no user fingerprint cache; bundled recipe present but %v", err)
		r.Fixable = true
		r.FixHint = "install/repair Claude Code (or fix PATH) so a claude binary is resolvable"
		return r
	}
	fp.BinaryPath = binPath
	if err := fingerprint.ValidateForRuntime(fp); err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("bundled fingerprint not runtime-usable: %v", err)
		return r
	}
	r.Status = StatusOK
	r.Detail = fmt.Sprintf("no user cache; bundled recipe (cc %s) is runtime-usable with %s",
		fp.CCVersion, binPath)
	return r
}

// CheckOAuthCredentials is check 9: does the main session have an OAuth /
// subscription credential on disk? **Purely informational** — its purpose is to
// report which auth the MAIN session can use, NOT to flag a problem. cc-fleet's
// model is: main session on the OAuth subscription, provider teammates on their
// own API key. A main session that runs entirely on a provider profile has no
// credentials.json and that is completely fine — so ABSENCE is reported as OK
// (informational), never a WARN, and this check can never flip doctor's overall OK.
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
	// Absent is fine and NOT a warning: only a main session on an OAuth /
	// subscription login needs this file; provider teammates authenticate with
	// their own API key via apiKeyHelper.
	r.Status = StatusOK
	r.Detail = "no credentials.json (fine — only a main session on an OAuth/subscription login needs it; provider teammates use their own API key)"
	return r
}
