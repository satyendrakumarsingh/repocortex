# Non-goals

Things RepoCortex deliberately does not do, each with the reason.

None of the reasoning below is written for this page. This codebase argues its
non-goals in its own package documentation and command help, because a
constraint that lives only in a design document is one the next contributor
removes. What follows is that argument, gathered in one place and pointed back
at the file where it is enforced.

A non-goal is not a gap. Every entry here is a decision that would make the tool
worse to reverse, and several of them are the reason a capability works at all.

---

## It is not a code-search engine competing on retrieval

**Where:** `internal/context/doc.go`

The objective the context engine optimises is **useful engineering understanding
per token**, not recall. An agent has a finite context window, and every token
spent on a file it did not need is a token it cannot spend on thinking.
Returning fifty files is a failure even when the right five are among them: the
agent still has to read fifty, and the noise degrades its reasoning. The golden
test for context assembly is written so that the assertion carrying the weight
is the one about what must be **absent**.

There is a second, harder claim underneath. Lexical retrieval alone cannot do
this job, and the bundled payment fixture contains the trap in miniature:
`unrelated/marketing/MarketingReport.java` carries the comment "Unrelated to
payments", so for the task "Add idempotency to payment callback processing"
full-text search ranks it **above** `CallbackController` — the word "payment"
appears more prominently in a file that has nothing to do with payments. Any
engine scoring by lexical similarity returns it.

What separates the two is not vocabulary but structure. So graph connectivity is
an **admission gate**, not merely a ranking signal, and lexical evidence alone
admits a candidate only when several distinct task terms corroborate it.

`cortex search` exists and is a perfectly ordinary FTS5 query. It is a
utility, not the product, and `cortex context` is not a better version of it.

## It does not use a model as a resolver

**Where:** `internal/resolve/scip/doc.go`, `internal/resolve/infer`

A syntax tree can tell you that `svc.updateStatus()` is a call expression. It
cannot tell you *which* `updateStatus`, because that needs a type for `svc`,
which needs imports, which needs the build graph. A language model asked the
same question will answer, and the answer will be plausible.

Plausible is the problem. The confidence system exists to separate a
compiler-proven fact from a heuristic guess, and a guess stamped `exact` is
precisely the lie it exists to prevent. So heuristic resolution tops out at
`high`, and `exact` is reserved for facts a typechecker established — enforced
in three independent places: `model.Edge.Validate` in Go, a SQL `CHECK` in
migration 0001, and `registry.validateRun` before a run is published.
`doctor --verify-index` checks the whole graph for violations afterwards.

The `scip` package holds the line even where it would be easy not to. SCIP does
not record whether a reference is a call, so **no `CALLS` edges are emitted from
it at all** — a plausible `CALLS` edge stamped "exact" would be the same lie in
a smaller package.

## It does not call a model anywhere else either

**Where:** `internal/compile`, `internal/verify`, `internal/bench`,
`internal/context`

Four packages that could plausibly use one, and the same argument in each:
determinism is what makes the output usable.

- **`compile`** — a contract that changed between two runs of the same input
  could not be cached, tested, or trusted. "The brief drifted" is a worse
  failure than "the brief was thin".
- **`verify`** — a verdict that changed between two runs of the same inputs
  could not be gated on.
- **`bench`** — an LLM judge would make the score depend on a model version and
  a sampling temperature, and the regression this most needs to catch (a pack
  that quietly stopped containing the payment controller) is exactly the one a
  generous judge forgives.
- **`context`** — an agent that gets a different answer on a retry cannot build
  a cache, a test, or trust.

Two shipped features rest on this directly. `cortex diff` is only meaningful
because two runs over identical source produce identical graphs; otherwise "this
edge disappeared" might mean a worker pool interleaved differently.
`cortex docs --check` can only gate CI because generated documents are
byte-reproducible.

`compile` also refuses the adjacent temptation: it does not invent requirements
the index cannot support. It has no idea what "idempotent" should mean for a
given handler, and an invented acceptance criterion would be indistinguishable,
to the reader, from one that came from the repository. Those gaps are emitted as
**Unknowns** instead — "which is the whole difference between a brief and a
hallucination".

## It does not persist runtime spans alongside a static index

**Where:** `internal/trace/trace.go`, `cortex trace --help`

`cortex trace` correlates an exported trace against the graph and then throws
the spans away. Runtime data ages out in hours while an index describes a
commit, and storing them together would produce a graph whose edges are true at
different times.

Three narrower refusals sit inside the same package:

- **It does not sample.** Sampling is a decision the producer already made
  before those bytes existed; re-sampling here would silently change the
  denominator under every count reported, and a call rate computed from a sample
  the caller did not choose is worse than no call rate.
- **It does not talk to a collector.** The only input is a byte slice the caller
  already holds. A service that must be running for RepoCortex to work is a
  service RepoCortex has no business requiring, and the export is the artefact CI
  already produces. It is also why nothing here can leak a repository's trace
  data outward.
- **It does not retain the export.** Spans carry arbitrary user attributes — SQL
  with embedded credentials, URLs with bearer tokens, request bodies. Only a
  small allow-list of keys survives parsing.

Correlation stays heuristic and says so per match: an explicit `code.function`
attribute names the symbol outright, an `http.route` match is strong, and a span
name matching a symbol is weak, tried only when nothing better exists and only
when the name is unique — "a span called `handle` tells you nothing about which
of nine handlers ran".

## It does not widen its MCP tool surface

**Where:** `internal/mcp/server.go`

> THE TOOL COUNT IS A PERFORMANCE CONSTRAINT, NOT A PREFERENCE.

Every exposed tool's schema is injected into the agent's context on **every**
request, and a long tool list measurably degrades tool selection. A server
exposing forty tools would spend context on descriptions the agent never uses —
which is precisely the waste RepoCortex exists to eliminate. Shipping a small
default is part of the product, not a limitation of it.

The default profile exposes seven, and `get_task_context` is listed first
because it is the tool an agent should reach for first: one call instead of a
dozen exploratory ones.

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

Two related refusals in the same package. **Responses are bounded and never
contain whole files** — a tool that returns a whole file has made the context
problem worse, forever, on every call. And **`isError` is reserved**: a model
that sees a tool fail early concludes the toolset is broken and stops calling it
for the rest of the session, so "you have not indexed yet" is returned as a
successful result whose text carries the next step, not as a failure. The cost
of getting that wrong is not one bad answer, it is every answer RepoCortex would
have given afterwards.

## It does not infer your architecture

**Where:** `cortex arch --help`, `internal/arch`

Components are **declared**, never inferred. A component model RepoCortex guessed
would be a description of the code's current shape, and checking code against a
description of itself finds nothing by construction.

This is why `cortex arch` on a repository with no `architecture:` block does
not silently invent one from directory names — it says there is nothing declared
and explains what to declare. `cortex docs` behaves the same way: a repository
that declares no components is told so rather than handed a component model
invented for it.

The architecture rules are read from the repository's own config even though
that file is untrusted, and that is deliberate too: they are pure declarative
data — path globs and component names, matched by a hand-written matcher and
never compiled as a regular expression — so nothing in them can select a tool or
an argument. And it is where they belong: **a rule that lives in one
contributor's home directory is a rule that is not enforced.**

## It does not ship vulnerability data

**Where:** `cortex deps --help`

`deps` reports versions accurately so that a real scanner can consume them, and
performs no vulnerability lookup itself. A scanner shipping an embedded advisory
list is stale the day it is built; the lookup belongs against a live database.

It reads **manifests, not lock files**. A lock file is the resolved transitive
closure — hundreds of packages, most of which no line of the repository mentions.
What a project depends on is a statement its authors made, and "you declare
cobra and never import it" is actionable in a way "your lock file contains
golang.org/x/sys" is not.

And an unused dependency is reported as a **prompt, not a verdict**: a package
can be loaded by reflection, required by a build plugin, or imported under a
name that does not match its distribution name.

## It does not read configuration values

**Where:** `cortex config --help`, `internal/analyze/config`,
`internal/security/doc.go`

Values are never read. Not masked, not truncated — never read at all. The
scanners stop at the delimiter and no structure in the tool has a field to hold
a value.

That is a stronger guarantee than redaction, and the reason is worth stating
plainly: a redaction list can be wrong about a key nobody anticipated, and a
data structure with nowhere to put a value cannot leak one. Keys whose **name**
indicates a credential are labelled sensitive, which requires reading nothing.

The same asymmetry governs `internal/security` generally. A false positive
redacts a line of code and someone files an annoyed issue; a false negative
ships a live credential to a model provider's logs, and no release fixes that —
the credential has to be rotated, assuming anyone notices. Every judgement call
leans toward redacting, and the package uses **only the standard library**,
because a supply-chain compromise in the redaction path would be invisible and
total.

There is no `security:` key in the repository config, and its absence is the
control: a repository you cloned cannot express an opinion about redacting its
own secrets, in either direction. Writing one is a parse error, not a setting
that is quietly ignored.

## It does not let the repository decide what runs on your machine

**Where:** `internal/indexer/doc.go`, `internal/config`

`cortex index` is expected to be run immediately after `git clone`, on code
nobody has read yet, so everything in the working tree is attacker-controlled.
Two of the four indexers execute the repository's own build system, and a Maven
plugin or a Gradle build script is arbitrary code by design — that is what a
build system *is*.

So execution settings live in `~/.config/cortex/config.yml` and the repository
config **structurally cannot express them**: the fields do not exist on the
type, and unknown keys are a parse error. `TestRepoCannotSetExecutionSettings`
holds the line.

Five more refusals follow from the same question — "can the repository influence
this?":

- **Nothing is read from the repository except the names of marker files.**
  Detection stats `pom.xml`, `go.mod`, `package.json`; it never opens them.
- **Provisioning is never silent and never unpinned.** Versions and digests are
  compile-time constants; a missing or mismatched digest refuses the install.
- **There is no shell.** Every subprocess is an explicit argv with an absolute
  executable path — a relative path would resolve against the hostile
  repository's directory.
- **Every subprocess is timeboxed and its output bounded**, in the runner rather
  than left to callers to remember.
- **Repository-derived strings never become command arguments.** `project.name`
  from `.cortex.yml` is the obvious temptation; it is replaced by a fixed
  literal.

The payoff is that `exact` means something. If a detected language cannot be
indexed by a compiler, `exact` mode **fails and names the language and the
missing tool**. It never degrades quietly, because a silent downgrade from
"proven" to "guessed" is the lie the confidence system exists to prevent.

## It does not claim to measure task completion

**Where:** `internal/bench/bench.go`

`cortex bench` measures one thing: whether the context pack contained what a
human said it should, and whether it stayed inside its budget.

Whether an agent handed a good pack then went on to write correct code is a
property of the agent and the model, not of retrieval. Folding the two together
makes a retrieval regression indistinguishable from a bad day at the model
provider, which destroys the only signal the package exists to produce. The
command's own help puts it more bluntly: conflating them "would be the most
flattering possible mistake".

The `forbid` list is the other half of the same discipline. Recall alone is
trivially gamed by returning the whole repository, so a case says what the
answer must contain **and** what it must not, and a forbidden item retrieved is
reported as a precision failure rather than averaged away.

## `verify` is not a linter, a test runner, or a reviewer

**Where:** `internal/verify/verify.go`

It asks four questions — scope, architecture rules, missed callers, test
coverage — and refuses to ask a fifth. Every other judgement about a diff
already has a tool, and **a verifier that also had opinions about naming would
be switched off along with them**.

It does not run git either. The changed files are an **input**, because a
verifier that shelled out to `git diff` would work in exactly one situation — a
clean checkout of a branch — and not in the one that matters: an agent asking
"is what I have in my working tree acceptable" before it writes anything
anywhere.

And it does not report a finding it cannot evidence. Every finding carries the
evidence and the confidence it rests on, and an exit filter drops anything that
does not.

## It does not tell you whether an ADR is still true

**Where:** `cortex arch adr --help`

An ADR is why a choice was made. Surfacing it to an agent is how "we
deliberately use optimistic locking here" survives contact with a refactor
instead of being re-litigated from scratch every session.

But a record saying the system uses Postgres is evidence of a **decision**, not
of the current state. The graph is where you check the current state, and
conflating the two would let a stale document overrule the code.

## It does not discover routes from strings that look like paths

**Where:** `cortex api --help`, `internal/analyze/api`

Routes are discovered from the framework's own spelling — a Spring annotation, a
Go router registration, a FastAPI or Flask decorator, a Django `path()` in a
urlconf, an Express member call, a NestJS controller decorator. That is what
keeps a log message mentioning `GET /users` out of the report. Django routes
are reported at `medium` confidence, because the `include()` mount prefix is
not visible from the file.

The consequence is stated rather than hidden: OpenAPI documents, GraphQL
schemas, gRPC service definitions and API-gateway configuration are **not
covered**, so a service defined entirely in those reports no endpoints — which is
why the supported framework list is printed alongside every result.

## It does not treat a bounded answer as a complete one

**Where:** `internal/graph/graph.go`, `internal/capability/capability.go`,
`internal/diffgraph/doc.go`

Not a feature so much as a rule the whole system is held to, and the one most
often traded away elsewhere.

A traversal that quietly stops at a limit and returns a partial answer as if it
were complete is worse than an error: an impact analysis built on it
under-reports risk, and nothing downstream can tell. So every graph result
carries `Truncated` and a reason. Every capability report carries a
`Completeness` value that `NewCompleteness` constructs so the lower-bound and
dynamic-dispatch caveats **cannot** be omitted by a caller who forgets. Every
diff section records how many entries it omitted. Every empty relationship query
reports the index's measured resolution coverage instead of an empty list.

---

## What this leaves

Capabilities are subcommands over one shared graph, not separate products. Their
value is in the joins — "what breaks if I change this?" needs symbols *and* APIs
*and* database writes *and* tests answered in one traversal. Anything that would
require a second store, a second source of truth, or a non-reproducible pass is
a candidate for this page rather than for the roadmap.

See [GUIDE.md](GUIDE.md) for what it does do, and [FAQ.md](FAQ.md) for the
questions these decisions raise.

## Semantic (embedding-based) code search — not now, and the condition that would change it

**What it is.** Convert code into vectors and retrieve by meaning rather than by
keyword, so "where do we handle refunds?" finds code about chargebacks. Three of
the eight projects surveyed in `docs/internal/research/` have it: sense, codeseeker and
codebase-context.

**Why it is not built here**, in the order the reasons actually matter:

**1. It does not fix the number that is weak.** RepoCortex's measured gap is
*resolution* recall: `high` catches **61.5%** of adjudicable calls, and 2,855 of
8,315 known call sites produce no edge at all (`cortex accuracy`). No embedding
model closes that. Finding a file by meaning and binding `s.repo.Save(id)` to its
definition are different operations — the second needs a receiver type, and a
vector has no opinion about types. Effort spent on embeddings is effort not spent
on the deficit we publish.

**2. Retrieval is not currently the bottleneck.** The bench set scores 3/3 at
100% recall. That is a weak instrument (see below) but it is the instrument we
have, and it is not reporting a retrieval problem.

**3. It sits against a headline property.** Every practical ONNX runtime for Go
is a CGO binding, and "single binary, zero CGO, contributors need no C toolchain"
is a promise on the front page. A build tag would preserve the default build, but
it would also mean the accuracy story splits in two — one for the tagged build
and one for the untagged one — and every measurement in `docs/accuracy.md` would
need saying which it applies to.

**4. The competitor evidence is narrower than it looks, and cuts our way.**
codeseeker's own ablation puts its knowledge graph at **±0% MRR** on top of
BM25+embeddings. That is often read as "embeddings win"; what it actually says is
that a graph built *for retrieval* adds nothing to a retrieval product. RepoCortex's
graph is not for retrieval. It is for impact, blast radius and verify — questions
an embedding cannot answer at all, because they are about reachability rather
than similarity.

**What would change this.** Not an argument — a measurement. So the measurement
was taken.

### The benchmark was built, and it argues against half of the above

`fixtures/bench/self.yml` runs against this repository — 400+ real Go files —
with task descriptions written the way a person asks rather than the way the code
is spelled. Against `payment.yml`'s 3/3 at 100%, it scores:

```
confidence-floor-on-impact   FAIL   50%    missing DefaultMinConfidence
empty-answer-is-not-absence  FAIL    0%    missing ResolutionStats
secret-redaction             FAIL    0%    missing Redact
what-runs-the-index-twice    FAIL    0%    missing RegistryDigest, forbidden: internal/bench/
which-tests-cover-a-change   pass  100%
1/5 passed, mean recall 30%, 1 forbidden item retrieved
```

The one case that passes is the one whose vocabulary matches the code: "find the
tests that exercise a function" → `FindTests`. Every failure is a vocabulary
mismatch — "unreliable edges" → `DefaultMinConfidence`, "returned nothing... code
is unused" → `ResolutionStats`, "never print a password" → `Redact`, "same code
twice, different results" → `RegistryDigest`.

**That is the semantic-search case, stated in our own numbers.** Point 2 above —
"retrieval is not currently the bottleneck" — was true only of a 9-file fixture
and is now retracted. Points 1, 3 and 4 stand unchanged: embeddings still do not
close the resolution-recall gap, the CGO tension is unchanged, and codeseeker's
ablation still says a graph built for retrieval adds nothing.

So the honest position is narrower than "not now". Lexical retrieval demonstrably
misses code when the asker's vocabulary diverges from the codebase's, and this is
now a measured defect with a reproducible harness rather than a hypothesis. What
has NOT been established is that an ONNX dependency is the right fix: the same
five cases should first be tried against cheaper, CGO-free options — identifier
splitting and stemming in the FTS5 tokenizer, synonym expansion over the terms
the graph already knows, and ranking by graph proximity to whatever the lexical
pass did find.

**The benchmark now exists to falsify whichever is tried.** That is the part
every competitor's semantic search is missing, and it is why it was worth
building before the feature rather than after.

### The cheap fix was tried, measured, and reverted

Inverse document frequency over task terms. The reasoning was sound: term
weights are assigned by grammatical role — `identifier 1.5, noun 1.0,
verb 0.35` — so "file" and "password" both score 1.0, in a repository where one
appears in a third of all symbols and the other in a handful. Weighting each
term by how rare it is *here* should have promoted the discriminating word.

It was applied to both retrieval paths (symbol names in `findSeeds`, file bodies
in `searchLexical`), computed from the in-memory catalogue with no new query and
no schema change. Measured on the same index, varying only the binary:

| | symbol-level (5 cases) | area-level (4 cases) |
|---|---|---|
| without IDF | 1/5, 30% | 2/4, 50%, 1 forbidden |
| **with IDF** | **1/5, 30%** | **2/4, 50%, 1 forbidden** |

**Identical. Not better, not worse — no effect at all.** Reverted, under the
same rule that reverted the two resolver guards: a change with no measured
benefit does not ship, however good the argument for it sounds.

### Where the retrieval gap actually is — four attempts, located precisely

Four principled fixes were tried against the five failing cases. **Every one
measured as no change: 1/5, 30%.** They are listed because each ruled out a
hypothesis, and together they locate the problem exactly.

| # | Fix | Why it seemed right | Why it did nothing |
|---|---|---|---|
| 1 | IDF on symbol-name matching | "file" and "password" both score 1.0 as nouns | `Redact` matches no word in the task, so no name weighting can reach it |
| 2 | IDF on file-text matching | a file matching "password" should beat one matching "file" | changed the numerator of `h.weight/total` only; the denominator stayed role-weighted, so every file shrank by the same factor and no ordering moved |
| 3 | IDF on both sides of the ratio | fixes #2's asymmetry | correct, and still nothing — the candidate was being rejected before scoring |
| 4 | Admit on term RARITY, not term COUNT | `MinLexicalTerms: 2` rejected `redact.go`, which matches one term | admission succeeded; the candidate then ranked below 2,847 others |

**The actual mechanism**, confirmed with `--show-excluded`: `Redact` is now a
candidate and is one of *2,847 candidates below the marginal-value cut-off*, with
1,576 more that did not fit a 100-item / 20-file / ~8,000-token budget. It ranks
too low because a candidate admitted on lexical evidence scores at most
`Lexical: 0.9`, while every symbol *named* `File` or `Config` scores
`SymbolNameMatch: 3.0`. A name match is worth 3.3× a body match, and the task
contains the common word but not the rare symbol's name.

**The weight ratio was then tried too, and that framing was also wrong.**
Instrumenting the pack gave the numbers that settle it: the inclusion threshold
for this task is **1.62**, the top item scores **6.17**, and `Redact` scores
about **0.13** on lexical evidence alone. That is a **12x** gap, not the 3.3x
the weight ratio suggests — so raising `Lexical` toward `SymbolNameMatch` could
never have closed it, and doing so far enough to try would flood every pack with
weak body matches.

A fifth attempt combined the two halves that had only been tried separately:
discrimination applied to name scores (to lower the junk holding the threshold
up) and specificity applied to lexical scores (to raise a file matching one rare
word). Measured, it made things **worse**: the threshold rose 1.62 -> 2.08 and
the top score rose 6.17 -> 6.61, because the rare words in these tasks
("password", "unreliable") appear in file BODIES and almost never in symbol
NAMES, so name discrimination barely moved the noise while specificity lifted
everything. Reverted.

### The architectural conclusion

For a task whose key noun appears only in file bodies and never in a symbol
name, this architecture cannot surface the right symbol. Symbol candidates are
scored overwhelmingly by name evidence — `SymbolNameMatch: 3.0` and
`GraphProximity: 2.2` against `Lexical: 0.9` — and the graph expansion that
supplies proximity starts from name-matched seeds. A symbol reachable only
through its file's TEXT therefore enters with one weak signal and no path to
acquire a strong one.

That is a design property, not a tuning error. The obvious fix — seed the graph
traversal from strong lexical hits, not only from name matches — **was then
built and measured. It also did not move the number**, and the attempt is worth
recording because of what it found on the way.

### Lexical seeding: built, instrumented, still 1/5

Strong full-text hits became traversal roots alongside name matches, bounded by
`MaxLexicalSeeds` and gated on term rarity. Instrumenting it produced two
findings that invalidate the earlier attempts in this section:

**1. The discrimination map was a no-op, and had been all along.** Rarity was
measured across SYMBOL NAMES, where even "config" and "make" appear in under 2%
of symbols — so every term scored 1.0:

```
disc = map[config:1 file:0.89 make:1 never:1 password:1 print:1 sure:1]
```

**That is why four separate reweighting attempts measured exactly zero.** They
were multiplying by one. The discriminating corpus for a full-text hit is the
set of FILES, not the set of names.

**2. One file consumed the entire seed budget.** All eight slots went to
`internal/analyze/db/orm.go` — the first file at the top signal — because the
loop took every unit from a file before moving to the next. A per-file cap fixed
it and the seeds then spread across four files.

With both corrected, seeding demonstrably works: eight roots, four files,
rarity varying properly (`password` 0.90, `config`/`file`/`never` 0.76,
`make` 0.27 as a verb). **The bench did not move.** `redact.go` still lost, now
because "sure" — idiomatic scaffolding in "make sure" — scored 0.97 on rarity
and out-ranked "password". Adding "sure" to the stopword list did not close it
either.

Reverted. Seven attempts total, all measuring 1/5 at 30%.

### What this section is actually saying

Each fix here was sound in isolation and each revealed the next layer: term
weighting was a no-op, so seeding could not have worked; seeding drained one
file, so rarity could not spread; rarity promoted idiom, so the right file never
seeded. That is the signature of a problem that wants a redesign of how a task
becomes a query, not another multiplier on the existing one.

What has NOT been tried, and is the honest next step: stop treating the task as
a bag of independently-weighted words. Every failure here traces to the same
root — "make sure we never print a password from a config file" contains one
word that matters and six that do not, and the pipeline has no representation
for that distinction beyond a grammatical prior and a rarity count, both of
which measured as noise. A structured intent — a subject, an action, and the
rest discarded — would make `password` the query and the rest context. That is a
design change, and it should be attempted against these five cases with
`payment.yml` as the regression guard.

Two things worth keeping. `MinLexicalTerms: 2` is documented as guarding against
a decoy that matches one COMMON word, and it does — but it conflates "one term"
with "weak evidence", and those differ. And an embedding tier would have landed
in exactly the same place: a semantic hit on `redact.go` produces a candidate
carrying `Lexical`-weight evidence, which is the weight that loses. **Semantic
search would not have fixed these five cases either.** That is the strongest
argument in this whole section for not reaching for it.

Worth stating plainly: this section describes four fixes that did not work. It
is here because the next person will otherwise re-derive all four, and because a
negative result with a mechanism attached is worth more than an untested idea.
