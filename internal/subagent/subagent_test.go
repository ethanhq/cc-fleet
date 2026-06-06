package subagent

import (
	"strings"
	"testing"
	"time"
)

// ----- buildArgv -----

func TestBuildArgv(t *testing.T) {
	const bin = "/v/claude"
	const prof = "/p/glm.json"
	const model = "glm-4.6"

	t.Run("text prompt mode", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "do it"}, slimArgv{})
		assertSeq(t, argv, bin, "--dangerously-skip-permissions", "--settings", prof, "--model", model, "-p", "do it")
		assertAbsent(t, argv, "--output-format")
		assertAbsent(t, argv, "--permission-mode")
	})

	t.Run("json forces output-format", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "x", JSON: true}, slimArgv{})
		assertPairAfter(t, argv, "--output-format", "json")
	})

	t.Run("output-format json without --json", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "x", OutputFormat: "json"}, slimArgv{})
		assertPairAfter(t, argv, "--output-format", "json")
	})

	t.Run("permission-mode overrides skip-permissions", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "x", PermissionMode: "plan"}, slimArgv{})
		assertPairAfter(t, argv, "--permission-mode", "plan")
		assertAbsent(t, argv, "--dangerously-skip-permissions")
	})

	t.Run("resume before settings", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "x", Resume: "sess-123"}, slimArgv{})
		assertPairAfter(t, argv, "--resume", "sess-123")
		if idxOf(argv, "--resume") > idxOf(argv, "--settings") {
			t.Fatalf("--resume should precede --settings: %v", argv)
		}
	})

	t.Run("max-turns and max-budget", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "x", MaxTurns: 8, MaxBudgetUSD: 0.5}, slimArgv{})
		assertPairAfter(t, argv, "--max-turns", "8")
		assertPairAfter(t, argv, "--max-budget-usd", "0.5")
	})

	t.Run("prompt-file keeps -p value out of argv", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "SECRET", PromptReader: strings.NewReader("from stdin")}, slimArgv{})
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

func TestRun_UnknownVendor(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	res := Run(Request{Vendor: "nope", Prompt: "hi", JSON: true})
	if res.OK || res.ErrorCode != ErrCodeUnknownVendor {
		t.Fatalf("want UNKNOWN_VENDOR, got OK=%v code=%s", res.OK, res.ErrorCode)
	}
}
