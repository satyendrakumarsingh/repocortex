# Naming conventions

How tools, commands, flags, routes and code are named in RepoCortex, and why.
The rules for tools and commands are enforced by tests, named beside each rule.

## Contents

- [Agent tools](#agent-tools)
- [Commands](#commands)
- [Tool to command](#tool-to-command)
- [Flags and arguments](#flags-and-arguments)
- [HTTP routes](#http-routes)
- [Renaming something](#renaming-something)
- [Go code](#go-code)

## Agent tools

A tool name is read by a model on every request, before its description. It is
the cheapest steer the server has, so every tool follows one form:

```text
<verb>_<noun phrase>        lower snake case
```

The verbs are the ones the wider tool ecosystem already uses, with the meanings
it already gives them, so a model that has seen any other server arrives knowing
what each returns. `get`, `list` and `search` are the vocabulary of GitHub's and
most vendors' MCP servers; `find` is the language servers' word for a
relationship lookup. The list is closed.

| Verb | The answer is | Example |
|---|---|---|
| `get_` | One known thing, by name or path | `get_symbol`, `get_file_outline` |
| `list_` | A collection of one kind, optionally filtered. Takes no target | `list_api_endpoints`, `list_hotspots`, `list_dead_code` |
| `find_` | The things related to one named symbol. May be empty, and empty is never proof of absence | `find_callers`, `find_tests` |
| `search_` | Ranked text matches. No relationship is claimed | `search_code` |
| `trace_` | An ordered route through the graph | `trace_call_path` |
| `analyze_` | A computed judgement, not a list of facts | `analyze_impact`, `analyze_change` |
| `plan_` | An ordered set of edits. Never writes | `plan_rename`, `plan_change` |
| `minimize_` | A transformation of text the caller supplies | `minimize_diff` |

Rules:

- No noun phrases. It is `analyze_impact`, not `impact_analysis`.
- `list_` when the caller names nothing, `find_` when the caller names a symbol.
  `list_hotspots` ranks the repository; `find_callers` answers about one function.
- No product prefix. The server is already named; `repocortex_find_callers` spends
  tokens saying it twice.
- One name per tool. There are no aliases: a second name is a second thing for a
  model to choose between.
- Adding a verb is a design decision about a new kind of answer. It happens in
  `internal/mcp/names.go`, with a line in the table above.
- Prompts follow the same form: `plan_refactor`, `analyze_change`, `get_oriented`.

Enforced by `TestEveryToolNameFollowsTheConvention` in `internal/mcp`.

## Commands

```text
lower case, words joined by hyphens:   dead-code   plan-change   minimize-diff
```

| Rule | Right | Wrong |
|---|---|---|
| A noun for a thing you look at | `callers`, `hotspots`, `blindspots` | `show-callers` |
| A verb or verb-noun for a thing you do | `index`, `rename`, `plan-change` | `renamer` |
| Never noun-verb | `minimize-diff` | `diff-minimize` |
| The tool's name minus its verb, where a tool exists | `dead-code` for `list_dead_code` | `unused` |
| One line of help, capitalised, no full stop | `Find what calls a symbol` | `finds callers.` |

Every command belongs to one group in `repocortex --help`, in the order a new user
meets them: set up, ask, change, check, serve. The grouping is one table in
`internal/cli/groups.go`.

Enforced by `TestCommandNamesFollowTheConvention` and `TestEveryCommandHasAGroup`
in `internal/cli`.

## Tool to command

Every agent tool has a command for a person. `TestEveryAgentToolHasACommand`
keeps this table complete.

| Tool | Command | | Tool | Command |
|---|---|---|---|---|
| `get_task_context` | `context` | | `plan_rename` | `rename` |
| `get_project_overview` | `status` | | `plan_change` | `plan-change` |
| `get_resolution_coverage` | `status` | | `list_api_endpoints` | `api` |
| `search_code` | `search` | | `list_db_tables` | `db` |
| `find_symbol`, `get_symbol` | `symbol` | | `get_config_key` | `config` |
| `find_references` | `references` | | `get_file_outline` | `outline` |
| `find_callers` | `callers` | | `minimize_diff` | `minimize-diff` |
| `find_callees` | `callees` | | `list_blindspots` | `blindspots` |
| `find_implementations` | `implementations` | | `list_dead_code` | `dead-code` |
| `trace_call_path` | `path` | | `list_hotspots` | `hotspots` |
| `find_tests` | `tests` | | `analyze_impact` | `impact` |
| `analyze_change` | `impact --base --head` | | `find_owners` | `owners` |

Any tool is also callable as itself: `repocortex tool call <tool>`.

## Flags and arguments

| Rule | Example |
|---|---|
| Flags are lower case with hyphens | `--min-confidence`, `--include-exported` |
| Tool arguments are lower snake case | `min_confidence`, `include_exported` |
| The same idea has the same name everywhere | `--min-confidence` on every relationship command |
| A boolean flag names what turning it on adds | `--include-tests`, not `--no-skip-tests` |
| `--limit` is `-n`; `--path` narrows by path prefix or disambiguates by substring | |
| `--json` is global and prints the report itself | |

## HTTP routes

| Route | Rule |
|---|---|
| `/mcp` | The protocol's own name |
| `/api/v1/...` | Versioned from the first release; a breaking change is `/api/v2` |
| `/api/v1/tools/{name}` | The tool name, unchanged. `POST` calls, `GET` describes |
| `/healthz` | The convention supervisors already probe |

JSON keys are lower snake case (`is_error`, `estimated_tokens`), matching the
tool arguments.

## Renaming something

A tool, a command and a flag each have exactly one name. Nothing has an alias.

- **Before the first tagged release, a rename is just a rename.** Nobody depends
  on the old name, and keeping it would mean documenting, completing and testing
  two spellings of one thing.
- **After a release, a rename is a breaking change** and is made as one: in a
  major version, named in the changelog, with the old name removed.

`TestCommandNamesFollowTheConvention` fails if a command declares an alias.

## Go code

The full standard is in [design and architecture](../DESIGN_AND_ARCHITECTURE.md#3-code-conventions--engineering-standards).
The parts about names:

| Thing | Rule | Example |
|---|---|---|
| Package | One lower-case word, named for what it holds | `deadcode`, `hotspots`, `restapi` |
| File | Lower case; named for its subject, not its type | `structure.go`, `names.go` |
| Interface | Declared by the consumer, one to three methods | `deadcode.Source`, `hotspots.History` |
| Sentinel error | Starts with `Err` | `ErrUnknownTool` |
| Test | States the property, not the function | `TestRESTAndMCPReturnTheSameAnswer` |
| Constant with a unit | Carries the unit in its name | `MaxBodyBytes`, `auditArgBytes` |
