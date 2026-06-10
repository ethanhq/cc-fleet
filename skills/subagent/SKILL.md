---
name: subagent
description: Run a one-shot or a flat parallel batch of vendor LLM subagents (headless `cc-fleet subagent`) for offloaded, specialized, or bulk work that returns a result. Use when fanning out N independent tasks, doing bulk edits or per-file analysis, calling a specialized model (DeepSeek / GLM / Kimi / Qwen / MiniMax), or offloading heavy one-shot work from the main session. NOT a long-lived collaborator you message back and forth (that is /cc-fleet:team); NOT a multi-phase pipeline with dependencies or resume (that is /cc-fleet:workflow); NOT trivial single-file work the main session should just do.
---

# subagent — one-shot / batch / background provider subagent

**Wrong lane?** A long-lived collaborator you message back and forth → /cc-fleet:team; a multi-phase pipeline with dependencies or resume → /cc-fleet:workflow; arbitration in shared/routing.md. Shared docs are cited as shared/<file>.md; paths are relative to the skill's own directory, so from here a shared doc is ../shared/<file>.md.

`cc-fleet subagent` runs a provider model headless and returns the result directly on Bash stdout — **no** tmux pane, **no** `TeamCreate`/`SendMessage`/`TeamDelete`. The analog of the native `Agent`/`Task` tool, but the model can be a provider id. It reuses the same provider selection and the same fingerprint self-heal flow as spawn (shared/troubleshooting.md). It's the lightweight synchronous branch.

## When to use it
- **One-shot research / analysis / judgement** — you want an answer, not a long-lived colleague.
- **Batch fan-out** — N independent tasks in parallel. subagent is **lock-free**, so N calls don't serialize behind a server lock the way N spawns do — true parallelism.
- **Cost-bounded probes** — `--timeout` caps wall-clock; the return value carries `usage` / `total_cost_usd`; it stops when done (nothing to forget to tear down).

## Choosing the provider (ask at most once per task)
1. The user named a provider or model → use it.
2. Else run `cc-fleet default --json`: if it returns a provider (source "configured" or "auto"), use it and STATE it in your kickoff line (e.g. "using glm (default)").
3. Else (several providers, none default) ask the user ONCE which to use — list the enabled providers from `cc-fleet list --json` (name + default_model + the one-line note in shared/providers.md). After they pick, run `cc-fleet default <chosen>` so you never ask again. (`cc-fleet default <p>` is user-layer; only run it to FILL a blank default, never with --force.)
4. A mid-task provider failure (insufficient balance / rate limit / auth) → STOP, tell the user what happened, propose the next provider, and WAIT for their confirmation. Never switch providers silently.

Model tier within a provider: fan-out / leaf work → omit `--model` (or `--model fast`); judge / synthesis / sustained work → `--model strong`. The provider's roster decides the actual model — see shared/providers.md.

## Calling it (run via Bash, always with `--json`)
```bash
cc-fleet subagent --prompt "<task>" --json                 # no provider arg → default provider
cc-fleet subagent deepseek --model strong \
  --prompt "Analyze the worst-case complexity of quicksort in src/sort.go; give a triggering input" --json
```

The provider arg is **optional** — with no provider, cc-fleet uses the default. `NO_DEFAULT_PROVIDER` / `DEFAULT_PROVIDER_DISABLED` in the failure envelope mean there is no usable default — apply the ask ladder above.

**Session grouping:** `cc-fleet subagent` auto-detects the current parent Claude session when launched from a Claude Bash tool, so standalone subagents normally group under the current Agents Board session without any extra flag. When working inside a known team, you may still pass the explicit team session id from `~/.claude/teams/<team>/config.json` (`leadSessionId`); explicit `--lead-session-id` wins over auto-detection and is the safest way to force a job to match that team's teammates. Auto-detection is fail-closed: if the parent session registry cannot be validated, the job appears under `(no session)` instead of guessing.

```bash
# Optional explicit override, with a known team:
lead_session_id=$(jq -r '.leadSessionId // empty' "$HOME/.claude/teams/<team>/config.json")
cc-fleet subagent --prompt "..." --lead-session-id "$lead_session_id" --json
```

Useful flags (full list in shared/cli-reference.md):
- **Name it** → `--label "<short-alias>"` (e.g. `--label sort-complexity`). The Agents Board shows the label instead of the opaque job id — pass one on every launch, like a teammate name. Display-only metadata; capped at 256 bytes.
- **Large / sensitive prompt** → `--prompt-file <path>` (read from file, piped via stdin, kept out of argv / `ps`). Use it once a single prompt approaches **~128 KiB** (`MAX_ARG_STRLEN`, the per-argument cap — not the ~2 MB total `ARG_MAX`). `--prompt-file -` reads stdin.
- **Long task** → `--timeout 600s` (default 300s). For tasks that may exceed the timeout, prefer `--background` (below). Note: a provider that's down on **auth (401) or quota (429)** makes claude retry **~180s** before surfacing `KEY_INVALID` / `INSUFFICIENT_BALANCE`, so keep `--timeout ≥ ~200s` (the 300s default is fine) — a shorter timeout reports those as `SUBAGENT_TIMEOUT` instead. `--probe` does **not** catch a bad key (the models endpoint may not 401 it).
- **Cost / runaway gates** → `--max-budget-usd 0.5` (cap spend) and `--max-turns 8` (cap the agentic tool loop). On fan-out, strongly consider passing these on every call.
- **Prompt profile** — `slim` is the DEFAULT (native-mirror context, far smaller first request, which cache-less providers pay per call); `--profile slim-ro` for read-only research; `--profile full` ONLY to compare against a full session or diagnose a suspected slim regression. The full profile block — tool whitelists, `--tools` / `--skills=false` / `--mcp`, downgrade behavior — is in shared/providers.md; do not re-derive it from memory. Weak provider models skip tools on weak-imperative prompts under **any** profile — write prescriptive prompts ("Run `cmd`", "Use the Read tool on X"), not "look at" / "check".
- **Probe** is **off by default** (`--probe` to opt in): the inner `claude -p` call is itself the authoritative reachability + auth test. On a big fan-out, run one shared `cc-fleet doctor` / probe up front rather than paying 3s × N.
- `--prompt` and `--prompt-file` are mutually exclusive — pass exactly one (else `error_code=SUBAGENT_BAD_ARGS`, no claude launched).

## Success envelope
```json
{"ok":true,"result":"<answer text>","vendor":"deepseek","model":"deepseek-reasoner",
 "duration_ms":12044,"usage":{"input_tokens":812,"output_tokens":1530},
 "total_cost_usd":0.0031,"session_id":"…"}
```
→ Take `.result` as this subagent's output and hand it back / continue orchestrating. `model` is the model the provider actually billed (routing evidence). Keep `.session_id` if you intend a multi-turn follow-up (below).

## Failure envelope — dispatch on `error_code` (do not parse prose)
`error_msg` is a canonical string only; never matched on. Dispatch on `error_code`:

| `error_code` | Meaning | What you do |
|---|---|---|
| `SUBAGENT_BAD_ARGS` | Missing/both `--prompt` & `--prompt-file`. | Fix the call (exactly one). |
| `NO_DEFAULT_PROVIDER` | No provider arg and no default configured. | Apply the ask ladder (Choosing the provider). |
| `DEFAULT_PROVIDER_DISABLED` | The default provider is disabled. | Apply the ask ladder; or the user re-enables via `cc-fleet edit <provider> --enable`. |
| `UNKNOWN_VENDOR` / `VENDOR_DISABLED` | Provider not configured / disabled. | Tell the user to `cc-fleet add` / `cc-fleet edit <provider> --enable`. |
| `FINGERPRINT_MISSING` | An existing `fingerprint.json` is corrupt (a fresh install uses the bundled recipe, so this is rare). | Run the **self-heal flow** in shared/troubleshooting.md, then retry. |
| `FINGERPRINT_STALE` | No `claude` binary found anywhere (not a missing recipe). | Tell the user to install/fix Claude Code or PATH; the self-heal flow can't help. `cc-fleet doctor` confirms. |
| `KEY_INVALID` | Provider 401/403. | Have the user rotate the key; do not retry blindly. |
| `INSUFFICIENT_BALANCE` | Out of balance / quota (429/402 + balance signature). | Retry can't help — propose the next provider (ask ladder step 4) or fall back to native `Agent`; tell the user they're out of credit. |
| `RATE_LIMITED` | Provider 429. | Wait briefly, retry once, or propose a switch (ask ladder step 4). |
| `MODEL_NOT_FOUND` | Model name rejected (400). | `cc-fleet refresh <provider>` then retry, or drop `--model` to use the default. |
| `VENDOR_UNREACHABLE` | Transport failure (only with `--probe`). | `cc-fleet doctor`; if urgent, fall back to native `Agent`. |
| `SUBAGENT_TIMEOUT` | Exceeded `--timeout`. | Real long task → raise `--timeout` (or use `--background`) and retry; suspected hang → switch provider / fall back (with user confirmation). |
| `VENDOR_API_ERROR` | Other provider failure (5xx / overloaded). | Retry once or propose a switch. |
| `CODEX_PROXY_UNAVAILABLE` | The codex conversion daemon could not start (no login, or the loopback port is held). | Tell the user: `cc-fleet codex login`, or free / change the port (`cc-fleet codex add --port <n>`). |
| `CODEX_CLOUDFLARE_BLOCKED` | The ChatGPT backend's edge blocked this IP/client — not a key problem. | Switch network/IP or retry later; don't rotate credentials. |
| `SUBAGENT_FAILED` | claude exited with no parseable result (or turn/budget exhaustion). | Inspect; retry or switch provider. |

## Batch fan-out (parallel, each returns synchronously)
```bash
# These Bash calls are independent and can fire in parallel; subagent is
# lock-free, so they don't queue behind each other.
cc-fleet subagent glm      --prompt "Summarize docs/a.md" --json
cc-fleet subagent glm      --prompt "Summarize docs/b.md" --json
cc-fleet subagent deepseek --prompt "Summarize docs/c.md" --json
# Each returns its own {ok, result, total_cost_usd}; aggregate them. No
# TeamCreate / TeamDelete needed.
```
A flat fan-out only — phases, dependencies, or dynamic orchestration over the results is /cc-fleet:workflow.

## Long tasks: `--background` + `subagent-status` (poll, not push)
A subagent is a separate process and **cannot push a notification to you** (unlike native Agent, which is in-process). For a task that may run longer than you want to block a Bash call:
```bash
cc-fleet subagent --prompt "<long task>" --background --json
# → {"ok":true,"job_id":"<uuid>","status":"running","output_file":"…","pid":…}
# later, poll:
cc-fleet subagent-status <job_id> --json
# → {"status":"running"}  … then …  {"ok":true,"status":"done","result":"…", …}
```
This is a **poll** model: re-check `subagent-status` after a while; there is no idle notification. (Need push-on-done → that's a teammate's job, /cc-fleet:team.) `cc-fleet subagent-gc --json` prunes finished job files.

## Multi-turn: `--resume`
Continue a prior subagent session (stateful, but not long-lived between turns — each turn is a fresh `claude -p --resume`):
```bash
cc-fleet subagent --resume <session_id> --prompt "<follow-up>" --json
```
`<session_id>` is the `.session_id` from the previous turn's envelope. A default-profile (slim) resume is silent; an explicitly passed `--profile` over `--resume` warns on stderr — it swaps the system prompt mid-session. Keep the profile constant across a session's turns.

## Cleanup vs. resume — they're independent
A one-shot **sync** subagent is just a process that exits — no pane, no team, **nothing to tear down**. "Cleanup" only ever concerns **`--background` job records** on the Agents Board:

- **Finished → safe to prune.** `cc-fleet subagent-gc --json` removes *finished* background job files (default: only those older than 24h; **`cc-fleet subagent-gc --older-than 0s` clears all finished now**). Running jobs are always kept.
- **Scope your cleanup to a session.** `cc-fleet subagent-gc --session <lead_session_id> --json` clears only *that* session's finished jobs/runs immediately — prefer it over a blanket `--older-than 0s` clear-all so you never wipe another session's records. A user can **pin** records on the Agents Board (kept across any GC); pinned records are user-owned, so an agent's housekeeping must never try to force-remove them. Be deliberate about clearing everything.
- **Pruning does NOT end the conversation.** gc only deletes cc-fleet's bookkeeping under `~/.config/cc-fleet/subagent-jobs/`; it never touches Claude's session transcript (`~/.claude/projects/…`). So **`--resume <session_id>` still works after gc** — *as long as you kept the `session_id`*. That id lives in the result envelope, which gc removes with the job, so **if a follow-up is likely, capture `.session_id` before pruning** (or just leave the job until you're done resuming).
- The flip side: **leaving the job record does not by itself let you resume** — resume needs the `session_id` (plus Claude's own session retention), not the cc-fleet record. The way to preserve a follow-up is *recording the session_id*, not *skipping cleanup*.

## Anti-patterns
- Using subagent for work that needs multiple turns / collaboration → /cc-fleet:team.
- Chaining subagents into a dependent pipeline by hand → /cc-fleet:workflow.
- `TeamCreate` / `SendMessage` / polling `cc-fleet ps --check` for a subagent → unnecessary; the result is on stdout.
- Stuffing a giant prompt into `--prompt` (hits `MAX_ARG_STRLEN` ~128 KiB) → use `--prompt-file`.
- Running a possibly-stuck provider with no bound → the default `--timeout 300s` caps it, but tune per task on fan-out, and use `--background` for genuinely long work.
- Looping on a failure without dispatching `.error_code` → every `--json` failure carries a code; switch on it (table above; spawn-side codes in shared/troubleshooting.md).
- Switching providers silently after a balance / rate-limit / auth failure → stop, tell the user, wait for their pick (ask ladder step 4).
