import { spawn } from 'node:child_process';
import { test, expect, cold, settle, assertClean, assertNoPlaceholders, bodyText } from '../support/fixtures.mjs';
import { REPOS, REFRESH } from '../support/ports.mjs';
import { BINARY, portFree, waitForPortal, stopPid } from '../support/servers.mjs';

/*
  THE INDEX'S OWN PROVENANCE, AND THE ONE ROUTE THAT WRITES.

  Two states, and the difference between them is the whole security posture of
  the portal: without --allow-refresh there is no POST route at all, and the
  page must therefore show no button — a control that cannot work teaches a
  reader the product is broken when it is merely read-only.

  The default portals this suite starts are the read-only state. The refresh
  state needs a portal of its own, started below with the flag, against a
  repository this suite does NOT pin counts for: a refresh rebuilds the index,
  and rebuilding a fixture whose file and route counts other specs assert would
  make this spec quietly break those.
*/


test.describe('read-only portals — index provenance', () => {
  for (const key of Object.keys(REPOS)) {
    const repo = REPOS[key];

    test(`${repo.name}: the bar offers a provenance disclosure, closed until asked`, async ({ page }) => {
      await cold(page, repo);
      const toggle = page.locator('#prov-toggle');
      await expect(toggle, 'the disclosure is on every screen').toBeVisible();
      await expect(toggle).toHaveAttribute('aria-expanded', 'false');
      await expect(toggle).toHaveAttribute('aria-controls', 'provenance');
      await expect(page.locator('#provenance'), 'nothing is rendered until it is opened').toBeHidden();

      await toggle.click();
      await expect(toggle).toHaveAttribute('aria-expanded', 'true');
      await expect(page.locator('#provenance')).toBeVisible();
      assertClean(page, `${repo.name} provenance disclosure`);
    });

    test(`${repo.name}: the panel dates the index, names its revision and refuses to claim it is HEAD`, async ({ page }) => {
      await cold(page, repo);
      await page.locator('#prov-toggle').click();
      const panel = page.locator('#provenance');
      await expect(panel).toBeVisible();
      const said = (await panel.innerText()).trim();

      // An absolute local moment, not only a relative age: "3 hours ago" is not
      // something a reader can line up against the commit they remember making.
      expect(said, 'the panel dates the build absolutely').toMatch(/\b\d{4},?\s+\d{1,2}:\d{2}\b/);
      expect(said, 'the panel keeps the relative age too').toMatch(/ago/);
      expect(said, 'the panel names the revision the index was built from, or says there was none')
        .toMatch(/revision\s+[0-9a-f]{7,}|recorded no revision/);

      // The load-bearing honesty: a revision is a claim about committed
      // history, and the working tree is not committed history.
      expect(said, 'a matching revision must not read as a matching working tree')
        .toMatch(/staged, unstaged, or committed/);
      expect(said, 'the panel says it cannot see the checkout').toMatch(/still HEAD/);
      expect(said, 'and hands over the command that answers it').toContain('git rev-parse HEAD');

      // The schema version is not in any payload this package marshals. It is
      // stated as a gap with the command that closes it, in the same omissions
      // vocabulary the rest of the portal uses.
      expect(said, 'the schema version is named as unstated, not invented').toMatch(/schema version/i);
      expect(said, 'with the command that prints it').toContain('cortex status');

      expect(said, 'what an index costs to build').toMatch(/zero model calls/);
      assertNoPlaceholders(said, `${repo.name} provenance panel`);
      assertClean(page, `${repo.name} provenance panel`);
    });

    test(`${repo.name}: no dead refresh button; the command is shown instead`, async ({ page }) => {
      await cold(page, repo);
      await expect(page.locator('#refresh-index'), 'a read-only portal shows no refresh control')
        .toHaveCount(0);
      await page.locator('#prov-toggle').click();
      const said = (await page.locator('#provenance').innerText()).trim();
      expect(said, 'it names the command that rebuilds').toContain('cortex index && cortex resolve');
      expect(said, 'and the flag that would enable the button').toContain('--allow-refresh');
      expect(said, 'and says why there is no button').toMatch(/read-only/i);
    });

    test(`${repo.name}: the commit hook is reported, never installed from here`, async ({ page }) => {
      await cold(page, repo);
      await page.locator('#prov-toggle').click();
      const said = (await page.locator('#provenance').innerText()).trim();
      expect(said, 'the hook has a stated verdict').toMatch(/commit hook\s*\n(installed|not installed|not checked)/i);
      if (/commit hook\s*\nnot installed/i.test(said)) {
        expect(said, 'a missing hook means the index is only as current as the last build')
          .toMatch(/only as current as/);
        expect(said, 'and the command that installs it is named').toContain('cortex hooks install');
        expect(said, 'and the portal says why it does not do it itself').toMatch(/\.git directory/);
      }
      // Whatever the state, the page must never offer to write to the repo.
      const body = await bodyText(page);
      expect(body, 'the portal never offers to install anything').not.toMatch(/Install now|Add the hook for me/);
    });
  }
});

/*
  THE REFRESH FLOW, against a portal started with the flag.

  The success path is exercised for real — a real POST, a real rebuild, a real
  new run — because the point of the feature is that the numbers on the screen
  stop being stale, and only a real rebuild proves that. The 409 and the failure
  are driven through route interception: "two refreshes at once" and "the
  indexer failed" are server states this suite cannot arrange on demand, and the
  copy a reader sees in them is exactly what must not regress.
*/
test.describe('--allow-refresh — the refresh control', () => {
  // The refresh repository is machine-local; a tier without one has no root.
  test.skip(!REFRESH.root, 'no refresh repository on this tier; see REFRESH in support/ports.mjs');

  let pid = 0;
  const origin = `http://127.0.0.1:${REFRESH.port}`;

  test.beforeAll(async () => {
    if (!(await portFree(REFRESH.port))) {
      throw new Error(`port ${REFRESH.port} is already in use — this spec refuses to touch a port it does not own`);
    }
    const child = spawn(BINARY, ['ui', '--port', String(REFRESH.port), '--project', REFRESH.root,
      '--allow-refresh', '--quiet'], { stdio: 'ignore', detached: true });
    child.unref();
    pid = child.pid;
    await waitForPortal(REFRESH.port);
  });

  test.afterAll(() => stopPid(pid));

  const open = async (page) => {
    await page.goto(`${origin}/`, { waitUntil: 'domcontentloaded' });
    await settle(page);
    await expect(page.locator('#prov-toggle')).toBeVisible();
    await page.locator('#prov-toggle').click();
  };

  test('the control exists, is a button, and says what it will do', async ({ page }) => {
    await open(page);
    const btn = page.locator('#refresh-index');
    await expect(btn).toBeVisible();
    await expect(btn).toHaveText('Refresh index');
    await expect(page.locator('#refresh-status'), 'a screen reader is told, not just shown')
      .toHaveAttribute('aria-live', 'polite');
    await expect(page.locator('#refresh-status')).toHaveAttribute('role', 'status');
    assertClean(page, 'refresh control');
  });

  test('it is reached by keyboard, shows focus, and fires on Enter', async ({ page }) => {
    // Answered without rebuilding anything: what is under test is the control,
    // and a conflict reply is the cheapest real answer the server gives.
    await page.route('**/api/reindex', (route) => route.fulfill({
      status: 409,
      contentType: 'application/json',
      body: JSON.stringify({ status: 'already_running', detail: 'a refresh is already in progress' }),
    }));
    await open(page);

    // Tab, not .focus(): :focus-visible is a statement about how focus arrived,
    // and a programmatic focus draws no ring in any browser that implements it.
    await page.locator('#prov-toggle').focus();
    await page.keyboard.press('Tab');
    const focused = await page.evaluate(() => document.activeElement && document.activeElement.id);
    expect(focused, 'the control follows the disclosure in the tab order').toBe('refresh-index');

    const outline = await page.evaluate(() => {
      const node = document.getElementById('refresh-index');
      const style = getComputedStyle(node);
      return `${style.outlineStyle} ${style.outlineWidth}`;
    });
    expect(outline, 'a keyboard reader can see where they are').not.toMatch(/^none/);

    await page.keyboard.press('Enter');
    await expect(page.locator('#refresh-status')).toContainText('A refresh is already in progress');
  });

  test('while a refresh runs the control is disabled, busy, and shows progress', async ({ page }) => {
    // A held-open response, because the real rebuild of a small repository is
    // over in a second or two and a test that raced it would be a flake, not a
    // check. What is asserted here is the state a reader is left in meanwhile.
    await page.route('**/api/reindex', async (route) => {
      await new Promise((done) => setTimeout(done, 2500));
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          status: 'ok',
          result: { revision: 99, files: 12, symbols: 34, edges: 56, duration_ms: 2500 },
        }),
      });
    });
    await open(page);
    const btn = page.locator('#refresh-index');
    await btn.click();

    await expect(btn, 'the control is disabled while it runs').toBeDisabled();
    await expect(btn).toHaveAttribute('aria-busy', 'true');
    await expect(btn).toHaveText('Refreshing…');
    await expect(page.locator('#refresh-status'), 'and the reader is told what is happening')
      .toContainText('Rebuilding the index');
    await expect(page.locator('#refresh-elapsed'), 'progress, counted in seconds')
      .toHaveText(/\d+s/);
    await expect(page.locator('#refresh-elapsed'), 'but not announced every second')
      .toHaveAttribute('aria-hidden', 'true');

    await expect(page.locator('#refresh-status'))
      .toContainText('Rebuilt in 2.5 s: 12 files, 34 symbols, 56 edges, revision 99.', { timeout: 30_000 });
    await expect(btn, 'the control comes back').toBeEnabled();
  });

  test('a real refresh carries the anti-CSRF header, reports what changed, and re-reads the screen', async ({ page }) => {
    const posts = [];
    const reads = [];
    page.on('request', (r) => {
      if (r.method() === 'POST') posts.push({ url: r.url(), header: r.headers()['x-cortex-reindex'] });
      if (r.method() === 'GET' && r.url().includes('/api/overview')) reads.push(r.url());
    });

    await open(page);
    const before = reads.length;
    const btn = page.locator('#refresh-index');
    await btn.click();

    const status = page.locator('#refresh-status');
    await expect(status, 'what changed, in numbers')
      .toHaveText(/Rebuilt in .+: [\d,]+ files?, [\d,]+ symbols, [\d,]+ edges, revision \d+\./, { timeout: 120_000 });
    await expect(status).toContainText('This screen has been re-read from it.');

    expect(posts.length, 'exactly one POST').toBe(1);
    expect(posts[0].url, 'to the refresh route').toContain('/api/reindex');
    expect(posts[0].header, 'carrying the header a cross-site request cannot set').toBe('1');

    await expect(btn, 'the control comes back').toBeEnabled();
    await expect(btn).toHaveText('Refresh index');

    // "Re-read from it" is a claim about the network, so it is checked there.
    // The screen the reader is on refetches; nothing is left describing a run
    // that has been rebuilt underneath it.
    await expect.poll(() => reads.length, { timeout: 30_000, message: 'the current screen refetches' })
      .toBeGreaterThan(before);
    await expect(page.locator('#refresh-index'), 'and focus comes back to the control')
      .toBeFocused();
    assertClean(page, 'refresh success');
  });

  test('a refresh already in flight is reported, not retried', async ({ page }) => {
    await page.route('**/api/reindex', (route) => route.fulfill({
      status: 409,
      contentType: 'application/json',
      body: JSON.stringify({
        status: 'already_running',
        detail: 'a refresh is already in progress; this one was not started so two indexers cannot write the same file',
      }),
    }));
    await open(page);
    await page.locator('#refresh-index').click();
    const status = page.locator('#refresh-status');
    await expect(status).toContainText('A refresh is already in progress');
    await expect(status, 'and why this click did nothing').toContainText('this one was not started');
    await expect(page.locator('#refresh-index'), 'the control comes back').toBeEnabled();
    assertNoPlaceholders(await bodyText(page), 'refresh conflict');
  });

  test('a failed refresh shows the server’s words and says the index survived', async ({ page }) => {
    await page.route('**/api/reindex', (route) => route.fulfill({
      status: 500,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'the refresh failed and the index was left as it was: parse stage aborted' }),
    }));
    await open(page);
    await page.locator('#refresh-index').click();
    const status = page.locator('#refresh-status');
    await expect(status, 'the server is quoted, never a bare "error"')
      .toContainText('The refresh failed and the index was left as it was: parse stage aborted');
    const said = await bodyText(page);
    expect(said, 'no bare error word stands alone').not.toMatch(/^\s*error\s*$/im);
    assertNoPlaceholders(said, 'refresh failure');
  });

  test('a refusal states the index is intact even when the server does not', async ({ page }) => {
    await page.route('**/api/reindex', (route) => route.fulfill({
      status: 403,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'refused: this refresh came from elsewhere, which is not this portal.' }),
    }));
    await open(page);
    await page.locator('#refresh-index').click();
    await expect(page.locator('#refresh-status')).toContainText('Refused: this refresh came from elsewhere');
    await expect(page.locator('#refresh-status')).toContainText('The index was left as it was');
  });
});
