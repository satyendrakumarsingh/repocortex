# Portal API — what changed, and what the client must render

Written for the agent rebuilding `assets/`. The authoritative machine-readable
list is `testdata/api-contract.golden` (regenerate with
`go test ./internal/web -run TestAPIFieldContractIsFrozen -update`). This file
is the reasoning behind the fields that are new or renamed.

Two tests in `contract_test.go` will fail until the client consumes them, and
both failures name the field and say why it exists:

- `TestJSReadsOnlyFieldsTheAPISends` — every `x.some_field` in the shipped JS
  must be a field some response marshals. It is currently failing on
  `low_confidence_hidden`, which no longer exists (see below).
- `TestClientRendersTheHonestySurface` — a fixed list of fields the client MUST
  mention at all. Each entry is a defect that reached a user.

---

## 1. Omissions: one omission, one number

**Removed:** `omissions.low_confidence_hidden`.

It duplicated the `below_confidence_floor` entries inside `omissions.omitted`.
Both carried 8,015 on the same page and the portal drew them as two lines, so one
population read as two totalling 16,030.

**Now:**

```jsonc
"omissions": {
  "omitted": [
    { "reason": "below_confidence_floor",
      "label": "hidden below the standard of proof",   // NEW: render this, not the reason
      "count": 8015,
      "detail": "...",
      "remedy": "lower the confidence floor to `low`" }
  ],
  "totals": [                                       // NEW: derived rollup of `omitted`
    { "reason": "below_confidence_floor",
      "label": "hidden below the standard of proof",
      "count": 8015,
      "entries": 2 }                                // how many rows summed to it
  ],
  "complete": false
}
```

`totals` is computed at marshal time from `omitted`. They cannot disagree.
Render `totals` for the headline and `omitted` for the detail rows, or either
alone — never as two independent lists.

**Never render `reason` as a UI label.** That is what produced `row_budget:`
appearing twice on one card with two unrelated meanings. Use `label`.

`blast.low_confidence_hidden` is also gone; the figure is in
`omissions.totals[below_confidence_floor]`.

## 2. Index health — `meta.index_health`

`run.status: "degraded"` was sent on every response and rendered by nothing.

```jsonc
"meta": {
  "run": { "status": "degraded", ... },
  "index_health": {
    "status": "degraded",
    "severity": "warn",              // ok | warn | error
    "trustworthy": false,            // gate the banner on THIS, not on a string compare
    "headline": "This index is degraded: 3 problem(s) mean every count on every screen is a lower bound, not a total.",
    "reasons": [
      { "code": "files_unaccounted", "count": 412, "detail": "...", "remedy": "repocortex doctor --verify-index" }
    ]
  }
}
```

Present on every screen's `meta`. `/api/health` also carries it at the top level
as `index_health`, distinct from its `status: "ok"` which is the *process's*
health.

Render it wherever a reader looks first. `trustworthy: false` must be visible
without scrolling.

## 3. Graph — levels, clusters, aggregated edges, budgets

`view` is gone; `level` replaces it and is addressable:

- `GET /api/graph` → `level: "containers"`
- `GET /api/graph?level=files&container=X` → **new**, one container's files
- `GET /api/graph?level=symbols&container=X` → as before (still the default when
  `container` is given without `level`)

`options.levels` enumerates them.

Every level carries:

| field | meaning |
|---|---|
| `clusters[]` | community per node — `id`, `label`, `members[]`, `size`, `internal_edges`, `external_edges`, `method`, `caveat` |
| `cluster_edges[]` | aggregated edges *between* clusters, same shape as any other aggregated edge |
| `node_budget` | `{parameter, requested, maximum, available, drawn, excluded, rule, remedy}` |

Nodes carry `cluster` (the cluster id). Aggregated edges carry `from_cluster` /
`to_cluster`; symbol edges carry `src_cluster` / `dst_cluster`. So the client can
collapse/expand without a refetch and without re-deriving the partition.

Clustering is **deterministic** (label propagation over a total order — see
`cluster.go`). Do not re-cluster client-side; you will get a different answer.

`clusters[].caveat` says a community is a drawing aid, not a declared
architecture. Render it.

### The aggregated-edge table is first-class

`container_edges`, `file_edges` and `cluster_edges` are all `AggregatedEdge`:

```jsonc
{ "from": "document-service", "to": "shared-service",
  "count": 79,                     // edges AT OR ABOVE the floor
  "confidence": "high",            // the WEAKEST surviving tier — draw the line at this
  "histogram": { "exact": 0, "high": 74, "medium": 5, "low": 0, "unknown": 0, "total": 79 },
  "kinds": [ {"kind":"CALLS","count":74}, {"kind":"EXTENDS","count":5} ],
  "evidence": { "file_path": "backend/.../client.py", "line": 212 } }
```

`kinds` now sums **exactly** to `count`. It previously did not — `count` was
floored and `kinds` was not, producing rows reading `edges 50 · CALLS×572`.

This table (from · to · edges · weakest · kinds · example evidence) is the single
artifact no competitor ships. It must not be behind a `<details>`.

### Renamed on `ContainerNode`

- `files` → **`files_with_symbols`** and **`files_matched`** (see §4)
- `exported` → `exported_symbols` (unchanged in fact, stated here because the
  old contract test's second bug was exactly this field)

## 4. One component, two file counts — both named

Graph said `task-app 551 files`; governance said `3,425`. Both correct, both
labelled "files".

- `files_matched` — every file the component's include patterns select.
- `files_with_symbols` — the subset that produced a symbol. Only these can be
  drawn.
- governance adds `files_without_symbols` (the difference) per component row.

Governance `components[]` is now `ComponentRow`, not
`architecture.ComponentSummary`. Both screens read the same arithmetic from the
same place, so they agree by construction. **Show both, or show neither.**

## 5. Cross-stack — real paging

`GET /api/crossstack?limit=&offset=`

```jsonc
"page": {
  "offset": 12, "limit": 12, "total": 411, "returned": 12,
  "first": 13, "last": 24,
  "has_prev": true, "has_next": true,
  "prev_offset": 0, "next_offset": 24,      // -1 when the direction does not exist
  "ordering": "routes with a recorded caller first, then ... then endpoint symbol id — total, so a page boundary cannot duplicate or drop a route",
  "max_limit": 250
}
```

Always present, including on a complete result. **Do not parse the omission
detail string** — that is what disabled Next on every load and made 399 of 411
routes unreachable.

### Counts split by scope

`counts.endpoints`, `counts.with_client`, `counts.with_handler`,
`counts.consumes_edges` are **whole-index** and do not change with the page.

`counts.page.*` (`chains`, `with_client`, `with_handler`, `with_service`,
`with_table`, `complete_to_table`, `writing_to_a_table`) are **page-scoped**, and
`counts.page.scope` is the sentence saying so. Render them in a visibly different
place from the headline row.

## 6. Verify — `pass` and `unverified` are different verdicts

The screen rendered a green `PASS` / `exit 0` for two files the contract took no
position on.

`verdict` may now be `unverified` where it previously said `pass`, with
`exit_code: 2` and `signal: "unverified"`. New:

- `unverified: true` — a boolean, so a client cannot mis-compare a string.
- `questions[]` — the four questions, in fixed order:

```jsonc
{ "id": "scope",
  "question": "Did the change stay inside the declared scope?",
  "asked": true,        // the inputs existed to ask it
  "answered": false,    // it engaged THIS change set
  "covered": 0,
  "unit": "changed files the contract took a position on",
  "findings": 0,
  "detail": "The contract declared 12 scope entr(ies) and 8 exclusion(s), and none of them names a file in this change. The check ran and had nothing to say: it did not find the change acceptable, it found the change unrelated to the brief." }
```

`asked && !answered` is the state that used to render as a pass. The four-questions
table must show both columns.

`report.checked` gains `declared_changed_files`, `changed_scope_symbols`,
`attributed_violations`.

---

---

## `GET /api/source` — the code map's Code view

```jsonc
{
  "path": "internal/pipeline/resolve.go", "language": "go", "container": "internal/pipeline",
  "from": 1, "to": 348, "lines": ["…"],            // 1-based window; ?from= moves it
  "has_more": false, "next_from": 0,               // set when the 4,000-line window cut the file
  "declarations": [ { "id": 32808, "name": "Resolve", "kind": "function", "line": 80,
                      "exported": true, "confidence": "high", "callers": 10, "callees": 12 } ],
  "evidence_lines": [ { "line": 69, "count": 1 } ],  // lines a relationship cites
  "min_confidence": "medium",                       // the floor callers/callees were counted at
  "omissions": { … },                               // row_budget counts the lines not sent
  "guidance": { … }                                 // no path / not in the index / no stored text
}
```

The text comes from `FileExcerpt`, the full-text copy taken at index time — never
the filesystem. A path the index does not hold is answered with guidance and is
never read; a file whose symbols were recorded without its text says so rather
than rendering as an empty file. Both cases are tested in `source_test.go`.

---

## Regenerating the contract

```
go test ./internal/web -run TestAPIFieldContractIsFrozen -update
go test ./internal/web
```

If `TestClientRendersTheHonestySurface` fails, the client is not rendering
something that reached a user as a defect before. The failure message says which
and why.
