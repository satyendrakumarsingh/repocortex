package metrics

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestNewGraphSortsKeysAndDropsDanglingEdges(t *testing.T) {
	g := NewGraph(
		[]Node{{Key: "c"}, {Key: "a"}, {Key: "b"}},
		[]Edge{
			{From: "a", To: "b", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
			{From: "a", To: "zzz", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
			{From: "b", To: "b", Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh},
		},
	)

	if got, want := g.Order(), 3; got != want {
		t.Fatalf("Order() = %d, want %d", got, want)
	}
	keys := g.Keys()
	if want := []string{"a", "b", "c"}; len(keys) != 3 || keys[0] != want[0] || keys[1] != want[1] || keys[2] != want[2] {
		t.Fatalf("Keys() = %v, want %v", keys, want)
	}
	// The dangling edge to "zzz" and the self-loop are both dropped.
	if got, want := len(g.Edges()), 1; got != want {
		t.Fatalf("len(Edges()) = %d, want %d", got, want)
	}
}
