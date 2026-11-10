# RepoCortex portal — end-to-end suite

One command runs everything:

    npm --prefix internal/web/e2e install     # once
    npm --prefix internal/web/e2e test

`globalSetup` runs `go build ./cmd/cortex`, copies `fixtures/payment-platform`
out of the tree and indexes it with that binary, then starts one portal per
fixture repository (payment-platform on :7925, and any machine-local fixtures
you declare on :7920, :7921 and :7923). `globalTeardown` stops only the processes it started
and removes the copy; ports it does not own make the run fail rather than be
taken over. Three further ports are borrowed briefly by individual specs and
released again: :7922 for the resilience suite's throwaway portals, :7924 for
the cold-start performance measurement and :7926 for the refresh flow's
portal.

Fixture repositories: the one that ships with the repository is declared in
`support/ports.mjs`; machine-local ones are declared in
`support/local-fixtures.mjs`, which is gitignored because a fixture that is a
real repository on one developer's disk is not something a public file should
name. Copy `support/local-fixtures.example.mjs` to start one. The shipped
fixture is enough for every screen to render and every pinned count to be
checked; the local ones exist to reproduce shapes a ten-file corpus cannot:

| fixture | why it is here |
| --- | --- |
| payment-platform | 10 files, 54 symbols, 1 endpoint, 2 tables — the corpus the retrieval benchmark holds the product to, SHIPPED IN THE TREE, so it is the one fixture a CI runner can have (`support/fixture-ci.mjs`) |
| a small local repository | tens of files, no endpoints, no tables — the EMPTY STATES, which are the easiest thing to render as a blank panel |
| a mid-sized local repository | ~1,000 files with a DEGRADED index (parse failures) and no declared architecture — so containers are directories |
| a large polyglot local repository | ~15,000 files with DECLARED components across many languages — SCALE, polyglot edges, a container id hostile to a URL, and a cold structural build big enough for a performance budget to mean something |

Not every claim is repo-independent, and the ones that are not are fixture
data rather than a branch in a spec: `routes` (cross-stack counts routes at the
floor; the overview counts endpoint symbols unfiltered, and on a large
repository those differ), `groupingLabel` (the container rung is called "components" where
an architecture is declared and "directories" where it is not), `intent` (an
intent that names nothing in a repository compiles an honest but empty
contract, so a fixture whose code the default intent does not name carries its own) and `containerNeedle` (a substring that
matches at least one container id).

Selected files or one test:

    npm --prefix internal/web/e2e test -- specs/04-graph.spec.mjs
    npm --prefix internal/web/e2e test -- -g "Tab walks"

## The ci tier

    CORTEX_E2E_TIER=ci npm --prefix internal/web/e2e test

keeps only the shipped fixture. The three machine-local fixtures are removed
from `REPOS` before any spec reads them, and the refresh repository's root is
emptied. Specs that are about size or about a machine-local repository skip
under this tier, keyed on the FIELD they need — a scale budget, a route count
larger than one page, a refresh root — never on a fixture's name; the run
summary reports them as skipped rather than passed.

The ci tier still needs two ports free, not one: :7925 for the shipped
fixture's own portal, and :7922, which the resilience suite (`11-resilience`)
borrows for its throwaway portals in every tier, including ci. A CI job that
only reserves 7925 will see that suite fail on a port collision.

What the specs cover:

| file | subject |
| --- | --- |
| `01-navigation` | rail, `?s=` deep links on cold loads, `/#screen` legacy links, history, unknown screens |
| `02-chrome` | index bar, run badge, confidence tier bar, theme toggle, Copy link, footer |
| `03-overview` | inventory, runtime surface, freshness, resolution, expanders, copy buttons, next actions, the ask box |
| `04-graph` | canvas text rendering, label uniqueness, zoom/pan/fit, node click, cluster expand, keyboard walk, minimap, every filter, exports |
| `05-crossstack` | route totals, chain hops, walking every page for duplicates and drops, page size, walk depth, evidence |
| `06-blast` | resolved / ambiguous / not-found symbols, every depth, deep links |
| `07-contract` | compiling a real task, scope / non-scope / unknowns, every scope budget |
| `08-verify` | pass, advisory and unverified rendered distinctly; questions; file standings |
| `09-governance` | undeclared architecture, every violation-confidence floor |
| `10-hygiene` | placeholder strings, console errors, horizontal scroll at 1440x900 and 375x812, failed requests, a11y |
| `11-resilience` | server killed mid-session, 500, 404, a request that never answers, a malformed body, a dead API at boot |
| `12-graph-advanced` | auto-scope instead of a blank canvas, the client-side confidence lens, cross-language edges, focus to depth N, path finding between two nodes, search and fly-to by keyboard, every lens surviving a cold load, and the JSON export's own arithmetic |
| `13-graph-scale` | pulse only: labels rendered at every rung on 14,614 files, every omission asserted as a count whose arithmetic closes, the container id `(no declared component)` driven through every control that builds a URL, and three performance budgets (cold portal, cold browser load, interaction) |


## Performance budgets

`13-graph-scale` starts its own portal on :7924 and times the FIRST
`/api/graph` request against pulse — the request that builds the whole
structural map. That build reads files, symbols and edges concurrently
(`internal/web/index.go`); serialising them again costs roughly 700ms more,
which is what the budget is sized to catch. The budgets live in
`support/ports.mjs` next to the fixture they apply to, so a machine that is
genuinely slower is one number to change rather than a spec to edit.

## Clicking the canvas

The canvas is asserted, not assumed: `support/fixtures.mjs` wraps
`CanvasRenderingContext2D.fillText` before any page script runs and records
every drawn string with the transform in force, so a test can assert that text
was rendered and can convert a drawn label back into a clickable coordinate.

Two traps that the shared helpers exist to avoid, both of which look like
product bugs and are not:

- The recorder accumulates every frame since page load, including the frames
  drawn while the force layout was still settling. `clickableLabels()` forces
  one repaint and reads only that frame.
- The graph screen is taller than a 900px viewport, so a label can be drawn, be
  perfectly visible to a reader who scrolls, and still be outside the viewport
  when a test clicks its coordinates. `clickableLabels()` scrolls the canvas
  into view and returns only the labels whose click point is actually inside
  the viewport.
