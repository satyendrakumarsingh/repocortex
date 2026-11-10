import {
  test, expect, cold, settle, assertClean, assertNoPlaceholders,
  drawnOn, resetRecorder, repaint, isNodeTitle, clickableLabels, clickLabel, drawnCount, openFilters,
} from '../support/fixtures.mjs';
import { REPOS } from '../support/ports.mjs';

/*
  THE GRAPH EXPLORER'S SECOND LAYER.

  04-graph proves the map draws, pans, zooms and can be walked. This file proves
  the four things that make a map answer a question instead of decorating a
  page, and it proves each of them the same way: by reading text that was
  actually rendered — canvas fillText for anything drawn, DOM innerText for
  anything written — never by asserting that a code path exists.

    AUTO-SCOPE      the drilled rungs used to render an empty canvas when no
                    container was named. Now they choose one, say which and why,
                    and put it in the address bar.
    CONFIDENCE LENS a second, client-side floor. Raise it and watch the map
                    shrink; the count held back is stated and nothing is lost.
    FOCUS           one node's neighbourhood to depth N, everything else dimmed,
                    with the population and the horizon both counted.
    PATH            "how does this reach that", drawn as a chain with a measured
                    tier and a checkable file:line on every hop.
    SEARCH          find a node by name and fly to it, keyboard only.
    SHAREABLE VIEW  every one of the above survives a cold load of the URL.

  Two rules are enforced on all of them: no lens may remove something without
  saying how much it removed, and no control may put a value into a URL without
  encoding it.
*/

const GRAPH = '?s=graph';

/* Select a node on the canvas and return the inspector locator. */
async function selectAnyNode(page, where) {
  await repaint(page);
  const { clickable, labels } = await clickableLabels(page);
  expect(clickable.length,
    `a clickable node title on ${where} (drawn: ${labels.length})`).toBeGreaterThan(0);
  await clickLabel(page, clickable[0]);
  const inspector = page.locator('#graph-inspector');
  await expect(inspector, `selecting "${clickable[0].text}" opens the inspector`).toBeVisible();
  return { inspector, label: clickable[0], clickable };
}

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];

  test.describe(`${repo.name} — graph explorer, advanced`, () => {
    /* ---------------------------------------------------- defect 1: auto-scope */

    test('level=files with NO container never renders an empty canvas', async ({ page }) => {
      await cold(page, repo, `${GRAPH}&level=files`);

      const { note, drawn } = await drawnCount(page);
      const scope = (await page.locator('#graph-scope').innerText()).trim();
      const chosen = new URL(page.url()).searchParams.get('container');

      if (chosen) {
        // The honest answer when containers exist: pick one, and say so.
        expect(drawn, `something is drawn after auto-scoping to "${chosen}" — note: ${note}`).toBeGreaterThan(0);
        expect(scope, 'the banner names the scope that was chosen').toContain(chosen);
        expect(scope, 'the banner says the reader did not choose it').toMatch(/chosen for you/i);
        expect(scope, 'the banner states the rule it chose by').toMatch(/largest of/i);
        expect(scope, 'the banner states the rule is mechanical, not a ranking').toMatch(/never a relevance score/i);
        const labels = (await drawnOn(page)).filter(isNodeTitle);
        expect(labels.length, `the auto-scoped canvas draws labels: ${JSON.stringify(labels.slice(0, 4))}`).toBeGreaterThan(0);
      } else {
        // The honest answer when there are none: say there are none.
        expect(scope, 'with no containers to choose from, the screen says so instead of drawing nothing')
          .toMatch(/no .* to scope by/i);
      }
      assertNoPlaceholders(scope, `${repo.name} scope banner`);
      assertClean(page, `${repo.name} auto-scope files`);
    });

    test('level=symbols with NO container auto-scopes to the same container as files', async ({ page }) => {
      await cold(page, repo, `${GRAPH}&level=files`);
      const fromFiles = new URL(page.url()).searchParams.get('container');
      await cold(page, repo, `${GRAPH}&level=symbols`);
      const fromSymbols = new URL(page.url()).searchParams.get('container');
      expect(fromSymbols,
        'the rule is the same at both rungs, so the same container is chosen — a reader switching rung does not switch subject')
        .toBe(fromFiles);
      if (fromSymbols) {
        const { drawn, note } = await drawnCount(page);
        expect(drawn, `the auto-scoped symbol view draws something — ${note}`).toBeGreaterThan(0);
      }
    });

    test('the auto-scoped view is REPRODUCIBLE: its URL cold-loads to the same picture', async ({ page }) => {
      await cold(page, repo, `${GRAPH}&level=files`);
      const url = page.url();
      const first = await drawnCount(page);
      await page.goto('about:blank');
      await page.goto(url, { waitUntil: 'domcontentloaded' });
      await settle(page);
      const second = await drawnCount(page);
      expect(second.drawn, `cold-loading ${url} draws the same count: ${first.note} → ${second.note}`)
        .toBe(first.drawn);
    });

    /* ------------------------------------------------- the confidence lens */

    test('the CONFIDENCE LENS states its arithmetic and holds nothing back in silence', async ({ page }) => {
      await cold(page, repo, GRAPH);
      const readout = page.locator('#graph-lens-readout');
      await expect(readout, 'the lens states what is drawn').toBeVisible();

      const before = (await readout.innerText()).trim();
      expect(before, 'it says how many of how many are drawn').toMatch(/[\d,]+ of [\d,]+ combined edges? drawn/);
      expect(before, 'it says how much of the map is compiler-resolved').toMatch(/are proven/);
      assertNoPlaceholders(before, `${repo.name} lens readout`);

      const total = Number((before.match(/of ([\d,]+) combined/) || [])[1].replace(/,/g, ''));
      const drawnBefore = Number((before.match(/([\d,]+) of /) || [])[1].replace(/,/g, ''));
      expect(drawnBefore, 'with no lens, everything drawn is everything there is').toBe(total);

      await openFilters(page);
      await page.selectOption('#graph-lens', 'exact');
      await page.waitForTimeout(500);
      const after = (await readout.innerText()).trim();
      const drawnAfter = Number((after.match(/([\d,]+) of /) || [])[1].replace(/,/g, ''));
      const held = Number((after.match(/([\d,]+) held back by the exact lens/) || [0, '0'])[1].replace(/,/g, ''));

      expect(drawnAfter, `the exact lens cannot draw more than the unfiltered view: "${before}" → "${after}"`)
        .toBeLessThanOrEqual(drawnBefore);
      expect(drawnAfter + held,
        `the arithmetic closes: ${drawnAfter} drawn + ${held} held back must equal the ${total} that exist. "${after}"`)
        .toBe(total);
      if (held > 0) {
        expect(after, 'the lens says the held-back edges still exist').toMatch(/still in the index/i);
        const table = await page.locator('#graph-table').innerText();
        expect(table, 'the table repeats the count it is not showing').toMatch(/held? them back|holds them back/i);
      }
      expect(new URL(page.url()).searchParams.get('lens'), '?lens= records the lens').toBe('exact');
      assertClean(page, `${repo.name} confidence lens`);
    });

    test('the confidence lens NEVER refetches — it is a redraw, not a request', async ({ page }) => {
      await cold(page, repo, GRAPH);
      const calls = [];
      page.on('request', (r) => { if (r.url().includes('/api/graph')) calls.push(r.url()); });
      await openFilters(page);
      await page.selectOption('#graph-lens', 'high');
      await page.waitForTimeout(700);
      expect(calls, 'moving the client-side lens issued no /api/graph request').toEqual([]);
    });

    test('the CROSS-LANGUAGE lens is a stated count, and the table names the pair', async ({ page }) => {
      await cold(page, repo, GRAPH);
      const readout = page.locator('#graph-lens-readout');
      const before = (await readout.innerText()).trim();
      const crossing = Number((before.match(/([\d,]+) cross a language boundary/) || [0, '0'])[1].replace(/,/g, ''));

      await page.locator('#graph-xlang').click();
      await page.waitForTimeout(600);
      await expect(page.locator('#graph-xlang'), 'the chip records that it is on').toHaveAttribute('aria-pressed', 'true');
      expect(new URL(page.url()).searchParams.get('xlang'), '?xlang= records the lens').toBe('1');

      const after = (await readout.innerText()).trim();
      expect(after, 'it states how many were hidden as same-language').toMatch(/[\d,]+ hidden as same-language/);
      const hidden = Number((after.match(/([\d,]+) hidden as same-language/) || [])[1].replace(/,/g, ''));
      const drawn = Number((after.match(/([\d,]+) of /) || [])[1].replace(/,/g, ''));
      const total = Number((after.match(/of ([\d,]+) combined/) || [])[1].replace(/,/g, ''));
      expect(drawn + hidden, `${drawn} drawn + ${hidden} same-language must equal ${total}: "${after}"`).toBe(total);

      const table = await page.locator('#graph-table').innerText();
      expect(table, 'the relationships table has a cross-language column').toMatch(/Crosses languages/i);
      if (crossing > 0) {
        expect(drawn, 'the cross-language-only view draws the crossing edges').toBeGreaterThan(0);
      }
      assertNoPlaceholders(after, `${repo.name} cross-language readout`);
      assertClean(page, `${repo.name} cross-language lens`);
    });

    /* ------------------------------------------------------------- focus */

    test('FOCUS lights a neighbourhood, counts it, and counts what it left outside', async ({ page }) => {
      await cold(page, repo, GRAPH);
      const { label } = await selectAnyNode(page, `${repo.name} focus`);
      await page.locator('#act-focus').click();
      await page.waitForTimeout(500);

      const focus = new URL(page.url()).searchParams.get('focus');
      expect(focus, `focusing "${label.text}" records it in the URL`).toBeTruthy();

      const { note } = await drawnCount(page);
      expect(note, 'the canvas names what it is focused on').toMatch(/Focus:/);
      expect(note, 'the canvas counts the neighbourhood against the whole').toMatch(/[\d,]+ of [\d,]+/);
      expect(note, 'the canvas says the rest are dimmed, not deleted').toMatch(/dimmed, not removed/);
      const m = note.match(/Focus:.*?([\d,]+) of ([\d,]+)/);
      expect(m, `the focus sentence carries both counts: ${note}`).toBeTruthy();
      const inside = Number(m[1].replace(/,/g, ''));
      const all = Number(m[2].replace(/,/g, ''));
      expect(inside, 'a focused node is at least itself').toBeGreaterThan(0);
      expect(inside, 'the neighbourhood cannot exceed the population').toBeLessThanOrEqual(all);
      assertNoPlaceholders(note, `${repo.name} focus note`);
      assertClean(page, `${repo.name} focus`);
    });

    test('focus DEPTH changes the neighbourhood and never shrinks as it grows', async ({ page }) => {
      await cold(page, repo, GRAPH);
      await selectAnyNode(page, `${repo.name} focus depth`);
      await page.locator('#act-focus').click();
      await page.waitForTimeout(400);

      const reach = async () => {
        const { note } = await drawnCount(page);
        const m = note.match(/Focus:.*?([\d,]+) of ([\d,]+)/);
        return m ? Number(m[1].replace(/,/g, '')) : NaN;
      };
      await openFilters(page);
      await page.selectOption('#graph-fdepth', '1');
      await page.waitForTimeout(400);
      const at1 = await reach();
      await openFilters(page);
      await page.selectOption('#graph-fdepth', '4');
      await page.waitForTimeout(500);
      const at4 = await reach();

      expect(Number.isFinite(at1) && Number.isFinite(at4), `both depths reported a reach: ${at1}, ${at4}`).toBe(true);
      expect(at4, `4 hops reaches at least as far as 1 hop (${at1} → ${at4})`).toBeGreaterThanOrEqual(at1);
      expect(new URL(page.url()).searchParams.get('fdepth'), '?fdepth= records the depth').toBe('4');
    });

    test('clearing focus restores every node to full strength', async ({ page }) => {
      await cold(page, repo, GRAPH);
      await selectAnyNode(page, `${repo.name} focus clear`);
      await page.locator('#act-focus').click();
      await page.waitForTimeout(400);
      expect((await drawnCount(page)).note, 'focused').toMatch(/Focus:/);
      await page.locator('#graph-clear-lenses').click();
      await page.waitForTimeout(500);
      expect((await drawnCount(page)).note, 'the focus sentence is gone').not.toMatch(/Focus:/);
      expect(new URL(page.url()).searchParams.get('focus') || '', '?focus= is cleared').toBe('');
    });

    /* -------------------------------------------------------------- path */

    test('PATH between two nodes draws the chain, with a tier and evidence on every hop', async ({ page }) => {
      // A path runs between two NODES. Where the rung is large enough for the
      // map to collapse into groups, a collapsed group is not one of them, so
      // this asks for the ungrouped map — which is the view a reader tracing a
      // route would open anyway.
      await cold(page, repo, `${GRAPH}&group=none`);
      const { clickable } = await selectAnyNode(page, `${repo.name} path start`);
      test.skip(clickable.length < 2, 'this rung drew only one clickable node, so there is no pair to connect');

      await page.locator('#act-path-from').click();
      await page.waitForTimeout(300);
      const start = new URL(page.url()).searchParams.get('pfrom');
      expect(start, 'the start is recorded').toBeTruthy();

      // Walk the other drawn nodes until one of them is connected to the start.
      // The positions above were recorded before the inspector opened and
      // narrowed the canvas; re-read them on every pass so each click lands
      // on where its label is now, not where it was.
      let panel = '';
      let ended = '';
      for (const cand of clickable.slice(1, 8)) {
        const fresh = (await clickableLabels(page)).clickable.find((c) => c.text === cand.text) || cand;
        await clickLabel(page, fresh);
        if (!(await page.locator('#act-path-to').count())) continue;
        await page.locator('#act-path-to').click();
        await page.waitForTimeout(700);
        panel = (await page.locator('#graph-path').innerText()).trim();
        ended = cand.text;
        if (/hops?, weakest/i.test(panel)) break;
      }

      expect(panel.length, 'the path panel says something for every pair, connected or not').toBeGreaterThan(40);
      expect(panel, 'the panel names itself').toMatch(/How does this reach that/);
      assertNoPlaceholders(panel, `${repo.name} path panel`);

      if (/hops?, weakest/i.test(panel)) {
        expect(panel, 'the chain reports its hop count').toMatch(/HOPS\n[\d,]+/i);
        expect(panel, 'the chain reports the tier that limits it').toMatch(/WEAKEST HOP/i);
        expect(panel, 'the chain names the hop to check first').toMatch(/hop that limits this chain/i);
        expect(panel, 'the chain states the population it searched').toMatch(/Searched [\d,]+ relationships?/i);
        const hops = await page.locator('#graph-path .chain-hop').count();
        expect(hops, `the chain draws its hops as rows (ended at "${ended}")`).toBeGreaterThan(0);
        const tiers = await page.locator('#graph-path .chain-hop .tier').count();
        expect(tiers, 'every hop carries a measured tier').toBe(hops);
        const ev = await page.locator('#graph-path .chain-hop .ev .p').count();
        expect(ev, 'every hop carries a checkable file:line').toBe(hops);
        const first = (await page.locator('#graph-path .chain-hop .ev .p').first().innerText()).trim();
        expect(first, 'the evidence is a real path').toMatch(/\S+/);
      } else {
        // "No route" is a real answer, and it must be bounded rather than absolute.
        expect(panel, 'a no-route answer is scoped to this view, not to the repository')
          .toMatch(/statement about THIS VIEW/i);
        expect(panel, 'a no-route answer says what it searched').toMatch(/[\d,]+ relationships?/i);
      }
      assertClean(page, `${repo.name} path`);
    });

    test('a path with only ONE end set asks for the other rather than rendering nothing', async ({ page }) => {
      await cold(page, repo, `${GRAPH}&group=none`);
      await selectAnyNode(page, `${repo.name} half path`);
      await page.locator('#act-path-from').click();
      await page.waitForTimeout(500);
      const panel = (await page.locator('#graph-path').innerText()).trim();
      expect(panel, 'the panel is present and explains what is missing').toMatch(/A path needs both ends/);
      expect(panel, 'it names the control that supplies it').toMatch(/Path to here/);
      assertNoPlaceholders(panel, `${repo.name} half path`);
    });

    /* ------------------------------------------------------------ search */

    test('SEARCH finds a node by name and flies to it, keyboard only', async ({ page }) => {
      await cold(page, repo, GRAPH);
      const labels = (await repaint(page)).filter(isNodeTitle);
      expect(labels.length, 'the map drew something to search for').toBeGreaterThan(0);
      const needle = labels[0].text.replace(/\s\+\d+$/, '').trim().slice(0, 6);

      // "/" from anywhere on the screen, then arrows and Enter. No mouse.
      await page.locator('#graph-canvas').focus();
      await page.keyboard.press('/');
      await expect(page.locator('#graph-search'), '"/" moved focus to the search box').toBeFocused();
      await page.keyboard.type(needle);
      await page.waitForTimeout(700);
      const list = page.locator('#graph-search-list');
      await expect(list, 'the search opens a listbox').toBeVisible();
      const options = await list.locator('button.combo-opt').count();
      expect(options, `searching "${needle}" found something`).toBeGreaterThan(0);

      await page.keyboard.press('ArrowDown');
      await expect(page.locator('#graph-search'), 'the active option is announced')
        .toHaveAttribute('aria-activedescendant', /combo-opt-/);
      await page.keyboard.press('Enter');
      await page.waitForTimeout(1000);

      await expect(page.locator('#graph-inspector'), 'flying to a node selects it').toBeVisible();
      const inspected = (await page.locator('#graph-inspector').innerText()).trim();
      expect(inspected.length, 'the inspector filled with the node flown to').toBeGreaterThan(20);
      expect(new URL(page.url()).searchParams.get('sel'), 'the selection is in the URL').toBeTruthy();
      assertNoPlaceholders(inspected, `${repo.name} search target`);
      assertClean(page, `${repo.name} search`);
    });

    test('a search that matches nothing says so instead of silently doing nothing', async ({ page }) => {
      await cold(page, repo, GRAPH);
      await page.locator('#graph-search').click();
      await page.locator('#graph-search').fill('zzqx-not-a-symbol-anywhere');
      await page.waitForTimeout(700);
      const note = (await page.locator('#graph-search-list').innerText()).trim();
      expect(note, 'the empty result explains itself').toMatch(/Nothing at this level matches/i);
      expect(note, 'and names the reasons it could be absent').toMatch(/standard of proof|not extracted/i);
    });

    /* ------------------------------------------------- shareable views */

    test('EVERY lens survives a cold load of its own URL', async ({ page }) => {
      await cold(page, repo, `${GRAPH}&lens=high&xlang=1&fdepth=3&group=none`);
      await expect(page.locator('#graph-lens'), '?lens= restores the confidence lens').toHaveValue('high');
      await expect(page.locator('#graph-fdepth'), '?fdepth= restores the depth').toHaveValue('3');
      await expect(page.locator('#graph-xlang'), '?xlang= restores the cross-language lens')
        .toHaveAttribute('aria-pressed', 'true');
      await expect(page.locator('#graph-group'), '?group= restores the grouping').toHaveValue('none');
      const readout = (await page.locator('#graph-lens-readout').innerText()).trim();
      expect(readout, 'the restored view states its arithmetic').toMatch(/[\d,]+ of [\d,]+ combined/);
      assertNoPlaceholders(readout, `${repo.name} restored lenses`);
      assertClean(page, `${repo.name} shareable view`);
    });

    test('the JSON export carries the whole view, including what a lens was hiding', async ({ page }) => {
      await cold(page, repo, `${GRAPH}&lens=exact`);
      const [download] = await Promise.all([
        page.waitForEvent('download', { timeout: 20_000 }),
        page.locator('#screen-graph button.chip', { hasText: 'Export JSON' }).click(),
      ]);
      const stream = await download.createReadStream();
      let raw = '';
      for await (const chunk of stream) raw += chunk;
      const payload = JSON.parse(raw);

      expect(payload.view, 'the export describes the view it came from').toBeTruthy();
      expect(payload.view.confidence_lens, 'the lens is recorded').toBe('exact');
      expect(String(payload.view.shareable_url), 'the export carries the link back to itself').toContain('lens=exact');
      expect(payload.edges.length, 'every edge is exported, not only the drawn ones')
        .toBe(payload.view.edges_in_export);
      expect(payload.view.edges_in_export, 'the export is a superset of what was drawn')
        .toBeGreaterThanOrEqual(payload.view.edges_drawn);
      const hidden = payload.edges.filter((e) => e.hidden_by_lens).length;
      expect(payload.view.edges_drawn + hidden,
        `the export's own arithmetic closes: ${payload.view.edges_drawn} drawn + ${hidden} hidden = ${payload.edges.length}`)
        .toBe(payload.edges.length);
      expect(String(payload.note), 'the export says a lens was in force').toMatch(/hidden_by_lens/);
      for (const e of payload.edges.slice(0, 20)) {
        expect(typeof e.crosses_language, 'every exported edge states whether it crosses a language').toBe('boolean');
      }
    });

    /* --------------------------------------------- honesty of the counts */

    test('the canvas note never compares two different populations as if they were one', async ({ page }) => {
      // It used to: "from 168 files the server sent, of 114 it holds". Both
      // figures were right about different things — 114 is the requested
      // container, 168 includes the bounded neighbours a drilled view pulls in
      // from outside it — and a view claiming to have received more than
      // existed is a number a reader cannot repair by looking at anything else.
      await cold(page, repo, `${GRAPH}&level=files`);
      const { note } = await drawnCount(page);
      const num = (re) => {
        const m = note.match(re);
        return m ? Number(m[1].replace(/,/g, '')) : null;
      };
      const inView = num(/from ([\d,]+) /);
      const drew = num(/the server drew ([\d,]+) of/);
      const holds = num(/the server drew [\d,]+ of ([\d,]+) it holds/);
      const outside = num(/added ([\d,]+) from outside/) || 0;
      if (drew !== null) {
        expect(drew, `the server cannot have drawn more than it holds: "${note}"`).toBeLessThanOrEqual(holds);
        expect(drew + outside,
          `what is in the view is what the server drew plus the boundary it added: "${note}"`)
          .toBe(inView);
      } else {
        // No cap bit, so the whole scope is in the view and there is nothing
        // to reconcile — which the note says by not making the claim at all.
        expect(inView, 'the note still states the population it drew from').toBeGreaterThan(0);
      }
      assertNoPlaceholders(note, `${repo.name} canvas note populations`);
    });

    test('the held-back count never exceeds the population it is counting', async ({ page }) => {
      await cold(page, repo, `${GRAPH}&group=none`);
      const { note } = await drawnCount(page);
      const held = note.match(/([\d,]+) \w+ held back/);
      const from = note.match(/from ([\d,]+) /);
      if (held && from) {
        const h = Number(held[1].replace(/,/g, ''));
        const f = Number(from[1].replace(/,/g, ''));
        expect(h, `held back (${h}) cannot exceed the population it came from (${f}): "${note}"`)
          .toBeLessThanOrEqual(f);
      }
      assertNoPlaceholders(note, `${repo.name} canvas note, ungrouped`);
    });
  });
}
