---
name: subagent
description: Run a one-shot or flat parallel batch of provider LLM subagents (headless `cc-fleet subagent`) that return a result. Use when fanning out N independent tasks, doing bulk per-file work, or calling a specialized provider model (DeepSeek / GLM / Kimi / Qwen / MiniMax). NOT a long-lived collaborator you message back and forth (that is /cc-fleet:team); NOT a multi-phase pipeline with dependencies or resume (that is /cc-fleet:workflow); NOT trivial work the main session should just do.
---

# subagent — one-shot / batch / background provider subagent

**Wrong lane?** A long-lived collaborator you message back and forth → /cc-fleet:team; a multi-phase pipeline with dependencies or resume → /cc-fleet:workflow; arbitration in cc-fleet-shared/routing.md.

When this skill cites `cc-fleet-shared/<file>.md`, OPEN it with the Read tool at `../cc-fleet-shared/<file>.md` relative to this SKILL.md — the cited content is load-bearing, not optional background.

`cc-fleet subagent` runs a provider model headless and returns the result directly on Bash stdout — **no** tmux pane, **no** `TeamCreate`/`SendMessage`/`TeamDelete`. The analog of the native `Agent`/`Task` tool, but the model can be a provider id. It reuses the same provider selection and the same fingerprint self-heal flow as spawn (cc-fleet-shared/troubleshooting.md). It's the lightweight synchronous branch.

## When to use it
- **One-shot research / analysis / judgement** — you want an answer, not a long-lived colleague.
- **Batch fan-out** — N independent tasks in parallel. subagent is **lock-free**, so N calls don't serialize behind a server lock the way N spawns do — true parallelism.
- **Cost-bounded probes** — `--timeout` caps wall-clock; the return value carries `usage` / `total_cost_usd`; it stops when done (nothing to forget to tear down).

## The provider ask ladder (ask at most once per task)
1. The user named a provider or model → use it.
2. Else run `cc-fleet default --json`: if it returns a provider (source "configured" or "auto"), use it and STATE it in your kickoff line (e.g. "using glm (default)").
3. Else (several providers, none default) ask the user ONCE which to use — list the enabled providers from `cc-fleet list --json` (name + default_model + the one-line note in cc-fleet-shared/providers.md). After they pick, run `cc-fleet default <chosen>` so you never ask again. (`cc-fleet default <p>` is user-layer; only run it to FILL a blank default, never with --force.)
4. A mid-task provider failure (insufficient balance / rate limit / auth) → STOP, tell the user what happened, propose the next provider, and WAIT for their confirmation. Never switch providers silently.

Model tier within a provider: fan-out / leaf work → omit `--model` (or `--model fast`); judge / synthesis / sustained work → `--model strong`. The provider's roster decides the actual model — see cc-fleet-shared/providers.md.

## Calling it (run via Bash, always with `--json`)
```bash
cc-fleet subagent --prompt "<task>" --json                 # no provider arg → default provider
cc-fleet subagent deepseek --model strong \
  --prompt "Analyze the worst-case complexity of quicksort in src/sort.go; give a triggering input" --json
```

The provider arg is **optional** — with no provider, cc-fleet uses the default. `NO_DEFAULT_PROVIDER` / `DEFAULT_PROVIDER_DISABLED` / `DEFAULT_PROVIDER_UNKNOWN` in the failure envelope mean there is no usable default — apply the provider ask ladder above.

**Session grouping — no flag needed by default.** `cc-fleet subagent` auto-detects the parent Claude session (fail-closed: an unvalidatable registry shows `(no session)`, never a guess). The one exception: inside a known team, pass `--lead-session-id` (the team's `leadSessionId` from `~/.claude/teams/<team>/config.json`) to force the job under that team's session — an explicit flag always wins.

```bash
# Optional explicit override, with a known team:
lead_session_id=$(jq -r '.leadSessionId // empty' "$HOME/.claude/teams/<team>/config.json")
cc-fleet subagent --prompt "..." --lead-session-id "$lead_session_id" --json
```

Useful flags (full list in cc-fleet-shared/cli-reference.md):
- **Name it** → `--label "<short-alias>"` (e.g. `--label sort-complexity`). The Agents Board shows the label instead of the opaque job id — pass one on every launch, like a teammate name. Display-only metadata; capped at 256 bytes.
- **Large / sensitive prompt** → `--prompt-file <path>` (read from file, piped via stdin, kept out of argv / `ps`). Use it once a single prompt approaches **~128 KiB** (`MAX_ARG_STRLEN`, the per-argument cap — not the ~2 MB total `ARG_MAX`). `--prompt-file -` reads stdin.
- **Long task** → `--timeout 600s` (default 300s). For tasks that may exceed the timeout, run the sync call in a backgrounded Bash, or use `--background` (both below). Note: a provider that's down on **auth (401) or quota (429)** makes claude retry **~180s** before surfacing `KEY_INVALID` / `INSUFFICIENT_BALANCE`, so keep `--timeout ≥ ~200s` (the 300s default is fine) — a shorter timeout reports those as `SUBAGENT_TIMEOUT` instead. `--probe` does **not** catch a bad key (the models endpoint may not 401 it).
- **Cost / runaway gates** → `--max-budget-usd 0.5` (cap spend) and `--max-turns 8` (cap the agentic tool loop). On fan-out, strongly consider passing these on every call.
- **Prompt profile** — `slim` is the DEFAULT; read-only research → `--profile slim-ro`; `--profile full` ONLY to compare against a full session or diagnose a suspected slim regression. `--tools` REPLACES the whole tool set, never appends. Write prescriptive prompts ("Run `cmd`", "Use the Read tool on X"), not "look at" / "check" — weak provider models skip tools on weak imperatives under any profile. Tool whitelists / `--skills` / `--mcp` defaults / downgrade behavior: cc-fleet-shared/providers.md.
- **Probe** is **off by default** (`--probe` to opt in): the inner `claude -p` call is itself the authoritative reachability + auth test. On a big fan-out, run one shared `cc-fleet doctor` / probe up front rather than paying 3s × N.
- `--prompt` and `--prompt-file` are mutually exclusive — pass exactly one (else `error_code=SUBAGENT_BAD_ARGS`, no claude launched).

## Success envelope
```json
{"ok":true,"result":"<answer text>","provider":"deepseek","model":"deepseek-reasoner",
 "duration_ms":12044,"usage":{"input_tokens":812,"output_tokens":1530},
 "total_cost_usd":0.0031,"session_id":"…"}
```
→ Take `.result` as this subagent's output and hand it back / continue orchestrating. `model` is the model the provider actually billed (routing evidence). Keep `.session_id` if you intend a multi-turn follow-up (below).

## Failure envelope — dispatch on `error_code` (do not parse prose)
`error_msg` is a canonical string only; never matched on. Dispatch on `error_code`:

| `error_code` | Meaning | What you do |
|---|---|---|
| `SUBAGENT_BAD_ARGS` | Missing/both `--prompt` & `--prompt-file`. | Fix the call (exactly one). |
| `NO_DEFAULT_PROVIDER` | No provider arg and no default configured. | Apply the provider ask ladder. |
| `DEFAULT_PROVIDER_DISABLED` | The default provider is disabled. | Apply the provider ask ladder; or the user re-enables via `cc-fleet edit <provider> --enable`. |
| `DEFAULT_PROVIDER_UNKNOWN` | The default names a provider that no longer exists. | Apply the provider ask ladder; the user re-pins with `cc-fleet default <p>`. |
| `CONFIG_LOAD_FAILED` | `providers.toml` failed to load/validate. | `cc-fleet doctor`; surface to the user — don't retry. |
| `UNKNOWN_PROVIDER` / `PROVIDER_DISABLED` | Provider not configured / disabled. | Tell the user to `cc-fleet add` / `cc-fleet edit <provider> --enable`. |
| `FINGERPRINT_MISSING` | An existing `fingerprint.json` is corrupt (a fresh install uses the bundled recipe, so this is rare). | Run the **self-heal flow** in cc-fleet-shared/troubleshooting.md, then retry. |
| `FINGERPRINT_STALE` | No `claude` binary found anywhere (not a missing recipe). | Tell the user to install/fix Claude Code or PATH; the self-heal flow can't help. `cc-fleet doctor` confirms. |
| `KEY_INVALID` | Provider 401/403. | Have the user rotate the key; do not retry blindly. |
| `INSUFFICIENT_BALANCE` | Out of balance / quota (429/402 + balance signature). | Retry can't help — propose the next provider (provider ask ladder, step 4) or fall back to native `Agent`; tell the user they're out of credit. |
| `RATE_LIMITED` | Provider 429. | Wait briefly, retry once, or propose a switch (provider ask ladder, step 4). |
| `MODEL_NOT_FOUND` | Model name rejected (400). | `cc-fleet refresh <provider>` then retry, or drop `--model` to use the default. |
| `PROVIDER_UNREACHABLE` | Transport failure (only with `--probe`). | `cc-fleet doctor`; if urgent, fall back to native `Agent`. |
| `SUBAGENT_TIMEOUT` | Exceeded `--timeout`. | Real long task → raise `--timeout` (or use `--background`) and retry; suspected hang → switch provider / fall back (with user confirmation). |
| `PROVIDER_API_ERROR` | Other provider failure (5xx / overloaded). | Retry once or propose a switch. |
| `CODEX_PROXY_UNAVAILABLE` | The codex conversion daemon could not start (no login, or the loopback port is held). | Tell the user: `cc-fleet codex login`, or free / change the port (`cc-fleet codex add --port <n>`). |
| `CODEX_CLOUDFLARE_BLOCKED` | The ChatGPT backend's edge blocked this IP/client — not a key problem. | Switch network/IP or retry later; don't rotate credentials. |
| `SUBAGENT_FAILED` | claude exited with no parseable result (or turn/budget exhaustion). | Inspect; retry or switch provider. |
| `SUBAGENT_OUTPUT_TOO_LARGE` | The child's stdout/stderr exceeded the byte cap; the run was killed. | Have it write its output to a file and return a short answer (or narrow the ask) — a blind retry overflows again. |
| `SUBAGENT_STOPPED` | An operator stopped the job (`workflow stop` / a leaf stop) — terminal, NOT a failure. | Never auto-retry; surface it. |

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

## Long tasks: a backgrounded Bash is the push notification
A finished subagent CAN wake you: a backgrounded Bash command's exit is delivered to the session as a task notification. Never spawn an agent (or loop yourself) to poll. Two shapes:

1. **Sync call in a backgrounded Bash** (first choice). Run the ordinary `cc-fleet subagent … --json` with the Bash tool's `run_in_background=true`, end your turn, and the harness wakes you when it exits — the envelope is the task output. Zero extra mechanism; the process is tied to your session.
2. **`--background` + a waited status** — when the job must survive your session, or you want it grouped on the Agents Board:
```bash
cc-fleet subagent --prompt "<long task>" --background --json
# → {"ok":true,"job_id":"<uuid>","status":"running","output_file":"…","pid":…}
# arm the notifier (backgrounded Bash — its exit wakes you):
cc-fleet subagent-status <job_id> --wait --timeout 5m --json
```
Wake-up dispatch on the exit code: **0** done (envelope has `.result`) · **1** failed OR stopped — check `.status` first: `stopped` is an operator stop, never auto-retry; `failed` → dispatch on `.error_code` · **3** held (a workflow-leaf id an operator parked — surface it, never wait it out) · **124** still pending at `--timeout` (a heartbeat: re-arm; escalate only if the job is far past its own `--timeout`) · **130** interrupted. Always pass `--timeout`, and re-arm any still-pending wait after a session restart.

`cc-fleet subagent-gc --json` prunes finished job files.

## Multi-turn: `--resume`
Continue a prior subagent session (stateful, but not long-lived between turns — each turn is a fresh `claude -p --resume`):
```bash
cc-fleet subagent --resume <session_id> --prompt "<follow-up>" --json
```
`<session_id>` is the `.session_id` from the previous turn's envelope. A default-profile (slim) resume is silent; an explicitly passed `--profile` over `--resume` warns on stderr — it swaps the system prompt mid-session. Keep the profile constant across a session's turns.

## Cleanup vs. resume — they're independent
A **sync** subagent has nothing to tear down; "cleanup" only concerns `--background` job records on the Agents Board. The rules:

- **The one rule that matters: capture `.session_id` BEFORE pruning** if a follow-up is likely. gc deletes cc-fleet's job record (which holds the envelope with the id) but never Claude's transcript — so `--resume` works after gc *iff* you kept the id, and keeping the record without the id buys you nothing.
- **Prune finished, scoped to your session:** `cc-fleet subagent-gc --session <lead_session_id> --json` (immediate, skips pinned) — prefer it over a blanket `subagent-gc --older-than 0s` so you never wipe another session's records. Default gc only removes finished jobs older than 24h; running jobs are always kept; pinned records are user-owned — never force-remove them.

## Anti-patterns
- Using subagent for work that needs multiple turns / collaboration → /cc-fleet:team.
- Chaining subagents into a dependent pipeline by hand → /cc-fleet:workflow.
- `TeamCreate` / `SendMessage` / polling `cc-fleet ps --check` for a subagent → unnecessary; the result is on stdout.
- Stuffing a giant prompt into `--prompt` (hits `MAX_ARG_STRLEN` ~128 KiB) → use `--prompt-file`.
- Running a possibly-stuck provider with no bound → the default `--timeout 300s` caps it, but tune per task on fan-out, and run genuinely long work via a backgrounded Bash or `--background` (Long tasks above).
- Polling `subagent-status` in a loop (or delegating an agent to watch a job) → arm `subagent-status --wait` in a backgrounded Bash once; its exit is the notification.
- Looping on a failure without dispatching `.error_code` → every `--json` failure carries a code; switch on it (table above; spawn-side codes in cc-fleet-shared/troubleshooting.md).
- Switching providers silently after a balance / rate-limit / auth failure → stop, tell the user, wait for their pick (provider ask ladder, step 4).
