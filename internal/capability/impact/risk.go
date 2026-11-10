package impact

import (
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Risk is a score that shows its work.
//
// Spec §2.8 requires any score RepoCortex emits to be explainable. That is not a
// presentation concern: a bare "Risk: HIGH" tells a developer to be careful
// without telling them what to be careful ABOUT, and the three things that
// drive it — public API, caller count, absent tests — call for three completely
// different responses. So Factors is not optional decoration attached to a
// number; the number is defined as the sum of the factors, and a factor with
// zero points is still listed because "this symbol is not exported" is an input
// the reader needs to see was considered.
//
// Certainty is deliberately separate from Level. They answer different
// questions: Level is "how much could break", Certainty is "how much should you
// trust that estimate". A high-risk score derived entirely from low-confidence
// edges and a high-risk score derived from compiler-proven ones are not the
// same finding, and collapsing them into one word would lose the distinction
// this whole package exists to preserve.
type Risk struct {
	// Level is low | moderate | high. Derived from Score by Formula.
	Level string `json:"level"`

	// Score is the sum of Factors, clamped to 0..100.
	Score int `json:"score"`

	// Factors are the inputs, in the order they were applied. Never empty.
	Factors []Factor `json:"factors"`

	// Formula states exactly how Level follows from Score, so the thresholds
	// are visible rather than buried in this file.
	Formula string `json:"formula"`

	// Certainty is how much the score itself should be trusted: high | medium |
	// low. Driven by the confidence mix of the impacted set and by whether the
	// traversal was truncated.
	Certainty string `json:"certainty"`

	// CertaintyReason gives the counts behind Certainty.
	CertaintyReason string `json:"certainty_reason"`

	// LowerBound is always true. The score is computed from a bounded traversal
	// over a partially resolved index, so it can only ever understate.
	LowerBound bool `json:"lower_bound"`
}

// Factor is one scored input with its justification.
type Factor struct {
	// Name is a stable machine-readable key (public_api, direct_callers, …).
	Name string `json:"name"`

	// Points is this factor's contribution to Score. May be negative — present
	// test coverage reduces risk — and may be zero, which is still reported.
	Points int `json:"points"`

	// Detail is the one-sentence explanation a human reads.
	Detail string `json:"detail"`

	// Evidence names the concrete fact the factor rests on.
	Evidence string `json:"evidence,omitempty"`
}

// Scoring weights. Stated as named constants rather than inline numbers so the
// model is auditable in one place — and so a future change to it is a visible
// diff rather than a tweak buried in an expression.
const (
	pointsExported       = 25 // public API: callers outside this repo are not in the index
	pointsPerDirect      = 5  // per direct caller …
	maxPointsDirect      = 25 // … capped, because 40 callers is not 8x worse than 5
	pointsIndirectDiv    = 5  // one point per 5 indirect symbols …
	maxPointsIndirect    = 20 // … capped for the same reason
	pointsNoTests        = 20 // nothing observed to cover this symbol
	pointsHasTests       = -10
	pointsTruncated      = 10 // the true blast radius is larger than measured
	pointsDynamicUnknown = 10 // reachable through dispatch we cannot see
	pointsDispatchReach  = 10 // implementations a call to the target can land on

	// pointsConventionTestsOnly sits between pointsNoTests and pointsHasTests
	// because that is exactly what convention-only coverage is: a test was found
	// by its NAME or its FILE, and no recorded edge says it exercises the
	// symbol. Scoring it as covered would let a filename discharge the risk.
	pointsConventionTestsOnly = 10

	riskModerateAt = 25 // Score >= this is moderate
	riskHighAt     = 60 // Score >= this is high
)

// riskFormula is the one statement of how Level follows from Score. Shared by
// the report-level and the symbol-level scorer so the two cannot drift into
// meaning different things by the same word.
func riskFormula() string {
	return fmt.Sprintf(
		"score = sum(factors), clamped to 0..100; level = low below %d, moderate below %d, high at or above %d",
		riskModerateAt, riskHighAt, riskHighAt)
}

func finishRisk(risk *Risk) {
	for _, f := range risk.Factors {
		risk.Score += f.Points
	}
	if risk.Score < 0 {
		risk.Score = 0
	}
	if risk.Score > 100 {
		risk.Score = 100
	}
	switch {
	case risk.Score >= riskHighAt:
		risk.Level = "high"
	case risk.Score >= riskModerateAt:
		risk.Level = "moderate"
	default:
		risk.Level = "low"
	}
}

// scoreRisk computes the risk block from a finished report.
//
// It is a pure function of the report on purpose: everything it uses is already
// visible to the reader, so there is no hidden input that could make the score
// unreproducible from what was printed.
func scoreRisk(r Report) Risk {
	risk := Risk{LowerBound: true, Formula: riskFormula()}

	// 1. Public API.
	if r.Target.Exported {
		risk.Factors = append(risk.Factors, Factor{
			Name: "public_api", Points: pointsExported,
			Detail: "Target is exported. Callers outside this repository are not indexed, " +
				"so the reported blast radius excludes them entirely.",
			Evidence: fmt.Sprintf("%s is exported (%s)", r.Target.FQN, r.Target.Kind),
		})
	} else {
		risk.Factors = append(risk.Factors, Factor{
			Name: "public_api", Points: 0,
			Detail:   "Target is unexported, so its blast radius is bounded by this repository.",
			Evidence: fmt.Sprintf("%s is not exported", r.Target.FQN),
		})
	}

	// 2. Direct callers.
	direct := len(r.Direct)
	dp := direct * pointsPerDirect
	if dp > maxPointsDirect {
		dp = maxPointsDirect
	}
	risk.Factors = append(risk.Factors, Factor{
		Name: "direct_callers", Points: dp,
		Detail: fmt.Sprintf("%d symbol(s) reference the target directly (depth 1). "+
			"Capped at %d points: caller count grows faster than risk does.", direct, maxPointsDirect),
		Evidence: fmt.Sprintf("%d direct impacted symbols in this report", direct),
	})

	// 3. Indirect reach.
	indirect := len(r.Indirect)
	ip := indirect / pointsIndirectDiv
	if ip > maxPointsIndirect {
		ip = maxPointsIndirect
	}
	risk.Factors = append(risk.Factors, Factor{
		Name: "indirect_reach", Points: ip,
		Detail: fmt.Sprintf("%d symbol(s) are reachable at depth 2 or more. "+
			"Weighted far below direct callers because a long path is a weaker claim.", indirect),
		Evidence: fmt.Sprintf("%d indirect impacted symbols in this report", indirect),
	})

	// 4. Observed test coverage. This is coverage OBSERVED IN THE GRAPH — a
	// test symbol that reaches the target — not a coverage report. It is
	// therefore evidence that something exercises the symbol, never evidence
	// that the symbol is well tested.
	tests, example := countTests(r)
	if tests > 0 {
		risk.Factors = append(risk.Factors, Factor{
			Name: "observed_tests", Points: pointsHasTests,
			Detail: fmt.Sprintf("%d of the impacted symbols are test code, so a change here is at least "+
				"partly guarded. This is reachability, not a coverage measurement.", tests),
			Evidence: "e.g. " + example,
		})
	} else {
		risk.Factors = append(risk.Factors, Factor{
			Name: "observed_tests", Points: pointsNoTests,
			Detail: "No test symbol reaches this target in the indexed graph. Either it is untested, " +
				"or its tests are invisible to static analysis (table-driven dispatch, generated tests, " +
				"a separate test repository).",
			Evidence: "0 impacted symbols classified as tests",
		})
	}

	// 5. Truncation.
	if r.Truncated {
		risk.Factors = append(risk.Factors, Factor{
			Name: "truncated_traversal", Points: pointsTruncated,
			Detail: "The traversal hit a bound, so the counts above are floors. " +
				"The score is raised because an unmeasured region is a risk, not an absence of one.",
			Evidence: r.TruncatedReason,
		})
	}

	// 6. Dynamic dispatch.
	if u, ok := findUnknown(r, UnknownDynamicDispatch); ok {
		risk.Factors = append(risk.Factors, Factor{
			Name: "dynamic_dispatch", Points: pointsDynamicUnknown,
			Detail: "The target participates in a subtype relationship, so calls can reach it through a " +
				"supertype without any edge recording that they do.",
			Evidence: u.Detail,
		})
	}

	// 7. Dispatch expansion. Scored separately from factor 6: that one fires
	// when the TARGET implements something and the callers of the supertype are
	// unknown, this one when something implements the TARGET and those
	// implementations were expanded. A symbol can have both, and they are
	// different reasons to be careful.
	if n := len(r.Dispatch); n > 0 {
		risk.Factors = append(risk.Factors, Factor{
			Name: "dispatch_reach", Points: pointsDispatchReach,
			Detail: fmt.Sprintf("%d symbol(s) are reachable only by dispatching through %d recorded "+
				"implementation(s) of the target. A change to the target's contract lands on all of them, "+
				"and no call site records which one it takes.", n, len(r.DispatchThrough)),
			Evidence: dispatchEvidence(r),
		})
	}

	finishRisk(&risk)
	risk.Certainty, risk.CertaintyReason = certainty(r)
	return risk
}

func dispatchEvidence(r Report) string {
	if len(r.DispatchThrough) == 0 {
		return ""
	}
	names := make([]string, 0, 3)
	for i, s := range r.DispatchThrough {
		if i == 3 {
			names = append(names, fmt.Sprintf("… and %d more", len(r.DispatchThrough)-3))
			break
		}
		names = append(names, fmt.Sprintf("%s (%s)", s.Name, s.FilePath))
	}
	return "implementations: " + strings.Join(names, ", ")
}

// Reach is how a symbol came to be in a blast radius. It is the same
// three-way distinction Report makes between Direct, Indirect and Dispatch,
// named so a per-symbol score can carry it.
type Reach string

const (
	ReachDirect   Reach = "direct"
	ReachIndirect Reach = "indirect"
	ReachDispatch Reach = "dispatch"
)

// Coverage is what is known about tests for one reached symbol.
type Coverage string

const (
	// CoverageProven: a recorded edge chain runs from a test symbol to this one.
	CoverageProven Coverage = "proven_by_graph"

	// CoverageConventionOnly: a test was found by naming convention or by
	// sibling file, and no edge says it exercises this symbol.
	CoverageConventionOnly Coverage = "convention_only"

	// CoverageNone: no signal matched. Not proof of absence — see the caveats on
	// any report carrying this.
	CoverageNone Coverage = "none"

	// CoverageNotExamined: the symbol is in the blast radius but coverage was
	// not looked up for it, because the examination budget ran out.
	CoverageNotExamined Coverage = "not_examined"
)

// ReachedSymbol is one symbol in a blast radius, with the facts the risk model
// scores. It is the input to ScoreReachedSymbol.
type ReachedSymbol struct {
	Symbol   capability.Symbol
	Reach    Reach
	Depth    int
	Coverage Coverage

	// TestCount is how many tests were discovered for the symbol, whatever the
	// signal. Reported in the factor's evidence, never as points on its own.
	TestCount int

	// Confidence is the path minimum that reached the symbol.
	Confidence model.Confidence
}

// ScoreReachedSymbol scores ONE symbol in a blast radius, using the same
// factors, weights, thresholds and Formula as the report-level score.
//
// It exists so a test-gap report can rank uncovered symbols without inventing a
// second notion of risk. The two scorers necessarily read different inputs — a
// report knows caller counts, a single reached symbol knows its distance and
// its coverage — but a "high" from one means the same arithmetic as a "high"
// from the other, because both sum the constants above and both call
// finishRisk.
//
// Like every score here it shows its work: Factors is never empty, and a factor
// worth zero points is still listed so the reader can see it was considered.
func ScoreReachedSymbol(in ReachedSymbol) Risk {
	risk := Risk{LowerBound: true, Formula: riskFormula()}

	if in.Symbol.Exported {
		risk.Factors = append(risk.Factors, Factor{
			Name: "public_api", Points: pointsExported,
			Detail: "Symbol is exported, so its own callers may live outside this repository and " +
				"outside this index.",
			Evidence: fmt.Sprintf("%s is exported (%s)", in.Symbol.FQN, in.Symbol.Kind),
		})
	} else {
		risk.Factors = append(risk.Factors, Factor{
			Name: "public_api", Points: 0,
			Detail:   "Symbol is unexported, so what it affects is bounded by this repository.",
			Evidence: fmt.Sprintf("%s is not exported", in.Symbol.FQN),
		})
	}

	// Proximity. Depth 1 is worth the same as the report-level cap on direct
	// callers, and each further hop divides it, because a longer path is a
	// weaker claim that this symbol is affected at all.
	depth := in.Depth
	if depth < 1 {
		depth = 1
	}
	risk.Factors = append(risk.Factors, Factor{
		Name: "proximity", Points: maxPointsDirect / depth,
		Detail: fmt.Sprintf("Reached at depth %d (%s). Closer symbols are more likely to be genuinely "+
			"affected, so distance divides the points rather than adding them.", depth, in.Reach),
		Evidence: fmt.Sprintf("%s is %d hop(s) from the changed symbol", in.Symbol.Name, depth),
	})

	if in.Reach == ReachDispatch {
		risk.Factors = append(risk.Factors, Factor{
			Name: "dispatch_reach", Points: pointsDispatchReach,
			Detail: "Reached by dispatching through a declared implementation rather than by a recorded " +
				"call. Whether a given call site takes this path cannot be determined statically, which " +
				"is a reason for caution and not a reason to discount it.",
			Evidence: fmt.Sprintf("%s was reached across an IMPLEMENTS/EXTENDS edge", in.Symbol.Name),
		})
	}

	switch in.Coverage {
	case CoverageProven:
		risk.Factors = append(risk.Factors, Factor{
			Name: "observed_tests", Points: pointsHasTests,
			Detail: "A recorded edge chain reaches this symbol from a test, so a change to it is at least " +
				"partly guarded. This is reachability, not a coverage measurement.",
			Evidence: fmt.Sprintf("%d test(s) discovered, at least one proven by the graph", in.TestCount),
		})
	case CoverageConventionOnly:
		risk.Factors = append(risk.Factors, Factor{
			Name: "observed_tests", Points: pointsConventionTestsOnly,
			Detail: "The only tests found for this symbol were matched by naming convention or by sibling " +
				"file. Nothing recorded says they exercise it, so they reduce the risk less than a proven " +
				"edge chain would.",
			Evidence: fmt.Sprintf("%d test(s) discovered, none proven by the graph", in.TestCount),
		})
	case CoverageNotExamined:
		risk.Factors = append(risk.Factors, Factor{
			Name: "observed_tests", Points: 0,
			Detail: "Coverage was not looked up for this symbol — the examination budget ran out before " +
				"reaching it. Scored as neither covered nor uncovered, because it is neither known.",
			Evidence: "not examined",
		})
	default:
		risk.Factors = append(risk.Factors, Factor{
			Name: "observed_tests", Points: pointsNoTests,
			Detail: "No test was found for this symbol by call graph, naming convention or sibling file. " +
				"Either it is untested, or its tests are invisible to static analysis.",
			Evidence: "0 tests discovered",
		})
	}

	finishRisk(&risk)
	risk.Certainty, risk.CertaintyReason = symbolCertainty(in)
	return risk
}

// symbolCertainty describes how much this one symbol's score should be trusted.
//
// Kept separate from the score for the same reason the report-level one is:
// "this is probably affected and definitely untested" and "this might be
// affected and is definitely untested" are different findings.
func symbolCertainty(in ReachedSymbol) (string, string) {
	base := fmt.Sprintf("reached at depth %d along a path whose weakest edge is %s",
		in.Depth, orUnrecorded(string(in.Confidence)))
	switch {
	case in.Coverage == CoverageNotExamined:
		return "low", base + ", and its coverage was never examined, so the score rests on reachability alone."
	case in.Reach == ReachDispatch:
		return "low", base + ". It was reached by dispatch, so it is possibly affected rather than " +
			"provably affected."
	case in.Confidence.AtLeast(model.ConfidenceHigh) && in.Depth == 1:
		return "high", base + ", which is the strongest claim this index makes."
	case in.Confidence.AtLeast(model.ConfidenceHigh):
		return "medium", base + "; the individual hops are strong but the chain is not depth 1."
	default:
		return "low", base + ", so the reachability itself is uncertain."
	}
}

func orUnrecorded(s string) string {
	if s == "" {
		return "unrecorded"
	}
	return s
}

// certainty describes how much the score should be trusted, from the confidence
// mix of the evidence it was computed from.
//
// Separate from the score because they can move in opposite directions: a
// symbol with 200 low-confidence callers scores high and deserves little trust,
// while one with 3 compiler-proven callers scores low and deserves a lot.
func certainty(r Report) (string, string) {
	var strong, weak int
	for _, list := range [][]Impacted{r.Direct, r.Indirect} {
		for _, imp := range list {
			if imp.Confidence.AtLeast(model.ConfidenceHigh) {
				strong++
			} else {
				weak++
			}
		}
	}
	total := strong + weak

	switch {
	case total == 0 && r.Truncated:
		return "low", "No impacted symbols were found, but the traversal was truncated — absence of results " +
			"here is not evidence of absence of impact."
	case total == 0:
		return "low", fmt.Sprintf(
			"No impacted symbols were found. With %.1f%% of this run's bindable references "+
				"bound, an empty result is as likely to mean 'not indexed' as 'not used'.",
			100*r.Completeness.BindingRate)
	}

	reason := fmt.Sprintf(
		"%d of %d impacted symbols were reached along a path whose weakest edge is high or exact; "+
			"%d were reached only through medium- or low-confidence edges. Reference resolution for this run: "+
			"%.1f%% of the references that name something this index defines.",
		strong, total, weak, 100*r.Completeness.BindingRate)
	if r.Truncated {
		reason += " The traversal was also truncated, so this mix describes only the part that was explored."
	}

	switch {
	case r.Truncated, weak*2 > total:
		return "low", reason
	// THE THRESHOLD NOW MEASURES WHAT IT ALWAYS MEANT. It gates "high" on the
	// index having understood enough of the repository to trust an empty or
	// narrow impact result. Read over EVERY reference it could not do that: a
	// service that calls a lot of third-party code could never reach 0.8
	// however perfectly it bound its own, so the gate was partly a measure of
	// dependency surface. Over bindable references it is the intended test, and
	// the constant is unchanged so the intent is visible in the diff.
	case strong == total && r.Completeness.BindingRate >= 0.8:
		return "high", reason
	default:
		return "medium", reason
	}
}

func countTests(r Report) (int, string) {
	var n int
	var example string
	for _, list := range [][]Impacted{r.Direct, r.Indirect} {
		for _, imp := range list {
			if !imp.IsTest {
				continue
			}
			n++
			if example == "" {
				example = fmt.Sprintf("%s (%s)", imp.Name, imp.FilePath)
			}
		}
	}
	return n, example
}

func findUnknown(r Report, kind UnknownKind) (Unknown, bool) {
	for _, u := range r.Unknown {
		if u.Kind == kind {
			return u, true
		}
	}
	return Unknown{}, false
}
