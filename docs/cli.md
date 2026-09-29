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
| `export` | Write a keyless TOML bundle of the provider roster for another machine (`--out`, `--provider`, `--json`). |
| `import <bundle>` | Apply a provider bundle — validates it whole, then merges (`--force` overwrites + backs up; `--json`). |
| `models <provider>` | Show a provider's configured model roster (default / strong / fast slots). |
| `refresh <provider>` | Re-query a provider's `/v1/models` and update the cache. |
| `keyget <provider>` | Print a provider API key once — Claude's `apiKeyHelper` calls this. |
| `codex add` / `login` / `logout` / `status` | Register a ChatGPT-subscription provider + manage cc-fleet's own codex login (`--credential` for multiple). |
| `codex-proxy status` / `stop` | Inspect / stop the local conversion daemon (started lazily; `serve` is internal). |

**Execution lanes**

| Command | What it does |
|---------|--------------|
| `teammate setup` | One-time: enable provider teammates on Claude Code's native agent teams (launcher shim, `ccf-*` agent definitions, two settings keys). |
| `teammate check [provider]` | Check that a provider teammate can start from this Claude Code session and print its `agent_type` (provider optional → default). |
| `subagent [provider]` | Run a one-shot headless provider subagent (provider optional → default). |
| `subagent-status <job>` | Check a background job; `--wait` blocks until it settles. |
| `subagent-gc` | Prune finished subagent jobs (`--older-than`, `--session`). |
| `run [provider]` | Launch an interactive provider-backed `claude` session you drive yourself. |
| `workflow …` | The JS orchestration group — see [Workflows](#workflows). |

**Fleet ops & maintenance**

| Command | What it does |
|---------|--------------|
| `ps` | List the provider teammates Claude Code started, with a `state` per row (`--json`, `--check` for pane health). |
| `watch` | Stream the whole fleet — teammates, jobs, runs — as text until interrupted. |
| `hide` / `show <%N\|name@team>` | Park / restore a provider teammate's tmux pane without killing it. |
| `teardown <%N\|name@team\|team>` | Kill provider teammates after re-verifying each one's identity; never edits Claude Code's team files. |
| `doctor` | Run the health checks — Core vs Optional; only a Core failure fails the run. |
| `repair` | Rewrite every provider's profile JSON from `providers.toml`; re-pin the teammate launcher shim. |
| `update` | Self-update the binary via its install channel + refresh the plugin (`update rollback` undoes it). |
| `uninstall` | Reset cc-fleet state (re-installable); `--all` also removes the skills, plugin, and binary — method-aware. |

**Removed commands.** `spawn` and `refresh-fingerprint` are gone: Claude Code now starts teammates itself and builds their command line, so there is no spawn recipe to capture. Both remain as hidden stubs that print `{"ok":false,"error_code":"COMMAND_REMOVED",…}` and exit 1, whatever the arguments.

| Removed | Use instead |
|---------|-------------|
| `cc-fleet spawn <p> --as <name> --team <t>` | `cc-fleet teammate check <p> --json`, then in the lead `Agent({name, subagent_type: <agent_type>, prompt})` (see [Teammates](#teammates--native-agent-teams-provider-backed)). The spawn flags (`--as`, `--team`, `--model`, `--color`, `--probe`, `--verify`, `--permission-mode`, …) have no replacement: Claude Code names, colors and places the teammate and passes the lead's permission mode; pick a model with the slot (`--slot strong` → `ccf-<p>.strong`). |
| `cc-fleet refresh-fingerprint [--probe-team <t>]` | Nothing to refresh. If something fails, run `cc-fleet doctor`. |
| native `TeamCreate` / `TeamDelete` | Nothing: every terminal `claude` session already has its own team, and Claude Code removes it when the session exits. |

## Registering a provider from the CLI

The TUI is the easy path, but Anthropic-protocol registration also scripts. Pipe the key on stdin so it never lands in argv or shell history:

```bash
printf '%s' "$DEEPSEEK_API_KEY" | cc-fleet add deepseek \
  --base-url https://api.deepseek.com/anthropic \
  --models-endpoint https://api.deepseek.com/v1/models \
  --default-model deepseek-v4-flash \
  --secret-backend file --secret-ref deepseek.key --api-key-stdin
```

`add` probes the models endpoint synchronously (up to 10s) and only persists on success. Optional model-roster flags: `--strong-model` / `--fast-model` (tier slots), `--effort low|medium|high|xhigh|max` (reasoning effort), `--default-permission` (the default permission mode for `cc-fleet run` sessions). `edit` patches any of these later, plus `--key-rotation` and `--enable`/`--disable`.

Once provider teammates are set up, `add` / `edit` / `remove` also resync the `ccf-*` agent definitions. If that sync fails, or a `ccf-*.md` without the cc-fleet marker is in the way (it is never overwritten), the command itself still succeeds and `--json` carries a `teammate_sync_error` string; `cc-fleet repair` retries the sync and prints a warning for such a file (its `--json` lists it in `agent_defs.conflicts`).

**OpenAI-protocol and codex providers register through the TUI** (the add form's OpenAI and CLI-auth groups) — `cc-fleet add` has no protocol flag. See [Codex](#codex--reuse-a-chatgpt-subscription-as-a-provider) for the codex CLI path.

## Default provider & model tiers

`cc-fleet default <provider>` sets the fleet-wide default; every provider-less `teammate check` / `subagent` / `run` / workflow leaf resolves to it (`default` alone shows it, `--unset` clears it). The id `claude` is reserved for the native leaf and can never be the default — `cc-fleet default claude` refuses with `PROVIDER_NAME_INVALID`. A provider's roster gives Claude stable handles instead of hardcoded IDs:

- `--model strong` / `--model fast` / `--model default` resolve through the roster.
- Each slot can carry a 1M-context marker (`[1m]`) and the provider an effort level — both set in the TUI form or via `add`/`edit` flags.

## Export / import the provider roster

Carry your provider roster between machines as a keyless, versioned TOML bundle — review it, diff it, or check it into a private dotfiles repo. The bundle never contains a key.

```bash
cc-fleet export --out fleet-providers.toml          # all providers
cc-fleet export --provider deepseek,glm > roster.toml
cc-fleet import fleet-providers.toml                # apply on the new machine
```

What travels: each provider's config (protocol, base URL, for OpenAI-protocol providers the upstream URL, models endpoint, model roster, effort, default permission, key rotation, enabled, secret backend + reference) and the global default. What doesn't: the **key itself** (file-backend keys stay on the source; `pass`/`1password`/`vault`/`keyring` rows carry only their reference), and **codex** providers (their login is machine-local — omitted on export, skipped on import).

`import` validates the whole bundle before writing anything, so a bad bundle leaves the roster untouched. A provider that collides with an existing one is skipped unless `--force`, which backs up `providers.toml` first and replaces it in a single atomic write. A daemon-backed (OpenAI-protocol) provider's loopback `base_url` is re-derived on the target — its `upstream_url` is what travels. Import writes config only and never contacts a provider; profiles are rebuilt afterward (a derived cache — re-run `repair` if that step reports a failure).

> Only import a bundle you trust: its base URLs, models endpoints, and secret references drive later local secret-manager reads and key-bearing requests.

After import, finish the setup:

```bash
printf '%s' "$KEY" | cc-fleet edit deepseek --api-key-stdin   # re-enter file-backend keys
cc-fleet codex login                                          # for any codex providers
cc-fleet doctor
```

The `bundle_version` in the file is the bundle format version — deliberately separate from the `version` (schema) of `providers.toml`.

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

**The reserved `claude` leaf.** `cc-fleet subagent claude` (and a workflow leaf with `provider: "claude"`) runs the official `claude` CLI on your own Claude Code login — no provider row, no profile, no key material; child env credentials are scrubbed as always, so it needs a real stored login. Explicit-only: it never auto-resolves, never shows in `list`, and `cc-fleet add claude` is rejected (`PROVIDER_NAME_INVALID`). `--model` takes a literal id (`fable` / `opus` / `sonnet` / a full id — roster keywords are rejected); omitted means your login's default tier, typically the costliest. It spends your own subscription window — use it for a synthesis node or two, never a wide fan-out.

## Interactive — a provider-backed session you drive

```bash
cc-fleet run deepseek                              # interactive claude on deepseek
cc-fleet run deepseek --model strong
cc-fleet run deepseek --dangerously-skip-permissions
```

`cc-fleet run [provider]` replaces the current process with an interactive `claude` REPL whose backend is the provider — **omit the provider to use the fleet-wide default** (the profile pins the `apiKeyHelper` + base URL; the model is the provider's `default_model` unless `--model` overrides). Unlike teammates and subagents, this is **you** using a provider, not Claude delegating.

- `--permission-mode <mode>` / `--dangerously-skip-permissions` — the session's permission posture (mutually exclusive). `run` execs the binary directly, so a `claude` shell alias that adds such a flag does not carry over — pass it here.
- `--no-probe` — skip the pre-launch endpoint protocol check (before launching, `run` checks how an Anthropic-protocol provider's endpoint answers `POST /v1/messages`; a bare 404 — typically an OpenAI-only endpoint added as Anthropic-protocol — is rejected with a fix hint instead of claude's generic "model may not exist").
- `-- <claude args>` — everything after `--` is forwarded to `claude`.

Requires an interactive terminal. Works on Linux, macOS, and Windows.

## Teammates — native agent teams, provider-backed

A provider teammate is a teammate Claude Code starts itself, with the `Agent` tool, from an agent type cc-fleet installs (`ccf-<provider>`, plus `ccf-<provider>.strong` / `.fast` when those slots hold a different model). Claude Code owns the team, the pane, the inbox, `SendMessage`, permission inheritance and cleanup; cc-fleet only routes the pane's `claude` to the provider.

**Where it works:** a terminal `claude` (Claude Code ≥ 2.1.278) running inside tmux or iTerm2, with `teammateMode` resolving to a pane (tmux, or auto inside tmux/iTerm2). The Claude desktop app, `claude -p` and SDK sessions have no agent team, and in-process teammates cannot be routed — there, use `subagent` / `workflow`. Not available on Windows (`UNSUPPORTED_ON_WINDOWS` from `teammate`, `hide`, `show` and `teardown`).

**One-time setup:**

```bash
cc-fleet teammate setup                          # lists what it would change, changes nothing (BAD_ARGS)
cc-fleet teammate setup --yes                    # apply
cc-fleet teammate setup --yes --teammate-mode tmux   # also set teammateMode=tmux when it is unset or in-process
cc-fleet teammate setup --remove --yes           # undo (the shim file is kept)
```

`setup --yes` writes, in this order: the launcher shim `~/.config/cc-fleet/bin/claude-teammate` (0755), one `~/.claude/agents/ccf-*.md` per enabled provider, and in `~/.claude/settings.json` `env.CLAUDE_CODE_TEAMMATE_COMMAND` (the shim) plus `env.CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS="1"` (only if not already on). `--teammate-mode` defaults to `keep`; `tmux` leaves an existing `auto` / `tmux` / `iterm2` alone. It refuses with `SETUP_CONFLICT` when `CLAUDE_CODE_TEAMMATE_COMMAND` already points at something else (`--force` replaces it) or when a `ccf-*.md` without cc-fleet's marker exists (never overwritten, even with `--force`). Restart `claude` afterwards (`restart_required` in the JSON). `--remove` deletes the launcher setting and cc-fleet's definitions and marks the lane disabled; it keeps the shim (running sessions still point at it) and leaves `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS` and `teammateMode` as they are. `settings.json` is edited in place: key order, symlinks and file mode are preserved.

**Per teammate (the skill does this):**

```bash
cc-fleet teammate check deepseek --slot strong --json   # from the lead's Bash tool
# → {"ok":true,"protocol":1,"agent_type":"ccf-deepseek.strong","team":"session-7c8f769b","backend_hint":"tmux",…}
```

then, in the lead, `Agent({name: "worker-1", subagent_type: "ccf-deepseek.strong", prompt: "…"})`. Always pass `name`; never pass `model`, `isolation` or `cwd` for a `ccf-*` type — the plugin's `PreToolUse(Agent)` hook blocks such calls (and any `ccf-*` call that `check` would refuse). `check` exits 0 when `ok`, 1 otherwise, with a stable `error_code` + `detail`: `TEAMMATE_LANE_UNAVAILABLE` (no lead session, Claude Code too old, no session team — desktop app / `-p` / SDK), `TEAMMATE_SETUP_REQUIRED` (launcher or definition missing or foreign), `LEAD_RESTART_REQUIRED` (setup ran after this `claude` started), `TEAMMATE_MODE_IN_PROCESS`, `CLAUDE_NOT_FOUND`, plus the provider errors shared with `subagent`. `--no-probe` skips the provider reachability probe.

**Inspect, park, stop:**

```bash
cc-fleet ps --json --check                       # rows with state + pane health
cc-fleet hide worker-1@session-7c8f769b          # park the pane in the claude-hidden session
cc-fleet show %42 --socket /private/tmp/tmux-501/default
cc-fleet teardown worker-1@session-7c8f769b --json
cc-fleet teardown session-7c8f769b --json        # every provider teammate of that team
```

- **`ps`** lists only teammates cc-fleet can attribute: a `ccf-*` member of the session team whose `--settings` is that provider's profile, a dead pane showing a launcher failure line, a `ccf-*` teammate that bypassed the launcher, or a 0.3.x teammate (`legacy:true`). Native teammates are never listed, even under a provider lead. Row fields: `agent_id, name, team, pane_id, provider, model, pid, tmux_socket_path, backend (tmux|in-process|unknown), lead_pid, lead_session_id, state, error_code, hidden, legacy`, plus `status / error_class / detail` with `--check`. `state` is `running`, `orphaned` (its lead session is gone), `failed` (the launcher refused it; see `error_code`) or `bypassed` (it runs without the launcher, so on Claude, not the provider — stop it with `TaskStop`). The table has a `STATE` column.
- **Breaking rename:** the row field `tmux_socket` (a `-L` socket name, empty inside tmux) is now `tmux_socket_path`, the absolute socket path, always set. Replace `tmux -L <tmux_socket> …` with `tmux -S <tmux_socket_path> …`, e.g. `tmux -S "$path" capture-pane -p -t %42`.
- **Targets** for `hide`, `show` and `teardown`: a pane id `%N` (add `--socket <tmux_socket_path>` when several tmux servers have that pane — otherwise `AMBIGUOUS_TARGET`) or an agent id `name@team`; `teardown` also takes a whole `team`. The 0.3.x forms `team` and `team/member` return `BAD_ARGS` from `hide` / `show`.
- **`teardown`** re-verifies each teammate (pane, exact argv, process start time) right before killing it, then kills the pane and reaps the process. Output: `{ok, target, killed:[{agent_id, pane_id, tmux_socket_path, pid}], skipped:[{agent_id, pane_id, reason}], error_code, error_msg, suggestion}`; `reason` is `IDENTITY_MISMATCH` (left alone) or `IN_PROCESS` (use `TaskStop` in the lead). A target with nothing left returns `ok:true` with an empty `killed`. It never touches the lead, native teammates, or `~/.claude/teams`. The 0.3.x keys `panes`, `members`, `killed_pids`, `team_removed` and `warnings` are gone.
- **`hide` / `show`** always print one object `{ok, action, agent_id, team, name, pane_id, tmux_socket_path, hidden, error_code, error_msg, suggestion}`. The origin window is stored on the pane (tmux option `@ccf_origin`), not in a file. `show` rejoins the pane without moving focus: the lead keeps the keyboard and the current window does not change. tmux panes only: a teammate on a detached swarm server returns `SWARM_UNSUPPORTED`, one outside tmux `BACKEND_UNSUPPORTED`.
- **Ending a teammate:** ask it to shut down (native `shutdown_request`), or `TaskStop` it from the lead. `teardown` is for orphans (the lead crashed), 0.3.x leftovers, and cases where `TaskStop` is not available. When the lead exits normally Claude Code removes its panes and team directory itself — there is no `TeamDelete` step any more.

## Workflows

`cc-fleet workflow run <script.js>` executes a JS orchestration script in a **detached engine**: `agent()` leaves are provider subagents, `parallel`/`pipeline`/`phase`/`budget` mirror Claude Code's native Workflow tool, and the run survives your session. The full script API is documented in **[Writing workflows](workflows.md)**; the command surface:

```bash
RUN=$(cc-fleet workflow run audit.js)        # detached; prints ONLY the run id
cc-fleet workflow run audit.js --foreground  # inline (debugging)
cc-fleet workflow status "$RUN" --json       # manifest + every leaf (run → phase → agent)
cc-fleet workflow result "$RUN" --label <leaf> --json  # read a finished leaf's ANSWER (status/wait omit answers)
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
- `--resume` and `restart` run the remaining leaves in the directory the run was started from (recorded in the manifest), not the caller's cwd; if that directory is gone the run fails with a clear error.
- `--resume` parses and compiles the new script and checks its `meta` first; a script that fails is refused before anything is written, so the run and its saved script (the one `restart` runs) stay as they were.
- A leaf with `isolation: "worktree"` that leaves changes behind is saved as branch `cc-fleet/wf-<job>-a<attempt>` before its worktree is removed (see [Writing workflows](workflows.md#isolated-worktrees)). After a run is stopped or killed, the next `restart` / `--resume` clean-up or `workflow rm` / `prune` rescues unsaved work to a `cc-fleet/wf-salvage-*` branch; a directory it cannot save is kept with a `.cc-fleet-keep` marker, and `rm` / `prune` / `restart` print one line per kept directory on stderr. cc-fleet never deletes these branches.
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
- `round_robin` — advance a counter on each worker start.
- `random` — pick a random enabled key.

Disabled keys are filtered out before selection. Keys render masked everywhere (`sk-…238`); plaintext only ever reaches `keyget` stdout and the password-echo input.

## Secret backends

`--secret-backend` selects where the key lives: `file` (default, `0600` under `~/.config/cc-fleet/secrets/`), or an external manager referenced by `--secret-ref` — `pass`, `1password`, `vault`, or the OS `keyring`. For non-file backends you provision the secret through that backend's own CLI; cc-fleet only resolves it at `keyget` time.

## Health, repair, update

- `cc-fleet doctor` — the health checks, grouped **Core** (config, binary, claude, profiles, skills…) vs **Optional** (tmux, attached session, and check 8 "teammate lane (optional)": shim, pinned cc-fleet path, agent definitions, `teammateMode`, agent teams on; OK with "not set up" when the lane is off); only a Core *failure* flips the overall result (the skills check warns, never fails). Check 4 finds `claude` on PATH or under `~/.local/share/claude/versions` and notes when it is older than the 2.1.278 provider teammates need. Doctor never repairs anything — failures print fix hints.
- `cc-fleet repair` — rebuild provider profile JSON from `providers.toml`; when the teammate lane is enabled or the shim exists, re-pin the shim to this binary and restore its mode, and resync the `ccf-*` definitions (lane enabled). `--json` adds `shim`, `shim_repinned` and `agent_defs` (`written` / `removed` / `unchanged`). It never edits `settings.json`.
- `cc-fleet update` — method-aware self-update: a tarball install swaps the binary in place (checksum-verified, `.previous` kept for `update rollback`), npm/go installs delegate to their package manager; the plugin is refreshed in the same pass. `--check` only reports; `--binary-only` skips the plugin refresh. Not available on Windows — update via npm or a fresh zip.
- `cc-fleet watch` — a read-only text stream of the whole fleet (teammates + jobs + runs); `--interval`, `--timeout`, `--check`.
- `cc-fleet uninstall` — first undoes the teammate lane like `teammate setup --remove` (the shim path is listed under `kept`: delete it once every `claude` session has restarted), then resets all config and state (file secrets kept unless `--wipe-secrets`); a bare uninstall never touches the skills, plugin, or binary, so you can `init` again. `uninstall --all` is the complete uninstall — skills, plugin, then the binary + `ccf` alias, routed by install method (npm → `npm uninstall -g`; whatever can't be removed from inside the process — and everything on Windows — is printed as manual commands). `--all` wipes secrets unless `--keep-secrets` is passed, asks for confirmation, and requires `--yes` when non-interactive or `--json`.

## Files & locations

| Path | Contents |
|------|----------|
| `~/.config/cc-fleet/providers.toml` | Provider definitions (mode `0600`). |
| `~/.config/cc-fleet/secrets/` | File-backend keys (dir `0700`, keys `0600`). |
| `~/.config/cc-fleet/subagent-jobs/` | Background job metadata + result cache. |
| `~/.config/cc-fleet/subagent-jobs/runs/` | Workflow run manifests, journals, events. |
| `~/.config/cc-fleet/bin/claude-teammate` | Teammate launcher shim (`teammate setup`, `repair`). |
| `~/.claude/profiles/` | Generated per-provider profiles (`--settings` for teammates, subagents, `run`). |
| `~/.claude/agents/ccf-*.md` | Provider teammate agent definitions (managed by cc-fleet; carry a `managed-by: cc-fleet` marker). |
| `~/.claude/settings.json` | cc-fleet sets `env.CLAUDE_CODE_TEAMMATE_COMMAND`, `env.CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS` and, only when asked, `teammateMode` (`teammate setup`). |
| `~/.claude/teams/<team>/` | Native team state — owned by Claude Code; cc-fleet only reads it. |

The `~/.config/cc-fleet` base honors `$XDG_CONFIG_HOME` when set; the `~/.claude` paths honor `$CLAUDE_CONFIG_DIR` (profiles stay under `~/.claude/profiles`).
