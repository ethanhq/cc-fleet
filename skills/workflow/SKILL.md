---
name: workflow
description: Orchestrate a MULTI-PHASE, dependent, or resumable run over many provider subagents from a JS script, off the main context (`cc-fleet workflow`). Use for fan-out→barrier→synthesis, per-item pipelines, loop-until-dry, or a run that must survive a kill and `--resume` from its journal. NOT a flat fan-out of independent tasks (that is /cc-fleet:subagent — cheaper, no script); NOT interactive collaboration (that is /cc-fleet:team); NOT trivial single-shot work for the main session.
---

# workflow — multi-phase JS orchestration over provider subagents

**Wrong lane?** A flat one-shot fan-out of independent tasks → /cc-fleet:subagent; interactive collaboration you message back and forth → /cc-fleet:team; arbitration in cc-fleet-shared/routing.md.

When this skill cites `cc-fleet-shared/<file>.md`, OPEN it with the Read tool at `../cc-fleet-shared/<file>.md` relative to this SKILL.md — the cited content is load-bearing, not optional background.

A **workflow** is a JavaScript script that fans out provider `cc-fleet subagent` leaves and runs in a **cc-fleet process, OFF the main session's context**. You write the script; `cc-fleet workflow run` executes it. The orchestration plan lives in script variables (CPU, ~0 of your tokens) — you are invoked only when *authoring* the script, not on every scheduling decision. The API mirrors the native Claude Code Workflow tool — write the script exactly as you would a native workflow; the only addition is the `provider` option on `agent()`.

## When to use it
- **Multi-phase or dynamic** orchestration over many provider subagents: fan-out + barrier, per-item pipeline, loop-until-dry, branch-on-result, with a board run-tree.
- A single flat batch of independent one-shots is **not** a workflow — that's /cc-fleet:subagent. Don't write a script for it.

## The provider ask ladder (ask at most once per task)
1. The user named a provider or model → use it.
2. Else run `cc-fleet default --json`: if it returns a provider (source "configured" or "auto"), use it and STATE it in your kickoff line (e.g. "using glm (default)").
3. Else (several providers, none default) ask the user ONCE which to use — list the enabled providers from `cc-fleet list --json` (name + default_model + the one-line note in cc-fleet-shared/providers.md). After they pick, run `cc-fleet default <chosen>` so you never ask again. (`cc-fleet default <p>` is user-layer; only run it to FILL a blank default, never with --force.)
4. A mid-task provider failure (insufficient balance / rate limit / auth) → STOP, tell the user what happened, propose the next provider, and WAIT for their confirmation. Never switch providers silently.

Model tier within a provider: fan-out / leaf work → omit `--model` (or `--model fast`); judge / synthesis / sustained work → `--model strong`. The provider's roster decides the actual model — see cc-fleet-shared/providers.md.

In a script, `agent()`'s `opts.provider` is **optional**: omitted, the leaf uses the run's default provider, resolved ONCE at launch and recorded with the run — so `--resume` stays stable even if the default changes later. A script meant to be shared or reproducible should still pin `provider` explicitly.

## The script API (mirrors the native Workflow tool)
- `const meta = {name, description, whenToUse?, model?, phases?: [{title, detail?}]}` — a top-level **pure literal** (no calls/vars/spreads; the native `export const meta` form is also accepted). `name` + `description` are **required**; `model` is the default for agents that omit it. Read statically before the run → the board shows the named, phase-skeletoned run immediately.
- `agent(prompt, opts) → Promise<string|object>` — runs ONE provider subagent leaf. `opts.provider` is optional (omitted → the run's default provider, above); `provider: "claude"` runs the official `claude` CLI on the user's OWN Claude Code login (subscription OAuth) instead of a configured provider — a literal `model` id (`opus`/`sonnet`/a full id, omitted → claude's login default, typically the costliest tier so name one), no roster keywords, no key material. The rest are optional: `model`, `schema`, `label`, `phase`, `timeout` (seconds), `max_budget_usd`, `max_turns`, `isolation: "worktree"`, `profile` ("slim" default / "slim-ro" / "full"), `tools`, `skills`, `mcp`. An unknown option key throws (typos fail loudly). On a leaf failure the promise **rejects** — an un-caught top-level `await agent()` aborts the run; inside `parallel`/`pipeline` a failed element degrades to `null`. Leaf failures classify like subagent failures — dispatch table in "Leaf failures" below.
  - **`schema`** (a plain object) goes to the claude child via `--json-schema`: claude injects a forced `StructuredOutput` tool and enforces that it is CALLED (the native mechanism — no JSON instruction is added to the prompt); the promise resolves with the parsed structured payload. The three rules:
    - a validation failure — or a result envelope without a structured payload — FAILS the leaf; there is NO automatic retry;
    - the forced `StructuredOutput` call costs turns — give a schema'd leaf `max_turns` ≥ 3 (a budget of 1 starves it);
    - needs claude ≥ 2.1.88 (the slim-profile floor); an older claude fails the leaf with a classified usage error.
    Client-side validation backstops with a recursive JSON-Schema subset: `type` (object/array/string/number/integer/boolean/null; `integer` accepts `5.0`), `required`, nested `properties`, array `items`, scalar `enum`, string `pattern` (RE2 best-effort — the wire enforces the authoritative ECMA regex) / `format` (email/uri/uuid/date/date-time), `additionalProperties`, `allOf`/`anyOf`/`oneOf`, and intra-document `$ref` (`#/…` pointers; an external URI is unsupported and fails).
  - **`isolation: "worktree"`** runs the leaf with cwd = a fresh git worktree (torn down after), so parallel file-editing leaves don't collide (requires a git repo).
  - **`profile`**: `"slim"` (the default; write-capable) / `"slim-ro"` (read-only research) / `"full"` (ONLY to compare against a full session or diagnose a suspected slim regression). Writes files → `slim`, read-only → `slim-ro`. `tools`, `skills` (default `true`) and `mcp` refine a slim leaf, are rejected with `profile: "full"`, and `tools` REPLACES the whole set, never appends. Tool whitelists / per-profile `mcp` defaults / the pre-2.1.88 fail-open downgrade: cc-fleet-shared/providers.md. The run journal folds the effective profile + tools, so a `--resume` re-runs a leaf whose shape changed.
- **Background = an unawaited promise.** There is no `run_in_background`/`wait()`: start a leaf with `const p = agent(...)`, keep working, `await p` later (`Promise.all` for a batch). Every leaf — awaited or not — is pool-bounded, journaled at completion, and the run only finalizes after all of them settle. A leaf that **rejects with nobody ever handling it fails the run** (a silently dropped failure is still a failure); fire-and-forget tolerance is an explicit `p.catch(() => null)`.
- `parallel(thunks) → Promise<array>` — run each 0-arg thunk concurrently; **BARRIER** (settles once all finish), `null` where an element failed: `await parallel([() => agent("a", {provider: "glm"}), () => agent("b", {provider: "glm"})])`. Concurrent execs stay ~pool size even for a huge list (excess queues).
- `pipeline(items, ...stages) → Promise<array>` — push each item through all stages independently with **NO inter-stage barrier** (item A can be in stage 3 while B is in stage 1). Each stage is `(prev, item, index) => …` (sync or async; its return value is awaited). A failing stage drops that item to `null` and skips its remaining stages. **DEFAULT to `pipeline` over `parallel`** — only use `parallel` when a stage genuinely needs ALL prior results together.
- `workflow(path, args?) → Promise` — run another `.js` inline on the same engine (shared pool/journal/budget), **one level deep** only; resolves with the child's top-level `return` value.
- `budget` — two parallel cap surfaces. **USD:** `budget.total` (the `--budget-usd` cap in USD, or `null`), `budget.spent()`, `budget.remaining()` (`Infinity` when uncapped) — USD floats (an Anthropic list-price estimate). **Tokens:** `budget.tokens_total` (the `--budget-tokens` cap, or `null`), `budget.tokens_spent()`, `budget.tokens_remaining()` — ints (input+output, cache-read excluded). `agent()` throws once **either** cap is reached; a `while (budget.remaining() > N)` loop scales depth to the cap. (Native's `budget.total` is a token target; here it is **USD** — `--budget-usd` is the cross-provider cap since providers price tokens differently — and tokens are the separate `tokens_*` surface.) A `provider: "claude"` leaf spends the **lead session's own subscription window**, not a metered provider — use it for one or two synthesis / judgement nodes, never a wide fan-out. Its usage still flows into the run's token / USD surfaces, but the USD is claude's notional list-price (a subscription is not metered per token); `max_budget_usd` / `--budget-usd` still gate against that notional figure.
- `phase(title, detail?)` — name the current phase (tags subsequent agents lacking an explicit `phase`; the detail shows on the board row). `log(msg)` — a narrator line (board live log + stderr); `console.log/info/warn/error/debug` alias onto it (non-strings render as JSON, Errors by message).
- `args` — the parsed `--args-json '<json>'` value (or the `workflow(child, args)` value); `undefined` when none was given.

## What a workflow script can NOT use (determinism — the journal depends on it)
- `Date` / `Math.random()` **throw**; `eval` / `Function` / dynamic code are removed; there is **no** `setTimeout` / `require` / `fs` / ESM `import` — pass timestamps or randomness in via `args`.
- Plain script statements only (the body runs inside an async wrapper, so top-level `await` and `return` work); async generators (`async function*`) are not supported.

## Running it
```bash
RUN=$(cc-fleet workflow run audit.js)        # detached; prints ONLY the bare run id
cc-fleet workflow status "$RUN" --json       # manifest + every tagged leaf (run→phase→agent)
cc-fleet workflow list --json                # all runs, newest first
cc-fleet workflow stop "$RUN"                # reap a running run (engine + in-flight leaves)
cc-fleet workflow stop "$RUN" --leaf <job|label>  # hold ONE agent in place (run keeps going); --phase <title> holds a phase
cc-fleet workflow restart "$RUN" --leaf <job|label>  # re-run a held/running agent in place; --phase <title> a phase;
                                             # on a FINISHED run: keyed re-run (whole run, --leaf, or --phase)
cc-fleet workflow wait "$RUN" --timeout 3m --json  # block silently until the run settles ("Waiting on a run" below)
# or watch the board's Dynamic Workflows view: live log, token/cost columns, prompt/answer drill-in.
# x/r there are level-scoped: run row = the run, Phases pane = the phase, agent pane = the leaf
# (a held agent shows ▶ until you restart it). --foreground runs inline (debug).
# `held` in status output = parked by the control plane: an operator paused it (board
# x, stop --leaf/--phase) or a restart was refused (budget gate); a restart in flight
# may show it briefly. Not an error/retry/backoff — the run waits on it indefinitely.
# If held persists across polls, resume it with restart --leaf/--phase or tell the
# user it is parked; never wait it out.
# --max-concurrency N overrides the default pool (min(16, cores-2));
# --budget-usd N caps total spend; --no-persist-io disables the prompt/answer drill-in.
```
The run is detached so it outlives this call and your session stays responsive.

## Waiting on a run: arm `wait` in a backgrounded Bash (push, not poll)
Right after launching, arm the notifier — a backgrounded Bash whose EXIT is your wake-up:
```bash
RUN=$(cc-fleet workflow run audit.js)
# Bash tool with run_in_background=true; the harness wakes you when it exits:
cc-fleet workflow wait "$RUN" --timeout 3m --json
```
End your turn and keep working — never spawn an agent (or loop yourself) to poll a run.
On the wake, dispatch on the envelope's `wait_outcome` (+ exit code):
- **`terminal`** (exit 0 done/stopped · 1 failed) — fetch the detail with `workflow status "$RUN" --json` (it carries `run_error` and the per-leaf list; the wait envelope deliberately doesn't) and report.
- **`engine_gone`** (1) — the engine died without finalizing; propose `cc-fleet workflow run <script> --resume "$RUN"` (the journal replays the finished leaves).
- **`parked`** (3) — every remaining leaf is held. FIRST re-check `workflow status`: leaves running/queued again means it was a transient (the engine was between leaves) — re-arm silently. Still parked → name the envelope's `held` leaves to the user and propose `restart --leaf`; never wait it out.
- **`timeout`** (124) — a heartbeat, not a verdict. Compare `counts`/`spent_*` with the previous snapshot: progress → one short progress line and re-arm with a longer window; zero delta → inspect (`workflow status`; is one long leaf still inside its own `timeout`?) and escalate only on a real anomaly, else re-arm.

Window sizing: make the FIRST window short (2–3m — a provider auth/balance failure surfaces on the first leaf call), then 10–15m per re-arm. One wait per run; they are independent. After a session restart, re-arm every `running` run from `workflow list --json`.

For a human live view: `cc-fleet workflow watch "$RUN"` streams the run's events as text (in a terminal, or a backgrounded shell → the `/tasks` panel) and `cc-fleet watch` streams the whole fleet; the board's Dynamic Workflows view has the rich drill-in. Both print only canonical status — never a provider reply.

## Leaf failures — dispatch on `error_code` (do not parse prose)
A failed leaf's `error_code` is in `workflow status --json` (`jobs[]`) and in the rejection that reaches the script. Same vocabulary as a one-shot subagent (the full table with context lives in /cc-fleet:subagent); the dispatch:

| `error_code` | What you do |
|---|---|
| `INSUFFICIENT_BALANCE` / `KEY_INVALID` / `RATE_LIMITED` | STOP — provider ask ladder, step 4 (never switch silently). `KEY_INVALID` → the user rotates the key; `RATE_LIMITED` → brief wait, one retry. |
| `NO_DEFAULT_PROVIDER` / `DEFAULT_PROVIDER_DISABLED` / `DEFAULT_PROVIDER_UNKNOWN` / `DEFAULT_PROVIDER_RESERVED` | No usable default for a provider-less `agent()` (`RESERVED` = `default_provider` hand-set to `claude`, explicit-only — the user unsets/re-pins) — apply the provider ask ladder, then re-run. |
| `MODEL_NOT_FOUND` | `cc-fleet refresh <provider>`, or drop the leaf's `model` to use the provider default. |
| `SUBAGENT_TIMEOUT` | Raise the leaf's `timeout` or split the task; awaited background leaves are backstopped at 45m. |
| `SUBAGENT_OUTPUT_TOO_LARGE` | The leaf's output exceeded the byte cap — have it write to a file and answer concisely; a blind retry overflows again. |
| `SUBAGENT_STOPPED` | An operator stopped it (`stop --leaf` / run stop) — terminal, NOT a failure; never auto-retry. |
| `SUBAGENT_FAILED` / `PROVIDER_API_ERROR` | Inspect (`workflow status`); `restart --leaf` once, or propose a provider switch (ask first). A `provider: "claude"` leaf on a logged-out machine fails here (the error preview names the login problem, no dedicated code) — tell the user to log in to Claude Code interactively. |
| `FINGERPRINT_MISSING` / `FINGERPRINT_STALE` | Self-heal flow in cc-fleet-shared/troubleshooting.md (`STALE` = no claude binary — the flow can't help; fix Claude Code / PATH). |
| `CODEX_PROXY_UNAVAILABLE` / `CODEX_CLOUDFLARE_BLOCKED` | `cc-fleet codex login` / free the port; a Cloudflare block → switch network, don't rotate credentials. |
| `UNKNOWN_PROVIDER` / `PROVIDER_DISABLED` / `CONFIG_LOAD_FAILED` | Config problem — `cc-fleet list --json`, `cc-fleet add` / `edit --enable`; `CONFIG_LOAD_FAILED` → `cc-fleet doctor`. |
| `PROVIDER_RESERVED` | A providers.toml row is named `claude` (reserved for the native leaf) — the user renames or removes it. |
| `SUBAGENT_BAD_ARGS` | Bad leaf options — fix the script, re-run. |

## Resume (content-hash journal)
Each run records a content-hash **journal** of its completed leaves. Re-run the same script under an existing run id to replay:
```bash
cc-fleet workflow run audit.js --resume "$RUN"   # journaled leaves return cached (no provider exec); only un-run leaves run
```
A leaf is keyed by its determinant (provider + model + prompt + schema + slim shape), so an unchanged re-run is ~100% cache hits, a leaf whose prompt you edited (and anything downstream of its output) re-runs, and a run that was killed resumes by replaying what finished before the kill. The determinism lockdown makes this exact: with no clock/PRNG, the same script+args produce the same keys. A **failed** leaf is never journaled, so resume re-runs it.

## Non-goals (state plainly, don't oversell)
- **No pause.** A running `claude -p` can't be cleanly suspended; use `workflow stop` (reaps the run) + `run --resume` (cheap restart via the journal) instead.
- **Client-side `schema` validation is a JSON-Schema subset** — the list above, not the full spec (an external `$ref` URI is unsupported and fails; an unknown `format` is an annotation, not enforced). claude enforces that `StructuredOutput` is called; this backstop checks what it was filled with, and a failure is terminal (no retry).
- Key-safety is unchanged: the provider key flows only via `apiKeyHelper`; prompts go to the leaf via stdin, never argv; the journal/events/board carry no key.

## Worked example — research sweep (fan-out → pipeline → loop)
```js
const meta = {
    name: "api audit",
    description: "map endpoints, draft checks, then probe for gaps",
    phases: [{title: "map"}, {title: "build"}, {title: "probe"}],
};

phase("map");
const maps = (await parallel(
    args.map((m) => () => agent("List exported endpoints in module " + m,
                                {provider: "deepseek", label: "map:" + m}))
)).filter(Boolean);  // e.g. --args-json '["auth","billing","users"]'

phase("build");
// pipeline (no barrier): each map flows straight into its own checklist draft
const checklists = await pipeline(
    maps,
    (prev, item, i) => agent("Draft an audit checklist for these endpoints:\n" + prev,
                             {provider: "glm", label: "build:" + i}),
);

phase("probe");
const gaps = [];
while (gaps.length < 10) {           // loop-until-dry (the runtime hard-caps 1000 leaves/run)
    const g = await agent("Given these checklists, name ONE uncovered risk, or reply NONE:\n"
                          + checklists.join("\n"), {provider: "kimi"});
    if (g.trim() === "NONE") break;
    gaps.push(g);
}

// one final synthesis node on your OWN subscription — a single judgement leaf, not a fan-out
const verdict = await agent("Rank these gaps by severity and name the top three:\n"
                            + gaps.join("\n"), {provider: "claude", model: "opus"});

log(`done: ${maps.length} maps, ${checklists.length} checklists, ${gaps.length} gaps`);
return { maps, checklists, gaps, verdict };
```
One run, three phases, a barriered fan-out, a no-barrier pipeline, a bounded loop-until-dry, and a single `claude` synthesis node — all sequenced by the script in a cc-fleet process, off your context.

## Anti-patterns
- A script for a single flat independent batch → /cc-fleet:subagent.
- A long-lived collaborator you message back and forth → /cc-fleet:team.
- `Date.now()` / `setTimeout` — unavailable (determinism); pass timestamps via `args`.
- Trusting `schema` as deep validation, or treating a plain `agent()` result as JSON without `schema`.
- Unbounded ambition: the runtime hard-caps 1000 `agent()` calls/run, pools concurrency at `min(16, cores-2)`, and caps a single `parallel`/`pipeline` list at 100,000 elements.
- Switching providers silently after a balance / rate-limit / auth failure → stop, tell the user, wait for their pick (provider ask ladder, step 4).
