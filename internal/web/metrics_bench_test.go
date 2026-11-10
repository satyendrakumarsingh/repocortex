package web

import (
	"context"
	"fmt"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func syntheticMetricsGraph(n int) (metrics.Graph, map[string]metricsNode) {
	nodes := make([]metrics.Node, 0, n)
	lookup := make(map[string]metricsNode, n)
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("pkg%03d/file%04d.go:Sym%06d", i%200, i%1000, i)
		nodes = append(nodes, metrics.Node{Key: k, Kind: model.KindFunction})
		lookup[k] = metricsNode{Key: k, Label: fmt.Sprintf("Sym%06d", i), Path: fmt.Sprintf("pkg%03d/file%04d.go", i%200, i%1000), Kind: model.KindFunction, Exported: i%3 == 0}
	}
	edges := make([]metrics.Edge, 0, n*5)
	for i := 0; i < n; i++ {
		for j := 1; j <= 5; j++ {
			t := (i*7 + j*13) % n
			if t == i {
				continue
			}
			edges = append(edges, metrics.Edge{
				From: nodes[i].Key, To: nodes[t].Key,
				Kind: model.EdgeCalls, Confidence: model.ConfidenceHigh, Weight: 1,
			})
		}
	}
	return metrics.NewGraph(nodes, edges), lookup
}

func BenchmarkBuildSections12k(b *testing.B) {
	g, lookup := syntheticMetricsGraph(12_000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := buildSections(ctx, LevelSymbol, g, lookup, MetricsFilters{}, metrics.DefaultCycleBudget()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuildSections100k(b *testing.B) {
	g, lookup := syntheticMetricsGraph(100_000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := buildSections(ctx, LevelSymbol, g, lookup, MetricsFilters{}, metrics.DefaultCycleBudget()); err != nil {
			b.Fatal(err)
		}
	}
}
