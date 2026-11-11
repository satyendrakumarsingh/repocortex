# RepoCortex Documentation

Welcome to the RepoCortex documentation portal. RepoCortex is a local-first, zero-CGO code intelligence and change safety engine that parses source code into a deterministic knowledge graph and serves it to AI coding agents over Model Context Protocol (MCP).

---

## 🏛️ Documentation Taxonomy

```text
docs/
├── getting-started/     # First-time adoption & environment setup
├── concepts/            # Architectural principles, invariants & mental models
├── reference/           # Exhaustive CLI, MCP, and configuration specifications
└── operations/          # CI/CD change-safety gates, tuning & troubleshooting
```

---

## 1. 🚀 Getting Started

Quick onboarding paths to get RepoCortex running in your project and connected to your AI agent.

| Guide | Description |
|---|---|
| [**Quickstart Guide**](getting-started/quickstart.md) | 60-second end-to-end guide: install, index, resolve, and verify |
| [**Agent Integration**](getting-started/agent-integration.md) | Wiring instructions for Cursor, Claude Code, Windsurf, Antigravity, Claude Desktop, and Zed |
| [**Installation**](getting-started/installation.md) | Automated curl installer, Go install, and precompiled universal release binaries |

---

## 2. 🧠 Concepts & Architecture

The engineering rationale and formal models behind deterministic code graphs.

| Document | Description |
|---|---|
| [**Why Not Vector RAG?**](concepts/why-not-vector-rag.md) | Lexical similarity vs causal call-graph reachability for code intelligence |
| [**Architecture & Invariants**](concepts/architecture.md) | Hexagonal pipeline, in-memory CSR graph, and zero-CGO design |
| [**Accuracy & Provenance**](reference/accuracy-and-benchmarks.md) | Adjudication of 30,975 edges against compiler ground truth across 4 confidence tiers |
| [**Non-Goals**](NON-GOALS.md) | Deliberate architectural refusals and scope boundaries |

---

## 3. 📖 Technical Reference

Authoritative specifications for commands, tools, and options.

| Reference | Description |
|---|---|
| [**CLI Reference**](reference/cli.md) | Complete manual of all 35+ CLI commands, flags, and outputs |
| [**MCP Tools Reference**](reference/mcp-tools.md) | Parameters and profiles for all 18 Model Context Protocol tools |
| [**Configuration**](user-manual.md#configuration) | Local `.cortex.yml` schema and global configuration options |
| [**FAQ**](FAQ.md) | Answers to common technical and architectural questions |

---

## 4. ⚙️ Operations & Hardening

Running RepoCortex in production pipelines and automated environments.

| Guide | Description |
|---|---|
| [**CI/CD Change-Safety Gates**](operations/ci-cd-gates.md) | GitHub Actions workflow, SARIF 2.1.0 reporting, and sticky PR comments |
| [**Troubleshooting Catalogue**](operations/troubleshooting.md) | Comprehensive error catalog keyed to literal error strings with remedies |
| [**Performance & Tuning**](operations/performance.md) | Memory-mapped I/O, SQLite pragmas, CSR memory footprints, and worker scaling |
| [**Security & Threat Model**](security.md) | Path containment, secret redaction, and local execution boundaries |

---

## Contributing & Community

- [**Contributing Guidelines**](../.github/CONTRIBUTING.md): Core invariants, coding standards, and test tiers
- [**Code of Conduct**](../.github/CODE_OF_CONDUCT.md): Contributor standards and pledge
- [**Security Policy**](../.github/SECURITY.md): Coordinated vulnerability disclosure
