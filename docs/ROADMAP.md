# Roadmap

What RepoCortex does not do yet, and what is being worked on. Nothing here has a
date; items move to the [CHANGELOG](../CHANGELOG.md) when they ship.

## Distribution

| Item | Status | Why it matters |
|---|---|---|
| Tagged releases with binaries | **Shipped in v1.0.1** | `install.sh`, `install.ps1` and the offline archives resolve a published release. |
| Homebrew formula updated by the release job | **Ready; publishes with the next release** | The release job renders `repocortex.rb` from the release's `SHA256SUMS` (`scripts/release/render-homebrew-formula.sh`) and opens a pull request on `satyendrakumarsingh/homebrew-tap` that replaces the old `cortex` formula, with a rename entry so existing installs follow. It needs a `HOMEBREW_TAP_TOKEN` secret. `packaging/homebrew/repocortex.rb` is the rendered v1.0.1 formula. |
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
| Test discovery through interface dispatch | **Shipped** | `find_tests` and `untested_critical` follow a method to the interface or base-class methods it implements, and report the tests that call those as `interface_dispatch`. |
| Declarative graph query (`search_graph`) | **Shipped** | Guarded read-only SQL graph query tool via MCP (`search_graph`), CLI (`repocortex query`) and compact frontdoor (`action: query`), matching GitNexus and codebase-memory-mcp. |
| Call binding by column and name | **Shipped** | `PromoteCalls` pairs compiler occurrences with parser call columns via `pairsOneToOne` and `(line, name)` grouping. |
| Incremental re-binding of cross-file handlers | **Shipped** | An incremental update runs the same resolve pass as `index --full`, so a route handler declared in another file is bound again after either file changes. |
| Deeper resolution for more languages | Exploring | 12 languages are `typed` (C#, C++, Go, Java, JavaScript, Kotlin, PHP, Python, Ruby, Rust, Scala, TypeScript), 29 are `resolved` (Apex, Bash, C, Clojure, CMake, CUDA, Dart, Elixir, Erlang, F#, Fortran, GLSL, Groovy, Haskell, HLSL, Julia, Lua, MATLAB, Objective-C, OCaml, Perl, PowerShell, R, Solidity, Svelte, Swift, TSX, Vue, Zig), 74 are `parsed` (definitions and search only) and 91 are text only. Python was promoted on a measurement against `scip-python` over four repositories ([accuracy.md](accuracy.md)), and JavaScript against `scip-typescript` on npm's own source (99.88% of 4,932 `high` edges correct). PHP, Ruby and Rust are typed on multi-file fixtures only, with no compiler-index measurement; the others were read by hand on one project each ([verification](verification/2026-11-13/README.md)), where their `low` tier measured poorly. Measuring PHP, Ruby and Rust against `scip-php`, `scip-ruby` and `rust-analyzer` is next. See `repocortex languages`. |

## Known issues

Found while writing the [CLI output guide](guides/cli-output-guide.md) and the [portal guide](guides/portal-guide.md) by running every command and screen. Every issue found then has been fixed; see the [CHANGELOG](../CHANGELOG.md). New ones go here.

Suggestions and votes: [open a discussion](https://github.com/satyendrakumarsingh/repocortex/discussions).
