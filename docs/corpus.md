# The corpus gate

## Why it exists

Every defect this project found in the week it was hardened was found by running
against a codebase we did not write:

| Defect | Found by |
|---|---|
| `index` panicked on a string literal containing `WITH a FROM b` | a real repo |
| a Vue component bound to a Python function at `high` confidence | a real repo |
| `accuracy` took twenty minutes on a 91k-symbol tree | a real repo |
| `"undefined"` rendered into the repo fingerprint | a real repo |
| three unordered-read determinism bugs | a real repo, at scale |
| the graph canvas painted zero pixels | a real repo |

None was found by the unit suite, which was green throughout — and green
*while* each of those defects was live. That is not a criticism of the suite.
It is what a suite is: a record of what we already thought of. A foreign
codebase tests what we did not.

`fixtures/corpus/` does not close this gap, because we wrote it. A fixture
cannot surprise its author.

## The corpus

Ten codebases: eight competitor projects read during the market teardown, one
live polyglot application, and RepoCortex itself. Five languages, written by ten
teams, none of whom knew RepoCortex existed.

Membership is defined by **upstream URL** in
[`script/corpus-clone.sh`](../script/corpus-clone.sh), not by a list of local
paths. The list of paths this replaced did not survive its own session: eight of
its ten entries pointed into a temp directory that is deleted between runs, so
the committed definition of the corpus was rotting the day it landed.

The clones track upstream **HEAD** rather than a pinned commit. A corpus frozen
at one snapshot stops finding new shapes — `sense` had already moved from
`4343638` to `7f73fdc` between being read and being gated. Every SHA is printed,
so drift is visible rather than silent, and a nightly failure caused by upstream
change looks the same as one caused by us until you read those SHAs.

## What it asserts

Invariants, not results. Results differ per repository and cannot be pinned —
`sense` has a different edge count than `pulse` and both are correct. These six
properties must hold on **any** codebase, and each is a bug that shipped:

1. **Indexes and resolves to completion.** No panic, no partial run.
2. **The registry digest is stable across repeated runs.** Same source bytes,
   same graph. Three shipped bugs had this shape — an unordered `SELECT`
   feeding a first-wins rule.

   *"Same source bytes" is load-bearing and is not free.* A repository under
   active development mutates while the gate runs, and a differing digest then
   says nothing about RepoCortex. The first real run of this gate reported exactly
   that false failure: a live application whose `.pytest_cache`, `.ruff_cache`
   and Playwright report alone rewrote 1,879 files in ninety minutes. So the
   gate fingerprints the tree around the measurement, and a tree that moved is
   reported **UNMEASURED** — printed distinctly, counted separately, and neither
   a pass nor a failure. Calling it a pass would be as wrong as calling it a
   failure. This is the same rule the product uses on itself, where unverified
   exits 2 rather than 0.
3. **`doctor --verify-index` is clean.** No dangling edge, no unproven `exact`.
4. **No `high` edge between languages that share no module system.** A Vue file
   cannot call a Python function. This one shipped as a false architecture
   violation on a user's repository.
5. **No portal endpoint serialises the string `"undefined"`.** The signature of
   a JS reader and a Go field name that disagree.
6. **`accuracy` completes inside 120s.** A report nobody runs stops being true
   without anyone noticing.

## Running it

The full ten, including the private member:

```
script/corpus-local.sh
```

CI runs the nine it can reach — the private application cannot be cloned by a
public runner, so CI reports nine rather than implying ten. Point
`CORPUS_EXTRA` at that checkout to include it locally; it is named when absent,
never dropped in silence.

Determinism is the one invariant that repository will usually not satisfy, and
that is a fact about the repository rather than a defect: it is under active
development, and its test-runner caches alone rewrite thousands of files an
hour. Expect `UNMEASURED` there.

Against an explicit list of paths:

```
script/corpus-gate.sh <repo-list-file> [repocortex-binary]
```

Exit 0 if every repository satisfies every invariant; 1 otherwise, naming the
repository and the invariant that broke. Unmeasured invariants are reported in
their own tally and do not fail the gate — they are also not evidence of health.

## Adding to it

Add a path to `script/corpus.repos`. The bar for admission is that we did not
write it. A repository that fails on the day it is added is the harness working:
record the failure, fix the defect, keep the repository.
