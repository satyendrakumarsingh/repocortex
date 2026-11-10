# CLI reference

One entry per command: the question it answers, its flags, one example that was
actually run, and the exit code where it is load-bearing. This is a lookup table.
For "which of these do I need", start at [GUIDE.md](GUIDE.md); for the reasoning
and the longer worked examples, [user-manual.md](user-manual.md).

Where a command's own `--help` argues its design, that argument is not repeated
here. Run `cortex <command> --help` — the help text in this tool is written to
be read.

## Global flags

Available on every command.

| Flag | Effect |
|---|---|
| `--json` | emit machine-readable JSON on stdout |
| `--project <dir>` | project root (default: current directory) |
| `-q`, `--quiet` | suppress all logging |
| `-v`, `--verbose` | verbose logging |

Every query command takes `--json`. That is deliberate: a git hook or a CI check
should not have to speak JSON-RPC to ask the same questions the MCP tools answer.

## The provenance warning

Every index run records the build that wrote it. When a query command opens an
index written by a **different** build, it says so on stderr and then answers
anyway:

```
$ cortex callers Greet
time=… level=WARN msg="This index was written by cortex dev and you are running
9.9.9-test. Resolution rules change between builds, so these results may come from
different rules than this binary would produce. Re-index to refresh."
Greet (function) at main.go:3

KIND   SYMBOL  EVIDENCE   CONFIDENCE
CALLS  main    main.go:5  high (source_code)

1 caller(s).
```

An index is data *plus* the resolution rules that produced it, and those rules
have already changed twice in this project's history in ways that moved real
edges. An index written either side of such a change holds different edges for
identical source, and every query against it succeeds and looks exactly as
trustworthy as a current one. There is no symptom to notice, which is why the
index has to say so itself.

A run with no stamp — one written before the column existed — reports **unknown**
provenance rather than known staleness, because the advice differs: "re-index to
be sure" is not the same as "these results came from other rules".

Neither is an error. `-q` suppresses it along with all other logging. A schema
mismatch is a different thing and *is* an error, because the reader cannot
safely interpret the rows at all:

```
$ cortex callers Redact
error: index schema is v7 but this build of cortex expects v8 — run 'cortex index' to upgrade it
```

## Exit codes

Most commands are 0 on success and 1 on error. Four gate:

| Command | Codes |
|---|---|
| [`verify`](#verify) | 0 `pass`, 0 `advisory`, 1 `fail`, 2 `unverified` |
| [`arch --fail-on`](#arch) | 1 when a violation at or above that severity exists |
| [`bench --fail-on-regression`](#bench) | 1 when any case fails |
| [`docs --check`](#docs) | 1 when the committed documents have drifted |
| [`rename`](#rename-old-new) | 0 would apply, or applied with `verify` passing or advisory; 1 error, `verify` `fail`, or an incomplete rollback; 2 refused (nothing written); 3 applied but not verified complete |

---

## Index lifecycle

### `init`

**Answers:** nothing yet — it creates `.cortex/` and applies the registry
schema. It does not index.

| Flag | Effect |
|---|---|
| `--force` | recreate the index if it already exists |

```
$ cortex init
Initialized RepoCortex for "payfix2"
  Index:          /path/payfix2/.cortex/index.db
  Schema version: 8 (8 migration(s) applied)
  Accuracy mode:  auto

Next: cortex index
```

### `index`

**Answers:** what is in this repository? Walks it, parses every supported file,
and makes the result queryable. Incremental by default — files are compared by
**content hash**, not mtime, so a `git checkout` does not trigger a full
reparse.

| Flag | Effect |
|---|---|
| `--full` | rebuild from scratch. Discards resolved edges; re-run `resolve` |
| `--accuracy fast\|auto\|exact` | override the accuracy mode (full path only) |
| `--workers N` | parse concurrency (default 4). Peak memory scales with this, not with repository size |
| `--digest` | print the registry digest — the determinism gate |

`--workers` and `--accuracy` take effect only on the `--full` path, but are
validated on every path, so a typo fails immediately rather than being silently
discarded:

```
$ cortex index --accuracy nonsense
error: --accuracy "nonsense": want fast, auto or exact
$ echo $?
1
```

A full rebuild publishes atomically: the new revision becomes visible only once
the whole run is written, so an interrupted rebuild leaves the previous revision
in place.

```
$ cortex index --full --digest
  Files:         422 indexed, 54 skipped (476 discovered)
  Symbols:       6259
  References:    29506 (unresolved until M2)
  Imports:       2063
  Digest:        74f82dd64551014cbaad1be87228d2ac720c7ac0ca49ccfd856eb0bfe4a5d54b
  Duration:      7346ms
```

### `resolve`

**Answers:** which definition does each reference point at? Builds the call
graph. Separate from `index` because binding needs the complete symbol table,
which does not exist until every file has been parsed.

| Flag | Effect |
|---|---|
| `--min-confidence high\|medium\|low` | drop bindings weaker than this instead of recording them |
| `--no-scip` | do not import an `index.scip` found in the project root or `.cortex/` |

Resolution itself never produces `exact`; that is reserved for compiler-backed
SCIP data and is enforced in Go, in a SQL `CHECK`, and again at commit time. If
an `index.scip` sits in the project root or `.cortex/`, `resolve` imports it
afterwards (as `scip import` would) and prints a `SCIP:` line with the edges
upgraded, or, when the import fails, the path and the reason. A failed import
does not fail `resolve`. JSON output carries the same under `scip`.

```
$ cortex resolve
  References:  29506
  Resolved:    12062 (40.9%)
  Ambiguous:   2063
  Unresolved:  15381  (calls outside this repository)
  Edges:       11720
    high:    9714
    medium:  133
    low:     2215
  Data access: 476 statement(s), 458 bound to a declared table, 450 edges
  Config:      32 read(s), 2 bound to a declared key, 2 edges
  Duration:    1260ms
```

### `watch`

**Answers:** keeps the index current as you edit. Runs until interrupted.

| Flag | Effect |
|---|---|
| `--debounce <duration>` | quiet period before reacting to a burst of edits (default 300ms) |

Editors emit bursts of events for one save, so changes are debounced and
coalesced. Each file updates in its own transaction, so an agent querying
concurrently always sees a file either entirely before or entirely after a
change.

### `status`

**Answers:** what state is the index in, and how much is in it? All figures are
read from the registry; nothing is estimated. No flags beyond the global ones.

```
$ cortex status
Project:        payfix2
Index:          READY
Schema:         v8
Revision:       f7fd7cc51ad9fd4fde96ba72b24d4f60f6d5b2a5
Files:          10
Symbols:        54
Relationships:  26
Last indexed:   2026-08-18T03:23:11+05:30
Index size:     268.0 KiB
Accuracy mode:  auto
MCP profile:    minimal
Capabilities:   code
```

### `doctor`

**Answers:** is the environment sane, and is the graph structurally sound?

| Flag | Effect |
|---|---|
| `--verify-index` | run the graph invariant gate: dangling edges, orphaned symbols, edges claiming compiler certainty without compiler evidence |
| `--resolution-precision` | adjudicate the heuristic resolver against compiler facts already imported into this index |

`--resolution-precision` needs a run holding both heuristic and compiler edges,
so import a SCIP index first. It reports precision, never recall, and it reports
the coverage the precision was computed over. `doctor`'s exit status is **not**
affected by the measurement — add `--json` to gate on it yourself; the report
lands under `resolution_precision`.

```
$ cortex doctor --verify-index
[ok  ]  go runtime                go1.25.13 darwin/amd64
[ok  ]  sqlite                    3.53.3
[ok  ]  fts5                      ENABLE_FTS5 present
[ok  ]  dangling edges            none
[ok  ]  orphaned symbols          none
[ok  ]  unproven exact edges      none
...
```

### `version`

**Answers:** which build is this? A source build reports `dev`/`none`/`unknown`,
which is correct rather than broken — the real values are injected with
`-ldflags` at release time.

```
$ cortex version
cortex dev (commit none, built unknown, darwin/amd64)
```

---

### `languages`

**Answers:** which languages does this binary actually resolve, and to what
depth? No flags beyond `--verbose`, which lists the parsed-tier grammars too.

The tiers are computed from the query set at run time, not read from a list,
so this output cannot disagree with what the binary does. A language is `typed`
when it has definition, import, receiver and arity queries; `resolved` when it
has definitions and imports; `parsed` when only a grammar ships for it.

```
$ cortex languages
TIER      COUNT  WHAT YOU GET
typed     1      definitions, imports, receiver and arity discrimination — references can reach 'high'
resolved  11     definitions and imports — references bind across files
parsed    194    definitions and search only — no cross-file references claimed
total     206

typed:    [go]
resolved: [c_sharp java javascript kotlin php python ruby rust scala tsx typescript]
```

Read the tiers as a coverage statement before trusting an empty relationship
result: a `parsed`-tier language will never produce a caller, and the absence is
a property of the tier, not of the code.

### `hooks install`, `hooks status`, `hooks uninstall`

**Answers:** keep the index current on every commit, without a daemon.

| Flag | Effect |
|---|---|
| `--binary PATH` | (`install`) the `cortex` the hook should call; default is this executable |

`install` writes a Git `post-commit` hook that re-indexes after each commit, so
the portal and any connected agent describe the commit you just made. The block
is written inside marked delimiters: an existing hook is appended to, never
replaced, and `uninstall` removes exactly the marked block. The hook runs
detached and swallows every failure — a hook that can fail a commit is a hook
people delete.

```
$ cortex hooks status
post-commit: not installed
The index is only as current as the last 'cortex index'.
Install it with: cortex hooks install
```

Prefer `watch` when you want the index to follow edits rather than commits, and
the hook when you want nothing running in the background.

## Compiler-backed accuracy

### `indexers`

**Answers:** which compiler-backed indexers apply here, and can this machine run
them? Two subcommands.

`indexers list` is pure inspection — it changes nothing and installs nothing.

```
$ cortex indexers list
Accuracy mode: auto
Toolchain:     go: go version go1.25.13 darwin/amd64

LANGUAGE  INDEXER  STATUS                 PATH
go        scip-go  ready (found on PATH)  .
```

`indexers install` provisions them. Every download is pinned to an exact version
and verified against a recorded digest; a mismatch refuses rather than
installing, and an indexer with no recorded digest is never fetched at all.

| Flag | Effect |
|---|---|
| `--yes` | install without prompting (still verifies digests) |
| `--out-dir DIR` | write the `.scip` files into `DIR` (created if absent, must be empty) instead of a temporary directory, so a script knows where to find them |
| `--allow-repo-build` | permit running the repository's own build (Java); it executes code from the repo. Refused by default; prefer `accuracy.indexers.runner=docker` |

`indexers pins` prints one tab-separated line per indexer this build may
install, with five fields: name, version, channel, the reference it installs,
and the digest it verifies (`-` where the channel verifies itself, as
`go install` does against the checksum database).

```
$ cortex indexers pins
scip-go	v0.2.7	go-install	github.com/scip-code/scip-go/cmd/scip-go@v0.2.7	-
scip-java	0.12.2	docker	sourcegraph/scip-java@sha256:a05564b9...	sha256:a05564b9...
scip-python	0.6.6	npm	@sourcegraph/scip-python@0.6.6	be8e0a1e...
scip-typescript	0.3.14	npm	@sourcegraph/scip-typescript@0.3.14	92a88678...
```

(Digests abbreviated here.) The output depends on nothing but the binary: no
repository, config or network is read. That makes it a sound CI cache key: the
key changes exactly when a pin changes, which is how the change-safety workflow
caches `scip-go`.

Which indexers may be installed, and whether installation may happen without
prompting, is controlled by your **user** config (`~/.config/cortex/config.yml`)
and never by the repository. A cloned repo must not be able to decide what runs
on your machine.

### `scip import`

**Answers:** takes a `.scip` index that already exists — usually from CI — and
writes its relationships in as `exact`/`compiler` facts. Nothing is ever
downgraded, so importing a stale index cannot weaken the graph, and importing
the same index twice is a no-op.

| Flag | Effect |
|---|---|
| `--path-prefix <dir>` | repo-relative directory the index's documents hang off, when the index records absolute build-container paths |
| `--max-bytes N` | cap the index size in bytes (0 = default, negative = no cap) |

Order matters: index, resolve, then import.

```
$ cortex scip import index.scip
scip-go 0.2.7: 313 document(s), 276 matched
symbols: 4376 matched, 14810 with no counterpart in the index
edges:   15885 new, 0 upgraded from heuristic, 1026 already known
         72729 reference(s) point outside the indexed repository
         3173 reference(s) the indexer attributed to a file rather than a definition were skipped
duration: 2.218s

37 of 313 document(s) matched no indexed file (88% matched).
```

Paths are the usual failure. If the matched-document count is zero, pass
`--path-prefix`. `0 upgraded from heuristic` is expected, not a failure — see
[FAQ.md](FAQ.md#why-does-scip-import-say-0-upgraded-from-heuristic).

---

## Asking the index questions

All five of these answer the same questions as the MCP tools, against the same
index.

### `search`

**Answers:** which files contain this text? Full-text over the index's FTS5
tables. Identifiers are indexed by their parts, so `callback` matches
`handlePaymentCallback`. This searches **text**; to find a definition by name,
use `symbol`.

| Flag | Effect |
|---|---|
| `-n`, `--limit N` | maximum files to report (default 20) |

```
$ cortex search callback -n 5
internal/search/tokenize_test.go
internal/compile/compile_test.go
fixtures/payment-platform/src/main/java/com/pay/controller/CallbackController.java
internal/context/task_test.go
internal/cli/final_test.go

5 file(s) matching "callback".
```

Coverage is indexed files only: binary, generated and oversized files are
recorded in the index but carry no searchable text, and secrets are redacted
before storage.

### `symbol`

**Answers:** where is this defined? Exact, not fuzzy — asking for
`TransactionService` returns that type, not the forty symbols whose names
contain it.

| Flag | Effect |
|---|---|
| `--kind <kind>` | restrict to one kind (class, interface, method, function, …) |
| `-n`, `--limit N` | maximum definitions to report (default 50) |

```
$ cortex symbol Server --kind class
KIND   NAME    LOCATION                   CONFIDENCE
class  Server  internal/mcp/server.go:41  high (source_code)

1 definition(s) named "Server".
```

The confidence shown is the confidence of the **extraction**, so a
compiler-proven definition is distinguishable from a parsed one.

### `references`, `callers`, `callees`

**Answer:** what points at this symbol, what calls it, and what does it call.
`references` is broadest — calls, imports, implementations, extensions and plain
references. `callers` is call edges only. `callees` points away from the symbol.
All three share the same flags.

| Flag | Effect |
|---|---|
| `-n`, `--limit N` | maximum relationships to report (default 100) |
| `--min-confidence exact\|high\|medium\|low` | drop edges below this confidence |
| `--path <substring>` | disambiguate by file path substring |

```
$ cortex callers Redact
Redact (function) at internal/security/redact.go:67

KIND   SYMBOL       EVIDENCE                          CONFIDENCE
CALLS  Bound        internal/mcp/framing.go:153       high (source_code)
CALLS  RedactFile   internal/security/redact.go:122   high (source_code)
...
17 caller(s).
```

**An empty result is never presented as proof of absence.** It reports the
index's measured resolution coverage instead:

```
$ cortex callees profileTools
profileTools (function) at internal/mcp/server.go:129

No callees found.

41% of 29506 references are bound to a definition (15381 point outside this
repository, 2063 were too ambiguous to bind). An empty result is a lower bound,
not proof that nothing uses this symbol.
```

### `context`

**Answers:** what would an agent be handed for this task? This is what
`get_task_context` serves over MCP, printed for a human, and it is the fastest
way to see *why* an agent received what it received.

| Flag | Effect |
|---|---|
| `--max-files N` | override the file budget |
| `--max-lines N` | override the excerpt line budget |
| `--max-symbols N` | override the symbol budget |
| `--show-excluded` | list everything considered and left out, with the reason |

```
$ cortex context "Add secret redaction to MCP responses"
Task: Add secret redaction to MCP responses
Understood as: Concepts: mcp, redaction, responses, secret, add; action: add.
Anchored on 20 symbols named by the task (MCPProfile, MCPMinimal, …); expanded
to 1462 reachable symbols in the call graph

KIND      NAME        LOCATION                        WHY
class     MCPProfile  internal/config/config.go:90    named by the task (matches "mcp"); text also matches …
...
```

Every item carries a non-empty reason it earned its tokens, and `--show-excluded`
prints the rejections with theirs.

---

## Structural surfaces

### `api`

**Answers:** what HTTP endpoints does this service expose, and what handles
each? No flags beyond the global ones.

Routes are discovered from the framework's own spelling — a Spring annotation, a
Go router registration, a FastAPI or Flask decorator, a Django `path()` in a
urlconf, an Express member call, a NestJS controller decorator — never from
strings that merely look like paths. Django routes are reported at `medium`
confidence, since the `include()` mount prefix is not visible from the file.
**Not covered:** OpenAPI documents, GraphQL schemas, gRPC service definitions,
API-gateway configuration.

```
$ cortex api
METHOD & PATH           HANDLER         DECLARED AT
POST /payment/callback  handleCallback  …/controller/CallbackController.java:21

1 endpoint(s). Discovery covers spring, go-http, go-router, fastapi, flask, django, express, nest.
5 further route(s) are declared in test files and are not counted above — they
are fixtures, not this service's API.
```

### `db`, `db table`

**Answer:** which tables does this repository declare or query, and what reads
or writes each. `READ`, `WRITE`, `READ_WRITE`, `DDL` and `UNKNOWN` are kept
distinct — "what touches this table" is a mildly useful question, "what *writes*
it" is the one asked before changing a column. Neither takes flags beyond the
global ones.

Tables nothing here declares are still reported: that is the normal shape of a
service reading a database another service owns.

```
$ cortex db table transactions
transactions (declared in this repository)

WRITTEN BY (1)
  save (…/TransactionRepository.java)      …TransactionRepository.java:20  high (UPDATE (literal SQL))

READ BY (1)
  findById (…/TransactionRepository.java)  …TransactionRepository.java:14  high (SELECT (literal SQL))
```

### `config`

**Answers:** every configuration key the repository declares or reads, and — the
two findings worth acting on — the mismatches between them. A key read by code
that nothing declares is how a service fails at start-up; a key declared and
never read is either dead configuration or a lookup this scan cannot see.

| Flag | Effect |
|---|---|
| `--all` | list every key, not only the mismatches |

**Values are never read.** Not masked, not truncated — never read at all. Keys
whose *name* indicates a credential are labelled sensitive, which requires
reading nothing. Declarations come from `.env`, YAML, JSON, TOML and
`.properties`; Kubernetes, Helm, Terraform and CI definitions are not read.

```
$ cortex config
READ BUT NOT DECLARED (16) — a service that needs these and finds nothing fails at start-up
  API_KEY                5 reader(s)  sensitive (by name)
  DATABASE_URL           5 reader(s)
  ...
179 key(s) total: 161 declared and never read, 4 labelled sensitive by name.
Pass --all for the full inventory.
```

### `deps`

**Answers:** what does this project declare it depends on, and does anything
import it? Aliased as `dependencies`.

| Flag | Effect |
|---|---|
| `--all` | list every dependency, not only the unused ones |

Manifests, not lock files — `go.mod`, `package.json`, `pom.xml`, `build.gradle`,
`requirements.txt`, `pyproject.toml`, `Cargo.toml`. **No vulnerability data**:
versions are reported accurately so a real scanner can consume them, but a
scanner shipping an embedded advisory list is stale the day it is built. An
unused dependency is a prompt, not a verdict.

```
$ cortex deps
DECLARED BUT NOT IMPORTED (1)
  github.com/never/used  v0.1.0  runtime  go.mod:7

  Check before removing: a package can be loaded by reflection, required by a
  build plugin, or imported under a name that differs from its distribution name.

2 dependency(ies) declared. Pass --all for the full list.
```

### `arch`

**Answers:** does the code match the architecture you declared? Compares the
dependency **graph** against components and rules in `.cortex.yml`. Components
are declared, never inferred.

| Flag | Effect |
|---|---|
| `--crossings` | list every component-to-component dependency, not only violations |
| `--fail-on high\|medium\|low` | **exit 1** when a violation of this severity or worse exists |
| `--min-confidence exact\|high\|medium\|low` | ignore relationships below this confidence |

```
$ cortex arch --fail-on high
VIOLATIONS (113)
  SEVERITY  RULE                           CROSSING                              CONFIDENCE
  high      cli-goes-through-the-registry  cli -> store (symbolName -> symbols)  high
  ...
error: 113 architecture violation(s), worst severity "high" (--fail-on high)
$ echo $?
1
```

The report always states how many symbols belong to no declared component and
were therefore not checked. The confidence column is the confidence of the
*edge*, not of the rule: `--min-confidence exact` narrows the same run above
from 113 violations to 42.

### `arch adr`

**Answers:** what decisions are on record, and why? Reads `docs/adr/`,
`docs/decisions/` and the other conventional locations, in both the Nygard and
MADR formats. Takes an optional number or substring; no flags beyond the global
ones.

RepoCortex does not check whether an ADR is still **true**. A record saying the
system uses Postgres is evidence of a decision, not of the current state — the
graph is where you check the current state.

---

### `infra`

**Answers:** which service does this code run in, and what restarts when a
configuration key changes? Read from Dockerfiles, Compose files, Kubernetes
manifests, Helm charts and Terraform.

| Flag | Effect |
|---|---|
| `--for PATH` | which deployment unit does this repo-relative file run in |
| `--key NAME` | what does changing this configuration key name restart |
| `--keys` | list every configuration key name each unit sets |
| `--format F` | restrict to `dockerfile`, `compose`, `kubernetes`, `helm` or `terraform` |
| `--disk` | scan the working tree instead of the index; works before `cortex index` |

**Values are never printed.** These files hold credentials, and the guarantee is
structural rather than a filter: the extractor has no field a value fits in, and
a Kubernetes `Secret` is read as the key names it declares. What you get is
"`DB_PASSWORD`, from Secret `db-creds`", which is the half you can act on.

Manifests are stored by `cortex index`, so a service is a node in the same
graph as the code and a key name binds to the code that reads it; `resolve`
joins a service to the endpoints built from its build context. `--disk` reads
the tree directly instead, and the output says which of the two it is reading,
because they can disagree.

What could not be read is reported: a manifest that fails to parse is listed
with the reason, and Helm templates are reported as unrendered rather than
guessed at. Terraform is read as block headers only, never attribute values.
**Not read:** CloudFormation, systemd units, CI definitions.

```
$ cortex infra
The index holds no deployment manifests. If you expected some, re-run 'cortex index';
to check the working tree directly, run 'cortex infra --disk'.

Read: Dockerfile, docker-compose.yml, Kubernetes manifests, Chart.yaml and values.yaml,
and .tf files. CloudFormation, systemd units and CI definitions are not read — that is a
coverage limit, not a finding about your deployment.
```

## Change-safety and context tools

These run against the committed index. Each takes the global `--json`.

### `events`, `events topic`

**Answers:** which topics, queues and exchanges exist, and who publishes to or
consumes from them?

`cortex events` lists them across Kafka, RabbitMQ/AMQP, SQS, SNS and Google
Cloud Pub/Sub. `cortex events topic <name>` lists the functions that publish
to or consume from one. Event topology is not an MCP tool.

### `missed`

**Answers:** where is this index blind, or where is the code hiding risk?

| Flag | Effect |
|---|---|
| `--kind <kind>` | one of `unresolved_call`, `untested_critical`, `unmapped_endpoint`, `orphan_table`, `missing_config`, `unparsed_file` |
| `--limit <n>` | blindspots reported per category (default 50) |
| `--fail-on <severity>` | exit 1 when a blindspot of this severity (`high`, `medium`, `low`) or worse exists |

MCP equivalent: `detect_blindspots`.

### `plan-change <symbol>`

**Answers:** what is the ordered, checkable sequence of steps to change this
symbol safely? Output carries the target, direct callers, risk, covering tests
and pre/post-conditions per step.

| Flag | Effect |
|---|---|
| `--min-confidence <tier>` | lowest edge confidence to walk: `exact`, `high`, `medium` (default), `low` |

MCP equivalent: `plan_change`.

### `rename <old> <new>`

**Answers:** can this symbol be renamed at its definition and at every
reference RepoCortex resolved, and is every one of those sites safe to edit?
A dry run by default: it lists every site with its confidence and says whether
the rename could be applied. Nothing is written without `--apply`.

`--apply` writes only if **every** site passes:

1. its confidence is at or above the floor: `exact` (a compiler-proven edge) by
   default, or `high` with `--floor high`;
2. the file is byte-for-byte what was indexed (otherwise re-index);
3. the old name occurs on the site's line exactly once, or the recorded column
   lands on it exactly;
4. the file is inside the project root, and is a regular, writable file (a
   read-only file is the owner saying "do not touch", and is refused);
5. every reference was fetched, and the new name is not already defined in a
   file being edited.

If any site fails, nothing is written, every failing site is listed, and the
exit code is 2. Files are written atomically (write, then rename), contained to
the project root, with the hash re-checked immediately before each replace. If
a later write fails, the files already written are restored. Only one edit runs
per project root at a time: a `.cortex-edit.lock` file at the project root
exists only while a commit is running, and a second `rename --apply` that finds
it is refused. A lock left behind by a crashed process is **never** stolen; the
error names the lock's holder and its path, and once you have confirmed no
cortex process is running, delete it by hand and retry. It exists only
during a commit, so there is no need to ignore it in `.gitignore`, though it can
appear briefly in `git status`.

After applying, a task contract for the rename is written to
`.cortex/rename-contract.json` and [`verify`](#verify) runs on the changed
files. **Verify reads the pre-rename index**: it checks the change against what
was indexed before the edit. Run `cortex index` afterwards to refresh it.

| Flag | Effect |
|---|---|
| `--apply` | write the edits (default is a dry run) |
| `--floor exact\|high` | weakest confidence an edit may rest on (default `exact`). `high` accepts an explicit import plus an unambiguous receiver type, measured 86.8-96.1% precise, and prints a warning saying so before applying |
| `--path <substr>` | substring of the defining file's path, when several definitions share the name |
| `--max-sites N` | refuse renames with more references than this (default 2000) |
| `--contract-out FILE` | where to write the rename's task contract (default `.cortex/rename-contract.json`) |
| global `--json` | machine-readable report, including `leftovers` |

| Exit | Meaning |
|---|---|
| 0 | dry run: would apply. Apply: written, and `verify` passed or advised, and no occurrence of the old name remains |
| 1 | error, `verify` returned `fail`, or a rollback after a failed write was incomplete |
| 2 | refused: nothing was written (a site below the floor, an unchanged-file or ambiguity failure, a locked project root, a read-only file) |
| 3 | applied, but **not verified complete**: `verify` was `unverified`, or `LEFTOVER OCCURRENCES` of the old name remain in indexed files |

**Limits.** RepoCortex never compiles your code. A reference it could not bind,
and interface implementations or overrides that have no edge to the symbol, are
not listed and **not renamed**; comments and strings that mention the old name
are listed as leftovers but left alone. Every run prints this as a
`NOT COMPLETE` note. Search for the old name, then build and test.

A dry run on a copy of `fixtures/payment-platform` (Java, so no compiler index
exists and every site is `high`, below the default `exact` floor):

```
$ cortex rename updateStatus applyStatus
Rename updateStatus -> applyStatus  (src/main/java/com/pay/service/TransactionService.java:updateStatus, src/main/java/com/pay/service/TransactionService.java:18)
REFUSED: nothing was written. Every site must be exact or better, unambiguous on its line, and unchanged since indexing.
  High sites need a compiler-backed index to become exact ('cortex indexers install', then 'cortex scip import'), or an explicit --floor high (measured 86.8-96.1% precise).

SITES
  high  src/main/java/com/pay/controller/CallbackController.java:23  CALLS
  high  src/main/java/com/pay/service/TransactionService.java:18     DEFINITION

REFUSED SITES
  high    src/main/java/com/pay/controller/CallbackController.java:23  confidence high is below exact
  high    src/main/java/com/pay/service/TransactionService.java:18  confidence high is below exact

NOT COMPLETE: RepoCortex renames the definition and the references it resolved, nothing else, ...
$ echo $?
2
```

(The `NOT COMPLETE` note is truncated here; the tool prints it in full.) With
the explicit opt-in to `high`, on the same copy:

```
$ cortex rename updateStatus applyStatus --floor high --apply
WARNING --floor high: `high` bindings measure 86.8-96.1% precision against compiler indexes (docs/accuracy.md); at 90%, a rename touching thirty sites gets three wrong. Read every high site below, then build and test.
Rename updateStatus -> applyStatus  (src/main/java/com/pay/service/TransactionService.java:updateStatus, src/main/java/com/pay/service/TransactionService.java:18)
APPLIED to 2 file(s), but NOT VERIFIED COMPLETE: the old name is still in the files listed under LEFTOVER OCCURRENCES.

SITES
  high  src/main/java/com/pay/controller/CallbackController.java:23  CALLS
  high  src/main/java/com/pay/service/TransactionService.java:18     DEFINITION

LEFTOVER OCCURRENCES (2 line(s) still name updateStatus; showing 2)
  References RepoCortex could not bind, or comments and strings. Nothing here was changed; review each by hand.
  src/test/java/com/pay/controller/CallbackControllerTest.java:8  verify(transactionService).updateStatus("txn-1", "COMPLETED");
  src/test/java/com/pay/service/TransactionServiceTest.java:7  service.updateStatus("txn-1", "COMPLETED");

VERIFY: PASS  (contract: /tmp/rn/.cortex/rename-contract.json)
  [UNKNOWN] The repository declares no architecture, so there was no rule for this change to break. ...
  [UNKNOWN] The contract names no covering test, so for the 1 changed scope symbol(s) there is nothing to check coverage against. ...
  Checked against the pre-rename index; run 'cortex index' to refresh it.

NOT COMPLETE: RepoCortex renames the definition and the references it resolved, nothing else, ...
$ echo $?
3
```

Here the two test files call the method through a mock and a field that
RepoCortex did not bind, so the old name survives; the exit code is 3, never 0,
until you handle them by hand. Verify passing does not make a rename complete.

MCP equivalent: none. The MCP server only plans (`plan_rename`, `plan_change`); it never edits.

### `prune <file>`

**Answers:** what is this file's interface, without its implementation? Bodies
are replaced by a placeholder; signatures, types and docstrings stay.

| Flag | Effect |
|---|---|
| `--no-docstrings` | strip docstrings as well as bodies |
| `--placeholder <text>` | text left in stripped bodies (default `/* ... */`) |

MCP equivalent: `get_file_outline`.

### `diff-minimize [patch-file]`

**Answers:** what does this diff change, in fewer tokens? Reads a patch file or
stdin, strips redundant headers, names enclosing symbols and compresses long
unchanged runs.

```
git diff HEAD~1 | cortex diff-minimize
```

MCP equivalent: `minimize_diff`.

## The loop: compile → work → verify

### `compile`

**Answers:** what brief should an agent have been given for this one-line ask?
Scope, non-scope, constraints, blast radius, tests, acceptance, and what it does
not know.

| Flag | Effect |
|---|---|
| `--max-scope N` | bound the scope list (0 = default) |

Everything is derived, nothing is invented: every constraint and risk carries the
file or symbol it came from, and a constraint with no evidence is dropped rather
than emitted. No model, no network, no clock — the same intent against the same
index produces a byte-identical contract.

```
$ cortex compile "Add secret redaction to MCP responses"
# Task Contract

## Intent
Add secret redaction to MCP responses

## Interpretation
Concepts: mcp, redaction, responses, secret, add; action: add. …

## Scope
- `MCPProfile` in `internal/config/config.go` (class) — named by the task …

## Out of scope
- `Declarations` in `internal/analyze/config/config.go` — relevant (1.79) but
  dropped to stay inside the ~8000-token budget
…
```

Pipe `--json` into `verify`.

### `verify`

**Answers:** did the change honour the contract? Four questions — scope,
architecture rules, missed callers, test coverage — and it refuses to ask a
fifth. It is not a linter, a test runner or a reviewer.

| Flag | Effect |
|---|---|
| `--contract <path>` | contract from `compile --json` (**required**) |
| `--changed <a,b,c>` | comma-separated changed file paths |
| `--changed-from <file>` | file holding one changed path per line (`-` for stdin) |
| `--fail-on-leads` | exit non-zero when only leads were found |
| `--min-confidence exact\|high\|medium\|low` | raise the contract's confidence floor |

One of `--changed` or `--changed-from` is required. `verify` does not run git:
the changed set is an **input**, because the situation that matters is an agent
asking "is what I have in my working tree acceptable" before it writes anything.

**The exit codes are the command.**

| Code | Disposition | Meaning |
|---|---|---|
| 0 | `pass` | every question was asked, nothing was found |
| 0 | `advisory` | findings exist, all of them leads (`--fail-on-leads` makes this 1) |
| 1 | `fail` | at least one finding is a FACT |
| 2 | `unverified` | there was nothing to check against |

A finding is a FACT when the edge behind it is compiler-proven, and a LEAD when
it rests on a heuristic edge — because the edge itself may be wrong. Presenting
the two identically makes the report either alarmist or ignorable, and both end
with it switched off.

`unverified` is deliberately not `pass`. "We looked and found nothing" and
"there was nothing to look at" must not render the same, or the gate stops
gating on the day contract generation breaks and nobody notices. All four,
observed:

```
$ cortex verify --contract contract.json --changed internal/cli/opendb.go
VERIFY: FAIL
[FACT] internal/cli/opendb.go violates architecture rule cli-goes-through-the-registry:
       openReadOnly (cli) depends on CheckVersion (store).
  evidence: internal/cli/opendb.go:48 — openReadOnly REFERENCES CheckVersion, exact
            confidence; rule cli-goes-through-the-registry, verdict forbidden
  scip: scip-go 0.2.7
$ echo $?
1

$ cortex verify --contract contract.json --changed src/main/java/com/pay/service/TransactionService.java
VERIFY: ADVISORY
[LEAD] …/TransactionService.java violates architecture rule services-do-not-touch-the-database:
       findTransaction (service) calls findById (repository).
  evidence: …/TransactionService.java:26 — findTransaction CALLS findById, high
            confidence; rule services-do-not-touch-the-database, verdict forbidden
  receiver type matches
$ echo $?
0
$ cortex verify --contract contract.json --changed src/…/TransactionService.java --fail-on-leads
$ echo $?
1

$ cortex verify --contract contract.json --changed src/main/java/com/pay/model/Transaction.java
VERIFY: PASS
No findings.
$ echo $?
0

$ cortex verify --contract hollow.json --changed README.md
VERIFY: UNVERIFIED
No findings.
$ echo $?
2
```

Every report also prints what it could **not** check, under `NOT CHECKED` and
`UNKNOWN`, rather than presenting a narrow clean result as a broad one.

---

## Reporting and measurement

### `diff`

**Answers:** what changed about the code's **facts** between two indexed runs —
symbols added, removed, moved or orphaned; relationships added, removed or
re-graded; endpoints whose handler changed; tables that gained or lost a reader
or a writer.

| Flag | Effect |
|---|---|
| `--from <ref>` | base (older) side: a run id, or a commit SHA recorded on an indexed run |
| `--to <ref>` | head (newer) side (default: the committed run) |
| `--from-index <path>` | read the base side from this index database |
| `--to-index <path>` | read the head side from this index database |
| `--max-per-section N` | cap the changes reported per section (0 = default) |
| `--max-evidence-sites N` | cap the evidence sites carried per edge fact (0 = default) |

Nothing here invokes git. A ref matching no indexed run fails and says so — it
never falls back to whichever run happens to be committed. And committing a run
retires the one before it, so **one index database holds one committed run**;
comparing across commits normally means two index files, which is what
`--from-index`/`--to-index` are for.

```
$ cortex diff --from-index /tmp/paybase/index.db --from 2 --to 3
diff 8e6705118c97…..8e6705118c97… (runs 2..3)
  base: status=ready accuracy=auto digest=3f5cf0b410ea
  head: status=ready accuracy=auto digest=03dbf1aa57cd

SYMBOLS
  moved    …/TransactionService.java:TransactionService (class)  line 6 -> 6 (+0)
  added    …/TransactionService.java:refund (method)  line 29

EDGES
  added    …:refund (method) -[CALLS]-> …/TransactionRepository.java:findById (method)
           high/source_code at …/TransactionService.java:30:24
  added    …:refund (method) -[CALLS]-> …/TransactionRepository.java:save (method)
           high/source_code at …/TransactionService.java:30:8

COULD NOT COMPARE
  - 1 files did not parse cleanly in one of the runs; changes inside them are
    marked uncertain
```

The report always ends with what it could not compare. A degraded run, a file
that did not parse, or two runs indexed in different accuracy modes all mean an
absence may be an indexing gap rather than a deletion.

### `docs`

**Answers:** generates `ARCHITECTURE.md` and `CAPABILITIES.md` from the indexed
graph, with Mermaid diagrams. Every claim cites the file and line it came from;
where the graph is silent, the document says so instead of guessing.

| Flag | Effect |
|---|---|
| `--out <dir>` | write the two files there (default: stdout) |
| `--check` | regenerate in memory and compare; **exit 1** on drift |
| `--max-components N` / `--max-depth N` / `--max-endpoints N` | caps (0 = default) |

Output is deterministic: the same index regenerates byte-identical files, so
these can be committed and reviewed like any other artifact. `--check` is the
useful one in CI.

```
$ cortex docs --out /tmp/paydocs
wrote /tmp/paydocs/ARCHITECTURE.md
wrote /tmp/paydocs/CAPABILITIES.md

$ cortex docs --check --out /tmp/paydocs
documentation is current (2 file(s) checked).
$ echo $?
0
```

### `bench`

**Answers:** did the context engine retrieve the right code? Runs hand-written
cases through the engine and scores the pack it actually produces.

| Flag | Effect |
|---|---|
| `--fail-on-regression` | **exit 1** when any case fails, for CI |

A case names a task, the code a correct answer must contain (`expect`), and the
code it must not (`forbid`). The second half is what makes the number mean
something — recall alone is trivially gamed by returning the whole repository.

```
$ cortex bench fixtures/bench/payment.yml
CASE                              RESULT  RECALL  TOKENS  MISSING
change-transaction-status-column  pass    100%    1567    —
idempotent-callback               pass    100%    1368    —
notification-failure-handling     pass    100%    1388    —

3/3 passed, mean recall 100%.
```

**What this does not measure:** whether an agent completed the task. Only whether
the context contained what a human said it should.

### `trace`

**Answers:** which indexed code did production actually reach? Takes an exported
OpenTelemetry or Jaeger file — a **file**, not a live collector — and lines it up
against the graph. No flags beyond the global ones.

Correlation is heuristic and every match says how it was made: a `code.function`
attribute names the symbol outright, an `http.route` matching an endpoint is
strong, a span name matching a symbol is weak and is only tried when nothing
better exists and the name is unique.

```
$ cortex trace spans.json
otlp-json: 3 span(s) across 1 trace(s), service(s): pay

EXERCISED (3)
  ENTITY                  KIND      CALLS  ERRORS  P95   MATCHED BY
  POST /payment/callback  endpoint  1      0       40ms  route
  transactions            table     1      0       15ms  table
  updateStatus            method    1      0       25ms  code-attribute
```

Spans are **not persisted**. Runtime data ages out in hours while an index
describes a commit, and storing them together would produce a graph whose edges
are true at different times.

---

### `impact`

**Answers:** what does a git range put at risk? Reads the files the range
changed, finds the symbols defined in them, and walks *inward* to everything
that depends on each.

| Flag | Effect |
|---|---|
| `--base REV` | the revision to compare from (required) |
| `--head REV` | the revision to compare to (default `HEAD`) |
| `--depth N` | hops to follow from each changed symbol |
| `--min-confidence` | floor for the walk (default `medium`) |
| `--max-seeds N` | maximum changed symbols to analyse |
| `--limit N` | rows per section |

The direction is the point: a change threatens the code that depends on it,
not the code it calls, so this walks callers. Renamed and deleted files are
seeded under the name the index has for them, since those are the two changes
most likely to break a caller and the ones a naive diff would report as empty.

Nothing here checks out or re-indexes. The range is read from the repository
and the symbols from the committed run, so the index may describe neither end
of the range. That case is reported as **STALE INDEX** at the top rather than
answered quietly, because every symbol in a stale report is about other code:

```
$ cortex impact --base HEAD~6 --head HEAD
Blast radius of HEAD~6..HEAD

UNVERIFIED PROVENANCE

  STALE INDEX: this index was built at commit c91cf807, which is neither end of the
  analysed range HEAD~6..HEAD. … Re-run `cortex index` at one end of the range and ask again.

13 files changed; 192 changed symbols found in the index, 192 analysed.
144 symbols at risk through recorded references, plus 0 reachable only through dispatch.
Riskiest change: internal/resolve/infer/language.go:For — risk moderate (score 59, certainty low).
  This is the MAXIMUM over the changed symbols, never a total: two risks of 40 do not make an 80.
```

The result is a lower bound, always: files the index holds no symbols for are
named rather than counted, and an empty result is "nothing found", never
"nothing breaks". `--json` is the form a CI gate consumes.

### `who`

**Answers:** who has context on the code a change to this symbol can reach?
Computes the blast radius, then asks git history who has touched the files it
lands in, ranked by how much of the radius they have touched.

| Flag | Effect |
|---|---|
| `--depth N` | hops to follow out from the symbol |
| `--commits N` | how far back from `HEAD` to read history |
| `--limit N` | maximum identities to list |
| `--min-confidence` | floor for the blast radius |

This is not CODEOWNERS. CODEOWNERS routes by the directory a file sits in,
which answers "who administers this path" and not "who has seen the code this
change can break": a two-line edit to a shared decoder reaches thirty call sites
in six packages, and the person listed for the decoder's directory may never
have touched any of them.

What it reports is context, not ownership, and every limit is printed with the
answer: git records that somebody changed a file, not that they understand it,
own it, or still work here; attribution is per file; the history window is
bounded; an identity is an address. A name that matches more than one
definition is refused rather than guessed:

```
$ cortex who Execute
"Execute" matches 2 definitions. Re-ask with a fully-qualified name:

  internal/cli/root.go:Execute             internal/cli/root.go:192
  internal/indexer/orchestrate.go:Execute  internal/indexer/orchestrate.go:477
```

### `report sarif`, `report pr`

**Answers:** what should a CI run show the reviewer? Turns the JSON that
`cortex --json arch|missed|impact` already printed into two CI artefacts. It
reads those files, not the index, so it needs no database and runs anywhere the
JSON can be carried. At least one input is required; an input you do not pass is
reported as "not run", and so is `arch` output from a project with no declared
architecture. Each input is shape-checked: passing `missed` output to `--arch`
is an error naming the file.

`report sarif` writes one SARIF 2.1.0 run for code scanning.

| Flag | Effect |
|---|---|
| `--arch FILE` | output of `cortex --json arch` |
| `--missed FILE` | output of `cortex --json missed` |
| `--path-prefix P` | prefix joined onto every file path, for an index built from a subdirectory |
| `--out FILE` | output file, or `-` for stdout (default `-`) |

Findings with no file path cannot be shown by code scanning. They are left out
of `results` and counted in a run-level `toolExecutionNotification`, so they are
never silently lost. File URIs are percent-encoded.

`report pr` writes the Markdown body of a pull-request comment.

| Flag | Effect |
|---|---|
| `--impact FILE` | output of `cortex --json impact` |
| `--arch FILE` | output of `cortex --json arch` |
| `--missed FILE` | output of `cortex --json missed` |
| `--max-rows N` | rows per table before the rest are counted (default 20) |
| `--out FILE` | output file, or `-` for stdout (default `-`) |

When `--impact` is given, the comment also carries a "How much of the blast
radius is proven" table: the share of `impact`'s reached symbols at each path
confidence (`exact (compiler-proven)`, `high`, `medium`; `low` only when
present), rounded to whole percent, with a one-line form in the summary row.
Symbols reached only through interface dispatch are a different claim and are
not counted; a confidence value that is not one of the four tiers is bucketed
as `unlabelled` and never echoed. When no symbol is `exact`, the comment says
that no part of the blast radius is compiler-proven and how to change that.
With no `--impact`, or an `impact` that reached nothing, the table is absent
and the summary row says "not run".

The first line is the sticky marker `<!-- cortex:pr-report -->`, which a
workflow searches for to edit its own comment instead of adding another. Text
that came from the repository (symbol names, paths, messages) is escaped, every
table is bounded with an "...and N more" line, the whole body is capped at
60000 characters with a truncation note, and a stale-index banner leads the
report when `impact` says the index does not describe the range.

```
$ cortex --json missed > missed.json
$ cortex report pr --missed missed.json
<!-- cortex:pr-report -->
## RepoCortex change-safety report

| Check | Result |
|---|---|
| Blast radius | not run |
| Architecture | not run |
| Blindspots | 38 (1 high) |

### Blindspots

| Severity | Kind | Target | Location |
|---|---|---|---|
| medium | unresolved\_call | request | src/main/java/com/pay/controller/CallbackController.java:23 |
| ... | ... | ... | ... |

_…and 18 more._
```

Neither subcommand sets a failing exit code for findings; gating stays with
`arch --fail-on` and `missed --fail-on`, so a workflow can publish first and
enforce afterwards. See the [CI guide](USER_GUIDE.md#7-cicd-integration-github-actions-change-safety-gate).

### `export`

**Answers:** the committed symbol graph, in a format another tool reads.

| Flag | Effect |
|---|---|
| `--format F` | `csv`, `dot`, `graphml`, `json` or `svg` |
| `--out PATH` | write to a file instead of stdout; written beside its destination and renamed into place |
| `--min-confidence` | floor for exported facts (default `low`, which keeps everything) |

```bash
cortex export --format dot | dot -Tsvg -o graph.svg
cortex export --format graphml --out graph.graphml     # Gephi, yEd, Cytoscape
cortex export --format csv --out edges.csv             # spreadsheets, pandas
```

Every format carries edge direction, and every format that can hold a
confidence does. A format that cannot hold something says so — on stderr, so a
pipeline reading stdout is unaffected:

```
$ cortex export --format csv | head -3
index_revision: 1  built at commit c91cf80
note: 675 symbol(s) share a fully-qualified name with another; their node ids carry a '@<path>' suffix …
note: csv export is lossy — edge list only: nodes with no edges are omitted, node kind is not carried, …
```

The CSV writer prefixes a cell starting with `=`, `+`, `-` or `@` with a quote,
so an exported symbol name cannot become a spreadsheet formula.

### `system`

**Answers:** what crosses between repositories? Joins several members' indexes
into one read-only view and reports outbound HTTP calls answered by endpoints
declared elsewhere, and tables more than one member touches.

| Flag | Effect |
|---|---|
| `--member name=path/to/index.db` | a member, repeatable |
| `--members FILE` | the member list as YAML (`members: [{name, index}]`); paths resolve relative to the file |
| `--stale-after D` | age at which a member index is called out as old |
| `--max-edges`, `--max-shared-tables`, `--max-unjoined` | caps on each section |

Nothing is merged or migrated: each member keeps its own index, runs and
digest. Past two repositories, keep the list in a file — the estate is a
property of the team, not of an invocation, and retyping eight flags is how a
member silently goes missing. It is deliberately not `.cortex.yml`, which is
per-repository configuration.

```yaml
members:
  - name: web
    index: ../web/.cortex/index.db
  - name: orders
    index: ../orders/.cortex/index.db
```

Confidence is weaker here than anywhere else in the tool, and the output says
so: matching `/users/{id}` against `/users/:id` is a guess no compiler proved,
so nothing this command produces is `exact`, and an edge's confidence is the
minimum over every leg of the path including the match. A call two members
could both answer is refused rather than assigned. The report always begins
with the members, including any whose index is absent or stale, and ends with
what it could not join. Exit status is 2 when a member could not be read, so
the report is a floor rather than a total.

A single member is refused: one repository is already answered by `api` and
`db`.

### `licenses`

**Answers:** regenerate `THIRD_PARTY.md` from the module graph, or verify it.

| Flag | Effect |
|---|---|
| `--check` | verify the committed file is current; non-zero exit if not — the form CI runs |

```
$ cortex licenses --check
THIRD_PARTY.md is current (147 modules)
```

## Serving

### `serve`

**Answers:** runs an MCP server on stdio, exposing the index to a coding agent.

| Flag | Effect |
|---|---|
| `--list-tools` | print the exposed tools and exit |

```bash
claude mcp add cortex -- cortex --project /path/to/repo serve
```

The tool set is governed by `mcp.profile` in `.cortex.yml` — `minimal`
(default), `standard`, `full`. The default is deliberately small: every tool's
schema is injected into the agent's context on every request, so a large surface
spends the very context this tool exists to save.

```
$ cortex serve --list-tools
MCP profile "minimal" exposes 7 tool(s):
  get_project_overview
  search_code
  find_symbol
  get_symbol
  find_references
  find_callers
  get_task_context
```

Responses are bounded and never contain whole files. Repository content is
labelled as untrusted evidence, because indexed source may have been written by
someone other than you.

### `ui`

**Answers:** a local, read-only web portal over the index — twenty screens
grouped by what you want to know. Index health stands alone above the
groups; then five groups, named the way a reader would ask the question:

- **Understand the code** — Map of the code (graph explorer), Find a
  symbol, Shape of the code.
- **Before you change something** — What could break (blast radius for one
  symbol), Task brief (the compiled task contract), Check a change (the
  verify report against that contract), Who knows this code, What changed.
- **How it runs** — APIs and what they touch (the cross-stack view from a
  frontend component through the route, handler and tables it reaches),
  Database tables, Settings and secrets, What actually ran, Across
  repositories.
- **Quality** — Test coverage, Architecture rules (governance),
  Dependencies, How accurate is this index.
- **For your AI agent** — Connect an agent, Retrieval quality.

Every screen opens with its answer in one plain sentence. Evidence and the
things the view can't see fold below that sentence, closed by default, and
`?open=1` on the URL unfolds them all. Confidence reads as a word (proven,
very likely, uncertain, a guess) with the tier id kept on every field and
flag underneath. A "What do these words mean?" button opens a glossary for
the terms that remain.

| Flag | Effect |
|---|---|
| `--port N` | port to listen on; `0` picks a free one (default `7777`) |
| `--host ADDR` | address to bind; anything but loopback exposes your codebase's structure and the command says so |
| `--max-nodes N` | symbol nodes per graph response; what is excluded is stated, never dropped (default `1500`) |
| `--allow-refresh` | add the one write route, a POST that rebuilds the index; off by default |
| `--print-url` | print the URL that would be served and exit |

The portal opens the index read-only and serves only GET routes. Without
`--allow-refresh` no route that writes is registered at all — read-only is
enforced by the absence of a route, not by a check inside a handler. With it,
a refresh must carry a header a cross-site form cannot set, and a foreign
`Origin` is refused even so.

It binds to `127.0.0.1` and refuses requests whose `Host` is not a loopback
address, because a page on the public internet can otherwise point your own
browser at `localhost:7777` and read the structure of your code. Everything is
served from the binary: no npm, no build step, no CDN, and a Content-Security-
Policy that forbids loading from any other origin, so it works offline.

Confidence is the visual language of every screen: a compiler-proven edge is
drawn solid with a tick, a `medium` edge dashed, and `low` edges are off by
default with the count of what that hides stated in every response.

```
$ cortex ui --print-url
http://127.0.0.1:7777/
```

### `install`

**Answers:** register RepoCortex's MCP server with every agent client detected on
this machine, preserving everything else in each client's config file.

| Flag | Effect |
|---|---|
| `--dry-run` | report what would be written without writing it |

Covers Claude Code, Claude Desktop, Cursor and Windsurf. A client whose config
file does not exist is skipped rather than created, since creating one would
guess at a layout the client may not accept. For Zed, which uses a different
config key, use `agent` and place the output yourself.

```
$ cortex install --dry-run
would write Claude Code      ~/.claude.json
would write Claude Desktop   ~/Library/Application Support/Claude/claude_desktop_config.json
skip  Cursor           (no config at ~/.cursor/mcp.json)
skip  Windsurf         (no config at ~/.codeium/windsurf/mcp_config.json)

Restart your agent, then ask it to use RepoCortex.
```

### `agent`

**Answers:** the two things that make RepoCortex usable by an agent — the MCP
registration, and the guidance that tells the agent *when* to reach for it.

| Flag | Effect |
|---|---|
| `--guidance` | print only the guidance block, to append to `CLAUDE.md` or `AGENTS.md` |
| `--check` | report which parts of the integration are wired up; reads only |
| `--write PATH` | write the guidance block to a new file; refuses to replace one unless `--force` |
| `--force` | with `--write`, replace an existing file and say so |

The guidance is the part that is usually missing. An agent with the tools wired
up but no guidance still greps, because nothing told it a blast radius is
available before it edits a symbol, or that an empty caller list means
"unresolved" rather than "unused". The default prints and leaves placement to
you; nothing here touches your configuration.

```bash
cortex agent --guidance >> CLAUDE.md
```

`--check` never reports a missing piece as a failure — an un-integrated project
is the normal starting state — so it exits zero unless something is broken:

```
$ cortex agent --check
[ok  ]  index             revision 1 — 690 files, 9059 symbols, 16495 relationships
[ok  ]  resolution        79% of the 20656 references that name something this index defines are bound
[ok  ]  mcp tools         profile "minimal" exposes 7: get_project_overview, search_code, …
                            means: impact_analysis and find_tests are not exposed on this profile
                            do:    set mcp.profile: standard in .cortex.yml to add them
[warn]  mcp registration  not in this repository's .mcp.json
                            do:    cortex agent, and run the 'claude mcp add' line it prints
[warn]  guidance          not found in CLAUDE.md or AGENTS.md
                            means: the agent has the tools but no rule telling it when to use them
```

### `completion`

Standard Cobra shell-completion generator: `cortex completion bash|zsh|fish|powershell`.
