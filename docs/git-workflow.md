# Git workflow

## Branching

`main` is the only long-lived branch. Work happens on a topic branch and lands
through a pull request; CI gates the merge.

There is no release branch and no version tag yet — `repocortex version` reports
`dev`/`none`/`unknown` from a source build, and real values are injected with
`-ldflags` at release time. Nothing in the repository depends on branch naming.

## Commit messages

The convention here is stronger than most, and it is stated in `CONTRIBUTING.md`:

> Explain *why* in the body, not just what. If you fixed a bug, say what the
> failure mode was — the next person to touch that code needs to know what it was
> protecting against.

Read `git log --format='%H %s%n%b'` before writing your first one. The existing
messages are long, and they are long for a reason: they are the only place the
reasoning behind a measurement-driven decision survives. A diff shows that
`defaultWorkers` returns 4. Only the commit message carries the table that shows
10 workers bought 10 seconds and cost 4 GB.

### Structure as practised

**Subject** — imperative, no type prefix, no scope, no ticket number. It says what
the change accomplishes, not which files moved:

```
Fix scale, add observability, error taxonomy, and soak testing
Add the filesystem watcher backing `repocortex watch`
README: state what actually works, and what does not
M2: resolve references to definitions, building the call graph
```

A `Component:` prefix appears where it genuinely disambiguates (`README:`), and a
milestone prefix where the commit closes one (`M2:`). Neither is a required
format.

**Body** — the substance. What the existing messages actually do:

- **Open with the problem, not the solution.** *"RepoCortex could extract 531,290
  references and bind none of them."* *"Indexing a 10,920-file tree peaked at 6.5
  GB."* The reader learns why the commit exists before they learn what it did.
- **Name the failure mode.** Not "fixed pragma handling" but: SQLite pragmas are
  per-connection, `db.ExecContext("PRAGMA foreign_keys = ON")` configures one
  connection out of eight, `ON DELETE CASCADE` silently did not fire, and the
  verification query passed because it reused the connection it had just
  configured.
- **Include the measurement that decided it.** Tables of workers against peak
  heap, parse caps against peak heap, before-and-after totals. `CONTRIBUTING.md`
  lists "benchmark numbers not produced by an actual run" as grounds for
  pushback, and the commit body is where the run is recorded.
- **Record the rejected alternative and why.** *"Binding to an arbitrary one is a
  coin flip presented as a fact; binding to all of them floods the graph."*
  *"Pairing requires a matching base name... Anything else degrades to
  delete+create — which the index handles correctly anyway, so a wrong guess
  would be worse than no guess."*
- **Say what is still missing.** The initial import ends with a "Not yet
  implemented" list. Overstating readiness is how a project loses the trust it
  needs most in its first weeks.

**Trailers** — none. A commit carries its author and nothing else: do not add a
`Co-Authored-By` line or any other attribution for an AI assistant or a tool,
in commit messages, tag messages or pull-request text. To check a branch before
pushing, `git log --format='%an | %cn%n%(trailers)'` should show only its
author.

### Worked shape

A constructed model, not a quotation — the numbers in it are real, taken from
the measurements in [performance.md](performance.md), but no commit has exactly
this subject. For real ones, read `git log`; `Fix scale, add observability, error
taxonomy, and soak testing` is the closest single example of everything above
done at once.

```
Bound in-flight bytes so peak memory stops depending on file ordering

A worker count limits concurrency but not memory. Eight workers that each
happen to pick up a megabyte of generated code will allocate gigabytes
between them, and the scheduler decides when that happens — which makes
peak memory a property of file ordering rather than of the repository.

Workers now acquire budget proportional to the bytes they are about to
parse, before reading the file. Small files run wide; a large file waits
and then effectively runs alone. A file larger than the whole budget is
admitted alone rather than deadlocking, because a file that can never be
indexed is worse than a brief spike.

Throughput on ordinary code is unchanged, because ordinary files are
small: 54s before, 54s after on the 10,920-file tree, at 1,595 MB rather
than 6,531 MB peak.
```

## Before you commit

```bash
gofmt -l .          # must print nothing
go vet ./...
go test -race ./...
golangci-lint run
```

**If you touched a file behind a build tag, `go vet ./...` did not compile it.**
CI vets the tagged builds separately, and so should you:

```bash
go vet -tags soak ./tests/soak/...
go vet -tags perf ./internal/context/...
```

`cgo_parser` is the third tag; it needs a C toolchain and is vetted in the
separate `cgo-backend.yml` workflow. The failure mode this prevents is a tagged
file that stopped compiling weeks ago and whose only symptom is a nightly job
failing long after the commit that broke it.

And, when the change touches the graph, the parser, the registry or the walker:

```bash
repocortex doctor --verify-index
```

Run it against a **copy** of the repository, not the working tree — every
RepoCortex command writes `.repocortex/` into the directory it is pointed at.

CI enforces all of the above on Linux, macOS and Windows, plus a `CGO_ENABLED=0`
build of all five release targets, plus `go mod tidy` producing no diff, plus
`govulncheck`. Running them locally first is faster than a CI round trip; running
them locally is not a substitute for CI, because the platform matrix is where the
FTS5 guarantee actually gets tested.

If you fixed a bug, the commit must also contain a test that **fails before your
change**. See [development.md](development.md#prove-your-check-can-fail).

## Shipped migrations are immutable

This is the one rule in the repository that is enforced at runtime, on a user's
machine, not just in CI.

A migration that has been applied is recorded in `schema_migrations` with a
sha256 of its SQL. On every subsequent `Migrate`, previously-applied migrations
are checksum-verified against the embedded file. A mismatch is a **hard error**,
not a warning:

```
error: apply schema: migration 0001_initial was modified after being applied
(recorded a3f9c2b41e7d, embedded 8c1e05fa9b32); shipped migrations are immutable
— add a new migration instead
```

The reason is that a silently-edited migration means two installs of the same
version have different schemas. One user's index has a column, another's does
not, and every bug report from that point on is unreproducible. A warning would
be worse than nothing, because it would be ignored.

`TestMigrateDetectsEditedMigration` enforces it in the test suite, alongside
`TestMigrateFreshInstall`, `TestMigrateIsIdempotent` and `TestMigrateInterrupted`
— fresh, upgrade, re-run and interrupted, which are the four states a migration
runner has to survive.

**So: never edit a file under `internal/store/migrations/` once it has shipped.**
Add `NNNN_name.sql` with the next version number instead. Filenames must match
`NNNN_name.sql`, versions must be unique, and they are applied in ascending
order, each in its own transaction so an interrupted run resumes from the last
fully-applied version.

Four have shipped — `0001_initial`, `0002_references`, `0003_data_access`,
`0004_config_usage` — so `repocortex init` reports `Schema version: 4`, and the
next one is `0005_`. The last three are the same pattern applied three times:
extract a fact during indexing, stage it, bind it in a second pass. If your
change is a fourth instance of that, read 0003's header comment before writing
it; the reasoning is already there.

Editing an *unshipped* migration — one you added on your own branch that has not
been merged — is fine, and is the normal way to iterate. The moment it lands on
`main`, it is frozen.

The same logic applies to two things that are not migrations but behave like
schema:

- **Skip reason strings** (`too_large`, `binary`, `symlink`,
  `symlink_escapes_root`, `irregular`, `unreadable`). They are written into the
  index and read back out. `walk.go` says so explicitly: "These are stable
  identifiers, not prose... changing one is a schema change."
- **The digest version** (`repocortex/registry-digest/v1`). It is part of the
  hashed input, so changing what the digest covers changes every digest — an old
  value can never silently compare equal under a new scheme. Bump it whenever the
  field set changes.

## Pull requests

Same standard as commit messages: explain why, name the failure mode, include the
measurement. If the PR is a single commit, the commit body *is* the PR body.

State what you did **not** do. The most useful sentence in a PR is often the one
that says which case is still unhandled and why it was out of scope.
