package codexproxy

// convCtx is the per-request conversion context threaded through an upstream's
// call (Anthropic -> upstream request) and convert (upstream SSE -> Anthropic).
// It collapses the former (model, apiKey) parameters and carries the tool-name map
// so a name sanitized on the way out is restored on the way back. Built once per
// inbound /v1/messages request and never shared between requests. upErrType is
// the only mutable field: convert writes it, and handleMessages reads it after
// convert returns on the same goroutine, so no lock is needed.
type convCtx struct {
	model   string
	apiKey  string       // presented upstream key (openai-*); "" for codex (OAuth bearer)
	toolMap *toolNameMap // nil when every tool name already conforms
	// upErrType is the Anthropic error type of an upstream mid-stream error event
	// (rate_limit_error / authentication_error / api_error), set by the converter
	// before it emits the error; it picks the non-streaming response status.
	upErrType string
}

func newConvCtx(areq *anthropicRequest, apiKey string) *convCtx {
	return &convCtx{model: areq.Model, apiKey: apiKey, toolMap: newToolNameMap(areq.Tools)}
}
