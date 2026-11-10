/*
  THE ANSWER LINE AND THE FOLD.

  A screen exists to answer one question, and the answer is the first thing
  on it: one sentence, plain words, the key number in bold, and — when the
  API sent one — the single next action, in the API's own words. Everything a
  reader might want after that (the rows, the caveats) sits one click below in
  a fold that starts closed. Nothing is removed: a caveat folded is a caveat
  kept, and ?open=1 unfolds every one for an agent or a screenshot.
*/
const OPEN_ALL = new URLSearchParams(location.search).get('open') === '1';

function node(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}

/*
  answerLine(sentence, opts)
    sentence: string, or an array of strings and {b: string} parts for bold numbers
    opts.support: one supporting sentence under it (optional)
    opts.next:    {label, command, href, detail} — the API's next_action (optional)
*/
export function answerLine(sentence, opts) {
  const o = opts || {};
  const wrap = node('div', 'answer');
  const h = node('h2');
  for (const part of (Array.isArray(sentence) ? sentence : [sentence])) {
    if (part && typeof part === 'object' && 'b' in part) h.appendChild(node('b', null, part.b));
    else h.appendChild(document.createTextNode(String(part)));
  }
  wrap.appendChild(h);
  if (o.support) wrap.appendChild(node('p', 'support', o.support));
  if (o.next && (o.next.label || o.next.command)) {
    const p = node('p', 'next');
    if (o.next.command) {
      // The verb is visible: the button says what it does and to what, the
      // API's label stays as the sentence before it and as the title, and the
      // label flips to "Copied" for a moment so a click is seen to have
      // happened — the same shape as the copy buttons under every evidence
      // line, so a reader learns it once.
      if (o.next.label) p.appendChild(node('span', 'k', o.next.label + ' '));
      const b = node('button', 'primary', 'Copy: ' + o.next.command);
      b.type = 'button';
      b.title = o.next.label || 'Copy the command';
      b.addEventListener('click', () => {
        const done = () => { b.textContent = 'Copied'; setTimeout(() => { b.textContent = 'Copy: ' + o.next.command; }, 1200); };
        if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(o.next.command).then(done, done);
        else done();
      });
      p.appendChild(b);
    } else {
      const b = node('button', 'primary', o.next.label || 'Next');
      b.type = 'button';
      p.appendChild(b);
      if (o.next.href) b.addEventListener('click', () => { location.href = o.next.href; });
    }
    if (o.next.detail) p.appendChild(node('span', 'muted', ' ' + o.next.detail));
    wrap.appendChild(p);
  }
  return wrap;
}

/* A number the sentence can bold: bold("31 things") -> {b: "31 things"} */
export const bold = (s) => ({ b: String(s) });

/*
  foldable(summary, body, opts)
    summary: the plain line shown when closed, with the count in it
    body:    a Node
    opts.key: a stable id; open/closed is remembered per key in localStorage
    opts.open: force open. The aggregated-edge table is NEVER passed here —
               it is first-class by contract and is not wrapped at all.
*/
export function foldable(summary, body, opts) {
  const o = opts || {};
  const d = node('details', 'fold');
  const s = node('summary', null, summary);
  d.append(s, body);
  // The memory lives in localStorage under fold:<key>. That store is per
  // origin, so per portal port: two portals on different ports keep separate
  // memories, and a key shared between them is a preference, so a collision
  // would be harmless either way.
  let remembered = null;
  try { remembered = o.key ? localStorage.getItem('fold:' + o.key) : null; } catch (e) { remembered = null; }
  d.open = OPEN_ALL || o.open === true || remembered === 'open';
  // A details element whose `open` is set queues a toggle event that a
  // listener added right after still receives. Only a toggle the reader
  // made is remembered: the one the initial state earns is skipped, and an
  // ?open=1 visit — an agent, a screenshot — never writes anything, or one
  // such visit would leave every fold open for good.
  const initial = d.open;
  let armed = !initial;
  d.addEventListener('toggle', () => {
    if (!armed) { armed = true; return; }
    if (OPEN_ALL || !o.key) return;
    try { localStorage.setItem('fold:' + o.key, d.open ? 'open' : 'closed'); } catch (e) { /* private mode */ }
  });
  return d;
}
