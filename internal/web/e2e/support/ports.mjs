import fs from 'node:fs';

export { PORTS } from './port-table.mjs';
import { PORTS } from './port-table.mjs';

export const REPOS = {
  /*
    The fixture that ships with the repository. `root` is the temp copy
    global-setup prepares, passed through the environment because setup and
    the spec workers are different processes; every other field is pinned the
    way the machine-local fixtures pin theirs.
  */
  payment: {
    name: 'payment-platform',
    port: PORTS.payment,
    root: process.env.REPOCORTEX_E2E_PAYMENT_ROOT || '',
    endpoints: 1,
    tables: 2,
    // The class has one definition; the service name is shared by a class and
    // its constructor, so it resolves to two.
    resolvedSymbol: 'src/main/java/com/pay/controller/CallbackController.java:CallbackController',
    ambiguousSymbol: 'TransactionService',
    inScopeChanged: 'src/main/java/com/pay/controller/CallbackController.java',
    // The one endpoint is one route: the two populations coincide here, and the
    // number is written down rather than defaulted so a drift in either shows.
    routes: 1,
    // The suite's default intent was written FOR this corpus — it is the golden
    // task the retrieval benchmark holds the product to.
    intent: 'Add idempotency to payment callback processing',
    // No declared architecture, so containers are directories, every one of
    // them under src/ except unrelated/marketing.
    containerNeedle: 'src',
    groupingLabel: 'directories',
  },
};

/*
  MACHINE-LOCAL FIXTURES.

  Only the shipped fixture is declared in this file, because this file is
  public and a fixture that is a real repository on one developer's disk is
  not. Those live in local-fixtures.mjs, which is gitignored; the shape is in
  local-fixtures.example.mjs. The file is optional: absent, or under
  REPOCORTEX_E2E_TIER=ci, the suite runs against the shipped fixture alone, and
  a spec that needs what only a local fixture has skips on the missing field.
*/
const LOCAL_FILE = new URL('./local-fixtures.mjs', import.meta.url);
let local = {};
if (process.env.REPOCORTEX_E2E_TIER !== 'ci' && fs.existsSync(LOCAL_FILE)) {
  local = await import(LOCAL_FILE.href);
}
Object.assign(REPOS, local.LOCAL_REPOS || {});

/*
  The repository the refresh flow rebuilds (specs/14-provenance). Machine-local
  like the rest; without one, `root` is empty and the refresh block skips.
*/
export const REFRESH = local.REFRESH || { name: '', port: PORTS.refresh, root: '' };

/*
  Fixture defaults. `routes` is the cross-stack population and `containerNeedle`
  is a string that matches at least one container id — both differ between a
  repository that declares components and one that does not, and both used to be
  hardcoded in the specs against whichever fixture was written first.
*/
for (const repo of Object.values(REPOS)) {
  if (repo.routes === undefined) repo.routes = repo.endpoints;
  if (repo.containerNeedle === undefined) repo.containerNeedle = 'src';
  if (repo.groupingLabel === undefined) repo.groupingLabel = 'directories';
}

export const INTENT = 'Add idempotency to payment callback processing';

/*
  The intent a spec should compile against a given fixture. An intent that names
  nothing in the repository produces an honest but useless contract — no scope,
  nothing to verify — so a fixture that needs its own says so above.
*/
export const intentFor = (repo) => repo.intent || INTENT;
export const base = (repo) => `http://127.0.0.1:${repo.port}`;
export const SCREENS = ['overview', 'graph', 'crossstack', 'blast', 'contract', 'verify', 'governance'];
