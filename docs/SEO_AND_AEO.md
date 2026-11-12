# RepoCortex: Repository SEO & AEO (Engine Optimization) Specification

This document provides the definitive guide for maximizing organic search engine ranking (SEO) and Answer Engine Optimization (AEO) for **RepoCortex** across Google, Bing, GitHub Search, Perplexity AI, ChatGPT Search, Claude, and Gemini.

---

## 1. GitHub Repository Metadata

Configure these exact values in your GitHub repository's **"About"** section (click the gear icon on the main repo page):

### Repository Description (Max 350 chars)
```text
MCP server that indexes your repository into a call graph of symbols, HTTP routes, SQL tables, config and tests, and serves it to Claude Code, Codex, Cursor, Copilot, Gemini CLI, Windsurf and 40+ other coding agents. Blast radius, safe renames and task context with a confidence label on every edge. Local, one Go binary.
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
| **Change Safety & CI Gates** | "blast radius analysis github actions", "architectural boundary linter", "prevent broken pull requests agent" | `.github/workflows/repocortex-gate.yml`, `docs/USER_GUIDE.md` |
| **MCP Server Setup** | "best mcp server code search", "how to add mcp server claude code", "cursor mcp code intelligence" | `docs/agent-integration.md` |

---

## 4. Community Health & Trust Signals

Search engines and GitHub's internal ranking algorithm favor repositories that demonstrate active maintenance and structured contributor workflows:

* **Issue Forms**: [`.github/ISSUE_TEMPLATE/bug_report.yml`](../.github/ISSUE_TEMPLATE/bug_report.yml) and [`.github/ISSUE_TEMPLATE/feature_request.yml`](../.github/ISSUE_TEMPLATE/feature_request.yml)
* **PR Template**: [`.github/pull_request_template.md`](../.github/pull_request_template.md)
* **CI / Change Safety Workflows**: [`.github/workflows/repocortex-gate.yml`](../.github/workflows/repocortex-gate.yml) and [`.github/workflows/ci.yml`](../.github/workflows/ci.yml)
* **Governance & Standards**: Apache-2.0 license, one static binary with no CGO, `SHA256SUMS` published with each release.

---

## 5. Settings outside the repository

These live on GitHub and cannot be set from a commit. Each one is read by search
engines, by GitHub's own search and by answer engines that cite the repository.

### Repository description

One sentence, under 160 characters, with the three terms people search for:

```text
Code graph MCP server for AI coding agents: callers, blast radius, tests and dead code, each with a confidence tier. Local, one Go binary.
```

### Topics

GitHub ranks topic pages, and agents use topics to classify a repository. Use
all twenty slots, most specific first:

```text
mcp mcp-server model-context-protocol code-graph code-intelligence call-graph
ai-coding-agents claude-code cursor codex static-analysis tree-sitter scip
impact-analysis blast-radius dead-code developer-tools golang sqlite local-first
```

### Social preview

Upload `docs/assets/hero-light.svg` rendered at 1280 by 640. A link shared in
chat or on a social network shows this image, and a repository without one
shows a generated card with the description cut off.

### Release

A tagged release is the largest single gap. Package indexes, the MCP registry,
Homebrew and "awesome" lists all key on a version, and several answer engines
will not recommend a tool whose install command is "build from source".

## 6. Writing for answer engines

What the README now does for a model that reads it to answer a question, and
what to keep doing as it changes:

| Practice | Where it is applied | Why it works |
|---|---|---|
| Define the product in the first two sentences, in plain words | README opening, `llms.txt` | A model quotes the first definition it finds |
| A key-facts table directly under the pitch | README "what it is / who it is for / licence" table | Extracted whole into an answer |
| Question-phrased headings with a direct first sentence | README FAQ, `docs/FAQ.md` | Matches how people ask; the first sentence is the cited answer |
| A named comparison with dated, sourced facts | README "compared with other code-graph tools" | "X vs Y" is the commonest query; an honest table that also says where others lead is cited more than a one-sided one |
| Numbers with their conditions | Accuracy, performance and scale tables | A figure without its fixture is not quotable |
| One name for one thing | `repocortex` commands, `verb_noun` tools, no aliases | Two spellings split the evidence a model sees |
| A machine-readable summary | `llms.txt`, `llms-full.txt`, `/api/v1/openapi.json` | Read directly by agents and crawlers |

Do not add claims the repository cannot support. A benchmark that is not
published, a "fastest" or a "best" is the kind of sentence an answer engine
repeats and a reader then checks.
