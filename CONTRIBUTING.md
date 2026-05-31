# Contributing to cc-fleet

Thanks for helping improve cc-fleet! PRs of every size are welcome — bug fixes, new vendor
recipes, docs, tests, and features. This document is the contribution standard; please read
it before opening a PR.

## Before you start

- For anything larger than a small fix, **open an issue first** to align on the approach.
- Keep changes **surgical and focused** — one logical change per PR. Split unrelated work.
- Match the surrounding code style; fix comments your change makes stale.

## Development setup

```bash
git clone https://github.com/ethanhq/cc-fleet.git && cd cc-fleet
make build           # -> ./bin/cc-fleet
```

## Before you submit — required checks

Your branch must be green on all of these:

```bash
go test -race ./...      # full suite must pass
gofmt -l .               # must print nothing
go vet ./...             # must be clean
make build               # must compile
```

If you touched the plugin or skill, also run:

```bash
claude plugin validate . --strict   # must stay green
make skill-drift-check              # if you edited the canonical skill
```

## Commits

- Use [Conventional Commits](https://www.conventionalcommits.org/) — e.g. `fix: …`,
  `feat: …`, `docs: …`, `test: …`, `refactor: …`.
- Keep each commit a coherent, buildable step. Squash noise before opening the PR.

### AI-assisted commits

If an AI tool helped you write a commit (you reviewed and own the result), credit it as a
co-author in the commit message trailer:

```
Co-Authored-By: <Tool/Model name> <noreply@example.com>
```

## Pull requests

- Fill in the **[PR template](.github/pull_request_template.md)** — it's applied
  automatically when you open a PR.
- **UI changes or bug fixes must include a screenshot or GIF** showing the before/after or the
  reproduced-then-fixed behavior. A TUI or pane change without a visual is incomplete.
- Link the issue the PR closes (`Closes #123`).
- Make sure the required checks above pass; CI runs them too.

### Fully-automated (AI-authored) PRs

If a PR was **generated end-to-end by an AI agent with no human authoring the diff**, you must
declare it at the **bottom of the PR body** with this marker:

```
> 🤖 This PR was generated autonomously by an AI agent. A human is accountable for it: <name/handle>.
```

This is about transparency, not gatekeeping — autonomous PRs are welcome, they just have to
say so. (AI-*assisted* PRs, where you authored and reviewed the change, only need the
co-author trailer above — no PR-body marker.)

## Review

Maintainers review for correctness, security (keys must never reach env/argv/history),
scope, and adherence to the invariants documented in the codebase. Expect requests for tests
and screenshots where relevant. Thanks again for contributing!
