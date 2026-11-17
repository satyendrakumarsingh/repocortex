#!/usr/bin/env bash
#
# update-manifests.sh — point the package manifests at a release.
#
# The manifests under packaging/ and the registry entry server.json all name a
# release by its version, its asset URLs and the SHA-256 of those assets. This
# script rewrites exactly those fields, from the checksum files the release
# workflow publishes, so nothing is typed by hand:
#
#   packaging/scoop/repocortex.json           version, url, hash
#   packaging/winget/*.yaml                   PackageVersion, InstallerUrl,
#                                             InstallerSha256, ReleaseDate,
#                                             ReleaseNotesUrl
#   packaging/mcpb/manifest.json              version
#   packaging/homebrew/repocortex.rb          the whole formula, rendered by
#                                             render-homebrew-formula.sh
#   server.json                               version, image tag, bundle URLs
#                                             and fileSha256
#
# Usage:
#   scripts/release/update-manifests.sh <version> [checksum-file ...]
#
#   <version>        the release tag, with or without the leading "v"
#   checksum-file    one or more files in `sha256sum` format. With none given,
#                    SHA256SUMS and MCPB_SHA256SUMS are downloaded from the
#                    GitHub release for <version>.
#
# Environment:
#   RELEASE_DATE     the winget ReleaseDate, YYYY-MM-DD (default: today, UTC)
#   REPO_ROOT        the tree to edit (default: this repository)
#
# The Windows archive must be in the checksums; the script stops if it is not.
# The MCP bundles are optional: a release made before the bundle job existed
# has none, and server.json then keeps its all-zero placeholder hashes. The
# script says so, and a server.json in that state must not be published.

# The jq filters below are single-quoted on purpose: $v, $h and the rest are
# jq variables, not shell ones.
# shellcheck disable=SC2016
set -euo pipefail

REPO="satyendrakumarsingh/repocortex"
ZERO_HASH="0000000000000000000000000000000000000000000000000000000000000000"

usage() { sed -n '2,/^$/p' "$0" | sed -e 's/^# \{0,1\}//'; }

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
  "") usage; exit 2 ;;
esac

for tool in jq sed awk; do
  command -v "$tool" >/dev/null 2>&1 || { echo "error: $tool is required" >&2; exit 1; }
done

ver="${1#v}"
shift
case "$ver" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *)
    resolved="$(git tag --points-at HEAD "v*.*.*" 2>/dev/null | head -n1 || git describe --tags --match="v[0-9]*.[0-9]*.[0-9]*" 2>/dev/null || echo "")"
    if [[ "${resolved#v}" =~ ^[0-9]+\.[0-9]+\.[0-9]+ ]]; then
      ver="${resolved#v}"
    else
      echo "error: '$ver' is not a release version (want e.g. v1.2.3)" >&2
      exit 2
    fi
    ;;
esac
tag="v${ver}"

root="${REPO_ROOT:-$(cd "$(dirname "$0")/../.." && pwd)}"
base="https://github.com/${REPO}/releases/download/${tag}"
date="${RELEASE_DATE:-$(date -u +%Y-%m-%d)}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
sums="$tmp/sums"
: > "$sums"

if [ "$#" -eq 0 ]; then
  command -v curl >/dev/null 2>&1 || { echo "error: curl is required to fetch checksums" >&2; exit 1; }
  echo "fetching ${base}/SHA256SUMS"
  curl -fsSL "${base}/SHA256SUMS" >> "$sums"
  if curl -fsSL "${base}/MCPB_SHA256SUMS" -o "$tmp/mcpb" 2>/dev/null; then
    echo "fetching ${base}/MCPB_SHA256SUMS"
    cat "$tmp/mcpb" >> "$sums"
  else
    echo "note: ${tag} publishes no MCPB_SHA256SUMS"
  fi
else
  for f in "$@"; do
    [ -f "$f" ] || { echo "error: no such checksum file: $f" >&2; exit 1; }
    cat "$f" >> "$sums"
  done
fi

# hash_of <asset name>: the SHA-256 recorded for that exact file name, or
# nothing. `sha256sum` writes "<hash>  <name>", and "<hash> *<name>" in binary
# mode; both are accepted.
hash_of() {
  awk -v want="$1" '{ name = $2; sub(/^\*/, "", name); if (name == want) { print tolower($1); exit } }' "$sums"
}

is_sha256() { printf '%s' "$1" | grep -Eq '^[0-9a-f]{64}$'; }

# rewrite <file> <jq filter> [jq args...]: applies the filter in place, keeping
# the file's indentation width.
rewrite() {
  local file="$1" filter="$2" indent
  shift 2
  indent="$(awk 'match($0, /^ +/) { print RLENGTH; exit }' "$file")"
  jq --indent "${indent:-2}" "$@" "$filter" "$file" > "$tmp/out"
  cat "$tmp/out" > "$file"
}

# set_yaml <file> <key> <value>: replaces the value of a top-level or
# list-nested scalar key, keeping its indentation. Fails if the key is absent,
# so a renamed field cannot be skipped silently.
set_yaml() {
  local file="$1" key="$2" value="$3"
  grep -Eq "^[ -]*${key}: " "$file" || { echo "error: ${file} has no '${key}:' line" >&2; exit 1; }
  sed -E "s|^([ -]*${key}: ).*|\\1${value}|" "$file" > "$tmp/out"
  cat "$tmp/out" > "$file"
}

win_zip="repocortex_${tag}_windows_amd64.zip"
win_hash="$(hash_of "$win_zip")"
is_sha256 "$win_hash" || { echo "error: ${win_zip} is absent from the checksums" >&2; exit 1; }
win_url="${base}/${win_zip}"

# --- Scoop -------------------------------------------------------------------
scoop="$root/packaging/scoop/repocortex.json"
rewrite "$scoop" \
  '.version = $v | .architecture."64bit".url = $u | .architecture."64bit".hash = $h' \
  --arg v "$ver" --arg u "$win_url" --arg h "$win_hash"
echo "updated ${scoop#"$root"/}"

# --- winget ------------------------------------------------------------------
winget="$root/packaging/winget"
id="SatyendraKumarSingh.RepoCortex"
for f in "$winget/$id.yaml" "$winget/$id.installer.yaml" "$winget/$id.locale.en-US.yaml"; do
  set_yaml "$f" PackageVersion "$ver"
done
set_yaml "$winget/$id.installer.yaml" InstallerUrl "$win_url"
set_yaml "$winget/$id.installer.yaml" InstallerSha256 "$(printf '%s' "$win_hash" | tr '[:lower:]' '[:upper:]')"
set_yaml "$winget/$id.installer.yaml" ReleaseDate "$date"
set_yaml "$winget/$id.locale.en-US.yaml" ReleaseNotesUrl "https://github.com/${REPO}/releases/tag/${tag}"
echo "updated ${winget#"$root"/}/$id.*.yaml"

# --- Homebrew ----------------------------------------------------------------
# The release job copies this file into satyendrakumarsingh/homebrew-tap.
brew="$root/packaging/homebrew/repocortex.rb"
"$(dirname "$0")/render-homebrew-formula.sh" "$tag" "$sums" > "$tmp/formula"
cat "$tmp/formula" > "$brew"
echo "updated ${brew#"$root"/}"

# --- MCP bundle manifest -----------------------------------------------------
mcpb="$root/packaging/mcpb/manifest.json"
rewrite "$mcpb" '.version = $v' --arg v "$ver"
echo "updated ${mcpb#"$root"/}"

# --- MCP registry entry ------------------------------------------------------
# Each bundle entry is found by the platform suffix of its file name, so the
# version inside the URL can change underneath it.
server="$root/server.json"
rewrite "$server" \
  '.version = $v
   | .packages |= map(
       if .registryType == "oci" then
         .identifier |= sub(":[^:/]+$"; ":" + $v)
       elif .registryType == "mcpb" then
         (.identifier | capture("_(?<suffix>[a-z0-9]+_[a-z0-9]+)\\.mcpb$").suffix) as $s
         | .identifier = $base + "/repocortex_" + $tag + "_" + $s + ".mcpb"
         | .fileSha256 = $zero
       else . end)' \
  --arg v "$ver" --arg tag "$tag" --arg base "$base" --arg zero "$ZERO_HASH"

missing=""
for name in $(jq -r '.packages[] | select(.registryType == "mcpb") | .identifier | sub(".*/"; "")' "$server"); do
  h="$(hash_of "$name")"
  if is_sha256 "$h"; then
    rewrite "$server" \
      '(.packages[] | select(.registryType == "mcpb" and (.identifier | endswith("/" + $n))) | .fileSha256) = $h' \
      --arg n "$name" --arg h "$h"
  else
    missing="${missing} ${name}"
  fi
done
echo "updated ${server#"$root"/}"

if [ -n "$missing" ]; then
  echo ""
  echo "WARNING: no checksum for:${missing}"
  echo "         server.json keeps a placeholder fileSha256 for each of them."
  echo "         Do not publish it to the MCP registry in this state."
fi
