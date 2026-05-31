# Lane 1 — vendor teammate full workflow

Read this when you've decided on a **long-lived vendor teammate** (lane 1). The SKILL.md decision flow is the summary; this is the detail.

## Contents
- How to pick vendor + model
- Spawn workflow (the happy path) + two worked examples
- Getting a teammate's result back (SendMessage, then capture-pane)
- Watching for stuck vendor teammates (the one runtime difference from native)
- Hiding / showing a teammate's pane + the Agent status board
- Vendor cheat sheet

---

## How to pick vendor + model

1. **List configured vendors.** `cc-fleet list --json` → `{"ok":true,"vendors":[{"name":"deepseek","default_model":"…","enabled":true,...}, ...]}`. Skip any vendor with `"enabled":false`.
2. **Filter by need** against the cheat sheet (capability + cost + language).
3. **Check the vendor's model list** (cached): `cc-fleet models <vendor> --json`. If it shows `"stale": true` or lacks the model you want, `cc-fleet refresh <vendor> --json`, then re-check.
4. **Fall back to the vendor's `default_model`** if you have no strong preference — that's what `cc-fleet spawn` uses when you omit `--model`.

---

## Spawn workflow

The full happy-path sequence. Steps 1, 3, 6 are **native tools** (call them directly). Steps 2, 4, 5 are cc-fleet (run via Bash with `--json`).

```
1. TeamCreate({team_name: "<team>"})
   ← native tool, must run first so the main session is the lead

2. cc-fleet spawn <vendor> --as <name> --team <team> [--model <m>] --json
   ← Bash, parse JSON, check ok:true, grab .pane_id / .agent_id
   (Outside tmux this auto-builds a detached swarm session — see SKILL "static lane".)

3. SendMessage({to: "<name>", message: "<task>. When done, send your result back with SendMessage."})
   ← native tool, deliver work. Tell it to report — see "Getting a result back".

4. (optional) repeat 2+3 to spawn more workers in parallel

5. wait for idle notifications (the harness delivers these automatically for healthy teammates)
   ⚠ VENDOR CAVEAT: a vendor teammate whose API returns 429 / out-of-balance / 401
   wedges in a retry loop and never goes idle — so "just wait" blocks forever. Set a
   timeout and health-check via `cc-fleet ps --json --check`. See "Watching for stuck
   vendor teammates".

6. Report the result to the user, then ASK before tearing down — DON'T auto-kill on
   task completion. A vendor teammate cost real tokens to spin up and is REUSABLE (you
   can SendMessage it the next task, or the user may want to look at its pane), and
   teardown is destructive. Summarize what it produced, then ask whether to keep it or
   tear it down. Only tear down on user confirm — or when they already said "clean up
   when done" / it's a throwaway probe team. Then, BOTH in this order:

   cc-fleet teardown <team> --json   ← Bash: reclaim the vendor panes + procs FIRST
   then TeamDelete()                 ← native tool: remove the team / tasks dirs
```

**Why both, in that order.** `TeamDelete()` only deletes `~/.claude/teams|tasks/<team>/` (+ worktrees); it NEVER touches tmux. A native teammate self-closes its pane; a vendor teammate is an external tmux process that does NOT — so `TeamDelete()` alone leaves an orphan pane + process (a wedged one can keep billing the vendor). `cc-fleet teardown` kills the pane + reaps the process, and must run BEFORE `TeamDelete()` because it reads the team's `config.json` (the swarm socket lives there) to find the panes — which `TeamDelete()` deletes. `teardown` is forceful (it kills; not a graceful shutdown) — exactly what's wanted, and for a vendor team it is REQUIRED. Same order for wedged / probe teams.

### Example: one DeepSeek worker on a refactor
```bash
TeamCreate({team_name: "refactor-api"})                                   # native
cc-fleet spawn deepseek --as worker-1 --team refactor-api --model deepseek-reasoner --json   # Bash
# → {"ok":true,"agent_id":"worker-1@refactor-api","name":"worker-1","pane_id":"%42", ... }
SendMessage({to: "worker-1", message: "Refactor src/api/handlers.go: split each handler into its own file under src/api/handlers/. Keep tests passing. Report your result via SendMessage when done."})   # native
# … wait (idle notifications) …
# report to the user; tear down only after they confirm (or want to reuse the worker):
cc-fleet teardown refactor-api --json    # Bash, FIRST
TeamDelete()                             # native
```

### Example: three vendor workers in parallel
```bash
TeamCreate({team_name: "translate-docs"})                                # native
# fire all three spawns in parallel; each Bash call is independent
cc-fleet spawn kimi     --as zh-1   --team translate-docs --model kimi-latest --json
cc-fleet spawn kimi     --as zh-2   --team translate-docs --model kimi-latest --json
cc-fleet spawn deepseek --as polish --team translate-docs --json
SendMessage({to: "zh-1",   message: "Translate docs/intro.md to zh-CN beside it. SendMessage me when done."})   # native ×3
SendMessage({to: "zh-2",   message: "Translate docs/api.md to zh-CN beside it. SendMessage me when done."})
SendMessage({to: "polish", message: "When zh-1 and zh-2 finish, copy-edit their outputs for tone consistency. SendMessage me the result."})
# … notifications arrive when each goes idle; report first, tear down only on confirm:
cc-fleet teardown translate-docs --json   # FIRST kills all three vendor panes + procs
TeamDelete()
```

---

## Getting a teammate's result back

A teammate reports the way every teammate does — it calls **`SendMessage`** to the lead and the harness delivers it to you. This is mode-independent: in-tmux split panes and out-of-tmux swarm panes both report via `SendMessage` (the pane is just where the teammate *runs*, never how you talk to it). Two practical notes for **vendor** teammates:

1. **Tell it to report.** End the task message with an explicit instruction — *"When done, send your final result back to me with SendMessage."* Weaker vendor models (e.g. `glm`) often finish the work and go idle **without** calling SendMessage — they just print the answer in their own pane.

2. **Idle but no result message → ask once more, then read the pane.** First re-`SendMessage`: *"You appear done — reply with your result via SendMessage."* If that **second** ask still yields nothing, don't bother the user — read the teammate's pane directly:
   ```bash
   cc-fleet ps --json          # → the teammate's tmux_socket + pane_id
   tmux -L <tmux_socket> capture-pane -t <pane_id> -p | tail -40
   ```
   This is safe: the vendor API key is **never** in the pane (it's resolved through `apiKeyHelper` / `cc-fleet keyget`, never printed), so reading pane text can't leak it. `tmux_socket` is empty for in-tmux teammates (use plain `tmux capture-pane`) and `cc-fleet-swarm-<team>` for swarm teammates.

---

## Watching for stuck vendor teammates

**This is the one place vendor teammates genuinely differ from native `Agent` at runtime — read it before you "just wait for idle".**

A native teammate runs on the subscription; if it errors it can still reason about the failure and message you or go idle. A **vendor teammate's brain *is* the vendor API.** When that API returns `429` / out-of-balance / `401`, the teammate's claude process retries in a loop and **never goes idle and never messages you** — doing either would need the very LLM that's down. The harness only auto-delivers *idle* notifications, so a lead that only waits blocks **forever**. Vendor teammates hit this far more than native ones: independent metered quota → `429` / `余额不足` / rate-limit are common, not rare. The error shows up **only in the teammate's tmux pane** — never in the inbox the lead reads. So you must poll.

### The rule
After `SendMessage` to a vendor teammate, do **not** wait open-endedly:

1. **Set a timeout.** A vendor API error surfaces on the *first* LLM call, so an initial check **~60–90s** after dispatch catches "spawned but immediately out-of-balance / rate-limited". While a task legitimately runs, re-check every **~2–3 min**. An idle notification arriving cancels the wait (that's success — stop checking).

2. **On each check, poll health instead of sleeping blindly:** `cc-fleet ps --json --check`. `--check` scans each teammate's pane and adds `status` (`ok` | `error` | `unknown`), plus `error_class` + `detail` when `status=="error"`. It reports only the error **class**, never raw pane text. Dispatch on `status`:
   - `ok` — no API-error signature; keep waiting (within your ceiling).
   - `unknown` — pane couldn't be captured (teammate exited / tmux down). Confirm with `cc-fleet ps --json`; treat a vanished teammate as failed and fall back.
   - `error` — **act now.** Dispatch on `error_class`:

   | `error_class` | Meaning | What you do |
   |---|---|---|
   | `insufficient_balance` | Vendor out of balance / quota (`余额不足`). | Retrying can't help. Tear down + switch vendor **or** fall back to native `Agent`. Tell the user. |
   | `auth` | Vendor rejected the key (`401`/`403`). | Tear down. Tell the user to rotate the key — file backend: `cc-fleet edit <vendor> --api-key-stdin <<<"$NEW_KEY"` (or `--api-key-file <path>`); other backends: rotate via the secret manager. Don't re-spawn the same vendor. **Never** put the raw key in argv. |
   | `rate_limit` | Vendor `429` / rate limit. | Tear down; wait a bit and re-spawn, or switch vendor. Don't keep a wedged teammate looping. |
   | `api_error` | Generic vendor failure (`Request rejected`, 5xx, overloaded). | Tear down + retry once or switch vendor. |

3. **If `--check` is `unknown` or not specific enough, `capture-pane` the pane and read it yourself** (same command + key-safety note as "Getting a result back" above — the key is never in the pane). `ps --check` is the right *first* probe; reading the raw pane directly is a fine fallback and doesn't need the user.

### Acting on a wedged teammate
Tear down just the wedged worker (siblings keep running) by pane id: `cc-fleet teardown <pane_id> --json`. Or the whole team with `cc-fleet teardown <team> --json` (then `TeamDelete()` if you're done — teardown reclaims the vendor panes/procs, TeamDelete only removes native dirs). Then **fall back and tell the user**:
- Switch vendor: `cc-fleet spawn <other-vendor> --as <name> --team <team> --json`, then re-`SendMessage` the task.
- Or native subscription: `Agent({subagent_type: "general-purpose", model: "sonnet", prompt: "<task>"})`.
- Always surface it, e.g. *"`glm` hit 429 / 余额不足 and was stuck, so I tore it down and switched to `deepseek`"*.

Never leave a vendor teammate wedged and keep waiting — that is the exact failure this section exists to prevent.

---

## Hiding / showing a teammate's pane (in-tmux teammates only)

To declutter the split layout while an **in-tmux** teammate keeps working — **without** killing its process — move its pane into a detached hidden session and bring it back later:
```bash
cc-fleet hide <target> --json    # pane → detached "claude-hidden" session; process keeps running
cc-fleet show <target> --json    # pane → back to its origin window, re-tiled main-vertical
```
`<target>` = a tmux pane id `%42` · `team/member` · `name@team` · a bare `team` (every member with a pane). The origin window is recorded **at hide time**, so `show` restores the pane where it was.

- **In-tmux only.** Hide/show is unavailable for **out-of-tmux swarm** teammates — they run on a detached private server you aren't attached to (nothing to declutter), and the swarm session has no leader-pane anchor, so hiding the last teammate would destroy it and orphan the others. The CLI returns `error_code: "SWARM_UNSUPPORTED"` for a swarm teammate — treat it as a **terminal no-op, not a recoverable tmux failure**; to view a swarm teammate, use its `attach_command` (`tmux -L cc-fleet-swarm-<team> attach -t claude-swarm`).
- **hide does NOT kill the teammate** — `pgrep`/`ps` still find it, inbox/`SendMessage` still work, `teardown` still cleans it (no leak). It just disappears from the visible layout.
- **Dispatch on `error_code`** (don't parse prose): `SWARM_UNSUPPORTED` (out-of-tmux swarm teammate) / `TEAM_NOT_FOUND` / `MEMBER_NOT_FOUND` / `PANE_NOT_FOUND` / `NOT_HIDDEN` (show on a visible pane) / `NO_ORIGIN` (no recorded origin) / `TMUX_FAILED`. Hiding an already-hidden pane is idempotent `ok`. `--json` emits one object for a single target, an array when a bare team expands to several.
- **Best-effort layout**: a reflow hiccup never fails the op — `show` only fails if the load-bearing `join-pane` itself fails.

You usually won't need this programmatically — it's mainly for the Agent status board below.

### Agent status board (interactive TUI — for the human)
Run bare `cc-fleet` in a terminal → press `Tab` from **Vendors** to **Agent status**: a live board of every teammate across **all teams** (with `ps --check` health + a `HIDDEN` column) plus subagent jobs, grouped by parent Claude session then team. Session headers show the Claude `/rename` title when available while keeping the short UUID. Keys: `↑/↓` select · `h` hide / `s` show the selected teammate · `r` refresh · auto-refreshes every 3s. The job table shows only id/vendor/model/status/started — never answer text. This is a human-facing view; you (the orchestrator) use `cc-fleet ps --json --check` for teammate data programmatically.

---

## Vendor cheat sheet

Default seeds for the five built-in vendors. URLs may shift; always confirm via `cc-fleet list --json` and `cc-fleet models <vendor> --json` for current state.

| Vendor | base_url (Anthropic-compat) | Suggested model | Notes |
|---|---|---|---|
| `deepseek` | `https://api.deepseek.com/anthropic` | `deepseek-chat` (cheap, fast) / `deepseek-reasoner` (math/code) | Use canonical names; legacy aliases silently fall back to default. |
| `kimi` (Moonshot) | `https://api.moonshot.cn/anthropic` | `kimi-latest`, `kimi-k2-0905-preview` | 200k+ context; strong Chinese. |
| `glm` (智谱) | `https://open.bigmodel.cn/api/anthropic` | `glm-4.6`, `glm-4.5` | Domain Chinese, industry vertical. |
| `qwen` (DashScope) | `https://dashscope.aliyuncs.com/...` | varies by region | Often needs OpenAI-format conversion; consult user docs if `refresh` fails. |
| `minimax` | `https://api.minimaxi.com/v1/anthropic` | `MiniMax-M2`, `abab7-chat-preview` | — |

For a vendor not in this table, it works the same way — the user runs `cc-fleet add <vendor> --base-url <url> --models-endpoint <url> --default-model <id> --api-key-stdin <<<"$KEY"` first (use `--api-key-stdin` or `--api-key-file`; **never** put the raw key in argv).
