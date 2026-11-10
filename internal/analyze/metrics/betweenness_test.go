package metrics

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func bridgeGraph() Graph {
	// Two triangles joined only through "bridge".
	e := func(f, t string) Edge {
		return Edge{From: f, To: t, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh}
	}
	return NewGraph(
		[]Node{{Key: "a1"}, {Key: "a2"}, {Key: "bridge"}, {Key: "b1"}, {Key: "b2"}},
		[]Edge{
			e("a1", "a2"), e("a2", "a1"),
			e("a2", "bridge"), e("bridge", "a2"),
			e("bridge", "b1"), e("b1", "bridge"),
			e("b1", "b2"), e("b2", "b1"),
		},
	)
}

func TestBetweennessFindsTheBridge(t *testing.T) {
	got, err := Betweenness(context.Background(), bridgeGraph(), DefaultBetweennessOptions())
	if err != nil {
		t.Fatalf("Betweenness: %v", err)
	}
	if got[0].Key != "bridge" {
		t.Fatalf("highest betweenness = %q, want \"bridge\"", got[0].Key)
	}
}

func TestBetweennessSamplingIsSeededAndReproducible(t *testing.T) {
	g := bridgeGraph()
	opts := BetweennessOptions{SampleSize: 2, Seed: 42}
	first, err := Betweenness(context.Background(), g, opts)
	if err != nil {
		t.Fatalf("Betweenness: %v", err)
	}
	for i := 0; i < 25; i++ {
		again, err := Betweenness(context.Background(), g, opts)
		if err != nil {
			t.Fatalf("Betweenness: %v", err)
		}
		for j := range first {
			if again[j].Key != first[j].Key || again[j].Value != first[j].Value {
				t.Fatalf("run %d differs at %d: %+v vs %+v", i, j, again[j], first[j])
			}
		}
	}
}
