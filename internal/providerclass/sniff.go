package providerclass

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// messagesProbeBody is a minimal syntactically-valid Messages request. The probe
// attaches no credentials, so an Anthropic-protocol endpoint that authenticates
// requests rejects it at auth (401/403) without reaching a model.
const messagesProbeBody = `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`

// sniffClient never follows redirects: a redirecting base_url answers with its
// 3xx (not the redirect target's status), so a host canonicalization can't turn
// the POST into a GET whose 404 would read as a protocol mismatch.
var sniffClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// MessagesRouteMissing reports strong evidence that baseURL does not serve the
// Anthropic Messages API: an unauthenticated POST {base}/v1/messages answering
// HTTP 404. Status is the one usable discriminator — verified Anthropic-protocol
// providers answer the unauthenticated probe 401/403 (auth precedes routing)
// while OpenAI-only endpoints 404 the route, and auth-failure BODIES vary too
// much to classify (plain text, flat JSON, OpenAI-shaped envelopes). A 404 that
// carries an Anthropic error envelope is treated as an Anthropic endpoint after
// all (e.g. an anonymously-reachable gateway resolving the probe's dummy
// model), and a base_url carrying userinfo, a query, or a fragment is not
// probed at all (the client would send its userinfo as Basic auth). Every other
// outcome — any status, redirect, timeout, transport failure — returns false:
// this is evidence, not proof, so the guard blocks only on the positive signal.
func MessagesRouteMissing(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.JoinPath("v1", "messages").String(), strings.NewReader(messagesProbeBody))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := sniffClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		return false
	}
	return !anthropicErrorEnvelope(resp.Body)
}

// anthropicErrorEnvelope reports whether body is an Anthropic-format error
// ({"type":"error","error":{...}}), reading at most a small prefix.
func anthropicErrorEnvelope(body io.Reader) bool {
	var e struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 4096)).Decode(&e); err != nil {
		return false
	}
	return e.Type == "error" && len(e.Error) > 0
}
