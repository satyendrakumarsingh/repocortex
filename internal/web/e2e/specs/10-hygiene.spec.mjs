import { test, expect, cold, settle, bodyText, assertNoPlaceholders, horizontalOverflow, assertClean } from '../support/fixtures.mjs';
import { REPOS, SCREENS, intentFor, base } from '../support/ports.mjs';

/*
  The five failure modes the brief names, asserted across every screen of both
  repositories. These are deliberately separate from the per-screen specs: a
  screen can do its job and still leak "undefined" into one cell.
*/

// Screens are visited with the query that makes them render their fullest
// state, so the checks run over populated views rather than empty prompts.
const fullState = (repo) => ({
  overview: '?s=overview',
  graph: '?s=graph',
  crossstack: '?s=crossstack',
  blast: `?s=blast&sym=${encodeURIComponent(repo.resolvedSymbol)}`,
  contract: `?s=contract&intent=${encodeURIComponent(intentFor(repo))}`,
  verify: `?s=verify&intent=${encodeURIComponent(intentFor(repo))}&changed=${encodeURIComponent(repo.inScopeChanged)}`,
  governance: '?s=governance',
});

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];
  const states = fullState(repo);

  test.describe(`${repo.name} — hygiene`, () => {
    test('FAILURE MODE 2 — "undefined", "NaN" and "[object Object]" appear on no screen', async ({ page }) => {
      for (const screen of SCREENS) {
        await cold(page, repo, states[screen]);
        await settle(page, 45_000);
        assertNoPlaceholders(await bodyText(page), `${repo.name} ?s=${screen}`);
      }
    });

    test('FAILURE MODE 5 — no console error and no page exception on any screen', async ({ page }) => {
      for (const screen of SCREENS) {
        await cold(page, repo, states[screen]);
        await settle(page, 45_000);
        assertClean(page, `${repo.name} ?s=${screen}`);
      }
    });

    test('FAILURE MODE 4 — no horizontal body scroll at 1440x900', async ({ page }) => {
      await page.setViewportSize({ width: 1440, height: 900 });
      for (const screen of SCREENS) {
        await cold(page, repo, states[screen]);
        await settle(page, 45_000);
        const o = await horizontalOverflow(page);
        expect(o.scrollWidth,
          `${repo.name} ?s=${screen} at 1440x900 scrolls horizontally (${o.scrollWidth} > ${o.clientWidth}); culprits: ${o.culprits.join(', ')}`)
          .toBeLessThanOrEqual(o.clientWidth + 1);
      }
    });

    test('FAILURE MODE 4 — no horizontal body scroll at 375x812', async ({ page }) => {
      await page.setViewportSize({ width: 375, height: 812 });
      for (const screen of SCREENS) {
        await cold(page, repo, states[screen]);
        await settle(page, 45_000);
        const o = await horizontalOverflow(page);
        expect(o.scrollWidth,
          `${repo.name} ?s=${screen} at 375x812 scrolls horizontally (${o.scrollWidth} > ${o.clientWidth}); culprits: ${o.culprits.join(', ')}`)
          .toBeLessThanOrEqual(o.clientWidth + 1);
      }
    });

    test('FAILURE MODE 3 — no screen quietly issues a request that 4xx or 5xx', async ({ page }) => {
      for (const screen of SCREENS) {
        await cold(page, repo, states[screen]);
        await settle(page, 45_000);
      }
      expect(page.badResponses,
        `${repo.name}: these requests failed while the UI showed no error`).toEqual([]);
    });

    test('the rail is reachable and readable at 375x812', async ({ page }) => {
      await page.setViewportSize({ width: 375, height: 812 });
      await cold(page, repo);
      const rail = page.locator('nav.rail');
      await expect(rail).toBeVisible();
      const box = await rail.boundingBox();
      expect(box.width, 'the rail does not eat the small viewport').toBeLessThanOrEqual(375);
      for (const screen of SCREENS) {
        const btn = page.locator(`.nav[data-screen="${screen}"]`);
        await btn.scrollIntoViewIfNeeded();
        await expect(btn, `${screen} is reachable at 375px`).toBeVisible();
      }
    });

    test('the skip link is the first focusable thing and targets the main region', async ({ page }) => {
      await cold(page, repo);
      await page.keyboard.press('Tab');
      const focused = await page.evaluate(() => {
        const a = document.activeElement;
        return { tag: a.tagName, cls: a.className, href: a.getAttribute('href'), text: a.textContent.trim() };
      });
      expect(focused.cls, 'the first tab stop is the skip link').toContain('skip');
      expect(focused.href, 'it targets #main').toBe('#main');
      await expect(page.locator('#main'), 'the target exists').toBeVisible();
    });

    test('the graph canvas carries a text equivalent for a screen reader', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const label = await page.locator('#graph-canvas').getAttribute('aria-label');
      expect(label, 'the canvas is labelled').toBeTruthy();
      expect(label.length, 'the label is a sentence, not a word').toBeGreaterThan(40);
      await expect(page.locator('#graph-canvas'), 'the canvas declares itself an image').toHaveAttribute('role', 'img');
      expect((await page.locator('#graph-table').innerText()).trim().length,
        'the promised text equivalent is actually rendered').toBeGreaterThan(100);
    });

    test('the portal declares itself read-only and refuses non-loopback Hosts', async ({ page, request }) => {
      await cold(page, repo);
      await expect(page.locator('#scope-note')).toHaveText(/read-only/);
      const res = await request.get(`${base(repo)}/api/health`, { headers: { Host: 'evil.example.com' } });
      expect(res.status(), 'a non-loopback Host is refused').toBeGreaterThanOrEqual(400);
    });
  });
}
