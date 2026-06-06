---
name: cc-fleet
description: Delegate work to any third-party LLM provider with an Anthropic-compatible API (e.g. DeepSeek, GLM, Kimi, Qwen, MiniMax) as real Claude Code agent-team teammates or one-shot subagents via cc-fleet — driven exactly like native teammates, and your main session's own auth (OAuth or API key) stays untouched. Use when the user asks to spawn a vendor teammate or subagent, when bulk / parallel / specialized work warrants offloading from the main session, or when a `cc-fleet` command needs to be invoked.
---

# cc-fleet

This skill teaches you to use the `cc-fleet` CLI to run any third-party LLM with an Anthropic-compatible endpoint (e.g. DeepSeek, GLM, Kimi, Qwen, MiniMax) as **real Claude Code agent-team teammates** — same full tool stack and native team coordination as a native teammate, just with the LLM backend swapped to a vendor model. You drive them exactly like a native `Agent` teammate.

**Your main session's own auth is untouched** — whether it logs in with an OAuth subscription or an API key. Vendor workers bill the vendor API; your main session is unaffected. Native `Agent({model: 'sonnet|opus|haiku'})` cannot accept `--settings <path>` or vendor model ids — that's exactly the gap cc-fleet fills.

This file is the router + the common path. Depth lives in `references/` — read the one a step points you to.

---

## Routing: three lanes (read this first)

Sort every request into **exactly one** lane before doing anything else:

| Lane | When | Action |
|---|---|---|
| **1 · Long-lived vendor teammate** | Parallel build · user says "spawn workers" · >10-file batch · multi-turn collaboration · model specialization on a *sustained* task | `cc-fleet spawn` a teammate. Core loop below; full detail `references/teammates.md`. |
| **2 · One-shot vendor offload** | User names a vendor for a *one-time* job ("use deepseek to analyze this file") · a batch fan-out of independent one-shot tasks | `cc-fleet subagent` — synchronous, **no pane, no `TeamCreate`/`SendMessage`/`TeamDelete`**. Summary below; full detail `references/subagent.md`. |
| **3 · Ordinary local work** | **No vendor named *and* no parallel/batch dimension** — a single-file edit, a quick question, interactive work whose context lives in the main session | **Do it yourself — do NOT delegate.** |

> **"One-off" ≠ "never offload."** Lane 3's "single-file / one-off" only means it isn't worth a **long-lived teammate** — it does **not** mean ignore an explicit vendor request. If the user named a vendor ("use deepseek …"), that's lane **2** (a subagent), even for one file. Fall to lane 3 only when **no vendor was named and there's no parallel dimension**.

### Lane 1 vs lane 2 — default by environment (tmux)

Once you've decided to offload (not lane 3) and the task itself doesn't force a lane, let the **environment** pick the default (check `printenv TMUX` via Bash):
- **In tmux (`$TMUX` set) → default to a teammate (lane 1).** The pane is visible to the user; you can watch / coordinate it live.
- **Not in tmux → default to a subagent (lane 2).** A teammate would have to build a swarm session the user can't see, so the one-shot subagent is the smoother default.

Overrides, in priority order:
1. **An explicit user mode request wins** — "use a deepseek subagent" → lane 2 even in tmux; "spawn a kimi teammate" → lane 1 even outside tmux (it builds a swarm session, see "Where a teammate runs").
2. **A task that clearly forces a lane** still does — an explicit one-shot job is lane 2; a sustained >10-file parallel build is lane 1.
3. **The lane-1 precondition still gates teammate mode** (below).

### Lane 1 — spawn a long-lived teammate when any hold
1. **The user explicitly asks for a teammate / workers** ("spawn a kimi worker", "N teammates on N files, use the cheapest").
2. **Parallel batch work** — >10 file edits / >5 independent units concurrent (bulk refactor, batch translation, per-file analysis).
3. **Model specialization** on a sustained task: `deepseek-reasoner` (math/logic/debug) · `kimi-k2`/`kimi-latest` (Chinese / 200k+ context) · `glm-4.6` (domain Chinese) · `qwen` (Chinese + tools / cost).
4. **Main-session quota at risk** — long session, heavy tool use, user mentions limits.
5. **Data residency** — traffic must stay in-region (Chinese vendor for Chinese data).

A *one-shot* version of any of these is **lane 2** (`cc-fleet subagent`), not a teammate.

### Lane 1 precondition — agent-teams must be ON

Teammate mode is driven by Claude's **native `TeamCreate` / `SendMessage` tools**, which exist **only when this session has agent-teams enabled**. `cc-fleet spawn` is a plain binary: it will launch a vendor pane **even when those tools are absent** — and you'd then have no way to `SendMessage` it, leaving an **orphan pane billing the vendor with no work**. So before any lane-1 spawn:

- **Check your own tool list for a `SendMessage` (or `TeamCreate`) tool.** Present → proceed with lane 1. Absent → **do NOT spawn**; **tell the user agent-teams appears off** — they can enable it by setting `"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1"` in `~/.claude/settings.json` (`env` block) and restarting Claude Code, **or** you can run this job now in **lane 2** (`cc-fleet subagent`, needs no native team tools). Offer both; default to lane 2 if they don't want to reconfigure.
- This is the ONLY reliable signal, and **only you can see it.** Don't ask `cc-fleet` whether agent-teams is on — it's an external process and can't observe Claude's runtime state. `cc-fleet doctor` deliberately does not report it.

### Lane 3 — do NOT spawn (handle in the main session) when
- No vendor named *and* it's a single-file edit / one-off question (overhead > benefit).
- The work is interactive / needs main-session context not written to disk.
- The task needs a tool only the main-session model is good at, with no parallel dimension.

If `cc-fleet list --json` returns an empty `vendors` array, neither lane is possible — tell the user to `cc-fleet add <vendor>` first.

---

## Decision flow (one screen)

```
request → which lane? (above)
  lane 3 → handle in main session, do NOT spawn
  offload, lane ambiguous → default by env: in tmux → lane 1 · not in tmux → lane 2
                            (explicit user request / clear task-nature overrides)
  lane 2 → cc-fleet subagent (no TeamCreate / pane) — see below + references/subagent.md
  lane 1 → ↓
     agent-teams on? (do YOU have a SendMessage/TeamCreate tool?)
        no  → drop to lane 2 + tell user agent-teams appears off; do NOT spawn
        yes → cc-fleet list --json  (empty → tell user to `cc-fleet add`, stop)
              → TeamCreate (native) → spawn → SendMessage → wait → report → cleanup
```

---

## Lane 1 — the teammate core loop

Steps 1, 3, 6 are **native tools**; steps 2, 4, 5 are `cc-fleet` via Bash with `--json`. Full workflow, two worked examples, vendor/model selection, hide/show, and the cheat sheet are in **`references/teammates.md`** — read it for a real spawn.

```
1. TeamCreate({team_name})                                            ← native, FIRST
2. cc-fleet spawn <vendor> --as <name> --team <team> [--model <m>] --json   ← Bash, check ok:true
3. SendMessage({to: <name>, message: "<task>. SendMessage me the result when done."})  ← native
4. (optional) repeat 2+3 to spawn more workers in parallel
5. wait for idle notifications (harness auto-delivers for healthy teammates)
6. report to the user → ASK before cleanup → on confirm: teardown then TeamDelete
```

Four things that trip people up (detail in `references/teammates.md`):

- **Pick the model deliberately.** `cc-fleet list --json` → `cc-fleet models <vendor> --json` (`refresh` if stale); omit `--model` to use the vendor default. The cheat sheet maps capability/cost/language.
- **A vendor teammate can wedge silently.** If the vendor API returns `429` / out-of-balance / `401`, the teammate retries forever and **never goes idle and never messages you** — so "just wait" blocks forever. Set a timeout (~60–90s first check, then ~2–3 min) and poll `cc-fleet ps --json --check` (redacted `error_class`). On `error`: tear it down and switch vendor / fall back to native `Agent`, and tell the user.
- **Idle but no result?** Weaker models (e.g. `glm`) often finish and go idle **without** calling SendMessage. Ask once more via SendMessage; if still nothing, read the pane directly (`tmux -L <tmux_socket> capture-pane -t <pane_id> -p`) — the API key is never in the pane, so this is safe. Don't bother the user.
- **Cleanup is two steps, in order:** `cc-fleet teardown <team> --json` (Bash — kills the vendor pane + reaps the proc) **then** `TeamDelete()` (native — removes the team/tasks dirs). `TeamDelete` never touches tmux, so a vendor pane would orphan; and teardown needs the `config.json` that TeamDelete deletes. Don't auto-kill on task completion — the teammate is reusable; ask first (unless the user already said "clean up when done").

On a `cc-fleet spawn` failure (`ok:false`), dispatch on `error_code` — see **`references/troubleshooting.md`** (`FINGERPRINT_MISSING` / `SPAWN_DID_NOT_SETTLE` run the self-heal probe flow there; most other codes are config issues to surface to the user).

### Where a teammate runs (in-tmux split vs out-of-tmux swarm)

Either way you drive it with native `SendMessage` and receive results via `SendMessage` — the pane is only where the teammate's process *runs*, never how you talk to it.
- **In tmux** → the teammate splits a pane in your visible window. `cc-fleet hide`/`show` can tuck it away (see `references/teammates.md`).
- **Not in tmux** → `cc-fleet spawn` auto-builds a **detached** `cc-fleet-swarm-<team>` tmux server and runs the teammate there. It's **silent** — you never see it unless you `tmux -L cc-fleet-swarm-<team> attach`. (Why a pane at all? A reusable, SendMessage-able teammate is an interactive Claude process that polls its inbox, which needs a TTY; a vendor teammate must be a *separate* process for its own apiKeyHelper, so it can't run in-process. The truly-headless one-shot path is lane 2.) `hide`/`show` is **in-tmux only** — for a swarm teammate it returns `SWARM_UNSUPPORTED` (a terminal no-op; attach to view it).

---

## Lane 2 — one-shot subagent (summary)

`cc-fleet subagent <vendor> --model <m> --prompt "<task>" --json` runs a vendor model headless and returns the result synchronously on stdout — **no pane, no `TeamCreate`/`SendMessage`/`TeamDelete`**. Use it for one-shot research/analysis and lock-free batch fan-out. `--prompt-file` for big/sensitive prompts, `--timeout` / `--max-budget-usd` / `--max-turns` to bound it, `--profile slim` (or `slim-ro` for read-only research) for a native-mirror context instead of the full session prompt — much smaller first request on cache-less vendors, `--background` + `cc-fleet subagent-status` for long tasks, `--resume <session_id>` for multi-turn. Full flags, envelopes, and `error_code` dispatch: **`references/subagent.md`**. For multi-phase or dynamic orchestration over many vendor subagents — a Starlark script (`cc-fleet workflow run`) that runs OFF this context with fan-out/pipeline/loop and a board run-tree — see **`references/workflow.md`**.

---

## Anti-patterns

- **Spawning a teammate for a single-file edit / quick question** — use the main session; the overhead isn't worth it.
- **Bypassing `SendMessage` to type into a vendor pane** — task delivery is always `SendMessage`. (Reading a pane for a result *is* fine — see the core loop.)
- **Skipping `TeamCreate` before spawn** → `NO_LEAD_SESSION` / `TEAM_NOT_FOUND`. Native `TeamCreate` first (or `--auto-team`, default on).
- **Looping on errors without dispatching `.error_code`** — every `--json` failure carries a code; switch on it.
- **Running the self-heal flow on every spawn** — only on `FINGERPRINT_MISSING` / `SPAWN_DID_NOT_SETTLE`, never preemptively, never for `FINGERPRINT_STALE` (that's a missing binary — fix Claude Code / PATH).
- **Putting the vendor API key in argv / env** — cc-fleet does it through `apiKeyHelper`; keys never enter `env` / `ps aux` / history. Don't export `ANTHROPIC_API_KEY`.
- **Editing `vendors.toml` / profile / fingerprint.json by hand** — go through `cc-fleet add`/`edit`/`repair`/`refresh-fingerprint`.
- **`rm -rf ~/.claude/teams/...` to tear down** — skips pane/proc cleanup. Vendor team = `cc-fleet teardown` FIRST, then `TeamDelete()`.
- **Waiting open-endedly on a vendor teammate** — it can wedge on a vendor error and never go idle; always timeout + `ps --check`.

---

## Reference files (read on demand)

- **`references/teammates.md`** — lane 1 full workflow: spawn + examples, vendor/model pick, getting results, stuck-teammate monitoring + `error_class` table, hide/show + the Agent status board, vendor cheat sheet.
- **`references/subagent.md`** — lane 2 full manual: when, calling, flags, envelopes, batch fan-out, `--background`, `--resume`, cleanup.
- **`references/workflow.md`** — the workflow runtime: a Starlark script (`cc-fleet workflow run`) run OFF this context that fans out vendor subagents via `meta`/`agent`/`parallel`/`pipeline`/`phase`/`log`, with an enforced pool + board run-tree; non-goals (no resume, no live stop, shallow schema).
- **`references/cli-reference.md`** — every `cc-fleet` command (user + Claude layer), spawn flags + permission inheritance, JSON envelopes, native-`Agent`-vs-vendor table.
- **`references/troubleshooting.md`** — spawn `error_code` dispatch table + the 6-step fingerprint self-heal flow.

---

## One-line summary

`TeamCreate` (native) → `cc-fleet spawn <vendor> --as <n> --team <t> --json` (Bash, parse) → `SendMessage` (native, ask it to report) → wait **with a timeout, polling `cc-fleet ps --json --check`** (a vendor teammate can wedge on a 429 / balance / 401 and never go idle; idle-but-silent → re-ask, then read the pane) → **report + confirm before cleanup** (reusable; don't auto-kill) → on confirm, `cc-fleet teardown <team> --json` (Bash) **then** `TeamDelete` (native), in that order. One-shot instead → `cc-fleet subagent` (lane 2). `FINGERPRINT_MISSING` / `SPAWN_DID_NOT_SETTLE` → self-heal flow; `FINGERPRINT_STALE` → fix Claude Code / PATH.
