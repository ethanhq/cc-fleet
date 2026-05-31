# AGENTS.md

This repo's agent/AI guidance lives in **[`CLAUDE.md`](CLAUDE.md)** — it is the single source
of truth for any coding agent (Claude Code, Codex, Cursor, etc.). Read it before editing:
what cc-fleet is, the two execution modes, the architecture, the load-bearing invariants, and
the build/test commands.

The contribution standard (required checks, commit/PR rules, screenshots, AI attribution) is
**[`CONTRIBUTING.md`](CONTRIBUTING.md)**.

## Coding standards (the essentials)

- **Minimal intrusion.** Change the fewest lines that solve the task. No out-of-scope refactor,
  rename, or reformat. Match the surrounding style.
- **Simplest correct implementation.** No speculative abstraction, dependency, or config
  surface (YAGNI). Reuse an existing helper before adding one.
- **Concise comments.** Explain *why*, not *what*; no narration or ticket/changelog notes. Fix
  comments your change makes stale.
- **Respect the invariants** documented in CLAUDE.md: keys never in env/argv/history; classified
  `Result` envelopes from `spawn`/`subagent`; validate names before use; honor the lock order.
- **Verify before done:** `go test -race ./...`, `gofmt -l .`, `go vet ./...` clean; plus
  `claude plugin validate . --strict` if you touched the plugin/skill.

## AI attribution

- **AI-*assisted*** (a human authored/reviewed the diff) → `Co-Authored-By:` trailer naming the
  tool/model in the commit message.
- **Fully AI-*authored*** PR → add the autonomous-PR marker as the last line of the PR body
  (see `CONTRIBUTING.md`).
