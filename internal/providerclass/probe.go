package providerclass

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/models"
	"github.com/ethanhq/cc-fleet/internal/neterr"
	"github.com/ethanhq/cc-fleet/internal/secrets"
)

// probeTimeout caps the provider reachability check: long enough for a healthy
// provider, short enough that an outage doesn't stall the caller.
const probeTimeout = 3 * time.Second

// Probe is the outcome of a provider reachability check (Reachability). It is
// decision-only: callers map it onto their own Result type. Block=true means
// the caller should abort with Code; Warn (non-empty) is a non-blocking notice
// the caller prints to stderr.
type Probe struct {
	Block      bool   // true → caller should abort the operation
	Code       string // error code when Block: PROVIDER_UNREACHABLE | KEY_INVALID
	Msg        string // human message for the failure Result
	Suggestion string // remediation hint
	Warn       string // non-blocking warning (e.g. a 5xx); print to stderr, then proceed
}

// Reachability does a 3s GET against the provider's models_endpoint (with the
// provider key, best-effort) and classifies the outcome. spawn and subagent both
// call it so the classification stays single-sourced.
//
//   - transport failure (DNS / dial / TLS / timeout)  -> Block PROVIDER_UNREACHABLE
//   - HTTP 401 / 403                                   -> Block KEY_INVALID
//   - HTTP 2xx                                         -> no block
//   - other 4xx/5xx                                    -> no block + Warn
//   - 2xx-but-unparseable / odd response               -> no block
//
// Core principle: any HTTP response proves the network is reachable, so only a
// connection-layer failure is UNREACHABLE. The models endpoint is used only for
// probe/refresh — the real call authenticates against base_url at runtime — so a
// misbehaving endpoint must not block the caller.
//
// The Code values are the canonical error-code strings shared with spawn.Result
// and subagent.Result (their ErrCode* consts equal these literals).
func Reachability(v *config.Provider) Probe {
	if v == nil || v.ModelsEndpoint == "" {
		// No probe possible — treat as success rather than blocking.
		return Probe{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	// Best-effort key: a lookup failure must not block, so fall back to a
	// keyless reachability probe (classification below still holds).
	key, _ := secrets.Keyget(v.Name)

	_, err := models.FetchWithKey(ctx, v.ModelsEndpoint, key)
	switch {
	case err == nil:
		// 2xx: reachable and authorized.
		return Probe{}
	case errors.Is(err, models.ErrKeyInvalid):
		// HTTP 401: provider responded, but rejected the key.
		return Probe{
			Block:      true,
			Code:       "KEY_INVALID",
			Msg:        fmt.Sprintf("probe %s: HTTP 401 (key rejected)", v.ModelsEndpoint),
			Suggestion: "Verify the API key: cc-fleet edit " + v.Name + " (or re-add with the correct key)",
		}
	}

	var httpErr *models.HTTPStatusError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode == http.StatusForbidden {
			// HTTP 403: also an auth/permission failure, not unreachability.
			return Probe{
				Block:      true,
				Code:       "KEY_INVALID",
				Msg:        fmt.Sprintf("probe %s: HTTP 403 (key forbidden)", v.ModelsEndpoint),
				Suggestion: "Verify the API key / account permissions: cc-fleet edit " + v.Name,
			}
		}
		// Other 4xx/5xx: the provider is reachable, the endpoint just answered
		// unhappily. Don't block (the real call uses base_url, not this), but
		// surface a warning so a genuinely sick provider isn't silent.
		return Probe{
			Warn: fmt.Sprintf(
				"cc-fleet: warning: probe %s returned HTTP %d; provider reachable, continuing\n",
				v.ModelsEndpoint, httpErr.StatusCode),
		}
	}

	if neterr.IsTransport(err) {
		// Connection-layer failure: no HTTP response at all -> unreachable.
		return Probe{
			Block:      true,
			Code:       "PROVIDER_UNREACHABLE",
			Msg:        fmt.Sprintf("probe %s: %v", v.ModelsEndpoint, err),
			Suggestion: "Check network / DNS or run cc-fleet doctor",
		}
	}

	// Got an HTTP response we couldn't parse (e.g. 2xx with an odd body) or a
	// non-network error: the provider is reachable, so proceed.
	return Probe{}
}
