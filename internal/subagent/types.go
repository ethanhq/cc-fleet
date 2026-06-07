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
	"encoding/json"
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
	WorkingDir     string  // child's cwd (empty = inherit); used for git-worktree isolation
	MaxTurns       int     // --max-turns (claude graceful cap); 0 → omit
	MaxBudgetUSD   float64 // --max-budget-usd (claude graceful cap); 0 → omit
	LeadSessionID  string  // parent Claude session id for agent-status board grouping

	// Workflow run grouping (all optional). A workflow orchestrator tags each
	// subagent with the run it belongs to, the phase within that run, and a human
	// label, so the board can group N jobs into one run tree. Distinct from
	// LeadSessionID (a Claude session can host many ad-hoc subagents and runs).
	RunID string // run this job belongs to
	Phase string // phase label within the run
	Label string // human label for this agent within the run
	// JobID, when set, REUSES an existing job record instead of minting a fresh one — the
	// workflow engine mints a queued placeholder (PID=0) before a leaf gets a pool slot and
	// passes its id here so the same on-disk job flips queued→running→terminal as ONE file.
	// Empty (the bare-CLI path) mints a fresh id, byte-identical to before.
	JobID string
	// Attempt is the leaf's 1-based exec ordinal; the workflow engine sets 1 (a schema
	// mismatch is terminal, so a leaf never re-runs). The board shows "attempt N" only
	// when >1 — reachable now only via caches from engines that retried schema
	// mismatches. Zero (a non-workflow call) renders no marker.
	Attempt int

	// PersistIO opts this job into board drill-in: persist the prompt + answer to
	// per-job 0600 side files (<id>.prompt / <id>.answer) for the Workflows detail card.
	// It is CONTENT-PRIVACY, not key-safety — the vendor key never enters the prompt or
	// answer (it flows via apiKeyHelper). The terminal result CACHE stays answer-stripped
	// regardless (so the board TABLE never shows a reply); these side files are a separate,
	// opt-in surface the engine sets default-on with a --no-persist-io opt-out.
	PersistIO bool
	// IOPrompt is the prompt text to persist when PersistIO (the workflow engine feeds the
	// prompt via PromptReader/stdin, which is consumed by the child, so it passes the text
	// here separately for the side file). Empty → no prompt side file.
	IOPrompt string
	// StreamActivity opts the SYNC leaf into `--output-format stream-json --verbose` so its
	// per-leaf tool calls + running token usage stream to <jobID>.activity for the board's
	// Activity feed WHILE it runs. Workflow-engine-only (agent() sets it = PersistIO);
	// content-privacy, gated like PersistIO. Bare `cc-fleet subagent` leaves it false → the
	// `--output-format json` envelope, the byte-cap, and the Result stay byte-identical.
	StreamActivity bool
	// JournalKey is the leaf's content-hash key (workflow-engine-only). Persisted on the job
	// record so the board can target THIS leaf for restart — drop its journal entry and resume,
	// which re-runs only it (+ its dependents). It is a sha256 hex, never a secret.
	JournalKey string

	// PromptProfile selects the prompt shape: "" / "full" (today's full claude -p
	// session, byte-identical argv) | "slim" (native generic-subagent mirror) |
	// "slim-ro" (native Explore/Plan read-only mirror). The zero value is full.
	PromptProfile string
	// Tools replaces a slim profile's default tool set (validated + canonicalized).
	// Empty → the profile's default set. Ignored for full.
	Tools []string
	// NoSkills drops the Skill tool + host skill listing from a slim profile. The
	// zero value (false) is the documented default: skills ON (native parity).
	NoSkills bool
	// MCP, when true, inherits the host MCP config (native parity) for a slim
	// profile; false means --strict-mcp-config. The user-facing boundaries (CLI /
	// workflow agent()) resolve the per-profile default — slim inherits, slim-ro
	// strict — and pass the FINAL value here; the zero value stays strict for
	// direct constructors.
	MCP bool
	// JSONSchema, when non-empty, is the leaf's JSON Schema (canonical JSON text).
	// buildArgv passes it via --json-schema, making claude inject a forced
	// StructuredOutput tool — profile-independent (the injected tool survives a
	// slim --tools whitelist). The flag needs claude >= 2.1.88 (the slim floor);
	// an older claude fails the leaf with the ordinary classified usage error.
	// Workflow-engine-only: the bare CLI exposes no schema flag.
	JSONSchema string
}

// Usage mirrors the token-usage subset of claude's inner envelope we surface.
type Usage struct {
	InputTokens              int `json:"input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"` // tokens billed to WRITE the prompt cache
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

	// Workflow run grouping (optional; mirrors the Request fields). Carried on the
	// job files so the board can group jobs into a run → phase → agent tree.
	RunID string `json:"run_id,omitempty"`
	Phase string `json:"phase,omitempty"`
	Label string `json:"label,omitempty"`
	// JournalKey is the leaf's content-hash key, persisted so the board can restart THIS leaf
	// (invalidate its journal entry + resume). A sha256 hex — never a secret.
	JournalKey string `json:"journal_key,omitempty"`
	// Attempt is the 1-based exec ordinal the leaf ran at (>1 occurs only in caches from
	// engines that retried schema mismatches); 0 backfills a cache that predates the field.
	Attempt int `json:"attempt,omitempty"`

	// PromptProfile is the EFFECTIVE profile this run used (post-version-gate);
	// SlimDowngrade is non-empty when a slim request ran full instead (the reason).
	PromptProfile string `json:"prompt_profile,omitempty"`
	SlimDowngrade string `json:"slim_downgrade,omitempty"`

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
	// StructuredOutput is the inner envelope's structured_output payload (present
	// when the run carried --json-schema). In-process only, like Raw: the workflow
	// engine consumes it in agent() and persists it only via the run journal — it
	// never enters the CLI envelope, the sanitized result cache, or jobMeta.
	StructuredOutput json.RawMessage `json:"-"`
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
	ErrCodeStopped        = "SUBAGENT_STOPPED"          // a still-running leaf finalized by `workflow stop` (a stop, not a failure)
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
