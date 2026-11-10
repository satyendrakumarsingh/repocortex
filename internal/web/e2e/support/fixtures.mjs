import { test as base, expect } from '@playwright/test';
import { base as origin } from './ports.mjs';

/*
  Every page in this suite carries three always-on recorders:

    page.consoleErrors  — console.error / console.warn-level messages
    page.pageErrors     — uncaught exceptions
    page.badResponses   — any same-origin response with status >= 400

  The canvas recorder is installed before any script runs and captures every
  CanvasRenderingContext2D.fillText call together with the transform in force at
  the time, so a test can assert that TEXT WAS DRAWN and can convert a drawn
  label back into a clickable screen coordinate.
*/
export const test = base.extend({
  page: async ({ page }, use) => {
    const consoleErrors = [];
    const pageErrors = [];
    const badResponses = [];

    page.on('console', (m) => { if (m.type() === 'error') consoleErrors.push(m.text()); });
    page.on('pageerror', (e) => pageErrors.push(`${e.name}: ${e.message}`));
    page.on('response', (r) => {
      if (r.status() >= 400) badResponses.push(`${r.status()} ${r.url()}`);
    });

    await page.addInitScript(() => {
      window.__drawnText = [];
      const proto = CanvasRenderingContext2D.prototype;
      const original = proto.fillText;
      proto.fillText = function (text, x, y, maxWidth) {
        try {
          const m = this.getTransform();
          window.__drawnText.push({
            text: String(text), x, y,
            sx: m.a * x + m.c * y + m.e,
            sy: m.b * x + m.d * y + m.f,
            align: this.textAlign,
            canvasId: this.canvas && this.canvas.id,
          });
        } catch (_) { /* recorder must never break the page */ }
        return maxWidth === undefined ? original.call(this, text, x, y) : original.call(this, text, x, y, maxWidth);
      };
    });

    page.consoleErrors = consoleErrors;
    page.pageErrors = pageErrors;
    page.badResponses = badResponses;
    await use(page);
  },
});

export { expect };

/* Navigate cold: a fresh document load straight at the deep link. */
export async function cold(page, repo, query = '') {
  await page.goto(`${origin(repo)}/${query}`, { waitUntil: 'domcontentloaded' });
  await settle(page);
}

/* Wait until no loading spinner is left on a visible screen. */
export async function settle(page, ms = 20_000) {
  await page.waitForFunction(() => {
    const screens = [...document.querySelectorAll('section.screen')].filter((s) => !s.hidden);
    if (screens.length === 0) return false;
    return screens.every((s) => s.querySelectorAll('.loading').length === 0);
  }, null, { timeout: ms });
  await page.waitForTimeout(250);
}

/*
  Open every fold on the page. A screen opens with its answer and its evidence
  and caveats folded; a spec that reads the rows or the counts inside a fold
  opens them first, the way a reader would, and asserts the same things.
*/
/*
  What a screen says about its omissions: the block that lists them, or —
  when the index held nothing back for the question — the one sentence that
  says so. A spec that asks "does this screen state what it left out" accepts
  either, because both are the statement.
*/
export const OMISSIONS_STATED = /What this view can't see|Nothing was left out of this view/;

export async function openFolds(page) {
  await page.evaluate(() => { for (const d of document.querySelectorAll('details.fold')) d.open = true; });
  await page.waitForTimeout(50);
}

/*
  The map's five filters — grouping, language, edge kinds, standard of proof,
  focus depth — live in a popover behind the Filters button. A spec that drives
  one of them opens the popover first, the way a reader would; the control's
  id, handler and query parameter are unchanged, only its box moved.
*/
export async function openFilters(page) {
  const pop = page.locator('#graph-filters-pop');
  if (await pop.isHidden()) await page.locator('#graph-filters').click();
  await pop.waitFor({ state: 'visible' });
}

export function visibleScreen(page) {
  return page.locator('section.screen:not([hidden])');
}

export async function screenText(page) {
  return page.evaluate(() => {
    const s = [...document.querySelectorAll('section.screen')].find((x) => !x.hidden);
    return s ? (s.innerText || '') : '';
  });
}

/* The whole rendered page as a user reads it: rail, index bar, screen, footer. */
export async function bodyText(page) {
  return page.evaluate(() => document.body.innerText || '');
}

export function assertClean(page, where) {
  expect(page.pageErrors, `uncaught page exceptions on ${where}`).toEqual([]);
  expect(page.consoleErrors, `console errors on ${where}`).toEqual([]);
}

/* Failure mode 2: three literal strings that must never reach a reader. */
export const FORBIDDEN = ['undefined', 'NaN', '[object Object]'];

export function assertNoPlaceholders(text, where) {
  for (const bad of FORBIDDEN) {
    const hits = [...text.matchAll(new RegExp(bad.replace(/[[\]]/g, '\\$&'), 'g'))];
    if (hits.length) {
      const around = hits.map((h) => text.slice(Math.max(0, h.index - 70), h.index + 70).replace(/\n/g, ' ⏎ '));
      expect(hits.length, `"${bad}" rendered on ${where}:\n  ${around.join('\n  ')}`).toBe(0);
    }
  }
}

export async function horizontalOverflow(page) {
  return page.evaluate(() => {
    const doc = document.documentElement;
    const over = [];
    if (doc.scrollWidth > doc.clientWidth + 1) {
      // An element wider than the viewport inside its own overflow-x container
      // is by design; only an uncontained one can widen the document.
      const contained = (node) => {
        let a = node.parentElement;
        while (a && a !== document.body) {
          const ox = getComputedStyle(a).overflowX;
          if (ox === 'auto' || ox === 'scroll' || ox === 'hidden') return true;
          a = a.parentElement;
        }
        return false;
      };
      for (const node of document.querySelectorAll('body *')) {
        const r = node.getBoundingClientRect();
        if (r.width > 0 && r.right > doc.clientWidth + 1 && !contained(node)) {
          over.push(`${node.tagName}.${String(node.className).slice(0, 30)} right=${Math.round(r.right)} text="${(node.textContent || '').trim().slice(0, 40)}"`);
          if (over.length > 6) break;
        }
      }
    }
    return { scrollWidth: doc.scrollWidth, clientWidth: doc.clientWidth, culprits: over };
  });
}

/* ------------------------------------------------------------------ canvas

  THE GRAPH CANVAS, AS A TESTABLE SURFACE.

  Everything below exists because a canvas has no DOM: the only proof that a
  label was drawn is the fillText the recorder above captured, and the only way
  to click a node is to convert that capture back into a screen coordinate.

  The screen also does not fit in a 900px viewport — the graph explorer carries
  an index bar, a heading, a scope banner, a control row, a lens row and then a
  570px canvas — so a label can be drawn, be perfectly visible to a reader who
  scrolls, and still sit below the fold at the moment a test clicks. Clicking
  page coordinates outside the viewport silently does nothing, which is exactly
  the failure that looks like a product bug and is not one.

  So: scroll the canvas into view, re-read its box, and only ever offer a test
  the labels whose click point is actually inside the viewport. A test that
  finds none of them says so rather than clicking into space.
*/

/* Every label drawn on one canvas, in CSS pixels relative to that canvas. */
export async function drawnOn(page, canvasId = 'graph-canvas') {
  return page.evaluate((id) => {
    const dpr = window.devicePixelRatio || 1;
    return (window.__drawnText || [])
      .filter((t) => t.canvasId === id)
      .map((t) => ({ text: t.text, x: t.sx / dpr, y: t.sy / dpr, align: t.align }));
  }, canvasId);
}

export async function resetRecorder(page) {
  await page.evaluate(() => { window.__drawnText = []; });
}

/* Force one repaint and capture only what that frame drew. */
export async function repaint(page) {
  await resetRecorder(page);
  await page.locator('#canvas-tools button', { hasText: 'Fit' }).click();
  await page.waitForTimeout(400);
  return drawnOn(page);
}

/*
  A node TITLE, as opposed to the sub-line or the cluster "+". The sub-line is
  "N items" under a collapsed group and "N files with symbols" under a
  container; both are drawn left-aligned like a title, and a small map with no
  groups to collapse is where the second one turns up.
*/
export const SUB_LINE = /^\d+ (items?|files with symbols)$/;

export function isNodeTitle(l) {
  const t = String(l.text || '').trim();
  return l.align === 'left' && t.length > 2 && !SUB_LINE.test(t);
}

/*
  Node titles that can actually be clicked right now: the canvas is scrolled
  into view first, so the returned points are inside the viewport.
*/
export async function clickableLabels(page) {
  const canvas = page.locator('#graph-canvas');
  // ONE FRAME, NOT THE WHOLE SESSION. The recorder accumulates every fillText
  // since the page loaded, including the frames drawn while the layout was
  // still settling. Positions from those frames are stale by hundreds of
  // pixels, and clicking one lands on empty canvas — which reads as "the node
  // is not clickable" and is really "that is where the node used to be".
  await repaint(page);
  await canvas.scrollIntoViewIfNeeded();
  await page.waitForTimeout(120);
  const box = await canvas.boundingBox();
  const vp = page.viewportSize();
  const labels = await drawnOn(page);
  const ok = labels.filter(isNodeTitle).filter((l) => {
    const px = box.x + l.x + 6;
    const py = box.y + l.y;
    return px > box.x + 2 && px < box.x + box.width - 2
      && py > Math.max(4, box.y) && py < Math.min(vp.height - 4, box.y + box.height - 2);
  });
  return { box, labels, clickable: ok };
}

/* Click one label returned by clickableLabels, re-scrolling first. */
export async function clickLabel(page, label) {
  const canvas = page.locator('#graph-canvas');
  await canvas.scrollIntoViewIfNeeded();
  await page.waitForTimeout(80);
  const box = await canvas.boundingBox();
  await page.mouse.click(box.x + label.x + 6, box.y + label.y);
  await page.waitForTimeout(420);
}

export async function dblClickLabel(page, label) {
  const canvas = page.locator('#graph-canvas');
  await canvas.scrollIntoViewIfNeeded();
  await page.waitForTimeout(80);
  const box = await canvas.boundingBox();
  await page.mouse.dblclick(box.x + label.x + 6, box.y + label.y);
  await page.waitForTimeout(620);
}

/* The "N drawn" figure the canvas prints about itself. */
export async function drawnCount(page) {
  const note = (await page.locator('#canvas-note').innerText()).trim();
  const m = note.match(/([\d,]+) drawn/);
  return { note, drawn: m ? Number(m[1].replace(/,/g, '')) : NaN };
}

/*
  The numbers fit() decided with, read from the data-graph-metrics attribute
  the canvas writes after every fit: node count, mean/min/max measured width,
  the layout's box-derived rest length, the zoom, whether that zoom sits on the
  legibility floor, and — once the layout has settled — every node's width by
  key. Polls until settled, then waits a beat, because a settled map may still
  be rebuilt once by fitBudgetToWindow when it overflowed.
*/
export async function graphMetrics(page, ms = 60_000) {
  const read = async () => {
    const raw = await page.locator('#graph-canvas').getAttribute('data-graph-metrics');
    return raw ? JSON.parse(raw) : null;
  };
  await expect.poll(async () => { const m = await read(); return !!(m && m.settled); },
    { timeout: ms, message: 'the map settled and reported its metrics' }).toBe(true);
  await page.waitForTimeout(600);
  await expect.poll(async () => { const m = await read(); return !!(m && m.settled); },
    { timeout: ms, message: 'the map is still settled after the budget check' }).toBe(true);
  return read();
}

/*
  The settled geometry: every drawn node's world position, sorted by key, and
  every routed edge's via points, sorted by its ends, so two loads of the same
  URL can be compared with toEqual. The layout is required to be deterministic
  - a map that moves on every reload is a map a reader cannot build a memory
  of - and this is the surface that proves it. Both come from the same hook as
  the widths, rounded to 0.1px there. Each node carries its box (w, h) and its
  layout rank and the height of the row band it sits in; each via carries the
  height of the row it crosses; so the structural specs can rebuild the path
  the renderer draws, ask whether it runs through a box, and whether an edge
  descends a rank. An edge drawn as a plain chord has no via entry. `edges` is
  every edge the layout was given, by drawn key, which is the only source of
  a grouped or drilled view's edge list: those views' keys are not the
  container ids /api/graph answers with.
*/
export async function nodeGeometry(page, ms = 60_000) {
  const m = await graphMetrics(page, ms);
  const byKey = (a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0);
  return {
    nodes: (m.positions || [])
      .map((p) => ({ key: p.key, x: p.x, y: p.y, w: p.w, h: p.h, band: p.band, rank: p.rank }))
      .sort(byKey),
    edges: (m.edges || []).map((e) => ({ a: e.a, b: e.b })),
    vias: (m.vias || [])
      .map((v) => ({ from: v.from, to: v.to, points: v.points.map((q) => ({ x: q.x, y: q.y, h: q.h })) }))
      .sort((a, b) => byKey({ key: a.from + '\u0000' + a.to }, { key: b.from + '\u0000' + b.to })),
  };
}
