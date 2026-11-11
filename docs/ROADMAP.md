# Roadmap

What RepoCortex does not do yet, and what is being worked on. Nothing here has a
date; items move to the [CHANGELOG](../CHANGELOG.md) when they ship.

## Distribution

| Item | Status | Why it matters |
|---|---|---|
| Tagged releases with binaries | **Needed now** | Only a `canary` pre-release exists. `install.sh` resolves `releases/latest` and the Homebrew formula downloads `v1.0.0` assets; both fail until a versioned release is published. |
| Homebrew formula updated by the release job | Planned | The formula in `satyendrakumarsingh/homebrew-tap` pins version and checksums by hand. The release job should open the bump. |
| Container image on GHCR | Planned | A `Dockerfile` exists but no image is published. CI users would pull a pinned image instead of building. |
| Windows package (winget / Scoop) | Planned | Windows users currently run `install.ps1` or download a zip. |
| Signed releases, SBOM and provenance | Planned | The release job publishes `SHA256SUMS` only. Signing and an SBOM let users verify what they run. |
| Listing in the MCP registry | Planned | `smithery.yaml` exists; the official MCP registry entry does not. The registry accepts npm, PyPI, NuGet, Cargo, OCI and MCPB packages, so it needs an MCPB bundle or the GHCR image first. |
| MCPB bundle | Planned | One-click install in Claude Desktop and other clients that read `.mcpb` files. |

## Integrations

| Item | Status | Why it matters |
|---|---|---|
| Reusable GitHub Action (`uses: satyendrakumarsingh/repocortex@v1`) | Planned | Today the change-safety gate is a workflow you copy. A versioned action is one line and updates itself. |
| Install links for agents | Exploring | Cursor, VS Code and LM Studio accept install deeplinks; a link per agent in the docs and the portal saves a step. |
| Editor extension (VS Code) | Exploring | Blast radius and callers inline, from the same local index the agent uses. |
| Recorded demo | Planned | A short screen recording of an agent calling `get_task_context` and `impact_analysis`. |

## Product

| Item | Status | Why it matters |
|---|---|---|
| Test discovery through interface dispatch | Planned | `untested_critical` misses tests that reach a method through an interface or a shared method name. It is reported, not gated, until this lands. |
| Call binding by column, not line | Exploring | The compiler import only promotes a call when its line has one target, so `return r.Key()` beside a `Key` type stays unbound. |
| Incremental re-binding of cross-file handlers | Planned | `cortex resolve` binds route handlers declared in another file; an incremental update does not re-run that pass. |
| Deeper resolution for more languages | Exploring | Go, Java and TypeScript are `typed`; 11 more languages are `resolved`; 192 are `parsed` (definitions and search only). See `cortex languages`. |

## Known issues

Found while writing the [CLI output guide](guides/cli-output-guide.md) and the [portal guide](guides/portal-guide.md) by running every command and screen.

| Area | Issue |
|---|---|
| `rename` | Definition sites stay `high` after a compiler import, so the default `--floor exact` refuses every rename; only `--floor high` applies. |
| `index` | An incremental index does not start a new run, so `diff` between two commits can compare a run with itself. `--full` works around it. |
| `index` / `resolve` | Output says "unresolved until M2" (an internal milestone name), and `resolve` after an incremental index reports "Edges: 0" for new edges only. |
| `compile` / `verify` | The 12-entry scope can fill with configuration keys and push the changed class out, so `verify` reports the file as in neither scope. |
| `references` | A type used as a field or constructor parameter is not found as a reference. |
| `callees` / `missed` | Fields used as receivers are listed as calls; `missed` totals change with `--limit`. |
| `config` / `deps` | Keys in `.cortex.yml` and Compose files count as "declared and never read"; Maven coordinates never match Java package names, so every dependency reads "not imported". |
| `trace` | A span matched by `code.function` and `code.namespace` is not joined to the method; only `http.route` matches. |
| `plan-change` vs `impact` | The two report risk on different scales without saying so. |
| Portal: Plan a refactor | Caller lines show `:undefined`, the example chips name symbols from another codebase, and `?sym=` is ignored. |
| Portal: What changed | The empty state suggests `?from=`/`?to=`, but the page reads `dfrom`/`dto`. |
| Portal: Test coverage vs Code blindspots | Test coverage shows symbols with no proven test while Code blindspots shows "Untested critical 0". |
| `cortex ui --help`, CLI reference | Still say twenty screens; the rail has 23. |

Suggestions and votes: [open a discussion](https://github.com/satyendrakumarsingh/repocortex/discussions).
