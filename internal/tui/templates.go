package tui

import "github.com/ethanhq/cc-fleet/internal/config"

// Template is a built-in vendor seed: prefill values for the add wizard so a
// user picking "DeepSeek" doesn't have to type the base_url / models_endpoint
// by hand. These are SEEDS — vendor URLs and model ids drift over time, so the
// add wizard lets the user confirm or edit every field, and `cc-fleet add`
// still probes /v1/models before committing.
//
// Each entry follows the vendor's published Anthropic-compatible endpoint. An
// entry WITHOUT a Note has a models_endpoint that has been probe-verified; an
// entry WITH a Note has an inferred or plan-specific endpoint flagged for the
// user to confirm (the probe on add does this automatically). Only first-party
// LLM vendors that run their own models are seeded — not Claude-relay
// aggregators, which run Claude and defeat the point of a vendor teammate.
type Template struct {
	Name           string // vendors.toml table name (lowercase id)
	Label          string // display name in the picker
	BaseURL        string // ANTHROPIC_BASE_URL
	ModelsEndpoint string // /v1/models URL used for the probe + refresh
	DefaultModel   string // suggested default model id
	Note           string // optional caveat shown in the picker preview
}

// OAITemplate seeds the OpenAI-protocol add form. base_url is the loopback
// conversion-daemon URL (auto-assigned on add), so it is NOT a template field;
// upstream_url is the real OpenAI-compatible endpoint and Protocol selects the
// wire surface (Chat Completions vs Responses).
type OAITemplate struct {
	Label        string // picker label
	Name         string // default provider id
	Protocol     string // config.ProtocolOpenAIChat | config.ProtocolOpenAIResponses
	UpstreamURL  string // real OpenAI-compatible base, usually ending in /v1
	DefaultModel string // suggested default (blank → pick from the probed list)
	Note         string
}

// OAITemplates seeds the OpenAI-protocol picker. A synthetic "Custom" entry is
// appended by the picker, so it is intentionally NOT in this slice.
var OAITemplates = []OAITemplate{
	{
		Label:       "OpenAI · Responses API (official)",
		Name:        "openai",
		Protocol:    config.ProtocolOpenAIResponses,
		UpstreamURL: "https://api.openai.com/v1",
		Note:        "High-fidelity reasoning surface; pick the default model from the probed list.",
	},
	{
		Label:       "OpenAI · Chat Completions (official)",
		Name:        "openai-chat",
		Protocol:    config.ProtocolOpenAIChat,
		UpstreamURL: "https://api.openai.com/v1",
	},
	{
		Label:    "OpenAI-compatible · Chat (Groq / Together / Fireworks / vLLM …)",
		Name:     "",
		Protocol: config.ProtocolOpenAIChat,
		Note:     "Set upstream_url to the vendor's base (often …/v1, e.g. https://api.groq.com/openai/v1).",
	},
}

// Templates is the built-in seed table. Order is the display order in the
// add wizard's picker. A synthetic "Custom" entry (empty fields) is appended
// by the picker itself, so it is intentionally NOT in this slice.
var Templates = []Template{
	// Core vendors — widely used; endpoints verified.
	{
		Name:           "deepseek",
		Label:          "DeepSeek",
		BaseURL:        "https://api.deepseek.com/anthropic",
		ModelsEndpoint: "https://api.deepseek.com/v1/models",
		DefaultModel:   "deepseek-v4-flash",
	},
	{
		Name:           "kimi",
		Label:          "Moonshot Kimi",
		BaseURL:        "https://api.moonshot.cn/anthropic",
		ModelsEndpoint: "https://api.moonshot.cn/anthropic/v1/models",
		DefaultModel:   "kimi-latest",
	},
	{
		Name:           "glm",
		Label:          "Zhipu GLM",
		BaseURL:        "https://open.bigmodel.cn/api/anthropic",
		ModelsEndpoint: "https://open.bigmodel.cn/api/paas/v4/models",
		DefaultModel:   "glm-4.6",
	},
	{
		Name:           "qwen",
		Label:          "Qwen (Alibaba DashScope)",
		BaseURL:        "https://dashscope.aliyuncs.com/apps/anthropic",
		ModelsEndpoint: "https://dashscope.aliyuncs.com/compatible-mode/v1/models",
		DefaultModel:   "qwen-max",
		Note:           "DashScope endpoints vary by region and plan; if the probe fails on add, correct base_url per the DashScope docs.",
	},
	{
		Name:           "minimax",
		Label:          "MiniMax",
		BaseURL:        "https://api.minimaxi.com/anthropic",
		ModelsEndpoint: "https://api.minimaxi.com/v1/models",
		DefaultModel:   "MiniMax-M2",
	},
	{
		Name:           "xiaomimimo",
		Label:          "Xiaomi MiMo",
		BaseURL:        "https://api.xiaomimimo.com/anthropic",
		ModelsEndpoint: "https://api.xiaomimimo.com/v1/models",
		DefaultModel:   "mimo-v2.5-pro",
		Note:           "models_endpoint sits at the host root (not under /anthropic); probe-verified.",
	},
	// Additional first-party LLM vendors — newer; entries with a Note have an
	// inferred or plan-specific endpoint that the probe on add confirms.
	{
		Name:           "zai",
		Label:          "Zhipu GLM (z.ai, international)",
		BaseURL:        "https://api.z.ai/api/anthropic",
		ModelsEndpoint: "https://api.z.ai/api/paas/v4/models",
		DefaultModel:   "glm-4.6",
		Note:           "International GLM site; same shape as the mainland endpoint but a separate API key.",
	},
	{
		Name:           "stepfun",
		Label:          "StepFun",
		BaseURL:        "https://api.stepfun.com/step_plan",
		ModelsEndpoint: "https://api.stepfun.com/v1/models",
		DefaultModel:   "step-3.5-flash-2603",
		Note:           "base_url is the step-plan (coding) endpoint; models_endpoint (/v1) is inferred — the probe on add verifies it.",
	},
	{
		Name:           "longcat",
		Label:          "LongCat (Meituan)",
		BaseURL:        "https://api.longcat.chat/anthropic",
		ModelsEndpoint: "https://api.longcat.chat/v1/models",
		DefaultModel:   "LongCat-Flash-Chat",
		Note:           "models_endpoint is inferred — the probe on add verifies it.",
	},
	{
		Name:           "volcengine",
		Label:          "Volcengine Ark (ByteDance)",
		BaseURL:        "https://ark.cn-beijing.volces.com/api/coding",
		ModelsEndpoint: "https://ark.cn-beijing.volces.com/api/v3/models",
		DefaultModel:   "ark-code-latest",
		Note:           "Coding-plan endpoint with a fixed code model; the general API addresses models by endpoint id (ep-...), so the model list may come back empty — enter the model by hand.",
	},
	{
		Name:           "doubao",
		Label:          "Doubao Seed (ByteDance Volcengine)",
		BaseURL:        "https://ark.cn-beijing.volces.com/api/compatible",
		ModelsEndpoint: "https://ark.cn-beijing.volces.com/api/v3/models",
		DefaultModel:   "doubao-seed-2-0-code-preview-latest",
		Note:           "Same endpoint-id scheme as Volcengine Ark; confirm the model name/list in the Ark console.",
	},
	{
		Name:           "qianfan",
		Label:          "Baidu Qianfan",
		BaseURL:        "https://qianfan.baidubce.com/anthropic/coding",
		ModelsEndpoint: "https://qianfan.baidubce.com/v2/models",
		DefaultModel:   "qianfan-code-latest",
		Note:           "Coding-plan endpoint with a fixed code model; confirm the model list / general model in the Qianfan docs.",
	},
	{
		Name:           "bailing",
		Label:          "Ant Ling (Bailing)",
		BaseURL:        "https://api.tbox.cn/api/anthropic",
		ModelsEndpoint: "https://api.tbox.cn/v1/models",
		DefaultModel:   "Ling-2.5-1T",
		Note:           "models_endpoint is inferred — the probe on add verifies it.",
	},
}
