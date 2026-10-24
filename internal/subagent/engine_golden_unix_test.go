//go:build !windows

package subagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/diag"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/engine_golden")

// Characterization of what Run hands the claude child and what it returns: the
// argv, the worker env, the Result (with its unserialized fields), the slim
// sidecar, the job files, and which side effects (daemon ensure, profile write)
// a failed gate leaves out. Any refactor of the launch path must keep every
// golden byte-identical. Regenerate with
// `go test ./internal/subagent -run TestEngineGolden -update`.

const goldenProviders = `version = 1

[glm]
base_url        = "https://example.invalid/anthropic"
default_model   = "glm-4.6"
models_endpoint = "https://example.invalid/v1/models"
secret_backend  = "file"
secret_ref      = "glm.key"
enabled         = true
added_at        = 2026-05-24T05:00:00Z

[glmfx]
base_url        = "https://fx.example.invalid/anthropic"
default_model   = "fx-default"
strong_model    = "fx-strong"
effort          = "high"
strong_effort   = "max"
models_endpoint = "https://fx.example.invalid/v1/models"
secret_backend  = "file"
secret_ref      = "glmfx.key"
enabled         = true
added_at        = 2026-05-24T05:00:00Z

[cdx]
base_url        = "http://127.0.0.1:17299/"
default_model   = "gpt-5.5"
models_endpoint = "http://127.0.0.1:17299/v1/models"
secret_backend  = "codex-oauth"
secret_ref      = "codex-oauth"
enabled         = true
added_at        = 2026-06-08T05:00:00Z
`

// The fake claude records its argv (NUL-separated, then $0 apart), an
// allowlisted env subset, its cwd and its stdin, then replays the case's stdout/stderr/exit. Each record is
// renamed into place so a reader never sees a partial file.
// goldenScrubbed is childenv.Clean's drop list, spelled out so shrinking that
// list still fails here: each is planted on the host and must never reach the
// child.
var goldenScrubbed = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_BASE_URL",
	"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS",
	"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_EFFORT_LEVEL",
}

// goldenEnvPattern is the env names the fake records: the worker vars a child
// must carry, plus every scrubbed name.
func goldenEnvPattern() string {
	keys := append([]string{
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "CLAUDE_CODE_GB_DISK_CACHE_WHEN_TELEMETRY_OFF",
		"CLAUDE_CODE_DISABLE_WORKFLOWS", "CLAUDE_CODE_DISABLE_CLAUDE_MDS",
	}, goldenScrubbed...)
	return strings.Join(keys, "|")
}

const goldenFakeClaude = `#!/bin/sh
d='%s'
for a in "$@"; do printf '%%s\0' "$a"; done > "$d/argv.tmp" && mv "$d/argv.tmp" "$d/argv"
printf '%%s\n' "$0" > "$d/argv0.tmp" && mv "$d/argv0.tmp" "$d/argv0"
pwd -P > "$d/cwd.tmp" && mv "$d/cwd.tmp" "$d/cwd"
env | grep -E '^(%s)=' | LC_ALL=C sort > "$d/env.tmp"
mv "$d/env.tmp" "$d/env"
cat > "$d/stdin.tmp"; mv "$d/stdin.tmp" "$d/stdin"
cat "$d/stdout.txt"
cat "$d/stderr.txt" >&2
: > "$d/done"
exit "$(cat "$d/exit")"
`

const (
	goldenStream = `{"type":"system","subtype":"init","session_id":"84c5b474-aaaa"}
{"type":"assistant","message":{"content":[{"type":"text","text":"working"},{"type":"tool_use","name":"Read","input":{"file_path":"/src/main.go"}}],"usage":{"input_tokens":10,"output_tokens":2}}}
` + smokeSuccessJSON + "\n"
	goldenSchemaJSON = `{"type":"result","subtype":"success","is_error":false,"api_error_status":null,"duration_ms":1200,"duration_api_ms":1000,"num_turns":2,"stop_reason":"end_turn","session_id":"84c5b474-bbbb","total_cost_usd":0.01,"usage":{"input_tokens":100,"output_tokens":20},"modelUsage":{"glm-4.6":{"inputTokens":100,"outputTokens":20,"costUSD":0.01}},"result":"","structured_output":{"answer":42},"permission_denials":[],"terminal_reason":"completed"}`
	golden401JSON    = `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"duration_ms":50,"duration_api_ms":0,"num_turns":1,"stop_reason":"stop_sequence","session_id":"84c5b474-cccc","total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0},"modelUsage":{},"result":"Failed to authenticate. API Error: 401 {\"error\":{\"message\":\"invalid api key\"}}","permission_denials":[],"terminal_reason":"completed"}`
)

type goldenEnv struct {
	xdg, home, wd, out, bin string
	proxyCalls              []string // providers ensureProviderProxy was asked for, in order
	proxyErr                error
}

type goldenCase struct {
	name    string
	req     Request
	reader  string // "" | "file" | "strings": feed the prompt through PromptReader
	stdout  string
	stderr  string
	exit    int
	version string // claude version the binary gate reports; "" → above the slim floor
	setup   func(t *testing.T, g *goldenEnv, req *Request)
}

func goldenCases() []goldenCase {
	var cases []goldenCase
	formats := []struct {
		name   string
		apply  func(*Request)
		stdout string
	}{
		{"json", func(r *Request) { r.JSON = true }, smokeSuccessJSON},
		{"stream", func(r *Request) { r.JSON, r.StreamActivity = true, true }, goldenStream},
		{"text", func(*Request) {}, "plain answer\n"},
	}
	for _, prof := range []string{ProfileFull, ProfileSlim, ProfileSlimRO} {
		for _, reader := range []string{"", "strings"} {
			for _, f := range formats {
				req := Request{Provider: "glm", PromptProfile: prof}
				f.apply(&req)
				in := "prompt"
				if reader != "" {
					in = "reader"
				}
				cases = append(cases, goldenCase{
					name: prof + "-" + in + "-" + f.name, req: req, reader: reader, stdout: f.stdout,
				})
			}
		}
	}
	return append(cases,
		goldenCase{name: "native-literal-model", req: Request{Provider: config.ReservedNativeProvider, Model: "opus", JSON: true}, stdout: smokeSuccessJSON},
		goldenCase{name: "native-default-model-slim-ro", req: Request{Provider: config.ReservedNativeProvider, JSON: true, PromptProfile: ProfileSlimRO}, stdout: smokeSuccessJSON},
		goldenCase{name: "effort-default-slot", req: Request{Provider: "glmfx", JSON: true}, stdout: smokeSuccessJSON},
		goldenCase{name: "effort-strong-slot-slim", req: Request{Provider: "glmfx", Model: "strong", JSON: true, PromptProfile: ProfileSlim}, stdout: smokeSuccessJSON},
		goldenCase{name: "permission-mode", req: Request{Provider: "glm", PermissionMode: "plan", JSON: true}, stdout: smokeSuccessJSON},
		goldenCase{name: "resume", req: Request{Provider: "glm", Resume: "84c5b474-aaaa", JSON: true}, stdout: smokeSuccessJSON},
		goldenCase{name: "json-schema", req: Request{Provider: "glm", JSONSchema: `{"type":"object","properties":{"answer":{"type":"integer"}}}`, JSON: true, StreamActivity: true, PromptProfile: ProfileSlim}, stdout: goldenSchemaJSON + "\n"},
		goldenCase{name: "disallowed-tools", req: Request{Provider: "glm", DisallowedTools: "Bash(rm:*),mcp__x", JSON: true}, stdout: smokeSuccessJSON},
		goldenCase{name: "max-turns-budget", req: Request{Provider: "glm", MaxTurns: 3, MaxBudgetUSD: 0.5, JSON: true}, stdout: smokeSuccessJSON},
		goldenCase{name: "slim-tools-noskills-mcp", req: Request{Provider: "glm", PromptProfile: ProfileSlim, Tools: []string{"Read", "Grep"}, NoSkills: true, MCP: true, JSON: true}, stdout: smokeSuccessJSON},
		goldenCase{name: "slim-below-floor", req: Request{Provider: "glm", PromptProfile: ProfileSlim, JSON: true}, version: "2.1.10", stdout: smokeSuccessJSON},
		goldenCase{name: "persist-io", req: Request{Provider: "glm", JSON: true, PersistIO: true, IOPrompt: "persisted prompt"}, stdout: smokeSuccessJSON},
		goldenCase{name: "error-401", req: Request{Provider: "glm", JSON: true}, stdout: golden401JSON, exit: 1},
		goldenCase{name: "text-nonzero-exit", req: Request{Provider: "glm"}, stderr: "boom: worker crashed\n", exit: 3},
		goldenCase{
			name: "register-held", req: Request{Provider: "glm", JSON: true, PromptProfile: ProfileSlim, RunID: "run-g"},
			setup: func(t *testing.T, _ *goldenEnv, req *Request) {
				id := MintQueuedLeaf(*req, "glm-4.6")
				if id == "" {
					t.Fatal("mint queued leaf")
				}
				HoldLeaf(id)
				req.JobID = id
			},
		},
		goldenCase{
			name: "profile-write-fails", req: Request{Provider: "glm", JSON: true},
			setup: func(t *testing.T, g *goldenEnv, _ *Request) {
				// A regular file where the profiles dir belongs fails MkdirAll even as root.
				if err := os.MkdirAll(filepath.Join(g.home, ".claude"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(g.home, ".claude", "profiles"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		goldenCase{
			name: "binary-gate-fails", req: Request{Provider: "cdx", JSON: true},
			setup: func(t *testing.T, _ *goldenEnv, _ *Request) {
				orig := resolveBinaryPathVersion
				resolveBinaryPathVersion = func() (string, string, error) { return "", "", errors.New("no claude on PATH") }
				t.Cleanup(func() { resolveBinaryPathVersion = orig })
			},
		},
		goldenCase{
			name: "proxy-fails", req: Request{Provider: "cdx", JSON: true},
			setup: func(_ *testing.T, g *goldenEnv, _ *Request) { g.proxyErr = errors.New("daemon refused to start") },
		},
		goldenCase{name: "daemon-backed", req: Request{Provider: "cdx", JSON: true, PromptProfile: ProfileSlim}, stdout: smokeSuccessJSON},
		goldenCase{name: "bg-daemon-backed", req: Request{Provider: "cdx", Background: true, JSON: true}, stdout: goldenStream},
		goldenCase{name: "bg-file-slim-persist", req: Request{Provider: "glm", Background: true, JSON: true, PromptProfile: ProfileSlim, PersistIO: true, IOPrompt: "persisted prompt"}, reader: "file", stdout: goldenStream},
		goldenCase{name: "bg-strings-full", req: Request{Provider: "glmfx", Background: true}, reader: "strings", stdout: goldenStream},
		goldenCase{name: "bg-prompt-slim-ro-text", req: Request{Provider: "glm", Background: true, PromptProfile: ProfileSlimRO}, stdout: goldenStream},
	)
}

func TestEngineGolden(t *testing.T) {
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			g := &goldenEnv{xdg: t.TempDir(), home: t.TempDir(), wd: t.TempDir(), out: t.TempDir(), bin: t.TempDir()}
			t.Setenv("XDG_CONFIG_HOME", g.xdg)
			t.Setenv("HOME", g.home)
			t.Setenv("SHELL", "/bin/sh")
			// The slim prompt reports whether its cwd is in a git repo; never let
			// git find one above the temp dir (a TMPDIR inside a checkout).
			t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(g.wd))
			t.Setenv("CLAUDE_CODE_DISABLE_CLAUDE_MDS", "")
			os.Unsetenv("CLAUDE_CODE_DISABLE_CLAUDE_MDS")
			// Sentinels: Clean must drop the scrubbed ones, WithWorkerEnv must
			// override the host's worker var.
			for _, k := range goldenScrubbed {
				t.Setenv(k, "sentinel-"+k)
			}
			t.Setenv("CLAUDE_CODE_DISABLE_WORKFLOWS", "0")

			cfgDir := filepath.Join(g.xdg, "cc-fleet")
			mustWrite(t, filepath.Join(cfgDir, "providers.toml"), goldenProviders, 0o600)
			fake := filepath.Join(g.bin, "claude")
			mustWrite(t, fake, fmt.Sprintf(goldenFakeClaude, g.out, goldenEnvPattern()), 0o755)
			mustWrite(t, filepath.Join(g.out, "stdout.txt"), c.stdout, 0o600)
			mustWrite(t, filepath.Join(g.out, "stderr.txt"), c.stderr, 0o600)
			mustWrite(t, filepath.Join(g.out, "exit"), strconv.Itoa(c.exit), 0o600)
			version := c.version
			if version == "" {
				version = "2.1.150"
			}
			binStubResolver(t, fake, version)
			origProxy := ensureProviderProxy
			ensureProviderProxy = func(v *config.Provider, _ *diag.Logger) error {
				g.proxyCalls = append(g.proxyCalls, v.Name)
				return g.proxyErr
			}
			t.Cleanup(func() { ensureProviderProxy = origProxy })

			req := c.req
			req.WorkingDir = g.wd
			req.LeadSessionID = "lead-golden"
			const prompt = "golden prompt"
			switch c.reader {
			case "":
				req.Prompt = prompt
			case "strings":
				req.PromptReader = strings.NewReader(prompt)
			case "file":
				pf := filepath.Join(g.out, "prompt-in")
				mustWrite(t, pf, prompt, 0o600)
				f, err := os.Open(pf)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { f.Close() })
				req.PromptReader = f
			}
			if c.setup != nil {
				c.setup(t, g, &req)
			}

			res := Run(context.Background(), req)
			if req.Background && res.PID > 0 {
				var ws syscall.WaitStatus
				_, _ = syscall.Wait4(res.PID, &ws, 0, nil)
			}
			rec := goldenRecord(t, g, res)
			if req.Background && res.JobID != "" {
				// The detached child is reaped: StatusFor classifies its terminal .out.
				st, err := json.Marshal(StatusFor(res.JobID))
				if err != nil {
					t.Fatal(err)
				}
				rec += "== status\n" + maskJSON(t, st)
			}
			checkGolden(t, c.name, g.norm(rec))
		})
	}
}

func mustWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func goldenRecord(t *testing.T, g *goldenEnv, res Result) string {
	t.Helper()
	var b strings.Builder
	section := func(name, body string) {
		fmt.Fprintf(&b, "== %s\n%s", name, body)
		if body != "" && !strings.HasSuffix(body, "\n") {
			b.WriteString("\n")
		}
	}
	rj, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	section("result", maskJSON(t, rj))
	section("raw", string(res.Raw))
	section("exit_code", strconv.Itoa(res.ExitCode))
	section("structured_output", string(res.StructuredOutput))
	section("proxy_ensure", strings.Join(g.proxyCalls, "\n"))

	var profiles strings.Builder
	if entries, err := os.ReadDir(filepath.Join(g.home, ".claude", "profiles")); err == nil {
		for _, e := range entries {
			profiles.WriteString(e.Name() + "\n")
		}
	}
	section("profiles", profiles.String())

	if _, err := os.Stat(filepath.Join(g.out, "done")); err != nil {
		section("child", "not launched\n")
	} else {
		argv, _ := os.ReadFile(filepath.Join(g.out, "argv"))
		var lines strings.Builder
		for _, a := range strings.Split(strings.TrimSuffix(string(argv), "\x00"), "\x00") {
			fmt.Fprintf(&lines, "%q\n", a)
		}
		section("argv", lines.String())
		argv0, _ := os.ReadFile(filepath.Join(g.out, "argv0"))
		section("argv0", string(argv0))
		cwd, _ := os.ReadFile(filepath.Join(g.out, "cwd"))
		section("cwd", string(cwd))
		env, _ := os.ReadFile(filepath.Join(g.out, "env"))
		section("env", string(env))
		stdin, _ := os.ReadFile(filepath.Join(g.out, "stdin"))
		section("stdin", string(stdin))
	}

	dir := filepath.Join(g.xdg, "cc-fleet", jobsDirName)
	entries, _ := os.ReadDir(dir)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	section("jobs", "")
	for _, n := range names {
		body, _ := os.ReadFile(filepath.Join(dir, n))
		switch {
		case strings.HasSuffix(n, ".json"):
			section("jobs/"+n, maskJSON(t, body))
		case strings.HasSuffix(n, ".activity"):
			var recs strings.Builder
			for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
				recs.WriteString(maskJSON(t, []byte(line)))
			}
			section("jobs/"+n, recs.String())
		case strings.HasSuffix(n, ".slimprompt"), strings.HasSuffix(n, ".prompt"), strings.HasSuffix(n, ".answer"):
			section("jobs/"+n, string(body))
		default: // .out/.err: presence only
			section("jobs/"+n, fmt.Sprintf("(%d bytes)\n", len(body)))
		}
	}
	return b.String()
}

// maskJSON re-renders a JSON object with its process- and clock-dependent
// fields masked and keys sorted.
func maskJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	for _, k := range []string{"pid", "pgid", "proc_start", "started_at", "child_pid", "child_proc_start", "ts"} {
		if _, ok := m[k]; ok {
			m[k] = "<masked>"
		}
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

var (
	goldenUUIDRe    = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	goldenSlimEnvRe = regexp.MustCompile(`(?m)^(Platform|OS Version|Today's date): .*$`)
)

func (g *goldenEnv) norm(s string) string {
	type sub struct{ path, label string }
	var paths []sub
	subs := []sub{{g.xdg, "<XDG>"}, {g.home, "<HOME>"}, {g.wd, "<WD>"}, {g.out, "<OUT>"}, {g.bin, "<BIN>"}}
	// A background child ignores WorkingDir and inherits the caller's cwd.
	if pkg, err := os.Getwd(); err == nil {
		subs = append(subs, sub{pkg, "<PKG>"})
	}
	for _, p := range subs {
		paths = append(paths, p)
		// pwd -P reports the resolved path (macOS /var → /private/var).
		if r, err := filepath.EvalSymlinks(p.path); err == nil && r != p.path {
			paths = append(paths, sub{r, p.label})
		}
	}
	// Longest first, so a path nested in another temp dir is replaced whole.
	sort.SliceStable(paths, func(i, j int) bool { return len(paths[i].path) > len(paths[j].path) })
	for _, p := range paths {
		s = strings.ReplaceAll(s, p.path, p.label)
	}
	// Number ids by first appearance, so a run that mixes up two ids still differs.
	seen := map[string]string{}
	s = goldenUUIDRe.ReplaceAllStringFunc(s, func(id string) string {
		if _, ok := seen[id]; !ok {
			seen[id] = fmt.Sprintf("<UUID%d>", len(seen)+1)
		}
		return seen[id]
	})
	return goldenSlimEnvRe.ReplaceAllString(s, "$1: <X>")
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "engine_golden", name+".golden")
	if *updateGolden {
		mustWrite(t, path, got, 0o644)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update): %v", err)
	}
	if !bytes.Equal(want, []byte(got)) {
		t.Errorf("%s drifted from its golden.\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}
