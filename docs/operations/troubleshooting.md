# Troubleshooting

Symptom, cause, fix. Every message quoted here is either a literal from the
source or a transcript of an actual run; the CLI prints all command errors to
stderr prefixed with `error: ` and exits 1.

A few entries below describe behaviour that has since been **fixed**. They are
kept, marked as such, rather than deleted — a reader who hit the old behaviour on
an older binary, or who remembers the warning, needs to know it no longer applies
and why. An entry with no such note describes current behaviour.

---

## Getting started

### `error: no index at /path/.repocortex/index.db — run 'repocortex init' first`

**Cause.** `index`, `resolve` and `watch` all require the database file to exist.
`init` creates it. `serve` reports the same condition with a slightly longer
message: `no index at ... — run 'repocortex init' and 'repocortex index' first`.

**Fix.** `repocortex init`. If you expected an index to be there, check the path in
the message — you are probably in a subdirectory, or the wrong repository. Use
`--project /path/to/repo` to be explicit.

### `error: index already exists at /path/.repocortex/index.db (use --force to recreate)`

**Cause.** `init` refuses to overwrite. It is not idempotent, deliberately: the
index is expensive to rebuild.

**Fix.** Nothing, usually — run `repocortex index` instead. `repocortex init --force`
deletes the database and starts over, losing everything indexed.

### `error: no committed index — run 'repocortex index' first`

Also seen as `no committed index revision — run 'repocortex index' first` (watch)
and `no committed index for this project — run 'repocortex index' first` (MCP).

**Cause.** The database exists and is migrated, but no index run has ever been
committed. `repocortex status` shows `Index: EMPTY`.

**Fix.** `repocortex index`.

**Caveat worth knowing.** `resolve` and `watch` discard the underlying error at
this point, so a genuine database failure — locked, corrupt, permissions — is
reported with this same message and will send you down the wrong path. If
`repocortex index` also fails, read *that* error: it does not swallow the cause.

### `error: project root "/some/path" is not a directory`

**Cause.** `--project` points at a file, or at nothing.

**Fix.** Point it at a directory. Three distinct messages come from the same
check in `internal/cli/root.go`, and they mean different things:

| Message | Means |
|---|---|
| `project root "/x" is not a directory` | The path exists and is a file |
| `project root "/x": stat /x: no such file or directory` | The path does not exist |
| `resolve project root "x": ...` | The path could not be made absolute — usually `os.Getwd()` failing under a deleted working directory |

---

## Configuration

### `error: repo config: parse .repocortex.yml: yaml: unmarshal errors: line N: field X not found in type config.RepoConfig`

**Cause.** An unrecognised key in `.repocortex.yml`. Repository config is decoded
with `KnownFields(true)`, so an unknown key is a parse error rather than a silent
no-op.

This is intentional and it is a security control. If the key you tried is
`accuracy`, `indexers`, `provision` or `runner`, you have hit the trust boundary:
repository config structurally cannot express execution settings. See
[security.md](security.md#repository-config-cannot-grant-execution).

**Fix.** For a typo, correct it against the reference in
[user-manual.md](user-manual.md#repocortexyml--every-key-that-exists). For an
execution setting, move it to `~/.config/repocortex/config.yml`, which is yours.

### `error: user config /path/config.yml: parse: yaml: ...`

**Cause.** Malformed YAML in the user config. Unlike repository config, unknown
keys here are ignored — only syntax errors surface.

**Fix.** Fix the YAML.

### `error: accuracy.mode "x": want fast, auto or exact`

Plus the sibling validation errors:

```
accuracy.indexers.provision "x": want never, prompt or auto
accuracy.indexers.runner "x": want native or docker
mcp.profile "x": want minimal, standard or full
context.max_graph_depth 50: want 1..10 (unbounded traversal is a DoS on large repos)
context.max_files 0: must be >= 1
```

**Cause.** A resolved-config value outside its permitted set. These run after
both files are merged, so the message does not say which file it came from.

**Fix.** `repocortex doctor` reports which config files were found, including the
user config path. Check the trusted one first — it is applied before the
repository's.

### My `.repocortex.yml` exclusion did not remove `.git` / `node_modules`

**Cause.** Working as designed. Repository excludes are **appended** to the
defaults, never substituted, and negated patterns (`!...`) in an exclude list are
discarded outright.

**Fix.** None available, and that is the point — a hostile repository must not be
able to make RepoCortex index the object store.

### `field security not found in type config.RepoConfig`

**Cause.** There is no `security:` key, in either configuration file. It used to
exist, was read by nothing, and has been removed — so writing it is now a parse
error rather than a setting that is silently discarded.

That is deliberate, and the absence is the control. Redaction runs
unconditionally in `pipeline.RegistrySink` before any file text reaches the
search index. A repository you cloned cannot express an opinion about redacting
its own secrets in either direction, which is stronger than the old rule that let
it turn redaction on but never off: there is no longer a field to argue with.

**Fix.** Delete the `security:` block. If you need a secrets-bearing file kept
out of the index entirely rather than redacted, exclude it in `index.exclude` —
an excluded file is never read.

Verified: a repository containing `AKIAIOSFODNN7EXAMPLE` in a `.go` file and a
`.env` holding credentials indexes with no configuration set, and neither value
is findable:

```
$ repocortex search AKIAIOSFODNN7EXAMPLE
No files match "AKIAIOSFODNN7EXAMPLE".
```

(A control search for an ordinary identifier in the same file does match, so the
empty result is redaction rather than a broken index.)

---

## Indexing

### `error: indexing cancelled; previous revision left in place`

**Cause.** You interrupted `repocortex index --full`. The staging run is discarded
rather than published; a partial index that looks complete is worse than no new
index.

**Fix.** Re-run. The previous revision is untouched and still queryable. The
abandoned run's rows are cleaned up automatically at the start of the next
`repocortex index` (`ReclaimAbandoned`), and you will see
`level=WARN msg="reclaimed abandoned index runs" runs=[2]`.

### `--workers` or `--accuracy` seems to be ignored

**Cause.** Both flags take effect only on the **full** index path. Without
`--full`, `repocortex index` takes the incremental path and neither is applied.

They *are* validated on both paths, so a typo fails rather than being silently
discarded:

```
$ repocortex index --accuracy nonsense
error: --accuracy "nonsense": want fast, auto or exact          # exit 1

$ repocortex index --workers -1
error: --workers -1: must be zero (use the default) or positive
```

**Fix.** Add `--full` if you need the flag to do something. Nothing warns you
that a valid-but-inapplicable flag was ignored, which is the remaining gap.

**Two older symptoms that no longer occur**, noted because earlier revisions of
these docs described them and a reader may be looking for them:

- `repocortex index --accuracy nonsense` on an up-to-date index used to exit 0
  having silently discarded the flag. Validation now runs before the path is
  chosen — `internal/cli/index.go` says why: "a typo in CI would have looked like
  success."
- The `--workers` help text used to say "default: CPU count" while the real
  default was 4. It now reads `parse concurrency (default 4; peak memory scales
  with this, not with repository size)`.

### Files are missing from the index

Check `repocortex status` for the skipped count, then query the `files` table for
per-path reasons. The skip reasons and what each means:

| `skip_reason` | Cause | Fix |
|---|---|---|
| `too_large` | Over 2 MiB, or grew past it during hashing | Exclude it, or accept the gap |
| `binary` | NUL byte in the first 8000 bytes (git's own heuristic) | Expected |
| `symlink` | Symlinks are never followed, even inside the root | Expected; index the target directly |
| `symlink_escapes_root` | The link resolves outside the repository | Expected, and deliberate — see [security.md](security.md#symlinks-are-never-followed) |
| `irregular` | Socket, device or FIFO | Expected. Opening a FIFO would hang the indexer |
| `unreadable` | Permission denied, broken link, unreadable directory | Fix permissions |

Two more that appear as `parse_error` rather than a skip reason:

- `no grammar for this file type` — the extension is not recognised by the
  grammar registry.
- `no tags query available for "markdown"` — the grammar exists but ships no tags
  query, so no symbols can be extracted. Common for markup and data formats.

### `source is 300000 bytes, over the 262144 limit`

**Cause.** The file exceeded `parse.MaxSourceBytes` (256 KiB). It was discovered,
hashed and recorded — but not parsed. Tree-sitter's node arenas scale
super-linearly with input; a 2 MiB cap measured 6,667 MB peak heap against 2,260
MB at 256 KiB.

**Fix.** Usually, exclude the generated tree — an excluded file is never read at
all, while a skipped one has already been hashed. Raising the constant is a
source change with a measured memory cost; see
[performance.md](performance.md#tuning-for-a-large-monorepo).

### `error: grammar "x" failed to load` / `tags query for "x" rejected: ...`

**Cause.** A grammar failed to initialise, or its tags query would not compile
against it. Recorded per language rather than failing the run, so one broken
grammar degrades that language only.

**Fix.** Not user-actionable. Report it with the language name.

### `parser panicked: ...` or `parse exceeded 5s`

**Cause.** A pathological file. Both are contained to the file: the panic is
recovered and the timeout enforced per file, and the run continues with
`parse_status = 'failed'`.

**Fix.** Nothing required — the index is still published, as `degraded` rather
than `ready`. If the file is legitimate source you need indexed, it is worth
reporting with the file attached.

### `error: parse: CGO backend not compiled in; rebuild with -tags cgo_parser`

**Cause.** Something requested the CGO parsing backend in a default build.
RepoCortex ships CGO-free.

**Fix.** Do not request it, or rebuild with `-tags cgo_parser` and accept that
you now need a C toolchain.

---

## Database and schema

### `error: apply schema: migration 0001_initial was modified after being applied (recorded ..., embedded ...); shipped migrations are immutable — add a new migration instead`

**Cause.** The sha256 recorded in `schema_migrations` does not match the migration
embedded in the binary. Either you edited a shipped migration, or you are running
a binary whose migrations diverge from the one that created this index.

Silently proceeding would mean two installs of the same version have different
schemas, and every bug report from that point on becomes unreproducible.

**Fix.** As a user: `repocortex init --force` to rebuild from scratch, or go back
to the binary that created the index. As a developer: never edit a shipped
migration — add `NNNN_name.sql` with the next version. See
[git-workflow.md](git-workflow.md#shipped-migrations-are-immutable).

### `error: SQLite build lacks FTS5 (ENABLE_FTS5 absent); search is unavailable — this indicates an unexpected modernc.org/sqlite build`

**Cause.** `modernc.org/sqlite` enables FTS5 through a codegen flag it does not
document, so RepoCortex checks at startup rather than discovering it as a mangled
search result later. Seeing this means a replace directive, a vendored fork, or a
platform the codegen has not covered.

**Fix.** Check `go.mod` for a `replace` on `modernc.org/sqlite`. `repocortex doctor`
reports it as `[FAIL] fts5`; `init` refuses to proceed at all.

### `error: foreign_keys is off on N of 32 sampled connections; ON DELETE CASCADE would silently not fire and the graph would accumulate dangling rows`

**Cause.** The DSN pragmas did not take effect on pooled connections. This is the
check that catches the exact bug it was written for — pragmas are per-connection,
and configuring one connection out of eight leaves cascades silently broken.

**Fix.** Not user-actionable; it indicates a driver or DSN regression. Report it
with the Go and `modernc.org/sqlite` versions from `repocortex doctor`.

### `database is locked` / `database is busy`

**Cause.** Two processes writing at once — most often a `repocortex watch` running
alongside a manual `repocortex index`. SQLite is a single-writer store. The busy
timeout is 5 seconds by default.

There is **no bespoke RepoCortex message for this**; the raw driver text arrives
wrapped by whichever call site hit it, typically
`registry: begin batch: database is locked` or
`read schema_migrations: database is locked`.

**Fix.** Stop the watcher, or wait. A hung query is worse than a clear error,
which is why the timeout is bounded rather than infinite.

### `database disk image is malformed` / `file is not a database`

**Cause.** The index file is corrupt or is not a SQLite database. As above, this
is raw driver text, not a RepoCortex message.

**Fix.** `repocortex init --force` and re-index. The registry is a derived
artefact; there is nothing in it that cannot be rebuilt from the repository.

### `error: create index directory: permission denied`

**Cause.** `.repocortex/` cannot be created — read-only filesystem, or no write
permission on the repository root.

**Fix.** Fix permissions, or use `--project` against a writable copy.

---

## Graph integrity

### `repocortex doctor --verify-index` reports failures

```
[FAIL]  dangling edges  12 found — edges pointing at symbols that do not exist
error: 1 check(s) failed
```

| Check | Means |
|---|---|
| `dangling edges` | Edges point at symbols that do not exist |
| `orphaned symbols` | Symbols whose file row is gone |
| `unproven exact edges` | Edges claim compiler certainty without compiler evidence |
| `cross-run edges` | Edges join symbols from different index runs |
| `uncommitted visible runs` | More than one run is marked committed |

**Cause.** Any of these means the index is structurally corrupt. Historically the
first two were produced by foreign keys being off on most pooled connections; a
30-second soak generated 1,010 dangling symbols that way.

**Fix.** `repocortex index --full` rebuilds. If the failure recurs on a fresh index,
it is a bug worth reporting with the repository that triggers it — that is a
reproducer for a class of defect that is otherwise very hard to attribute.

### `error: registry: run 5 has 3 edge(s) with confidence=exact but source<>compiler`

**Cause.** A writer tried to publish an edge claiming compiler certainty without
compiler evidence. `CommitRun` re-validates the whole staging run before
publishing, so **the run is not published** — the previous revision stays.

**Fix.** Not user-actionable; it is a writer bug. The sibling messages are
`... edge(s) whose src symbol belongs to another run`, `... whose dst symbol
belongs to another run`, and `... symbol(s) whose file belongs to another run`.

### `error: registry: ReplaceFile "x.go": run 3 is committed and is not writable (pass registry.AllowCommittedRun() for incremental updates)`

**Cause.** Programmer error — an in-place write against a published run without
the explicit opt-in. Incremental indexing passes it deliberately; anything else
must not.

**Fix.** Not user-actionable. Report it with the command you ran.

---

## Watching

### `error: start watcher: watch: watched directory limit exceeded: 8192 directories, at "some/dir"`

**Cause.** The repository needs more than `DefaultMaxWatchDirs` (8192) watched
directories. The cap is set to be hit *before* Linux's historical
`fs.inotify.max_user_watches` default, so you get a clear RepoCortex error instead
of an opaque kernel one.

The watcher refuses to start rather than covering part of the tree: a watcher that
quietly covers 60% of a repository is worse than one that refuses.

**Fix.** Exclude large trees in `.repocortex.yml`. Excluding removes them from the
walk *and* from the watcher's ignore predicate, so it fixes both problems at
once. `.git`, `node_modules` and `.repocortex` are already refused unconditionally,
so look for build output, caches and vendored SDKs.

### `watch "some/dir": no space left on device`

**Cause.** The kernel refused the watch. On Linux this is
`fs.inotify.max_user_watches` (or `max_user_instances`) exhausted; on macOS/BSD
it is `RLIMIT_NOFILE`, because kqueue costs a file descriptor per directory.

This arrives on the `Errors()` channel as a warning and degrades coverage rather
than ending the session.

**Fix.** Exclude large trees first. If the repository genuinely is that large,
raise the OS limit: `sudo sysctl fs.inotify.max_user_watches=524288` on Linux, or
`ulimit -n` on macOS.

### `level=WARN msg="watcher error" error="watch: backend: ..."`

**Cause.** A backend error while running — commonly
`fsnotify: queue or buffer overflow` during a large checkout. Events were dropped.

**Fix.** Usually self-correcting: the watcher re-diffs the working tree by content
hash on every batch rather than trusting the event stream, so a dropped event for
a file that is later touched is caught. If you suspect the index is stale after a
storm of changes, `repocortex index` reconciles everything.

### Edits are not picked up

Check in order:

1. Is the file excluded? Config exclusions are applied before anything else.
2. Is it in `.git`, `node_modules` or `.repocortex`? Those are refused
   unconditionally, whatever the configuration says.
3. Did the content actually change? The watcher re-diffs by hash, so a
   touched-but-identical file produces no update and is logged at **debug** only:
   `filesystem events produced no content change`. Run with `-v` to see it.
4. Was the file created inside a directory created moments earlier? That race is
   handled twice over: entries already inside a new directory are replayed as
   synthetic creates, and the watch on the directory is re-asserted one debounce
   later. The re-assertion is not belt-and-braces — on macOS/BSD, fsnotify
   v1.10.1 can register a watch on a newly created directory and then strip its
   write notifications while registering its own, leaving a directory that is
   watched, reports no error, and never fires again. See "A whole subdirectory
   goes quiet" below. If you can still reproduce a loss here, it is a bug worth
   reporting.

### A whole subdirectory goes quiet (macOS/BSD)

**Symptom.** Edits anywhere else in the repository are picked up, but nothing
inside one directory is ever reported. No error is logged, `Add` succeeded, and
the directory is in the watch list.

**Cause.** A race inside fsnotify v1.10.1's kqueue backend. When a directory is
created in a watched tree, fsnotify emits the create event and only afterwards
registers its own watch on it, sampling the current notification flags *before*
that event is delivered. RepoCortex reacts to the event by adding its own watch —
and if that lands in the window between the sample and the write, fsnotify
re-registers the descriptor with the stale flags. kqueue's `EV_ADD` replaces
filter flags rather than merging them, so the write notification is dropped. The
window is microseconds wide and whether it is hit is decided by the scheduler.

**Fix.** Handled automatically since the re-assertion pass was added: every
directory registered while watching is re-watched, and rescanned, one debounce
period later, which is long after the window has closed. Nothing to configure.

If you see this on a build without that pass, restarting `repocortex watch` clears
it, and `repocortex index` reconciles whatever was missed.

### `level=WARN msg="watcher error" error="watch: N filesystem event(s) dropped because the queue was full"`

**Cause.** RepoCortex buffers backend events so that registering watches for a new
subtree cannot stall the backend — a stalled backend is how inotify queues
overflow. The buffer holds 4096 events; a change storm larger than the consumer
can absorb overflows it.

**Fix.** The message is the recovery instruction: do not trust the last batch as
a complete picture. `repocortex index` reconciles the tree. Dropping here rather
than blocking is deliberate — it keeps every *later* event flowing, and it is
reported rather than silent, unlike a kernel-side overflow.

### `error: initial reconcile: ...`

**Cause.** The startup reconcile pass failed before watching began. Files change
while the watcher is not running, so `watch` diffs first; a failure here is
usually the same underlying problem `repocortex index` would report.

**Fix.** Run `repocortex index` and read its error, which is not wrapped.

---

## Resolution

### `error: --min-confidence "bogus": want exact, high, medium or low`

**Cause.** Invalid flag value.

**Fix.** Use `high`, `medium` or `low`. `exact` is accepted and is meaningful,
but not for `repocortex resolve`: the heuristic resolver never emits `exact`, so
`resolve --min-confidence exact` records nothing. On a *query* —
`repocortex references X --min-confidence exact` — it filters to compiler-proven
facts only, which is exactly what you want after `repocortex scip import`.

### Resolution rate looks low

```
  References:  55
  Resolved:    16 (29.1%)
  Ambiguous:   10
  Unresolved:  29  (calls outside this repository)
```

**Cause.** Usually correct, not broken.

- **Unresolved** means the name matched nothing indexed — almost always a
  standard-library or third-party call. On a Spring/JDBC codebase this is a large
  share of every file and there is nothing to fix.
- **Ambiguous** means the resolver refused to guess. A name matching more than 8
  symbols produces no edge at all, by design.

**Fix.** If unresolved is high, check whether the dependency's source is actually
in the repository and not excluded. If ambiguous is high, the code uses very
common method names (`get`, `save`, `handle`) and there is genuinely no evidence
to disambiguate on. Compiler-backed SCIP resolution is the answer to that, and it
is built: `repocortex indexers list` reports whether an indexer for your language is
on `PATH`, `repocortex indexers install` provisions one, and `repocortex scip import`
ingests an index you already have.

### `repocortex resolve` reports nothing but references are clearly bound

Not a fault. `resolve` is idempotent, and `AddResolvedEdges` is `INSERT OR
IGNORE`, so a second run over an unchanged index reports the same reference and
resolution counts with `Edges: 0` — the edges already exist. Only a full re-index
or new references make it write again.

### `repocortex status` shows `Relationships: 0` after a successful resolve

**This no longer happens, and the entry is kept only to retire it.** `status`
used to read `index_runs.edges_found`, a column `CommitRun` writes once and
`resolve` never updates, so it reported 0 while edges sat in the table. It now
counts live rows — `SELECT count(*) FROM edges WHERE index_run_id = ?` — along
with files, symbols, parse failures and partial parses, so it agrees with the
database rather than with a snapshot taken before the graph was built.

Verified on the payment fixture:

```
$ repocortex status | grep Relationships
Relationships:  1                 # after index, before resolve

$ repocortex resolve
  Edges:       16
  Data access: 7 statement(s), 7 bound to a declared table, 7 edges

$ repocortex status | grep Relationships
Relationships:  24
```

**`Relationships` is every edge in the run, not only `CALLS`,** which is why the
three numbers above reconcile as 1 + 16 + 7 rather than matching the `Edges:`
line. The 1 is the endpoint-to-handler edge indexing wrote on its own — both ends
are in the same file, so it needs no resolution pass. `repocortex scip import` adds
`REFERENCES` edges on top of all of it.

### Resolved edges disappeared

**Cause.** You ran `repocortex index --full`. A full rebuild creates a new revision
and retires the old one, including its edges. References are re-extracted but
start unbound.

**Fix.** Run `repocortex resolve` again after every `--full` rebuild.

---

## The analysis and capability commands

These share a property worth stating once: **an empty result is a result, not an
error, and each one prints its own coverage limits alongside it.** Exit code 0
with "nothing found" means the analysis ran and found nothing; it does not mean
the analysis was skipped. If you want a non-zero exit for CI, `repocortex arch` has
`--fail-on`, and `repocortex bench` has `--fail-on-regression`. The others do not.

### `repocortex arch` says "No architecture declared" and exits 0

**Cause.** No `architecture:` block in `.repocortex.yml`. This is not an error, and
it deliberately is not reported as zero violations either:

```
$ repocortex arch
No architecture declared.

Add an `architecture:` block to .repocortex.yml with the components of your system
and the rules between them. See 'repocortex arch --help'.
```

Reporting "0 violations" here would be a pass mark nobody earned. Components are
declared rather than inferred because checking code against a description
RepoCortex generated from that same code finds nothing by construction.

**Fix.** Declare components and rules. Note that `--json` emits JSON on this path
too, with an `architecture_declared` field, so a script can branch on a field
rather than parse English.

### `repocortex config` or `repocortex deps` finds nothing

**Cause.** Usually coverage, and both commands say which:

```
$ repocortex config
No configuration keys found.

Declarations are read from .env, YAML, JSON, TOML and .properties files; usage
from os.Getenv, @Value, process.env and similar lookups. Kubernetes, Helm,
Terraform and CI definitions are not read — that is a coverage limit, not a
finding about your code.
```

```
$ repocortex deps
No dependency manifest found.

Read: go.mod, package.json, pom.xml, build.gradle, requirements.txt,
pyproject.toml, Cargo.toml.
```

**Fix.** Check your manifest is one of the listed formats and is not excluded. A
service configured entirely through Kubernetes manifests reports nothing from
`repocortex config`, and that is the tool's limit rather than a fact about the
service.

### `error: parse "x.json": trace: unrecognised export format: expected OTLP/JSON (resourceSpans) or Jaeger UI JSON (data/traceID)`

**Cause.** `repocortex trace` reads an exported file in one of exactly two shapes.
A collector's native format, a protobuf export, or a zipped bundle is not one of
them.

**Fix.** Re-export as OTLP/JSON or Jaeger UI JSON. There is no live-collector
mode: the input is always a file the caller already holds, which is what keeps
the correlation reproducible and stops trace data leaving the machine.

### `error: parse "cases.yml": bench: case file contains no cases`

**Cause.** The YAML parsed but yielded no case list. `repocortex bench` expects a
top-level sequence, not a mapping — see `fixtures/bench/payment.yml`.

### `error: scip: index.scip: scip: parse index after 0 byte(s): ... proto: invalid field number`

**Cause.** The file is not a SCIP protobuf index. Truncated downloads and
HTML error pages saved under a `.scip` name both land here.

### `repocortex scip import` matched nothing, or matched a low fraction

**Cause.** Path reconciliation. An index built in a container or by a tool that
records absolute paths does not line up with RepoCortex's repository-relative file
list. The command reports the fraction and prints examples:

```
37 of 276 document(s) matched no indexed file (87% matched).
  e.g. ../../../../../../../Users/you/Library/Caches/go-build/f8/...
```

**Fix.** If the unmatched paths share a leading directory the repository does not
have, re-run with `--path-prefix`. Paths under a build cache, as above, are
normal and need nothing — they are generated files the index never saw. An index
matching *nothing* is a hard error rather than an empty result, because zero
edges and no error is indistinguishable from a repository with no relationships.

### `scip import` reports `0 upgraded from heuristic`

**Not a failure**, and the command says so itself. SCIP records that one symbol
references another; it does not record that the reference is a *call*, and
RepoCortex will not stamp a guess about that with `exact`. Compiler facts therefore
arrive as `REFERENCES` edges alongside the resolver's `CALLS` edges rather than
replacing them. An upgrade happens only where both agree on the relationship kind
*and* the evidence line.

Measured on RepoCortex's own source with a `scip-go` index: 14,532 new edges, 905
already known, 0 upgraded. `repocortex doctor --verify-index` reported `unproven
exact edges  none` afterwards, which is the check that matters.

## Git metadata

### `git: path is not a repository`

**Cause.** A history operation was attempted against a plain directory. Note that
this is **not** an error for indexing: `repocortex index` treats a missing
repository as metadata that is simply absent, records an empty git revision, and
proceeds. RepoCortex indexes extracted tarballs and vendored trees.

### `error: git: read HEAD: reference not found`

**Cause.** A repository with no commits yet.

**Fix.** Nothing required for indexing — the revision is recorded as empty.

### `level=WARN msg="working tree has uncommitted changes; the index describes files on disk, not the commit"`

**Cause.** Informational. The index run is stamped with the HEAD SHA, but the
files indexed were the ones on disk.

**Fix.** None needed; be aware that the recorded revision is not a complete
description of what was indexed.

---

## Telemetry

### `unknown telemetry exporter "x" (want none, stdout or otlp)`

**Cause.** An invalid exporter name passed to `observe.ParseExporter`.

### `telemetry exporter is otlp but no endpoint is configured`

**Cause.** OTLP selected without an endpoint. There is deliberately no default
collector address, so a misconfiguration cannot send data somewhere by accident.

### `this repocortex build was compiled without OTLP support (-tags repocortex_nootlp); use the stdout exporter, or rebuild without the tag`

**Cause.** OTLP requested in a build made with `-tags repocortex_nootlp`, which
omits the exporter to save about 13 MB of binary.

### A test panics with `unsafe telemetry attribute "file.path": key contains "path", which names data RepoCortex does not export`

**Cause.** Working as intended. The attribute validator panics inside a test
binary and scrubs in a release build, so a call site that attaches a path fails
the suite. The offending value is deliberately not included in the message.

**Fix.** Use the typed constructors in `internal/observe/redact.go` —
`observe.Language`, `observe.SizeBucket`, `observe.Confidence`, and so on.
