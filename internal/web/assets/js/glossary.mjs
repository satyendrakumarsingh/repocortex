/*
  THE WORDS THIS PORTAL STILL HAS TO USE, DEFINED WHERE THEY ARE USED.

  Some terms cannot be replaced without losing the meaning: an index is an
  index. Each gets one sentence, and term() wraps the word so the sentence is
  a hover, a long-press, and an accessible description. The list is short on
  purpose — a word that needs defining is a word to try to avoid first.
*/
export const TERMS = {
  'index': 'What RepoCortex built from your code: every file, symbol and relationship it could read, stored locally.',
  'symbol': 'A named thing in code — a function, method, type, class or variable.',
  'relationship': 'One piece of code using another: a call, an import, a read of a table, a route to a handler.',
  'package': 'A folder of code that belongs together, in whatever the language calls it.',
  'endpoint': 'A URL and method a service answers, such as POST /payment/callback.',
  'hop': 'One step along relationships. Two hops from A reaches what A uses, and what those use.',
  'lower bound': 'At least this many. The index only counts what it could read, so the real number may be higher.',
  'degraded': 'Some files could not be read, so counts on every screen may be lower than reality.',
  'blast radius': 'Everything that could be affected if you change one thing.',
  'compiler-backed': "Confirmed by the language's own compiler, not inferred from names.",
};

/* A word with its definition attached. Unknown words come back unwrapped. */
export function term(word, shown) {
  const def = TERMS[word];
  const d = document.createElement(def ? 'dfn' : 'span');
  d.textContent = shown || word;
  if (def) { d.title = def; d.setAttribute('aria-description', def); d.className = 'term'; }
  return d;
}

/* The whole list, for the "What do these words mean?" panel. */
export function renderGlossary() {
  const dl = document.createElement('dl');
  dl.className = 'glossary';
  for (const [w, def] of Object.entries(TERMS)) {
    const dt = document.createElement('dt'); dt.textContent = w;
    const dd = document.createElement('dd'); dd.textContent = def;
    dl.append(dt, dd);
  }
  return dl;
}
