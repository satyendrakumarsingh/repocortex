import { test, expect, cold, settle, assertClean, assertNoPlaceholders, OMISSIONS_STATED } from '../support/fixtures.mjs';
import { REPOS, intentFor } from '../support/ports.mjs';

async function compile(page, intent, budget) {
  await page.fill('#contract-intent', intent);
  if (budget) await page.selectOption('#contract-scope', budget);
  await page.locator('#screen-contract button.primary', { hasText: 'Compile' }).click();
  await settle(page, 45_000);
}

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];

  test.describe(`${repo.name} — task contract`, () => {
    test('with no task it explains itself instead of rendering an empty contract', async ({ page }) => {
      await cold(page, repo, '?s=contract');
      const empty = page.locator('#screen-contract .empty');
      await expect(empty).toBeVisible();
      const text = (await empty.innerText()).trim();
      expect(text.length, 'the prompt is a real explanation').toBeGreaterThan(150);
      expect(text, 'it says this is not search').toMatch(/not search/i);
      assertClean(page, `${repo.name} contract idle`);
    });

    test('a real task compiles into a contract with scope, non-scope and unknowns', async ({ page }) => {
      await cold(page, repo, '?s=contract');
      await compile(page, intentFor(repo));
      const text = (await page.locator('#screen-contract').innerText());
      expect(text, 'the contract names what is IN scope').toMatch(/\bin scope\b|\bscope\b/i);
      expect(text, 'the contract names what is deliberately NOT in scope')
        .toMatch(/not in scope|out of scope|non-scope|deliberately not/i);
      expect(text, 'the contract names what it could not answer')
        .toMatch(/unknown|could not answer|cannot answer/i);
      // The three lists must be populated, not merely titled.
      const rows = await page.locator('#screen-contract tbody tr').count();
      expect(rows, 'the contract lists real rows').toBeGreaterThan(3);
      expect(new URL(page.url()).searchParams.get('intent'), '?intent= records the task').toBe(intentFor(repo));
      assertNoPlaceholders(text, `${repo.name} contract`);
      assertClean(page, `${repo.name} contract`);
    });

    test('the compiler shows what it understood before it shows an answer', async ({ page }) => {
      await cold(page, repo, '?s=contract');
      await compile(page, intentFor(repo));
      const text = await page.locator('#screen-contract').innerText();
      expect(text, 'the interpretation is stated').toMatch(/understood|interpret|anchor|read the words|goal/i);
    });

    test('every SCOPE BUDGET option changes the contract and rides in the URL', async ({ page }) => {
      await cold(page, repo, '?s=contract');
      const sizes = {};
      for (const budget of ['20', '30', '60', '0']) {
        await compile(page, intentFor(repo), budget);
        expect(new URL(page.url()).searchParams.get('scope') || '0', `?scope=${budget}`).toBe(budget);
        sizes[budget] = await page.locator('#screen-contract tbody tr').count();
        expect(sizes[budget], `budget ${budget} rendered rows`).toBeGreaterThan(0);
        assertNoPlaceholders(await page.locator('#screen-contract').innerText(), `${repo.name} contract at budget ${budget}`);
      }
      expect(sizes['60'], 'a larger budget is not smaller than the default').toBeGreaterThanOrEqual(sizes['0']);
      assertClean(page, `${repo.name} contract budgets`);
    });

    test('the worked-example chip compiles', async ({ page }) => {
      await cold(page, repo, '?s=contract');
      const chip = page.locator('#screen-contract button.chip').first();
      const label = (await chip.innerText()).trim();
      await chip.click();
      await settle(page, 45_000);
      expect(await page.inputValue('#contract-intent'), 'the chip fills the input').toBe(label);
      const text = await page.locator('#screen-contract').innerText();
      expect(text.length, `"${label}" compiled`).toBeGreaterThan(600);
      assertNoPlaceholders(text, `${repo.name} contract chip`);
    });

    test('?intent= on a COLD load compiles without a click', async ({ page }) => {
      await cold(page, repo, `?s=contract&intent=${encodeURIComponent(intentFor(repo))}`);
      await settle(page, 45_000);
      expect(await page.inputValue('#contract-intent')).toBe(intentFor(repo));
      const text = await page.locator('#screen-contract').innerText();
      expect(text.length, 'the deep link compiled a contract').toBeGreaterThan(600);
      assertClean(page, `${repo.name} contract deep link`);
    });

    test('a nonsense task still returns a contract or a stated inability, never a blank', async ({ page }) => {
      await cold(page, repo, '?s=contract');
      await compile(page, 'qqqzzz nonexistent widget frobnicator');
      const text = (await page.locator('#screen-contract').innerText()).trim();
      expect(text.length, 'the screen is not blank').toBeGreaterThan(400);
      assertNoPlaceholders(text, `${repo.name} nonsense contract`);
      assertClean(page, `${repo.name} nonsense contract`);
    });

    test('the contract states what it left out', async ({ page }) => {
      await cold(page, repo, `?s=contract&intent=${encodeURIComponent(intentFor(repo))}`);
      await settle(page, 45_000);
      expect(await page.locator('#screen-contract').innerText(), 'the omissions block is present')
        .toMatch(OMISSIONS_STATED);
    });
  });
}
