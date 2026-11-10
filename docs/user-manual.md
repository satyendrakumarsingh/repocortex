# User manual

This manual is organised by what you are trying to do. Every command shown here
has been run; every output block is real output, not an illustration.

> **What works today.** Indexing and incremental re-indexing, watching and a
> commit hook, heuristic reference resolution, graph traversal, the context
> engine, an MCP server (`serve`) with client registration (`install`, `agent`),
> compiler-backed `exact` confidence via `scip import` and its measurement
> (`accuracy`), the API and database surface (`api`, `db`), configuration,
> dependency and deployment analysis (`config`, `deps`, `infra`), architecture
> governance (`arch`), change safety (`impact`, `who`, `compile`, `verify`,
> `diff`), a local portal (`ui`), a federated view across repositories
> (`system`), runtime trace correlation (`trace`), graph export (`export`),
> generated documentation (`docs`) and the retrieval benchmark (`bench`).
>
> What is NOT built is stated per command rather than here, because a global
> "not yet built" list is the first thing to go stale — this one claimed four
> capabilities were missing while the same document went on to describe all
> four. Each command's section says what it does not cover, and
> [architecture.md](architecture.md) carries the whole picture.

## Contents

- **Getting the index:** [Installing](#installing) · [Initialising a project](#initialising-a-project) · [Indexing](#indexing) · [Watching](#watching) · [Re-indexing on every commit](#re-indexing-on-every-commit)
- **Making it accurate:** [Resolving references](#resolving-references) · [Importing a compiler-backed index](#importing-a-compiler-backed-index)
- **Asking questions:** [Querying the index](#querying-the-index)
- **Change safety:** [`impact`, `who`, `compile`, `verify`, `diff`](#change-safety)
- **Structural surfaces:** [API and database](#api-and-database-surface) · [Configuration and dependencies](#configuration-and-dependencies) · [Deployment manifests](#deployment-manifests) · [Architecture governance](#architecture-governance)
- **Working with agents:** [Serving the index to an agent](#serving-the-index-to-an-agent)
- **Seeing it:** [The portal](#the-portal) · [Across repositories](#across-repositories)
- **Runtime and measurement:** [Runtime correlation](#runtime-correlation) · [Documentation, export and the benchmark](#documentation-export-and-the-benchmark)
- **Health and configuration:** [Checking status](#checking-status) · [Diagnosing with `doctor`](#diagnosing-with-doctor) · [Configuration reference](#configuration-reference) · [Troubleshooting](#troubleshooting)

Per-command flags are in [CLI.md](CLI.md); task-shaped walkthroughs are in
[GUIDE.md](GUIDE.md).

---

## Installing

Go 1.25+. Nothing else — no C compiler, no CGO.

```bash
go install github.com/satyendrakumarsingh/repocortex/cmd/cortex@latest
```

Or from source:

```bash
git clone https://github.com/satyendrakumarsingh/repocortex.git && cd cortex
go build -o cortex ./cmd/cortex
```

Verify it:

```
$ cortex version
cortex dev (commit none, built unknown, darwin/amd64)
```

`version`, `commit` and `build_time` are injected with `-ldflags` at release
time. A source build reports `dev`/`none`/`unknown`, which is correct rather than
broken.

---

## Initialising a project

```
$ cortex init
Initialized RepoCortex for "payment-platform"
  Index:          /path/to/repo/.cortex/index.db
  Schema version: 4 (4 migration(s) applied)
  Accuracy mode:  auto

Next: cortex index
```

This creates `.cortex/`, writes a `.gitignore` containing `*` inside it, and
applies the schema. It does **not** index anything.

The `.gitignore` is not a nicety. The registry contains a structural map of the
codebase — every symbol name, signature and doc comment — and must never be
committed.

`init` also verifies FTS5 is present in this SQLite build before migrating, so a
broken build is reported here rather than as mangled search results later.

Re-running `init` on an existing index refuses:

```
$ cortex init
error: index already exists at /path/to/repo/.cortex/index.db (use --force to recreate)
```

`--force` deletes the database and recreates it. Everything indexed is lost.

### Choosing the project root

Every command resolves a project root, defaulting to the current directory.
`--project <dir>` overrides it. The root must exist and be a directory:

```
error: project root "/nope" is not a directory
```

---

## Indexing

```
$ cortex index
time=... level=INFO msg=indexing progress=10% files=1/10
...
Indexed payment-platform
  Revision:      1
  Files:         9 indexed, 1 skipped (10 discovered)
  Symbols:       41
  References:    55 (unresolved until M2)
  Imports:       3
  Duration:      270ms
```

`cortex index` is **incremental by default**. It compares the working tree
against the committed revision by content hash and reparses only what changed. On
a fresh index there is nothing to compare against, so it falls through to a full
build automatically — which is why the first run above produced a full index
without `--full`.

A subsequent run with no changes:

```
$ cortex index
payment-platform is up to date (10 files, revision 1, 26ms)
```

And with one file edited:

```
$ cortex index
Updated payment-platform (revision 1)
  Added:     0
  Modified:  1
  Deleted:   0
  Unchanged: 9
  Symbols:   6 written
  Duration:  92ms
```

Note the revision number does not change. An incremental pass updates the
committed run **in place**, file by file, each in its own transaction. That keeps
the index continuously queryable, at the cost of not being atomic across the
whole pass — interrupt one and you get a consistent index that is partially
updated.

### Rebuilding from scratch

```bash
cortex index --full
```

A full rebuild allocates a new revision, builds it invisibly, and swaps it in one
transaction. An interrupted rebuild leaves the previous revision exactly as it
was:

```
error: indexing cancelled; previous revision left in place
```

Use `--full` when you have changed exclude patterns, upgraded RepoCortex, or want
to reclaim space from a long-lived incrementally-updated index.

**A full rebuild discards resolved edges.** The new revision starts with
references extracted but unbound, so re-run `cortex resolve` afterwards.

### Flags

| Flag | Effect |
|---|---|
| `--full` | Rebuild from scratch instead of updating incrementally |
| `--workers N` | Parse concurrency. **Full path only** |
| `--accuracy fast\|auto\|exact` | Override the accuracy mode recorded on the run. **Full path only** |
| `--digest` | Print the registry digest (the determinism gate) |

The two "full path only" flags are worth knowing about, because nothing warns
you:

```
$ cortex index --accuracy nonsense
error: --accuracy "nonsense": want fast, auto or exact      # exit 1, either path
```

Both flags are now VALIDATED on both paths even though they only take effect on
the full one — an earlier version exited 0 having silently discarded the flag,
and `internal/cli/index.go` records why that was fixed: "a typo in CI would have
looked like success." Nothing yet warns that a valid-but-inapplicable flag was
ignored, which is the remaining gap.

Also: `--workers` defaults to 4, not to the CPU count — peak memory scales with
worker count rather than with repository size, which is what the help text now
says. See [performance.md](performance.md#cause-1--peak-memory-tracked-worker-count-not-repository-size).

### What gets skipped, and why it is still recorded

A skipped file is written to the index with `parse_status = 'skipped'` and a
reason. It is not silently absent — "present but not indexed" and "not present"
are different facts, and confusing them makes an index look complete when it is
not.

| Reason | Meaning |
|---|---|
| `too_large` | Over the discovery ceiling (2 MiB), or grew past it during hashing |
| `binary` | A NUL byte in the first 8000 bytes — git's own heuristic |
| `symlink` | Symlinks are never followed, even when they stay inside the root |
| `symlink_escapes_root` | The link resolved outside the repository. See [security.md](security.md) |
| `irregular` | Socket, device or FIFO. Opening a FIFO would block the indexer forever |
| `unreadable` | Permission denied, broken link, or an unreadable directory |

Additionally, a file whose language has no tags query is recorded as skipped with
`no tags query available for "markdown"`, and a file over `parse.MaxSourceBytes`
(256 KiB) with `source is N bytes, over the 262144 limit`.

---

## Watching

```
$ cortex watch
Watching payment-platform (revision 1). Press Ctrl-C to stop.
```

Runs until interrupted. Editors emit bursts of filesystem events for a single
save, so changes are debounced (`--debounce`, default 300ms) and coalesced before
any reparsing happens.

Two design decisions you will notice in practice:

**It reconciles before watching.** Files change while the watcher is not running
— a branch switch, a pull, an edit between sessions. Starting straight from the
existing index would leave those changes invisible until the affected file
happened to be touched again. If anything changed, you see it at startup:

```
level=INFO msg="reconciled changes made while not watching" added=0 modified=3 deleted=1
```

**It re-diffs rather than trusting the event batch.** A filesystem event says a
path was touched, not that its content differs — editors rewrite files unchanged,
and formatters run on every save. A touched-but-identical file costs a hash, not
a reparse, and is logged at debug rather than info.

Directories are never watched if they are ignored, and `.git`, `node_modules` and
`.cortex` are refused **unconditionally**, regardless of configuration. `.git`
alone would burn scarce OS watch descriptors to deliver thousands of events
nobody wants, and watching `.cortex` would feed RepoCortex's own writes back to
itself.

The watcher caps itself at 8192 watched directories — chosen to be hit *before*
Linux's historical `max_user_watches` default, so you get a clear RepoCortex error
rather than an opaque kernel one. Exceeding it at startup is a hard failure:

```
error: start watcher: watch: watched directory limit exceeded: 8192 directories, at "some/dir"
```

A watcher that quietly covers 60% of a repository is worse than one that refuses
to start.

Errors encountered while running (a lost watch descriptor, a permission problem
on one directory) degrade coverage rather than ending the session, and are
reported as warnings.

---

## Re-indexing on every commit

`watch` follows edits; a hook follows commits, and needs nothing running.

```bash
cortex hooks install
cortex hooks status
cortex hooks uninstall
```

`install` writes a Git `post-commit` hook that re-indexes after each commit, so
the portal and any connected agent describe the commit you just made rather
than the one you made on Tuesday. The block is written inside marked
delimiters: an existing hook is appended to, never replaced, and `uninstall`
removes exactly the marked block. The hook runs detached and swallows every
failure, because a hook that can fail a commit is a hook people delete.

```
$ cortex hooks status
post-commit: not installed
The index is only as current as the last 'cortex index'.
Install it with: cortex hooks install
```

A stale index is worse than no index, because it looks current. Pick one of the
two mechanisms and leave it on.

## Resolving references

Indexing extracts references but cannot bind them: binding requires the complete
symbol table, which does not exist until every file is parsed. Resolution is
therefore a second pass, and you run it explicitly.

```
$ cortex resolve
Resolved payment-platform (revision 1)
  References:  55
  Resolved:    16 (29.1%)
  Ambiguous:   10
  Unresolved:  29  (calls outside this repository)
  Edges:       16
    high:    16
  Duration:    29ms
```

Read those numbers as reported, not as a score to maximise:

- **Unresolved** means the name matched nothing in this repository. Almost always
  a standard-library or third-party call — Spring, JDBC, a test framework. Real,
  but outside the index. It is recorded rather than hidden, because "how much of
  this repository can we actually resolve" is a question the tool has to answer
  honestly.
- **Ambiguous** means the resolver refused to guess. A name matching more than
  `MaxCandidates = 8` symbols produces **no edge at all**. Binding to an
  arbitrary one is a coin flip presented as a fact; binding to all of them floods
  the graph and makes every downstream traversal useless.

### The confidence tiers

Each tier exists because it rules out a specific way of being wrong. They are
tried in order:

| Tier | Evidence | Confidence |
|---|---|---|
| 1 | Defined in the same file | `high` |
| 2 | Receiver type matches the candidate's owner | `high` |
| 2b | Receiver matches several candidates (an overload set) | `medium` |
| 3 | Same package directory | `high` |
| 4 | Single match in an imported package | `high` |
| 4b | Several matches among imports | `medium` |
| 5 | Unique exported name anywhere in the repository, no import evidence | `low` |

Tiers 1–4 each require the evidence to yield a **unique** candidate before
claiming `high`. Nothing in this resolver ever emits `exact`; that is reserved
for compiler-backed SCIP data and is enforced in Go, in a SQL `CHECK`, and again
at commit time.

`--min-confidence high|medium|low` drops bindings weaker than the given level
instead of recording them. An invalid value is rejected:

```
$ cortex resolve --min-confidence bogus
error: --min-confidence "bogus": want exact, high, medium or low
```

### `status` and resolved edges

This section used to describe a gap where `status` printed `Relationships: 0`
after a successful `resolve`. It no longer reproduces — `status` counts the
`edges` table and agrees with what `resolve` reported:

```
$ cortex resolve
  Edges:       1
$ cortex status
Relationships:  1
```

The `index_runs.edges_found` column is still written only by `CommitRun` and is
still not updated by `resolve`. `status` simply no longer reads it, so the stored
column and the reported count can disagree; the reported count is the accurate
one.

Worth knowing why this entry survived being wrong for a while: an index built by
an older binary keeps that binary's stored counts, so the old behaviour appeared
to reproduce on any index that had not been rebuilt. That class of confusion is
what the provenance warning in Troubleshooting now exists to surface — re-index
before concluding that a documented gap is still real.

---

## Importing a compiler-backed index

```bash
cortex scip import build/index.scip
```

`resolve` builds a call graph by inference and tops out at `high` confidence,
honestly — binding `svc.updateStatus()` to a definition needs a type for `svc`,
which needs the build graph. A SCIP index is what a compiler concluded, and it
is the only input RepoCortex accepts as proof.

Use this when the index already exists. CI usually has the toolchain and the
build cache that a developer's machine may not; `cortex indexers install`
provisions and runs those tools locally, and this is the path for an index you
already have.

Order matters — index, resolve, then import:

```bash
cortex index
cortex resolve
cortex scip import index.scip
```

### What it does to the graph

Relationships the compiler proved are written with confidence `exact` and source
`compiler`. Nothing is ever downgraded, so importing a stale index cannot weaken
the graph, and importing the same index twice is a no-op.

**Expect most facts to arrive rather than upgrade.** SCIP records that one symbol
references another; it does not record that the reference is a *call*, and
RepoCortex will not stamp a guess about that with `exact`. So compiler facts land
as `REFERENCES` edges and sit alongside the resolver's `CALLS` edges instead of
replacing them. Where both describe the same relationship at the same line, the
weaker edge is upgraded in place rather than duplicated — and the report says
which happened, because "the compiler found 4,000 new relationships" and "the
compiler confirmed 4,000 known ones" are different facts.

Measured on RepoCortex's own source, with a 5.4 MB index from `scip-go`:

```
scip-go 0.2.7: 219 document(s), 195 matched
symbols: 3005 matched, 10277 with no counterpart in the index
edges:   11353 new, 0 upgraded from heuristic, 664 already known
         50833 reference(s) point outside the indexed repository
         2457 reference(s) the indexer attributed to a file rather than a definition were skipped
duration: 415ms
```

### Reading that report

**It never adds symbols.** A definition the compiler found that RepoCortex has not
indexed is reported, not invented — writing symbols into a published index is
what the two-phase swap exists to prevent.

**A large "no counterpart" count is normal.** A compiler sees local variables,
struct fields and generated declarations that a tags-based extractor never
emits, which is why 10,277 of 13,477 above are unmatched with nothing wrong. Only
a very low match *rate* suggests the index and the working tree have diverged,
and the command says so when it does.

**References pointing outside the repository are normal too** — a call into a
third-party package has no symbol here to attach to. They are counted rather
than dropped silently, because otherwise "SCIP produced fewer edges than I
expected" is undiagnosable.

### When nothing matches

Paths are the usual failure. An index built in a container records paths like
`/build/repo/src/Main.java`. RepoCortex reconciles those against its own file list
and reports how many documents matched; if that fraction is low it prints
examples. When they share a leading directory the repository does not have:

```bash
cortex scip import index.scip --path-prefix src
```

An index that matches *nothing* is a hard error rather than an empty result,
because zero edges and no error is indistinguishable from a repository with no
relationships.

### Verifying the result

```bash
cortex references updateStatus --min-confidence exact
cortex doctor --verify-index
```

The first shows only compiler-proven facts. The second re-checks that every
`exact` edge in the store carries `source=compiler` — the invariant this whole
path exists to uphold, enforced in Go, in a SQL `CHECK`, and again here.

---

### `accuracy` — precision, recall and F1 on your repository

```bash
cortex accuracy
cortex accuracy --json
```

Once a compiler-backed index is imported, this adjudicates every heuristic
`CALLS` edge against it and reports precision, recall and F1 per language and
per confidence tier. Recall is restricted to call sites the compiler resolved
to exactly one definition, which is what makes the denominator sound: SCIP
records references, a superset of calls, and dividing by every reference would
penalise the resolver for correctly declining to emit a call edge at a type
annotation.

A language with no SCIP index imported reports `NOT MEASURED`. It is never
shown as 0% and never dropped from the table, because an unmeasured language
that reads like a measured one is the single failure a trust report cannot
survive. The numbers this project publishes, and how they were produced, are in
[accuracy.md](accuracy.md).

### `languages` — which languages resolve, and how far

```
$ cortex languages
TIER      COUNT  WHAT YOU GET
typed     1      definitions, imports, receiver and arity discrimination — references can reach 'high'
resolved  11     definitions and imports — references bind across files
parsed    194    definitions and search only — no cross-file references claimed
```

The tiers are computed from the query set at run time, so this output cannot
disagree with what the binary does. Read it before trusting an empty
relationship result: a `parsed`-tier language never produces a caller, and the
absence is a property of the tier, not of the code. `--verbose` lists the
parsed-tier grammars too.

## Querying the index

Six commands answer the same questions the MCP tools do, against the same
index, without an agent or a server in the loop. Use them when you want to know
what an agent was told, when you are scripting a CI check or a git hook, or when
you simply want to look something up.

All of them require a committed index — run `cortex index`, then
`cortex resolve` for anything relationship-shaped. All of them take `--json`.

### `search` — full-text across indexed files

```bash
cortex search "callback"
cortex search "idempotency" -n 50
```

Identifiers are indexed by their parts, so `callback` matches
`handlePaymentCallback` and `PaymentCallbackController`.

This searches **text**. It covers indexed files only: binary, generated and
oversized files are recorded in the index but carry no searchable text, and
secrets are redacted before storage, so a committed API key is not findable
here. An empty result says so rather than implying the term is absent.

### `symbol` — definitions by exact name

```bash
cortex symbol TransactionService
cortex symbol handle --kind method
```

Exact, not fuzzy. Asking for `TransactionService` returns that type, not the
forty symbols whose names contain it — a fuzzy symbol lookup is a guess, and an
agent acting on the wrong definition is worse off than one told nothing matched.
Use `search` for partial matches.

Each result reports the confidence of the **extraction** and its source, so a
compiler-proven definition is distinguishable from a parsed one.

### `references`, `callers`, `callees` — relationships

```bash
cortex references updateStatus          # everything pointing at it
cortex callers updateStatus             # narrower: call edges only
cortex callees updateStatus             # what it depends on
cortex callers Save --path svc/repo.go  # disambiguate
cortex callers Save --min-confidence high
```

`references` is the broadest: calls, imports, implementations, extensions and
plain references. `callers` counts call edges only — an import is a reference
but not a call. `callees` looks the other way.

Every edge carries the confidence that justified it and the file and line that
evidences it, so each claim is checkable rather than taken on faith.

**An empty result is never presented as proof of absence.** It reports the
index's measured resolution coverage instead:

```
No callers found.

47% of 12988 references are bound to a definition (6486 point outside this
repository, 454 were too ambiguous to bind). An empty result is a lower bound,
not proof that nothing uses this symbol.
```

That distinction matters more than any other output in this tool: "nothing calls
this method" means it is safe to delete, and "nothing I could resolve calls this
method" means nothing at all.

**And where the index knows why, it says why.** Coverage tells you how much to
trust an empty answer in general; it does not tell you what happened to the name
you asked about. Every reference records its own outcome, so an empty result that
has unbound references to that exact name is diagnosed rather than merely
reported:

```
No callers found.

WHY: 3 reference(s) to "Persist" were seen and not bound.
  use/u.go:5    ambiguous, receiver "s"
  cmd/main.go:22  ambiguous, receiver "store"
  api/handler.go:91  unresolved

  "ambiguous" means the reference was found and RepoCortex could not decide which
  definition it meant; "unresolved" means it resolved outside this repository.
  Either way the call may well exist — this is a blind spot, not an absence.
```

Three situations produce an identical empty result in every other tool in this
category: the call does not exist, the file never parsed, or the reference was
seen and could not be bound. They have three different remedies, and only the
third is a RepoCortex limitation you can route around — by passing `--path` to
disambiguate, or by importing a SCIP index so the compiler decides instead.

References that bound successfully are deliberately not listed here. They are
already visible as edges, and repeating them would bury the rows that are the
problem.

When several definitions share a name, the query answers for the first and then
lists the others so you can re-run with `--path`. It never picks one silently.

### `context` — the pack an agent would receive

```bash
cortex context "Add idempotency to payment callback processing"
cortex context "fix the debounce timer" --show-excluded
```

Assembles a bounded, ranked context pack — the same thing `get_task_context`
serves over MCP, printed for a human.

The output leads with how the task was **understood**, because almost every bad
pack comes from a misread task rather than from bad ranking, and seeing the
interpretation first turns a baffling result into an obvious one. Every item
carries the reason it earned its tokens. `--show-excluded` lists what was
considered and cut, with the reason for each — a bounded answer is only
trustworthy if the boundary is visible.

`--max-files`, `--max-symbols` and `--max-lines` override the configured budget
for one run.

---

## Change safety

The commands that answer "what does this change put at risk", before and after
it is made. `impact` and `who` read a change; `compile` and `verify` bracket
one; `diff` compares two indexed states of the code. Every one of them reports
a lower bound and says so: files the index holds no symbols for are named, not
counted, and an empty result is "nothing found", never "nothing breaks".

### `impact` — the blast radius of a git range

```bash
cortex impact --base HEAD~1 --head HEAD
cortex impact --base main --head my-branch --depth 3
cortex impact --base origin/main --json           # the form a CI gate consumes
```

Reads the files the range changed, finds the symbols defined in them, and walks
*inward* to everything that depends on each. The direction is the point: a
change threatens the code that depends on it, not the code it calls, so this
walks callers. Renamed and deleted files are seeded under the name the index has
for them, since those are the two changes most likely to break a caller and the
ones a naive diff would report as empty.

Nothing here checks out or re-indexes. The range is read from the repository
and the symbols from the committed run, so the index may describe neither end
of the range. That case is reported as **STALE INDEX** at the top rather than
answered quietly, because every symbol in a stale report is about other code:

```
$ cortex impact --base HEAD~6 --head HEAD
Blast radius of HEAD~6..HEAD

UNVERIFIED PROVENANCE

  STALE INDEX: this index was built at commit c91cf807, which is neither end of the
  analysed range HEAD~6..HEAD. … Re-run `cortex index` at one end of the range and ask again.

13 files changed; 192 changed symbols found in the index, 192 analysed.
144 symbols at risk through recorded references, plus 0 reachable only through dispatch.
Riskiest change: internal/resolve/infer/language.go:For — risk moderate (score 59, certainty low).
  This is the MAXIMUM over the changed symbols, never a total: two risks of 40 do not make an 80.
```

### `who` — who has context on the code a change can reach

```bash
cortex who PaymentService.capture
cortex who pkg.Decode --depth 2 --commits 500
```

Computes the blast radius of a change to a symbol, then asks git history who
has touched the files it lands in, ranked by how much of the radius they have
touched. This is not CODEOWNERS: CODEOWNERS routes by the directory a file sits
in, which answers "who administers this path" and not "who has seen the code
this change can break". A two-line edit to a shared decoder reaches thirty call
sites in six packages, and the person listed for the decoder's directory may
never have touched any of them.

What it reports is context, not ownership, and every limit is printed with the
answer: git records that somebody changed a file, not that they understand it,
own it, or still work here; attribution is per file; the history window is
bounded; an identity is an address. A name matching more than one definition is
refused rather than guessed:

```
$ cortex who Execute
"Execute" matches 2 definitions. Re-ask with a fully-qualified name:

  internal/cli/root.go:Execute             internal/cli/root.go:192
  internal/indexer/orchestrate.go:Execute  internal/indexer/orchestrate.go:477
```

### `compile` — the brief an agent should have been given

```bash
cortex compile "Add idempotency to payment callback processing"
```

Turns a one-line request plus the assembled context into an explicit task
contract: goal, scope with a reason per item, what is deliberately out of scope,
constraints, acceptance criteria, risks — and the questions it could not answer.

The thesis is that most bad agent output comes from an under-specified task
rather than a weak model. An agent told "add idempotency" invents the scope, the
constraints and the definition of done. A contract states them.

**Everything is derived; nothing is invented.** Every constraint and risk carries
the file or symbol it came from — a test file in scope produces "existing tests
must still pass", an endpoint produces "changing the route is a breaking change",
a table produces "a schema change needs a migration". A constraint that cannot
cite evidence is an opinion, and is not emitted.

The **Unknowns** section is never suppressed. A contract that pretends to be
complete is worse than one that names its gaps.

This does not call a model and does not write code.

A contract is compiled **with** the architecture rules in force and the blast
radius of the symbols it scopes, not from the context pack alone — a brief that
cannot say which rule the change must respect, or what the change would reach, is
a scope statement rather than a contract. Those two sections are also what
`cortex verify` later checks the finished change against.

### `verify` — did the change honour the contract?

```bash
cortex compile "Add idempotency to payment callbacks" --json > contract.json
# ... the change gets written ...
cortex verify --contract contract.json --changed "$(git diff --name-only | paste -sd, -)"
```

The second half of the loop, and the half nothing else in this category has.
`compile` says what the task is; `verify` asks whether the change that followed
stayed inside it. Four questions:

1. Did the change stay in the contract's declared scope?
2. Did it violate a declared architecture rule?
3. Did it miss a caller the blast radius flagged?
4. Do the tests the contract named still cover the changed symbols?

**Facts and leads are rendered differently, and the distinction is the point.** A
violation on a compiler-proven edge is a fact. The same violation on a heuristic
edge is a lead, because the edge itself may be wrong. Presenting the two
identically makes the report either alarmist or ignorable, and both end with the
gate switched off.

Exit codes, because CI branches on them and never reads the prose:

| Code | Disposition | Meaning |
|---|---|---|
| 0 | `pass` | every question was asked, nothing was found |
| 0 | `advisory` | findings exist, all of them leads |
| 1 | `fail` | at least one fact |
| 2 | `unverified` | there was nothing to check against |

`--fail-on-leads` promotes `advisory` to exit 1, for a team that wants the
stricter gate. `--min-confidence` raises the floor above the contract's own.

`unverified` is deliberately **not** `pass`. "We looked and found nothing" and
"there was nothing to look at" must not render the same, or the gate silently
stops gating on the day contract generation breaks and nobody notices.

For the same reason every report prints a **NOT CHECKED** and an **UNKNOWN**
section: symbols excluded because every path to them crossed an edge below the
confidence floor, exclusions it declined to enforce, and questions the repository
gave it no way to answer — a repository that declares no architecture is told so,
rather than being passed on a rule check that never ran.

Two things it deliberately does not do. It does not shell out to git: a verify
that ran its own `git diff` would disagree with the caller about staged, unstaged
and untracked files. And it will not invent a contract — with no `--contract` it
fails rather than checking a change against nothing.

**Scope is symbol-level, and one file routinely holds both sides.** The context
engine ranks each symbol on its own, so a file can supply symbols the task needs
and symbols it does not. An edit to such a file is not reported as out of scope;
the exclusion it declined to enforce is counted in UNKNOWN instead.

**The blast radius includes symbols reached by interface or virtual dispatch**,
and each one says so rather than citing a call site. A dispatch reach *has* no
call site — nothing names the symbol, which is exactly why a human reviewer
misses it, and exactly why it belongs in the radius. It is reported as a claim
about reachability, never as a proven call.

### `diff` — what changed about the code's facts

```bash
cortex diff --from 41 --to 42
cortex diff 8f3c1ab..a19d7e0
cortex diff --from-index ../release-1.4/.cortex/index.db --from 8f3c1ab
```

Not changed lines — changed **facts**: symbols added, removed, moved or orphaned;
relationships added, removed or re-graded; endpoints whose handler changed;
tables that gained or lost a reader or a writer. *"This endpoint now writes
`refunds`"* and *"this symbol lost its last caller"* are the review questions a
line diff cannot answer.

This is only sound because the index is byte-reproducible. Identity is
content-based — path, FQN and kind for a symbol; the two endpoints and the kind
for an edge — never a row id, so a reported difference means the code changed
rather than that a worker pool interleaved differently. Diffing a run against
itself reports nothing, and that is a test.

Nothing here invokes git. A ref that matches no indexed run fails and says which
runs the index does hold; it never falls back to whichever run happens to be
committed, because a diff against the wrong run is worse than no diff. Committing
a run retires the one before it, so the two sides of a cross-commit comparison
normally live in two index files — which is what `--from-index` and `--to-index`
are for.

The report always ends with what it could **not** compare. A degraded run, a file
that did not parse, or two runs indexed in different accuracy modes all mean an
absence may be an indexing gap rather than a deletion, and saying so is the
difference between a diff and a guess.

## API and database surface

Two commands answer the questions APIMap and DBMind exist for — what this
service exposes, and what its data access actually is. Both read the same graph
`index` and `resolve` built.

### `api` — the HTTP surface

```bash
cortex api
cortex api --json
```

```
METHOD & PATH           HANDLER         DECLARED AT
POST /payment/callback  handleCallback  src/main/java/com/pay/controller/CallbackController.java:21

1 endpoint(s). Discovery covers spring, go-http, go-router, fastapi, flask, django, express, nest.
```

Routes come from the framework's own spelling — a Spring annotation, a Go router
registration, a FastAPI or Flask decorator, a Django `path()` in a urlconf, an
Express member call, a NestJS controller decorator — never from strings that
merely look like paths. That is what keeps a log message mentioning `GET
/users` out of the report, and it is why the class-level `@RequestMapping` is
tracked: reporting `/callback` for a controller mapped at `/payment` would name
an endpoint no client can call. Django routes are reported at `medium`
confidence, because the `include()` mount prefix that precedes a urlconf's own
paths is not visible from the file.

An endpoint whose handler the framework did not name reports no handler.
Guessing at the nearest function would send you to the wrong file.

**Coverage is printed with every result**, because an empty list has two very
different meanings. OpenAPI documents, GraphQL schemas, gRPC service definitions
and gateway configuration are **not** read; a service defined entirely in those
reports nothing, and that is a limit of the tool rather than a finding about
your code.

### `db` — tables, reads and writes

```bash
cortex db
cortex db table transactions
```

```
TABLE               READS  WRITES  DECLARED HERE
transactions        1      1       yes
transaction_events  0      1       yes
audit_log           0      1       no — not declared in this repository
```

`READ`, `WRITE`, `READ_WRITE`, `DDL` and `UNKNOWN` are kept distinct. "What
touches this table" is a mildly useful question; "what **writes** it" is the one
asked before changing a column, and answering it with reads mixed in is worse
than not answering. A single `INSERT ... SELECT` is `READ_WRITE`, because
collapsing it would hide one half or the other.

A table nothing here declares is still reported. That is the normal shape of a
service reading a database another service owns, and dropping it would make
"which tables do we touch that we do not own" unanswerable.

`db table` answers the question in its useful form — not whether a table is
used, but exactly what writes it and where:

```
transactions (declared in this repository)

WRITTEN BY (1)
  save (src/.../TransactionRepository.java)   src/.../TransactionRepository.java:20   high (UPDATE (literal SQL))

READ BY (1)
  findById (src/.../TransactionRepository.java)  src/.../TransactionRepository.java:14  high (SELECT (literal SQL))
```

Attribution is to the **method**, not the class: "TransactionRepository writes
transactions" hides which of its methods does.

### What the SQL analysis can and cannot see

It is a recogniser, not a parser. It finds the statement's verb and the tables
it names, and stops. That is a deliberate boundary — a full multi-dialect parser
fails by silence, and a table that appears in no query looks exactly like a table
nobody uses.

- **Seen:** SQL in `.sql` files, and SQL in string literals in source, including
  multi-line forms (Go backticks, Java text blocks, Python triple quotes).
- **Reported with low confidence:** queries assembled at run time. The table is
  real and the verb is real; whether that exact statement executes as written is
  not something a static read can establish, so the edge is weakened rather than
  dropped.
- **Not seen:** queries built by an ORM's fluent API, stored procedures,
  triggers and views. `cortex db` says so when it finds nothing.

Bound parameters (`?`, `$1`, `:id`) are **not** treated as signs of dynamic
assembly — they are how a safe query is written, and flagging them would
downgrade every correct call site in the repository.

---

## Configuration and dependencies

Two more commands over the same index. Both lead with the **mismatch** rather
than the inventory, because the inventory is the boring half: "you declare 47
settings" is a number, "three settings your code reads are declared nowhere" is
a deployment about to fail at start-up.

### `config` — settings, and what reads them

```bash
cortex config          # the mismatches
cortex config --all    # the full inventory
```

```
READ BUT NOT DECLARED (3) — a service that needs these and finds nothing fails at start-up
  API_KEY               3 reader(s)  sensitive (by name)
  DB_PASSWORD           1 reader(s)  sensitive (by name)
  PORT                  1 reader(s)

10 key(s) total: 2 declared and never read, 4 labelled sensitive by name.
```

**Values are never read.** Not masked, not truncated — never read at all. The
scanners stop at the delimiter, and no structure in the tool has a field to hold
a value; the database schema has no column for one either. That is a stronger
guarantee than redaction, because a redaction list can be wrong about a key
nobody anticipated, and a data structure with nowhere to put a value cannot leak
one. Keys are labelled sensitive from their **name**, which requires reading
nothing.

Declarations come from `.env`, YAML, JSON, TOML and `.properties`. Usage comes
from `os.Getenv`, `System.getenv`, `@Value("${...}")`, `os.environ`,
`process.env`, Viper and generic `config.Get` lookups.

One thing worth knowing: a setting declared as `database.url` in a YAML file and
read as `DATABASE_URL` from the environment is **one** key, not two. Every
twelve-factor service crosses that spelling boundary, and treating the two as
different would report the same setting as both unused and missing.

Not read: Kubernetes manifests, Helm values, Terraform and CI definitions. Each
is a schema of its own, and treating a Kubernetes manifest as flat configuration
produces keys like `spec.template.spec.containers` that no code ever looks up.

### `deps` — what is declared, and what is imported

```bash
cortex deps            # the unimported ones
cortex deps --all      # the full list
```

```
DECLARED BUT NOT IMPORTED (1)
  github.com/never/imported  v0.1.0  runtime  go.mod:7

  Check before removing: a package can be loaded by reflection, required by a
  build plugin, or imported under a name that differs from its distribution name.
```

Manifests, not lock files — `go.mod`, `package.json`, `pom.xml`,
`build.gradle`, `requirements.txt`, `pyproject.toml`, `Cargo.toml`. A lock file
is the resolved transitive closure, hundreds of packages most of which no line
of your code mentions; what a project depends on is a statement its authors
made.

The usage join is free: the indexer already records every import of every file,
so no second extraction pass is needed. Matching is ecosystem-aware, because the
relationship between a package name and what source writes differs per
ecosystem — `github.com/go-git/go-git/v5/plumbing` uses
`github.com/go-git/go-git/v5`, `lodash/fp` uses `lodash`, `serde_json::Value`
uses the `serde-json` crate.

Dependencies the manifest itself marks transitive — Go's `// indirect`, npm's
`peerDependencies` — are never flagged as unused. That would restate a fact the
manifest already makes.

**No vulnerability data, deliberately.** Spec §27 forbids inventing it, and a
scanner shipping an embedded advisory list is exactly that — stale the day it is
built. Versions are reported accurately so a real scanner can consume them; the
lookup belongs against a live database.

---

## Deployment manifests

Source code cannot answer "which service does this code run in". That fact
lives in a compose file's build context or a Deployment's image, and nowhere
else.

```bash
cortex infra                     # every deployment unit the index holds
cortex infra --for src/api.go    # which unit does this file run in
cortex infra --key DB_URL        # what does changing this key name restart
cortex infra --keys              # every key name each unit sets
cortex infra --disk              # scan the tree instead of the index
```

Read: Dockerfiles, Compose files, Kubernetes manifests, Helm charts and
Terraform. **Values are never printed**, and the guarantee is structural rather
than a filter: the extractor has no field a value fits in, and a Kubernetes
`Secret` is read as the key names it declares. What you get is "`DB_PASSWORD`,
from Secret `db-creds`", which is the half you can act on.

These facts are stored by `cortex index`, so a service is a node in the same
graph as the code and a key name binds to the code that reads it. `--disk`
reads the working tree directly instead, which is right on a repository you
have just cloned and not indexed; the output says which of the two it is
reading, because they can disagree.

What could not be read is reported: a manifest that fails to parse is listed
with the reason, and Helm templates are reported as unrendered rather than
guessed at. Terraform is read as block headers only, never attribute values.
**Not read:** CloudFormation, systemd units, CI definitions.

```
$ cortex infra
The index holds no deployment manifests. If you expected some, re-run 'cortex index';
to check the working tree directly, run 'cortex infra --disk'.
```

## Architecture governance

```bash
cortex arch                      # violations
cortex arch --crossings          # every component-to-component dependency
cortex arch --fail-on high       # non-zero exit for CI
cortex arch adr                  # architecture decision records
```

### Why this is not a dependency linter

An import linter checks imports. This checks the **graph**, which includes
relationships no import reveals: an endpoint exposing a handler, a method
writing a table, a service reading a configuration key.

"The reporting module must not write the payments table" is an architectural
rule no import-based tool can see, because the write happens through a
repository class that reporting is perfectly entitled to call. RepoCortex sees it
because M7 put the table in the graph.

### Components are declared, never inferred

```yaml
# .cortex.yml
architecture:
  components:
    - name: controller
      include: ["src/main/java/com/pay/controller/**"]
    - name: service
      include: ["src/main/java/com/pay/service/**"]
    - name: repository
      include: ["src/main/java/com/pay/repository/**"]
  rules:
    - id: controllers-go-through-services
      description: Controllers must not reach the repository layer directly.
      from: [controller]
      allowed_to: [service]
      forbidden: [repository]
      severity: high
```

A component model RepoCortex guessed would be a description of the code's current
shape, and checking code against a description of itself finds nothing by
construction. If nothing is declared, the command says so rather than reporting
zero violations — a pass mark nobody earned.

**These rules live in the repository's config even though that file is
untrusted.** That is a deliberate exception with a specific justification:
architecture rules are pure declarative data — path globs and component names,
matched by a hand-written matcher and never compiled as a regular expression, so
a hostile pattern cannot even cause catastrophic backtracking. Nothing in the
block selects a tool, a path to execute, or an argument. And it is where they
belong: a rule that lives in one contributor's home directory is a rule that is
not enforced.

### Reading the report

```
VIOLATIONS (1)
  SEVERITY  RULE                            CROSSING                                          EVIDENCE                       CONFIDENCE
  high      controllers-go-through-services  controller -> repository (handleCallback -> findById)  .../CallbackController.java:12  high
```

Every violation cites the file and line that causes it, and the **confidence of
the relationship**. That distinction matters: a violation inferred from a
`low`-confidence heuristic edge is a lead, one on a compiler-proven edge is a
fact. Presenting them identically makes the report either alarmist or
ignorable, and both end with it switched off.

Two coverage statements are printed with every run, because both silently weaken
the check:

- **Symbols in no declared component** were not checked at all. A forbidden
  dependency routed through one of them is invisible.
- **A component that matched nothing** means its include patterns are wrong, and
  every rule naming it has been silently inert.

Output is sorted deterministically — worst severity first, then location — so
this can live in CI and "did it get worse?" is answerable from a diff.

### `arch adr` — decision records

Lists ADRs from `docs/adr/`, `docs/decisions/` and the other conventional
locations, in both the Nygard and MADR formats.

An ADR is *why* a choice was made. Surfacing it is how "we deliberately use
optimistic locking here" survives contact with a refactor, instead of being
re-litigated from scratch every session.

RepoCortex does not check whether an ADR is still **true**. A record saying the
system uses Postgres is evidence of a decision, not of the current state — the
graph is where you check the current state.

### Not covered

Deployment manifests, service registries and runtime traces, all listed as
sources in spec §26. Those describe a running system; this describes a
repository.

---

## Serving the index to an agent

```bash
cortex serve
```

Runs an MCP server on stdio. Register it with Claude Code:

```bash
claude mcp add cortex -- cortex --project /path/to/repo serve
```

The database is opened **read-only**: an agent session must never be able to
mutate the index it is querying.

To see what is exposed without starting a server:

```
$ cortex serve --list-tools
MCP profile "minimal" exposes 7 tool(s):
  get_project_overview
  search_code
  find_symbol
  get_symbol
  find_references
  find_callers
  get_task_context
```

`get_task_context` is the one to reach for first — it answers in a single call
what the other six answer in a dozen. `standard` adds `impact_analysis` and
`find_tests`; `full` is currently the same set as `standard`.

The tool set is governed by `mcp.profile` in `.cortex.yml` — `minimal`
(default), `standard` or `full`. The default is small on purpose: every exposed
tool's schema enters the agent's context on **every** request, and a long tool
list measurably degrades tool selection. A server that quietly reached forty
tools would spend the context this product exists to save, on every call,
forever. A test fails if the minimal profile grows past eight tools.

Two properties of the responses are worth knowing as a user:

- **An empty caller list is not presented as proof.** The index resolves only
  part of any repository, so "no callers" is a lower bound. Every empty
  relationship result carries the unresolved and ambiguous counts, because an
  agent told otherwise will delete the method.
- **Repository content is labelled untrusted.** It arrives under a `SOURCE
  EVIDENCE` heading with an explicit note that it is data, not instructions.
  Indexed source may have been written by someone other than you. See
  [security.md](security.md#prompt-injection).

Responses are bounded centrally and never contain whole files. A truncated
response says so rather than pretending to be complete.

`serve` requires a committed index:

```
error: no index at /path/.cortex/index.db — run 'cortex init' and 'cortex index' first
```

### `install` — register with every detected client

```bash
cortex install --dry-run
cortex install
```

Writes the MCP registration into the config file of every detected client,
preserving every other server and setting already there. Covers Claude Code,
Claude Desktop, Cursor and Windsurf. A client whose config file does not exist
is skipped rather than created, since creating one would guess at a layout the
client may not accept. For Zed, which uses a different config key, use `agent`
and place the output yourself.

```
$ cortex install --dry-run
would write Claude Code      ~/.claude.json
would write Claude Desktop   ~/Library/Application Support/Claude/claude_desktop_config.json
skip  Cursor           (no config at ~/.cursor/mcp.json)
skip  Windsurf         (no config at ~/.codeium/windsurf/mcp_config.json)

Restart your agent, then ask it to use RepoCortex.
```

### `agent` — the registration, and the guidance that makes it used

```bash
cortex agent                     # the registration and the guidance
cortex agent --guidance >> CLAUDE.md
cortex agent --check             # what is and is not wired up
```

The guidance is the part that is usually missing. An agent with the tools wired
up but no guidance still greps, because nothing told it a blast radius is
available before it edits a symbol, or that an empty caller list means
"unresolved" rather than "unused". The default prints and leaves placement to
you; nothing here touches your configuration. `--check` never reports a missing
piece as a failure, since an un-integrated project is the normal starting
state:

```
$ cortex agent --check
[ok  ]  index             revision 1 — 690 files, 9059 symbols, 16495 relationships
[ok  ]  resolution        79% of the 20656 references that name something this index defines are bound
[ok  ]  mcp tools         profile "minimal" exposes 7: get_project_overview, search_code, …
[warn]  mcp registration  not in this repository's .mcp.json
                            do:    cortex agent, and run the 'claude mcp add' line it prints
[warn]  guidance          not found in CLAUDE.md or AGENTS.md
                            means: the agent has the tools but no rule telling it when to use them
```

## The portal

```bash
cortex ui                        # http://127.0.0.1:7777/
cortex ui --port 0               # pick a free port
cortex ui --allow-refresh        # let the page rebuild the index (opt-in)
```

A local web portal over the index, with twenty screens grouped by what you
want to know. Index health stands alone above the groups, carrying the same
weight as a group's first entry. Then five groups, named the way a reader
would ask the question:

- **Understand the code** — Map of the code (a graph explorer that drills
  repository → package → file → symbol), Find a symbol, Shape of the code.
- **Before you change something** — What could break (blast radius for one
  symbol), Task brief (the compiled task contract), Check a change (the
  verify report that marks a completed change against that contract), Who
  knows this code, What changed.
- **How it runs** — APIs and what they touch (the cross-stack view from a
  frontend component through the route it calls, the handler that answers
  it and the tables that handler reaches), Database tables, Settings and
  secrets, What actually ran, Across repositories.
- **Quality** — Test coverage, Architecture rules (governance), Dependencies,
  How accurate is this index.
- **For your AI agent** — Connect an agent, Retrieval quality.

Every screen opens with its answer in one plain sentence, built from the same
data the screen renders. The evidence behind that sentence, and the things
the view can't see, sit folded below it — closed by default, so the answer is
what a reader meets first — and `?open=1` on any screen's URL unfolds all of
them at once, for an agent or a screenshot that needs the whole page. Where
the old portal showed confidence as a raw tier, it now reads as a word
(proven, very likely, uncertain, a guess); the tier's id is kept on every
field and flag underneath so a script can still match on it. A "What do these
words mean?" button in the rail opens a glossary for the handful of terms
that remain.

The portal is read-only. It opens the index read-only and serves only GET
routes; without `--allow-refresh` no route that writes is registered at all,
so read-only is enforced by the absence of a route rather than by a check
inside a handler. With the flag, a refresh must carry a header a cross-site
form cannot set, and a foreign `Origin` is refused even so.

It binds to `127.0.0.1` and refuses requests whose `Host` is not a loopback
address, because a page on the public internet can otherwise point your own
browser at `localhost:7777` and read the structure of your code. Everything is
served from the binary: no npm, no build step, no CDN, and a
Content-Security-Policy that forbids loading from any other origin, so it works
offline on a machine with no Node.

Confidence is the visual language of every screen: a proven edge is drawn
solid with a tick, an uncertain one dashed, and guesses are off by default
with the count of what that hides stated in every response. Every
level of the graph states what it did not draw, and every screen ends with what
it could not see.

## Across repositories

```bash
cortex system --member web=../web/.cortex/index.db \
                --member orders=../orders/.cortex/index.db
cortex system --members ./cortex.system.yml
```

Joins several repositories' indexes into one read-only view and reports the
edges that cross between them: an outbound HTTP call in one repository
answered by an endpoint declared in another, and database tables more than one
repository touches. Nothing is merged or migrated; each member keeps its own
index, runs and digest.

Past two repositories, keep the list in a file. The estate is a property of the
team, not of an invocation, and retyping eight flags is how a member silently
goes missing. It is deliberately not `.cortex.yml`, which is per-repository:

```yaml
members:
  - name: web
    index: ../web/.cortex/index.db
  - name: orders
    index: ../orders/.cortex/index.db
```

Confidence is weaker here than anywhere else in the tool, and the output says
so. Matching `/users/{id}` against `/users/:id` is a guess no compiler proved,
so nothing this command produces is `exact`, and an edge's confidence is the
minimum over every leg of the path including the match. A call two members
could both answer is refused rather than assigned. The report always begins
with the members, including any whose index is absent or stale, and ends with
what it could not join; exit status is 2 when a member could not be read, so
the report is a floor rather than a total.

## Runtime correlation

What actually ran, correlated against what could.

### `trace` — what actually ran

```bash
cortex trace otel-export.json
```

Reads an exported OpenTelemetry or Jaeger trace and reports which indexed code it
reached. Static analysis says what *could* run; this says what *did*. An endpoint
the index knows and no span reached is either dead or untested, and both are
worth knowing before a refactor.

It reads an exported **file**, not a live collector. A service that must be
running for RepoCortex to work is a service RepoCortex has no business requiring, and
the export is the artefact CI already produces.

Every match reports **how** it was made, because they are not equally strong:

| Match | Evidence |
|---|---|
| `code-attribute` | OpenTelemetry `code.function` named the symbol outright |
| `route` | `http.route` matched an endpoint the index extracted |
| `table` | `db.sql.table` matched a table the index extracted |
| `name` | the span name matched a symbol name — weakest, and only when unique |

That last one is only attempted when nothing better exists, and only when the
name is unique in the index: a span called `handle` tells you nothing about which
of nine handlers ran.

Spans are **not persisted**. Runtime data ages out in hours while an index
describes a commit; storing them together would produce a graph whose edges are
true at different times, which is the most confusing thing a knowledge graph can
do. And "never reached" is always relative to the export you supplied — an
endpoint absent from one trace window is not dead code.

## Documentation, export and the benchmark

Three ways to get the graph out, and one way to hold the context engine to a number.

### `docs` — architecture documentation derived from the graph

```bash
cortex docs --out docs/generated
cortex docs --check --out docs/generated   # non-zero if the committed copies drifted
```

Writes `ARCHITECTURE.md` and `CAPABILITIES.md` with Mermaid diagrams for
component dependencies, endpoint-to-table flows and inheritance. Every claim
cites the file and line it came from.

The distinction that justifies the command: this is **derived, not written**. A
component appears because the config declares it, an edge appears because an
indexer recorded it, and where the graph is silent the document says so instead
of guessing. A repository that declares no components is told that, rather than
being handed a component model invented from its directory names.

Output is deterministic — the same index regenerates byte-identical files — which
is what makes these committable and reviewable like any other artifact, and what
a model-written document, however fluent, cannot offer. `--check` is the CI form:
a change that alters the architecture without updating its documentation fails
the build rather than being noticed months later.

### `export` — the graph in a format other tools read

```bash
cortex export --format dot | dot -Tsvg -o graph.svg
cortex export --format graphml --out graph.graphml     # Gephi, yEd, Cytoscape
cortex export --format json --out graph.json           # notebooks, jq, diffing
cortex export --format csv --out edges.csv             # spreadsheets, pandas
```

Every format carries edge direction, and every format that can hold a
confidence does. A format that cannot hold something says so on stderr, so a
pipeline reading stdout is unaffected. `--min-confidence` drops facts below a
floor; the default `low` keeps everything. With `--out`, the file is written
beside its destination and renamed into place, so an interrupted export never
replaces a good artifact with a truncated one.

`cortex licenses --check` is the one remaining command, and it is for
maintainers: it verifies `THIRD_PARTY.md` against the module graph, which CI
gates.

### `bench` — measured retrieval quality

```bash
cortex bench fixtures/bench/payment.yml
cortex bench fixtures/bench/payment.yml --fail-on-regression   # for CI
```

Runs hand-written cases through the context engine and scores what comes back.
This exists because every other claim about retrieval quality would otherwise be
an assertion.

```yaml
- id: idempotent-callback
  task: Add idempotency to payment callback processing
  expect: [CallbackController, TransactionService, TransactionRepository, transactions]
  forbid: [MarketingReport]
  max_tokens: 8000
```

**The `forbid` list is what makes the number mean something.** Recall alone is
trivially gamed by returning the whole repository, so a case names both what must
appear and what must not. A forbidden item retrieved is a precision failure, not
a rounding error — it is context an agent will read and act on.

`max_tokens` matters for the same reason: a pack that contains everything is not
a good pack.

What this does **not** measure: whether an agent completed the task. Only whether
the context contained what a human said it should. Conflating those would be the
most flattering possible mistake.

## Checking status

```
$ cortex status
Project:        payment-platform
Index:          READY
Schema:         v2
Files:          10
Symbols:        41
Relationships:  0
Last indexed:   2026-08-16T02:46:05+05:30
Index size:     164.0 KiB
Accuracy mode:  auto
MCP profile:    minimal
Capabilities:   code
```

`Index:` reports one of:

| State | Meaning |
|---|---|
| `ABSENT` | No `.cortex/index.db`. Run `cortex init` |
| `EMPTY` | Schema applied, nothing indexed. Run `cortex index` |
| `READY` | A committed revision with no parse failures |
| `DEGRADED` | A committed revision where some files failed or partially parsed |
| `INDEXING` | Should not persist; a run is in flight or crashed mid-index |

All counts come from the **committed run only** — an in-flight indexing run never
leaks partial numbers into status. Nothing is estimated.

`--json` emits the same report machine-readably, on every command.

---

## Diagnosing with `doctor`

```
$ cortex doctor
[ok  ]  go runtime    go1.25.0 darwin/amd64
[ok  ]  sqlite        3.53.3
[ok  ]  fts5          ENABLE_FTS5 present
[ok  ]  user config   using defaults (no user config)
[ok  ]  repo config   none
[ok  ]  index         /path/to/repo/.cortex/index.db
```

The `repo config` line is worth reading when you are working in an unfamiliar
repository — it tells you a `.cortex.yml` is in play and reminds you of its
limits:

```
[ok  ]  repo config   /path/to/repo/.cortex.yml (indexing settings only; cannot grant execution)
```

### `--verify-index`

Adds the graph invariant gate:

```
$ cortex doctor --verify-index
...
[ok  ]  dangling edges            none
[ok  ]  orphaned symbols          none
[ok  ]  unproven exact edges      none
[ok  ]  cross-run edges           none
[ok  ]  uncommitted visible runs  none
```

| Check | Catches |
|---|---|
| dangling edges | edges pointing at symbols that do not exist |
| orphaned symbols | symbols whose file is gone |
| unproven exact edges | edges claiming compiler certainty without compiler evidence |
| cross-run edges | edges joining symbols from different index runs |
| uncommitted visible runs | more than one run marked committed |

A failure renders as `[FAIL]  dangling edges  12 found — edges pointing at
symbols that do not exist`, and `doctor` exits 1 with `error: N check(s) failed`.

Any failure here means the index is structurally corrupt. `cortex index --full`
rebuilds it; if the failure recurs on a fresh index, it is a bug worth reporting
with the repository that triggers it.

`doctor` also opens an in-memory SQLite database to test the driver, so it works
even when there is no index — in which case it reports `[warn] index absent — run
'cortex init'`.

---

## Configuration reference

RepoCortex reads two files with **deliberately different trust levels**. Which file
a setting lives in is a security decision, not an organisational one — see
[security.md](security.md) and
[architecture.md](architecture.md#the-trust-domain-split-is-a-security-boundary).

### The short version

| | `.cortex.yml` | `~/.config/cortex/config.yml` |
|---|---|---|
| Location | Repository root, committed | User's config dir, never in a repo |
| Trust | **Untrusted** — attacker-supplied when you clone | **Trusted** — only the machine owner writes it |
| Controls | What gets **indexed** | What gets **executed** |
| Unknown keys | **Parse error** | Ignored |
| Can it weaken a default? | No. Excludes are additive; redaction is not expressible at all | Yes — it is yours |

The repository file structurally cannot express execution settings: the fields do
not exist on `config.RepoConfig`. Attempting one is loud, not silent:

```
$ cortex status
error: repo config: parse .cortex.yml: yaml: unmarshal errors:
  line 3: field accuracy not found in type config.RepoConfig
```

### `.cortex.yml` — every key that exists

```yaml
project:
  name: payment-platform        # string. Defaults to the directory basename

index:
  include: ["**"]               # []string. REPLACES the default ["**"]
  exclude: ["generated/**"]     # []string. ADDED to the defaults; cannot remove them

languages: [java, go]           # []string. Parsed and validated; not yet consumed
capabilities: [code, impact]    # []string. Default ["code"]. Reported by `status`

context:                        # bounds every pack `query` and `compile` assemble
  max_files: 20                 # int, must be >= 1
  max_symbols: 100              # int
  max_lines: 5000               # int
  max_graph_depth: 3            # int, must be 1..10

mcp:
  profile: minimal              # minimal | standard | full
```

There is no `security` key. `security.redact_secrets` used to exist and is now a
parse error; redaction is unconditional and was never switchable. See
[What is wired up](#what-is-wired-up-and-what-is-not) below.

There is also no `architecture` example here because it is a section of its own;
see [Architecture governance](#architecture-governance).

Defaults, from `config.Defaults()`:

| Key | Default |
|---|---|
| `index.include` | `["**"]` |
| `index.exclude` | `.git/**`, `node_modules/**`, `vendor/**`, `target/**`, `build/**`, `dist/**`, `coverage/**`, `.cortex/**`, `**/*.min.js`, `**/*.lock`, `**/testdata/**` |
| `capabilities` | `["code"]` |
| `context.max_files` | 20 |
| `context.max_symbols` | 100 |
| `context.max_lines` | 5000 |
| `context.max_graph_depth` | 3 |
| `mcp.profile` | `minimal` |

One asymmetry to understand: `index.include` **replaces** the default when
non-empty; `index.exclude` **appends** to it. That is the point — a repository
can add exclusions but can never un-exclude `.git` or `node_modules`. Negated
exclude patterns (`!...`) are discarded rather than honoured.

`max_graph_depth` outside 1..10 is rejected with an explanation, because
unbounded traversal is a denial-of-service on a large repository:

```
error: context.max_graph_depth 50: want 1..10 (unbounded traversal is a DoS on large repos)
```

#### What is wired up, and what is not

An honest note, kept current against the code rather than against the last time
someone wrote this paragraph.

**Consulted:**

- `project.name`, `index.include`, `index.exclude` — drive discovery.
- `mcp.profile` — selects the tool set `cortex serve` exposes.
- `capabilities` — validated and displayed by `cortex status`.
- `context.max_files`, `max_symbols`, `max_lines`, `max_graph_depth` — become the
  budget for every pack `cortex query`, `cortex compile` and `cortex bench`
  assemble. This entry previously said they were consumed by nothing; that stopped
  being true when the context engine shipped, and it is now pinned by a test
  (`TestContextLimitsReachTheAssemblerBudget`).
- `architecture` — components and rules, checked by `cortex arch check`.

**Not consulted:**

- `languages` is parsed, validated and stored, and no shipped code path reads it.
  Language selection today follows from what the parsers recognise in the files
  discovery finds, not from this list.
- The `context.*` limits do **not** reach the MCP server. `cortex serve` builds
  its packs from the compiled-in defaults, so an agent gets `max_files: 20`
  whatever the repository configured. Use the CLI if the limits matter to you.

**Removed:** `security.redact_secrets`. It is now a parse error rather than a key
that quietly does nothing. Redaction of secrets is unconditional — every file's
text passes through `security.RedactFile` before a search row is written — and it
was never switchable: the old field honoured only `true`, which was already the
behaviour, and ignored `false` so that a cloned repository could not disable
redaction of its own secrets. If your `.cortex.yml` sets it, delete the two
lines; nothing about your index changes.

### `~/.config/cortex/config.yml` — every key that exists

```yaml
accuracy:
  mode: auto                    # fast | auto | exact
  indexers:
    provision: prompt           # never | prompt | auto
    runner: native              # native | docker
    languages: [java, go]       # []string
    timeout: "10m"              # Go duration string. Bounds one indexer subprocess
    mount_local_artifacts: false # bool. Give a containerised build the host's ~/.m2
```

| Key | Default | Meaning |
|---|---|---|
| `accuracy.mode` | `auto` | `fast`: tree-sitter only. `auto`: compiler-backed where a toolchain exists. `exact`: compiler-backed everywhere, fails loudly otherwise |
| `accuracy.indexers.provision` | `prompt` | `never`: download nothing. `prompt`: ask before each download. `auto`: download without asking, checksums still verified |
| `accuracy.indexers.runner` | `native` | `native` or `docker` |
| `accuracy.indexers.languages` | unset | Restrict which languages get compiler-backed indexing |
| `accuracy.indexers.timeout` | `10m` | Bounds one indexer subprocess. Applied to the resolved config; an unparseable duration falls back to the default |
| `accuracy.indexers.mount_local_artifacts` | `false` | Lets a containerised build read and write your `~/.m2`. Off by default: it hands an untrusted repository's build one directory of your machine |

The defaults are deliberately conservative: heuristics rather than executing
builds, prompting rather than downloading.

**Honest note on `accuracy.mode`.** The mode is a real decision under `cortex
indexers`: `fast` plans and runs nothing, `auto` runs the indexers whose
toolchain is present and skips the rest, and `exact` refuses to start when a
detected language cannot be compiler-indexed, naming the language and the missing
tool. It is **not** consulted by `cortex index`, which records the mode on the
run for provenance and never calls the orchestrator. So `exact` does not make an
ordinary index fail; it makes `cortex indexers install` fail. Compiler-proven
edges enter the graph through `cortex scip import`, which you run on the
`.scip` files `cortex indexers install` produces.

### Where the user config lives

`os.UserConfigDir()` + `cortex/config.yml`:

| OS | Path |
|---|---|
| Linux | `~/.config/cortex/config.yml` |
| macOS | `~/Library/Application Support/cortex/config.yml` |
| Windows | `%AppData%\cortex\config.yml` |

`CORTEX_CONFIG_HOME` overrides the directory:
`$CORTEX_CONFIG_HOME/config.yml`. This is the only environment variable
RepoCortex reads outside the soak test suite.

`cortex doctor` reports which one was found:

```
[ok  ]  user config   using defaults (no user config)
[ok  ]  user config   /Users/you/Library/Application Support/cortex/config.yml
```

### Precedence

Defaults, then user config (trusted), then repository config (untrusted, and
structurally unable to reach execution settings). The resolved config is then
validated as a whole.

---

## Troubleshooting

Symptom-first entries with the real messages are in
[troubleshooting.md](troubleshooting.md). The failure modes worth knowing before
you hit them:

### `no index at ... — run 'cortex init' first`

`index`, `resolve` and `watch` all require `.cortex/index.db` to exist. `init`
creates it. Check you are in the right directory, or pass `--project`.

### `no committed index — run 'cortex index' first`

The database exists and is migrated, but nothing has ever been committed. `status`
shows `Index: EMPTY`. Run `cortex index`.

Caveat: `resolve` and `watch` discard the underlying error at this point, so a
genuine database failure (locked, corrupt) is reported with this same message.
If `cortex index` also fails, read *its* error — it does not swallow the cause.

### `SQLite build lacks FTS5 (ENABLE_FTS5 absent)`

`modernc.org/sqlite` enables FTS5 through a codegen flag it does not document, so
RepoCortex checks rather than assumes. Seeing this means you have an unexpected
build of that dependency — a replace directive, a vendored fork, or a platform
the codegen has not covered. `cortex doctor` reports it as `[FAIL] fts5`, and
`init` refuses to proceed.

### `watch: watched directory limit exceeded`

The repository has more than 8192 directories RepoCortex would need to watch. First
check that large trees are actually excluded — `.git`, `node_modules` and
`.cortex` are refused unconditionally, but a monorepo's build outputs are not.
Adding them to `index.exclude` removes them from the walk *and* from the
watcher's predicate.

On Linux you may also hit the kernel first if `fs.inotify.max_user_watches` has
been lowered below 8192; that surfaces as `watch "some/dir": no space left on
device`.

### Oversized files are skipped

Two different limits, and they mean different things:

- `fs.DefaultMaxFileSize` = **2 MiB**. Discovery ceiling. The file is never
  hashed and never read; it is recorded with `skip_reason = too_large`.
- `parse.MaxSourceBytes` = **256 KiB**. Parse ceiling. The file *is* hashed and
  recorded, but not parsed: `source is N bytes, over the 262144 limit`.

Both are compile-time constants, not flags. If you are losing symbols you care
about, the right fix is almost always to exclude the generated tree rather than
raise the cap — see
[performance.md](performance.md#tuning-for-a-large-monorepo) for the memory cost
of raising it.

### The index looks stale after a branch switch

It should not — diffing is by content hash, not mtime, precisely because `git
checkout` rewrites timestamps wholesale. If you are running `cortex watch`,
the startup reconcile catches changes made while it was stopped. If you are not,
run `cortex index`.

### `This index was written by cortex X and you are running Y`

Not an error, and the query still answers. An index is data **plus the resolution
rules that produced it**, and those rules change between builds — resolver
behaviour on this project has been changed and reverted more than once after
measurement. An index written either side of such a change holds different edges
for identical source, and nothing about querying it looks wrong: it succeeds, and
its results carry the same confidence tiers a current index would.

So each run records the build that wrote it, and a read against an index from a
different build says so on stderr. Run `cortex index && cortex resolve` to
refresh. The warning never appears on stdout, so it cannot contaminate `--json`
output a script is parsing.

A variant says the index **does not record** which build wrote it. That is an
index created before the stamp existed. It is a different claim from a version
mismatch — unknown provenance rather than known staleness — and it is reported
differently for that reason. Re-indexing resolves both.

### A file failed to parse

The run still succeeds. A single unparseable file must not cost the entire index,
so the file is recorded with `parse_status = 'failed'` and its error text, the
run is published as `degraded` rather than `ready`, and `cortex status` reports
the failure count. Query the `files` table for the reason per path.

---
