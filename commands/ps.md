---
description: List cc-fleet vendor teammates, async jobs, and pane health (read-only)
disable-model-invocation: true
allowed-tools: Bash
---

!`cc-fleet ps --check 2>&1 || echo "(cc-fleet not available — install it and ensure it is on PATH)"`

Summarize the status above for the user:
- how many vendor teammates are running, and on which teams,
- any unhealthy / unreachable panes,
- any async subagent jobs and their state.

This is a **read-only** view — do not spawn, hide/show, or tear down anything from this command.
