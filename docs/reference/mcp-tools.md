# Model Context Protocol (MCP) Tools Reference

RepoCortex exposes a native, high-performance [Model Context Protocol (MCP)](https://modelcontextprotocol.io) server over `stdio` JSON-RPC. It communicates natively with **Cursor, Claude Code, Windsurf, Google Antigravity, Claude Desktop, and Zed**.

---

## Tool Overview

| Tool Name | Profile | Purpose | Token Budget |
|---|:---:|---|:---:|
| `get_task_context` | Minimal / Standard / Full | Single-call causal task retrieval (files, symbols, callers, tests) | ~800 tokens |
| `impact_analysis` | Minimal / Standard / Full | Transitive blast radius of modifying a symbol with confidence tiers | ~1,200 tokens |
| `find_callers` | Minimal / Standard / Full | Immediate and upstream callers entering a function or method | ~400 tokens |
| `find_tests` | Minimal / Standard / Full | Covering test functions proven along static call paths | ~500 tokens |
| `find_symbol` | Standard / Full | Precise symbol definition lookup and signature | ~300 tokens |
| `find_references` | Standard / Full | Cross-file references to a symbol with provenance | ~600 tokens |
| `search_code` | Standard / Full | SQLite FTS5 BM25 search over indexed symbols & docstrings | ~500 tokens |
| `get_project_overview` | Standard / Full | Language breakdown, symbol counts, and graph health | ~350 tokens |
| `detect_blindspots` | Full | Unreferenced symbols, orphaned SQL tables, and dead routes | ~700 tokens |
| `plan_change` | Full | Ordered refactoring sequence with dependency verification gates | ~1,500 tokens |

---

## Detailed Tool Specifications

### 1. `get_task_context`
Resolves entry-point files, definitions, call hierarchies, and covering test files for a user prompt.

* **Parameters**:
  - `task` (string, required): The task description or question (e.g. `"how does authentication token refresh work?"`).
  - `max_tokens` (integer, optional): Maximum response token budget (default: 1500).
* **Returns**:
  Structured Markdown containing relevant source paths, entry symbols, upstream callers, downstream callees, and test files with confidence ratings.

### 2. `impact_analysis`
Traces the directional transitive blast radius if a symbol is edited or refactored.

* **Parameters**:
  - `symbol` (string, required): Fully-qualified symbol name or unique suffix (e.g. `OrderController.Checkout`).
  - `depth` (integer, optional): Maximum traversal depth (default: 3).
  - `min_confidence` (string, optional): Confidence floor: `exact`, `high`, `medium`, `low` (default: `high`).
* **Returns**:
  Reachability tree, risk score (0-100), breakdown of compiler-proven vs heuristic edges, and impacted HTTP routes and event topics.

### 3. `find_callers`
Traces who invokes a given function or method.

* **Parameters**:
  - `symbol` (string, required): Symbol to inspect.
  - `depth` (integer, optional): Upstream traversal depth (default: 1).

### 4. `find_tests`
Finds automated tests that exercise a given function along proven static call edges.

* **Parameters**:
  - `symbol` (string, required): Symbol to test.

---

## MCP Profiles

Configure the tool footprint via `.cortex.yml` or CLI flag `--mcp-profile`:
- **`minimal`**: Exposes only `get_task_context`, `impact_analysis`, `find_callers`, and `find_tests`. Optimal for token-constrained contexts.
- **`standard`** (default): Adds `find_symbol`, `find_references`, `search_code`, and `get_project_overview`.
- **`full`**: Adds `detect_blindspots` and `plan_change`.
