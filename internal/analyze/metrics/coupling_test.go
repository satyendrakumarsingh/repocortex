package metrics

import (
	"context"
	"math"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func testGraph() Graph {
	return NewGraph(
		[]Node{{Key: "a"}, {Key: "b"}, {Key: "c"}},
		[]Edge{
			{From: "a", To: "b", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
			{From: "a", To: "c", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
			{From: "b", To: "c", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
		},
	)
}

func TestCouplingInstability(t *testing.T) {
	got, err := CouplingOf(context.Background(), testGraph())
	if err != nil {
		t.Fatalf("CouplingOf: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// Results are key-ascending: a, b, c.
	// a: fanIn 0, fanOut 2 -> I = 2/2 = 1.0  (maximally unstable)
	// b: fanIn 1, fanOut 1 -> I = 1/2 = 0.5
	// c: fanIn 2, fanOut 0 -> I = 0/2 = 0.0  (maximally stable)
	want := []float64{1.0, 0.5, 0.0}
	for i, w := range want {
		if math.Abs(got[i].Instability-w) > 1e-9 {
			t.Errorf("%s Instability = %v, want %v", got[i].Key, got[i].Instability, w)
		}
	}
	if got[0].Key != "a" || got[2].Key != "c" {
		t.Errorf("results not key-ascending: %v", got)
	}
}

func TestCouplingIsolatedNodeIsStableNotNaN(t *testing.T) {
	g := NewGraph([]Node{{Key: "lonely"}}, nil)
	got, err := CouplingOf(context.Background(), g)
	if err != nil {
		t.Fatalf("CouplingOf: %v", err)
	}
	if got[0].Instability != 0 {
		t.Fatalf("isolated Instability = %v, want 0 (not NaN)", got[0].Instability)
	}
}
