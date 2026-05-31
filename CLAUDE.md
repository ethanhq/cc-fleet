# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`cc-fleet` is a Go CLI (+ one Claude Code skill) that runs third-party LLM vendors
(DeepSeek, GLM, Kimi, Qwen, MiniMax — anything with an Anthropic-compatible API) as **real
Claude Code workers**. The core trick: a vendor worker is a genuine `claude` process whose
LLM backend is swapped by launching it with `--settings <vendor-profile>.json` (which sets
`ANTHROPIC_BASE_URL` + an `apiKeyHelper`) and `--model <vendor-model-id>`. The main session's
own auth (OAuth or API key) is never touched.

There are **two execution modes** — internalize this split, most of the code maps onto it:

- **Teammate (lane 1)** — `internal/spawn`. Launches a long-lived vendor `claude` in a tmux
  pane; the user's main session drives it via native `TeamCreate`/`SendMessage`. Stateful,
  reusable across turns.
- **Subagent (lane 2)** — `internal/subagent`. Runs `claude -p` headless, synchronously, no
  tmux/team. One-shot.

The skill (`skills/cc-fleet/SKILL.md`) teaches the *main* Claude session *when* to pick each
lane. It is not loaded when you work *on* this repo — it is product, not project config.

## Coding standards (read before editing)

The full contribution standard is **[`CONTRIBUTING.md`](CONTRIBUTING.md)** — required checks,
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
- **Respect the invariants.** Keys never in env/argv/history; classified `Result` envelopes
  (never raw errors) from `spawn`/`subagent`; validate names before use; honor the lock order.
  These are correctness/security boundaries, not style preferences.
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
make test             # → go test ./...   (633 tests across 73 files)
make smoke            # build + print --version
go test ./internal/spawn                     # one package
go test ./internal/spawn -run TestSpawn_RollbackOnEnsureInboxFailure   # one test (-run regex)
go test -v ./internal/fingerprint            # verbose
go vet ./...          # there is NO golangci config — vet + gofmt are the bar
make cross-compile    # 4 platform binaries → ./dist
make release-archive  # per-platform tarballs (dev fallback; CI uses goreleaser on tag)
```

`make install` installs the **binary + `ccf` alias only** (to `$PREFIX`, default
`~/.local/bin`). It deliberately does **not** install the skill — that ships via the plugin or
`make install-skill`; installing both ways duplicates it. Version is stamped at link time via
`-ldflags "-X .../internal/version.Version=<tag>"`; a plain local build reports `0.1.0-dev`.

## Architecture

### The fingerprint "recipe" (`internal/fingerprint`) — the load-bearing idea

To make a vendor worker behave like a native teammate, cc-fleet must launch `claude` with the
*exact* flags/env Claude Code itself uses to spawn an `Agent`. It learns these by **capturing
a live native-Agent process**: `CaptureFromPid` reads its argv + a tiny env allowlist,
`templatize()` replaces the per-spawn values with placeholders (`--agent-id {name}@{team}`,
`--team-name {team}`, `--agent-color {color}`, `--parent-session-id {lead_session_id}`) and
**strips `--model`/`--settings`** (cc-fleet always appends its own). At spawn, `Apply()`
substitutes the placeholders back and `buildSpawnCommand` prepends `env -u ANTHROPIC_API_KEY
-u ANTHROPIC_AUTH_TOKEN` + the vendor profile + model.

- A **bundled default recipe** (`bundled.go` / `default_fingerprint.json`) ships in the binary,
  so a fresh install spawns with no probe. A user can re-probe with `refresh-fingerprint` when
  a Claude Code upgrade drifts the flags.
- `ResolveBinaryPath` resolves the live `claude` binary (cached path if still on disk, else
  `ccver.Detect()`) so a CC upgrade that GC'd the pinned path can't strand a spawn.
- `CurrentVersionExceedsRecipe` gates the post-spawn "settle" check — only pay that latency
  when the running CC is newer than the recipe (flags may have drifted). Error codes
  `FINGERPRINT_MISSING` / `SPAWN_DID_NOT_SETTLE` trigger the skill's self-heal; `FINGERPRINT_STALE`
  means no binary at all.

### Key safety (`internal/secrets`, `internal/profile`) — never put the key in argv/env

The vendor API key must never reach `env`, `argv`, `ps aux`, or shell history. The mechanism:
`profile.GenerateForVendor` writes `~/.claude/profiles/<vendor>.json` with
`apiKeyHelper: "<cc-fleet-abs-path> keyget <vendor>"`. Claude Code invokes that at runtime;
`cc-fleet keyget` (→ `secrets.Keyget`) resolves the key from the configured backend
(`file` | `pass` | `1password` | `vault` | `keyring`) and writes it to stdout exactly once.
**Nothing in `internal/secrets` may log key bytes.** The `file` backend supports multi-key sets
(`<vendor>.keys.json`) with per-key enable/disable and rotation (`off`/`round_robin`/`random`).

### Config & on-disk layout (`internal/config`)

- `~/.config/cc-fleet/vendors.toml` — single source of truth users edit (schema `version = 1`,
  mode `0600`). `config.Load` is **strict**: an invalid `secret_backend`/`key_rotation`/version
  is rejected, not defaulted. Honors `$XDG_CONFIG_HOME`.
- `~/.config/cc-fleet/secrets/` — file-backend keys (gitignored, `0700`/`0600`).
- `~/.claude/profiles/<vendor>.json` — generated spawn profiles (regenerate with `repair`).
- `~/.claude/teams/<team>/config.json` — native Claude team state; cc-fleet appends `Member`
  rows here so panes/teammates are discoverable and teardownable.

### Concurrency: file locks (`internal/config/lock.go`)

cc-fleet is invoked as **many short-lived external processes**, so it cannot serialize
in-process — it uses `flock`. Three disjoint scopes:

- `WithTeamLock(team)` — `~/.claude/teams/<team>/.cc-fleet-lock`. **Every** mutation of per-team
  state (config.json members, inbox, profile install) runs under it.
- `WithServerLock` — global `~/.claude/.cc-fleet-tmux.lock`. Serializes tmux split/layout races
  that span teams (per-team locks sit on different inodes and can't).
- `WithVendorsConfigLock` — global `<ConfigDir>/.cc-fleet-vendors.lock`. Guards the
  `vendors.toml` load→mutate→save cycle (`add`/`edit`/`remove`).

**Lock ordering (no cycles):** vendors-config OUTERMOST, then team, then server INNERMOST.

### The JSON envelope contract

`spawn.Spawn` and `subagent.Run` **never return a Go error** — they always return a `Result`
with `ok: true|false` and, on failure, a stable `error_code` + suggestion. The `--json` output
is the CLI↔skill contract; the skill dispatches on `error_code`. Preserve this discipline when
editing those packages: turn failures into classified Results, don't propagate raw errors.

### Name validation is a security boundary (`internal/ids`)

Vendor/team/agent names flow into file paths (`filepath.Join` → traversal risk) and into the
`apiKeyHelper` shell string (`<bin> keyget <name>` → injection risk). Validate **before** use:
`ValidateVendorName` / `ValidateTeamName` / `ValidateMemberName`, and `EnsureUnderRoot` to
confirm a constructed path stays inside its ownership root. `config.Load` and `profile`
re-validate as defense-in-depth — keep it that way.

### Cross-platform process introspection

Linux reads `/proc`; macOS has none and falls back to `ps`. This is abstracted by
`internal/procintrospect` (`_linux.go` / `_darwin.go` / `_other.go` stub) and the fingerprint
capture split (`capture.go` + `capture_darwin.go` / `capture_notdarwin.go`). Anything touching
`/proc` or process tables must go through `procintrospect` and stay behind the right build tags.
Supported targets: linux & darwin, amd64 & arm64.

### Layout

- `cmd/cc-fleet/` — Cobra entrypoint; one file per subcommand, all registered in `main.go`.
  Bare `cc-fleet` in an interactive TTY launches the Bubbletea TUI (`internal/tui`: vendors hub
  + agent-status board); non-interactive contexts fall through to `--help`.
- User-layer commands: `init`/`add`/`edit`/`remove`/`list`/`repair`/`uninstall`.
  Claude-layer (machine-driven): `spawn`/`subagent`/`ps`/`hide`/`show`/`teardown`/`keyget`/
  `refresh-fingerprint`.

## Editing the skill (canonical vs local copy)

The skill's canonical source is **`skills/cc-fleet/`** (SKILL.md + `references/`). The repo also
has a gitignored install copy at `.claude/skills/cc-fleet/` that `make skill-sync` refreshes
from canonical. **Edit only the canonical source**, then run `make skill-sync`;
`make skill-drift-check` fails if the two diverge.

## Distribution

GoReleaser builds + GitHub release on a `v*` tag (`.github/workflows/release.yml`). Also shipped
via npm (`npm/`, postinstall fetches the platform binary), the one-line `install.sh`, and the
Claude Code plugin marketplace (`.claude-plugin/`, with `commands/` slash commands and a
SessionStart hook in `hooks/`).
