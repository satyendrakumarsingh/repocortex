import {
  test, expect, cold, settle, openFolds, assertClean, assertNoPlaceholders, bodyText, screenText, OMISSIONS_STATED,
} from '../support/fixtures.mjs';
import { REPOS, base } from '../support/ports.mjs';

/*
  THE TEN PILLARS THAT NOW FETCH THEIR OWN ENDPOINT.

  Each of these screens used to render a placeholder panel because the endpoint
  behind it did not exist. It does now. What this file asserts is NOT that the
  screens are full — on these fixtures most of them are correctly empty, because
  no compiler index has been imported, no trace exported, no bench cases
  written, no estate declared, and each index holds one committed run. It
  asserts that an empty screen still ANSWERS:

    1. every screen renders either real rows or the server's own guidance, and
       never a bare empty state;
    2. the tests screen never renders "untested" as a verdict of its own;
    3. the accuracy screen never prints a recall score it did not measure;
    4. the deps screen never renders the not-computed reverse direction as zero;
    5. every screen states what it left out;
    6. nothing 4xx, nothing throws, nothing prints "undefined".

  Point 2 needs care, and the care is the point. `coverage_legend` is served as
  data and every verdict carries a `does_not_mean` — and the API's own sentence
  for `no_covering_test_proven` is "It does NOT mean the symbol is untested."
  Suppressing that sentence to keep a word off the page would delete the
  clearest statement on the screen. So the assertion is the honest one: the word
  may appear only inside a sentence that negates it, and never as a verdict,
  a badge or a table cell.
*/

/* Screen id → the endpoint it must call. */
const WIRED = {
  tables: '/api/db',
  config: '/api/config',
  deps: '/api/deps',
  tests: '/api/tests',
  accuracy: '/api/accuracy',
  diff: '/api/diff',
  ownership: '/api/who',
  trace: '/api/trace',
  retrieval: '/api/bench',
  estate: '/api/system',
};

const SCREENS = Object.keys(WIRED);


/*
  Collapse every run of whitespace to one space.

  Several of these payloads are written for a terminal as well as for this page
  — a guidance step aligns a command and its comment with a run of spaces, and a
  YAML example carries real newlines. CSS folds all of that into single spaces
  when the browser lays it out, so comparing raw payload text against innerText
  fails on the formatting rather than on the content. The content is what these
  assertions are about.
*/
const flat = (s) => String(s === null || s === undefined ? '' : s).replace(/\s+/g, ' ').trim();

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];

  test.describe(`${repo.name} — the wired pillars`, () => {
    test('each one fetches its own endpoint, and the endpoint answers', async ({ page }) => {
      for (const [screen, endpoint] of Object.entries(WIRED)) {
        const seen = [];
        const listener = (req) => {
          const path = new URL(req.url()).pathname;
          if (path.startsWith('/api/')) seen.push(path);
        };
        page.on('request', listener);
        await cold(page, repo, `?s=${screen}`);
        await settle(page, 60_000);
        page.off('request', listener);
        expect(seen, `${screen} must call ${endpoint}`).toContain(endpoint);
      }
      expect(page.badResponses, `${repo.name}: a wired pillar made a failing request`).toEqual([]);
    });

    test('each renders rows or the server’s guidance, never a bare empty state', async ({ page }) => {
      for (const [screen, endpoint] of Object.entries(WIRED)) {
        const payload = await (await page.request.get(base(repo) + endpoint)).json();
        await cold(page, repo, `?s=${screen}`);
        await settle(page, 60_000);
        const host = page.locator(`#screen-${screen}`);
        const body = await host.innerText();

        // Something a reader can act on: a table with rows, or a guidance card,
        // or a stated reason. Never a panel with a heading and nothing under it.
        const tables = await host.locator('table tbody tr').count();
        const kpis = await host.locator('.kpi').count();
        const prose = await host.locator('p.prose').count();

        /*
          Either the screen states what the index holds, or it states why it
          cannot state it. `?s=ownership` with no symbol is the second case on
          purpose: the reach figures behind it are all zero until a symbol is
          named, and four zeros under four labels is exactly the answer-shaped
          nothing this whole file exists to keep off the page.
        */
        const guided = !!(payload.guidance && payload.guidance.title
          && body.includes(payload.guidance.title));
        expect(kpis > 0 || guided,
          `${screen} states what the index holds, or the server's reason why it cannot`).toBe(true);
        expect(tables + prose, `${screen} shows rows or says why there are none`).toBeGreaterThan(0);

        // The omission block is on every screen, including the clean case.
        expect(body, `${screen} states what it left out`).toMatch(OMISSIONS_STATED);

        // Never a blank panel, never a placeholder that reached a reader.
        expect(body.length, `${screen} is not a blank panel`).toBeGreaterThan(400);
        assertNoPlaceholders(await bodyText(page), `${repo.name} ?s=${screen}`);
        assertClean(page, `${repo.name} ?s=${screen}`);
      }
    });

    /*
      An empty screen must name the ABSENCE and what fixes it, not just be
      short. Where the API sends `guidance`, its title has to be on the page —
      the portal is forbidden from paraphrasing it — and where it sends none,
      the screen has rows.
    */
    test('where the API sends guidance, the portal prints the API’s words', async ({ page }) => {
      for (const [screen, endpoint] of Object.entries(WIRED)) {
        const url = base(repo) + endpoint + (screen === 'ownership' ? '?symbol=' : '');
        const payload = await (await page.request.get(url)).json();
        await cold(page, repo, `?s=${screen}`);
        await settle(page, 60_000);
        const body = await screenText(page);

        if (payload.guidance && payload.guidance.title) {
          expect(flat(body), `${screen} must print the server's guidance title verbatim`)
            .toContain(flat(payload.guidance.title));
          if (payload.guidance.detail) {
            expect(flat(body), `${screen} must print the server's guidance detail verbatim`)
              .toContain(flat(payload.guidance.detail));
          }
          for (const step of payload.guidance.steps || []) {
            // Steps are multi-line in some payloads; the first line is enough
            // to prove the step reached the page rather than being dropped.
            // Compared with whitespace collapsed on both sides: these steps
            // are laid out for a terminal and carry runs of spaces and newlines
            // that CSS folds into one when the browser renders them. The text
            // is what must survive, not the shell alignment.
            const first = flat(String(step).split('\n')[0]);
            if (first.length > 12) {
              expect(flat(body), `${screen} must print guidance step: ${first}`).toContain(first);
            }
          }
        }
        if (payload.next_action && payload.next_action.label) {
          expect(flat(body), `${screen} must print the next action the API named`)
            .toContain(flat(payload.next_action.label));
        }
      }
    });

    /*
      TESTMIND. Two verdicts, never blended, and the word this screen must not
      assert.
    */
    test('the tests screen prints both verdicts and never asserts "untested"', async ({ page }) => {
      await cold(page, repo, '?s=tests');
      await settle(page, 60_000);
      await openFolds(page);
      const body = await screenText(page);

      const payload = await (await page.request.get(base(repo) + '/api/tests')).json();
      for (const verdict of payload.coverage_legend || []) {
        expect(body, `the legend must carry what "${verdict.verdict}" means`).toContain(verdict.means);
        expect(body, `the legend must carry what "${verdict.verdict}" does NOT mean`)
          .toContain(verdict.does_not_mean);
      }
      // Proven and suggested are two vocabularies on the page, not one.
      expect(body.toLowerCase(), 'the proven verdict is named').toContain('covering test proven');
      expect(body.toLowerCase(), 'the convention verdict is named').toContain('suggested by convention');

      // "untested" may only appear inside a sentence that negates it.
      const hits = [...body.matchAll(/untested/gi)];
      for (const hit of hits) {
        const before = body.slice(Math.max(0, hit.index - 90), hit.index);
        expect(/\b(not|never|no)\b/i.test(before),
          `"untested" was rendered without a negation before it: …${before.slice(-80)}untested`)
          .toBe(true);
      }
      // And never as a verdict, a badge or a cell of its own.
      const asVerdict = await page.locator('#screen-tests').evaluate((host) =>
        [...host.querySelectorAll('td, th, .tier, h3, h4')]
          .map((n) => (n.textContent || '').trim().toLowerCase())
          .filter((t) => t === 'untested' || t === 'no tests' || t === 'uncovered'));
      expect(asVerdict, 'no cell, badge or heading on the tests screen is a bare negative verdict')
        .toEqual([]);

      assertClean(page, `${repo.name} tests screen`);
    });

    /*
      ACCURACY. `recall_measured` is false on every fixture here, and a zero or
      a dash in its place reads as a score. It has to be a word.
    */
    test('the accuracy screen prints no recall score it did not measure', async ({ page }) => {
      await cold(page, repo, '?s=accuracy');
      await settle(page, 60_000);
      const payload = await (await page.request.get(base(repo) + '/api/accuracy')).json();
      const scores = payload.scores || {};

      const kpis = await page.locator('#screen-accuracy .kpi').evaluateAll((nodes) => nodes.map((n) => ({
        label: (n.querySelector('.k') || {}).textContent || '',
        value: (n.querySelector('.v') || {}).textContent || '',
      })));
      const recall = kpis.find((k) => /^recall$/i.test(k.label.trim()));
      expect(recall, 'the accuracy screen carries a recall figure of some kind').toBeTruthy();

      if (!scores.recall_measured) {
        expect(recall.value.trim(), 'an unmeasured recall must be a word, not a number or a dash')
          .toBe('not measured');
        expect(/\d/.test(recall.value), 'an unmeasured recall must contain no digits').toBe(false);
        expect(recall.value.trim(), 'an unmeasured recall must not render as an em dash').not.toBe('—');
        // The reason has to be on the page, not just the absence.
        const body = await screenText(page);
        expect(body, 'the screen says why recall was not measured')
          .toContain('Recall was not measured, and this screen will not print a number for it');
      } else {
        expect(recall.value, 'a measured recall is a percentage').toMatch(/%/);
      }

      const precision = kpis.find((k) => /^precision$/i.test(k.label.trim()));
      expect(precision, 'the accuracy screen carries a precision figure').toBeTruthy();
      if (!scores.precision_measured) {
        expect(precision.value.trim(), 'an unmeasured precision is a word too').toBe('not measured');
      }
      assertClean(page, `${repo.name} accuracy screen`);
    });

    /*
      DEPMIND. `imported_but_not_declared` is -1 for NOT COMPUTED. It must not
      render as -1 and must not render as 0.
    */
    test('the deps screen renders the not-computed direction as a word, not a count', async ({ page }) => {
      await cold(page, repo, '?s=deps');
      await settle(page, 60_000);
      const payload = await (await page.request.get(base(repo) + '/api/deps')).json();
      const reverse = payload.imported_but_not_declared || {};

      const kpis = await page.locator('#screen-deps .kpi').evaluateAll((nodes) => nodes.map((n) => ({
        label: (n.querySelector('.k') || {}).textContent || '',
        value: (n.querySelector('.v') || {}).textContent || '',
      })));
      const row = kpis.find((k) => /imported, declared nowhere/i.test(k.label));
      expect(row, 'the reverse direction has a figure on the screen').toBeTruthy();

      if (reverse.available !== true) {
        expect(row.value.trim(), 'a direction nothing enumerated is not a number').toBe('not measured');
        const body = await screenText(page);
        expect(body, 'the screen prints the API’s explanation of why').toContain(reverse.detail);
        expect(body, 'and says a zero would be a claim').toContain('a zero here would be a claim');
      }
      /*
        The sentinel must never reach a reader AS A FIGURE. A bare
        `not.toContain('-1')` over the whole screen is the wrong instrument
        here: a Maven snapshot version is legitimately `1.1.0-20250926.044709-1`,
        and failing on that would be the test reading a version string as a
        count. So the check is on the places a count is rendered.
      */
      const figures = await page.locator('#screen-deps .kpi .v, #screen-deps td.num')
        .evaluateAll((nodes) => nodes.map((n) => (n.textContent || '').trim()));
      expect(figures.filter((v) => v === '-1'), 'the -1 sentinel never renders as a count').toEqual([]);

      // The tier ladder is the other half of this pillar.
      const body = await screenText(page);
      for (const tier of payload.language_tiers || []) {
        expect(body, `the ${tier.tier} tier is on the page`).toContain(tier.what_you_get);
      }
      assertClean(page, `${repo.name} deps screen`);
    });

    /*
      DBMIND. Readers and writers are two populations, and a table with neither
      carries a caveat sentence that must be on the screen.
    */
    test('the tables screen keeps readers and writers apart and prints the per-table caveat', async ({ page }) => {
      await cold(page, repo, '?s=tables');
      await settle(page, 60_000);
      await openFolds(page);
      const payload = await (await page.request.get(base(repo) + '/api/db?min_confidence=medium')).json();
      const body = await screenText(page);

      expect(body, 'the coverage limit is on every answer').toContain(payload.coverage_limit);

      // Only over the rows the page actually drew: the inventory is capped at
      // 30 and says so, and a caveat belonging to row 400 is not missing from
      // the screen, it is on the page that row is on.
      const drawn = (payload.tables || []).slice(0, 30);
      const withCaveat = drawn.filter((t) => t.caveat);
      for (const t of withCaveat.slice(0, 3)) {
        expect(body, `the caveat for ${t.table} is on the screen`).toContain(t.caveat);
      }

      const accessed = (payload.tables || []).filter((t) => (t.written_by || []).length);
      if (accessed.length) {
        expect(body, 'writers are labelled as writers').toContain('Written by');
        expect(body, 'readers are labelled separately').toContain('Read by');
      }
      assertClean(page, `${repo.name} tables screen`);
    });

    /*
      AGENTBENCH. Recall alone is gamed by returning the repository, so the
      forbid half is never absent from the screen.
    */
    test('the retrieval screen never shows recall without the forbid half beside it', async ({ page }) => {
      await cold(page, repo, '?s=retrieval');
      await settle(page, 60_000);
      const body = await screenText(page);
      expect(body, 'the forbid score is on the screen').toMatch(/forbid score/i);
      expect(body, 'and so is the count of cases that have no forbid list')
        .toMatch(/cases with no forbid list/i);
      assertClean(page, `${repo.name} retrieval screen`);
    });

    /*
      OWNERSHIP. Three statuses, and none of them is a blank panel. `no_query`
      is the state a reader arrives in.
    */
    test('the ownership screen asks for a symbol, then answers with caveats attached', async ({ page }) => {
      await cold(page, repo, '?s=ownership');
      await settle(page, 60_000);
      let body = await screenText(page);
      expect(body, 'the empty state names what it needs').toMatch(/Pick a symbol/i);

      await cold(page, repo, `?s=ownership&sym=${encodeURIComponent(repo.resolvedSymbol)}`);
      await settle(page, 60_000);
      body = await screenText(page);

      const payload = await (await page.request.get(
        base(repo) + '/api/who?symbol=' + encodeURIComponent(repo.resolvedSymbol) + '&min_confidence=medium')).json();
      if (payload.status === 'ok') {
        expect(body, 'the target is named').toContain(payload.target.name);
        // The caveats are the part a ranking must never be printed without.
        for (const caveat of (payload.caveats || []).slice(0, 3)) {
          expect(body, 'the API’s caveat is on the screen').toContain(caveat);
        }
        expect(body, 'the bus factor is stated').toMatch(/bus factor/i);
      }
      assertNoPlaceholders(await bodyText(page), `${repo.name} ownership answered`);
      assertClean(page, `${repo.name} ownership screen`);
    });

    /*
      THE THREE FILE-BACKED SCREENS. Each echoes the artefact it tried to read,
      whether or not it found one — "you asked for this and it is missing" and
      "we guessed and the guess is missing" are different messages.
    */
    test('trace, retrieval and estate each name the file they looked for', async ({ page }) => {
      const files = { trace: 'export', retrieval: 'case_file', estate: 'manifest' };
      const endpoints = { trace: '/api/trace', retrieval: '/api/bench', estate: '/api/system' };
      for (const [screen, field] of Object.entries(files)) {
        await cold(page, repo, `?s=${screen}`);
        await settle(page, 60_000);
        const payload = await (await page.request.get(base(repo) + endpoints[screen])).json();
        const ref = payload[field] || {};
        const body = await screenText(page);
        expect(body, `${screen} names the file it read or looked for`).toContain(ref.path);
        if (ref.defaulted) {
          expect(body, `${screen} says the portal chose the path`).toContain(ref.defaulted_from);
        }
        if (ref.problem) {
          expect(body, `${screen} says why the file could not be read`).toContain(ref.problem);
        }
      }
      assertClean(page, `${repo.name} file-backed screens`);
    });

    /*
      FACT DIFF. One committed run per index is the healthy state, and a grid of
      zeroes with no sentence over it would read as "nothing changed".
    */
    test('the fact diff never shows a zero it did not compute', async ({ page }) => {
      await cold(page, repo, '?s=diff');
      await settle(page, 60_000);
      const payload = await (await page.request.get(base(repo) + '/api/diff')).json();
      const body = await screenText(page);

      if (!payload.compared) {
        const kpis = await page.locator('#screen-diff .kpi .v')
          .evaluateAll((nodes) => nodes.map((n) => (n.textContent || '').trim()));
        expect(kpis.length, 'the diff screen carries figures').toBeGreaterThan(0);
        for (const v of kpis) {
          expect(v, 'an uncompared count is a word, not a zero').toBe('not compared');
        }
        expect(body, 'and the API’s own sentence about those placeholders is printed')
          .toContain(payload.counts.scope);
      }
      assertClean(page, `${repo.name} fact diff`);
    });
  });
}
