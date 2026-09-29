package doctor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/onboarding"
	"github.com/ethanhq/cc-fleet/internal/teammate"
	"github.com/ethanhq/cc-fleet/internal/version"
)

// writePluginCache drops a cc-fleet plugin cache dir named by version (the
// layout Claude Code uses: plugins/cache/<marketplace>/cc-fleet/<version>/) so
// the binary↔plugin skew check has something to compare against.
func writePluginCache(t *testing.T, home, ver string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "plugins", "cache", "ethanhq", "cc-fleet", ver)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir plugin cache: %v", err)
	}
}

// pinVersion overrides the binary's reported version for one test and restores
// it after. version.Version is the link-time stamp; Resolve() returns it
// verbatim when it differs from the dev default.
func pinVersion(t *testing.T, v string) {
	t.Helper()
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })
	version.Version = v
}

// setupHome installs a fresh $HOME + $XDG_CONFIG_HOME inside a t.TempDir() so
// every check below sees a controlled, empty filesystem. Returns the home dir
// for follow-up file writes.
func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows reads USERPROFILE; keep the sandbox hermetic there
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

// writeFakeClaude drops a runnable `claude` into dir for ccver.Detect to exec
// for its --version. The fake is a #!/bin/sh script, so tests using it skip on
// windows (no sh).
func writeFakeClaude(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires sh: fake claude is a #!/bin/sh script not runnable on windows")
	}
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write claude: %v", err)
	}
}

// installMockTmux drops a fake `tmux` shell script into a fresh tempdir and
// prepends it to PATH. The script writes argv to $MOCK_ARGS_FILE and prints
// the contents of $MOCK_OUTPUT_FILE, then exits $MOCK_EXIT_CODE. This mirrors
// the helper in internal/tmux/tmux_test.go so we can test checks 3 and 5 in
// hermetic isolation.
func installMockTmux(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires sh: mock tmux is a #!/bin/sh fake binary not runnable on windows")
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.log")
	binPath := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
for a in "$@"; do
  printf '%s\n' "$a" >> "$MOCK_ARGS_FILE"
done
printf '__END__\n' >> "$MOCK_ARGS_FILE"
if [ -n "$MOCK_OUTPUT_FILE" ] && [ -f "$MOCK_OUTPUT_FILE" ]; then
  cat "$MOCK_OUTPUT_FILE"
fi
exit "${MOCK_EXIT_CODE:-0}"
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_ARGS_FILE", argsPath)
	t.Setenv("MOCK_OUTPUT_FILE", "")
	t.Setenv("MOCK_EXIT_CODE", "0")
	return argsPath
}

// setMockTmuxOutput points the mock tmux at a stdout-capture file containing
// lines.
func setMockTmuxOutput(t *testing.T, lines string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stdout.txt")
	if err := os.WriteFile(p, []byte(lines), 0o644); err != nil {
		t.Fatalf("write mock output: %v", err)
	}
	t.Setenv("MOCK_OUTPUT_FILE", p)
}

// ---------- Check 1: settings.json ----------

func TestCheckSettingsJSON_OK(t *testing.T) {
	home := setupHome(t)
	cdir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "settings.json"), []byte(`{"foo":"bar"}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	r := CheckSettingsJSON()
	if r.ID != 1 {
		t.Fatalf("ID = %d, want 1", r.ID)
	}
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (detail=%s)", r.Status, r.Detail)
	}
}

func TestCheckSettingsJSON_Missing(t *testing.T) {
	setupHome(t)
	r := CheckSettingsJSON()
	if r.Status != StatusFail {
		t.Fatalf("Status = %s, want fail", r.Status)
	}
	if !strings.Contains(r.Detail, "not found") {
		t.Fatalf("detail = %q, want 'not found'", r.Detail)
	}
}

func TestCheckSettingsJSON_Invalid(t *testing.T) {
	home := setupHome(t)
	cdir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "settings.json"), []byte(`{"broken":`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	r := CheckSettingsJSON()
	if r.Status != StatusFail {
		t.Fatalf("Status = %s, want fail", r.Status)
	}
	if !strings.Contains(r.Detail, "invalid JSON") {
		t.Fatalf("detail = %q, want 'invalid JSON'", r.Detail)
	}
}

func TestCheckSettingsJSON_NoHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // windows home var, so the no-home path holds on windows runners
	r := CheckSettingsJSON()
	if r.Status != StatusFail {
		t.Fatalf("Status = %s, want fail when HOME empty", r.Status)
	}
}

// ---------- Check 2: profiles writable ----------

func TestCheckProfilesDirWritable_OK(t *testing.T) {
	home := setupHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "profiles"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	r := CheckProfilesDirWritable()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (detail=%s)", r.Status, r.Detail)
	}
}

func TestCheckProfilesDirWritable_MissingIsOK(t *testing.T) {
	setupHome(t)
	r := CheckProfilesDirWritable()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok — a fresh machine has no profiles dir until the first provider is added (detail=%s)", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "not created yet") {
		t.Fatalf("detail = %q, want the not-created-yet note", r.Detail)
	}
}

func TestCheckProfilesDirWritable_MissingButUncreatable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based write denial doesn't work on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits")
	}
	home := setupHome(t)
	cdir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(cdir, 0o500); err != nil { // exists, not writable
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(cdir, 0o700) })
	r := CheckProfilesDirWritable()
	if r.Status != StatusFail {
		t.Fatalf("Status = %s, want fail when the profiles dir cannot be created (detail=%s)", r.Status, r.Detail)
	}
	if r.FixHint == "" {
		t.Fatal("FixHint empty, want a permissions hint")
	}
}

func TestCheckProfilesDirWritable_IsAFile(t *testing.T) {
	home := setupHome(t)
	cdir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "profiles"), []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	r := CheckProfilesDirWritable()
	if r.Status != StatusFail {
		t.Fatalf("Status = %s, want fail (detail=%s)", r.Status, r.Detail)
	}
}

// ---------- Check 3: tmux installed ----------

func TestCheckTmuxInstalled_OK(t *testing.T) {
	installMockTmux(t)
	setMockTmuxOutput(t, "tmux 3.3a\n")
	r := CheckTmuxInstalled()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (detail=%s)", r.Status, r.Detail)
	}
	if r.Detail != "tmux 3.3a" {
		t.Fatalf("detail = %q, want 'tmux 3.3a'", r.Detail)
	}
}

func TestCheckTmuxInstalled_Missing(t *testing.T) {
	// Don't install the mock; PATH points only at empty tempdir. tmux is needed
	// only for live teammates, so a missing tmux is a WARN (not a FAIL) and must
	// not flip doctor's overall OK.
	t.Setenv("PATH", t.TempDir())
	r := CheckTmuxInstalled()
	if r.Status != StatusWarn {
		t.Fatalf("Status = %s, want warn (tmux is optional — live teammates only)", r.Status)
	}
	if r.FixHint == "" {
		t.Fatalf("FixHint empty, want install hint")
	}
}

// ---------- Check 4: claude binary ----------

func TestCheckClaudeBinary_OK_FromVersionsLayout(t *testing.T) {
	home := setupHome(t)
	// Make PATH empty so ccver falls back to the per-version dir layout.
	t.Setenv("PATH", t.TempDir())
	verDir := filepath.Join(home, ".local", "share", "claude", "versions", "2.1.150")
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The dir alone is not enough — doctor only reports OK when a runnable claude
	// actually lives inside. Write the executable.
	writeFakeClaude(t, verDir)
	r := CheckClaudeBinary()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (detail=%s)", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "2.1.150") {
		t.Fatalf("detail = %q, want it to mention 2.1.150", r.Detail)
	}
}

// TestCheckClaudeBinary_VersionDirNoExecutable_Fail: a versioned dir with NO
// runnable claude inside is a missing-binary case, so the check must Fail (a
// version dir name alone must not report as a present binary).
func TestCheckClaudeBinary_VersionDirNoExecutable_Fail(t *testing.T) {
	home := setupHome(t)
	t.Setenv("PATH", t.TempDir())
	// Highest (and only) version dir exists but holds no executable claude.
	verDir := filepath.Join(home, ".local", "share", "claude", "versions", "2.1.150")
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	r := CheckClaudeBinary()
	if r.Status != StatusFail {
		t.Fatalf("Status = %s, want fail (no runnable claude in version dir; detail=%s)", r.Status, r.Detail)
	}
}

func TestCheckClaudeBinary_NotFound(t *testing.T) {
	setupHome(t)
	t.Setenv("PATH", t.TempDir())
	r := CheckClaudeBinary()
	if r.Status != StatusFail {
		t.Fatalf("Status = %s, want fail", r.Status)
	}
}

// TestCheckClaudeBinaryUsesClaudebin: check 4 resolves the binary the way the
// lanes do (claudebin): the flat versions/<semver> layout is accepted, a
// 0-byte claude on PATH is not, and a pre-2.1.278 version only adds a note.
func TestCheckClaudeBinaryUsesClaudebin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the flat versions layout and x bits are unix-only")
	}
	home := setupHome(t)
	pathDir := t.TempDir()
	t.Setenv("PATH", pathDir)
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(versions, "2.1.281")
	if err := os.WriteFile(flat, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := CheckClaudeBinary()
	if r.Status != StatusOK || !strings.Contains(r.Detail, "2.1.281") || strings.Contains(r.Detail, "provider teammates need") {
		t.Fatalf("flat 2.1.281: %+v", r)
	}

	if err := os.WriteFile(filepath.Join(pathDir, "claude"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if r := CheckClaudeBinary(); r.Status != StatusFail {
		t.Fatalf("0-byte claude on PATH: %+v, want fail", r)
	}

	if err := os.Remove(filepath.Join(pathDir, "claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(flat, filepath.Join(versions, "2.1.150")); err != nil {
		t.Fatal(err)
	}
	r = CheckClaudeBinary()
	if r.Status != StatusOK || !strings.Contains(r.Detail, "provider teammates need ≥ "+teammate.MinTeammateCC) {
		t.Fatalf("2.1.150: %+v, want ok with the teammate note", r)
	}
}

// ---------- Check 5: attached tmux session ----------

func TestCheckAttachedTmux_OK(t *testing.T) {
	installMockTmux(t)
	// list-panes output: pane_id session window pane_active attached cmd
	setMockTmuxOutput(t, "%1 main 0 1 1 claude\n%2 main 0 0 1 zsh\n%3 bg 0 1 0 vim\n")
	r := CheckAttachedTmux()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (detail=%s)", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "main") {
		t.Fatalf("detail = %q, want it to mention 'main'", r.Detail)
	}
}

// Check 5 is a Warn (not Fail): the out-of-tmux swarm path builds its own
// session, so an attached in-session target is optional. A Warn keeps
// DoctorResult.OK true.
func TestCheckAttachedTmux_NoAttachedWarns(t *testing.T) {
	installMockTmux(t)
	setMockTmuxOutput(t, "%1 bg 0 1 0 vim\n")
	r := CheckAttachedTmux()
	if r.Status != StatusWarn {
		t.Fatalf("Status = %s, want warn (out-of-tmux swarm works)", r.Status)
	}
}

func TestCheckAttachedTmux_TmuxErrorWarns(t *testing.T) {
	installMockTmux(t)
	t.Setenv("MOCK_EXIT_CODE", "1")
	r := CheckAttachedTmux()
	if r.Status != StatusWarn {
		t.Fatalf("Status = %s, want warn (out-of-tmux swarm works)", r.Status)
	}
}

// ---------- Check 6: provider keys ----------

// installProviderWithEndpoint writes providers.toml + secret file pointing at
// endpointURL so a CheckProviderKeys call probes the test server.
func installProviderWithEndpoint(t *testing.T, name, endpoint string, enabled bool) {
	t.Helper()
	v := &config.Provider{
		Name:           name,
		BaseURL:        endpoint,
		DefaultModel:   name + "-latest",
		ModelsEndpoint: endpoint,
		SecretBackend:  "file",
		SecretRef:      name + ".key",
		Enabled:        enabled,
		AddedAt:        time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC),
	}
	cfg := &config.Config{Version: config.SchemaVersion, Providers: map[string]*config.Provider{name: v}}
	cfgPath, err := config.ProvidersPath()
	if err != nil {
		t.Fatalf("ProvidersPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := config.SaveToPath(cfg, cfgPath); err != nil {
		t.Fatalf("SaveToPath: %v", err)
	}
	secDir, err := config.SecretsDir()
	if err != nil {
		t.Fatalf("SecretsDir: %v", err)
	}
	if err := os.MkdirAll(secDir, 0o700); err != nil {
		t.Fatalf("mkdir secrets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secDir, v.SecretRef), []byte("sk-test-key"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
}

func TestCheckProviderKeys_NoProviders(t *testing.T) {
	setupHome(t)
	r := CheckProviderKeys()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (detail=%s)", r.Status, r.Detail)
	}
	if r.Detail != "no providers configured" {
		t.Fatalf("detail = %q, want 'no providers configured'", r.Detail)
	}
}

func TestCheckProviderKeys_AllReachable(t *testing.T) {
	setupHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"x","owned_by":"y"}]}`)
	}))
	defer srv.Close()
	installProviderWithEndpoint(t, "deepseek", srv.URL, true)
	r := CheckProviderKeys()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (detail=%s)", r.Status, r.Detail)
	}
}

func TestCheckProviderKeys_OneFails(t *testing.T) {
	setupHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// Encode as JSON so the response body is well-formed.
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad key"})
	}))
	defer srv.Close()
	installProviderWithEndpoint(t, "deepseek", srv.URL, true)
	r := CheckProviderKeys()
	if r.Status != StatusFail {
		t.Fatalf("Status = %s, want fail (detail=%s)", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "deepseek") {
		t.Fatalf("detail = %q, want it to mention failed provider name", r.Detail)
	}
	if r.FixHint == "" {
		t.Fatalf("FixHint empty, want a hint")
	}
}

func TestCheckProviderKeys_DisabledSkipped(t *testing.T) {
	setupHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// If this fires, the disabled provider was incorrectly probed.
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	installProviderWithEndpoint(t, "deepseek", srv.URL, false)
	r := CheckProviderKeys()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (detail=%s)", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "no enabled") {
		t.Fatalf("detail = %q, want 'no enabled'", r.Detail)
	}
}

// ---------- Check 7: skill installed ----------

func TestCheckSkillInstalled_OK(t *testing.T) {
	home := setupHome(t)
	skillDir := filepath.Join(home, ".claude", "skills", "cc-fleet")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# skill"), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	r := CheckSkillInstalled()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (detail=%s)", r.Status, r.Detail)
	}
}

func TestCheckSkillInstalled_Missing(t *testing.T) {
	setupHome(t)
	r := CheckSkillInstalled()
	if r.Status != StatusWarn {
		t.Fatalf("Status = %s, want warn (missing legacy path AND no plugin copy)", r.Status)
	}
	if r.FixHint == "" {
		t.Fatalf("FixHint empty, want install hint")
	}
}

// TestCheckSkillInstalled_ViaPlugin: the legacy ~/.claude/skills path is absent
// but the cc-fleet plugin has unpacked the skill under plugins/cache/... — check
// 7 must report OK.
func TestCheckSkillInstalled_ViaPlugin(t *testing.T) {
	home := setupHome(t)
	pluginSkill := filepath.Join(home, ".claude", "plugins", "cache",
		"ethanhq", "cc-fleet", "0.1.1", "skills", "cc-fleet")
	if err := os.MkdirAll(pluginSkill, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginSkill, "SKILL.md"), []byte("# skill"), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	r := CheckSkillInstalled()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (skill delivered via plugin) detail=%s", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "plugin") {
		t.Fatalf("Detail = %q, want mention of plugin delivery", r.Detail)
	}
}

// writeSkill drops a SKILL.md at <home>/.claude/skills/<dir>/SKILL.md.
func writeSkill(t *testing.T, home, dir string) {
	t.Helper()
	d := filepath.Join(home, ".claude", "skills", dir)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", d, err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("# skill"), 0o600); err != nil {
		t.Fatalf("write %s: %v", d, err)
	}
}

// writeSharedDocs drops the full shared-doc set under root/<dir>/.
func writeSharedDocs(t *testing.T, root, dir string) {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", d, err)
	}
	for _, doc := range sharedDocs {
		if err := os.WriteFile(filepath.Join(d, doc), []byte("# doc"), 0o600); err != nil {
			t.Fatalf("write %s: %v", doc, err)
		}
	}
}

// TestCheckSkillInstalled_PerLaneAllThree: the three global per-lane skills + the
// shared docs present → OK.
func TestCheckSkillInstalled_PerLaneAllThree(t *testing.T) {
	home := setupHome(t)
	for _, lane := range skillLanes {
		writeSkill(t, home, "cc-fleet-"+lane)
	}
	writeSharedDocs(t, filepath.Join(home, ".claude", "skills"), sharedDirName)
	r := CheckSkillInstalled()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok for all three per-lane skills (detail=%s)", r.Status, r.Detail)
	}
}

// TestCheckSkillInstalled_SharedDocsMissingWarns: all three lanes present but no
// shared docs beside them → WARN (every lane links ../cc-fleet-shared/<doc>).
func TestCheckSkillInstalled_SharedDocsMissingWarns(t *testing.T) {
	home := setupHome(t)
	for _, lane := range skillLanes {
		writeSkill(t, home, "cc-fleet-"+lane)
	}
	r := CheckSkillInstalled()
	if r.Status != StatusWarn || !strings.Contains(r.Detail, "shared") {
		t.Fatalf("Status = %s detail=%q, want warn naming the missing shared docs", r.Status, r.Detail)
	}
}

// TestCheckSkillInstalled_LegacySharedLayoutOK: OLD lane skills (citing shared/)
// beside a legacy un-namespaced shared/ dir — a self-consistent install — read OK.
func TestCheckSkillInstalled_LegacySharedLayoutOK(t *testing.T) {
	home := setupHome(t)
	for _, lane := range skillLanes {
		writeSkill(t, home, "cc-fleet-"+lane) // body "# skill" — no cc-fleet-shared/ citation
	}
	writeSharedDocs(t, filepath.Join(home, ".claude", "skills"), "shared")
	r := CheckSkillInstalled()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok for the legacy shared layout (detail=%s)", r.Status, r.Detail)
	}
}

// TestCheckSkillInstalled_SkewedSharedWarns: CURRENT lane skills (citing
// cc-fleet-shared/) beside only the legacy shared/ dir — every link broken —
// must WARN, not pass on the legacy dir's presence.
func TestCheckSkillInstalled_SkewedSharedWarns(t *testing.T) {
	home := setupHome(t)
	for _, lane := range skillLanes {
		writeSkill(t, home, "cc-fleet-"+lane)
	}
	// The subagent lane cites the namespaced dir (the current skill shape).
	sub := filepath.Join(home, ".claude", "skills", "cc-fleet-subagent", "SKILL.md")
	if err := os.WriteFile(sub, []byte("# skill\nsee ../cc-fleet-shared/routing.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSharedDocs(t, filepath.Join(home, ".claude", "skills"), "shared")
	r := CheckSkillInstalled()
	if r.Status != StatusWarn || !strings.Contains(r.Detail, "shared") {
		t.Fatalf("Status = %s detail=%q, want warn for new skills beside only the legacy shared dir", r.Status, r.Detail)
	}
}

// TestCheckSkillInstalled_PerLanePartialWarns: only one of the three present → WARN
// (a partial install must not read as healthy — the other lanes are uninvokable).
func TestCheckSkillInstalled_PerLanePartialWarns(t *testing.T) {
	home := setupHome(t)
	writeSkill(t, home, "cc-fleet-subagent") // team + workflow missing
	r := CheckSkillInstalled()
	if r.Status != StatusWarn {
		t.Fatalf("Status = %s, want warn for a partial per-lane install (detail=%s)", r.Status, r.Detail)
	}
}

// TestCheckSkillInstalled_CoexistenceWarns: the per-lane skills AND a legacy single
// skill both present → WARN (the old router competes).
func TestCheckSkillInstalled_CoexistenceWarns(t *testing.T) {
	home := setupHome(t)
	for _, lane := range skillLanes {
		writeSkill(t, home, "cc-fleet-"+lane)
	}
	writeSkill(t, home, "cc-fleet") // the legacy router
	r := CheckSkillInstalled()
	if r.Status != StatusWarn {
		t.Fatalf("Status = %s, want warn on old+new coexistence (detail=%s)", r.Status, r.Detail)
	}
}

// TestCheckSkillInstalled_PluginLegacyCacheNoWarn: the new per-lane skills present
// alongside a LEGACY single skill that only lingers in the plugin cache (a stale
// version Claude Code won't load) must NOT WARN — only a manual ~/.claude/skills/cc-fleet
// copy actively competes.
func TestCheckSkillInstalled_PluginLegacyCacheNoWarn(t *testing.T) {
	home := setupHome(t)
	for _, lane := range skillLanes {
		writeSkill(t, home, "cc-fleet-"+lane)
	}
	writeSharedDocs(t, filepath.Join(home, ".claude", "skills"), sharedDirName)
	// Stale legacy in the plugin cache (an old version dir), NOT under ~/.claude/skills.
	legacy := filepath.Join(home, ".claude", "plugins", "cache", "ethanhq", "cc-fleet", "0.1.0", "skills", "cc-fleet")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "SKILL.md"), []byte("# old"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	r := CheckSkillInstalled()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (stale plugin-cache legacy must not WARN) detail=%s", r.Status, r.Detail)
	}
}

// TestCheckSkillInstalled_PluginPerLane: the three per-lane skills under one plugin
// version root → OK.
func TestCheckSkillInstalled_PluginPerLane(t *testing.T) {
	home := setupHome(t)
	root := filepath.Join(home, ".claude", "plugins", "cache", "ethanhq", "cc-fleet", "0.2.0", "skills")
	for _, lane := range skillLanes {
		d := filepath.Join(root, lane)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("# skill"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	writeSharedDocs(t, root, sharedDirName)
	r := CheckSkillInstalled()
	if r.Status != StatusOK || !strings.Contains(r.Detail, "plugin") {
		t.Fatalf("Status = %s detail = %q, want ok + plugin mention", r.Status, r.Detail)
	}
}

// ---------- Check 8: teammate lane ----------

// cliLaneHome is setupHome plus a separate CLAUDE_CONFIG_DIR, with the
// variables check 8 reads from the environment cleared. Returns the Claude
// Code config dir.
func cliLaneHome(t *testing.T) string {
	t.Helper()
	home := setupHome(t)
	claudeDir := filepath.Join(home, "claude-config")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	for _, k := range []string{teammate.EnvTeammateCommand, teammate.EnvAgentTeams, teammate.EnvProcessWrapper} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	return claudeDir
}

// cliLaneSetup saves one enabled provider and runs a full teammate setup with
// teammateMode tmux, so check 8 starts from a healthy lane.
func cliLaneSetup(t *testing.T) teammate.SetupResult {
	t.Helper()
	installProviderWithEndpoint(t, "glm", "https://glm.example.test/v1/models", true)
	res := teammate.Setup(teammate.SetupOptions{TeammateMode: "tmux"})
	if !res.OK {
		t.Fatalf("teammate setup: %s: %s", res.ErrorCode, res.ErrorMsg)
	}
	return res
}

// cliSetSetting rewrites one string key of the user settings.json.
func cliSetSetting(t *testing.T, claudeDir string, value string, keyPath ...string) {
	t.Helper()
	if _, err := onboarding.EditSettings(filepath.Join(claudeDir, "settings.json"),
		[]onboarding.SettingsEdit{{Path: keyPath, Value: value}}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckTeammateLaneNotSetUp(t *testing.T) {
	cliLaneHome(t)
	r := CheckTeammateLane()
	if r.ID != 8 || r.Title != "teammate lane (optional)" || r.Status != StatusOK ||
		r.Detail != "not set up (optional) — run: cc-fleet teammate setup" {
		t.Fatalf("got %+v", r)
	}

	cfgDir, err := config.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "fingerprint.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = CheckTeammateLane()
	if r.Status != StatusWarn || !strings.Contains(r.Detail, "legacy fingerprint.json can be deleted") {
		t.Fatalf("with legacy fingerprint.json: %+v", r)
	}
}

func TestCheckTeammateLaneBrokenShimFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the teammate lane is unix-only")
	}
	claudeDir := cliLaneHome(t)
	res := cliLaneSetup(t)
	if r := CheckTeammateLane(); r.Status != StatusOK {
		t.Fatalf("healthy lane: %+v", r)
	}

	// Shim not executable.
	if err := os.Chmod(res.Shim, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := CheckTeammateLane(); r.Status != StatusFail || !r.Fixable || !strings.Contains(r.FixHint, "cc-fleet repair") {
		t.Errorf("non-executable shim: %+v", r)
	}
	if err := os.Chmod(res.Shim, 0o755); err != nil {
		t.Fatal(err)
	}

	// Shim pins a cc-fleet that no longer exists.
	if err := os.WriteFile(res.Shim, teammate.RenderShim(filepath.Join(t.TempDir(), "gone", "cc-fleet")), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := CheckTeammateLane(); r.Status != StatusFail {
		t.Errorf("pinned binary gone: %+v", r)
	}

	// The setting names a file that does not exist.
	cliSetSetting(t, claudeDir, filepath.Join(t.TempDir(), "missing-shim"), "env", teammate.EnvTeammateCommand)
	if r := CheckTeammateLane(); r.Status != StatusFail {
		t.Errorf("missing shim: %+v", r)
	}
	if groupForID(8) != GroupOptional {
		t.Error("a teammate lane Fail must stay Optional")
	}
}

func TestCheckTeammateLaneWarnings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the teammate lane is unix-only")
	}
	cases := []struct {
		name string
		mut  func(t *testing.T, claudeDir string, res teammate.SetupResult)
		want string
	}{
		{"mode in-process", func(t *testing.T, d string, _ teammate.SetupResult) {
			cliSetSetting(t, d, "in-process", teammate.KeyTeammateMode)
		}, "teammateMode is unset or in-process"},
		{"agent teams off", func(t *testing.T, d string, _ teammate.SetupResult) {
			cliSetSetting(t, d, "0", "env", teammate.EnvAgentTeams)
		}, teammate.EnvAgentTeams},
		{"defs out of sync", func(t *testing.T, d string, _ teammate.SetupResult) {
			if err := os.Remove(filepath.Join(d, "agents", "ccf-glm.md")); err != nil {
				t.Fatal(err)
			}
		}, "out of sync: ccf-glm.md"},
		{"unmarked ccf def", func(t *testing.T, d string, _ teammate.SetupResult) {
			if err := os.WriteFile(filepath.Join(d, "agents", "ccf-mine.md"), []byte("---\nname: ccf-mine\n---\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "not managed by cc-fleet: ccf-mine.md"},
		{"zinc harbor", func(t *testing.T, d string, _ teammate.SetupResult) {
			if err := os.WriteFile(filepath.Join(d, ".claude.json"),
				[]byte(`{"cachedGrowthBookFeatures":{"tengu_zinc_harbor":true}}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "tengu_zinc_harbor"},
		{"process wrapper", func(t *testing.T, _ string, _ teammate.SetupResult) {
			t.Setenv(teammate.EnvProcessWrapper, "/usr/bin/true")
		}, teammate.EnvProcessWrapper},
		{"pinned binary differs", func(t *testing.T, _ string, res teammate.SetupResult) {
			other := filepath.Join(t.TempDir(), "cc-fleet")
			if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(res.Shim, teammate.RenderShim(other), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "not this cc-fleet binary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claudeDir := cliLaneHome(t)
			res := cliLaneSetup(t)
			if r := CheckTeammateLane(); r.Status != StatusOK || !strings.Contains(r.Detail, res.Shim) {
				t.Fatalf("healthy lane: %+v", r)
			}
			tc.mut(t, claudeDir, res)
			r := CheckTeammateLane()
			if r.Status != StatusWarn || !strings.Contains(r.Detail, tc.want) {
				t.Fatalf("got %+v, want warn mentioning %q", r, tc.want)
			}
			if !strings.Contains(r.FixHint, "cc-fleet repair") || !strings.Contains(r.FixHint, "cc-fleet teammate setup") {
				t.Errorf("fix hint = %q", r.FixHint)
			}
		})
	}
}

func TestGroupOptional358(t *testing.T) {
	for id := 1; id <= 10; id++ {
		want := GroupCore
		if id == 3 || id == 5 || id == 8 {
			want = GroupOptional
		}
		if got := groupForID(id); got != want {
			t.Errorf("groupForID(%d) = %s, want %s", id, got, want)
		}
	}
}

// ---------- Check 9: OAuth credentials ----------

func TestCheckOAuthCredentials_PresentDotfile(t *testing.T) {
	home := setupHome(t)
	cdir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cdir, ".credentials.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	r := CheckOAuthCredentials()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (detail=%s)", r.Status, r.Detail)
	}
}

func TestCheckOAuthCredentials_PresentLegacy(t *testing.T) {
	home := setupHome(t)
	cdir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "credentials.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	r := CheckOAuthCredentials()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (detail=%s)", r.Status, r.Detail)
	}
}

func TestCheckOAuthCredentials_MissingIsOK(t *testing.T) {
	// Absence is informational, NOT a warning. A main session on a provider profile
	// legitimately has no credentials.json — doctor must not show a yellow WARN
	// for it.
	setupHome(t)
	r := CheckOAuthCredentials()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (absence is informational, never a warn)", r.Status)
	}
}

// ---------- Check 10: binary ↔ plugin version skew ----------

func TestCheckPluginVersionMatch_DevBuildOK(t *testing.T) {
	// A dev/non-release binary is not comparable — OK, never a warn, even with a
	// plugin cached at some version.
	home := setupHome(t)
	pinVersion(t, devVersionForTest())
	writePluginCache(t, home, "0.1.6")
	r := CheckPluginVersionMatch()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok for a dev build (detail=%s)", r.Status, r.Detail)
	}
}

func TestCheckPluginVersionMatch_NoPluginOK(t *testing.T) {
	setupHome(t)
	pinVersion(t, "v0.1.6")
	r := CheckPluginVersionMatch()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok when no plugin is installed (detail=%s)", r.Status, r.Detail)
	}
}

func TestCheckPluginVersionMatch_MatchOK(t *testing.T) {
	home := setupHome(t)
	pinVersion(t, "v0.1.6")            // binary carries the git tag (leading v)
	writePluginCache(t, home, "0.1.6") // plugin manifest carries the bare version
	r := CheckPluginVersionMatch()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok when binary == plugin (detail=%s)", r.Status, r.Detail)
	}
}

func TestCheckPluginVersionMatch_SkewWarns(t *testing.T) {
	home := setupHome(t)
	pinVersion(t, "v0.1.8")
	writePluginCache(t, home, "0.1.6")
	r := CheckPluginVersionMatch()
	if r.Status != StatusWarn {
		t.Fatalf("Status = %s, want warn on binary≠plugin skew (detail=%s)", r.Status, r.Detail)
	}
	if r.FixHint == "" {
		t.Fatalf("FixHint empty, want a `ccf update` hint")
	}
}

// TestCheckPluginVersionMatch_NewestCachedWins: a stale older cache entry must
// not WARN against a matching newer one.
func TestCheckPluginVersionMatch_NewestCachedWins(t *testing.T) {
	home := setupHome(t)
	pinVersion(t, "v0.1.8")
	writePluginCache(t, home, "0.1.6") // stale
	writePluginCache(t, home, "0.1.8") // current
	r := CheckPluginVersionMatch()
	if r.Status != StatusOK {
		t.Fatalf("Status = %s, want ok (newest cached 0.1.8 matches; detail=%s)", r.Status, r.Detail)
	}
}

// devVersionForTest returns a value version.IsRelease treats as a non-release,
// matching the package's dev default without importing the unexported constant.
func devVersionForTest() string { return "0.1.0-dev" }
