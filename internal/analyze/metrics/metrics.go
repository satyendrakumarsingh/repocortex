// Package metrics computes structural properties of a code graph.
//
// Everything here is pure and deterministic: the same nodes and edges produce
// byte-identical output on every machine. That is not a nicety. These numbers
// feed the registry digest and a reader's diff between two index runs, so a
// value that wobbles because a map iterated differently would read as real
// architectural churn.
//
// Loading edges out of SQLite is the registry's job, not this package's.
//
// # Measured performance
//
// From bench_test.go on a 100k-symbol / 500k-edge synthetic graph (Apple M4
// Pro, arm64, -benchtime 5x), wall time per call, re-measured when the cycle
// budget landed:
//
//	PageRank (100 iterations)                 ->    86.7ms
//	Components (Tarjan SCC)                   ->   149.2ms
//	Cycles (DefaultCycleBudget)               ->   865.4ms
//	Betweenness, sampled (SampleSize: 70)     -> 1,034.7ms
//
// Cycles is the entry that was missing here until it was measured: unbudgeted,
// the same fixture cost 10.1s, and on a denser 20k-node graph 26.4s. Its cost
// follows density rather than node count, so no caller can bound it by counting
// nodes; DefaultCycleBudget bounds the WORK instead, deterministically. See
// cycles.go for the numbers behind those bounds.
//
// Betweenness is O(V*E) per source, so its cost scales directly with sample
// size. At the documented default of SampleSize: 100 this fixture measured over
// the 2s budget this package holds itself to, so the default in
// DefaultBetweennessOptions was lowered to 70. PageRank and Components need no
// such trade: both clear the budget by more than 10x at full 100k scale.
//
// The fixture (bench_test.go's syntheticGraph) has a near-regular degree
// distribution — measured in-degree min 4 / max 5 / mean ~5.0 across all
// 100k nodes — not the hub-and-tail shape a real call graph has. Betweenness
// is sensitive to exactly this: shortest paths concentrate through hubs in a
// skewed graph in a way they cannot in a near-uniform one, so these numbers
// are a throughput measurement at a fixed edge count, not a guarantee of the
// same wall time on a hub-heavy production graph.
package metrics

import (
	"sort"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Node is one vertex, identified by a stable key (a symbol FQN or file path).
type Node struct {
	Key  string
	Kind model.EntityKind
}

// Edge is one directed relationship between node keys.
//
// Direction is a field, never a convention. An undirected view is derived on
// demand by the algorithms that need one; it is never the stored form.
type Edge struct {
	From       string
	To         string
	Kind       model.EdgeKind
	Confidence model.Confidence
	Weight     int
}

// Graph is an immutable, validated adjacency view over nodes and edges.
type Graph struct {
	keys  []string
	nodes map[string]Node
	edges []Edge
	out   map[string][]int
	in    map[string][]int
}

// NewGraph validates and indexes nodes and edges.
//
// Edges naming an unknown node are dropped rather than creating one: a phantom
// vertex invented at analysis time would be reported as a real code element.
// Self-loops are dropped because every metric here treats them as noise.
func NewGraph(nodes []Node, edges []Edge) Graph {
	keys := make([]string, 0, len(nodes))
	byKey := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		if _, dup := byKey[n.Key]; dup {
			continue
		}
		byKey[n.Key] = n
		keys = append(keys, n.Key)
	}
	sort.Strings(keys)

	kept := make([]Edge, 0, len(edges))
	for _, e := range edges {
		if e.From == e.To {
			continue
		}
		if _, ok := byKey[e.From]; !ok {
			continue
		}
		if _, ok := byKey[e.To]; !ok {
			continue
		}
		if e.Weight <= 0 {
			e.Weight = 1
		}
		kept = append(kept, e)
	}
	// Total order over edges so every downstream accumulation is sequenced.
	sort.Slice(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Kind < b.Kind
	})

	out := make(map[string][]int, len(keys))
	in := make(map[string][]int, len(keys))
	for i, e := range kept {
		out[e.From] = append(out[e.From], i)
		in[e.To] = append(in[e.To], i)
	}
	return Graph{keys: keys, nodes: byKey, edges: kept, out: out, in: in}
}

// Order is the number of nodes.
func (g Graph) Order() int { return len(g.keys) }

// Keys returns node keys in ascending order. The slice is a copy.
func (g Graph) Keys() []string {
	out := make([]string, len(g.keys))
	copy(out, g.keys)
	return out
}

// Edges returns the retained edges in canonical order. The slice is a copy.
func (g Graph) Edges() []Edge {
	out := make([]Edge, len(g.edges))
	copy(out, g.edges)
	return out
}

// Node looks up a node by key.
func (g Graph) Node(key string) (Node, bool) {
	n, ok := g.nodes[key]
	return n, ok
}

// OutEdges returns edges leaving key, in canonical order.
func (g Graph) OutEdges(key string) []Edge { return g.pick(g.out[key]) }

// InEdges returns edges entering key, in canonical order.
func (g Graph) InEdges(key string) []Edge { return g.pick(g.in[key]) }

func (g Graph) pick(idx []int) []Edge {
	out := make([]Edge, 0, len(idx))
	for _, i := range idx {
		out = append(out, g.edges[i])
	}
	return out
}
