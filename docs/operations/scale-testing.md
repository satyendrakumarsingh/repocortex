# Scale testing

How RepoCortex is tested at the size of a large repository, what it measured,
and how to run the same measurement on your machine.

The suite is `tests/scale`. It runs in the default `go test ./...` at a small
size and takes the real measurement when asked for a larger one.

## Contents

- [Why a separate suite](#why-a-separate-suite)
- [Running it](#running-it)
- [What it asserts](#what-it-asserts)
- [Measured results](#measured-results)
- [What the suite found](#what-the-suite-found)
- [Adding a test](#adding-a-test)

## Why a separate suite

Unit tests check correctness on a dozen symbols. A query that is correct on a
dozen symbols can still read every relationship in the index once per file, and
nothing but size shows it. That exact defect was written while this suite was
being built: `repocortex hotspots` passed every unit test and did not return in two
minutes on a 19,000-relationship index.

## Running it

```bash
go test ./tests/scale/                               # the CI gate: 5,000 symbols, a few seconds
```

```bash
go test ./tests/scale/ -v -timeout 40m -scale.symbols=250000 -scale.source-files=5000 -scale.commits=2000
```

```bash
go test ./tests/scale/ -v -timeout 90m -scale.symbols=1000000 -scale.source-files=20000
```

| Flag | Environment variable | Default | Sets |
|---|---|---|---|
| `-scale.symbols` | `REPOCORTEX_SCALE_SYMBOLS` | 5,000 | Symbols in the synthetic graph |
| `-scale.source-files` | `REPOCORTEX_SCALE_SOURCE_FILES` | 150 | Files in the generated source tree |
| `-scale.commits` | `REPOCORTEX_SCALE_COMMITS` | 400 | Commits in the synthetic history |

Run with `-v`: each test logs what it built and what it measured.

It is not behind a build tag. A tagged suite is invisible to the compiler, to
`go vet` and to the default run, so it rots until the night someone needs it. A
suite that always runs small cannot.

A million symbols needs about 4 GB of memory for the in-memory fixture.

## What it asserts

Three properties, most important first.

**1. Every answer is bounded.** A response to an agent must fit the host's
inline limit (24 KB) whatever the size of the index. An answer over the limit is
written to a file by the host and read back in pieces, which costs more than the
search the tool replaced. `TestEveryToolAnswerStaysBoundedAtScale` calls every
registered tool and fails if a tool has no call in the test.

**2. Memory is bounded.** A streaming scan must not keep memory in proportion to
what it scanned. The suite compares live heap before and after each operation,
after a collection on both sides.

**3. Time grows no worse than linearly.** A budget is a fixed allowance plus an
allowance per symbol or per file:

```go
budget{fixed: 2 * time.Second, perUnit: 60 * time.Microsecond}
```

So the same test fails at 5,000 symbols and at a million for the same reason:
something became quadratic. Budgets are several times looser than the measured
figures below. They are tripwires for a change in the shape of a cost; a tight
budget on a shared CI runner is a flaky test.

Two kinds of fixture are used:

| Fixture | Built by | Finds |
|---|---|---|
| Synthetic graph | Rows inserted straight into the store, hub-heavy like a real call graph | Query plans, traversal bounds, answer size |
| Generated source tree | Go files on disk, then `repocortex init`, `index` and `resolve` | Parsing, extraction, resolution, the commit |

## Measured results

One run, Apple M4 Pro, Go 1.26, no race detector, on 2026-11-12. The fixture was
250,000 symbols and 1,258,823 relationships across 2,500 files, with a history
of 2,000 commits. Reproduce before treating any figure as a target.

### Whole-repository reports

| Operation | Time | Heap left behind |
|---|---|---|
| Unused-code scan (10,638 unreferenced definitions found) | 4.2 s | 1.1 MB |
| Unused-code scan, one directory | 3.9 s | 0.3 MB |
| Hotspot ranking (2,375 changed files) | 3.0 s | none |
| Path between two symbols | 10 ms | none |
| Impact from a hub with 1,000 callers | 1.0 s | 170 MB (the symbol index, cached once per server) |
| Test discovery for one symbol | 2 ms | none |

### Every tool, warm, 95th percentile of five calls

Measured with nothing else running on the machine. The same test run alongside
other test suites reported `list_dead_code` at 15 s instead of 4.8 s: a p95 of
five samples moves a great deal under load, so take these on an idle machine.

| Tool | p95 | Answer |
|---|---|---|
| `find_symbol`, `get_symbol`, `get_resolution_coverage`, `search_code`, `minimize_diff`, `find_tests`, `find_owners` | 2–5 ms | 0.5–1.7 KB |
| `find_callers`, `find_callees`, `find_references`, `find_implementations`, `analyze_impact`, `analyze_change` | 54–78 ms | 0.4–7.4 KB |
| `trace_call_path`, `plan_rename`, `get_task_context`, `plan_change` | 85–95 ms | 1.7–18.3 KB |
| `list_api_endpoints`, `get_config_key`, `get_project_overview` | 174–347 ms | 0.7–0.8 KB |
| `list_blindspots` | 1.5 s | 10.3 KB |
| `list_hotspots` | 3.3 s | 2.4 KB |
| `list_dead_code` | 4.8 s | 5.6 KB |

Every answer was under the 24 KB limit. The largest was `get_task_context` at
18.3 KB.

Two of these were several times slower in the first run and were fixed by
bounding their test probes: `plan_change` went from 4.4 s to 92 ms and
`list_blindspots` from 5.5 s to 1.5 s.

### Indexing a generated source tree

| Files | Symbols | `init` + `index` + `resolve` | Throughput |
|---|---|---|---|
| 2,502 | 13,752 | 6.5 s | 385 files a second |
| 5,002 | 27,502 | 13.4 s | 374 files a second |

Doubling the tree multiplied the time by 2.06. On the larger tree, a query for
the callers of a function called from every file took 69 ms, `dead-code` took
0.6 s and returned only the planted unused functions, and re-indexing with
nothing changed took 1.0 s.

## What the suite found

| Defect | Measured | Fix |
|---|---|---|
| The unused-code query read every relationship once per candidate when the database had no statistics | 8.5 s at 5,000 symbols | Each probe names its index: 36 ms. Pinned by `TestUnreferencedSymbolsUsesItsIndexes` |
| The hotspot query read every relationship once per file | Over 2 minutes on a 19,000-relationship index | Join order forced. Pinned by `TestFileStructureForUsesTheDestinationIndex` |
| `list_blindspots` ran one count query per exported function and loaded every symbol | 2.4 s at 5,000 symbols | One aggregate query |
| A hotspot score of zero for every file with no recorded size | All scores 0 | Size floors at one byte |

Still open, and in the [implementation plan](../IMPLEMENTATION_PLAN.md#phase-2-depth):
`list_dead_code` and `list_hotspots` take 3 to 5 seconds at this size, because
each visits every definition or every changed file once; and the symbol index
holds about 170 MB for 250,000 symbols once loaded.

## Adding a test

1. Use the shared fixture (`load(t)`); do not build another. At a million
   symbols the build is the expensive part.
2. Wrap the operation in `measure`, then call `requireWithin` with a budget whose
   two terms are written at the call site, and `requireRetainedUnder` if the
   operation streams.
3. Never call `t.Parallel()`. Heap figures are process-wide.
4. If the test adds a tool, add its call to `TestEveryToolAnswerStaysBoundedAtScale`.
   The test fails until you do.
5. Pin a query plan with `EXPLAIN QUERY PLAN` in the package that owns the query
   when a fix depends on the plan. A plan regresses without any test of the
   result noticing.
