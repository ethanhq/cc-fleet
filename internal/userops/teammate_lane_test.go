package userops

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/onboarding"
	"github.com/ethanhq/cc-fleet/internal/teammate"
)

// cliLaneHome is setupHome plus a separate CLAUDE_CONFIG_DIR, with the
// session variables the teammate lane reads cleared. Returns the Claude Code
// config dir.
func cliLaneHome(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the teammate lane is unix-only")
	}
	home := setupHome(t)
	claudeDir := filepath.Join(home, "claude-config")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	for _, k := range []string{teammate.EnvTeammateCommand, teammate.EnvAgentTeams} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	return claudeDir
}

// cliSetLane records the teammate lane as enabled or not.
func cliSetLane(t *testing.T, enabled bool) {
	t.Helper()
	st, _ := onboarding.LoadState()
	st.TeammateLane.Enabled = enabled
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
}

// cliModelsServer is a loopback /v1/models endpoint for Add's probe.
func cliModelsServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"m-1"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func cliAdd(t *testing.T, name string) *AddResult {
	t.Helper()
	url := cliModelsServer(t)
	res, err := Add(AddRequest{
		Name: name, BaseURL: url, ModelsEndpoint: url + "/v1/models", DefaultModel: name + "-1",
		SecretBackend: "file", SecretRef: name + ".key", APIKey: "sk-test-MARKER", Enabled: true,
	})
	if err != nil {
		t.Fatalf("Add(%s): %v", name, err)
	}
	return res
}

func TestAddSyncsAgentDefsWhenLaneOn(t *testing.T) {
	claudeDir := cliLaneHome(t)
	cliSetLane(t, true)

	res := cliAdd(t, "glm")
	if res.TeammateSyncError != "" {
		t.Fatalf("sync error: %s", res.TeammateSyncError)
	}
	def := filepath.Join(claudeDir, "agents", "ccf-glm.md")
	if !teammate.IsManagedDef(def) {
		t.Fatalf("%s not written", def)
	}

	// Disabling the provider (Edit) and removing it drop its definition again.
	off := false
	eres, err := Edit(EditRequest{Name: "glm", Enabled: &off})
	if err != nil || eres.TeammateSyncError != "" {
		t.Fatalf("Edit: %v %q", err, eres.TeammateSyncError)
	}
	if _, err := os.Stat(def); !os.IsNotExist(err) {
		t.Errorf("definition kept for a disabled provider (err=%v)", err)
	}
	on := true
	if _, err := Edit(EditRequest{Name: "glm", Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if !teammate.IsManagedDef(def) {
		t.Fatal("definition not restored on enable")
	}
	rres, err := Remove(RemoveRequest{Name: "glm"})
	if err != nil || rres.TeammateSyncError != "" {
		t.Fatalf("Remove: %v %q", err, rres.TeammateSyncError)
	}
	if _, err := os.Stat(def); !os.IsNotExist(err) {
		t.Errorf("definition kept after remove (err=%v)", err)
	}
}

func TestAddSkipsSyncWhenLaneOff(t *testing.T) {
	claudeDir := cliLaneHome(t)
	res := cliAdd(t, "glm")
	if res.TeammateSyncError != "" {
		t.Fatalf("sync error: %s", res.TeammateSyncError)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "agents")); !os.IsNotExist(err) {
		t.Errorf("agents dir touched with the lane off (err=%v)", err)
	}
}

// TestSyncErrorIsNonFatal: a definition sync that fails (the agents path is a
// file) is reported in the result, and the provider change still lands.
func TestSyncErrorIsNonFatal(t *testing.T) {
	claudeDir := cliLaneHome(t)
	cliSetLane(t, true)
	if err := os.WriteFile(filepath.Join(claudeDir, "agents"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := cliAdd(t, "glm")
	if res.TeammateSyncError == "" {
		t.Fatal("want a teammate_sync_error")
	}
	if strings.Contains(res.TeammateSyncError, "sk-test-MARKER") {
		t.Fatal("sync error leaks the key")
	}
	l, err := List()
	if err != nil || len(l.Providers) != 1 || l.Providers[0].Name != "glm" {
		t.Fatalf("provider not added: %+v %v", l, err)
	}
	model := "glm-2"
	eres, err := Edit(EditRequest{Name: "glm", DefaultModel: &model})
	if err != nil || eres.TeammateSyncError == "" || eres.Provider.DefaultModel != model {
		t.Fatalf("Edit: err=%v res=%+v", err, eres)
	}
	if l, _ := List(); l.Providers[0].DefaultModel != model {
		t.Errorf("edit not saved: %+v", l.Providers[0])
	}
}

// TestFx2tmSyncConflictIsWarning: an unmarked ccf-<p>.md already on disk is
// never overwritten; Add/Edit/Remove still succeed and report it in
// teammate_sync_error, and Repair reports it in agent_defs.conflicts.
func TestFx2tmSyncConflictIsWarning(t *testing.T) {
	claudeDir := cliLaneHome(t)
	cliSetLane(t, true)
	def := filepath.Join(claudeDir, "agents", "ccf-glm.md")
	if err := os.MkdirAll(filepath.Dir(def), 0o755); err != nil {
		t.Fatal(err)
	}
	const mine = "---\nname: ccf-glm\nmodel: sonnet\n---\nmine\n"
	if err := os.WriteFile(def, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	res := cliAdd(t, "glm")
	if !strings.Contains(res.TeammateSyncError, def) || !strings.Contains(res.TeammateSyncError, "not managed by cc-fleet") {
		t.Fatalf("teammate_sync_error = %q, want a conflict warning naming %s", res.TeammateSyncError, def)
	}
	if l, err := List(); err != nil || len(l.Providers) != 1 || l.Providers[0].Name != "glm" {
		t.Fatalf("provider not added: %+v %v", l, err)
	}
	if got, _ := os.ReadFile(def); string(got) != mine {
		t.Fatalf("unmarked definition overwritten: %q", got)
	}

	model := "glm-2"
	if eres, err := Edit(EditRequest{Name: "glm", DefaultModel: &model}); err != nil || !strings.Contains(eres.TeammateSyncError, def) {
		t.Fatalf("Edit: err=%v res=%+v", err, eres)
	}
	rep, err := Repair()
	if err != nil || rep.AgentDefs == nil || !reflect.DeepEqual(rep.AgentDefs.Conflicts, []string{"ccf-glm.md"}) {
		t.Fatalf("Repair: err=%v res=%+v", err, rep)
	}
	if w := AgentDefConflictWarning(rep.AgentDefs.Conflicts); !strings.Contains(w, def) {
		t.Fatalf("AgentDefConflictWarning = %q, want %s", w, def)
	}
	if w := AgentDefConflictWarning(nil); w != "" {
		t.Fatalf("AgentDefConflictWarning(nil) = %q", w)
	}
	// Remove drops the provider, so nothing wants ccf-glm.md any more.
	if rres, err := Remove(RemoveRequest{Name: "glm"}); err != nil || rres.TeammateSyncError != "" {
		t.Fatalf("Remove: err=%v res=%+v", err, rres)
	}
}

func TestRepairRepinsShim(t *testing.T) {
	claudeDir := cliLaneHome(t)
	seedProvider(t, "glm")

	// Lane off and no shim: repair leaves the lane alone.
	res, err := Repair()
	if err != nil {
		t.Fatal(err)
	}
	if res.Shim != "" || res.ShimRepinned || res.AgentDefs != nil {
		t.Fatalf("lane off: %+v", res)
	}

	// Lane on, shim pinned to a binary that moved away and stripped of its x bits.
	cliSetLane(t, true)
	shim, err := teammate.ShimPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, teammate.RenderShim("/old/place/cc-fleet"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = Repair()
	if err != nil {
		t.Fatal(err)
	}
	if res.Shim != shim || !res.ShimRepinned {
		t.Fatalf("repair: %+v", res)
	}
	st := teammate.InspectShim(shim)
	if !st.Executable || !st.PinnedIsSelf {
		t.Errorf("shim after repair: %+v", st)
	}
	if res.AgentDefs == nil || !reflect.DeepEqual(res.AgentDefs.Written, []string{"ccf-glm.md"}) {
		t.Errorf("agent_defs = %+v", res.AgentDefs)
	}
	if !teammate.IsManagedDef(filepath.Join(claudeDir, "agents", "ccf-glm.md")) {
		t.Error("definition not synced")
	}

	// Idempotent: nothing left to re-pin.
	if res, err = Repair(); err != nil || res.ShimRepinned || len(res.AgentDefs.Written) != 0 {
		t.Errorf("second repair: %+v %v", res, err)
	}

	// Lane off but the shim exists: it is still re-pinned, definitions untouched.
	cliSetLane(t, false)
	if err := os.WriteFile(shim, teammate.RenderShim("/old/place/cc-fleet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if res, err = Repair(); err != nil || !res.ShimRepinned || res.AgentDefs != nil {
		t.Errorf("lane off, shim present: %+v %v", res, err)
	}
}

func TestUninstallRemovesLane(t *testing.T) {
	claudeDir := cliLaneHome(t)
	seedProvider(t, "glm")
	setup := teammate.Setup(teammate.SetupOptions{TeammateMode: "tmux"})
	if !setup.OK {
		t.Fatalf("setup: %+v", setup)
	}
	settings := filepath.Join(claudeDir, "settings.json")

	res, err := Uninstall(UninstallRequest{KeepSecrets: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		settings + ": env." + teammate.EnvTeammateCommand,
		filepath.Join(claudeDir, "agents", "ccf-glm.md"),
	} {
		if !contains(res.Removed, want) {
			t.Errorf("Removed lacks %q: %v", want, res.Removed)
		}
	}
	if !contains(res.Kept, setup.Shim+" (can be deleted after every claude session has restarted)") {
		t.Errorf("Kept lacks the shim: %v", res.Kept)
	}
	if _, present, _ := onboarding.SettingsString(settings, "env", teammate.EnvTeammateCommand); present {
		t.Error("launcher setting still present")
	}
	if v, _, _ := onboarding.SettingsString(settings, teammate.KeyTeammateMode); v != "tmux" {
		t.Errorf("teammateMode = %q, want kept", v)
	}
	if _, err := os.Stat(setup.Shim); err != nil {
		t.Errorf("shim removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "agents", "ccf-glm.md")); !os.IsNotExist(err) {
		t.Errorf("definition still present (err=%v)", err)
	}
}
