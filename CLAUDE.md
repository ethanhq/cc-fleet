# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`cc-fleet` is a Go CLI (+ a Claude Code plugin) that runs third-party LLM providers
(DeepSeek, GLM, Kimi, Qwen, MiniMax — anything with an Anthropic-compatible API, plus
ChatGPT-subscription codex and OpenAI-protocol endpoints) as **real Claude Code workers**.
The core trick: a provider worker is a genuine `claude` process whose LLM backend is swapped
by launching it with `--settings <provider-profile>.json` (which sets `ANTHROPIC_BASE_URL` +
an `apiKeyHelper`) and `--model <provider-model-id>`. The main session's own auth (OAuth or
API key) is never touched.

There are **four execution lanes** — internalize this split, most of the code maps onto it:

- **Teammate** — `internal/teammate`. A native Claude Code teammate the lead starts with
  `Agent({name, subagent_type:"ccf-<provider>[.strong|.fast]", prompt})` and drives with
  `SendMessage`; Claude Code owns the team, pane, inbox and cleanup. Claude Code runs the pane
  through `CLAUDE_CODE_TEAMMATE_COMMAND` = cc-fleet's shim (`<ConfigDir>/bin/claude-teammate`),
  which execs the launcher `cc-fleet __teammate-launch`; the launcher swaps in the provider's
  profile and model and execs the lead's exact `claude`. `teammate check` (lead's Bash) and
  `teammate guard` (the plugin's `PreToolUse(Agent)` hook) share one `Evaluate`. Terminal
  `claude` in tmux/iTerm2 only, Claude Code ≥ 2.1.278; not the desktop app, `-p`, or SDK.
  Unix-only.
- **Subagent** — `internal/subagent`. `claude -p` headless, one-shot (or `--background` with a
  job file); classified `Result` envelope on stdout. The reserved id `claude` runs the leaf on
  the user's own Claude login (explicit-only; subagent/workflow lanes only — no profile, no keyget).
- **Workflow** — `internal/workflow`. A detached engine running a JS script on an embedded
  goja VM; each `agent()` leaf is a provider subagent. Journaled (`--resume`), leaves can be
  held/restarted live, `workflow wait` is the push-notification verb.
- **Interactive** — `internal/run`. Execs a provider-backed interactive `claude` in the
  current terminal. No tmux, no team, no locks.

The skills (`skills/{subagent,team,workflow}/SKILL.md`) teach the *main* Claude session when
to pick each lane. They are not loaded when you work *on* this repo — product, not project
config.

The full system design — teammate launcher, key safety, provider classes/daemon, workflow
engine, lock map, platform matrix — is **[`docs/architecture.md`](docs/architecture.md)**.
Read it before structural changes; this file keeps only what an editing agent needs constantly.
The user-facing references are [`docs/cli.md`](docs/cli.md) (zh mirror `docs/cli.zh.md`) and
[`docs/workflows.md`](docs/workflows.md) — **update them when you change a command, flag,
envelope, or the workflow script API**.

## Coding standards (read before editing)

The full contribution standard is **[`CONTRIBUTING.md`](.github/CONTRIBUTING.md)** — required checks,
commit/PR rules, screenshots for UI/bug changes, and the AI-attribution policy. The principles
that matter most when an AI agent edits this repo:

- **Minimal intrusion.** Change the fewest lines that fully solve the task. Don't refactor,
  rename, reformat, or "tidy" code outside the scope of your change. Match the surrounding
  style instead of imposing your own.
- **Simplest implementation that's correct.** Prefer the straightforward solution over a
  clever or speculative one. No new abstraction, dependency, or config surface unless the task
  truly needs it (YAGNI). Reuse an existing helper before adding another.
- **Concise comments.** Comment *why*, not *what*; let the code say what. No narration, no
  changelog/ticket numbers in comments, no restating the obvious. Fix comments your change
  makes stale.
- **Respect the invariants** (below). They are correctness/security boundaries, not style
  preferences.
- **Verify before declaring done.** `go test -race ./...`, `gofmt -l .`, `go vet ./...` must be
  clean; `claude plugin validate . --strict` if you touched the plugin/skill.

### AI attribution

- **AI-*assisted*** (a human authored/reviewed the diff) → add a `Co-Authored-By:` trailer
  naming the tool/model in the commit message.
- **Fully AI-*authored*** PR (no human authored the diff) → add the autonomous-PR marker as the
  last line of the PR body (see `CONTRIBUTING.md`).

## Build, test, run

```bash
make build            # → ./bin/cc-fleet  (or: go build -o bin/cc-fleet ./cmd/cc-fleet)
make test             # → go test ./...
make smoke            # build + print --version
go test ./internal/teammate                  # one package
go test ./internal/teammate -run TestRouteRewritesArgv   # one test (-run regex)
go vet ./...          # there is NO golangci config — vet + gofmt are the bar
make cross-compile    # 6 platform binaries (linux/darwin/windows × amd64/arm64) → ./dist
make release-archive  # per-platform archives (dev fallback; CI uses goreleaser on tag)
```

`make install` installs the **binary + `ccf` alias only** (to `$PREFIX`, default
`~/.local/bin`). It deliberately does **not** install the skills — those ship via the plugin
or `make install-skill`; installing both ways duplicates them. Version is stamped at link
time via `-ldflags "-X .../internal/version.Version=<tag>"`; a plain local build reports a
dev version.

`subagent`/`run` and provider teammates launch **real** provider processes. Tests must be
hermetic: temp `HOME` / `CLAUDE_CONFIG_DIR` / `XDG_CONFIG_HOME`, a fake `claude`, never the real
`~/.claude` or key store. The teammate lane needs a terminal `claude` inside tmux;
`scripts/e2e-teammate.sh` sets up an isolated canary environment for it (re-run after Claude
Code upgrades), and `scripts/test-install-replace.sh` checks that re-installing over a running
binary replaces it by rename.

## The invariants (do not regress)

- **Keys never in env/argv/history.** Provider auth flows only through the profile
  `apiKeyHelper` → `cc-fleet keyget`; the teammate launcher execs through
  `childenv.CleanForTeammate`, subagent/run scrub via `childenv.Clean`, and every profile blanks
  `ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_API_KEY` / `ANTHROPIC_CUSTOM_HEADERS` and the seven
  `CLAUDE_CODE_USE_*` switches so settings-level credentials never reach a provider. The
  launcher, `teammate check`/`guard` and `ps` never read a key. Nothing in `internal/secrets`
  may log key bytes; UI/logs show only `MaskKey` (`sk-…238`). For daemon-backed providers
  (codex, openai-*), the bearer never leaves `internal/codexproxy`.
- **Gates before side effects.** The binary gate (`claudebin.Resolve` / `ForSession`) runs in
  every lane before any profile write, daemon start or lock; for teammates the `Evaluate` gate
  (shared by `teammate check` and `teammate guard`, fail-closed) runs before the `Agent` call.
  Fail-before-mutation.
- **Classified `Result` envelopes.** `subagent.Run` / `teammate check` never return raw Go
  errors — always a `Result` with `ok` and a stable UPPER_SNAKE `error_code` + suggestion
  (plus `detail` and `protocol` for teammate check). The skills dispatch on `error_code`;
  preserve the vocabulary.
- **cc-fleet never writes Claude Code's team state.** `~/.claude/teams`, `tasks/`, `sessions/`
  are read-only; ownership in `ps`/`teardown` needs positive evidence (`ccf-*` agent type), not
  just a profile path — native teammates of a provider lead inherit its `--settings`.
- **Name validation before use** (`internal/ids`). Provider/team/agent names reach file paths
  and the `apiKeyHelper` string — `ValidateProviderName`/`ValidateTeamName`/
  `ValidateMemberName` + `EnsureUnderRoot`, re-validated in `config.Load` and `profile`.
- **Nine flock scopes; the nesting pair in order.** `WithProvidersConfigLock` →
  `WithServerLock` when combined; the other seven (workflow per-run, codexproxy per-port,
  per-credential token, handshake-secret, selfupdate update, update-check cache, subagent
  per-job live-scan checkpoint) are standalone — held with no other scope. There is no team
  lock. See `docs/architecture.md`.
- **Single outlets.** Every tmux call goes through `tmux.Server` (`-S <socket path>`); every
  atomic write through `fileutil.AtomicWrite`; every Claude Code path through `claudepaths`
  (honors `CLAUDE_CONFIG_DIR`); `settings.json` edits through `onboarding.EditSettings`. Pane
  identity is (socket path, pane id), never (team, name); kills re-verify exact argv and the
  process start token.
- **`config.Load` is strict.** Invalid `key_rotation` / unknown `secret_backend` / wrong
  version are rejected at load, never defaulted.
- **No cgo.** Platform behavior splits via `internal/procintrospect` and per-package
  `_unix.go`/`_windows.go`/`_darwin.go` seams; anything touching `/proc` or process tables
  goes through `procintrospect` (darwin: exact argv via `kern.procargs2`). Targets: linux,
  darwin, windows × amd64, arm64 — on Windows the teammate lane refuses (`teammate
  check`/`setup`, the launcher, `teardown`, `hide`, `show` emit `UNSUPPORTED_ON_WINDOWS`; no
  agent definitions); subagent/workflow/run/TUI are native, and process identity is (pid,
  start-token).

## Config & on-disk layout (`internal/config`)

- `~/.config/cc-fleet/providers.toml` — single source of truth users edit (schema
  `version = 1`, mode `0600`). Honors `$XDG_CONFIG_HOME`.
- `~/.config/cc-fleet/secrets/` — file-backend keys (`0700`/`0600`). Backends:
  `file | pass | 1password | vault | keyring`.
- `~/.config/cc-fleet/subagent-jobs/` — background job meta + result caches; `runs/` beneath
  it holds workflow manifests/journals.
- `~/.config/cc-fleet/bin/claude-teammate` — the teammate launcher shim (`teammate setup`,
  re-pinned by `repair`; kept by `setup --remove` and `uninstall`).
- `~/.config/cc-fleet/onboarding.json` — also the teammate lane state (`teammate_lane`).
- `~/.claude/profiles/<provider>.json` — generated provider profiles (regenerate with `repair`).
- `~/.claude/agents/ccf-*.md` — provider teammate agent definitions; only files with the
  `managed-by: cc-fleet` marker are written or removed.
- `~/.claude/settings.json` — cc-fleet sets `env.CLAUDE_CODE_TEAMMATE_COMMAND`,
  `env.CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS` and, only on request, `teammateMode`.
- `~/.claude/teams/<team>/config.json` — native team state, owned by Claude Code; cc-fleet only
  reads it.
- The `~/.claude` paths honor `$CLAUDE_CONFIG_DIR` (via `internal/claudepaths`); profiles stay
  under `~/.claude/profiles`.

## Command map (`cmd/cc-fleet/`)

One file per subcommand, all registered in `main.go`. Bare `cc-fleet` in an interactive TTY
launches the Bubbletea TUI (`internal/tui`); non-interactive falls through to help.

- **User-layer:** `init` `add` `edit` `remove` `list` `default` `models` `refresh` `doctor`
  `repair` `uninstall` `update` `run` `codex {add,login,logout,status}`
  `codex-proxy {status,stop}`.
- **Claude-layer (machine-driven, `--json`):** `teammate {check,setup}` `subagent`
  `subagent-status` `subagent-gc` `teardown` `hide` `show` `ps` `watch` `keyget`
  `workflow {run,new,list,status,saved,stop,restart,watch,wait,rm,prune}`.
- **Hidden:** `teammate guard` (hook), `__teammate-launch` (the shim's target, dispatched in
  `main.go` before cobra), and the `COMMAND_REMOVED` stubs `spawn` / `refresh-fingerprint`.

## Editing the skills (canonical vs local copy)

The skills' canonical source is **`skills/{subagent,team,workflow}/SKILL.md`** plus the shared
docs in `skills/cc-fleet-shared/` (no SKILL.md there — deliberately not a skill). The repo
also has a gitignored install copy under `.claude/skills/` that `make skill-sync` refreshes
from canonical. **Edit only the canonical source**, then run `make skill-sync`;
`make skill-drift-check` fails if the two diverge.

## Distribution

GoReleaser builds six archives + checksums on a `v*` tag (`.github/workflows/release.yml`;
Windows ships as zips). Also shipped via npm (`npm/`, postinstall fetches the platform
binary), the one-line `install.sh`, and the Claude Code plugin marketplace (`.claude-plugin/`,
with a SessionStart hook and the `PreToolUse(Agent)` teammate guard in `hooks/`). `cc-fleet update`
(`internal/selfupdate`) self-updates through the recorded install method and refreshes the
plugin in the same pass.
