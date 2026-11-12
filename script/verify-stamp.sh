#!/usr/bin/env bash
# Proves -ldflags actually reached the Version symbol.
#
# A wrong -X symbol path is SILENT: the build succeeds and ships "dev". This
# script is the only thing standing between a typo and a release that lies
# about its own version. It is a script, not an inline CI step, so a developer
# can run the same check locally before tagging.
set -euo pipefail

# This script's entire purpose is to be runnable by a developer BEFORE
# tagging, from wherever their shell happens to be sitting - so it must not
# depend on cwd. `go build ./cmd/repocortex` only resolves relative to the
# module root; run this from any other directory and it fails with "go.mod
# file not found in current directory or any parent directory". Resolve the
# repo root first and cd into it. Fall back to the script's own location if
# git is unavailable (e.g. a extracted tarball with no .git).
cd "$(git rev-parse --show-toplevel 2>/dev/null || dirname "$(dirname "$0")")"

want="${1:?usage: verify-stamp.sh <version>}"
pkg="github.com/satyendrakumarsingh/repocortex/internal/cli"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
out="$tmpdir/repocortex"

go build -trimpath \
  -ldflags "-X ${pkg}.Version=${want} -X ${pkg}.Commit=test -X ${pkg}.BuildTime=test" \
  -o "$out" ./cmd/repocortex

got="$("$out" version)"
echo "$got"
case "$got" in
  *"$want"*) echo "stamp OK" ;;
  *) echo "::error::-ldflags did not stamp the version: wanted ${want}, got: ${got}"; exit 1 ;;
esac
