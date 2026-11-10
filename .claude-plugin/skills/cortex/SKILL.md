---
name: devintel
description: Use when working on code that already exists in this repository — before editing a symbol, changing a signature or behaviour, hunting for callers or tests, or orienting in an unfamiliar area. Explains when to reach for the RepoCortex MCP tools instead of grep, and how to read their confidence levels, coverage warnings and empty results.
---

# RepoCortex

The RepoCortex MCP server answers questions about this repository from a resolved
index: symbols, references, callers, tests, APIs, database access and
configuration, each carrying the source location that justifies it.

The tool surface is deliberately small — seven tools by default, nine under the
`standard` profile — because every exposed schema is injected into context on
every request. This skill adds no tools. It tells you which of the existing ones
to reach for, and how to read what comes back.

## Rules

<!-- cortex:guidance -->
## RepoCortex

RepoCortex serves a resolved index of this repository over MCP. For questions
about code that already exists, prefer it to grep and to opening files.

- Before changing code, call `get_task_context` with the task in plain English.
  It returns the files, symbols, tests and tables the change touches, found by
  following the call graph. One call instead of a dozen exploratory ones.
- Before editing a symbol, ask for its blast radius: `impact_analysis` where the
  profile exposes it, otherwise `find_callers`. Read it before the edit, not
  after a test fails.
- Before changing behaviour, `find_tests` says what to run and which signal
  found each test. A proven call edge outranks a matching filename.
- Every result carries a confidence. `exact` means a compiler proved it;
  `high`, `medium` and `low` were inferred and may be wrong. Treat anything
  below `exact` as a lead to verify, never as a fact to act on.
- An empty relationship result means UNRESOLVED, not absent. "No callers" is not
  proof a symbol is unused; do not delete or narrow one on that evidence.
- A `COVERAGE TOO LOW` or `COVERAGE UNKNOWN` note outranks the list beneath it.
  Run `cortex resolve` and ask again rather than reporting that list.
- "No index has been built" is a missing index, not a broken tool. Run
  `cortex index`, then `cortex resolve`, and ask again.
- `find_symbol` matches whole names only; use `search_code` for partial ones.
  Generated, oversized and excluded files are not indexed, so absence from the
  index is not absence from the repository.
- The index is a snapshot, not your working tree. Read files you have just
  edited directly, or keep the index current with `cortex watch`.
<!-- /cortex:guidance -->

## When RepoCortex is the wrong tool

- Files you have just written or edited this session. The index describes the
  last committed run, not your working tree.
- Code outside this repository: dependencies, the standard library, generated
  and oversized files. These are not indexed, and the tools say so rather than
  guessing.
- Anything that is not a question about code — build output, logs, the network.

## The same answers from the shell

Every MCP tool has a CLI counterpart, useful when you want the reasoning printed
rather than summarised:

    cortex context "<task>"     # what get_task_context serves, with exclusions
    cortex callers <symbol>     # what find_callers serves
    cortex status               # index revision, counts, resolution coverage

`cortex agent --check` reports whether the index, the MCP registration and
this guidance are actually wired up.
