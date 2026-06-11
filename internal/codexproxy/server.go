package codexproxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// models offered to a ChatGPT subscription (served at /v1/models for the probe and
// `cc-fleet models`). Pass-through: an unsupported slug surfaces the backend error.
var codexModels = []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5.2-codex"}

// StaticModels is the codex model list, copied for callers outside the package.
// `cc-fleet codex add` seeds it into the models cache so model resolution works
// before the lazily-started daemon has ever run (the add path skips the probe).
func StaticModels() []string { return append([]string(nil), codexModels...) }

// server is the loopback HTTP handler set: /v1/messages (Anthropic inbound),
// /v1/models (codex probe list), and /healthz (upstream-independent readiness).
// It records last-activity so the daemon can gauge idleness. The upstream + auth
// behavior are chosen by the daemon's protocol.
type server struct {
	up           upstream
	protocol     string
	handshake    string       // handshake secret gating /shutdown for every protocol, and /v1/messages for codex-oauth; "" only before Serve loads it
	lastActivity atomic.Int64 // unix nanos of the last /v1/messages request
	shutdown     func()       // drains the listener; set by Serve, nil in handler-only tests
}

func newServer(up upstream, protocol, handshake string) *server {
	s := &server{up: up, protocol: protocol, handshake: handshake}
	s.lastActivity.Store(time.Now().UnixNano())
	return s
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", s.handleMessages)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/shutdown", s.handleShutdown)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// handleShutdown drains the daemon on an authenticated request. The handshake
// secret is required for every protocol (an openai daemon needs no secret to serve
// /v1/messages, but a bare /shutdown would be an any-local-process kill switch), so
// a missing or wrong x-api-key is refused. On a match it responds 200, then drains
// in a goroutine so the response flushes before the listener closes. The loopback
// binding is enforced by the listener.
func (s *server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.handshake == "" || r.Header.Get("x-api-key") != s.handshake {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)
	if s.shutdown != nil {
		go s.shutdown()
	}
}

func (s *server) handleModels(w http.ResponseWriter, _ *http.Request) {
	list := s.up.models()
	data := make([]map[string]any, 0, len(list))
	for _, m := range list {
		data = append(data, map[string]any{"id": m, "type": "model"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// authorize checks the inbound x-api-key for the daemon's protocol and returns the
// credential to use upstream. codex requires the handshake secret (its OAuth
// bearer lives only here) and uses no upstream key; an openai-* daemon forwards
// the presented real key verbatim as the upstream Bearer.
func (s *server) authorize(key string) (upstreamKey string, ok bool) {
	if s.protocol == config.ProtocolCodexOAuth {
		if s.handshake == "" || key != s.handshake {
			return "", false
		}
		return "", true
	}
	if key == "" {
		return "", false
	}
	return key, true
}

func (s *server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	upstreamKey, ok := s.authorize(r.Header.Get("x-api-key"))
	if !ok {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "proxy handshake failed")
		return
	}
	s.lastActivity.Store(time.Now().UnixNano())

	var areq anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&areq); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "bad request body: "+err.Error())
		return
	}

	// One per-request conversion context (model + upstream key + tool-name map),
	// shared read-only by call and convert.
	cc := newConvCtx(&areq, upstreamKey)
	body, err := s.up.call(r.Context(), &areq, cc)
	if err != nil {
		ue, _ := err.(*upstreamError)
		status, etype, msg := anthropicErrorFor(ue)
		writeAnthropicError(w, status, etype, msg)
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	sink := &httpSSE{w: w, flusher: w.(http.Flusher)}
	// convert already emitted the client-visible terminal event (a clean
	// message_stop, or an error event on a failed-upstream / read-error stream),
	// so its returned error needs no further handling on the wire.
	_ = s.up.convert(body, sink, cc)
}

// httpSSE writes Anthropic SSE events to the response and flushes each one.
type httpSSE struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (h *httpSSE) event(name string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	h.flusher.Flush()
	return nil
}

// anthropicErrorFor maps a classified upstream failure to an Anthropic error
// (status, type, message). The status is what providerclass / the claude client see.
func anthropicErrorFor(ue *upstreamError) (int, string, string) {
	if ue == nil {
		return http.StatusBadGateway, "api_error", "codex upstream error"
	}
	switch ue.kind {
	case upQuota:
		return http.StatusTooManyRequests, "rate_limit_error", ue.message
	case upCloudflare:
		return http.StatusForbidden, "api_error", ue.message
	case upAuth:
		return http.StatusUnauthorized, "authentication_error", ue.message
	case upTransient:
		return http.StatusBadGateway, "api_error", ue.message
	default:
		return http.StatusBadRequest, "invalid_request_error", ue.message
	}
}

func writeAnthropicError(w http.ResponseWriter, status int, etype, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": etype, "message": message},
	})
}
