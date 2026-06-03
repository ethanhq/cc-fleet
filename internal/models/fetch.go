package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/redact"
	"github.com/ethanhq/cc-fleet/internal/secrets"
)

// ErrKeyInvalid is a sentinel returned when the vendor responds 401 to the
// /v1/models request. The skill (and `cc-fleet refresh --json`) dispatch on
// this via errors.Is to map it to error_code=KEY_INVALID without parsing
// prose.
var ErrKeyInvalid = errors.New("vendor key invalid (HTTP 401)")

// HTTPStatusError reports a non-2xx, non-401 response from a models endpoint.
// (401 maps to the ErrKeyInvalid sentinel instead.) It carries the raw
// StatusCode so a reachability probe can tell an auth failure (403) apart from
// "vendor is reachable, the endpoint just answered unhappily" (other 4xx/5xx).
// In every case the vendor returned an HTTP response, so the network is
// reachable — only a transport-layer failure (no response at all) means
// unreachable.
//
// The default Error() string must NEVER include raw vendor body bytes —
// vendors occasionally echo bearer keys or x-api-key fragments in 4xx/5xx
// bodies. The body is preserved on the
// struct but only exposed via BodyPreview() (already sanitized) for human
// diagnostics. JSON envelopes / log lines that flow into stable contracts
// (refresh --json, add --json, etc.) MUST use Error() — which is canonical and
// safe — never BodyPreview().
type HTTPStatusError struct {
	StatusCode int
	Endpoint   string
	body       string // sanitized; access via BodyPreview()
}

// Error returns a canonical, key-safe summary suitable for JSON envelopes,
// logs, and any caller that might persist or forward the string. No raw vendor
// body bytes ever appear here.
func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("models: http %d from %s", e.StatusCode, e.Endpoint)
}

// BodyPreview returns the (already key-mask-sanitized) body snippet for human
// debugging only. It exists so a CLI text mode can show the snippet without
// it sneaking into JSON / log surfaces that callers stream into reports.
// Empty when no body was captured.
func (e *HTTPStatusError) BodyPreview() string {
	if e == nil {
		return ""
	}
	return e.body
}

// fetchTimeout caps each /v1/models HTTP request. Set high enough to tolerate
// trans-Pacific latency to Anthropic-compat shims, low enough to surface
// hangs to the user / skill within one terminal heartbeat.
const fetchTimeout = 10 * time.Second

// bodySnippetMax bounds how much of an error response body we echo into the
// error message. Vendors occasionally include the bearer key in 401 bodies
// ("Invalid key sk-..."), so we both cap the snippet AND rely on
// truncateBodyPreview to ensure callers never log unbounded vendor bytes.
const bodySnippetMax = 200

// maxModelsBody bounds how much of a /v1/models response we buffer in memory. A
// model list is normally a few KB; the cap only stops a misconfigured or hostile
// endpoint from OOMing add/refresh by streaming an unbounded body within
// fetchTimeout (which caps time, not bytes). Package var so tests can shrink it.
var maxModelsBody = 8 << 20 // 8 MiB

// Fetch hits v.ModelsEndpoint and returns the parsed model list.
//
// It looks up the vendor's API key via the secrets package (so the caller
// never has to handle key bytes itself) and sends it as a Bearer token.
//
// Response parsing handles both common shapes:
//
//   - OpenAI-style:    {"data": [{"id": "...", "owned_by": "..."}]}
//   - Anthropic-style: {"data": [{"id": "...", "display_name": "..."}]}
//   - Bare array:      [{"id": "..."}, ...]            (some self-hosted shims)
//
// Errors:
//
//   - 401                       -> ErrKeyInvalid (sentinel, wrapped)
//   - other >= 400              -> error containing status code + truncated body
//   - parse failure             -> error containing endpoint URL (no key)
//   - context.DeadlineExceeded  -> propagated via ctx
//
// SECURITY: the bearer key is set on a single request header; it is never
// logged, never placed into returned error messages, and the response body is
// truncated to bodySnippetMax bytes before being formatted into any error.
func Fetch(ctx context.Context, v *config.Vendor) ([]Model, error) {
	if v == nil {
		return nil, errors.New("models: nil Vendor")
	}
	if v.ModelsEndpoint == "" {
		return nil, fmt.Errorf("models: vendor %q has empty models_endpoint", v.Name)
	}

	key, err := secrets.Keyget(v.Name)
	if err != nil {
		return nil, fmt.Errorf("models: %s: keyget: %w", v.Name, err)
	}

	return FetchWithKey(ctx, v.ModelsEndpoint, key)
}

// FetchWithKey is Fetch's HTTP core: it GETs endpoint with key as the bearer
// token and parses the model list, WITHOUT consulting the secrets store. It
// exists so callers that already hold a key in memory can reuse the exact same
// request + parse + error-classification path without persisting the key first:
//
//   - the add wizard (key just typed in the TUI, not yet written to disk), and
//   - the spawn reachability probe (which classifies the result into
//     VENDOR_UNREACHABLE / KEY_INVALID / OK).
//
// An empty key sends no Authorization header, so it doubles as a keyless
// reachability probe.
//
// Errors (mirrors Fetch's contract — existing callers dispatch on these):
//
//   - 401                       -> ErrKeyInvalid (sentinel, wrapped)
//   - other >= 400              -> *HTTPStatusError (carries StatusCode)
//   - transport failure         -> wrapped url/net error (errors.As detectable)
//   - context.DeadlineExceeded  -> propagated via ctx (wrapped)
//   - parse failure             -> error containing endpoint URL (no key, no body)
//
// SECURITY: the bearer key is set on a single request header; it is never
// logged, never placed into returned error messages, and response bodies are
// truncated to bodySnippetMax bytes before being formatted into any error.
func FetchWithKey(ctx context.Context, endpoint string, key []byte) ([]Model, error) {
	if endpoint == "" {
		return nil, errors.New("models: empty models_endpoint")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("models: build request for %s: %w", endpoint, err)
	}
	// Bearer token. Some vendors also gate on a Anthropic-style x-api-key
	// header, but every vendor we target (OpenAI-compat, Anthropic-compat
	// shims) accepts the Bearer form on /v1/models. Keep it minimal. An empty
	// key (keyless reachability probe) sends no auth header at all.
	if len(key) > 0 {
		req.Header.Set("Authorization", "Bearer "+string(key))
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: fetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		// http.Client wraps context errors (timeout/cancel) — preserve via %w
		// so callers can errors.Is(err, context.DeadlineExceeded).
		return nil, fmt.Errorf("models: http %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	// Read body up-front so error paths can include a truncated snippet without
	// burning the body before parsing — but BOUNDED: a misconfigured or hostile
	// endpoint can stream gigabytes within fetchTimeout. LimitReader+1 lets us
	// tell "exactly the cap" from "over the cap". Status classification below is
	// unchanged (it doesn't need the whole body); only the 2xx parse path treats
	// an overflow as an error, so an oversized 401/5xx still classifies normally.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(maxModelsBody)+1))
	if readErr != nil {
		return nil, fmt.Errorf("models: read body from %s (status %d): %w",
			endpoint, resp.StatusCode, readErr)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("models: %s: %w", endpoint, ErrKeyInvalid)
	}
	if resp.StatusCode >= 400 {
		return nil, &HTTPStatusError{
			StatusCode: resp.StatusCode,
			Endpoint:   endpoint,
			// Still capture a snippet for human debug via BodyPreview(), but
			// pre-sanitize through redact.MaskKeyLike so a vendor that echoes a
			// bearer key into the body cannot leak it even via the preview surface.
			body: truncateBodyPreview(redact.MaskKeyLike(body)),
		}
	}

	// Success path needs the whole body to parse — so an oversized response is a
	// hard error here (size only, never body bytes: key-safe), not a silent
	// truncation that would mis-parse into a confusing "neither data nor array".
	if len(body) > maxModelsBody {
		return nil, fmt.Errorf("models: response from %s exceeds %d bytes", endpoint, maxModelsBody)
	}

	out, err := parseModelsBody(body)
	if err != nil {
		// Do NOT include the body — it may contain unexpected vendor data
		// (in degenerate cases including a key echo). Endpoint is safe.
		return nil, fmt.Errorf("models: parse response from %s: %w", endpoint, err)
	}
	return out, nil
}

// rawModelEntry covers both vendor response styles: OpenAI uses owned_by,
// Anthropic uses display_name. We accept either and surface a single
// `owned_by` to callers so the on-disk cache schema stays small.
type rawModelEntry struct {
	ID          string `json:"id"`
	OwnedBy     string `json:"owned_by"`
	DisplayName string `json:"display_name"`
}

// parseModelsBody decodes the response in either the {"data":[...]} envelope
// or as a bare JSON array. Each entry may carry either OpenAI's `owned_by`
// or Anthropic's `display_name` — we keep `owned_by` populated from whichever
// is present (preferring `owned_by` when both appear).
func parseModelsBody(body []byte) ([]Model, error) {
	// First try the {"data": [...]} envelope. This covers both OpenAI and
	// Anthropic (Anthropic's /v1/models also returns a `data` array).
	var enveloped struct {
		Data []rawModelEntry `json:"data"`
	}
	if err := json.Unmarshal(body, &enveloped); err == nil && enveloped.Data != nil {
		return toModels(enveloped.Data), nil
	}

	// Fallback: bare top-level array (some self-hosted compat shims).
	var bare []rawModelEntry
	if err := json.Unmarshal(body, &bare); err == nil {
		return toModels(bare), nil
	}

	return nil, errors.New("response is neither {\"data\":[...]} nor a JSON array of models")
}

// toModels normalizes parsed entries to []Model, preferring owned_by when set,
// falling back to display_name otherwise. Entries with no id are skipped.
func toModels(entries []rawModelEntry) []Model {
	out := make([]Model, 0, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		owned := e.OwnedBy
		if owned == "" {
			owned = e.DisplayName
		}
		out = append(out, Model{ID: e.ID, OwnedBy: owned})
	}
	return out
}

// truncateBodyPreview returns the first bodySnippetMax bytes of body as a
// string, with an ellipsis marker if more was elided. The body is expected to
// have already passed through redact.MaskKeyLike — this helper only handles
// length capping (sanitize first, truncate second).
func truncateBodyPreview(body []byte) string {
	if len(body) <= bodySnippetMax {
		return string(body)
	}
	return string(body[:bodySnippetMax]) + "...(truncated)"
}
