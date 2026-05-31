package spawn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// nativeConfigJSON is a real Claude-written team config: a team-lead entry that
// omits color/isActive/backendType/prompt/planModeRequired, plus a regular
// teammate that carries them, including an unmodelled future field on the
// teammate.
const nativeConfigJSON = `{
  "name": "demo",
  "description": "d",
  "createdAt": 1779687473380,
  "leadAgentId": "team-lead@demo",
  "leadSessionId": "sess-1",
  "members": [
    {
      "agentId": "team-lead@demo",
      "name": "team-lead",
      "agentType": "team-lead",
      "model": "claude-opus-4-7[1m]",
      "joinedAt": 1779687473380,
      "tmuxPaneId": "",
      "cwd": "/repo",
      "subscriptions": []
    },
    {
      "agentId": "worker@demo",
      "name": "worker",
      "color": "blue",
      "joinedAt": 1779687538084,
      "tmuxPaneId": "%84",
      "subscriptions": [],
      "agentType": "general-purpose",
      "model": "opus",
      "prompt": "do the thing",
      "planModeRequired": false,
      "cwd": "/repo",
      "backendType": "tmux",
      "isActive": true,
      "futureField": "keep me"
    }
  ]
}`

// seedConfig writes raw bytes to the team's config.json under a fresh temp HOME
// and returns the team name.
func seedConfig(t *testing.T, raw string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	const team = "demo"
	p, err := TeamConfigPath(team)
	if err != nil {
		t.Fatalf("TeamConfigPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	return team
}

// readConfigMembers reads the team config.json back from disk and returns its
// members keyed by name → that member's raw key/value map.
func readConfigMembers(t *testing.T, team string) map[string]map[string]any {
	t.Helper()
	p, _ := TeamConfigPath(team)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	out := map[string]map[string]any{}
	ms, _ := top["members"].([]any)
	for _, m := range ms {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		name, _ := mm["name"].(string)
		out[name] = mm
	}
	return out
}

// TestWriteTeamConfig_RoundTripPreservesPerMemberFields proves a load→write
// round-trip keeps the per-member fields (color,
// isActive, backendType, prompt, planModeRequired) AND any unmodelled field,
// while leaving the team-lead entry — which legitimately omits those keys —
// byte-stable (omitempty must not inject zero values into it).
func TestWriteTeamConfig_RoundTripPreservesPerMemberFields(t *testing.T) {
	team := seedConfig(t, nativeConfigJSON)

	tc, err := LoadTeamConfig(team)
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	if err := WriteTeamConfig(team, tc); err != nil {
		t.Fatalf("WriteTeamConfig: %v", err)
	}

	members := readConfigMembers(t, team)

	w := members["worker"]
	if w["color"] != "blue" {
		t.Errorf("worker color = %v, want blue", w["color"])
	}
	if w["backendType"] != "tmux" {
		t.Errorf("worker backendType = %v, want tmux", w["backendType"])
	}
	if w["isActive"] != true {
		t.Errorf("worker isActive = %v, want true", w["isActive"])
	}
	if w["prompt"] != "do the thing" {
		t.Errorf("worker prompt = %v, want \"do the thing\"", w["prompt"])
	}
	// planModeRequired was explicitly false in the source; the Raw merge must
	// preserve the *key* even though omitempty drops the typed zero value.
	if v, ok := w["planModeRequired"]; !ok || v != false {
		t.Errorf("worker planModeRequired present=%v value=%v, want present false", ok, v)
	}
	if w["futureField"] != "keep me" {
		t.Errorf("worker lost unmodelled futureField: %v", w["futureField"])
	}

	// The team-lead entry must not have gained any of the omitempty keys.
	l := members["team-lead"]
	for _, k := range []string{"color", "isActive", "backendType", "prompt", "planModeRequired"} {
		if v, ok := l[k]; ok {
			t.Errorf("team-lead entry gained %q = %v; omitempty should keep it absent", k, v)
		}
	}
}

// TestWriteTeamConfig_AppendDoesNotStripExistingMembers: a cc-fleet spawn
// appends a member and rewrites the file — it must not blank the UI fields of
// the members already in the team.
func TestWriteTeamConfig_AppendDoesNotStripExistingMembers(t *testing.T) {
	team := seedConfig(t, nativeConfigJSON)

	tc, err := LoadTeamConfig(team)
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	// Append a vendor member shaped the way spawn.go builds one (Raw nil).
	tc.Members = append(tc.Members, Member{
		AgentID:       "vendor@demo",
		Name:          "vendor",
		Color:         "green",
		AgentType:     "general-purpose",
		Model:         "deepseek-v4-flash",
		JoinedAt:      99,
		TmuxPaneID:    "%99",
		Cwd:           "/repo",
		Subscriptions: []string{},
		BackendType:   "tmux",
		IsActive:      true,
	})
	if err := WriteTeamConfig(team, tc); err != nil {
		t.Fatalf("WriteTeamConfig: %v", err)
	}

	members := readConfigMembers(t, team)

	// The pre-existing native worker must keep every UI field after the append.
	w := members["worker"]
	for k, want := range map[string]any{"color": "blue", "backendType": "tmux", "isActive": true} {
		if w[k] != want {
			t.Errorf("existing worker %q = %v, want %v (stripped on append!)", k, w[k], want)
		}
	}
	if w["futureField"] != "keep me" {
		t.Errorf("existing worker lost unmodelled field on append")
	}

	// The new vendor member carries the fields the main session renders.
	v := members["vendor"]
	if v["color"] != "green" || v["backendType"] != "tmux" || v["isActive"] != true {
		t.Errorf("new vendor member missing UI fields: %+v", v)
	}
	// ...and omits the fields cc-fleet never sets (omitempty).
	for _, k := range []string{"prompt", "planModeRequired"} {
		if val, ok := v[k]; ok {
			t.Errorf("new vendor member should omit %q, got %v", k, val)
		}
	}
}

// hiddenMemberConfigJSON is a team with one member currently hidden — it carries
// both hidden=true and an originWindow, the shape panevis writes on hide.
const hiddenMemberConfigJSON = `{
  "leadSessionId": "sess-1",
  "members": [
    {
      "agentId": "worker@demo",
      "name": "worker",
      "agentType": "general-purpose",
      "model": "glm-4.6",
      "joinedAt": 1779687538084,
      "tmuxPaneId": "%84",
      "cwd": "/repo",
      "subscriptions": [],
      "hidden": true,
      "originWindow": "main:0"
    }
  ]
}`

// memberPtr returns a pointer to the named member in tc, or nil.
func memberPtr(tc *TeamConfig, name string) *Member {
	for i := range tc.Members {
		if tc.Members[i].Name == name {
			return &tc.Members[i]
		}
	}
	return nil
}

// TestMember_RoundTripsHiddenOrigin: a hidden member's hidden/originWindow
// survive a load→write round-trip (both as typed fields and in the JSON).
func TestMember_RoundTripsHiddenOrigin(t *testing.T) {
	team := seedConfig(t, hiddenMemberConfigJSON)
	tc, err := LoadTeamConfig(team)
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	w := memberPtr(tc, "worker")
	if w == nil || !w.Hidden || w.OriginWindow != "main:0" {
		t.Fatalf("loaded member hidden/origin wrong: %+v", w)
	}
	if err := WriteTeamConfig(team, tc); err != nil {
		t.Fatalf("WriteTeamConfig: %v", err)
	}
	m := readConfigMembers(t, team)["worker"]
	if m["hidden"] != true {
		t.Errorf("hidden not preserved on round-trip: %v", m["hidden"])
	}
	if m["originWindow"] != "main:0" {
		t.Errorf("originWindow not preserved on round-trip: %v", m["originWindow"])
	}
}

// TestWriteTeamConfig_ShowClearsHiddenKeys is the omitempty-Raw-shadow regression
// test. Phase B proves the trap is real (setting the typed zero alone leaves the
// stale Raw key shadowing through); Phase A proves the defeat (deleting the keys
// from Raw first, as panevis does, drops them from the written JSON).
func TestWriteTeamConfig_ShowClearsHiddenKeys(t *testing.T) {
	// Phase B — necessity: typed zero WITHOUT the Raw delete leaves the key.
	teamB := seedConfig(t, hiddenMemberConfigJSON)
	tcB, err := LoadTeamConfig(teamB)
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	wB := memberPtr(tcB, "worker")
	wB.Hidden = false
	wB.OriginWindow = ""
	if err := WriteTeamConfig(teamB, tcB); err != nil {
		t.Fatalf("WriteTeamConfig: %v", err)
	}
	shadowed := readConfigMembers(t, teamB)["worker"]
	if _, ok := shadowed["hidden"]; !ok {
		t.Fatal("expected the Raw shadow to keep hidden present without the delete; " +
			"if this changed, the panevis delete-from-Raw step may be dead code")
	}

	// Phase A — defeat: delete from Raw first, then set typed zero → keys gone.
	teamA := seedConfig(t, hiddenMemberConfigJSON)
	tcA, err := LoadTeamConfig(teamA)
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	wA := memberPtr(tcA, "worker")
	delete(wA.Raw, "hidden")
	delete(wA.Raw, "originWindow")
	wA.Hidden = false
	wA.OriginWindow = ""
	if err := WriteTeamConfig(teamA, tcA); err != nil {
		t.Fatalf("WriteTeamConfig: %v", err)
	}
	cleared := readConfigMembers(t, teamA)["worker"]
	if v, ok := cleared["hidden"]; ok {
		t.Errorf("hidden key should be gone after show, got %v", v)
	}
	if v, ok := cleared["originWindow"]; ok {
		t.Errorf("originWindow key should be gone after show, got %v", v)
	}
}

// TestTmuxSocket_RawRoundTrip: the out-of-tmux swarm socket persists via
// Raw — a typed TeamConfig field would NOT round-trip (WriteTeamConfig only
// force-writes leadSessionId + members). SetTmuxSocket→write→load reads it back;
// clearing it deletes the Raw key with no residue, while unrelated Raw keys are
// preserved throughout.
func TestTmuxSocket_RawRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const team = "swarmteam"
	if err := EnsureTeamDir(team); err != nil {
		t.Fatalf("EnsureTeamDir: %v", err)
	}

	tc := &TeamConfig{LeadSessionID: "lead-1", Raw: map[string]any{"futureKey": "keep-me"}}
	tc.SetTmuxSocket("cc-fleet-swarm-swarmteam")
	if err := WriteTeamConfig(team, tc); err != nil {
		t.Fatalf("WriteTeamConfig: %v", err)
	}

	got, err := LoadTeamConfig(team)
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	if got.TmuxSocket() != "cc-fleet-swarm-swarmteam" {
		t.Fatalf("socket round-trip = %q, want cc-fleet-swarm-swarmteam", got.TmuxSocket())
	}
	if got.Raw["futureKey"] != "keep-me" {
		t.Fatalf("unrelated Raw key not preserved: %v", got.Raw["futureKey"])
	}

	// Clear → delete from Raw → no residual key on reload (the omitempty-Raw-shadow
	// trap: a typed empty string alone would leave the old value visible).
	got.SetTmuxSocket("")
	if err := WriteTeamConfig(team, got); err != nil {
		t.Fatalf("WriteTeamConfig (clear): %v", err)
	}
	reloaded, err := LoadTeamConfig(team)
	if err != nil {
		t.Fatalf("LoadTeamConfig (clear): %v", err)
	}
	if reloaded.TmuxSocket() != "" {
		t.Fatalf("socket should be cleared, got %q", reloaded.TmuxSocket())
	}
	if _, ok := reloaded.Raw["tmuxSocket"]; ok {
		t.Fatal("tmuxSocket key should be deleted from Raw, not lingering")
	}
	if reloaded.Raw["futureKey"] != "keep-me" {
		t.Fatalf("unrelated Raw key lost after clear: %v", reloaded.Raw["futureKey"])
	}
}

// TestRoundTrip_NeverHiddenMembersGainNoHiddenKey: a config with no hide state
// must not gain hidden/originWindow keys on round-trip — omitempty must not
// inject the new fields into the lead or any plain member.
func TestRoundTrip_NeverHiddenMembersGainNoHiddenKey(t *testing.T) {
	team := seedConfig(t, nativeConfigJSON)
	tc, err := LoadTeamConfig(team)
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	if err := WriteTeamConfig(team, tc); err != nil {
		t.Fatalf("WriteTeamConfig: %v", err)
	}
	for name, mm := range readConfigMembers(t, team) {
		for _, k := range []string{"hidden", "originWindow"} {
			if v, ok := mm[k]; ok {
				t.Errorf("member %q gained %q=%v; omitempty should keep it absent", name, k, v)
			}
		}
	}
}

// TestFindMemberByPane_HitMissAndNoRoot: a registered pane resolves to its
// (team, member); an unknown pane and a missing teams root both report
// found=false with no error.
func TestFindMemberByPane_HitMissAndNoRoot(t *testing.T) {
	team := seedConfig(t, nativeConfigJSON) // worker owns pane %84

	gotTeam, gotName, found, err := FindMemberByPane("%84")
	if err != nil || !found || gotTeam != team || gotName != "worker" {
		t.Fatalf("hit: team=%q name=%q found=%v err=%v, want demo/worker", gotTeam, gotName, found, err)
	}

	if _, _, found, err := FindMemberByPane("%999"); err != nil || found {
		t.Fatalf("miss: found=%v err=%v, want false,nil", found, err)
	}

	// A HOME with no teams root → found=false, nil err (not an error).
	t.Setenv("HOME", t.TempDir())
	if _, _, found, err := FindMemberByPane("%84"); err != nil || found {
		t.Fatalf("no teams root: found=%v err=%v, want false,nil", found, err)
	}
}

// TestMember_NewMemberEmitsUIFieldsOmitsUnset documents the on-disk shape of a
// freshly constructed cc-fleet member (no preserved Raw): color/backendType/
// isActive present, prompt/planModeRequired absent.
func TestMember_NewMemberEmitsUIFieldsOmitsUnset(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const team = "fresh"
	tc := &TeamConfig{
		LeadSessionID: "s",
		Raw:           map[string]any{},
		Members: []Member{{
			AgentID:       "w@fresh",
			Name:          "w",
			Color:         "cyan",
			AgentType:     "general-purpose",
			Model:         "m",
			JoinedAt:      1,
			TmuxPaneID:    "%1",
			Cwd:           "/x",
			Subscriptions: []string{},
			BackendType:   "tmux",
			IsActive:      true,
		}},
	}
	if err := WriteTeamConfig(team, tc); err != nil {
		t.Fatalf("WriteTeamConfig: %v", err)
	}
	m := readConfigMembers(t, team)["w"]
	if m["color"] != "cyan" || m["backendType"] != "tmux" || m["isActive"] != true {
		t.Errorf("new member shape wrong: %+v", m)
	}
	for _, k := range []string{"prompt", "planModeRequired"} {
		if v, ok := m[k]; ok {
			t.Errorf("new member should omit %q, got %v", k, v)
		}
	}
}
