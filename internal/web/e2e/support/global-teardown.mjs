import fs from 'node:fs';
import path from 'node:path';
import { BINARY, stopPid } from './servers.mjs';
import { removeCiFixture } from './fixture-ci.mjs';

export default async function globalTeardown() {
  const file = path.join(path.dirname(BINARY), 'pids.json');
  if (!fs.existsSync(file)) return;
  const state = JSON.parse(fs.readFileSync(file, 'utf8'));
  for (const pid of Object.values(state.pids || {})) stopPid(pid);
  removeCiFixture(state.fixtureRoot);
  fs.rmSync(file, { force: true });
}
