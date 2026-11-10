// Package testgap joins the two questions the other capabilities answer
// separately: "what does changing this reach" and "what covers that".
//
// impact answers the first, tests answers the second, and until this package
// existed the join was left to the reader — who had to run an impact analysis,
// copy each symbol out of it, ask for its tests one at a time, and remember
// which ones came back empty. That is the mechanical part of preparing to make
// a change, and it is exactly the part a query layer should do.
//
// # What it is, and what it is not
//
// It is a JOIN over two existing reports. It parses nothing, writes nothing and
// owns no schema, like everything else under internal/capability. It does not
// re-implement traversal, test discovery or risk scoring: impact.AnalyzeWith
// produces the blast radius, tests.FindTestsWith answers coverage per symbol,
// and impact.ScoreReachedSymbol ranks what is left over — the same constants,
// the same thresholds and the same Formula the impact report prints, so a
// "high" here and a "high" there are the same arithmetic.
//
// It is NOT a coverage tool. Nothing in this package executes a test or reads a
// coverage profile. "Covered" here means a test symbol was DISCOVERED for the
// symbol, and the report always says by which signal — a recorded edge chain is
// evidence of execution, a matching filename is evidence of intent. Those are
// kept apart (CoverageProven vs CoverageConventionOnly) because collapsing them
// would let a naming convention discharge a risk.
//
// # Bounded, and honest about the bound
//
// A blast radius can hold thousands of symbols and each coverage lookup is a
// traversal, so examination is capped (Options.MaxExamine). The symbols nearest
// the change are examined first and the remainder is reported as
// CoverageNotExamined with a count — never as uncovered, and never silently
// dropped. "Not looked at" and "looked at and found nothing" are different
// answers and this package refuses to render them the same way.
package testgap

import (
	"context"
	"fmt"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/internal/capability/tests"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// DefaultMaxExamine bounds how many reached symbols get a coverage lookup.
//
// Each lookup is one bounded traversal plus a handful of index probes —
// measured at ~1.5 ms per symbol on the 100k-symbol fixture in
// capability/tests' benchmarks — so 200 is a few hundred milliseconds in the
// worst case, and this query sits in front of an interactive agent. Symbols
// beyond it are reported, not hidden; see Report.NotExamined.
const DefaultMaxExamine = 200

// Options bounds one test-gap analysis.
type Options struct {
	// Impact bounds the blast radius. Its confidence floor governs this report
	// too: a symbol the floor kept out of the impact report is not examined
	// here either, and Report.LowConfidenceHidden carries the count through.
	Impact impact.Options

	// Tests bounds each per-symbol coverage lookup.
	Tests tests.Options

	// MaxExamine caps coverage lookups. <= 0 uses DefaultMaxExamine.
	MaxExamine int
}

func (o Options) withDefaults() Options {
	if o.MaxExamine <= 0 {
		o.MaxExamine = DefaultMaxExamine
	}
	return o
}

// CoveringTest is one discovered test, reduced to what a gap report needs.
//
// The full tests.TestHit carries every signal that matched; repeating all of
// them for every covered symbol in a 200-symbol report would bury the finding.
// The strongest signal is kept, with its own detail, so the claim stays
// checkable.
type CoveringTest struct {
	capability.Symbol

	Signal     tests.Signal     `json:"signal"`
	Detail     string           `json:"detail"`
	Confidence model.Confidence `json:"confidence"`
}

// Reached is one symbol in the blast radius, with its coverage verdict.
type Reached struct {
	capability.Symbol

	// Reach and Depth say how the change gets here: a direct caller, something
	// further out, or an implementation reached by dispatch. Carried through
	// from the impact report unchanged.
	Reach impact.Reach `json:"reach"`
	Depth int          `json:"depth"`

	// Confidence is the path minimum that reached this symbol.
	Confidence model.Confidence `json:"confidence"`

	// Coverage is the verdict. CoverageNone is a finding; CoverageNotExamined is
	// the absence of one.
	Coverage impact.Coverage `json:"coverage"`

	// Tests are the discovered tests, strongest signal first. Empty for
	// CoverageNone and CoverageNotExamined.
	Tests []CoveringTest `json:"tests,omitempty"`

	// Risk is populated for uncovered and convention-only symbols — the ones a
	// reader has to act on — and scored by impact.ScoreReachedSymbol so it uses
	// the same model as the impact report's own score.
	Risk *impact.Risk `json:"risk,omitempty"`

	// IsTest marks the reached symbol as test code itself. A test in the blast
	// radius that has no tests of its own is not a gap worth ranking, and saying
	// why beats silently omitting it.
	IsTest bool `json:"is_test"`
}

// Report is one test-gap answer: the blast radius, split by whether anything
// covers it.
type Report struct {
	Target capability.Target `json:"target"`

	// Reached is how many symbols the change reaches — the denominator every
	// other number here is a fraction of. Callers plus the dispatch expansion.
	Reached int `json:"reached"`

	// Examined is how many of them got a coverage lookup.
	Examined int `json:"examined"`

	// NotExamined is Reached - Examined: symbols in the radius whose coverage is
	// unknown because the budget ran out. Listed in NotExaminedSymbols up to the
	// same budget so the reader can re-ask about them.
	NotExamined        int       `json:"not_examined"`
	NotExaminedSymbols []Reached `json:"not_examined_symbols,omitempty"`

	// CoveredProven and CoveredByConventionOnly split "covered" by the strength
	// of the evidence. A reader who treats the second as the first has been
	// reassured by a filename.
	CoveredProven           int `json:"covered_proven"`
	CoveredByConventionOnly int `json:"covered_by_convention_only"`

	// Covered lists them, proven first.
	Covered []Reached `json:"covered,omitempty"`

	// Uncovered is the answer to the question this package exists for: reached,
	// examined, and nothing found. Ranked by Risk.Score descending, then by
	// depth, then by symbol id so the order is stable.
	Uncovered []Reached `json:"uncovered,omitempty"`

	// TestSymbolsReached counts reached symbols that are themselves test code.
	// They are excluded from Uncovered — a test with no test is not a gap — and
	// counted here so the exclusion is visible rather than a silent filter.
	TestSymbolsReached int `json:"test_symbols_reached"`

	// Unknown is carried through from the impact report unchanged: the gaps in
	// the blast radius are gaps in this report too, and re-deriving them would
	// let the two disagree.
	Unknown []impact.Unknown `json:"unknown"`

	// LowConfidenceHidden is impact's count of symbols the confidence floor kept
	// out of the radius. They were never examined for coverage either, so a
	// reader sizing this report's blind spots needs it here.
	LowConfidenceHidden int              `json:"low_confidence_hidden,omitempty"`
	ConfidenceFloor     model.Confidence `json:"confidence_floor,omitempty"`

	// Risk is the impact report's own risk score for the target, carried
	// through. The per-symbol scores rank the gaps; this one describes the
	// change as a whole.
	Risk impact.Risk `json:"risk"`

	Completeness capability.Completeness `json:"completeness"`

	Truncated       bool   `json:"truncated"`
	TruncatedReason string `json:"truncated_reason,omitempty"`
}

// UncoveredCaveat is carried by every gap report. It states the one thing a
// reader is most likely to get wrong about it.
const UncoveredCaveat = "\"Uncovered\" here means NO TEST WAS DISCOVERED for the symbol by call graph, naming " +
	"convention or sibling test file — not that the symbol is untested. Equally, \"covered\" means a test was " +
	"discovered, never that it asserts anything useful: this report counts discovered tests, it does not run " +
	"them and it does not read a coverage profile."

// Analyze reports which of the symbols a change reaches are covered by a test.
func Analyze(ctx context.Context, deps capability.Deps, runID int64, target string) (Report, error) {
	return AnalyzeWith(ctx, deps, runID, target, Options{})
}

// AnalyzeWith is Analyze under explicit bounds.
func AnalyzeWith(ctx context.Context, deps capability.Deps, runID int64, target string, opts Options) (Report, error) {
	if err := deps.Validate(); err != nil {
		return Report{}, err
	}
	opts = opts.withDefaults()

	imp, err := impact.AnalyzeWith(ctx, deps, runID, target, opts.Impact)
	if err != nil {
		return Report{}, err
	}

	rep := Report{
		Target:              imp.Target,
		Reached:             imp.TotalReached(),
		Unknown:             imp.Unknown,
		LowConfidenceHidden: imp.LowConfidenceHidden,
		ConfidenceFloor:     imp.ConfidenceFloor,
		Risk:                imp.Risk,
		Completeness:        imp.Completeness,
		Truncated:           imp.Truncated,
		TruncatedReason:     imp.TruncatedReason,
	}
	rep.Completeness.Caveats = append(rep.Completeness.Caveats, UncoveredCaveat)

	// Nearest first. The examination budget is spent where the claim is
	// strongest, so if it runs out it runs out on the symbols furthest from the
	// change rather than on an arbitrary slice.
	candidates := flatten(imp)

	for i, cand := range candidates {
		if cand.IsTest {
			rep.TestSymbolsReached++
		}
		if i >= opts.MaxExamine {
			cand.Coverage = impact.CoverageNotExamined
			rep.NotExamined++
			rep.NotExaminedSymbols = append(rep.NotExaminedSymbols, cand)
			continue
		}
		rep.Examined++

		// The FQN, not the bare name: a bare name that several symbols share
		// would come back as capability.AmbiguousTargetError, and a gap report
		// that fails because two packages both define `Close` would be useless
		// on any real repository.
		tr, err := tests.FindTestsWith(ctx, deps, runID, cand.FQN, opts.Tests)
		if err != nil {
			return Report{}, fmt.Errorf("testgap: coverage of %s: %w", cand.FQN, err)
		}
		cand.Coverage, cand.Tests = classify(tr)

		switch cand.Coverage {
		case impact.CoverageProven:
			rep.CoveredProven++
			rep.Covered = append(rep.Covered, cand)
		case impact.CoverageConventionOnly:
			rep.CoveredByConventionOnly++
			cand.Risk = scoreOf(cand, len(cand.Tests))
			rep.Covered = append(rep.Covered, cand)
		default:
			if cand.IsTest {
				// A test symbol with no tests of its own is not a coverage gap,
				// and ranking it would push real gaps down the list. It is still
				// counted in TestSymbolsReached above.
				continue
			}
			cand.Risk = scoreOf(cand, 0)
			rep.Uncovered = append(rep.Uncovered, cand)
		}
	}

	sortByRisk(rep.Uncovered)
	sortCovered(rep.Covered)
	return rep, nil
}

// flatten turns the impact report's three sets into one list, nearest first,
// keeping the label that says which set each symbol came from.
//
// The label is the reason this is not a simple concatenation: a dispatch row
// must stay identifiable as a dispatch row all the way to the ranked output,
// because "an implementation of the changed interface has no tests" and "a
// caller of the changed function has no tests" are different findings and
// ScoreReachedSymbol weighs them differently.
func flatten(imp impact.Report) []Reached {
	out := make([]Reached, 0, imp.TotalReached())
	add := func(sym capability.Symbol, reach impact.Reach, depth int, conf model.Confidence, isTest bool) {
		out = append(out, Reached{
			Symbol: sym, Reach: reach, Depth: depth, Confidence: conf, IsTest: isTest,
		})
	}
	for _, d := range imp.Direct {
		add(d.Symbol, impact.ReachDirect, d.Depth, d.Confidence, d.IsTest)
	}
	for _, d := range imp.Indirect {
		add(d.Symbol, impact.ReachIndirect, d.Depth, d.Confidence, d.IsTest)
	}
	for _, d := range imp.Dispatch {
		add(d.Symbol, impact.ReachDispatch, d.Depth, d.Confidence, d.IsTest)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Depth != out[j].Depth {
			return out[i].Depth < out[j].Depth
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// classify turns a test report into a coverage verdict.
//
// Proven requires a GRAPH signal on at least one discovered test. Anything else
// found only by convention is convention-only, however many conventions agreed:
// three filenames pointing the same way are still three filenames.
func classify(tr tests.TestReport) (impact.Coverage, []CoveringTest) {
	if tr.Untested || len(tr.Tests) == 0 {
		return impact.CoverageNone, nil
	}
	out := make([]CoveringTest, 0, len(tr.Tests))
	for _, t := range tr.Tests {
		out = append(out, CoveringTest{
			Symbol:     t.Symbol,
			Signal:     t.Strongest,
			Detail:     t.Signals[0].Detail,
			Confidence: t.Confidence,
		})
	}
	if tr.ProvenByGraph > 0 {
		return impact.CoverageProven, out
	}
	return impact.CoverageConventionOnly, out
}

func scoreOf(r Reached, testCount int) *impact.Risk {
	risk := impact.ScoreReachedSymbol(impact.ReachedSymbol{
		Symbol:     r.Symbol,
		Reach:      r.Reach,
		Depth:      r.Depth,
		Coverage:   r.Coverage,
		TestCount:  testCount,
		Confidence: r.Confidence,
	})
	return &risk
}

func sortByRisk(in []Reached) {
	sort.SliceStable(in, func(i, j int) bool {
		si, sj := score(in[i]), score(in[j])
		if si != sj {
			return si > sj
		}
		if in[i].Depth != in[j].Depth {
			return in[i].Depth < in[j].Depth
		}
		return in[i].ID < in[j].ID
	})
}

func score(r Reached) int {
	if r.Risk == nil {
		return 0
	}
	return r.Risk.Score
}

// sortCovered puts the proven coverage first, so a reader skimming the list
// meets the strong evidence before the conventional kind.
func sortCovered(in []Reached) {
	sort.SliceStable(in, func(i, j int) bool {
		if (in[i].Coverage == impact.CoverageProven) != (in[j].Coverage == impact.CoverageProven) {
			return in[i].Coverage == impact.CoverageProven
		}
		if in[i].Depth != in[j].Depth {
			return in[i].Depth < in[j].Depth
		}
		return in[i].ID < in[j].ID
	})
}
