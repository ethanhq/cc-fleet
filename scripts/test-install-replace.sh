#!/bin/sh
# test-install-replace.sh — regression test for re-installing over a running cc-fleet.
#
# Overwriting the binary in place (same inode) while a copy of it is running makes
# macOS SIGKILL (exit 137) every later launch of the new bytes. The installers must
# copy to a temp file in the target dir and rename it over the target instead.
#
#   sh scripts/test-install-replace.sh                # repo install.sh, then release/install.sh
#   sh scripts/test-install-replace.sh <install.sh>   # only the given top-level-style installer
#
# Temp dirs only; no network (installs from file:// archives); never runs claude.
# Exits 1 with the failed assertion on any failure.

set -eu

repo=$(cd "$(dirname "$0")/.." && pwd)
given=${1:-}
if [ -n "$given" ]; then
    given=$(cd "$(dirname "$given")" && pwd)/$(basename "$given")
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/ccf-install-replace.XXXXXX")
watch_pid=
cleanup() {
    if [ -n "$watch_pid" ]; then kill "$watch_pid" 2>/dev/null || true; fi
    rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 1' INT TERM

fail() { echo "FAIL: $*" >&2; exit 1; }
# soft records a failed assertion and keeps going, so one run reports every symptom.
bad=0
soft() { echo "FAIL: $*" >&2; bad=1; }

case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) fail "unsupported OS $(uname -s)" ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) fail "unsupported arch $(uname -m)" ;; esac
pkg="cc-fleet-${os}-${arch}"

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
    else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# Two builds that differ only in the stamped version: replacing a binary with the
# same bytes never trips the code-signature cache, so identical builds prove nothing.
for v in a b; do
    stage="$tmp/stage-$v/$pkg"
    mkdir -p "$stage" "$tmp/dist-$v"
    CGO_ENABLED=0 go -C "$repo" build -buildvcs=false \
        -ldflags "-X github.com/ethanhq/cc-fleet/internal/version.Version=v0.0.0-$v" \
        -o "$stage/cc-fleet" ./cmd/cc-fleet || fail "go build ($v)"
    cp "$repo/release/install.sh" "$stage/install.sh"
    tar -czf "$tmp/dist-$v/$pkg.tar.gz" -C "$tmp/stage-$v" "$pkg"
    echo "$(sha256_of "$tmp/dist-$v/$pkg.tar.gz")  $pkg.tar.gz" > "$tmp/dist-$v/checksums.txt"
done

inode() { ls -di "$1" | awk '{print $1}'; }

# Runs an installer with a throwaway HOME so nothing outside $tmp is written.
iso() { env -u CLAUDE_CONFIG_DIR -u XDG_CONFIG_HOME HOME="$tmp/home" "$@"; }

# install_with <kind> <installer> <a|b> <prefix>
install_with() {
    case "$1" in
        top) iso env CCF_BASE_URL="file://$tmp/dist-$3" sh "$2" --skill none --prefix "$4" --version "v0.0.0-$3" >/dev/null ;;
        release)
            rm -rf "$tmp/x-$3"; mkdir -p "$tmp/x-$3"
            tar -xzf "$tmp/dist-$3/$pkg.tar.gz" -C "$tmp/x-$3"
            iso bash "$tmp/x-$3/$pkg/install.sh" --skill none --prefix "$4" >/dev/null ;;
    esac
}

# check_version <prefix> <when>
check_version() {
    rc=0
    out=$(iso "$1/cc-fleet" --version 2>&1) || rc=$?
    [ "$rc" -eq 0 ] || soft "$label: cc-fleet --version $2 exited $rc (137 = SIGKILL): $out"
    case "$out" in
        *v0.0.0-b*) ;;
        *) soft "$label: cc-fleet --version $2 printed '$out', want v0.0.0-b" ;;
    esac
}

# run_case <label> <kind> <installer>
run_case() {
    label=$1
    prefix="$tmp/bin-$label"
    mkdir -p "$tmp/home"
    install_with "$2" "$3" a "$prefix" || fail "$label: installing A failed"

    iso "$prefix/cc-fleet" watch --timeout 60s >/dev/null 2>&1 &
    watch_pid=$!
    sleep 1
    kill -0 "$watch_pid" 2>/dev/null || fail "$label: background 'cc-fleet watch' (A) is not running"

    before=$(inode "$prefix/cc-fleet")
    install_with "$2" "$3" b "$prefix" || fail "$label: installing B failed"
    after=$(inode "$prefix/cc-fleet")
    [ "$before" != "$after" ] || soft "$label: inode unchanged ($before): the installer overwrote the binary in place"

    check_version "$prefix" "while A's watch runs"
    kill "$watch_pid" 2>/dev/null || true
    wait "$watch_pid" 2>/dev/null || true
    watch_pid=
    check_version "$prefix" "after A's watch ended"
    [ "$bad" -eq 0 ] || exit 1
    echo "ok: $label (inode $before -> $after; v0.0.0-b runs, exit 0)"
}

if [ -n "$given" ]; then
    run_case given top "$given"
else
    run_case install.sh top "$repo/install.sh"
    run_case release-install.sh release ""
fi
echo "PASS"
