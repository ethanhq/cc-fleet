---
name: team
description: Start long-lived provider LLM teammates as native Claude Code agent-team members (the native Agent tool with a ccf-<provider> type) that you message with SendMessage — multi-turn, collaborative, watchable in tmux panes. Use for sustained parallel build/work ("start 3 glm teammates", N teammates on N files), or when you need a collaborator you message across turns, from a terminal `claude` session. NOT a fire-and-forget one-shot or a flat batch of independent prompts — that is /cc-fleet:subagent. NOT a scripted multi-phase run — that is /cc-fleet:workflow.
---

# team — long-lived provider teammates

Run a third-party provider model as a real Claude Code teammate: you start it with the native `Agent` tool, Claude Code creates the pane and does all the team plumbing, and cc-fleet's launcher swaps the teammate's LLM backend to the provider. Same tool stack and team coordination as a native teammate; your main session's own auth stays untouched.

**Wrong lane?** A fire-and-forget one-shot or flat batch → /cc-fleet:subagent; a scripted multi-phase run → /cc-fleet:workflow; full arbitration in cc-fleet-shared/routing.md.

When this skill cites `cc-fleet-shared/<file>.md`, OPEN it with the Read tool at `../cc-fleet-shared/<file>.md` relative to this SKILL.md — the cited content is load-bearing, not optional background.

> **Execution environment — check before running anything.** Confirm your shell tool executes on the host where cc-fleet is installed. In sandboxed or remote agent sessions, a tool named Bash may run on an isolated machine with a different filesystem, PATH, processes, and tmux server — `command not found`, a healthy-looking `doctor` whose leaves can't reach your files, or a wrong working directory should prompt you to verify whether you are in a sandbox shell, not conclude that cc-fleet is broken. If so, route commands through a host-executing bridge tool (for example, desktop-commander) and pass host paths for any files you reference; do not retry the same Bash call expecting different results. If no host-executing tool is available, stop and explain that cc-fleet must run on its installation host.

**Precondition — `cc-fleet teammate check` must pass.** Before the first teammate of a provider/slot in this session, run `cc-fleet teammate check [<provider>] [--slot strong|fast] --json` via Bash and require `ok:true` **and** `protocol` equal to `1`. Use its `agent_type` verbatim as the `Agent` call's `subagent_type`. `ok:false` → dispatch on `error_code` + `detail` (cc-fleet-shared/troubleshooting.md); never call `Agent` with a `ccf-*` type after a failed check. A `protocol` other than `1` means cc-fleet and the plugin are out of step — tell the user to update both (`cc-fleet update`, then `/plugin update`) and use /cc-fleet:subagent meanwhile. `check` also writes the provider profile and starts the local proxy a codex / openai-* provider needs; `--no-probe` skips its 3s reachability probe.

**Where this lane works.** Only a terminal `claude` session (Claude Code ≥ 2.1.278, macOS/Linux) whose teammates open in panes: `teammateMode` `tmux` (inside or outside tmux), or `auto` while the lead runs inside tmux or iTerm2. The Claude desktop app, `claude -p` and SDK sessions have no agent team (`TEAMMATE_LANE_UNAVAILABLE`); in-process teammates would silently run on Claude, not the provider (`TEAMMATE_MODE_IN_PROCESS`); Windows has no teammate lane (`UNSUPPORTED_ON_WINDOWS`). In all of these, go straight to /cc-fleet:subagent or /cc-fleet:workflow. A terminal session (in tmux or iTerm2) can get the same `TEAMMATE_LANE_UNAVAILABLE` / `no_session_team`: with `printenv CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS` empty or not `1`/`true`/`yes`/`on`, agent teams are off because `cc-fleet teammate setup` never ran — degrade now and offer the one-time setup below; with it on, cc-fleet could not identify the team (after `/clear` or a resume), and the user starts a new `claude` inside tmux or iTerm2. Dispatch on `detail`, not the code alone (cc-fleet-shared/troubleshooting.md).

**One-time setup is the user's call.** `TEAMMATE_SETUP_REQUIRED` / `launcher_not_configured` means `cc-fleet teammate setup` never ran — so does `no_session_team` in a terminal session with agent teams off (above), because check stops at the missing team before it looks at the launcher. Setup writes cc-fleet's launcher shim, the `ccf-*` agent definitions, and `env.CLAUDE_CODE_TEAMMATE_COMMAND` (plus agent teams, if off) in `~/.claude/settings.json`. Tell the user what it changes; run `cc-fleet teammate setup --yes --json` only after they agree (add `--teammate-mode tmux` when their `teammateMode` is unset or `in-process`), or let them run it. They must then restart claude inside tmux — until they do, `check` may return `LEAD_RESTART_REQUIRED`, and returns `no_session_team` if agent teams were off before setup. Check stops before it reads `teammateMode` on these paths; if setup's JSON shows `teammate_mode` `in-process`, ask before rerunning it with `--teammate-mode tmux`, before the restart.

---

## Core loop

Steps 1 and 3 are `cc-fleet` via Bash with `--json`; steps 2, 4 and 5 are **native tools**.

```
1. cc-fleet teammate check [<provider>] [--slot strong|fast] --json
   ← Bash; require ok:true and protocol == 1; keep .agent_type and .team
   The provider arg is OPTIONAL — omitted, the default provider applies
   (see "The provider ask ladder").

2. Agent({name: "<name>", subagent_type: "<agent_type>",
          prompt: "<task>. When done, send your result to team-lead with SendMessage — once."})
   ← native; `name` is what makes it a teammate. Repeat for more teammates
   (the same agent_type needs no second check).

3. ~10s later: cc-fleet ps --json --check
   ← Bash; find the row whose agent_id is "<name>@<team>"
   state running → carry on; failed / bypassed → TaskStop it and report
   (see "Watching for stuck teammates").

4. SendMessage({to: "<name>", message: "<next task>. Report back with SendMessage."})
   ← native, as often as you like; the teammate keeps its context.
   Wait for its message or idle notification — WITH a timeout (a teammate on a
   failed provider API never goes idle).

5. report to the user, then ASK before ending it. On confirm:
   SendMessage({to: "<name>", message: {type: "shutdown_request", reason: "<why>"}})
   ← native; the teammate approves and exits, Claude Code closes its pane.
   Stuck teammate → TaskStop; no task id → cc-fleet teardown <pane_id> --socket <tmux_socket_path> --json
```

**Never, on a `ccf-*` type:**
- omit `name` — it would run as an in-process subagent on Claude, not the provider (`BAD_AGENT_CALL` / `missing_name`);
- pass `model` — it overrides the provider routing (`model_param`); the slot comes with the type (`ccf-<p>.strong` / `ccf-<p>.fast`, from `teammate check --slot`);
- pass `isolation` or `cwd` — neither starts a teammate (`isolation` / `cwd`).

The plugin's PreToolUse guard blocks these, and any `ccf-*` call while the lane is not ready, with a deny reason `cc-fleet: <CODE>(<detail>): …` — dispatch it like a check failure; never retry the same call. There is one implicit team per session, which Claude Code creates and removes itself: `team_name` is ignored, and the old `TeamCreate` / `TeamDelete` tools and `cc-fleet spawn` (now `COMMAND_REMOVED`) are gone.

### Example: one teammate on a refactor

```bash
cc-fleet teammate check --slot strong --json                      # Bash; default provider
# → {"ok":true,"protocol":1,"provider":"glm","slot":"strong","model":"glm-4.6",
#    "agent_type":"ccf-glm.strong","team":"session-7c8f769b",...,"warnings":[]}
Agent({name: "worker-1", subagent_type: "ccf-glm.strong", prompt: "Refactor src/api/handlers.go: split each handler into its own file under src/api/handlers/. Keep tests passing. When done, send your result to team-lead with SendMessage — once."})   # native
cc-fleet ps --json --check                                        # ~10s later: worker-1@session-7c8f769b running
# … its SendMessage arrives (timeout + ps --check meanwhile); report; end it only after the user confirms:
SendMessage({to: "worker-1", message: {type: "shutdown_request", reason: "refactor done"}})
```

### Example: three teammates in parallel

```bash
cc-fleet teammate check kimi --json                               # → agent_type "ccf-kimi"
cc-fleet teammate check deepseek --slot strong --json             # → agent_type "ccf-deepseek.strong"
Agent({name: "zh-1",   subagent_type: "ccf-kimi",            prompt: "Translate docs/intro.md to zh-CN beside it. SendMessage team-lead once when done."})
Agent({name: "zh-2",   subagent_type: "ccf-kimi",            prompt: "Translate docs/api.md to zh-CN beside it. SendMessage team-lead once when done."})
Agent({name: "polish", subagent_type: "ccf-deepseek.strong", prompt: "Wait for my next message; I will send you the outputs to copy-edit."})
cc-fleet ps --json --check                                        # all three running?
# … when zh-1 and zh-2 report:
SendMessage({to: "polish", message: "Copy-edit docs/intro.zh-CN.md and docs/api.zh-CN.md for tone consistency. SendMessage me the result."})
# report first; end them on the user's confirm (shutdown_request to each).
```

---

## The provider ask ladder (ask at most once per task)
1. The user named a provider or model → use it.
2. Else run `cc-fleet default --json`: if it returns a provider (source "configured" or "auto"), use it and STATE it in your kickoff line (e.g. "using glm (default)").
3. Else (several providers, none default) ask the user ONCE which to use — list the enabled providers from `cc-fleet list --json` (name + default_model + the one-line note in cc-fleet-shared/providers.md). After they pick, run `cc-fleet default <chosen>` so you never ask again. (`cc-fleet default <p>` is user-layer; only run it to FILL a blank default, never with --force.)
4. A mid-task provider failure (insufficient balance / rate limit / auth) → STOP, tell the user what happened, propose the next provider, and WAIT for their confirmation. Never switch providers silently.
   STOP freezes the affected provider operation and any unapproved provider switch, not independent authorized work. The error table's single rate-limit retry remains allowed; do not retry uncertain writes before establishing their prior result. Main-session handling is allowed only where the workflow already permits it and the user has not required that provider. Changing a provider or the saved default still requires the corresponding explicit authorization.

Model tier within a provider: fan-out / leaf work → omit `--slot` (or `--slot fast`); judge / synthesis / sustained work → `--slot strong`. The provider's roster decides the actual model — see cc-fleet-shared/providers.md. A teammate takes only a configured slot, never a literal model id: a model the user names must first be configured as a slot (`cc-fleet edit <provider> --strong-model <id>`, user-run).

---

## Getting a teammate's result back

A teammate reports by calling `SendMessage` to the lead; the harness delivers it to you. Wherever its pane lives, you talk to it only through `SendMessage`. Two provider-specific notes:

1. **Tell it to report, once.** End every task message with *"When done, send your final result back to me with SendMessage — once."* Weaker provider models often finish and go idle WITHOUT calling SendMessage — the answer sits in their pane — and some repeat the message many times.

2. **Idle but no result → ask once more, then read the pane.** Re-`SendMessage`: *"You appear done — reply with your result via SendMessage."* If the second ask still yields nothing, read the pane directly — don't bother the user:
   ```bash
   cc-fleet ps --json          # → the teammate's tmux_socket_path + pane_id
   tmux -S <tmux_socket_path> capture-pane -t <pane_id> -p | tail -40
   ```
   Safe: the provider API key is never in the pane (resolved via `apiKeyHelper`, never printed). `tmux_socket_path` is the tmux server's socket path for `tmux -S` (it replaced 0.3.x's `tmux_socket`, a `-L` name). A row with `backend` `in-process` or `unknown` (iTerm2 native panes) has no tmux pane to read.

---

## Watching for stuck teammates

The one runtime difference from a native teammate. A provider teammate's brain *is* the provider API: on `429` / out-of-balance / `401` its claude process retries in a loop and **never goes idle, never messages you** — either would need the very LLM that's down. The error shows only in its pane, never in your inbox. So you must poll.

1. **Set a timeout.** A provider API error surfaces on the first LLM call — check ~60–90s after dispatch, then every ~2–3 min while a task legitimately runs. A message or idle notification cancels the wait.

2. **Poll health, don't sleep blindly:** `cc-fleet ps --json --check`. Each row has a `state`:
   - `running` — normal; `--check` then adds `status` (`ok` | `error` | `unknown`) plus `error_class` + `detail` on error — only the class, never raw pane text.
   - `failed` — the launcher refused to start it (the row's `error_code` uses the check vocabulary; `error_class` `launch_failed`). TaskStop it, fix the cause per cc-fleet-shared/troubleshooting.md, re-run `teammate check`, start a fresh teammate.
   - `bypassed` — it started WITHOUT cc-fleet's launcher, so it runs on Claude, not the provider (`error_class` `launcher_bypassed`). TaskStop it at once and report; re-run `teammate check` before trying again.
   - `orphaned` — its lead session is gone (a crashed lead). Not yours to reuse; after asking, clean up with `cc-fleet teardown <team> --json`.

   Note the key: this runtime detection dispatches on **`error_class`** (lower_snake), a DIFFERENT key from the **`error_code`** (UPPER_SNAKE) that `teammate check` and subagent failure envelopes carry — don't switch on the wrong one. For `status`:
   - `ok` — keep waiting (within your ceiling).
   - `unknown` — pane couldn't be captured (teammate exited / tmux down). Confirm with `cc-fleet ps --json`; treat a vanished teammate as failed.
   - `error` — act now, per `error_class`:

   | `error_class` | Meaning | What you do |
   |---|---|---|
   | `insufficient_balance` | Provider out of balance / quota. | Retrying can't help. Stop the teammate; STOP, tell the user, propose the next provider, wait for confirm (provider ask ladder, step 4). |
   | `auth` | Provider rejected the key (`401`/`403`). | Stop the teammate. Tell the user to rotate the key — file backend: `cc-fleet edit <provider> --api-key-stdin <<<"$NEW_KEY"` (or `--api-key-file <path>`); other backends via the secret manager. Don't start another teammate on the same provider. **Never** the raw key in argv. |
   | `rate_limit` | Provider `429`. | Stop the teammate; wait a bit and start a fresh one, or propose a switch (confirm first). Never keep a wedged teammate looping. |
   | `api_error` | Generic provider failure (5xx, overloaded, rejected). | Stop the teammate; retry once, or propose a switch (confirm first). |
   | `cloudflare_blocked` | The ChatGPT backend's edge blocked this IP/client — not a key problem. | Stop the teammate; switch network or retry later; don't rotate credentials. |

3. **`unknown` or not specific enough → `capture-pane` and read it yourself** (same command + key-safety note as above). `ps --check` is the first probe; the raw pane is a fine fallback.

### Acting on a wedged teammate

Stop just the wedged teammate (siblings keep running) with **TaskStop** — Claude Code kills its pane and removes it from the team. Without a task id, use `cc-fleet teardown <pane_id> --socket <tmux_socket_path> --json` or `cc-fleet teardown <name>@<team> --json`: it re-verifies the teammate's identity (pane, exact argv, process start) and kills only cc-fleet provider teammates. It returns `{ok, target, killed[], skipped[]}`; a `skipped` entry with reason `IDENTITY_MISMATCH` means that pane no longer holds the teammate (nothing was killed), `IN_PROCESS` means only TaskStop can stop it. `cc-fleet teardown <team> --json` kills every provider teammate of the team — for orphans after a lead crash. teardown never edits `~/.claude/teams`; Claude Code removes the member when the lead exits. Then surface it and propose the fallback — another provider (a new teammate + re-`SendMessage` after the user confirms) or native `Agent({subagent_type: "general-purpose", model: "sonnet", prompt: "<task>"})`. Never leave a teammate wedged and keep waiting.

---

## Where a teammate runs

Claude Code picks the pane; you drive the teammate with native `SendMessage` either way, and Claude Code closes its panes when the lead exits.

- **`teammateMode` tmux, lead inside tmux** (or `auto` inside tmux) → a split pane in your visible window; hide/show available (below).
- **`teammateMode` tmux, lead outside tmux** → Claude Code runs it on its own detached tmux server, silent unless the user attaches: `tmux -S <tmux_socket_path> attach`. hide/show returns `SWARM_UNSUPPORTED`.
- **`auto` inside iTerm2, outside tmux** → native iTerm2 panes (`ps` shows `backend` `unknown`; hide/show returns `BACKEND_UNSUPPORTED`), or, when iTerm2's `it2` integration is missing, a detached tmux server as with `tmux` outside tmux (`SWARM_UNSUPPORTED`). `teammate check` reports this as `backend_hint` `iterm2-or-tmux-external`.

---

## Hiding / showing a pane (tmux split panes only)

Declutter the layout without killing the process:

```bash
cc-fleet hide <target> [--socket <tmux_socket_path>] --json    # pane → detached "claude-hidden" session; keeps running
cc-fleet show <target> [--socket <tmux_socket_path>] --json    # pane → back to its origin window, re-tiled
```

`<target>` = pane id `%42` · `name@team`. Add `--socket` when the envelope says `AMBIGUOUS_TARGET` (the same pane id exists on several tmux servers). The old forms (bare `team`, `team/member`) return `BAD_ARGS`. The origin window is recorded on the pane itself at hide time.

- **hide does NOT kill** — `SendMessage` still works, `teardown` still cleans it.
- **Detached swarm servers and non-tmux panes are unsupported** — `SWARM_UNSUPPORTED` / `BACKEND_UNSUPPORTED` are terminal no-ops, not tmux failures; attach or use the terminal app instead.
- Dispatch on `error_code`, not prose: `SWARM_UNSUPPORTED` / `BACKEND_UNSUPPORTED` / `PANE_NOT_FOUND` / `NOT_HIDDEN` / `NO_ORIGIN` / `AMBIGUOUS_TARGET` / `IDENTITY_MISMATCH` (the pane no longer holds that teammate) / `TMUX_FAILED` / `BAD_ARGS` (malformed or old-form target) / `UNSUPPORTED_ON_WINDOWS` / `INTERNAL`. Hiding an already-hidden pane is idempotent `ok`.

**Agents Board** (human-facing): bare `cc-fleet` → `Tab` to a live board of every provider teammate (`ps --check` health, HIDDEN column, `h`/`s` hide/show). You use `cc-fleet ps --json --check` programmatically, not the TUI.

---

## Anti-patterns

- **Starting a teammate for a single-file edit / quick question** — main session; the overhead isn't worth it (cc-fleet-shared/routing.md).
- **Calling `Agent` with a `ccf-*` type before `teammate check` passes**, or without `name`, or with `model` / `isolation` / `cwd`.
- **Using provider teammates from the desktop app, `claude -p`, or an in-process session** — they cannot run on the provider there; use /cc-fleet:subagent or /cc-fleet:workflow.
- **Typing into a provider pane instead of `SendMessage`** — task delivery is always `SendMessage`. (Reading a pane for a result is fine.)
- **Waiting open-endedly on a teammate** — it can wedge and never go idle; always timeout + `ps --check`.
- **Switching providers silently after a failure** — provider ask ladder, step 4: stop, tell, propose, wait for confirm.
- **Auto-ending a teammate on task completion** — the teammate is reusable; ask first.
- **Editing or `rm -rf`-ing `~/.claude/teams/...`** — Claude Code owns the team. End a teammate with `shutdown_request` or TaskStop; clean up orphans with `cc-fleet teardown`.
- **Putting the provider API key in argv / env** — cc-fleet uses `apiKeyHelper`; keys never enter env / `ps aux` / history.
- **Looping on errors without dispatching `.error_code`** — every `--json` failure carries a code (cc-fleet-shared/troubleshooting.md).
