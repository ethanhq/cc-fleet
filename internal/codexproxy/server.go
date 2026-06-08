package codexproxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// models offered to a ChatGPT subscription (served at /v1/models for the probe and
// `cc-fleet models`). Pass-through: an unsupported slug surfaces the backend error.
var codexModels = []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5.2-codex"}

// StaticModels is the codex model list, copied for callers outside the package.
// `cc-fleet codex add` seeds it into the models cache so model resolution works
// before the lazily-started daemon has ever run (the add path skips the probe).
func StaticModels() []string { return append([]string(nil), codexModels...) }

// server is the loopback HTTP handler set: /v1/messages (Anthropic inbound) and
// /v1/models (probe). It records last-activity so the daemon can gauge idleness.
type server struct {
	upstream     *upstreamClient
	loopbackAuth string
	lastActivity atomic.Int64 // unix nanos of the last /v1/messages request
}

func newServer(source tokenSource, loopbackAuth string) *server {
	s := &server{upstream: newUpstreamClient(source), loopbackAuth: loopbackAuth}
	s.lastActivity.Store(time.Now().UnixNano())
	return s
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", s.handleMessages)
	mux.HandleFunc("/v1/models", s.handleModels)
	return mux
}

func (s *server) handleModels(w http.ResponseWriter, _ *http.Request) {
	data := make([]map[string]any, 0, len(codexModels))
	for _, m := range codexModels {
		data = append(data, map[string]any{"id": m, "type": "model"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func (s *server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// claude sends keyget's output (the loopback handshake secret) as x-api-key.
	if s.loopbackAuth != "" && r.Header.Get("x-api-key") != s.loopbackAuth {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "proxy handshake failed")
		return
	}
	s.lastActivity.Store(time.Now().UnixNano())

	var areq anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&areq); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "bad request body: "+err.Error())
		return
	}
	rreq, err := translateRequest(&areq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	body, _ := json.Marshal(rreq)

	resp, err := s.upstream.call(r.Context(), body)
	if err != nil {
		ue, _ := err.(*upstreamError)
		status, etype, msg := anthropicErrorFor(ue)
		writeAnthropicError(w, status, etype, msg)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	sink := &httpSSE{w: w, flusher: w.(http.Flusher)}
	conv := newStreamConverter(sink, areq.Model)
	// Convert already emitted the client-visible terminal event (a clean
	// message_stop, or an error event on a failed-upstream / read-error stream),
	// so its returned error needs no further handling on the wire.
	_ = conv.Convert(resp.Body)
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
// (status, type, message). The status is what vendorclass / the claude client see.
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
