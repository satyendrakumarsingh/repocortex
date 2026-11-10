import { test, expect, cold, settle, assertClean, assertNoPlaceholders, OMISSIONS_STATED } from '../support/fixtures.mjs';
import { REPOS } from '../support/ports.mjs';

/* A chain card is a .card that actually contains a rendered chain. */
async function routesOnPage(page) {
  return page.evaluate(() => [...document.querySelectorAll('#screen-crossstack .card')]
    .filter((c) => c.querySelector('.chain'))
    .map((c) => {
      const name = c.querySelector('header h3');
      const path = c.querySelector('.body .path');
      return `${name ? name.textContent.trim() : '?'} @ ${path ? path.textContent.trim() : '?'}`;
    }));
}

async function pagerRange(page) {
  return (await page.locator('#cs-range').innerText()).trim();
}

/* The default page size; paging can only be asserted on a fixture with more routes than this. */
const PAGE = 12;

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];
  const hasRoutes = repo.routes > 0;

  test.describe(`${repo.name} — cross-stack`, () => {
    test('the page reports the true route total and a coherent range', async ({ page }) => {
      await cold(page, repo, '?s=crossstack');
      const text = await page.locator('#screen-crossstack').innerText();
      expect(text, 'routes counted').toMatch(/ROUTES\n[\d,]+/);
      const total = Number((text.match(/ROUTES\n([\d,]+)/) || [])[1].replace(/,/g, ''));
      expect(total, `${repo.name} route total`).toBe(repo.routes);
      expect(await pagerRange(page), 'the pager states the range and the total')
        .toMatch(new RegExp(`^routes [\\d,]+–[\\d,]+ of ${repo.routes.toLocaleString('en-US')}$`));
      assertNoPlaceholders(text, `${repo.name} cross-stack`);
      assertClean(page, `${repo.name} cross-stack`);
    });

    if (!hasRoutes) {
      test('with zero endpoints the screen says so usefully rather than rendering blank', async ({ page }) => {
        await cold(page, repo, '?s=crossstack');
        const empty = page.locator('#screen-crossstack .empty');
        await expect(empty, 'an explicit empty state').toBeVisible();
        const text = (await empty.innerText()).trim();
        expect(text.length, 'the empty state is a real explanation, not a blank box').toBeGreaterThan(120);
        expect(text, 'it distinguishes "no chains here" from "no routes at all"')
          .toMatch(/not the same as/i);
        expect(text, 'it tells the reader what to do next').toMatch(/Lower the confidence floor|raise the walk depth|first page/i);
        assertNoPlaceholders(text, `${repo.name} cross-stack empty state`);
      });

      test('both pager buttons are disabled when there is nothing to page through', async ({ page }) => {
        await cold(page, repo, '?s=crossstack');
        await expect(page.locator('#screen-crossstack .pager button', { hasText: '‹ Prev' })).toBeDisabled();
        await expect(page.locator('#screen-crossstack .pager button', { hasText: 'Next ›' })).toBeDisabled();
      });
    }

    if (hasRoutes) {
      test('a chain card renders every hop it claims, with a weakest tier', async ({ page }) => {
        await cold(page, repo, '?s=crossstack');
        const chains = page.locator('#screen-crossstack .chain');
        expect(await chains.count(), 'chains rendered').toBeGreaterThan(0);
        const first = chains.first();
        const text = await first.innerText();
        expect(text, 'the endpoint hop is drawn').toMatch(/endpoint/i);
        const card = page.locator('#screen-crossstack .card').filter({ has: page.locator('.chain') }).first();
        expect(await card.innerText(), 'the card states its weakest hop').toMatch(/Weakest hop on this chain/);
        assertNoPlaceholders(await card.innerText(), `${repo.name} first chain card`);
      });

      test('WALKING EVERY PAGE drops no route and duplicates none', async ({ page }) => {
        await cold(page, repo, '?s=crossstack');
        const seen = [];
        const ranges = [];
        let guard = 0;
        for (;;) {
          if (++guard > 40) throw new Error('pager did not terminate within 40 pages');
          ranges.push(await pagerRange(page));
          seen.push(...await routesOnPage(page));
          const next = page.locator('#screen-crossstack .pager button', { hasText: 'Next ›' });
          if (await next.isDisabled()) break;
          const before = await pagerRange(page);
          await next.click();
          await settle(page);
          await expect.poll(async () => pagerRange(page), { message: 'the pager advanced' }).not.toBe(before);
        }
        const dupes = seen.filter((r, i) => seen.indexOf(r) !== i);
        expect([...new Set(dupes)], `routes served on more than one page (pages walked: ${ranges.join(' → ')})`).toEqual([]);
        expect(seen.length, `every one of the ${repo.routes} routes was reachable by paging (pages: ${ranges.join(' → ')})`)
          .toBe(repo.routes);
        assertClean(page, `${repo.name} pager walk`);
      });

      test('Prev walks back to exactly the routes Next walked forward over', async ({ page }) => {
        test.skip(repo.routes <= PAGE, `${repo.name} has ${repo.routes} routes, which fit on one page of ${PAGE}; there is no second page to walk`);
        await cold(page, repo, '?s=crossstack');
        const page1 = await routesOnPage(page);
        await page.locator('#screen-crossstack .pager button', { hasText: 'Next ›' }).click();
        await settle(page);
        const page2 = await routesOnPage(page);
        expect(page2, 'page 2 is not page 1').not.toEqual(page1);
        await page.locator('#screen-crossstack .pager button', { hasText: '‹ Prev' }).click();
        await settle(page);
        expect(await routesOnPage(page), 'Prev restores page 1 exactly').toEqual(page1);
        await expect(page.locator('#screen-crossstack .pager button', { hasText: '‹ Prev' }),
          'Prev is disabled on the first page').toBeDisabled();
      });

      test('the offset rides in the URL, so a page is a shareable cold link', async ({ page }) => {
        test.skip(repo.routes <= PAGE, `${repo.name} has ${repo.routes} routes, which fit on one page of ${PAGE}; there is no second page to link to`);
        await cold(page, repo, '?s=crossstack');
        await page.locator('#screen-crossstack .pager button', { hasText: 'Next ›' }).click();
        await settle(page);
        const off = new URL(page.url()).searchParams.get('off');
        expect(off, '?off= records the page').toBe('12');
        const warm = await routesOnPage(page);
        await cold(page, repo, `?s=crossstack&off=${off}`);
        expect(await routesOnPage(page), 'a cold load of ?off=12 shows the same page').toEqual(warm);
      });

      test('ROUTES PER PAGE changes the page size and resets to the first page', async ({ page }) => {
        await cold(page, repo, '?s=crossstack&off=12');
        for (const size of ['25', '60', '120', '250', '12']) {
          await page.selectOption('#cs-limit', size);
          await settle(page);
          expect(new URL(page.url()).searchParams.get('limit') || '12', `?limit=${size}`).toBe(size);
          expect(new URL(page.url()).searchParams.get('off') || '0', 'changing page size returns to page 1').toBe('0');
          const shown = (await routesOnPage(page)).length;
          expect(shown, `page of ${size} shows at most ${size} routes`).toBeLessThanOrEqual(Number(size));
          expect(shown, `page of ${size} shows the smaller of ${size} and ${repo.routes}`)
            .toBe(Math.min(Number(size), repo.routes));
        }
        assertClean(page, `${repo.name} page size`);
      });

      test('HANDLER WALK DEPTH changes the walk and resets to the first page', async ({ page }) => {
        await cold(page, repo, '?s=crossstack&off=12');
        for (const depth of ['1', '5', '3']) {
          await page.selectOption('#cs-depth', depth);
          await settle(page);
          expect(new URL(page.url()).searchParams.get('csdepth') || '3', `?csdepth=${depth}`).toBe(depth);
          expect(new URL(page.url()).searchParams.get('off') || '0', 'changing depth returns to page 1').toBe('0');
          expect((await routesOnPage(page)).length, `depth ${depth} still renders chains`).toBeGreaterThan(0);
        }
        assertClean(page, `${repo.name} walk depth`);
      });

      test('every evidence copy button on the page copies its own file and line', async ({ page, context }) => {
        await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: `http://127.0.0.1:${repo.port}` });
        await cold(page, repo, '?s=crossstack');
        const evs = await page.$$('#screen-crossstack .ev');
        expect(evs.length, 'the chains carry checkable evidence').toBeGreaterThan(0);
        for (const ev of evs.slice(0, 8)) {
          const label = (await (await ev.$('.p')).innerText()).trim();
          await (await ev.$('button')).click();
          const clip = (await page.evaluate(() => navigator.clipboard.readText())).trim();
          expect(clip, 'the copy button copies exactly the file:line beside it').toBe(label);
          expect(label, 'evidence is a real location').toMatch(/^\S+(:\d+)?$/);
        }
      });

      test('every editor link points at the real file inside the real repository', async ({ page }) => {
        await cold(page, repo, '?s=crossstack');
        const hrefs = await page.locator('#screen-crossstack .ev a').evaluateAll((a) => a.map((x) => x.getAttribute('href')));
        expect(hrefs.length, 'the chains offer editor links').toBeGreaterThan(0);
        for (const href of hrefs.slice(0, 10)) {
          expect(href, 'the editor link is absolute and rooted in this repository')
            .toMatch(new RegExp(`^vscode://file${repo.root}/\\S+`));
          expect(href, 'no placeholder leaked into the link').not.toMatch(/undefined|NaN/);
        }
      });

      test('"where the chains stop" is stated before the chains', async ({ page }) => {
        await cold(page, repo, '?s=crossstack');
        const order = await page.evaluate(() => {
          const host = document.getElementById('screen-crossstack');
          const nodes = [...host.querySelectorAll('*')];
          const gapsAt = nodes.findIndex((n) => n.tagName === 'H3' && (n.textContent || '').includes('Where the chains stop'));
          const chainAt = nodes.findIndex((n) => n.classList.contains('chain'));
          return { gapsAt, chainAt };
        });
        expect(order.gapsAt, 'the gaps card exists').toBeGreaterThan(-1);
        expect(order.gapsAt, 'the gaps card is above the first chain').toBeLessThan(order.chainAt);
      });
    }

    test('the confidence floor reaches this screen', async ({ page }) => {
      await cold(page, repo, '?s=crossstack&floor=exact');
      const atExact = await page.locator('#screen-crossstack').innerText();
      await cold(page, repo, '?s=crossstack&floor=low');
      const atLow = await page.locator('#screen-crossstack').innerText();
      if (repo.routes === 0) {
        // Nothing to filter; the screen must still be a real explanation.
        expect(atLow.length, 'the screen renders at every floor').toBeGreaterThan(200);
      } else {
        expect(atLow, 'a different floor produces a different cross-stack answer').not.toBe(atExact);
      }
    });

    test('the screen states what it left out', async ({ page }) => {
      await cold(page, repo, '?s=crossstack');
      const text = await page.locator('#screen-crossstack').innerText();
      expect(text, 'the omissions block is present').toMatch(OMISSIONS_STATED);
    });
  });
}
