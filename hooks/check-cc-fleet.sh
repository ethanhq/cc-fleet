#!/usr/bin/env sh
# cc-fleet plugin · SessionStart binary check.
#
# This plugin ships ONLY the skill (+ this hook + read-only commands). The
# native `cc-fleet` Go binary is installed SEPARATELY onto PATH — a plugin
# can't compile or reliably ship a native binary (no install-time hook; the
# plugin root path is ephemeral and would break the apiKeyHelper pin baked
# into vendor profiles).
#
# If the binary is missing, the skill can still route a request but every CLI
# call it makes will fail, so we nudge once at session start.
#
# Contract: SessionStart hooks must NEVER block the session — we always exit 0.
#   - binary present  -> print nothing (no context noise)
#   - binary missing  -> one-line install hint on stdout (Claude Code folds it
#                        into the session context and shows the user)
if command -v cc-fleet >/dev/null 2>&1; then
  exit 0
fi

echo "cc-fleet binary not found on PATH — the cc-fleet skills need it to spawn vendor teammates/subagents. Install: go install github.com/ethanhq/cc-fleet/cmd/cc-fleet@latest  (or grab a release / see https://github.com/ethanhq/cc-fleet#install)."
exit 0
