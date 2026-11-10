package graph

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestEdgeCostIsMonotonicInConfidence(t *testing.T) {
	exact := EdgeCost(model.ConfidenceExact)
	high := EdgeCost(model.ConfidenceHigh)
	medium := EdgeCost(model.ConfidenceMedium)
	low := EdgeCost(model.ConfidenceLow)

	if exact >= high || high >= medium || medium >= low {
		t.Fatalf("cost must rise as confidence falls: exact=%d high=%d medium=%d low=%d",
			exact, high, medium, low)
	}
	if exact < 1 {
		t.Errorf("even a proven edge must cost at least 1 hop, got %d", exact)
	}
}

// The property that makes this worth having: a SHORT path through guesses must
// not automatically beat a LONGER path through proofs. Two low edges cost more
// than three exact ones.
func TestTwoLowEdgesCostMoreThanThreeExactEdges(t *testing.T) {
	guesses := PathCost([]model.Confidence{model.ConfidenceLow, model.ConfidenceLow})
	proofs := PathCost([]model.Confidence{
		model.ConfidenceExact, model.ConfidenceExact, model.ConfidenceExact,
	})
	if guesses <= proofs {
		t.Errorf("two low edges (%d) must cost more than three exact edges (%d)", guesses, proofs)
	}
}

func TestPathCostOfNoEdgesIsZero(t *testing.T) {
	if got := PathCost(nil); got != 0 {
		t.Errorf("PathCost(nil) = %d, want 0", got)
	}
}

// An unset or unrecognised confidence must cost the most, not the least.
// Treating unknown provenance as proven is the failure mode worth guarding.
func TestUnknownConfidenceCostsAsMuchAsLow(t *testing.T) {
	if got, want := EdgeCost(model.Confidence("")), EdgeCost(model.ConfidenceLow); got != want {
		t.Errorf("EdgeCost(unset) = %d, want %d (same as low)", got, want)
	}
	if got, want := EdgeCost(model.Confidence("bogus")), EdgeCost(model.ConfidenceLow); got != want {
		t.Errorf("EdgeCost(bogus) = %d, want %d (same as low)", got, want)
	}
}
