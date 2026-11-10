/*
  CONFIDENCE, IN WORDS.

  The four tier ids — exact, high, medium, low — are the product's stable
  vocabulary in every API field, data-tier attribute and CLI flag, and they
  stay there. On screen a reader gets the word that says what the id means,
  with the measurement that earned it, because "medium" tells nobody that
  the edge is right roughly one time in five.
*/
export const TIER_WORDS = {
  exact:   { word: 'proven',      because: 'a compiler proved it' },
  high:    { word: 'very likely', because: 'measured 87–96% right' },
  medium:  { word: 'uncertain',   because: 'measured 6–38% right' },
  low:     { word: 'a guess',     because: 'measured under 3% right' },
  unknown: { word: 'unmeasured',  because: 'no measurement recorded' },
};

export function tierWord(id) { return (TIER_WORDS[id] || TIER_WORDS.unknown).word; }

/* "very likely (measured 87–96% right)" — for titles and hover cards. */
export function tierSentence(id) {
  const t = TIER_WORDS[id] || TIER_WORDS.unknown;
  return t.word + ' (' + t.because + ')';
}
