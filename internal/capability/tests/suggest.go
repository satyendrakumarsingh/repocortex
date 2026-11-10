package tests

import (
	"fmt"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// SuggestionKind names a structural coverage gap.
type SuggestionKind string

const (
	// GapPublicAPIUntested: an exported symbol with no discovered test.
	GapPublicAPIUntested SuggestionKind = "public_api_untested"

	// GapErrorPathUntested: the target provably calls into error construction,
	// and nothing was found that exercises it.
	GapErrorPathUntested SuggestionKind = "error_path_untested"

	// GapNoProvenExecution: tests exist, but only naming and co-location found
	// them. Nothing in the graph shows any of them actually reaching the
	// target — so the coverage claim rests entirely on convention.
	GapNoProvenExecution SuggestionKind = "no_proven_execution"
)

// Suggestion is a structural coverage gap, with the evidence that justifies it.
type Suggestion struct {
	Kind SuggestionKind `json:"kind"`

	// Scenario is what to test, in one line.
	Scenario string `json:"scenario"`

	// Why is the structural argument for the gap existing.
	Why string `json:"why"`

	// Evidence is the specific fact in the index or graph that the suggestion
	// rests on. A suggestion without one must not be emitted.
	Evidence string `json:"evidence"`

	// Confidence is how strongly the GAP is believed to be real — not how
	// important the test would be. A gap derived from "this symbol is exported
	// and no signal matched" is only as good as the discovery that found
	// nothing.
	Confidence model.Confidence `json:"confidence"`
}

// SuggestMissingScenarios returns structural coverage gaps for target.
//
// # What this deliberately does NOT do
//
// It does not read names or doc comments and imagine scenarios from them. Spec
// §23 forbids generating suggestions from keywords alone, and for a good
// reason: a suggestion invented from the word "payment" in a function name is
// indistinguishable, to the reader, from one derived from an actual gap in the
// graph — and once one fabricated suggestion is in the list, none of them can be
// trusted.
//
// Every suggestion here is therefore traceable to a fact already printed in the
// report: the target's export status, a recorded CALLS edge into error
// construction, or the complete absence of a graph-proven test. Branch coverage
// and state-transition scenarios are NOT emitted, even though the spec mentions
// them as desirable, because RepoCortex has no control-flow graph — there is no
// evidence in the index from which to justify them, so emitting them would be
// invention.
//
// Returning nil is a correct and common outcome. It means no gap could be
// justified, not that the code is well tested.
func SuggestMissingScenarios(target capability.Target, report TestReport) []Suggestion {
	var out []Suggestion

	// 1. Exported symbol with nothing covering it. The export flag is a parsed
	//    fact and the absence is the report's own finding, so both halves are
	//    visible to the reader.
	if target.Exported && len(report.Tests) == 0 {
		out = append(out, Suggestion{
			Kind: GapPublicAPIUntested,
			Scenario: fmt.Sprintf("A test that calls %s directly and asserts its result for at least one "+
				"representative input.", target.Name),
			Why: "This symbol is part of the repository's public surface, so a behaviour change here is a " +
				"breaking change for callers that are not in this index at all.",
			Evidence: fmt.Sprintf("%s (%s in %s) is exported, and no test was found by the call graph, "+
				"by naming convention, or in a sibling test file.",
				target.FQN, target.Kind, target.FilePath),
			// Medium, not high: the gap is only as certain as the discovery
			// that found nothing, and discovery cannot see dynamic invocation.
			Confidence: model.ConfidenceMedium,
		})
	}

	// 2. Proven error paths with no proven test. Requires the CALLS edge — a
	//    name that looks like an error constructor with no edge behind it
	//    produces nothing.
	if len(report.ErrorPaths) > 0 && report.ProvenByGraph == 0 {
		for _, ep := range report.ErrorPaths {
			out = append(out, Suggestion{
				Kind: GapErrorPathUntested,
				Scenario: fmt.Sprintf("A test that drives %s down the path that reaches %s, and asserts the "+
					"error it produces.", target.Name, ep.Callee.Name),
				Why: "Error paths are the branches most likely to be wrong and least likely to be exercised " +
					"by a happy-path test, and no test in this index is shown to reach this symbol at all.",
				Evidence: fmt.Sprintf("recorded CALLS edge %s -> %s (confidence=%s) at %s:%d; "+
					"0 tests reach %s through the graph",
					target.Name, ep.Callee.Name, ep.Confidence,
					ep.Evidence.FilePath, ep.Evidence.Range.Start.Line, target.Name),
				// The edge is proven; the classification of the callee as
				// error-constructing is a convention, so the pair is medium.
				Confidence: minConf(ep.Confidence, model.ConfidenceMedium),
			})
		}
	}

	// 3. Tests exist, but none is proven to execute the target.
	if len(report.Tests) > 0 && report.ProvenByGraph == 0 {
		out = append(out, Suggestion{
			Kind: GapNoProvenExecution,
			Scenario: fmt.Sprintf("Confirm that one of the %d candidate test(s) actually exercises %s — "+
				"or add one that calls it directly.", len(report.Tests), target.Name),
			Why: "Every candidate was found by naming convention or file layout. Nothing in the graph shows " +
				"any of them reaching this symbol, so the apparent coverage may be entirely nominal.",
			Evidence: fmt.Sprintf("%d test(s) discovered, %d by naming convention, %d by sibling test file, "+
				"0 by a recorded edge chain.",
				len(report.Tests), report.SignalCounts[SignalNaming], report.SignalCounts[SignalColocation]),
			Confidence: model.ConfidenceHigh, // the absence of graph edges is itself a recorded fact
		})
	}
	return out
}

func minConf(a, b model.Confidence) model.Confidence {
	if b.Rank() < a.Rank() {
		return b
	}
	return a
}
