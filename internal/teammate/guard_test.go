package teammate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// guardReason decodes one deny line and returns its reason, failing the test
// unless the line is exactly the hook's deny shape.
func guardReason(t *testing.T, out string) string {
	t.Helper()
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("deny output must be one line, got %q", out)
	}
	var m map[string]map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("deny output %q: %v", out, err)
	}
	hso, ok := m["hookSpecificOutput"]
	if len(m) != 1 || !ok || len(hso) != 3 || hso["hookEventName"] != "PreToolUse" || hso["permissionDecision"] != "deny" {
		t.Fatalf("deny output shape: %s", out)
	}
	return hso["permissionDecisionReason"]
}

// evalHookInput renders a PreToolUse(Agent) payload from testdata with tool_input overridden.
func evalHookInput(t *testing.T, toolName string, toolInput map[string]any) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "eval", "pretooluse-agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	payload["tool_name"] = toolName
	ti := payload["tool_input"].(map[string]any)
	for k, v := range toolInput {
		if v == nil {
			delete(ti, k)
		} else {
			ti[k] = v
		}
	}
	out, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestGuardStdinMatrix(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(*testing.T, *evalFixture)
		stdin     func(*testing.T) string
		rc        int
		denyStart string // "" = no output expected
	}{
		{"allowed", nil, func(t *testing.T) string { return evalHookInput(t, "Agent", nil) }, 0, ""},
		{"not the Agent tool", nil, func(t *testing.T) string { return evalHookInput(t, "Task", map[string]any{"name": nil}) }, 0, ""},
		{"native type", func(_ *testing.T, f *evalFixture) { f.detectOK = false },
			func(t *testing.T) string {
				return evalHookInput(t, "Agent", map[string]any{"subagent_type": "general-purpose", "name": nil})
			}, 0, ""},
		{"no type", nil, func(t *testing.T) string { return evalHookInput(t, "Agent", map[string]any{"subagent_type": nil}) }, 0, ""},
		{"missing name", nil, func(t *testing.T) string { return evalHookInput(t, "Agent", map[string]any{"name": nil}) }, 0,
			"cc-fleet: BAD_AGENT_CALL(missing_name): "},
		{"model", nil, func(t *testing.T) string { return evalHookInput(t, "Agent", map[string]any{"model": "opus"}) }, 0,
			"cc-fleet: BAD_AGENT_CALL(model_param): "},
		{"isolation", nil, func(t *testing.T) string { return evalHookInput(t, "Agent", map[string]any{"isolation": "worktree"}) }, 0,
			"cc-fleet: BAD_AGENT_CALL(isolation): "},
		{"cwd", nil, func(t *testing.T) string { return evalHookInput(t, "Agent", map[string]any{"cwd": "/tmp"}) }, 0,
			"cc-fleet: BAD_AGENT_CALL(cwd): "},
		{"bad slot", nil, func(t *testing.T) string {
			return evalHookInput(t, "Agent", map[string]any{"subagent_type": "ccf-glm.huge"})
		}, 0,
			"cc-fleet: BAD_ARGS: "},
		{"launcher missing", func(t *testing.T, f *evalFixture) {
			t.Setenv(EnvTeammateCommand, "")
			evalWriteJSON(t, filepath.Join(f.h.ClaudeDir, "settings.json"), map[string]any{"teammateMode": "tmux"})
		}, func(t *testing.T) string { return evalHookInput(t, "Agent", nil) }, 0,
			"cc-fleet: TEAMMATE_SETUP_REQUIRED(launcher_not_configured): "},
		{"in-process", func(_ *testing.T, f *evalFixture) { f.argv = []string{"claude", "--teammate-mode", "in-process"} },
			func(t *testing.T) string { return evalHookInput(t, "Agent", nil) }, 0,
			"cc-fleet: TEAMMATE_MODE_IN_PROCESS(mode_in_process:cli): "},
		{"not JSON", nil, func(*testing.T) string { return `{"tool_name":"Agent",` }, 1, ""},
		{"wrong field type", nil, func(t *testing.T) string { return evalHookInput(t, "Agent", map[string]any{"name": 7}) }, 1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := evalSetup(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			var out bytes.Buffer
			rc := Guard(context.Background(), Protocol, strings.NewReader(tc.stdin(t)), &out)
			if rc != tc.rc {
				t.Fatalf("exit %d, want %d (output %q)", rc, tc.rc, out.String())
			}
			if strings.Contains(out.String(), `"allow"`) {
				t.Fatalf("guard printed an allow decision: %s", out.String())
			}
			if tc.denyStart == "" {
				if out.Len() != 0 {
					t.Fatalf("want no output, got %q", out.String())
				}
			} else if reason := guardReason(t, out.String()); !strings.HasPrefix(reason, tc.denyStart) || !strings.Contains(reason, " — ") {
				t.Fatalf("reason %q, want prefix %q and a suggestion", reason, tc.denyStart)
			}
			// the guard never prepares or probes
			if len(f.probed)+len(f.proxied) != 0 {
				t.Errorf("guard called probe/proxy seams: %v %v", f.probed, f.proxied)
			}
		})
	}
}

func TestGuardProtocolMismatch(t *testing.T) {
	evalSetup(t)
	for _, p := range []int{0, 2} {
		var out bytes.Buffer
		if rc := Guard(context.Background(), p, strings.NewReader(evalHookInput(t, "Agent", map[string]any{"name": nil})), &out); rc != 3 || out.Len() != 0 {
			t.Errorf("protocol %d: exit %d, output %q; want 3 and nothing", p, rc, out.String())
		}
	}
}

// evalTreeSnapshot lists every path under root with its size and mtime.
func evalTreeSnapshot(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, p+" "+fi.Mode().String()+" "+fi.ModTime().Format(time.RFC3339Nano)+" "+fmt.Sprint(fi.Size()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestGuardReadOnlyHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions are POSIX-only")
	}
	f := evalSetup(t)
	root := filepath.Dir(f.h.Home)
	dirs := []string{f.h.Home, f.h.ClaudeDir, f.h.ConfigHome, filepath.Join(f.h.ConfigHome, "cc-fleet")}
	for _, d := range dirs {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, d := range dirs {
			_ = os.Chmod(d, 0o755)
		}
	})
	before := evalTreeSnapshot(t, root)

	var out bytes.Buffer
	if rc := Guard(context.Background(), Protocol, strings.NewReader(evalHookInput(t, "Agent", nil)), &out); rc != 0 || out.Len() != 0 {
		t.Fatalf("allowed call on a read-only home: exit %d, output %q", rc, out.String())
	}
	out.Reset()
	if rc := Guard(context.Background(), Protocol, strings.NewReader(evalHookInput(t, "Agent", map[string]any{"subagent_type": "ccf-off"})), &out); rc != 0 {
		t.Fatalf("denied call on a read-only home: exit %d", rc)
	}
	if reason := guardReason(t, out.String()); !strings.HasPrefix(reason, "cc-fleet: PROVIDER_DISABLED: ") {
		t.Fatalf("reason %q", reason)
	}
	if after := evalTreeSnapshot(t, root); !reflect.DeepEqual(before, after) {
		t.Errorf("guard changed the file tree:\nbefore %v\nafter  %v", before, after)
	}
}

// evalGuardScript returns the absolute path of hooks/teammate-guard.sh.
func evalGuardScript(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the guard hook is a POSIX sh script")
	}
	p, err := filepath.Abs(filepath.Join("..", "..", "hooks", "teammate-guard.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// evalRunGuardScript runs the hook with /bin/sh, PATH = bin plus the system
// directories, and stdin; it returns stdout, stderr and the exit code.
func evalRunGuardScript(t *testing.T, bin, stdin string) (stdout, stderr string, rc int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", evalGuardScript(t))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + filepath.Dir(bin)}
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		rc = ee.ExitCode()
	default:
		t.Fatalf("run guard script: %v", err)
	}
	return o.String(), e.String(), rc
}

// evalFakeCCFleet writes a fake cc-fleet into dir that records its argv and
// stdin next to itself, then runs body.
func evalFakeCCFleet(t *testing.T, dir, body string) string {
	t.Helper()
	return writeScript(t, filepath.Join(dir, "cc-fleet"),
		`d=$(dirname "$0"); printf '%s\n' "$*" > "$d/args"; cat > "$d/stdin"`+"\n"+body)
}

const evalCcfInput = `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Agent","tool_input":{"name":"w","subagent_type":"ccf-glm","prompt":"do it"}}`

func TestGuardScriptSyntax(t *testing.T) {
	if out, err := exec.Command("/bin/sh", "-n", evalGuardScript(t)).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
}

func TestGuardScriptNoCCFleetOnPath(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	out, errOut, rc := evalRunGuardScript(t, bin, evalCcfInput)
	if rc != 2 || out != "" || !strings.Contains(errOut, "TEAMMATE_SETUP_REQUIRED("+DetailCCFleetNotOnPath+")") {
		t.Fatalf("exit %d, stdout %q, stderr %q", rc, out, errOut)
	}
}

func TestGuardScriptBlocksWhenGuardFails(t *testing.T) {
	for _, code := range []string{"1", "3", "127"} {
		bin := filepath.Join(t.TempDir(), "bin")
		evalFakeCCFleet(t, bin, "echo partial; exit "+code+"\n")
		out, errOut, rc := evalRunGuardScript(t, bin, evalCcfInput)
		if rc != 2 || out != "" || !strings.Contains(errOut, "teammate guard unavailable (exit "+code+")") {
			t.Errorf("guard exit %s: script exit %d, stdout %q, stderr %q", code, rc, out, errOut)
		}
	}
}

func TestGuardScriptPassesDenyThrough(t *testing.T) {
	deny := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"cc-fleet: X: y — z"}}`
	bin := filepath.Join(t.TempDir(), "bin")
	evalFakeCCFleet(t, bin, "printf '%s\\n' '"+deny+"'\n")
	out, errOut, rc := evalRunGuardScript(t, bin, evalCcfInput)
	if rc != 0 || out != deny+"\n" || errOut != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", rc, out, errOut)
	}
	args, _ := os.ReadFile(filepath.Join(bin, "args"))
	stdin, _ := os.ReadFile(filepath.Join(bin, "stdin"))
	if string(args) != "teammate guard --protocol 1\n" || string(stdin) != evalCcfInput {
		t.Errorf("cc-fleet got args %q, stdin %q", args, stdin)
	}

	// allowed: exit 0 with no output stays silent
	evalFakeCCFleet(t, bin, "exit 0\n")
	if out, errOut, rc := evalRunGuardScript(t, bin, evalCcfInput); rc != 0 || out != "" || errOut != "" {
		t.Fatalf("allow: exit %d, stdout %q, stderr %q", rc, out, errOut)
	}
	// the spaced JSON form also reaches cc-fleet
	spaced := `{"tool_name": "Agent", "tool_input": {"name": "w", "subagent_type": "ccf-glm"}}`
	evalFakeCCFleet(t, bin, "printf '%s\\n' '"+deny+"'\n")
	if out, _, rc := evalRunGuardScript(t, bin, spaced); rc != 0 || out != deny+"\n" {
		t.Fatalf("spaced: exit %d, stdout %q", rc, out)
	}
}

func TestGuardScriptSkipsNativeTypes(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin")
	evalFakeCCFleet(t, bin, "exit 1\n")
	for _, in := range []string{
		`{"tool_name":"Agent","tool_input":{"name":"w","subagent_type":"general-purpose","prompt":"mention ccf-glm here"}}`,
		`{"tool_name":"Agent","tool_input":{"prompt":"no type"}}`,
		`{"tool_name":"Bash","tool_input":{"command":"ls"}}`,
	} {
		out, errOut, rc := evalRunGuardScript(t, bin, in)
		if rc != 0 || out != "" || errOut != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", in, rc, out, errOut)
		}
		if _, err := os.Stat(filepath.Join(bin, "args")); err == nil {
			t.Fatalf("%s: cc-fleet was called", in)
		}
	}
}

func TestGuardScriptRegisteredInHooksJSON(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	type hook struct{ Type, Command string }
	type entry struct {
		Matcher string
		Hooks   []hook
	}
	var cfg struct{ Hooks map[string][]entry }
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	want := []entry{{Matcher: "Agent", Hooks: []hook{{Type: "command", Command: `sh "${CLAUDE_PLUGIN_ROOT}/hooks/teammate-guard.sh"`}}}}
	if !reflect.DeepEqual(cfg.Hooks["PreToolUse"], want) {
		t.Errorf("PreToolUse = %+v, want %+v", cfg.Hooks["PreToolUse"], want)
	}
	start := []entry{{Matcher: "startup", Hooks: []hook{{Type: "command", Command: `node "${CLAUDE_PLUGIN_ROOT}/hooks/check-cc-fleet.js"`}}}}
	if !reflect.DeepEqual(cfg.Hooks["SessionStart"], start) {
		t.Errorf("SessionStart changed: %+v", cfg.Hooks["SessionStart"])
	}
}
