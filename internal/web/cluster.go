package web

import (
	"context"
	"fmt"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/cluster"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
)

// Community detection for the graph explorer, computed server-side.
//
// The algorithm and the reasoning behind it now live in
// internal/analyze/cluster: partitioning a graph is analysis, and this package
// is the presentation adapter. What stays here is what the explorer draws — the
// cluster's label, its member list, its internal/external edge counts, and the
// caveat that says a community is a description of the code's current shape and
// not a declared architecture.

// detectClusters translates the drawn nodes and aggregated edges of one level
// into the analysis package's graph, and reads the partition back as the
// key -> cluster id map the summary below is written against.
//
// Non-positive counts are dropped rather than passed through: metrics.NewGraph
// rewrites a weight of zero or less to 1, which would let an edge the explorer
// draws nothing for pull two communities together.
func detectClusters(ctx context.Context, keys []string, edges []AggregatedEdge) (map[string]string, error) {
	nodes := make([]metrics.Node, 0, len(keys))
	for _, k := range keys {
		nodes = append(nodes, metrics.Node{Key: k})
	}
	me := make([]metrics.Edge, 0, len(edges))
	for _, e := range edges {
		if e.Count <= 0 {
			continue
		}
		me = append(me, metrics.Edge{From: e.From, To: e.To, Weight: e.Count})
	}
	assignments, err := cluster.Detect(ctx, metrics.NewGraph(nodes, me), cluster.DefaultOptions())
	if err != nil {
		return nil, fmt.Errorf("clustering %d graph nodes: %w", len(keys), err)
	}
	out := make(map[string]string, len(assignments))
	for _, a := range assignments {
		out[a.Key] = a.ClusterID
	}
	return out, nil
}

// Cluster is one detected community, as the explorer draws it.
type Cluster struct {
	// ID is the smallest member key — a real node, so it is stable and
	// look-up-able.
	ID string `json:"id"`

	// Label is what to print on the collapsed node.
	Label string `json:"label"`

	// Members is every node in this community, in ascending key order.
	Members []string `json:"members"`
	Size    int      `json:"size"`

	// Internal is relationships that stay inside this community; External is
	// relationships that leave it. Their ratio is the only evidence a reader
	// has that the partition means anything, so it is reported rather than
	// asserted.
	Internal int `json:"internal_edges"`
	External int `json:"external_edges"`

	// Method names how the partition was produced, and Caveat says what it does
	// and does not claim. Served as data so the client cannot present a drawing
	// aid as a declared architecture.
	Method string `json:"method"`
	Caveat string `json:"caveat"`
}

const clusterMethod = "deterministic asynchronous label propagation over the drawn graph, " +
	"visited in ascending node order with ties broken on the smaller label"

const clusterCaveat = "A community is a description of how this code currently talks to itself, " +
	"not a declared architecture. It is a drawing aid: it decides what is collapsed by default, " +
	"nothing more. Declared components come from .cortex.yml and are the only grouping this " +
	"product presents as intent."

// summariseClusters turns an assignment into the ordered cluster list, counting
// each community's internal and external edges from the same edge list the
// canvas draws.
//
// Ordering is size descending then id ascending — total, so the list is
// byte-identical across runs.
func summariseClusters(assignment map[string]string, edges []AggregatedEdge) []Cluster {
	if len(assignment) == 0 {
		return []Cluster{}
	}
	members := map[string][]string{}
	for k, c := range assignment {
		members[c] = append(members[c], k)
	}
	internal := map[string]int{}
	external := map[string]int{}
	for _, e := range edges {
		a, aok := assignment[e.From]
		b, bok := assignment[e.To]
		if !aok || !bok {
			continue
		}
		if a == b {
			internal[a]++
			continue
		}
		external[a]++
		external[b]++
	}
	out := make([]Cluster, 0, len(members))
	for id, ms := range members {
		sort.Strings(ms)
		out = append(out, Cluster{
			ID: id, Label: clusterLabel(id, len(ms)), Members: ms, Size: len(ms),
			Internal: internal[id], External: external[id],
			Method: clusterMethod, Caveat: clusterCaveat,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Size != out[j].Size {
			return out[i].Size > out[j].Size
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func clusterLabel(id string, size int) string {
	if size <= 1 {
		return id
	}
	return id + " +" + itoa(size-1)
}

// itoa avoids pulling strconv in for one call site in a hot-ish path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
