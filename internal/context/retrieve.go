package context

import (
	"context"
	"sort"
	"strings"
)

// seed is a symbol the task named, directly or through one of its words.
type seed struct {
	symbolID int64
	score    float64
	matched  []string
}

// findSeeds finds the symbols whose names answer the task.
//
// Not a call to registry.FindSymbolsByName, deliberately. FindSymbolsByName
// matches EXACT names, which is the right contract for an agent asking about
// "TransactionService" and the wrong one here: the task says "callback", and
// the symbol is called handleCallback. Matching identifier PARTS is what
// bridges prose to code, and that cannot be expressed as an indexed equality
// lookup against the database.
//
// It used to be a full scan of the catalogue for that reason. It no longer has
// to be: the catalogue indexes each symbol under its identifier parts and their
// stems when it is built, so the same part-matching is a lookup — and the
// candidate set is exactly the set a full scan would have scored above zero,
// not a sample of it. See catalogue.seedCandidates.
func findSeeds(c *catalogue, in Intent, th Thresholds) []seed {
	var seeds []seed
	for _, id := range c.seedCandidates(in.Terms) {
		score, matched := nameSignal(c.symbols[id].Name, in.Terms)
		if score < th.SeedName {
			continue
		}
		seeds = append(seeds, seed{symbolID: id, score: score, matched: matched})
	}

	// Strongest first, then by id: two symbols with identical evidence must not
	// swap places between runs.
	sort.Slice(seeds, func(i, j int) bool {
		if seeds[i].score != seeds[j].score {
			return seeds[i].score > seeds[j].score
		}
		return seeds[i].symbolID < seeds[j].symbolID
	})
	if len(seeds) > th.MaxSeeds {
		seeds = seeds[:th.MaxSeeds]
	}
	return seeds
}

// lexicalHit is one file's full-text evidence.
type lexicalHit struct {
	fileID int64

	// terms are the DISTINCT task terms this file matched. The count is what
	// separates corroborated evidence from a single word in a comment, and it
	// is the admission test for a candidate with no structural connection to
	// the task (see admit).
	terms []string

	// weight is the summed weight of the matched terms — how much of the task's
	// intent this file accounts for.
	weight float64

	// best is the strongest bm25 score across the matching terms. FTS5 bm25 is
	// negative and more negative is better; it is kept in that form and only
	// normalised at the point of use.
	best float64
}

// searchLexical runs one full-text query per task term.
//
// Per term rather than one OR query, on purpose. A single `a OR b OR c` query
// returns a ranked list but not WHICH terms each file matched, and that count is
// load-bearing: in the payment fixture, unrelated/marketing/MarketingReport.java
// outranks CallbackController on the word "payment" alone — it contains the
// comment "Unrelated to payments" — and the only thing that distinguishes them
// lexically is that the controller matches two task terms and the decoy matches
// one. The cost is bounded by MaxQueryTerms queries, each indexed.
func searchLexical(ctx context.Context, reg Registry, in Intent, th Thresholds) (map[int64]*lexicalHit, error) {
	hits := map[int64]*lexicalHit{}

	terms := in.Terms
	if len(terms) > th.MaxQueryTerms {
		terms = terms[:th.MaxQueryTerms]
	}
	for _, t := range terms {
		for _, word := range queryForms(t) {
			matches, err := reg.SearchFiles(ctx, word, th.LexicalLimit)
			if err != nil {
				// A malformed FTS expression is a bug in escaping, not a
				// user-visible failure mode; a term that finds nothing is
				// normal. Either way the pack is still assemblable from the
				// structural signals, so this degrades rather than aborts.
				continue
			}
			for _, m := range matches {
				h := hits[m.FileID]
				if h == nil {
					h = &lexicalHit{fileID: m.FileID}
					hits[m.FileID] = h
				}
				if !contains(h.terms, t.Text) {
					h.terms = append(h.terms, t.Text)
					h.weight += t.Weight
				}
				if m.Score < h.best {
					h.best = m.Score
				}
			}
		}
	}
	for _, h := range hits {
		sort.Strings(h.terms)
	}
	return hits, nil
}

// queryForms returns the FTS expressions to try for one term: the word itself
// and its stem, each quoted.
//
// Quoting is not cosmetic. FTS5 treats bare AND/OR/NOT/NEAR as operators and
// a leading '-' as negation, so an unquoted term taken from a user's sentence
// can change the meaning of the query or make it a syntax error. Everything
// outside [a-z0-9_] is dropped first, so nothing a caller writes can reach the
// FTS parser as syntax.
func queryForms(t Term) []string {
	var out []string
	for _, w := range []string{t.Text, t.Stem} {
		clean := sanitiseFTS(w)
		if clean == "" {
			continue
		}
		q := `"` + clean + `"`
		if !contains(out, q) {
			out = append(out, q)
		}
	}
	return out
}

func sanitiseFTS(w string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(w) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// addLexicalSeeds lets corroborated TEXT evidence seed graph expansion.
//
// THE GAP THIS CLOSES. Until this existed, `proximity` was reachable only from
// findSeeds, which matches symbol NAMES. So when a task was phrased in the
// user's vocabulary rather than the code's spelling, the call graph — the one
// signal this tool has that its competitors do not — contributed nothing, and
// assemble.go said so out loud: "matched them textually, so there was nothing
// to expand from". A file could be found and admitted, but never radiate.
//
// Seven attempts to close it by moving ranking coefficients were all reverted
// on measurement, and that was the finding: no weight can rescue a signal that
// is never computed. This is the structural fix rather than the eighth tweak.
//
// WHY IT CANNOT REGRESS WHAT ALREADY WORKS. Name seeds are chosen first and
// keep every slot they claim; lexical seeds only fill capacity left over under
// MaxSeeds. A task whose names already fill the seed set produces a byte-
// identical pack. The change is reachable only where retrieval was previously
// returning nothing to expand from.
//
// The bar is MinLexicalTerms — the SAME corroboration admission already
// requires, not a new tunable. One task word in one comment is how the payment
// fixture's marketing decoy outranks the real controller; two distinct terms is
// what separates evidence from coincidence, and that threshold has already been
// measured. Seeds enter at exactly th.SeedName, the weakest admissible score,
// so a text match can never outrank a symbol the task actually named.
func addLexicalSeeds(c *catalogue, in Intent, seeds []seed, hits map[int64]*lexicalHit, th Thresholds) []seed {
	// A RESERVED QUOTA, not leftover capacity. The first version of this took
	// only slots the name seeds had not claimed, which sounded safe and was
	// inert: measured on the self bench, every one of the five cases arrived
	// here with the seed set already full — 24 name seeds, zero room, and
	// between 53 and 98 corroborated lexical files discarded unused. The result
	// was byte-identical to no change at all.
	//
	// That is the real shape of the gap. It was never a shortage of seeds; it
	// is that the budget is monopolised by weak partial name matches. At the
	// SeedName floor a single shared identifier part qualifies, so a task
	// containing a common word like "index" fills all 24 slots with symbols
	// that merely spell it, and the file whose text actually discusses the task
	// never gets to expand.
	//
	// So lexical evidence is given its own slots and displaces the WEAKEST name
	// seeds — findSeeds sorts strongest-first, so the ones dropped are the
	// marginal part-matches at the bottom. Unlike the first version this CAN
	// regress, which is why it ships on a measurement rather than on the
	// argument above.
	if len(hits) == 0 {
		return seeds
	}
	room := th.MaxSeeds - len(seeds)
	if room < th.LexicalSeeds {
		room = th.LexicalSeeds
		if keep := th.MaxSeeds - room; keep < len(seeds) {
			seeds = seeds[:keep]
		}
	}
	if room <= 0 {
		return seeds
	}

	corroborated := make([]*lexicalHit, 0, len(hits))
	for _, h := range hits {
		if len(h.terms) >= th.MinLexicalTerms {
			corroborated = append(corroborated, h)
		}
	}
	// Most of the task's intent first, then by id: two files with identical
	// evidence must not swap places between runs, for the same reason
	// findSeeds sorts by id — the pack is part of the determinism contract.
	sort.Slice(corroborated, func(i, j int) bool {
		if corroborated[i].weight != corroborated[j].weight {
			return corroborated[i].weight > corroborated[j].weight
		}
		return corroborated[i].fileID < corroborated[j].fileID
	})

	seen := make(map[int64]struct{}, len(seeds))
	for _, s := range seeds {
		seen[s.symbolID] = struct{}{}
	}

	for _, h := range corroborated {
		if room <= 0 {
			return seeds
		}
		// One seed per file, not all of its symbols. A matching file is
		// evidence that the AREA is relevant; expanding from every symbol in it
		// would flood the walk with the file's incidental helpers and drown the
		// structural signal this exists to recover.
		// WHICH symbol, not just which file. This used to take whichever unit
		// happened to be first in the file, which is an arbitrary choice with a
		// real cost: expansion radiates from the seed, so seeding a file's
		// first helper instead of the function the task is about spends the
		// walk's depth budget going the wrong way out of the right file.
		//
		// The unit whose NAME best answers the task wins. That also lets two
		// weak signals corroborate: a file the task matches textually, holding
		// a symbol the task matches by name, is better evidence than either
		// alone -- and neither would have seeded on its own here.
		var best int64
		var bestScore float64
		for _, id := range c.unitsByFile[h.fileID] {
			if _, dup := seen[id]; dup {
				continue
			}
			score, _ := nameSignal(c.symbols[id].Name, in.Terms)
			// Ties by id, never by map order: the pack is part of the
			// determinism contract.
			if best == 0 || score > bestScore || (score == bestScore && id < best) {
				best, bestScore = id, score
			}
		}
		if best != 0 {
			seen[best] = struct{}{}
			seeds = append(seeds, seed{symbolID: best, score: th.SeedName, matched: h.terms})
			room--
		}
	}
	return seeds
}
