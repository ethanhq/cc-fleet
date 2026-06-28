---
name: subagent
description: Fan out a one-shot or flat parallel batch of cc-fleet PROVIDER subagents (headless `cc-fleet subagent`) that return a result — DeepSeek / GLM / Kimi / Qwen / MiniMax, or a Codex/Claude subscription. Trigger ONLY to delegate a task to a cc-fleet provider worker (parallel research, bulk per-file work, a specialized model). Do NOT trigger for a multi-phase pipeline (that is /cc-fleet:workflow), for ordinary local shell/coding, or for editing or researching cc-fleet's own code.
---

# subagent — one-shot / batch / background provider subagent

**Wrong lane?** A multi-phase pipeline with dependencies or resume → /cc-fleet:workflow.

When this skill cites `cc-fleet-shared/<file>.md`, read `../cc-fleet-shared/<file>.md` relative to this SKILL.md (load-bearing, not optional background).

`cc-fleet subagent` runs a provider model headless and returns the result directly on your shell's stdout — no tmux pane, no team, no locks. A one-shot provider agent whose model can be a provider id. It reuses the same provider selection and fingerprint gate as the rest of cc-fleet. It's the lightweight synchronous branch.

**Preflight (first fan-out per session).** Run `cc-fleet doctor --json` once and read the per-check results. The ONLY hard stops are the **claude binary** check and the **fingerprint** check (the worker engine) — if either fails, tell the user to install or fix Claude Code, and stop. Do **not** stop on the "all configured providers' keys reachable" check: it aggregates every enabled provider, so one unrelated provider — especially a daemon-backed `codex` / `openai-*`, whose loopback proxy is only up during a spawn — flips `ok:false` / exit 1 while your target provider is fine. Provider API keys are configured separately in cc-fleet, so a provider model needs the claude **binary** but not a Claude subscription. To check the provider you'll actually use, run `cc-fleet models <provider> --json` (`PROVIDER_UNKNOWN` ⇒ not configured); that confirms it's configured + enabled, not that it's reachable — the run itself proves reachability.

## Approval & sandbox (non-bypass codex)
`cc-fleet` spawns a **network-using child** (`claude -p` → the provider API) and writes a job record (plus a file when you use `--prompt-file` or redirect `--json` to one). On a codex that is **not** in full-bypass mode, expect two things — neither is a cc-fleet bug, so set the user's expectation rather than reporting a failure:
- **Approval prompts are normal.** A non-bypass approval policy asks the user to approve each `cc-fleet …` shell call; a fan-out or chunked wait re-prompts. Say so up front — approving is expected; to reduce the prompts the user can lower the approval friction (`--ask-for-approval`, a less-restrictive mode) or run codex bypassed.
- **A sandbox can block the provider call.** A workspace-write sandbox disables network by default, so the `claude -p` child cannot reach the provider and the leaf fails looking like `PROVIDER_UNREACHABLE` / a timeout — the real cause is the sandbox, not the provider. Heuristic: **`cc-fleet doctor` is locally clean but every provider leaf is "unreachable" → the sandbox is blocking egress.** Fix it by letting that command reach the network — approve the escalation codex offers, have the user enable network for the workspace-write sandbox, or run codex bypassed if they accept that.

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

**The reserved native leaf — `claude`.** `cc-fleet subagent claude …` runs the official `claude` CLI on the user's OWN Claude Code login (their subscription OAuth or whatever they're logged in as) — no providers.toml row, no profile, no key material. A NAMED deliberate choice, never the auto-default (`cc-fleet default claude` errors; it never auto-resolves). `--model` takes a literal id (`opus` / `sonnet` / a full id) — the slot keywords `default`/`strong`/`fast` are rejected (no roster); omitted = claude's login default, which is typically the most expensive tier, so naming a model is usually wise. Profiles apply unchanged. The leaf spends the **lead session's own subscription window** — use it for one or two synthesis / judgement nodes, never a wide fan-out.

```bash
cc-fleet subagent claude --model opus --prompt "<synthesis over the gathered notes>" --json
```

## Calling it (run with your shell tool, always with `--json`)
```bash
cc-fleet subagent --prompt "<task>" --label "codex/<cwd-basename>/<short>" --json   # no provider arg → default provider
cc-fleet subagent deepseek --model strong \
  --prompt "Analyze the worst-case complexity of quicksort in src/sort.go; give a triggering input" \
  --label "codex/cc-fleet/quicksort" --json
```

**Label every job.** A codex-launched job is grouped on the Agents Board under your Codex launcher (a `codex <thread>` header, not `(no session)`); a `--label "codex/<cwd-basename>/<short>"` still helps you tell your jobs apart within that group.

**Expected-large output** → redirect `--json` to a file and read the file, sidestepping any shell-output cap: `cc-fleet subagent … --json > out.json`, then read `out.json`.

The provider arg is **optional** — with no provider, cc-fleet uses the default. `NO_DEFAULT_PROVIDER` / `DEFAULT_PROVIDER_DISABLED` / `DEFAULT_PROVIDER_UNKNOWN` in the failure envelope mean there is no usable default — apply the provider ask ladder above.

Useful flags (full list: `cc-fleet subagent --help`):
- **Name it** → `--label "<short-alias>"` (e.g. `--label sort-complexity`). The Agents Board shows the label instead of the opaque job id — pass one on every launch, like a teammate name. Display-only metadata; capped at 256 bytes.
- **Large / sensitive prompt** → `--prompt-file <path>` (read from file, piped via stdin, kept out of argv / `ps`). Use it once a single prompt approaches **~128 KiB** (`MAX_ARG_STRLEN`, the per-argument cap — not the ~2 MB total `ARG_MAX`). `--prompt-file -` reads stdin.
- **Long task** → `--timeout 600s` (default 300s). For tasks that may exceed the timeout, raise `--timeout` or use `--background` (below). Note: a provider that's down on **auth (401) or quota (429)** makes claude retry **~180s** before surfacing `KEY_INVALID` / `INSUFFICIENT_BALANCE`, so keep `--timeout ≥ ~200s` (the 300s default is fine) — a shorter timeout reports those as `SUBAGENT_TIMEOUT` instead. `--probe` does **not** catch a bad key (the models endpoint may not 401 it).
- **Cost / runaway gates** → `--max-budget-usd 0.5` (cap spend) and `--max-turns` (cap the agentic tool loop). On fan-out, strongly consider a budget cap on every call. **Size `--max-turns` generously up front** — a read-heavy / multi-file / git-inspecting task spends ~1 turn per file read or command, so give it **30–50 (or omit it)**; a small cap (8/10) starves it and the leaf fails `SUBAGENT_MAX_TURNS` mid-task (budget ~$0.3–0.9 for such a leaf).
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
| `DEFAULT_PROVIDER_RESERVED` | `default_provider` is hand-set to the reserved `claude` (explicit-only). | The user runs `cc-fleet default --unset` or re-pins a real provider; don't retry. |
| `CONFIG_LOAD_FAILED` | `providers.toml` failed to load/validate. | `cc-fleet doctor`; surface to the user — don't retry. |
| `UNKNOWN_PROVIDER` / `PROVIDER_DISABLED` | Provider not configured / disabled. | Tell the user to `cc-fleet add` / `cc-fleet edit <provider> --enable`. |
| `PROVIDER_RESERVED` | A providers.toml row is named `claude` (reserved for the native leaf). | Tell the user to rename or `cc-fleet remove claude`; spawn still uses the row meanwhile. |
| `FINGERPRINT_MISSING` | An existing `~/.config/cc-fleet/fingerprint.json` is corrupt (a fresh install uses the bundled recipe, so this is rare). | `cc-fleet doctor --json` confirms. Tell the user to remove `~/.config/cc-fleet/fingerprint.json` — cc-fleet then falls back to the runtime-usable bundled recipe — and retry. (Re-snapshotting a custom recipe needs `cc-fleet refresh-fingerprint` from a Claude Code session; it is not available from Codex.) |
| `FINGERPRINT_STALE` | No `claude` binary found anywhere (not a missing recipe). | Tell the user to install/fix Claude Code or PATH; the self-heal flow can't help. `cc-fleet doctor` confirms. |
| `KEY_INVALID` | Provider 401/403. | Have the user rotate the key; do not retry blindly. |
| `INSUFFICIENT_BALANCE` | Out of balance / quota (429/402 + balance signature). | Retry can't help — propose the next provider (provider ask ladder, step 4) or handle it in the main session yourself; tell the user they're out of credit. |
| `RATE_LIMITED` | Provider 429. | Wait briefly, retry once, or propose a switch (provider ask ladder, step 4). |
| `MODEL_NOT_FOUND` | Model name rejected (400). | `cc-fleet refresh <provider>` then retry, or drop `--model` to use the default. |
| `PROVIDER_UNREACHABLE` | Transport failure (only with `--probe`). | `cc-fleet doctor`; if urgent, handle it in the main session yourself. |
| `SUBAGENT_TIMEOUT` | Exceeded `--timeout`. | Real long task → raise `--timeout` (or use `--background`) and retry; suspected hang → switch provider / fall back (with user confirmation). |
| `PROVIDER_API_ERROR` | Other provider failure (5xx / overloaded). | Retry once or propose a switch. |
| `CODEX_PROXY_UNAVAILABLE` | The codex conversion daemon could not start (no login, or the loopback port is held). | Tell the user: `cc-fleet codex login`, or free / change the port (`cc-fleet codex add --port <n>`). |
| `CODEX_CLOUDFLARE_BLOCKED` | The ChatGPT backend's edge blocked this IP/client — not a key problem. | Switch network/IP or retry later; don't rotate credentials. |
| `SUBAGENT_MAX_TURNS` | claude hit the `--max-turns` cap without finishing (the spent cost is surfaced — not silently $0). | Raise `--max-turns` (or omit it) and retry — a read-heavy / multi-file task needs ~1 turn per file read or command; a genuinely long task can use `--background`. |
| `SUBAGENT_FAILED` | claude exited with no parseable result (or budget exhaustion). For a `claude` native leaf on a logged-out machine, this is the login failure — the error preview names it (no dedicated code). | Inspect; retry or switch provider. A logged-out native leaf → tell the user to log in to Claude Code interactively. |
| `SUBAGENT_OUTPUT_TOO_LARGE` | The child's stdout/stderr exceeded the byte cap; the run was killed. | Have it write its output to a file and return a short answer (or narrow the ask) — a blind retry overflows again. |
| `SUBAGENT_STOPPED` | An operator stopped the job (`workflow stop` / a leaf stop) — terminal, NOT a failure. | Never auto-retry; surface it. |

## Batch fan-out (parallel, each returns synchronously)
```bash
# These calls are independent; subagent is lock-free, so running them
# concurrently won't queue them behind a shared lock.
cc-fleet subagent glm      --prompt "Summarize docs/a.md" --label "codex/docs/a" --json
cc-fleet subagent glm      --prompt "Summarize docs/b.md" --label "codex/docs/b" --json
cc-fleet subagent deepseek --prompt "Summarize docs/c.md" --label "codex/docs/c" --json
# Each returns its own {ok, result, total_cost_usd}; aggregate them.
```
A flat fan-out only — phases, dependencies, or dynamic orchestration over the results is /cc-fleet:workflow.

## Long tasks: synchronous — set `--timeout`, or `--background` + poll
`cc-fleet subagent` is **synchronous** — your shell tool blocks until it returns the envelope; set `--timeout` for long tasks (default 300s; raise it for research / many-turn work). If a job must outlive the call (or you want it on the Agents Board), launch it detached and poll yourself:
```bash
cc-fleet subagent --prompt "<long task>" --label "codex/<cwd-basename>/<short>" --background --json
# → {"ok":true,"job_id":"<uuid>","status":"running","output_file":"…","pid":…}
cc-fleet subagent-status <job_id> --wait --timeout 120s --json
```
`subagent-status --wait` blocks up to `--timeout`, then exits **124** while the job is still pending — re-issue it in chunks until it settles (codex has no background-task wake). If the codex shell yields with "Process running with session id …", that backgrounded `--wait` is STILL the live wait — do NOT start a second one; let it return (size each `--timeout` shorter than your shell's foreground window so it returns cleanly). Exit dispatch: **0** done (envelope has `.result`) · **1** failed OR stopped — check `.status` first: `stopped` is an operator stop, never auto-retry; `failed` → dispatch on `.error_code` · **3** held (a workflow-leaf an operator parked — surface it, never wait it out) · **124** still pending (re-issue) · **130** interrupted. Never spawn an agent to poll.

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
- **Prune finished:** `cc-fleet subagent-gc --json` removes finished jobs older than 24h; running jobs are always kept; pinned records are user-owned — never force-remove them. `--older-than 0s` clears all finished jobs now, but it is machine-wide — codex jobs have no lead session to scope to, so it also sweeps other sessions' finished records; prefer the 24h default and identify your own jobs by their `--label`.

## Anti-patterns
- Using subagent as a sustained back-and-forth collaborator → it's one-shot/batch; `--resume` is for a short follow-up, not a long conversation.
- Chaining subagents into a dependent pipeline by hand → /cc-fleet:workflow.
- Polling `cc-fleet ps --check` for a subagent → unnecessary; the result is on stdout.
- Stuffing a giant prompt into `--prompt` (hits `MAX_ARG_STRLEN` ~128 KiB) → use `--prompt-file`.
- Running a possibly-stuck provider with no bound → the default `--timeout 300s` caps it, but tune per task on fan-out, and run genuinely long work via `--background` (Long tasks above).
- Spawning / delegating an agent to watch a job → never; re-issue `subagent-status --wait --timeout 120s --json` yourself in chunks (codex has no background-task wake).
- Looping on a failure without dispatching `.error_code` → every `--json` failure carries a code; switch on it (table above).
- Switching providers silently after a balance / rate-limit / auth failure → stop, tell the user, wait for their pick (provider ask ladder, step 4).
- Fanning out N `claude` native leaves → drains your own subscription window; use a metered provider for breadth, `claude` only for one or two synthesis nodes.
