package capability

import (
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// CoverageFloor is THE published coverage floor, for every surface that
// prints a verdict on how far to trust this index's relationships.
//
// One constant because there used to be two, and they disagreed in the same
// session: impact_analysis and find_tests judged ResolutionStats.Rate against
// 0.5 while resolution_coverage judged BindingRate against 0.6, so one real
// index was simultaneously "COVERAGE TOO LOW" at 37% and unremarkable at 79%.
// An agent given both numbers cannot act on either.
//
// The rate that survived is BindingRate, on the argument Rate's own doc
// comment makes against itself: Rate's denominator counts every reference that
// matched zero candidates in this index, overwhelmingly calls into libraries,
// so a project that imports heavily scores badly however well RepoCortex
// resolved it — a dependency surface reading as a defect rate. BindingRate
// divides by what this index claims to define, which is the population any
// improvement to binding could ever move.
//
// The value is 0.6, not 0.5, because a floor belongs to its denominator.
// Half of ALL references is a modest bar over a population RepoCortex does not
// control; over the bindable population it is a low one. Below three in five
// of what the index itself claims full ownership of, most of it was left
// unbound, and that is a resolver problem rather than a description of the
// project.
//
// THE FLOOR DOES NOT LICENSE HIDING THE DENOMINATOR. ResolutionStats.Bindable
// states the condition on which unresolved references may be excluded:
// callers print Unresolved beside the rate, never instead of it. Every surface
// that quotes this floor states the unresolved and ambiguous counts with it.
const CoverageFloor = 0.6

// The two statements every capability report must carry, verbatim.
//
// They are constants rather than prose written at each call site because the
// requirement is that they CANNOT be forgotten. A caveat that each report
// author remembers to add is a caveat that a future report omits, and the
// omission is invisible — the report still looks authoritative. Tests assert
// these exact strings are present in every report this package produces.
const (
	// LowerBoundCaveat states the single most important property of any answer
	// derived from a static index: it is a floor, not a ceiling.
	LowerBoundCaveat = "This result is a LOWER BOUND, not a complete answer. " +
		"RepoCortex reports only relationships it could resolve statically in the indexed run; " +
		"anything it could not resolve is absent from this report, not absent from the code."

	// DynamicDispatchCaveat names the specific mechanisms that are invisible.
	// Naming them matters: "may be incomplete" is ignorable, "your DI container
	// wiring is not represented here" is actionable.
	//
	// It used to say dynamic dispatch was not represented at all, which was true
	// while the index produced no IMPLEMENTS or EXTENDS edges outside a
	// compiler-backed SCIP import. The heuristic index now emits them for Go,
	// Java, TypeScript, TSX, JavaScript and Python, and impact expands through
	// them (see impact.Report.Dispatch), so the blanket denial is no longer
	// true. It is replaced by the narrower one that still is: the DECLARED half
	// of dispatch — which types implement which interface — is represented where
	// an edge was recorded; the RESOLVED half — which call site reaches which
	// implementation at run time — is not, and never can be from a static index.
	// Under-claiming would be as wrong as over-claiming: a reader who is told
	// dispatch is invisible will not read the dispatch section that is right
	// there.
	DynamicDispatchCaveat = "Reflection, runtime dependency injection, code generation, string-keyed lookups " +
		"and cross-language calls are invisible to static analysis and are NOT represented in this result. " +
		"Dynamic dispatch is represented only in part: where a declared subtype relationship was indexed " +
		"(an IMPLEMENTS or EXTENDS edge), implementations reached through it are expanded and labelled as " +
		"such, never as direct callers. WHICH implementation a given call site actually reaches at run time " +
		"is still unknown, and any subtype relationship the index did not record is absent from that " +
		"expansion. Verify before relying on it."

	// DispatchExpansionCaveat is carried by a report that actually performed the
	// dispatch expansion, and states what the labelled set does and does not
	// claim. Separate from DynamicDispatchCaveat because it is only true when
	// there was something to expand, and a caveat that is present when it does
	// not apply is a caveat readers learn to skip.
	DispatchExpansionCaveat = "This report expanded through IMPLEMENTS/EXTENDS edges: symbols listed under the " +
		"dispatch expansion are reachable at run time because a call naming the target can land on an " +
		"implementation. That is a POSSIBILITY claim derived from a declared subtype relationship, not a " +
		"recorded call — it is weaker than a direct caller and must not be counted as one. The expansion is " +
		"also a lower bound on the implementations themselves: it sees only the ones an edge recorded."
)

// Completeness is the honest account of what a report does not cover.
//
// Every capability report embeds one. It is constructed only through
// NewCompleteness so the mandatory caveats are always present — see Spec §20,
// which requires impact results to separate what is known from what is not, and
// forbids presenting a bounded traversal as an exhaustive one.
type Completeness struct {
	// LowerBound is always true. It is a field rather than an implicit property
	// so that a consumer serialising this to JSON, or rendering it for an
	// agent, has a machine-readable flag and not just prose to grep.
	LowerBound bool `json:"lower_bound"`

	// Resolution is the run's raw reference-resolution outcome. Published as
	// recorded, never estimated.
	Resolution registry.ResolutionStats `json:"resolution"`

	// BindableReferences is the references that name something this index
	// defines — bound, plus too ambiguous to choose between — and BindingRate
	// is the bound share of it. Together they are the single number that says
	// how much of what RepoCortex COULD understand it did.
	//
	// resolved_rate was that number and is deliberately gone rather than kept
	// alongside. It divided by every reference including the ones naming
	// library code no index of this repository can bind, so a project that
	// calls a lot of third-party code scored low however perfectly it resolved
	// — and it disagreed with the same figure in the CLI and the portal. The
	// raw counts are all in Resolution above, so a consumer that wants the old
	// ratio can still compute it; what is removed is a second published answer
	// to one question.
	BindableReferences int64   `json:"bindable_references"`
	BindingRate        float64 `json:"binding_rate"`

	// UnresolvedReferences and AmbiguousReferences are the counts a reader
	// needs to size the gap: each one is a use of a name that could have been
	// a call to the target and was not attributed to anything.
	UnresolvedReferences int64 `json:"unresolved_references"`
	AmbiguousReferences  int64 `json:"ambiguous_references"`
	PendingReferences    int64 `json:"pending_references"`

	// IndexedSymbols is the denominator behind every count in the report.
	IndexedSymbols int `json:"indexed_symbols"`

	// Truncated reports that a traversal stopped at a bound. Distinct from the
	// resolution gap: this is RepoCortex choosing to stop looking, not failing to
	// resolve. Both make the answer a lower bound; only one is fixable by
	// re-asking with wider bounds.
	Truncated       bool   `json:"truncated"`
	TruncatedReason string `json:"truncated_reason,omitempty"`

	// Caveats are the human/agent-readable statements. Always non-empty, always
	// beginning with LowerBoundCaveat.
	Caveats []string `json:"caveats"`
}

// NewCompleteness builds the completeness block for a report.
//
// It is the only constructor. Callers cannot produce a Completeness that omits
// the mandatory caveats without deliberately building the struct by hand, which
// is what the tests in this package check has not happened.
func NewCompleteness(stats registry.ResolutionStats, indexedSymbols int, truncated bool, reason string) Completeness {
	c := Completeness{
		LowerBound:           true,
		Resolution:           stats,
		BindableReferences:   stats.Bindable(),
		BindingRate:          stats.BindingRate(),
		UnresolvedReferences: stats.Unresolved,
		AmbiguousReferences:  stats.Ambiguous,
		PendingReferences:    stats.Pending,
		IndexedSymbols:       indexedSymbols,
		Truncated:            truncated,
		TruncatedReason:      reason,
	}
	c.Caveats = append(c.Caveats, LowerBoundCaveat, DynamicDispatchCaveat)

	// The resolution numbers are stated as a sentence as well as as fields.
	// An agent reads the prose; the fields are for anything that computes.
	if stats.Total > 0 {
		by := ""
		if stats.ByCompiler > 0 {
			by = fmt.Sprintf(" %d of those were settled by an imported compiler index rather "+
				"than by name matching.", stats.ByCompiler)
		}
		c.Caveats = append(c.Caveats, fmt.Sprintf(
			"Reference resolution for this index run: %d of %d references that name something this "+
				"index defines (%.1f%%) were bound to a definition.%s %d remain too ambiguous to "+
				"choose between and %d are still pending. A further %d matched no definition here "+
				"at all — mostly calls into libraries, which an index of this repository cannot "+
				"bind. Any of the %d unbound could be a use of this symbol that does not appear "+
				"above.",
			stats.Resolved, stats.Bindable(), 100*stats.BindingRate(), by,
			stats.Ambiguous, stats.Pending, stats.Unresolved,
			stats.Unresolved+stats.Ambiguous+stats.Pending))
	} else {
		// Zero references recorded is not "perfectly resolved". Saying so
		// prevents a 0/0 from rendering as a reassuring 100%.
		c.Caveats = append(c.Caveats,
			"This index run recorded no references at all, so resolution coverage is unknown rather than complete. "+
				"Relationship counts below may be far from the truth.")
	}

	if truncated {
		c.Caveats = append(c.Caveats, "Traversal was TRUNCATED: "+reason+
			". Counts below are what fit inside the traversal bounds, not what exists.")
	}
	return c
}

// HasCaveat reports whether a specific statement is present. Used by tests and
// by anything rendering a report that needs to check for a particular warning.
func (c Completeness) HasCaveat(s string) bool {
	for _, got := range c.Caveats {
		if got == s {
			return true
		}
	}
	return false
}
