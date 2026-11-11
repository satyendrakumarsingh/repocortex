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
| Listing in the MCP registry | Planned | `smithery.yaml` exists; the official MCP registry entry does not. |

## Integrations

| Item | Status | Why it matters |
|---|---|---|
| Reusable GitHub Action (`uses: satyendrakumarsingh/repocortex@v1`) | Planned | Today the change-safety gate is a workflow you copy. A versioned action is one line and updates itself. |
| Editor extension (VS Code) | Exploring | Blast radius and callers inline, from the same local index the agent uses. |
| Recorded demo | Planned | A short screen recording of an agent calling `get_task_context` and `impact_analysis`. |

## Product

| Item | Status | Why it matters |
|---|---|---|
| Test discovery through interface dispatch | Planned | `untested_critical` misses tests that reach a method through an interface or a shared method name. It is reported, not gated, until this lands. |
| Call binding by column, not line | Exploring | The compiler import only promotes a call when its line has one target, so `return r.Key()` beside a `Key` type stays unbound. |
| Incremental re-binding of cross-file handlers | Planned | `cortex resolve` binds route handlers declared in another file; an incremental update does not re-run that pass. |
| Deeper resolution for more languages | Exploring | Go, Java and TypeScript are `typed`; 11 more languages are `resolved`; 192 are `parsed` (definitions and search only). See `cortex languages`. |

Suggestions and votes: [open a discussion](https://github.com/satyendrakumarsingh/repocortex/discussions).
