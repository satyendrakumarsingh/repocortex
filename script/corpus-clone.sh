#!/usr/bin/env bash
#
# corpus-clone — fetch the corpus and write a repo list for corpus-gate.sh.
#
# These are THE corpus, not stand-ins: the eight projects actually read during
# the market teardown, at their real upstream URLs. An earlier version of this
# script cloned a convenience set of well-known repositories instead, which
# meant CI exercised the invariants against code nobody had studied while the
# measured result quoted elsewhere came from different code. A gate that runs
# something other than what its report describes is worse than no gate.
#
# The tenth member (a live private application) cannot be cloned by public CI.
# Pass its path in CORPUS_EXTRA to include it locally; CI runs the nine it can
# reach and says so, rather than quietly reporting ten.
set -euo pipefail

DEST="${1:?usage: corpus-clone.sh <dest-dir> <list-file>}"
LIST="${2:?usage: corpus-clone.sh <dest-dir> <list-file>}"
mkdir -p "$DEST"
: > "$LIST"

# Tracked at HEAD, not pinned. The commit each was read at is recorded in
# docs/internal/research/; the gate's job is the opposite of reproducing that snapshot —
# a corpus frozen at one commit stops finding new shapes. The SHA is printed on
# every run so drift is visible rather than silent.
REPOS="
https://github.com/Egonex-AI/Understand-Anything
https://github.com/PatrickSys/codebase-context
https://github.com/colbymchenry/codegraph
https://github.com/abdulmunimjemal/codescope-mcp
https://github.com/jghiringhelli/codeseeker
https://github.com/MikeRecognex/mcp-codebase-index
https://github.com/James-Chahwan/repo-graph
https://github.com/luuuc/sense
"

want=0; got=0
for url in $REPOS; do
  want=$((want+1))
  name=$(basename "$url")
  if [ ! -d "$DEST/$name" ]; then
    git clone --depth 1 --quiet "$url" "$DEST/$name" 2>/dev/null || {
      echo "warn: could not clone $url — EXCLUDED from this run" >&2; continue; }
  fi
  got=$((got+1))
  echo "$DEST/$name" >> "$LIST"
  printf '  %-24s %s\n' "$name" "$(git -C "$DEST/$name" rev-parse --short HEAD)"
done

echo "$PWD" >> "$LIST"   # RepoCortex's own checkout — a real corpus member
printf '  %-24s %s\n' "$(basename "$PWD")" "$(git rev-parse --short HEAD 2>/dev/null || echo working-tree)"

if [ -n "${CORPUS_EXTRA:-}" ] && [ -d "$CORPUS_EXTRA" ]; then
  echo "$CORPUS_EXTRA" >> "$LIST"
  printf '  %-24s %s\n' "$(basename "$CORPUS_EXTRA")" "local"
fi

echo "corpus-clone: $got/$want upstream repositories + $(( $(wc -l < "$LIST") - got )) local"

# A corpus that silently shrinks reports a pass it did not earn. Losing one
# repository to an upstream outage is tolerable; losing several means the run
# covered something much smaller than its name claims.
if [ "$got" -lt $((want - 1)) ]; then
  echo "corpus-clone: FAILED — only $got of $want upstream repositories available" >&2
  exit 1
fi
