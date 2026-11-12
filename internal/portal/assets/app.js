/*
  RepoCortex portal (next).

  THE PAGE HAS NO DATA ROUTES OF ITS OWN. Everything it shows is the answer to
  a tool call on /api/v1/tools/{name}: the same tools, arguments and answers an
  agent gets. A page and an agent therefore cannot disagree, and the "ask your
  agent the same thing" command beside each answer is the call that produced it.

  One consequence shapes the code: every tool returns the same envelope
  ({summary, items, notes, truncated}), so one renderer draws every answer and
  a view is mostly a list of which tools to call.

  No inline script or style anywhere: the Content-Security-Policy forbids both.
  Dimensions are set through the DOM, which it permits.
*/

const API = '/api/v1';

// ---------------------------------------------------------------- dom

/* h builds an element. A string child is text, never markup, so nothing read
   from the index can be interpreted as HTML. */
function h(tag, attrs, ...kids) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    node.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
  return node;
}
const $ = (sel, root = document) => root.querySelector(sel);
const view = () => $('#view');

function toast(message) {
  const t = $('#toast');
  t.textContent = message;
  t.classList.add('on');
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => t.classList.remove('on'), 1600);
}

// ---------------------------------------------------------------- api

async function call(tool, args = {}) {
  const res = await fetch(`${API}/tools/${tool}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify(args),
  });
  if (!res.ok) {
    let message = `The server answered ${res.status}.`;
    try { message = (await res.json()).error.message; } catch { /* keep the status */ }
    throw new Error(message);
  }
  const body = await res.json();
  return { tool, args, text: body.text, failed: body.is_error, ...(body.result || {}) };
}

/* The command a person can hand to their agent, or run, for the same answer. */
function commandFor(tool, args) {
  const parts = Object.entries(args).filter(([, v]) => v !== '' && v != null && v !== false)
    .map(([k, v]) => (/^[\w./:@-]+$/.test(String(v)) ? `${k}=${v}` : `${k}='${String(v).replace(/'/g, "'\\''")}'`));
  return `repocortex tool call ${tool}${parts.length ? ' ' + parts.join(' ') : ''}`;
}

// ---------------------------------------------------------------- vocabulary

/* The four tiers, in the reader's words. The tool says exact/high/medium/low;
   a person deciding whether to act needs to know what each lets them do. */
const TIERS = {
  exact:  { label: 'proven',      use: 'A compiler proved it. Safe to act on without checking.' },
  high:   { label: 'very likely', use: 'Bound from the syntax tree and imports. Act on it; it is rarely wrong.' },
  medium: { label: 'uncertain',   use: 'One of several candidates. Check it before relying on it.' },
  low:    { label: 'a guess',     use: 'A name match only. Do not act on it alone.' },
};

function tierMark(confidence) {
  const t = TIERS[confidence];
  if (!t) return h('span', { class: 'kindtag', text: 'not graded' });
  return h('span', { class: 'tier', title: t.use },
    h('span', { class: `sw fill-${confidence}`, 'aria-hidden': 'true' }), t.label);
}

const KIND_WORDS = {
  changed: 'changed here', reached: 'within reach',
  calls: 'calls', references: 'refers to', imports: 'imports', subtype: 'built on it', supertype: 'builds on',
  changes_with: 'changes with', file: 'file', test: 'test',
};
const kindWord = (k) => KIND_WORDS[k] || String(k || '').replace(/_/g, ' ');

// ---------------------------------------------------------------- components

function copyable(text) {
  return h('div', { class: 'cmd' },
    h('code', { text }),
    h('button', { type: 'button', onclick: async () => {
      try { await navigator.clipboard.writeText(text); toast('Copied'); }
      catch { toast('Copy failed. Select the text instead.'); }
    } }, 'Copy'));
}

function symbolLink(name, path) {
  const q = path ? `?path=${encodeURIComponent(path)}` : '';
  return h('a', { class: 'name', href: `#/s/${encodeURIComponent(name)}${q}`, text: name });
}

/* A name is linked to its page when it looks like one symbol. "A -> B" (a hop
   on a route) and file rows are shown as text. */
function nameCell(item) {
  const name = item.name || '(unnamed)';
  const linkable = /^[\w$.#:<>~-]+$/.test(name) && !['file', 'changes_with', 'hop'].includes(item.kind);
  return linkable ? symbolLink(name) : h('span', { class: 'name', text: name });
}

function itemsTable(items, { showWhy = true } = {}) {
  const graded = items.some((i) => i.confidence);
  return h('div', { class: 'table-wrap' }, h('table', null,
    h('thead', null, h('tr', null,
      h('th', { text: 'Name' }), h('th', { text: 'Relationship' }),
      graded && h('th', { text: 'How sure' }),
      h('th', { text: 'Where' }),
      showWhy && h('th', { text: 'How it was found' }))),
    h('tbody', null, items.map((i) => h('tr', null,
      h('td', null, nameCell(i)),
      h('td', null, h('span', { class: 'kindtag', text: kindWord(i.kind) })),
      graded && h('td', null, i.confidence ? tierMark(i.confidence) : ''),
      h('td', null, h('span', { class: 'where', text: i.path ? (i.line ? `${i.path}:${i.line}` : i.path) : '' })),
      showWhy && h('td', null, h('span', { class: 'why', text: i.why || '' })))))));
}

/* Notes that say what an answer cannot see come first: they change how far
   the rows above can be trusted, and everything else is detail. */
const isLimit = (n) => /NOT VISIBLE|LOWER BOUND|COVERAGE|not proof|cannot see|unresolved|truncat/i.test(n);

function limitsNote(answer) {
  const notes = [...(answer.notes || [])];
  if (answer.truncated && answer.truncated_reason) notes.unshift('Cut short: ' + answer.truncated_reason + '.');
  const limits = notes.filter(isLimit);
  const rest = notes.filter((n) => !isLimit(n));
  const box = h('div', { class: 'note caution' }, h('h3', { text: 'What this answer cannot see' }));
  if (!limits.length) {
    box.append(h('p', { text: 'The index reported no gaps for this answer. An empty list still means nothing was found, never that nothing exists.' }));
  }
  const shown = limits.slice(0, 2);
  const hidden = limits.slice(2).concat(rest);
  shown.forEach((n) => box.append(h('p', { text: n })));
  if (hidden.length) {
    const more = h('button', { class: 'linklike', type: 'button', onclick: () => {
      hidden.forEach((n) => box.insertBefore(h('p', { text: n }), more));
      more.remove();
    } }, `Show ${hidden.length} more note${hidden.length > 1 ? 's' : ''}`);
    box.append(more);
  }
  return box;
}

function askNote(answer) {
  return h('div', { class: 'note' },
    h('h3', { text: 'Ask your agent the same thing' }),
    h('p', { text: `It gets this answer from one call to ${answer.tool}.` }),
    copyable(commandFor(answer.tool, answer.args)));
}

function loading(what) {
  return h('div', { class: 'state' }, h('p', { class: 'busy', text: what }));
}

function failure(what, err) {
  return h('div', { class: 'state bad' },
    h('h2', { text: `${what} could not be loaded` }),
    h('p', { text: err.message }),
    h('p', { text: 'If the index is missing or out of date, run repocortex index and repocortex resolve, then reload this page.' }));
}

/* An answer with no rows. The tool's own summary and notes say what that means
   and what to try; this adds nothing of its own. */
function emptyState(answer) {
  return h('div', { class: 'state' },
    h('h2', { text: answer.summary || 'Nothing found' }),
    (answer.notes || []).slice(0, 2).map((n) => h('p', { text: n })));
}

/* answerBlock draws one tool's answer: its sentence, its rows, its limits. */
function answerBlock(answer, opts = {}) {
  const items = answer.items || [];
  // With rows, the summary introduces them. Without, the summary IS the
  // answer and is shown once, as the heading of the empty state.
  const main = items.length
    ? h('div', null, h('p', { class: 'sub', text: answer.summary || '' }), itemsTable(items, opts))
    : h('div', null, emptyState(answer));
  return h('div', { class: 'with-aside' }, main, h('aside', null, limitsNote(answer), askNote(answer)));
}

/* section loads a tool and replaces its own placeholder with the answer. */
function liveSection(title, tool, args, opts = {}) {
  const body = h('div', null, loading(`Reading ${title.toLowerCase()}`));
  call(tool, args)
    .then((answer) => body.replaceChildren(answerBlock(answer, opts)))
    .catch((err) => body.replaceChildren(failure(title, err)));
  return h('section', { class: 'section' }, h('h2', { text: title }), opts.intro && h('p', { class: 'sub', text: opts.intro }), body);
}

function taskRows(rows) {
  return h('ul', { class: 'rows' }, rows.map(([href, title, detail]) =>
    h('li', null, h('a', { class: 'row-link', href }, h('span', { class: 't', text: title }), h('span', { class: 's', text: detail })))));
}

// ---------------------------------------------------------------- recents

const RECENT_KEY = 'repocortex.recent';
function recents() {
  try { return JSON.parse(localStorage.getItem(RECENT_KEY)) || []; } catch { return []; }
}
function remember(name) {
  try {
    const next = [name, ...recents().filter((n) => n !== name)].slice(0, 6);
    localStorage.setItem(RECENT_KEY, JSON.stringify(next));
  } catch { /* storage is a convenience; the page works without it */ }
}

// ---------------------------------------------------------------- views

function homeView() {
  const project = $('#project').textContent;
  // Each line of the index's state is asked for and drawn on its own. One of
  // the three checks takes seconds on a large index, and holding the other
  // two back for it would leave the reader looking at a spinner for facts
  // that arrived at once.
  const count = (n) => Number(n).toLocaleString('en-US');
  const statusRow = (tool, href, waiting, title, detail) => {
    const li = h('li', { class: 'plain-row busy', text: waiting });
    call(tool).then((a) => {
      li.className = '';
      li.replaceChildren(h('a', { class: 'row-link', href },
        h('span', { class: 't', text: title(a.summary || '') || a.summary }),
        h('span', { class: 's', text: detail })));
    }).catch(() => { li.className = 'plain-row'; li.textContent = `${waiting}: could not be read. Run repocortex index, then reload.`; });
    return li;
  };
  const attention = h('ul', { class: 'rows' },
    statusRow('get_project_overview', '#/explore', 'Counting what is indexed', (t) => {
      const m = /(\d+) files, (\d+) symbols, (\d+) relationships/.exec(t);
      return m && `${count(m[1])} files, ${count(m[2])} definitions and ${count(m[3])} relationships are indexed`;
    }, 'See the routes it exposes and the tables it uses.'),
    statusRow('get_resolution_coverage', '#/health', 'Measuring how complete the index is', (t) => {
      const m = /\(([\d.]+)%\)/.exec(t);
      return m && `${Math.round(Number(m[1]))}% of the references the index could connect are connected`;
    }, 'Every answer here is only as complete as this. The rest are listed as what the index could not see.'),
    statusRow('list_blindspots', '#/health', 'Looking for what the index could not see', (t) => {
      const m = /^(\d+)/.exec(t);
      return m && `${count(m[1])} things the index could not see`;
    }, 'Mostly calls into libraries. Each kind is listed with what fixes it.'));

  const recent = recents();
  return [
    h('h1', { class: 'page-title', text: `What do you want to know about ${project}?` }),
    h('p', { class: 'lede', text: 'Search for a function, type or method. Its page shows what calls it, what a change to it can reach, and which tests to run.' }),
    h('div', { class: 'actions' },
      h('button', { class: 'btn', type: 'button', onclick: () => $('#q').focus() }, 'Search the code'),
      h('a', { class: 'btn quiet', href: '#/change' }, 'Plan a change')),

    recent.length && h('section', { class: 'section' },
      h('h2', { text: 'Pick up where you left off' }),
      h('ul', { class: 'rows' }, recent.map((n) => h('li', { class: 'plain-row' }, symbolLink(n))))),

    h('section', { class: 'section' }, h('h2', { text: 'The state of this index' }), attention),

    h('section', { class: 'section' },
      h('h2', { text: 'How to read an answer' }),
      h('p', { class: 'sub', text: 'Every relationship is marked with how it was established. The mark tells you what you can do with it.' }),
      h('ul', { class: 'legend' }, Object.entries(TIERS).map(([key, t]) => h('li', null,
        h('span', { class: `sw fill-${key}`, 'aria-hidden': 'true' }),
        h('span', { class: 'lab', text: t.label }),
        h('span', { class: 'use', text: t.use }))))),

    h('section', { class: 'section' },
      h('h2', { text: 'Start from a task' }),
      h('div', { class: 'cols' },
        taskRows([['#/change', 'Plan a change safely', 'The edits in order, the callers to update and the tests that guard each.'],
          ['#/health', 'Find what to clean up', 'Where change concentrates, and what looks unused.']]),
        taskRows([['#/explore', 'Find your way around', 'The routes this service exposes and the tables it uses.'],
          ['#/health', 'Check what the index missed', 'Calls it could not connect, and how to connect them.']]),
        taskRows([['#/agents', 'Connect your coding agent', 'One command for Claude Code, Cursor and the rest.'],
          ['#/agents', 'See what each tool costs', 'The context every exposed tool spends, per request.']]))),
  ];
}

function searchView({ q }) {
  const results = h('div', null, loading(`Searching for ${q}`));
  Promise.allSettled([call('find_symbol', { name: q }), call('search_code', { query: q, limit: 15 })])
    .then(([exact, text]) => {
      const out = [];
      const defs = exact.status === 'fulfilled' ? (exact.value.items || []) : [];
      const hits = text.status === 'fulfilled' ? (text.value.items || []) : [];
      if (defs.length) {
        out.push(h('section', { class: 'section' },
          h('h2', { text: `Defined as ${q}` }),
          h('div', { class: 'table-wrap' }, h('table', null,
            h('thead', null, h('tr', null, h('th', { text: 'Name' }), h('th', { text: 'Kind' }), h('th', { text: 'Where' }))),
            h('tbody', null, defs.map((i) => h('tr', null,
              h('td', null, symbolLink(i.name || q, i.path)),
              h('td', null, h('span', { class: 'kindtag', text: i.kind || '' })),
              h('td', null, h('span', { class: 'where', text: `${i.path || ''}${i.line ? ':' + i.line : ''}` })))))))));
      }
      if (hits.length) {
        out.push(h('section', { class: 'section' },
          h('h2', { text: 'Mentioned in' }),
          h('p', { class: 'sub', text: 'Files whose text matches. A text match is not a relationship: it says the words appear, nothing more.' }),
          h('ul', { class: 'rows' }, hits.map((i) => h('li', { class: 'plain-row' },
            h('span', { class: 'name', text: i.path || i.name || '' }),
            i.why && h('span', { class: 'why', text: ' ' + i.why }))))));
      }
      if (!out.length) {
        const answer = exact.status === 'fulfilled' ? exact.value : { summary: `Nothing named ${q} was found.`, notes: [] };
        out.push(emptyState(answer));
        out.push(h('p', { class: 'sub', text: 'Names are matched exactly, including case. Try the exact name of a function or type, or a word that appears in the code.' }));
      }
      results.replaceChildren(...out);
    });
  return [h('h1', { class: 'page-title', text: `Results for ${q}` }), results];
}

const SYMBOL_TABS = [
  { id: 'callers', label: 'Callers', tool: 'find_callers', arg: 'name', floor: true, lede: 'What calls it. Each row is a call site.' },
  { id: 'calls', label: 'Calls', tool: 'find_callees', arg: 'name', floor: true, lede: 'What it calls, inside this repository.' },
  { id: 'reach', label: 'What could break', tool: 'analyze_impact', arg: 'symbol', floor: true, lede: 'Everything a change to it can reach, nearest first.' },
  { id: 'tests', label: 'Tests', tool: 'find_tests', arg: 'symbol', lede: 'Tests that reach it. Run the ones found through a call first.' },
  { id: 'types', label: 'Type hierarchy', tool: 'find_implementations', arg: 'name', lede: 'What it implements or extends, and what is built on it.' },
];

function symbolView({ name, path, tab = 'callers', floor = '' }) {
  remember(name);
  const header = h('div', null, h('p', { class: 'crumb busy', text: 'Looking it up' }));
  const verdict = h('p', { class: 'verdict' });
  const panel = h('div', { role: 'tabpanel', id: 'tabpanel' });
  const cache = new Map();

  call('find_symbol', { name }).then((a) => {
    const def = (a.items || []).find((i) => !path || (i.path || '').includes(path)) || (a.items || [])[0];
    header.replaceChildren(
      h('p', { class: 'crumb', text: def ? `${def.path}${def.line ? ':' + def.line : ''}` : 'Not found in the index' }),
      h('div', { class: 'entity' }, h('h1', { text: name }), def && def.kind && h('span', { class: 'tag', text: def.kind }),
        (a.items || []).length > 1 && h('span', { class: 'tag', text: `${a.items.length} definitions share this name` })));
    if (!def) verdict.textContent = a.summary || '';
  }).catch((err) => header.replaceChildren(failure('This symbol', err)));

  // Every tab's answer is asked for at once. Each is a few milliseconds, and
  // it puts a count on each tab, so the reader sees the size of an answer
  // before choosing to read it. The promises are kept and reused by the tab.
  const preload = (t) => {
    const a = { [t.arg]: name };
    if (path && t.arg === 'name') a.path = path;
    return call(t.tool, a);
  };
  for (const t of SYMBOL_TABS) cache.set(t.id, preload(t));

  // The one-sentence answer: the judgement from analyze_impact, and how many
  // tests reach the symbol.
  Promise.allSettled([cache.get('reach'), cache.get('tests')]).then(([reach, tests]) => {
    if (reach.status !== 'fulfilled') return;
    const m = /(\d+) direct, (\d+) indirect\. Risk (\w+)/.exec(reach.value.summary || '');
    if (!m) { if (!verdict.textContent) verdict.textContent = reach.value.summary || ''; return; }
    const [, direct, indirect, risk] = m;
    const parts = ['Used directly in ', h('b', { text: `${direct} place${direct === '1' ? '' : 's'}` }),
      '. A change can reach ', h('b', { text: `${Number(direct) + Number(indirect)} symbols` })];
    const n = tests.status === 'fulfilled' ? (tests.value.items || []).length : 0;
    if (n) parts.push(', and ', h('b', { text: `${n}${tests.value.truncated ? '+' : ''} test${n === 1 ? '' : 's'}` }), ' reach it');
    parts.push(`. Risk is ${risk}.`);
    verdict.replaceChildren(...parts);
  });

  const tabs = h('div', { class: 'tabs', role: 'tablist', 'aria-label': `Views of ${name}` },
    SYMBOL_TABS.map((t) => h('button', {
      role: 'tab', type: 'button', id: `tab-${t.id}`, 'aria-controls': 'tabpanel',
      'aria-selected': String(t.id === tab), tabindex: t.id === tab ? '0' : '-1',
      onclick: () => go({ tab: t.id }),
    }, t.label, h('span', { class: 'c', id: `count-${t.id}` }))));
  for (const t of SYMBOL_TABS) {
    cache.get(t.id).then((a) => {
      const slot = $(`#count-${t.id}`);
      if (slot) slot.textContent = `${(a.items || []).length}${a.truncated ? '+' : ''}`;
    }).catch(() => {});
  }
  // Arrow keys move between tabs, as the tab pattern expects.
  tabs.addEventListener('keydown', (e) => {
    if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft') return;
    const i = SYMBOL_TABS.findIndex((t) => t.id === tab);
    const next = SYMBOL_TABS[(i + (e.key === 'ArrowRight' ? 1 : SYMBOL_TABS.length - 1)) % SYMBOL_TABS.length];
    go({ tab: next.id }, true);
  });

  function go(change, focusTab) {
    const p = new URLSearchParams();
    if (path) p.set('path', path);
    const next = { tab, floor, ...change };
    if (next.tab !== 'callers') p.set('tab', next.tab);
    if (next.floor) p.set('floor', next.floor);
    const qs = p.toString();
    sessionStorage.setItem('repocortex.focusTab', focusTab ? next.tab : '');
    location.hash = `#/s/${encodeURIComponent(name)}${qs ? '?' + qs : ''}`;
  }

  const spec = SYMBOL_TABS.find((t) => t.id === tab) || SYMBOL_TABS[0];
  const args = { [spec.arg]: name };
  if (path && spec.arg === 'name') args.path = path;
  if (spec.floor && floor) args.min_confidence = floor;

  const filter = spec.floor && h('div', { class: 'seg', role: 'group', 'aria-label': 'Which relationships to show' },
    h('button', { type: 'button', 'aria-pressed': String(!floor), onclick: () => go({ floor: '' }) }, 'Everything'),
    h('button', { type: 'button', 'aria-pressed': String(floor === 'high'), onclick: () => go({ floor: 'high' }) }, 'Very likely or better'));

  panel.setAttribute('aria-labelledby', `tab-${spec.id}`);
  panel.append(h('div', { class: 'panel-head' }, h('p', { text: spec.lede }), filter), loading('Reading the index'));
  // Unfiltered, the tab shows the answer already fetched for its count.
  (floor ? call(spec.tool, args) : cache.get(spec.id))
    .then((a) => { panel.lastChild.replaceWith(answerBlock(a)); })
    .catch((err) => { panel.lastChild.replaceWith(failure(spec.label, err)); });

  queueMicrotask(() => {
    const want = sessionStorage.getItem('repocortex.focusTab');
    if (want) { sessionStorage.removeItem('repocortex.focusTab'); const b = $(`#tab-${want}`); if (b) b.focus(); }
  });
  return [header, verdict, tabs, panel];
}

function exploreView() {
  return [
    h('h1', { class: 'page-title', text: 'How this codebase is laid out' }),
    h('p', { class: 'lede', text: 'What it exposes and what it stores. Follow any name to its page.' }),
    liveSection('Routes it exposes', 'list_api_endpoints', {}, { intro: 'Each HTTP route the index found, with the function that handles it.' }),
    liveSection('Tables it uses', 'list_db_tables', {}, { intro: 'Each table the code reads or writes.' }),
  ];
}

function changeView({ symbol = '', base = '', head = '' }) {
  // Two ways in, because a change arrives two ways: as a branch someone has
  // already written, or as one thing someone is about to touch.
  const baseInput = h('input', { type: 'search', id: 'change-base', value: base || 'main', autocomplete: 'off', spellcheck: 'false',
    'aria-label': 'The branch or commit the change starts from', placeholder: 'main' });
  const branchForm = h('form', { class: 'search', role: 'search', onsubmit: (e) => {
    e.preventDefault();
    const v = baseInput.value.trim();
    if (v) location.hash = `#/change?base=${encodeURIComponent(v)}`;
  } }, baseInput);

  const symbolInput = h('input', { type: 'search', id: 'change-symbol', value: symbol, autocomplete: 'off', spellcheck: 'false',
    'aria-label': 'The function, method or type you intend to change', placeholder: 'A function, method or type' });
  const symbolForm = h('form', { class: 'search', role: 'search', onsubmit: (e) => {
    e.preventDefault();
    const v = symbolInput.value.trim();
    if (v) location.hash = `#/change?symbol=${encodeURIComponent(v)}`;
  } }, symbolInput);

  const out = [
    h('h1', { class: 'page-title', text: 'What does a change put at risk?' }),
    h('p', { class: 'lede', text: 'Review a branch that is already written, or plan a change to one thing before you make it.' }),
    h('div', { class: 'cols' },
      h('div', { class: 'note' }, h('h3', { text: 'Review a branch' }),
        h('p', { text: 'Everything from this branch or commit up to the current HEAD. Press Enter to run.' }), branchForm),
      h('div', { class: 'note' }, h('h3', { text: 'Plan a change to one thing' }),
        h('p', { text: 'The edits in order, the callers to update and the tests that guard each. Press Enter to run.' }), symbolForm)),
  ];
  if (base) {
    const args = { base };
    if (head) args.head = head;
    out.push(liveSection(`What ${base}..${head || 'HEAD'} puts at risk`, 'analyze_change', args, {
      intro: 'Changed symbols first, then the tests that reach them, then the other code within reach. Run the tests listed here before anything else.',
    }));
    out.push(h('section', { class: 'section' }, h('h2', { text: 'Put this on the pull request' }),
      h('p', { class: 'sub', text: 'The same analysis as a comment on the pull request, from CI.' }),
      copyable(`repocortex impact --base ${base} --head ${head || 'HEAD'} --json | repocortex report pr`)));
  }
  if (symbol) {
    out.push(liveSection(`Plan for ${symbol}`, 'plan_change', { symbol }));
    out.push(liveSection('Tests to run first', 'find_tests', { symbol }));
  }
  return out;
}

/*
  Coverage is the figure every other answer depends on, so it is drawn as
  figures a person can read at a glance, not as the sentence the tool writes
  for a model. The sentence is still there, under "Show the detail".
*/
function coverageSection() {
  const body = h('div', null, loading('Measuring how complete the index is'));
  call('get_resolution_coverage').then((a) => {
    const m = /resolved (\d+) of (\d+)[^(]*\(([\d.]+)%\); (\d+) ambiguous, (\d+) matched no definition/.exec(a.summary || '');
    if (!m) { body.replaceChildren(answerBlock(a)); return; }
    const n = (v) => Number(v).toLocaleString('en-US');
    const fact = (figure, label) => h('div', { class: 'fact' }, h('b', { text: figure }), h('span', { text: label }));
    body.replaceChildren(h('div', { class: 'with-aside' },
      h('div', null,
        h('p', { class: 'verdict' }, h('b', { text: `${Math.round(Number(m[3]))}%` }),
          ' of the references this index could connect to a definition are connected.'),
        h('div', { class: 'facts' },
          fact(n(m[1]), 'connected to a definition'),
          fact(n(m[4]), 'matched several definitions, so none was chosen'),
          fact(n(m[5]), 'point outside this repository, mostly into libraries')),
        h('p', { class: 'sub', text: 'A compiler index connects most of the rest and turns very likely into proven.' }),
        copyable('repocortex indexers install --yes --out-dir .repocortex/scip')),
      h('aside', null, limitsNote(a), askNote(a))));
  }).catch((err) => body.replaceChildren(failure('Coverage', err)));
  return h('section', { class: 'section' }, h('h2', { text: 'How complete the index is' }),
    h('p', { class: 'sub', text: 'Every other answer in this portal is only as complete as this.' }), body);
}

function healthView() {
  return [
    h('h1', { class: 'page-title', text: 'Where this codebase needs attention' }),
    h('p', { class: 'lede', text: 'Three lists, most useful first: where change concentrates, what looks unused, and what the index could not see.' }),
    coverageSection(),
    liveSection('Hotspots', 'list_hotspots', {}, { intro: 'Files that are both large and changed often. A row marked "changes with" names a file usually committed with the one above it, whether or not any code connects them.' }),
    liveSection('Unused code', 'list_dead_code', {}, { intro: 'Definitions nothing calls, checked against the source text as well as the graph. Only the ones with every other explanation ruled out are listed.' }),
    liveSection('What the index could not see', 'list_blindspots', {}, { intro: 'Each kind of gap, with what fixes it.' }),
  ];
}

function agentsView() {
  const table = h('div', null, loading('Reading the tool list'));
  fetch(`${API}/tools`, { headers: { Accept: 'application/json' } }).then((r) => r.json()).then((body) => {
    const tools = body.tools || [];
    const max = Math.max(...tools.map((t) => t.estimated_tokens), 1);
    table.replaceChildren(
      h('p', { class: 'sub', text: `${tools.length} tools, about ${body.total_estimated_tokens.toLocaleString('en-US')} tokens of description per request when all are exposed. The default profile exposes seven.` }),
      h('div', { class: 'table-wrap' }, h('table', null,
        h('thead', null, h('tr', null, h('th', { text: 'Tool' }), h('th', { text: 'Context cost per request' }), h('th', { text: 'What it returns' }))),
        h('tbody', null, tools.map((t) => {
          const bar = h('span', { class: 'bar' });
          bar.style.width = `${Math.round((t.estimated_tokens / max) * 120)}px`;
          return h('tr', null,
            h('td', null, h('span', { class: 'name', text: t.name })),
            h('td', null, bar, `about ${t.estimated_tokens} tokens`),
            h('td', null, h('span', { class: 'why', text: (t.description || '').split('. ')[0] + '.' })));
        })))));
  }).catch((err) => table.replaceChildren(failure('The tool list', err)));

  const way = (title, detail, command) => h('div', { class: 'note' }, h('h3', { text: title }), h('p', { text: detail }), copyable(command));
  return [
    h('h1', { class: 'page-title', text: 'Connect a coding agent' }),
    h('p', { class: 'lede', text: 'The same tools reach an agent four ways. Pick the one your agent can use.' }),
    h('section', { class: 'section' }, h('h2', { text: 'Four ways in' }), h('div', { class: 'cols' },
      way('Agents on this machine', 'Claude Code, Cursor, Codex and most others. Configures each one it finds.', 'repocortex install --rules'),
      way('Agents that need a URL', 'ChatGPT, Android Studio. Serves MCP over HTTP, with a token.', 'repocortex serve --http 127.0.0.1:7788'),
      way('Anything that calls HTTP', 'Plain JSON, with an OpenAPI document at /api/v1/openapi.json.', 'curl -H "Authorization: Bearer $REPOCORTEX_MCP_TOKEN" -d \'{"name":"ParseConfig"}\' http://127.0.0.1:7788/api/v1/tools/find_callers'),
      way('A shell and nothing else', 'Aider, a CI step, a script.', 'repocortex tool call find_callers name=ParseConfig'))),
    h('section', { class: 'section' }, h('h2', { text: 'What each tool costs' }),
      h('p', { class: 'sub', text: 'Every tool an agent can see spends context on every request, whether or not it is used. Expose the smallest set that covers the work.' }), table),
    h('section', { class: 'section' }, h('h2', { text: 'See what an agent asked' }),
      h('p', { class: 'sub', text: 'Turn on the audit log to record every call an agent makes: the tool, what it asked, how long it took. It records the question and never the answer.' }),
      copyable('repocortex serve --audit-log .repocortex/audit.jsonl')),
  ];
}

// ---------------------------------------------------------------- router

function parseHash() {
  const raw = location.hash.replace(/^#\/?/, '') || 'home';
  const [pathPart, query = ''] = raw.split('?');
  const params = Object.fromEntries(new URLSearchParams(query));
  const [head, ...rest] = pathPart.split('/');
  return { head, tail: decodeURIComponent(rest.join('/')), params };
}

function render() {
  const { head, tail, params } = parseHash();
  let nodes;
  let section = head;
  let title = head;
  if (head === 's' && tail) { nodes = symbolView({ name: tail, ...params }); section = ''; title = tail; }
  else if (head === 'search' && params.q) { nodes = searchView(params); section = ''; title = `Results for ${params.q}`; }
  else if (head === 'explore') nodes = exploreView();
  else if (head === 'change') nodes = changeView(params);
  else if (head === 'health') nodes = healthView();
  else if (head === 'agents') nodes = agentsView();
  else { nodes = homeView(); section = 'home'; title = 'Home'; }

  const main = view();
  main.replaceChildren(...nodes.filter(Boolean));
  for (const a of document.querySelectorAll('.side a')) {
    if (a.dataset.section === section) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  }
  // A symbol's name is shown as written; only section names are capitalised.
  document.title = `RepoCortex: ${section ? title.charAt(0).toUpperCase() + title.slice(1) : title}`;
  // A tab change keeps the reader where they are; a new page starts at the top.
  if (render.last !== `${head}/${tail}`) { window.scrollTo(0, 0); main.focus({ preventScroll: true }); }
  render.last = `${head}/${tail}`;
}

// ---------------------------------------------------------------- start

$('#search').addEventListener('submit', (e) => {
  e.preventDefault();
  const q = $('#q').value.trim();
  if (q) location.hash = `#/search?q=${encodeURIComponent(q)}`;
});

document.addEventListener('keydown', (e) => {
  const typing = ['INPUT', 'TEXTAREA'].includes(document.activeElement.tagName);
  if (e.key === '/' && !typing) { e.preventDefault(); $('#q').focus(); }
  if (e.key === 'Escape' && typing) document.activeElement.blur();
});

const THEME_KEY = 'repocortex.theme';
function applyTheme(theme) {
  const root = document.documentElement;
  if (theme) root.dataset.theme = theme; else delete root.dataset.theme;
  const dark = theme ? theme === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
  $('#theme').textContent = dark ? 'Light theme' : 'Dark theme';
}
$('#theme').addEventListener('click', () => {
  const dark = document.documentElement.dataset.theme
    ? document.documentElement.dataset.theme === 'dark'
    : matchMedia('(prefers-color-scheme: dark)').matches;
  const next = dark ? 'light' : 'dark';
  try { localStorage.setItem(THEME_KEY, next); } catch { /* not remembered, still applied */ }
  applyTheme(next);
});
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
  if (!document.documentElement.dataset.theme) applyTheme('');
});
let saved = '';
try { saved = localStorage.getItem(THEME_KEY) || ''; } catch { /* default to the system theme */ }
applyTheme(saved);

fetch(API, { headers: { Accept: 'application/json' } }).then((r) => r.json())
  .then((info) => { if (info.project) $('#project').textContent = info.project; })
  .catch(() => {})
  .finally(() => { addEventListener('hashchange', render); render(); });
