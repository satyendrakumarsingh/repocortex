import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { REPO_ROOT } from './servers.mjs';

/*
  THE REPOSITORY-LOCAL FIXTURE.

  Every other fixture is a real repository on one developer's disk, which is
  right for the shapes they exercise and useless for a CI runner. This one is
  fixtures/payment-platform — the corpus the retrieval benchmark and the
  golden context test already hold the product to — copied out of the tree so
  it can carry its own .cortex/ and its own git history (one commit), then
  indexed with the binary under test.

  It is small on purpose: 10 files, 54 symbols, 26 relationships, one Spring
  endpoint, two tables. That is enough for every screen to render something
  and for every count the specs pin to be checked, and it indexes in well
  under a second, which is what lets the whole suite run on every pull request.
*/
export const CI_FIXTURE_SOURCE = path.join(REPO_ROOT, 'fixtures', 'payment-platform');

/*
  The copy lives one level down inside the temp directory, under the source's
  own name: the portal names a project by its directory, and specs assert that
  name in the rail and the footer. The temp directory is what teardown removes.
*/
export function prepareCiFixture(binary) {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'cortex-e2e-payment-'));
  const root = path.join(tmp, path.basename(CI_FIXTURE_SOURCE));
  fs.cpSync(CI_FIXTURE_SOURCE, root, {
    recursive: true,
    filter: (src) => path.basename(src) !== '.cortex',
  });
  const run = (cmd, args) => execFileSync(cmd, args, { cwd: root, stdio: 'pipe' });
  run('git', ['init', '-q', '.']);
  run('git', ['add', '-A']);
  run('git', ['-c', 'user.name=cortex-e2e', '-c', 'user.email=e2e@cortex.invalid', 'commit', '-q', '-m', 'fixture']);
  run(binary, ['init', '-q']);
  run(binary, ['index', '-q']);
  run(binary, ['resolve', '-q']);
  return root;
}

export function removeCiFixture(root) {
  if (!root) return;
  const tmp = path.dirname(root);
  if (path.basename(tmp).startsWith('cortex-e2e-payment-')) fs.rmSync(tmp, { recursive: true, force: true });
}
