![cc-fleet — run any provider LLM as native Claude Code agents: dynamic workflows, teammates, subagents](docs/assets/cc-fleet-banner.png)

<h1 align="center">🚢 cc-fleet</h1>

<p align="center"><strong>🤖 Plug any third-party model into Claude Code's ⚙️ Dynamic Workflows, 👥 Agent Teams, and ⚡ Subagents — from DeepSeek · GLM · Kimi · Qwen … to your Codex subscription, with your main session's auth untouched; no Claude subscription needed to run a full Claude Code on any provider 🚀</strong></p>

<div align="center">

[![Release](https://img.shields.io/github/v/release/ethanhq/cc-fleet?style=for-the-badge&color=2ea043&label=release)](https://github.com/ethanhq/cc-fleet/releases) [![npm](https://img.shields.io/npm/v/@ethanhq/cc-fleet?style=for-the-badge&color=cb3837)](https://www.npmjs.com/package/@ethanhq/cc-fleet) [![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20macOS%20%7C%20Windows-8957e5?style=for-the-badge)](https://github.com/ethanhq/cc-fleet/releases) [![License](https://img.shields.io/badge/license-Apache%202.0-1f6feb?style=for-the-badge)](LICENSE)

**English** · [简体中文](docs/README_zh.md)

</div>

---

Claude Code's multi-agent orchestration — Dynamic Workflows, Agent Teams, Subagents — only runs Anthropic's own models. cc-fleet lets any model with an Anthropic- or OpenAI-compatible API (DeepSeek, GLM, Kimi, Qwen … even your Codex subscription) join as a workflow leaf, a long-lived teammate, or a one-shot subagent — scheduled by your main session, with the same identity and powers as a native Claude agent.

Every third-party worker is a real `claude` process with its LLM backend swapped to the provider, so Claude Code drives it exactly like a native agent. Your main session's own auth (OAuth subscription or API key) is untouched, and provider keys never enter env, argv, or shell history — zero leak risk.

Two steps to start: one-line install, register a provider. Then state your intent in Claude Code with `/workflow`, `/team`, or `/subagent` — or just describe the task in plain language; intent recognition and CLI calls are all Claude's own reasoning and action.

No Claude subscription? `ccf run <provider>` starts an interactive session driven by that provider — the same `claude` you know, just running on the provider's model.

## Quickstart

**0. Install Claude Code first** — cc-fleet drives the official `claude` CLI, so install it if you don't have it yet (skip if `claude` is already on your PATH):

**macOS / Linux:**
```bash
curl -fsSL https://claude.ai/install.sh | bash
```
**Windows (PowerShell):**
```powershell
irm https://claude.ai/install.ps1 | iex
```

### Option 1 — one-line script (recommended)

One command does it all — downloads and verifies the CLI, puts it on your PATH (with a `ccf` alias, so `ccf` launches it from then on), and installs the Claude Code plugin (skill + session hook) via the marketplace. Ready to use right after:

**macOS / Linux:**
```bash
curl -fsSL https://raw.githubusercontent.com/ethanhq/cc-fleet/main/install.sh | sh
```
**Windows (PowerShell):**
```powershell
irm https://raw.githubusercontent.com/ethanhq/cc-fleet/main/install.ps1 | iex
```

Override the installer's defaults by appending flags after `| sh -s --`:

```bash
curl -fsSL https://raw.githubusercontent.com/ethanhq/cc-fleet/main/install.sh | sh -s -- \
  --skill plugin \        # how to add the skill: plugin (default, via marketplace) | global (copy into ~/.claude/skills) | none
  --scope user \          # plugin scope: user (default) | project | local
  --prefix ~/.local/bin \ # where to install the binary (default: ~/.local/bin)
  --version v0.2.1         # pin a specific release (default: latest)
```

On Windows the PowerShell installer takes the same overrides via environment variables:

```powershell
$env:CCF_VERSION = "v0.2.1"; $env:CCF_PREFIX = "$HOMEin"; irm https://raw.githubusercontent.com/ethanhq/cc-fleet/main/install.ps1 | iex
```

### Option 2 — from another source

These channels install the **CLI only** — you then add the plugin by hand. npm / go / Releases work on Linux, macOS, and Windows alike:

| Channel | Command |
|------|------|
| npm | `npm install -g @ethanhq/cc-fleet` |
| go install | `go install github.com/ethanhq/cc-fleet/cmd/cc-fleet@latest` |
| [Releases](https://github.com/ethanhq/cc-fleet/releases) | download the archive for your OS / arch, then `tar -xzf cc-fleet-*.tar.gz && cd cc-fleet-*/ && ./install.sh` (Windows: unzip and run the bundled `install.ps1`) |
| Source *(Linux / macOS)* | `git clone https://github.com/ethanhq/cc-fleet.git && cd cc-fleet && make install` — needs `make`; from source on Windows, use the `go install` row above |

**Add the Claude Code plugin** — this is what teaches Claude *when* to delegate: its three skills teach it to schedule Workflows, Agent Teams, and Subagents in the right situations and pick the right lane. Either way:

- **Inside Claude Code (recommended):** launch `claude` and type the two slash commands (or run `/plugin` for the interactive Marketplaces / Discover panel):
  ```
  /plugin marketplace add ethanhq/cc-fleet
  /plugin install cc-fleet@ethanhq
  ```
- **From the CLI:**
  ```bash
  claude plugin marketplace add ethanhq/cc-fleet
  claude plugin install cc-fleet@ethanhq        # installs at user scope; pass --scope project|local to change
  ```

### Requirements & maintenance

**Requirements:** Agent Team mode depends on tmux, so it **can't run on Windows**; everything else (Subagent / Workflow / run / TUI) behaves identically with no extra dependency. To run an Agent Team in the foreground, install tmux first (macOS: `brew install tmux`; Debian / Ubuntu: `sudo apt install tmux`).

**Common commands:**

```bash
ccf                      # open the interactive TUI
ccf doctor               # health check: dependencies, providers, plugin status
ccf update               # self-update by install channel + refresh the plugin
ccf update rollback      # roll back to the previous version
ccf uninstall            # reset config & state (keeps the binary and secrets)
ccf uninstall --all      # also remove the binary and plugin
```

Once installed, run `ccf` to register a provider and start delegating.

## Highlights
<table>
<tr>
<td width="50%" valign="top" align="center">

**🔌 Provider management — one form in, zero key leakage**

<img src="docs/assets/demo-provider.webp" alt="provider management demo" width="100%" />

15+ built-in presets (13 Anthropic-protocol vendors, OpenAI Responses / Chat, and Groq / Together / vLLM-style compatibles), with your Codex subscription usable as a provider too (device-code login, multiple credentials, the token never leaves the local daemon); models split into default / strong / fast tiers, each taggable with `[1m]` context and an effort level; multiple keys with enable/disable and rotation (off / round_robin / random), and five secret backends to choose from; keys are fetched per call via `apiKeyHelper`, never enter env / argv, and render masked everywhere.

</td>
<td width="50%" valign="top" align="center">

**🖥️ `ccf run` — a full Claude Code without a subscription**

<img src="docs/assets/demo-run.webp" alt="ccf run demo" width="100%" />

exec straight into an interactive `claude` on any provider — all tools, the complete REPL, it *is* `claude`; no Anthropic auth needed and your own login stays untouched; pick a `--model strong/fast` tier, set the same permission modes, forward anything with `-- <claude args>`; no tmux, team, or locks — one command on Linux, macOS, or Windows.

</td>
</tr>
<tr>
<td width="50%" valign="top" align="center">

**⚙️ Dynamic Workflows — orchestration scripts on a native runtime**

<img src="docs/assets/demo-workflow.webp" alt="dynamic workflow demo" width="100%" />

the same JS API as Claude Code's native Workflow tool (`agent()` / `parallel()` / `pipeline()` / `budget` … the only addition is a `provider` option), executed by a detached engine that costs your session zero tokens; a content-hash journal makes `--resume` re-run only what never finished, a single leaf can be held (▶) and restarted in place, and the exit of `workflow wait` *is* the completion push; the reserved `claude` id even runs opus on your own subscription for a synthesis node — cheap fan-out, native-model finish.

</td>
<td width="50%" valign="top" align="center">

**👥 Agent Teams — native tmux pane collaboration**

<img src="docs/assets/demo-team.webp" alt="agent team demo" width="100%" />

a teammate is a real `claude` process launched with the exact recipe Claude Code uses for its own agents, driven by native `TeamCreate` / `SendMessage`; each works live in its own pane beside you, with cross-provider mixed teams, follow-ups across turns, `hide`/`show` parking, and permissions inherited from your lead session; outside tmux the team runs headless in a detached `cc-fleet-swarm-<team>` server — fully drivable as ever, `tmux attach` whenever you want to watch.

</td>
</tr>
<tr>
<td width="50%" valign="top" align="center">

**⚡ Subagents — the lightest delegation**

<img src="docs/assets/demo-subagent.webp" alt="subagent demo" width="100%" />

the default slim profile sends a native-subagent-sized prompt (a fraction of a full session's first request), lock-free so fan-out is truly parallel; a `slim-ro` read-only profile lets a provider analyze your repo with no Edit / Write risk and `--resume` continues a multi-turn session; `--background` with `subagent-status --wait` turns completion into a push; cost is bounded by USD / turn / timeout caps; failures carry a stable `error_code` instead of prose to parse.

</td>
<td width="50%" valign="top" align="center">

**📊 The TUI board — your whole fleet on one screen**

<img src="docs/assets/demo-tui.webp" alt="TUI board demo" width="100%" />

bare `cc-fleet` opens it — a project → session → Workflows / Teams / Subagents master-detail view; a live run → phase → leaf tree with token / cost columns, prompt-and-answer drill-in, and `x` / `r` stop-restart at the run / phase / leaf level; `★` pins survive cleanup, teamhist keeps ended teams on record, `h` / `s` parks panes; the Provider hub carries the grouped add picker, the multi-key manager, and the Codex login wizard; adaptive light / dark palette.

</td>
</tr>
</table>

**🧱 Cross-platform, built to last**: zero cgo, six release-target builds; Windows natively supports Subagent / Workflow / run / TUI (`irm | iex` one-liner); `ccf update` self-updates by install channel, refreshes the plugin, and rolls back in one command; `doctor` runs ten health checks, each with a concrete fix hint; atomic writes and nine flock scopes throughout — a crash never leaves torn state.
## The four lanes

You never pick a lane by hand — the skills route each request. For reference:

| Lane | Shape | Needs |
|------|-------|-------|
| [**Workflow**](#workflow--scripted-orchestration-off-your-context) | a JS script fanning out subagent leaves in a detached engine | — |
| [**Teammate**](#teammate--a-long-lived-worker-on-your-team) | long-lived `claude` in a tmux pane, driven by `SendMessage` | tmux, agent-teams |
| [**Subagent**](#subagent--a-one-shot-headless-call) | one-shot headless call, result on stdout | — |
| [**`cc-fleet run`**](#cc-fleet-run--a-provider-session-you-drive) | an interactive provider-backed `claude` you drive yourself | a terminal |

### Workflow — scripted orchestration off your context

> *"Write a workflow that maps every module with deepseek, then has glm draft an audit checklist per module."*

Multi-phase orchestration lives in a JavaScript file and runs in a **detached cc-fleet engine** — scheduling costs none of your session's tokens. The API mirrors Claude Code's native Workflow tool, plus a `provider` option per agent:

```js
const meta = {
  name: "api audit",
  description: "map endpoints, then draft audit checklists",
  phases: [{ title: "map" }, { title: "build" }, { title: "judge" }],
};

phase("map");
const maps = (await parallel(
  ["auth", "billing", "users"].map((m) =>
    () => agent("List the exported endpoints in module " + m, { provider: "deepseek" }))
)).filter(Boolean);

phase("build");
const checklists = await pipeline(maps,
  (endpoints, _, i) => agent("Draft an audit checklist:\n" + endpoints,
                             { provider: "glm", label: "build:" + i }));

phase("judge");
const verdict = await agent("Pick the strongest checklist and say why:\n" + checklists.join("\n---\n"),
                            { provider: "claude", model: "opus", label: "judge" });
return { checklists, verdict };
```

```bash
RUN=$(cc-fleet workflow run audit.js)            # detached — prints the run id
cc-fleet workflow wait "$RUN" --timeout 10m      # blocks; its exit IS the completion push
cc-fleet workflow stop "$RUN" --leaf build:1     # hold one leaf in place (run keeps going)
cc-fleet workflow restart "$RUN" --leaf build:1  # resume it
cc-fleet workflow run audit.js --resume "$RUN"   # journal replay — finished leaves are cached
```

Runs are journaled by content hash, leaves can be held and restarted individually (live on the TUI board), and budgets cap spend in USD or tokens. A `provider: "claude"` leaf runs on your **own** Claude login — the judge node above bills your subscription, not a provider key.

### Teammate — a long-lived worker on your team

> *"Spawn a glm teammate and a deepseek teammate; have each summarize its model's strengths, then compare the two."*

Claude calls native `TeamCreate`, cc-fleet launches the provider's own `claude` process in a tmux pane, and Claude drives it with native `SendMessage`. The teammate stays alive across turns — keep handing it follow-ups, or run several in parallel.

It needs tmux — start inside one (`tmux new-session -s work`) so panes split alongside your lead — and Claude Code's agent-teams, enabled once in `~/.claude/settings.json` (the other three lanes need neither):

```json
{ "env": { "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1" } }
```

> [!TIP]
> **Not in tmux?** The teammate runs in a detached `cc-fleet-swarm-<team>` server instead — same flow, the pane just isn't on screen. Attach with `tmux -L cc-fleet-swarm-<team> attach`.

### Subagent — a one-shot headless call

> *"Fan out kimi, qwen, and glm subagents over these three files in parallel and collect the results."*

`cc-fleet subagent [provider]` runs the model headless and returns the result synchronously — no pane, no team. Ideal for one-off analysis and batch fan-out. The reserved id `claude` (`cc-fleet subagent claude --model opus …`) runs the native CLI on your own Claude Code login instead of a provider — explicit-only and billed to your subscription, so save it for a synthesis node, not a wide fan-out. Long jobs run with `--background`, and `cc-fleet subagent-status <job> --wait` blocks until the job settles, so its exit wakes the session that launched it — push, not poll.

### `cc-fleet run` — a provider session you drive

> *Not delegation — this one is all you.*

```bash
cc-fleet run deepseek        # an interactive claude, on DeepSeek, billing the provider key
```

Drops you straight into an interactive Claude Code session with the backend swapped — the same `claude` you know, on a cheaper or different-jurisdiction model for day-to-day work. `--model` overrides the default; `--permission-mode` sets the permission posture.

## Providers

The add form ships **13 presets** for Anthropic-protocol endpoints — DeepSeek, Moonshot Kimi, Zhipu GLM (two presets: bigmodel.cn and z.ai international), Qwen (DashScope), MiniMax, Xiaomi MiMo, StepFun, LongCat (Meituan), Volcengine Ark, Doubao Seed, Baidu Qianfan, and Ant Ling (Bailing) — plus *Custom* for any other Anthropic-compatible API.

Beyond those, two more provider classes register through the same TUI form:

- **OpenAI-protocol** — official OpenAI via the Responses or Chat Completions API, or any OpenAI-compatible endpoint (Groq, Together, Fireworks, a local vLLM). A loopback conversion daemon translates Claude's Anthropic calls; the upstream key never reaches the `claude` process.
- **Codex (ChatGPT subscription)** — see below.

Each provider carries a model roster — a default plus optional **strong** and **fast** slots (per-slot 1M-context markers, a reasoning-effort level) — so Claude can ask for "the strong model" without hardcoding IDs. Set a fleet-wide default provider with `cc-fleet default
<provider>` and every provider-less call resolves to it. File-backend providers hold multiple
keys with `off` / `round_robin` / `random` rotation.

### ChatGPT subscription as a provider (codex)

```bash
cc-fleet codex add && cc-fleet codex login   # register + one-time device-code OAuth
```

The subscription becomes a regular provider — teammate, subagent, workflow leaf, or `run`, answering through gpt-5.x. The OAuth token lives only inside the local conversion daemon, and cc-fleet keeps its own login chain, so the codex CLI's auth is untouched. Multiple credentials are supported (`codex login --credential work`). **Unofficial** — reusing a subscription outside the codex CLI may violate OpenAI's terms; `codex login` asks for explicit confirmation. [Details.](docs/cli.md#codex--reuse-a-chatgpt-subscription-as-a-provider)

## Security model

The provider key is treated as radioactive:

- Claude's `apiKeyHelper` calls `cc-fleet keyget <provider>` at request time — the key goes to stdout exactly once and is **never** placed in env, argv, a profile, or shell history.
- Spawned workers start with `env -u ANTHROPIC_API_KEY -u ANTHROPIC_AUTH_TOKEN`, so your main session's credentials cannot leak into a worker — and vice versa.
- Keys live `0600` in `~/.config/cc-fleet/secrets/` (the `file` backend) or in `pass`, 1Password, Vault, or your OS keyring — resolved only at `keyget` time.
- Every UI, log, and error renders keys masked (`sk-…238`). For daemon-backed providers (OpenAI, codex), the upstream bearer never leaves the loopback daemon.

## The Claude Code plugin

The plugin ships the intelligence; the binary does the work. Installed via:

```bash
claude plugin marketplace add ethanhq/cc-fleet
claude plugin install cc-fleet@ethanhq
```

It contains **three skills** (subagent / team / workflow — they teach Claude when to delegate, which lane to pick, and how to recover from provider failures) and a **SessionStart hook** (one quiet line if the binary is missing, never blocks). The binary always installs separately: a plugin can't ship a native executable, and its cache path is too ephemeral to pin the `apiKeyHelper` to.

## The board

`cc-fleet` with no arguments opens the TUI: a provider hub (add/edit, key manager, codex login) and an **Agents Board** — projects → sessions → teams, subagent jobs, and workflow runs, with health, spend, prompt/answer drill-in, and per-leaf hold/restart. Pin records (`p`) survive cleanup; hide (`h`) / show (`s`) park a teammate pane without killing it.

## Documentation

- **[CLI reference & advanced usage](docs/cli.md)** — every command, flag, and envelope.
- **[Writing workflows](docs/workflows.md)** — the JS scripting API for the workflow lane.
- **[Architecture](docs/architecture.md)** — how spawning, key safety, the conversion daemon, and the workflow engine actually work.
- `cc-fleet <cmd> --help` — always authoritative.

## Contributing

PRs are very welcome — bug fixes, new provider presets, docs, tests, features. Please read the **[contribution guide](.github/CONTRIBUTING.md)** first; a few house rules:

- **UI changes and bug fixes need a screenshot or GIF** in the PR.
- **AI-*assisted*** commits credit the tool with a `Co-Authored-By` trailer.
- **Fully AI-*authored*** PRs add an autonomous-PR marker at the bottom of the PR body.

## License

[Apache-2.0](LICENSE).
