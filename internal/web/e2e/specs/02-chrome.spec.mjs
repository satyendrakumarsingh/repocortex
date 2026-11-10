import { test, expect, cold, settle, assertClean } from '../support/fixtures.mjs';
import { REPOS, SCREENS } from '../support/ports.mjs';

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];

  test.describe(`${repo.name} — persistent chrome`, () => {
    test('the index bar states the run verdict and its facts', async ({ page }) => {
      await cold(page, repo);
      const bar = page.locator('#indexbar');
      await expect(bar.locator('.runbadge')).toBeVisible();
      const badge = (await bar.locator('.runbadge').innerText()).trim();
      expect(badge, 'run badge names the index status').toMatch(/^index \S+/);
      const facts = (await bar.locator('.facts').innerText()).trim();
      expect(facts, 'index bar states when it was indexed').toMatch(/indexed .+ ago/);
      if (repo.name === 'apm') {
        expect(facts, 'apm index is DEGRADED with 37 parse failures — the bar must say so')
          .toMatch(/37 files failed to parse/);
      }
      assertClean(page, `${repo.name} index bar`);
    });

    test('the run badge jumps to the overview freshness card', async ({ page }) => {
      await cold(page, repo, '?s=verify');
      await page.locator('#indexbar .runbadge').click();
      await settle(page);
      await expect(page.locator('#screen-overview')).toBeVisible();
      await expect(page.locator('#freshness')).toBeVisible();
    });

    test('the four confidence tiers are a working filter, and the floor rides in the URL', async ({ page }) => {
      await cold(page, repo);
      const tiers = page.locator('#indexbar .tierbar button');
      await expect(tiers, 'four tier chips').toHaveCount(4);
      for (const tier of ['exact', 'high', 'medium', 'low']) {
        await page.locator(`#indexbar .tierbar button.t-${tier}`).click();
        await settle(page);
        const floor = new URL(page.url()).searchParams.get('floor') || 'medium';
        expect(floor, `?floor= after clicking ${tier}`).toBe(tier);
        await expect(page.locator(`#indexbar .tierbar button.t-${tier}`), `${tier} chip pressed`)
          .toHaveAttribute('aria-pressed', 'true');
      }
      assertClean(page, `${repo.name} tier bar`);
    });

    test('the confidence floor actually changes what the graph draws', async ({ page }) => {
      // The product's claim is that the tier bar "moves the floor on every
      // screen". The graph is where the floor is a filter, so that is where it
      // is asserted: a different floor must produce a different drawn set.
      await cold(page, repo, '?s=graph&floor=exact');
      const atExact = (await page.locator('#canvas-note').innerText()).trim();
      await cold(page, repo, '?s=graph&floor=low');
      const atLow = (await page.locator('#canvas-note').innerText()).trim();
      expect(atLow, `the floor is not decorative: exact drew "${atExact}", low drew "${atLow}"`)
        .not.toBe(atExact);
    });

    test('the theme toggle cycles system → light → dark and persists across a reload', async ({ page }) => {
      await cold(page, repo);
      const toggle = page.locator('#theme-toggle');
      await expect(toggle).toHaveText('Theme: system');
      await toggle.click();
      await expect(toggle).toHaveText('Theme: light');
      await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
      await toggle.click();
      await expect(toggle).toHaveText('Theme: dark');
      await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
      const darkBg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
      await toggle.click();
      await expect(toggle).toHaveText('Theme: system');
      await page.reload({ waitUntil: 'domcontentloaded' });
      await settle(page);
      await expect(page.locator('#theme-toggle'), 'theme survives a reload').toHaveText('Theme: system');
      await page.locator('#theme-toggle').click();
      await page.locator('#theme-toggle').click();
      const darkBgAgain = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
      expect(darkBgAgain, 'dark theme repaints the body').toBe(darkBg);
      assertClean(page, `${repo.name} theme`);
    });

    test('Copy link copies the current screen, filters and all', async ({ page, context }) => {
      await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: `http://127.0.0.1:${repo.port}` });
      await cold(page, repo, '?s=crossstack&floor=low&csdepth=4');
      const share = page.locator('#indexbar button[title="This page, with its filters and selection, as a URL"]');
      await share.click();
      await expect(share, 'the button confirms the copy').toHaveText('Link copied');
      const copied = await page.evaluate(() => navigator.clipboard.readText());
      expect(copied, 'clipboard holds the screen').toContain('s=crossstack');
      expect(copied, 'clipboard holds the confidence floor').toContain('floor=low');
      expect(copied, 'clipboard holds the walk depth').toContain('csdepth=4');
      await expect(share, 'the button reverts').toHaveText('Copy link', { timeout: 4000 });
    });

    test('the footer states the project, run and file count on every screen', async ({ page }) => {
      for (const screen of SCREENS) {
        await cold(page, repo, `?s=${screen}`);
        const foot = (await page.locator('#foot-meta').innerText()).trim();
        expect(foot, `footer on ${screen}`).toMatch(/^.+ · run \d+ · [\d,]+ files indexed · read-only$/);
        expect(foot, `footer names the project on ${screen}`).toContain(repo.name);
      }
    });

    test('the rail states the index path it is reading', async ({ page }) => {
      await cold(page, repo);
      const path = (await page.locator('#index-path').innerText()).trim();
      expect(path, 'rail shows the index file').toBe(`${repo.root}/.cortex/index.db`);
      await expect(page.locator('#project')).toHaveText(repo.name);
    });
  });
}
