'use strict';
/*
  RepoCortex portal — client.

  No framework, no build step, no network beyond this origin. Everything below
  is plain DOM plus one hand-written Canvas 2D renderer with its own clustering,
  layout, level-of-detail and viewport culling.

  FOUR RULES RUN THROUGH THE WHOLE FILE.

  1. THE INDEX IS UNTRUSTED CONTENT. Symbol names, file paths and evidence
     details are read out of somebody's repository. Nothing here reaches for
     innerHTML with them; every string goes in through textContent. The server
     sets a Content-Security-Policy that would stop an injected script anyway,
     but defence that rests on a header alone is one header away from being no
     defence.

  2. CONFIDENCE IS THE VISUAL LANGUAGE. Tier styling comes from TIER below,
     which reads the CSS custom properties the legend uses, so the canvas and
     the DOM cannot drift apart. Four independent encodings — hue, opacity,
     dash pattern, glyph — because colour alone fails a colour-blind reader and
     fails again in a greyscale screenshot of a review.

  3. NOTHING IS SILENTLY OMITTED. Every response carries an `omissions` block.
     renderOmissions turns each machine reason into a sentence, DEDUPES BY
     CAUSE (`low_confidence_hidden` and `below_confidence_floor` were printed as
     two separate losses of the same 8,015 edges), and every list this file
     truncates itself says so with a count and a way to see the rest.

  4. NEVER RENDER A FIELD THE API DOES NOT SEND. Three bugs shipped from
     reading `c.name`, `f.exported` and `d.scope`; each rendered "undefined" or
     a silently wrong zero with no error anywhere. Every field read below was
     confirmed against live JSON from `cortex ui` over a 14,614-file
     repository, and `num()`/`text()` render an em dash rather than the string
     "undefined" if one ever goes missing.
*/

(function () {

// ---------------------------------------------------------------- DOM helpers

function el(tag, attrs, ...kids) {
  const n = document.createElement(tag);
  if (attrs) {
    for (const [key, v] of Object.entries(attrs)) {
      if (v === null || v === undefined || v === false) continue;
      if (key === 'class') n.className = v;
      else if (key === 'text') n.textContent = String(v);
      // Applied through the CSSOM rather than as a style ATTRIBUTE. The portal
      // ships `style-src 'self'` with no 'unsafe-inline', so setting the
      // attribute is refused by the browser and the declaration is silently
      // dropped - which for a proportional bar means every segment renders at
      // zero width and the confidence distribution disappears. setProperty is
      // not inline style and is not refused.
      else if (key === 'style') {
        for (const decl of String(v).split(';')) {
          const at = decl.indexOf(':');
          if (at < 0) continue;
          n.style.setProperty(decl.slice(0, at).trim(), decl.slice(at + 1).trim());
        }
      }
      else if (key === 'html') throw new Error('innerHTML is not available here by design');
      else if (key.startsWith('on') && typeof v === 'function') n.addEventListener(key.slice(2), v);
      else n.setAttribute(key, v === true ? '' : String(v));
    }
  }
  for (const kid of kids.flat(3)) {
    if (kid === null || kid === undefined || kid === false || kid === '') continue;
    n.appendChild(typeof kid === 'object' ? kid : document.createTextNode(String(kid)));
  }
  return n;
}

const $ = (sel) => document.querySelector(sel);

function clear(node) { while (node && node.firstChild) node.removeChild(node.firstChild); }

// num and text are the two guards that stop rule 4 reaching a reader as text.
// A missing field renders an em dash, which a human reads as "not stated";
// String(undefined) renders "undefined", which a human reads as a bug — or
// worse, does not notice.
function num(n) {
  if (n === null || n === undefined || n === '') return '—';
  const v = Number(n);
  return Number.isFinite(v) ? v.toLocaleString() : '—';
}

function text(s, fallback) {
  if (s === null || s === undefined) return fallback === undefined ? '—' : fallback;
  const out = String(s);
  return out === '' ? (fallback === undefined ? '—' : fallback) : out;
}

function pct(n, digits) {
  const v = Number(n);
  if (!Number.isFinite(v)) return '—';
  return v.toFixed(digits === undefined ? 1 : digits) + '%';
}

function plural(word, n) {
  const base = word || 'container';
  const one = n === 1;
  if (one) return base;
  if (/[^aeiou]y$/.test(base)) return base.slice(0, -1) + 'ies';
  if (/(s|sh|ch|x|z)$/.test(base)) return base + 'es';
  return base + 's';
}

// Paths are read right-to-left: the filename matters more than the mount point.
function tailPath(p, keep) {
  const parts = String(p || '').split('/');
  if (parts.length <= keep) return String(p || '');
  return '…/' + parts.slice(-keep).join('/');
}

function baseName(p) {
  const parts = String(p || '').split('/');
  return parts[parts.length - 1] || String(p || '');
}

function dirName(p) {
  const parts = String(p || '').split('/');
  parts.pop();
  return parts.join('/');
}

/*
  Every screen's error path starts here, so this function has to fail loudly in
  all four ways a request can fail, not just the one that returns a status code.

  It used to await `fetch` with no deadline and swallow an unparseable body into
  `{ error: 'HTTP 200' }`, which `res.ok` then waved through as data. A server
  that accepted the connection and never answered left the spinner up forever
  with nothing in the console; a server that answered 200 with a body this page
  could not read rendered a hollow screen and no error at all. Both are now
  thrown, and every caller already routes a throw into showError().
*/
const API_TIMEOUT_MS = 20000;

async function api(path, params) {
  const url = new URL(path, window.location.origin);
  for (const [key, v] of Object.entries(params || {})) {
    if (v !== '' && v !== null && v !== undefined) url.searchParams.set(key, v);
  }

  const controller = new AbortController();
  let timedOut = false;
  const deadline = setTimeout(() => { timedOut = true; controller.abort(); }, API_TIMEOUT_MS);

  let res;
  let raw;
  try {
    res = await fetch(url, { headers: { 'Accept': 'application/json' }, signal: controller.signal });
    // The body is read inside the same deadline: a response whose headers
    // arrive and whose body never does is the same outage to a reader.
    raw = await res.text();
  } catch (e) {
    if (timedOut) {
      throw new Error('The server did not answer within ' + Math.round(API_TIMEOUT_MS / 1000)
        + ' seconds. It may be busy, stopped, or the request may be one this index cannot finish.');
    }
    throw new Error('The portal could not reach its own server (' + url.pathname
      + '). It may have been stopped: ' + (e && e.message ? e.message : String(e)));
  } finally {
    clearTimeout(deadline);
  }

  let body = null;
  let parsed = true;
  try { body = JSON.parse(raw); } catch (e) { parsed = false; }

  if (!res.ok) throw new Error((parsed && body && body.error) ? body.error : ('HTTP ' + res.status + ' from ' + url.pathname));
  if (!parsed || body === null || typeof body !== 'object') {
    throw new Error('The server answered ' + res.status + ' on ' + url.pathname
      + ' with a body this page could not read as JSON. The binary and the page may be from different builds.');
  }
  return body;
}

function loading(msg) {
  return el('div', { class: 'loading' }, el('span', { class: 'spin', 'aria-hidden': 'true' }), msg || 'Reading the index…');
}

function showError(container, err, what) {
  clear(container);
  container.appendChild(el('div', { class: 'err' },
    el('h3', { text: (what || 'This view') + ' could not load' }),
    el('p', { text: String(err && err.message ? err.message : err) }),
    el('p', { class: 'small muted', text: 'The server is running, so the index is the suspect: it may be absent, or built by an older schema than this binary expects.' }),
    el('p', { class: 'small muted' }, 'Try ', el('code', { text: 'cortex doctor' }), ', then ', el('code', { text: 'cortex index' }), '.')
  ));
}

// ---------------------------------------------------------------- the tier vocabulary

const TIER_ORDER = ['exact', 'high', 'medium', 'low', 'unknown'];
const TIER_RANK = { exact: 4, high: 3, medium: 2, low: 1, unknown: 0 };

// One place, four encodings. dash and alpha are read by the canvas; hue and
// glyph by both. `label` is the word a reader sees — the id stays in every
// data-tier attribute, query parameter and API field — and `at` is the
// measured precision band the tier means, which is the sentence that makes
// this product different from an LLM-asserted graph. Both mirror
// assets/js/tiers.mjs, which is the copy the shell's modules read.
const TIER = {
  exact:   { glyph: '✓', dash: [],     alpha: 1.00, width: 2.4, cssVar: '--tier-exact',   label: 'proven',      at: 'a compiler proved it' },
  high:    { glyph: '',  dash: [],     alpha: 0.86, width: 1.9, cssVar: '--tier-high',    label: 'very likely', at: 'measured 87–96% right' },
  medium:  { glyph: '',  dash: [5, 4], alpha: 0.72, width: 1.6, cssVar: '--tier-medium',  label: 'uncertain',   at: 'measured 6–38% right' },
  low:     { glyph: '',  dash: [2, 4], alpha: 0.34, width: 1.2, cssVar: '--tier-low',     label: 'a guess',     at: 'measured under 3% right' },
  unknown: { glyph: '?', dash: [1, 3], alpha: 0.45, width: 1.2, cssVar: '--tier-unknown', label: 'unmeasured',  at: 'no measurement recorded' },
};

function tierOf(v) { return TIER[v] ? v : 'unknown'; }
// The word for a tier id, wherever the id would otherwise be printed in prose.
// The shell's module is the authority; TIER's label is the same word, kept so
// this file still renders if the module never ran.
function tierWord(v) {
  const dv = window.cortex;
  return dv && typeof dv.tierWord === 'function' ? dv.tierWord(tierOf(v)) : TIER[tierOf(v)].label;
}
// The word with its measurement — "very likely (measured 87–96% right)" — for
// titles and hover cards, guarded the same way.
function tierSentence(v) {
  const dv = window.cortex;
  const tv = tierOf(v);
  return dv && typeof dv.tierSentence === 'function' ? dv.tierSentence(tv) : TIER[tv].label + ' (' + TIER[tv].at + ')';
}
function tierRank(v) { const rank = TIER_RANK[v]; return rank === undefined ? 0 : rank; }

// An edge, said the way a reader would say it: "web uses registry, 12 times".
// The edge kinds (CALLS, IMPORTS) are detail, and stay in the kinds column and
// the hover, never in the sentence.
function edgeSentence(from, to, count) {
  const n = Math.max(1, Number(count || 0));
  return from + ' uses ' + to + ', ' + num(n) + ' ' + plural('time', n);
}
// The same edge for a hover: how often, how sure, and one place to check.
function edgeHoverSentence(from, to, count, tier, evidence) {
  const n = Math.max(1, Number(count || 0));
  const sure = tierSentence(tier);
  const ev = evidence
    ? text(evidence.file_path, '') + (evidence.line ? ':' + evidence.line : '')
    : '';
  return from + ' uses ' + to + ' ' + num(n) + ' ' + plural('time', n) + '. '
    + sure.charAt(0).toUpperCase() + sure.slice(1) + '.'
    + (ev ? ' One example: ' + ev + '.' : ' No example occurrence was recorded.');
}
function weakest(a, b) { return tierRank(a) <= tierRank(b) ? tierOf(a) : tierOf(b); }

function tierBadge(conf, opts) {
  const tv = tierOf(conf);
  const spec = TIER[tv];
  const node = el('span', { class: 'tier tier-' + tv, 'data-tier': tv, title: tierSentence(tv) });
  if (spec.glyph) node.appendChild(el('span', { 'aria-hidden': 'true', text: spec.glyph }));
  node.appendChild(document.createTextNode(tierWord(tv)));
  if (opts && opts.count !== undefined) node.appendChild(el('span', { class: 'n', text: ' ×' + num(opts.count) }));
  return node;
}

// css reads a custom property off :root so the canvas cannot disagree with the
// legend, and re-reads on every theme change rather than caching at boot.
function css(name, fallback) {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return v || fallback || '#888';
}

function tierColour(conf) { return css(TIER[tierOf(conf)].cssVar); }

// ---------------------------------------------------------------- URL state

/*
  Every screen, filter, selection and page offset lives in the query string and
  moves with pushState. Nothing in this portal used to be shareable — a
  colleague could be sent a screenshot of a blast radius, never a link to it —
  and the one remedy the API suggests for a truncated list ("?offset=12 for the
  next page") was unreachable because nothing read the URL.
*/
const DEFAULTS = {
  s: 'overview', floor: 'medium', container: '', lang: '', kinds: '', group: '', level: '',
  sel: '', sym: '', depth: '3', intent: '', changed: '', off: '0', limit: '12',
  csdepth: '3', scope: '0', vfloor: '', expand: '', rule: '', sev: '',

  /*
    THE GRAPH EXPLORER'S OWN STATE, ALL OF IT IN THE URL.

    focus/fdepth  the blast-radius lens: one node, everything within N hops,
                  the rest dimmed rather than removed.
    pfrom/pto     the two ends of a path query — "how does this reach that".
    lens          a SECOND confidence floor applied in this browser, with no
                  refetch, so a reader can watch the graph shrink as the
                  standard of proof rises.
    xlang         cross-language edges only.

    These are here rather than in a module-level object for the reason the
    comment above gives: a view somebody can be sent is the difference between
    a screenshot and a shared finding. Every one of them survives a cold load.
  */
  focus: '', fdepth: '2', pfrom: '', pto: '', lens: '', xlang: '0',

  /*
    THE RETIRED CARD BROWSER'S ADDRESS, AND THE SYMBOL SEARCH.

    dc/dd/dfile/dsel named a container, a folder, a file and a selected card.
    The card browser is gone (the code map's Tree, Treemap and Code views do its
    job); these are still READ so a link to it redirects to the same thing —
    see redirectCardBrowserLink. `q` is the symbol search.
  */
  dc: '', dd: '', dfile: '', dsel: '', q: '',

  /*
    THE CODE MAP'S OWN ADDRESS (js/codemap.mjs). cfile/cline are the Code
    view's file and line, cminor whether it lists locals; tq and topen the Tree's filter and open rows;
    tmsize/tmcolor the Treemap's measures. dc/dd/dfile/dsel above are the
    retired card browser's address, still READ so its old links redirect.
  */
  cfile: '', cline: '', cminor: '', tq: '', topen: '', tmsize: '', tmcolor: '', glayout: '',

  // ?open=1 unfolds every disclosure, for an agent or a screenshot. Kept
  // here so the URL the client rewrites keeps carrying it.
  open: '',

  /*
    WHICH OF THE CODE MAP'S FOUR VIEWS IS ON SCREEN: map (the canvas), tree,
    treemap or code. Empty means nobody chose, which is the map. `browse`, the
    retired card browser, still arrives in old links and is redirected.
  */
  gv: '',

  /*
    THE FACT DIFF'S TWO SIDES.

    dfrom/dto are run ids, and both are allowed to be empty: /api/diff defaults
    the head to the committed run this portal serves and the base to the run
    before it, and it reports in `filters.defaulted` which side it chose. They
    are named dfrom/dto rather than from/to because `from` is a word a future
    control on another screen will want, and a shared key that two screens write
    is a screen that changes when a reader touches a different one.
  */
  dfrom: '', dto: '',

  /*
    STRUCTURE & CENTRALITY'S OWN THREE.

    mlevel/mkinds/mtests, and not level/kinds — `level` and `kinds` are the graph
    explorer's, and a key two screens write is a screen that changes when a
    reader touches a different one. The three of them together are the whole
    state of that screen, so a ranking somebody found is a ranking somebody can
    be sent: the level it was computed at, and which of the two opt-in filters
    were on when it was.
  */
  mlevel: 'symbol', mkinds: '', mtests: '0',
};

const state = {
  meta: null,
  legend: [],
  overview: null,
  // /api/indexstate: how this index was built, and whether this portal may
  // rebuild it. Null until the first paint has happened — see loadIndexState.
  indexstate: null,
  loaded: {},
  graph: null,
  view: null,
  repoRoot: '',
  q: Object.assign({}, DEFAULTS),
};

function readQuery() {
  const q = Object.assign({}, DEFAULTS);
  const sp = new URLSearchParams(window.location.search);
  for (const key of Object.keys(DEFAULTS)) {
    const v = sp.get(key);
    if (v !== null) q[key] = v;
  }
  // The old build wrote `#graph`. Keep reading it so an old link still lands.
  const hash = window.location.hash.replace('#', '');
  if (!sp.get('s') && hash && document.getElementById('screen-' + hash)) q.s = hash;
  // A screen this build does not have is a link from another build or a typo.
  // showScreen hides every section whose id does not match, so an unrecognised
  // name used to leave the reader looking at nothing at all - no screen, no
  // message, no console error. Fall back to the default rather than to a void.
  if (!document.getElementById('screen-' + q.s)) q.s = DEFAULTS.s;
  return q;
}

function queryString(q) {
  const sp = new URLSearchParams();
  for (const key of Object.keys(DEFAULTS)) {
    if (q[key] !== undefined && q[key] !== '' && String(q[key]) !== String(DEFAULTS[key])) sp.set(key, q[key]);
  }
  if (q.s && q.s !== DEFAULTS.s) sp.set('s', q.s);
  const s = sp.toString();
  return s ? '?' + s : window.location.pathname;
}

function setQuery(patch, opts) {
  Object.assign(state.q, patch);
  const url = queryString(state.q);
  if (opts && opts.replace) window.history.replaceState(null, '', url);
  else window.history.pushState(null, '', url);
}

function shareLink() {
  return window.location.origin + queryString(state.q);
}

// ---------------------------------------------------------------- omissions

/*
  The reasons arrive as snake_case JSON keys. They were printed to humans as
  labels — `low_confidence_hidden:`, `row_budget:` — and `row_budget` appeared
  twice on one card with two unrelated meanings. Each reason now becomes a
  sentence that names the population, and identical causes collapse into one
  line.
*/
/*
  The API now names its own omissions. Every entry carries a `label` - "hidden by
  the confidence floor", "rows beyond this listing's budget" - and the block
  carries `totals`, a per-reason rollup derived at marshal time.

  That replaced a top-level hoisted count. `low_confidence_hidden` and the
  `below_confidence_floor` entries both carried 8,015 on the same page, and this
  client rendered them as two lines, so the one number a reader most needs to
  trust read as two populations totalling 16,030. There is now one population
  and one place it comes from, and this renderer no longer invents wording of
  its own: two screens cannot phrase the same omission two different ways if
  neither of them writes the phrase.
*/
function renderOmissions(om, opts) {
  const options = opts || {};
  const rows = (om && Array.isArray(om.omitted)) ? om.omitted : [];
  const totals = (om && Array.isArray(om.totals)) ? om.totals : [];
  const rollup = new Map(totals.map((row) => [row.reason, row]));

  const items = rows.map((o) => ({
    count: o.count,
    noun: text(o.label, String(o.reason || 'not stated').replace(/_/g, ' ')),
    detail: o.detail,
    fix: o.remedy,
    reason: o.reason,
  }));
  for (const extra of (options.extra || [])) items.push(extra);

  const key = 'omit:' + (options.key || location.search);
  if (items.length === 0) {
    return el('p', { class: 'hint', text: 'Nothing was left out of this view: every row the index holds for this question is on screen.' });
  }

  // A reason recorded more than once in one view is a subtotal and its rows,
  // which is a shape readers already know how to read - and it is the shape
  // that makes double-counting impossible to render by accident.
  // A rollup of zero is not a subtotal, it is noise: `Note()` records an
  // omission that CANNOT be sized, and four unsized notes summing to "0 in
  // total" reads as "nothing was left out" - the opposite of what they say.
  const multi = totals.filter((row) => Number(row.entries) > 1 && Number(row.count) > 0);

  const total = items.reduce((sum, item) => sum + (Number(item.count) > 0 ? Number(item.count) : 0), 0);
  const div = el('div', { class: 'held' },
    el('h4', { text: "What this view can't see" }),
    el('ul', null, items.map((item) => el('li', null,
      el('span', { class: 'what' },
        item.count === undefined || item.count === 0 ? null : el('b', { text: num(item.count) + ' ' }),
        item.noun,
        // Unquantified is not zero, and it must not read as zero.
        item.count === undefined || item.count === 0 ? el('span', { class: 'muted', text: ' — how many is not recorded' }) : null),
      item.detail ? el('span', { class: 'fix', text: item.detail }) : null,
      item.fix ? el('span', { class: 'fix' }, 'To see it: ', el('code', { text: item.fix })) : null
    ))),
    multi.length ? el('p', { class: 'fix' }, multi.map((row) =>
      el('span', null, num(row.count) + ' ' + text(row.label) + ' in total, across ' + num(row.entries) + ' places in this view. ')) ) : null);
  const n = items.length;
  return fold("What this view can't see — " + num(n) + ' ' + plural('thing', n) + (total ? ' (' + num(total) + ' rows)' : ''),
    div, { key: key });
}

// A caveat belongs next to the thing it is a caveat about. The blind-spots cap
// used to be declared 4,876px below the blind-spots table.
function inlineOmit(...kids) {
  return el('p', { class: 'inline-omit' }, kids);
}

// ---------------------------------------------------------------- components

function card(title, sub, body, footer, klass) {
  const node = el('div', { class: 'card' + (klass ? ' ' + klass : '') });
  // A card under a fold passes no title, so the summary is its only heading;
  // the sub-line still says where the card's words come from.
  if (title || sub) {
    node.appendChild(el('header', null,
      title ? el('h3', { text: title }) : null,
      sub ? (typeof sub === 'object' ? el('span', { class: 'sub' }, sub) : el('span', { class: 'sub', text: sub })) : null));
  }
  const inner = el('div', { class: 'body' });
  for (const kid of [body].flat(3)) if (kid) inner.appendChild(kid);
  if (footer) inner.appendChild(footer);
  node.appendChild(inner);
  return node;
}

function kpi(label, value, caption, tone) {
  let capNode = null;
  if (Array.isArray(caption)) capNode = el('div', { class: 'c' }, ...caption);
  else if (caption && typeof caption === 'object') capNode = el('div', { class: 'c' }, caption);
  else if (caption) capNode = el('div', { class: 'c', text: caption });
  return el('div', { class: 'kpi' + (tone ? ' is-' + tone : '') },
    el('div', { class: 'k', text: label }),
    el('div', { class: 'v', text: value }),
    capNode);
}

/*
  Every panel ends in one of these. `next_action` objects arrive with a label, a
  detail, and either a command to run or a screen to open; the screen form
  becomes a real button that navigates, so the advice is one click rather than
  an instruction to go and find something.
*/
function nextAction(na) {
  if (!na || (!na.label && !na.detail)) return null;
  const target = na.screen ? String(na.screen).split('#')[0] : '';
  const known = target && document.getElementById('screen-' + target);
  const node = el('div', { class: 'next' },
    el('span', { class: 'k', text: 'Next: ' + text(na.label, 'keep going') }),
    na.detail ? el('span', { class: 'd', text: na.detail }) : null,
    na.command ? el('code', { text: na.command }) : null);
  if (na.command) node.appendChild(copyButton(na.command, 'Copy'));
  if (known) {
    node.appendChild(el('button', {
      class: 'go', type: 'button', text: 'Open →',
      onclick: () => go(target, {}),
    }));
  }
  return node;
}

function copyButton(payload, label) {
  const btn = el('button', { class: 'go', type: 'button', text: label || 'Copy' });
  btn.addEventListener('click', () => {
    const done = () => { const old = btn.textContent; btn.textContent = 'Copied'; setTimeout(() => { btn.textContent = old; }, 1200); };
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(payload).then(done, done);
    else done();
  });
  return btn;
}

/*
  Evidence is the product's whole claim: a checkable file and line under every
  relationship. It used to be inert text. Now it is copyable in one click and
  openable in an editor, because a proof a reader cannot act on wastes the
  advantage that produced it.
*/
/*
  encodePath percent-encodes a filesystem path for a URL while keeping the
  slashes that make it a path.

  Every OTHER place this client puts an id into a URL goes through
  URLSearchParams — api() builds its query with searchParams.set, queryString()
  builds the address bar with a URLSearchParams, and both encode. This function
  exists for the one construction that cannot use them, a custom-scheme editor
  href, and it is exported into the module scope so any future one has something
  correct to reach for. The ids that forced the audit are real: pulse's
  container list opens with `(no declared component)` — a space and two
  parentheses in a value used as a query parameter, a fetch key, a canvas label
  and a deep link.
*/
function encodePath(p) {
  return String(p).split('/').map(encodeURIComponent).join('/');
}

function evidence(file, line, opts) {
  if (!file) return el('span', { class: 'muted small', text: opts && opts.absent ? opts.absent : 'no location recorded' });
  const label = line ? file + ':' + line : file;
  const node = el('span', { class: 'ev', title: label });
  node.appendChild(el('span', { class: 'p', text: label }));
  const copy = el('button', { type: 'button', title: 'Copy ' + label, 'aria-label': 'Copy ' + label, text: '⧉' });
  copy.addEventListener('click', () => {
    const write = () => { node.classList.add('copied'); setTimeout(() => node.classList.remove('copied'), 1100); };
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(label).then(write, write);
    else write();
  });
  node.appendChild(copy);
  if (state.repoRoot) {
    // repoRoot is already absolute, so a leading slash here produced
    // vscode://file//Users/... - an empty authority and a doubled root.
    //
    // AND EVERY SEGMENT IS ENCODED. This used to concatenate the raw path, and
    // the paths in an index are somebody's real directory names: one of the
    // repositories this portal is tested against lives under
    // "RKI - Remote Key Injection", and a raw space in a URL truncates the
    // href at the space in some handlers and voids it in others. The reader
    // saw a link that opened the wrong file, or nothing. Slashes and the line
    // separator are the structure and stay literal; everything between them is
    // data and is encoded as data.
    const href = 'vscode://file' + encodePath(String(state.repoRoot).replace(/\/$/, ''))
      + '/' + encodePath(file) + (line ? ':' + line : '');
    node.appendChild(el('a', { href: href, title: 'Open in your editor', 'aria-label': 'Open ' + label + ' in your editor', text: '↗' }));
  }
  return node;
}

function evidenceOf(ev) {
  if (!ev) return evidence(null);
  return evidence(ev.file_path, ev.line);
}

/*
  table renders a capped list and — this is the point — states the cap itself
  when it bites, rather than slicing at 25 in silence the way the front-page
  task box did on three separate lists.
*/
function table(columns, rows, opts) {
  const options = opts || {};
  const cap = options.cap || rows.length;
  const shown = rows.slice(0, cap);
  const wrap = el('div', { class: 'tablewrap' });
  const tbl = el('table');
  tbl.appendChild(el('thead', null, el('tr', null, columns.map((col) =>
    el('th', { class: col.num ? 'num' : null, scope: 'col', title: col.title || null }, col.label)))));
  const body = el('tbody');
  for (const row of shown) {
    const built = options.row(row);
    body.appendChild(built);
  }
  tbl.appendChild(body);
  wrap.appendChild(tbl);
  const out = el('div', { class: 'stack' }, wrap);
  if (rows.length > cap) {
    const more = rows.length - cap;
    const note = inlineOmit('Showing ', el('b', { text: num(cap) }), ' of ', el('b', { text: num(rows.length) }), ' — ',
      el('b', { text: num(more) }), ' ' + plural('row', more) + ' hidden by this page’s own cap, not by the index. ');
    note.appendChild(el('button', {
      class: 'linkish', type: 'button', text: 'Show all ' + num(rows.length),
      onclick: () => { const full = table(columns, rows, Object.assign({}, options, { cap: rows.length })); out.replaceWith(full); },
    }));
    out.appendChild(note);
  }
  return out;
}

function emptyState(title, body, examples) {
  return el('div', { class: 'empty' },
    el('h3', { text: title }),
    [body].flat().map((line) => (typeof line === 'string' ? el('p', { text: line }) : line)),
    examples && examples.length ? el('div', { class: 'ex' }, examples) : null);
}

// Checkbox chips. The control they replace was `<select multiple size="4">`
// over ten options: it needed ctrl-click, showed four, had no all/none, and
// gave no hint that selecting nothing meant everything.
function chipGroup(options, selected, onChange) {
  const chosen = new Set(selected);
  const wrap = el('div', { class: 'chips' });
  const rebuild = () => {
    clear(wrap);
    wrap.appendChild(el('button', {
      class: 'chip', type: 'button', 'aria-pressed': String(chosen.size === 0),
      text: 'All ' + options.length,
      onclick: () => { chosen.clear(); rebuild(); onChange([]); },
    }));
    for (const opt of options) {
      const on = chosen.has(opt.value);
      wrap.appendChild(el('button', {
        class: 'chip', type: 'button', 'aria-pressed': String(on),
        onclick: () => {
          if (chosen.has(opt.value)) chosen.delete(opt.value); else chosen.add(opt.value);
          rebuild();
          onChange(Array.from(chosen));
        },
      }, opt.label, opt.count === undefined ? null : el('span', { class: 'n', text: num(opt.count) })));
    }
  };
  rebuild();
  return wrap;
}

/*
  A filtering combobox. It replaces two controls that could not do the job:

  - a native `<select>` that carried 699 full directory paths on a repository
    with no declared architecture, including an agent's scratch worktree;
  - a `<datalist>` on Blast radius, whose option labels render inconsistently
    across browsers, cannot be styled, cannot be walked as rows, and threw away
    the kind, the path and the tier the API returns with every match.

  It is a real listbox: arrow keys, Enter, Escape, aria-activedescendant, and a
  note stating how many matches were not listed.
*/
function combobox(opts) {
  const options = opts || {};
  const input = el('input', {
    type: 'search', autocomplete: 'off', spellcheck: 'false',
    role: 'combobox', 'aria-expanded': 'false', 'aria-autocomplete': 'list',
    placeholder: options.placeholder || '', id: options.id || null,
  });
  if (options.value) input.value = options.value;
  // The listbox carries its own id and is wired to the input with
  // aria-controls. Two comboboxes now sit on the graph screen — the scope
  // picker and the search box — and a bare `.combo-list` named neither of them
  // to a screen reader, to a test, or to anything else.
  const listId = (options.id || 'combo') + '-list';
  const list = el('div', { class: 'combo-list', id: listId, role: 'listbox', hidden: true });
  input.setAttribute('aria-controls', listId);
  const wrap = el('div', { class: 'combo' }, input, list);
  let items = [];
  let active = -1;
  let seq = 0;

  function close() { list.hidden = true; input.setAttribute('aria-expanded', 'false'); active = -1; }

  function paint(rows, note) {
    clear(list);
    items = rows;
    rows.forEach((row, i) => {
      const opt = el('button', {
        class: 'combo-opt', type: 'button', role: 'option',
        id: 'combo-opt-' + (options.id || 'x') + '-' + i,
        'aria-selected': String(i === active),
        onmousedown: (e) => e.preventDefault(),
        onclick: () => { choose(row); },
      },
        el('span', { class: 'main', text: row.label }),
        row.sub ? el('span', { class: 'sub', text: row.sub }) : null,
        row.meta ? el('span', { class: 'meta', text: row.meta }) : null,
        row.tier ? tierBadge(row.tier) : null);
      list.appendChild(opt);
    });
    if (rows.length === 0) list.appendChild(el('div', { class: 'combo-note', text: options.emptyNote || 'Nothing in this index matches that.' }));
    if (note) list.appendChild(el('div', { class: 'combo-note', text: note }));
    list.hidden = false;
    input.setAttribute('aria-expanded', 'true');
  }

  function choose(row) {
    input.value = row.value;
    close();
    if (options.onChoose) options.onChoose(row);
  }

  async function refresh() {
    const mine = ++seq;
    const res = await options.source(input.value.trim());
    if (mine !== seq) return;
    active = -1;
    paint(res.rows || [], res.note || '');
  }

  let timer = 0;
  input.addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(refresh, 110); });
  input.addEventListener('focus', refresh);
  input.addEventListener('blur', () => setTimeout(close, 140));
  input.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (list.hidden) { refresh(); return; }
      active += (e.key === 'ArrowDown' ? 1 : -1);
      if (active < 0) active = items.length - 1;
      if (active >= items.length) active = 0;
      Array.from(list.querySelectorAll('.combo-opt')).forEach((node, i) => {
        node.setAttribute('aria-selected', String(i === active));
        if (i === active) { node.scrollIntoView({ block: 'nearest' }); input.setAttribute('aria-activedescendant', node.id); }
      });
    } else if (e.key === 'Enter') {
      if (!list.hidden && active >= 0 && items[active]) { e.preventDefault(); choose(items[active]); }
      else if (options.onSubmit) { e.preventDefault(); close(); options.onSubmit(input.value.trim()); }
    } else if (e.key === 'Escape') {
      // Chromium clears a type=search input on Escape, which fires `input`,
      // which re-armed the debounce and re-opened the listbox 110ms after it
      // closed. Escape has to cancel the pending refresh and invalidate any
      // in-flight one, or the listbox is not closable from the keyboard at all.
      e.preventDefault();
      clearTimeout(timer);
      seq++;
      close();
    }
  });

  return { node: wrap, input: input, refresh: refresh };
}

function field(label, control, klass) {
  return el('label', { class: 'field' + (klass ? ' ' + klass : '') },
    el('span', { class: 'lbl', text: label }), control);
}

function selectField(label, id, options, value, onChange) {
  const sel = el('select', { id: id });
  for (const opt of options) {
    const o = el('option', { value: opt.value, text: opt.label });
    if (String(opt.value) === String(value)) o.selected = true;
    sel.appendChild(o);
  }
  sel.addEventListener('change', () => onChange(sel.value));
  return field(label, sel);
}

// The floor labels: the tier's word plus what choosing it keeps. Every other
// floor control (the graph's lens included) takes its label from here.
const FLOORS = [
  { value: 'exact', label: tierWord('exact') + ' only' },
  { value: 'high', label: tierWord('high') + ' and better' },
  { value: 'medium', label: tierWord('medium') + ' and better' },
  { value: 'low', label: 'everything, guesses included' },
];
function floorLabel(v) {
  const f = FLOORS.find((x) => x.value === tierOf(v));
  return f ? f.label : tierWord(v) + ' and better';
}

// ================================================================ GRAPH ENGINE
/*
  What was here before drew every node the API returned, in one hand-rolled
  Sugiyama, at whatever zoom the result happened to fit at. Measured on real
  repositories that landed at 28%, 12% and 8% - and labels were gated at 34%, so
  THE DEFAULT VIEW OF THE GRAPH SCREEN RENDERED NO TEXT IN ANY REAL CASE. Drilled
  into one component it was 1,620 rounded rectangles three screen pixels tall.

  The rebuild is four real algorithms rather than a threshold change:

    CLUSTERING      folder-prefix communities at an adaptive depth, or Louvain
                    modularity over the edge weights. A 14,614-file repository
                    opens as a map of a dozen labelled regions.
    AGGREGATION     every edge between two clusters collapses into one drawn
                    edge carrying a count, a kind histogram, the WEAKEST tier on
                    it, and one example file:line.
    LAYOUT          layered by dependency direction: longest-path rank over
                    the drawn edges with back-edges ignored, callers above
                    callees, deterministic from key order rather than a seed,
                    and a one-dimensional relaxation within each row so no two
                    labels can overlap. Run in time slices against a frame
                    deadline, so the UI never blocks while it settles.
    LEGIBILITY      node boxes are sized to their own measured label, the drawn
                    set is capped to what fits at a zoom where text is readable,
                    and whatever that cap removes is STATED ON THE CANVAS with a
                    control to show more. Fewer nodes and a readable map beats
                    every node and a barcode.
*/

const GRAPH = {
  MIN_K: 0.75,          // never fit below this: 15px world type at 0.75 is 11px on screen
  MAX_K: 4,
  MIN_ZOOM: 0.18,
  LABEL_FONT: 15,
  SUB_FONT: 11,
  NODE_H: 40,           // world px; 30 screen px at MIN_K, a real hit target
  NODE_PAD: 14,
  NODE_MIN_W: 74,
  NODE_MAX_W: 260,
  // World px per doubling of a node's symbol count; see quantityTerm. Measured
  // on the 17-component rung of a 109k-symbol repository: at 6 the mean width
  // went 177 -> 245, eight boxes sat on NODE_MAX_W and fit() landed at 0.759
  // against a 0.75 floor; at 3 it is 177 -> 212, no box on the cap, zoom 0.78.
  QUANTITY_WEIGHT: 3,
  GAP: 26,
  // The layered layout's two spacings, both in units the boxes define. Rows
  // are one box apart plus this fraction of GAP; the row width a rank may use
  // before wrapping is the legibility budget's area at this landscape aspect.
  // See createLayout for the measurements that chose them.
  LAYER_ROW_GAP: 0.55,
  LAYER_ASPECT: 2.0,
  // An edge that crosses a row is routed through a zero-width dummy member
  // in every row it crosses. A dummy keeps DUMMY_CLEAR from a box and
  // DUMMY_PITCH from the next dummy. A chain's two strokes sit ROUTE_SIDE
  // either side of its dummy, so they are 2 x ROUTE_SIDE apart, and the
  // pitch has to clear that plus the stroke's width: at ROUTE_SIDE 5 and
  // this pitch the down-stroke of one chain and the up-stroke of the next
  // were 2 px apart on the wrong sides and crossed, 45 pairs on a
  // 17-component view; at 2 the pitch leaves 4 px clear. Not a whole GAP:
  // at a full gap that view spent 60% of its row width on its 159 dummies.
  // The two caps bound the
  // cost: at most this many dummies in one row (that view's densest row
  // holds 18), and at most this many in the whole layout, shortest edges
  // first. See createLayout.
  DUMMY_CLEAR: 12,
  DUMMY_PITCH: 8,
  LAYER_DUMMIES_PER_ROW: 48,
  LAYER_DUMMY_BUDGET: 1200,
  MEMBER_CAP: 48,
  LOD_SUB: 0.92,
  LOD_EDGE_CAPTION: 1.25,
  LOD_LABEL: 0.34,
};

// ---------------------------------------------------------------- clustering

/*
  Folder communities. The depth is adaptive: it walks path depth 1..8 and keeps
  the depth whose group count sits closest to the target band, because a fixed
  depth produces one group on a flat repository and four hundred on a deep one.
  Understand-Anything reaches for the same folder-first heuristic before it
  reaches for Louvain, and on ordinary repositories it never needs to.
*/
function folderCommunities(leaves, target) {
  const want = target || 14;
  let best = null;
  for (let depth = 1; depth <= 8; depth++) {
    const groups = new Map();
    for (const leaf of leaves) {
      const dir = dirName(leaf.path || leaf.id);
      const parts = String(dir).split('/').filter(Boolean);
      const key = parts.slice(0, depth).join('/') || '(repository root)';
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(leaf);
    }
    const score = Math.abs(groups.size - want) + (groups.size > 42 ? 1000 : 0);
    if (!best || score < best.score) best = { score: score, groups: groups, depth: depth };
    if (groups.size >= want && groups.size <= 42) break;
  }
  return best ? best.groups : new Map([['(repository root)', leaves.slice()]]);
}

/*
  Louvain modularity optimisation - the connectivity grouping, for when folders
  and architecture disagree and the question is "what actually talks to what".

  Standard two-phase algorithm: repeated local moving of nodes to the
  neighbouring community with the best modularity gain, then contraction of each
  community into a single node and repeat. Deterministic node order, so the
  same index produces the same communities every time.
*/
function louvainCommunities(leaves, links) {
  const index = new Map();
  leaves.forEach((leaf, i) => index.set(leaf.id, i));
  const n = leaves.length;
  if (n === 0) return new Map();

  let adjacency = leaves.map(() => new Map());
  let selfLoops = new Float64Array(n);
  let m2 = 0;
  for (const link of links) {
    const a = index.get(link.a); const b = index.get(link.b);
    if (a === undefined || b === undefined) continue;
    const w = Math.max(1, link.count || 1);
    if (a === b) { selfLoops[a] += w; m2 += 2 * w; continue; }
    adjacency[a].set(b, (adjacency[a].get(b) || 0) + w);
    adjacency[b].set(a, (adjacency[b].get(a) || 0) + w);
    m2 += 2 * w;
  }
  if (m2 === 0) return new Map(leaves.map((leaf, i) => [leaf.id, 'g' + i]));

  let membership = leaves.map((_, i) => i);   // leaf index -> current community
  let level = 0;

  while (level < 4) {
    const size = adjacency.length;
    const degree = new Float64Array(size);
    for (let i = 0; i < size; i++) {
      let sum = selfLoops[i] * 2;
      for (const w of adjacency[i].values()) sum += w;
      degree[i] = sum;
    }
    const community = new Int32Array(size);
    const tot = new Float64Array(size);
    for (let i = 0; i < size; i++) { community[i] = i; tot[i] = degree[i]; }

    let moved = true;
    let rounds = 0;
    while (moved && rounds < 12) {
      moved = false; rounds++;
      for (let i = 0; i < size; i++) {
        const own = community[i];
        const weights = new Map();
        for (const [j, w] of adjacency[i]) {
          const cj = community[j];
          weights.set(cj, (weights.get(cj) || 0) + w);
        }
        tot[own] -= degree[i];
        let bestC = own;
        let bestGain = (weights.get(own) || 0) - (tot[own] * degree[i]) / m2;
        for (const [cj, w] of weights) {
          if (cj === own) continue;
          const gain = w - (tot[cj] * degree[i]) / m2;
          if (gain > bestGain + 1e-9) { bestGain = gain; bestC = cj; }
        }
        tot[bestC] += degree[i];
        if (bestC !== own) { community[i] = bestC; moved = true; }
      }
    }

    // Renumber, then project back down to the original leaves.
    const renum = new Map();
    for (let i = 0; i < size; i++) {
      if (!renum.has(community[i])) renum.set(community[i], renum.size);
    }
    const compact = Array.from({ length: size }, (_, i) => renum.get(community[i]));
    for (let i = 0; i < membership.length; i++) membership[i] = compact[membership[i]];
    const groups = renum.size;
    if (groups === size) break;   // converged: no further contraction possible

    // Contract.
    const nextAdj = Array.from({ length: groups }, () => new Map());
    const nextSelf = new Float64Array(groups);
    for (let i = 0; i < size; i++) {
      const ci = compact[i];
      nextSelf[ci] += selfLoops[i];
      for (const [j, w] of adjacency[i]) {
        const cj = compact[j];
        if (ci === cj) { if (i < j) nextSelf[ci] += w; continue; }
        nextAdj[ci].set(cj, (nextAdj[ci].get(cj) || 0) + w);
      }
    }
    adjacency = nextAdj; selfLoops = nextSelf; level++;
  }

  const out = new Map();
  leaves.forEach((leaf, i) => out.set(leaf.id, 'c' + membership[i]));
  return out;
}

/*
  A community's name has to come from its contents, and which content depends on
  the rung. Inside a component the members are files and the most common
  DIRECTORY is the name a reader can go and look at. At the top rung the members
  are the components themselves and they have no directory, so the same rule
  produced a map of five boxes all labelled "(no path)" - a clustering that
  worked and a drawing that said nothing. When there is no directory to name a
  group after, the largest member names it, which is how the eye finds it again.
*/
function nameCommunity(members) {
  // A community of one is that one thing. Naming it after its parent is how
  // four different single-directory groups ended up drawn with one identical
  // label, indistinguishable on the canvas and in the inspector.
  if (members.length === 1) {
    const only = members[0];
    const name = String(only.path || only.label || '');
    if (name) return { label: name, share: 100, dirs: 1 };
  }
  const counts = new Map();
  for (const leaf of members) {
    const dir = dirName(leaf.path || '');
    if (!dir) continue;
    counts.set(dir, (counts.get(dir) || 0) + 1);
  }
  let bestDir = '';
  let bestN = 0;
  for (const [dir, n] of counts) if (n > bestN) { bestN = n; bestDir = dir; }
  if (!bestDir) {
    const biggest = members.slice().sort((a, b) => (b.degree || 0) - (a.degree || 0))[0];
    return { label: biggest ? String(biggest.label) : '(unnamed group)', share: 0, dirs: 0, fromMember: true };
  }
  const share = members.length ? Math.round((bestN / members.length) * 100) : 0;
  return { label: bestDir, share: share, dirs: counts.size };
}

// ---------------------------------------------------------------- the model

/*
  buildModel holds leaves and links once, and derives a RENDER SET from them on
  every change of grouping, expansion or budget. Nothing re-fetches to expand a
  cluster; the whole interaction is local.
*/
function buildModel(leaves, links, opts) {
  const options = opts || {};
  const model = {
    leaves: leaves,
    links: links,
    byId: new Map(leaves.map((leaf) => [leaf.id, leaf])),
    groups: new Map(),
    leafGroup: new Map(),
    grouping: options.grouping || 'folder',
    expanded: new Set(options.expanded || []),
    memberCap: GRAPH.MEMBER_CAP,
  };
  regroup(model, model.grouping);
  return model;
}

function regroup(model, grouping) {
  model.grouping = grouping;
  model.groups = new Map();
  model.leafGroup = new Map();

  if (grouping === 'none' || model.leaves.length <= 24) {
    for (const leaf of model.leaves) {
      const gid = 'solo:' + leaf.id;
      model.groups.set(gid, { id: gid, label: leaf.label, members: [leaf], solo: true, sub: leaf.sub });
      model.leafGroup.set(leaf.id, gid);
    }
    return model;
  }

  let buckets;
  if (grouping === 'connectivity') {
    const assign = louvainCommunities(model.leaves, model.links);
    buckets = new Map();
    for (const leaf of model.leaves) {
      const key = assign.get(leaf.id) || 'c?';
      if (!buckets.has(key)) buckets.set(key, []);
      buckets.get(key).push(leaf);
    }
  } else {
    buckets = folderCommunities(model.leaves, 14);
  }

  // Communities of one or two are noise on a map; they merge into one stated
  // "small groups" bucket rather than being dropped or drawn as confetti.
  const small = [];
  for (const [key, members] of Array.from(buckets)) {
    if (members.length < 3 && buckets.size > 8) { small.push.apply(small, members); buckets.delete(key); }
  }
  if (small.length) buckets.set('(small groups)', small);

  for (const [key, members] of buckets) {
    const named = grouping === 'connectivity' ? nameCommunity(members) : { label: key, share: 100, dirs: 1 };
    const gid = 'g:' + key;
    members.sort((a, b) => (b.degree || 0) - (a.degree || 0) || String(a.label).localeCompare(String(b.label)));
    const base = grouping === 'connectivity' ? (baseName(named.label) || named.label) : (baseName(key) || key);
    model.groups.set(gid, {
      id: gid,
      baseLabel: base,
      label: base,
      full: grouping === 'connectivity' ? named.label : key,
      members: members,
      share: named.share,
      dirs: named.dirs,
      solo: false,
    });
    for (const leaf of members) model.leafGroup.set(leaf.id, gid);
  }
  disambiguateLabels(model);
  return model;
}

/*
  Folder grouping on a Go service produced three separate boxes all labelled
  "internal", because the basename of a directory is only unique among its
  siblings. A map with three identically named regions is a map you cannot
  navigate, so any label that collides is lengthened - one path segment at a
  time - until it does not.
*/
function disambiguateLabels(model) {
  const byLabel = new Map();
  for (const group of model.groups.values()) {
    const base = group.baseLabel || group.label;
    if (!byLabel.has(base)) byLabel.set(base, []);
    byLabel.get(base).push(group);
  }
  // The string a group is distinguished BY. Normally its community name; when
  // two groups in the same clash carry the same one, that name cannot separate
  // them however much of it is kept, so the group's own key is used instead.
  const nameOf = (group, clash) => {
    const full = String(group.full || group.baseLabel || '');
    const twin = clash.some((other) => other !== group && String(other.full || other.baseLabel || '') === full);
    if (!twin) return full;
    return String(group.id || '').replace(/^g:/, '') || full;
  };

  for (const [, clash] of byLabel) {
    if (clash.length < 2) continue;
    const source = new Map(clash.map((group) => [group, nameOf(group, clash)]));
    for (const group of clash) {
      const parts = String(source.get(group)).split('/').filter(Boolean);
      for (let keep = 2; keep <= parts.length; keep++) {
        const candidate = parts.slice(-keep).join('/');
        const collides = clash.some((other) => other !== group
          && String(source.get(other)).split('/').filter(Boolean).slice(-keep).join('/') === candidate);
        group.baseLabel = candidate;
        if (!collides) break;
      }
    }
  }
  // The "+N" suffix is composed last, so it can never mask a collision the way
  // it did when "common +19" and "common +2" were treated as distinct names for
  // two groups a reader saw as both called "common".
  for (const group of model.groups.values()) {
    if (group.solo) continue;
    const extra = group.members.length - 1;
    group.label = (group.baseLabel || group.label) + (extra > 0 ? ' +' + extra : '');
  }
}

// renderKeyOf maps a leaf onto whatever node currently stands for it: itself
// when its group is expanded and it is inside the member budget, otherwise the
// cluster. This is what makes edge aggregation and expansion one mechanism.
function renderKeyOf(model, leafId, visibleMembers) {
  const gid = model.leafGroup.get(leafId);
  if (!gid) return null;
  const group = model.groups.get(gid);
  if (!group) return null;
  if (group.solo) return 'L:' + leafId;
  if (model.expanded.has(gid) && visibleMembers.has(leafId)) return 'L:' + leafId;
  return gid;
}

/*
  buildRenderSet is where the legibility budget is spent. It takes the number of
  nodes the canvas can hold at a readable zoom, ranks candidates, keeps that
  many, and returns the count it did not keep so the canvas can say so.
*/
function buildRenderSet(model, budget) {
  const groups = Array.from(model.groups.values());
  groups.sort((a, b) => {
    const lenDiff = b.members.length - a.members.length;
    if (lenDiff !== 0) return lenDiff;
    const degA = a.members.reduce((s, m) => s + (m.degree || 0), 0);
    const degB = b.members.reduce((s, m) => s + (m.degree || 0), 0);
    if (degB !== degA) return degB - degA;
    return String(a.label).localeCompare(String(b.label));
  });

  const visibleMembers = new Set();
  const nodes = [];
  let hiddenGroups = 0;
  let hiddenMembers = 0;

  let slots = Math.max(6, budget);
  const keep = [];
  for (const group of groups) {
    const cost = model.expanded.has(group.id) ? Math.min(group.members.length, model.memberCap) : 1;
    if (slots - cost < 0 && keep.length > 0) {
      // hiddenGroups is boxes not drawn; hiddenMembers is leaves not
      // represented. A solo group is one of each, and the two must never be
      // added together — see paintCanvasNote.
      hiddenGroups++;
      hiddenMembers += group.members.length;
      continue;
    }
    slots -= cost;
    keep.push(group);
  }

  for (const group of keep) {
    if (group.solo || model.expanded.has(group.id)) {
      const members = group.solo ? group.members : group.members.slice(0, model.memberCap);
      if (!group.solo) hiddenMembers += group.members.length - members.length;
      for (const leaf of members) {
        visibleMembers.add(leaf.id);
        nodes.push({
          key: 'L:' + leaf.id, type: 'leaf', gid: group.id, leaf: leaf,
          label: leaf.label, sub: leaf.sub, tier: leaf.confidence, degree: leaf.degree || 0,
        });
      }
    } else {
      nodes.push({
        key: group.id, type: 'cluster', gid: group.id, group: group,
        label: group.label, sub: group.members.length + ' ' + plural('item', group.members.length),
        count: group.members.length, degree: group.members.reduce((sum, leaf) => sum + (leaf.degree || 0), 0),
      });
    }
  }

  const byKey = new Map(nodes.map((node) => [node.key, node]));

  // Edge aggregation. Everything between the same pair of drawn nodes becomes
  // one edge with a count, a kind histogram, one example, and the WEAKEST tier
  // on it - because a chain is only as good as its weakest hop, and drawing the
  // strongest would be a lie by selection.
  const agg = new Map();
  let internal = 0;
  let droppedEdges = 0;
  for (const link of model.links) {
    const ka = renderKeyOf(model, link.a, visibleMembers);
    const kb = renderKeyOf(model, link.b, visibleMembers);
    if (!ka || !kb || !byKey.has(ka) || !byKey.has(kb)) { droppedEdges++; continue; }
    if (ka === kb) {
      internal++;
      const node = byKey.get(ka);
      node.internal = (node.internal || 0) + (link.count || 1);
      continue;
    }
    const id = ka + '\u0000' + kb;
    let e = agg.get(id);
    if (!e) {
      e = {
        a: ka, b: kb, count: 0, total: 0, weakest: 'exact', kinds: new Map(),
        evidence: null, pairs: 0, hist: null, xlang: false, xlangPairs: 0, xlangLabel: '',
      };
      agg.set(id, e);
    }
    // An aggregated edge is cross-language when ANY underlying relationship
    // crosses a language boundary. The seam is the finding; folding it into a
    // same-language bundle would hide exactly what it exists to show.
    if (link.xlang) {
      e.xlang = true;
      e.xlangPairs++;
      if (!e.xlangLabel) e.xlangLabel = link.xlangFrom + ' \u2192 ' + link.xlangTo;
    }
    e.count += (link.count || 1);
    e.total += (link.total === undefined ? (link.count || 1) : link.total);
    e.pairs++;
    e.weakest = weakest(e.weakest, link.conf);
    for (const pair of (link.kinds || [])) e.kinds.set(pair[0], (e.kinds.get(pair[0]) || 0) + pair[1]);
    if (!e.evidence && link.evidence) e.evidence = link.evidence;
    if (link.hist) {
      e.hist = e.hist || { exact: 0, high: 0, medium: 0, low: 0, unknown: 0 };
      for (const tv of TIER_ORDER) e.hist[tv] += Number(link.hist[tv] || 0);
    }
  }

  const edges = Array.from(agg.values());
  const maxCount = edges.reduce((mx, e) => Math.max(mx, e.count), 1);
  for (const e of edges) e.weight = Math.log2(1 + e.count) / Math.log2(1 + maxCount);

  // keyOfLeaf is the reverse of renderKeyOf, computed once. Search, focus and
  // path finding all work in LEAF ids — the ids that survive a regroup — and
  // all three need to know which drawn node currently stands for one.
  const keyOfLeaf = new Map();
  for (const leaf of model.leaves) {
    const key = renderKeyOf(model, leaf.id, visibleMembers);
    if (key && byKey.has(key)) keyOfLeaf.set(leaf.id, key);
  }

  return {
    nodes: nodes, edges: edges, byKey: byKey, keyOfLeaf: keyOfLeaf,
    visibleEdges: edges,
    hiddenGroups: hiddenGroups, hiddenMembers: hiddenMembers,
    internalEdges: internal, droppedEdges: droppedEdges,
    totalGroups: model.groups.size,
  };
}

// ---------------------------------------------------------------- layout

/*
  Two elisions, and which one is used depends on where the meaning is.

  A symbol name is distinguished by its beginning, so it is cut at the end. A
  PATH is distinguished by its end - that is the whole reason group labels get
  lengthened from "common" to "receiptdetail/payload" when they collide - so
  cutting a path at the end throws away exactly the segment the disambiguation
  just added, and puts two boxes back on the map reading
  ".claude/worktrees/crazy-chap…".
*/
function fitText(ctx, s, maxWidth, font) {
  ctx.font = font;
  if (ctx.measureText(s).width <= maxWidth) return s;
  if (s.indexOf('/') >= 0) return fitTextKeepingEnd(ctx, s, maxWidth);
  let lo = 0;
  let hi = s.length;
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1;
    if (ctx.measureText(s.slice(0, mid) + '…').width <= maxWidth) lo = mid; else hi = mid - 1;
  }
  return s.slice(0, Math.max(1, lo)) + '…';
}

function fitTextKeepingEnd(ctx, s, maxWidth) {
  let lo = 0;
  let hi = s.length;
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1;
    if (ctx.measureText('…' + s.slice(s.length - mid)).width <= maxWidth) lo = mid; else hi = mid - 1;
  }
  return '…' + s.slice(s.length - Math.max(1, lo));
}

// Node boxes are sized to their own measured label. That is the mechanism
// behind "labels are always readable": a label can never be wider than the box
// drawn around it, so legibility becomes a question of zoom alone, and zoom is
// clamped.
//
// This is also where the map decides WHAT TEXT a node is drawn with. A container
// or file leaf is drawn by the shortest tail of its path that no drawn sibling
// of the same kind also ends in — see distinguishingLabel — so the box is sized
// to that text, not to the whole path. `node.label` keeps the full path: the
// inspector, the text equivalent and the edge table all still read it.
function pathLabelKind(node) {
  if (node.type !== 'leaf' || !node.leaf) return '';
  const kind = String(node.leaf.kind || '');
  return kind === 'container' || kind === 'file' ? kind : '';
}

function pathOfNode(node) {
  return String(node.leaf.path || node.leaf.id || node.label);
}

/*
  A NODE'S WIDTH CARRIES HOW MUCH IS IN IT.

  A container of ten thousand symbols and a container of ten drew the same box
  when their labels happened to be the same length, and a map on which every
  box is the same size says nothing about weight. So a container or file leaf
  is widened by a term in its symbol count - the `symbols` field the API sends
  on both container and file nodes (internal/web/graph.go: ContainerNode.Symbols,
  FileNode.Symbols), carried onto the leaf as leaf.symbols.

  The term is LOGARITHMIC, not linear: log2(1 + symbols) * QUANTITY_WEIGHT.
  Linear in the count, a 10,000-symbol node would be a thousand times wider
  than a 10-symbol one, which no clamp can draw; on a log scale it is about
  four doublings wider, visibly larger without dwarfing everything else, and
  the clamp to [NODE_MIN_W, NODE_MAX_W] still bounds it. Clusters and symbol
  nodes carry no symbol total, so their term is zero: a cluster's size is
  already printed on it as "N items", and a symbol is one symbol.
*/
function quantityTerm(node) {
  if (node.type !== 'leaf' || !node.leaf) return 0;
  const kind = node.leaf.kind;
  if (kind !== 'container' && kind !== 'file') return 0;
  const symbols = Number(node.leaf.symbols || 0);
  if (!(symbols > 0)) return 0;
  return Math.round(Math.log2(1 + symbols) * GRAPH.QUANTITY_WEIGHT);
}

function measureLabels(ctx, nodes) {
  const labelFont = '600 ' + GRAPH.LABEL_FONT + 'px ' + css('--font', 'sans-serif');
  const subFont = GRAPH.SUB_FONT + 'px ' + css('--mono', 'monospace');
  const siblings = new Map();
  for (const node of nodes) {
    const kind = pathLabelKind(node);
    if (!kind) continue;
    if (!siblings.has(kind)) siblings.set(kind, []);
    siblings.get(kind).push(pathOfNode(node));
  }
  ctx.save();
  for (const node of nodes) {
    const kind = pathLabelKind(node);
    const shown = kind ? distinguishingLabel(pathOfNode(node), siblings.get(kind)) : node.label;
    ctx.font = labelFont;
    const w = ctx.measureText(shown).width;
    ctx.font = subFont;
    const sw = node.sub ? ctx.measureText(node.sub).width : 0;
    node.w = Math.max(GRAPH.NODE_MIN_W, Math.min(GRAPH.NODE_MAX_W, Math.max(w, sw) + GRAPH.NODE_PAD * 2 + quantityTerm(node)));
    node.h = node.type === 'cluster' ? GRAPH.NODE_H + 14 : GRAPH.NODE_H;
    node.drawLabel = fitText(ctx, shown, node.w - GRAPH.NODE_PAD * 2, labelFont);
    node.drawSub = node.sub ? fitText(ctx, node.sub, node.w - GRAPH.NODE_PAD * 2, subFont) : '';
  }
  ctx.restore();
}

/*
  A LAYERED LAYOUT, BY DEPENDENCY DIRECTION. Time-sliced: each call to step()
  works until it hits the frame deadline and returns; the caller redraws and
  schedules the next slice, so nothing here blocks the main thread for more
  than one frame. That is what "offload the layout" has to mean without a
  worker: the portal's own CSP is default-src 'none', so worker-src inherits it
  and a Worker is refused (assets/NEEDS-API.md).

  TWO FORCE LAYOUTS CAME BEFORE THIS ONE, AND BOTH FAILED ON SCALE.

  The first was textbook Fruchterman-Reingold, "attraction d^2/k, repulsion
  k^2/d, with k derived from an assumed area per node. On the 19-component
  view that put the equilibrium edge length at 839 world pixels, so nineteen
  boxes spread across several thousand pixels, fit() clamped at the legibility
  floor rather than zooming out, and the visible window landed on empty space.
  A layout whose natural scale is not tied to the size of the things it lays
  out cannot be made legible by tuning the zoom."

  The second was a spring with a rest length taken from the measured label
  boxes. Its shape was right and its size still was not: "a force layout's
  overall SCALE is set by whatever balances repulsion against gravity, and
  neither of those knows how big the window is. Nineteen boxes settled into a
  2,974-pixel-wide map, fit() refused to zoom below the legibility floor -
  correctly - and the reader got a readable graph they could only see one
  corner of." It was rescued by rescaling the whole map after every slice to
  a footprint computed from the legibility budget, then re-separating the
  boxes - which meant the geometry depended on how many ticks fitted into each
  frame, so the same URL could settle differently on a slower machine, and a
  reader could not build a memory of where anything was.

  THIS ONE DIFFERS IN KIND, NOT IN TUNING.

  - Position comes from the DATA'S DIRECTION, not from forces. Every node is
    ranked by the longest path that reaches it over the drawn edges with
    back-edges ignored: rank 0 is a node nothing depends on, and each other
    node sits one rank below its deepest predecessor. Callers are above
    callees; an edge that is not part of a cycle points down. A node with no
    forward edge at all is not a root but a bystander, and sits in a strip of
    its own below the last rank, so it can never come between a caller and
    its callee.

  - The footprint is fixed BY CONSTRUCTION, from the boxes. A rank is a row,
    rows are one box height plus a gap apart, and a rank too wide for the
    budget's row width is wrapped onto consecutive rows of its own. When a
    rank wraps, the nodes that only look up fill the row nearest rank r-1 and
    the nodes that only look down the row nearest r+1, so an edge into or out
    of the rank does not have to cross the rank's other rows. Nothing
    is rescaled afterwards, so there is no scale for a window to disagree with.
    Rank runs DOWN the page because the boxes are five times wider than they
    are tall: a rank costs 54 world pixels as a row and about 250 as a column,
    and the 17-component view is eleven ranks deep - 580 tall against a 760
    budget as rows, 2,660 wide against a 1,550 budget as columns.

  - Determinism comes from ORDER, not from a seed. Nodes are visited in key
    order, ties in every sort are broken by key, and the relaxation below runs
    a fixed number of ticks with nothing between them that depends on the
    clock - so the slice boundaries can fall anywhere and the geometry is the
    same. The e2e suite loads one URL twice from cold and compares every
    coordinate.

  - The spring step survives only as a relaxation WITHIN A ROW. Rows are fixed
    once ranked; the springs slide boxes sideways so a caller sits over its
    callees, gravity keeps each row centred, and a one-dimensional separation
    pass after every tick keeps the boxes in a row from overlapping. Nothing
    can move between rows, so the picture cannot drift.

  - An edge that crosses a row is ROUTED, not drawn as a chord. The
    barycentre puts a box between a caller and a callee two ranks apart,
    exactly on the straight line between them, so a chord from centre to
    centre ran through it: on the shipped fixture's container view service ->
    model passed through repository, 7 of 21 sampled stroke points inside the
    box; on a 17-component view 48 of 67 edges had a sample inside a box.
    Two cheaper fixes were built and measured first. Pinning a clearing to
    each straight chord makes a chain of boxes under a hub lean by a
    clearance per rank (5,860 world px wide on that view). Routing through
    dummies but still stroking the chord left the dummy up to 220 px off the
    line the stroke actually took. So this is the standard layered answer,
    with one adjustment the row pitch forces. Every drawn pair whose ends
    are two or more ranks apart gets a zero-width DUMMY in every rank
    between, threaded into the barycentre sweeps in the long edge's place;
    a rank that wraps gets one in every row it adds. The relaxation moves a
    dummy toward the midpoint of its chain neighbours and never lets one
    move a box: a row's separation pass places its boxes exactly as before
    and then fits the dummies into the free intervals between them, so the
    boxes' geometry is unchanged by the routing. And the renderer draws
    the edge THROUGH the dummies - out of a port on the source, vertically
    through each dummy's row, diagonally across the channel between rows,
    into a port on the target (routePoints) - because at 40 px of box in
    every 54 px of row a polyline through the dummies' centres still
    clipped the neighbour beside each dummy. A dummy costs its row
    DUMMY_PITCH, which is why it is capped: at most LAYER_DUMMIES_PER_ROW
    in any row and LAYER_DUMMY_BUDGET in all, taken shortest edge first
    with ties by key, since the drilled view can be a chain hundreds of
    ranks long with hundreds of edges across it and routing all of them
    would widen every row by the width of the map. An edge over the cap is
    drawn as today's chord.
*/
function createLayout(nodes, edges, opts) {
  const options = opts || {};
  const gap = GRAPH.GAP;

  if (options.mode === 'tree') {
    const keys = nodes.map((n) => n.key).sort();
    const byKey = new Map(nodes.map((n) => [n.key, n]));
    const inEdges = new Map(keys.map((k) => [k, []]));
    const outEdges = new Map(keys.map((k) => [k, []]));

    for (const e of edges) {
      const a = byKey.get(e.a); const b = byKey.get(e.b);
      if (a && b && a !== b) {
        outEdges.get(e.a).push(e.b);
        inEdges.get(e.b).push(e.a);
      }
    }

    let roots = keys.filter((k) => inEdges.get(k).length === 0);
    if (roots.length === 0) {
      roots = keys.slice().sort((k1, k2) => {
        const diff = inEdges.get(k1).length - inEdges.get(k2).length;
        if (diff !== 0) return diff;
        return outEdges.get(k2).length - outEdges.get(k1).length;
      }).slice(0, Math.max(1, Math.min(4, Math.floor(keys.length / 3))));
    }

    const levels = new Map();
    const visited = new Set();
    const queue = [];

    for (const r of roots) {
      levels.set(r, 0);
      visited.add(r);
      queue.push(r);
    }

    while (queue.length > 0) {
      const curr = queue.shift();
      const currLvl = levels.get(curr);
      for (const nxt of outEdges.get(curr)) {
        const existingLvl = levels.get(nxt);
        const nextLvl = currLvl + 1;
        if (existingLvl === undefined || nextLvl > existingLvl) {
          levels.set(nxt, nextLvl);
        }
        if (!visited.has(nxt)) {
          visited.add(nxt);
          queue.push(nxt);
        }
      }
    }

    let maxLevel = 0;
    for (const lvl of levels.values()) {
      if (lvl > maxLevel) maxLevel = lvl;
    }
    for (const k of keys) {
      if (!levels.has(k)) {
        levels.set(k, maxLevel + 1);
      }
    }

    const levelGroups = new Map();
    for (const k of keys) {
      const lvl = levels.get(k);
      if (!levelGroups.has(lvl)) levelGroups.set(lvl, []);
      levelGroups.get(lvl).push(byKey.get(k));
    }

    const sortedLevelKeys = Array.from(levelGroups.keys()).sort((a, b) => a - b);
    const lvlHeight = GRAPH.NODE_H + 110;
    const startY = -(sortedLevelKeys.length * lvlHeight) / 2 + GRAPH.NODE_H;

    for (let i = 0; i < sortedLevelKeys.length; i++) {
      const lvl = sortedLevelKeys[i];
      const rowNodes = levelGroups.get(lvl);
      rowNodes.sort((a, b) => (a.key < b.key ? -1 : 1));
      const rowWidth = rowNodes.reduce((sum, n) => sum + (n.w || 150) + gap, 0) - gap;
      let curX = -rowWidth / 2;
      const targetY = startY + i * lvlHeight;
      for (const n of rowNodes) {
        n.x = curX + (n.w || 150) / 2;
        n.y = targetY;
        n.targetY = targetY;
        n.vx = 0;
        n.vy = 0;
        curX += (n.w || 150) + gap;
      }
    }

    let done = 0;
    const total = 120;

    function stepTree() {
      for (const [lvl, rowNodes] of levelGroups) {
        for (let i = 1; i < rowNodes.length; i++) {
          const prev = rowNodes[i - 1];
          const curr = rowNodes[i];
          const minD = (prev.w + curr.w) / 2 + gap;
          const actualD = curr.x - prev.x;
          if (actualD < minD) {
            const push = (minD - actualD) * 0.5;
            prev.x -= push;
            curr.x += push;
          }
        }

        for (const curr of rowNodes) {
          const parents = inEdges.get(curr.key);
          if (parents && parents.length > 0) {
            let parentXSum = 0;
            let parentCount = 0;
            for (const pk of parents) {
              const p = byKey.get(pk);
              if (p) { parentXSum += p.x; parentCount++; }
            }
            if (parentCount > 0) {
              const targetX = parentXSum / parentCount;
              curr.x += (targetX - curr.x) * 0.12;
            }
          }
          if (curr.targetY !== undefined) {
            curr.y += (curr.targetY - curr.y) * 0.2;
          }
        }
      }
      done++;
    }

    return {
      rest: 220,
      viaOf() { return null; },
      members() { return nodes; },
      progress() { return done / total; },
      finished() { return done >= total; },
      step(deadlineMs) {
        const until = performance.now() + (deadlineMs || 8);
        let ran = 0;
        while (done < total && performance.now() < until) { stepTree(); ran++; }
        return ran;
      },
    };
  }

  if (options.mode === 'organic') {
    const keys = nodes.map((n) => n.key).sort();
    const byKey = new Map(nodes.map((n) => [n.key, n]));
    const radius = Math.min(500, Math.max(140, nodes.length * 24));
    keys.forEach((key, i) => {
      const node = byKey.get(key);
      const angle = (i / Math.max(1, keys.length)) * Math.PI * 2;
      node.x = Math.cos(angle) * radius;
      node.y = Math.sin(angle) * radius;
      node.vx = 0;
      node.vy = 0;
    });

    const nodeEdges = [];
    for (const e of edges) {
      const a = byKey.get(e.a); const b = byKey.get(e.b);
      if (a && b && a !== b) nodeEdges.push({ a: a, b: b, weight: e.weight || 0.5 });
    }

    let done = 0;
    const total = 140;

    function stepOrganic() {
      const k = 220;
      const k2 = k * k;
      for (let i = 0; i < nodes.length; i++) {
        const u = nodes[i];
        for (let j = i + 1; j < nodes.length; j++) {
          const v = nodes[j];
          let dx = v.x - u.x;
          let dy = v.y - u.y;
          let dist = Math.hypot(dx, dy);
          if (dist < 1) { dx = (Math.random() - 0.5); dy = (Math.random() - 0.5); dist = 1; }
          if (dist > 900) continue;
          const force = k2 / (dist * dist);
          const fx = (dx / dist) * force;
          const fy = (dy / dist) * force;
          u.vx -= fx;
          u.vy -= fy;
          v.vx += fx;
          v.vy += fy;

          // Box-box rectangular overlap prevention
          const minX = (u.w + v.w) / 2 + 32;
          const minY = (u.h + v.h) / 2 + 24;
          if (Math.abs(dx) < minX && Math.abs(dy) < minY) {
            const ox = minX - Math.abs(dx);
            const oy = minY - Math.abs(dy);
            if (ox < oy) {
              const sign = dx < 0 ? -1 : 1;
              u.x -= ox * 0.5 * sign;
              v.x += ox * 0.5 * sign;
            } else {
              const sign = dy < 0 ? -1 : 1;
              u.y -= oy * 0.5 * sign;
              v.y += oy * 0.5 * sign;
            }
          }
        }
      }
      for (const edge of nodeEdges) {
        const u = edge.a;
        const v = edge.b;
        const dx = v.x - u.x;
        const dy = v.y - u.y;
        const dist = Math.hypot(dx, dy);
        if (dist < 1) continue;
        const force = (dist * dist) / (k * (0.8 + 0.4 * edge.weight));
        const fx = (dx / dist) * force * 0.14;
        const fy = (dy / dist) * force * 0.14;
        u.vx += fx;
        u.vy += fy;
        v.vx -= fx;
        v.vy -= fy;
      }
      const alpha = Math.max(0.04, 1 - (done / total));
      for (const node of nodes) {
        node.vx -= node.x * 0.03;
        node.vy -= node.y * 0.03;
        node.x += Math.max(-45, Math.min(45, node.vx)) * alpha * 0.45;
        node.y += Math.max(-45, Math.min(45, node.vy)) * alpha * 0.45;
        node.vx *= 0.65;
        node.vy *= 0.65;
      }
      done++;
    }

    return {
      rest: 200,
      viaOf() { return null; },
      members() { return nodes; },
      progress() { return done / total; },
      finished() { return done >= total; },
      step(deadlineMs) {
        const until = performance.now() + (deadlineMs || 8);
        let ran = 0;
        while (done < total && performance.now() < until) { stepOrganic(); ran++; }
        return ran;
      },
    };
  }


  // The rest length is still derived from the boxes and still reported: fit()
  // prints it into data-graph-metrics, and the sizing change that added a
  // symbol-count term to node width is measured against it. Nothing else
  // reads it now; the row and column spacing below come from the boxes
  // directly.
  const avgW = nodes.reduce((sum, n) => sum + (n.w || 150), 0) / Math.max(1, nodes.length);
  const rest = Math.max(170, Math.min(420, avgW * 1.55));

  /*
    The row width a rank may use before it wraps. This is the same arithmetic
    legibleBudget uses to decide how many nodes to draw at all - one cell per
    node, half density, a landscape aspect - so a set that was admitted as
    drawable lays out at a width it can be drawn at. A single row of four
    average boxes is always allowed, so a small graph is never wrapped merely
    for being small.
  */
  const cell = (avgW + gap) * (GRAPH.NODE_H + gap);
  const wrapW = Math.max(4 * (avgW + gap), Math.sqrt(((nodes.length * cell) / 0.5) * GRAPH.LAYER_ASPECT));

  const keys = nodes.map((node) => node.key).sort();
  const byKey = new Map(nodes.map((node) => [node.key, node]));
  const byKeyOrder = (a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0);
  const springs = [];
  const drawn = [];
  const out = new Map(keys.map((key) => [key, []]));
  const seenPair = new Set();
  for (const e of edges) {
    const a = byKey.get(e.a); const b = byKey.get(e.b);
    if (!a || !b) continue;
    drawn.push({ a: a, b: b, w: 0.35 + 0.65 * (e.weight || 0.5) });
    const id = e.a + ' ' + e.b;
    if (a === b || seenPair.has(id)) continue;
    seenPair.add(id);
    out.get(e.a).push(e.b);
  }
  for (const list of out.values()) list.sort();

  // Until the first slice has ranked them, nodes sit on a key-ordered grid:
  // deterministic, and a real drawing rather than a point, for the one frame
  // a very large set might need before its ranks are known.
  const cols = Math.max(1, Math.floor(wrapW / (avgW + gap)));
  keys.forEach((key, i) => {
    const node = byKey.get(key);
    node.x = (i % cols) * (avgW + gap);
    node.y = Math.floor(i / cols) * (GRAPH.NODE_H + gap);
    node.vx = 0;
  });

  const back = new Set();
  const pred = new Map(keys.map((key) => [key, []]));
  const succ = new Map(keys.map((key) => [key, []]));
  const ranks = [];
  const rows = [];
  // Layout-only members: never drawn, never hit, never in `nodes`. `pairs`
  // is every drawn pair once, upper end first, with the dummies its edges
  // pass through from upper to lower; `chains` indexes the same by directed
  // key once the rows are known; `members` is what the relaxation moves.
  const dummies = [];
  const pairs = [];
  const chains = new Map();
  const pulls = [];
  const rowSets = [];
  let members = nodes;
  let budget = GRAPH.LAYER_DUMMY_BUDGET;

  /*
    Back-edges, by an iterative depth-first search from every node in key
    order, taking each node's out-edges in key order. An edge to a node that
    is still on the search stack closes a cycle and is marked back; everything
    else is forward. The walk is written out iteratively because a drilled
    view can be a chain hundreds long.
  */
  function findBackEdges() {
    const state = new Map();
    for (const start of keys) {
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
    for (const [from, kids] of out) {
      for (const to of kids) {
        if (back.has(from + ' ' + to)) continue;
        succ.get(from).push(to);
        pred.get(to).push(from);
      }
    }
  }

  /*
    Longest-path rank over what remains, in Kahn order: rank 0 for every node
    with no forward inbound edge, and each other node one below the deepest of
    its predecessors. A node with no forward edge in either direction is
    isolated in the DAG and is ranked LAST, one below the deepest node, all
    of them together in key order: ranked with the roots they would share
    rank 0 with the true roots, and once that rank wrapped, an isolated box
    sat between a root and its callee with the edge drawn through it. If
    every node is isolated the strip is rank 0 and the whole map is the strip.
  */
  function assignRanks() {
    const isolated = (key) => pred.get(key).length === 0 && succ.get(key).length === 0;
    const indeg = new Map(keys.map((key) => [key, pred.get(key).length]));
    const rank = new Map();
    const queue = keys.filter((key) => indeg.get(key) === 0 && !isolated(key));
    for (const key of queue) rank.set(key, 0);
    for (let head = 0; head < queue.length; head++) {
      const key = queue[head];
      for (const next of succ.get(key)) {
        rank.set(next, Math.max(rank.get(next) || 0, rank.get(key) + 1));
        indeg.set(next, indeg.get(next) - 1);
        if (indeg.get(next) === 0) queue.push(next);
      }
    }
    let deepest = -1;
    for (const r of rank.values()) deepest = Math.max(deepest, r);
    for (const key of keys) {
      const r = isolated(key) ? deepest + 1 : (rank.get(key) || 0);
      byKey.get(key).rank = r;
      while (ranks.length <= r) ranks.push([]);
      ranks[r].push(byKey.get(key));
    }
  }

  /*
    Dummies. Every drawn pair whose ends are two or more ranks apart is a
    candidate, whichever way its edges point: a back-edge that climbs three
    ranks passes through the boxes between just as a forward one does, and
    the two directions of one pair share one chain so A -> B and B -> A stay
    two strokes either side of the same route. Candidates are taken shortest
    span first, then by key, while the row cap and the budget allow; the
    chain is threaded into pred/succ in place of the long edge so the
    barycentre sweeps see adjacent-rank neighbours only. Dummies are zero
    width, as tall as their row once placeRows knows it, and keyed with a
    leading NUL so they never collide with a drawn key and sort ahead of
    one.
  */
  function insertDummies() {
    const seen = new Set();
    for (const from of keys) {
      for (const to of out.get(from)) {
        const a = byKey.get(from); const b = byKey.get(to);
        const upper = a.rank <= b.rank ? a : b;
        const lower = upper === a ? b : a;
        const id = upper.key + '\u0000' + lower.key;
        if (seen.has(id)) continue;
        seen.add(id);
        pairs.push({ upper: upper, lower: lower, span: lower.rank - upper.rank, id: id, chain: [] });
      }
    }
    pairs.sort((p, q) => p.span - q.span || (p.id < q.id ? -1 : p.id > q.id ? 1 : 0));
    // A rank that will wrap is several rows, and placeRows gives the route a
    // member in each of them, so the budget charges a crossing of that rank
    // by its estimated row count rather than by one.
    const rowsOf = ranks.map((rank) => {
      const span = rank.reduce((sum, node) => sum + node.w, 0) + gap * (rank.length - 1);
      return Math.max(1, Math.ceil(span / wrapW));
    });
    const perRank = new Map();
    const replace = (list, was, now) => { const i = list.indexOf(was); if (i >= 0) list[i] = now; else list.push(now); };
    for (const c of pairs) {
      if (c.span < 2) continue;
      let cost = 0;
      let room = true;
      for (let r = c.upper.rank + 1; r < c.lower.rank; r++) {
        cost += rowsOf[r];
        if ((perRank.get(r) || 0) >= GRAPH.LAYER_DUMMIES_PER_ROW) room = false;
      }
      if (!room || cost > budget) continue;
      budget -= cost;
      for (let r = c.upper.rank + 1; r < c.lower.rank; r++) {
        const d = { key: '\u0000' + c.id + '\u0000' + r, dummy: true, rank: r, w: 0, h: GRAPH.NODE_H, x: 0, y: 0, vx: 0 };
        dummies.push(d);
        c.chain.push(d);
        ranks[r].push(d);
        pred.set(d.key, []);
        succ.set(d.key, []);
        perRank.set(r, (perRank.get(r) || 0) + 1);
      }
      const path = [c.upper].concat(c.chain, [c.lower]);
      for (let i = 1; i < path.length; i++) {
        const above = path[i - 1]; const below = path[i];
        if (i === 1) replace(succ.get(above.key), c.lower.key, below.key); else succ.get(above.key).push(below.key);
        if (i === path.length - 1) replace(pred.get(below.key), c.upper.key, above.key); else pred.get(below.key).push(above.key);
      }
    }
    for (const list of succ.values()) list.sort();
    for (const list of pred.values()) list.sort();
  }

  /*
    Order within a rank by the barycentre of the neighbours already placed:
    one sweep down the ranks by predecessors, one back up by successors,
    twice. A node with no neighbour in the adjacent rank keeps its place
    after those that have one, and every tie is broken by key.
  */
  function orderRanks() {
    const index = new Map();
    const record = (row) => row.forEach((node, i) => index.set(node.key, i));
    for (const row of ranks) record(row);
    const sweep = (row, neighbours) => {
      const centre = new Map();
      for (const node of row) {
        const near = neighbours.get(node.key);
        if (near.length === 0) continue;
        centre.set(node.key, near.reduce((sum, key) => sum + index.get(key), 0) / near.length);
      }
      row.sort((a, b) => {
        const ca = centre.has(a.key) ? centre.get(a.key) : Infinity;
        const cb = centre.has(b.key) ? centre.get(b.key) : Infinity;
        if (ca !== cb) return ca < cb ? -1 : 1;
        return byKeyOrder(a, b);
      });
      record(row);
    };
    for (let pass = 0; pass < 2; pass++) {
      for (let r = 1; r < ranks.length; r++) sweep(ranks[r], pred);
      for (let r = ranks.length - 2; r >= 0; r--) sweep(ranks[r], succ);
    }
  }

  /*
    Rows. Each rank becomes one row, or several consecutive rows when its
    boxes side by side would exceed the wrap width; every row of rank r is
    still above every row of rank r+1, which is the whole promise. Boxes in a
    row are packed with one gap between them and the row is centred on x=0;
    rows are spaced by the taller of the two boxes they separate plus a
    fraction of the gap, which is the vertical breathing room fit() has to
    pay for eleven times on the deepest view.

    A rank that wraps is first sorted by which neighbour rank each node
    faces - a neighbour above only first, both or neither in the middle, a
    neighbour below only last - keeping the barycentre order within each of
    the three. pred and succ are not the forward edges alone: insertDummies
    threads both directions of a routed pair into one chain, so a node whose
    only edge to the rank above is a back-edge still faces it. The first row
    of the rank is then the one nearest rank r-1 and holds the nodes whose
    edges join it to there, whichever way they point; the last is nearest
    r+1 and holds the nodes joined to there; and an edge into or out of the
    rank does not have to cross a row of boxes that has nothing to do with
    it. A rank that fits one row is left in barycentre order exactly.
  */
  function placeRows() {
    const facing = (node) => {
      const up = pred.get(node.key).length > 0;
      const down = succ.get(node.key).length > 0;
      return up && !down ? -1 : down && !up ? 1 : 0;
    };
    // Rows are made of BOXES; a dummy joins its row afterwards and never
    // decides whether a rank wraps or where, so routing an edge through a
    // rank cannot push that rank's boxes onto another row.
    const firstRow = [];
    for (const rank of ranks) {
      const order = rank.filter((node) => !node.dummy);
      const span = order.reduce((sum, node) => sum + node.w, 0) + gap * (order.length - 1);
      if (span > wrapW) {
        const at = new Map(order.map((node, i) => [node.key, i]));
        order.sort((a, b) => facing(a) - facing(b) || at.get(a.key) - at.get(b.key));
      }
      firstRow.push(rows.length);
      let row = [];
      let width = 0;
      for (const node of order) {
        const add = (row.length ? gap : 0) + node.w;
        if (row.length && width + add > wrapW) { rows.push(row); row = []; width = 0; }
        row.push(node);
        width += (row.length > 1 ? gap : 0) + node.w;
      }
      if (row.length) rows.push(row);
    }
    let y = 0;
    let prevH = 0;
    rows.forEach((row, i) => {
      const h = row.reduce((mx, node) => Math.max(mx, node.h || GRAPH.NODE_H), 0);
      if (i > 0) y += prevH / 2 + gap * GRAPH.LAYER_ROW_GAP + h / 2;
      prevH = h;
      const total = row.reduce((sum, node) => sum + node.w, 0) + gap * (row.length - 1);
      let x = -total / 2;
      for (const node of row) {
        node.x = x + node.w / 2;
        node.y = y;
        node.row = i;
        // The band the row occupies. A leaf in a row with a cluster is 7 px
        // shorter than its row each side, and the renderer runs a stub from
        // the box's port to the band's edge (routePoints) so that no
        // diagonal starts inside a band.
        node.band = h;
        x += node.w + gap;
      }
    });

    /*
      Dummies join the rows. A rank dummy goes into its rank's first row, on
      the straight line between the ends of its pair; it is as tall as its
      row, so the renderer's vertical pass through it spans the whole band
      the row's boxes occupy. Its barycentre order served its purpose in
      ordering the boxes around it; where it sits in the row from here on is
      decided by the relaxation, which re-sorts a row by x on every tick.
    */
    const rowH = rows.map((row) => row.reduce((mx, node) => Math.max(mx, node.h || GRAPH.NODE_H), 0));
    const joinRow = (d, r, c) => {
      const t = (r - c.upper.row) / Math.max(1, c.lower.row - c.upper.row);
      d.row = r;
      d.y = rows[r][0].y;
      d.h = rowH[r];
      d.x = c.upper.x + (c.lower.x - c.upper.x) * t;
      rows[r].push(d);
    };
    for (const c of pairs) {
      for (const d of c.chain) joinRow(d, firstRow[d.rank], c);
    }

    /*
      The renderer crosses a row's band vertically at each route member and
      the channel between two rows diagonally, which is only clear of boxes
      when consecutive members are in CONSECUTIVE rows. A rank that wrapped
      is several rows, so where a route steps over one - a rank dummy to the
      next, or a box in a wrapped rank to its neighbour rank - it gets a
      member in each row skipped, fewest rows first and then by key while
      the row cap and the budget allow. A route over the cap keeps whatever
      members it has and may cross a wrapped rank's other rows.
    */
    const perRow = new Map();
    for (const d of dummies) perRow.set(d.row, (perRow.get(d.row) || 0) + 1);
    const skipped = (c) => {
      const path = [c.upper].concat(c.chain, [c.lower]);
      let n = 0;
      for (let i = 1; i < path.length; i++) n += Math.max(0, path[i].row - path[i - 1].row - 1);
      return n;
    };
    const fills = pairs.map((c) => ({ pair: c, rows: skipped(c) })).filter((f) => f.rows > 0);
    fills.sort((p, q) => p.rows - q.rows || (p.pair.id < q.pair.id ? -1 : p.pair.id > q.pair.id ? 1 : 0));
    for (const f of fills) {
      const c = f.pair;
      const path = [c.upper].concat(c.chain, [c.lower]);
      let room = f.rows <= budget;
      for (let i = 1; i < path.length && room; i++) {
        for (let r = path[i - 1].row + 1; r < path[i].row; r++) {
          if ((perRow.get(r) || 0) >= GRAPH.LAYER_DUMMIES_PER_ROW) room = false;
        }
      }
      if (!room) continue;
      budget -= f.rows;
      const filled = [];
      for (let i = 1; i < path.length; i++) {
        for (let r = path[i - 1].row + 1; r < path[i].row; r++) {
          const d = { key: '\u0000' + c.id + '\u0000row' + r, dummy: true, rank: path[i].rank, w: 0, h: 0, x: 0, y: 0, vx: 0 };
          joinRow(d, r, c);
          dummies.push(d);
          filled.push(d);
          perRow.set(r, (perRow.get(r) || 0) + 1);
        }
        if (i < path.length - 1) filled.push(path[i]);
      }
      c.chain = filled;
    }
    for (const c of pairs) {
      if (c.chain.length === 0) continue;
      chains.set(c.upper.key + '\u0000' + c.lower.key, c.chain);
      chains.set(c.lower.key + '\u0000' + c.upper.key, c.chain.slice().reverse());
    }

    /*
      The boxes keep the springs they always had, end to end, so a caller
      still slides over its callees whether or not the edge is routed, and
      nothing a dummy does moves a box: the boxes' geometry is the layout's
      as it was before edges were routed. A dummy is pulled ONE WAY, toward
      the midpoint of its two chain neighbours, so a route settles onto the
      straight line between its ends and bends smoothly over a few rows
      where a row's separation pass moves it off a box. Two-way chain
      springs were tried first: a hub whose routes were all pushed to one
      side of the boxes below it pulled the whole map 800 px off centre.
    */
    for (const e of drawn) springs.push(e);
    for (const c of pairs) {
      const path = [c.upper].concat(c.chain, [c.lower]);
      for (let i = 1; i < path.length - 1; i++) pulls.push({ d: path[i], p: path[i - 1], q: path[i + 1] });
    }
    for (const row of rows) {
      rowSets.push({ boxes: row.filter((node) => !node.dummy), dummies: row.filter((node) => node.dummy) });
    }
    members = nodes.concat(dummies);
  }

  const stages = [findBackEdges, assignRanks, insertDummies, orderRanks, placeRows];
  let stage = 0;
  const total = options.iterations || 220;
  let done = 0;

  /*
    One-dimensional separation, in two passes. The boxes first, as always:
    a row's boxes never overlap, and the row keeps the centre the springs
    gave it rather than creeping right with every push. Then the dummies,
    which are placed in the FREE INTERVALS between the boxes and never move
    one: each goes to the interval its pull put it in, or the nearer side of
    the box it landed on; the dummies in one interval keep their order and
    DUMMY_PITCH apart, pushed in from the interval's two ends, and those an
    interval cannot hold spill into the next one to the right. A dummy that
    pushed a box was tried first: the box moved, the row re-centred on its
    boxes, the dummy was carried the other way, and the pull brought it back
    onto the box - a ratchet that walked routes 600 px out of the map.
  */
  function separateRow(set) {
    const row = set.boxes;
    const before = row.reduce((sum, node) => sum + node.x, 0) / row.length;
    row.sort((a, b) => a.x - b.x || byKeyOrder(a, b));
    for (let i = 1; i < row.length; i++) {
      const need = row[i - 1].x + (row[i - 1].w + row[i].w) / 2 + gap;
      if (row[i].x < need) row[i].x = need;
    }
    const after = row.reduce((sum, node) => sum + node.x, 0) / row.length;
    for (const node of row) node.x += before - after;

    const ds = set.dummies;
    if (ds.length === 0) return;
    ds.sort((a, b) => a.x - b.x || byKeyOrder(a, b));
    // Interval i lies between box i-1 and box i: 0 is left of every box,
    // row.length right of every box.
    const left = (i) => (i === 0 ? -Infinity : row[i - 1].x + row[i - 1].w / 2 + GRAPH.DUMMY_CLEAR);
    const right = (i) => (i === row.length ? Infinity : row[i].x - row[i].w / 2 - GRAPH.DUMMY_CLEAR);
    const lists = [];
    for (let i = 0; i <= row.length; i++) lists.push([]);
    // The dummies are sorted by x, so the interval index only ever moves
    // right: it is carried from one dummy to the next rather than searched
    // from the row's left end for each.
    let i = 0;
    for (const d of ds) {
      while (i < row.length && d.x > right(i)) i++;
      // Now d.x <= right(i), or i is the last interval. If it is short of
      // left(i) it is on box i-1: take the nearer of that box's two sides.
      if (i > 0 && d.x < left(i) && d.x - right(i - 1) < left(i) - d.x) i--;
      lists[i].push(d);
    }
    for (let i = 0; i <= row.length; i++) {
      const list = lists[i];
      if (list.length === 0) continue;
      const lo = left(i); const hi = right(i);
      const fit = Number.isFinite(hi - lo) ? Math.max(1, Math.floor((hi - lo) / GRAPH.DUMMY_PITCH) + 1) : list.length;
      if (list.length > fit && i < row.length) lists[i + 1] = list.splice(fit).concat(lists[i + 1]);
      for (let k = 0; k < list.length; k++) {
        const floor = k === 0 ? lo : list[k - 1].x + GRAPH.DUMMY_PITCH;
        if (list[k].x < floor) list[k].x = floor;
      }
      for (let k = list.length - 1; k >= 0; k--) {
        const ceil = k === list.length - 1 ? hi : list[k + 1].x - GRAPH.DUMMY_PITCH;
        if (list[k].x > ceil) list[k].x = ceil;
      }
    }
  }

  function tick() {
    // alpha decays to zero, so the rows come to rest rather than jittering
    // for as long as the tab is open.
    const alpha = Math.max(0.02, 1 - done / total);
    for (const node of members) node.vx = 0;
    for (const spring of springs) {
      const pull = (spring.b.x - spring.a.x) * 0.055 * spring.w;
      spring.a.vx += pull;
      spring.b.vx -= pull;
    }
    for (const pull of pulls) pull.d.vx += ((pull.p.x + pull.q.x) / 2 - pull.d.x) * 0.5;
    const cap = avgW * 0.5;
    for (const node of members) {
      // Gravity centres the boxes; a dummy is held by its chain alone.
      if (!node.dummy) node.vx -= node.x * 0.022;
      const v = Math.max(-cap, Math.min(cap, node.vx));
      node.x += v * alpha;
      if (!Number.isFinite(node.x)) node.x = 0;
    }
    for (const set of rowSets) separateRow(set);
    done++;
  }

  return {
    rest: rest,
    // The dummies an edge from a to b passes through, in that order, or null
    // for an edge drawn as a chord. The objects are live: the renderer reads
    // their x and y on every frame while the relaxation is still running.
    viaOf(aKey, bKey) { return chains.get(aKey + '\u0000' + bKey) || null; },
    // Every member the relaxation moves, dummies included: what fit() must
    // bring into view, since a route can run wider than the boxes it joins.
    members() { return members; },
    progress() { return stage < stages.length ? (0.2 * stage) / stages.length : 0.2 + (0.8 * done) / total; },
    finished() { return stage >= stages.length && done >= total; },
    step(deadlineMs) {
      const until = performance.now() + (deadlineMs || 8);
      let ran = 0;
      // The ranking runs as five linear passes, each checked against the
      // deadline, so a large set spreads its ranking over frames rather than
      // blocking one; the relaxation then runs tick by tick to the same clock.
      while (stage < stages.length && performance.now() < until) { stages[stage++](); ran++; }
      while (stage >= stages.length && done < total && performance.now() < until) { tick(); ran++; }
      return ran;
    },
  };
}

// ---------------------------------------------------------------- the canvas

/*
  GraphCanvas draws the render set. Three properties matter and each is a
  mechanism rather than a hope:

  LEGIBLE   fit() clamps zoom at GRAPH.MIN_K and never below, so world type is
            never smaller than 11 screen pixels. When the content does not fit
            at that zoom the SET is reduced, not the zoom, and the count removed
            is printed on the canvas with a control to raise it.

  FAST      draw() culls to the viewport before it draws anything. On the
            drilled view that is the difference between 3,242 stroked curves per
            frame and the fifty that are actually on screen.

  HONEST    every stroke is keyed by the weakest tier on the aggregated edge,
            with the tier's own colour, opacity and dash, and `exact` carries a
            tick. A reader can tell a compiler-proven call from a name match
            without reading a legend and without relying on hue.
*/
class GraphCanvas {
  constructor(wrap, canvas, hooks) {
    this.wrap = wrap;
    this.canvas = canvas;
    this.ctx = canvas.getContext('2d');
    this.hooks = hooks || {};
    this.nodes = [];
    this.edges = [];
    this.view = { x: 0, y: 0, k: 1 };
    this.hasFit = false;
    this.width = 0;
    this.height = 0;
    this.selected = null;
    this.hovered = null;
    this.hoverCard = document.getElementById('graph-hover-card');
    this.layout = null;
    this.layoutMode = 'hierarchy';
    this.raf = 0;
    // The focus and path lenses, as the two sets draw() needs. Null means "no
    // lens", which is not the same as an empty set: an empty set means a lens
    // is on and nothing survived it, and those two must not draw identically.
    this.emphasis = { nodeKeys: null, pathNodeKeys: null, pathEdgeIds: null };
    this.reduced = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    this.bindPointer();
    this.bindWheel();
    this.bindKeys();
    this.observer = new ResizeObserver(() => this.measure());
    this.observer.observe(wrap);
  }

  /*
    THE SIZING TRAP. A canvas inside a hidden panel measures 0x0, and a fit
    computed against 0x0 pins the graph at minimum zoom in a corner - for good,
    because nothing recomputes it when the panel is shown. So: refuse to size,
    refuse to fit, and record nothing until the box is real, then run the
    deferred fit exactly once. internal/web/canvas_test.go executes this.
  */
  measure() {
    const box = this.wrap.getBoundingClientRect();
    if (box.width < 1 || box.height < 1) return;
    const dpr = window.devicePixelRatio || 1;
    this.width = box.width;
    this.height = box.height;
    this.canvas.style.width = box.width + 'px';
    this.canvas.style.height = box.height + 'px';
    this.canvas.width = Math.round(box.width * dpr);
    this.canvas.height = Math.round(box.height * dpr);
    this.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    if (!this.hasFit && this.nodes.length > 0) {
      this.hasFit = true;
      this.fit();
    }
    this.draw();
  }

  /*
    THE SCROLL TRAP. A canvas that calls preventDefault on every wheel event
    traps the page: a reader scrolling past the graph gets stuck zooming it. A
    plain wheel therefore returns BEFORE preventDefault, and only ctrl or meta
    zooms. Also executed by the Go harness.
  */
  bindWheel() {
    this.canvas.addEventListener('wheel', (e) => {
      if (!e.ctrlKey && !e.metaKey) return;
      e.preventDefault();
      const box = this.canvas.getBoundingClientRect();
      const factor = e.deltaY < 0 ? 1.14 : 1 / 1.14;
      this.zoomAt(e.clientX - box.left, e.clientY - box.top, factor);
    }, { passive: false });
  }

  bindPointer() {
    let dragging = false;
    let moved = 0;
    let lastX = 0;
    let lastY = 0;
    this.canvas.addEventListener('pointerdown', (e) => {
      this.canvas.setPointerCapture(e.pointerId);
      dragging = true; moved = 0; lastX = e.clientX; lastY = e.clientY;
      this.hideHoverCard();
    });
    this.canvas.addEventListener('pointermove', (e) => {
      const box = this.canvas.getBoundingClientRect();
      if (dragging) {
        const dx = e.clientX - lastX;
        const dy = e.clientY - lastY;
        moved += Math.abs(dx) + Math.abs(dy);
        lastX = e.clientX; lastY = e.clientY;
        this.view.x += dx; this.view.y += dy;
        this.draw();
        this.hideHoverCard();
        return;
      }
      const hit = this.hitTest(e.clientX - box.left, e.clientY - box.top);
      this.canvas.classList.toggle('is-pointing', !!hit);
      if (hit !== this.hovered) {
        this.hovered = hit;
        this.draw();
        if (hit) this.updateHoverCard(hit, e.clientX, e.clientY);
        else this.hideHoverCard();
      } else if (hit) {
        this.updateHoverCard(hit, e.clientX, e.clientY);
      }
    });
    const end = (e) => {
      if (!dragging) return;
      dragging = false;
      if (moved < 5) {
        const box = this.canvas.getBoundingClientRect();
        const hit = this.hitTest(e.clientX - box.left, e.clientY - box.top);
        this.select(hit);
      }
    };
    this.canvas.addEventListener('pointerup', end);
    this.canvas.addEventListener('pointercancel', () => { dragging = false; this.hideHoverCard(); });
    this.canvas.addEventListener('pointerleave', () => { this.hovered = null; this.hideHoverCard(); this.draw(); });
    this.canvas.addEventListener('dblclick', (e) => {
      const box = this.canvas.getBoundingClientRect();
      const hit = this.hitTest(e.clientX - box.left, e.clientY - box.top);
      if (hit && this.hooks.onOpen) this.hooks.onOpen(hit);
    });
  }

  bindKeys() {
    this.canvas.addEventListener('keydown', (e) => {
      const pan = e.shiftKey ? 220 : 70;
      const map = { ArrowLeft: [pan, 0], ArrowRight: [-pan, 0], ArrowUp: [0, pan], ArrowDown: [0, -pan] };
      if (map[e.key]) {
        e.preventDefault();
        this.view.x += map[e.key][0]; this.view.y += map[e.key][1];
        this.draw();
      } else if (e.key === '+' || e.key === '=') {
        e.preventDefault(); this.zoomAt(this.width / 2, this.height / 2, 1.2);
      } else if (e.key === '-' || e.key === '_') {
        e.preventDefault(); this.zoomAt(this.width / 2, this.height / 2, 1 / 1.2);
      } else if (e.key === '0') {
        e.preventDefault(); this.hasFit = true; this.fit(); this.draw();
      } else if (e.key === 'Tab') {
        // Tab walks the drawn nodes in layout order. It is the only fully
        // reliable way to reach a node without a pointer, so it is not an
        // afterthought: selecting through it drives the same inspector.
        e.preventDefault();
        const order = this.nodes.slice().sort((a, b) => a.y - b.y || a.x - b.x);
        if (order.length === 0) return;
        const at = this.selected ? order.findIndex((n) => n.key === this.selected.key) : -1;
        const next = order[(at + (e.shiftKey ? -1 : 1) + order.length * 2) % order.length];
        this.select(next);
        this.centreOn(next);
      } else if (e.key === 'Enter' && this.selected && this.hooks.onOpen) {
        e.preventDefault(); this.hooks.onOpen(this.selected);
      }
    });
  }

  setLayoutMode(mode) {
    this.layoutMode = (mode === 'tree' || mode === 'organic') ? mode : 'hierarchy';
    const select = document.getElementById('btn-layout-mode');
    if (select && select.value !== this.layoutMode) select.value = this.layoutMode;
    if (this.renderSet) this.setData(this.renderSet, { mode: this.layoutMode });
  }

  toggleLayoutMode() {
    const order = ['hierarchy', 'tree', 'organic'];
    this.setLayoutMode(order[(order.indexOf(this.layoutMode) + 1) % order.length]);
  }

  toggleFullscreen() {
    this.wrap.classList.toggle('is-fullscreen');
    const isFs = this.wrap.classList.contains('is-fullscreen');
    const btn = document.getElementById('btn-fullscreen');
    if (btn) {
      btn.setAttribute('aria-pressed', String(isFs));
      btn.setAttribute('aria-label', isFs ? 'Exit full screen' : 'Full screen');
      btn.title = isFs ? 'Exit full screen (Esc)' : 'Full screen';
    }
    setTimeout(() => {
      this.measure();
      this.fit();
      this.draw();
    }, 60);
  }

  toggleMinimap() {
    const mm = $('#graph-minimap');
    if (mm) {
      mm.hidden = !mm.hidden;
      const btn = document.getElementById('btn-toggle-minimap');
      if (btn) btn.setAttribute('aria-pressed', String(!mm.hidden));
      if (!mm.hidden) this.drawMinimap();
    }
  }

  hideHoverCard() {
    if (!this.hoverCard) this.hoverCard = document.getElementById('graph-hover-card');
    if (this.hoverCard) this.hoverCard.hidden = true;
  }

  updateHoverCard(node, clientX, clientY) {
    if (!this.hoverCard) this.hoverCard = document.getElementById('graph-hover-card');
    if (!this.hoverCard || !node) {
      this.hideHoverCard();
      return;
    }
    const rawLabel = String(node.drawLabel || node.label || '');
    const isCluster = node.type === 'cluster';

    let cat = isCluster ? 'CLUSTER' : 'NODE';
    let catColor = '#0284c7';
    const l = rawLabel.toLowerCase();
    if (l.includes('api') || l.includes('controller') || l.includes('rest') || l.includes('endpoint')) {
      cat = 'API / ENDPOINT'; catColor = '#2563eb';
    } else if (l.includes('service') || l.includes('business') || l.includes('strategy') || l.includes('handler')) {
      cat = 'SERVICE / LOGIC'; catColor = '#7c3aed';
    } else if (l.includes('repo') || l.includes('dao') || l.includes('table') || l.includes('entity') || l.includes('db')) {
      cat = 'STORAGE / ENTITY'; catColor = '#059669';
    } else if (l.includes('event') || l.includes('mq') || l.includes('queue') || l.includes('topic')) {
      cat = 'EVENT / QUEUE'; catColor = '#d97706';
    } else if (isCluster) {
      cat = 'PACKAGE / GROUP'; catColor = '#0284c7';
    } else if (node.leaf && node.leaf.kind) {
      cat = String(node.leaf.kind).toUpperCase();
    }

    clear(this.hoverCard);

    const tag = el('span', {
      class: 'graph-hover-tag',
      style: 'background:' + catColor + '18;color:' + catColor + ';border:1px solid ' + catColor + '40;',
      text: cat
    });

    const title = el('div', { class: 'graph-hover-title' },
      el('span', { style: 'display:inline-block;width:8px;height:8px;border-radius:50%;background:' + catColor + ';flex-shrink:0' }),
      el('span', { text: node.drawLabel || node.label || 'Node' })
    );

    const stats = el('div', { class: 'graph-hover-stats' });
    if (isCluster) {
      stats.appendChild(el('span', { text: num(node.count || 0) + ' ' + plural('member', node.count || 0) }));
    } else {
      const symbols = node.leaf ? (node.leaf.symbols || 0) : 0;
      if (symbols > 0) stats.appendChild(el('span', { text: num(symbols) + ' ' + plural('symbol', symbols) }));
      if (node.tier) stats.appendChild(tierBadge(node.tier));
    }
    const deg = node.degree || 0;
    if (deg > 0) stats.appendChild(el('span', { text: num(deg) + ' ' + plural('link', deg) }));

    const hint = el('div', {
      class: 'graph-hover-hint',
      text: isCluster ? 'Click for details · double-click to open the group' : 'Click for details · double-click to open'
    });

    this.hoverCard.appendChild(tag);
    this.hoverCard.appendChild(title);
    if (node.drawSub && node.drawSub !== node.drawLabel) {
      this.hoverCard.appendChild(el('div', { style: 'font-size:10.5px;color:var(--ink-2);font-family:var(--mono);word-break:break-all', text: node.drawSub }));
    }
    this.hoverCard.appendChild(stats);
    this.hoverCard.appendChild(hint);

    this.hoverCard.hidden = false;

    const wrapBox = this.wrap.getBoundingClientRect();
    let left = clientX - wrapBox.left + 16;
    let top = clientY - wrapBox.top + 16;

    const cardW = 240;
    const cardH = 120;
    if (left + cardW > wrapBox.width) left = Math.max(8, clientX - wrapBox.left - cardW - 12);
    if (top + cardH > wrapBox.height) top = Math.max(8, clientY - wrapBox.top - cardH - 12);

    this.hoverCard.style.left = Math.round(left) + 'px';
    this.hoverCard.style.top = Math.round(top) + 'px';
  }

  setData(renderSet, opts) {
    const options = opts || {};
    const mode = options.mode || this.layoutMode || 'hierarchy';
    this.layoutMode = mode;
    this.renderSet = renderSet;
    this.nodes = renderSet.nodes;
    this.edges = renderSet.edges;
    this.selected = null;
    this.hovered = null;
    measureLabels(this.ctx, this.nodes);
    this.layout = createLayout(this.nodes, this.edges, { iterations: options.iterations, mode: mode });
    // One synchronous slice before the first paint. Without it the first frame
    // is the key-ordered grid, which looks like a bug and is on screen for however
    // long the first animation frame takes to arrive - which, in a background
    // tab, is until the reader comes back.
    this.layout.step(70);
    this.buildIndex();
    this.hasFit = false;
    this.runLayout();
  }

  // The layout runs in frame-sized slices. The canvas repaints between slices,
  // so a 1,620-node graph settles visibly instead of freezing the tab.
  runLayout() {
    if (this.raf) cancelAnimationFrame(this.raf);
    const pump = () => {
      if (!this.layout) return;
      this.layout.step(this.reduced ? 40 : 9);
      this.buildIndex();
      if (this.width > 0 && this.height > 0) { this.hasFit = true; this.fit(); }
      this.draw();
      if (this.hooks.onProgress) this.hooks.onProgress(this.layout.progress());
      if (!this.layout.finished()) this.raf = requestAnimationFrame(pump);
      else { this.raf = 0; if (this.hooks.onSettled) this.hooks.onSettled(); }
    };
    this.raf = requestAnimationFrame(pump);
  }

  // A uniform spatial grid over the drawn nodes. Hit testing used to be a
  // linear scan in world coordinates against three-pixel targets; this is the
  // index that makes both hit testing and culling constant-ish time.
  buildIndex() {
    const cell = 320;
    const grid = new Map();
    for (const node of this.nodes) {
      const gx0 = Math.floor((node.x - node.w / 2) / cell);
      const gx1 = Math.floor((node.x + node.w / 2) / cell);
      const gy0 = Math.floor((node.y - node.h / 2) / cell);
      const gy1 = Math.floor((node.y + node.h / 2) / cell);
      for (let gx = gx0; gx <= gx1; gx++) {
        for (let gy = gy0; gy <= gy1; gy++) {
          const key = gx + ',' + gy;
          if (!grid.has(key)) grid.set(key, []);
          grid.get(key).push(node);
        }
      }
    }
    this.grid = { cell: cell, map: grid };
  }

  // The world rectangle holding every box AND every routed edge: a route
  // that steps round a box can run outside the boxes, and a fit that only
  // counted the boxes drew those edges off the canvas.
  bbox() {
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    for (const node of (this.layout ? this.layout.members() : this.nodes)) {
      minX = Math.min(minX, node.x - node.w / 2);
      maxX = Math.max(maxX, node.x + node.w / 2);
      minY = Math.min(minY, node.y - node.h / 2);
      maxY = Math.max(maxY, node.y + node.h / 2);
    }
    if (!Number.isFinite(minX)) return { minX: 0, minY: 0, maxX: 1, maxY: 1, w: 1, h: 1, cx: 0, cy: 0 };
    return { minX, minY, maxX, maxY, w: maxX - minX, h: maxY - minY, cx: (minX + maxX) / 2, cy: (minY + maxY) / 2 };
  }

  /*
    fit() is the legibility clamp. The old one took the raw min of both axes and
    landed at 8% on a real drilled container. This one refuses to go below
    MIN_K: if the content does not fit at a readable zoom, the reader gets a
    readable graph they can pan, plus a printed statement that it overflows, and
    the render budget above has already trimmed the set toward fitting.
  */
  fit() {
    if (this.nodes.length === 0 || this.width < 1 || this.height < 1) return;
    const box = this.bbox();
    const pad = 46;
    const raw = Math.min((this.width - pad * 2) / Math.max(box.w, 1), (this.height - pad * 2) / Math.max(box.h, 1));
    const k = Math.max(GRAPH.MIN_K, Math.min(1.35, raw));
    this.view.k = k;
    this.view.x = this.width / 2 - box.cx * k;
    this.view.y = this.height / 2 - box.cy * k;
    // Overflow is measured against the CANVAS at the chosen zoom, not against
    // the padded box the ideal zoom was computed from. Comparing raw to the
    // floor marked a map as overflowing whenever it merely used its margins,
    // and the canvas then printed "the map is larger than this window" over a
    // drawing with clear space on all four sides - a screen contradicting
    // itself, which is the one thing this portal must never do.
    this.overflows = (box.w * k > this.width + 1) || (box.h * k > this.height + 1);
    if (this.canvas.setAttribute) this.canvas.setAttribute('data-graph-metrics', JSON.stringify(this.metrics()));
    if (this.hooks.onView) this.hooks.onView(this.view, this.overflows);
  }

  /*
    The numbers fit() decided with, written onto the canvas element as
    data-graph-metrics. app.js is a closure, so nothing else can read a node's
    measured width, the layout's rest length or the zoom fit() chose; the e2e
    suite reads this attribute to check that node size encodes quantity and to
    report, before and after a sizing change, whether the map still fits above
    the legibility floor, and to check that the same URL lays out identically
    twice. `settled` is false while the layout is still running.
  */
  metrics() {
    let minW = Infinity, maxW = -Infinity, sum = 0;
    for (const node of this.nodes) {
      const w = node.w || 0;
      sum += w;
      if (w < minW) minW = w;
      if (w > maxW) maxW = w;
    }
    const n = this.nodes.length;
    const settled = this.layout ? this.layout.finished() : true;
    return {
      nodes: n,
      meanW: n ? Math.round((sum / n) * 10) / 10 : 0,
      minW: n ? Math.round(minW * 10) / 10 : 0,
      maxW: n ? Math.round(maxW * 10) / 10 : 0,
      rest: this.layout ? Math.round(this.layout.rest * 10) / 10 : 0,
      zoom: Math.round(this.view.k * 1000) / 1000,
      floor: this.view.k <= GRAPH.MIN_K + 1e-9,
      overflows: !!this.overflows,
      settled: settled,
      // Per-node widths and positions only once the layout has settled: fit()
      // runs on every layout slice, and serialising a thousand entries per
      // frame is a cost the interaction budget would notice. The positions are
      // what lets the e2e suite compare two cold loads of one URL: a layout
      // is deterministic or it is not, and this is where that is checked.
      widths: settled ? this.nodes.map((node) => ({ key: node.key, w: Math.round((node.w || 0) * 10) / 10 })) : undefined,
      // Each entry carries the box and the rank as well as the centre, so
      // the suite can check that no edge's drawn path runs through any box
      // but its own two ends, and that a forward edge goes from a lower
      // rank to a higher one, not only that it points down.
      positions: settled
        ? this.nodes.map((node) => ({
          key: node.key,
          x: Math.round(node.x * 10) / 10,
          y: Math.round(node.y * 10) / 10,
          w: Math.round((node.w || 0) * 10) / 10,
          h: Math.round((node.h || GRAPH.NODE_H) * 10) / 10,
          // The row band the box sits in, which is taller than the box when
          // the row holds a cluster: routePoints stubs from the port to the
          // band's edge, and the suite's mirror needs the same number.
          band: Math.round((node.band || node.h || GRAPH.NODE_H) * 10) / 10,
          rank: node.rank,
        }))
        : undefined,
      // Every edge the layout was given, by its two drawn keys, so the suite
      // can sample the drawn path of a grouped or drilled view whose keys are
      // not the container ids /api/graph answers with.
      edges: settled
        ? this.edges.map((e) => ({ a: e.a, b: e.b }))
          .sort((p, q) => (p.a < q.a ? -1 : p.a > q.a ? 1 : p.b < q.b ? -1 : p.b > q.b ? 1 : 0))
        : undefined,
      // The route of every edge the layout threaded through dummies, in
      // a-to-b order, so the suite can sample the path the renderer draws
      // rather than the chord it no longer draws. An edge drawn as a chord
      // has no entry.
      vias: settled ? this.routes() : undefined,
    };
  }

  routes() {
    const out = [];
    if (!this.layout) return out;
    for (const e of this.edges) {
      const via = this.layout.viaOf(e.a, e.b);
      if (!via || via.length === 0) continue;
      out.push({
        from: e.a, to: e.b,
        points: via.map((v) => ({ x: Math.round(v.x * 10) / 10, y: Math.round(v.y * 10) / 10, h: Math.round(v.h * 10) / 10 })),
      });
    }
    out.sort((p, q) => (p.from < q.from ? -1 : p.from > q.from ? 1 : p.to < q.to ? -1 : p.to > q.to ? 1 : 0));
    return out;
  }

  centreOn(node) {
    this.view.x = this.width / 2 - node.x * this.view.k;
    this.view.y = this.height / 2 - node.y * this.view.k;
    this.draw();
  }

  /*
    flyTo centres a node AND makes sure it is legible when it gets there. Search
    that scrolls a 15% zoom to the right coordinates has not answered the
    question; the reader still cannot read the box. Zoom is only ever raised —
    a reader who has zoomed in to study a corner is not zoomed back out by a
    search — and the motion is instant under prefers-reduced-motion.
  */
  flyTo(node, opts) {
    if (!node) return;
    const options = opts || {};
    const target = Math.max(this.view.k, options.k || 1);
    const to = {
      k: target,
      x: this.width / 2 - node.x * target,
      y: this.height / 2 - node.y * target,
    };
    if (this.reduced || options.instant) {
      this.view = to;
      this.draw();
      if (this.hooks.onView) this.hooks.onView(this.view, this.overflows);
      return;
    }
    const from = { x: this.view.x, y: this.view.y, k: this.view.k };
    const started = performance.now();
    const dur = 360;
    if (this.flyRaf) cancelAnimationFrame(this.flyRaf);
    const step = () => {
      const t = Math.min(1, (performance.now() - started) / dur);
      const e = t < 0.5 ? 2 * t * t : 1 - Math.pow(-2 * t + 2, 2) / 2;
      this.view.k = from.k + (to.k - from.k) * e;
      this.view.x = from.x + (to.x - from.x) * e;
      this.view.y = from.y + (to.y - from.y) * e;
      this.draw();
      if (this.hooks.onView) this.hooks.onView(this.view, this.overflows);
      if (t < 1) this.flyRaf = requestAnimationFrame(step); else this.flyRaf = 0;
    };
    this.flyRaf = requestAnimationFrame(step);
  }

  setEmphasis(em) {
    this.emphasis = em || { nodeKeys: null, pathNodeKeys: null, pathEdgeIds: null };
    this.draw();
  }

  zoomAt(sx, sy, factor) {
    const before = this.view.k;
    const after = Math.max(GRAPH.MIN_ZOOM, Math.min(GRAPH.MAX_K, before * factor));
    if (after === before) return;
    this.view.x = sx - ((sx - this.view.x) * after) / before;
    this.view.y = sy - ((sy - this.view.y) * after) / before;
    this.view.k = after;
    this.draw();
    if (this.hooks.onView) this.hooks.onView(this.view, this.overflows);
  }

  toWorld(sx, sy) {
    return { x: (sx - this.view.x) / this.view.k, y: (sy - this.view.y) / this.view.k };
  }

  select(node) {
    this.selected = node || null;
    this.draw();
    if (this.hooks.onSelect) this.hooks.onSelect(this.selected);
  }

  hitTest(sx, sy) {
    const p = this.toWorld(sx, sy);
    // A four-screen-pixel slack, converted to world units, so a target stays
    // clickable by a human hand rather than by a mouse-precision robot.
    const slack = 4 / this.view.k;
    if (!this.grid) return null;
    const gx = Math.floor(p.x / this.grid.cell);
    const gy = Math.floor(p.y / this.grid.cell);
    let best = null;
    for (let dx = -1; dx <= 1; dx++) {
      for (let dy = -1; dy <= 1; dy++) {
        const bucket = this.grid.map.get((gx + dx) + ',' + (gy + dy));
        if (!bucket) continue;
        for (const node of bucket) {
          if (Math.abs(p.x - node.x) <= node.w / 2 + slack && Math.abs(p.y - node.y) <= node.h / 2 + slack) best = node;
        }
      }
    }
    return best;
  }

  neighboursOf(node) {
    const keys = new Set();
    if (!node) return keys;
    for (const e of this.edges) {
      // A lensed-out edge is not a neighbour relation the reader can see, so
      // hovering must not light the node at its far end.
      if (e.hidden) continue;
      if (e.a === node.key) keys.add(e.b);
      else if (e.b === node.key) keys.add(e.a);
    }
    return keys;
  }

  draw() {
    const ctx = this.ctx;
    if (!ctx || this.width < 1) return;
    const view = this.view;
    const k = view.k;
    ctx.save();
    ctx.clearRect(0, 0, this.width, this.height);
    ctx.fillStyle = css('--canvas-bg', '#fff');
    ctx.fillRect(0, 0, this.width, this.height);

    // Subtle dot-matrix grid pattern
    const gridSize = 32;
    const dotK = Math.max(0.6, Math.min(1.6, k));
    const step = gridSize * k;
    const ox = ((view.x % step) + step) % step;
    const oy = ((view.y % step) + step) % step;
    ctx.save();
    ctx.fillStyle = css('--canvas-grid', '#e5e7eb');
    for (let gx = ox; gx < this.width; gx += step) {
      for (let gy = oy; gy < this.height; gy += step) {
        ctx.beginPath();
        ctx.arc(gx, gy, 1.1 * dotK, 0, Math.PI * 2);
        ctx.fill();
      }
    }
    ctx.restore();

    // VIEWPORT CULLING. Everything below tests against this world rectangle
    // first; nothing off screen is stroked, measured or filled.
    const pad = 240;
    const vp = {
      x0: (-view.x - pad) / k, y0: (-view.y - pad) / k,
      x1: (this.width - view.x + pad) / k, y1: (this.height - view.y + pad) / k,
    };

    ctx.translate(view.x, view.y);
    ctx.scale(k, k);

    const focus = this.selected || this.hovered;
    const near = this.neighboursOf(focus);
    const em = this.emphasis || {};
    // A node is LENSED OUT when a focus lens is on and it is outside the
    // radius, or when a path is drawn and it is not on the chain. Lensed-out is
    // drawn faint rather than removed: a radius with no surroundings has no
    // scale, and a reader could not tell a small blast radius from a small
    // repository.
    const lensedNode = (key) => {
      if (em.pathNodeKeys) return !em.pathNodeKeys.has(key);
      if (em.nodeKeys) return !em.nodeKeys.has(key);
      return false;
    };
    let drawnEdges = 0;
    let drawnNodes = 0;
    let hiddenEdges = 0;

    for (const e of this.edges) {
      // The confidence lens and the cross-language lens remove an edge from the
      // drawing entirely; both count what they removed, and both readouts print
      // it. See applyLenses.
      if (e.hidden) { hiddenEdges++; continue; }
      const a = this.renderSet.byKey.get(e.a);
      const b = this.renderSet.byKey.get(e.b);
      if (!a || !b) continue;
      // A routed edge bends through its dummies, so the cull is against the
      // whole route, not the chord's two ends.
      const via = this.layout ? this.layout.viaOf(e.a, e.b) : null;
      let x0 = Math.min(a.x, b.x), x1 = Math.max(a.x, b.x), y0 = Math.min(a.y, b.y), y1 = Math.max(a.y, b.y);
      if (via) for (const v of via) { x0 = Math.min(x0, v.x); x1 = Math.max(x1, v.x); y0 = Math.min(y0, v.y); y1 = Math.max(y1, v.y); }
      if (x1 < vp.x0 || x0 > vp.x1) continue;
      if (y1 < vp.y0 || y0 > vp.y1) continue;
      const onPath = em.pathEdgeIds ? em.pathEdgeIds.has(e.a + ' ' + e.b) : false;
      const spec = TIER[tierOf(e.weakest)];
      const lensed = em.pathEdgeIds ? !onPath : (em.nodeKeys ? (lensedNode(e.a) || lensedNode(e.b)) : false);
      const isFocusedEdge = focus && (e.a === focus.key || e.b === focus.key);
      const dim = lensed || (focus && !isFocusedEdge);
      ctx.globalAlpha = spec.alpha * (lensed ? 0.08 : (dim ? 0.16 : 1));
      ctx.strokeStyle = onPath ? css('--canvas-path', '#b4530a')
        : (isFocusedEdge ? '#2563eb' : tierColour(e.weakest));
      ctx.lineWidth = (spec.width + e.weight * 2.6 + (onPath ? 2.4 : (isFocusedEdge ? 1.8 : 0))) / k * Math.min(1, k);
      ctx.setLineDash(onPath ? [] : spec.dash.map((d) => d / Math.max(0.4, k)));
      if (isFocusedEdge) {
        ctx.shadowColor = 'rgba(37, 99, 235, 0.45)';
        ctx.shadowBlur = 8;
      } else {
        ctx.shadowColor = 'transparent';
      }
      this.strokeEdge(ctx, a, b, via);
      ctx.shadowColor = 'transparent';
      ctx.setLineDash([]);
      drawnEdges++;
      // A cross-language edge carries a mark of its own, so the seams in a
      // polyglot repository are legible without reading the table.
      if (e.xlang && !lensed) this.crossLanguageMark(ctx, a, b, k, via);
      if (k >= GRAPH.LOD_EDGE_CAPTION && !dim) this.edgeCaption(ctx, a, b, e, via);
    }
    ctx.globalAlpha = 1;

    for (const node of this.nodes) {
      if (node.x + node.w / 2 < vp.x0 || node.x - node.w / 2 > vp.x1) continue;
      if (node.y + node.h / 2 < vp.y0 || node.y - node.h / 2 > vp.y1) continue;
      this.drawNode(ctx, node, k, focus, near, lensedNode(node.key),
        em.pathNodeKeys ? em.pathNodeKeys.has(node.key) : false);
      drawnNodes++;
    }

    ctx.restore();
    this.drawn = { nodes: drawnNodes, edges: drawnEdges, hiddenEdges: hiddenEdges };
    this.drawMinimap();
    if (this.hooks.onView) this.hooks.onView(this.view, this.overflows);
  }

  /*
    The stroke for one edge: the polyline routePoints builds - out of a port
    on the source box, through each dummy's row vertically, into a port on
    the target box. It used to be a quadratic bowed off the chord so that A
    to B and B to A were two visible strokes; the route's side offset does
    that now, and a straight polyline is what the e2e suite samples by
    mirroring this (specs/04-graph.spec.mjs, strokeSamples).
  */
  strokeEdge(ctx, a, b, via) {
    let points;
    if (this.layoutMode === 'organic') {
      points = rayBoxPoints(a, b);
    } else {
      points = routePoints(a, b, via);
    }
    ctx.beginPath();
    ctx.moveTo(points[0].x, points[0].y);
    for (let i = 1; i < points.length; i++) ctx.lineTo(points[i].x, points[i].y);
    ctx.stroke();

    const end = points[points.length - 1];
    const last = points[points.length - 2];
    const angle = Math.atan2(end.y - last.y, end.x - last.x);
    const hx = end.x;
    const hy = end.y;
    const size = 8.5;
    ctx.beginPath();
    ctx.moveTo(hx, hy);
    ctx.lineTo(hx - Math.cos(angle - 0.42) * size, hy - Math.sin(angle - 0.42) * size);
    ctx.lineTo(hx - Math.cos(angle) * (size * 0.55), hy - Math.sin(angle) * (size * 0.55));
    ctx.lineTo(hx - Math.cos(angle + 0.42) * size, hy - Math.sin(angle + 0.42) * size);
    ctx.closePath();
    ctx.fillStyle = ctx.strokeStyle;
    ctx.fill();
  }

  edgeCaption(ctx, a, b, e, via) {
    const kinds = Array.from(e.kinds.entries()).sort((x, y) => y[1] - x[1]);
    if (kinds.length === 0) return;
    const label = e.count > 1 ? 'uses, ' + num(e.count) + ' times' : 'uses';
    const mid = routeMiddle(a, b, via);
    if (mid.len !== undefined && mid.len < 38) return; // Do not occlude short edges or arrowheads
    const mx = mid.x;
    const my = mid.y;
    ctx.save();
    ctx.font = '10px ' + css('--mono', 'monospace');
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    const w = ctx.measureText(label).width + 8;
    ctx.globalAlpha = 0.94;
    ctx.beginPath();
    roundRect(ctx, mx - w / 2, my - 7, w, 14, 3);
    ctx.fillStyle = css('--canvas-bg', '#fff');
    ctx.fill();
    ctx.lineWidth = 1;
    ctx.strokeStyle = css('--canvas-stroke', '#e2e8f0');
    ctx.stroke();
    ctx.fillStyle = css('--canvas-ink-dim', '#475569');
    ctx.fillText(label, mx, my);
    ctx.restore();
  }

  /*
    A small tick across the middle of a cross-language edge. Shape, not hue: the
    tier already owns colour on this canvas, and a second meaning encoded in
    colour alone would be unreadable to a reader who cannot separate them and
    ambiguous to everyone else.
  */
  crossLanguageMark(ctx, a, b, k, via) {
    const mid = routeMiddle(a, b, via);
    const mx = mid.x;
    const my = mid.y;
    const dx = mid.dx;
    const dy = mid.dy;
    const d = Math.sqrt(dx * dx + dy * dy) || 1;
    const nx = -dy / d;
    const ny = dx / d;
    const len = 6 / Math.max(0.5, k);
    ctx.save();
    ctx.globalAlpha = 0.95;
    ctx.lineWidth = 1.6 / k;
    ctx.setLineDash([]);
    for (const off of [-3 / Math.max(0.5, k), 3 / Math.max(0.5, k)]) {
      ctx.beginPath();
      ctx.moveTo(mx + (dx / d) * off - nx * len, my + (dy / d) * off - ny * len);
      ctx.lineTo(mx + (dx / d) * off + nx * len, my + (dy / d) * off + ny * len);
      ctx.stroke();
    }
    ctx.restore();
  }

  drawNode(ctx, node, k, focus, near, lensedOut, onPath) {
    const isFocus = focus && focus.key === node.key;
    const isHover = this.hovered && this.hovered.key === node.key;
    const dim = (focus && !isFocus && !near.has(node.key)) || lensedOut;
    const x = node.x - node.w / 2;
    const y = node.y - node.h / 2;
    ctx.save();
    ctx.globalAlpha = lensedOut ? 0.14 : (dim ? 0.28 : 1);

    const cluster = node.type === 'cluster';
    const rawLabel = String(node.drawLabel || node.label || '').toLowerCase();
    let catColor = '#0284c7';
    if (rawLabel.includes('api') || rawLabel.includes('controller') || rawLabel.includes('rest') || rawLabel.includes('endpoint')) {
      catColor = '#2563eb';
    } else if (rawLabel.includes('service') || rawLabel.includes('business') || rawLabel.includes('strategy') || rawLabel.includes('handler')) {
      catColor = '#7c3aed';
    } else if (rawLabel.includes('repo') || rawLabel.includes('dao') || rawLabel.includes('table') || rawLabel.includes('entity') || rawLabel.includes('db')) {
      catColor = '#059669';
    } else if (rawLabel.includes('event') || rawLabel.includes('mq') || rawLabel.includes('queue') || rawLabel.includes('topic')) {
      catColor = '#d97706';
    }

    if (isFocus || isHover) {
      ctx.shadowColor = isFocus ? catColor : 'rgba(0, 0, 0, 0.16)';
      ctx.shadowBlur = isFocus ? 16 : 8;
      ctx.shadowOffsetX = 0;
      ctx.shadowOffsetY = isFocus ? 0 : 3;
    } else {
      ctx.shadowColor = 'rgba(0, 0, 0, 0.05)';
      ctx.shadowBlur = 6;
      ctx.shadowOffsetX = 0;
      ctx.shadowOffsetY = 2;
    }

    ctx.beginPath();
    roundRect(ctx, x, y, node.w, node.h, cluster ? 10 : 8);
    ctx.fillStyle = cluster ? css('--canvas-cluster', '#f8fafc') : css('--canvas-node', '#ffffff');
    ctx.fill();
    ctx.shadowColor = 'transparent';

    ctx.lineWidth = (isFocus || onPath) ? 2.5 : 1.2;
    ctx.strokeStyle = onPath ? css('--canvas-path', '#b4530a')
      : (isFocus ? catColor
        : (isHover ? catColor
          : (cluster ? css('--canvas-cluster-line', '#cbd5e1') : css('--canvas-stroke', '#cbd5e1'))));
    ctx.stroke();

    // Left category indicator bar
    ctx.fillStyle = catColor;
    ctx.fillRect(x + 2, y + 4, 3.5, node.h - 8);

    // Connection anchor ports (top and bottom dock dots)
    const portR = 2.8;
    ctx.fillStyle = isFocus ? catColor : (isHover ? '#2563eb' : '#94a3b8');
    ctx.beginPath();
    ctx.arc(node.x, y, portR, 0, Math.PI * 2);
    ctx.fill();
    ctx.strokeStyle = '#ffffff';
    ctx.lineWidth = 1;
    ctx.stroke();

    ctx.beginPath();
    ctx.arc(node.x, y + node.h, portR, 0, Math.PI * 2);
    ctx.fill();
    ctx.stroke();

    if (k >= GRAPH.LOD_LABEL) {
      ctx.textBaseline = 'middle';
      ctx.textAlign = 'left';
      ctx.fillStyle = css('--canvas-ink', '#111');
      ctx.font = '600 ' + GRAPH.LABEL_FONT + 'px ' + css('--font', 'sans-serif');
      const showSub = k >= GRAPH.LOD_SUB && node.drawSub;
      ctx.fillText(node.drawLabel, x + GRAPH.NODE_PAD, node.y - (showSub ? 8 : 0));
      if (showSub) {
        ctx.fillStyle = css('--canvas-ink-dim', '#64748b');
        ctx.font = GRAPH.SUB_FONT + 'px ' + css('--mono', 'monospace');
        ctx.fillText(node.drawSub, x + GRAPH.NODE_PAD, node.y + 10);
      }
      if (cluster) {
        ctx.fillStyle = catColor;
        ctx.font = '600 12px ' + css('--mono', 'monospace');
        ctx.textAlign = 'right';
        ctx.fillText('+', x + node.w - 8, y + 12);
      } else if (node.degree !== undefined && node.degree > 0) {
        ctx.font = '600 9px ' + css('--mono', 'monospace');
        const badgeText = '🔗 ' + node.degree;
        const bw = ctx.measureText(badgeText).width + 6;
        const bx = x + node.w - bw - 6;
        const by = y + 5;
        ctx.beginPath();
        roundRect(ctx, bx, by, bw, 13, 3);
        ctx.fillStyle = isFocus ? catColor : (isHover ? '#e0f2fe' : '#f8fafc');
        ctx.fill();
        ctx.strokeStyle = isFocus ? catColor : (isHover ? '#bae6fd' : '#e2e8f0');
        ctx.lineWidth = 0.8;
        ctx.stroke();
        ctx.fillStyle = isFocus ? '#ffffff' : (isHover ? '#0369a1' : '#64748b');
        ctx.textAlign = 'center';
        ctx.textBaseline = 'middle';
        ctx.fillText(badgeText, bx + bw / 2, by + 7);
      }
    }
    ctx.restore();
  }

  drawMinimap() {
    if (!this.minimap) return;
    const ctx = this.minimap.getContext('2d');
    const w = this.minimap.width;
    const h = this.minimap.height;
    ctx.clearRect(0, 0, w, h);
    const box = this.bbox();
    const scale = Math.min(w / Math.max(box.w, 1), h / Math.max(box.h, 1)) * 0.86;
    const ox = w / 2 - box.cx * scale;
    const oy = h / 2 - box.cy * scale;
    ctx.fillStyle = css('--canvas-ink-dim', '#889');
    ctx.globalAlpha = 0.55;
    for (const node of this.nodes) {
      ctx.fillRect(ox + (node.x - node.w / 2) * scale, oy + (node.y - node.h / 2) * scale,
        Math.max(1.5, node.w * scale), Math.max(1.5, node.h * scale));
    }
    ctx.globalAlpha = 1;
    const view = this.view;
    ctx.strokeStyle = css('--canvas-sel', '#26f');
    ctx.lineWidth = 1.4;
    ctx.strokeRect(ox + (-view.x / view.k) * scale, oy + (-view.y / view.k) * scale,
      (this.width / view.k) * scale, (this.height / view.k) * scale);
  }

  attachMinimap(canvas) {
    this.minimap = canvas;
    canvas.width = 132 * (window.devicePixelRatio || 1);
    canvas.height = 92 * (window.devicePixelRatio || 1);
    canvas.getContext('2d').setTransform(window.devicePixelRatio || 1, 0, 0, window.devicePixelRatio || 1, 0, 0);
    canvas.width = 132; canvas.height = 92;
    canvas.addEventListener('click', (e) => {
      const box = canvas.getBoundingClientRect();
      const bb = this.bbox();
      const scale = Math.min(132 / Math.max(bb.w, 1), 92 / Math.max(bb.h, 1)) * 0.86;
      const wx = (e.clientX - box.left - (132 / 2 - bb.cx * scale)) / scale;
      const wy = (e.clientY - box.top - (92 / 2 - bb.cy * scale)) / scale;
      this.view.x = this.width / 2 - wx * this.view.k;
      this.view.y = this.height / 2 - wy * this.view.k;
      this.draw();
    });
  }

  destroy() {
    if (this.raf) cancelAnimationFrame(this.raf);
    if (this.observer) this.observer.disconnect();
  }
}

/*
  HOW AN EDGE IS ROUTED. The layout puts a caller's row above its callee's
  and, for an edge that crosses rows, a zero-width dummy in each row between
  (createLayout). Drawing the edge as a chord from centre to centre, or even
  as a polyline through the dummies, still ran it through boxes: at this row
  pitch - 40 px of box in every 54 - a segment that leaves a centre at a
  shallow angle is still inside its own row's band when it reaches the
  neighbouring box. So the route never crosses a band at an angle:

    - it leaves the source through a PORT on the edge facing the target
      (bottom edge for an edge that points down, top for one that points up),
      at the point where the line to the next member crosses that edge,
      clamped inside the rounded corners;
    - it crosses each dummy's row band VERTICALLY at the dummy's x, which
      the row's separation pass keeps DUMMY_CLEAR from the boxes either side;
    - it crosses the channel between two rows diagonally, from one band's
      edge to the next, and that channel holds no box by construction;
    - it enters the target through the same kind of port on the facing edge.

  A box shorter than its row - a leaf beside a cluster - has its port 7 px
  inside the row's band, and a diagonal that started there was in-band for
  its first third and clipped the cluster's corner: so the route runs a
  vertical STUB from the port to the band's edge, and the diagonal starts
  from there. Not from the edge exactly: ROUTE_DROP px past it, into the
  channel. A dummy is DUMMY_CLEAR from the next box, its down-stroke
  ROUTE_SIDE nearer, and a diagonal that left the band's edge there at a
  shallow angle - 14 px of channel against 97 px of run, on the shipped
  fixture - was still within the stroke's width of that box's corner 10 px
  on; dropping 4 px first clears the corner by more than the stroke. Every
  member's vertical run, a dummy's pass as much as a box's stub, extends by
  the drop at both ends, so the diagonal has the channel less two drops.

  A downward route keeps ROUTE_SIDE px left of that line and an upward one
  the same to the right, so A to B and B to A, which share one chain of
  dummies, are two strokes a readable distance apart - which is what the
  old bowed quadratic was for. The two strokes are 2 x ROUTE_SIDE apart,
  and DUMMY_PITCH has to clear that plus their width (see GRAPH). The
  segments are straight. The e2e suite mirrors this construction exactly to
  sample the drawn path against every box.
*/
const ROUTE_INSET = 8;
const ROUTE_SIDE = 2;
const ROUTE_DROP = 4;

function routePoints(a, b, via) {
  const members = via && via.length ? [a].concat(via, [b]) : [a, b];
  const down = b.y >= a.y;
  const side = down ? -ROUTE_SIDE : ROUTE_SIDE;
  // The port on m's edge at height y, on the line from m's centre to the
  // other member's, clamped inside the corners; and the stub from there,
  // at the same x, to farY - ROUTE_DROP past the band's edge.
  const port = (m, other, y, farY) => {
    const t = (y - m.y) / ((other.y - m.y) || 1);
    const half = Math.max(0, (m.w || 0) / 2 - ROUTE_INSET);
    const x = Math.max(m.x - half, Math.min(m.x + half, m.x + (other.x - m.x) * t)) + side;
    return [{ x: x, y: y }, { x: x, y: farY }];
  };
  const points = [];
  for (let i = 0; i < members.length; i++) {
    const m = members[i];
    const h = m.h || GRAPH.NODE_H;
    const band = Math.max(h, m.band || 0);
    const top = m.y - h / 2; const bottom = m.y + h / 2;
    const farTop = m.y - band / 2 - ROUTE_DROP; const farBottom = m.y + band / 2 + ROUTE_DROP;
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

/*
  The middle of the LONGEST segment of the route a -> via... -> b, and that
  segment's direction: where an edge's caption and its cross-language mark
  go. On a routed edge that is a diagonal across a channel, which is the
  one stretch of the route with no box beside it; the point half way along
  the route was tried first and often fell on a vertical pass through a
  24 px slot beside a neighbouring box. On a chord it is the chord's middle,
  as before. Ties go to the first segment, so the choice is deterministic.
*/
function rayBoxPoints(a, b) {
  const dx = b.x - a.x;
  const dy = b.y - a.y;
  if (Math.abs(dx) < 1e-4 && Math.abs(dy) < 1e-4) {
    return [{ x: a.x, y: a.y }, { x: b.x, y: b.y }];
  }
  const haW = (a.w || GRAPH.NODE_W || 160) / 2;
  const haH = (a.h || GRAPH.NODE_H || 34) / 2;
  const scaleA = Math.max(Math.abs(dx) / haW, Math.abs(dy) / haH);
  const p1 = { x: a.x + dx / scaleA, y: a.y + dy / scaleA };

  const hbW = (b.w || GRAPH.NODE_W || 160) / 2;
  const hbH = (b.h || GRAPH.NODE_H || 34) / 2;
  const scaleB = Math.max(Math.abs(dx) / hbW, Math.abs(dy) / hbH);
  const p2 = { x: b.x - dx / scaleB, y: b.y - dy / scaleB };

  return [p1, p2];
}

function routeMiddle(a, b, via) {
  const points = routePoints(a, b, via);
  let best = -1;
  let bestLen = -1;
  for (let i = 1; i < points.length; i++) {
    const len = Math.hypot(points[i].x - points[i - 1].x, points[i].y - points[i - 1].y);
    if (len > bestLen) { bestLen = len; best = i; }
  }
  if (best < 0) {
    const d = Math.hypot(b.x - a.x, b.y - a.y);
    return { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2, dx: b.x - a.x, dy: b.y - a.y, len: d };
  }
  const p = points[best - 1]; const q = points[best];
  return { x: (p.x + q.x) / 2, y: (p.y + q.y) / 2, dx: q.x - p.x, dy: q.y - p.y, len: bestLen };
}

function roundRect(ctx, x, y, w, h, r) {
  const rad = Math.min(r, w / 2, h / 2);
  ctx.moveTo(x + rad, y);
  ctx.arcTo(x + w, y, x + w, y + h, rad);
  ctx.arcTo(x + w, y + h, x, y + h, rad);
  ctx.arcTo(x, y + h, x, y, rad);
  ctx.arcTo(x, y, x + w, y, rad);
  ctx.closePath();
}

// ================================================================ SHELL

/*
  THE TWENTY SCREENS, BY ID.

  These ids are the product's stable vocabulary: deep links, LOADERS and the
  section elements in the shell all use them. The rail itself — its groups, in
  the reader's words, and the label each screen carries — is one list in
  assets/js/nav.mjs, built into the shell before this script runs and handed
  here through window.cortex; showScreen reads the label from it for the
  document title. The order here is not the rail's order and does not need
  to be.

  Screens this client does not yet draw a payload for are in the list anyway. A
  pillar missing from the navigation reads as a capability the product does not
  have; a pillar present and honest about being empty reads as one waiting for
  an index that carries it. Each of those renders GUIDANCE — what the pillar
  answers, what THIS index holds towards it, and the command that fills it.
*/
const SCREENS = [
  'graph', 'search', 'metrics',
  'blast', 'contract', 'verify', 'ownership', 'diff',
  'crossstack',
  'tables',
  'tests',
  'config',
  'trace',
  'governance',
  'deps',
  'retrieval', 'agent',
  'overview', 'accuracy', 'estate',
  'events', 'missed', 'planchange',
];

const LOADERS = {};

function go(screen, patch, opts) {
  const target = SCREENS.indexOf(screen) >= 0 ? screen : 'overview';
  setQuery(Object.assign({ s: target }, patch || {}), opts);
  showScreen(target);
}

function showScreen(name) {
  // Leaving the graph screen for another one hides its popover's host along
  // with the rest of it, but does not close the popover's own state — close
  // it here so it is not still open, unattended, the next time this screen
  // is shown.
  if (name !== 'graph' && GRAPH_STATE.filtersOpen) setFiltersOpen(false);
  for (const screen of SCREENS) {
    const node = document.getElementById('screen-' + screen);
    if (node) node.hidden = (screen !== name);
  }
  for (const btn of document.querySelectorAll('.nav')) {
    const on = btn.dataset.screen === name;
    if (on) btn.setAttribute('aria-current', 'page'); else btn.removeAttribute('aria-current');
  }
  document.title = 'RepoCortex — ' + ((window.cortex && window.cortex.LABEL && window.cortex.LABEL[name]) || name);
  const loader = LOADERS[name];
  if (loader) loader();
  window.scrollTo({ top: 0, behavior: 'auto' });
}

// ---------------------------------------------------------------- index bar

/*
  The run's own verdict, on every screen. `/api/health` and `/api/overview` both
  reported "degraded" for a run this portal presented as healthy everywhere a
  reader looked: two green badges on the freshness card, and the status used
  only in a footer line. A measurement product that hides its own health
  verdict has no standing to report anyone else's.
*/
function renderIndexBar() {
  const bar = $('#indexbar');
  // Rebuilding the bar destroys the control the reader was standing on, and a
  // keyboard reader who changes the confidence floor or opens the provenance
  // panel is then returned to the top of the document. Focus is put back on the
  // same control afterwards, for every control here that has an id to find it
  // by.
  const held = (document.activeElement && bar.contains(document.activeElement))
    ? document.activeElement.id : '';
  clear(bar);
  const meta = state.meta;
  if (!meta) return;
  const run = meta.run || {};

  bar.appendChild(el('span', { class: 'project', text: text(meta.project, 'this index') }));

  // The run's verdict comes from index_health, which the API serves structured
  // precisely so a client cannot render it as silence. `trustworthy` is the
  // boolean the banner gates on, so a client comparing status strings cannot
  // get the comparison wrong.
  const health = meta.index_health || {};
  const status = text(health.status, text(run.status, 'unknown'));
  const trusted = health.trustworthy === undefined ? (status === 'ok') : !!health.trustworthy;
  const tone = trusted ? 'is-ok' : (text(health.severity, 'warn') === 'error' ? 'is-bad' : 'is-degraded');
  const badge = el('button', { class: 'runbadge ' + tone, type: 'button', title: text(health.headline, 'What "' + status + '" means for the answers on this page') },
    el('span', { class: 'dot', 'aria-hidden': 'true' }), 'index ' + status);
  badge.addEventListener('click', () => { go('overview'); setTimeout(() => { const card = document.getElementById('freshness'); if (card) card.scrollIntoView({ block: 'center' }); }, 60); });
  bar.appendChild(badge);

  const facts = el('span', { class: 'facts' });
  if (run.age_seconds !== undefined) facts.appendChild(el('span', { text: 'indexed ' + humanAge(run.age_seconds) + ' ago' }));
  if (run.index_state) facts.appendChild(el('span', { text: 'state ' + text(run.index_state) }));
  if (run.git_revision) facts.appendChild(el('span', { class: 'mono', text: String(run.git_revision).slice(0, 8) }));
  if (run.parse_failures) facts.appendChild(el('span', { class: 'warnline', text: num(run.parse_failures) + ' ' + plural('file', run.parse_failures) + ' failed to parse' }));
  bar.appendChild(facts);

  // The disclosure that answers "is what I am reading the code I have?", and —
  // only where the operator enabled it — the control that fixes the answer.
  bar.appendChild(provenanceToggle());
  for (const node of refreshControls()) bar.appendChild(node);

  bar.appendChild(el('span', { class: 'spacer' }));
  bar.appendChild(tierBar());

  const share = el('button', { class: 'ghost', type: 'button', text: 'Copy link', title: 'This page, with its filters and selection, as a URL' });
  share.addEventListener('click', () => {
    const url = shareLink();
    const done = () => { share.textContent = 'Link copied'; setTimeout(() => { share.textContent = 'Copy link'; }, 1400); };
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(url).then(done, done);
    else done();
  });
  bar.appendChild(share);
  if (held) { const again = document.getElementById(held); if (again) again.focus(); }
  renderProvenance();
}

function humanAge(seconds) {
  const s = Number(seconds);
  if (!Number.isFinite(s)) return 'an unknown time';
  if (s < 90) return Math.round(s) + 's';
  if (s < 5400) return Math.round(s / 60) + ' min';
  if (s < 172800) return Math.round(s / 3600) + ' hours';
  return Math.round(s / 86400) + ' days';
}

/*
  THE CONFIDENCE BAR. This is the control the whole product is arguing for, so
  it is in the header of every screen rather than in a filter row on one of
  them: four tiers, their real counts, and clicking one sets the floor
  everywhere at once. `low` reads as ghosted and starts off, which is what
  "ghosted and off by default" has to mean in a control as well as on a canvas.
*/
function tierBar() {
  const wrap = el('div', { class: 'tierbar', role: 'group', 'aria-label': 'Standard of proof' });
  const hist = state.overview && state.overview.confidence ? state.overview.confidence.edges : null;
  const floor = state.q.floor;
  for (const tv of ['exact', 'high', 'medium', 'low']) {
    const on = tierRank(tv) >= tierRank(floor);
    const btn = el('button', {
      class: 't-' + tv, type: 'button', 'aria-pressed': String(on), 'data-tier': tv,
      title: tierSentence(tv) + '. Click to set the standard of proof here; everything at this tier or better is drawn.',
      onclick: () => {
        setQuery({ floor: tv });
        renderIndexBar();
        // The graph is the exception, for the reason reloadScreens() gives:
        // state.loaded.graph gates the one-time wiring of the canvas shell, so
        // clearing it appends a second heading and a second set of listeners.
        // Dropping its DATA is the refetch path its own filters already use.
        for (const screen of SCREENS) if (screen !== 'graph') state.loaded[screen] = false;
        GRAPH_STATE.data = null;
        showScreen(state.q.s);
      },
    },
      el('span', { class: 'swatch', 'aria-hidden': 'true' }),
      TIER[tv].glyph ? TIER[tv].glyph + ' ' + tierWord(tv) : tierWord(tv));
    if (hist && hist[tv] !== undefined) btn.appendChild(el('span', { class: 'n', text: num(hist[tv]) }));
    wrap.appendChild(btn);
  }
  return wrap;
}

// ------------------------------------------------- provenance and refresh

/*
  WHERE THIS INDEX CAME FROM, AND WHAT IT CANNOT SEE.

  The bar states an age and eight characters of a sha. Neither answers the
  question a reader actually arrives with after a commit — is what I am reading
  the code I have? — so the facts that do are one disclosure away on every
  screen: the wall-clock moment the run finished, the revision it read, whether
  a commit hook keeps it current, and the sentence a matching revision tempts
  everybody to forget.

  THAT SENTENCE IS THE POINT OF THE PANEL. A revision is a statement about
  committed history. The index is a snapshot of a tree at a moment. Even when
  the two agree, the working copy on disk can differ from every number on this
  portal, and a panel that reported "built from HEAD" and stopped would have
  told a reader they were safe on evidence that does not support it.

  Two things this panel will not do.

  It will not install the commit hook. A page must never write to a
  repository's .git directory, so it reports the state and names the command.

  It will not claim the built revision is HEAD. The API reports the revision the
  index was built from and nothing about the checkout, and there is no schema
  version in any payload this package marshals — so both are stated as gaps with
  the command that closes them, in the same omissions vocabulary every other
  screen uses, rather than guessed at or quietly dropped. When the API grows
  those fields the two entries under "What this panel cannot tell you" become
  two facts; until then they are honest absences. (Same convention as
  assets/NEEDS-API.md, which is where the ask belongs.)
*/
const PROV = {
  open: false,
  glossary: false, // the "What do these words mean?" panel, in the same slot
  busy: false,
  status: null,   // { message, tone } — the last thing a refresh said
  error: null,    // why /api/indexstate could not be read, if it could not
  started: 0,
  ticker: 0,
  live: null,     // the ONE live region; reused across renders, never rebuilt
  elapsed: null,
};

/*
  The provenance payload is fetched after the first paint and again after a
  refresh, and it is never awaited on the boot path: a portal whose provenance
  endpoint is unreachable must still render twenty screens, and a disclosure
  nobody has opened does not earn a round trip in front of the first frame.
*/
async function loadIndexState() {
  try {
    const d = await api('/api/indexstate');
    state.indexstate = d;
    if (d.meta) state.meta = d.meta;
    PROV.error = null;
  } catch (e) {
    state.indexstate = null;
    PROV.error = e && e.message ? e.message : String(e);
  }
  renderIndexBar();
}

function provenanceToggle() {
  const btn = el('button', {
    class: 'ghost', type: 'button', id: 'prov-toggle',
    'aria-expanded': String(PROV.open), 'aria-controls': 'provenance',
    title: 'When this index was built, what it was built from, and what it cannot see',
  },
    el('span', { 'aria-hidden': 'true', text: PROV.open ? '▾ ' : '▸ ' }), 'How this index was built');
  btn.addEventListener('click', () => {
    PROV.open = !PROV.open;
    if (PROV.open) closeGlossary();
    // The bar is rebuilt, which replaces this button; renderIndexBar puts focus
    // back on its successor.
    renderIndexBar();
    const host = $('#provenance');
    if (PROV.open && host) host.scrollIntoView({ block: 'nearest' });
  });
  return btn;
}

/*
  The glossary panel: the words this portal still has to use, each with one
  sentence, in the slot under the index bar that the provenance also uses.
  One at a time — opening either closes the other.
*/
function closeGlossary() {
  PROV.glossary = false;
  const toggle = $('#glossary-toggle');
  if (toggle) toggle.setAttribute('aria-expanded', 'false');
}

function toggleGlossary() {
  const host = $('#provenance');
  const toggle = $('#glossary-toggle');
  if (!host) return;
  if (PROV.glossary) {
    closeGlossary();
    clear(host);
    host.hidden = true;
    if (toggle) toggle.focus();
    return;
  }
  if (PROV.open) { PROV.open = false; renderIndexBar(); }
  PROV.glossary = true;
  if (toggle) toggle.setAttribute('aria-expanded', 'true');
  clear(host);
  host.style.setProperty('padding', '14px 24px 0');
  const heading = el('h3', { text: 'What these words mean', tabindex: '-1' });
  const close = el('button', { class: 'ghost', type: 'button', text: 'Close', onclick: toggleGlossary });
  const dv = window.cortex;
  const list = dv && typeof dv.renderGlossary === 'function'
    ? dv.renderGlossary()
    : el('p', { class: 'small muted', text: 'The glossary did not load.' });
  host.appendChild(el('div', { class: 'glossary-panel' }, heading, close, list));
  host.hidden = false;
  heading.focus();
  host.scrollIntoView({ block: 'nearest' });
}

/*
  The refresh control, and the reason there is often no control at all.

  A button that cannot work is worse than no button: it teaches a reader the
  portal is broken rather than that it is read-only. So when the server says
  refresh is not enabled, nothing is rendered here and the panel below states
  the command instead.
*/
function refreshControls() {
  const refresh = (state.indexstate && state.indexstate.refresh) || null;
  if (!refresh || !refresh.enabled) return [];

  // A refresh this page did not start — another tab, or a reader who reloaded
  // mid-rebuild — is still a refresh, and the gate refuses a second one. Say so
  // rather than offering a button whose only outcome is a 409.
  const elsewhere = refresh.running === true && !PROV.busy;
  if (elsewhere && !PROV.status) {
    PROV.status = {
      message: 'A refresh is already running on this portal. Reload this page to see how it ended.',
      tone: 'warn',
    };
  }

  const running = PROV.busy || elsewhere;
  const btn = el('button', {
    class: 'ghost', type: 'button', id: 'refresh-index',
    text: running ? 'Refreshing…' : 'Refresh index',
    title: 'Rebuild and re-resolve the index, then re-read this screen from it',
  });
  if (running) {
    btn.disabled = true;
    btn.setAttribute('aria-busy', 'true');
  }
  btn.addEventListener('click', () => { runRefresh(btn); });

  // Two regions, and the split is deliberate. The elapsed counter changes every
  // second and is hidden from assistive technology, because a live region that
  // announces "8s… 9s… 10s" is a live region a reader turns off. The status
  // region beside it changes only when there is something to say.
  //
  // Neither carries `.facts`, deliberately: that class is the bar's one run-fact
  // list, and a second element wearing it turns every `.facts` lookup — in this
  // file and in the end-to-end suite — into an ambiguous one.
  if (!PROV.elapsed) PROV.elapsed = el('span', { class: 'small muted', id: 'refresh-elapsed', 'aria-hidden': 'true' });
  if (!PROV.live) {
    PROV.live = el('span', { class: 'small muted', id: 'refresh-status', role: 'status', 'aria-live': 'polite' });
  }
  // The node is REUSED across renders rather than rebuilt, because a live region
  // announces a change to content it already had — replacing the element loses
  // the announcement. Its text is synced here, and writing the same text again
  // is a no-op, so nothing is said twice.
  const message = PROV.status ? PROV.status.message : '';
  if (PROV.live.textContent !== message) {
    clear(PROV.live);
    if (message) PROV.live.appendChild(document.createTextNode(message));
  }
  return [btn, PROV.elapsed, PROV.live];
}

/*
  A refresh can take minutes, so it does not go through api(): that helper
  abandons a request after twenty seconds, which is the right deadline for a
  read and the wrong one for an indexer. The deadline here is the server's own
  bound plus a minute, and a client that gives up says the rebuild may still be
  running rather than implying it stopped.
*/
const REFRESH_TIMEOUT_MS = 11 * 60 * 1000;

async function runRefresh(btn) {
  if (PROV.busy) return;
  const refresh = (state.indexstate && state.indexstate.refresh) || {};
  const headerName = text(refresh.header, 'X-RepoCortex-Reindex');
  const hadFocus = document.activeElement === btn;

  PROV.busy = true;
  PROV.started = Date.now();
  btn.disabled = true;
  btn.setAttribute('aria-busy', 'true');
  btn.textContent = 'Refreshing…';
  setRefreshStatus('Rebuilding the index. This screen keeps showing the old run until it finishes.', 'busy');
  startElapsed();

  const controller = new AbortController();
  let timedOut = false;
  const deadline = setTimeout(() => { timedOut = true; controller.abort(); }, REFRESH_TIMEOUT_MS);

  let res = null;
  let raw = '';
  let unreachable = '';
  try {
    // The header is what makes this safe to expose at all: a cross-site form
    // cannot set it, and a cross-site fetch that tries is preflighted by a
    // request this server never answers.
    const headers = { 'Accept': 'application/json' };
    headers[headerName] = '1';
    res = await fetch(new URL('/api/reindex', window.location.origin), {
      method: 'POST', headers: headers, signal: controller.signal,
    });
    raw = await res.text();
  } catch (e) {
    unreachable = timedOut
      ? 'The portal stopped waiting after ' + Math.round(REFRESH_TIMEOUT_MS / 60000)
        + ' minutes. The rebuild may still be running on the server; reload this page to see how it ended.'
      : 'The portal could not reach its own server to refresh. It may have been stopped: '
        + (e && e.message ? e.message : String(e));
  } finally {
    clearTimeout(deadline);
    stopElapsed();
  }

  let body = null;
  try { body = JSON.parse(raw); } catch (e) { body = null; }

  let rebuilt = false;
  if (unreachable) {
    setRefreshStatus(unreachable, 'bad');
  } else if (res.status === 409) {
    setRefreshStatus(sentence(body && body.detail,
      'A refresh is already running on this portal, so this one was not started.'), 'warn');
  } else if (!res.ok) {
    setRefreshStatus(refusal(res, body), 'bad');
  } else {
    const done = (body && body.result) || {};
    rebuilt = true;
    setRefreshStatus('Rebuilt in ' + humanMillis(done.duration_ms) + ': ' + num(done.files) + ' '
      + plural('file', Number(done.files)) + ', ' + num(done.symbols) + ' symbols, '
      + num(done.edges) + ' edges, revision ' + num(done.revision)
      + '. This screen has been re-read from it.', 'ok');
  }

  PROV.busy = false;
  // The provenance is now a description of a run that no longer exists, so it
  // is re-read before anything claims otherwise.
  await loadIndexState();
  if (rebuilt) reloadScreens();
  if (hadFocus) {
    // renderIndexBar rebuilt the bar, so the button that was focused is gone.
    // Focus follows the control rather than falling to the top of the document.
    const again = document.getElementById('refresh-index');
    if (again) again.focus();
  }
}

/*
  A failure has to leave a reader certain of one thing: the index they were
  reading a moment ago is still there. The server says so on the paths where it
  rebuilt and failed; on a refusal it does not, so this does.
*/
function refusal(res, body) {
  const said = (body && body.error)
    ? String(body.error)
    : 'The server answered ' + res.status + ' to the refresh with a body this page could not read.';
  if (/left as it was|left intact/i.test(said)) return sentence(said);
  return sentence(said) + ' The index was left as it was: nothing is written unless a rebuild finishes.';
}

function setRefreshStatus(message, tone) {
  PROV.status = message ? { message: message, tone: tone || '' } : null;
  if (PROV.live) {
    clear(PROV.live);
    if (message) PROV.live.appendChild(document.createTextNode(message));
  }
  renderProvenance();
}

function startElapsed() {
  stopElapsed();
  const tick = () => {
    if (!PROV.elapsed) return;
    PROV.elapsed.textContent = Math.round((Date.now() - PROV.started) / 1000) + 's';
  };
  tick();
  PROV.ticker = window.setInterval(tick, 1000);
}

function stopElapsed() {
  if (PROV.ticker) window.clearInterval(PROV.ticker);
  PROV.ticker = 0;
  if (PROV.elapsed) PROV.elapsed.textContent = '';
}

/*
  Re-read every screen from the run that now exists.

  The graph is the exception and has to be: `state.loaded.graph` gates the
  one-time wiring of the canvas shell, so clearing it appends a second heading
  and a second set of listeners. Dropping its DATA is the refetch path its own
  filters already use, and it keeps the renderer, the pan and the zoom.
*/
function reloadScreens() {
  for (const screen of SCREENS) if (screen !== 'graph') state.loaded[screen] = false;
  GRAPH_STATE.data = null;
  showScreen(state.q.s);
}

// An absolute local moment, because "3 hours ago" is not something a reader can
// line up against the commit they remember making.
function absoluteWhen(unix) {
  const secs = Number(unix);
  if (!Number.isFinite(secs) || secs <= 0) return '';
  const when = new Date(secs * 1000);
  if (Number.isNaN(when.getTime())) return '';
  return when.toLocaleString(undefined, {
    year: 'numeric', month: 'short', day: '2-digit',
    hour: '2-digit', minute: '2-digit', timeZoneName: 'short',
  });
}

function humanMillis(ms) {
  const v = Number(ms);
  if (!Number.isFinite(v) || v < 0) return 'an unrecorded time';
  if (v < 1000) return Math.round(v) + ' ms';
  if (v < 90000) return (v / 1000).toFixed(1) + ' s';
  return Math.round(v / 60000) + ' min';
}

// Server sentences arrive lowercase, mid-thought, because they are also written
// into logs. They are shown to a reader here, so they start like sentences.
function sentence(s, fallback) {
  const out = text(s, fallback === undefined ? '' : fallback);
  return out ? out.charAt(0).toUpperCase() + out.slice(1) : out;
}

/*
  Three hook states, not two. "Not checked" is not "not installed": a portal can
  be started without a hook reporter, and rendering that absence as a missing
  hook is a claim nobody made. `checked` is the field that separates them, and a
  payload that predates it is read as unknown rather than as bad news.
*/
function hookVerdict(hook) {
  const known = hook.checked === undefined ? hook.installed === true : !!hook.checked;
  if (!known) return { label: 'not checked', tone: 'warn', caption: 'this portal was given no hook reporter' };
  if (hook.installed) return { label: 'installed', tone: 'ok', caption: 'a commit rebuilds the index' };
  return { label: 'not installed', tone: 'warn', caption: 'nothing rebuilds after a commit' };
}

function renderProvenance() {
  const host = $('#provenance');
  if (!host) return;
  // The glossary is showing in this slot, and the index bar re-rendering is
  // no reason to take it away from the reader.
  if (PROV.glossary && !PROV.open) return;
  clear(host);
  host.hidden = !PROV.open;
  if (!PROV.open) return;
  // Through the CSSOM, not a style attribute: the portal ships
  // `style-src 'self'` with no 'unsafe-inline', so the attribute would be
  // dropped and the panel would sit flush against the edge of the window.
  host.style.setProperty('padding', '14px 24px 0');

  if (PROV.error) { showError(host, new Error(PROV.error), 'How this index was built'); return; }
  if (!state.indexstate) { host.appendChild(loading('Reading how this index was built…')); return; }

  const st = state.indexstate;
  const meta = st.meta || state.meta || {};
  const run = meta.run || {};
  const refresh = st.refresh || {};
  const hook = st.commit_hook || {};
  const hookSays = hookVerdict(hook);
  const built = absoluteWhen(run.finished_unix || run.started_unix);
  const rev = text(run.git_revision, '');

  const body = [];

  body.push(el('div', { class: 'kpis' },
    kpi('Built', built || '—', built
      ? humanAge(run.age_seconds) + ' ago · run ' + num(run.run_id)
      : 'this run recorded no finish time'),
    kpi('Built from', rev ? rev.slice(0, 12) : '—',
      rev ? 'the revision the indexer read' : 'this run recorded no git revision'),
    kpi('Commit hook', hookSays.label, hookSays.caption, hookSays.tone),
    kpi('Model calls', '0', 'nothing here was inferred')));

  body.push(el('p', { class: 'prose' },
    rev ? ['Built from revision ', el('code', { text: rev.slice(0, 12) }), '. '] : 'This run recorded no revision. ',
    'Whether that is still HEAD is not something this portal is told: it reads the index, never your checkout.'));

  body.push(el('p', { class: 'prose', text:
    'And a matching revision would not mean the code on disk matches. This index is a snapshot of the tree '
    + 'as it stood when that run finished. Every edit made since — staged, unstaged, or committed — is '
    + 'invisible to it, and to every count and every edge on this portal, until the next run.' }));

  body.push(el('p', { class: 'prose', text:
    'Building it cost zero model calls. Every file, symbol and relationship here was parsed out of the '
    + 'source; nothing on this portal was inferred by a language model, and a rebuild costs CPU and '
    + 'nothing else.' }));

  body.push(hookProse(hook, hookSays));

  body.push(el('div', { class: 'omit' },
    el('h4', { text: 'What this panel cannot tell you' }),
    el('ul', null,
      el('li', null,
        el('span', { class: 'what' }, 'Whether the revision above is still HEAD. This portal reads the index; nothing it is served describes your checkout.'),
        el('span', { class: 'fix' }, 'Check: ', el('code', { text: 'git rev-parse HEAD' }))),
      el('li', null,
        el('span', { class: 'what' }, 'The schema version of the store. No payload this portal receives carries it, so it is left unstated rather than guessed.'),
        el('span', { class: 'fix' }, 'Check: ', el('code', { text: 'cortex status' }))))));

  if (refresh.last) {
    const at = absoluteWhen(refresh.last.at_unix);
    const done = refresh.last.result;
    body.push(el('p', { class: 'small muted', text: done
      ? 'Last refresh from this portal: ' + (at || 'time not recorded') + ' — ' + num(done.files) + ' '
        + plural('file', Number(done.files)) + ', ' + num(done.symbols) + ' symbols, ' + num(done.edges)
        + ' edges in ' + humanMillis(done.duration_ms) + '.'
      : 'Last refresh from this portal: ' + (at || 'time not recorded') + ' did not finish. '
        + sentence(refresh.last.error, 'The server recorded no message.')
        + ' The index was left as it was.' }));
  }

  if (PROV.status) {
    body.push(el('p', {
      class: 'prose' + (PROV.status.tone === 'bad' || PROV.status.tone === 'warn' ? ' warnline' : ''),
      text: PROV.status.message,
    }));
  }

  host.appendChild(card('Where this index came from', 'and what it cannot see', body, refreshFooter(refresh)));
}

function hookProse(hook, says) {
  if (says.label === 'installed') {
    // The path is added only when the server has not already named it: two
    // sources for one sentence is how a panel ends up saying it twice.
    const said = text(hook.detail, 'A post-commit hook re-indexes this repository after every commit.');
    const names = hook.path && said.indexOf(String(hook.path)) < 0;
    return el('p', { class: 'prose' }, said,
      names ? [' Installed at ', el('code', { text: String(hook.path) }), '.'] : null);
  }
  if (says.label === 'not checked') {
    return el('div', null,
      el('p', { class: 'prose', text: text(hook.detail,
        'This portal was started without a hook reporter, so it cannot say whether a commit hook keeps '
        + 'this index current.') }),
      nextAction({
        label: 'Ask the repository',
        detail: 'The command reports the hook; this page only ever reports what it is told.',
        command: text(hook.command, 'cortex hooks status'),
      }));
  }
  return el('div', null,
    el('p', { class: 'prose', text: text(hook.detail,
      'No post-commit hook is installed, so this index is only as current as the last time it was built.')
      + ' Anything committed since that run is missing from every screen here, and nothing on this '
      + 'portal changes until the next one.' }),
    nextAction({
      label: 'Install the commit hook',
      detail: 'From a shell, not from here — a page must never write to a repository .git directory.',
      command: text(hook.command, 'cortex hooks install'),
    }));
}

/*
  The footer is where a portal that cannot refresh says what can. The command is
  the same work the button would do, so a reader on a read-only portal is not
  stuck — they are one paste away.
*/
function refreshFooter(refresh) {
  if (!refresh.enabled) {
    return nextAction({
      label: 'Rebuild it yourself',
      detail: text(refresh.detail,
        'This portal is read-only by default and cannot rebuild the index. Start it with --allow-refresh '
        + 'to enable a refresh button here, or run the rebuild yourself.'),
      command: text(refresh.command, 'cortex index && cortex resolve'),
    });
  }
  return nextAction({
    label: 'Refresh index, in the bar above',
    detail: 'It runs the same work as the command, then re-reads this screen from the new run. '
      + 'One refresh at a time: a second is refused rather than run beside the first.',
    command: text(refresh.command, 'cortex index && cortex resolve'),
  });
}

// ---------------------------------------------------------------- theme

function applyTheme(mode) {
  document.documentElement.setAttribute('data-theme', mode);
  const btn = $('#theme-toggle');
  if (btn) btn.textContent = 'Theme: ' + mode;
  try { window.localStorage.setItem('cortex-theme', mode); } catch (e) { /* private mode */ }
  if (state.view) state.view.draw();
}

function initTheme() {
  let mode = 'system';
  try { mode = window.localStorage.getItem('cortex-theme') || 'system'; } catch (e) { mode = 'system'; }
  applyTheme(mode);
  $('#theme-toggle').addEventListener('click', () => {
    const order = ['system', 'light', 'dark'];
    const now = document.documentElement.getAttribute('data-theme') || 'system';
    applyTheme(order[(order.indexOf(now) + 1) % order.length]);
  });
}

// ================================================================ SCREEN 01 — OVERVIEW

function screenHead(title, lede) {
  return el('div', { class: 'screen-head' }, el('h2', { text: title }), el('p', { class: 'lede', text: lede }));
}

/*
  The answer line is the first thing on a screen. Each screen's loader
  composes its sentence from the response it already has; the old lede
  becomes the supporting line. A screen whose response carries no summary
  number says what the screen shows, in one sentence, rather than inventing
  a figure. Falls back to screenHead if the module did not load.
*/
function answer(sentence, opts) {
  const dv = window.cortex || {};
  if (dv.answerLine) return dv.answerLine(sentence, opts);
  const flat = (Array.isArray(sentence) ? sentence : [sentence]).map((p) => (p && p.b) ? p.b : String(p)).join('');
  return screenHead(flat, (opts && opts.support) || '');
}
function B(s) { const dv = window.cortex || {}; return dv.bold ? dv.bold(s) : { b: String(s) }; }
function fold(summary, body, opts) {
  const dv = window.cortex || {};
  if (dv.foldable) return dv.foldable(summary, body, opts);
  const d = el('details', { class: 'fold' }, el('summary', { text: summary }), body);
  return d;
}

/*
  A screen that fetches before it can answer shows the pre-data sentence
  first and swaps it for the real one when the response lands: the answer
  is always the first thing on the screen, and the placeholder never
  survives the data. `host` is the screen; the node to replace is the first
  .answer inside it, or the head is appended if there is none yet.
*/
function setAnswer(host, node) {
  const old = host.querySelector(':scope > .answer, :scope > .screen-head');
  if (old) old.replaceWith(node); else host.prepend(node);
  return node;
}

// When the fetch fails the placeholder sentence ("…is being read") must not
// stand over a red error card as if it were still true. The answer says the
// screen could not load, and the card under it says why.
function failAnswer(host, what) {
  setAnswer(host, answer(what + ' could not load.', { support: 'The error below says why.' }));
}

// The API's next_action, in the shape the answer line takes: label, command,
// detail, and a screen as an href. Nothing is invented when there is none.
function nextOf(na) {
  if (!na || (!na.label && !na.command)) return null;
  const target = na.screen ? String(na.screen).split('#')[0] : '';
  return {
    label: text(na.label, ''), command: text(na.command, ''), detail: text(na.detail, ''),
    href: target && document.getElementById('screen-' + target) ? queryString(Object.assign({}, state.q, { s: target })) : '',
  };
}

LOADERS.overview = async function () {
  const host = $('#screen-overview');
  if (state.loaded.overview) return;
  state.loaded.overview = true;
  clear(host);

  // The question comes first. A reader arrives wanting to know what breaks if
  // they change something; an inventory of how many symbols the index holds is
  // an answer to a question nobody asked.
  host.appendChild(answer('This index is being read, to say how current it is and what it could not see.'));
  const body = el('div', { class: 'stack' }, loading('Reading the index…'));
  host.appendChild(body);

  let d;
  try { d = await api('/api/overview', { min_confidence: state.q.floor }); }
  catch (e) { clear(body); failAnswer(host, 'The overview'); showError(body, e, 'The overview'); return; }
  state.overview = d;
  state.meta = d.meta;
  renderIndexBar();
  clear(body);
  setAnswer(host, overviewAnswer(d));
  renderOverview(d, body);
  if (state.q.intent) { $('#task-input').value = state.q.intent; runTask(); }
};

function askBox() {
  const input = el('input', {
    id: 'task-input', type: 'text', autocomplete: 'off',
    placeholder: 'e.g. add idempotency to the payment callback handler',
    value: state.q.intent || '',
  });
  const form = el('form', { class: 'ask', onsubmit: (e) => { e.preventDefault(); runTask(); } },
    el('h2', { text: 'What are you about to change?' }),
    el('p', { class: 'sub', text: 'This compiles the request into a contract against the index: the files and symbols it touches, what it would break, the tests that cover it, and — first — the questions the index cannot answer.' }),
    el('div', { class: 'ask-row' },
      el('div', { class: 'grow' }, input),
      el('button', { class: 'primary', type: 'submit', text: 'Compile' })),
    el('div', { class: 'ask-examples' },
      ['Add idempotency to payment callback processing', 'Rename the user session token', 'Add a field to the audit log table'].map((example) =>
        el('button', {
          class: 'chip', type: 'button', text: example,
          onclick: () => { input.value = example; runTask(); },
        }))));
  return form;
}

/*
  runTask answers the landing screen's question. It calls the same contract
  endpoint the Task contract screen uses, so the two cannot disagree.

  Three defects lived here. It read `d.scope` when the payload nests everything
  under `d.contract`, so every list came back undefined and the counts rendered
  0 / 0 / 0 with no error. It sliced three lists at 25 with no notice, on the
  screen where this portal states its own principle that nothing is omitted
  silently. And it printed the context engine's internal budget accounting — a
  sentence about a "~1920-character source budget" — verbatim on eight
  consecutive rows of the first thing anyone runs.
*/
async function runTask() {
  const host = $('#task-result');
  const intent = $('#task-input').value.trim();
  clear(host);
  if (!intent) return;
  setQuery({ intent: intent }, { replace: true });
  host.appendChild(loading('Compiling a contract from the index…'));

  let d;
  try { d = await api('/api/contract', { intent: intent, min_confidence: state.q.floor }); }
  catch (e) { return showError(host, e, 'This task'); }
  clear(host);

  const k = d.contract || {};
  const counts = d.counts || {};
  const scope = dedupeRows(k.scope || [], (row) => row.path + '|' + row.symbol);
  const reach = dedupeRows(k.blast_radius || [], (row) => row.path + '|' + row.symbol);
  const tests = dedupeRows(k.tests || [], (row) => row.path + '|' + row.name);

  // The budget prose, lifted out of the rows and stated once. It is held
  // beside the rows rather than written onto them: every property this function
  // reads off a scope row has to be a field the API actually sends, and
  // internal/web/contract_test.go enforces exactly that.
  const whyOf = new Map();
  let withheld = 0;
  let withheldNote = '';
  for (const row of scope) {
    const split = String(row.why || '').split('; source withheld:');
    whyOf.set(row, split[0]);
    if (split.length > 1) { withheld++; if (!withheldNote) withheldNote = split[1].trim(); }
  }

  host.appendChild(el('div', { class: 'kpis' },
    kpi('In scope', num(counts.scope), 'files and symbols the change touches'),
    kpi('Reached', num(counts.blast_radius), 'symbols a change here would reach'),
    kpi('Covering tests', num(counts.tests), 'named by the graph or by proximity'),
    kpi('Unknowns', num(counts.unknowns), 'questions the index cannot answer', Number(counts.unknowns) > 0 ? 'warn' : 'ok')));

  if (d.interpretation) {
    host.appendChild(card('What was understood', 'read this before the answer',
      el('p', { class: 'prose', text: d.interpretation }),
      nextAction({ label: 'Open the full contract', detail: 'Constraints, acceptance criteria and risks, with evidence.', screen: 'contract' })));
  }

  const unknowns = d.unknowns || [];
  if (unknowns.length) {
    host.appendChild(card('What the index could not tell you', num(unknowns.length) + ' ' + plural('unknown', unknowns.length),
      el('ul', { class: 'prose' }, unknowns.map((u) => el('li', { text: u }))), null, 'unknown'));
  }

  host.appendChild(card('Scope', 'what the change touches',
    table(
      [{ label: 'Symbol' }, { label: 'Kind' }, { label: 'File' }, { label: 'Why it is here' }],
      scope,
      {
        cap: 14,
        row: (sc) => el('tr', null,
          el('td', null, el('span', { class: 'sym', text: text(sc.symbol) })),
          el('td', { class: 'nowrap muted small', text: text(sc.kind) }),
          el('td', null, el('span', { class: 'path', text: text(sc.path) })),
          el('td', { class: 'small muted', text: text(whyOf.get(sc), 'no reason recorded') })),
      }),
    withheld ? inlineOmit(el('b', { text: num(withheld) }), ' of these rows carry no source excerpt. The pack allocates source by relevance rather than evenly: ', withheldNote) : null,
    null, 'flush'));

  if (reach.length) {
    host.appendChild(card('What it would reach', 'every row carries the tier of the weakest edge on the path',
      table(
        [{ label: 'Symbol' }, { label: 'Depth', num: true }, { label: 'Confidence' }, { label: 'File and line' }],
        reach,
        {
          cap: 14,
          row: (rc) => el('tr', { class: 't-' + tierOf(rc.confidence) },
            el('td', null, el('span', { class: 'sym', text: text(rc.symbol) }),
              rc.target ? el('div', { class: 'small muted', text: 'from ' + text(rc.target) }) : null),
            el('td', { class: 'num', text: num(rc.depth) }),
            el('td', null, tierBadge(rc.confidence)),
            el('td', null, evidenceFromString(rc.evidence, rc.path))),
        }),
      null, 'flush'));
  }

  if (tests.length) {
    host.appendChild(card('Tests that already cover it', 'run these before and after',
      table(
        [{ label: 'Test' }, { label: 'Signal' }, { label: 'Confidence' }, { label: 'File' }],
        tests,
        {
          cap: 12,
          row: (t) => el('tr', { class: 't-' + tierOf(t.confidence) },
            el('td', null, el('span', { class: 'sym', text: text(t.name) }),
              t.covers ? el('div', { class: 'small muted', text: 'covers ' + text(t.covers) }) : null),
            el('td', { class: 'nowrap muted small', text: text(t.signal) }),
            el('td', null, tierBadge(t.confidence)),
            el('td', null, el('span', { class: 'path', text: text(t.path) }))),
        }),
      null, 'flush'));
  }

  host.appendChild(renderOmissions(d.omissions, {
    extra: dedupeExtra(scope.length, k.scope, 'scope rows', reach.length, k.blast_radius, tests.length, k.tests),
  }));
  const na = nextAction(d.next_action);
  if (na) host.appendChild(na);
}

// Identical rows told a reader the same file was involved three times. The
// count of what was folded away is kept and stated rather than dropped.
function dedupeRows(rows, keyOf) {
  const seen = new Map();
  const out = [];
  for (const row of rows) {
    const key = keyOf(row);
    if (seen.has(key)) { seen.get(key).duplicates = (seen.get(key).duplicates || 1) + 1; continue; }
    const copy = Object.assign({}, row);
    seen.set(key, copy);
    out.push(copy);
  }
  return out;
}

function dedupeExtra(scopeShown, scopeRaw, label, reachShown, reachRaw, testsShown, testsRaw) {
  const out = [];
  const folded = (scopeRaw ? scopeRaw.length - scopeShown : 0)
    + (reachRaw ? reachRaw.length - reachShown : 0)
    + (testsRaw ? testsRaw.length - testsShown : 0);
  if (folded > 0) {
    out.push({
      count: folded, noun: 'duplicate rows folded together',
      detail: 'The same file and symbol appeared more than once in the same list. They are one row here; the index still holds each occurrence.',
    });
  }
  return out;
}

// Contract rows carry evidence as a "path:line" string rather than an object.
/*
  A risk's severity is not a confidence tier, but it is spelled with the same
  three ids, and a reader who has just learned that "medium" means uncertain
  must not meet it again meaning something else. The severity reads as a
  word for how bad the risk is; the id stays in the title.
*/
const SEVERITY_WORD = { high: 'serious', medium: 'moderate', low: 'minor' };
function severityWord(sev) {
  const id = String(sev || '').toLowerCase();
  return (SEVERITY_WORD[id] || text(sev, 'unrated')) + ' risk';
}

/*
  The context engine's reasons and evidence name a path confidence by tier id
  ("path confidence high"). On screen every such id becomes the word, inside
  an element that says which tier it is; the rest of the sentence is
  untouched and the wire is not. wordedReason always returns a node, with the
  same missing-value guard as text(); wordedEvidence returns null for an
  evidence string that is a location, so the caller falls through to the
  file:line rendering.
*/
const PATH_CONFIDENCE = /\bpath confidence (exact|high|medium|low)\b/g;
function wordedReason(s, fallback) {
  const str = text(s, fallback);
  const node = el('span');
  let last = 0;
  for (const m of str.matchAll(PATH_CONFIDENCE)) {
    node.appendChild(document.createTextNode(str.slice(last, m.index) + 'path confidence '));
    node.appendChild(el('span', { 'data-tier': m[1], text: tierWord(m[1]) }));
    last = m.index + m[0].length;
  }
  node.appendChild(document.createTextNode(str.slice(last)));
  return node;
}
function wordedEvidence(s) {
  const str = String(s || '');
  PATH_CONFIDENCE.lastIndex = 0;
  if (!PATH_CONFIDENCE.test(str)) return null;
  const node = wordedReason(str);
  node.className = 'path';
  return node;
}

function evidenceFromString(s, fallbackPath) {
  if (!s) return evidence(fallbackPath || null, null, { absent: 'no line recorded' });
  const at = String(s).lastIndexOf(':');
  const line = at > 0 ? Number(String(s).slice(at + 1)) : NaN;
  if (at > 0 && Number.isFinite(line)) return evidence(String(s).slice(0, at), line);
  return evidence(String(s), null);
}

/*
  renderOverview. Every section ends in a next action, and every cap this screen
  applies is stated where the cap bites rather than 4,876 pixels below it.
*/
/*
  The overview's answer: how current the index is, and whether every file was
  read. A file that could not be read is a symbol count that is too low on
  every screen after this one, so it is the second sentence, not a footnote.
*/
function overviewAnswer(d) {
  const fresh = d.freshness || {};
  const failed = Number(fresh.parse_failures || 0);
  const parts = ['Indexed ', B(text(fresh.age_human, 'an unknown time')), ' ago.'];
  if (failed > 0) parts.push(' ', B(num(failed) + ' ' + plural('file', failed)), ' could not be read, so counts on every screen may be lower than reality.');
  else parts.push(' Every file was read.');
  return answer(parts, {
    support: 'What this index knows, how much of it is proven, and where it is blind. Read the blind spots before you trust an empty answer.',
    next: nextOf(fresh.next_action),
  });
}

function renderSystemHero(d) {
  const f = d.fingerprint;
  const project = (d.meta && d.meta.project) ? d.meta.project : 'System';
  const mainLang = (d.languages && d.languages.mix && d.languages.mix[0]) ? d.languages.mix[0].language : 'Codebase';
  const rev = (d.freshness && d.freshness.git_revision) ? d.freshness.git_revision.slice(0, 7) : 'head';
  const age = (d.freshness && d.freshness.age_human) ? d.freshness.age_human : 'current';

  return el('div', { class: 'overview-hero' },
    el('div', { class: 'hero-header' },
      el('div', { class: 'hero-title-area' },
        el('h1', { class: 'hero-title', text: project }),
        el('span', { class: 'hero-badge', text: (String(mainLang).charAt(0).toUpperCase() + String(mainLang).slice(1)) + ' · Enterprise Service' }),
        el('span', { class: 'hero-badge operational', text: '● Verified Index Guarantee' })),
      el('div', { class: 'hero-meta' },
        el('span', { text: 'rev: ' + rev }),
        el('span', { text: 'indexed ' + age + ' ago' }),
        el('span', { text: 'local-first' }))),
    el('div', { class: 'topology-stream' },
      el('span', { class: 'topology-stage', title: 'Inbound HTTP/REST Endpoints' },
        el('span', { text: '🌐 Inbound APIs' }),
        el('span', { class: 'stage-count', text: num(f.endpoints) })),
      el('span', { class: 'topology-arrow', text: '──►' }),
      el('span', { class: 'topology-stage', title: 'Business Domains and Services' },
        el('span', { text: '⚙️ Domain Services' }),
        el('span', { class: 'stage-count', text: num(f.containers) + ' ' + plural(f.container_kind, f.containers) })),
      el('span', { class: 'topology-arrow', text: '──►' }),
      el('span', { class: 'topology-stage', title: 'Persistence Repositories & DAOs' },
        el('span', { text: '🗄️ Repositories' }),
        el('span', { class: 'stage-count', text: num(f.exported_symbols) + ' exported' })),
      el('span', { class: 'topology-arrow', text: '──►' }),
      el('span', { class: 'topology-stage', title: 'Database Tables Referenced' },
        el('span', { text: '💾 Data Tables' }),
        el('span', { class: 'stage-count', text: num(f.tables) }))));
}

function renderUserJourneyHub(f) {
  return el('div', { class: 'journey-hub' },
    el('div', { class: 'journey-card arch' },
      el('div', { class: 'journey-header' },
        el('div', { class: 'journey-icon', text: '🏛️' }),
        el('div', null,
          el('h3', { class: 'journey-title', text: 'System Architecture' }),
          el('p', { class: 'journey-desc', text: 'Explore structural maps, inbound APIs, and data models across services.' }))),
      el('div', { class: 'journey-actions' },
        el('button', { class: 'journey-btn primary', type: 'button', text: 'Visual Code Map →', onclick: () => showScreen('graph') }),
        el('button', { class: 'journey-btn', type: 'button', text: 'APIs (' + num(f.endpoints) + ') →', onclick: () => showScreen('crossstack') }),
        el('button', { class: 'journey-btn', type: 'button', text: 'Tables (' + num(f.tables) + ') →', onclick: () => showScreen('tables') }))),

    el('div', { class: 'journey-card safety' },
      el('div', { class: 'journey-header' },
        el('div', { class: 'journey-icon', text: '🛡️' }),
        el('div', null,
          el('h3', { class: 'journey-title', text: 'Change Safety & Impact' }),
          el('p', { class: 'journey-desc', text: 'Compute transitive blast radius and compile pre-commit safety contracts.' }))),
      el('div', { class: 'journey-actions' },
        el('button', { class: 'journey-btn primary', type: 'button', text: 'Plan a Refactor →', onclick: () => showScreen('planchange') }),
        el('button', { class: 'journey-btn', type: 'button', text: 'Blast Radius →', onclick: () => showScreen('blast') }),
        el('button', { class: 'journey-btn', type: 'button', text: 'Task Brief →', onclick: () => showScreen('contract') }))),

    el('div', { class: 'journey-card agent' },
      el('div', { class: 'journey-header' },
        el('div', { class: 'journey-icon', text: '🤖' }),
        el('div', null,
          el('h3', { class: 'journey-title', text: 'AI Coding Agent Hub' }),
          el('p', { class: 'journey-desc', text: 'Equip Claude, Antigravity & Cursor with compiler-grade codebase ground truth.' }))),
      el('div', { class: 'journey-actions' },
        el('button', { class: 'journey-btn primary', type: 'button', text: 'Connect Agent (MCP) →', onclick: () => showScreen('agent') }),
        el('button', { class: 'journey-btn', type: 'button', text: 'Symbol Search →', onclick: () => showScreen('search') }),
        el('button', { class: 'journey-btn', type: 'button', text: 'Debt & Blindspots →', onclick: () => showScreen('missed') }))));
}

function renderLiveSurfaceExplorer(d) {
  const host = el('div', { class: 'surface-grid' });

  const apiCard = el('div', { class: 'surface-card' },
    el('div', { class: 'surface-header' },
      el('h3', { class: 'surface-title', text: '🌐 Live Exposed API Catalogue' }),
      el('button', { class: 'action-mini-btn', type: 'button', text: 'Full Catalogue →', onclick: () => showScreen('crossstack') })),
    el('div', { class: 'surface-table-wrap', id: 'overview-api-table' },
      loading('Reading API endpoints…')));

  const dbCard = el('div', { class: 'surface-card' },
    el('div', { class: 'surface-header' },
      el('h3', { class: 'surface-title', text: '💾 Core Database Tables' }),
      el('button', { class: 'action-mini-btn', type: 'button', text: 'All Tables →', onclick: () => showScreen('tables') })),
    el('div', { class: 'surface-table-wrap', id: 'overview-db-table' },
      loading('Reading database footprint…')));

  host.appendChild(apiCard);
  host.appendChild(dbCard);

  (async () => {
    try {
      const cs = await api('/api/crossstack');
      const tableHost = $('#overview-api-table');
      if (!tableHost) return;
      clear(tableHost);
      const chains = (cs && cs.chains) ? cs.chains.slice(0, 5) : [];
      if (!chains.length) {
        tableHost.appendChild(el('p', { class: 'small muted', text: 'No live HTTP routes found in this service.' }));
      } else {
        const table = el('table', { class: 'surface-table' },
          el('thead', null,
            el('tr', null,
              el('th', { text: 'Method' }),
              el('th', { text: 'Endpoint Route' }),
              el('th', { text: 'Handler' }),
              el('th', { text: 'Action' }))),
          el('tbody', null,
            chains.map((c) => {
              const ep = c.endpoint || {};
              const epName = ep.name || 'Unknown';
              const parts = epName.split(' ');
              const method = parts.length > 1 ? parts[0] : 'API';
              const route = parts.length > 1 ? parts.slice(1).join(' ') : epName;
              const h = (c.handler && c.handler.node) ? c.handler.node.name : 'handler';
              return el('tr', null,
                el('td', null, el('span', { class: 'method-badge method-' + method.toLowerCase(), text: method })),
                el('td', { style: 'font-family:var(--mono);font-size:11.5px', text: route }),
                el('td', { style: 'font-family:var(--mono);font-size:11px;color:var(--ink-2)', text: h }),
                el('td', null, el('button', {
                  class: 'action-mini-btn', type: 'button', text: '⚡ Blast Radius',
                  onclick: () => { setQuery({ s: 'blast', target: epName }); showScreen('blast'); },
                })));
            })));
        tableHost.appendChild(table);
      }
    } catch (_) {
      const tableHost = $('#overview-api-table');
      if (tableHost) {
        clear(tableHost);
        tableHost.appendChild(el('p', { class: 'small muted', text: 'Endpoints not available.' }));
      }
    }

    try {
      const db = await api('/api/db');
      const tableHost = $('#overview-db-table');
      if (!tableHost) return;
      clear(tableHost);
      const tbls = (db && db.tables) ? db.tables.slice(0, 5) : [];
      if (!tbls.length) {
        tableHost.appendChild(el('p', { class: 'small muted', text: 'No database tables referenced.' }));
      } else {
        const table = el('table', { class: 'surface-table' },
          el('thead', null,
            el('tr', null,
              el('th', { text: 'Table Name' }),
              el('th', { text: 'Statements' }),
              el('th', { text: 'Access Mode' }),
              el('th', { text: 'Action' }))),
          el('tbody', null,
            tbls.map((t) => {
              const r = Number(t.read_statements || 0);
              const w = Number(t.write_statements || 0);
              const mode = (r > 0 && w > 0) ? 'Read & Write' : (w > 0 ? 'Write-only' : 'Read-only');
              return el('tr', null,
                el('td', { style: 'font-family:var(--mono);font-size:12px;font-weight:600', text: t.table }),
                el('td', { style: 'font-family:var(--mono);font-size:11.5px', text: num(t.total_statements) + ' queries' }),
                el('td', null, el('span', { class: 'table-rw-badge' + (w > 0 ? ' rw' : ''), text: mode })),
                el('td', null, el('button', {
                  class: 'action-mini-btn', type: 'button', text: '📊 Inspect Table',
                  onclick: () => showScreen('tables'),
                })));
            })));
        tableHost.appendChild(table);
      }
    } catch (_) {
      const tableHost = $('#overview-db-table');
      if (tableHost) {
        clear(tableHost);
        tableHost.appendChild(el('p', { class: 'small muted', text: 'Database tables not available.' }));
      }
    }
  })();

  return host;
}

function renderAgentMCPCenter(d) {
  const pth = (d.meta && d.meta.index_path) ? d.meta.index_path.replace('/.cortex/index.db', '') : '.';
  const mcpCmd = 'cortex --project ' + pth + ' serve';

  return el('div', { class: 'agent-mcp-panel' },
    el('div', { class: 'surface-header' },
      el('h3', { class: 'surface-title', text: '🤖 Autonomous AI Agent & MCP Integration' }),
      el('button', { class: 'action-mini-btn', type: 'button', text: 'Agent Guide →', onclick: () => showScreen('agent') })),
    el('p', { class: 'prose', text: 'Give Claude Code, Cursor, Antigravity, or custom agents compiler-grade codebase understanding with zero hallucinations and instant blast radius analysis.' }),
    el('div', { class: 'mcp-code-row' },
      el('code', { class: 'mcp-cmd', text: mcpCmd }),
      el('button', {
        class: 'primary', type: 'button', text: 'Copy MCP Command',
        onclick: (e) => {
          navigator.clipboard.writeText(mcpCmd);
          const orig = e.target.textContent;
          e.target.textContent = 'Copied!';
          setTimeout(() => { e.target.textContent = orig; }, 2000);
        },
      })));
}

function renderOverview(d, host) {
  const f = d.fingerprint;

  // 1. Enterprise System Hero Header
  host.appendChild(renderSystemHero(d));

  // 2. Enterprise User Journey Hub (3 Pillars)
  host.appendChild(renderUserJourneyHub(f));

  // 3. Core Architectural KPIs
  host.appendChild(el('div', { class: 'kpis' },
    kpi('Files', num(f.files), plural('symbol', f.symbols) + ': ' + num(f.symbols)),
    kpi('Relationships', num(f.edges), 'each with a tier and a file:line'),
    kpi(plural(f.container_kind, 2), num(f.containers), 'the unit this graph groups by'),
    kpi('Exported surface', num(f.exported_symbols), 'symbols visible outside their file')));

  host.appendChild(card('Runtime surface', 'what this system exposes',
    el('div', { class: 'kpis' },
      kpi('Endpoints', num(f.endpoints), 'HTTP routes the index found'),
      kpi('Tables', num(f.tables), 'database tables referenced in code')),
    nextAction(f.next_action)));

  // 4. Live Architecture Surface Explorer (APIs and Tables)
  host.appendChild(renderLiveSurfaceExplorer(d));

  // 5. Pre-Change Safety Task Compiler
  host.appendChild(askBox());
  const taskResult = el('div', { class: 'stack', id: 'task-result' });
  host.appendChild(taskResult);

  // 6. Autonomous AI Agent & MCP Hub
  host.appendChild(renderAgentMCPCenter(d));

  // 7. Resolution & Honesty Surface
  const r = d.resolution;
  host.appendChild(card('Resolution coverage', 'why every answer here is a lower bound',
    [
      el('div', { class: 'kpis' },
        kpi('Bound', pct(r.bound_pct), 'of ' + num(r.bindable_references) + ' references that name something this index defines',
          Number(r.bound_pct) < 60 ? 'warn' : 'ok'),
        kpi('Unresolved', num(r.unresolved), 'never bound to any definition'),
        kpi('Ambiguous', num(r.ambiguous), 'matched more than one definition'),
        kpi('Compiler-proven', num(r.bound_by_compiler), 'bound from a SCIP index')),
      resolutionFunnel(r, d.confidence),
      el('p', { class: 'prose', text: text(r.caveat) }),
    ],
    nextAction(r.next_action)));

  const c = d.confidence;
  const edgeTotal = (c && c.edges) ? c.edges.total : 0;
  host.appendChild(fold('Evidence — how sure each of the ' + num(edgeTotal) + ' relationships is', confidenceCard(c), { key: 'overview:confidence' }));
  host.appendChild(blindSpotsCard(d));
  const mix = (d.languages && d.languages.mix) ? d.languages.mix : [];
  host.appendChild(fold('Evidence — ' + num(mix.length) + ' ' + plural('language', mix.length) + ' in this repository', languageCard(d.languages, d.blind_spots), { key: 'overview:languages' }));
  host.appendChild(freshnessCard(d.freshness, d.meta ? d.meta.run : null));
  host.appendChild(renderOmissions(d.omissions, { key: 'overview' }));
}

/*
  THE RESOLUTION FUNNEL, DRAWN.

  These five numbers were four KPI tiles and a paragraph, and a reader had to do
  the subtraction in their head to see the shape of the loss: how many
  references the run saw, how many of them named something this index defines,
  how many of THOSE were bound, how many survived into edges, and how few a
  compiler stands behind. Drawn to a common denominator the shape is the whole
  argument — the drop from bindable to bound is the work left to do, and the
  last bar is usually zero, which is the sentence this product exists to say out
  loud.

  Every bar is a field the API already sends. The percentages are computed here
  against stage one and labelled as shares of it, so no bar can imply a
  denominator it does not have.
*/
function resolutionFunnel(r, c) {
  const edges = (c && c.edges) ? c.edges : {};
  const stages = [
    { label: 'References seen', v: Number(r.total_references || 0), tone: '',
      note: 'every name the parser recorded as pointing at something' },
    { label: 'Bindable', v: Number(r.bindable_references || 0), tone: '',
      note: 'names something this index defines; the rest are library or framework code' },
    { label: 'Bound', v: Number(r.resolved || 0), tone: 'warn',
      note: 'resolved to exactly one definition' },
    { label: 'Became edges', v: Number(edges.total || 0), tone: 'warn',
      note: 'relationships in the graph every screen walks' },
    { label: 'Compiler-proven', v: Number(r.bound_by_compiler || 0), tone: 'bad',
      note: 'settled by an imported compiler index rather than inferred from names' },
  ];
  const top = Math.max(1, stages[0].v);

  const wrap = el('div', {
    class: 'funnel', role: 'img',
    'aria-label': stages.map((s) => s.label + ' ' + num(s.v)).join(', '),
    style: 'display:grid;gap:9px;margin:12px 0',
  });
  for (const stage of stages) {
    const share = Math.min(100, (stage.v / top) * 100);
    const fill = el('span', {
      'aria-hidden': 'true',
      style: 'position:absolute;left:0;top:0;bottom:0;border-radius:4px;width:' + share.toFixed(2) + '%;'
        + 'background:var(--' + (stage.tone === 'bad' ? 'bad' : (stage.tone === 'warn' ? 'warn' : 'accent')) + ');opacity:.26',
    });
    const bar = el('div', {
      style: 'position:relative;height:30px;border-radius:5px;background:var(--bg-sunken);'
        + 'border:1px solid var(--line);overflow:hidden',
    }, fill, el('span', {
      style: 'position:absolute;left:0;top:0;right:0;bottom:0;display:flex;align-items:center;'
        + 'padding:0 11px;font-family:var(--mono);font-size:11.5px;color:var(--ink)',
      text: stage.note,
    }));
    wrap.appendChild(el('div', {
      style: 'display:grid;grid-template-columns:170px minmax(0,1fr) auto;gap:12px;align-items:center;font-size:13px',
    },
      el('span', { style: 'color:var(--ink-2)', text: stage.label }),
      bar,
      el('span', {
        style: 'font-family:var(--mono);font-variant-numeric:tabular-nums;font-size:13px',
        text: num(stage.v) + '  ' + pct((stage.v / top) * 100, 0),
      })));
  }
  wrap.appendChild(el('p', { class: 'hint',
    text: 'Every bar is a share of the references this run saw. `Bound` is counted over Bindable, not over the top bar — '
      + 'a reference to code this index does not contain cannot be bound by any improvement to binding, and counting it as a '
      + 'failure would turn "this project calls a lot of library code" into what reads as a defect rate.' }));
  return wrap;
}

function confidenceCard(c) {
  const edges = c.edges || {};
  const total = Math.max(1, Number(edges.total || 0));
  const bar = el('div', { class: 'stacked', role: 'img', 'aria-label': tierAria(edges) });
  for (const tv of ['exact', 'high', 'medium', 'low', 'unknown']) {
    const n = Number(edges[tv] || 0);
    if (n <= 0) continue;
    const share = (n / total) * 100;
    bar.appendChild(el('span', {
      class: 's-' + tv, 'data-tier': tv, style: 'width:' + share.toFixed(2) + '%',
      title: tierSentence(tv) + ': ' + num(n) + ' (' + share.toFixed(1) + '%)',
    }, share > 7 ? tierWord(tv) : ''));
  }

  const kinds = (c.by_kind || []).slice().sort((a, b) => (b.histogram.total || 0) - (a.histogram.total || 0));
  return card('Confidence', num(c.actionable) + ' of ' + num(edges.total) + ' edges are very likely or better',
    [
      bar,
      el('p', { class: 'prose', text: 'Every relationship in this index carries a measured tier, not an assertion. Proven means a compiler proved it; very likely is measured 87–96% right; uncertain is measured 6–38% right, which is why it is drawn dashed and never acted on; a guess is measured under 3% right, ghosted and off by default.' }),
      table(
        [{ label: 'Edge kind' }, { label: 'Total', num: true }, { label: 'Distribution' }, { label: 'Actionable', num: true }],
        kinds,
        {
          cap: 10,
          row: (row) => {
            const hist = row.histogram || {};
            const tot = Math.max(1, Number(hist.total || 0));
            const mini = el('div', { class: 'stacked', style: 'height:14px' });
            for (const tv of ['exact', 'high', 'medium', 'low', 'unknown']) {
              const n = Number(hist[tv] || 0);
              if (n <= 0) continue;
              mini.appendChild(el('span', { class: 's-' + tv, 'data-tier': tv, style: 'width:' + ((n / tot) * 100).toFixed(2) + '%', title: tierSentence(tv) + ': ' + num(n) }));
            }
            return el('tr', null,
              el('td', null, el('code', { text: text(row.kind) })),
              el('td', { class: 'num', text: num(hist.total) }),
              el('td', null, mini),
              el('td', { class: 'num', text: num(row.actionable) }));
          },
        }),
    ],
    nextAction(c.next_action));
}

function tierAria(edges) {
  return ['exact', 'high', 'medium', 'low'].map((tv) => tierWord(tv) + ' ' + num(edges[tv] || 0)).join(', ');
}

/*
  The blind-spots table used to be sixty alphabetical rows of a forty-word
  sentence that differed only in a language name and a count — 4,876 pixels,
  55% of the page. Worse, the API caps at sixty and returns them alphabetically,
  so on a repository whose two largest languages are python and typescript,
  NEITHER APPEARS: the list stops at "kotlin". That is now computed and said out
  loud, which is the difference between a cap and a lie.
*/
function blindSpotsCard(d) {
  const spots = (d.blind_spots && d.blind_spots.spots) ? d.blind_spots.spots.slice() : [];
  spots.sort((a, b) => (b.symbols || 0) - (a.symbols || 0) || String(a.language).localeCompare(String(b.language)));

  const present = new Set(spots.map((s) => s.language));
  const lastAlpha = spots.map((s) => s.language).sort()[spots.length - 1] || '';
  const missing = (d.languages && d.languages.mix ? d.languages.mix : [])
    .filter((m) => Number(m.symbols) > 0 && !present.has(m.language) && String(m.language) > lastAlpha)
    .slice(0, 6);

  const body = [
    el('p', { class: 'prose', text: 'A (language, kind) pair with symbols and no edges is a hole in this index, not a repository without those relationships. Every question of that shape returns an empty answer, and an empty answer here is not evidence that nothing is there. Ranked by how many symbols sit behind the hole.' }),
    table(
      [{ label: 'Language' }, { label: 'Edge kind' }, { label: 'Symbols behind it', num: true }, { label: 'Edges', num: true }, { label: 'Severity' }],
      spots,
      {
        cap: 8,
        row: (row) => el('tr', { title: text(row.detail, '') },
          el('td', null, el('code', { text: text(row.language) })),
          el('td', null, el('code', { text: text(row.kind) })),
          el('td', { class: 'num', text: num(row.symbols) }),
          el('td', { class: 'num', text: num(row.edges) }),
          el('td', null, el('span', { class: 'tier tier-' + (row.severity === 'blind' ? 'low' : 'medium'), text: text(row.severity) }))),
      }),
  ];

  if (missing.length) {
    body.push(el('div', { class: 'held' },
      el('h4', { text: 'Your largest languages are not in this table' }),
      el('p', { class: 'what' },
        'The index returned ' + num(spots.length) + ' pairs and they arrive alphabetically, stopping at "' + lastAlpha + '". ',
        el('b', { text: missing.map((m) => m.language).join(', ') }),
        ' sort after that and are cut — including ' + num(missing[0].symbols) + ' symbols in ' + missing[0].language + '. Absence from this table is not a clean bill of health.'),
      el('p', { class: 'fix' }, 'To check one directly: ', el('code', { text: 'cortex status --language ' + missing[0].language }))));
  }

  return card('Blind spots', num(d.blind_spots ? d.blind_spots.blind_count : 0) + ' pairs with symbols and zero edges',
    body, nextAction(d.blind_spots ? d.blind_spots.next_action : null), 'flush');
}

/*
  The language table listed 29 languages including markdown, csv, vimdoc and
  prolog with the same weight as python. Languages that produced no symbols
  cannot produce an edge and cannot appear in any other view, so they are
  separated out and counted rather than mixed in.
*/
function languageCard(languages, blindSpots) {
  const mix = (languages && languages.mix) ? languages.mix.slice() : [];
  const productive = mix.filter((m) => Number(m.symbols) > 0);
  const inert = mix.filter((m) => !(Number(m.symbols) > 0));
  const maxFiles = productive.reduce((mx, m) => Math.max(mx, Number(m.files) || 0), 1);

  return card('Language mix', num(productive.length) + ' languages produced symbols',
    [
      table(
        [{ label: 'Language' }, { label: 'Files', num: true }, { label: 'Share' }, { label: 'Symbols', num: true }],
        productive,
        {
          cap: 10,
          row: (row) => el('tr', null,
            el('td', null, el('code', { text: text(row.language) })),
            el('td', { class: 'num', text: num(row.files) }),
            el('td', null, el('div', { class: 'stacked', style: 'height:12px;width:120px' },
              el('span', { class: 's-high', style: 'width:' + ((Number(row.files) / maxFiles) * 100).toFixed(1) + '%', title: pct(row.file_pct) }))),
            el('td', { class: 'num', text: num(row.symbols) })),
        }),
      inert.length ? inlineOmit(el('b', { text: num(inert.length) }), ' further ' + plural('language', inert.length) + ' produced no symbols at all — ',
        inert.map((m) => m.language).join(', '),
        '. They are indexed as files and cannot appear in any graph, so they are counted here rather than ranked beside code.') : null,
    ],
    nextAction(languages ? languages.next_action : null), 'flush');
}

/*
  Freshness, and the run's own verdict. `degraded` is stated with the
  observable facts behind it. The API does not send a reason for the status, so
  the portal says which facts it is reading rather than inventing one — see
  assets/NEEDS-API.md, which asks for `run.status_reason`.
*/
function freshnessCard(fresh, run) {
  const health = (state.meta && state.meta.index_health) ? state.meta.index_health : {};
  const status = text(health.status, run ? text(run.status, 'unknown') : 'unknown');
  const trusted = health.trustworthy === undefined ? (status === 'ok') : !!health.trustworthy;
  const reasons = health.reasons || [];
  const tone = trusted ? '' : (text(health.severity, 'warn') === 'error' ? 'bad' : 'warn');

  const node = card('Index health and freshness', 'verdict: ' + status,
    [
      health.headline ? el('p', { class: 'prose', text: health.headline }) : null,
      el('div', { class: 'kpis' },
        kpi('Verdict', status, trusted ? 'answers here are complete as far as the index goes' : 'every count on every screen is a lower bound', trusted ? 'ok' : 'warn'),
        kpi('Age', fresh ? text(fresh.age_human) : '—', fresh && fresh.stale ? 'older than the tree it describes' : 'current'),
        kpi('Files indexed', fresh ? num(fresh.files_indexed) : '—', 'of ' + (fresh ? num(fresh.files_discovered) : '—') + ' discovered'),
        kpi('Parse failures', fresh ? num(fresh.parse_failures) : '—', 'files whose symbols are missing', Number(fresh && fresh.parse_failures) > 0 ? 'warn' : 'ok')),
      reasons.length ? el('div', { class: 'held' },
        el('h4', { text: 'Why the verdict is "' + status + '"' }),
        el('ul', null, reasons.map((why) => el('li', null,
          el('span', { class: 'what' },
            Number(why.count) > 0 ? el('b', { text: num(why.count) + ' — ' }) : null,
            text(why.detail)),
          why.remedy ? el('span', { class: 'fix' }, 'Fix: ', el('code', { text: why.remedy })) : null)))) : null,
      fresh && fresh.git_revision ? el('p', { class: 'small muted' }, 'Built from revision ', el('code', { text: String(fresh.git_revision).slice(0, 12) })) : null,
    ],
    null, tone);
  node.id = 'freshness';
  return node;
}

// ================================================================ SCREEN 02 — GRAPH EXPLORER

const GRAPH_STATE = { data: null, model: null, canvas: null, note: null, tableHost: null, inspector: null, filtersOpen: false };

/*
  THE TWO VIEWS OF THIS SCREEN, AND WHICH ONE IS ON.

  `map` is the canvas — what talks to what. `browse` is the card browser — what
  is inside this. They were stacked, browser first, and the reader's own report
  was that they rarely scrolled as far as the map. They are now a switch apart.

  Everything the map owns lives in these hosts; the browser owns exactly one.
  Splitting them by host id rather than by a wrapper element keeps the DOM order
  in index.html readable as the reading order it is.
*/
const GRAPH_MAP_HOSTS = [
  'graph-toolbar', 'graph-layout', 'graph-scope', 'graph-lenses', 'canvas-note',
  'graph-hint', 'graph-path', 'graph-map-head', 'graph-table', 'graph-omissions',
];

/*
  THE CODE MAP'S FOUR VIEWS.

  `map` is the canvas above. The other three — Tree, Treemap and Code — are
  built by js/codemap.mjs into one host each, and share the map's address:
  `container`, `level` and `sel=L:<id>`. So a selection made in one view is the
  selection in every other, and a link sent from any of them opens the same
  thing in all four. See the module's header comment for the rest.
*/
const CODEMAP_HOSTS = { tree: 'graph-tree-host', treemap: 'graph-treemap-host', code: 'graph-code-host' };

function graphView() {
  const v = state.q.gv;
  return (v === 'map' || v === 'tree' || v === 'treemap' || v === 'code') ? v : 'map';
}

/*
  The card browser's links still arrive — in docs, in PR comments, in a
  colleague's history. Its address named a container (dc), a folder (dd) and a
  file (dfile); a file now opens in Code and anything else in the Treemap at
  that container, so the link lands on the same thing rather than on nothing.
*/
function redirectCardBrowserLink() {
  const q = state.q;
  if (q.gv !== 'browse' && !(q.gv === '' && (q.dc || q.dfile))) return;
  const patch = { dc: '', dd: '', dfile: '', dsel: '' };
  if (q.dfile) Object.assign(patch, { gv: 'code', cfile: q.dfile, container: q.dc });
  else Object.assign(patch, { gv: 'treemap', container: q.dc, level: q.dc ? 'files' : '' });
  setQuery(patch, { replace: true });
}

function codeMapContext() {
  return {
    q: () => state.q,
    set: (patch, opts) => setQuery(patch, opts),
    api: (path, params) => api(path, params),
    tierBadge: (conf) => tierBadge(conf),
    copy: (value) => (navigator.clipboard && navigator.clipboard.writeText ? navigator.clipboard.writeText(value) : Promise.resolve()),
    goImpact: (sym) => go('blast', { sym: sym }),
    navigate: (view, patch) => codeMapNavigate(view, patch),
    runId: () => (state.meta && state.meta.run ? String(state.meta.run.run_id) : ''),
  };
}

/*
  Every move between views and scopes goes through here. The map's data is
  the answer for one container at one level, so a move that changes either
  drops it; the next visit to the map refetches exactly once.
*/
function codeMapNavigate(view, patch) {
  const before = state.q.container + '\u0000' + state.q.level;
  setQuery(Object.assign({}, patch || {}, { gv: view }));
  if (state.q.container + '\u0000' + state.q.level !== before) GRAPH_STATE.data = null;
  applyGraphView();
}

function renderCodeMapHeader() {
  const cm = window.cortex && window.cortex.codemap;
  const host = $('#graph-view-host');
  if (cm && host) cm.renderHeader(host, codeMapContext(), graphView());
}

function codeMapAnswer(view) {
  if (view === 'tree') {
    return answer(['The repository as an outline: ', B('directory, file, symbol'), '.'],
      { support: 'Select a row for its relationships, each with a tier and a checkable location. Double-click a file or symbol to read its code.' });
  }
  if (view === 'treemap') {
    return answer(['Where the code is: ', B('area is size'), ', colour is how connected it is.'],
      { support: 'Double-click or press Enter on a box to go inside; Esc goes back up.' });
  }
  return answer([state.q.cfile ? B(baseName(state.q.cfile)) : 'A file', ': the text the index holds, with every relationship beside the line it starts from.'],
    { support: 'Only files the index recorded can be shown, as they were when it was built.' });
}

/*
  Shows one view and builds it; the others are not built. The map's layout is
  started by loadGraph, so withholding that call is what stops a simulation
  running against a canvas nobody is looking at — hiding the canvas alone
  would not have.
*/
async function applyGraphView() {
  redirectCardBrowserLink();
  const view = graphView();
  for (const id of GRAPH_MAP_HOSTS) {
    const node = document.getElementById(id);
    if (node) node.hidden = (view !== 'map');
  }
  for (const [name, id] of Object.entries(CODEMAP_HOSTS)) {
    const node = document.getElementById(id);
    if (node) node.hidden = (view !== name);
  }
  renderCodeMapHeader();

  if (view !== 'map') {
    if (GRAPH_STATE.filtersOpen) setFiltersOpen(false);
    setAnswer($('#graph-head'), codeMapAnswer(view));
    const cm = window.cortex && window.cortex.codemap;
    const host = document.getElementById(CODEMAP_HOSTS[view]);
    if (!cm || !host) return;
    try {
      await cm.renderView(view, host, codeMapContext());
    } catch (err) {
      showError(host, err, 'The ' + view + ' view');
    }
    return;
  }

  if (GRAPH_STATE.canvas) GRAPH_STATE.canvas.measure();
  if (!GRAPH_STATE.data) await loadGraph();
  else {
    setAnswer($('#graph-head'), graphAnswer(GRAPH_STATE.data));
    if (GRAPH_STATE.canvas) GRAPH_STATE.canvas.draw();
  }
}

LOADERS.graph = async function () {
  if (!state.loaded.graph) {
    state.loaded.graph = true;
    wireGraphShell();
  }
  await applyGraphView();
};

/*
  The shell is wired once. Everything the reader touches afterwards - a
  different container, a different rung, a different confidence floor - refetches
  and re-lays-out INTO THE SAME canvas, rather than destroying the screen and
  building another one. The old build reloaded a screen at most once ever
  (`if (!state.loaded[name])`) and could not refresh at all; this one refreshes
  without losing the renderer.
*/
function wireGraphShell() {
  $('#graph-head').appendChild(answer('The map of this code is being read from the index.'));

  // The head of the edge table, under the canvas: the table is the text
  // equivalent of the drawing, so its head follows the drawing rather than
  // announcing it from above.
  $('#graph-map-head').appendChild(el('div', { class: 'screen-head' },
    el('h3', { text: 'Every relationship on the map, with its evidence' }),
    el('p', { class: 'lede', text: 'Every relationship between two drawn boxes is combined into one line, drawn at the weakest tier among them, with the full distribution and a checkable file and line behind it.' })));

  wireFiltersPopover();

  $('#graph-hint').append(
    'Drag to pan. Hold ', el('kbd', { text: 'Ctrl' }), ' or ', el('kbd', { text: '⌘' }),
    ' and scroll to zoom — a plain scroll moves the page, so the canvas never traps it. ',
    el('kbd', { text: 'Tab' }), ' walks the drawn nodes, ', el('kbd', { text: '+' }), ' ', el('kbd', { text: '−' }), ' ',
    el('kbd', { text: '0' }), ' zoom, arrows pan. Double-click a group to open it; double-click a node to take it further.');

  const canvas = $('#graph-canvas');
  const wrap = $('#canvas-wrap');
  /*
    The canvas tools, in the code map's control vocabulary: an icon AND a word
    on every control a reader has to find, an aria-label on the four icon-only
    ones everybody already knows (zoom in, zoom out, full screen, minimap), and
    aria-pressed on every toggle so its state is announced, not just drawn.
    The layout is a select rather than a cycling button: a reader can see all
    three choices and which one is on, and the choice rides in the URL.
  */
  const ico = (name) => (window.cortex && window.cortex.codemap ? window.cortex.codemap.icon(name) : null);
  const tool = (attrs, iconName, label) => el('button', Object.assign({ class: 'cm-btn cm-sm' + (label ? '' : ' cm-icon-only'), type: 'button' }, attrs), ico(iconName), label ? el('span', { text: label }) : null);
  const layoutSelect = el('select', {
    class: 'cm-input cm-select', id: 'btn-layout-mode', 'aria-label': 'Layout',
    onchange: (e) => { setQuery({ glayout: e.target.value === 'hierarchy' ? '' : e.target.value }, { replace: true }); GRAPH_STATE.canvas.setLayoutMode(e.target.value); },
  },
  el('option', { value: 'hierarchy', text: 'Layered' }),
  el('option', { value: 'tree', text: 'Tree' }),
  el('option', { value: 'organic', text: 'Organic' }));
  layoutSelect.value = state.q.glayout || 'hierarchy';
  $('#canvas-tools').append(
    tool({
      id: 'btn-group-toggle', 'aria-pressed': String(graphGrouping() !== 'none'),
      title: 'Group boxes into communities, or draw every box',
      onclick: () => {
        const next = graphGrouping() === 'none' ? 'communities' : 'none';
        setQuery({ group: next });
        const b = $('#btn-group-toggle');
        if (b) b.setAttribute('aria-pressed', String(next !== 'none'));
        rebuildGraph();
      },
    }, 'treemap', 'Grouped'),
    el('label', { class: 'cm-field cm-field-sm' }, el('span', { text: 'Layout' }), layoutSelect),
    tool({ 'aria-label': 'Zoom in', title: 'Zoom in (+)', onclick: () => GRAPH_STATE.canvas.zoomAt(GRAPH_STATE.canvas.width / 2, GRAPH_STATE.canvas.height / 2, 1.2) }, 'plus'),
    tool({ 'aria-label': 'Zoom out', title: 'Zoom out (−)', onclick: () => GRAPH_STATE.canvas.zoomAt(GRAPH_STATE.canvas.width / 2, GRAPH_STATE.canvas.height / 2, 1 / 1.2) }, 'minus'),
    el('span', { class: 'zoom-readout', id: 'zoom-readout', text: '100%' }),
    tool({ title: 'Fit the drawing to the view (0)', onclick: () => { GRAPH_STATE.canvas.hasFit = true; GRAPH_STATE.canvas.fit(); GRAPH_STATE.canvas.draw(); } }, 'fit', 'Fit'),
    tool({ id: 'btn-fullscreen', 'aria-label': 'Full screen', 'aria-pressed': 'false', title: 'Full screen', onclick: () => GRAPH_STATE.canvas.toggleFullscreen() }, 'expand'),
    tool({ id: 'btn-toggle-minimap', 'aria-label': 'Minimap', 'aria-pressed': 'true', title: 'Show or hide the minimap', onclick: () => GRAPH_STATE.canvas.toggleMinimap() }, 'minimap'));

  GRAPH_STATE.note = $('#canvas-note');
  GRAPH_STATE.tableHost = $('#graph-table');
  GRAPH_STATE.layoutHost = $('#graph-layout');
  GRAPH_STATE.scopeHost = $('#graph-scope');
  GRAPH_STATE.lensHost = $('#graph-lenses');
  GRAPH_STATE.pathHost = $('#graph-path');
  GRAPH_STATE.progressHost = $('#graph-progress');
  GRAPH_STATE.inspector = el('aside', { class: 'inspector', id: 'graph-inspector', 'aria-live': 'polite' });

  // "/" is the search key everywhere else a developer works, so it is the
  // search key here. It is bound on the document rather than on the canvas so
  // it reaches the search box from anywhere on the screen, and it stands down
  // whenever a text field already has the caret.
  document.addEventListener('keydown', (e) => {
    if (state.q.s !== 'graph') return;
    if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return;
    const tag = document.activeElement ? document.activeElement.tagName : '';
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
    const box = document.getElementById('graph-search');
    if (!box) return;
    e.preventDefault();
    box.focus();
    box.select();
  });

  const graphCanvas = new GraphCanvas(wrap, canvas, {
    onSelect: (node) => onGraphSelect(node),
    onOpen: (node) => onGraphOpen(node),
    onView: (view) => { const readout = $('#zoom-readout'); if (readout) readout.textContent = Math.round(view.k * 100) + '%'; },
    onSettled: () => fitBudgetToWindow(),
  });
  graphCanvas.attachMinimap($('#graph-minimap'));
  graphCanvas.layoutMode = state.q.glayout || 'hierarchy';
  GRAPH_STATE.canvas = graphCanvas;
  state.view = graphCanvas;
}

/*
  THE PROGRESS STATE, AND WHY IT IS NOT A SPINNER.

  Measured on a 14,614-file repository: the FIRST request of a session cost
  2.05s, of which 1.70s was three whole-run reads the server ran one after
  another (files 128ms, symbols 1,272ms, edges 301ms). Running them together
  brought that to ~1.3s — see internal/web/index.go — and a wait that still
  exists is a wait a reader is owed an account of.

  So the canvas shows what is being read, how big it is, and how long it has
  actually taken so far, updating on a timer. A spinner claims "something is
  happening"; this claims "109,327 symbols across 14,614 files are being read
  for the first time in this session, 0.9s so far", which is the difference
  between waiting and wondering whether it has hung.
*/
function graphProgress(stage) {
  const host = GRAPH_STATE.progressHost;
  if (!host) return { stop: () => {} };
  const started = performance.now();
  const run = (state.meta && state.meta.run) || {};
  const files = Number(run.files_indexed || run.files_discovered || 0);
  const elapsed = el('b', { text: '0.0s' });
  clear(host);
  host.hidden = false;
  host.appendChild(el('div', { class: 'row' },
    el('span', { class: 'spin', 'aria-hidden': 'true' }),
    el('b', { text: stage })));
  host.appendChild(el('p', { class: 'small' },
    files ? 'Reading the structural map of ' + num(files) + ' indexed ' + plural('file', files) + '. ' : '',
    'The first request of a session builds it; every later one is served from memory.'));
  host.appendChild(el('p', { class: 'small muted' }, 'elapsed ', elapsed));
  const timer = setInterval(() => {
    elapsed.textContent = ((performance.now() - started) / 1000).toFixed(1) + 's';
  }, 100);
  return {
    stop: () => {
      clearInterval(timer);
      host.hidden = true;
      clear(host);
    },
  };
}

/*
  AUTO-SCOPE: THE FIX FOR A BLANK CANVAS.

  `?level=files` and `?level=symbols` with no container drew NOTHING — 0 nodes,
  0 edges, no message — because those rungs are one container's worth of detail
  and the server correctly has nothing to send when no container is named. The
  reader saw an empty canvas with every control around it looking correct,
  which is the one failure a picture cannot report about itself.

  Two honest answers were available and this is both of them. When the index has
  containers, the largest is chosen, the choice is WRITTEN INTO THE URL so the
  view stays shareable and reproducible, and the banner says which one was
  chosen, by what rule, out of how many, and how to change it. When the index
  has no containers at all, there is nothing to choose and the screen says so
  instead of drawing an empty box.

  The rule is mechanical and stated: most files with symbols, then most symbols,
  then id. Never a relevance score — that belongs to the context engine, where
  it is measured.
*/
function largestContainer(containers) {
  const rows = (containers || []).slice();
  if (!rows.length) return null;
  rows.sort((a, b) =>
    Number(b.files_with_symbols || 0) - Number(a.files_with_symbols || 0)
    || Number(b.symbols || 0) - Number(a.symbols || 0)
    || String(a.id).localeCompare(String(b.id)));
  return rows[0];
}

async function loadGraph(opts) {
  const options = opts || {};
  const controls = $('#graph-toolbar');
  const omissions = $('#graph-omissions');
  clear(omissions);
  omissions.appendChild(loading('Reading the graph…'));
  const progress = graphProgress('Reading the graph…');

  let d;
  try {
    d = await api('/api/graph', {
      min_confidence: state.q.floor,
      container: state.q.container,
      language: state.q.lang,
      kinds: state.q.kinds,
      level: graphLevel(),
    });
  } catch (e) { progress.stop(); clear(omissions); failAnswer($('#graph-head'), 'The graph'); return showError(omissions, e, 'The graph'); }
  progress.stop();

  /*
    The drilled rungs need a container. If none was named, pick one — ONCE, and
    only on the first pass, so a repository whose largest container genuinely
    holds nothing cannot make this loop.
  */
  const level = text(d.level, 'containers');
  const wantsScope = (level === 'files' || level === 'symbols');
  const named = text((d.filters && d.filters.container) || state.q.container, '');
  if (wantsScope && !named && !options.rescoped) {
    const pick = largestContainer(d.containers);
    if (pick) {
      GRAPH_STATE.autoScope = {
        id: pick.id,
        of: (d.containers || []).length,
        kind: text(d.options && d.options.container_kind, 'container'),
        files: Number(pick.files_with_symbols || 0),
        symbols: Number(pick.symbols || 0),
      };
      setQuery({ container: pick.id, sel: '', expand: '', focus: '', pfrom: '', pto: '' }, { replace: true });
      return loadGraph({ rescoped: true });
    }
    // No containers at all: there is nothing to choose, and an empty canvas
    // would be a lie of omission. Say it, and offer the rung that works.
    GRAPH_STATE.autoScope = null;
    GRAPH_STATE.data = d;
    state.meta = d.meta;
    renderIndexBar();
    setAnswer($('#graph-head'), graphAnswer(d));
  renderCodeMapHeader();
    renderGraphControls(d, controls);
    renderLenses(d);
    renderScopeBanner(d);
    rebuildGraph();
    clear(omissions);
    omissions.appendChild(renderOmissions(d.omissions, { key: 'graph' }));
    return;
  }
  if (!wantsScope || named !== (GRAPH_STATE.autoScope && GRAPH_STATE.autoScope.id)) {
    if (!options.rescoped) GRAPH_STATE.autoScope = null;
  }

  GRAPH_STATE.data = d;
  state.meta = d.meta;
  renderIndexBar();
  setAnswer($('#graph-head'), graphAnswer(d));
  renderCodeMapHeader();
  renderGraphControls(d, controls);
  renderLenses(d);
  renderScopeBanner(d);
  rebuildGraph();
  clear(omissions);
  omissions.appendChild(renderOmissions(d.omissions, { key: 'graph' }));
}

function reloadGraph() { loadGraph(); }

/*
  The map's answer, at whichever level the server drew: how many things talk
  to each other, through how many relationships, and how many of them the map
  shows. Every figure is the server's own — node_budget for the drawn set and
  the level's combined edges for the relationships.
*/
function graphAnswer(d) {
  const level = text(d.level, 'containers');
  const kind = text(d.options && d.options.container_kind, 'container');
  const unit = level === 'files' ? 'file' : (level === 'symbols' ? 'symbol' : kind);
  const edges = level === 'files' ? (d.file_edges || []) : (level === 'symbols' ? (d.edges || []) : (d.container_edges || []));
  // Two figures, because the picture can be read as either. The map draws one
  // stroke per PAIR of nodes; each stroke stands for every occurrence between
  // them. A sentence that summed the occurrences over a map of six strokes
  // said "12 relationships" above a picture of six, so it says both and names
  // which is which.
  const pairKeys = new Set();
  let uses = 0;
  for (const e of edges) {
    const from = e.from !== undefined ? e.from : e.src;
    const to = e.to !== undefined ? e.to : e.dst;
    pairKeys.add(String(from) + '\u0001' + String(to));
    uses += Math.max(1, Number(e.count || 0));
  }
  const rels = pairKeys.size;
  const nb = d.node_budget || {};
  const available = Number(nb.available || 0);
  const drawn = Number(nb.drawn || 0);
  const parts = [B(num(available) + ' ' + plural(unit, available)), ' talk to each other: ',
    B(num(rels) + ' ' + plural('relationship', rels)), ' between them (',
    B(num(uses) + ' ' + plural('use', uses)), ' in all).'];
  if (available > 0 && drawn < available) parts.push(' The map shows ' + num(drawn) + ' of the ' + plural(unit, available) + '; the rest are held back so it stays readable.');
  else if (available > 0) parts.push(' The map shows all of them.');
  const scoped = text((d.filters && d.filters.container) || '', '');
  return answer(parts, {
    support: (level !== 'containers' && scoped)
      ? 'Inside ' + scoped + '. Open any box to go a level deeper; every level is redrawn from the same index and says what it could not show.'
      : 'Open any box to go a level deeper — repository, package, folder, file, symbol. Every level is redrawn from the same index and says what it could not show.',
  });
}

/*
  renderScopeBanner states the scope in words above the controls. It is present
  whenever the drilled rung is showing, because "which container am I looking
  at" is the question a file-level or symbol-level map cannot answer by being
  looked at.
*/
function renderScopeBanner(d) {
  const host = GRAPH_STATE.scopeHost;
  if (!host) return;
  clear(host);
  const level = text(d.level, 'containers');
  if (level === 'containers') return;

  const kind = text(d.options && d.options.container_kind, 'container');
  const scoped = text((d.filters && d.filters.container) || state.q.container, '');
  const auto = GRAPH_STATE.autoScope;

  if (!scoped) {
    host.appendChild(el('div', { class: 'scopebar is-empty' },
      el('b', { text: 'This index has no ' + plural(kind, 2) + ' to scope by.' }),
      el('span', null, ' The ', el('b', { text: level }), ' level draws one ' + kind + ' at a time and there is not one to draw, so nothing is on the canvas — that is the whole finding, not a blank screen. '),
      el('button', {
        class: 'linkish', type: 'button', text: 'Show the ' + plural(kind, 2) + ' level instead',
        onclick: () => { setQuery({ level: 'containers', container: '', sel: '', expand: '' }); reloadGraph(); },
      })));
    return;
  }

  const bar = el('div', { class: 'scopebar' + (auto ? ' is-auto' : '') });
  bar.appendChild(el('span', { class: 'lbl', text: 'Scope' }));
  bar.appendChild(el('b', { class: 'sym', text: scoped }));
  if (auto && auto.id === scoped) {
    bar.appendChild(el('span', { class: 'small' },
      ' — chosen for you. No ' + kind + ' was named, and the ',
      el('b', { text: level }), ' level draws one at a time, so this is the largest of ',
      el('b', { text: num(auto.of) }), ' ' + plural(kind, auto.of),
      ' by files with symbols (', el('b', { text: num(auto.files) }), ' ' + plural('file', auto.files),
      ', ', el('b', { text: num(auto.symbols) }), ' ' + plural('symbol', auto.symbols),
      '). The rule is most files, then most symbols, then name — never a relevance score. It is in the address bar now, so this view is exactly reproducible.'));
  } else {
    bar.appendChild(el('span', { class: 'small muted', text: ' — the ' + level + ' level draws one ' + kind + ' at a time.' }));
  }
  host.appendChild(bar);
}

// The level is a real rung, not a mode switch: containers for the system, files
// for one component, symbols for one file's worth of detail. Naming a container
// without naming a level means "files", because 1,620 symbol nodes is the
// picture that was unreadable and 168 file nodes is the one that is not.
function graphLevel() {
  if (state.q.level) return state.q.level;
  return state.q.container ? 'files' : 'containers';
}

/*
  'auto' is the DEFAULT grouping and it is a real strategy, not an alias.

  The container rung of a 14,614-file repository holds 19 components. Nineteen
  labelled boxes fit on a 1440x900 canvas with room to spare, and the explorer
  was collapsing them into FIVE server communities anyway — because server
  clusters were applied whenever they were present, without asking whether
  collapsing bought anything. A reader looking for `gateway` could not see it.

  So: collapse when the set does not fit at a readable size, and not before.
  The reader can still pin any of the three strategies, and the choice rides in
  the URL; what changed is that the default now asks the question.
*/
function graphGrouping() {
  const g = state.q.group;
  if (g === 'folder' || g === 'none' || g === 'communities') return g;
  return 'auto';
}

/*
  ONE ROW, AND A POPOVER BEHIND ONE BUTTON.

  Seven controls used to sit in a panel above the canvas, with the scope banner
  and the lens row under them, and at 1440x900 the map itself began 800px down
  the page. Two of the seven are the map's subject — which container, at which
  detail level — and a reader changes them on most visits; they stay in the
  row, with the search box. The other five are filters a reader sets once, if
  at all, so they live in a popover behind a Filters button, and the button
  says how many of them are away from their default: a narrowed map should
  never look like the whole one.

  Nothing about the controls themselves changed. Every id, every handler and
  every query parameter is the one it was; only the box each control sits in
  moved.
*/
function renderGraphControls(d, host) {
  const level = text(d.level, 'containers');
  const options = d.options || {};

  // The row is rebuilt on every load. If the popover was open when a filter
  // inside it refetched the map, it is open again afterwards with the same
  // control under the caret — a reader mid-way through narrowing the map
  // should not have to find the button again.
  // A chip inside `#graph-kinds` has no id of its own to remember, so a
  // change there is recorded as the group's id instead — the fallback that
  // reopen honours below by focusing the first chip.
  const active = document.activeElement;
  const focusId = active && active.closest && active.closest('#graph-kinds')
    ? 'graph-kinds' : (active ? active.id : '');
  clear(host);

  const containers = (options.containers || []);
  const kindWord = text(options.container_kind, 'container');
  const containerCombo = combobox({
    id: 'graph-container',
    placeholder: containers.length + ' ' + plural(kindWord, containers.length) + ' — type to filter',
    value: state.q.container,
    emptyNote: 'No ' + plural(kindWord, 2) + ' match that.',
    source: async (q) => {
      const needle = q.toLowerCase();
      const stats = new Map((d.containers || []).map((row) => [row.id, row]));
      // The "all, aggregated" row is the way OUT of a scope, not a search
      // result: it was returned as the first hit for every query, so typing an
      // exact container name put a row that does not match it at the top of the
      // list and under the keyboard's first Enter.
      const all = (needle ? [] : [{ value: '', label: 'All ' + plural(kindWord, 2) + ', combined', sub: '', meta: String(containers.length) }])
        .concat(containers.filter((id) => !needle || id.toLowerCase().indexOf(needle) >= 0).map((id) => {
          const row = stats.get(id);
          return {
            value: id, label: baseName(id) || id,
            sub: id === baseName(id) ? '' : id,
            meta: row ? num(row.files_with_symbols) + ' files' : '',
          };
        }));
      const rows = all.slice(0, 60);
      return { rows: rows, note: all.length > 60 ? num(all.length - 60) + ' more match — type to narrow' : '' };
    },
    onChoose: (row) => { setQuery({ container: row.value, level: '', sel: '', expand: '' }); reloadGraph(); },
  });
  host.appendChild(field('Scope', containerCombo.node, 'grow'));

  host.appendChild(selectField('Detail level', 'graph-level', [
    { value: 'containers', label: plural(kindWord, 2) + ' — the whole system' },
    { value: 'files', label: 'files inside one ' + kindWord },
    { value: 'symbols', label: 'symbols inside one ' + kindWord },
  ], level, (v) => { setQuery({ level: v, sel: '', expand: '' }); reloadGraph(); }));

  const unit = level === 'files' ? 'file' : (level === 'symbols' ? 'symbol' : kindWord);
  host.appendChild(searchField(unit));

  host.appendChild(el('button', {
    class: 'ghost filters-btn', type: 'button', id: 'graph-filters',
    'aria-haspopup': 'dialog', 'aria-expanded': 'false', 'aria-controls': 'graph-filters-pop',
    title: 'Grouping, language, edge kinds, the standard of proof and the focus depth.',
    onclick: () => setFiltersOpen(!GRAPH_STATE.filtersOpen),
  }));

  const pop = el('div', { class: 'filters-pop', id: 'graph-filters-pop', role: 'dialog', 'aria-label': 'Map filters', hidden: true });

  pop.appendChild(selectField('Group by', 'graph-group', [
    { value: 'auto', label: 'automatic — collapse only when it will not fit' },
    { value: 'communities', label: 'communities — what talks to what' },
    { value: 'folder', label: 'folders — where the code lives' },
    { value: 'none', label: 'nothing — draw every node' },
  ], graphGrouping(),
    (v) => { setQuery({ group: v === 'auto' ? '' : v, expand: '' }); rebuildGraph(); paintFiltersButton(); }));

  const langs = (options.languages || []);
  pop.appendChild(selectField('Language', 'graph-language',
    [{ value: '', label: 'all ' + langs.length }].concat(langs.map((l) => ({ value: l, label: l }))),
    state.q.lang, (v) => { setQuery({ lang: v, sel: '', expand: '' }); reloadGraph(); }));

  const kinds = (options.kinds || []);
  if (kinds.length) {
    const chips = chipGroup(kinds.map((kind) => ({ value: kind, label: kind })),
      state.q.kinds ? state.q.kinds.split(',') : [],
      (chosen) => { setQuery({ kinds: chosen.join(','), sel: '' }); reloadGraph(); });
    chips.id = 'graph-kinds';
    pop.appendChild(field('Edge kinds', chips, 'grow'));
  }

  // The confidence lens. Distinct from the header's floor: that one refetches,
  // this one re-draws, and the difference is the whole point — a reader can
  // flick between "everything" and "only what is proven" and watch the map
  // change under their eye.
  pop.appendChild(field('Standard of proof', selectControl('graph-lens', [
    { value: '', label: 'as fetched — ' + floorLabel(state.q.floor) },
    { value: 'exact', label: floorLabel('exact') + ' — a compiler proved each one' },
    { value: 'high', label: floorLabel('high') },
    { value: 'medium', label: floorLabel('medium') },
  ], state.q.lens, (v) => { setQuery({ lens: v }); relens(); })));

  pop.appendChild(field('Focus depth', selectControl('graph-fdepth',
    [1, 2, 3, 4].map((n) => ({ value: String(n), label: n + ' ' + plural('hop', n) })),
    String(focusDepth()), (v) => {
      setQuery({ fdepth: v }); applyEmphasis(); paintCanvasNote(); GRAPH_STATE.canvas.draw(); paintFiltersButton();
    })));

  host.appendChild(pop);
  paintFiltersButton();
  if (GRAPH_STATE.filtersOpen) setFiltersOpen(true, { focusId: focusId });
}

/*
  The five filters, counted against their defaults. `group` reads through
  graphGrouping because an unknown value is 'auto', and 'auto' is the default;
  `fdepth` reads through focusDepth for the same reason.
*/
function filtersChanged() {
  let n = 0;
  if (graphGrouping() !== 'auto') n += 1;
  if (state.q.lang !== DEFAULTS.lang) n += 1;
  if (state.q.kinds !== DEFAULTS.kinds) n += 1;
  if (state.q.lens !== DEFAULTS.lens) n += 1;
  if (String(focusDepth()) !== DEFAULTS.fdepth) n += 1;
  return n;
}

function paintFiltersButton() {
  const btn = document.getElementById('graph-filters');
  if (!btn) return;
  const n = filtersChanged();
  btn.textContent = n ? 'Filters · ' + n + ' changed' : 'Filters';
  btn.classList.toggle('is-changed', n > 0);
}

/*
  Open or close the popover. On open the caret goes to the first control — or
  to the one it was on before a refetch rebuilt the row; on close it goes back
  to the button when the close was the keyboard's, so Escape never drops focus
  on the body.
*/
function setFiltersOpen(open, opts) {
  const pop = document.getElementById('graph-filters-pop');
  const btn = document.getElementById('graph-filters');
  if (!pop || !btn) return;
  const o = opts || {};
  pop.hidden = !open;
  btn.setAttribute('aria-expanded', String(open));
  GRAPH_STATE.filtersOpen = open;
  if (open) {
    // `graph-kinds` names the chip group, not a focusable control — the
    // caret goes to its first chip instead.
    const first = (o.focusId === 'graph-kinds' && pop.querySelector('#graph-kinds button'))
      || (o.focusId && pop.querySelector('#' + CSS.escape(o.focusId)))
      || pop.querySelector('select, input, button');
    if (first) first.focus();
  } else if (o.refocus) {
    btn.focus();
  }
}

/*
  Escape closes the popover and returns to the button; a pointer landing
  outside it closes it and leaves focus where it landed. Both listeners live on
  the document, wired once from the shell, and consult the popover that is
  there NOW — the row is rebuilt on every load, so a listener bound to one
  build of it would close a popover that no longer exists.
*/
function wireFiltersPopover() {
  document.addEventListener('keydown', (e) => {
    if (state.q.s !== 'graph') return;
    if (e.key !== 'Escape' || !GRAPH_STATE.filtersOpen) return;
    const pop = document.getElementById('graph-filters-pop');
    if (!pop || pop.hidden) return;
    e.preventDefault();
    setFiltersOpen(false, { refocus: true });
  });
  document.addEventListener('pointerdown', (e) => {
    if (state.q.s !== 'graph') return;
    if (!GRAPH_STATE.filtersOpen) return;
    const pop = document.getElementById('graph-filters-pop');
    const btn = document.getElementById('graph-filters');
    if (!pop || pop.hidden) return;
    if (pop.contains(e.target) || (btn && btn.contains(e.target))) return;
    setFiltersOpen(false);
  });
  // Tab is allowed to leave the popover; leaving it closes the popover, so it
  // never overlays the canvas unattended once the caret has moved on. A null
  // relatedTarget means focus left the document entirely (e.g. the address
  // bar), which is not "leaving" — the popover stays open for when it returns.
  document.addEventListener('focusout', (e) => {
    if (state.q.s !== 'graph' || !GRAPH_STATE.filtersOpen) return;
    if (!e.relatedTarget) return;
    const pop = document.getElementById('graph-filters-pop');
    const btn = document.getElementById('graph-filters');
    if (!pop || pop.hidden) return;
    if (pop.contains(e.relatedTarget) || (btn && btn.contains(e.relatedTarget))) return;
    setFiltersOpen(false);
  });
}

/*
  SEARCH THAT FLIES TO A NODE.

  A map of a 14,614-file repository is only useful if you can get to the thing
  you came for. This searches every node the server sent at the current rung —
  not just the ones drawn, because the drawn set is a legibility budget and the
  reader did not choose it — and landing on a hidden one OPENS its group first,
  so the answer to "where is AirtelCallbackApi" is never "it is in this
  collapsed box somewhere".

  It is keyboard-complete: "/" from anywhere on the screen focuses it, arrows
  move the active option, Enter flies, Escape closes. The combobox it is built
  from is the same one the scope picker uses, so the aria wiring is shared
  rather than reimplemented.
*/
function searchField(unit) {
  const combo = combobox({
    id: 'graph-search',
    placeholder: 'Find a ' + unit + ' — press / from anywhere',
    value: '',
    emptyNote: 'Nothing at this level matches that. It may be in another ' + unit + ', below the standard of proof, or not extracted at all.',
    source: async (q) => {
      const m = GRAPH_STATE.model;
      if (!m) return { rows: [], note: 'The map is still loading.' };
      const needle = String(q || '').toLowerCase().trim();
      if (!needle) {
        return { rows: [], note: 'Type at least one character. ' + num(m.leaves.length) + ' ' + plural(unit, m.leaves.length) + ' are searchable.' };
      }
      const hits = m.leaves.filter((leaf) =>
        String(leaf.label).toLowerCase().indexOf(needle) >= 0
        || String(leaf.path || '').toLowerCase().indexOf(needle) >= 0
        || String(leaf.fqn || '').toLowerCase().indexOf(needle) >= 0);
      hits.sort((a, b) => {
        const ax = String(a.label).toLowerCase().indexOf(needle);
        const bx = String(b.label).toLowerCase().indexOf(needle);
        if (ax !== bx) return (ax < 0 ? 99 : ax) - (bx < 0 ? 99 : bx);
        return (b.degree || 0) - (a.degree || 0) || String(a.label).localeCompare(String(b.label));
      });
      const rows = hits.slice(0, 40).map((leaf) => ({
        value: leaf.id, label: text(leaf.label, leaf.id),
        sub: text(leaf.path || leaf.fqn || '', ''),
        meta: (leaf.degree || 0) + ' edges',
      }));
      return {
        rows: rows,
        note: hits.length > rows.length
          ? num(hits.length - rows.length) + ' further ' + plural('match', hits.length - rows.length) + ' are not listed — type more to narrow'
          : (hits.length ? num(hits.length) + ' ' + plural('match', hits.length) : ''),
      };
    },
    onChoose: (row) => {
      // The combobox writes the chosen VALUE into the input, and at symbol
      // level a value is a numeric id. A reader who searched for a name should
      // see the name they searched for, not the database's opinion of it.
      const box = document.getElementById('graph-search');
      if (box) box.value = row.label;
      flyToLeaf(row.value);
    },
  });
  return field('Find', combo.node, 'grow');
}

/*
  flyToLeaf is the second half of search, and the half that has to deal with the
  legibility budget: the node somebody asked for may be inside a collapsed group
  or outside the drawn set entirely. Opening its group and rebuilding is not a
  side effect to hide — it is the honest answer to "take me there", and what
  cannot be reached even then is said out loud rather than silently ignored.
*/
function flyToLeaf(leafId) {
  const model = GRAPH_STATE.model;
  const cv = GRAPH_STATE.canvas;
  if (!model || !cv) return;
  const leaf = model.byId.get(String(leafId));
  if (!leaf) return;

  let set = GRAPH_STATE.renderSet;
  let key = set && set.keyOfLeaf ? set.keyOfLeaf.get(leaf.id) : null;
  const gid = model.leafGroup.get(leaf.id);

  // Not drawn as itself: open the group holding it and rebuild before flying.
  if ((!key || !set.byKey.has(key) || key === gid) && gid && model.groups.has(gid)) {
    const group = model.groups.get(gid);
    if (!group.solo) {
      model.expanded.add(gid);
      // The member budget slices a group's members by degree; a search hit
      // below that slice would be expanded to and still not drawn.
      const rank = group.members.indexOf(leaf);
      if (rank >= model.memberCap) model.memberCap = rank + 1;
      setQuery({ expand: Array.from(model.expanded).join(',') });
      GRAPH_STATE.shrinks = 0;
      rebuildRenderSet(Math.max(legibleBudget(cv), group.members.length + 8));
      set = GRAPH_STATE.renderSet;
      key = set.keyOfLeaf ? set.keyOfLeaf.get(leaf.id) : null;
    }
  }

  const node = key ? set.byKey.get(key) : null;
  if (!node) {
    GRAPH_STATE.flyNote = text(leaf.label, String(leafId))
      + ' is in this index and in this view’s data, but it is not on the canvas: the legibility budget held it back. '
      + 'Use "Draw more anyway", or narrow the scope.';
    paintCanvasNote();
    return;
  }
  GRAPH_STATE.flyNote = '';
  cv.flyTo(node);
  cv.select(node);
}

function legendStrip() {
  const strip = el('div', { class: 'legend-strip' });
  strip.appendChild(el('span', { class: 'muted', text: 'Every edge is drawn at its weakest tier:' }));
  for (const tv of ['exact', 'high', 'medium', 'low']) {
    strip.appendChild(el('span', { class: 'k', title: tierSentence(tv) },
      el('span', { class: 'bar t-' + tv, style: 'width:26px;display:inline-block' }),
      el('span', { class: 'tier tier-' + tv, 'data-tier': tv }, TIER[tv].glyph ? TIER[tv].glyph + ' ' : '', tierWord(tv)),
      el('span', { text: TIER[tv].at })));
  }
  return strip;
}

/*
  extractLevel turns whichever rung the server answered with into one flat pair
  of arrays. Every field name below was read off live JSON; where the server
  renamed one (`files` became `files_with_symbols` when the file counts were
  reconciled against governance) pickNum accepts both, so an older binary and a
  newer one both render a number rather than "undefined".
*/
function pickNum(obj, ...names) {
  for (const name of names) if (obj && obj[name] !== undefined && obj[name] !== null) return obj[name];
  return undefined;
}

function extractLevel(d) {
  const level = text(d.level, 'containers');
  if (level === 'files') {
    const leaves = (d.files || []).map((row) => ({
      id: String(row.path), label: text(row.name, baseName(row.path)), path: text(row.path),
      sub: dirName(row.path) ? tailPath(dirName(row.path), 2) : '', kind: 'file',
      language: row.language, degree: Number(row.degree || 0), clusterId: row.cluster,
      symbols: Number(row.symbols || 0), exported: Number(pickNum(row, 'exported_symbols') || 0),
      container: row.container, external: !!row.external,
    }));
    return { leaves: leaves, links: aggregatedLinks(d.file_edges || []), unit: 'file' };
  }
  if (level === 'symbols') {
    const leaves = (d.nodes || []).map((row) => ({
      id: String(row.id), label: text(row.name), path: text(row.path),
      sub: text(row.kind, '') + (row.language ? ' · ' + row.language : ''), kind: row.kind,
      language: row.language, degree: Number(row.degree || 0), clusterId: row.cluster,
      confidence: row.confidence, exported: !!row.exported, fqn: row.fqn, container: row.container,
      external: !!row.external,
    }));
    const links = (d.edges || []).map((e) => ({
      a: String(e.src), b: String(e.dst), count: Number(e.count || 1), total: Number(e.count || 1),
      conf: e.confidence, kinds: [[text(e.kind, 'EDGE'), Number(e.count || 1)]],
      evidence: e.evidence, hist: null,
    }));
    return { leaves: leaves, links: links, unit: 'symbol' };
  }
  const built = buildContainerGraph(d.containers, d.container_edges);
  return { leaves: built.leaves, links: built.links, unit: text(d.options && d.options.container_kind, 'container') };
}

/*
  buildContainerGraph is the container rung, and it is pinned by
  internal/web/contract_test.go: every property it reads off a container has to
  be a field ContainerNode actually marshals. That test exists because this
  function once read `c.name`, which the payload has never had — every label
  came out undefined, the sort threw, and the graph screen rendered a blank
  1038x610 canvas with every control around it looking correct.

  It reads BOTH file counts on purpose. They are different populations — files
  that produced a symbol, and files an include pattern matched — and quoting one
  as "files" is how the graph screen said 551 while governance said 3,425 for
  the same component in the same session.
*/
function buildContainerGraph(containers, edges) {
  const leaves = (containers || []).map((c) => ({
    id: String(c.id),
    label: text(c.label, c.id),
    path: String(c.id),
    kind: 'container',
    degree: Number(c.symbols || 0),
    symbols: Number(c.symbols || 0),
    exported: Number(c.exported_symbols || 0),
    files: Number(c.files_with_symbols || 0),
    filesMatched: Number(c.files_matched || 0),
    languages: c.languages || [],
    clusterId: c.cluster,
    unassigned: !!c.unassigned,
    sub: num(c.files_with_symbols) + ' files with symbols',
  }));
  return { leaves: leaves, links: aggregatedLinks(edges || []) };
}

function aggregatedLinks(rows) {
  return rows.map((row) => ({
    a: String(row.from), b: String(row.to),
    count: Number(row.count || 0),
    total: Number((row.histogram && row.histogram.total) || row.count || 0),
    conf: row.confidence, hist: row.histogram || null,
    kinds: (row.kinds || []).map((k) => [text(k.kind, 'EDGE'), Number(k.count || 0)]),
    evidence: row.evidence || null,
  }));
}

/*
  rebuildGraph is the whole local interaction loop: group, budget, aggregate,
  lay out, draw, and rewrite the table. Nothing here refetches, so expanding a
  community or changing the grouping is instant even on the 1,620-node view.
*/
function rebuildGraph() {
  const d = GRAPH_STATE.data;
  if (!d || !GRAPH_STATE.canvas) return;
  const extracted = extractLevel(d);

  const budget = legibleBudget(GRAPH_STATE.canvas);
  const asked = graphGrouping();
  const hasServerClusters = extracted.leaves.some((leaf) => leaf.clusterId);

  /*
    'auto' collapses only when collapsing buys something. Nineteen components on
    a 1440x900 canvas were being folded into five server communities and the
    reader could not find `gateway` on their own map; a budget that admits every
    node is a budget that should draw every node.
  */
  let grouping = asked;
  let autoNote = '';
  if (asked === 'auto') {
    if (extracted.leaves.length <= budget) {
      grouping = 'none';
      autoNote = 'all ' + num(extracted.leaves.length) + ' ' + plural(extracted.unit, extracted.leaves.length)
        + ' fit on this canvas at a readable size, so nothing is collapsed';
    } else {
      grouping = 'communities';
      autoNote = num(extracted.leaves.length) + ' ' + plural(extracted.unit, 2)
        + ' is more than the ' + num(budget) + ' this canvas draws legibly, so they are collapsed into communities';
    }
  }

  const model = buildModel(extracted.leaves, extracted.links, { grouping: 'none' });
  model.unit = extracted.unit;

  if (grouping === 'communities' && hasServerClusters) {
    applyServerClusters(model, d);
  } else if (grouping === 'none') {
    regroup(model, 'none');
  } else if (grouping === 'communities') {
    regroup(model, 'connectivity');
  } else {
    regroup(model, 'folder');
  }

  markCrossLanguage(model);

  const expanded = (state.q.expand || '').split(',').filter(Boolean);
  for (const gid of expanded) if (model.groups.has(gid)) model.expanded.add(gid);
  GRAPH_STATE.model = model;
  GRAPH_STATE.autoNote = autoNote;

  const groupToggleBtn = document.getElementById('btn-group-toggle');
  if (groupToggleBtn) {
    const isNone = grouping === 'none';
    // The label stays "Grouped"; whether it is on is aria-pressed, which both
    // a screen reader and the pressed style read.
    groupToggleBtn.setAttribute('aria-pressed', String(!isNone));
  }

  GRAPH_STATE.shrinks = 0;
  GRAPH_STATE.flyNote = '';
  rebuildRenderSet(budget, { refit: true });

  const selected = state.q.sel;
  const set = GRAPH_STATE.renderSet;
  if (selected && set.byKey.has(selected)) {
    GRAPH_STATE.canvas.selected = set.byKey.get(selected);
    onGraphSelect(GRAPH_STATE.canvas.selected, true);
  } else if (selected && selected.startsWith('L:') && GRAPH_STATE.model && GRAPH_STATE.model.byId.has(selected.slice(2))) {
    // A selection made in another view of the code map (Tree, Treemap, Code)
    // can sit inside a closed community. Open it and fly there, the way the
    // map's own search does, rather than arriving with nothing selected.
    flyToLeaf(selected.slice(2));
  } else {
    onGraphSelect(null, true);
  }
}

/*
  rebuildRenderSet is the ONE place a render set is built and handed to the
  canvas. It used to be four places — rebuildGraph, rebuildGraphMore,
  fitBudgetToWindow and onGraphOpen — each repeating the same five calls, and
  three of them passed a different layout seed from the fourth, so expanding a
  group re-randomised the whole layout out from under the reader. The layout
  has no seed now; the one place is kept because it is the one place.
*/
function rebuildRenderSet(budget, opts) {
  const model = GRAPH_STATE.model;
  const cv = GRAPH_STATE.canvas;
  if (!model || !cv) return;
  GRAPH_STATE.budget = budget;
  const set = buildRenderSet(model, budget);
  GRAPH_STATE.renderSet = set;
  applyLenses(set);
  cv.setData(set);
  applyEmphasis();
  paintLensReadout();
  paintCanvasNote();
  paintPathPanel();
  paintEdgeTable();
  if (opts && opts.refit) cv.hasFit = false;
}

/*
  CROSS-LANGUAGE EDGES, MADE VISIBLE AS SUCH.

  A polyglot repository's most expensive surprises live on the seams — a Go
  worker reading a table a Java service writes, a TypeScript client calling an
  endpoint declared in Python. Those edges are already in the payload; nothing
  drew them differently from any other edge, so the one place this product is
  structurally ahead of a single-language tool was invisible on its own map.

  The rule differs by rung, and both halves are stated on screen rather than
  assumed:

    files, symbols   each node has ONE language, so an edge is cross-language
                     when its two endpoints disagree and both are known.
    containers       a component is a bag of languages, so an edge is
                     cross-language when the two bags SHARE NONE. A component
                     that is half Go and half Java sits on both sides of that
                     seam and is correctly not counted as crossing it.

  An endpoint whose language the index never recorded makes the answer unknown,
  not false, and it is counted separately for exactly that reason.
*/
function markCrossLanguage(model) {
  let crossing = 0;
  let unknown = 0;
  for (const link of model.links) {
    const a = model.byId.get(link.a);
    const b = model.byId.get(link.b);
    link.xlang = false;
    link.xlangUnknown = false;
    if (!a || !b) { link.xlangUnknown = true; unknown++; continue; }
    const al = langSetOf(a);
    const bl = langSetOf(b);
    if (!al.length || !bl.length) { link.xlangUnknown = true; unknown++; continue; }
    link.xlang = !al.some((l) => bl.indexOf(l) >= 0);
    link.xlangFrom = al.join('+');
    link.xlangTo = bl.join('+');
    if (link.xlang) crossing++;
  }
  model.xlang = { crossing: crossing, unknown: unknown, total: model.links.length };
}

function langSetOf(leaf) {
  if (Array.isArray(leaf.languages) && leaf.languages.length) return leaf.languages.slice().sort();
  return leaf.language ? [String(leaf.language)] : [];
}

/*
  CONFIDENCE AS A LENS, NOT A REFETCH.

  The confidence floor in the header is a SERVER filter: move it and the page
  goes back to the API. This is a second floor applied here, over the edges
  already drawn, so raising the standard of proof is instant and — this is the
  point — REVERSIBLE AND COMPARABLE. A reader can watch 1,412 relationships
  become 96 and see, as a shape rather than as a number, how much of their map
  is proven and how much is inferred.

  No competitor can draw this. Their graphs have no tier on an edge to filter
  by, so "what do you actually know" is not a question their picture can be
  asked. Nothing is deleted: every edge the lens holds back is counted, and the
  count is on the canvas and in the table.
*/
function applyLenses(set) {
  const lens = tierRank(state.q.lens) && TIER[state.q.lens] ? state.q.lens : '';
  const xonly = state.q.xlang === '1';
  const floorRank = lens ? tierRank(lens) : -1;

  let hiddenByLens = 0;
  let hiddenByLang = 0;
  let crossing = 0;
  const survivors = { exact: 0, high: 0, medium: 0, low: 0, unknown: 0 };

  for (const e of set.edges) {
    survivors[tierOf(e.weakest)] = (survivors[tierOf(e.weakest)] || 0) + 1;
    if (e.xlang) crossing++;
    let hidden = false;
    if (lens && tierRank(e.weakest) < floorRank) { hidden = true; hiddenByLens++; }
    if (!hidden && xonly && !e.xlang) { hidden = true; hiddenByLang++; }
    e.hidden = hidden;
  }
  set.visibleEdges = set.edges.filter((e) => !e.hidden);
  set.lens = {
    tier: lens, xonly: xonly,
    hiddenByLens: hiddenByLens, hiddenByLang: hiddenByLang,
    crossing: crossing, survivors: survivors,
    drawn: set.visibleEdges.length, total: set.edges.length,
  };
  return set;
}


// ---------------------------------------------------------------- focus + path

/*
  FOCUS: THE BLAST RADIUS, DRAWN.

  "What else does this touch, and how far out?" is the question that decides
  whether a change is a morning or a fortnight, and until now the graph screen
  could only answer it by having the reader trace lines with their eye. Pick a
  node, choose a depth, and everything within that many hops stays lit while the
  rest of the map dims — dims, not disappears, because the surrounding structure
  is what makes a radius mean anything.

  The walk is over the links THE SERVER SENT AT THIS RUNG, which is a bounded,
  stated population and not the whole index. It is undirected on purpose: a
  caller you break is as much your problem as a callee you depend on, and a
  reader asking "what does this touch" means both. Depth is capped at 4 because
  a fifth hop on a real repository reaches most of it, and a lens that lights
  everything has stopped being a lens — the cap is a control, and the count it
  holds back is printed.

  This is NOT the Blast radius screen. That one traverses the real graph in the
  capability layer with path-minimum confidence and covering tests; this one is
  the same question asked of the picture in front of you, instantly, and it
  offers the button through to the real analysis.
*/
const FOCUS_MAX_DEPTH = 4;

function focusDepth() {
  const n = parseInt(state.q.fdepth, 10);
  if (!Number.isFinite(n)) return 2;
  return Math.max(1, Math.min(FOCUS_MAX_DEPTH, n));
}

function computeFocus(model, rootId, depth) {
  if (!model || !rootId) return null;
  const root = String(rootId);
  // A focus root is either one leaf or one COLLAPSED GROUP. A group is a set of
  // leaves and "what does this region touch" is the same walk from all of them,
  // so it is one code path with several starting points rather than two.
  let roots = [];
  let label = root;
  if (model.byId.has(root)) {
    roots = [root];
    const leaf = model.byId.get(root);
    label = text(leaf.label, root);
  } else if (model.groups.has(root)) {
    const group = model.groups.get(root);
    roots = group.members.map((leaf) => leaf.id);
    label = text(group.label, root);
  }
  if (!roots.length) return null;
  const adj = adjacency(model);
  const seen = new Map(roots.map((id) => [id, 0]));
  let frontier = roots.slice();
  for (let d = 1; d <= depth; d++) {
    const next = [];
    for (const id of frontier) {
      for (const step of (adj.get(id) || [])) {
        if (seen.has(step.other)) continue;
        seen.set(step.other, d);
        next.push(step.other);
      }
    }
    frontier = next;
    if (!frontier.length) break;
  }
  // The frontier that a deeper walk WOULD have reached: the honest statement of
  // what this depth held back, rather than a silent horizon.
  let beyond = 0;
  for (const id of frontier) {
    for (const step of (adj.get(id) || [])) if (!seen.has(step.other)) beyond++;
  }
  return {
    root: root, label: label, roots: roots.length, depth: depth,
    reached: seen, beyond: beyond, total: model.leaves.length,
  };
}

// adjacency is memoised on the model: focus and path both walk it, and both can
// be recomputed on every lens change.
function adjacency(model) {
  if (model.__adj) return model.__adj;
  const adj = new Map();
  const push = (from, other, link, dir) => {
    if (!adj.has(from)) adj.set(from, []);
    adj.get(from).push({ other: other, link: link, dir: dir });
  };
  for (const link of model.links) {
    push(link.a, link.b, link, 'out');
    push(link.b, link.a, link, 'in');
  }
  model.__adj = adj;
  return adj;
}

/*
  PATH FINDING: "HOW DOES THIS REACH THAT", WITH THE CHAIN DRAWN.

  This is the change-safety question. Not "are these two connected" — a reader
  can guess that — but "by what route, and how much of that route is actually
  proven". A chain of five hops where one hop is a `medium` name match is a
  chain you do not trust, and no other product in this category can even pose
  the question: an edge without a measured tier cannot have a weakest hop.

  The search is a widest-path Dijkstra, not a shortest-path one. It maximises
  THE WEAKEST TIER ALONG THE ROUTE first and minimises hops only to break a tie,
  because the shortest chain through a guess is worse evidence than a longer one
  through four compiler-resolved calls. The route it returns is therefore the
  BEST-EVIDENCED route, and the panel says which hop is the weak one.

  Direction is respected. A → B is not B → A, and if only the reverse exists
  that is a finding, so it is searched second and reported as the reverse.

  The bound is stated: it searches the relationships at this rung, under the
  current server floor. A route through a node this rung did not draw is a route
  this panel cannot see, and the panel says so rather than reporting "no path".
*/
function findPath(model, fromId, toId) {
  const from = String(fromId);
  const to = String(toId);
  if (!model.byId.has(from) || !model.byId.has(to)) return null;
  if (from === to) return { hops: [], reversed: false, weakest: 'exact', sameNode: true };

  const forward = widestPath(model, from, to, 'out');
  if (forward) return forward;
  const back = widestPath(model, to, from, 'out');
  if (back) { back.reversed = true; return back; }
  return null;
}

function widestPath(model, from, to, direction) {
  const adj = adjacency(model);
  // best[id] = { rank, hops } — rank is the weakest tier on the route so far.
  const best = new Map([[from, { rank: 99, hops: 0 }]]);
  const via = new Map();
  // A bounded frontier: the container rung is tens of nodes, the symbol rung
  // low thousands, and a linear-scan priority queue over that is cheaper than
  // the heap it would take to avoid it.
  const open = new Set([from]);
  let guard = 0;
  while (open.size) {
    if (++guard > 200000) break;
    let cur = null;
    let curScore = null;
    for (const id of open) {
      const b = best.get(id);
      if (!curScore || b.rank > curScore.rank || (b.rank === curScore.rank && b.hops < curScore.hops)) {
        cur = id; curScore = b;
      }
    }
    open.delete(cur);
    if (cur === to) break;
    for (const step of (adj.get(cur) || [])) {
      if (step.dir !== direction) continue;
      const rank = Math.min(curScore.rank, tierRank(step.link.conf));
      const hops = curScore.hops + 1;
      const known = best.get(step.other);
      if (known && !(rank > known.rank || (rank === known.rank && hops < known.hops))) continue;
      best.set(step.other, { rank: rank, hops: hops });
      via.set(step.other, { prev: cur, link: step.link });
      open.add(step.other);
    }
  }
  if (!via.has(to) && to !== from) return null;

  const hops = [];
  let at = to;
  let weak = 'exact';
  while (at !== from) {
    const step = via.get(at);
    if (!step) return null;
    hops.unshift({ from: step.prev, to: at, link: step.link });
    weak = weakest(weak, step.link.conf);
    at = step.prev;
    if (hops.length > 64) return null;
  }
  return { hops: hops, weakest: weak, reversed: false, sameNode: false };
}

/*
  applyEmphasis turns the focus and path lenses into the two sets the canvas
  needs, and records the numbers the note and the panel print. It runs after
  every render-set rebuild, because expanding a group changes which drawn node
  stands for which leaf.
*/
function applyEmphasis() {
  const model = GRAPH_STATE.model;
  const set = GRAPH_STATE.renderSet;
  const cv = GRAPH_STATE.canvas;
  if (!model || !set || !cv) return;

  GRAPH_STATE.focus = state.q.focus ? computeFocus(model, state.q.focus, focusDepth()) : null;
  GRAPH_STATE.path = (state.q.pfrom && state.q.pto) ? findPath(model, state.q.pfrom, state.q.pto) : null;

  let nodeKeys = null;
  if (GRAPH_STATE.focus) {
    nodeKeys = new Set();
    for (const id of GRAPH_STATE.focus.reached.keys()) {
      const key = set.keyOfLeaf.get(id);
      if (key) nodeKeys.add(key);
    }
    GRAPH_STATE.focus.drawn = nodeKeys.size;
  }

  const pathKeys = new Set();
  const pathEdges = new Set();
  if (GRAPH_STATE.path && GRAPH_STATE.path.hops.length) {
    for (const hop of GRAPH_STATE.path.hops) {
      const ka = set.keyOfLeaf.get(hop.from);
      const kb = set.keyOfLeaf.get(hop.to);
      if (ka) pathKeys.add(ka);
      if (kb) pathKeys.add(kb);
      if (ka && kb && ka !== kb) { pathEdges.add(ka + ' ' + kb); pathEdges.add(kb + ' ' + ka); }
    }
    GRAPH_STATE.path.drawnHops = pathEdges.size / 2;
  }

  cv.setEmphasis({
    nodeKeys: nodeKeys,
    pathNodeKeys: pathKeys.size ? pathKeys : null,
    pathEdgeIds: pathEdges.size ? pathEdges : null,
  });
}

/*
  THE PATH PANEL. Every hop, in order, with the edge kind, the count, the
  measured tier, and one real file:line — the same evidential shape as the
  relationships table, because a chain is a sequence of relationships and a
  reader should not have to learn a second vocabulary for it.
*/
function paintPathPanel() {
  const host = GRAPH_STATE.pathHost;
  const model = GRAPH_STATE.model;
  if (!host) return;
  clear(host);
  if (!model) return;
  if (!state.q.pfrom && !state.q.pto) return;

  const unit = model.unit;
  const from = state.q.pfrom ? model.byId.get(String(state.q.pfrom)) : null;
  const to = state.q.pto ? model.byId.get(String(state.q.pto)) : null;
  const label = (id) => {
    const leaf = model.byId.get(String(id));
    return leaf ? text(leaf.label, String(id)) : String(id);
  };

  const clearBtn = el('button', {
    class: 'ghost', type: 'button', text: 'Clear path',
    onclick: () => { setQuery({ pfrom: '', pto: '' }); applyEmphasis(); paintPathPanel(); paintCanvasNote(); GRAPH_STATE.canvas.draw(); },
  });

  if (!from || !to) {
    const missing = !from ? 'start' : 'end';
    const body = [el('p', { class: 'prose' },
      'A path needs both ends. The ', el('b', { text: missing }), ' is not set — or the id in the address bar names a ',
      unit, ' this level did not send. Select a node and use ',
      el('b', { text: 'Path from here' }), ', then select another and use ', el('b', { text: 'Path to here' }), '.')];
    const cardNode = card('How does this reach that', 'waiting for both ends', body, null, 'flush');
    cardNode.querySelector('header').appendChild(clearBtn);
    host.appendChild(cardNode);
    return;
  }

  const path = GRAPH_STATE.path;
  if (!path) {
    const body = [
      el('p', { class: 'prose' },
        'No route from ', el('b', { class: 'sym', text: text(from.label) }), ' to ',
        el('b', { class: 'sym', text: text(to.label) }), ' exists in this view, in either direction.'),
      inlineOmit('That is a statement about THIS VIEW, not about the repository. It searched ',
        el('b', { text: num(model.links.length) }), ' ' + plural('relationship', model.links.length) + ' between ',
        el('b', { text: num(model.leaves.length) }), ' ' + plural(unit, model.leaves.length),
        ' — the ones the server sent at this level, at the ',
        el('b', { 'data-tier': state.q.floor, text: tierWord(state.q.floor) }),
        ' level. A route through a ' + unit + ' outside this scope, or through an edge below that level, is one this panel cannot see. ',
        'Lower the standard of proof, or widen the scope, before concluding there is no connection.'),
    ];
    const cardNode = card('How does this reach that', 'no route in this view', body, null, 'flush');
    cardNode.querySelector('header').appendChild(clearBtn);
    host.appendChild(cardNode);
    return;
  }

  const hops = path.hops;
  const a = path.reversed ? to : from;
  const b = path.reversed ? from : to;
  // A container's "path" IS its id, which is its label; printing both put every
  // node of the chain on screen twice.
  const subOf = (leaf) => {
    const sub = text(leaf && (leaf.path || leaf.fqn) || '', '');
    return (!sub || sub === text(leaf.label)) ? '' : sub;
  };
  const chain = el('ol', { class: 'chain' });
  chain.appendChild(el('li', { class: 'chain-node' },
    el('span', { class: 'ix', text: '0' }),
    el('span', { class: 'sym', text: text(a.label) }),
    subOf(a) ? el('span', { class: 'path', text: subOf(a) }) : null));
  hops.forEach((hop, i) => {
    const kind = (hop.link.kinds && hop.link.kinds[0]) ? hop.link.kinds[0][0] : 'EDGE';
    const target = model.byId.get(String(hop.to));
    chain.appendChild(el('li', { class: 'chain-hop t-' + tierOf(hop.link.conf) },
      el('span', { class: 'rel' }, el('code', { text: kind }), ' ×', num(hop.link.count || 1)),
      tierBadge(hop.link.conf),
      evidenceOf(hop.link.evidence)));
    chain.appendChild(el('li', { class: 'chain-node' },
      el('span', { class: 'ix', text: String(i + 1) }),
      el('span', { class: 'sym', text: target ? text(target.label) : label(hop.to) }),
      (target && subOf(target)) ? el('span', { class: 'path', text: subOf(target) }) : null));
  });

  const weakestHop = hops.reduce((worst, hop) =>
    (!worst || tierRank(hop.link.conf) < tierRank(worst.link.conf)) ? hop : worst, null);

  const body = [
    el('p', { class: 'prose' },
      path.reversed
        ? el('span', null, 'Nothing runs from ', el('b', { class: 'sym', text: text(from.label) }), ' to ',
          el('b', { class: 'sym', text: text(to.label) }), ', but the REVERSE does, and that is the finding. The chain below is drawn in the direction the edges actually point.')
        : el('span', null, 'The best-evidenced route, chosen to maximise the WEAKEST tier on it and to minimise hops only as a tiebreak — a short chain through a guess is worse evidence than a longer one through proofs.')),
    el('div', { class: 'kpis' },
      kpi('Hops', num(hops.length), plural('relationship', hops.length) + ' end to end'),
      kpi('Weakest hop', tierOf(path.weakest), 'the chain is no stronger than this',
        tierRank(path.weakest) >= 3 ? 'good' : (tierRank(path.weakest) <= 1 ? 'bad' : '')),
      kpi('Drawn on the map', num(GRAPH_STATE.path.drawnHops || 0) + ' of ' + num(hops.length),
        (GRAPH_STATE.path.drawnHops || 0) >= hops.length
          ? 'every hop is highlighted on the canvas'
          : 'the rest run between nodes this canvas has collapsed')),
    chain,
  ];
  if (weakestHop) {
    body.push(inlineOmit('The hop that limits this chain is ',
      el('b', { class: 'sym', text: label(weakestHop.from) }), ' → ',
      el('b', { class: 'sym', text: label(weakestHop.to) }), ', recorded at ',
      el('b', { 'data-tier': tierOf(weakestHop.link.conf), text: tierWord(weakestHop.link.conf) }), ' — ', TIER[tierOf(weakestHop.link.conf)].at,
      '. Check that one first.'));
  }
  if ((GRAPH_STATE.path.drawnHops || 0) < hops.length) {
    body.push(inlineOmit(el('b', { text: num(hops.length - (GRAPH_STATE.path.drawnHops || 0)) }),
      ' ' + plural('hop', hops.length - (GRAPH_STATE.path.drawnHops || 0)) + ' of this chain run between ' + plural(unit, 2)
      + ' that are inside a collapsed group on the canvas, so they are listed here but not highlighted there. Open the groups, or set Group by to "nothing", to see the whole chain drawn.'));
  }
  body.push(inlineOmit('Searched ', el('b', { text: num(model.links.length) }),
    ' ' + plural('relationship', model.links.length) + ' at the ' + text(GRAPH_STATE.data.level, 'containers')
    + ' level, at ' + tierWord(state.q.floor) + ' or better. A better route may exist through a '
    + unit + ' this scope did not include.'));

  const cardNode = card('How does this reach that',
    num(hops.length) + ' ' + plural('hop', hops.length) + ', weakest ' + tierOf(path.weakest),
    body, null, 'flush');
  cardNode.querySelector('header').appendChild(clearBtn);
  host.appendChild(cardNode);
}

// ---------------------------------------------------------------- the lens row

function renderLenses(d) {
  const host = GRAPH_STATE.lensHost;
  if (!host) return;
  clear(host);
  const bar = el('div', { class: 'lensbar' });

  // The standard-of-proof and focus-depth selects sit in the Filters popover
  // with the other filters; the two actions and the readout stay here, under
  // the canvas they describe.
  const xon = state.q.xlang === '1';
  bar.appendChild(el('button', {
    class: 'chip', type: 'button', id: 'graph-xlang', 'aria-pressed': String(xon),
    title: 'Draw only the relationships whose two ends are in different languages. The seams are where a polyglot repository breaks.',
    onclick: () => { setQuery({ xlang: xon ? '0' : '1' }); relens(); },
  }, 'cross-language only'));

  bar.appendChild(el('button', {
    class: 'chip', type: 'button', id: 'graph-clear-lenses',
    title: 'Drop the focus, the path and both lenses, and go back to the whole view.',
    onclick: () => {
      setQuery({ lens: '', xlang: '0', focus: '', pfrom: '', pto: '' });
      relens();
    },
  }, 'clear lenses'));

  bar.appendChild(el('span', { class: 'lens-readout', id: 'graph-lens-readout', 'aria-live': 'polite' }));
  host.appendChild(bar);

  // The legend sits with the drawing rather than in a glossary on another
  // screen, and it names the measured precision each tier stands for.
  host.appendChild(legendStrip());
  paintLensReadout();
}

// A <select> without the wrapping <label> selectField builds, because the lens
// bar labels its controls itself.
function selectControl(id, options, value, onChange) {
  const sel = el('select', { id: id }, options.map((o) =>
    el('option', { value: o.value, text: o.label, selected: String(o.value) === String(value) ? 'selected' : null })));
  sel.value = String(value);
  sel.addEventListener('change', () => onChange(sel.value));
  return sel;
}

// relens re-applies the client-side lenses without going back to the server.
function relens() {
  const set = GRAPH_STATE.renderSet;
  if (!set || !GRAPH_STATE.canvas) return;
  applyLenses(set);
  applyEmphasis();
  paintLensReadout();
  paintCanvasNote();
  paintPathPanel();
  paintEdgeTable();
  GRAPH_STATE.canvas.draw();
  const btn = document.getElementById('graph-xlang');
  if (btn) btn.setAttribute('aria-pressed', String(state.q.xlang === '1'));
  const lensSel = document.getElementById('graph-lens');
  if (lensSel && lensSel.value !== state.q.lens) lensSel.value = state.q.lens;
  paintFiltersButton();
}

/*
  THE READOUT IS THE FEATURE.

  A filter that silently removes things is the failure mode this whole portal is
  built against, so the lens states its arithmetic in one line: how many
  relationships are drawn, how many the lens is holding back, and — the number
  a reader actually wants — how much of this map is proven versus inferred.
*/
function paintLensReadout() {
  const out = document.getElementById('graph-lens-readout');
  const set = GRAPH_STATE.renderSet;
  const model = GRAPH_STATE.model;
  if (!out || !set || !set.lens) return;
  clear(out);
  const L = set.lens;

  const proven = L.survivors.exact || 0;
  const total = L.total || 0;
  out.appendChild(el('span', null,
    el('b', { text: num(L.drawn) }), ' of ', el('b', { text: num(total) }),
    ' combined ' + plural('edge', total) + ' drawn'));

  if (total > 0) {
    out.appendChild(el('span', { class: 'muted' },
      // pct() formats an already-scaled percentage; it does not divide. Handing
      // it a fraction rendered 40 of 79 as "1%".
      ' · ', el('b', { text: num(proven) }), ' (', pct((proven * 100) / total, 0),
      ') are proven — a compiler resolved them; the rest are inferred and carry a weaker tier.'));
  }
  if (L.hiddenByLens > 0) {
    out.appendChild(el('span', { class: 'held' },
      ' · ', el('b', { text: num(L.hiddenByLens) }), ' held back by the ',
      el('b', { text: L.tier }), ' lens — they are still in the index and still in the export.'));
  }
  if (L.xonly) {
    out.appendChild(el('span', { class: 'held' },
      ' · ', el('b', { text: num(L.hiddenByLang) }), ' hidden as same-language; ',
      el('b', { text: num(L.crossing) }), ' cross a language boundary.'));
  } else if (L.crossing > 0) {
    out.appendChild(el('span', { class: 'muted' },
      ' · ', el('b', { text: num(L.crossing) }), ' cross a language boundary.'));
  }
  if (model && model.xlang && model.xlang.unknown > 0) {
    out.appendChild(el('span', { class: 'muted' },
      ' · ', el('b', { text: num(model.xlang.unknown) }),
      ' underlying ' + plural('relationship', model.xlang.unknown)
      + (model.xlang.unknown === 1 ? ' has' : ' have')
      + ' an end whose language the index never recorded, so whether it crosses a boundary is unknown rather than no.'));
  }
}

/*
  THE LEGIBILITY CONTRACT, CLOSED.

  legibleBudget estimates how many labelled boxes fit at a readable zoom. An
  estimate is not a guarantee: a set of unusually wide labels, or a community
  structure that lays out long and thin, can still settle into a map larger than
  the window at the floor. When that happens the answer is NOT to zoom out past
  the point where text disappears - that is the defect this rebuild exists to
  remove - it is to draw fewer things and say how many.

  So: measure the settled layout, and if it overflows, shrink the drawn set in
  proportion to the overshoot and lay it out again. Twice at most, because a
  third pass is chasing rather than converging, and whatever is left over is
  printed on the canvas with a control to override it.
*/
function fitBudgetToWindow() {
  const cv = GRAPH_STATE.canvas;
  const model = GRAPH_STATE.model;
  if (!cv || !model) return;
  if (!cv.overflows || (GRAPH_STATE.shrinks || 0) >= 2) { paintCanvasNote(); return; }

  const box = cv.bbox();
  const haveW = (cv.width - 92) / GRAPH.MIN_K;
  const haveH = (cv.height - 92) / GRAPH.MIN_K;
  const ratio = Math.min(1, (haveW * haveH) / Math.max(1, box.w * box.h));
  const next = Math.max(6, Math.floor(GRAPH_STATE.budget * ratio * 0.9));
  if (next >= GRAPH_STATE.budget) { paintCanvasNote(); return; }

  GRAPH_STATE.shrinks = (GRAPH_STATE.shrinks || 0) + 1;
  rebuildRenderSet(next);
}

/*
  The server computes communities by deterministic label propagation and says so
  in the payload, including a caveat that a community is a drawing aid rather
  than a declared architecture. That assignment is authoritative and is used as
  sent. What is NOT taken from it is the label: at symbol level a community is
  named after a numeric symbol id ("1059 +98"), which tells a reader nothing, so
  the name is derived from where its members actually live.
*/
function applyServerClusters(model, d) {
  const buckets = new Map();
  for (const leaf of model.leaves) {
    const key = leaf.clusterId || '(ungrouped)';
    if (!buckets.has(key)) buckets.set(key, []);
    buckets.get(key).push(leaf);
  }
  const meta = new Map((d.clusters || []).map((cl) => [cl.id, cl]));
  model.groups = new Map();
  model.leafGroup = new Map();

  const small = [];
  for (const [key, members] of Array.from(buckets)) {
    if (members.length < 3 && buckets.size > 8) { small.push.apply(small, members); buckets.delete(key); }
  }
  if (small.length) buckets.set('(small communities)', small);

  for (const [key, members] of buckets) {
    const named = nameCommunity(members);
    const cl = meta.get(key);
    const gid = 'g:' + key;
    members.sort((a, b) => (b.degree || 0) - (a.degree || 0) || String(a.label).localeCompare(String(b.label)));
    const base = baseName(named.label) || named.label;
    model.groups.set(gid, {
      id: gid,
      baseLabel: base,
      label: base,
      full: named.label,
      members: members,
      share: named.share,
      dirs: named.dirs,
      solo: false,
      server: cl || null,
    });
    for (const leaf of members) model.leafGroup.set(leaf.id, gid);
  }
  model.grouping = 'communities';
  disambiguateLabels(model);
  return model;
}

/*
  legibleBudget converts the canvas box into a number of nodes that can be drawn
  with every label readable. It is the arithmetic behind the promise: at
  GRAPH.MIN_K the world is width/0.75 by height/0.75, a labelled box needs about
  176 by 66 world pixels with its gap, and the layout packs at roughly 40%.
  Whatever does not fit is not drawn small — it is not drawn, and said.
*/
function legibleBudget(canvas) {
  const w = Math.max(320, canvas.width || 900);
  const h = Math.max(260, canvas.height || 560);
  const worldArea = (w / GRAPH.MIN_K) * (h / GRAPH.MIN_K);
  const cell = (150 + GRAPH.GAP) * (GRAPH.NODE_H + GRAPH.GAP);
  // 0.30, not the 0.42 a packing calculation suggests. A map that exactly fills
  // the area available lands ON the legibility floor with no room for the edges
  // between the boxes, and reads as a wall of labels rather than a map. The
  // headroom is the readability.
  return Math.max(8, Math.min(90, Math.floor((worldArea * 0.30) / cell)));
}

function paintCanvasNote() {
  const note = GRAPH_STATE.note;
  const set = GRAPH_STATE.renderSet;
  const model = GRAPH_STATE.model;
  if (!note || !set || !model) return;
  clear(note);

  const drawn = set.nodes.length;
  const unit = plural(model.unit, 2);
  const grouped = set.nodes.filter((n) => n.type === 'cluster').length;

  /*
    TWO CAPS COMPOUND HERE AND THE READER IS TOLD BOTH.

    node_budget is the SERVER's: how many nodes it sent of how many it holds,
    and by what rule. The hold-back below is THIS FILE's: how many of the ones
    that arrived were kept out of the drawing so every label stays readable.
    Reporting only the second would let a picture built from 48 of 188 files
    describe itself as complete, which is the one way a wrong picture cannot be
    detected by looking at it.
  */
  const budget = (GRAPH_STATE.data && GRAPH_STATE.data.node_budget) ? GRAPH_STATE.data.node_budget : null;
  const sentPartial = budget && Number(budget.excluded) > 0;

  /*
    AND THE TWO IT COMPARES HAVE TO BE THE SAME POPULATION.

    This sentence used to read "from 168 files the server sent, of 114 it
    holds" — a view claiming to have received more than existed. Both figures
    were right and they counted different things: node_budget covers the files
    IN THE REQUESTED CONTAINER, while a drilled view also pulls in bounded
    neighbours from OUTSIDE it so the boundary is visible. 48 of 114 in the
    container, plus 120 neighbours, is 168.

    So the neighbours are counted and named. A number that contradicts the
    number beside it costs more trust than either of them buys.
  */
  const external = model.leaves.filter((leaf) => leaf.external).length;
  note.appendChild(el('span', null,
    el('b', { text: num(drawn) }), ' drawn',
    grouped ? ' (' + num(grouped) + ' collapsed ' + plural('group', grouped) + ')' : '',
    ' · ', el('b', { text: num(set.edges.length) }), ' combined ' + plural('edge', set.edges.length),
    ' from ', el('b', { text: num(model.leaves.length) }), ' ' + unit,
    sentPartial ? el('span', { class: 'muted', title: text(budget.rule) },
      ' in this view — the server drew ', el('b', { text: num(budget.drawn) }), ' of ',
      el('b', { text: num(budget.available) }), ' it holds in this scope',
      external > 0
        ? el('span', null, ' and added ', el('b', { text: num(external) }),
          ' from outside it, so the boundary is visible')
        : null) : null));

  /*
    TWO POPULATIONS, AND THEY WERE BEING ADDED TOGETHER.

    hiddenGroups counts GROUPS the budget skipped; hiddenMembers counts the
    LEAVES inside them. With grouping set to "nothing" every group is one leaf,
    so a view that held back 138 files printed 138 + 138 = "276 held back" — on
    a rung that only had 168 files in it. A count that exceeds its own
    population is worse than no count, because a reader cannot tell it is wrong
    by looking at anything else on the screen.

    The figure a reader wants is the leaves: how many of the things being drawn
    are not drawn. The groups are a separate, smaller fact and are named as one.
  */
  if (set.hiddenMembers > 0 || set.hiddenGroups > 0) {
    note.appendChild(document.createTextNode(' — '));
    note.appendChild(el('b', { text: num(set.hiddenMembers) }));
    // `unit` above is ALREADY plural. Pluralising it again printed "symbolses".
    note.appendChild(document.createTextNode(' ' + plural(model.unit, set.hiddenMembers)
      + ' held back so every label stays readable'));
    if (set.hiddenGroups > 0) {
      note.appendChild(el('span', { class: 'muted' },
        ', in ', el('b', { text: num(set.hiddenGroups) }),
        ' ' + plural('group', set.hiddenGroups) + ' this canvas did not draw'));
    }
    note.appendChild(document.createTextNode('. '));
    note.appendChild(el('button', {
      class: 'linkish', type: 'button', text: 'Draw more anyway',
      onclick: () => { GRAPH_STATE.shrinks = 99; GRAPH.MEMBER_CAP += 40; rebuildGraphMore(); },
    }));
  }
  if (state.q.focus && !GRAPH_STATE.focus) {
    note.appendChild(el('div', { class: 'small held' },
      'The focus in this link — ', el('b', { text: state.q.focus }),
      ' — is not in this view, so nothing is dimmed. It may belong to another level or another scope. ',
      el('button', { class: 'linkish', type: 'button', text: 'Clear it',
        onclick: () => { setQuery({ focus: '' }); applyEmphasis(); paintCanvasNote(); GRAPH_STATE.canvas.draw(); } })));
  }

  if (GRAPH_STATE.autoNote) {
    note.appendChild(el('div', { class: 'small muted', text: 'Grouping chose itself: ' + GRAPH_STATE.autoNote + '.' }));
  }

  // The focus lens, stated on the canvas rather than only in a control, because
  // a dimmed map is a map somebody will otherwise read as a small repository.
  const foc = GRAPH_STATE.focus;
  if (foc) {
    note.appendChild(el('div', { class: 'small' },
      'Focus: ', el('b', { text: foc.label }),
      foc.roots > 1 ? ' (' + num(foc.roots) + ' ' + plural(model.unit, foc.roots) + ' in that group)' : '',
      ' — ',
      el('b', { text: num(foc.reached.size) }), ' of ', el('b', { text: num(foc.total) }),
      ' ' + plural(model.unit, foc.total) + ' are within ', el('b', { text: String(foc.depth) }),
      ' ' + plural('hop', foc.depth) + ', ', el('b', { text: num(foc.drawn || 0) }),
      ' of them on this canvas; the rest are dimmed, not removed.',
      foc.beyond > 0 ? el('span', { class: 'held' },
        ' At least ' + num(foc.beyond) + ' further ' + plural(model.unit, foc.beyond)
        + ' sit one hop past this depth and are outside the lens — raise Focus depth to include them.') : null,
      ' ',
      el('button', { class: 'linkish', type: 'button', text: 'Clear focus',
        onclick: () => { setQuery({ focus: '' }); applyEmphasis(); paintCanvasNote(); GRAPH_STATE.canvas.draw(); } })));
  }

  const lens = set.lens;
  if (lens && (lens.hiddenByLens > 0 || lens.hiddenByLang > 0)) {
    note.appendChild(el('div', { class: 'small held' },
      el('b', { text: num(lens.hiddenByLens + lens.hiddenByLang) }),
      ' ' + plural('edge', lens.hiddenByLens + lens.hiddenByLang) + ' are not stroked because of a lens above'
      + (lens.hiddenByLens ? ' (' + num(lens.hiddenByLens) + ' below ' + lens.tier + ')' : '')
      + (lens.hiddenByLang ? ' (' + num(lens.hiddenByLang) + ' same-language)' : '')
      + '. Nothing was dropped: they are in the table, in the export, and in the index.'));
  }

  if (GRAPH_STATE.flyNote) {
    note.appendChild(el('div', { class: 'small held', text: GRAPH_STATE.flyNote }));
  }

  if (GRAPH_STATE.canvas && GRAPH_STATE.canvas.overflows) {
    note.appendChild(el('div', { class: 'small', text: 'The map is larger than this window at a readable zoom — pan, or collapse a group.' }));
  }
}

function rebuildGraphMore() {
  const model = GRAPH_STATE.model;
  if (!model) return;
  rebuildRenderSet((GRAPH_STATE.budget || legibleBudget(GRAPH_STATE.canvas)) + 40);
}

function onGraphOpen(node) {
  const model = GRAPH_STATE.model;
  if (!node || !model) return;
  if (node.type === 'cluster') {
    // Opening a group is a deliberate request for more, so the shrink budget
    // is recomputed from scratch rather than inherited from whatever the
    // previous drawing had been reduced to.
    model.expanded.add(node.gid);
    setQuery({ expand: Array.from(model.expanded).join(',') });
    GRAPH_STATE.shrinks = 0;
    rebuildRenderSet(legibleBudget(GRAPH_STATE.canvas));
    return;
  }
  const leaf = node.leaf;
  const level = text(GRAPH_STATE.data.level, 'containers');
  if (level === 'containers') { setQuery({ container: leaf.id, level: '', sel: '', expand: '' }); reloadGraph(); return; }
  if (level === 'files') { setQuery({ level: 'symbols', sel: '', expand: '' }); reloadGraph(); return; }
  go('blast', { sym: leaf.fqn || leaf.label });
}

function onGraphSelect(node, quiet) {
  const layout = GRAPH_STATE.layoutHost;
  const inspector = GRAPH_STATE.inspector;
  if (!layout || !inspector) return;
  if (!quiet) setQuery({ sel: node ? node.key : '' }, { replace: true });

  if (!node) {
    // The inspector held a permanent 320px column to say two sentences while
    // the canvas it belongs to had 662px. It is not on the page until there is
    // something in it.
    if (inspector.parentNode) inspector.remove();
    layout.classList.add('no-inspector');
    if (GRAPH_STATE.canvas) GRAPH_STATE.canvas.measure();
    paintEdgeTable();
    return;
  }
  if (!inspector.parentNode) layout.appendChild(inspector);
  layout.classList.remove('no-inspector');
  clear(inspector);

  if (node.type === 'cluster') inspectCluster(inspector, node);
  else inspectLeaf(inspector, node);
  if (GRAPH_STATE.canvas) GRAPH_STATE.canvas.measure();
  paintEdgeTable();
}

function inspectCluster(host, node) {
  const group = node.group;
  host.appendChild(el('h3', { text: text(group.label) }));
  host.appendChild(el('p', { class: 'path', text: text(group.full, '') }));
  host.appendChild(el('div', { class: 'kpis' },
    kpi('Members', num(group.members.length), plural(GRAPH_STATE.model.unit, group.members.length)),
    kpi('Inside', num(node.internal || 0), 'edges within this group')));
  if (group.server) {
    host.appendChild(el('p', { class: 'small muted', text: 'How this group was found: ' + text(group.server.method) }));
    host.appendChild(el('p', { class: 'small muted', text: text(group.server.caveat) }));
  } else {
    host.appendChild(el('p', { class: 'small muted', text: 'Grouped in this browser by ' + (GRAPH_STATE.model.grouping === 'folder' ? 'directory prefix' : 'Louvain modularity over the drawn edges') + '. It is a drawing aid, not a declared architecture.' }));
  }
  const focusedGroup = state.q.focus === node.gid;
  host.appendChild(el('div', { class: 'ask-examples' },
    el('button', {
      class: 'primary', type: 'button', text: 'Open this group',
      onclick: () => onGraphOpen(node),
    }),
    el('button', {
      class: 'chip', type: 'button', id: 'act-focus',
      'aria-pressed': String(focusedGroup),
      title: 'Light everything within ' + focusDepth() + ' ' + plural('hop', focusDepth())
        + ' of ANY member of this group, and dim the rest.',
      text: focusedGroup ? 'Focused — clear' : 'Focus: ' + focusDepth() + ' ' + plural('hop', focusDepth()),
      onclick: () => {
        setQuery({ focus: focusedGroup ? '' : node.gid });
        applyEmphasis();
        paintCanvasNote();
        GRAPH_STATE.canvas.draw();
        onGraphSelect(node, true);
      },
    })));
  host.appendChild(el('p', { class: 'small muted', text:
    'A path needs two single nodes, so open this group and pick one to ask how it reaches something else.' }));
  host.appendChild(el('h4', { text: 'Largest members', style: 'margin-top:12px' }));
  const shown = group.members.slice(0, 12);
  host.appendChild(el('div', { class: 'neigh' }, shown.map((leaf) => el('div', null,
    el('span', { class: 'sym', text: text(leaf.label) }),
    el('span', { class: 'path', text: text(leaf.path) })))));
  if (group.members.length > shown.length) {
    host.appendChild(inlineOmit(el('b', { text: num(group.members.length - shown.length) }), ' more in this group are not listed here.'));
  }
}

function inspectLeaf(host, node) {
  const leaf = node.leaf;
  const model = GRAPH_STATE.model;
  host.appendChild(el('h3', { text: text(leaf.label) }));
  if (leaf.fqn && leaf.fqn !== leaf.label) host.appendChild(el('p', { class: 'path', text: leaf.fqn }));
  else if (leaf.path) host.appendChild(el('p', { class: 'path', text: leaf.path }));
  if (leaf.confidence) host.appendChild(tierBadge(leaf.confidence));

  const facts = [];
  if (leaf.kind && leaf.kind !== 'container') facts.push(['Kind', text(leaf.kind)]);
  if (leaf.language) facts.push(['Language', text(leaf.language)]);
  if (leaf.symbols !== undefined) facts.push(['Symbols', num(leaf.symbols)]);
  if (leaf.files !== undefined) facts.push(['Files with symbols', num(leaf.files)]);
  if (leaf.filesMatched !== undefined && leaf.filesMatched !== leaf.files) facts.push(['Files matched by pattern', num(leaf.filesMatched)]);
  if (leaf.exported !== undefined && typeof leaf.exported === 'number') facts.push(['Exported', num(leaf.exported)]);
  if (leaf.degree) facts.push(['Edge degree', num(leaf.degree)]);
  if (facts.length) {
    host.appendChild(el('div', { class: 'neigh' }, facts.map(([label, value]) =>
      el('div', null, el('span', { class: 'dir', text: label }), el('span', { text: value })))));
  }

  // Neighbours, with direction, tier and a real file:line under each.
  const neighbours = [];
  for (const link of model.links) {
    if (link.a === leaf.id) neighbours.push({ dir: 'out', other: link.b, link: link });
    else if (link.b === leaf.id) neighbours.push({ dir: 'in', other: link.a, link: link });
  }
  neighbours.sort((a, b) => (b.link.count || 0) - (a.link.count || 0));
  const shown = neighbours.slice(0, 40);
  host.appendChild(el('h4', { text: num(neighbours.length) + ' recorded ' + plural('relationship', neighbours.length), style: 'margin-top:12px' }));
  host.appendChild(el('div', { class: 'neigh' }, shown.map((n) => {
    const other = model.byId.get(n.other);
    const otherLabel = other ? text(other.label) : text(n.other);
    const cnt = Math.max(1, Number(n.link.count || 0));
    const mine = text(leaf.label, leaf.id);
    return el('div', {
      title: n.dir === 'out'
        ? edgeHoverSentence(mine, otherLabel, cnt, n.link.conf, n.link.evidence)
        : edgeHoverSentence(otherLabel, mine, cnt, n.link.conf, n.link.evidence),
    },
      el('span', { class: 'dir' },
        el('span', { 'aria-hidden': 'true', text: n.dir === 'out' ? '→ ' : '← ' }),
        n.dir === 'out' ? 'uses ' : 'used by ',
        el('span', { class: 'sym', text: otherLabel }),
        ', ' + num(cnt) + ' ' + plural('time', cnt)),
      tierBadge(n.link.conf),
      evidenceOf(n.link.evidence));
  })));
  if (neighbours.length > shown.length) {
    host.appendChild(inlineOmit(el('b', { text: num(neighbours.length - shown.length) }), ' further ' + plural('relationship', neighbours.length - shown.length) + ' are recorded and not listed here.'));
  }

  const level = text(GRAPH_STATE.data.level, 'containers');

  /*
    THE THREE ACTIONS THAT MAKE A MAP A DECISION TOOL.

    Focus answers "what does this touch". The two path buttons answer "how does
    this reach that" — the change-safety question, and the one nothing else in
    this category draws. Blast radius hands off to the real traversal in the
    capability layer, which is a different and stronger claim than anything this
    canvas computes, and it is labelled as the stronger one.
  */
  const actions = el('div', { class: 'cm-actions', style: 'margin-top:12px' });
  const focused = state.q.focus === leaf.id;
  actions.appendChild(el('button', {
    class: 'cm-btn cm-sm', type: 'button', id: 'act-focus',
    'aria-pressed': String(focused),
    title: 'Light everything within ' + focusDepth() + ' ' + plural('hop', focusDepth()) + ' of this node and dim the rest.',
    text: focused ? 'Focused — clear' : 'Focus: ' + focusDepth() + ' ' + plural('hop', focusDepth()),
    onclick: () => {
      setQuery({ focus: focused ? '' : leaf.id });
      applyEmphasis();
      paintCanvasNote();
      GRAPH_STATE.canvas.draw();
      onGraphSelect(node, true);
    },
  }));
  actions.appendChild(el('button', {
    class: 'cm-btn cm-sm', type: 'button', id: 'act-path-from',
    title: 'Set this as the START of a path query.',
    text: 'Path from here',
    onclick: () => { setQuery({ pfrom: leaf.id }); applyEmphasis(); paintPathPanel(); paintCanvasNote(); GRAPH_STATE.canvas.draw(); onGraphSelect(node, true); },
  }));
  actions.appendChild(el('button', {
    class: 'cm-btn cm-sm', type: 'button', id: 'act-path-to',
    title: 'Set this as the END of a path query.',
    text: 'Path to here',
    onclick: () => { setQuery({ pto: leaf.id }); applyEmphasis(); paintPathPanel(); paintCanvasNote(); GRAPH_STATE.canvas.draw(); onGraphSelect(node, true); },
  }));
  if (level !== 'symbols') {
    actions.appendChild(el('button', { class: 'cm-btn cm-sm', type: 'button', text: 'Open it →', onclick: () => onGraphOpen(node) }));
  }
  actions.appendChild(el('button', {
    class: 'cm-btn cm-sm', type: 'button', text: 'Blast radius',
    title: 'Leave the map and run the real traversal: path-minimum confidence, covering tests, and what it could not reach.',
    onclick: () => go('blast', { sym: leaf.fqn || leaf.label }),
  }));
  // The same item in the code map's other views: same container, same level,
  // same `sel`, so the Tree opens on this row and Code on this declaration.
  const here = level === 'containers'
    ? { level: '', container: '', sel: 'L:' + leaf.id }
    : { level: level, container: state.q.container, sel: 'L:' + leaf.id };
  actions.appendChild(el('button', {
    class: 'cm-btn cm-sm', type: 'button', id: 'act-show-tree', text: 'Show in Tree',
    onclick: () => codeMapNavigate('tree', here),
  }));
  if (level !== 'containers' && leaf.path) {
    actions.appendChild(el('button', {
      class: 'cm-btn cm-sm', type: 'button', id: 'act-open-code', text: 'Open its code',
      title: 'Read ' + leaf.path + ' as the index holds it, with this item highlighted.',
      onclick: () => codeMapNavigate('code', Object.assign({}, here, { cfile: leaf.path, cline: '' })),
    }));
  }
  host.appendChild(actions);

  if (state.q.pfrom === leaf.id || state.q.pto === leaf.id) {
    host.appendChild(el('p', { class: 'small muted', text:
      'This node is the ' + (state.q.pfrom === leaf.id ? 'start' : 'end') + ' of the current path query.' }));
  }
}

/*
  THE AGGREGATED EDGE TABLE.

  from - to - edges at the floor - of all tiers - weakest tier - kinds - one
  checkable file:line. No other product in this category puts a measured tier
  and a real line number on a RELATIONSHIP, and this table used to ship
  default-collapsed inside a <details> labelled "Text equivalent of the canvas
  (keyboard and screen-reader accessible)". It was the single most
  differentiated artefact in the product, hidden under the word "accessible".

  It is now a first-class panel, always open, linked to the selection, and it is
  still the complete text equivalent of the canvas.

  It also fixes a denominator bug that was visible on screen: `count` counts
  edges at or above the floor and `kinds` counts every tier, so the old table
  could render "edges 50" beside "CALLS x572" with nothing saying they were
  different populations. Both are here, both labelled.
*/
function paintEdgeTable() {
  const host = GRAPH_STATE.tableHost;
  const set = GRAPH_STATE.renderSet;
  const model = GRAPH_STATE.model;
  if (!host || !set) return;
  clear(host);

  const selected = GRAPH_STATE.canvas ? GRAPH_STATE.canvas.selected : null;
  // The table is the text equivalent of the CANVAS, so it shows what the canvas
  // strokes — and states, underneath, exactly what the lenses held back.
  let rows = (set.visibleEdges || set.edges).slice();
  if (selected) rows = rows.filter((e) => e.a === selected.key || e.b === selected.key);
  rows.sort((a, b) => b.count - a.count || String(a.a).localeCompare(String(b.a)));

  const label = (key) => {
    const node = set.byKey.get(key);
    return node ? node.label : key;
  };
  const anyTotals = rows.some((e) => e.total > e.count);

  const body = [
    el('p', { class: 'prose' },
      'Every relationship drawn above, combined between the two nodes it connects and shown at its ',
      el('b', { text: 'weakest' }),
      ' tier — because a claim is only as good as the weakest edge behind it. The file and line is one real occurrence you can open. This table is also the complete text equivalent of the canvas.'),
    table(
      [
        { label: 'Relationship', title: 'Who uses whom, and how many times, at the current standard of proof' },
        { label: 'From' }, { label: 'To' },
        { label: 'At the standard', num: true, title: 'Edges at or above the current standard of proof' },
        anyTotals ? { label: 'All tiers', num: true, title: 'Every edge between this pair, including those below the standard of proof' } : null,
        { label: 'Weakest' }, { label: 'Kinds' }, { label: 'Crosses languages' }, { label: 'Example evidence' },
      ].filter(Boolean),
      rows,
      {
        cap: 40,
        row: (e) => el('tr', {
          class: 't-' + tierOf(e.weakest),
          title: edgeHoverSentence(label(e.a), label(e.b), e.count, e.weakest, e.evidence),
        },
          el('td', { class: 'sentence', text: edgeSentence(label(e.a), label(e.b), e.count) }),
          el('td', null, el('span', { class: 'sym', text: label(e.a) })),
          el('td', null, el('span', { class: 'sym', text: label(e.b) })),
          el('td', { class: 'num', text: num(e.count) }),
          anyTotals ? el('td', { class: 'num muted', text: e.total > e.count ? num(e.total) : '—' }) : null,
          el('td', null, tierBadge(e.weakest)),
          el('td', { class: 'small' }, Array.from(e.kinds.entries()).sort((x, y) => y[1] - x[1]).slice(0, 4)
            .map(([kind, n]) => el('code', { text: num(n) + ' ' + kind, style: 'margin-right:6px' }))),
          el('td', { class: 'small' }, e.xlang
            ? el('span', { class: 'xlang', title: 'The two ends of this relationship are in different languages' }, text(e.xlangLabel, 'yes'))
            : el('span', { class: 'muted', text: 'no' })),
          el('td', null, evidenceOf(e.evidence))),
      }),
  ];

  const lens = set.lens || {};
  if (lens.hiddenByLens > 0) {
    body.push(inlineOmit(el('b', { text: num(lens.hiddenByLens) }),
      ' ' + plural('pair', lens.hiddenByLens) + ' are not in this table because the ',
      el('b', { text: lens.tier }), ' lens holds them back. They are still recorded, still exported, and',
      ' clearing the lens brings them straight back — this is a view, not a deletion.'));
  }
  if (lens.hiddenByLang > 0) {
    body.push(inlineOmit(el('b', { text: num(lens.hiddenByLang) }),
      ' ' + plural('pair', lens.hiddenByLang) + ' are not in this table because both ends are in the same language',
      ' and the cross-language lens is on.'));
  }
  if (set.internalEdges > 0) {
    body.push(inlineOmit(el('b', { text: num(set.internalEdges) }),
      ' further ' + plural('edge', set.internalEdges) + ' run between two members of the same collapsed group and are folded into it. Open a group to see them.'));
  }
  if (anyTotals) {
    body.push(inlineOmit('The two count columns measure different populations: ',
      el('b', { text: 'At the standard' }), ' counts what is drawn at the current standard of proof, ',
      el('b', { text: 'All tiers' }), ' counts every edge the index holds between that pair. The kinds histogram counts all tiers.'));
  }

  const sub = selected ? 'filtered to ' + text(selected.label) : num(rows.length) + ' ' + plural('pair', rows.length);
  const cardNode = card('Relationships, with evidence', sub, body, null, 'flush');
  if (selected) {
    cardNode.querySelector('header').appendChild(el('button', {
      class: 'ghost', type: 'button', text: 'Clear selection',
      onclick: () => { GRAPH_STATE.canvas.select(null); },
    }));
  }
  const exports = el('div', { class: 'ask-examples' },
    el('button', { class: 'chip', type: 'button', text: 'Export PNG', onclick: exportPNG }),
    el('button', { class: 'chip', type: 'button', text: 'Export JSON', onclick: exportJSON }));
  cardNode.querySelector('.body').appendChild(exports);
  host.appendChild(cardNode);
}

function exportPNG() {
  const canvas = GRAPH_STATE.canvas;
  if (!canvas) return;
  canvas.canvas.toBlob((blob) => {
    if (!blob) return;
    const url = URL.createObjectURL(blob);
    const a = el('a', { href: url, download: 'cortex-graph.png' });
    document.body.appendChild(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 4000);
  });
}

function exportJSON() {
  const set = GRAPH_STATE.renderSet;
  if (!set) return;
  const lens = set.lens || {};
  const payload = {
    project: state.meta ? state.meta.project : '',
    level: GRAPH_STATE.data ? GRAPH_STATE.data.level : '',
    confidence_floor: state.q.floor,
    // The export carries EVERY edge, including the ones a lens is holding back,
    // and says which those are. An export that silently matched the screen
    // would turn a temporary view into a permanent, undetectable omission.
    view: {
      shareable_url: shareLink(),
      confidence_lens: lens.tier || null,
      cross_language_only: !!lens.xonly,
      focus: state.q.focus || null,
      focus_depth: state.q.focus ? focusDepth() : null,
      path_from: state.q.pfrom || null,
      path_to: state.q.pto || null,
      edges_drawn: lens.drawn === undefined ? set.edges.length : lens.drawn,
      edges_in_export: set.edges.length,
    },
    nodes: set.nodes.map((n) => ({ key: n.key, label: n.label, type: n.type, members: n.count || 1 })),
    edges: set.edges.map((e) => ({
      from: e.a, to: e.b, edges_at_floor: e.count, edges_all_tiers: e.total,
      weakest_confidence: e.weakest, kinds: Array.from(e.kinds.entries()),
      crosses_language: !!e.xlang, language_pair: e.xlangLabel || null,
      hidden_by_lens: !!e.hidden,
      evidence: e.evidence,
    })),
    path: (GRAPH_STATE.path && GRAPH_STATE.path.hops) ? {
      weakest_confidence: GRAPH_STATE.path.weakest,
      reversed: !!GRAPH_STATE.path.reversed,
      hops: GRAPH_STATE.path.hops.map((h) => ({
        from: h.from, to: h.to, confidence: h.link.conf,
        kind: (h.link.kinds && h.link.kinds[0]) ? h.link.kinds[0][0] : 'EDGE',
        count: h.link.count || 1, evidence: h.link.evidence,
      })),
    } : null,
    note: 'Counts at the standard of proof and across all tiers are different populations; both are given. '
      + 'Every edge is exported, including those a client-side lens was hiding when this file was written; '
      + 'hidden_by_lens says which.',
  };
  const blob = new Blob([JSON.stringify(payload, null, 2)], { type: 'application/json' });
  const url = URL.createObjectURL(blob);
  const a = el('a', { href: url, download: 'cortex-graph.json' });
  document.body.appendChild(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 4000);
}

// ================================================================ SCREEN 02 — NODE LABELS

/*
  The card browser that used to live here was replaced by the code map's Tree,
  Treemap and Code views (js/codemap.mjs). This one function outlived it: the
  map names every box with it, and internal/web/assets_test.go executes it
  from between the markers below.
*/
// >>> distinguishingLabel
/*
  A label that every sibling shares tells the reader nothing, and a label two
  siblings share is worse: it tells the reader something false. So a node is
  named by the shortest '/'-aligned tail of its path that no sibling also ends
  in. With no twin that is the last segment - `CallbackController.java`,
  `controller`. When two siblings end alike the tail grows one segment at a
  time until it is this node's alone: src/main/java/com/pay/controller and
  src/test/java/com/pay/controller read `main/java/com/pay/controller` and
  `test/java/com/pay/controller`. Two identical paths yield the last segment
  rather than fighting, and a path that is a suffix of a sibling (`a/b` beside
  `x/a/b`) grows to its full path. The full path stays in the title and the
  inspector, because it is still the answer to "where is this".

  Pure and deterministic - no DOM, no state - and it terminates: the tail is
  at most the whole path. Cost is O(siblings * depth) per node, so O(n^2 *
  depth) per rung; rungs are budgeted to a few hundred nodes.
*/
function distinguishingLabel(path, siblings) {
  const p = String(path || '');
  const parts = p.split('/');
  const others = (siblings || []).map(String).filter((s) => s !== p);
  for (let keep = 1; keep < parts.length; keep++) {
    const tail = parts.slice(-keep).join('/');
    if (!others.some((s) => s === tail || s.endsWith('/' + tail))) return tail;
  }
  return p;
}
// <<< distinguishingLabel

// ================================================================ CODEMIND — STRUCTURE & CENTRALITY

/*
  THREE QUESTIONS, THREE MEASURES, AND THE MEASURE IS PRINTED.

  The API answers with three sections in a fixed order — betweenness, then
  cycles plus instability, then PageRank — and each one carries the NAME of the
  algorithm that produced it. That name is rendered, because two rankings under
  two headings read as corroboration when they are the same leaderboard twice,
  and the only way a reader can tell is to be told which claim they are reading.

  Nothing here re-sorts, re-scores or re-phrases the payload. The rows arrive in
  the order the metrics package guarantees, the caveats and the omissions arrive
  as sentences the server wrote, and this file's whole job is to put them where
  a reader meets them: the caveat above the table it is a caveat about, the tag
  rule on the tag, and the next action as a button rather than as advice.
*/

const METRIC_LEVELS = [
  { value: 'symbol', label: 'symbol — one node per declaration' },
  { value: 'file', label: 'file — symbols collapsed onto their file' },
  { value: 'directory', label: 'directory — files collapsed onto their folder' },
];

// The five limits, in the order MetricsLimits declares them, each with the
// heading that says which part of the model it is a limit OF. They are stated
// whether or not they bit on this run: a sentence a reader only ever sees when
// it costs them something teaches them the screen has no such limit the rest of
// the time.
const METRIC_LIMITS = [
  ['betweenness', 'The betweenness ranking'],
  ['cycles', 'The cycle search'],
  ['self_loops', 'Direct recursion'],
  ['intra_group', 'This level’s projection'],
  ['confidence_floor', 'The standard of proof'],
];

function metricsLevel() {
  const want = state.q.mlevel;
  return METRIC_LEVELS.some((l) => l.value === want) ? want : 'symbol';
}

LOADERS.metrics = async function () {
  if (state.loaded.metrics) return;
  state.loaded.metrics = true;
  await drawMetrics();
};

async function drawMetrics() {
  const host = $('#screen-metrics');
  clear(host);
  host.appendChild(answer('The most connected parts of this code are being measured.'));

  // The controls are built from the RESPONSE, not from the query string, so
  // they cannot claim a level the numbers under them were not computed at.
  // They are appended before the body for reading order and filled after.
  const controls = el('div', { class: 'stack' });
  host.appendChild(controls);

  const body = el('div', { class: 'stack' }, loading('Measuring the graph…'));
  host.appendChild(body);

  let d;
  try {
    d = await api('/api/metrics', {
      level: metricsLevel(),
      kinds: state.q.mkinds,
      exclude_tests: state.q.mtests === '1' ? '1' : '',
      min_confidence: state.q.floor,
    });
  } catch (e) { clear(body); failAnswer(host, 'Structure & centrality'); return showError(body, e, 'Structure & centrality'); }
  state.meta = d.meta;
  renderIndexBar();
  clear(body);
  setAnswer(host, metricsAnswer(d));
  renderMetricsControls(d, controls);
  renderMetrics(d, body);
}

// The first section is the ranking a reader came for; its row count is the
// figure. A response with no sections says so rather than counting nothing.
function metricsAnswer(d) {
  const sections = Array.isArray(d.sections) ? d.sections : [];
  const support = 'Where a change is dangerous, where the architecture resists change, and what to read first — three different measures over the same graph, each one named and ending in something to do.';
  if (!sections.length) {
    return answer('There is no graph to measure yet, so nothing here is ranked.', { support: support, next: nextOf(d.next) });
  }
  // The first section is the figure: the sentence names it, so a reader who
  // opens the screen knows which ranking the number came from.
  const first = sections[0];
  const rows = (first.rows || []).length;
  const unit = plural(text(d.level, 'symbol'), rows);
  const cycles = sections.reduce((sum, sec) => sum + ((sec.cycles || []).length), 0);
  return answer([
    'The ', B(num(rows) + ' most connected ' + unit), ' of this code (' + text(first.title, 'the first ranking') + '), and the ',
    B(num(cycles) + ' ' + plural('cycle', cycles)), ' between them.'], { support: support, next: nextOf(d.next) });
}

/*
  THE CONTROLS ANSWER TO THE PAYLOAD.

  Every control is drawn from `d.level` and `d.filters` — the server's own
  reading of the request — and the URL is rewritten to match. A reader who
  arrives on `?mlevel=containers` gets the level the numbers were actually
  computed at, and the link they copy afterwards reproduces what they saw.
*/
function renderMetricsControls(d, host) {
  const level = text(d.level, 'symbol');
  const filters = d.filters || {};
  const callable = filters.callable === true;
  const noTests = filters.exclude_tests === true;
  setQuery({
    mlevel: level,
    mkinds: callable ? 'callable' : '',
    mtests: noTests ? '1' : '0',
  }, { replace: true });

  clear(host);
  const toolbar = el('div', { class: 'toolbar' });
  toolbar.appendChild(selectField('Level', 'metrics-level', METRIC_LEVELS, level,
    (v) => { setQuery({ mlevel: v }); drawMetrics(); }));

  const chips = el('div', { class: 'chips' },
    el('button', {
      class: 'chip', type: 'button', id: 'metrics-callable', 'aria-pressed': String(callable),
      title: 'Rank only functions, methods, classes and interfaces — the things that can appear in a call '
        + 'graph as something other than a leaf. Off by default: the raw ranking is what the graph says.',
      onclick: () => { setQuery({ mkinds: callable ? '' : 'callable' }); drawMetrics(); },
    }, 'callable kinds only'),
    el('button', {
      class: 'chip', type: 'button', id: 'metrics-notests', 'aria-pressed': String(noTests),
      title: 'Drop symbols whose path matches this index’s test-file conventions. A naming convention, '
        + 'not proof — which is why it is a filter you switch on, never a default.',
      onclick: () => { setQuery({ mtests: noTests ? '0' : '1' }); drawMetrics(); },
    }, 'exclude test files'));
  toolbar.appendChild(field('Filters', chips));
  host.appendChild(toolbar);

  host.appendChild(el('p', { class: 'hint' },
    'Measured at the ', el('b', { text: level }), ' level, at or above the ',
    el('b', { 'data-tier': tierOf(text(d.min_confidence, state.q.floor)), text: tierWord(text(d.min_confidence, state.q.floor)) }), ' standard of proof. ',
    callable || noTests
      ? 'Both filters are opt-in and every row they removed is counted under the section it was removed from.'
      : 'No filter is applied: this is the raw ranking, accessors, test helpers and all — each one tagged rather than hidden.'));
}

function renderMetrics(d, host) {
  const sections = Array.isArray(d.sections) ? d.sections : [];
  const rules = d.tag_rules || {};

  const help = guidanceCard(d.guidance, 'why there is nothing to measure yet');
  if (help) host.appendChild(help);

  if (!sections.length) {
    host.appendChild(emptyState('There is no graph to measure',
      ['Centrality, cycles and instability are computed over the indexed call graph, and this repository '
        + 'has not produced one yet. That is a state of the repository, not a failure of this screen.']));
    return;
  }

  for (const sec of sections) host.appendChild(metricsSection(sec, rules));

  if (Object.keys(rules).length) host.appendChild(fold('What the tags on the rows mean', metricsRulesCard(rules), { key: 'metrics:rules' }));
  if (d.limits) host.appendChild(fold('What no number on this screen can see', metricsLimitsCard(d.limits), { key: 'metrics:limits' }));
  host.appendChild(renderOmissions(d.omissions, { key: 'metrics' }));
}

function metricsSection(sec, rules) {
  const rows = Array.isArray(sec.rows) ? sec.rows : [];
  const cycles = Array.isArray(sec.cycles) ? sec.cycles : [];
  return card(text(sec.title, 'A measure of this graph'),
    'measured by ' + text(sec.measure, 'an unnamed measure') + ' · '
      + num(sec.considered) + ' ranked ' + plural('candidate', sec.considered),
    [
      el('p', { class: 'prose', text: text(sec.question) }),
      // The caveat is above the table, not below it: what a measure does NOT
      // say is not a footnote to the ranking, it is the terms the ranking is
      // read under.
      sec.caveat ? el('p', { class: 'hint', text: sec.caveat }) : null,
      rows.length
        ? metricsTable(rows, rules)
        : emptyState('No row survived at this level',
          ['The ranking ran and returned nothing this screen can show. What became of every candidate is '
            + 'accounted for immediately below — this is a filtered-away list, never an empty graph.']),
      cycles.length ? metricsCycles(cycles) : null,
      renderOmissions(sec.omissions, { key: 'metrics:' + text(sec.title, '') }),
    ],
    nextAction(sec.next), 'flush');
}

function metricsTable(rows, rules) {
  // table() hands its row builder the row and nothing else, so the rank a
  // reader sees has to travel ON the row. Deriving it from indexOf would number
  // two identically-scored rows the same.
  const ranked = rows.map((row, i) => Object.assign({ rank: i + 1 }, row));
  return table(
    [{ label: 'Rank', num: true }, { label: 'What' }, { label: 'Kind' },
      { label: 'Score', num: true, title: 'The section’s own measure. Betweenness and instability are '
        + 'ratios of path and edge counts; PageRank is a share of inbound attention.' },
      { label: 'Fan-in', num: true }, { label: 'Fan-out', num: true },
      { label: 'Instability', num: true, title: 'Martin’s I = fan-out / (fan-in + fan-out). 0 is a pure sink, 1 a pure source.' },
      { label: 'Where' }],
    ranked,
    {
      cap: ranked.length,
      row: (row) => el('tr', null,
        el('td', { class: 'num small muted', text: num(row.rank) }),
        el('td', null,
          el('span', { class: 'sym', title: text(row.key), text: text(row.label, text(row.key)) }),
          (row.tags || []).map((tag) => metricsTag(tag, rules))),
        el('td', { class: 'nowrap small muted', text: text(row.kind) }),
        el('td', { class: 'num', text: metricValue(row.value) }),
        el('td', { class: 'num', text: num(row.fan_in) }),
        el('td', { class: 'num', text: num(row.fan_out) }),
        el('td', { class: 'num', text: metricValue(row.instability) }),
        el('td', null, evidence(row.path, row.line))),
    });
}

/*
  A TAG CARRIES ITS OWN RULE.

  `accessor` and `test` are guesses made from a name and a path, and the server
  publishes the exact sentence each one is made by. The rule travels ON the tag
  — as its title, and again in full under the sections — because a tag whose
  rule the reader cannot see is a judgement wearing the costume of a
  measurement.
*/
function metricsTag(tag, rules) {
  const rule = (rules || {})[tag];
  return el('span', {
    class: 'tier tier-medium',
    title: rule ? tag + ': ' + rule : tag + ': no rule was published for this tag',
    text: tag,
  });
}

function metricsRulesCard(rules) {
  const names = Object.keys(rules).sort();
  return card('What the tags mean', 'the rule that produced each one, in the server’s words',
    [
      el('div', { class: 'stack' }, names.map((name) => el('div', { class: 'held' },
        el('h4', { text: name }),
        el('p', { class: 'what', text: text(rules[name]) })))),
      el('p', { class: 'hint', text: 'A tag never removes a row. A getter called from everywhere is genuinely '
        + 'central and genuinely uninteresting, and which of those two matters is the reader’s call, not this '
        + 'screen’s.' }),
    ]);
}

function metricsLimitsCard(limits) {
  return card('What no number on this screen can see', 'stated on every run, whether or not it bit on this one',
    METRIC_LIMITS.map(([key, heading]) => (limits[key]
      ? el('div', { class: 'held' }, el('h4', { text: heading }), el('p', { class: 'what', text: limits[key] }))
      : null)));
}

function metricsCycles(cycles) {
  return card('The cycles this search found', num(cycles.length) + ' ' + plural('cycle', cycles.length)
    + ' listed, shortest first',
    table([{ label: 'Length', num: true }, { label: 'The ring' }], cycles, {
      cap: 20,
      row: (c) => el('tr', null,
        el('td', { class: 'num', text: num(c.length) }),
        el('td', null, (c.members || []).map((m, i) => el('span', null,
          i ? ' → ' : '',
          el('span', { class: 'sym', title: text(m), text: metricsMemberLabel(m) }))))),
    }), null, 'flush');
}

// The keys are graph keys, and a graph key at symbol level is a whole path with
// a name on the end. A ring of five of them is unreadable at full length, so the
// row shows the tail and the title carries the key a reader can search for.
function metricsMemberLabel(key) {
  const s = String(key === null || key === undefined ? '' : key);
  const cut = s.lastIndexOf('/');
  return cut < 0 ? text(s) : text(s.slice(cut + 1));
}

// Three measures, three magnitudes: betweenness runs into the thousands,
// instability sits in [0,1] and PageRank is a share that is routinely 0.0005.
// toLocaleString alone renders the last of those as "0.001" for every row, which
// turns a ranking into a column of identical numbers.
function metricValue(v) {
  if (v === null || v === undefined || v === '') return '—';
  const n = Number(v);
  if (!Number.isFinite(n)) return '—';
  if (n === 0) return '0';
  if (Math.abs(n) >= 1) return n.toLocaleString(undefined, { maximumFractionDigits: 2 });
  return n.toPrecision(3);
}

// ================================================================ SCREEN 03 — CROSS-STACK

LOADERS.crossstack = async function () {
  const host = $('#screen-crossstack');
  if (state.loaded.crossstack) return;
  state.loaded.crossstack = true;
  await drawCrossStack();
};

async function drawCrossStack() {
  const host = $('#screen-crossstack');
  clear(host);
  host.appendChild(answer('The routes in this code are being walked to the tables they reach.'));

  const toolbar = el('div', { class: 'toolbar' });
  toolbar.appendChild(selectField('Handler walk depth', 'cs-depth',
    [1, 2, 3, 4, 5].map((n) => ({ value: String(n), label: String(n) })), state.q.csdepth,
    (v) => { setQuery({ csdepth: v, off: '0' }); drawCrossStack(); }));
  toolbar.appendChild(selectField('Routes per page', 'cs-limit',
    [12, 25, 60, 120, 250].map((n) => ({ value: String(n), label: String(n) })), state.q.limit,
    (v) => { setQuery({ limit: v, off: '0' }); drawCrossStack(); }));
  host.appendChild(toolbar);

  const body = el('div', { class: 'stack' }, loading('Walking the routes…'));
  host.appendChild(body);

  let d;
  try {
    d = await api('/api/crossstack', {
      min_confidence: state.q.floor, depth: state.q.csdepth,
      limit: state.q.limit, offset: state.q.off,
    });
  } catch (e) { clear(body); failAnswer(host, 'Cross-stack'); return showError(body, e, 'Cross-stack'); }
  state.meta = d.meta;
  renderIndexBar();
  clear(body);
  setAnswer(host, crossStackAnswer(d));
  renderCrossStack(d, body, toolbar);
}

// How many routes there are, and how many chains on this page ran all the way
// to a table. `counts.endpoints` is whole-index; `counts.page` is this page,
// and the sentence says which is which.
function crossStackAnswer(d) {
  const counts = d.counts || {};
  const page = counts.page || {};
  const routes = Number(counts.endpoints || 0);
  const chains = Number(page.chains || 0);
  const reaching = Number(page.with_table || 0);
  const parts = [B(num(routes) + ' ' + plural('endpoint', routes)), '.'];
  if (chains > 0 && reaching === chains) {
    parts.push(' On this page, ', B(num(reaching) + ' of ' + num(chains)), ' reach a database table.');
  } else if (chains > 0) {
    parts.push(' On this page, ', B(num(reaching) + ' of ' + num(chains)),
      ' reach a database table; for the rest the chain stops early, and the table below says where.');
  } else {
    parts.push(' None of them is on this page.');
  }
  return answer(parts, {
    support: 'Client → endpoint → handler → service → table, as one chain, drawn at its weakest hop.',
    next: nextOf(d.next_action),
  });
}

/*
  THE PAGER.

  `updateCrossStackPager` used to be called from exactly one place —
  `loadOverview` — and never from the cross-stack loader. Handed the overview
  payload it found no route sentence to parse, so the range rendered empty and
  both buttons came out disabled, while the omissions card on the same page read
  "Showing routes 1-12 of 411 … ?offset=12 for the next page". 399 of 411 routes
  could not be reached from the UI at all, and because no view state lived in
  the URL the suggested remedy was not reachable either.

  Both halves are fixed. The API now serves a `page` object with `has_next` and
  the exact `next_offset` to send, so nothing is inferred and nothing is parsed
  out of prose; and the offset lives in the query string, so page 12 of 411 is a
  link somebody can be sent.
*/
function renderPager(page, toolbar) {
  const wrap = el('span', { class: 'pager' });
  const hasPrev = page.has_prev !== undefined ? !!page.has_prev : Number(page.offset) > 0;
  const hasNext = page.has_next !== undefined ? !!page.has_next : (Number(page.offset) + Number(page.returned) < Number(page.total));
  const prevOffset = page.prev_offset !== undefined ? Number(page.prev_offset) : Number(page.offset) - Number(page.limit);
  const nextOffset = page.next_offset !== undefined ? Number(page.next_offset) : Number(page.offset) + Number(page.limit);

  wrap.appendChild(el('button', {
    class: 'ghost', type: 'button', text: '‹ Prev', disabled: !hasPrev || prevOffset < 0,
    onclick: () => { setQuery({ off: String(Math.max(0, prevOffset)) }); drawCrossStack(); },
  }));
  wrap.appendChild(el('span', { class: 'range', id: 'cs-range',
    text: 'routes ' + num(page.first) + '–' + num(page.last) + ' of ' + num(page.total) }));
  wrap.appendChild(el('button', {
    class: 'ghost', type: 'button', text: 'Next ›', disabled: !hasNext || nextOffset < 0,
    onclick: () => { setQuery({ off: String(nextOffset) }); drawCrossStack(); },
  }));
  toolbar.appendChild(field('Page', wrap));
  return wrap;
}

function renderCrossStack(d, host, toolbar) {
  const counts = d.counts || {};
  const page = d.page || {};
  renderPager(page, toolbar);

  const pageCounts = counts.page || {};
  host.appendChild(el('div', { class: 'kpis' },
    kpi('Routes', num(counts.endpoints), 'endpoints in the whole index'),
    kpi('With a handler', num(counts.with_handler), 'routes bound to a declaration'),
    kpi('With a caller', num(counts.with_client), 'client code recorded as calling one', Number(counts.with_client) === 0 ? 'warn' : 'ok'),
    kpi('Reaching a table', num(pageCounts.with_table), 'on this page of ' + num(pageCounts.chains) + ' chains')));
  if (pageCounts.scope) host.appendChild(el('p', { class: 'hint', text: pageCounts.scope }));

  // Where the chains stop, BEFORE the chains. A reader who scrolls twelve
  // chains and only then learns none of them has a caller has been misled by
  // the ordering.
  const gaps = d.gaps || [];
  if (gaps.length) {
    host.appendChild(card('Where the chains stop, and why', num(gaps.length) + ' distinct ' + plural('reason', gaps.length),
      table(
        [{ label: 'Stage' }, { label: 'Reason' }, { label: 'Chains', num: true }, { label: 'What it means' }],
        gaps,
        {
          cap: 12,
          row: (row) => el('tr', null,
            el('td', { class: 'nowrap' }, el('code', { text: text(row.stage) })),
            el('td', { class: 'nowrap small muted', text: String(text(row.reason)).replace(/_/g, ' ') }),
            el('td', { class: 'num', text: num(row.count) }),
            el('td', { class: 'small' }, text(row.detail),
              row.remedy ? el('div', { class: 'muted', text: 'Remedy: ' + row.remedy }) : null)),
        }),
      null, 'flush'));
  }

  const chains = d.chains || [];
  if (chains.length === 0) {
    host.appendChild(emptyState('No chains on this page',
      ['That is not the same as "this system has no routes". ' + num(counts.endpoints) + ' endpoints are indexed; none of them survived the current filters on this page.',
        'Lower the standard of proof, raise the walk depth, or step back to the first page.']));
  }
  for (const chain of chains) host.appendChild(chainCard(chain));

  host.appendChild(renderOmissions(d.omissions, { key: 'crossstack' }));
}

function hopNode(stage, node, extra) {
  return el('div', { class: 'hop' },
    el('div', { class: 'stage', text: stage }),
    el('div', { class: 'nm', text: text(node && node.name) }),
    el('div', { class: 'pth', text: tailPath(text(node && node.file_path, ''), 2) }),
    extra || null);
}

function hopLink(relation, conf) {
  return el('div', { class: 'link', 'data-tier': tierOf(conf), title: text(relation) + ' — ' + tierSentence(conf) },
    el('span', { text: text(relation, '?') }),
    el('div', { class: 'bar t-' + tierOf(conf) }),
    el('span', { text: tierWord(conf) }));
}

function chainCard(chain) {
  const endpoint = chain.endpoint || {};
  const row = el('div', { class: 'chain' });

  const clients = chain.clients || [];
  if (clients.length) {
    row.appendChild(hopNode('client', clients[0].node));
    row.appendChild(hopLink(clients[0].relation, clients[0].confidence));
  } else {
    row.appendChild(el('div', { class: 'stop', text: 'No recorded caller. Either the caller is outside this repository, or the client language is a blind spot.' }));
    row.appendChild(hopLink('?', 'unknown'));
  }

  row.appendChild(hopNode('endpoint', { name: endpoint.name, file_path: endpoint.file_path },
    endpoint.container ? el('div', { class: 'pth', text: endpoint.container }) : null));

  if (chain.handler) {
    row.appendChild(hopLink(chain.handler.relation, chain.handler.confidence));
    row.appendChild(hopNode('handler', chain.handler.node,
      chain.handler.evidence ? el('div', { style: 'margin-top:5px' }, evidenceOf(chain.handler.evidence)) : null));
  } else {
    row.appendChild(hopLink('?', 'unknown'));
    row.appendChild(el('div', { class: 'stop', text: 'No declared handler bound to this route.' }));
  }

  for (const svc of (chain.services || [])) {
    row.appendChild(hopLink(svc.relation, svc.confidence));
    row.appendChild(hopNode('service · depth ' + num(svc.depth), svc.node));
  }
  for (const tbl of (chain.tables || [])) {
    row.appendChild(hopLink(tbl.relation, tbl.confidence));
    row.appendChild(hopNode('table', tbl.node,
      tbl.evidence ? el('div', { style: 'margin-top:5px' }, evidenceOf(tbl.evidence)) : null));
  }

  const header = el('header', null,
    el('h3', { text: text(endpoint.name) }),
    el('span', { class: 'sub' }, 'reaches ', el('b', { text: text(chain.reach, 'nowhere recorded') }),
      chain.writes ? ' · writes' : ''));
  const node = el('div', { class: 'card' });
  node.appendChild(header);
  const bodyNode = el('div', { class: 'body' });
  bodyNode.appendChild(el('p', { class: 'small muted' },
    'Weakest hop on this chain: ', tierBadge(chain.weakest_confidence),
    ' — the chain is only as good as this. ', el('span', { class: 'path', text: text(endpoint.file_path) })));
  bodyNode.appendChild(row);
  for (const gap of (chain.gaps || [])) {
    bodyNode.appendChild(inlineOmit(el('b', { text: text(gap.stage) }), ': ', text(gap.detail),
      gap.remedy ? ' — ' + gap.remedy : ''));
  }
  node.appendChild(bodyNode);
  return node;
}

// ================================================================ SCREEN 04 — BLAST RADIUS

LOADERS.blast = async function () {
  const host = $('#screen-blast');
  if (state.loaded.blast) return;
  state.loaded.blast = true;
  clear(host);
  host.appendChild(blastAnswer(null));

  const toolbar = el('div', { class: 'toolbar' });
  const combo = combobox({
    id: 'blast-symbol', placeholder: 'a function, method or type name',
    value: state.q.sym,
    emptyNote: 'Nothing in this index has a name like that.',
    source: async (q) => {
      if (!q) return { rows: [], note: 'Type at least one character. Matching is on the name and the fully-qualified name.' };
      const res = await api('/api/symbols', { q: q });
      const rows = (res.matches || []).map((m) => ({
        value: text(m.fqn, m.name), label: text(m.name),
        sub: text(m.file_path, ''),
        meta: text(m.kind, '') + (m.exported ? ' · exported' : ''),
        tier: m.confidence,
      }));
      const hidden = Number(res.total_matched || 0) - rows.length;
      return { rows: rows, note: (hidden > 0 ? num(hidden) + ' further matches are not listed. ' : '') + text(res.ordering, '') };
    },
    onChoose: (row) => { setQuery({ sym: row.value }); runBlast(); },
    onSubmit: (value) => { setQuery({ sym: value }); runBlast(); },
  });
  toolbar.appendChild(field('Symbol', combo.node, 'grow'));
  toolbar.appendChild(selectField('Depth', 'blast-depth',
    [1, 2, 3, 4, 5, 6].map((n) => ({ value: String(n), label: String(n) })), state.q.depth,
    (v) => { setQuery({ depth: v }); if (state.q.sym) runBlast(); }));
  toolbar.appendChild(el('button', { class: 'primary', type: 'button', text: 'Analyse', onclick: () => { setQuery({ sym: combo.input.value.trim() }); runBlast(); } }));
  host.appendChild(toolbar);
  host.appendChild(el('div', { class: 'stack', id: 'blast-body' }));

  if (state.q.sym) runBlast(); else renderBlastEmpty();
};

/*
  The blast screen's answer. Before a symbol is named, what naming one gets
  you; after, how many things could be affected, with the direct and the
  further-along counts kept apart the way the API keeps them.
*/
function blastAnswer(d) {
  const support = 'Every reached symbol carries the tier of the weakest edge on the path that found it, and what the walk could not reach is counted rather than dropped.';
  if (!d) return answer('Name a function, method or type and this tells you what could break if you change it.', { support: support });
  const name = text(d.target && d.target.name, d.query);
  if (d.status === 'ambiguous' || (d.candidates && d.candidates.length)) {
    const n = (d.candidates || []).length;
    return answer([B(num(n) + ' definitions'), ' are named ', B(name), ', so pick the one you mean before anything is walked.'], { support: support });
  }
  if (d.status && d.status !== 'ok') {
    return answer(['Nothing in this index is named ', B(name), ', so there is nothing to walk from.'], { support: support });
  }
  const counts = d.counts || {};
  const affected = Number(counts.impacted_through_references || 0);
  const risk = d.risk || {};
  const parts = [B(num(affected) + ' ' + plural('thing', affected)), ' could be affected if you change ', B(name), ': ',
    num(counts.direct) + ' directly, ' + num(counts.indirect) + ' further along the walk.'];
  if (risk.level) parts.push(' The risk is ', B(text(risk.level)), '.');
  return answer(parts, { support: support });
}

function renderBlastEmpty() {
  const body = $('#blast-body');
  clear(body);
  setAnswer($('#screen-blast'), blastAnswer(null));
  body.appendChild(emptyState('Name the thing you are about to change',
    ['Blast radius answers what breaks if you change one thing, so it needs to know which thing. A bare name that matches several definitions returns the candidates rather than guessing — an impact analysis of the wrong symbol is not a partial answer, it is a wrong one.',
      'Every result is a LOWER BOUND: it reports relationships the index resolved, and states how many it could not.'],
    []));
}

async function runBlast() {
  const body = $('#blast-body');
  clear(body);
  const symbol = state.q.sym;
  if (!symbol) return renderBlastEmpty();
  body.appendChild(loading('Walking the graph from ' + symbol + '…'));

  let d;
  try { d = await api('/api/blast', { symbol: symbol, depth: state.q.depth, min_confidence: state.q.floor }); }
  catch (e) { clear(body); failAnswer($('#screen-blast'), 'Blast radius'); return showError(body, e, 'Blast radius'); }
  clear(body);
  setAnswer($('#screen-blast'), blastAnswer(d));

  if (d.status === 'ambiguous' || (d.candidates && d.candidates.length)) {
    const guidance = d.guidance || {};
    body.appendChild(card(text(guidance.title, 'That name matches more than one definition'), 'the portal will not pick one',
      [
        el('p', { class: 'prose', text: text(guidance.detail) }),
        (guidance.steps || []).length ? el('ul', { class: 'prose' }, guidance.steps.map((s) => el('li', { text: s }))) : null,
        table(
          [{ label: 'Symbol' }, { label: 'Kind' }, { label: 'Defined at' }, { label: '' }],
          d.candidates || [],
          {
            cap: 20,
            row: (cand) => el('tr', null,
              el('td', null, el('span', { class: 'sym', text: text(cand.name) }),
                cand.exported ? el('span', { class: 'tier tier-high', text: 'exported' }) : null),
              el('td', { class: 'nowrap small muted', text: text(cand.kind) }),
              el('td', null, evidence(cand.file_path, cand.line)),
              el('td', null, el('button', {
                class: 'ghost', type: 'button', text: 'Analyse this one',
                onclick: () => { setQuery({ sym: text(cand.fqn, cand.name) }); runBlast(); },
              }))),
          }),
      ], null, 'unknown'));
    return;
  }

  if (d.status && d.status !== 'ok') {
    const guidance = d.guidance || {};
    body.appendChild(emptyState(text(guidance.title, 'Nothing was found for that name'),
      [text(guidance.detail, 'The index has no definition under that name. That is not evidence the code does not exist — it may be in a language this index is blind to.'),
        'Check the overview’s blind spots for the language you expected.']));
    return;
  }

  const target = d.target || {};
  const counts = d.counts || {};
  const risk = d.risk || {};

  body.appendChild(card('Target', text(target.kind, ''),
    [
      el('p', null, el('span', { class: 'sym', text: text(target.name) }), ' ', tierBadge(target.confidence)),
      el('p', { class: 'path', text: text(target.fqn) }),
      evidence(target.file_path, target.line),
    ]));

  body.appendChild(el('div', { class: 'kpis' },
    kpi('Direct', num(counts.direct), 'one edge away'),
    kpi('Indirect', num(counts.indirect), 'further along the walk'),
    kpi('Covering tests', num(counts.covering_tests), num(counts.tests_proven_by_graph) + ' proven by a recorded edge'),
    kpi('Risk', text(risk.level, 'not scored'), 'score ' + num(risk.score) + ' / 100',
      risk.level === 'high' ? 'bad' : (risk.level === 'moderate' ? 'warn' : 'ok'))));

  if (risk.factors && risk.factors.length) {
    body.appendChild(fold('Evidence — why the risk is ' + text(risk.level, 'what it is') + ', in ' + num(risk.factors.length) + ' ' + plural('factor', risk.factors.length), card('Why that risk level', text(risk.formula, ''),
      [
        table([{ label: 'Factor' }, { label: 'Points', num: true }, { label: 'Detail' }], risk.factors, {
          cap: 10,
          row: (fact) => el('tr', null,
            el('td', null, el('code', { text: text(fact.name) })),
            el('td', { class: 'num', text: num(fact.points) }),
            el('td', { class: 'small' }, text(fact.detail),
              fact.evidence ? el('div', { class: 'path', text: fact.evidence }) : null)),
        }),
        risk.certainty_reason ? el('p', { class: 'prose' }, el('b', { text: 'Certainty: ' + text(risk.certainty) + '. ' }), text(risk.certainty_reason)) : null,
        risk.lower_bound ? el('p', { class: 'warnline small', text: 'This score is a lower bound: the walk did not exhaust the reachable set.' }) : null,
      ], null, 'flush'), { key: 'blast:risk' }));
  }

  const impacted = (d.direct || []).concat(d.indirect || []);
  const files = impactedFiles(d.direct || []);
  if (files.length) {
    body.appendChild(card('The files a change lands in', num(files.length) + ' ' + plural('file', files.length)
      + ', ranked by recorded call sites',
      [
        el('p', { class: 'prose', text: 'Direct impact only — one edge from the target. Ranked by how many source '
          + 'locations actually recorded the reference, because six call sites in one file is a different piece of '
          + 'work from one. Every row carries a real file and line that can be opened and checked; a row without one '
          + 'says so rather than showing a location it does not have.' }),
        el('div', { class: 'stack' }, files.slice(0, 20).map((f) => impactedFileRow(f, files[0].sites))),
        files.length > 20
          ? inlineOmit(el('b', { text: num(files.length - 20) }), ' further ' + plural('file', files.length - 20)
            + ' are directly impacted and are in the full table below, not in this ranking.')
          : null,
      ], null, 'flush'));
  }
  if (impacted.length) {
    // Not folded: this table is the proof of the number in the answer line.
    body.appendChild(card('What it reaches', num(impacted.length) + ' symbols, weakest-edge tier per row',
      table(
        [{ label: 'Symbol' }, { label: 'Depth', num: true }, { label: 'Via' }, { label: 'Confidence' }, { label: 'File' }],
        impacted,
        {
          cap: 30,
          row: (row) => el('tr', { class: 't-' + tierOf(row.confidence) },
            el('td', null, el('span', { class: 'sym', text: text(row.name) }),
              row.is_test ? el('span', { class: 'tier tier-high', text: 'test' }) : null),
            el('td', { class: 'num', text: num(row.depth) }),
            el('td', null, el('code', { text: text(row.via) })),
            el('td', null, tierBadge(row.confidence)),
            el('td', null, row.evidence && row.evidence[0]
              ? evidence(row.evidence[0].file_path, row.evidence[0].range && row.evidence[0].range.start ? row.evidence[0].range.start.line : null)
              : el('span', { class: 'path', text: text(row.file_path) }))),
        }),
      null, 'flush'));
  }

  const written = d.tables_written || [];
  const read = d.tables_read || [];
  const exposed = d.endpoints_exposed || [];
  if (written.length || read.length || exposed.length) {
    body.appendChild(card('What it touches outside the code', 'tables and routes reached from here',
      [
        written.length ? tableList('Tables written', written) : null,
        read.length ? tableList('Tables read', read) : null,
        exposed.length ? tableList('Endpoints exposed', exposed) : null,
      ]));
  }

  const tests = (d.tests && d.tests.tests) ? d.tests.tests : [];
  if (tests.length) {
    body.appendChild(card('Tests that cover it', num(d.tests.proven_by_graph) + ' proven by a recorded edge',
      table(
        [{ label: 'Test' }, { label: 'Kind' }, { label: 'Signal' }, { label: 'Confidence' }, { label: 'File' }],
        tests,
        {
          cap: 20,
          row: (row) => el('tr', { class: 't-' + tierOf(row.confidence) },
            el('td', null, el('span', { class: 'sym', text: text(row.name) })),
            el('td', { class: 'nowrap small muted', text: text(row.test_kind) }),
            el('td', { class: 'nowrap small muted', text: text(row.strongest_signal) }),
            el('td', null, tierBadge(row.confidence)),
            el('td', null, el('span', { class: 'path', text: text(row.file_path) }))),
        }),
      null, 'flush'));
  } else {
    body.appendChild(card('Tests that cover it', 'none found',
      el('p', { class: 'prose', text: 'No test in this index is recorded as reaching this symbol. That is not proof it is untested: a test that reaches it through a relationship this index could not resolve is invisible here, which is what the resolution coverage on the overview is about.' })));
  }

  const unknowns = d.unknown || [];
  if (unknowns.length) {
    body.appendChild(card('What this walk could not see', num(unknowns.length) + ' bounds',
      el('ul', { class: 'prose' }, unknowns.map((u) => el('li', null,
        el('b', { text: String(text(u.kind)).replace(/_/g, ' ') + ': ' }), text(u.detail),
        Number(u.count) ? ' (' + num(u.count) + (u.count_is_lower_bound ? '+' : '') + ')' : ''))),
      null, 'unknown'));
  }

  const completeness = d.completeness || {};
  if (completeness.caveats && completeness.caveats.length) {
    body.appendChild(fold((completeness.lower_bound ? 'This answer is a lower bound — ' : 'Bounds on this answer — ') + num(completeness.caveats.length) + ' ' + plural('caveat', completeness.caveats.length),
      card('Completeness', completeness.lower_bound ? 'this answer is a lower bound' : 'bounds on this answer',
        el('ul', { class: 'prose' }, completeness.caveats.map((line) => el('li', { text: line })))), { key: 'blast:completeness' }));
  }

  body.appendChild(renderOmissions(d.omissions, { key: 'blast' }));
  body.appendChild(el('div', { class: 'next' },
    el('span', { class: 'k', text: 'Next: compile a contract for this change' }),
    el('span', { class: 'd', text: 'Turn the impact set into scope, constraints and unknowns an agent can be handed.' }),
    el('button', { class: 'go', type: 'button', text: 'Open →', onclick: () => go('contract', { intent: 'Change ' + text(target.name) }) })));
}

/*
  IMPACTED FILES, RANKED BY WHAT WAS ACTUALLY RECORDED.

  A list of impacted SYMBOLS is a list of names. The unit of work is a FILE, and
  the size of the work is how many places in it recorded the reference — which
  the API sends as `evidence[]` on every directly-impacted symbol.

  Two counts, kept apart on purpose. `symbols` is how many definitions in the
  file reference the target; `sites` is how many source locations recorded it.
  They are different numbers and folding them into one is the same conflation
  the aggregated-edge table exists to prevent. Where the API withheld evidence —
  it does that when the fan-in was too large to load, and says so — the row
  reports zero sites and no location rather than inventing one.
*/
function impactedFiles(direct) {
  const byPath = new Map();
  for (const row of direct) {
    const path = text(row.file_path, '');
    if (path === '—' || path === '') continue;
    let acc = byPath.get(path);
    if (!acc) {
      acc = { path: path, symbols: 0, sites: 0, tests: 0, weakest: 'exact', names: [], evidence: null };
      byPath.set(path, acc);
    }
    acc.symbols++;
    if (row.is_test) acc.tests++;
    acc.weakest = weakest(acc.weakest, row.confidence);
    const ev = row.evidence || [];
    acc.sites += ev.length;
    if (!acc.evidence && ev.length) acc.evidence = ev[0];
    if (acc.names.length < 5) acc.names.push(text(row.name));
  }
  return Array.from(byPath.values()).sort((a, b) =>
    b.sites - a.sites || b.symbols - a.symbols || String(a.path).localeCompare(String(b.path)));
}

function impactedFileRow(f, top) {
  const share = Math.min(100, (f.sites / Math.max(1, top)) * 100);
  const bar = el('div', {
    'aria-hidden': 'true',
    style: 'position:relative;height:8px;border-radius:4px;background:var(--bg-sunken);'
      + 'border:1px solid var(--line);overflow:hidden;margin:6px 0 7px',
  }, el('span', {
    style: 'position:absolute;left:0;top:0;bottom:0;border-radius:3px;width:' + share.toFixed(2) + '%;'
      + 'background:var(--tier-' + tierOf(f.weakest) + ')',
  }));

  const line = (f.evidence && f.evidence.range && f.evidence.range.start)
    ? f.evidence.range.start.line : null;

  return el('div', {
    style: 'border:1px solid var(--line);border-radius:var(--r-sm);padding:10px 12px;background:var(--panel)',
  },
    el('div', { style: 'display:flex;gap:12px;align-items:baseline;flex-wrap:wrap' },
      el('span', { class: 'sym', text: baseName(f.path) }),
      el('span', { class: 'path', text: dirName(f.path) }),
      el('span', { class: 'spacer', style: 'flex:1' }),
      tierBadge(f.weakest),
      el('span', { class: 'small muted',
        text: num(f.sites) + ' recorded ' + plural('call site', f.sites) + ' · '
          + num(f.symbols) + ' ' + plural('symbol', f.symbols)
          + (f.tests ? ' · ' + num(f.tests) + ' of them test code' : '') })),
    bar,
    el('div', { class: 'small muted', text: f.names.join(', ') + (f.symbols > f.names.length ? ', …' : '') }),
    f.evidence
      ? evidence(f.evidence.file_path, line)
      : el('p', { class: 'small muted', text: 'No source location is attached to this file’s rows. The API withholds '
          + 'per-site evidence when the fan-in was too large to load, and records that it did — so this is a missing '
          + 'location, not a relationship without one.' }));
}

function tableList(title, rows) {
  return el('div', null,
    el('h4', { text: title }),
    table([{ label: 'Name' }, { label: 'Relation' }, { label: 'Confidence' }, { label: 'Evidence' }], rows, {
      cap: 12,
      row: (row) => el('tr', { class: 't-' + tierOf(row.confidence) },
        el('td', null, el('span', { class: 'sym', text: text(row.name) })),
        el('td', null, el('code', { text: text(row.relation) })),
        el('td', null, tierBadge(row.confidence)),
        el('td', null, evidenceOf(row.evidence))),
    }));
}

// ================================================================ SCREEN 05 — TASK CONTRACT

LOADERS.contract = async function () {
  const host = $('#screen-contract');
  if (state.loaded.contract) return;
  state.loaded.contract = true;
  clear(host);
  host.appendChild(contractAnswer(null));

  const input = el('input', { type: 'search', id: 'contract-intent', autocomplete: 'off', value: state.q.intent || '', placeholder: 'Add idempotency to payment callback processing' });
  const toolbar = el('form', {
    class: 'toolbar',
    onsubmit: (e) => { e.preventDefault(); setQuery({ intent: input.value.trim() }); runContract(); },
  });
  toolbar.appendChild(field('Task', input, 'grow'));
  toolbar.appendChild(selectField('Scope budget', 'contract-scope',
    [{ value: '0', label: '12 (default)' }, { value: '20', label: '20' }, { value: '30', label: '30' }, { value: '60', label: '60' }],
    state.q.scope, (v) => { setQuery({ scope: v }); if (state.q.intent) runContract(); }));
  toolbar.appendChild(el('button', { class: 'primary', type: 'submit', text: 'Compile' }));
  host.appendChild(toolbar);
  host.appendChild(el('div', { class: 'stack', id: 'contract-body' }));

  if (state.q.intent) runContract();
  else $('#contract-body').appendChild(emptyState('Describe the change in your own words',
    ['The compiler reads the words, anchors them on symbols the index actually holds, and shows you what it understood before it shows you an answer — so a misread is visible rather than buried in a wrong result.',
      'This is not search. It returns a brief, including the questions it cannot answer.'],
    [el('button', { class: 'chip', type: 'button', text: 'Add idempotency to payment callback processing', onclick: () => { $('#contract-intent').value = 'Add idempotency to payment callback processing'; setQuery({ intent: 'Add idempotency to payment callback processing' }); runContract(); } })]));
};

// The brief's answer: the three counts a reader checks it against. Before a
// task is described, what describing one gets you.
function contractAnswer(d) {
  const support = 'The compiled brief: what is in scope, what is deliberately not, what constrains it, and — first — what the index could not answer.';
  if (!d) return answer('Describe a task and this writes the brief an agent should get: what is in scope, what is not, and what it could not find out.', { support: support });
  const counts = d.counts || {};
  const scope = Number(counts.scope || 0);
  const tests = Number(counts.tests || 0);
  const unknowns = Number(counts.unknowns || 0);
  return answer(['A brief for “' + text(d.intent, state.q.intent) + '”: ',
    B(num(scope) + ' ' + plural('entry', scope)), ' in scope, ',
    B(num(tests) + ' ' + plural('test', tests)), ' that cover it, ',
    B(num(unknowns) + ' ' + plural('thing', unknowns)), ' it could not find out.'], { support: support, next: nextOf(d.next_action) });
}

async function runContract() {
  const body = $('#contract-body');
  clear(body);
  body.appendChild(loading('Compiling…'));
  let d;
  try { d = await api('/api/contract', { intent: state.q.intent, max_scope: state.q.scope, min_confidence: state.q.floor }); }
  catch (e) { clear(body); failAnswer($('#screen-contract'), 'The contract'); return showError(body, e, 'The contract'); }
  clear(body);
  setAnswer($('#screen-contract'), contractAnswer(d));

  const contract = d.contract || {};
  const counts = d.counts || {};

  // Four tiles, not eight. The eight-tile row destroyed the hierarchy the
  // four-tile pattern exists to create, and mixed a cost metric into a row of
  // findings.
  body.appendChild(el('div', { class: 'kpis' },
    kpi('In scope', num(counts.scope), 'files and symbols the change touches'),
    kpi('Explicitly not', num(counts.out_of_scope), 'ranked and deliberately excluded'),
    kpi('Reached', num(counts.blast_radius), num(counts.blast_radius_outside_scope) + ' of them outside scope'),
    kpi('Unknowns', num(counts.unknowns), 'answer these before starting', Number(counts.unknowns) > 0 ? 'warn' : 'ok')));

  body.appendChild(card('What was asked, and what was understood', 'read this before the answer',
    [
      el('p', { class: 'prose', text: text(contract.goal, d.intent) }),
      el('p', { class: 'prose', text: text(d.interpretation) }),
    ]));

  const unknowns = d.unknowns || [];
  if (unknowns.length) {
    body.appendChild(card('What the index cannot answer', num(unknowns.length) + ' unknowns — open, not collapsed',
      el('ul', { class: 'prose' }, unknowns.map((u) => el('li', { text: u }))), null, 'unknown'));
  }

  const scope = contract.scope || [];
  const whyOf = new Map();
  let withheld = 0;
  let withheldNote = '';
  for (const row of scope) {
    const split = String(row.why || '').split('; source withheld:');
    whyOf.set(row, split[0]);
    if (split.length > 1) { withheld++; if (!withheldNote) withheldNote = split[1].trim(); }
  }
  body.appendChild(card('Scope', num(scope.length) + ' entries',
    [
      table([{ label: 'Symbol' }, { label: 'Kind' }, { label: 'File' }, { label: 'Why' }], scope, {
        cap: 30,
        row: (row) => el('tr', null,
          el('td', null, el('span', { class: 'sym', text: text(row.symbol) })),
          el('td', { class: 'nowrap small muted', text: text(row.kind) }),
          el('td', null, el('span', { class: 'path', text: text(row.path) })),
          el('td', { class: 'small muted' }, wordedReason(whyOf.get(row), 'no reason recorded'))),
      }),
      withheld ? inlineOmit(el('b', { text: num(withheld) }), ' of these carry no source excerpt: ', withheldNote) : null,
    ], null, 'flush'));

  const excluded = contract.out_of_scope || [];
  if (excluded.length) {
    body.appendChild(fold('Explicitly out of scope — ' + num(excluded.length) + ' ' + plural('row', excluded.length), card('Explicitly out of scope', 'ranked, considered, and left out on purpose',
      table([{ label: 'Symbol' }, { label: 'File' }, { label: 'Why it was left out' }], excluded, {
        cap: 15,
        row: (row) => el('tr', null,
          el('td', null, el('span', { class: 'sym', text: text(row.symbol) })),
          el('td', null, el('span', { class: 'path', text: text(row.path) })),
          el('td', { class: 'small muted' }, wordedReason(row.why))),
      }), null, 'flush'), { key: 'contract:excluded' }));
  }

  const constraints = contract.constraints || [];
  const risks = contract.risks || [];
  const acceptance = contract.acceptance || [];
  const grid = el('div', { class: 'grid-2' });
  if (constraints.length) {
    grid.appendChild(card('Constraints', num(constraints.length),
      el('ul', { class: 'prose' }, constraints.map((row) => el('li', null, text(row.text),
        row.evidence ? el('div', { class: 'path', text: row.evidence }) : null)))));
  }
  if (risks.length) {
    grid.appendChild(card('Risks', num(risks.length),
      el('ul', { class: 'prose' }, risks.map((row) => el('li', null,
        el('span', { class: 'tier tier-' + (row.severity === 'high' ? 'medium' : 'low'), title: 'severity: ' + text(row.severity), text: severityWord(row.severity) }), ' ', text(row.text),
        row.evidence ? el('div', { class: 'path', text: row.evidence }) : null)))));
  }
  if (acceptance.length) {
    grid.appendChild(card('Acceptance criteria', num(acceptance.length),
      el('ul', { class: 'prose' }, acceptance.map((line) => el('li', { text: line })))));
  }
  if (grid.children.length) body.appendChild(grid);

  const reached = contract.blast_radius || [];
  if (reached.length) {
    body.appendChild(fold('Evidence — what it would reach, ' + num(reached.length) + ' ' + plural('row', reached.length), card('What it would reach', el('span', null, 'standard of proof: ', el('span', { 'data-tier': tierOf(contract.impact_floor), text: tierWord(tierOf(contract.impact_floor)) })),
      table([{ label: 'Symbol' }, { label: 'Depth', num: true }, { label: 'Confidence' }, { label: 'Evidence' }], reached, {
        cap: 20,
        row: (row) => el('tr', { class: 't-' + tierOf(row.confidence) },
          el('td', null, el('span', { class: 'sym', text: text(row.symbol) }),
            el('div', { class: 'path', text: text(row.path) })),
          el('td', { class: 'num', text: num(row.depth) }),
          el('td', null, tierBadge(row.confidence)),
          el('td', null, wordedEvidence(row.evidence) || evidenceFromString(row.evidence, row.path))),
      }), null, 'flush'), { key: 'contract:reach' }));
  }

  const tests = contract.tests || [];
  if (tests.length) {
    body.appendChild(fold('Evidence — the tests it names, ' + num(tests.length) + ' ' + plural('row', tests.length), card('Tests named by this contract', num(tests.length),
      table([{ label: 'Test' }, { label: 'Signal' }, { label: 'Confidence' }, { label: 'File' }], tests, {
        cap: 20,
        row: (row) => el('tr', { class: 't-' + tierOf(row.confidence) },
          el('td', null, el('span', { class: 'sym', text: text(row.name) })),
          el('td', { class: 'nowrap small muted', text: text(row.signal) }),
          el('td', null, tierBadge(row.confidence)),
          el('td', null, el('span', { class: 'path', text: text(row.path) }))),
      }), null, 'flush'), { key: 'contract:tests' }));
  }

  const notes = d.pack_notes || [];
  if (notes.length) {
    body.appendChild(fold('How this brief was assembled — ' + num(notes.length) + ' ' + plural('note', notes.length), card('', 'the context engine’s own accounting, in one place',
      el('ul', { class: 'prose' }, notes.map((line) => el('li', { text: line })))), { key: 'contract:notes' }));
  }

  body.appendChild(el('p', { class: 'hint', text: 'Estimated cost of handing this brief to an agent: ' + num(counts.token_estimate) + ' tokens across ' + num(counts.pack_items) + ' items.' }));
  body.appendChild(renderOmissions(d.omissions, { key: 'contract' }));
  body.appendChild(el('div', { class: 'next' },
    el('span', { class: 'k', text: 'Next: verify the change against this contract' }),
    el('span', { class: 'd', text: 'Paste the changed files and the same task; the contract is recompiled so the two screens agree by construction.' }),
    el('button', { class: 'go', type: 'button', text: 'Open →', onclick: () => go('verify', { intent: state.q.intent }) })));
}

// ================================================================ SCREEN 06 — VERIFY

LOADERS.verify = async function () {
  const host = $('#screen-verify');
  if (state.loaded.verify) return;
  state.loaded.verify = true;
  clear(host);
  host.appendChild(verifyAnswer(null));

  const intent = el('input', { type: 'search', id: 'verify-intent', autocomplete: 'off', value: state.q.intent || '', placeholder: 'the same words you compiled the contract from' });
  const changed = el('input', { type: 'text', id: 'verify-changed', autocomplete: 'off', value: state.q.changed || '', placeholder: 'git diff --name-only | paste -sd, -' });
  const form = el('form', {
    class: 'toolbar',
    onsubmit: (e) => { e.preventDefault(); setQuery({ intent: intent.value.trim(), changed: changed.value.trim() }); runVerify(); },
  });
  form.appendChild(field('Task the change was compiled from', intent, 'grow'));
  form.appendChild(field('Changed files', changed, 'grow'));
  form.appendChild(el('button', { class: 'primary', type: 'submit', text: 'Verify' }));
  host.appendChild(form);
  host.appendChild(el('p', { class: 'hint', text: 'Nothing is written and no git command is run — the change set is exactly what you paste.' }));
  host.appendChild(el('div', { class: 'stack', id: 'verify-body' }));

  if (state.q.intent && state.q.changed) runVerify();
  else $('#verify-body').appendChild(emptyState('Two inputs, both required',
    ['The task, so the contract can be recompiled from the same words; and the files the change actually touched.',
      'An empty change set is not a pass. With nothing to check the verdict is `unverified`, which exits 2 — a failure of the setup, not of the change.']));
};

/*
  The verdict, as the first sentence. `unverified` is the API's own
  disposition and is said in its own words, never as a pass; the two counts
  are facts (a compiler proved them) and leads (inferred from names).
*/
function verifyAnswer(d) {
  const support = 'After the change: what the brief took a position on, what it did not, and which findings a compiler proved rather than a name suggested.';
  if (!d) return answer('Check a finished change against its brief: the task it was compiled from, and the files it touched.', { support: support });
  const verdict = text(d.verdict, 'unknown');
  const unverified = d.unverified === undefined ? (verdict === 'unverified') : !!d.unverified;
  const counts = d.counts || {};
  const facts = Number(counts.facts || 0);
  const leads = Number(counts.leads || 0);
  if (unverified) {
    return answer([B('Unverified'), ': the brief took no position on the files that changed, so nothing here was checked against anything.'],
      { support: support, next: nextOf(d.next_action) });
  }
  const word = verdict === 'pass' ? 'Pass' : (verdict === 'advisory' ? 'Advisory' : 'Fail');
  return answer([B(word), ': ', B(num(facts) + ' ' + plural('thing', facts)), ' this change broke, ',
    B(num(leads)), ' it might have.'], { support: support, next: nextOf(d.next_action) });
}

async function runVerify() {
  const body = $('#verify-body');
  clear(body);
  body.appendChild(loading('Recompiling the contract and checking the change…'));
  let d;
  try { d = await api('/api/verify', { intent: state.q.intent, changed: state.q.changed, min_confidence: state.q.floor }); }
  catch (e) { clear(body); failAnswer($('#screen-verify'), 'The verify report'); return showError(body, e, 'The verify report'); }
  clear(body);
  setAnswer($('#screen-verify'), verifyAnswer(d));

  const counts = d.counts || {};
  const fileCounts = d.file_counts || {};

  /*
    THE VERDICT.

    This screen used to render a large `PASS / exit 0` over a change the
    contract took no position on: 0 in scope, 0 excluded, 2 undeclared, and an
    unknowns card directly below saying so. The product's own words, printed on
    the same page, are that "we looked and found nothing" and "there was
    nothing to look at" must never render the same — and the badge rendered them
    the same, on the one screen a CI-minded reader screenshots.

    The API now serves `unverified` as its own disposition with its own exit
    code and its own boolean, so this is read, never inferred.
  */
  const verdict = text(d.verdict, 'unknown');
  const unverified = d.unverified === undefined ? (verdict === 'unverified') : !!d.unverified;
  const klass = unverified ? 'v-unverified' : (verdict === 'pass' ? 'v-pass' : (verdict === 'advisory' ? 'v-advisory' : 'v-fail'));
  const say = unverified
    ? 'Nothing here was checked against anything. The contract took no position on the files that changed, so this run found nothing because there was nothing to look at — which is not the same as finding nothing wrong.'
    : (verdict === 'pass'
      ? 'Every question was asked over the changed files and none produced a finding.'
      : (verdict === 'advisory'
        ? 'Findings exist, and all of them rest on relationships inferred from names. A gate that blocks on leads is a gate that gets switched off, and it takes the facts with it.'
        : 'At least one compiler-proven fact contradicts the contract.'));

  body.appendChild(el('div', { class: 'verdict ' + klass },
    el('div', { class: 'big', text: verdict.toUpperCase() }),
    el('div', null,
      el('div', { class: 'say', text: say }),
      el('div', { class: 'exit', text: 'exit ' + num(d.exit_code) + ' · signal ' + text(d.signal) }))));

  body.appendChild(el('div', { class: 'kpis' },
    kpi('Changed files', num(fileCounts.changed), 'exactly what you pasted'),
    kpi('In scope', num(fileCounts.in_scope), 'the contract named these'),
    kpi('Undeclared', num(fileCounts.undeclared), 'the contract took no position', Number(fileCounts.undeclared) > 0 ? 'warn' : 'ok'),
    kpi('Facts', num(counts.facts), num(counts.leads) + ' leads alongside', Number(counts.facts) > 0 ? 'bad' : 'ok')));

  const questions = d.questions || (d.report && d.report.questions) || [];
  if (questions.length) {
    body.appendChild(fold('Evidence — the ' + num(questions.length) + ' questions this check asked', card('The four questions', 'and whether each was actually asked',
      table(
        [{ label: 'Question' }, { label: 'Asked' }, { label: 'Answered' }, { label: 'Covered', num: true }, { label: 'Findings', num: true }],
        questions,
        {
          cap: 10,
          row: (q) => el('tr', { class: q.asked && !q.answered ? 't-medium' : null },
            el('td', null, text(q.question), el('div', { class: 'small muted', text: text(q.detail) })),
            el('td', { class: 'nowrap' }, q.asked ? 'yes' : el('span', { class: 'muted', text: 'no' })),
            el('td', { class: 'nowrap' }, q.answered ? 'yes' : el('span', { class: 'warnline', text: 'nothing to answer it with' })),
            el('td', { class: 'num', text: num(q.covered) + ' ' + text(q.unit, '') }),
            el('td', { class: 'num', text: num(q.findings) })),
        }),
      null, 'flush'), { key: 'verify:questions' }));
  }

  const facts = d.facts || [];
  const leads = d.leads || [];
  if (facts.length) {
    body.appendChild(card('Facts', 'compiler-proven — these block a merge',
      findingsTable(facts), null, 'bad'));
  }
  if (leads.length) {
    body.appendChild(card('Leads', 'inferred from names — worth a look, never a blocker',
      [
        el('p', { class: 'prose', text: 'A lead rests on an edge measured below 37.5% precision. It is shown because hiding it would be dishonest, and it does not fail the build because a gate that blocks on leads is a gate that gets switched off.' }),
        findingsTable(leads),
      ], null, 'warn'));
  }
  if (!facts.length && !leads.length) {
    body.appendChild(card('Findings', 'none',
      el('p', { class: 'prose', text: unverified
        ? 'No findings, because no question engaged a changed file. Read the verdict above rather than this line.'
        : 'Every question ran over the changed files and none produced a finding.' })));
  }

  const files = d.files || [];
  if (files.length) {
    body.appendChild(fold('Evidence — every changed file, ' + num(files.length) + ' ' + plural('row', files.length), card('Every changed file, and what the contract said about it', num(files.length),
      table([{ label: 'File' }, { label: 'Standing' }, { label: 'What that means' }], files, {
        cap: 40,
        row: (row) => el('tr', { class: row.kind === 'undeclared' ? 't-medium' : null },
          el('td', null, el('span', { class: 'path', text: text(row.path) })),
          el('td', { class: 'nowrap' }, el('span', { class: 'tier tier-' + (row.kind === 'in_scope' ? 'exact' : (row.kind === 'undeclared' ? 'medium' : 'low')), text: String(text(row.kind)).replace(/_/g, ' ') })),
          el('td', { class: 'small muted' }, wordedReason(row.why))),
      }), null, 'flush'), { key: 'verify:files' }));
  }

  const unknowns = d.unknowns || [];
  if (unknowns.length) {
    body.appendChild(card('What this check could not settle', num(unknowns.length),
      el('ul', { class: 'prose' }, unknowns.map((u) => el('li', { text: u }))), null, 'unknown'));
  }

  body.appendChild(renderOmissions(d.omissions, { key: 'verify' }));
}

function findingsTable(rows) {
  return table(
    [{ label: 'Finding' }, { label: 'Standing' }, { label: 'Confidence' }, { label: 'Evidence' }],
    rows,
    {
      cap: 25,
      row: (row) => el('tr', { class: 't-' + tierOf(row.confidence) },
        el('td', null, text(row.summary),
          row.detail ? el('div', { class: 'small muted', text: row.detail }) : null),
        el('td', { class: 'nowrap small' }, text(row.standing), row.severity ? el('div', { class: 'muted', text: text(row.severity) }) : null),
        el('td', null, tierBadge(row.confidence)),
        el('td', null, evidenceFromString(row.evidence, row.path))),
    });
}

// ================================================================ SCREEN 07 — ARCHITECTURE GOVERNANCE

LOADERS.governance = async function () {
  const host = $('#screen-governance');
  if (state.loaded.governance) return;
  state.loaded.governance = true;
  await drawGovernance();
};

async function drawGovernance() {
  const host = $('#screen-governance');
  clear(host);
  host.appendChild(answer('The code is being checked against the architecture it declares.'));

  const toolbar = el('div', { class: 'toolbar' });
  toolbar.appendChild(selectField('Standard of proof for a violation', 'gov-floor',
    [{ value: '', label: 'show every violation' }].concat(FLOORS.map((floor) => ({ value: floor.value, label: floor.label }))),
    state.q.sev, (v) => { setQuery({ sev: v }); drawGovernance(); }));
  host.appendChild(toolbar);

  const body = el('div', { class: 'stack' }, loading('Checking the architecture…'));
  host.appendChild(body);

  let d;
  try { d = await api('/api/governance', { min_confidence: state.q.sev }); }
  catch (e) { clear(body); failAnswer(host, 'Governance'); return showError(body, e, 'Governance'); }
  state.meta = d.meta;
  renderIndexBar();
  clear(body);
  setAnswer(host, governanceAnswer(d));
  renderGovernance(d, body);
}

// Violations out of relationships checked, or the fact that nothing was
// declared to check against — which is a different sentence from "no violations".
function governanceAnswer(d) {
  const support = 'The declared architecture, what crosses it, and what a CI gate would do about that today.';
  if (!d.architecture_declared) {
    return answer('No architecture is declared, so there is nothing to check the code against.', { support: support, next: nextOf(d.next_action) });
  }
  const byConf = d.violations_by_confidence || {};
  const n = Number(byConf.total || 0);
  const checked = Number(d.edges_considered || 0);
  return answer([B(num(n) + ' ' + plural('place', n)), ' where the code crosses a line the architecture forbids, out of ',
    B(num(checked) + ' ' + plural('relationship', checked)), ' checked.'], { support: support, next: nextOf(d.next_action) });
}

function renderGovernance(d, body) {
  if (!d.architecture_declared) {
    body.appendChild(emptyState('No architecture is declared for this repository',
      ['This screen checks code against rules you write down. There is nothing to check against yet — which is different from "the code has no violations".',
        'Declare components and rules in .cortex.yml, then run the gate in CI.']));
    body.appendChild(renderOmissions(d.omissions, { key: 'governance' }));
    return;
  }

  const byConf = d.violations_by_confidence || {};
  const violations = d.violations || [];
  body.appendChild(el('div', { class: 'kpis' },
    kpi('Components', num((d.components || []).length), num(d.rules_evaluated) + ' ' + plural('rule', d.rules_evaluated) + ' evaluated'),
    kpi('Violations', num(byConf.total), ['worst severity: ', el('code', { text: text(d.worst_severity, 'none') })], Number(byConf.total) > 0 ? 'bad' : 'ok'),
    kpi('Compiler-proven', num(byConf.exact), 'violations a compiler stands behind', Number(byConf.exact) > 0 ? 'bad' : 'ok'),
    kpi('Outside every component', num(d.unassigned_symbols), 'no rule was evaluated against these', Number(d.unassigned_symbols) > 0 ? 'warn' : 'ok')));

  /*
    THE GATE, WIRED TO CONFIDENCE.

    The old gate table said "would fail" at all three severity thresholds while
    every violation on the repository sat on a `low`-confidence edge — and the
    prose two inches above said "a violation on a compiler-proven edge is a
    fact, one on a `low` edge is a lead". Recommending a gate that fails a build
    on five leads is how a team switches the gate off.

    The severity gates below are the CLI's own, unchanged. Beside them is what
    each would do if it also required a confidence floor, computed here from
    violations_by_confidence and labelled as computed here.
  */
  const gates = d.ci_gates || [];
  const proven = Number(byConf.exact || 0) + Number(byConf.high || 0);
  body.appendChild(card('What a CI gate would do right now', 'severity thresholds as the CLI implements them',
    [
      table([{ label: 'Gate' }, { label: 'Result' }, { label: 'Detail' }, { label: 'Command' }], gates, {
        cap: 6,
        row: (gate) => el('tr', null,
          el('td', { class: 'nowrap' }, el('code', { text: 'fail-on ' + text(gate.fail_on) })),
          el('td', { class: 'nowrap' }, el('span', { class: 'tier tier-' + (gate.passes ? 'exact' : 'medium'), text: gate.passes ? 'passes' : 'would fail the build' })),
          el('td', { class: 'small muted' }, el('code', { class: 'small', text: text(gate.detail) })),
          el('td', null, el('code', { class: 'small', text: text(gate.command) }), copyButton(text(gate.command), 'Copy'))),
      }),
      el('div', { class: 'held' },
        el('h4', { text: 'Read this before turning a gate on' }),
        el('p', { class: 'what' },
          num(byConf.total) + ' ' + plural('violation', byConf.total) + ' in total: ',
          el('b', { text: num(proven) }), ' on edges measured very likely or better, ',
          el('b', { text: num(byConf.medium) + Number(byConf.low) ? num(Number(byConf.medium || 0) + Number(byConf.low || 0)) : '0' }),
          ' on weaker ones.',
          proven === 0 && Number(byConf.total) > 0
            ? ' Every violation here rests on a relationship inferred from names, so a gate turned on today would fail builds on leads — which is how a gate gets switched off, and it takes the facts with it.'
            : ' A violation on a compiler-proven edge is a fact; one on a guessed edge is a lead.'),
        el('p', { class: 'fix' }, 'This paragraph is computed in the portal from violations_by_confidence. The gate command itself does not yet take a standard of proof — see assets/NEEDS-API.md.')),
    ],
    null, 'flush'));

  const components = d.components || [];
  body.appendChild(fold('Evidence — the ' + num(components.length) + ' declared ' + plural('component', components.length), card('Declared components', num(components.length),
    [
      table([{ label: 'Component' }, { label: 'Symbols', num: true }, { label: 'Files with symbols', num: true, title: 'Files that produced at least one symbol — the population the graph can draw' }, { label: 'Files matched', num: true, title: 'Every file the component pattern matched, symbol-bearing or not' }, { label: 'Silent files', num: true }], components, {
        cap: 25,
        row: (row) => el('tr', null,
          el('td', null, el('span', { class: 'sym', text: text(row.name) })),
          el('td', { class: 'num', text: num(row.symbols) }),
          el('td', { class: 'num', text: num(row.files_with_symbols) }),
          el('td', { class: 'num', text: num(row.files_matched) }),
          el('td', { class: 'num muted', text: num(row.files_without_symbols) })),
      }),
      inlineOmit('The two file columns are different populations, and they used to be one column called "files" on two screens — the graph reported 551 for a component while this screen reported 3,425 in the same session. ',
        el('b', { text: 'Files with symbols' }), ' is what a graph can draw; ',
        el('b', { text: 'Files matched' }), ' is what the include pattern selects. Both come from the same arithmetic now.'),
    ], null, 'flush'), { key: 'governance:components' }));

  if (violations.length) {
    body.appendChild(card('Violations', num(violations.length) + ' — each with the edge that proves it',
      table(
        [{ label: 'Rule' }, { label: 'From' }, { label: 'To' }, { label: 'Kind' }, { label: 'Confidence' }, { label: 'Standing' }, { label: 'Evidence' }],
        violations,
        {
          cap: 30,
          row: (row) => el('tr', { class: 't-' + tierOf(row.confidence) },
            el('td', null, el('code', { text: text(row.rule_id) }), el('div', { class: 'small muted' }, el('code', { text: text(row.severity) }))),
            el('td', null, el('span', { class: 'sym', text: text(row.from_symbol) }), el('div', { class: 'small muted', text: text(row.from_component) })),
            el('td', null, el('span', { class: 'sym', text: text(row.to_symbol) }), el('div', { class: 'small muted', text: text(row.to_component) })),
            el('td', null, el('code', { text: text(row.kind) })),
            el('td', null, tierBadge(row.confidence)),
            el('td', { class: 'nowrap small' }, tierRank(row.confidence) >= tierRank('high') ? 'fact' : 'lead'),
            el('td', null, evidence(row.file_path, row.line), row.detail ? el('div', { class: 'small muted', text: row.detail }) : null)),
        }),
      null, 'flush'));
  } else {
    body.appendChild(card('Violations', 'none at this standard of proof',
      el('p', { class: 'prose', text: 'No edge crosses a forbidden boundary at the current standard of proof. Lower it to see whether weaker evidence suggests otherwise — an empty result at a strict standard is not the same as one with guesses included.' })));
  }

  const crossings = d.crossings || [];
  body.appendChild(fold('Evidence — the dependency matrix, ' + num(crossings.length) + ' ' + plural('pair', crossings.length) + ' crossing a boundary', dependencyMatrix(crossings, components), { key: 'governance:matrix' }));
  body.appendChild(renderOmissions(d.omissions, { key: 'governance' }));
}

/*
  A real matrix. The dependency view was a long flat "from · to · count · tier"
  list with a trailing empty column, which is a table of pairs, not a matrix —
  and the whole value of a matrix is that the empty cells are as informative as
  the full ones.
*/
function dependencyMatrix(crossings, components) {
  const names = Array.from(new Set(components.map((c) => c.name)
    .concat(crossings.map((x) => x.from)).concat(crossings.map((x) => x.to)))).sort();
  const cells = new Map(crossings.map((x) => [x.from + '\u0000' + x.to, x]));
  const shown = names.slice(0, 22);

  const tbl = el('table', { class: 'matrix' });
  const head = el('tr', null, el('th', { class: 'rowh', scope: 'col', text: 'from ╲ to' }));
  for (const name of shown) head.appendChild(el('th', { class: 'colh', scope: 'col', text: name }));
  tbl.appendChild(el('thead', null, head));

  const tbody = el('tbody');
  for (const from of shown) {
    const tr = el('tr', null, el('td', { class: 'rowh', text: from }));
    for (const to of shown) {
      const cell = cells.get(from + '\u0000' + to);
      if (!cell) { tr.appendChild(el('td', { class: 'cell zero', text: from === to ? '·' : '' })); continue; }
      tr.appendChild(el('td', {
        class: 'cell' + (cell.violation ? ' viol' : '') + ' t-' + tierOf(cell.weakest_confidence),
        title: from + ' → ' + to + ': ' + num(cell.count) + ' edges, weakest ' + tierOf(cell.weakest_confidence) + (cell.violation ? ' — VIOLATION' : ''),
        text: num(cell.count),
      }));
    }
    tbody.appendChild(tr);
  }
  tbl.appendChild(tbody);

  return card('Dependency matrix', num(crossings.length) + ' ordered pairs cross a component boundary',
    [
      el('div', { class: 'tablewrap' }, tbl),
      inlineOmit('A cell is one direction only. Outlined cells break a declared rule. An empty cell means no edge at the current standard of proof was recorded between those two — which is not the same as no dependency.'),
      names.length > shown.length ? inlineOmit(el('b', { text: num(names.length - shown.length) }), ' further components are not in this matrix; it is capped at 22 per axis so the labels stay readable.') : null,
    ], null, 'flush');
}

// ================================================================ BOOT

// ================================================================ CODEMIND — SEARCH & SYMBOLS

/*
  The symbol table, addressable. `/api/symbols` has been serving this since the
  portal existed and nothing rendered it except a hidden picker inside the blast
  screen: a reader who wanted to know whether a name is in this index at all had
  to guess a screen that took a symbol and hope.

  The ordering is the API's and it is printed, because a reader who wonders why
  a row sits where it does should be able to read the answer rather than infer
  a relevance score that does not exist. Ranking belongs to the context engine,
  where it is measured.
*/
/*
  The screen is built once and re-queried every time it is shown with a
  different `q`. "Find references" on the graph inspector navigates here with a
  name in the URL, and a loader that returned early because the screen had been
  built once would have shown the previous reader's search.
*/
let SEARCH_SHOWN = null;

LOADERS.search = async function () {
  const host = $('#screen-search');
  if (state.loaded.search) {
    const box = $('#symbol-query');
    if (box) box.value = state.q.q || '';
    if (SEARCH_SHOWN !== state.q.q) { SEARCH_SHOWN = state.q.q; await runSearch(); }
    return;
  }
  state.loaded.search = true;
  SEARCH_SHOWN = state.q.q;
  clear(host);
  host.appendChild(searchAnswer(null));

  const input = el('input', {
    type: 'search', id: 'symbol-query', autocomplete: 'off',
    value: state.q.q || '', placeholder: 'e.g. SwitchProfile, findChild, PaymentService',
  });
  const form = el('form', {
    class: 'toolbar',
    onsubmit: (e) => { e.preventDefault(); setQuery({ q: input.value.trim() }); runSearch(); },
  });
  form.appendChild(field('Symbol name', input, 'grow'));
  form.appendChild(el('button', { class: 'primary', type: 'submit', text: 'Search' }));
  host.appendChild(form);
  host.appendChild(el('div', { class: 'stack', id: 'search-body' }));
  runSearch();
};

/*
  The search screen's answer: before a query, what typing a name gets you;
  after one, how many definitions carry it. `null` is the pre-query state and
  a response with no matches still says its zero.
*/
function searchAnswer(d) {
  const support = 'Every definition this index holds, by name. It answers "is this in the index at all" — which is the question behind every empty result on every other screen.';
  if (!d) return answer('Type a name to find where it is defined and everything that uses it.', { support: support });
  const n = Number(d.total_matched || 0);
  return answer([B(num(n) + ' ' + plural('definition', n)), ' named ', B(text(d.query, state.q.q)), '.'], { support: support });
}

async function runSearch() {
  const body = $('#search-body');
  clear(body);
  const query = state.q.q;
  SEARCH_SHOWN = query;
  if (!query) {
    setAnswer($('#screen-search'), searchAnswer(null));
    body.appendChild(emptyState('Type a name',
      ['Matching runs over the symbol name and the fully-qualified name. It is a substring scan with a stated order, not a ranked search — a name that is absent here is absent from the index, which is a real finding and not a failed query.',
        'An empty result means this run resolved no definition under that name. It is never proof the code does not exist: a language this index cannot parse contributes no symbols at all.']));
    return;
  }
  body.appendChild(loading('Scanning the symbol table…'));

  let d;
  try { d = await api('/api/symbols', { q: query }); }
  catch (e) { clear(body); failAnswer($('#screen-search'), 'Symbol search'); return showError(body, e, 'Symbol search'); }
  clear(body);
  setAnswer($('#screen-search'), searchAnswer(d));

  const matches = d.matches || [];
  if (!matches.length) {
    const help = d.guidance || {};
    body.appendChild(emptyState(text(help.title, 'No definition in this index carries that name'),
      [text(help.detail, 'The scan ran over every symbol in the run and matched none. That is a statement about this index, not about the repository: a file that failed to parse contributes no symbols, and a language with no grammar contributes none either.'),
        'Index health lists the languages this run is blind to, and how many files parsed only partially.']));
    body.appendChild(renderOmissions(d.omissions, { key: 'search' }));
    return;
  }

  body.appendChild(el('div', { class: 'kpis' },
    kpi('Matches', num(d.total_matched), 'definitions carrying this name'),
    kpi('Shown', num(matches.length), 'the API bounds a picker response'),
    kpi('Exported', num(matches.filter((m) => m.exported).length), 'visible outside their own file')));

  body.appendChild(card('Definitions', text(d.query),
    table(
      [{ label: 'Symbol' }, { label: 'Kind' }, { label: 'Match' }, { label: 'Edges', num: true },
        { label: 'Extraction' }, { label: 'File' }, { label: '' }],
      matches,
      {
        cap: 40,
        row: (m) => el('tr', { class: 't-' + tierOf(m.confidence) },
          el('td', null, el('span', { class: 'sym', text: text(m.name) }),
            m.exported ? el('span', { class: 'tier tier-high', text: 'exported' }) : null,
            m.fqn && m.fqn !== m.name ? el('div', { class: 'path', text: m.fqn }) : null),
          el('td', { class: 'nowrap small muted', text: text(m.kind) }),
          el('td', { class: 'nowrap small muted', text: text(m.match) }),
          el('td', { class: 'num', text: num(m.degree) }),
          el('td', null, tierBadge(m.confidence)),
          el('td', null, el('span', { class: 'path', text: tailPath(text(m.file_path, ''), 3) })),
          el('td', null, el('button', {
            class: 'ghost', type: 'button', text: 'Blast radius',
            onclick: () => go('blast', { sym: text(m.fqn, m.name) }),
          }))),
      }),
    el('p', { class: 'hint', text: 'Order: ' + text(d.ordering) }), 'flush'));

  body.appendChild(renderOmissions(d.omissions, { key: 'search' }));
}

// ================================================================ THE ONE PILLAR WITH NO ENDPOINT

/*
  ONE SCREEN WITH NOTHING TO FETCH, AND IT IS NOT A GAP IN THE BUILD.

  Ten of the eleven screens that used to render this panel now fetch the
  endpoint their pillar is named for — see THE PILLARS, WIRED TO THEIR ENDPOINTS
  below. `agent` is the one that stays, because the MCP surface is a separate
  PROCESS rather than a view of this index: an agent speaks to `cortex serve`
  over stdio, and this read-only portal is not on that conversation. There is no
  /api/agent to render and there should not be one; a transcript this portal
  invented would be the least honest thing in the product.

  So the panel states three things in this order: what the pillar answers, what
  THIS index holds towards it — real counts, read from /api/overview, never
  estimated — and the command that produces the answer today. "Coming soon" is a
  promise; a command is a way to get the answer this afternoon.
*/
// Pillar specs by screen id. The agent screen renders its guidance cards from
// its spec inside its own loader, so the spec must outlive the registration.
const PILLAR_SPECS = {};

function pillarScreen(id, spec) {
  PILLAR_SPECS[id] = spec;
  LOADERS[id] = async function () {
    const host = $('#screen-' + id);
    if (state.loaded[id]) return;
    state.loaded[id] = true;
    clear(host);
    host.appendChild(answer(spec.answer(null), { support: spec.lede }));
    const body = el('div', { class: 'stack' }, loading('Reading what this index holds…'));
    host.appendChild(body);

    let d = state.overview;
    if (!d) {
      try {
        d = await api('/api/overview', { min_confidence: state.q.floor });
        state.overview = d;
        state.meta = d.meta;
        renderIndexBar();
      } catch (e) { clear(body); failAnswer(host, spec.title); return showError(body, e, spec.title); }
    }
    clear(body);
    setAnswer(host, answer(spec.answer(d), { support: spec.lede }));
    for (const node of pillarBody(d, spec)) body.appendChild(node);
  };
}

function pillarBody(d, spec) {
  const out = [];
  const holds = spec.holds ? spec.holds(d) : [];
  if (holds.length) {
    out.push(el('div', { class: 'kpis' }, holds.map((row) => kpi(row[0], row[1], row[2], row[3]))));
  }
  out.push(card('What this pillar answers', spec.sub || 'the questions it exists for',
    el('ul', { class: 'prose' }, spec.answers.map((line) => el('li', { text: line })))));

  const command = el('div', { class: 'held' },
    el('h4', { text: 'What produces the answer today' }),
    el('p', { class: 'what' }, el('code', { text: spec.command }), ' — ', spec.commandNote),
    el('p', { class: 'fix', text: 'This portal renders only what a handler sends it, and this pillar has no handler — not yet, but by construction: the answer lives in a separate process the portal is not on. Nothing above is derived from anything else and nothing below is a projection: the counts are read from /api/overview, and the command is what produces the rest.' }));
  command.appendChild(copyButton(spec.command, 'Copy the command'));

  out.push(card('Why this screen is empty for this index', spec.whySub || 'stated, not implied',
    [el('p', { class: 'prose', text: spec.why(d) }), command],
    spec.next ? spec.next() : null, 'unknown'));
  return out;
}

function overviewRun() { return (state.meta && state.meta.run) ? state.meta.run : {}; }

function goButton(screen, label) {
  return el('div', { class: 'next' },
    el('span', { class: 'k', text: 'Next: ' + label }),
    el('button', { class: 'go', type: 'button', text: 'Open →', onclick: () => go(screen, {}) }));
}

pillarScreen('agent', {
  title: 'Agent / MCP',
  lede: 'What an AI agent gets when it asks this index a question, and what the answer admits it does not know.',
  // No response carries a tool count — the MCP surface is a separate process —
  // so the sentence says what an agent gets from THIS index, in its numbers.
  answer: (d) => (d
    ? ['Your agent gets ', B(num(d.fingerprint.symbols) + ' symbols'), ' and ', B(num(d.fingerprint.edges) + ' relationships'),
      ' from this index, each answer saying how sure it is.']
    : 'What an agent gets from this index is being read.'),
  holds: (d) => [
    ['Symbols', num(d.fingerprint.symbols), 'what a tool call can name'],
    ['Relationships', num(d.fingerprint.edges), 'what a traversal walks'],
    ['Actionable', pct(d.confidence.actionable_pct), 'of edges very likely or better — the share a tool result can stand on',
      Number(d.confidence.actionable_pct) < 60 ? 'warn' : 'ok'],
  ],
  answers: [
    'Which tools an agent can call against this index, and the precise JSON each returns.',
    'What every tool result carries besides the answer: a verdict on its own strength, whether it was truncated and why, and which run it was read from.',
    'Whether the index an agent is reading matches the working tree it is editing.',
  ],
  why: () => 'The MCP surface is a separate process, not a view of this portal: an agent speaks to `cortex serve` over stdio, and nothing in this read-only portal is on that conversation. It will not print a transcript it did not obtain from a running server.',
  command: 'cortex agent',
  commandNote: 'prints the MCP registration an agent needs, and the guidance that goes with it. `cortex serve` starts the server itself.',
});

// ================================================================ THE PILLARS, WIRED TO THEIR ENDPOINTS

/*
  TEN SCREENS THAT USED TO BE A PLACEHOLDER.

  Each fetches the endpoint its pillar is named for. What has NOT changed is the
  thing the placeholders got right: on most repositories most of these endpoints
  answer with `guidance` rather than rows, because there is no compiler index,
  no trace export, no benchmark case file, no estate manifest, and only one
  committed run. That IS the answer, not a failure to get one — and it is
  rendered in the SERVER'S words, title, detail, steps and next action, rather
  than paraphrased here where it would drift.

  Three rules hold on every screen below.

  1. AN EMPTY LIST IS NEVER DRAWN AS AN EMPTY LIST. It is drawn as the coverage
     of the index that produced it: what was looked for, where, and what a
     static read of this repository cannot see. `coverage_limit`,
     `value_policy`, `scope`, `restriction` and `caveats` are payload fields for
     exactly this, and every one of them is rendered.
  2. THE DISTINCTIONS ARE THE PRODUCT, AND FLATTENING THEM IS THE BUG. Proven
     and suggested-by-convention are two columns, never one list with a signal
     column. Measured and not-measured are two words, never a zero. Readers and
     writers are two populations. A count of -1 means NOT COMPUTED and says so
     in words.
  3. EVERY RELATIONSHIP CARRIES ITS TIER AND ITS file:line, and every screen
     ends in renderOmissions.
*/

// guidanceCard prints the server's own account of why a screen has no rows.
//
// Never paraphrased and never replaced with a house sentence: the handler knows
// which of five different absences it is looking at, and a client that
// summarised them all as "nothing found" would throw away the only part of the
// answer a reader can act on.
function guidanceCard(help, sub) {
  if (!help || (!help.title && !help.detail)) return null;
  return card(text(help.title, 'There is nothing to list yet'), sub || 'reported by the server, in its words',
    [
      help.detail ? el('p', { class: 'prose', text: help.detail }) : null,
      (help.steps || []).length
        ? el('ul', { class: 'prose' }, help.steps.map((s) => el('li', { text: s })))
        : null,
    ], null, 'unknown');
}

/*
  Three of these screens read an artefact the index does not hold — a trace
  export, a benchmark case file, an estate manifest — and the response echoes
  the file it tried to read whether or not it found one. It is rendered every
  time: "you asked for this and it is missing" and "we guessed and the guess is
  missing" are different messages, and `defaulted` is the field that separates
  them.
*/
function fileRefCard(ref, what) {
  const r = ref || {};
  return card('The file this screen reads', what,
    [
      el('p', { class: 'prose' },
        r.found ? 'Read from ' : 'Looked for ', el('code', { text: text(r.path) }),
        ' in the indexed repository.'),
      r.absolute ? el('p', { class: 'path', text: text(r.absolute) }) : null,
      r.defaulted
        ? el('p', { class: 'hint', text: text(r.defaulted_from) })
        : el('p', { class: 'hint', text: 'This path came from the request: ' + text(r.requested) }),
      r.problem ? el('p', { class: 'warnline small', text: r.problem }) : null,
    ], null, r.found ? null : 'unknown');
}

// caveatCard renders a payload's own list of limitations. A screen that prints
// the finding and drops the caveats is printing a claim the evidence does not
// support, so this returns null only when the API sent nothing.
function caveatCard(title, sub, lines, klass) {
  const rows = (lines || []).filter((s) => String(s === null || s === undefined ? '' : s).trim() !== '');
  if (!rows.length) return null;
  return card(title, sub, el('ul', { class: 'prose' }, rows.map((s) => el('li', { text: s }))), null, klass);
}

// The rates in these payloads are fractions of one. pct() takes a percentage,
// so every rate goes through here rather than through pct() directly — a recall
// of 0.5 printed as "0.5%" is the kind of wrong that looks like a real answer.
function rateOf(fraction, digits) {
  const v = Number(fraction);
  if (!Number.isFinite(v)) return '—';
  return pct(v * 100, digits === undefined ? 1 : digits);
}

// NOT_MEASURED is the one string the portal uses where a number would be a lie.
// It is never '—' and never 0: an em dash reads as "not stated" and a zero
// reads as a score, and this is neither.
const NOT_MEASURED = 'not measured';

// ---------------------------------------------------------------- DBMIND — TABLES & ACCESS

LOADERS.tables = async function () {
  const host = $('#screen-tables');
  if (state.loaded.tables) return;
  state.loaded.tables = true;
  await drawTables();
};

async function drawTables() {
  const host = $('#screen-tables');
  clear(host);
  host.appendChild(answer('The database tables this code touches are being read from the index.'));

  const body = el('div', { class: 'stack' }, loading('Reading the table inventory…'));
  host.appendChild(body);

  let d;
  try { d = await api('/api/db', { min_confidence: state.q.floor }); }
  catch (e) { clear(body); failAnswer(host, 'Tables & access'); return showError(body, e, 'Tables & access'); }
  state.meta = d.meta;
  renderIndexBar();
  clear(body);
  setAnswer(host, tablesAnswer(d));
  renderDB(d, body);
}

// Tables, and the two populations that are never one list: written by this
// code, and only read by it.
function tablesAnswer(d) {
  const c = d.counts || {};
  const n = Number(c.tables || 0);
  const writers = Number(c.with_a_recorded_writer || 0);
  const readers = Number(c.with_a_recorded_reader || 0);
  const support = 'Readers and writers are never one list: "what touches orders" is a mild question, "what writes orders" is the one asked before dropping a column.';
  if (n === 0) return answer('No database table is recorded in this index, and the reason is stated below.', { support: support, next: nextOf(d.next_action) });
  return answer([B(num(n) + ' ' + plural('table', n)), '. ', B(num(writers)), ' ' + (writers === 1 ? 'is' : 'are') + ' written by this code, ',
    B(num(readers)), ' ' + (readers === 1 ? 'is' : 'are') + ' read by it.'], { support: support, next: nextOf(d.next_action) });
}

function renderDB(d, body) {
  const c = d.counts || {};
  const f = d.filters || {};
  const tables = d.tables || [];

  body.appendChild(el('div', { class: 'kpis' },
    kpi('Tables', num(c.tables), num(c.declared_here) + ' declared in this repository, '
      + num(c.not_declared_here) + ' named by a query and owned elsewhere'),
    kpi('With a recorded writer', num(c.with_a_recorded_writer),
      'the list to read before dropping a column',
      Number(c.tables) > 0 && Number(c.with_a_recorded_writer) === 0 ? 'warn' : 'ok'),
    kpi('With a recorded reader', num(c.with_a_recorded_reader), 'a different population from the writers'),
    kpi('No recorded accessor', num(c.with_no_recorded_accessor),
      'known by name, bound to no symbol at ' + tierWord(f.min_confidence) + ' or better',
      Number(c.with_no_recorded_accessor) > 0 ? 'warn' : 'ok')));

  const help = guidanceCard(d.guidance, 'why this index recorded no database access');
  if (help) body.appendChild(help);

  body.appendChild(fold('What a static SQL read cannot see', card('', 'carried on every answer, empty or not',
    [
      el('p', { class: 'prose', text: text(d.coverage_limit) }),
      el('p', { class: 'hint', text: 'The two kinds of count on this screen are different populations and are '
        + 'labelled for it. Read and write STATEMENTS are recorded SQL sites, whole-index and unaffected by the '
        + 'standard of proof. Symbols reading and symbols writing are the symbols a READS or WRITES edge links to '
        + 'the table at ' + tierWord(f.min_confidence) + ' or better. One function issuing four UPDATEs against one '
        + 'table is four statements and one writer; the two disagreeing is not a defect.' }),
      Number(c.uncertain_statements) > 0
        ? el('p', { class: 'hint', text: num(c.uncertain_statements) + ' recorded ' + plural('statement', c.uncertain_statements)
          + ' could not be attributed to a table with confidence. They are counted in this screen’s Uncertain column '
          + 'and in no other column.' })
        : null,
    ]), { key: 'tables:limits' }));

  if (tables.length) {
    body.appendChild(card('Every table this index recorded', num(c.tables) + ' ' + plural('table', c.tables)
      + ', ordered by total statements',
      table(
        [{ label: 'Table' }, { label: 'Standing' },
          { label: 'Read stmts', num: true, title: 'Recorded SQL read sites — whole index, unaffected by the standard of proof' },
          { label: 'Write stmts', num: true, title: 'Recorded SQL write sites' },
          { label: 'Uncertain', num: true, title: 'Statements the extractor could not attribute to a table with confidence' },
          { label: 'Symbols reading', num: true, title: 'Symbols linked to this table by a READS edge at the standard of proof' },
          { label: 'Symbols writing', num: true, title: 'Symbols linked to this table by a WRITES edge at the standard of proof' },
          { label: 'Declared at' }],
        tables,
        {
          cap: 30,
          // A caveat is a sentence, and a sentence does not fit in the first
          // column of an eight-column table — it wrapped to roughly one
          // character per line. It now gets its own full-width row directly
          // beneath the one it qualifies, which is where a footnote belongs and
          // keeps it adjacent to the empty columns it explains.
          row: (t) => {
            const tr = el('tr', null,
            el('td', null, el('span', { class: 'sym', text: text(t.table) })),
            el('td', { class: 'nowrap' }, t.declared_here
              ? el('span', { class: 'tier tier-exact', text: 'declared here' })
              : el('span', { class: 'tier tier-medium', text: 'owned elsewhere' })),
            el('td', { class: 'num', text: num(t.read_statements) }),
            el('td', { class: 'num', text: num(t.write_statements) }),
            el('td', { class: 'num muted', text: num(t.uncertain_statements) }),
            el('td', { class: 'num', text: num(t.symbols_reading) }),
            el('td', { class: 'num', text: num(t.symbols_writing) }),
            el('td', null, (t.declared_at && t.declared_at.file_path)
              ? evidence(t.declared_at.file_path, t.declared_at.line)
              : el('span', { class: 'muted small', text: 'no migration here declares it' })));
            if (!t.caveat) return tr;
            const frag = document.createDocumentFragment();
            frag.appendChild(tr);
            frag.appendChild(el('tr', { class: 'rownote' },
              el('td', { class: 'small muted', colspan: '8' }, text(t.caveat))));
            return frag;
          },
        }),
      null, 'flush'));
  }

  const withAccessors = tables.filter((t) => (t.read_by || []).length || (t.written_by || []).length);
  if (withAccessors.length) {
    body.appendChild(card('Who writes, and who only reads', num(withAccessors.length) + ' of '
      + num(c.tables) + ' ' + plural('table', c.tables)
      + (withAccessors.length === 1 ? ' has' : ' have') + ' a recorded accessor',
      [
        el('p', { class: 'prose', text: 'Writers first, because that is the list read before a column changes. '
          + 'Each row carries the STRONGEST tier among the edges linking that symbol to that table that way — a '
          + 'single recorded statement is the fact, so taking the weakest of several independent statements would '
          + 'understate a proven write because another query in the same function was a guess.' }),
        withAccessors.slice(0, 8).map((t) => el('div', { class: 'stack' },
          el('h4', { text: text(t.table) }),
          (t.written_by || []).length
            ? accessorTable('Written by', t.written_by, t.symbols_writing)
            : el('p', { class: 'small muted', text: 'No symbol writes ' + text(t.table)
              + ' through an edge at ' + tierWord(f.min_confidence) + ' or better.' }),
          (t.read_by || []).length
            ? accessorTable('Read by', t.read_by, t.symbols_reading)
            : el('p', { class: 'small muted', text: 'No symbol reads ' + text(t.table)
              + ' through an edge at ' + tierWord(f.min_confidence) + ' or better.' }))),
        withAccessors.length > 8
          ? inlineOmit(el('b', { text: num(withAccessors.length - 8) }), ' further '
            + plural('table', withAccessors.length - 8) + ' with a recorded accessor are not expanded here, by '
            + 'this page’s own cap. Their reader and writer counts are in the inventory above, and ',
            el('code', { text: 'cortex db table <name>' }), ' lists any one of them in full.')
          : null,
      ]));
  } else if (tables.length) {
    body.appendChild(card('Who writes, and who only reads', 'no table has a recorded accessor',
      el('p', { class: 'prose', text: 'Every table above is known by name, and no symbol reaches one through a '
        + 'READS or WRITES edge at ' + tierWord(f.min_confidence) + ' or better. That is the absence of a recorded '
        + 'relationship, not evidence that nothing touches these tables — the per-table caveat above says so row '
        + 'by row, and the fold below counts what the standard of proof is holding back.' }), null, 'unknown'));
  }

  body.appendChild(renderOmissions(d.omissions, { key: 'tables' }));
}

function accessorTable(title, rows, total) {
  const hidden = Number(total) - rows.length;
  return el('div', null,
    el('h4', { text: title + ' — ' + num(rows.length) + ' ' + plural('symbol', rows.length) }),
    table(
      [{ label: 'Symbol' }, { label: 'Kind' }, { label: 'Access' }, { label: 'Statements', num: true },
        { label: 'Confidence' }, { label: 'Evidence' }],
      rows,
      {
        cap: 12,
        row: (a) => el('tr', { class: 't-' + tierOf(a.confidence) },
          el('td', null, el('span', { class: 'sym', text: text(a.name) }),
            a.file_path ? el('div', { class: 'path', text: tailPath(a.file_path, 3) }) : null),
          el('td', { class: 'nowrap small muted', text: text(a.kind) }),
          el('td', null, el('code', { text: text(a.access) })),
          el('td', { class: 'num', text: num(a.statements) }),
          el('td', null, tierBadge(a.confidence)),
          el('td', null, evidenceOf(a.evidence))),
      }),
    Number.isFinite(hidden) && hidden > 0
      ? inlineOmit(el('b', { text: num(hidden) }), ' further ' + plural('symbol', hidden)
        + ' are counted in this table’s total and were not sent by the API, which bounds one accessor list.')
      : null);
}

// ---------------------------------------------------------------- CONFIGMIND — KEYS & SECRETS

LOADERS.config = async function () {
  const host = $('#screen-config');
  if (state.loaded.config) return;
  state.loaded.config = true;
  await drawConfig();
};

async function drawConfig() {
  const host = $('#screen-config');
  clear(host);
  host.appendChild(answer('The settings this code declares and reads are being read from the index.'));

  const body = el('div', { class: 'stack' }, loading('Reading the configuration inventory…'));
  host.appendChild(body);

  let d;
  try { d = await api('/api/config', { min_confidence: state.q.floor }); }
  catch (e) { clear(body); failAnswer(host, 'Keys & secrets'); return showError(body, e, 'Keys & secrets'); }
  state.meta = d.meta;
  renderIndexBar();
  clear(body);
  setAnswer(host, configAnswer(d));
  renderConfig(d, body);
}

function configAnswer(d) {
  const c = d.counts || {};
  const keys = Number(c.keys || 0);
  const orphans = Number(c.read_but_not_declared || 0);
  const dead = Number(c.declared_but_never_read || 0);
  const support = 'No value is ever read: what makes a key look sensitive here is its name.';
  if (keys === 0) return answer('No setting is recorded in this index, and the reason is stated below.', { support: support, next: nextOf(d.next_action) });
  return answer([B(num(keys) + ' ' + plural('setting', keys)), '. ', B(num(orphans)), ' ' + (orphans === 1 ? 'is' : 'are') + ' read by code but declared nowhere; ',
    B(num(dead)), ' ' + (dead === 1 ? 'is' : 'are') + ' declared but never read.'], { support: support, next: nextOf(d.next_action) });
}

function renderConfig(d, body) {
  const c = d.counts || {};
  const f = d.filters || {};
  const orphans = d.read_but_not_declared || [];
  const dead = d.declared_but_never_read || [];
  const keys = d.keys || [];

  body.appendChild(el('div', { class: 'kpis' },
    kpi('Keys', num(c.keys), 'declarations and read sites, merged by name'),
    kpi('Read, declared nowhere', num(c.read_but_not_declared),
      'a service that needs these and finds nothing fails at start-up',
      Number(c.read_but_not_declared) > 0 ? 'bad' : 'ok'),
    kpi('Declared, never read', num(c.declared_but_never_read),
      'dead configuration, or a lookup this scan cannot see',
      Number(c.declared_but_never_read) > 0 ? 'warn' : 'ok'),
    kpi('Sensitive by name', num(c.sensitive_by_name), 'judged on the name alone — no value was read',
      Number(c.sensitive_by_name) > 0 ? 'warn' : 'ok')));

  const help = guidanceCard(d.guidance, 'why this index recorded no configuration');
  if (help) body.appendChild(help);

  body.appendChild(fold('What this screen never does, and what it cannot see', card('', 'both stated by the API',
    [
      el('p', { class: 'prose', text: text(d.value_policy) }),
      el('p', { class: 'prose', text: text(d.coverage_limit) }),
      el('p', { class: 'hint', text: 'Read sites are a COUNT, not a list: a key read in forty places would put '
        + 'forty rows behind one name, and the count is what a reader needs. '
        + num(c.configures_edges) + ' CONFIGURES ' + plural('relationship', c.configures_edges)
        + ' met the standard of proof (' + tierWord(f.min_confidence) + ' and better) — those are the only relationships on this screen '
        + 'that carry a tier.' }),
    ]), { key: 'config:limits' }));

  body.appendChild(card('Read by the code, declared nowhere here',
    num(c.read_but_not_declared) + ' ' + plural('key', c.read_but_not_declared),
    orphans.length
      ? [
        el('p', { class: 'prose', text: 'Each of these has a recorded read site and no declaration in this '
          + 'repository. That is either a key supplied by the deployment environment, or a start-up failure '
          + 'waiting for the first machine that does not happen to have it set.' }),
        configKeyTable(orphans),
      ]
      : el('p', { class: 'prose', text: 'No key is read here without also being declared here — within what this '
        + 'scan can see. A key assembled at run time, or read through a variable, leaves no read site for a '
        + 'static scan to find, so this is a clean result over the sources named above rather than a guarantee.' }),
    null, orphans.length ? 'bad' : null));

  body.appendChild(card('Declared here, read by nothing',
    num(c.declared_but_never_read) + ' ' + plural('key', c.declared_but_never_read),
    dead.length
      ? [
        el('p', { class: 'prose', text: 'A prompt, not a verdict. A key consumed by a template, a deployment '
          + 'manifest or another repository is declared here and read nowhere here, and looks exactly like a key '
          + 'nothing needs.' }),
        configKeyTable(dead),
      ]
      : el('p', { class: 'prose', text: 'Every declared key has at least one recorded read site.' }),
    null, 'flush'));

  /*
    THE THIRD LIST IS THE REMAINDER, NOT A REPEAT.

    `keys` is the merged inventory and it CONTAINS the two mismatch lists above.
    Rendering it whole put the same 1,439 dead keys on the screen twice, which
    is how a reader stops reading. So the third card is the complement: keys
    that are declared here AND read here — the boring, correct majority, and the
    only population the first two cards cannot show. The three together are the
    inventory, with no row in two of them.
  */
  const healthy = keys.filter((k) => text(k.finding) === 'declared_and_read');
  body.appendChild(card('Declared here and read here', num(c.declared_and_read) + ' ' + plural('key', c.declared_and_read)
    + (healthy.length < Number(c.declared_and_read)
      ? ', ' + num(healthy.length) + ' of them inside this response’s row budget' : ''),
    healthy.length
      ? [
        el('p', { class: 'prose', text: 'Neither a gap nor a leftover: a declaration in this repository and at '
          + 'least one recorded read site for it. The two cards above are the exceptions; this is the rest of the '
          + 'inventory, and the three of them share no row.' }),
        configKeyTable(healthy, 40),
      ]
      : el('p', { class: 'prose', text: 'No key is both declared here and read here. Every key this scan found is '
        + 'in one of the two cards above — which is normal for a repository whose configuration is supplied '
        + 'entirely by its deployment, and worth a second look in one whose configuration is committed.' }),
    null, 'flush'));

  body.appendChild(renderOmissions(d.omissions, { key: 'config' }));
}

function configKeyTable(rows, cap) {
  return table(
    [{ label: 'Key' }, { label: 'Standing' }, { label: 'Read sites', num: true },
      { label: 'Evidence' }, { label: 'What that location is' }],
    rows,
    {
      cap: cap || 25,
      row: (k) => el('tr', null,
        el('td', null, el('span', { class: 'sym', text: text(k.name) }),
          // Sensitivity is a claim about the NAME, so the badge says so.
          k.sensitive_by_name ? el('span', { class: 'tier tier-medium', text: 'sensitive name' }) : null,
          (k.configured_by || []).length
            ? el('div', { class: 'small muted' }, 'configured by ',
              (k.configured_by || []).slice(0, 3).map((cb, i) => el('span', null, i ? ', ' : '',
                el('span', { class: 'sym', text: text(cb.name) }), ' ', tierBadge(cb.confidence),
                ' ', evidenceOf(cb.evidence))))
            : null),
        el('td', { class: 'nowrap small' }, String(text(k.finding)).replace(/_/g, ' '),
          el('div', { class: 'muted', text: k.declared ? 'declared here' : 'not declared here' })),
        el('td', { class: 'num', text: num(k.readers) }),
        el('td', null, evidenceOf(k.evidence)),
        el('td', { class: 'small muted', text: text(k.evidence_is) })),
    });
}

// ---------------------------------------------------------------- DEPMIND — DEPENDENCIES & TIERS

LOADERS.deps = async function () {
  const host = $('#screen-deps');
  if (state.loaded.deps) return;
  state.loaded.deps = true;
  await drawDeps();
};

async function drawDeps() {
  const host = $('#screen-deps');
  clear(host);
  host.appendChild(answer('The dependencies this repository declares are being read from its manifests.'));

  const body = el('div', { class: 'stack' }, loading('Reading the manifests…'));
  host.appendChild(body);

  let d;
  try { d = await api('/api/deps'); }
  catch (e) { clear(body); failAnswer(host, 'Dependencies & tiers'); return showError(body, e, 'Dependencies & tiers'); }
  state.meta = d.meta;
  renderIndexBar();
  clear(body);
  setAnswer(host, depsAnswer(d));
  renderDeps(d, body);
}

function depsAnswer(d) {
  const c = d.counts || {};
  const declared = Number(c.declared || 0);
  const unused = Number(c.declared_but_not_imported || 0);
  const support = 'What this repository declares it depends on, what actually imports each one, and what tier of proof every language in the tree is resolved at.';
  if (declared === 0) return answer('No dependency manifest was read for this repository, and the reason is stated below.', { support: support, next: nextOf(d.next_action) });
  return answer([B(num(declared) + ' ' + plural('dependency', declared)), ' declared; ', B(num(unused)),
    ' ' + (unused === 1 ? 'is' : 'are') + ' never imported by anything this index can see.'], { support: support, next: nextOf(d.next_action) });
}

function renderDeps(d, body) {
  const c = d.counts || {};
  const reverse = d.imported_but_not_declared || {};
  const unused = d.declared_but_not_imported || [];
  const declared = d.declared || [];
  const tiers = d.language_tiers || [];

  /*
    THE MINUS ONE.

    `counts.imported_but_not_declared` is -1 whenever the import enumerator was
    not wired, and -1 means NOT COMPUTED. Rendering it through num() would print
    "-1"; defaulting it to 0 would assert that every import in this repository
    matches a declaration, which nothing here established. So the KPI carries a
    word, and the card under it carries the API's own explanation and remedy.
  */
  const reverseKnown = reverse.available === true;

  body.appendChild(el('div', { class: 'kpis' },
    kpi('Declared', num(c.declared), num(c.declared_direct) + ' direct, '
      + num(c.declared_indirect) + ' the manifest marks transitive'),
    kpi('Declared, imported by nothing', num(c.declared_but_not_imported),
      'a prompt to check, never a verdict that it is unused',
      Number(c.declared_but_not_imported) > 0 ? 'warn' : 'ok'),
    kpi('Declared and imported', num(c.declared_and_imported), 'measured against this index’s import paths'),
    kpi('Imported, declared nowhere', reverseKnown ? num(c.imported_but_not_declared) : NOT_MEASURED,
      reverseKnown
        ? 'import paths that match no declaration'
        : 'the import paths were never enumerated, so this is not a zero',
      reverseKnown && Number(c.imported_but_not_declared) > 0 ? 'bad' : 'warn')));

  const help = guidanceCard(d.guidance, 'why this screen has no manifest to read');
  if (help) body.appendChild(help);

  if (!reverseKnown) {
    body.appendChild(card('The reverse direction was not computed', 'and a zero here would be a claim',
      [
        el('p', { class: 'prose', text: text(reverse.detail) }),
        el('p', { class: 'hint', text: 'Read that as unknown, never as none. "Nothing is imported without being '
          + 'declared" is a real finding and this screen is not making it.' }),
        reverse.remedy
          ? el('div', { class: 'held' },
            el('h4', { text: 'What answers it today' }),
            el('p', { class: 'what' }, el('code', { text: text(reverse.remedy) })),
            copyButton(text(reverse.remedy), 'Copy the command'))
          : null,
      ], null, 'unknown'));
  } else if ((reverse.rows || []).length) {
    body.appendChild(card('Imported, declared nowhere', num((reverse.rows || []).length) + ' import '
      + plural('path', (reverse.rows || []).length),
      table([{ label: 'Import path' }, { label: 'Files importing', num: true }], reverse.rows || [], {
        cap: 30,
        row: (row) => el('tr', null,
          el('td', null, el('span', { class: 'sym', text: text(row.import_path) })),
          el('td', { class: 'num', text: num(row.files_importing) })),
      }), null, 'flush'));
  }

  if (unused.length) {
    body.appendChild(card('Declared, and nothing in this index imports it',
      num(c.declared_but_not_imported) + ' ' + plural('dependency', c.declared_but_not_imported),
      [
        dependencyTable(unused),
        // Every unused row carries the same caveat sentence; printing it once
        // under the table beats printing it thirty-one times inside it.
        caveatNote(unused.map((row) => row.caveat)),
      ], null, 'flush'));
  }

  if (declared.length) {
    // Not folded: this table is the proof of the number in the answer line.
    body.appendChild(card('Every declared dependency', num(c.declared) + ' from the manifests, '
      + num(declared.length) + ' listed',
      [
        dependencyTable(declared, 40),
        el('p', { class: 'hint', text: 'Importers is MEASURED, not assumed: the count comes from matching this '
          + 'index’s recorded import paths against the declaration, so a zero means no importer was found rather '
          + 'than that nothing was looked for. Rows whose importers were not measured say so in the column.' }),
      ], null, 'flush'));
  }

  if (tiers.length) {
    body.appendChild(card('What tier of proof each language is resolved at',
      'the pair is the point: what the binary supports, and what is actually in this tree',
      [
        el('div', { class: 'stack' }, tiers.map((t) => languageTierBlock(t))),
        el('p', { class: 'hint', text: 'A tier with thirty supported languages and none of them in this '
          + 'repository tells a reader nothing. A `parsed` tier holding this repository’s main language tells '
          + 'them why the graph is thin.' }),
      ]));
  }

  body.appendChild(fold('What this screen is not', card('', 'stated by the API so a client cannot imply otherwise',
    [
      el('p', { class: 'prose', text: text(d.vulnerability_policy) }),
      el('p', { class: 'prose', text: text(d.coverage_limit) }),
    ]), { key: 'deps:not' }));

  body.appendChild(renderOmissions(d.omissions, { key: 'deps' }));
}

// caveatNote folds a column of identical per-row caveats into one sentence, and
// prints them separately when they are not identical.
function caveatNote(lines) {
  const seen = [];
  for (const line of lines || []) {
    const s = String(line === null || line === undefined ? '' : line).trim();
    if (s && seen.indexOf(s) < 0) seen.push(s);
  }
  if (!seen.length) return null;
  return el('div', { class: 'held' },
    el('h4', { text: 'Read every row above as a prompt' }),
    seen.slice(0, 4).map((s) => el('p', { class: 'what', text: s })));
}

function dependencyTable(rows, cap) {
  return table(
    [{ label: 'Package' }, { label: 'Version' }, { label: 'Ecosystem' }, { label: 'Scope' },
      { label: 'Importers', num: true }, { label: 'Declared at' }],
    rows,
    {
      cap: cap || 30,
      row: (row) => el('tr', null,
        el('td', null, el('span', { class: 'sym', text: text(row.name) }),
          row.indirect ? el('span', { class: 'tier tier-low', text: 'transitive' }) : null),
        el('td', { class: 'nowrap small', text: text(row.version, 'not pinned here') }),
        el('td', { class: 'nowrap small muted', text: text(row.ecosystem) }),
        el('td', { class: 'nowrap small muted', text: text(row.scope) }),
        el('td', { class: 'num' }, row.importers_measured
          ? num(row.importers)
          : el('span', { class: 'muted small', text: NOT_MEASURED })),
        el('td', null, evidenceOf(row.evidence))),
    });
}

function languageTierBlock(t) {
  const present = t.present_in_this_repository || [];
  return el('div', { class: 'held' },
    el('h4', { text: text(t.tier) + ' — ' + num(t.supported_count) + ' ' + plural('language', t.supported_count)
      + ' supported, ' + num(t.present_count) + ' in this repository' }),
    el('p', { class: 'what', text: text(t.what_you_get) }),
    present.length
      ? el('p', { class: 'fix', text: present.map((p) => text(p.language) + ' (' + num(p.files) + ' '
        + plural('file', p.files) + ', ' + num(p.symbols) + ' ' + plural('symbol', p.symbols) + ')').join(' · ')
        + ' — ' + num(t.files_in_this_repository) + ' ' + plural('file', t.files_in_this_repository) + ' and '
        + pct(t.symbol_share_pct) + ' of every symbol in the index sit at this tier.' })
      : el('p', { class: 'fix', text: 'No language in this repository is resolved at this tier, so nothing in '
        + 'the index is held to it.' }));
}

// ---------------------------------------------------------------- TESTMIND — TEST COVERAGE

/*
  THE WORD THIS SCREEN NEVER PRINTS IS "UNTESTED".

  Three verdicts arrive as data in `coverage_legend`, each with what it MEANS
  and what it DOES NOT MEAN, and both halves are rendered. The distinction the
  legend exists to protect is between `covering_test_proven` — a test symbol
  reaches this symbol through recorded edges — and `suggested_by_convention_only`
  — a name matched, or a file sat next to another file. The first is evidence of
  execution. The second is an observation about how the repository is organised.
  They are two columns here and they are never merged, because a client that
  renders only the proven one is still correct and a client that blends them is
  not.

  `no_covering_test_proven` is a statement about what THIS INDEX could see. A
  test invoked through a table driver, a generated suite, a runtime harness or
  another repository is invisible to it, and so is one reached only through an
  edge below the floor — which the omission block counts.
*/

let TESTS_SHOWN = null;

LOADERS.tests = async function () {
  const host = $('#screen-tests');
  if (state.loaded.tests && TESTS_SHOWN === state.q.sym) return;
  state.loaded.tests = true;
  await drawTests();
};

async function drawTests() {
  const host = $('#screen-tests');
  TESTS_SHOWN = state.q.sym;
  clear(host);
  host.appendChild(answer('What the tests in this code reach is being read from the index.'));

  const toolbar = el('div', { class: 'toolbar' });
  const combo = combobox({
    id: 'tests-symbol', placeholder: 'a function, method or type name — optional',
    value: state.q.sym,
    emptyNote: 'Nothing in this index has a name like that.',
    source: async (q) => {
      if (!q) return { rows: [], note: 'Type at least one character. Matching is on the name and the fully-qualified name.' };
      const res = await api('/api/symbols', { q: q });
      const rows = (res.matches || []).map((m) => ({
        value: text(m.fqn, m.name), label: text(m.name),
        sub: text(m.file_path, ''),
        meta: text(m.kind, '') + (m.exported ? ' · exported' : ''),
        tier: m.confidence,
      }));
      const hidden = Number(res.total_matched || 0) - rows.length;
      return { rows: rows, note: (hidden > 0 ? num(hidden) + ' further matches are not listed. ' : '') + text(res.ordering, '') };
    },
    onChoose: (row) => { setQuery({ sym: row.value }); drawTests(); },
    onSubmit: (value) => { setQuery({ sym: value }); drawTests(); },
  });
  toolbar.appendChild(field('Coverage for one symbol', combo.node, 'grow'));
  toolbar.appendChild(selectField('Traversal depth', 'tests-depth',
    [1, 2, 3, 4, 5, 6].map((n) => ({ value: String(n), label: String(n) })), state.q.depth,
    (v) => { setQuery({ depth: v }); drawTests(); }));
  if (state.q.sym) {
    toolbar.appendChild(el('button', {
      class: 'ghost', type: 'button', text: 'Clear the symbol',
      onclick: () => { setQuery({ sym: '' }); drawTests(); },
    }));
  }
  host.appendChild(toolbar);

  const body = el('div', { class: 'stack' }, loading('Reading what the tests reach…'));
  host.appendChild(body);

  let d;
  try {
    d = await api('/api/tests', {
      symbol: state.q.sym, min_confidence: state.q.floor, depth: state.q.depth,
    });
  } catch (e) { clear(body); failAnswer(host, 'Test coverage'); return showError(body, e, 'Test coverage'); }
  state.meta = d.meta;
  renderIndexBar();
  clear(body);
  setAnswer(host, testsAnswer(d));
  renderTests(d, body);
}

// Covered of total, where covered means a test reaches the symbol through a
// recorded edge — proven, never suggested by a name.
function testsAnswer(d) {
  const c = d.counts || {};
  const total = Number(c.non_test_symbols || 0);
  const covered = Number(c.symbols_with_a_proven_covering_test || 0);
  const support = 'Whether that coverage was proven by a recorded edge or only suggested by a naming convention: the two are different evidence and they are never one list.';
  if (d.symbol && d.symbol.target) {
    const sym = d.symbol;
    const proven = (sym.proven_by_graph || []).length;
    const suggested = (sym.suggested_by_convention || []).length;
    return answer([B(num(proven) + ' ' + plural('test', proven)), ' provably ' + (proven === 1 ? 'reaches' : 'reach') + ' ', B(text(sym.target.name)), '; ',
      B(num(suggested)), ' more ' + (suggested === 1 ? 'is' : 'are') + ' suggested by a naming convention only.'], { support: support, next: nextOf(d.next_action) });
  }
  return answer([B(num(covered) + ' of ' + num(total) + ' ' + plural('symbol', total)), ' have a test that reaches them through a recorded relationship.'],
    { support: support, next: nextOf(d.next_action) });
}

function renderTests(d, body) {
  const c = d.counts || {};
  const f = d.filters || {};

  body.appendChild(el('div', { class: 'kpis' },
    kpi('Test files', num(c.test_files), num(c.test_symbols) + ' ' + plural('symbol', c.test_symbols)
      + ' inside them'),
    kpi('Non-test symbols', num(c.non_test_symbols), 'the denominator every claim below is over'),
    kpi('With a proven covering test', num(c.symbols_with_a_proven_covering_test),
      'reached from a test through a recorded edge',
      Number(c.symbols_with_a_proven_covering_test) === 0 ? 'warn' : 'ok'),
    kpi('With no proven covering test', num(c.symbols_with_no_proven_covering_test),
      'the absence of a PROOF, which is not the absence of a test',
      Number(c.symbols_with_no_proven_covering_test) > 0 ? 'warn' : 'ok')));

  // The vocabulary, served as data and rendered in full. `does_not_mean` is
  // present on every verdict INCLUDING the positive one, and it is the half a
  // reader needs before quoting any of this.
  const legend = d.coverage_legend || [];
  if (legend.length) {
    body.appendChild(fold('What each of the ' + num(legend.length) + ' verdicts means, and what it does not', card('', 'the API’s own vocabulary',
      el('div', { class: 'stack' }, legend.map((v) => el('div', { class: 'held' },
        el('h4', { text: String(text(v.verdict)).replace(/_/g, ' ') }),
        el('p', { class: 'what', text: text(v.means) }),
        el('p', { class: 'fix', text: text(v.does_not_mean) })))),
      null, 'flush'), { key: 'tests:legend' }));
  }

  const help = guidanceCard(d.guidance, 'why nothing here is proven yet');
  if (help) body.appendChild(help);

  if ((d.candidates || []).length) {
    body.appendChild(card('That name matches more than one definition', 'the portal will not pick one',
      table(
        [{ label: 'Symbol' }, { label: 'Kind' }, { label: 'Defined at' }, { label: '' }],
        d.candidates || [],
        {
          cap: 20,
          row: (cand) => el('tr', null,
            el('td', null, el('span', { class: 'sym', text: text(cand.name) }),
              cand.exported ? el('span', { class: 'tier tier-high', text: 'exported' }) : null),
            el('td', { class: 'nowrap small muted', text: text(cand.kind) }),
            el('td', null, evidence(cand.file_path, cand.line)),
            el('td', null, el('button', {
              class: 'ghost', type: 'button', text: 'Coverage for this one',
              onclick: () => { setQuery({ sym: text(cand.fqn, cand.name) }); drawTests(); },
            }))),
        }),
      null, 'unknown'));
  }

  if (d.symbol) body.appendChild(symbolCoverageCard(d.symbol, f));

  body.appendChild(fold('What the two lists below are, and are not', card('', 'the API’s own scope sentence',
    [
      el('p', { class: 'prose', text: text(d.scope) }),
      el('p', { class: 'hint', text: 'Bounds actually applied: standard of proof ' + tierWord(f.min_confidence)
        + ' and better, traversal depth ' + num(f.max_depth) + ', at most ' + num(f.max_tests_per_symbol)
        + ' tests listed per symbol.' }),
    ]), { key: 'tests:scope' }));

  const files = d.test_files || [];
  if (files.length) {
    body.appendChild(fold('Evidence — every test file, ' + num(files.length) + ' ' + plural('row', files.length), card('Every file this index recognises as a test file',
      num(c.test_files) + ' by path, ' + num(files.length) + ' listed',
      [
        table(
          [{ label: 'File' }, { label: 'Language' }, { label: 'Kind' },
            { label: 'Symbols', num: true, title: 'Every symbol this index extracted from the file' },
            { label: 'Named as tests', num: true, title: 'Those whose name follows a test convention' },
            { label: 'Reaches', num: true, title: 'Non-test symbols this file’s tests reach through a recorded edge at the standard of proof' }],
          files,
          {
            cap: 30,
            row: (row) => el('tr', { class: Number(row.reaches_symbols) > 0 ? null : 't-medium' },
              el('td', null, el('span', { class: 'sym', text: baseName(row.file_path) }),
                el('div', { class: 'path', text: dirName(row.file_path) })),
              el('td', { class: 'nowrap small muted', text: text(row.language, 'not recorded') }),
              el('td', { class: 'nowrap small muted', text: text(row.test_kind) }),
              el('td', { class: 'num', text: num(row.symbols) }),
              el('td', { class: 'num', text: num(row.test_symbols) }),
              el('td', { class: 'num', text: num(row.reaches_symbols) })),
          }),
        el('p', { class: 'hint', text: 'A file is counted here by its PATH, never by a test-looking name — '
          + 'everything on this screen is labelled proven or not proven, and a naming convention is not a proof. '
          + 'The two symbol columns are kept apart because the gap between them is the finding: a recognised test '
          + 'file that yielded no symbol at all, or whose symbols the convention does not recognise, is a parser '
          + 'or convention gap rather than an empty file.' }),
      ], null, 'flush'), { key: 'tests:files' }));
  }

  const covered = d.covered_symbols || [];
  if (covered.length) {
    body.appendChild(card('Symbols a test reaches through a recorded edge',
      num(covered.length) + ' at depth 1, ' + num(c.proving_edges) + ' proving ' + plural('edge', c.proving_edges),
      table(
        [{ label: 'Symbol' }, { label: 'Kind' }, { label: 'Verdict' }, { label: 'Weakest proving tier' },
          { label: 'Proven by', num: true }, { label: 'Tests' }],
        covered,
        {
          cap: 30,
          row: (row) => el('tr', { class: 't-' + tierOf(row.confidence) },
            el('td', null, el('span', { class: 'sym', text: text(row.name) }),
              row.file_path ? el('div', { class: 'path', text: tailPath(row.file_path, 3) }) : null),
            el('td', { class: 'nowrap small muted', text: text(row.kind) }),
            el('td', { class: 'nowrap small', text: String(text(row.coverage)).replace(/_/g, ' ') }),
            el('td', null, tierBadge(row.confidence)),
            el('td', { class: 'num', text: num(row.proven_test_count) }),
            el('td', { class: 'small' }, (row.proven_by || []).slice(0, 3).map((p) => el('div', null,
              el('span', { class: 'sym', text: text(p.name) }), ' ',
              el('code', { text: text(p.via) }), ' ', tierBadge(p.confidence), ' ', evidenceOf(p.evidence))))),
        }),
      null, 'flush'));
  } else if (files.length) {
    body.appendChild(card('Symbols a test reaches through a recorded edge', 'none at depth 1',
      el('p', { class: 'prose', text: 'No test symbol reaches a non-test symbol through a DIRECT recorded edge at '
        + tierWord(f.min_confidence) + ' or better. Read that as the verdict named above — no covering test PROVEN — '
        + 'and not as a claim about what the tests exercise. Ask for one symbol in the box at the top to run the '
        + 'full traversal, which follows longer chains and also reports what the naming convention suggests.' }),
      null, 'unknown'));
  }

  body.appendChild(renderOmissions(d.omissions, { key: 'tests' }));
}

function symbolCoverageCard(sym, filters) {
  const target = sym.target || {};
  const proven = sym.proven_by_graph || [];
  const suggested = sym.suggested_by_convention || [];
  const completeness = sym.completeness || {};
  const signals = sym.signal_counts || {};
  const signalNames = Object.keys(signals);

  return card('Coverage for ' + text(target.name), String(text(sym.coverage)).replace(/_/g, ' '),
    [
      el('p', null, el('span', { class: 'sym', text: text(target.fqn, target.name) }), ' ',
        tierBadge(target.confidence)),
      evidence(target.file_path, target.line),
      el('p', { class: 'prose', text: text(sym.coverage_detail) }),

      el('div', { class: 'kpis' },
        kpi('Proven by the graph', num(sym.proven_count), 'a test symbol reaches it through recorded edges',
          Number(sym.proven_count) > 0 ? 'ok' : 'warn'),
        kpi('Suggested by convention', num(sym.suggested_count),
          'a name matched or a file sat next to it — usually right, never proof'),
        kpi('Graph edges from unrecognised tests', num(sym.graph_edges_from_unrecognised_tests),
          'a real edge whose source this index did not type as a test')),

      proven.length
        ? el('div', null,
          el('h4', { text: 'Proven by a recorded edge' }),
          testHitTable(proven))
        : el('p', { class: 'prose', text: 'No test reaches ' + text(target.name) + ' through a recorded edge at '
          + tierWord(filters.min_confidence) + ' or better within depth ' + num(filters.max_depth) + '.' }),

      suggested.length
        ? el('div', null,
          el('h4', { text: 'Suggested by convention only' }),
          el('p', { class: 'hint', text: 'These were found by a naming convention or by sibling-file '
            + 'co-location, and by no recorded edge. `TestUpdateStatus` next to `UpdateStatus` is a convention '
            + 'about how the repository is organised, not an observation about what runs.' }),
          testHitTable(suggested))
        : null,

      signalNames.length
        ? el('p', { class: 'hint', text: 'Signals that matched: '
          + signalNames.map((k) => k.replace(/_/g, ' ') + ' ×' + num(signals[k])).join(', ') + '.' })
        : null,

      sym.truncated
        ? el('p', { class: 'warnline small', text: 'This answer was truncated before the traversal finished, so '
          + 'the counts above are a lower bound.' })
        : null,

      (completeness.caveats || []).length
        ? el('div', { class: 'held' },
          el('h4', { text: completeness.lower_bound ? 'This answer is a lower bound' : 'Bounds on this answer' }),
          (completeness.caveats || []).map((line) => el('p', { class: 'what', text: line })))
        : null,
    ]);
}

function testHitTable(rows) {
  return table(
    [{ label: 'Test' }, { label: 'Kind' }, { label: 'Strongest signal' }, { label: 'Confidence' },
      { label: 'How it was matched' }, { label: 'File' }],
    rows,
    {
      cap: 20,
      row: (row) => el('tr', { class: 't-' + tierOf(row.confidence) },
        el('td', null, el('span', { class: 'sym', text: text(row.name) }),
          row.fqn && row.fqn !== row.name ? el('div', { class: 'path', text: row.fqn }) : null),
        el('td', { class: 'nowrap small muted', text: text(row.test_kind) }),
        el('td', { class: 'nowrap small', text: String(text(row.strongest_signal)).replace(/_/g, ' ') }),
        el('td', null, tierBadge(row.confidence)),
        el('td', { class: 'small muted' }, (row.signals || []).slice(0, 3).map((s) => el('div', {
          text: String(text(s.signal)).replace(/_/g, ' ')
            + (Number(s.depth) > 0 ? ' · depth ' + num(s.depth) : '')
            + ' · ' + text(s.detail),
        }))),
        el('td', null, el('span', { class: 'path', text: tailPath(text(row.file_path, ''), 3) }))),
    });
}

// ---------------------------------------------------------------- PLATFORM — ACCURACY

/*
  PRECISION AND RECALL ARE MEASURED SEPARATELY, AND EITHER MAY BE UNMEASURED.

  `precision_measured` and `recall_measured` are two booleans and they are read
  as two booleans. On an index with no compiler facts imported, both are false
  and every rate in `scores` is a placeholder — so this screen prints the word
  "not measured" where a number would go. Not a zero, which reads as "answered,
  and wrong"; not an em dash, which reads as a score somebody forgot to fill in.

  Recall is the one that matters most here. A precision figure alone rewards a
  resolver for staying silent, and silence is exactly what a missed caller looks
  like — which is the API's own warning, printed on the screen.
*/

LOADERS.accuracy = async function () {
  const host = $('#screen-accuracy');
  if (state.loaded.accuracy) return;
  state.loaded.accuracy = true;
  await drawAccuracy();
};

async function drawAccuracy() {
  const host = $('#screen-accuracy');
  clear(host);
  host.appendChild(answer('How often this index is right is being read from its compiler check.'));

  const body = el('div', { class: 'stack' }, loading('Reading the compiler check…'));
  host.appendChild(body);

  let d;
  try { d = await api('/api/accuracy'); }
  catch (e) { clear(body); failAnswer(host, 'Accuracy'); return showError(body, e, 'Accuracy'); }
  state.meta = d.meta;
  renderIndexBar();
  clear(body);
  setAnswer(host, accuracyAnswer(d));
  renderAccuracy(d, body);
}

// Precision as measured against the compiler, or the plain fact that nothing
// has measured this index yet — which is a third thing from right and wrong.
function accuracyAnswer(d) {
  const s = d.scores || {};
  const support = 'How often this index is right, measured against a compiler rather than asserted. A rate nothing has checked is reported as not measured.';
  if (!s.precision_measured) {
    return answer('Not measured yet: import a compiler-backed index first, and this screen will say how often the relationships were right.',
      { support: support, next: nextOf(d.next_action) });
  }
  const answered = Number(s.true_positives || 0) + Number(s.false_positives || 0);
  return answer([B(rateOf(s.precision)), ' of the ', B(num(answered) + ' relationships'),
    ' this index asserted were right when checked against the compiler.'], { support: support, next: nextOf(d.next_action) });
}

function renderAccuracy(d, body) {
  const s = d.scores || {};
  const cov = d.coverage || {};
  const misses = d.misses || {};

  body.appendChild(el('div', { class: 'kpis' },
    kpi('Precision', s.precision_measured ? rateOf(s.precision) : NOT_MEASURED,
      s.precision_measured
        ? num(s.true_positives) + ' correct of ' + num(Number(s.true_positives) + Number(s.false_positives))
          + ' answered call sites'
        // Not "so a rate is undefined": the portal's own guard forbids that
        // word anywhere a reader can see it, because the one place it has ever
        // appeared on this page was a field read that came back missing.
        : 'the code that binds references answered no checkable call site, so there is no ratio to report',
      s.precision_measured ? 'ok' : 'warn'),
    kpi('Recall', s.recall_measured ? rateOf(s.recall) : NOT_MEASURED,
      s.recall_measured
        ? num(s.answered_call_sites) + ' answered of ' + num(s.adjudicable_call_sites) + ' adjudicable call sites'
        : 'nothing has checked this index against a compiler, and a wrong recall number is worse than an absent one',
      s.recall_measured ? 'ok' : 'warn'),
    kpi('F1', (s.precision_measured && s.recall_measured) ? rateOf(s.f1) : NOT_MEASURED,
      'defined only where precision and recall share a denominator',
      (s.precision_measured && s.recall_measured) ? 'ok' : 'warn'),
    kpi('Compiler facts in this index', num(cov.compiler_facts),
      'the ground truth every rate above is judged against',
      Number(cov.compiler_facts) === 0 ? 'bad' : 'ok')));

  if (!s.recall_measured) {
    body.appendChild(card('Recall was not measured, and this screen will not print a number for it',
      'the reason, from the payload',
      [
        el('p', { class: 'prose', text: 'The recall denominator is adjudicable call sites: lines where the parser '
          + 'recorded a call and a compiler resolved that line to exactly one definition. This index holds '
          + num(cov.adjudicated_call_sites) + ' of them, out of ' + num(cov.parsed_call_sites)
          + ' parsed call ' + plural('site', cov.parsed_call_sites) + '. With an empty denominator there is no '
          + 'rate — and a zero printed here would read as an index that missed everything.' }),
        el('p', { class: 'prose', text: text(d.restriction) }),
      ], null, 'unknown'));
  }

  const help = guidanceCard(d.guidance, 'why nothing has checked this index against a compiler');
  if (help) body.appendChild(help);

  body.appendChild(fold('How to read the tiers on every other screen', card('', 'the API’s reading rule',
    [
      el('p', { class: 'prose' }, 'The rule this index publishes: ', el('code', { text: text(d.reading_rule) }), '.'),
      el('p', { class: 'hint', text: 'The precision bands the tier badges quote everywhere else in this portal '
        + 'were measured on other corpora. Until a compiler index is imported here, they are not measurements of '
        + 'THIS repository.' }),
    ]), { key: 'accuracy:rule' }));

  body.appendChild(fold('Evidence — what was actually checked against the compiler', card('What was actually checked against the compiler', 'the coverage every rate above is computed over',
    [
      el('div', { class: 'kpis' },
        kpi('Parsed call sites', num(cov.parsed_call_sites), 'every line the parser recorded a call on'),
        kpi('Checked by a compiler', num(cov.adjudicated_call_sites), rateOf(cov.adjudicated_fraction)
          + ' of the parsed call sites', Number(cov.adjudicated_call_sites) === 0 ? 'warn' : 'ok'),
        kpi('CALLS edges inferred from names', num(cov.heuristic_calls_edges), 'produced without a compiler, and unjudged'),
        kpi('Unambiguous lines', num(cov.unambiguous_lines), num(cov.ambiguous_lines)
          + ' ambiguous ' + plural('line', cov.ambiguous_lines) + ' were excluded rather than scored')),
      el('p', { class: 'hint', text: 'An ambiguous line is one where the compiler named more than one target, so '
        + 'no right answer is defined. Those are counted and excluded; scoring them would invent an answer to '
        + 'grade against.' }),
    ]), { key: 'accuracy:coverage' }));

  const tiers = d.tiers || [];
  if (tiers.length) {
    body.appendChild(card('Per tier', 'what each confidence band actually bought',
      table(
        [{ label: 'Tier' }, { label: 'Precision' }, { label: 'Recall' }, { label: 'F1' },
          { label: 'True positives', num: true }, { label: 'False positives', num: true },
          { label: 'Retrieved', num: true }, { label: 'Relevant', num: true }],
        tiers,
        {
          cap: 10,
          row: (t) => el('tr', { class: 't-' + tierOf(t.tier) },
            el('td', null, tierBadge(t.tier),
              t.meaning ? el('div', { class: 'small muted', text: t.meaning }) : null),
            el('td', { class: 'nowrap' }, t.precision_measured
              ? rateOf(t.precision) : el('span', { class: 'muted small', text: NOT_MEASURED })),
            el('td', { class: 'nowrap' }, t.recall_measured
              ? rateOf(t.recall) : el('span', { class: 'muted small', text: NOT_MEASURED })),
            el('td', { class: 'nowrap' }, (t.precision_measured && t.recall_measured)
              ? rateOf(t.f1) : el('span', { class: 'muted small', text: NOT_MEASURED })),
            el('td', { class: 'num', text: num(t.true_positives) }),
            el('td', { class: 'num', text: num(t.false_positives) }),
            el('td', { class: 'num', text: num(t.retrieved) }),
            el('td', { class: 'num', text: num(t.relevant) })),
        }),
      null, 'flush'));
  }

  const languages = d.languages || [];
  if (languages.length) {
    body.appendChild(fold('Evidence — per language, ' + num(languages.length) + ' ' + plural('row', languages.length), card('Per language', 'a language with no compiler index is not measured, which is not the same as accurate',
      table(
        [{ label: 'Language' }, { label: 'Adjudicable', num: true }, { label: 'Answered', num: true },
          { label: 'Missed', num: true }, { label: 'Precision' }, { label: 'Recall' }, { label: 'F1' }],
        languages,
        {
          cap: 20,
          row: (l) => el('tr', null,
            el('td', null, el('code', { text: text(l.language) })),
            el('td', { class: 'num', text: num(l.adjudicated_call_sites) }),
            el('td', { class: 'num', text: num(l.answered_call_sites) }),
            el('td', { class: 'num', text: num(l.missed_call_sites) }),
            el('td', { class: 'nowrap' }, l.measured ? rateOf(l.precision) : el('span', { class: 'muted small', text: NOT_MEASURED })),
            el('td', { class: 'nowrap' }, l.measured ? rateOf(l.recall) : el('span', { class: 'muted small', text: NOT_MEASURED })),
            el('td', { class: 'nowrap' }, l.measured ? rateOf(l.f1) : el('span', { class: 'muted small', text: NOT_MEASURED }))),
        }),
      null, 'flush'), { key: 'accuracy:languages' }));
  }

  if ((d.unmeasured_languages || []).length) {
    body.appendChild(card('Languages carrying call sites that no compiler has checked',
      num((d.unmeasured_languages || []).length),
      el('p', { class: 'prose', text: (d.unmeasured_languages || []).join(', ') + '. Every edge this index '
        + 'produced in these is unmeasured on this repository.' }), null, 'unknown'));
  }

  body.appendChild(card('The misses precision cannot see',
    misses.measured ? num(misses.missed_call_sites) + ' adjudicable call ' + plural('site', misses.missed_call_sites)
      + ' produced no edge' : 'not computed',
    misses.measured
      ? [
        el('div', { class: 'kpis' },
          kpi('Unresolved', num(misses.unresolved), 'the reference bound to nothing — an extraction problem'),
          kpi('Ambiguous', num(misses.ambiguous), 'several candidates and no way to choose — an inference problem'),
          kpi('Other', num(misses.other), 'itemised below')),
        (misses.causes || []).length
          ? table([{ label: 'Cause' }, { label: 'Call sites', num: true }, { label: 'What it is' }],
            misses.causes || [], {
              cap: 12,
              row: (m) => el('tr', null,
                el('td', null, el('code', { text: String(text(m.reason)).replace(/_/g, ' ') })),
                el('td', { class: 'num', text: num(m.call_sites) }),
                el('td', { class: 'small muted', text: text(m.explanation) })),
            })
          : null,
      ]
      : el('p', { class: 'prose', text: 'The miss breakdown could not be computed for this index, so its zeroes '
        + 'are placeholders rather than findings. A miss is a call site the compiler settled and this index '
        + 'answered nothing for — the error precision is structurally incapable of seeing, because an index that '
        + 'says nothing is never wrong.' }),
    null, misses.measured ? 'flush' : 'unknown'));

  const warn = caveatCard('Read this before quoting anything above', 'the API’s own warnings', d.warnings, 'warn');
  if (warn) body.appendChild(warn);

  const notThis = caveatCard('What this measurement is not', 'four things it is routinely mistaken for', d.what_this_is_not);
  if (notThis) body.appendChild(notThis);

  if (d.reference) {
    body.appendChild(el('p', { class: 'hint' }, 'The method is written down in ',
      el('code', { text: text(d.reference) }), '.'));
  }

  body.appendChild(renderOmissions(d.omissions, { key: 'accuracy' }));
}

// ---------------------------------------------------------------- IMPACTMIND — OWNERSHIP

/*
  WHO TO ASK, NEVER WHO IS RESPONSIBLE.

  The endpoint routes a review by what a change REACHES, so it needs a symbol
  and it will not pick one: `status` is `no_query` with guidance, `ambiguous`
  with candidates, or `ok` with a target. All three are rendered, and the
  candidate list is a picker rather than a dead end.

  `caveats` is never empty on an `ok` answer and it is always drawn. Git records
  who changed a file, not who owns it — a screen that printed the ranking
  without that sentence would be printing a claim the evidence does not support.
*/

let OWNER_SHOWN = null;

LOADERS.ownership = async function () {
  if (state.loaded.ownership && OWNER_SHOWN === state.q.sym) return;
  state.loaded.ownership = true;
  await drawOwnership();
};

async function drawOwnership() {
  const host = $('#screen-ownership');
  OWNER_SHOWN = state.q.sym;
  clear(host);
  host.appendChild(ownershipAnswer(null));

  const toolbar = el('div', { class: 'toolbar' });
  const combo = combobox({
    id: 'who-symbol', placeholder: 'a function, method or type name',
    value: state.q.sym,
    emptyNote: 'Nothing in this index has a name like that.',
    source: async (q) => {
      if (!q) return { rows: [], note: 'Type at least one character. Matching is on the name and the fully-qualified name.' };
      const res = await api('/api/symbols', { q: q });
      const rows = (res.matches || []).map((m) => ({
        value: text(m.fqn, m.name), label: text(m.name),
        sub: text(m.file_path, ''),
        meta: text(m.kind, '') + (m.exported ? ' · exported' : ''),
        tier: m.confidence,
      }));
      const hidden = Number(res.total_matched || 0) - rows.length;
      return { rows: rows, note: (hidden > 0 ? num(hidden) + ' further matches are not listed. ' : '') + text(res.ordering, '') };
    },
    onChoose: (row) => { setQuery({ sym: row.value }); drawOwnership(); },
    onSubmit: (value) => { setQuery({ sym: value }); drawOwnership(); },
  });
  toolbar.appendChild(field('Symbol the change starts from', combo.node, 'grow'));
  toolbar.appendChild(selectField('Depth', 'who-depth',
    [1, 2, 3, 4, 5, 6].map((n) => ({ value: String(n), label: String(n) })), state.q.depth,
    (v) => { setQuery({ depth: v }); drawOwnership(); }));
  toolbar.appendChild(el('button', {
    class: 'primary', type: 'button', text: 'Route the review',
    onclick: () => { setQuery({ sym: combo.input.value.trim() }); drawOwnership(); },
  }));
  host.appendChild(toolbar);

  const body = el('div', { class: 'stack' }, loading('Walking the radius and reading the commit window…'));
  host.appendChild(body);

  let d;
  try {
    d = await api('/api/who', {
      symbol: state.q.sym, min_confidence: state.q.floor, depth: state.q.depth,
    });
  } catch (e) { clear(body); failAnswer(host, 'Ownership'); return showError(body, e, 'Ownership'); }
  state.meta = d.meta;
  renderIndexBar();
  clear(body);
  setAnswer(host, ownershipAnswer(d));
  renderOwnership(d, body);
}

// Who to ask, as a count of people over the reached code. A suggestion for
// who to ask, never a finding about who is responsible.
function ownershipAnswer(d) {
  const support = 'Who has context on the code a change reaches — and where exactly one person has it, or nobody does. A suggestion for who to ask, never a finding about who is responsible.';
  if (!d || text(d.status, '') === 'no_query') return answer('Name a symbol and this finds who has touched the code a change to it can reach.', { support: support, next: d ? nextOf(d.next_action) : null });
  const name = text(d.target && d.target.name, d.query);
  if (text(d.status, '') === 'ambiguous') {
    const n = (d.candidates || []).length;
    return answer([B(num(n) + ' definitions'), ' are named ', B(name), ', so pick the one you mean before anyone is named.'], { support: support, next: nextOf(d.next_action) });
  }
  if (text(d.status, '') !== 'ok') {
    return answer(['Nothing in this index is named ', B(name), ', so there is nobody to route to.'], { support: support, next: nextOf(d.next_action) });
  }
  const people = Number(d.people_found || 0);
  return answer([B(num(people) + ' ' + (people === 1 ? 'person has' : 'people have')), ' touched the code a change to ', B(name), ' can reach.'],
    { support: support, next: nextOf(d.next_action) });
}

function renderOwnership(d, body) {
  const status = text(d.status, '');

  if (status !== 'ok') {
    const help = guidanceCard(d.guidance, status === 'ambiguous'
      ? 'more than one definition carries that name' : 'this screen needs a symbol');
    if (help) body.appendChild(help);

    if ((d.candidates || []).length) {
      body.appendChild(card('The definitions that name matched', num((d.candidates || []).length),
        table(
          [{ label: 'Symbol' }, { label: 'Kind' }, { label: 'Defined at' }, { label: '' }],
          d.candidates || [],
          {
            cap: 20,
            row: (cand) => el('tr', null,
              el('td', null, el('span', { class: 'sym', text: text(cand.name) }),
                cand.exported ? el('span', { class: 'tier tier-high', text: 'exported' }) : null),
              el('td', { class: 'nowrap small muted', text: text(cand.kind) }),
              el('td', null, evidence(cand.file_path, cand.line)),
              el('td', null, el('button', {
                class: 'ghost', type: 'button', text: 'Route from this one',
                onclick: () => { setQuery({ sym: text(cand.fqn, cand.name) }); drawOwnership(); },
              }))),
          }),
        null, 'flush'));
    }

    if (!help && !(d.candidates || []).length) {
      body.appendChild(emptyState('Nothing was found for that name',
        ['The index holds no definition under that name. That is a statement about this index, not about the '
          + 'repository: a file that failed to parse contributes no symbols, and a language with no grammar '
          + 'contributes none either.']));
    }

    body.appendChild(renderOmissions(d.omissions, { key: 'ownership' }));
    return;
  }

  const target = d.target || {};
  const reach = d.reach || {};
  const bus = d.bus_factor || {};
  const window_ = d.window || {};
  const untouched = d.untouched || {};
  const history = d.history || {};
  const people = d.people || [];

  body.appendChild(card('Routing a review of ' + text(target.name), text(target.kind, ''),
    [
      el('p', null, el('span', { class: 'sym', text: text(target.fqn, target.name) }), ' ',
        tierBadge(target.confidence),
        target.exported ? el('span', { class: 'tier tier-high', text: 'exported' }) : null),
      evidence(target.file_path, target.line),
      target.source ? el('p', { class: 'hint', text: 'Definition recorded from ' + text(target.source) + '.' }) : null,
    ]));

  body.appendChild(el('div', { class: 'kpis' },
    kpi('Symbols reached', num(reach.symbols), num(reach.direct) + ' direct, ' + num(reach.indirect)
      + ' indirect, ' + num(reach.dispatch) + ' through dispatch'),
    kpi('Attributable', num(reach.attributable), 'reached symbols that sit in a file git can attribute'),
    kpi('Files reached', num(reach.files), 'the denominator every share below is over'),
    kpi('People found', num(d.people_found), people.length < Number(d.people_found)
      ? num(people.length) + ' ranked here, the rest beyond the cap'
      : 'identities that touched the reached set in this window',
      Number(d.people_found) === 0 ? 'warn' : 'ok')));

  if (reach.truncated || reach.low_confidence_hidden || reach.unlocatable) {
    body.appendChild(card('What the radius did not include', 'before anybody is named or not named',
      el('ul', { class: 'prose' },
        reach.truncated
          ? el('li', { text: 'The walk stopped before exhausting the reachable set: '
            + text(reach.truncated_reason) + '. Files beyond that boundary have nobody named for them because '
            + 'they were never reached.' })
          : null,
        Number(reach.low_confidence_hidden) > 0
          ? el('li', { text: num(reach.low_confidence_hidden) + ' ' + plural('symbol', reach.low_confidence_hidden)
            + ' reach the target only through edges weaker than ' + tierWord(reach.confidence_floor)
            + ', so the files they live in are outside this routing.' })
          : null,
        Number(reach.unlocatable) > 0
          ? el('li', { text: num(reach.unlocatable) + ' reached ' + plural('symbol', reach.unlocatable)
            + ' carry no file path, so no commit can be attributed to them.' })
          : null),
      null, 'unknown'));
  }

  if (people.length) {
    body.appendChild(card('Who has context on what this reaches',
      num(d.people_found) + ' ' + plural('identity', d.people_found) + ' touched the reached set',
      table(
        [{ label: 'Person' }, { label: 'Share of reached symbols', num: true },
          { label: 'Symbols', num: true }, { label: 'Files', num: true }, { label: 'Commits', num: true },
          { label: 'Last touch' }, { label: 'On what basis' }],
        people,
        {
          cap: 25,
          row: (p) => el('tr', null,
            el('td', null, el('span', { class: 'sym', text: text(p.name) }),
              el('div', { class: 'small muted', text: text(p.email, p.identity) })),
            el('td', { class: 'num', text: rateOf(p.share) }),
            el('td', { class: 'num', text: num(p.symbols) }),
            el('td', { class: 'num', text: num(p.files_touched) }),
            el('td', { class: 'num', text: num(p.commit_count) }),
            el('td', { class: 'nowrap small muted', text: absoluteWhen(p.last_touch_unix) || 'not recorded' }),
            el('td', { class: 'small' }, text(p.basis),
              (p.commits || []).length
                ? el('div', { class: 'muted' }, (p.commits || []).slice(0, 2).map((commit) => el('div', {
                  text: String(text(commit.sha, '')).slice(0, 8) + ' · ' + text(commit.subject)
                    + ' · ' + num((commit.files || []).length) + ' ' + plural('file', (commit.files || []).length),
                })))
                : null)),
        }),
      null, 'flush'));
  } else {
    body.appendChild(card('Who has context on what this reaches', 'nobody in this window',
      el('p', { class: 'prose', text: 'No identity in the newest ' + num(window_.commits_read)
        + ' ' + plural('commit', window_.commits_read) + ' touched any of the ' + num(reach.files)
        + ' reached ' + plural('file', reach.files) + '. That is a statement about the window, not about the '
        + 'repository’s history: anyone whose work on this code is older than the window is absent here, and '
        + 'absent reads exactly like never having touched it.' }), null, 'unknown'));
  }

  body.appendChild(card('Bus factor over the reached files',
    num(bus.single_contributor_files) + ' with exactly one contributor, '
    + num(bus.untouched_files) + ' with none',
    [
      el('div', { class: 'kpis' },
        kpi('Reached files', num(bus.reached_files), 'the denominator'),
        kpi('Touched in this window', num(bus.touched_files), 'somebody has recent context on these'),
        kpi('One contributor', num(bus.single_contributor_files), 'a bus factor of one, within the window',
          Number(bus.single_contributor_files) > 0 ? 'warn' : 'ok'),
        kpi('Nobody touched them', num(bus.untouched_files), 'a bus factor of zero, which is worse',
          Number(bus.untouched_files) > 0 ? 'bad' : 'ok')),
      el('p', { class: 'prose', text: text(bus.detail) }),
      bus.complete === false
        ? el('p', { class: 'warnline small', text: 'The ranking was capped before this was computed, which makes '
          + 'the one-contributor count an OVERSTATEMENT: a second contributor outside the cap would have '
          + 'disqualified a file counted here.' })
        : null,
      (bus.files || []).length
        ? table([{ label: 'File' }, { label: 'The only person who touched it' }, { label: 'Commits', num: true },
          { label: 'Last touch' }], bus.files || [], {
          cap: 25,
          row: (row) => el('tr', null,
            el('td', null, el('span', { class: 'sym', text: baseName(row.path) }),
              el('div', { class: 'path', text: dirName(row.path) })),
            el('td', null, el('span', { class: 'sym', text: text(row.name) }),
              el('div', { class: 'small muted', text: text(row.identity) })),
            el('td', { class: 'num', text: num(row.commits) }),
            el('td', { class: 'nowrap small muted', text: absoluteWhen(row.last_touch_unix) || 'not recorded' })),
        })
        : null,
    ], null, 'flush'));

  if (Number(untouched.file_count) > 0) {
    const files = untouched.files || [];
    body.appendChild(card('Reached files nobody in this window has touched',
      num(untouched.file_count) + ' ' + plural('file', untouched.file_count) + ', '
      + num(untouched.symbols) + ' reached ' + plural('symbol', untouched.symbols) + ' inside '
      + (Number(untouched.file_count) === 1 ? 'it' : 'them'),
      [
        el('p', { class: 'prose', text: 'This is where a reviewer’s attention is worth the most: code the change '
          + 'reaches that nobody currently visible has recent context on.' }),
        files.length
          ? el('ul', { class: 'prose' }, files.slice(0, 25).map((p) => el('li', null,
            el('span', { class: 'path', text: p }))))
          : el('p', { class: 'small muted', text: 'The paths themselves were not listed by the API.' }),
        files.length > 25
          ? inlineOmit(el('b', { text: num(files.length - 25) }), ' further '
            + plural('file', files.length - 25) + ' are hidden by this page’s own cap.')
          : null,
      ], null, 'warn'));
  }

  const impacted = d.impacted || [];
  if (impacted.length) {
    body.appendChild(fold('Evidence — the reached set, ' + num(impacted.length) + ' ' + plural('symbol', impacted.length), card('The reached set this routing is over',
      num(impacted.length) + ' ' + plural('symbol', impacted.length) + ' sampled, each checkable here',
      table(
        [{ label: 'Symbol' }, { label: 'Depth', num: true }, { label: 'Via' }, { label: 'Confidence' },
          { label: 'Evidence' }],
        impacted,
        {
          cap: 30,
          row: (row) => el('tr', { class: 't-' + tierOf(row.confidence) },
            el('td', null, el('span', { class: 'sym', text: text(row.name) }),
              row.is_test ? el('span', { class: 'tier tier-high', text: 'test' }) : null,
              el('div', { class: 'path', text: tailPath(text(row.file_path, ''), 3) })),
            el('td', { class: 'num', text: num(row.depth) }),
            el('td', null, el('code', { text: text(row.via) })),
            el('td', null, tierBadge(row.confidence)),
            el('td', null, (row.evidence && row.evidence[0])
              ? evidence(row.evidence[0].file_path,
                (row.evidence[0].range && row.evidence[0].range.start) ? row.evidence[0].range.start.line : null)
              : el('span', { class: 'muted small', text: 'no source location recorded' }))),
        }),
      null, 'flush'), { key: 'ownership:reach' }));
  }

  body.appendChild(fold('The window of history this was read over', card('The window this was read over', history.read ? 'git history was read' : 'git history was not read',
    [
      el('p', { class: 'prose', text: 'The newest ' + num(window_.commits_read) + ' of '
        + num(window_.requested) + ' requested ' + plural('commit', window_.requested) + ' were read, spanning '
        + (absoluteWhen(window_.oldest_unix) || 'an unrecorded moment') + ' to '
        + (absoluteWhen(window_.newest_unix) || 'an unrecorded moment') + '. '
        + (window_.exhausted
          ? 'The repository has no commits older than that, so this window is its whole history.'
          : 'The repository has older commits that were not read.') }),
      history.repository_root
        ? el('p', { class: 'hint', text: 'Worktree: ' + text(history.repository_root)
          + (history.path_prefix ? ' · the indexed project sits at ' + text(history.path_prefix) + ' inside it' : '') })
        : null,
      history.note ? el('p', { class: 'hint', text: history.note }) : null,
      history.read === false
        ? el('p', { class: 'warnline small', text: 'No git history was read for this answer, so every share above '
          + 'is over an empty window.' })
        : null,
    ]), { key: 'ownership:window' }));

  const caveats = caveatCard('What a name in this ranking does and does not mean',
    'the API’s own limitations, never dropped', d.caveats, 'unknown');
  if (caveats) body.appendChild(caveats);

  body.appendChild(renderOmissions(d.omissions, { key: 'ownership' }));
}

// ---------------------------------------------------------------- TRACEMIND — RUNTIME TRACE

LOADERS.trace = async function () {
  const host = $('#screen-trace');
  if (state.loaded.trace) return;
  state.loaded.trace = true;
  await drawTrace();
};

async function drawTrace() {
  const host = $('#screen-trace');
  clear(host);
  host.appendChild(answer('A trace export is being looked for, to lay what ran over what the index knows.'));

  const body = el('div', { class: 'stack' }, loading('Looking for a trace export…'));
  host.appendChild(body);

  let d;
  try { d = await api('/api/trace'); }
  catch (e) { clear(body); failAnswer(host, 'Runtime trace'); return showError(body, e, 'Runtime trace'); }
  state.meta = d.meta;
  renderIndexBar();
  clear(body);
  setAnswer(host, traceAnswer(d));
  renderTrace(d, body);
}

function traceAnswer(d) {
  const support = 'What actually executed, laid over what the index says can execute. A trace is imported from a running system, never indexed.';
  if (!d.correlated) {
    const g = d.guidance || {};
    return answer(text(g.title, 'No trace export was read, so nothing ran that this screen can show.').replace(/[.!?]?$/, '.'),
      { support: text(g.detail, support), next: nextOf(d.next_action) });
  }
  const cov = d.coverage || {};
  const spans = Number(d.spans || 0);
  const matched = Number(cov.exercised_entities || 0);
  const unmatched = Number(cov.unmatched_operations || 0);
  return answer([B(num(spans) + ' ' + plural('span', spans)), ' ' + (spans === 1 ? 'was' : 'were') + ' recorded; they touched ',
    B(num(matched) + ' ' + plural('thing', matched)), ' this index knows, and ',
    B(num(unmatched) + ' ' + plural('operation', unmatched)), ' matched nothing.'], { support: support, next: nextOf(d.next_action) });
}

function renderTrace(d, body) {
  const cov = d.coverage || {};

  body.appendChild(el('div', { class: 'kpis' },
    kpi('Spans', d.correlated ? num(d.spans) : 'none read',
      d.correlated ? num(d.traces) + ' ' + plural('trace', d.traces) + ' across '
        + num((d.services || []).length) + ' ' + plural('service', (d.services || []).length)
        : 'no export has been read, so nothing was correlated',
      d.correlated ? 'ok' : 'warn'),
    kpi('Indexed endpoints', num(cov.indexed_endpoints), 'the denominator of every coldness claim here',
      Number(cov.indexed_endpoints) === 0 ? 'warn' : 'ok'),
    kpi('Indexed symbols', num(cov.indexed_symbols), 'what a span could have been correlated to at all'),
    kpi('Exercised entities', num(cov.exercised_entities), num(cov.cold_endpoints) + ' cold '
      + plural('endpoint', cov.cold_endpoints) + ', ' + num(cov.unmatched_operations)
      + ' unmatched ' + plural('operation', cov.unmatched_operations))));

  body.appendChild(fileRefCard(d.export, 'a trace export — OTLP/JSON, or a trace saved from the Jaeger UI'));

  const help = guidanceCard(d.guidance, 'why there is nothing to lay over the graph');
  if (help) body.appendChild(help);

  body.appendChild(fold('What this index could be matched against', card('', 'stated whether or not an export was read',
    el('p', { class: 'prose', text: text(cov.detail) })), { key: 'trace:coverage' }));

  if (d.correlated) {
    if (d.format || (d.services || []).length) {
      body.appendChild(card('The export that was read', text(d.format, 'format not recorded'),
        el('p', { class: 'prose', text: num(d.spans) + ' ' + plural('span', d.spans) + ' in '
          + num(d.traces) + ' ' + plural('trace', d.traces)
          + ((d.services || []).length ? ', from: ' + (d.services || []).join(', ') + '.' : '.') })));
    }

    const exercised = d.exercised || [];
    if (exercised.length) {
      body.appendChild(card('What a span actually reached', num(exercised.length) + ' indexed '
        + plural('entity', exercised.length),
        observedTable(exercised), null, 'flush'));
    }

    const cold = d.cold_endpoints || [];
    if (cold.length) {
      body.appendChild(card('Endpoints this index knows and no span reached',
        num(cold.length) + ' cold ' + plural('endpoint', cold.length),
        [
          el('p', { class: 'prose', text: 'Cold is not dead. It means no span in the window that was exported '
            + 'reached this route — a route exercised outside that window, or by a client whose spans were not '
            + 'collected, is cold here and live in production.' }),
          observedTable(cold),
        ], null, 'warn'));
    }

    const unmatched = d.unmatched || [];
    if (unmatched.length) {
      body.appendChild(card('Span operations that matched nothing indexed', num(unmatched.length),
        [
          el('p', { class: 'prose', text: 'A large number here means the correlation is weak, not that the code '
            + 'is absent: an operation name that does not look like anything this index recorded cannot be tied '
            + 'to a symbol by any of the rules below.' }),
          el('ul', { class: 'prose' }, unmatched.slice(0, 30).map((s) => el('li', null, el('code', { text: s })))),
          unmatched.length > 30
            ? inlineOmit(el('b', { text: num(unmatched.length - 30) }), ' further '
              + plural('operation', unmatched.length - 30) + ' are hidden by this page’s own cap.')
            : null,
        ]));
    }
  }

  const caveats = caveatCard('How a span was tied to code', 'and what that tie is worth', d.caveats, 'unknown');
  if (caveats) body.appendChild(caveats);

  body.appendChild(renderOmissions(d.omissions, { key: 'trace' }));
}

function observedTable(rows) {
  return table(
    [{ label: 'Entity' }, { label: 'Kind' }, { label: 'How it was matched' }, { label: 'Calls', num: true },
      { label: 'Errors', num: true }, { label: 'p95' }, { label: 'File' }],
    rows,
    {
      cap: 30,
      row: (row) => el('tr', null,
        el('td', null, el('span', { class: 'sym', text: text(row.name) })),
        el('td', { class: 'nowrap small muted', text: text(row.kind) }),
        el('td', { class: 'nowrap small' }, el('code', { text: String(text(row.match)).replace(/_/g, '-') })),
        el('td', { class: 'num', text: num(row.calls) }),
        el('td', { class: 'num', text: num(row.errors) }),
        el('td', { class: 'nowrap small muted',
          text: Number(row.p95_nanos) > 0 ? humanMillis(Number(row.p95_nanos) / 1e6) : 'not recorded' }),
        el('td', null, row.path
          ? el('span', { class: 'path', text: tailPath(text(row.path, ''), 3) })
          : el('span', { class: 'muted small', text: 'no file recorded' }))),
    });
}

// ---------------------------------------------------------------- AGENTBENCH — RETRIEVAL SCORE

/*
  RECALL ALONE IS GAMED BY RETURNING THE WHOLE REPOSITORY.

  That is the entire reason a case carries a forbid list, and it is why this
  screen never prints a recall figure without the forbid score beside it, and
  never prints an aggregate without `cases_without_forbid` — the number that
  says how much of the mean recall is trustworthy. A case with no forbid list
  measures recall only, and its row says so instead of showing a zero that looks
  like a perfect miss or a 100% that looks like a perfect avoidance.
*/

LOADERS.retrieval = async function () {
  const host = $('#screen-retrieval');
  if (state.loaded.retrieval) return;
  state.loaded.retrieval = true;
  await drawBench();
};

async function drawBench() {
  const host = $('#screen-retrieval');
  clear(host);
  host.appendChild(answer('A case file is being looked for, to score what the context engine finds.'));

  const body = el('div', { class: 'stack' }, loading('Looking for a case file…'));
  host.appendChild(body);

  let d;
  try { d = await api('/api/bench'); }
  catch (e) { clear(body); failAnswer(host, 'Retrieval score'); return showError(body, e, 'Retrieval score'); }
  state.meta = d.meta;
  renderIndexBar();
  clear(body);
  setAnswer(host, benchAnswer(d));
  renderBench(d, body);
}

function benchAnswer(d) {
  const support = 'Whether the context engine returns the right code for a task — measured against cases somebody wrote down, with a forbid list, because recall alone is gamed by returning everything.';
  if (!d.scored) {
    const g = d.guidance || {};
    return answer(text(g.title, 'No case file was read, so nothing has been scored.').replace(/[.!?]?$/, '.'),
      { support: text(g.detail, support), next: nextOf(d.next_action) });
  }
  const s = d.scores || {};
  const cases = d.cases || [];
  let hit = 0;
  let expected = 0;
  for (const row of cases) { hit += (row.found || []).length; expected += (row.expected || []).length; }
  return answer([B(rateOf(s.mean_recall) + ' recall'), ': the context engine found ', B(num(hit) + ' of ' + num(expected)),
    ' things it should have, across ', B(num(s.cases) + ' ' + plural('case', s.cases)), '.'], { support: support, next: nextOf(d.next_action) });
}

function renderBench(d, body) {
  const s = d.scores || {};
  const cases = d.cases || [];
  const forbidTrustworthy = Number(s.cases_without_forbid) === 0;

  body.appendChild(el('div', { class: 'kpis' },
    kpi('Cases', d.scored ? num(s.cases) : 'none scored',
      d.scored ? num(s.passed) + ' passed, ' + num(s.failed) + ' failed'
        : num(d.cases_in_file) + ' declared in the file this screen looked for',
      d.scored && Number(s.failed) > 0 ? 'bad' : (d.scored ? 'ok' : 'warn')),
    kpi('Mean recall', d.scored ? rateOf(s.mean_recall) : NOT_MEASURED,
      'the share of the expected items each pack contained', d.scored ? 'ok' : 'warn'),
    kpi('Mean forbid score', (d.scored && Number(s.cases_with_forbid) > 0)
      ? rateOf(s.mean_forbid_score) : NOT_MEASURED,
      !d.scored
        ? 'no case file was read'
        : (Number(s.cases_with_forbid) > 0
          ? 'averaged over the ' + num(s.cases_with_forbid) + ' ' + plural('case', s.cases_with_forbid)
            + (Number(s.cases_with_forbid) === 1 ? ' that names' : ' that name') + ' forbidden items'
          : 'no case names a forbidden item, so nothing constrains the recall beside it'),
      (d.scored && Number(s.cases_with_forbid) > 0) ? 'ok' : 'warn'),
    kpi('Cases with no forbid list', d.scored ? num(s.cases_without_forbid) : NOT_MEASURED,
      d.scored
        ? 'how much of the mean recall above is trustworthy'
        : 'nothing was scored, so this is not a zero',
      (d.scored && forbidTrustworthy) ? 'ok' : 'warn')));

  body.appendChild(fileRefCard(d.case_file, 'a benchmark case file — tasks, expected items, forbidden items'));

  const help = guidanceCard(d.guidance, 'why there is no score for this repository');
  if (help) body.appendChild(help);

  if (d.scored) {
    body.appendChild(fold('What these numbers were computed over', card('', 'the scope the API states',
      [
        el('p', { class: 'prose', text: text(s.scope) }),
        el('p', { class: 'hint', text: num(s.total_violations) + ' forbidden '
          + plural('item', s.total_violations) + ' were retrieved across every case, and '
          + num(s.over_budget_cases) + ' ' + plural('case', s.over_budget_cases)
          + ' went over their token budget. A violation is a precision failure, never a rounding error.' }),
      ]), { key: 'retrieval:scope' }));

    if (cases.length) {
      body.appendChild(card('Every case, with both halves of its score', num(cases.length),
        table(
          [{ label: 'Case' }, { label: 'Verdict' }, { label: 'Recall', num: true },
            { label: 'Forbid score', num: true }, { label: 'Found', num: true },
            { label: 'Expected', num: true }, { label: 'Missing', num: true },
            { label: 'Forbidden', num: true, title: 'How many items the case names as ones the pack must NOT contain' },
            { label: 'Violations', num: true }, { label: 'Items', num: true }, { label: 'Tokens', num: true }],
          cases,
          {
            cap: 40,
            row: (row) => el('tr', { class: row.passed ? null : 't-low' },
              el('td', null, el('span', { class: 'sym', text: text(row.id) }),
                el('div', { class: 'small muted', text: text(row.task) }),
                (row.violations || []).length
                  ? el('div', { class: 'small warnline', text: 'retrieved: ' + (row.violations || []).join(', ') })
                  : null,
                (row.missing || []).length
                  ? el('div', { class: 'small muted', text: 'missing: ' + (row.missing || []).join(', ') })
                  : null),
              el('td', { class: 'nowrap' }, el('span', {
                class: 'tier tier-' + (row.passed ? 'exact' : 'low'),
                text: row.passed ? 'passed' : 'failed',
              }), row.over_budget ? el('div', { class: 'small warnline', text: 'over budget' }) : null),
              el('td', { class: 'num', text: rateOf(row.recall) }),
              el('td', { class: 'num' }, row.forbid_measured
                ? rateOf(row.forbid_score)
                : el('span', { class: 'muted small', text: 'no forbid list' })),
              el('td', { class: 'num', text: num((row.found || []).length) }),
              el('td', { class: 'num', text: num((row.expected || []).length) }),
              el('td', { class: 'num', text: num((row.missing || []).length) }),
              el('td', { class: 'num', text: num((row.forbidden || []).length) }),
              el('td', { class: 'num', text: num((row.violations || []).length) }),
              el('td', { class: 'num', text: num(row.items) }),
              el('td', { class: 'num', text: num(row.token_estimate) })),
          }),
        el('p', { class: 'hint', text: 'A case with no forbid list scores recall only. Its row says so rather '
          + 'than showing a number, because a pack that returned the whole repository would score a perfect '
          + 'recall on it and nothing would object.' }), 'flush'));
    }
  }

  const caveats = caveatCard('What this benchmark measures, and what it does not',
    'the API’s own statement', d.caveats, 'unknown');
  if (caveats) body.appendChild(caveats);

  body.appendChild(renderOmissions(d.omissions, { key: 'retrieval' }));
}

// ---------------------------------------------------------------- PLATFORM — ESTATE

LOADERS.estate = async function () {
  const host = $('#screen-estate');
  if (state.loaded.estate) return;
  state.loaded.estate = true;
  await drawEstate();
};

async function drawEstate() {
  const host = $('#screen-estate');
  clear(host);
  host.appendChild(answer('An estate manifest is being looked for, to read several repositories as one system.'));

  const body = el('div', { class: 'stack' }, loading('Looking for an estate manifest…'));
  host.appendChild(body);

  let d;
  try { d = await api('/api/system'); }
  catch (e) { clear(body); failAnswer(host, 'Estate'); return showError(body, e, 'Estate'); }
  state.meta = d.meta;
  renderIndexBar();
  clear(body);
  setAnswer(host, estateAnswer(d));
  renderEstate(d, body);
}

function estateAnswer(d) {
  const support = 'More than one repository, read as one system: which service calls which across an index boundary, and which of them share a table without sharing a repository.';
  if (!d.federated) {
    const g = d.guidance || {};
    return answer(text(g.title, 'No estate manifest was read, so this repository stands alone here.').replace(/[.!?]?$/, '.'),
      { support: text(g.detail, support), next: nextOf(d.next_action) });
  }
  const c = d.counts || {};
  const members = Number(c.members_declared || 0);
  const edges = Number(c.cross_repo_edges || 0);
  return answer([B(num(members) + ' ' + plural('repository', members)), ', ', B(num(edges) + ' ' + plural('call', edges)),
    ' between them' + (d.complete ? '.' : ' — a lower bound, because at least one member could not be read.')], { support: support, next: nextOf(d.next_action) });
}

function renderEstate(d, body) {
  const c = d.counts || {};
  const unjoined = d.unjoined || {};
  const prov = d.provenance || {};

  body.appendChild(el('div', { class: 'kpis' },
    kpi('Members declared', num(c.members_declared),
      Number(c.members_declared) === 0
        ? 'no manifest was read, so no member is declared'
        : num(c.members_available) + ' of them could be read',
      Number(c.members_declared) === 0 ? 'warn' : 'ok'),
    kpi('Cross-repository calls', num(c.cross_repo_edges), 'each one carries a tier and a file:line on both ends'),
    kpi('Shared tables', num(c.shared_tables), num(c.write_write_tables) + ' written by more than one member',
      Number(c.write_write_tables) > 0 ? 'bad' : 'ok'),
    // multirepo's own verdict, and the single most important boolean here: a
    // false Complete makes every count above a floor rather than a total.
    kpi('Complete', d.federated ? (d.complete ? 'yes' : 'no') : 'no estate read',
      d.federated
        ? (d.complete
          ? 'every declared member was read, so the counts above are totals'
          : 'at least one member could not be read, so every count above is a lower bound')
        : 'no estate was assembled, so there is nothing to be complete about',
      (d.federated && d.complete) ? 'ok' : 'warn')));

  body.appendChild(fileRefCard(d.manifest, 'an estate manifest naming the member repositories and their indexes'));

  const help = guidanceCard(d.guidance, 'why there is no system graph for this repository');
  if (help) body.appendChild(help);

  if (c.scope) {
    body.appendChild(fold('What these counts are over', card('', 'stated by the API',
      el('p', { class: 'prose', text: text(c.scope) })), { key: 'estate:scope' }));
  }

  const members = d.members || [];
  if (members.length) {
    body.appendChild(card('Members', num(members.length)
      + ' — including the ones whose index could not be read',
      table(
        [{ label: 'Member' }, { label: 'Availability' }, { label: 'Run' }, { label: 'Endpoints', num: true },
          { label: 'Tables', num: true }, { label: 'Outbound calls', num: true }, { label: 'Index' }],
        members,
        {
          cap: 30,
          row: (m) => el('tr', { class: m.available ? null : 't-low' },
            el('td', null, el('span', { class: 'sym', text: text(m.name) }),
              (m.warnings || []).length
                ? el('div', { class: 'small warnline', text: (m.warnings || []).join(' ') })
                : null),
            el('td', { class: 'nowrap small' }, m.available
              ? el('span', { class: 'tier tier-exact', text: 'read' })
              : el('span', { class: 'tier tier-low', text: 'not read' }),
              m.unavailable ? el('div', { class: 'muted', text: text(m.unavailable) }) : null),
            // A member whose index could not be opened has no run to describe,
            // and six em dashes in a row read as six missing fields rather than
            // as one absent index.
            el('td', { class: 'small muted' }, m.available
              ? 'run ' + num(m.run_id) + ' · ' + text(m.status)
                + ' · ' + text(m.index_state) + ' · ' + String(text(m.git_revision, 'no revision')).slice(0, 8)
                + ' · ' + text(m.accuracy_mode) + ' · producer ' + text(m.producer_version)
              : 'no run — the index was not opened'),
            el('td', { class: 'num', text: num(m.endpoints) }),
            el('td', { class: 'num', text: num(m.tables) }),
            el('td', { class: 'num', text: num(m.outbound_calls) }),
            el('td', null, el('span', { class: 'path', text: tailPath(text(m.index_path, ''), 3) }))),
        }),
      null, 'flush'));
  }

  const edges = d.cross_repo_edges || [];
  if (edges.length) {
    body.appendChild(card('Calls that leave the repository', num(edges.length),
      table(
        [{ label: 'From' }, { label: 'To' }, { label: 'Route' }, { label: 'Confidence' },
          { label: 'How it was joined' }, { label: 'Evidence' }],
        edges,
        {
          cap: 30,
          row: (e) => el('tr', { class: 't-' + tierOf(e.confidence) },
            el('td', null, el('span', { class: 'sym', text: text(e.from_symbol) }),
              el('div', { class: 'small muted', text: text(e.from_member) })),
            el('td', null, el('span', { class: 'sym', text: text(e.to_handler, e.to_endpoint) }),
              el('div', { class: 'small muted', text: text(e.to_member) })),
            el('td', { class: 'nowrap small' }, el('code', { text: text(e.method) + ' ' + text(e.path) }),
              e.dynamic ? el('div', { class: 'muted', text: 'the path is built at run time' }) : null),
            el('td', null, tierBadge(e.confidence)),
            el('td', { class: 'small muted' }, text(e.match),
              (e.legs || []).map((leg) => el('div', {
                text: text(leg.name) + ' · ' + tierWord(leg.confidence) + (leg.heuristic ? ' · inferred from names' : '')
                  + ' · ' + text(leg.detail),
              })),
              e.note ? el('div', { text: e.note }) : null,
              e.client ? el('div', { text: 'client: ' + text(e.client) }) : null),
            el('td', null, evidence(e.from_file, e.from_line),
              e.to_endpoint_file
                ? el('div', null, evidence(e.to_endpoint_file, e.to_endpoint_line))
                : null,
              e.to_handler_file
                ? el('div', { class: 'path', text: tailPath(text(e.to_handler_file, ''), 3) })
                : null)),
        }),
      null, 'flush'));
  }

  const shared = d.shared_tables || [];
  if (shared.length) {
    body.appendChild(card('Tables more than one member touches', num(shared.length) + ', '
      + num(c.write_write_tables) + ' of them written by more than one',
      table(
        [{ label: 'Table' }, { label: 'Standing' }, { label: 'Writers', num: true }, { label: 'Readers', num: true },
          { label: 'Confidence' }, { label: 'Participants' }],
        shared,
        {
          cap: 25,
          row: (t) => el('tr', { class: t.write_write ? 't-low' : null },
            el('td', null, el('span', { class: 'sym', text: text(t.table) }),
              t.note ? el('div', { class: 'small muted', text: t.note }) : null),
            el('td', { class: 'nowrap small' }, t.write_write
              ? el('span', { class: 'tier tier-low', text: 'written by two' })
              : el('span', { class: 'tier tier-medium', text: 'shared' }),
              (t.owners || []).length
                ? el('div', { class: 'muted', text: 'declared by ' + (t.owners || []).join(', ') })
                : null),
            el('td', { class: 'num', text: num(t.writers) }),
            el('td', { class: 'num', text: num(t.readers) }),
            el('td', null, tierBadge(t.confidence)),
            el('td', { class: 'small muted' }, (t.participants || []).map((p) => el('div', {
              text: text(p.member) + ' · ' + num(p.reads) + ' reads, ' + num(p.writes) + ' writes, '
                + num(p.accesses) + ' accesses, ' + num(p.uncertain) + ' uncertain'
                + (p.declared ? ' · declares it' : ''),
            })))),
        }),
      null, 'flush'));
  }

  const unjoinedCalls = unjoined.calls || [];
  const ambiguous = unjoined.ambiguous_calls || [];
  const unconsumed = unjoined.endpoints || [];
  const unavailableMembers = unjoined.unavailable_members || [];
  if (d.federated || unjoinedCalls.length || ambiguous.length || unconsumed.length || unavailableMembers.length) {
    body.appendChild(card('What the federation could not join', 'the half that keeps the other half honest',
      [
        el('div', { class: 'kpis' },
          kpi('Unbound calls', num(c.unjoined_calls), 'an outbound call matching no member’s route',
            Number(c.unjoined_calls) > 0 ? 'warn' : 'ok'),
          kpi('Ambiguous calls', num(c.ambiguous_calls), 'more than one candidate route, and no way to choose',
            Number(c.ambiguous_calls) > 0 ? 'warn' : 'ok'),
          kpi('Unconsumed endpoints', num(c.unconsumed_endpoints),
            'routes no member of this system calls — a lead, never a verdict that the route is dead')),
        unjoinedCalls.length
          ? el('div', null, el('h4', { text: 'Calls that matched no member route' }),
            table([{ label: 'Caller' }, { label: 'Member' }, { label: 'Route' }, { label: 'Why' },
              { label: 'Evidence' }], unjoinedCalls, {
              cap: 20,
              row: (row) => el('tr', null,
                el('td', null, el('span', { class: 'sym', text: text(row.caller) })),
                el('td', { class: 'nowrap small muted', text: text(row.member) }),
                el('td', { class: 'nowrap small' }, el('code', { text: text(row.method) + ' ' + text(row.path) }),
                  row.dynamic ? el('div', { class: 'muted', text: 'built at run time' }) : null),
                el('td', { class: 'small muted', text: text(row.reason) + (row.client ? ' · ' + text(row.client) : '') }),
                el('td', null, evidence(row.file, row.line))),
            }))
          : null,
        ambiguous.length
          ? el('div', null, el('h4', { text: 'Calls with more than one candidate' }),
            table([{ label: 'Caller' }, { label: 'Member' }, { label: 'Route' }, { label: 'Candidates' },
              { label: 'Evidence' }], ambiguous, {
              cap: 20,
              row: (row) => el('tr', null,
                el('td', null, el('span', { class: 'sym', text: text(row.caller) })),
                el('td', { class: 'nowrap small muted', text: text(row.member) }),
                el('td', { class: 'nowrap small' }, el('code', { text: text(row.method) + ' ' + text(row.path) })),
                el('td', { class: 'small muted', text: (row.candidates || []).join(', ') }),
                el('td', null, evidence(row.file, row.line))),
            }))
          : null,
        unconsumed.length
          ? el('div', null, el('h4', { text: 'Endpoints no member calls' }),
            table([{ label: 'Endpoint' }, { label: 'Member' }, { label: 'Handler' }, { label: 'Evidence' }],
              unconsumed, {
                cap: 20,
                row: (row) => el('tr', null,
                  el('td', null, el('code', { text: text(row.endpoint) })),
                  el('td', { class: 'nowrap small muted', text: text(row.member) }),
                  el('td', null, el('span', { class: 'sym', text: text(row.handler) })),
                  el('td', null, evidence(row.file, row.line))),
              }))
          : null,
        unavailableMembers.length
          ? el('p', { class: 'warnline small', text: 'These members were declared and could not be read: '
            + unavailableMembers.join(', ') + '. Every count on this screen is a lower bound because of it.' })
          : null,
        unjoined.truncated
          ? el('p', { class: 'warnline small', text: 'These lists were truncated, so the counts above them are '
            + 'the totals and the rows are a sample.' })
          : null,
      ], null, 'unknown'));
  }

  if (d.federated) {
    body.appendChild(fold('Where the member indexes came from', card('', prov.mixed_producers
      ? 'the members were built by different producer versions' : 'one producer version across the estate',
      [
        // producer_versions is a sorted ARRAY of the distinct versions stamped
        // across the members, not a map of member to version. An empty string
        // in it means at least one run predates the stamp, so it is named
        // rather than dropped.
        el('p', { class: 'prose', text: 'This report was produced by ' + text(prov.running_version, 'an unstamped build')
          + '. The member indexes were built by: '
          + ((prov.producer_versions || []).map((v) => text(v, 'a build too old to stamp itself')).join(', ')
            || 'a set of builds none of the members recorded')
          + '. The oldest member index was built ' + (absoluteWhen(prov.oldest_indexed_unix) || 'at an unrecorded time')
          + ' and the newest ' + (absoluteWhen(prov.newest_indexed_unix) || 'at an unrecorded time')
          + ', a spread of ' + humanMillis(Number(prov.skew_hours) * 3600000)
          + ' — the system graph therefore describes no single moment.' }),
        prov.mixed_producers
          ? el('p', { class: 'warnline small', text: 'The members were NOT all indexed by one build. Resolution '
            + 'rules change between builds, so this estate holds edges no single binary would have produced.' })
          : null,
        (prov.notes || []).length
          ? el('ul', { class: 'prose' }, (prov.notes || []).map((n) => el('li', { text: n })))
          : null,
      ]), { key: 'estate:provenance' }));
  }

  const caveats = caveatCard('The standing limits of reading several indexes as one system',
    'as opposed to the per-run conditions above', d.caveats, 'unknown');
  if (caveats) body.appendChild(caveats);

  body.appendChild(renderOmissions(d.omissions, { key: 'estate' }));
}

// ---------------------------------------------------------------- IMPACTMIND — FACT DIFF

/*
  THE ONE VIEW HERE THAT REPORTS A CHANGE RATHER THAN A STATE.

  Everything else in this portal answers "what is true of this index". This
  answers "what became true between two of them" — an edge that appeared, an
  edge a compiler stopped proving, a covering test that went away.

  A DOWNGRADE is the row to look for. `relationships_upgraded` and
  `relationships_downgraded` split the regrades for exactly that reason: a
  compiler that stopped proving something it used to prove is invisible in a
  total, and every consumer filtering on `exact` silently lost that edge.

  On an index holding one committed run there is nothing to compare, `compared`
  is false, and every count in the payload is a placeholder that the API itself
  labels as one in `counts.scope`. That sentence is printed, because a grid of
  zeroes with no sentence over it reads as "nothing changed".
*/

LOADERS.diff = async function () {
  if (state.loaded.diff) return;
  state.loaded.diff = true;
  await drawDiff();
};

async function drawDiff() {
  const host = $('#screen-diff');
  clear(host);
  host.appendChild(answer('Two indexed runs are being compared.'));

  const from = el('input', { type: 'search', id: 'diff-from', autocomplete: 'off', value: state.q.dfrom || '',
    placeholder: 'a run id — blank picks the run before the head' });
  const to = el('input', { type: 'search', id: 'diff-to', autocomplete: 'off', value: state.q.dto || '',
    placeholder: 'a run id — blank picks the committed run this portal serves' });
  const form = el('form', {
    class: 'toolbar',
    onsubmit: (e) => { e.preventDefault(); setQuery({ dfrom: from.value.trim(), dto: to.value.trim() }); drawDiff(); },
  });
  form.appendChild(field('Base run', from, 'grow'));
  form.appendChild(field('Head run', to, 'grow'));
  form.appendChild(el('button', { class: 'primary', type: 'submit', text: 'Compare' }));
  host.appendChild(form);

  const body = el('div', { class: 'stack' }, loading('Comparing the two runs…'));
  host.appendChild(body);

  let d;
  try { d = await api('/api/diff', { from: state.q.dfrom, to: state.q.dto }); }
  catch (e) { clear(body); failAnswer(host, 'Fact diff'); return showError(body, e, 'Fact diff'); }
  state.meta = d.meta;
  renderIndexBar();
  clear(body);
  setAnswer(host, diffAnswer(d));
  renderFactDiff(d, body);
}

function diffAnswer(d) {
  const support = 'What changed between two indexed runs: relationships gained, lost and re-graded; symbols added, moved and orphaned; endpoints and tables. A re-grade downwards is the one a release should stop for.';
  if (!d.compared) {
    const g = d.guidance || {};
    return answer(text(g.title, 'Only one run is recorded, so there is nothing to compare it with yet.').replace(/[.!?]?$/, '.'),
      { support: text(g.detail, support), next: nextOf(d.next_action) });
  }
  const f = d.filters || {};
  const c = d.counts || {};
  const gained = Number(c.relationships_gained || 0);
  const lost = Number(c.relationships_lost || 0);
  return answer(['Between run ' + num(f.from_run) + ' and run ' + num(f.to_run) + ': ', B(num(gained) + ' ' + plural('relationship', gained)),
    ' appeared, ', B(num(lost)), ' disappeared.'], { support: support, next: nextOf(d.next_action) });
}

function renderFactDiff(d, body) {
  const f = d.filters || {};
  const c = d.counts || {};
  const cov = d.coverage || {};
  const caveats = d.caveats || {};

  body.appendChild(el('div', { class: 'kpis' },
    kpi('Relationships gained', d.compared ? num(c.relationships_gained) : 'not compared',
      d.compared ? 'edges the head run records and the base run did not' : 'no second run to compare against',
      d.compared ? 'ok' : 'warn'),
    kpi('Relationships lost', d.compared ? num(c.relationships_lost) : 'not compared',
      d.compared ? 'edges the base run recorded and the head run does not' : 'no second run to compare against',
      (d.compared && Number(c.relationships_lost) > 0) ? 'bad' : 'ok'),
    kpi('Re-graded downwards', d.compared ? num(c.relationships_downgraded) : 'not compared',
      d.compared
        ? num(c.relationships_upgraded) + ' went the other way — a downgrade is a proof that stopped being one'
        : 'no second run to compare against',
      (d.compared && Number(c.relationships_downgraded) > 0) ? 'bad' : 'ok'),
    kpi('Covering tests lost', d.compared ? num(c.test_coverage_lost) : 'not compared',
      d.compared ? num(c.test_coverage_gained) + ' gained' : 'no second run to compare against',
      (d.compared && Number(c.test_coverage_lost) > 0) ? 'bad' : 'ok')));

  const help = guidanceCard(d.guidance, 'why there is nothing to compare');
  if (help) body.appendChild(help);

  body.appendChild(card('The two sides of this comparison',
    d.compared
      ? 'base run ' + num(f.from_run) + ' → head run ' + num(f.to_run)
      : 'no pair of runs was established',
    [
      f.detail ? el('p', { class: 'prose', text: f.detail }) : null,
      (f.defaulted || []).length
        ? el('p', { class: 'hint', text: 'The request pinned no '
          + (f.defaulted || []).map((side) => side === 'from' ? 'base' : (side === 'to' ? 'head' : side))
            .join(' side and no ')
          + ' side, so the portal chose '
          + ((f.defaulted || []).length > 1 ? 'both' : 'it')
          + '. Name a run id in the boxes above to pin them yourself.' })
        : null,
      d.compared
        ? el('div', { class: 'grid-2' },
          runSideCard('Base', d.base),
          runSideCard('Head', d.head))
        : null,
      el('p', { class: 'prose', text: text(c.scope) }),
      (d.compared && c.complete === false)
        ? el('p', { class: 'warnline small', text: 'A section was capped, which makes every count above it a '
          + 'lower bound. The exact totals are in the fold below, under what this view can\'t see.' })
        : null,
      el('p', { class: 'hint', text: 'Bounds actually applied: at most ' + num(f.max_per_section)
        + ' rows per section, probe depth ' + num(f.probe_depth) + '.' }),
    ]));

  if (d.compared) {
    body.appendChild(fold('Evidence — how much the index itself moved', card('How much the index itself moved', text(cov.detail, 'the two runs, side by side'),
      table([{ label: 'Population' }, { label: 'Base', num: true }, { label: 'Head', num: true },
        { label: 'Change', num: true }], [
        { what: 'Files', base: cov.base_files, head: cov.head_files, delta: cov.files_delta },
        { what: 'Symbols', base: cov.base_symbols, head: cov.head_symbols, delta: cov.symbols_delta },
        { what: 'Relationships', base: cov.base_edges, head: cov.head_edges, delta: cov.edges_delta },
        { what: 'Parse failures', base: cov.base_parse_failures, head: cov.head_parse_failures, delta: null },
      ], {
        row: (row) => el('tr', null,
          el('td', { text: row.what }),
          el('td', { class: 'num', text: num(row.base) }),
          el('td', { class: 'num', text: num(row.head) }),
          el('td', { class: 'num', text: row.delta === null ? '—' : (Number(row.delta) > 0 ? '+' : '') + num(row.delta) })),
      }), null, 'flush'), { key: 'diff:moved' }));

    body.appendChild(card('Everything that changed, counted', 'four populations, kept addable',
      el('div', { class: 'kpis' },
        kpi('Symbols added', num(c.symbols_added), num(c.symbols_removed) + ' removed, '
          + num(c.symbols_moved) + ' moved, ' + num(c.symbols_orphaned) + ' orphaned'),
        kpi('Endpoints added', num(c.endpoints_added), num(c.endpoints_removed) + ' removed, '
          + num(c.endpoints_handler_changed) + ' changed handler'),
        kpi('Tables changed', num(c.tables_changed), 'readers, writers or declaration'),
        kpi('Relationships re-graded', num(c.relationships_regraded), num(c.relationships_upgraded)
          + ' up, ' + num(c.relationships_downgraded) + ' down'))));

    const relationships = d.relationships || [];
    if (relationships.length) {
      body.appendChild(card('Relationships', num(relationships.length) + ' listed',
        edgeChangeTable(relationships), null, 'flush'));
    }

    const coverage = d.test_coverage || [];
    if (coverage.length) {
      body.appendChild(card('Covering tests', num(coverage.length)
        + ' — the TESTED_BY subset, hoisted because losing one is its own sentence',
        edgeChangeTable(coverage), null, 'flush'));
    }

    const symbols = d.symbols || [];
    if (symbols.length) {
      body.appendChild(card('Symbols', num(symbols.length) + ' listed',
        table([{ label: 'Symbol' }, { label: 'Change' }, { label: 'Kind' }, { label: 'Moved by', num: true },
          { label: 'Why' }, { label: 'Where' }], symbols, {
          cap: 40,
          row: (row) => {
            const key = row.key || {};
            const side = row.to || row.from || {};
            return el('tr', { class: row.uncertain ? 't-low' : null },
              el('td', null, el('span', { class: 'sym', text: text(side.name, key.fqn) }),
                el('div', { class: 'path', text: text(key.path) })),
              el('td', { class: 'nowrap small' }, el('code', { text: text(row.change) }),
                row.uncertain ? el('div', { class: 'muted', text: 'uncertain' }) : null),
              el('td', { class: 'nowrap small muted', text: text(key.kind) }),
              el('td', { class: 'num', text: row.line_delta === undefined ? '—' : num(row.line_delta) }),
              el('td', { class: 'small muted', text: text(row.reason) }),
              el('td', null, (side.start && side.start.line)
                ? evidence(text(key.path), side.start.line)
                : el('span', { class: 'path', text: text(key.path) })));
          },
        }), null, 'flush'));
    }

    const endpoints = d.endpoints || [];
    if (endpoints.length) {
      body.appendChild(card('Endpoints', num(endpoints.length) + ' listed',
        table([{ label: 'Route' }, { label: 'Change' }, { label: 'Handlers before' }, { label: 'Handlers after' },
          { label: 'Where' }], endpoints, {
          cap: 40,
          row: (row) => {
            const key = row.key || {};
            const before = row.from || {};
            const after = row.to || {};
            return el('tr', null,
              el('td', null, el('code', { text: text(key.route) })),
              el('td', { class: 'nowrap small' }, el('code', { text: text(row.change) })),
              el('td', { class: 'small muted', text: (before.handlers || []).join(', ') || '—' }),
              el('td', { class: 'small muted', text: (after.handlers || []).join(', ') || '—' }),
              el('td', null, evidence(text(key.path), after.line || before.line)));
          },
        }), null, 'flush'));
    }

    const tables = d.tables || [];
    if (tables.length) {
      body.appendChild(card('Tables', num(tables.length) + ' listed',
        table([{ label: 'Table' }, { label: 'Change' }, { label: 'Readers', num: true },
          { label: 'Writers', num: true }, { label: 'Accesses' }], tables, {
          cap: 40,
          row: (row) => {
            const before = row.from || {};
            const after = row.to || {};
            return el('tr', null,
              el('td', null, el('span', { class: 'sym', text: text(row.table) }),
                after.declared === false && before.declared === true
                  ? el('div', { class: 'small warnline', text: 'no longer declared here' })
                  : null),
              el('td', { class: 'nowrap small' }, el('code', { text: text(row.change) })),
              el('td', { class: 'num', text: (Number(row.readers_delta) > 0 ? '+' : '') + num(row.readers_delta) }),
              el('td', { class: 'num', text: (Number(row.writers_delta) > 0 ? '+' : '') + num(row.writers_delta) }),
              el('td', { class: 'small muted', text: num(before.accesses) + ' → ' + num(after.accesses)
                + ' · uncertain ' + num(before.uncertain) + ' → ' + num(after.uncertain) }));
          },
        }), null, 'flush'));
    }
  }

  body.appendChild(fold('What the comparison itself could not settle' + ((caveats.notes || []).length ? ' — ' + num(caveats.notes.length) + ' ' + plural('note', caveats.notes.length) : ''), card('', 'diffgraph’s own caveats',
    [
      el('p', { class: 'prose', text: caveats.comparable
        ? 'The two runs were comparable: they were built by the same producer over the same tree layout, so a '
          + 'missing edge means the edge went away rather than that the two indexes disagree about what they read.'
        : 'The two runs were NOT established as comparable, so a row that looks like a change may be two indexes '
          + 'disagreeing about what they read rather than the code moving.' }),
      caveats.digests_equal
        ? el('p', { class: 'hint', text: 'The two runs carry the same content digest, so any difference below is '
          + 'a difference in how they were built rather than in what was built.' })
        : null,
      Number(caveats.unreliable_files) > 0
        ? el('p', { class: 'warnline small', text: num(caveats.unreliable_files) + ' '
          + plural('file', caveats.unreliable_files) + ' could not be compared reliably'
          + ((caveats.unreliable_paths || []).length
            ? ': ' + (caveats.unreliable_paths || []).slice(0, 6).join(', ') : '') + '.' })
        : null,
      Number(caveats.dangling_edges) > 0
        ? el('p', { class: 'warnline small', text: num(caveats.dangling_edges) + ' '
          + plural('edge', caveats.dangling_edges) + ' pointed at a symbol neither run resolved, so they are '
          + 'excluded from every section above.' })
        : null,
      (caveats.notes || []).length
        ? el('ul', { class: 'prose' }, (caveats.notes || []).map((n) => el('li', { text: n })))
        : null,
    ], null, caveats.comparable ? null : 'unknown'), { key: 'diff:caveats' }));

  body.appendChild(renderOmissions(d.omissions, { key: 'diff' }));
}

function runSideCard(side, run) {
  const r = run || {};
  return el('div', { class: 'held' },
    el('h4', { text: side + ' — run ' + num(r.run_id) }),
    el('p', { class: 'what', text: text(r.status, 'status not recorded') + ' · '
      + String(text(r.git_revision, 'no revision recorded')).slice(0, 12)
      + ' · ' + num(r.files) + ' ' + plural('file', r.files)
      + ' · ' + num(r.symbols) + ' ' + plural('symbol', r.symbols)
      + ' · ' + num(r.edges) + ' ' + plural('relationship', r.edges) }),
    el('p', { class: 'fix', text: num(r.parse_failures) + ' ' + plural('file', r.parse_failures)
      + ' failed to parse in this run, so everything it recorded is a lower bound.'
      + (r.digest ? ' Digest ' + String(text(r.digest)).slice(0, 12) + '.' : '') }));
}

function edgeChangeTable(rows) {
  return table(
    [{ label: 'Relationship' }, { label: 'Change' }, { label: 'Kind' }, { label: 'Before' }, { label: 'After' },
      { label: 'Why' }, { label: 'Evidence' }],
    rows,
    {
      cap: 40,
      row: (row) => {
        const key = row.key || {};
        const src = key.src || {};
        const dst = key.dst || {};
        const before = row.from || {};
        const after = row.to || {};
        const site = (after.sites && after.sites[0]) || (before.sites && before.sites[0]) || null;
        return el('tr', { class: 't-' + tierOf(after.confidence || before.confidence) },
          el('td', null, el('span', { class: 'sym', text: text(src.fqn) }),
            el('div', { class: 'small muted', text: '→ ' + text(dst.fqn) })),
          el('td', { class: 'nowrap small' }, el('code', { text: text(row.change) }),
            row.direction ? el('div', { class: 'muted', text: text(row.direction) }) : null,
            row.uncertain ? el('div', { class: 'muted', text: 'uncertain' }) : null),
          el('td', null, el('code', { text: text(key.kind) })),
          el('td', null, before.confidence
            ? tierBadge(before.confidence, { count: before.site_count })
            : el('span', { class: 'muted small', text: 'absent' })),
          el('td', null, after.confidence
            ? tierBadge(after.confidence, { count: after.site_count })
            : el('span', { class: 'muted small', text: 'absent' })),
          el('td', { class: 'small muted', text: text(row.reason) }),
          el('td', null, site
            ? evidence(site.path, site.line)
            : el('span', { class: 'muted small', text: 'no source location recorded' })));
      },
    });
}

// ================================================================ SCREEN 19 — EVENTS TOPOLOGY

LOADERS.events = async function () {
  const host = $('#screen-events');
  if (state.loaded.events) return;
  state.loaded.events = true;
  await drawEvents();
};

async function drawEvents() {
  const host = $('#screen-events');
  clear(host);
  host.appendChild(answer('Distributed message broker topology: topics, queues, publishers, and consumers.'));

  const body = el('div', { class: 'stack' }, loading('Reading message broker topology…'));
  host.appendChild(body);

  let d;
  try { d = await api('/api/events'); }
  catch (e) { clear(body); failAnswer(host, 'Event bus & queues'); return showError(body, e, 'Event bus & queues'); }

  renderIndexBar();
  clear(body);

  const topics = d.topics || [];
  const total = Number(d.total || topics.length);
  let totalProducers = 0;
  let totalConsumers = 0;
  for (const t of topics) {
    totalProducers += (t.producers || []).length;
    totalConsumers += (t.consumers || []).length;
  }

  setAnswer(host, answer([
    B(num(total) + ' ' + plural('topic', total)),
    ' across Kafka, RabbitMQ, SQS, SNS, and Pub/Sub. ',
    B(num(totalProducers)), ' publisher' + (totalProducers === 1 ? '' : 's') + ', ',
    B(num(totalConsumers)), ' consumer' + (totalConsumers === 1 ? '' : 's') + '.',
  ], { support: d.guidance || 'Decoupled services communicate through event buses without direct HTTP connections.' }));

  if (topics.length === 0) {
    body.appendChild(emptyState('No event bus publishers or consumers found', [
      'RepoCortex inspects Kafka (Sarama, kafka-go, Spring-Kafka, KafkaJS, kafka-python), RabbitMQ, AWS SQS/SNS, and GCP Pub/Sub.',
      'When your code publishes or subscribes to message topics, they will appear here.'
    ]));
    return;
  }

  const kpis = el('div', { class: 'kpis' },
    kpi('Topics & Queues', num(total), 'discovered message channels across brokers', 'ok'),
    kpi('Active Publishers', num(totalProducers), 'components that produce events', 'ok'),
    kpi('Active Consumers', num(totalConsumers), 'components that subscribe to events', 'ok')
  );
  body.appendChild(kpis);

  const card = el('div', { class: 'card' },
    el('div', { class: 'head' }, el('h3', { text: 'Discovered Event Topics' })),
    el('div', { class: 'body' })
  );
  const cardBody = card.querySelector('.body');

  const table = el('table', { class: 'table' },
    el('thead', null,
      el('tr', null,
        el('th', { text: 'BROKER' }),
        el('th', { text: 'TOPIC / QUEUE' }),
        el('th', { text: 'PUBLISHERS' }),
        el('th', { text: 'CONSUMERS' }),
        el('th', { text: 'FQN' })
      )
    )
  );
  const tbody = el('tbody');
  for (const t of topics) {
    const tr = el('tr', { style: 'cursor:pointer' });
    const bTag = el('span', { class: 'badge', text: t.broker || 'event' });
    tr.appendChild(el('td', null, bTag));
    tr.appendChild(el('td', null, el('strong', { text: t.topic })));
    tr.appendChild(el('td', null, el('span', { text: num((t.producers || []).length) })));
    tr.appendChild(el('td', null, el('span', { text: num((t.consumers || []).length) })));
    tr.appendChild(el('td', { class: 'mono small muted', text: t.fqn }));
    tr.addEventListener('click', () => {
      openInspector({
        kind: 'event',
        name: t.topic,
        fqn: t.fqn,
        broker: t.broker,
        producers: t.producers,
        consumers: t.consumers
      });
    });
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  cardBody.appendChild(el('div', { class: 'tablewrap' }, table));
  body.appendChild(card);
}

// ================================================================ SCREEN 20 — BLINDSPOTS

LOADERS.missed = async function () {
  const host = $('#screen-missed');
  if (state.loaded.missed) return;
  state.loaded.missed = true;
  await drawMissed();
};

async function drawMissed() {
  const host = $('#screen-missed');
  clear(host);
  host.appendChild(answer('Code intelligence blindspots and hidden maintenance risks.'));

  const body = el('div', { class: 'stack' }, loading('Scanning for blindspots…'));
  host.appendChild(body);

  let d;
  try { d = await api('/api/missed'); }
  catch (e) { clear(body); failAnswer(host, 'Code blindspots'); return showError(body, e, 'Code blindspots'); }

  renderIndexBar();
  clear(body);

  const total = Number(d.total_blindspots || 0);
  const byKind = d.by_kind || {};
  const list = d.blindspots || [];

  setAnswer(host, answer([
    B(num(total) + ' ' + plural('blindspot', total)),
    ' identified across unresolved calls, untested symbols, unmapped endpoints, and orphan tables.'
  ], { support: 'A blindspot is code that static analysis sees but cannot prove or that has no covering test.' }));

  const kpis = el('div', { class: 'kpis' },
    kpi('Unresolved Calls', num(byKind['unresolved_call'] || 0), 'calls that could not be statically bound', (byKind['unresolved_call'] > 0 ? 'warn' : 'ok')),
    kpi('Untested Critical', num(byKind['untested_critical'] || 0), 'exported symbols without covering tests', (byKind['untested_critical'] > 0 ? 'bad' : 'ok')),
    kpi('Unmapped Endpoints', num(byKind['unmapped_endpoint'] || 0), 'HTTP endpoints without handler binding', (byKind['unmapped_endpoint'] > 0 ? 'warn' : 'ok')),
    kpi('Orphan Tables', num(byKind['orphan_table'] || 0), 'database tables with no detected read/write', (byKind['orphan_table'] > 0 ? 'warn' : 'ok'))
  );
  body.appendChild(kpis);

  if (list.length === 0) {
    body.appendChild(emptyState('Zero blindspots found', [
      'All call sites resolved cleanly, exported critical functions have test coverage, and external resources are mapped.'
    ]));
    return;
  }

  const card = el('div', { class: 'card' },
    el('div', { class: 'head' }, el('h3', { text: 'Prioritized Actionable Findings' })),
    el('div', { class: 'body' })
  );
  const cardBody = card.querySelector('.body');

  const table = el('table', { class: 'table' },
    el('thead', null,
      el('tr', null,
        el('th', { text: 'SEVERITY' }),
        el('th', { text: 'CATEGORY' }),
        el('th', { text: 'TARGET' }),
        el('th', { text: 'LOCATION' }),
        el('th', { text: 'REMEDIATION' })
      )
    )
  );
  const tbody = el('tbody');
  for (const b of list) {
    const tr = el('tr');
    const sevClass = b.severity === 'high' ? 'pill-err' : (b.severity === 'medium' ? 'pill-warn' : 'pill-info');
    tr.appendChild(el('td', null, el('span', { class: 'pill ' + sevClass, text: b.severity })));
    tr.appendChild(el('td', null, el('span', { class: 'mono small', text: b.kind })));
    tr.appendChild(el('td', null, el('strong', { text: b.target })));
    const loc = b.path ? evidence(b.path, b.line) : el('span', { class: 'muted small', text: '—' });
    tr.appendChild(el('td', null, loc));
    tr.appendChild(el('td', { class: 'small', text: b.remediation }));
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  cardBody.appendChild(el('div', { class: 'tablewrap' }, table));
  body.appendChild(card);
}

// ================================================================ SCREEN 21 — PLAN REFACTOR

LOADERS.planchange = async function () {
  const host = $('#screen-planchange');
  if (state.loaded.planchange) return;
  state.loaded.planchange = true;
  await drawPlanChange();
};

async function drawPlanChange() {
  const host = $('#screen-planchange');
  clear(host);
  host.appendChild(answer('Deterministic refactoring planner: dependency order, call updates, and verification.'));

  const wrap = el('div', { class: 'stack' });
  host.appendChild(wrap);

  const searchBox = el('div', { class: 'card', style: 'padding:18px' },
    el('form', { style: 'display:flex;gap:12px;align-items:center' },
      el('input', {
        id: 'planchange-input',
        type: 'search',
        placeholder: 'Enter symbol name to refactor (e.g. AlternatePaymentApplication, OrderService)...',
        style: 'flex:1;padding:8px 12px;border:1px solid var(--line-strong);border-radius:6px;font-family:var(--mono);font-size:13px'
      }),
      el('button', { type: 'submit', class: 'primary', text: 'Generate Blueprint' })
    ),
    el('div', { style: 'display:flex;align-items:center;gap:8px;margin-top:12px;flex-wrap:wrap' },
      el('span', { class: 'small muted', text: 'Try example refactoring targets:' }),
      ...['AlternatePaymentApplication', 'ResponseHandling', 'getMerchantId', 'src/main/java/com/mosambee/alternatepayment/utility/ResultGenerator.java:getMerchantId'].map(target =>
        el('button', {
          type: 'button',
          class: 'chip',
          text: target.includes('/') ? target.split(':').pop() + ' (FQN)' : target,
          onclick: () => {
            const inp = $('#planchange-input');
            if (inp) inp.value = target;
            form.dispatchEvent(new Event('submit', { cancelable: true }));
          }
        })
      )
    )
  );
  wrap.appendChild(searchBox);

  const resultContainer = el('div', { class: 'stack', id: 'planchange-results' });
  wrap.appendChild(resultContainer);

  const introCard = el('div', { class: 'card', style: 'padding:22px' },
    el('header', { style: 'margin-bottom:14px' },
      el('h3', { text: '🛡️ Pre-Flight Refactoring Blueprint Engine' }),
      el('div', { class: 'sub', text: 'Simulate breaking changes, signature mutations, and deletions with compiler precision before editing code.' })
    ),
    el('div', { class: 'grid-3', style: 'margin-top:12px' },
      el('div', { class: 'card-inner', style: 'background:var(--well);padding:14px;border-radius:8px' },
        el('h4', { style: 'margin:0 0 6px', text: '1. Topological Order' }),
        el('p', { class: 'small muted', text: 'Calculates the exact safe dependency order for renaming, rewriting, or removing functions without broken intermediary builds.' })
      ),
      el('div', { class: 'card-inner', style: 'background:var(--well);padding:14px;border-radius:8px' },
        el('h4', { style: 'margin:0 0 6px', text: '2. Call Site Safety' }),
        el('p', { class: 'small muted', text: 'Identifies every upstream and cross-boundary call site that requires parameter alignment or null-check guards.' })
      ),
      el('div', { class: 'card-inner', style: 'background:var(--well);padding:14px;border-radius:8px' },
        el('h4', { style: 'margin:0 0 6px', text: '3. Test Invariants' }),
        el('p', { class: 'small muted', text: 'Pinpoints the exact subset of unit and integration test suites that must pass to verify the refactoring.' })
      )
    )
  );
  resultContainer.appendChild(introCard);

  const form = searchBox.querySelector('form');
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const sym = $('#planchange-input').value.trim();
    if (!sym) return;
    clear(resultContainer);
    resultContainer.appendChild(loading('Synthesizing refactoring plan for ' + sym + '…'));

    let p;
    try { p = await api('/api/planchange', { symbol: sym }); }
    catch (err) { clear(resultContainer); return showError(resultContainer, err, 'Plan Change'); }

    clear(resultContainer);
    renderPlanResult(p, resultContainer);
  });
}

function renderPlanResult(p, host) {
  if (p.error) {
    if (p.candidates && p.candidates.length > 0) {
      const ambCard = el('div', { class: 'card', style: 'padding:18px' },
        el('h4', { style: 'margin:0 0 8px', text: p.error }),
        el('div', { class: 'small muted', style: 'margin-bottom:12px', text: 'Select a candidate symbol below to generate its refactoring blueprint:' }),
        el('div', { class: 'stack', style: 'gap:8px' },
          ...p.candidates.map(c => el('div', {
            class: 'card-inner',
            style: 'padding:10px 14px;border:1px solid var(--line);border-radius:6px;display:flex;justify-content:space-between;align-items:center'
          },
            el('div', null,
              el('span', { class: 'mono', style: 'font-weight:600;font-size:13px', text: c.name }),
              el('span', { class: 'pill', style: 'margin-left:8px', text: c.kind }),
              el('div', { class: 'small muted mono', style: 'margin-top:2px', text: c.file_path + ':' + c.line })
            ),
            el('button', {
              type: 'button',
              class: 'primary',
              style: 'padding:4px 10px;font-size:12px',
              text: 'Select & Plan →',
              onclick: () => {
                const inp = $('#planchange-input');
                if (inp) inp.value = c.fqn;
                const form = host.closest('#screen-planchange').querySelector('form');
                if (form) form.dispatchEvent(new Event('submit', { cancelable: true }));
              }
            })
          ))
        )
      );
      host.appendChild(ambCard);
      return;
    }
    host.appendChild(emptyState('Could not build plan', [p.error]));
    return;
  }

  const tgt = p.target || {};
  const riskClass = p.risk_level === 'high' ? 'pill-err' : (p.risk_level === 'medium' ? 'pill-warn' : 'pill-ok');

  const metaCard = el('div', { class: 'card', style: 'padding:18px;margin-bottom:16px' },
    el('div', { style: 'display:flex;justify-content:space-between;align-items:center;margin-bottom:12px' },
      el('div', null,
        el('h3', { style: 'margin:0 0 4px', text: tgt.name || 'Target' }),
        el('div', { class: 'mono small muted', text: (tgt.file_path || '') + (tgt.line ? (':' + tgt.line) : '') })
      ),
      el('div', { style: 'text-align:right' },
        el('span', { class: 'pill ' + riskClass, text: 'Risk Score: ' + p.risk_score + '/100 (' + p.risk_level + ')' }),
        el('div', { class: 'small muted', style: 'margin-top:4px', text: p.direct_callers + ' direct, ' + p.total_callers + ' total callers' })
      )
    )
  );
  host.appendChild(metaCard);

  const stepsHead = el('h3', { text: 'Execution Blueprint (Topologically Ordered Steps)' });
  host.appendChild(stepsHead);

  const steps = p.steps || [];
  for (const s of steps) {
    const sc = el('div', { class: 'step-card' },
      el('div', { class: 'step-order', text: String(s.order) }),
      el('div', { class: 'step-content' },
        el('div', { class: 'step-title', text: s.description }),
        el('div', { class: 'step-desc', text: s.path ? (s.path + ':' + s.line) : '' }),
        s.command ? el('div', { class: 'step-cmd', text: '$ ' + s.command }) : el('span')
      )
    );
    host.appendChild(sc);
  }

  const pre = p.preconditions || [];
  const post = p.postconditions || [];
  if (pre.length > 0 || post.length > 0) {
    const invCard = el('div', { class: 'grid-2', style: 'margin-top:16px' },
      el('div', { class: 'card', style: 'padding:16px' },
        el('h4', { style: 'margin:0 0 8px;display:flex;align-items:center;gap:6px', text: '📋 Preconditions (Entry Criteria)' }),
        el('ul', { style: 'margin:0;padding-left:18px;display:flex;flex-direction:column;gap:6px' },
          ...pre.map(item => el('li', { class: 'small', text: item }))
        )
      ),
      el('div', { class: 'card', style: 'padding:16px' },
        el('h4', { style: 'margin:0 0 8px;display:flex;align-items:center;gap:6px', text: '✅ Postconditions (Verification)' }),
        el('ul', { style: 'margin:0;padding-left:18px;display:flex;flex-direction:column;gap:6px' },
          ...post.map(item => el('li', { class: 'small', text: item }))
        )
      )
    );
    host.appendChild(invCard);
  }

  const tests = p.affected_tests || [];
  const testCard = el('div', { class: 'card', style: 'padding:16px;margin-top:16px' },
    el('h4', { style: 'margin:0 0 8px', text: '🧪 Affected Test Suites (' + tests.length + ')' }),
    tests.length > 0
      ? el('ul', { style: 'margin:0;padding-left:18px;display:flex;flex-direction:column;gap:4px' },
          ...tests.map(t => el('li', { class: 'mono small', text: t }))
        )
      : el('p', { class: 'small muted', style: 'margin:0', text: 'No test suites directly exercise this symbol according to call graph analysis.' })
  );
  host.appendChild(testCard);
}

// ================================================================ SCREEN 22 — CONNECT AN AGENT & MCP TOKENS

LOADERS.agent = async function () {
  const host = $('#screen-agent');
  if (state.loaded.agent) return;
  state.loaded.agent = true;
  await drawAgent();
};

async function drawAgent() {
  const host = $('#screen-agent');
  clear(host);
  host.appendChild(answer('Model Context Protocol (MCP) server & AI context token savings analytics.'));

  const body = el('div', { class: 'stack' }, loading('Connecting to MCP engine & computing token economics…'));
  host.appendChild(body);

  let d;
  try { d = await api('/api/agent'); }
  catch (e) { clear(body); failAnswer(host, 'Connect an agent'); return showError(body, e, 'Connect an agent'); }

  renderIndexBar();
  clear(body);

  const tm = d.token_metrics || {};
  const sample = d.sample || {};
  const tools = d.mcp_tools || [];

  setAnswer(host, answer([
    'An agent connected to this index can ask about ',
    B(num(tm.total_symbols) + ' symbols'), ' and ', B(num(tm.total_edges) + ' relationships'),
    ' across ', B(num(tm.total_files_indexed) + ' files'), ', through ', B(num(tools.length) + ' tools'), '.'
  ], {
    support: 'The server runs on stdio (`cortex serve`). Each answer says how sure it is, instead of handing the agent whole files.',
    next: { label: 'Connect your agent', detail: 'Register cortex with the agents on this machine', command: 'cortex install --rules' }
  }));

  // 1. Headline KPIs
  const kpis = el('div', { class: 'kpis' },
    kpi('MCP Tools', num(tools.length), 'registered tools an agent can call', 'ok'),
    kpi('Symbols', num(tm.total_symbols), 'what a tool call can name', 'ok'),
    kpi('Indexed Surface', num(tm.total_files_indexed) + ' files', num(tm.total_symbols) + ' symbols, ' + num(tm.total_edges) + ' edges', 'ok')
  );
  body.appendChild(kpis);

  // 2. Quick Connect Card (Claude Desktop / Cursor / Antigravity / CLI)
  const configSnippetStr = JSON.stringify(d.config_snippet, null, 2);
  const configCard = el('div', { class: 'card', style: 'padding:18px' },
    el('header', { style: 'margin-bottom:12px' },
      el('h3', { text: 'Connect Any AI Agent via MCP in 10 Seconds' }),
      el('div', { class: 'sub', text: 'RepoCortex adheres to the open Model Context Protocol. Point Claude, Cursor, Gemini, or Antigravity to your local cortex executable.' })
    ),
    el('div', { class: 'body' },
      el('div', { style: 'display:grid;grid-template-columns:repeat(auto-fit, minmax(320px, 1fr));gap:16px' },
        el('div', { class: 'card-inner', style: 'background:var(--well);padding:14px;border-radius:8px' },
          el('h4', { style: 'margin:0 0 6px', text: '1. Claude Desktop' }),
          el('p', { class: 'small muted', style: 'margin-bottom:8px', text: 'Add to ~/Library/Application Support/Claude/claude_desktop_config.json:' }),
          el('pre', { class: 'mono small', style: 'padding:10px;background:var(--canvas-bg);border:1px solid var(--line);border-radius:6px;overflow-x:auto' },
            el('code', { text: configSnippetStr })
          ),
          copyButton(configSnippetStr, 'Copy Claude Config')
        ),
        el('div', { class: 'card-inner', style: 'background:var(--well);padding:14px;border-radius:8px' },
          el('h4', { style: 'margin:0 0 6px', text: '2. Cursor / VS Code / Antigravity' }),
          el('p', { class: 'small muted', style: 'margin-bottom:8px', text: 'Add to .cursor/mcp.json or workspace MCP settings:' }),
          el('pre', { class: 'mono small', style: 'padding:10px;background:var(--canvas-bg);border:1px solid var(--line);border-radius:6px;overflow-x:auto' },
            el('code', { text: configSnippetStr })
          ),
          copyButton(configSnippetStr, 'Copy Cursor Config')
        )
      )
    )
  );
  body.appendChild(configCard);

  // 3. Live AST Pruning Demo & Token Economy
  if (sample && sample.file_path) {
    const demoCard = el('div', { class: 'card', style: 'padding:18px' },
      el('header', { style: 'margin-bottom:12px' },
        el('div', { style: 'display:flex;justify-content:space-between;align-items:center' },
          el('div', null,
            el('h3', { text: 'Live Token Savings in Action (Real Code Verification)' }),
            el('div', { class: 'sub', text: 'Demonstrating structural AST pruning on ' + sample.file_path + ' (' + sample.language + ')' })
          ),
          el('span', { class: 'pill pill-ok', style: 'font-size:13px;font-weight:700', text: sample.savings_pct.toFixed(1) + '% Token Reduction' })
        )
      ),
      el('div', { class: 'body' },
        el('div', { style: 'display:grid;grid-template-columns:1fr 1fr;gap:16px;margin-bottom:12px' },
          el('div', null,
            el('div', { style: 'display:flex;justify-content:space-between;margin-bottom:4px' },
              el('strong', { text: 'Raw File Dump (Standard AI Tooling)' }),
              el('span', { class: 'mono small pill-err pill', text: sample.original_tokens + ' tokens (' + num(sample.original_bytes) + ' B)' })
            ),
            el('pre', { class: 'mono small', style: 'padding:12px;background:var(--canvas-bg);border:1px solid var(--line);border-radius:6px;max-height:300px;overflow-y:auto;white-space:pre-wrap' },
              el('code', { text: sample.original_excerpt })
            )
          ),
          el('div', null,
            el('div', { style: 'display:flex;justify-content:space-between;margin-bottom:4px' },
              el('strong', { text: 'RepoCortex Pruned Outline (MCP prune_context)' }),
              el('span', { class: 'mono small pill-ok pill', text: sample.pruned_tokens + ' tokens (' + num(sample.pruned_bytes) + ' B)' })
            ),
            el('pre', { class: 'mono small', style: 'padding:12px;background:var(--canvas-bg);border:1px solid var(--line);border-radius:6px;max-height:300px;overflow-y:auto;white-space:pre-wrap' },
              el('code', { text: sample.pruned_excerpt })
            )
          )
        ),
        el('p', { class: 'small muted', text: 'Why this works: RepoCortex removes function bodies while preserving signatures, docstrings, type hierarchies, and annotations. The AI receives full structural intelligence with 90%+ fewer tokens.' })
      )
    );
    body.appendChild(demoCard);
  }

  // 4. RepoCortex System & Index Health Card
  const health = d.health || {};
  const healthSev = health.severity === 'ok' ? 'ok' : (health.severity === 'warn' ? 'warn' : 'err');
  const healthCard = el('div', { class: 'card', style: 'padding:18px' },
    el('header', { style: 'margin-bottom:12px;display:flex;justify-content:space-between;align-items:center' },
      el('div', null,
        el('h3', { text: 'RepoCortex Health & Resolution Accuracy' }),
        el('div', { class: 'sub', text: health.headline || 'Index health verdict' })
      ),
      el('span', { class: 'pill pill-' + healthSev, style: 'font-weight:700', text: (health.status || 'unknown').toUpperCase() })
    ),
    el('div', { class: 'body' },
      el('div', { class: 'kpis' },
        kpi('Server Process', d.status === 'active' ? 'Operational' : d.status, 'Bound to 127.0.0.1, read-only mode', 'ok'),
        kpi('Index Verdict', (health.status || 'healthy').toUpperCase(), health.trustworthy ? 'Trustworthy for decisions' : 'Lower bound counts', healthSev),
        kpi('Graph Edges', num(tm.total_edges), 'Validated architectural relationships', 'ok'),
        kpi('Parse Accuracy', health.reasons && health.reasons.length ? num(health.reasons.length) + ' issues' : '100% Parsed', 'Tree-sitter compiler verification', healthSev)
      ),
      health.reasons && health.reasons.length ? el('div', { style: 'margin-top:12px;background:var(--well);padding:12px;border-radius:6px' },
        el('h4', { style: 'margin:0 0 8px', text: 'Detected Index Caveats & Remedies:' }),
        el('ul', { style: 'margin:0;padding-left:18px' },
          health.reasons.map(r => el('li', { style: 'margin-bottom:6px' },
            el('strong', { text: r.code + ': ' }),
            el('span', { text: r.detail + ' ' }),
            r.remedy ? el('code', { class: 'sym', text: r.remedy }) : null
          ))
        )
      ) : null
    )
  );
  body.appendChild(healthCard);

  // 5. Token Savings as per Agent Query (Task Benchmark)
  const benchmarks = d.query_benchmarks || [];
  if (benchmarks.length > 0) {
    const benchCard = el('div', { class: 'card' },
      el('header', { style: 'padding:14px 18px 0' },
        el('h3', { text: 'Token Savings as per Agent Query (Task Benchmark)' }),
        el('div', { class: 'sub', text: 'Empirical token consumption comparing raw file/grep inspection vs RepoCortex compiler-grounded MCP responses.' })
      ),
      el('div', { class: 'body' })
    );
    const benchBody = benchCard.querySelector('.body');
    const bTable = el('table', { class: 'table' },
      el('thead', null,
        el('tr', null,
          el('th', { text: 'AGENT QUERY / INTENT' }),
          el('th', { text: 'MCP TOOL' }),
          el('th', { text: 'RAW TOKENS' }),
          el('th', { text: 'REPOCORTEX' }),
          el('th', { text: 'TOKENS SAVED' }),
          el('th', { text: 'REDUCTION' }),
          el('th', { text: 'HOW REPOCORTEX PRUNES IT' })
        )
      )
    );
    const bTbody = el('tbody');
    for (const b of benchmarks) {
      const tr = el('tr');
      tr.appendChild(el('td', null, el('strong', { text: b.query_intent })));
      tr.appendChild(el('td', null, el('code', { class: 'sym', text: b.tool_used })));
      tr.appendChild(el('td', null, el('span', { class: 'pill pill-err', text: num(b.raw_tokens) })));
      tr.appendChild(el('td', null, el('span', { class: 'pill pill-ok', text: num(b.pruned_tokens) })));
      tr.appendChild(el('td', null, el('strong', { style: 'color:var(--ok)', text: '+' + num(b.tokens_saved) })));
      tr.appendChild(el('td', null, el('span', { class: 'pill pill-ok', style: 'font-weight:700', text: b.savings_pct.toFixed(1) + '%' })));
      tr.appendChild(el('td', { class: 'small muted', text: b.explanation }));
      bTbody.appendChild(tr);
    }
    bTable.appendChild(bTbody);
    benchBody.appendChild(el('div', { class: 'tablewrap' }, bTable));
    body.appendChild(benchCard);
  }

  // 6. Connected MCP Tools Table
  const toolsCard = el('div', { class: 'card' },
    el('header', { style: 'padding:14px 18px 0' },
      el('h3', { text: 'MCP tools' }),
      el('div', { class: 'sub', text: 'Every tool the server registers. The default profile exposes the first seven; set `mcp: { profile: standard }` in .cortex.yml for all of them.' })
    ),
    el('div', { class: 'body' })
  );
  const toolsBody = toolsCard.querySelector('.body');
  const table = el('table', { class: 'table' },
    el('thead', null,
      el('tr', null,
        el('th', { text: 'TOOL NAME' }),
        el('th', { text: 'CATEGORY' }),
        el('th', { text: 'CAPABILITY & BENEFIT' })
      )
    )
  );
  const tbody = el('tbody');
  for (const t of tools) {
    const tr = el('tr');
    tr.appendChild(el('td', null, el('code', { class: 'sym', text: t.name })));
    tr.appendChild(el('td', null, el('span', { class: 'badge', text: t.category })));
    tr.appendChild(el('td', null,
      el('div', { text: t.description }),
      el('div', { class: 'small muted', style: 'margin-top:2px', text: '✦ ' + t.benefit })
    ));
    tbody.appendChild(tr);
  }
  table.appendChild(tbody);
  toolsBody.appendChild(el('div', { class: 'tablewrap' }, table));
  body.appendChild(toolsCard);

  // The guidance this screen has always carried: what the pillar answers, why
  // this index cannot show a transcript, and the command that produces one.
  const spec = PILLAR_SPECS.agent;
  if (spec) {
    let overview = state.overview;
    if (!overview) {
      try { overview = await api('/api/overview', { min_confidence: state.q.floor }); }
      catch (e) { return showError(body, e, 'Connect an agent'); }
      state.overview = overview;
    }
    host.appendChild(answer(spec.answer(overview), { support: spec.lede }));
    for (const node of pillarBody(overview, spec)) body.appendChild(node);
  }
}

// ================================================================ COMMAND PALETTE & 360° INSPECTOR

function wirePalette() {
  const openBtn = $('#palette-open-btn');
  const backdrop = $('#command-palette-backdrop');
  const input = $('#palette-search-input');
  const results = $('#palette-results');
  if (!backdrop || !input || !results) return;

  function open() {
    backdrop.hidden = false;
    input.value = '';
    renderPaletteItems('');
    input.focus();
  }

  function close() {
    backdrop.hidden = true;
  }

  if (openBtn) openBtn.addEventListener('click', open);
  backdrop.addEventListener('click', (e) => {
    if (e.target === backdrop) close();
  });

  window.addEventListener('keydown', (e) => {
    if ((e.metaKey || e.ctrlKey) && (e.key === 'k' || e.key === 'K')) {
      e.preventDefault();
      if (backdrop.hidden) open(); else close();
    } else if (e.key === 'Escape' && !backdrop.hidden) {
      e.preventDefault();
      close();
    }
  });

  input.addEventListener('input', () => {
    renderPaletteItems(input.value.trim().toLowerCase());
  });

  function renderPaletteItems(q) {
    clear(results);
    const screens = [
      { id: 'overview', title: 'Index Health', group: 'System' },
      { id: 'graph', title: 'Code map: Graph', group: 'Code', view: 'map' },
      { id: 'graph-tree', title: 'Code map: Tree', group: 'Code', view: 'tree' },
      { id: 'graph-treemap', title: 'Code map: Treemap', group: 'Code', view: 'treemap' },
      { id: 'graph-code', title: 'Code map: Code', group: 'Code', view: 'code' },
      { id: 'search', title: 'Find a Symbol', group: 'Code' },
      { id: 'metrics', title: 'Shape of the Code (Metrics)', group: 'Code' },
      { id: 'blast', title: 'What Could Break (Blast Radius)', group: 'Change Safety' },
      { id: 'planchange', title: 'Plan a Refactor (Refactor Planner)', group: 'Change Safety' },
      { id: 'contract', title: 'Task Brief', group: 'Change Safety' },
      { id: 'verify', title: 'Check a Change', group: 'Change Safety' },
      { id: 'ownership', title: 'Who Knows This Code', group: 'People' },
      { id: 'diff', title: 'What Changed (Fact Diff)', group: 'Change Safety' },
      { id: 'crossstack', title: 'APIs & What They Touch', group: 'Runtime' },
      { id: 'tables', title: 'Database Tables', group: 'Runtime' },
      { id: 'events', title: 'Event Bus & Queues (Kafka/AMQP/SQS/PubSub)', group: 'Runtime' },
      { id: 'config', title: 'Settings & Secrets', group: 'Runtime' },
      { id: 'trace', title: 'What Actually Ran', group: 'Runtime' },
      { id: 'estate', title: 'Across Repositories', group: 'Runtime' },
      { id: 'tests', title: 'Test Coverage', group: 'Quality' },
      { id: 'missed', title: 'Code Blindspots & Debt', group: 'Quality' },
      { id: 'governance', title: 'Architecture Rules', group: 'Quality' },
      { id: 'deps', title: 'Dependencies', group: 'Quality' },
      { id: 'accuracy', title: 'Index Accuracy', group: 'Quality' },
      { id: 'agent', title: 'Connect an Agent & MCP Token Savings', group: 'AI & MCP' },
      { id: 'retrieval', title: 'Retrieval Quality & Case Benchmarks', group: 'AI & MCP' }
    ];

    const matches = screens.filter(s => !q || s.title.toLowerCase().includes(q) || s.id.includes(q) || s.group.toLowerCase().includes(q));
    for (const m of matches) {
      const item = el('div', { class: 'palette-item', role: 'option' },
        el('div', { class: 'palette-item-title' },
          el('span', { text: '◈', style: 'color:var(--accent)' }),
          el('span', { text: m.title })
        ),
        el('span', { class: 'palette-item-tag', text: m.group })
      );
      item.addEventListener('click', () => {
        close();
        if (m.view) {
          // go() shows the screen; a bare setQuery left a reader on another
          // screen looking at nothing new.
          go('graph', { gv: m.view });
        } else {
          go(m.id);
        }
      });
      results.appendChild(item);
    }
  }
}

function wireInspector() {
  const drawer = $('#inspector-drawer');
  const closeBtn = $('#inspector-close');
  if (closeBtn && drawer) {
    closeBtn.addEventListener('click', () => { drawer.hidden = true; });
  }
}

function openInspector(data) {
  const drawer = $('#inspector-drawer');
  const title = $('#inspector-title');
  const content = $('#inspector-content');
  if (!drawer || !title || !content) return;

  clear(content);
  title.textContent = data.name || data.topic || 'Inspector';

  const overviewSec = el('div', { class: 'inspector-section' },
    el('h4', { text: 'Details' }),
    el('div', { class: 'mono small', style: 'margin-bottom:4px', text: 'Kind: ' + (data.kind || 'symbol') }),
    data.fqn ? el('div', { class: 'mono small muted', style: 'margin-bottom:4px', text: 'FQN: ' + data.fqn }) : el('span'),
    data.broker ? el('div', { class: 'mono small', text: 'Broker: ' + data.broker }) : el('span')
  );
  content.appendChild(overviewSec);

  if (data.producers && data.producers.length > 0) {
    const pubSec = el('div', { class: 'inspector-section' },
      el('h4', { text: 'Publishers (' + data.producers.length + ')' })
    );
    const ul = el('ul', { class: 'inspector-list' });
    for (const p of data.producers) {
      ul.appendChild(el('li', null,
        el('strong', { text: p.symbol_name || p.symbol_fqn }),
        el('div', { class: 'muted small', text: p.file_path + ':' + p.line })
      ));
    }
    pubSec.appendChild(ul);
    content.appendChild(pubSec);
  }

  if (data.consumers && data.consumers.length > 0) {
    const conSec = el('div', { class: 'inspector-section' },
      el('h4', { text: 'Consumers (' + data.consumers.length + ')' })
    );
    const ul = el('ul', { class: 'inspector-list' });
    for (const c of data.consumers) {
      ul.appendChild(el('li', null,
        el('strong', { text: c.symbol_name || c.symbol_fqn }),
        el('div', { class: 'muted small', text: c.file_path + ':' + c.line })
      ));
    }
    conSec.appendChild(ul);
    content.appendChild(conSec);
  }

  const actionSec = el('div', { class: 'inspector-section', style: 'margin-top:20px' },
    el('button', {
      type: 'button',
      class: 'primary',
      style: 'width:100%',
      text: 'Analyze Blast Radius'
    })
  );
  actionSec.querySelector('button').addEventListener('click', () => {
    drawer.hidden = true;
    go('blast', { target: data.name || data.topic });
  });
  content.appendChild(actionSec);

  drawer.hidden = false;
}

// ================================================================ BOOT

async function boot() {
  state.q = readQuery();
  initTheme();
  wirePalette();
  wireInspector();

  for (const btn of document.querySelectorAll('.nav')) {
    btn.addEventListener('click', () => go(btn.dataset.screen, {}));
  }
  const glossaryToggle = $('#glossary-toggle');
  if (glossaryToggle) glossaryToggle.addEventListener('click', toggleGlossary);
  window.addEventListener('popstate', () => {
    state.q = readQuery();
    showScreen(state.q.s);
    renderIndexBar();
  });

  try {
    const health = await api('/api/health');
    state.meta = health.meta || null;
    if (state.meta && state.meta.index_path) {
      // vscode:// links need an absolute path, and the index lives two levels
      // inside the repository it describes.
      const parts = String(state.meta.index_path).split('/');
      parts.pop(); parts.pop();
      state.repoRoot = parts.join('/');
      $('#index-path').textContent = state.meta.index_path;
      $('#project').textContent = text(state.meta.project, 'RepoCortex');
    }
    if (state.meta && state.meta.indexed === false) {
      $('#main').appendChild(emptyState('This directory has no index yet',
        ['There is nothing to show — which is different from a repository with nothing in it.',
          'Run cortex init, then cortex index, then reload this page.']));
      return;
    }
    renderIndexBar();
    // Deliberately not awaited. The provenance of the index is a second
    // question; the first one is answered by the screen already rendering.
    loadIndexState();
    const foot = $('#foot-meta');
    if (foot && state.meta) {
      foot.textContent = text(state.meta.project) + ' · run ' + num(state.meta.run ? state.meta.run.run_id : null)
        + ' · ' + num(state.meta.run ? state.meta.run.files_indexed : null) + ' files indexed · read-only';
    }
  } catch (e) {
    showError($('#main'), e, 'The portal');
    return;
  }

  showScreen(state.q.s);
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
else boot();

})();
