package web

import (
	"context"
	"testing"
)

// TestDetectClustersDropsNonPositiveCountEdges is a mutation-testing
// regression: deleting the `e.Count <= 0` filter in detectClusters passed the
// committed suite undetected. The filter exists because metrics.NewGraph
// rewrites a non-positive weight to 1 instead of dropping the edge, so
// without it a zero-count AggregatedEdge (one the explorer draws nothing for)
// becomes a real weight-1 relationship. Here node "x" has no relationship to
// the a-clique except a zero-count edge; it must stay in its own cluster.
func TestDetectClustersDropsNonPositiveCountEdges(t *testing.T) {
	keys := []string{"a1", "a2", "a3", "x"}
	edges := []AggregatedEdge{
		{From: "a1", To: "a2", Count: 5},
		{From: "a2", To: "a3", Count: 5},
		{From: "a3", To: "a1", Count: 5},
		{From: "a1", To: "x", Count: 0}, // no real relationship recorded
	}

	got, err := detectClusters(context.Background(), keys, edges)
	if err != nil {
		t.Fatalf("detectClusters: %v", err)
	}

	if got["x"] == got["a1"] {
		t.Fatalf("x merged into a1's cluster via a zero-count edge that should have been dropped: %v", got)
	}
}
