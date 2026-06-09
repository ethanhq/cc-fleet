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
	"github.com/ethanhq/cc-fleet/internal/models"
	"github.com/ethanhq/cc-fleet/internal/tmux"
	"github.com/ethanhq/cc-fleet/internal/version"
)

// vendorProbeTimeout caps each vendor's /v1/models probe in check 6 at
// 3s/vendor so the total check time stays bounded even with several vendors
// configured.
const vendorProbeTimeout = 3 * time.Second

// (There is deliberately no agent-teams detector here: agent-teams is a Claude
// runtime state set by GrowthBook, invisible to an external process. The env
// var is an unreliable proxy that misfires for the common default-on case, so
// cc-fleet does not detect it. Whether teammate mode is usable is decided by
// the skill from Claude's own tool availability. The tmux capability check used
// by first-run onboarding lives in internal/onboarding.)

// homeDir resolves $HOME. We don't use os.UserHomeDir because that consults
// /etc/passwd as a fallback — tests rely on t.Setenv("HOME", tempDir) and
// shouldn't see real-user files leak in.
func homeDir() (string, error) {
	h := os.Getenv("HOME")
	if h == "" {
		return "", errors.New("HOME is not set")
	}
	return h, nil
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
		r.FixHint = "for live teammates, install tmux (apt-get install tmux | brew install tmux)"
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

// CheckVendorKeys is check 6: every enabled vendor's /v1/models endpoint
// answers within vendorProbeTimeout. Each vendor is probed with its own
// 3s-bounded context so a slow vendor can't drag the rest down.
//
// "Enabled" means Vendor.Enabled = true in vendors.toml. Disabled vendors are
// reported in the detail but not probed. A missing vendors.toml is OK (returns
// an empty Config) — the check just reports "no vendors configured".
func CheckVendorKeys() CheckResult {
	r := CheckResult{ID: 6, Title: "all configured vendors' keys reachable"}
	cfg, err := config.Load()
	if err != nil {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("load vendors.toml: %s", err.Error())
		return r
	}
	if len(cfg.Vendors) == 0 {
		// No vendors configured — nothing to probe; surface as OK with a
		// hint so the user knows the check did consider its inputs.
		r.Status = StatusOK
		r.Detail = "no vendors configured"
		return r
	}

	// Deterministic order so detail text doesn't churn between runs.
	names := make([]string, 0, len(cfg.Vendors))
	for n := range cfg.Vendors {
		names = append(names, n)
	}
	sort.Strings(names)

	var (
		probed   int
		failures []string
	)
	for _, name := range names {
		v := cfg.Vendors[name]
		if !v.Enabled {
			continue
		}
		probed++
		ctx, cancel := context.WithTimeout(context.Background(), vendorProbeTimeout)
		_, fetchErr := models.Fetch(ctx, v)
		cancel()
		if fetchErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", name, fetchErr.Error()))
		}
	}
	if probed == 0 {
		r.Status = StatusOK
		r.Detail = "no enabled vendors"
		return r
	}
	if len(failures) > 0 {
		r.Status = StatusFail
		r.Detail = fmt.Sprintf("%d/%d vendor(s) failed: %s",
			len(failures), probed, strings.Join(failures, "; "))
		r.FixHint = "verify the vendor's API key (cc-fleet keyget <vendor>) and base_url"
		return r
	}
	r.Status = StatusOK
	r.Detail = fmt.Sprintf("%d vendor(s) reachable", probed)
	return r
}

// CheckSkillInstalled is check 7: the cc-fleet skill is installed at
// ~/.claude/skills/cc-fleet/SKILL.md (the manual `make install-skill`
// channel). We deliberately don't validate SKILL.md content; existence is enough.
//
// The skill ships through two channels and doctor checks BOTH:
//  1. the manual `make install-skill` path ~/.claude/skills/cc-fleet/SKILL.md, and
//  2. the cc-fleet plugin, which Claude Code unpacks under
//     ~/.claude/plugins/cache/<marketplace>/cc-fleet/<version>/skills/cc-fleet/SKILL.md.
//
// Either present → OK. A MISSING skill (neither channel) is only a WARN, not a
// FAIL — doctor can't auto-install (the source lives in the cc-fleet repo), so
// it surfaces a hint instead.
func CheckSkillInstalled() CheckResult {
	r := CheckResult{ID: 7, Title: "skill installed at ~/.claude/skills/cc-fleet/ (or via plugin)"}
	cdir, err := claudeDir()
	if err != nil {
		r.Status = StatusFail
		r.Detail = err.Error()
		return r
	}
	path := filepath.Join(cdir, "skills", "cc-fleet", "SKILL.md")
	info, err := os.Stat(path)
	if err == nil {
		if info.IsDir() {
			r.Status = StatusFail
			r.Detail = fmt.Sprintf("%s is a directory, not a file", path)
			return r
		}
		r.Status = StatusOK
		r.Detail = path
		return r
	}
	if !errors.Is(err, os.ErrNotExist) {
		// A real stat error (permission, etc.) — not a clean "absent".
		r.Status = StatusFail
		r.Detail = err.Error()
		return r
	}
	// Not at the legacy manual-install path — look for the plugin-delivered copy
	// before warning, so a plugin user reports OK instead of a false WARN.
	if p, ok := pluginSkillPath(cdir); ok {
		r.Status = StatusOK
		r.Detail = fmt.Sprintf("installed via the cc-fleet plugin: %s", p)
		return r
	}
	r.Status = StatusWarn
	r.Detail = fmt.Sprintf("not found at %s and no cc-fleet plugin skill detected", path)
	r.FixHint = "run `make install-skill`, or install the cc-fleet plugin (/plugin install cc-fleet@<marketplace>)"
	return r
}

// pluginSkillPath returns the path to a cc-fleet SKILL.md delivered by the
// cc-fleet plugin, if one is installed, and ok=false otherwise. Claude Code
// unpacks marketplace plugins under
// ~/.claude/plugins/cache/<marketplace>/<plugin>/<version>/; the cc-fleet plugin
// (plugin name "cc-fleet", fixed in .claude-plugin/plugin.json) ships
// skills/cc-fleet/SKILL.md. The glob spans any marketplace + version so a
// fork or an upgrade still matches; a layout change just falls back to the WARN
// (no worse than before).
func pluginSkillPath(cdir string) (string, bool) {
	pattern := filepath.Join(cdir, "plugins", "cache", "*", "cc-fleet", "*", "skills", "cc-fleet", "SKILL.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", false // only ErrBadPattern, impossible for this fixed pattern
	}
	for _, m := range matches {
		if info, statErr := os.Stat(m); statErr == nil && !info.IsDir() {
			return m, true
		}
	}
	return "", false
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
// model is: main session on the OAuth subscription, vendor teammates on their
// own API key. A main session that runs entirely on a vendor profile has no
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
	// subscription login needs this file; vendor teammates authenticate with
	// their own API key via apiKeyHelper.
	r.Status = StatusOK
	r.Detail = "no credentials.json (fine — only a main session on an OAuth/subscription login needs it; vendor teammates use their own API key)"
	return r
}
