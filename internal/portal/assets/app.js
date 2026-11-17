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

/* ask is call, remembered for as long as the reader stays on one page. A tab
   change redraws the page, and a page that follows a route to forty tables
   must not ask forty questions again to show the next tab. render() empties
   this when the reader moves to another page, so nothing outlives its page. */
const asked = new Map();
function ask(tool, args = {}) {
  const key = `${tool} ${JSON.stringify(args)}`;
  if (!asked.has(key)) {
    asked.set(key, call(tool, args).catch((err) => { asked.delete(key); throw err; }));
  }
  return asked.get(key);
}

/* each runs fn over items a few at a time and keeps the results in order. The
   server answers one question in milliseconds; forty at once would still make
   every other part of the page wait behind them. */
async function each(items, fn, width = 6) {
  const out = new Array(items.length);
  let next = 0;
  const worker = async () => {
    while (next < items.length) { const i = next++; out[i] = await fn(items[i], i); }
  };
  await Promise.all(Array.from({ length: Math.min(width, items.length) }, worker));
  return out;
}

/* A tool that could not answer says so in text: a code, what went wrong, and a
   hint written for a model. A person is shown what went wrong. */
function toolFailure(answer) {
  const lines = String(answer.text || '').split('\n').map((l) => l.trim())
    .filter((l) => l && !l.startsWith('===') && !l.startsWith('error_code:') && !l.startsWith('hint:'));
  return lines.join(' ') || 'The tool gave no reason.';
}
const notFound = (answer) => answer.failed && /error_code: not_found/.test(answer.text || '');

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
  endpoint: 'route', table: 'table', reads: 'reads it', writes: 'writes to it', depends_on: 'is built on it',
  exposes: 'is served by',
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

/* Four kinds of thing have a page: a symbol, a file, a route and a table. Every
   name anywhere in the portal is a link to the page of the thing it names, so
   the reader never types a name the page has already shown them. */
const enc = encodeURIComponent;
/* withQuery adds the parts of an address after the "?", leaving out the empty ones. */
function withQuery(base, query) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(query)) if (v) p.set(k, v);
  const qs = p.toString();
  return qs ? `${base}?${qs}` : base;
}
const symbolHref = (name, path, more = {}) => withQuery(`#/s/${enc(name)}`, { path, ...more });
const fileHref = (path) => `#/f/${String(path).split('/').map(enc).join('/')}`;
const routeHref = (signature, at, more = {}) => withQuery(`#/r/${enc(signature)}`, { at, ...more });
const tableHref = (name, more = {}) => withQuery(`#/t/${enc(name)}`, more);

function symbolLink(name, path, line) {
  return h('a', { class: 'name', href: symbolHref(name, path, { line: line ? String(line) : '' }), text: name });
}
function fileLink(path, line) {
  return h('a', { class: 'where', href: fileHref(path) }, breakablePath(line ? `${path}:${line}` : path));
}

/* A path may break after each '/' or '_' and nowhere else. A table column then
   takes the room of its longest word rather than of the whole path, and the
   last column stays on screen; a path is never split inside a word. */
function breakablePath(text) {
  return breakAfter(text, /(?<=[\/_])/);
}

/* A qualified name may break after each '.', '#', '::' or '_' and nowhere
   else, so Class.method wraps between the two and a snake_case name between
   its words, never inside a word. */
function breakableName(text) {
  return breakAfter(text, /(?<=\.|#|::|_)/);
}

function breakAfter(text, at) {
  const out = [];
  for (const part of String(text).split(at)) out.push(part, h('wbr'));
  out.pop();
  return out;
}

/* The page a row's name leads to, or nothing when the name is not one thing:
   "A -> B" (a hop on a route), a person, a call the index could not connect. */
function hrefFor(item, path) {
  const name = item.name || '';
  if ((item.kind === 'file' || item.kind === 'changes_with') && item.path) return fileHref(item.path);
  if (item.kind === 'endpoint') return routeHref(name, item.path);
  if (item.kind === 'table') return tableHref(name);
  if (['hop', 'person', 'unresolved_call'].includes(item.kind)) return '';
  return /^[\w$.#:<>~-]+$/.test(name) ? symbolHref(name, path) : '';
}

function nameCell(item, path) {
  const name = item.name || '(unnamed)';
  // A candidate row is one of several definitions sharing a name. Its link
  // carries the path and line that select it, which the name alone cannot.
  const href = item.kind === 'candidate' && item.path
    ? symbolHref(name, item.path, { line: item.line ? String(item.line) : '' })
    : hrefFor(item, path);
  return href ? h('a', { class: 'name', href }, breakableName(name)) : h('span', { class: 'name' }, breakableName(name));
}

/* itemsTable draws rows of one answer. `relationship` and `why` are column
   headings; either can be turned off where it would repeat itself down the
   whole column. On a file's own page every row is in that file, so lineOnly
   shows the line and not the path again. */
function itemsTable(items, { showWhy = true, relationship = 'Relationship', why = 'How it was found', pathOnName = false, lineOnly = false } = {}) {
  const graded = items.some((i) => i.confidence);
  return h('div', { class: 'table-wrap' }, h('table', null,
    h('thead', null, h('tr', null,
      h('th', { text: 'Name' }), relationship && h('th', { text: relationship }),
      graded && h('th', { text: 'How sure' }),
      h('th', { text: lineOnly ? 'Line' : 'Where' }),
      showWhy && h('th', { text: why }))),
    h('tbody', null, items.map((i) => h('tr', null,
      h('td', null, nameCell(i, pathOnName ? i.path : '')),
      relationship && h('td', null, h('span', { class: 'kindtag', text: kindWord(i.kind) })),
      graded && h('td', null, i.confidence ? tierMark(i.confidence) : ''),
      h('td', null, lineOnly ? h('span', { class: 'where', text: i.line || '' }) : (i.path ? fileLink(i.path, i.line) : '')),
      showWhy && h('td', null, h('span', { class: 'why', text: i.why || '' })))))));
}

/* REACH_SENTENCE reads the counts and the risk out of analyze_impact's
   summary. Written in the syntax JavaScript and Go share, so the Go tests can
   run it against the summaries the tool really writes. */
const REACH_SENTENCE = /(\d+) direct, (\d+) indirect\b[\s\S]*?\bRisk (\w+)( or higher)?(?: \(([^)]*)\))?/;

/* caveatFor says in one plain line why a symbol's figures are not the whole
   story, with the tool's own explanation folded beneath it. */
const CAVEATS = {
  incomplete: 'Not every use is recorded, so these are the least the numbers could be.',
  unverified: 'Some of these uses may be bound to the wrong definition. Check them before relying on them.',
  low: 'Most uses in this language could not be connected, so this is a small part of them.',
  none: 'Nothing in this language could be connected, so the index cannot say what uses it.',
};
function caveatFor(level, why) {
  const line = CAVEATS[level] || 'The index cannot stand behind these numbers.';
  return [h('p', { text: line }), why && h('details', null, h('summary', { text: 'Why' }), h('p', { text: why }))];
}

/* Notes that say what an answer cannot see come first: they change how far
   the rows above can be trusted, and everything else is detail. */
const isLimit = (n) => /NOT VISIBLE|LOWER BOUND|COVERAGE|INCOMPLETE FOR|PROBABLY NOT|not proof|cannot see|could not be determined|unresolved|truncat|left out|not ranked|excluded|not listed|static (read|scan)|\bonly\b/i.test(n);

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
  // A tool that failed has no summary. Drawing that as "Nothing found" would
  // turn "the question could not be asked" into "the answer is none".
  if (answer.failed) {
    return h('div', { class: 'state bad' },
      h('h2', { text: 'This question could not be answered' }),
      h('p', { text: toolFailure(answer) }),
      h('p', { text: 'Nothing can be concluded from this: it is a failure to answer, not an empty answer.' }));
  }
  return h('div', { class: 'state' },
    h('h2', { text: answer.summary || 'Nothing found' }),
    (answer.notes || []).slice(0, 2).map((n) => h('p', { text: n })));
}

/* A page's own empty state: what is missing, why, and what to do about it. */
function nothing(title, ...lines) {
  return h('div', { class: 'state' }, h('h2', { text: title }), lines.filter(Boolean).map((l) => (l.nodeType ? l : h('p', { text: l }))));
}

/* What an answer found, without the caveat the tool puts in front of it.
   For an agent the summary opens "INCOMPLETE — <why>. <what was found>", so
   the warning cannot be missed. On a page the aside beside the rows states
   the same warning in full, and printing it above the rows as well buried the
   sentence that introduces them. The caveat is removed only when it is the
   reliability lead verbatim, so nothing the aside does not say is dropped. */
function foundLine(answer) {
  const summary = answer.summary || '';
  const lead = (answer.reliability || {}).lead || '';
  const m = /^[A-Z]+ — /.exec(summary);
  if (!m || !lead || !summary.startsWith(m[0] + lead)) return summary;
  return summary.slice(m[0].length + lead.length).trim() || summary;
}

/* answerBlock draws one tool's answer: its sentence, its rows, its limits. */
function answerBlock(answer, opts = {}) {
  const items = answer.items || [];
  // With rows, the summary introduces them. Without, the summary IS the
  // answer and is shown once, as the heading of the empty state.
  const main = items.length
    ? h('div', null, h('p', { class: 'sub', text: foundLine(answer) }), itemsTable(items, opts))
    : h('div', null, emptyState(answer));
  return h('div', { class: 'with-aside' }, main, h('aside', null, limitsNote(answer), askNote(answer), opts.aside));
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
/* Each entry is [label, address]. An entry saved before files, routes and
   tables had pages is a bare symbol name, and is read as one. */
function recents() {
  let saved = [];
  try { saved = JSON.parse(localStorage.getItem(RECENT_KEY)) || []; } catch { /* nothing remembered */ }
  return saved.map((e) => (Array.isArray(e) ? e : [e, symbolHref(e)])).filter(([label, href]) => label && String(href).startsWith('#/'));
}
function remember(label, href = symbolHref(label)) {
  try {
    const next = [[label, href], ...recents().filter(([, other]) => other !== href)].slice(0, 6);
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
    h('p', { class: 'lede', text: 'Search for a function, a type, a file, a route or a table. Each has a page that shows what uses it and what to check before you change it.' }),
    h('div', { class: 'actions' },
      h('button', { class: 'btn', type: 'button', onclick: () => $('#q').focus() }, 'Search the code'),
      h('a', { class: 'btn quiet', href: '#/change' }, 'Plan a change')),

    recent.length && h('section', { class: 'section' },
      h('h2', { text: 'Pick up where you left off' }),
      h('ul', { class: 'rows' }, recent.map(([label, href]) => h('li', { class: 'plain-row' }, h('a', { class: 'name', href, text: label }))))),

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
  // A query with a slash or a dot and no space may be a file's path. Asking
  // for the file is how the page learns whether there is one.
  const pathLike = /[/.]/.test(q) && !/\s/.test(q);
  Promise.allSettled([call('find_symbol', { name: q }), call('search_code', { query: q, limit: 15 }),
    pathLike ? call('get_file_outline', { path: q }) : Promise.reject(new Error('not a path'))])
    .then(([exact, text, file]) => {
      const out = [];
      const defs = exact.status === 'fulfilled' ? (exact.value.items || []) : [];
      const hits = text.status === 'fulfilled' ? (text.value.items || []) : [];
      if (file.status === 'fulfilled' && !file.value.failed) {
        out.push(h('section', { class: 'section' },
          h('h2', { text: 'A file at this path' }),
          h('ul', { class: 'rows' }, h('li', null, h('a', { class: 'row-link', href: fileHref(q) },
            h('span', { class: 't', text: q }),
            h('span', { class: 's', text: 'What it holds, how often it changes and what changes with it.' }))))));
      }
      if (defs.length) {
        // A route and a table are found by their exact names like anything
        // else, and each row leads to the page for its own kind of thing.
        out.push(h('section', { class: 'section' },
          h('h2', { text: `Defined as ${q}` }),
          h('div', { class: 'table-wrap' }, h('table', null,
            h('thead', null, h('tr', null, h('th', { text: 'Name' }), h('th', { text: 'Kind' }), h('th', { text: 'Where' }))),
            h('tbody', null, defs.map((i) => h('tr', null,
              h('td', null, nameCell({ ...i, name: i.name || q }, i.path)),
              h('td', null, h('span', { class: 'kindtag', text: kindWord(i.kind) })),
              h('td', null, i.path ? fileLink(i.path, i.line) : ''))))))));
      }
      if (hits.length) {
        out.push(h('section', { class: 'section' },
          h('h2', { text: 'Mentioned in' }),
          h('p', { class: 'sub', text: 'Files whose text matches. A text match is not a relationship: it says the words appear, nothing more.' }),
          h('ul', { class: 'rows' }, hits.map((i) => h('li', { class: 'plain-row' },
            i.path ? fileLink(i.path) : h('span', { class: 'name', text: i.name || '' }),
            i.why && h('span', { class: 'why', text: ' ' + i.why }))))));
      }
      if (!out.length) {
        const answer = exact.status === 'fulfilled' ? exact.value : { summary: `Nothing named ${q} was found.`, notes: [] };
        out.push(emptyState(answer));
        out.push(h('p', { class: 'sub', text: 'Names are matched exactly, including case. Try the exact name of a function or type, a file\'s whole path, a route as it is written (GET /users), a table\'s name, or a word that appears in the code.' }));
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

function symbolView({ name, path, line, tab = 'callers', floor = '' }) {
  remember(name, symbolHref(name, path, { line }));
  const header = h('div', null, h('p', { class: 'crumb busy', text: 'Looking it up' }));
  const verdict = h('p', { class: 'verdict' });
  const caveat = h('div', { class: 'verdict-note' });
  const panel = h('div', { role: 'tabpanel', id: 'tabpanel' });
  const cache = new Map();

  call('find_symbol', { name }).then((a) => {
    const inPath = (a.items || []).filter((i) => !path || (i.path || '').includes(path));
    const def = inPath.find((i) => !line || String(i.line) === String(line)) || inPath[0] || (a.items || [])[0];
    // A table and a route are also found by name, and each has a page made
    // for it. The tag says so, where a reader landing here would look.
    const own = def && def.kind === 'table' ? h('a', { class: 'tag', href: tableHref(name) }, 'See what reads and writes this table')
      : def && def.kind === 'endpoint' ? h('a', { class: 'tag', href: routeHref(name, def.path) }, 'See what this route reaches') : null;
    const serves = h('span', { class: 'tags' });
    header.replaceChildren(
      h('p', { class: 'crumb' }, def ? fileLink(def.path, def.line) : 'Not found in the index'),
      h('div', { class: 'entity' }, h('h1', { text: name }), def && def.kind && h('span', { class: 'tag', text: kindWord(def.kind) }),
        (a.items || []).length > 1 && h('span', { class: 'tag', text: `${a.items.length} definitions share this name` }), own, serves));
    if (!def) verdict.textContent = 'Not found in the index.';
    // A function that answers a route says so, and leads to the route's page.
    if (def) {
      ask('list_api_endpoints').then((eps) => {
        const mine = (eps.items || []).map(endpointOf).filter((e) => e.handler === name && (e.handlerPath === def.path));
        serves.replaceChildren(...mine.slice(0, 3).map((e) => h('a', { class: 'tag', href: routeHref(e.signature, e.path) }, `Answers ${e.signature}`)));
      }).catch(() => { /* the tag is an extra; the page stands without it */ });
    }
  }).catch((err) => header.replaceChildren(failure('This symbol', err)));

  // Every tab's answer is asked for at once. Each is a few milliseconds, and
  // it puts a count on each tab, so the reader sees the size of an answer
  // before choosing to read it. The promises are kept and reused by the tab.
  // Every one of these tools takes path and line, so the definition the page
  // is about is the definition each tab asks about. A name shared by several
  // definitions comes back as a list of them to choose from.
  const selector = () => {
    const a = {};
    if (path) a.path = path;
    if (line) a.line = Number(line);
    return a;
  };
  const preload = (t) => call(t.tool, { [t.arg]: name, ...selector() });
  for (const t of SYMBOL_TABS) cache.set(t.id, preload(t));

  // The one-sentence answer: the judgement from analyze_impact, and how many
  // tests reach the symbol. It stays one sentence whatever the index can vouch
  // for. When it cannot vouch for the numbers, the sentence says how (at
  // least, or recorded), a line under it says why in plain words, and the
  // tool's own explanation is one click away; the reach tab's "What this
  // answer cannot see" lists every reason. Printing the tool's whole first
  // line as the heading put eighty words where one sentence belongs.
  Promise.allSettled([cache.get('reach'), cache.get('tests')]).then(([reach, tests]) => {
    if (reach.status !== 'fulfilled') return;
    const r = reach.value;
    const rel = r.reliability || {};
    const level = rel.level || 'good';
    // "45 direct, 535 indirect; at least 6 more ... Risk moderate or higher (...)":
    // the counts and the risk can have a clause between them.
    const m = REACH_SENTENCE.exec(r.summary || '');
    if (!m || level === 'none') {
      if (!verdict.textContent) verdict.textContent = 'The index cannot say what a change to it would reach.';
      caveat.replaceChildren(...caveatFor(level, rel.lead || r.summary || ''));
      return;
    }
    const [, direct, indirect, risk, orHigher, riskWhy] = m;
    const floor = level === 'incomplete' || level === 'low';
    const atLeast = floor ? 'at least ' : '';
    const parts = ['Used directly in ', h('b', { text: `${atLeast}${direct} place${direct === '1' ? '' : 's'}` }),
      '. A change can reach ', h('b', { text: `${atLeast}${Number(direct) + Number(indirect)} symbols` })];
    const n = tests.status === 'fulfilled' ? ((tests.value.page || {}).total ?? (tests.value.items || []).length) : 0;
    if (n) parts.push(', and ', h('b', { text: `${n}${tests.value.truncated ? '+' : ''} test${n === 1 ? '' : 's'}` }), ' reach it');
    parts.push(risk === 'unknown' ? '. Its risk could not be judged.' : `. Risk is ${risk}${orHigher ? ' or higher' : ''}.`);
    verdict.replaceChildren(...parts);
    if (level !== 'good') caveat.replaceChildren(...caveatFor(level, rel.lead || ''));
    else if (orHigher && riskWhy) caveat.replaceChildren(h('p', { text: `The risk is a floor: ${riskWhy}.` }));
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
      if (!slot) return;
      // The counted total when the tool gives one; a list of candidates is not
      // a count of anything.
      // A refusal because the name has no definition of the kind the tab is
      // about (kind_not_indexed) counted nothing either, and a 0 there reads as
      // an answer.
      if (a.outcome === 'ambiguous_name' || a.outcome === 'kind_not_indexed') slot.textContent = '?';
      else if (a.page && typeof a.page.total === 'number') slot.textContent = String(a.page.total);
      else slot.textContent = `${(a.items || []).length}${a.truncated ? '+' : ''}`;
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
    if (line) p.set('line', line);
    const next = { tab, floor, ...change };
    if (next.tab !== 'callers') p.set('tab', next.tab);
    if (next.floor) p.set('floor', next.floor);
    const qs = p.toString();
    sessionStorage.setItem('repocortex.focusTab', focusTab ? next.tab : '');
    location.hash = `#/s/${encodeURIComponent(name)}${qs ? '?' + qs : ''}`;
  }

  const spec = SYMBOL_TABS.find((t) => t.id === tab) || SYMBOL_TABS[0];
  const args = { [spec.arg]: name, ...selector() };
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
  return [header, verdict, caveat, tabs, panel];
}

// ---------------------------------------------------------------- a file, a route, a table

/*
  tabbed draws the facets of one thing: a tab for each, with its count, and the
  panel of the one chosen. The tab is kept in the address, so a reload and the
  back button return to it.

  A tab is { id, label, lede, count, panel }. count is a promise of the figure
  on the tab; panel builds the chosen tab's content. Both read through ask(),
  so the count and the panel are one question, asked once.
*/
function tabbed(base, params, tabs, label) {
  const current = tabs.find((t) => t.id === params.tab) || tabs[0];
  const go = (id, focus) => {
    const p = new URLSearchParams(params);
    if (id === tabs[0].id) p.delete('tab'); else p.set('tab', id);
    try { sessionStorage.setItem('repocortex.focusTab', focus ? id : ''); } catch { /* focus stays where it was */ }
    location.hash = withQuery(base, Object.fromEntries(p));
  };
  const list = h('div', { class: 'tabs', role: 'tablist', 'aria-label': label }, tabs.map((t) => {
    const figure = h('span', { class: 'c' });
    Promise.resolve(t.count).then((n) => { if (n != null) figure.textContent = String(n); }).catch(() => {});
    return h('button', {
      role: 'tab', type: 'button', id: `tab-${t.id}`, 'aria-controls': 'tabpanel',
      'aria-selected': String(t === current), tabindex: t === current ? '0' : '-1', onclick: () => go(t.id),
    }, t.label, figure);
  }));
  // Arrow keys move between tabs, as the tab pattern expects.
  list.addEventListener('keydown', (e) => {
    if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft') return;
    const i = tabs.indexOf(current);
    go(tabs[(i + (e.key === 'ArrowRight' ? 1 : tabs.length - 1)) % tabs.length].id, true);
  });
  const panel = h('div', { role: 'tabpanel', id: 'tabpanel', 'aria-labelledby': `tab-${current.id}` },
    h('div', { class: 'panel-head' }, h('p', { text: current.lede })), current.panel());
  queueMicrotask(() => {
    let want = '';
    try { want = sessionStorage.getItem('repocortex.focusTab'); sessionStorage.removeItem('repocortex.focusTab'); } catch { /* nothing to restore */ }
    const button = want && $(`#tab-${want}`);
    if (button) button.focus();
  });
  return [list, panel];
}

/* pending holds a place until a promise settles, then shows what draw makes
   of the value. `what` names the thing in the sentence a reader sees. */
function pending(promise, what, draw) {
  const box = h('div', null, loading(`Reading ${what}`));
  promise.then((value) => box.replaceChildren(...[draw(value)].flat().filter(Boolean)))
    .catch((err) => box.replaceChildren(failure(what.charAt(0).toUpperCase() + what.slice(1), err)));
  return box;
}

const withAside = (main, ...notes) => h('div', { class: 'with-aside' }, h('div', null, main), h('aside', null, notes));
const note = (title, ...kids) => h('div', { class: 'note' }, h('h3', { text: title }),
  kids.filter(Boolean).map((k) => (k.nodeType ? k : h('p', { text: k }))));
const plural = (n, one, many = one + 's') => `${Number(n).toLocaleString('en-US')} ${Number(n) === 1 ? one : many}`;
const count = (answer, rows) => `${rows.length}${answer.truncated ? '+' : ''}`;

/* endpointOf reads one row of list_api_endpoints. The tool names the function
   that answers a route in a sentence, "handler: name (file)". When it names
   none, the route is real and only its function is unknown. */
function endpointOf(item) {
  const m = /^handler: (\S+)(?: \((.+)\))?$/.exec(item.why || '');
  return {
    signature: item.name || '', path: item.path || '', line: item.line,
    handler: m ? m[1] : '', handlerPath: m ? (m[2] || item.path || '') : '',
  };
}

/* Some questions take a name and no file. When several definitions share the
   name, the one in this file is asked for by its full name instead. */
function exactName(name, path) {
  return ask('find_symbol', { name }).then((a) => {
    const items = a.items || [];
    const here = items.find((i) => i.path === path);
    return items.length > 1 && here && here.fqn ? here.fqn : name;
  }).catch(() => name);
}

// ---------------------------------------------------------------- following calls

/* How many pairs a page follows before it is asked to. Each is one question to
   the index; a service with three hundred routes should not be asked three
   hundred questions because someone opened a page. */
const FOLLOW_LIMIT = 40;
const TIER_ORDER = ['low', 'medium', 'high', 'exact'];

/* follow asks, for each pair, whether the index can walk from one thing to the
   other through the calls it connected: the question trace_call_path answers.
   tick reports progress, so the reader watches a count and not a spinner. */
function follow(pairs, tick) {
  let done = 0;
  return each(pairs, async (pair) => {
    let answer = null;
    try { answer = await ask('trace_call_path', pair.args); } catch { /* counted by the caller as not checked */ }
    done += 1;
    if (tick) tick(done, pairs.length);
    return { ...pair, answer, chain: chainOf(answer) };
  });
}

/* chainOf turns the hops of the first, shortest route into the names along it,
   the weakest link, and how the last step touches its target. */
function chainOf(answer) {
  if (!answer || answer.failed) return null;
  const all = answer.items || [];
  const first = all.filter((x) => /^route 1,/.test(x.why || ''));
  const hops = first.length ? first : all;
  if (!hops.length) return null;
  const names = [];
  hops.forEach((x, i) => {
    const [from, to] = String(x.name || '').split(' -> ');
    if (i === 0) names.push(from);
    names.push(to || '');
  });
  const tiers = hops.map((x) => x.confidence).filter(Boolean).sort((a, b) => TIER_ORDER.indexOf(a) - TIER_ORDER.indexOf(b));
  return { names, weakest: tiers[0] || '', access: hops[hops.length - 1].kind || '' };
}

/* The chain as a reader follows it: each function a link to its page, the
   table at the end a link to the table's page unless this is that page. */
function chainCell(chain, { tableAtEnd = true } = {}) {
  const last = chain.names.length - 1;
  return h('span', { class: 'chain' }, chain.names.map((n, i) => [
    i > 0 && h('span', { class: 'arrow', text: ' → ' }),
    i === last ? (tableAtEnd ? h('a', { class: 'name', href: tableHref(n), text: n }) : h('span', { class: 'name', text: n })) : symbolLink(n),
  ]));
}

const ACCESS_WORDS = { reads: 'reads it', writes: 'writes to it', calls: 'calls it' };
const accessWord = (kind) => ACCESS_WORDS[kind] || kindWord(kind);

const followCaveat = 'A chain is followed only through calls the index connected, and at most six calls deep. One call it could not connect breaks the chain, so a missing row is not proof of no connection.';

function followAsk(sample, per) {
  return note('Ask your agent the same thing',
    `It gets this by calling trace_call_path once for each ${per}.`,
    sample && copyable(commandFor('trace_call_path', sample)));
}

/* A count that fills in as a follow proceeds. */
function progress(noun) {
  const line = h('p', { class: 'sub busy', text: `Checking each ${noun}` });
  return { line, tick: (done, total) => { line.textContent = `Checked ${done} of ${total} ${noun}s`; } };
}

// ---------------------------------------------------------------- file page

/* The history tool ranks files. Asked about one file, its note on how to read
   a ranking has nothing to say, and is left out. */
const withoutRankingNote = (a) => ({ ...a, notes: (a.notes || []).filter((n) => !/^The score orders this list/.test(n)) });

/* fileHistory reads this file's own row out of list_hotspots: how much it
   holds, how much leans on it, how often it changes. */
function fileHistory(answer, path) {
  const row = (answer.items || []).find((i) => i.kind === 'file' && i.path === path);
  const span = (/last (\d+) commit/.exec(answer.summary || '') || [])[1] || '';
  if (!row) return { span };
  const m = /(\d+) commit\(s\), ([^,]+), (\d+) symbol\(s\), (\d+) inbound relationship\(s\)(?:, last changed (\S+))?/.exec(row.why || '');
  return { span, found: true, raw: row.why || '', commits: m && m[1], definitions: m && m[3], uses: m && m[4], last: m && m[5] };
}

/*
  changePartners finds the files committed together with one file.

  The history tool names partners only for the files it ranks, and only
  partners inside the folder it was asked about. Asked about one file alone it
  can therefore name none. So the question is put to the whole repository
  first, then to each folder on the way down to the file, and the widest search
  that ranks this file is the one shown. The page says which that was, because
  a search of one folder cannot see a partner outside it.
*/
async function changePartners(path) {
  const folders = path.split('/').slice(0, -1);
  const scopes = ['', ...folders.map((_, i) => `${folders.slice(0, i + 1).join('/')}/`)];
  let answer = null;
  for (const scope of scopes) {
    answer = withoutRankingNote(await ask('list_hotspots', { ...(scope ? { path: scope } : {}), limit: 50, include_tests: true }));
    const items = answer.items || [];
    const at = items.findIndex((i) => i.kind === 'file' && i.path === path);
    let end = at + 1;
    while (at >= 0 && end < items.length && items[end].kind === 'changes_with') end += 1;
    // A file at the very end of an answer that was cut short may have lost
    // its partners with the rest; the next, narrower search will hold them.
    if (at >= 0 && !(answer.truncated && end === items.length)) {
      return { answer, scope, found: true, partners: items.slice(at + 1, end) };
    }
  }
  return { answer, scope: '', found: false, partners: [] };
}

/* The file's one-sentence answer: how much it holds, how much leans on it and
   how often it changes. All three figures are the history tool's. */
function fileSentence(f) {
  if (!f.found) {
    return [`No history to show for this file. The history covers files that hold definitions and were changed in the last ${f.span || 'few hundred'} commits.`];
  }
  if (!f.commits) return [f.raw];
  const them = f.definitions === '1' ? 'it' : 'them';
  return ['Holds ', h('b', { text: plural(f.definitions, 'definition') }), '. ',
    ...(Number(f.uses) ? [`Code in other files uses ${them} in `, h('b', { text: plural(f.uses, 'place') }), '. ']
      : [`Nothing in another file is known to use ${them}. `]),
    'Changed in ', h('b', { text: `${f.commits} of the last ${plural(f.span, 'commit')}` }), f.last ? `, most recently on ${f.last}.` : '.'];
}

/* Words that open a line of code in most languages and name nothing. */
const NOT_NAMES = new Set(('package import from export default return func function def class struct interface type const var let '
  + 'public private protected static final void async await new this self extends implements string int bool error nil null true false')
  .split(' '));

/* Names in a file's opening lines that the index confirms are defined in that
   file become links to their pages. Each word is put to the index, and only an
   exact name defined in this file is linked, so no link here is a guess. */
function linkDefinitions(code, text, path, done) {
  const lines = text.split('\n').filter((l) => !/^\s*(\/\/|#|\*|\/\*|--|"""|''')/.test(l));
  const words = [...new Set(lines.join('\n').match(/[A-Za-z_][\w]{2,}/g) || [])].filter((w) => !NOT_NAMES.has(w)).slice(0, 30);
  each(words, (w) => ask('find_symbol', { name: w })
    .then((a) => ((a.items || []).some((i) => i.path === path) ? w : null)).catch(() => null))
    .then((found) => {
      const names = found.filter(Boolean);
      if (names.length) {
        const parts = text.split(new RegExp(`\\b(${names.join('|')})\\b`));
        code.replaceChildren(...parts.map((part, i) => (i % 2 ? symbolLink(part, path) : part)));
      }
      done(names.length);
    });
}

/* How the file opens. The outline tool returns its first lines and stops, and
   the page says so in the place the reader would otherwise assume the rest. */
function outlinePanel(a, path, next) {
  if (a.failed) return answerBlock(a, { aside: next });
  const text = String(((a.items || [])[0] || {}).excerpt || '');
  const cut = /… \(truncated\)\s*$/.test(text);
  const body = text.replace(/\n?… \(truncated\)\s*$/, '');
  const shown = body.split('\n').length;
  const code = h('code', { text: body });
  const linked = h('p', { class: 'sub' });
  linkDefinitions(code, body, path, (n) => {
    if (n) {
      linked.textContent = n === 1 ? 'One name in these lines is defined in this file. It is a link to its page.'
        : `${n} names in these lines are defined in this file. Each is a link to its page.`;
    }
  });
  const limits = cut ? { ...a, notes: [`Cut short: only the first ${shown} lines of the outline are returned.`, ...(a.notes || [])] } : a;
  return withAside([
    h('pre', { class: 'outline', tabindex: '0', role: 'region', 'aria-label': `The start of ${path}` }, code),
    linked,
    cut && h('p', { class: 'sub', text: `These are its first ${shown} lines. The answer stops there, so what the file defines further down is not listed on this page. This prints all of it:` }),
    cut && copyable(`repocortex outline ${path}`),
  ], limitsNote(limits), askNote(a), next);
}

function partnersPanel(found, next) {
  const a = found.answer;
  const rows = found.partners.map((p) => {
    const m = /in (\d+) of its (\d+) commit/.exec(p.why || '');
    return h('tr', null, h('td', null, fileLink(p.path)),
      h('td', null, h('span', { class: 'why', text: m ? `${m[1]} of the ${plural(m[2], 'commit')} that changed this file` : (p.why || '') })));
  });
  const where = found.scope
    ? `Only files under ${found.scope} were searched: that is the widest search in which this file is ranked. A partner outside that folder would not appear.`
    : 'The whole repository was searched.';
  const main = !found.found
    ? nothing('No change partners to show for this file',
      'Partners are worked out only for the files that change most, and this file is not among them in the repository or in its own folder. Either no recent commit touched it, or it holds no definitions, or many files near it change more often.',
      'This prints the ranking for its folder:', copyable(`repocortex hotspots --include-tests${a && a.args.path ? ` --path ${a.args.path}` : ''}`))
    : rows.length
      ? [h('div', { class: 'table-wrap' }, h('table', null,
        h('thead', null, h('tr', null, h('th', { text: 'File' }), h('th', { text: 'Changed together in' }))), h('tbody', null, rows))),
      h('p', { class: 'sub', text: where })]
      : nothing('No file changes together with this one often enough to list',
        'It was committed on its own, or with a different set of files each time.', where,
        'Commits that touch a great many files at once are left out: they pair everything with everything.');
  return withAside(main, limitsNote(found.scope ? { ...a, notes: [`Only files under ${found.scope} were searched.`, ...(a.notes || [])] } : a), askNote(a), next);
}

function unusedPanel(a, rows, next) {
  if (a.failed) return answerBlock(a, { aside: next });
  const main = rows.length
    ? itemsTable(rows, { relationship: 'Kind', why: 'Why it is listed', pathOnName: true, lineOnly: true })
    : nothing('Everything defined here is used somewhere, as far as the index can tell',
      'Nothing in this file is without a caller in the index. That says the index found a use for each definition, not that every use is a good one.');
  return withAside(main, limitsNote(a), askNote(a), next);
}

function fileRoutesPanel(a, rows, next) {
  const main = rows.length ? routesTable(rows)
    : nothing('No route is declared in this file or answered from it',
      (a.items || []).length ? 'The index found routes elsewhere in this repository.' : (a.notes || [])[0],
      h('p', null, h('a', { href: '#/explore', text: 'See every route the index found' })));
  return withAside(main, limitsNote(a), askNote(a), next);
}

function fileView({ path, ...params }) {
  const slash = path.lastIndexOf('/');
  remember(path, fileHref(path));

  // Five questions, asked at once and drawn as each answers. The history is
  // read from git and can take seconds; nothing else waits for it.
  const history = ask('list_hotspots', { path, include_tests: true }).then(withoutRankingNote);
  const partners = changePartners(path);
  const outline = ask('get_file_outline', { path });
  const unused = ask('list_dead_code', { path, min_certainty: 'possible', include_exported: true, limit: 50 });
  const routes = ask('list_api_endpoints');
  const unusedHere = (a) => (a.items || []).filter((i) => i.path === path);
  const routesHere = (a) => (a.items || []).map(endpointOf).filter((e) => e.path === path || e.handlerPath === path);

  const verdict = h('p', { class: 'verdict busy', text: 'Reading its history' });
  history.then((a) => { verdict.className = 'verdict'; verdict.replaceChildren(...fileSentence(fileHistory(a, path))); })
    .catch(() => { verdict.className = 'verdict'; verdict.textContent = 'Its history could not be read.'; });

  const next = note('Before you change this file',
    'What uses a definition, which tests reach it and who last changed it are answered for one definition at a time. Follow a name on this page, or search for the one you will touch.',
    h('p', null, h('a', { href: '#/change', text: 'Plan a change to one definition' })));
  const tabs = [
    { id: 'top', label: 'Start of the file',
      lede: 'How the file opens, with the bodies of its functions removed: what it says it is and what it brings in.',
      panel: () => pending(outline, 'the start of the file', (a) => outlinePanel(a, path, next)) },
    { id: 'with', label: 'Changes with', count: partners.then((f) => (f.found ? f.partners.length : '')),
      lede: 'Files usually committed together with this one, whether or not any code connects them. Check them when you change it.',
      panel: () => pending(partners, 'what changes with it', (f) => partnersPanel(f, next)) },
    { id: 'unused', label: 'Looks unused', count: unused.then((a) => count(a, unusedHere(a))),
      lede: 'Definitions here that nothing in the index calls. That is a reason to look, never proof: search for the name before deleting anything.',
      panel: () => pending(unused, 'what looks unused', (a) => unusedPanel(a, unusedHere(a), next)) },
    { id: 'routes', label: 'Routes', count: routes.then((a) => routesHere(a).length),
      lede: 'Routes declared in this file, and routes answered by a function in it.',
      panel: () => pending(routes, 'the routes', (a) => fileRoutesPanel(a, routesHere(a), next)) },
  ];

  const body = h('div', null, verdict, tabbed(fileHref(path), params, tabs, `Views of ${path}`));
  // The file is read from the working copy. One that is not there has no page.
  outline.then((a) => {
    if (!notFound(a)) return;
    const stem = path.slice(slash + 1).replace(/\.[^.]+$/, '');
    body.replaceChildren(nothing(`No file at ${path}`,
      'A path is read exactly as written, from the top of the repository, in the working copy as it is now. A file that was renamed or deleted is not here.',
      h('p', null, h('a', { href: `#/search?q=${enc(stem)}`, text: `Search for ${stem}` }))));
  }).catch(() => {});

  return [
    h('p', { class: 'crumb', text: slash < 0 ? 'At the top of the repository' : path.slice(0, slash + 1) }),
    h('div', { class: 'entity' }, h('h1', { text: path.slice(slash + 1) }), h('span', { class: 'tag', text: 'file' })),
    body,
  ];
}

/* Routes as a table: the route, the function that answers it, where it is
   declared. Used wherever routes are listed. */
function routesTable(endpoints) {
  return h('div', { class: 'table-wrap' }, h('table', null,
    h('thead', null, h('tr', null, h('th', { text: 'Route' }), h('th', { text: 'Answered by' }), h('th', { text: 'Declared at' }))),
    h('tbody', null, endpoints.map((e) => h('tr', null,
      h('td', null, h('a', { class: 'name', href: routeHref(e.signature, e.path), text: e.signature })),
      h('td', null, e.handler ? symbolLink(e.handler, e.handlerPath) : h('span', { class: 'why', text: 'not recorded' })),
      h('td', null, e.path ? fileLink(e.path, e.line) : ''))))));
}

/* Chains as a table: the thing at the far end of each, how the chain touches
   the table, its weakest link, and the calls along the way. */
function chainsTable(heading, rows, firstCell, chainOptions) {
  return h('div', { class: 'table-wrap' }, h('table', null,
    h('thead', null, h('tr', null, h('th', { text: heading }), h('th', { text: 'Nearest use' }), h('th', { text: 'How sure' }), h('th', { text: 'By way of' }))),
    h('tbody', null, rows.map((r) => h('tr', null,
      h('td', null, firstCell(r)),
      h('td', null, h('span', { class: 'kindtag', text: accessWord(r.chain.access) })),
      h('td', null, r.chain.weakest ? tierMark(r.chain.weakest) : ''),
      h('td', null, chainCell(r.chain, chainOptions)))))));
}

/* The margin beside a followed list: what following cannot see, and the call. */
function followNotes(rows, per) {
  const sample = ((rows.find((r) => r.chain) || rows[0] || {}).answer) || null;
  return [limitsNote(sample ? { ...sample, notes: [followCaveat, ...(sample.notes || [])] } : { notes: [followCaveat] }),
    followAsk(sample && sample.args, per)];
}

// ---------------------------------------------------------------- route page

function routeView({ signature, at = '', ...params }) {
  const header = h('div', null, h('p', { class: 'crumb busy', text: 'Looking it up' }));
  const verdict = h('p', { class: 'verdict' });
  const body = h('div', null);
  const title = () => h('div', { class: 'entity' }, h('h1', { text: signature }), h('span', { class: 'tag', text: 'route' }));

  ask('list_api_endpoints').then((eps) => {
    const all = (eps.items || []).map(endpointOf);
    const e = all.find((x) => x.signature === signature && (!at || x.path === at)) || all.find((x) => x.signature === signature);
    if (!e) {
      header.replaceChildren(title());
      body.replaceChildren(nothing(`No route written ${signature} is in the index`,
        'A route is matched as it is written in the code, method first: GET /users/{id}.',
        (eps.notes || [])[0],
        h('p', null, h('a', { href: '#/explore', text: `See the ${plural(all.length, 'route')} the index found` }))));
      return;
    }
    remember(signature, routeHref(signature, e.path));
    header.replaceChildren(h('p', { class: 'crumb' }, 'Declared at ', fileLink(e.path, e.line)), title());
    if (!e.handler) {
      verdict.textContent = 'The index found this route, but not the function that answers it.';
      body.replaceChildren(nothing('Nothing to follow from here',
        'The route is real. The way it is declared did not name a function the index could tie to it, so what it calls, the tables it reaches and the tests that cover it cannot be worked out.',
        h('p', null, 'Read the declaration at ', fileLink(e.path, e.line), '.')));
      return;
    }
    drawRoute(e, params, verdict, body);
  }).catch((err) => { header.replaceChildren(title()); body.replaceChildren(failure('This route', err)); });

  return [header, verdict, body];
}

/* The route's one-sentence answer, said as its parts arrive: who answers it at
   once, then what that function reaches, then whether a test covers it. */
function routeSentence(e, said) {
  const parts = ['Answered by ', symbolLink(e.handler, e.handlerPath), '.'];
  if (said.reach != null) {
    parts.push(' It reaches ', h('b', { text: said.reach ? plural(said.reach, 'table') : 'no table' }), said.reach ? '.' : ' the index can follow it to.');
  }
  if (said.tests) {
    const t = said.tests;
    parts.push(' ', h('b', { text: t.n ? `${t.n}${t.more ? '+' : ''} test${t.n === 1 ? '' : 's'}` : 'No test' }),
      !t.n ? ' is known to reach it.' : t.proven ? ' reach it.' : ' may cover it; none is tied to it by a call.');
  }
  return parts;
}

/* The tables a route's function can get to. Each is one question to the index,
   so the panel counts them off as they are answered. */
function reachedTablesPanel(e, tables, followed, meter, next) {
  const box = h('div', null, meter.line);
  Promise.all([tables, followed]).then(([t, rows]) => {
    const total = (t.items || []).length;
    const hits = rows.filter((r) => r.chain);
    const failed = rows.filter((r) => !r.answer).length;
    const main = !total ? nothing('The index found no database tables in this repository', (t.notes || [])[0])
      : !hits.length ? nothing(`No table is within reach of ${e.handler}, as far as the index can follow`, followCaveat,
        h('p', null, h('a', { href: symbolHref(e.handler, e.handlerPath, { tab: 'calls' }), text: `See what ${e.handler} calls` })))
        : [h('p', { class: 'sub', text: `${hits.length} of the ${plural(rows.length, 'table')} checked. Only the shortest chain to each is shown, so a table reached by a read here may also be written to by a longer one.` }),
          chainsTable('Table', hits, (r) => h('a', { class: 'name', href: tableHref(r.table), text: r.table }))];
    box.replaceChildren(withAside([main,
      rows.length < total && h('p', { class: 'sub' }, `The ${rows.length} most used tables were checked, of ${total}. `,
        h('a', { href: routeHref(e.signature, e.path, { tab: 'tables', all: '1' }), text: `Check all ${total}` })),
      failed > 0 && h('p', { class: 'sub', text: `${plural(failed, 'table')} could not be checked because the question failed.` }),
    ], followNotes(rows, 'table'), next));
  }).catch((err) => box.replaceChildren(failure('The tables it reaches', err)));
  return box;
}

function drawRoute(e, params, verdict, body) {
  const calls = ask('find_callees', { name: e.handler, path: e.handlerPath });
  const tests = exactName(e.handler, e.handlerPath).then((symbol) => ask('find_tests', { symbol }));
  const tables = ask('list_db_tables');
  const meter = progress('table');
  const followed = tables.then((t) => follow((t.items || []).slice(0, params.all ? undefined : FOLLOW_LIMIT)
    .map((i) => ({ table: i.name, args: { from: e.handler, from_path: e.handlerPath, to: i.name } })), meter.tick));
  const reached = followed.then((rows) => rows.filter((r) => r.chain));

  const said = { reach: null, tests: null };
  const say = () => verdict.replaceChildren(...routeSentence(e, said));
  say();
  reached.then((rows) => { said.reach = rows.length; say(); }).catch(() => {});
  tests.then((a) => {
    if (a.failed) return;
    const proven = /(\d+) proven by a call/.exec(a.summary || '');
    said.tests = { n: (a.items || []).length, more: a.truncated, proven: proven ? Number(proven[1]) > 0 : false };
    say();
  }).catch(() => {});

  const next = note('Before you change it',
    'The function that answers the route is where a change lands. Its page lists what calls it and everything a change to it can reach.',
    h('p', null, h('a', { href: `#/change?symbol=${enc(e.handler)}`, text: `Plan a change to ${e.handler}` })),
    'Settings the route reads and events it sends are not on this page: no tool lists them for one route yet. These print them for the whole repository:',
    copyable('repocortex config'), copyable('repocortex events'));
  const tabs = [
    { id: 'calls', label: 'What it calls', count: calls.then((a) => (a.failed ? '' : count(a, a.items || []))),
      lede: `What ${e.handler} calls directly, inside this repository. Each row is a call site.`,
      panel: () => pending(calls, 'what it calls', (a) => answerBlock(a, { aside: next })) },
    { id: 'tables', label: 'Tables it reaches', count: reached.then((rows) => rows.length),
      lede: `The tables ${e.handler} can get to by following calls, and the shortest chain to each.`,
      panel: () => reachedTablesPanel(e, tables, followed, meter, next) },
    { id: 'tests', label: 'Tests', count: tests.then((a) => (a.failed ? '' : count(a, a.items || []))),
      lede: `Tests that reach ${e.handler}. One found through a call is evidence; one found by its name or its place in the tree is a candidate.`,
      panel: () => pending(tests, 'the tests', (a) => answerBlock(a, { aside: next })) },
  ];
  body.replaceChildren(...tabbed(routeHref(e.signature), { ...params, at: e.path }, tabs, `Views of ${e.signature}`));
}

// ---------------------------------------------------------------- table page

const touches = (answer) => (answer && !answer.failed ? (answer.items || []) : []);

/* What reads and writes a table is asked of the table as the index recorded
   it. A name can be declared in several files (a migration, a test fixture)
   and the statements hang on one of them; if the first has none, the others
   are tried before the page says there are none. A table nothing here declares
   has no list at all, and the answer is null. */
async function tableUses(name, declared) {
  const places = (await declared).items || [];
  if (!places.length) return { answer: null, path: '' };
  let best = { answer: await ask('find_references', { name }), path: '' };
  for (const p of places.slice(0, 4)) {
    if (touches(best.answer).length) break;
    const answer = await ask('find_references', { name, path: p.path });
    if (touches(answer).length) best = { answer, path: p.path };
  }
  return best;
}

/* The table's one-sentence answer: the counts are list_db_tables' own. */
function tableSentence(row, routes) {
  const m = /(\d+) read\(s\), (\d+) write\(s\); (.*)/.exec(row.why || '');
  if (!m) return [row.why || ''];
  const parts = [h('b', { text: plural(m[1], 'statement') }), ` read${m[1] === '1' ? 's' : ''} it and `,
    h('b', { text: m[2] }), ` write${m[2] === '1' ? 's' : ''} to it. `,
    /^declared/.test(m[3]) ? 'It is declared in this repository.' : 'Nothing here declares it, so another service most likely owns it.'];
  if (routes != null) {
    parts.push(' ', h('b', { text: routes ? plural(routes, 'route') : 'No route' }), routes === 1 ? ' leads to it.' : routes ? ' lead to it.' : ' is known to lead to it.');
  }
  return parts;
}

/* One side of a table's use: what writes to it, or what reads it. */
function usesPanel(name, kind, emptyTitle, u, t, next) {
  const search = h('p', null, h('a', { href: `#/search?q=${enc(name)}`, text: `Search the code for ${name}` }));
  if (!u.answer) {
    return withAside(nothing('Used here, declared somewhere else',
      'Nothing in this repository declares this table, so the index has a count of the statements that name it and no list of the code behind them. That is the usual shape of a service reading a database another service owns.',
      search), limitsNote(t), askNote(t), next);
  }
  if (u.answer.failed) return answerBlock(u.answer, { aside: next });
  const rows = touches(u.answer).filter((i) => i.kind === kind);
  const main = rows.length
    ? [u.answer.truncated && h('p', { class: 'sub', text: `The answer stops at ${touches(u.answer).length} rows, shared between readers and writers, so some are missing from this list.` }),
      itemsTable(rows, { relationship: '', why: 'Statement' })]
    : nothing(emptyTitle, 'Only statements written out in the code are recognised. One built by a query library, or put together far from where it runs, is not visible to the index.', search);
  return withAside(main, limitsNote({ ...u.answer, notes: [...(u.answer.notes || []), ...(t.notes || [])] }), askNote(u.answer), next);
}

/* The routes whose function can get to this table. */
function leadingRoutesPanel(name, followed, meter, next) {
  const box = h('div', null, meter.line);
  followed.then((f) => {
    const hits = f.rows.filter((r) => r.chain);
    const main = !(f.total + f.blind) ? nothing('The index found no routes in this repository', f.notes[0])
      : !hits.length ? nothing('No route is known to lead to this table', followCaveat,
        'Tables are also reached from scheduled jobs, command-line tools and tests. Those are in the lists of what reads and writes it.')
        : chainsTable('Route', hits, (r) => h('a', { class: 'name', href: routeHref(r.endpoint.signature, r.endpoint.path), text: r.endpoint.signature }), { tableAtEnd: false });
    box.replaceChildren(withAside([main,
      f.rows.length < f.total && h('p', { class: 'sub' }, `The first ${f.rows.length} routes were checked, of ${f.total}. `,
        h('a', { href: tableHref(name, { tab: 'routes', all: '1' }), text: `Check all ${f.total}` })),
      f.blind > 0 && h('p', { class: 'sub', text: `${plural(f.blind, 'route')} could not be followed: the index did not record the function that answers ${f.blind === 1 ? 'it' : 'them'}.` }),
    ], followNotes(f.rows, 'route'), next));
  }).catch((err) => box.replaceChildren(failure('The routes that reach it', err)));
  return box;
}

function declaredPanel(d, rest, next) {
  const places = d.items || [];
  const main = places.length
    ? [h('ul', { class: 'rows' }, places.map((p) => h('li', { class: 'plain-row' }, fileLink(p.path, p.line)))),
      rest.length > 0 && h('h3', { class: 'minor', text: 'Built on it' }),
      rest.length > 0 && itemsTable(rest, { relationship: '', why: 'Statement' })]
    : nothing('Nothing in this repository declares this table',
      'It is named by statements here and created somewhere else, most likely by the service that owns the database.');
  return withAside(main, limitsNote(d), askNote(d), next);
}

function tableView({ name, ...params }) {
  const usage = ask('list_db_tables', { table: name });
  const declared = ask('find_symbol', { name, kind: 'table' });
  const routes = ask('list_api_endpoints');
  const uses = tableUses(name, declared);
  uses.catch(() => { /* each reader of this promise reports the failure where it shows */ });
  const known = (t) => (t.items || []).find((i) => String(i.name).toLowerCase() === name.toLowerCase());
  const rowsOf = (kind) => uses.then((u) => (u.answer ? count(u.answer, touches(u.answer).filter((i) => i.kind === kind)) : ''));

  const meter = progress('route');
  const followed = Promise.all([routes, uses, usage]).then(([r, u, t]) => {
    if (!known(t)) return { rows: [], total: 0, blind: 0, notes: [] };
    const all = (r.items || []).map(endpointOf);
    const withHandler = all.filter((e) => e.handler);
    const pairs = withHandler.slice(0, params.all ? undefined : FOLLOW_LIMIT).map((e) => ({
      endpoint: e, args: { from: e.handler, from_path: e.handlerPath, to: name, ...(u.path ? { to_path: u.path } : {}) },
    }));
    return follow(pairs, meter.tick).then((rows) => ({ rows, total: withHandler.length, blind: all.length - withHandler.length, notes: r.notes || [] }));
  });
  const leading = followed.then((f) => f.rows.filter((r) => r.chain).length);

  const verdict = h('p', { class: 'verdict busy', text: 'Counting what touches it' });
  const body = h('div', null);
  const next = note('Before you change a column',
    'Check everything that writes to it first, then everything that reads it. A statement put together while the program runs may be missing from both lists.',
    copyable(`repocortex db table ${name}`));
  const side = (kind, what, emptyTitle) => () => pending(Promise.all([uses, usage]), what, ([u, t]) => usesPanel(name, kind, emptyTitle, u, t, next));
  const tabs = [
    { id: 'writers', label: 'Written by', count: rowsOf('writes'),
      lede: 'Code that inserts, updates or deletes rows. This is the list to check before changing a column.',
      panel: side('writes', 'what writes to it', 'Nothing in the index writes to this table') },
    { id: 'readers', label: 'Read by', count: rowsOf('reads'), lede: 'Code that selects from it.',
      panel: side('reads', 'what reads it', 'Nothing in the index reads this table') },
    { id: 'routes', label: 'Routes that reach it', count: leading,
      lede: 'Routes whose function can get to this table by following calls, and the shortest chain from each.',
      panel: () => leadingRoutesPanel(name, followed, meter, next) },
    { id: 'declared', label: 'Declared in', count: declared.then((a) => (a.items || []).length),
      lede: 'Where the table is created or altered: migrations, schema files and test fixtures.',
      panel: () => pending(Promise.all([declared, uses]), 'where it is declared',
        ([d, u]) => declaredPanel(d, touches(u.answer).filter((i) => i.kind === 'depends_on'), next)) },
  ];

  usage.then((t) => {
    const row = known(t);
    verdict.className = 'verdict';
    if (!row) {
      verdict.textContent = '';
      body.replaceChildren(nothing(`No table called ${name} is in the index`, (t.notes || [])[0],
        h('p', null, h('a', { href: '#/explore', text: 'See the tables the index found' }))));
      return;
    }
    remember(name, tableHref(name));
    verdict.replaceChildren(...tableSentence(row, null));
    leading.then((n) => verdict.replaceChildren(...tableSentence(row, n))).catch(() => {});
    body.replaceChildren(...tabbed(tableHref(name), params, tabs, `Views of ${name}`));
  }).catch((err) => { verdict.className = 'verdict'; verdict.textContent = ''; body.replaceChildren(failure('This table', err)); });

  return [
    h('p', { class: 'crumb', text: 'Database table' }),
    h('div', { class: 'entity' }, h('h1', { text: name }), h('span', { class: 'tag', text: 'table' })),
    verdict, body,
  ];
}

// ---------------------------------------------------------------- explore

function exploreView() {
  const routes = ask('list_api_endpoints');
  const tables = ask('list_db_tables');
  const section = (title, intro, body) => h('section', { class: 'section' }, h('h2', { text: title }), h('p', { class: 'sub', text: intro }), body);
  return [
    h('h1', { class: 'page-title', text: 'How this codebase is laid out' }),
    h('p', { class: 'lede', text: 'What it exposes and what it stores. Every route, function, file and table here leads to its own page.' }),
    section('Routes it exposes', 'Each HTTP route the index found, with the function that answers it. Open a route to see the tables it reaches and the tests that cover it.',
      pending(routes, 'the routes', (a) => {
        const rows = (a.items || []).map(endpointOf);
        return withAside(rows.length ? routesTable(rows) : emptyState(a), limitsNote(a), askNote(a));
      })),
    section('Tables it uses', 'Each table the code reads or writes, most used first. Open a table to see what reads it, what writes to it and the routes that lead there.',
      pending(tables, 'the tables', (a) => {
        const rows = a.items || [];
        if (!rows.length) return withAside(emptyState(a), limitsNote(a), askNote(a));
        return withAside(h('div', { class: 'table-wrap' }, h('table', null,
          h('thead', null, h('tr', null, h('th', { text: 'Table' }), h('th', { class: 'num', text: 'Statements that read it' }),
            h('th', { class: 'num', text: 'Statements that write to it' }), h('th', { text: 'Declared' }))),
          h('tbody', null, rows.map((i) => {
            const m = /(\d+) read\(s\), (\d+) write\(s\); (.*)/.exec(i.why || '');
            return h('tr', null,
              h('td', null, h('a', { class: 'name', href: tableHref(i.name), text: i.name })),
              h('td', { class: 'num', text: m ? m[1] : '' }), h('td', { class: 'num', text: m ? m[2] : '' }),
              h('td', null, h('span', { class: 'why', text: m ? (/^declared/.test(m[3]) ? 'in this repository' : 'somewhere else') : (i.why || '') })));
          })))), limitsNote(a), askNote(a));
      })),
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
      h('p', { class: 'sub', text: 'This page cannot list recent calls yet: the record of them is a file on this machine, and no tool reads it back. Until one does, turn the record on and read the file.' }),
      h('div', { class: 'cols' },
        way('Turn the record on', 'Every call an agent makes is added as one line: the tool, what it asked, how long it took and how large the answer was. The question is recorded and never the answer.', 'repocortex serve --audit-log .repocortex/audit.jsonl'),
        way('Read the latest calls', 'Each line is one call, newest last.', 'tail -n 20 .repocortex/audit.jsonl'))),
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
  // Leaving a page forgets what it asked, so a return to it reads the index afresh.
  if (render.last !== `${head}/${tail}`) asked.clear();
  if (head === 's' && tail) { nodes = symbolView({ name: tail, ...params }); section = ''; title = tail; }
  else if (head === 'f' && tail) { nodes = fileView({ ...params, path: tail }); section = ''; title = tail; }
  else if (head === 'r' && tail) { nodes = routeView({ ...params, signature: tail }); section = ''; title = tail; }
  else if (head === 't' && tail) { nodes = tableView({ ...params, name: tail }); section = ''; title = `${tail} (table)`; }
  else if (head === 'search' && params.q) { nodes = searchView(params); section = ''; title = `Results for ${params.q}`; }
  else if (head === 'explore') nodes = exploreView();
  else if (head === 'change') nodes = changeView(params);
  else if (head === 'health') nodes = healthView();
  else if (head === 'agents') nodes = agentsView();
  else { nodes = homeView(); section = 'home'; title = 'Home'; }

  const main = view();
  main.replaceChildren(...nodes.flat().filter(Boolean));
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
