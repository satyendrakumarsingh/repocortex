package graph

import (
	"context"
	"reflect"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestWithCSRMatchesSQLTraverse pins that routing Graph.Traverse through the
// CSR snapshot answers exactly as the SQL path does, for both directions and a
// confidence floor, so enabling WithCSR in serve and ui changes latency only.
func TestWithCSRMatchesSQLTraverse(t *testing.T) {
	sqlGraph := build(t, []tedge{
		e(1, 2),
		e(1, 3).withConf(model.ConfidenceMedium),
		e(2, 4),
		e(3, 4).withConf(model.ConfidenceLow),
		e(4, 5),
		e(5, 1),
	})
	csrGraph := New(sqlGraph.db, WithCSR())

	queries := map[string]Query{
		"outgoing":      {Roots: []int64{1}, Direction: Outgoing, MaxDepth: 4},
		"incoming":      {Roots: []int64{5}, Direction: Incoming, MaxDepth: 4},
		"min high":      {Roots: []int64{1}, Direction: Outgoing, MinConfidence: model.ConfidenceHigh, MaxDepth: 4},
		"depth limited": {Roots: []int64{1}, Direction: Outgoing, MaxDepth: 1},
		"node cap":      {Roots: []int64{1}, Direction: Outgoing, MaxDepth: 4, MaxNodes: 3},
		"seeding cap":   {Roots: []int64{1, 2, 3}, Direction: Outgoing, MaxDepth: 4, MaxNodes: 2},
	}
	for name, q := range queries {
		t.Run(name, func(t *testing.T) {
			want, err := sqlGraph.Traverse(context.Background(), q)
			if err != nil {
				t.Fatalf("sql traverse: %v", err)
			}
			got, err := csrGraph.Traverse(context.Background(), q)
			if err != nil {
				t.Fatalf("csr traverse: %v", err)
			}
			if !reflect.DeepEqual(ids(got.Nodes), ids(want.Nodes)) {
				t.Fatalf("node ids = %v, want %v", ids(got.Nodes), ids(want.Nodes))
			}
			for _, w := range want.Nodes {
				g := nodeByID(t, got, w.SymbolID)
				if g.Depth != w.Depth || g.Confidence != w.Confidence {
					t.Errorf("node %d = depth %d conf %v, want depth %d conf %v",
						w.SymbolID, g.Depth, g.Confidence, w.Depth, w.Confidence)
				}
			}
			if got.Truncated != want.Truncated {
				t.Errorf("Truncated = %v, want %v", got.Truncated, want.Truncated)
			}
			if got.TruncatedReason != want.TruncatedReason {
				t.Errorf("TruncatedReason = %q, want %q", got.TruncatedReason, want.TruncatedReason)
			}
		})
	}
}
