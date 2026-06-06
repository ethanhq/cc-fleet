# Workflow runtime — Starlark orchestration over vendor subagents

A **workflow** is a Starlark script that fans out vendor `cc-fleet subagent` leaves and
runs in a **cc-fleet process, OFF the main session's context**. You write the script;
`cc-fleet workflow run` executes it. The orchestration plan lives in script variables
(CPU, ~0 of your tokens) — you are invoked only when *authoring* the script, not on
every scheduling decision. This mirrors the native Claude Code Workflow tool; the only
differences are `agent()` takes a `vendor=`, and the script is **Starlark** (Python-ish),
not JS.

## When to use it
- **Multi-phase or dynamic** orchestration over many vendor subagents: fan-out + barrier,
  per-item pipeline, loop-until-dry, branch-on-result, with a board run-tree.
- A single flat batch of independent one-shots is **not** a workflow — that's a lane-2
  `cc-fleet subagent` batch (`references/subagent.md`). Don't write a script for it.

## The script API (predeclared; mirrors the native Workflow tool)
- `meta = {"name": …, "description": …, "whenToUse": …, "model": …, "phases": [{"title": …,
  "detail": …}, …]}` — a top-level **pure literal** (no calls/vars). `name` + `description`
  are **required**; `whenToUse` (board text) and `model` (the default model for agents that
  omit `model=`) are optional; `phases` is the declared plan (optional). Read statically
  before the run → the board shows the named, phase-skeletoned run immediately.
- `agent(prompt, vendor=…, model=None, schema=None, label=None, phase=None, timeout=None,
  max_budget_usd=None, max_turns=None, run_in_background=False, isolation=None,
  profile="full", tools=None, skills=True, mcp=False)` — runs ONE
  vendor subagent leaf and **blocks** until it returns the answer **string**. With `schema=`
  (a dict) it asks for JSON and validates it against a real (recursive) JSON-Schema subset —
  `type` (object/array/string/number/integer/boolean/null; `integer` accepts `5.0`),
  `required`, nested `properties`, array `items`, scalar `enum` — **retrying up to twice**,
  then returns the parsed value. (Composition keywords `$ref`/`allOf`/`oneOf` are NOT
  enforced.) On a leaf failure it **raises** — a bare top-level `agent()` aborts the run;
  inside `parallel`/`pipeline` it becomes `None`. Omitting `model` uses `meta.model` then the
  vendor's `default_model`; `timeout=` (seconds) and `max_budget_usd=` accept int or float.
  `run_in_background=True` returns a **handle** immediately (await it with `wait()`); not
  combinable with `schema=`. `isolation="worktree"` runs the leaf with cwd = a fresh git
  worktree (torn down after), so parallel file-editing leaves don't collide (requires a git repo).
  `profile="slim"` (generic-subagent mirror; keeps CLAUDE.md) or `"slim-ro"` (read-only
  Explore mirror; no CLAUDE.md) swaps the full session prompt for the native subagent shape —
  a far smaller first request per leaf; rule of thumb: writes files → `slim`, read-only
  research → `slim-ro`. `tools=` (whitelist), `skills=` (default `True`), `mcp=` (default
  `False`) refine a slim leaf and are rejected with `profile="full"`. The run journal folds
  the effective profile + tools, so a `--resume` re-runs a leaf whose shape changed; on a
  claude below 2.1.88 the leaf fails open to `full` (notice logged before the journal lookup).
- `wait(handle | [handles])` — block for one background handle (returns its string) or a
  list of them (returns a list, order preserved). **Named `wait`, not `await`** — Starlark
  reserves `await` as a keyword.
- `parallel(thunks)` — run each 0-arg thunk concurrently; **BARRIER** (returns once all
  finish) as a list, `None` where a thunk failed. `thunks` are **functions**:
  `parallel([lambda: agent("a", vendor="glm"), lambda: agent("b", vendor="glm")])`. Live
  goroutines stay ~pool size even for a huge list (excess queues).
- `pipeline(items, *stages)` — push each item through all stages independently with **NO
  inter-stage barrier** (item A can be in stage 3 while B is in stage 1). Each stage is
  `lambda prev, item, index: …`. A failing stage drops that item to `None`.
  **DEFAULT to `pipeline` over `parallel`** — only use `parallel` when a stage genuinely
  needs ALL prior results together.
- `workflow(script_path, args=None)` — run another `.star` inline on the same engine
  (shared pool/journal/budget), **one level deep** only. Returns the child's module global
  `result` (Starlark module bodies have no top-level `return`), or `None`.
- `budget` — `budget.total` (the `--budget-usd` cap, or `None`), `budget.spent()`,
  `budget.remaining()` (`+inf` when uncapped). `agent()` raises once `spent() >= total`; a
  `for`-loop guarded by `budget.remaining()` scales depth to the cap.
- `phase(title, detail=None)` — name the current phase (tags subsequent agents lacking an
  explicit `phase=`; the detail shows on the board row). `log(msg)` — a narrator line (board
  live log + stderr).
- `args` — predeclared when you pass `--args-json '<json>'` (or `workflow(child, args=…)`).

## Starlark idioms you must use (the syntax diffs from native JS)
- Thunks are **`lambda: …`**; closures must NOT mutate shared state — return values
  instead. The thunks/items you pass to `parallel`/`pipeline` (and anything they capture)
  are **frozen before dispatch**, so a thunk that mutates shared captured state — or code
  that mutates the passed list afterward — raises a "cannot mutate frozen" error (inside a
  thunk that surfaces as `None`). Collect return values, don't accumulate into a shared box.
- **No `while`.** Loop-until-dry is a bounded `for`:
  ```python
  found = []
  for _ in range(20):              # bounded — the runtime also hard-caps 1000 leaves/run
      r = agent("probe for the next gap", vendor="deepseek")
      if not r: break
      found.append(r)
  ```
- Drop failures (the `.filter(Boolean)` equivalent):
  ```python
  ok = [r for r in parallel([lambda: agent(p, vendor="glm") for p in prompts]) if r != None]
  ```

## Running it
```bash
RUN=$(cc-fleet workflow run audit.star)      # detached; prints ONLY the bare run id
cc-fleet workflow status "$RUN" --json       # manifest + every tagged leaf (run→phase→agent)
cc-fleet workflow list --json                # all runs, newest first
cc-fleet workflow stop "$RUN"                # reap a running run (engine + in-flight leaves)
# or watch the board's Workflows view: live log, token/cost columns, prompt/answer drill-in,
# x = stop, r = restart (= run --resume). --foreground runs inline (debug).
# --max-concurrency N overrides the default pool (min(16, cores-2));
# --budget-usd N caps total spend; --no-persist-io disables the prompt/answer drill-in.
```
The run is detached so it outlives this call and your session stays responsive; poll
`workflow status` or watch the board.

To surface a detached run INSIDE this session (the run is otherwise invisible here):
```bash
cc-fleet workflow watch "$RUN"               # stream its live events as text until it finishes
cc-fleet watch                               # stream the whole fleet (teammates + jobs + runs)
```
Run either in a backgrounded shell to surface it in the `/tasks` panel, or delegate the
`cc-fleet:workflow-watch` agent (a run id) / `cc-fleet:fleet-watch` agent (the fleet) to surface
it in the agent panel. Both are read-only and print only canonical status — never a vendor reply.

## Resume (content-hash journal)
Each run records a content-hash **journal** of its completed leaves. Re-run the same
script under an existing run id to replay:
```bash
cc-fleet workflow run audit.star --resume "$RUN"   # journaled leaves return cached (no vendor exec); only un-run leaves run
```
A leaf is keyed by its determinant (vendor + model + prompt + schema), so an unchanged
re-run is ~100% cache hits, a leaf whose prompt you edited (and anything downstream of its
output) re-runs, and a run that was killed resumes by replaying what finished before the
kill. Determinism makes this exact: Starlark has no clock/PRNG, so the same script+args
produce the same keys. A **failed** leaf is never journaled, so resume re-runs it.

## Non-goals (state plainly, don't oversell)
- **No pause.** A running `claude -p` can't be cleanly suspended; use `workflow stop` (reaps
  the run) + `run --resume` (cheap restart via the journal) instead.
- **`schema=` is a practical subset** — `type`/`required`/nested `properties`/array
  `items`/scalar `enum`, not the full JSON-Schema spec (`$ref`/`allOf`/`oneOf` are ignored).
- **No deep `$ref`/composition.** Keep schemas concrete.
- Key-safety is unchanged: the vendor key flows only via `apiKeyHelper`; prompts go to the
  leaf via stdin, never argv; the journal/events/board carry no key.

## Worked example — research sweep (fan-out → pipeline → loop)
```python
meta = {
    "name": "api audit",
    "description": "map endpoints, draft checks, then probe for gaps",
    "phases": [{"title": "map"}, {"title": "build"}, {"title": "probe"}],
}

phase("map")
maps = [r for r in parallel([
    lambda: agent("List exported endpoints in module " + m, vendor="deepseek", label="map:" + m)
    for m in args  # e.g. --args-json '["auth","billing","users"]'
]) if r != None]

phase("build")
# pipeline (no barrier): each map flows straight into its own checklist draft
checklists = pipeline(
    maps,
    lambda prev, item, i: agent("Draft an audit checklist for these endpoints:\n" + prev,
                                vendor="glm", label="build:%d" % i),
)

phase("probe")
gaps = []
for _ in range(10):                 # loop-until-dry, bounded
    g = agent("Given these checklists, name ONE uncovered risk, or reply NONE:\n"
              + "\n".join(checklists), vendor="kimi")
    if g.strip() == "NONE": break
    gaps.append(g)

log("done: %d maps, %d checklists, %d gaps" % (len(maps), len(checklists), len(gaps)))
```
One run, three phases, a barriered fan-out, a no-barrier pipeline, and a bounded
loop-until-dry — all sequenced by the script in a cc-fleet process, off your context.

## Anti-patterns
- A script for a single flat independent batch → use lane-2 `cc-fleet subagent`.
- Thunks that append to a shared list instead of returning values → they hit the frozen
  guard and become `None`; collect return values instead.
- Trusting `schema=` as deep validation, or `.result` as JSON without `schema=`.
- Unbounded ambition: the runtime hard-caps 1000 `agent()` calls/run (a schema agent may
  do up to 3 vendor calls across its retries) and pools concurrency at `min(16, cores-2)`;
  a single `parallel`/`pipeline` list is likewise capped at 1000 elements.
