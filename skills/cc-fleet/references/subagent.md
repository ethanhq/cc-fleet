# Lane 2 — one-shot vendor subagent (synchronous, headless, no pane)

Read this when the task is a **one-shot** vendor offload (the user named a vendor for a one-time job, or a batch fan-out of independent one-shot tasks). The analog of the native `Agent`/`Task` tool, but the model can be a vendor id.

`cc-fleet subagent` has **no** tmux pane, needs **no** `TeamCreate`/`SendMessage`/`TeamDelete`, and returns the result directly on Bash stdout. It reuses the same vendor selection and the same fingerprint self-heal flow as spawn (run it on `FINGERPRINT_MISSING`; `FINGERPRINT_STALE` = no claude binary, which a probe can't fix). It's the lightweight synchronous branch.

## Contents
- When to use subagent instead of a teammate
- Calling it (+ session grouping, useful flags)
- Success / failure envelopes (dispatch on `error_code`)
- Batch fan-out
- Long tasks: `--background` + `subagent-status`
- Multi-turn: `--resume`
- Cleanup vs. resume
- Anti-patterns

## When to use subagent instead of a teammate
- **One-shot research / analysis / judgement** — you want an answer, not a long-lived colleague.
- **Batch fan-out** — N independent tasks in parallel. subagent is **lock-free**, so N calls don't serialize behind a server lock the way N spawns do — true parallelism.
- **Cost-bounded probes** — `--timeout` caps wall-clock; the return value carries `usage` / `total_cost_usd`; it stops when done (no teammate to forget to tear down).

→ Need multi-turn / collaboration / a pane to watch / session continuity → use `cc-fleet spawn` (teammate, see `references/teammates.md`) instead.

## Calling it (run via Bash, always with `--json`)
```bash
cc-fleet subagent <vendor> --model <m> --prompt "<task>" --json
# e.g.
cc-fleet subagent deepseek --model deepseek-reasoner \
  --prompt "Analyze the worst-case complexity of quicksort in src/sort.go; give a triggering input" --json
```

**Session grouping:** `cc-fleet subagent` auto-detects the current parent Claude session when it is launched from a Claude Bash tool, so standalone subagents normally group under the current Agents Board session without any extra flag. When you are working inside a known team, you may still pass the explicit team session id from `~/.claude/teams/<team>/config.json` (`leadSessionId`); explicit `--lead-session-id` wins over auto-detection and is the safest way to force a job to match that team's teammates. Auto-detection is fail-closed: if the parent session registry cannot be validated, the job appears under `(no session)` instead of guessing.

```bash
# Optional explicit override in cc-fleet context, with a known team:
lead_session_id=$(jq -r '.leadSessionId // empty' "$HOME/.claude/teams/<team>/config.json")
cc-fleet subagent deepseek --prompt "..." --lead-session-id "$lead_session_id" --json
```

Useful flags:
- **Name it** → `--label "<short-alias>"` (e.g. `--label sort-complexity`). The Agents Board shows the label instead of the opaque job id — pass one on every launch, like a teammate name. Display-only metadata; capped at 256 bytes.
- **Large / sensitive prompt** → `--prompt-file <path>` (read from file, piped via stdin, kept out of argv / `ps`). Use it once a single prompt approaches **~128 KiB** (`MAX_ARG_STRLEN`, the per-argument cap — not the ~2 MB total `ARG_MAX`). `--prompt-file -` reads stdin.
- **Long task** → `--timeout 600s` (default 300s). For tasks that may exceed the timeout, prefer `--background` (below). Note: a vendor that's down on **auth (401) or quota (429)** makes claude retry **~180s** before surfacing `KEY_INVALID` / `INSUFFICIENT_BALANCE`, so keep `--timeout ≥ ~200s` (the 300s default is fine) — a shorter timeout reports those as `SUBAGENT_TIMEOUT` instead. `--probe` does **not** catch a bad key (the models endpoint may not 401 it).
- **Cost / runaway gates** → `--max-budget-usd 0.5` (cap spend) and `--max-turns 8` (cap the agentic tool loop). On fan-out, strongly consider passing these on every call.
- **Prompt profile — `slim` is the DEFAULT** → `slim` (generic-subagent mirror: keeps CLAUDE.md + gitStatus, write-capable) and `--profile slim-ro` (read-only Explore mirror: no CLAUDE.md, advisory read-only) replace the full session prompt with the native subagent shape and a restricted tool whitelist — a far smaller first request, which cache-less vendors pay per call. Default tool sets: `slim` = Bash, Edit, Glob, Grep, Read, Skill, Write; `slim-ro` = Bash, Glob, Grep, Read, Skill. Any tool beyond the whitelist (e.g. WebSearch / WebFetch) must be passed explicitly via `--tools`, and `--tools` REPLACES the whole set, never appends — `--tools "WebSearch"` gives the subagent ONLY WebSearch. Rule of thumb: the leaf writes files → `slim`; read-only research → `slim-ro`; `--profile full` restores the full session prompt — use it ONLY to compare behavior against a full session or to diagnose a suspected slim regression. Refine with `--tools "Bash,Read,Grep"` (whitelist), `--skills=false` (drop the Skill tool + host skill listing), `--mcp` (default is per-profile: `slim` inherits the host MCP config — native parity — while `slim-ro` runs `--strict-mcp-config`; an explicit `--mcp` / `--mcp=false` overrides). `--tools` / `--skills=false` / `--mcp` are slim-only — combined with `--profile full` they are rejected. On a claude below 2.1.88 the profile fails open to `full` and the envelope carries `slim_downgrade`. Weak vendor models skip tools on weak-imperative prompts under **any** profile — write prescriptive prompts ("Run `cmd`", "Use the Read tool on X"), not "look at"/"check".
- **Probe** is **off by default** (`--probe` to opt in): the inner `claude -p` call is itself the authoritative reachability + auth test. On a big fan-out, run one shared `cc-fleet doctor` / probe up front rather than paying 3s × N.
- `--prompt` and `--prompt-file` are mutually exclusive — pass exactly one (else `error_code=SUBAGENT_BAD_ARGS`, no claude launched).

## Success envelope
```json
{"ok":true,"result":"<answer text>","vendor":"deepseek","model":"deepseek-reasoner",
 "duration_ms":12044,"usage":{"input_tokens":812,"output_tokens":1530},
 "total_cost_usd":0.0031,"session_id":"…"}
```
→ Take `.result` as this subagent's output and hand it back / continue orchestrating. `model` is the model the vendor actually billed (routing evidence). Keep `.session_id` if you intend a multi-turn follow-up (see below).

## Failure envelope — dispatch on `error_code` (do not parse prose)
`error_msg` is a canonical string only; never matched on. Dispatch on `error_code`:

| `error_code` | Meaning | What you do |
|---|---|---|
| `SUBAGENT_BAD_ARGS` | Missing/both `--prompt` & `--prompt-file`. | Fix the call (exactly one). |
| `UNKNOWN_VENDOR` / `VENDOR_DISABLED` | Vendor not configured / disabled. | Tell the user to `cc-fleet add` / `cc-fleet edit <vendor> --enable`. |
| `FINGERPRINT_MISSING` | An existing `fingerprint.json` is corrupt (a fresh install uses the bundled recipe, so this is rare). | Run the **same self-heal flow** as spawn (`references/troubleshooting.md`), then retry. |
| `FINGERPRINT_STALE` | No `claude` binary found anywhere (not a missing recipe). | Tell the user to install/fix Claude Code or PATH; the self-heal flow can't help. `cc-fleet doctor` confirms. |
| `KEY_INVALID` | Vendor 401/403. | Have the user rotate the key; do not retry blindly. |
| `INSUFFICIENT_BALANCE` | Out of balance / quota (429/402 + balance signature). | Retry can't help — switch vendor or fall back to native `Agent`; tell the user they're out of credit. |
| `RATE_LIMITED` | Vendor 429. | Wait briefly, retry once, or switch vendor. |
| `MODEL_NOT_FOUND` | Model name rejected (400). | `cc-fleet refresh <vendor>` then retry, or drop `--model` to use the default. |
| `VENDOR_UNREACHABLE` | Transport failure (only with `--probe`). | `cc-fleet doctor`; if urgent, fall back to native `Agent`. |
| `SUBAGENT_TIMEOUT` | Exceeded `--timeout`. | Real long task → raise `--timeout` (or use `--background`) and retry; suspected hang → switch vendor / fall back. |
| `VENDOR_API_ERROR` | Other vendor failure (5xx / overloaded). | Retry once or switch vendor. |
| `SUBAGENT_FAILED` | claude exited with no parseable result (or turn/budget exhaustion). | Inspect; retry or switch vendor. |

## Batch fan-out example (parallel, each returns synchronously)
```bash
# These Bash calls are independent and can fire in parallel; subagent is
# lock-free, so they don't queue behind each other.
cc-fleet subagent glm      --prompt "Summarize docs/a.md" --json
cc-fleet subagent glm      --prompt "Summarize docs/b.md" --json
cc-fleet subagent deepseek --prompt "Summarize docs/c.md" --json
# Each returns its own {ok, result, total_cost_usd}; aggregate them. No
# TeamCreate / TeamDelete needed.
```

## Long tasks: `--background` + `subagent-status` (poll, not push)
A subagent is a separate process and **cannot push a notification to you** (unlike native Agent, which is in-process). For a task that may run longer than you want to block a Bash call:
```bash
cc-fleet subagent <vendor> --prompt "<long task>" --background --json
# → {"ok":true,"job_id":"<uuid>","status":"running","output_file":"…","pid":…}
# later, poll:
cc-fleet subagent-status <job_id> --json
# → {"status":"running"}  … then …  {"ok":true,"status":"done","result":"…", …}
```
This is a **poll** model: re-check `subagent-status` after a while; there is no idle notification. (Need push-on-done → that's a teammate's job.) `cc-fleet subagent-gc --json` prunes finished job files.

## Multi-turn: `--resume`
Continue a prior subagent session (stateful, but not long-lived between turns — each turn is a fresh `claude -p --resume`):
```bash
cc-fleet subagent <vendor> --resume <session_id> --prompt "<follow-up>" --json
```
`<session_id>` is the `.session_id` from the previous turn's envelope. A default-profile (slim) resume is silent; an explicitly passed `--profile` over `--resume` warns on stderr — it swaps the system prompt mid-session. Keep the profile constant across a session's turns.

## Cleanup vs. resume — they're independent
A one-shot **sync** subagent is just a process that exits — no pane, no team, **nothing to tear down**. "Cleanup" only ever concerns **`--background` job records** on the Agents Board:

- **Finished → safe to prune.** `cc-fleet subagent-gc --json` removes *finished* background job files (default: only those older than 24h; **`cc-fleet subagent-gc --older-than 0s` clears all finished now**). Running jobs are always kept.
- **Pruning does NOT end the conversation.** gc only deletes cc-fleet's bookkeeping under `~/.config/cc-fleet/subagent-jobs/`; it never touches Claude's session transcript (`~/.claude/projects/…`). So **`--resume <session_id>` still works after gc** — *as long as you kept the `session_id`*. That id lives in the result envelope, which gc removes with the job, so **if a follow-up is likely, capture `.session_id` before pruning** (or just leave the job until you're done resuming).
- The flip side: **leaving the job record does not by itself let you resume** — resume needs the `session_id` (plus Claude's own session retention), not the cc-fleet record. The way to preserve a follow-up is *recording the session_id*, not *skipping cleanup*.

## Anti-patterns (subagent-specific)
- Using subagent for work that needs multiple turns / collaboration → use a teammate.
- `TeamCreate` / `SendMessage` / polling `cc-fleet ps --check` for a subagent → unnecessary; the result is on stdout.
- Stuffing a giant prompt into `--prompt` (hits `MAX_ARG_STRLEN` ~128 KiB) → use `--prompt-file`.
- Running a possibly-stuck vendor with no bound → the default `--timeout 300s` caps it, but tune per task on fan-out, and use `--background` for genuinely long work.
