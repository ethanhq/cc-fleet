#!/bin/sh
# e2e-teammate.sh — canary environment for the provider teammate lane. Re-run it after
# every Claude Code upgrade: the lane depends on CC's teammate command contract.
#
#   sh scripts/e2e-teammate.sh iso-setup <dir> [--claude <file under ~/.local/share/claude/versions>]
#   sh scripts/e2e-teammate.sh lead <dir> [claude args...]
#   sh scripts/e2e-teammate.sh cleanup <dir>
#
# iso-setup  builds an isolated HOME under <dir> (HOME, CLAUDE_CONFIG_DIR, XDG_CONFIG_HOME),
#            copies this repo's bin/cc-fleet (run `make build` first), links claude the way
#            the native installer lays it out (symlink on PATH, regular file under
#            versions/), pre-seeds onboarding + trust in .claude.json, and makes two git
#            projects under /tmp/ccfe.*. It never touches providers or keys: it prints the
#            import commands to run next. Everything is recorded in <dir>/ccfe.env.
# lead       starts an ISO lead (`cc-fleet run <provider>`) in `tmux -L ccfe2e`.
#            CCFE_PROVIDER (default openrouter) and CCFE_PROJ (projA | projB, default projA)
#            pick the provider and the project.
# cleanup    kills every tmux server of the ISO TMUX_TMPDIR, then removes TMUX_TMPDIR and the
#            project dir. <dir> itself (evidence) is left in place.
#
# Writes only under <dir> and /tmp/ccfe.*. Never reads or copies a key.

set -eu

repo=$(cd "$(dirname "$0")/.." && pwd)

die() { echo "e2e-teammate.sh: $*" >&2; exit 1; }

# q prints $1 as a single-quoted sh word.
q() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }

abs_dir() { (cd "$1" && pwd -P); }

load_env() {
    [ -f "$1/ccfe.env" ] || die "$1/ccfe.env not found — run: sh $0 iso-setup $1"
    # shellcheck disable=SC1091
    . "$1/ccfe.env"
}

iso_setup() {
    [ $# -ge 1 ] || die "usage: iso-setup <dir> [--claude <file>]"
    dir=$1; shift
    claude=
    while [ $# -gt 0 ]; do
        case "$1" in
            --claude) claude=${2:?--claude needs a file}; shift 2 ;;
            --claude=*) claude=${1#--claude=}; shift ;;
            *) die "iso-setup: unknown argument: $1" ;;
        esac
    done
    if [ -z "$claude" ]; then
        vdir="$HOME/.local/share/claude/versions"
        [ -d "$vdir" ] || die "no $vdir — pass --claude <file>"
        newest=$(ls "$vdir" | command grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1)
        [ -n "$newest" ] || die "no versioned claude under $vdir — pass --claude <file>"
        claude="$vdir/$newest"
    fi
    [ -f "$claude" ] && [ -x "$claude" ] && [ -s "$claude" ] || die "--claude must be a non-empty executable regular file: $claude"
    [ -x "$repo/bin/cc-fleet" ] || die "$repo/bin/cc-fleet not found — run: make -C $repo build"

    mkdir -p "$dir"
    T=$(abs_dir "$dir")
    [ ! -e "$T/ccfe.env" ] || die "$T is already set up (found ccfe.env); run cleanup or pick a new dir"

    mkdir -p "$T/bin" "$T/home/.claude" "$T/home/.config" "$T/home/.local/share/claude/versions"
    cp "$repo/bin/cc-fleet" "$T/bin/cc-fleet"
    ln -s "$claude" "$T/bin/claude"
    ver=$(basename "$claude")
    # A hard link keeps versions/<v> a regular file, as in a real install.
    if ! ln "$claude" "$T/home/.local/share/claude/versions/$ver" 2>/dev/null; then
        echo "note: hard link failed (different volume?) — copying $claude instead"
        cp "$claude" "$T/home/.local/share/claude/versions/$ver"
    fi

    # Short socket paths: a unix socket path is limited to 104 bytes. Physical paths, so
    # they match the cwd claude records.
    tt=$(abs_dir "$(mktemp -d /tmp/ccfe.XXXXXX)")
    pj=$(abs_dir "$(mktemp -d /tmp/ccfe.XXXXXX)")
    for p in projA projB; do
        mkdir -p "$pj/$p"
        printf '# %s\n' "$p" > "$pj/$p/README.md"
        env HOME="$T/home" GIT_CONFIG_NOSYSTEM=1 git -C "$pj/$p" init -q
        env HOME="$T/home" GIT_CONFIG_NOSYSTEM=1 git -C "$pj/$p" add README.md
        env HOME="$T/home" GIT_CONFIG_NOSYSTEM=1 git -C "$pj/$p" \
            -c user.name=ccfe -c user.email=ccfe@localhost commit -q -m init
    done

    cat > "$T/home/.claude/.claude.json" <<EOF
{
  "hasCompletedOnboarding": true,
  "projects": {
    "$pj/projA": { "hasTrustDialogAccepted": true },
    "$pj/projB": { "hasTrustDialogAccepted": true }
  }
}
EOF

    {
        echo "# written by scripts/e2e-teammate.sh iso-setup; source it in an ISO shell"
        echo "export T=$(q "$T")"
        echo "export HOME=$(q "$T/home")"
        echo "export CLAUDE_CONFIG_DIR=$(q "$T/home/.claude")"
        echo "export XDG_CONFIG_HOME=$(q "$T/home/.config")"
        echo "export TMUX_TMPDIR=$(q "$tt")"
        echo "export PJ=$(q "$pj")"
        echo "export PATH=$(q "$T/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin")"
        echo "unset TMUX TMUX_PANE"
    } > "$T/ccfe.env"

    cat <<EOF
ISO environment ready: $T
  claude      $claude (symlink $T/bin/claude, hard link under versions/)
  cc-fleet    $T/bin/cc-fleet
  projects    $pj/projA  $pj/projB
  TMUX_TMPDIR $tt

Next (providers and keys are not handled here; run these yourself, keys stay in the pipe).
Run them from your normal shell: the real cc-fleet must see your real HOME, so only the
ISO side of each command sources ccfe.env, in a subshell.
  <real cc-fleet> export --provider openrouter --out $T/or.toml
  (. $T/ccfe.env && \$T/bin/cc-fleet import \$T/or.toml --json)
  (. $T/ccfe.env && \$T/bin/cc-fleet edit openrouter --base-url http://127.0.0.1:17299 --json)
  <real cc-fleet> keyget openrouter | (. $T/ccfe.env && \$T/bin/cc-fleet edit openrouter --secret-backend file --api-key-stdin --json)
  (. $T/ccfe.env && \$T/bin/cc-fleet teammate setup --yes --json)
Then: sh $0 lead $T
EOF
}

lead() {
    [ $# -ge 1 ] || die "usage: lead <dir> [claude args...]"
    load_env "$1"; shift
    provider=${CCFE_PROVIDER:-openrouter}
    case "${CCFE_PROJ:-projA}" in projA|projB) proj="$PJ/${CCFE_PROJ:-projA}" ;; *) die "CCFE_PROJ must be projA or projB" ;; esac
    [ -d "$proj" ] || die "project dir missing: $proj"

    cmd="$(q "$T/bin/cc-fleet") run $(q "$provider") --permission-mode default -- --plugin-dir $(q "$repo") --teammate-mode tmux"
    for a in "$@"; do cmd="$cmd $(q "$a")"; done

    # A clean environment: nothing from the calling session (TMUX, CLAUDECODE, session ids)
    # reaches the ISO tmux server or the lead.
    env -i HOME="$HOME" CLAUDE_CONFIG_DIR="$CLAUDE_CONFIG_DIR" XDG_CONFIG_HOME="$XDG_CONFIG_HOME" \
        TMUX_TMPDIR="$TMUX_TMPDIR" PATH="$PATH" TERM="${TERM:-xterm-256color}" LANG="${LANG:-en_US.UTF-8}" \
        USER="${USER:-$(id -un)}" SHELL=/bin/sh \
        tmux -L ccfe2e new-session -d -s lead -x 220 -y 60 -c "$proj" "$cmd"
    cat <<EOF
lead started: tmux -L ccfe2e, session 'lead', cwd $proj, provider $provider
  drive:   TMUX_TMPDIR=$TMUX_TMPDIR tmux -L ccfe2e send-keys -t lead '<text>' Enter
  capture: TMUX_TMPDIR=$TMUX_TMPDIR tmux -L ccfe2e capture-pane -p -S - -t lead
  attach:  TMUX_TMPDIR=$TMUX_TMPDIR tmux -L ccfe2e attach -t lead
EOF
}

cleanup() {
    [ $# -eq 1 ] || die "usage: cleanup <dir>"
    load_env "$1"
    sockdir="$TMUX_TMPDIR/tmux-$(id -u)"
    if [ -d "$sockdir" ]; then
        for s in "$sockdir"/*; do
            [ -S "$s" ] || continue
            tmux -S "$s" kill-server 2>/dev/null || true
            echo "killed tmux server $s"
        done
    fi
    for d in "$TMUX_TMPDIR" "$PJ"; do
        case "$d" in
            /tmp/ccfe.*|/private/tmp/ccfe.*) rm -rf "$d"; echo "removed $d" ;;
            *) echo "refusing to remove $d (not under /tmp/ccfe.*)" >&2 ;;
        esac
    done
    echo "left in place: $T (remove it yourself once the evidence is saved)"
}

[ $# -ge 1 ] || die "usage: $0 iso-setup|lead|cleanup <dir> ..."
sub=$1; shift
case "$sub" in
    iso-setup) iso_setup "$@" ;;
    lead) lead "$@" ;;
    cleanup) cleanup "$@" ;;
    *) die "unknown subcommand: $sub (iso-setup | lead | cleanup)" ;;
esac
