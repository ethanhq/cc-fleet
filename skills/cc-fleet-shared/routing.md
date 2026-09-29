# Routing — cross-lane arbitration

Shared reference — each lane skill links here with one line.

Three lanes: a long-lived provider teammate (/cc-fleet:team), a one-shot provider subagent (/cc-fleet:subagent), or handle it in the main session (lane 3, below). Multi-phase / dynamic orchestration over many subagents is /cc-fleet:workflow — a lane-2 shape, no team tools needed.

---

## The tmux default (tie-breaker)

Once you've decided to offload and neither the user nor the task picks a lane, let the environment pick — check `printenv TMUX` via Bash:

- **In tmux (`$TMUX` set) → default to a long-lived teammate** (/cc-fleet:team). The pane is visible to the user; you can watch and coordinate it live.
- **Not in tmux → default to a one-shot subagent** (/cc-fleet:subagent). Outside tmux a teammate starts only with `teammateMode` `tmux`, on a detached tmux server the user can't see unless they attach — or in iTerm2 with `auto`: iTerm2's own panes, or a detached tmux server when `it2` is missing. The subagent is the smoother default.

Overrides, in priority order:
1. **Explicit user request wins** — "use a deepseek subagent" → subagent even in tmux; "start a kimi teammate" → teammate even outside tmux, when `teammate check` passes (it then runs on a detached tmux server, or in iTerm2's own panes).
2. **A task that clearly forces a lane** — an explicit one-shot job is a subagent; a sustained multi-file parallel build is a teammate.
3. **The teammate precondition** (below) still gates teammate mode.

## Teammate precondition — gates /cc-fleet:team

A provider teammate is a member of Claude Code's native agent team: you start it with the native `Agent` tool (`subagent_type: "ccf-<provider>"`), and cc-fleet's launcher routes it to the provider. Before the first one, run `cc-fleet teammate check [<provider>] --json` via Bash:

- **`ok:true` and `protocol` 1 → proceed** with /cc-fleet:team.
- **`ok:false` → dispatch on `error_code`** (troubleshooting.md). The lane is **not available** — go straight to /cc-fleet:subagent or /cc-fleet:workflow, which need no team — when teammates would run **in-process** (`TEAMMATE_MODE_IN_PROCESS`: a `ccf-*` teammate would silently run on Claude, not the provider), when the session is **not an interactive terminal `claude`** — the **desktop app**, `claude -p` and SDK sessions have no agent team (`TEAMMATE_LANE_UNAVAILABLE`) — and on **Windows** (`UNSUPPORTED_ON_WINDOWS`). Setup and restart codes (`TEAMMATE_SETUP_REQUIRED`, `LEAD_RESTART_REQUIRED`) → tell the user the one-time fix and use the subagent lane meanwhile unless they want to fix it now. The same `TEAMMATE_LANE_UNAVAILABLE` / `no_session_team` in a terminal session (`$TMUX` set, or iTerm2) with `printenv CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS` empty or not `1`/`true`/`yes`/`on` means agent teams are off because `cc-fleet teammate setup` never ran — degrade now and offer that one-time setup (/cc-fleet:team); with agent teams on, the user starts a new `claude` inside tmux or iTerm2.
- **`teammate check` is the signal — not your tool list.** `SendMessage` and `ListAgents` exist even in sessions with no team (the desktop app has both), so their presence proves nothing.

## Lane 3 — handle in the main session, do NOT offload

- No provider named *and* it's a trivial single-file edit / one-off question (overhead > benefit).
- The work needs main-session context not written to disk.
- The task needs a tool only the main-session model is good at, with no parallel dimension.

"One-off" ≠ "never offload": if the user named a provider, that's /cc-fleet:subagent even for one file. Lane 3 holds only when no provider was named *and* there's no parallel dimension.

## No providers configured

If `cc-fleet list --json` returns an empty provider list, no offload lane is possible — tell the user to `cc-fleet add <provider>` first (provider notes are in providers.md, commands in cli-reference.md — both beside this file).

Exception: even with no provider configured, the subagent and workflow lanes can still use the reserved id `claude` (`cc-fleet subagent claude …`, `agent(..., {provider: "claude"})`), which runs the user's OWN Claude Code login — no providers.toml row needed (providers.md). It does NOT enable the teammate lane: a provider teammate still needs a configured provider, and a native Claude teammate is just the native `Agent` tool with a native agent type — there is no `ccf-claude`.
