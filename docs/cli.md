# CLI reference & advanced usage

The `cc-fleet` binary is the engine under the skill. Most of the time you let Claude Code
drive it through plain language, but every command also works directly. Run `cc-fleet <cmd>
--help` for the authoritative flag list. `ccf` is an alias for `cc-fleet`.

## Command overview

| Command | What it does |
|---------|--------------|
| `cc-fleet` | Open the interactive TUI (vendors hub + agent-status board). |
| `init` | Create the config tree and optionally add a first vendor (runs the health checks). |
| `add <vendor>` | Register a vendor and probe its `/v1/models` endpoint. |
| `edit <vendor>` | Modify fields on an existing vendor. |
| `remove <vendor>` | Delete a vendor and its profile (optionally its secret). |
| `list` | List configured vendors with status and cache info. |
| `models <vendor>` | List cached models for a vendor. |
| `refresh <vendor>` | Re-query a vendor's `/v1/models` and update the cache. |
| `keyget <vendor>` | Fetch a vendor API key — used internally by Claude's `apiKeyHelper`. |
| `spawn <vendor>` | Spawn a vendor teammate as a tmux pane (Claude layer). |
| `subagent <vendor>` | Run a one-shot headless vendor subagent. |
| `ps` | List live cc-fleet teammates (`--json`, `--check` for health). |
| `hide` / `show` | Hide / restore a teammate's tmux pane without killing it. |
| `teardown <team\|%pane>` | Kill teammate panes and clean up team state. |
| `doctor` | Run the health checks (`--fix` attempts safe repairs). |
| `repair` | Rewrite every vendor's profile JSON from `vendors.toml`. |
| `refresh-fingerprint` | Re-probe the Claude Code spawn template via a live probe agent. |
| `uninstall` | Remove all cc-fleet config + cached state (never touches the binary). |

## Registering a vendor from the CLI

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
- `--background` — run detached; poll with `cc-fleet subagent-status`.
- `--resume <session_id>` — continue a previous subagent for multi-turn work.
- `--timeout` / `--max-turns` / `--max-budget-usd` — bound runtime and cost.

It needs no tmux and no agent-teams — pure stdout in, result out.

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

**Cleanup order for a vendor team:** `cc-fleet teardown <team>` **first** (it reaps the tmux
panes/processes), then your native `TeamDelete` (which only removes `~/.claude/teams/<team>/`).
Running `TeamDelete` alone leaves orphan vendor panes billing the key.

## Multiple keys & rotation

A file-backend vendor can hold several API keys (`<vendor>.keys.json`, mode `0600`) with
per-key enable/disable, managed from the TUI key-manager. `keyget` is the rotation point —
strategy is per vendor:

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

## Health & repair

- `cc-fleet doctor` runs the checks; `--fix` attempts safe repairs.
- `cc-fleet repair` rebuilds vendor profile JSON from `vendors.toml`.
- `cc-fleet refresh-fingerprint` re-captures Claude Code's spawn template if a CC upgrade
  changed it.

## Files & locations

| Path | Contents |
|------|----------|
| `~/.config/cc-fleet/vendors.toml` | Vendor definitions (mode `0600`). |
| `~/.config/cc-fleet/secrets/` | File-backend keys (dir `0700`, keys `0600`, gitignored). |
| `~/.claude/profiles/` | Generated per-vendor spawn profiles. |
| `~/.claude/teams/<team>/` | Native team state (managed by Claude, not cc-fleet). |
