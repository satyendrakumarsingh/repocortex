package metrics

import (
	"context"
	"fmt"
	"math"
	"sort"
)

// Score is one node's value under a centrality measure.
type Score struct {
	Key   string
	Value float64
}

// PageRankOptions tunes the power iteration.
type PageRankOptions struct {
	// Damping is the probability of following an edge rather than teleporting.
	Damping float64
	// MaxIterations bounds the walk so a pathological graph terminates.
	MaxIterations int
	// Tolerance is the L1 delta below which the ranks are called converged.
	Tolerance float64
}

// DefaultPageRankOptions returns the conventional damping of 0.85 with bounds
// that converge on a sparse code graph well inside the iteration cap.
func DefaultPageRankOptions() PageRankOptions {
	return PageRankOptions{Damping: 0.85, MaxIterations: 100, Tolerance: 1e-10}
}

// PageRank scores every node by inbound importance, highest first.
//
// On a call graph this answers "which symbol would hurt most to break": a
// function called by many callers that are themselves widely called ranks above
// one with the same raw in-degree from leaves. Raw degree cannot see that
// difference, which is why god-node lists built on degree alone are dominated
// by loggers and helpers.
//
// Dangling nodes (no outbound edges) have their mass redistributed uniformly
// rather than dropped; otherwise total rank leaks away every iteration and the
// scores stop being comparable between runs of different shape.
//
// Edge.Weight is IGNORED. Every outbound edge of a node carries the same share
// of that node's rank, so ten calls to one helper and one call to another
// contribute equally. That is the classic formulation and it is what this
// implementation computes; it is stated here because a graph that carries a
// weight and a ranking that does not read it is exactly the pairing a caller
// would otherwise assume was wired up.
//
// All accumulation runs over key-ascending slices, never map iteration, so the
// result is bit-reproducible.
func PageRank(ctx context.Context, g Graph, opts PageRankOptions) ([]Score, error) {
	n := g.Order()
	if n == 0 {
		return nil, nil
	}
	if opts.Damping <= 0 || opts.Damping >= 1 {
		return nil, fmt.Errorf("pagerank damping must be in (0,1), got %v", opts.Damping)
	}
	if opts.MaxIterations <= 0 {
		return nil, fmt.Errorf("pagerank max iterations must be positive, got %d", opts.MaxIterations)
	}

	keys := g.keys
	idx := make(map[string]int, n)
	for i, k := range keys {
		idx[k] = i
	}

	// Out-degree and successor indices, both in canonical order.
	succ := make([][]int, n)
	for i, k := range keys {
		for _, e := range g.OutEdges(k) {
			succ[i] = append(succ[i], idx[e.To])
		}
	}

	cur := make([]float64, n)
	next := make([]float64, n)
	init := 1.0 / float64(n)
	for i := range cur {
		cur[i] = init
	}

	teleport := (1.0 - opts.Damping) / float64(n)

	for iter := 0; iter < opts.MaxIterations; iter++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("computing pagerank: %w", err)
		}

		// Dangling mass is summed in index order before distribution.
		var dangling float64
		for i := 0; i < n; i++ {
			if len(succ[i]) == 0 {
				dangling += cur[i]
			}
		}
		base := teleport + opts.Damping*dangling/float64(n)
		for i := range next {
			next[i] = base
		}
		for i := 0; i < n; i++ {
			d := len(succ[i])
			if d == 0 {
				continue
			}
			share := opts.Damping * cur[i] / float64(d)
			for _, j := range succ[i] {
				next[j] += share
			}
		}

		var delta float64
		for i := 0; i < n; i++ {
			delta += math.Abs(next[i] - cur[i])
		}
		cur, next = next, cur
		if delta < opts.Tolerance {
			break
		}
	}

	out := make([]Score, 0, n)
	for i, k := range keys {
		out = append(out, Score{Key: k, Value: cur[i]})
	}
	// Descending by value, ties broken on key so the order is total.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value != out[j].Value {
			return out[i].Value > out[j].Value
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// BetweennessOptions tunes Brandes' algorithm.
type BetweennessOptions struct {
	// SampleSize caps how many source nodes are used as BFS roots. Zero or a
	// value >= Order() means exact: every node is a source.
	SampleSize int
	// Seed pins which sources a sample picks. Sampling without a fixed seed
	// would make the scores wobble between runs and read as real change.
	Seed int64
}

// DefaultBetweennessOptions samples at most 70 sources with a fixed seed.
//
// Exact betweenness is O(V*E), which on a 100k-symbol graph is not an
// interactive operation. Sampling 70 sources makes this affordable at
// repocortex's scale, but the sampling is deterministic striding over
// sorted keys (see selectSources), not the uniform-random draw that
// Brandes' classical estimator assumes. That is a deliberate departure
// from "standard": a wobbling centrality score reads as real architectural
// change to a caller who reruns the same query, and this codebase already
// made that trade once (label propagation over Louvain, for the same
// reason). The cost is that the scaled result is not a statistically
// unbiased estimate of the exact score, and for a given graph/key-ordering
// interaction can be systematically off rather than merely noisy — see the
// Betweenness doc comment for what this does and does not guarantee.
//
// 70 is a budget number, not a statistically derived one: it is the sample
// size that brought this package's own 100k-node/500k-edge benchmark fixture
// under the 2s budget (SampleSize: 100 measured 2,078ms; 70 measures 1,034.7ms
// — the figure metrics.go's "Measured performance" section publishes, and the
// one to change here when it is re-measured), nothing more principled than
// that. Lowering the sample from exact costs real fidelity, and not
// merely as smoothed-over noise: because striding is deterministic rather
// than a random draw, a small sample can reorder results relative to the
// exact computation rather than just perturb magnitudes. On this package's
// own bridgeGraph fixture, SampleSize=1 with Seed=3 ranks b1 (15) above
// bridge (10), while the exact computation (SampleSize: 0) ranks bridge (8)
// above b1 (6) — a full inversion, not just noise in the margins. A caller
// that needs a trustworthy ranking, not just a fast stable-under-rerun
// signal, should set SampleSize: 0 and pay for the exact computation.
func DefaultBetweennessOptions() BetweennessOptions {
	return BetweennessOptions{SampleSize: 70, Seed: 42}
}

// Betweenness scores nodes by how often they lie on shortest paths, highest
// first.
//
// This is the measure that finds architectural chokepoints: a node with modest
// degree that nonetheless every path must cross. PageRank will not surface it
// and degree certainly will not.
//
// Sources are chosen by striding the key-ascending node list from a
// seed-derived offset, not by a random number generator, so the sample is a
// pure function of (graph, options) with no hidden state: the same graph and
// options always pick the same sources and produce the same score, run after
// run. That reproducibility is the point, and it is bought with a real cost:
// deterministic striding is not uniform random sampling, so the "* n/sample"
// scale-up below is only a scale-up, not the unbiased estimator classical
// Brandes sampling theory promises. It can systematically favor or disfavor
// particular nodes depending on how they land relative to the stride, and a
// small sample can even reorder results relative to an exact run — on this
// package's own bridgeGraph fixture, SampleSize=1 with Seed=3 ranks b1 above
// bridge, while the exact computation ranks bridge above b1. A caller that
// needs a ranking it can act on should set SampleSize: 0 and pay for the
// exact computation; the sampled path is for a fast, stable-under-rerun
// signal, not a substitute for the exact ranking.
func Betweenness(ctx context.Context, g Graph, opts BetweennessOptions) ([]Score, error) {
	n := g.Order()
	if n == 0 {
		return nil, nil
	}
	keys := g.keys
	idx := make(map[string]int, n)
	for i, k := range keys {
		idx[k] = i
	}
	succ := make([][]int, n)
	for i, k := range keys {
		for _, e := range g.OutEdges(k) {
			succ[i] = append(succ[i], idx[e.To])
		}
	}

	sources := selectSources(n, opts)
	score := make([]float64, n)

	for _, s := range sources {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("computing betweenness: %w", err)
		}
		// Brandes: BFS, then accumulate dependencies in reverse order.
		sigma := make([]float64, n)
		dist := make([]int, n)
		delta := make([]float64, n)
		preds := make([][]int, n)
		for i := range dist {
			dist[i] = -1
		}
		sigma[s] = 1
		dist[s] = 0

		queue := []int{s}
		var order []int
		for len(queue) > 0 {
			v := queue[0]
			queue = queue[1:]
			order = append(order, v)
			for _, w := range succ[v] {
				if dist[w] < 0 {
					dist[w] = dist[v] + 1
					queue = append(queue, w)
				}
				if dist[w] == dist[v]+1 {
					sigma[w] += sigma[v]
					preds[w] = append(preds[w], v)
				}
			}
		}
		for i := len(order) - 1; i >= 0; i-- {
			w := order[i]
			for _, v := range preds[w] {
				if sigma[w] != 0 {
					delta[v] += (sigma[v] / sigma[w]) * (1 + delta[w])
				}
			}
			if w != s {
				score[w] += delta[w]
			}
		}
	}

	// Scale a sampled estimate up by n/len(sources) so magnitudes are in the
	// same ballpark as an exact run. This is NOT the unbiased correction from
	// Brandes' sampling theory — that requires uniform random sources, and
	// selectSources deliberately strides instead (see the doc comments above
	// for why). Treat the scaled score as a fast, reproducible signal, not as
	// interchangeable with an exact-run ranking.
	if len(sources) > 0 && len(sources) < n {
		scale := float64(n) / float64(len(sources))
		for i := range score {
			score[i] *= scale
		}
	}

	out := make([]Score, 0, n)
	for i, k := range keys {
		out = append(out, Score{Key: k, Value: score[i]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value != out[j].Value {
			return out[i].Value > out[j].Value
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// selectSources picks BFS roots deterministically by striding the sorted node
// list. A stride is used rather than math/rand so the choice is reproducible
// without carrying a generator's internal state into the result.
func selectSources(n int, opts BetweennessOptions) []int {
	if opts.SampleSize <= 0 || opts.SampleSize >= n {
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		return all
	}
	stride := n / opts.SampleSize
	if stride < 1 {
		stride = 1
	}
	offset := 0
	if opts.Seed != 0 {
		o := opts.Seed % int64(n)
		if o < 0 {
			o += int64(n)
		}
		offset = int(o)
	}
	// No collision guard: for 1 <= SampleSize < n, stride = floor(n/SampleSize)
	// >= 1, and the strided sequence (offset + i*stride) mod n for i = 0, 1, 2,
	// ... visits the coset of <stride> in Z/nZ generated from offset, which has
	// size n/gcd(stride, n) >= n/stride >= SampleSize. So the first SampleSize
	// iterations already produce SampleSize distinct values, and a repeat can
	// only occur, if ever, after the sample is already full — the loop below
	// never runs long enough to hit one. Verified empirically for n = 1..500,
	// every SampleSize < n, across 5 seeds: zero collisions.
	out := make([]int, 0, opts.SampleSize)
	for i := 0; len(out) < opts.SampleSize; i++ {
		out = append(out, (offset+i*stride)%n)
	}
	sort.Ints(out)
	return out
}
