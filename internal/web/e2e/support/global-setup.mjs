import fs from 'node:fs';
import path from 'node:path';
import { REPOS } from './ports.mjs';
import { buildBinary, startPortal, stopPid, BINARY } from './servers.mjs';
import { prepareCiFixture, removeCiFixture } from './fixture-ci.mjs';

export default async function globalSetup() {
  fs.mkdirSync(path.dirname(BINARY), { recursive: true });
  buildBinary();

  // Everything below can fail after something has already started (a busy
  // port, a portal that never becomes healthy). Without this, that failure
  // leaks the temp fixture copy and any portals already up, because
  // pids.json — what teardown reads — is only written at the very end.
  const pids = {};
  let fixtureRoot;
  try {
    // The shipped fixture is indexed here, with the binary just built. Its
    // root is handed to the spec workers through the environment: globalSetup
    // runs in a different process, so a field set on REPOS here would never
    // reach them.
    if (REPOS.payment) {
      fixtureRoot = prepareCiFixture(BINARY);
      REPOS.payment.root = fixtureRoot;
      process.env.CORTEX_E2E_PAYMENT_ROOT = fixtureRoot;
      process.stdout.write(`fixture prepared: ${REPOS.payment.name} at ${fixtureRoot}\n`);
    }

    for (const key of Object.keys(REPOS)) {
      pids[key] = await startPortal(REPOS[key]);
      process.stdout.write(`portal up: ${REPOS[key].name} on :${REPOS[key].port} (pid ${pids[key]})\n`);
    }
    fs.writeFileSync(path.join(path.dirname(BINARY), 'pids.json'), JSON.stringify({ pids, fixtureRoot }));
  } catch (err) {
    for (const pid of Object.values(pids)) stopPid(pid);
    removeCiFixture(fixtureRoot);
    throw err;
  }
}
