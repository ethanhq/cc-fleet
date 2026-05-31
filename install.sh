#!/usr/bin/env bash
# install.sh — build cc-fleet from source and copy the binary into a bin dir on PATH.
#
# Default install prefix: ~/.local/bin (cargo / pyenv style).
# Use --prefix to override (e.g. /usr/local/bin, /opt/cc-fleet/bin).
#
# This script only installs the binary. The vendor-fleet skill is installed
# separately via `make install-skill` so users can opt in / opt out per machine.

set -euo pipefail

DEFAULT_PREFIX="${HOME}/.local/bin"
PREFIX="${DEFAULT_PREFIX}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    cat <<EOF
install.sh — build cc-fleet and copy the binary onto your PATH.

Usage:
    ./install.sh [--prefix DIR] [--help]

Options:
    --prefix DIR    Install the binary into DIR/cc-fleet.
                    Default: ${DEFAULT_PREFIX}
                    Common alternatives: /usr/local/bin, /opt/cc-fleet/bin
    -h, --help      Show this help and exit.

Examples:
    ./install.sh                                # installs to ~/.local/bin/cc-fleet
    ./install.sh --prefix /usr/local/bin        # may require sudo
    ./install.sh --prefix \$HOME/bin

After install, run:
    cc-fleet init        # create config tree at ~/.config/cc-fleet/
    cc-fleet doctor      # health-check the install
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

# --- Sanity checks ------------------------------------------------------------

if ! command -v go >/dev/null 2>&1; then
    cat >&2 <<EOF
install.sh: 'go' not found on PATH.

cc-fleet is a Go program and needs the Go toolchain (>= 1.24) to build from
source. Install Go from https://go.dev/dl/ and re-run this script.
EOF
    exit 1
fi

GO_VERSION="$(go version 2>/dev/null || echo 'unknown')"

# --- Build --------------------------------------------------------------------

DEST="${PREFIX}/cc-fleet"

echo "==> Building cc-fleet (${GO_VERSION})"
echo "    source : ${SCRIPT_DIR}"
echo "    target : ${DEST}"

mkdir -p "${PREFIX}"

# Build directly to the install location. We build from SCRIPT_DIR so this script
# works no matter where the user runs it from.
#
# -buildvcs=false: a `git archive` extraction (release tarball) has no .git dir,
# so a default `go build` aborts with "error obtaining VCS status: exit 128".
# Disabling the VCS stamp only drops informational build metadata and has no
# downside for a normal git-clone build, so we set it unconditionally rather
# than try to detect the archive case.
(
    cd "${SCRIPT_DIR}"
    go build -buildvcs=false -o "${DEST}" ./cmd/cc-fleet
)

chmod +x "${DEST}"

echo "==> Installed: ${DEST}"

# Create the `ccf` short alias as a relative symlink next to the binary, the
# same way `make install-bin` does. os.Executable() resolves the symlink, so a
# spawned teammate's apiKeyHelper still points at the real cc-fleet path.
ln -sf cc-fleet "${PREFIX}/ccf"
echo "==> ccf alias: ${PREFIX}/ccf -> cc-fleet"

# --- PATH check ---------------------------------------------------------------

# Use ":${PATH}:" with sentinels so we match exact entries, not substrings.
case ":${PATH}:" in
    *":${PREFIX}:"*)
        echo "==> ${PREFIX} is already on PATH."
        ;;
    *)
        cat <<EOF

  ${PREFIX} is not on your PATH. Add this line to your shell rc
   (~/.zshrc, ~/.bashrc, ~/.profile, depending on your shell):

      export PATH="${PREFIX}:\$PATH"

   Then start a new shell, or run: source ~/.zshrc  (or ~/.bashrc)
EOF
        ;;
esac

# --- Next steps ---------------------------------------------------------------

cat <<EOF

==> Next steps

   cc-fleet init                # create config at ~/.config/cc-fleet/
   cc-fleet add <vendor> ...    # register your first vendor
   cc-fleet doctor              # health-check
   make install-skill           # (optional) install the vendor-fleet skill

   See README.md for the full quick-start.
EOF
