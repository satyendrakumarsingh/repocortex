# MCP tools reference

RepoCortex runs as an MCP server over stdio (`cortex --project <repo> serve`). This page lists the tools the server registers, generated from the server's own `tools/list` response.

## Profiles

Every tool's schema is sent to the agent on every request, so the default profile is small. Set the profile in `.cortex.yml`:

```yaml
mcp:
  profile: standard   # minimal (default), standard or full
```

| Profile | Tools |
|---|---|
| `minimal` (default) | 7: `get_task_context`, `get_project_overview`, `search_code`, `find_symbol`, `get_symbol`, `find_references`, `find_callers` |
| `standard` | All 18 |
| `full` | All 18, and any tool added later |

## Tools

| Tool | Profile | Parameters | What it does |
|---|---|---|---|
| `get_task_context` | minimal (default) | `task` (required), `max_files`, `min_confidence`, `hub_degree_limit` | CALL THIS FIRST when you are about to change code, before exploring the repository by hand. |
| `get_project_overview` | minimal (default) | none | Call this FIRST, before any other RepoCortex tool. |
| `search_code` | minimal (default) | `query` (required), `limit` | Full-text search across indexed source. |
| `find_symbol` | minimal (default) | `name` (required), `kind`, `min_confidence` | Locate a symbol's definition by name. |
| `get_symbol` | minimal (default) | `name` (required), `path`, `min_confidence` | Get a symbol's definition with a bounded source excerpt and its immediate relationships. |
| `find_references` | minimal (default) | `name` (required), `path`, `min_confidence` | Find where a symbol is used. |
| `find_callers` | minimal (default) | `name` (required), `path`, `min_confidence` | Find the functions and methods that call a symbol — the callers, not merely the files mentioning it. |
| `find_tests` | standard | `symbol` (required), `min_confidence`, `hub_degree_limit` | Find the tests covering a symbol, and which signal found each one — a proven call edge is far stronger evidence than a matching filename, and the response names which you got. |
| `impact_analysis` | standard | `symbol` (required), `min_confidence`, `hub_degree_limit` | Before changing a symbol, find out what depends on it. |
| `resolution_coverage` | standard | none | Ask this when a find_callers, find_references or impact_analysis result came back shorter than you expected, BEFORE concluding anything from it. |
| `plan_rename` | standard | `name` (required), `new_name` (required), `path`, `min_confidence` | Plan a symbol rename. |
| `find_api_endpoints` | standard | `method` | List the HTTP endpoints this service exposes, with the handler that serves each one and where it is declared. |
| `find_db_access` | standard | `table` | Report which code reads and which code writes each database table, with per-table read/write counts. |
| `get_config_key` | standard | `key` (required) | Look up one configuration key by name: where it is declared and how many places read it, joined into a single mismatch check. |
| `get_file_outline` | standard | `path` (required) | Get a token-efficient structural outline of a source file by pruning implementation bodies while preserving all imports, types, interfaces, structs, function signatures, and docstrings. |
| `minimize_diff` | standard | `diff` (required) | Compress unified git diffs into structured, token-efficient representations. |
| `plan_change` | standard | `symbol` (required), `min_confidence`, `hub_degree_limit` | Generate a deterministic refactoring and change plan for a symbol. |
| `detect_blindspots` | standard | none | Audit the repository for code intelligence blindspots and hidden risks. |

## Parameters

### `get_task_context`

CALL THIS FIRST when you are about to change code, before exploring the repository by hand. Give it the task in plain English and it returns the minimum set of files, symbols, tests and database tables the change actually touches, found by walking the call graph rather than by keyword matching. Every included item states why it is there, and notable near-misses are listed with the reason they were excluded. Using this instead of grepping the repository is the point of this server; prefer it over search_code and find_symbol when you are starting a change rather than looking up one known symbol.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `task` | string | yes | Describe the change you are about to make, in plain English, as you would to a colleague. For example: 'Add idempotency to payment callback processing'. Full sentences work better than keywords — the task is parsed for intent, not matched literally. |
| `max_files` | integer | no | Cap on returned items (default 20). Raise only if the default answer is genuinely too narrow. |
| `min_confidence` | string | no | Lowest confidence to return: exact, high, medium or low. Default low. Recommended: 'high' for safe automated code changes and blast radius analysis to filter out heuristic false positives. |
| `hub_degree_limit` | integer | no | Stop the walk from expanding THROUGH any symbol with more than this many edges. Default 0 (off), which is what every result you have seen so far used. Set it (50 is a reasonable start) when a result is enormous because the walk crossed a logger, a util or a base class; pruned symbols are still listed, only their neighbours are not explored, and the response says how many were cut. |

### `get_project_overview`

Call this FIRST, before any other RepoCortex tool. Reports what the project is, how much of it is indexed, which languages it uses, and how much of the code RepoCortex could resolve. The resolution figure is the important one: it tells you whether relationship answers for this repo are trustworthy or mostly inferred. Do not guess at coverage — this tool states it.

No parameters.

### `search_code`

Full-text search across indexed source. Prefer this over your own grep: it searches the index, returns file paths and matching lines rather than whole files, and costs a fraction of the context. Use it when you know roughly what you are looking for but not where it lives, and use it to confirm any relationship result that came back empty or low-confidence.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `query` | string | yes | Search terms. Matches identifiers and code text; camelCase identifiers are searchable by their parts, so 'callback' finds handleCallback. |
| `limit` | integer | no | Maximum results (default 20). |

### `find_symbol`

Locate a symbol's definition by name. Faster and more precise than searching when you already know the identifier. If several symbols share the name, pass `path` to disambiguate. If this returns nothing, the symbol may still exist under a different name or in an unindexed file — fall back to search_code rather than concluding it is absent.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Symbol name to find, e.g. 'TransactionService' or 'updateStatus'. Exact match on the name, not a substring. |
| `kind` | string | no | Optional filter: class, interface, function, method, field, constant, type_alias. |
| `min_confidence` | string | no | Lowest confidence to return: exact, high, medium or low. Default low. Recommended: 'high' for safe automated code changes and blast radius analysis to filter out heuristic false positives. |

### `get_symbol`

Get a symbol's definition with a bounded source excerpt and its immediate relationships. Call this after find_symbol instead of reading the whole file. The excerpt is bounded and may be truncated; the response says so explicitly when it is, so check that flag rather than assuming you have the full body.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Symbol name whose references or callers you want. |
| `path` | string | no | Optional file path to disambiguate when several symbols share a name. |
| `min_confidence` | string | no | Lowest confidence to return: exact, high, medium or low. Default low. Recommended: 'high' for safe automated code changes and blast radius analysis to filter out heuristic false positives. |

### `find_references`

Find where a symbol is used. Every result carries a confidence tier: 'exact' means a compiler proved the binding, 'high' and 'medium' and 'low' mean RepoCortex inferred it and may be wrong. Pass min_confidence='high' when a wrong result costs more than a missing one. CRITICAL: an EMPTY result is ambiguous — it may mean nothing references this symbol, or that RepoCortex could not resolve the references that exist. Never conclude a symbol is unused from an empty result; confirm with search_code.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Symbol name whose references or callers you want. |
| `path` | string | no | Optional file path to disambiguate when several symbols share a name. |
| `min_confidence` | string | no | Lowest confidence to return: exact, high, medium or low. Default low. Recommended: 'high' for safe automated code changes and blast radius analysis to filter out heuristic false positives. |

### `find_callers`

Find the functions and methods that call a symbol — the callers, not merely the files mentioning it. Use before changing a signature or behaviour. Every result carries a confidence tier, from 'exact' (a compiler proved the binding) down through 'high', 'medium' and 'low' (RepoCortex inferred it and may be wrong); pass min_confidence='high' to see only bindings RepoCortex can defend. CRITICAL: an EMPTY result is ambiguous and unresolved calls are reported separately, so absence of callers here is NOT evidence the symbol is dead. Confirm with search_code before deleting anything.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Symbol name whose references or callers you want. |
| `path` | string | no | Optional file path to disambiguate when several symbols share a name. |
| `min_confidence` | string | no | Lowest confidence to return: exact, high, medium or low. Default low. Recommended: 'high' for safe automated code changes and blast radius analysis to filter out heuristic false positives. |

### `find_tests`

Find the tests covering a symbol, and which signal found each one — a proven call edge is far stronger evidence than a matching filename, and the response names which you got. Use before changing behaviour, to know what to run. A symbol reported as untested is a positive finding, not silence, but it is bounded by what the index could resolve: confirm with search_code before trusting it on a low-resolution repository.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `symbol` | string | yes | The symbol you are about to change, e.g. 'updateStatus' or 'TransactionService'. |
| `min_confidence` | string | no | Lowest confidence to return: exact, high, medium or low. Default low. Recommended: 'high' for safe automated code changes and blast radius analysis to filter out heuristic false positives. |
| `hub_degree_limit` | integer | no | Stop the walk from expanding THROUGH any symbol with more than this many edges. Default 0 (off), which is what every result you have seen so far used. Set it (50 is a reasonable start) when a result is enormous because the walk crossed a logger, a util or a base class; pruned symbols are still listed, only their neighbours are not explored, and the response says how many were cut. |

### `impact_analysis`

Before changing a symbol, find out what depends on it. Separates DIRECT callers (a proven call edge) from INDIRECT reach and from dispatch rows the runtime MAY execute but no call site proves, and names what it CANNOT see — dynamic dispatch, unresolved references, traversal limits. The risk score and its factors are only as good as the resolution behind them; the response states the coverage rate ahead of the score so you know whether to trust it. The result is a LOWER BOUND: treat an empty or short result as 'nothing found', never as proof that changing the symbol is safe.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `symbol` | string | yes | The symbol you are about to change, e.g. 'updateStatus' or 'TransactionService'. |
| `min_confidence` | string | no | Lowest confidence to return: exact, high, medium or low. Default low. Recommended: 'high' for safe automated code changes and blast radius analysis to filter out heuristic false positives. |
| `hub_degree_limit` | integer | no | Stop the walk from expanding THROUGH any symbol with more than this many edges. Default 0 (off), which is what every result you have seen so far used. Set it (50 is a reasonable start) when a result is enormous because the walk crossed a logger, a util or a base class; pruned symbols are still listed, only their neighbours are not explored, and the response says how many were cut. |

### `resolution_coverage`

Ask this when a find_callers, find_references or impact_analysis result came back shorter than you expected, BEFORE concluding anything from it. It reports how many of this index's references were actually bound to a definition, and names the two populations excluded from that rate: the ambiguous ones, where several definitions matched and RepoCortex refused to guess, and the ones that matched nothing here at all, which are mostly library calls. A low figure means every relationship answer from this index is a LOWER BOUND drawn from a minority of the evidence — an empty result then tells you about the index, not about the code — and the response names the command that raises it. Takes no arguments.

No parameters.

### `plan_rename`

Plan a symbol rename. Returns the edit SITES — a file and a line each, with no column — split into 'apply' (confidence 'exact': a compiler proved the binding) and 'review' (RepoCortex inferred the binding and may be wrong). You must locate the identifier on each line yourself; a line may contain it more than once or as part of a longer name. WRITES NOTHING. The plan is never complete: references RepoCortex could not resolve are absent and are not reported as absent, so after applying it, run search_code for the old name to find bindings that were missed entirely.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Symbol to rename. |
| `new_name` | string | yes | The replacement identifier. Used only to render the plan; nothing is written. |
| `path` | string | no | Optional file path to disambiguate when several symbols share a name. |
| `min_confidence` | string | no | Lowest confidence to return: exact, high, medium or low. Default low. Recommended: 'high' for safe automated code changes and blast radius analysis to filter out heuristic false positives. |

### `find_api_endpoints`

List the HTTP endpoints this service exposes, with the handler that serves each one and where it is declared. Routes are recognised from the framework's own syntax — a Spring annotation, a Go router registration, a FastAPI or Flask decorator, an Express member call — never from a string that merely looks like a path, which is what keeps a log message mentioning 'GET /users' out of the result. Pass method to filter to one HTTP verb. NOT covered: OpenAPI documents, GraphQL schemas, gRPC service definitions and API-gateway configuration — a service defined entirely in those reports no endpoints here, which is a coverage limit, not a finding about the code. Prefer this over grepping route decorators by hand.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `method` | string | no | Filter to one HTTP method, e.g. GET. Case-insensitive. |

### `find_db_access`

Report which code reads and which code writes each database table, with per-table read/write counts. Pass table to look up one table by name. READ and WRITE are counted separately: 'what touches this table' is a weaker question than 'what writes it', and the second is the one to check before changing a column. A table no migration here declares is still reported — that is the normal shape of a service reading a database another service owns. Some statements are assembled at run time; the response says how many, because whether that exact statement executes as written cannot be established by a static read. Prefer this over grepping for SQL string literals or ORM calls: those are exactly the queries this scan may also miss, and this tool says so instead of reporting a false all-clear.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `table` | string | no | Filter to one table name. Case-insensitive. |

### `get_config_key`

Look up one configuration key by name: where it is declared and how many places read it, joined into a single mismatch check. The two findings worth acting on are a key READ but nowhere DECLARED — the way a service fails at start-up — and a key declared and never read, which is either dead configuration or a lookup this scan cannot see. VALUES ARE NEVER READ: no field in the response can hold one, so a wrong redaction rule cannot leak one either; a key whose name looks like a credential is labelled sensitive by name alone. Declarations are read from .env, YAML, JSON, TOML and .properties files — Kubernetes, Helm, Terraform and CI definitions are not, so a key set only there reports as undeclared even though it works in production, which is a coverage limit, not a finding. Prefer this over grepping for the key string across the repository.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `key` | string | yes | The configuration key to look up, e.g. DATABASE_URL. |

### `get_file_outline`

Get a token-efficient structural outline of a source file by pruning implementation bodies while preserving all imports, types, interfaces, structs, function signatures, and docstrings. Use this instead of reading an entire large file (reducing 10,000 tokens to 300 tokens) when you only need to inspect declarations, API contracts, or symbol signatures.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `path` | string | yes | File path to outline (relative to repo root or absolute). Prunes implementation bodies to reduce token size while preserving all types, structs, interfaces, signatures, and docstrings. |

### `minimize_diff`

Compress unified git diffs into structured, token-efficient representations. Strips redundant index hashes and file timestamps, extracts enclosing symbol headers, and collapses unchanged context runs to minimize context token usage. Use this when analyzing pull requests, patches, or git diffs to preserve context window space.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `diff` | string | yes | Unified diff text (e.g. git diff output) to compress into a structured, token-efficient representation. |

### `plan_change`

Generate a deterministic refactoring and change plan for a symbol. Identifies all direct callers, assesses risk score, discovers covering test files, and produces an ordered execution sequence with pre/post-conditions. Use this before modifying or refactoring existing code to ensure regression-free changes.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `symbol` | string | yes | The symbol you are about to change, e.g. 'updateStatus' or 'TransactionService'. |
| `min_confidence` | string | no | Lowest confidence to return: exact, high, medium or low. Default low. Recommended: 'high' for safe automated code changes and blast radius analysis to filter out heuristic false positives. |
| `hub_degree_limit` | integer | no | Stop the walk from expanding THROUGH any symbol with more than this many edges. Default 0 (off), which is what every result you have seen so far used. Set it (50 is a reasonable start) when a result is enormous because the walk crossed a logger, a util or a base class; pruned symbols are still listed, only their neighbours are not explored, and the response says how many were cut. |

### `detect_blindspots`

Audit the repository for code intelligence blindspots and hidden risks. Detects unresolved external and internal calls, untested critical functions with high fan-in, unmapped HTTP endpoints, orphan database tables, missing config declarations, and unparsed files. Use this to audit codebase health and find coverage gaps.

No parameters.

Setup for each client: [agent integration](../agent-integration.md).
