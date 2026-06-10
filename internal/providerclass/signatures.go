// Package providerclass is the one shared place that turns a raw provider failure
// into a stable, key-safe classification. Two call sites need the same answer:
//
//   - teardown/health.go scans a teammate's tmux pane text (`ps --check`);
//   - internal/subagent parses claude's inner `is_error` result envelope.
//
// Both must map provider noise into the same small vocabulary
// (insufficient_balance / cloudflare_blocked / auth / rate_limit / api_error) with
// the same priority, and a reachability probe (the spawn / subagent `--probe`) must
// classify a models-endpoint result identically. Keeping both here stops the two
// copies from drifting.
//
// Imports are deliberately limited to config / models / secrets / neterr so this
// package never forms an import cycle with spawn / teardown / subagent.
package providerclass

import "strings"

// Provider error classes returned by MatchClass. They match teardown's
// error_class vocabulary so the lead's mental model is identical across
// `ps --check` (teammate) and `subagent` error codes:
// auth↔KEY_INVALID, rate_limit↔RATE_LIMITED, insufficient_balance↔INSUFFICIENT_BALANCE,
// api_error↔PROVIDER_API_ERROR, cloudflare_blocked↔CODEX_CLOUDFLARE_BLOCKED.
const (
	ClassInsufficientBalance = "insufficient_balance"
	ClassAuth                = "auth"
	ClassRateLimit           = "rate_limit"
	ClassAPIError            = "api_error"
	ClassCloudflareBlocked   = "cloudflare_blocked"
)

// Signature sets are matched against the lowercased input. We prefer specific
// phrases (very low false-positive rate) and only use bare HTTP numbers in
// parenthesized form, e.g. "(401)" / "(429)", because that shape is almost
// always a status code in error context — whereas a bare "429" could be a
// timestamp, duration, or token count and would false-positive.
var (
	balanceSignatures = []string{
		"余额不足", "无可用资源包", "余额已用尽", "欠费",
		"insufficient balance", "insufficient_quota", "insufficient quota",
		"insufficient funds", "out of credits", "quota exceeded",
		"exceeded your current quota",
	}
	// cloudflareSignatures must be matched BEFORE authSignatures: a Cloudflare
	// edge block answers 403, so the generic "(403)" auth rule would otherwise
	// misread an IP/client block as a bad key.
	cloudflareSignatures = []string{
		"blocked by cloudflare", "cf-mitigated",
	}
	authSignatures = []string{
		"unauthorized", "invalid api key", "invalid_api_key",
		"incorrect api key", "api key is invalid", "authentication error",
		"authentication failed", "(401)", "(403)",
	}
	rateLimitSignatures = []string{
		"rate limit", "ratelimit", "rate_limit", "too many requests",
		"请求过于频繁", "(429)",
	}
	apiErrorSignatures = []string{
		"request rejected", "api error", "overloaded", "service unavailable",
		"internal server error", "bad gateway", "(500)", "(502)", "(503)", "(529)",
	}
)

// MatchClass classifies provider error text into one of the Class* constants, or
// "" when no signature matches.
//
// Priority: out-of-balance > cloudflare > auth > rate-limit > generic API
// error. A single real-world line like "API Error: Request rejected (429)
// 余额不足无可用资源包" carries several signals at once; the most actionable
// root cause wins — being out of balance means a retry can't help (top up /
// switch provider), whereas a bare 429 might clear on its own, so balance must
// outrank the 429 symptom. Cloudflare outranks auth because an edge block
// answers 403, which the generic auth rule would misread as a bad key.
func MatchClass(text string) string {
	h := strings.ToLower(text)
	switch {
	case containsAny(h, balanceSignatures...):
		return ClassInsufficientBalance
	case containsAny(h, cloudflareSignatures...):
		return ClassCloudflareBlocked
	case containsAny(h, authSignatures...):
		return ClassAuth
	case containsAny(h, rateLimitSignatures...):
		return ClassRateLimit
	case containsAny(h, apiErrorSignatures...):
		return ClassAPIError
	default:
		return ""
	}
}

// containsAny reports whether haystack contains any of the needles.
func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
