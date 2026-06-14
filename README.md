![cc-fleet — run any provider LLM as native Claude Code agents: dynamic workflows, teammates, subagents](docs/assets/cc-fleet-banner.png)

<h1 align="center">🚢 cc-fleet</h1>

<p align="center"><strong>🤖 Plug any third-party model into Claude Code's ⚙️ Dynamic Workflows, 👥 Agent Teams, and ⚡ Subagents — from DeepSeek · GLM · Kimi · Qwen … to your Codex subscription, with your main session's auth untouched; no Claude subscription needed to run a full Claude Code on any provider 🚀</strong></p>

<div align="center">

[![Release](https://img.shields.io/github/v/release/ethanhq/cc-fleet?style=for-the-badge&color=2ea043&label=release)](https://github.com/ethanhq/cc-fleet/releases) [![npm](https://img.shields.io/npm/v/@ethanhq/cc-fleet?style=for-the-badge&color=cb3837)](https://www.npmjs.com/package/@ethanhq/cc-fleet) [![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20macOS%20%7C%20Windows-8957e5?style=for-the-badge)](https://github.com/ethanhq/cc-fleet/releases) [![License](https://img.shields.io/badge/license-Apache%202.0-1f6feb?style=for-the-badge)](LICENSE)

**English** · [简体中文](docs/README_zh.md)

</div>

---

Claude Code's multi-agent orchestration — Dynamic Workflows, Agent Teams, Subagents — only runs Anthropic's own models. cc-fleet lets any model with an Anthropic- or OpenAI-compatible API, even your Codex subscription, join as a workflow leaf, a long-lived teammate, or a one-shot subagent — scheduled by your main session, **with the same identity and powers as a native Claude agent**.

Every third-party worker is a real `claude` process with its LLM backend swapped to the provider, so Claude Code drives it exactly like a native agent. Your main session's own auth (OAuth subscription or API key) is untouched, and provider keys never enter env, argv, or shell history — **zero leak risk**.

**Two steps to start**: one-line install, register a provider. Then state your intent in Claude Code with `/workflow`, `/team`, or `/subagent` — or just describe the task in plain language; intent recognition and CLI calls are all Claude's own reasoning and action.

No Claude subscription? `ccf run <provider>` starts an interactive session driven by that provider — **the same `claude` you know**, just running on the provider's model.

## Install

**0. Install Claude Code first** — cc-fleet drives the official `claude` CLI, so install it if you don't have it yet (skip if `claude` is already on your PATH):

**macOS / Linux:**
```bash
curl -fsSL https://claude.ai/install.sh | bash
```
**Windows (PowerShell):**
```powershell
irm https://claude.ai/install.ps1 | iex
```

**1. Install cc-fleet with the one-line script (recommended)** — one command does it all: downloads and verifies the CLI, puts it on your PATH (with a `ccf` alias, so `ccf` launches it from then on), and installs the Claude Code plugin (skill + session hook) via the marketplace. Ready to use right after:

**macOS / Linux:**
```bash
curl -fsSL https://raw.githubusercontent.com/ethanhq/cc-fleet/main/install.sh | sh
```
**Windows (PowerShell):**
```powershell
irm https://raw.githubusercontent.com/ethanhq/cc-fleet/main/install.ps1 | iex
```

> Other channels (npm / go install / Releases / source), installer overrides, and requirements & maintenance live in **[Install & maintenance](docs/install.md)**.

**Common commands:**

```bash
ccf                      # open the interactive TUI
ccf doctor               # health check: dependencies, providers, plugin status
ccf update               # self-update by install channel + refresh the plugin
ccf uninstall --all      # remove the binary and plugin too
```

Once installed, run `ccf` to register a provider and start delegating.

## Quickstart
<table>
<tr>
<td width="50%" valign="top">
<div align="center">

**🔌 Provider management — one API key, connected**

<img src="docs/assets/demo-provider.webp" alt="provider management demo" width="100%" />

</div>

1. **`ccf` opens the TUI**; choose Add provider
2. **Pick any Anthropic / OpenAI-compatible vendor**
3. **Enter the API key and default model**; optionally an effort level and a Claude permission mode
4. **Save and go**; add more models and toggle them in the list, `s` sets the default, `d` deletes
5. **(Optional) Add Codex**: reuse an existing OAuth, or log in fresh (cc-fleet holds its own credential)

</td>
<td width="50%" valign="top">
<div align="center">

**🖥️ `ccf run` — run Claude Code on any provider**

<img src="docs/assets/demo-run.webp" alt="ccf run demo" width="100%" />

</div>

1. **`ccf run` launches an interactive `claude` on your default provider**; `ccf run <provider>` picks one
2. **All tools, the full REPL**; no Anthropic subscription, your main session's login untouched
3. **Switch anytime mid-session**: `/model` for the model, `/effort` for thinking effort, `Shift+Tab` for permission mode

</td>
</tr>
<tr>
<td width="50%" valign="top">
<div align="center">

**⚙️ Dynamic Workflows — the same orchestration API as native workflows**

<img src="docs/assets/demo-workflow.webp" alt="dynamic workflow demo" width="100%" />

</div>

1. **`/workflow` to kick it off, or just tell Claude**: "map each module with deepseek, glm drafts an audit checklist per module, gpt synthesizes"
2. **Claude writes the JS script and runs it in the background **— no tokens off your main session
3. **`workflow wait` blocks until it finishes **— event-driven, no polling
4. **The TUI board shows every leaf and phase live, `x` to hold / `r` to rerun a single leaf or a whole phase**

</td>
<td width="50%" valign="top">
<div align="center">

**👥 Agent Teams — native multi-agent collaboration in tmux panes**

<img src="docs/assets/demo-team.webp" alt="agent team demo" width="100%" />

</div>

1. **`/team` to kick it off, or just tell Claude**: "spawn a glm and a deepseek teammate, then compare their strengths"
2. **Each teammate is a real `claude` process working live in a side tmux pane **— mix providers in one team, hand follow-ups across turns
3. **The TUI board shows each teammate's full inbox and status**; `h` hides / `s` shows a pane — split in the foreground or run in the background

</td>
</tr>
<tr>
<td width="50%" valign="top">
<div align="center">

**⚡ Subagents — the lightest one-shot delegation**

<img src="docs/assets/demo-subagent.webp" alt="subagent demo" width="100%" />

</div>

1. **`/subagent` to kick it off, or just tell Claude**: "fan out kimi, qwen, and glm over these three files in parallel"
2. **Claude runs the models and collects results synchronously **— dispatch as many in parallel as you like
3. **`slim-ro` read-only mode**: let a provider analyze your repo safely, without touching code
4. **The TUI board shows each job's prompt, answer, and spend**

</td>
<td width="50%" valign="top">
<div align="center">

**📊 The TUI board — your whole fleet on one screen**

<img src="docs/assets/demo-tui.webp" alt="TUI board demo" width="100%" />

</div>

1. **After `ccf` launches, press `Tab` for the Agents Board **— every Workflow / Team / Subagent laid out by project → session
2. **Open any one for detail**: a Workflow's run → phase → leaf progress tree, a Team's teammate inbox, each drilling into prompt, answer, and spend
3. **Act inside the board**: `x` / `r` to stop or rerun, `p` to pin against cleanup, `c` to clear finished, `d` to delete, `h` / `s` to hide or show a pane
4. **Finished teams stay on record**; the UI follows your system light / dark theme

</td>
</tr>
</table>

## Deep dive

cc-fleet's capabilities fall into two groups:

- **Three delegation lanes** (Workflow / Agent Team / Subagent) — scheduled automatically by Claude: say what you want and the skills pick the lane and the model for you, no manual choice.
- **Provider and `ccf run`** — tools you configure and use directly.

---

### ⚙️ Dynamic Workflows

**Orchestration API**: multi-phase orchestration lives in a JavaScript file, with an API identical to Claude Code's native Workflow tool — `agent()` starts a node, `parallel()` fans out, `pipeline()` chains a flow. The one difference is that `agent()` takes a `provider` option to assign each node's model, so different vendors mix and run in parallel within a single run:

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
const verdict = await agent("Pick the strongest one and say why:\n" + checklists.join("\n---\n"),
                            { provider: "claude", model: "opus", label: "judge" });
return { checklists, verdict };
```

**Running and managing**: once kicked off, the whole run executes in a background engine, managed by a small command set. Runs are journaled by content hash, and budgets cap spend in USD or tokens:

```bash
RUN=$(ccf workflow run audit.js)            # starts in the background, prints the run id
ccf workflow wait "$RUN" --timeout 10m      # blocks until done, event-driven
ccf workflow stop "$RUN" --leaf build:1     # hold a single leaf (run keeps going)
ccf workflow restart "$RUN" --leaf build:1  # resume it
ccf workflow run audit.js --resume "$RUN"   # replay the journal, finished leaves hit cache
```

**Holding and restarting**: `ccf workflow stop --leaf` / `--phase` doesn't fail the run — it just pauses the named node: the run keeps going and other nodes carry on, until you `restart` it to run again. If every node currently running gets paused, the whole run settles into a *parked* state that needs you to step in before it continues.

**Wait without polling**: `ccf workflow wait` blocks until the run ends, then exits — drop it in the background and come back when it exits, no repeated checking. The exit code states the outcome: `0` succeeded, `1` failed, `3` parked and waiting, `124` wait timed out (run still going), `130` interrupted.

**Budget-adaptive**: `budget.spent()` / `budget.remaining()` are readable live inside the script (USD or tokens), so a workflow can decide for itself whether to dispatch another batch.

**Mixing in your own subscription**: a node assigned `provider: "claude"` runs on your **own** Claude login — the `judge` above uses your subscription, not a provider key, which suits a synthesis-and-finish node.

---

### 👥 Agent Teams

**Prerequisites**: Agent Team is the only lane that needs setup up front, and because it relies on tmux it **doesn't support Windows yet**. Two conditions before you use it:

1. **Be inside a tmux session** (`tmux new-session -s work`) so teammate panes can show up alongside you;
2. **Enable Claude Code's agent-teams**: the first time you run `ccf` it detects this isn't on and offers to write it in for you — or add it once yourself to `~/.claude/settings.json`:

```json
{ "env": { "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1" } }
```

**How they collaborate**: each teammate is a real `claude` process; Claude builds the team with native `TeamCreate` and assigns work with native `SendMessage`, and teammates stay alive across turns so you can keep adding tasks. One team can use several providers at once, then have one teammate gather and compare the results.

**Permission inheritance**: each teammate inherits your main session's permission posture (plan / acceptEdits / default). If that can't be detected, it falls back to the safest default and won't open up risky permissions on its own.

**Park and restore**: `ccf hide` tucks a teammate's pane out of the way while the process keeps running — messages and context are never lost — and `ccf show` brings it back. At cleanup, `ccf teardown` thoroughly clears every related process, including ones still running in the background and burning the key after their pane was closed, so no ghost quietly bills you.

**Outside tmux**: the teammate runs in a background `cc-fleet-swarm-<team>` session, exactly the same flow with the pane just not on screen. To look in, attach with `tmux -L cc-fleet-swarm-<team> attach`.

---

### ⚡ Subagents

**Three run modes**: the default **slim** sends the model a trimmed system prompt with a narrowed tool set, so the first request is much smaller than a full session — cheaper and faster on metered providers. `--profile slim-ro` is read-only, with tools cut to inspection only (Bash / Glob / Grep / Read / Skill) and no creating, editing, or deleting files, so a provider can read code and check logs without touching your workspace. Use `--profile full` when you need full-session capability.

**Tool trimming and parallelism**: `--tools` takes a comma-separated list to set exactly which tools are open (a replacement, not an addition to the defaults), `--skills=false` turns off the Skill tool, and `--mcp` controls whether the host MCP config carries over. A subagent builds no team, takes no pane, and holds no locks, so running many against one provider doesn't interfere — ideal for large batches in parallel.

**Background and wake-up**: add `--background` to send a long job to the background, then `ccf subagent-status <job> --wait` blocks until it's done and wakes the session that launched it — no repeated checking. Each job is capped by spend (USD), turns, and timeout, and a failure returns a fixed `error_code` your program can branch on.

**Structured result**: with `--json` you get a result object with fixed fields, easy for scripts to parse — besides the answer text it includes the model id that actually responded, the call's spend, token usage, turns, and session_id.

**Run key nodes on your own subscription**: as with Workflow, setting the provider to the reserved name `claude` (`ccf subagent claude --model opus …`) uses your own Claude login and bills your subscription — good for a synthesis-and-finish node, not large parallel batches.

---

### 🔌 Provider management

**Broad compatibility**: works with any Anthropic- or OpenAI-compatible API endpoint — the former includes DeepSeek, Kimi, GLM, Qwen, and more; the latter Groq, Together, Fireworks, a local vLLM, and OpenAI itself. Common vendors ship as presets — select one and the endpoint and protocol are filled in, no manual entry; for anything not listed, pick *Custom* and enter the address.

**Model tiers**: each provider can carry **default / strong / fast** model slots, each separately taggable with 1M context and a reasoning effort. So Claude just asks for "the strong model" — no hardcoded model IDs. `ccf default <provider>` sets the fleet-wide default provider, and any call that doesn't name one goes to it.

**Multi-key rotation**: one provider can hold several API keys, rotated by `off` / `round_robin` / `random` to spread quota and dodge rate limits.

**API key protection**: a key is fetched only at request time, emitted once, and never written into environment variables, command-line arguments, or shell history; a worker process starts with the main session's credentials cleared, so the two never leak into each other. On disk it's saved `0600`, readable only by you, or handed to `pass`, 1Password, Vault, or your OS keyring; every UI and log shows the key masked (`sk-…238`).

**Codex (ChatGPT subscription)**: one device-code login and your ChatGPT subscription becomes a regular provider — usable across Workflow / Team / Subagent / run.

> [!WARNING]
> **Codex is unofficial.** Reusing a ChatGPT subscription outside the codex CLI may violate OpenAI's terms, and `ccf codex login` asks you to confirm first. The OAuth token lives only inside the local conversion daemon; cc-fleet keeps its own login chain and never touches the codex CLI's auth.

---

### 🖥️ `ccf run`

**Launch**: `ccf run <provider>` opens an interactive `claude` on the named provider; with no provider (`ccf run`) it resolves to the fleet-wide default.

```bash
ccf run deepseek        # an interactive claude, on DeepSeek, billing the provider key
```

**It's just native claude**: `ccf run` adds no extra process layer — it replaces itself with `claude`, cc-fleet steps out entirely, and from then on it's a plain `claude` process whose feel and exit behavior match typing `claude` yourself.

**Credentials isolated automatically**: even if you run it from inside a logged-in Claude Code session, it first clears any leftover Anthropic auth from the environment and uses the chosen provider's auth instead — so billing lands on the provider's key, never your own subscription.

**Flags**:

- `--model strong / fast` — override the default with the provider's strong or fast model
- `--permission-mode` — set the permission posture
- `-- <claude args>` — everything after is passed through verbatim to the underlying `claude`

> [!NOTE]
> This lane is for hands-on interactive use: it needs a real terminal and doesn't support pipes or redirects — for non-interactive, one-shot output use a Subagent instead.

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
