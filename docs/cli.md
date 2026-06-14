# CLI reference & advanced usage

The `cc-fleet` binary is the engine under the skills. Most of the time you let Claude Code drive it through plain language, but every command also works directly. Run `cc-fleet <cmd> --help` for the authoritative flag list. `ccf` is an alias for `cc-fleet`. A global `--verbose` flag step-traces any command to stderr (the TUI logs to a `0600` file instead).

## Command overview

**Providers & keys**

| Command | What it does |
|---------|--------------|
| `cc-fleet` | Open the interactive TUI (provider hub + Agents Board). |
| `init` | Create the config tree and optionally add a first provider (runs the health checks). |
| `add <provider>` | Register an Anthropic-protocol provider and probe its `/v1/models` endpoint. |
| `edit <provider>` | Modify fields on an existing provider (no probe). |
| `remove <provider>` | Delete a provider and its profile (`--keep-secret` preserves the key). |
| `list` | List configured providers with status and cache info (`--json` includes the default). |
| `default [provider]` | Show / set / unset the fleet-wide default provider (`--unset`, `--force`). |
| `models <provider>` | Show a provider's configured model roster (default / strong / fast slots). |
| `refresh <provider>` | Re-query a provider's `/v1/models` and update the cache. |
| `keyget <provider>` | Print a provider API key once — Claude's `apiKeyHelper` calls this. |
| `codex add` / `login` / `logout` / `status` | Register a ChatGPT-subscription provider + manage cc-fleet's own codex login (`--credential` for multiple). |
| `codex-proxy status` / `stop` | Inspect / stop the local conversion daemon (started lazily; `serve` is internal). |

**Execution lanes**

| Command | What it does |
|---------|--------------|
| `spawn [provider]` | Spawn a provider teammate as a tmux pane (unix only; provider optional → default). |
| `subagent [provider]` | Run a one-shot headless provider subagent (provider optional → default). |
| `subagent-status <job>` | Check a background job; `--wait` blocks until it settles. |
| `subagent-gc` | Prune finished subagent jobs (`--older-than`, `--session`). |
| `run [provider]` | Launch an interactive provider-backed `claude` session you drive yourself. |
| `workflow …` | The JS orchestration group — see [Workflows](#workflows). |

**Fleet ops & maintenance**

| Command | What it does |
|---------|--------------|
| `ps` | List live cc-fleet teammates (`--json`, `--check` for pane health). |
| `watch` | Stream the whole fleet — teammates, jobs, runs — as text until interrupted. |
| `hide` / `show` | Park / restore a teammate's tmux pane without killing it. |
| `teardown <team\|%pane>` | Kill teammate panes and clean up team state. |
| `doctor` | Run the health checks — Core vs Optional; only a Core failure fails the run. |
| `repair` | Rewrite every provider's profile JSON from `providers.toml`. |
| `refresh-fingerprint` | Re-capture the Claude Code spawn template from a live probe agent. |
| `update` | Self-update the binary via its install channel + refresh the plugin (`update rollback` undoes it). |
| `uninstall` | Reset cc-fleet state (re-installable); `--all` also removes the skills, plugin, and binary — method-aware. |

## Registering a provider from the CLI

The TUI is the easy path, but Anthropic-protocol registration also scripts. Pipe the key on stdin so it never lands in argv or shell history:

```bash
printf '%s' "$DEEPSEEK_API_KEY" | cc-fleet add deepseek \
  --base-url https://api.deepseek.com/anthropic \
  --models-endpoint https://api.deepseek.com/v1/models \
  --default-model deepseek-v4-flash \
  --secret-backend file --secret-ref deepseek.key --api-key-stdin
```

`add` probes the models endpoint synchronously (3s) and only persists on success. Optional model-roster flags: `--strong-model` / `--fast-model` (tier slots), `--effort low|medium|high|xhigh|max` (reasoning effort), `--default-permission` (the default permission mode for `cc-fleet run` sessions). `edit` patches any of these later, plus `--key-rotation` and `--enable`/`--disable`.

**OpenAI-protocol and codex providers register through the TUI** (the add form's OpenAI and CLI-auth groups) — `cc-fleet add` has no protocol flag. See [Codex](#codex--reuse-a-chatgpt-subscription-as-a-provider) for the codex CLI path.

## Default provider & model tiers

`cc-fleet default <provider>` sets the fleet-wide default; every provider-less `spawn` / `subagent` / `run` / workflow leaf resolves to it (`default` alone shows it, `--unset` clears it). The id `claude` is reserved for the native leaf and can never be the default — `cc-fleet default claude` refuses with `DEFAULT_PROVIDER_RESERVED`. A provider's roster gives Claude stable handles instead of hardcoded IDs:

- `--model strong` / `--model fast` / `--model default` resolve through the roster.
- Each slot can carry a 1M-context marker (`[1m]`) and the provider an effort level — both set in the TUI form or via `add`/`edit` flags.

## Subagent — one-shot headless calls

```bash
cc-fleet subagent deepseek --prompt "Summarize this log" --json
```

- `--prompt-file <path>` — for large or sensitive prompts (`-` reads stdin).
- `--background` — run detached and print a job id; `cc-fleet subagent-status <job> --wait --timeout 10m` blocks until the job settles. Exit codes: settled `0`/`1` (per the job's envelope), `3` = the leaf is **held** (operator-parked — resume it, don't wait), `124` = still pending at the deadline (a heartbeat, not a failure), `130` = interrupted.
- `--resume <session_id>` — continue a previous subagent for multi-turn work.
- `--timeout` (default 300s) / `--max-turns` / `--max-budget-usd` — bound runtime and cost.
- `--profile` — `slim` (default) mirrors the native subagent context, a far smaller first request than the full session prompt (tools: Bash, Edit, Glob, Grep, Read, Skill, Write); `slim-ro` is the read-only mirror (Bash, Glob, Grep, Read, Skill); `full` restores the full session prompt — only to compare behavior or diagnose a suspected slim regression.
- `--tools` / `--skills` / `--mcp` — refine a slim run (rejected with `--profile full`). `--tools` replaces the whole set, never appends: `--tools WebSearch` leaves ONLY WebSearch. `--skills` is a boolean (default true; `--skills=false` drops the Skill tool). MCP defaults per profile — `slim` inherits the host MCP config, `slim-ro` runs `--strict-mcp-config`; an explicit `--mcp` overrides.
- `subagent-gc` prunes finished jobs (`--older-than 24h` default; `--session <id>` clears one session's finished jobs, pinned records excluded).

No tmux, no agent-teams — prompt in, result envelope out.

**The reserved `claude` leaf.** `cc-fleet subagent claude` (and a workflow leaf with `provider: "claude"`) runs the official `claude` CLI on your own Claude Code login — no provider row, no profile, no key material; child env credentials are scrubbed as always, so it needs a real stored login. Explicit-only: it never auto-resolves, never shows in `list`, and `cc-fleet add claude` is rejected (`PROVIDER_RESERVED`). `--model` takes a literal id (`opus` / `sonnet` / a full id — roster keywords are rejected); omitted means your login's default tier, typically the costliest. It spends your own subscription window — use it for a synthesis node or two, never a wide fan-out.

## Interactive — a provider-backed session you drive

```bash
cc-fleet run deepseek                              # interactive claude on deepseek
cc-fleet run deepseek --model strong
cc-fleet run deepseek --dangerously-skip-permissions
```

`cc-fleet run [provider]` replaces the current process with an interactive `claude` REPL whose backend is the provider — **omit the provider to use the fleet-wide default** (the profile pins the `apiKeyHelper` + base URL; the model is the provider's `default_model` unless `--model` overrides). Unlike spawn/subagent, this is **you** using a provider, not Claude delegating.

- `--permission-mode <mode>` / `--dangerously-skip-permissions` — the session's permission posture (mutually exclusive). `run` execs the binary directly, so a `claude` shell alias that adds such a flag does not carry over — pass it here.
- `-- <claude args>` — everything after `--` is forwarded to `claude`.

Requires an interactive terminal. Works on Linux, macOS, and Windows.

## Teammates — spawn, inspect, hide, tear down

```bash
cc-fleet spawn deepseek --as worker --team squad --json   # usually Claude does this
cc-fleet ps --json --check                                # list teammates + pane health
cc-fleet hide worker@squad                                # park the pane out of view
cc-fleet show worker@squad                                # bring it back
cc-fleet teardown squad --json                            # reap panes + team state
```

In tmux, panes split alongside your lead. Outside tmux, teammates run in a detached `cc-fleet-swarm-<team>` server (attach with `tmux -L cc-fleet-swarm-<team> attach`). `hide` / `show` are in-tmux only. The teammate lane is unix-only — on Windows these commands refuse (`spawn`/`hide`/`show` with `error_code: UNSUPPORTED_ON_WINDOWS`).

Spawn extras: `--verify`/`--no-verify` (the post-spawn settle check — paid only when the live Claude Code is newer than the spawn recipe), `--probe`/`--no-probe` (the pre-spawn key probe, default on), `--permission-mode` (otherwise the teammate inherits the lead's).

**Cleanup order for a provider team:** `cc-fleet teardown <team>` **first** (it reaps the tmux panes/processes), then native `TeamDelete` (which only removes `~/.claude/teams/<team>/`). Running `TeamDelete` alone leaves orphan provider panes billing the key.

## Workflows

`cc-fleet workflow run <script.js>` executes a JS orchestration script in a **detached engine**: `agent()` leaves are provider subagents, `parallel`/`pipeline`/`phase`/`budget` mirror Claude Code's native Workflow tool, and the run survives your session. The full script API is documented in **[Writing workflows](workflows.md)**; the command surface:

```bash
RUN=$(cc-fleet workflow run audit.js)        # detached; prints ONLY the run id
cc-fleet workflow run audit.js --foreground  # inline (debugging)
cc-fleet workflow status "$RUN" --json       # manifest + every leaf (run → phase → agent)
cc-fleet workflow list --json                # all runs, newest first
cc-fleet workflow watch "$RUN"               # stream events until terminal
cc-fleet workflow wait "$RUN" --timeout 10m  # block silently until the run settles
cc-fleet workflow stop "$RUN"                # reap the whole run
cc-fleet workflow stop "$RUN" --leaf <job|label>     # hold ONE leaf (run keeps going); --phase holds a phase
cc-fleet workflow restart "$RUN" --leaf <job|label>  # resume a held leaf; on a finished run: keyed replay
cc-fleet workflow run audit.js --resume "$RUN"       # journal replay — completed leaves return cached
cc-fleet workflow rm "$RUN" / prune          # delete a run / every engine-less run
```

- **`wait` exit codes:** `0` done/stopped · `1` failed or engine-gone · `3` **parked** (every remaining leaf is held — operator action required) · `124` timeout (a heartbeat snapshot, not a verdict) · `130` interrupted · `2` IO/unknown run. Armed in a backgrounded shell, its exit is a push notification — no polling loop needed. The envelope carries outcome + status counts + spend; per-leaf detail stays in `workflow status`.
- A **held** leaf (`stop --leaf`, or the board's `x`) is parked indefinitely — not an error, not retried; `restart --leaf` re-runs it in place (same job id, attempt +1).
- `run` flags: `--max-concurrency` (default `min(16, cores-2)`), `--budget-usd` / `--budget-tokens` (the engine stops minting leaves at the cap), `--args-json` (the script's `args`), `--no-persist-io` (disable prompt/answer drill-in), `--saved` (run a saved script).
- The journal keys each leaf by content hash (provider + model + prompt + schema + profile shape), so `--resume` re-runs only what changed or never finished; failed leaves are never journaled.
- `workflow saved` lists board-saved scripts (the names `run --saved` accepts); `workflow new <name> --phase <title>…` mints an empty run with an ordered phase plan, for manually grouping `subagent --run-id/--phase` jobs under one board tree.

## Codex — reuse a ChatGPT subscription as a provider

A codex provider drives gpt-5.x through your existing ChatGPT/Codex subscription — as a teammate, subagent, workflow leaf, or `run` session:

```bash
cc-fleet codex add      # register the provider (port + default model auto-picked)
cc-fleet codex login    # one-time device-code OAuth (prints a URL + code)
```

The `claude` process speaks the Anthropic API to a loopback conversion daemon (`codex-proxy`, started lazily, self-exits when idle); the daemon translates to the OpenAI Responses API and calls the ChatGPT backend. The OAuth bearer lives only inside the daemon — `keyget` hands claude a low-value loopback handshake secret, and the token never enters env, argv, or any profile. cc-fleet keeps its **own** token chain (`codex login`), never reading or writing `~/.codex` auth, so the codex CLI's login is unaffected.

Multiple subscriptions coexist: `codex add --name codex-work` registers another provider, and `codex login|logout|status --credential <ref>` manage each credential independently. The same daemon also serves the OpenAI-protocol provider classes (`openai-responses`, `openai-chat`) registered through the TUI — one port per provider, upstream key handled the same way.

> **Unofficial:** reusing a subscription outside the codex CLI may violate OpenAI's terms; the account could be rate-limited or banned. `codex login` asks for explicit confirmation, and quota errors surface with their reset time.

## Multiple keys & rotation

A file-backend provider can hold several API keys (`<provider>.keys.json`, mode `0600`) with per-key enable/disable, managed from the TUI key-manager. `keyget` is the rotation point — strategy is per provider:

- `off` — always the first enabled key.
- `round_robin` — advance a counter on each worker spawn.
- `random` — pick a random enabled key.

Disabled keys are filtered out before selection. Keys render masked everywhere (`sk-…238`); plaintext only ever reaches `keyget` stdout and the password-echo input.

## Secret backends

`--secret-backend` selects where the key lives: `file` (default, `0600` under `~/.config/cc-fleet/secrets/`), or an external manager referenced by `--secret-ref` — `pass`, `1password`, `vault`, or the OS `keyring`. For non-file backends you provision the secret through that backend's own CLI; cc-fleet only resolves it at `keyget` time.

## Health, repair, update

- `cc-fleet doctor` — the health checks, grouped **Core** (config, binary, claude, profiles, skills…) vs **Optional** (the tmux checks); only a Core *failure* flips the overall result (the skills check warns, never fails). Doctor never repairs anything — failures print fix hints.
- `cc-fleet repair` — rebuild provider profile JSON from `providers.toml`.
- `cc-fleet refresh-fingerprint --probe-team <team>` — re-capture Claude Code's spawn template after a CC upgrade changes it (the skill triggers this self-heal automatically).
- `cc-fleet update` — method-aware self-update: a tarball install swaps the binary in place (checksum-verified, `.previous` kept for `update rollback`), npm/go installs delegate to their package manager; the plugin is refreshed in the same pass. `--check` only reports; `--binary-only` skips the plugin refresh. Not available on Windows — update via npm or a fresh zip.
- `cc-fleet watch` — a read-only text stream of the whole fleet (teammates + jobs + runs); `--interval`, `--timeout`, `--check`.
- `cc-fleet uninstall` — reset all config and state (file secrets kept unless `--wipe-secrets`); a bare uninstall never touches the skills, plugin, or binary, so you can `init` again. `uninstall --all` is the complete uninstall — skills, plugin, then the binary + `ccf` alias, routed by install method (npm → `npm uninstall -g`; whatever can't be removed from inside the process — and everything on Windows — is printed as manual commands). `--all` wipes secrets unless `--keep-secrets` is passed, asks for confirmation, and requires `--yes` when non-interactive or `--json`.

## Files & locations

| Path | Contents |
|------|----------|
| `~/.config/cc-fleet/providers.toml` | Provider definitions (mode `0600`). |
| `~/.config/cc-fleet/secrets/` | File-backend keys (dir `0700`, keys `0600`). |
| `~/.config/cc-fleet/subagent-jobs/` | Background job metadata + result cache. |
| `~/.config/cc-fleet/subagent-jobs/runs/` | Workflow run manifests, journals, events. |
| `~/.claude/profiles/` | Generated per-provider spawn profiles. |
| `~/.claude/teams/<team>/` | Native team state (managed by Claude, not cc-fleet). |

The `~/.config/cc-fleet` base honors `$XDG_CONFIG_HOME` when set.
