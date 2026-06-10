---
name: team
description: Spawn long-lived vendor LLM teammates in tmux panes that you message via the native agent-team tools — multi-turn, collaborative, watchable. Use for sustained parallel build/work ("spawn workers", N teammates on N files), or when you need a collaborator you message across turns. NOT a fire-and-forget one-shot or a flat batch of independent prompts — that is /cc-fleet:subagent. NOT a scripted multi-phase run — that is /cc-fleet:workflow.
---

# team — long-lived provider teammates

Run a third-party provider model as a real Claude Code teammate in a tmux pane: same tool stack and team coordination as a native teammate, LLM backend swapped to the provider. Your main session's own auth stays untouched.

**Wrong lane?** A fire-and-forget one-shot or flat batch → /cc-fleet:subagent; a scripted multi-phase run → /cc-fleet:workflow; full arbitration in shared/routing.md.

Shared docs are cited as shared/<file>.md (../shared/<file>.md from here).

**Precondition — agent-teams must be ON:** check your own tool list for `SendMessage` / `TeamCreate`; absent → do NOT spawn (an unmessageable pane bills the provider with no work) — enablement + fallback in shared/routing.md.

---

## Core loop

Steps 1, 3, 6 are **native tools**; steps 2, 5, 6a are `cc-fleet` via Bash with `--json`.

```
1. TeamCreate({team_name: "<team>"})
   ← native, FIRST — the main session becomes the lead

2. cc-fleet spawn [<provider>] --as <name> --team <team> [--model <slot|id>] --json
   ← Bash; check ok:true, grab .pane_id / .agent_id
   The provider arg is OPTIONAL — omitted, the default provider applies
   (see "Choosing the provider"). Full flag table: shared/cli-reference.md.

3. SendMessage({to: "<name>", message: "<task>. When done, send your result
   back with SendMessage."})
   ← native; always tell it to report — see "Getting a result back"

4. (optional) repeat 2+3 to fan out more workers in parallel

5. wait for idle notifications — WITH a timeout. A teammate on a failed
   provider API (429 / out-of-balance / 401) wedges in a retry loop and never
   goes idle, so "just wait" blocks forever. Poll cc-fleet ps --json --check
   and dispatch on error_class — see "Watching for stuck teammates".

6. report to the user, then ASK before tearing down. On confirm, BOTH, in order:
   a. cc-fleet teardown <team> --json   ← Bash FIRST: kills provider panes + reaps procs
   b. TeamDelete()                      ← native SECOND: removes the team/tasks dirs
```

On a spawn failure (`ok:false`), dispatch on `error_code` — table + self-heal flow in shared/troubleshooting.md.

**Why teardown before TeamDelete.** `TeamDelete()` only deletes `~/.claude/teams|tasks/<team>/`; it never touches tmux. A provider teammate is an external tmux process that does NOT self-close — `TeamDelete()` alone leaves an orphan pane + process (a wedged one keeps billing the provider). `cc-fleet teardown` kills the pane and reaps the process, and must run first because it reads the team's `config.json` (the swarm socket lives there) — which `TeamDelete()` deletes. `teardown` is forceful by design; for a provider team it is required. Same order for wedged / probe teams.

**Ask before teardown.** Don't auto-kill on task completion. A teammate cost real tokens to spin up and is reusable — you can SendMessage it the next task, and the user may want to look at its pane. Summarize what it produced, then ask whether to keep or tear down. Skip the ask only when the user already said "clean up when done" or it's a throwaway probe team.

### Example: one worker on a refactor

```bash
TeamCreate({team_name: "refactor-api"})                                       # native
cc-fleet spawn --as worker-1 --team refactor-api --model strong --json        # default provider
# → {"ok":true,"agent_id":"worker-1@refactor-api","name":"worker-1","pane_id":"%42", ...}
SendMessage({to: "worker-1", message: "Refactor src/api/handlers.go: split each handler into its own file under src/api/handlers/. Keep tests passing. Report your result via SendMessage when done."})
# … wait with timeout + ps --check …
# report; tear down only after the user confirms:
cc-fleet teardown refactor-api --json    # Bash, FIRST
TeamDelete()                             # native
```

### Example: three workers in parallel

```bash
TeamCreate({team_name: "translate-docs"})
cc-fleet spawn kimi --as zh-1   --team translate-docs --json                  # leaf: omit --model
cc-fleet spawn kimi --as zh-2   --team translate-docs --json
cc-fleet spawn deepseek --as polish --team translate-docs --model strong --json   # synthesis: strong
SendMessage({to: "zh-1",   message: "Translate docs/intro.md to zh-CN beside it. SendMessage me when done."})
SendMessage({to: "zh-2",   message: "Translate docs/api.md to zh-CN beside it. SendMessage me when done."})
SendMessage({to: "polish", message: "When zh-1 and zh-2 finish, copy-edit their outputs for tone consistency. SendMessage me the result."})
# … notifications per worker; report first, tear down on confirm:
cc-fleet teardown translate-docs --json   # FIRST: kills all three panes + procs
TeamDelete()
```

---

## Choosing the provider (ask at most once per task)
1. The user named a provider or model → use it.
2. Else run `cc-fleet default --json`: if it returns a provider (source "configured" or "auto"), use it and STATE it in your kickoff line (e.g. "using glm (default)").
3. Else (several providers, none default) ask the user ONCE which to use — list the enabled providers from `cc-fleet list --json` (name + default_model + the one-line note in shared/providers.md). After they pick, run `cc-fleet default <chosen>` so you never ask again. (`cc-fleet default <p>` is user-layer; only run it to FILL a blank default, never with --force.)
4. A mid-task provider failure (insufficient balance / rate limit / auth) → STOP, tell the user what happened, propose the next provider, and WAIT for their confirmation. Never switch providers silently.

Model tier within a provider: fan-out / leaf work → omit `--model` (or `--model fast`); judge / synthesis / sustained work → `--model strong`. The provider's roster decides the actual model — see shared/providers.md.

---

## Getting a teammate's result back

A teammate reports by calling `SendMessage` to the lead; the harness delivers it to you. Mode-independent — split pane or swarm pane, the pane is where it *runs*, never how you talk to it. Two provider-specific notes:

1. **Tell it to report.** End every task message with *"When done, send your final result back to me with SendMessage."* Weaker provider models often finish and go idle WITHOUT calling SendMessage — the answer sits in their pane.

2. **Idle but no result → ask once more, then read the pane.** Re-`SendMessage`: *"You appear done — reply with your result via SendMessage."* If the second ask still yields nothing, read the pane directly — don't bother the user:
   ```bash
   cc-fleet ps --json          # → the teammate's tmux_socket + pane_id
   tmux -L <tmux_socket> capture-pane -t <pane_id> -p | tail -40
   ```
   Safe: the provider API key is never in the pane (resolved via `apiKeyHelper`, never printed). `tmux_socket` is empty for in-tmux teammates (plain `tmux capture-pane`) and `cc-fleet-swarm-<team>` for swarm teammates.

---

## Watching for stuck teammates

The one runtime difference from a native teammate. A provider teammate's brain *is* the provider API: on `429` / out-of-balance / `401` its claude process retries in a loop and **never goes idle, never messages you** — either would need the very LLM that's down. The error shows only in its tmux pane, never in your inbox. So you must poll.

1. **Set a timeout.** A provider API error surfaces on the first LLM call — check ~60–90s after dispatch, then every ~2–3 min while a task legitimately runs. An idle notification cancels the wait.

2. **Poll health, don't sleep blindly:** `cc-fleet ps --json --check`. `--check` scans each pane and adds `status` (`ok` | `error` | `unknown`) plus `error_class` + `detail` on error — only the class, never raw pane text. Dispatch:
   - `ok` — keep waiting (within your ceiling).
   - `unknown` — pane couldn't be captured (teammate exited / tmux down). Confirm with `cc-fleet ps --json`; treat a vanished teammate as failed.
   - `error` — act now, per `error_class`:

   | `error_class` | Meaning | What you do |
   |---|---|---|
   | `insufficient_balance` | Provider out of balance / quota. | Retrying can't help. Tear down; STOP, tell the user, propose the next provider, wait for confirm (ask-policy rule 4). |
   | `auth` | Provider rejected the key (`401`/`403`). | Tear down. Tell the user to rotate the key — file backend: `cc-fleet edit <provider> --api-key-stdin <<<"$NEW_KEY"` (or `--api-key-file <path>`); other backends via the secret manager. Don't re-spawn the same provider. **Never** the raw key in argv. |
   | `rate_limit` | Provider `429`. | Tear down; wait a bit and re-spawn, or propose a switch (confirm first). Never keep a wedged teammate looping. |
   | `api_error` | Generic provider failure (5xx, overloaded, rejected). | Tear down; retry once, or propose a switch (confirm first). |

3. **`unknown` or not specific enough → `capture-pane` and read it yourself** (same command + key-safety note as above). `ps --check` is the first probe; the raw pane is a fine fallback.

### Acting on a wedged teammate

Tear down just the wedged worker (siblings keep running) by pane id: `cc-fleet teardown <pane_id> --json`, or the whole team with `cc-fleet teardown <team> --json` (then `TeamDelete()` if done). Then surface it and propose the fallback — another provider (re-spawn + re-`SendMessage` after the user confirms) or native `Agent({subagent_type: "general-purpose", model: "sonnet", prompt: "<task>"})`. Never leave a teammate wedged and keep waiting.

---

## Where a teammate runs (in-tmux split vs out-of-tmux swarm)

Either way you drive it with native `SendMessage` and it reports via `SendMessage`.

- **In tmux** → the teammate splits a pane in your visible window; hide/show available (below).
- **Not in tmux** → spawn auto-builds a detached `cc-fleet-swarm-<team>` tmux server and runs the teammate there — silent unless you `tmux -L cc-fleet-swarm-<team> attach`. (A reusable, SendMessage-able teammate is an interactive process polling its inbox — needs a TTY; the truly-headless path is /cc-fleet:subagent.)

---

## Hiding / showing a pane (in-tmux only)

Declutter the layout without killing the process:

```bash
cc-fleet hide <target> --json    # pane → detached "claude-hidden" session; keeps running
cc-fleet show <target> --json    # pane → back to its origin window, re-tiled
```

`<target>` = pane id `%42` · `team/member` · `name@team` · bare `team` (every member with a pane). Origin is recorded at hide time.

- **hide does NOT kill** — inbox/`SendMessage` still work, `teardown` still cleans it.
- **Swarm teammates are unsupported** — `error_code: "SWARM_UNSUPPORTED"` is a terminal no-op, not a tmux failure; attach to view instead.
- Dispatch on `error_code`, not prose: `SWARM_UNSUPPORTED` / `TEAM_NOT_FOUND` / `MEMBER_NOT_FOUND` / `PANE_NOT_FOUND` / `NOT_HIDDEN` / `NO_ORIGIN` / `TMUX_FAILED`. Hiding an already-hidden pane is idempotent `ok`.

**Agents Board** (human-facing): bare `cc-fleet` → `Tab` to a live board of every teammate across all teams (`ps --check` health, HIDDEN column, `h`/`s` hide/show). You use `cc-fleet ps --json --check` programmatically, not the TUI.

---

## Anti-patterns

- **Spawning a teammate for a single-file edit / quick question** — main session; the overhead isn't worth it (shared/routing.md).
- **Typing into a provider pane instead of `SendMessage`** — task delivery is always `SendMessage`. (Reading a pane for a result is fine.)
- **Skipping `TeamCreate` before spawn** → `NO_LEAD_SESSION` / `TEAM_NOT_FOUND`. Native `TeamCreate` first.
- **Waiting open-endedly on a teammate** — it can wedge and never go idle; always timeout + `ps --check`.
- **Switching providers silently after a failure** — ask-policy rule 4: stop, tell, propose, wait for confirm.
- **Auto-tearing down on task completion** — the teammate is reusable; ask first.
- **`rm -rf ~/.claude/teams/...` to tear down** — skips pane/proc cleanup. `cc-fleet teardown` FIRST, then `TeamDelete()`.
- **Putting the provider API key in argv / env** — cc-fleet uses `apiKeyHelper`; keys never enter env / `ps aux` / history.
- **Looping on errors without dispatching `.error_code`** — every `--json` failure carries a code (shared/troubleshooting.md).
