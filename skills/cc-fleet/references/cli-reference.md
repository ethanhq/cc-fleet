# cc-fleet command reference

Two layers — **user commands** (humans run interactively, pretty output by default) and **Claude-layer commands** (you run via Bash with `--json`, machine-readable envelopes).

## Contents
- User layer (don't run these for the user — they involve credentials)
- Claude layer (you run with `--json`)
- Spawn flags (full set) + permission inheritance
- Spawn JSON envelopes (success / failure)
- How vendor teammates differ from native `Agent`

---

## User layer — for the human, not you

```
cc-fleet init                            First-time setup: create dirs, run doctor,
                                         prompt to add first vendor.
cc-fleet add <vendor> [flags]            Add a vendor (interactive prompts when
                                         flags omitted on a tty).
cc-fleet edit <vendor> [flags]           Modify one or more vendor fields. Incl.
                                         --api-key-stdin / --api-key-file (rotate
                                         single key, file backend; never plain
                                         --api-key — argv leaks to history) and
                                         --key-rotation off|round_robin|random.
cc-fleet remove <vendor>                 Delete vendor + derived files (incl. multi-
                                         key store + rotation counter).
cc-fleet list                            Pretty table of configured vendors.
cc-fleet doctor [--fix]                  Run the health checks (Core + live-teammate
                                         Optional); --fix auto-repairs the safe ones.
cc-fleet repair                          Rebuild derived files from vendors.toml.
cc-fleet uninstall [--wipe-secrets]      Remove config/profiles/models cache. Secrets are
                                         PRESERVED by default; --wipe-secrets also removes
                                         them. Keeps the skill dir (owned by the plugin /
                                         make install-skill). (Removing the BINARY + ccf
                                         alias is `make uninstall`.)
cc-fleet run <vendor>                    Launch an INTERACTIVE claude REPL on the vendor in
                                         the foreground (execs into claude, takes over the
                                         terminal). Flags: --model, --permission-mode <m> |
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
cc-fleet codex-proxy stop                started lazily by spawn / subagent / run and
                                         self-exits when no codex worker remains).
```

`ccf` is a short alias (symlink) for `cc-fleet` — every command works as `ccf …` too. (Install creates it; `make uninstall` removes it. The apiKeyHelper a spawn writes always points at the real `cc-fleet` path regardless.)

**Multi-key + per-worker rotation:** a file-backend vendor can hold several API keys (managed in the interactive TUI: edit a vendor → "Manage API keys →" → add/edit/delete/enable-disable, keys shown masked `sk-…238`). With `--key-rotation round_robin` (or `random`) and ≥2 enabled keys, each spawned worker / subagent draws the next key via `keyget` — granularity is **per-worker** (Claude caches apiKeyHelper per process), so a fan-out of N workers spreads across the enabled keys to share vendor quota / rate limits. Default `off` = always the first enabled key. Disabled keys are never selected.

**Tell the user to run `init` / `add` / `edit` / `remove` / `uninstall` themselves** — you do not run them on their behalf (they involve credentials). Same for **`run`** — it's interactive and execs into `claude`, so it would block / replace you; the human runs it.

---

## Claude layer — you run these with --json

```
cc-fleet spawn <vendor> --as <name> --team <team> [--model <m>] --json
                                         Spawn a vendor teammate into a tmux pane.
                                         Outside tmux ($TMUX empty) it auto-builds an
                                         out-of-tmux swarm session; --json carries
                                         tmux_socket + attach_command (also on stderr).

cc-fleet subagent <vendor> --model <m> --prompt "<task>" [--lead-session-id <id>] --json
                                         One-shot headless vendor subagent;
                                         synchronous result on stdout. No pane,
                                         no team. Parent Claude session auto-detected
                                         when possible; --lead-session-id overrides.
                                         (Full manual: references/subagent.md.)

cc-fleet subagent-status <job_id> --json Poll a --background subagent job
                                         (running | done | failed).
cc-fleet subagent-gc --json              Remove finished background job files (default:
                                         older than 24h). --session <id> clears only that
                                         lead session's finished jobs/runs now (excludes
                                         pinned); prefer it over a blanket clear-all.

cc-fleet teardown <team-or-pane> --json  Clean up. Arg starting with "%" is a pane id;
                                         otherwise a team.

cc-fleet hide <target> --json            Hide a teammate's pane (move to the detached
cc-fleet show <target> --json            claude-hidden session) / restore it — process
                                         keeps running. IN-TMUX teammates only; a swarm
                                         teammate returns SWARM_UNSUPPORTED. target =
                                         %pane | team/member | name@team | team.

cc-fleet ps --json [--check]             List live cc-fleet teammates across all teams.
                                         Empty → ok:true with []. --check adds per-pane
                                         health (status / error_class), redacted.

cc-fleet list --json                     Configured vendors + enabled flag + cache
                                         freshness. Use to pick a vendor.
cc-fleet models <vendor> --json          Cached model list for vendor. Use to pick
                                         --model. Empty → run refresh.
cc-fleet refresh <vendor> --json         Re-query vendor's models endpoint. Updates cache.

cc-fleet refresh-fingerprint --probe-team <team> --json
                                         Snapshot Claude Code's spawn template from a
                                         live probe teammate (Linux: /proc; macOS: ps).
                                         Used inside the self-heal flow only
                                         (references/troubleshooting.md).
```

---

## Spawn flags (full set)

```
cc-fleet spawn <vendor>
  --as <name>                            Teammate name. Required.
  --team <team>                          Target team. Required (or use --auto-team).
  --model <model-id>                     Vendor model id. Default: vendor's default_model.
  --color <color>                        Pane color tag. Default: auto-pick.
  --target <tmux-target>                 tmux session/window/pane.
                                         Default: largest attached session, right split.
  --probe / --no-probe                   Probe vendor reachability (3s). Default: --probe.
  --auto-team / --no-auto-team           Create the team if it doesn't exist. Default: on.
  --lead-session-id <uuid>               Override parent session UUID. Default: team config.
  --permission-mode <mode>               Override inherited permission mode.
                                         <default|acceptEdits|plan|auto|bypassPermissions>.
  --dangerously-skip-permissions         Equivalent to --permission-mode bypassPermissions.
  --json                                 Machine-readable envelope. Always use this.
```

**Permission mode (best-effort startup-intent inheritance).** By default a vendor teammate inherits the permission mode the **lead session was started with** (e.g. the lead launched with `--dangerously-skip-permissions` or `--permission-mode acceptEdits`), detected from the lead process at spawn time; a lead on `default`/`plan` passes nothing down. Pass `--permission-mode <mode>` or `--dangerously-skip-permissions` to override per spawn (highest precedence; the two override flags are mutually exclusive). The `--json` envelope reports `permission_inheritance`: `"manual"` (you overrode), `"lead-flag"` (took the lead's explicit startup flag), `"lead-default"` (lead had none → none applied), or `"frozen-template"` (couldn't detect the lead → fell back to the bundled recipe's flags). **Limitation:** a permission-mode switch made at **runtime** inside the lead session (after startup) is NOT propagated — only the startup intent is captured.

## Spawn JSON envelope (success)
```json
{
  "ok": true,
  "agent_id": "worker-1@refactor-api",
  "name": "worker-1",
  "team": "refactor-api",
  "pane_id": "%42",
  "tmux_session": "1",
  "model": "deepseek-reasoner",
  "base_url": "https://api.deepseek.com/anthropic",
  "color": "cyan",
  "spawn_time": "2026-05-24T05:34:12Z"
}
```
(Out-of-tmux swarm spawns also carry `tmux_socket` + `attach_command`.)

## Spawn JSON envelope (failure)
```json
{
  "ok": false,
  "error_code": "VENDOR_UNREACHABLE",
  "error_msg": "Could not reach api.deepseek.com (timeout 3s)",
  "vendor": "deepseek",
  "suggestion": "Run cc-fleet doctor"
}
```
Dispatch on `error_code` (see `references/troubleshooting.md`), never parse `error_msg`.

---

## How vendor teammates differ from native `Agent`

| | Native `Agent({model: 'sonnet'})` | cc-fleet vendor teammate |
|---|---|---|
| LLM backend | Anthropic | Any Anthropic-compatible vendor (DeepSeek, GLM, …) |
| Billing | Main session's own quota (OAuth or API key) | Vendor metered API |
| Lifecycle | One-shot, exits when done | Long-lived in a tmux pane, multi-turn |
| Tool stack | Full Claude Code | Full Claude Code (same harness) |
| Rate limit | Shared with main session | Independent (vendor's quota) |
| Privacy | Anthropic | Vendor (e.g. Chinese data → Chinese vendor) |
| Spawned via | Native `Agent` tool | `cc-fleet spawn` (this skill) |
| `--settings` injection | Not possible | Yes (vendor profile JSON) |
| Vendor model id | Not possible (enum-locked) | Yes (`--model <vendor-id>`) |

If you only need Anthropic and the work fits the main session, native `Agent` is simpler. cc-fleet is for the cases where the four right-column properties matter.
