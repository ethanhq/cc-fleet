---
description: Run cc-fleet's setup/health diagnostics and explain any failures (read-only)
disable-model-invocation: true
allowed-tools: Bash
---

!`cc-fleet doctor 2>&1 || echo "(cc-fleet not available — install it and ensure it is on PATH)"`

Read the diagnostics above and give the user a short, actionable summary:
- which checks pass, warn, or fail,
- for each WARN/FAIL, the concrete next step (the check's own fix hint is usually the answer).

Note: the "skills installed" check is OK when the per-lane skills (subagent / team / workflow) are present via plugin OR `make install-skill`; it WARNs only on a partial install or if the legacy single skill coexists with the new ones. This is a **read-only** diagnostic; do not change any config.
