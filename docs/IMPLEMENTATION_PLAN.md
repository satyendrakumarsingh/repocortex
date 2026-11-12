# Implementation plan

How RepoCortex closes the gaps found in the [competitive analysis](COMPETITIVE_ANALYSIS.md):
what shipped, what is next, and what is declined. Written 2026-11-12.

Each item names the competitor capability it answers, the design constraint it
has to respect, and how it is verified. Nothing here has a date; items move to
the [changelog](../CHANGELOG.md) when they ship and to the
[roadmap](ROADMAP.md) while they wait.

## Contents

- [Goal](#goal)
- [Constraints every item must respect](#constraints-every-item-must-respect)
- [Phase 0: shipped](#phase-0-shipped)
- [Phase 1: distribution and proof](#phase-1-distribution-and-proof)
- [Phase 2: depth](#phase-2-depth)
- [Phase 3: breadth](#phase-3-breadth)
- [Declined](#declined)
- [Findings from building phase 0](#findings-from-building-phase-0)

## Goal

Be the best open-source code graph for coding agents, on every protocol an agent
can speak. Concretely:

1. An agent reaches the same tools over MCP (stdio or HTTP), plain HTTP with
   JSON, or a shell, and gets the same answer on each.
2. Every capability a paid or open-source competitor offers that can be built
   deterministically and locally is either shipped or planned here.
3. Every answer stays bounded and honest at the size of a large monorepo, and a
   test proves it.

## Constraints every item must respect

These come from the [architecture document](DESIGN_AND_ARCHITECTURE.md) and the
[non-goals](NON-GOALS.md). A feature that needs one of them relaxed is declined,
not negotiated.

| Constraint | What it rules out |
|---|---|
| No model, no embeddings, no network | Semantic search, model-written wikis, AI review comments |
| `exact` only from a compiler | Any heuristic that promotes its own output |
| Bounded memory and bounded answers | Loading a whole symbol table; an answer that grows with the repository |
| Honest incompleteness | A list presented as complete; "no result" presented as "none exist" |
| One tool surface | A capability that exists on one protocol and not the others |

## Phase 0: shipped

Built and tested in this change set.

### Capabilities

| Capability | Competitors that have it | Tool | Command |
|---|---|---|---|
| Unused-code detection, graded | codebase-memory-mcp, code-review-graph, Repowise, jCodeMunch, five more | `list_dead_code` | `repocortex dead-code` |
| Change hotspots and change coupling | CodeScene, Repowise, jCodeMunch, axon, CKB | `list_hotspots` | `repocortex hotspots` |
| Path between two symbols | GitNexus, Graphify, Repowise, codebase-memory-mcp | `trace_call_path` | `repocortex path` |
| Type hierarchy, both directions | Serena, jCodeMunch, codebase-memory-mcp | `find_implementations` | `repocortex implementations` |
| Callees for agents | Graphify, jCodeMunch | `find_callees` | `repocortex callees` |
| Tests for one symbol, from the shell | Repowise | `find_tests` | `repocortex tests` |
| Audit log of agent calls | Sourcegraph, Cursor, Graphite, Kodus | all tools | `--audit-log`, `REPOCORTEX_AUDIT_LOG` |
| Session guidance and workflow prompts | GitNexus, code-review-graph | MCP instructions, 3 prompts | not applicable |

How each differs from the competitor version:

- **Unused code is graded, not listed.** A row is `likely` only when it is
  unexported, its file parsed cleanly, no unbound reference is spelled like it,
  and its name appears nowhere in the code outside its own definition. The last
  check was added after the first version listed every MCP handler in this
  repository as unused: each is passed to a registration call as a value, which
  leaves no call edge. Everything else is `possible`, with the reason, and is
  not shown unless asked for.
- **Hotspots report coupling the graph cannot see.** Each ranked file lists the
  files usually committed with it. Where no call or import connects them, that
  is a dependency `analyze_impact` misses, and the answer says so.
- **A path carries evidence per hop.** Every hop has its file, line and tier, so
  a route is as trustworthy as its weakest hop and the reader can see which
  that is.

### Shipped after the first pass

| Item | Result |
|---|---|
| `analyze_change`: the blast radius of a commit range as a tool | Agents, the REST endpoint and the portal's Change page can review a branch. Was `repocortex impact` only |
| `find_owners`: who has recent context on the code a change reaches | Was `repocortex owners` only. Names, never email addresses |
| `repocortex query`: guarded read-only SQL over the index | The escape hatch other tools offer as a graph query language. One SELECT, read-only connection, capped rows, a timeout |
| The command, config and environment are all named `repocortex` | Was `cortex`, a name shared with several other tools. `.repocortex/`, `.repocortex.yml`, `REPOCORTEX_*` |
| Agent benchmark, round 1 | Run and published. It shows no advantage on direct-caller lookups of uniquely named functions; see [benchmarking](BENCHMARKING.md#5-agent-benchmark) |
| History cached by HEAD | `repocortex hotspots` 7.4 s to 0.1 s on this repository |
| Bounded test probes in `list_blindspots` and `plan_change` | A two-hop, 400-node probe in place of a four-hop, 5,000-node walk per candidate; an inconclusive probe is never reported as "untested" |
| One compact output schema per tool | `tools/list` 84 KB to 31 KB |
| Industry verb vocabulary and no aliases | `get`, `list`, `find`, `search`; five tools renamed to `list_`; every alias removed |

### Protocols

| Surface | What it is | Verified by |
|---|---|---|
| MCP over stdio and Streamable HTTP | Existing | Existing tests |
| REST with JSON at `/api/v1` | Every tool as `POST /api/v1/tools/{name}`, behind the MCP token | `internal/restapi`, `TestRESTAndMCPReturnTheSameAnswer` |
| OpenAPI 3.1 at `/api/v1/openapi.json` | Generated from the registered tools on each request | `TestOpenAPIListsExactlyTheServedTools` |
| Shell: `repocortex tool list`, `describe`, `call` | Every tool by its MCP name and arguments | `TestToolCallPrintsWhatAnMCPClientReceives` |
| Liveness at `/healthz` | Unauthenticated, reveals nothing | `TestHealthzIsOpenAndSaysNothing` |

The REST endpoint and `repocortex tool` hold no handlers. Both open an in-process
MCP client against the server, so validation, output bounds, untrusted-content
framing, retired-name aliases and the audit record apply to all three by
construction. See the [protocols guide](guides/protocols.md).

### Naming

| Change | Detail |
|---|---|
| Tools follow `<verb>_<noun>` with a closed list of eight verbs, the ones other MCP servers and language servers already use | `get`, `list`, `find`, `search`, `trace`, `analyze`, `plan`, `minimize`. Enforced by a test |
| Noun-phrase tool names removed | `impact_analysis` is `analyze_impact`; `resolution_coverage` is `get_resolution_coverage` |
| Collections use `list_` | `list_api_endpoints`, `list_db_tables`, `list_hotspots`, `list_dead_code`, `list_blindspots` |
| Commands renamed to match their tools | `blindspots` (was `missed`), `minimize-diff` (was `diff-minimize`), `outline` (was `prune`), `owners` (was `who`) |
| No aliases anywhere | Nothing is released, so no old name is kept. A test fails if a command declares one |
| `repocortex --help` is grouped by task | Set up, ask, change, check, serve |

Rules and rationale: [naming conventions](reference/naming-conventions.md).

### Scale

A new suite, `tests/scale`, runs in the default test run at 5,000 symbols and
takes the real measurement on request. Results at 250,000 symbols and 1.26
million relationships are in [scale testing](operations/scale-testing.md). It
found three defects while phase 0 was being built; see
[findings](#findings-from-building-phase-0).

### Portal

A new portal was designed from the reader's tasks up, and its first cut is
built: `repocortex ui --next`. Five sections replace twenty-three screens, and a
symbol has one page with its callers, what it calls, what a change can reach,
its tests and its type hierarchy as tabs. It has no data routes of its own:
every figure comes from the same tool endpoint agents use, so a page and an
agent cannot disagree. The current portal is unchanged and remains the default.
Design, audit and remaining work: [portal UX plan](design/PORTAL_UX_PLAN.md).

## Phase 1: distribution and proof

The gaps that keep a capable tool from being chosen. None needs new analysis.

| Item | Answers | Done when |
|---|---|---|
| First tagged release with binaries | Every competitor | `install.sh` and the Homebrew formula resolve a real version |
| Signed releases, SBOM, provenance | codebase-memory-mcp (SLSA 3, cosign) | A user can verify a download without trusting the release page |
| Container image, winget and Scoop, MCPB bundle, MCP registry entry | codebase-memory-mcp (8 channels), CodeScene (`.mcpb`) | One install command per platform |
| Reproducible agent benchmark | codegraph, codebase-memory-mcp, jCodeMunch, Graphify, Repowise | A script runs fixed tasks on public repositories with and without the server and prints tool calls, tokens and correctness; results and raw transcripts published |
| Published impact-analysis accuracy | code-review-graph (the only one) | Precision and recall of `analyze_impact` against the files real commits changed, on public repositories |
| Linux-kernel index figure | narsil, codebase-memory-mcp, codegraph | Time and peak memory recorded in [performance](performance.md) |

The benchmark is first. Six competitors publish one, and a claim without one is
the weakest part of this project's case.

## Phase 2: depth

Better answers from the graph that already exists.

| Item | Answers | Approach | Constraint to watch |
|---|---|---|---|
| Community detection as a tool | Graphify, GitNexus, code-review-graph, axon | Expose `internal/analyze/cluster` (already used by the portal) as `find_communities` | Deterministic labels; cluster ids stable across runs |
| API contract checks across repositories | GitNexus `shape_check`, Repowise, jCodeMunch | Join a client call in one index to a route in another in `repocortex system`; report a call with no matching route | Never claim a match across repositories above `high` |
| Knowledge distribution | CodeScene, Repowise | Bus factor per directory from the history `repocortex owners` already reads | Report people only as the history spells them |
| Unused code, deeper | Repowise, jCodeMunch | "Only reached from tests"; framework entry points from route and event edges. Not built: it needs a scan of every referenced symbol's callers, and wants its own budget and scale test | A new reason, never a promotion to `likely` |
| The rest of the new portal | CodeScene, Repowise, GitNexus all ship a dashboard | Pages for a file, a route and a table; a change page that takes a branch; recent agent calls. Then make it the default | Served from the binary: no build step, no network, system fonts |

## Phase 3: breadth

Larger pieces, each a project of its own.

| Item | Answers | Notes |
|---|---|---|
| Deeper resolution for more languages | codebase-memory-mcp (11 languages with built-in type resolution), Serena | Raise C#, Kotlin, Rust and Python to the typed tier; measure each against a compiler index before claiming it |
| Read-only query surface | Cypher in four competitors, Joern, CodeQL | A small, documented, read-only query over symbols and edges. A graph query language is a large commitment; start with saved queries |
| Dependency reachability | Endor Labs, Semgrep, Repowise | Join the call graph to lockfiles and a local advisory database; no network at query time |
| More edits applied by the tool | Serena, GitNexus | `repocortex rename --apply` exists; add move and extract under the same dry-run, verify and rollback protocol |
| Editor extension | claude-context, Repowise, jCodeMunch, CodeScene | Callers and blast radius inline, from the same local index |
| Team mode | Sourcegraph, Cursor, GitLab Orbit | A shared read-only index served over the existing HTTP transport, with per-token scopes. Breaks "single user"; does not break "no model" |

## Declined

| Capability | Who has it | Why not |
|---|---|---|
| Embedding or semantic search | Ten open-source tools, every paid one | Graph connectivity is the admission gate here; see [why not vector search](WHY_NOT_VECTOR_RAG.md). Lexical search with identifier splitting covers the cases a name can answer |
| Model-written wiki, AI review comments, learned rules | DeepWiki, CodeRabbit, Greptile, Bugbot | Needs a model. `repocortex docs` generates deterministic documentation from the index |
| Cross-file taint analysis | Semgrep, Snyk, SonarQube, CodeQL | Needs a per-language intermediate representation. SAST tools do this well; RepoCortex emits SARIF beside them |
| Hosted index, SSO, SCIM, SOC 2 | Sourcegraph, Augment, Cursor | These attach to a hosted service. The local answer is that nothing leaves the machine, and now that every agent call can be audited |
| Tickets, chat and documents as context | Unblocked, Bito, Glean | A different product |

## Findings from building phase 0

Things the work itself turned up, recorded because each changed the result.

| Finding | How it was found | What was done |
|---|---|---|
| The planner read every edge once per file in the hotspot query | `repocortex hotspots` did not return in two minutes on this repository's 19,000-edge index | Join order forced; plan pinned by `TestFileStructureForUsesTheDestinationIndex` |
| The same class of plan in the unused-code query, only without statistics | Scale suite: 8.5 s at 5,000 symbols | Every probe names its index; 36 ms. Pinned by `TestUnreferencedSymbolsUsesItsIndexes` |
| Unused-code overclaimed for functions passed as values | Every MCP handler in this repository listed as `likely` | Text check against the indexed source; comment lines and a definition's own body excluded |
| `list_blindspots` ran one query per exported function | Scale suite: 2.4 s at 5,000 symbols | One aggregate query (`HighFanInSymbols`). Its remaining cost is test discovery; see phase 2 |
| Each tool's wire definition is about three quarters output schema | `repocortex tool list` printed 21,500 tokens for 23 tools before the count was narrowed to what a model sees | Reported both figures; reduction planned in phase 2 |
| Generated bundles outranked all source in hotspots | Top hotspot on this repository was a 550 KB asset | Files with no definitions are left out unless `--all-files` |
| The local linter binary is older than the Go toolchain | `golangci-lint` refused to run | Not fixed here; `go vet` is clean and CI runs the linter |
