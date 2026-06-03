// Package subagent runs a ONE-SHOT, HEADLESS vendor subagent: it launches
// `claude -p` backed by a third-party vendor model (via the vendor profile's
// --settings + --model) and returns the result synchronously. It is the lean
// half of the spawn pipeline (load vendor → write profile → load fingerprint)
// plus an exec shell, with the entire tmux / team / lock half removed.
//
// Three invariants hold here:
//
//   - Keys never leak: the vendor key flows ONLY via the profile's apiKeyHelper
//     (`<abs cc-fleet> keyget <vendor>`), which claude execs; cc-fleet's subagent
//     process never reads key bytes, never puts a key in argv/env/log/stdout.
//   - Lock-free: subagent writes no team config / members / inbox and splits no
//     tmux pane, so it takes NEITHER WithTeamLock NOR WithServerLock. The only
//     write is profile.WriteForVendor (already atomic + idempotent), so N
//     concurrent subagents for one vendor are embarrassingly parallel.
//   - The headless child's env strips the lead's creds AND the nested-CC /
//     teams markers (see childenv.Clean); fp.Env is deliberately NOT re-applied.
package subagent

import (
	"io"
	"time"
)

// Request is the input to Run. Zero values fall back to the documented
// defaults (OutputFormat "text", Timeout 300s, Probe off).
type Request struct {
	Vendor       string        // required: vendors.toml table name
	Model        string        // empty → vendor.default_model
	Prompt       string        // task text (mutually exclusive with PromptReader)
	PromptReader io.Reader     // --prompt-file / stdin; non-nil feeds claude's stdin, keeps -p value out of argv
	OutputFormat string        // "text" | "json" — claude's inner output format
	JSON         bool          // cc-fleet's own machine-readable Result envelope (forces inner json)
	Timeout      time.Duration // hard wall-clock deadline; 0 → 300s
	Probe        bool          // pre-run 3s reachability check; default off (opposite of spawn)

	PermissionMode string  // empty → --dangerously-skip-permissions; else --permission-mode <v>
	Resume         string  // --resume <session_id> (multi-turn)
	Background     bool    // --background: launch detached, return a job handle
	MaxTurns       int     // --max-turns (claude graceful cap); 0 → omit
	MaxBudgetUSD   float64 // --max-budget-usd (claude graceful cap); 0 → omit
	LeadSessionID  string  // parent Claude session id for agent-status board grouping
}

// Usage mirrors the token-usage subset of claude's inner envelope we surface.
type Usage struct {
	InputTokens          int `json:"input_tokens,omitempty"`
	OutputTokens         int `json:"output_tokens,omitempty"`
	CacheReadInputTokens int `json:"cache_read_input_tokens,omitempty"`
}

// Result is cc-fleet's stable outer envelope (mirrors spawn.Result style). It
// is decoupled from claude's inner schema: Run parses the inner envelope, then
// distills these fields, so a claude version bump doesn't break the skill
// contract. The JSON tags are part of that contract — keep them stable.
type Result struct {
	OK bool `json:"ok"`

	// Success path (distilled from claude's inner result envelope).
	Result        string  `json:"result,omitempty"` // inner .result (the answer text)
	Vendor        string  `json:"vendor,omitempty"`
	Model         string  `json:"model,omitempty"`           // inner modelUsage key (route evidence) else req.Model
	DurationMs    int64   `json:"duration_ms,omitempty"`     // inner .duration_ms (incl. retry wall-clock)
	APIDurationMs int64   `json:"duration_api_ms,omitempty"` // inner .duration_api_ms (pure API time)
	NumTurns      int     `json:"num_turns,omitempty"`
	StopReason    string  `json:"stop_reason,omitempty"`
	Usage         *Usage  `json:"usage,omitempty"`
	CostUSD       float64 `json:"total_cost_usd,omitempty"`
	SessionID     string  `json:"session_id,omitempty"` // for --resume
	LeadSessionID string  `json:"lead_session_id,omitempty"`
	PermDenials   int     `json:"permission_denials,omitempty"`

	// Async / background job fields. Present on --background launch and
	// subagent-status / subagent-gc results.
	JobID      string `json:"job_id,omitempty"`
	Status     string `json:"status,omitempty"` // running | done | failed
	OutputFile string `json:"output_file,omitempty"`
	PID        int    `json:"pid,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	Removed    int    `json:"removed,omitempty"` // subagent-gc: job groups removed

	// Failure path. error_msg is a CANONICAL string only (never raw vendor body
	// text); the one raw-text exception is the SUBAGENT_FAILED stderr preview,
	// which is claude's own (key-safe) stderr.
	ErrorCode      string `json:"error_code,omitempty"`
	ErrorMsg       string `json:"error_msg,omitempty"`
	Suggestion     string `json:"suggestion,omitempty"`
	APIErrorStatus int    `json:"api_error_status,omitempty"` // inner .api_error_status (e.g. 429/401)

	// Raw passthrough, never serialized in cc-fleet's envelope: used by the
	// human/debug --output-format json path (Raw = claude's inner JSON) and the
	// bare text path (Raw = claude's text answer), each exiting ExitCode.
	Raw      []byte `json:"-"`
	ExitCode int    `json:"-"`
}

// Error code enumeration. Skills switch on these strings without parsing prose.
// The runtime codes deliberately echo teardown's error_class vocab
// (auth↔KEY_INVALID, rate_limit↔RATE_LIMITED, …) for a consistent mental model
// across teammate (`ps --check`) and subagent.
const (
	// Pre-flight failures (claude never launched). Reuse spawn's code spellings
	// so the skill already recognizes them.
	ErrCodeBadArgs            = "SUBAGENT_BAD_ARGS"   // --prompt/--prompt-file missing or both given (CLI layer)
	ErrCodeUnknownVendor      = "UNKNOWN_VENDOR"      // vendor not in vendors.toml
	ErrCodeVendorDisabled     = "VENDOR_DISABLED"     // enabled = false
	ErrCodeFingerprintMissing = "FINGERPRINT_MISSING" // never captured → skill self-heal
	ErrCodeFingerprintStale   = "FINGERPRINT_STALE"   // BinaryPath gone from disk

	// Probe failure (only when --probe).
	ErrCodeVendorUnreachable = "VENDOR_UNREACHABLE" // transport-layer failure

	// Runtime failures parsed from claude's inner envelope.
	ErrCodeKeyInvalid          = "KEY_INVALID"          // api_error_status 401/403
	ErrCodeRateLimited         = "RATE_LIMITED"         // 429 (no balance signature)
	ErrCodeInsufficientBalance = "INSUFFICIENT_BALANCE" // 429/402 + balance signature
	ErrCodeModelNotFound       = "MODEL_NOT_FOUND"      // 400 + model-name rejection
	ErrCodeVendorAPIError      = "VENDOR_API_ERROR"     // other is_error / 5xx / overloaded

	// cc-fleet layer.
	ErrCodeTimeout        = "SUBAGENT_TIMEOUT"          // --timeout deadline fired before claude returned
	ErrCodeFailed         = "SUBAGENT_FAILED"           // non-zero exit with no parseable envelope / internal error
	ErrCodeOutputTooLarge = "SUBAGENT_OUTPUT_TOO_LARGE" // child stdout/stderr exceeded the byte cap; group killed
)

// fail builds a failure Result, stamping vendor for context (mirrors spawn.fail).
func fail(code, msg, vendor, suggestion string) Result {
	return Result{
		OK:         false,
		ErrorCode:  code,
		ErrorMsg:   msg,
		Vendor:     vendor,
		Suggestion: suggestion,
	}
}
