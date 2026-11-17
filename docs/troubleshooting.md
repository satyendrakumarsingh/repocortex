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
message, `no index at ... — run 'repocortex init' and 'repocortex index' first`,
when it is run from a terminal or with `--http`.

A `serve` that an MCP client launched (its standard input is a pipe) does not
exit: the client could only show that as "server disconnected". It starts,
lists its tools, and answers every call with "RepoCortex is running, but this
project has no index yet", the directory, and the three commands to run there.
The answer's `index_status` is `absent`, the server's instructions say so at
connect time, and the same line is written to its log. Once the index exists
the running server answers from it; there is nothing to restart. It never
indexes on its own.

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
mcp.profile "x": want minimal, standard, full, compact or frontdoor
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

`repocortex status` says how many files were parsed, how many are text only, how
many were over the size limit (naming the largest) and how many were skipped for
each other reason. For every path, query the `files` table:

```bash
repocortex query "SELECT path, size_bytes, parse_status, parse_error FROM files WHERE index_run_id = :run AND parse_status <> 'ok' ORDER BY size_bytes DESC"
```

The skip reasons and what each means:

| `parse_error` starts with | Cause | Fix |
|---|---|---|
| `too_large` | Over the size limit (2 MiB unless `index.max_file_bytes` sets another), or grew past it during hashing | Raise the limit, exclude the file, or accept the gap; see below |
| `binary` | NUL byte in the first 8000 bytes (git's own heuristic) | Expected |
| `symlink` | Symlinks are never followed, even inside the root | Expected; index the target directly |
| `symlink_escapes_root` | The link resolves outside the repository | Expected, and deliberate — see [security.md](security.md#symlinks-are-never-followed) |
| `irregular` | Socket, device or FIFO | Expected. Opening a FIFO would hang the indexer |
| `unreadable` | Permission denied, broken link, unreadable directory | Fix permissions |

A hidden file or directory that is absent has usually been left out by rule
and has no row at all: hidden directories are not indexed unless they are one of
a short list (`.github`, `.gitlab`, `.circleci`, `.devcontainer`, `.husky` and
the like), and hidden data and temporary files (`.report.json`, `.x.swp`,
`.DS_Store`) are not indexed. To index one, name it in `index.include` with its
dot, keeping `**` in the list: `include: ["**", ".tooling/**"]`. The rule is in
the user manual under "Hidden files and directories".

Two more:

- `no grammar for this file type` — the extension is not recognised by the
  grammar registry. The file is recorded and not indexed. `status` counts it
  under `Skipped` as `no_grammar`.
- `text_only: no definition query for "markdown"; …` — the grammar exists but
  ships no definition query, so no symbols can be extracted. The file's **text is
  indexed**: `repocortex search` finds a name that appears in it, `repocortex
  symbol` does not. 91 of the 206 grammars are in this group, mostly data and
  configuration formats; `repocortex languages --verbose` lists them.

### `repocortex index` says "up to date" after an edit

**Cause.** An incremental run reads a file only when it might have changed: a
file whose size and modification time match the index, and are more than two
seconds older than the previous run, is taken as unchanged without being opened
(on Linux and macOS its inode change time must be that old too). An ordinary
save always fails that test and is hashed. What can slip through is a same-size
edit whose modification time was deliberately restored on Windows, or one made
across a clock that stepped backwards or a network filesystem whose clock is
more than two seconds behind.

**Fix.** `repocortex index --rehash` reads and hashes every file. If that finds
the edit, the timestamps on that filesystem are not trustworthy; use `--rehash`
there. `repocortex index --full` rebuilds from scratch and is not needed for
this.

### `too_large: 6902462 bytes, over the 2097152 byte limit (index.max_file_bytes)`

**Cause.** The file is over the size limit. It is recorded with its size and is
neither parsed nor text-indexed. `repocortex index` and `repocortex status` count
these files and name the ten largest.

The limit is 2 MiB. Parsing costs memory in proportion to the source: a 2 MiB C
file takes about 0.9 s and 300 MB of process memory while it is parsed (the
measurements are in [performance.md](performance.md#the-size-limit)).

**Fix.** If the file is source you want indexed, raise the limit for the
repository, up to 8 MiB:

```yaml
# .repocortex.yml
index:
  max_file_bytes: 4194304
```

If it is generated or vendored, exclude it instead. An excluded file is never
read and is not reported.

An index written by an older version shows `source is 300000 bytes, over the
262144 limit` for the same condition: the parser used to refuse anything over
256 KiB. Re-indexing with this version parses those files.

### `budget_exceeded: parser stopped at byte N of M (line L) on …`

**Cause.** The parse was stopped before the end of the file. What was read is
indexed (`parse_status = 'partial'`); definitions after line L are missing, and
the index is `DEGRADED`. The text names the limit:

- `its iteration limit`, `its node limit`, `its stack depth limit`, `its memory
  budget`: the parser's own work limits, which are derived from the file's length
  and counted by the parser. The same file gets the same verdict on every machine.
- `the 1m0s wall-clock ceiling`: a last-resort safety net, for a file that stays
  inside every work limit and still does not finish. This one depends on machine
  speed, and the text says so.

`stopped_early: …` is the same outcome when the parser gave up for a reason of
its own. Before a file is recorded either way, it is parsed a second time by the
parser's other error-recovery route, and the attempt that read further is kept.

**Fix.** Nothing is required: the rest of the index is unaffected. If the file is
generated, exclude it. If it is ordinary source, it is worth reporting with the
file attached.

### `error: grammar "x" failed to load` / `tags query for "x" rejected: ...`

**Cause.** A grammar failed to initialise, or its tags query would not compile
against it. Recorded per language rather than failing the run, so one broken
grammar degrades that language only.

**Fix.** Not user-actionable. Report it with the language name.

### `parser panicked: ...`

**Cause.** A pathological file. It is contained to the file: the panic is
recovered and the run continues with `parse_status = 'failed'`.

**Fix.** Nothing required — the index is still published, as `degraded` rather
than `ready`. If the file is legitimate source you need indexed, it is worth
reporting with the file attached.

`parse exceeded 5s: context deadline exceeded` is what an older version recorded
for a file that did not parse within five seconds of wall-clock time. That limit
is gone: it made the result depend on how busy the machine was, and the parse it
gave up on kept running. See `budget_exceeded` above for what replaced it.

### `syntax_errors: N error nodes and M missing tokens, first at line L`

**Cause.** Not a failure. The parser read the file to its end and recovered from
input the grammar does not accept: `parse_status = 'partial'`, and the index is
not degraded by it. An error node is text the parser could not fit into the
grammar; a missing token is one it had to assume in order to continue.

This is common in valid code. Grammars do not run the C preprocessor, so a macro
call at file scope, a `#if` between the cases of a `switch` or a `#define` inside
an initialiser all produce error nodes. In redis, 408 of 1,025 C files have at
least one.

**Fix.** Usually nothing. If a definition near line L cannot be found, this is
why.

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
`fs.inotify.max_user_watches` (or `max_user_instances`) exhausted. On macOS it is
the open-file limit, and the message is `out of file descriptors after N watched
directories`: RepoCortex holds one descriptor per watched directory there and
none per file. On the BSDs, where fsnotify's kqueue backend is used, it is a
descriptor per directory and per file.

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

### `watch` takes a long time to print "Watching", or an edit takes seconds to be noticed

**Start-up.** `watch` prints `watching N directories...` to standard error every
two seconds while it registers the tree, then reconciles, then prints
`Watching <project>: N directories (revision R), ready in T`. Ctrl-C works
throughout. Up to v1.0.1 start-up on macOS opened every file in the repository
one at a time (17 s to over a minute on a 7,100-file repository on a loaded
machine), printed nothing, could not be interrupted, and lost edits saved while
it ran.

**An edit that takes a couple of seconds on macOS.** A file rewritten in place
(an append, some editors' default save) is found by a sweep of file sizes and
times that runs every two seconds, or less often on a tree that takes long to
sweep. A save made by writing a temporary file and renaming it, which is what
most editors do, is reported by the kernel at once. Linux and Windows report
both at once. See `watch` in the CLI reference for why.

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

### A whole subdirectory goes quiet (BSD; macOS up to v1.0.1)

This is a fault in fsnotify's kqueue backend. RepoCortex no longer uses that
backend on macOS, so it applies there only to v1.0.1 and earlier.

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

**Cause.** The start-up reconcile pass failed. Files change while the watcher is
not running and while it is starting, so `watch` registers its watches and then
diffs the tree against the index before it reports ready; a failure here is
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
  Unresolved:  29  (no definition of that name is indexed: code outside this
               repository, or code in it that the index did not extract)
```

**Cause.** Usually correct, not broken.

- **Unresolved** means no definition with that name is in the index. Usually
  that is a standard-library or third-party call; on a Spring/JDBC codebase it
  is a large share of every file and there is nothing to fix. It is also what a
  call to code in this repository looks like when the index holds no definition
  for it: a language whose definitions are not extracted, a macro, a file that
  failed to parse. `repocortex languages` and the parse failures in
  `repocortex status` say which applies.
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

### `Shallow clone: N commit(s) available`

**Cause.** The repository was cloned with `--depth`, which is the default for
most CI checkouts. `hotspots`, `bus-factor`, `owners` and their agent tools read
the commits the clone holds and stop at its boundary. The oldest commit is
counted but contributes no changed files, because the parent it would be
compared with was never fetched. Everyone and everything older than the depth is
absent, and the output says so instead of "the whole history was read".

**Fix.** `git fetch --unshallow`, or in GitHub Actions `fetch-depth: 0` on
`actions/checkout`. Nothing is needed if the recent commits are all you meant to
rank.

### `git: this is a shallow clone and the commit needed is older than the history that was fetched`

**Cause.** A revision such as `HEAD~20` or a base branch passed to
`impact --base` / `analyze_change` names a commit beyond a shallow clone's
depth.

**Fix.** `git fetch --unshallow`, or fetch with a `--depth` that reaches the
base revision.

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
