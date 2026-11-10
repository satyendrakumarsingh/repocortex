package metrics

import (
	"context"
	"math"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestPageRankRanksTheSinkHighest(t *testing.T) {
	got, err := PageRank(context.Background(), testGraph(), DefaultPageRankOptions())
	if err != nil {
		t.Fatalf("PageRank: %v", err)
	}
	if got[0].Key != "c" {
		t.Fatalf("highest-ranked = %q, want \"c\" (two inbound edges)", got[0].Key)
	}
	var sum float64
	for _, s := range got {
		sum += s.Value
	}
	if math.Abs(sum-1.0) > 1e-6 {
		t.Fatalf("scores sum to %v, want 1.0", sum)
	}
}

func TestPageRankHandlesDanglingNodes(t *testing.T) {
	// "c" has no outbound edges. Its rank must be redistributed, not lost.
	got, err := PageRank(context.Background(), testGraph(), DefaultPageRankOptions())
	if err != nil {
		t.Fatalf("PageRank: %v", err)
	}
	var sum float64
	for _, s := range got {
		sum += s.Value
	}
	if math.Abs(sum-1.0) > 1e-6 {
		t.Fatalf("mass leaked from dangling node: sum = %v", sum)
	}
}

func TestPageRankIsBitReproducible(t *testing.T) {
	g := NewGraph(
		[]Node{{Key: "a"}, {Key: "b"}, {Key: "c"}, {Key: "d"}, {Key: "e"}},
		[]Edge{
			{From: "a", To: "b", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
			{From: "b", To: "c", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
			{From: "c", To: "d", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
			{From: "d", To: "b", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
			{From: "e", To: "a", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
		},
	)
	first, err := PageRank(context.Background(), g, DefaultPageRankOptions())
	if err != nil {
		t.Fatalf("PageRank: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := PageRank(context.Background(), g, DefaultPageRankOptions())
		if err != nil {
			t.Fatalf("PageRank: %v", err)
		}
		for j := range first {
			// Bit-identical, not approximately equal. Any drift means an
			// unordered accumulation crept in.
			if again[j].Key != first[j].Key || again[j].Value != first[j].Value {
				t.Fatalf("run %d differs at %d: %+v vs %+v", i, j, again[j], first[j])
			}
		}
	}
}
