package cluster

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func twoCliques() metrics.Graph {
	e := func(f, t string) metrics.Edge {
		return metrics.Edge{From: f, To: t, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh, Weight: 1}
	}
	return metrics.NewGraph(
		[]metrics.Node{{Key: "a1"}, {Key: "a2"}, {Key: "a3"}, {Key: "b1"}, {Key: "b2"}, {Key: "b3"}},
		[]metrics.Edge{
			e("a1", "a2"), e("a2", "a3"), e("a3", "a1"),
			e("b1", "b2"), e("b2", "b3"), e("b3", "b1"),
			e("a1", "b1"), // one thin bridge
		},
	)
}

func TestDetectSeparatesTwoCliques(t *testing.T) {
	got, err := Detect(context.Background(), twoCliques(), DefaultOptions())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	byKey := map[string]string{}
	for _, a := range got {
		byKey[a.Key] = a.ClusterID
	}
	if byKey["a1"] != byKey["a2"] || byKey["a2"] != byKey["a3"] {
		t.Errorf("a-clique split across clusters: %v", byKey)
	}
	if byKey["b1"] != byKey["b2"] || byKey["b2"] != byKey["b3"] {
		t.Errorf("b-clique split across clusters: %v", byKey)
	}
}

func TestDetectIsDeterministic(t *testing.T) {
	g := twoCliques()
	first, err := Detect(context.Background(), g, DefaultOptions())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := Detect(context.Background(), g, DefaultOptions())
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("run %d differs at %d: %+v vs %+v", i, j, again[j], first[j])
			}
		}
	}
}

// asymmetricBridge is deliberately NOT symmetric the way twoCliques is: node
// "m" hangs off both cliques with EQUAL edge weight, so its cluster is decided
// purely by the documented tie-break ("ties break on the smaller label
// string"), not by weight. twoCliques can't exercise this — its bridge only
// ever pulls one way.
func asymmetricBridge() metrics.Graph {
	e := func(f, t string, w int) metrics.Edge {
		return metrics.Edge{From: f, To: t, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh, Weight: w}
	}
	return metrics.NewGraph(
		[]metrics.Node{{Key: "a1"}, {Key: "a2"}, {Key: "a3"}, {Key: "b1"}, {Key: "b2"}, {Key: "b3"}, {Key: "m"}},
		[]metrics.Edge{
			e("a1", "a2", 5), e("a2", "a3", 5), e("a3", "a1", 5),
			e("b1", "b2", 5), e("b2", "b3", 5), e("b3", "b1", 5),
			e("m", "a1", 3), e("m", "b1", 3), // equal weight to both cliques: a genuine tie
		},
	)
}

// TestDetectBreaksTiesOnSmallerLabel is a mutation-testing regression: with
// twoCliques() alone, inverting the tie-break in Detect from "c < best" to
// "c > best" passed the suite undetected, because that fixture's bridge is
// weighted so only one side can ever win — the tie-break condition is never
// actually exercised. Here "m" has equal-weight edges into both cliques, so
// the outcome depends entirely on which candidate label is lexically smaller.
// Node "m" must land in a1's cluster, not b1's.
func TestDetectBreaksTiesOnSmallerLabel(t *testing.T) {
	got, err := Detect(context.Background(), asymmetricBridge(), DefaultOptions())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	byKey := map[string]string{}
	for _, a := range got {
		byKey[a.Key] = a.ClusterID
	}
	if byKey["m"] != byKey["a1"] {
		t.Fatalf("tied node m should join the smaller-label cluster (a1's), got cluster %q (a1 is in %q, b1 is in %q)",
			byKey["m"], byKey["a1"], byKey["b1"])
	}
}

// weightedChain is a chain where each successive edge is strictly heavier than
// the last, moving away from c1. Because Detect visits nodes in ascending key
// order within a pass, the heavy end's label can only propagate backward one
// hop per pass — full convergence into a single cluster needs five passes for
// this six-node chain.
func weightedChain() metrics.Graph {
	e := func(f, t string, w int) metrics.Edge {
		return metrics.Edge{From: f, To: t, Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh, Weight: w}
	}
	return metrics.NewGraph(
		[]metrics.Node{{Key: "c1"}, {Key: "c2"}, {Key: "c3"}, {Key: "c4"}, {Key: "c5"}, {Key: "c6"}},
		[]metrics.Edge{
			e("c1", "c2", 1), e("c2", "c3", 2), e("c3", "c4", 3), e("c4", "c5", 4), e("c5", "c6", 5),
		},
	)
}

// TestDetectConvergesOverMultiplePasses is a mutation-testing regression: the
// committed suite passed unchanged when DefaultOptions().MaxPasses was cut
// from 20 to 1, because twoCliques() happens to fully converge in a single
// pass. weightedChain() needs five passes for the heavy end's label to reach
// c1; asserting the fully-converged partition (not the pass count) fails if
// MaxPasses is too small to let it finish.
func TestDetectConvergesOverMultiplePasses(t *testing.T) {
	got, err := Detect(context.Background(), weightedChain(), DefaultOptions())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	byKey := map[string]string{}
	for _, a := range got {
		byKey[a.Key] = a.ClusterID
	}
	for _, k := range []string{"c1", "c2", "c3", "c4", "c5", "c6"} {
		if byKey[k] != byKey["c1"] {
			t.Fatalf("chain did not fully converge into one cluster (need >1 pass): %v", byKey)
		}
	}
}

func TestDetectClusterIDIsSmallestMember(t *testing.T) {
	got, err := Detect(context.Background(), twoCliques(), DefaultOptions())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	members := map[string][]string{}
	for _, a := range got {
		members[a.ClusterID] = append(members[a.ClusterID], a.Key)
	}
	for id, ms := range members {
		smallest := ms[0]
		for _, m := range ms {
			if m < smallest {
				smallest = m
			}
		}
		if id != smallest {
			t.Errorf("cluster id %q is not its smallest member %q", id, smallest)
		}
	}
}
