import {
  test, expect, cold, settle, assertClean, assertNoPlaceholders,
  drawnOn, resetRecorder, repaint, isNodeTitle, clickableLabels, clickLabel, dblClickLabel, SUB_LINE, OMISSIONS_STATED, openFilters,
  graphMetrics, nodeGeometry,
} from '../support/fixtures.mjs';
import { REPOS, base } from '../support/ports.mjs';

/*
  The canvas helpers live in support/fixtures.mjs so this file and the two that
  follow it share one definition of "a label that can be clicked". They scroll
  the canvas into view before converting a recorded fillText back into a screen
  point: the graph screen is taller than a 900px viewport, so a label can be
  drawn, be visible to a reader who scrolls, and still be outside the viewport
  at the instant a test clicks it.
*/
const drawnLabels = drawnOn;
const repaintAndCapture = repaint;

/*
  The back-edges of a directed graph, by the same walk app.js's layout uses:
  an iterative depth-first search started from every node in key order, each
  node's out-edges taken in key order, and an edge to a node that is still on
  the search stack marked as back. The two must agree edge for edge, because
  the assertion below is "every edge that is NOT back points down", and a
  test that chose its own back-edges would be asserting its own choice.
*/
function backEdgesOf(keys, edges) {
  const ids = keys.slice().sort();
  const out = new Map(ids.map((k) => [k, []]));
  for (const e of edges) if (out.has(e.a) && out.has(e.b)) out.get(e.a).push(e.b);
  for (const list of out.values()) list.sort();
  const state = new Map();
  const back = new Set();
  for (const start of ids) {
    if (state.has(start)) continue;
    state.set(start, 1);
    const stack = [{ key: start, i: 0 }];
    while (stack.length) {
      const top = stack[stack.length - 1];
      const kids = out.get(top.key);
      if (top.i >= kids.length) { state.set(top.key, 2); stack.pop(); continue; }
      const next = kids[top.i++];
      const seen = state.get(next);
      if (seen === 1) back.add(top.key + ' ' + next);
      else if (!seen) { state.set(next, 1); stack.push({ key: next, i: 0 }); }
    }
  }
  return back;
}

/*
  The container edges the map was given, as drawn-key pairs: the same /api/graph
  answer the page fetched, restricted to the pairs whose both ends are in the
  settled geometry. With grouping off the drawn nodes are the containers
  themselves, so this is the graph the layout ranked, edge for edge.
*/
async function drawnContainerEdges(page, repo, geometry) {
  const drawn = new Set(geometry.map((g) => g.key));
  const res = await page.request.get(`${base(repo)}/api/graph`, { headers: { Accept: 'application/json' } });
  expect(res.ok(), '/api/graph answers').toBe(true);
  return ((await res.json()).container_edges || [])
    .map((e) => ({ a: 'L:' + String(e.from), b: 'L:' + String(e.to) }))
    .filter((e) => e.a !== e.b && drawn.has(e.a) && drawn.has(e.b));
}

/*
  The path the renderer draws for an edge, by the same construction as
  app.js's routePoints, so that this file samples what is on the canvas and
  not a chord the canvas no longer draws. The route leaves the source through
  a port on the edge facing the target - where the line to the next member
  crosses that edge, clamped 8 px inside the corners - runs a vertical stub
  from that port to 4 px past the edge of the row's band (the band is taller
  than the box when the row holds a cluster), passes each via vertically
  through the whole height of its row and 4 px past it at each end, crosses
  what is left of the channel between rows diagonally, and enters the target
  through the same kind of stub and port; a route that points down keeps
  2 px left of that line and one that points up 2 px right, so the two
  directions of one pair are two strokes. The segments are straight.
  Twenty-one samples per segment, labelled by segment so a failure says
  where along the route the stroke went wrong.
*/
function routePoints(a, b, via) {
  const members = via && via.length ? [a, ...via, b] : [a, b];
  const down = b.y >= a.y;
  const side = down ? -2 : 2;
  const port = (m, other, y, farY) => {
    const t = (y - m.y) / ((other.y - m.y) || 1);
    const half = Math.max(0, (m.w || 0) / 2 - 8);
    const x = Math.max(m.x - half, Math.min(m.x + half, m.x + (other.x - m.x) * t)) + side;
    return [{ x, y }, { x, y: farY }];
  };
  const points = [];
  for (let i = 0; i < members.length; i++) {
    const m = members[i];
    const h = m.h || 40;
    const band = Math.max(h, m.band || 0);
    const top = m.y - h / 2; const bottom = m.y + h / 2;
    const farTop = m.y - band / 2 - 4; const farBottom = m.y + band / 2 + 4;
    if (i > 0) {
      const into = down ? port(m, members[i - 1], top, farTop) : port(m, members[i - 1], bottom, farBottom);
      for (let k = into.length - 1; k >= 0; k--) points.push(into[k]);
    }
    if (i < members.length - 1) {
      const from = down ? port(m, members[i + 1], bottom, farBottom) : port(m, members[i + 1], top, farTop);
      for (const p of from) points.push(p);
    }
  }
  return points;
}

function strokeSamples(a, b, via) {
  const points = routePoints(a, b, via);
  const samples = [];
  for (let s = 1; s < points.length; s++) {
    const p = points[s - 1]; const q = points[s];
    for (let i = 0; i <= 20; i++) {
      const u = i / 20;
      samples.push({ x: p.x + (q.x - p.x) * u, y: p.y + (q.y - p.y) * u, at: `segment ${s}/${points.length - 1}, ${i}/20` });
    }
  }
  return samples;
}

async function canvasBox(page) {
  await page.locator('#graph-canvas').scrollIntoViewIfNeeded();
  return page.locator('#graph-canvas').boundingBox();
}

/* One clickable node title, or a failure that says why there was none. */
async function pickNode(page, where) {
  const { clickable, labels } = await clickableLabels(page);
  expect(clickable.length,
    `a node label inside the viewport to click on ${where}; ` +
    `${labels.length} labels were drawn, ${labels.filter(isNodeTitle).length} of them node titles`)
    .toBeGreaterThan(0);
  return clickable[0];
}

for (const key of Object.keys(REPOS)) {
  const repo = REPOS[key];

  test.describe(`${repo.name} — graph explorer`, () => {
    test('the screen opens on the GRAPH, and the view tabs reach the other three', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      await expect(page.locator('#graph-canvas')).toBeVisible();
      for (const host of ['#graph-tree-host', '#graph-treemap-host', '#graph-code-host']) {
        await expect(page.locator(host), `${host} is not rendered on the graph view`).toBeHidden();
      }
      await expect(page.locator('#graph-view-switch [role=tab][aria-selected=true]')).toHaveText(/Graph/);

      await page.locator('#graph-view-switch [data-view=tree]').click();
      await expect(page.locator('#graph-tree-host')).toBeVisible();
      await expect(page.locator('#graph-canvas'), 'the map is not rendered on the tree view').toBeHidden();

      // The view is addressable: a reload lands on the same one.
      expect(page.url()).toContain('gv=tree');
      await page.reload();
      await expect(page.locator('#graph-tree-host')).toBeVisible();
    });

    test('the map is above the fold and its filters are behind one button', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      await settle(page, 45_000);
      const canvas = page.locator('#screen-graph canvas').first();
      const box = await canvas.boundingBox();
      expect(box, 'the canvas is laid out').not.toBeNull();
      expect(box.y, 'the canvas starts within the first 560px of the page').toBeLessThan(560);
      const pop = page.locator('#graph-filters-pop');
      await expect(pop).toBeHidden();
      await page.locator('#graph-filters').click();
      await expect(pop).toBeVisible();
      for (const id of ['graph-group', 'graph-language', 'graph-kinds', 'graph-lens', 'graph-fdepth']) {
        await expect(pop.locator('#' + id), id + ' lives in the popover').toBeVisible();
      }
      await page.keyboard.press('Escape');
      await expect(pop).toBeHidden();
      await expect(page.locator('#graph-filters')).toHaveAttribute('aria-expanded', 'false');
    });

    test('the canvas RENDERS TEXT at the default view, not just an empty canvas element', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      await expect(page.locator('#graph-canvas')).toBeVisible();
      const labels = await drawnLabels(page);
      const nonEmpty = labels.filter((l) => l.text && l.text.trim().length > 1);
      expect(nonEmpty.length,
        `fillText calls on #graph-canvas at the default view (total fillText calls: ${labels.length}); ` +
        `sample: ${JSON.stringify(labels.slice(0, 8))}`).toBeGreaterThan(2);
      const distinct = new Set(nonEmpty.map((l) => l.text));
      expect(distinct.size, `distinct label strings drawn: ${[...distinct].slice(0, 10).join(' | ')}`).toBeGreaterThan(2);
      for (const l of nonEmpty) {
        expect(l.text, 'a drawn label is never a placeholder').not.toMatch(/^(undefined|NaN|\[object Object\])$/);
      }
      assertClean(page, `${repo.name} graph`);
    });

    test('every drawn node label names one node — no two nodes carry the same label', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      // Exactly one frame: the recorder is cleared and a single pan redraws.
      await resetRecorder(page);
      await page.locator('#graph-canvas').press('ArrowLeft');
      await page.waitForTimeout(300);
      const labels = await drawnLabels(page);
      // The node title is the first fillText per node; the sub-line ("N items"
      // under a group, "N files with symbols" under a container) and the cluster
      // "+" affordance are not names. The " +N" suffix is a member count, and
      // app.js's own comment says it must never be what makes two labels look
      // different, so it is stripped before comparing.
      const titles = labels.filter((l) => l.align === 'left').map((l) => l.text.trim())
        .filter((t) => t.length > 1 && !SUB_LINE.test(t))
        .map((t) => t.replace(/\s\+\d+$/, ''));
      expect(titles.length, 'node titles were drawn').toBeGreaterThan(1);
      const dupes = titles.filter((t, i) => titles.indexOf(t) !== i);
      expect([...new Set(dupes)],
        `these labels are drawn on more than one node, so a reader cannot tell those nodes apart. All drawn titles: ${JSON.stringify(titles)}`)
        .toEqual([]);
    });

    test('the canvas note states how much was drawn, and agrees with the node count', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const note = (await page.locator('#canvas-note').innerText()).trim();
      expect(note, 'the canvas says what it drew').toMatch(/\d+ drawn/);
      assertNoPlaceholders(note, `${repo.name} canvas note`);
      const drawn = Number((note.match(/(\d+) drawn/) || [])[1]);
      expect(drawn, 'something was drawn').toBeGreaterThan(0);
    });

    test('zoom in, zoom out and fit each move the zoom readout', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const readout = page.locator('#zoom-readout');
      const start = await readout.innerText();
      await page.locator('#canvas-tools button[aria-label="Zoom in"]').click();
      await page.waitForTimeout(150);
      const zoomedIn = await readout.innerText();
      expect(parseInt(zoomedIn), `zoom in: ${start} → ${zoomedIn}`).toBeGreaterThan(parseInt(start));
      await page.locator('#canvas-tools button[aria-label="Zoom out"]').click();
      await page.locator('#canvas-tools button[aria-label="Zoom out"]').click();
      await page.waitForTimeout(150);
      const zoomedOut = await readout.innerText();
      expect(parseInt(zoomedOut), `zoom out: ${zoomedIn} → ${zoomedOut}`).toBeLessThan(parseInt(zoomedIn));
      await page.locator('#canvas-tools button', { hasText: 'Fit' }).click();
      await page.waitForTimeout(250);
      expect(await readout.innerText(), 'fit restores the fitted zoom').toBe(start);
    });

    test('dragging pans the canvas — the same labels move', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const before = await repaintAndCapture(page);
      expect(before.length, 'labels to pan').toBeGreaterThan(0);
      const box = await canvasBox(page);
      // Drag from a point that is inside BOTH the canvas and the viewport.
      const vh = page.viewportSize().height;
      const cy = Math.min(box.y + box.height / 2, vh - 90);
      await page.mouse.move(box.x + box.width / 2, cy);
      await page.mouse.down();
      await page.mouse.move(box.x + box.width / 2 - 140, cy - 70, { steps: 8 });
      await page.mouse.up();
      await page.waitForTimeout(250);
      await resetRecorder(page);
      // Nudge one more frame without resetting the view.
      await page.locator('#graph-canvas').press('ArrowLeft');
      await page.waitForTimeout(250);
      const after = await drawnLabels(page);
      const first = before.find((b) => after.some((a) => a.text === b.text));
      expect(first, 'a label survives the pan').toBeTruthy();
      const moved = after.find((a) => a.text === first.text);
      expect(Math.abs(moved.x - first.x) + Math.abs(moved.y - first.y),
        `label "${first.text}" moved from (${Math.round(first.x)},${Math.round(first.y)}) to (${Math.round(moved.x)},${Math.round(moved.y)})`)
        .toBeGreaterThan(20);
    });

    test('clicking a drawn node opens the inspector with that node in it', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      await repaintAndCapture(page);
      const target = await pickNode(page, `${repo.name} graph`);
      await clickLabel(page, target);
      const inspector = page.locator('#graph-inspector');
      await expect(inspector, `clicking "${target.text}" opens the inspector`).toBeVisible();
      const text = (await inspector.innerText()).trim();
      expect(text.length, 'the inspector says something').toBeGreaterThan(20);
      assertNoPlaceholders(text, `${repo.name} graph inspector`);
      assertClean(page, `${repo.name} graph node click`);
    });

    test('double-clicking a cluster expands it: the URL records it and more is drawn', async ({ page }) => {
      // Grouping is automatic by default and collapses nothing when the whole
      // rung fits, which on a 19-component repository means there is no cluster
      // to expand. This test is about the cluster, so it asks for clustering.
      await cold(page, repo, '?s=graph&group=communities');
      const noteBefore = (await page.locator('#canvas-note').innerText()).trim();
      const drawnBefore = Number((noteBefore.match(/(\d+) drawn/) || [])[1]);
      await repaintAndCapture(page);
      const target = await pickNode(page, `${repo.name} cluster expand`);
      await dblClickLabel(page, target);
      const expand = new URL(page.url()).searchParams.get('expand');
      expect(expand, `double-click on "${target.text}" records the expansion in the URL`).toBeTruthy();
      const noteAfter = (await page.locator('#canvas-note').innerText()).trim();
      const drawnAfter = Number((noteAfter.match(/(\d+) drawn/) || [])[1]);
      expect(drawnAfter, `expanding drew more: "${noteBefore}" → "${noteAfter}"`).toBeGreaterThan(drawnBefore);
      const after = await repaintAndCapture(page);
      expect(after.filter((l) => l.text.trim().length > 1).length,
        'the expanded view still draws labels').toBeGreaterThan(0);
      assertClean(page, `${repo.name} cluster expand`);
    });

    test('Tab walks the drawn nodes and each step selects a different node', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const canvas = page.locator('#graph-canvas');
      await canvas.focus();
      const seen = [];
      for (let i = 0; i < 4; i++) {
        await canvas.press('Tab');
        await page.waitForTimeout(300);
        await expect(page.locator('#graph-inspector'), `Tab step ${i} opens the inspector`).toBeVisible();
        expect((await page.locator('#graph-inspector').innerText()).trim().length,
          `Tab step ${i} filled the inspector`).toBeGreaterThan(20);
        seen.push(new URL(page.url()).searchParams.get('sel'));
      }
      expect(new Set(seen).size, `Tab walked four distinct nodes: ${JSON.stringify(seen)}`).toBe(4);
      assertClean(page, `${repo.name} keyboard walk`);
    });

    test('keyboard + − and 0 zoom the canvas', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const canvas = page.locator('#graph-canvas');
      const readout = page.locator('#zoom-readout');
      await canvas.focus();
      const start = await readout.innerText();
      await canvas.press('+');
      await page.waitForTimeout(150);
      const up = await readout.innerText();
      expect(parseInt(up), `"+" zoomed: ${start} → ${up}`).toBeGreaterThan(parseInt(start));
      await canvas.press('-');
      await canvas.press('-');
      await page.waitForTimeout(150);
      const down = await readout.innerText();
      expect(parseInt(down), `"-" zoomed out: ${up} → ${down}`).toBeLessThan(parseInt(up));
      await canvas.press('0');
      await page.waitForTimeout(250);
      expect(await readout.innerText(), '"0" refits').toBe(start);
    });

    test('the minimap is drawn and clicking it moves the viewport', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const drew = await page.evaluate(() =>
        (window.__drawnText || []).length >= 0 && (() => {
          const c = document.getElementById('graph-minimap');
          const ctx = c.getContext('2d');
          const d = ctx.getImageData(0, 0, c.width, c.height).data;
          let painted = 0;
          for (let i = 3; i < d.length; i += 4) if (d[i] > 0) painted++;
          return painted;
        })());
      expect(drew, 'the minimap has painted pixels').toBeGreaterThan(50);
      const before = await repaintAndCapture(page);
      await page.locator('#graph-minimap').scrollIntoViewIfNeeded();
      const mini = await page.locator('#graph-minimap').boundingBox();
      await page.mouse.click(mini.x + mini.width * 0.15, mini.y + mini.height * 0.15);
      await page.waitForTimeout(300);
      await resetRecorder(page);
      await page.locator('#graph-canvas').press('ArrowLeft');
      await page.waitForTimeout(250);
      const after = await drawnLabels(page);
      const common = before.find((b) => after.some((a) => a.text === b.text));
      expect(common, 'a label survives the minimap jump').toBeTruthy();
      const moved = after.find((a) => a.text === common.text);
      expect(Math.abs(moved.x - common.x) + Math.abs(moved.y - common.y),
        'clicking the minimap moved the viewport').toBeGreaterThan(10);
    });

    test('the relationships table is a text equivalent with a checkable file and line', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const table = page.locator('#graph-table');
      const text = await table.innerText();
      expect(text, 'the table names itself and its size').toMatch(/Relationships, with evidence/);
      expect(text, 'the table states how many pairs').toMatch(/\d+ pairs?/);
      assertNoPlaceholders(text, `${repo.name} graph table`);
      const rows = await table.locator('tbody tr').count();
      const pairs = Number((text.match(/([\d,]+) pairs?/) || [])[1].replace(/,/g, ''));
      if (pairs > 0) {
        expect(rows, 'the table draws the pairs it claims').toBeGreaterThan(0);
        const ev = table.locator('tbody tr .ev .p').first();
        await expect(ev, 'every relationship carries a file:line').toBeVisible();
        expect((await ev.innerText()).trim(), 'evidence is a real path').toMatch(/\S+(:\d+)?/);
      } else {
        expect(rows, 'zero pairs draws zero rows').toBe(0);
      }
    });

    /*
      The map speaks in sentences. An edge is "web uses registry, 12 times",
      not "CALLS x12"; its weakest tier is a word, not an id; and the answer
      line says both figures the picture could be read as — the PAIRS it draws
      and the OCCURRENCES behind them — rather than one number that matches
      neither.
    */
    test('the relationships table and the answer line speak in sentences', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      await settle(page, 45_000);
      const head = (await page.locator('#graph-head .answer h2').first().innerText()).replace(/\s+/g, ' ').trim();
      expect(head, 'the answer line states pairs and occurrences, and names which is which')
        .toMatch(/\d[\d,]* relationships? between them \(\d[\d,]* uses? in all\)/);

      const rows = page.locator('#graph-table tbody tr');
      expect(await rows.count(), 'the shipped fixture has at least one pair to describe').toBeGreaterThan(0);
      const first = (await rows.first().innerText()).replace(/\s+/g, ' ').trim();
      expect(first, 'a row is a sentence: who uses whom, how many times').toMatch(/ uses .+, \d[\d,]* times?\b/);
      expect(first, 'no edge-kind code or multiplication sign stands in for the sentence').not.toMatch(/CALLS x|×/);
      const tier = (await rows.first().locator('.tier').first().innerText()).trim();
      expect(tier, 'the weakest tier is a word').toMatch(/^(✓ ?)?(proven|very likely|uncertain|a guess|unmeasured)$/);
      await expect(rows.first().locator('.tier').first()).toHaveAttribute('data-tier', /exact|high|medium|low|unknown/);
    });

    test('the DETAIL LEVEL select changes the level and the URL', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const before = (await page.locator('#canvas-note').innerText()).trim();
      await page.selectOption('#graph-level', 'files');
      await settle(page);
      await page.waitForTimeout(500);
      expect(new URL(page.url()).searchParams.get('level'), '?level= follows the select').toBe('files');
      const after = (await page.locator('#canvas-note').innerText()).trim();
      expect(after, `level=files draws a different set: "${before}" → "${after}"`).not.toBe(before);
      assertClean(page, `${repo.name} graph level`);
    });

    test('the GROUP BY select changes the grouping and the URL', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const before = (await page.locator('#canvas-note').innerText()).trim();
      await expect(page.locator('#graph-group'), 'the default grouping is automatic — it collapses only when it must')
        .toHaveValue('auto');
      await openFilters(page);
      await page.selectOption('#graph-group', 'none');
      await page.waitForTimeout(600);
      expect(new URL(page.url()).searchParams.get('group'), '?group= follows the select').toBe('none');
      const after = (await page.locator('#canvas-note').innerText()).trim();
      expect(after, `group=none draws a different set: "${before}" → "${after}"`).not.toBe(before);
      await openFilters(page);
      await page.selectOption('#graph-group', 'folder');
      await page.waitForTimeout(600);
      expect(new URL(page.url()).searchParams.get('group')).toBe('folder');
      assertClean(page, `${repo.name} graph grouping`);
    });

    test('the LANGUAGE select narrows the graph and rides in the URL', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const options = await page.locator('#graph-language option').evaluateAll((o) => o.map((x) => x.value));
      const java = options.find((v) => v === 'java');
      expect(java, `the language select offers java (offered: ${options.join(', ')})`).toBeTruthy();
      await openFilters(page);
      await page.selectOption('#graph-language', 'java');
      await settle(page);
      await page.waitForTimeout(400);
      expect(new URL(page.url()).searchParams.get('lang'), '?lang= follows the select').toBe('java');
      const labels = await drawnLabels(page);
      expect(labels.length, 'the java-only graph still draws').toBeGreaterThan(0);
      assertClean(page, `${repo.name} graph language`);
    });

    test('the EDGE KINDS chips filter, toggle, and reset to All', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      await openFilters(page);
      const chips = page.locator('#graph-kinds button');
      const n = await chips.count();
      expect(n, 'an All chip plus one per edge kind').toBeGreaterThan(1);
      await expect(chips.first(), 'All is pressed by default').toHaveAttribute('aria-pressed', 'true');
      const firstKind = chips.nth(1);
      const kindLabel = (await firstKind.innerText()).trim();
      await firstKind.click();
      await settle(page);
      await page.waitForTimeout(400);
      await expect(page.locator('#graph-kinds button').nth(1), `${kindLabel} chip is pressed`)
        .toHaveAttribute('aria-pressed', 'true');
      expect(new URL(page.url()).searchParams.get('kinds'), '?kinds= follows the chip').toContain(kindLabel);
      await page.locator('#graph-kinds button').first().click();
      await settle(page);
      await page.waitForTimeout(400);
      expect(new URL(page.url()).searchParams.get('kinds') || '', 'All clears ?kinds=').toBe('');
      assertClean(page, `${repo.name} edge kind chips`);
    });

    test('the SCOPE combobox searches containers and scopes the graph', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const input = page.locator('#graph-container');
      await input.click();
      await input.fill(repo.containerNeedle);
      await page.waitForTimeout(600);
      // The graph screen carries two comboboxes now — the scope picker and the
      // search box — so a test names the listbox it means by its own id.
      const list = page.locator('#graph-container-list');
      await expect(list, 'the combobox opens a listbox').toBeVisible();
      const opts = list.locator('button.combo-opt');
      const count = await opts.count();
      expect(count, `the combobox found containers matching "${repo.containerNeedle}"`).toBeGreaterThan(0);
      // Every row is now a real container: "All, aggregated" is offered only
      // when the box is empty, so it can never be the first hit for a query.
      const real = opts.first();
      const chosen = (await real.locator('.main').innerText()).trim();
      await real.click();
      await settle(page);
      await page.waitForTimeout(600);
      const container = new URL(page.url()).searchParams.get('container');
      expect(container, `?container= follows the choice "${chosen}"`).toBeTruthy();
      assertClean(page, `${repo.name} scope combobox`);
    });

    test('the combobox is keyboard operable: arrows move the active option, Escape closes', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const input = page.locator('#graph-container');
      await input.click();
      await input.fill(repo.containerNeedle);
      await page.waitForTimeout(600);
      await input.press('ArrowDown');
      await page.waitForTimeout(200);
      const active = await page.locator('#graph-container-list .combo-opt[aria-selected="true"]').count();
      expect(active, 'ArrowDown marks an active option').toBe(1);
      await expect(input, 'the input points at the active option')
        .toHaveAttribute('aria-activedescendant', /combo-opt-/);
      await input.press('Escape');
      await page.waitForTimeout(200);
      await expect(page.locator('#graph-container-list'), 'Escape closes the listbox').toBeHidden();
    });

    test('Export JSON and Export PNG each produce a download', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      for (const label of ['Export JSON', 'Export PNG']) {
        const [download] = await Promise.all([
          page.waitForEvent('download', { timeout: 20_000 }),
          page.locator('#screen-graph button.chip', { hasText: label }).click(),
        ]);
        const name = download.suggestedFilename();
        expect(name, `${label} names its file`).toMatch(label === 'Export JSON' ? /\.json$/ : /\.png$/);
        const stream = await download.createReadStream();
        let bytes = 0;
        for await (const chunk of stream) bytes += chunk.length;
        expect(bytes, `${label} produced a non-empty file`).toBeGreaterThan(100);
      }
      assertClean(page, `${repo.name} graph exports`);
    });

    test('the graph states what it left out', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const text = await page.locator('#graph-omissions').innerText();
      expect(text, 'the omissions block names itself').toMatch(OMISSIONS_STATED);
      assertNoPlaceholders(text, `${repo.name} graph omissions`);
    });

    /*
      LABELS NAME WHAT DIFFERS, ON THE MAP. A container or file node is drawn
      by the shortest tail of its path that no drawn sibling of its kind also
      ends in. At the files level that is the basename wherever no other file
      shares it — `CallbackController.java`, never `…/pay/controller/
      CallbackController.java`. At the container level the shipped fixture
      holds src/main/java/com/pay/controller AND src/test/java/com/pay/controller,
      and two nodes that both read `controller` are the one thing a map must
      never draw.

      The node set comes from the same /api/graph answer the page drew, and the
      labels from the fillText recorder; app.js is a closure, so the render set
      itself is not reachable from a test. A file node also draws its directory
      as a sub-line, so "every drawn string is a basename" is not the claim —
      the claim is that each basename is drawn as itself, and that nothing
      longer ending in it is drawn instead.
    */
    test('a file node is drawn by its basename, and twin containers are drawn apart', async ({ page }) => {
      const baseOf = (p) => p.slice(p.lastIndexOf('/') + 1);
      const titles = async () => (await repaintAndCapture(page)).filter(isNodeTitle).map((l) => l.text.trim());
      const nodesOf = async (query) => {
        const res = await page.request.get(`${base(repo)}/api/graph${query}`, { headers: { Accept: 'application/json' } });
        expect(res.ok(), `/api/graph${query} answers`).toBe(true);
        return res.json();
      };

      // loadGraph picks a scope for the files level when none was named; the
      // URL records which, so the same answer can be fetched again here. The
      // grouping is switched off because the claim is about LEAF labels: on a
      // large scope the automatic grouping draws only clusters, and a cluster
      // is named by disambiguateLabels, not by this rule.
      await cold(page, repo, '?s=graph&level=files&group=none');
      const scope = new URL(page.url()).searchParams.get('container');
      expect(scope, 'the files level names its scope in the URL').toBeTruthy();
      const files = ((await nodesOf(`?level=files&container=${encodeURIComponent(scope)}`)).files || [])
        .map((f) => String(f.path));
      expect(files.length, `the scope ${scope} holds files`).toBeGreaterThan(0);
      const drawnFiles = await titles();
      const shared = new Map();
      for (const f of files) shared.set(baseOf(f), (shared.get(baseOf(f)) || 0) + 1);
      let asBasename = 0;
      for (const f of files) {
        const name = baseOf(f);
        if (shared.get(name) !== 1) continue;
        // '/'-aligned: AxisCheckStatusRepository.java is not a longer path to CheckStatusRepository.java.
        const longer = drawnFiles.filter((t) => t.endsWith('/' + name));
        expect(longer, `${f} has no twin, so it is drawn as ${name} and never as a longer path ending in it`).toEqual([]);
        if (drawnFiles.includes(name)) asBasename++;
      }
      if (asBasename === 0) {
        test.skip(true, `${repo.name}: no file leaf of ${scope} is drawn at the default view (drawn: ${drawnFiles.join(' | ')}); nothing to name`);
      }

      await cold(page, repo, '?s=graph');
      const containers = ((await nodesOf('')).containers || []).map((c) => String(c.id));
      const drawnContainers = await titles();
      const byName = new Map();
      for (const c of containers) {
        if (!byName.has(baseOf(c))) byName.set(baseOf(c), []);
        byName.get(baseOf(c)).push(c);
      }
      const twins = [...byName.entries()].filter(([, group]) => group.length > 1);
      if (key === 'payment') {
        expect(twins.map(([name]) => name), 'the shipped fixture has two controller packages').toContain('controller');
      }
      for (const [name, group] of twins) {
        expect(drawnContainers, `no node reads bare "${name}" when ${group.length} containers end in it`).not.toContain(name);
        // A drawn label wider than its box keeps its END behind an ellipsis.
        const drawn = drawnContainers.filter((t) => baseOf(t.replace(/^…/, '')) === name);
        if (key === 'payment') expect(drawn.length, `both ${name} containers are drawn`).toBe(group.length);
        expect(new Set(drawn).size, `the ${name} containers are drawn apart: ${drawn.join(' | ')}`).toBe(drawn.length);
        for (const t of drawn) {
          expect(group.some((c) => c.endsWith(t.replace(/^…/, ''))), `"${t}" is a tail of one of ${group.join(', ')}`).toBe(true);
        }
      }
      assertClean(page, `${repo.name} map labels`);
    });

    /*
      A NODE'S SIZE SAYS HOW MUCH IS IN IT. A container holding ten thousand
      symbols and one holding ten used to draw the same box if their labels
      were the same length; width now carries the count, on a log scale so the
      big one is wider without being a thousand times wider, and both stay
      inside [NODE_MIN_W, NODE_MAX_W]. The counts come from the same /api/graph
      answer the page drew; the widths from the metrics fit() writes onto the
      canvas, keyed the way the render set keys a leaf.
    */
    test('a container drawn with more symbols is drawn wider, within the clamp', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      const res = await page.request.get(`${base(repo)}/api/graph`, { headers: { Accept: 'application/json' } });
      expect(res.ok(), '/api/graph answers').toBe(true);
      const containers = (await res.json()).containers || [];
      const m = await graphMetrics(page);
      const widthOf = new Map((m.widths || []).map((w) => [w.key, w.w]));
      const drawn = containers.filter((c) => widthOf.has('L:' + String(c.id)));
      const counts = new Set(drawn.map((c) => Number(c.symbols || 0)));
      test.skip(drawn.length < 2 || counts.size < 2,
        `${repo.name}: ${drawn.length} container leaves drawn with ${counts.size} distinct symbol counts; nothing to compare`);
      const most = drawn.reduce((a, c) => (Number(c.symbols) > Number(a.symbols) ? c : a));
      const fewest = drawn.reduce((a, c) => (Number(c.symbols) < Number(a.symbols) ? c : a));
      expect(widthOf.get('L:' + most.id),
        `${most.id} (${most.symbols} symbols, ${widthOf.get('L:' + most.id)}px) is wider than ` +
        `${fewest.id} (${fewest.symbols} symbols, ${widthOf.get('L:' + fewest.id)}px)`)
        .toBeGreaterThan(widthOf.get('L:' + fewest.id));
      for (const w of m.widths) {
        expect(w.w, `${w.key} is no narrower than NODE_MIN_W`).toBeGreaterThanOrEqual(74);
        expect(w.w, `${w.key} is no wider than NODE_MAX_W`).toBeLessThanOrEqual(260);
      }
      assertClean(page, `${repo.name} node quantity`);
    });

    /*
      THE LAYOUT IS DETERMINISTIC. The same URL, loaded twice from cold, puts
      every node at the same world coordinate and routes every edge through
      the same points. A reader compares two runs of the same repository on
      this map, and a map that reshuffles itself on reload is one they
      cannot compare against.
    */
    test('the layout is identical across two cold loads of the same URL', async ({ page }) => {
      await cold(page, repo, '?s=graph');
      await settle(page, 45_000);
      const a = await nodeGeometry(page);
      await page.reload();
      await settle(page, 45_000);
      const b = await nodeGeometry(page);
      expect(a.nodes.length, 'the first load drew nodes').toBeGreaterThan(0);
      // Nodes and routed edges alike: a via that lands somewhere else on
      // the second load is an edge drawn differently.
      expect(b).toEqual(a);
    });

    /*
      THE LAYOUT FOLLOWS DEPENDENCY DIRECTION. Nodes are ranked by the longest
      path over the drawn edges with back-edges ignored, and a lower rank is
      drawn ABOVE a higher one - callers over callees - so every edge that is
      not part of a cycle points down. Both halves are asserted: the rank the
      layout reports for the caller is lower than the callee's, AND the
      caller's y is smaller, since a rank that wraps onto several rows is
      still a rank, and the promise is about the rows too. The drawn nodes,
      their positions and their ranks come from the metrics hook; the edges
      from the same /api/graph answer the page drew, restricted to the pairs
      that were both drawn, which is the graph the layout was given. Grouping
      is off so the drawn nodes are the containers themselves rather than
      clusters the API knows nothing about.
    */
    test('every edge that is not a back-edge points from a lower rank above to a higher rank below', async ({ page }) => {
      await cold(page, repo, '?s=graph&group=none');
      await settle(page, 45_000);
      const geometry = (await nodeGeometry(page)).nodes;
      const at = new Map(geometry.map((g) => [g.key, g]));
      const edges = await drawnContainerEdges(page, repo, geometry);
      const connected = new Set(edges.flatMap((e) => [e.a, e.b]));
      test.skip(connected.size < 2, `${repo.name}: the container level draws ${connected.size} connected nodes; nothing to rank`);
      for (const g of geometry) expect(Number.isInteger(g.rank), `${g.key} reports an integer rank (${g.rank})`).toBe(true);
      const back = backEdgesOf([...at.keys()], edges);
      const forward = edges.filter((e) => !back.has(e.a + ' ' + e.b));
      expect(forward.length, `some edge survives cycle removal (${back.size} of ${edges.length} are back-edges)`).toBeGreaterThan(0);
      const describe = (e) => `${e.a} (rank ${at.get(e.a).rank}, y ${at.get(e.a).y}) -> ${e.b} (rank ${at.get(e.b).rank}, y ${at.get(e.b).y})`;
      const wrongRank = forward.filter((e) => !(at.get(e.a).rank < at.get(e.b).rank)).map(describe);
      expect(wrongRank, `${forward.length} forward edges all descend a rank; ${back.size} back-edges ignored`).toEqual([]);
      const wrongY = forward.filter((e) => !(at.get(e.a).y < at.get(e.b).y)).map(describe);
      expect(wrongY, `${forward.length} forward edges all point down the page; ${back.size} back-edges ignored`).toEqual([]);
    });

    /*
      NO EDGE IS DRAWN THROUGH A BOX. Two promises, both asserted here.

      The first layered layout ranked isolated nodes with the roots, so on
      this fixture's container view rank 0 wrapped onto two rows and the
      second row - two test directories and unrelated/marketing, none with
      an edge - sat between the controller and the service it calls, with
      the edge drawn through one of them. Isolated nodes now sit in a strip
      of their own below the last rank, and every isolated node is drawn
      below every connected one.

      Then the stronger one, which this test did not make until edges were
      routed: no edge's drawn path - the polyline the renderer builds out of
      a port, through each via, into a port, sampled 21 times per segment -
      lands inside ANY box other than its own two ends. Before routing, a
      chord from centre to centre ran through whatever box the barycentre
      put between a caller and a callee two ranks apart: on this fixture
      service -> model through repository, 7 of 21 samples; on a dense
      17-component view 48 of 67 edges had a sample in some box. The layout
      now gives such an edge a dummy in every row it crosses, and the
      renderer draws through them; this is what proves the two agree.

      Three views, because the container level with grouping off is the
      one where every box is a leaf of one height: the grouped landing view
      mixes 40 px leaves with 54 px clusters in one row, which is where a
      port inside the row's band clipped a neighbour's corner, and the
      drilled map is where the ranks are long. The container level's edges
      are cross-checked against /api/graph; the other two views' keys are
      not container ids, so their edges come from the layout's own list.
      An edge over the dummy budget is drawn as a chord and may cross a
      box; how many were is reported as a note, not a failure, so a
      budget that starts biting on a fixture is visible in the report.
    */
    const checkNoEdgeThroughBox = async (page, query, edgesOf) => {
      await cold(page, repo, query);
      await settle(page, 45_000);
      const geometry = await nodeGeometry(page);
      const at = new Map(geometry.nodes.map((g) => [g.key, g]));
      const viaOf = new Map(geometry.vias.map((v) => [v.from + '\u0000' + v.to, v.points]));
      const edges = await edgesOf(geometry);
      test.skip(edges.length < 1, `${repo.name}: ${query} draws ${edges.length} edges; nothing to be in the way of`);
      const connected = new Set(edges.flatMap((e) => [e.a, e.b]));
      const isolated = geometry.nodes.filter((g) => !connected.has(g.key));
      if (isolated.length > 0) {
        const lowest = Math.max(...geometry.nodes.filter((g) => connected.has(g.key)).map((g) => g.y));
        const above = isolated.filter((g) => !(g.y > lowest)).map((g) => `${g.key} (y ${g.y}, rank ${g.rank})`);
        expect(above, `${isolated.length} isolated nodes all sit below the lowest connected node (y ${lowest})`).toEqual([]);
      }
      // 2 px of slack each way for the stroke's half-width: a sample on the
      // centre line can be outside a box while the stroke's edge is not.
      const inside = (p, g) => Math.abs(p.x - g.x) <= g.w / 2 + 2 && Math.abs(p.y - g.y) <= g.h / 2 + 2;
      const through = [];
      let routed = 0;
      let chords = 0;
      for (const e of edges) {
        const via = viaOf.get(e.a + '\u0000' + e.b) || [];
        if (via.length) routed++;
        else if (Math.abs(at.get(e.a).rank - at.get(e.b).rank) >= 2) chords++;
        const hit = new Map();
        for (const p of strokeSamples(at.get(e.a), at.get(e.b), via)) {
          for (const g of geometry.nodes) {
            if (g.key === e.a || g.key === e.b || !inside(p, g)) continue;
            if (!hit.has(g.key)) hit.set(g.key, p.at);
          }
        }
        for (const [key, where] of hit) through.push(`${e.a} -> ${e.b} (${via.length} vias) passes through ${key}, first at ${where}`);
      }
      test.info().annotations.push({
        type: 'chord fallbacks',
        description: `${repo.name} ${query}: ${chords} of ${edges.length} edges span 2+ ranks and were drawn as chords (no vias); ${routed} routed`,
      });
      expect(through,
        `${edges.length} drawn edges (${routed} routed through dummies), none passing through any of ${geometry.nodes.length} boxes`)
        .toEqual([]);
      assertClean(page, `${repo.name} edges clear of every box on ${query}`);
    };
    const layoutEdges = (geometry) => {
      const drawn = new Set(geometry.nodes.map((g) => g.key));
      return geometry.edges.filter((e) => e.a !== e.b && drawn.has(e.a) && drawn.has(e.b));
    };

    test('no edge runs through any box but its own two ends', async ({ page }) => {
      await checkNoEdgeThroughBox(page, '?s=graph&group=none', (geometry) => drawnContainerEdges(page, repo, geometry.nodes));
    });

    test('no edge runs through any box on the grouped landing view', async ({ page }) => {
      await checkNoEdgeThroughBox(page, '?s=graph', layoutEdges);
    });

    test('no edge runs through any box on a drilled map', async ({ page }) => {
      // The level the treemap descends to: its largest directory, opened, read
      // back out of the URL it wrote - and then the MAP of that container.
      await cold(page, repo, '?s=graph&gv=treemap');
      await settle(page, 45_000);
      const cell = page.locator('#graph-treemap-host .cm-cell').first();
      await expect(cell, 'the treemap drew at least one directory').toBeVisible();
      await cell.dblclick();
      await expect.poll(() => new URL(page.url()).searchParams.get('container'), { timeout: 20_000, message: 'the treemap opened a directory' }).toBeTruthy();
      const container = new URL(page.url()).searchParams.get('container');
      await checkNoEdgeThroughBox(page, `?s=graph&container=${encodeURIComponent(container)}&level=files`, layoutEdges);
    });

    test('a deep-linked graph state is restored on a COLD load', async ({ page }) => {
      await cold(page, repo, '?s=graph&group=folder&lang=java&floor=low');
      await expect(page.locator('#graph-group')).toHaveValue('folder');
      await expect(page.locator('#graph-language')).toHaveValue('java');
      await expect(page.locator('#indexbar .tierbar button.t-low')).toHaveAttribute('aria-pressed', 'true');
      const labels = await drawnLabels(page);
      expect(labels.filter((l) => l.text.trim().length > 1).length,
        'the deep-linked graph still draws labels').toBeGreaterThan(0);
      assertClean(page, `${repo.name} graph deep link`);
    });
  });
}
