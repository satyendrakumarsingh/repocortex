import { test, expect, cold, settle, assertClean, assertNoPlaceholders, OMISSIONS_STATED } from '../support/fixtures.mjs';
import { REPOS } from '../support/ports.mjs';

const FLOORS = ['', 'exact', 'high', 'medium', 'low'];

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];

  test.describe(`${repo.name} — architecture governance`, () => {
    test('the screen renders and never leaves the reader with a blank panel', async ({ page }) => {
      await cold(page, repo, '?s=governance');
      const text = (await page.locator('#screen-governance').innerText()).trim();
      expect(text.length, 'the screen has real content').toBeGreaterThan(300);
      expect(text, 'the screen names itself').toMatch(/crosses a line the architecture forbids|No architecture is declared/);
      assertNoPlaceholders(text, `${repo.name} governance`);
      assertClean(page, `${repo.name} governance`);
    });

    test('with no declared architecture it says so, and says what that is NOT', async ({ page }) => {
      await cold(page, repo, '?s=governance');
      const text = await page.locator('#screen-governance').innerText();
      const declared = !/No architecture is declared/.test(text);
      if (!declared) {
        const empty = page.locator('#screen-governance .empty');
        await expect(empty).toBeVisible();
        const et = (await empty.innerText()).trim();
        expect(et, 'it distinguishes "nothing declared" from "no violations"')
          .toMatch(/different from .the code has no violations|nothing to check against/i);
        expect(et, 'it names the file to declare in').toMatch(/\.repocortex\.yml/);
      } else {
        expect(text, 'a declared architecture renders a gate verdict').toMatch(/CI gate/i);
        const rows = await page.locator('#screen-governance tbody tr').count();
        expect(rows, 'a declared architecture lists components').toBeGreaterThan(0);
      }
    });

    test('every violation-confidence floor option loads and rides in the URL', async ({ page }) => {
      await cold(page, repo, '?s=governance');
      for (const floor of FLOORS) {
        await page.selectOption('#gov-floor', floor);
        await settle(page, 30_000);
        expect(new URL(page.url()).searchParams.get('sev') || '', `?sev=${floor || '(all)'}`).toBe(floor);
        const text = (await page.locator('#screen-governance').innerText()).trim();
        expect(text.length, `floor "${floor || 'all'}" still renders`).toBeGreaterThan(300);
        assertNoPlaceholders(text, `${repo.name} governance at sev=${floor || 'all'}`);
      }
      assertClean(page, `${repo.name} governance floors`);
    });

    test('?sev= on a COLD load selects that floor', async ({ page }) => {
      await cold(page, repo, '?s=governance&sev=high');
      await expect(page.locator('#gov-floor')).toHaveValue('high');
      assertClean(page, `${repo.name} governance deep link`);
    });

    test('the screen states what it left out', async ({ page }) => {
      await cold(page, repo, '?s=governance');
      expect(await page.locator('#screen-governance').innerText()).toMatch(OMISSIONS_STATED);
    });
  });
}
