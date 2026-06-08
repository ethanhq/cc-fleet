package codexproxy

import (
	"context"
	"fmt"
	"io"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// upstream is the wire-specific half of the daemon: one implementation per
// protocol. call translates the Anthropic request to the upstream format, sends
// it, and returns the streaming body (or a classified *upstreamError); convert
// runs that stream through the upstream's SSE→Anthropic converter; models is the
// /v1/models list (static for codex, unused for openai — its models come from the
// real upstream's models_endpoint, not this daemon).
type upstream interface {
	call(ctx context.Context, areq *anthropicRequest, apiKey string) (io.ReadCloser, error)
	// convert runs the upstream's SSE→Anthropic converter. apiKey is the presented
	// key (openai-*) so a streaming error chunk that echoes it is redacted before
	// it reaches the client; codex ignores it (its bearer is OAuth, not the key).
	convert(body io.Reader, sink sseSink, model, apiKey string) error
	models() []string
}

// buildUpstream selects the upstream implementation for a daemon's protocol.
// codex builds its own OAuth token source; openai-* carries the real key per
// request (apiKey passed to call), so it only needs the upstream base URL.
func buildUpstream(protocol, upstreamURL string) (upstream, error) {
	switch protocol {
	case config.ProtocolCodexOAuth:
		source, err := newCLIRideStore()
		if err != nil {
			return nil, err
		}
		return newCodexUpstream(source), nil
	case config.ProtocolOpenAIChat:
		return newOpenAIChatUpstream(upstreamURL), nil
	case config.ProtocolOpenAIResponses:
		return newOpenAIResponsesUpstream(upstreamURL), nil
	default:
		return nil, fmt.Errorf("codexproxy: unsupported protocol %q", protocol)
	}
}
