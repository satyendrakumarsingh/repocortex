/*
  THE CODE MAP'S SHELL AND ITS THREE NON-CANVAS VIEWS.

  The graph screen is one destination with four views of the same index:

    Graph    the canvas map (app.js: GraphCanvas, rebuildGraph). Unchanged in
             what it computes; this module only gives it a header.
    Tree     repository → directory → file → symbol, as an outline.
    Treemap  where the code is: directories, then a directory's files, sized and
             coloured by a measure. It replaces the card browser's job — "what
             is inside this, and how big is it" — at a glance.
    Code     one file's INDEXED text (/api/source), its declarations, and the
             lines that relationships cite.

  WHY THEY SHARE ONE ADDRESS. The views agree on the map's own URL vocabulary:
  `container` and `level` say where the reader is, and `sel=L:<id>` names the
  selection at that level — a container id, a file path or a symbol id. Select
  a symbol in the tree, press Graph, and the map lands on that symbol's
  container at the symbol level with it selected; the reverse holds too. One
  vocabulary means a link sent from any view opens the same thing in all four.

  WHY NO innerHTML AND NO style ATTRIBUTE. The portal ships a Content-Security-
  Policy with `style-src 'self'` and no inline exceptions. Every node here is
  built with createElement, and the only styles set are through the CSSOM
  (style.setProperty), which the policy allows — the treemap's geometry and
  colours depend on that.

  WHAT IT READS. Only fields the API marshals; contract_test.go checks every
  snake_case read in this file against testdata/api-contract.golden.
*/

const xmlnsSvg = 'http://www.w3.org/2000/svg';

/* ---------------------------------------------------------------- DOM */

function h(tag, attrs, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') n.className = v;
    else if (k === 'text') n.textContent = String(v);
    else if (k === 'style') for (const [p, val] of Object.entries(v)) n.style.setProperty(p, val);
    else if (k.startsWith('on') && typeof v === 'function') n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, v === true ? '' : String(v));
  }
  for (const kid of kids.flat(3)) {
    if (kid === null || kid === undefined || kid === false || kid === '') continue;
    n.appendChild(typeof kid === 'object' ? kid : document.createTextNode(String(kid)));
  }
  return n;
}

/* One 16px stroke icon set. Decorative: every control that carries one also
   carries words or an aria-label. */
const ICON_PATHS = {
  graph: ['M5 4h6', 'M4.3 5.3l2.9 5.4', 'M11.7 5.3l-2.9 5.4', 'C3.5 4 1.5', 'C12.5 4 1.5', 'C8 12 1.5'],
  tree: ['M2.5 2.5h4v4h-4z', 'M9.5 9.5h4v4h-4z', 'M4.5 6.5v5h5', 'M9.5 4.5h4'],
  treemap: ['M2 2.5h12v11H2z', 'M8 2.5v11', 'M8 8h6', 'M2 9h6'],
  code: ['M5.5 4.5 2 8l3.5 3.5', 'M10.5 4.5 14 8l-3.5 3.5', 'M9 3 7 13'],
  chev: ['M6 4l4 4-4 4'],
  search: ['C7 7 4.5', 'M10.5 10.5l3 3'],
  copy: ['M5 5h8.5v8.5H5z', 'M3 10.5v-7a1 1 0 0 1 1-1h6.5'],
  up: ['M8 13V3', 'M4 7l4-4 4 4'],
  x: ['M4 4l8 8', 'M12 4l-8 8'],
  dir: ['M2 4.5h4.5L8 6h6v6.5H2z'],
  file: ['M4 2h5l3 3v9H4z', 'M9 2v3h3'],
  sym: ['C8 8 2.2'],
  target: ['C8 8 5.5', 'C8 8 2'],
  plus: ['M8 3v10', 'M3 8h10'],
  minus: ['M3 8h10'],
  fit: ['M2.5 6V2.5H6', 'M10 2.5h3.5V6', 'M13.5 10v3.5H10', 'M6 13.5H2.5V10'],
  expand: ['M2.5 6V2.5H6', 'M10 2.5h3.5V6', 'M13.5 10v3.5H10', 'M6 13.5H2.5V10', 'M6 6l4 4', 'M10 6l-4 4'],
  minimap: ['M2 3h12v10H2z', 'M8.5 8.5h4v3h-4z'],
  check: ['M3 8.5l3 3 7-7'],
};
function icon(name) {
  const s = document.createElementNS(xmlnsSvg, 'svg');
  s.setAttribute('viewBox', '0 0 16 16');
  s.setAttribute('class', 'cm-ic');
  s.setAttribute('aria-hidden', 'true');
  for (const d of ICON_PATHS[name] || []) {
    if (d.startsWith('C')) {
      const [cx, cy, r] = d.slice(1).trim().split(' ');
      const c = document.createElementNS(xmlnsSvg, 'circle');
      c.setAttribute('cx', cx); c.setAttribute('cy', cy); c.setAttribute('r', r);
      s.appendChild(c);
    } else {
      const p = document.createElementNS(xmlnsSvg, 'path');
      p.setAttribute('d', d);
      s.appendChild(p);
    }
  }
  return s;
}

function btn(label, opts) {
  const o = opts || {};
  return h('button', {
    type: 'button', class: 'cm-btn' + (o.variant ? ' cm-btn-' + o.variant : '') + (o.small ? ' cm-sm' : '') + (label ? '' : ' cm-icon-only'),
    title: o.title, 'aria-label': o.aria, 'aria-pressed': o.pressed === undefined ? null : String(o.pressed),
    'data-action': o.action, onclick: o.onclick, disabled: o.disabled,
  }, o.icon ? icon(o.icon) : null, label ? h('span', { text: label }) : null);
}

/* Short, whole words for kind tags — a clipped "functi" reads as a bug. */
const KIND_SHORT = { function: 'func', method: 'method', class: 'class', struct: 'struct', interface: 'iface', constant: 'const', variable: 'var', field: 'field', type: 'type', enum: 'enum', module: 'module', endpoint: 'route', table: 'table', parameter: 'param', property: 'prop' };
const kindShort = (k) => KIND_SHORT[String(k || '')] || String(k || '').slice(0, 6);
const fmt = (n) => (Number.isFinite(Number(n)) ? Number(n).toLocaleString() : '—');
const plural = (n, one, many) => fmt(n) + ' ' + (Number(n) === 1 ? one : (many || one + 's'));
const base = (p) => String(p || '').split('/').pop();
const dirOf = (p) => String(p || '').split('/').slice(0, -1).join('/');
const isTest = (p, name) => /(_test\.go|\.test\.[jt]sx?|\.spec\.[jt]sx?|Test\.java|_test\.py)$/.test(p || '') || /(^|\/)tests?\//.test(p || '') || /^Test[A-Z]/.test(name || '');

/* ---------------------------------------------------------------- data */

/*
  One cache per page, keyed by the request. A view switch re-reads nothing it
  already has; a floor change is a different key, so it re-reads exactly once.
*/
function makeStore(ctx) {
  const cache = new Map();
  const floor = () => ctx.q().floor || 'medium';
  const get = (path, params) => {
    const key = path + '?' + new URLSearchParams(params).toString();
    if (!cache.has(key)) cache.set(key, ctx.api(path, params).catch((e) => { cache.delete(key); throw e; }));
    return cache.get(key);
  };
  return {
    containers: () => get('/api/graph', { level: 'containers', nodes: '512', min_confidence: floor() }),
    files: (c) => get('/api/graph', { level: 'files', container: c, min_confidence: floor() }),
    symbols: (c) => get('/api/graph', { level: 'symbols', container: c, min_confidence: floor() }),
    source: (path, from) => get('/api/source', { path: path, from: String(from || 1), min_confidence: floor() }),
  };
}

/* The selection, decoded against the level it was made at. */
function selection(q) {
  const raw = String(q.sel || '');
  if (!raw.startsWith('L:')) return null;
  const id = raw.slice(2);
  const level = q.level || (q.container ? 'files' : 'containers');
  if (level === 'symbols') return { type: 'symbol', id: id, container: q.container };
  if (level === 'files') return { type: 'file', id: id, container: q.container };
  return { type: 'dir', id: id, container: id };
}
function selPatch(item) {
  if (!item) return { sel: '' };
  if (item.type === 'dir') return { level: '', container: '', sel: 'L:' + item.id };
  if (item.type === 'file') return { level: 'files', container: item.container, sel: 'L:' + item.id };
  return { level: 'symbols', container: item.container, sel: 'L:' + item.id };
}

/* ---------------------------------------------------------------- header */

export const VIEWS = [
  { id: 'map', label: 'Graph', icon: 'graph', hint: 'What talks to what' },
  { id: 'tree', label: 'Tree', icon: 'tree', hint: 'What is inside what' },
  { id: 'treemap', label: 'Treemap', icon: 'treemap', hint: 'Where the code is' },
  { id: 'code', label: 'Code', icon: 'code', hint: 'The source, with its relationships' },
];

/*
  The breadcrumb and the view tabs, above every view. The tabs are a real
  tablist: arrow keys move between them, the active one is the only tab stop,
  and choosing one keeps the scope and the selection.
*/
export function renderHeader(host, ctx, view) {
  while (host.firstChild) host.removeChild(host.firstChild);
  const q = ctx.q();
  const crumbs = h('nav', { class: 'cm-crumbs', 'aria-label': 'Where you are' });
  const items = [{ label: 'Repository', patch: { container: '', level: '', sel: '', cfile: '' } }];
  if (q.container) items.push({ label: q.container, patch: { container: q.container, level: 'files', sel: '', cfile: '' } });
  if (view === 'code' && q.cfile) items.push({ label: base(q.cfile), patch: null });
  items.forEach((it, i) => {
    if (i) crumbs.appendChild(icon('chev'));
    const last = i === items.length - 1;
    crumbs.appendChild(last
      ? h('span', { class: 'cm-crumb', 'aria-current': 'location', text: it.label })
      : h('button', { type: 'button', class: 'cm-crumb', text: it.label, onclick: () => ctx.navigate(view === 'code' ? 'map' : view, it.patch) }));
  });
  if (q.container && view !== 'code') {
    crumbs.appendChild(btn('Up a level', {
      icon: 'up', small: true, variant: 'quiet', title: 'Up a level (Esc)',
      onclick: () => ctx.navigate(view, q.level === 'symbols' ? { level: 'files', sel: '' } : { container: '', level: '', sel: '' }),
    }));
  }

  const tabs = h('div', { class: 'cm-tabs', role: 'tablist', 'aria-label': 'View', id: 'graph-view-switch' });
  for (const v of VIEWS) {
    const on = v.id === view;
    tabs.appendChild(h('button', {
      type: 'button', role: 'tab', class: 'cm-tab', 'data-view': v.id, 'aria-selected': String(on),
      'aria-pressed': String(on), tabindex: on ? '0' : '-1', title: v.hint,
      onclick: () => { if (!on) ctx.navigate(v.id, {}); },
      onkeydown: (e) => {
        if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft') return;
        e.preventDefault();
        const all = [...tabs.querySelectorAll('[role=tab]')];
        const next = all[(all.indexOf(e.currentTarget) + (e.key === 'ArrowRight' ? 1 : -1) + all.length) % all.length];
        next.focus();
      },
    }, icon(v.icon), h('span', { text: v.label })));
  }
  host.appendChild(h('div', { class: 'cm-bar' }, crumbs, tabs));
}

/* ---------------------------------------------------------------- shared panel */

/*
  The details panel the three views share. It states the facts the API sent,
  the relationships with a tier and a checkable location each, and the moves
  this item allows — open it, see it in another view, analyse its impact.
*/
function relList(rows, labelOf, ctx, onPick) {
  if (!rows.length) return h('p', { class: 'cm-muted', text: 'None recorded at this floor.' });
  const ul = h('ul', { class: 'cm-rels' });
  for (const r of rows.slice(0, 12)) {
    ul.appendChild(h('li', null,
      h('div', { class: 'cm-rel-top' },
        h('button', { type: 'button', class: 'cm-link cm-mono', text: labelOf(r.other), onclick: () => onPick(r.other) }),
        h('span', { class: 'cm-count', text: '×' + fmt(r.count) }),
        ctx.tierBadge(r.confidence)),
      r.evidence ? copyLoc(r.evidence.file_path, r.evidence.line, ctx) : null));
  }
  if (rows.length > 12) ul.appendChild(h('li', { class: 'cm-muted', text: 'and ' + fmt(rows.length - 12) + ' more — the Graph view lists them all' }));
  return ul;
}
function copyLoc(path, line, ctx) {
  const where = line ? path + ':' + line : path;
  const label = h('span', { text: where });
  const b = h('button', {
    type: 'button', class: 'cm-loc', title: 'Copy ' + where, 'aria-label': 'Copy ' + where,
    onclick: () => {
      Promise.resolve(ctx.copy(where)).finally(() => {
        b.classList.add('is-copied');
        label.textContent = 'Copied';
        setTimeout(() => { b.classList.remove('is-copied'); label.textContent = where; }, 1200);
      });
    },
  }, label, icon('copy'));
  return b;
}
function section(title, ...kids) {
  return h('section', { class: 'cm-psec' }, title ? h('h4', { text: title }) : null, ...kids);
}
function facts(rows) {
  const dl = h('dl', { class: 'cm-dl' });
  for (const [k, v] of rows) { if (v === null || v === undefined || v === '') continue; dl.appendChild(h('dt', { text: k })); dl.appendChild(h('dd', null, v)); }
  return dl;
}

async function paintDetails(panel, item, ctx, store, current) {
  while (panel.firstChild) panel.removeChild(panel.firstChild);
  panel.hidden = !item;
  if (!item) return;
  const head = (title, sub) => h('header', { class: 'cm-phead' },
    h('div', null, h('h3', { class: 'cm-mono', id: 'cm-details-title', tabindex: '-1', text: title }), sub ? h('p', { class: 'cm-muted', text: sub }) : null),
    btn('', { icon: 'x', variant: 'quiet', aria: 'Close details', onclick: () => ctx.navigate(current, { sel: '' }) }));
  const views = (patch, exclude) => h('div', { class: 'cm-actions' },
    VIEWS.filter((v) => v.id !== exclude && v.id !== 'code').map((v) =>
      btn(v.id === 'map' ? 'Show on graph' : v.label, { icon: v.icon, small: true, onclick: () => ctx.navigate(v.id, patch) })));
  panel.appendChild(h('p', { class: 'cm-muted', text: 'Reading…' }));
  try {
    if (item.type === 'dir') {
      const d = await store.containers();
      const c = (d.containers || []).find((x) => String(x.id) === item.id) || { id: item.id };
      const edges = d.container_edges || [];
      const outs = edges.filter((e) => String(e.from) === item.id).map((e) => ({ other: String(e.to), count: e.count, confidence: e.confidence, evidence: e.evidence })).sort((a, b) => b.count - a.count);
      const ins = edges.filter((e) => String(e.to) === item.id).map((e) => ({ other: String(e.from), count: e.count, confidence: e.confidence, evidence: e.evidence })).sort((a, b) => b.count - a.count);
      const pick = (id) => ctx.navigate(current, selPatch({ type: 'dir', id: id }));
      while (panel.firstChild) panel.removeChild(panel.firstChild);
      panel.append(
        head(item.id, 'Directory'),
        section(null, facts([
          ['Files with symbols', fmt(c.files_with_symbols) + (c.files_matched !== undefined ? ' of ' + fmt(c.files_matched) + ' matched' : '')],
          ['Symbols', fmt(c.symbols) + ' (' + fmt(c.exported_symbols) + ' exported)'],
          ['Languages', (c.languages || []).join(', ')],
        ])),
        section(null, h('div', { class: 'cm-actions' },
          btn('Open its files', { variant: 'primary', small: true, onclick: () => ctx.navigate(current === 'code' ? 'map' : current, { container: item.id, level: 'files', sel: '' }) })),
          views(selPatch(item), current)),
        section('Uses ' + plural(outs.length, 'directory', 'directories'), relList(outs, (x) => x, ctx, pick)),
        section('Used by ' + plural(ins.length, 'directory', 'directories'), relList(ins, (x) => x, ctx, pick)));
    } else if (item.type === 'file') {
      const d = await store.files(item.container);
      const f = (d.files || []).find((x) => String(x.path) === item.id) || { path: item.id };
      const edges = d.file_edges || [];
      const outs = edges.filter((e) => String(e.from) === item.id).map((e) => ({ other: String(e.to), count: e.count, confidence: e.confidence, evidence: e.evidence }));
      const ins = edges.filter((e) => String(e.to) === item.id).map((e) => ({ other: String(e.from), count: e.count, confidence: e.confidence, evidence: e.evidence }));
      const pick = (p) => ctx.navigate(current, selPatch({ type: 'file', id: p, container: item.container }));
      while (panel.firstChild) panel.removeChild(panel.firstChild);
      panel.append(
        head(base(item.id), dirOf(item.id)),
        section(null, facts([
          ['File', copyLoc(item.id, 0, ctx)],
          ['Symbols', fmt(f.symbols) + ' (' + fmt(f.exported_symbols) + ' exported)'],
          ['Connections', fmt(f.degree)],
          ['Language', f.language || ''],
          ['Parse', f.partial_parse ? 'Partial — some symbols may be missing' : null],
        ])),
        section(null, h('div', { class: 'cm-actions' },
          btn('Open its code', { variant: 'primary', small: true, icon: 'code', onclick: () => ctx.navigate('code', { cfile: item.id, cline: '', container: item.container }) }),
          btn('Its symbols on the graph', { small: true, icon: 'graph', onclick: () => ctx.navigate('map', { container: item.container, level: 'symbols', sel: '' }) })),
          views(Object.assign(selPatch(item), { cfile: item.id }), current)),
        section('Uses ' + plural(outs.length, 'file'), relList(outs, base, ctx, pick)),
        section('Used by ' + plural(ins.length, 'file'), relList(ins, base, ctx, pick)));
    } else {
      const d = await store.symbols(item.container);
      const nodes = d.nodes || [];
      const s = nodes.find((x) => String(x.id) === item.id);
      while (panel.firstChild) panel.removeChild(panel.firstChild);
      if (!s) { panel.append(head('Symbol ' + item.id, ''), section(null, h('p', { class: 'cm-muted', text: 'This symbol is not in the ' + (item.container || 'current') + ' answer at this floor.' }))); return; }
      const byId = new Map(nodes.map((x) => [String(x.id), x]));
      const edges = d.edges || [];
      const outs = edges.filter((e) => String(e.src) === item.id).map((e) => ({ other: String(e.dst), count: e.count || 1, confidence: e.confidence, evidence: e.evidence }));
      const ins = edges.filter((e) => String(e.dst) === item.id).map((e) => ({ other: String(e.src), count: e.count || 1, confidence: e.confidence, evidence: e.evidence }));
      const lab = (id) => (byId.get(id) ? byId.get(id).name : id);
      const pick = (id) => { const o = byId.get(id); ctx.navigate(current, selPatch({ type: 'symbol', id: id, container: o ? (o.container || item.container) : item.container })); };
      panel.append(
        head(s.name, (s.kind || 'symbol') + (s.exported ? ' · exported' : '')),
        section(null, facts([
          ['Declared at', copyLoc(s.path, s.line, ctx)],
          ['Proof', ctx.tierBadge(s.confidence)],
          ['Connections', fmt(s.degree)],
        ])),
        section(null, h('div', { class: 'cm-actions' },
          btn('Analyse impact', { variant: 'primary', small: true, icon: 'target', onclick: () => ctx.goImpact(s.fqn || s.name) }),
          current === 'code' ? null : btn('Open its code', { small: true, icon: 'code', onclick: () => ctx.navigate('code', { cfile: s.path, cline: String(s.line || ''), container: item.container, level: 'symbols', sel: 'L:' + item.id }) })),
          views(Object.assign(selPatch(item), { cfile: s.path }), current)),
        section('Used by ' + plural(ins.length, 'symbol'), relList(ins, lab, ctx, pick)),
        section('Uses ' + plural(outs.length, 'symbol'), relList(outs, lab, ctx, pick)));
    }
  } catch (e) {
    while (panel.firstChild) panel.removeChild(panel.firstChild);
    panel.append(head(item.id, ''), section(null, h('p', { class: 'cm-err', text: String(e && e.message ? e.message : e) })));
  }
}

function layoutWithPanel(host) {
  const main = h('div', { class: 'cm-main' });
  const panel = h('aside', { class: 'cm-panel', 'aria-label': 'Details', hidden: true });
  host.appendChild(h('div', { class: 'cm-layout' }, main, panel));
  return { main, panel };
}

/* ---------------------------------------------------------------- TREE */

/*
  The outline. Directories come from the containers rung (every one, not the
  48 the map draws); a directory's files load when it opens and a file's
  symbols when it opens, once per directory. The tree implements the WAI-ARIA
  tree pattern: one tab stop, arrows move and open, Home/End jump.
*/
async function renderTree(host, ctx, store) {
  const q = ctx.q();
  const { main, panel } = layoutWithPanel(host);
  const shell = h('div', { class: 'cm-shell' });
  main.appendChild(shell);
  const filterBox = h('input', { class: 'cm-input', type: 'search', placeholder: 'Filter directories and loaded files', value: q.tq || '', 'aria-label': 'Filter the tree', autocomplete: 'off' });
  const count = h('span', { class: 'cm-muted', 'aria-live': 'polite' });
  const tools = h('div', { class: 'cm-tools', role: 'toolbar', 'aria-label': 'Tree tools' },
    h('label', { class: 'cm-search' }, icon('search'), filterBox), count, h('span', { class: 'cm-grow' }),
    btn('Expand all', { small: true, onclick: () => { walkDirs(root, (n) => open.add(n.key)); paint(); } }),
    btn('Collapse all', { small: true, onclick: () => { open.clear(); open.add('r:'); paint(); } }));
  const headRow = h('div', { class: 'cm-tree-head', 'aria-hidden': 'true' }, h('span', { text: 'Name' }), h('span', { text: 'Symbols' }), h('span', { class: 'cm-c-exp', text: 'Exported' }), h('span', { class: 'cm-c-deg', text: 'Connections' }));
  const tree = h('div', { class: 'cm-tree', role: 'tree', 'aria-label': 'Repository outline' });
  shell.append(tools, headRow, tree, h('div', { class: 'cm-note', id: 'cm-tree-note' }));
  tree.appendChild(h('p', { class: 'cm-muted cm-pad', text: 'Reading the directories…' }));

  let d;
  try { d = await store.containers(); } catch (e) { tree.textContent = ''; tree.appendChild(h('p', { class: 'cm-err cm-pad', text: String(e.message || e) })); return; }
  const containers = d.containers || [];
  const root = { key: 'r:', name: 'Repository', type: 'root', kids: new Map() };
  for (const c of containers) {
    const id = String(c.id);
    const segs = id.startsWith('(') ? [id] : id.split('/');
    let node = root; const acc = [];
    for (const s of segs) {
      acc.push(s);
      if (!node.kids.has(s)) node.kids.set(s, { key: 'p:' + acc.join('/'), name: s, type: 'pkg', kids: new Map() });
      node = node.kids.get(s);
    }
    node.type = 'dir'; node.key = 'd:' + id; node.c = c;
  }
  const totals = (n) => {
    if (n.t) return n.t;
    let sym = 0, exp = 0;
    if (n.c) { sym += Number(n.c.symbols || 0); exp += Number(n.c.exported_symbols || 0); }
    for (const k of n.kids.values()) { const t = totals(k); sym += t.sym; exp += t.exp; }
    n.t = { sym, exp }; return n.t;
  };
  totals(root);
  const filesOf = new Map();   // container id → [file rows] (own files only)
  const symsOf = new Map();    // file path → [symbol rows]
  const loading = new Set();
  const open = new Set((q.topen || 'r:').split(',').filter(Boolean));
  open.add('r:');

  const sel = selection(q);
  const selKey = sel ? (sel.type === 'dir' ? 'd:' + sel.id : sel.type === 'file' ? 'f:' + sel.id : 's:' + sel.id) : '';
  /* Reveal the selection: open every ancestor, and load what it sits in. */
  if (sel) {
    const c = sel.type === 'dir' ? sel.id : sel.container;
    if (c) {
      const segs = c.split('/'); segs.forEach((_, i) => open.add('p:' + segs.slice(0, i + 1).join('/')));
      if (sel.type !== 'dir') { open.add('d:' + c); await loadFiles(c); }
      if (sel.type === 'symbol') {
        await loadSymbols(c);
        const s = (await store.symbols(c)).nodes.find((x) => String(x.id) === sel.id);
        if (s) open.add('f:' + s.path);
      }
    }
  }
  async function loadFiles(c) {
    if (filesOf.has(c)) return;
    const fd = await store.files(c);
    filesOf.set(c, (fd.files || []).filter((f) => !f.external && String(f.container) === c));
  }
  async function loadSymbols(c) {
    if (loading.has('s:' + c)) return;
    loading.add('s:' + c);
    const sd = await store.symbols(c);
    for (const s of sd.nodes || []) {
      if (String(s.container) !== c) continue;
      if (!symsOf.has(s.path)) symsOf.set(s.path, []);
      symsOf.get(s.path).push(s);
    }
    for (const list of symsOf.values()) list.sort((a, b) => (a.line || 0) - (b.line || 0));
  }
  function walkDirs(n, f) { if (n.type !== 'file' && n.type !== 'sym') { f(n); for (const k of n.kids.values()) walkDirs(k, f); } }
  function children(n) {
    if (n.type === 'sym') return [];
    if (n.type === 'file') return (symsOf.get(n.path) || []).map((s) => ({ key: 's:' + s.id, name: s.name, type: 'sym', s, container: n.container }));
    const dirs = [...n.kids.values()].sort((a, b) => a.name.localeCompare(b.name));
    const files = n.c && filesOf.has(String(n.c.id)) ? filesOf.get(String(n.c.id)).map((f) => ({ key: 'f:' + f.path, name: base(f.path), type: 'file', path: f.path, f, container: String(n.c.id), kids: new Map() })).sort((a, b) => a.name.localeCompare(b.name)) : [];
    return [...dirs, ...files];
  }
  const filter = () => filterBox.value.trim().toLowerCase();
  const matches = (n) => n.name.toLowerCase().includes(filter());
  const subtree = (n, depth) => matches(n) || (depth < 8 && children(n).some((k) => subtree(k, depth + 1)));
  const rowsOut = [];
  function walk(n, level) {
    if (filter() && !subtree(n, 0)) return;
    rowsOut.push({ n, level });
    if (filter() || open.has(n.key)) for (const k of children(n)) walk(k, level + 1);
  }
  const maxDeg = () => Math.max(1, ...[...filesOf.values()].flat().map((f) => Number(f.degree || 0)), ...[...symsOf.values()].flat().map((s) => Number(s.degree || 0)));
  const ICON = { root: 'dir', pkg: 'dir', dir: 'dir', file: 'file', sym: 'sym' };

  function paint(focusKey) {
    rowsOut.length = 0;
    walk(root, 1);
    while (tree.firstChild) tree.removeChild(tree.firstChild);
    let hits = 0;
    const md = maxDeg();
    rowsOut.forEach(({ n, level }, i) => {
      if (filter() && matches(n)) hits++;
      const expandable = n.type !== 'sym' && (n.type === 'file' ? Number(n.f.symbols || 0) > 0 : (n.kids.size > 0 || !!n.c));
      const expanded = expandable ? (filter() ? true : open.has(n.key)) : null;
      const t = n.type === 'file' ? { sym: n.f.symbols, exp: n.f.exported_symbols, deg: n.f.degree } : n.type === 'sym' ? { sym: '', exp: n.s.exported ? 'yes' : '', deg: n.s.degree } : { sym: totals(n).sym, exp: totals(n).exp, deg: '' };
      const name = h('span', { class: 'cm-nm' });
      const f = filter(); const at = f ? n.name.toLowerCase().indexOf(f) : -1;
      if (at >= 0) name.append(n.name.slice(0, at), h('mark', { text: n.name.slice(at, at + f.length) }), n.name.slice(at + f.length));
      else name.textContent = n.name;
      const extra = n.type === 'sym' ? [h('span', { class: 'cm-tag', text: n.s.kind }), h('span', { class: 'cm-muted cm-xs', text: 'L' + (n.s.line || '?') })]
        : n.type === 'file' && n.f.partial_parse ? [h('span', { class: 'cm-tag cm-warn', text: 'partial parse' })]
        : n.c && (n.c.languages || []).length ? [h('span', { class: 'cm-muted cm-xs', text: n.c.languages.slice(0, 3).join(', ') })] : [];
      const deg = t.deg ? h('span', { class: 'cm-deg cm-c-deg' }, h('i', { style: { width: Math.max(4, (Number(t.deg) / md) * 64) + 'px' } }), fmt(t.deg)) : h('span', { class: 'cm-c-deg' });
      const row = h('div', {
        role: 'treeitem', class: 'cm-ti', 'data-key': n.key, 'aria-level': String(level),
        'aria-expanded': expanded === null ? null : String(expanded), 'aria-selected': String(n.key === selKey),
        tabindex: n.key === (focusKey || selKey) || (!focusKey && !selKey && i === 0) ? '0' : '-1',
      },
      h('span', { class: 'cm-lab', style: { 'padding-left': ((level - 1) * 16 + 4) + 'px' } },
        h('span', { class: 'cm-tw' }, expandable ? icon('chev') : null), icon(ICON[n.type]), name, ...extra),
      h('span', { class: 'cm-num', text: t.sym === '' ? '' : fmt(t.sym) }),
      h('span', { class: 'cm-num cm-c-exp', text: typeof t.exp === 'number' ? fmt(t.exp) : String(t.exp || '') }),
      deg);
      row.__node = n;
      tree.appendChild(row);
      if (expanded && n.c && !filesOf.has(String(n.c.id))) tree.appendChild(h('div', { class: 'cm-ti-more', style: { 'padding-left': (level * 16 + 24) + 'px' }, text: 'Reading files…' }));
      if (expanded && n.type === 'file' && !symsOf.has(n.path) && loading.has('s:' + n.container)) tree.appendChild(h('div', { class: 'cm-ti-more', style: { 'padding-left': (level * 16 + 24) + 'px' }, text: plural(n.f.symbols, 'symbol') + ' — none above the confidence floor in this directory\'s answer' }));
    });
    count.textContent = filter() ? plural(hits, 'match', 'matches') : '';
    const el = tree.querySelector('[data-key="' + CSS.escape(focusKey || selKey) + '"]');
    if (el) { if (focusKey) el.focus(); el.scrollIntoView({ block: 'nearest' }); }
    const note = document.getElementById('cm-tree-note');
    if (note) note.textContent = plural(containers.length, 'directory', 'directories') + '. Files load when a directory opens; symbols when a file opens. Symbols are those the graph answers at the "' + (ctx.q().floor || 'medium') + '" floor or above.';
  }
  async function toggle(n, force) {
    const want = force === undefined ? !open.has(n.key) : force;
    if (want) open.add(n.key); else open.delete(n.key);
    ctx.set({ topen: [...open].join(',') }, { replace: true });
    paint(n.key);
    if (want && n.c) { await loadFiles(String(n.c.id)); paint(n.key); }
    if (want && n.type === 'file') { await loadSymbols(n.container); paint(n.key); }
  }
  function pick(n) {
    if (n.type === 'pkg' || n.type === 'root') { toggle(n); return; }
    const item = n.type === 'dir' ? { type: 'dir', id: String(n.c.id) } : n.type === 'file' ? { type: 'file', id: n.path, container: n.container } : { type: 'symbol', id: String(n.s.id), container: n.container };
    ctx.set(selPatch(item), { replace: true });
    for (const r of tree.querySelectorAll('[role=treeitem]')) r.setAttribute('aria-selected', String(r === tree.querySelector('[data-key="' + CSS.escape(n.key) + '"]')));
    paintDetails(panel, item, ctx, store, 'tree');
  }
  tree.addEventListener('click', (e) => {
    const r = e.target.closest('[role=treeitem]'); if (!r) return;
    const n = r.__node;
    if (e.target.closest('.cm-tw') && r.hasAttribute('aria-expanded')) { toggle(n); return; }
    pick(n);
    if (r.getAttribute('aria-expanded') === 'false' && n.type !== 'pkg' && n.type !== 'root') toggle(n, true);
  });
  tree.addEventListener('dblclick', (e) => {
    const r = e.target.closest('[role=treeitem]'); if (!r) return;
    const n = r.__node;
    if (n.type === 'sym') ctx.navigate('code', { cfile: n.s.path, cline: String(n.s.line || ''), container: n.container, level: 'symbols', sel: 'L:' + n.s.id });
    if (n.type === 'file') ctx.navigate('code', { cfile: n.path, cline: '', container: n.container });
  });
  tree.addEventListener('keydown', (e) => {
    const r = e.target.closest('[role=treeitem]'); if (!r) return;
    const all = [...tree.querySelectorAll('[role=treeitem]')]; const i = all.indexOf(r);
    const move = (j) => { const t = all[Math.max(0, Math.min(all.length - 1, j))]; all.forEach((x) => { x.tabIndex = -1; }); t.tabIndex = 0; t.focus(); };
    const exp = r.getAttribute('aria-expanded');
    if (e.key === 'ArrowDown') { e.preventDefault(); move(i + 1); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); move(i - 1); }
    else if (e.key === 'Home') { e.preventDefault(); move(0); }
    else if (e.key === 'End') { e.preventDefault(); move(all.length - 1); }
    else if (e.key === 'ArrowRight') { e.preventDefault(); if (exp === 'false') toggle(r.__node, true); else if (exp === 'true') move(i + 1); }
    else if (e.key === 'ArrowLeft') {
      e.preventDefault();
      if (exp === 'true' && !filter()) toggle(r.__node, false);
      else { const lvl = +r.getAttribute('aria-level'); for (let j = i - 1; j >= 0; j--) if (+all[j].getAttribute('aria-level') < lvl) { move(j); break; } }
    }
    else if (e.key === 'Enter') { e.preventDefault(); pick(r.__node); if (e.shiftKey && r.__node.type === 'sym') tree.dispatchEvent(new MouseEvent('dblclick', { bubbles: true })); }
  });
  let tmr;
  filterBox.addEventListener('input', () => { clearTimeout(tmr); tmr = setTimeout(() => { ctx.set({ tq: filterBox.value.trim() }, { replace: true }); paint(); }, 140); });
  paint();
  if (sel) paintDetails(panel, sel, ctx, store, 'tree');
}

/* ---------------------------------------------------------------- TREEMAP */

function squarify(items, x, y, w, h0) {
  const total = items.reduce((s, i) => s + i.v, 0) || 1;
  const out = []; let hgt = h0;
  let rest = items.slice().sort((a, b) => b.v - a.v).map((i) => Object.assign({}, i, { a: (i.v / total) * w * hgt }));
  const worst = (row, s) => { const sum = row.reduce((t, r) => t + r.a, 0); const mx = Math.max(...row.map((r) => r.a)); const mn = Math.min(...row.map((r) => r.a)); return Math.max((s * s * mx) / (sum * sum), (sum * sum) / (s * s * mn)); };
  while (rest.length) {
    const s = Math.min(w, hgt); let row = [rest[0]]; let wv = worst(row, s);
    for (let i = 1; i < rest.length; i++) { const r2 = row.concat(rest[i]); const w2 = worst(r2, s); if (w2 > wv) break; row = r2; wv = w2; }
    const sum = row.reduce((t, r) => t + r.a, 0);
    if (w >= hgt) { const cw = sum / hgt; let cy = y; for (const r of row) { const rh = r.a / cw; out.push(Object.assign(r, { x, y: cy, w: cw, h: rh })); cy += rh; } x += cw; w -= cw; }
    else { const ch = sum / w; let cx = x; for (const r of row) { const rw = r.a / ch; out.push(Object.assign(r, { x: cx, y, w: rw, h: ch })); cx += rw; } y += ch; hgt -= ch; }
    rest = rest.slice(row.length);
  }
  return out;
}
/* Fixed fills, not theme tokens: every cell carries white text, and these
   hold 4.5:1 against it in both themes (computed in UX_PREVIEW_REPORT.md). */
const LANG_FILLS = ['#2249AE', '#14794A', '#8A5600', '#853880', '#007483', '#B3261E'];
const RAMP_LO = [91, 99, 113];   /* #5B6371 */
const RAMP_HI = [34, 73, 174];   /* #2249AE */

async function renderTreemap(host, ctx, store) {
  const q = ctx.q();
  const inDir = !!q.container;
  const { main, panel } = layoutWithPanel(host);
  const shell = h('div', { class: 'cm-shell' });
  main.appendChild(shell);
  const sizeSel = h('select', { class: 'cm-input cm-select', 'aria-label': 'Size by', onchange: (e) => ctx.navigate('treemap', { tmsize: e.target.value === 'symbols' ? '' : e.target.value }) },
    h('option', { value: 'symbols', text: 'Symbols' }), h('option', { value: 'files', text: inDir ? 'Equal' : 'Files' }));
  sizeSel.value = q.tmsize || 'symbols';
  const colorSel = h('select', { class: 'cm-input cm-select', 'aria-label': 'Colour by', onchange: (e) => ctx.navigate('treemap', { tmcolor: e.target.value === 'degree' ? '' : e.target.value }) },
    h('option', { value: 'degree', text: 'Connections' }), h('option', { value: 'exported', text: 'Share exported' }), h('option', { value: 'lang', text: 'Language' }));
  colorSel.value = q.tmcolor || 'degree';
  const legend = h('span', { class: 'cm-legend-inline' });
  shell.append(h('div', { class: 'cm-tools', role: 'toolbar', 'aria-label': 'Treemap tools' },
    h('label', { class: 'cm-field' }, 'Size by', sizeSel), h('label', { class: 'cm-field' }, 'Colour by', colorSel), h('span', { class: 'cm-grow' }), legend));
  const tm = h('div', { class: 'cm-tm', role: 'list', 'aria-label': (inDir ? 'Files of ' + q.container : 'Directories') + ', sized by ' + sizeSel.value });
  const note = h('div', { class: 'cm-note' });
  shell.append(tm, note);
  tm.appendChild(h('p', { class: 'cm-muted cm-pad', text: 'Reading…' }));

  let items;
  try {
    if (inDir) {
      const d = await store.files(q.container);
      items = (d.files || []).filter((f) => !f.external && String(f.container) === q.container).map((f) => ({
        key: 'f:' + f.path, item: { type: 'file', id: String(f.path), container: q.container }, label: base(f.path),
        v: q.tmsize === 'files' ? 1 : Math.max(1, Number(f.symbols || 0)), deg: Number(f.degree || 0),
        exp: f.symbols ? Number(f.exported_symbols || 0) / Number(f.symbols) : 0, lang: f.language,
        sub: plural(f.symbols, 'symbol') + ' · ' + plural(f.degree, 'connection'),
      }));
    } else {
      const d = await store.containers();
      const edges = d.container_edges || [];
      items = (d.containers || []).map((c) => {
        const id = String(c.id);
        const deg = edges.reduce((s, e) => s + (String(e.from) === id || String(e.to) === id ? Number(e.count || 0) : 0), 0);
        return {
          key: 'd:' + id, item: { type: 'dir', id }, label: id,
          v: q.tmsize === 'files' ? Math.max(1, Number(c.files_with_symbols || 0)) : Math.max(1, Number(c.symbols || 0)), deg,
          exp: c.symbols ? Number(c.exported_symbols || 0) / Number(c.symbols) : 0, lang: (c.languages || [])[0],
          sub: plural(c.symbols, 'symbol') + ' · ' + plural(c.files_with_symbols, 'file') + ' · ' + plural(deg, 'use'),
        };
      });
      const nb = ((d.omissions && d.omissions.totals) || []).find((t) => t.reason === 'node_budget');
      if (nb) note.append(fmt(nb.count) + ' of the smallest directories were not sent by the server (node budget). ');
    }
  } catch (e) { tm.textContent = ''; tm.appendChild(h('p', { class: 'cm-err cm-pad', text: String(e.message || e) })); return; }
  note.append(inDir ? plural(items.length, 'file') + ' in ' + q.container + '.' : plural(items.length, 'directory', 'directories') + '.', ' Click for details; double-click or Enter to go inside; Esc goes up.');

  const langs = [...new Set(items.map((i) => i.lang).filter(Boolean))];
  const maxD = Math.max(1, ...items.map((i) => i.deg));
  const mode = q.tmcolor || 'degree';
    const colour = (i) => {
    if (mode === 'lang') return LANG_FILLS[langs.indexOf(i.lang) % LANG_FILLS.length];
    const t = mode === 'exported' ? i.exp : Math.sqrt(i.deg / maxD);
    return 'rgb(' + RAMP_LO.map((lo, k) => Math.round(lo + (RAMP_HI[k] - lo) * t)).join(',') + ')';
  };
  if (mode === 'lang') langs.slice(0, 6).forEach((l) => legend.append(h('span', { class: 'cm-swatch', style: { background: colour({ lang: l }) } }), l, ' '));
  else legend.append('fewer', h('span', { class: 'cm-ramp' }), 'more ' + (mode === 'exported' ? 'exported' : 'connected'));

  const sel = selection(q);
  const selKey = sel ? (sel.type === 'dir' ? 'd:' + sel.id : 'f:' + sel.id) : '';
  /* Redraw only when the box really changed size, and keep focus on the same
     cell across a redraw: a ResizeObserver fires once right after observe(),
     and a redraw that drops focus makes the next key press go nowhere. */
  let drawnAt = '';
  const draw = () => {
    const W = tm.clientWidth, H = tm.clientHeight;
    if (W < 1 || H < 1 || drawnAt === W + 'x' + H) return;
    drawnAt = W + 'x' + H;
    const focused = tm.contains(document.activeElement) && document.activeElement.__c ? document.activeElement.__c.key : '';
    while (tm.firstChild) tm.removeChild(tm.firstChild);
    squarify(items, 0, 0, W, H).forEach((c, i) => {
      const cell = h('div', {
        class: 'cm-cell' + (c.key === selKey ? ' is-sel' : ''), role: 'listitem', tabindex: c.key === selKey || (!selKey && i === 0) ? '0' : '-1',
        'aria-label': c.label + ': ' + c.sub, title: c.label + ' — ' + c.sub,
        style: { left: c.x + 'px', top: c.y + 'px', width: c.w + 'px', height: c.h + 'px', background: colour(c) },
      }, c.w > 60 && c.h > 22 ? h('div', { class: 'cm-cell-t', text: c.label }) : null, c.w > 60 && c.h > 38 ? h('div', { class: 'cm-cell-s', text: c.sub }) : null);
      cell.__c = c;
      tm.appendChild(cell);
      if (c.key === focused) cell.focus();
    });
  };
  const drill = (c) => {
    if (c.item.type === 'dir') ctx.navigate('treemap', { container: c.item.id, level: 'files', sel: '' });
    else ctx.navigate('code', { cfile: c.item.id, cline: '', container: c.item.container });
  };
  tm.addEventListener('click', (e) => {
    const cell = e.target.closest('.cm-cell'); if (!cell) return;
    for (const x of tm.querySelectorAll('.cm-cell')) { x.classList.toggle('is-sel', x === cell); x.tabIndex = x === cell ? 0 : -1; }
    ctx.set(selPatch(cell.__c.item), { replace: true });
    paintDetails(panel, cell.__c.item, ctx, store, 'treemap');
  });
  tm.addEventListener('dblclick', (e) => { const cell = e.target.closest('.cm-cell'); if (cell) drill(cell.__c); });
  tm.addEventListener('keydown', (e) => {
    const cell = e.target.closest('.cm-cell'); if (!cell) return;
    const all = [...tm.querySelectorAll('.cm-cell')]; const i = all.indexOf(cell);
    const move = (j) => { const t = all[Math.max(0, Math.min(all.length - 1, j))]; all.forEach((x) => { x.tabIndex = -1; }); t.tabIndex = 0; t.focus(); };
    if (e.key === 'ArrowRight' || e.key === 'ArrowDown') { e.preventDefault(); move(i + 1); }
    else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') { e.preventDefault(); move(i - 1); }
    else if (e.key === 'Enter') { e.preventDefault(); drill(cell.__c); }
    else if (e.key === ' ') { e.preventDefault(); cell.click(); }
  });
  /* Esc goes up from anywhere in the view, not only from a focused cell. */
  main.parentElement.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape' || !inDir || e.target.closest('input, select, textarea')) return;
    e.preventDefault();
    ctx.navigate('treemap', { container: '', level: '', sel: '' });
  });
  draw();
  new ResizeObserver(draw).observe(tm);
  if (sel && (sel.type === 'dir' || sel.type === 'file')) paintDetails(panel, sel, ctx, store, 'treemap');
}

/* ---------------------------------------------------------------- CODE */

/*
  A deliberately small highlighter: comments, strings and keywords for the
  C-family syntax most indexed code uses. It colours; it does not parse, and a
  wrong colour costs nothing while a wrong line number would cost trust — the
  line numbers come from the server.
*/
const KEYWORDS = new Set(('func return if else for range switch case default break continue go defer select chan map struct interface type var const package import nil true false ' +
  'class extends implements public private protected static final new this super void int long boolean try catch throw throws function let async await export from def self None True False lambda yield with as in not and or is pass raise').split(' '));
function highlightLine(line) {
  const out = h('span', { class: 'cm-tx' });
  let code = line; let comment = '';
  const m = /(\/\/|#(?!include)|--\s)/.exec(line);
  if (m && (line.slice(0, m.index).split('"').length % 2 === 1)) { code = line.slice(0, m.index); comment = line.slice(m.index); }
  for (const part of code.split(/("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|`[^`]*`)/)) {
    if (!part) continue;
    if (/^["'`]/.test(part)) { out.appendChild(h('span', { class: 'cm-str', text: part })); continue; }
    for (const tok of part.split(/(\b[A-Za-z_][A-Za-z0-9_]*\b)/)) {
      if (!tok) continue;
      if (KEYWORDS.has(tok)) out.appendChild(h('span', { class: 'cm-kw', text: tok }));
      else if (/^[A-Z][A-Za-z0-9_]*$/.test(tok)) out.appendChild(h('span', { class: 'cm-ty', text: tok }));
      else out.appendChild(document.createTextNode(tok));
    }
  }
  if (comment) out.appendChild(h('span', { class: 'cm-com', text: comment }));
  if (!line) out.appendChild(document.createTextNode(' '));
  return out;
}

async function renderCode(host, ctx, store) {
  const q = ctx.q();
  const { main, panel } = layoutWithPanel(host);
  const shell = h('div', { class: 'cm-shell' });
  main.appendChild(shell);
  if (!q.cfile) {
    shell.appendChild(h('div', { class: 'cm-empty' }, icon('code'), h('h3', { text: 'Pick a file to read' }),
      h('p', { text: 'Open a file from the Tree or the Treemap, or a symbol from the Graph — its declaration opens here with every relationship beside the line it starts from.' }),
      h('div', { class: 'cm-actions' }, btn('Open the Tree', { variant: 'primary', icon: 'tree', onclick: () => ctx.navigate('tree', {}) }), btn('Open the Treemap', { icon: 'treemap', onclick: () => ctx.navigate('treemap', {}) }))));
    return;
  }
  shell.appendChild(h('p', { class: 'cm-muted cm-pad', text: 'Reading ' + q.cfile + '…' }));
  let d;
  try { d = await store.source(q.cfile, 1); } catch (e) { shell.textContent = ''; shell.appendChild(h('p', { class: 'cm-err cm-pad', text: String(e.message || e) })); return; }
  while (shell.firstChild) shell.removeChild(shell.firstChild);
  if (d.guidance) {
    shell.appendChild(h('div', { class: 'cm-empty' }, icon('code'), h('h3', { text: d.guidance.title }), h('p', { text: d.guidance.detail || '' }),
      (d.guidance.steps || []).length ? h('ul', null, d.guidance.steps.map((s) => h('li', { class: 'cm-mono', text: s }))) : null));
    return;
  }
  const container = d.container || q.container || '';
  const decls = d.declarations || [];
  const evidence = new Map((d.evidence_lines || []).map((e) => [e.line, e.count]));
  const sel = selection(q);
  const selDecl = sel && sel.type === 'symbol' ? decls.find((x) => String(x.id) === sel.id) : null;
  /* The index records locals too (`variable`, `field`, `parameter`). They
     neither end a declaration's range nor crowd the outline by default. */
  const minor = (x) => /^(variable|field|parameter|local|property)$/.test(String(x.kind || ''));
  const nextLine = (dl) => { const later = decls.filter((x) => x.line > dl.line && !minor(x)).map((x) => x.line); return later.length ? Math.min(...later) - 1 : d.to; };
  const range = selDecl ? [selDecl.line, nextLine(selDecl)] : null;
  const target = Number(q.cline || 0) || (selDecl && selDecl.line) || 0;

  const find = h('input', { class: 'cm-input', type: 'search', placeholder: 'Find in file', 'aria-label': 'Find in file', autocomplete: 'off' });
  const findN = h('span', { class: 'cm-muted', 'aria-live': 'polite' });
  shell.appendChild(h('div', { class: 'cm-tools', role: 'toolbar', 'aria-label': 'Code tools' },
    copyLoc(d.path, target || 0, ctx),
    h('span', { class: 'cm-muted cm-xs', text: (d.language || '') + ' · lines ' + fmt(d.from) + '–' + fmt(d.to) }),
    h('span', { class: 'cm-grow' }),
    h('label', { class: 'cm-search' }, icon('search'), find), findN,
    container ? btn('Graph of this file\'s directory', { small: true, icon: 'graph', onclick: () => ctx.navigate('map', { container, level: 'symbols', sel: selDecl ? 'L:' + selDecl.id : '' }) }) : null));

  const minors = decls.filter(minor).length;
  const showMinor = q.cminor === '1' || (selDecl && minor(selDecl));
  const outline = h('nav', { class: 'cm-outline', 'aria-label': 'Declarations in ' + base(d.path) },
    h('h4', { text: plural(decls.length - minors, 'declaration') + (minors ? ' and ' + plural(minors, 'local') : '') }));
  if (minors) {
    outline.appendChild(h('div', { class: 'cm-pad-sm' }, btn(showMinor ? 'Hide locals' : 'Show ' + plural(minors, 'local'), {
      small: true, variant: 'quiet', pressed: !!showMinor,
      onclick: () => ctx.navigate('code', { cminor: showMinor ? '' : '1' }),
    })));
  }
  for (const dl of decls) {
    if (minor(dl) && !showMinor) continue;
    outline.appendChild(h('button', {
      type: 'button', 'aria-current': String(selDecl === dl), title: plural(dl.callers, 'caller') + ', ' + plural(dl.callees, 'callee'),
      onclick: () => ctx.navigate('code', { cline: String(dl.line), level: 'symbols', container, sel: 'L:' + dl.id }),
    }, h('span', { class: 'cm-tag', title: dl.kind, text: kindShort(dl.kind) }), h('span', { class: 'cm-mono', text: dl.name }),
    h('span', { class: 'cm-muted cm-xs cm-right', text: dl.callers + '↓ ' + dl.callees + '↑' })));
  }
  if (!decls.length) outline.appendChild(h('p', { class: 'cm-muted cm-pad', text: 'No declarations recorded in this file.' }));

  const src = h('div', { class: 'cm-src', tabindex: '0', role: 'region', 'aria-label': 'Source of ' + d.path });
  const declAt = new Map(decls.map((x) => [x.line, x]));
  (d.lines || []).forEach((line, i) => {
    const no = d.from + i;
    const dl = declAt.get(no); const ev = evidence.get(no);
    const gut = dl
      ? h('button', { type: 'button', class: 'cm-gut cm-gut-decl', title: dl.name + ': ' + plural(dl.callers, 'caller') + ', ' + plural(dl.callees, 'callee'), 'aria-label': 'Details for ' + dl.name, onclick: () => ctx.navigate('code', { cline: String(no), level: 'symbols', container, sel: 'L:' + dl.id }) })
      : ev ? h('span', { class: 'cm-gut cm-gut-ev', title: plural(ev, 'recorded relationship') + ' cite this line' }) : h('span', { class: 'cm-gut' });
    src.appendChild(h('div', { class: 'cm-ln' + (range && no >= range[0] && no <= range[1] ? ' is-hl' : '') + (dl ? ' is-decl' : '') + (no === target && !selDecl ? ' is-flash' : ''), id: 'cm-L' + no },
      gut, h('span', { class: 'cm-no', text: String(no) }), highlightLine(line)));
  });
  if (d.has_more) src.appendChild(h('div', { class: 'cm-ti-more', text: 'The file continues past line ' + fmt(d.to) + ' — the server sends ' + fmt(d.lines.length) + ' lines per request.' }));

  shell.appendChild(h('div', { class: 'cm-code' }, outline, src));
  shell.appendChild(h('div', { class: 'cm-note' },
    'This is the text the index holds — the lines every relationship was extracted from — as of the indexed revision. ',
    'Filled markers are declarations (callers↓ callees↑ at the "' + (d.min_confidence || q.floor || 'medium') + '" floor); hollow markers are lines a relationship cites.'));
  if (target) { const row = src.querySelector('#cm-L' + target); if (row) requestAnimationFrame(() => { src.scrollTop = row.offsetTop - src.offsetTop - 80; }); }

  const lines = d.lines || [];
  let hits = []; let at = 0;
  const jump = () => { const row = src.querySelector('#cm-L' + hits[at]); if (row) src.scrollTop = row.offsetTop - src.offsetTop - 80; findN.textContent = hits.length ? (at + 1) + ' of ' + hits.length : 'no match'; };
  find.addEventListener('input', () => {
    for (const r of src.querySelectorAll('.cm-ln.is-match')) r.classList.remove('is-match');
    const needle = find.value.trim().toLowerCase();
    hits = needle ? lines.map((l, i) => (l.toLowerCase().includes(needle) ? d.from + i : 0)).filter(Boolean) : []; at = 0;
    for (const n of hits) { const r = src.querySelector('#cm-L' + n); if (r) r.classList.add('is-match'); }
    if (needle) jump(); else findN.textContent = '';
  });
  find.addEventListener('keydown', (e) => { if (e.key === 'Enter' && hits.length) { e.preventDefault(); at = (at + (e.shiftKey ? hits.length - 1 : 1)) % hits.length; jump(); } });
  if (selDecl && container) paintDetails(panel, { type: 'symbol', id: String(selDecl.id), container }, ctx, store, 'code');
}

/* ---------------------------------------------------------------- entry */

let STORE = null;
let STORE_FLOOR = '';

/*
  Renders one of the three non-canvas views into its host. The store is
  rebuilt when the floor or the committed run changes, so every number on
  screen is at the floor the top bar says, from the run the bar names.
*/
export async function renderView(view, host, ctx) {
  // Keyed by the committed run as well as the floor: a reindex from the
  // portal (--allow-refresh) must not be answered from the previous run.
  const floor = (ctx.q().floor || 'medium') + '@' + (ctx.runId ? ctx.runId() : '');
  if (!STORE || STORE_FLOOR !== floor) { STORE = makeStore(ctx); STORE_FLOOR = floor; }
  while (host.firstChild) host.removeChild(host.firstChild);
  if (view === 'tree') return renderTree(host, ctx, STORE);
  if (view === 'treemap') return renderTreemap(host, ctx, STORE);
  if (view === 'code') return renderCode(host, ctx, STORE);
  return undefined;
}

export { icon as codeMapIcon };
