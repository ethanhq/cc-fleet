package codexproxy

// convCtx is the per-request, immutable conversion context threaded through an
// upstream's call (Anthropic -> upstream request) and convert (upstream SSE ->
// Anthropic). It collapses the former (model, apiKey) parameters and carries the
// tool-name map so a name sanitized on the way out is restored on the way back.
// Built once per inbound /v1/messages request and only read afterwards, so it is
// safe to share across the call/convert goroutines without a lock even though the
// daemon serves requests concurrently.
type convCtx struct {
	model   string
	apiKey  string       // presented upstream key (openai-*); "" for codex (OAuth bearer)
	toolMap *toolNameMap // nil when every tool name already conforms
}

func newConvCtx(areq *anthropicRequest, apiKey string) *convCtx {
	return &convCtx{model: areq.Model, apiKey: apiKey, toolMap: newToolNameMap(areq.Tools)}
}
