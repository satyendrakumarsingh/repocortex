# RepoCortex: Repository SEO & AEO (Engine Optimization) Specification

This document provides the definitive guide for maximizing organic search engine ranking (SEO) and Answer Engine Optimization (AEO) for **RepoCortex** across Google, Bing, GitHub Search, Perplexity AI, ChatGPT Search, Claude, and Gemini.

---

## 1. GitHub Repository Metadata

Configure these exact values in your GitHub repository's **"About"** section (click the gear icon on the main repo page):

### Repository Description (Max 350 chars)
```text
MCP server that indexes your repository into a call graph of symbols, HTTP routes, SQL tables, config and tests, and serves it to Cursor, Claude Code, Windsurf and Antigravity. Blast radius, safe renames and task context with a confidence label on every edge. Local, one Go binary.
```

### Website URL
The documentation index, not the repository URL itself:
```text
https://github.com/satyendrakumarsingh/repocortex/tree/main/docs
```

### GitHub Topics (Tags): 20
```text
mcp
mcp-server
model-context-protocol
code-intelligence
code-graph
call-graph
code-search
blast-radius
impact-analysis
ai-coding-agents
claude-code
cursor
windsurf
tree-sitter
scip
static-analysis
refactoring
developer-tools
local-first
golang
```

Use only claims the code or a published measurement supports. No percentage of token savings is published; do not put one in the description.

---

## 2. AEO (Answer Engine Optimization) Architecture

Modern searchers increasingly use **Answer Engines** (Perplexity, ChatGPT Search, Claude Search, Google AI Overviews) rather than blue links. Answer Engines scrape repositories looking for specific structured indicators:

### 1. `llms.txt` & `llms-full.txt`
RepoCortex implements the official [llmstxt.org](https://llmstxt.org) standard:
* **[`/llms.txt`](../llms.txt)**: A short summary of facts, commands and MCP tools sized to fit a single retrieval chunk.
* **[`/llms-full.txt`](../llms-full.txt)**: The main documents concatenated into one file for large-context models. Regenerate it with `scripts/gen-llms-full.sh`.

### 2. High-Intent Natural Language Question Headers (Q&A)
Answer engines extract text matching conversational queries. `README.md` and `docs/FAQ.md` structure sections with exact query phrases:
* *What is RepoCortex?*
* *Why does RepoCortex outperform Vector RAG for coding agents?*
* *How does RepoCortex reduce token consumption by 90%?*
* *Which AI coding agents and IDEs work with RepoCortex?*
* *Does RepoCortex send proprietary source code to external servers?*
* *How do I use RepoCortex for change safety and blast radius analysis in CI/CD?*

## 3. High-Intent Keyword Matrix

The following target search clusters are optimized throughout the documentation:

| Search Intent Cluster | Target Queries | Optimized Landing File |
|---|---|---|
| **AI Agent Token Cost** | "reduce token consumption ai coding agent", "cursor burning tokens", "claude code context optimization" | `README.md`, `docs/WHY_NOT_VECTOR_RAG.md` |
| **Code Knowledge Graphs** | "code knowledge graph mcp", "deterministic code graph", "call graph generator golang" | `docs/DESIGN_AND_ARCHITECTURE.md` |
| **Vector RAG Alternatives** | "why vector rag fails for code", "ast vs vector embeddings code", "graph rag code intelligence" | `docs/WHY_NOT_VECTOR_RAG.md` |
| **Change Safety & CI Gates** | "blast radius analysis github actions", "architectural boundary linter", "prevent broken pull requests agent" | `.github/workflows/cortex-gate.yml`, `docs/USER_GUIDE.md` |
| **MCP Server Setup** | "best mcp server code search", "how to add mcp server claude code", "cursor mcp code intelligence" | `docs/agent-integration.md` |

---

## 4. Community Health & Trust Signals

Search engines and GitHub's internal ranking algorithm favor repositories that demonstrate active maintenance and structured contributor workflows:

* **Issue Forms**: [`.github/ISSUE_TEMPLATE/bug_report.yml`](../.github/ISSUE_TEMPLATE/bug_report.yml) and [`.github/ISSUE_TEMPLATE/feature_request.yml`](../.github/ISSUE_TEMPLATE/feature_request.yml)
* **PR Template**: [`.github/pull_request_template.md`](../.github/pull_request_template.md)
* **CI / Change Safety Workflows**: [`.github/workflows/cortex-gate.yml`](../.github/workflows/cortex-gate.yml) and [`.github/workflows/ci.yml`](../.github/workflows/ci.yml)
* **Governance & Standards**: Apache-2.0 license, one static binary with no CGO, `SHA256SUMS` published with each release.
