# CLI reference & advanced usage

The `cc-fleet` binary is the engine under the skill. Most of the time you let Claude Code
drive it through plain language, but every command also works directly. Run `cc-fleet <cmd>
--help` for the authoritative flag list. `ccf` is an alias for `cc-fleet`.

## Command overview

| Command | What it does |
|---------|--------------|
| `cc-fleet` | Open the interactive TUI (Model Providers hub + Agents Board). |
| `init` | Create the config tree and optionally add a first provider (runs the health checks). |
| `add <provider>` | Register a provider and probe its `/v1/models` endpoint. |
| `edit <provider>` | Modify fields on an existing provider. |
| `remove <provider>` | Delete a provider and its profile (optionally its secret). |
| `list` | List configured providers with status and cache info. |
| `models <provider>` | List cached models for a provider. |
| `refresh <provider>` | Re-query a provider's `/v1/models` and update the cache. |
| `keyget <provider>` | Fetch a provider API key — used internally by Claude's `apiKeyHelper`. |
| `spawn [provider]` | Spawn a provider teammate as a tmux pane (provider optional → default). |
| `subagent [provider]` | Run a one-shot headless provider subagent (provider optional → default). |
| `run [provider]` | Launch an interactive provider-backed `claude` session (provider optional → default; foreground). |
| `codex add` / `login` / `logout` / `status` | Register the ChatGPT-subscription provider + manage cc-fleet's own codex login. |
| `codex-proxy status` / `stop` | Inspect / stop the local codex conversion daemon. |
| `ps` | List live cc-fleet teammates (`--json`, `--check` for health). |
| `hide` / `show` | Hide / restore a teammate's tmux pane without killing it. |
| `teardown <team\|%pane>` | Kill teammate panes and clean up team state. |
| `doctor` | Run the health checks (`--fix` attempts safe repairs). |
| `repair` | Rewrite every provider's profile JSON from `providers.toml`. |
| `refresh-fingerprint` | Re-probe the Claude Code spawn template via a live probe agent. |
| `uninstall` | Remove all cc-fleet config + cached state (never touches the binary). |

## Registering a provider from the CLI

The TUI is the easy path, but you can script registration. Pipe the key on stdin so it
never lands in argv or shell history:

```bash
printf '%s' "$DEEPSEEK_API_KEY" | cc-fleet add deepseek \
  --base-url https://api.deepseek.com/anthropic \
  --models-endpoint https://api.deepseek.com/v1/models \
  --default-model deepseek-chat \
  --secret-backend file --secret-ref deepseek.key --api-key-stdin
```

## Subagent — one-shot headless calls

```bash
cc-fleet subagent deepseek --model deepseek-chat --prompt "Summarize this log" --json
```

- `--prompt-file <path>` — for large or sensitive prompts.
- `--background` — run detached; `cc-fleet subagent-status` checks it (`--wait` blocks until it settles).
- `--resume <session_id>` — continue a previous subagent for multi-turn work.
- `--timeout` / `--max-turns` / `--max-budget-usd` — bound runtime and cost.
- `--profile` — `slim` (the default) mirrors the native subagent context, a far smaller
  first request than the full session prompt (tools: Bash, Edit, Glob, Grep, Read, Skill,
  Write); `slim-ro` is the read-only mirror (Bash, Glob, Grep, Read, Skill); `full`
  restores the full session prompt — use it only to compare behavior against a full
  session or to diagnose a suspected slim regression.
- `--tools` / `--skills` / `--mcp` — refine a slim run (rejected with `--profile full`).
  `--tools` replaces the whole set, never appends: any tool beyond the whitelist (e.g.
  WebSearch / WebFetch) must be listed explicitly, and `--tools WebSearch` leaves ONLY
  WebSearch. MCP defaults per profile — `slim` inherits the host MCP config, `slim-ro`
  runs `--strict-mcp-config`; an explicit `--mcp` (either value) overrides.

It needs no tmux and no agent-teams — pure stdout in, result out.

## Interactive — a provider-backed session you drive

```bash
cc-fleet run deepseek                              # interactive claude on deepseek
cc-fleet run deepseek --model deepseek-reasoner
cc-fleet run deepseek --dangerously-skip-permissions
```

`cc-fleet run [provider]` (provider optional → default) replaces the current process with an interactive `claude` REPL whose LLM
backend is the provider (the profile pins the `apiKeyHelper` + base URL; the model is the provider's
`default_model` unless `--model` overrides). Unlike spawn/subagent, this is **you** using a
provider, not Claude delegating. No tmux, no agent-teams — just a terminal.

- `--permission-mode <mode>` / `--dangerously-skip-permissions` — the session's permission posture
  (mutually exclusive). It execs the binary directly, so a `claude` shell alias that adds such a
  flag does not carry over — pass it here.
- `-- <claude args>` — everything after `--` is forwarded to `claude`.

Requires an interactive terminal; macOS / Linux only.

## Teammates — spawn, inspect, hide, tear down

```bash
cc-fleet spawn deepseek --as worker --team squad --json   # usually Claude does this
cc-fleet ps --json --check                                # list teammates + health
cc-fleet hide worker@squad                                # break the pane out of view
cc-fleet show worker@squad                                # bring it back
cc-fleet teardown squad --json                            # reap panes + team state
```

In tmux, panes split alongside your lead. Outside tmux, teammates run in a detached
`cc-fleet-swarm-<team>` server (attach with `tmux -L cc-fleet-swarm-<team> attach`). `hide` /
`show` are in-tmux only.

**Cleanup order for a provider team:** `cc-fleet teardown <team>` **first** (it reaps the tmux
panes/processes), then your native `TeamDelete` (which only removes `~/.claude/teams/<team>/`).
Running `TeamDelete` alone leaves orphan provider panes billing the key.

## Multiple keys & rotation

A file-backend provider can hold several API keys (`<provider>.keys.json`, mode `0600`) with
per-key enable/disable, managed from the TUI key-manager. `keyget` is the rotation point —
strategy is per provider:

- `off` — always the first enabled key.
- `round_robin` — advance a counter on each worker spawn.
- `random` — pick a random enabled key.

Disabled keys are filtered out before selection. Keys are shown masked everywhere
(`sk-…238`); plaintext only ever reaches `keyget` stdout and the password-echo input.

## Secret backends

`--secret-backend` selects where the key lives: `file` (default, `0600` under
`~/.config/cc-fleet/secrets/`), or an external manager referenced by `--secret-ref`
(1Password, Vault, keyring). For non-file backends you provision the secret through that
backend's own CLI; cc-fleet only resolves it at `keyget` time.

## Codex — reuse a ChatGPT subscription as a provider

A `codex` provider drives OpenAI gpt-5.x through your existing ChatGPT/Codex
subscription — as a teammate, subagent, workflow leaf, or `cc-fleet run` session:

```bash
cc-fleet codex add      # register the provider (port + default model auto-picked)
cc-fleet codex login    # one-time device-code OAuth (prints a URL + code)
```

The `claude` process speaks the Anthropic API to a loopback conversion daemon
(`codex-proxy`, started lazily, self-exits when idle with no codex worker left);
the daemon translates to the OpenAI Responses API and calls the ChatGPT backend.
The OAuth bearer lives only inside the daemon — `keyget` hands claude a low-value
loopback handshake secret, and the token never enters env, argv, or any profile.
cc-fleet keeps its **own** token chain (`codex login`), never reading or writing
`~/.codex` auth, so the codex CLI's login is unaffected.

> **Unofficial:** reusing a subscription outside the codex CLI may violate
> OpenAI's terms; the account could be rate-limited or banned. `codex login`
> asks for explicit confirmation, and quota errors surface with their reset time.

## Health & repair

- `cc-fleet doctor` runs the checks; `--fix` attempts safe repairs.
- `cc-fleet repair` rebuilds provider profile JSON from `providers.toml`.
- `cc-fleet refresh-fingerprint` re-captures Claude Code's spawn template if a CC upgrade
  changed it.

## Files & locations

| Path | Contents |
|------|----------|
| `~/.config/cc-fleet/providers.toml` | Provider definitions (mode `0600`). |
| `~/.config/cc-fleet/secrets/` | File-backend keys (dir `0700`, keys `0600`, gitignored). |
| `~/.claude/profiles/` | Generated per-provider spawn profiles. |
| `~/.claude/teams/<team>/` | Native team state (managed by Claude, not cc-fleet). |
