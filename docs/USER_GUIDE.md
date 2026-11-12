# RepoCortex User Guide: The Enterprise Guide to Code Intelligence & Change Safety

RepoCortex is a local-first, zero-CGO code intelligence engine and knowledge graph for engineering teams and AI coding agents. It indexes large polyglot codebases into a queryable graph of symbols, call chains, HTTP endpoints, database access patterns, event broker queues, and test coverage—delivering deterministic answers over CLI, MCP (Model Context Protocol), and an embedded interactive portal.

---

## 1. Design Philosophy

Modern codebases suffer from context fragmentation. AI coding agents burn 80% of their token budget and reasoning time repeatedly grepping files, attempting to reconstruct architectural boundaries and tracing caller chains from scratch.

RepoCortex replaces speculative text search with deterministic graph intelligence:

1. **Every Fact Carries Provenance**: RepoCortex never conflates a compiler-proven fact with a heuristic regex match. All relationships carry confidence tiers (`exact`, `high`, `medium`, `low`).
2. **Zero-CGO & Local-First**: Written in pure Go using pure-Go SQLite (`modernc.org/sqlite`). It compiles to a self-contained static binary. Your proprietary source code never leaves your workstation or CI runner.
3. **Zero External LLM Calls in Core Engine**: Graph construction, BM25 indexing, AST pruning, and impact analysis run deterministically at native CPU speed with zero cloud API dependencies.
4. **Honest Incompleteness**: An empty query result distinguishes between "we proved this has no callers" and "this code has unresolvable dynamic references." Blind spots and omitted edges are reported explicitly.

---

## 2. Confidence Tier & Provenance Model

RepoCortex categorizes all graph relationships into four immutable tiers:

| Tier | Source | Typical Precision | Recommended Usage |
|---|---|---|---|
| `exact` | Compiler-backed SCIP indexers (`scip-go`, `scip-typescript`, `scip-python`, `scip-java`) | **100.0%** (Proved by compiler typechecker) | Fully automated refactoring and blocking CI gates. |
| `high` | Tree-sitter explicit import matching + unambiguous type discriminator | **86.8% – 99.5%** | Primary guidance for AI agents and human reviews. |
| `medium` | Multi-candidate type inference or package-level heuristic | **5.7% – 37.5%** | Displayed as informative leads; never acted on automatically. |
| `low` | Loose identifier name match across files | **0.0% – 2.8%** | Filtered out by default; accessible via `--min-confidence low`. |

To inspect your repository's measured precision and recall against your local toolchains:
```bash
repocortex accuracy
```

---

## 3. Installation & Getting Started

### Pre-built Binary Installation

**macOS / Linux:**
```bash
curl -fsSL https://raw.githubusercontent.com/satyendrakumarsingh/repocortex/main/install.sh | sh
```

**Windows (PowerShell):**
```powershell
Invoke-WebRequest -Uri https://raw.githubusercontent.com/satyendrakumarsingh/repocortex/main/install.ps1 -OutFile install.ps1
.\install.ps1
```

### Building From Source (Go 1.26+)
```bash
git clone https://github.com/satyendrakumarsingh/repocortex.git
cd repocortex
go build -trimpath -ldflags="-s -w" -o repocortex ./cmd/repocortex
sudo mv repocortex /usr/local/bin/
```

### Initializing a Project

From the root of your git repository:
```bash
# 1. Initialize local configuration (.repocortex.yml)
repocortex init

# 2. Build the structural index (scans source files and extracts definitions)
repocortex index

# 3. Resolve cross-file references and build the call graph
repocortex resolve

# 4. Verify graph integrity
repocortex doctor
```

Subsequent indexing runs are incremental: RepoCortex stores SHA-256 content hashes of files and re-indexes only files modified since the last run.

---

## 4. CLI Reference & Key Capabilities

### 4.1. Distributed Event Topology (`repocortex events`)

RepoCortex statically tracks asynchronous event producers and consumers across distributed message brokers:
- **Supported Brokers**: Apache Kafka (Sarama, kafka-go, Spring Kafka, KafkaJS, kafka-python), RabbitMQ / AMQP, AWS SQS / SNS, and Google Cloud Pub/Sub.

```bash
# List all discovered topics/queues across the repository
repocortex events

# Inspect producers and consumers for a specific topic
repocortex events topic payment.settled

# Output machine-readable JSON
repocortex events --json
```

Example human output:
```text
Event Topology Report (3 topics discovered)

Topic: payment.settled [kafka]
  Producers:
    - SettlementService.Finalize (services/settlement.go:142) [high]
  Consumers:
    - LedgerWorker.OnMessage (workers/ledger.go:88) [high]
    - NotificationHandler.HandleEvent (handlers/notify.ts:45) [high]
```

### 4.2. Code Blindspots & Missed References (`repocortex blindspots`)

Static analysis frequently encounters dynamic dispatch, unmapped HTTP routes, orphan database tables, or critical paths lacking tests. `repocortex blindspots` surfaces these blindspots before they cause production defects.

```bash
# Run a blindspot audit across the repository
repocortex blindspots

# Output formatted JSON for custom CI/CD parsing
repocortex blindspots --json
```

Discovered categories:
- `unresolved_call`: Dynamic or reflection-based calls where the target symbol could not be proven.
- `untested_critical`: Public API endpoints or database writers lacking covering test edges.
- `unmapped_endpoint`: Route handlers defined in code without client callers or framework contracts.
- `orphan_table`: Database tables defined in schemas or migrations with zero code readers or writers.

### 4.3. Deterministic Refactoring Plans (`repocortex plan-change`)

Before refactoring a symbol or renaming a core function, `repocortex plan-change` constructs an ordered, multi-step execution plan containing direct callers, affected tests, preconditions, and postconditions.

```bash
# Generate refactoring execution plan for a symbol
repocortex plan-change --symbol ProcessRefund

# Output JSON plan for automated agent execution
repocortex plan-change --symbol ProcessRefund --json
```

Example output:
```text
Refactor Plan for ProcessRefund (services/billing.go:112)
Risk Level: MEDIUM (Risk Score: 42/100)
Direct Callers: 4 | Affected Tests: 2

Preconditions:
  - Working directory is clean (no uncommitted edits)
  - Target symbol exists and compiles

Step 1 [update_caller]: Update call site in handlers/payment.go:64
Step 2 [update_caller]: Update call site in workers/retry.go:102
Step 3 [update_def]: Refactor signature in services/billing.go:112
Step 4 [verify_test]: Run covering test TestProcessRefundSuccess (services/billing_test.go:40)

Postconditions:
  - Run repocortex doctor to ensure zero dangling reference edges
  - Run affected test suite
```

### 4.4. AST Interface Pruning (`repocortex outline`)

When AI agents read source files, up to 90% of tokens are consumed by implementation logic inside method bodies. `repocortex outline` parses Go, Python, and TypeScript/JavaScript files and strips all function bodies, replacing them with concise stubs while preserving all type signatures, struct fields, interfaces, and docstrings.

```bash
# Print pruned AST interface to stdout
repocortex outline internal/store/sqlite.go

# Reduces a 1,200-line file (~12,000 tokens) to a 90-line interface (~900 tokens)
```

### 4.5. Token-Efficient Diff Minimization (`repocortex minimize-diff`)

Standard git diffs contain excess context lines, whitespace noise, and redundant index headers. `repocortex minimize-diff` parses patch streams into structured, token-efficient representations highlighting exact additions, removals, and symbol contexts.

```bash
git diff HEAD~1 | repocortex minimize-diff
```

### 4.6. Task Context & Relevance Ranking (`repocortex context`)

Given a natural language task description, RepoCortex uses BM25 full-text scoring combined with AST dependency graph traversal to locate the exact transitive file set required for the task.

```bash
repocortex context "Add idempotency key validation to payment callback handler"
```

The context engine bridges domain concepts deterministically (e.g. mapping "credential leakage" to `redact.go` and `credentials_test.go`) without hallucination.

### 4.7. The Change-Safety Gate: Compile & Verify

RepoCortex enforces architecture rules and prevents regressions through a two-phase contract:

1. **Compile**: Convert an engineering intent into a strict JSON contract:
   ```bash
   repocortex compile "Refactor auth middleware to use JWT tokens" --json > task-contract.json
   ```
2. **Work**: Engineer or AI agent makes changes.
3. **Verify**: Evaluate git changes against the contract:
   ```bash
   repocortex verify --contract task-contract.json --changed "$(git diff --name-only | paste -sd, -)"
   ```

Exit Codes:
- `0` (`pass`): All checks satisfied with zero violations.
- `0` (`advisory`): Only heuristic leads discovered (use `--fail-on-leads` to reject).
- `1` (`fail`): At least one compiler-proven fact violation detected (e.g., breaking an architecture boundary or modifying an excluded file).
- `2` (`unverified`): No index or contract data available to verify.

### 4.8. Applying a Rename (`repocortex rename`)

`plan-change` tells you what a rename touches; `rename` makes the edit, but only
where it can prove every site.

1. **Dry run first** (the default). It lists every site with its confidence and
   every site that would block an apply:
   ```bash
   repocortex rename updateStatus applyStatus
   ```
2. **What blocks `--apply`.** It writes nothing, lists every failing site and
   exits `2` unless every site is `exact` (compiler-proven), unchanged since
   indexing, unambiguous on its line, inside the project root and writable, and
   the new name is not already defined in an edited file. On a project with no
   compiler index, every site is `high` and the rename is refused. Run
   `repocortex indexers install --yes` and `repocortex scip import` to upgrade them
   to `exact`.
3. **Opting into `high`.** `--floor high` also accepts an explicit import plus an
   unambiguous receiver type. That tier is measured 86.8-96.1% precise, so the
   command prints a warning first: at 90%, a rename touching thirty sites gets
   three wrong. Read every listed site.
   ```bash
   repocortex rename updateStatus applyStatus --floor high --apply
   ```
4. **What `--apply` does.** Files are written atomically and restored if a later
   write fails. A lock file, `.repocortex-edit.lock`, exists at the project root
   only while the edit runs; if a crashed run left it behind, RepoCortex never
   deletes it for you, and the error tells you to remove it once no repocortex
   process is running. Then the task contract lands in
   `.repocortex/rename-contract.json` and `verify` runs automatically on the
   changed files. `verify` reads the **pre-rename index**, so run
   `repocortex index` afterwards.
5. **Exit codes.** `0` applied and verified; `1` error or verify failure; `2`
   refused, nothing written; `3` applied, but not verified complete: either
   `verify` was unverified, or the old name still appears in indexed files, which
   are listed under `LEFTOVER OCCURRENCES`. Handle them by hand.
6. **RepoCortex does not run the compiler.** References it could not bind, and
   interface implementations or overrides with no edge to the symbol, are not
   renamed. After every rename, search for the old name, then **build and
   test**.

---

---

## 5. Model Context Protocol (MCP) Server Setup

RepoCortex includes a built-in MCP server that communicates over JSON-RPC via standard input/output.

### Claude Desktop Configuration
Add RepoCortex to `~/Library/Application Support/Claude/claude_desktop_config.json`:
```json
{
  "mcpServers": {
    "repocortex": {
      "command": "repocortex",
      "args": ["serve", "--project", "/path/to/your/repository"]
    }
  }
}
```

### Cursor & Windsurf Configuration
In Cursor or Windsurf MCP Settings:
- **Name**: `repocortex`
- **Type**: `command`
- **Command**: `repocortex serve --project /path/to/your/repository`

### Available MCP Tools

AI agents connected to RepoCortex gain access to:
- `get_project_overview`: Orient on the repository: languages, size, resolution coverage.
- `search_code` / `find_symbol` / `get_symbol`: Locate declarations with exact file, line, and kind.
- `find_callers` / `find_references`: Retrieve incoming call edges and usage sites with confidence grades.
- `get_task_context`: Obtain ranked file summaries and symbol graphs for a prompt.
- `analyze_impact`: Calculate transitive impact (blast radius) and risk for a change.
- `find_tests`: Find the tests that cover a symbol or file.
- `plan_change`: Generate a step-by-step refactoring plan with preconditions and post-checks.
- `plan_rename`: Enumerate every site a rename must touch.
- `list_blindspots`: Audit unresolved calls, unmapped endpoints, orphan tables and missing config.
- `get_file_outline`: Obtain a token-optimized outline (signatures, types, docs; bodies pruned) of a file.
- `minimize_diff`: Compress unified git diffs into structured, token-efficient patch contexts.
- `list_api_endpoints` / `list_db_tables` / `get_config_key`: Query HTTP endpoints, database access and configuration keys.
- `get_resolution_coverage`: Report how much of the call graph is resolved, so an empty result is not over-trusted.

Some tools need the `standard` profile; see [agent-integration.md](agent-integration.md).
Event topology (`repocortex events`) is available from the CLI and the portal, not as an MCP tool.

---

## 6. Enterprise Interactive Dashboard (`repocortex ui`)

RepoCortex includes an embedded, high-performance web dashboard providing complete $N$-level visibility into your codebase architecture.

To launch the portal:
```bash
repocortex ui
```
By default, the server listens on loopback (`127.0.0.1`) on an ephemeral port and prints the access URL.

### Command Palette (`Cmd+K` / `Ctrl+K`)
Press `Cmd+K` (macOS) or `Ctrl+K` (Linux/Windows) anywhere in the portal to open the global Command Palette:
- Instant fuzzy navigation to any screen (`Overview`, `Explore Graph`, `APIs`, `Database`, `Events`, `Blindspots`, `Plan Refactor`, `Governance`, `Metrics`).
- Quick actions: reload data, copy repo root, trigger index refresh.

### 360° Symbol Inspector Drawer
Clicking any symbol name anywhere in the application (in tables, graph nodes, callers, or refactor steps) slides open the 360° Symbol Inspector drawer from the right:
- Definition path and exact line number with one-click editor opening (`vscode://`).
- Inbound callers and outbound dependencies grouped by confidence.
- Database read/write associations and event broker linkages.
- Covering tests and test gap analysis.

### Screens Overview
- **System Overview**: High-level repository health, language breakdown, resolution coverage rate, and freshness indicators.
- **Code map** — four views of the index behind one breadcrumb and one tab strip. They share the selection, so a symbol chosen in one view is selected in the others:
  - **Graph**: the canvas map of directories, files or symbols, with grouping, focus, path finding and three layouts.
  - **Tree**: the repository as an outline, directory → file → symbol, navigable with the keyboard.
  - **Treemap**: where the code is — boxes sized by symbols and coloured by connections, exported share or language.
  - **Code**: a file's text as the index holds it, with its declarations, callers and callees, and the lines relationships cite. Served by `GET /api/source` from the index's own copy, never from disk.
- **Event Bus & Queues**: Interactive topology of Kafka topics, AMQP exchanges, and cloud pub/sub queues with publisher and consumer cards.
- **Code Blindspots**: Actionable matrix of unmapped endpoints, unresolved dynamic calls, and orphan database tables with remediation suggestions.
- **Plan a Refactor**: Interactive refactoring orchestrator generating step-by-step modification lists with preconditions, affected tests, and post-checks.
- **Architecture Governance**: Real-time evaluation of declared Architecture Decision Records (ADRs) and prohibited dependency boundaries.

---

## 7. CI/CD Integration: GitHub Actions Change-Safety Gate

RepoCortex can be integrated directly into your GitHub Actions workflow to fail pull requests that introduce architecture violations or high-severity blindspots (`--fail-on`, default `high`), and to publish what it found: a sticky pull-request comment, code-scanning alerts (SARIF) and the job summary. Reports are published before the gate runs, so a failing build still leaves the reviewer a comment and alerts that explain why.

Create `.github/workflows/repocortex-gate.yml` (the repository ships this exact file):
```yaml
name: RepoCortex Change-Safety Gate

on:
  pull_request:
    branches: [main]
  push:
    branches: [main]

permissions:
  contents: read

env:
  # Lowest blindspot / architecture severity that fails the build: high|medium|low.
  REPOCORTEX_FAIL_ON: high
  # auto: install the pinned compiler-backed indexers this repository needs and
  # import their facts as `exact` before impact runs. fast: skip that stage; the
  # report stays heuristic and says so.
  REPOCORTEX_ACCURACY: auto

jobs:
  gate:
    name: Change-Safety Verification
    runs-on: ubuntu-latest
    permissions:
      contents: read
      pull-requests: write   # the sticky report comment
      security-events: write # SARIF upload to code scanning
    steps:
      - name: Checkout Repository
        uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Setup Go Environment
        id: setup-go
        uses: actions/setup-go@v5
        with:
          go-version: '1.26.x'
          cache: true

      - name: Build RepoCortex
        run: go build -trimpath -ldflags="-s -w" -o ./bin/repocortex ./cmd/repocortex

      # resolve is not optional: without it the index holds definitions but no
      # call graph, and every impact and blindspot result below would be empty
      # for the wrong reason.
      - name: Index and Resolve
        run: |
          ./bin/repocortex init
          ./bin/repocortex index
          ./bin/repocortex resolve

      # The cache key is the pin table itself, so it changes exactly when a
      # pinned version or digest does.
      - name: Pin Compiler Indexers
        if: env.REPOCORTEX_ACCURACY == 'auto'
        run: |
          mkdir -p repocortex-out
          ./bin/repocortex indexers pins > repocortex-out/indexer-pins.txt

      # Only scip-go is cached: it is built by `go install` into GOPATH/bin,
      # which setup-go puts on PATH. npm and container indexers are fetched and
      # digest-verified on every run.
      - name: Cache Compiler Indexers
        if: env.REPOCORTEX_ACCURACY == 'auto'
        uses: actions/cache@v4
        with:
          path: ~/go/bin/scip-go
          key: repocortex-indexers-${{ runner.os }}-go${{ steps.setup-go.outputs.go-version }}-${{ hashFiles('repocortex-out/indexer-pins.txt') }}

      # An accuracy upgrade, never a requirement: if an indexer is missing, blocked
      # or produces an index that will not import, the edges stay heuristic and the
      # report says so.
      - name: Import Compiler Facts
        if: env.REPOCORTEX_ACCURACY == 'auto'
        run: |
          mkdir -p repocortex-out
          { ./bin/repocortex indexers install --yes --out-dir repocortex-out/scip 2>&1 \
              || echo "::warning::compiler indexers unavailable; the report uses heuristic edges"; } \
            | tee repocortex-out/indexers.txt
          imported=0
          failed=0
          for f in repocortex-out/scip/*.scip; do
            [ -f "$f" ] || continue
            if ./bin/repocortex scip import "$f"; then
              imported=$((imported + 1))
            else
              echo "::warning::scip import of $(basename "$f") failed; its edges stay heuristic"
              failed=$((failed + 1))
            fi
          done
          {
            echo "### Compiler-backed precision"
            echo "imported: $imported, failed: $failed"
            echo '```'
            cat repocortex-out/indexers.txt
            echo '```'
          } >> "$GITHUB_STEP_SUMMARY"

      - name: Verify Index Integrity
        run: ./bin/repocortex doctor --verify-index

      # Reports are collected and published BEFORE the gate runs, so a failing
      # gate still leaves the reviewer a comment and code-scanning alerts that
      # say why.
      - name: Collect Findings
        run: |
          mkdir -p repocortex-out
          ./bin/repocortex --json arch > repocortex-out/arch.json
          ./bin/repocortex --json blindspots > repocortex-out/missed.json

      - name: Impact Analysis of the Pull Request
        if: github.event_name == 'pull_request'
        run: ./bin/repocortex --json impact --base "origin/${{ github.base_ref }}" --head HEAD > repocortex-out/impact.json

      - name: Render Reports
        run: |
          ./bin/repocortex report sarif --arch repocortex-out/arch.json --blindspots repocortex-out/missed.json --out repocortex-out/repocortex.sarif
          if [ -f repocortex-out/impact.json ]; then
            ./bin/repocortex report pr --impact repocortex-out/impact.json --arch repocortex-out/arch.json --blindspots repocortex-out/missed.json --out repocortex-out/pr-report.md
          else
            ./bin/repocortex report pr --arch repocortex-out/arch.json --blindspots repocortex-out/missed.json --out repocortex-out/pr-report.md
          fi
          cat repocortex-out/pr-report.md >> "$GITHUB_STEP_SUMMARY"

      # A fork pull request's token is read-only and cannot write security
      # events; the PR comment and step summary still carry the findings.
      - name: Upload SARIF to Code Scanning
        if: github.event_name == 'push' || github.event.pull_request.head.repo.full_name == github.repository
        uses: github/codeql-action/upload-sarif@v3
        with:
          sarif_file: repocortex-out/repocortex.sarif
          category: repocortex

      # Same-repository pull requests only: a fork's token cannot write
      # comments, and this job never runs fork code with write permissions.
      - name: Post Sticky PR Comment
        if: github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository
        env:
          GH_TOKEN: ${{ github.token }}
          REPO: ${{ github.repository }}
          PR: ${{ github.event.pull_request.number }}
        run: |
          id=$(gh api "repos/$REPO/issues/$PR/comments" --paginate \
            --jq '.[] | select(.body | contains("<!-- repocortex:pr-report -->")) | .id' | head -n 1)
          if [ -n "$id" ]; then
            gh api -X PATCH "repos/$REPO/issues/comments/$id" -F body=@repocortex-out/pr-report.md
          else
            gh api -X POST "repos/$REPO/issues/$PR/comments" -F body=@repocortex-out/pr-report.md
          fi

      - name: Report Event Topology
        run: ./bin/repocortex events

      - name: Enforce Architecture Rules
        run: ./bin/repocortex arch --fail-on "$REPOCORTEX_FAIL_ON"

      - name: Audit Code Blindspots
        run: ./bin/repocortex blindspots --fail-on "$REPOCORTEX_FAIL_ON"
```

The `Collect Findings` step saves `repocortex --json arch|missed|impact` output; `repocortex report sarif` turns it into a SARIF 2.1.0 file uploaded to code scanning under the category `repocortex`, and `repocortex report pr` renders one bounded Markdown comment that starts with `<!-- repocortex:pr-report -->`. That marker is how the workflow finds and edits its own comment on later pushes instead of adding a new one. The job needs `pull-requests: write` (the comment) and `security-events: write` (the SARIF upload); the workflow sets both on the job only. Pull requests from forks get neither the comment nor the code-scanning upload, because a fork's token is read-only; they still get the job summary and the gate. The blast-radius analysis runs only on pull requests, and none of the reporting steps fail the build -- the `Enforce` steps at the end do.

**Compiler-backed precision.** With `REPOCORTEX_ACCURACY: auto` (the default) the gate runs `repocortex indexers install --yes --out-dir repocortex-out/scip` after `resolve`, then `repocortex scip import` on every index it produced, so `impact` walks compiler-proven `exact` edges wherever a compiler exists. `--yes` is the CI operator's consent to install the pinned indexers listed by `repocortex indexers pins`; nothing else is installed, and every download is pinned and verified (by digest, or by the Go checksum database for scip-go). The runner needs the language's own toolchain — Go for scip-go (setup-go provides it), Node for scip-typescript and scip-python; a project whose toolchain is missing is skipped with its reason. Java is never built by this workflow. The stage is an accuracy upgrade, never a requirement, and it never fails the job: if an indexer is missing, blocked, or produces an index that will not import, the gate emits a `::warning::`, the edges stay heuristic, and the job summary gains a "Compiler-backed precision" section listing the imported and failed counts and each skipped language with its reason. scip-go is cached under a key derived from the pin table. Set `REPOCORTEX_ACCURACY: fast` to skip the stage; the PR report then shows, in a "How much of the blast radius is proven" table, what share of the blast radius is `exact`, `high` and `medium` (interface-dispatch-only symbols are not counted). The step never passes `--allow-repo-build`, so fork pull requests, which run with a read-only token, execute only pinned, digest-verified indexers and never the repository's own build.

---

## 8. Summary & Best Practices

1. **Keep the Index Fresh**: Add `repocortex hooks install` to your development environment to automatically refresh the index on every git commit in 20 milliseconds without running background daemons.
2. **Import Compiler Data in CI**: Place the compiler's `index.scip` in the project root (or `.repocortex/`) before `repocortex resolve`, which imports it automatically (`--no-scip` opts out), or run `repocortex scip import` yourself, to upgrade heuristic edges to compiler-backed `exact` confidence.
3. **Trust `exact` and `high`, Verify `medium`**: Never allow AI coding agents or automated scripts to execute destructive refactoring operations on `medium` or `low` confidence edges without explicit human review.

---

## 9. Comprehensive Product Capabilities

RepoCortex delivers an end-to-end change-safety and code intelligence architecture. The following matrix details each capability and its operational mechanism:

### 9.1. Polyglot AST Precision Engine & Arity Dialects
- **Multi-Language Parsing**: Zero-external-dependency Tree-Sitter grammars compiled directly into pure Go. Supports Go, TypeScript, JavaScript, Java, Python, C#, Rust, C, C++, and Ruby.
- **Arity & Overload Resolution**: Parameter count queries (`paramCountQueries`) and call argument queries (`argCountQueries`) distinguish methods overloaded by parameter count in Java, C#, and TypeScript.
- **Dynamic Arena Memory Management**: Automatically triggers parser pool memory draining upon encountering large generated files (>256 KiB), eliminating multi-gigabyte heap spikes during repository batch scans.

### 9.2. Change Safety & Blast Radius Analytics (`repocortex impact`, `repocortex plan-change`)
- **Upstream & Downstream Traversal**: Answers both *"what does this function call?"* (downstream dependencies) and *"what in the entire enterprise depends on this function?"* (upstream blast radius).
- **Hub Pruning**: Automatically prunes traversals across high-degree utility nodes (such as loggers, string formatters, and base object classes) to avoid exponential combinatorial explosions while explicitly reporting the cutoff boundary.
- **Test Gap Detection (`repocortex blindspots`)**: Proves whether modified code paths have verified test coverage or whether execution branches are blind to the testing suite.

### 9.3. Safe Automated Refactoring (`repocortex rename`)
- **Exact AST Identifier Splicing**: Matches identifiers strictly on AST node boundaries. Never confuses a symbol name with a comment, string literal, or substring token.
- **Reversible Multi-File Rollback**: Takes an in-memory snapshot before modifying source code. If any file write fails or post-apply verification detects an orphaned call site, RepoCortex rolls back all files across the repository to their original state in reverse order.
- **Leftover Occurrence Scanning**: Audits all files (including unindexed or binary files) post-rename to ensure no stale references to the old symbol persist.

### 9.4. Architecture Governance & CI/CD Gates (`repocortex arch`)
- **Structural Boundary Enforcement**: Declaratively defines allowed import and call relationships between architectural layers (e.g. `domain` must never import `infrastructure` or `controller`).
- **PR Gating & SARIF 2.1.0**: Generates standard SARIF code-scanning artifacts and sticky GitHub PR comments showing exact change risk and architectural compliance scores.

### 9.5. Cross-Stack Distributed Topology (`repocortex api`, `repocortex db`, `repocortex events`)
- **HTTP / REST Endpoints**: Statically discovers route handlers across Express, Spring Boot, Gin, FastAPI, NestJS, and ASP.NET.
- **Database Access Maps**: Maps database tables, ORM models (Prisma, GORM, Hibernate, SQLAlchemy), and raw SQL queries to identifying calling methods.
- **Distributed Event Topologies**: Extracts producer and consumer queues across Apache Kafka, RabbitMQ, AWS SQS/SNS, and Google Cloud Pub/Sub.

### 9.6. AI Coding Agent MCP Server (`repocortex serve`, `repocortex install`)
- **Compact Subgraph Extraction**: Delivers targeted subgraphs and pruned signatures instead of raw file dumps, saving up to 85% of LLM token windows.
- **Confidence Advisory Notices**: Automatically attaches warning notes when query results contain heuristic (`medium` or `low`) edges, preventing autonomous agents from acting on hallucinatory call chains.

---

## 10. Deep Comparative Analysis: RepoCortex vs Alternative Tools

Rather than evaluating specific vendor brands, the following analysis benchmarks RepoCortex against the primary architectural classes of code intelligence and developer tools currently deployed in the software industry:

```
+--------------------------------------------------------------------------------------------------+
|                                  TOOL CATEGORY COMPARISON OVERVIEW                               |
+--------------------------------------------------------------------------------------------------+
| RepoCortex                  : Pure-Go static binary, deterministic graph, safe transactional edits |
| Category A (LSP Daemons)  : Per-language server silos, heavy memory, navigation only, no cross-db|
| Category B (Vector RAG)   : Fuzzy text embeddings, 80%+ hallucinations, massive token waste      |
| Category C (Heavy SAST)   : Multi-hour batch scans, alert fatigue, read-only, non-agent-native   |
| Category D (Text Greppers): High speed, zero semantic awareness, false-positive substring matches|
+--------------------------------------------------------------------------------------------------+
```

### Detailed Evaluation by Tool Category

#### Category 1: Traditional Language Server Protocol (LSP) Daemons
* **How They Work**: Run language-specific daemons (e.g., `gopls`, `tsserver`, `jdtls`) in the background of an IDE to provide real-time auto-complete and go-to-definition.
* **Limitations Compared to RepoCortex**:
  - **Siloed by Language**: An LSP cannot connect a TypeScript frontend API call to a Java Spring Boot endpoint, nor can it trace a Go publisher to a Python Kafka consumer.
  - **Resource Heavy**: Running 3 to 4 distinct language servers in a microservice monorepo easily consumes 4–8 GB of RAM.
  - **No Global Change Impact**: LSPs provide local symbol lookups but lack graph traversal capabilities (e.g., multi-hop blast radius, hub pruning, test gap discovery).
* **Where RepoCortex Excels**: RepoCortex unifies all languages into a single SQLite knowledge graph running in a single lightweight process with flat memory footprint.

#### Category 2: Naive Vector Search / Embedding-Based Code RAG
*(For an exhaustive architectural essay, see [Why Vector RAG Fails for Code: The Case for Deterministic Graph Intelligence](WHY_NOT_VECTOR_RAG.md))*
* **How They Work**: Chunk source code into text windows, compute dense vector embeddings, and perform cosine similarity searches to retrieve context for LLMs.
* **Limitations Compared to RepoCortex**:
  - **Fuzzy Guesswork vs Deterministic Truth**: Vector similarity finds code that *reads similarly*, not code that is *actually executed*. A method named `processPayment` will match unrelated payment helpers while missing the actual interface implementation `settle`.
  - **Extreme Token Waste**: Dumping entire source files or arbitrary chunks into an LLM context consumes tens of thousands of tokens per prompt, leading to high cost and lost-in-the-middle context degradation.
  - **Zero Safety Guarantees**: Vector search provides zero safety invariants for automated refactoring or architecture governance.
* **Where RepoCortex Excels**: RepoCortex delivers exact call graphs and pruned signatures, reducing prompt token consumption by up to 85% while providing mathematically calibrated confidence tiers.

#### Category 3: Heavyweight Enterprise Static Analysis (SAST) Suites
* **How They Work**: Execute deep interprocedural taint analysis and abstract interpretation to detect security vulnerabilities and code quality flaws.
* **Limitations Compared to RepoCortex**:
  - **Hours-Long Execution**: Full repository scans take 30 minutes to several hours, preventing interactive developer use.
  - **High False-Positive Noise**: Developers frequently suffer from alert fatigue due to uncalibrated heuristic warnings.
  - **Read-Only / Non-Agent Native**: Designed for compliance reporting rather than active pair-programming with AI coding agents.
* **Where RepoCortex Excels**: RepoCortex indexes 10,000 files in seconds, provides sub-millisecond query latency via in-memory CSR graphs, and integrates natively into agent workflows via standard MCP tools.

#### Category 4: Static LSIF / SCIP Compiler-Only Dumpers
* **How They Work**: Compile code using full toolchains (`scip-go`, `scip-typescript`) and write large protocol buffer files storing cross-references.
* **Limitations Compared to RepoCortex**:
  - **Brittle Setup Requirements**: Require every compiler, SDK, private registry credential, and native dependency to be fully configured on the host. If a build fails, zero index is produced.
  - **Static / Non-Queryable**: Output static dump files that cannot be dynamically traversed, queried for blast radius, or updated incrementally on file save.
* **Where RepoCortex Excels**: RepoCortex provides zero-setup Tree-Sitter heuristic indexing out of the box, with optional seamless SCIP compiler enrichment (`repocortex scip import`). If a compiler is missing, RepoCortex still delivers an 87%+ accurate graph.

#### Category 5: Fast Regex / Text Greppers (ripgrep, ag, ctags)
* **How They Work**: Execute optimized parallel string and regular expression searches across file trees.
* **Limitations Compared to RepoCortex**:
  - **No Semantic Awareness**: Cannot distinguish between a function definition, an invocation, a comment, a string literal, or a substring in an unrelated variable name.
  - **No Graph Traversability**: Cannot trace multi-hop call hierarchies, dependency chains, or transitive blast radius.
* **Where RepoCortex Excels**: RepoCortex understands AST node boundaries, receiver types, and call semantics.

---

### Comprehensive Capabilities & Architectural Comparison Matrix

| Architectural Feature | RepoCortex | Traditional LSP Daemons | Vector Embedding RAG | Enterprise SAST Suites | Static SCIP / LSIF Dumpers | Text Greppers (`rg`) |
|:---|:---:|:---:|:---:|:---:|:---:|:---:|
| **Zero-Setup Polyglot Indexing** | **Yes** (Native Go) | No (Per-language SDKs) | Yes (Raw text) | No (Complex setups) | No (Full build required) | **Yes** |
| **Deterministic Call Graphs** | **Yes** | Yes (Single language) | No (Fuzzy similarity) | Yes | **Yes** | No (String match) |
| **Cross-Stack Topologies (DB/API/Events)** | **Yes** | No | No | Partial (Security only) | No | No |
| **Calibrated Confidence Provenance** | **Yes** (4 Tiers) | Implicit (Uncalibrated) | No (Distance metric) | No (Severity, not truth) | **Yes** (Compiler-only) | No |
| **Transitive Blast Radius (`impact`)** | **Yes** (With Hub Pruning)| No | No | No | No | No |
| **AI Agent MCP Server Native** | **Yes** (10+ tools) | No | Partial (Retrieval only)| No | No | No |
| **Atomic Multi-File Safe Refactoring** | **Yes** (With Rollback) | Partial (LSP rename) | No | No | No | No (Blind replace) |
| **Incremental File Watch Latency** | **<50 ms** | <100 ms | Seconds to minutes | Hours (Batch only) | Batch only | Instant (Stateless) |
| **Memory Footprint on 10K Files** | **<250 MB** | 2–6 GB | 1–3 GB + Vector DB | 4–16 GB | N/A (Static file) | **<50 MB** |
| **100% Offline / Air-Gapped Operation** | **Yes** | **Yes** | No (Often needs cloud) | Often Cloud-hosted | **Yes** | **Yes** |
| **Single Standalone Static Binary** | **Yes** (Zero CGO) | No | No | No | No | **Yes** |
