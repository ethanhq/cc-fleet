<!--
Please read .github/CONTRIBUTING.md before opening this PR.
Fill in each section; delete the HTML comments.
-->

## Summary

<!-- What does this PR change, and why? One or two sentences. -->

## Type of change

- [ ] Bug fix
- [ ] New feature
- [ ] Docs
- [ ] Tests
- [ ] Refactor / chore

## Related issue

<!-- e.g. Closes #123 -->

## Screenshots / recording

<!--
REQUIRED for any UI change or bug fix: attach a screenshot or GIF showing the
before/after, or the bug reproduced and then fixed. Delete this section only if
the change has no visible or behavioral effect.
-->

## Checklist

- [ ] `go test -race ./...` passes
- [ ] `gofmt -l .` prints nothing and `go vet ./...` is clean
- [ ] `make build` compiles
- [ ] If plugin/skill touched: `claude plugin validate . --strict` is green
- [ ] Commits follow Conventional Commits; AI-assisted commits carry a `Co-Authored-By` trailer

<!--
If this PR was generated end-to-end by an AI agent with no human authoring the diff,
add the autonomous-PR marker as the LAST line of this body (see .github/CONTRIBUTING.md):

> 🤖 This PR was generated autonomously by an AI agent. A human is accountable for it: <name/handle>.
-->
