import { test, expect, cold, settle, openFolds, assertClean, assertNoPlaceholders } from '../support/fixtures.mjs';
import { REPOS, intentFor } from '../support/ports.mjs';

const KPI = (page, label) =>
  page.locator('#screen-overview .kpi', { has: page.locator('.k', { hasText: new RegExp(`^${label}$`, 'i') }) });

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];

  test.describe(`${repo.name} — overview`, () => {
    test('the inventory reports real, non-zero counts for a real repository', async ({ page }) => {
      await cold(page, repo);
      const text = await page.locator('#screen-overview').innerText();
      expect(text, 'files counted').toMatch(/FILES\n[\d,]+/);
      expect(text, 'relationships counted').toMatch(/RELATIONSHIPS\n[\d,]+/);
      // The container rung is called "components" where an architecture is
      // declared and "directories" where it is not. The KPI is labelled with
      // whichever this index actually has, so the assertion is too.
      expect(text, `${repo.groupingLabel} counted`)
        .toMatch(new RegExp(`${repo.groupingLabel.toUpperCase()}\\n[\\d,]+`, 'i'));
      const files = Number((text.match(/FILES\n([\d,]+)/) || [])[1].replace(/,/g, ''));
      expect(files, 'file count is real').toBeGreaterThan(0);
      assertNoPlaceholders(text, `${repo.name} overview`);
      assertClean(page, `${repo.name} overview`);
    });

    test('the runtime-surface card states endpoints and tables — including when there are none', async ({ page }) => {
      await cold(page, repo);
      const text = await page.locator('#screen-overview').innerText();
      expect(text, 'endpoints stated').toMatch(/ENDPOINTS\n[\d,]+/);
      expect(text, 'tables stated').toMatch(/TABLES\n[\d,]+/);
      const endpoints = Number((text.match(/ENDPOINTS\n([\d,]+)/) || [])[1].replace(/,/g, ''));
      const tables = Number((text.match(/TABLES\n([\d,]+)/) || [])[1].replace(/,/g, ''));
      expect(endpoints, `${repo.name} endpoints`).toBe(repo.endpoints);
      expect(tables, `${repo.name} tables`).toBe(repo.tables);
    });

    test('the freshness card exists and says what the index state means', async ({ page }) => {
      await cold(page, repo);
      await expect(page.locator('#freshness')).toBeVisible();
      const text = await page.locator('#freshness').innerText();
      expect(text.trim().length, 'freshness card is not empty').toBeGreaterThan(40);
      assertNoPlaceholders(text, `${repo.name} freshness card`);
      if (repo.name === 'apm') {
        expect(text, 'a degraded index must say so on the freshness card').toMatch(/parse|fail|degrad/i);
      }
    });

    test('resolution coverage renders a bound percentage that is a number, not NaN', async ({ page }) => {
      await cold(page, repo);
      const text = await page.locator('#screen-overview').innerText();
      expect(text, 'bound percentage').toMatch(/BOUND\n\d+(\.\d+)?%/);
      expect(text, 'unresolved count').toMatch(/UNRESOLVED\n[\d,]+/);
      expect(text, 'ambiguous count').toMatch(/AMBIGUOUS\n[\d,]+/);
    });

    test('every "Show all N" expander reveals exactly N rows and removes its own cap notice', async ({ page }) => {
      await cold(page, repo);
      await openFolds(page);
      const expanders = page.locator('#screen-overview button.linkish');
      const n = await expanders.count();
      expect(n, 'at least one capped table on the overview').toBeGreaterThan(0);
      for (let i = 0; i < n; i++) {
        const btn = page.locator('#screen-overview button.linkish').first();
        const label = await btn.innerText();
        const total = Number((label.match(/Show all ([\d,]+)/) || [])[1].replace(/,/g, ''));
        const wrap = btn.locator('xpath=ancestor::div[contains(@class,"stack")][1]');
        const before = await wrap.locator('tbody tr').count();
        expect(before, 'a capped table shows fewer rows than it has').toBeLessThan(total);
        await btn.click();
        await page.waitForTimeout(150);
        const after = await page.locator('#screen-overview tbody tr').count();
        expect(after, `"${label}" reveals more rows than the capped view did`).toBeGreaterThan(before);
      }
      expect(await page.locator('#screen-overview button.linkish').count(),
        'every expander is consumed once used').toBe(0);
      assertClean(page, `${repo.name} overview expanders`);
    });

    test('every Copy button confirms, and copies a real command', async ({ page, context }) => {
      await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: `http://127.0.0.1:${repo.port}` });
      await cold(page, repo);
      // Element handles, not a text-filtered locator: the label changes to
      // "Copied" on click, which would re-resolve a filtered locator onto a
      // different button and silently test the same one twice.
      await openFolds(page);
      const handles = await page.$$('#screen-overview button.go');
      const copies = [];
      for (const h of handles) if ((await h.innerText()).trim() === 'Copy') copies.push(h);
      expect(copies.length, 'the overview offers copyable next actions').toBeGreaterThan(0);
      for (let i = 0; i < copies.length; i++) {
        await copies[i].click();
        await expect.poll(async () => (await copies[i].innerText()).trim(),
          { message: `copy button ${i} confirms` }).toBe('Copied');
        const clip = (await page.evaluate(() => navigator.clipboard.readText())).trim();
        expect(clip.length, `copy button ${i} put something on the clipboard`).toBeGreaterThan(0);
        expect(clip, `copy button ${i} copied a command, not a placeholder`).not.toMatch(/undefined|NaN|\[object Object\]/);
        await expect.poll(async () => (await copies[i].innerText()).trim(),
          { message: `copy button ${i} reverts`, timeout: 5000 }).toBe('Copy');
      }
    });

    test('every "Open →" next action navigates to a real screen', async ({ page }) => {
      await cold(page, repo);
      await openFolds(page);
      const n = await page.locator('#screen-overview button.go', { hasText: 'Open →' }).count();
      expect(n, 'the overview offers next actions').toBeGreaterThan(0);
      for (let i = 0; i < n; i++) {
        await cold(page, repo);
        await openFolds(page);
        await page.locator('#screen-overview button.go', { hasText: 'Open →' }).nth(i).click();
        await settle(page);
        const visible = await page.evaluate(() =>
          [...document.querySelectorAll('section.screen')].filter((s) => !s.hidden).map((s) => s.id));
        expect(visible, `Open → #${i} landed on exactly one screen`).toHaveLength(1);
        const text = await page.locator(`#${visible[0]}`).innerText();
        expect(text.trim().length, `Open → #${i} landed on a screen with content`).toBeGreaterThan(80);
      }
    });

    test('the ask box compiles a real task and renders a contract inline', async ({ page }) => {
      await cold(page, repo);
      await page.fill('#task-input', intentFor(repo));
      await page.locator('#screen-overview form.ask button.primary').click();
      await page.waitForFunction(() => {
        const r = document.getElementById('task-result');
        return r && r.querySelectorAll('.loading').length === 0 && r.innerText.trim().length > 100;
      }, null, { timeout: 30_000 });
      const result = await page.locator('#task-result').innerText();
      expect(result, 'the compiled contract names its scope').toMatch(/scope/i);
      expect(result, 'the intent rides in the URL').toBeTruthy();
      expect(new URL(page.url()).searchParams.get('intent'), '?intent= is set').toBe(intentFor(repo));
      assertNoPlaceholders(result, `${repo.name} inline contract`);
      assertClean(page, `${repo.name} ask box`);
    });

    test('each example chip fills the box and compiles', async ({ page }) => {
      await cold(page, repo);
      const chips = page.locator('#screen-overview .ask-examples button.chip');
      const n = await chips.count();
      expect(n, 'three worked examples').toBe(3);
      for (let i = 0; i < n; i++) {
        await cold(page, repo);
        const chip = page.locator('#screen-overview .ask-examples button.chip').nth(i);
        const label = (await chip.innerText()).trim();
        await chip.click();
        await page.waitForFunction(() => {
          const r = document.getElementById('task-result');
          return r && r.querySelectorAll('.loading').length === 0 && r.innerText.trim().length > 100;
        }, null, { timeout: 30_000 });
        expect(await page.inputValue('#task-input'), `chip ${i} fills the input`).toBe(label);
        const result = await page.locator('#task-result').innerText();
        expect(result.trim().length, `chip "${label}" produced a contract`).toBeGreaterThan(100);
        assertNoPlaceholders(result, `${repo.name} chip "${label}"`);
      }
    });

    test('?intent= on a cold load compiles without a click', async ({ page }) => {
      await cold(page, repo, `?intent=${encodeURIComponent(intentFor(repo))}`);
      await page.waitForFunction(() => {
        const r = document.getElementById('task-result');
        return r && r.querySelectorAll('.loading').length === 0 && r.innerText.trim().length > 100;
      }, null, { timeout: 30_000 });
      expect(await page.inputValue('#task-input')).toBe(intentFor(repo));
    });

    test('an empty submit does nothing rather than compiling an empty contract', async ({ page }) => {
      await cold(page, repo);
      await page.fill('#task-input', '   ');
      await page.locator('#screen-overview form.ask button.primary').click();
      await page.waitForTimeout(800);
      expect((await page.locator('#task-result').innerText()).trim(), 'empty task produces no result block').toBe('');
    });
  });
}
