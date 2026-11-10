import { test, expect, cold, settle } from '../support/fixtures.mjs';
import { REPOS, PORTS } from '../support/ports.mjs';
import { startPortal, stopPid } from '../support/servers.mjs';

/*
  FAILURE MODE 1 — a dead server, a 500 or a timeout must reach the reader.

  The brief's charge is that a portal whose server disappears mid-session shows
  an infinite spinner and nothing else. Each test below puts the UI into that
  exact situation and asserts an ERROR STATE appears within a bounded time.

  "An error state" is asserted as: the spinner is gone, and the screen carries
  text that names the failure. A screen that simply keeps spinning fails.
*/

const BOUND = 15_000;

async function waitForErrorState(page, where, ms = BOUND) {
  const deadline = Date.now() + ms;
  let last = '';
  for (;;) {
    const state = await page.evaluate(() => {
      const screens = [...document.querySelectorAll('section.screen')].filter((s) => !s.hidden);
      const main = document.getElementById('main');
      const scope = screens.length ? screens : [main];
      return {
        spinning: scope.some((s) => s.querySelectorAll('.loading').length > 0),
        errors: scope.reduce((n, s) => n + s.querySelectorAll('.err').length, 0),
        text: scope.map((s) => s.innerText || '').join('\n'),
      };
    });
    last = `spinner still on screen: ${state.spinning}; .err blocks: ${state.errors}`;
    if (!state.spinning && state.errors > 0) return state;
    if (Date.now() > deadline) {
      expect(state.errors,
        `${where}: no error state within ${ms}ms — ${last}. Rendered text was:\n${state.text.slice(0, 600)}`)
        .toBeGreaterThan(0);
      return state;
    }
    await page.waitForTimeout(250);
  }
}

// Nothing here depends on what a repository contains; the throwaway portals
// only need a root to boot from. The first fixture is the smallest, so its
// portals come up fastest.
const repo = Object.values(REPOS)[0];

test.describe('resilience', () => {
  test('a server killed MID-SESSION surfaces an error state, not an infinite spinner', async ({ page }) => {
    // Its own throwaway portal, so nothing else in the suite is disturbed.
    const doomed = { name: 'doomed', port: PORTS.doomed, root: repo.root };
    const pid = await startPortal(doomed);
    try {
      await cold(page, doomed);
      await expect(page.locator('#screen-overview'), 'the portal came up').toBeVisible();
      stopPid(pid);
      // Now ask for a screen that has not been loaded yet. Every fetch it makes
      // will fail at the socket.
      await page.locator('.nav[data-screen="crossstack"]').click();
      await waitForErrorState(page, 'cross-stack after the server died');
    } finally {
      stopPid(pid);
    }
  });

  test('every screen surfaces a dead server rather than spinning forever', async ({ page }) => {
    const doomed = { name: 'doomed', port: PORTS.doomed, root: repo.root };
    const pid = await startPortal(doomed);
    try {
      await cold(page, doomed);
      stopPid(pid);
      for (const screen of ['graph', 'crossstack', 'governance']) {
        await page.locator(`.nav[data-screen="${screen}"]`).click();
        await waitForErrorState(page, `${screen} after the server died`);
      }
    } finally {
      stopPid(pid);
    }
  });

  test('a 500 from the API is shown to the reader, not swallowed', async ({ page }) => {
    await page.route('**/api/crossstack**', (route) =>
      route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ error: 'the index blew up' }) }));
    await cold(page, repo);
    await page.locator('.nav[data-screen="crossstack"]').click();
    const state = await waitForErrorState(page, 'cross-stack on a 500');
    expect(state.text, 'the reader is told what went wrong').toMatch(/the index blew up|could not load/i);
  });

  test('a 404 from the API is shown to the reader, not swallowed', async ({ page }) => {
    await page.route('**/api/governance**', (route) =>
      route.fulfill({ status: 404, contentType: 'application/json', body: JSON.stringify({ error: 'no such route' }) }));
    await cold(page, repo);
    await page.locator('.nav[data-screen="governance"]').click();
    const state = await waitForErrorState(page, 'governance on a 404');
    expect(state.text, 'the reader is told what went wrong').toMatch(/no such route|could not load/i);
  });

  test('a request that never answers surfaces as an error, not a permanent spinner', async ({ page }) => {
    // A hung endpoint is the timeout case: the socket stays open and no body
    // ever arrives.
    await page.route('**/api/blast**', () => { /* never fulfilled, never aborted */ });
    await cold(page, repo);
    await page.locator('.nav[data-screen="blast"]').click();
    await page.fill('#blast-symbol', repo.ambiguousSymbol);
    await page.locator('#screen-blast button.primary', { hasText: 'Analyse' }).click();
    await waitForErrorState(page, 'blast on a hung request', 30_000);
  });

  test('a malformed JSON body surfaces as an error rather than a placeholder-filled screen', async ({ page }) => {
    await page.route('**/api/overview**', (route) =>
      route.fulfill({ status: 200, contentType: 'application/json', body: '{ this is not json' }));
    await cold(page, repo);
    await waitForErrorState(page, 'overview on a malformed body');
  });

  test('a dead server at BOOT is stated, not left as an empty shell', async ({ page }) => {
    const doomed = { name: 'doomed', port: PORTS.doomed, root: repo.root };
    const pid = await startPortal(doomed);
    let html;
    try {
      html = await (await fetch(`http://127.0.0.1:${doomed.port}/`)).text();
    } finally {
      stopPid(pid);
    }
    // Serve the real shell from a dead origin: every /api call 500s.
    await page.route('**/*', async (route) => {
      const url = route.request().url();
      if (url.endsWith('/') || url.endsWith('/index.html')) return route.fulfill({ status: 200, contentType: 'text/html', body: html });
      if (url.includes('/api/')) return route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: 'server is down' }) });
      return route.continue();
    });
    await page.goto(`http://127.0.0.1:${repo.port}/`, { waitUntil: 'domcontentloaded' });
    const state = await waitForErrorState(page, 'boot against a dead API');
    expect(state.text, 'the boot failure is named').toMatch(/server is down|could not load/i);
  });
});
