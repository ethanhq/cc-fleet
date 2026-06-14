# Writing workflows

A workflow is a JavaScript file that orchestrates provider subagents — fan-out, pipelines, loops, barriers — executed by `cc-fleet workflow run <script.js>` in a **detached engine**, so the run outlives your terminal and costs a Claude session none of its context. The API mirrors Claude Code's native Workflow tool; the one addition is the `provider` option on `agent()`.

This page is the script-API reference. Command flags and exit codes live in [the CLI reference](cli.md#workflows); how the engine works (journal, held leaves, the `wait` push) is in [architecture.md](architecture.md).

## A first script

```js
const meta = {
  name: "api audit",
  description: "map endpoints, then draft audit checklists",
  phases: [{ title: "map" }, { title: "build" }],
};

phase("map");
const maps = (await parallel(
  args.map((m) => () => agent("List the exported endpoints in module " + m,
                              { provider: "deepseek", label: "map:" + m }))
)).filter(Boolean);

phase("build");
const checklists = await pipeline(maps,
  (endpoints, _, i) => agent("Draft an audit checklist:\n" + endpoints,
                             { provider: "glm", label: "build:" + i }));
return { checklists };
```

```bash
RUN=$(cc-fleet workflow run audit.js --args-json '["auth","billing","users"]')
cc-fleet workflow status "$RUN" --json    # run → phase → leaf tree, spend, budget
cc-fleet workflow wait "$RUN" --timeout 10m   # block until it settles
```

## The API

- **`meta`** — a top-level **pure literal** (no calls, variables, or spreads): `{name, description, whenToUse?, model?, phases?: [{title, detail?}]}`. `name` and `description` are required. It is read statically before the run starts, so the board shows the named, phase-skeletoned run immediately.
- **`agent(prompt, opts) → Promise<string|object>`** — one provider-subagent leaf. All options are optional: `provider` (omitted → the run's default provider, resolved once at launch and recorded with the run; `provider: "claude"` runs the leaf on your own Claude Code login — literal `model` ids only, explicit-only, bills your subscription, so keep it to a synthesis node or two), `model` (`"strong"` / `"fast"` / a literal id), `schema`, `label`, `phase`, `timeout` (seconds; a leaf with no timeout defaults to 300s), `max_budget_usd`, `max_turns`, `isolation: "worktree"` (run in a fresh git worktree so parallel file-editing leaves don't collide), `profile` (`"slim"` default / `"slim-ro"` read-only / `"full"` diagnostic), `tools` (replaces the whole whitelist, never appends), `skills`, `mcp`. An unknown option key throws — typos fail loudly. On a leaf failure the promise **rejects**: an uncaught top-level `await agent()` aborts the run; inside `parallel`/`pipeline` a failed element degrades to `null`.
- **Background = an unawaited promise.** There is no `run_in_background`: start a leaf with `const p = agent(...)`, keep going, `await p` later. Every leaf — awaited or not — is pool-bounded and journaled, and the run only finalizes after all of them settle. A leaf that rejects with nobody handling it **fails the run**; fire-and-forget tolerance is an explicit `p.catch(() => null)`.
- **`parallel(thunks) → Promise<array>`** — run 0-arg thunks concurrently; a **barrier** (settles when all finish), `null` where an element failed.
- **`pipeline(items, ...stages) → Promise<array>`** — push each item through all stages independently, **no inter-stage barrier** (item A can be in stage 3 while B is in stage 1). Each stage is `(prev, item, index) => …`. A failing stage drops that item to `null`. **Default to `pipeline`**; use `parallel` only when a stage genuinely needs all prior results together.
- **`workflow(path, args?) → Promise`** — run another `.js` inline on the same engine (shared pool, journal, budget), one level deep only.
- **`budget`** — two cap surfaces. USD (`--budget-usd`): `budget.total`, `budget.spent()`, `budget.remaining()`. Tokens (`--budget-tokens`): `budget.tokens_total`, `budget.tokens_spent()`, `budget.tokens_remaining()`. `agent()` throws once **either** cap is reached, so `while (budget.remaining() > N) { … }` scales depth to the cap.
- **`phase(title, detail?)`** — name the current phase (tags subsequent agents); **`log(msg)`**— a narrator line on the board's live log (`console.*` alias onto it).
- **`args`** — the parsed `--args-json` value (or the value passed by a parent `workflow()` call); `undefined` when none was given.

### Structured output (`schema`)

Pass a plain JSON-Schema object and the leaf's `claude` child is forced to call a `StructuredOutput` tool; the promise resolves with the parsed payload. Three rules:

- a validation failure fails the leaf — there is **no automatic retry**;
- the forced call costs turns — give a schema'd leaf `max_turns` ≥ 3;
- it needs claude ≥ 2.1.88; older versions fail the leaf with a classified error.

The client-side validator covers a JSON-Schema subset: `type`, `required`, nested `properties`, array `items`, `enum` (members may be any JSON value, compared by deep equality), string `pattern`/`format`, `additionalProperties`, `allOf`/`anyOf`/`oneOf`, and intra-document `$ref` (`#/…`; external URIs are unsupported). The forced `StructuredOutput` tool is profile-independent — it survives a custom slim `tools` whitelist, so a schema'd leaf works regardless of `tools`.

## Determinism (and why)

`new Date()` with no arguments and `Date.now()` **throw** (`Date.parse` / `Date.UTC` / `new Date(…args)` still work); `Math.random()` **throws**. `eval`, `Function`, `setTimeout`, `require`, `fs`, and ESM `import` don't exist. Pass timestamps or randomness in via `args`. This is what makes the journal exact: with no clock or PRNG, the same script + args always produce the same leaf keys.

## Resume — the content-hash journal

Every completed leaf is journaled, keyed by its determinant (provider + model + prompt + schema + profile shape):

```bash
cc-fleet workflow run audit.js --resume "$RUN"
```

Unchanged leaves return cached (no provider call); a leaf you edited — and everything downstream of its output — re-runs. A killed run resumes by replaying what finished — and any re-run reusing the same run id resumes against the journal, `--resume` being the explicit form. Failed leaves are never journaled, so a resume always retries them.

## Live control

```bash
cc-fleet workflow stop "$RUN" --leaf <job-or-label>     # hold ONE leaf; the run keeps going
cc-fleet workflow restart "$RUN" --leaf <job-or-label>  # re-run it in place (attempt +1)
cc-fleet workflow stop/restart "$RUN" --phase <title>   # same, per phase
```

A held leaf is parked indefinitely — not an error, never auto-retried; its `agent()` promise simply stays unsettled until you restart it. The TUI board's Dynamic Workflows view does the same per row (`x` / `r`) with prompt/answer drill-in and per-leaf spend.

## Waiting on a run

`cc-fleet workflow wait "$RUN" --timeout 10m` blocks silently and exits exactly once — `0` done/stopped, `1` failed or engine-gone, `3` parked (every remaining leaf is held), `124` timeout heartbeat, `130` interrupt, `2` IO/unknown-run error. Run it in a backgrounded shell and its exit is your completion notification; no polling loop needed.

## Limits

- 1,000 `agent()` calls per run; a single `parallel`/`pipeline` list caps at 100,000 items.
- Concurrency pools at `min(16, cores − 2)` (override with `--max-concurrency`).
- No pause: a running `claude -p` can't be cleanly suspended — `stop` + `--resume` is the cheap equivalent, courtesy of the journal.
- Key safety is unchanged: prompts reach leaves via stdin, keys only via `apiKeyHelper`, and the journal/events/board never carry either.
