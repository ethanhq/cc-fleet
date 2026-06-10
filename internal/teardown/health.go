package teardown

import (
	"github.com/ethanhq/cc-fleet/internal/providerclass"
	"github.com/ethanhq/cc-fleet/internal/tmux"
)

// Health status values reported by `cc-fleet ps --check`. They describe what a
// teammate's tmux pane currently SHOWS, not whether its task ultimately
// succeeded — the pane is the only window we have into a teammate's LLM state
// once it's spawned (lead↔teammate messaging goes through inbox files, and a
// wedged teammate stops writing those).
const (
	statusOK      = "ok"      // no provider API-error signature in recent pane output
	statusError   = "error"   // an API-error signature was found (see error_class)
	statusUnknown = "unknown" // pane could not be captured (gone / tmux unreachable)
)

// Error classes attached to a statusError teammate so the lead can dispatch on
// the actionable root cause without parsing prose (and without ever seeing the
// raw pane text, which may carry key fragments).
const (
	errClassRateLimit    = "rate_limit"           // HTTP 429 / "rate limit"
	errClassInsufficient = "insufficient_balance" // out of balance / quota
	errClassAuth         = "auth"                 // HTTP 401/403 / bad key
	errClassAPIError     = "api_error"            // generic provider API failure
	errClassCloudflare   = "cloudflare_blocked"   // Cloudflare edge blocked this IP/client
)

// captureFn is the seam tests substitute so they never need a live tmux server
// (and never touch a real pane). Production wiring is capturePane.
//
// The seam takes (socket, paneID) so the production call can scope capture-pane
// to the right tmux server — swarm panes live on a private socket the default
// server can't see, and a default-server capture silently fails for every swarm
// pane, marking them statusUnknown forever.
var captureFn = capturePane

// AnnotateHealth fills Status / ErrorClass / Detail on each teammate by
// capturing and classifying its tmux pane's recent output.
//
// Why this exists: a provider teammate whose API backend returns 429 / 401 /
// out-of-balance gets wedged in claude's retry loop. It never finishes AND
// never emits an idle notification — emitting one would require the LLM, which
// is exactly what's down. A lead that only "waits for idle" then blocks
// forever. This scan lets the lead poll for that state instead of waiting.
//
// SECURITY: a pane may contain fragments of the provider API key (e.g. echoed in
// a verbose error). classifyPaneOutput therefore returns only canonical,
// hard-coded strings; we NEVER copy any substring of the captured text into
// the result, and never log it.
//
// Best-effort: a pane that can't be captured (already gone, tmux down) is
// marked statusUnknown rather than failing the whole listing. Mutates and
// returns the same slice for call-site convenience.
func AnnotateHealth(teammates []Teammate) []Teammate {
	for i := range teammates {
		// Scope capture to the pane's owning server. Without socket-scoping the
		// swarm pane's default-server capture silently fails for every
		// out-of-tmux teammate.
		out, err := captureFn(teammates[i].Socket, teammates[i].PaneID)
		if err != nil {
			teammates[i].Status = statusUnknown
			teammates[i].Detail = "pane output unavailable (pane gone or tmux unreachable)"
			continue
		}
		status, class, detail := classifyPaneOutput(out)
		teammates[i].Status = status
		teammates[i].ErrorClass = class
		teammates[i].Detail = detail
	}
	return teammates
}

// capturePane returns the visible plain-text content of a tmux pane via
// internal/tmux.Server.CapturePane, funneling every tmux exec through the one
// Server.command outlet. No -e is passed, so escape sequences are stripped and
// the caller gets clean text to grep.
//
// socket is the tmux server socket name (empty = default server). For an
// out-of-tmux swarm pane, socket = "cc-fleet-swarm-<team>"; for an in-tmux pane,
// socket = "". The server inserts "-L <socket>" only when non-empty so the
// in-tmux path stays byte-identical.
func capturePane(socket, paneID string) (string, error) {
	return tmux.NewServer(socket).CapturePane(paneID)
}

// classifyPaneOutput inspects recent pane text for provider API-error signatures
// and returns (status, errorClass, detail).
//
// It returns ONLY canonical strings — never any substring of the input — so a
// key fragment in the pane can't leak into ps output (see AnnotateHealth's
// SECURITY note). The signature matching + priority (out-of-balance > cloudflare
// > auth > rate-limit > generic API error) live in internal/providerclass so the
// subagent envelope classifier shares the exact same vocabulary; here we map
// the shared class back onto teardown's status/error_class/detail triple.
func classifyPaneOutput(text string) (status, errorClass, detail string) {
	switch providerclass.MatchClass(text) {
	case providerclass.ClassInsufficientBalance:
		return statusError, errClassInsufficient,
			"provider account out of balance / quota — top up or switch provider"
	case providerclass.ClassCloudflareBlocked:
		return statusError, errClassCloudflare,
			"provider edge (Cloudflare) blocked this IP/client — switch network or retry later"
	case providerclass.ClassAuth:
		return statusError, errClassAuth,
			"provider rejected the API key (HTTP 401/403) — rotate the key"
	case providerclass.ClassRateLimit:
		return statusError, errClassRateLimit,
			"provider rate limit (HTTP 429) — wait then retry once, or switch provider"
	case providerclass.ClassAPIError:
		return statusError, errClassAPIError,
			"provider API error in pane — inspect with tmux capture-pane; consider switching provider"
	default:
		return statusOK, "", ""
	}
}
