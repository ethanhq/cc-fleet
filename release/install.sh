#!/usr/bin/env bash
# release/install.sh — install a PRE-BUILT cc-fleet from a release archive.
#
# Shipped INSIDE each cc-fleet-<os>-<arch>.tar.gz. It copies the prebuilt binary
# sitting next to it onto your PATH (no Go toolchain, no download), creates the
# `ccf` alias, and installs the cc-fleet skill — by default via the Claude Code
# plugin. For a from-source build, clone the repo and run `make install`.

set -euo pipefail

REPO="ethanhq/cc-fleet"      # GitHub owner/repo (plugin marketplace source)
MARKETPLACE="ethanhq"        # claude plugin marketplace name
PLUGIN="cc-fleet"            # claude plugin name
SKILL_NAME="cc-fleet"        # skill dir under ~/.claude/skills/ (for --skill global)

DEFAULT_PREFIX="${HOME}/.local/bin"
PREFIX="${DEFAULT_PREFIX}"
SKILL_MODE="plugin"          # plugin | global | none
SCOPE="user"                 # user | project | local (only for --skill plugin)
SKILL_DIR="${HOME}/.claude/skills/${SKILL_NAME}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    cat <<EOF
install.sh — install prebuilt cc-fleet from this release archive.

Usage:
    ./install.sh [options]

Options:
    --skill plugin|global|none  How to install the skill. Default: plugin.
                                  plugin = via Claude Code plugin (also adds the
                                           SessionStart hook + /ps //doctor commands).
                                  global = copy the bundled SKILL.md into a skill dir.
                                  none   = binary only.
    --scope user|project|local  Plugin install scope (--skill plugin). Default: user.
    --prefix DIR                Install the binary into DIR. Default: ${DEFAULT_PREFIX}.
    --skill-dir DIR             Skill dir for --skill global. Default: ${SKILL_DIR}.
    -h, --help                  Show this help and exit.
EOF
}

# --- Parse args ---------------------------------------------------------------

while [[ $# -gt 0 ]]; do
    case "$1" in
        --skill)     SKILL_MODE="${2:?--skill requires a value}"; shift 2 ;;
        --skill=*)   SKILL_MODE="${1#--skill=}"; shift ;;
        --scope)     SCOPE="${2:?--scope requires a value}"; shift 2 ;;
        --scope=*)   SCOPE="${1#--scope=}"; shift ;;
        --prefix)    PREFIX="${2:?--prefix requires a value}"; shift 2 ;;
        --prefix=*)  PREFIX="${1#--prefix=}"; shift ;;
        --skill-dir) SKILL_DIR="${2:?--skill-dir requires a value}"; shift 2 ;;
        --skill-dir=*) SKILL_DIR="${1#--skill-dir=}"; shift ;;
        -h|--help)   usage; exit 0 ;;
        *)
            echo "install.sh: unknown argument: $1" >&2
            echo "Run './install.sh --help' for usage." >&2
            exit 2
            ;;
    esac
done

case "$SKILL_MODE" in plugin|global|none) ;; *) echo "install.sh: --skill must be plugin|global|none" >&2; exit 2 ;; esac
case "$SCOPE" in user|project|local) ;; *) echo "install.sh: --scope must be user|project|local" >&2; exit 2 ;; esac

# --- Sanity check -------------------------------------------------------------

if [[ ! -x "${SCRIPT_DIR}/cc-fleet" ]]; then
    echo "install.sh: prebuilt 'cc-fleet' binary not found next to this script" >&2
    echo "  (expected ${SCRIPT_DIR}/cc-fleet — is this the extracted release archive?)" >&2
    exit 1
fi

# --- Binary + ccf alias -------------------------------------------------------

mkdir -p "${PREFIX}"
cp "${SCRIPT_DIR}/cc-fleet" "${PREFIX}/cc-fleet"
chmod +x "${PREFIX}/cc-fleet"
# Relative symlink (same as `make install-bin` / the top-level installer).
# os.Executable() resolves it, so a spawned teammate's apiKeyHelper still points
# at the real cc-fleet path.
ln -sf cc-fleet "${PREFIX}/ccf"
echo "==> Installed: ${PREFIX}/cc-fleet (+ ccf alias)"

# Install manifest (co-located with the binary) so `cc-fleet update` self-updates
# in place without guessing the method, preserves the plugin scope, and leaves
# the plugin alone for a --skill none/global install.
cat > "${PREFIX}/.cc-fleet-install.json" <<EOF
{"method":"tarball","plugin_scope":"${SCOPE}","skill":"${SKILL_MODE}"}
EOF

# --- Skill --------------------------------------------------------------------

case "$SKILL_MODE" in
    plugin)
        if command -v claude >/dev/null 2>&1; then
            claude plugin marketplace add "${REPO}" --scope "${SCOPE}" >/dev/null 2>&1 || true
            if claude plugin install "${PLUGIN}@${MARKETPLACE}" --scope "${SCOPE}"; then
                echo "==> Installed the cc-fleet skill via plugin (scope: ${SCOPE})"
                echo "    uninstall: claude plugin uninstall ${PLUGIN}@${MARKETPLACE}"
            else
                echo "==> Could not install the plugin automatically. To add the skill, run:" >&2
                echo "    claude plugin marketplace add ${REPO} --scope ${SCOPE}" >&2
                echo "    claude plugin install ${PLUGIN}@${MARKETPLACE} --scope ${SCOPE}" >&2
            fi
        else
            echo "==> 'claude' not on PATH — skipped plugin install. To add the skill later:"
            echo "    claude plugin marketplace add ${REPO} --scope ${SCOPE}"
            echo "    claude plugin install ${PLUGIN}@${MARKETPLACE} --scope ${SCOPE}"
            echo "    (or re-run with --skill global to copy the bundled skill instead)"
        fi
        ;;
    global)
        if [[ -f "${SCRIPT_DIR}/SKILL.md" ]]; then
            mkdir -p "${SKILL_DIR}"
            cp "${SCRIPT_DIR}/SKILL.md" "${SKILL_DIR}/SKILL.md"
            if [[ -d "${SCRIPT_DIR}/references" ]]; then
                mkdir -p "${SKILL_DIR}/references"
                cp "${SCRIPT_DIR}/references/"*.md "${SKILL_DIR}/references/"
            fi
            echo "==> Installed the cc-fleet skill (global) to ${SKILL_DIR}/"
        else
            echo "install.sh: SKILL.md not found next to this script — cannot --skill global" >&2
            exit 1
        fi
        ;;
    none)
        echo "==> Skipped skill install (--skill none)"
        ;;
esac

# --- Claude Code precondition note --------------------------------------------

if ! command -v claude >/dev/null 2>&1; then
    cat <<EOF

==> Note: Claude Code ('claude') was not found on PATH.
    cc-fleet drives Claude Code — install it to run teammates / subagents / workflows:
    https://docs.anthropic.com/claude-code
EOF
fi

# --- PATH check ---------------------------------------------------------------

case ":${PATH}:" in
    *":${PREFIX}:"*)
        echo "==> ${PREFIX} is already on PATH."
        ;;
    *)
        cat <<EOF

  ${PREFIX} is not on your PATH. Add this line to your shell rc
   (~/.zshrc, ~/.bashrc, ~/.profile, depending on your shell):

      export PATH="${PREFIX}:\$PATH"
EOF
        ;;
esac

# --- Next steps ---------------------------------------------------------------

cat <<EOF

==> Next steps

   cc-fleet             # launch the interactive TUI — register a vendor and get started
                        #   (config is auto-created on first save; no init needed)
   cc-fleet doctor      # optional: run the health checks
   cc-fleet update      # later: update cc-fleet + the plugin to the latest release

   tmux is needed only for live teammates; subagent / workflow / run work without it.

   See README.md in this archive for the full quick-start.
EOF
