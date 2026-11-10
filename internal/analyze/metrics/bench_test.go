package metrics

import (
	"context"
	"fmt"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// syntheticGraph builds an n-node graph with roughly 5 edges per node. The
// stride formula below (i*7 + j*13) % n spreads each node's out-edges almost
// uniformly across the key space, so the resulting graph has a near-regular
// degree distribution, not a hub-and-tail one: measured over all 100k nodes,
// in-degree is min 4 / max 5 / mean ~5.0 (see TestDegreeDistribution-style
// measurement in the fix-round report). A real call graph is not shaped like
// this — it has a few widely-called hubs and a long tail of rarely-called
// leaves — so the benchmark numbers below characterize this package's raw
// throughput on a fixed edge count, not its behavior on skewed real-world
// shortest-path traffic. See metrics.go's "Measured performance" section for
// the caveat this implies for Betweenness specifically.
func syntheticGraph(n int) Graph {
	nodes := make([]Node, 0, n)
	for i := 0; i < n; i++ {
		nodes = append(nodes, Node{Key: fmt.Sprintf("pkg%03d/file%04d.go:Sym%06d", i%200, i%1000, i)})
	}
	edges := make([]Edge, 0, n*5)
	for i := 0; i < n; i++ {
		for j := 1; j <= 5; j++ {
			t := (i*7 + j*13) % n
			if t == i {
				continue
			}
			edges = append(edges, Edge{
				From:       nodes[i].Key,
				To:         nodes[t].Key,
				Kind:       model.EdgeCalls,
				Confidence: model.ConfidenceHigh,
			})
		}
	}
	return NewGraph(nodes, edges)
}

func BenchmarkPageRank100k(b *testing.B) {
	g := syntheticGraph(100_000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := PageRank(ctx, g, DefaultPageRankOptions()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBetweennessSampled100k(b *testing.B) {
	g := syntheticGraph(100_000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Betweenness(ctx, g, DefaultBetweennessOptions()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkComponents100k(b *testing.B) {
	g := syntheticGraph(100_000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Components(ctx, g); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCycles100k is the benchmark whose absence let an unbounded
// enumeration ship: at the time this package published PageRank, Components and
// Betweenness figures, Cycles cost 10.1s on this same fixture and nothing here
// measured it.
func BenchmarkCycles100k(b *testing.B) {
	g := syntheticGraph(100_000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Cycles(ctx, g, DefaultCycleBudget()); err != nil {
			b.Fatal(err)
		}
	}
}
