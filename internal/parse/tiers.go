package parse

// Grammar count is not capability. The pure-Go backend can tag definitions in
// every grammar it carries, but only a handful of languages have the import,
// inheritance, receiver and arity queries that resolution actually needs.
// Publishing one number for both would be the overclaim this project exists
// to avoid, so the tier is DERIVED from what the parser can do rather than
// written down. A language cannot be promoted by editing a table; it is
// promoted by gaining the capability, which is the only thing that changes
// what a user gets.
//
// # Two ways to be typed
//
// What separates the typed tier from the resolved one is DISCRIMINATION: given
// several definitions of one name, something in the source that says which one
// a call means. There are two such things, and a language needs one of them.
//
// Arity — how many arguments a call passes against how many parameters each
// candidate declares. Go, Java and TypeScript have it.
//
// The receiver's class — `self.save()` is the `save` of the class it is written
// in; `entry.post()` is Ledger's when the function says `entry = Ledger(...)`.
// Python has it, from the scope analysis in pyscope.go, and has no arity query:
// one was built, measured against a compiler index and removed, because it
// decided nothing (see the note on paramCountQueries).
//
// The second route was added on a measurement, not to let Python in. Against
// scip-python on four repositories Python's `high` tier sits in the band the
// typed languages measure in, on precision and on the share of calls it
// catches, and listing it beside languages that bind by name alone would have
// understated what a user gets. docs/accuracy.md has the numbers — including
// the two repositories where the compiler index's own handling of wildcard
// re-exports has to be set aside first, which is done there in the open.

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

	// TierTyped: TierResolved plus discrimination between candidates of one
	// name — by argument count, or by the class the source states for the
	// receiver — which is what lets a reference reach `high` confidence rather
	// than `medium`.
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
		return "definitions, imports, receiver discrimination by arity or class — references can reach 'high'"
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
	// A single-file component has no import query of its own: its <script> is
	// handed to the TypeScript or JavaScript parser (sfc.go), which reads the
	// imports, and the template's components are bound through them. It is
	// resolved by the capability of the language its script is written in.
	if isSFC(language) {
		hasImports = ImportQuery("typescript") != ""
	}
	hasReceiver := HasMeasuredReceivers(language)
	_, hasArity := arityDialectFor(language)

	switch {
	case hasImports && hasReceiver && (hasArity || HasScopeAnalysis(language)):
		return TierTyped
	case hasImports:
		return TierResolved
	default:
		return TierParsed
	}
}

// HasScopeAnalysis reports whether the parser walks a language's scopes to
// state what a query over single nodes cannot: the class a definition belongs
// to and the class a receiver holds.
//
// Derived from the same switch the parser dispatches on (see
// genericParser.analyzeScopes), so a language reports it exactly when the walk
// runs for it.
func HasScopeAnalysis(language string) bool {
	return scopeAnalysisFor(language) != nil
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
