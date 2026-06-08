package codexproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
)

// openaiResponsesUpstream speaks the OpenAI Responses API
// (POST <upstream>/responses) with a per-request Bearer key. It REUSES codex's
// Responses request translator + SSE converter; only the auth, URL, headers, and
// the max_output_tokens cap differ (codex omits the cap and rides the ChatGPT
// backend over OAuth). It is the upstream for the openai-responses protocol.
type openaiResponsesUpstream struct {
	http    *http.Client
	baseURL string
}

func newOpenAIResponsesUpstream(baseURL string) *openaiResponsesUpstream {
	return &openaiResponsesUpstream{http: &http.Client{Timeout: 0}, baseURL: baseURL}
}

// models is empty: the model list comes from the real upstream models_endpoint.
func (u *openaiResponsesUpstream) models() []string { return nil }

func (u *openaiResponsesUpstream) convert(body io.Reader, sink sseSink, model, apiKey string) error {
	c := newStreamConverter(sink, model)
	c.redact = func(s string) string { return redactKey(s, apiKey) }
	return c.Convert(body)
}

// call translates the Anthropic request with the shared Responses translator,
// adds the max_output_tokens cap (a billed account honors it), and sends it with
// Authorization: Bearer <apiKey> — no codex headers, no OAuth refresh, no
// Cloudflare path. A non-2xx body is redacted before becoming an upstreamError.
func (u *openaiResponsesUpstream) call(ctx context.Context, areq *anthropicRequest, apiKey string) (io.ReadCloser, error) {
	rreq, err := translateRequest(areq)
	if err != nil {
		return nil, &upstreamError{upBadRequest, http.StatusBadRequest, err.Error()}
	}
	if areq.MaxTokens > 0 {
		rreq.MaxOutputTokens = areq.MaxTokens
	}
	body, _ := json.Marshal(rreq)
	endpoint, err := url.JoinPath(u.baseURL, "responses")
	if err != nil {
		return nil, &upstreamError{upBadRequest, http.StatusBadRequest, "invalid upstream url"}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, &upstreamError{upTransient, http.StatusBadGateway, "openai upstream: " + redactKey(err.Error(), apiKey)}
	}
	if resp.StatusCode/100 == 2 {
		return resp.Body, nil
	}
	return nil, classifyOpenAI(resp, apiKey)
}
