# Architecture

How cc-fleet actually works — the spawn recipe, the key-safety model, the conversion daemon, the workflow engine, and the invariants the codebase is built around. Package names refer to `internal/<pkg>`; the code is always the source of truth.

## The shape of the system

cc-fleet is a single cgo-free Go binary (plus a Claude Code plugin carrying skills, a hook, and two read-only slash commands). Everything it runs is a **real `claude` process** whose LLM backend has been swapped: a generated per-provider profile sets `ANTHROPIC_BASE_URL` and an `apiKeyHelper`, and the worker is launched with `--settings <profile>.json --model <id>`. The main session's own auth is never read or modified.

Four execution lanes share that mechanism:

| Lane | Package | Process shape |
|------|---------|---------------|
| Teammate | `spawn` | long-lived `claude` in a tmux pane, driven by native `TeamCreate`/`SendMessage` |
| Subagent | `subagent` | one-shot headless `claude -p`, classified result envelope on stdout |
| Workflow | `workflow` | a detached engine fanning out subagent leaves from a JS script |
| Interactive | `run` | execs an interactive provider-backed `claude` in the current terminal |

The CLI is invoked as many short-lived external processes (by Claude, by hooks, by the user), so cross-process coordination happens through the filesystem: TOML/JSON state files, atomic writes (`fileutil.AtomicWrite` is the single outlet), and `flock` scopes (below).

## The fingerprint recipe (`fingerprint`)

To behave like a native teammate, a worker must be launched with the *exact* flags/env Claude Code itself uses to spawn an agent. cc-fleet learns these by capturing a live native-agent process: `CaptureFromPid` reads its argv plus a tiny env allowlist, and `templatize()` replaces per-spawn values with placeholders (`--agent-id {name}@{team}`, `--team-name {team}`, …) and strips `--model`/`--settings` (cc-fleet always appends its own). At spawn, `Apply()` substitutes the placeholders back.

- A **bundled default recipe** ships in the binary (`//go:embed`), so a fresh install spawns without ever probing. `refresh-fingerprint` re-captures when a Claude Code upgrade drifts the flags; the skills run that self-heal automatically on `FINGERPRINT_MISSING`.
- **The gate runs before any side effect.** Every spawn/subagent resolves `LoadOrBundled → ResolveBinaryPath → ValidateForRuntime` *before* a profile is written, a lock is taken, or a pane is split — a launch that cannot work fails before it mutates anything. No `claude` binary anywhere is the one hard stop (`FINGERPRINT_STALE`).
- A post-spawn **settle check** (pane-based, so identical on Linux/macOS) is paid only when the live Claude Code is newer than the recipe.

## Key safety (`secrets`, `profile`, `childenv`)

The provider key must never reach env, argv, `ps` output, or shell history:

- The profile pins `apiKeyHelper: "<cc-fleet> keyget <provider>"`. Claude Code invokes it at request time; `keyget` resolves the configured backend (`file` | `pass` | `1password` | `vault` | `keyring`) and writes the key to stdout exactly once. Nothing in `secrets` logs key bytes.
- Spawned teammate commands begin `env -u ANTHROPIC_API_KEY -u ANTHROPIC_AUTH_TOKEN`; the subagent/run lanes scrub the same vars plus nested-Claude markers through the shared `childenv.Clean` (case-insensitive on Windows).
- Everything user-facing renders keys through `MaskKey` (`sk-…238`); `redact.MaskKeyLike` scrubs `sk-…` / `Bearer …` / `x-api-key` tokens from any error or log line that could carry one.
- The reserved id `claude` (subagent/workflow leaves only) deliberately runs the leaf on the caller's own Claude Code login — no profile, no `keyget`; the child env is still scrubbed, so it requires a stored login rather than an env key, and the name is rejected everywhere a provider is configured.
- The `file` backend supports multi-key sets with per-key enable/disable and rotation (`off` / `round_robin` — a flock-guarded counter — / `random`); disabled keys are filtered before selection, and `keyget` is the per-worker rotation point.

## Provider classes (`providerclass`, `codexproxy`)

A provider's `protocol` field selects one of three classes:

1. **Anthropic-native** (empty protocol) — no daemon; `claude` talks straight to `base_url` and `keyget` hands it the real key.
2. **OpenAI-protocol** (`openai-responses`, `openai-chat`) — `claude` speaks Anthropic to a loopback conversion daemon, which translates to the OpenAI Responses or Chat Completions API and attaches the upstream key. The key reaches the daemon as a header and is forwarded as the upstream Bearer; every error surface is redacted.
3. **codex-oauth** — reuses a ChatGPT subscription. cc-fleet keeps its own token store per credential (`codex_oauth[-<ref>].json`, never touching `~/.codex`), refreshes under a per-credential lock, and the daemon converts to the Responses API.

For daemon-backed classes, **the bearer never leaves the daemon**: `keyget` serves only a per-install loopback handshake secret. Daemons are per-port (state, lock, and lease keyed by port; identity-checked reuse; `/healthz` readiness), started lazily, and self-exit when idle.

## The workflow engine (`workflow`)

`workflow run <script.js>` executes the script on an embedded goja VM in a detached process. One loop goroutine owns the VM; leaves run on a bounded goroutine pool calling `subagent.Run` in-process. Determinism is sealed at bootstrap — wall-clock `Date`, `Math.random`, `eval`, and dynamic code are removed — which makes the **content-hash journal** exact: a leaf is keyed by provider + model + prompt + schema + profile shape, so `--resume` replays finished leaves from cache and re-runs only what changed. Failed leaves are never journaled.

Live control runs over a polled per-run control file: `stop --leaf` pre-marks the leaf **held** (a nonterminal status — the `agent()` promise stays unsettled, the engine keeps the run open indefinitely) and then kills the attempt; `restart --leaf` re-execs the same job id with attempt +1. `stop --phase`/`restart --phase` do the same per phase.

`workflow wait` is the push-notification verb: it polls the manifest and job files (never the event stream) and exits exactly once — `0` terminal-ok, `1` failed/engine-gone, `3` parked (zero running, zero queued, at least one held — debounced over consecutive polls), `124` heartbeat timeout, `130` interrupt. Armed in a backgrounded shell, its exit wakes the launching session; the envelope stays slim (counts + spend, no per-leaf detail) because it is injected into a session unasked.

## Concurrency: flock scopes (`config/lock.go` and friends)

Nine scopes. Three nest, strictly in this order when combined:

1. `WithProvidersConfigLock` — the `providers.toml` load→mutate→save cycle (outermost).
2. `WithTeamLock(team)` — every mutation of per-team state.
3. `WithServerLock` — global tmux split/layout races (innermost).

The other six are standalone, each held with no other scope: the per-run workflow lifecycle lock; codexproxy's per-port daemon lock; codexproxy's per-credential token lock (read → refresh → persist); the create-once handshake-secret lock; selfupdate's whole-run update lock; and the update-check cache lock.

## The JSON envelope contract

`spawn.Spawn` and `subagent.Run` never return a raw Go error to the CLI — they return a `Result` with `ok: true|false` and, on failure, a stable UPPER_SNAKE `error_code` plus a suggestion. The `--json` output is the CLI↔skill contract: the skills dispatch on `error_code`, never on prose. (Runtime wedge detection is the separate lower_snake `error_class` from `ps --check`.) Preserve this discipline when editing those packages.

## Name validation is a security boundary (`ids`)

Provider/team/agent names flow into file paths (`filepath.Join` → traversal risk) and into the `apiKeyHelper` string (`<bin> keyget <name>` → injection risk). `ValidateProviderName` / `ValidateTeamName` / `ValidateMemberName` run before use, and `EnsureUnderRoot` confirms constructed paths stay inside their ownership root; `config.Load` and `profile` re-validate as defense in depth.

## Platform matrix (`procintrospect` + per-package seams)

No cgo anywhere; `CGO_ENABLED=0` across all six release targets (linux/darwin/windows × amd64/arm64).

- **Linux** — full; `/proc` is the reference introspection path.
- **macOS** — full and CI-tested; introspection falls back to `ps`, and the settle gate is pane-based so teammate liveness behaves identically.
- **Windows** — everything except the tmux teammate lane: `subagent`, `workflow`, `run`, and the TUI are native; `spawn`/`hide`/`show` refuse with `UNSUPPORTED_ON_WINDOWS` (`teardown` refuses with the same message). Process identity is (pid, start-token): a token mismatch is decisive, and a token match never overrides a readable argv mismatch.

Platform splits live in `procintrospect` and per-package `_unix.go` / `_windows.go` seams — anything touching process tables goes through them.

## Identity, state, and the board

- **Pane identity is (tmux socket, pane id)** — never (team, name). The default server is the empty socket; out-of-tmux teammates live on a per-team `cc-fleet-swarm-<team>` socket. All tmux access goes through the single `tmux.Server` outlet.
- `config.Load` is **strict**: an invalid `key_rotation`, unknown `secret_backend`, or wrong schema version is rejected at load, not defaulted.
- The TUI (`tui`) is one Bubbletea app: the provider hub (add/edit forms, key manager, codex login) and a project-first master-detail **Agents Board** over teammates, subagent jobs, and workflow runs — with per-leaf hold/restart, prompt/answer drill-in, and spend columns. `teamhist` keeps ended teams visible as faint snapshot rows; `pinned` marks records as out-of-band files so they survive GC and clears. The whole palette is adaptive (dark/light).

## Distribution & self-update (`selfupdate`, `version`)

GoReleaser builds six archives on a `v*` tag; the same artifacts feed the one-line installer, npm (`@ethanhq/cc-fleet`, postinstall downloads the platform binary), and manual zips. Every installer writes a small manifest next to the binary recording its install method; `cc-fleet update` reads it and updates through the same channel — a tarball install swaps the binary in place (sha256-verified, `--version` smoke-tested, previous binary kept for `update rollback`), npm/go delegate to their package manager — then refreshes the plugin in the same pass. Only a comparable release version ever updates; a dev build is left alone.
