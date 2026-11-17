# CLI reference

One entry per command: the question it answers, its flags, one example that was
actually run, and the exit code where it is load-bearing. This is a lookup table.
For "which of these do I need", start at [GUIDE.md](../GUIDE.md); for the reasoning
and the longer worked examples, [user-manual.md](../user-manual.md).

Where a command's own `--help` argues its design, that argument is not repeated
here. Run `repocortex <command> --help` — the help text in this tool is written to
be read.

## Global flags

Available on every command.

| Flag | Effect |
|---|---|
| `--json` | emit machine-readable JSON on stdout |
| `--project <dir>` | project root (default: current directory) |
| `-q`, `--quiet` | suppress all logging |
| `-v`, `--verbose` | verbose logging |

Every query command takes `--json`. That is deliberate: a git hook or a CI check
should not have to speak JSON-RPC to ask the same questions the MCP tools answer.

## The provenance warning

Every index run records the build that wrote it. When a query command opens an
index written by a **different** build, it says so on stderr and then answers
anyway:

```
$ repocortex callers Greet
time=… level=WARN msg="This index was written by repocortex dev and you are running
9.9.9-test. Resolution rules change between builds, so these results may come from
different rules than this binary would produce. Re-index to refresh."
Greet (function) at main.go:3

KIND   SYMBOL  EVIDENCE   CONFIDENCE
CALLS  main    main.go:5  high (source_code)

1 caller(s).
```

An index is data *plus* the resolution rules that produced it, and those rules
have already changed twice in this project's history in ways that moved real
edges. An index written either side of such a change holds different edges for
identical source, and every query against it succeeds and looks exactly as
trustworthy as a current one. There is no symptom to notice, which is why the
index has to say so itself.

A run with no stamp — one written before the column existed — reports **unknown**
provenance rather than known staleness, because the advice differs: "re-index to
be sure" is not the same as "these results came from other rules".

Neither is an error. `-q` suppresses it along with all other logging. A schema
mismatch is a different thing and *is* an error, because the reader cannot
safely interpret the rows at all:

```
$ repocortex callers Redact
error: index schema is v7 but this build of repocortex expects v8 — run 'repocortex index' to upgrade it
```

## Exit codes

Most commands are 0 on success and 1 on error. Four gate:

| Command | Codes |
|---|---|
| [`verify`](#verify) | 0 `pass`, 0 `advisory`, 1 `fail`, 2 `unverified` |
| [`arch --fail-on`](#arch) | 1 when a violation at or above that severity exists |
| [`bench --fail-on-regression`](#bench) | 1 when any case fails |
| [`docs --check`](#docs) | 1 when the committed documents have drifted |
| [`vulns --fail-on`](#vulns) | 1 when a finding at that reachability or stronger exists, when its version could not be compared, or when an advisory file could not be read |
| [`rename`](#rename-old-new) | 0 would apply, or applied with `verify` passing or advisory; 1 error, `verify` `fail`, or an incomplete rollback; 2 refused (nothing written); 3 applied but not verified complete |
| [`move`](#move-symbol-from-file-to-file) | 0 would apply, or applied and the package built; 1 error or an incomplete rollback; 2 refused (nothing written, or everything restored); 3 applied but no build checked it |

---

## Index lifecycle

### `init`

**Answers:** nothing yet — it creates `.repocortex/` and applies the registry
schema. It does not index.

| Flag | Effect |
|---|---|
| `--force` | recreate the index if it already exists |

```
$ repocortex init
Initialized RepoCortex for "payfix2"
  Index:          /path/payfix2/.repocortex/index.db
  Schema version: 8 (8 migration(s) applied)
  Accuracy mode:  auto

Next: repocortex index
```

### `index`

**Answers:** what is in this repository? Walks it, parses every supported file,
and makes the result queryable. Incremental by default: only files whose
**content hash** changed are reparsed, so a `git checkout` that rewrites
timestamps does not trigger a full reparse.

An update costs a small fraction of a rebuild, but it is not independent of
repository size: every file is stat'ed on every run, and after a change
references are re-resolved across the whole index. On django (7,090 files) that
is 0.13 s when nothing changed and about 1 s after a two-file edit; see
[performance](../performance.md#figures-other-tools-publish-measured-here).

**Which files are read.** A file is read and hashed only when it might have
changed. It is taken as unchanged, without being opened, when all of these hold:

1. the previous run recorded it, under the same build;
2. its size is the recorded size;
3. its modification time is the recorded one, to the second;
4. that time is at least two seconds older than the moment the previous run
   began;
5. on Linux and macOS, its inode change time is that old too.

Every other file is hashed and the hash decides. A file that was touched but
not changed is hashed once, found identical, not reparsed, and its new time
recorded. A same-size edit made in the same second as the save the index
recorded is not missed, because rule 4 does not believe a time that close to
the run that read it. Rule 5 catches a same-size edit whose modification time
was put back (`cp -p`, `rsync -t`, `touch -r`): a program cannot set the change
time.

Not covered, and what `--rehash` is for: Windows offers no change time without
opening the file, so a same-size edit whose modification time was restored to
the second is not seen there; and a clock stepped backwards by more than two
seconds between runs, or a network filesystem whose clock is that far behind,
can hide a same-size edit that lands on the recorded second.

**Hidden files.** Hidden configuration is indexed (`.github/workflows/*.yml`,
`.eslintrc.js`, `.env.example`, `.gitlab-ci.yml`). Hidden editor and tool state
is not: any hidden directory other than a short list of automation directories
(`.github`, `.gitlab`, `.circleci`, `.devcontainer`, `.husky` and the like), and
hidden data or temporary files such as `.syms.json`, `.DS_Store` and `.x.swp`.
`.git` and `.repocortex` are never indexed at any depth. `index.exclude` in
`.repocortex.yml` removes more, and an `index.include` pattern that names a
hidden path with a dot of its own (`.claude/**`, `.syms.json`) brings it back.
The full rule is in the
[user manual](../user-manual.md#hidden-files-and-directories).

`status` "Last indexed", and the `provenance.age_seconds` every tool answer
carries, are the time of the latest run, full or incremental, including a run
that found nothing to change.

| Flag | Effect |
|---|---|
| `--full` | rebuild from scratch. Discards resolved edges; re-run `resolve` |
| `--rehash` | incremental, but read and hash every file instead of trusting an unchanged size and modification time |
| `--accuracy fast\|auto\|exact` | override the accuracy mode (full path only) |
| `--workers N` | parse concurrency (default 4). Peak memory scales with this, not with repository size |
| `--digest` | print the registry digest — the determinism gate |

`--workers` and `--accuracy` take effect only on the `--full` path, but are
validated on every path, so a typo fails immediately rather than being silently
discarded:

```
$ repocortex index --accuracy nonsense
error: --accuracy "nonsense": want fast, auto or exact
$ echo $?
1
```

A full rebuild publishes atomically: the new revision becomes visible only once
the whole run is written, so an interrupted rebuild leaves the previous revision
in place.

```
$ repocortex index --full --digest
  Files:         708 indexed, 15 skipped (723 discovered)
  Symbols:       10832
  References:    60185 (bound by `repocortex resolve`)
  Imports:       3829
  Parsed:         708 (702 clean, 6 with syntax errors)
  Syntax errors:  6 files read to the end with errors the parser recovered from; definitions inside the unreadable regions may be missing
  Text only:      7 files searchable by text, no definitions (no definition query for the language): yaml 3, html 2, dockerfile 1, gomod 1
  Skipped:        8 files not indexed: no_grammar 8
  Digest:        747fbf7969c177e83eeb413fa7f568e843936546d22db05c7b888354d39cb52e
  Duration:      10895ms
```

*This output was captured before YAML, JSON, HTML and Markdown gained definition queries. On a current build those languages are parsed and yield symbols, so a `Text only` row lists fewer of them (for example Dockerfile, `go.mod` and diffs).*

The rows after `Imports` say what happened to every file, and they are read from
the index, so `status` and `doctor` print the same ones. A row that would say
zero is left out.

| Row | Meaning |
|---|---|
| `Parsed` | files a syntax tree was built for: read cleanly, read with syntax errors, or stopped early |
| `Syntax errors` | the parser reached the end of the file and reported error or missing nodes. The file is indexed; a definition inside a region the parser could not read may be absent |
| `Incomplete` | the parse was stopped before the end of the file. What was read is indexed, the rest is missing, and each file is named with the byte and line it stopped at and the limit that stopped it |
| `Parse failures` | nothing was extracted. Each file is named with its error |
| `Text only` | the language's grammar has no definition query, so no symbols exist for it. The file's text is indexed, so `search` finds a name that appears in it |
| `Too large` | over the size limit: not parsed, not searchable. The largest ten are named with their sizes |
| `Skipped` | everything else that was not indexed, by reason: `no_grammar`, `binary`, `symlink` and so on |

Only `Incomplete` and `Parse failures` make the index `DEGRADED`. A file that
parses cleanly and declares nothing (a package comment, a header of constants) is
`clean` and is not a problem.

**The size limit.** A file over 2 MiB (2,097,152 bytes) is not read. Set a
different limit for a repository in `.repocortex.yml`:

```yaml
index:
  max_file_bytes: 4194304     # 1024 to 8388608
  exclude:
    - "**/generated/**"       # generated and vendored trees are better left out
```

A value outside the range is an error, not a silent default. The limit is
bounded because parsing costs memory in proportion to file size (about 150 bytes
of process memory per source byte) and `.repocortex.yml` arrives with the
repository. An unchanged tree still names what is left out:

```
$ repocortex index
nj is up to date (1176 files, revision 3, 993ms)
  Too large: 1 file over the size limit, not parsed and not searchable (raise index.max_file_bytes in .repocortex.yml to include them)
      Src/Newtonsoft.Json.Tests/large.json (6.6 MiB)
```

`Symbols`, and the `Partial parses` and `Parse failures` lines printed when there
are any, are counted from the rows the run published, so they are the numbers
`repocortex status` prints next and the two parse figures add up to the run's
stored `parse_failures`. `Symbols` is everything the index holds a definition
for: what the parsers extracted, plus the endpoints, tables, configuration keys
and deployment units the analysers found. A partial parse is a file the parser
recovered from or got nothing out of; a parse failure is a file with no symbols
at all. The JSON form carries them as `symbols`, `partial_parses` and
`parse_failures`.

### `resolve`

**Answers:** which definition does each reference point at? Builds the call
graph. Separate from `index` because binding needs the complete symbol table,
which does not exist until every file has been parsed.

| Flag | Effect |
|---|---|
| `--min-confidence high\|medium\|low` | drop bindings weaker than this instead of recording them |
| `--no-scip` | do not import an `index.scip` found in the project root or `.repocortex/` |
| `--accuracy fast\|auto\|exact` | override the configured accuracy mode for this run; `fast` skips the Go type check and the compiler indexers below |
| `--allow-repo-build` | let the compiler pass run indexers that execute code the repository chooses: `scip-java` without Docker (Maven, Gradle), `scip-clang` (the compilers and flags in `compile_commands.json`) and `rust-analyzer` (cargo build scripts and procedural macros). Refused by default; only your command line sets it, `resolve` never does |

Resolution itself never produces `exact`; that is reserved for compiler-backed
data and is enforced in Go, in a SQL `CHECK`, and again at commit time.

**Go is type-checked by default.** In `auto` and `exact` mode, when the index
holds Go and a `go.mod` covers it, `resolve` runs the standard library's own
type checker (`go/types`, no tool to install, no child process, no network)
after the heuristic pass and lands what it proves as `exact` edges whose detail
says `go/types (built in)`: every call, construction and reference to a
repository function, method, type, variable or constant, and which repository
types implement which repository interfaces. Heuristic edges the compiler
confirms are raised, calls the heuristic could not bind are added, edges a
previous pass proved and this one does not are retracted, and heuristic call
edges the compiler resolved to something else are withdrawn. The standard
library is read from `GOROOT/src` (found from `GOROOT`, else `go` on `PATH`,
without running it) and dependencies from `vendor/` or the module cache at the
version `go.mod` requires; an import that cannot be found becomes an empty
stub, and anything that depends on a stub is withheld, not guessed. Only the
host platform's view of build constraints is checked: files another platform
would build keep their heuristic edges. The block is printed as `Go types:`;
`status` reports whether the Go graph is type-checked. A per-run cost is the
type check itself: a few seconds for a repository of this size.

**Other languages use the compiler indexer you already have.** In `auto` and
`exact` mode, after the Go type check, `resolve` builds the plan that
[`indexers list`](#indexers) shows and runs the compiler indexer of each project
whose indexer is already installed: `scip-typescript` for TypeScript and
JavaScript, `scip-python` for Python, `scip-clang` for C and C++, `rust-analyzer
scip` for Rust and `scip-java` for Java where the plan allows it. The trust rules are the planner's, unchanged. Which indexers may run comes
from your user configuration and never from the repository; `resolve` installs
and downloads nothing, so an indexer that is not installed is reported as not
run; a step the plan marks blocked (Java without an allowed build) does not run;
Go is left to the type check, which is why a `scip-go` index is never produced
here, and if you put one in `index.scip` and the type check disagrees about a
call, the type check's answer is kept and the other edge is withdrawn. A Python
project nested in another is read by the run of the one above it. Each tool's
output is kept under `.repocortex/scip/` (one `.scip` file per project) and every
file there is imported through the same importer `scip import` uses. A step that
fails or runs past its limit (the smaller of `accuracy.indexers.timeout` and five
minutes) is reported on one line with the tool's own first lines and does not
fail `resolve`.

**C and C++ (`scip-clang`) and Rust (`rust-analyzer scip`).** Both are the
user's own installs: RepoCortex has no pinned release of either, so there is no
version and digest to verify, and it never fetches them (`indexers pins` does not
list them; `indexers install` skips them, saying they are user-installed). Both run only with `--allow-repo-build`: `scip-clang` runs the compilers and flags
named in the repository's own `compile_commands.json`, and `cargo` runs its build
scripts and macros, so the repository would choose what executes. A C or C++ project
runs only with a `compile_commands.json` beside its `CMakeLists.txt` or
`meson.build`; without one it is reported as "cannot run, because there is no
compile_commands.json", with the command that makes one, and nothing is run. A
Rust project is a single Cargo package; a Cargo workspace
is reported as not run, with the `rust-analyzer scip` line to run yourself and
`scip import`. These two have been exercised against stand-in programs that write
a hand-made index, never against the real tools, so no accuracy is claimed for
them.

**A missing indexer is said, not silent.** After `resolve`, and in `status`, a
language with at least 10 files that have definitions, whose indexer is not
installed, gets one `Suggested:` line: the language, what the indexer would add,
and the command to run (`repocortex indexers install` for the pinned ones, the
tool's own install line for the rest). It installs nothing. It is not printed
with `--quiet` or `--accuracy fast`, and the same entries are under
`suggested_indexers` in `--json` output of both commands.

An index is reused, and the tool not run again, while nothing it reads has
changed. The key beside each file (`.repocortex/scip/<project>.key`) is a digest of
the indexer's name, version and runner; the content hash of every indexed source
file of its languages under the project; the content of the project's
configuration and lock files (`package.json`, `tsconfig*.json`, `pyproject.toml`,
`requirements*.txt`, `pom.xml`, the lock files); and the modification time of
`node_modules`, `.venv` and `venv`, which the indexers read dependencies from. A
dependency changed in place, without touching the directory, is the one change
the key does not see. A reused index is still imported into the current index
run, which is what makes `index` followed by `resolve` correct after a change:
the files whose hash moved re-run their project's indexer, and the rest are
re-imported from disk.

What an import proves is written beside the edges. The files the indexer read
are recorded (`compiler_coverage`; a file the indexer skipped is not in it), and
an unbound reference is marked as ruled out when the indexer resolved an
occurrence spelled the same, on the same line, to a symbol outside this
repository, and there are at least as many such occurrences as references. A
position the indexer resolved to nothing (a receiver of unknown type) is never
ruled out. Answers such as `find_callers` stop counting a ruled-out reference as
a possible caller of a definition in a file the indexer read, and say how many
they set aside ("shown by the compiler to be uses of something else"). Heuristic
call edges the indexer resolved the same way are withdrawn. One block per
indexer is printed as `Compiler:`: whether it ran or was reused, the files it
covered, the calls it proved, the edges it raised to `exact` and added, and the
time; `status` prints one line per language saying whether its graph is
compiler-checked and by what.

If
an `index.scip` sits in the project root or `.repocortex/`, `resolve` imports it
afterwards (as `scip import` would) and prints a `SCIP:` line with the edges
upgraded, or, when the import fails, the path and the reason. A failed import
does not fail `resolve`. JSON output carries the same under `scip`.

```
$ repocortex resolve
  References:  29506
  Resolved:    12062 (40.9%)
    by confidence:
      high:    9714
      medium:  133
      low:     2215
  Ambiguous:   2063
  Unresolved:  15381  (no definition of that name is indexed: code outside this
               repository, or code in it that the index did not extract)
  Edges:       11720  (one per caller and definition; references between the same pair share one)
  Data access: 476 statement(s), 458 bound to a declared table, 450 edges
  Config:      32 read(s), 2 bound to a declared key, 2 edges
  Duration:    1260ms
```

The summary counts two different things and keeps them apart. The tiers under
`Resolved` are **references**: how many of the bound references were bound at
each confidence. `Edges` is **edge rows** in the graph, counted from the table,
with their own tiers beneath (omitted from the older sample above). There are
fewer edges than resolved references because references between the same two
definitions share one edge. On sinatra:

```
  Resolved:    2932 (35.9% of the 8178 that name something this index defines)
    by confidence:
      high:    2564
      medium:  367
      low:     1
  Ambiguous:   5246
  Unresolved:  6437  (no definition of that name is indexed: code outside this
               repository, or code in it that the index did not extract)
  Edges:       2342  (one per caller and definition; references between the same pair share one)
    high:    2073
    medium:  269
```

The one low-confidence reference has no edge of its own: a stronger reference
between the same two definitions already made it. A second `resolve` with
nothing new to bind prints no tiers under `Resolved`, because a reference's tier
is known only to the pass that binds it, and still prints the edges that are in
the graph ("this pass found nothing new to bind"). `Edges` counts every kind
this pass writes: `CALLS`, `INSTANTIATES` (a call that constructs a type; the
`Constructs:` line above says how many), `REFERENCES`, `EXTENDS` and
`IMPLEMENTS`.

An event topic declared in several files is one symbol per file until
resolution folds them into one node. When that happens an `Events:` line says
how many were merged, and `status` then counts that many fewer symbols than
`index` printed. `--json` carries it as `events_merged`.

**Unresolved** means no definition with that name is in the index. That is
every call into a library, and it is equally every call to code in this
repository that the index holds no definition for: a language whose definitions
are not extracted, a macro, a file that failed to parse. It is not a count of
external calls. `--json` carries `by_confidence` (references bound by this pass),
`edges` (edges written by this pass), and `edges_total` with
`edges_by_confidence` (the graph).

### `watch`

**Answers:** keeps the index current as you edit. Runs until interrupted.

| Flag | Effect |
|---|---|
| `--debounce <duration>` | quiet period before reacting to a burst of edits (default 300ms) |

Editors emit bursts of events for one save, so changes are debounced and
coalesced. Each file updates in its own transaction, so an agent querying
concurrently always sees a file either entirely before or entirely after a
change.

**Start-up.** `watch` registers every directory, then reconciles the index with
the tree, then prints its banner:

```
$ repocortex watch
Watching django: 3273 directories (revision 2), ready in 1.59s. Press Ctrl-C to stop.
```

When registration takes longer than two seconds, a `watching N directories...`
line goes to standard error every two seconds until it is done, so a slow start
says it is still starting. (It took 1.6 s on django, so none was printed there.)
Ctrl-C during start-up stops it at the next directory. The reconcile runs after
registration and not before, so an edit saved while `watch` was starting is
picked up before the banner and not lost between the two.

**What a watch costs, and what it can see.** One watch per directory, and none
per file, on every platform:

| Platform | Mechanism | A file rewritten in place | A file created, deleted or renamed (how most editors save) | Limit |
|---|---|---|---|---|
| Linux | inotify, through fsnotify | reported at once | reported at once | `fs.inotify.max_user_watches`, one per directory |
| Windows | ReadDirectoryChangesW, through fsnotify | reported at once | reported at once | one handle per directory |
| macOS | kqueue on each directory, plus a sweep | found by the next sweep | reported at once | one file descriptor per directory |

macOS is the one with a trade in it. kqueue reports a write to a file only to a
process holding that file open, so the usual approach (and fsnotify's) opens
every file: 10,400 descriptors and as many `open` calls for django's 7,100 files
and 3,273 directories, which is what made start-up take 17 s to over a minute
and put a ceiling on the size of repository that could be watched at all.
RepoCortex holds a descriptor per directory instead (3,298 on django). The
kernel then reports every change to a directory's listing at once, and a file
rewritten where it lies is found by a sweep that compares each file's size and
modification time with what was last seen. The sweep runs every two seconds, or
less often on a tree that takes long to sweep, so that it costs at most about a
twentieth of one core; an in-place write is therefore noticed within one sweep
interval and not instantly. On the BSDs fsnotify's kqueue backend is still
used, with a descriptor per file.

What the descriptor limit implies on macOS: the number of directories that can
be watched is bounded by the process's open-file limit (`ulimit -n`; the
per-process maximum is `sysctl kern.maxfilesperproc`, 10,240 by default on older
systems). `watch` refuses more than 8,192 directories with a named error before
that is reached. Hidden tool directories (`.venv`, `.idea`, `.claude` and the
like) are not watched; see [index](#index).

### `status`

**Answers:** what state is the index in, and how much is in it? All figures are
read from the registry; nothing is estimated. No flags beyond the global ones.

```
$ repocortex status
Project:        nj
Index:          READY
Schema:         v18
Revision:       52fa3aef1f2cadcd3a3f874251eddc98d3efbbaa
Files:          1176
Symbols:        10534
Relationships:  0
Parsed:         956 (895 clean, 61 with syntax errors)
Syntax errors:  61 files read to the end with errors the parser recovered from; definitions inside the unreadable regions may be missing
Text only:      42 files searchable by text, no definitions (no definition query for the language): json 24, xml 11, markdown 5, yaml 2
Too large:      1 file over the size limit, not parsed and not searchable (raise index.max_file_bytes in .repocortex.yml to include them)
                  Src/Newtonsoft.Json.Tests/large.json (6.6 MiB)
Skipped:        177 files not indexed: no_grammar 168, binary 9
Last indexed:   2026-11-12T20:19:35+05:30
Index size:     22.0 MiB
Accuracy mode:  auto
MCP profile:    minimal
Capabilities:   code
```

Below `Relationships`, one `<Language> graph:` line per language with
definitions says whether a compiler stands behind it: `Go graph:` for the
built-in type check, `Typescript graph:` or `Python graph:` when `resolve` ran
the compiler indexer (`compiler-checked (scip-typescript): 998 exact edge(s)
across 517 of 541 file(s)`), and `heuristic only` otherwise, with what would
change that. `--json` carries the same under `compiler_checked`.

*This output was captured before YAML, JSON, HTML and Markdown gained definition queries. On a current build those languages are parsed and yield symbols, so a `Text only` row lists fewer of them (for example Dockerfile, `go.mod` and diffs).*

The rows from `Parsed` to `Skipped` are the ones [`index`](#index) prints, read
live from the `files` table. `--json` carries them under `parse`, with
`largest_too_large`, `incomplete_files` and `failed_files` as lists of paths
with sizes; `parse_failures` is the files whose indexing was abandoned (failed plus
incomplete, the number that makes an index degraded) and `partial_parses` is the
files with `parse_status = 'partial'` (syntax errors plus incomplete).

`Index: DEGRADED` means at least one file's parse failed or was stopped early.
Syntax errors, text-only files and files over the size limit do not degrade an
index: nothing was abandoned in them.

**What `parse_status` means.** It describes what the parser did with the file,
not what was extracted from it. Whenever it is not `ok`, `parse_error` starts
with a reason token:

| `parse_status` | `parse_error` starts with | Meaning |
|---|---|---|
| `ok` | (empty) | read to the end, no syntax error |
| `partial` | `syntax_errors:` | read to the end; the text gives the number of error nodes and missing tokens and the line of the first |
| `partial` | `budget_exceeded:` | stopped on a parse limit; the text gives the byte and line and names the limit |
| `partial` | `stopped_early:` | the parser gave up before the end for a reason of its own |
| `failed` | (the error) | no usable syntax tree |
| `skipped` | `text_only:` | no definition query for the language; text indexed for search |
| `skipped` | `too_large:` | over the size limit; the text gives the size and the limit |
| `skipped` | `no grammar …`, `binary`, `symlink`, `empty`, … | not indexed |

An index written by an older version used `partial` for "no symbol or reference
was recorded" and never gave it a reason. Such a row is shown as `recorded by an
older version`; it says nothing about the parse. `repocortex index` with a new
version re-parses every file and rewrites them.

### `doctor`

**Answers:** is the environment sane, and is the graph structurally sound?

| Flag | Effect |
|---|---|
| `--verify-index` | run the graph invariant gate: dangling edges, orphaned symbols, edges claiming compiler certainty without compiler evidence |
| `--resolution-precision` | adjudicate the heuristic resolver against compiler facts already imported into this index |
| `--vacuum` | reclaim freelist pages and defragment index database |

`--resolution-precision` needs a run holding both heuristic and compiler edges,
so import a SCIP index first. It reports precision, never recall, and it reports
the coverage the precision was computed over. `doctor`'s exit status is **not**
affected by the measurement — add `--json` to gate on it yourself; the report
lands under `resolution_precision`.

```
$ repocortex doctor --verify-index
[ok  ]  go runtime                go1.25.13 darwin/amd64
[ok  ]  sqlite                    3.53.3
[ok  ]  fts5                      ENABLE_FTS5 present
[ok  ]  parse status              723 files: 702 clean, 6 with syntax errors, 0 incomplete, 0 failed, 9 text only
[ok  ]  dangling edges            none
[ok  ]  orphaned symbols          none
[ok  ]  unproven exact edges      none
...
```

On a repository with a file over the size limit:

```
[ok  ]  parse status  1176 files: 895 clean, 61 with syntax errors, 0 incomplete, 0 failed, 45 text only
[warn]  size limit    1 file over the size limit is not in the index
                        at Src/Newtonsoft.Json.Tests/large.json (6.6 MiB)
                        means: nothing in these files can be found, by name or by text
                        do:    raise index.max_file_bytes in .repocortex.yml, or leave generated files out
```

`parse status` is always printed when an index exists. It becomes a warning when
a parse failed or was stopped early, and then lists those files with the reason
stored for each. Two more checks appear only when they have something to say:
`size limit`, naming the largest files left out for their size, and `parse status
age`, when rows written by an older version are still in the index.

### `version`

**Answers:** which build is this? A source build reports `dev`/`none`/`unknown`,
which is correct rather than broken — the real values are injected with
`-ldflags` at release time.

```
$ repocortex version
repocortex dev (commit none, built unknown, darwin/amd64)
```

---

### `languages`

**Answers:** which languages does this binary actually resolve, and to what
depth? No flags beyond `--verbose`, which lists the parsed-tier grammars too.

The tiers are computed from what the parser can do at run time, not read from a
list, so this output cannot disagree with what the binary does. A language is
`typed` when it has definition, import and receiver queries and something that
tells candidates of one name apart — an arity query (Go, Java, TypeScript, C#, C++,
Kotlin, Scala) or a scope analysis that states the receiver's class (Python); `resolved` when it has
definitions and imports; `parsed` when it has a definition query and nothing more. A grammar that is
bundled without a definition query is not in a tier at all: it is listed as
`text only`: files in that language are indexed for full-text search and yield
no symbols. How much each language
actually yields was checked by hand; see
[verification](../verification/2026-11-13/README.md) (and the [first record](../verification/2026-11-12/README.md)).

```
$ repocortex languages
TIER              COUNT  WHAT YOU GET
typed             8      definitions, imports, receiver discrimination by arity or class — references can reach 'high'
resolved          6      definitions and imports — references bind across files
parsed            77     definitions and search only — no cross-file references claimed
with definitions  91
text only         115    no definition query: full-text search only, no symbols
total             206    

typed:    [c_sharp cpp go java kotlin python scala typescript]
resolved: [c javascript php ruby rust tsx]

(77 parsed-tier and 115 text-only languages not listed; pass --verbose)
```

Read the tiers as a coverage statement before trusting an empty relationship
result: a `parsed`-tier language will never produce a caller, and the absence is
a property of the tier, not of the code.

A file in a `text only` language (Perl, Haskell, Pascal, COBOL, Nix, Assembly and
the rest) is found by `search` and never by `symbol`; the index records it as
skipped with reason `text_only`. `--verbose` and `--json` (`text_only`) list
those languages.

### `hooks install`, `hooks status`, `hooks uninstall`

**Answers:** keep the index current on every commit, without a daemon.

| Flag | Effect |
|---|---|
| `--binary PATH` | (`install`) the `repocortex` the hook should call; default is this executable |

`install` writes a Git `post-commit` hook that re-indexes after each commit, so
the portal and any connected agent describe the commit you just made. The block
is written inside marked delimiters: an existing hook is appended to, never
replaced, and `uninstall` removes exactly the marked block. The hook runs
detached and swallows every failure — a hook that can fail a commit is a hook
people delete.

```
$ repocortex hooks status
post-commit: not installed
The index is only as current as the last 'repocortex index'.
Install it with: repocortex hooks install
```

Prefer `watch` when you want the index to follow edits rather than commits, and
the hook when you want nothing running in the background.

## Compiler-backed accuracy

### `indexers`

**Answers:** which compiler-backed indexers apply here, and can this machine run
them? Two subcommands.

`indexers list` is pure inspection — it changes nothing and installs nothing.

```
$ repocortex indexers list
Accuracy mode: auto
Toolchain:     go: go version go1.25.13 darwin/amd64

LANGUAGE  INDEXER  STATUS                 PATH
go        scip-go  ready (found on PATH)  .
```

`indexers install` provisions them. Every download is pinned to an exact version
and verified against a recorded digest; a mismatch refuses rather than
installing, and an indexer with no recorded digest is never fetched at all.

| Flag | Effect |
|---|---|
| `--yes` | install without prompting (still verifies digests) |
| `--out-dir DIR` | write the `.scip` files into `DIR` (created if absent, must be empty) instead of a temporary directory, so a script knows where to find them |
| `--allow-repo-build` | permit running code the repository chooses (Java's Maven or Gradle build, C/C++ via `compile_commands.json`, Rust's cargo build scripts and macros). Refused by default; prefer `accuracy.indexers.runner=docker` for Java |

`indexers pins` prints one tab-separated line per indexer this build may
install, with five fields: name, version, channel, the reference it installs,
and the digest it verifies (`-` where the channel verifies itself, as
`go install` does against the checksum database).

```
$ repocortex indexers pins
scip-go	v0.2.7	go-install	github.com/scip-code/scip-go/cmd/scip-go@v0.2.7	-
scip-java	0.12.2	docker	sourcegraph/scip-java@sha256:a05564b9...	sha256:a05564b9...
scip-python	0.6.6	npm	@sourcegraph/scip-python@0.6.6	be8e0a1e...
scip-typescript	0.3.14	npm	@sourcegraph/scip-typescript@0.3.14	92a88678...
```

`scip-clang` and `rust-analyzer` are not in this table: they are installed by you,
not by RepoCortex (see `resolve`), and `indexers list` shows them as `blocked`
and says, when they are missing, that RepoCortex does not install them and the tool's own install line.

(Digests abbreviated here.) The output depends on nothing but the binary: no
repository, config or network is read. That makes it a sound CI cache key: the
key changes exactly when a pin changes, which is how the change-safety workflow
caches `scip-go`.

Which indexers may be installed, and whether installation may happen without
prompting, is controlled by your **user** config (`~/.config/repocortex/config.yml`)
and never by the repository. A cloned repo must not be able to decide what runs
on your machine.

### `scip import`

**Answers:** takes a `.scip` index that already exists — usually from CI — and
writes its relationships in as `exact`/`compiler` facts. Nothing is ever
downgraded, so importing a stale index cannot weaken the graph, and importing
the same index twice is a no-op.

| Flag | Effect |
|---|---|
| `--path-prefix <dir>` | repo-relative directory the index's documents hang off, when the index records absolute build-container paths |
| `--max-bytes N` | cap the index size in bytes (0 = default, negative = no cap) |

Order matters: index, resolve, then import.

```
$ repocortex scip import index.scip
scip-go 0.2.7: 313 document(s), 276 matched
symbols: 4376 matched, 14810 with no counterpart in the index
edges:   15885 new, 0 upgraded from heuristic, 1026 already known
         72729 reference(s) point outside the indexed repository
         3173 reference(s) the indexer attributed to a file rather than a definition were skipped
duration: 2.218s

37 of 313 document(s) matched no indexed file (88% matched).
```

Paths are the usual failure. If the matched-document count is zero, pass
`--path-prefix`. `0 upgraded from heuristic` is expected, not a failure — see
[FAQ.md](../FAQ.md#why-does-scip-import-say-0-upgraded-from-heuristic).

---

## Asking the index questions

All five of these answer the same questions as the MCP tools, against the same
index.

### `search`

**Answers:** which files contain this text? Full-text over the index's FTS5
tables. Identifiers are indexed by their parts, so `callback` matches
`handlePaymentCallback`. This searches **text**; to find a definition by name,
use `symbol`.

| Flag | Effect |
|---|---|
| `-n`, `--limit N` | maximum files to report (default 20) |

```
$ repocortex search callback -n 5
internal/search/tokenize_test.go
internal/compile/compile_test.go
fixtures/payment-platform/src/main/java/com/pay/controller/CallbackController.java
internal/context/task_test.go
internal/cli/final_test.go

5 file(s) matching "callback".
```

Coverage is indexed files only: binary, generated and oversized files are
recorded in the index but carry no searchable text, and secrets are redacted
before storage.

### `symbol`

**Answers:** where is this defined? Exact, not fuzzy — asking for
`TransactionService` returns that type, not the forty symbols whose names
contain it.

| Flag | Effect |
|---|---|
| `--kind <kind>` | restrict to one kind (class, interface, method, function, …) |
| `-n`, `--limit N` | maximum definitions to report (default 50) |

```
$ repocortex symbol Server --kind class
KIND   NAME    LOCATION                   CONFIDENCE
class  Server  internal/mcp/server.go:41  high (source_code)

1 definition(s) named "Server".
```

The confidence shown is the confidence of the **extraction**, so a
compiler-proven definition is distinguishable from a parsed one.

### `references`, `callers`, `callees`

**Answer:** what points at this symbol, what calls it, and what does it call.
`references` is broadest — calls, imports, implementations, extensions and plain
references. `callers` is call edges only. `callees` points away from the symbol.
All three share the same flags.

| Flag | Effect |
|---|---|
| `-n`, `--limit N` | maximum relationships to report (default 100) |
| `--min-confidence exact\|high\|medium\|low` | drop edges below this confidence |
| `--path <substring>` | disambiguate by file path substring |

```
$ repocortex callers Redact
Redact (function) at internal/security/redact.go:67

KIND   SYMBOL       EVIDENCE                          CONFIDENCE
CALLS  Bound        internal/mcp/framing.go:153       high (source_code)
CALLS  RedactFile   internal/security/redact.go:122   high (source_code)
...
17 caller(s).
```

**An empty result is never presented as proof of absence.** It reports the
index's measured resolution coverage instead:

```
$ repocortex callees profileTools
profileTools (function) at internal/mcp/server.go:129

No callees found.

41% of 29506 references are bound to a definition (15381 point outside this
repository, 2063 were too ambiguous to bind). An empty result is a lower bound,
not proof that nothing uses this symbol.
```

### `context`

**Answers:** what would an agent be handed for this task? This is what
`get_task_context` serves over MCP, printed for a human, and it is the fastest
way to see *why* an agent received what it received.

| Flag | Effect |
|---|---|
| `--max-files N` | override the file budget |
| `--max-lines N` | override the excerpt line budget |
| `--max-symbols N` | override the symbol budget |
| `--show-excluded` | list everything considered and left out, with the reason |

```
$ repocortex context "Add secret redaction to MCP responses"
Task: Add secret redaction to MCP responses
Understood as: Concepts: mcp, redaction, responses, secret, add; action: add.
Anchored on 20 symbols named by the task (MCPProfile, MCPMinimal, …); expanded
to 1462 reachable symbols in the call graph

KIND      NAME        LOCATION                        WHY
class     MCPProfile  internal/config/config.go:90    named by the task (matches "mcp"); text also matches …
...
```

Every item carries a non-empty reason it earned its tokens, and `--show-excluded`
prints the rejections with theirs.

---

## Structural surfaces

### `api`

**Answers:** what HTTP endpoints does this service expose, and what handles
each? No flags beyond the global ones.

Routes are discovered from the framework's own spelling — a Spring annotation, a
Go router registration, a FastAPI or Flask decorator, a Django `path()` in a
urlconf, an Express member call, a NestJS controller decorator — never from
strings that merely look like paths. Django routes are reported at `medium`
confidence, since the `include()` mount prefix is not visible from the file.
**Not covered:** OpenAPI documents, GraphQL schemas, gRPC service definitions,
API-gateway configuration.

```
$ repocortex api
METHOD & PATH           HANDLER         DECLARED AT
POST /payment/callback  handleCallback  …/controller/CallbackController.java:21

1 endpoint(s). Discovery covers spring, go-http, go-router, fastapi, flask, django, express, nest, nextjs.
5 further route(s) are declared in test files and are not counted above — they
are fixtures, not this service's API.
```

### `db`, `db table`

**Answer:** which tables does this repository declare or query, and what reads
or writes each. `READ`, `WRITE`, `READ_WRITE`, `DDL` and `UNKNOWN` are kept
distinct — "what touches this table" is a mildly useful question, "what *writes*
it" is the one asked before changing a column. Neither takes flags beyond the
global ones.

Tables nothing here declares are still reported: that is the normal shape of a
service reading a database another service owns.

```
$ repocortex db table transactions
transactions (declared in this repository)

WRITTEN BY (1)
  save (…/TransactionRepository.java)      …TransactionRepository.java:20  high (UPDATE (literal SQL))

READ BY (1)
  findById (…/TransactionRepository.java)  …TransactionRepository.java:14  high (SELECT (literal SQL))
```

### `config`

**Answers:** every configuration key the repository declares or reads, and — the
two findings worth acting on — the mismatches between them. A key read by code
that nothing declares is how a service fails at start-up; a key declared and
never read is either dead configuration or a lookup this scan cannot see.

| Flag | Effect |
|---|---|
| `--all` | list every key, not only the mismatches |

**Values are never read.** Not masked, not truncated — never read at all. Keys
whose *name* indicates a credential are labelled sensitive, which requires
reading nothing. Declarations come from `.env`, YAML, JSON, TOML and
`.properties`; Kubernetes, Helm, Terraform and CI definitions are not read.

```
$ repocortex config
READ BUT NOT DECLARED (16) — a service that needs these and finds nothing fails at start-up
  API_KEY                5 reader(s)  sensitive (by name)
  DATABASE_URL           5 reader(s)
  ...
179 key(s) total: 161 declared and never read, 4 labelled sensitive by name.
Pass --all for the full inventory.
```

### `deps`

**Answers:** what does this project declare it depends on, and does anything
import it? Aliased as `dependencies`.

| Flag | Effect |
|---|---|
| `--all` | list every dependency, not only the unused ones |

Manifests, not lock files — `go.mod`, `package.json`, `pom.xml`, `build.gradle`,
`requirements.txt`, `pyproject.toml`, `Cargo.toml`. **No vulnerability data**:
versions are reported accurately so a real scanner can consume them, but a
scanner shipping an embedded advisory list is stale the day it is built. An
unused dependency is a prompt, not a verdict.

```
$ repocortex deps
DECLARED BUT NOT IMPORTED (1)
  github.com/never/used  v0.1.0  runtime  go.mod:7

  Check before removing: a package can be loaded by reflection, required by a
  build plugin, or imported under a name that differs from its distribution name.

2 dependency(ies) declared. Pass --all for the full list.
```

`deps` itself carries no advisory data. [`sbom`](#sbom) writes the same
declarations for a scanner, and [`vulns`](#vulns) checks them against advisories
you supply.

### `sbom`

**Answers:** what does this project declare it depends on, in a form another
tool reads? Writes a CycloneDX 1.5 JSON document, and nothing else, on stdout.

| Flag | Effect |
|---|---|
| `--timestamp <RFC 3339>` | write this time as `metadata.timestamp` (default: none) |
| `--name <name>` | name of the project described (default: the root manifest's own module or package name) |

```
$ repocortex sbom > sbom.cdx.json
$ repocortex sbom -q | head -5
{
  "$schema": "http://cyclonedx.org/schema/bom-1.5.schema.json",
  "bomFormat": "CycloneDX",
  "specVersion": "1.5",
  "version": 1,
```

Each component has a package URL for its ecosystem and the version exactly as
the manifest or a lockfile states it. The property `repocortex:versionState`
says how much is known:

| State | Meaning | `version` | `purl` |
|---|---|---|---|
| `pinned` | the manifest states one exact version | that version | with version |
| `locked` | the manifest gives a range; a lockfile beside it states the version | the lockfile's version | with version |
| `range` | declared as a range, and no lockfile pins it | the range as written | no version |
| `unpinned` | no constraint, or one managed elsewhere (a parent POM, a BOM) | as written, or absent | no version |
| `unresolved` | a property placeholder, a Git or path source, a local `replace` | as written | no version |

A range is never written into a package URL: `pkg:npm/lodash@^4.17.20` names a
release that does not exist. The other properties are `repocortex:declaredVersion`,
`repocortex:versionSource` (the manifest line or the lockfile), `repocortex:declaredIn`,
`repocortex:ecosystem`, `repocortex:scope`, `repocortex:indirect`,
`repocortex:declaredName` (under a Go `replace` or a Cargo rename) and
`repocortex:note`.

| Ecosystem | Manifest | Lockfile read for the version | Package URL |
|---|---|---|---|
| Go | `go.mod` (with `replace`) | none needed | `pkg:golang/<module>@<version>` |
| npm | `package.json` | `package-lock.json`, `npm-shrinkwrap.json`, `yarn.lock` | `pkg:npm/<name>@<version>` |
| PyPI | `requirements.txt`, `requirements-dev.txt`, `pyproject.toml` (`[project] dependencies`) | `poetry.lock`, `uv.lock` | `pkg:pypi/<name>@<version>` |
| Cargo | `Cargo.toml` | `Cargo.lock` | `pkg:cargo/<name>@<version>` |
| Maven | `pom.xml` (properties of the same POM resolved) | none | `pkg:maven/<group>/<artifact>@<version>` |
| Gradle | `build.gradle`, `build.gradle.kts` | `gradle.lockfile` | `pkg:maven/<group>/<artifact>@<version>` |

The output is deterministic — sorted, no serial number, no timestamp unless
`--timestamp` sets one, no tool version — so the file can be committed and its
diff is a diff of the dependencies.

**Declared dependencies only.** A package that appears only in a lockfile (the
transitive closure) is not listed, and the document says so itself
(`compositions: incomplete`). `pnpm-lock.yaml`, `Pipfile.lock` and Poetry's
`[tool.poetry.dependencies]` table are not read. Lockfiles are read from the
working tree; the declarations come from the index. A summary, and a warning
when components have no single version, go to the log on stderr.

### `vulns`

**Answers:** given advisories I supply, which declared dependencies are in an
affected range, and does the indexed code use them?

| Flag | Effect |
|---|---|
| `--advisories <path>` | OSV JSON file, or directory of them (repeatable) |
| `--fail-on <level>` | exit 1 when a finding at this reachability or stronger exists: `referenced`, `imported`, `affected` |
| `--ignore <id>` | leave out an advisory by id or alias (repeatable); the report lists what was ignored |
| `--all` | list the not-affected comparisons and every importing file |
| `-n`, `--limit <n>` | importing files and symbol references shown per finding (default 5) |

**No network, no built-in advisories.** The command never downloads a
vulnerability database and ships none. Advisories are
[OSV](https://ossf.github.io/osv-schema/) records in local files: one record, a
JSON array of records, an OSV API response, or `osv-scanner`'s JSON report.
Without `--advisories` it checks nothing, says where advisories come from, and
exits 0.

```
$ repocortex vulns --advisories used-cobra.json --advisories unused-gin.json
2 advisory record(s) from 2 file(s) checked against 75 declared dependency(ies)

AFFECTED (1)
  github.com/spf13/cobra v1.10.2  [go]  go.mod:13  HAND-2026-0001 (CVE-0000-0001)
    Hand-written test advisory: cobra argument validation (not a real vulnerability)
    version: v1.10.2 is in a range affected from the first release and before the fix 1.11.0
    fixed in: 1.11.0
    use: REFERENCED — 16 qualified reference(s) to a symbol the advisory names; 1 named method(s) were not checked (Command.ExecuteContext)
      internal/cli/apidb.go:241  cobra.ExactArgs in a file that imports github.com/spf13/cobra
      internal/cli/bench.go:49  cobra.ExactArgs in a file that imports github.com/spf13/cobra
      internal/cli/events.go:78  cobra.ExactArgs in a file that imports github.com/spf13/cobra
      internal/cli/planchange.go:33  cobra.ExactArgs in a file that imports github.com/spf13/cobra
      internal/cli/prune.go:26  cobra.ExactArgs in a file that imports github.com/spf13/cobra
      ... and 11 more (--all, or --json)
    imported by 52 file(s):
      internal/cli/accuracy.go:10  github.com/spf13/cobra
      internal/cli/accuracy_test.go:12  github.com/spf13/cobra
      internal/cli/agentsetup.go:14  github.com/spf13/cobra
      internal/cli/agentsetup_test.go:19  github.com/spf13/cobra
      internal/cli/apidb.go:15  github.com/spf13/cobra
      ... and 47 more (--all, or --json)

1 of 2 advisory record(s) name no DECLARED dependency. That is not the same as not present: a lockfile-only package is not examined.

What this report covers
  go: 75 declared, 75 with a concrete version; ordering: semver 2.0.0
    "not imported" is claimed: yes: a Go file must import a package to use it, and the import path names the module
    symbols: package-level functions called as pkg.Func, where the advisory lists symbols (the Go vulnerability database does); methods are not bound
    go: 698 file(s), imports recorded, 0 not fully parsed

Advisories: 2 record(s) used; skipped 0 withdrawn, 0 duplicate, 0 JSON file(s) with no OSV record
```

The report closes with three standing caveats, omitted above: only declared
dependencies were checked; imports and references are those of first-party
indexed code; and "imported, symbol use unknown" is not a finding that the
vulnerable code is unused.

**Version status.** `affected`: compared, and inside an affected range.
`could_not_compare`: the comparison could not be made, and this is never
reported as unaffected — a range with no lockfile, a placeholder, a Git commit
range, a version the ecosystem's ordering cannot read. `not_affected`: every
range was compared and the version is outside all of them (counted; `--all` and
`--json` list each comparison).

| Ecosystem | Ordering |
|---|---|
| npm, Go, Cargo | semver 2.0.0 |
| PyPI | PEP 440 |
| Maven, Gradle | Maven `ComparableVersion`; a dotted qualifier against a hyphenated one on the same release (`1.0.0.RC1` vs `1.0.0-RC2`) is refused, because Maven 3.9 reversed it |

**Reachability**, for each affected or uncompared dependency:

| State | Meaning |
|---|---|
| `referenced` | a file that imports the package references a symbol the advisory names |
| `imported_symbol_use_unknown` | imported; the advisory names no symbol, or the index could not bind one. Not a clearance |
| `not_imported` | no indexed file imports it |
| `import_use_unknown` | no import matched, where that shows nothing: an ecosystem whose package names do not determine import paths, or an index holding no source file in a language that could import the package |

`not_imported` is said only where a package name determines its import path:

| Ecosystem | Import evidence | `not_imported` claimed | Symbols bound |
|---|---|---|---|
| Go | import statements; an import belongs to the longest declared module path | yes | `pkg.Func` calls, from `ecosystem_specific.imports` or `affected_functions`; an advisory narrowed to packages is checked per package. Methods are not bound |
| npm | static `import` / `export … from`, and `require("x")` read from the indexed line. Dynamic `import()` is not recorded | yes | none |
| PyPI | import statements, by module name | only for about 50 well-known distributions whose module names are built in (`PyYAML` → `yaml`) | module-level functions from `affected_functions`. Methods are not bound |
| Cargo | `use` declarations | no: a crate can be used by path without `use` | a `use` of the affected function itself |
| Maven, Gradle | imports under the `groupId`, plus a few known package prefixes | no: a coordinate does not name its Java packages | none |

Every report ends with its coverage: per ecosystem, the languages whose imports
the index records, the source files that were not fully parsed, and the
standing limits. **Only declared dependencies are checked** — a vulnerable
package that appears only in a lockfile is not examined, and Go standard-library
advisories are counted and not evaluated (`govulncheck` covers both for Go;
`osv-scanner` reads lockfiles). One exception is reported rather than dropped:
a Go or npm package an advisory names that no manifest declares but the indexed
code imports anyway (a transitive dependency used directly). It has no declared
version, so it is always `could_not_compare`, shown as `NOT DECLARED in any
manifest`.

**`--fail-on`** follows [`blindspots --fail-on`](#missed). A dependency whose
version could not be compared fails the gate exactly as an affected one does,
`import_use_unknown` counts as imported, and an advisory file that could not be
read fails every level. `--fail-on` without `--advisories` is an error: a gate
with nothing to check must not pass.

### `arch`

**Answers:** does the code match the architecture you declared? Compares the
dependency **graph** against components and rules in `.repocortex.yml`. Components
are declared, never inferred.

| Flag | Effect |
|---|---|
| `--crossings` | list every component-to-component dependency, not only violations |
| `--fail-on high\|medium\|low` | **exit 1** when a violation of this severity or worse exists |
| `--min-confidence exact\|high\|medium\|low` | ignore relationships below this confidence |

```
$ repocortex arch --fail-on high
VIOLATIONS (113)
  SEVERITY  RULE                           CROSSING                              CONFIDENCE
  high      cli-goes-through-the-registry  cli -> store (symbolName -> symbols)  high
  ...
error: 113 architecture violation(s), worst severity "high" (--fail-on high)
$ echo $?
1
```

The report always states how many symbols belong to no declared component and
were therefore not checked. The confidence column is the confidence of the
*edge*, not of the rule: `--min-confidence exact` narrows the same run above
from 113 violations to 42.

### `arch adr`

**Answers:** what decisions are on record, and why? Reads `docs/adr/`,
`docs/decisions/` and the other conventional locations, in both the Nygard and
MADR formats. Takes an optional number or substring; no flags beyond the global
ones.

RepoCortex does not check whether an ADR is still **true**. A record saying the
system uses Postgres is evidence of a decision, not of the current state — the
graph is where you check the current state.

---

### `infra`

**Answers:** which service does this code run in, and what restarts when a
configuration key changes? Read from Dockerfiles, Compose files, Kubernetes
manifests, Helm charts and Terraform.

| Flag | Effect |
|---|---|
| `--for PATH` | which deployment unit does this repo-relative file run in |
| `--key NAME` | what does changing this configuration key name restart |
| `--keys` | list every configuration key name each unit sets |
| `--format F` | restrict to `dockerfile`, `compose`, `kubernetes`, `helm` or `terraform` |
| `--disk` | scan the working tree instead of the index; works before `repocortex index` |

**Values are never printed.** These files hold credentials, and the guarantee is
structural rather than a filter: the extractor has no field a value fits in, and
a Kubernetes `Secret` is read as the key names it declares. What you get is
"`DB_PASSWORD`, from Secret `db-creds`", which is the half you can act on.

Manifests are stored by `repocortex index`, so a service is a node in the same
graph as the code and a key name binds to the code that reads it; `resolve`
joins a service to the endpoints built from its build context. `--disk` reads
the tree directly instead, and the output says which of the two it is reading,
because they can disagree.

What could not be read is reported: a manifest that fails to parse is listed
with the reason, and Helm templates are reported as unrendered rather than
guessed at. Terraform is read as block headers only, never attribute values.
**Not read:** CloudFormation, systemd units, CI definitions.

```
$ repocortex infra
The index holds no deployment manifests. If you expected some, re-run 'repocortex index';
to check the working tree directly, run 'repocortex infra --disk'.

Read: Dockerfile, docker-compose.yml, Kubernetes manifests, Chart.yaml and values.yaml,
and .tf files. CloudFormation, systemd units and CI definitions are not read — that is a
coverage limit, not a finding about your deployment.
```

## Change-safety and context tools

These run against the committed index. Each takes the global `--json`.

### `events`, `events topic`

**Answers:** which topics, queues and exchanges exist, and who publishes to or
consumes from them?

`repocortex events` lists them across Kafka, RabbitMQ/AMQP, SQS, SNS, Google
Cloud Pub/Sub and ActiveMQ (STOMP/JMS). `repocortex events topic <name>` lists the functions that publish
to or consume from one. Event topology is not an MCP tool.

### `missed`

**Answers:** where is this index blind, or where is the code hiding risk?

| Flag | Effect |
|---|---|
| `--kind <kind>` | one of `unresolved_call`, `untested_critical`, `unmapped_endpoint`, `orphan_table`, `missing_config`, `unparsed_file` |
| `--limit <n>` | blindspots reported per category (default 50) |
| `--fail-on <severity>` | exit 1 when a blindspot of this severity (`high`, `medium`, `low`) or worse exists |

MCP equivalent: `list_blindspots`.

### `plan-change <symbol>`

**Answers:** what is the ordered, checkable sequence of steps to change this
symbol safely? Output carries the target, direct callers, risk, covering tests
and pre/post-conditions per step.

The callers are the ones the index bound. When references spelled like the
symbol were left unbound, or its language yields no references, the risk level
is `UNKNOWN` with the counts and no score, in place of `LOW`. `MODERATE` and
`HIGH` are printed as "or higher", because a floor survives missing callers.

| Flag | Effect |
|---|---|
| `--min-confidence <tier>` | lowest edge confidence to walk: `exact`, `high`, `medium` (default), `low` |

MCP equivalent: `plan_change`.

### `rename <old> <new>`

**Answers:** can this symbol be renamed at its definition and at every
reference RepoCortex resolved, and is every one of those sites safe to edit?
A dry run by default: it lists every site with its confidence and says whether
the rename could be applied. Nothing is written without `--apply`.

`--apply` writes only if **every** site passes:

1. its confidence is at or above the floor: `exact` (a compiler-proven edge) by
   default, or `high` with `--floor high`;
2. the file is byte-for-byte what was indexed (otherwise re-index);
3. the old name occurs on the site's line exactly once, or the recorded column
   lands on it exactly;
4. the file is inside the project root, and is a regular, writable file (a
   read-only file is the owner saying "do not touch", and is refused);
5. every reference was fetched, and the new name is not already defined in a
   file being edited.

If any site fails, nothing is written, every failing site is listed, and the
exit code is 2. Files are written atomically (write, then rename), contained to
the project root, with the hash re-checked immediately before each replace. If
a later write fails, the files already written are restored. Only one edit runs
per project root at a time: a `.repocortex-edit.lock` file at the project root
exists only while a commit is running, and a second `rename --apply` that finds
it is refused. A lock left behind by a crashed process is **never** stolen; the
error names the lock's holder and its path, and once you have confirmed no
repocortex process is running, delete it by hand and retry. It exists only
during a commit, so there is no need to ignore it in `.gitignore`, though it can
appear briefly in `git status`.

After applying, a task contract for the rename is written to
`.repocortex/rename-contract.json` and [`verify`](#verify) runs on the changed
files. **Verify reads the pre-rename index**: it checks the change against what
was indexed before the edit. Run `repocortex index` afterwards to refresh it.

| Flag | Effect |
|---|---|
| `--apply` | write the edits (default is a dry run) |
| `--floor exact\|high` | weakest confidence an edit may rest on (default `exact`). `high` accepts an explicit import plus an unambiguous receiver type, measured 86.8-96.1% precise, and prints a warning saying so before applying |
| `--path <substr>` | substring of the defining file's path, when several definitions share the name |
| `--max-sites N` | refuse renames with more references than this (default 2000) |
| `--contract-out FILE` | where to write the rename's task contract (default `.repocortex/rename-contract.json`) |
| global `--json` | machine-readable report, including `leftovers` |

| Exit | Meaning |
|---|---|
| 0 | dry run: would apply. Apply: written, and `verify` passed or advised, and no occurrence of the old name remains |
| 1 | error, `verify` returned `fail`, or a rollback after a failed write was incomplete |
| 2 | refused: nothing was written (a site below the floor, an unchanged-file or ambiguity failure, a locked project root, a read-only file) |
| 3 | applied, but **not verified complete**: `verify` was `unverified`, or `LEFTOVER OCCURRENCES` of the old name remain in indexed files |

**Limits.** RepoCortex never compiles your code. A reference it could not bind,
and interface implementations or overrides that have no edge to the symbol, are
not listed and **not renamed**; comments and strings that mention the old name
are listed as leftovers but left alone. Every run prints this as a
`NOT COMPLETE` note. Search for the old name, then build and test.

A dry run on a copy of `fixtures/payment-platform` (Java, so no compiler index
exists and every site is `high`, below the default `exact` floor):

```
$ repocortex rename updateStatus applyStatus
Rename updateStatus -> applyStatus  (src/main/java/com/pay/service/TransactionService.java:updateStatus, src/main/java/com/pay/service/TransactionService.java:18)
REFUSED: nothing was written. Every site must be exact or better, unambiguous on its line, and unchanged since indexing.
  High sites need a compiler-backed index to become exact ('repocortex indexers install', then 'repocortex scip import'), or an explicit --floor high (measured 86.8-96.1% precise).

SITES
  high  src/main/java/com/pay/controller/CallbackController.java:23  CALLS
  high  src/main/java/com/pay/service/TransactionService.java:18     DEFINITION

REFUSED SITES
  high    src/main/java/com/pay/controller/CallbackController.java:23  confidence high is below exact
  high    src/main/java/com/pay/service/TransactionService.java:18  confidence high is below exact

NOT COMPLETE: RepoCortex renames the definition and the references it resolved, nothing else, ...
$ echo $?
2
```

(The `NOT COMPLETE` note is truncated here; the tool prints it in full.) With
the explicit opt-in to `high`, on the same copy:

```
$ repocortex rename updateStatus applyStatus --floor high --apply
WARNING --floor high: `high` bindings measure 86.8-96.1% precision against compiler indexes (docs/accuracy.md); at 90%, a rename touching thirty sites gets three wrong. Read every high site below, then build and test.
Rename updateStatus -> applyStatus  (src/main/java/com/pay/service/TransactionService.java:updateStatus, src/main/java/com/pay/service/TransactionService.java:18)
APPLIED to 2 file(s), but NOT VERIFIED COMPLETE: the old name is still in the files listed under LEFTOVER OCCURRENCES.

SITES
  high  src/main/java/com/pay/controller/CallbackController.java:23  CALLS
  high  src/main/java/com/pay/service/TransactionService.java:18     DEFINITION

LEFTOVER OCCURRENCES (2 line(s) still name updateStatus; showing 2)
  References RepoCortex could not bind, or comments and strings. Nothing here was changed; review each by hand.
  src/test/java/com/pay/controller/CallbackControllerTest.java:8  verify(transactionService).updateStatus("txn-1", "COMPLETED");
  src/test/java/com/pay/service/TransactionServiceTest.java:7  service.updateStatus("txn-1", "COMPLETED");

VERIFY: PASS  (contract: /tmp/rn/.repocortex/rename-contract.json)
  [UNKNOWN] The repository declares no architecture, so there was no rule for this change to break. ...
  [UNKNOWN] The contract names no covering test, so for the 1 changed scope symbol(s) there is nothing to check coverage against. ...
  Checked against the pre-rename index; run 'repocortex index' to refresh it.

NOT COMPLETE: RepoCortex renames the definition and the references it resolved, nothing else, ...
$ echo $?
3
```

Here the two test files call the method through a mock and a field that
RepoCortex did not bind, so the old name survives; the exit code is 3, never 0,
until you handle them by hand. Verify passing does not make a rename complete.

MCP equivalent: none. The MCP server only plans (`plan_rename`, `plan_change`); it never edits.

### `move <symbol> <from-file> <to-file>`

**Answers:** can this Go declaration be moved to another file of its package,
and exactly what would change in the two files? It moves one top-level
declaration (a function, type, constant or variable, with its doc comment) and
corrects the import lists of both files: the imports the moved code uses are
added to the target, and the ones nothing else uses are removed from the
source. A method is named `Type.Method`. A dry run by default: nothing is
written without `--apply`.

Inside a package a move changes no reference, so the whole edit is syntax, and
it is read with the Go standard parser from the files as they are now. Paths
are relative to the project root.

`--apply` writes only if **all** of this holds:

1. both files are Go files in one directory and one package, built under the
   same constraints (the same `//go:build` line and the same `_test`,
   `_GOOS`, `_GOARCH` file-name suffix), and neither is generated or a cgo
   file. A target that does not exist is created, with the source's package
   clause and `//go:build` line;
2. both files are byte-for-byte what was indexed (otherwise re-index), inside
   the project root, regular, writable, and not reached through a symlink;
3. the name is declared exactly once in the source file, on lines of its own,
   and the target does not already declare it;
4. every name the moved code writes before a dot is accounted for: a local, a
   name the package declares, or an import whose package name is **known** —
   written in the file, a standard-library path, or reported by `go list`. The
   name of a package outside the standard library is never guessed from its
   import path;
5. the move cannot change what the program does at start-up (below);
6. with `go` on PATH, the package builds with the planned files in place.

If anything fails, nothing is written, every reason is listed, and the exit
code is 2.

**What is refused, and why**

| Refused | Why |
|---|---|
| any language but Go | there is no exact way here to cut a declaration out of another language's file |
| a target in another directory, or with another `package` clause | every reference would need a new qualifier and import, and unexported names would stop resolving |
| a method on its bare name; a method leaving its receiver type's package | a method moves only as `Type.Method`, and only inside the package |
| `init` | init functions run in the order of the files that hold them |
| a member of a constant group that uses `iota` or leaves a value out | the other members' values depend on its position |
| a variable with an initialiser, in a package where any initialiser runs code | Go initialises package variables in declaration order, files taken by name; moving one changes that order, which is invisible only when no initialiser calls a function |
| files with different build constraints, a `_test.go` file on one side only, an old-style `// +build` line with no `//go:build` | the declaration would be compiled in different builds than before, and one machine's build cannot show it |
| generated files, cgo files, files named `_x.go` or `.x.go` | the generator would undo the move; a cgo preamble binds declarations to their file; the go command ignores those names |
| a name declared more than once in the file, or in one statement with other names (`var a, b = 1, 2`) | the name does not say what to move |
| a declaration that shares a line with other code | it cannot be cut out by lines |
| a `//go:embed`, `//go:linkname`, `//go:generate` or `//export` directive on the declaration | each ties it to its file in a way syntax does not show |
| the target already declares the name, imports the same package under another name, or uses the import's name for a different package | the result would not compile, or would mean something else |
| the source dot-imports a package the moved code may draw on | what a dot-import supplies is not syntax |
| an import whose package name is unknown (no `go` on PATH, or the module is not downloaded) and that the moved code may use | give the import an explicit name, or make the toolchain available |
| a package that does not build before the move | no build could then tell a break from what was already broken; `--no-build` accepts a result checked by parsing only |
| a file changed since indexing, not in the index, read-only, over 8 MiB, or a file of the package that cannot be read safely | as for [`rename`](#rename-old-new) |

**How it is written and checked.** Before anything is written the two new
files are scanned back into tokens and compared with the old ones: the source
must be what it was less the cut lines, the target what it was plus the
declaration, with no comment lost and no import changed beyond the plan. Then,
with `go` on PATH, the package is compiled with the planned files substituted
through `go build -overlay` (`go test -c` when the files are test files), still
without writing. A second build, with the planned source made uncompilable,
must then fail: that is what shows the first one compiled the moved code. Where
it does not fail — the file's build constraints exclude it on this machine —
the move is reported as not build-checked, never as passing. `go` runs with
`GOPROXY=off`: nothing is downloaded.

`--apply` then writes the target, then the source, each atomically, under the
same `.repocortex-edit.lock` as `rename`. A new file is hard-linked into
place, which fails rather than replace a file that has appeared since the plan
(on a filesystem without hard links it is renamed into place after a last
check that the name is free). If the second write fails the first is undone. The package is built once more from the files on disk, and both files
are restored if that build fails. The index is not updated: run
`repocortex index` afterwards.

Each site carries a confidence. It is `exact` only when a compiler checked the
result; without a build the sites are `high`, located by the Go parser and
compiled by nobody.

| Flag | Effect |
|---|---|
| `--apply` | write the edits (default is a dry run) |
| `--no-build` | do not compile the result; it is checked by parsing only, sites are `high`, and an apply exits 3 |
| global `--json` | machine-readable report: `declaration`, `sites`, `refusals`, `build`, `notes`, `limits` |

| Exit | Meaning |
|---|---|
| 0 | dry run: would apply. Apply: written, and the package built from the files on disk |
| 1 | error, or a rollback that could not restore a file |
| 2 | refused: nothing was written, or everything written was restored |
| 3 | applied, but **no build checked it**: `--no-build`, no `go` on PATH, or files this platform does not build |

**Limits.** One declaration per run. A type's methods stay where they are;
move each with `Type.Method`. A new file gets no licence or header comment.
The declaration is appended to the target, not placed among related
declarations. A file not gofmt-formatted is edited in place and left as it
was; a formatted one stays formatted. A declaration taken out of a group is
always written in gofmt's form, its doc comment included (an empty last `//`
line goes, as for any top-level doc comment). The build check compiles for
this machine's platform only. Moving to another package is not supported.

A dry run, an apply and a refusal on a small module:

```
$ repocortex move Normalize config/load.go config/keys.go
Move func Normalize: config/load.go -> config/keys.go  (config/load.go:27-32)
DRY RUN: nothing was written. Re-run with --apply to make these edits.

SITES
  exact  config/keys.go:1      create-file    new file, package config
  exact  config/keys.go:3      add-import     import "strings", which the moved code uses
  exact  config/keys.go:5-10   insert         func Normalize with its doc comment
  exact  config/load.go:27-32  remove         func Normalize with its doc comment
  exact  config/load.go:8      remove-import  import "strings", which nothing else in this file uses

BUILD: PASS  go build .
  compiled with the planned files in place of the two on disk (go's -overlay); nothing was written for it

NOTES
  config/keys.go is a new file: it gets the package clause, the build constraint and the imports the declaration needs, and no licence or header comment. Add one if this repository requires it.

LIMITS: move relocates one declaration between two files of one Go package and corrects both import lists. ...
$ echo $?
0
$ repocortex move Normalize config/load.go config/keys.go --apply
Move func Normalize: config/load.go -> config/keys.go  (config/load.go:27-32)
APPLIED to 2 file(s).

SITES
  exact  config/keys.go:1      create-file    new file, package config
  exact  config/keys.go:3      add-import     import "strings", which the moved code uses
  exact  config/keys.go:5-10   insert         func Normalize with its doc comment
  exact  config/load.go:27-32  remove         func Normalize with its doc comment
  exact  config/load.go:8      remove-import  import "strings", which nothing else in this file uses

BUILD: PASS  go build .
  built from the files on disk after writing
...
$ echo $?
0
$ repocortex move FromFile config/kinds.go config/keys.go
Move FromFile: config/kinds.go -> config/keys.go
REFUSED: nothing was written.

REASONS
  config/kinds.go:8  FromFile is part of a constant group in which iota numbers the members, so taking a member out would change the values of the others. Move the whole group by hand
...
$ echo $?
2
```

(The `LIMITS` paragraph is truncated here; the tool prints it in full.)

MCP equivalent: none. The MCP server only plans; it never edits.

### `prune <file>`

**Answers:** what is this file's interface, without its implementation? Bodies
are replaced by a placeholder; signatures, types and docstrings stay.

| Flag | Effect |
|---|---|
| `--no-docstrings` | strip docstrings as well as bodies |
| `--placeholder <text>` | text left in stripped bodies (default `/* ... */`) |

MCP equivalent: `get_file_outline`.

### `diff-minimize [patch-file]`

**Answers:** what does this diff change, in fewer tokens? Reads a patch file or
stdin, strips redundant headers, names enclosing symbols and compresses long
unchanged runs.

```
git diff HEAD~1 | repocortex minimize-diff
```

MCP equivalent: `minimize_diff`.

## The loop: compile → work → verify

### `compile`

**Answers:** what brief should an agent have been given for this one-line ask?
Scope, non-scope, constraints, blast radius, tests, acceptance, and what it does
not know.

| Flag | Effect |
|---|---|
| `--max-scope N` | bound the scope list (0 = default) |

Everything is derived, nothing is invented: every constraint and risk carries the
file or symbol it came from, and a constraint with no evidence is dropped rather
than emitted. No model, no network, no clock — the same intent against the same
index produces a byte-identical contract.

```
$ repocortex compile "Add secret redaction to MCP responses"
# Task Contract

## Intent
Add secret redaction to MCP responses

## Interpretation
Concepts: mcp, redaction, responses, secret, add; action: add. …

## Scope
- `MCPProfile` in `internal/config/config.go` (class) — named by the task …

## Out of scope
- `Declarations` in `internal/analyze/config/config.go` — relevant (1.79) but
  dropped to stay inside the ~8000-token budget
…
```

Pipe `--json` into `verify`.

### `verify`

**Answers:** did the change honour the contract? Four questions — scope,
architecture rules, missed callers, test coverage — and it refuses to ask a
fifth. It is not a linter, a test runner or a reviewer.

| Flag | Effect |
|---|---|
| `--contract <path>` | contract from `compile --json` (**required**) |
| `--changed <a,b,c>` | comma-separated changed file paths |
| `--changed-from <file>` | file holding one changed path per line (`-` for stdin) |
| `--fail-on-leads` | exit non-zero when only leads were found |
| `--min-confidence exact\|high\|medium\|low` | raise the contract's confidence floor |

One of `--changed` or `--changed-from` is required. `verify` does not run git:
the changed set is an **input**, because the situation that matters is an agent
asking "is what I have in my working tree acceptable" before it writes anything.

**The exit codes are the command.**

| Code | Disposition | Meaning |
|---|---|---|
| 0 | `pass` | every question was asked, nothing was found |
| 0 | `advisory` | findings exist, all of them leads (`--fail-on-leads` makes this 1) |
| 1 | `fail` | at least one finding is a FACT |
| 2 | `unverified` | there was nothing to check against |

A finding is a FACT when the edge behind it is compiler-proven, and a LEAD when
it rests on a heuristic edge — because the edge itself may be wrong. Presenting
the two identically makes the report either alarmist or ignorable, and both end
with it switched off.

`unverified` is deliberately not `pass`. "We looked and found nothing" and
"there was nothing to look at" must not render the same, or the gate stops
gating on the day contract generation breaks and nobody notices. All four,
observed:

```
$ repocortex verify --contract contract.json --changed internal/cli/opendb.go
VERIFY: FAIL
[FACT] internal/cli/opendb.go violates architecture rule cli-goes-through-the-registry:
       openReadOnly (cli) depends on CheckVersion (store).
  evidence: internal/cli/opendb.go:48 — openReadOnly REFERENCES CheckVersion, exact
            confidence; rule cli-goes-through-the-registry, verdict forbidden
  scip: scip-go 0.2.7
$ echo $?
1

$ repocortex verify --contract contract.json --changed src/main/java/com/pay/service/TransactionService.java
VERIFY: ADVISORY
[LEAD] …/TransactionService.java violates architecture rule services-do-not-touch-the-database:
       findTransaction (service) calls findById (repository).
  evidence: …/TransactionService.java:26 — findTransaction CALLS findById, high
            confidence; rule services-do-not-touch-the-database, verdict forbidden
  receiver type matches
$ echo $?
0
$ repocortex verify --contract contract.json --changed src/…/TransactionService.java --fail-on-leads
$ echo $?
1

$ repocortex verify --contract contract.json --changed src/main/java/com/pay/model/Transaction.java
VERIFY: PASS
No findings.
$ echo $?
0

$ repocortex verify --contract hollow.json --changed README.md
VERIFY: UNVERIFIED
No findings.
$ echo $?
2
```

Every report also prints what it could **not** check, under `NOT CHECKED` and
`UNKNOWN`, rather than presenting a narrow clean result as a broad one.

---

## Reporting and measurement

### `diff`

**Answers:** what changed about the code's **facts** between two indexed runs —
symbols added, removed, moved or orphaned; relationships added, removed or
re-graded; endpoints whose handler changed; tables that gained or lost a reader
or a writer.

| Flag | Effect |
|---|---|
| `--from <ref>` | base (older) side: a run id, or a commit SHA recorded on an indexed run |
| `--to <ref>` | head (newer) side (default: the committed run) |
| `--from-index <path>` | read the base side from this index database |
| `--to-index <path>` | read the head side from this index database |
| `--max-per-section N` | cap the changes reported per section (0 = default) |
| `--max-evidence-sites N` | cap the evidence sites carried per edge fact (0 = default) |

Nothing here invokes git. A ref matching no indexed run fails and says so — it
never falls back to whichever run happens to be committed. And committing a run
retires the one before it, so **one index database holds one committed run**;
comparing across commits normally means two index files, which is what
`--from-index`/`--to-index` are for.

```
$ repocortex diff --from-index /tmp/paybase/index.db --from 2 --to 3
diff 8e6705118c97…..8e6705118c97… (runs 2..3)
  base: status=ready accuracy=auto digest=3f5cf0b410ea
  head: status=ready accuracy=auto digest=03dbf1aa57cd

SYMBOLS
  moved    …/TransactionService.java:TransactionService (class)  line 6 -> 6 (+0)
  added    …/TransactionService.java:refund (method)  line 29

EDGES
  added    …:refund (method) -[CALLS]-> …/TransactionRepository.java:findById (method)
           high/source_code at …/TransactionService.java:30:24
  added    …:refund (method) -[CALLS]-> …/TransactionRepository.java:save (method)
           high/source_code at …/TransactionService.java:30:8

COULD NOT COMPARE
  - 1 files did not parse cleanly in one of the runs; changes inside them are
    marked uncertain
```

The report always ends with what it could not compare. A degraded run, a file
that did not parse, or two runs indexed in different accuracy modes all mean an
absence may be an indexing gap rather than a deletion.

### `docs`

**Answers:** generates `ARCHITECTURE.md` and `CAPABILITIES.md` from the indexed
graph, with Mermaid diagrams. Every claim cites the file and line it came from;
where the graph is silent, the document says so instead of guessing.

| Flag | Effect |
|---|---|
| `--out <dir>` | write the two files there (default: stdout) |
| `--check` | regenerate in memory and compare; **exit 1** on drift |
| `--max-components N` / `--max-depth N` / `--max-endpoints N` | caps (0 = default) |

Output is deterministic: the same index regenerates byte-identical files, so
these can be committed and reviewed like any other artifact. `--check` is the
useful one in CI.

```
$ repocortex docs --out /tmp/paydocs
wrote /tmp/paydocs/ARCHITECTURE.md
wrote /tmp/paydocs/CAPABILITIES.md

$ repocortex docs --check --out /tmp/paydocs
documentation is current (2 file(s) checked).
$ echo $?
0
```

### `bench`

**Answers:** did the context engine retrieve the right code? Runs hand-written
cases through the engine and scores the pack it actually produces.

| Flag | Effect |
|---|---|
| `--fail-on-regression` | **exit 1** when any case fails, for CI |

A case names a task, the code a correct answer must contain (`expect`), and the
code it must not (`forbid`). The second half is what makes the number mean
something — recall alone is trivially gamed by returning the whole repository.

```
$ repocortex bench fixtures/bench/payment.yml
CASE                              RESULT  RECALL  TOKENS  MISSING
change-transaction-status-column  pass    100%    1567    —
idempotent-callback               pass    100%    1368    —
notification-failure-handling     pass    100%    1388    —

3/3 passed, mean recall 100%.
```

**What this does not measure:** whether an agent completed the task. Only whether
the context contained what a human said it should.

### `trace`

**Answers:** which indexed code did production actually reach? Takes an exported
OpenTelemetry or Jaeger file — a **file**, not a live collector — and lines it up
against the graph. No flags beyond the global ones.

Correlation is heuristic and every match says how it was made: a `code.function`
attribute names the symbol outright, an `http.route` matching an endpoint is
strong, a span name matching a symbol is weak and is only tried when nothing
better exists and the name is unique.

```
$ repocortex trace spans.json
otlp-json: 3 span(s) across 1 trace(s), service(s): pay

EXERCISED (3)
  ENTITY                  KIND      CALLS  ERRORS  P95   MATCHED BY
  POST /payment/callback  endpoint  1      0       40ms  route
  transactions            table     1      0       15ms  table
  updateStatus            method    1      0       25ms  code-attribute
```

Spans are **not persisted**. Runtime data ages out in hours while an index
describes a commit, and storing them together would produce a graph whose edges
are true at different times.

---

### `impact`

**Answers:** what does a git range put at risk? Reads the files the range
changed, finds the symbols defined in them, and walks *inward* to everything
that depends on each.

| Flag | Effect |
|---|---|
| `--base REV` | the revision to compare from (required) |
| `--head REV` | the revision to compare to (default `HEAD`) |
| `--depth N` | hops to follow from each changed symbol |
| `--min-confidence` | floor for the walk (default `medium`) |
| `--max-seeds N` | maximum changed symbols to analyse |
| `--limit N` | rows per section |

The direction is the point: a change threatens the code that depends on it,
not the code it calls, so this walks callers. Renamed and deleted files are
seeded under the name the index has for them, since those are the two changes
most likely to break a caller and the ones a naive diff would report as empty.

Nothing here checks out or re-indexes. The range is read from the repository
and the symbols from the committed run, so the index may describe neither end
of the range. That case is reported as **STALE INDEX** at the top rather than
answered quietly, because every symbol in a stale report is about other code:

```
$ repocortex impact --base HEAD~6 --head HEAD
Blast radius of HEAD~6..HEAD

UNVERIFIED PROVENANCE

  STALE INDEX: this index was built at commit c91cf807, which is neither end of the
  analysed range HEAD~6..HEAD. … Re-run `repocortex index` at one end of the range and ask again.

13 files changed; 192 changed symbols found in the index, 192 analysed.
144 symbols at risk through recorded references, plus 0 reachable only through dispatch.
Riskiest change: internal/resolve/infer/language.go:For — risk moderate (59/100, certainty low).
  This is the MAXIMUM over the changed symbols, never a total: two risks of 40 do not make an 80.
```

The result is a lower bound, always: files the index holds no symbols for are
named rather than counted, and an empty result is "nothing found", never
"nothing breaks". `--json` is the form a CI gate consumes.

### `who`

**Answers:** who has context on the code a change to this symbol can reach?
Computes the blast radius, then asks git history who has touched the files it
lands in, ranked by how much of the radius they have touched.

| Flag | Effect |
|---|---|
| `--depth N` | hops to follow out from the symbol |
| `--commits N` | how far back from `HEAD` to read history |
| `--limit N` | maximum identities to list |
| `--min-confidence` | floor for the blast radius |

This is not CODEOWNERS. CODEOWNERS routes by the directory a file sits in,
which answers "who administers this path" and not "who has seen the code this
change can break": a two-line edit to a shared decoder reaches thirty call sites
in six packages, and the person listed for the decoder's directory may never
have touched any of them.

What it reports is context, not ownership, and every limit is printed with the
answer: git records that somebody changed a file, not that they understand it,
own it, or still work here; attribution is per file; the history window is
bounded; an identity is an address. A name that matches more than one
definition is refused rather than guessed:

```
$ repocortex owners Execute
"Execute" matches 2 definitions. Re-ask with a fully-qualified name:

  internal/cli/root.go:Execute             internal/cli/root.go:192
  internal/indexer/orchestrate.go:Execute  internal/indexer/orchestrate.go:477
```

### `report sarif`, `report pr`

**Answers:** what should a CI run show the reviewer? Turns the JSON that
`repocortex --json arch|missed|impact` already printed into two CI artefacts. It
reads those files, not the index, so it needs no database and runs anywhere the
JSON can be carried. At least one input is required; an input you do not pass is
reported as "not run", and so is `arch` output from a project with no declared
architecture. Each input is shape-checked: passing `missed` output to `--arch`
is an error naming the file.

`report sarif` writes one SARIF 2.1.0 run for code scanning.

| Flag | Effect |
|---|---|
| `--arch FILE` | output of `repocortex --json arch` |
| `--blindspots FILE` | output of `repocortex --json blindspots` |
| `--path-prefix P` | prefix joined onto every file path, for an index built from a subdirectory |
| `--out FILE` | output file, or `-` for stdout (default `-`) |

Findings with no file path cannot be shown by code scanning. They are left out
of `results` and counted in a run-level `toolExecutionNotification`, so they are
never silently lost. File URIs are percent-encoded.

`report pr` writes the Markdown body of a pull-request comment.

| Flag | Effect |
|---|---|
| `--impact FILE` | output of `repocortex --json impact` |
| `--arch FILE` | output of `repocortex --json arch` |
| `--blindspots FILE` | output of `repocortex --json blindspots` |
| `--max-rows N` | rows per table before the rest are counted (default 20) |
| `--out FILE` | output file, or `-` for stdout (default `-`) |

When `--impact` is given, the comment also carries a "How much of the blast
radius is proven" table: the share of `impact`'s reached symbols at each path
confidence (`exact (compiler-proven)`, `high`, `medium`; `low` only when
present), rounded to whole percent, with a one-line form in the summary row.
Symbols reached only through interface dispatch are a different claim and are
not counted; a confidence value that is not one of the four tiers is bucketed
as `unlabelled` and never echoed. When no symbol is `exact`, the comment says
that no part of the blast radius is compiler-proven and how to change that.
With no `--impact`, or an `impact` that reached nothing, the table is absent
and the summary row says "not run".

The first line is the sticky marker `<!-- repocortex:pr-report -->`, which a
workflow searches for to edit its own comment instead of adding another. Text
that came from the repository (symbol names, paths, messages) is escaped, every
table is bounded with an "...and N more" line, the whole body is capped at
60000 characters with a truncation note, and a stale-index banner leads the
report when `impact` says the index does not describe the range.

```
$ repocortex --json blindspots > missed.json
$ repocortex report pr --blindspots missed.json
<!-- repocortex:pr-report -->
## RepoCortex change-safety report

| Check | Result |
|---|---|
| Blast radius | not run |
| Architecture | not run |
| Blindspots | 38 (1 high) |

### Blindspots

| Severity | Kind | Target | Location |
|---|---|---|---|
| medium | unresolved\_call | request | src/main/java/com/pay/controller/CallbackController.java:23 |
| ... | ... | ... | ... |

_…and 18 more._
```

Neither subcommand sets a failing exit code for findings; gating stays with
`arch --fail-on` and `missed --fail-on`, so a workflow can publish first and
enforce afterwards. See the [CI guide](../USER_GUIDE.md#7-cicd-integration-github-actions-change-safety-gate).

### `export`

**Answers:** the committed symbol graph, in a format another tool reads.

| Flag | Effect |
|---|---|
| `--format F` | `csv`, `dot`, `graphml`, `json` or `svg` |
| `--out PATH` | write to a file instead of stdout; written beside its destination and renamed into place |
| `--min-confidence` | floor for exported facts (default `low`, which keeps everything) |

```bash
repocortex export --format dot | dot -Tsvg -o graph.svg
repocortex export --format graphml --out graph.graphml     # Gephi, yEd, Cytoscape
repocortex export --format csv --out edges.csv             # spreadsheets, pandas
```

Every format carries edge direction, and every format that can hold a
confidence does. A format that cannot hold something says so — on stderr, so a
pipeline reading stdout is unaffected:

```
$ repocortex export --format csv | head -3
index_revision: 1  built at commit c91cf80
note: 675 symbol(s) share a fully-qualified name with another; their node ids carry a '@<path>' suffix …
note: csv export is lossy — edge list only: nodes with no edges are omitted, node kind is not carried, …
```

The CSV writer prefixes a cell starting with `=`, `+`, `-` or `@` with a quote,
so an exported symbol name cannot become a spreadsheet formula.

### `system`

**Answers:** what crosses between repositories? Joins several members' indexes
into one read-only view and reports outbound HTTP calls answered by endpoints
declared elsewhere, and tables more than one member touches.

| Flag | Effect |
|---|---|
| `--member name=path/to/index.db` | a member, repeatable |
| `--members FILE` | the member list as YAML (`members: [{name, index}]`); paths resolve relative to the file |
| `--stale-after D` | age at which a member index is called out as old |
| `--max-edges`, `--max-shared-tables`, `--max-unjoined` | caps on each section |

Nothing is merged or migrated: each member keeps its own index, runs and
digest. Past two repositories, keep the list in a file — the estate is a
property of the team, not of an invocation, and retyping eight flags is how a
member silently goes missing. It is deliberately not `.repocortex.yml`, which is
per-repository configuration.

```yaml
members:
  - name: web
    index: ../web/.repocortex/index.db
  - name: orders
    index: ../orders/.repocortex/index.db
```

Confidence is weaker here than anywhere else in the tool, and the output says
so: matching `/users/{id}` against `/users/:id` is a guess no compiler proved,
so nothing this command produces is `exact`, and an edge's confidence is the
minimum over every leg of the path including the match. A call two members
could both answer is refused rather than assigned. The report always begins
with the members, including any whose index is absent or stale, and ends with
what it could not join. Exit status is 2 when a member could not be read, so
the report is a floor rather than a total.

A single member is refused: one repository is already answered by `api` and
`db`.

### `system contracts`

**Answers:** does each client still call a route that exists, with the right
method? Compares the outbound HTTP calls indexed in every member with the
routes indexed in every member, and reports what does not line up.

```bash
repocortex system contracts --member web=../web/.repocortex/index.db \
                            --member orders=../orders/.repocortex/index.db
repocortex system contracts --members ./repocortex.system.yml
```

| Flag | Effect |
|---|---|
| `--member`, `--members`, `--stale-after` | the same flags as `system`, with the same meaning |
| `-n`, `--limit N` | most rows to list in each section (default 50); what was cut is counted |
| `--json` | the report as JSON, with the keys named below |

| Finding (`kind`) | Meaning | Strongest confidence |
|---|---|---|
| `method_mismatch` | the call's path matches a route, and no route with that path in any member accepts the call's method | `high` on a literal path, `medium` when a path variable absorbed a segment |
| `path_variable_mismatch` | a route starts where the call starts and the two differ in how many path segments follow | `medium` when the route needs a variable the call does not supply, `low` otherwise |
| `no_matching_route` | nothing any member declares matches the call: a broken call, or a call out of this system | `high` for a literal path, `low` for a run-time target |
| `base_path_unverified` | the call and a route agree only if a mount point, gateway prefix or client base URL is assumed | always `low` |

Every finding carries the file and line of the call and of each route it is
about; `no_matching_route` has no route side by definition. After the findings
come two weaker lists: `not_compared`, the calls the index holds too little of
to compare, and `uncalled_routes`, the routes no indexed call reaches. The
second is informational and always `low`: a mobile app, a cron job, a partner,
or a caller in a language whose calls are not extracted would not appear.

No finding is ever `exact` or above `high`. Only a compiler index can make
`exact`, and none spans two repositories.

What is compared is the method, the path and the number of path segments. Path
variables match across syntaxes (`{id}`, `:id`, `<int:id>`), and case, a
trailing slash, a query string and a fragment are ignored. Three things are not
guessed:

- **Hosts.** The host of an absolute URL is shown and never used, because no
  index records which member a hostname is.
- **Base URLs.** A target that begins with a run-time value, such as
  `` `${API_BASE}/orders` ``, keeps none of its path in the index. It is listed
  under `not_compared` with that reason.
- **Mount prefixes.** A service mounted under `/api`, or reached through a
  gateway prefix, declares its routes without it. A call that matches only if
  such a prefix is assumed is reported as `base_path_unverified`, naming the
  prefix that would have to be true.

Query, header and body parameters are in the index on neither side, so they
are not compared. Calls made from test files are counted and set aside: a test
calling a server it started is not a client of this system.

The report begins with the members. For each it lists the languages its
outbound calls were read in and the ones they were not: calls are extracted
from Go and the JavaScript family (`fetch`, `axios`, `net/http`), so a Java or
Python member makes calls this report cannot see, and says so. When the route
side is incomplete — a member whose index is missing, partial or older than
`--stale-after`, or a member holding a language routes are not extracted from
— every finding loses one confidence tier and the report names the member that
cost it.

Exit status is 0 when every member was read and 2 when one was not. Findings do
not change it: they are leads to check, not a gate.

### `licenses`

**Answers:** regenerate `THIRD_PARTY.md` from the module graph, or verify it.

| Flag | Effect |
|---|---|
| `--check` | verify the committed file is current; non-zero exit if not — the form CI runs |

```
$ repocortex licenses --check
THIRD_PARTY.md is current (147 modules)
```

## Commands added with the full tool profile

Each prints a table for a person. The same answer as an agent sees it is
`repocortex tool call <tool>`; see [protocols](../guides/protocols.md).

### `dead-code`

Lists definitions nothing in the index calls, graded `likely` or `possible`.

```bash
repocortex dead-code
repocortex dead-code --path internal/billing/ --all
repocortex dead-code --include-test-only
```

| Flag | Meaning |
|---|---|
| `--path PREFIX` | Only files under this prefix |
| `--all` | Also list candidates graded `possible` |
| `--include-exported` | Also list exported definitions (implies `--all`) |
| `--include-test-only` | Also list, in a separate section, definitions whose every caller is in a test file |
| `-n`, `--limit N` | Most candidates to list (default 50) |

A row is `likely` only when it is known to be unexported, its file parsed
cleanly, no unbound reference is spelled like it, and its name appears nowhere
in the code outside its own definition. Never delete on this evidence alone.
Agent tool: `list_dead_code`.

The index knows whether a definition is exported for Go only. In every other
language a candidate is graded `possible` at best, with the reason "export
status unknown", and nothing is left out for being exported. A language with
definitions and no reference in the index is not judged at all: every
definition in it would look unused.

A function a framework calls is never `likely`. The handler a route was bound
to has the route pointing at it, so it is not listed at all. A definition
spelled like a handler some route names and could not bind (`route handler`),
and a definition that consumes an event topic (`event consumer`), are graded
`possible` with that reason, and the count of them is printed.

`--include-test-only` adds a second list under its own heading, `REACHED ONLY
FROM TESTS`: definitions that do have callers, every one of them in a test
file. It does not change the first list or its counts.

| What | How it is reported |
|---|---|
| Certainty | Always `possible`, reason `only reached from tests`. Never `likely`: code only its tests keep alive may be finished work that lost its last caller or a helper written for the tests, and an index cannot tell which |
| Order | Rows confirmed against the source text first, largest first; then rows whose name also appears in code that is not a test, marked `name appears elsewhere` (a production use the graph has no edge for, such as a function passed as a value, or an unrelated identifier spelled the same) |
| `TEST REFS` | How many references point at the definition, all from test files |
| Left out | Exported definitions unless `--include-exported`, definitions named like an entry point, languages the index does not bind references for, and route handlers and event consumers |

Its limits are printed with it. Callers are direct callers only, so a helper
called by a test-only function is not listed. A use the index could not bind is
not counted. Tests written in the same file as the code they test, as in Rust,
are not seen as tests. It is a second pass over the index, bounded by its own
scan budget, and says so if it stops short.

### `hotspots`

Ranks files by commits times size over recent history, with the files usually
committed alongside each.

```bash
repocortex hotspots
repocortex hotspots --path internal/ --commits 1000
```

| Flag | Meaning |
|---|---|
| `--path PREFIX` | Only files under this prefix |
| `--commits N` | How many recent commits to read (default 500) |
| `--include-tests` | Also rank test files |
| `--all-files` | Also rank files with no definitions (assets, generated text) |
| `-n`, `--limit N` | Most files to list (default 20) |

Needs a git repository. Agent tool: `list_hotspots`.

On a shallow clone (`git clone --depth N`, which is what most CI checkouts are)
the ranking is drawn from the commits the clone holds and says so: "Shallow
clone: N commit(s) available". The oldest of those commits counts as read but
contributes no files, because its parent was never fetched and what it changed
cannot be computed. "The repository's whole history fits inside the window" is
printed only for a full clone. `bus-factor` and `owners` state the same thing in
the same words, and `impact --base` with a revision older than the clone depth
fails with the reason and the fix (`git fetch --unshallow`).

### `bus-factor`

Lists the directories whose recent commits come from the fewest people, most
concentrated first.

```bash
repocortex bus-factor
repocortex bus-factor --path internal/ --commits 2000
repocortex bus-factor --depth 1
repocortex bus-factor --files --path internal/billing/
```

| Flag | Meaning |
|---|---|
| `--path PREFIX` | Only files under this prefix |
| `--commits N` | How many recent commits to read (default 500) |
| `--min-commits N` | Fewest commits a directory needs to be measured (default 5) |
| `--depth N` | Group by the first N path segments instead of each file's own directory |
| `--files` | Measure each file instead of each directory |
| `-n`, `--limit N` | Most directories to list (default 20) |

```
$ repocortex bus-factor
Knowledge distribution from the last 38 commit(s) (2025-06-16 to 2025-07-23) by 6 author name(s).
4 directory(ies) measured, 3 with a bus factor of 1. Most concentrated first.

BUS FACTOR  DIRECTORY  COMMITS  PEOPLE  TOP CONTRIBUTOR  SHARE  THEIR LAST CHANGE  IDLE
1           auth       6        2       kit              83%    2025-07-15         8d
1           billing    11       2       Robin Vega       82%    2025-06-24         29d
1           search     6        2       ash              50%    2025-07-10         13d
2           api        8        3       Ash Berg         38%    2025-07-02         21d
```

| Column | Meaning |
|---|---|
| `BUS FACTOR` | The smallest number of people whose commits make up at least half of the commits that touched the directory |
| `PEOPLE` | Distinct author names among those commits |
| `SHARE` | The part of those commits made by the largest contributor |
| `IDLE` | Days between that person's last change there and the newest commit read. Measured against the history, not today |

The order is the lowest bus factor, then the largest share, then the most
commits. A commit counts once for each directory it touches. Below the table
the command gives the same figure for every commit read taken together, then
what it could not see, then what the figure cannot establish.

What to keep in mind when reading it:

- It counts who changed the code in the commits read, not who understands it.
  Review, pairing and co-authors named only in a commit message are not counted.
- People are the author names git recorded, exactly as spelled. Names are never
  merged, so one person under two spellings counts as two and a bus factor can
  read higher than it is. When two names committed from one address the output
  says which names, and still leaves them apart. Email addresses are never
  printed; a name that was itself recorded as an address is cut at the at sign.
- The output says how many commits were read and the dates they span. A shallow
  clone and a squash-merge workflow both distort the figures: the first has no
  history before its depth, the second records a whole branch as one commit
  under one name. On a shallow clone the command reads the commits the clone
  holds, says "Shallow clone: N commit(s) available", and never describes them
  as every commit reachable from HEAD.
- Directories with fewer than `--min-commits` commits are not measured and are
  counted in the output. They are unmeasured, not low risk. So are directories
  the index does not hold: never indexed, or deleted or renamed since. Renames
  are not followed.

A directory that is not a git repository gets an explanation, not an error.
Agent tool: `list_knowledge_risks`.

### `communities`

Groups files into communities: sets of files whose calls and references run
between each other more than they run outwards.

```bash
repocortex communities
repocortex communities --path internal/ --limit 10
repocortex communities --members 20 --min-confidence high
```

| Flag | Meaning |
|---|---|
| `--path PREFIX` | Only group files under this prefix; relationships leaving it are counted, not followed |
| `-n`, `--limit N` | Most communities to list, largest first (default 20) |
| `--members N` | Most files to list for each community (default 8; 0 lists none) |
| `--min-confidence TIER` | Weakest relationship that links two files: `exact`, `high`, `medium` or `low` (default `medium`) |

Each community from `repocortex communities --members 3` has this shape
(`<dir>`, `<file>` and the numbers stand for your own):

```text
3. <dir>  (<N> files)
   NAMED FOR  the directory every member is under
   ID         <first member in path order>
   MOSTLY IN  <dir> (<N>)
   INSIDE     <N> relationship(s) between members
   CROSSING   <N> relationship(s) with a file outside
   MEMBERS, most referenced by the others first
     <file>  <count>
     ... and <N> more; raise --members to list them
```

A community is named for the directory all its members share, or for its
most-referenced file when they share none; `NAMED FOR` says which. `ID` is its
first member in path order: a real file, and the same on every run over the same
index. The number beside a member is how many relationships from the other
members end in it.

A community describes how the indexed code refers to itself. It is not a
declared architecture, and it is computed only from relationships the index
bound: a reference it could not bind is a missing link, so files that work
together can be listed apart or in no community. Every answer prints how many
references are unbound, how many low-confidence relationships were left out, and
how many files are in no community. A graph of more than 250,000 file-to-file
links is not partitioned; narrow it with `--path`. Agent tool:
`list_communities`.

### `path <from> <to>`

Shows the shortest routes from one symbol to another, each hop with its file,
line and confidence.

```bash
repocortex path handleCallback saveTransaction --max-depth 8
```

Flags: `--from-path`, `--to-path` (disambiguate by file), `--max-depth` (default
6), `--max-paths` (default 3). "No route found" is not proof of independence.
Agent tool: `trace_call_path`.

### `implementations <name>`

Shows what implements or extends a type, directly or through other types, and
what it implements or extends. Direct subtypes come first; a deeper one shows its
depth and the type it names. Flags: `--path`, `--limit`, `--min-confidence`,
`--direct-only`. Only declared relationships are visible without a compiler
index. The first line is the one the agent tool `find_implementations` leads
with, including how many subtypes the index saw and could not list. A table row
is always a type: an anonymous class or a class declared inside a function is
counted on the first line and is not a row.

### `tests <symbol>`

Lists the tests that reach a symbol and how each was found. Flags:
`--min-confidence`, `--path`. Agent tool: `find_tests`.

The `SIGNAL` column, strongest first:

- `graph`: the test reaches the symbol through recorded call edges.
- `interface_dispatch`: the test calls a method the symbol implements or
  overrides (an interface or base-class method), so it runs the symbol when it
  holds this implementation. It is at most `medium` when the method has
  another implementation the test may hold instead.
- `naming_convention`: the test's name follows the symbol's.
- `co_location`: the test sits in the sibling test file.

### `coverage import <file>`

```
repocortex coverage import coverage.out
repocortex coverage import lcov.info --dry-run
```

Reads a coverage report (Go coverprofile, LCOV, Cobertura XML or JaCoCo XML,
detected from the content) and stores, for each function, method and class in
the index, how many of its instrumented lines ran, how many it has, and the
highest hit count. It prints how many symbols received coverage and how many
instrumented files matched no indexed file (a Go profile names files by import
path and LCOV by absolute path; both are matched to the indexed path by their
longest shared tail, and a short name that fits two files is left unmatched).

It never runs tests: produce the report with your own test command first. One
profile is kept per index; importing another replaces it, and it goes away with
the index run it described. The import records the commit `HEAD` named at that
moment and each matched file's content hash.

A file that changes after the import (an edit, a re-index) is **stale**: its
numbers describe other code, so they are not used and the answer says so.
Importing again fixes it. `--dry-run` parses and prints the summary without
touching the index, which is what the command did before it stored anything.

What uses it, and only these two:

- `analyze_impact` adds the target's covered share (`updateStatus: 12 of 14
  instrumented lines covered in the coverage imported at 087b9cb on
  2026-11-17`) and how many of the impacted callers have no covered lines,
  naming up to five. Nothing is added when no profile is stored.
- `find_tests` adds a note when the target has no covered line in a current
  profile: no test executed it in that run, whatever the graph found.

A coverage report holds the hit count per line for the whole run and never
which test produced it, so nothing is ranked or attributed to a test. A symbol
whose file is absent from the profile is "not known", not "zero": a package no
test was run for looks the same.

### `query [sql]`

Runs one read-only SELECT against the index.

```bash
repocortex query --schema
repocortex query "SELECT kind, COUNT(*) AS n FROM symbols WHERE index_run_id = :run GROUP BY kind"
```

`:run` is the committed run's id. Flags: `-n`, `--limit` (default 100, at most
5,000), `--schema`. Only a single SELECT or WITH statement is accepted and the
index is opened read-only. The tables are storage, not a stable interface.

### `tool list`, `tool describe`, `tool call`

Runs any agent tool from the shell by its MCP name and arguments.

```bash
repocortex tool list --profile standard
repocortex tool describe trace_call_path
repocortex tool call find_callers name=ParseConfig min_confidence=high
```

`call` exits 1 when the tool reports a failure and 0 otherwise. A name shared
by several definitions is not a failure: the answer lists them with the `path`
and `line` that select each, and the exit status is 0. Flags: `--profile`
(default `full`), `--args JSON`, `--args-file FILE`, `--audit-log FILE`. Full
description: [protocols](../guides/protocols.md#command-line). What the first line
of an answer means: [reading an answer](../reference/mcp-tools.md#reading-an-answer).

### `stats`

**Answers:** what the agent tools were used for, from an [audit
log](../guides/protocols.md#audit-log): calls, answer sizes, time and failures per
tool, per transport and per day, and the slowest calls.

```bash
repocortex stats .repocortex/audit.jsonl
repocortex stats --since 2026-11-01 --slowest 20
tail -n 500 audit.jsonl | repocortex stats -
```

| Flag | Effect |
|---|---|
| `--since DATE` | count only calls at or after this date or RFC 3339 time |
| `--until DATE` | count only calls before this date or RFC 3339 time |
| `--slowest N` | how many of the slowest calls to list (default 10, at most 100) |

With no file it reads the one named by `REPOCORTEX_AUDIT_LOG`; `-` reads
standard input. It needs no index.

```
$ repocortex stats audit.jsonl --slowest 3
Audit log: audit.jsonl
14 call(s) from 2026-11-12T09:35:35Z to 2026-11-12T09:39:18Z, 3 failed (21.4%).
Answers: 64,283 bytes of text, an estimated 16,076 tokens. Time answering: 955 ms.

BY TOOL
TOOL                  CALLS  FAILED  FAIL RATE  ANSWER BYTES  EST. TOKENS  TOTAL MS  SLOWEST MS
find_callers          6      2       33.3%      11,940        2,986        184       99
get_task_context      2      0       0.0%       45,434        11,360       627       519
search_code           2      0       0.0%       999           251          35        34
get_project_overview  1      0       0.0%       958           240          12        12
get_session_stats     1      0       0.0%       2,318         580          10        10
list_hotspots         1      0       0.0%       2,634         659          87        87
no_such_tool          1      1       100.0%     0             0            0         0

BY TRANSPORT
TRANSPORT  CALLS  FAILED  FAIL RATE  ANSWER BYTES  EST. TOKENS  TOTAL MS  SLOWEST MS
mcp        8      2       25.0%      34,921        8,733        144       108
cli        6      1       16.7%      29,362        7,343        811       519

BY DAY (UTC)
DAY         CALLS  FAILED  FAIL RATE  ANSWER BYTES  EST. TOKENS  TOTAL MS  SLOWEST MS
2026-11-12  14     3       21.4%      64,283        16,076       955       519

SLOWEST CALLS
TIME (UTC)            TOOL              TRANSPORT  MS   ANSWER BYTES  OUTCOME
2026-11-12T09:39:17Z  get_task_context  cli        519  22,457        ok
2026-11-12T09:35:35Z  get_task_context  mcp        108  22,977        ok
2026-11-12T09:39:16Z  find_callers      cli        99   1,824         ok

FILES THE ANSWERS NAMED
TRANSPORT  FILES  BYTES WHEN INDEXED  EST. TOKENS  A FILE IS COUNTED
mcp        16     283,106             70,777       once per session
cli        49     909,193             227,299      once per call: each 'tool call' is its own session
This is the size of the indexed files the answers pointed at: an upper bound on what
reading those files whole would have cost. It is not a saving. An agent with search
would not necessarily have read them, and could have read parts.

Token figures are estimates: bytes / 4, rounded up; no tokenizer is run and the true
count depends on the model. Times are the whole milliseconds the log records, so a call
faster than 1 ms counts as 0. This is usage; no saving is reported because none has been
measured.
```

How to read it:

- **It reports usage, not a saving.** No "tokens saved" figure is printed,
  because there is no measurement that supports one; see
  [benchmarking](../BENCHMARKING.md#5-agent-benchmark).
- **EST. TOKENS is an estimate**: each answer's bytes divided by four, rounded
  up. No tokenizer is run, and the true count depends on the model.
- **FAILED** counts answers the tool flagged as failures and calls that never
  reached a tool, such as an unknown tool name.
- **Times are whole milliseconds**, as the log records them, so a call faster
  than 1 ms counts as 0.
- **FILES THE ANSWERS NAMED** is the size of the indexed files the answers
  pointed at, and is printed only when the log carries it. It is an upper
  bound on reading those files whole, not a saving. It has one row per
  transport because the counting differs: an MCP session counts a file once
  however many answers name it, while each `repocortex tool call` is a session
  of its own and counts it again.
- **Only numbers, tool names and transports are printed.** The arguments in
  the log say what someone was working on, and are never read into the summary.
- A line that is not an audit event is skipped and counted, not fatal.

`--json` prints the same totals as `{file, calls, failures, failure_rate, tools,
transports, days, slowest, cited_files, ...}` with `token_basis: "estimated"`.
`cited_files` carries a `meaning` string that says what the figure is, so the
number does not travel without its label.
Agent tool: `get_session_stats`, which gives the same accounting for one live
session; see [MCP tools](../reference/mcp-tools.md#get_session_stats).

## Serving

### `serve`

With `--http`, the same listener also serves the tools as plain HTTP at
`/api/v1` with an OpenAPI document, and `--audit-log FILE` records every call.
See [protocols](../guides/protocols.md).

**Answers:** runs an MCP server on stdio, or on Streamable HTTP with `--http`,
exposing the index to a coding agent.

**On a project with no index.** What happens depends on who started it:

| Started by | Behaviour |
|---|---|
| An MCP client, over a pipe (Claude Desktop, an editor, any agent) | Starts. Lists its tools. Every call is answered, not as an error, with the project directory and the commands to run in it (`repocortex init`, `repocortex index`, `repocortex resolve`) and `index_status: absent`. The server's `instructions` say the same at connect time. When the index exists, the same process answers from it. |
| A person at a terminal (standard input is a terminal) | Exits 1 with `no index at ... run 'repocortex init' and 'repocortex index' first` |
| `--http` | Exits 1 with the same message |

It does not index implicitly in any of the three, and writes nothing to the
project. A client-launched server that exited was shown by the client as a
server that failed to start, with the message that says what to do left in a
log; that is why the stdio case differs.

| Flag | Effect |
|---|---|
| `--list-tools` | print the exposed tools and exit |
| `--http host:port` | serve MCP over Streamable HTTP at `/mcp` instead of stdio, for clients that only accept a remote server |
| `--allow-remote` | with `--http`, allow a non-loopback listen address |
| `--allowed-host name` | with `--http`, also accept this `Host`/`Origin` hostname, such as a tunnel's (repeatable) |
| `--tokens-file FILE` | with `--http`, accept the named, scoped tokens in this file instead of one token; see [`serve create-token`](#serve-create-token) |
| `--audit-log FILE` | append one JSON line per tool call (default: `$REPOCORTEX_AUDIT_LOG`); records the call, never the answer |
| `--check-edits` | on by default: before every answer, re-index the files changed in the working tree since the last check; `--check-edits=false` turns it off |
| `--watch` | also keep the index current between questions, as `repocortex watch` does |

**Edits are in the next answer.** An agent works by asking, editing and asking
again. Before every answer the server compares the working tree with the index
and re-indexes the files changed since the last check, so the next answer
describes the code as it is now. The answer says so: its provenance carries
`edits_checked_at` and `refreshed_files`, and its text has a line such as
`refreshed before answering: internal/a.go`.

- A check costs about 0.2 s on a 2,000-file repository and 0.5 s on a
  25,000-file one. It runs at most every 2 seconds, and less often the longer
  it takes (ten times its own duration, up to 30 seconds), so it uses at most
  about a tenth of the server's time.
- Nothing is written when nothing changed.
- The index is written through its own handle and under `.repocortex/reindex.lock`,
  the lock git hooks and `--watch` take. When another update holds it for more
  than 2 seconds, the answer is given from the index as it stands and says so
  (`index_busy`).
- It never upgrades the schema. An index older than the build is reported once
  and left for `repocortex migrate`.
- A file saved after the check, or an editor buffer that is not saved, is not
  in the answer.

```bash
claude mcp add repocortex -- repocortex --project /path/to/repo serve
repocortex --project /path/to/repo serve --http 127.0.0.1:7788
```

Over HTTP every request needs the token from `REPOCORTEX_MCP_TOKEN`, or the one
generated for the run and written to `.repocortex/mcp-token` (mode 0600), as `Authorization: Bearer <token>` or as the last path
segment (`/mcp/<token>`). See the
[coding agents guide](../guides/coding-agents.md#remote-mcp-over-http).

The tool set is governed by `mcp.profile` in `.repocortex.yml` — `minimal`
(default), `standard`, `full`, or `compact` (single front door). The default is deliberately small: every tool's
schema is injected into the agent's context on every request, so a large surface
spends the very context this tool exists to save. Using `compact` exposes a single unified `repocortex` tool, saving over 25 KB of agent prompt context.

```
$ repocortex serve --list-tools
MCP profile "minimal" exposes 7 tool(s):
  get_project_overview
  search_code
  find_symbol
  get_symbol
  find_references
  find_callers
  get_task_context
```

Responses are bounded and never contain whole files. Repository content is
labelled as untrusted evidence, because indexed source may have been written by
someone other than you.

### `serve create-token`

**Answers:** makes one named token for a server shared by a team, prints it
once, and stores only its SHA-256 hash.

| Flag | Effect |
|---|---|
| `--name NAME` | required. Who or what the token is for; the audit log records it. Lower-case letters, digits, `.`, `_`, `-` |
| `--profile P` | the tools the token may use: `minimal` (default), `standard` or `full` |
| `--tool NAME` | narrow the profile to this tool (repeatable). Default: the whole profile |
| `--expires WHEN` | `2027-01-31` (works through that day, UTC) or an RFC 3339 time. Default: no expiry |
| `--tokens-file FILE` | append the hashed entry to this file, created with mode 0600. Without it the entry is printed for you to add |

```bash
repocortex serve create-token --name ci --profile minimal --tokens-file team-tokens.jsonl
repocortex serve --http 127.0.0.1:7788 --tokens-file team-tokens.jsonl --audit-log audit.jsonl
```

```
$ repocortex serve create-token --name ci --profile minimal --tokens-file team-tokens.jsonl
Token "ci" (profile minimal, no expiry):

    rcx_<64 hexadecimal characters>

This is the only time it is shown. RepoCortex keeps its SHA-256 hash, not the token.
Appended the entry to team-tokens.jsonl. A running server reads the file at start: restart it to load this token.
```

`--json` prints `{name, token, entry, tokens_file, appended}`. The command needs
no index. It refuses what the server would refuse (a duplicate name, a tool the
profile does not expose, a tokens file that is already malformed) and an expiry
that has already passed.

With `--tokens-file`, `serve` takes a token only as `Authorization: Bearer`, a
tool outside a token's scope does not exist for it, `REPOCORTEX_MCP_TOKEN` is
ignored, and the file is read once at start: restart to add or revoke a token.
A malformed file stops the server. Running it behind a reverse proxy, and what
shared mode does not do (no SSO, no per-repository permissions, no user
directory): [sharing one index with a team](../guides/protocols.md#sharing-one-index-with-a-team).

### `ui`

**Answers:** a local, read-only web portal over the index. One page for every
function, type, file, route and table, and five sections to start from:

- **Home** — the state of the index and how to read an answer.
- **Explore** — the routes the code exposes and the tables it uses.
- **Change** — review a branch, or plan a change to one thing.
- **Health** — how complete the index is, hotspots, unused code and blind spots.
- **Agents** — how to connect an agent, and what each tool costs in context.

A symbol page has tabs for Callers, Calls, What could break, Tests and Type
hierarchy; route and table pages follow the same pattern. Every figure is a
call to the same tool endpoint under `/api/v1` that an agent uses, so a page
and an agent cannot disagree. Each answer shows "What this answer cannot see"
and "Ask your agent the same thing", the exact `repocortex tool call ...`
command that returns it. Confidence reads as a word: proven, very likely,
uncertain or a guess. There is a Light and Dark theme toggle. See the
[portal guide](../guides/portal-guide.md).

| Flag | Effect |
|---|---|
| `--port N` | port to listen on; `0` picks a free one (default `7777`) |
| `--host ADDR` | loopback address to bind: `127.0.0.1`, `::1` or `localhost`; any other address is refused |
| `--print-url` | print the URL that would be served and exit |

`--next` and `--max-nodes` are accepted but do nothing. `--allow-refresh` was
removed: the portal has no route that writes. Run `repocortex index` or keep
`repocortex watch` running to refresh the index.

The portal opens the index read-only and registers no route that writes.

It binds to `127.0.0.1` and refuses requests whose `Host` is not a loopback
address, because a page on the public internet can otherwise point your own
browser at `localhost:7777` and read the structure of your code. It also
refuses cross-origin requests. Everything is served from the binary: no npm, no
build step, no CDN, and a Content-Security-Policy that forbids loading from any
other origin, so it works offline.

```
$ repocortex ui --print-url
http://127.0.0.1:7777/
```

### `install`

**Answers:** register RepoCortex's MCP server with every coding agent detected on
this machine, in the format each agent expects, preserving everything else in
each config file.

| Flag | Effect |
|---|---|
| `--dry-run` | report what would be written without writing it |
| `--list` | list every supported agent, whether it is detected, its config file, and the manual step for agents set up by hand |
| `--create` | also write configs for agents that are not detected |
| `--rules` | add guidance to `AGENTS.md` (created if missing) and to agent instruction files that already exist; `CONVENTIONS.md` for Aider |
| `--only CLIENT` | restrict configuration to specific clients (repeatable, e.g. `--only claude-code`) |
| `--no-agents-md` | skip creating or updating `AGENTS.md` when `--rules` is specified |

Covers 30 agents automatically, including Claude Code, Codex CLI, Copilot CLI,
Gemini CLI, Cursor, VS Code, Windsurf, Antigravity, Zed, Goose and Kiro. A file
that cannot be merged safely, such as JSON with comments, is left untouched and
reported as `manual`. The full list is in the
[coding agents guide](../guides/coding-agents.md).

```
$ repocortex install --dry-run
would update  Claude Code                  ~/.claude.json
would update  Codex CLI                    ~/.codex/config.toml
would create  Cursor                       ~/.cursor/mcp.json

Nothing was written. Run without --dry-run to apply.
```

### `agent`

**Answers:** the two things that make RepoCortex usable by an agent — the MCP
registration, and the guidance that tells the agent *when* to reach for it.

| Flag | Effect |
|---|---|
| `--guidance` | print only the guidance block, to append to `CLAUDE.md` or `AGENTS.md` |
| `--check` | report which parts of the integration are wired up; reads only |
| `--write PATH` | write the guidance block to a new file; refuses to replace one unless `--force` |
| `--force` | with `--write`, replace an existing file and say so |

The guidance is the part that is usually missing. An agent with the tools wired
up but no guidance still greps, because nothing told it a blast radius is
available before it edits a symbol, or that an empty caller list means
"unresolved" rather than "unused". The default prints and leaves placement to
you; nothing here touches your configuration.

```bash
repocortex agent --guidance >> CLAUDE.md
```

`--check` never reports a missing piece as a failure — an un-integrated project
is the normal starting state — so it exits zero unless something is broken:

```
$ repocortex agent --check
[ok  ]  index             revision 1 — 690 files, 9059 symbols, 16495 relationships
[ok  ]  resolution        79% of the 20656 references that name something this index defines are bound
[ok  ]  mcp tools         profile "minimal" exposes 7: get_project_overview, search_code, …
                            means: analyze_impact and find_tests are not exposed on this profile
                            do:    set mcp.profile: standard in .repocortex.yml to add them
[warn]  mcp registration  not in this repository's .mcp.json
                            do:    repocortex agent, and run the 'claude mcp add' line it prints
[warn]  guidance          not found in CLAUDE.md or AGENTS.md
                            means: the agent has the tools but no rule telling it when to use them
```

### `completion`

Standard Cobra shell-completion generator: `repocortex completion bash|zsh|fish|powershell`.
