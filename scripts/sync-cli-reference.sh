#!/bin/sh
# Rebuilds docs/reference/cli.md from docs/CLI.md, the one to edit. The copy
# lives one directory deeper, so every relative link gains a "../"; links that
# are absolute, external or anchors only are left as they are.
# TestTheCLIReferenceCopyMatches fails when the two drift.
set -eu
perl -pe 's{\]\((?!https?:|mailto:|#|/)([^)\s]+)\)}{](../$1)}g' docs/CLI.md > docs/reference/cli.md
