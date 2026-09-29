package teammate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// evalModeHome is a hermetic home for ResolveMode with managedSettingsDirs
// pointed at <Home>/managed and a lead cwd at <Home>/proj.
type evalModeHome struct {
	testHome
	managed string
	proj    string
}

func evalModeSetup(t *testing.T) evalModeHome {
	t.Helper()
	h := evalModeHome{testHome: hermeticHome(t)}
	h.managed = filepath.Join(h.Home, "managed")
	h.proj = filepath.Join(h.Home, "proj")
	for _, d := range []string{h.managed, h.proj} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := managedSettingsDirs
	t.Cleanup(func() { managedSettingsDirs = old })
	managedSettingsDirs = func() []string { return []string{h.managed} }
	return h
}

// evalEnv returns a Getenv over a fixed map.
func evalEnv(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

func TestModeSourceChain(t *testing.T) {
	h := evalModeSetup(t)
	flagFile := filepath.Join(h.proj, "lead-settings.json")
	layers := []struct {
		source string
		path   string // file holding {"teammateMode": "<source>-mode"}
	}{
		{SourcePolicy, filepath.Join(h.managed, "managed-settings.json")},
		{SourceFlag, flagFile},
		{SourceLocal, filepath.Join(h.proj, ".claude", "settings.local.json")},
		{SourceProject, filepath.Join(h.proj, ".claude", "settings.json")},
		{SourceUser, filepath.Join(h.ClaudeDir, "settings.json")},
		{SourceGlobal, filepath.Join(h.ClaudeDir, ".claude.json")},
	}
	for _, l := range layers {
		evalWriteJSON(t, l.path, map[string]any{KeyTeammateMode: l.source + "-mode", "other": 1})
	}
	argv := []string{"claude", "--settings", "lead-settings.json", "--teammate-mode", "cli-mode"}
	resolve := func() ModeDecision {
		return ResolveMode(ModeInput{LeadArgv: argv, LeadCwd: h.proj, Getenv: evalEnv()})
	}

	if d := resolve(); d.Mode != "cli-mode" || d.Source != SourceCLI {
		t.Fatalf("cli: got %q from %s", d.Mode, d.Source)
	}
	argv = []string{"claude", "--settings", "lead-settings.json"}
	for _, l := range layers {
		d := resolve()
		if d.Mode != l.source+"-mode" || d.Source != l.source {
			t.Fatalf("want %s-mode from %s, got %q from %s", l.source, l.source, d.Mode, d.Source)
		}
		if err := os.Remove(l.path); err != nil {
			t.Fatal(err)
		}
	}
	d := resolve()
	if d.Mode != "in-process" || d.Source != SourceDefault {
		t.Fatalf("default: got %q from %s", d.Mode, d.Source)
	}
	if d.Code != CodeModeInProcess || d.Detail != DetailModeInProcess+":"+SourceDefault {
		t.Errorf("default decision %s/%s", d.Code, d.Detail)
	}

	t.Run("inline flag settings", func(t *testing.T) {
		argv := []string{"claude", "--settings={\"teammateMode\":\"tmux\"}"}
		d := ResolveMode(ModeInput{LeadArgv: argv, LeadCwd: h.proj, Getenv: evalEnv()})
		if d.Mode != "tmux" || d.Source != SourceFlag || d.Code != "" {
			t.Fatalf("got %+v", d)
		}
	})
	t.Run("managed drop-in overrides base", func(t *testing.T) {
		evalWriteJSON(t, filepath.Join(h.managed, "managed-settings.json"), map[string]any{KeyTeammateMode: "tmux"})
		evalWriteJSON(t, filepath.Join(h.managed, "managed-settings.d", "10-a.json"), map[string]any{KeyTeammateMode: "auto"})
		evalWriteJSON(t, filepath.Join(h.managed, "managed-settings.d", "20-b.json"), map[string]any{KeyTeammateMode: "in-process"})
		evalWriteFile(t, filepath.Join(h.managed, "managed-settings.d", "30-broken.json"), "{")
		d := ResolveMode(ModeInput{LeadArgv: []string{"claude"}, LeadCwd: h.proj, Getenv: evalEnv()})
		if d.Mode != "in-process" || d.Source != SourcePolicy {
			t.Fatalf("got %q from %s", d.Mode, d.Source)
		}
	})
	t.Run("empty and non-string values are skipped", func(t *testing.T) {
		for _, p := range []string{"managed-settings.json", "managed-settings.d"} {
			if err := os.RemoveAll(filepath.Join(h.managed, p)); err != nil {
				t.Fatal(err)
			}
		}
		evalWriteFile(t, filepath.Join(h.proj, ".claude", "settings.local.json"), `{"teammateMode":""}`)
		evalWriteFile(t, filepath.Join(h.proj, ".claude", "settings.json"), `{"teammateMode":7}`)
		evalWriteFile(t, filepath.Join(h.ClaudeDir, "settings.json"), `{"teammateMode":"iterm2"}`)
		d := ResolveMode(ModeInput{LeadArgv: []string{"claude", "--teammate-mode="}, LeadCwd: h.proj, Getenv: evalEnv()})
		if d.Mode != "iterm2" || d.Source != SourceUser {
			t.Fatalf("got %q from %s", d.Mode, d.Source)
		}
	})
}

// TestFxtmModeSettingSources: the lead's --setting-sources limits the user,
// project and local layers; policy and --settings are always read.
func TestFxtmModeSettingSources(t *testing.T) {
	h := evalModeSetup(t)
	user := filepath.Join(h.ClaudeDir, "settings.json")
	project := filepath.Join(h.proj, ".claude", "settings.json")
	local := filepath.Join(h.proj, ".claude", "settings.local.json")
	evalWriteJSON(t, user, map[string]any{KeyTeammateMode: "in-process"})
	evalWriteJSON(t, local, map[string]any{KeyTeammateMode: "tmux", "availableModels": []string{"sonnet"}})
	resolve := func(argv ...string) ModeDecision {
		return ResolveMode(ModeInput{LeadArgv: append([]string{"claude"}, argv...), LeadCwd: h.proj, Getenv: evalEnv("TMUX", "x")})
	}

	if d := resolve(); d.Mode != "tmux" || d.Source != SourceLocal || !reflect.DeepEqual(d.Blockers, []string{BlockerAvailableModels}) {
		t.Fatalf("no --setting-sources: got %q from %s, blockers %v", d.Mode, d.Source, d.Blockers)
	}
	d := resolve("--setting-sources", "user")
	if d.Mode != "in-process" || d.Source != SourceUser || d.Code != CodeModeInProcess || len(d.Blockers) != 0 {
		t.Fatalf("--setting-sources user: got %q from %s (%s), blockers %v", d.Mode, d.Source, d.Code, d.Blockers)
	}
	evalWriteJSON(t, project, map[string]any{KeyTeammateMode: "auto"})
	if d := resolve("--setting-sources=project, user"); d.Mode != "auto" || d.Source != SourceProject {
		t.Fatalf("--setting-sources=project, user: got %q from %s", d.Mode, d.Source)
	}
	if d := resolve("--setting-sources", ""); d.Mode != "in-process" || d.Source != SourceDefault {
		t.Fatalf("--setting-sources \"\": got %q from %s", d.Mode, d.Source)
	}
	// Policy and --settings stay in the chain whatever the sources.
	evalWriteJSON(t, filepath.Join(h.managed, "managed-settings.json"), map[string]any{"fallbackModel": "haiku"})
	d = resolve("--setting-sources", "user", "--settings", `{"teammateMode":"iterm2"}`)
	if d.Mode != "iterm2" || d.Source != SourceFlag || !reflect.DeepEqual(d.Blockers, []string{BlockerFallbackModel}) {
		t.Fatalf("policy/--settings: got %q from %s, blockers %v", d.Mode, d.Source, d.Blockers)
	}
}

func TestBackendMatrix(t *testing.T) {
	h := evalModeSetup(t)
	iterm := map[string][]string{
		"none":             nil,
		"TERM_PROGRAM":     {"TERM_PROGRAM", "iTerm.app"},
		"ITERM_SESSION_ID": {"ITERM_SESSION_ID", "w0t0p0:ABC"},
		"terminal":         {"terminal", "iTerm.app"},
	}
	for _, mode := range []string{"tmux", "auto", "iterm2", "in-process"} {
		for _, tmux := range []bool{false, true} {
			for sig, kv := range iterm {
				env := append([]string{"TERM_PROGRAM", "Apple_Terminal"}, kv...)
				if tmux {
					env = append(env, "TMUX", "/private/tmp/tmux-501/default,1,0")
				}
				d := ResolveMode(ModeInput{
					LeadArgv: []string{"claude", "--teammate-mode", mode},
					LeadCwd:  h.proj,
					Getenv:   evalEnv(env...),
				})
				name := mode + "/tmux=" + map[bool]string{false: "no", true: "yes"}[tmux] + "/iterm=" + sig
				inITerm := sig != "none"
				wantHint := HintTmuxExternal
				switch {
				case tmux:
					wantHint = HintTmux
				case inITerm:
					wantHint = HintITerm2OrTmuxExternal
				}
				var wantCode, wantDetail, wantWarn string
				switch mode {
				case "in-process":
					wantCode, wantDetail = CodeModeInProcess, DetailModeInProcess+":"+SourceCLI
				case "auto":
					if !tmux && !inITerm {
						wantCode, wantDetail = CodeModeInProcess, DetailAutoNoPaneTerminal
					} else {
						wantWarn = WarnAutoMayFallback
					}
				}
				if d.InTmux != tmux || d.InITerm2 != inITerm || d.BackendHint != wantHint ||
					d.Code != wantCode || d.Detail != wantDetail || d.Warning != wantWarn || d.Source != SourceCLI {
					t.Errorf("%s: got %+v; want tmux=%v iterm=%v hint=%s code=%q detail=%q warn=%q",
						name, d, tmux, inITerm, wantHint, wantCode, wantDetail, wantWarn)
				}
			}
		}
	}
}

func TestSentinelBlockers(t *testing.T) {
	const agentType = "ccf-glm"
	h := evalModeSetup(t)
	started := time.Now().Add(-time.Minute)
	defPath := filepath.Join(h.ClaudeDir, "agents", agentType+".md")
	evalWriteFile(t, defPath, "---\nname: ccf-glm\n---\n")
	old := started.Add(-time.Hour)
	if err := os.Chtimes(defPath, old, old); err != nil {
		t.Fatal(err)
	}
	type env struct {
		argv []string
		kv   []string
	}
	cases := []struct {
		name    string
		setup   func(t *testing.T) env
		blocker []string
	}{
		{"none", func(*testing.T) env { return env{} }, nil},
		{"SUBAGENT_MODEL_FORCE", func(*testing.T) env { return env{kv: []string{EnvSubagentModelForce, "1"}} }, []string{BlockerSubagentModelForce}},
		{"availableModels in project settings", func(t *testing.T) env {
			evalWriteFile(t, filepath.Join(h.proj, ".claude", "settings.json"), `{"availableModels":["sonnet"]}`)
			return env{}
		}, []string{BlockerAvailableModels}},
		{"availableModels in managed settings", func(t *testing.T) env {
			evalWriteFile(t, filepath.Join(h.managed, "managed-settings.json"), `{"availableModels":[]}`)
			return env{}
		}, []string{BlockerAvailableModels}},
		{"fallbackModel in user settings", func(t *testing.T) env {
			evalWriteFile(t, filepath.Join(h.ClaudeDir, "settings.json"), `{"fallbackModel":"haiku"}`)
			return env{}
		}, []string{BlockerFallbackModel}},
		{"fallbackModel in inline --settings", func(*testing.T) env {
			return env{argv: []string{"--settings", `{"fallbackModel":"haiku"}`}}
		}, []string{BlockerFallbackModel}},
		{"--fallback-model", func(*testing.T) env { return env{argv: []string{"--fallback-model=haiku"}} }, []string{BlockerFallbackModel}},
		{"definition newer than lead", func(t *testing.T) env {
			if err := os.Chtimes(defPath, time.Now(), time.Now()); err != nil {
				t.Fatal(err)
			}
			return env{}
		}, []string{BlockerAgentDefNewer}},
		{"several", func(t *testing.T) env {
			evalWriteFile(t, filepath.Join(h.proj, ".claude", "settings.local.json"), `{"availableModels":["opus"],"fallbackModel":"haiku"}`)
			return env{kv: []string{EnvSubagentModelForce, "1"}}
		}, []string{BlockerSubagentModelForce, BlockerAvailableModels, BlockerFallbackModel}},
		{"null values do not count", func(t *testing.T) env {
			evalWriteFile(t, filepath.Join(h.ClaudeDir, "settings.json"), `{"availableModels":null,"fallbackModel":""}`)
			return env{}
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, p := range []string{
				filepath.Join(h.proj, ".claude"), filepath.Join(h.managed, "managed-settings.json"), filepath.Join(h.ClaudeDir, "settings.json"),
			} {
				if err := os.RemoveAll(p); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chtimes(defPath, old, old); err != nil {
				t.Fatal(err)
			}
			e := tc.setup(t)
			for _, mode := range []string{"auto", "tmux"} {
				argv := append([]string{"claude", "--teammate-mode", mode}, e.argv...)
				d := ResolveMode(ModeInput{
					LeadArgv:      argv,
					LeadCwd:       h.proj,
					LeadStartedAt: started.UnixMilli(),
					AgentType:     agentType,
					Getenv:        evalEnv(append([]string{"TMUX", "/private/tmp/tmux-501/default,1,0"}, e.kv...)...),
				})
				if !reflect.DeepEqual(d.Blockers, tc.blocker) {
					t.Errorf("%s: blockers %v, want %v", mode, d.Blockers, tc.blocker)
				}
				switch {
				case mode == "tmux" || len(tc.blocker) == 0:
					// explicit pane mode never falls back; auto with an intact sentinel is allowed
					if d.Code != "" {
						t.Errorf("%s: refused %s/%s", mode, d.Code, d.Detail)
					}
				default:
					want := DetailAutoFallbackUnguarded + ":" + strings.Join(tc.blocker, ",")
					if d.Code != CodeModeInProcess || d.Detail != want {
						t.Errorf("%s: got %s/%s, want %s/%s", mode, d.Code, d.Detail, CodeModeInProcess, want)
					}
				}
			}
		})
	}
}
