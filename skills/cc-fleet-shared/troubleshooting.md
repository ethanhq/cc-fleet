# Troubleshooting — teammate check, guard and launcher failures

Read this when `cc-fleet teammate check --json` returns `ok:false`, when an `Agent` call with a `ccf-*` type is denied with a reason starting `cc-fleet:`, or when `cc-fleet ps --json` shows a teammate in state `failed`. (Subagent failure codes live in `the /cc-fleet:subagent skill`; hide/show codes in `the /cc-fleet:team skill`.)

## Contents
- The failure envelope (check, guard, pane line)
- `error_code` dispatch table
- `detail` dispatch table
- General rules

---

## Failure handling

`cc-fleet teammate check --json` always emits exactly one JSON envelope on stdout (exit 1 on failure):
```json
{"ok":false,"protocol":1,"warnings":[],"error_code":"<CODE>","detail":"<detail>","error_msg":"<what happened>","suggestion":"<next step>"}
```
Dispatch on `error_code`, then on `detail` — do **not** parse `error_msg` prose. `detail` is a stable word; `mode_in_process`, `auto_fallback_unguarded`, `mode_unknown` and `agent_def_shadowed` carry `:<value>` after it.

The same codes reach you two other ways:
- **The PreToolUse guard** denies an `Agent` call with a `ccf-*` type with the reason `cc-fleet: <CODE>(<detail>): <msg> — <suggestion>`. Dispatch it exactly like a check failure. A block reading `cc-fleet: teammate guard unavailable (exit <n>)` means cc-fleet is broken or older than the plugin — tell the user to update both (`cc-fleet update`; `/plugin update`) and use /cc-fleet:subagent meanwhile.
- **The launcher** refuses a teammate it cannot route and leaves one line in its (dead) pane: `cc-fleet teammate: <CODE>: <agent_id>: <msg> — <suggestion>`. `cc-fleet ps --json` then lists the teammate as `state:"failed"` with that `error_code` (and `error_class:"launch_failed"` under `--check`). TaskStop it (or `cc-fleet teardown <agent_id> --json`), fix the cause per the tables below, re-run `teammate check`, and start a fresh teammate.

`ok:true` may carry `warnings` — proceed, and mention them only if something later misbehaves: `launcher_binary_differs` (the shim pins another cc-fleet binary; `cc-fleet repair` re-pins it), `auto_may_fallback` (`teammateMode` auto: a pane failure could fall back to in-process — watch `ps --check` for `bypassed`), `provider_probe_warn` (the models endpoint answered with an HTTP error; the provider is reachable).

| `error_code` | What it means | What you do |
|---|---|---|
| `TEAMMATE_LANE_UNAVAILABLE` | This session cannot host a provider teammate at all (see `detail`). | **Degrade**: run the work through /cc-fleet:subagent or /cc-fleet:workflow; tell the user why in one line. |
| `TEAMMATE_MODE_IN_PROCESS` | Teammates in this session run in-process, so a `ccf-*` teammate would silently run on Claude. | **Degrade** now; offer the user the fix in the `detail` table (it needs a claude restart). |
| `TEAMMATE_SETUP_REQUIRED` | The one-time setup is missing or damaged (see `detail`). | **Setup / repair** per `detail`, then re-run check. |
| `LEAD_RESTART_REQUIRED` | The setup changed after this claude session started. | **Restart**: ask the user to exit and relaunch claude inside tmux; use /cc-fleet:subagent meanwhile. |
| `BAD_AGENT_CALL` | (guard) The `Agent` call itself is wrong for a teammate (see `detail`). | Fix the call; never retry it as is. |
| `CLAUDE_NOT_FOUND` | No claude binary found for this session's version. | Tell the user to install / fix Claude Code or PATH; `cc-fleet doctor` confirms. |
| `PROFILE_WRITE_FAILED` | Could not write `~/.claude/profiles/<provider>.json`. | Tell the user to check that directory's permissions, then run `cc-fleet repair`. |
| `PROVIDER_UNREACHABLE` | DNS / connect / timeout to the provider's models endpoint (3s probe failed). | Suggest `cc-fleet doctor`; if urgent, fall back to native `Agent({model: 'sonnet'})` and tell the user the provider is sick. |
| `KEY_INVALID` | Provider returned HTTP 401/403 — key wrong/expired. | Tell the user: re-add via `cc-fleet edit <provider> --api-key-stdin <<<"$NEW_KEY"` (file backend) or rotate in the secret manager and re-run. Don't retry without user action. **Never** put the raw key on the command line. |
| `UNKNOWN_PROVIDER` | The provider name isn't in `providers.toml`. | `cc-fleet list --json` to see configured providers; tell the user to `cc-fleet add <provider>` first. Don't guess. |
| `PROVIDER_DISABLED` | The provider row has `enabled = false`. | Pick a different provider or tell the user to `cc-fleet edit <provider> --enable`. |
| `NO_DEFAULT_PROVIDER` / `DEFAULT_PROVIDER_DISABLED` / `DEFAULT_PROVIDER_UNKNOWN` / `DEFAULT_PROVIDER_RESERVED` / `CONFIG_LOAD_FAILED` | No provider arg and the default can't be used: none configured / disabled / names a removed provider / hand-set to the reserved `claude` / providers.toml failed to load. | Apply the provider ask ladder (name a provider or have the user fix `cc-fleet default` — `RESERVED` → `cc-fleet default --unset`); `CONFIG_LOAD_FAILED` → `cc-fleet doctor`. |
| `CODEX_PROXY_UNAVAILABLE` | The codex / openai-* conversion daemon could not start or its loopback port is held by another process. | Run `cc-fleet codex-proxy status`; tell the user: `cc-fleet codex login` if not logged in; otherwise free the port in the provider's `base_url` (or re-add with `cc-fleet codex add --port <n>`). |
| `CODEX_CLOUDFLARE_BLOCKED` | The ChatGPT backend's edge (Cloudflare) blocked this IP/client — NOT a bad key. Surfaces on subagent/workflow-leaf envelopes; a teammate hitting it mid-task shows via `ps --check` (`cloudflare_blocked`) instead. | Switch network/IP or retry later; rotating credentials won't help. |
| `BAD_ARGS` | Bad arguments: a slot other than `default`/`strong`/`fast`, an illegal provider name, the reserved `claude` (there is no `ccf-claude` type — a native Claude teammate needs no cc-fleet, use a native agent type), a malformed teardown/hide/show target, or `teammate setup` without `--yes`. | Fix the call; don't retry as-is. |
| `UNSUPPORTED_ON_WINDOWS` | Windows has no teammate lane: check, setup, the launcher, teardown, hide and show all refuse. | Run the work through the subagent or workflow lane (both Windows-native, as is `cc-fleet run`). |
| `SETTINGS_UNPARSEABLE` | (setup) `~/.claude/settings.json` is not a JSON object. | Tell the user to fix the file, then rerun `cc-fleet teammate setup`. |
| `SETUP_CONFLICT` | (setup) `launcher_foreign`: `CLAUDE_CODE_TEAMMATE_COMMAND` already points at another launcher. `agent_def_foreign`: a `ccf-*.md` agent file cc-fleet does not manage is in the way. | Tell the user. `launcher_foreign` → `--force` replaces their launcher, so only with their explicit consent, and a claude restart follows (`restart_required` in the JSON); `agent_def_foreign` → they rename or remove that file (`--force` does not touch it), then rerun setup. |
| `COMMAND_REMOVED` | `cc-fleet spawn` or `cc-fleet refresh-fingerprint` was called; both were removed. | `spawn` → `cc-fleet teammate check <provider> --json`, then `Agent({name, subagent_type: <agent_type>, prompt})`. `refresh-fingerprint` → nothing to refresh; `cc-fleet doctor` if something fails. |
| `INTERNAL` | Unexpected local failure (permissions, disk). | Surface `error_msg` + `suggestion` to the user; `cc-fleet doctor`. |

## `detail` dispatch

| `error_code` / `detail` | What it means | What you do |
|---|---|---|
| `TEAMMATE_LANE_UNAVAILABLE` / `no_lead_session` | Not called from a Claude Code session's Bash tool. | Degrade to /cc-fleet:subagent. |
| … / `cc_too_old` | Claude Code is older than 2.1.278 (no native teammate launcher). | Degrade; tell the user `claude update` enables the lane (or cc-fleet 0.3.x keeps the old flow). |
| … / `no_session_team` | The session has no agent team cc-fleet can identify: the desktop app, `claude -p` and SDK sessions never get one; a terminal session has none while agent teams are off, and after `/clear` or resume its team cannot always be told apart from another lead's in the same directory. `error_msg` names the entrypoint, but a terminal lead can inherit the desktop app's, so it does not tell the cases apart. | Degrade. In a terminal session (`$TMUX` set, or iTerm2): `printenv CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS` empty or not `1`/`true`/`yes`/`on` → agent teams are off because `cc-fleet teammate setup` never ran; offer it (the user's consent, /cc-fleet:team), then a restart inside tmux. On → the user starts a new `claude` inside tmux or iTerm2. Desktop app, `-p`, SDK → the lane is unavailable. |
| … / `teammates_disabled_by_cc` | Claude Code turned named teammates off for this account. | Degrade. |
| `TEAMMATE_SETUP_REQUIRED` / `launcher_not_configured` | `cc-fleet teammate setup` never ran. | Explain what setup changes; with the user's agreement run `cc-fleet teammate setup --yes --json` (add `--teammate-mode tmux` if their `teammateMode` is unset or `in-process`); then they restart claude inside tmux. |
| … / `launcher_foreign` | `CLAUDE_CODE_TEAMMATE_COMMAND` points at someone else's launcher. | The user decides: remove that setting and run `cc-fleet teammate setup --yes`, or run `cc-fleet teammate setup --force --yes` (only with their explicit consent — it replaces their launcher). Either way they then restart claude inside tmux (setup reports `restart_required`). A re-check that still returns `launcher_foreign` means this session has not picked up the change — don't rerun setup. Use /cc-fleet:subagent meanwhile. |
| … / `shim_broken` | cc-fleet's launcher shim is missing, not executable, or pins a cc-fleet binary that no longer exists. | `cc-fleet repair`, then re-run check. |
| … / `launcher_target_missing` | (pane line only) The shim could not run cc-fleet — it moved or was uninstalled — so it refused the provider teammate. | TaskStop the failed teammate; `cc-fleet repair`; re-run check. |
| … / `agent_def_missing` | This type has no definition. Most often the `Agent` call used a hand-built `ccf-<p>.strong` / `.fast` whose slot has no model of its own: cc-fleet writes no definition for it, and check maps that slot to `ccf-<p>`. Otherwise the definition was deleted, its sync after `cc-fleet add` / `edit` failed, or the lane was removed (`cc-fleet teammate setup --remove`) while this session kept the launcher. | Re-run `cc-fleet teammate check <p> [--slot strong\|fast] --json` and use its `agent_type` verbatim. If check itself reports it, `cc-fleet repair`, then re-run check; still there → the lane is off (repair restores definitions only while it is enabled): degrade, and leave re-enabling (`cc-fleet teammate setup --yes`) to the user. |
| … / `agent_def_foreign` | `~/.claude/agents/<type>.md` exists but cc-fleet does not manage it. | Tell the user to rename or remove it, then rerun `cc-fleet teammate setup`. |
| … / `agent_def_shadowed:<path>` | A project agent definition whose frontmatter `name:` equals this type overrides cc-fleet's and would bypass the launcher — any `.claude/agents/**/*.md` from the lead's directory up to the git root. | Tell the user to remove `<path>` or change the `name:` in its frontmatter; renaming the file alone does not help (the match is by `name`). |
| `LEAD_RESTART_REQUIRED` / `launcher` | The launcher was configured after this claude session started. | Ask the user to restart claude (exit, relaunch inside tmux). |
| … / `teammate_mode` | `teammateMode` may have changed since this claude session started: cc-fleet wrote it, or (with an in-process safety-net blocker set) the settings file that holds it was edited. | Ask the user to restart claude inside tmux. |
| `TEAMMATE_MODE_IN_PROCESS` / `mode_in_process:<source>` | `teammateMode` resolves to in-process (`<source>`: `cli`, `policySettings`, `flagSettings`, `localSettings`, `projectSettings`, `userSettings`, `globalConfig` or `default`). | Degrade now. The fix is the user's and depends on `<source>`: `default` / `globalConfig` / `userSettings` → `cc-fleet teammate setup --teammate-mode tmux --yes` (their consent; it writes only `~/.claude/settings.json`), then a restart inside tmux. `localSettings` / `projectSettings` / `flagSettings` → they change `teammateMode` where it is set (the project's `.claude/settings.local.json` / `.claude/settings.json`, or the lead's `--settings` value); setup cannot override those layers. `cli` / `policySettings` → their launch flag / an admin policy. For any source, relaunching as `claude --teammate-mode tmux` also works. |
| … / `auto_no_pane_terminal` | `teammateMode` is auto but the lead is in neither tmux nor iTerm2, so teammates run in-process. | Degrade; the user restarts claude inside tmux, or starts `claude --teammate-mode tmux`. |
| … / `auto_fallback_unguarded:<blockers>` | `teammateMode` is auto and something disables the in-process safety net (`subagent_model_force`, `available_models`, `fallback_model`, `agent_def_newer_than_lead`). | Degrade; the user relaunches with `claude --teammate-mode tmux`, or changes `teammateMode` from `auto` to `tmux` where it is set and restarts claude — setup's `--teammate-mode tmux` replaces only an unset or `in-process` value in `~/.claude/settings.json`, so it leaves an `auto` there alone. `agent_def_newer_than_lead` alone clears with a claude restart. |
| … / `mode_unknown:<v>` | `teammateMode` has a value cc-fleet does not understand. | Degrade; the user sets it to `tmux` (or, when claude runs in iTerm2, `iterm2`; `auto` inside tmux or iTerm2) and restarts claude. |
| `BAD_AGENT_CALL` / `missing_name` | A `ccf-*` `Agent` call without `name` would run as an in-process subagent on Claude. | Pass `name`, or use /cc-fleet:subagent for one-shot work. |
| … / `model_param` | `model` would override the provider routing. | Drop `model`. Pick the slot with `cc-fleet teammate check <p> --slot strong\|fast --json` and use its `agent_type`; never build the type yourself. |
| … / `isolation` | `isolation` does not start a teammate. | Drop `isolation`. |
| … / `cwd` | `cwd` does not start a teammate. | Drop `cwd`. |
| `TEAMMATE_SETUP_REQUIRED` / `cc_fleet_not_on_path` | (guard block) The hook found no `cc-fleet` on PATH. | Tell the user to install cc-fleet; meanwhile use a native agent type or /cc-fleet:subagent. |

## General rules
- One retry max for `PROVIDER_UNREACHABLE` (after `doctor`). A 429 on check's probe is only the `provider_probe_warn` warning; a teammate rate-limited after it starts shows in `ps --json --check` as `rate_limit` (below).
- For config-level failures (`UNKNOWN_PROVIDER`, `KEY_INVALID`, `PROVIDER_DISABLED`), surface to the user and stop — don't re-run blindly.
- **Degrade, don't loop.** `TEAMMATE_LANE_UNAVAILABLE`, `TEAMMATE_MODE_IN_PROCESS`, `LEAD_RESTART_REQUIRED` and `UNSUPPORTED_ON_WINDOWS` won't change by re-running check in this session — move the work to /cc-fleet:subagent or /cc-fleet:workflow.
- **Settings changes are the user's.** `teammate setup` (and its `--force` / `--teammate-mode tmux`) edits `~/.claude/settings.json` — run it only after the user agrees. `cc-fleet repair` only regenerates cc-fleet's own files (profiles, the shim, the `ccf-*` definitions).
- **Check errors ≠ runtime errors — the dispatch KEYS differ.** Up-front failures carry `error_code` (UPPER_SNAKE, these tables). A teammate that *started* and then wedges on a `429` / balance / `401` mid-task produces **no error envelope and no idle notification** — that case is detected by `cc-fleet ps --json --check`, which reports `error_class` (lower_snake: `insufficient_balance` / `auth` / `rate_limit` / `api_error` / `cloudflare_blocked`, plus `launch_failed` / `launcher_bypassed`) — see "Watching for stuck teammates" in `the /cc-fleet:team skill`; never wait open-endedly.
