# cc-fleet command reference

Two layers — **user commands** (humans run interactively, pretty output by default) and **Claude-layer commands** (you run via Bash with `--json`, machine-readable envelopes).

## Contents
- User layer (don't run these for the user — they involve credentials)
- Claude layer (you run with `--json`)
- teammate check JSON envelopes (success / failure)
- ps rows: `tmux_socket` → `tmux_socket_path`
- How provider teammates differ from native `Agent`

---

## User layer — for the human, not you

```
cc-fleet init                            First-time setup: create dirs, run doctor,
                                         prompt to add first provider.
cc-fleet add <provider> [flags]            Add a provider (interactive prompts when
                                         flags omitted on a tty).
cc-fleet edit <provider> [flags]           Modify one or more provider fields. Incl.
                                         --api-key-stdin / --api-key-file (rotate
                                         single key, file backend; never plain
                                         --api-key — argv leaks to history) and
                                         --key-rotation off|round_robin|random.
cc-fleet remove <provider>                 Delete provider + derived files (incl. multi-
                                         key store + rotation counter).
cc-fleet list                            Pretty table of configured providers (--json adds
                                         default_provider + a per-row default flag).
cc-fleet default [provider]              Show or set the default provider used when a lane
                                         omits one. No arg shows it; <provider> pins it
                                         (refuses to overwrite without --force); --unset
                                         clears it. The model is still per-call from the
                                         provider's roster. --json for the structured view.
cc-fleet doctor                          Run the health checks (Core + Optional: tmux
                                         and the teammate lane); failures print fix hints.
cc-fleet repair                          Rebuild derived files from providers.toml
                                         (profiles; with the teammate lane on, also the
                                         launcher shim and the ccf-* agent definitions).
cc-fleet teammate setup [--teammate-mode tmux|keep] [--force] [--remove] [--yes] [--json]
                                         One-time enablement of provider teammates: writes
                                         the launcher shim, the ccf-* agent definitions and
                                         env.CLAUDE_CODE_TEAMMATE_COMMAND (+ agent teams,
                                         + teammateMode tmux with --teammate-mode tmux when
                                         it is unset or in-process) in ~/.claude/settings.json.
                                         Without --yes it only lists the changes (BAD_ARGS).
                                         --force replaces another launcher; --remove undoes
                                         it (keeps the shim file, agent teams, teammateMode).
                                         Takes effect after claude restarts.
cc-fleet uninstall [--wipe-secrets]      Undo teammate setup, then remove config/profiles/
                                         models cache. Secrets are PRESERVED by default;
                                         --wipe-secrets also removes them. Keeps the skill
                                         dir, plugin, and binary.
cc-fleet uninstall --all [--yes]         COMPLETE uninstall: also removes the skills,
                                         plugin, binary + ccf alias, and (unless
                                         --keep-secrets) secrets. Asks to confirm;
                                         non-interactive and --json callers must pass
                                         --yes. Whatever can't be removed from inside
                                         the process is printed as manual commands
                                         (JSON: "manual").
cc-fleet run [provider]                  Launch an INTERACTIVE claude REPL on the provider in
                                         the foreground (execs into claude, takes over the
                                         terminal). The provider arg is OPTIONAL — omit it to
                                         use the default provider (cc-fleet default). Flags:
                                         --model, --permission-mode <m> |
                                         --dangerously-skip-permissions, -- <claude args>.
                                         HUMAN-ONLY — never run it yourself (not a --json
                                         command; it would block + replace your process).
cc-fleet codex add [--name|--port|--model]
                                         Register the ChatGPT-subscription provider: picks
                                         the conversion daemon's loopback port and scans
                                         ~/.codex/config.toml for the default model.
cc-fleet codex login [--accept-risk]     Device-code OAuth login on cc-fleet's OWN token
                                         chain (~/.codex auth is never read or written).
                                         Shows an account-risk notice first — subscription
                                         reuse outside the codex CLI is unofficial.
cc-fleet codex logout                    Remove cc-fleet's codex login; stops the daemon.
cc-fleet codex status                    Show whether cc-fleet has a codex login.
cc-fleet codex-proxy status              Inspect / stop the local conversion daemon (it is
cc-fleet codex-proxy stop                started lazily by teammate check / a teammate /
                                         subagent / run and self-exits when no codex
                                         worker remains).
```

`ccf` is a short alias (symlink) for `cc-fleet` — every command works as `ccf …` too. (Install creates it; `make uninstall` removes it. The apiKeyHelper in a provider profile always points at the real `cc-fleet` path regardless.)

**Multi-key + per-worker rotation:** a file-backend provider can hold several API keys (managed in the interactive TUI: edit a provider → "Manage API keys →" → add/edit/delete/enable-disable, keys shown masked `sk-…238`). With `--key-rotation round_robin` (or `random`) and ≥2 enabled keys, each teammate / subagent draws the next key via `keyget` — granularity is **per-worker** (Claude caches apiKeyHelper per process), so a fan-out of N workers spreads across the enabled keys to share provider quota / rate limits. Default `off` = always the first enabled key. Disabled keys are never selected.

**Tell the user to run `init` / `add` / `edit` / `remove` / `uninstall` themselves** — you do not run them on their behalf (they involve credentials). Same for **`run`** — it's interactive and execs into `claude`, so it would block / replace you; the human runs it. **`teammate setup`** edits the user's Claude Code settings: run it with `--yes` only after the user agrees to the changes it lists.

---

## Claude layer — you run these with --json

```
cc-fleet teammate check [provider] [--slot default|strong|fast] [--no-probe] --json
                                         Gate + prepare a provider teammate for THIS Claude
                                         Code session (run it from the lead's Bash). ok:true
                                         carries protocol (1), agent_type — the Agent tool's
                                         subagent_type, e.g. ccf-glm.strong — plus team,
                                         model, teammate_mode, backend_hint, warnings[].
                                         Also writes the provider profile and starts a
                                         codex / openai-* proxy; --no-probe skips the 10s
                                         reachability probe. The provider arg is OPTIONAL —
                                         omit it to use the default provider (cc-fleet
                                         default; a provider-less call errors
                                         NO_DEFAULT_PROVIDER / DEFAULT_PROVIDER_DISABLED /
                                         DEFAULT_PROVIDER_UNKNOWN /
                                         DEFAULT_PROVIDER_RESERVED — the last when a hand-set
                                         default_provider = "claude" resolves: the reserved id
                                         is never an auto-default). Failure codes:
                                         cc-fleet-shared/troubleshooting.md.

cc-fleet subagent [provider] --model <m> --prompt "<task>" [--lead-session-id <id>] --json
                                         One-shot headless provider subagent (provider
                                         OPTIONAL — omit for the default provider);
                                         synchronous result on stdout. No pane,
                                         no team. Parent Claude session auto-detected
                                         when possible; --lead-session-id overrides.
                                         The reserved id `claude` runs the user's OWN
                                         Claude Code login (subscription OAuth, no
                                         providers.toml row) — explicit-only, never the
                                         default; spends the lead session's own window.
                                         (Full manual: the /cc-fleet:subagent skill.)

cc-fleet subagent-status <job_id> --json Check a --background subagent job
                                         (running | done | failed). --wait blocks
                                         until it settles (--timeout → exit 124) —
                                         arm it in a backgrounded Bash so the exit
                                         wakes the session (push, not poll).
cc-fleet subagent-gc --json              Remove finished background job files (default:
                                         older than 24h). --session <id> clears only that
                                         lead session's finished jobs/runs now (excludes
                                         pinned); prefer it over a blanket clear-all.

cc-fleet teardown <%N|name@team|team> [--socket <path>] --json
                                         Kill cc-fleet provider teammates after re-verifying
                                         identity (pane, exact argv, process start). %N = a
                                         pane id (add --socket <tmux_socket_path> on
                                         AMBIGUOUS_TARGET); name@team = an agent id; team =
                                         every provider / failed / bypassed teammate of that
                                         team (orphans after a lead crash). Envelope:
                                         {ok, target, killed[{agent_id, pane_id,
                                         tmux_socket_path, pid}], skipped[{agent_id, pane_id,
                                         reason}]}; reason IDENTITY_MISMATCH (no longer that
                                         teammate — nothing killed) or IN_PROCESS (TaskStop
                                         it). Nothing left → ok:true, killed:[]. Never edits
                                         ~/.claude/teams, never touches the lead or native
                                         teammates. For a live teammate prefer a
                                         shutdown_request or TaskStop.

cc-fleet hide <%N|name@team> [--socket <path>] --json
cc-fleet show <%N|name@team> [--socket <path>] --json
                                         Hide a teammate's pane (move to the detached
                                         claude-hidden session) / restore it — process
                                         keeps running; show leaves focus on the lead.
                                         tmux split panes only: a detached
                                         swarm server returns SWARM_UNSUPPORTED, a non-tmux
                                         pane BACKEND_UNSUPPORTED; the old targets (bare
                                         team, team/member) return BAD_ARGS.

cc-fleet ps --json [--check]             List cc-fleet provider teammates (native teammates
                                         never appear). Row: agent_id, name, team, provider,
                                         model, pid, tmux_socket_path, pane_id, backend
                                         (tmux | in-process | unknown), lead_pid,
                                         lead_session_id, state (running | orphaned | failed
                                         | bypassed), error_code (failed only), hidden,
                                         legacy. Empty → ok:true with []. --check adds
                                         per-pane health (status / error_class / detail),
                                         redacted.

cc-fleet watch [--check] [--interval] [--timeout]
                                         Stream a live TEXT snapshot of the whole fleet
                                         (teammates + subagent jobs + workflow runs) until
                                         interrupted. NOT a --json command — a human view;
                                         run it in a backgrounded shell to surface the
                                         fleet in /tasks. For machine reads use ps /
                                         subagent-status / workflow status instead.

cc-fleet list --json                     Configured providers + enabled flag + cache
                                         freshness. Use to pick a provider.
cc-fleet models <provider> --json          Cached model list for provider. Use to pick
                                         --model. Empty → run refresh.
cc-fleet refresh <provider> --json         Re-query provider's models endpoint. Updates cache.
```

**Removed:** `cc-fleet spawn` and `cc-fleet refresh-fingerprint` return `COMMAND_REMOVED`. A provider teammate now starts with the native `Agent` tool after `teammate check` (/cc-fleet:team); Claude Code builds the teammate command itself, so there is no fingerprint to refresh.

---

## teammate check JSON envelope (success)
```json
{
  "ok": true,
  "protocol": 1,
  "provider": "glm",
  "slot": "strong",
  "model": "glm-4.6",
  "agent_type": "ccf-glm.strong",
  "team": "session-7c8f769b",
  "lead_pid": 4242,
  "cc_version": "2.1.281",
  "entrypoint": "cli",
  "teammate_mode": "tmux",
  "teammate_mode_source": "userSettings",
  "backend_hint": "tmux",
  "launcher": "/Users/x/.config/cc-fleet/bin/claude-teammate",
  "warnings": []
}
```
Proceed only when `ok` is true **and** `protocol` is `1`; pass `agent_type` verbatim as `subagent_type`. A slot whose model equals the default comes back as the default type (`ccf-glm`).

## teammate check JSON envelope (failure)
```json
{
  "ok": false,
  "protocol": 1,
  "warnings": [],
  "error_code": "TEAMMATE_MODE_IN_PROCESS",
  "detail": "mode_in_process:default",
  "error_msg": "this session runs teammates in-process (set by default); a provider teammate would silently run on Claude",
  "suggestion": "run `cc-fleet teammate setup --teammate-mode tmux` and restart claude inside tmux (or start `claude --teammate-mode tmux`); use `cc-fleet subagent`/`workflow` for now"
}
```
Dispatch on `error_code`, then `detail` (see `cc-fleet-shared/troubleshooting.md`), never parse `error_msg`.

**Permission mode.** Claude Code hands every teammate the lead's live permission mode, provider teammates included — there is nothing to pass or override.

## ps rows: `tmux_socket` → `tmux_socket_path`

The ps row key `tmux_socket` (0.3.x: a `-L` socket name, empty for in-tmux teammates) is gone. `tmux_socket_path` is the absolute path of the teammate's tmux server socket, for `tmux -S`:
```bash
tmux -S "<tmux_socket_path>" capture-pane -t "<pane_id>" -p | tail -40
tmux -S "<tmux_socket_path>" attach          # a teammate on a detached server
```
Scripts that ran `tmux -L "$tmux_socket" …` must switch to `tmux -S "$tmux_socket_path" …`. Rows with `backend` `in-process` or `unknown` have no tmux pane.

---

## How provider teammates differ from native `Agent`

| | Native `Agent({model: 'sonnet'})` | cc-fleet provider teammate |
|---|---|---|
| LLM backend | Anthropic | Any Anthropic-compatible provider (DeepSeek, GLM, …) |
| Billing | Main session's own quota (OAuth or API key) | Provider metered API |
| Lifecycle | One-shot, exits when done | Long-lived in a tmux (or iTerm2) pane, multi-turn |
| Tool stack | Full Claude Code | Full Claude Code (same harness) |
| Rate limit | Shared with main session | Independent (provider's quota) |
| Privacy | Anthropic | Provider (e.g. Chinese data → Chinese provider) |
| Started via | Native `Agent` tool | Native `Agent` tool with `subagent_type: "ccf-<provider>[.strong\|.fast]"`, after `cc-fleet teammate check` (/cc-fleet:team) |
| `--settings` injection | Not possible | Yes (provider profile JSON, applied by cc-fleet's launcher) |
| Provider model id | Not possible (enum-locked) | Yes (the provider's `default` / `strong` / `fast` slot, picked by the agent type) |

If you only need Anthropic and the work fits the main session, native `Agent` is simpler. cc-fleet is for the cases where the four right-column properties matter.

A third point sits between them: the reserved `claude` leaf (`cc-fleet subagent claude …`, workflow `agent(..., {provider: "claude"})`) — an Anthropic backend like native `Agent`, billed to your OWN subscription (not a metered provider), but run through cc-fleet's subagent / workflow lane (off-context, journaled, board-tracked). Use it for a synthesis / judgement node when you want a cc-fleet leaf on your own login rather than a provider; it spends the lead session's own window, so one or two nodes, never a fan-out.
