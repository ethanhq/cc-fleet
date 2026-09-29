#!/bin/sh
# cc-fleet PreToolUse(Agent) guard: fail-CLOSED for ccf-* teammate types, no-op for everything else.
input=$(cat)
case "$input" in
  *'"subagent_type":"ccf-'*|*'"subagent_type": "ccf-'*) ;;
  *) exit 0 ;;
esac
ccf=$(command -v cc-fleet 2>/dev/null)
if [ -z "$ccf" ]; then
  echo "cc-fleet: TEAMMATE_SETUP_REQUIRED(cc_fleet_not_on_path): provider teammate blocked — install cc-fleet or use a native agent type." >&2
  exit 2
fi
out=$(printf '%s' "$input" | "$ccf" teammate guard --protocol 1 2>/dev/null); rc=$?
if [ "$rc" -eq 0 ]; then
  [ -n "$out" ] && printf '%s\n' "$out"
  exit 0
fi
echo "cc-fleet: teammate guard unavailable (exit $rc) — provider teammate blocked. Update cc-fleet and the plugin together (cc-fleet update; /plugin update)." >&2
exit 2
