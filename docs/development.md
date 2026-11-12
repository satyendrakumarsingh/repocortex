# Development

## Getting set up

**Go 1.26+. Nothing else.** No C compiler, no CGO, no system SQLite.

```bash
git clone https://github.com/satyendrakumarsingh/repocortex.git && cd repocortex
go build ./... && go test -race ./...
```

If that works you have a complete development environment.

The zero-CGO constraint is not a stylistic preference — it is a promise that `go
install` works on every platform and that contributors need no toolchain beyond
Go. It shapes the dependency list:

| Dependency | Role | Why this one |
|---|---|---|
| `modernc.org/sqlite` | Storage | Pure-Go SQLite. FTS5 is enabled via a codegen flag but **undocumented**, so a guarantee test pins it per platform |
| `github.com/odvcencio/gotreesitter` | Parsing | 206 grammars, no C runtime |
| `github.com/go-git/go-git/v5` | History | In-process git; RepoCortex never shells out to a `git` binary |
| `github.com/scip-code/scip/bindings/go/scip` | Compiler-backed resolution | SCIP index ingestion |
| `google.golang.org/protobuf` | SCIP wire format | SCIP indexes are protobuf; pulled in by the above |
| `github.com/modelcontextprotocol/go-sdk` | MCP | The protocol `repocortex serve` speaks |
| `github.com/fsnotify/fsnotify` | Watching | |
| `github.com/spf13/cobra` | CLI | |
| `gopkg.in/yaml.v3` | Config | Needed for `dec.KnownFields(true)`, which is what makes an unknown key in repo config a parse error |
| `go.opentelemetry.io/otel` | Observability | Off by default |

`go.mod` also requires `github.com/tree-sitter/go-tree-sitter` and the Go, Java,
Python and TypeScript grammars. Those are **not** the default parsing path —
they back the `cgo_parser` build tag, which needs a C toolchain and is
deliberately excluded from the default matrix. `go.mod` is the authoritative
list; the table above names only the dependencies whose choice needed a reason.

CI proves the promise rather than asserting it: a dedicated job builds all five
release targets (`darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`,
`windows/amd64`) from one runner with `CGO_ENABLED=0`, and runs the full test
suite with CGO disabled.

New dependencies get pushback, especially ones requiring CGO. `internal/acquire/fs`
implements `.gitignore` semantics in about 250 lines rather than taking a
dependency, and the file explains exactly why — the candidate library models a
flat pattern list, while correct semantics require every rule to remember the
directory of the file it came from.

## Before opening a PR

```bash
gofmt -l .          # must print nothing
go vet ./...
go vet -tags soak ./tests/soak/...          # go vet ./... cannot see these
go vet -tags perf ./internal/context/...
go test -race ./...
golangci-lint run
```

Two things about how CI splits this, because "CI is green" means different things
for different lines above:

- **`gofmt`, `go vet`, the tagged vets, `golangci-lint` and `go mod tidy` run on
  Linux only**, in a single `lint` job.
- **`go test -race ./...` runs on Linux, macOS and Windows.** That matrix is not
  ceremony: the FTS5 guarantee depends on a per-platform codegen flag, and the
  symlink-containment tests exercise genuinely different code paths per OS.

The enabled linters and the reason each is on are in `.golangci.yml`. Two worth
knowing: `errcheck`, because unchecked errors hide failures in an indexer, and
`errorlint`, because `%w` wrapping is what lets callers use `errors.Is`/`As`
instead of matching on strings.

`gosec`'s `G204` (subprocess execution) is excluded globally, because subprocess
execution is a core feature rather than an accident: `internal/indexer` runs
third-party SCIP indexers, which run build systems. It is governed by the trust
model in `internal/config` — a repository can never name what gets executed — and
that is enforced by `TestRepoCannotSetExecutionSettings` and the tests in
`internal/indexer`, not by suppressing the linter at every call site.

## The test tiers

Five tiers, each answering a different question. Match the level of scrutiny to
the risk. Three of them are hidden from `go test ./...` by a build tag, which is
the point — and also the hazard, so read "Tagged code rots silently" below before
adding to any of them.

### Tier 1 — unit and integration, with the race detector

```bash
go test -race ./...
```

Runs on every commit, on three operating systems. This is the tier that must stay
fast, so everything slow is deliberately kept out of it.

The race detector is not optional here. Indexing is concurrent by construction —
a bounded worker pool over a concurrent directory walk, writing into a serialised
sink — and a data race in that machinery produces a nondeterministic index rather
than a crash, which is the hardest kind of bug to attribute later.

The FTS5 guarantee test lives in `tests/integration/` and is also run by name on
every platform:

```bash
go test -race -v -run 'TestFTS5' ./tests/integration/...
```

It exists because `modernc.org/sqlite` enables FTS5 through an undocumented
codegen flag whose settings are per-platform. Verifying only on Linux would leave
Windows users to discover a broken search layer themselves.

### Tier 2 — CGO-free build matrix

```bash
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /dev/null ./cmd/repocortex
```

Breaking this breaks `go install` for everyone without a C toolchain, and breaks
cross-compilation. Note that this job runs the tests **without** `-race`, because
the race detector requires CGO.

### Tier 3 — soak

Endurance tests: the failure modes that only appear after a process has been
running for a long time. A watcher that leaks one goroutine per deleted directory
passes every unit test in the repository and dies on a developer's machine after
an afternoon of branch switching.

Every soak file carries the `soak` build tag, so the suite is invisible to `go
test ./...`. That is deliberate: a slow test in the default path is a test people
learn to skip, and a skipped test protects nothing.

```bash
# Short run, before a commit
go test -tags soak -race -v ./tests/soak/...

# A real run
go test -tags soak -timeout 2h ./tests/soak/... -soak.duration=1h
```

`-soak.duration` is the budget for **each** test, not the suite, so wall time is
roughly the duration times the number of tests (currently four). Set `-timeout`
accordingly — Go's default 10-minute test timeout will kill anything longer.

Flags, each with a `REPOCORTEX_SOAK_*` environment equivalent so CI can configure a
run without rewriting the command line:

| Flag | Environment variable | Default |
|---|---|---|
| `-soak.duration` | `REPOCORTEX_SOAK_DURATION` | 30s |
| `-soak.files` | `REPOCORTEX_SOAK_FILES` | 200 |
| `-soak.interval` | `REPOCORTEX_SOAK_INTERVAL` | 3ms |
| `-soak.seed` | `REPOCORTEX_SOAK_SEED` | 1 |

Run with `-v`. Every test logs a resource report — sampled goroutine counts, heap
trend, iterations, events — because **a soak test whose only output is PASS
teaches nothing** about how close the system is to the cliff.

What the four cover:

| Test | Covers |
|---|---|
| `TestWatcherEndurance` | Sustained churn through the watcher, convergence checked after quiesce |
| `TestWatcherDirectoryChurn` | Repeated directory create/delete; asserts watch descriptors are released |
| `TestIndexQueryConcurrencySoak` | Continuous incremental indexing under four concurrent readers |
| `TestRepeatedFullIndexStability` | The same tree indexed N times; digest and resource footprint compared |

House rules for anything added to `tests/soak`:

1. Never touch the real repository. Everything runs against a synthetic tree
   under `t.TempDir()`.
2. Never call `t.Parallel()`. Goroutine counts are process-wide, so a second test
   running alongside makes the leak check meaningless.
3. Sample only after settling. Go's GC is lazy and a returned goroutine is still
   counted for a moment; an unsettled sample produces exactly the flaky failure
   that gets a soak suite deleted.

This suite has already earned its keep. It is what found that foreign keys were
off on 97% of connections — a 30-second concurrent soak produced 1,010 dangling
symbols and 505 orphaned FTS rows.

### Tier 4 — fuzzing

Nightly and on demand, never blocking a PR.

```bash
go test -fuzz FuzzSplitIdentifier -fuzztime 5m ./internal/search/
go test -fuzz FuzzIgnoreMatcher   -fuzztime 5m ./internal/acquire/fs/
go test -fuzz FuzzParseGo         -fuzztime 5m ./internal/parse/
go test -fuzz FuzzIngest          -fuzztime 5m ./internal/resolve/scip/
go test -fuzz FuzzRedact          -fuzztime 5m ./internal/security/
```

**No count is given here on purpose.** The target list grows faster than this
page does, so get it from the tree rather than from a number someone wrote down:

```bash
grep -rn 'func Fuzz' --include='*.go' .
```

Two mechanics that catch people out:

- `-fuzz` takes **one target at a time**. Go's fuzzer does not run several in one
  invocation, and the argument is a regexp, so `-fuzz FuzzParse` matches more
  than one target in a package that has several.
- Target names are **not unique across packages** — `FuzzParse` exists in
  `internal/analyze/db`, `internal/analyze/deps` and `internal/trace`. Always
  pass the package path.

**Only `FuzzSplitIdentifier` runs in CI** (nightly, `-fuzztime 5m`). Every other
target exists and passes on demand, but nothing schedules it. Treat "the fuzz
suite is green" as a claim about one target unless you ran the rest yourself.

Anything reading untrusted input should have a target. Hostile repositories are
in the threat model, and `FuzzSplitIdentifier` is the pattern to copy: it found a
real Unicode bug (U+0130 is two bytes but folds to the single character "i", so a
byte-length check let noise tokens into the search index) within seconds of first
running. The failing input is committed as a seed, which is what stops the bug
coming back silently.

Corpus entries live under `testdata/` and are committed. Treat a new seed as part
of the fix, not as an artefact.

### Tier 5 — latency gates

Nightly and on demand, behind the `perf` build tag.

```bash
go test -tags perf -timeout 25m -v -run 'TestP95|TestCatalogueCacheRemoves' ./internal/context/
```

A wall-clock threshold does not belong on the push path. On a shared runner it
eventually fails for reasons unrelated to the commit, and a gate that cries wolf
gets switched off. Building the 94,000-symbol registry these tests need also took
`internal/context` from 26 s to 578 s under `-race`, which is a cost every
unrelated pull request would otherwise have paid.

**As of this writing the package does not build under the tag.** `go vet -tags
perf ./internal/context/...` fails with `latency_test.go:127:2: declared and not
used: testCache`, which means the CI `lint` job's "vet the tagged builds" step is
red. Do not read "the latency gate passes" into a green default matrix — the
default matrix cannot see this file. See
[performance.md](performance.md#context-assembly-at-scale) for what happens when
it is patched enough to run.

### Tagged code rots silently

`soak`, `perf`, `cgo_parser` and `repocortex_nootlp` each hide code from every
default `go build` and `go test`. That is what makes the tiers work, and it is
also how a tagged file stops compiling for weeks without anyone noticing — the
first symptom is a nightly job failing at 3am, days after the commit that broke
it.

CI compensates by vetting the tagged builds explicitly:

```bash
go vet -tags soak ./tests/soak/...
go vet -tags perf ./internal/context/...
```

`cgo_parser` needs a C toolchain and is vetted in the separate
`cgo-backend.yml` workflow instead. **Run the two above locally before pushing a
change to any tagged file** — `go vet ./...` will not tell you.

### A note on `tests/`

`tests/architecture`, `tests/integration` and `tests/soak` hold real suites.
`tests/bench`, `tests/e2e`, `tests/golden` and `tests/unit` are currently empty
directories. They mark intended homes; do not read them as coverage that exists.

`tests/architecture/layering_test.go` is worth knowing about specifically,
because it is `CLAUDE.md` §1 and §4.2 executed rather than audited: the domain
core's purity, the Repository adapter's import direction, confinement of the SQL
driver, and the absence of the process-wide cache that violated §4.2. Every
assertion names the clause it enforces, so a failure points at the rule rather
than at a package list.

```bash
go test ./tests/architecture/
```

## The correctness gates

"Error-free" is not shippable. Defects made loud and early is.

| Gate | Catches | How to run |
|---|---|---|
| Race detector on every test | Concurrency bugs in the worker pool and the walker | `go test -race ./...` |
| **Determinism** — index twice, compare digest | Map-iteration order, races, ordering bugs anywhere in the pipeline | `repocortex index --full --digest` twice |
| **Incremental == full re-index** | Incremental updates drifting from ground truth | `TestIncrementalMatchesFullReindex` |
| FTS5 dependency guarantee | `modernc.org/sqlite` silently dropping an undocumented feature | `go test -run TestFTS5 ./tests/integration/...` |
| Graph invariants | Dangling edges, orphans, cross-run edges, unproven `exact` claims | `repocortex doctor --verify-index` |
| Migration tests | Fresh / upgrade / interrupted / edited-migration | `go test ./internal/store/...` |
| Config security tests | Repo-supplied config gaining execution | `go test -run TestRepoCannot ./internal/config/... ./internal/indexer/...` |
| Layering | `CLAUDE.md` §1 and §4.2 drifting from the code | `go test ./tests/architecture/` |
| Redaction and path containment | Secrets or escaped paths reaching the index | `go test ./internal/security/...` |
| Fuzzing | Panics and hangs on hostile input | See above |
| CGO-free build matrix | Breaking `go install` for users without a C compiler | `CGO_ENABLED=0 go build ./...` |
| `govulncheck` | Known vulnerabilities in dependencies | CI |

### The determinism gate, in detail

It is the load-bearing one, so it is worth understanding what it actually proves.

Indexing the same tree twice must produce a byte-identical registry digest. If it
does not, something in the pipeline depends on goroutine scheduling, map
iteration order, or filesystem walk order — a class of bug that is close to
undebuggable once it reaches a user.

The digest is designed to make that cheap to detect: rows are read under an
explicit total `ORDER BY` over content columns, autoincrement ids are excluded
outright (edges reference their endpoints by content identity instead), and
timestamps are excluded because `mod_time_unix` differs after a fresh clone of
the same commit.

Crucially, it is verified **both ways**:

- **Stable** across 1, 2, 4 and 8 workers.
- **Sensitive** to a single added symbol.

A digest that never changed would pass the stability half while detecting nothing
at all. Checking only stability is the most natural way to build a gate that
looks green forever and protects nothing.

### The incremental gate

The digest after add / modify / delete / rename must equal the digest of a full
re-index of the same tree. Without this, incremental updates can drift from
ground truth arbitrarily far, and the drift is invisible until someone asks a
question whose answer depends on stale rows.

### Prove your check can fail

This is the principle the whole gate list rests on, from `CONTRIBUTING.md`:

> A gate that has only ever been seen passing proves nothing. Where practical,
> prove your check can fail by injecting the defect it's meant to catch — see
> `TestDoctorDetectsDanglingEdges`.

Every gate above has been observed failing on an injected defect, not just
passing. `TestDoctorDetectsDanglingEdges` is the pattern to copy: it deliberately
writes a dangling edge and asserts the invariant check reports it. CI runs it as
a named security gate precisely because it is the test that keeps the other
tests honest.

Applied to the smaller cases:

- **Any bug fix** — add a test that fails before your change. If you cannot make
  it fail, you have not found the bug.
- **Parsers and anything reading untrusted input** — add a fuzz target.
- **Anything touching the graph** — `repocortex doctor --verify-index` must still
  pass.
- **Anything security-relevant** — write the test that proves the attack
  *fails*, not just that the happy path works.

## Package boundaries

```
pkg/model             shared vocabulary. Imports fmt and nothing else.
internal/store        SQLite, migrations, connection policy. The ONLY package
                      that names a database driver.
internal/acquire/fs   discovery, gitignore, hashing.
internal/acquire/git  history, changed paths, recency.
internal/parse        language parsers behind one interface. No database.
internal/analyze      endpoints, SQL access, config reads, dependency
                      manifests. Pure, like parse. Subpackages: api, db,
                      config, deps.
internal/pipeline     concurrency, memory budget, the Sink contract.
internal/registry     the Repository adapter: index runs, atomic swap, digest,
                      invariant gate. Owns SQL; returns pkg/model.
internal/resolve      infer (heuristic), scip + scipimport (compiler-backed).
internal/indexer      detecting, provisioning and running external SCIP
                      indexers. Trust-sensitive: see internal/config.
internal/graph        traversal over the persisted graph.
internal/search       identifier tokenisation for FTS5. Imports no other
                      RepoCortex package.
internal/context      retrieve, rank, budget -> one bounded context pack.
internal/capability   query layers over the graph: impact, tests,
                      architecture, runtime. None parses, none writes.
internal/arch         the declared component model and ADR discovery.
internal/compile      task contracts. No model call, no network, no clock.
internal/bench        retrieval-quality scoring against hand-written cases.
internal/trace        reads OTel/Jaeger exports. Correlates nothing itself.
internal/security     secret redaction and path containment.
internal/watch        filesystem events, debounce, reconcile.
internal/mcp          the MCP server.
internal/config       the two-file trust split.
internal/observe      logging, spans, metrics, attribute redaction.
internal/errs         error taxonomy.
internal/cli          command assembly. Owns no domain logic.
```

**The rule:** adding a language or an analyzer must never require editing the
context engine. If your change does, the interface is wrong — raise it in the
issue before writing code.

None of these is a placeholder. An earlier revision of this file said
`internal/capability`, `internal/context`, `internal/indexer` and
`internal/security` were empty directories; all four ship, and all four are
reachable from the CLI.

**Two import rules are enforced by `tests/architecture/layering_test.go` rather
than by review**, so breaking one fails a test rather than surviving to `main`:

- `pkg/model` imports nothing outside the standard library, and within that only
  `fmt`. Anything it picks up becomes a dependency of every consumer of
  `model.Edge` and `model.Symbol`, which is nearly the whole product.
- The arrow between `internal/registry` and the analysis layer points one way.
  The concrete violation this catches actually happened: `internal/registry`
  imported `internal/analyze/deps` to decide whether an import path used a
  declared dependency. It compiled and it worked, and it gave the storage layer
  an opinion about package managers. The fix was a caller-supplied
  `registry.ImportMatcher`.

See `CLAUDE.md` §1 and [architecture.md](architecture.md) for the statement those
tests enforce, and the maintainers' compliance record for what was fixed to make them
pass.

## Adding language support

The highest-value contribution, and deliberately well-bounded.

| Tier | Delivers | Where |
|---|---|---|
| 1 | Symbols, imports, outline | Free — the generic path already covers 206 grammars |
| 1b | Recovering symbols the inferred tags query drops | `internal/parse/queries.go` |
| 2 | Cross-file resolution + confidence | `internal/resolve/infer` — but see below |
| 3 | Framework awareness (routes, ORM, config keys) | `internal/analyze/{api,db,config,deps}` |

There is **no `internal/parse/lang/<lang>/` directory**, and an earlier revision
of this file sent contributors looking for one. Language-specific knowledge lives
in two places: tags-query supplements in `internal/parse/queries.go`, and
framework recognition in the `internal/analyze` subpackages. That split is what
lets a framework recogniser be added without touching a parser, and a parser
without touching the context engine.

**Tier 2 has no per-language extension point today, and that is a real gap rather
than an omission from this page.** `internal/resolve/infer` is one
language-agnostic resolver: its tiers are same-file, receiver-type, same-package,
imported-package and unique-exported-name, and it contains no branch on language.
That generality is why it works at all across 206 grammars, and it is also why
its ceiling is `high` rather than `exact`. If your language needs resolution
rules the generic tiers cannot express, raise it before writing code — the answer
may be a compiler-backed indexer (`internal/indexer`) rather than a resolver
change.

Tier 1 is not always complete out of the box, and that is worth knowing before
you start. `gotreesitter` ships zero explicit tags queries — all 206 are
inferred, and coverage is uneven. The Go query silently dropped type, const and
var declarations; the additive supplements in `internal/parse/queries.go` recover
them, and the file documents the capture convention. Checking what your
language's inferred query actually captures is usually the first useful hour.

For Tier 2, implement resolution and **report measured precision against a SCIP
index for the same fixture**. That is now a cheap thing to do rather than an
aspiration: `repocortex indexers list` tells you whether an indexer for your
language is on `PATH`, and `repocortex scip import` gives you compiler-proven edges
to compare against. Do not claim accuracy you have not measured — a published
number that turns out to be optimistic costs more trust than a modest one that
holds.

## Things that will get pushback

From `CONTRIBUTING.md`, and they are all consequences of positions taken
elsewhere in these docs:

- New dependencies without clear justification, especially ones requiring CGO.
- Returning whole files through MCP. Bounded ranges only — the point of this
  product is spending less agent context, not more.
- Presenting inference as fact anywhere in output.
- Benchmark numbers not produced by an actual run.
- Suppressing a failing test or linter rather than fixing the cause.

## Useful commands

```bash
# Build a binary to test against
CGO_ENABLED=0 go build -o /tmp/repocortex ./cmd/repocortex

# Index a fixture end to end
cd fixtures/payment-platform && /tmp/repocortex init && /tmp/repocortex index --digest

# Determinism by hand: these two digests must match
/tmp/repocortex index --full --digest
/tmp/repocortex index --full --digest

# Vet what the default build cannot see
go vet -tags soak ./tests/soak/...
go vet -tags perf ./internal/context/...

# Package documentation
go doc ./internal/config
go doc ./internal/registry Registry

# Benchmarks (the observe suite asserts zero allocations on the disabled path)
go test -run '^$' -bench . -benchmem ./internal/observe/
go test -run '^$' -bench . -benchmem ./internal/registry/
go test -run '^$' -bench . -benchmem ./internal/resolve/infer/
go test -run '^$' -bench . -benchtime 1x ./internal/acquire/git/
```

**Work against a copy, not the repository.** Indexing writes `.repocortex/` into
whatever directory you point at, and the soak suite's first house rule exists for
the same reason. Copying the tree to a temp directory first also means
`repocortex index --full` runs against a fresh database, which matters more than it
sounds — see
[performance.md](performance.md#a-full-re-index-is-not-priced-like-a-first-index).
