package codexproxy

// Codex/ChatGPT OAuth + Responses protocol constants. These are factual interop
// values (a public PKCE client id and fixed endpoint URLs), not configuration.
const (
	// oauthClientID is the public PKCE client (no secret) shared by the codex CLI.
	oauthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// oauthTokenURL serves both the authorization-code exchange and refresh grants.
	oauthTokenURL = "https://auth.openai.com/oauth/token"

	// Device-code login: start, then poll for the authorization code + verifier.
	deviceUserCodeURL = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	deviceTokenURL    = "https://auth.openai.com/api/accounts/deviceauth/token"
	deviceRedirectURI = "https://auth.openai.com/deviceauth/callback"
	// deviceVerifyURL is shown to the user alongside the user code.
	deviceVerifyURL = "https://auth.openai.com/codex/device"

	// responsesURL is the ChatGPT-backend Responses endpoint a subscription token
	// authenticates against (api.openai.com does not accept this token).
	responsesURL = "https://chatgpt.com/backend-api/codex/responses"
	// modelsURL lists the models available to the subscription.
	modelsURL = "https://chatgpt.com/backend-api/codex/models"
)

// Upstream request headers. originator + a codex-shaped User-Agent present this as
// a genuine codex client (required to pass the backend's edge); OpenAI-Beta and
// chatgpt-account-id are required for a successful Responses call.
const (
	originatorValue = "codex_cli_rs"
	userAgentValue  = "codex_cli_rs/0.137.0"
	openAIBetaValue = "responses=experimental"

	// jwtAuthClaim is the namespaced id_token/access_token claim holding the
	// chatgpt_account_id used for the chatgpt-account-id header.
	jwtAuthClaim = "https://api.openai.com/auth"
)

// SecretBackend is the providers.toml secret_backend value selecting a codex provider,
// and the sentinel secret_ref it pairs with (config requires a non-empty secret_ref).
const (
	SecretBackend = "codex-oauth"
	SecretRef     = "codex-oauth"
)
