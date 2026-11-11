# Contributing to RepoCortex

Thanks for considering it. This document covers what you need to be productive.

## Getting set up

Requires **Go 1.26+**. Nothing else — no C compiler, no CGO.

```bash
git clone https://github.com/satyendrakumarsingh/repocortex && cd repocortex
go build ./... && go test -race ./...
```

If that works, you have a complete development environment.

## Before opening a PR

```bash
gofmt -l .          # must print nothing
go vet ./...
go test -race ./...
golangci-lint run
```

CI runs these on Linux, macOS and Windows. It also builds all five release
targets with `CGO_ENABLED=0`, because breaking that breaks `go install` for
everyone without a C toolchain.

## Architecture in one minute

```
acquire → parse → resolve → registry → graph → search → context → mcp
```

- `pkg/model` — shared vocabulary. No I/O, no parsing.
- `internal/store` — SQLite, migrations, connection policy.
- `internal/parse` — language parsers behind one interface.
- `internal/resolve` — turning references into edges, with confidence.
- `internal/context` — the context engine. The core of the product.
- `internal/capability` — what queries the graph. Never parses.

**Rule:** adding a language or analyzer must never require editing the context
engine. If your change does, the interface is wrong — raise it in the issue
first.

## The three invariants

These are not style preferences. Breaking them breaks the product's promise.

**1. Every fact carries provenance.** An `Edge` without `Confidence` and
`Evidence` is invalid, rejected by `model.Edge.Validate` and by a SQL `CHECK`.
Never introduce a code path that records a relationship without recording how
it was determined.

**2. `exact` means a compiler proved it.** `ConfidenceExact` requires
`SourceCompiler`. Enforced in Go *and* in SQL, deliberately — a bug in one
writer must not be able to turn a guess into a claim.

**3. Repository config cannot cause execution.** `.cortex.yml` is
attacker-controlled the moment someone clones an untrusted repo. It may
describe what to index; it may never influence what to run, what to download,
or whether secrets are redacted. See the package comment in
`internal/config/config.go`. `TestRepoCannotSetExecutionSettings` guards this.

## Testing expectations

Match the level of scrutiny to the risk:

- **Any bug fix** — add a test that fails before your change.
- **Parsers and anything reading untrusted input** — add a fuzz target. Hostile
  repositories are in the threat model. `FuzzSplitIdentifier` is the pattern to
  copy; it found a real Unicode bug within seconds of first running.
- **Anything touching the graph** — the invariant gate must still pass:
  `cortex doctor --verify-index`.
- **Anything security-relevant** — write the test that proves the attack
  *fails*, not just that the happy path works.

A gate that has only ever been seen passing proves nothing. Where practical,
prove your check can fail by injecting the defect it's meant to catch — see
`TestDoctorDetectsDanglingEdges`.

## Adding language support

The highest-value contribution, and deliberately well-bounded.

| Tier | What it delivers | Where |
|---|---|---|
| 1 | Symbols, imports, outline | free — the generic path already covers 206 grammars |
| 2 | Cross-file resolution + confidence | `internal/parse/lang/<lang>/` |
| 3 | Framework awareness (routes, ORM) | same package |

For Tier 2, implement resolution and report measured precision against a SCIP
index for the same fixture. **Do not claim accuracy you have not measured** — a
published number that turns out to be optimistic costs more trust than a modest
one that holds.

## Things we will push back on

- New dependencies without a clear justification, especially ones requiring CGO.
- Returning whole files through MCP. Bounded ranges only — the point of this
  product is spending less agent context, not more.
- Presenting inference as fact anywhere in output.
- Benchmark numbers not produced by an actual run.
- Suppressing a failing test or linter rather than fixing the cause.

## Commit and PR style

Explain *why* in the body, not just what. If you fixed a bug, say what the
failure mode was — the next person to touch that code needs to know what it was
protecting against.
