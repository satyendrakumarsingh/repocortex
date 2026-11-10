package context

import (
	"sort"
	"strings"
	"unicode"

	"github.com/satyendrakumarsingh/repocortex/internal/search"
)

// Term is one extracted concept from a task, with how much it should count.
//
// Weight is not frequency. It encodes how much INTENT the word carries: an
// identifier the author typed verbatim ("TransactionService", "handle_callback")
// is a near-certain pointer at code, a domain noun is a good one, and the verb
// of the sentence is mostly grammar.
type Term struct {
	Text   string
	Weight float64

	// Stem is a crude suffix-stripped form, or "" when stemming would not
	// change the word. "processing" -> "process", "transactions" ->
	// "transaction". Crude on purpose: a real stemmer is a dependency and a
	// source of surprises, and the only job here is to bridge the plural and
	// gerund forms that separate English prose from code identifiers.
	Stem string
}

// Intent is a task normalised into something retrievable.
type Intent struct {
	// Terms are de-duplicated and sorted by (descending weight, text) so that
	// everything downstream — including the Interpretation string — is stable.
	Terms []Term

	// Identifiers are tokens the author wrote in code form (CamelCase,
	// snake_case, or quoted). They are the strongest evidence a task carries.
	Identifiers []string

	// Verbs are the recognised action words, kept for Interpretation rather
	// than for retrieval.
	Verbs []string
}

// Weights assigned during extraction. Exported as constants rather than tuning
// knobs because they describe the grammar of a sentence, not a ranking policy.
const (
	weightIdentifier = 1.5
	weightConcept    = 1.2
	weightNoun       = 1.0
	weightVerb       = 0.35
)

// taskVerbs are the words that describe the ACTION rather than the subject.
//
// They are demoted, never dropped. "update" is a task verb and also half of
// TransactionService.updateStatus; deleting it would lose a real anchor, while
// treating it like a domain noun would drag in every setter in the repository.
var taskVerbs = map[string]bool{
	"add": true, "implement": true, "fix": true, "refactor": true, "remove": true,
	"update": true, "change": true, "support": true, "make": true, "create": true,
	"ensure": true, "improve": true, "migrate": true, "rename": true, "introduce": true,
	"delete": true, "move": true, "extract": true, "replace": true, "handle": true,
	"investigate": true, "debug": true, "optimise": true, "optimize": true, "write": true,
}

// stopwords carry no retrieval signal at all. Kept deliberately short: an
// aggressive stopword list eventually eats a word that mattered, and the
// weighting scheme already handles the merely-weak cases.
var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "for": true, "in": true,
	"on": true, "and": true, "or": true, "with": true, "at": true, "by": true,
	"from": true, "into": true, "to": true, "when": true, "that": true, "this": true,
	"is": true, "be": true, "it": true, "we": true, "our": true, "as": true,
	"so": true, "if": true, "not": true, "no": true, "all": true, "any": true,
	"should": true, "must": true, "can": true, "will": true, "would": true,
	"new": true, "old": true, "code": true, "please": true,
}

// minTermRunes drops one- and two-character fragments. They match almost
// everything in a full-text index, so they cost query time and buy noise.
const minTermRunes = 3

// ParseTask turns a plain-English task into an Intent.
//
// Quoted spans are lifted out first and treated as verbatim identifiers, which
// is how a user says "I mean this exact name" in a sentence that is otherwise
// prose.
func ParseTask(task string) Intent {
	var in Intent

	weights := map[string]float64{}
	seenIdent := map[string]bool{}
	seenVerb := map[string]bool{}

	// note records the strongest weight ever seen for a term. A word that
	// appears both as a bare noun and inside an identifier keeps the stronger
	// reading.
	note := func(text string, w float64) {
		if len([]rune(text)) < minTermRunes || stopwords[text] {
			return
		}
		if w > weights[text] {
			weights[text] = w
		}
	}

	body, quoted := liftQuoted(task)
	for _, q := range quoted {
		for _, tok := range fields(q) {
			addIdentifier(tok, &in, seenIdent, note)
		}
	}

	for _, tok := range fields(body) {
		lower := strings.ToLower(tok)
		switch {
		case isIdentifierLike(tok):
			addIdentifier(tok, &in, seenIdent, note)
		case taskVerbs[lower]:
			if !seenVerb[lower] {
				seenVerb[lower] = true
				in.Verbs = append(in.Verbs, lower)
			}
			note(lower, weightVerb)
		case isAllDigits(lower):
			// A bare number is never a useful retrieval term; it is a version,
			// a ticket id or a quantity.
		default:
			note(lower, weightNoun)
		}
	}

	expandConcepts(strings.ToLower(task), note)

	in.Terms = make([]Term, 0, len(weights))
	for text, w := range weights {
		in.Terms = append(in.Terms, Term{Text: text, Weight: w, Stem: stem(text)})
	}
	// Descending weight, then alphabetical. Determinism starts here: map
	// iteration order would otherwise leak into the FTS query, the seed set and
	// the Interpretation string.
	sort.Slice(in.Terms, func(i, j int) bool {
		if in.Terms[i].Weight != in.Terms[j].Weight {
			return in.Terms[i].Weight > in.Terms[j].Weight
		}
		return in.Terms[i].Text < in.Terms[j].Text
	})
	sort.Strings(in.Identifiers)
	sort.Strings(in.Verbs)
	return in
}

// addIdentifier records a code-shaped token both verbatim and split into its
// parts. Both forms are needed: the verbatim form matches the symbol exactly,
// and the parts match the sibling symbols that share a word with it —
// "handleCallback" should find CallbackController too.
func addIdentifier(tok string, in *Intent, seen map[string]bool, note func(string, float64)) {
	lower := strings.ToLower(tok)
	if len([]rune(lower)) < minTermRunes || stopwords[lower] {
		return
	}
	if !seen[tok] {
		seen[tok] = true
		in.Identifiers = append(in.Identifiers, tok)
	}
	note(lower, weightIdentifier)
	for _, part := range search.SplitIdentifier(tok) {
		note(part, weightNoun)
	}
}

// TotalWeight is the denominator for lexical coverage: what fraction of the
// task's intent a candidate accounts for.
func (in Intent) TotalWeight() float64 {
	var total float64
	for _, t := range in.Terms {
		total += t.Weight
	}
	return total
}

// Empty reports whether nothing retrievable came out of the task.
func (in Intent) Empty() bool { return len(in.Terms) == 0 }

// liftQuoted splits a task into its unquoted remainder and its quoted spans.
//
// An unterminated quote is treated as ordinary text rather than swallowing the
// rest of the sentence, because tasks are typed by humans and a stray
// apostrophe ("don't dedupe callbacks") must not silently delete the words
// after it.
func liftQuoted(task string) (body string, quoted []string) {
	var rest strings.Builder
	runes := []rune(task)

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r != '"' && r != '\'' && r != '`' {
			rest.WriteRune(r)
			continue
		}
		end := -1
		for j := i + 1; j < len(runes); j++ {
			if runes[j] == r {
				end = j
				break
			}
		}
		if end < 0 {
			rest.WriteRune(r)
			continue
		}
		quoted = append(quoted, string(runes[i+1:end]))
		rest.WriteRune(' ')
		i = end
	}
	return rest.String(), quoted
}

// fields splits on everything that cannot appear in an identifier.
func fields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
}

// isIdentifierLike reports whether a token was written in code form: it
// contains an underscore, a digit, or an internal capital.
func isIdentifierLike(tok string) bool {
	runes := []rune(tok)
	for i, r := range runes {
		if r == '_' || unicode.IsDigit(r) {
			return true
		}
		if i > 0 && unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

// stem strips the handful of English suffixes that stand between prose and code
// identifiers, and returns "" when it would not change the word.
//
// Deliberately not a Porter stemmer. Porter would turn "processing" into
// "process" (wanted) but also "idempotency" into "idempot" and "status" into
// "statu", inventing terms that match nothing and confuse the Interpretation.
// The rule here only removes a suffix when a recognisable word remains.
func stem(word string) string {
	const minStem = 4
	for _, suffix := range []string{"ing", "ies", "es", "ed", "s"} {
		if !strings.HasSuffix(word, suffix) {
			continue
		}
		base := word[:len(word)-len(suffix)]
		if suffix == "ies" {
			base += "y"
		}
		if len([]rune(base)) < minStem {
			continue
		}
		// A trailing "s" is only a plural after a plain consonant. "status",
		// "process" and "analysis" are singular already, and stripping their
		// final letter leaves a non-word that matches nothing and then appears
		// in the Interpretation looking like a bug.
		if suffix == "s" {
			switch {
			case strings.HasSuffix(word, "ss"), strings.HasSuffix(word, "us"),
				strings.HasSuffix(word, "is"), strings.HasSuffix(word, "os"):
				continue
			}
		}
		return base
	}
	return ""
}

// matchesTerm reports how well a single word matches a term, in [0,1].
//
// The three tiers are the whole of the lexical model and are ordered by how
// much they can be trusted: the word IS the term, the word CONTAINS the term as
// an identifier part, or the two share a stem.
func matchesTerm(word string, t Term) float64 {
	switch {
	case word == t.Text:
		return 1.0
	case t.Stem != "" && word == t.Stem:
		return 0.7
	case stem(word) != "" && (stem(word) == t.Text || (t.Stem != "" && stem(word) == t.Stem)):
		return 0.6
	default:
		return 0
	}
}
