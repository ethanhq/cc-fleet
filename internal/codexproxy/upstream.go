package codexproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// upstreamError carries a classified upstream failure so the server can map it to an
// Anthropic-shaped error body + HTTP status that vendorclass understands.
type upstreamError struct {
	kind    upstreamKind
	status  int
	message string
}

func (e *upstreamError) Error() string { return e.message }

type upstreamKind int

const (
	upTransient  upstreamKind = iota // 5xx / network: retryable
	upAuth                           // 401/403 after a refresh attempt
	upCloudflare                     // 403 cf-mitigated: IP/fingerprint block
	upQuota                          // 429 usage limit: terminal
	upBadRequest                     // 4xx request shape
)

// upstreamClient POSTs to the ChatGPT Responses backend with the reused bearer and
// the codex client headers, refreshing once on a 401.
type upstreamClient struct {
	http   *http.Client
	source tokenSource
}

func newUpstreamClient(source tokenSource) *upstreamClient {
	return &upstreamClient{http: &http.Client{Timeout: 0}, source: source}
}

// call sends the Responses body and returns the streaming response on success. The
// caller owns resp.Body. On failure it returns a classified *upstreamError.
func (u *upstreamClient) call(ctx context.Context, body []byte) (*http.Response, error) {
	resp, gen, err := u.do(ctx, body)
	if err != nil {
		return nil, err
	}
	// One refresh+retry on a 401 (expired access token mid-flight): invalidate
	// exactly the generation that was sent, so a token already rotated by a
	// concurrent caller is never wiped.
	if resp.StatusCode == http.StatusUnauthorized {
		b := drain(resp)
		u.source.invalidate(gen)
		resp, _, err = u.do(ctx, body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized {
			drain(resp)
			return nil, &upstreamError{upAuth, http.StatusUnauthorized, "codex auth rejected: " + b}
		}
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	return nil, classifyUpstream(resp)
}

// do sends one upstream request, returning the bearer generation it used so a
// 401 can invalidate precisely that credential.
func (u *upstreamClient) do(ctx context.Context, body []byte) (*http.Response, uint64, error) {
	bear, err := u.source.token(ctx)
	if err != nil {
		if errors.Is(err, ErrReauth) {
			return nil, 0, &upstreamError{upAuth, http.StatusUnauthorized, "codex login required (run: cc-fleet codex login)"}
		}
		return nil, 0, &upstreamError{upTransient, http.StatusBadGateway, "codex token: " + err.Error()}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, responsesURL, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bear.accessToken)
	req.Header.Set("chatgpt-account-id", bear.accountID)
	req.Header.Set("OpenAI-Beta", openAIBetaValue)
	req.Header.Set("originator", originatorValue)
	req.Header.Set("User-Agent", userAgentValue)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, 0, &upstreamError{upTransient, http.StatusBadGateway, "codex upstream: " + err.Error()}
	}
	return resp, bear.generation, nil
}

// classifyUpstream maps a non-2xx Responses status+body to an upstreamError.
func classifyUpstream(resp *http.Response) *upstreamError {
	body := drain(resp)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return &upstreamError{upQuota, http.StatusTooManyRequests, quotaMessage(body)}
	case resp.StatusCode == http.StatusForbidden && isCloudflareChallenge(resp.Header, body):
		return &upstreamError{upCloudflare, http.StatusForbidden, "blocked by Cloudflare (codex backend rejected this IP/client)"}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &upstreamError{upAuth, resp.StatusCode, "codex auth rejected: " + body}
	case resp.StatusCode/100 == 5:
		return &upstreamError{upTransient, resp.StatusCode, fmt.Sprintf("codex upstream http %d", resp.StatusCode)}
	default:
		return &upstreamError{upBadRequest, resp.StatusCode, fmt.Sprintf("codex upstream http %d: %s", resp.StatusCode, body)}
	}
}

// quotaMessage renders a 429: the backend's error code and, when reported, the
// reset time (error.resets_at, unix seconds) — the quota signal a lead can act
// on. An unparseable body falls back to a bounded raw snippet.
func quotaMessage(body string) string {
	var doc struct {
		Error struct {
			Code     string `json:"code"`
			ResetsAt int64  `json:"resets_at"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &doc) != nil || doc.Error.Code == "" {
		return "codex usage limit reached: " + body
	}
	msg := "codex " + doc.Error.Code
	if doc.Error.ResetsAt > 0 {
		msg += " (resets at " + time.Unix(doc.Error.ResetsAt, 0).UTC().Format(time.RFC3339) + ")"
	}
	return msg
}

// isCloudflareChallenge detects a Cloudflare edge block from headers/body markers.
func isCloudflareChallenge(h http.Header, body string) bool {
	if h.Get("cf-mitigated") != "" {
		return true
	}
	lower := body
	if len(lower) > 4096 {
		lower = lower[:4096]
	}
	return (h.Get("Server") == "cloudflare") &&
		(strings.Contains(lower, "Just a moment") || strings.Contains(lower, "Attention Required") || strings.Contains(lower, "cf-mitigated"))
}

// drain reads a bounded prefix of an error body (never the success stream) so the
// connection can be reused, returning a short, log-safe snippet.
func drain(resp *http.Response) string {
	const max = 2048
	b, _ := io.ReadAll(io.LimitReader(resp.Body, max))
	resp.Body.Close()
	return string(b)
}
