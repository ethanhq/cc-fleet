#!/usr/bin/env sh
# install.sh — one-line installer for cc-fleet.
#
# Downloads a prebuilt binary from GitHub Releases (no Go toolchain, no clone),
# installs `cc-fleet` plus the `ccf` alias, and — by default — installs the
# cc-fleet skill via the Claude Code plugin.
#
#   curl -fsSL https://raw.githubusercontent.com/ethanhq/cc-fleet/main/install.sh | sh
#
# Pass flags after `| sh -s --`, e.g.:
#   curl -fsSL .../install.sh | sh -s -- --prefix /usr/local/bin --skill none
#
# For a from-source build instead, clone the repo and run `make install`.

set -eu

REPO="ethanhq/cc-fleet"      # GitHub owner/repo (Release source + plugin marketplace)
MARKETPLACE="ethanhq"        # claude plugin marketplace name (.claude-plugin/marketplace.json)
PLUGIN="cc-fleet"            # claude plugin name
SKILL_NAME="cc-fleet"        # skill dir under ~/.claude/skills/ (for --skill global)

PREFIX="${HOME}/.local/bin"
SKILL_MODE="plugin"          # plugin | global | none
SCOPE="user"                 # user | project | local (only for --skill plugin)
VERSION=""                   # empty => latest release

usage() {
    cat <<EOF
install.sh — install prebuilt cc-fleet from GitHub Releases.

Usage:
    curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install.sh | sh
    curl -fsSL .../install.sh | sh -s -- [options]

Options:
    --skill plugin|global|none  How to install the skill. Default: plugin.
                                  plugin = via Claude Code plugin (also adds the
                                           SessionStart hook + /ps //doctor commands).
                                  global = copy SKILL.md into ~/.claude/skills/${SKILL_NAME}/.
                                  none   = binary only.
    --scope user|project|local  Plugin install scope (--skill plugin). Default: user.
    --prefix DIR                Install the binary into DIR. Default: ${HOME}/.local/bin.
    --version vX.Y.Z            Install a specific release. Default: latest.
    -h, --help                  Show this help and exit.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --skill) SKILL_MODE="${2:?--skill needs a value}"; shift 2 ;;
        --skill=*) SKILL_MODE="${1#--skill=}"; shift ;;
        --scope) SCOPE="${2:?--scope needs a value}"; shift 2 ;;
        --scope=*) SCOPE="${1#--scope=}"; shift ;;
        --prefix) PREFIX="${2:?--prefix needs a value}"; shift 2 ;;
        --prefix=*) PREFIX="${1#--prefix=}"; shift ;;
        --version) VERSION="${2:?--version needs a value}"; shift 2 ;;
        --version=*) VERSION="${1#--version=}"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "install.sh: unknown argument: $1" >&2; echo "Run with --help for usage." >&2; exit 2 ;;
    esac
done

case "$SKILL_MODE" in plugin|global|none) ;; *) echo "install.sh: --skill must be plugin|global|none" >&2; exit 2 ;; esac
case "$SCOPE" in user|project|local) ;; *) echo "install.sh: --scope must be user|project|local" >&2; exit 2 ;; esac

# --- platform detection -------------------------------------------------------

os="$(uname -s)"
case "$os" in
    Linux) os="linux" ;;
    Darwin) os="darwin" ;;
    *) echo "install.sh: unsupported OS '$os' (cc-fleet supports linux and darwin)" >&2; exit 1 ;;
esac

arch="$(uname -m)"
case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    arm64|aarch64) arch="arm64" ;;
    *) echo "install.sh: unsupported architecture '$arch' (cc-fleet supports amd64 and arm64)" >&2; exit 1 ;;
esac

# --- helpers ------------------------------------------------------------------

have() { command -v "$1" >/dev/null 2>&1; }

download() { # url dest
    if have curl; then
        curl -fsSL "$1" -o "$2"
    elif have wget; then
        wget -qO "$2" "$1"
    else
        echo "install.sh: need curl or wget to download" >&2; exit 1
    fi
}

sha256_of() { # file -> stdout hash
    if have sha256sum; then
        sha256sum "$1" | awk '{print $1}'
    elif have shasum; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        echo ""   # no tool available
    fi
}

# --- resolve version ----------------------------------------------------------

if [ -n "$VERSION" ]; then
    case "$VERSION" in v*) ;; *) VERSION="v${VERSION}" ;; esac
fi

if [ -z "$VERSION" ] && [ -z "${CCF_BASE_URL:-}" ]; then
    # Read the tag from the /releases/latest redirect — no JSON parsing needed.
    if have curl; then
        VERSION="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest" | sed 's#.*/tag/##')"
    fi
    [ -n "$VERSION" ] || { echo "install.sh: could not resolve the latest version; pass --version vX.Y.Z" >&2; exit 1; }
fi

# Asset base: the dir holding the tarball + checksums.txt. CCF_BASE_URL overrides
# it for a mirror or a local test (e.g. file:///path/to/dist).
ASSET_BASE="${CCF_BASE_URL:-https://github.com/${REPO}/releases/download/${VERSION}}"
TARBALL="cc-fleet-${os}-${arch}.tar.gz"

# --- download + verify --------------------------------------------------------

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "==> Downloading ${TARBALL} (${VERSION:-local})"
download "${ASSET_BASE}/${TARBALL}" "${tmp}/${TARBALL}"
download "${ASSET_BASE}/checksums.txt" "${tmp}/checksums.txt"

expected="$(awk -v f="${TARBALL}" '$2 == f {print $1}' "${tmp}/checksums.txt")"
actual="$(sha256_of "${tmp}/${TARBALL}")"
if [ -z "$expected" ]; then
    echo "install.sh: no checksum for ${TARBALL} in checksums.txt" >&2; exit 1
elif [ -z "$actual" ]; then
    echo "install.sh: no sha256 tool (sha256sum/shasum) found — cannot verify download" >&2; exit 1
elif [ "$expected" != "$actual" ]; then
    echo "install.sh: checksum mismatch for ${TARBALL}" >&2
    echo "  expected ${expected}" >&2
    echo "  actual   ${actual}" >&2
    exit 1
fi
echo "==> Checksum OK"

# --- extract + install binary -------------------------------------------------

tar -xzf "${tmp}/${TARBALL}" -C "${tmp}"
extract="${tmp}/cc-fleet-${os}-${arch}"   # archives wrap in this dir

mkdir -p "${PREFIX}"
cp "${extract}/cc-fleet" "${PREFIX}/cc-fleet"
chmod 0755 "${PREFIX}/cc-fleet"
# Relative symlink (os.Executable() resolves it, so a teammate's apiKeyHelper
# still points at the real cc-fleet path).
ln -sf cc-fleet "${PREFIX}/ccf"
echo "==> Installed ${PREFIX}/cc-fleet (+ ccf alias)"

# Install manifest (co-located with the binary) so `cc-fleet update` self-updates
# in place without guessing how it was installed.
cat > "${PREFIX}/.cc-fleet-install.json" <<EOF
{"method":"tarball","prefix":"${PREFIX}","plugin_scope":"${SCOPE}","marketplace":"${MARKETPLACE}"}
EOF

# --- skill --------------------------------------------------------------------

case "$SKILL_MODE" in
    plugin)
        if have claude; then
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
        fi
        ;;
    global)
        dir="${HOME}/.claude/skills/${SKILL_NAME}"
        mkdir -p "${dir}/references"
        cp "${extract}/SKILL.md" "${dir}/SKILL.md"
        cp "${extract}/references/"*.md "${dir}/references/"
        echo "==> Installed the cc-fleet skill (global) to ${dir}/"
        ;;
    none)
        echo "==> Skipped skill install (--skill none)"
        ;;
esac

# --- Claude Code precondition note --------------------------------------------

if ! have claude; then
    cat <<EOF

==> Note: Claude Code ('claude') was not found on PATH.
    cc-fleet drives Claude Code — install it to run teammates / subagents / workflows:
    https://docs.anthropic.com/claude-code
EOF
fi

# --- PATH check + next steps --------------------------------------------------

case ":${PATH}:" in
    *":${PREFIX}:"*) echo "==> ${PREFIX} is already on PATH." ;;
    *)
        cat <<EOF

  ${PREFIX} is not on your PATH. Add this to your shell rc
  (~/.zshrc, ~/.bashrc, ~/.profile):

      export PATH="${PREFIX}:\$PATH"
EOF
        ;;
esac

cat <<EOF

==> Next steps

   cc-fleet             # launch the interactive TUI — register a vendor and get started
                        #   (config is auto-created on first save; no init needed)
   cc-fleet doctor      # optional: run the health checks
   cc-fleet update      # later: update cc-fleet + the plugin to the latest release

   tmux is needed only for live teammates; subagent / workflow / run work without it.
EOF
