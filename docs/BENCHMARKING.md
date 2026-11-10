# Benchmarking

How to reproduce every number this project publishes, and what each one does and
does not mean.

This page is operational. The **method** for resolution accuracy — why judgement
is restricted to unambiguous lines, why recall is not reported, where the
remaining errors come from — is in [accuracy.md](accuracy.md), and this extends
that document rather than restating it. The performance numbers and the
measurements that chose each default are in [performance.md](performance.md).

The reason both exist: until they did, every confidence label in this tool was a
design intention rather than a measurement. If you disagree with a number, the
commands below are how you produce a competing one.

---

## What is measurable, and with what

| Claim | Command | Answers |
|---|---|---|
| Confidence tiers mean something | `doctor --resolution-precision` | precision per tier, against compiler ground truth |
| Retrieval returns the right code | `bench <cases.yml>` | recall and precision against hand-written cases |
| Indexing is reproducible | `index --full --digest` | one sha256 over a run's content |
| The graph is internally sound | `doctor --verify-index` | dangling edges, orphans, unproven `exact` claims |
| Operations cost what we say | `index`, `resolve`, query commands | wall clock, printed by the tool itself |

All five run against your repository, not ours. That is the point of shipping
them as commands rather than as a table.

## Prerequisites

```bash
CGO_ENABLED=0 go build -o /tmp/cortex ./cmd/cortex   # Go 1.25+
```

Zero CGO is not incidental: it is what makes `go install` work without a C
toolchain, and it means the binary you benchmark is the binary that ships.

Ground-truth indexers, pinned to the versions every published figure was taken
against:

| Tool | Version |
|---|---|
| `scip-go` | 0.2.7 |
| `scip-typescript` | 0.3.14 |
| `scip-python` | 0.6.6 |

`cortex indexers install --yes` provisions these at pinned versions with
verified digests. `cortex indexers list` tells you what applies to a
repository and what is missing, without installing anything.

## Corpora

Two documents use different corpora, deliberately, and neither set is
interchangeable with the other.

**[accuracy.md](accuracy.md) — real third-party code.** `--depth 1` clones of
prometheus (Go), nest (TypeScript), scrapy and requests (Python), plus
RepoCortex's own source. Third-party code the resolver was never tuned against is
the only kind that can falsify a resolution claim; RepoCortex's own numbers are
the least trustworthy in the table for exactly that reason, and they are the
highest.

**[performance.md](performance.md) — three corpora, each measuring a different
thing.** `fixtures/payment-platform` (9 files) measures fixed costs — process
start, migrations, transaction setup — rather than throughput. RepoCortex's own
source measures a real mid-size repository. A 5,000-file synthetic Go corpus
(`go5k`, ~20 MB, 210,000 symbols) measures scaling, and is byte-for-byte
reproducible because each file's content is a pure function of its index.

**Do not measure accuracy on the synthetic corpus.** Its identifiers repeat
across packages by construction, so `resolve` reports 0 resolved and 80,000
ambiguous. It measures the throughput of the binding path and says nothing about
binding correctness. For the same reason a synthetic *Java* corpus was
deliberately not used to measure Java: uniform generated code makes resolution
easy and would report an inflated precision, which is worse than no number.

---

## 1. Resolution precision

The measurement that turns the confidence labels from an intention into a
number. Four commands:

```bash
cortex index && cortex resolve
scip-go --output index.scip          # compiler ground truth
cortex scip import index.scip      # lands as REFERENCES/exact/compiler
cortex doctor --resolution-precision
```

Order matters: index, resolve, then import. A heuristic `CALLS` edge is
adjudicated by asking the compiler what the reference at that source position
resolves to; agreement means both name the same definition.

Reproduced on this repository's own working tree — 476 files discovered, 422
indexed, 6,259 symbols, against a 12.7 MB `scip-go` 0.2.7 index:

```
  TIER    VERDICT  JUDGED  AGREE  DISAGREE  PRECISION
  high    act on   5,190   5,086  104       98.0%
  medium  show     49      17     32        34.7%
  low     ignore   263     0      263       0.0%
  all     —        5,502   5,103  399       92.7%

COVERAGE
  Judged 5,502 of 11,703 heuristic CALLS edge(s) (47.0%); 6,201 were not adjudicated
  at all and are neither right nor wrong above.
  Against 25,736 compiler fact(s), on the 14,280 line(s) the compiler resolved to
  exactly one target; 4,844 ambiguous line(s) were excluded.

LANGUAGES
  LANGUAGE    FILES  CALLS EDGES  COMPILER FACTS  STATUS
  go          399    11,683       25,736          measured
  java        8      18           0               NOT MEASURED — no SCIP index imported for it
  typescript  1      2            0               NOT MEASURED — no SCIP index imported for it
```

[accuracy.md](accuracy.md) records **97.8% / 34.1% / 0.0%** over 4,619 judged
edges for RepoCortex's own source. The run above is the same measurement on a
larger, later tree, and it agrees to within a fraction of a point. **The two
numbers are not comparable as a before/after** — different file counts, different
`scip-go` output, different resolver. That is the single most important
discipline on this page, and it has its own section below.

### What this number is

- **It is precision.** SCIP records that one symbol *references* another, not
  that it *calls* it, so the compiler's edge set is a superset of the resolver's
  by construction. Dividing by it would penalise the resolver for doing exactly
  what it was designed to do. Recall for calls needs a ground truth that
  distinguishes call sites from type references, which `scip-go` does not emit.
- **It is not a claim about proven edges.** Compiler-backed edges are not scored
  against themselves. They are `exact` because a typechecker produced them, and
  `doctor --verify-index` separately enforces that nothing may claim `exact`
  without `source=compiler`.
- **It covers less than the whole graph, and says how much.** 47.0% of heuristic
  `CALLS` edges were adjudicated in the run above. The rest are neither right
  nor wrong in that table.
- **It names what it could not measure.** A language with heuristic edges and no
  imported SCIP index is listed as `NOT MEASURED` rather than omitted. Unmeasured
  is not the same as accurate.

### Why coverage is not higher

Judgement is restricted to lines the compiler resolved to **exactly one target**,
and that restriction is load-bearing because neither looser option is sound.
Matching by line alone is too generous — on RepoCortex's own source, 25.8% of
referenced lines carry more than one distinct target, so a heuristic edge could
be scored against a reference it was never about. Matching by line *and* column
is too strict — the two tools do not share a column convention (for
`svc.Method()` the resolver records the column of `svc` and `scip-go` records
the column of `Method`), and requiring exact columns once discarded two thirds
of the comparison without either tool being wrong.

Everything undecidable is reported as uncovered rather than scored. A
measurement that quietly counts cases it cannot decide is worse than a smaller
one that says so.

### Gating on it in CI

`doctor`'s exit status is deliberately **not** affected by the measurement. Add
`--json` and the report lands under `resolution_precision` alongside the checks,
for you to threshold yourself.

---

## 2. Retrieval quality

```bash
cortex bench fixtures/bench/payment.yml
cortex bench fixtures/bench/payment.yml --fail-on-regression   # exit 1 on any failure
```

Reproduced against a copy of the bundled payment fixture:

```
CASE                              RESULT  RECALL  TOKENS  MISSING
change-transaction-status-column  pass    100%    1567    —
idempotent-callback               pass    100%    1368    —
notification-failure-handling     pass    100%    1388    —

3/3 passed, mean recall 100%.
```

A case names a task, the code a correct answer must contain (`expect`), and the
code it must not (`forbid`):

```yaml
- id: idempotent-callback
  task: Add idempotency to payment callback processing
  expect: [CallbackController, TransactionService, TransactionRepository, transactions]
  forbid: [MarketingReport]
  max_tokens: 8000
```

**The `forbid` list is what makes the score mean anything.** Recall alone is
trivially gamed by returning the whole repository, and a forbidden item
retrieved is reported as a precision failure rather than averaged into the mean.

**What it does not measure:** whether an agent completed the task. Only whether
the context contained what a human said it should, and whether it stayed inside
its budget. Those are different claims, and conflating them would make a
retrieval regression indistinguishable from a bad day at the model provider.

To benchmark your own repository, write your own cases. The bundled file is a
worked example, not a standard.

---

## 3. Determinism

```bash
cortex index --full --digest
```

The digest is a sha256 over one run's content. The property is verified **both
ways**: stable across worker counts, and *sensitive* to a single added symbol. A
digest that never changed would pass the stability half while detecting nothing.

Reproduced on this repository's working tree, three full rebuilds from an empty
database:

```
Digest: 74f82dd64551014cbaad1be87228d2ac720c7ac0ca49ccfd856eb0bfe4a5d54b  13493ms  (workers=1)
Digest: 74f82dd64551014cbaad1be87228d2ac720c7ac0ca49ccfd856eb0bfe4a5d54b   7346ms  (workers=4)
Digest: 74f82dd64551014cbaad1be87228d2ac720c7ac0ca49ccfd856eb0bfe4a5d54b   5731ms  (workers=8)
```

Three rules make this possible, and `internal/diffgraph` inherits all three:
rows are read under an explicit total `ORDER BY` over content columns rather
than by rowid; autoincrement ids are excluded and edges reference their
endpoints by `(file path, fqn, start line, start col)`; and no timestamps are
covered, only content hash and size.

This is not a formality. Running the precision measurement above is what
surfaced a determinism defect nothing else could: three distinct digests across
six repeats over byte-identical source, caused by a "first declaration wins"
rule reading from a `SELECT` with **no `ORDER BY`** — and an unordered `SELECT`
has no first. [accuracy.md](accuracy.md) has the full account. The transferable
rule is that `ORDER BY` is a correctness clause whenever a query feeds a
first-wins rule, and that a digest checked only in CI against small fixtures
will not catch an ordering bug that needs a real name collision to appear.

Also worth running alongside:

```bash
cortex doctor --verify-index      # graph invariants
go test -race ./...                 # full suite
```

---

## 4. Performance

Every figure in [performance.md](performance.md) names the corpus it came from
and prints three trials rather than an average, because the spread between
trials on a shared developer laptop is the honest picture.

The method, reduced:

```bash
CGO_ENABLED=0 go build -o /tmp/di-docs ./cmd/cortex
cp -R <corpus> /tmp/work/repo && cd /tmp/work/repo && git init && git commit -am init
for trial in 1 2 3; do
  rm -rf .cortex && /tmp/di-docs init
  /tmp/di-docs index --full          # full index into an empty database
  /tmp/di-docs index                 # re-index, nothing changed
  echo '// touched' >> <first file>
  /tmp/di-docs index                 # re-index, one file changed
  /tmp/di-docs resolve
done
```

Durations are the ones RepoCortex prints itself, so they exclude process start.
Database sizes are `index.db` on disk. Peak memory is
`/usr/bin/time -l … | grep 'maximum resident set size'`.

Reference points for "is my run in the right range", from performance.md's three
real repositories on `darwin/arm64` native:

| Repository | Files indexed | Symbols | Index time | Peak RSS |
|---|---|---|---|---|
| prometheus (Go) | 1,030 | 21,429 | 8.8 s | 664 MB |
| nest (TypeScript) | 1,929 | 13,916 | 3.7 s | 590 MB |
| spring-boot (Java) | 9,236 | 94,304 | 46.4 s | 756 MB |

Reproduced here on this repository's working tree: 422 files indexed, 6,259
symbols, full index 10.0 s, `resolve` 1.3 s, `scip import` of a 12.7 MB index
1.8 s.

Two things a real repository showed that a synthetic one could not, both
recorded in performance.md: `DefaultDrainThreshold` at 1 GiB meant the pool
never drained on a 1,262-file Go repository and the threshold *was* the peak;
and `MaxInFlightBytes` does not bound memory despite having said so — it bounds
admission, while the drain threshold bounds memory.

---

## Pitfalls

### Pitfall 1: check your `GOARCH` before you benchmark anything

Every performance figure published before one particular revision of
performance.md was silently emulated. The development machine ran an amd64 Go
toolchain under Rosetta 2 on Apple Silicon for the entire life of the
repository, and Go's benchmark headers print `goarch: amd64` and
`cpu: VirtualApple @ 2.50GHz` — the emulated view, not the silicon.

Measured cost of the emulation on RepoCortex's own source:

| Operation | amd64 under Rosetta | arm64 native | Native is |
|---|---|---|---|
| Full index | 1,801 ms | 1,264 ms | 1.43x faster |
| Re-index, nothing changed | 124 ms | 67 ms | 1.84x faster |
| Re-index, one file changed | 206 ms | 129 ms | 1.60x faster |

Query-side commands gain more, because they are dominated by process start:
startup went 64.4 ms → 20.4 ms (3.2x) on a 135,000-symbol corpus.

```bash
go env GOARCH ; uname -m      # if these disagree, your numbers are a ceiling
```

Treat the published figures as roughly 1.4–3x above what a native build
delivers. Released binaries are unaffected — CI cross-compiles `darwin/arm64`.

### Pitfall 2: re-baseline on the same tree before comparing anything

This is the pitfall that most often produces a wrong conclusion, and
accuracy.md's own "Changes since the table above was measured" section exists
because of it.

A `--depth 1` prometheus clone of 727 Go files scores **86.7%** on an unmodified
tree against the documented **86.8%** for a 1,030-file checkout. That difference
is the corpus, not a regression. Every figure in that section is therefore paired
with a baseline re-measured on the same tree and the same `scip-go` index, rather
than compared against the published number — because **a −2.4pp regression
measured against the wrong baseline reads as −0.1pp of rounding.**

The same applies to the run reproduced at the top of this page: 98.0% here and
97.8% in accuracy.md are two different trees, not a change.

### Pitfall 3: `--full` discards resolved edges

`cortex index --full` rebuilds from scratch and drops the call graph. Re-run
`cortex resolve` afterwards, or every relationship query will report that no
references have been resolved yet. A plain incremental `cortex index` does not
have this effect.

Benchmark scripts that loop over `index --full` and then query without resolving
are measuring an empty graph.

### Pitfall 4: one index database holds one committed run

`registry.CommitRun` publishes a run and deletes the rows of the one it replaces.
So `cortex diff` across two commits normally needs **two index files** — the
working index and a second index built at the other ref — which is what
`--from-index` and `--to-index` are for:

```bash
cortex diff --from-index ../release-1.4/.cortex/index.db --from 8f3c1ab --to 42
```

A ref that matches no indexed run fails and says so; it never falls back to
whichever run happens to be committed, because a diff against the wrong run is
worse than no diff at all.

### Pitfall 5: a filter that removes candidates is not automatically safe

Recorded here because it will recur, and because it was learned by measurement
rather than review. Two resolver changes adopted from a competitor both looked
plainly safe and neither survived measurement:

- "A bare call cannot name a method in Go or Rust" — true of the languages,
  false of the parser, which leaves `Receiver` empty when its query could not
  express the receiver. Every chained Go call was read as bare, and
  `cortex callers Save` went from two bound callers to zero. **Every unit test
  passed throughout**, because they set `Receiver` directly; only the end-to-end
  CLI path exercised the parser.
- "Production code never binds to a test definition" — cost **2.4pp** of `high`
  precision. Shrinking the candidate set tipped references across the
  disambiguation thresholds, so 426 previously-ambiguous references resolved: 64
  right, **362 wrong**. It converted honest ambiguity into confident wrong
  answers.

The rule the resolver now states: removing candidates also removes the ambiguity
that was protecting the caller. "Fewer candidates" and "fewer wrong edges" are
different claims, and only the second is worth having.

### Pitfall 6: benchmark with a stamped, matching build

Every index run records the build that wrote it, and a query from a different
build warns on stderr before answering. If you are comparing two measurements
and one of them printed this, the comparison is between two sets of resolution
rules:

```
level=WARN msg="This index was written by cortex dev and you are running 9.9.9-test.
Resolution rules change between builds, so these results may come from different rules
than this binary would produce. Re-index to refresh."
```

Re-index rather than reasoning past it. `-q` silences the warning and does not
fix the cause.

---

## Reporting a number that disagrees

Include: the corpus and how it was obtained (`--depth 1` clone at a named
commit), `go env GOARCH` and `uname -m`, the `scip-*` version, the RepoCortex
build, and the coverage line from `doctor --resolution-precision` — not only the
precision line. A precision figure without its coverage cannot be compared to
anything.

If a claim in these documents cannot be reproduced, the convention this project
follows is to **delete the number rather than restate it with a caveat**, keep
the reasoning it supported, and name the commit so the original evidence is one
`git show` away. Where a document and the code disagree, the code is the answer.
