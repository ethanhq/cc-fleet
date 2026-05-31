---
description: Run cc-fleet's setup/health diagnostics and explain any failures (read-only)
disable-model-invocation: true
allowed-tools: Bash
---

!`cc-fleet doctor 2>&1 || echo "(cc-fleet not available — install it and ensure it is on PATH)"`

Read the diagnostics above and give the user a short, actionable summary:
- which checks pass, warn, or fail,
- for each WARN/FAIL, the concrete next step (the check's own fix hint is usually the answer).

Note: "skill installed" check may WARN if the skill was installed via this plugin rather than `make install-skill` — that is expected and not a problem. This is a **read-only** diagnostic; do not change any config.
