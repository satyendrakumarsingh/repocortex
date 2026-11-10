import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { test, expect, cold, settle, openFolds } from '../support/fixtures.mjs';
import { REPOS, intentFor } from '../support/ports.mjs';

/*
  THE RULE THAT KEEPS EVERY SCREEN IN PLAIN WORDS.

  A portal for anyone cannot say "resolver", "rung" or "confidence floor" on
  screen, or show a bare tier id where a reader expects a word. The
  vocabulary the product is allowed to use is small and it lives here, as an
  assertion, because a style rule that lives only in a design document is one
  the next screen forgets.

  Every screen of every fixture: no forbidden word in visible text, and no
  bare tier id outside an element that carries data-tier, the glossary, or
  code. And every screen opens with the answer — one sentence, the first
  heading on the screen — with every disclosure under it closed until asked;
  ?open=1 unfolds them all, so an agent or a screenshot sees everything.
*/
const FORBIDDEN = [
  /\bCodeMind\b/, /\bImpactMind\b/, /\bAPIMap\b/, /\bDBMind\b/, /\bTestMind\b/, /\bConfigMind\b/,
  /\bTraceMind\b/, /\bArchMind\b/, /\bDepMind\b/, /\bAgentBench\b/, /\bPlatform\b/,
  /\bresolver\b/i, /\brung\b/i, /\bconfidence floor\b/i, /\bomission(s)?\b/i, /\baggregated\b/i,
  /\badjudicated\b/i, /\bheuristic\b/i, /\bfloor\b/i, /\bprovenance\b/i,
];

/*
  The words themselves come from the module the shell serves, read as text so
  the spec cannot drift from it; confidence_test.go holds the Go copy to the
  same file. What the words are is that module's business — this file only
  insists that wherever an element says which tier it is, the reader sees the
  word.
*/
const TIER_WORDS = {};
{
  const src = readFileSync(fileURLToPath(new URL('../../assets/js/tiers.mjs', import.meta.url)), 'utf8');
  for (const m of src.matchAll(/^\s*(\w+):\s*\{\s*word:\s*'([^']*)'/gm)) TIER_WORDS[m[1]] = m[2];
}
for (const id of ['exact', 'high', 'medium', 'low', 'unknown']) {
  if (!TIER_WORDS[id]) throw new Error(`assets/js/tiers.mjs gives no word for the tier "${id}"`);
}

/*
  The two checks, as functions, so the per-screen tests and the compiled
  task brief run the same rule: no bare tier id outside an element that
  carries data-tier, and no forbidden word in what is on screen.
*/
async function expectTiersInWords(page) {
  const bare = await page.locator('main').evaluate((root) => {
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    const hits = [];
    let n;
    while ((n = walker.nextNode())) {
      const t = n.textContent;
      if (!/\b(exact|high|medium|low|Exact|High|Medium|Low)\b/.test(t)) continue;
      const p = n.parentElement;
      if (!p || p.closest('[hidden], [data-tier], .glossary, code, pre')) continue;
      if (!p.offsetParent && getComputedStyle(p).position !== 'fixed') continue; // not rendered
      hits.push(t.trim().slice(0, 80));
    }
    return hits;
  });
  expect(bare, 'a bare tier id where a plain word belongs').toEqual([]);

  // The inverse: an element that names its tier and says anything at
  // all must say the word. Bars and swatches with no text carry the
  // tier by colour and title and are not held to it.
  const wordless = await page.locator('main').evaluate((root, words) => {
    const hits = [];
    for (const p of root.querySelectorAll('[data-tier]')) {
      if (p.closest('[hidden]')) continue;
      if (!p.offsetParent && getComputedStyle(p).position !== 'fixed') continue; // not rendered
      const id = p.getAttribute('data-tier');
      const word = words[id];
      const t = (p.innerText || '').trim();
      if (!word || !t) continue;
      if (!t.toLowerCase().includes(word.toLowerCase())) hits.push(id + ': ' + t.slice(0, 80));
    }
    return hits;
  }, TIER_WORDS);
  expect(wordless, 'an element that names its tier without the word for it').toEqual([]);
}

async function expectNoJargon(page) {
  // innerText omits what a closed fold holds, and folded copy is the
  // copy most likely to still say the old word; the gate reads it open.
  await openFolds(page);
  const visible = await page.locator('main').innerText();
  for (const re of FORBIDDEN) {
    expect(visible, `"${re.source}" is product jargon and must not be on screen`).not.toMatch(re);
  }
}

const ALL_SCREENS = ['overview', 'graph', 'search', 'metrics', 'blast', 'contract', 'verify', 'ownership', 'diff',
  'crossstack', 'tables', 'config', 'trace', 'estate', 'tests', 'governance', 'deps', 'accuracy', 'agent', 'retrieval'];

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];
  test.describe(`${repo.name} — plain language`, () => {
    for (const screen of ALL_SCREENS) {
      test(`${screen}: tiers in words`, async ({ page }) => {
        await cold(page, repo, `?s=${screen}`);
        await settle(page, 45_000);
        await expectTiersInWords(page);
      });
      test(`${screen}: no product jargon`, async ({ page }) => {
        await cold(page, repo, `?s=${screen}`);
        await settle(page, 45_000);
        await expectNoJargon(page);
      });
      test(`${screen}: opens with the answer, details closed`, async ({ page }) => {
        await cold(page, repo, `?s=${screen}`);
        await settle(page, 45_000);
        const shown = page.locator('main section.screen:not([hidden])');
        // The first heading on the screen is the answer, in a sentence.
        const first = shown.locator('.answer h2').first();
        await expect(first, 'the screen opens with an answer line').toBeVisible();
        const sentence = (await first.innerText()).trim();
        expect(sentence.length, 'an answer line is a sentence, not a label').toBeGreaterThan(24);
        expect(sentence, 'a sentence ends with a full stop').toMatch(/[.!?]$/);
        // Nothing headed precedes it.
        const before = await shown.evaluate((root) => {
          const a = root.querySelector('.answer');
          const hs = [...root.querySelectorAll('h2, h3')];
          return hs.filter((h) => a && (h.compareDocumentPosition(a) & Node.DOCUMENT_POSITION_FOLLOWING)).map((h) => h.textContent.trim());
        });
        expect(before, 'no heading sits above the answer line').toEqual([]);
        // Every disclosure starts closed.
        expect(await shown.locator('details.fold[open]').count(), 'details start closed').toBe(0);
      });
    }
    test('a compiled task brief: tiers in words, no product jargon', async ({ page }) => {
      // The per-screen tests load the contract screen empty; the brief that
      // an intent compiles is the copy with a standard of proof in it, and
      // it is held to the same rule. ?intent= compiles on a cold load.
      await cold(page, repo, `?s=contract&intent=${encodeURIComponent(intentFor(repo))}`);
      await settle(page, 45_000);
      expect(await page.inputValue('#contract-intent')).toBe(intentFor(repo));
      await expect(page.locator('#screen-contract details.fold').first(), 'the brief rendered its evidence').toBeVisible();
      await openFolds(page);
      await expectTiersInWords(page);
      await expectNoJargon(page);
    });
    test('?open=1 unfolds every disclosure', async ({ page }) => {
      await cold(page, repo, '?s=overview&open=1');
      await settle(page, 45_000);
      const shown = page.locator('main section.screen:not([hidden])');
      expect(await shown.locator('details.fold').count(), 'the overview has something to unfold').toBeGreaterThan(0);
      expect(await shown.locator('details.fold:not([open])').count(), 'every fold is open').toBe(0);
    });
    test('?open=1 is not remembered', async ({ page }) => {
      // A details element given open=true reports it as a toggle, and a fold
      // that wrote that to memory would leave every later visit open. Same
      // page context, so the same localStorage: the second load must be cold.
      await cold(page, repo, '?s=overview&open=1');
      await settle(page, 45_000);
      expect(await page.locator('main section.screen:not([hidden]) details.fold').count()).toBeGreaterThan(0);
      await cold(page, repo, '?s=overview');
      await settle(page, 45_000);
      const shown = page.locator('main section.screen:not([hidden])');
      expect(await shown.locator('details.fold').count(), 'the overview still has folds').toBeGreaterThan(0);
      expect(await shown.locator('details.fold[open]').count(), 'no fold remembers the ?open=1 visit').toBe(0);
    });
  });
}
