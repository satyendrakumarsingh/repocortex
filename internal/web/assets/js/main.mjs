/*
  The shell's modules, published for the classic app.js that follows.

  app.js is a 10,000-line classic script; turning it into a module in one
  move would be the riskiest possible edit to a file nothing can hold whole.
  So the shell — rail, and in later tasks tier words, glossary, answer line
  and foldable — lives in modules, and this file hands them to app.js through
  one global. index.html loads this module first and app.js with `defer`
  second: a module script is deferred by default and deferred scripts run in
  document order, so window.cortex exists before app.js runs.
*/
import { GROUPS, LABEL, renderRail } from './nav.mjs';
import { TIER_WORDS, tierWord, tierSentence } from './tiers.mjs';
import { TERMS, term, renderGlossary } from './glossary.mjs';
import { answerLine, bold, foldable } from './answer.mjs';
import { renderHeader, renderView, codeMapIcon } from './codemap.mjs';

window.cortex = Object.assign(window.cortex || {}, {
  GROUPS, LABEL, renderRail,
  TIER_WORDS, tierWord, tierSentence,
  TERMS, term, renderGlossary,
  answerLine, bold, foldable,
  codemap: { renderHeader, renderView, icon: codeMapIcon },
});
const requested = new URLSearchParams(location.search).get('s') || 'overview';
const active = requested in LABEL ? requested : 'overview';
renderRail(document.querySelector('nav.rail'), active);
