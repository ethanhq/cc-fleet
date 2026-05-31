#!/usr/bin/env bash
# release/install.sh — install a PRE-BUILT cc-fleet from a release archive.
#
# This is the copy-binary installer shipped INSIDE each cc-fleet-<os>-<arch>.tar.gz.
# It does NOT build from source (no Go toolchain needed): it copies the prebuilt
# binary sitting next to it onto your PATH, creates the `ccf` alias, and installs
# the cc-fleet skill. For a from-source build, use the repo's top-level
# install.sh instead.

set -euo pipefail

DEFAULT_PREFIX="${HOME}/.local/bin"
PREFIX="${DEFAULT_PREFIX}"
SKILL_DIR="${HOME}/.claude/skills/cc-fleet"
INSTALL_SKILL=1

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    cat <<EOF
install.sh — install prebuilt cc-fleet from this release archive.

Usage:
    ./install.sh [--prefix DIR] [--skill-dir DIR] [--no-skill] [--help]

Options:
    --prefix DIR      Install the binary into DIR/cc-fleet (+ ccf alias).
                      Default: ${DEFAULT_PREFIX}
    --skill-dir DIR   Install SKILL.md (+ references/) into DIR.
                      Default: ${SKILL_DIR}
    --no-skill        Install the binary only; skip the skill. Use this if you
                      get the skill from the cc-fleet plugin (avoids a duplicate).
    -h, --help        Show this help and exit.
EOF
}

# --- Parse args ---------------------------------------------------------------

while [[ $# -gt 0 ]]; do
    case "$1" in
        --prefix)
            if [[ $# -lt 2 ]]; then
                echo "install.sh: --prefix requires a directory argument" >&2
                exit 2
            fi
            PREFIX="$2"
            shift 2
            ;;
        --prefix=*)
            PREFIX="${1#--prefix=}"
            shift
            ;;
        --skill-dir)
            if [[ $# -lt 2 ]]; then
                echo "install.sh: --skill-dir requires a directory argument" >&2
                exit 2
            fi
            SKILL_DIR="$2"
            shift 2
            ;;
        --skill-dir=*)
            SKILL_DIR="${1#--skill-dir=}"
            shift
            ;;
        --no-skill)
            INSTALL_SKILL=0
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "install.sh: unknown argument: $1" >&2
            echo "Run './install.sh --help' for usage." >&2
            exit 2
            ;;
    esac
done

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
# Relative symlink (same as `make install-bin` / top-level install.sh).
# os.Executable() resolves it, so a spawned teammate's apiKeyHelper still points
# at the real cc-fleet path.
ln -sf cc-fleet "${PREFIX}/ccf"
echo "==> Installed: ${PREFIX}/cc-fleet (+ ccf alias)"

# --- Skill --------------------------------------------------------------------
# Installed by default. Plugin users should pass --no-skill (the plugin already
# delivers the skill) so they don't end up with two copies. See README.md.

if [[ "${INSTALL_SKILL}" -eq 1 ]]; then
    if [[ -f "${SCRIPT_DIR}/SKILL.md" ]]; then
        mkdir -p "${SKILL_DIR}"
        cp "${SCRIPT_DIR}/SKILL.md" "${SKILL_DIR}/SKILL.md"
        # Progressive-disclosure references travel with the skill.
        if [[ -d "${SCRIPT_DIR}/references" ]]; then
            mkdir -p "${SKILL_DIR}/references"
            cp "${SCRIPT_DIR}/references/"*.md "${SKILL_DIR}/references/"
        fi
        echo "==> Installed skill: ${SKILL_DIR}/SKILL.md"
    fi
else
    echo "==> Skipped skill install (--no-skill); get the skill from the cc-fleet plugin."
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

   cc-fleet init        # create config at ~/.config/cc-fleet/
   cc-fleet add <vendor> ... --api-key-stdin <<<"\$KEY"   # register a vendor
   cc-fleet doctor      # health-check

   See README.md in this archive for the full quick-start.
EOF
