// Package cluster partitions a code graph into communities.
//
// # Why the server does this
//
// The graph explorer's job on a 14,000-file repository is to not be a hairball,
// and the only structural tool that works is grouping: draw communities, let
// the reader expand one, aggregate everything between them. A client that
// receives the whole graph and then clusters it has already paid for the whole
// graph — and would have to re-derive, in JavaScript, a partition the server
// can compute once over data it already holds in memory.
//
// # Why label propagation, and why this variant
//
// Louvain is the usual answer and it is the wrong one here for one reason: it
// is not deterministic without pinning a random seed AND an iteration order,
// and this codebase has already shipped three determinism bugs from unordered
// reads feeding first-wins rules. A partition that changed between two runs
// would change every cluster label, every aggregated cluster edge and the
// registry digest downstream of them.
//
// So: asynchronous label propagation over a TOTAL order.
//
//   - Nodes are visited in ascending key order, every pass.
//   - A node adopts the neighbouring label with the greatest summed edge
//     weight; ties break on the smaller label string.
//   - Iteration stops when a pass changes nothing, or at Options.MaxPasses.
//   - The final cluster id is the smallest member key, so the id is stable
//     against renumbering and is itself a real node the reader can look up.
//
// Every one of those rules exists to remove a choice the runtime would
// otherwise make for us. There is no map iteration anywhere in the loop, no
// randomness, and no dependence on worker count: the same node and edge lists
// produce byte-identical output on every machine.
//
// # What it is not
//
// It is not a claim about architecture. A community here is "these nodes talk
// to each other more than they talk outwards", which is a description of the
// code's current shape — the same thing the web layer's containerFor refuses to
// present as a declared component. It is a drawing aid, labelled as one by the
// caller that renders it, and the response says which method produced it.
//
// # Why it lives here and not in the web adapter
//
// This is analysis, not presentation: it reads a graph and returns a partition.
// Naming the clusters, counting their internal and external edges and deciding
// what a reader sees collapsed are the adapter's business, and stay there.
package cluster

import (
	"context"
	"fmt"
	"sort"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
)

// Assignment binds one node to its cluster.
type Assignment struct {
	Key       string
	ClusterID string
}

// Options bounds the propagation.
type Options struct {
	// MaxPasses bounds the loop. Label propagation on a sparse graph settles in
	// a handful of passes; the bound guarantees termination on a pathological
	// one rather than oscillating between two equally-weighted labels.
	MaxPasses int
}

// DefaultOptions returns the bound the web explorer has been shipping.
func DefaultOptions() Options { return Options{MaxPasses: 20} }

// Detect partitions the graph by asynchronous label propagation over a total
// order, returning key-ascending assignments.
//
// Edges are treated as undirected for clustering: "these things talk to each
// other" is a symmetric claim, and using direction here would split a caller
// from its callee for no reason a reader would accept.
//
// Callers that must not let a zero or negative weight count as a relationship
// have to drop those edges before building the graph: metrics.NewGraph rewrites
// a non-positive weight to 1 rather than discarding the edge.
func Detect(ctx context.Context, g metrics.Graph, opts Options) ([]Assignment, error) {
	if opts.MaxPasses <= 0 {
		return nil, fmt.Errorf("cluster max passes must be positive, got %d", opts.MaxPasses)
	}
	keys := g.Keys()
	if len(keys) == 0 {
		return nil, nil
	}

	// Adjacency, built with each neighbour list kept in sorted order so the
	// weighted vote below is summed in the same sequence on every run.
	type neighbour struct {
		Key    string
		Weight int
	}
	adj := make(map[string][]neighbour, len(keys))
	weight := map[[2]string]int{}
	for _, e := range g.Edges() {
		a, b := e.From, e.To
		if a > b {
			a, b = b, a
		}
		weight[[2]string{a, b}] += e.Weight
	}
	pairs := make([][2]string, 0, len(weight))
	for p := range weight {
		pairs = append(pairs, p)
	}
	// Ranged over a map only to collect; sorted immediately, and nothing before
	// this point depends on the order.
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	for _, p := range pairs {
		w := weight[p]
		adj[p[0]] = append(adj[p[0]], neighbour{Key: p[1], Weight: w})
		adj[p[1]] = append(adj[p[1]], neighbour{Key: p[0], Weight: w})
	}

	label := make(map[string]string, len(keys))
	for _, k := range keys {
		label[k] = k
	}

	for pass := 0; pass < opts.MaxPasses; pass++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("detecting communities: %w", err)
		}
		changed := false
		for _, k := range keys {
			ns := adj[k]
			if len(ns) == 0 {
				continue
			}
			votes := map[string]int{}
			for _, n := range ns {
				votes[label[n.Key]] += n.Weight
			}
			// Candidates sorted, then scanned once: the winner is the greatest
			// summed weight, and because the scan is ascending a tie is only
			// ever taken by the smaller label. No map scan decides anything.
			cands := make([]string, 0, len(votes))
			for l := range votes {
				cands = append(cands, l)
			}
			sort.Strings(cands)
			best, bestW := label[k], votes[label[k]]
			for _, c := range cands {
				if votes[c] > bestW || (votes[c] == bestW && c < best) {
					best, bestW = c, votes[c]
				}
			}
			if best != label[k] {
				label[k] = best
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	// Canonicalise: every member of a community adopts the smallest member key
	// as the id. Without this the id is whichever label happened to win, which
	// is stable but arbitrary — and an id that is a real node is one a reader
	// can look up.
	smallest := map[string]string{}
	for _, k := range keys {
		l := label[k]
		if cur, ok := smallest[l]; !ok || k < cur {
			smallest[l] = k
		}
	}
	out := make([]Assignment, 0, len(keys))
	for _, k := range keys {
		out = append(out, Assignment{Key: k, ClusterID: smallest[label[k]]})
	}
	return out, nil
}
