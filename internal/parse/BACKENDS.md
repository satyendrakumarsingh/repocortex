# Parser backends

RepoCortex parses with a pure-Go tree-sitter runtime by default, and can be built
against the official C runtime instead. This document explains how, when you
would want to, what it costs, and — most importantly — what the differential
test does and does not prove.

## The two backends

| | default | `-tags cgo_parser` |
|---|---|---|
| runtime | `github.com/odvcencio/gotreesitter` | `github.com/tree-sitter/go-tree-sitter` |
| implementation | pure Go | C, via CGO |
| grammars | 206, embedded | 4 (go, java, python, typescript) |
| C toolchain | not required | required |
| `go install` | works everywhere | needs a C compiler on the target |
| cross-compilation | five targets from one runner | one target per host |

Both implement `parse.Backend` (`backend.go`), which carries a language name,
source bytes and tagged symbols, and nothing else. The seam is narrow on
purpose: timeouts, pooling, size caps, confidence and provenance stay in
`genericParser` where there is exactly one copy of them, so the two backends
have no opportunity to implement RepoCortex's invariants differently.

## Building with the C runtime

```bash
CGO_ENABLED=1 go build -tags cgo_parser ./...
```

You need a working C compiler; the four grammar packages compile their
`parser.c` from source on first build, which takes minutes on a cold cache.

`CGO_ENABLED=0` together with `-tags cgo_parser` does not work and is not meant
to — the C bindings cannot compile without cgo. If you hit that, you asked for
the CGO backend in a build that forbids CGO; pick one.

`parse.ActiveBackend()` returns whichever runtime the build selected, and
`parse.ActiveBackendName` names it. Callers never branch on the answer.

## When you would actually want this

**Large monorepos where the pure-Go memory profile hurts.** This is the real
reason. gotreesitter's GLR parser allocates node arenas that scale
super-linearly with input size and are pooled as strong Go references — see the
measurements in the `MaxSourceBytes` comment in `generic.go` (6.7 GB peak on a
2,110-file corpus at a 2 MiB cap) and in `arena.go`. `MaxSourceBytes` and
`DrainPools` exist to contain that, and on most repositories they do. On a very
large monorepo, or one heavy with generated code, they may not be enough.

**Upstream risk.** `gotreesitter` was first published on 2026-02-20 and 3,426 of
its roughly 3,500 commits come from a single author. It is the largest
single-point dependency risk in RepoCortex, and every supported language degrades
at once if it stalls. This backend is the answer to "what do we do then" — and
the differential test is what makes that answer more than a hope.

**Do not reach for this to make parsing faster.** Nobody has measured that, and
CONTRIBUTING.md is explicit about not publishing numbers a run did not produce.

## What you give up

- **`go install repocortex` stops being trivial.** Every user needs a C toolchain.
  This is the single biggest cost and the reason the default will not change.
- **Cross-compilation stops working the easy way.** The release pipeline builds
  five targets from one runner with `CGO_ENABLED=0`. With CGO you need a cross
  toolchain, or one runner per target.
- **202 languages.** The CGO backend has four grammars. Every other language
  still needs the pure-Go path, so this is not a whole-product swap.
- **References and imports.** The seam covers symbol extraction only. Reference
  and import extraction still run through `genericParser` and gotreesitter.

That last point is why the CGO backend is **not** wired into the production
parsing path today. Routing `genericParser` through it would silently drop
reference edges for the four covered languages, and a graph quietly missing
edges is worse than one that never claimed them. The seam, the compile-time
swap and the evidence are in place; production routing is the next step, and it
needs the CGO backend to reach parity on references and imports first.

## The differential test

`differential_test.go`, run with `-tags cgo_parser`. It parses the
`fixtures/payment-platform` corpus plus generated sources covering classes,
interfaces, methods, generics, annotations and decorators, nested types and
malformed input, through **both** backends and compares the results.

It compares **sets of (name, start line)**. Not orderings — the two query
engines yield matches in different orders, which is not a disagreement. Not
columns or end positions — the two grammars place a declaration node's start
differently depending on modifiers, annotations and decorators, which is also
not a disagreement.

### The gates, and why they are asymmetric

The obvious design gates on two-way similarity. That design is wrong here and
would damage the project: the C runtime's grammars find symbols RepoCortex's own
tags supplements currently miss. A symmetric gate counts that extra coverage as
a failure, and the cheapest way to go green would be to **delete working
patterns from the CGO backend**. A test that rewards removing coverage is worse
than no test.

So there is one hard coverage gate, in the direction that decides whether the
escape hatch is usable:

| metric | gate | meaning |
|---|---|---|
| **cgo recall** | ≥ 0.95 | share of what the shipping pure-Go path finds that the C runtime also finds — what you would **lose** by flipping the tag |
| **kind agreement** | ≥ 0.90 | among symbols both found, do they agree what it *is* — a class labelled an interface puts wrong edges in the graph |
| Jaccard similarity | ≥ 0.60 tripwire | catches the catastrophic case of two near-disjoint symbol sets |
| pure recall | reported only | a shortfall is a missing pattern in `queries.go`, not a defect in the fallback |

Every disagreement is printed in full, grouped by file. A differential test that
reports only "83% match" is useless: the disagreements are the finding.

`TestDifferentialGateDetectsRegression` injects a backend that silently drops
two symbols in three and asserts the gate notices. A gate only ever seen passing
proves nothing.

### Normalisations

Each is documented in the test with its reason, and each is a place where the
test stops proving something, so the bar for adding one is a specific grammar
difference — never "this made the number go up".

- **anonymous** — a capture matching a node with no identifier text. Neither
  backend can name it and RepoCortex drops it downstream.

That is the whole list. Two others were removed, and the story is worth keeping:

- **go-type-family** collapsed class, interface and `type_alias` into one kind
  for Go, because Go spells all three as `type X ...` and the two query sets
  split it by right-hand side. Defensible for the queries of the day — and it
  blindfolded the kind-agreement gate for exactly one language, which was the
  language where every type declaration was being indexed as an alias. No Go
  struct was ever a class and no Go interface was ever an interface. The gate
  whose stated purpose is "a class labelled an interface puts wrong edges in the
  graph" could not see the largest instance of that in the codebase.
- **java-unclassified** folded Java `function` into `method` to describe a
  de-duplication defect that `kindSpecificity` had since fixed. It outlived its
  reason and normalised nothing.

Both were verified inert before removal — kind agreement holds at 1.000 for
every language with each disabled. **A normalisation that currently hides
nothing still hides whatever arrives next.**

### This gate had never run

The `cgo_parser` build did not compile: `sortSymbols` was referenced by
`backend_cgo.go` and defined nowhere, so every command above failed at build
time. The escape hatch was unbuildable and the differential test — the stated
mitigation for depending on a young single-maintainer runtime — had never
executed once.

It is restored and green. If you are adding to this document, the lesson is that
a documented gate and an executed gate are different things, and only the
`cgo-backend.yml` CI job makes the difference durable.

## What the test does NOT prove

Read this part before trusting the number.

1. **It does not prove the two query-inference paths agree.** They do not. The
   CGO backend's tags queries are derived from each grammar's upstream
   `tags.scm`, extended to the same coverage *intent* as `queries.go`'s
   supplements. Matching the intent is what makes the comparison measure the two
   **runtimes** rather than two unrelated query authors — but it means a
   maintainer, not the test, decided what "equivalent" means.

2. **It does not prove agreement under malformed input.** Error recovery is
   where two tree-sitter implementations are most entitled to differ.
   `TestDifferentialMalformedInput` asserts only that both survive and neither
   emits a nonsensical position.

3. **It does not cover 202 languages.** Four grammars are compiled in. Nothing
   is known about the other 202 beyond what the pure-Go tests already assert.

4. **It does not measure memory, and memory is the main reason to switch.**
   Nobody has profiled the C backend on a large corpus. The claim that it fixes
   the arena problem is an expectation about a different implementation, not a
   measurement.

5. **It does not cover references or imports**, which the seam does not carry.

6. **The corpus is small** — the payment fixture plus a handful of generated
   files. It is designed for construct coverage, not statistical weight.

## Adding a language to the CGO backend

1. `go get github.com/tree-sitter/tree-sitter-<lang>`.
2. Add it to `cgoGrammars()` and give it a tags query in `cgoTagsQueries`,
   derived from that repository's `queries/tags.scm`. Note any departure and
   why, as the existing entries do.
3. Add a probe to `TestBothBackendsLoadAndParse` and corpus entries to
   `generatedCorpus` exercising the constructs that language argues about.
4. Run the differential test and read the disagreements. If recall is below the
   gate, the query is wrong — do not lower the gate.
