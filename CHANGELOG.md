# Changelog

All notable changes to RepoCortex are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Entries explain *why* a change was made and what failure mode it addressed,
matching the standard the commit history sets. Every measurement quoted here came
from an actual run.

## [Unreleased]

### Added
- **`cortex serve --http host:port`** serves MCP over Streamable HTTP, for clients that accept only a remote server (ChatGPT developer mode, Android Studio). Stdio stays the default. Every request needs a bearer token, from `CORTEX_MCP_TOKEN` or generated per run into `.cortex/mcp-token` (mode 0600, never printed), sent as `Authorization: Bearer` or as the last path segment for clients that only take a URL; the listen address must be loopback unless `--allow-remote` is passed; `Host` and `Origin` must name this machine or a host given with `--allowed-host`, which blocks DNS rebinding and calls from web pages. The index stays read-only.
- **`cortex install` configures 30 coding agents** in the format each one documents, prints the exact step for agents configured through their own UI or command (`--list`), creates `AGENTS.md` guidance, and writes a CLI block to `CONVENTIONS.md` for Aider, which has no MCP.
- **`cortex rename <old> <new>`** renames a symbol at its definition and every resolved reference, for the case where an agent or a person today edits a rename by hand from a plan and misses or mis-edits a site. It is a dry run by default. `--apply` writes only when every site is at the floor, which is `exact` (compiler-proven) unless `--floor high` is passed (that prints the measured 86.8-96.1% precision before applying), the file is unchanged since indexing, the identifier is unambiguous on its line, the file is a writable regular file inside the project root, and the new name is not already defined in an edited file; otherwise it writes nothing, lists every failing site and exits 2. Files are written atomically with the hash re-checked before each replace and restored if a later write fails; one edit runs per project root at a time, behind a `.cortex-edit.lock` that is never stolen (a stale one is reported with its path for you to delete). Afterwards a task contract is written to `.cortex/rename-contract.json` and `verify` runs on the changed files. Exit codes: 0 applied and verified, 1 error or verify failure or incomplete rollback, 2 refused with nothing written, 3 applied but not verified complete. Because RepoCortex never compiles, an apply that leaves the old name anywhere in an indexed file lists those lines under `LEFTOVER OCCURRENCES` and exits 3, never 0; unbound references and interface implementations without an edge are not renamed, and every run says to build and test.
- **`cortex indexers pins`** prints the pinned compiler indexers (name, version, channel, reference, digest) from the binary alone, as a CI cache key that changes exactly when a pin does; `cortex indexers install --out-dir DIR` writes the `.scip` files to a known, empty directory instead of a random temporary one.
- **Compiler-backed gate.** The change-safety workflow installs the pinned SCIP indexers and imports their facts before `impact`, caching scip-go on the pin table, so the blast radius walks `exact` edges wherever a compiler exists. Without it the gate's blast radius was heuristic everywhere. The step is an upgrade, never a requirement: it never fails the job, degrades to heuristic edges with a `::warning::`, lists every skipped language and its reason in the step summary, and never passes `--allow-repo-build`, so fork pull requests run only pinned, digest-verified indexers with a read-only token. `CORTEX_ACCURACY: fast` turns it off.
- **Proven share of the blast radius in the PR report.** The summary row and a "How much of the blast radius is proven" table show the share of `impact`'s reached symbols that are `exact`, `high` and `medium` (interface-dispatch-only symbols are not counted; unrecognised values appear as `unlabelled`), and say so when none of it is compiler-proven, so a reviewer cannot read a heuristic blast radius as a proven one.
- `registry.EdgeHit.EvidenceCol` exposes the stored evidence column, documented as a hint.
- **`cortex report sarif|pr`** renders the JSON from `arch`, `missed` and `impact` as a SARIF 2.1.0 file for code scanning or as one bounded Markdown pull-request comment that starts with a sticky marker. Each input is shape-checked, findings with no file are counted rather than dropped, repository text is escaped, and the comment is capped at 60000 characters.
- **Change-safety gate v2.** The shipped workflow now collects findings, uploads SARIF under the category `cortex`, and posts or edits a single pull-request comment before it enforces `arch` and `missed`, so a failing build still explains itself. It needs `pull-requests: write` and `security-events: write`; fork pull requests get no comment.
- **"Indexing in progress" answers over MCP.** During a first index every tool now says the index is still being built, instead of "no index has been built" (which sent agents to start a second index), and an answer from a committed run adds a note when a newer run is being built.
- **Event topology, blindspot audit, refactor plans, pruning and diff minimization.** `cortex events` maps producers and consumers of Kafka, RabbitMQ, SQS, SNS and Pub/Sub topics; `cortex missed` audits unresolved calls, untested entrypoints, unmapped endpoints, orphan tables and undeclared config; `cortex plan-change` emits an ordered refactoring plan with preconditions, affected tests and post-checks; `cortex prune` reduces a file to its declarations; `cortex diff-minimize` compresses a unified diff. The MCP server exposes `plan_change`, `detect_blindspots`, `get_file_outline` and `minimize_diff`; event topology is CLI and portal only. Prisma joins GORM, JPA and SQLAlchemy as a mapped ORM.
- **Portal screens for events, blindspots and refactor planning**, a Command Palette (`Cmd+K`) and a symbol inspector drawer.
- **The code map: Graph, Tree, Treemap and Code views of one index.** The portal's graph screen now has four views behind one breadcrumb and one tab strip, sharing the map's address (`container`, `level`, `sel`), so a symbol selected in one view is the one selected in the others. The Tree lists every directory (the map draws the 48 largest) and loads files and symbols as they open; the Treemap shows where the code is, sized and coloured by a measure; the Code view shows a file's indexed text with its declarations, callers and callees, and the lines relationships cite. It reads the new `GET /api/source`, which serves only text the index holds, from the full-text copy taken at index time: there is no filesystem read, so no path outside the index can be reached. Canvas tools gained words and pressed states in place of emoji, and the layout is a select kept in the URL.
- **In-memory CSR graph.** `graph.New(db, graph.WithCSR())` answers `Traverse` from a compressed-sparse-row snapshot of the committed run, loaded on first use and replaced when the committed run changes. `cortex serve` and `cortex ui` enable it; library and CLI one-shot paths keep the SQL traversal. A parity test pins identical nodes, depths and confidences.
- **`cortex resolve` imports an `index.scip` found in the project root or `.cortex/`**, upgrading matching edges to `exact`. The outcome is always printed (and under `scip` in `--json`), and `--no-scip` opts out.

- **`cortex missed --fail-on <severity>`** exits non-zero when a blindspot at that severity or worse exists, so the audit can gate CI. The change-safety workflow now builds the call graph (`resolve`), checks `doctor --verify-index`, enforces `arch --fail-on` and `missed --fail-on`, and prints the pull request's blast radius in the job summary.

### Fixed
- **Lint cleanup.** CSR graph traversal truncation (node cap) is now an explicit reported reason instead of errors silently dropped, the SQL and CSR traversals agree on the truncation reason, and indexing into the CSR arrays is bounds-checked against `int32` overflow.
- Dangling-symlink escape in `security.ValidatePath`: a path through a symlink whose target did not exist yet passed validation; it is now followed and rejected when it leaves the root.
- `cortex --json arch --fail-on` never failed: the JSON branch printed the report and returned before the severity gate was reached, so the same command exited 0 in JSON mode and 1 in text mode. The gate now runs on both paths.
- The portal's code tree sent its "Map" button to `?c=`, which the map does not read, and its inspector asked the blast-radius screen for `target`, which it does not read either; both are gone with the old tree. The Command Palette's view shortcuts changed the URL without showing the graph screen when another screen was open.
- The shipped CI workflow could not fail: it ran `cortex impact "$file" || true`, but `impact` takes `--base`/`--head` and no file argument, so every call errored and the error was swallowed. It also never ran `resolve`, leaving the call graph empty. `missed` had no failing mode at all. All three are fixed, and tests pin that the workflow has no `|| true` and that the user guide carries it verbatim.
- `cortex missed --kind` with a mistyped kind filtered every blindspot out and printed "No code intelligence blindspots detected"; it now rejects an unknown kind and lists the valid ones. Its report also wrote to the process's stdout rather than the command's writer, and listed categories in random order.
- A failed SCIP auto-import was discarded without a trace, so a skipped `exact` upgrade looked the same as "no SCIP index present". It is now reported with the path and the cause; `resolve` still succeeds, since the heuristic graph is valid.
- `LoadCSR` selected `src_symbol_id` and `dst_symbol_id`, columns that do not exist (the schema has `src_id` and `dst_id`), so the CSR graph could not load from a real database. Caught by the new parity test before anything called it.
- The user guide named MCP tools the server never registered (`get_blast_radius`, `get_event_topology`, `get_refactor_plan`, `prune_file_ast`, `compress_diff`), and two server guidance notes pointed agents at `get_blast_radius` and `find_database_tables`. They now name `impact_analysis` and `find_db_access`, and a test keeps the guide and the registered tools in step.
- The README described a 1.0.0 GA release and "100% retrieval recall"; the project is at 0.1.0 and that figure is one five-question set.

### Security

- **`get_file_outline` no longer reads arbitrary paths.** The MCP tool read whatever path an agent supplied (path traversal, including `../` and absolute paths). It now reads through `os.Root` anchored at the project, and sensitive files (keys, credentials, environment files) are suppressed rather than outlined.
- **Web repo-file reads are contained** the same way, so a request to the portal cannot name a file outside the project.
- **Contained reads refuse anything but regular files and are size-capped,** so a FIFO, device or oversized file cannot block or exhaust a reader, and one reader serves every caller instead of each re-implementing the check.

## [0.1.0] — 2026-09-06

The first release. Its candidate exercised the release job — archives,
SHA256SUMS, SBOM and provenance — end to end; `cortex version` reports what
`-ldflags` injected rather than `dev`.

### Added
- **NestJS routes survive Swagger-style decorators and helper classes.** A decorator whose arguments spanned lines (`@ApiResponse({` ... `})`) lost the handler, and `@UseGuards(` followed by `AuthGuard('jwt'),` named the guard as the handler; the lookup now follows a decorator's brackets to their close. `@Controller` attaches to the next class only, so a helper class later in the file cannot borrow its prefix. And the language spec's test-file matcher splits directory segments on `/` alone, so it agrees with the classifier `verify` reads on a C# `Order.Tests/` project.
- **A preflight for the Docker credential helper.** Docker Desktop's `docker-credential-desktop` sometimes never returns from a shell with no Desktop session, so `docker pull` blocked before reaching the daemon and RepoCortex reported the pinned image as `could not install` after its ten-minute provisioning timeout — true, and naming nothing. Before pulling, the runner now resolves the helper the Docker CLI would consult for the image's registry (`credHelpers`, else `credsStore`) and asks it for `list` with a five-second bound; a helper that does not answer is reported by name, with the workaround (`DOCKER_CONFIG` at an empty directory, since a digest-pinned public image needs no credential). A helper that answers, fails, or is absent is left to `docker pull`, which reports those well. Measured cost when the helper is healthy: 170 ms.

- **Endpoint discovery for Django and NestJS.** A Django or Nest service
  reported zero endpoints, and an empty API surface reads as "no API" rather
  than "framework not covered". Django `path()` and `re_path()` entries in a
  urlconf are now routes at `medium` confidence with no verb — the view
  dispatches, and the `include()` prefix that mounts them is not visible from
  the file. `include()` itself and `admin.site.urls` are mounts and are
  skipped; a decorator-wrapped view is named by the view, not the decorator.
  NestJS routes come from a `@Controller` prefix and the verb decorators
  beneath it at `high` confidence; a decorator argument the scan cannot read
  (an array of paths, a wrapped object) is a miss rather than an invented route
  at the bare prefix. `cortex api` and the MCP discovery summary print the
  covered list — `spring, go-http, go-router, fastapi, flask, django, express,
  nest` — beside every result.
- **A declared language spec for C#, and test conventions for Ruby, Rust and
  PHP.** `c_sharp` sat in the `resolved` tier on the fallback spec, which
  declares no interop, so a `.cs` reference could bind to a Go or Python
  definition. Ruby, Rust and PHP had separators and no idea what a test file
  looks like, so `cortex verify` answered "not a test" for every `spec/` and
  `tests/` file in those languages. Each is now closed to itself on the rule the
  frontend languages already use — a wrong edge is worse than a missing one —
  and `internal/capability/testfiles.go`, the classifier `verify` actually
  reads, knows PHPUnit's `Test.php` and Cargo's `benches/`.
- **The portal's Playwright suite runs in CI.** Before the `portal-e2e` job,
  the Go and JavaScript behind `cortex ui` had no automated coverage a pull
  request could see; the suite needed an indexed repository on the developer's
  machine. A fixture tier that ships with the repository now drives it on every
  pull request — 149 passed, 23 skipped on the ci tier — with traces uploaded
  on failure.
- **"Not drawn", in visible text.** A relationship row on a graph card can
  point at an entity the level did not draw; that fact lived only in a hover
  title, which a phone, a screenshot and a screen reader never see. The row now
  says so, the level's omission note counts them, and a symbol card whose edge
  count and drawn rows differ states both populations, on a scope line that
  wraps, so they add back up.
- **Keeping the index current: `cortex hooks install`.** Writes a Git
  post-commit hook that re-indexes after each commit, so the portal and any
  connected agent describe the commit you just made rather than the one you made
  on Tuesday. A stale index is worse than no index because it looks current.
  The hook is written inside markers: an existing `post-commit` is appended to,
  never replaced, and `hooks uninstall` removes exactly the marked block. It runs
  detached and swallows every failure — a hook that can fail a commit is a hook
  people delete. Measured on a real repository: the commit itself took 0.79s and
  the re-index completed behind it.
- **Refreshing the index from the portal: `cortex ui --allow-refresh`.** Off by
  default, and the default is the point. The portal's security claim is that
  pointing a browser at localhost cannot make RepoCortex do anything, so without
  the flag the write route is *never registered* — read-only is enforced by the
  absence of a route rather than by a check inside a handler that a later edit
  could reorder. With the flag, a refresh must carry a header that a cross-site
  form cannot set and that `fetch` can only add behind a preflight this server
  never approves; a foreign `Origin` or `Sec-Fetch-Site` is refused even so, and
  refreshes are serialised so two indexers cannot write one SQLite file.
  `GET /api/indexstate` reports provenance, refresh state and hook status.

- **MCP server (`cortex serve`).** Serves the index to coding agents over
  stdio. The database is opened read-only, so an agent session cannot mutate the
  index it is querying. Tool count is treated as a performance constraint rather
  than a preference: every exposed tool's schema enters the agent's context on
  every request, so the `minimal` profile ships six tools and a test fails if
  that grows past eight. `--list-tools` prints the exposed set without starting a
  server.
- **Bounded, framed MCP responses.** Whole files are never returned. Bounding is
  enforced centrally rather than per tool, because a bound each tool implements
  for itself is one a tool will eventually forget. Repository content arrives
  labelled `SOURCE EVIDENCE` with an explicit note that it is data, not
  instructions, because indexed source may have been written by someone other
  than the user.
- **Two honesty properties in MCP output, enforced by tests.** An empty caller
  list is not presented as proof — the index resolves only part of any
  repository, so every empty relationship result carries the unresolved and
  ambiguous counts. And the confidence *and reason* for each binding reach the
  agent: "receiver type matches" is the difference between a fact an agent can
  weigh and one it must take on trust.
- **Graph traversal (`internal/graph`).** Level-synchronous BFS — one query per
  depth level, not per node. Confidence is tracked along each path, so "reachable
  at depth 3 through a low-confidence edge" stays distinguishable from a proven
  chain. Benchmarked at 100k symbols / 500k edges: depth-3 traversal p95 2.9 ms
  at 1k nodes, 86.5 ms at 50k. `MaxNodes`, not depth, is what keeps it
  interactive.
- **Reference resolution (`cortex resolve`).** Binds extracted references to
  their definitions, building the call graph — the question every downstream
  capability depends on. Five confidence tiers, each ruling out a specific way of
  being wrong: same file, receiver-type match, same directory, single import
  match, unique name only. Measured at **840 ns per reference**, so a
  531k-reference repository resolves in under a second. On the payment fixture it
  reconstructs `handleCallback -> updateStatus -> {save, findById}` from source.
- **Persisted references and imports (migration 0002).** Resolution needs the
  complete symbol table, which does not exist until every file is parsed, so it
  is inherently a second pass. Persisting means re-resolution — after the
  resolver improves, or once a SCIP index arrives — costs no reparsing.
- **Filesystem watcher (`cortex watch`).** Debounced, coalesced change sets. It
  reconciles before watching, because files change while the watcher is not
  running and starting from a stale index would leave those edits invisible until
  the affected file happened to be touched again. Verified live: modify, add and
  delete on a running watcher updated the index in 102 ms, 14 ms and 2 ms.
- **Observability (`internal/observe`).** All 13 metrics, off by default, 2.85 ns
  and zero allocations per span when disabled. Attributes go through typed
  constructors and a validator that panics in tests, so a file path or a secret
  cannot reach a span — telemetry from a private repository must never carry its
  contents.
- **Error taxonomy (`internal/errs`).** Eight kinds over stdlib wrapping.
  Retryability is the one that earns its keep: SQLite `BUSY` is transient, a
  corrupt index is not, and callers can now tell without string matching. Driver
  codes are matched structurally, so the package stays a leaf with no driver
  dependency.
- **Soak suite (`tests/soak`).** Build-tagged so it never slows the normal suite.
  Watcher endurance, directory churn, index/query concurrency, and digest
  stability across 110 passes. It reports trends rather than only passing — a
  soak test whose only output is `PASS` teaches nothing.
- **M0 foundations.** Apache-2.0, CI across three operating systems plus a
  CGO-free build matrix, versioned and checksummed migrations, two-trust-domain
  config, `init` / `status` / `doctor`.
- **M1 source indexing.** Discovery with full gitignore semantics, content
  hashing, symbol extraction across 206 grammars, import extraction, FTS5 search
  rows, git intelligence, two-phase atomic swap, and incremental indexing.
  Measured on the payment fixture: full index 219 ms, no-op re-index 14 ms,
  single-file change 55 ms. RepoCortex indexes its own source at 57 files, 804
  symbols.

### Fixed
- **The docker runner no longer writes a JDK cache into the indexed repository.** The container ran as the invoking uid with `HOME=/out`, which Maven honours, but the JVM's `user.home` comes from the passwd entry for that uid and resolves to `?` inside the image; scip-java fetches its JDK through coursier, a JVM program, so 611 MB of Temurin landed in a directory named `?` at the repository root, untracked and not ignored. The runner now also passes `JDK_JAVA_OPTIONS=-Duser.home=/out`, which every JDK 9+ launcher reads without displacing the image's own `JAVA_TOOL_OPTIONS`. Verified on spring-petclinic-rest: a re-index leaves `git status` clean and the cache lands beside the index.
- **The indexer output directory holds only the indexes it produced.** Containing a build's caches in the output mount is not a promise to keep 700 MB of somebody else's JDK under the system temp directory until the OS gets round to it, and `Results.OutputDir` was documented as the caller's to remove while no caller ever did. `indexer.Execute` now prunes everything but the `.scip` files before returning, on every exit path, and removes the directory outright when no index was produced. Only directories it created under the temp root are touched.

- **The lint job was red, so a new finding would land unread.** golangci-lint
  at the pinned v2.12.2 reported 28 findings on `main`. Every fix was
  mechanical; the hook-permission and path-containment findings are false
  positives and are excluded with the reason stated beside each exclusion.
- **The cycle-budget timing assertion flaked under `-race`.** The adversarial
  shape takes 1.4–2.3 s against a 2 s budget on the instrumented binary, which
  is a flake waiting for a slower runner. The wall-clock comparison now runs
  only on the binary that ships, through the same build-tagged constant
  `internal/context` already uses; the truncation and step-cap checks still run
  under `-race`.
- **`.cs` files were classified as `csharp`, a name no grammar carries.** The
  grammar is `c_sharp`. Declaring the C# spec exposed it: under the old name
  every C#-to-C# binding would have been refused the moment the spec existed.
  `languageOfPath` now returns the grammar name, with a regression test.
- **An incrementally-updated index named the wrong commit.** Only the full index
  path creates a run, so only it stamped a revision at `BeginRun`; the
  incremental path updates the committed run in place and left it naming
  whichever commit it was *first* built at, while its contents described a later
  one. `head_matches_build` then reported a mismatch for an index that was in
  fact current — and both the portal's index bar and the MCP provenance block
  read that field. A provenance claim that is wrong is worse than one that is
  absent, because a reader told twice that a current index is stale stops
  believing the field at all. Fixed by stamping the revision after a successful
  incremental pass, mirroring the existing producer stamp.
- **The portal claimed `read_only: true` while able to write.** With
  `--allow-refresh` the portal serves a route that rebuilds the index, and an
  operator checking that field to decide whether it is safe to leave the portal
  running was being told the opposite of the truth. It now reports reality on
  `/api/health`, `/api/overview` and every other `meta` block.

### Changed
- **The portal now reads in the words a person already uses, not the product's.** A reader used to meet a caveat, four bullets and a command before the answer, a rail grouped by subsystem, and a confidence tier printed as `exact`/`high`/`medium`/`low`. The rail's groups are now named by what a reader wants to know — Understand the code, Before you change something, How it runs, Quality, For your AI agent — with Index health standing alone above them, twenty screens in all. The four tier ids stay in every field and flag, but read on screen as proven / very likely / uncertain / a guess, each carrying the measurement that earned it; the handful of terms that cannot be replaced collect behind one "What do these words mean?" glossary button. Every screen now opens with its answer in one sentence built from its own data; the evidence and the things the view can't see fold one click below, closed by default, and `?open=1` unfolds all of them at once for an agent or a screenshot — not a count, reason or remedy is dropped, only deferred. The map moved above the fold: the canvas used to sit under three paragraphs and a seven-control panel and now starts within the first 560px of the page at 1440×900, with the five filters collapsed into one "Filters" popover that reports how many are changed. A node is now labelled by the shortest path-tail no sibling also ends in rather than a stripped common prefix, and sized by its symbol count (log2(1 + symbols), clamped to [NODE_MIN_W, NODE_MAX_W]) so a 10,000-symbol container reads wider than a 10-symbol one. Layout is now ranked rows by dependency direction — callers above callees, ties broken by neighbour barycentre and then key — in place of a spring simulation, so the same URL lays out identically on every cold load (payment's ten containers moved from an 873×352 spring footprint to 699×257 laid out in rows). Every edge, table row, card line and inspector row now reads as a sentence — `web uses registry, 12 times` — instead of an id and a multiplication sign. `internal/web/e2e/specs/18-plain-language.spec.mjs` is the rule that holds all of it: no product jargon on screen, an answer line on every screen, a closed fold by default. On the wire, the omissions block's `label` text changed from "hidden by the confidence floor" to "hidden below the standard of proof" — a value, not a field, so no client reading fields breaks. The dependencies screen's "what you get" sentences and `cortex accuracy`'s miss explanations were reworded to the same vocabulary. The shell modules under `internal/web/assets/js/` cover the rail, the tier words, the glossary and the answer line; the graph code stays in `app.js` for now, and the spec's `graph/` modules are not yet split out.

- **Java is measured.** It was the one indexed language with no compiler ground
  truth. Against `scip-java` on spring-petclinic-rest — the first Maven
  repository found that meets the closing condition `docs/accuracy.md` had set
  — `high` is 100.0% precise on 82 of 82 judged edges, with 32.7% recall: two
  thirds of known call sites produce no edge, the weakest recall of the four
  measured languages, on a small pool. TypeScript arity was measured on nest in
  the same pass and not shipped: the before and after reports were
  byte-identical (high 95.8%, recall 37.5%), so `internal/parse` was reverted
  and TypeScript stays `resolved`; `docs/accuracy.md` records why.
- **The release job waits for the portal suite.** `release` now needs
  `portal-e2e` alongside lint, test, cgo-free and security, so a tag cannot
  ship a portal no browser has driven.
- **Default worker count is 4, not `runtime.NumCPU()`.** Peak memory tracked
  worker count rather than repository size. On a 10,920-file tree: 2 workers
  1,293 MB / 58 s; 4 workers 1,595 MB / 54 s; 8 workers 2,502 MB / 54 s; 10
  workers 6,531 MB / 44 s. Going from 4 to 10 bought 10 seconds and cost 4 GB.
  Parsing is bound by memory bandwidth, not CPU, so throughput saturates long
  before concurrency does.
- **`parse.MaxSourceBytes` lowered from 2 MiB to 256 KiB.** Measured across caps
  on a 2,110-file corpus: 64 KiB → 962 MB, 256 KiB → 2,260 MB, 2 MiB → 6,667 MB.
  Twenty-two files cost 4.4 GB. 256 KiB is far above hand-written source and
  below essentially all generated code. Skipped files are still recorded, so the
  gap stays visible.
- **In-flight byte budget added to the pipeline.** A worker count bounds
  concurrency but not memory when file sizes vary by orders of magnitude. Workers
  now acquire budget proportional to what they are about to parse, so a large file
  waits and then runs alone. Net result on the same tree: **6,531 MB → 1,608 MB
  peak, 44 s → 49 s** — four times less memory for eleven percent more time.
- **`ANALYZE` at commit.** SQLite had no planner statistics and guessed badly on
  this schema: filtering by confidence made it scan every edge, ~400x slower than
  the same query unfiltered, and an `ORDER BY` turned a 1.9 ms neighbour lookup
  into 235 ms. A bounded `ANALYZE` plus `PRAGMA optimize` now runs after commit.
  Statistics fix the cause; the `INDEXED BY` hints that were covering for it only
  pinned one plan.
- **Symbol deduplication ranks by specificity.** Previously it sorted by kind, so
  the catch-all kind could win on alphabetical order. The catch-all is now ranked
  lowest and can never displace a real kind.
- **README states what actually works and what does not.** Corrected in both
  directions. Overstating readiness in a README is how a project loses the trust
  it needs most in its first weeks.

### Fixed

- **Foreign keys were off on 97% of connections.** SQLite pragmas are
  per-connection; `db.ExecContext("PRAGMA foreign_keys = ON")` configures one
  connection out of eight. `ON DELETE CASCADE` silently did not fire, and the
  verification query passed because it reused the connection it had just
  configured. A 30-second concurrent soak produced 1,010 dangling symbols and 505
  orphaned FTS rows that later abort indexing outright. Pragmas now go in the DSN
  so every connection gets them, and verification samples 32 goroutines
  concurrently — because a single-connection check is precisely what failed to
  catch this.
- **Java constructors were stored as `function`, not `method`.** Found by
  differential testing against the C tree-sitter runtime — a bug the pure-Go
  suite could not see, which is the entire argument for building that test.
  Overlapping patterns tagged constructors twice and deduplication sorted by
  kind, so `function` won alphabetically.
- **Missing symbol kinds recovered by per-language supplements.** Java
  `@interface`, TypeScript abstract classes, method and abstract-method
  signatures, generators, and Python module-scope constants were extracted by no
  grammar. Also: `gotreesitter` ships zero explicit tags queries — all 206 are
  inferred, and the Go query silently dropped type, const and var declarations.
- **camelCase was unsearchable.** FTS5's `unicode61` tokenizer treats an
  identifier as one opaque token, so a search for `callback` could not find
  `handleCallback`. Fixed with an `expanded` shadow column holding
  identifier-split text; the original text is kept so `snippet()` still returns
  real source.
- **`.git/**` matched `.git` itself**, because `**` spans zero or more path
  components. Git internals would have been indexed.
- **A byte-length check let single-character noise tokens into the search
  index.** U+0130 is two bytes but folds to the single character "i". Found by
  fuzzing in four seconds; the failing input is committed as a seed.
- **`store.OpenMemory` used `cache=shared`,** meaning every in-memory store was
  the *same* database. Tests would have contaminated each other intermittently.
- **Pooled parser arenas were never released.** `gotreesitter` holds them as
  strong Go references until drained, so peak memory became the sum of every parse
  rather than the largest few. Draining is now conditional on heap size — an
  unconditional periodic drain measured 17% lower peak for 80% worse throughput,
  because it discards warm parser state while memory is perfectly healthy.
  Additionally, pooled `Tagger`s hold arenas outside the library's own pool; a
  10,896-file run showed 3 GB held that way.
- **The sink context outlived the worker context.** `database/sql` rolls back a
  transaction as soon as the context it was created with is cancelled, so passing
  the internal cancellable context to the `Sink` would have silently destroyed the
  rows just written and failed the caller's `Flush` with `ErrTxDone`.

### Security

- **Repository config cannot cause execution.** `.cortex.yml` is
  attacker-controlled the moment someone clones an untrusted repo, so it may
  describe what to index but never what to run, download, or whether secrets are
  redacted. Execution fields do not exist on the repository config type, and
  unknown keys are a **parse error** — so hostile fields do not merely fail to
  apply, they fail loudly. Guarded by `TestRepoCannotSetExecutionSettings`, run
  by CI as a named security gate.
- **Repository excludes are additive and cannot be negated,** so a repository
  cannot un-exclude `.git` or `node_modules`. Negated exclude patterns are
  discarded rather than coerced, because rewriting `!**` as `**` would honour the
  letter of the rule while handing a repository a way to blank its own index.
- **Symlinks are never followed,** and links resolving outside the root are
  recorded as `symlink_escapes_root` so the attempt stays visible. A repository
  committing a symlink named `config.go` pointing at `/etc/passwd` would
  otherwise have its target hashed, indexed, and quoted back by an agent.
- **Irregular files are never opened.** Opening a committed FIFO blocks until a
  writer appears, which would hang the indexer indefinitely.
- **Fuzz targets on everything reading untrusted input** — ignore-file matching,
  pattern compilation, parsing, identifier tokenisation, and SCIP index framing.
  215k parser executions and 1.3M tokenizer executions clean at the initial
  import.

### Verification gates

Each has been observed *failing* on an injected defect, not only passing. A check
only ever seen passing is not evidence of anything.

- **Determinism** — index twice, compare the registry digest. Stable across
  1/2/4/8 workers, and *sensitive* to a single added symbol. A digest that never
  changed would pass the stability half while detecting nothing.
- **Incremental == full re-index** — the digest after add/modify/delete/rename
  equals a full re-index, so incremental updates cannot drift from ground truth.
- **Graph invariants** — `doctor --verify-index` catches dangling edges, orphaned
  symbols, cross-run edges and unproven `exact` claims.
- **FTS5 dependency guarantee** — pinned per platform, because
  `modernc.org/sqlite` enables FTS5 through an undocumented codegen flag.
- **Migration tests** — fresh, upgrade, interrupted, and edited-migration.
  Shipped migrations are immutable; an edited one is a hard error.
- **CGO-free build matrix** — all five release targets cross-compile from one
  machine and the full suite passes with `CGO_ENABLED=0`, so `go install` works
  everywhere and contributors need no C toolchain.

### Not yet implemented

Stated plainly rather than implied: SCIP orchestration (M3) — no compiler-backed
indexer is provisioned or run, so no edge currently carries `exact` confidence
and all three accuracy modes produce the same heuristic result; the context
engine (M4); and the capability layer (impact, tests, API, DB, config,
dependency, architecture, runtime).

[Unreleased]: https://keepachangelog.com/en/1.1.0/
