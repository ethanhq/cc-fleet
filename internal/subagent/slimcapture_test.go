//go:build !windows

package subagent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

// First-request capture test (acceptance a/b). It execs the REAL claude binary
// with the EXACT argv buildArgv produces and asserts the on-the-wire request
// shape per profile — STRUCTURAL assertions only (markers, exact tool NAMES +
// count, thinking type, CLAUDE.md presence), never byte sizes. The tool-name set
// is the canary: a claude-side tool rename turns a --tools name into a silent
// no-op and changes the request's tool array, which this catches.
//
// The harness mirrors the validated capture proxy: a 127.0.0.1 httptest server
// records each request body and answers 401, the child runs against it with a
// dummy key (no real API traffic), and its whole process group is killed the
// instant the first /v1/messages body lands — claude self-retries a 401 for
// ~180s otherwise. Skipped when no claude binary is present (CI without claude)
// and under -short.

// resolveClaudeForCapture returns the real claude path the way production does
// (the fingerprint resolver), falling back to PATH; "" when none is found.
func resolveClaudeForCapture() string {
	if fp, err := fingerprint.LoadOrBundled(); err == nil {
		if p, err := fingerprint.ResolveBinaryPath(fp); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	return ""
}

// captureFirstRequest execs claude with argv (cwd=dir, baseURL→capture server,
// dummy key, ANTHROPIC_AUTH_TOKEN unset, plus extraEnv) and returns the body of
// the first /v1/messages request, killing the whole process group the moment it
// arrives. Bounded at 60s.
func captureFirstRequest(t *testing.T, bin string, argv, extraEnv []string, dir string) []byte {
	t.Helper()

	var (
		mu   sync.Mutex
		body []byte
	)
	got := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/v1/messages" {
			mu.Lock()
			body = b
			mu.Unlock()
			once.Do(func() { close(got) })
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"probe"}}`))
	}))
	defer srv.Close()

	cmd := exec.Command(bin)
	cmd.Args = argv // argv[0] == bin by construction
	cmd.Dir = dir
	// ANTHROPIC_AUTH_TOKEN is left unset (not inherited): start from a scrubbed
	// env and add only the capture overrides.
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"ANTHROPIC_BASE_URL=" + srv.URL,
		"ANTHROPIC_API_KEY=sk-probe-dummy",
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	cmd.Env = append(env, extraEnv...)
	setGroupAttr(cmd) // own process group so -pid reaps the whole tree

	if err := cmd.Start(); err != nil {
		t.Fatalf("start claude: %v", err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	select {
	case <-got:
	case <-time.After(60 * time.Second):
		t.Fatal("no /v1/messages request captured within 60s")
	}
	mu.Lock()
	defer mu.Unlock()
	return body
}

// capturedRequest is the minimal slice of a /v1/messages body the assertions
// need; parsed with encoding/json (no regex over JSON).
type capturedRequest struct {
	System   json.RawMessage `json:"system"` // string or array of text blocks
	Messages json.RawMessage `json:"messages"`
	Tools    []struct {
		Name string `json:"name"`
	} `json:"tools"`
	Thinking struct {
		Type string `json:"type"`
	} `json:"thinking"`
}

func parseRequest(t *testing.T, body []byte) capturedRequest {
	t.Helper()
	if len(body) == 0 {
		t.Fatal("captured an empty request body")
	}
	var req capturedRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("parse request body: %v", err)
	}
	return req
}

func toolNames(req capturedRequest) []string {
	names := make([]string, 0, len(req.Tools))
	for _, x := range req.Tools {
		names = append(names, x.Name)
	}
	sort.Strings(names)
	return names
}

// expectedSlimWire is the on-the-wire tool set for a slim profile: the
// canonicalized declared default set plus LSP, which claude auto-adds to any
// --tools whitelist. Anchored to DefaultSlimTools so a tool rename (which would
// drop the renamed name from the request) is caught.
func expectedSlimWire(t *testing.T, profile string) []string {
	t.Helper()
	tools, err := CanonicalizeTools(DefaultSlimTools(profile, false))
	if err != nil {
		t.Fatalf("canonicalize default tools for %q: %v", profile, err)
	}
	tools = append(tools, "LSP")
	sort.Strings(tools)
	return tools
}

// newCaptureRepo makes a t.TempDir() git repo with a sentinel CLAUDE.md line so
// the assertions can prove CLAUDE.md presence/absence and the git env/status.
func newCaptureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init")
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(claudeMdSentinel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "init")
	return dir
}

const claudeMdSentinel = "SLIMCAPTURE_SENTINEL_CLAUDE_MD"

// slimCaptureArgv builds the real argv for a profile via buildSlimArgv +
// buildArgv. settingsPath is an inert `{}` settings file so the exact buildArgv
// shape is preserved without a real vendor profile.
func slimCaptureArgv(t *testing.T, bin, settingsPath, model, dir, profile string) ([]string, string) {
	t.Helper()
	const fakeModel = "claude-sonnet-4-6" // a real CC model id so the probe reaches /v1/messages
	req := Request{Prompt: "say hi", PromptProfile: profile, WorkingDir: dir, JSON: true}
	if profile == ProfileFull {
		req.PromptProfile = ""
	}
	sa, err := buildSlimArgv(profileForBuild(profile), "slimcap-"+profile, req, fakeModel)
	if err != nil {
		t.Fatalf("buildSlimArgv(%q): %v", profile, err)
	}
	argv := buildArgv(bin, settingsPath, fakeModel, req, sa)
	return argv, fakeModel
}

// profileForBuild maps the test's profile label to the effective profile
// buildSlimArgv expects (full → "", slim profiles unchanged).
func profileForBuild(profile string) string {
	if profile == ProfileFull {
		return ""
	}
	return profile
}

func TestSlimCaptureFirstRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("capture test execs the real claude binary; skipped under -short")
	}
	bin := resolveClaudeForCapture()
	if bin == "" {
		t.Skip("no claude binary found; skipping the capture test")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // sidecar lands under a temp jobs dir
	jd, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(jd, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("slim", func(t *testing.T) {
		dir := newCaptureRepo(t)
		argv, _ := slimCaptureArgv(t, bin, settings, "", dir, ProfileSlim)
		req := parseRequest(t, captureFirstRequest(t, bin, argv, nil, dir))
		sys := string(req.System)

		if !strings.Contains(sys, agentPromptMarker) {
			t.Errorf("slim system prompt missing the slim template marker %q", agentPromptMarker)
		}
		if strings.Contains(sys, "Tone and style") {
			t.Error("slim system prompt leaked the full main-prompt marker \"Tone and style\"")
		}
		if got, want := toolNames(req), expectedSlimWire(t, ProfileSlim); !reflect.DeepEqual(got, want) {
			t.Errorf("slim tool set canary: got %v, want %v", got, want)
		}
		if req.Thinking.Type != "disabled" {
			t.Errorf("slim thinking.type = %q, want \"disabled\"", req.Thinking.Type)
		}
		if !strings.Contains(string(req.Messages), claudeMdSentinel) {
			t.Error("slim first user message missing the CLAUDE.md sentinel (CLAUDE.md must be present)")
		}
		if !strings.Contains(sys, "gitStatus:") {
			t.Error("slim system prompt missing the gitStatus marker")
		}
	})

	t.Run("slim-ro", func(t *testing.T) {
		dir := newCaptureRepo(t)
		argv, _ := slimCaptureArgv(t, bin, settings, "", dir, ProfileSlimRO)
		req := parseRequest(t, captureFirstRequest(t, bin, argv,
			[]string{"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1"}, dir))
		sys := string(req.System)

		if strings.Contains(string(req.Messages), claudeMdSentinel) {
			t.Error("slim-ro must drop CLAUDE.md, but the sentinel reached the request")
		}
		if !strings.Contains(sys, "read-only research agent") {
			t.Error("slim-ro system prompt missing the read-only paragraph marker")
		}
		if got, want := toolNames(req), expectedSlimWire(t, ProfileSlimRO); !reflect.DeepEqual(got, want) {
			t.Errorf("slim-ro tool set canary: got %v, want %v", got, want)
		}
		if req.Thinking.Type != "disabled" {
			t.Errorf("slim-ro thinking.type = %q, want \"disabled\"", req.Thinking.Type)
		}
	})

	t.Run("full", func(t *testing.T) {
		dir := newCaptureRepo(t)
		argv, _ := slimCaptureArgv(t, bin, settings, "", dir, ProfileFull)
		req := parseRequest(t, captureFirstRequest(t, bin, argv, nil, dir))
		sys := string(req.System)

		if !strings.Contains(sys, "Tone and style") {
			t.Error("full system prompt missing the main-prompt marker \"Tone and style\" (full must be untouched)")
		}
		if n := len(req.Tools); n <= 20 {
			t.Errorf("full tool count = %d, want > 20 (the full pool, untouched)", n)
		}
	})
}
