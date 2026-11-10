package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestCSRGraphTraversalParity(t *testing.T) {
	// Construct a synthetic DAG with known branchings and weights:
	// 1 -> 2 (calls, exact)
	// 1 -> 3 (calls, medium)
	// 2 -> 4 (calls, exact)
	// 3 -> 4 (calls, guess)
	// 4 -> 5 (calls, exact)
	records := []EdgeRecord{
		{SrcID: 1, DstID: 2, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact},
		{SrcID: 1, DstID: 3, Kind: model.EdgeCalls, Confidence: model.ConfidenceMedium},
		{SrcID: 2, DstID: 4, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact},
		{SrcID: 3, DstID: 4, Kind: model.EdgeCalls, Confidence: model.ConfidenceLow},
		{SrcID: 4, DstID: 5, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact},
	}

	csr, err := NewCSRGraph(1, records)
	if err != nil {
		t.Fatalf("NewCSRGraph: %v", err)
	}

	if csr.NodeCount() != 5 {
		t.Fatalf("expected 5 nodes, got %d", csr.NodeCount())
	}
	if csr.EdgeCount() != 5 {
		t.Fatalf("expected 5 edges, got %d", csr.EdgeCount())
	}

	// 1. Traverse outgoing from root 1 with depth 2
	res, err := csr.Traverse(context.Background(), Query{
		Roots:     []int64{1},
		Direction: Outgoing,
		MaxDepth:  2,
		MaxNodes:  100,
	})
	if err != nil {
		t.Fatalf("unexpected traverse error: %v", err)
	}

	// Root (1) at depth 0, 2 & 3 at depth 1, 4 at depth 2
	if len(res.Nodes) != 4 {
		t.Fatalf("expected 4 reachable nodes within depth 2, got %d: %+v", len(res.Nodes), res.Nodes)
	}

	nodeMap := make(map[int64]Node)
	for _, n := range res.Nodes {
		nodeMap[n.SymbolID] = n
	}

	// Node 4 reached via 2 (exact->exact) should beat route via 3 (medium->guess)
	n4, ok := nodeMap[4]
	if !ok {
		t.Fatalf("node 4 missing from result")
	}
	if n4.Confidence != model.ConfidenceExact {
		t.Errorf("node 4 confidence = %v, want %v (stronger path should win)", n4.Confidence, model.ConfidenceExact)
	}

	// Depth limit 2 was reached, node 5 at depth 3 was not expanded
	if !res.Truncated {
		t.Errorf("expected truncated to be true at depth limit")
	}
}

func TestCSRGraphIncomingTraversal(t *testing.T) {
	records := []EdgeRecord{
		{SrcID: 10, DstID: 20, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact},
		{SrcID: 15, DstID: 20, Kind: model.EdgeCalls, Confidence: model.ConfidenceMedium},
		{SrcID: 5, DstID: 10, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact},
	}

	csr, err := NewCSRGraph(1, records)
	if err != nil {
		t.Fatalf("NewCSRGraph: %v", err)
	}

	// Traverse incoming callers of 20
	res, err := csr.Traverse(context.Background(), Query{
		Roots:     []int64{20},
		Direction: Incoming,
		MaxDepth:  2,
		MaxNodes:  10,
	})
	if err != nil {
		t.Fatalf("unexpected traverse error: %v", err)
	}

	if len(res.Nodes) != 4 {
		t.Fatalf("expected 4 nodes in caller tree (20, 10, 15, 5), got %d: %+v", len(res.Nodes), res.Nodes)
	}
}

func TestCSRGraphHubPruning(t *testing.T) {
	// Root 1 -> Hub 2 (with 50 outgoing edges) -> Target 100
	records := []EdgeRecord{
		{SrcID: 1, DstID: 2, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact},
	}
	for i := 10; i < 60; i++ {
		records = append(records, EdgeRecord{
			SrcID:      2,
			DstID:      int64(i),
			Kind:       model.EdgeCalls,
			Confidence: model.ConfidenceExact,
		})
	}

	csr, err := NewCSRGraph(1, records)
	if err != nil {
		t.Fatalf("NewCSRGraph: %v", err)
	}

	// Traverse with HubDegreeLimit = 10. Node 2 has out-degree 50 > 10, so it must be pruned.
	res, err := csr.Traverse(context.Background(), Query{
		Roots:          []int64{1},
		Direction:      Outgoing,
		HubDegreeLimit: 10,
		MaxDepth:       3,
		MaxNodes:       100,
	})
	if err != nil {
		t.Fatalf("traverse error: %v", err)
	}

	if len(res.PrunedHubs) != 1 {
		t.Fatalf("expected 1 pruned hub, got %d: %+v", len(res.PrunedHubs), res.PrunedHubs)
	}
	if res.PrunedHubs[0].SymbolID != 2 {
		t.Errorf("pruned hub symbol_id = %d, want 2", res.PrunedHubs[0].SymbolID)
	}

	// Result should contain only 1 (root) and 2 (pruned node itself, reported but not expanded)
	if len(res.Nodes) != 2 {
		t.Fatalf("expected 2 nodes reported when hub is pruned, got %d", len(res.Nodes))
	}
}

func BenchmarkCSRGraphTraverse(b *testing.B) {
	// Construct a synthetic 5,000 node graph with 25,000 edges
	var records []EdgeRecord
	for i := 1; i <= 5000; i++ {
		for j := 1; j <= 5; j++ {
			target := (i*j)%5000 + 1
			records = append(records, EdgeRecord{
				SrcID:      int64(i),
				DstID:      int64(target),
				Kind:       model.EdgeCalls,
				Confidence: model.ConfidenceExact,
			})
		}
	}

	csr, err := NewCSRGraph(1, records)
	if err != nil {
		b.Fatalf("NewCSRGraph: %v", err)
	}
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := csr.Traverse(ctx, Query{
			Roots:     []int64{1, 10, 50},
			Direction: Outgoing,
			MaxDepth:  3,
			MaxNodes:  500,
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// TestCSRGraphNodeCapTruncates pins the contract for hitting the node budget
// mid-expansion: the traversal returns the nodes found so far, flagged as
// truncated with a reason, and NO error. (This is what the nilerr linter
// flagged; it is deliberate and mirrors the SQL traversal.)
func TestCSRGraphNodeCapTruncates(t *testing.T) {
	records := []EdgeRecord{
		{SrcID: 1, DstID: 2, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact},
		{SrcID: 1, DstID: 3, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact},
		{SrcID: 1, DstID: 4, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact},
	}
	csr, err := NewCSRGraph(1, records)
	if err != nil {
		t.Fatalf("NewCSRGraph: %v", err)
	}

	res, err := csr.Traverse(context.Background(), Query{
		Roots: []int64{1}, Direction: Outgoing, MaxDepth: 3, MaxNodes: 2,
	})
	if err != nil {
		t.Fatalf("hitting the node cap must not be an error, got %v", err)
	}
	if !res.Truncated || res.TruncatedReason == "" {
		t.Fatalf("expected truncated result with a reason, got truncated=%v reason=%q", res.Truncated, res.TruncatedReason)
	}
	if len(res.Nodes) != 2 {
		t.Fatalf("expected 2 nodes at the cap, got %d", len(res.Nodes))
	}
}

// TestCSRGraphCancelledContext pins that a cancelled context is a real error,
// not a silent truncation.
func TestCSRGraphCancelledContext(t *testing.T) {
	records := []EdgeRecord{
		{SrcID: 1, DstID: 2, Kind: model.EdgeCalls, Confidence: model.ConfidenceExact},
	}
	csr, err := NewCSRGraph(1, records)
	if err != nil {
		t.Fatalf("NewCSRGraph: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = csr.Traverse(ctx, Query{Roots: []int64{1}, Direction: Outgoing, MaxDepth: 3, MaxNodes: 10})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestCheckCSRBounds(t *testing.T) {
	tests := []struct {
		name         string
		nodes, edges int
		wantErr      bool
	}{
		{"small", 10, 10, false},
		{"at limit", maxCSRIndex, maxCSRIndex, false},
		{"too many edges", 1, maxCSRIndex + 1, true},
		{"too many nodes", maxCSRIndex + 1, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkCSRBounds(tt.nodes, tt.edges); (err != nil) != tt.wantErr {
				t.Fatalf("checkCSRBounds(%d,%d) err=%v, wantErr=%v", tt.nodes, tt.edges, err, tt.wantErr)
			}
		})
	}
}
