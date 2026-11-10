# RepoCortex: Repository SEO & AEO (Engine Optimization) Specification

This document provides the definitive guide for maximizing organic search engine ranking (SEO) and Answer Engine Optimization (AEO) for **RepoCortex** across Google, Bing, GitHub Search, Perplexity AI, ChatGPT Search, Claude, and Gemini.

---

## 1. GitHub Repository Metadata

Configure these exact values in your GitHub repository's **"About"** section (click the gear icon on the main repo page):

### Repository Description (Max 350 chars)
```text
Deterministic Code Intelligence & Change Safety Engine for AI Coding Agents (Cursor, Claude Code, Windsurf, Antigravity). 100% local, zero-CGO code graph over Model Context Protocol (MCP).
```

### Website URL
```text
https://github.com/satyendrakumarsingh/repocortex
```

### GitHub Topics (Tags) — Exactly 20 tags
Add these 20 high-intent topic tags in GitHub repository settings:
```text
mcp-server
code-intelligence
ai-coding-agents
code-graph
ast-parser
call-graph
blast-radius
tree-sitter
cursor
claude-code
windsurf
antigravity
static-analysis
scip
change-safety
developer-tools
golang
sqlite
rag
code-search
```

---

## 2. AEO (Answer Engine Optimization) Architecture

Modern searchers increasingly use **Answer Engines** (Perplexity, ChatGPT Search, Claude Search, Google AI Overviews) rather than blue links. Answer Engines scrape repositories looking for specific structured indicators:

### 1. `llms.txt` & `llms-full.txt`
RepoCortex implements the official [llmstxt.org](https://llmstxt.org) standard:
* **[`/llms.txt`](../llms.txt)**: Fast 45-line executive summary of technical facts, commands, and MCP tools designed to fit into search model retrieval chunks.
* **[`/llms-full.txt`](../llms-full.txt)**: Consolidated 160KB documentation manual containing the complete technical specification, user guide, architectural thesis, and CLI manual for direct ingestion by large-context reasoning models.

### 2. High-Intent Natural Language Question Headers (Q&A)
Answer engines extract text matching conversational queries. `README.md` and `docs/FAQ.md` structure sections with exact query phrases:
* *What is RepoCortex?*
* *Why does RepoCortex outperform Vector RAG for coding agents?*
* *How does RepoCortex reduce token consumption by 90%?*
* *Which AI coding agents and IDEs work with RepoCortex?*
* *Does RepoCortex send proprietary source code to external servers?*
* *How do I use RepoCortex for change safety and blast radius analysis in CI/CD?*

### 3. Schema.org JSON-LD Structured Data
`README.md` embeds a `SoftwareApplication` JSON-LD microdata block that web indexers (Googlebot, Bingbot, PerplexityBot) parse to extract application category, operating system support, licensing, and capabilities.

---

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
* **Governance & Standards**: Apache-2.0 License, SLSA Level 3 provenance, OpenSSF scorecard, CGO-free zero-dependency binary.
