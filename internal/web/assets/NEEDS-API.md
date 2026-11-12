# Fields the portal wants and the API does not send

Written by the agent that owns `internal/web/assets/`. Each entry names the
field, the screen that wants it, why, and **what the portal does instead today**
— because a missing field must degrade into a stated limitation, never into a
blank space or an invented number.

Nothing here is blocking. Every screen listed below renders correctly without
the field; each would render something more honest or more useful with it.

---

## 1. `run.status_reason` — resolved, no longer needed

**Status: DELIVERED.** `meta.index_health` now carries `status`, `severity`,
`headline`, `reasons[]` (`code`, `detail`, `count`, `remedy`) and `trustworthy`.
The Overview's health card and the index bar on every screen render it directly.
This entry is kept only to record that the portal no longer infers a reason for
a degraded run from the parse-failure counter, which is what it did before.

---

## 2. A confidence floor on the architecture gate — `ci_gates[].min_confidence`

**Screen:** 07 Architecture governance, "What a CI gate would do right now".

**Why.** All five violations on `pulse` sit on `low`-confidence edges. The gate
table reports that `--fail-on high`, `--fail-on medium` and `--fail-on low` would
all fail the build, while the same screen's prose says a violation on a
compiler-proven edge is a fact and one on a `low` edge is a lead. Recommending a
gate that fails a build on five leads is how a team switches the gate off — and
it takes the facts with it.

**What is wanted.** Either a `min_confidence` dimension on `ci_gates[]`, so the
API states what `repocortex arch --fail-on high --min-confidence high` would do; or
a `--min-confidence` flag on `repocortex arch` whose effect the API reports.

**Graceful degradation, shipped.** The portal computes the breakdown itself from
`violations_by_confidence` and prints, beside the gate table, how many violations
are proven versus heuristic — and labels that paragraph as computed in the
portal rather than reported by the gate. It never restates a gate result the API
did not give.

---

## 3. A line number on contract scope rows — `contract.scope[].line`

**Screen:** 01 Overview (task box) and 05 Task contract, the Scope table.

**Why.** `contract.blast_radius[]` carries `evidence` as `path:line`, so every
reached symbol is checkable. `contract.scope[]` carries `path` only. On `pulse`
the scope came back with four rows for `ClientReader` in four near-identical
vendored paths and three rows named `handler` in the same file — with no line,
a reader cannot tell which declaration is meant, and the portal cannot dedupe
them safely either.

**Graceful degradation, shipped.** Rows are deduplicated on `path` + `symbol`,
the number folded away is stated in the omissions card, and the file column
renders the path alone rather than fabricating a location.

---

## 4. `worker-src 'self'` in the Content-Security-Policy

**Screen:** 02 Graph explorer. Not a payload field — a header, in `web.go`.

**Why.** The CSP is `default-src 'none'; script-src 'self'; …`. `worker-src` is
not declared, so it inherits `default-src` and every `new Worker(...)` is
refused, including one constructed from a same-origin `/assets/*.js`. The graph
layout is the one genuinely CPU-bound thing the portal does.

**Graceful degradation, shipped.** The layout is time-sliced against a frame
deadline instead: `createLayout(...).step(deadlineMs)` runs ranking passes and relaxation ticks
until it hits the budget, returns, lets the canvas repaint, and resumes on the
next animation frame. The main thread is never blocked for more than one frame,
which is the property that matters; a worker would only reduce total wall time.
A 1,620-symbol graph settles in well under a second this way, so this request is
low priority.

**Also note:** `style-src 'self'` without `'unsafe-inline'` refuses the `style`
ATTRIBUTE. That is correct and the portal now respects it — every dynamic
dimension is applied through `element.style.setProperty`, which is CSSOM and not
inline style. Anyone adding `style="…"` to a template will find the declaration
silently dropped by the browser, which for a proportional bar means every
segment renders at zero width. Please do not relax this; the client is already
compatible with it.

---

## 5. Per-language edge counts on the graph's language filter

**Screen:** 02 Graph explorer, the Language control.

**Why.** `options.languages[]` is a flat list of names. On `pulse` it offers
`markdown`, `json`, `yaml`, `toml`, `properties` and `dockerfile` — languages
that produced zero edges, so selecting one guarantees an empty graph with no
explanation of why. The Overview already knows which languages produced symbols
(`languages.mix[].symbols`); the graph does not know which produced *edges*.

**What is wanted.** `options.languages[]` as objects carrying at least
`{ language, edges }`, or a parallel `options.language_edges` map.

**Graceful degradation, shipped.** The Overview separates languages that
produced no symbols from those that did, names them, and says they cannot appear
in any graph. The graph's own filter still lists every language the API offers,
because dropping an option the API returned would be the portal deciding what
the index contains.

---

## 6. Cross-stack ordering key for a stable deep link

**Screen:** 03 Cross-stack.

**Why.** `page.ordering` states the total order in prose, and `page` now carries
`offset`, `first`, `last`, `has_next` and `next_offset`, which is enough to page
correctly — the dead pager is fixed and route 397 of 411 is reachable and
linkable. What a URL cannot yet do is name a *route*: `?off=396` points at a
position in an ordering, so it moves if the index is rebuilt.

**What is wanted.** Accept `?endpoint=<symbol_id>` on `/api/crossstack` and
return the page containing it.

**Graceful degradation, shipped.** The offset is in the query string and the
range is rendered from `page`, so a link is shareable within one indexed run.
Nothing claims it is stable across runs.

---

## 7. A second indexed run, so the explorer can draw a DIFF

**Screen:** 02 Graph explorer.

**Why.** `repocortex diff` exists as a command, and "what changed between the run
we shipped and the run we are about to ship" is the strongest question a change
-safety graph can answer: an edge that appeared, an edge that lost a tier, a
component that grew a dependency it did not have. It was considered for this
round of the explorer and REJECTED as unbuildable from the current surface,
rather than faked.

**What the API sends today.** Every read path resolves exactly one run —
`Server.run` returns `CurrentRun`, and `structuralIndex` is keyed by that single
run id and cached one-deep. There is no way to ask `/api/graph` for a second
run, no run list, and no per-edge first-seen or last-seen. A client cannot
diff two runs it cannot fetch.

**What is wanted.** The smallest useful version is a run list
(`GET /api/runs` — id, git revision, finished time, status) plus a `?run=` on
`/api/graph`. With those two, the whole diff is client-side: fetch both, key
edges on (from, to, kind), and render added / removed / re-tiered as three
stated populations with counts.

**Graceful degradation, shipped.** Nothing. There is no honest partial version
of a diff — a "diff" computed against a single run would be a comparison of a
thing with itself dressed up as a finding — so the explorer does not offer the
control at all rather than offering one that cannot mean anything. This is the
one entry in this file where the portal does nothing instead of doing less.

---

## 8. A `language` on `ContainerNode` edges, so cross-language is exact at the top rung

**Screen:** 02 Graph explorer, the cross-language lens.

**Why.** At the file and symbol rungs each node carries exactly one language, so
"this relationship crosses a language boundary" is a fact the client can compute
without inference. At the CONTAINER rung a component is a bag of languages, so
the client falls back to "the two components share no language at all" — which
is correct but coarse: a Go-and-Java component talking to a Java component is
not counted as crossing, even when the specific edges behind that bundle do.

**What is wanted.** A per-edge language pair on `container_edges[]` — the
languages of the two ENDPOINT SYMBOLS the aggregated bundle came from, as a
small histogram — which the server already has while it aggregates and the
client can never recover afterwards.

**Graceful degradation, shipped.** The lens states its own rule on screen, per
rung, and counts separately the relationships whose endpoint language the index
never recorded, so "unknown" never renders as "no".
