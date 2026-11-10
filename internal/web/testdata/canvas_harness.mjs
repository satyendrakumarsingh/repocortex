/*
  Executable proof for the two canvas traps in assets/app.js.

  WHY THIS EXISTS

  The renderer had two guards that were written, commented, and never once run:

    1. THE SIZING TRAP. A canvas inside a `hidden` panel measures 0x0. A fit
       computed from 0x0 leaves the graph at minimum zoom in a corner, and it
       stays there, because nothing recomputes it when the panel is shown.
       measure() therefore refuses to size, fit or record anything until it sees
       a box of at least 1x1.

    2. THE SCROLL TRAP. A canvas that calls preventDefault on every wheel event
       traps the page: a reader scrolling past the graph gets stuck zooming it.
       So a plain wheel returns BEFORE preventDefault and only ctrl/meta zooms.

  Both are behaviour, not shape, so neither can be checked by reading the file.
  This harness runs the real code.

  HOW, WITHOUT A JS TOOLCHAIN

  app.js is one IIFE that boots against a live DOM, so it cannot be imported.
  Instead the two units under test are extracted from the source by pattern and
  rebuilt with `new Function`, then driven against a hand-written fake DOM.
  Nothing is installed, nothing is bundled, and the shipped portal is untouched:
  the only requirement is a `node` binary, and internal/web/canvas_test.go skips
  rather than fails when there is not one.

  The cost of the approach is stated rather than hidden: extraction is by
  pattern, so a reformat of those two blocks breaks the harness. That failure is
  LOUD — extraction failure is a failed assertion, not a silent skip — which is
  the trade that makes it acceptable.

  THE SEQUENCE IS THE TEST. Measuring a real box alone passes even when the
  recovery is broken. The case that matters is: measure while hidden, THEN show
  and measure again, and assert the deferred fit finally happens.

  Usage:  node canvas_harness.mjs <path to app.js>
  Exit:   0 when every assertion holds, 1 otherwise.
*/

import { readFileSync } from 'fs';

const path = process.argv[2];
if (!path) {
  console.error('usage: node canvas_harness.mjs <path to app.js>');
  process.exit(2);
}
const src = readFileSync(path, 'utf8');

let pass = 0, fail = 0;
const ok = (n) => { pass++; console.log(`  PASS  ${n}`); };
const bad = (n, d) => { fail++; console.log(`  FAIL  ${n}\n        ${d}`); };

// app.js reads window.devicePixelRatio inside measure().
global.window = { devicePixelRatio: 2 };

// ---------------------------------------------------------------------------
// Trap 1: sizing while hidden.
// ---------------------------------------------------------------------------

const m = src.match(/measure\s*\([^)]*\)\s*\{([\s\S]*?)\n  \}/);
if (!m) {
  bad('measure() can be located in app.js',
      'the harness extracts it by pattern; if measure() was reformatted, update this regex');
} else {
  const measure = new Function(`return function(){${m[1]}}`)();

  // A GraphView stand-in. fit() is counted rather than run: what is under test
  // is whether measure() DECIDES to fit, not what fit computes.
  //
  // nodes must be non-empty — the real fit() is guarded on this.nodes.length,
  // so an empty fixture would report a false failure.
  const mk = (w, h) => ({
    wrap: { getBoundingClientRect: () => ({ width: w, height: h }) },
    canvas: {
      style: {}, width: 0, height: 0,
      getBoundingClientRect: () => ({ width: w, height: h, left: 0, top: 0 }),
    },
    ctx: {
      setTransform() {}, scale() {}, clearRect() {}, save() {}, restore() {},
      beginPath() {}, moveTo() {}, lineTo() {}, stroke() {}, fill() {},
      arc() {}, arcTo() {}, fillText() {}, measureText: () => ({ width: 10 }),
      setLineDash() {}, translate() {}, closePath() {}, rect() {}, fillRect() {},
    },
    nodes: [{ x: 0, y: 0, w: 10, h: 10 }, { x: 40, y: 20, w: 10, h: 10 }],
    edges: [],
    view: { x: 0, y: 0, k: 1 },
    hasFit: false,
    width: 0, height: 0,
    fits: 0,
    fit() { this.fits++; },
    draw() {},
  });

  const hidden = mk(0, 0);
  try {
    measure.call(hidden);
    if (hidden.canvas.width === 0 && hidden.fits === 0 && hidden.hasFit === false) {
      ok('hidden panel (0x0): refuses to size, to fit, or to record a measurement');
    } else {
      bad('hidden panel (0x0)',
          `canvas.width=${hidden.canvas.width} fits=${hidden.fits} hasFit=${hidden.hasFit} — ` +
          `a fit against a 0x0 box pins the graph at minimum zoom in a corner, permanently`);
    }
  } catch (e) { bad('hidden panel (0x0)', 'threw: ' + e.message); }

  // The sequence that actually ships: hidden first, shown second.
  const seq = mk(0, 0);
  try {
    measure.call(seq);
    if (seq.fits === 0) ok('recovery sequence: no fit while hidden');
    else bad('recovery sequence', 'fitted against a 0x0 box');

    seq.wrap.getBoundingClientRect = () => ({ width: 800, height: 600 });
    seq.canvas.getBoundingClientRect = () => ({ width: 800, height: 600, left: 0, top: 0 });
    measure.call(seq);

    if (seq.canvas.width === 1600 && seq.canvas.height === 1200) {
      ok('real box (800x600 @ dpr 2): backing store sized to 1600x1200');
    } else {
      bad('real box sizing', `canvas is ${seq.canvas.width}x${seq.canvas.height}, want 1600x1200`);
    }
    if (seq.width === 800 && seq.height === 600) {
      ok('real box: CSS-pixel dimensions recorded for hit testing');
    } else {
      bad('real box dimensions', `width=${seq.width} height=${seq.height}`);
    }
    if (seq.fits === 1 && seq.hasFit === true) {
      ok('real box: the FIRST real measurement runs the deferred fit, exactly once');
    } else {
      bad('deferred fit', `fits=${seq.fits} hasFit=${seq.hasFit} — a graph measured while hidden never recovers`);
    }

    // A second real measurement must not refit: that would throw away a
    // reader's pan and zoom on every resize.
    measure.call(seq);
    if (seq.fits === 1) ok('later measurements do not refit, so pan and zoom survive a resize');
    else bad('refit on resize', `fits=${seq.fits} after a second real measurement`);
  } catch (e) { bad('recovery sequence', 'threw: ' + e.message); }
}

// ---------------------------------------------------------------------------
// Trap 2: hijacking page scroll.
// ---------------------------------------------------------------------------

const w = src.match(/addEventListener\('wheel',\s*\(e\)\s*=>\s*\{([\s\S]*?)\n    \}/);
if (!w) {
  bad('the wheel handler can be located in app.js',
      'the harness extracts it by pattern; if bindWheel() was reformatted, update this regex');
} else {
  const handler = new Function('e', 'SELF', w[1].replace(/\bthis\./g, 'SELF.'));
  const mkSelf = () => ({
    zooms: 0, lastFactor: 0,
    canvas: { getBoundingClientRect: () => ({ left: 0, top: 0 }) },
    zoomAt(sx, sy, factor) { this.zooms++; this.lastFactor = factor; },
  });

  const drive = (evt) => {
    const self = mkSelf();
    let prevented = 0;
    handler({ clientX: 10, clientY: 10, preventDefault() { prevented++; }, ...evt }, self);
    return { self, prevented };
  };

  let r = drive({ ctrlKey: false, metaKey: false, deltaY: -1 });
  if (r.prevented === 0 && r.self.zooms === 0) {
    ok('plain wheel: the page scrolls — no zoom, no preventDefault');
  } else {
    bad('plain wheel', `prevented=${r.prevented} zooms=${r.self.zooms} — the canvas traps page scroll`);
  }

  r = drive({ ctrlKey: true, metaKey: false, deltaY: -1 });
  if (r.prevented === 1 && r.self.zooms === 1 && r.self.lastFactor > 1) {
    ok('ctrl+wheel up: zooms IN and suppresses the page scroll');
  } else {
    bad('ctrl+wheel', `prevented=${r.prevented} zooms=${r.self.zooms} factor=${r.self.lastFactor}`);
  }

  r = drive({ ctrlKey: false, metaKey: true, deltaY: 1 });
  if (r.prevented === 1 && r.self.zooms === 1 && r.self.lastFactor < 1) {
    ok('cmd+wheel down: zooms OUT (macOS) and suppresses the page scroll');
  } else {
    bad('cmd+wheel', `prevented=${r.prevented} zooms=${r.self.zooms} factor=${r.self.lastFactor}`);
  }
}

console.log(`\n${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
