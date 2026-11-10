#!/usr/bin/env bash
#
# corpus-local — run the gate over the FULL ten-member corpus, including the
# private application that public CI cannot reach.
#
# This exists because the checked-in path list it replaces did not survive its
# own session: eight of its ten entries pointed into a temp directory that is
# deleted between runs, so the committed corpus definition was already rotting
# the day it landed. Membership is now defined by upstream URL (which is stable)
# plus two local checkouts resolved at run time.
#
#   script/corpus-local.sh [cortex-binary]
#
# Env:
#   CORPUS_HOME   where the upstream clones live   (default ~/.cortex-corpus)
#   CORPUS_EXTRA  an optional local repository to add as a tenth member (default: none)
set -euo pipefail

cd "$(dirname "$0")/.."
DEV="${1:-}"
if [ -z "$DEV" ]; then
  DEV=$(mktemp -d)/cortex
  echo "building cortex…"
  go build -o "$DEV" ./cmd/cortex
fi

# A local repository to add to the corpus, if any; nothing is assumed about this machine.
export CORPUS_EXTRA="${CORPUS_EXTRA:-}"
HOME_DIR="${CORPUS_HOME:-$HOME/.cortex-corpus}"
LIST=$(mktemp)

if [ -z "$CORPUS_EXTRA" ] || [ ! -d "$CORPUS_EXTRA" ]; then
  # Named, not silently dropped. A nine-member run reported as the corpus is a
  # coverage claim the run did not earn.
  echo "note: no CORPUS_EXTRA repository${CORPUS_EXTRA:+ at $CORPUS_EXTRA} — running the nine public members" >&2
  unset CORPUS_EXTRA
fi

script/corpus-clone.sh "$HOME_DIR" "$LIST"
echo
exec script/corpus-gate.sh "$LIST" "$DEV"
