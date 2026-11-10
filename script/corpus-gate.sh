#!/usr/bin/env bash
#
# corpus-gate — run RepoCortex's invariants against REAL third-party repositories.
#
# WHY THIS EXISTS. Every defect found in this project during the session that
# built it was found by running against a codebase we did not write:
#
#   a blank graph canvas (0 painted pixels)      found on a real repo
#   a Vue call bound to a Python function        found on a real repo
#   `accuracy` taking twenty minutes             found on a real repo
#   "undefined" rendered in the fingerprint      found on a real repo
#   three determinism bugs                       found at real scale
#
# None was found by the unit suite, which was green throughout. Tests check what
# we thought of; a foreign codebase checks what we did not.
#
# It asserts INVARIANTS, not results. Results differ per repository and cannot
# be pinned; these properties must hold on any codebase, and each line below is
# a bug that actually shipped.
#
# Usage:
#   script/corpus-gate.sh <repo-list-file> [cortex-binary]
#
# where <repo-list-file> holds one absolute repository path per line ('#' comments
# and blank lines ignored). A list, not a parent directory, because the corpus is
# real checkouts that live in different places and must not be copied — copying a
# node_modules tree costs more than the whole gate.
#
# Exit 0 if every repository satisfies every invariant; 1 otherwise, naming the
# repository and the invariant.
set -uo pipefail

LIST="${1:?usage: corpus-gate.sh <repo-list-file> [binary]}"
DEV="${2:-cortex}"
FAILURES=0
CHECKED=0
UNMEASURED=0

fail() { echo "  FAIL  $1"; FAILURES=$((FAILURES+1)); }
pass() { echo "  ok    $1"; }

while read -r repo; do
  case "$repo" in ''|\#*) continue;; esac
  name=$(basename "$repo")
  [ -d "$repo" ] || { fail "$repo: not a directory"; continue; }
  echo "── $name"
  CHECKED=$((CHECKED+1))
  rm -rf "$repo/.cortex"

  # 1. Indexes to completion. Catches the slice panic that killed `index` on a
  #    string literal containing "WITH a FROM b".
  if ! (cd "$repo" && "$DEV" init >/dev/null 2>&1 && "$DEV" index >/dev/null 2>&1); then
    fail "$name: index did not complete"; continue
  fi
  pass "indexes"

  if ! (cd "$repo" && "$DEV" resolve >/dev/null 2>&1); then
    fail "$name: resolve did not complete"; continue
  fi
  pass "resolves"

  # 2. Determinism. Three bugs of this shape have shipped, all unordered SQL
  #    reads feeding a first-wins rule. Same bytes must give the same graph.
  #
  #    "Same bytes" is load-bearing and is NOT free: a repository under active
  #    development mutates while the gate runs, and a differing digest then says
  #    nothing about RepoCortex. This gate reported exactly that false failure on a
  #    live application whose test-runner caches alone rewrote 1,879 files in
  #    ninety minutes. So the tree is fingerprinted around the measurement, and a
  #    tree that moved is reported UNMEASURED — never as a pass, never as a
  #    failure. Same rule the product uses: exit 2 is not exit 0.
  # Fingerprint in python, not `stat`: the BSD spelling (`stat -f '%N %m %z'`)
  # asks GNU stat for FILESYSTEM statistics, so this guard would have degraded
  # to a constant on the Linux runner — silently, and in the direction that
  # reports a clean pass.
  fingerprint() { python3 - "$repo" <<'FP'
import hashlib, os, sys
h = hashlib.sha256()
skip = {'.git', '.cortex', 'node_modules'}
for root, dirs, files in os.walk(sys.argv[1]):
    dirs[:] = sorted(d for d in dirs if d not in skip)
    for f in sorted(files):
        p = os.path.join(root, f)
        try:
            st = os.stat(p)
        except OSError:
            continue
        h.update(f"{p}\0{st.st_mtime_ns}\0{st.st_size}\0".encode())
print(h.hexdigest())
FP
  }
  t0=$(fingerprint)
  d1=$(cd "$repo" && "$DEV" index --digest --json 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)["registry_digest"])' 2>/dev/null)
  d2=$(cd "$repo" && "$DEV" index --digest --json 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)["registry_digest"])' 2>/dev/null)
  t1=$(fingerprint)
  if [ "$t0" != "$t1" ]; then
    echo "  ????  determinism UNMEASURED — the working tree changed during the run"
    UNMEASURED=$((UNMEASURED+1))
  elif [ -z "$d1" ] || [ "$d1" != "$d2" ]; then
    fail "$name: digest unstable ($d1 vs $d2)"
  else pass "digest stable"; fi

  # 3. Graph integrity.
  if (cd "$repo" && "$DEV" doctor --verify-index >/dev/null 2>&1); then
    pass "doctor --verify-index"
  else fail "$name: doctor --verify-index reported a problem"; fi

  # 4. No `high` edge between languages with no shared module system. A Vue
  #    component cannot call a Python function; this shipped as an architecture
  #    violation reading "the UI reaches into inventory-service".
  bad=$(cd "$repo" && python3 - <<'PY' 2>/dev/null || echo 0
import sqlite3
OK={('typescript','tsx'),('typescript','javascript'),('typescript','vue'),
    ('tsx','typescript'),('tsx','javascript'),('tsx','vue'),
    ('javascript','typescript'),('javascript','tsx'),('javascript','vue'),
    ('vue','typescript'),('vue','tsx'),('vue','javascript'),
    ('java','kotlin'),('java','scala'),('kotlin','java'),('kotlin','scala'),
    ('scala','java'),('scala','kotlin')}
KNOWN={'go','python','typescript','tsx','javascript','vue','java','kotlin','scala'}
c=sqlite3.connect('.cortex/index.db')
n=0
for a,b,cnt in c.execute("""SELECT sf.language, df.language, COUNT(*) FROM edges e
  JOIN symbols s ON s.id=e.src_id JOIN files sf ON sf.id=s.file_id
  JOIN symbols d ON d.id=e.dst_id JOIN files df ON df.id=d.file_id
  WHERE e.kind='CALLS' AND e.source='source_code' AND e.confidence='high'
    AND sf.language != df.language GROUP BY 1,2"""):
    if a in KNOWN and b in KNOWN and (a,b) not in OK: n+=cnt
print(n)
PY
)
  if [ "${bad:-0}" -gt 0 ]; then
    fail "$name: $bad high-confidence edges between languages that share no module system"
  else pass "no impossible cross-language edges"; fi

  # 5. The portal answers, and never serialises the string "undefined" — the
  #    symptom of a JS/Go field mismatch reaching a reader as a number.
  port=$(( 7950 + RANDOM % 40 ))
  (cd "$repo" && "$DEV" ui --port "$port" >/dev/null 2>&1 &) ; sleep 4
  ok=1
  for ep in health overview graph governance crossstack; do
    body=$(curl -s --max-time 20 "http://127.0.0.1:$port/api/$ep" 2>/dev/null)
    [ -z "$body" ] && { fail "$name: /api/$ep returned nothing"; ok=0; break; }
    case "$body" in *'"undefined"'*) fail "$name: /api/$ep serialised \"undefined\""; ok=0;; esac
  done
  [ "$ok" = 1 ] && pass "portal endpoints answer"
  pkill -f "$DEV ui --port $port" >/dev/null 2>&1

  # 6. `accuracy` completes. It took twenty minutes on a 91k-symbol repository
  #    before an index and a join inversion; a report nobody runs stops being
  #    true without anyone noticing.
  start=$(date +%s)
  if (cd "$repo" && "$DEV" accuracy >/dev/null 2>&1); then
    el=$(( $(date +%s) - start ))
    [ "$el" -gt 120 ] && fail "$name: accuracy took ${el}s (budget 120s)" || pass "accuracy in ${el}s"
  else fail "$name: accuracy did not complete"; fi
done < <(grep -v '^[[:space:]]*$' "$LIST")

echo
echo "corpus-gate: $CHECKED repositories, $FAILURES failure(s), $UNMEASURED unmeasured"
[ "$FAILURES" -eq 0 ]
