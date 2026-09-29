# Architecture

How cc-fleet actually works — the teammate launcher, the key-safety model, the conversion daemon, the workflow engine, and the invariants the codebase is built around. Package names refer to `internal/<pkg>`; the code is always the source of truth.

## The shape of the system

cc-fleet is a single cgo-free Go binary (plus a Claude Code plugin carrying skills, a SessionStart hook and a `PreToolUse(Agent)` guard hook). Everything it runs is a **real `claude` process** whose LLM backend has been swapped: a generated per-provider profile sets `ANTHROPIC_BASE_URL` and an `apiKeyHelper`, and the worker is launched with `--settings <profile>.json --model <id>`. The main session's own auth is never read or modified.

Four execution lanes share that mechanism:

| Lane | Package | Process shape |
|------|---------|---------------|
| Teammate | `teammate` | a native teammate Claude Code starts in a tmux/iTerm2 pane (`Agent` + `SendMessage`); the cc-fleet launcher routes its `claude` to the provider |
| Subagent | `subagent` | one-shot headless `claude -p`, classified result envelope on stdout |
| Workflow | `workflow` | a detached engine fanning out subagent leaves from a JS script |
| Interactive | `run` | execs an interactive provider-backed `claude` in the current terminal |

The CLI is invoked as many short-lived external processes (by Claude, by hooks, by the user), so cross-process coordination happens through the filesystem: TOML/JSON state files, atomic writes (`fileutil.AtomicWrite` is the single outlet), and `flock` scopes (below).

## The teammate launcher (`teammate`)

Since Claude Code 2.1.278 every interactive terminal session has an implicit agent team (`session-<id8>`); `TeamCreate`/`TeamDelete` are gone. Claude Code owns everything about a teammate — team membership, the pane and its layout, the inbox, lead polling, `SendMessage`, the shutdown protocol, permission-mode inheritance, `TaskStop`, and removing the panes and team directory when the lead exits. cc-fleet only swaps the backend of the pane's `claude`, and never writes `~/.claude/teams`.

- **One-time setup** (`teammate setup`, or the TUI's first-run nudge, both with explicit consent): the shim `<ConfigDir>/bin/claude-teammate`, one agent definition per enabled provider (`<claude root>/agents/ccf-<p>.md`, plus `.strong` / `.fast` when those slots hold a different model), and in the user's `settings.json` `env.CLAUDE_CODE_TEAMMATE_COMMAND=<shim>` plus `env.CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`. `teammateMode` is written only on request (`--teammate-mode tmux`; the TUI passes it). Writes go shim → definitions → settings → lane state, so settings never point at a missing shim. The settings editor keeps key order, follows symlinks, preserves the file mode, and writes only when a value changes.
- **The shim contract.** Claude Code runs `CLAUDE_CODE_TEAMMATE_COMMAND` for *every* pane teammate. The shim (POSIX sh, `managed-by: cc-fleet (shim protocol 1)`) pins the cc-fleet binary path, probes `cc-fleet __teammate-launch --shim-protocol 1`, and on success `exec`s the launcher with the original argv. If cc-fleet is missing, downgraded, or broken, native teammates fall through to `claude` on PATH, and `ccf-*` teammates fail closed with a `launcher_target_missing` line. Shell, shim, launcher and the final `claude` are one pid: nothing forks or stays resident.
- **The launcher** (`cc-fleet __teammate-launch`, dispatched in `main.go` before cobra). A non-`ccf-*` `--agent-type` (or none) is passed through: argv and env unchanged, nothing written. For `ccf-<p>[.strong|.fast]` it loads `providers.toml`, resolves the slot's model, finds the lead's exact `claude` (the session named by `--parent-session-id` reports its version → `~/.local/share/claude/versions/<v>`, else `claudebin.Resolve`; refusing to exec itself or the shim), ensures the daemon for OpenAI-protocol/codex providers, writes the profile, strips `--agent-type`, `--settings`, `--model` (and `--effort` when the provider sets one) in both `--x v` and `--x=v` spellings, appends `--settings <profile> --model <id>`, and `exec`s with `childenv.CleanForTeammate`. Every other flag — team identity, permission mode, plugin dirs, MCP config, unknown flags — stays in place. Any failure prints one pane line `cc-fleet teammate: <CODE>: <agent_id>: <msg> — <suggestion>` and exits 1; the dead pane is what `ps` reports as `failed`.
- **Agent definitions** exist so the lead can pick the type. Their frontmatter `model` is a sentinel equal to the type name, so a `ccf-*` teammate that somehow runs in-process fails with a 404 instead of silently running on Claude; the body is never loaded on the pane path because the launcher strips `--agent-type`. The `managed-by: cc-fleet agentdefs v1` marker lives in the body; a same-named file without it is never overwritten.
- **One judgment, two callers** (`Evaluate`). `teammate check` (from the lead's Bash tool; prepares the profile/daemon and optionally probes the provider) and `teammate guard` (from the plugin's `PreToolUse(Agent)` hook; read-only, no network) share it: a live lead session with a session team, Claude Code ≥ `MinTeammateCC` (2.1.278), the launcher configured in the lead's env, a `teammateMode` that resolves to a pane, intact and unshadowed definitions, and a well-formed call (`name` present; no `model`, `isolation`, `cwd`). The desktop app, `-p` and SDK sessions have no team (`TEAMMATE_LANE_UNAVAILABLE/no_session_team`). The guard script (`hooks/teammate-guard.sh`) is sh, does nothing for non-`ccf-*` calls, and blocks a `ccf-*` call on any error; plugin and binary handshake on `--protocol 1`.
- **After the fact** (`ps`): a pane process still carrying `--agent-type ccf-*`, or a `ccf-*` team member with `backendType: in-process`, is `bypassed` (it runs on Claude); the skill stops it with `TaskStop`.
- **The binary gate** (`claudebin`) replaces the old fingerprint: every lane resolves `claude` (`ccver.Detect` on PATH, or the newest `~/.local/share/claude/versions/<semver>` regular executable file) *before* any side effect — profile write, daemon start, lock. No `claude` anywhere is the hard stop (`FINGERPRINT_STALE` for subagent/workflow/run, `CLAUDE_NOT_FOUND` for teammates). All `~/.claude` paths go through `claudepaths` and honor `CLAUDE_CONFIG_DIR`.

## Key safety (`secrets`, `profile`, `childenv`)

The provider key must never reach env, argv, `ps` output, or shell history:

- The profile pins `apiKeyHelper: "<cc-fleet> keyget <provider>"`. Claude Code invokes it at request time; `keyget` resolves the configured backend (`file` | `pass` | `1password` | `vault` | `keyring` | `codex-oauth`) and writes the key to stdout exactly once — the `codex-oauth` backend returns the loopback handshake secret rather than a real key. Nothing in `secrets` logs key bytes.
- Two layers keep the lead's own credentials away from a provider. **Process env:** the teammate launcher execs through `childenv.CleanForTeammate` (drops `ANTHROPIC_API_KEY` / `_AUTH_TOKEN` / `_BASE_URL` / `_CUSTOM_HEADERS`, `CLAUDE_CODE_OAUTH_TOKEN`, the model vars, the `CLAUDE_CODE_USE_*` cloud-backend switches and their Bedrock/Vertex/AWS/Google credentials, and the host-credential vars, but keeps `CLAUDECODE` and `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS`); the subagent/run lanes scrub credentials plus nested-Claude markers through `childenv.Clean` (case-insensitive on Windows). **Settings:** the worker still loads user/project settings, whose `env` block could inject a credential, so every profile — a flag-level setting, which outranks user and project settings — sets `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY`, `ANTHROPIC_CUSTOM_HEADERS` and the seven `CLAUDE_CODE_USE_{BEDROCK,VERTEX,FOUNDRY,ANTHROPIC_AWS,ANTHROPIC_GOOGLE_CLOUD,MANTLE,GATEWAY}` to `""`. Known blind spot: policy/managed settings outrank flag settings, so an `env` block there cannot be overridden.
- Routing is decided only by `--agent-type` looked up in the user's own `providers.toml`; the launcher never reads a definition body, and the guard refuses a project-level definition that shadows a `ccf-*` type.
- Everything user-facing renders keys through `MaskKey` (`sk-…238`); `redact.MaskKeyLike` scrubs `sk-…` / `Bearer …` / `x-api-key` tokens from any error or log line that could carry one.
- The reserved id `claude` (subagent/workflow leaves only) deliberately runs the leaf on the caller's own Claude Code login — no profile, no `keyget`; the child env is still scrubbed, so it requires a stored login rather than an env key, and the name is rejected everywhere a provider is configured.
- The `file` backend supports multi-key sets with per-key enable/disable and rotation (`off` / `round_robin` — a flock-guarded counter — / `random`); disabled keys are filtered before selection, and `keyget` is the per-worker rotation point.

## Provider classes (`providerclass`, `codexproxy`)

A provider's `protocol` field selects one of three classes:

1. **Anthropic-native** (empty protocol) — no daemon; `claude` talks straight to `base_url` and `keyget` hands it the real key.
2. **OpenAI-protocol** (`openai-responses`, `openai-chat`) — `claude` speaks Anthropic to a loopback conversion daemon, which translates to the OpenAI Responses or Chat Completions API and attaches the upstream key. The key reaches the daemon as a header and is forwarded as the upstream Bearer; every error surface is redacted.
3. **codex-oauth** — reuses a ChatGPT subscription. cc-fleet keeps its own token store per credential (`codex_oauth[-<ref>].json`, never touching `~/.codex`), refreshes under a per-credential lock, and the daemon converts to the Responses API.

For codex-oauth, **the bearer never leaves the daemon** — `keyget` serves `claude` only a per-install loopback handshake secret that gates `/v1/messages`, and the daemon holds the real token. The openai-* classes instead hand `claude` the real upstream key via `keyget` (it reaches the daemon as a header, forwarded as the upstream Bearer, every error surface redacted). Daemons are per-port (state, lock, and lease keyed by port; identity-checked reuse; `/healthz` readiness), started lazily, and self-exit when idle.

## The workflow engine (`workflow`)

`workflow run <script.js>` executes the script on an embedded goja VM in a detached process. One loop goroutine owns the VM; leaves run on a bounded goroutine pool calling `subagent.Run` in-process. Determinism is sealed at bootstrap — wall-clock `Date`, `Math.random`, `eval`, and dynamic code are removed — which makes the **content-hash journal** exact: a leaf is keyed by provider + model + prompt + schema + profile shape, so `--resume` replays finished leaves from cache and re-runs only what changed. Failed leaves are never journaled.

The engine `chdir`s into the run's recorded directory before its start-up sweep and before any leaf, so `--resume` and `restart` work in the project the run was launched from, whatever the caller's cwd; a vanished directory fails the run (and the resume preflight) with a clear error. An `isolation: "worktree"` leaf gets a detached worktree; at its end a clean worktree is removed, a dirty one is snapshot-committed to branch `cc-fleet/wf-<job>-a<attempt>` first (logged as `isolation worktree kept` in the run's events). If the snapshot fails the directory stays, marked `.cc-fleet-keep`, and both sweeps and `subagent.PurgeRun` skip it; if even the marker cannot be written the events carry an `UNPROTECTED` warning. Worktrees orphaned by a stopped or killed run are salvaged to `cc-fleet/wf-salvage-*` branches by the next sweep or `rm`/`prune`. cc-fleet never deletes those branches.

Live control runs over a polled per-run control file: `stop --leaf` pre-marks the leaf **held** (a nonterminal status — the `agent()` promise stays unsettled, the engine keeps the run open indefinitely) and then kills the attempt; `restart --leaf` re-execs the same job id with attempt +1. `stop --phase`/`restart --phase` do the same per phase.

`workflow wait` is the push-notification verb: it polls the manifest and job files (never the event stream) and exits exactly once — `0` terminal-ok, `1` failed/engine-gone, `3` parked (zero running, zero queued, at least one held — debounced over consecutive polls), `124` heartbeat timeout, `130` interrupt, `2` IO/unknown-run error. Armed in a backgrounded shell, its exit wakes the launching session; the envelope stays slim (counts + spend, no per-leaf detail) because it is injected into a session unasked.

## Concurrency: flock scopes (`config/lock.go` and friends)

Nine scopes. Two nest, strictly in this order when combined:

1. `WithProvidersConfigLock` — the `providers.toml` load→mutate→save cycle (outer).
2. `WithServerLock` — tmux layout changes by `hide` / `show` (inner).

There is no per-team lock any more: cc-fleet never writes team state. The teammate launcher takes only the standalone codexproxy locks (when it starts a daemon), and `teammate guard` none. The other seven are standalone, each held with no other scope: the per-run workflow lifecycle lock; codexproxy's per-port daemon lock; codexproxy's per-credential token lock (read → refresh → persist); the create-once handshake-secret lock; selfupdate's whole-run update lock; the update-check cache lock; and the subagent per-job live-scan checkpoint lock (a dedicated `<jobID>.scan.lock` file) that serializes a detached background job's incremental token-scan read-modify-write so concurrent board polls can't tick the persisted floor backward.

## The JSON envelope contract

`subagent.Run` and `teammate check` (`teammate.Evaluate`) never return a raw Go error to the CLI — they return a `Result` with `ok: true|false` and, on failure, a stable UPPER_SNAKE `error_code` plus a suggestion; `teammate check` also carries a lower_snake `detail` from a fixed vocabulary (`no_session_team`, `launcher_not_configured`, `mode_in_process:<source>`, …) and `protocol: 1`. `teammate setup`, `teardown`, `hide` and `show` follow the same `{ok, error_code, error_msg, suggestion}` shape. The `--json` output is the CLI↔skill contract: the skills dispatch on `error_code` and `detail`, never on prose. (Runtime wedge detection is the separate lower_snake `error_class` from `ps --check`.) Preserve this discipline when editing those packages.

## Name validation is a security boundary (`ids`)

Provider/team/agent names flow into file paths (`filepath.Join` → traversal risk) and into the `apiKeyHelper` string (`<bin> keyget <name>` → injection risk). `ValidateProviderName` / `ValidateTeamName` / `ValidateMemberName` run before use, and `EnsureUnderRoot` confirms constructed paths stay inside their ownership root; `config.Load` and `profile` re-validate as defense in depth.

## Platform matrix (`procintrospect` + per-package seams)

No cgo anywhere; `CGO_ENABLED=0` across all six release targets (linux/darwin/windows × amd64/arm64).

- **Linux** — full (the teammate lane is unit-tested and cross-compiled); `/proc` is the introspection path.
- **macOS** — full and CI-tested; exact argv and the process table come from `sysctl` (`kern.procargs2`, `kern.proc.all`), never from splitting `ps` output on spaces.
- **Windows** — everything except the teammate lane: `subagent`, `workflow`, `run`, and the TUI are native; `teammate check` / `setup`, the launcher, `teardown`, `hide` and `show` refuse with `UNSUPPORTED_ON_WINDOWS`, no agent definitions are generated, and the guard hook lets every non-`ccf-*` call through. Process identity is (pid, start-token): a token mismatch is decisive, and a token match never overrides a readable argv mismatch.
- **Where the teammate lane works:** a terminal `claude` inside tmux (split panes on the lead's server) or iTerm2 (its own splits, or tmux external when `it2` is missing); outside tmux with `teammateMode: tmux`, Claude Code runs its own detached `claude-swarm-<lead pid>` server. `in-process` (and `auto` outside a pane terminal) is refused, as are the desktop app, `-p` and SDK sessions.

Platform splits live in `procintrospect` and per-package `_unix.go` / `_windows.go` seams — anything touching process tables goes through them.

## Identity, state, and the board

- **Pane identity is (tmux socket path, pane id)** — never (team, name); an agent id `name@team` is only a lookup key. Every tmux call uses `-S <absolute socket path>` through the single `tmux.Server` outlet. Swarm servers belong to Claude Code (`claude-swarm-*`); a `cc-fleet-swarm-*` socket is a 0.3.x leftover (`legacy`).
- **Discovery keeps no ledger** (`teardown.DiscoverTeammates`, behind `ps`, `watch`, `teardown`, `hide`/`show` and the board). It enumerates every tmux server socket of the user, lists all panes, reads the process table with exact argv, and attributes a process only on positive evidence: argv still carrying `--agent-type ccf-*` (`bypassed`), or a session-team member whose `agentType` is `ccf-*` and whose `--settings` is that provider's profile, or a 0.3.x teammate with an independent marker. A profile path alone proves nothing — native teammates of a `cc-fleet run` lead inherit its `--settings` — so they are never listed or killed. Dead panes are scanned (`capture-pane -p -J`) for the launcher's failure line (`failed`); team `config.json` files are read only. A teammate whose lead session is gone is `orphaned`.
- **Kills re-verify identity.** `teardown` re-reads each candidate's exact argv and process start token right before `kill-pane` and before each of SIGTERM / SIGKILL; a mismatch is `skipped: IDENTITY_MISMATCH` and nothing is signalled. In-process teammates can only be stopped from the lead (`TaskStop`). `hide` keeps the origin window in the pane option `@ccf_origin`, so no file records it.
- `config.Load` is **strict**: an invalid `key_rotation`, unknown `secret_backend`, or wrong schema version is rejected at load, not defaulted.
- The TUI (`tui`) is one Bubbletea app: the provider hub (add/edit forms, key manager, codex login) and a project-first master-detail **Agents Board** over teammates, subagent jobs, and workflow runs — with per-leaf hold/restart, prompt/answer drill-in, spend columns, and a `ctrl+f` flat session browser that filters every past job / run / team by substring and opens the selected one's existing detail. Teammate rows come from the same discovery as `ps`, and a teammate card's messages from Claude Code's inbox file, read only (`teammate.InboxPath`). `teamhist` keeps ended teams visible as faint snapshot rows, recorded from discovery results under cc-fleet's own config dir; `pinned` marks records as out-of-band files so they survive GC and clears. The whole palette is adaptive (dark/light).

## Distribution & self-update (`selfupdate`, `version`)

GoReleaser builds six archives on a `v*` tag; the same artifacts feed the one-line installer, npm (`@ethanhq/cc-fleet`, postinstall downloads the platform binary), and manual zips. Every installer writes a small manifest next to the binary recording its install method; `cc-fleet update` reads it and updates through the same channel — a tarball install swaps the binary in place (sha256-verified, `--version` smoke-tested, previous binary kept for `update rollback`), npm/go delegate to their package manager — then refreshes the plugin in the same pass. Only a comparable release version ever updates; a dev build is left alone.
