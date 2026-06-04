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
- `meta = {"name": …, "description": …, "phases": [{"title": …, "detail": …}, …]}` — a
  top-level **pure literal** (no calls/vars). `name` + `description` are **required**;
  `phases` is the declared plan (optional). Read statically before the run → the board
  shows the named, phase-skeletoned run immediately.
- `agent(prompt, vendor=…, model=None, schema=None, label=None, phase=None, timeout=None,
  max_budget_usd=None, max_turns=None)` — runs ONE vendor subagent leaf and **blocks**
  until it returns the answer **string**. With `schema=` (a dict) it asks for JSON, parses
  it, checks the schema's top-level `required`/`properties` keys, **retries up to twice**,
  and returns the parsed value. `schema=` is normally a dict; any JSON-encodable value
  works (a non-dict → valid-JSON-only validation). On a leaf failure it **raises** — a
  bare top-level `agent()` aborts the run; inside `parallel`/`pipeline` it becomes `None`.
  Every arg after `vendor` is optional — omit it or pass `None` (omitting `model` uses the
  vendor's `default_model`); `timeout=` (seconds) and `max_budget_usd=` accept an int or a float.
- `parallel(thunks)` — run each 0-arg thunk concurrently; **BARRIER** (returns once all
  finish) as a list, `None` where a thunk failed. `thunks` are **functions**:
  `parallel([lambda: agent("a", vendor="glm"), lambda: agent("b", vendor="glm")])`.
- `pipeline(items, *stages)` — push each item through all stages independently with **NO
  inter-stage barrier** (item A can be in stage 3 while B is in stage 1). Each stage is
  `lambda prev, item, index: …`. A failing stage drops that item to `None`.
  **DEFAULT to `pipeline` over `parallel`** — only use `parallel` when a stage genuinely
  needs ALL prior results together.
- `phase(title, detail=None)` — name the current phase (tags subsequent agents lacking an
  explicit `phase=`; the detail shows on the board row). `log(msg)` — a narrator line (stderr).
- `args` — predeclared when you pass `--args-json '<json>'`.

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
# or watch the board's Workflows view (live run tree). --foreground runs inline (debug).
# --max-concurrency N overrides the default pool (min(16, cores-2)).
```
The run is detached so it outlives this call and your session stays responsive; poll
`workflow status` or watch the board.

## Non-goals (v1 — state plainly, don't oversell)
- **No journal / resume.** If the run process dies, in-flight work is lost; the manifest
  + already-tagged leaves persist, but the schedule does not resume.
- **No live stop.** An in-flight leaf is bounded only by its per-agent `timeout` (default
  300s) — size `timeout=` deliberately.
- **Shallow schema.** `schema=` validates valid-JSON + top-level required keys + retries;
  it is NOT full JSON-Schema enforcement. Don't rely on deep validation.
- Key-safety is unchanged: the vendor key flows only via `apiKeyHelper`; prompts go to the
  leaf via stdin, never argv.

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
