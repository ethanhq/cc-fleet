# Routing — cross-lane arbitration

Shared reference — each lane skill links here with one line.

Three lanes: a long-lived provider teammate (/cc-fleet:team), a one-shot provider subagent (/cc-fleet:subagent), or handle it in the main session (lane 3, below). Multi-phase / dynamic orchestration over many subagents is /cc-fleet:workflow — a lane-2 shape, no team tools needed.

---

## The tmux default (tie-breaker)

Once you've decided to offload and neither the user nor the task picks a lane, let the environment pick — check `printenv TMUX` via Bash:

- **In tmux (`$TMUX` set) → default to a long-lived teammate** (/cc-fleet:team). The pane is visible to the user; you can watch and coordinate it live.
- **Not in tmux → default to a one-shot subagent** (/cc-fleet:subagent). A teammate would have to build a detached swarm session the user can't see; the subagent is the smoother default.

Overrides, in priority order:
1. **Explicit user request wins** — "use a deepseek subagent" → subagent even in tmux; "spawn a kimi teammate" → teammate even outside tmux (it builds a swarm session).
2. **A task that clearly forces a lane** — an explicit one-shot job is a subagent; a sustained multi-file parallel build is a teammate.
3. **The agent-teams precondition** (below) still gates teammate mode.

## Agent-teams precondition — gates /cc-fleet:team

Teammate mode is driven by Claude's native `TeamCreate` / `SendMessage` tools, which exist only when this session has agent-teams enabled. `cc-fleet spawn` is a plain binary: it launches a provider pane even when those tools are absent — with no way to `SendMessage` it, that's an **orphan pane billing the provider with no work**. Before any teammate spawn:

- **Check your own tool list for a `SendMessage` (or `TeamCreate`) tool.** Present → proceed. Absent → **do NOT spawn**; tell the user agent-teams appears off — enable via `"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1"` in `~/.claude/settings.json` (`env` block) + restart Claude Code — **or** drop to /cc-fleet:subagent (needs no team tools). Offer both; default to the subagent if they don't want to reconfigure.
- This is the **only reliable signal, and only you can see it.** Don't ask `cc-fleet` whether agent-teams is on — it's an external process and can't observe Claude's runtime state; `cc-fleet doctor` deliberately doesn't report it.

## Lane 3 — handle in the main session, do NOT offload

- No provider named *and* it's a trivial single-file edit / one-off question (overhead > benefit).
- The work needs main-session context not written to disk.
- The task needs a tool only the main-session model is good at, with no parallel dimension.

"One-off" ≠ "never offload": if the user named a provider, that's /cc-fleet:subagent even for one file. Lane 3 holds only when no provider was named *and* there's no parallel dimension.

## No providers configured

If `cc-fleet list --json` returns an empty provider list, no offload lane is possible — tell the user to `cc-fleet add <provider>` first (provider notes are in providers.md, commands in cli-reference.md — both beside this file).
