import { test, expect, cold, settle, openFolds, assertClean, assertNoPlaceholders, OMISSIONS_STATED } from '../support/fixtures.mjs';
import { REPOS, intentFor, base } from '../support/ports.mjs';

async function verdictOf(page) {
  const v = page.locator('#verify-body .verdict');
  await expect(v, 'a verdict block is rendered').toBeVisible();
  return {
    word: (await v.locator('.big').innerText()).trim(),
    say: (await v.locator('.say').innerText()).trim(),
    exit: (await v.locator('.exit').innerText()).trim(),
    klass: await v.getAttribute('class'),
    style: await v.evaluate((n) => {
      const s = getComputedStyle(n);
      return `${s.backgroundColor}|${s.color}|${s.borderColor}|${s.borderLeftColor}`;
    }),
  };
}

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];

  test.describe(`${repo.name} — verify`, () => {
    test('with nothing to verify it demands both inputs rather than passing', async ({ page }) => {
      await cold(page, repo, '?s=verify');
      const empty = page.locator('#verify-body .empty');
      await expect(empty).toBeVisible();
      const text = (await empty.innerText()).trim();
      expect(text, 'an empty change set is explicitly not a pass').toMatch(/An empty change set is not a pass/);
      expect(text, 'it names the exit code that setup failure carries').toMatch(/unverified/);
      assertClean(page, `${repo.name} verify idle`);
    });

    test('a change inside the contract renders PASS, distinctly', async ({ page }) => {
      await cold(page, repo, `?s=verify&intent=${encodeURIComponent(intentFor(repo))}&changed=${encodeURIComponent(repo.inScopeChanged)}`);
      await settle(page, 45_000);
      const v = await verdictOf(page);
      expect(v.word, 'the verdict word').toBe('PASS');
      expect(v.klass, 'PASS carries its own class').toContain('v-pass');
      expect(v.exit, 'PASS exits 0').toMatch(/exit 0\b/);
      expect(v.say, 'PASS says every question was asked and answered')
        .toMatch(/Every question was asked over the changed files/);
      assertNoPlaceholders(await page.locator('#verify-body').innerText(), `${repo.name} verify PASS`);
      assertClean(page, `${repo.name} verify PASS`);
    });

    test('a change the contract took no position on renders UNVERIFIED, not PASS', async ({ page }) => {
      await cold(page, repo, `?s=verify&intent=${encodeURIComponent(intentFor(repo))}&changed=README.md`);
      await settle(page, 45_000);
      const v = await verdictOf(page);
      expect(v.word, 'the verdict word').toBe('UNVERIFIED');
      expect(v.klass, 'UNVERIFIED carries its own class').toContain('v-unverified');
      expect(v.exit, 'UNVERIFIED exits 2').toMatch(/exit 2\b/);
      expect(v.say, 'UNVERIFIED says there was nothing to look at')
        .toMatch(/not the same as finding nothing wrong/);
      const findings = await page.locator('#verify-body').innerText();
      expect(findings, 'the findings card points back at the verdict')
        .toMatch(/Read the verdict above rather than this line/);
      assertClean(page, `${repo.name} verify UNVERIFIED`);
    });

    test('PASS, ADVISORY and UNVERIFIED are three visually DISTINCT renderings', async ({ page }) => {
      // Neither fixture repository produces an `advisory` verdict from any
      // intent tried, so advisory is exercised against a stubbed /api/verify
      // response. Pass and unverified below are the real server's.
      const seen = {};

      await cold(page, repo, `?s=verify&intent=${encodeURIComponent(intentFor(repo))}&changed=${encodeURIComponent(repo.inScopeChanged)}`);
      await settle(page, 45_000);
      seen.pass = await verdictOf(page);

      await cold(page, repo, `?s=verify&intent=${encodeURIComponent(intentFor(repo))}&changed=README.md`);
      await settle(page, 45_000);
      seen.unverified = await verdictOf(page);

      await page.route('**/api/verify**', async (route) => {
        const real = await route.fetch();
        const body = await real.json();
        body.verdict = 'advisory';
        body.unverified = false;
        body.exit_code = 0;
        body.signal = 'advisory';
        body.counts = Object.assign({}, body.counts, { facts: 0, leads: 1 });
        body.leads = [{
          summary: 'A heuristic edge reaches a changed file',
          detail: 'stubbed lead, so the advisory rendering can be asserted',
          standing: 'lead', confidence: 'low',
          evidence: `${repo.inScopeChanged}:1`, path: repo.inScopeChanged,
        }];
        await route.fulfill({ response: real, json: body });
      });
      await cold(page, repo, `?s=verify&intent=${encodeURIComponent(intentFor(repo))}&changed=${encodeURIComponent(repo.inScopeChanged)}`);
      await settle(page, 45_000);
      seen.advisory = await verdictOf(page);

      expect(seen.advisory.word, 'the advisory verdict word').toBe('ADVISORY');
      expect(seen.advisory.klass, 'advisory carries its own class').toContain('v-advisory');

      const words = [seen.pass.word, seen.advisory.word, seen.unverified.word];
      expect(new Set(words).size, `three distinct verdict words: ${words.join(', ')}`).toBe(3);

      const sentences = [seen.pass.say, seen.advisory.say, seen.unverified.say];
      expect(new Set(sentences).size, 'three distinct explanations').toBe(3);

      const styles = [seen.pass.style, seen.advisory.style, seen.unverified.style];
      expect(new Set(styles).size,
        `three distinct computed styles — pass=${seen.pass.style} advisory=${seen.advisory.style} unverified=${seen.unverified.style}`)
        .toBe(3);

      const advisoryBody = await page.locator('#verify-body').innerText();
      expect(advisoryBody, 'advisory renders its leads and says they never block')
        .toMatch(/never a blocker|does not fail the build/i);
      assertNoPlaceholders(advisoryBody, `${repo.name} verify ADVISORY`);
    });

    test('the four questions are listed with whether each was actually asked', async ({ page }) => {
      await cold(page, repo, `?s=verify&intent=${encodeURIComponent(intentFor(repo))}&changed=${encodeURIComponent(repo.inScopeChanged)}`);
      await settle(page, 45_000);
      await openFolds(page);
      const text = await page.locator('#verify-body').innerText();
      expect(text, 'the questions card is rendered').toMatch(/The four questions/);
      const rows = await page.locator('#verify-body table tbody tr').count();
      expect(rows, 'the questions are listed as rows').toBeGreaterThan(0);
      expect(text, 'each question says whether it was answered').toMatch(/yes|nothing to answer it with/);
    });

    test('every changed file is accounted for with a standing', async ({ page }) => {
      const changed = `${repo.inScopeChanged},README.md`;
      await cold(page, repo, `?s=verify&intent=${encodeURIComponent(intentFor(repo))}&changed=${encodeURIComponent(changed)}`);
      await settle(page, 45_000);
      await openFolds(page);
      const text = await page.locator('#verify-body').innerText();
      expect(text, 'every changed file is listed').toMatch(/Every changed file/);
      expect(text, 'the in-scope file is named').toContain(repo.inScopeChanged);
      expect(text, 'the undeclared file is named').toContain('README.md');
      expect(text, 'undeclared is called what it is').toMatch(/undeclared/);
      assertNoPlaceholders(text, `${repo.name} verify file standings`);
    });

    test('submitting the form drives the same result as the deep link', async ({ page }) => {
      await cold(page, repo, '?s=verify');
      await page.fill('#verify-intent', intentFor(repo));
      await page.fill('#verify-changed', repo.inScopeChanged);
      await page.locator('#screen-verify button.primary', { hasText: 'Verify' }).click();
      await settle(page, 45_000);
      const v = await verdictOf(page);
      expect(v.word, 'the form produced the same verdict as the deep link').toBe('PASS');
      const url = new URL(page.url());
      expect(url.searchParams.get('intent'), '?intent= recorded').toBe(intentFor(repo));
      expect(url.searchParams.get('changed'), '?changed= recorded').toBe(repo.inScopeChanged);
      assertClean(page, `${repo.name} verify form`);
    });

    test('the report states what it left out', async ({ page }) => {
      await cold(page, repo, `?s=verify&intent=${encodeURIComponent(intentFor(repo))}&changed=${encodeURIComponent(repo.inScopeChanged)}`);
      await settle(page, 45_000);
      expect(await page.locator('#verify-body').innerText()).toMatch(OMISSIONS_STATED);
    });
  });
}
