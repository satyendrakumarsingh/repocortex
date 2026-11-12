# Competitive analysis

Where RepoCortex stands against the open-source and paid tools that give AI
coding agents an understanding of a codebase. Checked on 2026-11-12.

**How to read this.** Star counts are from GitHub on that date. Every benchmark
figure is the vendor's own and has not been reproduced here. "Unverified" marks
anything not confirmed on the vendor's own site, docs or README. Prices and
tool lists in this market change monthly; re-check a figure before quoting it.

The gaps this analysis finds, and what is being done about each, are in the
[implementation plan](IMPLEMENTATION_PLAN.md).

## Contents

- [Summary](#summary)
- [Open-source competitors](#open-source-competitors)
- [Paid and enterprise tools](#paid-and-enterprise-tools)
- [Five products checked in detail](#five-products-checked-in-detail)
- [What competitors have that RepoCortex lacked](#what-competitors-have-that-repocortex-lacked)
- [Where RepoCortex already leads](#where-repocortex-already-leads)
- [Numbers to answer](#numbers-to-answer)
- [Naming conventions across the field](#naming-conventions-across-the-field)

## Summary

RepoCortex's position is unusual and defensible: it is the only tool surveyed
that combines compiler-proven edges (SCIP), a measured confidence tier on every
edge, a CI gate with SARIF, architecture rules, ownership and trace import in
one permissively licensed binary with no model and no network calls. Several of
the best-known competitors cannot be used at work without paying (GitNexus,
jCodeMunch, CKB) or carry copyleft licences (Serena, Repowise, SocratiCode).

What it lacked falls into four groups:

1. **Whole-repository findings.** Unused code, change hotspots and files that
   change together. Nine open-source tools report dead code; CodeScene built a
   company on hotspots.
2. **Graph questions it could already answer but did not expose.** Callees,
   implementations and "how does A reach B" existed in the graph layer or the
   CLI and had no agent tool.
3. **Reach.** Every tool was MCP-only or CLI-only. Agents that speak neither
   (a plain HTTP function-calling client, a shell-only agent) had partial
   access. One competitor (Repowise) ships REST with OpenAPI.
4. **Proof and distribution.** Six competitors publish reproducible agent
   benchmarks and tool-schema costs; the leaders ship through five to eight
   package channels with signed releases. RepoCortex has no tagged release.

Some gaps are closed by decision, not by oversight: embeddings, model-written
wikis and AI review comments are [non-goals](NON-GOALS.md), and a hosted,
permission-synced index is a different product.

## Open-source competitors

### Local code graph with MCP (direct competitors)

| Tool | Stars | How it works | Languages | MCP tools | Licence |
|---|---|---|---|---|---|
| [Graphify](https://github.com/Graphify-Labs/graphify) | 146k (looks high; unverified) | tree-sitter, no embeddings; a model reads docs, PDFs and images only; Leiden communities | about 40 | 10 local, 10 hosted | Apache-2.0, paid cloud |
| [codegraph](https://github.com/colbymchenry/codegraph) | 77k | tree-sitter in a Rust kernel, SQLite and FTS5, no embeddings or model | 34 | 1 by default, 7 hidden | MIT |
| [codebase-memory-mcp](https://github.com/DeusData/codebase-memory-mcp) | 48.5k | one C binary, 163 grammars, built-in type resolution for 11 languages, bundled embedding model | 163 | 17, including a Cypher subset | MIT |
| [GitNexus](https://github.com/abhigyanpatwari/GitNexus) | 48.4k | tree-sitter, LadybugDB graph, optional embeddings, Leiden, optional taint | 15–18 | 19, plus 10 resources and 2 prompts | PolyForm Noncommercial |
| [code-review-graph](https://github.com/tirth8205/code-review-graph) | 32.7k | tree-sitter, SQLite and FTS5, optional embeddings, four confidence labels | about 40 | 30, plus 5 prompts | MIT |
| [Serena](https://github.com/oraios/serena) | 31.3k | language servers, no index | 40+ | about 35, including symbol-level edits | GPL-3.0 after v1.7.0 |
| [Repowise](https://github.com/repowise-dev/repowise) | 8.1k | tree-sitter, git mining, embeddings, optional model prose | 28 parsed, 42 total | 10 default, 7 opt-in | AGPL-3.0, paid cloud |
| [code-graph-rag](https://github.com/vitali87/code-graph-rag) | 5.3k | tree-sitter plus compiler front ends, model turns questions into Cypher | 14 full, 8 structural | 15 | MIT |
| [CodeGraphContext](https://github.com/CodeGraphContext/CodeGraphContext) | 4.4k | tree-sitter, optional SCIP, graph database | 23–25 | not listed | MIT |
| [jCodeMunch](https://github.com/jgravelle/jcodemunch-mcp) | 2.8k | tree-sitter symbol index, optional SCIP import | 79+ | 90+ behind a 6-tool front door | paid for commercial use |
| [CKB](https://github.com/SimplyLiz/ckb) | 112 | Go, SCIP, tree-sitter, language servers, git | 12 | 110 in presets | paid above $25k revenue |
| [narsil-mcp](https://github.com/postrv/narsil-mcp) | 187 | Rust, tree-sitter, in memory | 32 | 90–93, including taint and control flow | MIT / Apache |
| [axon](https://github.com/harshkedia177/axon) | 817 | tree-sitter, KuzuDB, embeddings, Leiden | 3 | 7 | MIT |

CKB is the closest architectural twin (Go, SCIP with tree-sitter, CODEOWNERS,
SARIF) and the least adopted. The most adopted tools are the ones with the
simplest install and a published benchmark, not the deepest analysis.

### Search and context packers

| Tool | What it does |
|---|---|
| [claude-context](https://github.com/zilliztech/claude-context) (12.7k) | BM25 with dense vectors on Milvus; claims about 40% fewer tokens |
| [Aider repo map](https://aider.chat/docs/repomap.html) (50k) | graph-ranked map of the repository in about 1,000 tokens; no MCP |
| [Repomix](https://github.com/yamadashy/repomix) (29k) | packs a repository into one file; `--compress` claims about 70% reduction |
| [Probe](https://github.com/probelabs/probe) (740) | ripgrep with tree-sitter, no index |
| [deepwiki-open](https://github.com/AsyncFuncAI/deepwiki-open) (18.4k) | model-written wiki |

Continue has deprecated its codebase index, Roo Code is archived, and Cline does
not index by design. Client-side embedding indexes are losing ground to
server-side graph tools.

### Engines without an agent interface

SCIP (now at `scip-code`), Zoekt, Glean, Kythe, ast-grep, CodeQL, Semgrep CE and
Joern. GitHub archived stack-graphs in September 2025. The CodeQL CLI licence
bars use on private code without Advanced Security; Semgrep CE limits taint
tracking to a single function.

## Paid and enterprise tools

| Tool | Price and deployment | How it understands code | What it has that RepoCortex does not |
|---|---|---|---|
| Sourcegraph | from $16K a year; single-tenant cloud or self-hosted | trigram search, SCIP navigation, agentic search | search across "100 or 1M+ repositories", cross-repository references, Batch Changes, SAML, SCIM, audit logs |
| Augment Code | $20–100 a month plus usage; SaaS | semantic index, method not disclosed | 400k–500k file repositories, SWE-bench Pro 51.80% (self-reported), SOC 2 II, ISO 42001 |
| Cursor | free to $200; Teams $30+ a user | Merkle tree with embeddings | shared team index, Bugbot with learned rules, analytics, audit logs |
| Cognition (Windsurf, Devin) | about $20 Pro (unverified) | trained retrieval model, model-drawn code maps, DeepWiki | shareable execution-flow maps, generated wiki |
| GitHub Copilot | $10–100 individual | semantic search index | index in up to 60 seconds, Spaces, coding agent, review memory |
| GitLab Orbit | $30 per 1,000 queries | lifecycle graph: code, merge requests, pipelines, ownership | permission-mirrored graph; 1.5M projects indexed in under 2 hours |
| Greptile, CodeRabbit, Qodo, Graphite | $20–72 a user | codebase graph with model review | AI review comments, learned rules, review analytics |
| CodeScene | €18–27 per active author; cloud or on-premises | git-history behaviour analysis | hotspots, knowledge distribution, off-boarding simulator, pull-request gates |
| SonarQube, Semgrep, Snyk Code, CodeQL | per line, per contributor or per committer | rule engines with data flow | cross-file taint, reachability, autofix |
| Endor Labs | quote | reachability-based dependency analysis | function-level reachability of vulnerable dependencies |
| Moderne | "typical" $250K and up | type-attributed Lossless Semantic Tree | thousands of type-aware recipes across thousands of repositories |
| CloudBees Smart Tests, Datadog, Develocity | quote or usage | models trained on build history, or per-test coverage | predictive test selection (35–70% less CI time claimed) |
| Unblocked, Bito, Glean | $29 a user or quote | code with Slack, Jira and documents | tickets, chat and incidents linked to code |
| Tabnine | acquired by Tricentis, July 2026 | vector, graph and agentic retrieval (unverified) | supported air-gapped deployment |

Gone or pivoted: Grit (acquired by Honeycomb, hosted product ended), CodeSee
(folded into GitKraken), Swimm (legacy modernisation), Bloop (COBOL migration).

## Five products checked in detail

A table of five commercial competitors was supplied for checking. Each claim was
verified against the vendor's own pages; the corrections are in the last column.

| Product | Supplied claim | What the vendor's pages say |
|---|---|---|
| **Graphify Cloud** | Per-seat SaaS, 14-day trial; tree-sitter with a model pass for docs and schema | **Partly wrong.** Free tier, a flat $15 Pro and a $29-a-seat Teams tier; two weeks free on paid tiers. Code is tree-sitter only. The model reads documents and media, not SQL: SQL and Terraform are parsed deterministically. |
| **GitNexus Enterprise** | $29 a seat a month, commercial licence; tree-sitter with type resolution on LadybugDB | **Confirmed with a caveat.** $29 a seat is the managed SaaS tier, not a licence price, and the page does not state the period. The open-source version is non-commercial. Approach confirmed. |
| **Repowise** | Hosted Pro and Teams; tree-sitter, typed resolution, git mining | **Mostly confirmed.** Pro $15 a month, Teams $20 a seat. "Typed resolution" is not supported by the docs, which describe 39 confidence-scored edge origins with compilers used only as benchmark oracles. |
| **jCodeMunch** | $79–$1,999 one-time; tree-sitter with SCIP | **Confirmed.** SCIP import is optional, CLI-only, capped at 200,000 rows and single-repository. |
| **CodeScene MCP** | Subscription, 14-day trial; behavioural and structural coupling graph | **Approach wrong for the MCP server.** It exposes per-file Code Health scores and reviews, not a coupling graph; hotspots and ownership come from the paid Core API. The page states both a 14-day and a 30-day trial. |

### What each one exposes

**Graphify** — local tools `query_graph`, `get_node`, `get_neighbors`,
`shortest_path`, `get_community`, `god_nodes`, `graph_stats`, `list_prs`,
`get_pr_impact`, `triage_prs`. Distinctive: communities, "god nodes", rationale
nodes from `# WHY:` comments, skills and hooks for 20+ agents. The cloud adds
re-index on push, pull-request review, cross-repository graphs and test impact.
Claims 71.5× fewer tokens per query on its own 52-file example.

**GitNexus** — `query`, `context`, `impact`, `trace`, `detect_changes`, `check`,
`rename`, `cypher`, `route_map`, `shape_check`, `api_impact`, `pdg_query` and
group tools. Distinctive: execution-flow ("process") detection, API
response-shape checks, taint analysis, token budgets. Claims a 68.4% solve rate
on DeepSWE against 37.0% for a bare agent.

**Repowise** — `get_overview`, `get_answer`, `get_context`, `get_symbol`,
`search_codebase`, `get_risk`, `get_change_risk`, `get_why`, `get_dead_code`,
`get_health`. Distinctive: REST with OpenAPI beside MCP, hotspots, co-change,
bus factor, decision records, a 53-detector health score, token and dollar
savings accounting. Claims call-graph precision of 93.8–99.8%.

**jCodeMunch** — about 90 tools behind a six-tool front door, including
`list_dead_code`, `get_hotspots`, `find_implementations`, `get_call_hierarchy`,
`check_edit_safe`, `get_session_stats`. Claims 96.2% fewer tokens per lookup
than grep-and-read.

**CodeScene MCP** — 27 tools around `code_health_score`, `code_health_review`
and `pre_commit_code_health_safeguard`. Claims up to 45% fewer tokens.

## What competitors have that RepoCortex lacked

Ranked by how many tools have it. The last column is the decision; details are
in the [implementation plan](IMPLEMENTATION_PLAN.md).

| # | Capability | Who has it | Decision |
|---|---|---|---|
| 1 | Semantic or hybrid search with a local embedding model | codebase-memory-mcp, GitNexus, code-review-graph, Repowise, claude-context, six more | **Declined.** [Non-goal](NON-GOALS.md); graph connectivity is the admission gate here |
| 2 | Published, reproducible agent benchmarks | codegraph, codebase-memory-mcp, code-review-graph, jCodeMunch, Graphify, Repowise | **Round 1 published**, showing no advantage on simple lookups; round 2 planned |
| 3 | Dead-code detection | nine open-source tools, Repowise, jCodeMunch | **Shipped**: `list_dead_code` |
| 4a | Blast radius of a commit range as a tool (`detect_changes`) | GitNexus, codebase-memory-mcp, jCodeMunch | **Shipped**: `analyze_change` |
| 4 | Execution-flow or path tracing | GitNexus, Repowise, Graphify, jCodeMunch, codebase-memory-mcp | **Shipped**: `trace_call_path` |
| 5 | Hotspots and change coupling | CodeScene, Repowise, jCodeMunch, axon, CKB | **Shipped**: `list_hotspots` |
| 6 | Implementations and type hierarchy as a tool | Serena, jCodeMunch, codebase-memory-mcp | **Shipped**: `find_implementations` |
| 7 | REST API with OpenAPI beside MCP | Repowise | **Shipped**: `/api/v1`, `openapi.json` |
| 8 | Published tool-schema cost | narsil, CKB | **Shipped**: `repocortex tool list` |
| 9 | Audit log of agent calls | Sourcegraph, Cursor, Graphite, Kodus | **Shipped**: `repocortex serve --audit-log` |
| 10 | Server instructions and prompts | GitNexus, code-review-graph | **Shipped** |
| 11 | Community detection | Graphify, GitNexus, code-review-graph, axon | Partly present (`internal/analyze/cluster`, portal only); tool **planned** |
| 12 | Ad-hoc query language | codebase-memory-mcp, GitNexus, axon, Joern, CodeQL | **Shipped**: `repocortex query`, guarded read-only SQL |
| 13 | Symbol-level edits applied by the tool | Serena, GitNexus, code-review-graph | Partly present (`repocortex rename --apply`); more **planned** |
| 14 | Built-in type resolution with no indexer step | Serena, codebase-memory-mcp, code-graph-rag | **Planned** (phase 3): deeper tiers for more languages |
| 15 | API contract checks across repositories | GitNexus, Repowise, jCodeMunch | **Planned** (phase 2), on `repocortex system` |
| 16 | Dependency reachability and SBOM | Endor Labs, Semgrep, narsil, Repowise | **Planned** (phase 3) |
| 17 | Distribution breadth, signed releases | codebase-memory-mcp (8 channels, SLSA 3) | **Planned** (phase 1): the first tagged release |
| 18 | Editor extension | claude-context, Repowise, jCodeMunch, CodeScene | **Planned** (phase 3) |
| 19 | Cross-file taint and data flow | Semgrep, Snyk, SonarQube, CodeQL, GitNexus | **Declined for now.** Needs a per-language IR; SAST tools do this well |
| 20 | Model-written wiki, AI review comments, learned rules | DeepWiki, CodeRabbit, Greptile, Bugbot | **Declined.** Needs a model; `repocortex docs` generates deterministic documentation |
| 21 | Hosted index, SSO, SCIM, repository-permission sync, SOC 2 | Sourcegraph, Augment, Cursor, GitLab Orbit | **Out of scope.** These attach to a hosted service; the local answer is that nothing leaves the machine |

## Where RepoCortex already leads

| Strength | Field |
|---|---|
| 206 grammars | 163 (codebase-memory-mcp), 79+ (jCodeMunch), about 40 (Graphify), 15–18 (GitNexus) |
| Compiler-proven `exact` edges from SCIP, across repositories | Only jCodeMunch imports SCIP, capped and single-repository |
| A measured precision figure per confidence tier | Others label edges without publishing how often a label is right |
| CI gate with pull-request comment and SARIF, free | Paid in Graphify (`gate`) and GitNexus (review) |
| Architecture rules, CODEOWNERS routing, OpenTelemetry trace import | CKB has the first two; no surveyed tool imports traces |
| One static Go binary, no model, no network | Python (Graphify, Repowise, jCodeMunch), Node (GitNexus) |
| Apache-2.0 | GitNexus, jCodeMunch and CKB restrict commercial use; Serena, Repowise and SocratiCode are copyleft |
| Every answer states what the index could not see | Rare; code-review-graph and Graphify label ambiguity but do not bound answers |

## Numbers to answer

Figures competitors publish that a credible alternative should match or
explain. None is independently reproduced.

| Measure | Published figure | RepoCortex today |
|---|---|---|
| Cold index of the Linux kernel | 45 s (narsil), 3 min (codebase-memory-mcp), under 12 min on 2 cores (codegraph) | Not measured. spring-boot (9,236 files) takes 46 s |
| Graph query latency | under 1 ms traversal (codebase-memory-mcp), 0.3 ms lookup (Codanna) | Sub-millisecond multi-hop BFS on the in-memory graph at 1M edges; no published per-tool figure |
| Incremental update | about 2.5 s for 2 files in 3k (code-review-graph) | Not published |
| Agent efficiency | 88% fewer tool calls, 62% fewer tokens (codegraph); 10× fewer tokens at 83% vs 92% answer quality (codebase-memory-mcp) | One round run: on direct-caller lookups of uniquely named functions, agents with search alone matched it (51 of 51 correct in both arms, tokens within 1%). The harder cases are not measured. See [benchmarking](BENCHMARKING.md#5-agent-benchmark) |
| Impact-analysis accuracy | F1 0.693, precision 0.546 (code-review-graph, the only one) | Not published. An opening: nobody else measures this |
| Call resolution per language | TypeScript 95.8%, Go 96.6% (codegraph) | Precision 94.2–100%, recall 33–66% from the syntax tree; `exact` with SCIP |
| Tool-schema overhead | 90 tools about 12,000 tokens (narsil); 25 tools about 4,000 (CKB) | Now printed by `repocortex tool list`; see [protocols](guides/protocols.md) |
| Repository scale | 400k–500k files (Augment); 500k-file cap (GitLab Orbit) | Synthetic scale suite runs to 1M symbols and 5M edges; see [scale testing](operations/scale-testing.md) |

## Naming conventions across the field

Tool names are part of the interface an agent reads on every request, so the
field's conventions were surveyed before RepoCortex's were tightened.

| Tool | Convention | Example |
|---|---|---|
| jCodeMunch | strict `verb_noun` | `list_dead_code`, `get_hotspots`, `check_rename_safe` |
| Repowise | uniform `get_<noun>` | `get_dead_code`, `get_risk` |
| Serena | `verb_noun` | `find_symbol`, `rename_symbol` |
| GitNexus | bare single words | `impact`, `trace`, `cypher` |
| Graphify | mixed locally, `graphify_<noun>` when hosted | `god_nodes`, `graphify_callers` |
| CodeScene | long descriptive | `pre_commit_code_health_safeguard` |

RepoCortex follows the `verb_noun` form with a closed list of verbs, enforced by
a test. The rules are in [naming conventions](reference/naming-conventions.md).
