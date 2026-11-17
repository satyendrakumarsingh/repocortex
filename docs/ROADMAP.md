# Roadmap

What RepoCortex does not do yet, and what is being worked on. Nothing here has a
date; items move to the [CHANGELOG](../CHANGELOG.md) when they ship.

## Distribution

| Item | Status | Why it matters |
|---|---|---|
| Tagged releases with binaries | **Shipped in v1.0.1** | `install.sh`, `install.ps1` and the offline archives resolve a published release. |
| Homebrew formula updated by the release job | **Needed now** | The formula in `satyendrakumarsingh/homebrew-tap` pins version and checksums by hand, and still names the old `cortex` binary. It must be renamed to `repocortex` and bumped to v1.0.1; the release job should open the bump. |
| Container image on GHCR | **Shipped** | Published at `ghcr.io/satyendrakumarsingh/repocortex` on release. CI users pull a pinned multi-arch container image. |
| Windows package (winget / Scoop) | **Shipped** | Package manifests published in `manifests/` for Scoop and Windows Package Manager. |
| Signed releases, SBOM and provenance | **Shipped** | Release artifacts signed via Cosign keyless OIDC with Syft SPDX SBOMs and GitHub in-toto provenance attestations. |
| Listing in the MCP registry | **Shipped** | `mcp-registry.json` and `smithery.yaml` configured for official MCP discovery. |
| MCPB bundle | **Shipped** | Builds and attaches `.mcpb` bundle assets for one-click desktop installs. |

## Integrations

| Item | Status | Why it matters |
|---|---|---|
| Reusable GitHub Action (`uses: satyendrakumarsingh/repocortex@v1`) | **Shipped** | Composite action in `action.yml` runs index, SCIP import, PR impact, SARIF and sticky comments in one step. |
| Install links for agents | **Shipped** | `repocortex install --links` prints 1-click deep-links for Cursor (`cursor://`), VS Code (`vscode://`), Windsurf and LM Studio. |
| Editor extension (VS Code) | **Shipped** | Blast radius, graph queries and callers inline in `editors/vscode`, reading from the local SQLite index. |
| Recorded demo | Planned | A short screen recording of an agent calling `get_task_context` and `analyze_impact`. |

## Product

| Item | Status | Why it matters |
|---|---|---|
| Vue 3 SFC SCIP indexing | **Shipped** | Synthesizes `.vue.ts` mirrors with whitespace masking to preserve 1:1 line/column coordinates for `scip-typescript`. |
| Compact MCP frontdoor profile | **Shipped** | Exposes a single unified `repocortex` router tool, about 550 tokens of schema against 9,100 for the full profile (`repocortex tool list`). |
| Temporal git coupling for impact | **Shipped** | Mines commit co-change history to boost PR blast-radius recall and discover coupled companion files. |
| Test discovery through interface dispatch | Planned | `untested_critical` misses tests that reach a method through an interface or a shared method name. It is reported, not gated, until this lands. |
| Declarative graph query (`search_graph`) | **Shipped** | Guarded read-only SQL graph query tool via MCP (`search_graph`), CLI (`repocortex query`) and compact frontdoor (`action: query`), matching GitNexus and codebase-memory-mcp. |
| Call binding by column and name | **Shipped** | `PromoteCalls` pairs compiler occurrences with parser call columns via `pairsOneToOne` and `(line, name)` grouping. |
| Incremental re-binding of cross-file handlers | **Shipped** | An incremental update runs the same resolve pass as `index --full`, so a route handler declared in another file is bound again after either file changes. |
| Deeper resolution for more languages | Exploring | Eight languages are `typed` (C#, C++, Go, Java, Kotlin, Python, Scala, TypeScript), six are `resolved` (C, JavaScript, PHP, Ruby, Rust, TSX), 77 are `parsed` (definitions and search only) and 115 are text only. Python was promoted on a measurement against `scip-python` over four repositories ([accuracy.md](accuracy.md)); the others are typed by an arity query and were read by hand on one project each ([verification](verification/2026-11-13/README.md)), where their `low` tier measured poorly. Rust is next and its tier has no compiler-index measurement. See `repocortex languages`. |

## Known issues

Found while writing the [CLI output guide](guides/cli-output-guide.md) and the [portal guide](guides/portal-guide.md) by running every command and screen. Every issue found then has been fixed; see the [CHANGELOG](../CHANGELOG.md). New ones go here.

Suggestions and votes: [open a discussion](https://github.com/satyendrakumarsingh/repocortex/discussions).
