# Provider selection + prompt profiles

Shared cross-lane reference — linked from `/cc-fleet:team`, `/cc-fleet:subagent`, and `/cc-fleet:workflow`. Siblings in this directory: routing.md, cli-reference.md, troubleshooting.md.

## Contents
- Picking a provider + model
- Provider cheat sheet
- Prompt profiles (subagent + workflow leaves)

---

## Picking a provider + model

1. **List configured providers.** `cc-fleet list --json`. Skip any provider with `"enabled": false`.
2. **Filter by need** against the cheat sheet below (capability + cost + language).
3. **Check the provider's model roster.** `cc-fleet models <provider> --json` → the configured `default` / `strong` / `fast` slots. It returns ONLY these 3 configured slots, never the provider's full catalog. If it shows `"stale": true` or lacks what you expect, `cc-fleet refresh <provider> --json`, then re-check.
4. **Pick the model.** Omit `--model` to use the provider's `default`, or pass the keyword `default` | `strong` | `fast` to select a slot. A literal model id also works, but prefer the slots — they are what the user configured.

---

## Provider cheat sheet

Template seeds for the built-in presets (what the TUI add picker prefills). Suggestions only — always confirm current state via `cc-fleet list --json` and `cc-fleet models <provider> --json`.

| Provider | Seeded default model | Notes |
|---|---|---|
| `deepseek` | `deepseek-v4-flash` | Use canonical names; legacy aliases silently fall back to default. |
| `kimi` (Moonshot) | `kimi-latest` | 200k+ context; strong Chinese. |
| `glm` (智谱, bigmodel.cn) | `glm-4.6` | Domain Chinese, industry vertical. |
| `zai` (GLM international, z.ai) | `glm-4.6` | Same models as `glm`; separate site + separate key. |
| `qwen` (DashScope) | `qwen-max` | Endpoint/plan vary by region; consult user docs if `refresh` fails. |
| `minimax` | `MiniMax-M2` | — |
| `xiaomimimo` (Xiaomi MiMo) | `mimo-v2.5-pro` | Models endpoint sits at the host root. |
| `stepfun` (阶跃) | `step-3.5-flash-2603` | Step-plan coding endpoint. |
| `longcat` (Meituan) | `LongCat-Flash-Chat` | — |
| `volcengine` (Ark, ByteDance) | `ark-code-latest` | Coding-plan; endpoint-id scheme — the model list may probe empty. |
| `doubao` (Doubao Seed) | `doubao-seed-2-0-code-preview-latest` | Endpoint-id scheme. |
| `qianfan` (Baidu) | `qianfan-code-latest` | Coding-plan endpoint. |
| `bailing` (Ant Ling) | `Ling-2.5-1T` | — |
| `codex` (ChatGPT subscription) | `gpt-5.5`, `gpt-5.3-codex` | Setup: `cc-fleet codex add` + `cc-fleet codex login` (user-run). Quota = the subscription; a 429 carries its reset time. |

OpenAI-protocol presets also exist — `openai` (Responses API), `openai-chat` (Chat Completions), and any OpenAI-compatible endpoint (Groq / Together / Fireworks / vLLM). They are registered via the TUI add form (not `cc-fleet add`), carry no seeded default model (pick from the probed list), and behave like any other provider once configured.

A provider with no built-in seed works the same way — the user adds it first: `cc-fleet add <provider> --base-url <url> --models-endpoint <url> --default-model <id> --api-key-stdin <<<"$KEY"` (use `--api-key-stdin` or `--api-key-file`; **never** the raw key in argv).

**The reserved id `claude` is not a table row** — it is not a configured provider. In the subagent / workflow lanes only (never spawn/teammates, never `cc-fleet run`), `claude` runs the official `claude` CLI on the user's OWN Claude Code login (subscription OAuth) — no providers.toml row, no profile, no key. It needs a real stored login (file / OS keychain); env-key auth (`ANTHROPIC_API_KEY`) is scrubbed like for every child — an API-key user adds a normal `anthropic` provider instead. It never shows in `cc-fleet list` (selected by the literal id, not discovered), can't be the default, and the name is reserved (`cc-fleet add claude` / the TUI add form reject it). `--model` / `model:` takes a literal id only (`opus` / `sonnet` / a full id; the slots `default`/`strong`/`fast` are rejected — no roster); omitted = claude's login default, typically the costliest tier, so name one. It spends the **lead session's own subscription window** — one or two synthesis / judgement nodes, never a wide fan-out. (A providers.toml row named `claude` from before the reservation still loads and lists, but a subagent / workflow `claude` call fails with `PROVIDER_RESERVED` — rename or remove the row; only spawn/teammates still use it.)

---

## Prompt profiles (subagent + workflow leaves)

One model, two surfaces with identical semantics: flags on `cc-fleet subagent` (`--profile`, `--tools`, `--skills=false`, `--mcp`) and options on a workflow `agent()` leaf (`profile`, `tools`, `skills`, `mcp`).

- **`slim` — the DEFAULT.** Generic-subagent mirror: keeps CLAUDE.md + gitStatus, write-capable. Tools: Bash, Edit, Glob, Grep, Read, Skill, Write.
- **`slim-ro`.** Read-only Explore mirror: no CLAUDE.md, advisory read-only. Tools: Bash, Glob, Grep, Read, Skill.
- **`full`.** Restores the full session prompt. Use it ONLY to compare behavior against a full session or to diagnose a suspected slim regression.

Rule of thumb: the leaf writes files → `slim`; read-only research → `slim-ro`.

`slim` / `slim-ro` replace the full session prompt with the native subagent shape plus a restricted tool whitelist — a far smaller first request, which cache-less providers pay per call. Refinements (slim-only — combined with `full` they are rejected):

- **`--tools` / `tools` REPLACES the whole set, never appends.** `--tools "WebSearch"` gives the subagent ONLY WebSearch. Any tool beyond the default whitelist (e.g. WebSearch / WebFetch) must be passed explicitly.
- **`--skills=false` / `skills: false`** drops the Skill tool + the host skill listing (default keeps both).
- **`--mcp` / `mcp`** defaults per profile: `slim` inherits the host MCP config (native parity); `slim-ro` runs `--strict-mcp-config`. An explicit value (either way) overrides.

The profiles need **claude ≥ 2.1.88**. On an older claude the profile **fails open to `full`** — the subagent envelope carries `slim_downgrade`; a workflow leaf logs a notice.

Weak provider models skip tools on weak-imperative prompts under ANY profile — write prescriptive prompts ("Run `cmd`", "Use the Read tool on X"), not "look at" / "check".
