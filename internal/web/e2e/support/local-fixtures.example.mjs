// MACHINE-LOCAL FIXTURES for the portal e2e suite — an EXAMPLE.
//
// Copy this file to local-fixtures.mjs (gitignored) and point each `root` at an
// already-indexed repository on your machine. The shipped fixture,
// fixtures/payment-platform, is declared in ports.mjs and needs nothing from
// you; everything here is optional and is skipped entirely under
// CORTEX_E2E_TIER=ci.
//
// Every value a spec pins lives on the fixture, never in a spec. The fields:
//   name, port, root            required; `port` from PORTS so nothing collides
//   endpoints, tables           what the overview reports
//   routes                      cross-stack routes at the floor (defaults to endpoints)
//   resolvedSymbol              a symbol that resolves to exactly one definition
//   ambiguousSymbol             one that resolves to more than one
//   inScopeChanged              a file the fixture's intent puts in scope
//   intent                      an intent that names code in this repository
//   containerNeedle             a substring matching at least one container id
//   groupingLabel               'components' where an architecture is declared, else 'directories'
//   components                  the declared component count, where one is declared
//   awkwardContainer            a container id hostile to a URL, if the repo has one
//   coldGraphBudgetMs, warmGraphBudgetMs, interactionBudgetMs
//                               performance budgets; only a large repository should carry them
//
// The maintainers' set covers three shapes worth reproducing: a small repository
// with EMPTY states (no endpoints, no tables), a ~1,000-file repository with a
// DEGRADED index and no declared architecture, and a ~15,000-file polyglot
// repository with declared components, which is where every budget bites.
import { PORTS } from './port-table.mjs';

export const LOCAL_REPOS = {
  small: {
    name: 'small-service',
    port: PORTS.local1,
    root: '/absolute/path/to/small-service',
    endpoints: 0,
    tables: 0,
    resolvedSymbol: 'src/main/java/com/example/Builder.java:Builder',
    ambiguousSymbol: 'Builder',
    inScopeChanged: 'src/main/java/com/example/Builder.java',
  },
};

// The repository the refresh flow rebuilds (specs/14-provenance). Not in
// LOCAL_REPOS because a refresh rebuilds the index, and rebuilding a fixture
// whose counts other specs assert would quietly break those. Leave `root`
// empty to skip that block.
export const REFRESH = {
  name: 'scratch-repo',
  port: PORTS.refresh,
  root: '',
};
