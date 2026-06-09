#!/usr/bin/env bash
# Assert the plugin manifest and npm package versions match the release tag, so
# a release can never ship a stale .claude-plugin/plugin.json (the marketplace
# serves the committed manifest) or a mismatched npm package. Run as a release
# prerequisite BEFORE any artifact is built; a mismatch fails the release loudly.
# We never auto-bump on a tag — fix it with `make release-prep VERSION=vX.Y.Z`,
# commit, then re-tag.
set -euo pipefail

tag="${1:-${GITHUB_REF_NAME:-}}"
tag="${tag#v}" # strip a leading v
if [ -z "$tag" ]; then
  echo "version-guard: no tag given (pass vX.Y.Z or set GITHUB_REF_NAME)" >&2
  exit 2
fi

jsonver() { sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$1" | head -1; }

plugin_ver="$(jsonver .claude-plugin/plugin.json)"
npm_ver="$(jsonver npm/package.json)"

fail=0
if [ "$plugin_ver" != "$tag" ]; then
  echo "version-guard: .claude-plugin/plugin.json version=$plugin_ver != tag=$tag" >&2
  fail=1
fi
if [ "$npm_ver" != "$tag" ]; then
  echo "version-guard: npm/package.json version=$npm_ver != tag=$tag" >&2
  fail=1
fi
if [ "$fail" -ne 0 ]; then
  echo "version-guard: run 'make release-prep VERSION=v$tag', commit, then re-tag." >&2
  exit 1
fi
echo "version-guard: plugin.json, npm/package.json, and tag all at $tag"
