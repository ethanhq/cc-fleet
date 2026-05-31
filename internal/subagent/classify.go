package subagent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ethanhq/cc-fleet/internal/redact"
	"github.com/ethanhq/cc-fleet/internal/vendorclass"
)

// stderrPreviewMax bounds the claude-stderr snippet attached to a
// SUBAGENT_FAILED message. claude's own stderr is key-safe (the key flows over
// the apiKeyHelper stdout pipe, never to claude's stderr), but we still cap
// length so a runaway log can't bloat the envelope.
const stderrPreviewMax = 512

// innerEnvelope is the LENIENT view of claude's `--output-format json` result
// object. Every field is optional: a missing field decodes to its zero value,
// never an error. We only depend on type / is_error / api_error_status / result
// / subtype for classification — the rest are distilled pass-through.
type innerEnvelope struct {
	Type              string                     `json:"type"`
	Subtype           string                     `json:"subtype"`
	IsError           bool                       `json:"is_error"`
	APIErrorStatus    int                        `json:"api_error_status"` // null decodes to 0
	Result            string                     `json:"result"`
	DurationMs        int64                      `json:"duration_ms"`
	APIDurationMs     int64                      `json:"duration_api_ms"`
	NumTurns          int                        `json:"num_turns"`
	StopReason        string                     `json:"stop_reason"`
	SessionID         string                     `json:"session_id"`
	TotalCostUSD      float64                    `json:"total_cost_usd"`
	Usage             *innerUsage                `json:"usage"`
	ModelUsage        map[string]json.RawMessage `json:"modelUsage"`
	PermissionDenials []json.RawMessage          `json:"permission_denials"`
}

type innerUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
}

// classify turns a finished claude invocation into a Result. It never returns
// an error — every path yields a Result (mirrors spawn). timedOut is the only
// out-of-band signal (ctx.DeadlineExceeded); innerJSON says whether we ran
// claude with --output-format json (and therefore expect a parseable envelope).
//
// Classification keys on is_error + api_error_status, NEVER subtype: subtype is
// "success" even on a 429 is_error:true.
func classify(req Request, model string, stdout, stderr []byte, exitCode int, timedOut, innerJSON bool) Result {
	if timedOut {
		return fail(ErrCodeTimeout, canonicalTimeoutMsg(req.Timeout), req.Vendor, suggestionFor(ErrCodeTimeout))
	}

	if !innerJSON {
		// Text mode: stdout is a plain answer, not an envelope. reportSubagent
		// dumps Raw verbatim; we still set OK from the exit code so Run's
		// structured return is sensible.
		//
		// Text-mode dead-job fallback: a detached background job that was
		// Released has no real exit code (StatusFor passes 0). If stdout is empty
		// and stderr is non-empty, the run failed — don't bless it as success
		// just because the exit code is "0". Only reachable when innerJSON=false
		// (an explicit text-mode background opt-in).
		if exitCode == 0 && bytes.TrimSpace(stdout) == nil && bytes.TrimSpace(stderr) != nil {
			return failWithPreview(req, stderr, exitCode)
		}
		if exitCode == 0 {
			return Result{OK: true, Vendor: req.Vendor, Model: model, Result: string(stdout)}
		}
		return failWithPreview(req, stderr, exitCode)
	}

	inner, ok := parseInner(stdout)
	if !ok {
		// No parseable envelope (claude crashed / printed nothing). Key-safe:
		// the preview is claude's OWN stderr, not vendor response text.
		return failWithPreview(req, stderr, exitCode)
	}

	if !inner.IsError {
		res := Result{
			OK:             true,
			Vendor:         req.Vendor,
			Result:         inner.Result,
			DurationMs:     inner.DurationMs,
			APIDurationMs:  inner.APIDurationMs,
			NumTurns:       inner.NumTurns,
			StopReason:     inner.StopReason,
			CostUSD:        inner.TotalCostUSD,
			SessionID:      inner.SessionID,
			PermDenials:    len(inner.PermissionDenials),
			APIErrorStatus: inner.APIErrorStatus,
			Model:          modelKey(inner.ModelUsage, model),
		}
		if inner.Usage != nil {
			res.Usage = &Usage{
				InputTokens:          inner.Usage.InputTokens,
				OutputTokens:         inner.Usage.OutputTokens,
				CacheReadInputTokens: inner.Usage.CacheReadInputTokens,
			}
		}
		return res
	}

	// is_error == true. Classify and emit a CANONICAL message: never copy
	// inner.result vendor text into error_msg.
	code := classifyError(inner)
	r := fail(code, errorMessage(code, inner), req.Vendor, suggestionFor(code))
	r.APIErrorStatus = inner.APIErrorStatus // always set
	// Carry a few non-secret structured fields that aid the lead even on error.
	r.DurationMs = inner.DurationMs
	r.SessionID = inner.SessionID
	r.Model = modelKey(inner.ModelUsage, model)
	// Budget / turn exhaustion (subtype error_*) gets actionable guidance instead
	// of the generic "inspect; retry" — the cost is already spent, so the useful
	// next step is raising the cap or picking a cheaper model. We also surface
	// the spent cost so the lead sees it wasn't silently wasted.
	switch inner.Subtype {
	case "error_max_budget_usd":
		r.CostUSD = inner.TotalCostUSD
		r.Suggestion = budgetSuggestion(inner.TotalCostUSD)
	case "error_max_turns":
		r.Suggestion = "Hit the --max-turns cap; raise it (or run a long task with --background) and retry"
	}
	return r
}

// classifyError maps an is_error inner envelope to an error code.
func classifyError(inner innerEnvelope) string {
	switch inner.APIErrorStatus {
	case 401, 403:
		return ErrCodeKeyInvalid
	case 429, 402:
		// Balance outranks a bare 429 — a retry can't fix being out of credit.
		if vendorclass.MatchClass(inner.Result) == vendorclass.ClassInsufficientBalance {
			return ErrCodeInsufficientBalance
		}
		return ErrCodeRateLimited
	case 400:
		if looksLikeModelRejection(inner.Result) {
			return ErrCodeModelNotFound
		}
		return ErrCodeVendorAPIError
	case 0:
		// No HTTP status. The turn/budget/exec-exhaustion variants carry subtype
		// error_* (and errors[] instead of result) — surface them as
		// SUBAGENT_FAILED, not a vendor API error.
		if strings.HasPrefix(inner.Subtype, "error_") {
			return ErrCodeFailed
		}
		if looksLikeTransport(inner.Result) {
			return ErrCodeVendorUnreachable
		}
		return ErrCodeVendorAPIError
	default:
		// 5xx / overloaded / anything else the vendor reported.
		return ErrCodeVendorAPIError
	}
}

// errorMessage returns a canonical, key-safe message for code. For
// SUBAGENT_FAILED arising from an error_* subtype, it names the subtype so a
// max_turns/budget exhaustion is legible without echoing vendor prose.
func errorMessage(code string, inner innerEnvelope) string {
	switch code {
	case ErrCodeKeyInvalid:
		return "vendor rejected the API key (HTTP 401/403)"
	case ErrCodeRateLimited:
		return "vendor rate limit (HTTP 429)"
	case ErrCodeInsufficientBalance:
		return "vendor out of balance/quota (HTTP 429/402)"
	case ErrCodeModelNotFound:
		return "vendor rejected the model name (HTTP 400)"
	case ErrCodeVendorUnreachable:
		return "vendor unreachable (transport failure reported by claude)"
	case ErrCodeVendorAPIError:
		if inner.APIErrorStatus > 0 {
			return fmt.Sprintf("vendor API error (HTTP %d)", inner.APIErrorStatus)
		}
		return "vendor API error"
	case ErrCodeFailed:
		// Budget exhaustion gets a friendly message that names the cap + spent $;
		// other error_* subtypes (e.g. error_max_turns) keep the subtype-named
		// generic line and get their actionable hint via the Suggestion.
		if inner.Subtype == "error_max_budget_usd" {
			return budgetMessage(inner.TotalCostUSD)
		}
		if inner.Subtype != "" {
			return "claude stopped without a result (" + inner.Subtype + ")"
		}
		return "claude failed without a result envelope"
	default:
		return "subagent failed"
	}
}

// budgetMessage names a --max-budget-usd exhaustion and, when claude reported a
// cost, the amount already spent — so the failure reads as "spent $X, capped"
// rather than an opaque stop. The cost is claude's own metering, not vendor body
// text, so it is key-safe.
func budgetMessage(spent float64) string {
	if spent > 0 {
		return "claude hit the --max-budget-usd cap after spending $" + formatUSD(spent)
	}
	return "claude hit the --max-budget-usd cap"
}

// budgetSuggestion points at the only useful next steps for a spent budget:
// raise the cap or switch to a cheaper model.
func budgetSuggestion(spent float64) string {
	if spent > 0 {
		return "Raise --max-budget-usd (already spent $" + formatUSD(spent) +
			") or switch to a cheaper model, then retry"
	}
	return "Raise --max-budget-usd or switch to a cheaper model, then retry"
}

// formatUSD renders a dollar amount with minimal digits (no trailing zeros, no
// scientific notation) so "$1" and "$0.2584" both read cleanly.
func formatUSD(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// suggestionFor echoes the SKILL dispatch table so the lead gets the same
// remediation hint inline that the SKILL documents.
func suggestionFor(code string) string {
	switch code {
	case ErrCodeBadArgs:
		return "Pass exactly one of --prompt or --prompt-file"
	case ErrCodeUnknownVendor:
		return "Run cc-fleet add <vendor> (or check cc-fleet list --json)"
	case ErrCodeVendorDisabled:
		return "Run cc-fleet edit <vendor> --enable"
	case ErrCodeFingerprintMissing, ErrCodeFingerprintStale:
		return "Run the FINGERPRINT self-heal flow (native probe → cc-fleet refresh-fingerprint), then retry"
	case ErrCodeKeyInvalid:
		return "Rotate the vendor API key; do not retry until fixed"
	case ErrCodeInsufficientBalance:
		return "Vendor out of credit — top up, switch vendor, or fall back to native Agent"
	case ErrCodeRateLimited:
		return "Wait briefly then retry once, or switch vendor"
	case ErrCodeModelNotFound:
		return "Run cc-fleet refresh <vendor> then retry, or drop --model to use the default"
	case ErrCodeVendorUnreachable:
		return "Run cc-fleet doctor; if urgent, fall back to native Agent"
	case ErrCodeTimeout:
		return "Real long task → raise --timeout (or use --background) and retry; suspected hang → switch vendor or fall back"
	case ErrCodeVendorAPIError:
		return "Retry once or switch vendor"
	case ErrCodeFailed:
		return "Inspect the error; retry or switch vendor"
	default:
		return ""
	}
}

// parseInner leniently decodes claude's result envelope. ok=false means "not a
// recognizable result envelope" (empty / garbage / non-result JSON), which the
// caller turns into SUBAGENT_FAILED rather than a false success.
func parseInner(stdout []byte) (innerEnvelope, bool) {
	var e innerEnvelope
	b := bytes.TrimSpace(stdout)
	if len(b) == 0 {
		return e, false
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, false
	}
	// Require some signal it's actually a result envelope. type:"result" is the
	// stable marker; accept is_error / result / subtype too so a future field
	// rename still parses rather than silently "succeeding".
	if e.Type != "result" && !e.IsError && e.Result == "" && e.Subtype == "" {
		return e, false
	}
	return e, true
}

// modelKey returns the model claude actually billed (the single key of inner
// modelUsage) as routing evidence; falls back to fallback (req.Model) when
// modelUsage is empty (e.g. {} on error).
func modelKey(modelUsage map[string]json.RawMessage, fallback string) string {
	for k := range modelUsage {
		if k != "" {
			return k
		}
	}
	return fallback
}

// failWithPreview builds the SUBAGENT_FAILED Result for a run that produced no
// parseable envelope. The message is canonical + a length-bounded preview of
// claude's OWN stderr.
//
// The preview is passed through redact.MaskKeyLike so a leaked key-like
// fragment on stderr can't reach the JSON envelope's ErrorMsg. Raw stderr is
// preserved for human-eye debugging via reportSubagent's text mode, but the
// JSON contract (which skills consume and may forward into reports) must be
// canonical.
func failWithPreview(req Request, stderr []byte, exitCode int) Result {
	msg := fmt.Sprintf("claude exited %d without a result envelope", exitCode)
	if prev := stderrPreview(stderr); prev != "" {
		msg += ": " + prev
	}
	return fail(ErrCodeFailed, msg, req.Vendor, suggestionFor(ErrCodeFailed))
}

// stderrPreview returns up to stderrPreviewMax bytes of stderr after passing
// them through redact.MaskKeyLike, honoring the "no key-like substrings in
// JSON" contract. Raw stderr for human debugging goes through the text-mode
// print path instead.
func stderrPreview(b []byte) string {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return ""
	}
	// Mask first, truncate second — truncating a key-like substring could
	// leave half of it visible.
	masked := redact.MaskKeyLike(b)
	if len(masked) > stderrPreviewMax {
		return string(masked[:stderrPreviewMax]) + "...(truncated)"
	}
	return string(masked)
}

func canonicalTimeoutMsg(timeout time.Duration) string {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return fmt.Sprintf("vendor subagent exceeded %s; process group killed", timeout)
}

// modelRejectionSignatures detect a 400 that means "bad --model name" rather
// than a generic bad request (vendors answer "supported names: …").
var modelRejectionSignatures = []string{
	"model not found", "invalid model", "unknown model", "supported names",
	"model_not_found", "does not exist", "no such model", "not a valid model",
	"模型不存在", "不支持的模型", "无效的模型",
}

func looksLikeModelRejection(text string) bool {
	h := strings.ToLower(text)
	for _, s := range modelRejectionSignatures {
		if strings.Contains(h, s) {
			return true
		}
	}
	return false
}

// transportSignatures catch a result text that describes a connection-layer
// failure when claude reported no api_error_status — surfaced as
// VENDOR_UNREACHABLE rather than a generic API error.
var transportSignatures = []string{
	"connection refused", "no such host", "dial tcp", "tls handshake",
	"network is unreachable", "connection reset", "timed out", "i/o timeout",
}

func looksLikeTransport(text string) bool {
	h := strings.ToLower(text)
	for _, s := range transportSignatures {
		if strings.Contains(h, s) {
			return true
		}
	}
	return false
}
