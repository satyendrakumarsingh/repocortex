package parse

// Grammar count is not capability. The pure-Go backend can tag definitions in
// every grammar it carries, but only a handful of languages have the import,
// inheritance, receiver and arity queries that resolution actually needs.
// Publishing one number for both would be the overclaim this project exists
// to avoid, so the tier is DERIVED from the query maps rather than written
// down. A language cannot be promoted by editing a table; it is promoted by
// gaining the query, which is the only thing that changes what a user gets.

import "sort"

// Tier is how far RepoCortex can go on a language. Ordered: higher is more.
type Tier int

const (
	// TierParsed: definitions and full-text search. The grammar exists and
	// the tags query yields symbols. No cross-file relationship is claimed.
	TierParsed Tier = iota

	// TierResolved: TierParsed plus imports, so references can be bound
	// across files and the call graph is meaningful.
	TierResolved

	// TierTyped: TierResolved plus receiver and arity discrimination, which
	// is what lets a reference reach `high` confidence rather than `medium`.
	TierTyped
)

func (t Tier) String() string {
	switch t {
	case TierTyped:
		return "typed"
	case TierResolved:
		return "resolved"
	default:
		return "parsed"
	}
}

// Description is the one-line explanation printed beside the tier.
func (t Tier) Description() string {
	switch t {
	case TierTyped:
		return "definitions, imports, receiver and arity discrimination — references can reach 'high'"
	case TierResolved:
		return "definitions and imports — references bind across files"
	default:
		return "definitions and search only — no cross-file references claimed"
	}
}

// TierFor reports the tier for a language name as the backend spells it.
// Unknown languages report TierParsed, which is the honest floor: if we do
// not know the language we certainly cannot resolve it.
func TierFor(language string) Tier {
	hasImports := ImportQuery(language) != ""
	hasReceiver := ReceiverQuery(language) != ""
	_, hasArity := arityDialectFor(language)

	switch {
	case hasImports && hasReceiver && hasArity:
		return TierTyped
	case hasImports:
		return TierResolved
	default:
		return TierParsed
	}
}

// Tiers reports the tier of every grammar the shipping backend carries.
func Tiers() map[string]Tier {
	langs := PureGoBackend().Languages()
	out := make(map[string]Tier, len(langs))
	for _, l := range langs {
		out[l] = TierFor(l)
	}
	return out
}

// LanguagesInTier returns the languages at exactly the given tier, sorted.
func LanguagesInTier(t Tier) []string {
	var out []string
	for lang, got := range Tiers() {
		if got == t {
			out = append(out, lang)
		}
	}
	sort.Strings(out)
	return out
}
