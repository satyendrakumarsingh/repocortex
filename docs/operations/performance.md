# Performance

Every number in this document came from a run on the machine described below,
made while writing this revision. Where a default was chosen, the measurement
that chose it is reproduced alongside, because a default without its evidence is
just an opinion that has been in the tree a long time.

Numbers that could not be reproduced on this machine have been **deleted**, not
restated with a caveat. Where the reasoning behind a decision survives without
its original figures, the reasoning is kept and the figures are gone; the commit
that made the change is named instead, so the original evidence is one
`git show` away.

## The machine, and how to falsify any of this

| | |
|---|---|
| Host | Apple M4 Pro, 14 cores, macOS 26.5.1 |
| Toolchain | Go 1.25.13, `darwin/amd64` — an amd64 toolchain under Rosetta 2 |
| Build | `CGO_ENABLED=0 go build -o /tmp/di-docs ./cmd/cortex` |
| Tree | commit `6a7b909` |
| Cache | cold database each trial (`.cortex` removed, `cortex init` re-run) |

Go's own benchmark headers on this host print `goarch: amd64` and
`cpu: VirtualApple @ 2.50GHz`, which is the emulated view, not the silicon. The
*shape* of each curve survives the difference; the absolute numbers do not.
Reproduce them on your own machine before treating any of them as a target.

### How much the emulation costs

Enough to matter, so it is measured rather than asserted. The same source built
twice — `GOARCH=amd64` and `GOARCH=arm64`, `CGO_ENABLED=0` both times — over
RepoCortex's own source (315 files), best of three trials on an idle machine:

| Operation | amd64 under Rosetta | arm64 native | Native is |
|---|---|---|---|
| Full index | 1,801 ms | 1,264 ms | 1.43x faster |
| Re-index, nothing changed | 124 ms | 67 ms | 1.84x faster |
| Re-index, one file changed | 206 ms | 129 ms | 1.60x faster |

Query-side commands gain more, because they are dominated by process start and
CPU rather than by disk: on a 135,000-symbol corpus, p95 over 30 runs, startup
went 64.4 ms → 20.4 ms (3.2x), `search` 114.2 ms → 61.5 ms (1.9x), `symbol`
72.9 ms → 38.4 ms (1.9x), `callers` 76.2 ms → 41.0 ms (1.9x).

**So treat every figure below as a ceiling, roughly 1.4–3x above what a native
build delivers.** Released binaries are unaffected: CI cross-compiles
`darwin/arm64` alongside `darwin/amd64`, so an Apple Silicon user installing a
release gets the native build. This is a property of one development machine,
not of the product.

If you benchmark this project, check `go env GOARCH` against `uname -m` first.
They disagreed here for the entire life of the repository, and every performance
number recorded before this revision was silently emulated.

Three trials per figure, and all three are printed rather than averaged. Other
agents were building and testing this repository during some of these runs, so
the spread between trials is the honest picture of a shared developer laptop —
averaging it away would hide the variance a reader actually cares about.

## How these numbers were produced

Three corpora are used, and every figure below names which one it came from.

**1. `fixtures/payment-platform`** — in the repository. 10 files discovered, 9
indexed, 41 symbols, 55 references, 3 imports. Small enough that it measures
fixed costs (process start, migrations, transaction setup) rather than
throughput.

**2. RepoCortex's own source** — `git clone` of this repository at `6a7b909` into a
temporary directory, so the working tree is untouched and the corpus is exactly
what the commit contains. 303 files discovered, 275 indexed, 28 skipped, 4,013
symbols, 18,429 references, 1,416 imports.

**3. A 5,000-file synthetic Go corpus (`go5k`)** — generated, ~20 MB of source,
100 packages of 50 files each, 40 top-level declarations per file, yielding
210,000 symbols, 240,000 references and 10,000 imports. Each file's content is a
pure function of its index, so the corpus is byte-for-byte reproducible. The
generator emits a mix of functions, methods, types and constants, plus a `Drive`
function per file that calls into the previous file of the same package, so the
reference-extraction and binding paths have real cross-file work to do:

```python
# one file of the corpus, in outline
package pkg000
type Widget3 struct { ID int; Name string }
func Compute3_0(a, b int) int { ... }          # kind rotates every 5 declarations
func (w *Widget3) Render3_1() string { ... }
const Limit3_2 = 17
type Option3_3 struct { ... }
func Normalize3_4(s string) string { ... }     # calls Compute3_0
func Drive3(total int) int { ... }             # calls into file002 of pkg000
```

Method, for every timing below:

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
`/usr/bin/time -l … | grep 'maximum resident set size'`. Go benchmarks are run
with the exact command quoted in each section.

**One thing the synthetic corpus cannot measure.** Its identifiers repeat across
packages by construction, so `cortex resolve` on it reports 0 resolved and
80,000 ambiguous. Its resolve timings measure the throughput of the binding
path; they say nothing about binding *accuracy*. Every accuracy figure in this
document comes from a real corpus instead.

## Indexing

Three samples of each operation, in trial order.

### `fixtures/payment-platform` — 9 files indexed, 41 symbols

| Operation | Time |
|---|---|
| Full index into an empty database | 297 / 303 / 413 ms |
| Re-index, nothing changed | 22 / 36 / 66 ms |
| Re-index, one file changed | 94 / 102 / 112 ms |

`index.db` after the full index: 232 KiB.

At this size the run is almost entirely fixed cost, which is why a no-op re-index
of nine files still costs tens of milliseconds and why the spread across trials
is as wide as the value itself. Do not read a throughput number out of this
table; read one out of the 5,000-file table below.

### RepoCortex's own source — 275 files indexed, 4,013 symbols

| Operation | Time |
|---|---|
| Full index into an empty database | 2.02 / 1.74 / 1.82 s |
| Re-index, nothing changed | 84 / 60 / 61 ms |
| Re-index, one file changed | 182 ms |

`index.db` after the full index: 8.25 MB.

### The 5,000-file synthetic corpus — 210,000 symbols

| Operation | Time |
|---|---|
| Full index into an empty database | 28.1 / 34.6 / 19.7 s |
| Re-index, nothing changed | 972 / 965 / 703 ms |
| Re-index, one file changed | 818 / 1,115 / 883 ms |

`index.db` after the full index: 102.4 MB; after `resolve`, 105.0 MB.

The 19.7–34.6 s spread on the full index is machine contention, not corpus
variance — the file set is identical across trials and the resulting symbol
counts are identical to the unit. Treat the low end as the figure for an idle
machine and the high end as what a laptop compiling something else delivers.

**The gap between a full index and a no-op is the whole point of incremental
indexing**, and getting there required one decision that is easy to get wrong.

**Diffing is by content hash, not mtime.** `git checkout` rewrites timestamps
wholesale on files whose content is identical. An mtime-based diff would report
every file as modified after a branch switch and reparse the entire repository —
turning the cheapest common case into the most expensive one. Hashing costs a
read per file: that is what the ~0.9 s no-op run over 5,000 files is paying for,
and it is roughly 180 µs per file including the walk.

## Three real repositories

Every other figure in this document comes from a synthetic corpus or from
RepoCortex's own 275-file source. Those are uniform in ways real code is not, and
that uniformity hid real defects — a generated corpus could not show the
resolution memory win, and a fixture that names every symbol uniquely never
exercised the candidate cap at all.

Cloned at `--depth 1`, indexed and resolved on `darwin/arm64` native:

| repository | language | files discovered | indexed | symbols | edges | index | peak RSS |
|---|---|---|---|---|---|---|---|
| prometheus | Go | 1,262 | 1,030 | 21,429 | 35,787 | 8.8 s | 664 MB |
| nest | TypeScript | 2,297 | 1,929 | 13,916 | 4,064 | 3.7 s | 590 MB |
| spring-boot | Java | 11,851 | 9,236 | 94,304 | 119,480 | 46.4 s | 756 MB |

All twelve `doctor --verify-index` invariants pass on all three. Editing six
real files in prometheus one at a time and re-indexing after each produced an
index identical to a full rebuild of the same tree — same symbols, edges,
references, bindings and config usages, and the same 35,787 edge set.

Three things only a real repository showed:

**Peak memory was set by a threshold nobody had tested against real code.**
`DefaultDrainThreshold` was 1 GiB, on the reasoning that a gigabyte is above
what an ordinary repository reaches. A 1,262-file Go repository reached 837 MB,
so the pool never drained and the threshold WAS the peak. At 256 MiB the three
repositories give up 24–42% of their peak for 3.7–11% more time.

**`MaxInFlightBytes` does not bound memory**, despite having said so. Varying
it from 4 MiB to 32 MiB moved spring-boot's peak RSS by less than run-to-run
noise (1,347 / 1,305 / 1,377 / 1,350 MB). It bounds admission; the drain
threshold bounds memory.

**Retrieval missed the one obviously relevant class.** "Add retry to the
datasource health check" returned zero mentions of `DataSourceHealthIndicator`
— indexed at high confidence throughout — because `SplitIdentifier` yields
[data source health indicator] and no token equals "datasource". "data source"
returned five. The failing phrasing is the one Spring uses in
`spring.datasource.url`.

Edge counts differ enormously by language for a real reason, not a bug. nest
resolves 10,361 references but produces 4,064 edges, because only 4,110 of
those references sit inside an indexed symbol: TypeScript puts most references
at module top level, and 1,929 files yielded 364 functions. A reference outside
any symbol has no source to hang an edge on, and inventing one would corrupt
caller queries.

## Migration 0005 — indexing the foreign keys that reference `symbols(id)`

`internal/store/migrations/0005_fk_indexes.sql` adds indexes on the columns that
reference `symbols(id)`. It is the largest single performance change in the tree,
and the trade it makes is deliberate: a little more disk and a slower full build,
in exchange for an incremental re-index that no longer degrades with repository
size.

Measured on a 5,000-file / 200,000-symbol synthetic Go corpus, `CGO_ENABLED=0`,
3 trials, with the same `darwin/amd64`-under-Rosetta binary on both sides — so
the ratio is like-for-like even though the absolute seconds are emulated:

| | Before 0005 | After 0005 | Change |
|---|---|---|---|
| One-file re-index | 5.10 s | 0.80 s | **6.3x faster** |
| Database size | 105.9 MB | 112.4 MB | +6.13% |
| Full index | 28.9 s | 32.1 s | +11% |
| Symbols indexed | 200,000 | 200,000 | identical |

The identical symbol count is the part that makes the rest trustworthy: the
indexes changed how rows are found, not which rows exist.

The asymmetry is the point. A full index is something a developer runs rarely and
knowingly; a one-file re-index is what `cortex watch` runs on every save. Paying
11% on the rare operation to take 84% off the common one is the right side of that
trade, and 6% more disk is the cheapest currency available.

## A full re-index costs about what a first index costs

An earlier revision of this document recorded that `cortex index --full`
against a database already holding a committed run cost roughly **ten times** the
same command against an empty one, and told readers to keep `index --full` out of
loops and hooks for that reason.

**That is no longer true**, and the advice built on it has been removed. Four
consecutive `cortex index --full` runs over RepoCortex's own source, on one
database, with no source changes between them:

| Run | Duration | `index.db` after |
|---|---|---|
| 1 — into an empty database | 1.83 s | 8.26 MB |
| 2 | 2.11 s | 16.23 MB |
| 3 | 2.12 s | 17.75 MB |
| 4 | 2.16 s | 18.03 MB |

Retiring a previous run now costs about 15%, not 900%. The cascade delete that
dominated it is the same cascade — what changed is that migration 0005 gave it
indexes to use instead of table scans.

For the same reason, `cortex init --force` followed by `cortex index` is no
longer meaningfully cheaper than `cortex index --full`: 1.75 s against 2.11 s
on the same tree. It remains a different *operation* — it discards everything
already indexed, so it is a rebuild rather than a refresh — but it is no longer a
performance workaround.

The database size still roughly doubles on the second run and then plateaus. Two
full runs are resident on disk for the duration of the staging phase, and SQLite
does not return freed pages to the filesystem, so the file stops growing once the
free list is large enough to reuse.

What has *not* been measured is how this scales past a 300-file repository on one
database. The plateau above is one repository on one machine.

## Resolution

**The resolver itself: 842 ns per reference.** From `BenchmarkResolveFile` in
`internal/resolve/infer`, which binds 500 references against a 5,000-symbol
index:

```bash
go test -run '^$' -bench BenchmarkResolveFile -benchmem -count=5 ./internal/resolve/infer/
```

Five runs: 427.4 / 420.1 / 420.8 / 421.6 / 416.6 µs per operation, 785 KB and
2,003 allocations per operation. Divided by the 500 references in the fixture,
that is 833–855 ns per reference.

**Do not read that as the cost of resolving a repository.** `cortex resolve`
over RepoCortex's own source — 18,429 references, 7,708 edges written, plus
data-access and config binding — took 627 / 507 / 518 ms over three samples, each
after a fresh full index. That is **28 µs per reference end to end, roughly 33x
the 842 ns inside the resolver.** The wall-clock cost is dominated by reading
references out of SQLite and writing edges back, not by the binding logic.
Optimising the resolver further would not move it. Any statement of the form
"N references resolve in T seconds" derived from the microbenchmark alone is
wrong by that factor.

The design the microbenchmark measures is the result of refusing two obvious
alternatives — per-reference SQL would be one query per reference, and a nested
loop would be the product of the two counts. References stream file by file, so
nothing accumulates, and edges flush in batches of `DefaultResolveBatch = 2000`
rather than being collected.

### Memory

Resolution's peak memory is flat in repository size. It did not use to be: the
run's entire symbol index was loaded into one map before anything was bound, so
memory tracked the repository rather than the work.

Synthetic Go corpus, ~15 references per file, `cortex resolve` alone, peak RSS:

| files | symbols | before | after |
|---|---|---|---|
| 1,000 | 29,000 | 71 MB | 68 MB |
| 5,000 | 145,000 | 205 MB | 168 MB |
| 25,000 | 725,000 | 500 MB | 205 MB |
| 50,000 | 1,450,000 | 819 MB | 208 MB |

Doubling the tree from 25,000 to 50,000 files now costs 3 MB. Go heap alone went
132 MB to 25 MB at 25,000 files; what remains in RSS is the binary image and
SQLite's page cache, both constant. Resolution time is unchanged (1.01x–1.08x)
and the edges and references produced are byte-identical at every size.

Three bounds do it, and each removes a term that grew with the repository:
`registry.CandidateCache` reads candidates per page behind an LRU bounded by
candidate rows; reads stop at `infer.MaxCandidates + 1` rows per name, because
the resolver abandons an over-subscribed name on the count alone; and the file
list is paged by key rather than materialised.

### On superlinearity

An earlier revision recorded that resolution scaled superlinearly — 44.2x for
25x the input, reduced to 34.2x (exponent 1.10) by writing resolution outcomes
in key order. **The residual does not reproduce here.** Measured on this
machine, native `darwin/arm64`, `cortex resolve` on prebuilt indexes:

| corpus | 25x input costs | exponent |
|---|---|---|
| ~15 refs/file (29k → 725k symbols) | 22.7x | 0.97 |
| ~100 refs/file (100k → 2.5M references) | 25.1x | 1.00 |
| ~100 refs/file, page cache cut to 8 MB | 28.3x | 1.04 |

The third row is the interesting one. The mechanism proposed for the residual
was index B-trees outgrowing the 64 MB page cache, so starving the cache
eightfold should exaggerate it; it moved the exponent from 1.00 to 1.04.

Two readings are available and this document does not choose between them: the
key-order fix removed more than it was credited with, or the 1.10 exponent was
a property of a corpus not reproduced here. What can be said is that on every
corpus available to this revision, up to 2.5 million references, resolution is
linear. `TestResolveScalesLinearly` currently reports 1.27x growth in
per-reference cost for 4x the references against a 2.5x bound.

### What it actually resolves

On RepoCortex's own source, identically across all three trials:

```
  References:  18429
  Resolved:    7880 (42.8%)
  Ambiguous:   903
  Unresolved:  9646  (calls outside this repository)
  Edges:       7708
  Data access: 329 statement(s), 320 bound to a declared table, 320 edges
```

On the payment fixture, `cortex resolve` reconstructs the callback flow from
source — `handleCallback -> updateStatus -> {save, findById}` — in 25 / 36 / 38 ms:

```
Resolved pay2 (revision 1)
  References:  55
  Resolved:    16 (29.1%)
  Ambiguous:   10
  Unresolved:  29  (calls outside this repository)
  Edges:       16
    high:    16
  Data access: 7 statement(s), 7 bound to a declared table, 7 edges
```

Those 29 references are genuinely external — Spring, JDBC, test frameworks — and
are recorded as unresolved rather than hidden. "How much of this repository can we
actually resolve" is a question the product has to answer honestly, so the
unresolved count is a published fact, not a swept-under number.

The `Data access` line is not decoration: `resolve` is where staged table accesses
and config reads are bound too, so its duration covers more than reference
binding.

## Importing a compiler-backed index

Measured on RepoCortex's own source with `scip-go` 0.2.7, whose output on this tree
is a 7.2 MB index:

```
scip-go 0.2.7: 288 document(s), 251 matched
symbols: 4071 matched, 13729 with no counterpart in the index
edges:   14758 new, 0 upgraded from heuristic, 918 already known
         67787 reference(s) point outside the indexed repository
         3050 reference(s) the indexer attributed to a file rather than a definition were skipped
duration: 600ms
```

Three trials: 600 / 575 / 871 ms. The 87% document match rate is stable; the
unmatched 37 are paths under Go's build cache, which the index never saw.

`0 upgraded from heuristic` is the expected result, not a failure — SCIP records
that one symbol references another, not that it *calls* it, so compiler facts
arrive as `REFERENCES` edges alongside the resolver's `CALLS` edges rather than
replacing them.

## Parallelism and peak memory

Measured on the 5,000-file synthetic corpus, `/usr/bin/time -l` around
`cortex index --full --workers N`, one run each:

| Workers | Peak RSS | Elapsed |
|---|---|---|
| 1 | 255 MB | 56.3 s |
| 2 | 255 MB | 45.9 s |
| 4 | 292 MB | 33.8 s |
| 8 | 310 MB | 31.9 s |
| 10 | 349 MB | 27.2 s |
| 14 | 386 MB | 30.3 s |

**Read this table as a counter-example to the default, not as support for it.**
On a corpus of 5,000 uniformly small files, memory barely moves with worker count
and throughput keeps improving to 10 workers. `pipeline.defaultWorkers()`
nonetheless returns 4 (or `runtime.NumCPU()` when the machine has fewer than 4
cores), because the failure mode it guards against does not appear on a corpus
like this one:

**Peak memory tracks worker count and file size, not repository size.** A worker
count bounds concurrency; it does not bound memory, because file sizes vary by
orders of magnitude. Eight workers that each happen to pick up a megabyte of
generated code will allocate gigabytes between them, and the *scheduler* decides
when that happens — which makes peak memory a property of file ordering. That is
the shape a uniform corpus cannot reproduce, and it is why the default is set for
the repository a developer actually has rather than for the one that benchmarks
best.

The original measurements that chose 4 were taken on a 10,920-file tree that is
not in this repository and could not be reconstructed for this revision. **Their
figures have been removed rather than repeated**; they survive in the commit that
made the change (`git log --grep 'defaultWorkers'`), where they can be read with
their own provenance attached.

If you have memory to spare and a repository of small files, the table above is a
reasonable argument for raising `--workers`. Measure your own tree before you do —
the knee moves with average file size.

### What bounds memory instead of the worker count

`pipeline.memBudget`. Workers acquire budget proportional to the bytes they are
about to parse, *before* reading the file:

- Small files run wide.
- A large file waits until the budget frees up, then effectively runs alone.
- A file larger than the entire budget is clamped and admitted alone rather than
  deadlocking. A file that can never be indexed is worse than a brief spike.

`DefaultInFlightBytes = 32 MiB` (verified in `internal/pipeline/pipeline.go:83`).
That is 32 MiB of *source*, not of memory — tree-sitter trees run one to two
orders of magnitude larger than their input. Enough to keep every worker busy on
ordinary code; low enough that a directory of generated files cannot exhaust the
machine. Throughput on ordinary code is unchanged, because ordinary files are
small.

### The parse cap

`parse.MaxSourceBytes` is **256 KiB** (`internal/parse/generic.go:50`). It was
lowered from 2 MiB because tree-sitter's node arenas scale super-linearly with
input, and that cost is internal to the library — it cannot be bounded from
outside, only avoided. A quarter-megabyte source file is a generated parser
table, a minified bundle, or embedded data, none of which yield symbols worth the
arena.

The cap-sweep measurements that chose 256 KiB were taken on a 2,110-file corpus
that is not in this repository. **Those figures have been removed rather than
repeated.** Raising the constant is a source change, and anyone making it should
re-measure on their own corpus rather than trust a number from a tree they cannot
see.

Skipped files are still recorded in the `files` table with `parse_status =
'skipped'` and a reason, so the gap is visible in `cortex status` and in the
index rather than looking like the file never existed.

## Two more allocation controls

### Arena draining

`gotreesitter` pools the arenas it allocates for parse trees, and its own
documentation is explicit that pooled arenas are strong Go references that are
not collected until explicitly drained. A batch scan over a repository is exactly
the case that warning describes: left undrained, the arena for every file ever
parsed stays reachable, so peak memory becomes the sum of every parse rather than
the largest few.

The pipeline drains every `parse.DrainInterval = 128` files — but only when the
heap warrants it. `Runner.maybeDrain` samples `runtime.MemStats` and returns
immediately unless `HeapInuse` exceeds `DefaultDrainThreshold = 1 GiB`. Both
constants verified in `internal/parse/arena.go:67` and
`internal/pipeline/pipeline.go:118`.

Sampling the heap first makes the cost proportional to the problem: on a
repository that never grows large, nothing is ever drained and there is no cost at
all. `ReadMemStats` stops the world briefly, which is why it runs once per 128
files rather than per file. An unconditional periodic drain was measured and
rejected during the original memory work; that comparison's figures are not
reproducible here and have been removed.

There is also a final unconditional drain when a run ends. Without it, the
indexer's peak footprint stays resident for the lifetime of a long-running
`cortex watch`.

### Tagger pools

Draining the library's pools is not sufficient. A pooled `Tagger` owns a live
`Parser`, so its arena is **not** in `gotreesitter`'s pool and is not reachable by
`DrainArenaPools`. `registry.DrainPools` (via the `parse.Drainable` interface)
drains both. The pool is a channel rather than a `sync.Pool` specifically because
it must be evictable on demand — `sync.Pool` gives the GC the decision, and the GC
will not free anything, because the pool holds strong references.

A related guard: `parse.PoolReturnLimit = 128 KiB`
(`internal/parse/generic.go:27`). A Tagger that has parsed a large file has grown
an arena that never shrinks, so it is dropped rather than returned to the pool.

## Graph traversal

From `internal/graph/bench_test.go` on its 100,000-symbol / 500,000-edge fixture:

```bash
go test -run '^$' -bench BenchmarkTraverse -benchtime 30x ./internal/graph/
```

p95 wall time for a depth-3 walk from 100 roots, by node cap:

| `MaxNodes` | p95 | Nodes reached |
|---|---|---|
| 1,000 | 3.0 ms | 1,000 |
| 10,000 | 27.1 ms | 10,000 |
| 50,000 | 78.0 ms | 28,193 |

A single-root depth-3 walk is 0.53 ms p95, reaching 199 nodes.

The traversal is level-synchronous: each BFS frontier becomes one indexed
`src_id IN (...)` query, chunked to stay inside SQLite's variable limit, so a
depth-3 walk costs 3 round trips rather than one per node discovered.

**`MaxNodes`, not depth, is what keeps this interactive.** The one shape that
ignores the bound proves it: `Direction=Both`, depth 3, no filters, no effective
node cap reaches 99,821 of the 100,000 symbols in **1.62 s p95**. That is a
whole-graph load wearing a traversal's clothes, which is why `MaxNodes` defaults
to a small value rather than to unlimited.

## Git operations

Measured on the synthetic 500-commit, 200-file repository built by
`internal/acquire/git/bench_test.go`, go-git v5.19.2, three runs each:

```bash
go test -run '^$' -bench . -benchtime 1x -count=3 ./internal/acquire/git/
```

`-benchtime 1x` is deliberate: several of these take seconds per operation, so the
default 1-second budget runs them once anyway, and pinning it to one iteration
makes the cheap and expensive rows comparable. The column below is the median of
the three.

| Operation | Time (median of 3) |
|---|---|
| `Open` | 1.3 ms |
| `Info` (includes worktree scan) | 14.9 ms |
| `Status` | 12.6 ms |
| `ChangedSince` HEAD~1..HEAD | 18.5 ms |
| `ChangedSince` HEAD~200..HEAD | 72.1 ms |
| `ChangedSince` from empty | 7.8 ms |
| `RecentCommitsMeta(50)` | 15.0 ms |
| `RecentCommits(50)` with paths | 698 ms |
| `RecentCommits(500)` with paths | 7.3 s |
| `FileLastModified`, recent file | 3.9 ms |
| `FileLastModified`, absent path | 594 ms (full 500-commit walk) |
| go-git `Blame`, one file | 2.05 s |

The number governing all of these is the tree diff, CPU-bound in go-git's tree
decoding rather than I/O-bound. That is why `RecentCommitsMeta` exists — 15 ms
against 698 ms for the same 50 commits, a factor of **47** here — and why callers
needing per-commit paths for more than a few dozen commits should persist them
rather than recompute.

Blame is deliberately **not exposed**. Two seconds for one file on a 500-commit
repository is not a per-file cost an indexer can pay, and it grows with history.
`BenchmarkGoGitBlame` stays in the tree as the evidence so the decision can be
revisited against measurements rather than impressions.

## Rename and edit

`cortex rename` does no indexing of its own: it reads the committed index,
then locates each site on its line, splices bottom-up, and writes each file once
atomically. The two costs worth knowing are measured on the machine above
(`darwin/amd64` under Rosetta 2, so the absolute numbers are the emulated view):

```bash
go test -run '^$' -bench . -benchmem -count=3 ./internal/edit/
```

| Benchmark | Cost |
|---|---|
| `BenchmarkLocate` — find one identifier on line 2,001 of a 2,000-line file | 39.1-41.8 µs/op, 21 B, 2 allocations |
| `BenchmarkLocateAndSplice1MiB2000Sites` — locate and splice 2,000 sites in a 1 MiB file | 3.51-3.60 ms/op (295-303 MB/s), 1.30 MB, 4,004 allocations |

The second shape is every site the command's default `--max-sites 2000` allows,
in a single 1 MiB file; the locate-and-splice is designed to be linear in file
size and site count. Writing adds one hash check, one
temporary file and one rename per changed file.

A dry run end to end, on a copy of `fixtures/payment-platform` that was already
indexed and resolved (`cortex rename updateStatus applyStatus`, five runs):
wall time 0.09-0.11 s, most of it process start and opening the index. A dry
run reads the database only.

## Other measured costs

```bash
go test -run '^$' -bench . -benchmem ./internal/observe/ ./internal/errs/
```

| Thing | Cost |
|---|---|
| `StartSpan`, telemetry disabled | 2.7 ns/op, zero allocations |
| `StartSpan`, disabled, with attributes | 76.9 ns/op, zero allocations |
| `SpanSetAttr`, disabled | 4.5 ns/op, zero allocations |
| `SpanRecordError`, disabled | 2.3 ns/op, zero allocations |
| `MetricRecord`, disabled | 7.1 ns/op, zero allocations |
| The safe attribute constructors | 32.4 ns/op, zero allocations |
| `StartSpan`, telemetry **enabled** | 8,345 ns/op, 4,395 B, 10 allocations |
| `MetricRecord`, telemetry **enabled** | 203 ns/op, 40 B, 2 allocations |
| `errs.KindOf`, classified error | 540 ns/op, zero allocations |
| `errs.KindOf`, unclassified error | 633 ns/op, zero allocations |
| `errs.IsRetryable`, classified error | 415 ns/op, zero allocations |

The zero-allocation property on the disabled path is what these benchmarks exist
to hold, and it survives; the nanosecond figures move with the machine and with
emulation. Note the three-orders-of-magnitude gap between a disabled and an
enabled span — telemetry being off by default is not a stylistic choice about
noise.

`registry.DefaultBatchSize = 1000` rows per write transaction
(`internal/registry/registry.go:73`). It trades SQLite's per-transaction commit
overhead, which wants large batches, against how long a write transaction blocks
concurrent readers on a single connection, which wants small ones. No benchmark
in the tree sweeps this value, so this document states the trade and not a
number.

Two other constants a reader may be looking for, both verified in the tree rather
than measured: `parse.DefaultParseTimeout = 5 s`
(`internal/parse/generic.go:23`) and `watch.DefaultMaxWatchDirs = 8192`
(`internal/watch/watch.go:62`).

## Tuning for a large monorepo

The defaults target a developer laptop indexing its own repository while an IDE is
running. A dedicated indexing machine can afford more.

**Raise workers if you have memory to spare**, and measure — see the worker table
above, and read the warning attached to it:

```bash
cortex index --full --workers 8
```

Watch peak RSS. The knee moves with average file size: a repository of small files
tolerates more workers than one full of large generated ones. `--workers` only
applies to the full-index path; see the caveats below.

**Raise `MaxSourceBytes` only if you must index generated code.** It is a
compile-time constant in `internal/parse/generic.go`, not a flag, precisely
because raising it is a decision with a memory price rather than a preference.
Before raising it, check what you would actually gain — `cortex status` reports
skipped files, and the `files` table records the reason per path.

**Exclude generated trees instead.** Almost always the better answer. Repository
excludes are additive to the defaults:

```yaml
index:
  exclude:
    - "**/generated/**"
    - "**/*.pb.go"
    - "**/vendor/**"
```

An excluded file is never read, never hashed and never parsed. A file skipped by
`MaxSourceBytes` has already been read and hashed.

**Use `cortex watch` rather than repeated full indexes.** On the 5,000-file
corpus a full index is 20–35 s while an incremental pass over an unchanged tree is
under a second, and a one-file change is around 0.8 s — one hash per file plus a
reparse of what actually changed.

### Caveats worth knowing before you tune

- `--workers` and `--accuracy` take effect only on the **full** index path.
  Without `--full`, `cortex index` takes the incremental path and neither is
  applied. They are, however, **validated on every path** — a typo fails
  immediately rather than being silently discarded:

  ```
  $ cortex index --accuracy nonsense
  error: --accuracy "nonsense": want fast, auto or exact     # exit 1
  ```

- `fs.DefaultMaxFileSize` (2 MiB, `internal/acquire/fs/walk.go:47`) and
  `parse.MaxSourceBytes` (256 KiB) are different limits. The first is a
  discovery-time ceiling on what gets hashed at all; the second is a parse-time
  ceiling. A 1 MiB source file is hashed and recorded but not parsed.
- The discovery walker still defaults to `runtime.NumCPU()` workers
  (`fs.WithWorkers`). Walking is I/O-bound and allocates little, so it was not
  part of the memory work.

## Context assembly at scale

Spec §39 sets a P95 target of 2 s for context assembly, measured on a synthetic
94,000-symbol repository by `TestP95LatencyAt94kSymbols` in
`internal/context/latency_test.go`, behind the `perf` build tag.

An earlier revision of this document reported that the gate did not compile and
did not measure what it claimed. **Both defects are fixed.** The gate builds,
runs, and passes:

```bash
go test -tags perf -timeout 25m -v -run 'TestP95|TestCatalogueCacheRemoves' ./internal/context/
```

```
94,000 symbols
  cold (builds the catalogue): 948ms
  warm p50=55ms p95=57ms p99=71ms max=71ms over 60 runs
  cache 60 hits / 1 misses, heap 75 MB
--- PASS: TestP95LatencyAt94kSymbols (13.06s)
--- PASS: TestCatalogueCacheRemovesTheDominantCost (0.93s)
    10,000 symbols: cold 91ms, warm 10ms
```

Three things in that output carry the weight:

- **p95 is 57 ms against a 2 s spec ceiling**, and against the tighter 200 ms
  regression gate the test also asserts. The tighter gate is the useful one: the
  ceiling has 35x headroom and would not notice a large regression.
- **60 hits / 1 miss** is machine-independent proof that the cached path is
  actually being exercised. The earlier version reported 0 hits / 0 misses, which
  was proof of the opposite.
- **Cold 948 ms against warm 55 ms** is the cost the cache exists to remove. At
  10,000 symbols the same comparison is 91 ms cold against 10 ms warm.

The cost being removed is the in-memory catalogue — two table scans plus enclosure
resolution and index building — which was rebuilt on every single question. An
agent asking three questions in a session paid for it three times, and the cost
scaled with the repository rather than with the question.

**Keying the cache on the run id alone would have been wrong.** A committed run is
not immutable: incremental indexing updates it IN PLACE, so the same id describes
different content minute to minute while a developer edits. A cache keyed on the
id would have served pre-edit answers for as long as the process lived — the exact
staleness this product exists to remove.

The key is a content fingerprint instead: file count, symbol count, and the
maximum id of each. Four indexed aggregates, checked on every request, one SQL
statement in `registry.CatalogueFingerprint`. It detects every mutation because
ids are AUTOINCREMENT — an insert always advances the maximum even when a
delete-then-insert leaves the count unchanged, which is exactly what `ReplaceFile`
does on every incremental update. A delete shows up in the count. The combination
that would fool it — removing rows and adding exactly as many while the maximum
stays put — cannot happen, because an id is never reissued.

Both directions are proven by tests that perform a real in-place update and a real
delete, and assert the rebuilt catalogue reflects them.

The gates skip under `-race` and sit behind the `perf` tag rather than in the
default suite, because the detector dominates the measurement: asserting a time
budget against instrumented code measures the detector rather than the product.
Correctness still runs under `-race`.
