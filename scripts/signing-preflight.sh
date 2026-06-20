#!/usr/bin/env bash
# Assert the Ed25519 public key embedded in the binary (releaseSigningKeyB64 in
# internal/selfupdate/signing.go) matches the public key derived from the
# CCF_RELEASE_SIGNING_KEY signing secret. Run as a release prerequisite BEFORE
# GoReleaser signs checksums.txt: a mismatch (or an unprovisioned secret) would
# publish a checksums.txt.sig that every updated client rejects, so it fails the
# release loudly. The embedded key is replaced when the signing key is rotated.
set -euo pipefail

embedded="$(sed -n 's/.*releaseSigningKeyB64[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' internal/selfupdate/signing.go | head -1)"
if [ -z "$embedded" ]; then
  echo "signing-preflight: could not read releaseSigningKeyB64 from internal/selfupdate/signing.go" >&2
  exit 1
fi

# Derives the public key from $CCF_RELEASE_SIGNING_KEY; fails if the secret is unset/malformed.
derived="$(go run ./tools/sign-checksums -pubkey)"

if [ "$embedded" != "$derived" ]; then
  echo "signing-preflight: embedded public key does not match CCF_RELEASE_SIGNING_KEY" >&2
  echo "  embedded: $embedded" >&2
  echo "  derived:  $derived" >&2
  echo "signing-preflight: update releaseSigningKeyB64 in signing.go to the release key, or fix the secret." >&2
  exit 1
fi
echo "signing-preflight: embedded public key matches the signing secret ($embedded)"
