import { spawn, execFileSync } from 'node:child_process';
import { setTimeout as sleep } from 'node:timers/promises';
import net from 'node:net';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../../..');
export const BINARY = path.join(REPO_ROOT, 'internal/web/e2e/.bin/cortex-e2e');

export function buildBinary() {
  execFileSync('go', ['build', '-o', BINARY, './cmd/cortex'], { cwd: REPO_ROOT, stdio: 'inherit' });
  return BINARY;
}

export async function portFree(port) {
  return new Promise((resolve) => {
    const s = net.createServer();
    s.once('error', () => resolve(false));
    s.once('listening', () => s.close(() => resolve(true)));
    s.listen(port, '127.0.0.1');
  });
}

export async function waitForPortal(port, ms = 30_000) {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`http://127.0.0.1:${port}/api/health`);
      if (res.ok) return true;
    } catch { /* not up yet */ }
    await sleep(200);
  }
  throw new Error(`portal on 127.0.0.1:${port} never became ready`);
}

export async function startPortal({ port, root }) {
  if (!(await portFree(port))) throw new Error(`port ${port} is already in use — this suite refuses to touch a port it does not own`);
  const child = spawn(BINARY, ['ui', '--port', String(port), '--project', root, '--quiet'], {
    stdio: 'ignore', detached: true,
  });
  child.unref();
  await waitForPortal(port);
  return child.pid;
}

export function stopPid(pid) {
  if (!pid) return;
  try { process.kill(pid, 'SIGKILL'); } catch { /* already gone */ }
}
