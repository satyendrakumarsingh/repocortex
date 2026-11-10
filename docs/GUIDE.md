# Start here

There are two documents in this repository that will answer almost any question
you have: [README.md](../README.md) tells you what RepoCortex is, and
[user-manual.md](user-manual.md) tells you everything about how it works. The
manual is 1,400 lines. It is a reference, not an entry point, and reading it
front to back to answer one question is the wrong use of it.

This page is the router. Find the row that describes you, follow the numbered
sequence, and stop when you have what you came for. Every command below has been
run; the output blocks are real.

| You are | Go to |
|---|---|
| Wiring RepoCortex into an AI coding agent | [Path A](#path-a--wire-it-into-an-agent) |
| Planning a refactor and you want the blast radius | [Path B](#path-b--blast-radius-before-a-refactor) |
| Enforcing architecture in CI | [Path C](#path-c--enforce-architecture-in-ci) |
| Evaluating whether the accuracy claims are real | [Path D](#path-d--check-whether-the-accuracy-is-real) |

Four other documents sit beside this one:

| Document | The question it answers |
|---|---|
| [CLI.md](CLI.md) | What does this command do, what are its flags, and what does it exit with? |
| [FAQ.md](FAQ.md) | The questions an evaluator asks, including the unflattering ones |
| [NON-GOALS.md](NON-GOALS.md) | What RepoCortex deliberately does not do, and why |
| [BENCHMARKING.md](BENCHMARKING.md) | How to reproduce every number this project publishes |

---

## The prelude every path needs

All four paths start the same way, because everything RepoCortex answers is
answered from an index.

```bash
go build -o cortex ./cmd/cortex   # or: go install github.com/satyendrakumarsingh/repocortex/cmd/cortex@latest
cortex init                          # creates .cortex/, applies the schema
cortex index                         # walks and parses; incremental after the first run
cortex resolve                       # binds references into a call graph
```

`resolve` is a separate step and not an oversight. Indexing extracts references
but cannot bind them: binding needs the complete symbol table, which does not
exist until every file has been parsed. Skip it and the relationship queries
have nothing to search — they will tell you so rather than return an empty
answer.

Real output, on a copy of this repository's own working tree:

```
$ cortex index
  Files:         422 indexed, 54 skipped (476 discovered)
  Symbols:       6259
  References:    29506 (unresolved until M2)
  Imports:       2063
  Duration:      10026ms

$ cortex resolve
  References:  29506
  Resolved:    12062 (40.9%)
  Ambiguous:   2063
  Unresolved:  15381  (calls outside this repository)
  Edges:       11720
    high:    9714
    medium:  133
    low:     2215
```

40.9% is the honest number for a heuristic pass over a repository whose
dependencies live outside it, and RepoCortex prints it on every empty relationship
result rather than letting you read "no callers" as "nothing calls this". If you
want that number higher, Path D is where you make the compiler do the work.

Depth: [user-manual.md → Indexing](user-manual.md#indexing) and
[→ Resolving references](user-manual.md#resolving-references).

---

## Path A — wire it into an agent

The point of RepoCortex is that an agent asks one question instead of running a
dozen exploratory greps. This is the shortest route to that.

**1. Do the prelude above.**

**2. See what the agent will be given.**

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

Seven, not forty. Every exposed tool's schema is injected into the agent's
context on **every** request, so a large tool surface spends the very context
this tool exists to save. That is a product decision, argued in
`internal/mcp/server.go`; see [NON-GOALS.md](NON-GOALS.md#it-does-not-widen-its-mcp-tool-surface).
`mcp.profile` in `.cortex.yml` takes `minimal` (default), `standard` or `full`.

**3. Register the server.**

```bash
claude mcp add cortex -- cortex --project /path/to/repo serve
```

The index is opened read-only for serving: an agent session must not be able to
mutate the index it is querying.

**4. Look at what `get_task_context` actually returns, without an agent in the
loop.** This is the fastest way to find out why an agent got a wrong answer.

```bash
cortex context "Add secret redaction to MCP responses"
```

Every item carries the reason it earned its tokens — `named by the task`,
`1 hop from what the task names, through the call graph (path confidence high)`,
`regression coverage for X, matched by test naming convention`. Add
`--show-excluded` and it also prints what it considered and cut, with the reason.
An engine that cannot tell you what it left out is one you cannot debug.

**5. Keep it current while you work.**

```bash
cortex watch          # debounced, coalesced; the index stays queryable throughout
```

**You are done when** the agent answers a question about your repository without
reading files. If it does not, `cortex context "<the same task>"` shows you
exactly what it was handed.

Depth: [user-manual.md → Serving the index to an agent](user-manual.md#serving-the-index-to-an-agent),
[→ Watching](user-manual.md#watching).

---

## Path B — blast radius before a refactor

**1. Do the prelude.**

**2. Find the thing by name.** `symbol` is exact, not fuzzy — it returns the
definition, not the forty symbols whose names contain it.

```
$ cortex symbol Assembler
KIND   NAME       LOCATION                         CONFIDENCE
class  Assembler  internal/context/assemble.go:47  high (source_code)

1 definition(s) named "Assembler".
```

If you do not know the name, `cortex search callback` is the full-text route;
identifiers are indexed by their parts, so `callback` matches
`handlePaymentCallback`.

**3. Ask what would break.** `callers` is call edges only; `references` is
broader — imports, implementations, extensions and plain references too.

```
$ cortex callers Redact
Redact (function) at internal/security/redact.go:67

KIND   SYMBOL                        EVIDENCE                              CONFIDENCE
CALLS  Bound                         internal/mcp/framing.go:153           high (source_code)
CALLS  RedactFile                    internal/security/redact.go:122       high (source_code)
CALLS  TestDetectsEachPatternClass   internal/security/redact_test.go:181  high (source_code)
...
17 caller(s).
```

Every edge carries the confidence that justified it and the line that evidences
it. **Read the tiers as: act on `high`, show `medium`, ignore `low`** — those
labels are measured, not asserted, and the measurements are in
[accuracy.md](accuracy.md). Filter with `--min-confidence high` when a wrong
edge costs more than a missing one.

Note what happens when there is nothing to report:

```
$ cortex callees profileTools
profileTools (function) at internal/mcp/server.go:129

No callees found.

41% of 29506 references are bound to a definition (15381 point outside this
repository, 2063 were too ambiguous to bind). An empty result is a lower bound,
not proof that nothing uses this symbol.
```

**4. Cross the boundaries a call graph alone does not.** Before changing a
column, the question is not "does anything use this table" but "what writes it,
and where":

```
$ cortex db table transactions
transactions (declared in this repository)

WRITTEN BY (1)
  save (…/TransactionRepository.java)      …TransactionRepository.java:20  high (UPDATE (literal SQL))

READ BY (1)
  findById (…/TransactionRepository.java)  …TransactionRepository.java:14  high (SELECT (literal SQL))
```

`cortex api` does the same for the HTTP surface, and `cortex config` for
settings read by code that nothing declares.

**5. Turn the ask into a contract, then check the change against it.** This is
the half no other tool in this category has.

```bash
cortex compile "Add idempotency to payment callbacks" --json > contract.json
# ... the change gets written ...
cortex verify --contract contract.json --changed "$(git diff --name-only | paste -sd, -)"
```

`verify` answers four questions — did the change stay in scope, break a declared
architecture rule, miss a caller the blast radius flagged, or lose the tests the
contract named — and separates **facts** from **leads**.

The example below is a different repository from the one above: RepoCortex's own
source, with the architecture of [Path C](#path-c--enforce-architecture-in-ci)
declared and a `scip-go` index imported. That combination is what makes a FACT
possible at all — without compiler evidence the same finding is reported as a
LEAD, and the disposition drops from `fail` to `advisory`.

```
$ cortex compile "open the index database from the CLI" --json > contract.json
$ cortex verify --contract contract.json --changed internal/cli/opendb.go
VERIFY: FAIL

[FACT] internal/cli/opendb.go violates architecture rule cli-goes-through-the-registry:
       openReadOnly (cli) depends on CheckVersion (store).
  evidence: internal/cli/opendb.go:48 — openReadOnly REFERENCES CheckVersion, exact
            confidence; rule cli-goes-through-the-registry, verdict forbidden
  scip: scip-go 0.2.7
```

That finding is a FACT because the edge behind it is compiler-proven. The same
violation on a heuristic edge is reported as a LEAD, and the disposition changes
with it. Exit codes are in [CLI.md](CLI.md#verify).

**You are done when** you have the caller list, the tables, and a contract you
can hold the change to.

Depth: [user-manual.md → Querying the index](user-manual.md#querying-the-index),
[→ `db` — tables, reads and writes](user-manual.md#db--tables-reads-and-writes),
[→ `verify` — did the change honour the contract?](user-manual.md#verify--did-the-change-honour-the-contract).

---

## Path C — enforce architecture in CI

RepoCortex checks the **graph**, not imports. "The reporting module must not write
the payments table" is a rule no import-based linter can see, because the write
happens through a repository class reporting is entitled to call.

**1. Do the prelude.**

**2. Declare the components.** They are declared, never inferred — a component
model RepoCortex guessed would be a description of the code's current shape, and
checking code against a description of itself finds nothing by construction. In
`.cortex.yml`:

```yaml
architecture:
  components:
    - name: cli
      include: ["internal/cli/**"]
    - name: registry
      include: ["internal/registry/**"]
    - name: store
      include: ["internal/store/**"]
  rules:
    - id: cli-goes-through-the-registry
      description: The CLI must not reach past the registry into storage.
      from: [cli]
      forbidden: [store]
      severity: high
```

**3. Look at the boundary crossings before you gate on them.**

```
$ cortex arch --crossings
...
COMPONENT DEPENDENCIES (4)
  cli -> store       113 edge(s)  weakest low  ← violates a rule
  cli -> registry    133 edge(s)  weakest low
  registry -> store  140 edge(s)  weakest low
  store -> registry  1 edge(s)    weakest exact

COMPONENTS
  cli       285 symbol(s)  31 file(s)
  registry  389 symbol(s)  25 file(s)
  store     222 symbol(s)  10 file(s)

387 relationship(s) crossed a component boundary.
3788 symbol(s) belong to no declared component and were not checked. A forbidden
dependency routed through one of them would not appear above.

Confidence is the confidence of the RELATIONSHIP, not of the rule. A violation
on a low-confidence edge is a lead; one on a compiler-proven edge is a fact.
```

Read the last two paragraphs before you read the table. Unclaimed symbols are a
hole in the check, and `arch` states the size of the hole rather than reporting a
narrow clean result as a broad one. And the confidence column is the confidence
of the *edge*, so `--min-confidence exact` is how you get the subset a compiler
proved — on the run above it cuts 113 violations to 42.

**4. Gate.** `--fail-on <severity>` is what turns the report into a build
failure:

```
$ cortex arch --fail-on high
VIOLATIONS (113)
  SEVERITY  RULE                           CROSSING                                CONFIDENCE
  high      cli-goes-through-the-registry  cli -> store (symbolName -> symbols)    high
  ...
error: 113 architecture violation(s), worst severity "high" (--fail-on high)
$ echo $?
1
```

**5. Add the other two gates that pay for themselves.**

```bash
cortex docs --check --out docs/generated   # exit 1 if the committed architecture docs drifted
cortex bench cases.yml --fail-on-regression # exit 1 if retrieval quality regressed
```

`docs --check` regenerates `ARCHITECTURE.md` and `CAPABILITIES.md` in memory and
compares. Output is deterministic — the same index regenerates byte-identical
files — so a change that alters the architecture without updating its
documentation fails the build:

```
$ cortex docs --check --out docs/generated
documentation is current (2 file(s) checked).
$ echo $?
0
```

**6. Gate the change itself, not only the tree.** In a PR check:

```bash
cortex compile "$PR_TITLE" --json > contract.json
cortex verify --contract contract.json --changed-from changed.txt
```

`verify` exits 0 on `pass` **and** on `advisory`, 1 on `fail`, and 2 on
`unverified`. `unverified` is deliberately not `pass`: "we looked and found
nothing" and "there was nothing to look at" must not render the same, or the
gate stops gating on the day contract generation breaks and nobody notices. Add
`--fail-on-leads` if you want advisories to block too.

**You are done when** CI fails on a rule violation you introduced on purpose.
Verify the gate that way at least once — a check only ever seen passing is not
evidence of anything.

Depth: [user-manual.md → Architecture governance](user-manual.md#architecture-governance),
[→ `docs`](user-manual.md#docs--architecture-documentation-derived-from-the-graph).

---

## Path D — check whether the accuracy is real

RepoCortex labels every relationship with a confidence. The claim that those
labels mean something is measurable, and it is measurable **on your repository,
not on ours**.

**1. Do the prelude.**

**2. Get compiler ground truth.** Either let RepoCortex provision the indexer —

```bash
cortex indexers list          # what applies here, and what is missing
cortex indexers install --yes # pinned versions, verified digests, refuses on mismatch
```

— or run one you already have:

```
$ scip-go --output index.scip
Resolving module name
Loading Packages
Visiting Packages [80/80]
Indexing Implementations [1222/1222]
Visiting Project Files [80/80]
```

**3. Land it in the graph.**

```
$ cortex scip import index.scip
scip-go 0.2.7: 313 document(s), 276 matched
symbols: 4376 matched, 14810 with no counterpart in the index
edges:   15885 new, 0 upgraded from heuristic, 1026 already known
         72729 reference(s) point outside the indexed repository
duration: 2.218s
```

`0 upgraded from heuristic` is the expected result, not a failure. SCIP records
that one symbol *references* another; it does not record that the reference is a
*call*, and RepoCortex will not stamp a guess about that with `exact`. Compiler
facts therefore arrive as `REFERENCES` edges alongside the resolver's `CALLS`
edges. Likewise, `14810 with no counterpart` is normal: a compiler sees local
variables and generated declarations a tags-based extractor never emits.

**4. Adjudicate the heuristics against the compiler.**

```
$ cortex doctor --resolution-precision
RESOLUTION PRECISION
  ! java, typescript carries heuristic edges that no compiler index in this
    run could judge. Those edges are UNMEASURED here, which is not the same as
    accurate — import a SCIP index for it before reading this report as
    covering them.

  TIER    VERDICT  JUDGED  AGREE  DISAGREE  PRECISION
  high    act on   3,393   3,332  61        98.2%
  medium  show     29      10     19        34.5%
  low     ignore   183     3      180       1.6%
  all     —        3,605   3,345  260       92.8%

COVERAGE
  Judged 3,605 of 7,944 heuristic CALLS edge(s) (45.4%); 4,339 were not adjudicated
  at all and are neither right nor wrong above.
```

Three things to notice, and they are the reason this path exists at all. It
reports **precision, never recall** — the compiler's edge set is a superset by
construction, so dividing by it would be meaningless. It reports the **coverage
the precision was computed over**, so you can see how much of the graph the
number actually speaks for. And it names the languages it **could not measure**
rather than leaving them out.

**5. Check the graph's own invariants, and the retrieval quality.**

```
$ cortex doctor --verify-index
[ok  ]  dangling edges            none
[ok  ]  orphaned symbols          none
[ok  ]  unproven exact edges      none
...

$ cortex bench fixtures/bench/payment.yml
CASE                              RESULT  RECALL  TOKENS  MISSING
change-transaction-status-column  pass    100%    1567    —
idempotent-callback               pass    100%    1368    —
notification-failure-handling     pass    100%    1388    —

3/3 passed, mean recall 100%.
```

`bench` scores against hand-written cases that say what a correct answer must
contain **and what it must not**; the second half is what makes the number mean
anything, since recall alone is trivially gamed by returning the whole
repository. It does not measure whether an agent completed a task — see
[NON-GOALS.md](NON-GOALS.md#it-does-not-claim-to-measure-task-completion).

**You are done when** you have a precision table for your own repository. Then
read [BENCHMARKING.md](BENCHMARKING.md) for the corpora, the pitfalls, and what
each number does and does not mean, and [accuracy.md](accuracy.md) for the
method and the one language still unmeasured.

---

## If something goes wrong

[troubleshooting.md](troubleshooting.md) is keyed on the literal error messages.
`cortex doctor` is the first thing to run; it checks the runtime prerequisites
and, with `--verify-index`, the graph itself.
