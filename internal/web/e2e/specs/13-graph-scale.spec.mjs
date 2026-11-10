import {
  test, expect, cold, settle, openFolds, assertClean, assertNoPlaceholders,
  drawnOn, repaint, isNodeTitle, clickableLabels, clickLabel, drawnCount, horizontalOverflow, openFilters,
  graphMetrics,
} from '../support/fixtures.mjs';
import { REPOS, PORTS, base } from '../support/ports.mjs';
import { startPortal, stopPid } from '../support/servers.mjs';

/*
  SCALE. This whole file runs against one fixture — pulse: 14,614 files, 109,327
  symbols, 77,541 relationships, every declared component, nineteen
  languages — because the things it asserts cannot be observed on a small one.

  Three claims are under test.

  1. USABLE AT SIZE. Not "it responds": labels are RENDERED at every rung, the
     drawn set is small enough to read, and interaction after the map has
     settled is measured against a budget rather than eyeballed.

  2. EVERY OMISSION IS A COUNT. At this size every budget in the stack bites at
     once — the server's node budget, the client's legibility budget, the member
     cap inside a collapsed group, and the two client-side lenses. Each is
     asserted to state a number, and the numbers are asserted to be arithmetic
     that closes rather than decoration.

  3. A HOSTILE ID SURVIVES EVERY URL. pulse's container list opens with
     `(no declared component)` — a space and two parentheses in a value that is
     used as a query parameter, a fetch key, a deep link and a canvas label. It
     is driven through every control here, and no request may fail.

  The performance budgets are measured numbers with headroom, not aspirations.
  On the machine this was written on a cold portal answered the container rung
  in ~1.3s and a cold browser load of the graph screen settled in ~1.9s; the
  budgets are set well above that so an ordinary slow machine does not fail the
  suite, but far below the ~4s that a regression to the previous serialised
  index build would produce. See internal/web/index.go.
*/

// The scale fixture is whichever one carries a scale budget. Under
// CORTEX_E2E_TIER=ci none does, and every test here is skipped by that
// absence rather than by the fixture's name.
const repo = Object.values(REPOS).find((r) => r.coldGraphBudgetMs !== undefined);
const GRAPH = '?s=graph';
const AWKWARD = repo ? repo.awkwardContainer : '';
const AWK = encodeURIComponent(AWKWARD);

/* Every count a canvas note claims, as numbers. */
function noteCounts(note) {
  const n = (re) => {
    const m = note.match(re);
    return m ? Number(m[1].replace(/,/g, '')) : null;
  };
  return {
    drawn: n(/([\d,]+) drawn/),
    edges: n(/([\d,]+) combined edges?/),
    // `from` is every node IN THIS VIEW. `drew` and `holds` are the server's
    // figures for the requested scope, and `outside` is the bounded set of
    // neighbours pulled in from beyond it. They are three populations and the
    // note used to compare two of them as if they were one.
    from: n(/from ([\d,]+) /),
    drew: n(/the server drew ([\d,]+) of/),
    holds: n(/the server drew [\d,]+ of ([\d,]+) it holds/),
    outside: n(/added ([\d,]+) from outside/),
    held: n(/([\d,]+) \w+ held back/),
  };
}

test.describe(`${repo ? repo.name : 'no fixture'} — the graph explorer at scale`, () => {
  test.skip(!repo, 'no fixture carries a scale budget; this spec is about size');

  /* ------------------------------------------------------------ usable */

  test('every rung RENDERS LABELS on a 14,614-file repository', async ({ page }) => {
    const rungs = [
      { q: GRAPH, what: 'components' },
      { q: `${GRAPH}&level=files`, what: 'files' },
      { q: `${GRAPH}&level=symbols`, what: 'symbols' },
    ];
    for (const rung of rungs) {
      await cold(page, repo, rung.q);
      const labels = (await drawnOn(page)).filter(isNodeTitle);
      const distinct = new Set(labels.map((l) => l.text.replace(/\s\+\d+$/, '')));
      expect(labels.length,
        `the ${rung.what} rung drew node labels (total fillText: ${(await drawnOn(page)).length})`)
        .toBeGreaterThan(2);
      expect(distinct.size,
        `the ${rung.what} rung drew DISTINGUISHABLE labels: ${[...distinct].slice(0, 8).join(' | ')}`)
        .toBeGreaterThan(2);
      for (const l of labels) {
        expect(l.text, `a drawn label on the ${rung.what} rung is never a placeholder`)
          .not.toMatch(/^(undefined|NaN|\[object Object\])$/);
      }
      assertClean(page, `pulse ${rung.what} rung`);
    }
  });

  test('every declared component is DRAWN AS ITSELF — the map does not collapse what fits', async ({ page }) => {
    await cold(page, repo, GRAPH);
    const { note, drawn } = await drawnCount(page);
    expect(note, 'the note says how many components exist').toMatch(new RegExp(`from ${repo.components} components`));
    expect(drawn, `all ${repo.components} are drawn, not folded into a handful of communities: "${note}"`).toBe(repo.components);
    expect(note, 'and the map says the grouping decided that for itself').toMatch(/Grouping chose itself/);
    const labels = new Set((await repaint(page)).filter(isNodeTitle).map((l) => l.text.replace(/\s\+\d+$/, '')));
    expect(labels.size, `${repo.components} distinct component labels are legible: ${[...labels].join(', ')}`)
      .toBeGreaterThanOrEqual(15);
  });

  test('the drilled rungs stay READABLE: a bounded set, and the bound is a number', async ({ page }) => {
    for (const level of ['files', 'symbols']) {
      await cold(page, repo, `${GRAPH}&container=${AWK}&level=${level}`);
      const { note } = await drawnCount(page);
      const c = noteCounts(note);
      expect(c.drawn, `the ${level} rung drew something: "${note}"`).toBeGreaterThan(0);
      expect(c.drawn, `the ${level} rung drew a set a human can read, not a barcode: "${note}"`)
        .toBeLessThan(120);
      expect(c.holds, `the ${level} rung states the population the server holds: "${note}"`).toBeGreaterThan(0);
      expect(c.drew, 'and how many of it the server actually drew').toBeLessThanOrEqual(c.holds);
      expect(note, 'the note attributes those figures to the server')
        .toMatch(/the server drew [\d,]+ of [\d,]+ it holds in this scope/);
      // The arithmetic that used to be wrong on screen: what arrived is what
      // the server drew from the scope PLUS the neighbours it added outside it.
      expect(c.drew + (c.outside || 0),
        `nodes in the view must be the scope's drawn set plus the neighbours added outside it: "${note}"`)
        .toBe(c.from);
      assertNoPlaceholders(note, `pulse ${level} note`);
    }
  });

  /* -------------------------------------------------- omissions as counts */

  test('EVERY omission at this size is stated as a count that adds up', async ({ page }) => {
    await cold(page, repo, `${GRAPH}&container=${AWK}&level=symbols`);

    // 1. The server's node budget, in the omissions block — opened first, the
    // way a reader would open it.
    await openFolds(page);
    const omissions = (await page.locator('#graph-omissions').innerText()).trim();
    expect(omissions, 'the omissions block names itself').toMatch(/What this view can't see/);
    expect(omissions, 'and carries numbers, not adjectives').toMatch(/[\d,]+/);
    assertNoPlaceholders(omissions, 'pulse graph omissions');

    // 2. The client's legibility budget, on the canvas.
    const { note } = await drawnCount(page);
    const c = noteCounts(note);
    expect(c.from, 'the note states how many nodes arrived').toBeTruthy();
    expect(c.holds, 'the note states how many the server holds in this scope').toBeTruthy();
    expect(c.drew, 'the server drew no more than it holds').toBeLessThanOrEqual(c.holds);
    expect(c.drew + (c.outside || 0), 'and the view is exactly those plus the boundary neighbours')
      .toBe(c.from);
    if (c.held !== null) {
      expect(c.held, `held back (${c.held}) cannot exceed what arrived (${c.from}): "${note}"`)
        .toBeLessThanOrEqual(c.from);
      expect(c.drawn + c.held, `drawn + held back cannot exceed what arrived: "${note}"`)
        .toBeLessThanOrEqual(c.from);
      expect(note, 'and there is a control to override it').toMatch(/Draw more anyway/);
    }

    // 3. The lenses, in the readout.
    const readout = (await page.locator('#graph-lens-readout').innerText()).trim();
    expect(readout, 'the lens states drawn-of-total').toMatch(/[\d,]+ of [\d,]+ combined/);

    // 4. The relationships table, which is the text equivalent, states its own cap.
    const table = (await page.locator('#graph-table').innerText()).trim();
    expect(table, 'the table states how many pairs it covers').toMatch(/[\d,]+ pairs?|filtered to/);
    assertNoPlaceholders(table, 'pulse graph table');
    assertClean(page, 'pulse omissions at scale');
  });

  test('collapsed groups say how many members they are hiding, and opening one reveals them', async ({ page }) => {
    await cold(page, repo, `${GRAPH}&container=${AWK}&level=symbols`);
    const before = await drawnCount(page);
    expect(before.note, 'the symbol rung collapses at this size').toMatch(/collapsed group/);

    const { clickable } = await clickableLabels(page);
    expect(clickable.length, 'a collapsed group to open').toBeGreaterThan(0);
    await clickLabel(page, clickable[0]);
    const inspector = page.locator('#graph-inspector');
    await expect(inspector, 'the group opens an inspector').toBeVisible();
    const text = (await inspector.innerText()).trim();
    expect(text, 'the inspector counts the members').toMatch(/MEMBERS\n[\d,]+/i);
    expect(text, 'and states how the group was found rather than asserting it is architecture')
      .toMatch(/drawing aid|How this group was found/i);
    assertNoPlaceholders(text, 'pulse cluster inspector');
  });

  /* ------------------------------------- the hostile id, through every path */

  test(`the container id "${AWKWARD}" survives every URL the client builds`, async ({ page }) => {
    // A cold deep link carrying the raw id.
    await cold(page, repo, `${GRAPH}&container=${AWK}&level=files`);
    expect(page.badResponses, 'no request failed on a cold deep link with a space and parentheses in it').toEqual([]);
    const scope = (await page.locator('#graph-scope').innerText()).trim();
    expect(scope, 'the banner shows the id exactly as it is').toContain(AWKWARD);
    expect((await drawnCount(page)).drawn, 'and the graph drew').toBeGreaterThan(0);

    // The address bar round-trips it: decoding the query gives the id back.
    const back = new URL(page.url()).searchParams.get('container');
    expect(back, 'the URL decodes back to exactly the id, spaces and parentheses intact').toBe(AWKWARD);

    // Every control that refetches, driven while that id is the scope.
    await page.selectOption('#graph-level', 'symbols');
    await settle(page);
    await page.waitForTimeout(400);
    expect(new URL(page.url()).searchParams.get('container'), 'changing rung keeps the id intact').toBe(AWKWARD);

    await openFilters(page);
    await page.selectOption('#graph-language', 'go');
    await settle(page);
    await page.waitForTimeout(400);
    expect(new URL(page.url()).searchParams.get('container'), 'filtering by language keeps it').toBe(AWKWARD);

    await page.locator('#indexbar .tierbar button.t-low').click();
    await settle(page);
    await page.waitForTimeout(600);
    expect(new URL(page.url()).searchParams.get('container'), 'moving the confidence floor keeps it').toBe(AWKWARD);

    expect(page.badResponses,
      `every request made while "${AWKWARD}" was the scope succeeded`).toEqual([]);
    assertClean(page, 'pulse awkward container id');
  });

  test(`the API itself answers for "${AWKWARD}" when the id is encoded`, async ({ page }) => {
    const res = await page.request.get(
      `${base(repo)}/api/graph?level=files&container=${AWK}`);
    expect(res.status(), 'an encoded id is a 200, not a 400 and not a dropped connection').toBe(200);
    const body = await res.json();
    expect(body.filters.container, 'the server echoes the id it actually applied').toBe(AWKWARD);
    expect(body.files.length, 'and it returns that container’s files').toBeGreaterThan(0);
    expect(body.node_budget.available, 'stating the population it selected from').toBeGreaterThan(0);
    expect(body.node_budget.excluded,
      'and how many that population it did not send')
      .toBe(body.node_budget.available - body.node_budget.drawn);
  });

  test('the container picker offers the awkward id and scoping to it works', async ({ page }) => {
    await cold(page, repo, GRAPH);
    const input = page.locator('#graph-container');
    await input.click();
    await input.fill('declared');
    await page.waitForTimeout(700);
    const opts = page.locator('#graph-container-list .combo-opt');
    expect(await opts.count(), 'the picker finds it by substring').toBeGreaterThan(0);
    const first = opts.first();
    expect((await first.locator('.main').innerText()).trim(), 'and shows the id as written').toContain('declared component');
    await first.click();
    await settle(page);
    await page.waitForTimeout(700);
    expect(new URL(page.url()).searchParams.get('container'), 'choosing it scopes the graph to it').toBe(AWKWARD);
    expect(page.badResponses, 'no request failed').toEqual([]);
  });

  test('every editor link is a well-formed URL whose path decodes back to the file it names', async ({ page }) => {
    await cold(page, repo, `${GRAPH}&container=${AWK}&level=symbols`);
    const rows = await page.locator('#graph-table tbody tr').count();
    test.skip(rows === 0, 'no relationships drawn, so no evidence links to check');
    const links = await page.locator('#graph-table .ev').evaluateAll((nodes) => nodes.map((n) => ({
      shown: (n.querySelector('.p') || {}).textContent || '',
      href: (n.querySelector('a') || {}).getAttribute ? n.querySelector('a').getAttribute('href') : null,
    })).filter((x) => x.href));
    expect(links.length, 'the table offers editor links').toBeGreaterThan(0);
    for (const link of links.slice(0, 12)) {
      expect(() => new URL(link.href), `"${link.href}" parses as a URL`).not.toThrow();
      expect(link.href, 'no raw space survived into the href').not.toMatch(/ /);
      const file = link.shown.split(':')[0];
      expect(decodeURIComponent(new URL(link.href).pathname),
        'the encoded path decodes back to the file the row names')
        .toContain(file);
    }
  });

  /* ------------------------------------------------------ quantity */

  /*
    A NODE'S SIZE SAYS HOW MUCH IS IN IT. Seventeen components ranging from a
    handful of symbols to tens of thousands: the width of each box carries its
    symbol count on a log scale, clamped to [NODE_MIN_W, NODE_MAX_W], so the
    biggest is visibly wider than the smallest without the map's rest length
    - which is derived from the mean width - blowing past the legibility floor.
    The floor and the rest length are REPORTED by the metrics hook, not asserted.
  */
  test('a component with more symbols is drawn wider, within the clamp, and the map still fits', async ({ page }) => {
    await cold(page, repo, GRAPH);
    const res = await page.request.get(`${base(repo)}/api/graph`, { headers: { Accept: 'application/json' } });
    expect(res.ok(), '/api/graph answers').toBe(true);
    const containers = (await res.json()).containers || [];
    const m = await graphMetrics(page);
    const widthOf = new Map((m.widths || []).map((w) => [w.key, w.w]));
    const drawn = containers.filter((c) => widthOf.has('L:' + String(c.id)));
    const counts = new Set(drawn.map((c) => Number(c.symbols || 0)));
    test.skip(drawn.length < 2 || counts.size < 2,
      `${drawn.length} component leaves drawn with ${counts.size} distinct symbol counts; nothing to compare`);
    const most = drawn.reduce((a, c) => (Number(c.symbols) > Number(a.symbols) ? c : a));
    const fewest = drawn.reduce((a, c) => (Number(c.symbols) < Number(a.symbols) ? c : a));
    console.log(`pulse containers: ${m.nodes} nodes, meanW ${m.meanW}, minW ${m.minW}, maxW ${m.maxW}, ` +
      `rest ${m.rest}, zoom ${m.zoom}, on the floor: ${m.floor}, overflows: ${m.overflows}`);
    expect(widthOf.get('L:' + most.id),
      `${most.id} (${most.symbols} symbols, ${widthOf.get('L:' + most.id)}px) is wider than ` +
      `${fewest.id} (${fewest.symbols} symbols, ${widthOf.get('L:' + fewest.id)}px)`)
      .toBeGreaterThan(widthOf.get('L:' + fewest.id));
    for (const w of m.widths) {
      expect(w.w, `${w.key} is no narrower than NODE_MIN_W`).toBeGreaterThanOrEqual(74);
      expect(w.w, `${w.key} is no wider than NODE_MAX_W`).toBeLessThanOrEqual(260);
    }
    assertClean(page, 'pulse node quantity');
  });

  /* --------------------------------------------------- performance budgets */

  test('a COLD portal answers the container rung inside its budget', async () => {
    // Its own portal on its own port, so the measurement is a genuine cold
    // structural build and nothing else in the suite is disturbed.
    const probe = { name: 'pulse-cold', port: PORTS.cold, root: repo.root };
    const pid = await startPortal(probe);
    try {
      const started = Date.now();
      const res = await fetch(`http://127.0.0.1:${probe.port}/api/graph?level=containers`);
      const body = await res.json();
      const ms = Date.now() - started;
      expect(res.status, 'the cold request succeeded').toBe(200);
      expect(body.containers.length, 'and returned the whole container rung').toBe(repo.components);
      expect(ms,
        `the FIRST request against a 14,614-file index builds the structural map. ` +
        `It reads files, symbols and edges CONCURRENTLY (internal/web/index.go); serialising them again ` +
        `costs roughly 700ms more and is exactly what this budget catches. Measured: ${ms}ms`)
        .toBeLessThan(repo.coldGraphBudgetMs);

      // And the second request is served from the cache, by a wide margin.
      const warmStart = Date.now();
      const warm = await fetch(`http://127.0.0.1:${probe.port}/api/graph?level=containers`);
      await warm.json();
      const warmMs = Date.now() - warmStart;
      expect(warmMs,
        `the structural index is built once per run and cached; a second request must not rebuild it. ` +
        `cold ${ms}ms, warm ${warmMs}ms`)
        .toBeLessThan(Math.max(400, ms / 2));
    } finally {
      stopPid(pid);
    }
  });

  test('a cold BROWSER load of the graph screen settles inside its budget', async ({ page }) => {
    const started = Date.now();
    await page.goto(`${base(repo)}/${GRAPH}`, { waitUntil: 'domcontentloaded' });
    await settle(page);
    const ms = Date.now() - started;
    const { drawn } = await drawnCount(page);
    expect(drawn, 'it settled on a real drawing').toBeGreaterThan(0);
    expect(ms, `a reader opening the graph link waits ${ms}ms for ${repo.components} components over 14,614 files`)
      .toBeLessThan(repo.warmGraphBudgetMs);
  });

  test('interaction stays responsive after the map has settled', async ({ page }) => {
    await cold(page, repo, `${GRAPH}&container=${AWK}&level=symbols`);

    // Zoom: a pure redraw over the drawn set.
    const zoomStart = Date.now();
    await page.locator('#canvas-tools button[aria-label="Zoom in"]').click();
    await expect.poll(async () => (await page.locator('#zoom-readout').innerText()).trim(),
      { message: 'the zoom readout moved' }).not.toBe('100%');
    const zoomMs = Date.now() - zoomStart;
    expect(zoomMs, `zooming redraws in ${zoomMs}ms`).toBeLessThan(repo.interactionBudgetMs);

    // The confidence lens: a re-filter and redraw with no network at all.
    await openFilters(page);
    const lensStart = Date.now();
    await page.selectOption('#graph-lens', 'exact');
    await expect.poll(async () => (await page.locator('#graph-lens-readout').innerText()).includes('lens'),
      { message: 'the lens readout updated' }).toBe(true);
    const lensMs = Date.now() - lensStart;
    expect(lensMs, `the confidence lens re-filters and redraws in ${lensMs}ms, with no request`)
      .toBeLessThan(repo.interactionBudgetMs);

    // Selection: hit test through the spatial grid, then the inspector.
    await openFilters(page);
    await page.selectOption('#graph-lens', '');
    // The popover overlays the top-right of the canvas; a reader closes it
    // before reaching for a node, and so does this.
    await page.keyboard.press('Escape');
    await page.waitForTimeout(400);
    const { clickable } = await clickableLabels(page);
    expect(clickable.length, 'a node to select').toBeGreaterThan(0);
    const selStart = Date.now();
    await clickLabel(page, clickable[0]);
    await expect(page.locator('#graph-inspector')).toBeVisible();
    const selMs = Date.now() - selStart;
    expect(selMs, `selecting a node fills the inspector in ${selMs}ms`).toBeLessThan(repo.interactionBudgetMs);
    assertClean(page, 'pulse interaction budget');
  });

  /* ----------------------------------------------------------- hygiene */

  test('the graph screen has no horizontal body scroll at either size, at scale', async ({ page }) => {
    for (const size of [{ width: 1440, height: 900 }, { width: 375, height: 812 }]) {
      await page.setViewportSize(size);
      await cold(page, repo, `${GRAPH}&container=${AWK}&level=symbols&lens=high&xlang=1`);
      // The popover overlays the canvas, so it must be measured too — a
      // narrow screen that only overflows while Filters is open would
      // otherwise pass unnoticed.
      await openFilters(page);
      const over = await horizontalOverflow(page);
      expect(over.scrollWidth,
        `${size.width}x${size.height}: the document must not scroll sideways. Culprits: ${over.culprits.join(' ; ')}`)
        .toBeLessThanOrEqual(over.clientWidth + 1);
      await page.keyboard.press('Escape');
    }
  });

  test('the scale view carries no placeholder text anywhere on the screen', async ({ page }) => {
    await cold(page, repo, `${GRAPH}&container=${AWK}&level=symbols&lens=high`);
    const text = await page.evaluate(() => {
      const s = document.getElementById('screen-graph');
      return s ? (s.innerText || '') : '';
    });
    assertNoPlaceholders(text, 'pulse graph screen at scale');
    assertClean(page, 'pulse graph screen at scale');
  });
});
