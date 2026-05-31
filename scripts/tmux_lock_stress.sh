#!/usr/bin/env bash
# tmux_lock_stress.sh — mechanism-level stress test for cc-fleet's pane-creation
# lock SCOPE. Reproduces cc-fleet's locked tmux section
#   display-message (leader) -> list-panes (count) -> split-window
#     [-> select-layout main-vertical + resize-pane leader 30%]
# running `sleep 30` panes (NO vendor / NO claude) concurrently across processes
# on a throwaway tmux socket, under 3 lock regimes:
#
#   sameteam : all N spawns share ONE lock  == cc-fleet spawning N teammates
#              into ONE team (common skill case)               -> expect SAFE
#   crossteam: each spawn uses its OWN lock  == N DISTINCT teams each spawning
#              concurrently into the same window (per-team locks don't
#              cross-serialize)                                -> expect RACE
#   global   : all N spawns share ONE global lock
#              (config.WithServerLock)                         -> expect SAFE
#
# Observed (tmux 3.4): crossteam loses panes ("no space for new pane") at N>=12;
# sameteam + global are clean. This is the empirical basis for WithServerLock.
#
# NOTE: this tests the MECHANISM (a shell replica). A real-binary follow-up
# (concurrent `./bin/cc-fleet spawn` across distinct teams, --no-probe, with
# git-stash before/after) is the gold-standard validation.
set -u
SOCK="ccfstress_$$"; TMUX="tmux -L $SOCK"; WORK="$(mktemp -d)"; ERR="$WORK/tmux.err"
cleanup(){ $TMUX kill-server 2>/dev/null; rm -rf "$WORK"; }; trap cleanup EXIT

spawn_once(){ # $1=lockfile  $2=target
  ( flock 9
    leader=$($TMUX display-message -p -t "$2" '#{pane_id}' 2>/dev/null) || { echo FAIL; exit; }
    count=$($TMUX list-panes -t "$leader" -F '#{pane_id}' 2>/dev/null | grep -c .)
    if [ "${count:-0}" -le 1 ]; then
      $TMUX split-window -t "$leader" -h -l 70% -d -P -F '#{pane_id}' 'sleep 30' >/dev/null 2>>"$ERR" || { echo FAIL; exit; }
    else
      $TMUX split-window -t "$leader" -v -d -P -F '#{pane_id}' 'sleep 30' >/dev/null 2>>"$ERR" || { echo FAIL; exit; }
      $TMUX select-layout -t "$leader" main-vertical 2>>"$ERR" || true
      $TMUX resize-pane -t "$leader" -x 30% 2>>"$ERR" || true
    fi
    echo OK
  ) 9>"$1"
}

run(){ # $1=label $2=strategy $3=N
  local label="$1" strat="$2" N="$3" i lock
  $TMUX kill-server 2>/dev/null; sleep 0.2
  $TMUX new-session -d -s s -x 220 -y 60
  local target; target=$($TMUX list-panes -t s -F '#{pane_id}' | head -1)
  : > "$ERR"; rm -f "$WORK"/o_*
  for i in $(seq 1 "$N"); do
    case "$strat" in
      sameteam)  lock="$WORK/oneteam";;
      crossteam) lock="$WORK/team_$i";;
      global)    lock="$WORK/global";;
    esac
    spawn_once "$lock" "$target" > "$WORK/o_$i" &
  done
  wait
  local fails panes expected lw
  fails=$(cat "$WORK"/o_* 2>/dev/null | grep -c FAIL)
  panes=$($TMUX list-panes -t s -F '#{pane_id}' 2>/dev/null | grep -c .)
  expected=$((1 + N)); lw=$($TMUX display-message -p -t "$target" '#{pane_width}' 2>/dev/null)
  printf '  %-42s split_fails=%-3s panes=%s/%s leader_w=%s\n' "$label" "$fails" "$panes" "$expected" "${lw:-?}"
}

echo "tmux $(tmux -V | awk '{print $2}')  ·  N concurrent spawns into ONE window, across regimes"
for N in 12 20; do
  echo "── N=$N concurrent spawns (expected final panes=$((1+N))) ──"
  run "sameteam  (cc-fleet: N teammates, 1 team)"     sameteam  "$N"
  run "crossteam (N teams x1, same window)"           crossteam "$N"
  run "global    (single global lock / WithServerLock)" global  "$N"
done
