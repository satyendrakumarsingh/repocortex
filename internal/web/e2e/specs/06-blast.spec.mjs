import { test, expect, cold, settle, assertClean, assertNoPlaceholders, OMISSIONS_STATED } from '../support/fixtures.mjs';
import { REPOS } from '../support/ports.mjs';

async function blastBody(page) {
  return (await page.locator('#screen-blast').innerText()).trim();
}

async function analyse(page, symbol, depth) {
  await page.fill('#blast-symbol', symbol);
  if (depth) await page.selectOption('#blast-depth', depth);
  await page.locator('#screen-blast button.primary', { hasText: 'Analyse' }).click();
  await settle(page, 40_000);
}

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];

  test.describe(`${repo.name} — blast radius`, () => {
    test('with no symbol it asks for one instead of rendering an empty report', async ({ page }) => {
      await cold(page, repo, '?s=blast');
      const empty = page.locator('#screen-blast .empty');
      await expect(empty).toBeVisible();
      const text = (await empty.innerText()).trim();
      expect(text.length, 'the prompt is a real explanation').toBeGreaterThan(150);
      expect(text, 'it says why a bare name is not enough').toMatch(/matches several definitions|which thing/i);
      assertNoPlaceholders(text, `${repo.name} blast prompt`);
      assertClean(page, `${repo.name} blast idle`);
    });

    test('a resolved symbol produces a report with a risk verdict and completeness', async ({ page }) => {
      await cold(page, repo, '?s=blast');
      await analyse(page, repo.resolvedSymbol);
      const text = await blastBody(page);
      expect(text, 'the report names a risk level').toMatch(/risk/i);
      expect(text.length, 'the report is substantial').toBeGreaterThan(400);
      expect(new URL(page.url()).searchParams.get('sym'), '?sym= records the analysed symbol')
        .toBe(repo.resolvedSymbol);
      assertNoPlaceholders(text, `${repo.name} blast report for ${repo.resolvedSymbol}`);
      assertClean(page, `${repo.name} blast report`);
    });

    test('an ambiguous name returns candidates rather than guessing, and each is clickable', async ({ page }) => {
      await cold(page, repo, '?s=blast');
      await analyse(page, repo.ambiguousSymbol);
      const text = await blastBody(page);
      expect(text, 'the screen says the name is ambiguous').toMatch(/matches|candidate|ambiguous/i);
      const rows = page.locator('#screen-blast tbody tr');
      const n = await rows.count();
      expect(n, `candidates listed for "${repo.ambiguousSymbol}"`).toBeGreaterThan(1);
      assertNoPlaceholders(text, `${repo.name} blast candidates`);
      // Choosing one must resolve to a single target.
      const chooser = page.locator('#screen-blast tbody tr button', { hasText: 'Analyse this one' }).first();
      expect(await chooser.count(), 'each candidate offers a way to analyse it').toBeGreaterThan(0);
      {
        await chooser.click();
        await settle(page, 40_000);
        const after = await blastBody(page);
        expect(after, 'choosing a candidate produced a different view').not.toBe(text);
        assertNoPlaceholders(after, `${repo.name} blast after choosing a candidate`);
      }
      assertClean(page, `${repo.name} blast ambiguity`);
    });

    test('a name the index does not hold says so, and does not render a false empty report', async ({ page }) => {
      await cold(page, repo, '?s=blast');
      await analyse(page, 'ThisSymbolDoesNotExistAnywhere_zzz');
      const text = await blastBody(page);
      expect(text, 'the screen says it found nothing by that name')
        .toMatch(/not found|nothing|no symbol|does not/i);
      assertNoPlaceholders(text, `${repo.name} blast not-found`);
      assertClean(page, `${repo.name} blast not-found`);
    });

    test('every DEPTH option runs and records itself in the URL', async ({ page }) => {
      await cold(page, repo, '?s=blast');
      for (const depth of ['1', '2', '4', '6', '3']) {
        await analyse(page, repo.resolvedSymbol, depth);
        expect(new URL(page.url()).searchParams.get('depth') || '3', `?depth=${depth}`).toBe(depth);
        const text = await blastBody(page);
        expect(text.length, `depth ${depth} rendered a report`).toBeGreaterThan(300);
        assertNoPlaceholders(text, `${repo.name} blast at depth ${depth}`);
      }
      assertClean(page, `${repo.name} blast depths`);
    });

    test('?sym= on a COLD load analyses without a click', async ({ page }) => {
      await cold(page, repo, `?s=blast&sym=${encodeURIComponent(repo.resolvedSymbol)}`);
      await settle(page, 40_000);
      expect(await page.inputValue('#blast-symbol'), 'the input is filled from the URL').toBe(repo.resolvedSymbol);
      const text = await blastBody(page);
      expect(text.length, 'the deep link produced a report').toBeGreaterThan(400);
      assertClean(page, `${repo.name} blast deep link`);
    });

    test('the report states that it is a lower bound and what it could not reach', async ({ page }) => {
      await cold(page, repo, `?s=blast&sym=${encodeURIComponent(repo.resolvedSymbol)}`);
      await settle(page, 40_000);
      const text = await blastBody(page);
      expect(text, 'the lower-bound caveat survives into the report').toMatch(/lower bound|could not|unknown|unresolved/i);
      expect(text, 'the report states what it left out').toMatch(OMISSIONS_STATED);
    });
  });
}
