package architecture

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability/impact"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Confidence is the one invariant the whole product rests on, and until this
// file existed nothing executed it.
//
// Every claim RepoCortex makes carries a tier, and each tier has a MEASURED
// meaning (docs/accuracy.md): on RepoCortex's own source, `high` is 97.8%,
// `medium` 34.1% and `low` 0.0%. Those numbers are what let a consumer act on
// one tier and ignore another. A refactor that quietly reordered the ranks, let
// an unproven edge call itself `exact`, or dropped the traversal floor would not
// fail a single existing test — it would just start telling agents that guesses
// are facts, and the first symptom would be a wrong refactor in someone's
// repository.
//
// These are law tests: they pin SEMANTICS, not implementation. Each one names
// the consequence of its own failure, so a future reader who wants to change the
// behaviour knows what they are trading away rather than deleting an assertion
// that looks arbitrary.

// TestTierOrderIsTotalAndStrict pins the ordering every filter depends on.
//
// `AtLeast` is what implements `--min-confidence` everywhere. If two tiers ever
// compared equal, a floor would admit the tier below it and the report would
// silently widen.
func TestTierOrderIsTotalAndStrict(t *testing.T) {
	descending := []model.Confidence{
		model.ConfidenceExact,
		model.ConfidenceHigh,
		model.ConfidenceMedium,
		model.ConfidenceLow,
	}
	for i := 0; i < len(descending)-1; i++ {
		hi, lo := descending[i], descending[i+1]
		if hi.Rank() <= lo.Rank() {
			t.Errorf("%s ranks %d, %s ranks %d: the order is not strict, so a floor at %s admits %s",
				hi, hi.Rank(), lo, lo.Rank(), lo, hi)
		}
		if !hi.AtLeast(lo) {
			t.Errorf("%s.AtLeast(%s) is false: --min-confidence %s would exclude %s", hi, lo, lo, hi)
		}
		if lo.AtLeast(hi) {
			t.Errorf("%s.AtLeast(%s) is true: --min-confidence %s would admit %s", lo, hi, hi, lo)
		}
	}
}

// TestAnUnknownTierIsNeverAdmitted: a typo must fail closed.
//
// Rank() returns 0 for anything unrecognised, so an unknown tier is below every
// real one and Valid() rejects it. The failure this prevents is a mis-spelled
// confidence in stored data reading as "at least low" and being traversed.
func TestAnUnknownTierIsNeverAdmitted(t *testing.T) {
	for _, junk := range []model.Confidence{"", "certain", "HIGH", "very high", "0.85"} {
		if junk.Valid() {
			t.Errorf("%q reports itself valid", junk)
		}
		if junk.AtLeast(model.ConfidenceLow) {
			t.Errorf("%q passes a floor of low; an unrecognised tier must fail closed", junk)
		}
	}
}

// TestOnlyTheCompilerProducesExact is the law that makes `exact` mean anything.
//
// `exact` is the tier a consumer is told to act on without checking. The moment
// a heuristic can label itself `exact`, the distinction between a proof and a
// guess stops existing anywhere in the product — including in `cortex verify`,
// where it is exactly what separates a FACT from a LEAD.
func TestOnlyTheCompilerProducesExact(t *testing.T) {
	notCompiler := []model.SourceType{
		model.SourceSourceCode,
		model.SourceInferred,
		model.SourceGit,
	}
	for _, src := range notCompiler {
		e := model.Edge{
			Kind: model.EdgeCalls, SrcID: 1, DstID: 2,
			Confidence: model.ConfidenceExact,
			Evidence:   model.Evidence{Source: src, FilePath: "a.go"},
		}
		if err := e.Validate(); err == nil {
			t.Errorf("an edge sourced from %q was allowed to claim `exact`; "+
				"`exact` must mean a compiler proved it, or it means nothing", src)
		}
	}

	ok := model.Edge{
		Kind: model.EdgeCalls, SrcID: 1, DstID: 2,
		Confidence: model.ConfidenceExact,
		Evidence:   model.Evidence{Source: model.SourceCompiler, FilePath: "a.go"},
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("a compiler-sourced exact edge was rejected: %v", err)
	}
}

// TestImpactTraversalFloorStaysAtMedium pins the highest-value accuracy decision
// in the plan (A17).
//
// Impact traversal used to have no floor, so a blast radius expanded through
// `low` edges measured at 0.0–2.8% precision. For a change-safety product,
// shipping 97%-wrong edges as blast radius is the worst failure available: the
// agent is told to go and update callers that do not exist.
//
// The floor is `medium`, not `high`, deliberately — `medium` is shown and never
// acted on, and dropping it would hide real reach. And the edges below the floor
// are FILTERED, never deleted: `--min-confidence low` brings them back, and the
// report states how many were withheld.
func TestImpactTraversalFloorStaysAtMedium(t *testing.T) {
	got := impact.DefaultMinConfidence

	if got != model.ConfidenceMedium {
		t.Fatalf("default impact floor is %q, want %q. Lowering it re-admits `low` edges, "+
			"measured at 0.0-2.8%%; raising it to `high` silently drops real reach.",
			got, model.ConfidenceMedium)
	}
	if model.ConfidenceLow.AtLeast(got) {
		t.Error("a `low` edge passes the default impact floor")
	}
	if !model.ConfidenceMedium.AtLeast(got) || !model.ConfidenceHigh.AtLeast(got) {
		t.Error("the default floor excludes medium or high, which it must not")
	}
}
