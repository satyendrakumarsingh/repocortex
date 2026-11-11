import {
  test, expect, cold, settle, assertClean, assertNoPlaceholders, bodyText, horizontalOverflow,
} from '../support/fixtures.mjs';
import { REPOS } from '../support/ports.mjs';

/*
  THE RAIL IS GROUPED BY WHAT THE READER WANTS TO KNOW.

  It used to be grouped by which internal faculty answers — CodeMind, DBMind,
  TestMind — names nobody arrives knowing. The headings are now questions a
  reader brings, the screen names say what the screen answers, and Index
  health sits alone at the top because its verdict qualifies every other
  screen. The ids under each heading are the stable part: deep links, SCREENS
  and LOADERS do not change.

  This file asserts the structure a reader actually meets: Index health first,
  then the group headings in order; every entry reaching its own screen and
  saying so in the URL; and — for the ONE screen this client has no payload
  renderer for — that it states what it answers, why it is empty for THIS
  index, and a command that fills it, rather than a blank panel or a number
  nothing measured.

  Nineteen of the twenty entries render their own endpoint, and what each of
  those must show is asserted in specs/17-pillar-screens.spec.mjs. `agent` is
  the one that stays a guidance panel, and it is not a gap in the build: the
  MCP surface is a separate PROCESS an agent speaks to over stdio, so there is
  no /api/agent to render and a transcript this portal invented would be the
  least honest thing in the product.
*/
const GROUPS = [
  ['', ['overview']],
  ['Understand the code', ['graph', 'search', 'metrics']],
  ['Before you change something', ['planchange', 'blast', 'contract', 'verify', 'ownership', 'diff']],
  ['How it runs', ['crossstack', 'tables', 'events', 'config', 'trace', 'estate']],
  ['Quality', ['tests', 'missed', 'governance', 'deps', 'accuracy']],
  ['For your AI agent', ['agent', 'retrieval']],
];
const PILLARS = GROUPS.filter(([name]) => name !== '');

const ALL = GROUPS.flatMap(([, screens]) => screens);

/*
  The screens this client renders a guidance panel for rather than a fetched
  payload. It used to be ten; the other nine now fetch their own endpoint. This
  one has no endpoint by design — see the file comment.
*/
const GUIDANCE = ['agent'];

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];

  test.describe(`${repo.name} — pillar rail`, () => {
    test("the rail is Index health, then five groups in the reader's words", async ({ page }) => {
      await cold(page, repo);
      // textContent, not innerText: the stylesheet renders these uppercase, and
      // innerText would compare the CSS transform rather than the copy.
      const headings = await page.locator('nav.rail .rail-label')
        .evaluateAll((nodes) => nodes.map((n) => n.textContent));
      expect(headings.map((h) => h.trim()), 'the rail group headings, in order')
        .toEqual(PILLARS.map(([name]) => name));

      expect(await page.locator('nav.rail .nav').first().getAttribute('data-screen'),
        'Index health is the first entry, above every group').toBe('overview');

      // Every button sits after its own heading and before the next one; the
      // top entry sits under no heading at all, and is accounted for first.
      const order = await page.evaluate(() => {
        const out = [];
        for (const node of document.querySelectorAll('nav.rail .rail-label, nav.rail .nav')) {
          out.push(node.classList.contains('rail-label')
            ? { group: node.textContent.trim() }
            : { screen: node.dataset.screen });
        }
        return out;
      });
      const rebuilt = [];
      let current = ['', []];
      rebuilt.push(current);
      for (const row of order) {
        if (row.group) { current = [row.group, []]; rebuilt.push(current); continue; }
        current[1].push(row.screen);
      }
      expect(rebuilt, 'the rail structure').toEqual(GROUPS);
    });

    test('every one of the twenty-three entries reaches its screen and writes ?s= into the URL', async ({ page }) => {
      await cold(page, repo);
      for (const screen of ALL) {
        await page.locator(`.nav[data-screen="${screen}"]`).click();
        await settle(page, 45_000);
        await expect(page.locator(`#screen-${screen}`), `#screen-${screen} visible`).toBeVisible();
        const s = new URL(page.url()).searchParams.get('s') || 'overview';
        expect(s, `?s= after clicking ${screen}`).toBe(screen);
        await expect(page.locator(`.nav[data-screen="${screen}"]`)).toHaveAttribute('aria-current', 'page');
        const visible = await page.evaluate(() =>
          [...document.querySelectorAll('section.screen')].filter((x) => !x.hidden).map((x) => x.id));
        expect(visible, `exactly one screen is visible on ${screen}`).toEqual([`screen-${screen}`]);
      }
      assertClean(page, `${repo.name} pillar rail walk`);
    });

    test('every rail entry is keyboard reachable, in rail order, with a focus ring', async ({ page }) => {
      await cold(page, repo);
      const seen = await page.evaluate(() => {
        const out = [];
        for (const btn of document.querySelectorAll('nav.rail .nav')) {
          btn.focus();
          if (document.activeElement === btn) out.push(btn.dataset.screen);
        }
        return out;
      });
      expect(seen, 'every entry takes focus, in rail order').toEqual(ALL);

      // The focus ring is the stylesheet's, and this asserts it still declares
      // one that applies to the rail rather than that a button happens to have
      // a browser default. getComputedStyle cannot evaluate a pseudo-CLASS, so
      // the rule is read out of the sheet instead of guessed from the element.
      const hasRing = await page.evaluate(() => {
        for (const sheet of document.styleSheets) {
          let rules;
          try { rules = sheet.cssRules; } catch (e) { continue; }
          for (const rule of rules) {
            if (rule.selectorText && rule.selectorText.includes(':focus-visible')
              && rule.style && rule.style.outline) return true;
          }
        }
        return false;
      });
      expect(hasRing, 'the stylesheet declares a visible focus ring').toBe(true);
      expect(await page.locator('nav.rail .nav').first().evaluate((n) => n.tagName),
        'rail entries are real buttons, not divs').toBe('BUTTON');
    });

    test('the twenty-three-entry rail still scrolls as one row at 375x812 without widening the page', async ({ page }) => {
      await page.setViewportSize({ width: 375, height: 812 });
      await cold(page, repo);
      const rail = page.locator('nav.rail');
      const box = await rail.boundingBox();
      expect(box.width, 'the rail does not eat the small viewport').toBeLessThanOrEqual(375);
      for (const screen of ALL) {
        const btn = page.locator(`.nav[data-screen="${screen}"]`);
        await btn.scrollIntoViewIfNeeded();
        await expect(btn, `${screen} is reachable at 375px`).toBeVisible();
      }
      const o = await horizontalOverflow(page);
      expect(o.scrollWidth, `the rail widened the document: ${o.culprits.join(', ')}`)
        .toBeLessThanOrEqual(o.clientWidth + 1);
    });
  });

  test.describe(`${repo.name} — the pillar with no endpoint`, () => {
    test('each states what it answers, why it is empty here, and the command that fills it', async ({ page }) => {
      for (const screen of GUIDANCE) {
        await cold(page, repo, `?s=${screen}`);
        await settle(page, 45_000);
        const host = page.locator(`#screen-${screen}`);
        const body = (await host.innerText());

        expect(body, `${screen} says what the pillar answers`).toContain('What this pillar answers');
        expect(body, `${screen} says why it is empty for this index`).toContain('Why this screen is empty for this index');
        expect(await host.locator('code').count(), `${screen} names a real command`).toBeGreaterThan(0);
        const command = (await host.locator('.held code').first().innerText()).trim();
        expect(command, `${screen}'s command is a cortex subcommand`).toMatch(/^cortex /);

        // What THIS index holds towards the pillar, as real counts.
        expect(await host.locator('.kpi').count(), `${screen} states what the index holds`).toBeGreaterThan(0);

        // Never a blank panel, never an invented number.
        assertNoPlaceholders(await bodyText(page), `${repo.name} ?s=${screen}`);
        expect(body.length, `${screen} is not a blank panel`).toBeGreaterThan(400);
        assertClean(page, `${repo.name} ?s=${screen}`);
      }
    });

    test('no guidance screen issues a request that 4xx or 5xx', async ({ page }) => {
      for (const screen of GUIDANCE) {
        await cold(page, repo, `?s=${screen}`);
        await settle(page, 45_000);
      }
      expect(page.badResponses, `${repo.name}: a guidance screen made a failing request`).toEqual([]);
    });
  });
}
