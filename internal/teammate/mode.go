package teammate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/claudepaths"
)

type ModeInput struct {
	LeadArgv      []string
	LeadCwd       string
	LeadStartedAt int64  // unix ms
	AgentType     string // for the agent_def_newer_than_lead check
	Getenv        func(string) string
}

type ModeDecision struct {
	Mode        string
	Source      string
	InTmux      bool
	InITerm2    bool
	Blockers    []string
	BackendHint string
	Code        string // "" means allowed
	Detail      string
	Warning     string // may be WarnAutoMayFallback when allowed
}

// managedSettingsDirs returns the directories holding file-based managed
// (policy) settings. A seam: tests point it at a temp dir so the machine's real
// policy files never leak in.
var managedSettingsDirs = func() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"/Library/Application Support/ClaudeCode"}
	case "linux":
		return []string{"/etc/claude-code"}
	}
	return nil
}

// settingsLayer is one parsed settings source, keyed by top-level key. path is
// the file it came from ("" for inline --settings JSON).
type settingsLayer struct {
	source string
	path   string
	keys   map[string]json.RawMessage
}

// ResolveMode replicates how the lead picks its teammate backend (Evaluate
// step 9): the effective teammateMode with its source, the pane-terminal
// signals, and the blockers that disable the in-process sentinel. Only
// deterministic, readable signals are used; MDM and server-pushed policy are a
// known blind spot.
func ResolveMode(in ModeInput) ModeDecision {
	d, _ := resolveMode(in)
	return d
}

// resolveMode is ResolveMode plus the settings file the effective
// teammateMode was read from ("" when it came from the CLI, inline JSON, the
// global config or the default).
func resolveMode(in ModeInput) (ModeDecision, string) {
	getenv := in.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	layers := modeSettingsLayers(in.LeadArgv, in.LeadCwd)

	d := ModeDecision{Mode: "in-process", Source: SourceDefault}
	modeFile := ""
	if v := argvFlag(in.LeadArgv, "--teammate-mode"); v != "" {
		d.Mode, d.Source = v, SourceCLI
	} else if v, l := layersTeammateMode(layers); v != "" {
		d.Mode, d.Source, modeFile = v, l.source, l.path
	} else if v := globalConfigTeammateMode(); v != "" {
		d.Mode, d.Source = v, SourceGlobal
	}

	d.InTmux = getenv("TMUX") != ""
	d.InITerm2 = getenv("TERM_PROGRAM") == "iTerm.app" || getenv("ITERM_SESSION_ID") != "" || getenv("terminal") == "iTerm.app"
	switch {
	case d.InTmux:
		d.BackendHint = HintTmux
	case d.InITerm2:
		d.BackendHint = HintITerm2OrTmuxExternal
	default:
		d.BackendHint = HintTmuxExternal
	}
	d.Blockers = sentinelBlockers(in, layers, getenv)

	switch d.Mode {
	case "tmux", "iterm2":
	case "in-process":
		d.Code, d.Detail = CodeModeInProcess, DetailModeInProcess+":"+d.Source
	case "auto":
		switch {
		case !d.InTmux && !d.InITerm2:
			d.Code, d.Detail = CodeModeInProcess, DetailAutoNoPaneTerminal
		case len(d.Blockers) > 0:
			d.Code, d.Detail = CodeModeInProcess, DetailAutoFallbackUnguarded+":"+strings.Join(d.Blockers, ",")
		default:
			d.Warning = WarnAutoMayFallback
		}
	default:
		d.Code, d.Detail = CodeModeInProcess, DetailModeUnknown+":"+d.Mode
	}
	return d, modeFile
}

// modeSettingsLayers reads the settings chain in precedence order: policy
// (managed-settings.json, then managed-settings.d/*.json by name), the lead's
// --settings (file or inline JSON), local, project, user. The lead's
// --setting-sources, when given, limits the local, project and user layers to
// those it lists. Unreadable or non-object sources are skipped.
func modeSettingsLayers(argv []string, cwd string) []settingsLayer {
	var out []settingsLayer
	add := func(source, path string, data []byte) {
		var keys map[string]json.RawMessage
		if json.Unmarshal(data, &keys) == nil && keys != nil {
			out = append(out, settingsLayer{source: source, path: path, keys: keys})
		}
	}
	addFile := func(source, path string) {
		if data, err := os.ReadFile(path); err == nil {
			add(source, path, data)
		}
	}
	enabled := func(string) bool { return true }
	if v, ok := argvLookup(argv, "--setting-sources"); ok {
		listed := map[string]bool{}
		for _, s := range strings.Split(v, ",") {
			listed[strings.TrimSpace(s)] = true
		}
		enabled = func(name string) bool { return listed[name] }
	}

	// Managed drop-ins merge over the base file, later names winning, so they
	// are listed first-wins in reverse.
	for _, dir := range managedSettingsDirs() {
		dropins, _ := filepath.Glob(filepath.Join(dir, "managed-settings.d", "*.json"))
		sort.Sort(sort.Reverse(sort.StringSlice(dropins)))
		for _, p := range dropins {
			addFile(SourcePolicy, p)
		}
		addFile(SourcePolicy, filepath.Join(dir, "managed-settings.json"))
	}
	if v := argvFlag(argv, "--settings"); v != "" {
		if trimmed := bytes.TrimSpace([]byte(v)); len(trimmed) > 0 && trimmed[0] == '{' {
			add(SourceFlag, "", trimmed)
		} else {
			if !filepath.IsAbs(v) && cwd != "" {
				v = filepath.Join(cwd, v)
			}
			addFile(SourceFlag, v)
		}
	}
	if cwd != "" {
		if enabled("local") {
			addFile(SourceLocal, filepath.Join(cwd, ".claude", "settings.local.json"))
		}
		if enabled("project") {
			addFile(SourceProject, filepath.Join(cwd, ".claude", "settings.json"))
		}
	}
	if p := claudepaths.Settings(); p != "" && enabled("user") {
		addFile(SourceUser, p)
	}
	return out
}

// layersTeammateMode returns the first non-empty string teammateMode in the
// chain and the layer holding it.
func layersTeammateMode(layers []settingsLayer) (string, settingsLayer) {
	for _, l := range layers {
		if s := rawString(l.keys[KeyTeammateMode]); s != "" {
			return s, l
		}
	}
	return "", settingsLayer{}
}

// globalConfigTeammateMode reads the legacy teammateMode from Claude Code's global config.
func globalConfigTeammateMode() string {
	p := claudepaths.GlobalConfig()
	if p == "" {
		return ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var cfg struct {
		TeammateMode json.RawMessage `json:"teammateMode"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	return rawString(cfg.TeammateMode)
}

// sentinelBlockers lists what would let an in-process teammate run on a real
// Claude model despite the sentinel model in its definition.
func sentinelBlockers(in ModeInput, layers []settingsLayer, getenv func(string) string) []string {
	var out []string
	if getenv(EnvSubagentModelForce) != "" {
		out = append(out, BlockerSubagentModelForce)
	}
	if anyLayerHas(layers, "availableModels") {
		out = append(out, BlockerAvailableModels)
	}
	if anyLayerHas(layers, "fallbackModel") || argvFlag(in.LeadArgv, "--fallback-model") != "" {
		out = append(out, BlockerFallbackModel)
	}
	if in.AgentType != "" {
		if dir := claudepaths.Agents(); dir != "" {
			fi, err := os.Stat(filepath.Join(dir, in.AgentType+".md"))
			if err == nil && fi.ModTime().UnixMilli() > in.LeadStartedAt {
				out = append(out, BlockerAgentDefNewer)
			}
		}
	}
	return out
}

// anyLayerHas reports whether any layer sets key to something other than null or "".
func anyLayerHas(layers []settingsLayer, key string) bool {
	for _, l := range layers {
		raw, ok := l.keys[key]
		if !ok {
			continue
		}
		if s := string(bytes.TrimSpace(raw)); s != "null" && s != `""` {
			return true
		}
	}
	return false
}

// rawString decodes a JSON string value; anything else reads as "".
func rawString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// argvFlag returns the last value of a long flag in argv ("--f v" or
// "--f=v"), skipping argv[0] and stopping at "--".
func argvFlag(argv []string, flag string) string {
	v, _ := argvLookup(argv, flag)
	return v
}

// argvLookup is argvFlag that also reports whether the flag was given, so an
// explicit empty value can be told from an absent flag.
func argvLookup(argv []string, flag string) (val string, found bool) {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			break
		}
		if a == flag && i+1 < len(argv) {
			val, found = argv[i+1], true
			i++
		} else if v, ok := strings.CutPrefix(a, flag+"="); ok {
			val, found = v, true
		}
	}
	return val, found
}
