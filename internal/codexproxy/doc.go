// Package codexproxy hosts the local Anthropic-Messages <-> OpenAI-Responses
// conversion daemon that lets a Codex/ChatGPT subscription drive OpenAI models as
// a cc-fleet provider. A launched claude process speaks the Anthropic Messages API
// to a loopback HTTP server here; the server translates each request to the OpenAI
// Responses API, calls the ChatGPT backend with a reused OAuth bearer, and
// translates the streamed reply back to Anthropic SSE.
//
// The OAuth bearer lives only inside this daemon: cc-fleet runs its own OAuth
// device-code login and keeps an independent token chain (it never reads or writes
// ~/.codex/auth.json), so reusing the subscription cannot disturb the codex CLI's
// own login. The claude->proxy hop carries only a low-value loopback handshake
// secret, never the upstream token.
package codexproxy
