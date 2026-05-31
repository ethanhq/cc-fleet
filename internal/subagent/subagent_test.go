package subagent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ----- cleanEnv (load-bearing: the env strip removes creds + nested-CC/teams markers) -----

func TestCleanEnv_StripsTheLoadBearingVars(t *testing.T) {
	in := []string{
		"ANTHROPIC_API_KEY=sk-leak",
		"ANTHROPIC_AUTH_TOKEN=tok-leak",
		"CLAUDECODE=1",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1",
		"PATH=/usr/bin",
		"HOME=/root",
		"NO_EQUALS_LINE",
	}
	out := cleanEnv(in)

	// The four credential/nested-CC vars + the teams trigger must all be gone.
	for _, banned := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
		"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS",
	} {
		for _, kv := range out {
			if strings.HasPrefix(kv, banned+"=") {
				t.Fatalf("cleanEnv leaked %q: %q", banned, kv)
			}
		}
	}
	// Defense in depth: no CLAUDECODE / teams marker may appear anywhere.
	joined := strings.Join(out, "\n")
	for _, marker := range []string{"CLAUDECODE", "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "sk-leak", "tok-leak"} {
		if strings.Contains(joined, marker) {
			t.Fatalf("cleanEnv output still contains %q: %q", marker, joined)
		}
	}
	// Unrelated vars (and the malformed no-'=' line) survive untouched.
	if !containsLine(out, "PATH=/usr/bin") || !containsLine(out, "HOME=/root") {
		t.Fatalf("cleanEnv dropped a keeper var: %v", out)
	}
	if !containsLine(out, "NO_EQUALS_LINE") {
		t.Fatalf("cleanEnv dropped the malformed line: %v", out)
	}
}

func containsLine(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

// ----- buildArgv -----

func TestBuildArgv(t *testing.T) {
	const bin = "/v/claude"
	const prof = "/p/glm.json"
	const model = "glm-4.6"

	t.Run("text prompt mode", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "do it"})
		assertSeq(t, argv, bin, "--dangerously-skip-permissions", "--settings", prof, "--model", model, "-p", "do it")
		assertAbsent(t, argv, "--output-format")
		assertAbsent(t, argv, "--permission-mode")
	})

	t.Run("json forces output-format", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "x", JSON: true})
		assertPairAfter(t, argv, "--output-format", "json")
	})

	t.Run("output-format json without --json", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "x", OutputFormat: "json"})
		assertPairAfter(t, argv, "--output-format", "json")
	})

	t.Run("permission-mode overrides skip-permissions", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "x", PermissionMode: "plan"})
		assertPairAfter(t, argv, "--permission-mode", "plan")
		assertAbsent(t, argv, "--dangerously-skip-permissions")
	})

	t.Run("resume before settings", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "x", Resume: "sess-123"})
		assertPairAfter(t, argv, "--resume", "sess-123")
		if idxOf(argv, "--resume") > idxOf(argv, "--settings") {
			t.Fatalf("--resume should precede --settings: %v", argv)
		}
	})

	t.Run("max-turns and max-budget", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "x", MaxTurns: 8, MaxBudgetUSD: 0.5})
		assertPairAfter(t, argv, "--max-turns", "8")
		assertPairAfter(t, argv, "--max-budget-usd", "0.5")
	})

	t.Run("prompt-file keeps -p value out of argv", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "SECRET", PromptReader: strings.NewReader("from stdin")})
		// -p must be present but its value (the prompt) must NOT be in argv.
		if idxOf(argv, "-p") < 0 {
			t.Fatalf("expected -p in argv: %v", argv)
		}
		for _, a := range argv {
			if a == "SECRET" || a == "from stdin" {
				t.Fatalf("prompt text leaked into argv with PromptReader set: %v", argv)
			}
		}
		// The token right after -p must be a flag (or nothing), never a value.
		i := idxOf(argv, "-p")
		if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
			t.Fatalf("-p followed by a value %q, want a flag/end: %v", argv[i+1], argv)
		}
	})
}

// ----- classify -----

// Real inner envelopes captured from a smoke run.
const smokeSuccessJSON = `{"type":"result","subtype":"success","is_error":false,"api_error_status":null,
 "duration_ms":3654,"duration_api_ms":3385,"ttft_ms":3397,"num_turns":1,"stop_reason":"end_turn",
 "session_id":"84c5b474-aaaa","total_cost_usd":0.258409,
 "usage":{"input_tokens":50750,"cache_read_input_tokens":18,"output_tokens":186,"service_tier":"standard"},
 "modelUsage":{"mimo-v2-flash":{"inputTokens":50750,"outputTokens":186,"costUSD":0.258409}},
 "result":"SUBAGENT_SMOKE_OK=42","permission_denials":[],"terminal_reason":"completed"}`

const smoke429BalanceJSON = `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,
 "duration_ms":178257,"duration_api_ms":0,"num_turns":1,"stop_reason":"stop_sequence",
 "result":"API Error: Request rejected (429) · [1113][余额不足或无可用资源包,请充值。]",
 "total_cost_usd":0,"modelUsage":{},"permission_denials":[],"terminal_reason":"completed"}`

func TestClassify(t *testing.T) {
	req := Request{Vendor: "v", JSON: true}

	t.Run("success", func(t *testing.T) {
		res := classify(req, "fallback-model", []byte(smokeSuccessJSON), nil, 0, false, true)
		if !res.OK {
			t.Fatalf("want OK, got %s/%s", res.ErrorCode, res.ErrorMsg)
		}
		if res.Result != "SUBAGENT_SMOKE_OK=42" {
			t.Fatalf("result = %q", res.Result)
		}
		if res.Model != "mimo-v2-flash" { // routing evidence = modelUsage key
			t.Fatalf("model = %q, want mimo-v2-flash (from modelUsage key)", res.Model)
		}
		if res.CostUSD == 0 || res.Usage == nil || res.Usage.InputTokens != 50750 {
			t.Fatalf("usage/cost not distilled: %+v usage=%+v", res, res.Usage)
		}
		if res.SessionID != "84c5b474-aaaa" {
			t.Fatalf("session_id = %q", res.SessionID)
		}
	})

	t.Run("429 balance", func(t *testing.T) {
		res := classify(req, "glm-4.6", []byte(smoke429BalanceJSON), nil, 1, false, true)
		if res.OK || res.ErrorCode != ErrCodeInsufficientBalance {
			t.Fatalf("want INSUFFICIENT_BALANCE, got OK=%v code=%s", res.OK, res.ErrorCode)
		}
		if res.APIErrorStatus != 429 {
			t.Fatalf("api_error_status = %d, want 429", res.APIErrorStatus)
		}
		// error_msg must be canonical, never the raw Chinese vendor text.
		if strings.Contains(res.ErrorMsg, "余额") {
			t.Fatalf("error_msg leaked raw vendor text: %q", res.ErrorMsg)
		}
	})

	t.Run("429 no balance signature → rate limited", func(t *testing.T) {
		js := `{"type":"result","is_error":true,"api_error_status":429,"result":"Too Many Requests"}`
		res := classify(req, "m", []byte(js), nil, 1, false, true)
		if res.ErrorCode != ErrCodeRateLimited {
			t.Fatalf("want RATE_LIMITED, got %s", res.ErrorCode)
		}
	})

	t.Run("401 → key invalid", func(t *testing.T) {
		js := `{"type":"result","is_error":true,"api_error_status":401,"result":"unauthorized"}`
		res := classify(req, "m", []byte(js), nil, 1, false, true)
		if res.ErrorCode != ErrCodeKeyInvalid || res.APIErrorStatus != 401 {
			t.Fatalf("want KEY_INVALID/401, got %s/%d", res.ErrorCode, res.APIErrorStatus)
		}
	})

	t.Run("400 model rejection → model not found", func(t *testing.T) {
		js := `{"type":"result","is_error":true,"api_error_status":400,"result":"model not found; supported names: a, b"}`
		res := classify(req, "m", []byte(js), nil, 1, false, true)
		if res.ErrorCode != ErrCodeModelNotFound {
			t.Fatalf("want MODEL_NOT_FOUND, got %s", res.ErrorCode)
		}
	})

	t.Run("400 generic → vendor api error", func(t *testing.T) {
		js := `{"type":"result","is_error":true,"api_error_status":400,"result":"bad request: malformed body"}`
		res := classify(req, "m", []byte(js), nil, 1, false, true)
		if res.ErrorCode != ErrCodeVendorAPIError {
			t.Fatalf("want VENDOR_API_ERROR, got %s", res.ErrorCode)
		}
	})

	t.Run("error_max_turns subtype → subagent failed (review fix #3)", func(t *testing.T) {
		js := `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":8,"errors":[{"message":"max turns"}]}`
		res := classify(req, "m", []byte(js), nil, 1, false, true)
		if res.ErrorCode != ErrCodeFailed {
			t.Fatalf("want SUBAGENT_FAILED, got %s", res.ErrorCode)
		}
		if !strings.Contains(res.ErrorMsg, "error_max_turns") {
			t.Fatalf("error_msg should name the subtype: %q", res.ErrorMsg)
		}
		// max_turns gets an actionable sibling hint (raise the cap / use
		// --background) via the suggestion.
		if !strings.Contains(res.Suggestion, "--max-turns") || !strings.Contains(res.Suggestion, "--background") {
			t.Fatalf("max_turns suggestion should mention raising --max-turns + --background: %q", res.Suggestion)
		}
	})

	t.Run("error_max_budget_usd → friendly budget guidance + spent cost", func(t *testing.T) {
		// api_error_status null (0) + subtype error_max_budget_usd + a reported
		// total_cost_usd — the realistic "spent the budget, no product" envelope.
		js := `{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":0.2584,"api_error_status":null}`
		res := classify(req, "glm-4.6", []byte(js), nil, 1, false, true)
		if res.OK || res.ErrorCode != ErrCodeFailed {
			t.Fatalf("want SUBAGENT_FAILED, got OK=%v code=%s", res.OK, res.ErrorCode)
		}
		// Message names the cap + the amount already spent (no silent-waste optics).
		if !strings.Contains(res.ErrorMsg, "--max-budget-usd") || !strings.Contains(res.ErrorMsg, "0.2584") {
			t.Fatalf("budget error_msg should name the cap + spent $: %q", res.ErrorMsg)
		}
		// Suggestion guides raising the cap (with spent $) or a cheaper model.
		if !strings.Contains(res.Suggestion, "Raise --max-budget-usd") ||
			!strings.Contains(res.Suggestion, "0.2584") ||
			!strings.Contains(res.Suggestion, "cheaper model") {
			t.Fatalf("budget suggestion should guide raise-budget/cheaper-model with spent $: %q", res.Suggestion)
		}
		// The spent cost is surfaced structurally too.
		if res.CostUSD != 0.2584 {
			t.Fatalf("CostUSD = %v, want 0.2584 carried from inner total_cost_usd", res.CostUSD)
		}
	})

	t.Run("error_max_budget_usd with no reported cost stays clean", func(t *testing.T) {
		js := `{"type":"result","subtype":"error_max_budget_usd","is_error":true,"api_error_status":null}`
		res := classify(req, "m", []byte(js), nil, 1, false, true)
		if res.ErrorCode != ErrCodeFailed {
			t.Fatalf("want SUBAGENT_FAILED, got %s", res.ErrorCode)
		}
		// No "$" spent claim when claude reported none; still guides raising the cap.
		if strings.Contains(res.ErrorMsg, "spending $") {
			t.Fatalf("no cost reported → message must not claim a spend: %q", res.ErrorMsg)
		}
		if !strings.Contains(res.Suggestion, "Raise --max-budget-usd") {
			t.Fatalf("suggestion should still guide raising the budget: %q", res.Suggestion)
		}
	})

	t.Run("5xx → vendor api error", func(t *testing.T) {
		js := `{"type":"result","is_error":true,"api_error_status":503,"result":"service unavailable"}`
		res := classify(req, "m", []byte(js), nil, 1, false, true)
		if res.ErrorCode != ErrCodeVendorAPIError || res.APIErrorStatus != 503 {
			t.Fatalf("want VENDOR_API_ERROR/503, got %s/%d", res.ErrorCode, res.APIErrorStatus)
		}
	})

	t.Run("empty stdout → subagent failed", func(t *testing.T) {
		res := classify(req, "m", []byte(""), []byte("boom on stderr"), 1, false, true)
		if res.ErrorCode != ErrCodeFailed {
			t.Fatalf("want SUBAGENT_FAILED, got %s", res.ErrorCode)
		}
		if !strings.Contains(res.ErrorMsg, "boom on stderr") {
			t.Fatalf("expected stderr preview in msg: %q", res.ErrorMsg)
		}
	})

	t.Run("garbage stdout → subagent failed", func(t *testing.T) {
		res := classify(req, "m", []byte("not json at all"), nil, 1, false, true)
		if res.ErrorCode != ErrCodeFailed {
			t.Fatalf("want SUBAGENT_FAILED, got %s", res.ErrorCode)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		res := classify(Request{Vendor: "v", Timeout: 2 * time.Second, JSON: true}, "m", nil, nil, -1, true, true)
		if res.ErrorCode != ErrCodeTimeout {
			t.Fatalf("want SUBAGENT_TIMEOUT, got %s", res.ErrorCode)
		}
	})

	t.Run("text mode success", func(t *testing.T) {
		res := classify(Request{Vendor: "v"}, "m", []byte("plain answer"), nil, 0, false, false)
		if !res.OK || res.Result != "plain answer" {
			t.Fatalf("text mode: OK=%v result=%q", res.OK, res.Result)
		}
	})
}

// ----- runClaude with fake binaries -----

// writeFakeBin writes an executable shell script and returns its path.
func writeFakeBin(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "claude")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}
	return p
}

func TestRunClaude_SuccessEnvelope(t *testing.T) {
	bin := writeFakeBin(t, "#!/bin/sh\nprintf '%s' '"+smokeSuccessJSON+"'\nexit 0\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stdout, stderr, code, err := runClaude(ctx, bin, []string{bin, "-p", "x"}, os.Environ(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	// A clean success must report no run error and an empty stderr channel.
	if err != nil {
		t.Fatalf("clean success returned err=%v, want nil", err)
	}
	if len(stderr) != 0 {
		t.Fatalf("clean success wrote to stderr: %q", stderr)
	}
	res := classify(Request{Vendor: "mimo", JSON: true}, "fallback", stdout, nil, code, false, true)
	if !res.OK || res.Result != "SUBAGENT_SMOKE_OK=42" {
		t.Fatalf("classify of fake success: OK=%v result=%q", res.OK, res.Result)
	}
}

func TestRunClaude_ErrorEnvelopeExit1(t *testing.T) {
	bin := writeFakeBin(t, "#!/bin/sh\nprintf '%s' '"+smoke429BalanceJSON+"'\nexit 1\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stdout, _, code, err := runClaude(ctx, bin, []string{bin, "-p", "x"}, os.Environ(), nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	// A non-zero exit must surface the run error (an *exec.ExitError), not be
	// swallowed — exitCode is derived from it.
	if err == nil {
		t.Fatal("exit-1 returned nil err, want a non-nil run error")
	}
	res := classify(Request{Vendor: "glm", JSON: true}, "glm-4.6", stdout, nil, code, false, true)
	if res.OK || res.ErrorCode != ErrCodeInsufficientBalance {
		t.Fatalf("want INSUFFICIENT_BALANCE from exit-1 envelope, got OK=%v code=%s", res.OK, res.ErrorCode)
	}
}

func TestRunClaude_StdinPrompt(t *testing.T) {
	// Echo back stdin so we prove --prompt-file/stdin actually reaches the child.
	bin := writeFakeBin(t, "#!/bin/sh\ncat\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stdout, _, code, _ := runClaude(ctx, bin, []string{bin, "-p"}, os.Environ(), strings.NewReader("piped-prompt"))
	if code != 0 || string(stdout) != "piped-prompt" {
		t.Fatalf("stdin not piped: code=%d stdout=%q", code, stdout)
	}
}

// TestRunClaude_TimeoutKillsProcessGroup proves a grandchild that IGNORES
// SIGTERM is still reaped (via the escalated SIGKILL to the whole process
// group) when the deadline fires — no orphan survives.
func TestRunClaude_TimeoutKillsProcessGroup(t *testing.T) {
	// Shrink the SIGTERM→SIGKILL grace so the test is fast.
	orig := waitGrace
	waitGrace = 500 * time.Millisecond
	t.Cleanup(func() { waitGrace = orig })

	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	// Leader and grandchild both trap (ignore) SIGTERM; only a SIGKILL to the
	// group can reap them. The grandchild records its pid so we can assert death.
	script := "#!/bin/sh\n" +
		"trap '' TERM\n" +
		"sh -c 'trap \"\" TERM; echo $$ > \"" + pidFile + "\"; sleep 30' &\n" +
		"sleep 30\n"
	bin := writeFakeBin(t, script)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, _, _, _ = runClaude(ctx, bin, []string{bin}, os.Environ(), nil)
	elapsed := time.Since(start)

	// Should return within deadline + grace + slack, not hang on the pipe-holding
	// grandchild (WaitDelay defeats that).
	if elapsed > 4*time.Second {
		t.Fatalf("runClaude took %v with a pipe-holding grandchild; WaitDelay/kill model broken", elapsed)
	}

	gpid := readPID(t, pidFile)
	if gpid <= 0 {
		t.Fatalf("grandchild never recorded its pid (%q)", pidFile)
	}
	// The grandchild ignored SIGTERM; only the group SIGKILL escalation reaps it.
	if alive := waitGone(gpid, 3*time.Second); alive {
		// Best-effort cleanup so a failure doesn't leak a sleeper.
		_ = syscall.Kill(gpid, syscall.SIGKILL)
		t.Fatalf("grandchild pid %d survived the timeout (orphan) — process-group SIGKILL escalation missing", gpid)
	}
}

// TestRun_NoUserFingerprint_UsesBundled: with NO
// ~/.config/cc-fleet/fingerprint.json, Run must NOT return FINGERPRINT_MISSING —
// LoadOrBundled supplies the embedded recipe and the binary path resolves live.
// A fast-exit fake claude on PATH keeps the run from reaching the (unreachable)
// vendor or launching a real claude.
func TestRun_NoUserFingerprint_UsesBundled(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	// Fake claude on PATH so ResolveBinaryPath finds a binary and runClaude
	// execs something that exits instantly (never touches the invalid base_url).
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "claude"),
		[]byte("#!/bin/sh\ncase \"$1\" in --version) echo \"2.1.150\";; esac\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Minimal vendors.toml with one enabled vendor.
	dir := filepath.Join(xdg, "cc-fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	toml := `version = 1

[glm]
base_url        = "https://example.invalid/anthropic"
default_model   = "glm-4.6"
models_endpoint = "https://example.invalid/v1/models"
secret_backend  = "file"
secret_ref      = "glm.key"
enabled         = true
added_at        = 2026-05-24T05:00:00Z
`
	if err := os.WriteFile(filepath.Join(dir, "vendors.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}

	res := Run(Request{Vendor: "glm", Prompt: "hi", JSON: true})
	// The bundled fallback must engage: no FINGERPRINT_MISSING, and the binary
	// resolved (no FINGERPRINT_STALE either, since the fake claude is on PATH).
	if res.ErrorCode == ErrCodeFingerprintMissing || res.ErrorCode == ErrCodeFingerprintStale {
		t.Fatalf("missing user fingerprint must fall back to bundled recipe, got %s: %s", res.ErrorCode, res.ErrorMsg)
	}
}

func TestRun_UnknownVendor(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	res := Run(Request{Vendor: "nope", Prompt: "hi", JSON: true})
	if res.OK || res.ErrorCode != ErrCodeUnknownVendor {
		t.Fatalf("want UNKNOWN_VENDOR, got OK=%v code=%s", res.OK, res.ErrorCode)
	}
}

// ----- argv assertion helpers -----

func idxOf(argv []string, tok string) int {
	for i, a := range argv {
		if a == tok {
			return i
		}
	}
	return -1
}

// assertSeq checks that the given tokens appear as a contiguous run in argv.
func assertSeq(t *testing.T, argv []string, seq ...string) {
	t.Helper()
	joined := strings.Join(argv, "\x00")
	want := strings.Join(seq, "\x00")
	if !strings.Contains(joined, want) {
		t.Fatalf("argv %v does not contain contiguous %v", argv, seq)
	}
}

// assertPairAfter checks flag is present and immediately followed by val.
func assertPairAfter(t *testing.T, argv []string, flag, val string) {
	t.Helper()
	i := idxOf(argv, flag)
	if i < 0 || i+1 >= len(argv) || argv[i+1] != val {
		t.Fatalf("expected %s %s in argv: %v", flag, val, argv)
	}
}

func assertAbsent(t *testing.T, argv []string, tok string) {
	t.Helper()
	if idxOf(argv, tok) >= 0 {
		t.Fatalf("expected %q absent from argv: %v", tok, argv)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	// The grandchild writes asynchronously; poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return -1
}

// waitGone polls pid for up to d and reports whether it is STILL alive
// (false = it died, which is what the no-orphan assertion wants).
func waitGone(pid int, d time.Duration) (stillAlive bool) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) != syscall.ESRCH
}
