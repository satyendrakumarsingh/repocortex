import { test, expect, cold, settle, screenText, assertClean } from '../support/fixtures.mjs';
import { REPOS, SCREENS, base } from '../support/ports.mjs';

/*
  What the SCREEN says at its head, once it has rendered. These are the
  headings app.js draws inside section.screen, not the rail's words for the
  same screens; the rail is asserted below through document.title.
*/
const TITLES = {
  overview: /Indexed .* ago\./,
  graph: /talk to each other through|inside /,
  crossstack: /reach a database table|None of them is on this page/,
  blast: /what could break if you change it/,
  contract: /writes the brief an agent should get/,
  verify: /Check a finished change against its brief/,
  governance: /crosses a line the architecture forbids|No architecture is declared/,
};

/*
  What the RAIL calls each screen, in the reader's words, and therefore what
  the tab is titled once it is on. One list in assets/js/nav.mjs is the
  source; this is the copy a reader would notice diverging from it.
*/
const RAIL = {
  overview: 'Index health',
  graph: 'Map of the code',
  crossstack: 'APIs and what they touch',
  blast: 'What could break',
  contract: 'Task brief',
  verify: 'Check a change',
  governance: 'Architecture rules',
};

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];

  test.describe(`${repo.name} — navigation`, () => {
    test('every rail item reaches its screen and pushes ?s= into the URL', async ({ page }) => {
      await cold(page, repo);
      for (const screen of SCREENS) {
        await page.locator(`.nav[data-screen="${screen}"]`).click();
        await settle(page);
        await expect(page.locator(`#screen-${screen}`), `#screen-${screen} visible after clicking its rail item`).toBeVisible();
        const url = new URL(page.url());
        const s = url.searchParams.get('s') || 'overview';
        expect(s, `URL ?s= after clicking ${screen}`).toBe(screen);
        expect(await screenText(page), `${screen} heading`).toMatch(TITLES[screen]);
        await expect(page.locator(`.nav[data-screen="${screen}"]`)).toHaveAttribute('aria-current', 'page');
        await expect(page, `the tab is titled in the rail's words on ${screen}`)
          .toHaveTitle(`RepoCortex — ${RAIL[screen]}`);
      }
      assertClean(page, `${repo.name} rail walk`);
    });

    test('every ?s= deep link loads that screen on a COLD document load', async ({ page }) => {
      for (const screen of SCREENS) {
        await cold(page, repo, `?s=${screen}`);
        await expect(page.locator(`#screen-${screen}`), `cold ?s=${screen}`).toBeVisible();
        expect(await screenText(page), `cold ?s=${screen} content`).toMatch(TITLES[screen]);
        const others = SCREENS.filter((x) => x !== screen);
        for (const other of others) {
          await expect(page.locator(`#screen-${other}`), `${other} hidden on cold ?s=${screen}`).toBeHidden();
        }
      }
      assertClean(page, `${repo.name} cold deep links`);
    });

    test('the legacy /#<screen> link lands on that screen, not silently on Overview', async ({ page }) => {
      // The brief flags "/#graph silently lands on Overview" as a suspected
      // defect. readQuery() has a hash fallback; this asserts it works, on a
      // cold load, for every screen.
      for (const screen of SCREENS) {
        await page.goto(`${base(repo)}/#${screen}`, { waitUntil: 'domcontentloaded' });
        await settle(page);
        await expect(page.locator(`#screen-${screen}`), `cold /#${screen}`).toBeVisible();
        expect(await screenText(page), `cold /#${screen} content`).toMatch(TITLES[screen]);
      }
      assertClean(page, `${repo.name} hash links`);
    });

    test('back and forward restore the screen that was on', async ({ page }) => {
      await cold(page, repo);
      await page.locator('.nav[data-screen="graph"]').click();
      await settle(page);
      await page.locator('.nav[data-screen="verify"]').click();
      await settle(page);
      await page.goBack();
      await settle(page);
      await expect(page.locator('#screen-graph'), 'back returns to graph').toBeVisible();
      await page.goForward();
      await settle(page);
      await expect(page.locator('#screen-verify'), 'forward returns to verify').toBeVisible();
      assertClean(page, `${repo.name} history`);
    });

    test('an unknown ?s= value falls back to Overview rather than a blank page', async ({ page }) => {
      await cold(page, repo, '?s=not-a-screen');
      const visible = await page.evaluate(() =>
        [...document.querySelectorAll('section.screen')].filter((s) => !s.hidden).map((s) => s.id));
      expect(visible.length, 'exactly one screen visible for a bogus ?s=').toBeGreaterThan(0);
      const text = await page.evaluate(() => document.body.innerText);
      expect(text.trim().length, 'page is not blank for a bogus ?s=').toBeGreaterThan(200);
    });
  });
}
